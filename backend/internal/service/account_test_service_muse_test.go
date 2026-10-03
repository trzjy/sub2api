//go:build unit

package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// muse 账号（PlatformMuse，APIKey 类型）经 TestAccountConnection 应路由到
// testOpenAIAccountConnection 做 Responses 探活，而非落入 claude 兜底（Anthropic 形态）。
// 这正是 ZB-T2 要消灭的误标来源：muse 上游为 Responses 协议，错误协议探测 + 兜底失败
// 路径 SetError 会把生产账号标 status=error/schedulable=false。
//
// 下列各测试复用 openai 测试文件的共享助手（queuedHTTPUpstream / openAIAccountTestRepo /
// newTestContext / newJSONResponse / requireOpenAICodexProbeHeaders 等），均与本文件同属
// //go:build unit 且 package service。

// 构造一个 muse 账号与承载它的 mock repo。
func newMuseTestSetup(t *testing.T, account *Account) (*AccountTestService, *openAIAccountTestRepo, *queuedHTTPUpstream, *gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx, recorder := newTestContext()

	upstream := &queuedHTTPUpstream{}
	repo := &openAIAccountTestRepo{
		mockAccountRepoForGemini: mockAccountRepoForGemini{
			accountsByID: map[int64]*Account{account.ID: account},
		},
	}
	svc := &AccountTestService{
		accountRepo:  repo,
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	return svc, repo, upstream, ctx, recorder
}

func completedResponsesBody() io.ReadCloser {
	return io.NopCloser(strings.NewReader(`data: {"type":"response.completed"}

`))
}

// ① + ② muse 账号路由到 Responses 探活，断言请求形态（URL/base_url、协议、鉴权头），
// 并显式验证未落入 claude 兜底（无 x-api-key、非 Anthropic /v1/messages 形态）。
func TestAccountTestService_MuseRoutesToResponsesProbe(t *testing.T) {
	account := &Account{
		ID:           164,
		Platform:     PlatformMuse,
		Type:         AccountTypeAPIKey,
		Status:       StatusActive,
		Concurrency:  1,
		Credentials: map[string]any{
			"api_key":  "muse-sk",
			"base_url": "https://muse.example.com/v1",
		},
	}
	svc, repo, upstream, ctx, recorder := newMuseTestSetup(t, account)

	resp := newJSONResponse(http.StatusOK, "")
	resp.Body = completedResponsesBody()
	upstream.responses = []*http.Response{resp}

	err := svc.TestAccountConnection(ctx, account.ID, "muse-model", "", "")
	require.NoError(t, err)

	// 恰好一次上游探活请求，且为 Responses 端点。
	require.Len(t, upstream.requests, 1)
	req := upstream.requests[0]
	require.Equal(t, http.MethodPost, req.Method)
	require.Equal(t, "https://muse.example.com/v1/responses", req.URL.String())
	require.Equal(t, HTTPUpstreamProfileOpenAI, HTTPUpstreamProfileFromContext(req.Context()))

	// 鉴权：Bearer credentials.api_key（muse 走 GetOpenAIProtocolAPIKey 直读 api_key）。
	require.Equal(t, "Bearer muse-sk", req.Header.Get("Authorization"))
	// 协议标头与 OpenAI Responses 探活一致。
	requireOpenAICodexProbeHeaders(t, req.Header)

	// 显式锚定：未落入 claude 兜底 —— claude 兜底用 x-api-key 而非 Bearer。
	require.Empty(t, req.Header.Get("x-api-key"))
	// 成功路径不得误标账号。
	require.Zero(t, repo.setErrorID)
	require.Contains(t, recorder.Body.String(), `"success":true`)
}

// ②（续）muse 账号未配置自定义 base_url 时，回落 DefaultMuseBaseURL。
func TestAccountTestService_MuseUsesDefaultMuseBaseURL(t *testing.T) {
	account := &Account{
		ID:          165,
		Platform:    PlatformMuse,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key": "muse-sk-default",
		},
	}
	svc, _, upstream, ctx, _ := newMuseTestSetup(t, account)

	resp := newJSONResponse(http.StatusOK, "")
	resp.Body = completedResponsesBody()
	upstream.responses = []*http.Response{resp}

	err := svc.TestAccountConnection(ctx, account.ID, "", "", "")
	require.NoError(t, err)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, DefaultMuseBaseURL+"/responses", upstream.requests[0].URL.String())
	require.Equal(t, "Bearer muse-sk-default", upstream.requests[0].Header.Get("Authorization"))
}

// ③ 成功路径：Responses 探活成功，不触发任何错误标记。
func TestAccountTestService_MuseSuccessDoesNotSetError(t *testing.T) {
	account := &Account{
		ID:          166,
		Platform:    PlatformMuse,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "muse-sk",
			"base_url": "https://muse.example.com/v1",
		},
	}
	svc, repo, upstream, ctx, _ := newMuseTestSetup(t, account)

	resp := newJSONResponse(http.StatusOK, "")
	resp.Body = completedResponsesBody()
	upstream.responses = []*http.Response{resp}

	err := svc.TestAccountConnection(ctx, account.ID, "muse-model", "", "")
	require.NoError(t, err)
	require.Zero(t, repo.setErrorID)
	require.Empty(t, repo.setErrorMsg)
}

// ③（续）失败路径：真实探活失败（401 鉴权失败）仍正确走 SetError 标记账号，
// 失败标记语义保留（本单修的是"错误协议探测 + 兜底误标"，不是取消失败标记）。
// 注意：此处不验证 403 误标旧行为（那是被消灭的 bug，而非应保留的语义）。
func TestAccountTestService_MuseFailureStillSetsError(t *testing.T) {
	account := &Account{
		ID:          167,
		Platform:    PlatformMuse,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "muse-sk-bad",
			"base_url": "https://muse.example.com/v1",
		},
	}
	svc, repo, _, ctx, _ := newMuseTestSetup(t, account)

	// 401 走的是 muse 自己的 Responses 探活（正确协议），失败路径仍正确标记。
	upstream := &queuedHTTPUpstream{responses: []*http.Response{
		newJSONResponse(http.StatusUnauthorized, `{"error":"bad token"}`),
	}}
	svc.httpUpstream = upstream

	err := svc.TestAccountConnection(ctx, account.ID, "muse-model", "", "")
	require.Error(t, err)
	require.Equal(t, account.ID, repo.setErrorID)
	require.Contains(t, repo.setErrorMsg, "Authentication failed (401)")
}
