//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCursorQuotaObservationUsesSourceUnit(t *testing.T) {
	t.Run("ShareAI 份额制以 Max-Used 判定，不要求 USD 今日余额", func(t *testing.T) {
		available, err := cursorQuotaObservationAvailable(&KeyQuotaLookupInfo{Max: 100, Used: 80})
		require.NoError(t, err)
		require.True(t, available)

		available, err = cursorQuotaObservationAvailable(&KeyQuotaLookupInfo{Max: 100, Used: 95})
		require.NoError(t, err)
		require.False(t, available)
	})

	t.Run("kn-cy usd_cent 只认今日剩余，缺字段失败关闭", func(t *testing.T) {
		todayRemaining := 50
		available, err := cursorQuotaObservationAvailable(&KeyQuotaLookupInfo{
			Max: 1000, Used: 0, Unit: "usd_cent", TodayRemainingCent: &todayRemaining,
		})
		require.NoError(t, err)
		require.True(t, available)

		_, err = cursorQuotaObservationAvailable(&KeyQuotaLookupInfo{Max: 1000, Used: 0, Unit: "usd_cent"})
		require.EqualError(t, err, "cursor quota observation missing today_remaining")
	})

	t.Run("未知单位失败关闭", func(t *testing.T) {
		_, err := cursorQuotaObservationAvailable(&KeyQuotaLookupInfo{Max: 100, Used: 0, Unit: "credits"})
		require.EqualError(t, err, `cursor quota observation has unknown unit "credits"`)
	})
}
