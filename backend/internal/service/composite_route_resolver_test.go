package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type compositeRouteRepoStub struct {
	routes []CompositeModelRoute
}

func TestCompositeRouteResolverCodexFilteringMatchesProductionPriority(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		routes []CompositeModelRoute
		want   []string
	}{
		{
			name: "endpoint beats priority",
			routes: []CompositeModelRoute{
				{ID: 1, PublicModel: "alias", MatchType: CompositeRouteMatchExact, Endpoint: CompositeRouteEndpointAny, TargetPlatform: PlatformOpenAI, Priority: 1},
				{ID: 2, PublicModel: "alias", MatchType: CompositeRouteMatchExact, Endpoint: CompositeRouteEndpointResponses, TargetPlatform: PlatformTypeSafe, Priority: 100},
			},
			want: []string{"unrouted"},
		},
		{
			name: "Responses OpenAI overrides any TypeSafe",
			routes: []CompositeModelRoute{
				{ID: 1, PublicModel: "alias", MatchType: CompositeRouteMatchExact, Endpoint: CompositeRouteEndpointAny, TargetPlatform: PlatformTypeSafe, Priority: 1},
				{ID: 2, PublicModel: "alias", MatchType: CompositeRouteMatchExact, Endpoint: CompositeRouteEndpointResponses, TargetPlatform: PlatformOpenAI, Priority: 100},
			},
			want: []string{"alias", "unrouted"},
		},
		{
			name: "longer prefix beats priority",
			routes: []CompositeModelRoute{
				{ID: 1, PublicModel: "a", MatchType: CompositeRouteMatchPrefix, TargetPlatform: PlatformOpenAI, Priority: 1},
				{ID: 2, PublicModel: "ali", MatchType: CompositeRouteMatchPrefix, TargetPlatform: PlatformTypeSafe, Priority: 100},
			},
			want: []string{"unrouted"},
		},
		{
			name: "exact beats endpoint prefix",
			routes: []CompositeModelRoute{
				{ID: 1, PublicModel: "alias", MatchType: CompositeRouteMatchPrefix, Endpoint: CompositeRouteEndpointResponses, TargetPlatform: PlatformTypeSafe},
				{ID: 2, PublicModel: "alias", MatchType: CompositeRouteMatchExact, Endpoint: CompositeRouteEndpointAny, TargetPlatform: PlatformOpenAI},
			},
			want: []string{"alias", "unrouted"},
		},
		{
			name: "lower priority wins",
			routes: []CompositeModelRoute{
				{ID: 1, PublicModel: "alias", MatchType: CompositeRouteMatchExact, TargetPlatform: PlatformTypeSafe, Priority: 100},
				{ID: 2, PublicModel: "alias", MatchType: CompositeRouteMatchExact, TargetPlatform: PlatformOpenAI, Priority: 1},
			},
			want: []string{"alias", "unrouted"},
		},
		{
			name: "lower ID breaks tie",
			routes: []CompositeModelRoute{
				{ID: 2, PublicModel: "alias", MatchType: CompositeRouteMatchExact, TargetPlatform: PlatformOpenAI},
				{ID: 1, PublicModel: "alias", MatchType: CompositeRouteMatchExact, TargetPlatform: PlatformTypeSafe},
			},
			want: []string{"unrouted"},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			for index := range scenario.routes {
				scenario.routes[index].Enabled = true
				scenario.routes[index].GroupID = 7
			}
			resolver := NewCompositeRouteResolver(compositeRouteRepoStub{routes: scenario.routes})
			models, err := resolver.FilterCodexModels(context.Background(), 7, []string{"alias", "unrouted"})
			require.NoError(t, err)
			require.Equal(t, scenario.want, models)
			decision, err := resolver.Resolve(context.Background(), 7, "alias", CompositeRouteEndpointResponses)
			require.NoError(t, err)
			require.True(t, decision.Matched)
			require.Equal(t, decision.TargetPlatform != PlatformTypeSafe, len(models) == 2)
			require.Equal(t, []string{decision.TargetPlatform}, decision.SupplyPlatforms)
			exactModels, err := resolver.ListExactPublicModels(context.Background(), 7, CompositeRouteEndpointResponses, false)
			require.NoError(t, err)
			if decision.TargetPlatform == PlatformTypeSafe {
				require.Empty(t, exactModels)
			} else {
				require.Contains(t, exactModels, "alias")
			}
		})
	}
}

func (s compositeRouteRepoStub) ListByGroup(ctx context.Context, groupID int64, includeDisabled bool) ([]CompositeModelRoute, error) {
	routes := make([]CompositeModelRoute, 0, len(s.routes))
	for _, route := range s.routes {
		if route.GroupID != groupID {
			continue
		}
		if !includeDisabled && !route.Enabled {
			continue
		}
		routes = append(routes, route)
	}
	return routes, nil
}

func (s compositeRouteRepoStub) Create(ctx context.Context, route *CompositeModelRoute) error {
	return nil
}

func (s compositeRouteRepoStub) Update(ctx context.Context, route *CompositeModelRoute) error {
	return nil
}

func (s compositeRouteRepoStub) Delete(ctx context.Context, id int64) error {
	return nil
}

func (s compositeRouteRepoStub) DeleteByGroup(ctx context.Context, groupID int64) error {
	return nil
}

func TestCompositeRouteResolverExplicitExactRouteRewritesModel(t *testing.T) {
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{
		routes: []CompositeModelRoute{
			{
				ID:             10,
				GroupID:        7,
				PublicModel:    "openrouter/gpt-5",
				MatchType:      CompositeRouteMatchExact,
				TargetPlatform: PlatformOpenAI,
				UpstreamModel:  "gpt-5",
				Endpoint:       CompositeRouteEndpointAny,
				Priority:       100,
				Enabled:        true,
			},
		},
	})

	decision, err := resolver.Resolve(context.Background(), 7, "openrouter/gpt-5", CompositeRouteEndpointChatCompletions)

	require.NoError(t, err)
	require.True(t, decision.Matched)
	require.Equal(t, CompositeRouteSourceExplicit, decision.Source)
	require.Equal(t, PlatformOpenAI, decision.TargetPlatform)
	require.Equal(t, "gpt-5", decision.UpstreamModel)
	require.NotNil(t, decision.Route)
	require.Equal(t, int64(10), decision.Route.ID)
	require.Equal(t, []string{PlatformOpenAI}, decision.SupplyPlatforms)
	require.Equal(t, CompositeChainOpenAI, decision.ServiceChain)
}

// Scenario: 唯一平台的精确别名可路由
func TestCompositeRouteResolverUsesAccountModelOwnershipForUnprefixedAlias(t *testing.T) {
	resolver := NewCompositeRouteResolver(nil)
	resolver.SetModelOwnershipResolver(func(_ context.Context, groupID int64, model string) (CompositeModelOwnership, error) {
		require.Equal(t, int64(7), groupID)
		require.Equal(t, "reasoning-alias", model)
		return CompositeModelOwnership{
			TargetPlatform: PlatformDeepseek,
			Matched:        true,
			Platforms:      []string{PlatformDeepseek},
			Claimants: []CompositeOwnershipClaimant{
				{AccountID: 1, Platform: PlatformDeepseek, Type: AccountTypeAPIKey},
			},
		}, nil
	})

	decision, err := resolver.Resolve(context.Background(), 7, "reasoning-alias", CompositeRouteEndpointChatCompletions)

	require.NoError(t, err)
	require.True(t, decision.Matched)
	require.Equal(t, CompositeRouteSourceAccount, decision.Source)
	require.Equal(t, PlatformDeepseek, decision.TargetPlatform)
	require.Equal(t, "reasoning-alias", decision.UpstreamModel)
	require.Equal(t, []string{PlatformDeepseek}, decision.SupplyPlatforms)
	require.Equal(t, CompositeChainOpenAI, decision.ServiceChain)
	require.Len(t, decision.SupplyAccounts, 1)
	require.Equal(t, PlatformDeepseek, decision.SupplyAccounts[0].Platform)
}

func TestCompositeRouteResolverAccountOwnershipOverridesBuiltInDetector(t *testing.T) {
	resolver := NewCompositeRouteResolver(nil)
	resolver.SetModelOwnershipResolver(func(context.Context, int64, string) (CompositeModelOwnership, error) {
		return CompositeModelOwnership{
			TargetPlatform: PlatformDeepseek,
			Matched:        true,
			Platforms:      []string{PlatformDeepseek},
			Claimants: []CompositeOwnershipClaimant{
				{AccountID: 1, Platform: PlatformDeepseek, Type: AccountTypeAPIKey},
			},
		}, nil
	})

	decision, err := resolver.Resolve(context.Background(), 7, "gpt-5", CompositeRouteEndpointResponses)

	require.NoError(t, err)
	require.True(t, decision.Matched)
	require.Equal(t, CompositeRouteSourceAccount, decision.Source)
	require.Equal(t, PlatformDeepseek, decision.TargetPlatform)
	require.Equal(t, []string{PlatformDeepseek}, decision.SupplyPlatforms)
	require.Equal(t, CompositeChainOpenAI, decision.ServiceChain)
}

// Scenario: 显式路由保持最高优先级
func TestCompositeRouteResolverExplicitRouteBeatsAccountOwnership(t *testing.T) {
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{
		routes: []CompositeModelRoute{{
			ID:             10,
			GroupID:        7,
			PublicModel:    "reasoning-alias",
			MatchType:      CompositeRouteMatchExact,
			TargetPlatform: PlatformOpenAI,
			UpstreamModel:  "gpt-5",
			Endpoint:       CompositeRouteEndpointAny,
			Enabled:        true,
		}},
	})
	resolver.SetModelOwnershipResolver(func(context.Context, int64, string) (CompositeModelOwnership, error) {
		return CompositeModelOwnership{TargetPlatform: PlatformDeepseek, Matched: true}, nil
	})

	decision, err := resolver.Resolve(context.Background(), 7, "reasoning-alias", CompositeRouteEndpointChatCompletions)

	require.NoError(t, err)
	require.True(t, decision.Matched)
	require.Equal(t, CompositeRouteSourceExplicit, decision.Source)
	require.Equal(t, PlatformOpenAI, decision.TargetPlatform)
	require.Equal(t, "gpt-5", decision.UpstreamModel)
	require.Equal(t, []string{PlatformOpenAI}, decision.SupplyPlatforms)
}

// Scenario: 同链多平台歧义放行选号（全部平台由 OpenAI 网关服务）
func TestCompositeRouteResolverDoesNotGuessAmbiguousAccountOwnership(t *testing.T) {
	resolver := NewCompositeRouteResolver(nil)
	resolver.SetModelOwnershipResolver(func(context.Context, int64, string) (CompositeModelOwnership, error) {
		return CompositeModelOwnership{
			Ambiguous: true,
			Platforms: []string{PlatformZhipu, PlatformCursor},
			Claimants: []CompositeOwnershipClaimant{
				{AccountID: 1, Platform: PlatformZhipu, Type: AccountTypeAPIKey},
				{AccountID: 2, Platform: PlatformCursor, Type: AccountTypeOAuth, CursorConnect: true},
			},
		}, nil
	})

	decision, err := resolver.Resolve(context.Background(), 7, "shared-alias", CompositeRouteEndpointChatCompletions)

	require.NoError(t, err)
	require.True(t, decision.Matched)
	require.Equal(t, CompositeRouteSourceAccount, decision.Source)
	require.Equal(t, PlatformOpenAI, decision.TargetPlatform)
	require.Equal(t, "shared-alias", decision.UpstreamModel)
	require.Equal(t, []string{PlatformCursor, PlatformZhipu}, decision.SupplyPlatforms)
	require.Equal(t, []string{CompositeChainOpenAI}, decision.QualifyingChains)
	require.Equal(t, CompositeChainOpenAI, decision.ServiceChain)
	require.Len(t, decision.SupplyAccounts, 2)
	require.Equal(t, CompositeRouteReasonAmbiguous, decision.Reason)
}

// Scenario: 多链 chat（zhipu 走 openai 链，anthropic 走 anthropic 链）按端点入口放行
func TestCompositeRouteResolverCrossChainAmbiguousOwnership(t *testing.T) {
	resolver := NewCompositeRouteResolver(nil)
	resolver.SetModelOwnershipResolver(func(context.Context, int64, string) (CompositeModelOwnership, error) {
		return CompositeModelOwnership{
			Ambiguous: true,
			Platforms: []string{PlatformZhipu, PlatformAnthropic},
			Claimants: []CompositeOwnershipClaimant{
				{AccountID: 1, Platform: PlatformZhipu, Type: AccountTypeAPIKey},
				{AccountID: 2, Platform: PlatformAnthropic, Type: AccountTypeAPIKey},
			},
		}, nil
	})

	decision, err := resolver.Resolve(context.Background(), 7, "shared-alias", CompositeRouteEndpointChatCompletions)

	require.NoError(t, err)
	require.True(t, decision.Matched, "混链按端点入口放行选号，不再阶段失败关闭")
	require.Empty(t, decision.TargetPlatform)
	require.Equal(t, []string{PlatformAnthropic, PlatformZhipu}, decision.SupplyPlatforms)
	require.Equal(t, []string{CompositeChainAnthropic, CompositeChainOpenAI}, decision.QualifyingChains)
	require.Equal(t, CompositeChainAnthropic, decision.ServiceChain, "chat 端点 eligible 链含 anthropic，入口为 anthropic")
	require.Len(t, decision.SupplyAccounts, 2)
	require.Equal(t, CompositeRouteReasonAmbiguous, decision.Reason)
	require.Equal(t, "model is exposed by multiple provider platforms", decision.Reason)
}

// Scenario: 多链 chat（zhipu 走 openai 链，gemini 走 anthropic 链）入口改走 anthropic，
// 避免落入未装配 gemini 转发服务的 OpenAI handler。
func TestCompositeRouteResolverCrossChainChatWithGeminiEntersAnthropic(t *testing.T) {
	resolver := NewCompositeRouteResolver(nil)
	resolver.SetModelOwnershipResolver(func(context.Context, int64, string) (CompositeModelOwnership, error) {
		return CompositeModelOwnership{
			Ambiguous: true,
			Platforms: []string{PlatformZhipu, PlatformGemini},
			Claimants: []CompositeOwnershipClaimant{
				{AccountID: 1, Platform: PlatformZhipu, Type: AccountTypeAPIKey},
				{AccountID: 2, Platform: PlatformGemini, Type: AccountTypeAPIKey},
			},
		}, nil
	})

	decision, err := resolver.Resolve(context.Background(), 7, "shared-alias", CompositeRouteEndpointChatCompletions)

	require.NoError(t, err)
	require.True(t, decision.Matched)
	require.Empty(t, decision.TargetPlatform)
	require.Equal(t, []string{PlatformGemini, PlatformZhipu}, decision.SupplyPlatforms)
	require.Equal(t, []string{CompositeChainAnthropic, CompositeChainOpenAI}, decision.QualifyingChains)
	require.Equal(t, CompositeChainAnthropic, decision.ServiceChain, "chat 端点 eligible 链含 gemini(anthropic)，入口为 anthropic")
	require.Len(t, decision.SupplyAccounts, 2)
	require.Equal(t, CompositeRouteReasonAmbiguous, decision.Reason)
}

// Scenario: 多链 responses（zhipu 走 openai 链，anthropic 走 anthropic 链）入口改走 anthropic。
func TestCompositeRouteResolverCrossChainResponsesEntersAnthropic(t *testing.T) {
	resolver := NewCompositeRouteResolver(nil)
	resolver.SetModelOwnershipResolver(func(context.Context, int64, string) (CompositeModelOwnership, error) {
		return CompositeModelOwnership{
			Ambiguous: true,
			Platforms: []string{PlatformZhipu, PlatformAnthropic},
			Claimants: []CompositeOwnershipClaimant{
				{AccountID: 1, Platform: PlatformZhipu, Type: AccountTypeAPIKey},
				{AccountID: 2, Platform: PlatformAnthropic, Type: AccountTypeAPIKey},
			},
		}, nil
	})

	decision, err := resolver.Resolve(context.Background(), 7, "shared-alias", CompositeRouteEndpointResponses)

	require.NoError(t, err)
	require.True(t, decision.Matched)
	require.Empty(t, decision.TargetPlatform)
	require.Equal(t, []string{PlatformAnthropic, PlatformZhipu}, decision.SupplyPlatforms)
	require.Equal(t, []string{CompositeChainAnthropic, CompositeChainOpenAI}, decision.QualifyingChains)
	require.Equal(t, CompositeChainAnthropic, decision.ServiceChain, "responses 端点 eligible 链含 anthropic，入口为 anthropic")
	require.Len(t, decision.SupplyAccounts, 2)
	require.Equal(t, CompositeRouteReasonAmbiguous, decision.Reason)
}

// Scenario: 同链多平台 + OAuth 声明方仍放行选号（用户裁定允许同链多平台）
func TestCompositeRouteResolverAllowsSameChainOAuthClaimant(t *testing.T) {
	resolver := NewCompositeRouteResolver(nil)
	resolver.SetModelOwnershipResolver(func(context.Context, int64, string) (CompositeModelOwnership, error) {
		return CompositeModelOwnership{
			Ambiguous: true,
			Platforms: []string{PlatformOpenAI, PlatformKimi},
			Claimants: []CompositeOwnershipClaimant{
				{AccountID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth},
				{AccountID: 2, Platform: PlatformKimi, Type: AccountTypeAPIKey},
			},
		}, nil
	})

	// gpt-* 可被内置探测器识别；同链歧义放行 OpenAI 网关，不会误入 Anthropic 管线。
	decision, err := resolver.Resolve(context.Background(), 7, "gpt-6-astra", CompositeRouteEndpointResponses)

	require.NoError(t, err)
	require.True(t, decision.Matched)
	require.Equal(t, PlatformOpenAI, decision.TargetPlatform)
	require.Equal(t, []string{PlatformKimi, PlatformOpenAI}, decision.SupplyPlatforms)
	require.Equal(t, []string{CompositeChainOpenAI}, decision.QualifyingChains)
	require.Equal(t, CompositeChainOpenAI, decision.ServiceChain)
	require.Equal(t, CompositeRouteReasonAmbiguous, decision.Reason)

	// 自定义别名同样按同链放行。
	decision, err = resolver.Resolve(context.Background(), 7, "team-alias", CompositeRouteEndpointMessages)
	require.NoError(t, err)
	require.True(t, decision.Matched)
	require.Equal(t, PlatformOpenAI, decision.TargetPlatform)
}

func TestCompositeRouteResolverOwnershipLookupErrorFallsBackOnlyForDetectableModels(t *testing.T) {
	lookupErr := errors.New("account catalog unavailable")
	resolver := NewCompositeRouteResolver(nil)
	resolver.SetModelOwnershipResolver(func(context.Context, int64, string) (CompositeModelOwnership, error) {
		return CompositeModelOwnership{}, lookupErr
	})

	detected, err := resolver.Resolve(context.Background(), 7, "gpt-5", CompositeRouteEndpointResponses)
	require.NoError(t, err)
	require.True(t, detected.Matched)
	require.Equal(t, CompositeRouteSourceDetector, detected.Source)
	require.Equal(t, PlatformOpenAI, detected.TargetPlatform)
	require.Equal(t, []string{PlatformOpenAI}, detected.SupplyPlatforms)
	require.Equal(t, CompositeChainOpenAI, detected.ServiceChain)

	unknown, err := resolver.Resolve(context.Background(), 7, "company-model", CompositeRouteEndpointResponses)
	require.ErrorIs(t, err, lookupErr)
	require.False(t, unknown.Matched)
}

func TestCompositeRouteResolverPrefersEndpointSpecificLongestPrefix(t *testing.T) {
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{
		routes: []CompositeModelRoute{
			{
				ID:             1,
				GroupID:        7,
				PublicModel:    "router/",
				MatchType:      CompositeRouteMatchPrefix,
				TargetPlatform: PlatformAnthropic,
				Endpoint:       CompositeRouteEndpointAny,
				Priority:       10,
				Enabled:        true,
			},
			{
				ID:             2,
				GroupID:        7,
				PublicModel:    "router/gpt-",
				MatchType:      CompositeRouteMatchPrefix,
				TargetPlatform: PlatformOpenAI,
				UpstreamModel:  "gpt-family",
				Endpoint:       CompositeRouteEndpointResponses,
				Priority:       100,
				Enabled:        true,
			},
		},
	})

	decision, err := resolver.Resolve(context.Background(), 7, "router/gpt-5", CompositeRouteEndpointResponses)

	require.NoError(t, err)
	require.True(t, decision.Matched)
	require.Equal(t, CompositeRouteSourceExplicit, decision.Source)
	require.Equal(t, PlatformOpenAI, decision.TargetPlatform)
	require.Equal(t, "gpt-family", decision.UpstreamModel)
	require.NotNil(t, decision.Route)
	require.Equal(t, int64(2), decision.Route.ID)
	require.Equal(t, []string{PlatformOpenAI}, decision.SupplyPlatforms)
}

// TestCompositeRouteResolverPrefixEmptyUpstreamPassesThroughRequestedModel 验证：
// 前缀匹配路由留空 upstream_model 时，转发的是具体请求模型（各自原样），而不是
// 塌缩成 public_model。这是「留空 = 透传原始模型」语义的核心场景。
func TestCompositeRouteResolverPrefixEmptyUpstreamPassesThroughRequestedModel(t *testing.T) {
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{
		routes: []CompositeModelRoute{
			{
				ID:             1,
				GroupID:        7,
				PublicModel:    "deepseek-v4",
				MatchType:      CompositeRouteMatchPrefix,
				TargetPlatform: PlatformOpenAI,
				UpstreamModel:  "", // 留空 = 透传
				Endpoint:       CompositeRouteEndpointAny,
				Priority:       100,
				Enabled:        true,
			},
		},
	})

	for _, model := range []string{"deepseek-v4-flash", "deepseek-v4-pro", "deepseek-v4"} {
		decision, err := resolver.Resolve(context.Background(), 7, model, CompositeRouteEndpointChatCompletions)
		require.NoError(t, err)
		require.True(t, decision.Matched, "model %q should match prefix route", model)
		require.Equal(t, CompositeRouteSourceExplicit, decision.Source)
		require.Equal(t, PlatformOpenAI, decision.TargetPlatform)
			require.Equal(t, model, decision.UpstreamModel, "model %q should pass through verbatim", model)
			require.Equal(t, []string{PlatformOpenAI}, decision.SupplyPlatforms, "model %q", model)
		}
}

// TestCompositeRouteResolverPrefixExplicitUpstreamStillFixed 验证：前缀匹配路由显式
// 填写 upstream_model 时，所有命中请求仍转发同一个固定上游模型（行为不变）。
func TestCompositeRouteResolverPrefixExplicitUpstreamStillFixed(t *testing.T) {
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{
		routes: []CompositeModelRoute{
			{
				ID:             1,
				GroupID:        7,
				PublicModel:    "deepseek-v4",
				MatchType:      CompositeRouteMatchPrefix,
				TargetPlatform: PlatformOpenAI,
				UpstreamModel:  "deepseek-chat",
				Endpoint:       CompositeRouteEndpointAny,
				Priority:       100,
				Enabled:        true,
			},
		},
	})

	for _, model := range []string{"deepseek-v4-flash", "deepseek-v4-pro"} {
		decision, err := resolver.Resolve(context.Background(), 7, model, CompositeRouteEndpointChatCompletions)
		require.NoError(t, err)
		require.True(t, decision.Matched)
		require.Equal(t, "deepseek-chat", decision.UpstreamModel)
		require.Equal(t, []string{PlatformOpenAI}, decision.SupplyPlatforms)
	}
}

func TestCompositeRouteResolverIgnoresDisabledRoutesAndFallsBackToDetector(t *testing.T) {
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{
		routes: []CompositeModelRoute{
			{
				ID:             1,
				GroupID:        7,
				PublicModel:    "gpt-5",
				MatchType:      CompositeRouteMatchExact,
				TargetPlatform: PlatformAnthropic,
				UpstreamModel:  "claude-sonnet-4-6",
				Endpoint:       CompositeRouteEndpointAny,
				Priority:       100,
				Enabled:        false,
			},
		},
	})

	decision, err := resolver.Resolve(context.Background(), 7, "gpt-5", CompositeRouteEndpointAny)

	require.NoError(t, err)
	require.True(t, decision.Matched)
	require.Equal(t, CompositeRouteSourceDetector, decision.Source)
	require.Equal(t, PlatformOpenAI, decision.TargetPlatform)
	require.Equal(t, "gpt-5", decision.UpstreamModel)
	require.Nil(t, decision.Route)
	require.Equal(t, []string{PlatformOpenAI}, decision.SupplyPlatforms)
	require.Equal(t, CompositeChainOpenAI, decision.ServiceChain)
}

func TestCompositeRouteResolverDetectsKimiCodeBareModels(t *testing.T) {
	resolver := NewCompositeRouteResolver(nil)

	for _, model := range []string{"k3", "k3-256k", "kimi-code/k3"} {
		t.Run(model, func(t *testing.T) {
			decision, err := resolver.Resolve(context.Background(), 7, model, CompositeRouteEndpointMessages)

			require.NoError(t, err)
			require.True(t, decision.Matched)
			require.Equal(t, CompositeRouteSourceDetector, decision.Source)
			require.Equal(t, PlatformKimi, decision.TargetPlatform)
			require.Equal(t, model, decision.UpstreamModel)
			require.Equal(t, []string{PlatformKimi}, decision.SupplyPlatforms)
			require.Equal(t, CompositeChainOpenAI, decision.ServiceChain)
		})
	}
}

func TestCompositeRouteResolverExplicitRoutesCoverBucketTwoProviders(t *testing.T) {
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{
		routes: []CompositeModelRoute{
			{
				ID:             1,
				GroupID:        7,
				PublicModel:    "all/gpt-5",
				MatchType:      CompositeRouteMatchExact,
				TargetPlatform: PlatformOpenAI,
				UpstreamModel:  "gpt-5",
				Endpoint:       CompositeRouteEndpointResponses,
				Priority:       100,
				Enabled:        true,
			},
			{
				ID:             2,
				GroupID:        7,
				PublicModel:    "all/claude-sonnet",
				MatchType:      CompositeRouteMatchExact,
				TargetPlatform: PlatformAnthropic,
				UpstreamModel:  "claude-sonnet-4-6",
				Endpoint:       CompositeRouteEndpointMessages,
				Priority:       100,
				Enabled:        true,
			},
			{
				ID:             3,
				GroupID:        7,
				PublicModel:    "all/gemini-pro",
				MatchType:      CompositeRouteMatchExact,
				TargetPlatform: PlatformGemini,
				UpstreamModel:  "gemini-2.5-pro",
				Endpoint:       CompositeRouteEndpointGemini,
				Priority:       100,
				Enabled:        true,
			},
			{
				ID:             4,
				GroupID:        7,
				PublicModel:    "all/grok",
				MatchType:      CompositeRouteMatchExact,
				TargetPlatform: PlatformGrok,
				UpstreamModel:  "grok-4.3",
				Endpoint:       CompositeRouteEndpointResponses,
				Priority:       100,
				Enabled:        true,
			},
		},
	})

	tests := []struct {
		model        string
		endpoint     string
		wantPlatform string
		wantUpstream string
	}{
		{"all/gpt-5", CompositeRouteEndpointResponses, PlatformOpenAI, "gpt-5"},
		{"all/claude-sonnet", CompositeRouteEndpointMessages, PlatformAnthropic, "claude-sonnet-4-6"},
		{"all/gemini-pro", CompositeRouteEndpointGemini, PlatformGemini, "gemini-2.5-pro"},
		{"all/grok", CompositeRouteEndpointResponses, PlatformGrok, "grok-4.3"},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			decision, err := resolver.Resolve(context.Background(), 7, tt.model, tt.endpoint)

			require.NoError(t, err)
			require.True(t, decision.Matched)
			require.Equal(t, CompositeRouteSourceExplicit, decision.Source)
			require.Equal(t, tt.wantPlatform, decision.TargetPlatform)
			require.Equal(t, tt.wantUpstream, decision.UpstreamModel)
			require.Equal(t, []string{tt.wantPlatform}, decision.SupplyPlatforms)
		})
	}
}

// Scenario: 单链 anthropic（anthropic APIKey + antigravity OAuth）→ 链代表 anthropic
func TestCompositeRouteResolverOwnershipSupplyAllAnthropicChain(t *testing.T) {
	resolver := NewCompositeRouteResolver(nil)
	resolver.SetModelOwnershipResolver(func(context.Context, int64, string) (CompositeModelOwnership, error) {
		return CompositeModelOwnership{
			Ambiguous: true,
			Platforms: []string{PlatformAnthropic, PlatformAntigravity},
			Claimants: []CompositeOwnershipClaimant{
				{AccountID: 1, Platform: PlatformAnthropic, Type: AccountTypeAPIKey},
				{AccountID: 2, Platform: PlatformAntigravity, Type: AccountTypeOAuth},
			},
		}, nil
	})

	decision, err := resolver.Resolve(context.Background(), 7, "shared-alias", CompositeRouteEndpointMessages)

	require.NoError(t, err)
	require.True(t, decision.Matched)
	require.Equal(t, CompositeRouteSourceAccount, decision.Source)
	require.Equal(t, PlatformAnthropic, decision.TargetPlatform)
	require.Equal(t, CompositeChainAnthropic, decision.ServiceChain)
	require.Equal(t, []string{CompositeChainAnthropic}, decision.QualifyingChains)
	require.Equal(t, []string{PlatformAnthropic, PlatformAntigravity}, decision.SupplyPlatforms)
	require.Len(t, decision.SupplyAccounts, 2)
	require.Equal(t, CompositeRouteReasonAmbiguous, decision.Reason)
}

// Scenario: 多链 messages（zhipu 走 openai 链，anthropic 走 anthropic 链）
func TestCompositeRouteResolverOwnershipSupplyMultiChainMessages(t *testing.T) {
	resolver := NewCompositeRouteResolver(nil)
	resolver.SetModelOwnershipResolver(func(context.Context, int64, string) (CompositeModelOwnership, error) {
		return CompositeModelOwnership{
			Ambiguous: true,
			Platforms: []string{PlatformZhipu, PlatformAnthropic},
			Claimants: []CompositeOwnershipClaimant{
				{AccountID: 1, Platform: PlatformZhipu, Type: AccountTypeAPIKey},
				{AccountID: 2, Platform: PlatformAnthropic, Type: AccountTypeAPIKey},
			},
		}, nil
	})

	decision, err := resolver.Resolve(context.Background(), 7, "shared-alias", CompositeRouteEndpointMessages)

	require.NoError(t, err)
	require.True(t, decision.Matched)
	require.Empty(t, decision.TargetPlatform)
	require.Equal(t, CompositeChainAnthropic, decision.ServiceChain, "messages 端点入口链为 anthropic")
	require.Equal(t, []string{CompositeChainAnthropic, CompositeChainOpenAI}, decision.QualifyingChains)
	require.Equal(t, []string{PlatformAnthropic, PlatformZhipu}, decision.SupplyPlatforms)
	require.Len(t, decision.SupplyAccounts, 2)
	require.Equal(t, CompositeRouteReasonAmbiguous, decision.Reason)
}

// Scenario: 端点裁剪 —— responses 端点 gemini 不可服务
func TestCompositeRouteResolverOwnershipSupplyEndpointPruning(t *testing.T) {
	t.Run("gemini+anthropic responses keeps only anthropic eligible", func(t *testing.T) {
		resolver := NewCompositeRouteResolver(nil)
		resolver.SetModelOwnershipResolver(func(context.Context, int64, string) (CompositeModelOwnership, error) {
			return CompositeModelOwnership{
				Ambiguous: true,
				Platforms: []string{PlatformGemini, PlatformAnthropic},
				Claimants: []CompositeOwnershipClaimant{
					{AccountID: 1, Platform: PlatformGemini, Type: AccountTypeAPIKey},
					{AccountID: 2, Platform: PlatformAnthropic, Type: AccountTypeAPIKey},
				},
			}, nil
		})

		decision, err := resolver.Resolve(context.Background(), 7, "shared-alias", CompositeRouteEndpointResponses)

		require.NoError(t, err)
		require.True(t, decision.Matched)
		require.Equal(t, PlatformAnthropic, decision.TargetPlatform, "单平台供给走唯一平台")
		require.Equal(t, CompositeChainAnthropic, decision.ServiceChain)
		require.Equal(t, []string{PlatformAnthropic}, decision.SupplyPlatforms, "SupplyPlatforms 只含 eligible 平台")
		require.Len(t, decision.SupplyAccounts, 1)
		require.Equal(t, PlatformAnthropic, decision.SupplyAccounts[0].Platform)
		require.Empty(t, decision.Reason)
	})

	t.Run("gemini-only responses has no eligible", func(t *testing.T) {
		resolver := NewCompositeRouteResolver(nil)
		resolver.SetModelOwnershipResolver(func(context.Context, int64, string) (CompositeModelOwnership, error) {
			return CompositeModelOwnership{
				Matched:   true,
				Platforms: []string{PlatformGemini},
				Claimants: []CompositeOwnershipClaimant{
					{AccountID: 1, Platform: PlatformGemini, Type: AccountTypeAPIKey},
				},
			}, nil
		})

		decision, err := resolver.Resolve(context.Background(), 7, "shared-alias", CompositeRouteEndpointResponses)

		require.NoError(t, err)
		require.False(t, decision.Matched)
		require.Empty(t, decision.TargetPlatform)
		require.Equal(t, []string{PlatformGemini}, decision.SupplyPlatforms, "空 eligible 时回落到全部声明平台用于诊断")
		require.Empty(t, decision.SupplyAccounts)
		require.Equal(t, "no declared account can serve this endpoint", decision.Reason)
	})
}
