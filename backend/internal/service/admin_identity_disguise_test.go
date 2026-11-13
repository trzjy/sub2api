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

// 「自动生成」开关必须随配置落库并原样回显 ——
// 禁词列表本身无法反推它（自动推导出的列表与人工填写的列表存储形态完全一样）。
func TestBuildIdentityDisguiseState_AutoDeriveFlag(t *testing.T) {
	t.Run("新键带 auto_derive_forbidden_names=true → 原样回显", func(t *testing.T) {
		a := &Account{Extra: map[string]any{
			IdentityDisguiseExtraKey: map[string]any{
				"profile":                    "kimi",
				"forbidden_names":            []any{"kimi-k2"},
				"auto_derive_forbidden_names": true,
				"rewrite_model":              true,
				"strip_reasoning":            true,
			},
		}}
		state := buildIdentityDisguiseState(a)
		require.True(t, state.AutoDeriveForbidden, "列表非空但开关为自动生成时必须回显 true")
		require.Len(t, state.ForbiddenNames, 1)
	})

	t.Run("新键带 auto_derive_forbidden_names=false 且列表非空 → 回显 false", func(t *testing.T) {
		a := &Account{Extra: map[string]any{
			IdentityDisguiseExtraKey: map[string]any{
				"profile":                    "kimi",
				"forbidden_names":            []any{"kimi-k2"},
				"auto_derive_forbidden_names": false,
			},
		}}
		state := buildIdentityDisguiseState(a)
		require.False(t, state.AutoDeriveForbidden, "人工填写的列表必须回显 false")
	})

	t.Run("存量账号缺该字段 → 回退旧口径（列表为空即自动推导）", func(t *testing.T) {
		a := &Account{Extra: map[string]any{
			IdentityDisguiseExtraKey: map[string]any{
				"profile":         "kimi",
				"forbidden_names": []any{},
			},
		}}
		state := buildIdentityDisguiseState(a)
		require.True(t, state.AutoDeriveForbidden, "存量账号回显口径必须与引入前一致")
	})

	t.Run("存量账号缺该字段且列表非空 → 回退为 false", func(t *testing.T) {
		a := &Account{Extra: map[string]any{
			IdentityDisguiseExtraKey: map[string]any{
				"profile":         "kimi",
				"forbidden_names": []any{"kimi-k2"},
			},
		}}
		state := buildIdentityDisguiseState(a)
		require.False(t, state.AutoDeriveForbidden)
	})
}

// DSG-C3F：UsesLegacyKeys 按「老键是否存在」判断，而非解析后的有效值。
// 老键存在但值为空串 / false 时仍须报「使用旧配置」，否则前端不提示迁移。
func TestBuildIdentityDisguiseState_UsesLegacyKeysByKeyPresence(t *testing.T) {
	t.Run("老键 identity_prompt 存在但为空串 → 报旧配置", func(t *testing.T) {
		a := &Account{Extra: map[string]any{
			"identity_prompt": "",
		}}
		state := buildIdentityDisguiseState(a)
		require.True(t, state.UsesLegacyKeys, "空串老键仍应报旧配置，否则前端不提示迁移")
		require.Empty(t, state.Prompt, "空串正文不应产生有效人设")
	})

	t.Run("老键 mask_upstream_identity 存在但为 false → 报旧配置", func(t *testing.T) {
		a := &Account{Extra: map[string]any{
			"mask_upstream_identity": false,
		}}
		state := buildIdentityDisguiseState(a)
		require.True(t, state.UsesLegacyKeys, "值为 false 的老键仍应报旧配置")
		require.False(t, state.RewriteModel, "false 不应被折进 RewriteModel")
	})

	t.Run("老键 strip_reasoning_content 存在但为 false → 报旧配置", func(t *testing.T) {
		a := &Account{Extra: map[string]any{
			"strip_reasoning_content": false,
		}}
		state := buildIdentityDisguiseState(a)
		require.True(t, state.UsesLegacyKeys, "值为 false 的老键仍应报旧配置")
		require.False(t, state.StripReasoning, "false 不应被折进 StripReasoning")
	})

	t.Run("老键 response_identity_mask 存在但为 false → 报旧配置", func(t *testing.T) {
		a := &Account{Extra: map[string]any{
			"response_identity_mask": false,
		}}
		state := buildIdentityDisguiseState(a)
		require.True(t, state.UsesLegacyKeys, "值为 false 的老键仍应报旧配置")
	})

	t.Run("无任何老键、无新键 → 不报旧配置（回归）", func(t *testing.T) {
		a := &Account{Extra: map[string]any{}}
		state := buildIdentityDisguiseState(a)
		require.False(t, state.UsesLegacyKeys, "无配置账号不应被误报为使用旧配置")
		require.Equal(t, IdentityDisguiseProfileNone, state.Profile)
		require.Empty(t, state.Prompt)
	})

	t.Run("新键存在 → 不报旧配置（回归）", func(t *testing.T) {
		a := &Account{Extra: map[string]any{
			IdentityDisguiseExtraKey: map[string]any{
				"profile":         "deepseek",
				"forbidden_names": []any{"x"},
				"rewrite_model":   false,
				"strip_reasoning": false,
			},
		}}
		state := buildIdentityDisguiseState(a)
		require.False(t, state.UsesLegacyKeys, "新键存在时不应报旧配置")
		require.Equal(t, IdentityDisguiseProfileDeepSeek, state.Profile)
	})
}
