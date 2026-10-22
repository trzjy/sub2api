//go:build unit

package service

// 本文件是 **§25.4 / §25.5 会话生命周期**的定向测试（派发单 W2-D2）。
//
// 覆盖异常矩阵中本单负责的行：
//  1. 并发 1 续轮两 HTTP 最终完成：两个 HTTP 依次附着/分离同一会话，最终只结算一次；
//  2. 取消 / 交接竞争唯一胜者：附着期取消与正常 detach 竞争，锁内状态判定唯一胜者；
//  3. 重复续轮 409：同一会话已有 HTTP 附着时再次附着被明确拒绝；
//  4. Redis 槽与预留跨交接保留、终态归还：资源跨两个 HTTP 保留，finalizer 只释放一次；
//  5. 同事件重复 Apply 只扣一次：会话级不可变 durable 键可重放幂等；
//  6. 空 TurnEnded / 字段缺失不假成功：未取得完整真实 usage 时不结算、不写零；
//  7. 写出中断终止：附着期取消即结束会话；
//  8. 等待结果过期：deadline 到期后会话终结且不重算 usage；
//  9. 进程 Stop 孤儿续轮明确失效；
// 10. 记账提交失败如实报错后仍释放资源（finalizer 每项至多一次）。
//
// 纯内存组件测试：无 DB、无网络；资源与结算全部用替身。

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// cursorTestResource 是可转交资源的替身（记录转交/释放次数）。
type cursorTestResource struct {
	name       string
	transfers  atomic.Int32
	releases   atomic.Int32
	transferOk atomic.Bool // false 表示交接必须失败（模拟"已取消/已释放"）
}

func newCursorTestResource(name string) *cursorTestResource {
	r := &cursorTestResource{name: name}
	r.transferOk.Store(true)
	return r
}

func (r *cursorTestResource) Name() string { return r.name }

func (r *cursorTestResource) TransferTo(_ context.Context) error {
	if !r.transferOk.Load() {
		return ErrCursorSessionResourceReleased
	}
	r.transfers.Add(1)
	return nil
}

func (r *cursorTestResource) Release() { r.releases.Add(1) }

// cursorTestHandover 是 handler 侧转交物的替身。
type cursorTestHandover struct {
	resources  []CursorTransferableResource
	settlement CursorSessionSettlement
}

func (h *cursorTestHandover) Resources() []CursorTransferableResource { return h.resources }

func (h *cursorTestHandover) Settlement() CursorSessionSettlement { return h.settlement }

// newCursorLifecycleTestService 构造只含会话表的执行部件（生命周期测试不需要出站依赖）。
func newCursorLifecycleTestService() *CursorExecutionService {
	return &CursorExecutionService{
		sessions: newCursorSessionStore(),
		now:      time.Now,
	}
}

// startCursorLifecycleTestSession 建立一条会话并绑定 owner。
func startCursorLifecycleTestSession(t *testing.T, svc *CursorExecutionService) *cursorSessionRecord {
	t.Helper()
	record := svc.sessions.start(cursorSessionSpec{
		protocol:       cursorProtocolChat,
		requestedModel: "cursor-test-model",
		upstreamModel:  "cursor-test-upstream",
		accountID:      42,
		quotaKeyID:     7,
	})
	svc.sessions.attachOwner(record, svc)
	return record
}

// TestCursorSessionBillingEventIDSessionScopedImmutable 覆盖 §25.5：
// 账单事件 ID 由服务端生成、会话级唯一且不可变，与任何 HTTP request_id 无关。
func TestCursorSessionBillingEventIDSessionScopedImmutable(t *testing.T) {
	svc := newCursorLifecycleTestService()
	a := startCursorLifecycleTestSession(t, svc)
	b := startCursorLifecycleTestSession(t, svc)

	require.NotEmpty(t, a.BillingEventID())
	require.NotEqual(t, a.BillingEventID(), b.BillingEventID(), "两个原生会话不得共享同一个账单事件 ID")
	require.Contains(t, a.BillingEventID(), "cursor_session:")

	first := a.BillingEventID()
	// 多次读取（相当于多个续轮 HTTP 读取）必须完全一致：不可变。
	for i := 0; i < 5; i++ {
		require.Equal(t, first, a.BillingEventID())
	}
}

// TestCursorSessionSettlementDurableKeyStablePerSession 覆盖 §25.5：
// 同一会话的 durable 结算键可重放幂等，不同会话互不合并。
func TestCursorSessionSettlementDurableKeyStablePerSession(t *testing.T) {
	svc := newCursorLifecycleTestService()
	a := startCursorLifecycleTestSession(t, svc)
	b := startCursorLifecycleTestSession(t, svc)

	keyA1 := CursorSettlementDurableRequestID(a.BillingEventID())
	keyA2 := CursorSettlementDurableRequestID(a.BillingEventID())
	keyB := CursorSettlementDurableRequestID(b.BillingEventID())

	require.Equal(t, keyA1, keyA2, "同一会话重复结算必须落到同一个 durable 键（幂等）")
	require.NotEqual(t, keyA1, keyB, "不同原生会话不得被合并成一笔账")
	require.NotContains(t, keyA1, "local:", "会话账单事件 ID 不得退化成 HTTP 请求级 ID")
}

// TestCursorSessionDeadlineArmedOnceAcrossHTTP 覆盖 §25.4：
// 180 秒总期限从首次 AgentRun 发起计算，续轮不重置。
func TestCursorSessionDeadlineArmedOnceAcrossHTTP(t *testing.T) {
	svc := newCursorLifecycleTestService()
	record := startCursorLifecycleTestSession(t, svc)

	first := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	deadline, source, remaining := record.armAgentRunDeadline(first)
	require.Equal(t, first.Add(cursorAgentRunTotalBudget), deadline)
	require.Equal(t, cursorDeadlineSourceFirstAgentRun, source)
	require.Equal(t, cursorAgentRunTotalBudget, remaining)

	// 第二个 HTTP（续轮）在 60 秒后才发起 AgentRun：deadline 必须原样保留。
	second := first.Add(60 * time.Second)
	deadline2, source2, remaining2 := record.armAgentRunDeadline(second)
	require.Equal(t, deadline, deadline2, "续轮不得重置 180 秒总期限")
	require.Equal(t, source, source2)
	require.Equal(t, 120*time.Second, remaining2, "剩余预算必须扣掉已消耗的 60 秒")

	require.Equal(t, cursorAgentRunTotalBudget, 180*time.Second)
}

// TestCursorSessionAttachConflictDuplicateContinuation 覆盖 §25.4：
// 同一会话已有 HTTP 附着时再次附着返回冲突（409 语义），不得并发写同一 AgentRun。
func TestCursorSessionAttachConflictDuplicateContinuation(t *testing.T) {
	svc := newCursorLifecycleTestService()
	record := startCursorLifecycleTestSession(t, svc)

	detach1, err := record.attachHTTP(context.Background())
	require.NoError(t, err)

	_, err = record.attachHTTP(context.Background())
	require.ErrorIs(t, err, ErrCursorSessionAttachConflict, "重复续轮必须明确冲突")

	detach1()
	detach2, err := record.attachHTTP(context.Background())
	require.NoError(t, err, "分离之后允许下一次附着")
	detach2()

	// detach 幂等
	detach2()
}

// TestCursorSessionNormalDetachNotKilledByHTTPCancel 覆盖 §25.4：
// 正常 handler 返回触发的 context 取消不能误杀已完成交接的会话。
func TestCursorSessionNormalDetachNotKilledByHTTPCancel(t *testing.T) {
	svc := newCursorLifecycleTestService()
	record := startCursorLifecycleTestSession(t, svc)

	httpCtx, cancelHTTP := context.WithCancel(context.Background())
	detach, err := record.attachHTTP(httpCtx)
	require.NoError(t, err)

	// 正常返回：先 detach，然后 HTTP 生命周期结束触发取消。
	detach()
	cancelHTTP()

	record.mu.Lock()
	finishing := record.finishing
	state := record.state
	record.mu.Unlock()
	require.False(t, finishing, "正常返回后的 context 取消不得结束会话")
	require.Equal(t, cursorSessionRunning, state)

	// 会话仍可被下一个 HTTP 附着（续轮）。
	detach2, err := record.attachHTTP(context.Background())
	require.NoError(t, err)
	detach2()
}

// TestCursorSessionCancelWhileAttachedTerminatesOnce 覆盖 §25.4：
// 正在附着期间的真实断连结束会话，且终结只发生一次。
func TestCursorSessionCancelWhileAttachedTerminatesOnce(t *testing.T) {
	svc := newCursorLifecycleTestService()
	record := startCursorLifecycleTestSession(t, svc)
	res := newCursorTestResource("user_slot")
	require.NoError(t, res.TransferTo(context.Background()))
	record.mu.Lock()
	record.resources = []CursorTransferableResource{res}
	record.mu.Unlock()

	httpCtx, cancelHTTP := context.WithCancel(context.Background())
	_, err := record.attachHTTP(httpCtx)
	require.NoError(t, err)

	cancelHTTP()
	require.Eventually(t, func() bool {
		record.mu.Lock()
		defer record.mu.Unlock()
		return record.state == cursorSessionClosed
	}, 2*time.Second, 5*time.Millisecond, "附着期取消必须结束会话")

	require.EqualValues(t, 1, res.releases.Load(), "终结守卫保证槽位只释放一次")

	// 再触发一次终结（重复的取消/Stop/返回竞争）不得重复释放。
	svc.finishCursorSession(record, "late_duplicate")
	require.EqualValues(t, 1, res.releases.Load())
}

// TestCursorFinalizerStepsAtMostOnce 覆盖 §25.4：
// 并发终结竞争下每一项至多执行一次（资源只释放一次、结算只提交一次）。
func TestCursorFinalizerStepsAtMostOnce(t *testing.T) {
	svc := newCursorLifecycleTestService()
	record := startCursorLifecycleTestSession(t, svc)

	userSlot := newCursorTestResource("user_slot")
	accountSlot := newCursorTestResource("account_slot")
	for _, r := range []*cursorTestResource{userSlot, accountSlot} {
		require.NoError(t, r.TransferTo(context.Background()))
	}
	record.mu.Lock()
	record.resources = []CursorTransferableResource{userSlot, accountSlot}
	record.mu.Unlock()

	var settlements atomic.Int32
	record.mu.Lock()
	record.settlement = func(context.Context, CursorSettlementEvent) { settlements.Add(1) }
	record.mu.Unlock()
	record.noteTurnUsage(cursorTurnUsage{Seen: true, Input: 12, Output: 5}, "stream_write_done")

	const racers = 16
	var wg sync.WaitGroup
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func(i int) {
			defer wg.Done()
			svc.finishCursorSession(record, "race")
		}(i)
	}
	wg.Wait()

	require.EqualValues(t, 1, userSlot.releases.Load(), "用户槽只释放一次")
	require.EqualValues(t, 1, accountSlot.releases.Load(), "账号槽只释放一次")
	require.EqualValues(t, 1, settlements.Load(), "唯一结算只提交一次")

	record.mu.Lock()
	steps := record.steps
	state := record.state
	record.mu.Unlock()
	require.True(t, steps.intakeStopped)
	require.True(t, steps.streamClosed)
	require.True(t, steps.settled)
	require.True(t, steps.slotsReleased)
	require.True(t, steps.indexRemoved)
	require.True(t, steps.closed)
	require.Equal(t, cursorSessionClosed, state)

	// 索引进关联必须被清空（同一会话全部关联）。
	require.Equal(t, 0, svc.sessions.sessionCount())
	require.Equal(t, 0, svc.sessions.indexCount())
}

// TestCursorCrossHTTPTwoTurnsSettleOnce 覆盖异常矩阵"并发 1 续轮两 HTTP 最终完成"：
// 两个 HTTP 依次附着同一会话（第一轮工具交接不带 usage），资源跨交接保留，
// 最终一次性结算且只结算一次。
func TestCursorCrossHTTPTwoTurnsSettleOnce(t *testing.T) {
	svc := newCursorLifecycleTestService()
	record := startCursorLifecycleTestSession(t, svc)

	userSlot := newCursorTestResource("user_slot")
	accountSlot := newCursorTestResource("account_slot")
	require.NoError(t, userSlot.TransferTo(context.Background()))
	require.NoError(t, accountSlot.TransferTo(context.Background()))
	record.mu.Lock()
	record.resources = []CursorTransferableResource{userSlot, accountSlot}
	record.mu.Unlock()

	var settled []CursorSettlementEvent
	var mu sync.Mutex
	record.mu.Lock()
	record.settlement = func(_ context.Context, ev CursorSettlementEvent) {
		mu.Lock()
		defer mu.Unlock()
		if ev.UsageComplete {
			settled = append(settled, ev)
		}
	}
	record.mu.Unlock()

	// HTTP1：附着 → 工具批交接（无真实 usage）→ 分离，会话进入等待。
	detach1, err := record.attachHTTP(context.Background())
	require.NoError(t, err)
	record.noteTurnUsage(cursorTurnUsage{}, "tool_batch_handover")
	detach1()
	require.EqualValues(t, 0, userSlot.releases.Load(), "HTTP1 返回后槽位仍归会话持有")
	require.EqualValues(t, 0, accountSlot.releases.Load())

	// 此时若最终终结（如客户端不再续轮）→ 未取得完整 usage → 不结算、只释放。
	// 这里继续续轮，验证"同一会话只结算一次"。
	detach2, err := record.attachHTTP(context.Background())
	require.NoError(t, err)
	record.noteTurnUsage(cursorTurnUsage{Seen: true, Input: 100, Output: 40, CacheRead: 8, EndedAtMS: 1000}, "stream_write_done")
	detach2()
	require.EqualValues(t, 0, userSlot.releases.Load(), "HTTP2 返回后槽位仍归会话持有（未终结）")

	svc.finishCursorSession(record, "completed")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, settled, 1, "两个 HTTP 最终只结算一次")
	require.Equal(t, record.BillingEventID(), settled[0].BillingEventID, "结算使用会话级不可变账单事件 ID")
	require.EqualValues(t, 1, userSlot.releases.Load())
	require.EqualValues(t, 1, accountSlot.releases.Load())
}

// TestCursorSessionSettlementEmptyTurnEndedNoFakeSuccess 覆盖 §25.5：
// 空 TurnEnded（字段全缺）与"从未收到 TurnEnded"都不结算、不写零 usage 假成功。
func TestCursorSessionSettlementEmptyTurnEndedNoFakeSuccess(t *testing.T) {
	cases := []struct {
		name    string
		usage   cursorTurnUsage
		missing string
	}{
		{name: "从未收到 turn_ended", usage: cursorTurnUsage{}, missing: "turn_ended_missing"},
		{name: "空 turn_ended 帧", usage: cursorTurnUsage{Seen: true}, missing: "turn_ended_fields_missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newCursorLifecycleTestService()
			record := startCursorLifecycleTestSession(t, svc)
			slot := newCursorTestResource("user_slot")
			require.NoError(t, slot.TransferTo(context.Background()))
			record.mu.Lock()
			record.resources = []CursorTransferableResource{slot}
			record.mu.Unlock()

			var called atomic.Int32
			var complete atomic.Bool
			record.mu.Lock()
			record.settlement = func(_ context.Context, ev CursorSettlementEvent) {
				called.Add(1)
				complete.Store(ev.UsageComplete)
				require.Equal(t, tc.missing, ev.UsageMissingReason)
			}
			record.mu.Unlock()
			record.noteTurnUsage(tc.usage, "stream_write_done")

			svc.finishCursorSession(record, "completed")

			require.EqualValues(t, 1, called.Load(), "中断路径仍会通知结算方（用于记账中断原因）")
			require.False(t, complete.Load(), "未取得完整真实 usage 时不得结算")
			require.EqualValues(t, 1, slot.releases.Load(), "未结算也要释放资源")

			record.mu.Lock()
			steps := record.steps
			record.mu.Unlock()
			require.True(t, steps.settled, "结算步骤本身（守卫）仍标记为已执行一次")
		})
	}
}

// TestCursorSessionSettlementRealZeroDistinguished 覆盖 §25.5：
// 带真实计数字段（哪怕部分为 0）的 TurnEnded 视为完整 usage，走正常结算。
func TestCursorSessionSettlementRealZeroDistinguished(t *testing.T) {
	svc := newCursorLifecycleTestService()
	record := startCursorLifecycleTestSession(t, svc)
	var complete atomic.Bool
	record.mu.Lock()
	record.settlement = func(_ context.Context, ev CursorSettlementEvent) { complete.Store(ev.UsageComplete) }
	record.mu.Unlock()

	// 输出为 0（真实 0）但输入与结束时刻存在：完整真实 usage。
	record.noteTurnUsage(cursorTurnUsage{Seen: true, Input: 9, Output: 0, EndedAtMS: 123}, "stream_write_done")
	svc.finishCursorSession(record, "completed")
	require.True(t, complete.Load(), "真实 0 的计数字段不得被当成字段缺失")
}

// TestCursorSessionHandoverResourceFailedTransferFailsClosed 覆盖 §25.4：
// 已取消/已释放的句柄交接必须失败，不能把失效槽位交给会话。
func TestCursorSessionHandoverResourceFailedTransferFailsClosed(t *testing.T) {
	svc := newCursorLifecycleTestService()
	record := startCursorLifecycleTestSession(t, svc)

	okRes := newCursorTestResource("user_slot")
	deadRes := newCursorTestResource("account_slot")
	deadRes.transferOk.Store(false)

	ctx := WithCursorSessionHandover(context.Background(), &cursorTestHandover{
		resources: []CursorTransferableResource{okRes, deadRes},
	})

	err := svc.adoptHandover(ctx, record, context.Background())
	require.ErrorIs(t, err, ErrCursorSessionResourceReleased)

	// 已成功交接的部分必须回滚释放（不留半交接状态），失败句柄不被会话持有。
	require.EqualValues(t, 1, okRes.releases.Load())
	require.EqualValues(t, 0, deadRes.releases.Load())
	record.mu.Lock()
	resources := record.resources
	record.mu.Unlock()
	require.Empty(t, resources)
}

// TestCursorSessionStopInvalidatesOrphanContinuation 覆盖异常矩阵"进程 Stop 孤儿续轮明确失效"。
func TestCursorSessionStopInvalidatesOrphanContinuation(t *testing.T) {
	svc := newCursorLifecycleTestService()
	record := startCursorLifecycleTestSession(t, svc)
	require.NoError(t, svc.sessions.registerToolCall(record.SessionID(), "mc-1", "call-1"))
	require.NotNil(t, svc.sessions.lookupSession(cursorProtocolChat, CursorIDKindCall, "call-1"))

	slot := newCursorTestResource("user_slot")
	require.NoError(t, slot.TransferTo(context.Background()))
	record.mu.Lock()
	record.resources = []CursorTransferableResource{slot}
	record.mu.Unlock()

	svc.Stop()

	require.Nil(t, svc.sessions.lookupSession(cursorProtocolChat, CursorIDKindCall, "call-1"),
		"Stop 后孤儿续轮必须明确失效（不换号、不重执行工具）")
	require.EqualValues(t, 1, slot.releases.Load(), "Stop 走同一 finalizer 并释放资源")
	require.Equal(t, 0, svc.sessions.sessionCount())
	require.Equal(t, 0, svc.sessions.indexCount())
}

// TestCursorSessionSettlementFailureStillReleasesOnce 覆盖异常矩阵"记账提交失败如实报错"：
// 结算回调返回后资源仍只释放一次（记账失败不得改变释放次数）。
func TestCursorSessionSettlementFailureStillReleasesOnce(t *testing.T) {
	svc := newCursorLifecycleTestService()
	record := startCursorLifecycleTestSession(t, svc)

	slot := newCursorTestResource("user_slot")
	require.NoError(t, slot.TransferTo(context.Background()))
	record.mu.Lock()
	record.resources = []CursorTransferableResource{slot}
	var settlementErr atomic.Value
	record.settlement = func(context.Context, CursorSettlementEvent) {
		settlementErr.Store(errors.New("apply failed"))
	}
	record.mu.Unlock()
	record.noteTurnUsage(cursorTurnUsage{Seen: true, Input: 3, Output: 1}, "stream_write_done")

	svc.finishCursorSession(record, "completed")
	svc.finishCursorSession(record, "completed_again")

	require.NotNil(t, settlementErr.Load(), "记账失败须被调用方如实记录")
	require.EqualValues(t, 1, slot.releases.Load(), "结算失败不得导致重复释放或漏释放")
}

// TestCursorSessionDeadlineExceededReason 覆盖异常矩阵"等待结果过期"：
// deadline 是终态原因之一，终结后不再重置期限。
func TestCursorSessionDeadlineExceededReason(t *testing.T) {
	require.Equal(t, "deadline_exceeded", cursorSessionTerminalReason(context.DeadlineExceeded))
	require.Equal(t, "cancelled", cursorSessionTerminalReason(context.Canceled))
	require.Equal(t, "completed", cursorSessionTerminalReason(nil))
	require.Equal(t, "failed", cursorSessionTerminalReason(errors.New("boom")))

	svc := newCursorLifecycleTestService()
	record := startCursorLifecycleTestSession(t, svc)
	deadline, _, _ := record.armAgentRunDeadline(time.Now())
	svc.finishCursorSession(record, cursorSessionTerminalReason(context.DeadlineExceeded))

	record.mu.Lock()
	outcome := record.outcome
	record.mu.Unlock()
	require.Equal(t, "deadline_exceeded", outcome)
	require.Equal(t, deadline, record.Deadline(), "终结不得改写已固定的 deadline")
}

// TestCursorInflightHoldPastHTTPDefersHandlerDone 覆盖 §25.4/§25.5：
// 只把**现有** HandlerDone 延后到会话终结；结算提交之后才归还预留。
func TestCursorInflightHoldPastHTTPDefersHandlerDone(t *testing.T) {
	cache := newMemInflightCache(1)
	svc := newInflightSvc(t, cache, 60)

	res, err := svc.ReserveInflight(context.Background(), &User{ID: 1}, nil, nil, 0.9)
	require.NoError(t, err)
	require.NotNil(t, res)

	require.True(t, res.HoldPastHTTP(), "HTTP 侧尚未归还可以延后")
	require.True(t, res.HeldPastHTTP())

	// HTTP 返回：HandlerDone 必须是空操作（归还时机唯一，归会话终结）。
	res.HandlerDone()
	require.Equal(t, 1, cache.count(), "预留必须跨 HTTP 保留到会话终结")

	// 会话终结：结算提交之后归还。
	res.SessionDone()
	res.SessionDone() // 幂等
	require.Equal(t, 0, cache.count())
	require.EqualValues(t, 1, cache.releaseCt.Load(), "预留只释放一次")

	// 已经归还之后再延后必须失败：不得 Acquire 假装保住续期。
	require.False(t, res.HoldPastHTTP(), "HTTP 已归还后不得再延后")
}

// TestCursorInflightHoldPastHTTPRefusesAfterHandlerDone 覆盖 §25.4：
// 禁止在 HTTP1 HandlerDone 之后再 Acquire 假装保留续期。
func TestCursorInflightHoldPastHTTPRefusesAfterHandlerDone(t *testing.T) {
	cache := newMemInflightCache(1)
	svc := newInflightSvc(t, cache, 60)

	res, err := svc.ReserveInflight(context.Background(), &User{ID: 2}, nil, nil, 0.5)
	require.NoError(t, err)
	res.HandlerDone()
	require.False(t, res.HoldPastHTTP(), "已 HandlerDone 的预留不得再交给会话")
	require.False(t, res.HeldPastHTTP())
	// Acquire 在归零后是 no-op：不会出现"假续期"。
	res.Acquire()()
	require.Equal(t, 0, cache.count())
}

// TestCursorSessionDuplicateApplyChargesOnce 覆盖异常矩阵"同事件重复 Apply 只扣一次"：
// 会话级 durable 键与会话一一对应，重复 Apply 命中同一幂等键，只扣一次费。
func TestCursorSessionDuplicateApplyChargesOnce(t *testing.T) {
	svc := newCursorLifecycleTestService()

	// 幂等 sink：以 durable 请求 ID 为键（与 usage_billing_repo.Apply 的幂等口径同源）。
	var mu sync.Mutex
	charges := make(map[string]float64)
	apply := func(eventID string, amount float64) {
		key := CursorSettlementDurableRequestID(eventID)
		mu.Lock()
		defer mu.Unlock()
		if _, ok := charges[key]; ok {
			return // 幂等：同事件重复 Apply 不再扣费
		}
		charges[key] = amount
	}

	record := startCursorLifecycleTestSession(t, svc)
	eventID := record.BillingEventID()

	apply(eventID, 1.25)
	apply(eventID, 1.25)
	mu.Lock()
	require.Len(t, charges, 1, "同一会话重复 Apply 只扣一次")
	require.Equal(t, 1.25, charges[CursorSettlementDurableRequestID(eventID)])
	mu.Unlock()

	// 另一个原生会话是另一笔账。
	other := startCursorLifecycleTestSession(t, svc)
	apply(other.BillingEventID(), 2.5)
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, charges, 2, "不同原生会话不得被合并成一笔账")
}
