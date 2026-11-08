package handler

// 覆盖「cursor 账号级上游故障」在换号循环里的接线：
//   - 该错误明确不做同账号重试，但必须仍然触发临时停号（幂等冷却 5 分钟);
//   - 账号被加入本次请求的排除列表，请求继续换到下一个账号（FailoverContinue）；
//   - 其他非 RetryableOnSameAccount 的错误不得因此被顺手停号（不扩大改动面）。
//
// 以及「上游集体抽风」判定（cursor 上游账号级失败的停调时长分级）：
//   - 同一请求内已失败的不同账号数达到 service.CursorUpstreamFlapThreshold 时，
//     判定为上游集体劣化，把后续账号的停调时长从 5 分钟降为 30 秒；
//   - 计数按 distinct 账号去重（同一账号重复失败不重复计数）；
//   - 计数跨换号迭代累计、不随迭代重置；
//   - 置位必须发生在停号动作之前（第 3 个账号停号时标记已为 true）。

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

// ---------------------------------------------------------------------------
// 上游集体抽风（flap）判定
// ---------------------------------------------------------------------------

// newCursorUpstreamFailureErr 构造一个「cursor 上游账号级失败」的 failover 错误。
//
// 每次调用都返回新对象：被测代码会把 CursorUpstreamFlapDetected 写在传入的
// failoverErr 上，复用同一对象会让前一次调用的标记污染后一次断言。
func newCursorUpstreamFailureErr() *service.UpstreamFailoverError {
	return &service.UpstreamFailoverError{
		StatusCode:        502,
		Scope:             service.GatewayFailureScopeAccount,
		Reason:            service.CursorUpstreamAccountFailureReason,
		NextAccountAction: service.NextAccountRetry,
		ClientStatusCode:  502,
	}
}

// TestHandleFailoverErrorCursorFlapNotDetectedBeforeThreshold 前 2 个不同账号因
// cursor 上游账号级失败而失败时，不得判定为「上游集体抽风」（停调仍为 5 分钟）。
func TestHandleFailoverErrorCursorFlapNotDetectedBeforeThreshold(t *testing.T) {
	fs := NewFailoverState(false)
	unscheduler := &mockTempUnscheduler{}

	for _, accountID := range []int64{101, 102} {
		act := fs.HandleFailoverError(context.Background(), unscheduler, accountID, service.PlatformCursor, 1,
			newCursorUpstreamFailureErr())
		require.Equal(t, FailoverContinue, act)
	}

	require.Len(t, unscheduler.calls, 2)
	require.False(t, unscheduler.calls[0].flapDetected, "第 1 个不同账号失败不得判定为集体抽风")
	require.False(t, unscheduler.calls[1].flapDetected, "第 2 个不同账号失败不得判定为集体抽风")
	require.Len(t, fs.CursorUpstreamFailedAccountIDs, 2)
}

// TestHandleFailoverErrorCursorFlapDetectedOnThirdDistinctAccount 第 3 个不同账号
// 因 cursor 上游账号级失败而失败时，必须判定为「上游集体抽风」。
//
// 同时验证时序：第 3 次调用停号时，传给 TempUnscheduleRetryableError 的标记
// 已经是 true（mock 在调用发生那一刻快照该值，故若置位被挪到停号之后，
// 本断言会精确变红）。
func TestHandleFailoverErrorCursorFlapDetectedOnThirdDistinctAccount(t *testing.T) {
	fs := NewFailoverState(false)
	unscheduler := &mockTempUnscheduler{}

	for _, accountID := range []int64{101, 102, 103} {
		act := fs.HandleFailoverError(context.Background(), unscheduler, accountID, service.PlatformCursor, 1,
			newCursorUpstreamFailureErr())
		require.Equal(t, FailoverContinue, act)
	}

	require.Len(t, unscheduler.calls, 3)
	require.False(t, unscheduler.calls[0].flapDetected)
	require.False(t, unscheduler.calls[1].flapDetected)
	require.True(t, unscheduler.calls[2].flapDetected,
		"第 3 个不同账号失败时，标记必须在停号动作之前就已置位（时序要求）")
	require.Len(t, fs.CursorUpstreamFailedAccountIDs, 3)
	require.Contains(t, fs.CursorUpstreamFailedAccountIDs, int64(103))
}

// TestHandleFailoverErrorCursorFlapSameAccountNotCountedTwice 同一账号连续失败
// 多次不重复计数（distinct 去重）：连续 3 次都是同一个账号时不得判定为集体抽风。
func TestHandleFailoverErrorCursorFlapSameAccountNotCountedTwice(t *testing.T) {
	fs := NewFailoverState(false)
	unscheduler := &mockTempUnscheduler{}

	for i := 0; i < 3; i++ {
		act := fs.HandleFailoverError(context.Background(), unscheduler, 201, service.PlatformCursor, 1,
			newCursorUpstreamFailureErr())
		require.Equal(t, FailoverContinue, act)
	}

	require.Len(t, unscheduler.calls, 3)
	for i, call := range unscheduler.calls {
		require.False(t, call.flapDetected,
			"同一账号重复失败不重复计数（第 %d 次仍不得判定为集体抽风）", i+1)
	}
	require.Len(t, fs.CursorUpstreamFailedAccountIDs, 1, "失败账号表按 distinct 账号去重")
}

// TestHandleFailoverErrorCursorFlapCountAccumulatesAcrossIterations 计数跨换号
// 迭代累计、不随迭代重置：同一个 FailoverState 连调 3 次，前两次不同账号、
// 第 3 次必须判定为集体抽风；中途夹一次其它原因的失败也不得清空计数。
func TestHandleFailoverErrorCursorFlapCountAccumulatesAcrossIterations(t *testing.T) {
	fs := NewFailoverState(false)
	unscheduler := &mockTempUnscheduler{}

	require.Equal(t, FailoverContinue, fs.HandleFailoverError(
		context.Background(), unscheduler, 301, service.PlatformCursor, 1, newCursorUpstreamFailureErr()))
	require.Len(t, fs.CursorUpstreamFailedAccountIDs, 1, "第 1 次迭代后计数为 1")
	require.Equal(t, 1, fs.SwitchCount)

	require.Equal(t, FailoverContinue, fs.HandleFailoverError(
		context.Background(), unscheduler, 302, service.PlatformCursor, 1, newCursorUpstreamFailureErr()))
	require.Len(t, fs.CursorUpstreamFailedAccountIDs, 2, "第 2 次迭代后计数累计为 2，不得重置")

	// 中途夹一次非 cursor 上游账号级失败的失败：不得清空已有计数。
	require.Equal(t, FailoverContinue, fs.HandleFailoverError(
		context.Background(), unscheduler, 303, service.PlatformCursor, 1,
		&service.UpstreamFailoverError{
			StatusCode:        502,
			Reason:            service.GatewayFailureReason("other_reason"),
			NextAccountAction: service.NextAccountRetry,
		}))
	require.Len(t, fs.CursorUpstreamFailedAccountIDs, 2, "其它原因的失败不得影响 flap 计数")

	require.Equal(t, FailoverContinue, fs.HandleFailoverError(
		context.Background(), unscheduler, 304, service.PlatformCursor, 1, newCursorUpstreamFailureErr()))
	require.Len(t, fs.CursorUpstreamFailedAccountIDs, 3)
	require.True(t, unscheduler.calls[len(unscheduler.calls)-1].flapDetected,
		"计数跨迭代累计，第 3 个不同账号失败时必须判定为集体抽风")
}

// TestHandleFailoverErrorCursorFlapNotTriggeredByOtherReasons 非 cursor 上游
// 账号级失败（如额度耗尽）既不参与 flap 计数，也不置 flap 标记。
func TestHandleFailoverErrorCursorFlapNotTriggeredByOtherReasons(t *testing.T) {
	fs := NewFailoverState(false)
	unscheduler := &mockTempUnscheduler{}

	for _, accountID := range []int64{401, 402, 403} {
		act := fs.HandleFailoverError(context.Background(), unscheduler, accountID, service.PlatformCursor, 1,
			&service.UpstreamFailoverError{
				StatusCode:        502,
				Scope:             service.GatewayFailureScopeAccount,
				Reason:            service.CursorQuotaExhaustedReason,
				NextAccountAction: service.NextAccountRetry,
				ClientStatusCode:  502,
			})
		require.Equal(t, FailoverContinue, act)
	}

	require.Len(t, unscheduler.calls, 3, "额度耗尽同样触发既有停号路径")
	for i, call := range unscheduler.calls {
		require.False(t, call.flapDetected,
			"非 cursor 上游账号级失败不得置 flap 标记（第 %d 次）", i+1)
	}
	require.Empty(t, fs.CursorUpstreamFailedAccountIDs, "非 cursor 上游账号级失败不参与 flap 计数")
}
