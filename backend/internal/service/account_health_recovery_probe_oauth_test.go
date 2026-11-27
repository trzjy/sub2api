//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// TestAccountHealthRecoveryProbeOAuthDataPlaneRouting 验证恢复探针的 OAuth 分流接线：
// OpenAI OAuth 号经 probeUpstreamDataPlaneWithProtocol 不再落 APIKey 分支报
// "probe account has no api key"，而是用 access_token 打 Codex /responses 端点，
// 并按 SSE 终态帧判定健康。
//
// 不注入 probeOverride——它在 runProbe 最前面直接返回，会绕过待测分流。这里用
// codeBuddyProbeUpstreamStub：它把请求的 host 改写到 httptest 服务器、path 保留
// chatgptCodexURL 的 /backend-api/codex/responses，因此可在不修改固定端点常量的
// 前提下让探针真发一次请求到假 Codex 端点并校验请求形状。
func TestAccountHealthRecoveryProbeOAuthDataPlaneRouting(t *testing.T) {
	// 正例必须是**有真实输出或真实 usage** 的终态帧：只有 id/status 的空 completed 帧
	// 是上游静默拒绝的形状（站内转发侧据此 failover，issue #5009），不得当成健康正例。
	completedSSE := "event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_probe\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}],\"usage\":{\"input_tokens\":11,\"output_tokens\":5,\"total_tokens\":16}}}\n\n" +
		"data: [DONE]\n\n"
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, completedSSE)
	p := &AccountHealthRecoveryProbeService{
		cfg:          &config.Config{},
		httpUpstream: upstream,
		accountRepo:  &oauthProbeFakeRepo{},
	}
	acc := &Account{
		ID:       990,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		// model_mapping 与生产 OAuth 号形态一致（生产 990/991 各带 10 条「键=值」映射）。
		// 探针模型必须取自账号真实可服务模型，取不到时按 degraded 语义（无模型可探），
		// 绝回落上游不支持的占位模型 gpt-5.4（生产实证 400 model_not_found）。
		Credentials: map[string]any{
			"access_token": "oauth-probe-token",
			"model_mapping": map[string]any{
				"gpt-6-astra": "gpt-6-astra",
				"gpt-6.1-sol": "gpt-6.1-sol",
			},
		},
		Concurrency: 1,
	}

	// 前提坐实：该 OAuth 号在旧 APIKey 分支确实取不到 key（根因），而分流谓词已命中。
	require.Empty(t, acc.GetOpenAIProtocolAPIKey(), "OAuth 号在旧 APIKey 分支无凭据，正是修复前提")
	require.True(t, isOpenAIOAuthProbeAccount(acc), "必须命中 OAuth 分流谓词")

	ok, err := p.probeUpstreamDataPlaneWithProtocol(context.Background(), acc, false)
	require.NoError(t, err, "OAuth 号不得再报 probe account has no api key（必须走 OAuth 分流）")
	require.True(t, ok, "假 Codex SSE completed 帧应判健康、可解除停调")

	rec := upstream.lastRecord(t)
	require.Equal(t, http.MethodPost, rec.Method)
	require.Equal(t, "/backend-api/codex/responses", rec.Path,
		"必须走 OAuth Codex 端点，不是 /v1/chat/completions")
	require.Equal(t, "Bearer oauth-probe-token", rec.Header.Get("Authorization"),
		"必须用 access_token 凭据，不是 api_key")
}

// TestAccountHealthRecoveryProbeOAuthEmptyCompletedFrameIsUnhealthy 是 BLOCKER-2 的
// 承重用例：上游回一个「只有 id/status、无 output/usage/error」的空 completed 帧时，
// 探针必须判不健康（不解除停调）——站内转发对这种流明确判定为上游静默拒绝并
// failover（issue #5009），探针若当成健康，一次探测即可清除额度/传输分量停调。
func TestAccountHealthRecoveryProbeOAuthEmptyCompletedFrameIsUnhealthy(t *testing.T) {
	emptyCompletedSSE := "event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_probe\",\"status\":\"completed\"}}\n\n" +
		"data: [DONE]\n\n"
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, emptyCompletedSSE)
	p := &AccountHealthRecoveryProbeService{
		cfg:          &config.Config{},
		httpUpstream: upstream,
		accountRepo:  &oauthProbeFakeRepo{},
	}
	acc := &Account{
		ID:       990,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "oauth-probe-token",
			"model_mapping": map[string]any{
				"gpt-6-astra": "gpt-6-astra",
			},
		},
		Concurrency: 1,
	}

	ok, err := p.probeUpstreamDataPlaneWithProtocol(context.Background(), acc, false)
	require.NoError(t, err, "空 completed 帧是响应内容问题，不是传输/配置错误")
	require.False(t, ok, "空 completed 帧（上游静默拒绝）必须判不健康，不得解除停调")
}

// TestAccountHealthRecoveryProbeOAuthTruncatedStreamIsUnhealthy 是 OAuth 流读取错误
// blocker 的回归用例：先给一段合法的 response.completed 帧（足以让
// probeOpenAIOAuthResponseOK 判成功），随后注入非 EOF 读错误。若探针忽略 io.ReadAll
// 的错误，就会拿已读到的合法终态帧判伪恢复、误清停调；必须失败关闭（ok=false）。
func TestAccountHealthRecoveryProbeOAuthTruncatedStreamIsUnhealthy(t *testing.T) {
	completedSSE := "event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_probe\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}],\"usage\":{\"input_tokens\":11,\"output_tokens\":5,\"total_tokens\":16}}}\n\n" +
		"data: [DONE]\n\n"
	body := &truncatedProbeBody{
		payload: []byte(completedSSE),
		err:     errors.New("simulated oauth stream interruption after completed frame"),
	}
	upstream := &truncatedProbeUpstream{status: http.StatusOK, body: body}
	p := &AccountHealthRecoveryProbeService{
		cfg:          &config.Config{},
		httpUpstream: upstream,
		accountRepo:  &oauthProbeFakeRepo{},
	}
	acc := &Account{
		ID:       990,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "oauth-probe-token",
			"model_mapping": map[string]any{
				"gpt-6-astra": "gpt-6-astra",
			},
		},
		Concurrency: 1,
	}

	// 前提坐实：这段 payload 单独判定时确实是健康成功体，否则测试无法证明
	// 「忽略读错误 ⇒ 伪恢复」这条因果链。
	require.True(t, probeOpenAIOAuthResponseOK(http.StatusOK, []byte(completedSSE)),
		"注入的 completed 帧本身必须是合法成功体")

	ok, err := p.probeUpstreamDataPlaneWithProtocol(context.Background(), acc, false)
	require.NoError(t, err, "读错误与传输失败同语义，不是配置/降级错误")
	require.False(t, ok, "合法 completed 帧后跟非 EOF 读错误必须失败关闭，不得伪恢复")

	require.Equal(t, 1, upstream.calls, "探测必须真实发出")
	require.True(t, body.errored, "必须实际触发注入的读错误，避免测试空过")
}
