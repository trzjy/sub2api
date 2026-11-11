package server_test

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/server/routes"

	"github.com/gin-gonic/gin"
)

// TestAdminQuotaKeyRoutes 断言激活码状态切换路由 POST /api/v1/admin/quota_keys/:id/status
// 已在 quota_keys 分组内注册（派发单 QKG-B7）。仅做最小断言：构建路由树并查找目标端点。
//
// 其余中间件 / 服务在注册期仅被转换为闭包持有，nil 不会触发解引用，因此全部传 nil 即可。
func TestAdminQuotaKeyRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &handler.Handlers{
		Admin: &handler.AdminHandlers{
			QuotaKey: &adminhandler.QuotaKeyHandler{},
		},
	}

	r := gin.New()
	v1 := r.Group("/api/v1")
	routes.RegisterAdminRoutes(v1, h, nil, nil, nil, nil, nil)

	const wantMethod = "POST"
	const wantPath = "/api/v1/admin/quota_keys/:id/status"

	found := false
	for _, rt := range r.Routes() {
		if rt.Method == wantMethod && rt.Path == wantPath {
			found = true
			break
		}
	}
	if !found {
		var got []string
		const prefix = "/api/v1/admin/quota_keys"
		for _, rt := range r.Routes() {
			if rt.Path == prefix || len(rt.Path) > len(prefix) && rt.Path[:len(prefix)] == prefix {
				got = append(got, rt.Method+" "+rt.Path)
			}
		}
		t.Fatalf("route %s %s not registered; quota_keys routes found: %v", wantMethod, wantPath, got)
	}
}
