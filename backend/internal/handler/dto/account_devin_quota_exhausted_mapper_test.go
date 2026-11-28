//go:build unit

package dto

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// Devin 额度耗尽停调必须作为独立字段投影到管理端 DTO，
// 让管理员不必从 schedulable/status 去猜上游额度是否可用。
func TestDevinQuotaExhaustedProjection(t *testing.T) {
	const marker = service.DevinUpstreamQuotaExhaustedMarker

	parked := func() *service.Account {
		return &service.Account{
			ID:       992,
			Platform: service.PlatformDevin,
			Status:   "active",
			// 管理员本地开关仍为 true，但上游额度耗尽已把账号停调。
			Schedulable:             true,
			TempUnschedulableReason: `{"until_unix":1,"matched_keyword":"` + marker + `"}`,
		}
	}

	t.Run("account detail", func(t *testing.T) {
		got := AccountFromServiceShallow(parked())
		require.NotNil(t, got)
		require.True(t, got.DevinQuotaExhausted)
	})

	t.Run("account list item", func(t *testing.T) {
		detail := AccountFromServiceShallow(parked())
		got := AccountListItemFromAccount(detail)
		require.NotNil(t, got)
		require.True(t, got.DevinQuotaExhausted)
	})

	t.Run("no marker stays false", func(t *testing.T) {
		account := parked()
		account.TempUnschedulableReason = `{"until_unix":1,"matched_keyword":"429"}`
		require.False(t, AccountFromServiceShallow(account).DevinQuotaExhausted)
	})

	t.Run("non-devin platform stays false", func(t *testing.T) {
		account := parked()
		account.Platform = service.PlatformOpenAI
		require.False(t, AccountFromServiceShallow(account).DevinQuotaExhausted)
	})
}
