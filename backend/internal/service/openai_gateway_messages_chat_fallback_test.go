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
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func forceChatMessagesFallbackAccount() *Account {
	account := rawChatCompletionsTestAccount()
	account.Extra = map[string]any{
		openai_compat.ExtraKeyResponsesMode: string(openai_compat.ResponsesSupportModeForceChatCompletions),
	}
	return account
}

// errTailReader yields the given data, then returns err instead of io.EOF,
// simulating an upstream connection that breaks mid-stream.
type errTailReader struct {
	data []byte
	off  int
	err  error
}

func (r *errTailReader) Read(p []byte) (int, error) {
	if r.off < len(r.data) {
		n := copy(p, r.data[r.off:])
		r.off += n
		return n, nil
	}
	return 0, r.err
}

func (r *errTailReader) Close() error { return nil }

func TestForwardAsAnthropic_ForceChatCompletionsPreservesFinalModelReasoningEffort(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name       string
		model      string
		mapped     string
		effortJSON string
		wantEffort string
		maxPolicy  string
	}{
		{
			name:       "policy caps converted effort",
			model:      "gpt-5.6-luna",
			mapped:     "gpt-5.6-luna",
			effortJSON: `,"output_config":{"effort":"max"}`,
			wantEffort: "medium",
			maxPolicy:  "medium",
		},
		{
			name:       "GPT56 max",
			model:      "luna",
			mapped:     "gpt-5.6-luna",
			effortJSON: `,"output_config":{"effort":"max"}`,
			wantEffort: "max",
		},
		{
			name:       "old model max",
			model:      "gpt-5.5",
			mapped:     "gpt-5.5",
			effortJSON: `,"output_config":{"effort":"max"}`,
			wantEffort: "xhigh",
		},
		{
			name:       "high remains high",
			model:      "gpt-5.6-luna",
			mapped:     "gpt-5.6-luna",
			effortJSON: `,"output_config":{"effort":"high"}`,
			wantEffort: "high",
		},
		{
			name:       "omitted defaults medium",
			model:      "gpt-5.6-luna",
			mapped:     "gpt-5.6-luna",
			wantEffort: "medium",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			body := `{"model":"` + tt.model + `","max_tokens":16,"messages":[{"role":"user","content":"hello"}]` + tt.effortJSON + `,"stream":false}`
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(body)))
			c.Request.Header.Set("Content-Type", "application/json")

			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"id":"chatcmpl_effort","object":"chat.completion","model":"` + tt.mapped + `","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
				)),
			}}
			account := forceChatMessagesFallbackAccount()
			account.Credentials["model_mapping"] = map[string]any{tt.model: tt.mapped}

			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			ctx := context.Background()
			if tt.maxPolicy != "" {
				ctx = WithOpenAIReasoningEffortPolicy(ctx, tt.maxPolicy, nil, "")
			}
			result, err := svc.ForwardAsAnthropic(ctx, c, account, []byte(body), "", "")
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, tt.mapped, gjson.GetBytes(upstream.lastBody, "model").String())
			require.Equal(t, tt.wantEffort, gjson.GetBytes(upstream.lastBody, "reasoning_effort").String())
			require.NotNil(t, result.ReasoningEffort)
			require.Equal(t, tt.wantEffort, *result.ReasoningEffort)
		})
	}
}

func TestForwardAsAnthropic_ForceChatCompletionsNonStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.4","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid_msg_chat_json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"chatcmpl_json","object":"chat.completion","model":"gpt-5.4","service_tier":"priority","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5,"prompt_tokens_details":{"cached_tokens":1}}}`,
		)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, forceChatMessagesFallbackAccount(), body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "http://upstream.example/v1/chat/completions", upstream.lastReq.URL.String())
	require.Equal(t, "hello", gjson.GetBytes(upstream.lastBody, "messages.0.content").String())
	require.False(t, gjson.GetBytes(upstream.lastBody, "input").Exists())
	require.True(t, gjson.GetBytes(upstream.lastBody, "stream_options").Exists() == false)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "assistant", gjson.Get(rec.Body.String(), "role").String())
	require.Equal(t, "ok", gjson.Get(rec.Body.String(), "content.0.text").String())
	require.Equal(t, 3, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)
	require.Equal(t, 1, result.Usage.CacheReadInputTokens)
	require.Nil(t, result.ServiceTier)
	require.Equal(t, "priority", result.UpstreamResponseServiceTier)
	require.False(t, result.Stream)
}

// Covers the fully-new streaming composition: text block is still open when
// [DONE] arrives, so finalization must close it (content_block_stop) before
// message_delta / message_stop.
func TestForwardAsAnthropic_ForceChatCompletionsStreamingClosesOpenBlockOnDone(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.4","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"id":"chatcmpl_s","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_s","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"content":"he"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_s","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"content":"llo"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_s","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"",
		`data: {"id":"chatcmpl_s","object":"chat.completion.chunk","model":"gpt-5.4","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_msg_chat_stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, forceChatMessagesFallbackAccount(), body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, gjson.GetBytes(upstream.lastBody, "stream_options.include_usage").Bool())

	out := rec.Body.String()
	require.Contains(t, out, "event: message_start")
	require.Contains(t, out, `"text":"he"`)
	require.Contains(t, out, `"text":"llo"`)
	require.Contains(t, out, "event: content_block_stop")
	require.Contains(t, out, `"stop_reason":"end_turn"`)
	require.Contains(t, out, "event: message_stop")

	blockStop := strings.Index(out, "event: content_block_stop")
	msgDelta := strings.Index(out, `"stop_reason":"end_turn"`)
	msgStop := strings.Index(out, "event: message_stop")
	require.Greater(t, msgDelta, blockStop, "content_block_stop must precede message_delta")
	require.Greater(t, msgStop, msgDelta, "message_delta must precede message_stop")

	require.Equal(t, 4, result.Usage.InputTokens)
	require.Equal(t, 3, result.Usage.OutputTokens)
	require.True(t, result.Stream)
	require.NotNil(t, result.FirstTokenMs)
}

// Covers multi-chunk tool_call fragments aggregated by index and finalized as
// an Anthropic tool_use block with stop_reason=tool_use.
func TestForwardAsAnthropic_ForceChatCompletionsStreamingToolCallAggregation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.4","max_tokens":32,"messages":[{"role":"user","content":"weather in sf?"}],"stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"id":"chatcmpl_t","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_t","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_t","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"sf\"}"}}]},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_t","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		"",
		`data: {"id":"chatcmpl_t","object":"chat.completion.chunk","model":"gpt-5.4","choices":[],"usage":{"prompt_tokens":6,"completion_tokens":5,"total_tokens":11}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_msg_chat_tool"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, forceChatMessagesFallbackAccount(), body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)

	out := rec.Body.String()
	require.Contains(t, out, `"type":"tool_use"`)
	require.Contains(t, out, `"name":"get_weather"`)
	require.Contains(t, out, `"input_json_delta"`)
	require.Contains(t, out, `"stop_reason":"tool_use"`)
	require.Contains(t, out, "event: message_stop")
	require.Equal(t, 6, result.Usage.InputTokens)
	require.Equal(t, 5, result.Usage.OutputTokens)
}

// finish_reason=length must survive the double conversion (CC → Responses →
// Anthropic) as stop_reason=max_tokens.
func TestForwardAsAnthropic_ForceChatCompletionsStreamingLengthMapsToMaxTokens(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.4","max_tokens":8,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"id":"chatcmpl_l","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"role":"assistant","content":"truncat"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_l","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":4,"completion_tokens":8,"total_tokens":12}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_msg_chat_len"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, forceChatMessagesFallbackAccount(), body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)

	out := rec.Body.String()
	require.Contains(t, out, `"stop_reason":"max_tokens"`)
	require.Contains(t, out, "event: message_stop")
}

// An upstream that ends immediately with [DONE] must still produce a fully
// framed (message_start → message_delta → message_stop) Anthropic stream.
func TestForwardAsAnthropic_ForceChatCompletionsEmptyStreamStillFramesMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.4","max_tokens":8,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_msg_chat_empty"}},
		Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, forceChatMessagesFallbackAccount(), body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)

	out := rec.Body.String()
	require.Contains(t, out, "event: message_start")
	require.Contains(t, out, "event: message_delta")
	require.Contains(t, out, "event: message_stop")
}

// Non-failover 4xx responses must go through the shared compat error handler:
// status-specific Anthropic error type, upstream message preserved, and ops
// upstream-error events recorded (previously this branch bypassed all three).
func TestForwardAsAnthropic_ForceChatCompletionsNonFailover400UsesSharedErrorHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.4","max_tokens":8,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid_msg_chat_400"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"invalid roles","type":"invalid_request_error"}}`)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, forceChatMessagesFallbackAccount(), body, "", "")
	require.Error(t, err)
	require.Nil(t, result)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "error", gjson.Get(rec.Body.String(), "type").String())
	require.Equal(t, "invalid_request_error", gjson.Get(rec.Body.String(), "error.type").String())
	require.Equal(t, "invalid roles", gjson.Get(rec.Body.String(), "error.message").String())

	statusVal, ok := c.Get(OpsUpstreamStatusCodeKey)
	require.True(t, ok, "shared handler must record the upstream status for ops")
	require.Equal(t, http.StatusBadRequest, statusVal)

	eventsVal, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok, "shared handler must append an ops upstream error event")
	events, castOK := eventsVal.([]*OpsUpstreamErrorEvent)
	require.True(t, castOK)
	require.Len(t, events, 1)
	require.Equal(t, http.StatusBadRequest, events[0].UpstreamStatusCode)
	require.Equal(t, "http_error", events[0].Kind)
	require.Equal(t, "invalid roles", events[0].Message)
}

// A broken upstream read mid-stream must surface an error and must NOT emit a
// synthetic message_stop that would disguise the truncation as a completion.
func TestForwardAsAnthropic_ForceChatCompletionsStreamReadErrorSkipsFinalize(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.4","max_tokens":8,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	partial := strings.Join([]string{
		`data: {"id":"chatcmpl_e","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_e","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"content":"he"},"finish_reason":null}]}`,
		"",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_msg_chat_err"}},
		Body:       &errTailReader{data: []byte(partial), err: errors.New("simulated upstream read failure")},
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, forceChatMessagesFallbackAccount(), body, "", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "stream usage incomplete")
	require.NotNil(t, result)
	require.True(t, result.Stream)

	out := rec.Body.String()
	require.Contains(t, out, `"text":"he"`, "delta emitted before the failure must reach the client")
	require.NotContains(t, out, "event: message_stop", "no synthetic completion after a broken read")
}

// Gate regression: an API-key account whose upstream is confirmed to support
// the Responses API must keep using /v1/responses, never the CC fallback.
func TestForwardAsAnthropic_ResponsesSupportedAccountStillUsesResponsesEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"output_config":{"effort":"high"},"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("User-Agent", "third-party-client/1.0.0")
	c.Request.Header.Set("originator", "opencode")

	upstreamBody := strings.Join([]string{
		`data: {"type":"response.completed","response":{"id":"resp_native","object":"response","model":"gpt-5.4","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_msg_native"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
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

	ctx := WithOpenAIReasoningEffortPolicy(context.Background(), "medium", nil, "")
	result, err := svc.ForwardAsAnthropic(ctx, c, account, body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, strings.HasSuffix(upstream.lastReq.URL.Path, "/responses"),
		"responses-capable account must stay on /v1/responses, got %s", upstream.lastReq.URL.String())
	require.True(t, gjson.GetBytes(upstream.lastBody, "input").Exists())
	require.Equal(t, "medium", gjson.GetBytes(upstream.lastBody, "reasoning.effort").String())
	require.NotNil(t, result.ReasoningEffort)
	require.Equal(t, "medium", *result.ReasoningEffort)
	require.False(t, gjson.GetBytes(upstream.lastBody, "messages").Exists())
	require.Equal(t, "third-party-client/1.0.0", upstream.lastReq.Header.Get("User-Agent"))
	require.Equal(t, "opencode", upstream.lastReq.Header.Get("originator"))
	require.Empty(t, upstream.lastReq.Header.Get("version"))
	require.Empty(t, upstream.lastReq.Header.Get("OpenAI-Beta"))
	require.Equal(t, "ok", gjson.Get(rec.Body.String(), "content.0.text").String())
}

// ---- W3 派发单：非流式→流式转换（方案 D2+D4.1）messages 入口接线测试 ----

// ns2sTestAccount 构造一个命中转换资格谓词的账号：deepseek apikey + 第三方
// 中转 base_url（非官方直连）+ ForceChatCompletions 走 raw CC fallback。
func ns2sTestAccount() *Account {
	acc := forceChatMessagesFallbackAccount()
	acc.Platform = PlatformDeepseek
	acc.Credentials = map[string]any{
		"api_key":  "sk-test",
		"base_url": "https://tokenharbor.ai/v1",
	}
	return acc
}

// ns2sEnabledService 构造开启转换的 service（默认键 false；零值即启用，注释零值安全）。
func ns2sEnabledService() *OpenAIGatewayService {
	cfg := rawChatCompletionsTestConfig()
	cfg.Gateway = config.GatewayConfig{NonstreamToStreamDisabled: false}
	return &OpenAIGatewayService{cfg: cfg}
}

// Done when #1：资格账号 + stream:false 客户端请求 → 上游收到 stream=true +
// include_usage；客户端 200 anthropic JSON（内容=聚合后），ForwardResult.Stream=false，
// usage 入 OpenAIForwardResult。
func TestForwardAnthropicViaRawChatCompletions_NonstreamConvertedToStream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"deepseek-chat","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"id":"chatcmpl_ns","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-chat","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_ns","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-chat","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"",
		`data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_ns2s_conv"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}

	svc := ns2sEnabledService()
	svc.httpUpstream = upstream

	result, err := svc.ForwardAsAnthropic(context.Background(), c, ns2sTestAccount(), body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)

	// 上游请求：stream=true + include_usage。
	require.True(t, gjson.GetBytes(upstream.lastBody, "stream").Bool(), "资格账号非流式客户端请求必须向上游发 stream=true")
	require.True(t, gjson.GetBytes(upstream.lastBody, "stream_options.include_usage").Bool(), "必须携带 include_usage")
	require.Equal(t, "text/event-stream", upstream.lastReq.Header.Get("Accept"), "强制流式后 Accept 须切 SSE")

	// 客户端：200 anthropic JSON（非流式），Content-Type=application/json。
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	out := rec.Body.String()
	require.Equal(t, "assistant", gjson.Get(out, "role").String())
	require.Equal(t, "ok", gjson.Get(out, "content.0.text").String(), "客户端内容=聚合后的完整内容，非 SSE 帧")
	require.Equal(t, "end_turn", gjson.Get(out, "stop_reason").String())

	// ForwardResult：Stream=false，usage 入账（计费口径）。
	require.False(t, result.Stream, "非流式客户端 ForwardResult.Stream 必须为 false")
	require.Equal(t, 3, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)
}

// Done when #2a：非资格账号（openai apikey / grok apikey / CN 官方直连 apikey）+
// stream:false → 走既有缓冲分支（上游请求体 stream 缺省/false，无 include_usage，
// 客户端 200 JSON 不变）。
func TestForwardAnthropicViaRawChatCompletions_NonEligibleStillBuffered(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamJSON := `{"id":"chatcmpl_buf","object":"chat.completion","model":"gpt-5.4","choices":[{"index":0,"message":{"role":"assistant","content":"buffered"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`

	cases := []struct {
		name    string
		account *Account
	}{
		{"openai apikey", func() *Account {
			acc := rawChatCompletionsTestAccount()
			acc.Extra = map[string]any{
				openai_compat.ExtraKeyResponsesMode: string(openai_compat.ResponsesSupportModeForceChatCompletions),
			}
			return acc
		}()},
		{"grok 直连", func() *Account {
			// 与既有 grok raw CC 测试同型：openai 平台 apikey + Extra 探针不支持
			// Responses → 走 raw CC fallback；谓词层面 grok 官方直连即排除（不转换）。
			acc := rawChatCompletionsTestAccount()
			acc.Name = "openai-compatible-grok"
			acc.Extra = map[string]any{openai_compat.ExtraKeyResponsesSupported: false}
			return acc
		}()},
		// 生产实证 #58/#132 同形态：zhipu apikey + 官方域（open.bigmodel.cn）→ 官方直连排除。
		{"zhipu官方直连", func() *Account {
			acc := forceChatMessagesFallbackAccount()
			acc.Platform = PlatformZhipu
			acc.Credentials = map[string]any{
				"api_key":  "sk-zhipu",
				"base_url": DefaultZhipuCodingBaseURL,
			}
			return acc
		}()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")

			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid_ns2s_non"}},
				Body:       io.NopCloser(strings.NewReader(upstreamJSON)),
			}}
			svc := ns2sEnabledService()
			svc.httpUpstream = upstream

			result, err := svc.ForwardAsAnthropic(context.Background(), c, tc.account, body, "", "")
			require.NoError(t, err)
			require.NotNil(t, result)

			// 既有缓冲分支：上游请求体 stream 缺省/false，无 include_usage。
			require.False(t, gjson.GetBytes(upstream.lastBody, "stream").Bool(), "非资格账号不得向上游发 stream=true")
			require.False(t, gjson.GetBytes(upstream.lastBody, "stream_options").Exists(), "非资格账号不得携带 include_usage")
			require.Equal(t, "application/json", upstream.lastReq.Header.Get("Accept"), "非资格账号保持原生非流式 Accept")

			require.Equal(t, http.StatusOK, rec.Code)
			require.Equal(t, "buffered", gjson.Get(rec.Body.String(), "content.0.text").String())
			require.False(t, result.Stream)
		})
	}
}

// Done when #2a 补充：不进入本 forwarder 的形态（web 接入 / openai OAuth）在
// 资格谓词层面即被排除（W1 谓词矩阵已钉，此处再钉入口不致误转换）。
func TestForwardAnthropicViaRawChatCompletions_WebAndOAuthEligibility(t *testing.T) {
	svc := ns2sEnabledService()

	// web 接入账号：谓词显式排除（IsWebAccessMode 独立判定源）。
	webAcc := forceChatMessagesFallbackAccount()
	webAcc.Credentials = map[string]any{
		"api_key":     "sk-test",
		"base_url":    "https://tokenharbor.ai/v1",
		"access_mode": AccountAccessModeWeb,
	}
	require.False(t, svc.shouldConvertNonstreamToStream(webAcc), "web 接入账号不得转换")

	// openai OAuth：谓词排除（Type 非 APIKey）。
	oauthAcc := rawChatCompletionsTestAccount()
	oauthAcc.Type = AccountTypeOAuth
	require.False(t, svc.shouldConvertNonstreamToStream(oauthAcc), "openai OAuth 不得转换")
}

// Done when #2b：clientStream=true → 既有流式分支行为不变（上游 stream=true +
// include_usage，客户端 SSE，ForwardResult.Stream=true）。
func TestForwardAnthropicViaRawChatCompletions_ClientStreamUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"deepseek-chat","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"id":"chatcmpl_s2","object":"chat.completion.chunk","model":"deepseek-chat","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_s2","object":"chat.completion.chunk","model":"deepseek-chat","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"",
		`data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_ns2s_stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	svc := ns2sEnabledService()
	svc.httpUpstream = upstream

	result, err := svc.ForwardAsAnthropic(context.Background(), c, ns2sTestAccount(), body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)

	require.True(t, gjson.GetBytes(upstream.lastBody, "stream").Bool())
	require.True(t, gjson.GetBytes(upstream.lastBody, "stream_options.include_usage").Bool())

	out := rec.Body.String()
	require.Contains(t, out, "event: message_start")
	require.Contains(t, out, `"text":"hi"`)
	require.Contains(t, out, "event: message_stop")
	require.True(t, result.Stream, "clientStream=true 时 ForwardResult.Stream 必须为 true（既有流式分支）")
	require.Equal(t, 3, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)
}

// Done when #3a：桥失败（缺 [DONE] / 缺 usage / 零 choice / 畸形帧）→ 客户端收到
// writeAnthropicError 规范错误（type=error + error.type=api_error + 502），零聚合内容写出。
func TestForwardAnthropicViaRawChatCompletions_ConvertedBridgeErrors(t *testing.T) {
	cases := []struct {
		name     string
		upstream string
		wantMsg  string
	}{
		{
			name: "missing DONE sentinel",
			upstream: strings.Join([]string{
				`data: {"id":"c1","object":"chat.completion.chunk","model":"deepseek-chat","choices":[{"index":0,"delta":{"role":"assistant","content":"partial"},"finish_reason":"stop"}]}`,
				"",
				`data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
				"",
			}, "\n"),
			wantMsg: "[DONE]",
		},
		{
			name: "missing usage frame",
			upstream: strings.Join([]string{
				`data: {"id":"c1","object":"chat.completion.chunk","model":"deepseek-chat","choices":[{"index":0,"delta":{"role":"assistant","content":"partial"},"finish_reason":"stop"}]}`,
				"",
				"data: [DONE]",
				"",
			}, "\n"),
			wantMsg: "usage",
		},
		{
			name: "zero choice chunks",
			upstream: strings.Join([]string{
				`data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
				"",
				"data: [DONE]",
				"",
			}, "\n"),
			wantMsg: "without response",
		},
		{
			name: "malformed frame in sequence",
			upstream: strings.Join([]string{
				`data: {"id":"c1","object":"chat.completion.chunk","model":"deepseek-chat","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`,
				"",
				"data: not-valid-json",
				"",
				`data: {"choices":[{"index":0,"delta":{"content":"more"},"finish_reason":"stop"}]}`,
				"",
				`data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
				"",
				"data: [DONE]",
				"",
			}, "\n"),
			wantMsg: "malformed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			body := []byte(`{"model":"deepseek-chat","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")

			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(tc.upstream)),
			}}
			svc := ns2sEnabledService()
			svc.httpUpstream = upstream

			result, err := svc.ForwardAsAnthropic(context.Background(), c, ns2sTestAccount(), body, "", "")
			require.Error(t, err, "桥失败必须显式上抛错误")
			require.Nil(t, result)

			out := rec.Body.String()
			require.Equal(t, http.StatusBadGateway, rec.Code, "规范错误状态码")
			require.Equal(t, "error", gjson.Get(out, "type").String(), "writeAnthropicError 信封")
			require.Equal(t, "api_error", gjson.Get(out, "error.type").String())
			require.Contains(t, gjson.Get(out, "error.message").String(), tc.wantMsg)
			require.NotContains(t, out, "partial", "失败关闭：零聚合内容写出")
			require.NotContains(t, out, "more", "失败关闭：零聚合内容写出")
			require.False(t, strings.Contains(out, "text/event-stream"), "错误响应不得带 SSE 头")
		})
	}
}

// Done when #3b：CN 首包超时形态（body 读被 watchdog 转译为 errOpenAICNFirstByteTimeout）
// → 走既有 failover 分支（UpstreamFailoverError 504 + NextAccountRetry），
// 不回写任何客户端字节。
func TestForwardAnthropicViaRawChatCompletions_ConvertedCNFirstByteTimeoutFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"deepseek-chat","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: &openAICNFirstByteTimeoutBody{
			ReadCloser: io.NopCloser(strings.NewReader("")),
			w:          cnTimedOutWatchdogForTest(),
		},
	}}
	svc := ns2sEnabledService()
	svc.httpUpstream = upstream

	result, err := svc.ForwardAsAnthropic(context.Background(), c, ns2sTestAccount(), body, "", "")
	require.Error(t, err)
	require.Nil(t, result)

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr, "CN 首包超时必须走既有透明 failover 分支")
	require.Equal(t, http.StatusGatewayTimeout, failoverErr.StatusCode)
	require.Equal(t, NextAccountRetry, failoverErr.NextAccountAction)
	require.False(t, c.Writer.Written(), "failover 前不得下发任何字节给客户端")
}

// cnTimedOutWatchdogForTest 构造一个已裁决为内部超时的 watchdog（decided=timedOut），
// 使 openAICNFirstByteTimeoutBody.Read 在读到零字节+错误时把读中断转译为
// errOpenAICNFirstByteTimeout（与真实 60s 首包超时形态同型）。
func cnTimedOutWatchdogForTest() *openAICNFirstByteWatchdog {
	w := &openAICNFirstByteWatchdog{done: make(chan struct{})}
	w.decided.Store(watchdogStateTimedOut)
	return w
}

// Done when #4（§D5 反向测试）：资格命中 + stream:false 请求 → context 中无心跳
// owner（心跳帧物理上无法进入非流式响应）。reqStream 口径 = 客户端 stream 标志，
// 转换发生在 service 层、晚于 handler 安装点，故不改变 owner 安装判定。
func TestForwardAnthropicViaRawChatCompletions_ConvertedNonstreamHasNoHeartbeatOwner(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"deepseek-chat","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	// 前置：模拟 handler 安装点对客户端 stream 标志的快照（reqStream=false）。
	snapshot := NewRequestBudgetSnapshot(90, 15, time.Now(), false)
	ctx := WithRequestBudgetSnapshot(context.Background(), snapshot)
	_, hasHB := UpstreamHeartbeatFromContext(ctx)
	require.False(t, hasHB, "reqStream=false 时不得安装心跳 owner（安装点零改动）")

	// 在该无 owner 的 context 上执行转换路径。
	upstreamBody := strings.Join([]string{
		`data: {"id":"chatcmpl_hb","object":"chat.completion.chunk","model":"deepseek-chat","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"",
		`data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	svc := ns2sEnabledService()
	svc.httpUpstream = upstream
	c.Request = c.Request.WithContext(ctx)

	result, err := svc.ForwardAsAnthropic(ctx, c, ns2sTestAccount(), body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)

	// 转换请求在无心跳 owner 的 context 上完成，响应为纯 JSON、无 SSE 心跳帧。
	_, stillNoHB := UpstreamHeartbeatFromContext(ctx)
	require.False(t, stillNoHB, "转换不得凭空安装心跳 owner")
	require.NotContains(t, rec.Body.String(), "keep-alive", "心跳帧不得进入非流式响应")
	require.False(t, result.Stream)
	// W10 P2 回修：对齐 responses 用例最强形态（openai_gateway_responses_chat_fallback_test.go:684），
	// 追加上游真实请求 ctx 的心跳断言——证明转换路径未在真实传播链上安装 owner。
	_, upstreamCtxHB := UpstreamHeartbeatFromContext(upstream.lastReq.Context())
	require.False(t, upstreamCtxHB, "转换路径不得在真实上游请求 ctx 上安装心跳 owner")
}
