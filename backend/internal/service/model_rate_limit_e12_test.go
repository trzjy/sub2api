//go:build unit

// E12 定向测试（service 层）：ActiveTokenHarborFreeTierScopes 是供 repository 候选粗筛
// 复用的权威判定导出入口，必须与探测链内部判定 activeTokenHarborFreeTierScopes 逐位等价，
// 防止两处语义漂移（repository 不复制语义、探测链仍为权威）。
package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestE12ActiveTokenHarborFreeTierScopes_MatchesProbePredicate(t *testing.T) {
	now := time.Unix(17000, 0).UTC()
	extra := map[string]any{
		modelRateLimitsKey: map[string]any{
			"m-precise-expired": map[string]any{
				"reason":              tokenHarborFreeTierReasonPrefix + ":x",
				"rate_limit_reset_at": now.Add(-time.Hour).Format(time.RFC3339),
				"precise_reset":       true,
			},
			"m-legacy-expired": map[string]any{
				"reason":              tokenHarborFreeTierReasonPrefix + ":x",
				"rate_limit_reset_at": now.Add(-time.Hour).Format(time.RFC3339),
				"precise_reset":       false,
			},
			"m-sentinel-future": map[string]any{
				"reason":              tokenHarborFreeTierReasonPrefix + ":x",
				"rate_limit_reset_at": now.Add(time.Hour).Format(time.RFC3339),
			},
			"m-other-prefix": map[string]any{
				"reason":              "some_other_limit",
				"rate_limit_reset_at": now.Add(time.Hour).Format(time.RFC3339),
			},
			"m-bad-time": map[string]any{
				"reason":              tokenHarborFreeTierReasonPrefix + ":x",
				"rate_limit_reset_at": "not-a-time",
			},
			"m-empty-reset": map[string]any{
				"reason": tokenHarborFreeTierReasonPrefix + ":x",
			},
		},
	}

	want := (&AccountHealthRecoveryProbeService{}).activeTokenHarborFreeTierScopes(&Account{ID: 1, Extra: extra}, now)
	got := ActiveTokenHarborFreeTierScopes(extra, now)
	require.ElementsMatch(t, want, got, "导出判定必须与探测链权威判定逐位等价")
	require.ElementsMatch(t,
		[]string{"m-legacy-expired", "m-sentinel-future"},
		got, "无信号哨兵 precise=false 过期仍纳入（不吃到期剔除）；precise=true 过期照常剔除；前缀不符/时间非法/缺 reset 排除")
}

func TestE12ActiveTokenHarborFreeTierScopes_NilAndEmpty(t *testing.T) {
	require.Nil(t, ActiveTokenHarborFreeTierScopes(nil, time.Now()))
	require.Nil(t, ActiveTokenHarborFreeTierScopes(map[string]any{}, time.Now()))
	require.Nil(t, ActiveTokenHarborFreeTierScopes(map[string]any{modelRateLimitsKey: "not-a-map"}, time.Now()))
}
