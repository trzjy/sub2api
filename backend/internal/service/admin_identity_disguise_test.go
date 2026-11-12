package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// DSG-G4：UsesLegacyKeys 只在账号 extra 中确实带有老键时才置位。
// 既无新键、也无老键的普通账号不得被误报为「正在使用旧配置」。

func TestBuildIdentityDisguiseState_UsesLegacyKeysOnlyWhenLegacyPresent(t *testing.T) {
	t.Run("无新键也无老键的账号 → 不报旧配置", func(t *testing.T) {
		a := &Account{Extra: map[string]any{}}
		state := buildIdentityDisguiseState(a)
		require.False(t, state.UsesLegacyKeys, "无配置的账号不应被误报为使用旧配置")
		require.Equal(t, IdentityDisguiseProfileNone, state.Profile, "无配置时 Profile 应为 none")
		require.Empty(t, state.ForbiddenNames)
		require.False(t, state.RewriteModel)
		require.False(t, state.StripReasoning)
		require.Empty(t, state.Prompt)
	})

	t.Run("只有老键 identity_prompt 的账号 → 报旧配置且原样解析", func(t *testing.T) {
		prompt := "你是 DeepSeek，由 DeepSeek 开发的智能助手。"
		a := &Account{Extra: map[string]any{
			"identity_prompt": prompt,
		}}
		state := buildIdentityDisguiseState(a)
		require.True(t, state.UsesLegacyKeys, "带 identity_prompt 老键应报旧配置")
		require.Equal(t, prompt, state.Prompt, "老键人设正文必须原样返回，不得重渲染")
		// 与既有老键解析结果一致。
		require.Equal(t, legacyIdentityDisguiseOf(a).RawPrompt, state.Prompt)
	})

	t.Run("只有老键 mask_upstream_identity 的账号 → 报旧配置且 RewriteModel 为真", func(t *testing.T) {
		a := &Account{Extra: map[string]any{
			"mask_upstream_identity": true,
		}}
		state := buildIdentityDisguiseState(a)
		require.True(t, state.UsesLegacyKeys, "带 mask_upstream_identity 老键应报旧配置")
		require.True(t, state.RewriteModel)
		require.Equal(t, legacyIdentityDisguiseOf(a).RewriteModel, state.RewriteModel)
	})

	t.Run("只有老键 strip_reasoning_content 的账号 → 报旧配置且 StripReasoning 为真", func(t *testing.T) {
		a := &Account{Extra: map[string]any{
			"strip_reasoning_content": true,
		}}
		state := buildIdentityDisguiseState(a)
		require.True(t, state.UsesLegacyKeys, "带 strip_reasoning_content 老键应报旧配置")
		require.True(t, state.StripReasoning)
		require.Equal(t, legacyIdentityDisguiseOf(a).StripReasoning, state.StripReasoning)
	})

	t.Run("四类老键并存 → 报旧配置且与既有老键解析逐字段一致", func(t *testing.T) {
		a := &Account{Extra: map[string]any{
			"identity_prompt":         "你是 Kimi，由月之暗面开发的智能助手。",
			"mask_upstream_identity":  true,
			"response_identity_mask":  "true",
			"strip_reasoning_content": "1",
		}}
		state := buildIdentityDisguiseState(a)
		require.True(t, state.UsesLegacyKeys)
		legacy := legacyIdentityDisguiseOf(a)
		require.Equal(t, legacy.Profile, state.Profile)
		require.Equal(t, legacy.ForbiddenNames, state.ForbiddenNames)
		require.Equal(t, legacy.RewriteModel, state.RewriteModel)
		require.Equal(t, legacy.StripReasoning, state.StripReasoning)
		require.Equal(t, legacy.RawPrompt, state.Prompt, "老键分支的正文必须是原文")
	})

	t.Run("新键存在的账号 → 不报旧配置（回归）", func(t *testing.T) {
		a := &Account{Extra: map[string]any{
			IdentityDisguiseExtraKey: map[string]any{
				"profile":         "deepseek",
				"forbidden_names": []any{"deepseek-flash"},
				"rewrite_model":   true,
				"strip_reasoning": true,
			},
		}}
		state := buildIdentityDisguiseState(a)
		require.False(t, state.UsesLegacyKeys, "新键存在时不应报旧配置")
		require.Equal(t, IdentityDisguiseProfileDeepSeek, state.Profile)
		require.True(t, state.RewriteModel)
		require.True(t, state.StripReasoning)
	})

	t.Run("新键与老键并存 → 以新键为准且不报旧配置", func(t *testing.T) {
		a := &Account{Extra: map[string]any{
			IdentityDisguiseExtraKey: map[string]any{
				"profile":         "kimi",
				"forbidden_names": []any{},
				"rewrite_model":   false,
				"strip_reasoning": false,
			},
			"identity_prompt":        "你是 DeepSeek，由 DeepSeek 开发的智能助手。",
			"mask_upstream_identity": true,
		}}
		state := buildIdentityDisguiseState(a)
		require.False(t, state.UsesLegacyKeys, "新键命中时不看老键")
		require.Equal(t, IdentityDisguiseProfileKimi, state.Profile)
		require.False(t, state.RewriteModel)
		require.False(t, state.StripReasoning)
	})

	t.Run("nil 账号不 panic 且不报旧配置", func(t *testing.T) {
		state := buildIdentityDisguiseState(nil)
		require.False(t, state.UsesLegacyKeys)
		require.Equal(t, IdentityDisguiseProfileNone, state.Profile)
	})
}
