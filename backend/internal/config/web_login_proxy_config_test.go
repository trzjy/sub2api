package config_test

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// TestServerConfig_WebLoginProxyPublicOrigin 验证隔离 origin 公开地址推导契约。
func TestServerConfig_WebLoginProxyPublicOrigin(t *testing.T) {
	// 默认显式 host → 推导出 http://host:port。
	require.Equal(t, "http://127.0.0.1:3400",
		(&config.ServerConfig{WebLoginProxyAddr: "127.0.0.1:3400"}).WebLoginProxyPublicOrigin())

	// 显式 origin 覆盖推导。
	require.Equal(t, "http://proxy.example.com:3400",
		(&config.ServerConfig{WebLoginProxyOrigin: "http://proxy.example.com:3400"}).WebLoginProxyPublicOrigin())

	// addr 为空 → 空 origin（回退同源）。
	require.Equal(t, "", (&config.ServerConfig{}).WebLoginProxyPublicOrigin())

	// 通配 host（监听所有接口但无法确定对外 host）→ 回退同源。
	require.Equal(t, "", (&config.ServerConfig{WebLoginProxyAddr: ":3400"}).WebLoginProxyPublicOrigin())
	require.Equal(t, "", (&config.ServerConfig{WebLoginProxyAddr: "0.0.0.0:3400"}).WebLoginProxyPublicOrigin())

	// 缺省端口 → 回落 3400。
	require.Equal(t, "http://10.0.0.5:3400",
		(&config.ServerConfig{WebLoginProxyAddr: "10.0.0.5"}).WebLoginProxyPublicOrigin())
}
