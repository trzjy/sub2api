package handler

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// compositeGateAccountRepo 是 handler 门禁测试用的 accountRepo stub，
// 仅实现 ListSchedulableByGroupID（门禁三级解析的 ownership 层级所依赖）。
type compositeGateAccountRepo struct {
	service.AccountRepository
	accounts []service.Account
	err      error
}

func (r *compositeGateAccountRepo) ListSchedulableByGroupID(ctx context.Context, groupID int64) ([]service.Account, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.accounts, nil
}

// newCompositeGateGatewayService 以最小结构构造 *service.GatewayService：accountRepo
// stub + ownership 闭包（由 NewGatewayService 自动将 resolver 的 modelOwnershipResolver
// 接到 svc.resolveCompositeModelOwnership）。resolver 的 repo 传 nil，使显式路由层级
// 跳过、仅走 ownership 与检测器，与线上「账号 model_mapping 独有模型」场景一致。
func newCompositeGateGatewayService(t *testing.T, accounts []service.Account, repoErr error) *service.GatewayService {
	t.Helper()
	repo := &compositeGateAccountRepo{accounts: accounts, err: repoErr}
	resolver := service.NewCompositeRouteResolver(nil)
	return service.NewGatewayService(
		repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, resolver, nil, nil,
	)
}

func TestCompositeTargetPlatformAllowedResolvesKnownAllowedModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/embeddings", nil)
	apiKey := &service.APIKey{Group: &service.Group{Platform: service.PlatformComposite}}
	svc := newCompositeGateGatewayService(t, nil, nil)

	require.True(t, compositeTargetPlatformAllowed(c, apiKey, "text-embedding-3-large", svc, service.PlatformOpenAI))
	platform, ok := service.ResolvedTargetPlatformFromContext(c.Request.Context())
	require.True(t, ok)
	require.Equal(t, service.PlatformOpenAI, platform)
}

func TestOpenAICompatibleTextTargetAllowsCompositeProviders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newCompositeGateGatewayService(t, nil, nil)

	providers := []struct {
		model    string
		platform string
	}{
		{model: "grok-4.3", platform: service.PlatformGrok},
		{model: "kimi-k2-thinking", platform: service.PlatformKimi},
		{model: "k3", platform: service.PlatformKimi},
		{model: "glm-5.2", platform: service.PlatformZhipu},
		{model: "deepseek-v3.2", platform: service.PlatformDeepseek},
		{model: "MiniMax-M3", platform: service.PlatformMiniMax},
	}
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses", "/v1/responses/input_tokens", "/v1/messages/count_tokens"} {
		for _, provider := range providers {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", path, nil)
			apiKey := &service.APIKey{Group: &service.Group{Platform: service.PlatformComposite}}

			require.True(t, openAICompatibleTextTargetAllowed(c, apiKey, provider.model, svc), "path=%s model=%s", path, provider.model)
			platform, ok := service.ResolvedTargetPlatformFromContext(c.Request.Context())
			require.True(t, ok, "path=%s model=%s", path, provider.model)
			require.Equal(t, provider.platform, platform, "path=%s model=%s", path, provider.model)
		}
	}
}

// WS ingress 对 CN 账号既过不了 transport 过滤、HTTP 桥也没有 Responses 转换，
// 放行只会把明确的策略拒绝换成 "no available account"，因此 WS 白名单保持 openai+grok。
func TestResponsesWebSocketCompositePlatformGuardKeepsOpenAIAndGrokOnly(t *testing.T) {
	require.True(t, isResponsesWebSocketCompositePlatform(service.PlatformOpenAI))
	require.True(t, isResponsesWebSocketCompositePlatform(service.PlatformGrok))
	for _, platform := range []string{
		service.PlatformKimi, service.PlatformZhipu, service.PlatformDeepseek, service.PlatformMiniMax,
		service.PlatformAnthropic, service.PlatformGemini,
	} {
		require.False(t, isResponsesWebSocketCompositePlatform(platform), "platform=%s", platform)
	}
}

func TestCompositeTargetPlatformAllowedRejectsWrongOrUnknownModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newCompositeGateGatewayService(t, nil, nil)

	for _, tc := range []struct {
		name  string
		model string
	}{
		{name: "wrong provider", model: "claude-sonnet-4-5"},
		{name: "unknown provider", model: "llama-4-maverick"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/embeddings", nil)
			apiKey := &service.APIKey{Group: &service.Group{Platform: service.PlatformComposite}}

			require.False(t, compositeTargetPlatformAllowed(c, apiKey, tc.model, svc, service.PlatformOpenAI))
		})
	}
}

func TestCompositeTargetPlatformResolvedRejectsUnknownModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	apiKey := &service.APIKey{Group: &service.Group{Platform: service.PlatformComposite}}
	svc := newCompositeGateGatewayService(t, nil, nil)

	require.False(t, compositeTargetPlatformResolved(c, apiKey, "llama-4-maverick", svc))
	_, ok := service.ResolvedTargetPlatformFromContext(c.Request.Context())
	require.False(t, ok)
}

func TestCompositeTargetPlatformResolvedAllowsConcreteGroupWithoutResolution(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	apiKey := &service.APIKey{Group: &service.Group{Platform: service.PlatformAnthropic}}

	require.True(t, compositeTargetPlatformResolved(c, apiKey, "llama-4-maverick", nil))
}

// ① 账号 model_mapping 独有模型（qwen3.8-flash 形态）：门禁经 ownership 层级放行且盖章平台正确。
func TestCompositeGateAllowsAccountModelMappingOwnedModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	apiKey := &service.APIKey{Group: &service.Group{ID: 7, Platform: service.PlatformComposite}}
	svc := newCompositeGateGatewayService(t, []service.Account{{
		ID:       1,
		Platform: service.PlatformDeepseek,
		Credentials: map[string]any{"model_mapping": map[string]any{"qwen3.8-flash": "qwen3.8"}},
	}}, nil)

	require.True(t, compositeTargetPlatformResolved(c, apiKey, "qwen3.8-flash", svc))
	platform, ok := service.ResolvedTargetPlatformFromContext(c.Request.Context())
	require.True(t, ok)
	require.Equal(t, service.PlatformDeepseek, platform)
}

// ② 显式路由命中：上游模型经 context 传播，门禁复解析短路一致（不覆盖、不丢失 upstream）。
func TestCompositeGateExplicitRouteUpstreamModelPropagates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	apiKey := &service.APIKey{Group: &service.Group{Platform: service.PlatformComposite}}
	// 模拟显式路由已在 context 中盖章（上游模型改写）。
	c.Request = c.Request.WithContext(service.WithCompositeRouteDecision(c.Request.Context(), service.CompositeRouteDecision{
		Matched:        true,
		Source:         service.CompositeRouteSourceExplicit,
		PublicModel:    "my-alias",
		TargetPlatform: service.PlatformOpenAI,
		UpstreamModel:  "gpt-5",
	}))
	svc := newCompositeGateGatewayService(t, nil, nil)
	ensureCompositeTargetPlatform(c, apiKey, "my-alias", svc)
	upstream, ok := service.ResolvedUpstreamModelFromContext(c.Request.Context())
	require.True(t, ok)
	require.Equal(t, "gpt-5", upstream)
}

// ③a resolver err：账号目录不可用 + 模型不可检测器识别 → 失败关闭。
func TestCompositeGateFailsClosedOnResolverError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	apiKey := &service.APIKey{Group: &service.Group{Platform: service.PlatformComposite}}
	svc := newCompositeGateGatewayService(t, nil, errors.New("repo boom"))

	require.False(t, compositeTargetPlatformResolved(c, apiKey, "llama-4-maverick", svc))
	_, ok := service.ResolvedTargetPlatformFromContext(c.Request.Context())
	require.False(t, ok)
}

// ③b 未命中：空账号 ownership 不命中、检测器不识别 → 失败关闭。
func TestCompositeGateFailsClosedOnUnmatchedModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	apiKey := &service.APIKey{Group: &service.Group{Platform: service.PlatformComposite}}
	svc := newCompositeGateGatewayService(t, nil, nil)

	require.False(t, compositeTargetPlatformResolved(c, apiKey, "llama-4-maverick", svc))
	_, ok := service.ResolvedTargetPlatformFromContext(c.Request.Context())
	require.False(t, ok)
}

// ③c gatewayCore == nil：失败关闭，不回退检测器、不 panic。
func TestCompositeGateFailsClosedOnNilGatewayService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	apiKey := &service.APIKey{Group: &service.Group{Platform: service.PlatformComposite}}

	require.False(t, compositeTargetPlatformResolved(c, apiKey, "gpt-5", nil))
	_, ok := service.ResolvedTargetPlatformFromContext(c.Request.Context())
	require.False(t, ok)
}

func TestOpenAIReasoningEffortPolicyForCompositeTarget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	group := &service.Group{
		Platform:           service.PlatformComposite,
		MaxReasoningEffort: "medium",
		ReasoningEffortMappings: []service.ReasoningEffortMapping{
			{From: "max", To: "xhigh"},
		},
	}
	apiKey := &service.APIKey{Group: group}
	body := []byte(`{"reasoning":{"effort":"max"}}`)

	openAICtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	openAICtx.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	openAICtx.Request = openAICtx.Request.WithContext(service.WithResolvedTargetPlatform(openAICtx.Request.Context(), service.PlatformOpenAI))
	got, changed, err := applyOpenAIReasoningEffortPolicyForRequest(openAICtx, apiKey, body)
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, `{"reasoning":{"effort":"medium"}}`, string(got))
	requested := service.RequestedReasoningEffortFromContext(openAICtx.Request.Context())
	require.NotNil(t, requested)
	require.Equal(t, "max", *requested)

	bindOpenAIReasoningEffortPolicyForMessagesRequest(openAICtx, apiKey, []byte(`{"output_config":{"effort":"max"}}`))
	bound, changed, err := service.ApplyOpenAIReasoningEffortPolicyFromContext(openAICtx.Request.Context(), body)
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, `{"reasoning":{"effort":"medium"}}`, string(bound))

	omittedCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	omittedCtx.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	omittedCtx.Request = omittedCtx.Request.WithContext(service.WithResolvedTargetPlatform(omittedCtx.Request.Context(), service.PlatformOpenAI))
	bindOpenAIReasoningEffortPolicyForMessagesRequest(omittedCtx, apiKey, []byte(`{"model":"gpt-5"}`))
	omitted, changed, err := service.ApplyOpenAIReasoningEffortPolicyFromContext(omittedCtx.Request.Context(), body)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, body, omitted)

	denyGroup := *group
	denyGroup.MaxReasoningEffortOverLimit = service.ReasoningEffortOverLimitDeny
	denyAPIKey := &service.APIKey{Group: &denyGroup}
	denyCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	denyCtx.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	denyCtx.Request = denyCtx.Request.WithContext(service.WithResolvedTargetPlatform(denyCtx.Request.Context(), service.PlatformOpenAI))
	_, _, err = applyOpenAIReasoningEffortPolicyForRequest(denyCtx, denyAPIKey, body)
	require.Error(t, err)
	var overLimit *service.ReasoningEffortOverLimitError
	require.ErrorAs(t, err, &overLimit)

	mappingDenyGroup := *group
	mappingDenyGroup.MaxReasoningEffort = ""
	mappingDenyGroup.ReasoningEffortMappings = []service.ReasoningEffortMapping{
		{From: "max", To: service.ReasoningEffortMappingDeny},
	}
	mappingDenyAPIKey := &service.APIKey{Group: &mappingDenyGroup}
	mappingDenyCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	mappingDenyCtx.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	mappingDenyCtx.Request = mappingDenyCtx.Request.WithContext(service.WithResolvedTargetPlatform(mappingDenyCtx.Request.Context(), service.PlatformOpenAI))
	_, changed, err = applyOpenAIReasoningEffortPolicyForRequest(mappingDenyCtx, mappingDenyAPIKey, body)
	require.Error(t, err)
	require.False(t, changed)
	var mappingDenied *service.ReasoningEffortMappingDeniedError
	require.ErrorAs(t, err, &mappingDenied)
	require.Equal(t, "max", mappingDenied.Requested)
	require.Contains(t, mappingDenied.Error(), "denied by this group's mapping policy")

	grokCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	grokCtx.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	grokCtx.Request = grokCtx.Request.WithContext(service.WithResolvedTargetPlatform(grokCtx.Request.Context(), service.PlatformGrok))
	got, changed, err = applyOpenAIReasoningEffortPolicyForRequest(grokCtx, apiKey, body)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, body, got)
}

func TestOpenAIShapedReasoningEffortPolicyForAnthropicGroup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	apiKey := &service.APIKey{Group: &service.Group{
		Platform:           service.PlatformAnthropic,
		MaxReasoningEffort: "xhigh",
	}}

	got, changed, err := applyOpenAIReasoningEffortPolicyForRequest(c, apiKey, []byte(`{"reasoning":{"effort":"max"}}`))
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "xhigh", gjson.GetBytes(got, "reasoning.effort").String())
}

func TestAnthropicReasoningEffortPolicyForConcreteAndCompositeTargets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"claude-fable-5-1","output_config":{"effort":"max"}}`)
	policy := func(platform string) *service.APIKey {
		return &service.APIKey{Group: &service.Group{
			Platform:           platform,
			MaxReasoningEffort: "xhigh",
			ReasoningEffortMappings: []service.ReasoningEffortMapping{
				{From: "max", To: "xhigh"},
			},
		}}
	}

	concrete, _ := gin.CreateTestContext(httptest.NewRecorder())
	concrete.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	got, changed, err := applyAnthropicReasoningEffortPolicyForRequest(concrete, policy(service.PlatformAnthropic), body)
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, `{"model":"claude-fable-5-1","output_config":{"effort":"xhigh"}}`, string(got))

	denyMax := policy(service.PlatformAnthropic)
	denyMax.Group.ReasoningEffortMappings = nil
	denyMax.Group.MaxReasoningEffortOverLimit = service.ReasoningEffortOverLimitDeny
	_, changed, err = applyAnthropicReasoningEffortPolicyForRequest(concrete, denyMax, body)
	require.Error(t, err)
	require.False(t, changed)
	var overLimit *service.ReasoningEffortOverLimitError
	require.ErrorAs(t, err, &overLimit)
	require.Equal(t, "max", overLimit.Requested)
	require.Equal(t, "xhigh", overLimit.Max)

	composite, _ := gin.CreateTestContext(httptest.NewRecorder())
	composite.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	composite.Request = composite.Request.WithContext(service.WithResolvedTargetPlatform(composite.Request.Context(), service.PlatformAnthropic))
	got, changed, err = applyAnthropicReasoningEffortPolicyForRequest(composite, policy(service.PlatformComposite), body)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "xhigh", gjson.GetBytes(got, "output_config.effort").String())

	openAI, _ := gin.CreateTestContext(httptest.NewRecorder())
	openAI.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	openAI.Request = openAI.Request.WithContext(service.WithResolvedTargetPlatform(openAI.Request.Context(), service.PlatformOpenAI))
	got, changed, err = applyAnthropicReasoningEffortPolicyForRequest(openAI, policy(service.PlatformComposite), body)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, body, got)
}

func TestAnthropicCompositePolicyClampsUnsupportedMinimalToLow(t *testing.T) {
	maxEffort, mappings := anthropicCompatibleReasoningEffortPolicy("minimal", []service.ReasoningEffortMapping{{From: "max", To: "minimal"}})
	require.Equal(t, "low", maxEffort)
	require.Equal(t, []service.ReasoningEffortMapping{{From: "max", To: "low"}}, mappings)
}

func TestClientRequestedModelUsesCompositePublicModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	c.Request = c.Request.WithContext(service.WithCompositeRouteDecision(c.Request.Context(), service.CompositeRouteDecision{
		Matched:        true,
		Source:         service.CompositeRouteSourceExplicit,
		PublicModel:    "public-alias",
		TargetPlatform: service.PlatformOpenAI,
		UpstreamModel:  "gpt-5",
	}))

	input := buildContentModerationInput(c, nil, middleware2.AuthSubject{UserID: 42}, service.ContentModerationProtocolOpenAIChat, "gpt-5", nil)
	require.Equal(t, "public-alias", input.Model)
	require.Equal(t, service.PlatformOpenAI, input.Provider)

	fields := clientRequestedUsageFields(c, service.ChannelMappingResult{MappedModel: "gpt-5"}, "gpt-5", "gpt-5")
	require.Equal(t, "public-alias", fields.OriginalModel)
	require.Equal(t, "public-alias", fields.ChannelMappedModel)
	require.Equal(t, "public-alias\u2192gpt-5", fields.ModelMappingChain)
}
