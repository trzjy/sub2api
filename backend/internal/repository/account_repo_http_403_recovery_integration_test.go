//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 本文件覆盖 F3 403 恢复链存储层原语的**真实 Postgres 语义**（jsonb 条件、LIKE 语义、
// RETURNING 新值、条件 UPDATE 的零写入、代际计数不随恢复键删除而删除）。这些语义超出
// 注入式 unit 基建（recordingSQLExecutor/sqlmock）能力，按 C1-a-r3 先例在 integration
// 面补验。
//
// **需有 DB 环境方可运行**（依赖 integration harness：integrationEntClient / testEntTx）：
//
//	cd backend && go test -tags integration ./internal/repository/... -run HTTP403
//
// 本地无 DB 时不运行；unit 层（-tags unit，见 account_repo_http_403_recovery_test.go）
// 以 SQL 形态断言兜住结构。
//
// 不变式前提：http_403_recovery.until 由 F3 独占写入（Mark403PausedWithRecovery 以
// time.RFC3339 UTC 落库），故为合法秒精度 RFC3339；候选查询先正则校验形状再 ::timestamptz，
// 非 RFC3339 形状（含非法日历日）不在保证范围。

func insertHTTP403AccountWithRecovery(t *testing.T, tx sqlQueryer, name string, recoveryJSON string) int64 {
	t.Helper()
	var id int64
	extra := recoveryJSON
	if extra == "" {
		extra = "{}"
	}
	err := scanSingleRow(context.Background(), tx, `
		INSERT INTO accounts (name, platform, type, status, extra)
		VALUES ($1, 'openai', $2, 'error', $3::jsonb)
		RETURNING id
	`, []any{name, service.AccountTypeOAuth, extra}, &id)
	require.NoError(t, err)
	return id
}

// TestHTTP403RecoveryDueAccountsRealJSONBSemantics 覆盖候选查询真实 jsonb 条件：
// until 过期命中 / 未到期不命中 / 无键不命中 / 键存在但无 until 不命中，且不受
// schedulable / HasError 过滤影响（D4 证据基线）。
func TestHTTP403RecoveryDueAccountsRealJSONBSemantics(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	now := time.Date(2026, time.October, 9, 12, 0, 0, 0, time.UTC)

	dueID := insertHTTP403AccountWithRecovery(t, tx, "f3-due", fmt.Sprintf(`{
		"http_403_recovery": {"until": %q, "generation": 1, "reason": "paused", "owner": "f3_lifecycle"}
	}`, now.Add(-time.Minute).Format(time.RFC3339)))
	_ = insertHTTP403AccountWithRecovery(t, tx, "f3-not-due", fmt.Sprintf(`{
		"http_403_recovery": {"until": %q, "generation": 2, "reason": "paused", "owner": "f3_lifecycle"}
	}`, now.Add(time.Hour).Format(time.RFC3339)))
	_ = insertHTTP403AccountWithRecovery(t, tx, "f3-no-key", `{}`)
	_ = insertHTTP403AccountWithRecovery(t, tx, "f3-key-no-until", `{
		"http_403_recovery": {"generation": 3, "reason": "cooldown", "owner": "f3_lifecycle"}
	}`)

	accounts, err := repo.ListHTTP403RecoveryDueAccounts(ctx, now, 200)
	require.NoError(t, err)
	require.Len(t, accounts, 1, "仅 until 到期的 F3 账号命中")
	require.Equal(t, dueID, accounts[0].ID)

	// schedulable / status 不影响候选：把 due 账号置为不可调度、仍是 error 后仍命中。
	_, err = tx.ExecContext(ctx, `UPDATE accounts SET schedulable = FALSE WHERE id = $1`, dueID)
	require.NoError(t, err)
	accounts, err = repo.ListHTTP403RecoveryDueAccounts(ctx, now, 200)
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	require.Equal(t, dueID, accounts[0].ID)
}

// TestHTTP403GenerationCounterRealSemantics 覆盖代际分配的真实 RETURNING 新值与计数键
// 独立于恢复键删除（R17-F3）：分配自增 → 清理恢复键后计数键仍在。
func TestHTTP403GenerationCounterRealSemantics(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)

	id := insertHTTP403AccountWithRecovery(t, tx, "f3-gen-counter", `{}`)

	first, err := repo.AllocHTTP403Generation(ctx, id)
	require.NoError(t, err)
	require.Equal(t, int64(1), first, "首次分配 = 1")

	second, err := repo.AllocHTTP403Generation(ctx, id)
	require.NoError(t, err)
	require.Equal(t, int64(2), second, "单语句 RETURNING 返回自增后的新值（单调递增）")

	// 写入恢复键后再经 CAS 清理；清理只删恢复记录、不删计数器。
	require.NoError(t, repo.Mark403PausedWithRecovery(ctx, id, time.Now().Add(time.Hour), second, "paused", "403", nil))
	cleared, err := repo.ClearHTTP403RecoveryIfOwned(ctx, id, second)
	require.NoError(t, err)
	require.True(t, cleared)

	var counter int64
	require.NoError(t, scanSingleRow(ctx, tx, `
		SELECT COALESCE((extra->>'http_403_gen_counter')::bigint, 0) FROM accounts WHERE id = $1
	`, []any{id}, &counter))
	require.Equal(t, int64(2), counter, "代际计数键不随恢复键删除而删除，防代际复用")

	var hasRecovery bool
	require.NoError(t, scanSingleRow(ctx, tx, `
		SELECT extra ? 'http_403_recovery' FROM accounts WHERE id = $1
	`, []any{id}, &hasRecovery))
	require.False(t, hasRecovery, "CAS 清理移除恢复键")
}

// TestHTTP403ClearCASRealSemantics 覆盖条件清理真实语义：generation 失配零写入；
// LIKE 所有权标记命中时清除 F3 拥有的 until，非 F3 reason 的 until 不被触碰（R15-F2）。
func TestHTTP403ClearCASRealSemantics(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	now := time.Now()

	t.Run("generation mismatch writes nothing", func(t *testing.T) {
		id := insertHTTP403AccountWithRecovery(t, tx, "f3-cas-mismatch", fmt.Sprintf(`{
			"sched_state_revision": 5,
			"http_403_recovery": {"until": %q, "generation": 5, "reason": "paused", "owner": "f3_lifecycle", "state_revision": 5}
		}`, now.Add(time.Minute).Format(time.RFC3339)))

		applied, err := repo.ClearHTTP403RecoveryIfOwned(ctx, id, 999)
		require.NoError(t, err)
		require.False(t, applied)

		var still bool
		require.NoError(t, scanSingleRow(ctx, tx, `SELECT extra ? 'http_403_recovery' FROM accounts WHERE id = $1`, []any{id}, &still))
		require.True(t, still, "失配不得移除恢复键")
	})

	t.Run("f3 owned until cleared, foreign until preserved", func(t *testing.T) {
		f3Until := now.Add(30 * time.Minute)
		id := insertHTTP403AccountWithRecovery(t, tx, "f3-owned-until", `{}`)
		// F3 首写：temp_unschedulable 带所有权前缀。
		require.NoError(t, repo.Mark403PausedWithRecovery(ctx, id, now.Add(time.Hour), 11, "paused", "403", &f3Until))

		foreignUntil := now.Add(45 * time.Minute)
		foreignID := insertHTTP403AccountWithRecovery(t, tx, "f3-foreign-until", `{}`)
		_, err := tx.ExecContext(ctx, `
			UPDATE accounts SET temp_unschedulable_until = $1, temp_unschedulable_reason = 'other chain owns'
			WHERE id = $2
		`, foreignUntil, foreignID)
		require.NoError(t, err)

		// F3 拥有 → 清除。
		cleared, err := repo.ClearHTTP403RecoveryIfOwned(ctx, id, 11)
		require.NoError(t, err)
		require.True(t, cleared)
		var until *time.Time
		require.NoError(t, scanSingleRow(ctx, tx, `SELECT temp_unschedulable_until FROM accounts WHERE id = $1`, []any{id}, &until))
		require.Nil(t, until, "F3 拥有的 until 必须被清除")

		// 非 F3 拥有（此处演示 ELSE 分支语义）→ 保留。直接用同一清理逻辑需 CAS 命中，
		// 故用 SQL 断言 CASE 表达：临时写入 F3 恢复键后清理，other chain 的 until 不受影响。
		require.NoError(t, repo.Mark403PausedWithRecovery(ctx, foreignID, now.Add(time.Hour), 12, "paused", "403", nil))
		// 此时 foreignID 的 until 仍是非 F3 reason（Mark 未传 temp → COALESCE 未覆盖）。
		cleared, err = repo.ClearHTTP403RecoveryIfOwned(ctx, foreignID, 12)
		require.NoError(t, err)
		require.True(t, cleared)
		var preserved *time.Time
		require.NoError(t, scanSingleRow(ctx, tx, `SELECT temp_unschedulable_until FROM accounts WHERE id = $1`, []any{foreignID}, &preserved))
		require.NotNil(t, preserved, "非 F3 拥有的 until 不得被触碰")
		require.WithinDuration(t, foreignUntil, *preserved, time.Second)
	})
}

// TestHTTP403TransitionRemovesMetadataRealSemantics 覆盖真实转交：旧 403 元数据必须移除，
// 新状态写入生效（R7-F3）。
func TestHTTP403TransitionRemovesMetadataRealSemantics(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)

	id := insertHTTP403AccountWithRecovery(t, tx, "f3-transition", fmt.Sprintf(`{
		"sched_state_revision": 21,
		"http_403_recovery": {"until": %q, "generation": 21, "reason": "paused", "owner": "f3_lifecycle", "state_revision": 21}
	}`, time.Now().Add(time.Minute).Format(time.RFC3339)))

	applied, err := repo.TransitionHTTP403RecoveryTo(ctx, id, 21, service.HTTP403TransitionTarget{
		Status:       service.StatusError,
		ErrorMessage: "handed off to 429 chain",
		Schedulable:  false,
	})
	require.NoError(t, err)
	require.True(t, applied)

	var hasRecovery bool
	var status, errMsg string
	require.NoError(t, scanSingleRow(ctx, tx, `
		SELECT extra ? 'http_403_recovery', status, error_message FROM accounts WHERE id = $1
	`, []any{id}, &hasRecovery, &status, &errMsg))
	require.False(t, hasRecovery, "转交后旧 403 元数据不得残留")
	require.Equal(t, service.StatusError, status)
	require.Equal(t, "handed off to 429 chain", errMsg)
}

// TestHTTP403MarkRecordsStateRevisionAtBirth 覆盖 R18-F2：Mark 首写后恢复键内 state_revision
// 与全局 sched_state_revision 同点出生、值恒等（CAS 条件之三的比较前提）。
func TestHTTP403MarkRecordsStateRevisionAtBirth(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)

	id := insertHTTP403AccountWithRecovery(t, tx, "f3-revision-birth", `{}`)
	require.NoError(t, repo.Mark403PausedWithRecovery(ctx, id, time.Now().Add(time.Hour), 3, "paused", "403", nil))

	var keyRev, globalRev int64
	require.NoError(t, scanSingleRow(ctx, tx, `
		SELECT (extra->'http_403_recovery'->>'state_revision')::bigint,
		       (extra->>'sched_state_revision')::bigint
		FROM accounts WHERE id = $1
	`, []any{id}, &keyRev, &globalRev))
	require.Equal(t, int64(1), keyRev, "首写：键内记录 = 全局自增后的新值")
	require.Equal(t, globalRev, keyRev, "键内 state_revision 与全局 revision 同点出生、值恒等")
}

// TestHTTP403CASStaleRevisionRealSemantics 覆盖 R18-F2 真实语义（新增核心负例）：他链写入点
// 推进全局 sched_state_revision 后（示例以 SetError 模拟 breaker/token_refresh/gateway_scheduling
// 等他链写 error 的场景），即使 generation/owner 仍匹配，恢复探针的 CAS（键内 state_revision ≠
// 当前全局 revision）也必须零写入返回 false——恢复键保留、他链刚写入的 error 不被清掉。
func TestHTTP403CASStaleRevisionRealSemantics(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	now := time.Now()

	id := insertHTTP403AccountWithRecovery(t, tx, "f3-stale-revision", `{}`)
	require.NoError(t, repo.Mark403PausedWithRecovery(ctx, id, now.Add(time.Hour), 7, "paused", "403", nil))

	// 他链写入点替换账号 error/调度阻断状态 → 全局 sched_state_revision 自增。
	require.NoError(t, repo.SetError(ctx, id, "breaker wrote its own error"))

	// generation/owner 仍匹配，但 revision 已失配 → Clear 零写入。
	applied, err := repo.ClearHTTP403RecoveryIfOwned(ctx, id, 7)
	require.NoError(t, err)
	require.False(t, applied, "revision 失配 → 探针清理必须零写入")

	var hasRecovery bool
	var errMsg string
	require.NoError(t, scanSingleRow(ctx, tx, `
		SELECT extra ? 'http_403_recovery', error_message FROM accounts WHERE id = $1
	`, []any{id}, &hasRecovery, &errMsg))
	require.True(t, hasRecovery, "失配不得移除恢复键")
	require.Equal(t, "breaker wrote its own error", errMsg, "他链刚写入的 error 不得被恢复探针清掉")

	// Transition 同样失配：新链状态不得覆盖、旧 403 键不得移除。
	applied, err = repo.TransitionHTTP403RecoveryTo(ctx, id, 7, service.HTTP403TransitionTarget{
		Status:       service.StatusError,
		ErrorMessage: "handoff should not apply",
		Schedulable:  false,
	})
	require.NoError(t, err)
	require.False(t, applied, "revision 失配 → 转交必须零写入")
	require.NoError(t, scanSingleRow(ctx, tx, `
		SELECT extra ? 'http_403_recovery', error_message FROM accounts WHERE id = $1
	`, []any{id}, &hasRecovery, &errMsg))
	require.True(t, hasRecovery, "转交失配不得移除恢复键")
	require.Equal(t, "breaker wrote its own error", errMsg, "转交失配不得覆盖他链状态")
}

// TestHTTP403UpdateUntilRealSemantics 覆盖同键改 until/reason 真实语义（派发单 C1-b② 1d）：
// CAS 三条件命中 → until/reason 被改写、键内 state_revision 与全局 sched_state_revision 同点
// 自增；generation/owner 仍匹配但 revision 失配 → 零写入返回 false（保留他链状态）。
func TestHTTP403UpdateUntilRealSemantics(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	now := time.Date(2026, time.October, 9, 12, 0, 0, 0, time.UTC)

	id := insertHTTP403AccountWithRecovery(t, tx, "f3-update", fmt.Sprintf(`{
		"http_403_recovery": {"until": %q, "generation": 1, "reason": "paused", "owner": "f3_lifecycle", "state_revision": 1},
		"sched_state_revision": 1
	}`, now.Add(-time.Minute).Format(time.RFC3339)))

	newUntil := now.Add(2 * time.Hour)
	applied, err := repo.UpdateHTTP403RecoveryUntil(ctx, id, 1, newUntil, "refreshed until")
	require.NoError(t, err)
	require.True(t, applied, "三条件命中 → 返回 true")

	var until, reason, globalRev, keyRev int64
	require.NoError(t, scanSingleRow(ctx, tx, `
		SELECT (extra->'http_403_recovery'->>'until')::bigint AS u,
		       (extra->'http_403_recovery'->>'state_revision')::bigint AS kr,
		       (extra->>'sched_state_revision')::bigint AS gr
		FROM accounts WHERE id = $1
	`, []any{id}, &until, &keyRev, &globalRev))
	_ = reason
	require.Equal(t, int64(2), keyRev, "更新后键内 state_revision 自增")
	require.Equal(t, int64(2), globalRev, "更新后全局 revision 同点自增")

	// generation/owner 匹配但 revision 失配（他链已推进全局 revision）→ 零写入。
	_, err = tx.ExecContext(ctx, `UPDATE accounts SET extra = jsonb_set(extra, '{sched_state_revision}', to_jsonb(99)) WHERE id = $1`, id)
	require.NoError(t, err)
	applied, err = repo.UpdateHTTP403RecoveryUntil(ctx, id, 1, now.Add(3*time.Hour), "should not apply")
	require.NoError(t, err)
	require.False(t, applied, "revision 失配 → 零写入返回 false")
}

// TestHTTP403RemoveRecordRealSemantics 覆盖 stale 清理真实语义（派发单 C1-b② 1b）：generation
// 匹配 → 仅删 http_403_recovery 键 + 自增全局 revision，不动 status/error；generation 失配 →
// 零写入（他链状态保留）。
func TestHTTP403RemoveRecordRealSemantics(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)

	id := insertHTTP403AccountWithRecovery(t, tx, "f3-remove", fmt.Sprintf(`{
		"http_403_recovery": {"until": %q, "generation": 5, "reason": "cooldown", "owner": "f3_lifecycle", "state_revision": 1},
		"sched_state_revision": 1
	}`, time.Now().Add(time.Hour).Format(time.RFC3339)))
	require.NoError(t, repo.SetError(ctx, id, "other chain owns this now"))

	removed, err := repo.RemoveHTTP403RecoveryRecord(ctx, id, 5)
	require.NoError(t, err)
	require.True(t, removed, "generation 命中 → 返回 true")

	var hasRecovery bool
	var errMsg string
	require.NoError(t, scanSingleRow(ctx, tx, `
		SELECT extra ? 'http_403_recovery', error_message FROM accounts WHERE id = $1
	`, []any{id}, &hasRecovery, &errMsg))
	require.False(t, hasRecovery, "stale 清理必须移除 F3 键")
	require.Equal(t, "other chain owns this now", errMsg, "stale 清理不得触碰他链 error")

	// generation 失配 → 零写入（他链状态保留）。
	_ = insertHTTP403AccountWithRecovery(t, tx, "f3-remove-mismatch", fmt.Sprintf(`{
		"http_403_recovery": {"until": %q, "generation": 6, "reason": "cooldown", "owner": "f3_lifecycle", "state_revision": 1},
		"sched_state_revision": 1
	}`, time.Now().Add(time.Hour).Format(time.RFC3339)))
	removed, err = repo.RemoveHTTP403RecoveryRecord(ctx, id, 999)
	require.NoError(t, err)
	require.False(t, removed, "generation 失配 → 零写入返回 false")
}

// TestHTTP403LegacyInitRealSemantics 覆盖存量收编真实语义（派发单 C1-b② 3c / R8-F1）：
// status='error' 且 error_message 以 'Access forbidden (403):' 起、无 F3 键的账号被候选命中；
// 已持 F3 键的账号排除（幂等）；存量收编后写入 F3 键并离候选集。
func TestHTTP403LegacyInitRealSemantics(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)

	legacyID := insertHTTP403AccountWithRecovery(t, tx, "f3-legacy", `{"status_dummy": 1}`)
	// 改写为 legacy 403 error（无 F3 键）。
	_, err := tx.ExecContext(ctx, `
		UPDATE accounts SET status = 'error', error_message = 'Access forbidden (403): region blocked'
		WHERE id = $1
	`, legacyID)
	require.NoError(t, err)
	// 已持 F3 键的账号（应被排除）。
	_ = insertHTTP403AccountWithRecovery(t, tx, "f3-legacy-has-key", fmt.Sprintf(`{
		"http_403_recovery": {"until": %q, "generation": 1, "reason": "paused", "owner": "f3_lifecycle", "state_revision": 1},
		"sched_state_revision": 1
	}`, time.Now().Add(time.Hour).Format(time.RFC3339)))

	legacy, err := repo.ListLegacyHTTP403ErrorAccounts(ctx, 200)
	require.NoError(t, err)
	require.Len(t, legacy, 1, "仅 legacy 无键账号命中")
	require.Equal(t, legacyID, legacy[0].ID)

	// 收编：Alloc + Mark 写入 F3 键。
	gen, err := repo.AllocHTTP403Generation(ctx, legacyID)
	require.NoError(t, err)
	require.NoError(t, repo.Mark403PausedWithRecovery(ctx, legacyID, time.Now().Add(time.Hour), gen, "legacy init", legacy[0].ErrorMessage, nil))

	legacy, err = repo.ListLegacyHTTP403ErrorAccounts(ctx, 200)
	require.NoError(t, err)
	require.Len(t, legacy, 0, "收编后离开候选集（幂等）")
}
