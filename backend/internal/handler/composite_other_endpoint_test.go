package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// --- 接入点 5（openAICompatibleTextTargetAllowed）验收口径⑦ ---
//
// openAICompatibleTextTargetAllowed 经 compositeTargetPlatformAllowed 读取请求已解析的
// 目标平台（来自 context 中的 CompositeRouteDecision），与 allowed 平台列表比对。
// v2 将 service.PlatformOther 追加进该列表：decision.TargetPlatform=other 应放行；
// 未知平台（unknownplat）仍拒绝——锁定 allowed 列表严格等于白名单∪{other}，
// 杜绝"非空即放行"漂移。

func TestOpenAICompatibleTextTargetAllowedAdmitsOtherPlatform(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(94)
	apiKey := &service.APIKey{Group: &service.Group{
		ID:       groupID,
		Platform: service.PlatformComposite,
	}}

	// decision.TargetPlatform = other：v2 追加 PlatformOther 后应放行。
	ctxOther := service.WithCompositeRouteDecision(context.Background(), service.CompositeRouteDecision{
		Matched:        true,
		TargetPlatform: service.PlatformOther,
	})
	reqOther := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctxOther)
	recOther := httptest.NewRecorder()
	cOther, _ := gin.CreateTestContext(recOther)
	cOther.Request = reqOther

	require.True(t,
		openAICompatibleTextTargetAllowed(cOther, apiKey, "qwen3.8-flash", nil),
		"接入点5：composite 解析到 other 目标平台应放行")

	// 负向：未知平台（unknownplat）即使请求带决策，也必须拒绝。
	ctxUnknown := service.WithCompositeRouteDecision(context.Background(), service.CompositeRouteDecision{
		Matched:        true,
		TargetPlatform: "unknownplat",
	})
	reqUnknown := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctxUnknown)
	recUnknown := httptest.NewRecorder()
	cUnknown, _ := gin.CreateTestContext(recUnknown)
	cUnknown.Request = reqUnknown

	require.False(t,
		openAICompatibleTextTargetAllowed(cUnknown, apiKey, "weird-model", nil),
		"接入点5：未知平台不得经 allowed 列表放行")
}

// --- 接入点 6（compositeAvailableModels）验收口径⑧ ---
//
// compositeAvailableModels 按固定平台切片逐平台取 GetAvailableModels（=账号 model_mapping
// 键，纯字符串匹配）。v2 将 service.PlatformOther 追加进切片：含 other 账号 model_mapping
// 键（qwen3.8-flash）；不含未知平台（unknownplat）映射键——后者不在切片内，天然被排除。

func TestCompositeAvailableModelsIncludesOtherMappingExcludesUnknown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(94)
	repo := &gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			groupID: {
				{
					ID:       1,
					Platform: service.PlatformOther,
					Credentials: map[string]any{
						"model_mapping": map[string]any{"qwen3.8-flash": "qwen3.8-flash:free"},
					},
				},
				{
					ID:       2,
					Platform: "unknownplat", // 未知平台但配了有效映射
					Credentials: map[string]any{
						"model_mapping": map[string]any{"weird-model": "upstream-x"},
					},
				},
			},
		},
	}
	h := newGatewayModelsHandlerForTest(repo)

	models := h.compositeAvailableModels(context.Background(), &groupID)

	require.Contains(t, models, "qwen3.8-flash",
		"接入点6：/v1/models 应含 other 账号 model_mapping 键")
	require.NotContains(t, models, "weird-model",
		"接入点6：未知平台（unknownplat）映射键不得进入聚合模型列表")
}

// --- 接入点 7（allowOpenAICompatibleMessagesDispatch composite 豁免分支）验收口径⑨ ---
//
// composite 分组 AllowMessagesDispatch=false 时，门禁按"请求已解析的目标平台"裁决：
// 豁免集合 {Grok, Kimi, Zhipu, Deepseek, MiniMax, Other} 必须放行（与 CN 同语义豁免，
// other 走既有 forwardAnthropicViaRawChatCompletions 转换链）；其余具体平台
// {Anthropic, Gemini, OpenAI, Antigravity} 及未知平台（unknownplat）必须拒绝。
//
// 表驱动逐平台断言：拒绝"单点 other=true/openai=false"式漏检实现
// （误写 platform != PlatformOpenAI 会漏检其余非豁免平台）。

func TestAllowOpenAICompatibleMessagesDispatch_CompositeOtherExemptionTable(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newCompositeCtxWithTarget := func(target string) (*gin.Context, *service.APIKey) {
		ctx := context.Background()
		if target != "" {
			ctx = service.WithResolvedTargetPlatform(ctx, target)
		}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
		apiKey := &service.APIKey{Group: &service.Group{
			ID:                    94,
			Platform:              service.PlatformComposite,
			AllowMessagesDispatch: false,
		}}
		return c, apiKey
	}

	// 豁免集合：解析到这些目标平台必须放行。
	exemptTargets := []struct {
		name   string
		target string
	}{
		{"Grok", service.PlatformGrok},
		{"Kimi", service.PlatformKimi},
		{"Zhipu", service.PlatformZhipu},
		{"Deepseek", service.PlatformDeepseek},
		{"MiniMax", service.PlatformMiniMax},
		{"Other", service.PlatformOther},
	}
	for _, tc := range exemptTargets {
		c, apiKey := newCompositeCtxWithTarget(tc.target)
		require.True(t, allowOpenAICompatibleMessagesDispatch(c, apiKey),
			"接入点7：composite 解析到 %s 目标应豁免放行", tc.name)
	}

	// 拒绝集合：具体非豁免平台及未知平台必须拒绝（AllowMessagesDispatch=false）。
	denyTargets := []struct {
		name   string
		target string
	}{
		{"Anthropic", service.PlatformAnthropic},
		{"Gemini", service.PlatformGemini},
		{"OpenAI", service.PlatformOpenAI},
		{"Antigravity", service.PlatformAntigravity},
		{"Unknown", "unknownplat"},
	}
	for _, tc := range denyTargets {
		c, apiKey := newCompositeCtxWithTarget(tc.target)
		require.False(t, allowOpenAICompatibleMessagesDispatch(c, apiKey),
			"接入点7：composite 解析到 %s 目标不应豁免", tc.name)
	}
}

// 负向回归：豁免仅限 composite 分组条件内。非 composite 分组即使上下文携带解析目标
// Other，仍按其 Group 自身 AllowMessagesDispatch 开关裁决——不得继承 composite 分支的
// Other 豁免（单点 other=true 不能外溢到非 composite 分组的开关逻辑）。
//
// 注（漂移说明）：dispatch 示例写"platform=other 普通分组 → false"，但 v2 已落地的直连
// other 分组豁免（openai_gateway_handler.go:322，属接入点 1-6 已落地代码，禁区不动）使
// 直连 other 分组恒放行，故此处以非 composite 且非豁免直连平台（openai）+ 上下文携带
// Other 目标 表达同一语义：composite 分支的 Other 豁免不泄漏到非 composite 分组的开关裁决。
func TestAllowOpenAICompatibleMessagesDispatch_OtherExemptionScopedToComposite(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx := service.WithResolvedTargetPlatform(context.Background(), service.PlatformOther)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)

	// 非 composite 分组（openai 平台，非豁免直连平台），开关关闭：拒绝（不继承 composite 豁免）。
	apiKeyOff := &service.APIKey{Group: &service.Group{
		ID:                    1,
		Platform:              service.PlatformOpenAI,
		AllowMessagesDispatch: false,
	}}
	require.False(t, allowOpenAICompatibleMessagesDispatch(c, apiKeyOff),
		"接入点7：非 composite 分组携带 Other 上下文不得继承 composite 豁免")

	// 对照：同一非 composite 分组开关开启则放行（证明裁决来自开关本身）。
	apiKeyOn := &service.APIKey{Group: &service.Group{
		ID:                    1,
		Platform:              service.PlatformOpenAI,
		AllowMessagesDispatch: true,
	}}
	require.True(t, allowOpenAICompatibleMessagesDispatch(c, apiKeyOn),
		"接入点7：非 composite 分组开关开启时应放行")
}
