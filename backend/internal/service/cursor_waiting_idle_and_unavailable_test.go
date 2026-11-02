//go:build unit

package service

// 本文件是派发单 A3 的定向测试（方案 cursor-tool-failure-user-diagnosis-plan-20261101
// §2.4「处理」第 3 条 + §2.5）：
//  1. 首轮正文前收到「当前模型暂不可用」模板 → 换号（*UpstreamFailoverError，
//     Reason=cursor_model_unavailable，NextAccountRetry），且不调停号回调；
//  2. 已发布正文后再收到模板 → 不换号，错误原样（errors.Is ErrCursorUpstreamModelUnavailable）；
//  3. 会话进入 waiting_tools 后由客户端续轮接管，继续复用同一 AgentRun；
//     不由适配层空闲计时器截断。
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
