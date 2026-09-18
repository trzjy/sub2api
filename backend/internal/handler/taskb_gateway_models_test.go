package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Task B：网关 /v1/models 按接入模式回落 web 模型目录（官方平台 + web 接入模式，
// 取代旧平台值判定，方案 §5.5 / B4 口径）。

// 官方 zhipu 分组（归并后 platform 已是官方值）+ web 账号存在：空 model_mapping 时
// 回落 web 模型目录，而非误回落到 Claude 默认模型。
func TestTaskBGatewayModels_ZhipuGroupWithWebAccountReturnsWebModels(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(31)
	h := newGatewayModelsHandlerForTest(
		&gatewayModelsAccountRepoStub{
			byGroup: map[int64][]service.Account{
				groupID: {
					{ID: 1, Platform: service.PlatformZhipu, Credentials: map[string]any{"access_mode": service.AccountAccessModeWeb, "cookie": "c"}},
				},
			},
		},
	)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{ID: groupID, Platform: service.PlatformZhipu},
	})

	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, "list", got.Object)
	require.Contains(t, modelIDsForTest(got.Data), "glm-5.3-flash", "zhipu group with web account advertises web model")
	require.NotContains(t, modelIDsForTest(got.Data), "claude-sonnet-4-6", "must not fall back to Claude defaults")
}

// 官方 zhipu 分组 + web 接入模式账号：空 model_mapping 时回落 web 模型目录（归并后唯一形态）。
func TestTaskBGatewayModels_ZhipuGroupWebModeReturnsWebModels(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(32)
	h := newGatewayModelsHandlerForTest(
		&gatewayModelsAccountRepoStub{
			byGroup: map[int64][]service.Account{
				groupID: {
					{ID: 1, Platform: service.PlatformZhipu, Credentials: map[string]any{"access_mode": service.AccountAccessModeWeb, "cookie": "c"}},
				},
			},
		},
	)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{ID: groupID, Platform: service.PlatformZhipu},
	})

	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)
	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Contains(t, modelIDsForTest(got.Data), "glm-5.3-flash")
}
