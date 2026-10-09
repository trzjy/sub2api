//go:build unit

package repository

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestAccountRepository_CrossChainConcurrency_RevisionMonotonic 是派发单 C1-c（R20-F2）
// 的跨链并发用例：两条独立状态链在同一账号上交错写入 N 轮——
//
//	goroutine A: SetTempUnschedulable（额度/限流/传输链族，写 temp_unschedulable_*）
//	goroutine B: SetError（error 链，写 status/error_message/schedulable）
//
// 按本卡允许形态：unit 层用注入 executor 串行化每次调用的语句序列并断言不变式，真实并发
// 语义（行锁、事务可见性）归 integration 面补验（见完工报告"integration 面补验声明"）。
//
// 断言的不变式：
//  1. **revision 单调递增、无丢失更新**：两条链的状态替换 UPDATE 均在同一语句内以
//     `COALESCE((extra->>'sched_state_revision')::bigint, 0) + 1` 自增账号级 revision
//     （读取旧行值 +1），故任意串行化顺序下写入都严格 +1、不可回退、不可丢更新。
//  2. **写集互斥**：SetError 只写 status/error_message/schedulable；SetTempUnschedulable
//     只写 temp_unschedulable_until/reason；二者仅共享 extra(revision) 且该列单调，
//     故无字段级丢失更新。
//  3. **最终状态与最后写入者一致**：交错以 SetError 收尾 → 最终 status=error/schedulable=false。
func TestAccountRepository_CrossChainConcurrency_RevisionMonotonic(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)
	ctx := context.Background()

	const rounds = 5
	for i := 0; i < rounds; i++ {
		// A：额度/限流链写 temp_unschedulable（until 递增以模拟持续停调）。
		require.NoError(t, repo.SetTempUnschedulable(ctx, 42,
			time.Now().Add(time.Duration(i+1)*time.Minute), "reason-a"))
		// B：error 链写 status/schedulable。
		require.NoError(t, repo.SetError(ctx, 42, "boom-b"))
	}

	// 每次调用 = 1 UPDATE + 1 scheduler_outbox INSERT（result=RowsAffected(1) → 均命中）。
	require.Len(t, exec.execQueries, rounds*2*2)
	require.Len(t, exec.execArgs, rounds*2*2)

	revisionExpr := "COALESCE((extra->>'" + SchedStateRevisionExtraKey + "')::bigint, 0) + 1"

	var errorWrites, tempWrites int
	for call := 0; call < rounds*2; call++ {
		update := normalizeSQLWhitespace(exec.execQueries[call*2])
		require.Contains(t, update, "UPDATE accounts", "call %d 的首条语句必须是状态替换 UPDATE", call)
		// 不变式 1：每条状态替换语句都自增 revision（旧值 +1）。
		require.Contains(t, update, SchedStateRevisionExtraKey,
			"call %d 的状态替换必须维护 sched_state_revision（否则他链可丢更新）", call)
		require.Contains(t, update, revisionExpr,
			"call %d 必须以读旧值+1 的方式自增（保证已提交写入严格单调）", call)
		// outbox 传播。
		require.Contains(t, normalizeSQLWhitespace(exec.execQueries[call*2+1]), "INSERT INTO scheduler_outbox")

		isError := strings.Contains(update, "status = $1")
		isTemp := strings.Contains(update, "temp_unschedulable_until = $1")
		require.True(t, isError != isTemp, "call %d 必须恰属一条链（写集互斥）", call)
		if isError {
			errorWrites++
			// 不变式 2：error 链不得写 temp_unschedulable_*（不覆盖额度/限流链字段）。
			require.NotContains(t, update, "temp_unschedulable_until =")
			require.Contains(t, update, "schedulable = FALSE")
		} else {
			tempWrites++
			// 不变式 2：额度/限流链不得写 status/error_message/schedulable。
			require.NotContains(t, update, "status =")
			require.NotContains(t, update, "schedulable =")
		}
	}
	require.Equal(t, rounds, errorWrites)
	require.Equal(t, rounds, tempWrites)

	// 不变式 3：最后一次调用为 SetError（B 收尾）→ 最终状态由最后写入者决定。
	lastUpdate := normalizeSQLWhitespace(exec.execQueries[(rounds*2-1)*2])
	require.Contains(t, lastUpdate, "status = $1")
	require.Contains(t, lastUpdate, "error_message = $2")
	require.Contains(t, lastUpdate, "schedulable = FALSE")
	require.Equal(t, service.StatusError, exec.execArgs[(rounds*2-1)*2][0],
		"最后写入者 SetError 的 status 参数须为 StatusError")
}
