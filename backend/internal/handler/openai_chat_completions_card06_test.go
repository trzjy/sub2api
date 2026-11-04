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
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

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

// CARD-06 §背景2：分层路由 token 预估失败 Fail-Open——不阻断请求、不回 400，
// 保留基础 context 按原路径正常转发，并打 Warn 日志。
func TestOpenAIChatCompletionsTierRoutingFailOpenPreservesBaseContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &OpenAIGatewayHandler{}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	core, logs := observer.New(zap.WarnLevel)
	reqLog := zap.New(core)

	// 门控开启，但 messages 含不支持的 role，使 token 估算失败。
	gateOpen := func(string) (bool, error) { return true, nil }
	body := []byte(`{"model":"kimi-k3","messages":[{"role":"bogus","content":"hi"}]}`)

	baseCtx := c.Request.Context()
	got := h.applyTierRoutingFailOpen(c, reqLog, "kimi-k3", "kimi-k3",
		service.TierRoutingProtocolChatCompletions, body, gateOpen)

	require.True(t, baseCtx == got,
		"分层路由估算失败必须 Fail-Open 保留基础 context（不回 400）")
	require.Equal(t, 1, logs.FilterMessage("openai_chat_completions.kimi_tier_estimate_failed").Len(),
		"估算失败必须打 Warn 日志")
}

// 对照：门控未开启时 applyTierRouting 返回 baseCtx（nil error），Fail-Open 封装原样返回。
func TestOpenAIChatCompletionsTierRoutingGateClosedReturnsBaseContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &OpenAIGatewayHandler{}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqLog := zap.NewNop()

	gateOpen := func(string) (bool, error) { return false, nil }
	body := []byte(`{"model":"kimi-k3","messages":[{"role":"user","content":"hi"}]}`)

	baseCtx := c.Request.Context()
	got := h.applyTierRoutingFailOpen(c, reqLog, "kimi-k3", "kimi-k3",
		service.TierRoutingProtocolChatCompletions, body, gateOpen)

	require.True(t, baseCtx == got, "门控未开启必须回落基础 context，零行为变化")
}
