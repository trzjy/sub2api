//go:build unit

package errors

import (
	"strings"
	"testing"
	"unicode"

	"github.com/stretchr/testify/require"
)

// TestDownstreamMessages_ForbiddenSubstrings_M1e 断言 M1-e 新增的"上游首字节超时"
// 客户端可见文案常量不含任何触发客户端 isCancelledLikeErrorMessage 的禁用子串，
// 且不含 "empty response output from model" 类短语。
func TestDownstreamMessages_ForbiddenSubstrings_M1e(t *testing.T) {
	// M1-e 本卡新增的文案常量。
	newMessages := map[string]string{
		"UpstreamFirstByteTimeoutJSON":        UpstreamFirstByteTimeoutJSON,
		"UpstreamFirstByteTimeoutStreamFrame": UpstreamFirstByteTimeoutStreamFrame,
	}

	// 禁用子串表（大小写不敏感），源自客户端 isCancelledLikeErrorMessage 规则。
	forbidden := []string{
		"abort",
		"aborted",
		"aborterror",
		"cancel",
		"cancelled",
		"canceled",
		"operation was aborted",
		"empty response output from model",
	}

	for name, msg := range newMessages {
		lower := strings.ToLower(msg)
		for _, f := range forbidden {
			require.Falsef(t, strings.Contains(lower, f),
				"新增文案常量 %s 命中禁用子串 %q：%q", name, f, msg)
		}
	}
}

// TestDownstreamMessages_Format_M1e 断言 M1-e 新增文案：非空、含中文（至少一中文字符）、
// 长度合理（UTF-8 编码不超过 200 字节），且面向终端用户可读。
func TestDownstreamMessages_Format_M1e(t *testing.T) {
	newMessages := map[string]string{
		"UpstreamFirstByteTimeoutJSON":        UpstreamFirstByteTimeoutJSON,
		"UpstreamFirstByteTimeoutStreamFrame": UpstreamFirstByteTimeoutStreamFrame,
	}

	for name, msg := range newMessages {
		require.NotEmptyf(t, msg, "新增文案常量 %s 不应为空", name)

		// 至少含一个中文字符（CJK 统一表意文字区）。
		hasHan := false
		for _, r := range msg {
			if unicode.Is(unicode.Scripts["Han"], r) {
				hasHan = true
				break
			}
		}
		require.Truef(t, hasHan, "新增文案常量 %s 应含至少一个中文字符：%q", name, msg)

		require.LessOrEqualf(t, len(msg), 200,
			"新增文案常量 %s 长度不应超过 200 字节：%q（%d 字节）", name, msg, len(msg))
	}
}

// TestDownstreamMessages_FirstByteFamily_NoForbiddenSubstrings 兜底：对既有首字节超时族
// 常量一并核验，确认其不触发客户端类取消判定（本卡不改动既有语义，仅登记核查结果）。
func TestDownstreamMessages_FirstByteFamily_NoForbiddenSubstrings(t *testing.T) {
	family := map[string]string{
		"UpstreamFirstByteTimeout":           UpstreamFirstByteTimeout,
		"UpstreamFirstByteTimeoutAfterStart": UpstreamFirstByteTimeoutAfterStart,
	}
	forbidden := []string{
		"abort", "aborted", "aborterror", "cancel",
		"cancelled", "canceled", "operation was aborted",
	}
	for name, msg := range family {
		lower := strings.ToLower(msg)
		for _, f := range forbidden {
			require.Falsef(t, strings.Contains(lower, f),
				"既有文案常量 %s 命中禁用子串 %q：%q", name, f, msg)
		}
	}
}

// cursorResumeReasonTable 逐字照抄方案 §2.2 续轮（CUR-RES）表的 reason 列。
var cursorResumeReasonTable = map[string][]string{
	"CUR-RES-01": {"unknown_association", "invalid_ticket", "unknown_call_id", "unknown_previous_response_id", "unexpected_result_id", "stale_call_id"},
	"CUR-RES-02": {"duplicate_result_id", "mixed_sessions", "result_type_mismatch", "missing_results"},
	"CUR-RES-03": {"session_finished", "not_waiting_tools", "no_pending_batch", "no_delivered_batch", "stale_batch", "stale_previous_response_id", "batch_not_delivered"},
	"CUR-RES-04": {"model_changed", "model_mapping_changed", "tool_set_changed", "config_changed", "instructions_withdrawn", "history_mismatch", "protocol_mismatch"},
	"CUR-RES-05": {"already_consumed", "session_busy"},
	"CUR-RES-06": {"undecodable_continuation", "unknown_protocol"},
	"CUR-RES-07": {"tool_choice_invalid", "tool_choice_required_without_tools", "tool_choice_undeclared"},
	"CUR-RES-08": {"account_unavailable", "account_changed", "source_changed", "source_fingerprint_unavailable", "platform_not_admitted", "group_not_allowed",
		"not_schedulable", "model_rate_limited", "quota_key_source_unbound", "quota_key_expired", "quota_key_state_unavailable",
		"quota_key_usage_unobserved", "quota_key_usage_observation_failed", "quota_key_usage_stale", "quota_key_usage_incomplete", "quota_key_usage_exhausted"},
	"CUR-RES-09": {"identity_mismatch"},
	"CUR-RES-10": {"execution_unavailable", "intake_unavailable", "source_unavailable"},
}

// cursorCodeMessages 列出 Cursor 段全部码与中文常量对。
var cursorCodeMessages = [][2]string{
	{CursorResUnknownAssociationCode, CursorResUnknownAssociation},
	{CursorResInvalidResultsCode, CursorResInvalidResults},
	{CursorResSessionEndedCode, CursorResSessionEnded},
	{CursorResRequestChangedCode, CursorResRequestChanged},
	{CursorResAlreadySubmittedCode, CursorResAlreadySubmitted},
	{CursorResUndecodableCode, CursorResUndecodable},
	{CursorResToolChoiceInvalidCode, CursorResToolChoiceInvalid},
	{CursorResAccountUnavailableCode, CursorResAccountUnavailable},
	{CursorResIdentityMismatchCode, CursorResIdentityMismatch},
	{CursorResServiceUnavailableCode, CursorResServiceUnavailable},
	{CursorResUnknownCode, CursorResUnknown},
	{CursorExecDeadlineExceededCode, CursorExecDeadlineExceeded},
	{CursorExecUnsupportedCode, CursorExecUnsupported},
	{CursorExecUpstreamProtocolCode, CursorExecUpstreamProtocol},
	{CursorExecBatchIncompleteCode, CursorExecBatchIncomplete},
	{CursorExecResultWriteFailedCode, CursorExecResultWriteFailed},
	{CursorExecUpstreamAuthFailedCode, CursorExecUpstreamAuthFailed},
	{CursorReqMissingModelCode, CursorReqMissingModel},
	{CursorReqInvalidImageCode, CursorReqInvalidImage},
	{CursorReqImageOutsideUserCode, CursorReqImageOutsideUser},
	{CursorReqUnsupportedMessageCode, CursorReqUnsupportedMessage},
	{CursorReqInvalidToolSchemaCode, CursorReqInvalidToolSchema},
	{CursorExecModelMappingInvalidCode, CursorExecModelMappingInvalid},
	{CursorExecInternalErrorCode, CursorExecInternalError},
	{CursorExecModelUnavailableCode, CursorExecModelUnavailable},
}

func TestCursorResumeReasonCode_PlanTable(t *testing.T) {
	total := 0
	for wantCode, reasons := range cursorResumeReasonTable {
		for _, reason := range reasons {
			total++
			code, msg := CursorResumeReasonCode(reason)
			require.Equalf(t, wantCode, code, "reason %q", reason)
			require.NotEqualf(t, CursorResUnknownCode, code, "reason %q 不应落入兜底码", reason)
			require.NotEmptyf(t, msg, "reason %q", reason)
		}
	}
	require.Equal(t, 51, total)
}

func TestCursorResumeReasonCode_Fallback(t *testing.T) {
	for _, reason := range []string{"resume_failed", "", "something_new"} {
		code, msg := CursorResumeReasonCode(reason)
		require.Equalf(t, "CUR-RES-99", code, "reason %q", reason)
		require.Equal(t, CursorResUnknown, msg)
	}
}

func TestCursorUserMessage_Format(t *testing.T) {
	require.Equal(t,
		"服务暂时不可用，请稍后重试。（错误码 CUR-RES-10，请求号 req-123）",
		CursorUserMessage(CursorResServiceUnavailableCode, CursorResServiceUnavailable, "  req-123 "))
	require.Equal(t,
		"服务暂时不可用，请稍后重试。（错误码 CUR-RES-10，请求号 未知）",
		CursorUserMessage(CursorResServiceUnavailableCode, CursorResServiceUnavailable, "   "))
	require.Equal(t,
		"服务暂时不可用。（错误码 CUR-RES-10，请求号 req-1）",
		CursorUserMessage(CursorResServiceUnavailableCode, "服务暂时不可用。", "req-1"))
}

func TestCursorCodeMessages_UniqueAndClean(t *testing.T) {
	var reasons []string
	for _, rs := range cursorResumeReasonTable {
		reasons = append(reasons, rs...)
	}
	reasons = append(reasons, "resume_failed")

	seenCode := map[string]bool{}
	for _, pair := range cursorCodeMessages {
		code, msg := pair[0], pair[1]
		require.Falsef(t, seenCode[code], "错误码重复：%s", code)
		seenCode[code] = true
		require.Truef(t, strings.HasPrefix(code, "CUR-RES-") || strings.HasPrefix(code, "CUR-EXEC-") || strings.HasPrefix(code, "CUR-REQ-"), "错误码格式：%s", code)
		require.NotContainsf(t, msg, "{", "%s 文案含占位符：%q", code, msg)
		for _, reason := range reasons {
			require.NotContainsf(t, msg, reason, "%s 文案含 reason 原文 %q", code, reason)
		}
	}
	require.Len(t, seenCode, 25)
}
