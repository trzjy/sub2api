package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGatewayCodeBuddyConfigValidate 覆盖"启动 fail-fast"硬性要求：
// enabled=true 但 IP/Host 缺失或 IP 非法 → Validate 报错；关闭或字段齐全 → 通过。
func TestGatewayCodeBuddyConfigValidate(t *testing.T) {
	t.Run("nil/disabled 不报错", func(t *testing.T) {
		require.NoError(t, GatewayCodeBuddyConfig{}.Validate())
		require.NoError(t, GatewayCodeBuddyConfig{
			DirectOrigin: map[string]CodeBuddyDirectOriginConfig{
				"cn": {Enabled: false},
			},
		}.Validate())
	})

	t.Run("enabled 缺 IP → fail-fast", func(t *testing.T) {
		err := GatewayCodeBuddyConfig{
			DirectOrigin: map[string]CodeBuddyDirectOriginConfig{
				"cn": {Enabled: true, Host: "copilot.tencent.com"},
			},
		}.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "ip")
	})

	t.Run("enabled 缺 Host → fail-fast", func(t *testing.T) {
		err := GatewayCodeBuddyConfig{
			DirectOrigin: map[string]CodeBuddyDirectOriginConfig{
				"cn": {Enabled: true, IP: "43.159.104.94"},
			},
		}.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "host")
	})

	t.Run("enabled IP 非法 → fail-fast", func(t *testing.T) {
		err := GatewayCodeBuddyConfig{
			DirectOrigin: map[string]CodeBuddyDirectOriginConfig{
				"cn": {Enabled: true, IP: "not-an-ip", Host: "copilot.tencent.com"},
			},
		}.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "not a valid IP")
	})

	t.Run("enabled 字段齐全 → 通过", func(t *testing.T) {
		require.NoError(t, GatewayCodeBuddyConfig{
			DirectOrigin: map[string]CodeBuddyDirectOriginConfig{
				"cn":   {Enabled: true, IP: "43.159.104.94", Host: "copilot.tencent.com", Port: 443},
				"intl": {Enabled: true, IP: "203.0.113.5", Host: "www.codebuddy.ai"},
			},
		}.Validate())
	})

	t.Run("intl 缺 IP 也 fail-fast", func(t *testing.T) {
		err := GatewayCodeBuddyConfig{
			DirectOrigin: map[string]CodeBuddyDirectOriginConfig{
				"intl": {Enabled: true, Host: "www.codebuddy.ai"},
			},
		}.Validate()
		require.Error(t, err)
	})
}
