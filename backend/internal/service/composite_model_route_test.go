//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNormalizeCompositeRouteEndpointPreservesResponsesWS 钉住 responses_ws 在
// normalize 阶段原样保留（不被归并成 any），使 A3/A1 资格判定与 WS 决策拿到
// 真实执行端点；同时保证既有端点与未知端点的归并行为不变。
func TestNormalizeCompositeRouteEndpointPreservesResponsesWS(t *testing.T) {
	// responses_ws 必须原样到达 A3/A1，不得被归并为 any。
	require.Equal(t, CompositeEndpointResponsesWS, normalizeCompositeRouteEndpoint(CompositeEndpointResponsesWS))

	// 大小写与首尾空白归一化后仍应保留 responses_ws。
	require.Equal(t, CompositeEndpointResponsesWS, normalizeCompositeRouteEndpoint("  RESPONSES_WS \t"))

	// 既有端点仍原样保留。
	require.Equal(t, CompositeRouteEndpointMessages, normalizeCompositeRouteEndpoint(CompositeRouteEndpointMessages))
	require.Equal(t, CompositeRouteEndpointResponses, normalizeCompositeRouteEndpoint(CompositeRouteEndpointResponses))

	// 未知端点仍归并为 any。
	require.Equal(t, CompositeRouteEndpointAny, normalizeCompositeRouteEndpoint("totally-unknown-endpoint"))
}
