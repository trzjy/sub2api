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

func TestCursorResumeReasonMessage(t *testing.T) {
	knownReasons := []string{
		"unknown_association", "duplicate_result_id", "session_finished", "model_changed",
		"already_consumed", "undecodable_continuation", "tool_choice_invalid",
		"account_unavailable", "identity_mismatch", "execution_unavailable", "account_pool_exhausted",
	}
	for _, reason := range knownReasons {
		require.NotEmptyf(t, CursorResumeReasonMessage(reason), "reason %q", reason)
	}
	for _, reason := range []string{"resume_failed", "", "something_new"} {
		require.Equalf(t, CursorResUnknown, CursorResumeReasonMessage(reason), "reason %q", reason)
	}
}

func TestCursorUserMessage_Format(t *testing.T) {
	require.Equal(t,
		"服务暂时不可用，请稍后重试。（请求号 req-123）",
		CursorUserMessage(CursorResServiceUnavailable, "  req-123 "))
	require.Equal(t,
		"服务暂时不可用，请稍后重试。（请求号 未知）",
		CursorUserMessage(CursorResServiceUnavailable, "   "))
	require.Equal(t,
		"服务暂时不可用。（请求号 req-1）",
		CursorUserMessage("服务暂时不可用。", "req-1"))
	require.NotContains(t, CursorUserMessage(CursorResServiceUnavailable, "req-1"), "CUR-")
}
