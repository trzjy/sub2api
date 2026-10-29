//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestCodeBuddyCreditProbeFromExtra 锁定 extra 聚合快照 → 探测结果还原口径：
// 余额查询的非 force 分支与调度阈值评估共用这三个聚合键，键缺失必须返回 nil（无快照），
// 不能造 0 余额。
func TestCodeBuddyCreditProbeFromExtra(t *testing.T) {
	updatedAt := "2026-10-29T15:32:48+08:00"
	extra := map[string]any{
		codebuddyCreditTotalRemainingKey:    "2030.22000035",
		codebuddyCreditTotalTotalKey:        "4200.00000000",
		codebuddyCreditUsedPercentKey:       float64(51.66),
		codebuddyCreditResetAtKey:           "2026-11-23T00:12:36+08:00",
		codebuddyCreditPackagesUpdatedAtKey: updatedAt,
	}

	got := CodeBuddyCreditProbeFromExtra(extra)
	require.NotNil(t, got)
	require.True(t, got.Success)
	require.InDelta(t, 4200.0, got.TotalCredit, 1e-6)
	require.InDelta(t, 4200.0-2030.22000035, got.UsedCredit, 1e-6)
	require.InDelta(t, 51.66, got.UsedPercent, 1e-6)
	require.Equal(t, "2026-11-23T00:12:36+08:00", got.ResetAt)
	want, err := time.Parse(time.RFC3339, updatedAt)
	require.NoError(t, err)
	require.Equal(t, want.Unix(), got.FetchedAt)
}

// TestCodeBuddyCreditProbeFromExtraNoSnapshot 锁定无快照语义：聚合键同时缺失返回 nil。
func TestCodeBuddyCreditProbeFromExtraNoSnapshot(t *testing.T) {
	require.Nil(t, CodeBuddyCreditProbeFromExtra(nil))
	require.Nil(t, CodeBuddyCreditProbeFromExtra(map[string]any{}))
	require.Nil(t, CodeBuddyCreditProbeFromExtra(map[string]any{
		codebuddyCreditUsedPercentKey: float64(0),
	}))
}
