//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestFreshnessView_ThreeTimestampsPassthrough 管理端透传：观测时间 / 尝试时间 / 重置时间
// 三维时间戳 + 原因 + 有效阈值 + 告警状态全部透传，阈值与三维唯一公式同一计算结果。
func TestFreshnessView_ThreeTimestampsPassthrough(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	observed := now.Add(-30 * time.Second).UTC().Format(time.RFC3339)
	attempted := now.Add(-10 * time.Second).UTC().Format(time.RFC3339)
	resetAt := now.Add(365 * 24 * time.Hour).UTC().Format(time.RFC3339)
	entry := map[string]any{
		FreshnessObservedAtKey:  observed,
		FreshnessAttemptedAtKey: attempted,
		FreshnessResetAtKey:     resetAt,
		FreshnessReasonKey:      "tokenharbor_free_tier_exhausted",
	}

	threshold := AccountModelFreshnessThreshold(10)
	v, err := BuildAccountFreshnessView(entry, "gpt-4", threshold, now, true)
	require.NoError(t, err)

	require.Equal(t, "gpt-4", v.Scope)
	require.Equal(t, FreshnessDimAccountModel, v.Dimension)
	require.Equal(t, observed, v.ObservedAt)
	require.Equal(t, attempted, v.AttemptedAt)
	require.Equal(t, resetAt, v.ResetAt)
	require.Equal(t, "tokenharbor_free_tier_exhausted", v.Reason)
	require.Equal(t, threshold, v.EffectiveThreshold)
	require.True(t, v.ActiveAlert)
	require.False(t, v.Stale, "阈值内不应陈旧")
	require.Equal(t, FreshnessDisplayObserved, v.DisplayState)

	// 账号级（无模型键）维度：同一 builder，维度标识切换，阈值走账号级基线口径。
	lv, err := BuildAccountFreshnessView(entry, tokenHarborAccountLevelProbeScope, AccountLevelFreshnessThreshold(10), now, false)
	require.NoError(t, err)
	require.Equal(t, FreshnessDimAccountLevel, lv.Dimension)
	require.Equal(t, AccountLevelFreshnessThreshold(10), lv.EffectiveThreshold)
	require.False(t, lv.ActiveAlert)
}

// TestFreshnessView_NoSignalPlaceholderNoCountdown 无信号占位（哨兵 reset_at、无权威观测）
// 展示为 waiting_probe「等待主动复探」，**不显示为正常倒计时**、不判陈旧。
func TestFreshnessView_NoSignalPlaceholderNoCountdown(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	// 只有哨兵远端 reset_at（恢复时刻未知）+ 尝试时间，无 observed_at。
	entry := map[string]any{
		FreshnessAttemptedAtKey: now.Add(-5 * time.Minute).UTC().Format(time.RFC3339),
		FreshnessResetAtKey:     now.Add(365 * 24 * time.Hour).UTC().Format(time.RFC3339),
	}

	v, err := BuildAccountFreshnessView(entry, "gpt-4", AccountModelFreshnessThreshold(10), now, false)
	require.NoError(t, err)
	require.Equal(t, FreshnessDisplayWaitingProbe, v.DisplayState)
	require.NotEqual(t, FreshnessDisplayObserved, v.DisplayState)
	require.False(t, v.Stale, "无权威观测不判陈旧")
	require.NotEmpty(t, v.ResetAt, "哨兵占位仍透传，但不作倒计时展示")
	require.Empty(t, v.ObservedAt)
}

// TestFreshnessView_StaleByObservedOnly 陈旧只看权威观测时间：attempted_at 很新也不解除
// 陈旧（尝试时间不解除告警）。
func TestFreshnessView_StaleByObservedOnly(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	threshold := AccountModelFreshnessThreshold(10)
	entry := map[string]any{
		FreshnessObservedAtKey:  now.Add(-(threshold + time.Second)).UTC().Format(time.RFC3339),
		FreshnessAttemptedAtKey: now.UTC().Format(time.RFC3339), // 刚刚尝试过
	}

	v, err := BuildAccountFreshnessView(entry, "gpt-4", threshold, now, true)
	require.NoError(t, err)
	require.True(t, v.Stale)
	require.Equal(t, FreshnessDisplayStale, v.DisplayState)
	require.True(t, v.ActiveAlert)
}

// TestFreshnessView_EmptyEntry 条目不存在（健康无流量 / 未观测）同样走 waiting_probe：
// 不产生陈旧、不显示倒计时。
func TestFreshnessView_EmptyEntry(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	v, err := BuildAccountFreshnessView(nil, "gpt-4", AccountModelFreshnessThreshold(10), now, false)
	require.NoError(t, err)
	require.Equal(t, FreshnessDisplayWaitingProbe, v.DisplayState)
	require.False(t, v.Stale)
	require.False(t, v.ActiveAlert)
}

// TestFreshnessView_InvalidObservedAtFailsClosed 权威数据损坏：observed_at 存在但格式非法时
// 视图必须返回明确错误，**不得**降级为 waiting_probe/observed 正常态（需与「空值=无观测」
// 严格区分）。第三轮终审 #5。
func TestFreshnessView_InvalidObservedAtFailsClosed(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	entry := map[string]any{
		FreshnessObservedAtKey: "not-a-timestamp",
	}
	_, err := BuildAccountFreshnessView(entry, "gpt-4", AccountModelFreshnessThreshold(10), now, false)
	require.Error(t, err, "非法 observed_at 必须失败关闭，不得降级为正常态")
	require.Contains(t, err.Error(), "invalid observed_at")
}

// TestFreshnessView_BlankObservedAtIsNoObservation 仅空白 observed_at 视作无权威观测
// （waiting_probe），不是损坏——与非法格式（错误）严格区分。
func TestFreshnessView_BlankObservedAtIsNoObservation(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	entry := map[string]any{FreshnessObservedAtKey: "   "}
	v, err := BuildAccountFreshnessView(entry, "gpt-4", AccountModelFreshnessThreshold(10), now, false)
	require.NoError(t, err, "空白 observed_at 是「无观测」而非损坏，不得报错")
	require.Equal(t, FreshnessDisplayWaitingProbe, v.DisplayState)
}

// TestFreshnessView_NullObservedAtIsNoObservation 字面 null（nil）observed_at 视作无权威
// 观测（waiting_probe），不是损坏——与「存在但类型非法」（错误）严格区分（第四轮终审 E21）。
func TestFreshnessView_NullObservedAtIsNoObservation(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	entry := map[string]any{FreshnessObservedAtKey: nil}
	v, err := BuildAccountFreshnessView(entry, "gpt-4", AccountModelFreshnessThreshold(10), now, false)
	require.NoError(t, err, "字面 null observed_at 是「无观测」而非损坏，不得报错")
	require.Equal(t, FreshnessDisplayWaitingProbe, v.DisplayState)
}

// TestFreshnessView_NonStringObservedAtFailsClosed observed_at 存在但类型非法（数字/布尔/
// 对象等损坏 JSON）必须失败关闭（返回明确错误），不得静默降级为无观测/正常态
// （第四轮终审 E21，E16 语义的类型维度补全）。
func TestFreshnessView_NonStringObservedAtFailsClosed(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	badTypes := []any{
		float64(1720000000),           // 数字
		true,                          // 布尔
		map[string]any{"x": "y"},      // 对象
		[]any{"2026-10-03T12:00:00Z"}, // 数组
	}
	for _, bad := range badTypes {
		entry := map[string]any{FreshnessObservedAtKey: bad}
		_, err := BuildAccountFreshnessView(entry, "gpt-4", AccountModelFreshnessThreshold(10), now, false)
		require.Error(t, err, "类型 %T 的 observed_at 必须失败关闭，不得降级为正常态", bad)
		require.Contains(t, err.Error(), "invalid observed_at", "类型 %T 应返回明确损坏错误", bad)
	}
}

// TestFreshnessView_ZeroObservedAtIsNoObservation 零值 RFC3339（0001-01-01T00:00:00Z）observed_at
// 视作无观测（waiting_probe、stale=false），与告警路径 accountObservedAt:393、探测路径 :1476
// 同一口径（IsZero 归无观测）——杜绝同一损坏/哨兵条目管理端显示「已观测健康」而告警按无观测
// 处理的口径分裂（E48 核心回归）。
func TestFreshnessView_ZeroObservedAtIsNoObservation(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	// 零值 RFC3339 字符串：time.Time{}.UTC().Format(time.RFC3339) = 0001-01-01T00:00:00Z。
	zeroRFC3339 := time.Time{}.UTC().Format(time.RFC3339)
	require.Equal(t, "0001-01-01T00:00:00Z", zeroRFC3339)
	entry := map[string]any{FreshnessObservedAtKey: zeroRFC3339}

	v, err := BuildAccountFreshnessView(entry, "gpt-4", AccountModelFreshnessThreshold(10), now, true)
	require.NoError(t, err, "零值 observed_at 是「无观测」而非损坏，不得报错")
	require.Equal(t, FreshnessDisplayWaitingProbe, v.DisplayState, "零值 observed_at 须归无观测")
	require.NotEqual(t, FreshnessDisplayObserved, v.DisplayState)
	require.False(t, v.Stale, "零值 observed_at 不判陈旧")
	require.True(t, v.ActiveAlert, "告警状态透传不受 observed_at 零值影响")
}

// TestFreshnessView_NonStringAttemptedAtFailsClosed attempted_at 存在但类型非法（数字/布尔/对象
// 等损坏 JSON）必须失败关闭（返回明确错误），与 isAccountLevelStale 同口径（E48）。绝不静默
// 忽略继续返回成功响应。
func TestFreshnessView_NonStringAttemptedAtFailsClosed(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	badTypes := []any{
		float64(123),                     // 数字
		true,                             // 布尔
		map[string]any{"x": "y"},         // 对象
		[]any{"2026-10-03T12:00:00Z"},    // 数组
	}
	for _, bad := range badTypes {
		entry := map[string]any{
			FreshnessObservedAtKey:  now.Add(-30 * time.Second).UTC().Format(time.RFC3339),
			FreshnessAttemptedAtKey: bad,
		}
		_, err := BuildAccountFreshnessView(entry, "gpt-4", AccountModelFreshnessThreshold(10), now, false)
		require.Error(t, err, "类型 %T 的 attempted_at 必须失败关闭，不得静默降级为成功响应", bad)
		require.Contains(t, err.Error(), "invalid attempted_at", "类型 %T 应返回明确损坏错误", bad)
	}
}

// TestFreshnessView_InvalidAttemptedAtFailsClosed attempted_at 为存在但非法时间字符串（如
// "not-a-time"）必须失败关闭（返回明确错误），与 isAccountLevelStale 同口径（E48）。
func TestFreshnessView_InvalidAttemptedAtFailsClosed(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	entry := map[string]any{
		FreshnessObservedAtKey:  now.Add(-30 * time.Second).UTC().Format(time.RFC3339),
		FreshnessAttemptedAtKey: "not-a-time",
	}
	_, err := BuildAccountFreshnessView(entry, "gpt-4", AccountModelFreshnessThreshold(10), now, false)
	require.Error(t, err, "非法 attempted_at 必须失败关闭，不得静默降级为成功响应")
	require.Contains(t, err.Error(), "invalid attempted_at")
}

// TestFreshnessView_AttemptedAtNoValueOrValidNoError attempted_at 缺失 / null / 空串 / 仅空白 /
// 合法值均不报错且既有展示不变；observed_at 合法值 stale/observed 判定不变（E48 无回归）。
func TestFreshnessView_AttemptedAtNoValueOrValidNoError(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	observed := now.Add(-30 * time.Second).UTC().Format(time.RFC3339)
	validAttempted := now.Add(-10 * time.Second).UTC().Format(time.RFC3339)
	cases := []struct {
		name string
		key  bool
		val  any
	}{
		{"缺失", false, nil},
		{"null", true, nil},
		{"空串", true, ""},
		{"仅空白", true, "   "},
		{"合法值", true, validAttempted},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			entry := map[string]any{FreshnessObservedAtKey: observed}
			if c.key {
				entry[FreshnessAttemptedAtKey] = c.val
			}
			v, err := BuildAccountFreshnessView(entry, "gpt-4", AccountModelFreshnessThreshold(10), now, false)
			require.NoError(t, err, "attempted_at=%v 不得报错", c.val)
			require.Equal(t, FreshnessDisplayObserved, v.DisplayState, "observed 合法值判定不变")
			require.False(t, v.Stale)
			if c.name == "合法值" {
				require.Equal(t, validAttempted, v.AttemptedAt, "合法 attempted_at 复制展示不变")
			} else {
				require.Empty(t, v.AttemptedAt, "无值 attempted_at 不复制字段")
			}
		})
	}
}
