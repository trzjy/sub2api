package admin

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 全局视觉路由 kill-switch 经 admin 设置写入链路的绑定与持久化测试。
// 复用 setting_handler_stepup_switch_test.go 的同包 doUpdateSettings helper。

// 非法值（非布尔）在绑定层即被拒绝。
func TestUpdateSettingsVisionRoutingInvalidBoolRejected(t *testing.T) {
	h, _ := newStepUpSwitchTestHandler(t, map[string]string{})

	rec := doUpdateSettings(t, h, map[string]any{"vision_routing_enabled": "not-a-bool"}, nil)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// 正常开关：显式 false 应 200 且最终持久化为 "false"。
func TestUpdateSettingsVisionRoutingDisablePersists(t *testing.T) {
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{})

	rec := doUpdateSettings(t, h, map[string]any{"vision_routing_enabled": false}, nil)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "false", repo.values[service.SettingKeyVisionRoutingEnabled])
}
