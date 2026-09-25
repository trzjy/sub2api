package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// cnQuotaProber 抽象额度探测（*CNProviderQuotaService 实现，测试可替换）。
type cnQuotaProber interface {
	QueryUsage(ctx context.Context, accountID int64) (*CNProviderQuotaProbeResult, error)
}

// cnQuotaProbeConcurrency 周期任务并发探测额度账号的并发度。
const cnQuotaProbeConcurrency = 4

// CNProviderBalanceCheckService 周期性探测国产供应商账号：
//   - payg（按量付费）：余额低于阈值则临时停调，恢复则清除（仅清除本服务写入的停调）；
//   - coding plan：调用 CNProviderQuotaService 探测 5h/weekly 滚动窗口并落 extra 快照，
//     调度阈值评估（cnProviderThresholdCandidates）据此自动停调/恢复。
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
	}

	// 预算按工作量放大：4 并发 × 15s/批 + payg/probe 每账号 5s，下限 30s 上限 300s。
	batches := (len(quotaTargets) + cnQuotaProbeConcurrency - 1) / cnQuotaProbeConcurrency
	timeout := 30*time.Second + time.Duration(batches)*15*time.Second +
		time.Duration(len(paygTargets)+len(probeTargets))*5*time.Second
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
		"model":     model,
		"messages":  []map[string]string{{"role": "user", "content": "hi"}},
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
			log.Printf("[CNBalance] probe account %d (%s) reactivated (balance recovered)", account.ID, account.Platform)
		}
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
