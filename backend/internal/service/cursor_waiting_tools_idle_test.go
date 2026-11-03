//go:build unit

package service

// 本文件覆盖**交付工具批后客户端不续轮的提前释放**（方案 §2.5，用户 2026-11-01 裁定 B）。
//
// 为什么需要这一层：Cursor 会话的用户槽/账号槽与余额预留跟着会话走（§25.4），只在真实
// 终态、客户端取消或服务 Stop 时归还。客户端在最后一批 tool_calls 后按 esc 中断、直接
// 退出或根本没打算续轮时，会话停在 waiting_tools 无人收尾，槽会一直挂到 Redis 侧
// concurrency_slot_ttl（默认 30 分钟）被动清理；期间该账号对新请求持续表现为「并发满」，
// 粘性会话又不换号，整条链路每 120 秒撞一次 429（2026-11-04 生产实证）。
//
// 本测试固定四条契约：
//  1. waiting_tools 且本批结果未被消费 + 计时到期 → 走同一 finalizer，槽只释放一次；
//  2. 本批结果已被消费（客户端已续轮）→ 到期也不得终结；
//  3. 会话已回到 running → 到期也不得终结；
//  4. 计时器在交付时启动、续轮/finalizer 时停止，同一时刻只有一个。

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// startWaitingToolsSession 建一条处于 waiting_tools、持有两个槽的会话。
func startWaitingToolsSession(t *testing.T, svc *CursorExecutionService) (*cursorSessionRecord, *cursorTestResource, *cursorTestResource) {
	t.Helper()
	record := startCursorLifecycleTestSession(t, svc)

	userSlot := newCursorTestResource("user_slot")
	accountSlot := newCursorTestResource("account_slot")
	record.mu.Lock()
	record.resources = []CursorTransferableResource{userSlot, accountSlot}
	record.state = cursorSessionWaitingTools
	record.pending = &cursorPendingBatch{batchID: "batch-1", delivered: true}
	record.mu.Unlock()
	return record, userSlot, accountSlot
}

// TestWaitingToolsIdleReleasesSlots 覆盖契约 1：
// 到期且本批未消费 → 走唯一 finalizer，两个槽各释放一次，会话出表，原因可观测。
func TestWaitingToolsIdleReleasesSlots(t *testing.T) {
	svc := newCursorLifecycleTestService()
	record, userSlot, accountSlot := startWaitingToolsSession(t, svc)

	svc.onWaitingToolsIdle(record)

	require.EqualValues(t, 1, userSlot.releases.Load(), "空闲释放必须归还用户槽")
	require.EqualValues(t, 1, accountSlot.releases.Load(), "空闲释放必须归还账号槽")
	require.Equal(t, 0, svc.sessions.sessionCount(), "释放后会话必须出表")
	require.Equal(t, 0, svc.sessions.indexCount())

	record.mu.Lock()
	state := record.state
	reason := record.interruptReason
	outcome := record.outcome
	record.mu.Unlock()
	require.Equal(t, cursorSessionClosed, state)
	require.Equal(t, "waiting_tools_idle", reason, "终态原因必须是 waiting_tools_idle（可观测）")
	require.Equal(t, "waiting_tools_idle", outcome)

	// 幂等：重复到期不得重复释放。
	svc.onWaitingToolsIdle(record)
	require.EqualValues(t, 1, userSlot.releases.Load())
	require.EqualValues(t, 1, accountSlot.releases.Load())
}

// TestWaitingToolsIdleSkipsConsumedBatch 覆盖契约 2：
// 本批结果已被消费（客户端已续轮）时，即使计时器到点也不得终结。
func TestWaitingToolsIdleSkipsConsumedBatch(t *testing.T) {
	svc := newCursorLifecycleTestService()
	record, userSlot, accountSlot := startWaitingToolsSession(t, svc)

	record.mu.Lock()
	record.pending.consumed = true
	record.state = cursorSessionRunning
	record.mu.Unlock()

	svc.onWaitingToolsIdle(record)

	require.EqualValues(t, 0, userSlot.releases.Load(), "已续轮的会话不得被空闲释放误杀")
	require.EqualValues(t, 0, accountSlot.releases.Load())
	require.Equal(t, 1, svc.sessions.sessionCount())
}

// TestWaitingToolsIdleSkipsRunningSession 覆盖契约 3：
// 会话已回到 running（正在输出下一轮）时，到期回调不得终结它。
func TestWaitingToolsIdleSkipsRunningSession(t *testing.T) {
	svc := newCursorLifecycleTestService()
	record, userSlot, _ := startWaitingToolsSession(t, svc)

	record.mu.Lock()
	record.state = cursorSessionRunning
	record.mu.Unlock()

	svc.onWaitingToolsIdle(record)

	require.EqualValues(t, 0, userSlot.releases.Load())
	require.Equal(t, 1, svc.sessions.sessionCount())
}

// TestWaitingToolsIdleTimerArmAndStop 覆盖契约 4：
// 进入 waiting_tools 起表、续轮或 finalizer 停表，且同一时刻只有一个计时器。
func TestWaitingToolsIdleTimerArmAndStop(t *testing.T) {
	svc := newCursorLifecycleTestService()
	record, _, _ := startWaitingToolsSession(t, svc)

	record.mu.Lock()
	svc.armWaitingToolsIdleTimer(record)
	first := record.waitingIdleTimer
	require.NotNil(t, first, "进入 waiting_tools 必须启动空闲释放计时器")

	// 重复进入：旧计时器被替换，不得并存。
	svc.armWaitingToolsIdleTimer(record)
	second := record.waitingIdleTimer
	require.NotNil(t, second)
	require.NotSame(t, first, second, "重复进入 waiting_tools 必须替换而不是叠加计时器")
	stopWaitingToolsIdleTimerLocked(record)
	require.Nil(t, record.waitingIdleTimer, "续轮/finalizer 必须停表")
	record.mu.Unlock()

	// finalizer 入口同样要停表。
	record.mu.Lock()
	svc.armWaitingToolsIdleTimer(record)
	record.mu.Unlock()
	svc.finishCursorSession(record, "completed")

	record.mu.Lock()
	require.Nil(t, record.waitingIdleTimer, "finalizer 必须停表，避免重复进入")
	record.mu.Unlock()
}

// TestWaitingToolsIdleNoResourcesStillFinalizes 覆盖无资源边界：
// 会话没持有槽（已提前释放）时到期仍应正常收尾，不 panic、不重复结算。
func TestWaitingToolsIdleNoResourcesStillFinalizes(t *testing.T) {
	svc := newCursorLifecycleTestService()
	record := startCursorLifecycleTestSession(t, svc)
	record.mu.Lock()
	record.state = cursorSessionWaitingTools
	record.pending = &cursorPendingBatch{batchID: "batch-empty", delivered: true}
	record.mu.Unlock()

	require.NotPanics(t, func() { svc.onWaitingToolsIdle(record) })
	require.Equal(t, 0, svc.sessions.sessionCount())

	record.mu.Lock()
	state := record.state
	record.mu.Unlock()
	require.Equal(t, cursorSessionClosed, state)
}
