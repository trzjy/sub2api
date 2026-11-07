//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTierRoutingEstimatedInputTokensReadsContext 覆盖 KC-19B 只读读取口：
//   - 未装门返回 0（不返回 -1 或任何哨兵）；
//   - WithEstimatedInputTokens 装入后可读回原值；
//   - 与 WithTierRouting 共用同一 tierRoutingContextKey，彼此不互相覆盖
//     class / estimated：分档门之上叠加估算值时，class 仍生效、estimated 被更新。
func TestTierRoutingEstimatedInputTokensReadsContext(t *testing.T) {
	// 1) 无值：返回 0。
	require.Equal(t, 0, TierRoutingEstimatedInputTokens(context.Background()))

	// 2) 仅装估算值（cursor 反代路径，空 class）。
	ctx := WithEstimatedInputTokens(context.Background(), 12345)
	require.Equal(t, 12345, TierRoutingEstimatedInputTokens(ctx), "装好的估算值必须可读回")
	require.Equal(t, "", tierRoutingPoolFromContext(ctx), "只装估算值不装分档门（class 空 → 池为空）")

	// 3) 既有分档门之上叠加估算值，class 不丢、estimated 更新。
	withClass := WithTierRouting(context.Background(), TierRoutingBig, 999)
	require.NotEqual(t, "", tierRoutingPoolFromContext(withClass), "分档门已装（池非空）")
	estOnClass := WithEstimatedInputTokens(withClass, 4321)
	require.Equal(t, 4321, TierRoutingEstimatedInputTokens(estOnClass), "估算值被更新")
	require.NotEqual(t, "", tierRoutingPoolFromContext(estOnClass), "叠加后分档门 class 仍生效")
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
