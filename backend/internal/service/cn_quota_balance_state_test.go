//go:build unit

package service

// 余额三态契约单元测试（方案 §4 F1 / §3.1 R3-F1 / R5-F1；C1 解析器唯一归属）。
//
// 覆盖 cn_quota_balance_state.go 的：
//   - 三态判定：缺键/缺时间戳/时间戳不可解析/值不可解析/超 10 分钟年龄门/未来时间戳
//     → BalanceStaleOrMissing（unknown 态，绝不产生耗尽结论）；
//   - 新鲜（含 0 与负值）→ BalanceFresh，且仅 fresh≤0 时 IsExhausted 为真；
//   - 逐来源独立年龄门：消费各自 {platform}_balance + {platform}_balance_updated_at
//     键族，不因他源或账号级时间串味；
//   - ResolveKiraVNDBalanceState / ResolveTHWalletBalanceState 委托与键族绑定。

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestResolveBalanceStateThreeStateContract(t *testing.T) {
	platform := PlatformKimi
	now := quotaLifecycleBase
	balanceKey := cnExtraKey(platform, cnBalanceExtraSuffixBalance)
	updatedKey := cnExtraKey(platform, cnBalanceExtraSuffixUpdated)

	t.Run("missing balance key -> stale-or-missing (unknown)", func(t *testing.T) {
		st := ResolveBalanceState(map[string]any{}, platform, now)
		require.Equal(t, BalanceStaleOrMissing, st.Kind)
		require.True(t, st.IsUnknown())
		require.False(t, st.IsExhausted())
		require.False(t, st.IsFresh())
	})

	t.Run("missing updated_at key -> stale-or-missing", func(t *testing.T) {
		extra := map[string]any{balanceKey: 100.0}
		st := ResolveBalanceState(extra, platform, now)
		require.Equal(t, BalanceStaleOrMissing, st.Kind)
		require.True(t, st.IsUnknown())
	})

	t.Run("empty updated_at -> stale-or-missing", func(t *testing.T) {
		extra := map[string]any{balanceKey: 100.0, updatedKey: ""}
		st := ResolveBalanceState(extra, platform, now)
		require.Equal(t, BalanceStaleOrMissing, st.Kind)
	})

	t.Run("unparseable updated_at -> stale-or-missing", func(t *testing.T) {
		extra := map[string]any{balanceKey: 100.0, updatedKey: "not-a-time"}
		st := ResolveBalanceState(extra, platform, now)
		require.Equal(t, BalanceStaleOrMissing, st.Kind)
	})

	t.Run("unparseable balance value -> stale-or-missing", func(t *testing.T) {
		extra := map[string]any{balanceKey: "abc", updatedKey: now.UTC().Format(time.RFC3339)}
		st := ResolveBalanceState(extra, platform, now)
		require.Equal(t, BalanceStaleOrMissing, st.Kind)
	})

	t.Run("beyond 10-min age gate -> stale-or-missing", func(t *testing.T) {
		old := now.Add(-(cnQuotaBalanceFreshnessThresholdMinutes + 1) * time.Minute)
		extra := map[string]any{balanceKey: 100.0, updatedKey: old.UTC().Format(time.RFC3339)}
		st := ResolveBalanceState(extra, platform, now)
		require.Equal(t, BalanceStaleOrMissing, st.Kind)
		require.True(t, st.IsUnknown(), "stale balance must be unknown, never exhausted")
	})

	t.Run("exactly at edge (10 min) is fresh", func(t *testing.T) {
		// 年龄门为严格大于阈值才判 stale；等于阈值仍新鲜。
		edge := now.Add(-cnQuotaBalanceFreshnessThresholdMinutes * time.Minute)
		extra := map[string]any{balanceKey: 100.0, updatedKey: edge.UTC().Format(time.RFC3339)}
		st := ResolveBalanceState(extra, platform, now)
		require.Equal(t, BalanceFresh, st.Kind)
	})

	t.Run("fresh positive -> BalanceFresh and not exhausted", func(t *testing.T) {
		recent := now.Add(-5 * time.Minute)
		extra := map[string]any{balanceKey: 100.0, updatedKey: recent.UTC().Format(time.RFC3339)}
		st := ResolveBalanceState(extra, platform, now)
		require.Equal(t, BalanceFresh, st.Kind)
		require.True(t, st.IsFresh())
		require.Equal(t, 100.0, st.Value)
		require.False(t, st.IsExhausted(), "positive balance is not exhausted")
	})

	t.Run("fresh zero balance -> BalanceFresh and IsExhausted", func(t *testing.T) {
		recent := now.Add(-1 * time.Minute)
		extra := map[string]any{balanceKey: 0.0, updatedKey: recent.UTC().Format(time.RFC3339)}
		st := ResolveBalanceState(extra, platform, now)
		require.Equal(t, BalanceFresh, st.Kind)
		require.True(t, st.IsExhausted(), "fresh zero is exhausted")
	})

	t.Run("fresh negative balance -> BalanceFresh and IsExhausted", func(t *testing.T) {
		recent := now.Add(-1 * time.Minute)
		extra := map[string]any{balanceKey: -1.0, updatedKey: recent.UTC().Format(time.RFC3339)}
		st := ResolveBalanceState(extra, platform, now)
		require.Equal(t, BalanceFresh, st.Kind)
		require.True(t, st.IsExhausted())
	})

	t.Run("future observed_at -> stale-or-missing (untrusted)", func(t *testing.T) {
		future := now.Add(5 * time.Minute)
		extra := map[string]any{balanceKey: 100.0, updatedKey: future.UTC().Format(time.RFC3339)}
		st := ResolveBalanceState(extra, platform, now)
		require.Equal(t, BalanceStaleOrMissing, st.Kind)
	})

	t.Run("empty platform -> stale-or-missing", func(t *testing.T) {
		st := ResolveBalanceState(map[string]any{}, "", now)
		require.Equal(t, BalanceStaleOrMissing, st.Kind)
	})
}

func TestResolveBalanceStatePerSourceIndependentAgeGate(t *testing.T) {
	// 逐来源独立年龄门：A 源新鲜、B 源过期不得互相替代；本解析器只消费传入 platform
	// 的键族，不因他源时间串味。
	now := quotaLifecycleBase
	stalePlatform := PlatformDeepseek
	freshPlatform := PlatformKimi

	staleExtra := map[string]any{
		cnExtraKey(stalePlatform, cnBalanceExtraSuffixBalance):   1.0,
		cnExtraKey(stalePlatform, cnBalanceExtraSuffixUpdated):  now.Add(-20 * time.Minute).UTC().Format(time.RFC3339),
		freshPlatform:                                           nil, // 故意不写新鲜源键族
	}
	// 消费 freshPlatform 键族：缺键 → unknown，即使 stalePlatform 源新鲜也不串味。
	st := ResolveBalanceState(staleExtra, freshPlatform, now)
	require.Equal(t, BalanceStaleOrMissing, st.Kind)

	// 消费 stalePlatform 键族：过期 → unknown（不因为同 extra 里没有新鲜源就误判）。
	st2 := ResolveBalanceState(staleExtra, stalePlatform, now)
	require.Equal(t, BalanceStaleOrMissing, st2.Kind)
}

func TestResolveKiraVNDBalanceStateDelegates(t *testing.T) {
	account := newQuotaLifecycleKiraAccount(208)
	recent := quotaLifecycleBase.Add(-1 * time.Minute)
	account.Extra[cnExtraKey(account.Platform, cnBalanceExtraSuffixBalance)] = 0.0
	account.Extra[cnExtraKey(account.Platform, cnBalanceExtraSuffixUpdated)] = recent.UTC().Format(time.RFC3339)

	st := ResolveKiraVNDBalanceState(account, quotaLifecycleBase)
	require.Equal(t, BalanceFresh, st.Kind)
	require.True(t, st.IsExhausted(), "fresh Kira VND=0 is exhausted")

	require.Equal(t, BalanceStaleOrMissing, ResolveKiraVNDBalanceState(nil, quotaLifecycleBase).Kind)
}

func TestResolveTHWalletBalanceStateDelegates(t *testing.T) {
	account := newQuotaLifecycleTHAccount(207)
	recent := quotaLifecycleBase.Add(-1 * time.Minute)
	account.Extra[TokenHarborWalletBalanceExtraKey] = 50.0
	account.Extra[TokenHarborWalletBalanceUpdatedAtExtraKey] = recent.UTC().Format(time.RFC3339)

	st := ResolveTHWalletBalanceState(account, quotaLifecycleBase)
	require.Equal(t, BalanceFresh, st.Kind)
	require.Equal(t, 50.0, st.Value)

	// 缺键 → unknown。
	require.Equal(t, BalanceStaleOrMissing, ResolveTHWalletBalanceState(newQuotaLifecycleTHAccount(207), quotaLifecycleBase).Kind)
}
