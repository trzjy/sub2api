//go:build unit

package handler

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
)

// CARD-06 §背景1：复合分组不支持模型时，统一返回 404 model_not_found +
// 中文提示（infraerrors.ModelNotInGroup）。
func TestOpenAIChatCompletionsCompositeModelNotFoundReturns404(t *testing.T) {
	gin.SetMode(gin.TestMode)

	apiKey := &service.APIKey{
		ID:     1,
		UserID: 1,
		User:   &service.User{ID: 1},
		Group:  &service.Group{ID: 1, Platform: service.PlatformComposite},
	}
	// 复合分组下未找到任何目标平台映射决策 → compositeTargetPlatformAllowed 返回 false
	ctx := context.Background()

	cache := &concurrencyCacheMock{
		acquireUserSlotFn: func(context.Context, int64, int, string) (bool, error) {
			return true, nil
		},
	}
	h := newOpenAIImageChatRejectionHandlerWithCache(t, cache)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := []byte(`{"model":"some-kimi-model","messages":[{"role":"user","content":"hi"}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)).WithContext(ctx)
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID, Concurrency: 1})

	h.ChatCompletions(c)
	t.Logf("body: %s", recorder.Body.String())
	require.Equal(t, http.StatusNotFound, recorder.Code, "composite 不支持模型必须返回 404")
	require.Equal(t, "model_not_found", gjson.Get(recorder.Body.String(), "error.type").String())
	require.Contains(t, gjson.Get(recorder.Body.String(), "error.message").String(),
		fmt.Sprintf(infraerrors.ModelNotInGroup, "some-kimi-model"),
		"必须携带中文 model_not_found 提示")
}

// 回归：composite 解析到 OpenAI 兼容目标平台（muse）时仍放行，不误伤 404。
func TestOpenAIChatCompletionsCompositeMusePlatformStillAllowed(t *testing.T) {
	gin.SetMode(gin.TestMode)

	apiKey := &service.APIKey{
		ID:     1,
		UserID: 1,
		User:   &service.User{ID: 1},
		Group:  &service.Group{ID: 1, Platform: service.PlatformComposite},
	}
	ctx := service.WithCompositeRouteDecision(context.Background(), service.CompositeRouteDecision{
		Matched:        true,
		TargetPlatform: service.PlatformMuse,
	})

	cache := &concurrencyCacheMock{
		acquireUserSlotFn: func(context.Context, int64, int, string) (bool, error) {
			return true, nil
		},
	}
	h := newOpenAIImageChatRejectionHandlerWithCache(t, cache)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := []byte(`{"model":"muse-spark-1.3","messages":[{"role":"user","content":"hi"}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)).WithContext(ctx)
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID, Concurrency: 1})

	h.ChatCompletions(c)

	require.NotEqual(t, http.StatusNotFound, recorder.Code,
		"composite 解析到 muse 目标不应被 404 拒绝")
	require.NotEqual(t, "model_not_found", gjson.Get(recorder.Body.String(), "error.type").String())
}
