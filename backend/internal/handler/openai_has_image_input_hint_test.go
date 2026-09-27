package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestChatCompletionsAndResponsesSeedHasImageInputHint 验证 chat_completions 与
// responses 两个入口在 body 解析后把 hasImageInput 判定写入 request context
// （供派发单 C 读取）。不依赖完整调度：用合法请求触发 handler，在到达账号
// 选择前（billing 资格或早期错误）即已写入 hint。
func TestChatCompletionsAndResponsesSeedHasImageInputHint(t *testing.T) {
	gin.SetMode(gin.TestMode)

	entries := []struct {
		name    string
		path    string
		body    string
		handler func(h *OpenAIGatewayHandler, c *gin.Context)
		want    bool
	}{
		{
			name:    "chat_completions image_url seeds true",
			path:    "/openai/v1/chat/completions",
			body:    `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]}]}`,
			handler: func(h *OpenAIGatewayHandler, c *gin.Context) { h.ChatCompletions(c) },
			want:    true,
		},
		{
			name:    "chat_completions text only seeds false",
			path:    "/openai/v1/chat/completions",
			body:    `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hello"}]}`,
			handler: func(h *OpenAIGatewayHandler, c *gin.Context) { h.ChatCompletions(c) },
			want:    false,
		},
		{
			name:    "responses input_image seeds true",
			path:    "/openai/v1/responses",
			body:    `{"model":"deepseek-v4.1-flash","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"what is this"},{"type":"input_image","image_url":"data:image/png;base64,AA=="}]}]}`,
			handler: func(h *OpenAIGatewayHandler, c *gin.Context) { h.Responses(c) },
			want:    true,
		},
		{
			name:    "responses text only seeds false",
			path:    "/openai/v1/responses",
			body:    `{"model":"deepseek-v4.1-flash","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
			handler: func(h *OpenAIGatewayHandler, c *gin.Context) { h.Responses(c) },
			want:    false,
		},
	}

	for _, tt := range entries {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := runOpenAIHandlerForImageInput(t, tt.path, tt.body, tt.handler)

			got, known := service.GetOpenAIHasImageInputHint(c)
			require.True(t, known, "handler must always seed the hint before account selection")
			require.Equal(t, tt.want, got, "body=%s", tt.body)
		})
	}
}

// TestChatCompletionsDoesNotSeedHasImageInputOnInvalidBody 验证 body 非法时
// 不写入 hint（不进入检测/调度）。
func TestChatCompletionsDoesNotSeedHasImageInputOnInvalidBody(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(`{"model":`))
	c.Request.Header.Set("Content-Type", "application/json")
	setImageChatTestAuth(c)

	newServiceTierHandlerTest(t).ChatCompletions(c)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	_, known := service.GetOpenAIHasImageInputHint(c)
	require.False(t, known, "invalid body must not seed hint")
}

// runOpenAIHandlerForImageInput 组装最小可用的 OpenAI 兼容入口上下文并调用 handler。
// responses 入口需要 HTTP client transport 标记，chat_completions 入口不需要。
func runOpenAIHandlerForImageInput(t *testing.T, path, body string, handler func(h *OpenAIGatewayHandler, c *gin.Context)) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	groupID := int64(7801)
	userID := int64(7802)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID:      7803,
		GroupID: &groupID,
		Group:   &service.Group{ID: groupID, Platform: service.PlatformOpenAI},
		User:    &service.User{ID: userID, Status: service.StatusActive},
	})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: userID, Concurrency: 1})

	h := newServiceTierHandlerTest(t)
	handler(h, c)
	return c, rec
}
