//go:build unit

package service

// EXP-B：QuotaKeySweeper「到期激活码 → 软删 Cursor 账号 + outbox」接线的测试。
//
// 覆盖两大部分：
//  1. 8 条内存 fake 用例：分派逻辑（active 标 expired + 统一账号补删）、幂等、
//     错误传播、不产生 outbox 的各类否定路径；
//  2. 2 条真实 PostgreSQL 用例（复用 quotaKeyTestEntClient / testDBRepo，
//     QUOTA_TEST_DSN 指定库）：SweepNow 端到端软删 cursor 账号、outbox 失败整体回滚。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// outboxRecordingRepo 在 fakeQuotaKeyRepo 之上记录 EnqueueAccountChangedOutbox 的
// 每次调用（传入账号 ID 集合），供断言"软删账号 → outbox 同事务"的调用次数与内容。
type outboxRecordingRepo struct {
	*fakeQuotaKeyRepo
	outboxCalls [][]int64
}

func newOutboxRecordingRepo() *outboxRecordingRepo {
	return &outboxRecordingRepo{fakeQuotaKeyRepo: newFakeQuotaKeyRepo()}
}

func (r *outboxRecordingRepo) EnqueueAccountChangedOutbox(_ context.Context, _ *ent.Tx, accountIDs []int64) error {
	r.outboxCalls = append(r.outboxCalls, append([]int64(nil), accountIDs...))
	return nil
}

// staleListOutboxRepo 组合 outboxRecordingRepo 与 staleExpiredRepo 的 ListExpired
// 行为：返回旧快照（当时到期），当前存储可能已被管理员清空 expires_at。
type staleListOutboxRepo struct {
	*outboxRecordingRepo
	stale []*ent.QuotaKey
}

func (r *staleListOutboxRepo) ListExpired(_ context.Context, _ time.Time) ([]*ent.QuotaKey, error) {
	return r.stale, nil
}

// outboxErrorRepo 注入 EnqueueAccountChangedOutbox 失败（模拟 outbox 写失败）。
type outboxErrorRepo struct {
	*fakeQuotaKeyRepo
}

func (r *outboxErrorRepo) EnqueueAccountChangedOutbox(_ context.Context, _ *ent.Tx, _ []int64) error {
	return errors.New("injected outbox failure")
}

// failingOutboxDBRepo 是真实 DB 用例的包装：embed *testDBRepo，仅让
// EnqueueAccountChangedOutbox 直接失败（验证整体回滚）。
type failingOutboxDBRepo struct {
	*testDBRepo
}

func (r *failingOutboxDBRepo) EnqueueAccountChangedOutbox(_ context.Context, _ *ent.Tx, _ []int64) error {
	return errors.New("injected outbox failure")
}

// mustCreateSweepAccount 在真实测试库中创建一个归属给定激活码的账号。
func mustCreateSweepAccount(t *testing.T, client *ent.Client, quotaKeyID int64, platform string, name string) *ent.Account {
	t.Helper()
	acc, err := client.Account.Create().
		SetName(name).
		SetPlatform(platform).
		SetType("oauth").
		SetCredentials(map[string]any{"k": "v"}).
		SetQuotaKeyID(quotaKeyID).
		Save(context.Background())
	require.NoError(t, err)
	return acc
}

// ---------------------------------------------------------------------------
// 内存 fake 用例
// ---------------------------------------------------------------------------

// TestQuotaKeySweep_ActiveExpired_DeletesAndEnqueues：active + 到期 →
// 标 expired（count==1）+ 调用账号删除 + outbox 收到返回 ID。
func TestQuotaKeySweep_ActiveExpired_DeletesAndEnqueues(t *testing.T) {
	repo := newOutboxRecordingRepo()
	repo.byID[1] = &ent.QuotaKey{ID: 1, Status: domain.QuotaKeyStatusActive, ExpiresAt: timePtr(time.Now().Add(-time.Hour))}
	repo.cursorDeletedIDs = []int64{100, 101}

	svc := newSweepTestSvc(repo)
	n, err := svc.sweepOnce(context.Background(), &ent.Tx{})
	require.NoError(t, err)
	require.Equal(t, 1, n, "active expired key must be marked expired")
	require.Equal(t, domain.QuotaKeyStatusExpired, repo.byID[1].Status)
	require.Equal(t, 1, repo.cursorDeleteCalls, "account deletion must be dispatched for the active branch")
	require.Len(t, repo.outboxCalls, 1, "outbox must be hit exactly once")
	require.ElementsMatch(t, []int64{100, 101}, repo.outboxCalls[0])
}

// TestQuotaKeySweep_AlreadyExpired_BackfillsAccounts：status=expired + 到期 +
// 返回 2 个 ID → 不标 expired（count==0）、仍调用账号删除、outbox 收到 2 个 ID（历史补删）。
func TestQuotaKeySweep_AlreadyExpired_BackfillsAccounts(t *testing.T) {
	repo := newOutboxRecordingRepo()
	repo.byID[1] = &ent.QuotaKey{ID: 1, Status: domain.QuotaKeyStatusExpired, ExpiresAt: timePtr(time.Now().Add(-time.Hour))}
	repo.cursorDeletedIDs = []int64{100, 101}

	svc := newSweepTestSvc(repo)
	n, err := svc.sweepOnce(context.Background(), &ent.Tx{})
	require.NoError(t, err)
	require.Equal(t, 0, n, "already-expired key must not be re-marked")
	require.Equal(t, 1, repo.cursorDeleteCalls, "already-expired key must still backfill accounts")
	require.Len(t, repo.outboxCalls, 1)
	require.ElementsMatch(t, []int64{100, 101}, repo.outboxCalls[0])
}

// TestQuotaKeySweep_NotExpired_NoAccountCall：active + 未到期 → 不触发账号删除。
func TestQuotaKeySweep_NotExpired_NoAccountCall(t *testing.T) {
	repo := newOutboxRecordingRepo()
	repo.byID[1] = &ent.QuotaKey{ID: 1, Status: domain.QuotaKeyStatusActive, ExpiresAt: timePtr(time.Now().Add(time.Hour))}

	svc := newSweepTestSvc(repo)
	n, err := svc.sweepOnce(context.Background(), &ent.Tx{})
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Equal(t, 0, repo.cursorDeleteCalls, "not-expired key must not trigger account deletion")
	require.Empty(t, repo.outboxCalls)
}

// TestQuotaKeySweep_ExpiresAtNull_NoAccountCall：expires_at=NULL → 不触发账号删除。
func TestQuotaKeySweep_ExpiresAtNull_NoAccountCall(t *testing.T) {
	repo := newOutboxRecordingRepo()
	repo.byID[1] = &ent.QuotaKey{ID: 1, Status: domain.QuotaKeyStatusActive, ExpiresAt: nil}

	svc := newSweepTestSvc(repo)
	n, err := svc.sweepOnce(context.Background(), &ent.Tx{})
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Equal(t, 0, repo.cursorDeleteCalls, "expires_at=NULL key must not trigger account deletion")
	require.Empty(t, repo.outboxCalls)
}

// TestQuotaKeySweep_StaleSnapshotExpiresCleared_NoAccountCall：ListExpired 返回旧快照
// （仍到期）但 MarkExpiredIfStillExpired 返回 0（管理员已清空 expires_at）且账号删除
// 返回空 → 不产生 outbox。
func TestQuotaKeySweep_StaleSnapshotExpiresCleared_NoAccountCall(t *testing.T) {
	repo := &staleListOutboxRepo{outboxRecordingRepo: newOutboxRecordingRepo()}
	// 当前存储：expires_at 已被管理员清空。
	repo.byID[1] = &ent.QuotaKey{ID: 1, Status: domain.QuotaKeyStatusActive, ExpiresAt: nil}
	// ListExpired 仍返回旧快照（当时到期）。
	repo.stale = []*ent.QuotaKey{{ID: 1, Status: domain.QuotaKeyStatusActive, ExpiresAt: timePtr(time.Now().Add(-time.Hour))}}
	repo.cursorDeletedIDs = nil // 账号删除复核后返回空集合

	svc := newSweepTestSvc(repo)
	n, err := svc.sweepOnce(context.Background(), &ent.Tx{})
	require.NoError(t, err)
	require.Equal(t, 0, n, "interleaved expires_at clear must not mark expired")
	require.Equal(t, domain.QuotaKeyStatusActive, repo.byID[1].Status)
	require.Empty(t, repo.outboxCalls, "empty account-delete result must not reach outbox")
}

// TestQuotaKeySweep_IdempotentSecondSweep：第一次删除返回 ID、第二次返回空 →
// 只产生一次 outbox 调用。
func TestQuotaKeySweep_IdempotentSecondSweep(t *testing.T) {
	repo := newOutboxRecordingRepo()
	repo.byID[1] = &ent.QuotaKey{ID: 1, Status: domain.QuotaKeyStatusActive, ExpiresAt: timePtr(time.Now().Add(-time.Hour))}
	repo.cursorDeletedIDs = []int64{100}

	svc := newSweepTestSvc(repo)
	ctx := context.Background()

	n, err := svc.sweepOnce(ctx, &ent.Tx{})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, repo.outboxCalls, 1)

	// 第二轮：账号已删，SoftDeleteCursorAccountsByExpiredQuotaKey 返回空集合。
	repo.cursorDeletedIDs = nil
	n2, err := svc.sweepOnce(ctx, &ent.Tx{})
	require.NoError(t, err)
	require.Equal(t, 0, n2, "second sweep must not re-mark")
	require.Len(t, repo.outboxCalls, 1, "second sweep must not enqueue outbox again")
}

// TestQuotaKeySweep_OutboxErrorPropagates：EnqueueAccountChangedOutbox 返回错误 →
// sweepOnce 返回错误（供上层事务回滚）。
func TestQuotaKeySweep_OutboxErrorPropagates(t *testing.T) {
	repo := &outboxErrorRepo{fakeQuotaKeyRepo: newFakeQuotaKeyRepo()}
	repo.byID[1] = &ent.QuotaKey{ID: 1, Status: domain.QuotaKeyStatusActive, ExpiresAt: timePtr(time.Now().Add(-time.Hour))}
	repo.cursorDeletedIDs = []int64{100}

	svc := newSweepTestSvc(repo)
	n, err := svc.sweepOnce(context.Background(), &ent.Tx{})
	require.Error(t, err, "outbox failure must propagate")
	require.ErrorContains(t, err, "enqueue outbox")
	// 标 expired 已在错误前计入 count；错误向上传播 → 上层事务回滚，count 不落库。
	require.Equal(t, 1, n, "mark activity counted before outbox failure (rolled back by caller)")
}

// TestQuotaKeySweep_AccountDeleteErrorPropagates：账号删除新方法返回错误 →
// sweepOnce 返回错误。
func TestQuotaKeySweep_AccountDeleteErrorPropagates(t *testing.T) {
	repo := newOutboxRecordingRepo()
	repo.byID[1] = &ent.QuotaKey{ID: 1, Status: domain.QuotaKeyStatusActive, ExpiresAt: timePtr(time.Now().Add(-time.Hour))}
	repo.cursorDeleteErr = errors.New("injected account delete failure")

	svc := newSweepTestSvc(repo)
	n, err := svc.sweepOnce(context.Background(), &ent.Tx{})
	require.Error(t, err, "account-delete failure must propagate")
	require.ErrorContains(t, err, "cursor accounts")
	// 标 expired 已在错误前计入 count；错误向上传播 → 上层事务回滚，count 不落库。
	require.Equal(t, 1, n, "mark activity counted before account-delete failure (rolled back by caller)")
	require.Empty(t, repo.outboxCalls, "failed account delete must not enqueue outbox")
}

// ---------------------------------------------------------------------------
// 真实 PostgreSQL 用例（QUOTA_TEST_DSN 指定库；复用 quotaKeyTestEntClient/testDBRepo）
// ---------------------------------------------------------------------------

// TestQuotaKeySweep_RealDB_SweepNowDeletesCursorAccounts：建 key（active + 过去
// expires_at）+ 2 个 cursor 账号 + 1 个 openai 账号 → SweepNow：
//   - key status='expired'；
//   - 2 个 cursor 账号 deleted_at 非空、行仍存在；openai 账号 deleted_at 仍为空；
//   - outbox 收到被软删的 2 个 cursor 账号 ID；
//   - 再次 SweepNow 幂等：账号 deleted_at 不被改写、key 状态不变、不重复写 outbox。
func TestQuotaKeySweep_RealDB_SweepNowDeletesCursorAccounts(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	past := time.Now().Add(-time.Hour)
	qk, err := client.QuotaKey.Create().
		SetCodeEncrypted("enc:v1:exp-b-sweep-now").
		SetStatus(domain.QuotaKeyStatusActive).
		SetExpiresAt(past).
		Save(ctx)
	require.NoError(t, err)

	cursor1 := mustCreateSweepAccount(t, client, qk.ID, domain.PlatformCursor, "exp-b-sweep-cursor-1")
	cursor2 := mustCreateSweepAccount(t, client, qk.ID, domain.PlatformCursor, "exp-b-sweep-cursor-2")
	openai := mustCreateSweepAccount(t, client, qk.ID, domain.PlatformOpenAI, "exp-b-sweep-openai-1")

	t.Cleanup(func() {
		// 真库物理清理：需 SkipSoftDelete 绕过软删 Hook（DELETE 默认转 UPDATE deleted_at）。
		cctx := mixins.SkipSoftDelete(context.Background())
		_ = client.Account.DeleteOneID(cursor1.ID).Exec(cctx)
		_ = client.Account.DeleteOneID(cursor2.ID).Exec(cctx)
		_ = client.Account.DeleteOneID(openai.ID).Exec(cctx)
		_ = client.QuotaKey.DeleteOneID(qk.ID).Exec(cctx)
	})

	repo := &testDBRepo{client: client}
	svc := NewQuotaKeySweeper(repo, nil, client)

	// 计数只作下限断言：测试库与其它 QuotaKey 用例共用且不清库，其它用例遗留的
	// 到期码会一并被扫，故不断言精确条数（本 key/账号的状态才是判据）。
	n, err := svc.SweepNow(ctx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, n, 1, "sweep must mark at least the created expired key")

	got, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.Equal(t, domain.QuotaKeyStatusExpired, got.Status, "key must be marked expired")

	// 软删后的账号行仍存在：查询需 SkipSoftDelete 绕过 deleted_at IS NULL 过滤。
	skipCtx := mixins.SkipSoftDelete(ctx)
	acc1, err := client.Account.Query().Where(account.IDEQ(cursor1.ID)).Only(skipCtx)
	require.NoError(t, err, "cursor account 1 row must still exist")
	require.NotNil(t, acc1.DeletedAt, "cursor account 1 must be soft-deleted")
	acc2, err := client.Account.Query().Where(account.IDEQ(cursor2.ID)).Only(skipCtx)
	require.NoError(t, err, "cursor account 2 row must still exist")
	require.NotNil(t, acc2.DeletedAt, "cursor account 2 must be soft-deleted")
	accOpenAI, err := client.Account.Query().Where(account.IDEQ(openai.ID)).Only(skipCtx)
	require.NoError(t, err)
	require.Nil(t, accOpenAI.DeletedAt, "openai account must not be touched")

	// outbox 收到被软删的 2 个 cursor 账号 ID（库里其它历史 key 的软删也会产生
	// outbox 调用，故用"存在一次调用恰含这两个 ID"断言）。
	var found bool
	for _, ids := range repo.outboxCalls {
		if len(ids) == 2 &&
			((ids[0] == cursor1.ID && ids[1] == cursor2.ID) || (ids[0] == cursor2.ID && ids[1] == cursor1.ID)) {
			found = true
			break
		}
	}
	require.True(t, found, "outbox must receive the two soft-deleted cursor account ids")
	afterFirst := len(repo.outboxCalls)

	// 再次 SweepNow：幂等——账号 deleted_at 不被改写、key 状态不变、outbox 不重复。
	_, err = svc.SweepNow(ctx)
	require.NoError(t, err)

	acc1b, err := client.Account.Query().Where(account.IDEQ(cursor1.ID)).Only(skipCtx)
	require.NoError(t, err)
	require.Equal(t, acc1.DeletedAt, acc1b.DeletedAt, "second sweep must not rewrite deleted_at")
	got2, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.Equal(t, domain.QuotaKeyStatusExpired, got2.Status, "key status must stay expired")
	require.Len(t, repo.outboxCalls, afterFirst, "second sweep must not enqueue outbox again")
}

// TestQuotaKeySweep_RealDB_OutboxFailureRollsBack：用包装（embed *testDBRepo，
// EnqueueAccountChangedOutbox 直接返回错误）驱动 SweepNow：
//   - SweepNow 返回错误；
//   - 同一真库内 key 仍为 active、账号 deleted_at 仍为空（软删与标记整体回滚，
//     无半完成状态）。
func TestQuotaKeySweep_RealDB_OutboxFailureRollsBack(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	past := time.Now().Add(-time.Hour)
	qk, err := client.QuotaKey.Create().
		SetCodeEncrypted("enc:v1:exp-b-rollback").
		SetStatus(domain.QuotaKeyStatusActive).
		SetExpiresAt(past).
		Save(ctx)
	require.NoError(t, err)
	cursor1 := mustCreateSweepAccount(t, client, qk.ID, domain.PlatformCursor, "exp-b-rollback-cursor-1")

	t.Cleanup(func() {
		// 真库物理清理：需 SkipSoftDelete 绕过软删 Hook（DELETE 默认转 UPDATE deleted_at）。
		cctx := mixins.SkipSoftDelete(context.Background())
		_ = client.Account.DeleteOneID(cursor1.ID).Exec(cctx)
		_ = client.QuotaKey.DeleteOneID(qk.ID).Exec(cctx)
	})

	repo := &failingOutboxDBRepo{testDBRepo: &testDBRepo{client: client}}
	svc := NewQuotaKeySweeper(repo, nil, client)

	_, err = svc.SweepNow(ctx)
	require.Error(t, err, "outbox failure must propagate")

	// 整体回滚：key 仍 active、账号 deleted_at 仍为空。
	got, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.Equal(t, domain.QuotaKeyStatusActive, got.Status, "key must remain active after rollback")
	acc, err := client.Account.Get(ctx, cursor1.ID)
	require.NoError(t, err)
	require.Nil(t, acc.DeletedAt, "account must remain undeleted after rollback")
}
