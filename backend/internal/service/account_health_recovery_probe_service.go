package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"go.uber.org/zap"
)

// accountHealthProbeCandidateLimit caps how many temp-unschedulable accounts a single
// probe sweep evaluates. The breaker cooldown is short (minutes), so the active set
// is small; the cap protects against pathological backlog after a long outage.
const accountHealthProbeCandidateLimit = 200

// 补探（retry burst）默认节奏：首探失败后不再空等一个完整轮次（默认 15s）才再试，
// 而是在本轮 sweep 内按短间隔连补（默认上限 50 次，AC-C1：3→50，受
// settings.probe.max_attempts 双重约束，生产配置 100）若干次，把恢复时长从「冷却到期」
// （cooldown 分钟级）拉回秒级；双重上限（本轮补探次数 + settings.probe.max_attempts）
// 避免探测风暴。补探自 AC-C1 起异步执行（独立 goroutine，纳入 Start 的 wg），
// 同账号补探由 accountHealthProbeRetryBurst（in-flight）去重，防止重叠。
const (
	// accountHealthProbeRetryInterval 首探失败后的补探间隔（秒级，AC-C1 不动）。
	accountHealthProbeRetryInterval = 5 * time.Second
	// accountHealthProbeRetryBurst 单轮 sweep 内每个失败账号的最大补探次数（AC-C1：3→50）。
	accountHealthProbeRetryBurst = 50
)

// probeRecoverableMarkers 列出可被探测恢复的停调标记词（TempUnschedState.MatchedKeyword）：
//   - openai_apikey_health_breaker：APIKey 健康熔断 L3 停调（CodeBuddy 影子准入后
//     同样携带该标记，方案 T1）；
//   - pool_mode_401_escalation：池模式 401 窗口升级停调（方案 T5，常量单一定义于
//     pool_401_escalation.go，此处禁止字面量重复）。
var probeRecoverableMarkers = []string{
	openAIAPIKeyHealthBreakerReason,
	PoolMode401EscalationMarker,
	oauth401RecoverableMarker,
}

// accountHealthProbeRepository is the narrow repository surface the recovery probe
// needs. It is intentionally a subset of AccountRepository so the real repository
// satisfies it without forcing every AccountRepository test double to implement the
// probe-specific methods.
type accountHealthProbeRepository interface {
	GetByID(ctx context.Context, id int64) (*Account, error)
	ListTempUnschedulableAccounts(ctx context.Context, now time.Time, limit int) ([]*Account, error)
	// ClearTempUnschedulableIfGeneration 是仓储层条件清除原语（AC-G2R5 must_fix / AC-G2R6 子秒
	// 精度）：用「截止时间」做不可变代际令牌。仅当该账号当前 temp_unschedulable_until 仍等于
	// expectedUntil（探测开始/入队时读到的代际令牌，timestamptz 全精度等值）时才清除停调并返回
	// true；若 until 已被他人改写（新停调接管，必写更晚 until）则零写入、返回 false，探测方据此
	// 保留新停调、不清除。仓储层单语句原子保证「读代际→清停调」不被并发覆盖
	// （见 account_repo.go ClearTempUnschedulableIfGeneration）。
	ClearTempUnschedulableIfGeneration(ctx context.Context, accountID int64, expectedUntil time.Time) (bool, error)
	// BumpTempUnschedulableReasonIfGeneration 是 attempt 回写的代际守卫原语（AC-G2R6 must_fix）：
	// 仅当当前 until 仍等于 expectedUntil 且仍处有效停调窗口时才改写 reason 并返回 true；否则
	// 返回 false（已被接管），调用方据此终止本轮补探、保留接管者 reason。
	BumpTempUnschedulableReasonIfGeneration(ctx context.Context, accountID int64, expectedUntil time.Time, newReason string) (bool, error)
}

// AccountHealthRecoveryProbeService implements the optional Phase C probe-based
// recovery. While an account is inside the health-breaker cooldown, it periodically
// sends a cheap, real upstream request (preferring /models, else a minimal
// completion). A successful probe clears the temp-unschedulable state early; a
// failing probe keeps the account parked until either the cooldown expires or
// maxAttempts is reached (then it falls back to expiry-based recovery).
//
// The probe is opt-in: Start() is a no-op unless the admin explicitly enables
// settings.probe.enabled. SSRF protection is enforced via cnValidateProbeURL before
// any request is built, reusing the same gate as the gateway's own outbound calls.
//
// Multi-instance note: each backend replica runs its own independent sweep, so the
// same temp-unschedulable account may be probed by several instances at once. The
// probe is read-only against upstream and its effects (ClearTempUnschedulable on
// success, or an incremented probe_attempts on failure) are idempotent, so the
// duplication is harmless beyond a little extra upstream traffic.
type AccountHealthRecoveryProbeService struct {
	accountRepo         accountHealthProbeRepository
	httpUpstream        HTTPUpstream
	cfg                 *config.Config
	rateLimit           *RateLimitService
	settingService      *SettingService
	tlsFPProfileService *TLSFingerprintProfileService

	startMu sync.Mutex
	stopCh  chan struct{}
	// stopping 是单停权标志（受 startMu 保护）：Stop 在开始关闭路径前于锁内置 true，
	// 声明独占关闭权并独占 wg.Wait；期间任何 Start 在锁内被 stopping 挡住，绝不执行
	// wg.Add，从而彻底消除「Stop 的 wg.Wait 与 Start 的 wg.Add 交错」（sync.WaitGroup
	// 禁止在 Wait 仍挂起时并发 Add）。Stop 完成 wg.Wait 后在锁内清 stopping，恢复 Start 可用。
	stopping bool
	wg       sync.WaitGroup

	// probeOverride, when set, replaces the real upstream probe (used by tests).
	probeOverride func(ctx context.Context, account *Account) (ok bool, err error)

	// retryInterval / retryBurst 覆盖补探节奏（仅测试注入，避免单测真实等待
	// accountHealthProbeRetryInterval）；零值经 probeRetryInterval/probeRetryBurst
	// 回落到默认常量。
	retryInterval time.Duration
	retryBurst    int

	// frozenMu / frozenBounds 保存每个 (account,scope) 进入调度时冻结的候选总数
	//（C，上限公式）。冻结值经 RateLimitService.freshnessBoundsProvider 供三维新鲜度
	// 阈值公式消费（account_freshness_threshold.go / account_freshness_alert.go），
	// 写入语义归 freshness 链所有，不随 D2 探针相位退役（方案 th-kira-quota-lifecycle §7）。
	frozenMu     sync.Mutex
	frozenBounds map[string]int

	// freshnessAlerts 是 D4 陈旧告警服务（可选注入）。
	freshnessAlerts *FreshnessAlertService
	// channelFreshness 是 E39 渠道维陈旧收敛窄面（可选注入）。账号恢复成功后触发关联
	// 渠道陈旧收敛；未注入时不收敛。
	channelFreshness ChannelFreshnessRefresher

	// retryInFlightMu / retryInFlight 是补探账号级 in-flight 去重集合（AC-C1 异步化后
	// 必需）：同一账号的补探正在异步执行时，新一轮 sweep 跳过该账号的本轮补探，避免
	// 同账号补探重叠。复用既有 wg 模式，不引入新 goroutine 管理机制。
	retryInFlightMu sync.Mutex
	retryInFlight   map[int64]struct{}
}

// SetChannelFreshnessRefresher 注入 E39 渠道维陈旧收敛窄面（可选）。未注入时不收敛。
func (p *AccountHealthRecoveryProbeService) SetChannelFreshnessRefresher(r ChannelFreshnessRefresher) {
	if p != nil {
		p.channelFreshness = r
	}
}

// SetFreshnessAlertService 注入 D4 陈旧告警服务（可选）。未注入时不做陈旧评估。
func (p *AccountHealthRecoveryProbeService) SetFreshnessAlertService(svc *FreshnessAlertService) {
	if p != nil {
		p.freshnessAlerts = svc
	}
}

// NewAccountHealthRecoveryProbeService constructs the recovery probe service.
func NewAccountHealthRecoveryProbeService(
	accountRepo accountHealthProbeRepository,
	httpUpstream HTTPUpstream,
	cfg *config.Config,
	rateLimit *RateLimitService,
	settingService *SettingService,
	tlsFPProfileService *TLSFingerprintProfileService,
) *AccountHealthRecoveryProbeService {
	return &AccountHealthRecoveryProbeService{
		accountRepo:         accountRepo,
		httpUpstream:        httpUpstream,
		cfg:                 cfg,
		rateLimit:           rateLimit,
		settingService:      settingService,
		tlsFPProfileService: tlsFPProfileService,
		frozenBounds:        make(map[string]int),
		retryInFlight:       make(map[int64]struct{}),
	}
}

// Start launches the probe sweep loop. The loop runs continuously and re-reads
// the probe switch on every tick (see RunOnce's per-class enable checks), so toggling
// the switch in admin settings takes effect within one interval (<= IntervalSeconds,
// default 15s, AC-C1: 旧 60s) without a process restart. It is safe to call multiple
// times (only the first effective start spawns a goroutine) and terminates via Stop.
func (p *AccountHealthRecoveryProbeService) Start(ctx context.Context) {
	// settingService 为 nil 时失败关闭安全返回（不启动探测循环），与旧 probeEnabled
	// 的 `p == nil || p.settingService == nil → return` 同款：重构使能门禁时该保护被
	// 移除，若不在此短路，下方解引用 nil 会 panic（第三轮终审 #7 回归）。
	if p == nil || p.settingService == nil {
		return
	}
	p.startMu.Lock()
	defer p.startMu.Unlock()
	// 停止期间（stopping 独占关闭权）或已在运行（stopCh 非空）一律拒绝重启：保证
	// wg.Add 只在锁内 stopCh==nil 且 !stopping 时发生，永不与 Stop 的 wg.Wait 交错。
	if p.stopCh != nil || p.stopping {
		return
	}

	// 探测 sweep 间隔：默认 15s（AC-C1：旧 60s），settings.Probe.IntervalSeconds
	// 经 normalize 已 clamp 到 [15,600]；>= 15 时采用配置值（外审实证：clamp 与
	// 默认值都改、此处不改仍跑 60s）。读取失败或 Probe 未配置时回落默认 15s。
	settings, serr := p.settingService.GetOpenAIAPIKeyHealthBreakerSettings(ctx)
	interval := p.probeIntervalFromSettings(settings, serr)

	p.stopCh = make(chan struct{})
	// 捕获当时有效的 stop channel 引用（AC-G2R-3）：主循环全程使用此局部引用，不再每轮
	// 重读 p.stopCh 字段。Stop 在锁内置 nil 仅用于防止新等待者读到已关闭的 channel，而
	// 已启动的主循环持有本处捕获的引用，故 Stop 置 nil 后运行中循环仍能收到关闭信号。
	stopCh := p.stopCh
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		logger.L().Info("openai.apikey_health_probe_started", zap.Duration("interval", interval))
		for {
			select {
			case <-stopCh:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.RunOnce(ctx)
			}
		}
	}()
}

// Stop terminates the probe loop if running. It must be called from the process
// graceful-shutdown path so the goroutine does not leak.
//
// 重入竞态修复（AC-G2R4 第 5 轮 must_fix）：Stop 在开始关闭路径前于锁内置 stopping=true
// 并取走 stopCh（置 nil 声明关闭权），锁外 close + wg.Wait；期间任何并发 Start 在锁内被
// stopping 挡住，绝不执行 wg.Add，从而彻底消除「Stop 的 wg.Wait 与 Start 的 wg.Wait 交错
// 并发 Add」这一 WaitGroup 误用。第二个并发 Stop 在锁内读到 stopping 已置位即直接返回，不再
// 重复关闭。wg.Wait 完成后在锁内清 stopping，恢复 Start 可用。
func (p *AccountHealthRecoveryProbeService) Stop() {
	if p == nil {
		return
	}
	p.startMu.Lock()
	if p.stopping {
		// 第二个并发 Stop：首个 Stop 已独占关闭权，本调用直接返回（不再重复 close/等待）。
		p.startMu.Unlock()
		return
	}
	p.stopping = true
	stopCh := p.stopCh
	// 取走 stopCh：声明关闭权（与 AC-G2R-3 同型），并发的第二个 Stop 因 stopping 已置位
	// 不会再进入本分支。主循环与补探 goroutine 均持有各自启动时捕获的 stopCh 引用，
	// 不受此处置 nil 影响，仍能由下方 close 唤醒。
	p.stopCh = nil
	p.startMu.Unlock()
	if stopCh != nil {
		// 拿到关闭权的调用在锁外 close（不持锁关闭），随后等待 goroutine 退出。
		close(stopCh)
		p.wg.Wait()
	}
	// 优雅关闭完成：清独占标志，恢复 Start 可用（stopCh 已为 nil，故 Start 可在下次调用重建）。
	p.startMu.Lock()
	p.stopping = false
	p.startMu.Unlock()
}

// RunOnce performs a single probe sweep over the currently cooldowned accounts.
//
// 旧「按候选类拆分使能门禁」中 TokenHarbor 免费档模型级相位（D2）已随
// th-kira-quota-lifecycle 方案 §7 旧链退役：该相位以裸模型名 ping 上游烧毁订阅额度，
// 其语义由 cn_quota_lifecycle_service.go 的额度耗尽状态机确认探针收编（429 分类
// isTokenHarborFreeTierExhausted 归状态机所有）。剩余唯一相位为账号级
// （temp-unschedulable/熔断）探测，维持双开关 ProbeEnabledForCandidate(
// ProbeCandidateCircuitBreaker) 门禁。settings 读取失败维持现状 err != nil → return。
func (p *AccountHealthRecoveryProbeService) RunOnce(ctx context.Context) {
	// settingService 为 nil 时与 p == nil / accountRepo == nil 同款失败关闭：重构使能门禁时
	// 移除了旧 probeEnabled 对 nil settingService 的保护，此处必须显式短路，否则下一行
	// 解引用 nil 会 panic（第三轮终审 #7 回归）。
	//
	// 其余依赖字段核查：httpUpstream/cfg/rateLimit/tlsFPProfileService/freshnessAlerts/
	// accountRepo 均已在各自解引用点做 nil 保护；frozenBounds 由
	// NewAccountHealthRecoveryProbeService 构造时初始化，生产构造点唯一（ProvideAccount...
	// 经构造器），既有测试引用处亦均显式赋值，故不再重复加守卫。
	if p == nil || p.accountRepo == nil || p.settingService == nil {
		return
	}
	settings, err := p.settingService.GetOpenAIAPIKeyHealthBreakerSettings(ctx)
	if err != nil || settings == nil || settings.Probe == nil {
		return
	}
	maxAttempts := settings.Probe.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	now := time.Now()

	// 账号级（temp-unschedulable/熔断）相位：维持双开关门禁。
	// 方案 §7 旧链退役后相位列表仅此一项；D2 TokenHarbor 模型级相位已删除，
	// 未来新增相位须在此追加独立分支并保持各自使能门禁（显式空处理：无相位时
	// 循环体不执行、孤儿告警清扫照常进行，见下方 freshnessAlerts 分支）。
	if ProbeEnabledForCandidate(settings, ProbeCandidateCircuitBreaker) {
		candidates, lerr := p.accountRepo.ListTempUnschedulableAccounts(ctx, now, accountHealthProbeCandidateLimit)
		if lerr != nil {
			logger.L().Warn("openai.apikey_health_probe_list_failed", zap.Error(lerr))
			return
		}
		// 首探失败的账号：主循环结束后按短间隔补探（不再空等一个完整轮次）。
		pendingRetry := make([]*Account, 0, len(candidates))
		// pendingUntils 记录每个入队补探账号在「入队时」读到的 temp_unschedulable_until
		// （完整 time.Time，全精度），作为后续条件清除/attempt 回写的不可变代际令牌
		// （AC-G2R5 must_fix / AC-G2R6 子秒精度）：用 until 作令牌后，同一次停调内
		// bumpProbeAttempts 只改 reason 不改 until，补探成功时令牌恒命中；他因接管（写更晚
		// until）必令令牌失配、保留新停调。补探 goroutine 每次回读的是最新 until，若探测期间
		// until 被他人改写，用最新 until 作令牌会误清新停调；故条件清除/attempt 回写一律用
		// 入队时读到的令牌。
		pendingUntils := make(map[int64]time.Time)
		retryInterval := p.probeRetryInterval()
		retryBurst := p.probeRetryBurst()
		for _, acc := range candidates {
			if !p.isHealthBreakerTrip(acc, now) {
				continue
			}
			// 补探互斥（AC-G2R-2）：同账号在途（含补探中）时本轮 sweep 完全跳过该账号：
			// 不首探、不入队补探。先查 in-flight（补探 burst 仍在异步执行中），命中即
			// 跳过（含冻结上界也不写入，避免误缩短尾部候选阈值）。该检查须在首探之前，
			// 否则上一轮补探仍在进行时本轮又对该账号做首探，造成同账号探测重叠。
			if p.isRetryInFlight(acc.ID) {
				logger.L().Info("openai.apikey_health_probe_retry_inflight_skip_sweep",
					zap.Int64("account_id", acc.ID),
					zap.Int("retry_burst", retryBurst),
				)
				continue
			}
			// 冻结账号级候选总数（freshness 链既有机制，D-QL-007 F1 恢复：删除 D2
			// 相位时该写入者被一并删除，但 EvaluateAccountLevelFreshness 仍经
			// AccountFreshnessUpperBound(accountID, "") 读 frozenBounds——缺写入者则
			// 账号级上界恒 0、阈值退化为基线）。候选总数=1：账号级相位每账号每轮
			// 至多一个候选，与预算管理器删除后的现状语义一致；旧 D2 按 scope 数+
			// 账号级计数冻结的公平旋转口径已随相位退役。冻结值生命周期由
			// clearCandidateBound（probeAccount 恢复路径 :365）负责清除，"首冻结
			// 不覆盖"语义保持。
			p.freezeCandidateBound(acc.ID, "", 1)
			if stillParked := p.probeAccount(ctx, acc, maxAttempts); stillParked {
				// 账号级 in-flight 去重（AC-C1 异步化后必需）：首探失败后才标记该账号
				// 补探在途并入队；markRetryInFlight 返回 false（理论上方已先查
				// isRetryInFlight，此处不会发生）仅作防御性跳过，避免同账号补探重叠。
				if p.markRetryInFlight(acc.ID) {
					pendingRetry = append(pendingRetry, acc)
					pendingUntils[acc.ID] = tempUnschedUntil(acc)
					logger.L().Info("openai.apikey_health_probe_retry_scheduled",
						zap.Int64("account_id", acc.ID),
						zap.Duration("retry_interval", retryInterval),
						zap.Int("retry_burst", retryBurst),
					)
				} else {
					logger.L().Info("openai.apikey_health_probe_retry_inflight_skip",
						zap.Int64("account_id", acc.ID),
						zap.Int("retry_burst", retryBurst),
					)
				}
			}
		}
		// 补探相位：先让所有候选都拿到一次首探（公平），再对失败者异步连补
		//（AC-C1 异步化：独立 goroutine，纳入 wg；Stop 等待语义不变；不阻塞本 sweep）。
		if len(pendingRetry) > 0 {
			// 补探 spawn 收进锁段（AC-G2R5 must_fix2）：读 stopCh、判 stopping、wg.Add、
			// 与「清理本轮已标 in-flight 标记」全部在 startMu 锁内完成——彻底消除「锁外
			// wg.Add 与 Stop 的 wg.Wait 交错」（sync.WaitGroup 禁止 Wait 仍挂起时并发 Add）。
			// 锁内若发现 stopCh 已取走（Stop 已声明关闭权）或 stopping=true（Stop 正在关闭），
			// 则绝不入队补探，并清掉本轮已 mark 的 in-flight 标记后直接跳过（runProbeRetryBurst
			// 的 defer 清理未启动故不会执行，遗漏则后续 sweep 凭 in-flight 标记永久跳过该账号）；
			// 否则捕获 stopCh 引用、锁内 wg.Add，再解锁启动 goroutine——goroutine 持有锁外独立
			// 引用，不受本锁/Stop 影响（AC-G2R-3 同型：避免 Stop 置 nil 后 close 失灵、wg.Wait
			// 挂死）。
			p.startMu.Lock()
			stopCh := p.stopCh
			if p.stopping {
				// Stop 进行中（stopping=true，Stop 已在锁内置独占关闭权并即将/正在
				// wg.Wait）：不入队补探、清掉本轮已 mark 的 in-flight 标记后直接跳过，
				// 杜绝「锁外 wg.Add 与 Stop 的 wg.Wait 交错」（sync.WaitGroup 禁止 Wait
				// 仍挂起时并发 Add）。stopCh 是否为 nil 不影响本判定——guard 只看 stopping，
				// 故未调用 Start（stopCh 为 nil）的直驱 RunOnce 路径仍可正常补探。
				p.startMu.Unlock()
				for _, acc := range pendingRetry {
					p.clearRetryInFlight(acc.ID)
				}
			} else {
				p.wg.Add(1)
				p.startMu.Unlock()
				go func(stopCh chan struct{}) {
					defer p.wg.Done()
					p.runProbeRetryBurst(ctx, pendingRetry, pendingUntils, maxAttempts, stopCh)
				}(stopCh)
			}
		}
	}

	// E45：孤儿告警清扫（候选过期转非活动的账号+模型维）。以 firing 新鲜度告警为界反向核对
	// 限流条目解析态，关闭再无评估/关闭路径的孤儿告警。错误只记日志，不阻断探测主路径
	// （与 evaluateAccountFreshness 调用方同口径）。
	if p.freshnessAlerts != nil {
		if sweepErr := p.freshnessAlerts.CloseOrphanedFreshnessAlerts(ctx); sweepErr != nil {
			logger.L().Warn("freshness_orphan_sweep_failed", zap.Error(sweepErr))
		}
	}
}

// isHealthBreakerTrip reports whether the account's active temp-unschedulable block
// is probe-recoverable (opened by the health breaker or by the pool-mode 401
// escalation, 方案 T5 marker 放行) and has not yet expired.
func (p *AccountHealthRecoveryProbeService) isHealthBreakerTrip(acc *Account, now time.Time) bool {
	if acc == nil || acc.TempUnschedulableReason == "" {
		return false
	}
	if acc.TempUnschedulableUntil == nil || !acc.TempUnschedulableUntil.After(now) {
		return false
	}
	state, ok := parseHealthBreakerState(acc.TempUnschedulableReason)
	if !ok || state == nil {
		return false
	}
	for _, marker := range probeRecoverableMarkers {
		if state.MatchedKeyword == marker {
			return true
		}
	}
	return false
}

// parseHealthBreakerState parses the JSON reason payload written by the breaker.
func parseHealthBreakerState(reason string) (*TempUnschedState, bool) {
	if strings.TrimSpace(reason) == "" {
		return nil, false
	}
	var state TempUnschedState
	if err := json.Unmarshal([]byte(reason), &state); err != nil {
		return nil, false
	}
	return &state, true
}

// probeAccount 探测一次并尝试恢复；代际令牌取探测开始时读到的 until
// （acc.TempUnschedulableUntil，全精度 time.Time）。保留三参签名以兼容既有直接调用方；补探异步
// 路径需传入其入队时读到的 until，故走 probeAccountWithReason。
//
// 返回值 stillParked 报告本次尝试后账号是否仍处于停调且未达 max_attempts 上限
// （即是否值得短间隔补探）：真实探测成功并完成恢复提交、或已到达尝试上限时返回 false。
// 「首次真实探测成功即解除停调」——不再要求连续两轮成功（旧 consecutive_successes
// 二次确认已移除）：恢复判据回到「一次真实探测成功」，把恢复时长从冷却到期拉回秒级。
func (p *AccountHealthRecoveryProbeService) probeAccount(ctx context.Context, acc *Account, maxAttempts int) (stillParked bool) {
	return p.probeAccountWithReason(ctx, acc, maxAttempts, tempUnschedUntil(acc))
}

// tempUnschedUntil 返回账号当前 temp_unschedulable_until 的完整 time.Time（代际令牌，
// 全精度）。until 为 nil（不应出现在已入队候选上）时回落零值 time.Time——零值与「until IS
// NOT NULL」守卫组合，保证失配、不误清（见 ClearTempUnschedulableIfGeneration）。
func tempUnschedUntil(acc *Account) time.Time {
	if acc == nil || acc.TempUnschedulableUntil == nil {
		return time.Time{}
	}
	return *acc.TempUnschedulableUntil
}

// probeAccountWithReason 与 probeAccount 同语义，但代际令牌 expectedUntil 由调用方显式
// 传入（完整 time.Time，全精度）。补探异步路径（runProbeRetryBurst）传账号入队时读到的 until，
// 而非每次回读的最新 until——若探测期间 until 被他人改写（新停调接管，必写更晚 until），回读的
// 最新 until 已不同，用其作令牌会误清新停调；用入队令牌则条件清除命中失配、rows==0、保留新停调
// （见 ClearTempUnschedulableIfGeneration）。
func (p *AccountHealthRecoveryProbeService) probeAccountWithReason(ctx context.Context, acc *Account, maxAttempts int, expectedUntil time.Time) (stillParked bool) {
	state, ok := parseHealthBreakerState(acc.TempUnschedulableReason)
	if !ok || state == nil {
		return false
	}
	attempts := state.ProbeAttempts + 1
	if attempts > maxAttempts {
		// Reached the attempt ceiling: give up probing and let the cooldown expire.
		logger.L().Info("openai.apikey_health_probe_gave_up",
			zap.Int64("account_id", acc.ID),
			zap.Int("attempt", attempts),
			zap.Int("max_attempts", maxAttempts),
		)
		return false
	}

	ok, err := p.runProbe(ctx, acc)
	// F：账号级候选观察时间（两维度，同一状态入口）。既有的 ClearTempUnschedulable
	// 恢复路径保持不变，这里仅追加记录账号级 observed_at / attempted_at。
	// #2：观测提交失败透传（与 E19 口径一致：提交确认成功才允许清理状态）。
	obsErr := p.recordAccountLevelObservation(ctx, acc.ID, ok, err)
	if err != nil {
		// Degraded platform (no safe/cheap endpoint): stop probing, fall back to expiry.
		logger.L().Warn("openai.apikey_health_probe_degraded",
			zap.Int64("account_id", acc.ID),
			zap.Error(err),
		)
		// E49：探测错误分支下，账号级权威观测未写入同样需结构化暴露
		//（与 err==nil 路径的 openai.apikey_health_probe_commit_failed 同口径），
		// 否则该观测提交失败不可观测。不改状态迁移：degraded Warn、bumpProbeAttempts、
		// return、parked/失败关闭语义全部不变。
		if obsErr != nil {
			logger.L().Warn("openai.apikey_health_probe_commit_failed",
				zap.Int64("account_id", acc.ID),
				zap.Error(obsErr),
			)
		}
		// attempt 回写加代际守卫（AC-G2R6 must_fix）：用入队令牌回写，若代际已被接管
		//（rows==0）则终止本轮补探（它在清的停调已不属于本代），记 Info。
		if !p.bumpProbeAttempts(ctx, acc.ID, maxAttempts, expectedUntil) {
			logger.L().Info("probe attempt bump skipped: temp-unsched generation taken over (degraded)",
				zap.Int64("account_id", acc.ID),
				zap.Time("expected_until", expectedUntil),
			)
			return false
		}
		return true
	}

	// #2：非 success outcome（探测失败/降级）且观测写错误 → Warn 后继续既有失败簿记
	//（attempted_at 丢失可容忍，状态无迁移，账号仍 parked）。
	if !ok && obsErr != nil {
		logger.L().Warn("openai.apikey_health_probe_commit_failed",
			zap.Int64("account_id", acc.ID),
			zap.Error(obsErr))
	}

	if ok {
		// #2：成功 outcome 但观测提交失败 → 失败关闭（E19 同口径）：保持 parked，
		// 不 ClearTempUnschedulable、不 clearCandidateBound、不计成功，账号仍
		// temp-unschedulable 直到下次探测成功提交——否则状态分裂（观测未提交而恢复已发生）。
		if obsErr != nil {
			logger.L().Warn("openai.apikey_health_probe_commit_failed",
				zap.Int64("account_id", acc.ID),
				zap.Error(obsErr))
			return true
		}
		// AC-G2R4 第 5 轮 must_fix / AC-G2R6 子秒精度：清除所有权——探测成功提交恢复时
		// 改条件清除。用探测开始/入队时读到的 until（全精度 time.Time）作为代际令牌，调用
		// 仓储层原子条件清除：仅当当前 until 仍等于令牌时才清停调（rows>0）；若 until 已被
		// 他人改写（新停调接管，rows==0）则不清除、记 Info 日志，账号保持新停调，避免错清
		// 他链刚写入的状态。
		if p.accountRepo != nil {
			cleared, clearErr := p.accountRepo.ClearTempUnschedulableIfGeneration(ctx, acc.ID, expectedUntil)
			if clearErr != nil {
				logger.L().Warn("openai.apikey_health_probe_clear_failed",
					zap.Int64("account_id", acc.ID),
					zap.Error(clearErr),
				)
				return
			}
			if !cleared {
				logger.L().Info("probe skipped clear: temp-unsched generation taken over",
					zap.Int64("account_id", acc.ID),
					zap.Time("expected_until", expectedUntil),
				)
				return true
			}
			// 池模式 401 升级停调的探测恢复出口（方案 T5 条目 4，R4-F3）：清停调
			// 成功后重置该账号的 401 窗口状态机——否则同一 epoch 内再次 401 不再
			// 跨越边沿，账号失去升级保护。健康熔断 marker 不触碰池窗口。
			if state.MatchedKeyword == PoolMode401EscalationMarker {
				resetPool401State(acc.ID)
			}
			// E39：账号恢复成功后触发关联渠道维陈旧收敛（最佳努力，失败只记日志，
			// 不回滚/不阻断恢复结论；恢复链提交在先，与本调用无关）。
			if p.channelFreshness != nil {
				if ferr := p.channelFreshness.RefreshChannelFreshnessForAccount(ctx, acc.ID); ferr != nil {
					logger.L().Warn("openai.apikey_health_probe_channel_freshness_refresh_failed",
						zap.Int64("account_id", acc.ID),
						zap.Error(ferr),
					)
				}
			}
		}
		// 账号级恢复成功：清除账号级（无模型键）冻结上界，下一轮进入调度重新冻结。
		p.clearCandidateBound(acc.ID, "")
		logger.L().Info("openai.apikey_health_probe_recovered",
			zap.Int64("account_id", acc.ID),
			zap.Int("attempt", attempts),
		)
		return false
	}

	// Probe failed: attempt 回写加代际守卫（AC-G2R6 must_fix）。代际失配（已被接管，
	// rows==0）→ 终止本轮补探（它在清的停调已不属于本代），记 Info 并不再补探该账号。
	bumped := p.bumpProbeAttempts(ctx, acc.ID, attempts, expectedUntil)
	if !bumped {
		logger.L().Info("probe attempt bump skipped: temp-unsched generation taken over",
			zap.Int64("account_id", acc.ID),
			zap.Time("expected_until", expectedUntil),
		)
		return false
	}
	logger.L().Info("openai.apikey_health_probe_failed",
		zap.Int64("account_id", acc.ID),
		zap.Int("attempt", attempts),
		zap.Int("max_attempts", maxAttempts),
	)
	return true
}

// bumpProbeAttempts rewrites the temp-unschedulable reason to record the new attempt
// count, guarded by the generation token (AC-G2R6 must_fix). It returns true when the
// write-back succeeded (the block still belongs to this generation) and false when the
// generation was taken over by another writer (rows==0: temp_unschedulable_until no
// longer matches expectedUntil), in which case the caller should stop retrying this
// account for the current round. A store failure only skips this cycle's bookkeeping
// and is reported as true so a transient DB error does not prematurely abort probing.
func (p *AccountHealthRecoveryProbeService) bumpProbeAttempts(ctx context.Context, accountID int64, attempts int, expectedUntil time.Time) bool {
	if p.accountRepo == nil {
		return false
	}
	acc, err := p.accountRepo.GetByID(ctx, accountID)
	if err != nil || acc == nil {
		return false
	}
	state, ok := parseHealthBreakerState(acc.TempUnschedulableReason)
	if !ok || state == nil {
		return false
	}
	state.ProbeAttempts = attempts
	updated, mErr := json.Marshal(state)
	if mErr != nil {
		return false
	}
	// attempt 回写加代际守卫（AC-G2R6 must_fix）：仅当 until 仍等于入队令牌且仍处有效
	// 停调窗口时才改写 reason。代际失配（已被他因接管，必写更晚 until）→ 返回 false，
	// 调用方据此终止本轮补探、保留接管者 reason；DB 写入失败按 best-effort 处理、返回 true
	//（不因瞬时 DB 错误误杀补探）。
	bumped, bErr := p.accountRepo.BumpTempUnschedulableReasonIfGeneration(ctx, accountID, expectedUntil, string(updated))
	if bErr != nil {
		logger.L().Debug("openai.apikey_health_probe_attempt_update_failed",
			zap.Int64("account_id", accountID), zap.Error(bErr))
	}
	return bumped
}

// probeRetryInterval 返回补探间隔：未显式注入（零值）时回落到默认常量。
func (p *AccountHealthRecoveryProbeService) probeRetryInterval() time.Duration {
	if p != nil && p.retryInterval > 0 {
		return p.retryInterval
	}
	return accountHealthProbeRetryInterval
}

// probeRetryBurst 返回单轮 sweep 内每个失败账号的最大补探次数：零值回落到默认常量。
func (p *AccountHealthRecoveryProbeService) probeRetryBurst() int {
	if p != nil && p.retryBurst > 0 {
		return p.retryBurst
	}
	return accountHealthProbeRetryBurst
}

// probeIntervalFromSettings 解析探测 sweep 间隔（AC-C1：旧默认 60s → 15s）。
// 规则：settings.Probe 未配置或读取失败时回落默认 15s；IntervalSeconds >= 15
// （经 normalize 已 clamp 到 [15,600]）时采用配置值，否则回落默认 15s。外审实证
// 三道门禁（clamp 下限、本函数默认、Start 内 >=15 判断）须同步改，只改其一仍跑 60s。
func (p *AccountHealthRecoveryProbeService) probeIntervalFromSettings(settings *OpenAIAPIKeyHealthBreakerSettings, err error) time.Duration {
	const defaultInterval = 15 * time.Second
	if err != nil || settings == nil || settings.Probe == nil {
		return defaultInterval
	}
	if settings.Probe.IntervalSeconds >= 15 {
		return time.Duration(settings.Probe.IntervalSeconds) * time.Second
	}
	return defaultInterval
}

// markRetryInFlight 标记某账号补探 in-flight（AC-C1 异步化后同账号去重）。返回 true
// 表示本次成功入队（该账号此前无在途补探）；返回 false 表示该账号已有补探在途，
// 调用方应跳过本轮补探以免重叠。并发安全（retryInFlightMu 保护）。
func (p *AccountHealthRecoveryProbeService) markRetryInFlight(id int64) bool {
	if p == nil {
		return false
	}
	p.retryInFlightMu.Lock()
	defer p.retryInFlightMu.Unlock()
	if p.retryInFlight == nil {
		p.retryInFlight = make(map[int64]struct{})
	}
	if _, ok := p.retryInFlight[id]; ok {
		return false
	}
	p.retryInFlight[id] = struct{}{}
	return true
}

// isRetryInFlight 只读报告某账号是否已有补探在途（AC-G2R-2：同账号去重查询）。与
// markRetryInFlight 配对：sweep 首探前先查本函数，命中则整账号跳过（不首探、不入队
// 补探）；markRetryInFlight 在首探失败后才设置 in-flight 标记。并发安全
// （retryInFlightMu 保护）。
func (p *AccountHealthRecoveryProbeService) isRetryInFlight(id int64) bool {
	if p == nil || p.retryInFlight == nil {
		return false
	}
	p.retryInFlightMu.Lock()
	defer p.retryInFlightMu.Unlock()
	_, ok := p.retryInFlight[id]
	return ok
}

// clearRetryInFlight 清除某账号的补探 in-flight 标记（补探完成/放弃后调用，允许后续
// sweep 重新纳入补探）。并发安全。
func (p *AccountHealthRecoveryProbeService) clearRetryInFlight(id int64) {
	if p == nil {
		return
	}
	p.retryInFlightMu.Lock()
	defer p.retryInFlightMu.Unlock()
	delete(p.retryInFlight, id)
}

// runProbeRetryBurst 对本轮首探失败的账号按短间隔补探（AC-C1 起由 RunOnce 在独立
// goroutine 异步调用，纳入 wg；不阻塞主 sweep），直到某个账号探测成功（由 probeAccount
// 内部完成解除停调）、账号不再处于停调（已被他实例恢复或冷却到期）、达到本轮补探次数
// 上限（默认 50，受 settings.probe.max_attempts 双重约束，生产配置 100），或触及
// settings.probe.max_attempts（由 probeAccount 判定 gave_up）。补探与主循环共用同一
// probeAccount 路径与 max_attempts 簿记，故补探不会绕过真实探测成功这一恢复前提，
// 也不会无限重试。退出时清除各账号的补探 in-flight 标记（accountHealthProbeRetryBurst
// 去重），允许后续 sweep 重新纳入补探。untils 为各账号「入队时」读到的
// temp_unschedulable_until（完整 time.Time，全精度，不可变代际令牌），供条件清除/attempt
// 回写使用（见 probeAccountWithReason / ClearTempUnschedulableIfGeneration /
// BumpTempUnschedulableReasonIfGeneration）。
func (p *AccountHealthRecoveryProbeService) runProbeRetryBurst(ctx context.Context, pending []*Account, untils map[int64]time.Time, maxAttempts int, stopCh chan struct{}) {
	if p == nil || p.accountRepo == nil || len(pending) == 0 {
		return
	}
	// 入队时固定原始账号集合快照，供 defer 清理使用：pending 在每轮补探后被重赋值为
	// 仅含仍需补探账号的 next（:602-633），补探成功/达上限/回读后不再停调/回读失败的账号
	// 退出队列后若仍按 pending 清理则标记永不清，后续 sweep 在 :264-278 凭 in-flight 标记
	// 永久跳过该账号、永不再对其探测。改为遍历入队时固定的原始集合，确保所有被标记
	// in-flight 的账号最终都能被清理。
	enqueued := make([]*Account, len(pending))
	copy(enqueued, pending)
	// 退出时清除本轮补探账号的 in-flight 标记，避免同账号补探去重误伤后续 sweep。
	defer func() {
		for _, acc := range enqueued {
			p.clearRetryInFlight(acc.ID)
		}
	}()
	interval := p.probeRetryInterval()
	burst := p.probeRetryBurst()
	for round := 1; round <= burst && len(pending) > 0; round++ {
		if !p.sleepBeforeProbeRetry(ctx, stopCh, interval) {
			return
		}
		next := make([]*Account, 0, len(pending))
		for _, acc := range pending {
			if ctx.Err() != nil {
				return
			}
			// 回读最新账号：补探前必须确认账号仍处于可探测停调态（他实例可能已恢复，
			// 冷却也可能已到期），且要拿到最新的 ProbeAttempts 计数（max_attempts 判据）。
			fresh, err := p.accountRepo.GetByID(ctx, acc.ID)
			if err != nil || fresh == nil {
				continue
			}
			if !p.isHealthBreakerTrip(fresh, time.Now()) {
				continue
			}
			// 补探序号与 probeAccount 内的 attempts 同口径（本次尝试后的累计次数），
			// 便于按 account_id+attempt 串联「首探→补探→恢复」日志链（事故复盘时
			// 恢复时长抖动需逐次定位）；解析失败退回 round+1，仅影响日志不阻塞补探。
			attempt := round + 1
			if st, ok := parseHealthBreakerState(fresh.TempUnschedulableReason); ok && st != nil {
				attempt = st.ProbeAttempts + 1
			}
			logger.L().Info("openai.apikey_health_probe_retry",
				zap.Int64("account_id", acc.ID),
				zap.Int("round", round),
				zap.Int("attempt", attempt),
				zap.Duration("interval", interval),
			)
			// 代际令牌取入队时读到的 until（untils，全精度 time.Time），而非回读的最新
			// until：若探测期间 until 被他人改写（新停调接管，必写更晚 until），用最新
			// until 作令牌会误清新停调；用入队令牌则条件清除命中失配、rows==0、保留新停调
			//（AC-G2R5 must_fix / AC-G2R6 子秒精度）。
			expectedUntil := tempUnschedUntil(fresh)
			if u, ok := untils[acc.ID]; ok {
				expectedUntil = u
			}
			if p.probeAccountWithReason(ctx, fresh, maxAttempts, expectedUntil) {
				next = append(next, fresh)
			}
		}
		pending = next
	}
	if len(pending) > 0 {
		logger.L().Info("openai.apikey_health_probe_retry_burst_exhausted",
			zap.Int("accounts", len(pending)),
			zap.Int("rounds", burst),
			zap.Duration("interval", interval),
		)
	}
}

// sleepBeforeProbeRetry 等一个补探间隔；ctx 取消或 Stop() 触发时立即返回 false
// （不把补探等待拖进优雅关闭路径）。stopCh 由补探 goroutine 启动时捕获并全程持有，
// 不在此重新读取 p.stopCh（否则 Stop 先置 nil 后本函数取到 nil channel、close 失灵、
// 睡眠不可唤醒、wg.Wait 挂死，AC-G2R-3）。stopCh 为 nil 时（RunOnce 未经 Start 直接
// 调用，无关闭路径）由 timer 正常驱动，不影响补探节奏。
func (p *AccountHealthRecoveryProbeService) sleepBeforeProbeRetry(ctx context.Context, stopCh chan struct{}, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-stopCh:
		return false
	case <-timer.C:
		return true
	}
}

// runProbe executes the upstream probe, preferring a real request but allowing an
// override for tests.
func (p *AccountHealthRecoveryProbeService) runProbe(ctx context.Context, acc *Account) (bool, error) {
	if p.probeOverride != nil {
		return p.probeOverride(ctx, acc)
	}
	return p.probeUpstream(ctx, acc)
}

// probeUpstream sends a cheap real upstream request through the account's own
// forwarding path. It prefers the zero-cost /models endpoint; if that cannot be
// constructed safely (e.g. a platform without a /models surface) it returns an error
// so the caller degrades to expiry-based recovery rather than spamming probes.
//
// Limitation: for some transit/proxy sites the /models endpoint is served by a
// lightweight front-end that does NOT exercise the heavy chat upstream. There a 2xx
// from /models can be green even while chat completions are still failing, so a
// successful probe would clear the block prematurely. This is mitigated by the
// probe being opt-in (default off); operators of such sites should keep probe
// disabled and rely on expiry-based recovery, or only enable it where /models and
// chat share the same upstream path.
func (p *AccountHealthRecoveryProbeService) probeUpstream(ctx context.Context, acc *Account) (bool, error) {
	if p.httpUpstream == nil || p.cfg == nil {
		return false, errors.New("probe transport not configured")
	}
	// AC-B1：CodeBuddy 平台账号（影子或直绑）没有 /models 面——buildOpenAIAPIKeyModelsRequest
	// 要求 type==apikey，oauth 账号构建必失败降级到期恢复——改派 CodeBuddy 探测分发器
	//（影子走既有 chat 探测、直绑走新增 billing-meter 探测）。判定用
	// isCodeBuddyPlatformAccount（直绑 platform=codebuddy）|| isCodeBuddyShadowAccount
	//（存量影子），二者均复用既有函数，不得新写判定（方案 §三 卡 B）。
	if isCodeBuddyPlatformAccount(acc) || isCodeBuddyShadowAccount(acc) {
		return p.probeCodeBuddyUpstream(ctx, acc)
	}
	validateBaseURL := func(raw string) (string, error) {
		return cnValidateProbeURL(p.cfg, raw)
	}
	req, err := buildOpenAIAPIKeyModelsRequest(ctx, acc, validateBaseURL)
	if err != nil {
		return false, fmt.Errorf("build probe request: %w", err)
	}

	proxyURL := ""
	if acc.ProxyID != nil && acc.Proxy != nil {
		proxyURL = acc.Proxy.URL()
	}
	var tlsProfile *tlsfingerprint.Profile
	if p.tlsFPProfileService != nil {
		tlsProfile = p.tlsFPProfileService.ResolveTLSProfile(acc)
	}

	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := p.httpUpstream.DoWithTLS(req.WithContext(callCtx), proxyURL, acc.ID, acc.Concurrency, tlsProfile)
	if err != nil {
		return false, nil
	}
	defer func() { _ = resp.Body.Close() }()

	// A 2xx (including 200 from /models) confirms the account is healthy again.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
		return true, nil
	}
	// Drain to reuse the connection, then treat any non-2xx as an unhealthy probe.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
	return false, nil
}

// ---------------------------------------------------------------------------
// CodeBuddy 探测分发器（方案 §三 卡 B，AC-B1）
// ---------------------------------------------------------------------------

// probeCodeBuddyUpstream 是 CodeBuddy 平台账号（影子或直绑）的探测分发器：
//   - 影子（isCodeBuddyShadowAccount）：走既有最小流式 chat 探测
//     probeCodeBuddyShadowUpstream（零改动，请求/判定同源转发链）；
//   - 直绑（isCodeBuddyPlatformAccount 命中、非影子）：走新增 billing-meter 探测
//     probeCodeBuddyDirectUpstream——直绑账号无 extra.shadow_model，复用 chat 探测
//     必因缺模型键降级（r4 阻塞1 回修），故直绑单独用配额面。
//
// 调用方 probeUpstream 已在进入本函数前用 isCodeBuddyPlatformAccount ||
// isCodeBuddyShadowAccount 收窄，故此处只需按影子/直绑二分。
func (p *AccountHealthRecoveryProbeService) probeCodeBuddyUpstream(ctx context.Context, acc *Account) (bool, error) {
	if isCodeBuddyShadowAccount(acc) {
		return p.probeCodeBuddyShadowUpstream(ctx, acc)
	}
	return p.probeCodeBuddyDirectUpstream(ctx, acc)
}

// ---------------------------------------------------------------------------
// CodeBuddy 影子流式 chat 探测（方案 T4，R2-F1/R3-F2）
// ---------------------------------------------------------------------------

// codeBuddyShadowProbeTimeout 单次探测超时，与既有 /models 探测同款（15s）。
const codeBuddyShadowProbeTimeout = 15 * time.Second

// codeBuddyShadowProbeMaxBodyBytes 探测响应读取上限。max_tokens=1 的最小流远小于该值；
// 达到上限即视为截断流（缺 data: [DONE]）→ 探测失败（fail-closed）。
const codeBuddyShadowProbeMaxBodyBytes = 256 << 10

// codeBuddyShadowProbeSystemMessage 是探测请求的 system 消息（PrepareCodeBuddyBody
// 规则 3b 要求首条消息为 system）；内容保持极短且不含任何框架指纹。
const codeBuddyShadowProbeSystemMessage = "health probe"

// probeCodeBuddyShadowUpstream 对 CodeBuddy 影子账号发一次最小流式 chat 探测。
//
// AC-B1 起本函数语义收窄为「影子型专用」子路径：由 probeCodeBuddyUpstream 按
// isCodeBuddyShadowAccount 分派而来，直绑账号改走 probeCodeBuddyDirectUpstream
// （billing-meter 探测，直绑无 extra.shadow_model，复用本函数必降级）。
//
// 协议约束（R2-F1）：上游强制 stream:true（codebuddy_upstream.go 规则 1，非流式直发
// 会被拒绝导致探测永远失败），故探测请求必须 stream:true 并复用既有本地 SSE 聚合
// （aggregateOpenAIChatCompletionsSSE）后再判定。端点用 chat completions（站点 SSOT
// codebuddy_site.go + codeBuddyChatCompletionsPath），明确不复用 testCodeBuddyAccountConnection
// 的 billing meter 端点（只读配额面，不经过 chat 上游，探不出 chat 链路健康）。
//
// 传输栈与转发链同源：token 解析 codeBuddyTestAccessToken 同口径、影子→母账号凭证
// 透传（resolveCredentialAccount 同语义，走探测窄仓库）、站点 SSOT、UA/代理/TLS 与
// buildCodeBuddyChatRequest 一致。
//
// 成功判定为四条件（R3-F2，缺一即探测失败、保持停调）：HTTP 2xx + 聚合体为有效
// chat completion（choices 非空、无业务错误信封）+ 原始 SSE 含 data: [DONE] 正常
// 终止 + 全程无错误帧。构造/凭证类错误返回 error → 调用方降级到期恢复（既有语义）。
func (p *AccountHealthRecoveryProbeService) probeCodeBuddyShadowUpstream(ctx context.Context, acc *Account) (bool, error) {
	if p.accountRepo == nil {
		return false, errors.New("probe account repo not configured")
	}

	// 影子→母账号凭证解析：母账号的 access_token / site / uid / enterprise_id / domain
	// 才是出站的真正身份。校验语义与 resolveCredentialAccount 一致（fail-closed）。
	credAccount := acc
	if acc.ParentAccountID != nil {
		parent, err := p.accountRepo.GetByID(ctx, *acc.ParentAccountID)
		if err != nil {
			return false, fmt.Errorf("resolve codebuddy shadow parent: %w", err)
		}
		if parent == nil || parent.IsShadow() {
			return false, fmt.Errorf("codebuddy shadow parent %d missing or itself a shadow", *acc.ParentAccountID)
		}
		if !(parent.IsCodeBuddy() && (parent.Type == AccountTypeOAuth || parent.Type == AccountTypeAPIKey)) {
			return false, fmt.Errorf("codebuddy shadow parent %d is not a codebuddy OAuth/APIKey account", parent.ID)
		}
		credAccount = parent
	}

	// 模型取影子 extra.shadow_model（转发链权威模型）；缺失则无法构造 chat 探测 → 降级。
	shadowModel := strings.TrimSpace(acc.GetExtraString(ShadowModelExtraKey))
	if shadowModel == "" {
		return false, errors.New("codebuddy shadow has no shadow_model to probe")
	}

	token := codeBuddyTestAccessToken(credAccount)
	if token == "" {
		return false, errors.New("codebuddy credential account is missing access_token credential")
	}

	// 最小探测体：system-first、极短 prompt、max_tokens=1、stream:true；再过既有
	// PrepareCodeBuddyBody 归一，保证全部协议规则（强制 stream、tool_choice 归一、
	// DeepSeek thinking 注入等）与转发链一致。探测体不设 reasoning_effort，
	// SupportedEfforts 留空（规则 5 对无该字段的请求是 no-op）。
	probeBody, err := PrepareCodeBuddyBody(codeBuddyShadowProbeBody(shadowModel), CodeBuddyRewriteOptions{
		Sanitize: p.codeBuddyProbeSanitizeEnabled(),
		Model:    shadowModel,
	})
	if err != nil {
		return false, fmt.Errorf("prepare codebuddy probe body: %w", err)
	}

	req, err := buildCodeBuddyShadowProbeRequest(ctx, credAccount, probeBody, token, p.cfg)
	if err != nil {
		return false, fmt.Errorf("build codebuddy probe request: %w", err)
	}

	proxyURL := ""
	if acc.ProxyID != nil && acc.Proxy != nil {
		proxyURL = acc.Proxy.URL()
	}
	var tlsProfile *tlsfingerprint.Profile
	if p.tlsFPProfileService != nil {
		tlsProfile = p.tlsFPProfileService.ResolveTLSProfile(acc)
	}

	callCtx, cancel := context.WithTimeout(ctx, codeBuddyShadowProbeTimeout)
	defer cancel()
	resp, err := p.httpUpstream.DoWithTLS(req.WithContext(callCtx), proxyURL, acc.ID, acc.Concurrency, tlsProfile)
	if err != nil {
		// 传输失败与 /models 探测同语义：上游不可达即不健康，保持停调（非降级）。
		return false, nil
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, codeBuddyShadowProbeMaxBodyBytes))
	return evaluateCodeBuddyShadowProbeResponse(resp.StatusCode, raw), nil
}

// ---------------------------------------------------------------------------
// CodeBuddy 直绑 billing-meter 探测（方案 §三 卡 B，AC-B1；r4 阻塞1 回修）
// ---------------------------------------------------------------------------

// probeCodeBuddyDirectUpstream 对 CodeBuddy 直绑账号（platform=codebuddy、非影子）
// 发一次 billing-meter 配额面探测。
//
// 背景（r4 阻塞1）：存量 probeCodeBuddyShadowUpstream 硬依赖 extra.shadow_model，
// 直绑账号无此键，复用必失败降级。直绑用自身凭据与自身站点（acc.CodeBuddySite()，
// 不查母账号），请求形态逐字段对齐 testCodeBuddyAccountConnection
// （account_test_service.go）：POST {BillingBase}/v2/billing/meter/get-user-resource、
// body {}、CodeBuddyClientUA、Origin/Referer、X-Product: SaaS、X-User-Id/
// X-No-Enterprise-Id/X-Domain、Bearer codeBuddyTestAccessToken(acc)、15s 超时
// （复用 codeBuddyShadowProbeTimeout）。
//
// 判定（卡 B 明示）：2xx = 恢复；401/403 与任何非 2xx 一律保持停调（fail-closed），
// 与 /models 探测同语义——返回 (false, nil) 而非 error，避免误触降级到期恢复。
func (p *AccountHealthRecoveryProbeService) probeCodeBuddyDirectUpstream(ctx context.Context, acc *Account) (bool, error) {
	if p.httpUpstream == nil || p.cfg == nil {
		return false, errors.New("probe transport not configured")
	}
	token := codeBuddyTestAccessToken(acc)
	if token == "" {
		return false, errors.New("codebuddy direct account is missing access_token credential")
	}

	req, err := buildCodeBuddyDirectBillingProbeRequest(ctx, acc)
	if err != nil {
		return false, fmt.Errorf("build codebuddy direct probe request: %w", err)
	}

	proxyURL := ""
	if acc.ProxyID != nil && acc.Proxy != nil {
		proxyURL = acc.Proxy.URL()
	}
	var tlsProfile *tlsfingerprint.Profile
	if p.tlsFPProfileService != nil {
		tlsProfile = p.tlsFPProfileService.ResolveTLSProfile(acc)
	}

	callCtx, cancel := context.WithTimeout(ctx, codeBuddyShadowProbeTimeout)
	defer cancel()
	resp, err := p.httpUpstream.DoWithTLS(req.WithContext(callCtx), proxyURL, acc.ID, acc.Concurrency, tlsProfile)
	if err != nil {
		// 传输失败与 /models、chat 探测同语义：上游不可达即不健康，保持停调（非降级）。
		return false, nil
	}
	defer func() { _ = resp.Body.Close() }()

	// 卡 B 判定（AC-G2R-1 整改）：2xx ≠ 恢复。直绑 billing-meter 配额面与 web 探活
	// 同源缺陷（account_test_service.go:evaluateCodeBuddyProbeBody）——认证/业务错误
	// 随 HTTP 200 返回，空 body 也可能被误判。必须按响应体内容判定后才算恢复：仅当
	// 2xx 且 evaluateCodeBuddyProbeBody 返回 ""（健康）才清停调；2xx 但业务校验不过 =
	// 探测失败（保持停调，fail-closed，返回 false, nil 而非 error，以免误触降级到期恢复）。
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, nil
	}
	if reason := evaluateCodeBuddyProbeBody(raw); reason != "" {
		logger.L().Warn("openai.apikey_health_probe_codebuddy_direct_body_rejected",
			zap.Int64("account_id", acc.ID),
			zap.String("reason", reason),
		)
		return false, nil
	}
	return true, nil
}

// buildCodeBuddyDirectBillingProbeRequest 构造 CodeBuddy 直绑 billing-meter 探测请求，
// 请求形状与 testCodeBuddyAccountConnection（account_test_service.go）同源：
// POST {BillingBase}/v2/billing/meter/get-user-resource、body {}、指纹头
// （Content-Type/Accept/X-Requested-With/Origin/Referer/UA/X-Product）、
// Authorization: Bearer + token、身份头 X-User-Id/X-No-*: 1 占位（取自账号自身，
// 直绑不查母账号）、账号级 ApplyHeaderOverrides 最后应用。
func buildCodeBuddyDirectBillingProbeRequest(ctx context.Context, acc *Account) (*http.Request, error) {
	ep := codeBuddyEndpointsFor(acc.CodeBuddySite())
	targetURL := strings.TrimRight(ep.BillingBase, "/") + codeBuddyBillingMeterPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", ep.OriginReferer)
	req.Header.Set("Referer", ep.OriginReferer+"/")
	req.Header.Set("User-Agent", CodeBuddyClientUA)
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("Authorization", "Bearer "+codeBuddyTestAccessToken(acc))

	uid := acc.GetCredential("uid")
	enterpriseID := acc.GetCredential("enterprise_id")
	domain := acc.GetCredential("domain")
	if uid != "" {
		req.Header.Set("X-User-Id", uid)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if enterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", enterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	if domain != "" {
		req.Header.Set("X-Domain", domain)
	} else {
		req.Header.Set("X-No-Domain", "1")
	}

	acc.ApplyHeaderOverrides(req.Header)
	return req, nil
}

// codeBuddyShadowProbeBody 构造最小探测请求体（构造失败返回 nil，由
// PrepareCodeBuddyBody 的 json.Valid 校验失败关闭）。
func codeBuddyShadowProbeBody(model string) []byte {
	encoded, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": codeBuddyShadowProbeSystemMessage},
			{"role": "user", "content": "ping"},
		},
		"max_tokens": 1,
		"stream":     true,
	})
	if err != nil {
		return nil
	}
	return encoded
}

// codeBuddyProbeSanitizeEnabled 返回探测出站的 system 指纹脱敏开关，与转发链
// （OpenAIGatewayService.codeBuddySanitizeEnabled）同默认：开启。
func (p *AccountHealthRecoveryProbeService) codeBuddyProbeSanitizeEnabled() bool {
	if p != nil && p.cfg != nil {
		return p.cfg.Gateway.CodeBuddy.SanitizeEnabled
	}
	return true
}

// buildCodeBuddyShadowProbeRequest 构造 CodeBuddy 影子探测请求，请求形状与转发链
// buildCodeBuddyChatRequest 同源（§2.4 指纹头：Content-Type/Accept/X-Requested-With/
// Origin/Referer/UA/Authorization/X-Product + 身份头空值 X-No-*: 1 占位；身份头取自
// 母账号，影子自身凭证为空；母账号 ApplyHeaderOverrides 最后应用）。
//
// 独立成函数的原因：buildCodeBuddyChatRequest 挂在 OpenAIGatewayService 上（探测服务
// 不依赖该服务），且 CARD-C 文件边界不含 codebuddy_gateway_forward.go，无法无损提取
// 纯构造。两处形状必须同步演进：改动转发请求形状时须同步此处。
//
// 安全红线同源：chat 请求绝不携带 X-Refresh-Token（该头仅用于 refresh 端点）。
func buildCodeBuddyShadowProbeRequest(ctx context.Context, credAccount *Account, body []byte, token string, cfg *config.Config) (*http.Request, error) {
	ep := codeBuddyEndpointsFor(credAccount.CodeBuddySite())
	targetURL := strings.TrimRight(ep.UpstreamBase, "/") + codeBuddyChatCompletionsPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))

	ua := CodeBuddyClientUA
	if cfg != nil {
		if candidate := strings.TrimSpace(cfg.Gateway.CodeBuddy.ChatUserAgent); candidate != "" {
			ua = candidate
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", ep.OriginReferer)
	req.Header.Set("Referer", ep.OriginReferer+"/")
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Product", "SaaS")

	uid := credAccount.GetCredential("uid")
	enterpriseID := credAccount.GetCredential("enterprise_id")
	domainCred := credAccount.GetCredential("domain")
	if uid != "" {
		req.Header.Set("X-User-Id", uid)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if enterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", enterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	if domainCred != "" {
		req.Header.Set("X-Domain", domainCred)
	} else {
		req.Header.Set("X-No-Domain", "1")
	}

	credAccount.ApplyHeaderOverrides(req.Header)
	return req, nil
}

// evaluateCodeBuddyShadowProbeResponse 实现探测成功四条件（方案 T4，R3-F2）：
//  1. HTTP 2xx（仅凭状态码判定被 R1-F2 明确禁止）；
//  2. 聚合结果为有效 chat completion：aggregateOpenAIChatCompletionsSSE 聚合成功
//     （非 SSE / 无 chunk / 纯错误信封在此返回 false）、choices 非空、无业务错误信封；
//  3. 原始 SSE 含 data: [DONE] 正常终止帧；
//  4. 全程无错误帧（event: error 帧 + data 帧业务错误信封，见扫描器注释）。
//
// 条件 3/4 必不可少：聚合器见首 chunk 即置位、缺 finish_reason 会合成 stop
// （openai_chat_completions_sse_aggregate.go），「部分内容+错误帧」与截断流的聚合体
// 仍可能有非空 choices，仅验聚合体会误清熔断。
func evaluateCodeBuddyShadowProbeResponse(statusCode int, rawSSE []byte) bool {
	if statusCode < 200 || statusCode >= 300 {
		return false
	}
	aggregated, ok := aggregateOpenAIChatCompletionsSSE(rawSSE)
	if !ok {
		return false
	}
	var completion struct {
		Choices []json.RawMessage `json:"choices"`
		Code    json.RawMessage   `json:"code"`
	}
	if err := json.Unmarshal(aggregated, &completion); err != nil {
		return false
	}
	if len(completion.Choices) == 0 {
		return false
	}
	if codeBuddySSEPayloadCarriesError(completion.Code) {
		return false
	}
	return scanCodeBuddyShadowProbeSSE(rawSSE)
}

// scanCodeBuddyShadowProbeSSE 扫描原始 SSE：返回是否「正常终止且全程无错误帧」。
// 终止判定：出现 data: [DONE] 帧。错误帧判定复用 gateway_upstream_response.go
// processSSEEvent 的 sseStreamErrorEventError 语义（event 名为 error 的帧），并按方案
// 「无业务错误信封」口径把 data 帧中携带 code!=0 / error 对象的帧一并视为错误帧
// ——CodeBuddy 业务错误正是以 JSON 信封随 HTTP 200 返回（account_test_service.go
// evaluateCodeBuddyProbeBody 同款判定口径）。保守方向正确：可疑流判失败只多停一轮
// 冷却，误判成功才会错清熔断。
func scanCodeBuddyShadowProbeSSE(raw []byte) bool {
	sawDone := false
	for _, rawLine := range strings.Split(string(raw), "\n") {
		line := strings.TrimSpace(rawLine)
		switch {
		case strings.HasPrefix(line, "event:"):
			if strings.TrimSpace(strings.TrimPrefix(line, "event:")) == "error" {
				return false
			}
		case strings.HasPrefix(line, "data:"):
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				sawDone = true
				continue
			}
			if payload != "" && codeBuddySSEPayloadCarriesError([]byte(payload)) {
				return false
			}
		}
	}
	return sawDone
}

// codeBuddySSEPayloadCarriesError 判定一个 data 帧 JSON 是否为业务错误信封：
//   - 顶层 error 字段非空（OpenAI 兼容流式错误形状 {"error":{...}}；null 不算）；
//   - 顶层 code 语义与 codeBuddyProbeEnvelopeCode 同口径：数字 != 0，或字符串非空
//     且 != "0"。
//
// 正常 chat.completion.chunk 不含这两个顶层字段，无误伤面；非 JSON / 非 object
// payload 一律按非错误处理（交给聚合器与终止帧判定兜底）。
func codeBuddySSEPayloadCarriesError(payload []byte) bool {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	var envelope struct {
		Code  json.RawMessage `json:"code"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return false
	}
	if errorField := bytes.TrimSpace(envelope.Error); len(errorField) > 0 && string(errorField) != "null" {
		return true
	}
	if len(envelope.Code) == 0 {
		return false
	}
	var codeValue any
	if err := json.Unmarshal(envelope.Code, &codeValue); err != nil {
		return false
	}
	switch value := codeValue.(type) {
	case float64:
		return value != 0
	case string:
		trimmedCode := strings.TrimSpace(value)
		return trimmedCode != "" && trimmedCode != "0"
	}
	return false
}

// SetProbeOverride installs a probe function override (tests only).
func (p *AccountHealthRecoveryProbeService) SetProbeOverride(fn func(ctx context.Context, account *Account) (bool, error)) {
	if p != nil {
		p.probeOverride = fn
	}
}

// ===========================================================================
// 探测观测状态面（D2 探针相位已退役，方案 th-kira-quota-lifecycle §7）。
// 以下符号被禁区文件消费，不随相位退役：
//   - tokenHarborAccountLevelProbeScope / entryObservedAtKey / entryAttemptedAtKey /
//     ProbeOutcome：ratelimit_service.go 写入口与 account_freshness_*.go 告警/视图链；
//   - probeBudgetPerMinute / probeStartupCooldown / probeRoundPeriod /
//     probeRequestHardTimeout / ComputeProbeUpperBound：三维新鲜度阈值公式；
//   - 冻结上界族（freezeCandidateBound 等）：freshness 阈值输入，写入语义归 freshness 链。
// ===========================================================================

// tokenHarborAccountLevelProbeScope 是账号级（无模型键）维度在 model_rate_limits
// 桶中的占位 scope，与模型级 scope 互不掩盖（F：两个维度不得互相刷新）。
const tokenHarborAccountLevelProbeScope = "tokenharbor_account_level_probe"

// 探测链写入条目内的时间字段键（与 ratelimit_service.go 的写入入口共用）。
const (
	entryObservedAtKey  = "observed_at"
	entryAttemptedAtKey = "attempted_at"
)

// 上限公式常量（C/E，单一事实源）。probeBudgetWindow / probeBudgetManager 等预算
// 执行体随 D2 相位退役；probeBudgetPerMinute 仍为上限公式的分母输入。
const (
	probeStartupCooldown    = 60 * time.Second
	probeRoundPeriod        = 60 * time.Second
	probeRequestHardTimeout = 30 * time.Second
)

// ProbeOutcome 是探测观测写入口对单次上游响应的分类（D）。
type ProbeOutcome int

const (
	// ProbeOutcomeSuccess 目标模型最小请求 2xx（含慢 2xx）→ 幂等清除该 scope 限流。
	ProbeOutcomeSuccess ProbeOutcome = iota
	// ProbeOutcomeFreeTier429 识别出免费档额度用光 429 → 确认仍受限，置 observed_at（不清）。
	ProbeOutcomeFreeTier429
	// ProbeOutcomeUnclassified 5xx/超时/传输错误/其他 429/401/403/非免费档 429 → 仅置 attempted_at。
	ProbeOutcomeUnclassified
)

// ComputeProbeUpperBound 是上限公式的单一事实源（C）。两口径统一于此：
//   - 普通场景（候选总数 ≤60 且窗口无先前占用）：无额外预算等待项（0 分钟），仅
//     1×轮次周期 + 请求硬超时 + 启动冷却 60s；
//   - 最坏场景（冻结值 >60）：预算等待项 = ceil(候选总数/60) 分钟（例如 100 → 2 分钟）
//   - 1×轮次周期 + 请求硬超时 + 启动冷却。
//
// 两类共用同一计算，展示/告警/验收读同一结果，不与「普通场景无预算等待项」矛盾：
// 普通场景由候选总数 ≤60 触发 0 预算分钟分支。公式语义 = 下一次探测尝试的完成界，
// 不是有限恢复时间保证；不可分类结果只更新 attempt 时间，超出单次尝试界后由 stale
// 告警暴露，不静默丢弃。
func ComputeProbeUpperBound(candidateTotal int, roundPeriod, hardTimeout, startupCooldown time.Duration) time.Duration {
	budgetMinutes := 0
	if candidateTotal > probeBudgetPerMinute {
		budgetMinutes = (candidateTotal + probeBudgetPerMinute - 1) / probeBudgetPerMinute
	}
	return time.Duration(budgetMinutes)*time.Minute + roundPeriod + hardTimeout + startupCooldown
}

const probeBudgetPerMinute = 60

func accountIDKey(accountID int64, modelKey string) string {
	return strconv.FormatInt(accountID, 10) + ":" + modelKey
}

// freezeCandidateBound 在候选首次进入调度时冻结其候选总数（C：冻结，不得因早期候选
// 恢复离开而缩短尾部候选的阈值）。后续进入不再覆盖。
func (p *AccountHealthRecoveryProbeService) freezeCandidateBound(accountID int64, modelKey string, total int) {
	p.frozenMu.Lock()
	defer p.frozenMu.Unlock()
	key := accountIDKey(accountID, modelKey)
	if _, ok := p.frozenBounds[key]; !ok {
		p.frozenBounds[key] = total
	}
}

// frozenCandidateBound 读取冻结值（未冻结返回 0）。
func (p *AccountHealthRecoveryProbeService) frozenCandidateBound(accountID int64, modelKey string) int {
	v, _ := p.frozenCandidateBoundOK(accountID, modelKey)
	return v
}

// frozenCandidateBoundOK 读取冻结值并报告该 key 是否确实冻结过（区别于冻结值 0）。
func (p *AccountHealthRecoveryProbeService) frozenCandidateBoundOK(accountID int64, modelKey string) (int, bool) {
	p.frozenMu.Lock()
	defer p.frozenMu.Unlock()
	v, ok := p.frozenBounds[accountIDKey(accountID, modelKey)]
	return v, ok
}

// clearCandidateBound 清除某 (account, scope) 的冻结值。冻结值生命周期绑定当前异常候选
// 周期：权威恢复写入口（模型级 Success 幂等清除 / 账号级恢复）成功后清除对应 key，下一轮
// 进入调度时按当时候选总数重新冻结，不复用上一轮（可能已过期的）候选总数。modelKey==""
// 为账号级维度。
func (p *AccountHealthRecoveryProbeService) clearCandidateBound(accountID int64, modelKey string) {
	if p == nil {
		return
	}
	p.frozenMu.Lock()
	defer p.frozenMu.Unlock()
	delete(p.frozenBounds, accountIDKey(accountID, modelKey))
}

// AccountFreshnessUpperBound 返回某 (account, modelKey) 的有效上界（D4 三维阈值公式的
// 账号侧输入）：复用冻结候选总数经工作项 1 上界公式导出。modelKey=="" 表示账号级维度。
// 未冻结（key 不存在，该候选尚未进入调度）直接返回 0 —— 调用方以 0 视为「空集合」走
// 基线；只有确实冻结过的候选才计算探测上界，避免未进入调度的 scope 得到与唯一公式
// 不一致的阈值。
func (p *AccountHealthRecoveryProbeService) AccountFreshnessUpperBound(accountID int64, modelKey string) time.Duration {
	if p == nil {
		return 0
	}
	total, frozen := p.frozenCandidateBoundOK(accountID, modelKey)
	if !frozen {
		return 0
	}
	return ComputeProbeUpperBound(total, probeRoundPeriod, probeRequestHardTimeout, probeStartupCooldown)
}

// recordAccountLevelObservation 记录账号级候选的观察/尝试时间（F，两维度共用入口）。
// 成功→observed_at；失败/降级/传输错误→attempted_at；绝不清除任何模型级状态。
// 返回 ApplyModelRateLimitObservation 的提交错误（#2：透传以便调用点按提交失败做失败关闭）；
// nil 守卫分支返回 nil。函数内 freshness 评估错误维持 Warn-only（评估不影响状态迁移），
// 不并入返回错误。
func (p *AccountHealthRecoveryProbeService) recordAccountLevelObservation(ctx context.Context, accountID int64, ok bool, probeErr error) error {
	if p == nil || p.rateLimit == nil {
		return nil
	}
	outcome := ProbeOutcomeUnclassified
	if probeErr == nil && ok {
		outcome = ProbeOutcomeSuccess
	}
	err := p.rateLimit.ApplyModelRateLimitObservation(ctx, accountID, tokenHarborAccountLevelProbeScope, ModelRateLimitObservation{
		EventTime:    time.Now(),
		Outcome:      outcome,
		AccountLevel: true,
	})
	// D4：账号级候选（temp-unschedulable/熔断）在既有恢复探测路径上同样评估陈旧——
	// 同一探测链、同一状态入口，账号级维度不被漏评估。评估失败不影响状态迁移，Warn-only。
	if p.freshnessAlerts != nil {
		if ferr := p.freshnessAlerts.EvaluateAccountLevelFreshness(ctx, accountID); ferr != nil {
			logger.L().Warn("freshness_alert_evaluate_failed",
				zap.Int64("account_id", accountID),
				zap.Bool("account_level", true),
				zap.Error(ferr))
		}
	}
	return err
}

// isAccountLevelStale 报告账号级探测是否已超出阈值未观察到权威响应（F：stale 告警）。
//
// observed_at 的类型判别复用 service 包单一判别口径 observedAtFromEntry（视图/告警同一
// 事实源，E21），解析复用 parseObservedAt（E16）。键不存在 / nil / 空串 / 仅空白 = 无观测，
// 既有「回退 attempted_at 判定」语义保持不变；存在但非法（类型非 string 或格式不可解析）=
// 权威数据损坏 → 返回明确错误令调用方失败关闭（跳过本轮状态变更），绝不静默降级为
// 「有新鲜观测」而漏告警（与 E16/E21 同口径）。
func (p *AccountHealthRecoveryProbeService) isAccountLevelStale(ctx context.Context, accountID int64, threshold time.Duration, now time.Time) (bool, error) {
	if p == nil || p.rateLimit == nil {
		return false, nil
	}
	entry, err := p.rateLimit.GetModelRateLimitObservation(ctx, accountID, tokenHarborAccountLevelProbeScope)
	if err != nil || entry == nil {
		return false, err
	}
	// observed_at：类型判别经 observedAtFromEntry（单一判别口径），解析经 parseObservedAt
	// （单一解析口径）。存在但非法 → 明确错误失败关闭，不再静默忽略后回退 attempted_at。
	rawObserved, oerr := observedAtFromEntry(entry)
	if oerr != nil {
		return false, oerr
	}
	observedAt, hasObserved, perr := parseObservedAt(rawObserved)
	if perr != nil {
		return false, perr
	}
	var last time.Time
	if hasObserved {
		last = observedAt
	}
	// attempted_at：同类类型断言按同口径失败关闭（键不存在 / nil = 无值；存在但类型非
	// string = 损坏 → 明确错误）。attempted_at 仅作回退判定，不解除告警（既有语义不变）。
	if raw, exists := entry[entryAttemptedAtKey]; exists && raw != nil {
		v, ok := raw.(string)
		if !ok {
			return false, fmt.Errorf("invalid attempted_at: wrong type %T (want string), authoritative data corrupted", raw)
		}
		if strings.TrimSpace(v) != "" {
			t, e := time.Parse(time.RFC3339, v)
			if e != nil {
				return false, fmt.Errorf("invalid attempted_at %q: %w", v, e)
			}
			if t.After(last) {
				last = t
			}
		}
	}
	if last.IsZero() {
		return false, nil
	}
	return now.Sub(last) > threshold, nil
}
