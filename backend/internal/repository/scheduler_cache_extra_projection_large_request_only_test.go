//go:build unit

package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestFilterSchedulerExtraLargeRequestOnly 钉死 large_request_only 必须留在调度元数据投影里。
//
// 候选列举 listSchedulableAccounts 读的是 filterSchedulerExtra 裁出的同一份投影，
// 大请求专属开关经 service.IsLargeRequestOnlyAccount 读 extra.large_request_only 判定。
// 若白名单漏掉该键，快照解码出的账号一律按 false 处理 → 该开关在选号侧恒为 false，
// 大请求专属号会照常抢小单（OC-T1-FIX3 同型根因：投影裁键 = 规则恒不生效）。
func TestFilterSchedulerExtraLargeRequestOnly(t *testing.T) {
	t.Run("保留 large_request_only 且丢弃非白名单键", func(t *testing.T) {
		got := filterSchedulerExtra(map[string]any{
			service.LargeRequestOnlyExtraKey: true,
			"other":                          1,
		})

		require.NotNil(t, got)
		require.Equal(t, true, got[service.LargeRequestOnlyExtraKey],
			"投影必须保留 large_request_only，否则大请求专属开关在选号侧恒为 false")
		require.NotContains(t, got, "other", "非白名单键仍应被裁剪")
	})

	t.Run("源数据不含该键时投影也不得凭空造出", func(t *testing.T) {
		// "other" 不在白名单，整体被裁光时返回 nil，属既有行为。
		got := filterSchedulerExtra(map[string]any{"other": 1})

		require.NotContains(t, got, service.LargeRequestOnlyExtraKey,
			"源 extra 没有该键时投影不得凭空生成")
	})
}
