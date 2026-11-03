//go:build unit

package service

// 流内「今日额度已用完」模板（CursorQuotaExhaustedError / ErrCursorQuotaExhaustedInStream）
// 单测。来源：2026-11-03 生产活体——上游以普通 TextDelta 下发当日额度耗尽文案（含卡 ID），
// EndStream 无错误 code，旧判定完全漏识别 → 账号不停调度、请求不换号、原文透传客户端。
//
// 修复语义：与 EndStream 的 ERROR_PRO_USER_RATE_LIMIT_EXCEEDED（RL-STOP）同级——
// 账号级终态：停号回调 + 未发布字节时替换为可换号 failover；已发布字节时保留原错误
// （禁止 mid-stream 换号），但停号回调仍必须调用，且用户文案绝不含卡 ID。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 流内额度耗尽活体样本（2026-11-03，Claude Code 客户端原样收到的完整文案）。
const cursorQuotaExhaustedLiveSample = "今日额度已用完，每日早上8点重置" +
	"（时长卡每日用量有上限，适合每日均匀使用；如短期使用强度较高可另外购买定量卡进行补充，无每日额度限制）" +
	"（1793676349583-clard21b）"

// TestCursorMatchQuotaExhaustedTemplate 覆盖模板匹配：活体样本命中新哨兵，
// 片段与轮次标识原样留存；且不得误归为拒付（拒付与额度耗尽都停号，但文案/处置不同族）。
func TestCursorMatchQuotaExhaustedTemplate(t *testing.T) {
	err := cursorMatchKeyRejected(cursorQuotaExhaustedLiveSample)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCursorQuotaExhaustedInStream)
	require.False(t, errors.Is(err, ErrCursorKeyRejectedUpstream), "额度耗尽不得归为拒付")

	var qe *CursorQuotaExhaustedError
	require.ErrorAs(t, err, &qe)
	require.Equal(t, cursorQuotaExhaustedLiveSample, qe.Template)
	require.Equal(t, "1793676349583-clard21b", qe.TurnID)

	// 无动态后缀同样识别。
	err = cursorMatchKeyRejected("今日额度已用完，每日早上8点重置")
	require.ErrorIs(t, err, ErrCursorQuotaExhaustedInStream)
	require.Empty(t, qe.TurnID[:0], "sanity")

	// 普通正文不得误伤。
	require.NoError(t, cursorMatchKeyRejected("今天的额度还剩很多，放心用"))
}

// TestCursorParseStreamQuotaExhaustedFailsClosed 覆盖解析层：正常帧序列（text_delta +
// turn_ended）中出现额度耗尽模板 → 终态错误，绝不当成功、绝不产 SSE。
func TestCursorParseStreamQuotaExhaustedFailsClosed(t *testing.T) {
	var stream []byte
	stream = append(stream, cursorTextDeltaFrame(t, cursorQuotaExhaustedLiveSample)...)
	stream = append(stream, cursorTurnEndedFrame(t,
		[2]uint64{cursorFldTurnEndedEndedAtMS, 1793676349714})...)

	p := cursorNewStreamParser("m")
	lines, err := p.cursorParseStream(stream)
	require.Error(t, err)
	require.Nil(t, lines, "额度耗尽流不得产出任何 SSE chunk")
	require.ErrorIs(t, err, ErrCursorQuotaExhaustedInStream)
}

// TestCursorParseStreamQuotaExhaustedAcrossDeltas 覆盖模板跨帧到达仍被识别。
func TestCursorParseStreamQuotaExhaustedAcrossDeltas(t *testing.T) {
	var stream []byte
	stream = append(stream, cursorTextDeltaFrame(t, "今日额度已用完，")...)
	stream = append(stream, cursorTextDeltaFrame(t, "每日早上8点重置（1793676349583-clard21b）")...)
	stream = append(stream, cursorTurnEndedFrame(t)...)

	_, err := cursorNewStreamParser("m").cursorParseStream(stream)
	require.ErrorIs(t, err, ErrCursorQuotaExhaustedInStream)
}

// TestCursorExecuteQuotaExhaustedNoBodySwitchesAccount 覆盖执行层 RL-STOP 同款处置 ①：
// 流内模板命中 + 未发布字节 → 停号回调 1 次 + 替换为可换号 failover（429）。
func TestCursorExecuteQuotaExhaustedNoBodySwitchesAccount(t *testing.T) {
	var stream []byte
	stream = append(stream, cursorTextDeltaFrame(t, cursorQuotaExhaustedLiveSample)...)
	stream = append(stream, cursorTurnEndedFrame(t,
		[2]uint64{cursorFldTurnEndedEndedAtMS, 1793676349714})...)
	stream = append(stream, cursorTestEndStream(t, "{}")...)

	svc, c := newCursorRLStopService(t, stream)
	var calls cursorRLStopHandlerCall
	svc.cursorExecution.SetAccountExhaustedHandler(calls.fn)

	acc := cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A")
	res, err := executeCursorForTest(t, svc, context.Background(), c, acc,
		cursorRLStopRequestBody(true), "cursor-sonnet", true, time.Now())
	require.Error(t, err)
	require.Nil(t, res, "额度耗尽未发布字节不得返回可计费结果")

	var fe *UpstreamFailoverError
	require.ErrorAs(t, err, &fe, "额度耗尽未发布字节必须替换为可换号 failover")
	require.Equal(t, 429, fe.StatusCode)
	require.Equal(t, GatewayFailureReason("cursor_quota_exhausted"), fe.Reason)
	require.Equal(t, 429, fe.ClientStatusCode)
	require.Equal(t, GatewayFailureScopeAccount, fe.Scope)
	require.True(t, fe.ShouldRetryNextAccount(), "必须允许换号（NextAccountRetry）")
	require.NotContains(t, fe.ClientMessage, "clard21b", "客户端文案绝不含卡 ID")

	n, aid, reason := calls.snapshot()
	require.Equal(t, 1, n, "停号回调必须恰好调用 1 次")
	require.Equal(t, int64(1), aid)
	require.Equal(t, "cursor upstream account daily quota exhausted (in-stream template)", reason)
}

// TestCursorExecuteQuotaExhaustedWithBodyStopsAccountOnly 覆盖执行层 RL-STOP 同款处置 ②：
// 已发布字节（模板跨帧、前缀已流出）→ 停号回调仍调用 1 次，但禁止 mid-stream 换号，
// 原错误保留（由 CursorUserFacingError 映射为干净的 CUR-EXEC-11 文案）。
func TestCursorExecuteQuotaExhaustedWithBodyStopsAccountOnly(t *testing.T) {
	var stream []byte
	stream = append(stream, cursorTextDeltaFrame(t, "正常开头正文")...)
	stream = append(stream, cursorTextDeltaFrame(t, cursorQuotaExhaustedLiveSample)...)
	stream = append(stream, cursorTurnEndedFrame(t,
		[2]uint64{cursorFldTurnEndedEndedAtMS, 1793676349714})...)
	stream = append(stream, cursorTestEndStream(t, "{}")...)

	svc, c := newCursorRLStopService(t, stream)
	var calls cursorRLStopHandlerCall
	svc.cursorExecution.SetAccountExhaustedHandler(calls.fn)

	acc := cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A")
	res, err := executeCursorForTest(t, svc, context.Background(), c, acc,
		cursorRLStopRequestBody(true), "cursor-sonnet", true, time.Now())
	require.Error(t, err)
	require.Nil(t, res)

	require.ErrorIs(t, err, ErrCursorQuotaExhaustedInStream)
	var fe *UpstreamFailoverError
	require.False(t, errors.As(err, &fe), "已发布字节不得替换成 failover（禁止 mid-stream 换号）")

	n, _, _ := calls.snapshot()
	require.Equal(t, 1, n, "已发布字节时停号回调仍须调用 1 次")

	// 用户面文案：干净的 429 + CUR-EXEC-11，绝不透传卡 ID。
	status, code, msg, ok := CursorUserFacingError(context.Background(), err)
	require.True(t, ok)
	require.Equal(t, 429, status)
	require.Equal(t, "CUR-EXEC-11", code)
	require.NotContains(t, msg, "clard21b", "用户文案绝不含卡 ID")
	require.NotContains(t, msg, "每日早上8点重置", "用户文案绝不透传上游原文")
	require.NotContains(t, msg, "时长卡", "用户文案绝不透传上游原文")
}
