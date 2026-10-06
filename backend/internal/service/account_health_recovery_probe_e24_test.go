//go:build unit

// E24 定向测试：isAccountLevelStale 的 observed_at / attempted_at 类型非法失败关闭
// （E21 顶回同类缺口）。判别复用 observedAtFromEntry + parseObservedAt（单一事实源，
// 与 E16/E21 同口径）：存在但非法 → 明确错误，绝不静默忽略后回退 attempted_at 当作
// 「有新鲜观测」而漏告警；无观测/空值回退 attempted_at 与合法值路径行为不变。
package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newE24StaleService(repo *d2ProbeObsRepo) *AccountHealthRecoveryProbeService {
	rl := &RateLimitService{accountRepo: repo}
	return &AccountHealthRecoveryProbeService{rateLimit: rl, accountRepo: repo}
}

// observed_at 存在但类型非法（数字/布尔/对象）→ 明确错误失败关闭，不得静默忽略后回退
// attempted_at 判定为「有新鲜观测」（即不得返回 stale=false, nil）。
func TestE24AccountLevelStale_NonStringObservedAtFailsClosed(t *testing.T) {
	now := time.Unix(30000, 0)
	for name, bad := range map[string]any{
		"number": float64(12345),
		"bool":   true,
		"object": map[string]any{"x": 1},
		"array":  []any{1, 2},
	} {
		t.Run(name, func(t *testing.T) {
			repo := newD2ProbeObsRepo()
			const acct int64 = 240
			// attempted_at 很新（若被当作回退判定会得出不陈旧）；observed_at 损坏必须直接报错。
			repo.presetEntry(acct, tokenHarborAccountLevelProbeScope, map[string]any{
				entryObservedAtKey:  bad,
				entryAttemptedAtKey: now.UTC().Format(time.RFC3339),
			})
			svc := newE24StaleService(repo)
			stale, err := svc.isAccountLevelStale(context.Background(), acct, time.Hour, now)
			require.Error(t, err, "类型非法 observed_at 必须失败关闭")
			require.False(t, stale, "失败关闭时不得判定为新鲜/陈旧")
		})
	}
}

// observed_at 存在且为 string 但格式非法 → 明确错误失败关闭（E16 同口径）。
func TestE24AccountLevelStale_InvalidFormatObservedAtFailsClosed(t *testing.T) {
	now := time.Unix(30000, 0)
	repo := newD2ProbeObsRepo()
	const acct int64 = 241
	repo.presetEntry(acct, tokenHarborAccountLevelProbeScope, map[string]any{
		entryObservedAtKey:  "corrupted-not-rfc3339",
		entryAttemptedAtKey: now.UTC().Format(time.RFC3339),
	})
	svc := newE24StaleService(repo)
	_, err := svc.isAccountLevelStale(context.Background(), acct, time.Hour, now)
	require.Error(t, err, "格式非法 observed_at 必须失败关闭")
}

// attempted_at 存在但类型非法 → 明确错误失败关闭（同口径补全）。
func TestE24AccountLevelStale_NonStringAttemptedAtFailsClosed(t *testing.T) {
	now := time.Unix(30000, 0)
	repo := newD2ProbeObsRepo()
	const acct int64 = 242
	repo.presetEntry(acct, tokenHarborAccountLevelProbeScope, map[string]any{
		entryAttemptedAtKey: float64(999),
	})
	svc := newE24StaleService(repo)
	_, err := svc.isAccountLevelStale(context.Background(), acct, time.Hour, now)
	require.Error(t, err, "类型非法 attempted_at 必须失败关闭")
}

// attempted_at 格式非法（string 不可解析）→ 明确错误失败关闭。
func TestE24AccountLevelStale_InvalidFormatAttemptedAtFailsClosed(t *testing.T) {
	now := time.Unix(30000, 0)
	repo := newD2ProbeObsRepo()
	const acct int64 = 243
	repo.presetEntry(acct, tokenHarborAccountLevelProbeScope, map[string]any{
		entryAttemptedAtKey: "not-a-time",
	})
	svc := newE24StaleService(repo)
	_, err := svc.isAccountLevelStale(context.Background(), acct, time.Hour, now)
	require.Error(t, err, "格式非法 attempted_at 必须失败关闭")
}

// observed_at 缺失 / nil（字面 null）/ 空串 / 仅空白 = 无观测 → 回退 attempted_at 判定，
// 既有语义不变（attempted_at 超阈值 → stale=true；未超 → stale=false；两者皆无 → false）。
func TestE24AccountLevelStale_NoObservationFallsBackToAttempted(t *testing.T) {
	now := time.Unix(30000, 0)
	cases := []struct {
		name      string
		entry     map[string]any
		wantStale bool
	}{
		{"missing both", map[string]any{}, false},
		{"null observed_at, fresh attempted", map[string]any{
			entryObservedAtKey:  nil,
			entryAttemptedAtKey: now.Add(-time.Minute).UTC().Format(time.RFC3339),
		}, false},
		{"empty observed_at, stale attempted", map[string]any{
			entryObservedAtKey:  "",
			entryAttemptedAtKey: now.Add(-2 * time.Hour).UTC().Format(time.RFC3339),
		}, true},
		{"blank observed_at, stale attempted", map[string]any{
			entryObservedAtKey:  "   ",
			entryAttemptedAtKey: now.Add(-2 * time.Hour).UTC().Format(time.RFC3339),
		}, true},
		{"null attempted_at only", map[string]any{
			entryAttemptedAtKey: nil,
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newD2ProbeObsRepo()
			const acct int64 = 244
			repo.presetEntry(acct, tokenHarborAccountLevelProbeScope, tc.entry)
			svc := newE24StaleService(repo)
			stale, err := svc.isAccountLevelStale(context.Background(), acct, time.Hour, now)
			require.NoError(t, err)
			require.Equal(t, tc.wantStale, stale)
		})
	}
}

// 合法值路径不变：observed_at 新鲜 → 不陈旧；observed_at 超阈值 → 陈旧；
// attempted_at 更新于 observed_at 时按更近者判定（既有回退语义）。
func TestE24AccountLevelStale_ValidPathsUnchanged(t *testing.T) {
	now := time.Unix(30000, 0)
	t.Run("fresh observed not stale", func(t *testing.T) {
		repo := newD2ProbeObsRepo()
		const acct int64 = 245
		repo.presetEntry(acct, tokenHarborAccountLevelProbeScope, map[string]any{
			entryObservedAtKey: now.Add(-time.Minute).UTC().Format(time.RFC3339),
		})
		stale, err := newE24StaleService(repo).isAccountLevelStale(context.Background(), acct, time.Hour, now)
		require.NoError(t, err)
		require.False(t, stale)
	})
	t.Run("old observed stale", func(t *testing.T) {
		repo := newD2ProbeObsRepo()
		const acct int64 = 246
		repo.presetEntry(acct, tokenHarborAccountLevelProbeScope, map[string]any{
			entryObservedAtKey: now.Add(-2 * time.Hour).UTC().Format(time.RFC3339),
		})
		stale, err := newE24StaleService(repo).isAccountLevelStale(context.Background(), acct, time.Hour, now)
		require.NoError(t, err)
		require.True(t, stale)
	})
	t.Run("newer attempted overrides observed", func(t *testing.T) {
		repo := newD2ProbeObsRepo()
		const acct int64 = 247
		repo.presetEntry(acct, tokenHarborAccountLevelProbeScope, map[string]any{
			entryObservedAtKey:  now.Add(-2 * time.Hour).UTC().Format(time.RFC3339),
			entryAttemptedAtKey: now.Add(-time.Minute).UTC().Format(time.RFC3339),
		})
		stale, err := newE24StaleService(repo).isAccountLevelStale(context.Background(), acct, time.Hour, now)
		require.NoError(t, err)
		require.False(t, stale, "attempted_at 更新于 observed_at 时取更近者（既有语义不变）")
	})
}
