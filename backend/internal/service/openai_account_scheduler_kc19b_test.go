//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTierRoutingEstimatedInputTokensReadsContext 覆盖 KC-19B 估算透传只读口：
//
//   - 未装估算值时返回 0（不返回 -1 或任何哨兵）；
//   - WithEstimatedInputTokens 装入后可读回原值；
//   - 在已有估算值之上再次装入可覆盖更新，装 0 即清回默认值。
//
// 全程基于 context.Background() 组织，不依赖任何已下线的分档门。
func TestTierRoutingEstimatedInputTokensReadsContext(t *testing.T) {
	// 1) 无值：返回 0。
	require.Equal(t, 0, TierRoutingEstimatedInputTokens(context.Background()))

	// 2) 装入估算值后可读回原值。
	ctx := WithEstimatedInputTokens(context.Background(), 12345)
	require.Equal(t, 12345, TierRoutingEstimatedInputTokens(ctx), "装好的估算值必须可读回")

	// 3) 在已有估算值之上再次装入，覆盖更新；装 0 即清回默认值。
	updated := WithEstimatedInputTokens(ctx, 4321)
	require.Equal(t, 4321, TierRoutingEstimatedInputTokens(updated), "估算值被覆盖更新")
	cleared := WithEstimatedInputTokens(updated, 0)
	require.Equal(t, 0, TierRoutingEstimatedInputTokens(cleared), "装 0 即清回默认值")
}

// TestWithEstimatedInputTokensNilSafeAndZeroable 覆盖边界：
//   - nil ctx 不 panic、返回 nil；
//   - 0 值可显式装回（等价于清估算）。
func TestWithEstimatedInputTokensNilSafeAndZeroable(t *testing.T) {
	require.Nil(t, WithEstimatedInputTokens(nil, 100), "nil ctx 原样返回 nil")

	ctx := WithEstimatedInputTokens(context.Background(), 100)
	require.Equal(t, 100, TierRoutingEstimatedInputTokens(ctx))
	cleared := WithEstimatedInputTokens(ctx, 0)
	require.Equal(t, 0, TierRoutingEstimatedInputTokens(cleared), "装 0 即清估算值")
}
