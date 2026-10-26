//go:build unit

// C1 定向测试：探针改探真实数据面 + 数据面判定 + 探测模型选择。
//
// 背景：旧实现探 GET {base}/v1/models，只要 2xx 即判定健康。中转站的 /models 由轻量
// 前端直接返回模型清单，与 chat/responses 数据面不共用链路——上游数据面全挂时 /models
// 照样 200，熔断 3~14 秒即被撤销（生产实证 2026-10-25 账号 137，11 次熔断全部在 3~14 秒
// 内被 attempt=1 的探测撤销，同期该账号持续报 cn upstream first byte timeout）。
//
// 本文件覆盖：
//   - evaluateProbeDataPlaneResponse：2xx 之外还要「合法 JSON + 无 error 信封 + 协议对应
//     成功体结构」，杜绝 200 + {"error":...} / 空体 / 非 JSON 误判健康；
//   - probeModelForAccount：熔断记录的触发模型优先，其次平台默认测试模型，最后
//     全局默认；空模型按降级处理；
//   - probeUpstreamDataPlane：按账号协议打真实数据面端点（非 /models）；无凭据等配置
//     错误按既有 degraded 语义返回 error，传输失败按「不健康」返回 (false, nil)。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// --- evaluateProbeDataPlaneResponse：只有真数据面成功体才算健康 ---

func TestEvaluateProbeDataPlaneResponse(t *testing.T) {
	chatOK := `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"content":"hi"}}]}`
	responsesOK := `{"id":"resp-1","object":"response","status":"completed"}`
	cases := []struct {
		name         string
		statusCode   int
		body         string
		useResponses bool
		want         bool
	}{
		{"chat 2xx with choices", http.StatusOK, chatOK, false, true},
		{"chat 201 with choices", http.StatusCreated, chatOK, false, true},
		{"chat 2xx error envelope", http.StatusOK, `{"error":{"message":"first byte timeout","type":"upstream_error"}}`, false, false},
		{"chat 2xx empty body", http.StatusOK, ``, false, false},
		{"chat 2xx blank body", http.StatusOK, "   \n", false, false},
		{"chat 2xx non-json body", http.StatusOK, `<html>gateway</html>`, false, false},
		{"chat 2xx json without choices", http.StatusOK, `{"object":"list","data":[]}`, false, false},
		{"chat 2xx choices not array", http.StatusOK, `{"choices":"oops"}`, false, false},
		{"chat 4xx with choices", http.StatusBadRequest, chatOK, false, false},
		{"chat 5xx", http.StatusBadGateway, `{"error":{"message":"boom"}}`, false, false},
		{"responses 2xx with id", http.StatusOK, responsesOK, true, true},
		{"responses 2xx with only object", http.StatusOK, `{"object":"response"}`, true, true},
		{"responses 2xx error envelope", http.StatusOK, `{"error":{"message":"upstream first byte timeout"}}`, true, false},
		{"responses 2xx empty body", http.StatusOK, ``, true, false},
		{"responses 2xx without id/object", http.StatusOK, `{"status":"completed"}`, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, evaluateProbeDataPlaneResponse(tc.statusCode, []byte(tc.body), tc.useResponses))
		})
	}
}

// --- probeModelForAccount：熔断记录的触发模型优先 ---

func TestProbeModelForAccount(t *testing.T) {
	svc := &AccountHealthRecoveryProbeService{}

	// 1) 熔断 state 里记录的触发模型优先（探的就是故障模型）。
	withModel := parkedBreakerAccountWithState(1, 0, 0, "cn-upstream-model")
	require.Equal(t, "cn-upstream-model", svc.probeModelForAccount(withModel))

	// 2) state 无模型 → 平台默认测试模型；无 profile 时回落全局默认。
	noModel := parkedBreakerAccount(2, 0)
	require.Equal(t, openai.DefaultTestModel, svc.probeModelForAccount(noModel))

	// 3) reason 无法解析 → 仍回落默认模型，不得空手构造请求。
	plain := &Account{ID: 3}
	require.Equal(t, openai.DefaultTestModel, svc.probeModelForAccount(plain))

	// 4) nil 账号 → 空模型（调用方按降级处理）。
	require.Empty(t, svc.probeModelForAccount(nil))
}

// --- probeUpstreamDataPlane：配置类错误按 degraded 返回 error ---

func TestProbeUpstreamDataPlaneConfigErrors(t *testing.T) {
	p := &AccountHealthRecoveryProbeService{cfg: &config.Config{}, httpUpstream: &failingProbeUpstream{}}
	ctx := context.Background()

	// nil 账号。
	_, err := p.probeUpstreamDataPlane(ctx, nil)
	require.Error(t, err, "nil 账号必须返回 error（降级），不得伪造失败")

	// 无可用凭据 → 配置错误降级。
	noKey := parkedBreakerAccount(11, 0)
	noKey.Credentials = map[string]any{}
	_, err = p.probeUpstreamDataPlane(ctx, noKey)
	require.Error(t, err, "无 api key 必须返回 error（降级到期恢复）")
}

// probeUpstreamDataPlane 打的是真实数据面端点（POST chat/completions），不再打
// /v1/models；成功判定走响应体，200 + error 信封不得判健康。
func TestProbeUpstreamDataPlaneUsesChatCompletionsAndJudgesBody(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	cand := parkedBreakerAccount(21, 0)
	cand.Credentials = map[string]any{"api_key": "sk-probe-test"}
	cand.Extra = map[string]any{openai_compat.ExtraKeyResponsesSupported: false}
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{21: cand},
		candidates: []*Account{cand},
	}
	// 200 + error 信封（部分中转站的业务失败形态）：必须判失败、保持停调。
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, `{"error":{"message":"first byte timeout"}}`)
	p := newProbeServiceWithUpstream(t, upstream, rl, repo)
	setFastProbeRetry(p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream.cancelAfterRequests(2, cancel)

	p.RunOnce(ctx)
	p.wg.Wait()

	require.Zero(t, repo.clearTempIfReasonCalls, "200 + error 信封不得判健康（这正是熔断被误撤销的形态）")
	rec := upstream.lastRecord(t)
	require.Equal(t, http.MethodPost, rec.Method, "必须发真实 POST 数据面请求")
	require.Equal(t, "/v1/chat/completions", rec.Path, "不得再探 /v1/models")
	require.Equal(t, "Bearer sk-probe-test", rec.Header.Get("Authorization"))
	require.Equal(t, "application/json", rec.Header.Get("Content-Type"))

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body, &body))
	messages, ok := body["messages"].([]any)
	require.True(t, ok, "探测体必须是最小 chat 请求（含 messages）")
	require.Equal(t, "user", messages[0].(map[string]any)["role"])
	require.Equal(t, float64(1), body["max_tokens"], "max_tokens=1 最小探测")
	require.Equal(t, false, body["stream"], "非流式探测")
}

// 传输层失败（上游不可达）按「不健康」处理，返回 (false, nil) 而非 error——保持停调，
// 不得误触降级到期恢复。
func TestProbeUpstreamDataPlaneTransportFailureIsUnhealthy(t *testing.T) {
	p := &AccountHealthRecoveryProbeService{
		cfg:          &config.Config{},
		httpUpstream: &failingProbeUpstream{err: errors.New("dial tcp: connection refused")},
	}
	cand := parkedBreakerAccount(22, 0)
	cand.Credentials = map[string]any{"api_key": "sk-probe-test"}

	ok, err := p.probeUpstreamDataPlane(context.Background(), cand)
	require.NoError(t, err, "传输失败不是降级：不得返回 error")
	require.False(t, ok, "上游不可达即不健康，保持停调")
}

// failingProbeUpstream 模拟上游不可达（DoWithTLS 直接返回错误）。
type failingProbeUpstream struct {
	err error
}

func (f *failingProbeUpstream) Do(*http.Request, string, int64, int) (*http.Response, error) {
	return nil, f.err
}

func (f *failingProbeUpstream) DoWithTLS(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error) {
	return nil, f.err
}
