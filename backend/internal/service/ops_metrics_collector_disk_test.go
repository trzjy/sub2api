package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestResolveDiskUsagePercent covers the root-partition disk usage computation
// used by the ops metrics collector (disk-guard plan 2026-09-26).
func TestResolveDiskUsagePercent(t *testing.T) {
	t.Run("正常情况: 40G 已用 / 50G 总量 = 80%", func(t *testing.T) {
		pct := resolveDiskUsagePercent(40*1024*1024*1024, 50*1024*1024*1024)
		require.NotNil(t, pct)
		require.InDelta(t, 80.0, *pct, 0.05)
	})

	t.Run("四舍五入到 1 位小数: 1/3 ≈ 33.3%", func(t *testing.T) {
		pct := resolveDiskUsagePercent(1, 3)
		require.NotNil(t, pct)
		require.InDelta(t, 33.3, *pct, 0.0001)
	})

	t.Run("边界情况: total = 0 返回 nil（避免除零）", func(t *testing.T) {
		require.Nil(t, resolveDiskUsagePercent(100, 0))
	})

	t.Run("边界情况: used = 0 返回 0%", func(t *testing.T) {
		pct := resolveDiskUsagePercent(0, 50*1024*1024*1024)
		require.NotNil(t, pct)
		require.InDelta(t, 0.0, *pct, 0.0001)
	})

	t.Run("告警阈值语义: 86% 超过 85% 阈值", func(t *testing.T) {
		pct := resolveDiskUsagePercent(43*1024*1024*1024, 50*1024*1024*1024)
		require.NotNil(t, pct)
		require.Greater(t, *pct, 85.0)
	})
}
