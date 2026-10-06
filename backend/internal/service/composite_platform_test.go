package service

import (
	"context"
	"strconv"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

type compositeOwnershipAccountRepo struct {
	AccountRepository
	accounts []Account
}

func (r *compositeOwnershipAccountRepo) ListSchedulableByGroupID(context.Context, int64) ([]Account, error) {
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
	require.Equal(t, CompositeModelOwnership{TargetPlatform: PlatformDeepseek, Matched: true}, deepSeekOwnership)

	openAIOwnership, err := svc.resolveCompositeModelOwnership(context.Background(), groupID, "gpt-public")
	require.NoError(t, err)
	require.Equal(t, CompositeModelOwnership{TargetPlatform: PlatformOpenAI, Matched: true}, openAIOwnership)
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
		require.Equal(t, CompositeModelOwnership{}, ownership, "model=%s", model)
	}

	ownership, err := svc.resolveCompositeModelOwnership(context.Background(), groupID, "grok-public")
	require.NoError(t, err)
	require.Equal(t, CompositeModelOwnership{TargetPlatform: PlatformGrok, Matched: true}, ownership)
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
	require.Equal(t, CompositeModelOwnership{TargetPlatform: PlatformOpenAI, Matched: true}, samePlatform)

	ambiguous, err := svc.resolveCompositeModelOwnership(context.Background(), groupID, "ambiguous")
	require.NoError(t, err)
	require.Equal(t, CompositeModelOwnership{Ambiguous: true}, ambiguous)
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
		nil,
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
		{name: "minimax", model: "MiniMax-M3", platform: PlatformMiniMax, ok: true},
		{name: "minimax prefix", model: "minimax/MiniMax-M2.5", platform: PlatformMiniMax, ok: true},
		{name: "abab legacy", model: "abab6.5-chat", platform: PlatformMiniMax, ok: true},
		{name: "abab7 legacy", model: "abab7-chat-preview", platform: PlatformMiniMax, ok: true},
		{name: "abab unrelated namespace", model: "abab-other", ok: false},
		{name: "unknown k3 alias", model: "k3-preview", ok: false},
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
	nonAggPlatforms := []string{
		PlatformAnthropic, PlatformGemini, PlatformOpenAI, PlatformAntigravity, PlatformGrok,
	}
	// 聚合族五平台：裸名 + "@agg" + schedulerSnapshotAggregationVersion（用常量拼，不硬编码版本号）。
	aggPlatforms := []string{
		PlatformDeepseek, PlatformZhipu, PlatformKimi, PlatformOther, PlatformMiniMax,
	}
	expected := make([]string, 0, len(nonAggPlatforms)+len(aggPlatforms))
	expected = append(expected, nonAggPlatforms...)
	for _, p := range aggPlatforms {
		expected = append(expected, p+"@agg"+strconv.Itoa(schedulerSnapshotAggregationVersion))
	}

	platforms := make([]string, 0, len(seen))
	for platform := range seen {
		platforms = append(platforms, platform)
	}
	require.ElementsMatch(t, expected, platforms)

	// 防回归：聚合族裸平台名不得出现在任何 bucket（标签缺失即回退到无标签键，违反 §2.1-3）。
	for _, p := range aggPlatforms {
		_, ok := seen[p]
		require.False(t, ok, "聚合族裸平台名 %q 不应出现在 bucket（§2.1-3 契约要求 @agg 标签）", p)
	}
}

func TestCompositeConcretePlatformsIncludeCNProviders(t *testing.T) {
	for _, platform := range []string{PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax} {
		require.True(t, isConcreteRequestPlatform(platform))
		require.True(t, canCopyAccountsFromGroupPlatform(PlatformComposite, platform))
	}
}
