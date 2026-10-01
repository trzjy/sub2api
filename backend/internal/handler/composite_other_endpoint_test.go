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
