//go:build unit

// 调度资格预检逻辑（AG-TEST-01）的单测：覆盖所有阻断场景与分组白名单提示场景。
// 命名以 TestAccountTestSchedulingGate 前缀命中验证白名单 `-run TestAccountTestSchedulingGate`。
package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// newActiveAntigravityAccount 构造一个状态 active、可调度、开启混合调度的
// Antigravity 账号，作为各项用例的基线。
func newActiveAntigravityAccount(mixedScheduling bool) *Account {
	return &Account{
		Platform:    PlatformAntigravity,
		Status:      StatusActive,
		Schedulable: true,
		Extra:       map[string]any{"mixed_scheduling": mixedScheduling},
	}
}

// TestAccountTestSchedulingGate_StatusNotActive 状态非 active 应被阻断。
func TestAccountTestSchedulingGate_StatusNotActive(t *testing.T) {
	acc := newActiveAntigravityAccount(true)
	acc.Status = "error"

	warnings, blockReason := evaluateAccountTestSchedulingGate(context.Background(), acc, "claude-sonnet-4-5")

	require.Empty(t, warnings)
	require.Contains(t, blockReason, "账号状态非 active")
}

// TestAccountTestSchedulingGate_SchedulableFalse 调度禁用应被阻断。
func TestAccountTestSchedulingGate_SchedulableFalse(t *testing.T) {
	acc := newActiveAntigravityAccount(true)
	acc.Schedulable = false

	warnings, blockReason := evaluateAccountTestSchedulingGate(context.Background(), acc, "claude-sonnet-4-5")

	require.Empty(t, warnings)
	require.Contains(t, blockReason, "账号调度已禁用")
}

// TestAccountTestSchedulingGate_AntigravityMissingMixedScheduling 缺少 mixed_scheduling
// 或值为 false 都应被阻断。
func TestAccountTestSchedulingGate_AntigravityMissingMixedScheduling(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		acc := &Account{
			Platform:    PlatformAntigravity,
			Status:      StatusActive,
			Schedulable: true,
			Extra:       map[string]any{},
		}

		warnings, blockReason := evaluateAccountTestSchedulingGate(context.Background(), acc, "claude-sonnet-4-5")

		require.Empty(t, warnings)
		require.Contains(t, blockReason, "混合调度未开启")
	})

	t.Run("false", func(t *testing.T) {
		acc := newActiveAntigravityAccount(false)

		warnings, blockReason := evaluateAccountTestSchedulingGate(context.Background(), acc, "claude-sonnet-4-5")

		require.Empty(t, warnings)
		require.Contains(t, blockReason, "混合调度未开启")
	})
}

// TestAccountTestSchedulingGate_AntigravityModelNotMapped 请求模型不在映射表中应被阻断。
func TestAccountTestSchedulingGate_AntigravityModelNotMapped(t *testing.T) {
	acc := newActiveAntigravityAccount(true)
	// 自定义一个极小的模型映射，排除未知模型。
	acc.Credentials = map[string]any{
		"model_mapping": map[string]any{
			"claude-sonnet-4-5": "claude-sonnet-4-5",
		},
	}

	warnings, blockReason := evaluateAccountTestSchedulingGate(context.Background(), acc, "totally-unknown-model-xyz")

	require.Empty(t, warnings)
	require.Contains(t, blockReason, "未在账号模型映射表中配置")
}

// TestAccountTestSchedulingGate_AntigravityPass 正常 Antigravity（状态 active、可调度、
// 开启混合调度、模型可映射）应通过预检。
func TestAccountTestSchedulingGate_AntigravityPass(t *testing.T) {
	acc := newActiveAntigravityAccount(true)
	// 加入一个未启用白名单的分组，避免触发"未加入任何分组"提示，且不应产生任何 warning。
	acc.Groups = []*Group{{ID: 1, ModelAllowlist: GroupModelAllowlist{Enabled: false}}}

	warnings, blockReason := evaluateAccountTestSchedulingGate(context.Background(), acc, "claude-sonnet-4-5")

	require.Empty(t, blockReason)
	require.Empty(t, warnings)
}

// TestAccountTestSchedulingGate_GroupAllowlistAllReject 账号所属分组全部启用白名单且都
// 拒绝请求模型时，应产生 [调度提示] warning（不阻断）。
func TestAccountTestSchedulingGate_GroupAllowlistAllReject(t *testing.T) {
	acc := newActiveAntigravityAccount(true)
	acc.Groups = []*Group{
		{ID: 1, ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"claude-sonnet-4-5"}}},
		{ID: 2, ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"gemini-3-pro"}}},
	}

	// claude-opus-4-7 在 Antigravity 默认映射中可解析（通过规则 4），
	// 但两个分组白名单都不放行，应产生分组白名单 warning。
	warnings, blockReason := evaluateAccountTestSchedulingGate(context.Background(), acc, "claude-opus-4-7")

	require.Empty(t, blockReason)
	require.Len(t, warnings, 1)
	require.Contains(t, warnings[0], "[调度提示]")
	require.Contains(t, warnings[0], "claude-opus-4-7")
	require.Contains(t, warnings[0], "不在账号所属任何分组的模型白名单内")
}

// TestAccountTestSchedulingGate_GroupAllowlistPartialAllow 任一启用白名单的分组放行该模型，
// 则不应产生分组白名单 warning。
func TestAccountTestSchedulingGate_GroupAllowlistPartialAllow(t *testing.T) {
	acc := newActiveAntigravityAccount(true)
	acc.Groups = []*Group{
		{ID: 1, ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"claude-sonnet-4-5"}}},
		{ID: 2, ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"gemini-3-pro"}}},
	}

	warnings, blockReason := evaluateAccountTestSchedulingGate(context.Background(), acc, "claude-sonnet-4-5")

	require.Empty(t, blockReason)
	require.Empty(t, warnings)
}

// TestAccountTestSchedulingGate_NoGroupWarning 账号未加入任何分组应产生 [调度提示] warning（不阻断）。
func TestAccountTestSchedulingGate_NoGroupWarning(t *testing.T) {
	acc := newActiveAntigravityAccount(true)

	warnings, blockReason := evaluateAccountTestSchedulingGate(context.Background(), acc, "claude-sonnet-4-5")

	require.Empty(t, blockReason)
	require.Len(t, warnings, 1)
	require.Contains(t, warnings[0], "[调度提示]")
	require.Contains(t, warnings[0], "未加入任何分组")
}

// TestAccountTestSchedulingGate_NonAntigravityModelMapping 非 Antigravity 平台，配置了模型映射表
// 但请求模型不在其中时应被阻断。
func TestAccountTestSchedulingGate_NonAntigravityModelMapping(t *testing.T) {
	acc := &Account{
		Platform:    PlatformGrok,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"grok-4": "grok-4",
			},
		},
	}

	warnings, blockReason := evaluateAccountTestSchedulingGate(context.Background(), acc, "gpt-4o")

	require.Empty(t, warnings)
	require.Contains(t, blockReason, "未在账号模型映射表中配置")
}
