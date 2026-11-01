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
