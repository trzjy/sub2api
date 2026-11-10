//go:build unit

package dto

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestUsageLogDTOLargeRequestRerouteMapping 覆盖对账两字段：映射后值必须与 service 层一致。
func TestUsageLogDTOLargeRequestRerouteMapping(t *testing.T) {
	t.Parallel()

	log := &service.UsageLog{
		RequestID:            "req_reroute",
		Model:                "gpt-5.6-sol",
		EstimatedInputTokens: 4321,
		LargeRequestReroute:  true,
	}

	user := UsageLogFromService(log)
	require.NotNil(t, user)
	require.Equal(t, 4321, user.EstimatedInputTokens)
	require.True(t, user.LargeRequestReroute)

	// 管理端导出函数委托同一映射，字段必须同时可见。
	admin := UsageLogFromServiceAdmin(log)
	require.NotNil(t, admin)
	require.Equal(t, 4321, admin.EstimatedInputTokens)
	require.True(t, admin.LargeRequestReroute)
}

// TestUsageLogDTOLargeRequestRerouteOmitEmpty 覆盖零值：omitempty 下两键不得出现在 JSON 中。
func TestUsageLogDTOLargeRequestRerouteOmitEmpty(t *testing.T) {
	t.Parallel()

	user := UsageLogFromService(&service.UsageLog{RequestID: "req_plain", Model: "gpt-5.6-sol"})
	require.NotNil(t, user)
	require.Zero(t, user.EstimatedInputTokens)
	require.False(t, user.LargeRequestReroute)

	payload, err := json.Marshal(user)
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(payload, &decoded))
	require.NotContains(t, decoded, "estimated_input_tokens")
	require.NotContains(t, decoded, "large_request_reroute")
}

// TestUsageLogDTOLargeRequestRerouteNonZeroSerialized 覆盖非零值：字段必须以约定键名出现。
func TestUsageLogDTOLargeRequestRerouteNonZeroSerialized(t *testing.T) {
	t.Parallel()

	user := UsageLogFromService(&service.UsageLog{
		RequestID:            "req_reroute_json",
		Model:                "gpt-5.6-sol",
		EstimatedInputTokens: 99,
		LargeRequestReroute:  true,
	})
	require.NotNil(t, user)

	payload, err := json.Marshal(user)
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(payload, &decoded))
	require.Equal(t, float64(99), decoded["estimated_input_tokens"])
	require.Equal(t, true, decoded["large_request_reroute"])
}
