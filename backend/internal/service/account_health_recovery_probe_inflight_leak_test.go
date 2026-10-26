// AC-G2R2 must_fix 回归：补探 in-flight 标记泄漏修复。
//
// 背景（gate② 增量复审 output_sha256 3118e4e9c72d759d）：旧 runProbeRetryBurst 的
// defer 清理遍历会被重赋值的 pending（:602-633 每轮把 pending 重赋值为仅含仍需补探
// 账号的 next），补探成功/达上限/回读后不再停调/回读失败的账号退出队列后标记永不清，
// 后续 sweep 在 :264-278 凭 isRetryInFlight 标记永久跳过该账号、永不再对其探测。
//
// 修复：退出清理改为遍历入队时固定的原始账号集合（enqueued 快照），不遍历会被重赋值的
// pending。本回归测试覆盖完整场景：首探失败（置标记）→ 补探成功恢复（退出队列）→
// 账号再次进入可探测停调 → 新 sweep 能再次对其首探（不被残留标记跳过）。
//
// 本文件无构建标签，普通 `go test ./internal/service/` 与 `-tags unit` 均可覆盖。
package service

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestProbeRetryInFlightNotLeakedAfterRecovery 是 AC-G2R2 的核心回归：
// 补探成功恢复（账号退出补探队列）后，其 in-flight 标记必须被清理，否则第二次
// sweep 在 :264-278 凭残留标记跳过该账号、永不再探测。
func TestProbeRetryInFlightNotLeakedAfterRecovery(t *testing.T) {
	const acct int64 = 7
	var calls int32
	// 首探失败，之后补探连续成功两次（第 2、3 次全局调用）→ C1 连续成功阈值达标恢复。
	p, repo, _ := newProbeRetryHarness(t, func(_ context.Context, _ *Account) (bool, error) {
		return atomic.AddInt32(&calls, 1) >= 2, nil
	})

	// 第一轮 sweep：首探失败 → 入队补探 → 补探连续成功达标恢复（账号退出补探队列）。
	p.RunOnce(context.Background())
	p.wg.Wait()

	require.Equal(t, int32(3), atomic.LoadInt32(&calls), "第一轮应 1 次失败首探 + 2 次成功补探")
	require.Equal(t, 1, repo.clearTempIfReasonCalls, "第一轮补探达标必须恢复")

	// must_fix 核心断言：恢复（退出队列）后账号的 in-flight 标记必须被 defer 清理，
	// 不得泄漏。旧实现遍历被重赋值的 pending（此时为空），标记永不清。
	require.False(t, p.isRetryInFlight(acct),
		"恢复账号的 in-flight 标记必须被 defer 清理，不能泄漏；否则后续 sweep 永久跳过该账号")

	// 账号再次进入可探测停调：mock 的 ClearTempUnschedulable 不清除 Account 对象，
	// 显式重置为全新 parked 态，确保第二轮 sweep 看到一个干净候选（与首轮同口径）。
	rePark := parkedBreakerAccount(acct, 0)
	repo.accounts[acct] = rePark
	repo.candidates = []*Account{rePark}

	// 第二轮 sweep 前清零探测计数，使两轮语义一致、断言确定。
	atomic.StoreInt32(&calls, 0)
	p.RunOnce(context.Background())
	p.wg.Wait()

	// 若标记泄漏，第二轮会在 isRetryInFlight 处整账号跳过（0 次探测）；修复后必须
	// 重新首探(1) + 补探(2) = 3 次探测。
	require.Equal(t, int32(3), atomic.LoadInt32(&calls),
		"第二轮 sweep 必须重新首探(1)+补探(2)该账号；残留 in-flight 标记会令其被跳过、0 次探测")
	require.Equal(t, 2, repo.clearTempIfReasonCalls, "第二轮补探达标必须再次恢复")
	// 第二轮结束后标记同样必须被清理（不泄漏）。
	require.False(t, p.isRetryInFlight(acct), "第二轮恢复后标记仍须被清理")
}
