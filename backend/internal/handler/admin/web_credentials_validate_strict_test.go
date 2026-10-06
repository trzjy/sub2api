package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 任务 A：ValidateWebCredentials 严格语义反例回归。
// 严格语义：当且仅当 官方平台（deepseek/zhipu/kimi）且 credentials.access_mode=="web"
// （trim 后）才放行；其余一律 400（含普通 API 账号、显式 api 模式、非官方平台 even with web）。
// 构造方式照抄 taskb_account_handler_test.go 同路由用例（httptest + gin TestContext，
// 直接调用 handler 方法，无依赖注入需求）。

func TestTaskAValidateWebCredentials_StrictReject(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 1. 普通 API 账号（官方 zhipu，无 access_mode）→ 400
	{
		h := &AccountHandler{}
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/validate-web-credentials",
			strings.NewReader(`{"platform":"zhipu","credentials":{"api_key":"sk-x"}}`))
		h.ValidateWebCredentials(c)
		require.Equal(t, http.StatusBadRequest, rec.Code, "plain api zhipu (no access_mode) must be rejected")
	}

	// 2. 官方 deepseek 显式 access_mode=api → 400
	{
		h := &AccountHandler{}
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/validate-web-credentials",
			strings.NewReader(`{"platform":"deepseek","credentials":{"api_key":"sk-x","access_mode":"api"}}`))
		h.ValidateWebCredentials(c)
		require.Equal(t, http.StatusBadRequest, rec.Code, "official deepseek with explicit access_mode=api must be rejected")
	}

	// 3. 官方 zhipu + access_mode=web + cookie → 200（合法 web 账号放行）
	{
		h := &AccountHandler{}
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/validate-web-credentials",
			strings.NewReader(`{"platform":"zhipu","credentials":{"access_mode":"web","cookie":"..."}}`))
		h.ValidateWebCredentials(c)
		require.Equal(t, http.StatusOK, rec.Code, "official zhipu + access_mode=web must be accepted")
	}

	// 4. 非官方平台 openai 即使 access_mode=web 也拒绝 → 400
	{
		h := &AccountHandler{}
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/validate-web-credentials",
			strings.NewReader(`{"platform":"openai","credentials":{"access_mode":"web","cookie":"..."}}`))
		h.ValidateWebCredentials(c)
		require.Equal(t, http.StatusBadRequest, rec.Code, "non-official openai even with access_mode=web must be rejected")
	}
}
