package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 伪装配置（identity disguise）路由的注册自检。
//
// 关键风险：gin v1.9 的路由树不允许静态段与 :id 参数同级（注册即 panic）。
// 模板目录挂在 admin 根的独立前缀 /identity-disguise，账号级读写挂在
// /accounts/:id/identity-disguise —— 两处必须都注册成功且路径不冲突。
//
// 本测试在**注册阶段**就捕获冲突（panic），不需要真发请求。

func TestIdentityDisguiseRoutesRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{
		Account: &adminhandler.AccountHandler{},
	}}
	adminAuth := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) { c.Next() })
	auditLog := servermiddleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() })
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() })

	// 注册即校验：路由冲突会在这里 panic（gin 的行为），测试直接失败。
	require.NotPanics(t, func() {
		RegisterAdminRoutes(router.Group("/api/v1"), handlers, adminAuth, auditLog, stepUp, nil, nil)
	})

	routes := router.Routes()
	registered := make(map[string]bool, len(routes))
	for _, r := range routes {
		registered[r.Method+" "+r.Path] = true
	}

	require.True(t, registered["GET /api/v1/admin/identity-disguise/profiles"],
		"模板目录接口未注册")
	require.True(t, registered["GET /api/v1/admin/accounts/:id/identity-disguise"],
		"读取伪装配置接口未注册")
	require.True(t, registered["PUT /api/v1/admin/accounts/:id/identity-disguise"],
		"写入伪装配置接口未注册")
}

// 路由冲突的回归护栏：若日后有人把模板目录挪进 /accounts 组下，
// 与 /accounts/:id/... 同级，gin 注册时会 panic —— 本测试必须在注册阶段就失败，
// 而不是等到服务启动才崩。
func TestIdentityDisguiseProfilesRouteNotUnderAccountsGroup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{
		Account: &adminhandler.AccountHandler{},
	}}
	adminAuth := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) { c.Next() })
	auditLog := servermiddleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() })
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() })

	require.NotPanics(t, func() {
		RegisterAdminRoutes(router.Group("/api/v1"), handlers, adminAuth, auditLog, stepUp, nil, nil)
	})

	// /accounts 组下只允许 :id 形态的子路径；出现静态段即意味着将来会与
	// /accounts/:id/... 撞车（gin 会 panic）。
	for _, r := range router.Routes() {
		if r.Path == "/api/v1/admin/accounts/identity-disguise/profiles" {
			t.Fatalf("模板目录接口不得挂在 /accounts 静态段下（与 /accounts/:id 同级会 panic）")
		}
	}
}

// 专用接口必须走 admin 鉴权链（不能裸奔）。
func TestIdentityDisguiseRoutesRequireAdminAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{
		Account: &adminhandler.AccountHandler{},
	}}
	adminAuth := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) {
		servermiddleware.AbortWithError(c, http.StatusUnauthorized, "UNAUTHORIZED", "Authorization required")
	})
	auditLog := servermiddleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() })
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() })
	RegisterAdminRoutes(router.Group("/api/v1"), handlers, adminAuth, auditLog, stepUp, nil, nil)

	for _, tc := range []struct {
		method string
		path   string
	}{
		{"GET", "/api/v1/admin/identity-disguise/profiles"},
		{"GET", "/api/v1/admin/accounts/1/identity-disguise"},
		{"PUT", "/api/v1/admin/accounts/1/identity-disguise"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		require.Equal(t, http.StatusUnauthorized, rec.Code,
			"%s %s 必须要求 admin 鉴权（实际 %d）", tc.method, tc.path, rec.Code)
	}
}
