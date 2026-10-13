//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// F2 派发单：/v1/messages 路径 reasoning_content 缓存回注。
//
// 背景：客户端回放历史时工具轮不带 thinking 明文 → 网关出站 reasoning_content 为空
// → DeepSeek thinking mode 400。网关不得伪造明文，故按 tool_call id 缓存网关自己
// 服务过的轮次的 reasoning，下一轮回注真实明文。
//
// F1 在缓存未命中时失败关闭（400），而缓存只能覆盖网关自身服务过的轮次，客户端重放
// 的历史轮次注定回注不到 ⇒ 长会话 100% 死锁（生产已回滚）。F2 改为失败开放：未命中
// 原样放行，同时补齐 B4 写侧的 4 个漏写洞。

// f2ToolTurnHistory 是生产同型历史：assistant 工具轮只带 tool_use，无 thinking
// 明文（Claude Code 回放形态）。
const f2ToolTurnHistory = `{"model":"deepseek-chat","max_tokens":16,"stream":false,` +
	`"tools":[{"name":"get_weather","description":"d","input_schema":{"type":"object","properties":{}}}],` +
	`"messages":[` +
	`{"role":"user","content":"weather in sf?"},` +
	`{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"get_weather","input":{"city":"sf"}}]},` +
	`{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"sunny"}]}` +
	`]}`

// f2ReasoningService 构造带 reasoning 缓存、且强制缓冲非流式分支的 service
// （NonstreamToStreamDisabled=true，排除非流式→流式转换对断言的干扰）。
func f2ReasoningService(upstream *httpUpstreamRecorder, cache *reasoningRecordingCache) *OpenAIGatewayService {
	cfg := rawChatCompletionsTestConfig()
	cfg.Gateway = config.GatewayConfig{NonstreamToStreamDisabled: true}
	return &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, cache: cache}
}

// f2ConvertingService 与 f2ReasoningService 相对：开启非流式→流式转换，用于覆盖
// collectCCStreamAsResponse（converted 分支）的写侧。
func f2ConvertingService(upstream *httpUpstreamRecorder, cache *reasoningRecordingCache) *OpenAIGatewayService {
	cfg := rawChatCompletionsTestConfig()
	cfg.Gateway = config.GatewayConfig{NonstreamToStreamDisabled: false}
	return &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, cache: cache}
}

// f2NoCacheService 构造不带 GatewayCache 的 service（s.cache == nil），用于验证
// 缓存不可用时读侧同样失败开放。注意：不能传 (*reasoningRecordingCache)(nil)，
// 那会得到一个非 nil 接口值。
func f2NoCacheService(upstream *httpUpstreamRecorder) *OpenAIGatewayService {
	cfg := rawChatCompletionsTestConfig()
	cfg.Gateway = config.GatewayConfig{NonstreamToStreamDisabled: true}
	return &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
}

func f2JSONUpstream() *httpUpstreamRecorder {
	return &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid_f2"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"chatcmpl_f2","object":"chat.completion","model":"deepseek-chat","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		)),
	}}
}

func f2NewContext(body string) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, rec
}

// ---- 读侧 ----

// 命中缓存 → 最终发往 /v1/chat/completions 的 JSON 注入缓存里的真实明文
// （不得为空/占位）。
func TestMessagesReasoningCache_HitInjectsRealReasoningIntoUpstreamJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cache := &reasoningRecordingCache{getResp: map[string]string{
		messagesToolCallReasoningKey("call_1"): "think hard",
	}}
	upstream := f2JSONUpstream()
	svc := f2ReasoningService(upstream, cache)

	c, rec := f2NewContext(f2ToolTurnHistory)
	result, err := svc.ForwardAsAnthropic(context.Background(), c, ns2sTestAccount(), []byte(f2ToolTurnHistory), "", "")
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Equal(t, "assistant", gjson.GetBytes(upstream.lastBody, "messages.1.role").String())
	require.Equal(t, "call_1", gjson.GetBytes(upstream.lastBody, "messages.1.tool_calls.0.id").String())
	require.Equal(t, "think hard", gjson.GetBytes(upstream.lastBody, "messages.1.reasoning_content").String(),
		"命中缓存必须注入缓存里的真实明文，而非空格/占位")

	remarshaled, err := json.Marshal(json.RawMessage(upstream.lastBody))
	require.NoError(t, err)
	require.Contains(t, string(remarshaled), `"reasoning_content":"think hard"`)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "ok", gjson.Get(rec.Body.String(), "content.0.text").String())
}

// 直接对注入后的 chatReq 做 json.Marshal 断言：证明出站 JSON 的 reasoning_content
// 字段与值来自网关缓存。
func TestMessagesInjectCachedReasoning_MarshaledChatRequestCarriesReasoningContent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var anthropicReq apicompat.AnthropicRequest
	require.NoError(t, json.Unmarshal([]byte(f2ToolTurnHistory), &anthropicReq))
	chatReq, err := apicompat.AnthropicToChatCompletionsRequest(&anthropicReq)
	require.NoError(t, err)
	require.Len(t, chatReq.Messages, 3)
	require.Empty(t, chatReq.Messages[1].ReasoningContent, "注入前必须为空（客户端未回传 thinking，即 400 根因形态）")

	cache := &reasoningRecordingCache{getResp: map[string]string{
		messagesToolCallReasoningKey("call_1"): "think hard",
	}}
	svc := &OpenAIGatewayService{cache: cache}

	require.NoError(t, svc.injectCachedMessagesReasoning(context.Background(), &anthropicReq, chatReq, ns2sTestAccount(), "deepseek-chat"))

	out, err := json.Marshal(chatReq)
	require.NoError(t, err)
	require.Contains(t, string(out), `"reasoning_content":"think hard"`)
	require.Equal(t, "think hard", gjson.GetBytes(out, "messages.1.reasoning_content").String())
	require.Equal(t, "think hard", chatReq.Messages[1].ReasoningContent)
}

// F2 核心：缓存未命中 → 失败开放。请求照常发往上游、零注入（reasoning_content
// 字段不存在）、不返回 400、不给客户端任何提示。
func TestMessagesReasoningCache_MissForwardsUpstreamUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cache := &reasoningRecordingCache{getResp: map[string]string{}} // 全未命中
	upstream := f2JSONUpstream()
	svc := f2ReasoningService(upstream, cache)

	c, rec := f2NewContext(f2ToolTurnHistory)
	result, err := svc.ForwardAsAnthropic(context.Background(), c, ns2sTestAccount(), []byte(f2ToolTurnHistory), "", "")
	require.NoError(t, err, "未命中必须原样放行，不得失败关闭")
	require.NotNil(t, result)

	require.NotNil(t, upstream.lastReq, "失败开放：请求必须照常发往上游")
	require.NotEmpty(t, upstream.lastBody, "失败开放：必须产生出站请求体")
	require.Equal(t, "assistant", gjson.GetBytes(upstream.lastBody, "messages.1.role").String())
	require.False(t, gjson.GetBytes(upstream.lastBody, "messages.1.reasoning_content").Exists(),
		"零注入：未命中不得写入任何 reasoning_content（含占位）")
	require.NotContains(t, string(upstream.lastBody), "reasoning_content")

	require.Equal(t, http.StatusOK, rec.Code, "未命中不得返回 400")
	require.NotEqual(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "ok", gjson.Get(rec.Body.String(), "content.0.text").String())
	require.Contains(t, rec.Body.String(), `"type":"text"`)
}

// 缓存不可用（s.cache == nil）同样失败开放：不得阻断转发。
func TestMessagesReasoningCache_CacheUnavailableForwardsUpstreamUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := f2JSONUpstream()
	svc := f2NoCacheService(upstream)

	c, rec := f2NewContext(f2ToolTurnHistory)
	result, err := svc.ForwardAsAnthropic(context.Background(), c, ns2sTestAccount(), []byte(f2ToolTurnHistory), "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, upstream.lastReq, "缓存不可用也必须照常发往上游")
	require.Equal(t, http.StatusOK, rec.Code)
}

// 门控外路径（非 deepseek 模型 + 非 deepseek 平台）→ 出站字节不变：即便缓存里有
// 该 tool_call id 的 reasoning，也不得注入。
func TestMessagesReasoningCache_NonGatedPathLeavesUpstreamBodyUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cache := &reasoningRecordingCache{getResp: map[string]string{
		messagesToolCallReasoningKey("call_1"): "think hard",
	}}
	upstream := f2JSONUpstream()
	svc := f2ReasoningService(upstream, cache)

	// 非 deepseek：model=gpt-5.4 + 账号 platform=openai。
	body := strings.Replace(f2ToolTurnHistory, `"model":"deepseek-chat"`, `"model":"gpt-5.4"`, 1)
	c, rec := f2NewContext(body)
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

// ---- 写侧 ----

// 流式：上游流出的 reasoning_content + tool_call 按 tool_call id 写入缓存，键带
// msg_tool: 命名空间前缀。
func TestMessagesReasoningCache_StreamingWriteCachesReasoningByToolCallID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"deepseek-chat","max_tokens":16,"messages":[{"role":"user","content":"weather in sf?"}],"stream":true}`)
	c, rec := f2NewContext(string(body))

	upstreamBody := strings.Join([]string{
		`data: {"id":"chatcmpl_f2s","object":"chat.completion.chunk","model":"deepseek-chat","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_f2s","object":"chat.completion.chunk","model":"deepseek-chat","choices":[{"index":0,"delta":{"reasoning_content":"think "},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_f2s","object":"chat.completion.chunk","model":"deepseek-chat","choices":[{"index":0,"delta":{"reasoning_content":"first"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_f2s","object":"chat.completion.chunk","model":"deepseek-chat","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"sf\"}"}}]},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_f2s","object":"chat.completion.chunk","model":"deepseek-chat","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
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
	svc := f2ReasoningService(upstream, cache)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, ns2sTestAccount(), body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Contains(t, rec.Body.String(), `"type":"tool_use"`)

	sets := cache.snapshotSets()
	require.Equal(t, "think first", sets[messagesToolCallReasoningKey("call_1")],
		"流式 reasoning 必须按 tool_call id 写入缓存（键带 msg_tool: 前缀）")
}

// 漏写洞 1/4 —— 流式早退路径（scan.Err）：上游流中途读失败时，本轮已采集到的
// reasoning 与 tool_call id 仍必须落缓存，不得整轮丢弃。
func TestMessagesReasoningCache_StreamEarlyReturnStillCachesCollectedReasoning(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"deepseek-chat","max_tokens":16,"messages":[{"role":"user","content":"weather in sf?"}],"stream":true}`)
	c, _ := f2NewContext(string(body))

	// 断在 tool_call 之后：reasoning 与 tool_call id 都已采集完毕，但流未正常结束。
	partial := strings.Join([]string{
		`data: {"id":"chatcmpl_f2e","object":"chat.completion.chunk","model":"deepseek-chat","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_f2e","object":"chat.completion.chunk","model":"deepseek-chat","choices":[{"index":0,"delta":{"reasoning_content":"think partial"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_f2e","object":"chat.completion.chunk","model":"deepseek-chat","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},"finish_reason":null}]}`,
		"",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &errTailReader{data: []byte(partial), err: errors.New("simulated upstream read failure")},
	}}

	cache := &reasoningRecordingCache{}
	svc := f2ReasoningService(upstream, cache)

	_, err := svc.ForwardAsAnthropic(context.Background(), c, ns2sTestAccount(), body, "", "")
	require.Error(t, err, "早退语义不变：仍返回 stream usage incomplete")
	require.Contains(t, err.Error(), "stream usage incomplete")

	sets := cache.snapshotSets()
	require.Equal(t, "think partial", sets[messagesToolCallReasoningKey("call_1")],
		"早退路径不得丢弃本轮已采集到的 reasoning/tool_call id")
}

// 漏写洞 3/4 —— Finalize 阶段补宣布的 tool_use id 也要纳入采集：上游 tool name
// 迟到/缺失时，content_block_start 由 Finalize 而非 emitChunk 产出。
func TestMessagesReasoningCache_FinalizeAnnouncedToolUseIDCached(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"deepseek-chat","max_tokens":16,"messages":[{"role":"user","content":"weather in sf?"}],"stream":true}`)
	c, rec := f2NewContext(string(body))

	// function.name 缺失 → handleCCAnthropicToolCall 推迟宣布，content_block_start
	// 最终由 FinalizeChatCompletionsAnthropicStream 产出。
	upstreamBody := strings.Join([]string{
		`data: {"id":"chatcmpl_f2f","object":"chat.completion.chunk","model":"deepseek-chat","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_f2f","object":"chat.completion.chunk","model":"deepseek-chat","choices":[{"index":0,"delta":{"reasoning_content":"think deferred"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_f2f","object":"chat.completion.chunk","model":"deepseek-chat","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_late","type":"function","function":{"arguments":"{}"}}]},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_f2f","object":"chat.completion.chunk","model":"deepseek-chat","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
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
	svc := f2ReasoningService(upstream, cache)

	_, err := svc.ForwardAsAnthropic(context.Background(), c, ns2sTestAccount(), body, "", "")
	require.NoError(t, err)
	require.Contains(t, rec.Body.String(), `"id":"call_late"`)

	sets := cache.snapshotSets()
	require.Equal(t, "think deferred", sets[messagesToolCallReasoningKey("call_late")],
		"Finalize 补宣布的 tool_use id 必须纳入写侧采集")
}

// 非流式（buffer）：完整 chat.completion 响应里的 reasoning_content 按 tool_call id
// 缓存；多工具轮每个 tool_call id 写同一条 reasoning。
func TestMessagesReasoningCache_NonStreamingWriteCachesReasoningForEveryToolCallID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"deepseek-chat","max_tokens":16,"messages":[{"role":"user","content":"weather in sf?"}],"stream":false}`)
	c, _ := f2NewContext(string(body))

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"chatcmpl_f2n","object":"chat.completion","model":"deepseek-chat","choices":[{"index":0,"message":{"role":"assistant","reasoning_content":"think once","tool_calls":[{"id":"call_1","type":"function","function":{"name":"a","arguments":"{}"}},{"id":"call_2","type":"function","function":{"name":"b","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		)),
	}}

	cache := &reasoningRecordingCache{}
	svc := f2ReasoningService(upstream, cache)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, ns2sTestAccount(), body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)

	sets := cache.snapshotSets()
	require.Equal(t, "think once", sets[messagesToolCallReasoningKey("call_1")])
	require.Equal(t, "think once", sets[messagesToolCallReasoningKey("call_2")], "多工具轮每个 tool_call id 写同一条 reasoning")
}

// 漏写洞 2/4 —— converted 分支（collectCCStreamAsResponse）只给 `reasoning` 字段
// 别名时也必须累加，否则该分支撑性 reasoning 恒为空 → 漏写。
func TestMessagesReasoningCache_ConvertedBranchCachesReasoningFieldAlias(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"deepseek-chat","max_tokens":16,"messages":[{"role":"user","content":"weather in sf?"}],"stream":false}`)
	c, _ := f2NewContext(string(body))

	// delta 里只带 reasoning（不带 reasoning_content），且必须带上 converted 分支
	// 的完整性要素：usage 帧 + [DONE] + finish_reason。
	upstreamBody := strings.Join([]string{
		`data: {"id":"chatcmpl_f2c","object":"chat.completion.chunk","model":"deepseek-chat","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_f2c","object":"chat.completion.chunk","model":"deepseek-chat","choices":[{"index":0,"delta":{"reasoning":"think alias"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_f2c","object":"chat.completion.chunk","model":"deepseek-chat","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_alias","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_f2c","object":"chat.completion.chunk","model":"deepseek-chat","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
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
	svc := f2ConvertingService(upstream, cache)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, ns2sTestAccount(), body, "", "")
	require.NoError(t, err)
	require.NotNil(t, result)

	sets := cache.snapshotSets()
	require.Equal(t, "think alias", sets[messagesToolCallReasoningKey("call_alias")],
		"converted 分支必须同时认 reasoning 字段别名，否则该分支恒漏写")
}

// 未命中日志的 thinking 块形态判定（脱敏：只出布尔，不出正文/签名）。
func TestMessagesTurnThinkingShapes(t *testing.T) {
	var withText apicompat.AnthropicRequest
	require.NoError(t, json.Unmarshal([]byte(`{"model":"m","messages":[{"role":"assistant","content":[`+
		`{"type":"thinking","thinking":"abc","signature":"sig"},`+
		`{"type":"tool_use","id":"call_1","name":"f","input":{}}]}]}`), &withText))
	shapes := messagesTurnThinkingShapes(&withText)
	require.True(t, shapes["call_1"].hasThinkingText)
	require.False(t, shapes["call_1"].signatureOnly)

	var sigOnly apicompat.AnthropicRequest
	require.NoError(t, json.Unmarshal([]byte(`{"model":"m","messages":[{"role":"assistant","content":[`+
		`{"type":"thinking","signature":"sig"},`+
		`{"type":"tool_use","id":"call_2","name":"f","input":{}}]}]}`), &sigOnly))
	shapes = messagesTurnThinkingShapes(&sigOnly)
	require.False(t, shapes["call_2"].hasThinkingText)
	require.True(t, shapes["call_2"].signatureOnly)

	var noThinking apicompat.AnthropicRequest
	require.NoError(t, json.Unmarshal([]byte(`{"model":"m","messages":[{"role":"assistant","content":[`+
		`{"type":"tool_use","id":"call_3","name":"f","input":{}}]}]}`), &noThinking))
	shapes = messagesTurnThinkingShapes(&noThinking)
	require.False(t, shapes["call_3"].hasThinkingText)
	require.False(t, shapes["call_3"].signatureOnly)

	require.Empty(t, messagesTurnThinkingShapes(nil))
}
