//go:build unit

package service

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// newReasoningCCResponse 构造一个多 choice 的非流式 CC 响应，每个 choice 的
// Message 同时带 ReasoningContent 与 Reasoning（模拟 SenseNova 实测的两个字段），
// 并保留 Content / ToolCalls / Role / FinishReason 与顶层 Usage，用于验证剥离后
// 这些字段逐字节不变。
func newReasoningCCResponse() *apicompat.ChatCompletionsResponse {
	content := json.RawMessage(`"客户端可见正文"`)
	usage := &apicompat.ChatUsage{PromptTokens: 11, CompletionTokens: 22, TotalTokens: 33}
	return &apicompat.ChatCompletionsResponse{
		ID:    "cmpl-reason-1",
		Model: "sensenova-x",
		Choices: []apicompat.ChatChoice{
			{
				Index: 0,
				Message: apicompat.ChatMessage{
					Role:             "assistant",
					Content:          content,
					ReasoningContent: "商汤日日新思考明文reasoning_content",
					Reasoning:        "商汤日日新思考明文reasoning",
					ToolCalls: []apicompat.ChatToolCall{
						{
							ID:       "call_1",
							Type:     "function",
							Function: apicompat.ChatFunctionCall{Name: "lookup", Arguments: `{"q":1}`},
						},
					},
				},
				FinishReason: "tool_calls",
			},
			{
				Index: 1,
				Message: apicompat.ChatMessage{
					Role:             "assistant",
					Content:          content,
					ReasoningContent: "第二条reasoning_content",
					Reasoning:        "第二条reasoning",
				},
				FinishReason: "stop",
			},
		},
		Usage: usage,
	}
}

// newReasoningChunk 构造一个多 choice 的流式 CC chunk，每个 delta 同时带
// ReasoningContent 与 Reasoning，并保留 Content / ToolCalls / Role。
func newReasoningChunk() *apicompat.ChatCompletionsChunk {
	content := "客户端可见正文"
	rc := "商汤日日新流式reasoning_content"
	r := "商汤日日新流式reasoning"
	return &apicompat.ChatCompletionsChunk{
		ID:    "chunk-reason-1",
		Model: "sensenova-x",
		Choices: []apicompat.ChatChunkChoice{
			{
				Index: 0,
				Delta: apicompat.ChatDelta{
					Role:             "assistant",
					Content:          &content,
					ReasoningContent: &rc,
					Reasoning:        &r,
					ToolCalls: []apicompat.ChatToolCall{
						{
							ID:       "call_1",
							Type:     "function",
							Function: apicompat.ChatFunctionCall{Name: "lookup", Arguments: `{}`},
						},
					},
				},
			},
			{
				Index: 1,
				Delta: apicompat.ChatDelta{
					Content:          &content,
					ReasoningContent: &rc,
					Reasoning:        &r,
				},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// assertions on downstream conversion output
// ---------------------------------------------------------------------------

func hasAnthropicThinkingBlock(r *apicompat.AnthropicResponse) bool {
	if r == nil {
		return false
	}
	for _, b := range r.Content {
		if b.Type == "thinking" {
			return true
		}
	}
	return false
}

func hasResponsesReasoningItem(r *apicompat.ResponsesResponse) bool {
	if r == nil {
		return false
	}
	for _, o := range r.Output {
		if o.Type == "reasoning" {
			return true
		}
	}
	return false
}

func hasThinkingDelta(events []apicompat.AnthropicStreamEvent) bool {
	for _, e := range events {
		if e.Type == "content_block_delta" && e.Delta != nil && e.Delta.Type == "thinking_delta" {
			return true
		}
	}
	return false
}

func hasThinkingBlockStart(events []apicompat.AnthropicStreamEvent) bool {
	for _, e := range events {
		if e.Type == "content_block_start" && e.ContentBlock != nil && e.ContentBlock.Type == "thinking" {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 1) 非流式：清空并保留
// ---------------------------------------------------------------------------

func TestStripCCResponseReasoningClearsAndPreserves(t *testing.T) {
	resp := newReasoningCCResponse()

	// 剥离前快照（逐字节/逐字段）
	beforeContent := resp.Choices[0].Message.Content
	beforeToolCalls := resp.Choices[0].Message.ToolCalls
	beforeRole := resp.Choices[0].Message.Role
	beforeFinish := resp.Choices[0].FinishReason
	beforeUsage := resp.Usage

	stripCCResponseReasoning(resp)

	// 两个字段都被清空（两个 choice 都验证）
	for i, c := range resp.Choices {
		require.Empty(t, c.Message.ReasoningContent, "choice %d ReasoningContent 应为空", i)
		require.Empty(t, c.Message.Reasoning, "choice %d Reasoning 应为空", i)
	}

	// 其余字段逐字节/逐字段不变
	require.Equal(t, beforeContent, resp.Choices[0].Message.Content, "Content 不应变")
	require.Equal(t, beforeToolCalls, resp.Choices[0].Message.ToolCalls, "ToolCalls 不应变")
	require.Equal(t, beforeRole, resp.Choices[0].Message.Role, "Role 不应变")
	require.Equal(t, beforeFinish, resp.Choices[0].FinishReason, "FinishReason 不应变")
	require.Equal(t, beforeUsage, resp.Usage, "Usage 不应变")
}

// ---------------------------------------------------------------------------
// 2) 非流式 nil 不 panic
// ---------------------------------------------------------------------------

func TestStripCCResponseReasoningNil(t *testing.T) {
	require.NotPanics(t, func() { stripCCResponseReasoning(nil) })
}

// ---------------------------------------------------------------------------
// 3) 流式：清空并保留
// ---------------------------------------------------------------------------

func TestStripCCChunkReasoningClearsAndPreserves(t *testing.T) {
	chunk := newReasoningChunk()

	beforeContent := chunk.Choices[0].Delta.Content
	beforeToolCalls := chunk.Choices[0].Delta.ToolCalls
	beforeRole := chunk.Choices[0].Delta.Role

	stripCCChunkReasoning(chunk)

	for i, c := range chunk.Choices {
		require.Nil(t, c.Delta.ReasoningContent, "choice %d ReasoningContent 应为 nil", i)
		require.Nil(t, c.Delta.Reasoning, "choice %d Reasoning 应为 nil", i)
	}

	require.Equal(t, beforeContent, chunk.Choices[0].Delta.Content, "Content 不应变")
	require.Equal(t, beforeToolCalls, chunk.Choices[0].Delta.ToolCalls, "ToolCalls 不应变")
	require.Equal(t, beforeRole, chunk.Choices[0].Delta.Role, "Role 不应变")
}

// ---------------------------------------------------------------------------
// 4) 流式 nil 不 panic
// ---------------------------------------------------------------------------

func TestStripCCChunkReasoningNil(t *testing.T) {
	require.NotPanics(t, func() { stripCCChunkReasoning(nil) })
}

// ---------------------------------------------------------------------------
// 5) 下游转换实效（核心断言）：剥离后 Anthropic/Responses 不再泄漏思考明文
// ---------------------------------------------------------------------------

func TestStripCCResponseReasoningBlocksDownstreamLeak(t *testing.T) {
	// 控制组：不剥离 → 下游转换确实产生 thinking 块 / reasoning item
	ctrl := newReasoningCCResponse()
	anthCtrl := apicompat.ChatCompletionsResponseToAnthropic(ctrl, "m")
	require.True(t, hasAnthropicThinkingBlock(anthCtrl), "控制组：不剥离时 Anthropic 应含 thinking 块（证明断言非恒真）")
	respCtrl := apicompat.ChatCompletionsResponseToResponses(ctrl, "m", nil, nil, false, nil)
	require.True(t, hasResponsesReasoningItem(respCtrl), "控制组：不剥离时 Responses 应含 reasoning item（证明断言非恒真）")

	// 实验组：剥离 → 下游转换不再产生 thinking 块 / reasoning item
	stripped := newReasoningCCResponse()
	stripCCResponseReasoning(stripped)
	anth := apicompat.ChatCompletionsResponseToAnthropic(stripped, "m")
	require.False(t, hasAnthropicThinkingBlock(anth), "剥离后 Anthropic 不应含 thinking 块")
	resp := apicompat.ChatCompletionsResponseToResponses(stripped, "m", nil, nil, false, nil)
	require.False(t, hasResponsesReasoningItem(resp), "剥离后 Responses 不应含 reasoning item")
}

// ---------------------------------------------------------------------------
// 6) 流式实效：剥离后 Anthropic 流事件不再含 thinking_delta / thinking 块 start
// ---------------------------------------------------------------------------

func TestStripCCChunkReasoningBlocksStreamLeak(t *testing.T) {
	// 控制组：不剥离 → 确实存在 thinking_delta 与 thinking content_block_start
	ctrl := newReasoningChunk()
	ctrlEvents := apicompat.ChatCompletionsChunkToAnthropicEvents(ctrl, apicompat.NewChatCompletionsToAnthropicStreamState("m"))
	require.True(t, hasThinkingDelta(ctrlEvents), "控制组：不剥离时流事件应含 thinking_delta（证明断言非恒真）")
	require.True(t, hasThinkingBlockStart(ctrlEvents), "控制组：不剥离时流事件应含 thinking content_block_start（证明断言非恒真）")

	// 实验组：剥离 → 不再有 thinking_delta / thinking 块 start
	stripped := newReasoningChunk()
	stripCCChunkReasoning(stripped)
	events := apicompat.ChatCompletionsChunkToAnthropicEvents(stripped, apicompat.NewChatCompletionsToAnthropicStreamState("m"))
	require.False(t, hasThinkingDelta(events), "剥离后流事件不应含 thinking_delta")
	require.False(t, hasThinkingBlockStart(events), "剥离后流事件不应含 thinking content_block_start")
}

func TestStripCCResponseReasoningClearsBothFields(t *testing.T) {
	resp := newReasoningCCResponse()
	stripCCResponseReasoning(resp)
	for i := range resp.Choices {
		require.Empty(t, resp.Choices[i].Message.ReasoningContent, "choice %d ReasoningContent not cleared", i)
		require.Empty(t, resp.Choices[i].Message.Reasoning, "choice %d Reasoning not cleared", i)
	}
}
