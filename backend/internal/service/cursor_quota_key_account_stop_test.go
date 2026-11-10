//go:build unit

package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// c1p1-H（F3 技术债）：码级恢复后清除已过恢复点的陈旧停调。
//
// 旧实现 ClearStopsUnderRecoveredKey 的 WHERE 带了 TempUnschedulableUntilGT(time.Now())，
// 导致码级额度恢复后、until 已经过期的账号（停到 19:00、现在 19:15）永远清不掉 reason，
// 后台一直显示旧停调文本。本用例验证：清除【不】挑 until，且按前缀精确清、不误清账号级停调。
// ---------------------------------------------------------------------------

// TestClearStopsUnderRecoveredKey_ClearsExpiredAndFutureStops：码级 reason、until 分别落在
// 过去与未来的两个账号，调用后 until/reason 都应被清空，返回行数 = 2。
func TestClearStopsUnderRecoveredKey_ClearsExpiredAndFutureStops(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	t.Cleanup(func() {
		_, _ = client.Account.Delete().Where(account.QuotaKeyID(qk.ID)).Exec(context.Background())
		_ = client.QuotaKey.DeleteOneID(qk.ID).Exec(context.Background())
	})

	now := time.Now()
	// 两个码级停调账号：until 一个在未来、一个在过去。
	pastUntil := now.Add(-1 * time.Hour)
	futureUntil := now.Add(1 * time.Hour)
	pastReason := fmt.Sprintf("%s until %s", cursorQuotaKeyExhaustedReasonPrefix, pastUntil.Format("01-02 15:04"))
	futureReason := fmt.Sprintf("%s until %s", cursorQuotaKeyExhaustedReasonPrefix, futureUntil.Format("01-02 15:04"))

	mustCreateKeyLevelStoppedAccount(t, client, qk.ID, futureUntil, futureReason)
	mustCreateKeyLevelStoppedAccount(t, client, qk.ID, pastUntil, pastReason)

	svc := &QuotaKeyService{client: client}
	n, err := svc.ClearStopsUnderRecoveredKey(ctx, qk.ID)
	require.NoError(t, err)
	require.Equal(t, int64(2), n, "both key-level stops (future + expired) must be cleared")

	accs, qerr := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).All(ctx)
	require.NoError(t, qerr)
	require.Len(t, accs, 2)
	for _, a := range accs {
		require.Nil(t, a.TempUnschedulableUntil, "until must be cleared for key-level stopped account")
		require.Empty(t, a.TempUnschedulableReason, "reason must be cleared for key-level stopped account")
	}
}

// TestClearStopsUnderRecoveredKey_KeepsNonKeyLevelStop：带非码级 reason（账号级
// cursor account blocked by upstream）且 until 未来的账号，调用后必须原样保留（不被误清）。
func TestClearStopsUnderRecoveredKey_KeepsNonKeyLevelStop(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	t.Cleanup(func() {
		_, _ = client.Account.Delete().Where(account.QuotaKeyID(qk.ID)).Exec(context.Background())
		_ = client.QuotaKey.DeleteOneID(qk.ID).Exec(context.Background())
	})

	// 账号级停调（不同原因串前缀、且 until 未来）。
	keepUntil := time.Now().Add(1 * time.Hour)
	keepReason := "cursor account blocked by upstream until 11-10 20:00"
	mustCreateKeyLevelStoppedAccount(t, client, qk.ID, keepUntil, keepReason)

	svc := &QuotaKeyService{client: client}
	n, err := svc.ClearStopsUnderRecoveredKey(ctx, qk.ID)
	require.NoError(t, err)
	require.Equal(t, int64(0), n, "account-level stop must NOT be cleared (prefix mismatch)")

	accs, qerr := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).All(ctx)
	require.NoError(t, qerr)
	require.Len(t, accs, 1)
	require.NotNil(t, accs[0].TempUnschedulableUntil, "account-level until must be preserved")
	require.NotNil(t, accs[0].TempUnschedulableReason, "account-level reason must be preserved")
	require.Equal(t, keepReason, *accs[0].TempUnschedulableReason, "account-level reason must be preserved")
}

// mustCreateKeyLevelStoppedAccount 落库一个带指定码级/账号级停调原因与到期时刻的账号。
func mustCreateKeyLevelStoppedAccount(t *testing.T, client *ent.Client, quotaKeyID int64, until time.Time, reason string) *ent.Account {
	t.Helper()
	acc, err := client.Account.Create().
		SetName(fmt.Sprintf("c1p1h-%d-%d", quotaKeyID, time.Now().UnixNano())).
		SetPlatform("cursor").
		SetType("oauth").
		SetCredentials(map[string]any{"k": "v"}).
		SetQuotaKeyID(quotaKeyID).
		SetTempUnschedulableUntil(until).
		SetTempUnschedulableReason(reason).
		Save(context.Background())
	require.NoError(t, err)
	return acc
}
