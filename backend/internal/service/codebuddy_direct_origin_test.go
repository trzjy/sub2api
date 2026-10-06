package service

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestCodeBuddySiteEgressHosts(t *testing.T) {
	cn := CodeBuddySiteEgressHosts("cn")
	require.Contains(t, cn, "copilot.tencent.com")
	require.Contains(t, cn, "www.codebuddy.cn")

	intl := CodeBuddySiteEgressHosts("intl")
	require.Contains(t, intl, "www.codebuddy.ai")
}

func TestCodeBuddyDirectOriginActive(t *testing.T) {
	require.False(t, CodeBuddyDirectOriginActive(nil))
	require.False(t, CodeBuddyDirectOriginActive(map[string]config.CodeBuddyDirectOriginConfig{
		"cn": {Enabled: false, IP: "1.2.3.4", Host: "copilot.tencent.com"},
	}))
	require.True(t, CodeBuddyDirectOriginActive(map[string]config.CodeBuddyDirectOriginConfig{
		"cn": {Enabled: true, IP: "1.2.3.4", Host: "copilot.tencent.com"},
	}))
}

func TestCodeBuddyDirectOriginResolve(t *testing.T) {
	direct := map[string]config.CodeBuddyDirectOriginConfig{
		"cn": {Enabled: true, IP: "43.159.104.94", Host: "copilot.tencent.com", Port: 443},
	}
	addr, ok := CodeBuddyDirectOriginResolve("copilot.tencent.com", direct)
	require.True(t, ok)
	require.Equal(t, "43.159.104.94:443", addr)

	// billing 域名（www.codebuddy.cn）同样命中
	addr, ok = CodeBuddyDirectOriginResolve("www.codebuddy.cn", direct)
	require.True(t, ok)
	require.Equal(t, "43.159.104.94:443", addr)

	// 非 CodeBuddy 域名不命中
	_, ok = CodeBuddyDirectOriginResolve("api.openai.com", direct)
	require.False(t, ok)

	// 关闭状态不命中
	_, ok = CodeBuddyDirectOriginResolve("copilot.tencent.com", map[string]config.CodeBuddyDirectOriginConfig{
		"cn": {Enabled: false, IP: "1.2.3.4", Host: "copilot.tencent.com"},
	})
	require.False(t, ok)
}

// TestWrapDirectOriginDialContext 是硬性"默认关闭=零行为变化"对照：
// 非 CodeBuddy 主机或关闭状态下，base 收到的 addr 与原 addr 逐字节一致；
// 仅 CodeBuddy 主机且启用时被重定向到钉点 IP:port。
func TestWrapDirectOriginDialContext(t *testing.T) {
	direct := map[string]config.CodeBuddyDirectOriginConfig{
		"cn": {Enabled: true, IP: "43.159.104.94", Host: "copilot.tencent.com", Port: 443},
	}
	var gotAddr string
	base := func(ctx context.Context, network, addr string) (net.Conn, error) {
		gotAddr = addr
		return nil, errors.New("fake dial")
	}
	wrapped := WrapDirectOriginDialContext(base, direct)

	// CodeBuddy 主机 → 重定向到钉点 IP
	_, _ = wrapped(context.Background(), "tcp", "copilot.tencent.com:443")
	require.Equal(t, "43.159.104.94:443", gotAddr)

	// 非 CodeBuddy 主机 → 原 addr，逐字节不变
	_, _ = wrapped(context.Background(), "tcp", "api.openai.com:443")
	require.Equal(t, "api.openai.com:443", gotAddr)

	// 关闭状态 → 原 addr 不变（零行为变化）
	disabled := map[string]config.CodeBuddyDirectOriginConfig{
		"cn": {Enabled: false, IP: "1.2.3.4", Host: "copilot.tencent.com"},
	}
	wrappedDisabled := WrapDirectOriginDialContext(base, disabled)
	_, _ = wrappedDisabled(context.Background(), "tcp", "copilot.tencent.com:443")
	require.Equal(t, "copilot.tencent.com:443", gotAddr)
}

// TestWrapDirectOriginDialContextPreservesHostHeader 是端到端对照：
// 请求发往 copilot.tencent.com，TCP 被重定向到本地 server（证明重定向生效），
// 而服务端收到的 Host 头仍为原域名（证明未把 Host 写成 IP）。
func TestWrapDirectOriginDialContextPreservesHostHeader(t *testing.T) {
	var gotHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	srvURL, err := url.Parse(srv.URL)
	require.NoError(t, err)
	port := srvURL.Port()
	require.NotEmpty(t, port)

	direct := map[string]config.CodeBuddyDirectOriginConfig{
		"cn": {Enabled: true, IP: "127.0.0.1", Host: "copilot.tencent.com", Port: atoi(port)},
	}
	transport := &http.Transport{
		DialContext: WrapDirectOriginDialContext((&net.Dialer{}).DialContext, direct),
	}
	client := &http.Client{Transport: transport}

	req, err := http.NewRequest(http.MethodGet, "http://copilot.tencent.com/ping", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	// Host 头保持原域名，未被改写成 IP
	require.Equal(t, "copilot.tencent.com", gotHost)
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
