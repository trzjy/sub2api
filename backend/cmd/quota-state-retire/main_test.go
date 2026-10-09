//go:build unit

package main

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/stretchr/testify/require"
)

// 本文件覆盖派发单 C1-c 退役命令 quota-state-retire 的 unit 层用例（注入式 sqlmock）。
// 真实 Postgres 语义（jsonb 条件值、行锁并发、CASE 求值）超出注入式 unit 基建能力，
// 按 C1-a-r3 / C1-b① 先例在 integration 面补真实语义用例；本层用 SQL 形态 + 语句序列
// 断言兜住结构，并逐条覆盖共存阻断 / 并发 / 幂等三组硬要求。
//
// "并发"用例采用本卡允许的形态：用注入 executor 串行化断言语句序列——退役 UPDATE 的
// WHERE 含代际 CAS，故并发新写入（generation 变化）时语句 0 行命中（sql.ErrNoRows），
// 退役不写、不丢新写入。

func newMockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db, mock
}

func candidateRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "has_f3", "f3_generation", "reason", "temp_unschedulable_until"})
}

func emptyPage() *sqlmock.Rows { return candidateRows() }

// TestRetire_NoCandidates_IdempotentZeroWrite 覆盖幂等：无目标账号时零写入、正常退出。
func TestRetire_NoCandidates_IdempotentZeroWrite(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery("SELECT id").WillReturnRows(emptyPage())
	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))

	sum, err := run(context.Background(), db, options{dryRun: false, batchSize: 200})

	require.NoError(t, err)
	require.Equal(t, int64(0), sum.Candidates)
	require.Equal(t, int64(0), sum.Processed)
	require.Equal(t, int64(0), sum.KeysRemoved())
	require.Equal(t, int64(0), sum.RemainingF3)
	require.NoError(t, mock.ExpectationsWereMet(), "无候选不得执行任何 UPDATE/INSERT")
}

// TestRetire_DryRun_NoWrites 覆盖 dry-run：只枚举统计、不写库。
func TestRetire_DryRun_NoWrites(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery("SELECT id").
		WillReturnRows(candidateRows().AddRow(int64(1), true, int64(3), "", nil))
	mock.ExpectQuery("SELECT id").WillReturnRows(emptyPage())
	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(1)))

	sum, err := run(context.Background(), db, options{dryRun: true, batchSize: 200})

	require.NoError(t, err)
	require.Equal(t, int64(1), sum.Candidates)
	require.Equal(t, int64(1), sum.F3KeysRemoved, "dry-run 报告将移除的键数")
	require.Equal(t, int64(0), sum.Processed)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestRetire_Execute_F3Only_UnfreezesRemovesKey 覆盖主路径：F3 独占冻结账号退役后
// 移除 F3 键 + 清除 F3 拥有的 error/调度阻断 + schedulable 重算为 true。
func TestRetire_Execute_F3Only_UnfreezesRemovesKey(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery("SELECT id").
		WillReturnRows(candidateRows().AddRow(int64(7), true, int64(5), "", nil))
	mock.ExpectQuery("UPDATE accounts").
		WillReturnRows(sqlmock.NewRows([]string{"schedulable"}).AddRow(true))
	mock.ExpectExec("INSERT INTO scheduler_outbox").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT id").WillReturnRows(emptyPage())
	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))

	sum, err := run(context.Background(), db, options{dryRun: false, batchSize: 200})

	require.NoError(t, err)
	require.Equal(t, int64(1), sum.Processed)
	require.Equal(t, int64(1), sum.F3KeysRemoved)
	require.Equal(t, int64(0), sum.SchemeTempKeysCleared)
	require.Equal(t, int64(1), sum.Unfrozen)
	require.Equal(t, int64(0), sum.KeptFrozen)
	require.Equal(t, int64(0), sum.RemainingF3)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestRetireUpdateSQL_Shape 覆盖单账号退役 UPDATE 的形态不变量（R20-F1 / R18-F2 / R19-F2）：
//   - schedulable 由 CASE 按剩余阻断重算，绝不直接常值置真；
//   - error 清除以 F3 所有权（revision 相等）为条件（他链 error 不被清）；
//   - temp_unschedulable 仅按本方案 reason 前缀清除（他链 reason 不动）；
//   - extra 用「删 F3 键 + 自增 sched_state_revision」同语句表达式，且不触碰代际计数键；
//   - F3 代际 CAS 守卫（并发新写入时零命中）。
func TestRetireUpdateSQL_Shape(t *testing.T) {
	t.Run("f3 owned", func(t *testing.T) {
		c := candidate{id: 7, hasF3: true, f3Generation: 5}
		query, args := retireUpdateSQL(c)
		q := normalizeSQL(query)

		require.Contains(t, q, "UPDATE accounts SET")
		require.Contains(t, q, "schedulable = CASE WHEN (", "schedulable 必须按剩余阻断重算")
		require.Contains(t, q, "status = CASE WHEN")
		require.NotContains(t, q, "schedulable = TRUE", "禁止直接把 schedulable 置真（R20-F1）")
		require.NotContains(t, q, "schedulable = FALSE,", "禁止直接把 schedulable 置假（须走 CASE 重算）")
		// 所有权：error 清除以 revision 相等为条件。
		require.Contains(t, q, "->>'state_revision')::bigint")
		require.Contains(t, q, "COALESCE((extra->>'"+kRev+"')::bigint, 0)")
		// 本方案 temp 清除以 reason 前缀为条件。
		require.Contains(t, q, "temp_unschedulable_reason LIKE '"+cnQuotaExhaustedReasonPrefix+"%'")
		// extra：删 F3 整键 + 自增 revision；不动代际计数键。
		require.Contains(t, q, "- '"+kF3+"'")
		require.Contains(t, q, "COALESCE((extra->>'"+kRev+"')::bigint, 0) + 1")
		require.NotContains(t, q, repository.HTTP403GenerationCounterExtraKey,
			"退役不得删除/写代际计数键（R17-F3：计数键独立于恢复键、防代际复用）")
		// CAS：代际匹配守卫。
		require.Contains(t, q, "extra ? '"+kF3+"' AND (extra->'"+kF3+"'->>'generation')::bigint = $2")
		require.Contains(t, q, "deleted_at IS NULL")
		require.Contains(t, q, "RETURNING schedulable")
		require.Len(t, args, 2)
		require.Equal(t, int64(7), args[0])
		require.Equal(t, int64(5), args[1])
	})

	t.Run("scheme temp only", func(t *testing.T) {
		until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
		c := candidate{id: 9, schemeTemp: true, tempReason: cnQuotaExhaustedReasonPrefix + ": exhausted", tempUntil: sql.NullTime{Time: until, Valid: true}}
		query, args := retireUpdateSQL(c)
		q := normalizeSQL(query)

		// temp 快照 CAS（reason + until 不变才清），保证并发新写入不被误清。
		require.Contains(t, q, "temp_unschedulable_reason = $2")
		require.Contains(t, q, "temp_unschedulable_until IS NOT DISTINCT FROM $3")
		require.NotContains(t, q, "(extra->'"+kF3+"'->>'generation')::bigint = $",
			"无 F3 键时不得加代际 CAS")
		require.Len(t, args, 3)
		require.Equal(t, int64(9), args[0])
		require.Equal(t, c.tempReason, args[1])
		require.Equal(t, until, args[2])
	})

	t.Run("null scheme temp until uses null-safe compare", func(t *testing.T) {
		c := candidate{id: 11, schemeTemp: true, tempReason: cnQuotaExhaustedReasonPrefix}
		query, args := retireUpdateSQL(c)
		require.Contains(t, normalizeSQL(query), "temp_unschedulable_until IS NOT DISTINCT FROM $3")
		require.Nil(t, args[2], "无效 until 以 NULL 参与 IS NOT DISTINCT FROM")
	})
}

// TestRetire_Execute_CoexistOtherError_KeepsFrozen 覆盖共存阻断 1：F3 键 + 本方案外
// error（如 401，经共享原语 SetError 写入并使用全局 revision 自增 → F3 键内 state_revision
// 失配）并存。退役移除 F3 键，但因 error 不由 F3 拥有（revision 失配）→ 维持冻结。
func TestRetire_Execute_CoexistOtherError_KeepsFrozen(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery("SELECT id").
		WillReturnRows(candidateRows().AddRow(int64(3), true, int64(9), "", nil))
	// 重算仍被 status=error（他链）阻断 → schedulable=false。
	mock.ExpectQuery("UPDATE accounts").
		WillReturnRows(sqlmock.NewRows([]string{"schedulable"}).AddRow(false))
	mock.ExpectExec("INSERT INTO scheduler_outbox").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT id").WillReturnRows(emptyPage())
	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))

	sum, err := run(context.Background(), db, options{dryRun: false, batchSize: 200})

	require.NoError(t, err)
	require.Equal(t, int64(1), sum.Processed, "F3 键被移除（代际 CAS 命中）")
	require.Equal(t, int64(1), sum.F3KeysRemoved)
	require.Equal(t, int64(0), sum.Unfrozen)
	require.Equal(t, int64(1), sum.KeptFrozen, "他链 error 仍阻断 → 维持冻结")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestRetire_Execute_CoexistNonSchemeTemp_KeepsUntil 覆盖共存阻断 2：F3 键 + 非本方案
// reason 的 temp_unschedulable 未过期并存。退役移除 F3 键，但 until（他链 reason）保留、
// 账号维持冻结。
func TestRetire_Execute_CoexistNonSchemeTemp_KeepsUntil(t *testing.T) {
	until := time.Now().Add(2 * time.Hour)
	db, mock := newMockDB(t)
	// reason 非本方案前缀 → schemeTemp=false → 不进 temp 清除分支。
	mock.ExpectQuery("SELECT id").
		WillReturnRows(candidateRows().AddRow(int64(4), true, int64(2), "token refresh retry exhausted: x", until))
	mock.ExpectQuery("UPDATE accounts").
		WillReturnRows(sqlmock.NewRows([]string{"schedulable"}).AddRow(false))
	mock.ExpectExec("INSERT INTO scheduler_outbox").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT id").WillReturnRows(emptyPage())
	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))

	sum, err := run(context.Background(), db, options{dryRun: false, batchSize: 200})

	require.NoError(t, err)
	require.Equal(t, int64(1), sum.Processed)
	require.Equal(t, int64(1), sum.F3KeysRemoved)
	require.Equal(t, int64(0), sum.SchemeTempKeysCleared, "非本方案 reason 的 until 不得清除")
	require.Equal(t, int64(1), sum.KeptFrozen)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestRetire_Execute_SchemeTempOnly_ClearsTemp 覆盖 F1-only 账号：只清本方案 temp，
// status/schedulable 不动，重算后恢复可调度。
func TestRetire_Execute_SchemeTempOnly_ClearsTemp(t *testing.T) {
	until := time.Now().Add(30 * time.Minute)
	db, mock := newMockDB(t)
	mock.ExpectQuery("SELECT id").
		WillReturnRows(candidateRows().AddRow(int64(8), false, int64(0), cnQuotaExhaustedReasonPrefix+": exhausted", until))
	mock.ExpectQuery("UPDATE accounts").
		WillReturnRows(sqlmock.NewRows([]string{"schedulable"}).AddRow(true))
	mock.ExpectExec("INSERT INTO scheduler_outbox").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT id").WillReturnRows(emptyPage())
	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))

	sum, err := run(context.Background(), db, options{dryRun: false, batchSize: 200})

	require.NoError(t, err)
	require.Equal(t, int64(1), sum.Processed)
	require.Equal(t, int64(0), sum.F3KeysRemoved)
	require.Equal(t, int64(1), sum.SchemeTempKeysCleared)
	require.Equal(t, int64(1), sum.Unfrozen)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestRetire_Execute_ConcurrentNewF3Write_CASSkips 覆盖退役并发：枚举后该账号被写入新 F3
// 状态（新 generation）→ 退役 UPDATE 代际 CAS 失配返回 sql.ErrNoRows → 零写入、不计 processed，
// 新写入不丢。
func TestRetire_Execute_ConcurrentNewF3Write_CASSkips(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery("SELECT id").
		WillReturnRows(candidateRows().AddRow(int64(5), true, int64(3), "", nil))
	mock.ExpectQuery("UPDATE accounts").WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT id").WillReturnRows(emptyPage())
	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(1)))

	sum, err := run(context.Background(), db, options{dryRun: false, batchSize: 200})

	require.NoError(t, err)
	require.Equal(t, int64(1), sum.Candidates)
	require.Equal(t, int64(0), sum.Processed)
	require.Equal(t, int64(1), sum.Skipped, "CAS 失配 → 跳过、不丢新写入")
	require.Equal(t, int64(1), sum.RemainingF3, "新写入仍在，闸门未满足")
	require.NoError(t, mock.ExpectationsWereMet(), "CAS 失配不得触发 outbox")
}

// TestIsSchemeReason 覆盖本方案 reason 归属判定（纯函数）。
func TestIsSchemeReason(t *testing.T) {
	require.True(t, isSchemeReason(cnQuotaExhaustedReasonPrefix))
	require.True(t, isSchemeReason(cnQuotaExhaustedReasonPrefix+": exhausted；停调至官方恢复时间 ..."))
	require.False(t, isSchemeReason("token refresh retry exhausted: x"))
	require.False(t, isSchemeReason(""))
	require.False(t, isSchemeReason("http_403_recovery_f3: paused"))
}

// TestRetire_Execute_BatchPagination 覆盖 id 游标分页：多批枚举 + 逐账号退役。
func TestRetire_Execute_BatchPagination(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery("SELECT id").WillReturnRows(candidateRows().
		AddRow(int64(1), true, int64(1), "", nil).
		AddRow(int64(2), true, int64(1), "", nil))
	mock.ExpectQuery("UPDATE accounts").WillReturnRows(sqlmock.NewRows([]string{"schedulable"}).AddRow(true))
	mock.ExpectExec("INSERT INTO scheduler_outbox").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("UPDATE accounts").WillReturnRows(sqlmock.NewRows([]string{"schedulable"}).AddRow(false))
	mock.ExpectExec("INSERT INTO scheduler_outbox").WillReturnResult(sqlmock.NewResult(0, 1))
	// 第二批（游标 > 2）：mock 不感知参数，此处模拟运维期新出现的 F1-only 目标。
	mock.ExpectQuery("SELECT id").
		WillReturnRows(candidateRows().AddRow(int64(3), false, int64(0), cnQuotaExhaustedReasonPrefix, nil))
	mock.ExpectQuery("UPDATE accounts").WillReturnRows(sqlmock.NewRows([]string{"schedulable"}).AddRow(true))
	mock.ExpectExec("INSERT INTO scheduler_outbox").WillReturnResult(sqlmock.NewResult(0, 1))
	// 第三批：空集结束。
	mock.ExpectQuery("SELECT id").WillReturnRows(emptyPage())
	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))

	sum, err := run(context.Background(), db, options{dryRun: false, batchSize: 2})

	require.NoError(t, err)
	require.Equal(t, int64(3), sum.Processed)
	require.Equal(t, int64(2), sum.F3KeysRemoved)
	require.Equal(t, int64(1), sum.SchemeTempKeysCleared)
	require.Equal(t, int64(2), sum.Unfrozen)
	require.Equal(t, int64(1), sum.KeptFrozen)
	require.NoError(t, mock.ExpectationsWereMet())
}

func normalizeSQL(sql string) string { return strings.Join(strings.Fields(sql), " ") }
