package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Task B：管理端入口按接入模式放行 web 凭证预校验（官方平台 + access_mode=web 组合生效，
// 旧 web-* 平台值保持兼容，方案 §5.5）。

func TestTaskBValidateWebCredentials_AcceptsOfficialPlatformWebMode(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 官方 zhipu + access_mode=web + cookie：放行。
	{
		h := &AccountHandler{}
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/validate-web-credentials",
			strings.NewReader(`{"platform":"zhipu","credentials":{"access_mode":"web","cookie":"c"}}`))
		h.ValidateWebCredentials(c)
		require.Equal(t, http.StatusOK, rec.Code, "official zhipu + web mode accepted")
	}

	// 官方 kimi + access_mode=web + access_token：放行。
	{
		h := &AccountHandler{}
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/validate-web-credentials",
			strings.NewReader(`{"platform":"kimi","credentials":{"access_mode":"web","access_token":"at"}}`))
		h.ValidateWebCredentials(c)
		require.Equal(t, http.StatusOK, rec.Code, "official kimi + web mode accepted")
	}

	// 旧 web-zhipu 平台值仍兼容。
	{
		h := &AccountHandler{}
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/validate-web-credentials",
			strings.NewReader(`{"platform":"web-zhipu","credentials":{"cookie":"c"}}`))
		h.ValidateWebCredentials(c)
		require.Equal(t, http.StatusOK, rec.Code, "legacy web-zhipu still accepted")
	}

	// 官方 zhipu 无 access_mode=web（api 账号）走 /v1/models 而非 web 预校验：拒绝（非 web 平台）。
	{
		h := &AccountHandler{}
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/validate-web-credentials",
			strings.NewReader(`{"platform":"zhipu","credentials":{"api_key":"sk-x"}}`))
		h.ValidateWebCredentials(c)
		require.Equal(t, http.StatusBadRequest, rec.Code, "plain api zhipu rejected as non-web")
	}

	// 官方 zhipu + access_mode=web 但缺 cookie：准入校验失败（response.ErrorFrom 对
	// 普通错误返回 500，属该端点既有契约，非本次改动范围）。
	{
		h := &AccountHandler{}
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/validate-web-credentials",
			strings.NewReader(`{"platform":"zhipu","credentials":{"access_mode":"web"}}`))
		h.ValidateWebCredentials(c)
		require.Equal(t, http.StatusInternalServerError, rec.Code, "zhipu+web without cookie rejected")
	}
}

// 确认 service 层 ValidateWebAccountCredential 对官方平台 + web 组合的导出行为一致（端到端入口复用）。
func TestTaskBValidateWebAccountCredentialExported(t *testing.T) {
	require.NoError(t, service.ValidateWebAccountCredential(service.PlatformZhipu, service.AccountTypeAPIKey,
		map[string]any{"access_mode": service.AccountAccessModeWeb, "cookie": "c"}))
	require.Error(t, service.ValidateWebAccountCredential(service.PlatformZhipu, service.AccountTypeAPIKey,
		map[string]any{"access_mode": service.AccountAccessModeWeb}))
}
