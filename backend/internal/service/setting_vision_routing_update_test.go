package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// 全局视觉路由 kill-switch（vision_routing_enabled）写入链路测试。
// 验证指针/nillable 语义：请求显式携带时写入，未携带（nil）时跳过保留存储值，
// 且既有校验路径不被旁路。
func TestBuildSystemSettingsUpdatesVisionRouting(t *testing.T) {
	// (1) 携带 true：显式写入 "true"。
	t.Run("explicit_true_writes_true", func(t *testing.T) {
		svc := &SettingService{}
		tr := true
		updates, err := svc.buildSystemSettingsUpdates(context.Background(), &SystemSettings{
			VisionRoutingEnabled: &tr,
		})
		require.NoError(t, err)
		require.Equal(t, "true", updates[SettingKeyVisionRoutingEnabled])
	})

	// (2) 未携带（nil）：不写入该 key，保留存储值（运维回滚可达）。
	t.Run("nil_omits_key_preserves_stored", func(t *testing.T) {
		svc := &SettingService{}
		updates, err := svc.buildSystemSettingsUpdates(context.Background(), &SystemSettings{
			VisionRoutingEnabled: nil,
		})
		require.NoError(t, err)
		_, ok := updates[SettingKeyVisionRoutingEnabled]
		require.False(t, ok, "nil VisionRoutingEnabled must not write the key")
	})

	// (3) 携带 false 且既有校验路径不被旁路：非法邮箱后缀导致整体返回错误，
	// 错误返回时 updates 尚未写入（先校验后写），kill-switch 不应被写入。
	t.Run("explicit_false_with_invalid_whitelist_returns_error_no_write", func(t *testing.T) {
		svc := &SettingService{}
		f := false
		updates, err := svc.buildSystemSettingsUpdates(context.Background(), &SystemSettings{
			VisionRoutingEnabled:             &f,
			RegistrationEmailSuffixWhitelist: []string{"@invalid"},
		})
		require.Error(t, err)
		_, ok := updates[SettingKeyVisionRoutingEnabled]
		require.False(t, ok, "validation error must short-circuit before any write")
	})
}
