package handler

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// profitVetoLoopResult 记录一次模拟选号循环的终止方式与步数。
type profitVetoLoopResult struct {
	outcome        string // "forwarded" | "exhausted" | "budget_exceeded"
	forwardedID    int64
	iterations     int
	backoffRetries int
}

// runProfitVetoLoop 模拟 handler 的「选号 → 利润终检 → 重选」循环，只保留与
// 活锁相关的状态机（FailoverState + 排除列表 + HandleSelectionExhausted），
// 不涉及真实调度器与上游转发。
//
// pool 按顺序给出候选账号；vetoed 中的账号每次终检都被利润门否决——这对应
// 「候选池快照与 per-account 快照短暂不一致」时门的确定性判定。
// maxIterations 是测试自身的预算：循环超过它即视为活锁。
func runProfitVetoLoop(t *testing.T, fs *FailoverState, pool []int64, vetoed map[int64]bool, maxIterations int) profitVetoLoopResult {
	t.Helper()
	res := profitVetoLoopResult{}
	for res.iterations = 1; res.iterations <= maxIterations; res.iterations++ {
		// 选号：返回第一个不在排除列表中的账号。
		var picked int64
		for _, id := range pool {
			if _, excluded := fs.FailedAccountIDs[id]; !excluded {
				picked = id
				break
			}
		}
		if picked == 0 {
			// 选号耗尽，交给退避决策。
			switch fs.HandleSelectionExhausted(context.Background()) {
			case FailoverContinue:
				res.backoffRetries++
				continue
			default:
				res.outcome = "exhausted"
				return res
			}
		}
		if vetoed[picked] {
			if fs.RecordProfitVeto(picked) == FailoverExhausted {
				res.outcome = "exhausted"
				return res
			}
			continue
		}
		res.outcome = "forwarded"
		res.forwardedID = picked
		return res
	}
	res.outcome = "budget_exceeded"
	return res
}

// TestProfitVetoAfter503DoesNotLivelock 钉死 #4925 引入的活锁回归在无退避语义
// 下同样成立：选号耗尽即按账号耗尽终止（无 503 退避分支可复活被否决账号），
// 整池被利润门否决时必然有限步终止。
func TestProfitVetoAfter503DoesNotLivelock(t *testing.T) {
	fs := NewFailoverState(false)
	// 已经历一次真实 503（Antigravity 单账号分组 MODEL_CAPACITY_EXHAUSTED 是设计内路径）。
	fs.LastFailoverErr = newTestFailoverErr(503, false, false)
	fs.SwitchCount = 1
	fs.FailedAccountIDs[1] = struct{}{}

	start := time.Now()
	res := runProfitVetoLoop(t, fs, []int64{1}, map[int64]bool{1: true}, 50)
	elapsed := time.Since(start)

	require.Equal(t, "exhausted", res.outcome, "整池被利润门否决时必须有限步终止")
	require.Zero(t, res.backoffRetries, "无退避语义下不得发生任何退避重试（旧 2s 空转循环已删除）")
	require.Less(t, elapsed, 10*time.Second, "不得每 2s 空转一轮")
}

// TestProfitVetoKeepsBackoffUsefulForHealthyAccount 钉死已批准的无退避语义下
// 利润否决不回池：选号耗尽（所有候选均在排除列表）即按账号耗尽终止，不再发生
// 503 退避清空排除列表并重试（旧行为会复活被利润门否决的账号）。
func TestProfitVetoKeepsBackoffUsefulForHealthyAccount(t *testing.T) {
	fs := NewFailoverState(false)
	fs.LastFailoverErr = newTestFailoverErr(503, false, false)
	fs.SwitchCount = 1
	// 账号 1 因真实 503 被排除；账号 2 会被利润门否决。
	fs.FailedAccountIDs[1] = struct{}{}

	res := runProfitVetoLoop(t, fs, []int64{2, 1}, map[int64]bool{2: true}, 50)

	require.Equal(t, "exhausted", res.outcome, "无退避语义下选号耗尽即终止，复活候选的 503 退避分支已不存在")
	require.Equal(t, 0, res.backoffRetries, "不得发生任何退避重试")
	require.Contains(t, fs.FailedAccountIDs, int64(2), "利润否决的账号在无退避语义下必须保持在排除集")
	require.Contains(t, fs.FailedAccountIDs, int64(1), "503 失败账号也必须保持在排除集，不因选号耗尽被清空")
}

// TestProfitVetoAttemptsCapped 钉死没有 503 参与时，大分组整池越线也会在
// 常数步内终止，而不是把整池逐个选一遍。
func TestProfitVetoAttemptsCapped(t *testing.T) {
	fs := NewFailoverState(false)
	pool := make([]int64, 0, 64)
	vetoed := make(map[int64]bool, 64)
	for id := int64(1); id <= 64; id++ {
		pool = append(pool, id)
		vetoed[id] = true
	}

	res := runProfitVetoLoop(t, fs, pool, vetoed, 200)

	require.Equal(t, "exhausted", res.outcome)
	require.Equal(t, maxProfitVetoAttempts, fs.ProfitVetoCount())
	require.Equal(t, maxProfitVetoAttempts, res.iterations, "达到上限即终止，不应继续遍历候选池")
}

// TestRecordProfitVetoExcludesAccount 钉死 RecordProfitVeto 仍然把账号加入
// 调度排除列表（选号入参用的就是 FailedAccountIDs）。
func TestRecordProfitVetoExcludesAccount(t *testing.T) {
	fs := NewFailoverState(false)
	require.Equal(t, FailoverContinue, fs.RecordProfitVeto(42))
	require.Contains(t, fs.FailedAccountIDs, int64(42))
	require.Equal(t, 1, fs.ProfitVetoCount())
}

// TestHandleSelectionExhaustedKeepsFailedAccounts 钉死已批准的无退避语义：
// HandleSelectionExhausted 无条件返回 FailoverExhausted，排除列表保持不变，
// 利润否决账号不会重新回池（不再有 503 退避清空排除列表的旧行为）。
func TestHandleSelectionExhaustedKeepsFailedAccounts(t *testing.T) {
	fs := NewFailoverState(false)
	fs.LastFailoverErr = newTestFailoverErr(503, false, false)
	fs.SwitchCount = 1
	fs.FailedAccountIDs[100] = struct{}{}
	fs.RecordProfitVeto(200)

	require.Equal(t, FailoverExhausted, fs.HandleSelectionExhausted(context.Background()))
	require.Contains(t, fs.FailedAccountIDs, int64(100), "非利润否决的失败账号必须保持在排除列表")
	require.Contains(t, fs.FailedAccountIDs, int64(200), "利润否决账号必须保持在排除列表（无退避不回池）")
}
