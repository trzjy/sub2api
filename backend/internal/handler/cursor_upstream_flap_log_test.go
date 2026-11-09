//go:build unit

package handler

// 本文件钉死「cursor 上游集体抽风」判定日志的契约（外审补漏单）：
//
// 既有测试只断言了 flap 的计数与布尔标记，service 层测试只断言了冷却时长与
// reason —— 若后续把这条日志的字段删除或改名，测试仍会全绿。故本文件补上
// observer 日志断言，锁定两件事：
//
//  1. 事件唯一触发：同一请求内连续多个不同账号因 cursor 上游账号级失败而失败，
//     「首次达成阈值」那一刻只打一条日志，重复账号不重复打。
//  2. 全部诊断字段：distinct_accounts / cooldown / account_id /
//     failed_account_ids / upstream_code / upstream_message 六个字段名与取值
//     都被精确绑定 —— 字段被改名或删除时本文件必须变红。

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// flapLogTestUpstreamCode / flapLogTestUpstreamMessage 是传给 failoverErr 的
// 上游传输错误码与原文摘要，用于断言日志原样带出（不被改写、不被截断成别的）。
const (
	flapLogTestUpstreamCode    = "internal"
	flapLogTestUpstreamMessage = "upstream stream interrupted: connection reset by peer"
)

// flapLogTestEventName 是抽风判定日志的固定事件名。
const flapLogTestEventName = "cursor_upstream_flap_detected"

// newCursorUpstreamFlapLogContext 造一个带 observer logger 的 context。
//
// 抽风判定日志由 logger.FromContext(ctx) 打出，故必须把 observer logger 注入
// ctx，否则测试只能看到全局 logger 的输出、无法断言字段。
func newCursorUpstreamFlapLogContext() (context.Context, *observer.ObservedLogs) {
	core, logs := observer.New(zap.WarnLevel)
	return logger.IntoContext(context.Background(), zap.New(core)), logs
}

// newCursorUpstreamFlapFailureErr 构造一次「cursor 上游账号级失败」的 failover 错误，
// 并带上游错误码与原文摘要。
//
// 每次调用都返回新对象：被测代码会把 CursorUpstreamFlapDetected 写在传入的
// failoverErr 上，复用同一对象会让前一次调用的标记污染后一次断言。
func newCursorUpstreamFlapFailureErr(code, message string) *service.UpstreamFailoverError {
	return &service.UpstreamFailoverError{
		StatusCode:                 502,
		Scope:                      service.GatewayFailureScopeAccount,
		Reason:                     service.CursorUpstreamAccountFailureReason,
		NextAccountAction:          service.NextAccountRetry,
		ClientStatusCode:           502,
		CursorUpstreamErrorCode:    code,
		CursorUpstreamErrorMessage: message,
	}
}

// TestCursorUpstreamFlapLogFiresOnceWithFullDiagnostics 连续 5 个不同账号因
// cursor 上游账号级失败而失败：抽风判定日志恰好打一条，且这条日志的六个诊断
// 字段全部正确。
//
// 字段断言刻意用 require.Contains(t, fields, "<字段名>") + 取值相等两步：
// 字段名被改名时 Contains 直接红；字段被删除时 Contains 红（且取值断言拿到 nil
// 也不等于期望值，双保险）。
func TestCursorUpstreamFlapLogFiresOnceWithFullDiagnostics(t *testing.T) {
	ctx, logs := newCursorUpstreamFlapLogContext()
	fs := NewFailoverState(false)
	unscheduler := &mockTempUnscheduler{}

	for _, accountID := range []int64{101, 102, 103, 104, 105} {
		act := fs.HandleFailoverError(ctx, unscheduler, accountID, service.PlatformCursor, 0,
			newCursorUpstreamFlapFailureErr(flapLogTestUpstreamCode, flapLogTestUpstreamMessage))
		require.Equal(t, FailoverContinue, act)
	}

	entries := logs.FilterMessage(flapLogTestEventName).All()
	require.Len(t, entries, 1,
		"抽风判定日志在整个请求内只能打一条（第 3 个不同账号达成阈值的那一刻）")

	fields := entries[0].ContextMap()

	// distinct_accounts：达成阈值时的不同账号数。
	// 注：zap.Int 的字段在 ContextMap() 中落为 int64（zapcore 的 Int 走 AddInt64），
	// 故此处断言 int64 而非 int。
	require.Contains(t, fields, "distinct_accounts")
	require.Equal(t, int64(3), fields["distinct_accounts"])

	// cooldown：判定为集体抽风时的停调时长。
	require.Contains(t, fields, "cooldown")
	cooldown, ok := fields["cooldown"].(time.Duration)
	require.True(t, ok, "cooldown 字段必须是 time.Duration，实际类型 %T", fields["cooldown"])
	require.Equal(t, service.CursorUpstreamFlapCooldown, cooldown)

	// account_id：达成阈值的那一刻正在失败的那个账号。
	require.Contains(t, fields, "account_id")
	require.Equal(t, int64(103), fields["account_id"])

	// failed_account_ids：本请求内已失败的不同账号集合（map 迭代顺序随机，
	// 故用 ElementsMatch 而非 Equal）。
	require.Contains(t, fields, "failed_account_ids")
	failedIDs, ok := fields["failed_account_ids"].([]interface{})
	require.True(t, ok, "failed_account_ids 字段必须是数组，实际类型 %T", fields["failed_account_ids"])
	require.ElementsMatch(t, []interface{}{int64(101), int64(102), int64(103)}, failedIDs)
	for _, id := range failedIDs {
		require.IsType(t, int64(0), id, "failed_account_ids 的元素必须是 int64")
	}

	// upstream_code：上游传输错误码原样带出。
	require.Contains(t, fields, "upstream_code")
	require.Equal(t, flapLogTestUpstreamCode, fields["upstream_code"])

	// upstream_message：上游错误原文摘要原样带出。
	require.Contains(t, fields, "upstream_message")
	require.Equal(t, flapLogTestUpstreamMessage, fields["upstream_message"])
}

// TestCursorUpstreamFlapLogNotFiredForRepeatedSameAccount 同一账号连续失败 3 次：
// distinct 计数为 1、永远达不到阈值，因此不得打抽风判定日志，也不得置 flap 标记。
func TestCursorUpstreamFlapLogNotFiredForRepeatedSameAccount(t *testing.T) {
	ctx, logs := newCursorUpstreamFlapLogContext()
	fs := NewFailoverState(false)
	unscheduler := &mockTempUnscheduler{}

	for i := 0; i < 3; i++ {
		act := fs.HandleFailoverError(ctx, unscheduler, 201, service.PlatformCursor, 0,
			newCursorUpstreamFlapFailureErr(flapLogTestUpstreamCode, flapLogTestUpstreamMessage))
		require.Equal(t, FailoverContinue, act)
	}

	require.Len(t, logs.FilterMessage(flapLogTestEventName).All(), 0,
		"同一账号重复失败不构成集体抽风，不得打抽风判定日志")
	require.Len(t, unscheduler.calls, 3)
	for i, call := range unscheduler.calls {
		require.False(t, call.flapDetected,
			"同一账号重复失败不重复计数（第 %d 次仍不得判定为集体抽风）", i+1)
	}
	require.Len(t, fs.CursorUpstreamFailedAccountIDs, 1, "失败账号表按 distinct 账号去重")
}

// TestCursorUpstreamFlapLogKeepsFieldsWhenUpstreamCodeEmpty 钉死「诊断字段恒在」：
// 上游没带错误码/原文（空串）时，日志仍必须带 upstream_code / upstream_message 两个
// 键，只是值为空串。若将来实现改成「空值不落字段」，本用例会红。
func TestCursorUpstreamFlapLogKeepsFieldsWhenUpstreamCodeEmpty(t *testing.T) {
	ctx, logs := newCursorUpstreamFlapLogContext()
	fs := NewFailoverState(false)
	unscheduler := &mockTempUnscheduler{}

	for _, accountID := range []int64{301, 302, 303} {
		fs.HandleFailoverError(ctx, unscheduler, accountID, service.PlatformCursor, 0,
			newCursorUpstreamFlapFailureErr("", ""))
	}

	entries := logs.FilterMessage(flapLogTestEventName).All()
	require.Len(t, entries, 1)

	fields := entries[0].ContextMap()
	require.Contains(t, fields, "upstream_code")
	require.Equal(t, "", fields["upstream_code"])
	require.Contains(t, fields, "upstream_message")
	require.Equal(t, "", fields["upstream_message"])
}
