package handler

// 覆盖「cursor 账号级上游故障」在换号循环里的接线：
//   - 该错误明确不做同账号重试，但必须仍然触发临时停号（幂等冷却 5 分钟）；
//   - 账号被加入本次请求的排除列表，请求继续换到下一个账号（FailoverContinue）；
//   - 其他非 RetryableOnSameAccount 的错误不得因此被顺手停号（不扩大改动面）。

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestHandleFailoverErrorCursorAccountFailureTempUnschedules(t *testing.T) {
	fs := NewFailoverState(false)
	unscheduler := &mockTempUnscheduler{}

	act := fs.HandleFailoverError(context.Background(), unscheduler, 362, service.PlatformCursor, 1,
		&service.UpstreamFailoverError{
			StatusCode:        502,
			Scope:             service.GatewayFailureScopeAccount,
			Reason:            service.CursorUpstreamAccountFailureReason,
			NextAccountAction: service.NextAccountRetry,
			ClientStatusCode:  502,
		})

	require.Equal(t, FailoverContinue, act, "必须继续换到下一个账号，而不是把 502 返回给用户")
	require.Len(t, unscheduler.calls, 1, "账号级上游故障必须停号冷却")
	require.Equal(t, int64(362), unscheduler.calls[0].accountID)
	require.Contains(t, fs.FailedAccountIDs, int64(362), "失败账号必须进入本次请求的排除列表")
}

func TestHandleFailoverErrorOtherNonRetryableDoesNotTempUnschedule(t *testing.T) {
	fs := NewFailoverState(false)
	unscheduler := &mockTempUnscheduler{}

	act := fs.HandleFailoverError(context.Background(), unscheduler, 7, service.PlatformCursor, 0,
		&service.UpstreamFailoverError{
			StatusCode:        502,
			Reason:            service.GatewayFailureReason("other_reason"),
			NextAccountAction: service.NextAccountRetry,
		})

	require.Equal(t, FailoverContinue, act)
	require.Empty(t, unscheduler.calls, "不得扩大既有停号条件")
}
