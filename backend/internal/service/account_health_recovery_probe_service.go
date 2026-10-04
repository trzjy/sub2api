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

// probeRecoverableMarkers 列出可被探测恢复的停调标记词（TempUnschedState.MatchedKeyword）：
//   - openai_apikey_health_breaker：APIKey 健康熔断 L3 停调（CodeBuddy 影子准入后
//     同样携带该标记，方案 T1）；
//   - pool_mode_401_escalation：池模式 401 窗口升级停调（方案 T5，常量单一定义于
//     pool_401_escalation.go，此处禁止字面量重复）。
var probeRecoverableMarkers = []string{
	openAIAPIKeyHealthBreakerReason,
	PoolMode401EscalationMarker,
}

// accountHealthProbeRepository is the narrow repository surface the recovery probe
// needs. It is intentionally a subset of AccountRepository so the real repository
// satisfies it without forcing every AccountRepository test double to implement the
// probe-specific methods.
type accountHealthProbeRepository interface {
	GetByID(ctx context.Context, id int64) (*Account, error)
	ListTempUnschedulableAccounts(ctx context.Context, now time.Time, limit int) ([]*Account, error)
	SetTempUnschedulableReason(ctx context.Context, id int64, reason string) error
	// ListTokenHarborModelRateLimitedAccounts 是 TokenHarbor 免费档模型级限流候选源
	// （R3）：返回 active 且 extra 含 model_rate_limits 的账号，精确过滤在探测服务内完成。
	ListTokenHarborModelRateLimitedAccounts(ctx context.Context, now time.Time, limit int) ([]*Account, error)
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
	wg      sync.WaitGroup

	// probeOverride, when set, replaces the real upstream probe (used by tests).
	probeOverride func(ctx context.Context, account *Account) (ok bool, err error)

	// ---- D2：TokenHarbor 免费档探测链 ----
	// nowFunc 可注入时钟（确定性测试用），默认 time.Now。
	nowFunc func() time.Time
	// startTime 是进程启动时刻，用于冷启动 60s 保守门禁（E）。
	startTime time.Time
	// budget 进程内 per-account 滚动预算管理器（B）。
	budget *probeBudgetManager
	// frozenBounds 保存每个 (account,scope) 进入调度时冻结的候选总数（C，上限公式）。
	frozenMu     sync.Mutex
	frozenBounds map[string]int

	// obsStatsMu / obsStats 是 R2 触发观测的 per-account 只读计数面（D4 工作项 3）：
	// 探测发送数、预算拒绝数、结果分类计数（成功/免费档429/其他）。仅埋点计数，不改限流语义。
	obsStatsMu sync.Mutex
	obsStats   map[int64]*probeObservationStats
	// probeTokenHarborOverride 替换真实的 TokenHarbor 模型级探测（测试用）。
	probeTokenHarborOverride func(ctx context.Context, account *Account, modelKey string) (ProbeOutcome, error)
	// freshnessAlerts 是 D4 陈旧告警服务（可选注入）。探测链是唯一探测链，账号两维的陈旧
	// 评估随候选派发一起进行——候选集合即「当前异常候选」，健康账号不在其中（R3）。
	freshnessAlerts *FreshnessAlertService
	// channelFreshness 是 E39 渠道维陈旧收敛窄面（可选注入）。账号恢复成功后触发关联
	// 渠道陈旧收敛；未注入时不收敛。
	channelFreshness ChannelFreshnessRefresher
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
	now := time.Now
	return &AccountHealthRecoveryProbeService{
		accountRepo:         accountRepo,
		httpUpstream:        httpUpstream,
		cfg:                 cfg,
		rateLimit:           rateLimit,
		settingService:      settingService,
		tlsFPProfileService: tlsFPProfileService,
		nowFunc:             now,
		startTime:           now(),
		budget:             newProbeBudgetManager(now),
		frozenBounds:       make(map[string]int),
		obsStats:           make(map[int64]*probeObservationStats),
	}
}

// SetProbeClock 注入确定性时钟（测试用），同时更新预算管理器。
func (p *AccountHealthRecoveryProbeService) SetProbeClock(nowFunc func() time.Time) {
	if p == nil || nowFunc == nil {
		return
	}
	p.nowFunc = nowFunc
	if p.budget != nil {
		p.budget.nowFunc = nowFunc
	}
}

// SetProbeStartTime 覆盖进程启动时刻（测试用冷启动门禁）。
func (p *AccountHealthRecoveryProbeService) SetProbeStartTime(t time.Time) {
	if p != nil {
		p.startTime = t
	}
}

// SetTokenHarborProbeOverride 安装 TokenHarbor 模型级探测覆盖（测试用）。
func (p *AccountHealthRecoveryProbeService) SetTokenHarborProbeOverride(fn func(ctx context.Context, account *Account, modelKey string) (ProbeOutcome, error)) {
	if p != nil {
		p.probeTokenHarborOverride = fn
	}
}

// Start launches the probe sweep loop. The loop runs continuously and re-reads
// the probe switch on every tick (see RunOnce's per-class enable checks), so toggling
// the switch in admin settings takes effect within one interval (<= IntervalSeconds,
// default 60s) without a process restart. It is safe to call multiple times (only
// the first effective start spawns a goroutine) and terminates via Stop.
func (p *AccountHealthRecoveryProbeService) Start(ctx context.Context) {
	// settingService 为 nil 时失败关闭安全返回（不启动探测循环），与旧 probeEnabled
	// 的 `p == nil || p.settingService == nil → return` 同款：重构使能门禁时该保护被
	// 移除，若不在此短路，下方解引用 nil 会 panic（第三轮终审 #7 回归）。
	if p == nil || p.settingService == nil {
		return
	}
	p.startMu.Lock()
	defer p.startMu.Unlock()
	if p.stopCh != nil {
		return
	}

	interval := 60 * time.Second
	if settings, err := p.settingService.GetOpenAIAPIKeyHealthBreakerSettings(ctx); err == nil && settings != nil && settings.Probe != nil {
		if settings.Probe.IntervalSeconds >= 30 {
			interval = time.Duration(settings.Probe.IntervalSeconds) * time.Second
		}
	}

	p.stopCh = make(chan struct{})
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		logger.L().Info("openai.apikey_health_probe_started", zap.Duration("interval", interval))
		for {
			select {
			case <-p.stopCh:
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
func (p *AccountHealthRecoveryProbeService) Stop() {
	if p == nil {
		return
	}
	p.startMu.Lock()
	stopCh := p.stopCh
	p.stopCh = nil
	p.startMu.Unlock()
	if stopCh != nil {
		close(stopCh)
		p.wg.Wait()
	}
}

// RunOnce performs a single probe sweep over the currently cooldowned accounts.
//
// 按候选类拆分使能门禁（方案工作项 1）：账号级（temp-unschedulable/熔断）相位维持
// 双开关 ProbeEnabledForCandidate(ProbeCandidateCircuitBreaker)；TokenHarbor 免费档
// 模型级相位只要求 settings.Probe.Enabled（ProbeEnabledForCandidate(
// ProbeCandidateTokenHarborModel)，默认 true，管理端可关）——不再被 settings.Enabled
// 双开关短路，使默认配置下 TokenHarbor 相位仍执行。settings 读取失败维持现状
// err != nil → return。
func (p *AccountHealthRecoveryProbeService) RunOnce(ctx context.Context) {
	// settingService 为 nil 时与 p == nil / accountRepo == nil 同款失败关闭：重构使能门禁时
	// 移除了旧 probeEnabled 对 nil settingService 的保护，此处必须显式短路，否则下一行
	// 解引用 nil 会 panic（第三轮终审 #7 回归）。
	//
	// 其余依赖字段核查：httpUpstream/cfg/rateLimit/tlsFPProfileService/freshnessAlerts/
	// accountRepo 均已在各自解引用点做 nil 保护；nowFunc/budget/frozenBounds 由
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

	now := p.nowFunc()

	// 账号级（temp-unschedulable/熔断）相位：维持双开关门禁。
	var accountLevelAccountIDs map[int64]struct{}
	if ProbeEnabledForCandidate(settings, ProbeCandidateCircuitBreaker) {
		candidates, lerr := p.accountRepo.ListTempUnschedulableAccounts(ctx, now, accountHealthProbeCandidateLimit)
		if lerr != nil {
			logger.L().Warn("openai.apikey_health_probe_list_failed", zap.Error(lerr))
			return
		}
		accountLevelAccountIDs = make(map[int64]struct{}, len(candidates))
		for _, acc := range candidates {
			if !p.isHealthBreakerTrip(acc, now) {
				continue
			}
			accountLevelAccountIDs[acc.ID] = struct{}{}
			p.probeAccount(ctx, acc, maxAttempts)
		}
	}

	// D2 第二相：TokenHarbor 免费档模型级候选，复用同一预算/分发管线（R3）。
	// 仅要求 settings.Probe.Enabled（默认 true），不被 settings.Enabled 短路；
	// 账号级候选集合（若双开关启用）一并传入以复用预算/冻结口径。
	if ProbeEnabledForCandidate(settings, ProbeCandidateTokenHarborModel) {
		p.runTokenHarborProbePhase(ctx, accountLevelAccountIDs)
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

// probeAccount performs one probe attempt for a cooldowned account.
func (p *AccountHealthRecoveryProbeService) probeAccount(ctx context.Context, acc *Account, maxAttempts int) {
	state, ok := parseHealthBreakerState(acc.TempUnschedulableReason)
	if !ok || state == nil {
		return
	}
	attempts := state.ProbeAttempts + 1
	if attempts > maxAttempts {
		// Reached the attempt ceiling: give up probing and let the cooldown expire.
		logger.L().Info("openai.apikey_health_probe_gave_up",
			zap.Int64("account_id", acc.ID),
			zap.Int("max_attempts", maxAttempts),
		)
		return
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
		p.bumpProbeAttempts(ctx, acc.ID, maxAttempts)
		return
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
			return
		}
		if p.rateLimit != nil {
			if clearErr := p.rateLimit.ClearTempUnschedulable(ctx, acc.ID); clearErr != nil {
				logger.L().Warn("openai.apikey_health_probe_clear_failed",
					zap.Int64("account_id", acc.ID),
					zap.Error(clearErr),
				)
				return
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
		return
	}

	// Probe failed: bump the attempt counter; keep the account parked.
	p.bumpProbeAttempts(ctx, acc.ID, attempts)
	logger.L().Info("openai.apikey_health_probe_failed",
		zap.Int64("account_id", acc.ID),
		zap.Int("attempt", attempts),
		zap.Int("max_attempts", maxAttempts),
	)
}

// bumpProbeAttempts rewrites the temp-unschedulable reason to record the new attempt
// count. It is best-effort: a store failure only skips this cycle's bookkeeping.
func (p *AccountHealthRecoveryProbeService) bumpProbeAttempts(ctx context.Context, accountID int64, attempts int) {
	if p.accountRepo == nil {
		return
	}
	acc, err := p.accountRepo.GetByID(ctx, accountID)
	if err != nil || acc == nil {
		return
	}
	state, ok := parseHealthBreakerState(acc.TempUnschedulableReason)
	if !ok || state == nil {
		return
	}
	state.ProbeAttempts = attempts
	updated, mErr := json.Marshal(state)
	if mErr != nil {
		return
	}
	if err := p.accountRepo.SetTempUnschedulableReason(ctx, accountID, string(updated)); err != nil {
		logger.L().Debug("openai.apikey_health_probe_attempt_update_failed",
			zap.Int64("account_id", accountID), zap.Error(err))
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
	// T4：CodeBuddy 影子（oauth 型）没有 /models 面——buildOpenAIAPIKeyModelsRequest
	// 要求 type==apikey，oauth 影子构建失败会降级到期恢复——改派最小流式 chat 探测。
	if isCodeBuddyShadowAccount(acc) {
		return p.probeCodeBuddyShadowUpstream(ctx, acc)
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
// D2：TokenHarbor 免费档探测链（唯一探测链，R4 红线——禁止第二链）
// ===========================================================================

// tokenHarborFreeTierProbeMarker 复用权威 reason 前缀，使模型级候选源与业务写入
// 共享同一前缀（A：「Marker into the authoritative recoverable-marker set」）。
const tokenHarborFreeTierProbeMarker = tokenHarborFreeTierReasonPrefix

// tokenHarborAccountLevelProbeScope 是账号级（无模型键）维度在 model_rate_limits
// 桶中的占位 scope，与模型级 scope 互不掩盖（F：两个维度不得互相刷新）。
const tokenHarborAccountLevelProbeScope = "tokenharbor_account_level_probe"

// 探测链写入条目内的时间字段键（与 ratelimit_service.go 的写入入口共用）。
const (
	entryObservedAtKey  = "observed_at"
	entryAttemptedAtKey = "attempted_at"
)

// D2 预算/上限公式常量（C/E，单一事实源）。
const (
	probeBudgetWindow      = 60 * time.Second
	probeBudgetPerMinute   = 60
	probeStartupCooldown   = 60 * time.Second
	probeRoundPeriod       = 60 * time.Second
	probeRequestHardTimeout = 15 * time.Second
)

// ProbeOutcome 是探测链对单次上游响应的分类（D）。
type ProbeOutcome int

const (
	// ProbeOutcomeSuccess 目标模型最小请求 2xx（含慢 2xx）→ 幂等清除该 scope 限流。
	ProbeOutcomeSuccess ProbeOutcome = iota
	// ProbeOutcomeFreeTier429 识别出免费档额度用光 429 → 确认仍受限，置 observed_at（不清）。
	ProbeOutcomeFreeTier429
	// ProbeOutcomeUnclassified 5xx/超时/传输错误/其他 429/401/403/非免费档 429 → 仅置 attempted_at。
	ProbeOutcomeUnclassified
)

// probeBudgetManager 进程内 per-account 滚动 60s 预算管理器（B）：
//   - 同一账号的所有候选（模型级 + 账号级）共享同一窗口，总出站探测 ≤60/min；
//   - 在途去重：模型级按 (account,model)，账号级按 (account) 各自独立；
//   - 无整轮完成门禁：仍占在途位的候选不阻塞其他候选的后续分派。
type probeBudgetManager struct {
	mu      sync.Mutex
	nowFunc func() time.Time
	window  time.Duration
	perMin  int
	accounts map[int64]*accountProbeBudget
}

type accountProbeBudget struct {
	sends           []time.Time
	inflightModel   map[string]bool
	inflightAccount bool
	cursor          int
}

func newProbeBudgetManager(nowFunc func() time.Time) *probeBudgetManager {
	return &probeBudgetManager{
		nowFunc:  nowFunc,
		window:   probeBudgetWindow,
		perMin:   probeBudgetPerMinute,
		accounts: make(map[int64]*accountProbeBudget),
	}
}

func (m *probeBudgetManager) get(accountID int64) *accountProbeBudget {
	b, ok := m.accounts[accountID]
	if !ok {
		b = &accountProbeBudget{inflightModel: make(map[string]bool)}
		m.accounts[accountID] = b
	}
	return b
}

func (b *accountProbeBudget) prune(now time.Time, window time.Duration) {
	cutoff := now.Add(-window)
	i := 0
	for ; i < len(b.sends); i++ {
		if b.sends[i].After(cutoff) {
			break
		}
	}
	if i > 0 {
		b.sends = b.sends[i:]
	}
}

// TryAcquire 尝试为一次出站探测预留预算。modelKey=="" 表示账号级候选。
// 返回是否获准（在途去重命中或窗口已满则返回 false）。
func (m *probeBudgetManager) TryAcquire(accountID int64, modelKey string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.nowFunc()
	b := m.get(accountID)
	b.prune(now, m.window)
	accountLevel := modelKey == ""
	if accountLevel {
		if b.inflightAccount {
			return false
		}
	} else {
		if b.inflightModel[modelKey] {
			return false
		}
	}
	if len(b.sends) >= m.perMin {
		return false
	}
	b.sends = append(b.sends, now)
	if accountLevel {
		b.inflightAccount = true
	} else {
		b.inflightModel[modelKey] = true
	}
	return true
}

// Release 释放某候选的在途位（无论成功、失败还是超时），使其不阻塞后续分派。
func (m *probeBudgetManager) Release(accountID int64, modelKey string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.get(accountID)
	if modelKey == "" {
		b.inflightAccount = false
	} else {
		delete(b.inflightModel, modelKey)
	}
}

// tokenHarborProbeResult 是单次 TokenHarbor 探测的分类结果。
type tokenHarborProbeResult struct {
	Outcome ProbeOutcome
	ResetAt time.Time
	Reason  string
}

type tokenHarborCandidate struct {
	modelKey    string
	accountLevel bool
	// probedThisScan 标记该账号级候选已在本 RunOnce 第一相被 probeAccount 探测过
	//（#1：避免第二相 dispatchAccountTokenHarbor 重复发上游请求 + 重复写观测）。
	probedThisScan bool
}

// coldStartNotComplete 报告是否仍处于冷启动保守门禁内（E）：重启后前 60s 内
// TokenHarbor 主动探测 failed-closed，不发送任何探测。这是显式可告警状态，非静默分支。
func (p *AccountHealthRecoveryProbeService) coldStartNotComplete() bool {
	if p == nil {
		return false
	}
	return p.nowFunc().Before(p.startTime.Add(probeStartupCooldown))
}

// ComputeProbeUpperBound 是上限公式的单一事实源（C）。两口径统一于此：
//   - 普通场景（候选总数 ≤60 且窗口无先前占用）：无额外预算等待项（0 分钟），仅
//     1×轮次周期 + 请求硬超时 + 启动冷却 60s；
//   - 最坏场景（冻结值 >60）：预算等待项 = ceil(候选总数/60) 分钟（例如 100 → 2 分钟）
//     + 1×轮次周期 + 请求硬超时 + 启动冷却。
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

// probeObservationStats 是 R2 触发观测的 per-account 计数（D4 工作项 3，只读面）。
type probeObservationStats struct {
	Sends        int64
	BudgetRejects int64
	Success      int64
	FreeTier429  int64
	Unclassified int64
}

// ProbeObservationStats 是 GetProbeObservationStats 的对外只读视图。
type ProbeObservationStats struct {
	AccountID    int64
	Sends        int64
	BudgetRejects int64
	Success      int64
	FreeTier429  int64
	Unclassified int64
}

// obsStatsForLocked 取（必要时惰性创建）某账号的 R2 计数条目；调用方须持有 obsStatsMu。
// 惰性建表使以结构体字面量构造的服务（既有测试夹具）也能安全埋点。
func (p *AccountHealthRecoveryProbeService) obsStatsForLocked(accountID int64) *probeObservationStats {
	if p.obsStats == nil {
		p.obsStats = make(map[int64]*probeObservationStats)
	}
	s := p.obsStats[accountID]
	if s == nil {
		s = &probeObservationStats{}
		p.obsStats[accountID] = s
	}
	return s
}

// recordProbeSend 累计某账号一次实际发出的探测（TryAcquire 成功后）。
func (p *AccountHealthRecoveryProbeService) recordProbeSend(accountID int64) {
	if p == nil {
		return
	}
	p.obsStatsMu.Lock()
	defer p.obsStatsMu.Unlock()
	s := p.obsStatsForLocked(accountID)
	s.Sends++
}

// recordProbeBudgetReject 累计某账号一次被预算拒绝的探测派发。
func (p *AccountHealthRecoveryProbeService) recordProbeBudgetReject(accountID int64) {
	if p == nil {
		return
	}
	p.obsStatsMu.Lock()
	defer p.obsStatsMu.Unlock()
	s := p.obsStatsForLocked(accountID)
	s.BudgetRejects++
}

// recordProbeOutcome 累计某账号一次探测的结果分类计数。
func (p *AccountHealthRecoveryProbeService) recordProbeOutcome(accountID int64, outcome ProbeOutcome) {
	if p == nil {
		return
	}
	p.obsStatsMu.Lock()
	defer p.obsStatsMu.Unlock()
	s := p.obsStatsForLocked(accountID)
	switch outcome {
	case ProbeOutcomeSuccess:
		s.Success++
	case ProbeOutcomeFreeTier429:
		s.FreeTier429++
	default:
		s.Unclassified++
	}
}

// GetProbeObservationStats 返回某账号的 R2 触发观测计数（只读查询面，D4 工作项 3）。
// 未观测过的账号返回 (零值, false)。仅埋点计数，不改限流语义。
func (p *AccountHealthRecoveryProbeService) GetProbeObservationStats(accountID int64) (ProbeObservationStats, bool) {
	out := ProbeObservationStats{AccountID: accountID}
	if p == nil {
		return out, false
	}
	p.obsStatsMu.Lock()
	defer p.obsStatsMu.Unlock()
	s, ok := p.obsStats[accountID]
	if !ok {
		return out, false
	}
	out.Sends = s.Sends
	out.BudgetRejects = s.BudgetRejects
	out.Success = s.Success
	out.FreeTier429 = s.FreeTier429
	out.Unclassified = s.Unclassified
	return out, true
}

// runTokenHarborProbePhase 第二相：列出 TokenHarbor 免费档模型级候选，按账号分组，
// 与账号级候选共享预算并冻结上限后公平旋转并发分派（A/C）。冷启动门禁内直接返回（E）。
func (p *AccountHealthRecoveryProbeService) runTokenHarborProbePhase(ctx context.Context, accountLevelAccountIDs map[int64]struct{}) {
	if p == nil || p.accountRepo == nil {
		return
	}
	if p.coldStartNotComplete() {
		return
	}
	now := p.nowFunc()
	accounts, err := p.accountRepo.ListTokenHarborModelRateLimitedAccounts(ctx, now, accountHealthProbeCandidateLimit)
	if err != nil {
		logger.L().Warn("openai.tokenharbor_probe_list_failed", zap.Error(err))
		return
	}
	type grouped struct {
		account *Account
		scopes  []string
	}
	byAccount := make(map[int64]*grouped)
	order := make([]int64, 0, len(accounts))
	for _, acc := range accounts {
		scopes := p.activeTokenHarborFreeTierScopes(acc, now)
		if len(scopes) == 0 {
			continue
		}
		if _, ok := byAccount[acc.ID]; !ok {
			byAccount[acc.ID] = &grouped{account: acc}
			order = append(order, acc.ID)
		}
		byAccount[acc.ID].scopes = append(byAccount[acc.ID].scopes, scopes...)
	}
	for _, accountID := range order {
		g := byAccount[accountID]
		_, hasAccountLevel := accountLevelAccountIDs[accountID]
		p.dispatchAccountTokenHarbor(ctx, g.account, g.scopes, hasAccountLevel)
	}
}

// dispatchAccountTokenHarbor 对单个账号的模型级 + 账号级候选做公平旋转分派：
// 候选总数 = 模型级 scope 数 + (账号级 ? 1 : 0)，冻结每个候选的阈值；
// 本回合按游标顺序获取预算，命中的候选并发分派（并发数=本回合获预算数），
// 占用在途位的候选超时/完成即释放，不阻塞其他候选。
func (p *AccountHealthRecoveryProbeService) dispatchAccountTokenHarbor(ctx context.Context, acc *Account, scopes []string, hasAccountLevel bool) {
	candidateTotal := len(scopes)
	if hasAccountLevel {
		candidateTotal++
	}
	if candidateTotal <= 0 {
		return
	}
	b := p.budget.get(acc.ID)
	start := b.cursor % candidateTotal

	cands := make([]tokenHarborCandidate, 0, candidateTotal)
	for _, s := range scopes {
		cands = append(cands, tokenHarborCandidate{modelKey: s})
	}
	if hasAccountLevel {
		// #1：该账号级候选已在本 RunOnce 第一相被 probeAccount 探测过（runProbe +
		// recordAccountLevelObservation 各一次），此处仅占位复用预算/冻结口径，不再
		// 发上游请求、不再写观测、不消耗预算。
		cands = append(cands, tokenHarborCandidate{modelKey: "", accountLevel: true, probedThisScan: true})
	}

	var wg sync.WaitGroup
	// E7 #5：候选进入本调度快照即冻结候选总数（TryAcquire 判定之前），预算拒绝/在途
	// 去重不影响冻结值——否则被拒绝的候选下轮首获准时按更小总数冻结，阈值被错误缩短。
	// 冻结会随预算滑动窗滑出后对后一轮重新进入的候选按当时候选总数再冻结（不做覆盖）。
	for i := 0; i < len(cands); i++ {
		c := cands[(start+i)%len(cands)]
		p.freezeCandidateBound(acc.ID, c.modelKey, candidateTotal)
		// #1：本相已探测过的账号级候选跳过预算获取与上游分派（不消耗预算、不再发请求、
		// 不再写观测），但仍留在 cands 中供下方 evaluateAccountFreshness 对其评估陈旧
		//（幂等，保持 D4「本轮派发落地后再评估」语义）。冻结上界仍按含账号级的候选总数
		// 冻结（阈值公平性不变）。
		if c.probedThisScan {
			continue
		}
		if !p.budget.TryAcquire(acc.ID, c.modelKey) {
			p.recordProbeBudgetReject(acc.ID) // R2：预算拒绝计数
			continue
		}
		b.cursor = (b.cursor + 1) % (candidateTotal + 1)
		wg.Add(1)
		go func(c tokenHarborCandidate) {
			defer wg.Done()
			defer p.budget.Release(acc.ID, c.modelKey)
			p.probeOneTokenHarbor(ctx, acc, c)
		}(c)
	}
	wg.Wait()

	// D4：本轮派发（含写入口状态迁移）全部落地后再评估陈旧——避免「先建告警、同轮恢复又关」
	// 的抖动。候选集合即当前异常候选；观测已刷新的候选不陈旧，未获权威观测的候选按阈值告警。
	p.evaluateAccountFreshness(ctx, acc.ID, cands)
}

// evaluateAccountFreshness 对本账号的当前异常候选逐个评估陈旧状态（账号+模型 / 账号级两维）。
// 只做评估与告警同步，不影响预算与派发语义；错误只记日志。
func (p *AccountHealthRecoveryProbeService) evaluateAccountFreshness(ctx context.Context, accountID int64, cands []tokenHarborCandidate) {
	if p == nil || p.freshnessAlerts == nil {
		return
	}
	for _, c := range cands {
		var err error
		if c.accountLevel {
			err = p.freshnessAlerts.EvaluateAccountLevelFreshness(ctx, accountID)
		} else {
			err = p.freshnessAlerts.EvaluateAccountModelFreshness(ctx, accountID, c.modelKey)
		}
		if err != nil {
			logger.L().Warn("freshness_alert_evaluate_failed",
				zap.Int64("account_id", accountID),
				zap.String("model", c.modelKey),
				zap.Bool("account_level", c.accountLevel),
				zap.Error(err))
		}
	}
}

// probeOneTokenHarbor 执行单个候选的探测并将分类结果写入统一状态入口。
func (p *AccountHealthRecoveryProbeService) probeOneTokenHarbor(ctx context.Context, acc *Account, c tokenHarborCandidate) {
	res, probeErr := p.runTokenHarborProbe(ctx, acc, c.modelKey)
	// E7 #1：探测错误不得默认成功。runTokenHarborProbe 返回错误时其零值结果
	// （ProbeOutcomeSuccess=iota=0）会被误判成功，触发 clearCandidateBound 与写入口
	// 清除真实限流——一切非明确成功（含错误路径）统一回退 ProbeOutcomeUnclassified
	// （仅置 attempted_at），失败关闭。
	if probeErr != nil {
		logger.L().Warn("openai.tokenharbor_probe_degraded",
			zap.Int64("account_id", acc.ID),
			zap.String("model", c.modelKey),
			zap.Error(probeErr),
		)
		res = tokenHarborProbeResult{Outcome: ProbeOutcomeUnclassified}
	}
	p.recordProbeSend(acc.ID) // R2：实际发出计数
	// E19：先写权威观测（唯一状态入口），提交成功（返回 nil）才清除冻结上界。提交失败
	// 沿既有探测错误路径处理（E7 同口径）：按失败尝试计（不计成功）、不清状态与冻结上界
	// ——否则限流条目仍受限而冻结上界被错误清除（阈值被错误缩短）。
	if p.rateLimit != nil {
		scope := c.modelKey
		if c.accountLevel {
			scope = tokenHarborAccountLevelProbeScope
		}
		if err := p.rateLimit.ApplyModelRateLimitObservation(ctx, acc.ID, scope, ModelRateLimitObservation{
			EventTime:    p.nowFunc(),
			Outcome:      res.Outcome,
			ResetAt:      res.ResetAt,
			Reason:       res.Reason,
			AccountLevel: c.accountLevel,
		}); err != nil {
			logger.L().Warn("openai.tokenharbor_probe_commit_failed",
				zap.Int64("account_id", acc.ID),
				zap.String("model", c.modelKey),
				zap.Error(err))
			res.Outcome = ProbeOutcomeUnclassified
		}
	}
	p.recordProbeOutcome(acc.ID, res.Outcome) // R2：结果分类计数（提交失败按失败尝试计）
	// 恢复成功：清除该候选的冻结上界，使冻结值生命周期绑定当前异常候选周期——下一次
	// 进入调度时按当时候选总数重新冻结，不复用上一轮（可能已过期）的候选总数。
	// E7 #1：只有确认 2xx（ProbeOutcomeSuccess）且提交成功才允许清状态与冻结上界。
	if res.Outcome == ProbeOutcomeSuccess {
		p.clearCandidateBound(acc.ID, c.modelKey)
	}
}

// runTokenHarborProbe 派发单次探测：优先用注入覆盖（测试），否则走真实上游分类。
func (p *AccountHealthRecoveryProbeService) runTokenHarborProbe(ctx context.Context, acc *Account, modelKey string) (tokenHarborProbeResult, error) {
	if p.probeTokenHarborOverride != nil {
		outcome, err := p.probeTokenHarborOverride(ctx, acc, modelKey)
		return tokenHarborProbeResult{Outcome: outcome}, err
	}
	if modelKey == "" {
		// 账号级：用既有 /models 等价探测判断账号健康。
		ok, err := p.probeUpstream(ctx, acc)
		if err != nil {
			return tokenHarborProbeResult{Outcome: ProbeOutcomeUnclassified}, err
		}
		if ok {
			return tokenHarborProbeResult{Outcome: ProbeOutcomeSuccess}, nil
		}
		return tokenHarborProbeResult{Outcome: ProbeOutcomeUnclassified}, nil
	}
	return p.probeTokenHarborModelUpstream(ctx, acc, modelKey)
}

// probeTokenHarborModelUpstream 向目标模型发最小 chat 完成请求并分类响应。
// 传输错误/5xx/超时/非免费档 429/401/403 → Unclassified（仅 attempted_at）；
// 2xx → Success（幂等清除）；免费档 429 → FreeTier429（确认仍受限）。
func (p *AccountHealthRecoveryProbeService) probeTokenHarborModelUpstream(ctx context.Context, acc *Account, modelKey string) (tokenHarborProbeResult, error) {
	if p.httpUpstream == nil || p.cfg == nil {
		return tokenHarborProbeResult{}, errors.New("probe transport not configured")
	}
	apiKey := strings.TrimSpace(acc.GetOpenAIProtocolAPIKey())
	if apiKey == "" {
		return tokenHarborProbeResult{}, errors.New("no api key for model probe")
	}
	baseURL := acc.GetOpenAIFormatBaseURL()
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://api.openai.com"
	}
	normalized, err := cnValidateProbeURL(p.cfg, baseURL)
	if err != nil {
		return tokenHarborProbeResult{}, fmt.Errorf("validate base url: %w", err)
	}
	body, err := json.Marshal(map[string]any{
		"model":    modelKey,
		"messages": []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
		"stream":  false,
	})
	if err != nil {
		return tokenHarborProbeResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(normalized, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return tokenHarborProbeResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	acc.ApplyHeaderOverrides(req.Header)

	proxyURL := ""
	if acc.ProxyID != nil && acc.Proxy != nil {
		proxyURL = acc.Proxy.URL()
	}
	var tlsProfile *tlsfingerprint.Profile
	if p.tlsFPProfileService != nil {
		tlsProfile = p.tlsFPProfileService.ResolveTLSProfile(acc)
	}
	callCtx, cancel := context.WithTimeout(ctx, probeRequestHardTimeout)
	defer cancel()
	resp, err := p.httpUpstream.DoWithTLS(req.WithContext(callCtx), proxyURL, acc.ID, acc.Concurrency, tlsProfile)
	if err != nil {
		// 传输错误：unclassified（保守保留限流）。
		return tokenHarborProbeResult{Outcome: ProbeOutcomeUnclassified}, nil
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return tokenHarborProbeResult{Outcome: ProbeOutcomeSuccess}, nil
	}
	if resp.StatusCode == http.StatusTooManyRequests && isTokenHarborFreeTierExhausted(raw) {
		return tokenHarborProbeResult{Outcome: ProbeOutcomeFreeTier429}, nil
	}
	return tokenHarborProbeResult{Outcome: ProbeOutcomeUnclassified}, nil
}

// activeTokenHarborFreeTierScopes 扫描账号 extra 返回当前 active 的免费档模型级限流
// scope 列表（reason 前缀 tokenharbor_free_tier_exhausted）。
//
// 薄委托 ActiveTokenHarborFreeTierScopes（E12 边界收敛）：判定语义全部收敛到
// model_rate_limit.go 的权威导出入口，本方法保留调用点签名，语义零变化。等价性由
// TestE12ActiveTokenHarborFreeTierScopes_MatchesProbePredicate 锁定防回归。
//
// E7 #6 到期语义分叉（precise_reset 标记为唯一判别依据）见委托实现注释，不再在
// 本文件复制/维护第二份正文。
func (p *AccountHealthRecoveryProbeService) activeTokenHarborFreeTierScopes(acc *Account, now time.Time) []string {
	if acc == nil {
		return nil
	}
	return ActiveTokenHarborFreeTierScopes(acc.Extra, now)
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
		EventTime:    p.nowFunc(),
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
