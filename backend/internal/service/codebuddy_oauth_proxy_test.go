package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// codeBuddyProxyRepoStub 仅实现 GetByID，其余方法由内嵌接口兜底（调用即 panic）。
type codeBuddyProxyRepoStub struct {
	ProxyRepository
	proxy *Proxy
	err   error
	calls int
}

func (r *codeBuddyProxyRepoStub) GetByID(context.Context, int64) (*Proxy, error) {
	r.calls++
	return r.proxy, r.err
}

func mustProxyURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

// TestNewCodeBuddyHTTPClient 覆盖 client 构造：无代理保持直连（Transport 为 nil，
// 与改造前一致），有代理挂上指向代理的 Transport，非法 URL 直接报错。
func TestNewCodeBuddyHTTPClient(t *testing.T) {
	direct, err := newCodeBuddyHTTPClient("")
	require.NoError(t, err)
	require.Nil(t, direct.Transport, "无代理时不应挂 Transport（保持既有行为）")

	spaced, err := newCodeBuddyHTTPClient("   ")
	require.NoError(t, err)
	require.Nil(t, spaced.Transport, "空白代理等同无代理")

	proxied, err := newCodeBuddyHTTPClient("http://proxy.example.com:8080")
	require.NoError(t, err)
	tr, ok := proxied.Transport.(*http.Transport)
	require.True(t, ok, "有代理时必须挂 *http.Transport")
	u, err := tr.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "copilot.tencent.com"}})
	require.NoError(t, err)
	require.Equal(t, "http://proxy.example.com:8080", u.String())

	_, err = newCodeBuddyHTTPClient("://invalid")
	require.Error(t, err, "非法代理 URL 必须报错")
}

// TestCodeBuddyOAuthService_ClientForProxyID 断言请求源与账号代理一致：
// 无代理复用共享直连 client；指定代理时构造独立 client 且 Transport.Proxy 指向该代理。
func TestCodeBuddyOAuthService_ClientForProxyID(t *testing.T) {
	repo := &codeBuddyProxyRepoStub{proxy: &Proxy{ID: 7, Protocol: "http", Host: "proxy.local", Port: 3128}}
	svc := NewCodeBuddyOAuthService(repo)

	direct, err := svc.clientForProxyID(context.Background(), nil)
	require.NoError(t, err)
	require.Same(t, svc.httpClient, direct, "无代理时必须复用共享直连 client")

	proxyID := int64(7)
	proxied, err := svc.clientForProxyID(context.Background(), &proxyID)
	require.NoError(t, err)
	require.NotSame(t, svc.httpClient, proxied)
	tr, ok := proxied.Transport.(*http.Transport)
	require.True(t, ok)
	u, err := tr.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "copilot.tencent.com"}})
	require.NoError(t, err)
	require.Equal(t, "http://proxy.local:3128", u.String(), "请求源必须与账号代理一致")
	require.Equal(t, 1, repo.calls)
}

// TestCodeBuddyOAuthService_AccountClientPrefersPreloadedProxy 断言账号刷新优先用
// 调度快照已加载的 Proxy，避免多余仓库查询；无 Proxy 时才按 ProxyID 查仓库。
func TestCodeBuddyOAuthService_AccountClientPrefersPreloadedProxy(t *testing.T) {
	repo := &codeBuddyProxyRepoStub{err: ErrProxyNotFound}
	svc := NewCodeBuddyOAuthService(repo)

	account := &Account{
		Platform: PlatformCodeBuddy,
		Type:     AccountTypeOAuth,
		Proxy:    &Proxy{ID: 9, Protocol: "http", Host: "preloaded.local", Port: 8080},
	}
	client, err := svc.accountClient(context.Background(), account)
	require.NoError(t, err)
	tr, ok := client.Transport.(*http.Transport)
	require.True(t, ok)
	u, err := tr.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "copilot.tencent.com"}})
	require.NoError(t, err)
	require.Equal(t, "http://preloaded.local:8080", u.String())
	require.Zero(t, repo.calls, "已加载 Proxy 时不应再查仓库")
}

// TestCodeBuddyOAuthService_ResolveProxyURLErrors 覆盖代理解析失败的可读错误，
// 避免代理配置错误被静默降级为直连（导致登录 IP 与使用 IP 不一致）。
func TestCodeBuddyOAuthService_ResolveProxyURLErrors(t *testing.T) {
	proxyID := int64(3)
	ctx := context.Background()

	// 无代理仓库
	_, err := NewCodeBuddyOAuthService(nil).resolveProxyURL(ctx, &proxyID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "proxy repository")

	// 仓库查不到代理
	_, err = NewCodeBuddyOAuthService(&codeBuddyProxyRepoStub{err: ErrProxyNotFound}).resolveProxyURL(ctx, &proxyID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "proxy")

	// 无代理 → 空串，不报错
	resolvedURL, err := NewCodeBuddyOAuthService(nil).resolveProxyURL(ctx, nil)
	require.NoError(t, err)
	require.Empty(t, resolvedURL)
}

// newCodeBuddyProxyCaptureServer 构造一个记录是否收到 CONNECT 的代理服务器，
// 对 CONNECT 返回 502 让客户端快速失败（不触达真实上游）。
func newCodeBuddyProxyCaptureServer(t *testing.T, connectSeen *atomic.Bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			connectSeen.Store(true)
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		http.Error(w, "unexpected non-CONNECT request", http.StatusBadGateway)
	}))
}

func proxyFromServer(t *testing.T, srv *httptest.Server) *Proxy {
	t.Helper()
	u := mustProxyURL(t, srv.URL)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	return &Proxy{ID: 7, Protocol: "http", Host: u.Hostname(), Port: port}
}

// TestCodeBuddyOAuthService_LoginAndPollGoThroughAccountProxy 端到端断言登录与 poll
// 都从账号代理出站：代理服务器收到 CONNECT 即证明请求源与账号代理一致。
func TestCodeBuddyOAuthService_LoginAndPollGoThroughAccountProxy(t *testing.T) {
	ctx := context.Background()

	t.Run("auth-url", func(t *testing.T) {
		var connectSeen atomic.Bool
		proxySrv := newCodeBuddyProxyCaptureServer(t, &connectSeen)
		defer proxySrv.Close()

		repo := &codeBuddyProxyRepoStub{proxy: proxyFromServer(t, proxySrv)}
		svc := NewCodeBuddyOAuthService(repo)
		proxyID := int64(7)

		_, err := svc.GenerateAuthURL(ctx, &proxyID)
		require.Error(t, err)
		require.True(t, connectSeen.Load(), "生成授权链接必须经账号代理出站")
	})

	t.Run("poll", func(t *testing.T) {
		var connectSeen atomic.Bool
		proxySrv := newCodeBuddyProxyCaptureServer(t, &connectSeen)
		defer proxySrv.Close()

		repo := &codeBuddyProxyRepoStub{proxy: proxyFromServer(t, proxySrv)}
		svc := NewCodeBuddyOAuthService(repo)
		proxyID := int64(7)

		_, err := svc.PollToken(ctx, "st-123", &proxyID)
		require.Error(t, err)
		require.True(t, connectSeen.Load(), "poll 必须与登录同一账号代理出站")
	})

	t.Run("refresh-account", func(t *testing.T) {
		var connectSeen atomic.Bool
		proxySrv := newCodeBuddyProxyCaptureServer(t, &connectSeen)
		defer proxySrv.Close()

		repo := &codeBuddyProxyRepoStub{}
		svc := NewCodeBuddyOAuthService(repo)
		account := &Account{
			Platform: PlatformCodeBuddy,
			Type:     AccountTypeOAuth,
			Proxy:    proxyFromServer(t, proxySrv),
			Credentials: map[string]any{
				"refresh_token": "rt-1",
				"uid":           "u-1",
			},
		}

		_, err := svc.RefreshAccountToken(ctx, account)
		require.Error(t, err)
		require.True(t, connectSeen.Load(), "账号 token 刷新必须经账号代理出站")
	})
}

// TestCodeBuddyOAuthService_NoProxyKeepsDirectBehaviour 无代理账号不受改造影响：
// 仍复用共享直连 client，不产生代理 Transport。
func TestCodeBuddyOAuthService_NoProxyKeepsDirectBehaviour(t *testing.T) {
	svc := NewCodeBuddyOAuthService(&codeBuddyProxyRepoStub{})
	account := &Account{Platform: PlatformCodeBuddy, Type: AccountTypeOAuth}
	client, err := svc.accountClient(context.Background(), account)
	require.NoError(t, err)
	require.Same(t, svc.httpClient, client)
	require.Nil(t, svc.httpClient.Transport)
}
