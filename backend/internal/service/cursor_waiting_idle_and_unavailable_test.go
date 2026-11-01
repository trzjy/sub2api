//go:build unit

package service

// 本文件是派发单 A3 的定向测试（方案 cursor-tool-failure-user-diagnosis-plan-20261101
// §2.4「处理」第 3 条 + §2.5）：
//  1. 首轮正文前收到「当前模型暂不可用」模板 → 换号（*UpstreamFailoverError，
//     Reason=cursor_model_unavailable，NextAccountRetry），且不调停号回调；
//  2. 已发布正文后再收到模板 → 不换号，错误原样（errors.Is ErrCursorUpstreamModelUnavailable）；
//  3. 会话进入 waiting_tools 后客户端不续轮 → 空闲计时器到期走唯一 finalizer，
//     终止原因 waiting_tools_idle；
//  4. 超时前续轮接管 → 计时器被停，会话不被 finish。
//
// 复用夹具：newCursorRLStopService / cursorRLStopHandlerCall / cursorRLStopRequestBody
// （cursor_execution_batch_test.go）、cursorTextDeltaFrame / cursorTestEndStream
// （cursor_protocol_response_test.go）、cursorModelUnavailableLiveSample
// （cursor_model_unavailable_test.go）、newCursorBatchSeqFixture（cursor_batch_seq_test.go）。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// cursorModelUnavailableStreamBody 构造一条含模型不可用模板的上游流。
// withBody=true 时先发一帧普通正文（pump publish 后 bodyPublished 置位）。
func cursorModelUnavailableStreamBody(t *testing.T, withBody bool) []byte {
	t.Helper()
	var out []byte
	if withBody {
		out = append(out, cursorTextDeltaFrame(t, "Hi")...)
	}
	out = append(out, cursorTextDeltaFrame(t, cursorModelUnavailableLiveSample)...)
	out = append(out, cursorTestEndStream(t, `{}`)...)
	return out
}

func TestCursorExecuteModelUnavailableNoBodySwitchesAccount(t *testing.T) {
	for _, stream := range []bool{true, false} {
		svc, c := newCursorRLStopService(t, cursorModelUnavailableStreamBody(t, false))
		var calls cursorRLStopHandlerCall
		svc.cursorExecution.SetAccountExhaustedHandler(calls.fn)

		acc := cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A")
		res, err := executeCursorForTest(t, svc, context.Background(), c, acc,
			cursorRLStopRequestBody(stream), "cursor-sonnet", stream, time.Now())
		require.Error(t, err, "stream=%v", stream)
		require.Nil(t, res)

		var fe *UpstreamFailoverError
		require.ErrorAs(t, err, &fe, "stream=%v：首轮正文前模型不可用必须换号", stream)
		require.Equal(t, 503, fe.StatusCode)
		require.Equal(t, GatewayFailureReason("cursor_model_unavailable"), fe.Reason)
		require.Equal(t, GatewayFailureScopeAccount, fe.Scope)
		require.Equal(t, NextAccountRetry, fe.NextAccountAction)
		require.Equal(t, 503, fe.ClientStatusCode)
		require.True(t, fe.ShouldRetryNextAccount())

		n, _, _ := calls.snapshot()
		require.Zero(t, n, "模型不可用不是账号问题，不得调停号回调")
	}
}

func TestCursorExecuteModelUnavailableWithBodyKeepsOriginalError(t *testing.T) {
	svc, c := newCursorRLStopService(t, cursorModelUnavailableStreamBody(t, true))
	var calls cursorRLStopHandlerCall
	svc.cursorExecution.SetAccountExhaustedHandler(calls.fn)

	acc := cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A")
	res, err := executeCursorForTest(t, svc, context.Background(), c, acc,
		cursorRLStopRequestBody(true), "cursor-sonnet", true, time.Now())
	require.Error(t, err)
	require.Nil(t, res)

	require.True(t, errors.Is(err, ErrCursorUpstreamModelUnavailable), "已发布正文必须原样返回模型不可用错误")
	var fe *UpstreamFailoverError
	require.False(t, errors.As(err, &fe), "已发布正文不得换号")

	n, _, _ := calls.snapshot()
	require.Zero(t, n, "模型不可用不得调停号回调")
}

// setWaitingIdleTimeoutForTest 临时收紧空闲期限，t.Cleanup 还原。
func setWaitingIdleTimeoutForTest(t *testing.T, d time.Duration) {
	t.Helper()
	old := cursorWaitingToolsIdleTimeout
	cursorWaitingToolsIdleTimeout = d
	t.Cleanup(func() { cursorWaitingToolsIdleTimeout = old })
}

// deliverOneBatch 走生产入口交付一批，使会话进入 waiting_tools，返回该批。
func deliverOneBatch(t *testing.T, f *cursorBatchSeqFixture) *cursorPreparedBatch {
	t.Helper()
	f.noteMcpBatch(t, "mc-idle", "call-idle", "get_weather")
	require.NoError(t, f.delegations.seal())
	prepared, err := f.prepare(t)
	require.NoError(t, err)
	f.commit(t, prepared)
	f.record.mu.Lock()
	defer f.record.mu.Unlock()
	require.Equal(t, cursorSessionWaitingTools, f.record.state)
	return prepared
}

func cursorRecordFinishInfo(record *cursorSessionRecord) (bool, string) {
	record.mu.Lock()
	defer record.mu.Unlock()
	return record.finishing, record.interruptReason
}

func TestCursorWaitingIdleNoResumeFinishesSession(t *testing.T) {
	setWaitingIdleTimeoutForTest(t, 50*time.Millisecond)
	f := newCursorBatchSeqFixture(t)
	deliverOneBatch(t, f)

	select {
	case <-f.record.finalizeSignal():
	case <-time.After(3 * time.Second):
		t.Fatal("waiting_tools 空闲超时后会话必须被 finish")
	}
	finishing, reason := cursorRecordFinishInfo(f.record)
	require.True(t, finishing)
	require.Equal(t, "waiting_tools_idle", reason)
}

func TestCursorWaitingIdleResumeBeforeTimeoutStopsTimer(t *testing.T) {
	setWaitingIdleTimeoutForTest(t, 50*time.Millisecond)
	f := newCursorBatchSeqFixture(t)
	prepared := deliverOneBatch(t, f)

	detach, err := f.svc.sessions.beginResumeTakeover(&CursorResumeTicket{
		SessionID: f.record.id,
		BatchID:   prepared.batchID,
		CallIDs:   prepared.callIDs,
	}, nil)
	require.NoError(t, err)
	defer detach()

	f.record.mu.Lock()
	require.Nil(t, f.record.waitingIdleTimer, "续轮接管必须停掉并清空空闲计时器")
	require.Equal(t, cursorSessionRunning, f.record.state)
	f.record.mu.Unlock()

	time.Sleep(200 * time.Millisecond)
	finishing, _ := cursorRecordFinishInfo(f.record)
	require.False(t, finishing, "超时前已续轮接管的会话不得被空闲释放")
}

// 过期回调（批次已变/已消费）必须是 no-op：守卫只认当前未消费的同一批。
func TestCursorWaitingIdleStaleBatchCallbackNoop(t *testing.T) {
	setWaitingIdleTimeoutForTest(t, time.Hour)
	f := newCursorBatchSeqFixture(t)
	deliverOneBatch(t, f)

	f.svc.onWaitingToolsIdle(f.record, "other-batch")
	finishing, _ := cursorRecordFinishInfo(f.record)
	require.False(t, finishing, "批次不符的过期回调不得 finish 会话")

	f.svc.finishCursorSession(f.record, "test_cleanup")
	f.record.mu.Lock()
	require.Nil(t, f.record.waitingIdleTimer, "进入 finalizing 必须停掉并清空空闲计时器")
	f.record.mu.Unlock()
}

// D8：续轮接管已发生（beginResumeTakeover 的效果：state=running、pending.consumed=true）后
// 才到期的空闲回调，必须在 finalizer 同锁复核中被挡下，不得误杀刚接管的会话。
func TestCursorWaitingIdleAfterTakeoverDoesNotFinish(t *testing.T) {
	setWaitingIdleTimeoutForTest(t, time.Hour)
	f := newCursorBatchSeqFixture(t)
	prepared := deliverOneBatch(t, f)

	f.record.mu.Lock()
	f.record.state = cursorSessionRunning
	f.record.pending.consumed = true
	f.record.mu.Unlock()

	f.svc.onWaitingToolsIdle(f.record, prepared.batchID)
	f.record.mu.Lock()
	require.False(t, f.record.finishing, "已被接管的会话不得进入 finalizing")
	require.Equal(t, cursorSessionRunning, f.record.state)
	f.record.mu.Unlock()

	f.svc.finishCursorSession(f.record, "test_cleanup")
}

// D8：precond 在持 record.mu 时判定；不成立则不置 finishing、不改状态、不停计时器、返回 false。
func TestCursorWaitingIdleFinishIfPrecondFalseNoop(t *testing.T) {
	setWaitingIdleTimeoutForTest(t, time.Hour)
	f := newCursorBatchSeqFixture(t)
	deliverOneBatch(t, f)

	called := false
	ok := f.svc.finishCursorSessionIf(f.record, "waiting_tools_idle", func() bool {
		called = true
		return false
	})
	require.True(t, called)
	require.False(t, ok)
	f.record.mu.Lock()
	require.False(t, f.record.finishing)
	require.Equal(t, cursorSessionWaitingTools, f.record.state)
	require.NotNil(t, f.record.waitingIdleTimer, "precond 不成立不得动计时器")
	f.record.mu.Unlock()

	require.True(t, f.svc.finishCursorSessionIf(f.record, "test_cleanup", nil))
}

// D8：precond=true 正常终结；已有胜者时再调返回 false 且不调 precond。
func TestCursorWaitingIdleFinishIfPrecondTrueFinishes(t *testing.T) {
	setWaitingIdleTimeoutForTest(t, time.Hour)
	f := newCursorBatchSeqFixture(t)
	deliverOneBatch(t, f)

	ok := f.svc.finishCursorSessionIf(f.record, "waiting_tools_idle", func() bool { return true })
	require.True(t, ok)
	select {
	case <-f.record.finalizeSignal():
	default:
		t.Fatal("胜者返回时 finalizer 必须已完成")
	}
	finishing, reason := cursorRecordFinishInfo(f.record)
	require.True(t, finishing)
	require.Equal(t, "waiting_tools_idle", reason)
	f.record.mu.Lock()
	require.Equal(t, cursorSessionClosed, f.record.state)
	require.Nil(t, f.record.waitingIdleTimer)
	f.record.mu.Unlock()

	called := false
	require.False(t, f.svc.finishCursorSessionIf(f.record, "dup", func() bool { called = true; return true }))
	require.False(t, called, "已有胜者时不应再判定 precond")
	_, reason = cursorRecordFinishInfo(f.record)
	require.Equal(t, "waiting_tools_idle", reason)
}

// D8：空闲回调与续轮接管并发，每轮只能有一个胜者（-race 下跑）。
func TestCursorWaitingIdleTakeoverRaceSingleWinner(t *testing.T) {
	setWaitingIdleTimeoutForTest(t, time.Hour)
	for i := 0; i < 200; i++ {
		f := newCursorBatchSeqFixture(t)
		prepared := deliverOneBatch(t, f)
		ticket := &CursorResumeTicket{
			SessionID: f.record.id,
			BatchID:   prepared.batchID,
			CallIDs:   prepared.callIDs,
		}

		start := make(chan struct{})
		done := make(chan struct{}, 2)
		var detach func()
		var takeoverErr error
		go func() {
			<-start
			f.svc.onWaitingToolsIdle(f.record, prepared.batchID)
			done <- struct{}{}
		}()
		go func() {
			<-start
			detach, takeoverErr = f.svc.sessions.beginResumeTakeover(ticket, nil)
			done <- struct{}{}
		}()
		close(start)
		<-done
		<-done

		f.record.mu.Lock()
		finishing, state := f.record.finishing, f.record.state
		f.record.mu.Unlock()
		if takeoverErr == nil {
			require.False(t, finishing, "第 %d 轮：接管胜出后不得再被空闲终结", i)
			require.Equal(t, cursorSessionRunning, state, "第 %d 轮", i)
			detach()
			f.svc.finishCursorSession(f.record, "test_cleanup")
		} else {
			require.True(t, finishing, "第 %d 轮：接管失败则空闲终结必须是胜者", i)
			_, reason := cursorRecordFinishInfo(f.record)
			require.Equal(t, "waiting_tools_idle", reason, "第 %d 轮", i)
		}
	}
}
