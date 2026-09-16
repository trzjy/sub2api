package config_test

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// TestServerConfig_WebLoginProxyPublicOrigin 验证隔离 origin 公开地址契约：
// 同源回退已禁止，独立 origin 必须显式配置，不再从监听地址推导。
func TestServerConfig_WebLoginProxyPublicOrigin(t *testing.T) {
	// 显式 origin 原样返回（已 TrimSpace）。
	require.Equal(t, "http://proxy.example.com:3400",
		(&config.ServerConfig{WebLoginProxyOrigin: "http://proxy.example.com:3400"}).WebLoginProxyPublicOrigin())
	require.Equal(t, "http://proxy.example.com:3400",
		(&config.ServerConfig{WebLoginProxyOrigin: "  http://proxy.example.com:3400  "}).WebLoginProxyPublicOrigin())

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
