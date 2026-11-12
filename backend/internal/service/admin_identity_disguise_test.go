//go:build unit

package service

import (
	"context"
	"fmt"
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

// DSG-C2F：写入路径不得再截断自动派生的禁词。
//
// 注意：读路径 parseIdentityDisguiseExtra 仍走 sanitizeForbiddenNames（受禁区约束，
// 不得改 identity_disguise.go），故断言必须针对「落库值」（UpdateExtra 收到的 payload），
// 而非 SetAccountIdentityDisguise 回显的 state.ForbiddenNames（读路径会被截断到 16 项）。

// identityDisguiseWriteRepoStub 复用 accountRepoStub 的全接口实现，
// 仅覆盖 SetAccountIdentityDisguise 实际用到的 GetByID 与 UpdateExtra。
type identityDisguiseWriteRepoStub struct {
	accountRepoStub
	byID        map[int64]*Account
	lastUpdates map[string]any
}

func (s *identityDisguiseWriteRepoStub) GetByID(_ context.Context, id int64) (*Account, error) {
	a, ok := s.byID[id]
	if !ok {
		return nil, fmt.Errorf("account %d not found", id)
	}
	return a, nil
}

func (s *identityDisguiseWriteRepoStub) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	s.lastUpdates = updates
	if a, ok := s.byID[id]; ok {
		if a.Extra == nil {
			a.Extra = map[string]any{}
		}
		for k, v := range updates {
			a.Extra[k] = v
		}
	}
	return nil
}

// storedForbiddenNames 从 UpdateExtra 的 payload 中取出落库的 forbidden_names（按字符串切片返回）。
func storedForbiddenNames(t *testing.T, updates map[string]any) []string {
	t.Helper()
	require.NotNil(t, updates, "UpdateExtra 应被调用")
	inner, ok := updates[IdentityDisguiseExtraKey].(map[string]any)
	require.True(t, ok, "UpdateExtra 应写入 identity_disguise 键")
	raw, ok := inner["forbidden_names"].([]any)
	require.True(t, ok, "identity_disguise 应含 forbidden_names")
	out := make([]string, 0, len(raw))
	for _, it := range raw {
		s, ok := it.(string)
		require.True(t, ok, "forbidden_names 元素应为字符串")
		out = append(out, s)
	}
	return out
}

func TestAdminIdentityDisguise_AutoDeriveKeepsAllNames(t *testing.T) {
	// 构造 model_mapping 含 >16 个键的账号；用无别名的纯模型名让派生结果可预测。
	const keys = 20
	mapping := make(map[string]any, keys)
	for i := 0; i < keys; i++ {
		mapping[fmt.Sprintf("auto-model-%02d", i)] = "upstream-target"
	}
	acc := &Account{ID: 1, Credentials: map[string]any{"model_mapping": mapping}}
	repo := &identityDisguiseWriteRepoStub{byID: map[int64]*Account{1: acc}}
	svc := &adminServiceImpl{accountRepo: repo}

	_, err := svc.SetAccountIdentityDisguise(context.Background(), 1, IdentityDisguiseUpdate{
		Profile:             IdentityDisguiseProfileKimi,
		AutoDeriveForbidden: true,
		RewriteModel:        true,
	})
	require.NoError(t, err)

	// 断言落库值（读路径仍按既有规则 sanitize，不能用来验证不截断）。
	stored := storedForbiddenNames(t, repo.lastUpdates)
	expected := DeriveIdentityDisguiseForbiddenNames(acc.GetModelMapping())
	require.Greater(t, len(expected), IdentityDisguiseForbiddenNamesMaxCount,
		"前置条件：派生结果必须超过 16 项才能验证不截断")
	require.Len(t, stored, keys, "自动派生落库应完整保留全部派生项")
	require.Equal(t, expected, stored,
		"自动派生落库必须完整保留全部派生项，不得被 16 项上限截断")
}

func TestAdminIdentityDisguise_ExplicitInputTruncated(t *testing.T) {
	// 显式输入超过 16 项，必须仍按既有规则截断（回归，证明显式路径未变）。
	const n = 20
	names := make([]string, 0, n)
	for i := 0; i < n; i++ {
		names = append(names, fmt.Sprintf("explicit-name-%02d", i))
	}
	acc := &Account{ID: 2, Credentials: map[string]any{}} // 空映射，确保不走派生
	repo := &identityDisguiseWriteRepoStub{byID: map[int64]*Account{2: acc}}
	svc := &adminServiceImpl{accountRepo: repo}

	_, err := svc.SetAccountIdentityDisguise(context.Background(), 2, IdentityDisguiseUpdate{
		Profile:             IdentityDisguiseProfileDeepSeek,
		ForbiddenNames:      names,
		AutoDeriveForbidden: false,
	})
	require.NoError(t, err)

	stored := storedForbiddenNames(t, repo.lastUpdates)
	expected := sanitizeForbiddenNames(names)
	require.Len(t, stored, IdentityDisguiseForbiddenNamesMaxCount,
		"显式输入超过 16 项时必须按既有规则截断到 16 项")
	require.Equal(t, expected, stored, "显式输入路径行为应完全等同于 sanitizeForbiddenNames")
}

func TestAdminIdentityDisguise_AutoDeriveEmptyStoresEmpty(t *testing.T) {
	// 空映射：派生结果为空，落库为空且不报错（回归）。
	acc := &Account{ID: 3, Credentials: map[string]any{}}
	repo := &identityDisguiseWriteRepoStub{byID: map[int64]*Account{3: acc}}
	svc := &adminServiceImpl{accountRepo: repo}

	state, err := svc.SetAccountIdentityDisguise(context.Background(), 3, IdentityDisguiseUpdate{
		Profile:             IdentityDisguiseProfileKimi,
		AutoDeriveForbidden: true,
	})
	require.NoError(t, err)
	stored := storedForbiddenNames(t, repo.lastUpdates)
	require.Empty(t, stored, "空映射派生落库应为空")
	require.Empty(t, state.ForbiddenNames)
}

func TestAdminIdentityDisguise_ExplicitEmptyStoresEmpty(t *testing.T) {
	// 显式输入为空：落库为空且不报错（回归，证明显式路径未变）。
	acc := &Account{ID: 4, Credentials: map[string]any{}}
	repo := &identityDisguiseWriteRepoStub{byID: map[int64]*Account{4: acc}}
	svc := &adminServiceImpl{accountRepo: repo}

	state, err := svc.SetAccountIdentityDisguise(context.Background(), 4, IdentityDisguiseUpdate{
		Profile:             IdentityDisguiseProfileKimi,
		ForbiddenNames:      nil,
		AutoDeriveForbidden: false,
	})
	require.NoError(t, err)
	stored := storedForbiddenNames(t, repo.lastUpdates)
	require.Empty(t, stored, "显式空输入落库应为空")
	require.Empty(t, state.ForbiddenNames)
}
