package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// --- 谓词窄域语义（方案 §改动清单 / 禁区）---

// TestIsCompositeOwnableAccountPlatformEqualsConcreteUnionOther 锁定谓词严格等于
// isConcreteRequestPlatform ∪ {other}：具体平台放行；other 放行；未知平台（含非空
// model_mapping 的 unknownplat）必须 false，杜绝"非空即放行"漂移（验收口径⑥）。
func TestIsCompositeOwnableAccountPlatformEqualsConcreteUnionOther(t *testing.T) {
	for _, plat := range []string{
		PlatformAnthropic, PlatformOpenAI, PlatformGemini, PlatformAntigravity, PlatformGrok,
		PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax,
	} {
		require.True(t, isCompositeOwnableAccountPlatform(plat), "concrete platform %q must be ownable", plat)
		require.True(t, isConcreteRequestPlatform(plat))
	}
	require.True(t, isCompositeOwnableAccountPlatform(PlatformOther), "other must be ownable")
	require.False(t, isConcreteRequestPlatform(PlatformOther), "other must NOT leak into concrete predicate")

	for _, plat := range []string{"", "unknownplat", "web-unknown", "qwen"} {
		require.False(t, isCompositeOwnableAccountPlatform(plat), "unknown platform %q must NOT be ownable", plat)
	}
}

// --- 验收口径①：other + 显式映射账号经归属层放行、盖章平台=other、UpstreamModel 传播正确 ---

func TestCompositeOwnershipAdmitsOtherPlatformExplicitMapping(t *testing.T) {
	groupID := int64(94)
	repo := &compositeOwnershipAccountRepo{
		accounts: []Account{
			{
				ID:       1,
				Platform: PlatformOther,
				Credentials: map[string]any{
					"model_mapping": map[string]any{"qwen3.8-flash": "qwen3.8-flash:free"},
				},
			},
		},
	}
	svc := &GatewayService{accountRepo: repo}

	// 归属层：other 显式映射账号被纳入，盖章平台=other。
	ownership, err := svc.resolveCompositeModelOwnership(context.Background(), groupID, "qwen3.8-flash")
	require.NoError(t, err)
	require.Equal(t, CompositeModelOwnership{TargetPlatform: PlatformOther, Matched: true}, ownership)

	// 门禁（resolver Matched 分支守卫）：ownership 产出不被二次拒，放行且盖章 other。
	resolver := NewCompositeRouteResolver(nil)
	resolver.SetModelOwnershipResolver(svc.resolveCompositeModelOwnership)
	decision, err := resolver.Resolve(context.Background(), groupID, "qwen3.8-flash", CompositeRouteEndpointResponses)
	require.NoError(t, err)
	require.True(t, decision.Matched, "门禁应放行 other 归属")
	require.Equal(t, CompositeRouteSourceAccount, decision.Source)
	require.Equal(t, PlatformOther, decision.TargetPlatform, "盖章平台必须为 other")
	require.Equal(t, "qwen3.8-flash", decision.UpstreamModel, "UpstreamModel 应传播为请求公开模型")

	// UpstreamModel 经 model_mapping 正确归一到上游模型名（deepseek 同机制实证）。
	mapped, ok := repo.accounts[0].ResolveMappedModel("qwen3.8-flash")
	require.True(t, ok)
	require.Equal(t, "qwen3.8-flash:free", mapped)
}

// --- 验收口径②：other 空映射账号不产生归属（天然安全）---

func TestCompositeOwnershipExcludesOtherEmptyMapping(t *testing.T) {
	groupID := int64(94)
	repo := &compositeOwnershipAccountRepo{
		accounts: []Account{
			{
				ID:       1,
				Platform: PlatformOther,
				// 空 model_mapping → other 无内置目录，无可服务模型。
				Credentials: map[string]any{
					"model_mapping": map[string]any{},
				},
			},
		},
	}
	svc := &GatewayService{accountRepo: repo}

	ownership, err := svc.resolveCompositeModelOwnership(context.Background(), groupID, "qwen3.8-flash")
	require.NoError(t, err)
	require.Equal(t, CompositeModelOwnership{}, ownership, "other 空映射账号不得产生归属")
}

// --- 验收口径⑤：other 与具体平台对同别名各自显式映射 → Ambiguous → 门禁 400（不按遍历择一）---

func TestCompositeOwnershipAmbiguousOtherVsConcretePlatform(t *testing.T) {
	groupID := int64(94)
	repo := &compositeOwnershipAccountRepo{
		accounts: []Account{
			{
				ID:       1,
				Platform: PlatformOther,
				Credentials: map[string]any{
					"model_mapping": map[string]any{"shared-alias": "qwen3.8-flash:free"},
				},
			},
			{
				ID:       2,
				Platform: PlatformDeepseek,
				Credentials: map[string]any{
					"model_mapping": map[string]any{"shared-alias": "deepseek-v4-pro"},
				},
			},
		},
	}
	svc := &GatewayService{accountRepo: repo}

	// 归属层判歧义，不按遍历顺序择一。
	ownership, err := svc.resolveCompositeModelOwnership(context.Background(), groupID, "shared-alias")
	require.NoError(t, err)
	require.Equal(t, CompositeModelOwnership{Ambiguous: true}, ownership)
	require.False(t, ownership.Matched)

	// 门禁：歧义 → 不匹配 → 上层 400。
	resolver := NewCompositeRouteResolver(nil)
	resolver.SetModelOwnershipResolver(svc.resolveCompositeModelOwnership)
	decision, err := resolver.Resolve(context.Background(), groupID, "shared-alias", CompositeRouteEndpointResponses)
	require.NoError(t, err)
	require.False(t, decision.Matched)
	require.Equal(t, "model is exposed by multiple provider platforms", decision.Reason)
}

// --- 验收口径⑥：未知平台（unknownplat）即使有有效 model_mapping，谓词 false、不产生归属、
// 不进 A/B 目录扫描结果 ---

func TestCompositeOwnershipRejectsUnknownPlatformWithValidMapping(t *testing.T) {
	groupID := int64(94)
	repo := &compositeOwnershipAccountRepo{
		accounts: []Account{
			{
				ID:       1,
				Platform: "unknownplat", // 未知平台但配了有效映射
				Credentials: map[string]any{
					"model_mapping": map[string]any{"my-model": "upstream-x"},
				},
			},
		},
	}
	svc := &GatewayService{accountRepo: repo}

	ownership, err := svc.resolveCompositeModelOwnership(context.Background(), groupID, "my-model")
	require.NoError(t, err)
	require.Equal(t, CompositeModelOwnership{}, ownership, "未知平台不得产生归属")

	// 同时不进 A/B 两目录扫描（扫描点 A/B 用同一谓词过滤）。
	accounts := repo.accounts
	a := resolveCodexCatalogMetadataModel(PlatformComposite, "my-model", accounts, nil, true)
	require.Equal(t, "my-model", a, "扫描点 A 不得收录 unknownplat 映射")
	p, _, ok := resolveCodexCompositeModelTarget("my-model", accounts, nil, true)
	require.False(t, ok, "扫描点 B 不得收录 unknownplat 映射")
	require.Empty(t, p)
}

// --- 验收口径③④：composite 分组 /v1/models 目录扫描点 A 与 B 分别独立用例 ---

// 扫描点 A（openai_codex_models_service.go:1045）：other 显式映射收录。
func TestResolveCodexCatalogMetadataModelCollectsOtherExplicitMapping(t *testing.T) {
	accounts := []Account{
		{
			ID:       1,
			Platform: PlatformOther,
			Credentials: map[string]any{
				"model_mapping": map[string]any{"qwen3.8-flash": "qwen3.8-flash:free"},
			},
		},
	}

	// 回到具体平台解析时，other 账号 claim → 返回其映射上游模型，证明被目录收录。
	got := resolveCodexCatalogMetadataModel(PlatformComposite, "qwen3.8-flash", accounts, nil, true)
	require.Equal(t, "qwen3.8-flash:free", got, "扫描点 A 应收录 other 显式映射并返回上游模型")
}

// 扫描点 A：other 空映射排除（空映射账号不 claim，目录回落到未归属原样，而非映射上游）。
func TestResolveCodexCatalogMetadataModelExcludesOtherEmptyMapping(t *testing.T) {
	accounts := []Account{
		{
			ID:       1,
			Platform: PlatformOther,
			Credentials: map[string]any{
				"model_mapping": map[string]any{}, // 空映射
			},
		},
	}

	got := resolveCodexCatalogMetadataModel(PlatformComposite, "qwen3.8-flash", accounts, nil, true)
	require.Equal(t, "qwen3.8-flash", got, "扫描点 A 应排除 other 空映射（不归一到 :free）")
}

// 扫描点 B（openai_codex_models_service.go:1200）：other 显式映射收录。
func TestResolveCodexCompositeModelTargetCollectsOtherExplicitMapping(t *testing.T) {
	accounts := []Account{
		{
			ID:       1,
			Platform: PlatformOther,
			Credentials: map[string]any{
				"model_mapping": map[string]any{"qwen3.8-flash": "qwen3.8-flash:free"},
			},
		},
	}

	platform, upstream, ok := resolveCodexCompositeModelTarget("qwen3.8-flash", accounts, nil, true)
	require.True(t, ok, "扫描点 B 应收录 other 显式映射")
	require.Equal(t, PlatformOther, platform, "盖章平台必须为 other")
	require.Equal(t, "qwen3.8-flash", upstream)
}

// 扫描点 B：other 空映射排除。
func TestResolveCodexCompositeModelTargetExcludesOtherEmptyMapping(t *testing.T) {
	accounts := []Account{
		{
			ID:       1,
			Platform: PlatformOther,
			Credentials: map[string]any{
				"model_mapping": map[string]any{},
			},
		},
	}

	platform, upstream, ok := resolveCodexCompositeModelTarget("qwen3.8-flash", accounts, nil, true)
	require.False(t, ok, "扫描点 B 应排除 other 空映射")
	require.Empty(t, platform)
	require.Empty(t, upstream)
}
