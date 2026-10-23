//go:build unit

package service

// 本文件是 **§25.17.7 A09（停止必须覆盖会话与计费依赖）会话层**的定向测试。
//
// 覆盖 §25.17.7「必须证明」中属于会话/执行部件的每一项：
//  1. waiting_tools + 已拿完整真实 usage 但结算未完成的会话，Stop 后索引/会话清零；
//  2. 原槽与预留只释放一次（重复 Stop 不重复释放）；
//  3. 真实 usage 沿既有结算回调落账一次；缺 usage 不写零账（只带中断原因）；
//  4. Stop 必须等唯一 finalizer **完成**（结算提交/资源归还/索引清理之后），
//     不能看到 finishing=true 就当完成——用阻塞结算任务作屏障证明；
//  5. cleanup 与结果回传竞争只有一个胜者（结算/释放各一次）；
//  6. Stop 之后 sessions.start 明确不可用、零 AgentRun；
//  7. ProbeWorker 在原唯一构造处 Start 一次、Stop 一次（幂等，不重复停）。
//
// 纯内存：无 DB、无网络。结算/资源/预留全部用替身，用确定性的 channel 屏障控制顺序，
// 绝不靠 sleep 猜竞态。

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// cursorStopSettlementRecord 是一次结算回调的可断言记录（按调用顺序追加）。
type cursorStopSettlementRecord struct {
	complete bool
	missing  string
	reason   string
}

// cursorStopFixture 汇齐 A09 会话层证明所需的一套装配。
type cursorStopFixture struct {
	svc    *CursorExecutionService
	record *cursorSessionRecord

	settlements []cursorStopSettlementRecord
	// settleMu 保护 settlements 追加（结算回调可能被 finalizer 与测试并发读）。
	settleMu  sync.Mutex
	resources []*cursorTestResource

	// settleEntered 在结算回调**进入**时关闭（Stop 不得越过它）。
	settleEntered chan struct{}
	// settleRelease 允许结算回调返回；未关闭前结算视为"仍在进行"。
	settleRelease chan struct{}
	// releaseOnce 保证只关闭一次。
	releaseOnce sync.Once
}

// newCursorStopFixture 建立一条 waiting_tools 会话：
// 槽已转交、预留已挂接、usage 按 complete 决定是否完整。
func newCursorStopFixture(t *testing.T, completeUsage bool) *cursorStopFixture {
	t.Helper()
	f := &cursorStopFixture{
		svc:           newCursorLifecycleTestService(),
		settleEntered: make(chan struct{}),
		settleRelease: make(chan struct{}),
	}

	record := startCursorLifecycleTestSession(t, f.svc)
	f.record = record

	userSlot := newCursorTestResource("user_slot")
	accountSlot := newCursorTestResource("account_slot")
	f.resources = []*cursorTestResource{userSlot, accountSlot}
	require.NoError(t, userSlot.TransferTo(context.Background()))
	require.NoError(t, accountSlot.TransferTo(context.Background()))

	var enterOnce sync.Once
	record.mu.Lock()
	record.resources = []CursorTransferableResource{userSlot, accountSlot}
	record.settlement = func(_ context.Context, ev CursorSettlementEvent) {
		f.settleMu.Lock()
		f.settlements = append(f.settlements, cursorStopSettlementRecord{
			complete: ev.UsageComplete,
			missing:  ev.UsageMissingReason,
			reason:   ev.InterruptReason,
		})
		f.settleMu.Unlock()
		// 结算屏障：进入后阻塞，直到测试放行。Stop 若在此之前返回即为红。
		enterOnce.Do(func() { close(f.settleEntered) })
		<-f.settleRelease
		// 真实实现在这里提交计费任务；替身在返回后就算"已提交"。
		if ev.InflightRelease != nil {
			ev.InflightRelease()
		}
	}
	record.mu.Unlock()

	if completeUsage {
		record.noteTurnUsage(cursorTurnUsage{Seen: true, Input: 120, Output: 45, CacheRead: 7, EndedAtMS: 999}, "pump_terminal")
	} else {
		record.noteTurnUsage(cursorTurnUsage{Seen: true}, "pump_terminal")
	}

	// 置为 waiting_tools 并登记一条索引关联（Stop 后必须一并清零）。
	record.mu.Lock()
	record.state = cursorSessionWaitingTools
	record.pending = &cursorPendingBatch{batchID: "batch-1", responseID: "resp-1", callIDs: []string{"call-1"}, delivered: true}
	record.mu.Unlock()
	require.NoError(t, f.svc.sessions.registerSessionID(record.id, "batch-1", CursorIDKindResponse, "resp-1"))
	require.NoError(t, f.svc.sessions.registerToolCall(record.id, "batch-1", "call-1"))

	return f
}

// release 放行结算回调。
func (f *cursorStopFixture) release() {
	f.releaseOnce.Do(func() { close(f.settleRelease) })
}

func (f *cursorStopFixture) settlementRecords() []cursorStopSettlementRecord {
	f.settleMu.Lock()
	defer f.settleMu.Unlock()
	return append([]cursorStopSettlementRecord(nil), f.settlements...)
}

// TestCursorStopWaitsForUniqueFinalizerCompletion 覆盖 §25.17.7 A09：
// Stop 必须等唯一 finalizer 完成结算提交/资源归还/索引清理，绝不能把 finishing=true
// 当成"已完成"。这里用阻塞结算任务作确定性屏障：Stop 到达 finalizer 后卡在结算里，
// 释放前不得返回。
func TestCursorStopWaitsForUniqueFinalizerCompletion(t *testing.T) {
	f := newCursorStopFixture(t, true)

	stopDone := make(chan struct{})
	go func() {
		f.svc.Stop()
		close(stopDone)
	}()

	// 屏障①：Stop 确实触发了 finalizer 并进入结算。
	select {
	case <-f.settleEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop 未驱动会话走原 finalizer 结算")
	}

	// 屏障②：结算仍在进行时 Stop 绝不能已返回。
	select {
	case <-stopDone:
		t.Fatal("Stop 在唯一 finalizer 完成结算提交之前就返回了（不得以 finishing=true 当完成）")
	case <-time.After(100 * time.Millisecond):
	}

	f.release()

	select {
	case <-stopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("结算放行后 Stop 未返回")
	}

	// 完成后：索引/会话清零，槽各释放一次，真实 usage 落账一次。
	require.Zero(t, f.svc.sessions.sessionCount(), "Stop 返回后会话表必须清零")
	require.Zero(t, f.svc.sessions.indexCount(), "Stop 返回后统一索引必须清零")
	for _, res := range f.resources {
		require.EqualValues(t, 1, res.releases.Load(), "原槽只释放一次")
	}
	records := f.settlementRecords()
	require.Len(t, records, 1, "真实 usage 只沿既有结算回调落账一次")
	require.True(t, records[0].complete, "已拿完整真实 usage 必须判定为可结算")
	require.Empty(t, records[0].missing)
}

// TestCursorStopMissingUsageWritesNoZeroAccount 覆盖 §25.17.7 A09：
// 缺 usage 的会话在 cleanup 后不得写零账——结算回调只带中断/缺失原因，UsageComplete=false。
func TestCursorStopMissingUsageWritesNoZeroAccount(t *testing.T) {
	f := newCursorStopFixture(t, false)
	f.release()

	f.svc.Stop()

	records := f.settlementRecords()
	require.Len(t, records, 1, "缺 usage 也只通知一次结算方（用于记账中断原因）")
	require.False(t, records[0].complete, "缺 usage 不得判定为可结算")
	require.Equal(t, "turn_ended_fields_missing", records[0].missing)
	require.Equal(t, "service_stop", records[0].reason, "cleanup 触发的终结原因必须如实传给结算方")

	// 槽仍要释放，且只释放一次。
	for _, res := range f.resources {
		require.EqualValues(t, 1, res.releases.Load())
	}
	require.Zero(t, f.svc.sessions.sessionCount())
	require.Zero(t, f.svc.sessions.indexCount())
}

// TestCursorStopRepeatedStopNoDoubleSettleOrRelease 覆盖 §25.17.7 A09：
// 重复 Stop 不重复结算、不重复释放、不重复清理。
func TestCursorStopRepeatedStopNoDoubleSettleOrRelease(t *testing.T) {
	f := newCursorStopFixture(t, true)
	f.release()

	f.svc.Stop()
	f.svc.Stop()
	f.svc.Stop()

	require.Len(t, f.settlementRecords(), 1, "重复 Stop 不得重复结算")
	for _, res := range f.resources {
		require.EqualValues(t, 1, res.releases.Load(), "重复 Stop 不得重复释放原槽")
	}
	require.Zero(t, f.svc.sessions.sessionCount())
	require.Zero(t, f.svc.sessions.indexCount())
}

// TestCursorStopRaceWithResultHandoffSingleWinner 覆盖 §25.17.7 A09：
// cleanup 与结果回传竞争时只有一个胜者（结算一次、释放一次、索引清一次）。
func TestCursorStopRaceWithResultHandoffSingleWinner(t *testing.T) {
	for i := 0; i < 50; i++ {
		f := newCursorStopFixture(t, true)

		var wg sync.WaitGroup
		wg.Add(2)
		// 一侧：cleanup 驱动 Stop。
		go func() {
			defer wg.Done()
			f.release()
			f.svc.Stop()
		}()
		// 另一侧：结果回传/终止路径竞争同一个 finalizer。
		go func() {
			defer wg.Done()
			f.svc.finishCursorSession(f.record, "result_handoff")
		}()
		wg.Wait()

		records := f.settlementRecords()
		require.LessOrEqual(t, len(records), 1, "竞争后只允许一个结算胜者")
		if len(records) == 1 {
			require.True(t, records[0].complete)
		}
		for _, res := range f.resources {
			require.EqualValues(t, 1, res.releases.Load(), "竞争后槽只释放一次")
		}
		require.Zero(t, f.svc.sessions.sessionCount())
		require.Zero(t, f.svc.sessions.indexCount())
	}
}

// TestCursorStopSessionsStartUnavailableZeroAgentRun 覆盖 §25.17.7 A09：
// Stop 后 sessions.start 明确不可用（返回 nil），调用方在模型出站/AgentRun 之前
// 失败关闭——绝不返回可继续执行的游离记录。
func TestCursorStopSessionsStartUnavailableZeroAgentRun(t *testing.T) {
	f := newCursorStopFixture(t, true)
	f.release()
	f.svc.Stop()

	// Stop 之后新建会话：明确不可用（nil），不留游离记录。
	orphan := f.svc.sessions.start(cursorSessionSpec{
		protocol:       cursorProtocolChat,
		requestedModel: "cursor-test-model",
		upstreamModel:  "cursor-test-upstream",
		accountID:      42,
	})
	require.Nil(t, orphan, "Stop 之后 sessions.start 必须明确不可用（零 AgentRun）")
	require.Zero(t, f.svc.sessions.sessionCount(), "不可用请求不得留下游离会话记录")

	// 孤儿续轮关联同样失效（不得凭旧 ID 拿到可继续执行的会话）。
	require.Nil(t, f.svc.sessions.lookupSession(cursorProtocolChat, CursorIDKindResponse, "resp-1"),
		"Stop 后旧响应 ID 关联必须失效")
}

// TestCursorStopDuringFinalizeNewRequestZeroAgentRun 覆盖 §25.17.7 A09：
// Stop **进行中**（finalizer 卡在结算里）时的新请求同样零 AgentRun——
// 会话表在 Stop 入口即被标记 stopped，不等 finalizer 跑完。
func TestCursorStopDuringFinalizeNewRequestZeroAgentRun(t *testing.T) {
	f := newCursorStopFixture(t, true)

	stopDone := make(chan struct{})
	go func() {
		f.svc.Stop()
		close(stopDone)
	}()
	select {
	case <-f.settleEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop 未驱动 finalizer 进入结算")
	}

	// Stop 仍在进行：新会话明确不可用。
	orphan := f.svc.sessions.start(cursorSessionSpec{
		protocol:       cursorProtocolChat,
		requestedModel: "cursor-test-model",
		upstreamModel:  "cursor-test-upstream",
		accountID:      42,
	})
	require.Nil(t, orphan, "Stop 进行期间的新会话请求必须零 AgentRun（明确不可用）")

	f.release()
	select {
	case <-stopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop 未返回")
	}
}

// TestCursorStopFinalizeDoneNotClosedByDuplicateFinalizer 覆盖 §25.17.7 A09：
// finalizeDone 只由唯一胜者关闭；重复终结者不关闭它（否则 Stop 会误判"已结算"）。
func TestCursorStopFinalizeDoneNotClosedByDuplicateFinalizer(t *testing.T) {
	f := newCursorStopFixture(t, true)

	// 先让重复终结者在胜者完成前到达：它必须直接返回，不关闭完成信号。
	blocked := make(chan struct{})
	var unblockOnce sync.Once
	var enteredOnce sync.Once
	f.record.mu.Lock()
	f.record.settlement = func(_ context.Context, _ CursorSettlementEvent) {
		// 进入结算即关闭 settleEntered，供测试确认"胜者已到达结算"；随后阻塞到放行。
		enteredOnce.Do(func() { close(f.settleEntered) })
		<-blocked
	}
	f.record.mu.Unlock()

	var firstDone sync.WaitGroup
	firstDone.Add(1)
	go func() {
		defer firstDone.Done()
		f.svc.finishCursorSession(f.record, "service_stop")
	}()
	<-f.settleEntered

	// 重复终结者：不得关闭 finalizeDone。
	f.svc.finishCursorSession(f.record, "result_handoff")
	select {
	case <-f.record.finalizeSignal():
		t.Fatal("重复终结者不得关闭 finalizer 完成信号")
	default:
	}

	unblockOnce.Do(func() { close(blocked) })
	f.release()
	firstDone.Wait()

	select {
	case <-f.record.finalizeSignal():
	case <-time.After(5 * time.Second):
		t.Fatal("唯一胜者完成后必须关闭 finalizer 完成信号")
	}
}

// TestCursorProbeWorkerStartOnceStopOnce 覆盖 §25.17.7 A09：
// ProbeWorker 在原唯一构造处 Start 一次；cleanup Stop 一次，重复 Stop 不重复停。
func TestCursorProbeWorkerStartOnceStopOnce(t *testing.T) {
	w := NewChannelMonitorProbeWorker(nil, nil, nil, nil, &config.Config{})
	// 原唯一构造处（ProvideChannelMonitorProbeWorker）做的唯一一次 Start。
	w.Start()
	require.NotNil(t, w.stopCh, "Start 必须装配停止信号")
	require.NotNil(t, w.ticker, "Start 必须装配 tick 循环")

	w.Stop()
	select {
	case <-w.stopCh:
	default:
		t.Fatal("Stop 必须关闭停止信号")
	}

	// 重复 Stop 幂等：不再重复关闭（close 已关闭的 channel 会 panic，这里即证明）。
	require.NotPanics(t, func() {
		w.Stop()
		w.Stop()
	}, "重复 Stop 必须幂等（cleanup 只调一次也不得重复停）")

	// Start 一次的可观测结果：tick 循环已退出，Stop 立刻返回。
	done := make(chan struct{})
	go func() { w.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop 后 tick 循环未退出")
	}
}

// TestCursorStopRealUsageLandsOnceThroughUsageRecordQueue 覆盖 §25.17.7 A09：
// 已有真实 usage 沿**既有任务队列**（UsageRecordWorkerPool）落账一次；
// 缺 usage 的会话不写零账（队列里不得出现零usage 任务）。
//
// 这里用真实 worker 池（与 cleanup 的③同一个类型）承接结算回调提交的任务，
// Stop 之后 StopAndWait 排空，断言真实 usage 恰好落账一次。
func TestCursorStopRealUsageLandsOnceThroughUsageRecordQueue(t *testing.T) {
	for _, tc := range []struct {
		name         string
		complete     bool
		wantLedgered int
	}{
		{name: "已拿完整 usage", complete: true, wantLedgered: 1},
		{name: "缺 usage", complete: false, wantLedgered: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newCursorLifecycleTestService()
			record := startCursorLifecycleTestSession(t, svc)
			record.mu.Lock()
			record.state = cursorSessionWaitingTools
			record.mu.Unlock()
			require.NoError(t, svc.sessions.registerToolCall(record.id, "batch-1", "call-1"))

			pool := NewUsageRecordWorkerPool(&config.Config{})
			t.Cleanup(pool.Stop)

			var ledgered atomic.Int32
			record.mu.Lock()
			record.settlement = func(ctx context.Context, ev CursorSettlementEvent) {
				// 既有计费任务入口：只有 UsageComplete（已拿完整真实 usage）才提交真实账务；
				// 缺 usage 时连任务都不提交——绝不写零账假成功。
				if !ev.UsageComplete {
					return
				}
				pool.Submit(func(context.Context) { ledgered.Add(1) })
			}
			record.mu.Unlock()

			if tc.complete {
				record.noteTurnUsage(cursorTurnUsage{Seen: true, Input: 11, Output: 22, EndedAtMS: 5}, "pump_terminal")
			} else {
				record.noteTurnUsage(cursorTurnUsage{Seen: true}, "pump_terminal")
			}

			svc.Stop()
			pool.Stop() // 与 cleanup 的③同语义：排空已提交任务

			require.EqualValues(t, tc.wantLedgered, ledgered.Load(),
				"真实 usage 沿既有队列恰好落账一次；缺 usage 不写零账（连任务都不提交）")
		})
	}
}

// TestCursorThreeGatewaysShareSingleExecutionInstance 覆盖 §25.17.7 A09：
// 三网关（Anthropic / OpenAI / Gemini）仍共用**同一个** CursorExecutionService 实例——
// cleanup 停止的正是这一个，绝不出现第二个执行器（否则会话与 cleanup 就会脱节）。
func TestCursorThreeGatewaysShareSingleExecutionInstance(t *testing.T) {
	cfg := &config.Config{}
	exec := newCursorLifecycleTestService()

	anthropic := NewGatewayService(
		nil, nil, nil, nil, nil, nil, nil, nil, cfg,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, exec,
	)
	openai := NewOpenAIGatewayService(
		nil, nil, nil, nil, nil, nil, nil, cfg,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		exec,
	)
	gemini := NewGeminiMessagesCompatService(
		nil, nil, nil, nil, nil, nil, nil, nil, cfg, exec,
	)

	require.Same(t, exec, anthropic.cursorExecution, "Anthropic 网关必须共用同一执行器")
	require.Same(t, exec, openai.cursorExecution, "OpenAI 网关必须共用同一执行器")
	require.Same(t, exec, gemini.cursorExecution, "Gemini 网关必须共用同一执行器")
}

// TestCursorStopInflightReservationReturnedOnce 覆盖 §25.17.7 A09：
// 会话持有的在途预留在 Stop 后只归还一次（重复 Stop 不重复归还）。
func TestCursorStopInflightReservationReturnedOnce(t *testing.T) {
	f := newCursorStopFixture(t, true)
	f.release()

	cache := &cursorStopReservationCache{}
	// 预留句柄直接在包内构造（与生产同一类型、同一引用计数语义，不新增第二套预留）。
	res := &InflightReservation{
		cache:     cache,
		userID:    7,
		requestID: "req-1",
		amount:    1.5,
		ttl:       defaultInflightReservationTTL,
		// 跨 HTTP 会话持有的预留：只在会话终结时（SessionDone）归还，不新增第二套预留。
		deferred: true,
	}
	res.refs.Store(1)
	f.record.mu.Lock()
	f.record.inflight = res
	f.record.mu.Unlock()

	f.svc.Stop()
	f.svc.Stop()

	require.EqualValues(t, 1, cache.releases.Load(), "在途预留只归还一次（重复 Stop 不得重复归还）")
}

// cursorStopReservationCache 是 InflightBalanceReservationCache 的计数替身。
type cursorStopReservationCache struct {
	releases atomic.Int32
}

func (c *cursorStopReservationCache) ReserveInflightBalance(context.Context, int64, string, float64, float64, time.Duration) (bool, float64, error) {
	return true, 0, nil
}

func (c *cursorStopReservationCache) ReleaseInflightBalance(context.Context, int64, string) error {
	c.releases.Add(1)
	return nil
}

// newTestInflightReservation 构造一个**已转交（HoldPastHTTP）**的测试用预留句柄：
// 引用计数 1、延迟到会话终结归还。finalizer 调用 SessionDone() 时正好归还一次，
// 触发 cache.ReleaseInflightBalance——用于断言"在途预留只归还一次"。
func newTestInflightReservation(cache InflightBalanceReservationCache, userID int64, requestID string, amount float64) *InflightReservation {
	r := &InflightReservation{
		cache:     cache,
		userID:    userID,
		requestID: requestID,
		amount:    amount,
		ttl:       time.Minute,
	}
	r.refs.Store(1)
	r.HoldPastHTTP()
	return r
}
