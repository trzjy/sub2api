//go:build unit

package handler

// 本文件是 **§25.4 handler 资源句柄转交**的定向测试（派发单 W2-D2）。
//
// 覆盖：
//  1. 转交在同一锁内停掉 HTTP 取消回调：转交后 HTTP 取消不再释放，session 取消才释放；
//  2. 已取消/已释放的句柄交接必须失败（不能把失效槽位交 session）；
//  3. 重复转交被拒绝；
//  4. 取消回调与转交竞争有唯一胜者（-race）；
//  5. 非 Cursor 账号不登记转交物（显式清空，保持原 HTTP 生命周期）；
//  6. 会话唯一结算：至多提交一次、使用会话级不可变账单事件 ID、字段缺失不结算。

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestCursorHandoverTransferStopsHTTPCancel 覆盖 §25.4：
// 转交后释放所有权归会话，HTTP context 取消不得再释放；session context 取消才释放。
func TestCursorHandoverTransferStopsHTTPCancel(t *testing.T) {
	var releases atomic.Int32
	httpCtx, cancelHTTP := context.WithCancel(context.Background())
	handle := newTransferableRelease(httpCtx, "user_slot", func() { releases.Add(1) })
	require.NotNil(t, handle)

	sessionCtx, cancelSession := context.WithCancel(context.Background())
	require.NoError(t, handle.TransferTo(sessionCtx))
	require.True(t, handle.HandedOver())

	// HTTP 生命周期结束（handler 正常返回 → gin/nethttp 取消请求 ctx）。
	// 用 require.Never 做**有界否定断言**：不靠 sleep 规避竞态，也不等待固定时长后取样。
	cancelHTTP()
	require.Never(t, func() bool { return releases.Load() > 0 }, 100*time.Millisecond, 5*time.Millisecond,
		"转交后 HTTP 取消不得释放槽位")

	// handler 侧 defer 仍会调用 Release：必须成为空操作。
	handle.Release()
	require.EqualValues(t, 0, releases.Load(), "已转交句柄的 handler 侧 Release 必须是空操作")

	// 会话终结才释放，且只释放一次。
	cancelSession()
	require.Eventually(t, func() bool { return releases.Load() == 1 }, 2*time.Second, 5*time.Millisecond)
	handle.Release()
	require.EqualValues(t, 1, releases.Load(), "整个生命周期只 release 一次")
}

// TestCursorHandoverTransferAfterReleaseFails 覆盖 §25.4：
// 已取消/已释放则本次交接失败，不能把失效槽位交 session。
func TestCursorHandoverTransferAfterReleaseFails(t *testing.T) {
	var releases atomic.Int32
	handle := newTransferableRelease(context.Background(), "account_slot", func() { releases.Add(1) })
	handle.Release()
	require.EqualValues(t, 1, releases.Load())

	require.ErrorIs(t, handle.TransferTo(context.Background()), errCursorHandleReleased)
	require.False(t, handle.HandedOver())
	require.EqualValues(t, 1, releases.Load(), "交接失败不得额外释放")

	// 由 HTTP 取消触发的释放同样使交接失败。
	var releases2 atomic.Int32
	httpCtx, cancelHTTP := context.WithCancel(context.Background())
	handle2 := newTransferableRelease(httpCtx, "user_slot", func() { releases2.Add(1) })
	cancelHTTP()
	require.Eventually(t, func() bool { return releases2.Load() == 1 }, 2*time.Second, 5*time.Millisecond)
	require.ErrorIs(t, handle2.TransferTo(context.Background()), errCursorHandleReleased)
}

// TestCursorHandoverTransferTwiceFails：同一句柄不得被转交两次。
func TestCursorHandoverTransferTwiceFails(t *testing.T) {
	handle := newTransferableRelease(context.Background(), "user_slot", func() {})
	sessionCtx := context.Background()
	require.NoError(t, handle.TransferTo(sessionCtx))
	require.ErrorIs(t, handle.TransferTo(sessionCtx), errCursorHandleHanded)
}

// TestCursorHandoverRaceSingleWinner 覆盖 §25.4：取消回调与转交竞争必须有唯一胜者，
// 不得出现"既释放又交给会话"或双重释放（本测试在 -race 下运行）。
func TestCursorHandoverRaceSingleWinner(t *testing.T) {
	for i := 0; i < 200; i++ {
		var releases atomic.Int32
		httpCtx, cancelHTTP := context.WithCancel(context.Background())
		handle := newTransferableRelease(httpCtx, "account_slot", func() { releases.Add(1) })
		sessionCtx, cancelSession := context.WithCancel(context.Background())

		var wg sync.WaitGroup
		wg.Add(2)
		var transferErr error
		go func() {
			defer wg.Done()
			transferErr = handle.TransferTo(sessionCtx)
		}()
		go func() {
			defer wg.Done()
			cancelHTTP()
		}()
		wg.Wait()

		if transferErr != nil {
			// 交接失败：槽位必须仍由 HTTP 生命周期释放，且只释放一次。
			cancelHTTP()
			require.Eventually(t, func() bool { return releases.Load() >= 1 }, 2*time.Second, time.Millisecond)
		}
		cancelSession()
		handle.Release()

		require.Eventually(t, func() bool { return releases.Load() == 1 }, 2*time.Second, time.Millisecond,
			"竞争下必须恰好释放一次（第 %d 轮）", i)
		require.LessOrEqual(t, releases.Load(), int32(1), "不得双重释放")
	}
}

// TestBindCursorHandoverForAttemptNonCursorClears 覆盖 §25.4：
// 未选中 Cursor 的其它供应商不得登记转交物（并显式清空上一次尝试的转交物）。
func TestBindCursorHandoverForAttemptNonCursorClears(t *testing.T) {
	cursorAccount := &service.Account{ID: 1, Platform: service.PlatformCursor}
	openAIAccount := &service.Account{ID: 2, Platform: service.PlatformOpenAI}
	handover := newCursorResourceHandover(nil, nil)

	ctx := bindCursorHandoverForAttempt(context.Background(), cursorAccount, handover)
	require.Same(t, handover, service.CursorSessionHandoverFromContext(ctx), "Cursor 账号必须登记转交物")
	require.NotNil(t, service.CursorSettlementOwnerFromContext(ctx), "结算所有者与转交物是同一对象")

	ctx = bindCursorHandoverForAttempt(ctx, openAIAccount, handover)
	require.Nil(t, service.CursorSessionHandoverFromContext(ctx), "非 Cursor 账号必须清空转交物")
	require.Nil(t, service.CursorSettlementOwnerFromContext(ctx))
}

// TestCursorHandoverSettlementSubmittedOnceWithSessionEventID 覆盖 §25.5：
// 会话唯一结算至多提交一次，且提交 ctx 带会话级不可变账单事件 ID。
func TestCursorHandoverSettlementSubmittedOnceWithSessionEventID(t *testing.T) {
	type submission struct {
		ctx    context.Context
		ran    atomic.Int32
		submit int
	}
	sub := &submission{}
	var mu sync.Mutex
	handover := newCursorResourceHandover(nil, func(ctx context.Context, task service.UsageRecordTask) {
		mu.Lock()
		sub.submit++
		sub.ctx = ctx
		mu.Unlock()
		if task != nil {
			task(ctx)
		}
	})

	eventID := "cursor_session:unit-test"
	releaseCalls := atomic.Int32{}
	ev := service.CursorSettlementEvent{
		SessionID:       "unit-test",
		BillingEventID:  eventID,
		UsageComplete:   true,
		InflightRelease: func() { releaseCalls.Add(1) },
	}

	// 会话终结时本 HTTP 的计费任务尚未登记（handler 计费块在其后执行）。
	handover.Settlement()(context.Background(), ev)
	require.False(t, handover.Settled(), "任务未登记时不得提前提交")

	// handler 计费块登记任务 → 立即结算一次。
	handover.AdoptSettlementTask(func(context.Context) { sub.ran.Add(1) })
	require.True(t, handover.Settled())
	require.EqualValues(t, 1, sub.ran.Load())
	require.EqualValues(t, 1, releaseCalls.Load(), "结算提交后归还在途预留引用")

	// 重复登记/重复续轮不得重复扣费。
	handover.AdoptSettlementTask(func(context.Context) { sub.ran.Add(1) })
	require.EqualValues(t, 1, sub.ran.Load(), "唯一结算至多执行一次")
	require.EqualValues(t, 1, releaseCalls.Load())

	mu.Lock()
	gotSubmit := sub.submit
	gotCtx := sub.ctx
	mu.Unlock()
	require.Equal(t, 1, gotSubmit)
	boundID, _ := gotCtx.Value(ctxkey.ClientRequestID).(string)
	require.Equal(t, eventID, boundID, "提交 ctx 必须携带会话级不可变账单事件 ID")
	require.Equal(t,
		service.CursorSettlementDurableRequestID(eventID),
		service.CursorSettlementDurableRequestID(boundID),
		"同一会话的重复结算落到同一个 durable 键")
}

// TestCursorHandoverSettlementSkippedWhenUsageIncomplete 覆盖 §25.5：
// 未取得完整真实 usage 时不提交结算（不写零 usage 假成功），但归还预留引用。
func TestCursorHandoverSettlementSkippedWhenUsageIncomplete(t *testing.T) {
	var submitted atomic.Int32
	var released atomic.Int32
	handover := newCursorResourceHandover(nil, func(context.Context, service.UsageRecordTask) {
		submitted.Add(1)
	})

	handover.Settlement()(context.Background(), service.CursorSettlementEvent{
		SessionID:       "unit-test",
		BillingEventID:  "cursor_session:unit-test",
		UsageComplete:   false,
		InflightRelease: func() { released.Add(1) },
	})
	handover.AdoptSettlementTask(func(context.Context) { submitted.Add(1) })

	require.EqualValues(t, 0, submitted.Load(), "未取得完整 usage 时不得结算")
	require.False(t, handover.Settled())
	require.EqualValues(t, 1, released.Load(), "未结算也必须归还预留引用")
}
