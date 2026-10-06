//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Stubs / helpers
// ---------------------------------------------------------------------------

// codeBuddy429AccountRepoStub 记录 429 冷却链可能写入的账号级/模型级字段。
type codeBuddy429AccountRepoStub struct {
	AccountRepository
	setRateLimitedCalls int
	lastRateLimitedAt   time.Time
	setModelCalls       int
	lastModel           string
	lastModelUntil      time.Time
	setTempCalls        int
	updateExtraCalls    int
}

func (r *codeBuddy429AccountRepoStub) SetRateLimited(_ context.Context, _ int64, resetAt time.Time) error {
	r.setRateLimitedCalls++
	r.lastRateLimitedAt = resetAt
	return nil
}

func (r *codeBuddy429AccountRepoStub) SetModelRateLimit(_ context.Context, _ int64, model string, resetAt time.Time, _ ...string) error {
	r.setModelCalls++
	r.lastModel = model
	r.lastModelUntil = resetAt
	return nil
}

func (r *codeBuddy429AccountRepoStub) SetTempUnschedulable(_ context.Context, _ int64, _ time.Time, _ string) error {
	r.setTempCalls++
	return nil
}

func (r *codeBuddy429AccountRepoStub) UpdateExtra(_ context.Context, _ int64, _ map[string]any) error {
	r.updateExtraCalls++
	return nil
}

// newCodeBuddy429Gateway 构造带 429 兜底冷却配置的网关服务（镜像 rate_limit_429_cooldown_test.go）。
func newCodeBuddy429Gateway(repo *codeBuddy429AccountRepoStub, cooldownEnabled bool, cooldownSeconds int) *OpenAIGatewayService {
	settingRepo := newMockSettingRepo()
	data, _ := json.Marshal(RateLimit429CooldownSettings{Enabled: cooldownEnabled, CooldownSeconds: cooldownSeconds})
	settingRepo.data[SettingKeyRateLimit429CooldownSettings] = string(data)
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc.SetSettingService(NewSettingService(settingRepo, &config.Config{}))
	return &OpenAIGatewayService{rateLimitService: svc}
}

func codeBuddy429ShadowAccount(id int64) *Account {
	parentID := int64(129)
	return &Account{
		ID:              id,
		Platform:        PlatformDeepseek,
		Type:            AccountTypeOAuth,
		ParentAccountID: &parentID,
		QuotaDimension:  QuotaDimensionCodeBuddy,
	}
}

func codeBuddy429TestContext() *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	return c
}

func codeBuddy429OpsEvents(c *gin.Context) []*OpsUpstreamErrorEvent {
	v, ok := c.Get(OpsUpstreamErrorsKey)
	if !ok {
		return nil
	}
	events, _ := v.([]*OpsUpstreamErrorEvent)
	return events
}

func codeBuddy429UpstreamResponse(status int) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}}
}

// ---------------------------------------------------------------------------
// T3：codeBuddyFailoverSignal
// ---------------------------------------------------------------------------

// 6004 主路径：信号带 Scope=Request + Reason，两个重试标志清零；恰好一条 ops failover 事件。
func TestCodeBuddyFailoverSignal_ModelLimit429(t *testing.T) {
	s := &OpenAIGatewayService{}
	c := codeBuddy429TestContext()
	account := codeBuddy429ShadowAccount(130)
	body := []byte(`{"code":6004,"msg":"模型请求过于频繁，将在 2026-09-30 12:00:00 重置"}`)
	resp := codeBuddy429UpstreamResponse(http.StatusTooManyRequests)

	sig := s.codeBuddyFailoverSignal(c, account, ClassifyCodeBuddyError(resp.StatusCode, body), resp, body, "rate limited")
	require.NotNil(t, sig)
	require.Equal(t, http.StatusTooManyRequests, sig.StatusCode)
	require.Equal(t, GatewayFailureScopeRequest, sig.Scope)
	require.Equal(t, codeBuddyModelLimitFailoverReason, sig.Reason)
	require.False(t, sig.RetryableOnSameAccount)
	require.False(t, sig.RequestScopedTransient)
	require.True(t, sig.ShouldRetryNextAccount(), "handler failover loop must switch accounts")
	require.False(t, sig.IsCredentialFailure())

	// Scope 消费方审查钉住（方案 §3 执行者义务）：
	// ① 熔断分类器：Scope=Request 被排除，不计入熔断窗口（与空 Scope 唯一差异点）。
	_, _, eligible := classifyOpenAIAPIKeyHealthFailure(sig)
	require.False(t, eligible, "model-level limit must not feed the health breaker")
	// ② 调度失败上报：非凭证失败与空 Scope 基线行为一致（均上报）。
	require.True(t, sig.ShouldReportAccountScheduleFailure())

	events := codeBuddy429OpsEvents(c)
	require.Len(t, events, 1)
	require.Equal(t, "failover", events[0].Kind)
	require.Equal(t, account.ID, events[0].AccountID)
	require.Equal(t, account.Platform, events[0].Platform)
	require.Equal(t, http.StatusTooManyRequests, events[0].UpstreamStatusCode)
}

// ⑧ 6004 + overloaded 文案：构造器 requestScopedCapacity 自动置位两个重试标志，
// 信号必须在构造后显式清零——handler 不做同账号重试，直接换号（R5-F2）。
func TestCodeBuddyFailoverSignal_ModelLimitOverloadedFlagsCleared(t *testing.T) {
	s := &OpenAIGatewayService{}
	c := codeBuddy429TestContext()
	account := codeBuddy429ShadowAccount(131)
	body := []byte(`{"code":6004,"msg":"server is overloaded, 模型请求过于频繁，将在 2026-09-30 12:00:00 重置"}`)
	resp := codeBuddy429UpstreamResponse(http.StatusTooManyRequests)

	// 前置：构造器对含 overloaded 文案的响应确实自动置位（证明断言有意义）。
	raw := newOpenAIUpstreamFailoverError(resp.StatusCode, resp.Header, body, "server is overloaded", false)
	require.True(t, raw.RetryableOnSameAccount)
	require.True(t, raw.RequestScopedTransient)

	sig := s.codeBuddyFailoverSignal(c, account, ClassifyCodeBuddyError(resp.StatusCode, body), resp, body, "server is overloaded")
	require.NotNil(t, sig)
	require.False(t, sig.RetryableOnSameAccount, "must not degrade model-level limit to same-account retry")
	require.False(t, sig.RequestScopedTransient)
	require.False(t, sig.IsOpenAICapacityShed())
}

// 账号级软限流（裸 429）：空 Scope、可被熔断计数（T1 放行的意义）、恰好一条 ops 事件。
func TestCodeBuddyFailoverSignal_AccountSoftLimit429(t *testing.T) {
	s := &OpenAIGatewayService{}
	c := codeBuddy429TestContext()
	account := codeBuddy429ShadowAccount(132)
	body := []byte(`{"code":-1,"msg":"requests too frequent"}`)
	resp := codeBuddy429UpstreamResponse(http.StatusTooManyRequests)

	kind := ClassifyCodeBuddyError(resp.StatusCode, body)
	require.Equal(t, CodeBuddyErrKindAccountSoftLimit, kind)

	sig := s.codeBuddyFailoverSignal(c, account, kind, resp, body, "too frequent")
	require.NotNil(t, sig)
	require.Empty(t, sig.Scope, "account soft limit keeps empty scope: counted by breaker")
	require.False(t, sig.RetryableOnSameAccount)
	require.False(t, sig.RequestScopedTransient)
	require.False(t, sig.IsCredentialFailure())
	_, _, eligible := classifyOpenAIAPIKeyHealthFailure(sig)
	require.True(t, eligible, "account-level 429 must feed the health breaker (T1)")
	require.Len(t, codeBuddy429OpsEvents(c), 1)
}

// ⑥ R3-F1 门禁：非 429 状态（含「5xx+限流文案」被分类器归为 AccountSoftLimit）信号必须
// 返回 nil 且无 ops 事件；/responses 回归——nil 回退后既有通用 helper 的 failover 保留。
func TestCodeBuddyFailoverSignal_Non429ReturnsNilAndFallbackKeepsGenericHelper(t *testing.T) {
	s := &OpenAIGatewayService{}
	c := codeBuddy429TestContext()
	account := codeBuddy429ShadowAccount(133)
	body := []byte(`{"code":-1,"msg":"rate limit exceeded"}`)

	kind := ClassifyCodeBuddyError(http.StatusInternalServerError, body)
	require.Equal(t, CodeBuddyErrKindAccountSoftLimit, kind, "classifier row 4 does classify 5xx+limit text")

	resp := codeBuddy429UpstreamResponse(http.StatusInternalServerError)
	sig := s.codeBuddyFailoverSignal(c, account, kind, resp, body, "rate limit exceeded")
	require.Nil(t, sig, "429 status gate must reject non-429 (R3-F1)")
	require.Empty(t, codeBuddy429OpsEvents(c), "no ops event when signal misses")

	// /responses nil 回退：既有通用 helper 仍产生 failover（R4-F2 回归，不得因新分流丢失）。
	helperErr := s.failoverOpenAIUpstreamHTTPError(context.Background(), c, account, resp, body, "rate limit exceeded", "deepseek-chat")
	require.NotNil(t, helperErr)
	require.True(t, helperErr.ShouldRetryNextAccount())
}

// ③ 终态类（余额/会话/审核/请求体/上游故障/未分类）一律不产生 failover 信号。
func TestCodeBuddyFailoverSignal_TerminalKindsReturnNil(t *testing.T) {
	s := &OpenAIGatewayService{}
	account := codeBuddy429ShadowAccount(134)
	cases := []struct {
		kind   CodeBuddyErrKind
		status int
	}{
		{CodeBuddyErrKindBalanceExhausted, http.StatusPaymentRequired},
		{CodeBuddyErrKindSessionDead, http.StatusOK},
		{CodeBuddyErrKindContentAudit, http.StatusBadRequest},
		{CodeBuddyErrKindRequestBody, http.StatusBadRequest},
		{CodeBuddyErrKindUpstreamFault, http.StatusServiceUnavailable},
		{CodeBuddyErrKindNone, http.StatusTooManyRequests},
	}
	for _, tc := range cases {
		c := codeBuddy429TestContext()
		resp := codeBuddy429UpstreamResponse(tc.status)
		sig := s.codeBuddyFailoverSignal(c, account, tc.kind, resp, []byte(`{}`), "msg")
		require.Nil(t, sig, "kind %s must not produce a failover signal", tc.kind)
		require.Empty(t, codeBuddy429OpsEvents(c), "no ops event for kind %s", tc.kind)
	}
}

// ---------------------------------------------------------------------------
// T3 R6-F1：ModelLimit 副作用分支
// ---------------------------------------------------------------------------

// ① reset 可解析、模型非空主路径：仅模型级冷却，账号级字段零写入（冷却配置开启也不例外）。
func TestApplyCodeBuddySideEffects_ModelLimitParsableWritesOnlyModelRateLimit(t *testing.T) {
	repo := &codeBuddy429AccountRepoStub{}
	gw := newCodeBuddy429Gateway(repo, true, 60)
	account := codeBuddy429ShadowAccount(135)
	body := []byte(`{"code":6004,"msg":"模型请求过于频繁，将在 2026-09-30 12:00:00 重置"}`)
	until, ok := parseCodeBuddyResetTime(body)
	require.True(t, ok)

	gw.applyCodeBuddyErrorSideEffects(context.Background(), account, codeBuddy429UpstreamResponse(http.StatusTooManyRequests), body, "deepseek-chat", CodeBuddyErrKindModelLimit, "limited")

	require.Equal(t, 1, repo.setModelCalls)
	require.Equal(t, "deepseek-chat", repo.lastModel)
	require.True(t, repo.lastModelUntil.Equal(until), "parsed reset time must be used as-is")
	require.Zero(t, repo.setRateLimitedCalls, "account-level rate_limited_at must stay empty (R4-F1)")
	require.Zero(t, repo.setTempCalls, "account-level temp_unschedulable must stay empty (R4-F1)")
}

// ⑩ 6004 + 模型非空 + reset 不可解析：以 ~60s 兜底窗口走模型级冷却，零账号级字段写入、
// 不经 handle429（R6-F1）。
func TestApplyCodeBuddySideEffects_ModelLimitUnparsableUsesFallbackCooldown(t *testing.T) {
	repo := &codeBuddy429AccountRepoStub{}
	gw := newCodeBuddy429Gateway(repo, true, 60)
	account := codeBuddy429ShadowAccount(136)
	body := []byte(`{"code":6004,"msg":"模型请求过于频繁"}`)

	before := time.Now()
	gw.applyCodeBuddyErrorSideEffects(context.Background(), account, codeBuddy429UpstreamResponse(http.StatusTooManyRequests), body, "deepseek-chat", CodeBuddyErrKindModelLimit, "limited")

	require.Equal(t, 1, repo.setModelCalls, "model-level cooldown with fallback window")
	require.Equal(t, "deepseek-chat", repo.lastModel)
	require.False(t, repo.lastModelUntil.Before(before.Add(codeBuddyModelLimitFallbackCooldown-5*time.Second)))
	require.False(t, repo.lastModelUntil.After(before.Add(codeBuddyModelLimitFallbackCooldown+5*time.Second)))
	require.Zero(t, repo.setRateLimitedCalls, "must not degrade to account-level handle429")
	require.Zero(t, repo.setTempCalls)
}

// ⑦ 6004 缺模型信息：退化 handle429 的既有已接受语义维持不变（R4-F1 钉住现状）。
func TestApplyCodeBuddySideEffects_ModelLimitNoModelDegradesToAccount429(t *testing.T) {
	repo := &codeBuddy429AccountRepoStub{}
	gw := newCodeBuddy429Gateway(repo, true, 12)
	account := codeBuddy429ShadowAccount(137)
	body := []byte(`{"code":6004,"msg":"模型请求过于频繁"}`)

	before := time.Now()
	gw.applyCodeBuddyErrorSideEffects(context.Background(), account, codeBuddy429UpstreamResponse(http.StatusTooManyRequests), body, "", CodeBuddyErrKindModelLimit, "limited")

	require.Zero(t, repo.setModelCalls)
	require.Equal(t, 1, repo.setRateLimitedCalls, "existing degeneration to handle429 is pinned")
	require.False(t, repo.lastRateLimitedAt.Before(before.Add(12*time.Second-2*time.Second)))
	require.False(t, repo.lastRateLimitedAt.After(before.Add(12*time.Second+2*time.Second)))
}

// ---------------------------------------------------------------------------
// T2：handle429 影子早退收窄
// ---------------------------------------------------------------------------

// CodeBuddy 影子 429 不再零冷却：冷却开启时写入 rate_limited_at（受 cooldown_seconds 配置控制）。
func TestHandle429_CodeBuddyShadowFallsIntoFallbackCooldown(t *testing.T) {
	repo := &codeBuddy429AccountRepoStub{}
	gw := newCodeBuddy429Gateway(repo, true, 60)
	account := codeBuddy429ShadowAccount(130)

	before := time.Now()
	gw.rateLimitService.handle429(context.Background(), account, http.Header{}, []byte(`{"error":{"message":"slow down"}}`))

	require.Equal(t, 1, repo.setRateLimitedCalls, "codebuddy shadow 429 must no longer be a zero-write early exit")
	require.False(t, repo.lastRateLimitedAt.Before(before.Add(60*time.Second-2*time.Second)))
	require.False(t, repo.lastRateLimitedAt.After(before.Add(60*time.Second+2*time.Second)))
}

// 冷却配置关闭时无冷却写入（受生产配置控制）。
func TestHandle429_CodeBuddyShadowFallbackDisabledSkipsMark(t *testing.T) {
	repo := &codeBuddy429AccountRepoStub{}
	gw := newCodeBuddy429Gateway(repo, false, 60)
	account := codeBuddy429ShadowAccount(138)

	gw.rateLimitService.handle429(context.Background(), account, http.Header{}, []byte(`{"error":{"message":"slow down"}}`))

	require.Zero(t, repo.setRateLimitedCalls)
}

// Spark 影子早退保留：冷却开启也零写入（T2 收窄后行为不变，含不写 OpenAI 平台快照）。
func TestHandle429_SparkShadowEarlyExitRetained(t *testing.T) {
	repo := &codeBuddy429AccountRepoStub{}
	gw := newCodeBuddy429Gateway(repo, true, 60)
	parentID := int64(900)
	spark := &Account{
		ID:              901,
		Platform:        PlatformOpenAI,
		Type:            AccountTypeOAuth,
		ParentAccountID: &parentID,
		QuotaDimension:  QuotaDimensionSpark,
	}

	gw.rateLimitService.handle429(context.Background(), spark, http.Header{}, []byte(`{"error":{"message":"slow down"}}`))

	require.Zero(t, repo.setRateLimitedCalls, "spark shadow early exit must be retained")
	require.Zero(t, repo.updateExtraCalls, "no codex snapshot for spark shadow")
}
