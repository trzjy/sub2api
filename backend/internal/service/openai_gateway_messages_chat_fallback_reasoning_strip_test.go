//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// recordingGatewayCache 是 GatewayCache 的最小实现：除 SetReasoningContent 外全部
// 空操作，SetReasoningContent 把被写入的 reasoning 全文记录下来，供断言「tool 轮
// 缓存写入的是剥离前的原始明文」。包内自建，避免引入会构成 import cycle 的 testutil。
// ---------------------------------------------------------------------------

type recordingGatewayCache struct {
	mu        sync.Mutex
	recorded  []string
	setCalled int
}

var _ GatewayCache = (*recordingGatewayCache)(nil)

func (r *recordingGatewayCache) GetSessionAccountID(_ context.Context, _ int64, _ string) (int64, error) {
	return 0, ErrStickySessionNotFound
}
func (r *recordingGatewayCache) SetSessionAccountID(_ context.Context, _ int64, _ string, _ int64, _ time.Duration) error {
	return nil
}
func (r *recordingGatewayCache) RefreshSessionTTL(_ context.Context, _ int64, _ string, _ time.Duration) error {
	return nil
}
func (r *recordingGatewayCache) DeleteSessionAccountID(_ context.Context, _ int64, _ string) error {
	return nil
}
func (r *recordingGatewayCache) SetGrokVideoPendingBilling(_ context.Context, _ string, _ []byte, _ time.Duration) error {
	return nil
}
func (r *recordingGatewayCache) GetGrokVideoPendingBilling(_ context.Context, _ string) ([]byte, error) {
	return nil, nil
}
func (r *recordingGatewayCache) ClaimGrokVideoBilled(_ context.Context, _ string, _ time.Duration) (bool, error) {
	return false, nil
}
func (r *recordingGatewayCache) ReleaseGrokVideoBilled(_ context.Context, _ string) error {
	return nil
}
func (r *recordingGatewayCache) SetReasoningContent(_ context.Context, _ string, content string, _ time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setCalled++
	r.recorded = append(r.recorded, content)
	return nil
}
func (r *recordingGatewayCache) GetReasoningContent(_ context.Context, _ string) (string, error) {
	return "", ErrReasoningContentNotFound
}

func (r *recordingGatewayCache) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.recorded))
	copy(out, r.recorded)
	return out
}

// ---------------------------------------------------------------------------
// 构造辅助
// ---------------------------------------------------------------------------

func strptr(s string) *string { return &s }

// stripAccount 返回一个开启了 strip_reasoning_content 的掩码账号。
func stripAccount() *Account {
	return &Account{
		ID:       1,
		Name:     "strip-acc",
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Extra:    map[string]any{"strip_reasoning_content": true},
	}
}

// plainAccount 返回未开启 strip 的账号（与 stripAccount 仅 Extra 不同）。
func plainAccount() *Account {
	return &Account{
		ID:       2,
		Name:     "plain-acc",
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Extra:    map[string]any{},
	}
}

// deepseekAccount 返回 platform=deepseek 的账号，使 messagesDeepSeekThinkingGate
// 通过，从而 reasoning 缓存/回注路径真正落库（供 #5 断言）。
func deepseekAccount() *Account {
	return &Account{
		ID:       3,
		Name:     "ds-acc",
		Platform: PlatformDeepseek,
		Type:     AccountTypeAPIKey,
		Extra:    map[string]any{"strip_reasoning_content": true},
	}
}

const originalUpstreamThinking = "商汤日日新 thinking plaintext leaks upstream family"

func nonStreamCCResp() *apicompat.ChatCompletionsResponse {
	return &apicompat.ChatCompletionsResponse{
		ID:    "chatcmpl-test",
		Model: "sensenova-xyz",
		Choices: []apicompat.ChatChoice{
			{
				Message: apicompat.ChatMessage{
					Role:             "assistant",
					Content:          json.RawMessage(`"final answer"`),
					ReasoningContent: originalUpstreamThinking,
				},
				FinishReason: "stop",
			},
		},
	}
}

// ---------------------------------------------------------------------------
// 非流式
// ---------------------------------------------------------------------------

// TestMessagesChatFallbackReasoningStrip_NonStream_Stripped 覆盖断言 #1：
// 开启 strip → 转出的 Anthropic response content 里没有 type=="thinking" 的块。
func TestMessagesChatFallbackReasoningStrip_NonStream_Stripped(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	ccResp := nonStreamCCResp()
	resp := &http.Response{Header: http.Header{}}
	usage := OpenAIUsage{}

	_, err := svc.writeChatCompletionsAsAnthropicResult(c, stripAccount(), resp, ccResp, usage,
		"claude-3-5-sonnet", "billing-model", "sensenova-xyz", nil, nil, time.Now(), false)
	require.NoError(t, err)

	var anthropicResp apicompat.AnthropicResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &anthropicResp))

	for _, b := range anthropicResp.Content {
		require.NotEqual(t, "thinking", b.Type, "stripped response must not contain thinking block; got %+v", b)
	}
}

// TestMessagesChatFallbackReasoningStrip_NonStream_NotStripped 覆盖断言 #2 与 #6：
// 未开启 → 输出与不剥离时逐字节一致（仍含 thinking 块）。
func TestMessagesChatFallbackReasoningStrip_NonStream_NotStripped(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	ccResp := nonStreamCCResp()
	resp := &http.Response{Header: http.Header{}}
	usage := OpenAIUsage{}

	_, err := svc.writeChatCompletionsAsAnthropicResult(c, plainAccount(), resp, ccResp, usage,
		"claude-3-5-sonnet", "billing-model", "sensenova-xyz", nil, nil, time.Now(), false)
	require.NoError(t, err)

	var anthropicResp apicompat.AnthropicResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &anthropicResp))

	// #6 反向对照：不剥离时确实产出 thinking 块。
	var hasThinking bool
	for _, b := range anthropicResp.Content {
		if b.Type == "thinking" {
			hasThinking = true
			require.Equal(t, originalUpstreamThinking, b.Thinking)
		}
	}
	require.True(t, hasThinking, "non-stripped response must contain thinking block")

	// #2 逐字节一致：未剥离时的产出应等于直接调用转换函数的结果。
	ref := apicompat.ChatCompletionsResponseToAnthropic(nonStreamCCResp(), "claude-3-5-sonnet")
	refBytes, err := json.Marshal(ref)
	require.NoError(t, err)
	require.JSONEq(t, string(refBytes), w.Body.String())
}

// ---------------------------------------------------------------------------
// 流式
// ---------------------------------------------------------------------------

// buildStreamBody 把若干 CC chunk 序列化成 SSE 响应体（含 [DONE] 哨兵）。
func buildStreamBody(chunks []*apicompat.ChatCompletionsChunk) string {
	var sb strings.Builder
	for _, ch := range chunks {
		b, _ := json.Marshal(ch)
		sb.WriteString("data: ")
		sb.Write(b)
		sb.WriteString("\n")
	}
	sb.WriteString("data: [DONE]\n")
	return sb.String()
}

// reasoningChunk / textChunk / toolChunk 构造流式各阶段 chunk。
func reasoningChunk() *apicompat.ChatCompletionsChunk {
	return &apicompat.ChatCompletionsChunk{
		ID: "cc-stream",
		Choices: []apicompat.ChatChunkChoice{
			{Index: 0, Delta: apicompat.ChatDelta{ReasoningContent: strptr(originalUpstreamThinking)}},
		},
	}
}

func textChunk() *apicompat.ChatCompletionsChunk {
	return &apicompat.ChatCompletionsChunk{
		ID: "cc-stream",
		Choices: []apicompat.ChatChunkChoice{
			{Index: 0, Delta: apicompat.ChatDelta{Content: strptr("final answer")}},
		},
	}
}

func toolChunk() *apicompat.ChatCompletionsChunk {
	idx := 0
	return &apicompat.ChatCompletionsChunk{
		ID: "cc-stream",
		Choices: []apicompat.ChatChunkChoice{
			{
				Index: 0,
				Delta: apicompat.ChatDelta{
					ToolCalls: []apicompat.ChatToolCall{
						{
							Index: &idx,
							ID:    "call_tool_1",
							Type:  "function",
							Function: apicompat.ChatFunctionCall{
								Name:      "get_weather",
								Arguments: "",
							},
						},
					},
				},
			},
		},
	}
}

func usageChunk() *apicompat.ChatCompletionsChunk {
	return &apicompat.ChatCompletionsChunk{
		Choices: []apicompat.ChatChunkChoice{},
		Usage:   &apicompat.ChatUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}
}

// runStream 用给定账号跑一遍真实 streamChatCompletionsAsAnthropic，返回写出的 SSE
// 文本与录制缓存。
func runStream(t *testing.T, account *Account) (string, *recordingGatewayCache) {
	gin.SetMode(gin.TestMode)
	rec := &recordingGatewayCache{}
	svc := &OpenAIGatewayService{cache: rec}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	body := buildStreamBody([]*apicompat.ChatCompletionsChunk{
		reasoningChunk(), textChunk(), toolChunk(), usageChunk(),
	})
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	_, err := svc.streamChatCompletionsAsAnthropic(c, account, resp,
		"claude-3-5-sonnet", "billing-model", "deepseek-chat", nil, nil, time.Now())
	require.NoError(t, err)
	return w.Body.String(), rec
}

// parseSSEEvents 把 SSE 文本解析为 AnthropicStreamEvent 列表。
func parseAnthropicSSEEvents(t *testing.T, body string) []apicompat.AnthropicStreamEvent {
	var events []apicompat.AnthropicStreamEvent
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			continue
		}
		var evt apicompat.AnthropicStreamEvent
		require.NoError(t, json.Unmarshal([]byte(payload), &evt))
		events = append(events, evt)
	}
	return events
}

// TestMessagesChatFallbackReasoningStrip_Stream_Stripped 覆盖断言 #3：
// 开启 strip → 转出的 Anthropic 事件里没有 thinking_delta、没有 thinking 的
// content_block_start。
func TestMessagesChatFallbackReasoningStrip_Stream_Stripped(t *testing.T) {
	body, _ := runStream(t, stripAccount())
	events := parseAnthropicSSEEvents(t, body)

	for _, evt := range events {
		if evt.Type == "content_block_delta" && evt.Delta != nil {
			require.NotEqual(t, "thinking_delta", evt.Delta.Type,
				"stripped stream must not emit thinking_delta; event=%+v", evt)
		}
		if evt.Type == "content_block_start" && evt.ContentBlock != nil {
			require.NotEqual(t, "thinking", evt.ContentBlock.Type,
				"stripped stream must not emit thinking content_block_start; event=%+v", evt)
		}
	}

	// 副作用确认：工具块仍在，证明只剥离了 thinking。
	var hasToolUse bool
	for _, evt := range events {
		if evt.Type == "content_block_start" && evt.ContentBlock != nil && evt.ContentBlock.Type == "tool_use" {
			hasToolUse = true
			require.Equal(t, "call_tool_1", evt.ContentBlock.ID)
		}
	}
	require.True(t, hasToolUse, "tool_use block must survive stripping")
}

// normalizeEvent 抽取事件的可比较字段，忽略 message_start 里随机的 id/created。
type normEvent struct {
	Type          string
	DeltaType     string
	DeltaText     string
	DeltaThinking string
	BlockType     string
	BlockID       string
	BlockName     string
	StopReason    string
}

func normalizeEvent(e apicompat.AnthropicStreamEvent) normEvent {
	ne := normEvent{Type: e.Type}
	if e.Delta != nil {
		ne.DeltaType = e.Delta.Type
		ne.DeltaText = e.Delta.Text
		ne.DeltaThinking = e.Delta.Thinking
	}
	if e.ContentBlock != nil {
		ne.BlockType = e.ContentBlock.Type
		ne.BlockID = e.ContentBlock.ID
		ne.BlockName = e.ContentBlock.Name
	}
	if e.Usage != nil {
		_ = e.Usage // usage 字段在 message_delta，内容确定，无需逐字段比较
	}
	return ne
}

func normalizeEvents(events []apicompat.AnthropicStreamEvent) []normEvent {
	out := make([]normEvent, 0, len(events))
	for _, e := range events {
		out = append(out, normalizeEvent(e))
	}
	return out
}

// TestMessagesChatFallbackReasoningStrip_Stream_NotStripped 覆盖断言 #4 与 #6：
// #6 反向对照——不剥离时确实产出 thinking_delta / thinking content_block_start；
// #4 逐字节一致——未开启时事件序列与「直接调用 apicompat 转换」的规范序列完全一致
// （证明本卡的闸门是纯 no-op，未改动 OFF 路径）。
func TestMessagesChatFallbackReasoningStrip_Stream_NotStripped(t *testing.T) {
	body, _ := runStream(t, plainAccount())
	events := parseAnthropicSSEEvents(t, body)

	var hasThinkingDelta, hasThinkingStart bool
	for _, evt := range events {
		if evt.Type == "content_block_delta" && evt.Delta != nil && evt.Delta.Type == "thinking_delta" {
			hasThinkingDelta = true
			require.Equal(t, originalUpstreamThinking, evt.Delta.Thinking)
		}
		if evt.Type == "content_block_start" && evt.ContentBlock != nil && evt.ContentBlock.Type == "thinking" {
			hasThinkingStart = true
		}
	}
	require.True(t, hasThinkingDelta, "#6: non-stripped stream must emit thinking_delta")
	require.True(t, hasThinkingStart, "#6: non-stripped stream must emit thinking content_block_start")

	// #4 逐字节一致：用 apicompat 直接按相同 chunk 序列转换，得到规范事件序列，
	// 与本卡网关函数未开启时的输出逐事件一致（忽略随机 id/created）。
	state := apicompat.NewChatCompletionsToAnthropicStreamState("claude-3-5-sonnet")
	var refNorm []normEvent
	for _, ch := range []*apicompat.ChatCompletionsChunk{
		reasoningChunk(), textChunk(), toolChunk(), usageChunk(),
	} {
		refNorm = append(refNorm, normalizeEvents(apicompat.ChatCompletionsChunkToAnthropicEvents(ch, state))...)
	}
	refNorm = append(refNorm, normalizeEvents(apicompat.FinalizeChatCompletionsAnthropicStream(state))...)

	gotNorm := normalizeEvents(events)
	require.Equal(t, refNorm, gotNorm,
		"non-stripped stream must be byte-identical to canonical conversion; got=%+v ref=%+v", gotNorm, refNorm)
}

// TestMessagesChatFallbackReasoningStrip_Stream_CacheKeepsOriginal 覆盖断言 #5（核心）：
// 即便开启 strip，tool 轮缓存仍拿到剥离前的原始 reasoning 明文（不是空）。
// 用 platform=deepseek 账号使缓存路径真正落库；录制缓存必须记录原始明文。
func TestMessagesChatFallbackReasoningStrip_Stream_CacheKeepsOriginal(t *testing.T) {
	// 开启 strip 的 deepseek 账号
	body, rec := runStream(t, deepseekAccount())
	_ = body
	recorded := rec.snapshot()

	require.Greater(t, rec.setCalled, 0, "cache must be written for deepseek tool turn")
	var foundOriginal bool
	for _, v := range recorded {
		if strings.Contains(v, originalUpstreamThinking) {
			foundOriginal = true
		}
		require.NotEmpty(t, strings.TrimSpace(v), "cached reasoning must not be empty")
	}
	require.True(t, foundOriginal,
		"cache must hold the pre-strip ORIGINAL reasoning, not empty; recorded=%v", recorded)
}

// TestMessagesChatFallbackReasoningStrip_Stream_CacheKeepsOriginal_NoStrip 反向确认：
// 不开启 strip 时缓存同样写原始明文（证明采集逻辑本身正确，且 #5 非恒真）。
func TestMessagesChatFallbackReasoningStrip_Stream_CacheKeepsOriginal_NoStrip(t *testing.T) {
	acc := deepseekAccount()
	acc.Extra = map[string]any{"strip_reasoning_content": false}
	_, rec := runStream(t, acc)
	recorded := rec.snapshot()
	require.Greater(t, rec.setCalled, 0, "cache must be written for deepseek tool turn")
	var foundOriginal bool
	for _, v := range recorded {
		if strings.Contains(v, originalUpstreamThinking) {
			foundOriginal = true
		}
	}
	require.True(t, foundOriginal, "no-strip path also caches original reasoning; recorded=%v", recorded)
}
