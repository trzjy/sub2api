//go:build unit

// E42 无信号哨兵不吃到期剔除：ActiveTokenHarborFreeTierScopes 对 precise_reset=false 条目
// 跳过 reset_at 到期剔除（持续受限占位值，须由主动复探确认）；precise_reset=true 条目仍按
// reset_at 到期照常剔除（标准行为，与前端展示语义一致）。
package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestE42TokenHarborNoSignalSentinelSkipsExpiryEviction(t *testing.T) {
	now := time.Unix(17000, 0).UTC()

	extra := map[string]any{
		modelRateLimitsKey: map[string]any{
			// 无信号哨兵：precise_reset=false + reset_at 已过去（模拟超过 365d 占位期限）
			"m-sentinel-expired": map[string]any{
				"reason":              tokenHarborFreeTierReasonPrefix + ":x",
				"rate_limit_reset_at": now.Add(-time.Hour).Format(time.RFC3339),
				"precise_reset":       false,
			},
			// 上游权威标记：precise_reset=true + reset_at 已过去 → 照常剔除（不回归）
			"m-precise-expired": map[string]any{
				"reason":              tokenHarborFreeTierReasonPrefix + ":x",
				"rate_limit_reset_at": now.Add(-time.Hour).Format(time.RFC3339),
				"precise_reset":       true,
			},
			// 未到期哨兵（远期占位值）→ 本就纳入
			"m-sentinel-future": map[string]any{
				"reason":              tokenHarborFreeTierReasonPrefix + ":x",
				"rate_limit_reset_at": now.Add(time.Hour).Format(time.RFC3339),
				"precise_reset":       false,
			},
			// 未到期权威标记 → 纳入
			"m-precise-future": map[string]any{
				"reason":              tokenHarborFreeTierReasonPrefix + ":x",
				"rate_limit_reset_at": now.Add(time.Hour).Format(time.RFC3339),
				"precise_reset":       true,
			},
			// 非 TokenHarbor 前缀 → 排除（不受到期逻辑影响）
			"m-other": map[string]any{
				"reason":              "some_other_limit",
				"rate_limit_reset_at": now.Add(-time.Hour).Format(time.RFC3339),
			},
		},
	}

	got := ActiveTokenHarborFreeTierScopes(extra, now)

	// 核心修复：无信号哨兵过期仍留在候选（不吃到期剔除）
	require.Contains(t, got, "m-sentinel-expired", "无信号哨兵 precise=false 过期应仍在候选")
	// 未到期哨兵 / 未到期权威标记仍在
	require.Contains(t, got, "m-sentinel-future")
	require.Contains(t, got, "m-precise-future")
	// precise=true 过期按要求照常剔除（不回归）
	require.NotContains(t, got, "m-precise-expired", "precise=true 过期应照常剔除")
	// 非 TokenHarbor 前缀排除
	require.NotContains(t, got, "m-other")
}
