//go:build unit

package service

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// generatedItemIDRe 匹配 apicompat.generateItemID 生成的随机 item id（12 随机字节
// → 24 hex）。每次转换都会重新随机生成，故两次独立转换（网关调用 vs 公共转换层基准）
// 的 id 必然不同——比较前统一归一化，使"逐字节一致"的断言只针对结构与内容差异，
// 排除固有随机性。
var generatedItemIDRe = regexp.MustCompile(`"item_[0-9a-f]{24}"`)

func normalizeGeneratedIDs(s string) string {
	return generatedItemIDRe.ReplaceAllString(s, `"item_NORMALIZED"`)
}

// 卡 C 单测：/v1/responses 降级路径（CC→Responses 双转换）的 L3 思考层剥离调用点。
// 覆盖非流式 writeChatCompletionsAsResponsesResult 与流式 streamChatCompletionsAsResponses
// 两处：开启 strip_reasoning_content 的账号在转换前就地剥离上游 reasoning 明文，
// 客户端可见响应不再含 reasoning item / reasoning 事件；未开启账号字节不变。
//
// 缓存判断依据（写入完工报告）：cacheReasoningItemsFromOutput/FromEvents 按 reasoning
// item id 索引缓存，仅当客户端在下一轮 input 里回传同 id 的 reasoning item 时，
// chatcompletions_responses_bridge.go 的 buildChatMessagesFromItems 才会经
// ReasoningContentByID 回退查用该缓存。客户端看不到 reasoning item（被剥离）就不会
// 回传其 id，故缓存天然为空，无需保留剥离前明文——本卡采用最简实现。

// upstreamCCResponseWithReasoning 构造一个带 reasoning_content 明文（含上游身份词）的
// 非流式 CC 响应，供两个写出点消费。
func upstreamCCResponseWithReasoning() *apicompat.ChatCompletionsResponse {
	return &apicompat.ChatCompletionsResponse{
		ID:      "resp_1",
		Object:  "chat.completion",
		Created: 1,
		Model:   "sensenova-x",
		Choices: []apicompat.ChatChoice{{
			Index: 0,
			Message: apicompat.ChatMessage{
				Role:             "assistant",
				Content:          json.RawMessage(`"hi"`),
				ReasoningContent: "商汤日日新思考过程",
			},
			FinishReason: "stop",
		}},
		Usage: &apicompat.ChatUsage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5},
	}
}

// upstreamCCStreamWithReasoning 构造带 reasoning_content chunk 的上游 SSE 体。
func upstreamCCStreamWithReasoning() string {
	lines := []string{
		`data: {"id":"s1","object":"chat.completion.chunk","model":"sensenova-x","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`data: {"id":"s1","model":"sensenova-x","choices":[{"index":0,"delta":{"reasoning_content":"商汤日日新思考"}}]}`,
		`data: {"id":"s1","model":"sensenova-x","choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`data: {"id":"s1","model":"sensenova-x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: {"id":"s1","model":"sensenova-x","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		"data: [DONE]",
	}
	return strings.Join(lines, "\n")
}

// referenceResponsesStreamFromCCSSE 用公共转换层（apicompat）直接把上游 CC SSE 转成
// Responses SSE 字符串——这正是网关在非剥离分支里做的事。作为"未开启账号"的字节基准，
// 用于证明剥离分支外行为逐字节不变（不重新实现一份转换逻辑）。
func referenceResponsesStreamFromCCSSE(upstreamSSE string) string {
	state := apicompat.NewChatCompletionsToResponsesStreamState("sensenova-x")
	var sb strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(upstreamSSE))
	for scanner.Scan() {
		payload, ok := extractOpenAISSEDataLine(scanner.Text())
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk apicompat.ChatCompletionsChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		for _, ev := range apicompat.ChatCompletionsChunkToResponsesEvents(&chunk, state) {
			if sse, err := apicompat.ResponsesEventToSSE(ev); err == nil {
				sb.WriteString(sse)
			}
		}
	}
	for _, ev := range apicompat.FinalizeChatCompletionsResponsesStream(state) {
		if sse, err := apicompat.ResponsesEventToSSE(ev); err == nil {
			sb.WriteString(sse)
		}
	}
	sb.WriteString("data: [DONE]\n\n")
	return sb.String()
}

// --- 用例 1：非流式 开启账号 → 无 reasoning item ---

func TestResponsesChatFallback_NonStreamingStrippedRemovesReasoningItem(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{Header: http.Header{}}

	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
	account := newReasoningStrippedTestAccount(301, map[string]any{"sensenova-x": "sensenova-x"})
	ccResp := upstreamCCResponseWithReasoning()

	_, err := svc.writeChatCompletionsAsResponsesResult(
		c, account, resp, ccResp, OpenAIUsage{}, "sensenova-x",
		nil, nil, false, nil, "sensenova-x", "sensenova-x", nil, nil, time.Now(), false,
	)
	require.NoError(t, err)

	downstream := rec.Body.String()
	require.NotContains(t, downstream, "商汤", "开启账号 reasoning 明文必须被剥离")
	hasReasoning := false
	gjson.Parse(downstream).Get("output").ForEach(func(_, v gjson.Result) bool {
		if v.Get("type").String() == "reasoning" {
			hasReasoning = true
		}
		return true
	})
	require.False(t, hasReasoning, "开启账号 output 不得含 type==reasoning 的 item")
	// content 与 usage 不受影响
	require.Contains(t, downstream, `"text":"hi"`, "开启账号 message content 必须保留")
	require.Equal(t, int64(5), gjson.Get(downstream, "usage.total_tokens").Int())
}

// --- 用例 2：非流式 未开启账号 → 字节不变（仍含 reasoning item） ---

func TestResponsesChatFallback_NonStreamingPlainKeepsReasoningByteIdentical(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{Header: http.Header{}}

	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
	account := newPlainTestAccount(302, map[string]any{"sensenova-x": "sensenova-x"})
	ccResp := upstreamCCResponseWithReasoning()

	_, err := svc.writeChatCompletionsAsResponsesResult(
		c, account, resp, ccResp, OpenAIUsage{}, "sensenova-x",
		nil, nil, false, nil, "sensenova-x", "sensenova-x", nil, nil, time.Now(), false,
	)
	require.NoError(t, err)

	downstream := rec.Body.String()
	// 字节基准：公共转换层直接转出的 JSON（网关非剥离分支正是如此）。item id 为随机生成，
	// 两次独立转换 id 必不同，比较前归一化（见 normalizeGeneratedIDs）。
	ref := apicompat.ChatCompletionsResponseToResponses(ccResp, "sensenova-x", nil, nil, false, nil)
	refBytes, err := json.Marshal(ref)
	require.NoError(t, err)
	require.Equal(t, normalizeGeneratedIDs(string(refBytes)), normalizeGeneratedIDs(downstream),
		"未开启账号输出须与公共转换层逐字节一致（排除随机 item id）")

	// 明确仍含 reasoning item（反向对照的基石）
	hasReasoning := false
	gjson.Parse(downstream).Get("output").ForEach(func(_, v gjson.Result) bool {
		if v.Get("type").String() == "reasoning" {
			hasReasoning = true
		}
		return true
	})
	require.True(t, hasReasoning, "未开启账号 output 必须含 reasoning item")
	require.Contains(t, downstream, "商汤", "未开启账号必须保留 reasoning 明文")
}

// --- 用例 3：流式 开启账号 → 无 reasoning item 事件 ---

func TestResponsesChatFallback_StreamingStrippedRemovesReasoningEvents(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	upstreamSSE := upstreamCCStreamWithReasoning()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamSSE)),
	}

	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
	account := newReasoningStrippedTestAccount(303, map[string]any{"sensenova-x": "sensenova-x"})

	_, err := svc.streamChatCompletionsAsResponses(
		c, account, resp, "sensenova-x",
		nil, nil, false, nil, "sensenova-x", "sensenova-x", nil, nil, time.Now(),
	)
	require.NoError(t, err)

	downstream := rec.Body.String()
	require.Contains(t, downstream, "data: [DONE]")
	require.NotContains(t, downstream, "商汤", "开启账号流式 reasoning 明文必须被剥离")

	for _, line := range strings.Split(downstream, "\n") {
		payload, ok := extractOpenAISSEDataLine(line)
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "" || payload == "[DONE]" {
			continue
		}
		evType := gjson.Get(payload, "type").String()
		require.NotEqual(t, "response.reasoning_summary_text.delta", evType,
			"不得出现 reasoning summary delta 事件: %s", line)
		if evType == "response.output_item.added" {
			require.NotEqual(t, "reasoning", gjson.Get(payload, "item.type").String(),
				"不得出现 reasoning 类型的 output_item.added: %s", line)
		}
	}
	// content 帧保留
	require.Contains(t, downstream, `"text":"hi"`, "开启账号流式 message content 必须保留")
}

// --- 用例 4：流式 未开启账号 → 字节不变（仍含 reasoning 事件） ---

func TestResponsesChatFallback_StreamingPlainKeepsReasoningByteIdentical(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	upstreamSSE := upstreamCCStreamWithReasoning()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamSSE)),
	}

	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
	account := newPlainTestAccount(304, map[string]any{"sensenova-x": "sensenova-x"})

	_, err := svc.streamChatCompletionsAsResponses(
		c, account, resp, "sensenova-x",
		nil, nil, false, nil, "sensenova-x", "sensenova-x", nil, nil, time.Now(),
	)
	require.NoError(t, err)

	downstream := rec.Body.String()
	// item id 随机生成，两次独立转换必不同，比较前归一化（见 normalizeGeneratedIDs）。
	require.Equal(t, normalizeGeneratedIDs(referenceResponsesStreamFromCCSSE(upstreamSSE)), normalizeGeneratedIDs(downstream),
		"未开启账号流式输出须与公共转换层逐字节一致（排除随机 item id）")

	// 明确仍含 reasoning 事件（反向对照）
	foundReasoning := false
	for _, line := range strings.Split(downstream, "\n") {
		payload, ok := extractOpenAISSEDataLine(line)
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "" || payload == "[DONE]" {
			continue
		}
		if gjson.Get(payload, "type").String() == "response.reasoning_summary_text.delta" {
			foundReasoning = true
		}
	}
	require.True(t, foundReasoning, "未开启账号流式输出必须含 reasoning summary delta 事件")
	require.Contains(t, downstream, "商汤", "未开启账号流式必须保留 reasoning 明文")
}

// --- 用例 5：反向对照 —— 证明断言不是恒真（同输入不剥离确产 reasoning） ---

func TestResponsesChatFallback_ReverseControlProducesReasoning(t *testing.T) {
	// 非流式：公共转换层（不剥离）必须产出 reasoning item
	ccResp := upstreamCCResponseWithReasoning()
	ref := apicompat.ChatCompletionsResponseToResponses(ccResp, "sensenova-x", nil, nil, false, nil)
	hasReasoning := false
	for _, out := range ref.Output {
		if out.Type == "reasoning" {
			hasReasoning = true
		}
	}
	require.True(t, hasReasoning, "反向对照：不剥离时公共转换层必须产出 reasoning item")
	require.Contains(t, referenceResponsesStreamFromCCSSE(upstreamCCStreamWithReasoning()),
		"response.reasoning_summary_text.delta",
		"反向对照：不剥离时流式必须产出 reasoning summary delta 事件")
}
