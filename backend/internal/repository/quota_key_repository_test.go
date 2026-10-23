//go:build unit

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// quotaKeyRepositoryTestClient 返回连接本地 quota_test 库的 ent client（懒初始化，幂等建表）。
// 与 service 包测试同型（C5a 之前 repository 无单测；此处沿用既有 DB 集成测试形态）。
func quotaKeyRepositoryTestClient(t *testing.T) *ent.Client {
	t.Helper()
	repoTestOnce.Do(func() {
		dsn := os.Getenv("QUOTA_TEST_DSN")
		if dsn == "" {
			dsn = "postgres://quota_test:quota_test_pw@127.0.0.1:5432/quota_test?sslmode=disable"
		}
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			repoTestInitErr = fmt.Errorf("open sql: %w", err)
			return
		}
		if err := db.Ping(); err != nil {
			_ = db.Close()
			repoTestInitErr = fmt.Errorf("ping quota_test db: %w", err)
			return
		}
		drv := entsql.OpenDB(dialect.Postgres, db)
		client := ent.NewClient(ent.Driver(drv))
		if err := client.Schema.Create(context.Background()); err != nil {
			_ = db.Close()
			repoTestInitErr = fmt.Errorf("schema create: %w", err)
			return
		}
		repoTestClient = client
	})
	if repoTestInitErr != nil {
		t.Fatalf("quota_key repository test DB init failed: %v", repoTestInitErr)
	}
	return repoTestClient
}

var (
	repoTestOnce    sync.Once
	repoTestClient  *ent.Client
	repoTestInitErr error
)

// TestQuotaKeyRepository_UpdateMeta：UpdateMeta 仅改 expires_at / notes，
// 不碰 code_encrypted / code_mac / code_last4 / status。
//
// tx 必填（A07/§25.17.6）：元数据写入只允许发生在调用方的同 key 事务内，
// 生产路径不得退回默认 client 走锁外独立提交。这里逐段真实开事务并 commit。
func TestQuotaKeyRepository_UpdateMeta(t *testing.T) {
	client := quotaKeyRepositoryTestClient(t)
	ctx := context.Background()
	repo := NewQuotaKeyRepository(client)

	// 前置：创建一条激活码（含 code / status 等既有字段）。
	uid := fmt.Sprintf("%d", time.Now().UnixNano())
	qk, err := client.QuotaKey.Create().
		SetCodeEncrypted("enc:v1:orig-" + uid).
		SetCodeLast4("abcd").
		SetStatus("active").
		Save(ctx)
	require.NoError(t, err)
	createdID := qk.ID

	// tx 为 nil 时失败关闭：不存在"锁外也能写"的旁路。
	require.Error(t, repo.UpdateMeta(ctx, nil, createdID, nil, nil),
		"UpdateMeta must fail closed without the caller's transaction")

	// 执行 UpdateMeta：改 expires_at / notes，不动 code / status。
	expiresAt := time.Now().Add(48 * time.Hour)
	notes := "updated note " + uid
	expiresAtValue := &expiresAt
	notesValue := &notes
	runInTx(t, client, func(tx *ent.Tx) error {
		return repo.UpdateMeta(ctx, tx, createdID, &expiresAtValue, &notesValue)
	})

	// 读回，验证元数据生效。
	got, err := client.QuotaKey.Get(ctx, createdID)
	require.NoError(t, err)
	require.NotNil(t, got.ExpiresAt)
	require.WithinDuration(t, expiresAt, *got.ExpiresAt, time.Second)
	require.NotNil(t, got.Notes)
	require.Equal(t, notes, *got.Notes)

	// 不碰 code / status（核心不变量）。
	require.Equal(t, "enc:v1:orig-"+uid, got.CodeEncrypted, "UpdateMeta must NOT touch code_encrypted")
	require.Equal(t, "abcd", got.CodeLast4, "UpdateMeta must NOT touch code_last4")
	require.Equal(t, "active", got.Status, "UpdateMeta must NOT touch status")

	// expires_at / notes 仅传其一时的分支：只改 notes。
	notes2 := "only notes " + uid
	notes2Value := &notes2
	runInTx(t, client, func(tx *ent.Tx) error {
		return repo.UpdateMeta(ctx, tx, createdID, nil, &notes2Value)
	})
	got2, err := client.QuotaKey.Get(ctx, createdID)
	require.NoError(t, err)
	require.NotNil(t, got2.Notes)
	require.Equal(t, notes2, *got2.Notes)
	require.NotNil(t, got2.ExpiresAt, "expires_at must remain when only notes is updated")
	require.WithinDuration(t, expiresAt, *got2.ExpiresAt, time.Second)

	// 显式 null 清空两个可空字段。
	var clearExpiresAt *time.Time
	var clearNotes *string
	runInTx(t, client, func(tx *ent.Tx) error {
		return repo.UpdateMeta(ctx, tx, createdID, &clearExpiresAt, &clearNotes)
	})
	got3, err := client.QuotaKey.Get(ctx, createdID)
	require.NoError(t, err)
	require.Nil(t, got3.ExpiresAt)
	require.Nil(t, got3.Notes)

	// 清理（软删口径：删除由 SoftDeleteMixin 拦截为置 deleted_at，行保留）。
	err = client.QuotaKey.DeleteOneID(createdID).Exec(ctx)
	require.NoError(t, err)
}

// runInTx 在真实事务内执行 fn 并提交（失败即 t.Fatal）。
func runInTx(t *testing.T, client *ent.Client, fn func(tx *ent.Tx) error) {
	t.Helper()
	tx, err := client.Tx(context.Background())
	require.NoError(t, err)
	require.NoError(t, fn(tx))
	require.NoError(t, tx.Commit())
}

// qkrEventAccountChanged 对应 service.SchedulerOutboxEventAccountChanged（"account_changed"）。
// 此处不复用 service 常量以避免测试包对 service 的额外耦合（repository 包已依赖 service）。
const qkrEventAccountChanged = "account_changed"

// qkrSchedulerOutboxDDL 复用仓库 migrations/036_scheduler_outbox.sql 与
// migrations/152_scheduler_outbox_dedup_key.sql 的 DDL（索引为非 CONCURRENTLY 写法）。
// scheduler_outbox 不是 ent 实体，故由测试显式建表。
var qkrSchedulerOutboxDDL = []string{
	`CREATE TABLE IF NOT EXISTS scheduler_outbox (
		id BIGSERIAL PRIMARY KEY,
		event_type TEXT NOT NULL,
		account_id BIGINT NULL,
		group_id BIGINT NULL,
		payload JSONB NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,
	`CREATE INDEX IF NOT EXISTS idx_scheduler_outbox_created_at ON scheduler_outbox (created_at)`,
	`ALTER TABLE scheduler_outbox ADD COLUMN IF NOT EXISTS dedup_key TEXT`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_scheduler_outbox_pending_dedup_key ON scheduler_outbox (dedup_key) WHERE dedup_key IS NOT NULL`,
}

// qkrOutboxFailTriggerCreate 在 scheduler_outbox 上建 BEFORE INSERT 触发器强制抛错，
// 用于验证软删与 outbox 同生共死（事务回滚）。
const qkrOutboxFailTriggerCreate = `
CREATE OR REPLACE FUNCTION qkr_raise_always() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'forced outbox failure'; END;
$$;
CREATE TRIGGER qkr_force_outbox_fail BEFORE INSERT ON scheduler_outbox FOR EACH ROW EXECUTE FUNCTION qkr_raise_always();
`

const qkrOutboxFailTriggerDrop = `
DROP TRIGGER IF EXISTS qkr_force_outbox_fail ON scheduler_outbox;
DROP FUNCTION IF EXISTS qkr_raise_always();
`

func timePtr(t time.Time) *time.Time { return &t }

// qkrCreateQuotaKey 创建一条激活码（code_encrypted 带时间戳唯一后缀）。
func qkrCreateQuotaKey(t *testing.T, client *ent.Client, status string, expiresAt *time.Time) *ent.QuotaKey {
	t.Helper()
	ctx := context.Background()
	uid := fmt.Sprintf("%d", time.Now().UnixNano())
	b := client.QuotaKey.Create().
		SetCodeEncrypted("enc:v1:exp-a1-" + uid).
		SetStatus(status)
	if expiresAt != nil {
		b = b.SetExpiresAt(*expiresAt)
	}
	qk, err := b.Save(ctx)
	require.NoError(t, err)
	return qk
}

// qkrCreateAccount 创建一条归属给定激活码、指定平台的账号。
func qkrCreateAccount(t *testing.T, client *ent.Client, quotaKeyID int64, platform string) *ent.Account {
	t.Helper()
	ctx := context.Background()
	uid := fmt.Sprintf("%d", time.Now().UnixNano())
	a, err := client.Account.Create().
		SetName("acc-" + uid).
		SetPlatform(platform).
		SetType("oauth").
		SetCredentials(map[string]any{"k": "v"}).
		SetQuotaKeyID(quotaKeyID).
		Save(ctx)
	require.NoError(t, err)
	return a
}

// qkrScanDeletedAt 绕过 SoftDeleteMixin 拦截，直接读 accounts.deleted_at（raw SQL）。
func qkrScanDeletedAt(t *testing.T, client *ent.Client, id int64) *time.Time {
	t.Helper()
	ctx := context.Background()
	rows, err := client.QueryContext(ctx, "SELECT deleted_at FROM accounts WHERE id = $1", id)
	require.NoError(t, err)
	defer rows.Close()
	require.True(t, rows.Next(), "account row must still physically exist, id=%d", id)
	var deletedAt *time.Time
	require.NoError(t, rows.Scan(&deletedAt))
	return deletedAt
}

// qkrCountRaw 执行裸 COUNT 查询（绕过 ent 软删拦截）。
func qkrCountRaw(t *testing.T, client *ent.Client, query string, args ...any) int {
	t.Helper()
	ctx := context.Background()
	rows, err := client.QueryContext(ctx, query, args...)
	require.NoError(t, err)
	defer rows.Close()
	require.True(t, rows.Next())
	var n int
	require.NoError(t, rows.Scan(&n))
	return n
}

// qkrEnsureSchedulerOutbox 幂等建 scheduler_outbox 表（含 dedup_key 列）。
func qkrEnsureSchedulerOutbox(t *testing.T, client *ent.Client) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range qkrSchedulerOutboxDDL {
		_, err := client.ExecContext(ctx, stmt)
		require.NoError(t, err)
	}
}

// qkrOutboxAccountIDs 取 scheduler_outbox 中给定 event_type 的 account_id 集合。
func qkrOutboxAccountIDs(t *testing.T, client *ent.Client, eventType string) []int64 {
	t.Helper()
	ctx := context.Background()
	rows, err := client.QueryContext(ctx, "SELECT account_id FROM scheduler_outbox WHERE event_type = $1", eventType)
	require.NoError(t, err)
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	return ids
}

// qkrRepo 取具体类型 *quotaKeyRepository（本单不改 interface，新方法仅在具体类型上）。
func qkrRepo(t *testing.T, client *ent.Client) *quotaKeyRepository {
	t.Helper()
	repo := NewQuotaKeyRepository(client)
	concrete, ok := repo.(*quotaKeyRepository)
	require.True(t, ok, "expected concrete *quotaKeyRepository")
	return concrete
}

// TestSoftDeleteCursorAccountsByExpiredQuotaKey_DeletesCursorAccountsOnly：
// 仅到期 cursor 账号被软删；openai 账号不受影响；三行仍物理存在。
func TestSoftDeleteCursorAccountsByExpiredQuotaKey_DeletesCursorAccountsOnly(t *testing.T) {
	client := quotaKeyRepositoryTestClient(t)
	ctx := context.Background()
	qkr := qkrRepo(t, client)

	now := time.Now()
	qk := qkrCreateQuotaKey(t, client, domain.QuotaKeyStatusExpired, timePtr(now.Add(-time.Hour)))
	c1 := qkrCreateAccount(t, client, qk.ID, domain.PlatformCursor)
	c2 := qkrCreateAccount(t, client, qk.ID, domain.PlatformCursor)
	o1 := qkrCreateAccount(t, client, qk.ID, "openai")
	t.Cleanup(func() {
		_, _ = client.Account.Delete().Where(account.IDIn(c1.ID, c2.ID, o1.ID)).Exec(ctx)
		_ = client.QuotaKey.DeleteOneID(qk.ID).Exec(ctx)
	})

	var got []int64
	runInTx(t, client, func(tx *ent.Tx) error {
		var err error
		got, err = qkr.SoftDeleteCursorAccountsByExpiredQuotaKey(ctx, tx, qk.ID, now)
		return err
	})

	require.ElementsMatch(t, []int64{c1.ID, c2.ID}, got)
	require.NotNil(t, qkrScanDeletedAt(t, client, c1.ID), "cursor account must be soft-deleted")
	require.NotNil(t, qkrScanDeletedAt(t, client, c2.ID), "cursor account must be soft-deleted")
	require.Nil(t, qkrScanDeletedAt(t, client, o1.ID), "openai account must NOT be soft-deleted")
	require.Equal(t, 3, qkrCountRaw(t, client, "SELECT count(*) FROM accounts WHERE quota_key_id = $1", qk.ID),
		"all three account rows must still physically exist")
}

// TestSoftDeleteCursorAccountsByExpiredQuotaKey_KeyNotExpiredSkips：
// status=active（即便 expires_at 已过去）不触发软删。
func TestSoftDeleteCursorAccountsByExpiredQuotaKey_KeyNotExpiredSkips(t *testing.T) {
	client := quotaKeyRepositoryTestClient(t)
	ctx := context.Background()
	qkr := qkrRepo(t, client)

	now := time.Now()
	qk := qkrCreateQuotaKey(t, client, domain.QuotaKeyStatusActive, timePtr(now.Add(-time.Hour)))
	a1 := qkrCreateAccount(t, client, qk.ID, domain.PlatformCursor)
	t.Cleanup(func() {
		_, _ = client.Account.Delete().Where(account.IDEQ(a1.ID)).Exec(ctx)
		_ = client.QuotaKey.DeleteOneID(qk.ID).Exec(ctx)
	})

	var got []int64
	runInTx(t, client, func(tx *ent.Tx) error {
		var err error
		got, err = qkr.SoftDeleteCursorAccountsByExpiredQuotaKey(ctx, tx, qk.ID, now)
		return err
	})

	require.Empty(t, got)
	require.Nil(t, qkrScanDeletedAt(t, client, a1.ID), "account must remain undeleted when key not expired")
}

// TestSoftDeleteCursorAccountsByExpiredQuotaKey_ExpiresAtInFutureSkips：
// status=expired 但 expires_at 在未来，不触发软删。
func TestSoftDeleteCursorAccountsByExpiredQuotaKey_ExpiresAtInFutureSkips(t *testing.T) {
	client := quotaKeyRepositoryTestClient(t)
	ctx := context.Background()
	qkr := qkrRepo(t, client)

	now := time.Now()
	qk := qkrCreateQuotaKey(t, client, domain.QuotaKeyStatusExpired, timePtr(now.Add(time.Hour)))
	a1 := qkrCreateAccount(t, client, qk.ID, domain.PlatformCursor)
	t.Cleanup(func() {
		_, _ = client.Account.Delete().Where(account.IDEQ(a1.ID)).Exec(ctx)
		_ = client.QuotaKey.DeleteOneID(qk.ID).Exec(ctx)
	})

	var got []int64
	runInTx(t, client, func(tx *ent.Tx) error {
		var err error
		got, err = qkr.SoftDeleteCursorAccountsByExpiredQuotaKey(ctx, tx, qk.ID, now)
		return err
	})

	require.Empty(t, got)
	require.Nil(t, qkrScanDeletedAt(t, client, a1.ID), "account must remain undeleted when expires_at is in the future")
}

// TestSoftDeleteCursorAccountsByExpiredQuotaKey_ExpiresAtNullSkips：
// status=expired 但 expires_at 为 NULL，不触发软删。
func TestSoftDeleteCursorAccountsByExpiredQuotaKey_ExpiresAtNullSkips(t *testing.T) {
	client := quotaKeyRepositoryTestClient(t)
	ctx := context.Background()
	qkr := qkrRepo(t, client)

	now := time.Now()
	qk := qkrCreateQuotaKey(t, client, domain.QuotaKeyStatusExpired, nil)
	a1 := qkrCreateAccount(t, client, qk.ID, domain.PlatformCursor)
	t.Cleanup(func() {
		_, _ = client.Account.Delete().Where(account.IDEQ(a1.ID)).Exec(ctx)
		_ = client.QuotaKey.DeleteOneID(qk.ID).Exec(ctx)
	})

	var got []int64
	runInTx(t, client, func(tx *ent.Tx) error {
		var err error
		got, err = qkr.SoftDeleteCursorAccountsByExpiredQuotaKey(ctx, tx, qk.ID, now)
		return err
	})

	require.Empty(t, got)
	require.Nil(t, qkrScanDeletedAt(t, client, a1.ID), "account must remain undeleted when expires_at is NULL")
}

// TestSoftDeleteCursorAccountsByExpiredQuotaKey_IdempotentSecondCall：
// 连续两次调用，第二次返回空且 deleted_at 值前后相等。
func TestSoftDeleteCursorAccountsByExpiredQuotaKey_IdempotentSecondCall(t *testing.T) {
	client := quotaKeyRepositoryTestClient(t)
	ctx := context.Background()
	qkr := qkrRepo(t, client)

	now := time.Now()
	qk := qkrCreateQuotaKey(t, client, domain.QuotaKeyStatusExpired, timePtr(now.Add(-time.Hour)))
	a1 := qkrCreateAccount(t, client, qk.ID, domain.PlatformCursor)
	t.Cleanup(func() {
		_, _ = client.Account.Delete().Where(account.IDEQ(a1.ID)).Exec(ctx)
		_ = client.QuotaKey.DeleteOneID(qk.ID).Exec(ctx)
	})

	var first []int64
	runInTx(t, client, func(tx *ent.Tx) error {
		var err error
		first, err = qkr.SoftDeleteCursorAccountsByExpiredQuotaKey(ctx, tx, qk.ID, now)
		return err
	})
	require.Equal(t, []int64{a1.ID}, first)
	afterFirst := qkrScanDeletedAt(t, client, a1.ID)
	require.NotNil(t, afterFirst)

	var second []int64
	runInTx(t, client, func(tx *ent.Tx) error {
		var err error
		second, err = qkr.SoftDeleteCursorAccountsByExpiredQuotaKey(ctx, tx, qk.ID, now)
		return err
	})
	require.Empty(t, second, "second call must be a no-op")
	afterSecond := qkrScanDeletedAt(t, client, a1.ID)
	require.NotNil(t, afterSecond)
	require.Equal(t, afterFirst.Unix(), afterSecond.Unix(), "deleted_at must be unchanged by the second call")
}

// TestSoftDeleteCursorAccountsByExpiredQuotaKey_OtherQuotaKeyUntouched：
// 另一激活码下的 cursor 账号不被删除。
func TestSoftDeleteCursorAccountsByExpiredQuotaKey_OtherQuotaKeyUntouched(t *testing.T) {
	client := quotaKeyRepositoryTestClient(t)
	ctx := context.Background()
	qkr := qkrRepo(t, client)

	now := time.Now()
	qkA := qkrCreateQuotaKey(t, client, domain.QuotaKeyStatusExpired, timePtr(now.Add(-time.Hour)))
	qkB := qkrCreateQuotaKey(t, client, domain.QuotaKeyStatusExpired, timePtr(now.Add(-time.Hour)))
	aA := qkrCreateAccount(t, client, qkA.ID, domain.PlatformCursor)
	aB := qkrCreateAccount(t, client, qkB.ID, domain.PlatformCursor)
	t.Cleanup(func() {
		_, _ = client.Account.Delete().Where(account.IDIn(aA.ID, aB.ID)).Exec(ctx)
		_ = client.QuotaKey.DeleteOneID(qkA.ID).Exec(ctx)
		_ = client.QuotaKey.DeleteOneID(qkB.ID).Exec(ctx)
	})

	var got []int64
	runInTx(t, client, func(tx *ent.Tx) error {
		var err error
		got, err = qkr.SoftDeleteCursorAccountsByExpiredQuotaKey(ctx, tx, qkA.ID, now)
		return err
	})

	require.Equal(t, []int64{aA.ID}, got)
	require.NotNil(t, qkrScanDeletedAt(t, client, aA.ID))
	require.Nil(t, qkrScanDeletedAt(t, client, aB.ID), "cursor account under a different quota key must NOT be touched")
}

// TestSoftDeleteCursorAccountsByExpiredQuotaKey_WithOutboxInSameTx：
// 同一事务内调用新方法 + EnqueueAccountChangedOutbox 后提交，每个被删账号各一条
// event_type='account_changed' 且 account_id 匹配。
func TestSoftDeleteCursorAccountsByExpiredQuotaKey_WithOutboxInSameTx(t *testing.T) {
	client := quotaKeyRepositoryTestClient(t)
	ctx := context.Background()
	qkr := qkrRepo(t, client)
	qkrEnsureSchedulerOutbox(t, client)
	// 清空既有 outbox 行，避免跨运行累积干扰断言。
	_, err := client.ExecContext(ctx, "DELETE FROM scheduler_outbox")
	require.NoError(t, err)

	now := time.Now()
	qk := qkrCreateQuotaKey(t, client, domain.QuotaKeyStatusExpired, timePtr(now.Add(-time.Hour)))
	a1 := qkrCreateAccount(t, client, qk.ID, domain.PlatformCursor)
	a2 := qkrCreateAccount(t, client, qk.ID, domain.PlatformCursor)
	t.Cleanup(func() {
		_, _ = client.Account.Delete().Where(account.IDIn(a1.ID, a2.ID)).Exec(ctx)
		_ = client.QuotaKey.DeleteOneID(qk.ID).Exec(ctx)
	})

	var got []int64
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	got, err = qkr.SoftDeleteCursorAccountsByExpiredQuotaKey(ctx, tx, qk.ID, now)
	require.NoError(t, err)
	require.ElementsMatch(t, []int64{a1.ID, a2.ID}, got)
	require.NoError(t, qkr.EnqueueAccountChangedOutbox(ctx, tx, got))
	require.NoError(t, tx.Commit())

	// 账号已软删。
	require.NotNil(t, qkrScanDeletedAt(t, client, a1.ID))
	require.NotNil(t, qkrScanDeletedAt(t, client, a2.ID))
	// 每个被删账号各一条 account_changed outbox，account_id 匹配。
	outboxIDs := qkrOutboxAccountIDs(t, client, qkrEventAccountChanged)
	require.ElementsMatch(t, []int64{a1.ID, a2.ID}, outboxIDs)
}

// TestSoftDeleteCursorAccountsByExpiredQuotaKey_OutboxFailureRollsBack：
// scheduler_outbox 上的 BEFORE INSERT 触发器强制抛错，同一事务内调用新方法 +
// EnqueueAccountChangedOutbox 返回错误并回滚，两个 cursor 账号 deleted_at 仍为 NULL
// （证明软删与 outbox 同生共死，无半完成状态）。
func TestSoftDeleteCursorAccountsByExpiredQuotaKey_OutboxFailureRollsBack(t *testing.T) {
	client := quotaKeyRepositoryTestClient(t)
	ctx := context.Background()
	qkr := qkrRepo(t, client)
	qkrEnsureSchedulerOutbox(t, client)

	_, err := client.ExecContext(ctx, qkrOutboxFailTriggerCreate)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = client.ExecContext(context.Background(), qkrOutboxFailTriggerDrop)
	})

	now := time.Now()
	qk := qkrCreateQuotaKey(t, client, domain.QuotaKeyStatusExpired, timePtr(now.Add(-time.Hour)))
	a1 := qkrCreateAccount(t, client, qk.ID, domain.PlatformCursor)
	a2 := qkrCreateAccount(t, client, qk.ID, domain.PlatformCursor)
	t.Cleanup(func() {
		_, _ = client.Account.Delete().Where(account.IDIn(a1.ID, a2.ID)).Exec(ctx)
		_ = client.QuotaKey.DeleteOneID(qk.ID).Exec(ctx)
	})

	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	ids, err := qkr.SoftDeleteCursorAccountsByExpiredQuotaKey(ctx, tx, qk.ID, now)
	require.NoError(t, err, "soft-delete itself succeeds inside the tx")
	require.ElementsMatch(t, []int64{a1.ID, a2.ID}, ids)
	outErr := qkr.EnqueueAccountChangedOutbox(ctx, tx, ids)
	require.Error(t, outErr, "outbox insert must be forced to fail by the trigger")
	require.NoError(t, tx.Rollback())

	require.Nil(t, qkrScanDeletedAt(t, client, a1.ID), "rollback must undo the soft-delete (deleted_at still NULL)")
	require.Nil(t, qkrScanDeletedAt(t, client, a2.ID), "rollback must undo the soft-delete (deleted_at still NULL)")
}
