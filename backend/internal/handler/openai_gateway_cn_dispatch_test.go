package handler

// CN 分组 /v1/messages 调度闸门回归（修复:正常途径创建的 CN 分组曾恒 403）：
// sanitizeGroupMessagesDispatchFields 对非 openai/composite 平台强制 AllowMessagesDispatch
// =false，故 CN 分组必须与 grok 一样在闸门处豁免，否则原生 Anthropic 直通
//（Claude Code 主用例）永远不可达。composite 分组解析到 grok/CN 目标时按
// 分组模型白名单准入同样覆盖（生产 group 64 allowlist 关闭即 claude-* 可达）。
// Muse 平台方案已批准下游全协议支持，与 grok/CN"messages 是主要服务形态"同语义，同一 403 陷阱，一并豁免。
// CodeBuddy 同语义（OAuth 账号经 CC 出站到 copilot.tencent.com），且开关对其恒 false
// 且前端不提供，只能豁免——不豁免则 codebuddy 分组 /v1/messages 永远 403。

import (
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestAllowOpenAICompatibleMessagesDispatch_CodeBuddyExempt 覆盖 codebuddy 分组
// /v1/messages 恒 403 的修复（2026-10-15 生产实证：group 109 全程 /v1/messages
// 成功数为 0，客户端只能改用 responses/chat_completions）。
//
// codebuddy 与 grok/CN/muse 同语义：其账号是 OAuth 型，/v1/messages 经 CC 出站
// 管线转发到 copilot.tencent.com，是 Claude Code 接入腾讯模型的主要服务形态。
// allow_messages_dispatch 开关对其恒为 false 且前端不提供该开关
// （groupsMessagesDispatch.ts 只对 openai/composite 渲染），故必须走豁免路径，
// 打开开关不可行也非正确修法。
func TestAllowOpenAICompatibleMessagesDispatch_CodeBuddyExempt(t *testing.T) {
	apiKey := &service.APIKey{Group: &service.Group{
		Platform:              service.PlatformCodeBuddy,
		AllowMessagesDispatch: false,
	}}
	require.True(t, allowOpenAICompatibleMessagesDispatch(nil, apiKey),
		"codebuddy 分组必须豁免 allow_messages_dispatch 闸门，否则 /v1/messages 恒 403")

	// 非回归：豁免不得放宽 openai 分组——它仍受开关控制。
	openaiOff := &service.APIKey{Group: &service.Group{Platform: service.PlatformOpenAI, AllowMessagesDispatch: false}}
	require.False(t, allowOpenAICompatibleMessagesDispatch(nil, openaiOff),
		"openai 分组在开关关闭时必须仍然拒绝，不得被 codebuddy 豁免误伤")
	openaiOn := &service.APIKey{Group: &service.Group{Platform: service.PlatformOpenAI, AllowMessagesDispatch: true}}
	require.True(t, allowOpenAICompatibleMessagesDispatch(nil, openaiOn))
}

// composite 分组解析到 codebuddy 目标时与独立 codebuddy 分组同语义豁免。
// 可达性依据：channel_service.go 的 composite 目标平台白名单含 PlatformCodeBuddy，
// 且 CompositeRouteResolver 对显式路由直接采用 route.TargetPlatform。
func TestAllowOpenAICompatibleMessagesDispatch_CompositeCodeBuddyTargetExempt(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newCtx := func(targetPlatform string, allow bool) (*gin.Context, *service.APIKey) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
		c.Request = c.Request.WithContext(service.WithResolvedTargetPlatform(c.Request.Context(), targetPlatform))
		return c, &service.APIKey{Group: &service.Group{Platform: service.PlatformComposite, AllowMessagesDispatch: allow}}
	}

	c, apiKey := newCtx(service.PlatformCodeBuddy, false)
	require.True(t, allowOpenAICompatibleMessagesDispatch(c, apiKey),
		"composite 解析到 codebuddy 目标时必须豁免")

	// 非回归：composite 解析到 openai 目标仍受自身开关控制。
	c, apiKey = newCtx(service.PlatformOpenAI, false)
	require.False(t, allowOpenAICompatibleMessagesDispatch(c, apiKey))
}

// codebuddy 分组不得套用 openai 专属的 gpt-5.x 调度级默认模型值：
// 其上游是 copilot.tencent.com，收到 gpt-5.4/gpt-5.3-codex 必被拒。
// 分组模型白名单未开启时 claude-* 请求可达本映射函数（生产 group 64 即 allowlist 关闭）。
func TestResolveOpenAIMessagesDispatchMappedModel_CodeBuddySkipsGroupMapping(t *testing.T) {
	apiKey := &service.APIKey{Group: &service.Group{
		Platform: service.PlatformCodeBuddy,
		MessagesDispatchModelConfig: service.OpenAIMessagesDispatchModelConfig{
			OpusMappedModel: "gpt-5.4",
		},
	}}
	for _, model := range []string{"claude-opus-4-6", "claude-sonnet-4-5-20250929", "claude-haiku-4-5-20251001"} {
		require.Empty(t, resolveOpenAIMessagesDispatchMappedModel(nil, apiKey, model), "model=%s", model)
	}
	// 非 claude-* 模型（如生产 group 109 的 space-bunny）本就返回空，保持不变。
	require.Empty(t, resolveOpenAIMessagesDispatchMappedModel(nil, apiKey, "space-bunny"))
}

func TestAllowOpenAICompatibleMessagesDispatch_CNProvidersExempt(t *testing.T) {
	require.True(t, allowOpenAICompatibleMessagesDispatch(nil, nil), "无 key 保持放行")

	for _, platform := range []string{service.PlatformKimi, service.PlatformZhipu, service.PlatformDeepseek, service.PlatformMiniMax, service.PlatformOpenCodeGo, service.PlatformGrok, service.PlatformMuse} {
		apiKey := &service.APIKey{Group: &service.Group{Platform: platform, AllowMessagesDispatch: false}}
		require.True(t, allowOpenAICompatibleMessagesDispatch(nil, apiKey),
			"%s 分组必须豁免 allow_messages_dispatch 闸门", platform)
	}

	// 非回归：openai 分组仍受开关控制。
	openaiOff := &service.APIKey{Group: &service.Group{Platform: service.PlatformOpenAI, AllowMessagesDispatch: false}}
	require.False(t, allowOpenAICompatibleMessagesDispatch(nil, openaiOff))
	openaiOn := &service.APIKey{Group: &service.Group{Platform: service.PlatformOpenAI, AllowMessagesDispatch: true}}
	require.True(t, allowOpenAICompatibleMessagesDispatch(nil, openaiOn))
}

func TestAllowOpenAICompatibleMessagesDispatch_CompositeResolvedTargets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gateSvc := newCompositeGateGatewayService(t, nil, nil)

	newCompositeCtx := func(model string, allow bool) (*gin.Context, *service.APIKey) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
		apiKey := &service.APIKey{Group: &service.Group{Platform: service.PlatformComposite, AllowMessagesDispatch: allow}}
		ensureCompositeTargetPlatform(c, apiKey, model, gateSvc)
		return c, apiKey
	}

	// 解析到 grok/CN 目标：与对应独立分组同语义豁免。
	for _, model := range []string{"grok-4.3", "kimi-k2-thinking", "glm-5.2", "deepseek-v3.2"} {
		c, apiKey := newCompositeCtx(model, false)
		require.True(t, allowOpenAICompatibleMessagesDispatch(c, apiKey), "model=%s", model)
	}

	// 解析到 openai 目标：受 composite 分组自身开关控制。
	c, apiKey := newCompositeCtx("gpt-5.5", false)
	require.False(t, allowOpenAICompatibleMessagesDispatch(c, apiKey))
	c, apiKey = newCompositeCtx("gpt-5.5", true)
	require.True(t, allowOpenAICompatibleMessagesDispatch(c, apiKey))

	// 未解析出目标平台：保持拒绝，不放宽。
	cNone, _ := gin.CreateTestContext(httptest.NewRecorder())
	cNone.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	require.False(t, allowOpenAICompatibleMessagesDispatch(cNone,
		&service.APIKey{Group: &service.Group{Platform: service.PlatformComposite, AllowMessagesDispatch: false}}))
}

// composite 解析到 grok/CN 目标时，Group 级调度映射（gpt-5.x 默认值为 openai
// 专属）不得注入，模型改写完全交给账号级 model_mapping。
func TestResolveOpenAIMessagesDispatchMappedModel_CompositeCNTargetsSkipGroupMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gateSvc := newCompositeGateGatewayService(t, nil, nil)

	for _, model := range []string{"kimi-k2-thinking", "glm-5.2", "deepseek-v3.2", "grok-4.3"} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
		apiKey := &service.APIKey{Group: &service.Group{Platform: service.PlatformComposite}}
		ensureCompositeTargetPlatform(c, apiKey, model, gateSvc)

		require.Empty(t, resolveOpenAIMessagesDispatchMappedModel(c, apiKey, "claude-sonnet-4-5-20250929"), "model=%s", model)
	}

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	c.Request = c.Request.WithContext(service.WithResolvedTargetPlatform(c.Request.Context(), service.PlatformOpenCodeGo))
	apiKey := &service.APIKey{Group: &service.Group{Platform: service.PlatformComposite}}
	require.Empty(t, resolveOpenAIMessagesDispatchMappedModel(c, apiKey, "claude-sonnet-4-5-20250929"),
		"composite → opencode_go 不得注入 openai 默认 gpt-5.x 映射")
}

// composite 解析到 codebuddy 目标时同样不得注入 openai 专属的 gpt-5.x 默认值。
func TestResolveOpenAIMessagesDispatchMappedModel_CompositeCodeBuddyTargetSkipsGroupMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	c.Request = c.Request.WithContext(service.WithResolvedTargetPlatform(c.Request.Context(), service.PlatformCodeBuddy))
	apiKey := &service.APIKey{Group: &service.Group{Platform: service.PlatformComposite}}

	require.Empty(t, resolveOpenAIMessagesDispatchMappedModel(c, apiKey, "claude-sonnet-4-5-20250929"))
}
