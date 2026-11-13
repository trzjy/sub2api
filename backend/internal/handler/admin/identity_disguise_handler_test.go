//go:build unit

package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func setupIdentityDisguiseRouter() (*gin.Engine, *stubAdminService) {
	gin.SetMode(gin.TestMode)
	adminSvc := newStubAdminService()
	handler := NewAccountHandler(adminSvc, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	router := gin.New()
	router.GET("/api/v1/admin/accounts/:id/identity-disguise", handler.GetAccountIdentityDisguise)
	router.PUT("/api/v1/admin/accounts/:id/identity-disguise", handler.UpdateAccountIdentityDisguise)
	return router, adminSvc
}

func getIdentityDisguise(t *testing.T, router *gin.Engine, path string) map[string]any {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code)

	var body struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	return body.Data
}

// 回归：禁词列表为空时，响应里的 forbidden_names 必须是 [] 而不是 null。
//
// Go 的 nil 切片会被序列化成 null，前端按 string[] 使用（join/遍历）会直接抛错，
// 把整个读取判为失败 —— 表现为「加载身份伪装配置失败」，且伪装配置**无法保存**
// （前端读到失败就跳过写回）。
func TestGetAccountIdentityDisguise_ForbiddenNamesIsArrayNotNull(t *testing.T) {
	router, adminSvc := setupIdentityDisguiseRouter()
	adminSvc.identityDisguiseState = &service.IdentityDisguiseState{
		Profile: service.IdentityDisguiseProfileNone,
		// 零值 ForbiddenNames 即 nil —— 这正是线上触发问题的形态。
	}

	data := getIdentityDisguise(t, router, "/api/v1/admin/accounts/12/identity-disguise")

	names, ok := data["forbidden_names"].([]any)
	require.True(t, ok, "forbidden_names 必须是数组，实际为 %v", data["forbidden_names"])
	require.Empty(t, names)
}

// 有禁词时原样返回。
func TestGetAccountIdentityDisguise_ForbiddenNamesRoundTrip(t *testing.T) {
	router, adminSvc := setupIdentityDisguiseRouter()
	adminSvc.identityDisguiseState = &service.IdentityDisguiseState{
		Profile:        service.IdentityDisguiseProfileKimi,
		ForbiddenNames: []string{"kimi-k2", "moonshot"},
	}

	data := getIdentityDisguise(t, router, "/api/v1/admin/accounts/12/identity-disguise")

	names, ok := data["forbidden_names"].([]any)
	require.True(t, ok)
	require.Len(t, names, 2)
	require.Equal(t, "kimi-k2", names[0])
	require.Equal(t, "kimi", data["profile"])
}

// 「自动生成」开关随配置回显（不从前端反推）。
func TestGetAccountIdentityDisguise_AutoDeriveFlag(t *testing.T) {
	router, adminSvc := setupIdentityDisguiseRouter()
	adminSvc.identityDisguiseState = &service.IdentityDisguiseState{
		Profile:             service.IdentityDisguiseProfileKimi,
		ForbiddenNames:      []string{"kimi-k2"},
		AutoDeriveForbidden: true,
	}

	data := getIdentityDisguise(t, router, "/api/v1/admin/accounts/12/identity-disguise")

	require.Equal(t, true, data["auto_derive_forbidden_names"])
}
