package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// cursorOpenAICompatAccount 构造「cursor 平台 + APIKey + OpenAI 兼容反代」账号，
// 与生产故障账号（cursor/acpikey，base https://kn-cy.com/v1）同形态。
func cursorOpenAICompatAccount() *Account {
	return &Account{
		ID:       201,
		Platform: PlatformCursor,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "kn-cy-key",
			"base_url": "https://kn-cy.com/v1",
			"model_mapping": map[string]any{
				"gemini-3.8-flash": "gemini-3.8-flash-mapped",
			},
		},
		Concurrency: 1,
	}
}

func TestForwardCursorOpenAICompatibleMessages(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("non-stream routes to chat completions and returns anthropic shape", func(t *testing.T) {
		upstreamBody := `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gemini-3.8-flash-mapped",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"hello from kn-cy"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14,` +
			`"prompt_tokens_details":{"cached_tokens":3,"cache_write_tokens":2}}}`
		httpStub := &geminiCompatHTTPUpstreamStub{
			response: &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"application/json"},
					"X-Request-Id": []string{"req-1"},
				},
				Body: io.NopCloser(strings.NewReader(upstreamBody)),
			},
		}
		svc := &GeminiMessagesCompatService{
			httpUpstream: httpStub,
			cfg:          &config.Config{},
		}

		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		body := []byte(`{"model":"gemini-3.8-flash","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))

		result, err := svc.Forward(context.Background(), c, cursorOpenAICompatAccount(), body)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, http.StatusOK, rec.Code)

		// ① 出站 URL 为 <base>/chat/completions，不含 /v1beta/
		require.NotNil(t, httpStub.lastReq)
		require.Equal(t, 1, httpStub.calls)
		require.Equal(t, "https://kn-cy.com/v1/chat/completions", httpStub.lastReq.URL.String())
		require.NotContains(t, httpStub.lastReq.URL.String(), "/v1beta/")
		require.Equal(t, http.MethodPost, httpStub.lastReq.Method)
		require.Equal(t, "application/json", httpStub.lastReq.Header.Get("Content-Type"))

		// ② Authorization: Bearer + 请求体含 messages 且 model 为映射后的值
		require.Equal(t, "Bearer kn-cy-key", httpStub.lastReq.Header.Get("Authorization"))
		sentBody, err := io.ReadAll(httpStub.lastReq.Body)
		require.NoError(t, err)
		var sent map[string]any
		require.NoError(t, json.Unmarshal(sentBody, &sent))
		require.Equal(t, "gemini-3.8-flash-mapped", sent["model"])
		messages, ok := sent["messages"].([]any)
		require.True(t, ok)
		require.NotEmpty(t, messages)

		// ③ 非流式响应回给客户端的是 Anthropic 形状，Usage 与上游一致
		var got map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		require.Equal(t, "message", got["type"])
		require.Equal(t, "assistant", got["role"])
		require.Equal(t, "gemini-3.8-flash", got["model"])
		require.Contains(t, got, "content")
		usage, ok := got["usage"].(map[string]any)
		require.True(t, ok)
		// input = prompt(10) - cached(3) - cache_write(2)
		require.Equal(t, float64(5), usage["input_tokens"])
		require.Equal(t, float64(4), usage["output_tokens"])
		require.Equal(t, float64(3), usage["cache_read_input_tokens"])
		require.Equal(t, float64(2), usage["cache_creation_input_tokens"])

		require.Equal(t, 5, result.Usage.InputTokens)
		require.Equal(t, 4, result.Usage.OutputTokens)
		require.Equal(t, 3, result.Usage.CacheReadInputTokens)
		require.Equal(t, 2, result.Usage.CacheCreationInputTokens)
		require.Equal(t, "req-1", result.RequestID)
		require.Equal(t, "gemini-3.8-flash", result.Model)
		require.Equal(t, "gemini-3.8-flash-mapped", result.UpstreamModel)
		require.False(t, result.Stream)
	})

	t.Run("stream returns anthropic SSE events and finalizes with message_stop", func(t *testing.T) {
		upstreamBody := `data: {"id":"chatcmpl-2","object":"chat.completion.chunk","created":1,"model":"gemini-3.8-flash-mapped","choices":[{"index":0,"delta":{"role":"assistant","content":"hel"},"finish_reason":null}]}` + "\n\n" +
			`data: {"id":"chatcmpl-2","object":"chat.completion.chunk","created":1,"model":"gemini-3.8-flash-mapped","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}` + "\n\n" +
			`data: {"id":"chatcmpl-2","object":"chat.completion.chunk","created":1,"model":"gemini-3.8-flash-mapped","choices":[],"usage":{"prompt_tokens":8,"completion_tokens":2,"total_tokens":10,"prompt_tokens_details":{"cached_tokens":1,"cache_write_tokens":4}}}` + "\n\n" +
			"data: [DONE]\n\n"
		httpStub := &geminiCompatHTTPUpstreamStub{
			response: &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"text/event-stream; charset=utf-8"},
					"X-Request-Id": []string{"req-2"},
				},
				Body: io.NopCloser(strings.NewReader(upstreamBody)),
			},
		}
		svc := &GeminiMessagesCompatService{
			httpUpstream: httpStub,
			cfg:          &config.Config{},
		}

		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		body := []byte(`{"model":"gemini-3.8-flash","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))

		result, err := svc.Forward(context.Background(), c, cursorOpenAICompatAccount(), body)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, http.StatusOK, rec.Code)

		require.NotNil(t, httpStub.lastReq)
		require.Equal(t, "https://kn-cy.com/v1/chat/completions", httpStub.lastReq.URL.String())
		require.NotContains(t, httpStub.lastReq.URL.String(), "/v1beta/")
		require.Equal(t, "Bearer kn-cy-key", httpStub.lastReq.Header.Get("Authorization"))
		sentBody, err := io.ReadAll(httpStub.lastReq.Body)
		require.NoError(t, err)
		var sent map[string]any
		require.NoError(t, json.Unmarshal(sentBody, &sent))
		require.Equal(t, "gemini-3.8-flash-mapped", sent["model"])
		require.Equal(t, true, sent["stream"])
		streamOpts, ok := sent["stream_options"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, true, streamOpts["include_usage"])

		// ④ 流式响应包含 Anthropic SSE 事件，结束后有 message_stop
		out := rec.Body.String()
		require.Contains(t, out, "event: message_start")
		require.Contains(t, out, "event: content_block_delta")
		require.Contains(t, out, `"text":"hel"`)
		require.Contains(t, out, `"text":"lo"`)
		require.Contains(t, out, "event: message_delta")
		require.Contains(t, out, "event: message_stop")
		require.Equal(t, "text/event-stream; charset=utf-8", rec.Header().Get("Content-Type"))

		require.True(t, result.Stream)
		require.Equal(t, "req-2", result.RequestID)
		// input = prompt(8) - cached(1) - cache_write(4)
		require.Equal(t, 3, result.Usage.InputTokens)
		require.Equal(t, 2, result.Usage.OutputTokens)
		require.Equal(t, 1, result.Usage.CacheReadInputTokens)
		require.Equal(t, 4, result.Usage.CacheCreationInputTokens)
		require.NotNil(t, result.FirstTokenMs)
	})

	t.Run("non-compatible cursor account stays on cursor native path", func(t *testing.T) {
		// 非兼容 cursor 账号（OAuth）必须仍进 forwardCursorMessages，不触碰 httpUpstream。
		// cursorExecution 故意留 nil：该分支会在 requireCursorExecution 处失败关闭，
		// 借此断言它没有误入 OpenAI 兼容分支。
		httpStub := &geminiCompatHTTPUpstreamStub{}
		svc := &GeminiMessagesCompatService{
			httpUpstream: httpStub,
			cfg:          &config.Config{},
		}
		account := &Account{
			ID:       202,
			Platform: PlatformCursor,
			Type:     AccountTypeOAuth,
			Credentials: map[string]any{
				"access_token": "cursor-oauth-token",
			},
			Concurrency: 1,
		}

		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		body := []byte(`{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"hi"}]}`)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))

		result, err := svc.Forward(context.Background(), c, account, body)
		require.Error(t, err)
		require.Nil(t, result)
		require.Equal(t, 0, httpStub.calls, "non-compatible cursor must not use the OpenAI-compatible httpUpstream path")
	})
}
