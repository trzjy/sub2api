package service

// web 探活 401 单口径续期重试回归测试（任务卡 TC-web-test-refresh-20260921 §2.4）。
//
// 覆盖：
//   - kimi web 探活 401 → refreshWebKimiAccessToken（与转发链同一实现）→ 新 token 重试 → 成功；
//   - kimi refresh 失败 → 保持既有 "web login credential is invalid (HTTP 401)" 文案；
//   - zhipu web 探活 401 → refreshWebZhipuAccessToken → 新 cookie 重试 → 成功；
//   - 403 不触发任何 refresh 端点调用（封禁语义不续期）。
//
// 凭证安全红线断言：SSE 输出绝不包含 token/cookie 值。

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

// newWebRefreshProbeTestService 组装探活服务 + 网关共享同一 recorder（探活与刷新
// 请求都落到同一响应队列/请求记录，按 URL 区分探活端点与 refresh 端点）。
func newWebRefreshProbeTestService(repo *kimiRefreshRepoStub) (*AccountTestService, *httpUpstreamRecorder) {
	recorder := &httpUpstreamRecorder{}
	cfg := &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true},
		},
	}
	svc := &AccountTestService{
		accountRepo:          repo,
		httpUpstream:         recorder,
		cfg:                  cfg,
		tlsFPProfileService:  &TLSFingerprintProfileService{},
		openaiGatewayService: &OpenAIGatewayService{accountRepo: repo, httpUpstream: recorder, cfg: cfg},
	}
	return svc, recorder
}

func newWebRefreshTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/1/test", nil)
	return c, rec
}

func unauthorizedTextResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("unauthorized")),
	}
}

func forbiddenTextResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusForbidden,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("forbidden")),
	}
}

const webProbeRefreshStatusText = "登录态已过期，正在用 refresh token 静默续期并重试"

// 用例 1：kimi web 探活首发 401 → 静默续期（同一 refresh 实现）→ 新 token 重试 200。
func TestAccountTestService_KimiWebProbe401RefreshesAndRetries(t *testing.T) {
	account := &Account{
		ID:          7001,
		Platform:    PlatformKimi,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_mode":   AccountAccessModeWeb,
			"access_token":  "expired-access-token",
			"refresh_token": "valid-refresh-token",
		},
	}
	repo := &kimiRefreshRepoStub{}
	svc, recorder := newWebRefreshProbeTestService(repo)
	recorder.responses = []*http.Response{
		unauthorizedTextResponse(), // 首发 401
		refreshResponse(http.StatusOK, `{"accessToken":"NEW-ACCESS-TOKEN"}`), // refresh 成功
		okSSEResponse(), // 新 token 重试 200
	}
	c, rec := newWebRefreshTestContext()

	err := svc.testWebAccountConnection(c, account, "", "")
	require.NoError(t, err)

	require.Len(t, recorder.requests, 3, "首发 + refresh + 重试共三次出站")
	// 首发：旧 token 打探活端点。
	require.Equal(t, webKimiChatPath, recorder.requests[0].URL.Path)
	require.Equal(t, "Bearer expired-access-token", recorder.requests[0].Header.Get("Authorization"))
	// refresh：与转发链同一端点（webKimiRefreshTokenURL），Authorization 不带 Bearer access_token。
	require.Equal(t, webKimiRefreshTokenURL, recorder.requests[1].URL.String())
	// 重试：新 token 重建请求打同一探活端点。
	require.Equal(t, webKimiChatPath, recorder.requests[2].URL.Path)
	require.Equal(t, "Bearer NEW-ACCESS-TOKEN", recorder.requests[2].Header.Get("Authorization"))

	// SSE：续期 status 文案 + 成功收口；且不泄露任何 token 值。
	body := rec.Body.String()
	require.Contains(t, body, webProbeRefreshStatusText)
	require.Contains(t, body, `"type":"test_complete"`)
	require.Contains(t, body, `"success":true`)
	require.NotContains(t, body, "expired-access-token")
	require.NotContains(t, body, "valid-refresh-token")
	require.NotContains(t, body, "NEW-ACCESS-TOKEN")

	// 凭据持久化：新 access_token 经 persistAccountCredentials 落库。
	require.Equal(t, account.ID, repo.updatedID)
	require.Equal(t, "NEW-ACCESS-TOKEN", repo.updatedCreds["access_token"])
	require.Equal(t, "NEW-ACCESS-TOKEN", account.GetCredential("access_token"), "内存对象同步更新")
}

// 用例 2：kimi refresh 端点失败（401）→ 不重试、不发续期 status，保持既有失效文案。
func TestAccountTestService_KimiWebProbe401RefreshFailsReportsInvalid(t *testing.T) {
	account := &Account{
		ID:          7002,
		Platform:    PlatformKimi,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_mode":   AccountAccessModeWeb,
			"access_token":  "expired-access-token",
			"refresh_token": "dead-refresh-token",
		},
	}
	repo := &kimiRefreshRepoStub{}
	svc, recorder := newWebRefreshProbeTestService(repo)
	recorder.responses = []*http.Response{
		unauthorizedTextResponse(),
		refreshResponse(http.StatusUnauthorized, `{"code":"unauthenticated"}`), // refresh 失败
	}
	c, rec := newWebRefreshTestContext()

	err := svc.testWebAccountConnection(c, account, "", "")
	require.Error(t, err)

	require.Len(t, recorder.requests, 2, "refresh 失败不得重试探活")
	require.Equal(t, webKimiRefreshTokenURL, recorder.requests[1].URL.String())

	body := rec.Body.String()
	require.Contains(t, body, "web login credential is invalid (HTTP 401)")
	require.NotContains(t, body, webProbeRefreshStatusText, "刷新失败不得发续期 status 事件")
	require.NotContains(t, body, `"success":true`)
	require.Zero(t, repo.updatedID, "刷新失败不写库")
}

// 用例 3：zhipu web 探活首发 401 → 静默续期 → 新 cookie 重试 200。
func TestAccountTestService_ZhipuWebProbe401RefreshesAndRetries(t *testing.T) {
	oldCookie := "chatglm_token=OLD-TOKEN; chatglm_refresh_token=OLD-REFRESH; track=abc"
	account := &Account{
		ID:          7003,
		Platform:    PlatformZhipu,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_mode": AccountAccessModeWeb,
			"cookie":      oldCookie,
		},
	}
	repo := &kimiRefreshRepoStub{}
	svc, recorder := newWebRefreshProbeTestService(repo)
	recorder.responses = []*http.Response{
		unauthorizedTextResponse(),
		refreshResponse(http.StatusOK, `{"result":{"access_token":"NEW-TOKEN","refresh_token":"NEW-REFRESH"}}`),
		okSSEResponse(),
	}
	c, rec := newWebRefreshTestContext()

	err := svc.testWebAccountConnection(c, account, "", "")
	require.NoError(t, err)

	require.Len(t, recorder.requests, 3, "首发 + refresh + 重试共三次出站")
	// 首发：旧 cookie 打流式端点。
	require.Equal(t, webZhipuStreamPath, recorder.requests[0].URL.Path)
	require.Contains(t, recorder.requests[0].Header.Get("Cookie"), "chatglm_token=OLD-TOKEN")
	// refresh：与转发链同一端点（base + webZhipuRefreshPath），Bearer 为 refresh token。
	require.Equal(t, DefaultWebZhipuBaseURL+webZhipuRefreshPath, recorder.requests[1].URL.String())
	require.Equal(t, "Bearer OLD-REFRESH", recorder.requests[1].Header.Get("Authorization"))
	// 重试：cookie 就地更新后的新值（与转发链 forwardWebZhipu 同款取法）。
	require.Equal(t, webZhipuStreamPath, recorder.requests[2].URL.Path)
	require.Contains(t, recorder.requests[2].Header.Get("Cookie"), "chatglm_token=NEW-TOKEN")
	require.Equal(t, "Bearer NEW-TOKEN", recorder.requests[2].Header.Get("Authorization"))

	// SSE：续期 status 文案 + 成功收口；且不泄露任何 token/cookie 值。
	body := rec.Body.String()
	require.Contains(t, body, webProbeRefreshStatusText)
	require.Contains(t, body, `"type":"test_complete"`)
	require.Contains(t, body, `"success":true`)
	require.NotContains(t, body, "OLD-TOKEN")
	require.NotContains(t, body, "NEW-TOKEN")
	require.NotContains(t, body, oldCookie)

	// 凭据持久化：cookie 中 chatglm_token/chatglm_refresh_token 就地替换。
	require.Equal(t, account.ID, repo.updatedID)
	require.Contains(t, repo.updatedCreds["cookie"], "chatglm_token=NEW-TOKEN")
	require.Contains(t, repo.updatedCreds["cookie"], "chatglm_refresh_token=NEW-REFRESH")
	require.Contains(t, repo.updatedCreds["cookie"], "track=abc", "其余 cookie 字段保留")
}

// 用例 4：首发 403 → 不调用 refresh 端点（封禁语义不续期），保持既有文案。
func TestAccountTestService_WebProbe403DoesNotAttemptRefresh(t *testing.T) {
	account := &Account{
		ID:          7004,
		Platform:    PlatformKimi,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_mode":   AccountAccessModeWeb,
			"access_token":  "expired-access-token",
			"refresh_token": "valid-refresh-token",
		},
	}
	repo := &kimiRefreshRepoStub{}
	svc, recorder := newWebRefreshProbeTestService(repo)
	recorder.responses = []*http.Response{forbiddenTextResponse()}
	c, rec := newWebRefreshTestContext()

	err := svc.testWebAccountConnection(c, account, "", "")
	require.Error(t, err)

	require.Len(t, recorder.requests, 1, "403 不得触发 refresh 端点调用")
	for _, r := range recorder.requests {
		require.NotEqual(t, webKimiRefreshTokenURL, r.URL.String())
	}
	require.Contains(t, rec.Body.String(), "web login credential is invalid (HTTP 403)")
	require.NotContains(t, rec.Body.String(), webProbeRefreshStatusText)
	require.Zero(t, repo.updatedID)
}
