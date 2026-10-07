package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// cnQuotaProber 抽象额度探测（*CNProviderQuotaService 实现，测试可替换）。
type cnQuotaProber interface {
	QueryUsage(ctx context.Context, accountID int64) (*CNProviderQuotaProbeResult, error)
}

// cnQuotaLifecycleHandover TH/Kira 额度耗尽的交接窄面（§4.1/§4.2）：周期探测
// 只刷新快照并上交耗尽信号，停调/告警/恢复全部由状态机负责。*CNQuotaLifecycleService
// 实现该接口。
type cnQuotaLifecycleHandover interface {
	OnUpstreamQuotaExhausted(ctx context.Context, account *Account, upstreamMsg string) error
}

// cnQuotaProbeConcurrency 周期任务并发探测额度账号的并发度。
const cnQuotaProbeConcurrency = 4

// CNProviderBalanceCheckService 周期性探测国产供应商账号：
//   - payg（按量付费）：余额低于阈值则临时停调，恢复则清除（仅清除本服务写入的停调）；
//   - coding plan：调用 CNProviderQuotaService 探测 5h/weekly 滚动窗口并落 extra 快照，
//     调度阈值评估（cnProviderThresholdCandidates）据此自动停调/恢复；
//   - Kira（kiraai.vn）/ TH（tokenharbor.ai）：只刷新快照（§4.3）+ 耗尽信号交
//     额度耗尽状态机（cn_quota_lifecycle_service.go），不做独立周期停调/清除
//     （2×interval 滚动打摆源退役，方案 §4.2/§7）。
//
// 克隆自 AccountExpiryService 的 Start/Stop/runOnce + ticker 骨架。
// 余额探测：kimi / deepseek 走原生 /user/balance；zhipu / minimax payg 走最小完成请求探测。
// 额度探测覆盖 kimi / zhipu 的 coding plan 账号（deepseek 无 coding 套餐）。
type CNProviderBalanceCheckService struct {
	accountRepo    AccountRepository
	balanceService *CNProviderBalanceService
	quotaService   cnQuotaProber
	rateLimitSvc   *RateLimitService
	httpUpstream   HTTPUpstream
	proxyRepo      ProxyRepository
	cfg            *config.Config
	interval       time.Duration
	stopCh         chan struct{}
	stopOnce       sync.Once
	wg             sync.WaitGroup
	// channelFreshness 是 E39 渠道维陈旧收敛窄面（可选注入）。账号余额恢复成功后触发
	// 关联渠道陈旧收敛；未注入时不收敛。
	channelFreshness ChannelFreshnessRefresher
	// thPassService TH Pass/用量快照探测链（TH 快照刷新用，懒装配/可注入）。
	thPassService *TokenHarborPassService
	// quotaLifecycle TH/Kira 额度耗尽状态机交接窄面（可选注入；未注入时仅
	// 刷新快照，耗尽信号丢失由状态机响应式入口兜底，见下方 TODO）。
	// TODO(wire, 方案 §4.2)：wire.go 装配时调用 SetQuotaLifecycleHandover 注入
	// CNQuotaLifecycleService，周期探测的耗尽确认/停调/告警即全量交给状态机。
	quotaLifecycle cnQuotaLifecycleHandover
}

// SetChannelFreshnessRefresher 注入 E39 渠道维陈旧收敛窄面（可选）。未注入时不收敛。
func (s *CNProviderBalanceCheckService) SetChannelFreshnessRefresher(r ChannelFreshnessRefresher) {
	if s != nil {
		s.channelFreshness = r
	}
}

// SetQuotaLifecycleHandover 注入 TH/Kira 额度耗尽状态机交接窄面（可选，接线点：
// wire.go 装配时注入 *CNQuotaLifecycleService，见方案 §4.2）。未注入时周期探测
// 只刷新快照，耗尽信号由响应式入口兜底。
func (s *CNProviderBalanceCheckService) SetQuotaLifecycleHandover(h cnQuotaLifecycleHandover) {
	if s != nil {
		s.quotaLifecycle = h
	}
}

// SetTokenHarborPassService 注入 TH Pass/用量快照探测服务（测试替换假站点用；
// 未注入时按需用本服务既有依赖懒装配）。
func (s *CNProviderBalanceCheckService) SetTokenHarborPassService(th *TokenHarborPassService) {
	if s != nil {
		s.thPassService = th
	}
}

// tokenHarborPass 返回 TH 快照探测链（懒装配：TH 账号刷新需要时才构造）。
func (s *CNProviderBalanceCheckService) tokenHarborPass() *TokenHarborPassService {
	if s == nil || s.accountRepo == nil || s.httpUpstream == nil {
		return nil
	}
	if s.thPassService == nil {
		s.thPassService = NewTokenHarborPassService(s.accountRepo, s.proxyRepo, s.httpUpstream)
	}
	return s.thPassService
}

// NewCNProviderBalanceCheckService 构造周期余额/额度检测服务。
// interval <= 0 时 Start() 直接返回（不启动），便于通过配置关闭。
//
// rateLimitSvc 供余额不足响应式停调（handleCNProviderInsufficientBalance）与
// coding plan 调度阈值停调（ApplyAccountSchedulingThreshold）复用；httpUpstream /
// proxyRepo 与 CNProviderBalanceService / CNProviderQuotaService 共用同一套既有
// 出客户端机制（不新建），供智谱 / MiniMax payg 最小完成请求探测使用。
func NewCNProviderBalanceCheckService(
	accountRepo AccountRepository,
	balanceService *CNProviderBalanceService,
	quotaService *CNProviderQuotaService,
	rateLimitSvc *RateLimitService,
	httpUpstream HTTPUpstream,
	proxyRepo ProxyRepository,
	cfg *config.Config,
	interval time.Duration,
) *CNProviderBalanceCheckService {
	return &CNProviderBalanceCheckService{
		accountRepo:    accountRepo,
		balanceService: balanceService,
		quotaService:   quotaService,
		rateLimitSvc:   rateLimitSvc,
		httpUpstream:   httpUpstream,
		proxyRepo:      proxyRepo,
		cfg:            cfg,
		interval:       interval,
		stopCh:         make(chan struct{}),
	}
}

func (s *CNProviderBalanceCheckService) Start() {
	if s == nil || s.accountRepo == nil || s.balanceService == nil || s.cfg == nil {
		return
	}
	if !s.cfg.Gateway.CNProviders.BalanceCheckEnabled {
		return
	}
	if s.interval <= 0 {
		return
	}
	log.Printf("[CNBalance] started (interval=%s threshold=%.2f)", s.interval, s.cfg.Gateway.CNProviders.BalanceThreshold)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()

		// 启动后先等待一个周期再首次探测，避免与进程启动峰重叠。
		for {
			select {
			case <-ticker.C:
				s.runOnce()
			case <-s.stopCh:
				return
			}
		}
	}()
}

func (s *CNProviderBalanceCheckService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	s.wg.Wait()
}

func (s *CNProviderBalanceCheckService) runOnce() {
	now := time.Now()
	// 收集 coding 探测目标（kimi/deepseek + 智谱）与 payg 检查队列。
	// coding 探测统一在收集完成后按 4 并发执行：单账号探测 15-20s，串行 ×
	// 多账号会耗尽整体预算（120s 上限），排在后面的账号快照会饥饿，
	// 连锁影响阈值停调的新鲜度判定。
	type quotaTarget struct {
		id       int64
		platform string
	}
	var quotaTargets []quotaTarget
	var paygTargets []*Account
	// probeTargets 收集智谱 / MiniMax 的 payg 账号：无公开余额端点，走最小完成
	// 请求探测（覆盖 native 余额不可用的中转型 payg 账号）。kimi/deepseek 的 payg
	// 维持原生 /user/balance 路径（仅在该路径失败时同周期转探测，见 payg 循环）。
	var probeTargets []*Account
	// kiraTargets 收集 Kira（kiraai.vn）账号（外审 F5：判定先于 IsCodingPlan()
	// 短路，coding 模式的 Kira 账号同样进 Kira 快照刷新链）：只刷新快照
	//（kira_usage_snapshot + VND 余额）+ 耗尽信号交状态机，不做独立周期停调。
	var kiraTargets []*Account
	// thTargets 收集 TH（tokenharbor.ai）账号：只刷新快照（th_pass_snapshot +
	// th_usage_snapshot），不做任何周期停调/清除（TH 停调交状态机，§4.2）。
	var thTargets []*Account
	collect := func(platform string, accounts []Account) {
		for i := range accounts {
			account := &accounts[i]
			if !account.IsActive() {
				continue
			}
			// 挂在国产平台下、base_url 指向官方 ollama.com 的账号由 Ollama Cloud
			// 用量窗口负责：CN 探测端点由 base_url 衍生，ollama.com 会被出站
			// URL 白名单拒绝（CN_BALANCE_URL_REJECTED），不跳过则每个周期都
			// 白跑并产生告警噪声。
			if IsOllamaCloudUsageAccount(account) {
				continue
			}
			// Kira（kiraai.vn）账号（F5：先于 IsCodingPlan() 短路——coding 模式的
			// Kira 账号 platform 可能挂在 kimi/deepseek/zhipu 下，但探测统一走
			// Kira 快照刷新链（dashboard JWT），既不走原生 /user/balance（会被按
			// platform 错打到 moonshot/deepseek），也不走 coding 阈值停调/最小完成
			// 请求探测（白耗上游 token）。快照刷新不要求 Schedulable——已被状态机
			// 停调的账号也需要新鲜快照决定恢复。
			if accountIsKiraBaseURL(account) {
				// 收集期年龄门（Kira 单门）：kira_usage_snapshot 够新鲜则跳过本轮
				// 快照刷新；缺失/解析失败 = 不过新，收集。
				if !s.shouldSkipKiraCollect(now, account) {
					kiraTargets = append(kiraTargets, account)
				}
				continue
			}
			// coding 账号：探测滚动窗口并落快照（不要求 Schedulable——已被
			// 阈值停调的账号也需要新鲜快照决定是否续停）。
			if account.IsCodingPlan() {
				quotaTargets = append(quotaTargets, quotaTarget{id: account.ID, platform: account.Platform})
				continue
			}
			// payg 余额探测：
			switch platform {
			case PlatformZhipu, PlatformMiniMax:
				// 智谱 / MiniMax 无公开余额端点：进最小完成请求探测队列。
				// Schedulable 是管理端手动开关：停用（false）的账号不进探测；
				// 临时停调账号 Schedulable 仍为 true、必须继续收集以支持充值后
				// 探测恢复（与管理端开关语义对齐，两层互不干扰）。
				if account.Schedulable {
					probeTargets = append(probeTargets, account)
				}
			default:
				// kimi/deepseek payg：维持原生 /user/balance 路径。
				if account.Schedulable {
					paygTargets = append(paygTargets, account)
				}
			}
		}
	}
	for _, platform := range s.platforms() {
		accounts, err := s.accountRepo.ListByPlatform(context.Background(), platform)
		if err != nil {
			log.Printf("[CNBalance] list %s accounts failed: %v", platform, err)
			continue
		}
		collect(platform, accounts)
	}
	// 智谱 / MiniMax 无余额端点，仅进额度探测。
	if s.quotaService != nil {
		for _, platform := range []string{PlatformZhipu, PlatformMiniMax} {
			accounts, err := s.accountRepo.ListByPlatform(context.Background(), platform)
			if err != nil {
				log.Printf("[CNBalance] list %s accounts failed: %v", platform, err)
				continue
			}
			collect(platform, accounts)
		}
	} else {
		// quotaService 未配置时智谱 / MiniMax 平台账号不参与收集（上方循环被 gate），
		// 但挂在 zhipu/minimax 平台下的 Kira 账号快照刷新不受该 gate 影响，单独补收
		//（外审 F5：coding 模式的 Kira 账号同样收集——Kira 判定先于 IsCodingPlan()，
		// 非 Kira 的 coding 账号在 quotaService 缺位时维持跳过）。
		for _, platform := range []string{PlatformZhipu, PlatformMiniMax} {
			accounts, err := s.accountRepo.ListByPlatform(context.Background(), platform)
			if err != nil {
				log.Printf("[CNBalance] list %s accounts failed: %v", platform, err)
				continue
			}
			for i := range accounts {
				account := &accounts[i]
				if !account.IsActive() || IsOllamaCloudUsageAccount(account) {
					continue
				}
				if accountIsKiraBaseURL(account) && !s.shouldSkipKiraCollect(now, account) {
					kiraTargets = append(kiraTargets, account)
				}
			}
		}
	}

	// TH（tokenharbor.ai）账号快照刷新（F1：周期链的 TH 快照刷新分支）：这类账号
	// platform 是 openai，按 base_url 识别；只刷新 th_pass_snapshot/th_usage_snapshot，
	// 不做任何周期停调/清除（TH 停调交状态机响应式入口，§4.2）。
	if s.tokenHarborPass() != nil {
		accounts, err := s.accountRepo.ListByPlatform(context.Background(), PlatformOpenAI)
		if err != nil {
			log.Printf("[CNBalance] list %s accounts failed: %v", PlatformOpenAI, err)
		} else {
			for i := range accounts {
				account := &accounts[i]
				if !account.IsActive() || IsOllamaCloudUsageAccount(account) {
					continue
				}
				if accountIsTokenHarborBaseURL(account) && !s.shouldSkipTokenHarborCollect(now, account) {
					thTargets = append(thTargets, account)
				}
			}
		}
	}

	// 预算按工作量放大：4 并发 × 15s/批 + payg/probe/快照刷新每账号 5s，下限 30s 上限 300s。
	batches := (len(quotaTargets) + cnQuotaProbeConcurrency - 1) / cnQuotaProbeConcurrency
	timeout := 30*time.Second + time.Duration(batches)*15*time.Second +
		time.Duration(len(paygTargets)+len(probeTargets)+len(kiraTargets)+len(thTargets))*5*time.Second
	if timeout > 300*time.Second {
		timeout = 300 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	threshold := s.cfg.Gateway.CNProviders.BalanceThreshold
	paused, cleared := 0, 0
	for _, account := range paygTargets {
		switch s.checkOne(ctx, account, threshold) {
		case cnBalancePaused:
			paused++
		case cnBalanceCleared:
			cleared++
		case cnBalanceNativeFailed:
			// kimi/deepseek payg 原生 /user/balance 不可用（中转未实现该端点）：
			// 同周期转最小完成请求探测，覆盖该人群。
			s.probeOne(ctx, account)
		}
	}

	// 智谱 / MiniMax payg：最小完成请求探测（无公开余额端点）。
	for _, account := range probeTargets {
		s.probeOne(ctx, account)
	}

	// Kira / TH：快照刷新 + 耗尽信号交状态机（打摆源退役，不做独立周期停调/清除）。
	for _, account := range kiraTargets {
		s.refreshKiraAccount(ctx, account)
	}
	for _, account := range thTargets {
		s.refreshTokenHarborAccount(ctx, account)
	}

	if len(quotaTargets) > 0 && s.quotaService != nil {
		sem := make(chan struct{}, cnQuotaProbeConcurrency)
		var wg sync.WaitGroup
		for _, target := range quotaTargets {
			wg.Add(1)
			go func(t quotaTarget) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				s.probeQuota(ctx, t.id, t.platform)
			}(target)
		}
		wg.Wait()
	}

	// 429 误禁闭对账（自愈）：快照刷新后，若限流账号的官方窗口余量充足，
	// 提前解除被误禁闭到窗口重置点的账号级限流（瞬时过载 429 误判的自愈）。
	// 对账范围需覆盖 zhipu（火山订阅号挂在该平台下， volcanos 仅进额度探测）。
	reconcilePlatforms := append(s.platforms(), PlatformZhipu)
	if clearedLimits := reconcileCNProviderRateLimits(ctx, s.accountRepo, reconcilePlatforms, time.Now()); clearedLimits > 0 {
		log.Printf("[CNBalance] rate-limit reconcile cleared=%d", clearedLimits)
	}

	if paused > 0 || cleared > 0 {
		log.Printf("[CNBalance] paused=%d cleared=%d (threshold=%.2f)", paused, cleared, threshold)
	}
}

// thProbeBackoffUntilExtraKey A 卡契约键名：TH 探测退避截止（unix 秒，存于账号
// Extra）。与 tokenharbor_pass_service.go / 额度耗尽状态机 A 卡字面值严格一致
//（B 卡与 A 卡经此 extra 键名对接）。null/缺失/过期 = 未退避。
const thProbeBackoffUntilExtraKey = "th_probe_backoff_until"

// shouldSkipTokenHarborCollect 判定 TH 账号是否跳过本轮收集（周期链收集期门控，
// 仅作用于 append 进 thTargets 之前）：
//   - 年龄门：th_usage_snapshot.fetched_at 距今 < th_probe_interval_minutes → 跳过
//     （快照缺失/解析失败 = 不过新，收集；pass/usage 同轮成对刷新，一门控两）；
//   - 退避门：extra th_probe_backoff_until 存在且 > now → 跳过（null/缺失/过期 = 收集）。
//
// 两门任一命中即跳过。配置间隔为 0（未配置）时年龄门不生效（向后兼容全收集，
// 既有 test 用零值 config）。
func (s *CNProviderBalanceCheckService) shouldSkipTokenHarborCollect(now time.Time, account *Account) bool {
	interval := time.Duration(s.cfg.Gateway.CNProviders.ThProbeIntervalMinutes) * time.Minute
	if interval > 0 {
		if snap, ok := TokenHarborUsageSnapshotFromExtra(account); ok && !snap.FetchedAt.IsZero() {
			if now.Sub(snap.FetchedAt) < interval {
				return true
			}
		}
	}
	if until, ok := thProbeBackoffUntilFromExtra(account); ok && until.After(now) {
		return true
	}
	return false
}

// thProbeBackoffUntilFromExtra 从账号 Extra 读 TH 探测退避截止（unix 秒）。
// 返回 (零值, false) 的情形：键缺失、nil、非数值、<=0（视为过期/未退避）。
func thProbeBackoffUntilFromExtra(account *Account) (time.Time, bool) {
	if account == nil || account.Extra == nil {
		return time.Time{}, false
	}
	raw, ok := account.Extra[thProbeBackoffUntilExtraKey]
	if !ok || raw == nil {
		return time.Time{}, false
	}
	var sec int64
	switch v := raw.(type) {
	case int64:
		sec = v
	case float64:
		sec = int64(v)
	case json.Number:
		sec, _ = v.Int64()
	case string:
		sec, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	default:
		return time.Time{}, false
	}
	if sec <= 0 {
		return time.Time{}, false
	}
	return time.Unix(sec, 0), true
}

// shouldSkipKiraCollect 判定 Kira 账号是否跳过本轮收集（周期链收集期门控，仅作用
// 于 append 进 kiraTargets 之前）。Kira 仅年龄门（方案锁定项 4：Kira 不加退避）：
// kira_usage_snapshot.fetched_at 距今 < kira_probe_interval_minutes → 跳过；
// 快照缺失/解析失败 = 不过新，收集。配置间隔为 0（未配置）时门不生效（向后兼容）。
func (s *CNProviderBalanceCheckService) shouldSkipKiraCollect(now time.Time, account *Account) bool {
	interval := time.Duration(s.cfg.Gateway.CNProviders.KiraProbeIntervalMinutes) * time.Minute
	if interval <= 0 {
		return false
	}
	fetchedAt := kiraUsageSnapshotFetchedAt(account)
	if fetchedAt.IsZero() {
		return false
	}
	return now.Sub(fetchedAt) < interval
}

// kiraUsageSnapshotFetchedAt 从账号 Extra 读 kira_usage_snapshot.fetched_at
//（RFC3339 字符串）。缺失/类型不符/解析失败返回零值（调用方据此视为"不过新"）。
func kiraUsageSnapshotFetchedAt(account *Account) time.Time {
	if account == nil || account.Extra == nil {
		return time.Time{}
	}
	raw, ok := account.Extra[kiraUsageSnapshotExtraKey]
	if !ok || raw == nil {
		return time.Time{}
	}
	var fetchedStr string
	switch v := raw.(type) {
	case map[string]any:
		if s, ok := v["fetched_at"].(string); ok {
			fetchedStr = s
		}
	case string:
		// 整段 JSON 字符串形态（极少见）：解析结构取 fetched_at。
		var snap struct {
			FetchedAt string `json:"fetched_at"`
		}
		if json.Unmarshal([]byte(v), &snap) == nil {
			fetchedStr = snap.FetchedAt
		}
	default:
		return time.Time{}
	}
	if fetchedStr == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, fetchedStr)
	if err != nil {
		return time.Time{}
	}
	return t
}

// probeQuota 探测单个 coding plan 账号的滚动窗口用量并落 extra 快照。
// 落快照后重载账号并应用调度阈值停调（与 codebuddy_quota_check_service.go 先例
// 一致：失败日志不阻断其他账号）。
func (s *CNProviderBalanceCheckService) probeQuota(ctx context.Context, accountID int64, platform string) {
	if s.quotaService == nil {
		return
	}
	result, err := s.quotaService.QueryUsage(ctx, accountID)
	if err != nil {
		log.Printf("[CNBalance] quota probe account %d (%s) failed: %v", accountID, platform, err)
		return
	}
	if result != nil && !result.Success && result.Error != "" {
		log.Printf("[CNBalance] quota probe account %d (%s) error: %s", accountID, platform, result.Error)
	}
	// 快照已落库：重载账号后应用调度阈值评估（含暂停账号续停/恢复）。
	if s.rateLimitSvc != nil {
		fresh, getErr := s.accountRepo.GetByID(ctx, accountID)
		if getErr != nil {
			log.Printf("[CNBalance] quota probe reload account %d (%s) failed: %v", accountID, platform, getErr)
			return
		}
		if s.rateLimitSvc.ApplyAccountSchedulingThreshold(ctx, fresh) {
			log.Printf("[CNBalance] account %d (%s) paused by scheduling threshold", accountID, platform)
		}
	}
}

// probeOne 对无公开余额端点的 payg 账号发起一次最小完成请求（chat/completions），
// 据此判定余额不足并停调 / 恢复：
//   - 响应体命中余额不足文案 → 停调 2× 检测周期（含 _balance_low 快照标记）；
//   - HTTP 2xx 且未命中余额不足 → 仅当本服务写入的余额前缀停调存在时清除（他因不动）；
//   - 其余（超时/连接失败/401/403/429 无文案/未识别错误体）→ 不停调不清除，下周期重试。
//
// 出客户端复用 CNProviderBalanceService / CNProviderQuotaService 同一套机制
// （httpUpstream + resolveProxyURL + cnValidateProbeURL），不新建客户端。
func (s *CNProviderBalanceCheckService) probeOne(ctx context.Context, account *Account) {
	if s == nil || s.httpUpstream == nil || s.rateLimitSvc == nil {
		log.Printf("[CNBalance] probe account %d (%s) skipped: service not fully configured", account.ID, account.Platform)
		return
	}
	model := resolveWebTestModel(account, "", account.Platform)
	if model == "" {
		log.Printf("[CNBalance] probe account %d (%s) skipped: cannot resolve web test model", account.ID, account.Platform)
		return
	}
	apiKey := strings.TrimSpace(account.GetOpenAIProtocolAPIKey())
	if apiKey == "" {
		log.Printf("[CNBalance] probe account %d (%s) skipped: empty api key", account.ID, account.Platform)
		return
	}
	baseURL := strings.TrimRight(strings.TrimSpace(account.GetOpenAIBaseURL()), "/")
	if baseURL == "" {
		log.Printf("[CNBalance] probe account %d (%s) skipped: empty base url", account.ID, account.Platform)
		return
	}
	rawURL := baseURL + "/chat/completions"
	targetURL, err := cnValidateProbeURL(s.cfg, rawURL)
	if err != nil {
		log.Printf("[CNBalance] probe account %d (%s) url rejected: %v", account.ID, account.Platform, err)
		return
	}
	payload, err := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 1,
	})
	if err != nil {
		log.Printf("[CNBalance] probe account %d (%s) marshal failed: %v", account.ID, account.Platform, err)
		return
	}
	proxyURL := s.resolveProxyURL(ctx, account)
	callCtx, cancel := context.WithTimeout(ctx, cnBalanceUpstreamTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, targetURL, bytes.NewReader(payload))
	if err != nil {
		log.Printf("[CNBalance] probe account %d (%s) build request failed: %v", account.ID, account.Platform, err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	account.ApplyHeaderOverrides(req.Header)

	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, maxInt(account.Concurrency, 1))
	if err != nil {
		log.Printf("[CNBalance] probe account %d (%s) request failed: %v", account.ID, account.Platform, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, cnBalanceMaxBodyBytes))

	if cnProviderResponseIndicatesInsufficientBalance(body) {
		s.rateLimitSvc.handleCNProviderInsufficientBalance(ctx, account, extractUpstreamErrorMessage(body))
		log.Printf("[CNBalance] probe account %d (%s) insufficient balance -> paused", account.ID, account.Platform)
		return
	}
	// 有效完成响应（HTTP 2xx）：按原生路径同款语义清除 balance_low 快照标记，
	// 再清除本服务写入的余额前缀停调；他因不动。
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// F1：清除响应式 402/429 写下的 balance_low 标记（镜像
		// cn_provider_balance_service.go:277 注释与写法）。UpdateExtra 失败仅
		// 告警，不阻断后续停调清除（与原生路径 'else result.Persisted' 一致）。
		if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
			cnExtraKey(account.Platform, cnBalanceExtraSuffixLow): false,
		}); err != nil {
			log.Printf("[CNBalance] probe account %d (%s) clear balance_low marker failed: %v", account.ID, account.Platform, err)
		}
		if account.TempUnschedulableUntil != nil &&
			strings.HasPrefix(account.TempUnschedulableReason, cnBalanceLowReasonPrefix) {
			if err := s.accountRepo.ClearTempUnschedulable(ctx, account.ID); err != nil {
				log.Printf("[CNBalance] probe account %d (%s) clear failed: %v", account.ID, account.Platform, err)
				return
			}
			// E39：余额恢复后触发关联渠道维陈旧收敛（最佳努力，不回滚/不阻断恢复结论）。
			if s.channelFreshness != nil {
				if ferr := s.channelFreshness.RefreshChannelFreshnessForAccount(ctx, account.ID); ferr != nil {
					log.Printf("[CNBalance] probe account %d (%s) channel freshness refresh failed: %v", account.ID, account.Platform, ferr)
				}
			}
			log.Printf("[CNBalance] probe account %d (%s) reactivated (balance recovered)", account.ID, account.Platform)
		}
	}
}

// refreshKiraAccount Kira 账号周期链（§4.2/§7 打摆源退役）：只刷新快照
// （kira_usage_snapshot 经额度探测 + VND 余额经余额探测）+ 免费池耗尽信号交
// 状态机确认探针；不做本服务的 2×interval 滚动停调/清除。探测失败（网络/鉴权/
// 解析）按失败关闭处理：本轮不上交耗尽信号、不动现状态，下周期重试。
func (s *CNProviderBalanceCheckService) refreshKiraAccount(ctx context.Context, account *Account) {
	exhausted := false
	if s.quotaService != nil {
		result, err := s.quotaService.QueryUsage(ctx, account.ID)
		switch {
		case err != nil:
			log.Printf("[CNBalance] kira usage snapshot account %d (%s) failed: %v", account.ID, account.Platform, err)
		case result != nil && !result.Success && result.Error != "":
			log.Printf("[CNBalance] kira usage snapshot account %d (%s) error: %s", account.ID, account.Platform, result.Error)
		case result != nil:
			exhausted = cnKiraResultExhausted(result)
		}
	}
	if s.balanceService != nil {
		if _, err := s.balanceService.QueryBalanceForAccount(ctx, account); err != nil {
			log.Printf("[CNBalance] kira balance snapshot account %d (%s) failed: %v", account.ID, account.Platform, err)
		}
	}
	// 耗尽信号交状态机（确认探针/停调至每日重置时刻/告警由状态机负责）。
	// TODO(wire, 方案 §4.2)：quotaLifecycle 未注入时信号由响应式 402/429 入口兜底。
	if exhausted && s.quotaLifecycle != nil {
		if err := s.quotaLifecycle.OnUpstreamQuotaExhausted(ctx, account, "kira daily free pool exhausted (periodic snapshot refresh)"); err != nil {
			log.Printf("[CNBalance] kira lifecycle handover account %d failed: %v", account.ID, err)
		}
	}
}

// cnKiraResultExhausted 由 Kira 用量探测结果判定免费池是否耗尽
// （used_tokens >= limit_tokens 且 limit>0；分母缺失/快照未产出视为不耗尽——
// 失败关闭语义，不把数据不明当耗尽）。
func cnKiraResultExhausted(result *CNProviderQuotaProbeResult) bool {
	if result == nil || result.Snapshot == nil ||
		result.Snapshot.UsedTokens == nil || result.Snapshot.LimitTokens == nil {
		return false
	}
	limit := *result.Snapshot.LimitTokens
	if limit <= 0 {
		return false
	}
	return *result.Snapshot.UsedTokens >= limit
}

// refreshTokenHarborAccount TH 账号周期链：只刷新 th_pass_snapshot +
// th_usage_snapshot（快照刷新不要求 Schedulable——已停调账号也需要新鲜快照），
// 不做任何周期停调/清除（TH 耗尽由响应式 402/429 交状态机，§4.2）。
func (s *CNProviderBalanceCheckService) refreshTokenHarborAccount(ctx context.Context, account *Account) {
	th := s.tokenHarborPass()
	if th == nil {
		return
	}
	passSnapshot, err := th.Probe(ctx, account)
	if err != nil {
		log.Printf("[CNBalance] th pass snapshot account %d failed: %v", account.ID, err)
	} else if err := th.PersistSnapshot(ctx, account.ID, passSnapshot); err != nil {
		log.Printf("[CNBalance] th pass snapshot persist account %d failed: %v", account.ID, err)
	}
	usageSnapshot, err := th.ProbeUsageSnapshot(ctx, account)
	if err != nil {
		// 失败关闭：本轮不落 th_usage_snapshot，不动现状态，下周期重试。
		log.Printf("[CNBalance] th usage snapshot account %d failed: %v", account.ID, err)
		return
	}
	if err := th.PersistUsageSnapshot(ctx, account.ID, usageSnapshot); err != nil {
		log.Printf("[CNBalance] th usage snapshot persist account %d failed: %v", account.ID, err)
	}
}

// resolveProxyURL 解析账号探测代理（与 CNProviderBalanceService 同口径）。
func (s *CNProviderBalanceCheckService) resolveProxyURL(ctx context.Context, account *Account) string {
	if account == nil || account.ProxyID == nil {
		return ""
	}
	if account.Proxy != nil {
		return account.Proxy.URL()
	}
	if s != nil && s.proxyRepo != nil {
		if proxy, err := s.proxyRepo.GetByID(ctx, *account.ProxyID); err == nil && proxy != nil {
			account.Proxy = proxy
			return proxy.URL()
		}
	}
	return ""
}

type cnBalanceCheckOutcome int

const (
	cnBalanceNoChange cnBalanceCheckOutcome = iota
	cnBalancePaused
	cnBalanceCleared
	// cnBalanceNativeFailed 表示原生 /user/balance 探测失败（err 或 !Success）：
	// 调用方据此决定是否转最小完成请求探测（覆盖中转未实现余额端点的 kimi/deepseek）。
	cnBalanceNativeFailed
)

// checkOne 探测单账号余额并决定停调/恢复。探测失败时不动现状（避免瞬时网络抖动
// 误解除或误停调）。原生余额端点不可用时返回 cnBalanceNativeFailed（不视为健康、
// 也不停调），供调用方转最小完成请求探测。
func (s *CNProviderBalanceCheckService) checkOne(ctx context.Context, account *Account, threshold float64) cnBalanceCheckOutcome {
	result, err := s.balanceService.QueryBalance(ctx, account.ID)
	if err != nil || result == nil || !result.Success {
		return cnBalanceNativeFailed
	}

	// 双币种（deepseek CNY+USD）任一币种余额达标即可继续调度；仅当全部低于
	// 阈值（或不可用）才停调。同程序中转的订阅制不限量（Unlimited）无数字余额
	// 可比，永不因阈值停调（否则 remaining=-1 的订阅 key 会被误停）。
	low := !result.Available || (!result.Unlimited && allCNBalancesBelowThreshold(result, threshold))
	if low {
		// 已被（任何来源）停调时不覆盖其 reason。
		if !account.IsSchedulable() {
			return cnBalanceNoChange
		}
		reason := cnBalanceLowReason(fmt.Sprintf("余额 %.4g %s 低于阈值 %.2f", result.Balance, result.Currency, threshold))
		if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, time.Now().Add(s.cooldown()), reason); err != nil {
			log.Printf("[CNBalance] pause account %d failed: %v", account.ID, err)
			return cnBalanceNoChange
		}
		log.Printf("[CNBalance] paused account %d (%s): balance=%.4g %s", account.ID, account.Platform, result.Balance, result.Currency)
		return cnBalancePaused
	}

	// 余额健康：仅清除「本服务写入」的临时停调（reason 前缀匹配），不触碰其他子系统。
	if account.TempUnschedulableUntil != nil && strings.HasPrefix(account.TempUnschedulableReason, cnBalanceLowReasonPrefix) {
		if err := s.accountRepo.ClearTempUnschedulable(ctx, account.ID); err != nil {
			log.Printf("[CNBalance] clear account %d failed: %v", account.ID, err)
			return cnBalanceNoChange
		}
		// E39：余额恢复后触发关联渠道维陈旧收敛（最佳努力，不回滚/不阻断恢复结论）。
		if s.channelFreshness != nil {
			if ferr := s.channelFreshness.RefreshChannelFreshnessForAccount(ctx, account.ID); ferr != nil {
				log.Printf("[CNBalance] account %d channel freshness refresh failed: %v", account.ID, ferr)
			}
		}
		log.Printf("[CNBalance] reactivated account %d (%s): balance=%.4g %s", account.ID, account.Platform, result.Balance, result.Currency)
		return cnBalanceCleared
	}
	return cnBalanceNoChange
}

func (s *CNProviderBalanceCheckService) platforms() []string {
	return []string{PlatformKimi, PlatformDeepseek}
}

// allCNBalancesBelowThreshold 判断全部币种余额是否均低于阈值。
// 无明细时退回主币种判定（与旧行为一致）。
func allCNBalancesBelowThreshold(result *CNProviderBalanceResult, threshold float64) bool {
	if len(result.Balances) == 0 {
		return result.Balance < threshold
	}
	for _, entry := range result.Balances {
		if entry.Balance >= threshold {
			return false
		}
	}
	return true
}

// cooldown 返回临时停调持续时长（= 2× 检测周期），与响应式 402/429 路径一致。
func (s *CNProviderBalanceCheckService) cooldown() time.Duration {
	minutes := 10
	if s.cfg != nil {
		if cfgMin := s.cfg.Gateway.CNProviders.BalanceCheckIntervalMinutes; cfgMin > 0 {
			minutes = cfgMin
		}
	}
	cooldown := time.Duration(minutes) * time.Minute * 2
	if cooldown < time.Minute {
		cooldown = 10 * time.Minute
	}
	return cooldown
}
