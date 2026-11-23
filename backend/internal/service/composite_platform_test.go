package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

type compositeOwnershipAccountRepo struct {
	AccountRepository
	accounts []Account
}

func (r *compositeOwnershipAccountRepo) ListModelAvailabilityCandidates(context.Context, *int64, []string, bool) ([]Account, error) {
	return r.accounts, nil
}

// Scenario: 唯一平台的精确别名可路由
func TestResolveCompositeModelOwnershipKeepsProviderAccountsIsolated(t *testing.T) {
	groupID := int64(7)
	repo := &compositeOwnershipAccountRepo{
		accounts: []Account{
			{
				ID:       1,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"model_mapping": map[string]any{"gpt-public": "gpt-5"},
				},
			},
			{
				ID:       2,
				Platform: PlatformDeepseek,
				Credentials: map[string]any{
					"model_mapping": map[string]any{"reasoning-alias": "deepseek-v4-pro"},
				},
			},
		},
	}
	svc := &GatewayService{accountRepo: repo}

	deepSeekOwnership, err := svc.resolveCompositeModelOwnership(context.Background(), groupID, "reasoning-alias")
	require.NoError(t, err)
	require.Equal(t, CompositeModelOwnership{
		TargetPlatform: PlatformDeepseek,
		Matched:        true,
		Platforms:      []string{PlatformDeepseek},
		Claimants: []CompositeOwnershipClaimant{
			{AccountID: 2, Platform: PlatformDeepseek},
		},
	}, deepSeekOwnership)

	openAIOwnership, err := svc.resolveCompositeModelOwnership(context.Background(), groupID, "gpt-public")
	require.NoError(t, err)
	require.Equal(t, CompositeModelOwnership{
		TargetPlatform: PlatformOpenAI,
		Matched:        true,
		Platforms:      []string{PlatformOpenAI},
		Claimants: []CompositeOwnershipClaimant{
			{AccountID: 1, Platform: PlatformOpenAI},
		},
	}, openAIOwnership)
}

// Scenario: 通配符和空映射不声明所有权
func TestResolveCompositeModelOwnershipRequiresNonEmptyExactMappings(t *testing.T) {
	groupID := int64(7)
	repo := &compositeOwnershipAccountRepo{
		accounts: []Account{
			{
				ID:       1,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"model_mapping": map[string]any{"*": "gpt-5", "gpt-*": "gpt-5", "empty-alias": ""},
				},
			},
			{
				ID:       2,
				Platform: PlatformGrok,
				Credentials: map[string]any{
					"model_mapping": map[string]any{"grok-public": "grok-4"},
				},
			},
		},
	}
	svc := &GatewayService{accountRepo: repo}

	for _, model := range []string{"gpt-5", "empty-alias", "unknown-alias"} {
		ownership, err := svc.resolveCompositeModelOwnership(context.Background(), groupID, model)
		require.NoError(t, err)
		require.Equal(t, CompositeModelOwnership{Platforms: []string{}}, ownership, "model=%s", model)
	}

	ownership, err := svc.resolveCompositeModelOwnership(context.Background(), groupID, "grok-public")
	require.NoError(t, err)
	require.Equal(t, CompositeModelOwnership{
		TargetPlatform: PlatformGrok,
		Matched:        true,
		Platforms:      []string{PlatformGrok},
		Claimants: []CompositeOwnershipClaimant{
			{AccountID: 2, Platform: PlatformGrok},
		},
	}, ownership)
}

func TestResolveCompositeModelOwnershipAllowsSamePlatformAndRejectsCrossPlatformAliases(t *testing.T) {
	groupID := int64(7)
	repo := &compositeOwnershipAccountRepo{
		accounts: []Account{
			{ID: 1, Platform: PlatformOpenAI, Credentials: map[string]any{"model_mapping": map[string]any{"shared-openai": "gpt-5", "ambiguous": "gpt-5"}}},
			{ID: 2, Platform: PlatformOpenAI, Credentials: map[string]any{"model_mapping": map[string]any{"shared-openai": "gpt-5.1"}}},
			{ID: 3, Platform: PlatformDeepseek, Credentials: map[string]any{"model_mapping": map[string]any{"ambiguous": "deepseek-v4-pro"}}},
		},
	}
	svc := &GatewayService{accountRepo: repo}

	samePlatform, err := svc.resolveCompositeModelOwnership(context.Background(), groupID, "shared-openai")
	require.NoError(t, err)
	require.Equal(t, CompositeModelOwnership{
		TargetPlatform: PlatformOpenAI,
		Matched:        true,
		Platforms:      []string{PlatformOpenAI},
		Claimants: []CompositeOwnershipClaimant{
			{AccountID: 1, Platform: PlatformOpenAI},
			{AccountID: 2, Platform: PlatformOpenAI},
		},
	}, samePlatform)

	ambiguous, err := svc.resolveCompositeModelOwnership(context.Background(), groupID, "ambiguous")
	require.NoError(t, err)
	require.Equal(t, CompositeModelOwnership{
		Ambiguous: true,
		Platforms: []string{PlatformDeepseek, PlatformOpenAI},
		Claimants: []CompositeOwnershipClaimant{
			{AccountID: 1, Platform: PlatformOpenAI},
			{AccountID: 3, Platform: PlatformDeepseek},
		},
	}, ambiguous)
}

// Scenario: 只有 OpenAI 网关族的 OAuth 声明方会被标记（API Key 上游可能兼容多协议，
// Anthropic / Antigravity 的 OAuth 在默认 Anthropic handler 中可正常转发）
func TestResolveCompositeModelOwnershipFlagsOpenAIGatewayOAuthClaimants(t *testing.T) {
	groupID := int64(7)
	repo := &compositeOwnershipAccountRepo{
		accounts: []Account{
			{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"model_mapping": map[string]any{"gpt-6-astra": "gpt-6-astra"}}},
			{ID: 2, Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Credentials: map[string]any{"model_mapping": map[string]any{"gpt-6-astra": "deepseek-v4-pro", "apikey-only": "deepseek-v4-pro"}}},
			{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"model_mapping": map[string]any{"apikey-only": "gpt-5"}}},
			// Antigravity 账号创建时会把默认映射（含 claude-*）写入 model_mapping。
			{ID: 4, Platform: PlatformAntigravity, Type: AccountTypeOAuth, Credentials: map[string]any{"model_mapping": map[string]any{"claude-opus-4-8": "claude-opus-4-8"}}},
			{ID: 5, Platform: PlatformAnthropic, Type: AccountTypeSetupToken, Credentials: map[string]any{"model_mapping": map[string]any{"claude-opus-4-8": "claude-opus-4-8"}}},
		},
	}
	svc := &GatewayService{accountRepo: repo}

	withOAuth, err := svc.resolveCompositeModelOwnership(context.Background(), groupID, "gpt-6-astra")
	require.NoError(t, err)
	require.Equal(t, CompositeModelOwnership{
		Ambiguous: true,
		Platforms: []string{PlatformDeepseek, PlatformOpenAI},
		Claimants: []CompositeOwnershipClaimant{
			{AccountID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth},
			{AccountID: 2, Platform: PlatformDeepseek, Type: AccountTypeAPIKey},
		},
	}, withOAuth)

	apiKeyOnly, err := svc.resolveCompositeModelOwnership(context.Background(), groupID, "apikey-only")
	require.NoError(t, err)
	require.Equal(t, CompositeModelOwnership{
		Ambiguous: true,
		Platforms: []string{PlatformDeepseek, PlatformOpenAI},
		Claimants: []CompositeOwnershipClaimant{
			{AccountID: 2, Platform: PlatformDeepseek, Type: AccountTypeAPIKey},
			{AccountID: 3, Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
		},
	}, apiKeyOnly)

	anthropicFamily, err := svc.resolveCompositeModelOwnership(context.Background(), groupID, "claude-opus-4-8")
	require.NoError(t, err)
	require.Equal(t, CompositeModelOwnership{
		Ambiguous: true,
		Platforms: []string{PlatformAnthropic, PlatformAntigravity},
		Claimants: []CompositeOwnershipClaimant{
			{AccountID: 4, Platform: PlatformAntigravity, Type: AccountTypeOAuth},
			{AccountID: 5, Platform: PlatformAnthropic, Type: AccountTypeSetupToken},
		},
	}, anthropicFamily)
}

func TestNewGatewayServiceWiresCompositeModelOwnershipResolver(t *testing.T) {
	groupID := int64(7)
	repo := &compositeOwnershipAccountRepo{
		accounts: []Account{{
			ID:          1,
			Platform:    PlatformDeepseek,
			Credentials: map[string]any{"model_mapping": map[string]any{"reasoning-alias": "deepseek-v4-pro"}},
		}},
	}
	resolver := NewCompositeRouteResolver(nil)
	svc := NewGatewayService(
		repo,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		resolver,
		nil,
		nil, nil, /* cursorExecution */
	)
	require.Same(t, resolver, svc.compositeResolver)

	decision, err := resolver.Resolve(context.Background(), groupID, "reasoning-alias", CompositeRouteEndpointResponses)
	require.NoError(t, err)
	require.True(t, decision.Matched)
	require.Equal(t, CompositeRouteSourceAccount, decision.Source)
	require.Equal(t, PlatformDeepseek, decision.TargetPlatform)
}

func TestDetectModelPlatform(t *testing.T) {
	tests := []struct {
		name     string
		model    string
		platform string
		ok       bool
	}{
		{name: "claude", model: "claude-sonnet-4-5", platform: PlatformAnthropic, ok: true},
		{name: "anthropic prefix", model: "anthropic/claude-opus-4-5", platform: PlatformAnthropic, ok: true},
		{name: "gpt", model: "gpt-5.1", platform: PlatformOpenAI, ok: true},
		{name: "o series", model: "o3-mini", platform: PlatformOpenAI, ok: true},
		{name: "embedding", model: "text-embedding-3-large", platform: PlatformOpenAI, ok: true},
		{name: "gemini", model: "gemini-3-pro", platform: PlatformGemini, ok: true},
		{name: "gemini models prefix", model: "models/gemini-2.5-flash", platform: PlatformGemini, ok: true},
		{name: "learnlm", model: "learnlm-2.0-flash-experimental", platform: PlatformGemini, ok: true},
		{name: "grok", model: "grok-4", platform: PlatformGrok, ok: true},
		{name: "xai prefix", model: "xai/grok-4", platform: PlatformGrok, ok: true},
		{name: "kimi", model: "kimi-k2-thinking", platform: PlatformKimi, ok: true},
		{name: "kimi code bare k3", model: "K3", platform: PlatformKimi, ok: true},
		{name: "kimi code bare k3 256k", model: "k3-256k", platform: PlatformKimi, ok: true},
		{name: "kimi code provider prefix", model: "kimi-code/k3", platform: PlatformKimi, ok: true},
		{name: "moonshot prefix", model: "moonshot/moonshot-v1-32k", platform: PlatformKimi, ok: true},
		{name: "zhipu", model: "glm-5.2", platform: PlatformZhipu, ok: true},
		{name: "deepseek", model: "deepseek-v4-pro", platform: PlatformDeepseek, ok: true},
		// muse 前缀归属 PlatformMuse（方案 QKG-C2，C3 模块的 muse 家族判定补充）。
		{name: "muse spark 1.3", model: "muse-spark-1.3", platform: PlatformMuse, ok: true},
		{name: "muse spark 1.2", model: "muse-spark-1.2", platform: PlatformMuse, ok: true},
		// 前缀必须位于开头：非开头匹配一律不可检测。
		{name: "muse not at start", model: "x-muse-foo", ok: false},
		{name: "minimax", model: "MiniMax-M3", platform: PlatformMiniMax, ok: true},
		{name: "minimax prefix", model: "minimax/MiniMax-M2.5", platform: PlatformMiniMax, ok: true},
		{name: "abab legacy", model: "abab6.5-chat", platform: PlatformMiniMax, ok: true},
		{name: "abab7 legacy", model: "abab7-chat-preview", platform: PlatformMiniMax, ok: true},
		{name: "jev", model: "jev-latest", platform: PlatformTypeSafe, ok: true},
		{name: "typesafe prefix", model: "typesafe/jev-latest", platform: PlatformTypeSafe, ok: true},
		{name: "abab unrelated namespace", model: "abab-other", ok: false},
		{name: "unknown k3 alias", model: "k3-preview", ok: false},
		// space-bunny 是 CodeBuddy 平台的公开模型 ID，无平台词根前缀，只能精确识别
		// （方案 composite-codebuddy-support-plan.md §改动清单 2，用户裁定「必须自动生效」）。
		{name: "space-bunny", model: "space-bunny", platform: PlatformCodeBuddy, ok: true},
		{name: "space-bunny case insensitive", model: "SPACE-BUNNY", platform: PlatformCodeBuddy, ok: true},
		{name: "space-bunny models prefix", model: "models/space-bunny", platform: PlatformCodeBuddy, ok: true},
		// 刻意不做前缀通配：下列近似 ID 必须仍不可检测，避免误吞未知模型。
		{name: "space-bunner near miss", model: "space-bunner", ok: false},
		{name: "space-bunny variant suffix", model: "space-bunny-2", ok: false},
		{name: "space-bunny prefix only", model: "space-", ok: false},
		{name: "unknown", model: "llama-4-maverick", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			platform, ok := DetectModelPlatform(tt.model)
			require.Equal(t, tt.ok, ok)
			require.Equal(t, tt.platform, platform)
		})
	}
}

func TestQuotaPlatformCompositeUsesResolvedOrForceOnly(t *testing.T) {
	apiKey := &APIKey{Group: &Group{Platform: PlatformComposite}}

	require.Equal(t, "", QuotaPlatform(context.Background(), apiKey))
	require.Equal(t, PlatformGemini, QuotaPlatform(WithResolvedTargetPlatform(context.Background(), PlatformGemini), apiKey))
	require.Equal(t, PlatformAntigravity, QuotaPlatform(context.WithValue(context.Background(), ctxkey.ForcePlatform, PlatformAntigravity), apiKey))

	ctx := WithResolvedTargetPlatform(context.Background(), PlatformAnthropic)
	ctx = context.WithValue(ctx, ctxkey.ForcePlatform, PlatformAntigravity)
	require.Equal(t, PlatformAntigravity, QuotaPlatform(ctx, apiKey))
}

// TestCompositeGroupSchedulerHasAllCanonicalPlatformBuckets 校验 schedulerCanonicalBuckets
// 返回的规范桶集合覆盖全部候选调度平台。
//
// 契约（方案 v15 §2.1-3）：聚合分组（deepseek/zhipu/kimi/minimax/other）的 bucket 平台键
// 追加 @agg<version> 标签（version = schedulerSnapshotAggregationVersion），使聚合规则变更
// （version bump）时旧分桶缓存键自然失效；codebuddy 自身与 anthropic/gemini/openai/antigravity/
// grok 不加标签，维持裸平台名。本测试按此契约断言，不得硬编码 @agg1，版本 bump 时须仍生效。
func TestCompositeGroupSchedulerHasAllCanonicalPlatformBuckets(t *testing.T) {
	seen := make(map[string]struct{})
	for _, bucket := range schedulerCanonicalBuckets(99) {
		seen[bucket.Platform] = struct{}{}
	}

	// 非聚合平台维持裸名（codebuddy 自身与 anthropic/gemini/openai/antigravity/grok 不加标签）。
	//
	// 合并上游 v0.2.15 后候选调度平台经 domain SSOT（domain.CompositePrecedencePlatformIDs）
	// 派生并追加我方 other（见 scheduler_snapshot_service.go）：上游新增
	// opencode_go/typesafe/command_code/cline 与我方自研 codebuddy/muse/cursor 均为裸名桶键，
	// 须一并纳入期望集合（消费者核对见本卡 notes「跨批依赖」）。
	nonAggPlatforms := []string{
		PlatformAnthropic, PlatformGemini, PlatformOpenAI, PlatformAntigravity, PlatformGrok,
		PlatformOpenCodeGo, PlatformTypeSafe, PlatformCommandCode, PlatformCline,
		PlatformCodeBuddy, PlatformMuse, PlatformCursor, PlatformDevin,
	}
	// 聚合族五平台：@agg 标签已废弃，平台键原样返回。
	aggPlatforms := []string{
		PlatformDeepseek, PlatformZhipu, PlatformKimi, PlatformOther, PlatformMiniMax,
	}
	expected := make([]string, 0, len(nonAggPlatforms)+len(aggPlatforms))
	expected = append(expected, nonAggPlatforms...)
	expected = append(expected, aggPlatforms...)

	platforms := make([]string, 0, len(seen))
	for platform := range seen {
		platforms = append(platforms, platform)
	}
	require.ElementsMatch(t, expected, platforms)
}

func TestCompositeConcretePlatformsIncludeCNProviders(t *testing.T) {
	for _, platform := range []string{PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo, PlatformTypeSafe, PlatformCommandCode, PlatformCline} {
		require.True(t, isConcreteRequestPlatform(platform))
		require.True(t, canCopyAccountsFromGroupPlatform(PlatformComposite, platform))
	}
}

// TestCompositeRouteDecisionContextRoundTrip 校验 WithCompositeRouteDecision 盖章的
// 完整 decision 可经 CompositeRouteDecisionFromContext 原样取回，无 decision 的 ctx 返回 false。
func TestCompositeRouteDecisionContextRoundTrip(t *testing.T) {
	decision := CompositeRouteDecision{
		Matched:         true,
		Source:          CompositeRouteSourceDetector,
		Reason:          CompositeRouteReasonAmbiguous,
		TargetPlatform:  PlatformOpenAI,
		SupplyPlatforms: []string{PlatformZhipu, PlatformCursor},
	}
	ctx := WithCompositeRouteDecision(context.Background(), decision)

	got, ok := CompositeRouteDecisionFromContext(ctx)
	require.True(t, ok)
	require.Equal(t, decision.SupplyPlatforms, got.SupplyPlatforms)
	require.Equal(t, decision.Reason, got.Reason)
	require.Equal(t, decision.TargetPlatform, got.TargetPlatform)

	_, ok = CompositeRouteDecisionFromContext(context.Background())
	require.False(t, ok)

	// Matched=false 不盖章，取回仍应为 false。
	_, ok = CompositeRouteDecisionFromContext(WithCompositeRouteDecision(context.Background(), CompositeRouteDecision{}))
	require.False(t, ok)
}

// TestCompositeSupplyAllowsPlatform 校验歧义场景按 SupplyPlatforms 求交（不得用
// TargetPlatform 兜底），具体场景按 TargetPlatform 判定，空 decision 失败关闭。
func TestCompositeSupplyAllowsPlatform(t *testing.T) {
	ambiguous := CompositeRouteDecision{
		Matched:         true,
		Reason:          CompositeRouteReasonAmbiguous,
		TargetPlatform:  PlatformOpenAI,
		SupplyPlatforms: []string{PlatformZhipu, PlatformCursor},
	}
	require.True(t, CompositeSupplyAllowsPlatform(ambiguous, PlatformOpenAI, PlatformZhipu, PlatformCursor))
	require.False(t, CompositeSupplyAllowsPlatform(ambiguous, PlatformOpenAI))

	concrete := CompositeRouteDecision{Matched: true, TargetPlatform: PlatformOpenAI}
	require.True(t, CompositeSupplyAllowsPlatform(concrete, PlatformOpenAI))
	require.False(t, CompositeSupplyAllowsPlatform(concrete, PlatformZhipu))

	empty := CompositeRouteDecision{}
	require.False(t, CompositeSupplyAllowsPlatform(empty, PlatformOpenAI))

	// 空 allowed 一律失败关闭。
	require.False(t, CompositeSupplyAllowsPlatform(ambiguous))
	require.False(t, CompositeSupplyAllowsPlatform(concrete))
}

// TestCompositeDecisionHelpers 校验 SupplyExists / Ambiguous 的边界条件。
func TestCompositeDecisionHelpers(t *testing.T) {
	// SupplyExists 等价于 Matched。
	require.False(t, CompositeDecisionSupplyExists(CompositeRouteDecision{}))
	require.True(t, CompositeDecisionSupplyExists(CompositeRouteDecision{Matched: true}))

	// Ambiguous：Matched=false。
	require.False(t, CompositeDecisionAmbiguous(CompositeRouteDecision{
		Reason:          CompositeRouteReasonAmbiguous,
		SupplyPlatforms: []string{PlatformZhipu},
	}))
	// Ambiguous：SupplyPlatforms 空。
	require.False(t, CompositeDecisionAmbiguous(CompositeRouteDecision{
		Matched: true,
		Reason:  CompositeRouteReasonAmbiguous,
	}))
	// Ambiguous：Reason 未设置（非 CompositeRouteReasonAmbiguous）。
	require.False(t, CompositeDecisionAmbiguous(CompositeRouteDecision{
		Matched:         true,
		Reason:          "",
		SupplyPlatforms: []string{PlatformZhipu},
	}))
	// Ambiguous：全部满足。
	require.True(t, CompositeDecisionAmbiguous(CompositeRouteDecision{
		Matched:         true,
		Reason:          CompositeRouteReasonAmbiguous,
		SupplyPlatforms: []string{PlatformZhipu},
	}))
}
