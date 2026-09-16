package server_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/server"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newWebLoginProxyHandlers() *handler.Handlers {
	h := admin.NewWebLoginProxyHandler(service.NewWebLoginCaptureStore())
	return &handler.Handlers{Admin: &handler.AdminHandlers{WebLoginProxy: h}}
}

// TestBuildWebLoginProxyEngine_RegistersProxyRoute 验证隔离 engine 仅注册代理路由。
func TestBuildWebLoginProxyEngine_RegistersProxyRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := server.BuildWebLoginProxyEngine(newWebLoginProxyHandlers())

	found := false
	for _, rt := range r.Routes() {
		if rt.Path == "/api/v1/web-login-proxy/:token/*path" {
			found = true
		}
	}
	require.True(t, found, "isolated engine must register the proxy route")
}

// TestBuildWebLoginProxyEngine_ReachableReturns410ForUnknownToken 验证隔离 engine
// 路由可达且复用同一 handler（未知 token 返回 410，不依赖上游）。
func TestBuildWebLoginProxyEngine_ReachableReturns410ForUnknownToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := server.BuildWebLoginProxyEngine(newWebLoginProxyHandlers())

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/web-login-proxy/nope/dashboard", nil))
	require.Equal(t, http.StatusGone, w.Code)
}

// TestProvideWebLoginProxyServer_DisabledWhenAddrEmpty 验证 addr 为空时不启用。
func TestProvideWebLoginProxyServer_DisabledWhenAddrEmpty(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.WebLoginProxyAddr = ""
	srv := server.ProvideWebLoginProxyServer(cfg, newWebLoginProxyHandlers())
	require.Nil(t, srv)
}

// TestProvideWebLoginProxyServer_EnabledWithAddr 验证非空 addr 生成可用 server。
func TestProvideWebLoginProxyServer_EnabledWithAddr(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.WebLoginProxyAddr = "127.0.0.1:3400"
	srv := server.ProvideWebLoginProxyServer(cfg, newWebLoginProxyHandlers())
	require.NotNil(t, srv)
	require.True(t, srv.Enabled())
	require.Equal(t, "127.0.0.1:3400", srv.Addr())
}
