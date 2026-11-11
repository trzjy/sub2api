package antigravity

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAggregateUserQuotaBuckets_WorstBucketWins(t *testing.T) {
	buckets := []UserQuotaBucket{
		{ModelID: "gemini-3.8-flash", TokenType: "REQUESTS", RemainingFraction: 0.25, ResetTime: "2026-01-02T00:00:00Z"},
		{ModelID: "gemini-3.8-flash", TokenType: "TOKENS", RemainingFraction: 0.4, ResetTime: "2026-01-03T00:00:00Z"},
	}

	got := AggregateUserQuotaBuckets(buckets)

	require.Len(t, got, 1)
	usage := got["gemini-3.8-flash"]
	require.Equal(t, 0.25, usage.RemainingFraction, "同一模型取剩余最少的维度")
	require.Equal(t, "2026-01-02T00:00:00Z", usage.ResetTime, "重置时间跟随最差的那条")
	require.Equal(t, 75.0, usage.UsagePercent())
}

func TestAggregateUserQuotaBuckets_NormalizesModelPrefix(t *testing.T) {
	got := AggregateUserQuotaBuckets([]UserQuotaBucket{
		{ModelID: " models/gemini-3.8-flash-high ", RemainingFraction: 0.6},
	})

	require.Contains(t, got, "gemini-3.8-flash-high")
	require.Equal(t, 40.0, got["gemini-3.8-flash-high"].UsagePercent())
}

func TestAggregateUserQuotaBuckets_SkipsNonModelKeys(t *testing.T) {
	got := AggregateUserQuotaBuckets([]UserQuotaBucket{
		{ModelID: "chat_20706", RemainingFraction: 1},
		{ModelID: "tab_flash_lite_preview", RemainingFraction: 1},
		{ModelID: "gemini-3.8-flash-high", RemainingFraction: 1},
	})

	require.Len(t, got, 1)
	require.Contains(t, got, "gemini-3.8-flash-high")
}

func TestAggregateUserQuotaBuckets_ClampsAndSkips(t *testing.T) {
	got := AggregateUserQuotaBuckets([]UserQuotaBucket{
		{ModelID: "", RemainingFraction: 0.1},
		{ModelID: "over", RemainingFraction: 1.7},
		{ModelID: "under", RemainingFraction: -0.5},
	})

	require.Len(t, got, 2)
	require.Equal(t, 0.0, got["over"].UsagePercent())
	require.Equal(t, 100.0, got["under"].UsagePercent())
}

func TestUserQuotaUsage_UsagePercentKeepsPrecision(t *testing.T) {
	require.Equal(t, 33.4, UserQuotaUsage{RemainingFraction: 0.666}.UsagePercent())
	require.Equal(t, 66.7, UserQuotaUsage{RemainingFraction: 0.333}.UsagePercent())
	require.Equal(t, 0.0, UserQuotaUsage{RemainingFraction: 1}.UsagePercent())
	require.Equal(t, 100.0, UserQuotaUsage{RemainingFraction: 0}.UsagePercent())
	// 大配额池里少量请求：不能取整成 0，否则面板把「用了」显示成「没用」。
	require.Equal(t, 0.05, UserQuotaUsage{RemainingFraction: 0.9995}.UsagePercent())
}

func TestAggregateUserQuotaBuckets_Empty(t *testing.T) {
	require.Empty(t, AggregateUserQuotaBuckets(nil))
}
