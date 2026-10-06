//go:build unit

package config

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGatewayCodeBuddyUrgencyBoostValidate 覆盖 §4.2 紧迫度加权配置的强制契约：
// urgency_boost_k 必须为有限数值且 ∈ [0,1]——负数/NaN/Inf/超区间一律失败关闭
// （启动与热加载同一路径经 cfg.Validate()，无静默钳位/默认值兜底）。
// k=0 与默认零值合法；开关关闭时显式配置非法 k 同样拒绝。
func TestGatewayCodeBuddyUrgencyBoostValidate(t *testing.T) {
	t.Run("默认零值（false/0）合法", func(t *testing.T) {
		require.NoError(t, GatewayCodeBuddyConfig{}.Validate())
	})

	t.Run("开启且 k=0 合法（关闭加权）", func(t *testing.T) {
		require.NoError(t, GatewayCodeBuddyConfig{
			UrgencyBoostEnabled: true,
			UrgencyBoostK:       0,
		}.Validate())
	})

	t.Run("开启且 k∈[0,1] 边界值合法", func(t *testing.T) {
		require.NoError(t, GatewayCodeBuddyConfig{UrgencyBoostEnabled: true, UrgencyBoostK: 0}.Validate())
		require.NoError(t, GatewayCodeBuddyConfig{UrgencyBoostEnabled: true, UrgencyBoostK: 1}.Validate())
		require.NoError(t, GatewayCodeBuddyConfig{UrgencyBoostEnabled: true, UrgencyBoostK: 0.5}.Validate())
	})

	t.Run("k<0 拒绝", func(t *testing.T) {
		err := GatewayCodeBuddyConfig{
			UrgencyBoostEnabled: true,
			UrgencyBoostK:       -0.1,
		}.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "urgency_boost_k")
	})

	t.Run("k>1 拒绝", func(t *testing.T) {
		err := GatewayCodeBuddyConfig{
			UrgencyBoostEnabled: true,
			UrgencyBoostK:       1.1,
		}.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "urgency_boost_k")
	})

	t.Run("k=NaN 拒绝", func(t *testing.T) {
		err := GatewayCodeBuddyConfig{
			UrgencyBoostEnabled: true,
			UrgencyBoostK:       math.NaN(),
		}.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "urgency_boost_k")
	})

	t.Run("k=±Inf 拒绝", func(t *testing.T) {
		require.Error(t, GatewayCodeBuddyConfig{
			UrgencyBoostEnabled: true,
			UrgencyBoostK:       math.Inf(1),
		}.Validate())
		require.Error(t, GatewayCodeBuddyConfig{
			UrgencyBoostEnabled: true,
			UrgencyBoostK:       math.Inf(-1),
		}.Validate())
	})

	t.Run("关闭开关但 k 非法同样拒绝（无静默兜底）", func(t *testing.T) {
		err := GatewayCodeBuddyConfig{
			UrgencyBoostEnabled: false,
			UrgencyBoostK:       2.0,
		}.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "urgency_boost_k")
	})
}