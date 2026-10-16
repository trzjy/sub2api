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

// muse 平台模型目录不得回落默认模型（系统托管目录，照 PlatformOther 返回 nil）。
// muse-3 派发单：muse 有系统托管目录，不回落默认模型。
func TestDefaultModelIDsForPlatform_MuseNoFallback(t *testing.T) {
	require.Nil(t, defaultModelIDsForPlatform(service.PlatformMuse),
		"muse 公开模型列表必须为空（不回落 Claude 默认模型）")
}

// composite 聚合模型列表应遍历到 muse 平台，并暴露 muse 账号的 model_mapping 键。
func TestCompositeAvailableModelsIncludesMuseMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(95)
	repo := &gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			groupID: {
				{
					ID:       1,
					Platform: service.PlatformMuse,
					Credentials: map[string]any{
						"model_mapping": map[string]any{"muse-spark-1.3": "muse-spark-1.3-contributor"},
					},
				},
			},
		},
	}
	h := newGatewayModelsHandlerForTest(repo)
	// 与 /v1/models 聚合入口同口径：不按接入点过滤，且含 systemone 托管目录。
	models := h.compositeAvailableModels(context.Background(), &groupID, "", true)
	require.Contains(t, models, "muse-spark-1.3",
		"composite 聚合应暴露 muse 账号 model_mapping 键（切片须覆盖 muse）")
}

// composite 分组解析到 muse 目标平台时，openAICompatibleTextTargetAllowed 应放行
// （照 MiniMax 同列入口，muse 经 OpenAI 网关转发）。
func TestOpenAICompatibleTextTargetAllowedAdmitsMusePlatform(t *testing.T) {
	gin.SetMode(gin.TestMode)
	apiKey := &service.APIKey{Group: &service.Group{
		ID:       95,
		Platform: service.PlatformComposite,
	}}
	ctx := service.WithCompositeRouteDecision(context.Background(), service.CompositeRouteDecision{
		Matched:        true,
		TargetPlatform: service.PlatformMuse,
	})
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)

	require.True(t, openAICompatibleTextTargetAllowed(c, apiKey, "muse-spark-1.3", nil),
		"composite 解析到 muse 目标应放行")
}

// composite 分组解析到 codebuddy 目标（space-bunny 经 DetectModelPlatform 自动生效）时，
// openAICompatibleTextTargetAllowed 应放行——否则 /v1/chat/completions、/v1/responses、
// count_tokens 等 OpenAI 兼容文本入口会对 space-bunny 恒 400。
func TestOpenAICompatibleTextTargetAllowedAdmitsCodeBuddyPlatform(t *testing.T) {
	gin.SetMode(gin.TestMode)
	apiKey := &service.APIKey{Group: &service.Group{
		ID:       94,
		Platform: service.PlatformComposite,
	}}
	ctx := service.WithCompositeRouteDecision(context.Background(), service.CompositeRouteDecision{
		Matched:        true,
		TargetPlatform: service.PlatformCodeBuddy,
	})
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil).WithContext(ctx)

	require.True(t, openAICompatibleTextTargetAllowed(c, apiKey, "space-bunny", nil),
		"composite 解析到 codebuddy 目标应放行")
}

// composite 聚合模型列表须遍历 codebuddy 平台，否则 space-bunny 不出现在 /v1/models
// （codebuddy 账号无 model_mapping，模型来自上游目录快照，合并点在
// GetAvailableModels 的 platform == PlatformCodeBuddy 分支）。
func TestCompositeAvailableModelsIncludesCodeBuddySnapshotModels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(96)
	repo := &codeBuddyModelsHandlerRepoStub{gatewayModelsAccountRepoStub: gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			groupID: {
				newCodeBuddySnapshotAccount(300, map[string]service.UpstreamModelMetadata{
					"space-bunny": {ID: "space-bunny"},
				}),
			},
		},
	}}
	h := newGatewayModelsHandlerForTest(repo)

	models := h.compositeAvailableModels(context.Background(), &groupID, "", true)
	require.Contains(t, models, "space-bunny",
		"composite 聚合应暴露 codebuddy 账号快照模型（切片须覆盖 codebuddy）")
}
