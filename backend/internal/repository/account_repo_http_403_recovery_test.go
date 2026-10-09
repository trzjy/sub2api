//go:build unit

package repository

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 本文件覆盖派发单 C1-b① F3 403 恢复链存储层原语。真实 Postgres 语义（jsonb 条件、
// 并发、LIKE 语义、RETURNING 实际值）超出注入式 unit 基建能力，按 C1-a-r3 先例在
// //go:build integration 面补真实语义用例（见 account_repo_http_403_recovery_integration_test.go，
// 需 DB 环境补验）；本层用 SQL 形态断言兜住结构。

func captureRepoForQuery(t *testing.T) (*accountRepository, *string, *[]any, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	var captured string
	var args []any
	repo := newAccountRepositoryWithSQL(nil, captureQuerySQL{db: db, captured: &captured, args: &args}, nil)
	return repo, &captured, &args, mock
}

// TestAccountRepository_AllocHTTP403Generation_SingleStatementReturning 覆盖代际分配：
// 单语句 RETURNING 形态 + 计数键独立于恢复键（形态层，R17-F3/R19-F1）。
func TestAccountRepository_AllocHTTP403Generation_SingleStatementReturning(t *testing.T) {
	repo, captured, args, mock := captureRepoForQuery(t)
	mock.ExpectQuery("UPDATE accounts").
		WillReturnRows(sqlmock.NewRows([]string{"generation"}).AddRow(int64(7)))

	generation, err := repo.AllocHTTP403Generation(context.Background(), 42)

	require.NoError(t, err)
	require.Equal(t, int64(7), generation, "RETURNING 自增后的新计数值")
	require.NoError(t, mock.ExpectationsWereMet())

	normalized := normalizeSQLWhitespace(*captured)
	require.Contains(t, normalized, "UPDATE accounts")
	require.Contains(t, normalized, "jsonb_set")
	require.Contains(t, normalized, HTTP403GenerationCounterExtraKey)
	require.Contains(t, normalized, "RETURNING")
	require.Contains(t, normalized, "deleted_at IS NULL")
	require.NotContains(t, normalized, HTTP403RecoveryExtraKey,
		"代际计数键独立于恢复键：分配语句不得触碰 http_403_recovery")
	require.NotContains(t, normalized, "SELECT extra->>'"+HTTP403GenerationCounterExtraKey+"' FROM",
		"禁止读-改-写两步：不得出现先读计数器的 SELECT")
	require.Len(t, *args, 1)
	require.Equal(t, int64(42), (*args)[0])
}

// TestAccountRepository_Mark403PausedWithRecovery_AtomicFieldEffects 覆盖首次写入：
// 原子单语句形态 + 复刻 SetError 链字段效果 + 恢复键 JSON 形态 + 所有权标记。
func TestAccountRepository_Mark403PausedWithRecovery_AtomicFieldEffects(t *testing.T) {
	t.Run("with temp unschedulable writes ownership-marked reason", func(t *testing.T) {
		exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
		repo := newAccountRepositoryWithSQL(nil, exec, nil)
		until := time.Now().Add(45 * time.Minute).UTC().Truncate(time.Second)
		tempUntil := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)

		err := repo.Mark403PausedWithRecovery(context.Background(), 42, until, 9, "paused until upstream", "Access forbidden (403)", &tempUntil)

		require.NoError(t, err)
		require.Len(t, exec.execQueries, 2, "首写为单条 UPDATE，命中后追加 scheduler_outbox")
		normalized := normalizeSQLWhitespace(exec.execQueries[0])
		require.Contains(t, normalized, "UPDATE accounts")
		// 复刻既有 SetError 字段效果（逐列对照表见完工报告）。
		require.Contains(t, normalized, "status = $1")
		require.Contains(t, normalized, "error_message = $2")
		require.Contains(t, normalized, "schedulable = FALSE")
		// 恢复键在同语句内合并（原子首写）+ revision 自增。
		require.Contains(t, normalized, "$5::jsonb")
		require.Contains(t, normalized, SchedStateRevisionExtraKey)
		// R18-F2：恢复键内 state_revision 记录与全局 sched_state_revision 复用同一 +1 表达式，
		// 在同一语句内同点出生、值恒等（CAS 条件之三的比较前提）。
		require.Contains(t, normalized, "'{"+HTTP403RecoveryExtraKey+","+SchedStateRevisionExtraKey+"}'",
			"恢复键内须写入 state_revision 记录（嵌套路径）")
		require.Contains(t, normalized, "COALESCE((extra->>'"+SchedStateRevisionExtraKey+"')::bigint, 0) + 1",
			"键内记录与全局自增共用同一表达式，保证同点出生")
		// 可选 temp_unschedulable 使用 COALESCE 保留旧值语义。
		require.Contains(t, normalized, "temp_unschedulable_until = COALESCE($3, temp_unschedulable_until)")
		require.Contains(t, normalized, "temp_unschedulable_reason = COALESCE($4, temp_unschedulable_reason)")
		require.Contains(t, normalized, "deleted_at IS NULL")
		require.Contains(t, normalizeSQLWhitespace(exec.execQueries[1]), "INSERT INTO scheduler_outbox")

		require.Len(t, exec.execArgs[0], 6)
		require.Equal(t, service.StatusError, exec.execArgs[0][0])
		require.Equal(t, "Access forbidden (403)", exec.execArgs[0][1])
		require.Equal(t, tempUntil, exec.execArgs[0][2])
		require.Equal(t, HTTP403RecoveryTempUnschedulableReasonPrefix+"paused until upstream", exec.execArgs[0][3])
		require.Equal(t, int64(42), exec.execArgs[0][5])

		var payload map[string]any
		require.NoError(t, json.Unmarshal([]byte(exec.execArgs[0][4].(string)), &payload))
		require.Equal(t, float64(9), payload["generation"])
		require.Equal(t, HTTP403RecoveryOwnerF3, payload["owner"], "owner 固定 F3 值，CAS 条件之一")
		require.Equal(t, "paused until upstream", payload["reason"])
		require.Equal(t, until.Format(time.RFC3339), payload["until"])
	})

	t.Run("without temp unschedulable does not touch until", func(t *testing.T) {
		exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
		repo := newAccountRepositoryWithSQL(nil, exec, nil)

		err := repo.Mark403PausedWithRecovery(context.Background(), 42, time.Now().Add(time.Hour), 3, "reason", "err", nil)

		require.NoError(t, err)
		require.Len(t, exec.execArgs[0], 6)
		require.Nil(t, exec.execArgs[0][2], "tempUntil=nil → COALESCE 保留旧值")
		require.Nil(t, exec.execArgs[0][3])
	})

	t.Run("missing account returns not found without outbox", func(t *testing.T) {
		exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
		repo := newAccountRepositoryWithSQL(nil, exec, nil)

		err := repo.Mark403PausedWithRecovery(context.Background(), 42, time.Now(), 1, "r", "e", nil)

		require.ErrorIs(t, err, service.ErrAccountNotFound)
		require.Len(t, exec.execQueries, 1, "0 行不得触发 outbox")
	})
}

// TestAccountRepository_ClearHTTP403RecoveryIfOwned_CAS 覆盖 CAS 清理四用例：
// generation+owner 匹配生效 / 失配零写入 / F3 拥有 until 被清除 / 非 F3 拥有 until 不被触碰。
// 前两例为行为断言，后两例为 SQL 形态断言（真实条件语义入 integration 面）。
func TestAccountRepository_ClearHTTP403RecoveryIfOwned_CAS(t *testing.T) {
	t.Run("generation and owner match applies and clears F3-owned until", func(t *testing.T) {
		exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
		repo := newAccountRepositoryWithSQL(nil, exec, nil)

		applied, err := repo.ClearHTTP403RecoveryIfOwned(context.Background(), 42, 7)

		require.NoError(t, err)
		require.True(t, applied)
		require.Len(t, exec.execQueries, 2, "命中 → 单条 UPDATE + outbox")
		normalized := normalizeSQLWhitespace(exec.execQueries[0])
		require.Contains(t, normalized, "deleted_at IS NULL")
		require.Contains(t, normalized, "extra->'"+HTTP403RecoveryExtraKey+"'->>'generation' = $2")
		require.Contains(t, normalized, "extra->'"+HTTP403RecoveryExtraKey+"'->>'owner' = $5")
		// R18-F2 条件之三：键内 state_revision 与当前全局 sched_state_revision 同语句相等
		// （列对列比较，任何他链状态替换自增全局 revision 即失配）。
		require.Contains(t, normalized,
			"extra->'"+HTTP403RecoveryExtraKey+"'->>'state_revision' = extra->>'"+SchedStateRevisionExtraKey+"'")
		// 恢复调度（复刻 ClearError/SetSchedulable(true) 字段效果）。
		require.Contains(t, normalized, "status = $3")
		require.Contains(t, normalized, "error_message = ''")
		require.Contains(t, normalized, "schedulable = TRUE")
		// 只有 F3 拥有的 until（reason 前缀命中）才清除，否则保留。
		require.Contains(t, normalized, "temp_unschedulable_reason LIKE $4 || '%'")
		require.Contains(t, normalized, "ELSE temp_unschedulable_until END")
		// 删除恢复键 + revision 自增；不触碰独立计数键。
		require.Contains(t, normalized, "- '"+HTTP403RecoveryExtraKey+"'")
		require.Contains(t, normalized, SchedStateRevisionExtraKey)
		require.NotContains(t, normalized, HTTP403GenerationCounterExtraKey)
		require.Contains(t, normalizeSQLWhitespace(exec.execQueries[1]), "INSERT INTO scheduler_outbox")

		require.Len(t, exec.execArgs[0], 5)
		require.Equal(t, int64(42), exec.execArgs[0][0])
		require.Equal(t, "7", exec.execArgs[0][1], "generation 以文本比较，避免 jsonb 数字强转错误")
		require.Equal(t, service.StatusActive, exec.execArgs[0][2])
		require.Equal(t, HTTP403RecoveryTempUnschedulableReasonPrefix, exec.execArgs[0][3])
		require.Equal(t, HTTP403RecoveryOwnerF3, exec.execArgs[0][4])
	})

	t.Run("mismatch writes nothing and emits no outbox", func(t *testing.T) {
		exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
		repo := newAccountRepositoryWithSQL(nil, exec, nil)

		applied, err := repo.ClearHTTP403RecoveryIfOwned(context.Background(), 42, 7)

		require.NoError(t, err)
		require.False(t, applied, "任一 CAS 条件失配 → false 且零写入")
		require.Len(t, exec.execQueries, 1, "失配不触发 outbox")
		require.NotContains(t, strings.Join(exec.execQueries, "\n"), "scheduler_outbox")
	})

	t.Run("non F3 owned until preserved by CASE else branch", func(t *testing.T) {
		// 形态层：CASE 未命中（reason 不以 F3 前缀开头）时保留原 until/reason。
		exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
		repo := newAccountRepositoryWithSQL(nil, exec, nil)

		_, err := repo.ClearHTTP403RecoveryIfOwned(context.Background(), 42, 7)

		require.NoError(t, err)
		normalized := normalizeSQLWhitespace(exec.execQueries[0])
		require.Contains(t, normalized, "ELSE temp_unschedulable_until END",
			"非 F3 拥有的 until 不被触碰")
		require.Contains(t, normalized, "ELSE temp_unschedulable_reason END",
			"非 F3 拥有的 reason 不被触碰")
	})
}

// TestAccountRepository_TransitionHTTP403RecoveryTo_Atomic 覆盖转交：移除 403 元数据 +
// 新状态写入同语句形态 + CAS 失配零写入。
func TestAccountRepository_TransitionHTTP403RecoveryTo_Atomic(t *testing.T) {
	target := HTTP403TransitionTarget{Status: service.StatusError, ErrorMessage: "handed off 401", Schedulable: false}

	t.Run("applied removes recovery metadata and writes target state", func(t *testing.T) {
		exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
		repo := newAccountRepositoryWithSQL(nil, exec, nil)

		applied, err := repo.TransitionHTTP403RecoveryTo(context.Background(), 42, 4, target)

		require.NoError(t, err)
		require.True(t, applied)
		require.Len(t, exec.execQueries, 2)
		normalized := normalizeSQLWhitespace(exec.execQueries[0])
		require.Contains(t, normalized, "UPDATE accounts")
		require.Contains(t, normalized, "status = $3")
		require.Contains(t, normalized, "error_message = $4")
		require.Contains(t, normalized, "schedulable = $5")
		require.Contains(t, normalized, "- '"+HTTP403RecoveryExtraKey+"'",
			"R7-F3：转交必须原子移除旧 403 元数据，不得残留")
		require.Contains(t, normalized, SchedStateRevisionExtraKey)
		require.NotContains(t, normalized, HTTP403GenerationCounterExtraKey)
		require.Contains(t, normalized, "extra->'"+HTTP403RecoveryExtraKey+"'->>'generation' = $2")
		require.Contains(t, normalized, "extra->'"+HTTP403RecoveryExtraKey+"'->>'owner' = $8")
		require.Contains(t, normalized,
			"extra->'"+HTTP403RecoveryExtraKey+"'->>'state_revision' = extra->>'"+SchedStateRevisionExtraKey+"'",
			"R18-F2：转交 CAS 同含 revision 相等条件")
		require.Contains(t, normalized, "temp_unschedulable_reason LIKE $9 || '%'",
			"R15-F2：转交按所有权标记条件清除 F3 拥有的 until")
		require.Contains(t, normalizeSQLWhitespace(exec.execQueries[1]), "INSERT INTO scheduler_outbox")
		require.Len(t, exec.execArgs[0], 9)
		require.Equal(t, "4", exec.execArgs[0][1])
		require.Equal(t, service.StatusError, exec.execArgs[0][2])
		require.Equal(t, false, exec.execArgs[0][4])
	})

	t.Run("cas mismatch writes nothing", func(t *testing.T) {
		exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
		repo := newAccountRepositoryWithSQL(nil, exec, nil)

		applied, err := repo.TransitionHTTP403RecoveryTo(context.Background(), 42, 4, target)

		require.NoError(t, err)
		require.False(t, applied)
		require.Len(t, exec.execQueries, 1)
	})
}

// TestAccountRepository_ListHTTP403RecoveryDueAccounts_SQLShape 覆盖候选查询三用例：
// until 过期命中 / 未到期不命中 / 无键不命中（形态层）+ 默认 limit。
func TestAccountRepository_ListHTTP403RecoveryDueAccounts_SQLShape(t *testing.T) {
	repo, captured, args, mock := captureRepoForQuery(t)
	mock.ExpectQuery("SELECT id").WillReturnRows(sqlmock.NewRows([]string{"id"}))

	out, err := repo.ListHTTP403RecoveryDueAccounts(context.Background(), time.Now(), 0)

	require.NoError(t, err)
	require.Empty(t, out)
	require.NoError(t, mock.ExpectationsWereMet())

	normalized := normalizeSQLWhitespace(*captured)
	require.Contains(t, normalized, "deleted_at IS NULL")
	// 无键不命中：必须是对象且 until 非空。
	require.Contains(t, normalized, "jsonb_typeof(extra->'"+HTTP403RecoveryExtraKey+"') = 'object'")
	require.Contains(t, normalized, "extra->'"+HTTP403RecoveryExtraKey+"'->>'until' IS NOT NULL")
	// until 过期命中 / 未到期不命中：仅 until <= now 入选。
	require.Contains(t, normalized, "(extra->'"+HTTP403RecoveryExtraKey+"'->>'until')::timestamptz <= $1")
	require.Contains(t, normalized, "ORDER BY")
	require.Contains(t, normalized, "ASC")
	require.Contains(t, normalized, "LIMIT $2")
	// F3 行硬要求：候选不受 schedulable / HasError / 快照新鲜度过滤影响。
	require.NotContains(t, normalized, "schedulable",
		"持有 F3 状态的账号必须被候选覆盖，不受 schedulable 过滤")
	require.NotContains(t, normalized, "status",
		"不得按 HasError/status 过滤")

	require.Len(t, *args, 2)
	require.Equal(t, 200, (*args)[1], "limit 默认 200（对齐 ListTempUnschedulableAccounts）")
}

// TestAccountRepository_SchedStateRevision_MaintainedInSharedPrimitives 覆盖 R19-F2：
// SetError / SetTempUnschedulable / ClearTempUnschedulable 均在单语句内自增
// sched_state_revision，且不触碰 F3 代际计数键。
func TestAccountRepository_SchedStateRevision_MaintainedInSharedPrimitives(t *testing.T) {
	primitives := []struct {
		name string
		call func(context.Context, *accountRepository) error
	}{
		{
			name: "SetError",
			call: func(ctx context.Context, repo *accountRepository) error {
				return repo.SetError(ctx, 42, "boom")
			},
		},
		{
			name: "SetTempUnschedulable",
			call: func(ctx context.Context, repo *accountRepository) error {
				return repo.SetTempUnschedulable(ctx, 42, time.Now().Add(time.Minute), "reason")
			},
		},
		{
			name: "ClearTempUnschedulable",
			call: func(ctx context.Context, repo *accountRepository) error {
				return repo.ClearTempUnschedulable(ctx, 42)
			},
		},
		{
			name: "ClearError",
			call: func(ctx context.Context, repo *accountRepository) error {
				return repo.ClearError(ctx, 42)
			},
		},
		{
			name: "SetSchedulable",
			call: func(ctx context.Context, repo *accountRepository) error {
				return repo.SetSchedulable(ctx, 42, false)
			},
		},
	}

	for _, primitive := range primitives {
		t.Run(primitive.name, func(t *testing.T) {
			exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
			repo := newAccountRepositoryWithSQL(nil, exec, nil)

			require.NoError(t, primitive.call(context.Background(), repo))
			require.NotEmpty(t, exec.execQueries)
			normalized := normalizeSQLWhitespace(exec.execQueries[0])
			require.Contains(t, normalized, "UPDATE accounts")
			require.Contains(t, normalized, SchedStateRevisionExtraKey,
				"%s 必须在同一语句内维护 sched_state_revision", primitive.name)
			require.Contains(t, normalized, "jsonb_set")
			require.Contains(t, normalized, "COALESCE((extra->>'"+SchedStateRevisionExtraKey+"')::bigint, 0) + 1")
			require.NotContains(t, normalized, HTTP403GenerationCounterExtraKey,
				"revision 与 F3 generation 计数键语义不同，不得互相写入")
		})
	}
}

// TestAccountRepository_HTTP403CAS_StaleRevisionNullifies 覆盖 R18-F2 新增 core 用例的
// **SQL 形态层**：Clear/Transition 两条 CAS 均把「键内 state_revision = 当前全局
// sched_state_revision」纳入同一条件 UPDATE（列对列，无参数），并在失配（0 行）时零写入、
// 不触发 outbox。真实「他链 SetError 推进 revision → CAS 失配」语义见 integration 面
// （account_repo_http_403_recovery_integration_test.go）。
func TestAccountRepository_HTTP403CAS_StaleRevisionNullifies(t *testing.T) {
	revisionCondition := "extra->'" + HTTP403RecoveryExtraKey + "'->>'state_revision' = extra->>'" +
		SchedStateRevisionExtraKey + "'"

	t.Run("clear", func(t *testing.T) {
		exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
		repo := newAccountRepositoryWithSQL(nil, exec, nil)

		applied, err := repo.ClearHTTP403RecoveryIfOwned(context.Background(), 42, 7)

		require.NoError(t, err)
		require.False(t, applied, "revision 失配 → 零写入返回 false")
		require.Len(t, exec.execQueries, 1, "失配不触发 outbox")
		require.Contains(t, normalizeSQLWhitespace(exec.execQueries[0]), revisionCondition)
	})

	t.Run("transition", func(t *testing.T) {
		exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
		repo := newAccountRepositoryWithSQL(nil, exec, nil)

		applied, err := repo.TransitionHTTP403RecoveryTo(context.Background(), 42, 7,
			HTTP403TransitionTarget{Status: service.StatusError, ErrorMessage: "x", Schedulable: false})

		require.NoError(t, err)
		require.False(t, applied, "revision 失配 → 零写入返回 false")
		require.Len(t, exec.execQueries, 1, "失配不触发 outbox")
		require.Contains(t, normalizeSQLWhitespace(exec.execQueries[0]), revisionCondition)
	})
}
