package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestEvaluateAccountSchedulingThreshold_CodeBuddyCreditWindowPauses 验证 PR3.2：
// CodeBuddy 积分额度快照（codebuddy_credit_used_percent + codebuddy_credit_reset_at）超阈值时，
// 调度阈值评估应判定暂停（window=credit, scope=codebuddy），且 UsedPercent/Until 与快照一致。
func TestEvaluateAccountSchedulingThreshold_CodeBuddyCreditWindowPauses(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(3 * time.Hour)

	account := &Account{
		Platform: PlatformCodeBuddy,
		Extra: map[string]any{
			codebuddyCreditUsedPercentKey: 92.0,
			codebuddyCreditResetAtKey:     resetAt.Format(time.RFC3339),
		},
	}

	decision := EvaluateAccountSchedulingThreshold(account, map[string]int{
		PlatformCodeBuddy: 90,
	}, now)

	require.True(t, decision.ShouldPause)
	require.Equal(t, PlatformCodeBuddy, decision.Platform)
	require.Equal(t, 90, decision.ThresholdPercent)
	require.Equal(t, "credit", decision.Window)
	require.Equal(t, PlatformCodeBuddy, decision.Scope)
	require.Equal(t, 92.0, decision.UsedPercent)
	require.NotNil(t, decision.Until)
	require.True(t, resetAt.Equal(*decision.Until))
}

// TestEvaluateAccountSchedulingThreshold_CodeBuddyBelowThresholdDoesNotPause 验证快照低于阈值
// 或缺失快照时不应触发暂停（fail-open，不误杀账号）。
func TestEvaluateAccountSchedulingThreshold_CodeBuddyBelowThresholdDoesNotPause(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(3 * time.Hour)

	below := &Account{
		Platform: PlatformCodeBuddy,
		Extra: map[string]any{
			codebuddyCreditUsedPercentKey: 50.0,
			codebuddyCreditResetAtKey:     resetAt.Format(time.RFC3339),
		},
	}
	require.False(t,
		EvaluateAccountSchedulingThreshold(below, map[string]int{PlatformCodeBuddy: 90}, now).ShouldPause,
		"低于阈值不应暂停")

	missing := &Account{Platform: PlatformCodeBuddy, Extra: map[string]any{}}
	require.False(t,
		EvaluateAccountSchedulingThreshold(missing, map[string]int{PlatformCodeBuddy: 90}, now).ShouldPause,
		"缺失快照不应暂停")
}
