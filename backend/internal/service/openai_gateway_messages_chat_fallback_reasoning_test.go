//go:build unit

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
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// F1 派发单：/v1/messages 路径 reasoning_content 缓存回注（DeepSeek thinking 400 根治）。
//
// 根因（P1 插桩实证）：客户端回放历史时工具轮不带 thinking 明文 → 网关出站
// reasoning_content 为空 → DeepSeek thinking mode 400。网关不得伪造明文，故与
// Responses 兄弟对齐：网关侧按 tool_call id 缓存 reasoning，下一轮回注真实明文；
// 回注不了就失败关闭。

// f1ToolTurnHistory 是 P1 捕获的生产同型历史：assistant 工具轮只带 tool_use，
// 无 thinking 明文（Claude Code 回放形态）。
const f1ToolTurnHistory = `{"model":"deepseek-chat","max_tokens":16,"stream":false,` +
	`"tools":[{"name":"get_weather","description":"d","input_schema":{"type":"object","properties":{}}}],` +
	`"messages":[` +
	`{"role":"user","content":"weather in sf?"},` +
	`{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"get_weather","input":{"city":"sf"}}]},` +
	`{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"sunny"}]}` +
	`]}`

// f1ReasoningService 构造带 reasoning 缓存、且强制缓冲非流式分支的 service
// （NonstreamToStreamDisabled=true，排除非流式→流式转换对断言的干扰）。
func f1ReasoningService(upstream *httpUpstreamRecorder, cache *reasoningRecordingCache) *OpenAIGatewayService {
	cfg := rawChatCompletionsTestConfig()
	cfg.Gateway = config.GatewayConfig{NonstreamToStreamDisabled: true}
	return &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, cache: cache}
}

func f1JSONUpstream() *httpUpstreamRecorder {
	return &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid_f1"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"chatcmpl_f1","object":"chat.completion","model":"deepseek-chat","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		)),
	}}
}

func f1NewContext(body string) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, rec
}

// Done when #1a + #2：客户端历史工具轮无 thinking + 缓存有该 tool_call id 的 reasoning
// → 最终发往 /v1/chat/completions 的 JSON 注入真实 reasoning_content。
func TestMessagesReasoningCache_HitInjectsRealReasoningIntoUpstreamJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cache := &reasoningRecordingCache{getResp: map[string]string{
		messagesToolCallReasoningKey("call_1"): "think hard",
	}}
	upstream := f1JSONUpstream()
	svc := f1ReasoningService(upstream, cache)

	c, rec := f1NewContext(f1ToolTurnHistory)
	result, err := svc.ForwardAsAnthropic(context.Background(), c, ns2sTestAccount(), []byte(f1ToolTurnHistory), "", "")
	require.NoError(t, err)
	require.NotNil(t, result)

	// 断言最终发往 /v1/chat/completions 的 JSON（gjson 精确取值）。
	require.Equal(t, "assistant", gjson.GetBytes(upstream.lastBody, "messages.1.role").String())
	require.Equal(t, "call_1", gjson.GetBytes(upstream.lastBody, "messages.1.tool_calls.0.id").String())
	require.Equal(t, "think hard", gjson.GetBytes(upstream.lastBody, "messages.1.reasoning_content").String(),
		"命中缓存必须注入缓存里的真实明文，而非空格/占位")

	// 同一份出站 JSON 的 json.Marshal 等价物（Done when #2 的字面量断言）。
	remarshaled, err := json.Marshal(json.RawMessage(upstream.lastBody))
	require.NoError(t, err)
	require.Contains(t, string(remarshaled), `"reasoning_content":"think hard"`)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "ok", gjson.Get(rec.Body.String(), "content.0.text").String())
}

// Done when #2（直接对注入后的 chatReq 做 json.Marshal 断言）：证明「最终发往
// /v1/chat/completions 的 JSON」的 reasoning_content 字段与值来自网关缓存。
func TestMessagesInjectCachedReasoning_MarshaledChatRequestCarriesReasoningContent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var anthropicReq apicompat.AnthropicRequest
	require.NoError(t, json.Unmarshal([]byte(f1ToolTurnHistory), &anthropicReq))
	chatReq, err := apicompat.AnthropicToChatCompletionsRequest(&anthropicReq)
	require.NoError(t, err)
	require.Len(t, chatReq.Messages, 3)
	require.Empty(t, chatReq.Messages[1].ReasoningContent, "注入前必须为空（客户端未回传 thinking，即 400 根因形态）")

	cache := &reasoningRecordingCache{getResp: map[string]string{
		messagesToolCallReasoningKey("call_1"): "think hard",
	}}
	svc := &OpenAIGatewayService{cache: cache}
	c, _ := f1NewContext(f1ToolTurnHistory)

	require.NoError(t, svc.injectCachedMessagesReasoning(context.Background(), c, chatReq, ns2sTestAccount(), "deepseek-chat"))

	out, err := json.Marshal(chatReq)
	require.NoError(t, err)
	require.Contains(t, string(out), `"reasoning_content":"think hard"`)
	require.Equal(t, "think hard", gjson.GetBytes(out, "messages.1.reasoning_content").String())
	require.Equal(t, "think hard", chatReq.Messages[1].ReasoningContent)
}

// Done when #1b：缓存全未命中 → 失败关闭：返回明确错误、客户端收到 Anthropic 规范
// 错误，**不发出上游请求**（不得伪造/空格/降级放行）。
func TestMessagesReasoningCache_MissFailsClosedWithoutUpstreamRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cache := &reasoningRecordingCache{getResp: map[string]string{}} // 全未命中
	upstream := f1JSONUpstream()
	svc := f1ReasoningService(upstream, cache)

	c, rec := f1NewContext(f1ToolTurnHistory)
	result, err := svc.ForwardAsAnthropic(context.Background(), c, ns2sTestAccount(), []byte(f1ToolTurnHistory), "", "")
	require.Error(t, err, "全未命中必须失败关闭")
	require.Nil(t, result)
	require.Contains(t, err.Error(), "missing reasoning_content")
	require.Contains(t, err.Error(), "call_1")

	require.Nil(t, upstream.lastReq, "失败关闭：不得发出上游请求")
	require.Empty(t, upstream.lastBody, "失败关闭：不得产生出站请求体")

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "error", gjson.Get(rec.Body.String(), "type").String())
	require.Equal(t, "invalid_request_error", gjson.Get(rec.Body.String(), "error.type").String())
	require.Contains(t, gjson.Get(rec.Body.String(), "error.message").String(), "reasoning_content")
}

// Done when #1c：门控外路径（非 deepseek 模型 + 非 deepseek 平台）→ 出站字节不变：
// 即便缓存里有该 tool_call id 的 reasoning，也不得注入。
func TestMessagesReasoningCache_NonGatedPathLeavesUpstreamBodyUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cache := &reasoningRecordingCache{getResp: map[string]string{
		messagesToolCallReasoningKey("call_1"): "think hard",
	}}
	upstream := f1JSONUpstream()
	svc := f1ReasoningService(upstream, cache)

	// 非 deepseek：model=gpt-5.4 + 账号 platform=openai。
	body := strings.Replace(f1ToolTurnHistory, `"model":"deepseek-chat"`, `"model":"gpt-5.4"`, 1)
	c, rec := f1NewContext(body)
	result, err := svc.ForwardAsAnthropic(context.Background(), c, forceChatMessagesFallbackAccount(), []byte(body), "", "")
	require.NoError(t, err)
	require.NotNil(t, result)

	require.False(t, gjson.GetBytes(upstream.lastBody, "messages.1.reasoning_content").Exists(),
		"门控外路径出站字节零变化：不得注入 reasoning_content")
	require.NotContains(t, string(upstream.lastBody), "reasoning_content")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "ok", gjson.Get(rec.Body.String(), "content.0.text").String())
}

// 门控判定本体：模型名（去 opencode 前缀后）以 deepseek 开头，或 platform=deepseek。
func TestMessagesDeepSeekThinkingGate(t *testing.T) {
	deepseekAcc := &Account{Platform: PlatformDeepseek}
	openaiAcc := &Account{Platform: PlatformOpenAI}

	require.True(t, messagesDeepSeekThinkingGate(deepseekAcc, "gpt-5.4"), "platform=deepseek 命中")
	require.True(t, messagesDeepSeekThinkingGate(openaiAcc, "deepseek-chat"), "模型名 deepseek 前缀命中")
	require.True(t, messagesDeepSeekThinkingGate(openaiAcc, "deepseek-reasoner"), "模型名 deepseek 前缀命中")
	require.True(t, messagesDeepSeekThinkingGate(openaiAcc, "opencode/deepseek-v4-pro"), "去 opencode 前缀后命中")
	require.False(t, messagesDeepSeekThinkingGate(openaiAcc, "gpt-5.4"), "非 deepseek 不命中")
	require.False(t, messagesDeepSeekThinkingGate(openaiAcc, "opencode/gpt-5.4"), "非 deepseek 不命中")
	require.False(t, messagesDeepSeekThinkingGate(nil, ""), "空模型名不命中")
}

// 写侧（流式）：上游流出的 reasoning_content + tool_call 必须按 tool_call id
// 写入既有 GatewayCache，键带 msg_tool: 命名空间前缀。
func TestMessagesReasoningCache_StreamingWriteCachesReasoningByToolCallID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"deepseek-chat","max_tokens":16,"messages":[{"role":"user","content":"weather in sf?"}],"stream":true}`)
	c, rec := f1NewContext(string(body))

	upstreamBody := strings.Join([]string{
		`data: {"id":"chatcmpl_f1s","object":"chat.completion.chunk","model":"deepseek-chat","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_f1s","object":"chat.completion.chunk","model":"deepseek-chat","choices":[{"index":0,"delta":{"reasoning_content":"think "},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_f1s","object":"chat.completion.chunk","model":"deepseek-chat","choices":[{"index":0,"delta":{"reasoning_content":"first"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_f1s","object":"chat.completion.chunk","model":"deepseek-chat","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"sf\"}"}}]},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_f1s","object":"chat.completion.chunk","model":"deepseek-chat","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		"",
		`data: {"choices":[],"usage":{"prompt_tokens":6,"completion_tokens":5,"total_tokens":11}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}

	cache := &reasoningRecordingCache{}
	svc := f1ReasoningService(upstream, cache)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, ns2sTestAccount(), body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Contains(t, rec.Body.String(), `"type":"tool_use"`)

	sets := cache.snapshotSets()
	require.Equal(t, "think first", sets[messagesToolCallReasoningKey("call_1")],
		"流式 reasoning 必须按 tool_call id 写入缓存（键带 msg_tool: 前缀）")
}

// 写侧（非流式）：完整 chat.completion 响应里的 reasoning_content 同样按 tool_call id
// 缓存；多工具轮每个 tool_call id 写同一条 reasoning。
func TestMessagesReasoningCache_NonStreamingWriteCachesReasoningForEveryToolCallID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"deepseek-chat","max_tokens":16,"messages":[{"role":"user","content":"weather in sf?"}],"stream":false}`)
	c, _ := f1NewContext(string(body))

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"chatcmpl_f1n","object":"chat.completion","model":"deepseek-chat","choices":[{"index":0,"message":{"role":"assistant","reasoning_content":"think once","tool_calls":[{"id":"call_1","type":"function","function":{"name":"a","arguments":"{}"}},{"id":"call_2","type":"function","function":{"name":"b","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		)),
	}}

	cache := &reasoningRecordingCache{}
	svc := f1ReasoningService(upstream, cache)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, ns2sTestAccount(), body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)

	sets := cache.snapshotSets()
	require.Equal(t, "think once", sets[messagesToolCallReasoningKey("call_1")])
	require.Equal(t, "think once", sets[messagesToolCallReasoningKey("call_2")], "多工具轮每个 tool_call id 写同一条 reasoning")
}
