//go:build unit

// B2-D11a：ModelProbeReasonExcluded 的定向单测。
// 锁定"仅凭 reason 即可排除"的判据：三个 404/401/plan-gated reason 与
// tokenharbor 前缀 → true；空 reason、普通模型、生图 scope + 非排除 reason → false。
package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestModelProbeReasonExcluded(t *testing.T) {
	const modelScope = "m-model"

	cases := []struct {
		name   string
		scope  string
		reason string
		want   bool
	}{
		{"upstream-404", modelScope, upstreamModelNotFoundReason, true},
		{"upstream-401", modelScope, upstreamModelNotFound401Reason, true},
		{"codex-plan-gated", modelScope, upstreamCodexPlanGatedModelReason, true},
		{"tokenharbor-prefix", modelScope, tokenHarborFreeTierReasonPrefix + ":x", true},
		{"empty-reason", modelScope, "", false},
		{"normal-model", modelScope, "rate_limit", false},
		{"image-scope-normal-reason", openAIImageGenerationRateLimitKey, "rate_limit", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, ModelProbeReasonExcluded(tc.scope, tc.reason))
		})
	}
}
