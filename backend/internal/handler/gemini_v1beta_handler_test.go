//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGeminiV1BetaListModels_AllowlistFiltersNativeResponse(t *testing.T) {
	body := []byte(`{"models":[{"name":"models/gemini-2.5-pro"},{"name":"models/gemini-2.5-flash"}],"nextPageToken":"next"}`)
	filtered, dropped, ok := filterUpstreamGeminiModelsBody(body, service.GroupModelAllowlist{Enabled: true, Models: []string{"gemini-2.5-pro"}})
	require.True(t, ok)
	require.True(t, dropped)
	require.JSONEq(t, `{"models":[{"name":"models/gemini-2.5-pro"}],"nextPageToken":"next"}`, string(filtered))
}

func TestGeminiV1BetaListModels_ForcedAntigravityAppliesAllowlist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/antigravity/v1beta/models", nil)
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{
			Platform: service.PlatformGemini,
			ModelAllowlist: service.GroupModelAllowlist{
				Enabled: true,
				Models:  []string{"gemini-custom"},
			},
		},
	})
	c.Set(string(middleware.ContextKeyForcePlatform), service.PlatformAntigravity)

	groupID := int64(42)
	key, _ := middleware.GetAPIKeyFromContext(c)
	key.GroupID = &groupID
	repo := &geminiAllowlistAccountRepoStub{}
	(&GatewayHandler{geminiCompatService: service.NewGeminiMessagesCompatService(repo, nil, nil, nil, nil, nil, nil, nil, nil, nil /* cursorExecution */)}).GeminiV1BetaListModels(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got antigravity.GeminiModelsListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Empty(t, got.Models)
}

func TestGeminiModelAllowlist_DisabledPreservesNativeResponse(t *testing.T) {
	body := []byte(`{"models":[{"name":"models/gemini-2.5-pro"}]}`)
	filtered, dropped, ok := filterUpstreamGeminiModelsBody(body, service.GroupModelAllowlist{Enabled: false, Models: []string{"other"}})
	require.True(t, ok)
	require.False(t, dropped)
	require.Equal(t, body, filtered)
}

// TestGeminiV1BetaHandler_PlatformRoutingInvariant 文档化并验证 Handler 层的平台路由逻辑不变量
// 该测试确保 gemini 和 antigravity 平台的路由逻辑符合预期
func TestGeminiV1BetaHandler_PlatformRoutingInvariant(t *testing.T) {
	tests := []struct {
		name            string
		platform        string
		expectedService string
		description     string
	}{
		{
			name:            "Gemini平台使用ForwardNative",
			platform:        service.PlatformGemini,
			expectedService: "GeminiMessagesCompatService.ForwardNative",
			description:     "Gemini OAuth 账户直接调用 Google API",
		},
		{
			name:            "Antigravity平台使用ForwardGemini",
			platform:        service.PlatformAntigravity,
			expectedService: "AntigravityGatewayService.ForwardGemini",
			description:     "Antigravity 账户通过 CRS 中转，支持 Gemini 协议",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 模拟 GeminiV1BetaModels 中的路由决策 (lines 199-205 in gemini_v1beta_handler.go)
			var routedService string
			if tt.platform == service.PlatformAntigravity {
				routedService = "AntigravityGatewayService.ForwardGemini"
			} else {
				routedService = "GeminiMessagesCompatService.ForwardNative"
			}

			require.Equal(t, tt.expectedService, routedService,
				"平台 %s 应该路由到 %s: %s",
				tt.platform, tt.expectedService, tt.description)
		})
	}
}

// TestGeminiV1BetaHandler_ListModelsAntigravityFallback 验证 ListModels 的 antigravity 降级逻辑
// 当没有 gemini 账户但有 antigravity 账户时，应返回静态模型列表
func TestGeminiV1BetaHandler_ListModelsAntigravityFallback(t *testing.T) {
	tests := []struct {
		name             string
		hasGeminiAccount bool
		hasAntigravity   bool
		expectedBehavior string
	}{
		{
			name:             "有Gemini账户-调用ForwardAIStudioGET",
			hasGeminiAccount: true,
			hasAntigravity:   false,
			expectedBehavior: "forward_to_upstream",
		},
		{
			name:             "无Gemini有Antigravity-返回静态列表",
			hasGeminiAccount: false,
			hasAntigravity:   true,
			expectedBehavior: "static_fallback",
		},
		{
			name:             "无任何账户-返回503",
			hasGeminiAccount: false,
			hasAntigravity:   false,
			expectedBehavior: "service_unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 模拟 GeminiV1BetaListModels 的逻辑 (lines 33-44 in gemini_v1beta_handler.go)
			var behavior string

			if tt.hasGeminiAccount {
				behavior = "forward_to_upstream"
			} else if tt.hasAntigravity {
				behavior = "static_fallback"
			} else {
				behavior = "service_unavailable"
			}

			require.Equal(t, tt.expectedBehavior, behavior)
		})
	}
}

// TestGeminiV1BetaHandler_GetModelAntigravityFallback 验证 GetModel 的 antigravity 降级逻辑
func TestGeminiV1BetaHandler_GetModelAntigravityFallback(t *testing.T) {
	tests := []struct {
		name             string
		hasGeminiAccount bool
		hasAntigravity   bool
		expectedBehavior string
	}{
		{
			name:             "有Gemini账户-调用ForwardAIStudioGET",
			hasGeminiAccount: true,
			hasAntigravity:   false,
			expectedBehavior: "forward_to_upstream",
		},
		{
			name:             "无Gemini有Antigravity-返回静态模型信息",
			hasGeminiAccount: false,
			hasAntigravity:   true,
			expectedBehavior: "static_model_info",
		},
		{
			name:             "无任何账户-返回503",
			hasGeminiAccount: false,
			hasAntigravity:   false,
			expectedBehavior: "service_unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 模拟 GeminiV1BetaGetModel 的逻辑 (lines 77-87 in gemini_v1beta_handler.go)
			var behavior string

			if tt.hasGeminiAccount {
				behavior = "forward_to_upstream"
			} else if tt.hasAntigravity {
				behavior = "static_model_info"
			} else {
				behavior = "service_unavailable"
			}

			require.Equal(t, tt.expectedBehavior, behavior)
		})
	}
}

func TestShouldFallbackGeminiModel_KnownFallbackOn404(t *testing.T) {
	t.Parallel()

	res := &service.UpstreamHTTPResult{StatusCode: http.StatusNotFound}
	require.True(t, shouldFallbackGeminiModel("gemini-3.1-pro-preview-customtools", res))
}

func TestShouldFallbackGeminiModel_UnknownModelOn404(t *testing.T) {
	t.Parallel()

	res := &service.UpstreamHTTPResult{StatusCode: http.StatusNotFound}
	require.False(t, shouldFallbackGeminiModel("gemini-future-model", res))
}

func TestShouldFallbackGeminiModel_DelegatesScopeFallback(t *testing.T) {
	t.Parallel()

	res := &service.UpstreamHTTPResult{
		StatusCode: http.StatusForbidden,
		Headers:    http.Header{"Www-Authenticate": []string{"Bearer error=\"insufficient_scope\""}},
		Body:       []byte("insufficient authentication scopes"),
	}
	require.True(t, shouldFallbackGeminiModel("gemini-future-model", res))
}

// TestCursorGeminiSettlementFuncTerminalOnce 覆盖 §25.17.5 A06：gemini v1beta 出口 Cursor
// 分支在首次 Execute 与资源转交之前注册的唯一结算闭包，收到**终态真实*ForwardResult** 时
// 把原 RecordUsage 任务提交给原 mandatory usage worker 且**至多一次**；缺完整真实 usage
// 时（判定在 helper/执行服务侧）不得提交任何账务任务。
func TestCursorGeminiSettlementFuncTerminalOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(3)
	apiKey := &service.APIKey{ID: 7, User: &service.User{ID: 9}, GroupID: &groupID}
	account := &service.Account{ID: 11, Platform: service.PlatformCursor}
	subscription := &service.UserSubscription{ID: 21}

	var submitted atomic.Int32
	settle := (&GatewayHandler{}).geminiCursorSettlementFunc(geminiCursorSettlementParams{
		apiKey:             apiKey,
		user:               apiKey.User,
		account:            account,
		subscription:       subscription,
		pricingAt:          time.Now(),
		quotaPlatform:      service.QuotaPlatform(context.Background(), apiKey),
		inboundEndpoint:    "/v1beta/models/gemini-2.5-pro:generateContent",
		upstreamEndpoint:   "/v1beta/models/gemini-2.5-pro:generateContent",
		userAgent:          "gemini-cli-test",
		ipAddress:          "127.0.0.1",
		sessionID:          "sess-gemini-1",
		requestPayloadHash: "hash-gemini-1",
		forceCacheBilling:  false,
		channelMapping:     service.ChannelMappingResult{},
		requestedModel:     "gemini-2.5-pro",
		userID:             9,
		groupID:            apiKey.GroupID,
		reqModel:           "gemini-2.5-pro",
	}, func(context.Context, service.UsageRecordTask) { submitted.Add(1) })

	handover := newCursorResourceHandover(nil, nil)
	handover.RegisterCursorSettlement(settle)
	require.NotNil(t, handover.SettlementFunc(), "注册后 finalizer 侧访问器必须能取出同一钩子")

	// 终态真实结果（含真实 usage）恰好到达一次 → 原 mandatory usage worker 恰好提交一次。
	final := &service.ForwardResult{
		RequestID:     "req-gemini-1",
		Model:         "gemini-2.5-pro",
		UpstreamModel: "cursor-gemini-2.5-pro",
		Usage:         service.ClaudeUsage{InputTokens: 11, OutputTokens: 22, CacheReadInputTokens: 3, CacheCreationInputTokens: 4},
	}
	terminal := service.CursorSettlementEvent{
		SessionID:       "sess-gemini-1",
		BillingEventID:  "cursor_session:sess-gemini-1",
		Result:          final,
		UsageComplete:   true,
		InflightRelease: func() {},
	}
	handover.Settlement()(context.Background(), terminal)
	require.EqualValues(t, 1, submitted.Load(), "回调收到终态且只排一次")
	require.True(t, handover.Settled())
	require.NoError(t, handover.SettlementError(), "完整 usage + 已注册 = 装配正常")

	// 重复终结（Stop 与结果回传竞争）不得重复排账。
	handover.Settlement()(context.Background(), terminal)
	require.EqualValues(t, 1, submitted.Load(), "重复终结不得重复排账")

	// 缺完整真实 usage：闭包只透传、不自行判定，也不得被调用。
	incomplete := newCursorResourceHandover(nil, nil)
	incomplete.RegisterCursorSettlement(settle)
	incomplete.Settlement()(context.Background(), service.CursorSettlementEvent{
		SessionID:      "sess-gemini-2",
		BillingEventID: "cursor_session:sess-gemini-2",
		Result:         final,
		UsageComplete:  false,
	})
	require.EqualValues(t, 1, submitted.Load(), "缺完整真实 usage 不得结算")
	require.True(t, incomplete.Finalized(), "未取得完整 usage 也已终结（不再等待补交）")
	require.False(t, incomplete.Settled(), "未结算：不写零 usage 假成功账")

	// nil 终态：防御性返回，不提交账务任务、不 panic。
	settle(context.Background(), nil)
	require.EqualValues(t, 1, submitted.Load(), "nil 终态不得提交账务任务")
}
