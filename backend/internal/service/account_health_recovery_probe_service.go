package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	}
}

// probeEnabled reports whether the recovery probe is enabled in current settings.
func (p *AccountHealthRecoveryProbeService) probeEnabled(ctx context.Context) bool {
	if p == nil || p.settingService == nil {
		return false
	}
	settings, err := p.settingService.GetOpenAIAPIKeyHealthBreakerSettings(ctx)
	if err != nil || settings == nil || settings.Probe == nil {
		return false
	}
	return settings.Enabled && settings.Probe.Enabled
}

// Start launches the probe sweep loop. The loop runs continuously and re-reads
// the probe switch on every tick (see RunOnce's probeEnabled check), so toggling
// the switch in admin settings takes effect within one interval (<= IntervalSeconds,
// default 60s) without a process restart. It is safe to call multiple times (only
// the first effective start spawns a goroutine) and terminates via Stop.
func (p *AccountHealthRecoveryProbeService) Start(ctx context.Context) {
	if p == nil {
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
func (p *AccountHealthRecoveryProbeService) RunOnce(ctx context.Context) {
	if p == nil || p.accountRepo == nil {
		return
	}
	if !p.probeEnabled(ctx) {
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
	candidates, err := p.accountRepo.ListTempUnschedulableAccounts(ctx, now, accountHealthProbeCandidateLimit)
	if err != nil {
		logger.L().Warn("openai.apikey_health_probe_list_failed", zap.Error(err))
		return
	}
	for _, acc := range candidates {
		if !p.isHealthBreakerTrip(acc, now) {
			continue
		}
		p.probeAccount(ctx, acc, maxAttempts)
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
	if err != nil {
		// Degraded platform (no safe/cheap endpoint): stop probing, fall back to expiry.
		logger.L().Warn("openai.apikey_health_probe_degraded",
			zap.Int64("account_id", acc.ID),
			zap.Error(err),
		)
		p.bumpProbeAttempts(ctx, acc.ID, maxAttempts)
		return
	}

	if ok {
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
		}
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
