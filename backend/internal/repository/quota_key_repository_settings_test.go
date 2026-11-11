//go:build unit

package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/pkg/credcrypt"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// 指针/双指针构造辅助（UpdateQuotaKeySettingsInput 用 **int 表达三态）。
// ============================================================================

func ptrInt(i int) *int           { return &i }
func ptrFloat(f float64) *float64 { return &f }

// dblInt 返回 **int：外层非 nil、内层非 nil（指向给定值）。
func dblInt(i int) **int {
	p := &i
	return &p
}

// dblNilInt 返回 **int：外层非 nil、内层 nil（表达"清空"三态）。
func dblNilInt() **int {
	var inner *int
	return &inner
}

// ============================================================================
// 裸 SQL 读取辅助（绕过 ent 软删拦截，用于断言"软删账号不被触碰"）。
// ============================================================================

func qkrScanExpiresAt(t *testing.T, client *ent.Client, id int64) *time.Time {
	t.Helper()
	ctx := context.Background()
	rows, err := client.QueryContext(ctx, "SELECT expires_at FROM accounts WHERE id = $1", id)
	require.NoError(t, err)
	defer rows.Close()
	require.True(t, rows.Next(), "account row must exist, id=%d", id)
	var exp *time.Time
	require.NoError(t, rows.Scan(&exp))
	return exp
}

func qkrScanConcurrency(t *testing.T, client *ent.Client, id int64) int {
	t.Helper()
	ctx := context.Background()
	rows, err := client.QueryContext(ctx, "SELECT concurrency FROM accounts WHERE id = $1", id)
	require.NoError(t, err)
	defer rows.Close()
	require.True(t, rows.Next(), "account row must exist, id=%d", id)
	var c int
	require.NoError(t, rows.Scan(&c))
	return c
}

// qkrCreateAccountWithCreds 创建一条归属给定激活码、指定平台、带给定 credentials 的账号。
func qkrCreateAccountWithCreds(t *testing.T, client *ent.Client, quotaKeyID int64, platform string, creds map[string]any) *ent.Account {
	t.Helper()
	ctx := context.Background()
	uid := fmt.Sprintf("%d", time.Now().UnixNano())
	a, err := client.Account.Create().
		SetName("acc-" + uid).
		SetPlatform(platform).
		SetType("oauth").
		SetCredentials(creds).
		SetQuotaKeyID(quotaKeyID).
		Save(ctx)
	require.NoError(t, err)
	return a
}

// ============================================================================
// Create：B 批上移字段落地（仅 quota_keys，不碰 accounts）。
// ============================================================================

func TestQuotaKeyRepository_Create_SetsSettingsFields(t *testing.T) {
	client := quotaKeyRepositoryTestClient(t)
	ctx := context.Background()

	mm := map[string]string{"gpt-4": "gpt-4o"}
	repo := NewQuotaKeyRepository(client)
	qk, err := repo.Create(ctx, service.CreateQuotaKeyInput{
		CodeEncrypted:  "enc:v1:create-settings-" + fmt.Sprintf("%d", time.Now().UnixNano()),
		CodeMAC:        "mac",
		CodeLast4:      "abcd",
		Status:         domain.QuotaKeyStatusActive,
		Exhausted:      false,
		Concurrency:    ptrInt(7),
		LoadFactor:     ptrInt(50),
		RateMultiplier: ptrFloat(2.5),
		ModelMapping:   mm,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.QuotaKey.DeleteOneID(qk.ID).Exec(ctx) })

	// 码级权威值落地。
	require.NotNil(t, qk.Concurrency)
	require.Equal(t, 7, *qk.Concurrency)
	require.NotNil(t, qk.LoadFactor)
	require.Equal(t, 50, *qk.LoadFactor)
	require.Equal(t, 2.5, qk.RateMultiplier)
	require.Equal(t, mm, qk.ModelMapping)
}

func TestQuotaKeyRepository_Create_IgnoresZeroAndNegativeSettings(t *testing.T) {
	client := quotaKeyRepositoryTestClient(t)
	ctx := context.Background()

	repo := NewQuotaKeyRepository(client)
	qk, err := repo.Create(ctx, service.CreateQuotaKeyInput{
		CodeEncrypted:  "enc:v1:create-zero-" + fmt.Sprintf("%d", time.Now().UnixNano()),
		CodeMAC:        "mac",
		CodeLast4:      "abcd",
		Status:         domain.QuotaKeyStatusActive,
		Concurrency:    ptrInt(0),           // >0 才落地：0 不写，沿用列默认 NULL
		LoadFactor:     ptrInt(0),           // >0 才落地
		RateMultiplier: ptrFloat(-1),        // >=0 才落地
		ModelMapping:   map[string]string{}, // len==0 不落地
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.QuotaKey.DeleteOneID(qk.ID).Exec(ctx) })

	require.Nil(t, qk.Concurrency, "concurrency=0 must NOT be persisted")
	require.Nil(t, qk.LoadFactor, "load_factor=0 must NOT be persisted")
	// rate_multiplier 为 NOT NULL DEFAULT 1.0，负值时按"未设置"处理，回落默认 1.0。
	require.Equal(t, 1.0, qk.RateMultiplier)
	require.Nil(t, qk.ModelMapping, "empty model_mapping must NOT be persisted")
}

// ============================================================================
// UpdateSettingsAndSyncAccounts：三态同步 + 模型映射进 credentials。
// ============================================================================

func TestQuotaKeyRepository_UpdateSettingsAndSyncAccounts_ConcurrencyClearedRestoresDefault3(t *testing.T) {
	client := quotaKeyRepositoryTestClient(t)
	ctx := context.Background()
	qkr := qkrRepo(t, client)
	qkrEnsureSchedulerOutbox(t, client)
	_, _ = client.ExecContext(ctx, "DELETE FROM scheduler_outbox")

	qk := qkrCreateQuotaKey(t, client, domain.QuotaKeyStatusActive, nil)
	acc := qkrCreateAccount(t, client, qk.ID, domain.PlatformCursor)
	t.Cleanup(func() {
		_, _ = client.Account.Delete().Where(account.IDEQ(acc.ID)).Exec(ctx)
		_ = client.QuotaKey.DeleteOneID(qk.ID).Exec(ctx)
	})

	// 清空 concurrency（内层 nil）→ 账号恢复默认 3。
	settings := service.UpdateQuotaKeySettingsInput{Concurrency: dblNilInt()}
	runInTx(t, client, func(tx *ent.Tx) error {
		return qkr.UpdateSettingsAndSyncAccounts(ctx, tx, qk.ID, settings)
	})

	got, err := client.Account.Get(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, 3, got.Concurrency, "cleared concurrency must restore account default 3")

	// 受影响账号入队 outbox。
	outboxIDs := qkrOutboxAccountIDs(t, client, qkrEventAccountChanged)
	require.Contains(t, outboxIDs, acc.ID)
}

func TestQuotaKeyRepository_UpdateSettingsAndSyncAccounts_ConcurrencySetPropagates(t *testing.T) {
	client := quotaKeyRepositoryTestClient(t)
	ctx := context.Background()
	qkr := qkrRepo(t, client)
	qkrEnsureSchedulerOutbox(t, client)
	_, _ = client.ExecContext(ctx, "DELETE FROM scheduler_outbox")

	qk := qkrCreateQuotaKey(t, client, domain.QuotaKeyStatusActive, nil)
	acc := qkrCreateAccount(t, client, qk.ID, domain.PlatformCursor)
	t.Cleanup(func() {
		_, _ = client.Account.Delete().Where(account.IDEQ(acc.ID)).Exec(ctx)
		_ = client.QuotaKey.DeleteOneID(qk.ID).Exec(ctx)
	})

	settings := service.UpdateQuotaKeySettingsInput{Concurrency: dblInt(11)}
	runInTx(t, client, func(tx *ent.Tx) error {
		return qkr.UpdateSettingsAndSyncAccounts(ctx, tx, qk.ID, settings)
	})

	got, err := client.Account.Get(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, 11, got.Concurrency)
}

func TestQuotaKeyRepository_UpdateSettingsAndSyncAccounts_LoadFactorCleared(t *testing.T) {
	client := quotaKeyRepositoryTestClient(t)
	ctx := context.Background()
	qkr := qkrRepo(t, client)
	qkrEnsureSchedulerOutbox(t, client)
	_, _ = client.ExecContext(ctx, "DELETE FROM scheduler_outbox")

	qk := qkrCreateQuotaKey(t, client, domain.QuotaKeyStatusActive, nil)
	acc := qkrCreateAccount(t, client, qk.ID, domain.PlatformCursor)
	t.Cleanup(func() {
		_, _ = client.Account.Delete().Where(account.IDEQ(acc.ID)).Exec(ctx)
		_ = client.QuotaKey.DeleteOneID(qk.ID).Exec(ctx)
	})

	// 清空 load_factor（内层 nil）→ 账号 load_factor 置 NULL。
	settings := service.UpdateQuotaKeySettingsInput{LoadFactor: dblNilInt()}
	runInTx(t, client, func(tx *ent.Tx) error {
		return qkr.UpdateSettingsAndSyncAccounts(ctx, tx, qk.ID, settings)
	})

	got, err := client.Account.Get(ctx, acc.ID)
	require.NoError(t, err)
	require.Nil(t, got.LoadFactor, "cleared load_factor must be NULL on the account")
}

func TestQuotaKeyRepository_UpdateSettingsAndSyncAccounts_LoadFactorSetPropagates(t *testing.T) {
	client := quotaKeyRepositoryTestClient(t)
	ctx := context.Background()
	qkr := qkrRepo(t, client)
	qkrEnsureSchedulerOutbox(t, client)
	_, _ = client.ExecContext(ctx, "DELETE FROM scheduler_outbox")

	qk := qkrCreateQuotaKey(t, client, domain.QuotaKeyStatusActive, nil)
	acc := qkrCreateAccount(t, client, qk.ID, domain.PlatformCursor)
	t.Cleanup(func() {
		_, _ = client.Account.Delete().Where(account.IDEQ(acc.ID)).Exec(ctx)
		_ = client.QuotaKey.DeleteOneID(qk.ID).Exec(ctx)
	})

	settings := service.UpdateQuotaKeySettingsInput{LoadFactor: dblInt(80)}
	runInTx(t, client, func(tx *ent.Tx) error {
		return qkr.UpdateSettingsAndSyncAccounts(ctx, tx, qk.ID, settings)
	})

	got, err := client.Account.Get(ctx, acc.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LoadFactor)
	require.Equal(t, 80, *got.LoadFactor)
}

func TestQuotaKeyRepository_UpdateSettingsAndSyncAccounts_RateMultiplierZero(t *testing.T) {
	client := quotaKeyRepositoryTestClient(t)
	ctx := context.Background()
	qkr := qkrRepo(t, client)
	qkrEnsureSchedulerOutbox(t, client)
	_, _ = client.ExecContext(ctx, "DELETE FROM scheduler_outbox")

	qk := qkrCreateQuotaKey(t, client, domain.QuotaKeyStatusActive, nil)
	acc := qkrCreateAccount(t, client, qk.ID, domain.PlatformCursor)
	t.Cleanup(func() {
		_, _ = client.Account.Delete().Where(account.IDEQ(acc.ID)).Exec(ctx)
		_ = client.QuotaKey.DeleteOneID(qk.ID).Exec(ctx)
	})

	// rate_multiplier=0 是合法值（允许为 0），必须传播到账号。
	settings := service.UpdateQuotaKeySettingsInput{RateMultiplier: ptrFloat(0)}
	runInTx(t, client, func(tx *ent.Tx) error {
		return qkr.UpdateSettingsAndSyncAccounts(ctx, tx, qk.ID, settings)
	})

	got, err := client.Account.Get(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, float64(0), got.RateMultiplier)
}

func TestQuotaKeyRepository_UpdateSettingsAndSyncAccounts_ModelMappingSyncPreservesEncryption(t *testing.T) {
	client := quotaKeyRepositoryTestClient(t)
	ctx := context.Background()
	qkr := qkrRepo(t, client)
	qkrEnsureSchedulerOutbox(t, client)
	_, _ = client.ExecContext(ctx, "DELETE FROM scheduler_outbox")

	qk := qkrCreateQuotaKey(t, client, domain.QuotaKeyStatusActive, nil)
	acc := qkrCreateAccountWithCreds(t, client, qk.ID, domain.PlatformCursor, map[string]any{
		"access_token": "at-secret",
		"api_key":      "sk-1",
		"base_url":     "https://example.com",
	})
	t.Cleanup(func() {
		_, _ = client.Account.Delete().Where(account.IDEQ(acc.ID)).Exec(ctx)
		_ = client.QuotaKey.DeleteOneID(qk.ID).Exec(ctx)
	})

	mm := map[string]string{"gpt-4": "gpt-4o", "gpt-3.5": "gpt-3.5-turbo"}
	withTestCredCipher(t, func() {
		settings := service.UpdateQuotaKeySettingsInput{
			ModelMapping:    mm,
			ModelMappingSet: true,
		}
		runInTx(t, client, func(tx *ent.Tx) error {
			return qkr.UpdateSettingsAndSyncAccounts(ctx, tx, qk.ID, settings)
		})

		got, err := client.Account.Get(ctx, acc.ID)
		require.NoError(t, err)

		// 敏感子键仍保持密文形态（未被破坏）。
		require.True(t, credcrypt.IsEncrypted(got.Credentials["access_token"].(string)), "access_token must remain encrypted")
		require.True(t, credcrypt.IsEncrypted(got.Credentials["api_key"].(string)), "api_key must remain encrypted")

		// 模型映射以明文写入（非敏感键）。
		mmStored, ok := got.Credentials["model_mapping"].(map[string]any)
		require.True(t, ok, "model_mapping must be stored as a nested map")
		require.Equal(t, "gpt-4o", mmStored["gpt-4"])
		require.Equal(t, "gpt-3.5-turbo", mmStored["gpt-3.5"])

		// 解密后整体一致：原有键保留 + 模型映射正确。
		plain, err := decryptCredentialsMap(got.Credentials)
		require.NoError(t, err)
		require.Equal(t, "at-secret", plain["access_token"])
		require.Equal(t, "sk-1", plain["api_key"])
		require.Equal(t, "https://example.com", plain["base_url"])
		require.Equal(t, toAnyMap(mm), plain["model_mapping"])

		// credentials_mac 随明文重算并保持完整（证明未绕过 prepareCredentialsForStorage）。
		require.NotEmpty(t, got.CredentialsMAC)
		mac, err := credentialsMACOf(plain)
		require.NoError(t, err)
		require.Equal(t, mac, got.CredentialsMAC)
	})

	// 受影响账号入队 outbox。
	outboxIDs := qkrOutboxAccountIDs(t, client, qkrEventAccountChanged)
	require.Contains(t, outboxIDs, acc.ID)
}

func TestQuotaKeyRepository_UpdateSettingsAndSyncAccounts_EmptyModelMappingRejected(t *testing.T) {
	client := quotaKeyRepositoryTestClient(t)
	ctx := context.Background()
	qkr := qkrRepo(t, client)
	qkrEnsureSchedulerOutbox(t, client)
	_, _ = client.ExecContext(ctx, "DELETE FROM scheduler_outbox")

	qk := qkrCreateQuotaKey(t, client, domain.QuotaKeyStatusActive, nil)
	acc := qkrCreateAccount(t, client, qk.ID, domain.PlatformCursor)
	t.Cleanup(func() {
		_, _ = client.Account.Delete().Where(account.IDEQ(acc.ID)).Exec(ctx)
		_ = client.QuotaKey.DeleteOneID(qk.ID).Exec(ctx)
	})

	// ModelMappingSet=true 但空映射：defense-in-depth 拒绝落库。
	err := runInTxErr(t, client, func(tx *ent.Tx) error {
		return qkr.UpdateSettingsAndSyncAccounts(ctx, tx, qk.ID, service.UpdateQuotaKeySettingsInput{
			ModelMappingSet: true,
			ModelMapping:    map[string]string{},
		})
	})
	require.Error(t, err, "empty model_mapping must be refused")

	// 拒绝后事务回滚：账号与激活码不被改动（model_mapping 未写到 quota_keys）。
	gotQK, gerr := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, gerr)
	require.Nil(t, gotQK.ModelMapping, "quota key must remain unchanged after rejection")
	gotAcc, gerr := client.Account.Get(ctx, acc.ID)
	require.NoError(t, gerr)
	_, hasMM := gotAcc.Credentials["model_mapping"]
	require.False(t, hasMM, "account credentials must remain unchanged after rejection")
}

// TestQuotaKeyRepository_UpdateSettingsAndSyncAccounts_SoftDeletedAccountUntouched：
// 同事务同步只覆盖未软删账号；已软删账号的并发/负载因子不被改写。
func TestQuotaKeyRepository_UpdateSettingsAndSyncAccounts_SoftDeletedAccountUntouched(t *testing.T) {
	client := quotaKeyRepositoryTestClient(t)
	ctx := context.Background()
	qkr := qkrRepo(t, client)
	qkrEnsureSchedulerOutbox(t, client)
	_, _ = client.ExecContext(ctx, "DELETE FROM scheduler_outbox")

	qk := qkrCreateQuotaKey(t, client, domain.QuotaKeyStatusActive, nil)
	accAlive := qkrCreateAccount(t, client, qk.ID, domain.PlatformCursor)
	accDeleted := qkrCreateAccount(t, client, qk.ID, domain.PlatformCursor)
	// 先给已软删账号一个与"将被同步值"不同的并发，便于事后断言未被改写。
	_, err := client.Account.UpdateOneID(accDeleted.ID).SetConcurrency(99).Save(ctx)
	require.NoError(t, err)
	_, err = client.Account.UpdateOneID(accDeleted.ID).SetDeletedAt(time.Now()).Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = client.Account.Delete().Where(account.IDIn(accAlive.ID, accDeleted.ID)).Exec(ctx)
		_ = client.QuotaKey.DeleteOneID(qk.ID).Exec(ctx)
	})

	settings := service.UpdateQuotaKeySettingsInput{Concurrency: dblInt(5)}
	runInTx(t, client, func(tx *ent.Tx) error {
		return qkr.UpdateSettingsAndSyncAccounts(ctx, tx, qk.ID, settings)
	})

	// 存活账号被同步为 5。
	gotAlive, err := client.Account.Get(ctx, accAlive.ID)
	require.NoError(t, err)
	require.Equal(t, 5, gotAlive.Concurrency)

	// 已软删账号并发保持 99（未被触碰，绕过 ent 软删拦截用裸 SQL 读）。
	require.Equal(t, 99, qkrScanConcurrency(t, client, accDeleted.ID))

	// outbox 仅含存活账号。
	outboxIDs := qkrOutboxAccountIDs(t, client, qkrEventAccountChanged)
	require.Contains(t, outboxIDs, accAlive.ID)
	require.NotContains(t, outboxIDs, accDeleted.ID)
}

// ============================================================================
// 失败关闭：tx 必填。
// ============================================================================

func TestQuotaKeyRepository_UpdateSettingsAndSyncAccounts_TxRequiredFailsClosed(t *testing.T) {
	client := quotaKeyRepositoryTestClient(t)
	ctx := context.Background()
	qkr := qkrRepo(t, client)
	require.Error(t, qkr.UpdateSettingsAndSyncAccounts(ctx, nil, 1, service.UpdateQuotaKeySettingsInput{}),
		"UpdateSettingsAndSyncAccounts must fail closed without the caller's transaction")
}

func TestQuotaKeyRepository_SyncAccountsExpiry_TxRequiredFailsClosed(t *testing.T) {
	client := quotaKeyRepositoryTestClient(t)
	ctx := context.Background()
	qkr := qkrRepo(t, client)
	require.Error(t, qkr.SyncAccountsExpiry(ctx, nil, 1, timePtr(time.Now())),
		"SyncAccountsExpiry must fail closed without the caller's transaction")
}

// ============================================================================
// UpdateUsageSnapshot / UpdateMeta / SyncAccountsExpiry：账号 expires_at 跟随。
// ============================================================================

func TestQuotaKeyRepository_UpdateUsageSnapshot_SyncsAccountExpiry(t *testing.T) {
	client := quotaKeyRepositoryTestClient(t)
	ctx := context.Background()
	qkr := qkrRepo(t, client)
	qkrEnsureSchedulerOutbox(t, client)
	_, _ = client.ExecContext(ctx, "DELETE FROM scheduler_outbox")

	qk := qkrCreateQuotaKey(t, client, domain.QuotaKeyStatusActive, nil)
	accAlive := qkrCreateAccount(t, client, qk.ID, domain.PlatformCursor)
	// 已软删账号，初始给一个过去时间，用于证明同步不触碰它。
	accDeleted := qkrCreateAccount(t, client, qk.ID, domain.PlatformCursor)
	past := time.Now().Add(-48 * time.Hour)
	_, err := client.Account.UpdateOneID(accDeleted.ID).SetExpiresAt(past).SetDeletedAt(time.Now()).Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = client.Account.Delete().Where(account.IDIn(accAlive.ID, accDeleted.ID)).Exec(ctx)
		_ = client.QuotaKey.DeleteOneID(qk.ID).Exec(ctx)
	})

	future := time.Now().Add(48 * time.Hour)
	expiresOuter := &future
	runInTx(t, client, func(tx *ent.Tx) error {
		return qkr.UpdateUsageSnapshot(ctx, tx, qk.ID, []byte(`{"ok":true}`), &expiresOuter)
	})

	// 存活账号 expires_at 跟随激活码。
	gotAlive, err := client.Account.Get(ctx, accAlive.ID)
	require.NoError(t, err)
	require.NotNil(t, gotAlive.ExpiresAt)
	require.WithinDuration(t, future, *gotAlive.ExpiresAt, time.Second)

	// 已软删账号保持原值（未被触碰）。
	require.WithinDuration(t, past, *qkrScanExpiresAt(t, client, accDeleted.ID), time.Second)

	// tx 内同步 → outbox 仅含存活账号。
	outboxIDs := qkrOutboxAccountIDs(t, client, qkrEventAccountChanged)
	require.Contains(t, outboxIDs, accAlive.ID)
	require.NotContains(t, outboxIDs, accDeleted.ID)
}

func TestQuotaKeyRepository_UpdateMeta_SyncsAccountExpiry(t *testing.T) {
	client := quotaKeyRepositoryTestClient(t)
	ctx := context.Background()
	qkr := qkrRepo(t, client)
	qkrEnsureSchedulerOutbox(t, client)
	_, _ = client.ExecContext(ctx, "DELETE FROM scheduler_outbox")

	qk := qkrCreateQuotaKey(t, client, domain.QuotaKeyStatusActive, nil)
	accAlive := qkrCreateAccount(t, client, qk.ID, domain.PlatformCursor)
	accDeleted := qkrCreateAccount(t, client, qk.ID, domain.PlatformCursor)
	past := time.Now().Add(-48 * time.Hour)
	_, err := client.Account.UpdateOneID(accDeleted.ID).SetExpiresAt(past).SetDeletedAt(time.Now()).Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = client.Account.Delete().Where(account.IDIn(accAlive.ID, accDeleted.ID)).Exec(ctx)
		_ = client.QuotaKey.DeleteOneID(qk.ID).Exec(ctx)
	})

	// 先设置到期 → 账号同步。
	future := time.Now().Add(48 * time.Hour)
	expiresOuter := &future
	runInTx(t, client, func(tx *ent.Tx) error {
		return qkr.UpdateMeta(ctx, tx, qk.ID, &expiresOuter, nil)
	})
	gotAlive, err := client.Account.Get(ctx, accAlive.ID)
	require.NoError(t, err)
	require.WithinDuration(t, future, *gotAlive.ExpiresAt, time.Second)

	// 再清空到期（外层非 nil、内层 nil）→ 账号 expires_at 一并清空，软删账号仍不动。
	var clearExpires **time.Time
	var innerNil *time.Time
	clearExpires = &innerNil
	runInTx(t, client, func(tx *ent.Tx) error {
		return qkr.UpdateMeta(ctx, tx, qk.ID, clearExpires, nil)
	})
	gotAlive2, err := client.Account.Get(ctx, accAlive.ID)
	require.NoError(t, err)
	require.Nil(t, gotAlive2.ExpiresAt, "account expires_at must be cleared when quota key expires_at is cleared")
	require.WithinDuration(t, past, *qkrScanExpiresAt(t, client, accDeleted.ID), time.Second,
		"soft-deleted account must remain untouched")
}

func TestQuotaKeyRepository_SyncAccountsExpiry_SyncsAndEnqueues(t *testing.T) {
	client := quotaKeyRepositoryTestClient(t)
	ctx := context.Background()
	qkr := qkrRepo(t, client)
	qkrEnsureSchedulerOutbox(t, client)
	_, _ = client.ExecContext(ctx, "DELETE FROM scheduler_outbox")

	qk := qkrCreateQuotaKey(t, client, domain.QuotaKeyStatusActive, nil)
	acc := qkrCreateAccount(t, client, qk.ID, domain.PlatformCursor)
	t.Cleanup(func() {
		_, _ = client.Account.Delete().Where(account.IDEQ(acc.ID)).Exec(ctx)
		_ = client.QuotaKey.DeleteOneID(qk.ID).Exec(ctx)
	})

	future := time.Now().Add(48 * time.Hour)
	runInTx(t, client, func(tx *ent.Tx) error {
		return qkr.SyncAccountsExpiry(ctx, tx, qk.ID, &future)
	})

	got, err := client.Account.Get(ctx, acc.ID)
	require.NoError(t, err)
	require.WithinDuration(t, future, *got.ExpiresAt, time.Second)

	outboxIDs := qkrOutboxAccountIDs(t, client, qkrEventAccountChanged)
	require.Contains(t, outboxIDs, acc.ID)
}

// runInTxErr 与 runInTx 类似，但返回 fn 的错误（不自动 Fatal），便于断言拒绝路径。
func runInTxErr(t *testing.T, client *ent.Client, fn func(tx *ent.Tx) error) error {
	t.Helper()
	tx, err := client.Tx(context.Background())
	require.NoError(t, err)
	ferr := fn(tx)
	if ferr != nil {
		_ = tx.Rollback()
		return ferr
	}
	return tx.Commit()
}

// ============================================================================
// B3 分组同步：UpdateSettingsAndSyncAccounts 的 GroupIDs 三态。
// ============================================================================

const qkrEventAccountGroupsChanged = service.SchedulerOutboxEventAccountGroupsChanged

// qkrUID 生成进程内唯一后缀，用于分组名等需要唯一约束的对象。
func qkrUID() string { return fmt.Sprintf("%d", time.Now().UnixNano()) }

// qkrCreateGroup 创建一条测试分组（未软删，满足 lockLiveGroups 的 deleted_at IS NULL 过滤）。
func qkrCreateGroup(t *testing.T, client *ent.Client, name string) *ent.Group {
	t.Helper()
	ctx := context.Background()
	g, err := client.Group.Create().
		SetName(name).
		SetPlatform(service.PlatformAnthropic).
		SetStatus(service.StatusActive).
		Save(ctx)
	require.NoError(t, err)
	return g
}

// qkrScanGroupIdsRaw 读取 quota_keys.group_ids 列的裸文本（绕过 ent 软删拦截与 JSON 解码），
// 用于断言「空切片落 [] 而非 NULL」。NULL 列返回字面量 "NULL"。
func qkrScanGroupIdsRaw(t *testing.T, client *ent.Client, id int64) string {
	t.Helper()
	ctx := context.Background()
	rows, err := client.QueryContext(ctx, "SELECT group_ids::text FROM quota_keys WHERE id = $1", id)
	require.NoError(t, err)
	defer rows.Close()
	require.True(t, rows.Next(), "quota key row must exist, id=%d", id)
	var s *string
	require.NoError(t, rows.Scan(&s))
	if s == nil {
		return "NULL"
	}
	return *s
}

// qkrAccountGroupRow 是一行 account_groups 绑定（用于断言 priority 与去重）。
type qkrAccountGroupRow struct {
	GroupID  int64
	Priority int
}

// qkrAccountGroupRows 读取某账号的全部 account_groups 绑定，按 priority 升序。
func qkrAccountGroupRows(t *testing.T, client *ent.Client, accountID int64) []qkrAccountGroupRow {
	t.Helper()
	ctx := context.Background()
	rows, err := client.QueryContext(ctx, "SELECT group_id, priority FROM account_groups WHERE account_id = $1 ORDER BY priority", accountID)
	require.NoError(t, err)
	defer rows.Close()
	var out []qkrAccountGroupRow
	for rows.Next() {
		var r qkrAccountGroupRow
		require.NoError(t, rows.Scan(&r.GroupID, &r.Priority))
		out = append(out, r)
	}
	return out
}

// qkrOutboxGroupChangedPayload 读取某账号的 account_groups_changed outbox payload，
// 返回其中的 group_ids（用于断言「旧∪新」去重并集）。
func qkrOutboxGroupChangedPayload(t *testing.T, client *ent.Client, accountID int64) []int64 {
	t.Helper()
	ctx := context.Background()
	rows, err := client.QueryContext(ctx, "SELECT payload FROM scheduler_outbox WHERE event_type = $1 AND account_id = $2", qkrEventAccountGroupsChanged, accountID)
	require.NoError(t, err)
	defer rows.Close()
	require.True(t, rows.Next(), "expected account_groups_changed outbox for account %d", accountID)
	var raw []byte
	require.NoError(t, rows.Scan(&raw))
	var p struct {
		GroupIDs []int64 `json:"group_ids"`
	}
	require.NoError(t, json.Unmarshal(raw, &p))
	return p.GroupIDs
}

// TestQuotaKeyRepositoryGroupIDs_OverwriteReplacesBindingsAndEnqueues：
// GroupIDs=[g1,g2] → 码级列与名下账号 account_groups 均更新；旧分组被删；
// priority 为 1、2；outbox 有 account_groups_changed 事件且 payload 含旧∪新。
func TestQuotaKeyRepositoryGroupIDs_OverwriteReplacesBindingsAndEnqueues(t *testing.T) {
	client := quotaKeyRepositoryTestClient(t)
	ctx := context.Background()
	qkr := qkrRepo(t, client)
	qkrEnsureSchedulerOutbox(t, client)
	_, _ = client.ExecContext(ctx, "DELETE FROM scheduler_outbox")

	g0 := qkrCreateGroup(t, client, "grp-old-"+qkrUID())
	g1 := qkrCreateGroup(t, client, "grp-new1-"+qkrUID())
	g2 := qkrCreateGroup(t, client, "grp-new2-"+qkrUID())

	qk := qkrCreateQuotaKey(t, client, domain.QuotaKeyStatusActive, nil)
	acc := qkrCreateAccount(t, client, qk.ID, domain.PlatformCursor)
	// 预置旧绑定：acc -> g0（priority 1）。
	_, err := client.AccountGroup.Create().SetAccountID(acc.ID).SetGroupID(g0.ID).SetPriority(1).Save(ctx)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = client.ExecContext(ctx, "DELETE FROM account_groups WHERE account_id = $1", acc.ID)
		_, _ = client.Group.Delete().Where(group.IDIn(g0.ID, g1.ID, g2.ID)).Exec(ctx)
		_, _ = client.Account.Delete().Where(account.IDEQ(acc.ID)).Exec(ctx)
		_ = client.QuotaKey.DeleteOneID(qk.ID).Exec(ctx)
	})

	newIDs := []int64{g1.ID, g2.ID}
	runInTx(t, client, func(tx *ent.Tx) error {
		return qkr.UpdateSettingsAndSyncAccounts(ctx, tx, qk.ID, service.UpdateQuotaKeySettingsInput{
			GroupIDs: &newIDs,
		})
	})

	// 码级列 = [g1, g2]（顺序保持）。Postgres jsonb::text 会在逗号后加空格，故解析后比较。
	rawGroupIDs := qkrScanGroupIdsRaw(t, client, qk.ID)
	var gotGroupIDs []int64
	require.NoError(t, json.Unmarshal([]byte(rawGroupIDs), &gotGroupIDs))
	require.Equal(t, []int64{g1.ID, g2.ID}, gotGroupIDs)

	// 旧绑定 g0 被删；新绑定 g1(priority 1)、g2(priority 2) 写入。
	rows := qkrAccountGroupRows(t, client, acc.ID)
	require.Len(t, rows, 2, "old binding must be removed and two new bindings created")
	require.Equal(t, g1.ID, rows[0].GroupID)
	require.Equal(t, 1, rows[0].Priority)
	require.Equal(t, g2.ID, rows[1].GroupID)
	require.Equal(t, 2, rows[1].Priority)

	// outbox：account_groups_changed 事件，payload 含旧∪新 = {g0, g1, g2}。
	outboxIDs := qkrOutboxAccountIDs(t, client, qkrEventAccountGroupsChanged)
	require.Contains(t, outboxIDs, acc.ID)
	payload := qkrOutboxGroupChangedPayload(t, client, acc.ID)
	require.ElementsMatch(t, []int64{g0.ID, g1.ID, g2.ID}, payload,
		"payload must be the deduped union of old and new group ids")
}

// TestQuotaKeyRepositoryGroupIDs_EmptySliceClearsBindings：
// GroupIDs 指向空切片 → 账号绑定全部清空、码级列为 []（非 NULL）。
func TestQuotaKeyRepositoryGroupIDs_EmptySliceClearsBindings(t *testing.T) {
	client := quotaKeyRepositoryTestClient(t)
	ctx := context.Background()
	qkr := qkrRepo(t, client)
	qkrEnsureSchedulerOutbox(t, client)
	_, _ = client.ExecContext(ctx, "DELETE FROM scheduler_outbox")

	g0 := qkrCreateGroup(t, client, "grp-empty-old-"+qkrUID())
	qk := qkrCreateQuotaKey(t, client, domain.QuotaKeyStatusActive, nil)
	acc := qkrCreateAccount(t, client, qk.ID, domain.PlatformCursor)
	_, err := client.AccountGroup.Create().SetAccountID(acc.ID).SetGroupID(g0.ID).SetPriority(1).Save(ctx)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = client.ExecContext(ctx, "DELETE FROM account_groups WHERE account_id = $1", acc.ID)
		_, _ = client.Group.Delete().Where(group.IDIn(g0.ID)).Exec(ctx)
		_, _ = client.Account.Delete().Where(account.IDEQ(acc.ID)).Exec(ctx)
		_ = client.QuotaKey.DeleteOneID(qk.ID).Exec(ctx)
	})

	empty := []int64{}
	runInTx(t, client, func(tx *ent.Tx) error {
		return qkr.UpdateSettingsAndSyncAccounts(ctx, tx, qk.ID, service.UpdateQuotaKeySettingsInput{
			GroupIDs: &empty,
		})
	})

	// 码级列必须写成 [] 而非 NULL。
	require.Equal(t, "[]", qkrScanGroupIdsRaw(t, client, qk.ID))
	// 账号绑定全部清空。
	require.Empty(t, qkrAccountGroupRows(t, client, acc.ID))
}

// TestQuotaKeyRepositoryGroupIDs_NilLeavesUnchanged：
// GroupIDs == nil → 分组与绑定均不变。
func TestQuotaKeyRepositoryGroupIDs_NilLeavesUnchanged(t *testing.T) {
	client := quotaKeyRepositoryTestClient(t)
	ctx := context.Background()
	qkr := qkrRepo(t, client)
	qkrEnsureSchedulerOutbox(t, client)
	_, _ = client.ExecContext(ctx, "DELETE FROM scheduler_outbox")

	g0 := qkrCreateGroup(t, client, "grp-nil-old-"+qkrUID())
	qk := qkrCreateQuotaKey(t, client, domain.QuotaKeyStatusActive, nil)
	acc := qkrCreateAccount(t, client, qk.ID, domain.PlatformCursor)
	_, err := client.AccountGroup.Create().SetAccountID(acc.ID).SetGroupID(g0.ID).SetPriority(7).Save(ctx)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = client.ExecContext(ctx, "DELETE FROM account_groups WHERE account_id = $1", acc.ID)
		_, _ = client.Group.Delete().Where(group.IDIn(g0.ID)).Exec(ctx)
		_, _ = client.Account.Delete().Where(account.IDEQ(acc.ID)).Exec(ctx)
		_ = client.QuotaKey.DeleteOneID(qk.ID).Exec(ctx)
	})

	// GroupIDs 不设置（nil）。
	runInTx(t, client, func(tx *ent.Tx) error {
		return qkr.UpdateSettingsAndSyncAccounts(ctx, tx, qk.ID, service.UpdateQuotaKeySettingsInput{})
	})

	// 绑定不变：仍是 g0（priority 7）。
	rows := qkrAccountGroupRows(t, client, acc.ID)
	require.Len(t, rows, 1, "binding must be unchanged when GroupIDs is nil")
	require.Equal(t, g0.ID, rows[0].GroupID)
	require.Equal(t, 7, rows[0].Priority)
	// 码级列不变（create 未设 GroupIds → 仍是 NULL）。
	require.Equal(t, "NULL", qkrScanGroupIdsRaw(t, client, qk.ID))
}

// ============================================================================
// B7 唯一冲突翻译：UpdateCredentialsAtomically 撞 code_mac 唯一索引。
// ============================================================================

// TestUpdateCredentialsAtomically_DuplicateCodeMACReturnsErrQuotaKeyDuplicateCode：
// 把第二条激活码的 code_mac 改成与第一条相同 → 撞 uq_quota_keys_code_mac →
// 必须翻译为 service.ErrQuotaKeyDuplicateCode（而非原样 pq 错误）。
func TestUpdateCredentialsAtomically_DuplicateCodeMACReturnsErrQuotaKeyDuplicateCode(t *testing.T) {
	client := quotaKeyRepositoryTestClient(t)
	ctx := context.Background()
	qkr := qkrRepo(t, client)

	// 两条激活码，code_mac 不同，均活跃、非 NULL（partial unique index 只对非 NULL 生效）。
	qk1 := qkrCreateQuotaKey(t, client, domain.QuotaKeyStatusActive, nil)
	_, err := client.QuotaKey.UpdateOneID(qk1.ID).SetCodeMAC("mac-dup").Save(ctx)
	require.NoError(t, err)
	qk2 := qkrCreateQuotaKey(t, client, domain.QuotaKeyStatusActive, nil)
	_, err = client.QuotaKey.UpdateOneID(qk2.ID).SetCodeMAC("mac-other").Save(ctx)
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = client.QuotaKey.DeleteOneID(qk1.ID).Exec(ctx)
		_ = client.QuotaKey.DeleteOneID(qk2.ID).Exec(ctx)
	})

	// 把 qk2 的 code_mac 改成与 qk1 相同 → 撞唯一索引。
	err = runInTxErr(t, client, func(tx *ent.Tx) error {
		return qkr.UpdateCredentialsAtomically(ctx, tx, qk2.ID, "enc:v1:dup", "mac-dup", "last4", false)
	})
	require.Error(t, err, "duplicate code_mac must fail")
	require.ErrorIs(t, err, service.ErrQuotaKeyDuplicateCode,
		"code_mac unique violation must translate to ErrQuotaKeyDuplicateCode")
}
