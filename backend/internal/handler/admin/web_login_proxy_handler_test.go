package admin_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// overrideTransport 把硬编码上游 host（chatglm.cn / chat.deepseek.com / www.kimi.com /
// platform.deepseek.com）的连接重定向到 mock 服务器，并跳过 TLS 校验。
// 仅对上游 host 生效，客户端→反代的连接保持正常拨号。返回还原函数。
func overrideTransport(t *testing.T, mock *httptest.Server) func() {
	t.Helper()
	orig := http.DefaultTransport
	u, err := url.Parse(mock.URL)
	require.NoError(t, err)
	dialer := &net.Dialer{}
	upstreamHosts := map[string]bool{
		"chatglm.cn":            true,
		"chat.deepseek.com":     true,
		"www.kimi.com":          true,
		"platform.deepseek.com": true,
	}
	http.DefaultTransport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if host, _, splitErr := net.SplitHostPort(addr); splitErr == nil && upstreamHosts[host] {
				return dialer.DialContext(ctx, network, u.Host)
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}
	return func() { http.DefaultTransport = orig }
}

// newProxyServer 创建真实的 httptest.Server 承载反代路由（ReverseProxy 需要真实
// http.ResponseWriter，httptest.ResponseRecorder 不实现 CloseNotifier）。
func newProxyServer(h *admin.WebLoginProxyHandler) *httptest.Server {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Any("/api/v1/web-login-proxy/:token/*path", h.Proxy)
	return httptest.NewServer(r)
}

// noFollowClient 不自动跟随重定向，便于直接检查 3xx 的 Location。
func noFollowClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// TestProxy_HeaderStrippedAndCookieCaptured 验证：安全响应头剥离、Set-Cookie 重写、
// 命中关键 cookie 后 GetCapture 返回 cookie、且日志不出现 cookie 值。
func TestProxy_HeaderStrippedAndCookieCaptured(t *testing.T) {
	var upstreamPath string
	mock := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		w.Header().Set("Set-Cookie", "chatglm_token=abc123; Domain=chatglm.cn; Path=/; Secure; HttpOnly; SameSite=Strict")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer mock.Close()
	restore := overrideTransport(t, mock)
	defer restore()

	store := service.NewWebLoginCaptureStore()
	h := admin.NewWebLoginProxyHandler(store)
	srv := newProxyServer(h)
	defer srv.Close()

	token, _, err := store.Create("web-zhipu")
	require.NoError(t, err)

	// 捕获日志输出，断言不出现 cookie 值。
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(log.Writer())

	resp, err := http.Get(srv.URL + "/api/v1/web-login-proxy/" + token + "/dashboard")
	require.NoError(t, err)
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "/dashboard", upstreamPath)

	// 安全响应头被剥离。
	require.Empty(t, resp.Header.Get("X-Frame-Options"))
	require.Empty(t, resp.Header.Get("Content-Security-Policy"))
	require.Empty(t, resp.Header.Get("Cross-Origin-Resource-Policy"))

	// Set-Cookie：Domain 被剥离，SameSite 改为 Lax，非 TLS 请求 Secure 被移除。
	setCookie := resp.Header.Get("Set-Cookie")
	require.NotEmpty(t, setCookie)
	require.NotContains(t, setCookie, "Domain=")
	require.Contains(t, setCookie, "SameSite=Lax")
	require.NotContains(t, setCookie, "Secure")
	require.Contains(t, setCookie, "HttpOnly")

	// 捕获命中关键 cookie：GetCapture 应返回 cookie。
	cookie, ok := store.Cookie(token)
	require.True(t, ok)
	require.Contains(t, cookie, "chatglm_token=abc123")

	// 日志不得出现 cookie 名/值。
	require.NotContains(t, logBuf.String(), "chatglm_token")
	require.NotContains(t, logBuf.String(), "abc123")
}

// TestProxy_NoLogOfCookie 单独确认响应日志中不会泄露 cookie（红线）。
func TestProxy_NoLogOfCookie(t *testing.T) {
	mock := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "chatglm_token=secretvalue; Domain=chatglm.cn; Path=/; Secure; HttpOnly")
		w.WriteHeader(http.StatusOK)
	}))
	defer mock.Close()
	restore := overrideTransport(t, mock)
	defer restore()

	store := service.NewWebLoginCaptureStore()
	h := admin.NewWebLoginProxyHandler(store)
	srv := newProxyServer(h)
	defer srv.Close()
	token, _, err := store.Create("web-zhipu")
	require.NoError(t, err)

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(log.Writer())

	resp, err := http.Get(srv.URL + "/api/v1/web-login-proxy/" + token + "/")
	require.NoError(t, err)
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	require.NotContains(t, logBuf.String(), "secretvalue")
	require.NotContains(t, logBuf.String(), "chatglm_token")
}

// TestProxy_HTMLRewrite 验证 text/html 注入 <base href> 并改写白名单绝对 URL。
func TestProxy_HTMLRewrite(t *testing.T) {
	mock := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>x</title></head><body><a href="https://chatglm.cn/foo">l</a></body></html>`))
	}))
	defer mock.Close()
	restore := overrideTransport(t, mock)
	defer restore()

	store := service.NewWebLoginCaptureStore()
	h := admin.NewWebLoginProxyHandler(store)
	srv := newProxyServer(h)
	defer srv.Close()
	token, _, err := store.Create("web-zhipu")
	require.NoError(t, err)

	resp, err := http.Get(srv.URL + "/api/v1/web-login-proxy/" + token + "/")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, string(body), `<base href="/api/v1/web-login-proxy/`+token+`/">`)
	require.Contains(t, string(body), `href="/api/v1/web-login-proxy/`+token+`/foo"`)
}

// TestProxy_UnknownTokenGone 验证未知 token 返回 410。
func TestProxy_UnknownTokenGone(t *testing.T) {
	store := service.NewWebLoginCaptureStore()
	h := admin.NewWebLoginProxyHandler(store)
	srv := newProxyServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/web-login-proxy/does-not-exist/dashboard")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusGone, resp.StatusCode)
}

// TestProxy_MethodNotAllowed 验证非白名单方法返回 405。
func TestProxy_MethodNotAllowed(t *testing.T) {
	store := service.NewWebLoginCaptureStore()
	h := admin.NewWebLoginProxyHandler(store)
	srv := newProxyServer(h)
	defer srv.Close()
	token, _, err := store.Create("web-zhipu")
	require.NoError(t, err)

	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/v1/web-login-proxy/"+token+"/dashboard", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

// TestProxy_RedirectRewrite 验证 3xx Location 在白名单 host 时被改写为代理路径。
func TestProxy_RedirectRewrite(t *testing.T) {
	mock := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://chatglm.cn/next")
		w.WriteHeader(http.StatusFound)
	}))
	defer mock.Close()
	restore := overrideTransport(t, mock)
	defer restore()

	store := service.NewWebLoginCaptureStore()
	h := admin.NewWebLoginProxyHandler(store)
	srv := newProxyServer(h)
	defer srv.Close()
	token, _, err := store.Create("web-zhipu")
	require.NoError(t, err)

	resp, err := noFollowClient().Get(srv.URL + "/api/v1/web-login-proxy/" + token + "/start")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusFound, resp.StatusCode)
	loc := resp.Header.Get("Location")
	require.Equal(t, "/api/v1/web-login-proxy/"+token+"/next", loc)
}

// TestProxy_RedirectNonAllowlistBlocked 验证指向非白名单 host 的重定向被拦截为 502。
func TestProxy_RedirectNonAllowlistBlocked(t *testing.T) {
	mock := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://evil.example.com/steal")
		w.WriteHeader(http.StatusFound)
	}))
	defer mock.Close()
	restore := overrideTransport(t, mock)
	defer restore()

	store := service.NewWebLoginCaptureStore()
	h := admin.NewWebLoginProxyHandler(store)
	srv := newProxyServer(h)
	defer srv.Close()
	token, _, err := store.Create("web-zhipu")
	require.NoError(t, err)

	resp, err := noFollowClient().Get(srv.URL + "/api/v1/web-login-proxy/" + token + "/start")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	require.Equal(t, http.StatusBadGateway, resp.StatusCode)
	// 响应体不得泄露目标 URL。
	require.NotContains(t, string(body), "evil.example.com")
}

// TestCreateSession_Validation 验证 CreateSession 的平台校验与返回结构。
func TestCreateSession_Validation(t *testing.T) {
	store := service.NewWebLoginCaptureStore()
	h := admin.NewWebLoginProxyHandler(store)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/v1/admin/web-login-proxy/sessions", h.CreateSession)

	// 空平台 → 400。
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/admin/web-login-proxy/sessions", strings.NewReader(`{}`)))
	require.Equal(t, http.StatusBadRequest, w.Code)

	// 不支持的平台 → 400。
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/admin/web-login-proxy/sessions", strings.NewReader(`{"platform":"web-x"}`)))
	require.Equal(t, http.StatusBadRequest, w.Code)

	// 合法平台 → 200，返回 token/url/expires_at。
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/admin/web-login-proxy/sessions", strings.NewReader(`{"platform":"web-zhipu"}`)))
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Data struct {
			Token     string `json:"token"`
			URL       string `json:"url"`
			ExpiresAt string `json:"expires_at"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.Data.Token)
	require.Equal(t, "/api/v1/web-login-proxy/"+resp.Data.Token+"/", resp.Data.URL)
	require.NotEmpty(t, resp.Data.ExpiresAt)
}

// TestGetCapture_AfterCapture 验证 GetCapture 返回捕获到的 cookie，且读后保留。
func TestGetCapture_AfterCapture(t *testing.T) {
	store := service.NewWebLoginCaptureStore()
	h := admin.NewWebLoginProxyHandler(store)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/v1/admin/web-login-proxy/sessions/:token/capture", h.GetCapture)

	token, _, err := store.Create("web-zhipu")
	require.NoError(t, err)

	// 捕获前：captured=false。
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/web-login-proxy/sessions/"+token+"/capture", nil))
	require.Equal(t, http.StatusOK, w.Code)
	var before struct {
		Data struct {
			Captured  bool   `json:"captured"`
			Cookie    string `json:"cookie"`
			ExpiresAt string `json:"expires_at"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &before))
	require.False(t, before.Data.Captured)
	require.Empty(t, before.Data.Cookie)

	// 写入 cookie 后再次查询：captured=true，且读后保留。
	store.SetCookie(token, "chatglm_token=abc")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/web-login-proxy/sessions/"+token+"/capture", nil))
	require.Equal(t, http.StatusOK, w.Code)
	var after struct {
		Data struct {
			Captured bool   `json:"captured"`
			Cookie   string `json:"cookie"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &after))
	require.True(t, after.Data.Captured)
	require.Equal(t, "chatglm_token=abc", after.Data.Cookie)

	// 读后保留：再次查询仍是 captured。
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/web-login-proxy/sessions/"+token+"/capture", nil))
	require.Equal(t, http.StatusOK, w.Code)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &after))
	require.True(t, after.Data.Captured)
}

// TestKimi_NoCapture 验证 kimi（关键 cookie 名为空）不会触发捕获。
func TestKimi_NoCapture(t *testing.T) {
	mock := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "kimi_some=val; Domain=www.kimi.com; Path=/; Secure")
		w.WriteHeader(http.StatusOK)
	}))
	defer mock.Close()
	restore := overrideTransport(t, mock)
	defer restore()

	store := service.NewWebLoginCaptureStore()
	h := admin.NewWebLoginProxyHandler(store)
	srv := newProxyServer(h)
	defer srv.Close()
	token, _, err := store.Create("web-kimi")
	require.NoError(t, err)

	resp, err := http.Get(srv.URL + "/api/v1/web-login-proxy/" + token + "/")
	require.NoError(t, err)
	defer resp.Body.Close()

	// kimi 关键 cookie 名为空 → 不捕获。
	_, ok := store.Cookie(token)
	require.False(t, ok)
}
