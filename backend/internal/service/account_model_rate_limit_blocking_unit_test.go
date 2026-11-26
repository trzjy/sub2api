//go:build unit

// B2-A1：模型限流准入谓词、归属判据与探测排程口径的定向单测。
// 验证 isRateLimitBlockingForKey 的「reset 未到期 OR probe_due_at 非空」双分支、
// isModelProbeOwned 的归属过滤，以及 ModelProbeDue 的四口径排程。
package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestModelRateLimitBlockingForKey(t *testing.T) {
	now := time.Now()

	acc := &Account{
		Extra: map[string]any{
			modelRateLimitsKey: map[string]any{
				"m-reset-active": map[string]any{
					"rate_limit_reset_at": now.Add(time.Hour).Format(time.RFC3339),
				},
				"m-probe-due": map[string]any{
					"rate_limit_reset_at":     now.Add(-time.Hour).Format(time.RFC3339),
					modelRateLimitProbeDueKey: now.Add(time.Minute).Format(time.RFC3339),
				},
				"m-expired-no-probe": map[string]any{
					"rate_limit_reset_at": now.Add(-time.Hour).Format(time.RFC3339),
				},
				"m-expired-empty-probe": map[string]any{
					"rate_limit_reset_at":     now.Add(-time.Hour).Format(time.RFC3339),
					modelRateLimitProbeDueKey: "  ",
				},
			},
		},
	}

	// reset 未到期 → 阻断
	require.True(t, acc.isRateLimitBlockingForKey("m-reset-active"))
	// reset 已到期但仍有 probe_due_at → 仍阻断
	require.True(t, acc.isRateLimitBlockingForKey("m-probe-due"))
	// reset 已到期且无 probe → 放行
	require.False(t, acc.isRateLimitBlockingForKey("m-expired-no-probe"))
	// probe_due_at 空白视为空 → 放行
	require.False(t, acc.isRateLimitBlockingForKey("m-expired-empty-probe"))
	// 不存在的 scope → 放行
	require.False(t, acc.isRateLimitBlockingForKey("m-missing"))

	// isRateLimitActiveForKey 保持原义：只看 reset_at，到期即为 false（不受 probe 影响）。
	require.True(t, acc.isRateLimitActiveForKey("m-reset-active"))
	require.False(t, acc.isRateLimitActiveForKey("m-probe-due"))

	// modelRateLimitEntry 取条目 / 不存在返回 nil。
	require.NotNil(t, acc.modelRateLimitEntry("m-probe-due"))
	require.Nil(t, acc.modelRateLimitEntry("m-missing"))
}

func TestModelProbeOwned(t *testing.T) {
	withBalanceProbe := &Account{Credentials: map[string]any{
		BalanceProbeConfigCredentialKey: map[string]any{"enabled": true},
	}, Platform: PlatformOpenAI}
	withoutBalanceProbe := &Account{}

	cases := []struct {
		name    string
		scope   string
		reason  string
		account *Account
		want    bool
	}{
		{"upstream-404", "m-model", upstreamModelNotFoundReason, withoutBalanceProbe, false},
		{"upstream-401", "m-model", upstreamModelNotFound401Reason, withoutBalanceProbe, false},
		{"codex-plan-gated", "m-model", upstreamCodexPlanGatedModelReason, withoutBalanceProbe, false},
		{"tokenharbor-prefix", "m-model", tokenHarborFreeTierReasonPrefix + ":x", withoutBalanceProbe, false},
		{"image-insufficient-balance-without-balance-probe", openAIImageGenerationRateLimitKey, openAIImagesBalanceRateLimitReason, withoutBalanceProbe, false},
		{"image-insufficient-balance-nil-account", openAIImageGenerationRateLimitKey, openAIImagesBalanceRateLimitReason, nil, false},
		{"image-insufficient-balance-with-balance-probe", openAIImageGenerationRateLimitKey, openAIImagesBalanceRateLimitReason, withBalanceProbe, true},
		{"image-rate-limit-not-owned-by-balance-probe", openAIImageGenerationRateLimitKey, openAIImageRateLimitReason, withBalanceProbe, false},
		{"image-capability-loss-not-owned-by-balance-probe", openAIImageGenerationRateLimitKey, openAIImageCapabilityLossReason, withBalanceProbe, false},
		{"cline-pass-wallet-not-model-probe", clinePassRateLimitKey, clinePassLimitReason, &Account{Platform: PlatformCline}, false},
		{"cline-credits-wallet-not-model-probe", clineCreditsRateLimitKey, clineCreditsLowReason, &Account{Platform: PlatformCline}, false},
		{"normal-model", "m-model", "rate_limit", withoutBalanceProbe, true},
		{"normal-model-nil-reason", "m-model", "", withoutBalanceProbe, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry := map[string]any{"reason": tc.reason}
			require.Equal(t, tc.want, isModelProbeOwned(tc.scope, entry, tc.account))
		})
	}
}

func TestModelProbeDue(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	resetAt := now.Add(37 * time.Minute)

	quotaKeyID := int64(42)
	zeroQuotaKeyID := int64(0)

	type accCase struct {
		name string
		acc  *Account
	}
	accs := []accCase{
		{"balance-probe-enabled", &Account{Credentials: map[string]any{
			BalanceProbeConfigCredentialKey: map[string]any{"enabled": true},
		}}},
		{"antigravity", &Account{Platform: PlatformAntigravity}},
		{"cursor-with-quota-key", &Account{Platform: PlatformCursor, QuotaKeyID: &quotaKeyID}},
	}

	for _, ac := range accs {
		t.Run(ac.name+"/precise", func(t *testing.T) {
			ok, due := ModelProbeDue(ac.acc, "m-model", "rate_limit", resetAt, true, now)
			require.True(t, ok)
			require.Equal(t, resetAt, due)
		})
		t.Run(ac.name+"/no-precise", func(t *testing.T) {
			ok, due := ModelProbeDue(ac.acc, "m-model", "rate_limit", resetAt, false, now)
			require.True(t, ok)
			require.Equal(t, now.Add(5*time.Minute), due)
		})
	}

	// 无额度入口：precise → resetAt；无 precise → now+30s。
	noQuotaAccs := []accCase{
		{"plain", &Account{Platform: "openai"}},
		{"cursor-zero-quota-key", &Account{Platform: PlatformCursor, QuotaKeyID: &zeroQuotaKeyID}},
		{"cursor-nil-quota-key", &Account{Platform: PlatformCursor}},
	}
	for _, ac := range noQuotaAccs {
		t.Run(ac.name+"/precise", func(t *testing.T) {
			ok, due := ModelProbeDue(ac.acc, "m-model", "rate_limit", resetAt, true, now)
			require.True(t, ok)
			require.Equal(t, resetAt, due)
		})
		t.Run(ac.name+"/no-precise", func(t *testing.T) {
			ok, due := ModelProbeDue(ac.acc, "m-model", "rate_limit", resetAt, false, now)
			require.True(t, ok)
			require.Equal(t, now.Add(30*time.Second), due)
		})
	}

	// 非本探测链负责 / 无账号 → 不排程，零值时刻。
	ok, due := ModelProbeDue(nil, "m-model", "rate_limit", resetAt, false, now)
	require.False(t, ok)
	require.True(t, due.IsZero())

	ok, due = ModelProbeDue(accs[0].acc, "m-model", upstreamModelNotFoundReason, resetAt, false, now)
	require.False(t, ok)
	require.True(t, due.IsZero())

	// 图片专用 scope 只排程可由余额接口验证的不足额状态；其它图片冷却按 reset 到期。
	ok, due = ModelProbeDue(accs[0].acc, openAIImageGenerationRateLimitKey, openAIImageRateLimitReason, resetAt, false, now)
	require.False(t, ok)
	require.True(t, due.IsZero())

	ok, due = ModelProbeDue(&Account{Platform: PlatformOpenAI, Credentials: map[string]any{
		BalanceProbeConfigCredentialKey: map[string]any{"enabled": true},
	}}, openAIImageGenerationRateLimitKey, openAIImagesBalanceRateLimitReason, resetAt, false, now)
	require.True(t, ok)
	require.Equal(t, now.Add(5*time.Minute), due)

	// Cline 钱包冷却由周期额度服务写入/解除，不进入通用模型探针。
	ok, due = ModelProbeDue(&Account{Platform: PlatformCline}, clinePassRateLimitKey, clinePassLimitReason, resetAt, false, now)
	require.False(t, ok)
	require.True(t, due.IsZero())
}
