//go:build unit

package handler

// 本文件是 **§25.4 handler 资源句柄转交**与 **§25.17.5 A06 唯一结算输入**的定向测试。
//
// 覆盖：
//  1. 转交在同一锁内停掉 HTTP 取消回调：转交后 HTTP 取消不再释放，session 取消才释放；
//  2. 已取消/已释放的句柄交接必须失败（不能把失效槽位交 session）；
//  3. 重复转交被拒绝；
//  4. 取消回调与转交竞争有唯一胜者（-race）；
//  5. 非 Cursor 账号不登记转交物（显式清空，保持原 HTTP 生命周期）；
//  6. 唯一结算输入：注册的 CursorSettlementFunc 收到终态 *ForwardResult 且至多一次、
//     使用会话级不可变账单事件 ID、重复注册按替换生效、未注册/晚注册/缺终态分别是
//     明确装配错误；
//  7. 未取得完整真实 usage 时一律不结算（不写零 usage 假成功）且仍归还预留引用；
//  8. UsageRecordTask 晚绑定链已从 helper 删除（源码归零断言）。

import (
	"context"
	"os"
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

// TestBindCursorHandoverForAttemptNonCursorClears 覆盖 §25.4 与 §25.17.5 A06：
// 未选中 Cursor 的其它供应商不得登记转交物（并显式清空上一次尝试的转交物）；
// helper 已删除 UsageRecordTask 晚绑定链，转交物不再充当"结算所有者"，
// 因此 handler 的 Cursor owner 改投分支不再命中（非 Cursor 计费路径保持原样）。
func TestBindCursorHandoverForAttemptNonCursorClears(t *testing.T) {
	cursorAccount := &service.Account{ID: 1, Platform: service.PlatformCursor}
	openAIAccount := &service.Account{ID: 2, Platform: service.PlatformOpenAI}
	handover := newCursorResourceHandover(nil, nil)

	ctx := bindCursorHandoverForAttempt(context.Background(), cursorAccount, handover)
	require.Same(t, handover, service.CursorSessionHandoverFromContext(ctx), "Cursor 账号必须登记转交物")
	require.Nil(t, service.CursorSettlementOwnerFromContext(ctx),
		"晚绑定链删除后转交物不得再实现结算所有者接口（否则 handler 会再次改投 UsageRecordTask）")

	ctx = bindCursorHandoverForAttempt(ctx, openAIAccount, handover)
	require.Nil(t, service.CursorSessionHandoverFromContext(ctx), "非 Cursor 账号必须清空转交物")
	require.Nil(t, service.CursorSettlementOwnerFromContext(ctx))
}

// TestCursorSettlementRegisteredFuncReceivesTerminalResultOnce 覆盖 §25.17.5 A06：
// 注册的 CursorSettlementFunc 收到**终态真实 *ForwardResult**，只执行一次，
// 提交 ctx 带会话级不可变账单事件 ID，提交之后才归还预留引用。
func TestCursorSettlementRegisteredFuncReceivesTerminalResultOnce(t *testing.T) {
	type submission struct {
		calls int
		ctx   context.Context
		final *service.ForwardResult
	}
	sub := &submission{}
	var mu sync.Mutex
	handover := newCursorResourceHandover(nil, nil)

	eventID := "cursor_session:unit-test"
	handover.RegisterCursorSettlement(func(ctx context.Context, final *service.ForwardResult) {
		mu.Lock()
		sub.calls++
		sub.ctx = ctx
		sub.final = final
		mu.Unlock()
	})
	require.NotNil(t, handover.SettlementFunc(), "注册后 finalizer 侧访问器必须能取出同一个钩子")
	require.False(t, handover.Settled(), "未终结前不得视为已结算")

	releaseCalls := atomic.Int32{}
	final := &service.ForwardResult{
		Model: "cursor-final",
		Usage: service.ClaudeUsage{InputTokens: 11, OutputTokens: 22, CacheReadInputTokens: 3, CacheCreationInputTokens: 4},
	}
	ev := service.CursorSettlementEvent{
		SessionID:       "unit-test",
		BillingEventID:  eventID,
		Result:          final,
		UsageComplete:   true,
		InflightRelease: func() { releaseCalls.Add(1) },
	}
	handover.Settlement()(context.Background(), ev)
	require.True(t, handover.Settled())
	require.NoError(t, handover.SettlementError())

	mu.Lock()
	calls, gotCtx, gotFinal := sub.calls, sub.ctx, sub.final
	mu.Unlock()
	require.Equal(t, 1, calls, "唯一结算至多执行一次")
	require.Same(t, final, gotFinal, "必须把终态真实 ForwardResult 原样交给结算钩子（不是首 HTTP 的空 usage）")
	require.EqualValues(t, 1, releaseCalls.Load(), "结算提交后归还在途预留引用")

	boundID, _ := gotCtx.Value(ctxkey.ClientRequestID).(string)
	require.Equal(t, eventID, boundID, "提交 ctx 必须携带会话级不可变账单事件 ID")
	require.Equal(t,
		service.CursorSettlementDurableRequestID(eventID),
		service.CursorSettlementDurableRequestID(boundID),
		"同一会话的重复结算落到同一个 durable 键")

	// 重复终结（Stop 与结果回传竞争）不得重复扣费、不得重复归还。
	handover.Settlement()(context.Background(), ev)
	mu.Lock()
	after := sub.calls
	mu.Unlock()
	require.Equal(t, 1, after, "重复终结不得重复结算")
	require.EqualValues(t, 1, releaseCalls.Load())
}

// TestCursorSettlementRegisterReplacesPrevious 覆盖 §25.17.5 A06 的幂等选择：
// 重复注册按"最后一次显式装配生效"替换（不静默保留旧快照）。
func TestCursorSettlementRegisterReplacesPrevious(t *testing.T) {
	var first, second atomic.Int32
	handover := newCursorResourceHandover(nil, nil)
	handover.RegisterCursorSettlement(func(context.Context, *service.ForwardResult) { first.Add(1) })
	handover.RegisterCursorSettlement(func(context.Context, *service.ForwardResult) { second.Add(1) })

	handover.Settlement()(context.Background(), service.CursorSettlementEvent{
		SessionID:      "unit-test",
		BillingEventID: "cursor_session:replace",
		Result:         &service.ForwardResult{Model: "cursor-final"},
		UsageComplete:  true,
	})
	require.EqualValues(t, 0, first.Load(), "被替换的旧钩子不得计费")
	require.EqualValues(t, 1, second.Load(), "最后一次注册的钩子生效")
}

// TestCursorSettlementNotRegisteredIsAssemblyError 覆盖 §25.17.5 A06：
// 完整 usage 却缺注册属**明确装配错误**——不提交、不写零 usage 账、可查错误，
// 但预留引用必须归还（不得留下无人归还的预留）。
func TestCursorSettlementNotRegisteredIsAssemblyError(t *testing.T) {
	var released atomic.Int32
	handover := newCursorResourceHandover(nil, nil)

	handover.Settlement()(context.Background(), service.CursorSettlementEvent{
		SessionID:       "unit-test",
		BillingEventID:  "cursor_session:no-hook",
		Result:          &service.ForwardResult{Model: "cursor-final"},
		UsageComplete:   true,
		InflightRelease: func() { released.Add(1) },
	})
	require.False(t, handover.Settled(), "缺注册不得假装已结算")
	require.ErrorIs(t, handover.SettlementError(), errCursorSettlementNotRegistered)
	require.EqualValues(t, 1, released.Load(), "未结算也必须归还预留引用")
	require.True(t, handover.Finalized())
}

// TestCursorSettlementMissingTerminalResultIsAssemblyError 覆盖 §25.17.5 A06：
// 完整性判定通过但没有真实终态结果可交，同样是明确装配错误（不得用空结果造账）。
func TestCursorSettlementMissingTerminalResultIsAssemblyError(t *testing.T) {
	var calls atomic.Int32
	var released atomic.Int32
	handover := newCursorResourceHandover(nil, nil)
	handover.RegisterCursorSettlement(func(context.Context, *service.ForwardResult) { calls.Add(1) })

	handover.Settlement()(context.Background(), service.CursorSettlementEvent{
		SessionID:       "unit-test",
		BillingEventID:  "cursor_session:no-result",
		UsageComplete:   true,
		InflightRelease: func() { released.Add(1) },
	})
	require.EqualValues(t, 0, calls.Load(), "缺终态结果不得结算")
	require.ErrorIs(t, handover.SettlementError(), errCursorSettlementMissingResult)
	require.EqualValues(t, 1, released.Load())
}

// TestCursorSettlementLateRegistrationRejected 覆盖 §25.17.5 A06 的注册时机约束：
// 会话终结后才注册无法再结算，必须拒绝并记录装配错误（不假装成功）。
func TestCursorSettlementLateRegistrationRejected(t *testing.T) {
	handover := newCursorResourceHandover(nil, nil)
	handover.Settlement()(context.Background(), service.CursorSettlementEvent{
		SessionID:      "unit-test",
		BillingEventID: "cursor_session:late",
		Result:         &service.ForwardResult{Model: "cursor-final"},
		UsageComplete:  true,
	})
	require.ErrorIs(t, handover.SettlementError(), errCursorSettlementNotRegistered)

	var calls atomic.Int32
	handover.RegisterCursorSettlement(func(context.Context, *service.ForwardResult) { calls.Add(1) })
	require.Nil(t, handover.SettlementFunc(), "终结后注册必须被拒绝")
	require.ErrorIs(t, handover.SettlementError(), errCursorSettlementLateRegister)
	require.EqualValues(t, 0, calls.Load())
	require.False(t, handover.Settled())
}

// TestCursorSettlementSkippedWhenUsageIncomplete 覆盖 §25.5/§25.17.5 A06：
// 未取得完整真实 usage 时不提交结算（不写零 usage 假成功），但归还预留引用；
// 即使终态带了结果对象也不得以不完整 usage 计费。
func TestCursorSettlementSkippedWhenUsageIncomplete(t *testing.T) {
	var calls atomic.Int32
	var released atomic.Int32
	handover := newCursorResourceHandover(nil, nil)
	handover.RegisterCursorSettlement(func(context.Context, *service.ForwardResult) { calls.Add(1) })

	handover.Settlement()(context.Background(), service.CursorSettlementEvent{
		SessionID:       "unit-test",
		BillingEventID:  "cursor_session:incomplete",
		Result:          &service.ForwardResult{Model: "cursor-final"},
		UsageComplete:   false,
		InflightRelease: func() { released.Add(1) },
	})
	require.EqualValues(t, 0, calls.Load(), "未取得完整 usage 时不得结算")
	require.False(t, handover.Settled())
	require.NoError(t, handover.SettlementError(), "缺 usage 是已接受的契约路径，不是装配错误")
	require.EqualValues(t, 1, released.Load(), "未结算也必须归还预留引用")
}

// TestCursorLateBindingChainRemoved 覆盖 §25.17.5 A06 删除项：
// helper 内不得再出现 UsageRecordTask 晚绑定链的入口（AdoptSettlementTask），
// 结算路径也不得再持有 UsageRecordTask 字段/提交函数。
// 直接扫描本包生产文件，避免"删除后又被悄悄加回"。
//
// 说明：`cursorSettlementSubmitter`（既有 worker 池提交签名）只作为 7 处 handler 构造
// 调用点的参数类型保留（本单禁区不可改 handler 文件），不参与任何结算；因此这里断言的是
// "结算路径不再使用它"，而非该类型名的字面归零。
func TestCursorLateBindingChainRemoved(t *testing.T) {
	source, err := os.ReadFile("gateway_helper.go")
	require.NoError(t, err, "必须能读取本包生产文件以核对晚绑定链归零")
	text := string(source)
	require.NotContains(t, text, "AdoptSettlementTask",
		"§25.17.5 A06 已删除晚绑定链：helper 内不得再出现 AdoptSettlementTask")
	require.NotContains(t, text, "submitTask",
		"§25.17.5 A06：转交物不得再持有计费任务字段")
}
