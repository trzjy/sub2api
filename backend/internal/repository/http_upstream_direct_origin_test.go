package repository

import (
	"net/url"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// TestDirectOriginCacheSuffix 覆盖"直连独立池"与"默认关闭"：
// 启用且直连 → "|direct"；关闭、缺字段、或有代理 → 空（与主池同键，零变化）。
func TestDirectOriginCacheSuffix(t *testing.T) {
	enabled := &httpUpstreamService{cfg: &config.Config{
		Gateway: config.GatewayConfig{
			CodeBuddy: config.GatewayCodeBuddyConfig{
				DirectOrigin: map[string]config.CodeBuddyDirectOriginConfig{
					"cn": {Enabled: true, IP: "43.159.104.94", Host: "copilot.tencent.com"},
				},
			},
		},
	}}
	disabled := &httpUpstreamService{cfg: &config.Config{
		Gateway: config.GatewayConfig{
			CodeBuddy: config.GatewayCodeBuddyConfig{DirectOrigin: nil},
		},
	}}

	require.Equal(t, "|direct", enabled.directOriginCacheSuffix(nil))
	require.Equal(t, "", disabled.directOriginCacheSuffix(nil))
	// 有代理（parsedProxy 非 nil）→ 不隔离（代理负责路由）
	require.Equal(t, "", enabled.directOriginCacheSuffix(mustParseURL(t, "http://proxy:8080")))
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}
