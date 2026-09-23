package service

// CARD-C 探测侧接线回归测试（方案 T4/T5）：
//   - T4：CodeBuddy 影子停调改派最小流式 chat 探测；httptest 上游校验请求形状
//     （stream:true、max_tokens=1、system-first、extra.shadow_model、母账号凭证、
//     chat completions 端点而非 billing meter）；SSE 正常终止 → 探测成功并清除停调；
//     SSE 错误帧 / 截断流（无 [DONE]）→ 探测失败、停调保留；非 CodeBuddy 账号探测
//     路径不变（仍走 /models）。
//   - T5：pool_mode_401_escalation marker 的停调纳入探测恢复；恢复出口调用
//     resetPool401State（同 epoch 再次 401 能重新跨越边沿）；健康熔断 marker 的
//     恢复不触碰池 401 窗口。
//
// marker 一律引用既有常量（openAIAPIKeyHealthBreakerReason / PoolMode401Escalation-
// Marker），禁止字面量重复定义（R1-F4）。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// codeBuddyProbeRequestRecord 记录探测上游收到的一次请求（形状断言用）。
type codeBuddyProbeRequestRecord struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// codeBuddyProbeUpstreamStub 是真实 HTTP 往返的探测上游桩：httptest server 承载
// SSE 字节，DoWithTLS 把探测请求按原样（方法/头/body）转发到该 server——请求形状
// 校验与响应处理都走真网络栈，而非内存回放。
type codeBuddyProbeUpstreamStub struct {
	serverURL string
	client    *http.Client

	status  int
	sseBody string

	mu      sync.Mutex
	records []codeBuddyProbeRequestRecord
}

func newCodeBuddyProbeUpstreamStub(t *testing.T, status int, sseBody string) *codeBuddyProbeUpstreamStub {
	t.Helper()
	stub := &codeBuddyProbeUpstreamStub{status: status, sseBody: sseBody}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		stub.mu.Lock()
		stub.records = append(stub.records, codeBuddyProbeRequestRecord{
			Method: r.Method,
			Path:   r.URL.Path,
			Header: r.Header.Clone(),
			Body:   body,
		})
		stub.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, stub.sseBody)
	}))
	t.Cleanup(server.Close)
	stub.serverURL = strings.TrimPrefix(server.URL, "http://")
	stub.client = server.Client()
	return stub
}

func (s *codeBuddyProbeUpstreamStub) Do(req *http.Request, proxyURL string, accountID int64, concurrency int) (*http.Response, error) {
	return s.DoWithTLS(req, proxyURL, accountID, concurrency, nil)
}

func (s *codeBuddyProbeUpstreamStub) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.URL.Scheme = "http"
	out.URL.Host = s.serverURL
	out.RequestURI = ""
	out.Close = true
	return s.client.Do(out)
}

func (s *codeBuddyProbeUpstreamStub) lastRecord(t *testing.T) codeBuddyProbeRequestRecord {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	require.NotEmpty(t, s.records, "探测必须向上游发起请求")
	return s.records[len(s.records)-1]
}

// codeBuddyProbeShadowAccount 构造被健康熔断停调的 CodeBuddy 影子账号
// （platform=分组平台 deepseek、oauth 型、quota_dimension=codebuddy，
// extra.shadow_model 为转发链权威模型），与生产影子账号 130 同构。
func codeBuddyProbeShadowAccount(id, parentID int64) *Account {
	until := time.Now().Add(5 * time.Minute)
	state := TempUnschedState{
		UntilUnix:      until.Unix(),
		MatchedKeyword: openAIAPIKeyHealthBreakerReason,
		Tier:           HealthBreakerTierTrip,
	}
	reason, _ := json.Marshal(state)
	return &Account{
		ID:                      id,
		Platform:                domain.PlatformDeepseek,
		Type:                    AccountTypeOAuth,
		ParentAccountID:         &parentID,
		QuotaDimension:          QuotaDimensionCodeBuddy,
		Extra:                   map[string]any{ShadowModelExtraKey: "deepseek-v4.1-flash"},
		TempUnschedulableUntil:  &until,
		TempUnschedulableReason: string(reason),
		Concurrency:             1,
	}
}

// codeBuddyProbeValidSSE 正常终止的最小 chat.completion.chunk 流（含 [DONE]）。
const codeBuddyProbeValidSSE = "data: {\"id\":\"chatcmpl-probe\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4.1-flash\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

// newProbeServiceWithUpstream 构造挂真实上游桩的探测服务（不走 probeOverride）。
func newProbeServiceWithUpstream(t *testing.T, upstream HTTPUpstream, rl *RateLimitService, repo *probeRepoMock) *AccountHealthRecoveryProbeService {
	t.Helper()
	settings := &OpenAIAPIKeyHealthBreakerSettings{
		Enabled: true,
		Probe:   &OpenAIAPIKeyHealthBreakerProbeSettings{Enabled: true, IntervalSeconds: 30, MaxAttempts: 10},
	}
	ss, _ := newSettingService(t, settings)
	return NewAccountHealthRecoveryProbeService(repo, upstream, &config.Config{}, rl, ss, nil)
}

// --- T4：CodeBuddy 影子探测分派与请求形状 ---

func TestCodeBuddyShadowProbeDispatchesMinimalStreamChat(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	parent := codebuddyProbeParentAccount(109, CodeBuddySiteCN)
	shadow := codeBuddyProbeShadowAccount(130, 109)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{109: parent, 130: shadow},
		candidates: []*Account{shadow},
	}
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, codeBuddyProbeValidSSE)
	p := newProbeServiceWithUpstream(t, upstream, rl, repo)

	p.RunOnce(context.Background())

	// 探测成功 → 既有 ClearTempUnschedulable 恢复路径，无 attempt 记账。
	require.Equal(t, 1, rlRepo.clearTempCalls)
	require.Zero(t, repo.setReasonCall)

	// httptest 上游校验请求形状。
	rec := upstream.lastRecord(t)
	require.Equal(t, http.MethodPost, rec.Method)
	require.Equal(t, "/v2/chat/completions", rec.Path, "必须用 chat completions 端点（禁止 billing meter）")
	require.Equal(t, "Bearer PARENT_TOKEN_SECRET", rec.Header.Get("Authorization"), "凭证取自母账号")
	require.Equal(t, CodeBuddyClientUA, rec.Header.Get("User-Agent"))
	require.Equal(t, "SaaS", rec.Header.Get("X-Product"))

	var body struct {
		Model     string `json:"model"`
		Stream    bool   `json:"stream"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(rec.Body, &body))
	require.Equal(t, "deepseek-v4.1-flash", body.Model, "模型取影子 extra.shadow_model")
	require.True(t, body.Stream, "上游强制流式，探测必须 stream:true")
	require.Equal(t, 1, body.MaxTokens, "max_tokens=1 最小探测")
	require.NotEmpty(t, body.Messages)
	require.Equal(t, "system", body.Messages[0].Role, "system-first（PrepareCodeBuddyBody 规则 3b）")
}

// --- T4：SSE 异常形态 → 探测失败、停调保留 ---

func TestCodeBuddyShadowProbeSSEErrorFrameKeepsParked(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	parent := codebuddyProbeParentAccount(109, CodeBuddySiteCN)
	shadow := codeBuddyProbeShadowAccount(130, 109)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{109: parent, 130: shadow},
		candidates: []*Account{shadow},
	}
	// 部分内容 + event: error 帧（无 [DONE]）：聚合体仍可能有非空 choices，
	// 仅验聚合体会误清熔断（R3-F2），必须判失败。
	errorSSE := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"par\"}}]}\n\n" +
		"event: error\ndata: {\"error\":{\"type\":\"upstream_error\",\"message\":\"boom\"}}\n\n"
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, errorSSE)
	p := newProbeServiceWithUpstream(t, upstream, rl, repo)

	p.RunOnce(context.Background())

	require.Zero(t, rlRepo.clearTempCalls, "错误帧必须判探测失败，不得恢复停调")
	require.Equal(t, 1, repo.setReasonCall, "失败后 attempt 计数 +1")
	var state TempUnschedState
	require.NoError(t, json.Unmarshal([]byte(shadow.TempUnschedulableReason), &state))
	require.Equal(t, 1, state.ProbeAttempts)
}

func TestCodeBuddyShadowProbeTruncatedStreamKeepsParked(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	parent := codebuddyProbeParentAccount(109, CodeBuddySiteCN)
	shadow := codeBuddyProbeShadowAccount(130, 109)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{109: parent, 130: shadow},
		candidates: []*Account{shadow},
	}
	// 截断流：有内容 chunk 但缺 data: [DONE] 正常终止帧 → 判失败。
	truncatedSSE := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"par\"},\"finish_reason\":\"stop\"}]}\n\n"
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, truncatedSSE)
	p := newProbeServiceWithUpstream(t, upstream, rl, repo)

	p.RunOnce(context.Background())

	require.Zero(t, rlRepo.clearTempCalls, "缺 [DONE] 终止帧必须判探测失败")
	require.Equal(t, 1, repo.setReasonCall)
}

func TestCodeBuddyShadowProbeNon2xxKeepsParked(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	parent := codebuddyProbeParentAccount(109, CodeBuddySiteCN)
	shadow := codeBuddyProbeShadowAccount(130, 109)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{109: parent, 130: shadow},
		candidates: []*Account{shadow},
	}
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusTooManyRequests, codeBuddyProbeValidSSE)
	p := newProbeServiceWithUpstream(t, upstream, rl, repo)

	p.RunOnce(context.Background())

	require.Zero(t, rlRepo.clearTempCalls, "非 2xx 不得恢复停调")
	require.Equal(t, 1, repo.setReasonCall)
}

// --- T4：四条件成功判定的纯函数表驱动钉（含业务错误信封形态） ---

func TestEvaluateCodeBuddyShadowProbeResponse(t *testing.T) {
	chunk := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\n"
	cases := []struct {
		name       string
		statusCode int
		raw        string
		want       bool
	}{
		{"valid stream with DONE", http.StatusOK, chunk + "data: [DONE]\n\n", true},
		{"non-2xx", http.StatusServiceUnavailable, chunk + "data: [DONE]\n\n", false},
		{"missing DONE", http.StatusOK, chunk, false},
		{"event error frame", http.StatusOK, chunk + "event: error\ndata: {\"error\":{}}\n\n", false},
		{"data frame business code envelope", http.StatusOK, chunk + "data: {\"code\":11128,\"message\":\"err\"}\n\ndata: [DONE]\n\n", false},
		{"data frame openai error object", http.StatusOK, chunk + "data: {\"error\":{\"message\":\"overloaded\"}}\n\ndata: [DONE]\n\n", false},
		{"pure error envelope no chunks", http.StatusOK, "data: {\"code\":11128}\n\ndata: [DONE]\n\n", false},
		{"non-sse json body", http.StatusOK, "{\"code\":0,\"choices\":[]}", false},
		{"empty body", http.StatusOK, "", false},
		{"code zero envelope is not an error", http.StatusOK, chunk + "data: {\"code\":0}\n\ndata: [DONE]\n\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evaluateCodeBuddyShadowProbeResponse(tc.statusCode, []byte(tc.raw))
			require.Equal(t, tc.want, got)
		})
	}
}

// --- T4 回归：非 CodeBuddy 账号探测路径不变（仍走 /models） ---

func TestProbeAPIKeyAccountKeepsModelsProbePath(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	cand := parkedBreakerAccount(7, 0)
	cand.Credentials = map[string]any{"api_key": "sk-probe-test"}
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{7: cand},
		candidates: []*Account{cand},
	}
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, "{\"object\":\"list\",\"data\":[]}")
	p := newProbeServiceWithUpstream(t, upstream, rl, repo)

	p.RunOnce(context.Background())

	require.Equal(t, 1, rlRepo.clearTempCalls, "apikey 账号 2xx /models 探测成功语义不变")
	rec := upstream.lastRecord(t)
	require.Equal(t, http.MethodGet, rec.Method)
	require.Equal(t, "/v1/models", rec.Path, "非 CodeBuddy 账号仍走既有 /models 探测")
}

// --- T5：pool_mode_401_escalation marker 探测放行 + 恢复出口重置 ---

func TestProbePool401MarkerRecoveryResetsPoolWindowState(t *testing.T) {
	const accID = 126
	resetPool401State(accID)
	t.Cleanup(func() { resetPool401State(accID) })

	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)

	// 预置池 401 窗口：满阈值并锁存 tripped（同 epoch 内不再跨越边沿）。
	now := time.Now()
	for i := 0; i < pool401EscalationThreshold; i++ {
		pool401Windows.record(accID, now)
	}
	pool401Windows.commitEscalation(accID)
	_, shouldEscalate := pool401Windows.record(accID, now)
	require.False(t, shouldEscalate, "同 epoch 已 tripped 的窗口不得再次跨越边沿")

	until := now.Add(pool401EscalationCooldown)
	reason, _ := json.Marshal(pool401EscalationState(now, pool401EscalationThreshold, until))
	cand := &Account{
		ID:                      accID,
		Platform:                domain.PlatformDeepseek,
		Type:                    AccountTypeAPIKey,
		TempUnschedulableUntil:  &until,
		TempUnschedulableReason: string(reason),
	}
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{accID: cand},
		candidates: []*Account{cand},
	}
	p := newProbeService(t, true, 10, rl, repo)
	p.SetProbeOverride(func(_ context.Context, _ *Account) (bool, error) { return true, nil })

	p.RunOnce(context.Background())

	// marker 放行：探测成功 → ClearTempUnschedulable 生效。
	require.Equal(t, 1, rlRepo.clearTempCalls)

	// resetPool401State 已被调用：窗口条目清空。
	pool401Windows.mu.Lock()
	_, entryExists := pool401Windows.entries[accID]
	pool401Windows.mu.Unlock()
	require.False(t, entryExists, "探测恢复必须重置池 401 窗口状态机（R4-F3）")

	// 同一账号再次 401 满 6 次 → 能重新跨越边沿（再次停调路径可用）。
	next := now.Add(time.Minute)
	for i := 0; i < pool401EscalationThreshold-1; i++ {
		_, should := pool401Windows.record(accID, next)
		require.False(t, should)
	}
	_, should := pool401Windows.record(accID, next)
	require.True(t, should, "恢复后同一 epoch 再次 401 满阈值必须能重新跨越边沿")
}

func TestProbePool401MarkerAccountIsSelected(t *testing.T) {
	// marker 放行的选号面：pool_mode_401_escalation 停调必须被 isHealthBreakerTrip 认领。
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	now := time.Now()
	until := now.Add(pool401EscalationCooldown)
	reason, _ := json.Marshal(pool401EscalationState(now, pool401EscalationThreshold, until))
	cand := &Account{
		ID:                      126,
		Platform:                domain.PlatformDeepseek,
		Type:                    AccountTypeAPIKey,
		TempUnschedulableUntil:  &until,
		TempUnschedulableReason: string(reason),
	}
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{126: cand},
		candidates: []*Account{cand},
	}
	p := newProbeService(t, true, 10, rl, repo)
	overrideCalls := 0
	p.SetProbeOverride(func(_ context.Context, _ *Account) (bool, error) {
		overrideCalls++
		return false, nil
	})

	p.RunOnce(context.Background())

	require.Equal(t, 1, overrideCalls, "池 401 升级停调必须被探测选号认领")
	require.Zero(t, rlRepo.clearTempCalls)
}

// --- T5 回归：健康熔断 marker 的恢复不触碰池 401 窗口 ---

func TestProbeHealthBreakerMarkerKeepsPoolWindowStateUntouched(t *testing.T) {
	const accID = 7
	resetPool401State(accID)
	t.Cleanup(func() { resetPool401State(accID) })

	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{accID: parkedBreakerAccount(accID, 0)},
		candidates: []*Account{parkedBreakerAccount(accID, 0)},
	}
	p := newProbeService(t, true, 10, rl, repo)
	p.SetProbeOverride(func(_ context.Context, _ *Account) (bool, error) { return true, nil })

	pool401Windows.record(accID, time.Now())

	p.RunOnce(context.Background())

	require.Equal(t, 1, rlRepo.clearTempCalls, "健康熔断 marker 恢复行为不变")
	pool401Windows.mu.Lock()
	_, entryExists := pool401Windows.entries[accID]
	pool401Windows.mu.Unlock()
	require.True(t, entryExists, "健康熔断 marker 的恢复不得重置池 401 窗口")
}
