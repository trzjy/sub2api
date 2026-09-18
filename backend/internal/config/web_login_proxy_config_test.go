package config_test

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// TestServerConfig_WebLoginProxyPublicOrigin 验证隔离 origin 公开地址契约：
// 仅接受显式 HTTPS origin（fail-closed）；HTTP、空值、非法 host 一律返回空；
// 同源回退已禁止，不再从监听地址推导。
func TestServerConfig_WebLoginProxyPublicOrigin(t *testing.T) {
	// 显式 HTTPS origin 原样返回（已 TrimSpace）。
	require.Equal(t, "https://wlp.example.com",
		(&config.ServerConfig{WebLoginProxyOrigin: "https://wlp.example.com"}).WebLoginProxyPublicOrigin())
	require.Equal(t, "https://wlp.example.com:8443",
		(&config.ServerConfig{WebLoginProxyOrigin: "  https://wlp.example.com:8443  "}).WebLoginProxyPublicOrigin())

	// 显式 HTTP origin 一律 fail-closed 返回空（禁止明文登录代理流量）。
	require.Equal(t, "", (&config.ServerConfig{WebLoginProxyOrigin: "http://proxy.example.com:3400"}).WebLoginProxyPublicOrigin())
	// 未知 scheme 同样拒绝。
	require.Equal(t, "", (&config.ServerConfig{WebLoginProxyOrigin: "ftp://proxy.example.com"}).WebLoginProxyPublicOrigin())
	// 非法 host（无法解析 / 缺 host）拒绝。
	require.Equal(t, "", (&config.ServerConfig{WebLoginProxyOrigin: "https://"}).WebLoginProxyPublicOrigin())
	require.Equal(t, "", (&config.ServerConfig{WebLoginProxyOrigin: "https://exa mple.com"}).WebLoginProxyPublicOrigin())
	require.Equal(t, "", (&config.ServerConfig{WebLoginProxyOrigin: "://not-a-url"}).WebLoginProxyPublicOrigin())

	// origin 为空 → 一律返回空（不再从 Addr 推导、不回退同源），无论 Addr 取值。
	require.Equal(t, "", (&config.ServerConfig{}).WebLoginProxyPublicOrigin())
	// 显式具体 host 也不再推导（同源回退已禁止）。
	require.Equal(t, "", (&config.ServerConfig{WebLoginProxyAddr: "127.0.0.1:3400"}).WebLoginProxyPublicOrigin())
	// 通配 host 同样不再推导。
	require.Equal(t, "", (&config.ServerConfig{WebLoginProxyAddr: ":3400"}).WebLoginProxyPublicOrigin())
	require.Equal(t, "", (&config.ServerConfig{WebLoginProxyAddr: "0.0.0.0:3400"}).WebLoginProxyPublicOrigin())
	// 缺省端口 addr 也不再推导。
	require.Equal(t, "", (&config.ServerConfig{WebLoginProxyAddr: "10.0.0.5"}).WebLoginProxyPublicOrigin())
	// origin 为空串（含空白）视为未配置。
	require.Equal(t, "", (&config.ServerConfig{WebLoginProxyOrigin: "   "}).WebLoginProxyPublicOrigin())
}

// TestServerConfig_WebLoginProxyPublicOrigin_StrictPureOrigin 验证严格纯 origin 契约：
// 携带 path/query/fragment/userinfo 的非纯 origin 一律 fail-closed 返回空——前端以
// proxyUrl = proxyOrigin + session.url 直接拼接，path/query/fragment 破坏拼接契约，
// userinfo（user:pass@host）可能经 public settings 泄漏。纯 origin（含端口）正常返回。
func TestServerConfig_WebLoginProxyPublicOrigin_StrictPureOrigin(t *testing.T) {
	// 携带 path（非 "/"）→ 拒绝。
	require.Equal(t, "", (&config.ServerConfig{WebLoginProxyOrigin: "https://example.com/base"}).WebLoginProxyPublicOrigin())
	// 携带 query → 拒绝。
	require.Equal(t, "", (&config.ServerConfig{WebLoginProxyOrigin: "https://example.com?x=1"}).WebLoginProxyPublicOrigin())
	// 携带 fragment → 拒绝。
	require.Equal(t, "", (&config.ServerConfig{WebLoginProxyOrigin: "https://example.com#frag"}).WebLoginProxyPublicOrigin())
	// 携带 userinfo → 拒绝（user:pass@host 不得进入公开 settings）。
	require.Equal(t, "", (&config.ServerConfig{WebLoginProxyOrigin: "https://user:pass@example.com:8443"}).WebLoginProxyPublicOrigin())
	// 根路径 "/" 视为归一化放行。
	require.Equal(t, "https://example.com:8443",
		(&config.ServerConfig{WebLoginProxyOrigin: "https://example.com:8443"}).WebLoginProxyPublicOrigin())
}

// TestServerConfig_WebLoginProxyPublicOrigin_HostnameAndPort 验证空 hostname 与
// 端口范围校验（fail-closed）：只有端口的 Host（https://:8443）拒绝；端口须为
// 1-65535 数字，非数字/越界拒绝。
func TestServerConfig_WebLoginProxyPublicOrigin_HostnameAndPort(t *testing.T) {
	// 空 hostname（只有端口）→ 拒绝。
	require.Equal(t, "", (&config.ServerConfig{WebLoginProxyOrigin: "https://:8443"}).WebLoginProxyPublicOrigin())
	// 合法 hostname + 端口 → 放行。
	require.Equal(t, "https://example.com:8443",
		(&config.ServerConfig{WebLoginProxyOrigin: "https://example.com:8443"}).WebLoginProxyPublicOrigin())
	// 端口越界 → 拒绝。
	require.Equal(t, "", (&config.ServerConfig{WebLoginProxyOrigin: "https://example.com:99999"}).WebLoginProxyPublicOrigin())
	// 端口非数字 → 拒绝。
	require.Equal(t, "", (&config.ServerConfig{WebLoginProxyOrigin: "https://example.com:abc"}).WebLoginProxyPublicOrigin())
	// 端口边界值放行。
	require.Equal(t, "https://example.com:1",
		(&config.ServerConfig{WebLoginProxyOrigin: "https://example.com:1"}).WebLoginProxyPublicOrigin())
	require.Equal(t, "https://example.com:65535",
		(&config.ServerConfig{WebLoginProxyOrigin: "https://example.com:65535"}).WebLoginProxyPublicOrigin())
}
