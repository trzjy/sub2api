//go:build unit

package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// fakeWebAuthHelper 实现 service.LocalCaptchaHelperWebAuth，固定返回 sess-1。
type fakeWebAuthHelper struct{}

func (f *fakeWebAuthHelper) WebAuthStart(context.Context, service.WebAuthStartRequest) (string, error) {
	return "sess-1", nil
}
func (f *fakeWebAuthHelper) WebAuthStatus(context.Context, string) (string, string, error) {
	return "pending", "", nil
}
func (f *fakeWebAuthHelper) WebAuthResult(context.Context, string, string, string) (string, string, error) {
	return "{}", "", nil
}

// decodeStartWebConsoleAuthBody 按 response.Success 的既有包裹结构解包 data。
// 注意：response.Success 的包裹为 {code, message, data}（无 success 布尔字段），
// 「顶层成功」在既有契约中的体现是 code == 0 且 message == "success"。
func decodeStartWebConsoleAuthBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, float64(0), body["code"], "response should flag success: %s", rec.Body.String())
	require.Equal(t, "success", body["message"], "response should flag success: %s", rec.Body.String())
	data, ok := body["data"].(map[string]any)
	require.True(t, ok, "response should wrap payload under data field: %s", rec.Body.String())
	return data
}

// TestStartWebConsoleAuthResponseContract 校验发起交互登录的响应只含 session_id / expires_in。
func TestStartWebConsoleAuthResponseContract(t *testing.T) {
	gin.SetMode(gin.TestMode)

	repo := &fakeAccountRepo{account: &service.Account{
		ID:       9200,
		Platform: service.PlatformDeepseek,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://cp.compshare.cn/v1",
		},
	}}
	authSvc := service.NewWebConsoleAuthService(nil, &fakeWebAuthHelper{}, repo)

	h := NewCNProviderHandler(nil, nil)
	h.SetWebConsoleServices(nil, authSvc)

	r := gin.New()
	r.POST("/accounts/:id/web-console-auth/start", h.StartWebConsoleAuth)

	req := httptest.NewRequest(http.MethodPost, "/accounts/9200/web-console-auth/start", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	data := decodeStartWebConsoleAuthBody(t, rec)

	require.Equal(t, "sess-1", data["session_id"])
	expiresIn, ok := data["expires_in"].(float64)
	require.True(t, ok, "expires_in should be numeric: %s", rec.Body.String())
	require.Greater(t, expiresIn, float64(0))

	for _, leaked := range []string{"Binding", "ExpectedUsername", "AccountID"} {
		require.NotContains(t, data, leaked, "internal field %q must not be exposed", leaked)
	}
}
