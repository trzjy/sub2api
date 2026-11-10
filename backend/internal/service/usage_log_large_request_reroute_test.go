//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUsageLogLargeRequestFields 验证 UsageLog 上两个对账字段的零值与可读回性。
func TestUsageLogLargeRequestFields(t *testing.T) {
	var zero UsageLog
	require.Equal(t, 0, zero.EstimatedInputTokens)
	require.False(t, zero.LargeRequestReroute)

	log := UsageLog{
		EstimatedInputTokens: 1234,
		LargeRequestReroute:  true,
	}
	require.Equal(t, 1234, log.EstimatedInputTokens)
	require.True(t, log.LargeRequestReroute)

	// 指针取用后按字段名读回，防止字段被挪到嵌套结构里而不自知。
	ptr := &log
	require.Equal(t, 1234, ptr.EstimatedInputTokens)
	require.True(t, ptr.LargeRequestReroute)
}
