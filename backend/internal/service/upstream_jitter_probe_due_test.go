//go:build unit

// JIT-03：上游抖动停调的模型恢复探测排程锁定为固定 15 秒，与额度入口无关。
package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUpstreamJitterProbeDue(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	resetAt := now.Add(37 * time.Minute)
	quotaKeyID := int64(42)

	// (a) upstream_jitter + cursor 账号（有额度入口）：owned=true，due≈now+15s，
	// 证明不会被「有额度入口 → 5 分钟」分支接管。
	t.Run("jitter_cursor_with_quota_key", func(t *testing.T) {
		acc := &Account{Platform: PlatformCursor, QuotaKeyID: &quotaKeyID}
		ok, due := ModelProbeDue(acc, "m-model", UpstreamJitterModelRateLimitReason, resetAt, false, now)
		require.True(t, ok)
		require.WithinDuration(t, now.Add(15*time.Second), due, time.Second)
	})

	// (b) upstream_jitter + 普通 OpenAI 账号（无额度入口）：owned=true，due≈now+15s，
	// 证明也不会被「无额度入口 → 30 秒」分支接管。
	t.Run("jitter_plain_openai_without_quota", func(t *testing.T) {
		acc := &Account{Platform: PlatformOpenAI}
		ok, due := ModelProbeDue(acc, "m-model", UpstreamJitterModelRateLimitReason, resetAt, false, now)
		require.True(t, ok)
		require.WithinDuration(t, now.Add(15*time.Second), due, time.Second)
	})

	// (c) 普通 429 类 reason：既有分支行为保持不变，证明本卡未改到其它分支。
	t.Run("ordinary_429_branches_unchanged", func(t *testing.T) {
		const reason = "ratelimit_429"

		// 有额度入口且 precise=false → now+5min。
		withQuota := &Account{Platform: PlatformCursor, QuotaKeyID: &quotaKeyID}
		ok, due := ModelProbeDue(withQuota, "m-model", reason, resetAt, false, now)
		require.True(t, ok)
		require.Equal(t, now.Add(5*time.Minute), due)

		// 无额度入口且 precise=false → now+30s。
		noQuota := &Account{Platform: PlatformOpenAI}
		ok, due = ModelProbeDue(noQuota, "m-model", reason, resetAt, false, now)
		require.True(t, ok)
		require.Equal(t, now.Add(30*time.Second), due)

		// 有额度入口且 precise=true → resetAt（补齐四格，确认 precise 分支未被触碰）。
		ok, due = ModelProbeDue(withQuota, "m-model", reason, resetAt, true, now)
		require.True(t, ok)
		require.Equal(t, resetAt, due)
	})

	// (d) acc == nil 时仍不排程，返回零值时刻。
	t.Run("nil_account_not_owned", func(t *testing.T) {
		ok, due := ModelProbeDue(nil, "m-model", UpstreamJitterModelRateLimitReason, resetAt, false, now)
		require.False(t, ok)
		require.True(t, due.IsZero())
	})
}
