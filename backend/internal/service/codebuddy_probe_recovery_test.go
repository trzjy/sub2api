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
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
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
	// onRequest 在记录每次请求后回调（服务端请求计数）。C1 起补探无轮数上限，失败场景
	// 只有取消 ctx 才能让补探循环收敛，用服务端收到的请求数划定边界最确定。
	onRequest func(requests int)
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
		hook := stub.onRequest
		requests := len(stub.records)
		stub.mu.Unlock()
		if hook != nil {
			hook(requests)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, stub.sseBody)
	}))
	t.Cleanup(server.Close)
	stub.serverURL = strings.TrimPrefix(server.URL, "http://")
	stub.client = server.Client()
	return stub
}

// cancelAfterRequests 让桩在累计收到 n 次探测请求后取消 ctx：与 setFastProbeRetry 搭配，
// 使「永久失败」场景的无上限补探按 ctx.Done 收敛，探测次数因此可确定断言。
func (s *codeBuddyProbeUpstreamStub) cancelAfterRequests(n int, cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onRequest = func(requests int) {
		if requests >= n {
			cancel()
		}
	}
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
		Probe:   &OpenAIAPIKeyHealthBreakerProbeSettings{Enabled: true, IntervalSeconds: 30},
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
	setFastProbeRetry(p)

	// C1：连续 2 次真实探测成功才恢复；首成功后异步补探在同一 sweep 内补齐第二次。
	p.RunOnce(context.Background())
	p.wg.Wait()

	require.Equal(t, 1, repo.clearTempIfReasonCalls)
	require.Equal(t, 1, repo.bumpTempIfReasonCalls, "首成功后回写连续成功计数，第二次成功才清除")

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
	setFastProbeRetry(p)
	// 永久失败：第 2 次探测后取消 ctx 划定补探边界（C1 补探无轮数上限）。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream.cancelAfterRequests(2, cancel)

	p.RunOnce(ctx)
	p.wg.Wait() // 等待异步补探按 ctx 取消收敛（AC-C1）

	require.Zero(t, repo.clearTempIfReasonCalls, "错误帧必须判探测失败，不得恢复停调")
	require.Equal(t, 2, repo.bumpTempIfReasonCalls, "失败后 attempt 计数 +1（首探 + 1 次补探）")
	var state TempUnschedState
	require.NoError(t, json.Unmarshal([]byte(shadow.TempUnschedulableReason), &state))
	require.Equal(t, 2, state.ProbeAttempts)
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
	setFastProbeRetry(p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream.cancelAfterRequests(2, cancel)

	p.RunOnce(ctx)
	p.wg.Wait() // 等待异步补探按 ctx 取消收敛（AC-C1）

	require.Zero(t, repo.clearTempIfReasonCalls, "缺 [DONE] 终止帧必须判探测失败")
	require.Equal(t, 2, repo.bumpTempIfReasonCalls, "首探 + 1 次补探")
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
	setFastProbeRetry(p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream.cancelAfterRequests(2, cancel)

	p.RunOnce(ctx)
	p.wg.Wait() // 等待异步补探按 ctx 取消收敛（AC-C1）

	require.Zero(t, repo.clearTempIfReasonCalls, "非 2xx 不得恢复停调")
	require.Equal(t, 2, repo.bumpTempIfReasonCalls, "首探 + 1 次补探")
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

// --- T4 回归：非 CodeBuddy 账号探测路径为真实数据面（C1 不再探 /models） ---

// C1：apikey 账号不再探 GET /v1/models（中转站 /models 由轻量前端直接返回，不共用
// chat/responses 数据面链路），改为按账号真实协议 POST 数据面请求并按响应体判定。
func TestProbeAPIKeyAccountUsesDataPlane(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	cand := parkedBreakerAccount(7, 0)
	cand.Credentials = map[string]any{"api_key": "sk-probe-test"}
	// 探测确认不支持 Responses 的账号 → 数据面走 chat/completions（CC 直转路径）。
	cand.Extra = map[string]any{openai_compat.ExtraKeyResponsesSupported: false}
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{7: cand},
		candidates: []*Account{cand},
	}
	// 数据面成功体：合法 JSON、无 error 信封、含 choices 数组。
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK,
		`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	p := newProbeServiceWithUpstream(t, upstream, rl, repo)
	setFastProbeRetry(p)

	// C1：连续 2 次数据面成功才恢复（首成功后由异步补探补齐第二次）。
	p.RunOnce(context.Background())
	p.wg.Wait()

	require.Equal(t, 1, repo.clearTempIfReasonCalls, "apikey 账号数据面探测连续成功必须恢复")
	rec := upstream.lastRecord(t)
	require.Equal(t, http.MethodPost, rec.Method, "数据面探测必须是 POST 真实请求，而非 GET /models")
	require.Equal(t, "/v1/chat/completions", rec.Path, "不支持 Responses 的账号必须打真实 chat/completions 数据面端点")
	require.NotEqual(t, "/v1/models", rec.Path, "不得再探 /models（不共用数据面链路）")
	require.Equal(t, "Bearer sk-probe-test", rec.Header.Get("Authorization"), "数据面探测用账号自身凭据")
}

// C1：未探测 Responses 能力的账号沿用转发链默认（Responses）→ 数据面探测打 /v1/responses，
// 成功判定要求响应体为合法 JSON、无 error 信封且含 id/object。
func TestProbeAPIKeyAccountDataPlaneResponsesByDefault(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	cand := parkedBreakerAccount(7, 0)
	cand.Credentials = map[string]any{"api_key": "sk-probe-test"}
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{7: cand},
		candidates: []*Account{cand},
	}
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, `{"id":"resp-1","object":"response","status":"completed"}`)
	p := newProbeServiceWithUpstream(t, upstream, rl, repo)
	setFastProbeRetry(p)

	p.RunOnce(context.Background())
	p.wg.Wait()

	require.Equal(t, 1, repo.clearTempIfReasonCalls, "Responses 数据面探测连续成功必须恢复")
	rec := upstream.lastRecord(t)
	require.Equal(t, http.MethodPost, rec.Method)
	require.Equal(t, "/v1/responses", rec.Path, "未探测能力的账号默认走 Responses 数据面（与转发链同源）")
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
	p := newProbeService(t, true, rl, repo)
	p.SetProbeOverride(func(_ context.Context, _ *Account) (bool, error) { return true, nil })
	setFastProbeRetry(p)

	// marker 放行 + 连续 2 次探测成功（C1）→ 清除。
	p.RunOnce(context.Background())
	p.wg.Wait()

	// marker 放行：探测成功 → ClearTempUnschedulable 生效。
	require.Equal(t, 1, repo.clearTempIfReasonCalls)

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
	p := newProbeService(t, true, rl, repo)
	var overrideCalls int32
	setFastProbeRetry(p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.SetProbeOverride(func(_ context.Context, _ *Account) (bool, error) {
		// 永久失败：第 2 次探测后取消 ctx，划定无上限补探的测试边界。
		if atomic.AddInt32(&overrideCalls, 1) >= 2 {
			cancel()
		}
		return false, nil
	})

	p.RunOnce(ctx)
	p.wg.Wait() // 等待异步补探按 ctx 取消收敛（AC-C1）

	require.Equal(t, int32(2), atomic.LoadInt32(&overrideCalls),
		"池 401 升级停调必须被探测选号认领（首探 + 1 次补探）")
	require.Zero(t, repo.clearTempIfReasonCalls)
}

// --- T5 回归：健康熔断 marker 的恢复不触碰池 401 窗口 ---

func TestProbeHealthBreakerMarkerKeepsPoolWindowStateUntouched(t *testing.T) {
	const accID = 7
	resetPool401State(accID)
	t.Cleanup(func() { resetPool401State(accID) })

	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	acc := parkedBreakerAccount(accID, 0)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{accID: acc},
		candidates: []*Account{acc},
	}
	p := newProbeService(t, true, rl, repo)
	p.SetProbeOverride(func(_ context.Context, _ *Account) (bool, error) { return true, nil })
	setFastProbeRetry(p)

	pool401Windows.record(accID, time.Now())

	// C1：连续 2 次探测成功才恢复，marker 分派语义不变。
	p.RunOnce(context.Background())
	p.wg.Wait()

	require.Equal(t, 1, repo.clearTempIfReasonCalls, "健康熔断 marker 恢复行为不变")
	pool401Windows.mu.Lock()
	_, entryExists := pool401Windows.entries[accID]
	pool401Windows.mu.Unlock()
	require.True(t, entryExists, "健康熔断 marker 的恢复不得重置池 401 窗口")
}
