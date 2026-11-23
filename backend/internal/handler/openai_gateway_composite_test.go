//go:build unit

package handler

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// newCompositeDecisionGinCtx 构造一个带 composite 盖章 decision 的 gin ctx。
func newCompositeDecisionGinCtx(decision service.CompositeRouteDecision) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	c.Request = c.Request.WithContext(service.WithCompositeRouteDecision(c.Request.Context(), decision))
	return c
}

func TestApplyCompositeSelectedAccountPlatform(t *testing.T) {
	compositeKey := &service.APIKey{Group: &service.Group{Platform: service.PlatformComposite}}

	// 同链歧义：代表平台 openai，真实供给 zhipu/cursor。
	ambiguous := service.CompositeRouteDecision{
		Matched:         true,
		TargetPlatform:  service.PlatformOpenAI,
		Reason:          service.CompositeRouteReasonAmbiguous,
		SupplyPlatforms: []string{service.PlatformZhipu, service.PlatformCursor},
	}

	t.Run("ambiguous writes actual platform and marks resolved", func(t *testing.T) {
		c := newCompositeDecisionGinCtx(ambiguous)

		// 选号前：代表平台 openai 未被当作配额平台（失败关闭）。
		require.Empty(t, service.QuotaPlatform(c.Request.Context(), compositeKey))

		applyCompositeSelectedAccountPlatform(c, &service.Account{Platform: service.PlatformZhipu})

		got, ok := service.ResolvedTargetPlatformFromContext(c.Request.Context())
		require.True(t, ok)
		require.Equal(t, service.PlatformZhipu, got)
		// actual-resolved 标记后，配额平台按实际账号平台计量。
		require.Equal(t, service.PlatformZhipu, service.QuotaPlatform(c.Request.Context(), compositeKey))
	})

	t.Run("nil safe", func(t *testing.T) {
		require.NotPanics(t, func() {
			applyCompositeSelectedAccountPlatform(nil, &service.Account{Platform: service.PlatformZhipu})
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
			applyCompositeSelectedAccountPlatform(c, nil)
		})
	})

	t.Run("no decision leaves ctx unchanged", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/messages", nil)

		applyCompositeSelectedAccountPlatform(c, &service.Account{Platform: service.PlatformZhipu})

		_, ok := service.ResolvedTargetPlatformFromContext(c.Request.Context())
		require.False(t, ok)
		require.Empty(t, service.QuotaPlatform(c.Request.Context(), compositeKey))
	})

	t.Run("concrete decision leaves ctx unchanged", func(t *testing.T) {
		concrete := service.CompositeRouteDecision{Matched: true, TargetPlatform: service.PlatformOpenAI}
		c := newCompositeDecisionGinCtx(concrete)

		applyCompositeSelectedAccountPlatform(c, &service.Account{Platform: service.PlatformZhipu})

		got, ok := service.ResolvedTargetPlatformFromContext(c.Request.Context())
		require.True(t, ok)
		require.Equal(t, service.PlatformOpenAI, got)
		require.Equal(t, service.PlatformOpenAI, service.QuotaPlatform(c.Request.Context(), compositeKey))
	})
}

func TestOpenAIMessagesDispatchExemptsCompositeSupply(t *testing.T) {
	compositeKey := &service.APIKey{
		Group: &service.Group{Platform: service.PlatformComposite, AllowMessagesDispatch: false},
	}

	t.Run("ambiguous supply with exempt platform allowed", func(t *testing.T) {
		// zhipu 是 CN provider（豁免集合），代表平台 openai 但整条供给含豁免平台 → 放行。
		decision := service.CompositeRouteDecision{
			Matched:         true,
			TargetPlatform:  service.PlatformOpenAI,
			Reason:          service.CompositeRouteReasonAmbiguous,
			SupplyPlatforms: []string{service.PlatformZhipu},
		}
		c := newCompositeDecisionGinCtx(decision)
		require.True(t, allowOpenAICompatibleMessagesDispatch(c, compositeKey))
	})

	t.Run("ambiguous supply with only openai denied", func(t *testing.T) {
		decision := service.CompositeRouteDecision{
			Matched:         true,
			TargetPlatform:  service.PlatformOpenAI,
			Reason:          service.CompositeRouteReasonAmbiguous,
			SupplyPlatforms: []string{service.PlatformOpenAI},
		}
		c := newCompositeDecisionGinCtx(decision)
		require.False(t, allowOpenAICompatibleMessagesDispatch(c, compositeKey))
	})

	t.Run("non composite group unchanged", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
		openAIKey := &service.APIKey{
			Group: &service.Group{Platform: service.PlatformOpenAI, AllowMessagesDispatch: false},
		}
		require.False(t, allowOpenAICompatibleMessagesDispatch(c, openAIKey))
		openAIKey.Group.AllowMessagesDispatch = true
		require.True(t, allowOpenAICompatibleMessagesDispatch(c, openAIKey))
	})
}

func TestCheckCompositeSelectedPlatformQuotaGating(t *testing.T) {
	ambiguous := service.CompositeRouteDecision{
		Matched:         true,
		TargetPlatform:  service.PlatformOpenAI,
		Reason:          service.CompositeRouteReasonAmbiguous,
		SupplyPlatforms: []string{service.PlatformZhipu},
	}
	concrete := service.CompositeRouteDecision{Matched: true, TargetPlatform: service.PlatformOpenAI}
	account := &service.Account{Platform: service.PlatformZhipu}

	compositeKey := func() *service.APIKey {
		return &service.APIKey{
			Group: &service.Group{Platform: service.PlatformComposite},
			User:  &service.User{ID: 1},
		}
	}

	t.Run("no decision returns nil", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
		h := &OpenAIGatewayHandler{}
		require.NoError(t, h.checkCompositeSelectedPlatformQuota(c, compositeKey(), nil, account))
	})

	t.Run("concrete decision returns nil", func(t *testing.T) {
		c := newCompositeDecisionGinCtx(concrete)
		h := &OpenAIGatewayHandler{billingCacheService: &service.BillingCacheService{}}
		require.NoError(t, h.checkCompositeSelectedPlatformQuota(c, compositeKey(), nil, account))
	})

	t.Run("ambiguous subscription group returns nil", func(t *testing.T) {
		c := newCompositeDecisionGinCtx(ambiguous)
		h := &OpenAIGatewayHandler{billingCacheService: &service.BillingCacheService{}}
		key := compositeKey()
		key.Group.SubscriptionType = service.SubscriptionTypeSubscription
		require.True(t, key.Group.IsSubscriptionType())
		require.NoError(t, h.checkCompositeSelectedPlatformQuota(c, key, &service.UserSubscription{}, account))
	})

	t.Run("ambiguous non-subscription with nil billing service degrades to nil", func(t *testing.T) {
		c := newCompositeDecisionGinCtx(ambiguous)
		h := &OpenAIGatewayHandler{} // billingCacheService == nil：降级，不 panic
		require.False(t, compositeKey().Group.IsSubscriptionType())
		require.NotPanics(t, func() {
			require.NoError(t, h.checkCompositeSelectedPlatformQuota(c, compositeKey(), nil, account))
		})
	})

	t.Run("nil args are safe", func(t *testing.T) {
		h := &OpenAIGatewayHandler{}
		require.NoError(t, h.checkCompositeSelectedPlatformQuota(nil, nil, nil, nil))
		c := newCompositeDecisionGinCtx(ambiguous)
		require.NoError(t, h.checkCompositeSelectedPlatformQuota(c, nil, nil, account))
		require.NoError(t, h.checkCompositeSelectedPlatformQuota(c, &service.APIKey{}, nil, nil))
	})
}

// compositeWSDecisionRouteRepo 是决策段测试用的最小 composite route repo。
type compositeWSDecisionRouteRepo struct {
	service.CompositeModelRouteRepository
	routes []service.CompositeModelRoute
	err    error
}

func (r *compositeWSDecisionRouteRepo) ListByGroup(context.Context, int64, bool) ([]service.CompositeModelRoute, error) {
	return r.routes, r.err
}

// compositeWSOwnership 构造一个 ownership resolver，把 claimants 原样交给
// responses_ws 的 chain 资格筛选（A1c 的 responses_ws 分支仅放行 openai/grok）。
func compositeWSOwnership(claimants ...service.CompositeOwnershipClaimant) service.CompositeModelOwnershipResolver {
	return func(context.Context, int64, string) (service.CompositeModelOwnership, error) {
		platforms := make([]string, 0, len(claimants))
		seen := map[string]struct{}{}
		for _, claimant := range claimants {
			if _, ok := seen[claimant.Platform]; ok {
				continue
			}
			seen[claimant.Platform] = struct{}{}
			platforms = append(platforms, claimant.Platform)
		}
		return service.CompositeModelOwnership{
			Matched:   true,
			Platforms: platforms,
			Claimants: claimants,
		}, nil
	}
}

// wsDecisionNeedsAnyTransport 复刻 WS requiredTransport 的 composite 多平台判据，
// 用于决策段断言：Matched && len(SupplyAccounts)>0 && len(SupplyPlatforms)>1。
func wsDecisionNeedsAnyTransport(d service.CompositeRouteDecision) bool {
	return d.Matched && len(d.SupplyAccounts) > 0 && len(d.SupplyPlatforms) > 1
}

func TestCompositeWebSocketResponsesWSEndpointEligibility(t *testing.T) {
	const model = "ws-decision-model"
	newResolver := func(claimants ...service.CompositeOwnershipClaimant) *service.CompositeRouteResolver {
		resolver := service.NewCompositeRouteResolver(&compositeWSDecisionRouteRepo{})
		resolver.SetModelOwnershipResolver(compositeWSOwnership(claimants...))
		return resolver
	}

	t.Run("openai only supply matched single platform", func(t *testing.T) {
		resolver := newResolver(service.CompositeOwnershipClaimant{AccountID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey})
		decision, err := resolveResponsesWSCompositeDecision(context.Background(), resolver, 7, model)
		require.NoError(t, err)
		require.True(t, decision.Matched)
		require.Equal(t, service.CompositeRouteSourceAccount, decision.Source)
		require.Equal(t, []string{service.PlatformOpenAI}, decision.SupplyPlatforms)
		require.Equal(t, service.CompositeChainOpenAI, decision.ServiceChain)
		require.False(t, wsDecisionNeedsAnyTransport(decision))
	})

	t.Run("grok only supply matched single platform", func(t *testing.T) {
		resolver := newResolver(service.CompositeOwnershipClaimant{AccountID: 2, Platform: service.PlatformGrok, Type: service.AccountTypeAPIKey})
		decision, err := resolveResponsesWSCompositeDecision(context.Background(), resolver, 7, model)
		require.NoError(t, err)
		require.True(t, decision.Matched)
		require.Equal(t, service.PlatformGrok, decision.TargetPlatform)
		require.Equal(t, []string{service.PlatformGrok}, decision.SupplyPlatforms)
		require.False(t, wsDecisionNeedsAnyTransport(decision))
	})

	t.Run("mixed openai and grok supply needs any transport", func(t *testing.T) {
		resolver := newResolver(
			service.CompositeOwnershipClaimant{AccountID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey},
			service.CompositeOwnershipClaimant{AccountID: 2, Platform: service.PlatformGrok, Type: service.AccountTypeAPIKey},
		)
		decision, err := resolveResponsesWSCompositeDecision(context.Background(), resolver, 7, model)
		require.NoError(t, err)
		require.True(t, decision.Matched)
		require.Len(t, decision.SupplyAccounts, 2)
		require.ElementsMatch(t, []string{service.PlatformOpenAI, service.PlatformGrok}, decision.SupplyPlatforms)
		require.True(t, wsDecisionNeedsAnyTransport(decision))
	})

	t.Run("anthropic only supply not matched", func(t *testing.T) {
		resolver := newResolver(service.CompositeOwnershipClaimant{AccountID: 3, Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey})
		decision, err := resolveResponsesWSCompositeDecision(context.Background(), resolver, 7, model)
		require.NoError(t, err)
		require.False(t, decision.Matched)
		require.Empty(t, decision.SupplyAccounts)
		require.False(t, wsDecisionNeedsAnyTransport(decision))
	})

	t.Run("gemini only supply not matched", func(t *testing.T) {
		resolver := newResolver(service.CompositeOwnershipClaimant{AccountID: 4, Platform: service.PlatformGemini, Type: service.AccountTypeAPIKey})
		decision, err := resolveResponsesWSCompositeDecision(context.Background(), resolver, 7, model)
		require.NoError(t, err)
		require.False(t, decision.Matched)
		require.Empty(t, decision.SupplyAccounts)
	})
}

func TestCompositeWebSocketExplicitRouteDecisionPriority(t *testing.T) {
	const model = "ws-explicit-alias"
	for _, endpoint := range []string{service.CompositeRouteEndpointResponses, service.CompositeRouteEndpointAny} {
		t.Run(endpoint, func(t *testing.T) {
			ownershipCalls := 0
			resolver := service.NewCompositeRouteResolver(&compositeWSDecisionRouteRepo{routes: []service.CompositeModelRoute{{
				GroupID: 7, PublicModel: model, MatchType: service.CompositeRouteMatchExact,
				TargetPlatform: service.PlatformOpenAI, UpstreamModel: "explicit-upstream",
				Endpoint: endpoint, Enabled: true,
			}}})
			// 显式 route 命中即返回，不再走 responses_ws 的 ownership 解析。
			resolver.SetModelOwnershipResolver(func(context.Context, int64, string) (service.CompositeModelOwnership, error) {
				ownershipCalls++
				return service.CompositeModelOwnership{Matched: true, Platforms: []string{service.PlatformAnthropic}}, nil
			})

			decision, err := resolveResponsesWSCompositeDecision(context.Background(), resolver, 7, model)
			require.NoError(t, err)
			require.True(t, decision.Matched)
			require.Equal(t, service.CompositeRouteSourceExplicit, decision.Source)
			require.Equal(t, service.PlatformOpenAI, decision.TargetPlatform)
			require.Equal(t, "explicit-upstream", decision.UpstreamModel)
			require.Zero(t, ownershipCalls)
			require.False(t, wsDecisionNeedsAnyTransport(decision))
		})
	}
}

func TestCompositeWebSocketResponsesWSNoMatchKeepsEmptyDecision(t *testing.T) {
	resolver := service.NewCompositeRouteResolver(&compositeWSDecisionRouteRepo{})
	decision, err := resolveResponsesWSCompositeDecision(context.Background(), resolver, 7, "unknown-ws-model")
	require.NoError(t, err)
	require.False(t, decision.Matched)
	require.Empty(t, decision.SupplyAccounts)
	require.False(t, wsDecisionNeedsAnyTransport(decision))
}

func TestCompositeWSRequiredTransport(t *testing.T) {
	multiPlatform := service.CompositeRouteDecision{
		Matched:         true,
		SupplyAccounts:  []service.CompositeOwnershipClaimant{{AccountID: 1, Platform: service.PlatformOpenAI}, {AccountID: 2, Platform: service.PlatformGrok}},
		SupplyPlatforms: []string{service.PlatformOpenAI, service.PlatformGrok},
	}
	singlePlatform := service.CompositeRouteDecision{
		Matched:         true,
		SupplyAccounts:  []service.CompositeOwnershipClaimant{{AccountID: 1, Platform: service.PlatformOpenAI}},
		SupplyPlatforms: []string{service.PlatformOpenAI},
	}

	t.Run("multi platform composite uses new transport", func(t *testing.T) {
		require.Equal(t, service.OpenAIUpstreamTransportResponsesWSOrGrokHTTP,
			compositeWSRequiredTransport(multiPlatform, service.PlatformOpenAI))
	})

	t.Run("single platform openai uses ws v2 ingress", func(t *testing.T) {
		require.Equal(t, service.OpenAIUpstreamTransportResponsesWebsocketV2Ingress,
			compositeWSRequiredTransport(singlePlatform, service.PlatformOpenAI))
	})

	t.Run("unmatched openai uses ws v2 ingress", func(t *testing.T) {
		require.Equal(t, service.OpenAIUpstreamTransportResponsesWebsocketV2Ingress,
			compositeWSRequiredTransport(service.CompositeRouteDecision{}, service.PlatformOpenAI))
	})

	t.Run("grok request uses http sse", func(t *testing.T) {
		require.Equal(t, service.OpenAIUpstreamTransportHTTPSSE,
			compositeWSRequiredTransport(service.CompositeRouteDecision{}, service.PlatformGrok))
	})

	t.Run("empty decision openai uses ws v2 ingress", func(t *testing.T) {
		require.Equal(t, service.OpenAIUpstreamTransportResponsesWebsocketV2Ingress,
			compositeWSRequiredTransport(service.CompositeRouteDecision{}, service.PlatformOpenAI))
	})
}

func TestOpenAICompatibleRequestPlatform(t *testing.T) {
	t.Run("composite with resolved zhipu uses concrete platform", func(t *testing.T) {
		apiKey := &service.APIKey{Group: &service.Group{Platform: service.PlatformComposite}}
		ctx := service.WithCompositeRouteDecision(context.Background(), service.CompositeRouteDecision{
			Matched:        true,
			TargetPlatform: service.PlatformZhipu,
		})
		require.Equal(t, service.PlatformZhipu, openAICompatibleRequestPlatform(ctx, apiKey))
	})

	t.Run("composite with resolved deepseek uses concrete platform", func(t *testing.T) {
		apiKey := &service.APIKey{Group: &service.Group{Platform: service.PlatformComposite}}
		ctx := service.WithCompositeRouteDecision(context.Background(), service.CompositeRouteDecision{
			Matched:        true,
			TargetPlatform: service.PlatformDeepseek,
		})
		require.Equal(t, service.PlatformDeepseek, openAICompatibleRequestPlatform(ctx, apiKey))
	})

	t.Run("composite without resolved platform keeps existing behavior", func(t *testing.T) {
		apiKey := &service.APIKey{Group: &service.Group{Platform: service.PlatformComposite}}
		// 无盖章时回退到分组平台归一：composite 未解析前仍收敛为 openai。
		require.Equal(t, service.PlatformOpenAI, openAICompatibleRequestPlatform(context.Background(), apiKey))
	})

	t.Run("non composite group keeps existing behavior", func(t *testing.T) {
		apiKey := &service.APIKey{Group: &service.Group{Platform: service.PlatformGrok}}
		// 非 composite：即便 ctx 盖章国产平台，仍走既有归一分支（归一后保留原值）。
		ctx := service.WithResolvedTargetPlatform(context.Background(), service.PlatformZhipu)
		require.Equal(t, service.PlatformZhipu, openAICompatibleRequestPlatform(ctx, apiKey))
		// 非 composite 且无盖章：使用分组平台。
		require.Equal(t, service.PlatformGrok, openAICompatibleRequestPlatform(context.Background(), apiKey))
	})
}

// TestOpenAIMessagesSizeExclusionAddsLogging 验证派发单 C25/C26 取证日志：规模排除规则
// 命中的 accountID 集合与数量被结构化打印（id 升序，空集也记录），使生产日志中的
// excluded=N 可精确归因到具体规则与账号。纯日志用例，不依赖 DB/网关桩。
func TestOpenAIMessagesSizeExclusionAddsLogging(t *testing.T) {
	core, observed := observer.New(zap.InfoLevel)
	reqLog := zap.New(core)
	gid := int64(118)

	logMessagesSizeExclusionAdds(reqLog, "self_host", &gid, service.PlatformZhipu, "glm-5.3-flash",
		map[int64]struct{}{104: {}, 101: {}})
	logMessagesSizeExclusionAdds(reqLog, "sensenova", &gid, service.PlatformZhipu, "glm-5.3-flash",
		map[int64]struct{}{})
	logMessagesSizeExclusionAdds(reqLog, "request_size_scope", &gid, service.PlatformZhipu, "glm-5.3-flash",
		map[int64]struct{}{205: {}, 203: {}, 204: {}})

	entries := observed.All()
	require.Len(t, entries, 3, "三条规则各记录一条")

	byRule := make(map[string]map[string]interface{}, len(entries))
	for _, e := range entries {
		require.Equal(t, "openai_messages.size_exclusion_adds", e.Message)
		ctx := e.ContextMap()
		require.Equal(t, int64(118), ctx["group_id"])
		require.Equal(t, service.PlatformZhipu, ctx["platform"])
		require.Equal(t, "glm-5.3-flash", ctx["model"])
		byRule[ctx["rule"].(string)] = ctx
	}

	self := byRule["self_host"]
	require.Equal(t, int64(2), self["count"])
	selfIDs := self["account_ids"].([]interface{})
	require.Len(t, selfIDs, 2)
	require.EqualValues(t, 101, selfIDs[0])
	require.EqualValues(t, 104, selfIDs[1])

	sense := byRule["sensenova"]
	require.Equal(t, int64(0), sense["count"])
	require.Empty(t, sense["account_ids"])

	scope := byRule["request_size_scope"]
	require.Equal(t, int64(3), scope["count"])
	scopeIDs := scope["account_ids"].([]interface{})
	require.Len(t, scopeIDs, 3)
	require.EqualValues(t, 203, scopeIDs[0])
	require.EqualValues(t, 204, scopeIDs[1])
	require.EqualValues(t, 205, scopeIDs[2])
}
