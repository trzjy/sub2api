//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestPlatformEligibilityUnifiedCriterion 校验平台资格唯一出口：同平台或任意聚合平台
// （含 devin）具备资格，原生异平台与 nil 不具备。它同时是 isAccountAllowedForPlatform(
// account, platform, true) 的同义判定。
func TestPlatformEligibilityUnifiedCriterion(t *testing.T) {
	tests := []struct {
		name     string
		platform string
		account  *Account
		want     bool
	}{
		{"native same platform", PlatformAnthropic, &Account{Platform: PlatformAnthropic}, true},
		{"native different platform", PlatformAnthropic, &Account{Platform: PlatformGemini}, false},
		{"devin aggregate", PlatformAnthropic, &Account{Platform: PlatformDevin}, true},
		{"cursor aggregate", PlatformAnthropic, &Account{Platform: PlatformCursor}, true},
		{"codebuddy aggregate", PlatformAnthropic, &Account{Platform: PlatformCodeBuddy}, true},
		{"antigravity aggregate", PlatformAnthropic, &Account{Platform: PlatformAntigravity}, true},
		{"other aggregate", PlatformAnthropic, &Account{Platform: PlatformOther}, true},
		{"nil account", PlatformAnthropic, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isAccountPlatformEligible(tt.account, tt.platform))
		})
	}
}

// TestPlatformFilteredForSelectionUnifiedCriterion 校验失败诊断不再把聚合账号一律误记为
// platform_filtered，与调度准入同口径。
func TestPlatformFilteredForSelectionUnifiedCriterion(t *testing.T) {
	t.Run("allow mixed", func(t *testing.T) {
		require.False(t, isPlatformFilteredForSelection(&Account{Platform: PlatformAnthropic}, PlatformAnthropic, true))
		require.False(t, isPlatformFilteredForSelection(&Account{Platform: PlatformDevin}, PlatformAnthropic, true), "devin aggregate must not be platform_filtered")
		require.False(t, isPlatformFilteredForSelection(&Account{Platform: PlatformCursor}, PlatformAnthropic, true), "cursor aggregate must not be platform_filtered")
		require.False(t, isPlatformFilteredForSelection(&Account{Platform: PlatformAntigravity, Extra: map[string]any{"mixed_scheduling": true}}, PlatformAnthropic, true))
		require.True(t, isPlatformFilteredForSelection(&Account{Platform: PlatformGemini}, PlatformAnthropic, true), "different native platform must be filtered")
		require.True(t, isPlatformFilteredForSelection(nil, PlatformAnthropic, true))
	})
	t.Run("mixed disabled", func(t *testing.T) {
		require.False(t, isPlatformFilteredForSelection(&Account{Platform: PlatformAnthropic}, PlatformAnthropic, false))
		require.True(t, isPlatformFilteredForSelection(&Account{Platform: PlatformDevin}, PlatformAnthropic, false))
		require.False(t, isPlatformFilteredForSelection(&Account{Platform: PlatformAnthropic}, "", false), "empty platform is never filtered")
	})
}

// TestPlatformAggregateAccountKeepsRateLimitedDiagnosis 是 R5 must_fix 的回归用例：
// 聚合候选（devin）此前被误记为 platform_filtered，吞掉了 model_rate_limited 与恢复建议。
func TestPlatformAggregateAccountKeepsRateLimitedDiagnosis(t *testing.T) {
	svc := &GatewayService{}
	const model = "devin-model"
	acc := &Account{
		ID:          1,
		Platform:    PlatformDevin,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{"model_mapping": map[string]any{model: model}},
		Extra: map[string]any{
			"model_rate_limits": map[string]any{
				model: map[string]any{
					"rate_limit_reset_at": time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339),
				},
			},
		},
	}

	got := svc.diagnoseSelectionFailure(context.Background(), acc, model, PlatformAnthropic, map[int64]struct{}{}, true)
	require.Equal(t, "model_rate_limited", got.Category, "aggregate candidate must keep rate-limit diagnosis instead of being misclassified as platform_filtered")
	require.Contains(t, got.Detail, "remaining=", "recovery hint must be preserved")
}

// TestStickyAggregatePlatformReuse 是 R5 must_fix 的回归用例：旧路径粘性此前只认原生
// 平台或 Antigravity，聚合平台（devin）账号能被选中但不保持粘性。这里把会话绑到一个
// 低优先级的 devin 账号，验证粘性命中被复用而不是回退到主选号。
func TestStickyAggregatePlatformReuse(t *testing.T) {
	setPriorityTierStrictForTest(t, false)
	const model = "devin-model"
	accounts := []Account{
		{ID: 1, Platform: PlatformAnthropic, Priority: 0, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"model_mapping": map[string]any{model: model}}},
		{ID: 2, Platform: PlatformDevin, Priority: 10, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"model_mapping": map[string]any{model: model}}},
	}
	repo := &mockAccountRepoForPlatform{accounts: accounts, accountsByID: map[int64]*Account{}}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}
	cache := &mockGatewayCacheForPlatform{sessionBindings: map[string]int64{"sess-devin": 2}}
	svc := &GatewayService{accountRepo: repo, cache: cache, cfg: testConfig()}

	acc, err := svc.selectAccountWithMixedScheduling(context.Background(), nil, "sess-devin", model, nil, PlatformAnthropic)
	require.NoError(t, err)
	require.NotNil(t, acc)
	require.Equal(t, int64(2), acc.ID, "aggregate-platform sticky binding must be reused")
}
