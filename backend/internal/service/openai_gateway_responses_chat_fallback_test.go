//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestForwardResponses_ForceChatCompletionsRoutesNonStreamingToChatCompletions(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.4","input":"hello","stream":false,"service_tier":"priority"}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid_resp_chat_json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"chatcmpl_json","object":"chat.completion","model":"gpt-5.4","service_tier":"default","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5,"prompt_tokens_details":{"cached_tokens":1}}}`,
		)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}
	SetActualOpenAIUpstreamEndpoint(c, "/v1/responses")

	result, err := svc.Forward(context.Background(), c, forceChatResponsesFallbackAccount(), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "http://upstream.example/v1/chat/completions", upstream.lastReq.URL.String())
	require.Equal(t, "/v1/chat/completions", GetActualOpenAIUpstreamEndpoint(c))
	require.Equal(t, HTTPUpstreamProfileOpenAI, HTTPUpstreamProfileFromContext(upstream.lastReq.Context()))
	require.Equal(t, "hello", gjson.GetBytes(upstream.lastBody, "messages.0.content").String())
	require.False(t, gjson.GetBytes(upstream.lastBody, "input").Exists())
	require.Equal(t, "response", gjson.Get(rec.Body.String(), "object").String())
	require.Equal(t, "ok", gjson.Get(rec.Body.String(), "output.0.content.0.text").String())
	require.Equal(t, 3, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)
	require.Equal(t, 1, result.Usage.CacheReadInputTokens)
	require.NotNil(t, result.ServiceTier)
	require.Equal(t, "priority", *result.ServiceTier)
	require.Equal(t, "default", result.UpstreamResponseServiceTier)
	require.False(t, result.Stream)
}

// Scenario: 第三方无推理模型不收到兼容档位。
func TestForwardResponses_ForceChatCompletionsOmitsNoneReasoningEffort(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"company-coding-model","input":"hello","reasoning":{"effort":"none"},"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"chatcmpl_none","object":"chat.completion","model":"company-coding-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	result, err := svc.Forward(context.Background(), c, forceChatResponsesFallbackAccount(), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "company-coding-model", gjson.GetBytes(upstream.lastBody, "model").String())
	require.False(t, gjson.GetBytes(upstream.lastBody, "reasoning_effort").Exists())
	require.Nil(t, result.ReasoningEffort)
}

func TestForwardResponses_PassthroughFlagWithUnsupportedResponsesUsesAccountMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, path := range []string{"/v1/responses", "/v1/responses/compact"} {
		path := path
		t.Run(path, func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.4-channel","input":"hello","stream":false}`)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")

			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"id":"chatcmpl_mapping","object":"chat.completion","model":"gpt-5.4-account","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
				)),
			}}
			svc := &OpenAIGatewayService{
				cfg:          rawChatCompletionsTestConfig(),
				httpUpstream: upstream,
			}
			account := rawChatCompletionsTestAccount()
			account.Credentials["model_mapping"] = map[string]any{
				"gpt-5.4-channel": "gpt-5.4-account",
			}
			account.Credentials["compact_model_mapping"] = map[string]any{
				"gpt-5.4-account": "gpt-5.4-compact",
			}
			account.Extra = map[string]any{
				"openai_passthrough":                     true,
				openai_compat.ExtraKeyResponsesSupported: false,
			}

			result, err := svc.Forward(context.Background(), c, account, body)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, "http://upstream.example/v1/chat/completions", upstream.lastReq.URL.String())
			require.Equal(t, "gpt-5.4-account", gjson.GetBytes(upstream.lastBody, "model").String())
		})
	}
}

func TestForwardResponses_ForceChatCompletionsRoutesStreamingToChatCompletions(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.4","input":"hello","stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"id":"chatcmpl_stream","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_stream","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"content":"he"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_stream","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"content":"llo"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_stream","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"",
		`data: {"id":"chatcmpl_stream","object":"chat.completion.chunk","model":"gpt-5.4","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_resp_chat_stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	result, err := svc.Forward(context.Background(), c, forceChatResponsesFallbackAccount(), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "http://upstream.example/v1/chat/completions", upstream.lastReq.URL.String())
	require.True(t, gjson.GetBytes(upstream.lastBody, "stream_options.include_usage").Bool())
	require.Contains(t, rec.Body.String(), "event: response.output_text.delta")
	require.Contains(t, rec.Body.String(), `"delta":"he"`)
	require.Contains(t, rec.Body.String(), "event: response.completed")
	require.Contains(t, rec.Body.String(), `"input_tokens":4`)
	require.Contains(t, rec.Body.String(), "data: [DONE]")
	require.Equal(t, 4, result.Usage.InputTokens)
	require.Equal(t, 3, result.Usage.OutputTokens)
	require.True(t, result.Stream)
	require.NotNil(t, result.FirstTokenMs)
}

func TestForwardResponses_ChatFallbackRejectsInvalidToolArgumentsAtOutputLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"deepseek-v4-flash","input":"run the command","stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"id":"chatcmpl_length_tool","object":"chat.completion.chunk","model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_length","type":"function","function":{"name":"exec_command","arguments":"{\"cmd\":\"ssh root@HOST"}}]},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_length_tool","object":"chat.completion.chunk","model":"deepseek-v4-flash","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":4,"completion_tokens":6492,"total_tokens":6496}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_length_tool"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	result, err := svc.Forward(context.Background(), c, forceChatResponsesFallbackAccount(), body)
	require.ErrorContains(t, err, "invalid JSON")
	require.NotNil(t, result)
	require.Equal(t, 4, result.Usage.InputTokens)
	require.Equal(t, 6492, result.Usage.OutputTokens)
	require.NotContains(t, rec.Body.String(), "response.function_call_arguments.done")
	require.NotContains(t, rec.Body.String(), "response.output_item.done")
	require.NotContains(t, rec.Body.String(), "data: [DONE]")
}

func TestForwardResponses_DeepSeekReasoningOnlyStreamProducesVisibleText(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"deepseek-reasoner","input":"hello","stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"id":"chatcmpl_reasoning","object":"chat.completion.chunk","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":""},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_reasoning","object":"chat.completion.chunk","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"reasoning_content":"visible fallback"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_reasoning","object":"chat.completion.chunk","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"content":""},"finish_reason":"length"}],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_deepseek_reasoning_responses_stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	result, err := svc.Forward(context.Background(), c, forceChatResponsesFallbackAccount(), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Stream)
	require.Contains(t, rec.Body.String(), "event: response.output_text.delta")
	require.Contains(t, rec.Body.String(), `"delta":"visible fallback"`)
	require.Contains(t, rec.Body.String(), `"status":"incomplete"`)
	require.Contains(t, rec.Body.String(), "data: [DONE]")
}

func TestForwardResponses_AutoSupportedAccountStillUsesResponsesEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.4","input":"hello","stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid_resp_native"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"resp_native","object":"response","model":"gpt-5.4","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}],"status":"completed"}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}`,
		)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}
	account := rawChatCompletionsTestAccount()
	account.Extra = map[string]any{
		openai_compat.ExtraKeyResponsesMode:      string(openai_compat.ResponsesSupportModeAuto),
		openai_compat.ExtraKeyResponsesSupported: true,
	}

	result, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "http://upstream.example/v1/responses", upstream.lastReq.URL.String())
	require.True(t, gjson.GetBytes(upstream.lastBody, "input").Exists())
	require.False(t, gjson.GetBytes(upstream.lastBody, "messages").Exists())
	require.Equal(t, "ok", gjson.Get(rec.Body.String(), "output.0.content.0.text").String())
}

func forceChatResponsesFallbackAccount() *Account {
	account := rawChatCompletionsTestAccount()
	account.Extra = map[string]any{
		openai_compat.ExtraKeyResponsesMode: string(openai_compat.ResponsesSupportModeForceChatCompletions),
	}
	return account
}

// reasoningRecordingCache 记录 reasoning 缓存写入、并按需响应回查。
type reasoningRecordingCache struct {
	stubGatewayCache
	mu      sync.Mutex
	sets    map[string]string
	getResp map[string]string
}

func (c *reasoningRecordingCache) SetReasoningContent(_ context.Context, itemID string, content string, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sets == nil {
		c.sets = make(map[string]string)
	}
	c.sets[itemID] = content
	return nil
}

func (c *reasoningRecordingCache) GetReasoningContent(_ context.Context, itemID string) (string, error) {
	if v, ok := c.getResp[itemID]; ok {
		return v, nil
	}
	return "", ErrReasoningContentNotFound
}

func (c *reasoningRecordingCache) snapshotSets() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]string, len(c.sets))
	for k, v := range c.sets {
		out[k] = v
	}
	return out
}

// 流式响应里的 reasoning_content 应按 reasoning item id 写入缓存，供后续轮次
// 客户端不回传明文 summary 时回注（DeepSeek thinking mode 400 修复的写入侧）。
func TestForwardResponses_ChatFallbackCachesStreamedReasoning(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"deepseek-reasoner","input":"hello","stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"id":"chatcmpl_rc","object":"chat.completion.chunk","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_rc","object":"chat.completion.chunk","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"reasoning_content":"think "},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_rc","object":"chat.completion.chunk","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"reasoning_content":"first"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_rc","object":"chat.completion.chunk","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":"stop"}]}`,
		"",
		`data: {"id":"chatcmpl_rc","object":"chat.completion.chunk","model":"deepseek-reasoner","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_reasoning_cache_stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	cache := &reasoningRecordingCache{}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
		cache:        cache,
	}

	result, err := svc.Forward(context.Background(), c, forceChatResponsesFallbackAccount(), body)
	require.NoError(t, err)
	require.NotNil(t, result)

	sets := cache.snapshotSets()
	require.Len(t, sets, 1, "应恰好缓存一个 reasoning item")
	for itemID, content := range sets {
		require.NotEmpty(t, itemID)
		require.Equal(t, "think first", content)
	}
}

// 请求侧：encrypted-only reasoning item（无明文 summary）经缓存回查补回
// reasoning_content；带明文 summary 的 item 顺手回写缓存（自愈）。
func TestForwardResponses_ChatFallbackRestoresReasoningFromCache(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{
		"model":"deepseek-reasoner",
		"stream":false,
		"input":[
			{"type":"reasoning","id":"item_plain","summary":[{"type":"summary_text","text":"plain thinking"}]},
			{"type":"function_call","call_id":"call_0","name":"get_value","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_0","output":"ok"},
			{"type":"reasoning","id":"item_enc1","summary":[],"encrypted_content":"opaque"},
			{"type":"function_call","call_id":"call_1","name":"get_value","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"ok"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"go on"}]}
		]
	}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid_reasoning_cache_restore"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"chatcmpl_restore","object":"chat.completion","model":"deepseek-reasoner","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		)),
	}}
	cache := &reasoningRecordingCache{
		getResp: map[string]string{"item_enc1": "cached thinking"},
	}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
		cache:        cache,
	}

	result, err := svc.Forward(context.Background(), c, forceChatResponsesFallbackAccount(), body)
	require.NoError(t, err)
	require.NotNil(t, result)

	// 明文 summary 的 assistant 工具调用消息：reasoning_content 来自 summary 本身。
	require.Equal(t, "plain thinking", gjson.GetBytes(upstream.lastBody, "messages.0.reasoning_content").String())
	require.Equal(t, "call_0", gjson.GetBytes(upstream.lastBody, "messages.0.tool_calls.0.id").String())
	require.Equal(t, "tool", gjson.GetBytes(upstream.lastBody, "messages.1.role").String())
	// encrypted-only 的 assistant 工具调用消息：reasoning_content 来自缓存回查。
	require.Equal(t, "cached thinking", gjson.GetBytes(upstream.lastBody, "messages.2.reasoning_content").String())
	require.Equal(t, "call_1", gjson.GetBytes(upstream.lastBody, "messages.2.tool_calls.0.id").String())
	require.Equal(t, "tool", gjson.GetBytes(upstream.lastBody, "messages.3.role").String())

	// 明文 summary 的 item 被回写进缓存（自愈）。
	require.Equal(t, "plain thinking", cache.snapshotSets()["item_plain"])
}

// ============================================================================
// n2s-W5：非流式→流式转换接线（方案 §D2 + §D4.3）
// ============================================================================

// ns2sResponsesEnabledConfig 返回开启非流式→流式转换的测试配置（保留宽松 URL 白名单；
// Disabled=false 零值即启用）。
func ns2sResponsesEnabledConfig() *config.Config {
	cfg := rawChatCompletionsTestConfig()
	cfg.Gateway.NonstreamToStreamDisabled = false
	return cfg
}

// ns2sResponsesEligibleAccount 构造资格账号：国产供应商 apikey + 第三方中转 base_url
// （非官方域），命中 shouldConvertNonstreamToStream。
func ns2sResponsesEligibleAccount() *Account {
	account := rawChatCompletionsTestAccount()
	account.Platform = PlatformDeepseek
	account.Credentials["base_url"] = "http://upstream.example/v1"
	return account
}

// ns2sResponsesStreamBody 构造一条完整可聚合的 CC SSE 流（含 finish_reason / usage / [DONE]）。
func ns2sResponsesStreamBody(flushDone bool) string {
	frames := []string{
		`data: {"id":"chatcmpl_ns2s","object":"chat.completion.chunk","created":1,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl_ns2s","object":"chat.completion.chunk","created":1,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl_ns2s","object":"chat.completion.chunk","created":1,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: {"id":"chatcmpl_ns2s","object":"chat.completion.chunk","created":1,"model":"deepseek-v4-flash","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
	}
	if flushDone {
		frames = append(frames, "data: [DONE]")
	}
	return strings.Join(frames, "\n") + "\n"
}

// Done when #1：资格账号 + stream:false → 上游收到 stream=true + include_usage；
// 客户端收到 200 Responses JSON（既有 CC→Responses 转换语义），usage 传递正确，
// ForwardResult.Stream=false。
func TestForwardResponsesViaRawChatCompletions_ConvertsEligibleNonstreamToStream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"deepseek-v4-flash","input":"hello","stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_ns2s_convert"}},
		Body:       io.NopCloser(strings.NewReader(ns2sResponsesStreamBody(true))),
	}}
	svc := &OpenAIGatewayService{cfg: ns2sResponsesEnabledConfig(), httpUpstream: upstream}

	result, err := svc.forwardResponsesViaRawChatCompletions(context.Background(), c, ns2sResponsesEligibleAccount(), body)
	require.NoError(t, err)
	require.NotNil(t, result)

	// 上游被强制流式 + include_usage。
	require.Equal(t, "http://upstream.example/v1/chat/completions", upstream.lastReq.URL.String())
	require.True(t, gjson.GetBytes(upstream.lastBody, "stream").Bool(), "资格账号非流式请求须以上游 stream=true 发出")
	require.True(t, gjson.GetBytes(upstream.lastBody, "stream_options.include_usage").Bool())

	// 客户端收到 200 Responses 契约 JSON。
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "response", gjson.Get(rec.Body.String(), "object").String())
	require.Equal(t, "ok", gjson.Get(rec.Body.String(), "output.0.content.0.text").String())
	require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
	require.NotContains(t, rec.Header().Get("Content-Type"), "text/event-stream")

	// usage 传递（计费口径）。
	require.Equal(t, 5, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)
	require.False(t, result.Stream)
}

// Done when #2（反向）：非资格账号 + stream:false → 既有缓冲分支（原生非流式转发），
// 上游 stream 标志保持 false。
func TestForwardResponsesViaRawChatCompletions_NonEligibleAccountKeepsBufferedPath(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.4","input":"hello","stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid_ns2s_ineligible"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"chatcmpl_plain","object":"chat.completion","model":"gpt-5.4","choices":[{"index":0,"message":{"role":"assistant","content":"native"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`,
		)),
	}}
	// 非资格账号：PlatformOpenAI（谓词不含），即便开关开启也保持既有缓冲分支。
	svc := &OpenAIGatewayService{cfg: ns2sResponsesEnabledConfig(), httpUpstream: upstream}

	result, err := svc.forwardResponsesViaRawChatCompletions(context.Background(), c, rawChatCompletionsTestAccount(), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, gjson.GetBytes(upstream.lastBody, "stream").Bool(), "非资格账号不得改写上游 stream 标志")
	require.False(t, gjson.GetBytes(upstream.lastBody, "stream_options.include_usage").Bool())
	require.Equal(t, "response", gjson.Get(rec.Body.String(), "object").String())
	require.Equal(t, "native", gjson.Get(rec.Body.String(), "output.0.content.0.text").String())
	require.False(t, result.Stream)
}

// Done when #2（反向）：clientStream=true → 既有流式分支行为不变（资格账号亦不聚合）。
func TestForwardResponsesViaRawChatCompletions_StreamingKeepsStreamBranch(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"deepseek-v4-flash","input":"hello","stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_ns2s_stream"}},
		Body:       io.NopCloser(strings.NewReader(ns2sResponsesStreamBody(true))),
	}}
	svc := &OpenAIGatewayService{cfg: ns2sResponsesEnabledConfig(), httpUpstream: upstream}

	result, err := svc.forwardResponsesViaRawChatCompletions(context.Background(), c, ns2sResponsesEligibleAccount(), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Stream, "clientStream=true 必须走既有流式分支")
	require.True(t, gjson.GetBytes(upstream.lastBody, "stream").Bool())
	require.Contains(t, rec.Body.String(), "event: response.output_text.delta")
	require.Contains(t, rec.Body.String(), "data: [DONE]")
}

// Done when #3：缺 [DONE] 哨兵 → writeOpenAIResponsesFallbackError 规范错误，零聚合内容。
func TestForwardResponsesViaRawChatCompletions_MissingDoneSentinelFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"deepseek-v4-flash","input":"hello","stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_ns2s_nodone"}},
		Body:       io.NopCloser(strings.NewReader(ns2sResponsesStreamBody(false))),
	}}
	svc := &OpenAIGatewayService{cfg: ns2sResponsesEnabledConfig(), httpUpstream: upstream}

	result, err := svc.forwardResponsesViaRawChatCompletions(context.Background(), c, ns2sResponsesEligibleAccount(), body)
	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Equal(t, "api_error", gjson.Get(rec.Body.String(), "error.type").String())
	require.NotContains(t, rec.Body.String(), "response.output_text", "失败关闭不得写出半个聚合响应")
}

// Done when #3：缺 usage 帧 → writeOpenAIResponsesFallbackError 规范错误、零计费。
func TestForwardResponsesViaRawChatCompletions_MissingUsageFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"deepseek-v4-flash","input":"hello","stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	streamBody := strings.Join([]string{
		`data: {"id":"chatcmpl_nousage","object":"chat.completion.chunk","created":1,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_ns2s_nousage"}},
		Body:       io.NopCloser(strings.NewReader(streamBody)),
	}}
	svc := &OpenAIGatewayService{cfg: ns2sResponsesEnabledConfig(), httpUpstream: upstream}

	result, err := svc.forwardResponsesViaRawChatCompletions(context.Background(), c, ns2sResponsesEligibleAccount(), body)
	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Equal(t, "api_error", gjson.Get(rec.Body.String(), "error.type").String())
	require.NotContains(t, rec.Body.String(), "output_text")
}

// Done when #3：CN 首包超时 → 既有 failover 分支（无新机制）。
func TestForwardResponsesViaRawChatCompletions_CNFirstByteTimeoutFailsOver(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"deepseek-v4-flash","input":"hello","stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_ns2s_cntimeout"}},
		Body:       &openAICompatBufferedReadErrorCloser{err: errOpenAICNFirstByteTimeout},
	}}
	svc := &OpenAIGatewayService{cfg: ns2sResponsesEnabledConfig(), httpUpstream: upstream}

	result, err := svc.forwardResponsesViaRawChatCompletions(context.Background(), c, ns2sResponsesEligibleAccount(), body)
	require.Error(t, err)
	require.Nil(t, result)
	var fo *UpstreamFailoverError
	require.True(t, errors.As(err, &fo), "CN 首包超时必须映射为既有 failover 错误")
	require.Equal(t, http.StatusGatewayTimeout, fo.StatusCode)
	require.Equal(t, NextAccountRetry, fo.NextAccountAction)
	require.Empty(t, rec.Body.String(), "failover 透传窗口内不得写客户端响应")
}

// Done when #4：资格命中 + stream:false → 转发上游的 context 中无心跳 owner
// （心跳 owner 仅 clientStream==true 时由 handler 安装；转换路径不引入 owner）。
func TestForwardResponsesViaRawChatCompletions_NoHeartbeatOwnerOnConvertedNonstream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"deepseek-v4-flash","input":"hello","stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_ns2s_hb"}},
		Body:       io.NopCloser(strings.NewReader(ns2sResponsesStreamBody(true))),
	}}
	svc := &OpenAIGatewayService{cfg: ns2sResponsesEnabledConfig(), httpUpstream: upstream}

	_, err := svc.forwardResponsesViaRawChatCompletions(context.Background(), c, ns2sResponsesEligibleAccount(), body)
	require.NoError(t, err)

	_, ok := UpstreamHeartbeatFromContext(upstream.lastReq.Context())
	require.False(t, ok, "转换请求（clientStream=false）上游 context 中不得有心跳 owner")
	_, ok = UpstreamHeartbeatFromContext(c.Request.Context())
	require.False(t, ok, "请求 context 不得因转换而被安装心跳 owner")
}
