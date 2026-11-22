//go:build unit

package service

import (
	"context"
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
	completedSSE := "event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_probe\",\"status\":\"completed\"}}\n\n" +
		"data: [DONE]\n\n"
	upstream := newCodeBuddyProbeUpstreamStub(t, http.StatusOK, completedSSE)
	p := &AccountHealthRecoveryProbeService{
		cfg:          &config.Config{},
		httpUpstream: upstream,
		accountRepo:  &oauthProbeFakeRepo{},
	}
	acc := &Account{
		ID:          990,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-probe-token"},
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
