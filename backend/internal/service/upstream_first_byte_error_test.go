//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestUpstreamFirstByteTimeoutErrorSafeToFailover 验证 SafeToFailoverAfterWrite=true。
func TestUpstreamFirstByteTimeoutErrorSafeToFailover(t *testing.T) {
	err := newUpstreamFirstByteTimeoutError(30 * time.Second)
	require.NotNil(t, err)
	require.True(t, err.SafeToFailoverAfterWrite, "guard timeout error must be SafeToFailoverAfterWrite=true")
	require.Equal(t, 504, err.StatusCode, "guard timeout maps to 504 Gateway Timeout")
	require.NotEmpty(t, err.ResponseBody)
}

// TestUpstreamFirstByteTimeoutErrorRetryBranch 验证剩余预算两分支（v5 更正，零新计数器）。
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
