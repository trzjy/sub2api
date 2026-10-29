//go:build unit

package service

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// firstByteForbiddenSubstrings 是客户端 isCancelledLikeErrorMessage 的禁用词表
// （方案文档 §1.4，大小写不敏感）。写全，不得遗漏任何一项。
var firstByteForbiddenSubstrings = []string{
	"abort",
	"aborted",
	"aborterror",
	"cancel",
	"cancelled",
	"canceled",
	"operation was aborted",
	"empty response output from model",
}

// assertNoForbiddenSubstring 断言 s 不含任何禁用子串（大小写不敏感）。
func assertNoForbiddenSubstring(t *testing.T, label, s string) {
	t.Helper()
	lower := strings.ToLower(s)
	for _, w := range firstByteForbiddenSubstrings {
		require.False(t, strings.Contains(lower, w),
			"[%s] 文案不得包含禁用子串 %q（违反客户端类取消判定规避约束）", label, w)
	}
}

// (a) 新的流内文案常量与类型常量确实不含上述任何禁用子串。
func TestUpstreamFirstByteTimeoutConstantsNoForbiddenSubstrings(t *testing.T) {
	assertNoForbiddenSubstring(t, "UpstreamFirstByteTimeoutType", UpstreamFirstByteTimeoutType)
	assertNoForbiddenSubstring(t, "UpstreamFirstByteTimeoutMessage", UpstreamFirstByteTimeoutMessage)
	assertNoForbiddenSubstring(t, "UpstreamFirstByteTimeoutStreamStartedMessage", UpstreamFirstByteTimeoutStreamStartedMessage)
}

// (b) 客户端可见错误体 JSON 可解析且 type 字段为 upstream_first_byte_timeout。
func TestUpstreamFirstByteTimeoutBodyParsesAndType(t *testing.T) {
	var parsed struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(upstreamFirstByteTimeoutBody, &parsed),
		"客户端可见错误体必须是可解析 JSON")
	require.Equal(t, UpstreamFirstByteTimeoutType, parsed.Error.Type,
		"错误体 type 字段必须为 upstream_first_byte_timeout")
	assertNoForbiddenSubstring(t, "upstreamFirstByteTimeoutBody.message", parsed.Error.Message)
}

// (c) 既有 newUpstreamFirstByteTimeoutError 的行为不变。
func TestUpstreamFirstByteTimeoutErrorSafeToFailover(t *testing.T) {
	err := newUpstreamFirstByteTimeoutError(30 * time.Second)
	require.NotNil(t, err)
	require.True(t, err.SafeToFailoverAfterWrite, "guard timeout error must be SafeToFailoverAfterWrite=true")
	require.Equal(t, http.StatusGatewayTimeout, err.StatusCode, "guard timeout maps to 504 Gateway Timeout")
	require.NotEmpty(t, err.ResponseBody)
}

func TestUpstreamFirstByteTimeoutErrorRetryBranch(t *testing.T) {
	// 剩余 ≥ 5s → ShouldRetryNextAccount=true（换号）。
	high := newUpstreamFirstByteTimeoutError(5 * time.Second)
	require.True(t, high.ShouldRetryNextAccount(), "remaining >= 5s must allow next-account retry")
	require.Equal(t, NextAccountRetry, high.NextAccountAction)

	// 剩余恰好 5s（边界，≥ 阈值）→ 仍换号。
	boundary := newUpstreamFirstByteTimeoutError(5 * time.Second)
	require.True(t, boundary.ShouldRetryNextAccount(), "remaining == 5s is the inclusive threshold for retry")

	// 剩余 < 5s → ShouldRetryNextAccount=false（耗尽，既有的 FailoverExhausted 消费）。
	low := newUpstreamFirstByteTimeoutError(4*time.Second + 999*time.Millisecond)
	require.False(t, low.ShouldRetryNextAccount(), "remaining < 5s must NOT retry (exhausted)")
	require.Equal(t, NextAccountStop, low.NextAccountAction)

	// 剩余 0 → 耗尽。
	zero := newUpstreamFirstByteTimeoutError(0)
	require.False(t, zero.ShouldRetryNextAccount(), "remaining 0 must be exhausted")
}

// (d) 派发单 G1-a：newUpstreamFirstByteTimeoutError 产出的错误
// Reason 必须为 GatewayFirstByteTimeoutReason（语义标记，替代错误体字符串匹配）。
func TestNewUpstreamFirstByteTimeoutErrorReasonSet(t *testing.T) {
	// 覆盖换号分支（剩余充足）与耗尽分支（剩余不足）两种构造。
	cases := []time.Duration{
		30 * time.Second,
		5 * time.Second,
		4*time.Second + 999*time.Millisecond,
		0,
	}
	for _, remaining := range cases {
		err := newUpstreamFirstByteTimeoutError(remaining)
		require.NotNil(t, err)
		require.Equal(t, GatewayFirstByteTimeoutReason, err.Reason,
			"newUpstreamFirstByteTimeoutError(remaining=%s) 的 Reason 必须为 GatewayFirstByteTimeoutReason", remaining)
	}
}

// (d) 派发单 G1-a：Reason 常量的字符串值必须恰为 "gateway_first_byte_timeout"，
// 且该常量与 G1-b / G1-c 的依赖逐字一致（名字与值都不得改动）。
func TestGatewayFirstByteTimeoutReasonValue(t *testing.T) {
	require.Equal(t, GatewayFailureReason("gateway_first_byte_timeout"), GatewayFirstByteTimeoutReason,
		"GatewayFirstByteTimeoutReason 的字符串值必须恰为 gateway_first_byte_timeout")
	require.Equal(t, "gateway_first_byte_timeout", string(GatewayFirstByteTimeoutReason),
		"GatewayFirstByteTimeoutReason 必须可原样比对为 gateway_first_byte_timeout")
}

// (c) 显式断言派发单要求的四个行为点（remaining 边界 / StatusCode / SafeToFailover）。
func TestNewUpstreamFirstByteTimeoutErrorBehaviorUnchanged(t *testing.T) {
	// 剩余 >= 最小可行窗口(5s) -> NextAccountRetry。
	retry := newUpstreamFirstByteTimeoutError(5 * time.Second)
	require.Equal(t, NextAccountRetry, retry.NextAccountAction,
		"remaining >= 5s 必须 NextAccountRetry")
	require.Equal(t, http.StatusGatewayTimeout, retry.StatusCode, "StatusCode 必须 504")
	require.True(t, retry.SafeToFailoverAfterWrite, "SafeToFailoverAfterWrite 必须 true")

	retryMore := newUpstreamFirstByteTimeoutError(10 * time.Second)
	require.Equal(t, NextAccountRetry, retryMore.NextAccountAction,
		"remaining > 5s 必须 NextAccountRetry")

	// 剩余 < 5s -> NextAccountStop（耗尽）。
	stop := newUpstreamFirstByteTimeoutError(4 * time.Second)
	require.Equal(t, NextAccountStop, stop.NextAccountAction,
		"remaining < 5s 必须 NextAccountStop")
	require.Equal(t, http.StatusGatewayTimeout, stop.StatusCode, "StatusCode 必须 504")
	require.True(t, stop.SafeToFailoverAfterWrite, "SafeToFailoverAfterWrite 必须 true")
}
