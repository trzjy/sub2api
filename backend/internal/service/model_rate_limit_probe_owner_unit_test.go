//go:build unit

// B2-D11a：ModelProbeReasonExcluded 的定向单测。
// 锁定显式的 reason/scope 所有权排除：404/401/plan-gated、tokenharbor、Cline 钱包
// 与无需余额探测的生图冷却跳过通用模型探针；普通模型和生图不足额仍进入对应探针。
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
		{"image-insufficient-balance-is-owned", openAIImageGenerationRateLimitKey, openAIImagesBalanceRateLimitReason, false},
		{"image-rate-limit-uses-natural-reset", openAIImageGenerationRateLimitKey, openAIImageRateLimitReason, true},
		{"cline-pass-wallet-is-owned-by-quota-service", clinePassRateLimitKey, clinePassLimitReason, true},
		{"cline-credits-wallet-is-owned-by-quota-service", clineCreditsRateLimitKey, clineCreditsLowReason, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, ModelProbeReasonExcluded(tc.scope, tc.reason))
		})
	}
}
