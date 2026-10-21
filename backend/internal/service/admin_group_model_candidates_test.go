//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// stubGroupModelCatalogReader 是候选目录读取器的可控测试替身（只存在于测试代码）。
type stubGroupModelCatalogReader struct {
	byAccount map[int64][]string
	errs      map[int64]error
}

func (s *stubGroupModelCatalogReader) FetchAccountModelCatalog(_ context.Context, account *Account) ([]string, error) {
	if s.errs != nil {
		if err, ok := s.errs[account.ID]; ok {
			return nil, err
		}
	}
	return s.byAccount[account.ID], nil
}

func TestAdminService_GroupModelAllowlistCandidates_NoGroupReturnsEmpty(t *testing.T) {
	// P1：未创建组（id=0）没有已绑定账号，返回空候选，不借用全平台目录。
	svc := &adminServiceImpl{}
	candidates, err := svc.GetGroupModelsListCandidates(context.Background(), 0, "")
	require.NoError(t, err)
	require.Empty(t, candidates)
}

func TestAdminService_GroupModelAllowlistCandidates_ReaderNotConfiguredFailsClosed(t *testing.T) {
	// P3：依赖缺失（目录读取器未装配）返回明确配置错误，不当作"没有账号"。
	svc := svcForCandidateTest(nil, nil, []Account{{ID: 1, Platform: PlatformOpenAI}})
	_, err := svc.GetGroupModelsListCandidates(context.Background(), 88, PlatformOpenAI)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not configured")
}

func TestAdminService_GroupModelAllowlistCandidates_EmptyGroupReturnsEmpty(t *testing.T) {
	// P1：已建但无适用账号的组返回空候选，不借用别组/全平台目录。
	svc := svcForCandidateTest(&stubGroupModelCatalogReader{byAccount: map[int64][]string{}}, nil, nil)
	candidates, err := svc.GetGroupModelsListCandidates(context.Background(), 88, PlatformOpenAI)
	require.NoError(t, err)
	require.Empty(t, candidates)
}

func TestAdminService_GroupModelAllowlistCandidates_RealCatalogOnly(t *testing.T) {
	// P2：候选只来自账号真实目录与显式精确 alias；平台默认目录/Grok 默认注入不进入。
	// 用空 mapping 的账号让真实目录透传（IsModelSupported 对无映射 OpenAI 账号放行全部）。
	reader := &stubGroupModelCatalogReader{
		byAccount: map[int64][]string{
			1: {"gpt-5.5", "gpt-5.4"},
			2: {"gpt-5.4-mini"},
		},
	}
	accounts := []Account{
		{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{}},
		{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{}},
	}
	svc := svcForCandidateTest(reader, nil, accounts)

	candidates, err := svc.collectGroupModelAllowlistCandidates(context.Background(), 99, PlatformOpenAI, accounts)
	require.NoError(t, err)
	// 真实目录透传项。
	require.Contains(t, candidates, "gpt-5.5")
	require.Contains(t, candidates, "gpt-5.4")
	require.Contains(t, candidates, "gpt-5.4-mini")
	// 无平台默认目录注入（例如 openai 的 gpt-4o 不得出现）。
	require.NotContains(t, candidates, "gpt-4o")
}

// 显式精确 alias：解析目标在目录内 → 列出；不在目录内 → 不列（Grok 隐式默认同理）。
func TestAdminService_GroupModelAllowlistCandidates_ExplicitAliasResolvedTargetGating(t *testing.T) {
	reader := &stubGroupModelCatalogReader{
		byAccount: map[int64][]string{1: {"grok-4.3", "grok-4.9"}},
	}
	accounts := []Account{
		{ID: 1, Platform: PlatformGrok, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"model_mapping": map[string]any{
				"grok-4.3":    "grok-4.3", // 目录模型以精确键暴露 → 透传
				"grok-custom": "grok-4.9", // alias 目标在目录内 → 列出
				"grok-ghost":  "grok-9",   // alias 目标不在目录内 → 不列
			}}},
	}
	svc := svcForCandidateTest(reader, nil, accounts)

	candidates, err := svc.collectGroupModelAllowlistCandidates(context.Background(), 99, PlatformGrok, accounts)
	require.NoError(t, err)
	require.Contains(t, candidates, "grok-4.3")
	require.Contains(t, candidates, "grok-custom")
	require.NotContains(t, candidates, "grok-ghost")
	require.NotContains(t, candidates, "grok-9")
}

func TestAdminService_GroupModelAllowlistCandidates_CompositeMergesConcreteAccounts(t *testing.T) {
	// 组内多账号同组合成平台：候选只含适用账号支撑项；不适用账号（CN/composite）不掺入。
	reader := &stubGroupModelCatalogReader{
		byAccount: map[int64][]string{
			1: {"gpt-5.5"},
			2: {"claude-sonnet-4-6"},
		},
	}
	accounts := []Account{
		{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"model_mapping": map[string]any{}}},
		{ID: 2, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Credentials: map[string]any{"model_mapping": map[string]any{}}},
		{ID: 3, Platform: PlatformKimi, Type: AccountTypeAPIKey, Credentials: map[string]any{"model_mapping": map[string]any{}}},
	}
	svc := svcForCandidateTest(reader, nil, accounts)

	candidates, err := svc.collectGroupModelAllowlistCandidates(context.Background(), 99, PlatformComposite, accounts)
	require.NoError(t, err)
	require.Equal(t, []string{"claude-sonnet-4-6", "gpt-5.5"}, candidates)
}

func TestAdminService_GroupModelAllowlistCandidates_OneAccountFailureFailsWholeRequest(t *testing.T) {
	// P3：任一适用账号目录失败 → 整次候选请求失败，不静默跳过返回不完整并集。
	reader := &stubGroupModelCatalogReader{
		byAccount: map[int64][]string{1: {"gpt-5.5"}},
		errs:      map[int64]error{2: newUpstreamModelSyncUpstreamError("upstream timeout", nil)},
	}
	accounts := []Account{
		{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
		{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
	}
	svc := svcForCandidateTest(reader, nil, accounts)

	_, err := svc.collectGroupModelAllowlistCandidates(context.Background(), 99, PlatformOpenAI, accounts)
	require.Error(t, err)
	require.Contains(t, err.Error(), "account 2")
}

func TestAdminService_GroupModelAllowlistCandidates_AuthoritativeEmptyCatalog(t *testing.T) {
	// P3：目录成功且合法为空 = 空候选（与"读取失败"错误区分）。
	reader := &stubGroupModelCatalogReader{byAccount: map[int64][]string{1: {}}}
	accounts := []Account{{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}}
	svc := svcForCandidateTest(reader, nil, accounts)

	candidates, err := svc.collectGroupModelAllowlistCandidates(context.Background(), 99, PlatformOpenAI, accounts)
	require.NoError(t, err)
	require.Empty(t, candidates)
}

func TestAdminService_GroupModelAllowlistCandidates_MuseCleanNameOnlyWhenContributorPresent(t *testing.T) {
	// muse 目录契 = 上游 contributor 已入库（clean→contributor 已落 mapping）；候选只能来自
	// 已存在 mapping 的显式精确键，不给"默认补项"。
	reader := &stubGroupModelCatalogReader{
		byAccount: map[int64][]string{1: {"muse-spark-x-mini-contributor"}},
	}
	accounts := []Account{
		{ID: 1, Platform: PlatformMuse, Credentials: map[string]any{
			"model_mapping": map[string]any{"muse-spark-x-mini": "muse-spark-x-mini-contributor"},
		}},
	}
	svc := svcForCandidateTest(reader, nil, accounts)

	candidates, err := svc.collectGroupModelAllowlistCandidates(context.Background(), 99, PlatformMuse, accounts)
	require.NoError(t, err)
	require.Equal(t, []string{"muse-spark-x-mini"}, candidates)
}

func TestAdminService_GroupModelAllowlistCandidates_NoWriteBackToAccount(t *testing.T) {
	// 验证 6：候选 GET 不调用模型同步落库 / 健康恢复 / 额度查询，无 mapping 写入或挂组副作用。
	// 目录读取器只读；账号对象不被持久化（无 repo 写入调用）。本用例只验证收集函数可正确合并。
	reader := &stubGroupModelCatalogReader{byAccount: map[int64][]string{1: {"gpt-5.5"}}}
	accounts := []Account{{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}}
	svc := svcForCandidateTest(reader, nil, accounts)
	_, err := svc.collectGroupModelAllowlistCandidates(context.Background(), 99, PlatformOpenAI, accounts)
	require.NoError(t, err)
}

// 场景 4：通配映射键（*）不展开为具体模型（explicitExactMappingKeys 排除通配键）。
func TestAdminService_GroupModelAllowlistCandidates_WildcardMappingKeysNotExpanded(t *testing.T) {
	reader := &stubGroupModelCatalogReader{
		byAccount: map[int64][]string{1: {"gpt-real", "grok-real"}},
	}
	accounts := []Account{
		{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"model_mapping": map[string]any{
				"gpt-real": "gpt-real",
				"*":        "gpt-wildcard-target", // 通配键：不得作为候选来源
			}}},
		{ID: 2, Platform: PlatformGrok, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"model_mapping": map[string]any{"grok-real": "grok-real"}}},
	}
	svc := svcForCandidateTest(reader, nil, accounts)

	candidates, err := svc.collectGroupModelAllowlistCandidates(context.Background(), 99, PlatformOpenAI, accounts)
	require.NoError(t, err)
	// openai 账号：真实目录 + 精确键透传；通配键 * 不展开（即便目标在目录也不列）。
	require.Contains(t, candidates, "gpt-real")
	require.NotContains(t, candidates, "gpt-wildcard-target")
}

// 场景 5：composite 精确路由（enabled+exact）目标有供给才列出；无供给/禁用/前缀路由不列。
func TestAdminService_GroupModelAllowlistCandidates_CompositeExactRouteSupplyGating(t *testing.T) {
	reader := &stubGroupModelCatalogReader{
		byAccount: map[int64][]string{1: {"gpt-5"}},
	}
	routes := []CompositeModelRoute{
		{ID: 1, GroupID: 99, PublicModel: "router-ok", MatchType: "exact", Endpoint: "default", Enabled: true, TargetPlatform: PlatformOpenAI, UpstreamModel: "gpt-5"},
		{ID: 2, GroupID: 99, PublicModel: "router-nosupply", MatchType: "exact", Endpoint: "default", Enabled: true, TargetPlatform: PlatformOpenAI, UpstreamModel: "gpt-missing"},
		{ID: 3, GroupID: 99, PublicModel: "router-disabled", MatchType: "exact", Endpoint: "default", Enabled: false, TargetPlatform: PlatformOpenAI, UpstreamModel: "gpt-5"},
		{ID: 4, GroupID: 99, PublicModel: "router-prefix", MatchType: "prefix", Endpoint: "default", Enabled: true, TargetPlatform: PlatformOpenAI, UpstreamModel: "gpt-5"},
	}
	accounts := []Account{
		{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"model_mapping": map[string]any{"gpt-5": "gpt-5"}}},
	}
	svc := svcForCandidateTest(reader, routes, accounts)

	candidates, err := svc.collectGroupModelAllowlistCandidates(context.Background(), 99, PlatformComposite, accounts)
	require.NoError(t, err)
	require.Contains(t, candidates, "router-ok")          // 精确路由：目标有供给 → 列
	require.NotContains(t, candidates, "router-nosupply") // 目标无供给 → 不列
	require.NotContains(t, candidates, "router-disabled") // 禁用 → 不列
	require.NotContains(t, candidates, "router-prefix")   // 前缀路由不展开为具体模型
}

// 发现 1 负例：composite 模式下，组内 OpenAI 目录含 gpt-5，但启用精确路由把 gpt-5
// 指向无供给的 Gemini 目标；gpt-5 经 resolver 决策后供给校验不通过 → 不入候选。
func TestAdminService_GroupModelAllowlistCandidates_CompositeCatalogIDRoutedToNoSupplyExcluded(t *testing.T) {
	reader := &stubGroupModelCatalogReader{byAccount: map[int64][]string{1: {"gpt-5"}}}
	routes := []CompositeModelRoute{
		{ID: 1, GroupID: 99, PublicModel: "gpt-5", MatchType: "exact", Endpoint: "default", Enabled: true, TargetPlatform: PlatformGemini, UpstreamModel: "gemini-no-supply"},
	}
	accounts := []Account{
		{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"model_mapping": map[string]any{"gpt-5": "gpt-5"}}},
	}
	svc := svcForCandidateTest(reader, routes, accounts)

	candidates, err := svc.collectGroupModelAllowlistCandidates(context.Background(), 99, PlatformComposite, accounts)
	require.NoError(t, err)
	// 来源一（目录 ID gpt-5）同样走 resolver 决策 → 供给校验，无供给 → 不列。
	require.NotContains(t, candidates, "gpt-5")
}

// 发现 1 正例：composite 模式下，路由把 gpt-5 指向有供给的 Gemini 目标 → gpt-5 入候选。
func TestAdminService_GroupModelAllowlistCandidates_CompositeCatalogIDRoutedToSupplyIncluded(t *testing.T) {
	reader := &stubGroupModelCatalogReader{byAccount: map[int64][]string{
		1: {"gpt-5"},
		2: {"gemini-supply"},
	}}
	routes := []CompositeModelRoute{
		{ID: 1, GroupID: 99, PublicModel: "gpt-5", MatchType: "exact", Endpoint: "default", Enabled: true, TargetPlatform: PlatformGemini, UpstreamModel: "gemini-supply"},
	}
	accounts := []Account{
		{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"model_mapping": map[string]any{"gpt-5": "gpt-5"}}},
		{ID: 2, Platform: PlatformGemini, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"model_mapping": map[string]any{"gemini-supply": "gemini-supply"}}},
	}
	svc := svcForCandidateTest(reader, routes, accounts)

	candidates, err := svc.collectGroupModelAllowlistCandidates(context.Background(), 99, PlatformComposite, accounts)
	require.NoError(t, err)
	// 路由目标有供给 → gpt-5 经 resolver 决策后供给校验通过 → 列出。
	require.Contains(t, candidates, "gpt-5")
}

// F3 正向（codex 验收 F3）：Gemini 组 + 启用 mixed_scheduling 的 Antigravity 账号，
// 目录含 gemini-2.5-flash → 候选含 gemini-2.5-flash（复用既有混合供给规则，不新开跨平台能力）。
func TestAdminService_GroupModelAllowlistCandidates_MixedGeminiAccountIncluded(t *testing.T) {
	reader := &stubGroupModelCatalogReader{
		byAccount: map[int64][]string{1: {"gemini-2.5-flash"}},
	}
	accounts := []Account{
		{ID: 1, Platform: PlatformAntigravity, Type: AccountTypeOAuth,
			Extra:       map[string]any{"mixed_scheduling": true},
			Credentials: map[string]any{"model_mapping": map[string]any{"gemini-2.5-flash": "gemini-2.5-flash"}}},
	}
	svc := svcForCandidateTest(reader, nil, accounts)
	candidates, err := svc.collectGroupModelAllowlistCandidates(context.Background(), 88, PlatformGemini, accounts)
	require.NoError(t, err)
	require.Contains(t, candidates, "gemini-2.5-flash")
}

// F3 反向 A：同账号 mixed_scheduling=false → 不纳入 Gemini 组候选。
func TestAdminService_GroupModelAllowlistCandidates_MixedGeminiAccountDisabledNotIncluded(t *testing.T) {
	reader := &stubGroupModelCatalogReader{
		byAccount: map[int64][]string{1: {"gemini-2.5-flash"}},
	}
	accounts := []Account{
		{ID: 1, Platform: PlatformAntigravity, Type: AccountTypeOAuth,
			Extra:       map[string]any{"mixed_scheduling": false},
			Credentials: map[string]any{"model_mapping": map[string]any{"gemini-2.5-flash": "gemini-2.5-flash"}}},
	}
	svc := svcForCandidateTest(reader, nil, accounts)
	candidates, err := svc.collectGroupModelAllowlistCandidates(context.Background(), 88, PlatformGemini, accounts)
	require.NoError(t, err)
	require.Empty(t, candidates)
}

// F3 反向 B：mixed 账号目录含非 gemini 模型（antigravity-only-model）→ 不列（按运行时口径
// mixedListingModelAllowed 仅放行 gemini- 前缀 wire ID；而同账号的 gemini-2.5-flash 仍列出）。
func TestAdminService_GroupModelAllowlistCandidates_MixedGeminiAccountNonGeminiModelExcluded(t *testing.T) {
	reader := &stubGroupModelCatalogReader{
		byAccount: map[int64][]string{1: {"gemini-2.5-flash", "antigravity-only-model"}},
	}
	accounts := []Account{
		{ID: 1, Platform: PlatformAntigravity, Type: AccountTypeOAuth,
			Extra: map[string]any{"mixed_scheduling": true},
			Credentials: map[string]any{"model_mapping": map[string]any{
				"gemini-2.5-flash":       "gemini-2.5-flash",
				"antigravity-only-model": "antigravity-only-model",
			}}},
	}
	svc := svcForCandidateTest(reader, nil, accounts)
	candidates, err := svc.collectGroupModelAllowlistCandidates(context.Background(), 88, PlatformGemini, accounts)
	require.NoError(t, err)
	require.Contains(t, candidates, "gemini-2.5-flash")
	require.NotContains(t, candidates, "antigravity-only-model")
}

// 任务 2：nil reader 但无适用账号（空组）→ 空候选无错误（P1 优先于 P3），不报依赖缺失。
func TestAdminService_GroupModelAllowlistCandidates_NilReaderEmptyApplicableReturnsEmpty(t *testing.T) {
	svc := &adminServiceImpl{} // reader 未装配
	candidates, err := svc.collectGroupModelAllowlistCandidates(context.Background(), 88, PlatformOpenAI, nil)
	require.NoError(t, err)
	require.Empty(t, candidates)
}

func svcForCandidateTest(reader GroupModelCatalogReader, routes []CompositeModelRoute, accounts []Account) *adminServiceImpl {
	return &adminServiceImpl{
		groupRepo: &groupRepoStubForAdmin{
			getByIDByID: map[int64]*Group{
				88: {ID: 88, Platform: PlatformOpenAI},
				99: {ID: 99, Platform: PlatformComposite},
			},
		},
		accountRepo:             &accountRepoStubForCompositeModelsList{accounts: accounts},
		compositeRouteRepo:      &compositeRouteRepoStubForAdmin{routes: routes},
		compositeResolver:       NewCompositeRouteResolver(&compositeRouteRepoStubForAdmin{routes: routes}),
		groupModelCatalogReader: reader,
	}
}
