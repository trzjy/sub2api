package repository

// E8 定向测试（repository 层）：写入口事件裁决三缺陷整改。
//   - #3 事件时间纳秒精度：CommitModelRateLimitObservation 写入的 meta last_event_at
//     必须为 RFC3339Nano（含纳秒），读取侧 GetModelRateLimitMeta 兼容解析；
//   - #4 初始 SET 参与事件裁决：SetModelRateLimitWithPreciseReset 读 meta → 裁决 →
//     单条 UPDATE 原子写（last_event_at=写入时刻、revision=rev+1）；较新 meta 时旧 SET
//     整体 no-op（不发 UPDATE）。

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"log"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

// metaPayloadMatcher 校验 CommitModelRateLimitObservation 写入的 meta 载荷：
// last_event_at 必须是 RFC3339Nano（含亚秒），revision 匹配期望值。
type metaPayloadMatcher struct {
	wantRevision int64
}

func (m metaPayloadMatcher) Match(value driver.Value) bool {
	raw, ok := value.([]byte)
	if !ok {
		return false
	}
	var meta struct {
		LastEventAt string `json:"last_event_at"`
		Revision    int64  `json:"revision"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return false
	}
	if meta.Revision != m.wantRevision {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, meta.LastEventAt)
	if err != nil {
		return false
	}
	// RFC3339Nano 含亚秒：秒级 RFC3339 无小数部分，必须检测到"含小数点"。
	for _, r := range meta.LastEventAt {
		if r == '.' {
			return true
		}
	}
	return false
}

// entryPayloadMatcher 校验条目载荷含 precise_reset 键。
type entryPayloadMatcher struct{}

func (entryPayloadMatcher) Match(value driver.Value) bool {
	raw, ok := value.([]byte)
	if !ok {
		return false
	}
	var entry map[string]any
	if err := json.Unmarshal(raw, &entry); err != nil {
		return false
	}
	_, ok = entry["precise_reset"]
	return ok
}

// CommitModelRateLimitObservation 单条 UPDATE 原子提交条目+meta：#3 纳秒写入口径。
func TestE8CommitObservation_MetaWritesNanoPrecision(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	driver := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(driver))
	t.Cleanup(func() { _ = client.Close() })
	repo := newAccountRepositoryWithSQL(client, db, nil)

	lastEventAt := time.Unix(1700000000, 123456789).UTC() // 带纳秒的事件时刻

	// 非事务路径：CommitModelRateLimitObservation 现复用 WithObservationTx 在 ctx 无事务时
	// 新开 r.client.Tx，将 UPDATE+meta+outbox 收进同一事务（BEGIN→UPDATE→outbox→COMMIT）。
	mock.ExpectBegin()
	// 单条 UPDATE（条目+meta 同语句），$5=meta 载荷含纳秒。
	mock.ExpectExec(regexp.QuoteMeta("UPDATE accounts SET")).
		WithArgs("deepseek:free", int64(42), false, sqlmock.AnyArg(), metaPayloadMatcher{wantRevision: 3}).
		WillReturnResult(sqlmock.NewResult(1, 1))
	// outbox INSERT（AccountChanged 走 dedup 分支，5 参数）。
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload, dedup_key)")).
		WithArgs(service.SchedulerOutboxEventAccountChanged, int64(42), nil, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	err = repo.CommitModelRateLimitObservation(context.Background(), 42, "deepseek:free", map[string]any{"rate_limit_reset_at": "2026-10-03T06:00:00Z"}, false, lastEventAt, 3)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// GetModelRateLimitMeta 读取侧兼容 RFC3339Nano（含纳秒）与历史秒级。
func TestE8GetModelRateLimitMeta_ParsesNanoAndSecondPrecision(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{name: "nano", value: `{"last_event_at":"2026-10-03T06:40:00.123456789Z","revision":5}`},
		{name: "legacy_seconds", value: `{"last_event_at":"2026-10-03T06:40:00Z","revision":2}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })

			mock.ExpectQuery(regexp.QuoteMeta("SELECT extra->'model_rate_limits_meta'->$1")).
				WithArgs("deepseek:free", int64(42)).
				WillReturnRows(sqlmock.NewRows([]string{"extra"}).AddRow([]byte(tc.value)))

			repo := newAccountRepositoryWithSQL(nil, db, nil)
			lastEventAt, rev, ok, err := repo.GetModelRateLimitMeta(context.Background(), 42, "deepseek:free")
			require.NoError(t, err)
			require.True(t, ok)
			if tc.name == "nano" {
				require.Equal(t, int64(5), rev)
				require.True(t, lastEventAt.Equal(time.Date(2026, 10, 3, 6, 40, 0, 123456789, time.UTC)), "纳秒精度必须无损还原")
			} else {
				require.Equal(t, int64(2), rev)
				require.True(t, lastEventAt.Equal(time.Date(2026, 10, 3, 6, 40, 0, 0, time.UTC)), "历史秒级必须兼容解析")
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

// #4 初始 SET 参与事件裁决：读 meta（无既有）→ 单条 UPDATE 原子写条目+meta
// （last_event_at=写入时刻、revision=0+1），再 outbox 广播。
func TestE8SetModelRateLimitWithPreciseReset_InitialSetCommitsMetaAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	driver := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(driver))
	t.Cleanup(func() { _ = client.Close() })
	repo := newAccountRepositoryWithSQL(client, db, nil)

	// 读 meta：无既有记录（NULL）。
	mock.ExpectQuery(regexp.QuoteMeta("SELECT extra->'model_rate_limits_meta'->$1")).
		WithArgs("deepseek:free", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"extra"}).AddRow(nil))

	// 非事务路径：CommitModelRateLimitObservation 复用 WithObservationTx 新开事务
	// （BEGIN→UPDATE→outbox→COMMIT）。
	mock.ExpectBegin()
	// 单条 UPDATE：$4=payload（含 precise_reset），$5=meta（last_event_at=纳秒、revision=1）。
	mock.ExpectExec(regexp.QuoteMeta("UPDATE accounts SET")).
		WithArgs("deepseek:free", int64(42), false, entryPayloadMatcher{}, metaPayloadMatcher{wantRevision: 1}).
		WillReturnResult(sqlmock.NewResult(1, 1))
	// outbox INSERT。
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload, dedup_key)")).
		WithArgs(service.SchedulerOutboxEventAccountChanged, int64(42), nil, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	err = repo.SetModelRateLimitWithPreciseReset(context.Background(), 42, "deepseek:free", time.Now().Add(48*time.Hour), true, service.SchedulerOutboxEventAccountChanged)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// #4 旧 SET 整体 no-op：读 meta 显示既有事件更新（last_event_at 在未来），
// 不得发 UPDATE 覆盖较新状态。
func TestE8SetModelRateLimitWithPreciseReset_OldSetNoOpWhenMetaNewer(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	driver := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(driver))
	t.Cleanup(func() { _ = client.Close() })
	repo := newAccountRepositoryWithSQL(client, db, nil)

	future := time.Now().Add(10 * time.Minute).UTC()
	metaJSON, err := json.Marshal(map[string]any{
		"last_event_at": future.Format(time.RFC3339Nano),
		"revision":      int64(9),
	})
	require.NoError(t, err)

	// 读 meta：较新的 last_event_at → 旧 SET 整体 no-op。
	mock.ExpectQuery(regexp.QuoteMeta("SELECT extra->'model_rate_limits_meta'->$1")).
		WithArgs("deepseek:free", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"extra"}).AddRow(metaJSON))
	// 无 UPDATE 期望：较新 meta 必须阻止旧 SET 覆盖。

	err = repo.SetModelRateLimitWithPreciseReset(context.Background(), 42, "deepseek:free", time.Now().Add(48*time.Hour), true, "reason")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet(), "较新 meta 时旧 SET 必须整体 no-op（无 UPDATE）")
}

// ===================== E37 收敛：SetModelRateLimit 并入事件裁决写链 =====================

// entryPayloadNoPreciseResetMatcher 校验 SetModelRateLimit 条目载荷：必须含
// rate_limited_at/rate_limit_reset_at/reason，不得含 precise_reset（E37 收敛后该路径不写
// precise_reset 键，条目载荷与收敛前逐键一致）。
type entryPayloadNoPreciseResetMatcher struct {
	reasonMust string
}

func (m entryPayloadNoPreciseResetMatcher) Match(value driver.Value) bool {
	raw, ok := value.([]byte)
	if !ok {
		return false
	}
	var entry map[string]any
	if err := json.Unmarshal(raw, &entry); err != nil {
		return false
	}
	if _, ok := entry["rate_limited_at"]; !ok {
		return false
	}
	if _, ok := entry["rate_limit_reset_at"]; !ok {
		return false
	}
	if _, ok := entry["precise_reset"]; ok {
		return false
	}
	rsn, ok := entry["reason"]
	if !ok {
		return false
	}
	return rsn == m.reasonMust
}

// E37 #1/#2：SetModelRateLimit 现经 commitModelRateLimitSet 共享写链提交条目+meta，
// 不再绕过 meta 直写 model_rate_limits 桶。验证：条目载荷含 rate_limited_at/
// rate_limit_reset_at/reason、不含 precise_reset；meta 基线推进（revision=0+1、纳秒）。
func TestE37SetModelRateLimit_CommitsEntryAndMetaViaSharedWriteChain(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	driver := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(driver))
	t.Cleanup(func() { _ = client.Close() })
	repo := newAccountRepositoryWithSQL(client, db, nil)

	// 读 meta：无既有记录（NULL）。
	mock.ExpectQuery(regexp.QuoteMeta("SELECT extra->'model_rate_limits_meta'->$1")).
		WithArgs("deepseek:free", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"extra"}).AddRow(nil))

	// 非事务路径：CommitModelRateLimitObservation 复用 WithObservationTx 新开事务
	// （BEGIN→UPDATE→outbox→COMMIT）。
	mock.ExpectBegin()
	// 单条 UPDATE：$4=条目（含 rate_limited_at/rate_limit_reset_at/reason、无 precise_reset），
	// $5=meta（revision=1、纳秒）。
	mock.ExpectExec(regexp.QuoteMeta("UPDATE accounts SET")).
		WithArgs("deepseek:free", int64(42), false, entryPayloadNoPreciseResetMatcher{reasonMust: "capacity-exhausted"}, metaPayloadMatcher{wantRevision: 1}).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload, dedup_key)")).
		WithArgs(service.SchedulerOutboxEventAccountChanged, int64(42), nil, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	err = repo.SetModelRateLimit(context.Background(), 42, "deepseek:free", time.Now().Add(48*time.Hour), "capacity-exhausted")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// E37 #4 裁决语义：预置较新 meta 事件后调用 SetModelRateLimit（构造严格更旧场景）→
// 整体 no-op，状态不被覆盖（不再有绕过 meta 的旧直写路径覆盖较新状态）。
func TestE37SetModelRateLimit_NoOpWhenMetaNewer(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	driver := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(driver))
	t.Cleanup(func() { _ = client.Close() })
	repo := newAccountRepositoryWithSQL(client, db, nil)

	future := time.Now().Add(10 * time.Minute).UTC()
	metaJSON, err := json.Marshal(map[string]any{
		"last_event_at": future.Format(time.RFC3339Nano),
		"revision":      int64(9),
	})
	require.NoError(t, err)

	// 读 meta：较新的 last_event_at → 旧 SET 整体 no-op。
	mock.ExpectQuery(regexp.QuoteMeta("SELECT extra->'model_rate_limits_meta'->$1")).
		WithArgs("deepseek:free", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"extra"}).AddRow(metaJSON))
	// 无 UPDATE 期望：较新 meta 必须阻止旧 SET 覆盖。

	err = repo.SetModelRateLimit(context.Background(), 42, "deepseek:free", time.Now().Add(48*time.Hour), "stale-reason")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet(), "较新 meta 时 SetModelRateLimit 必须整体 no-op（无 UPDATE）")
}

// ===================== E20 #2 恢复告警关闭纳入状态提交同一 ent 事务 =====================

// TestE20WithObservationTx_CommitsOnSuccess 验证窄事务面 WithObservationTx：fn 成功时整体提交
// （BEGIN→COMMIT），提交后 ctx 中状态写与同维告警关闭均已持久化。
func TestE20WithObservationTx_CommitsOnSuccess(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	driver := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(driver))
	t.Cleanup(func() { _ = client.Close() })
	repo := newAccountRepositoryWithSQL(client, db, nil)

	mock.ExpectBegin()
	mock.ExpectCommit()
	require.NoError(t, repo.WithObservationTx(context.Background(), func(txCtx context.Context) error {
		// 真实写入口 fn 内会经 txCtx 提交状态并发起告警关闭；此处用 no-op 验证事务编排本身。
		return nil
	}))
	require.NoError(t, mock.ExpectationsWereMet(), "fn 成功必须整体提交（BEGIN→COMMIT）")
}

// TestE20WithObservationTx_RollsBackOnFailure 验证窄事务面 WithObservationTx：fn（含状态提交或
// 同维告警关闭）任一失败 → 整体回滚（BEGIN→ROLLBACK），错误沿写入口传播；事务内已做写操作不提交。
func TestE20WithObservationTx_RollsBackOnFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	driver := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(driver))
	t.Cleanup(func() { _ = client.Close() })
	repo := newAccountRepositoryWithSQL(client, db, nil)

	fnErr := errors.New("simulated recovery step failure")
	mock.ExpectBegin()
	mock.ExpectRollback()
	got := repo.WithObservationTx(context.Background(), func(txCtx context.Context) error {
		return fnErr
	})
	require.ErrorIs(t, got, fnErr, "fn 失败必须沿写入口传播")
	require.NoError(t, mock.ExpectationsWereMet(), "fn 失败必须整体回滚（BEGIN→ROLLBACK）")
}

// ===================== E30 #1 outbox 事务感知 + 错误传播 =====================

// TestCommitModelRateLimitObservation_TxAwareRollback 验证恢复路径：CommitModelRateLimit
// Observation 内的 outbox 写入经 txAwareSQLExecutor 参与同一 ent 事务。fn（告警关闭）失败
// 整体回滚时，outbox 事件随事务丢弃（不落库）——调度器不会依据未提交/已回滚的恢复状态运行。
// 顺序断言（BEGIN→UPDATE→outbox INSERT→ROLLBACK）确保 outbox 写入发生在事务内而非裸连接。
func TestCommitModelRateLimitObservation_TxAwareRollback(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	driver := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(driver))
	t.Cleanup(func() { _ = client.Close() })
	repo := newAccountRepositoryWithSQL(client, db, nil)

	fnErr := errors.New("simulated recovery alert-close failure")
	mock.ExpectBegin()
	// 状态提交 UPDATE（事务内）。
	mock.ExpectExec(regexp.QuoteMeta("UPDATE accounts SET")).
		WithArgs("deepseek:free", int64(42), false, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	// outbox INSERT 经 txAwareSQLExecutor 参与同一事务（事务内 executor，而非裸 r.sql）。
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
		WithArgs(service.SchedulerOutboxEventAccountChanged, int64(42), nil, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectRollback()

	got := repo.WithObservationTx(context.Background(), func(txCtx context.Context) error {
		if werr := repo.CommitModelRateLimitObservation(txCtx, 42, "deepseek:free",
			map[string]any{"rate_limit_reset_at": "2026-10-03T06:00:00Z"}, false, time.Now(), 3); werr != nil {
			return werr
		}
		return fnErr
	})
	require.ErrorIs(t, got, fnErr, "恢复告警关闭失败必须沿写入口传播")
	require.NoError(t, mock.ExpectationsWereMet(), "outbox 必须写入事务内（随回滚丢弃，不落库）")
}

// TestCommitModelRateLimitObservation_OutboxFailurePropagates 验证 E41 修复：非事务路径下
// outbox 写失败随同一事务整体回滚（状态+meta 一并撤销，不再前一写独立提交），错误沿写入口
// 传播。行为同 E30 #1（outbox 失败不静默吞掉），但边界从「裸连接独立提交」升级为「同一事务」。
func TestCommitModelRateLimitObservation_OutboxFailurePropagates(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	driver := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(driver))
	t.Cleanup(func() { _ = client.Close() })
	repo := newAccountRepositoryWithSQL(client, db, nil)

	// 非事务路径：CommitModelRateLimitObservation 复用 WithObservationTx 新开事务。
	// 顺序断言（BEGIN→UPDATE→outbox 失败→ROLLBACK）确保 UPDATE 与 outbox 在同一事务内——
	// outbox 失败时状态+meta 随事务整体回滚，不独立提交。
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE accounts SET")).
		WithArgs("deepseek:free", int64(42), false, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	outboxErr := errors.New("simulated outbox insert failure")
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
		WithArgs(service.SchedulerOutboxEventAccountChanged, int64(42), nil, nil, sqlmock.AnyArg()).
		WillReturnError(outboxErr)
	mock.ExpectRollback()

	err = repo.CommitModelRateLimitObservation(context.Background(), 42, "deepseek:free",
		map[string]any{"rate_limit_reset_at": "2026-10-03T06:00:00Z"}, false, time.Now(), 3)
	require.Error(t, err, "outbox 写失败必须返回错误（不得静默吞掉）")
	require.ErrorIs(t, err, outboxErr)
	require.NoError(t, mock.ExpectationsWereMet(), "outbox 失败必须整体回滚（BEGIN→UPDATE→outbox失败→ROLLBACK），状态+meta 不独立提交")
}

// ===================== E41 非事务路径：快照同步在提交后执行 =====================

// e41RecordingSchedulerCache 是 service.SchedulerCache 的薄实现桩，仅用于在单测中提供非 nil
// 缓存，使 syncSchedulerAccountSnapshot 越过 nil 短路、真正发起 GetByID 读（从而可被顺序断言
// 捕捉到「读发生在 COMMIT 之后」）。除 SetAccount 记录外其余方法均为 no-op。
type e41RecordingSchedulerCache struct {
	mu          sync.Mutex
	setAccounts []*service.Account
}

func (c *e41RecordingSchedulerCache) GetSnapshot(context.Context, service.SchedulerBucket) ([]*service.Account, bool, error) {
	return nil, false, nil
}
func (c *e41RecordingSchedulerCache) CaptureBucketWriteToken(_ context.Context, bucket service.SchedulerBucket) (service.SchedulerBucketWriteToken, error) {
	return service.SchedulerBucketWriteToken{Bucket: bucket, Epoch: 1}, nil
}
func (c *e41RecordingSchedulerCache) SetSnapshot(context.Context, service.SchedulerBucket, service.SchedulerBucketWriteToken, []service.Account) error {
	return nil
}
func (c *e41RecordingSchedulerCache) RetireBucket(context.Context, service.SchedulerBucket) error { return nil }
func (c *e41RecordingSchedulerCache) ReopenBucket(_ context.Context, bucket service.SchedulerBucket) (service.SchedulerBucketWriteToken, error) {
	return service.SchedulerBucketWriteToken{Bucket: bucket, Epoch: 1}, nil
}
func (c *e41RecordingSchedulerCache) TryAcquireGroupLifecycleLease(_ context.Context, groupID int64, _ time.Duration) (service.SchedulerGroupLifecycleLease, bool, error) {
	return service.SchedulerGroupLifecycleLease{GroupID: groupID, OwnerToken: "e41-recorder"}, true, nil
}
func (c *e41RecordingSchedulerCache) ReleaseGroupLifecycleLease(context.Context, service.SchedulerGroupLifecycleLease) error {
	return nil
}
func (c *e41RecordingSchedulerCache) GetAccount(context.Context, int64) (*service.Account, error) { return nil, nil }
func (c *e41RecordingSchedulerCache) SetAccount(_ context.Context, account *service.Account) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setAccounts = append(c.setAccounts, account)
	return nil
}
func (c *e41RecordingSchedulerCache) DeleteAccount(context.Context, int64) error         { return nil }
func (c *e41RecordingSchedulerCache) UpdateLastUsed(context.Context, map[int64]time.Time) error { return nil }
func (c *e41RecordingSchedulerCache) TryLockBucket(context.Context, service.SchedulerBucket, time.Duration) (bool, error) {
	return true, nil
}
func (c *e41RecordingSchedulerCache) UnlockBucket(context.Context, service.SchedulerBucket) error { return nil }
func (c *e41RecordingSchedulerCache) ListBuckets(context.Context) ([]service.SchedulerBucket, error) {
	return nil, nil
}
func (c *e41RecordingSchedulerCache) GetOutboxWatermark(context.Context) (int64, error) { return 0, nil }
func (c *e41RecordingSchedulerCache) SetOutboxWatermark(context.Context, int64) error   { return nil }

// TestCommitModelRateLimitObservation_NonTxSyncAfterCommit 验证 E41 #2：非事务路径提交成功后
// 才执行快照同步——且同步的 GetByID 读必须发生在 COMMIT 之后（不得预览未提交状态）。
// 顺序断言（BEGIN→UPDATE→outbox→COMMIT→快照读 SELECT）。若同步被错误地放在提交前，SELECT 会
// 出现在 COMMIT 之前，ordered 断言随即失败；GetByID 读失败被 syncSchedulerAccountSnapshot 静默
// 吞掉，不影响 CommitModelRateLimitObservation 返回 nil（同步失败不影响原子性）。
func TestCommitModelRateLimitObservation_NonTxSyncAfterCommit(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	driver := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(driver))
	t.Cleanup(func() { _ = client.Close() })
	cache := &e41RecordingSchedulerCache{}
	repo := newAccountRepositoryWithSQL(client, db, cache)

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE accounts SET")).
		WithArgs("deepseek:free", int64(42), false, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
		WithArgs(service.SchedulerOutboxEventAccountChanged, int64(42), nil, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	// 提交后的快照同步读：该 SELECT 必须出现在 COMMIT 之后。
	mock.ExpectQuery(`(?s)SELECT .* FROM "accounts" WHERE "accounts"\."id" = \$1`).
		WillReturnError(errors.New("sync read not asserted in unit (proves ordering only)"))

	err = repo.CommitModelRateLimitObservation(context.Background(), 42, "deepseek:free",
		map[string]any{"rate_limit_reset_at": "2026-10-03T06:00:00Z"}, false, time.Now(), 3)
	require.NoError(t, err, "快照同步读失败被静默吞掉，提交结果不受影响")
	require.NoError(t, mock.ExpectationsWereMet(), "快照同步必须在 COMMIT 之后发生（不得预览未提交状态）")
}

// TestCommitModelRateLimitObservation_PostCommitSyncFailureObservable 验证 E47：非事务路径提交成功后，
// 提交后快照同步（SyncSchedulerAccountSnapshot 错误返回版）若失败，其错误必须可观测（经结构化日志暴露），
// 但不得被重新分类为提交失败——业务结果仍返回 nil（与 ratelimit_service.go 提交后同步约定同口径）。
// 与 E41 #2 的区别：E41 仅验证「同步在 COMMIT 之后」且「失败被静默吞掉」；E47 进一步要求同步失败
// 经错误返回版补做并被记录（不再静默丢失）。通过断言日志含 "after commit failed" 证明走了新的
// 错误返回版 SyncSchedulerAccountSnapshot（旧 syncSchedulerAccountSnapshot 错误丢弃版日志前缀不同，
// 不含该字样），且提交结果仍为 nil。
func TestCommitModelRateLimitObservation_PostCommitSyncFailureObservable(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	driver := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(driver))
	t.Cleanup(func() { _ = client.Close() })
	cache := &e41RecordingSchedulerCache{}
	repo := newAccountRepositoryWithSQL(client, db, cache)

	// 捕获标准日志输出：LegacyPrintf 在 logger 未初始化（单测默认）时回退到标准库 log。
	var logBuf bytes.Buffer
	prevLog := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(prevLog)

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE accounts SET")).
		WithArgs("deepseek:free", int64(42), false, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
		WithArgs(service.SchedulerOutboxEventAccountChanged, int64(42), nil, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	// 提交后快照同步读失败（模拟权威状态读取/缓存写入失败）。
	mock.ExpectQuery(`(?s)SELECT .* FROM "accounts" WHERE "accounts"\."id" = \$1`).
		WillReturnError(errors.New("sync read failed"))

	err = repo.CommitModelRateLimitObservation(context.Background(), 42, "deepseek:free",
		map[string]any{"rate_limit_reset_at": "2026-10-03T06:00:00Z"}, false, time.Now(), 3)
	// 业务结果已确认提交，不得被同步失败重新分类为提交失败。
	require.NoError(t, err, "提交后快照同步失败不得重分类为提交失败")
	require.NoError(t, mock.ExpectationsWereMet(), "快照同步必须在 COMMIT 之后发生（不得预览未提交状态）")
	// 同步失败必须可观测：错误返回版 SyncSchedulerAccountSnapshot 的错误经日志暴露，不静默丢失。
	require.Contains(t, logBuf.String(), "after commit failed",
		"提交后快照同步失败必须经错误返回版暴露（可观测，不得静默吞掉）")
	require.Contains(t, logBuf.String(), "sync read failed",
		"可观测日志应含真实同步错误原因")
}