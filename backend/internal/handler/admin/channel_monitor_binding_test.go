//go:build unit

package admin

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// muse-9 锚定:channel_monitor 的 Provider 写入口 oneof 白名单需含 muse
// （muse-6 已在前端 channelMonitor.ts 注册 'muse',此处后端写入口对齐）。
// 无既有 provider binding 测试,按派发单写最小锚定:provider=muse 通过校验进入
// handler,清单外值仍被拒。复用同包 bindGroupPlatformJSON 辅助。
func TestChannelMonitorProviderBinding_AllowsMuse(t *testing.T) {
	// 创建入口:required,oneof 含 muse
	t.Run("create_provider_muse", func(t *testing.T) {
		var req channelMonitorCreateRequest
		body := fmt.Sprintf(`{"name":"m","provider":%q,"interval_seconds":60}`, "muse")
		require.NoError(t, bindGroupPlatformJSON(t, &req, body),
			"provider muse 应通过 channelMonitorCreateRequest 校验")
		require.Equal(t, "muse", req.Provider)
	})

	// 更新入口:omitempty,oneof 含 muse
	t.Run("update_provider_muse", func(t *testing.T) {
		var req channelMonitorUpdateRequest
		body := fmt.Sprintf(`{"provider":%q}`, "muse")
		require.NoError(t, bindGroupPlatformJSON(t, &req, body),
			"provider muse 应通过 channelMonitorUpdateRequest 校验")
		require.NotNil(t, req.Provider)
		require.Equal(t, "muse", *req.Provider)
	})
}

func TestChannelMonitorProviderBinding_RejectsInvalidProvider(t *testing.T) {
	t.Run("create_provider_bogus_rejected", func(t *testing.T) {
		var req channelMonitorCreateRequest
		body := fmt.Sprintf(`{"name":"m","provider":%q,"interval_seconds":60}`, "bogus")
		require.Error(t, bindGroupPlatformJSON(t, &req, body),
			"清单外 provider 应被 channelMonitorCreateRequest 拒绝")
	})

	t.Run("update_provider_bogus_rejected", func(t *testing.T) {
		var req channelMonitorUpdateRequest
		body := fmt.Sprintf(`{"provider":%q}`, "bogus")
		require.Error(t, bindGroupPlatformJSON(t, &req, body),
			"清单外 provider 应被 channelMonitorUpdateRequest 拒绝")
	})
}
