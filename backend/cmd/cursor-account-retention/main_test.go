package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func ts(offset time.Duration) *time.Time {
	t := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(offset)
	return &t
}

func idsOf(rows []retentionRow) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.id)
	}
	return out
}

// 测试 1：5 个账号、target=2，last_success 各不相同 → 保留最近成功 2 个。
func TestSelectRetentionKeepsMostRecentSuccess(t *testing.T) {
	rows := []retentionRow{
		{id: 1, name: "a", createdAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), lastSuccess: ts(1 * time.Hour)},
		{id: 2, name: "b", createdAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), lastSuccess: ts(5 * time.Hour)},
		{id: 3, name: "c", createdAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), lastSuccess: ts(3 * time.Hour)},
		{id: 4, name: "d", createdAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), lastSuccess: ts(2 * time.Hour)},
		{id: 5, name: "e", createdAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), lastSuccess: ts(4 * time.Hour)},
	}

	keep, drop := selectRetention(rows, 2)
	require.Equal(t, []int64{2, 5}, idsOf(keep), "保留最近成功 2 个（5h, 4h）")
	require.Equal(t, []int64{3, 4, 1}, idsOf(drop))
}

// 测试 2：两个账号 last_success 为 NULL → 排在最后，仅当名额有剩才保留。
func TestSelectRetentionNullLastSuccessSortedLast(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// 2 个有最近成功时间 + 2 个 NULL。
	rows := []retentionRow{
		{id: 1, name: "a", createdAt: base, lastSuccess: ts(2 * time.Hour)},
		{id: 2, name: "b", createdAt: base, lastSuccess: ts(1 * time.Hour)},
		{id: 3, name: "c", createdAt: base, lastSuccess: nil},
		{id: 4, name: "d", createdAt: base, lastSuccess: nil},
	}

	// target=2：名额被 2 个非 NULL 占满，NULL 全部删除。
	keep, drop := selectRetention(rows, 2)
	require.Equal(t, []int64{1, 2}, idsOf(keep))
	// 两个 NULL 且 created_at 相同，按 id DESC 兜底：4 在 3 前。
	require.Equal(t, []int64{4, 3}, idsOf(drop))
	require.Len(t, keep, 2)

	// target=3：名额有剩，NULL 中保留 1 个（id DESC 兜底取 4）。
	keep, drop = selectRetention(rows, 3)
	require.Equal(t, []int64{1, 2, 4}, idsOf(keep))
	require.Equal(t, []int64{3}, idsOf(drop))
}

// 测试 3：last_success 相同 → 按 created_at 降序、再按 id 降序。
func TestSelectRetentionSameLastSuccessTieBreak(t *testing.T) {
	ls := ts(10 * time.Hour)
	rows := []retentionRow{
		{id: 1, name: "a", createdAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), lastSuccess: ls},
		{id: 2, name: "b", createdAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), lastSuccess: ls}, // 更新
		{id: 3, name: "c", createdAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), lastSuccess: ls}, // 同 created_at，id 更大
	}

	keep, drop := selectRetention(rows, 2)
	// 同为 created_at=1/2 的 id 2、3 应排在 id 1 之前；id 3 > id 2 故 3 在前。
	require.Equal(t, []int64{3, 2}, idsOf(keep))
	require.Equal(t, []int64{1}, idsOf(drop))
}

// 测试 4：target=1 时只保留 1 个。
func TestSelectRetentionTargetOne(t *testing.T) {
	rows := []retentionRow{
		{id: 1, name: "a", createdAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), lastSuccess: ts(1 * time.Hour)},
		{id: 2, name: "b", createdAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), lastSuccess: ts(9 * time.Hour)},
		{id: 3, name: "c", createdAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), lastSuccess: ts(5 * time.Hour)},
	}

	keep, drop := selectRetention(rows, 1)
	require.Equal(t, []int64{2}, idsOf(keep))
	require.Equal(t, []int64{3, 1}, idsOf(drop))
}

// 测试 5：账号数 ≤ target → 全部保留、drop 为空。
func TestSelectRetentionAllKeptWhenBelowTarget(t *testing.T) {
	rows := []retentionRow{
		{id: 1, name: "a", createdAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), lastSuccess: ts(1 * time.Hour)},
		{id: 2, name: "b", createdAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), lastSuccess: nil},
	}

	keep, drop := selectRetention(rows, 5)
	require.Equal(t, []int64{1, 2}, idsOf(keep))
	require.Empty(t, drop)

	// target 为负数时也不应越界（保留 0 个）。
	keep, drop = selectRetention(rows, -1)
	require.Empty(t, keep)
	require.Equal(t, []int64{1, 2}, idsOf(drop))
}
