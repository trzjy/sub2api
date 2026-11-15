//go:build unit

package service

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

// 「上游 200 但空白回答」的判据必须两条入站路径同源：内容块类型决定是否有东西
// 可渲染，thinking 不算。这里锁死边界取值，防止以后新增块类型被误判成空。
func TestAnthropicSSELineCarriesPayloadContent(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"正文块开始", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`, true},
		{"思考块开始", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`, false},
		{"脱敏思考块开始", `{"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking"}}`, false},
		{"工具调用块开始", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_1","name":"Read"}}`, true},
		{"文本增量", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"你好"}}`, true},
		{"工具参数增量", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"a\""}}`, true},
		{"思考增量", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"嗯"}}`, false},
		{"签名增量", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"abc"}}`, false},
		{"块结束", `{"type":"content_block_stop","index":1}`, false},
		{"消息开始", `{"type":"message_start","message":{"id":"msg_1"}}`, false},
		{"终态", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`, false},
		{"空行", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := anthropicSSELineCarriesPayloadContent(tc.payload); got != tc.want {
				t.Fatalf("anthropicSSELineCarriesPayloadContent(%s) = %v, want %v", tc.payload, got, tc.want)
			}
		})
	}
}

// 非流式响应体的同源判据。结构不可识别时必须放行（返回 true），
// 只拦「明确是空 content 数组 / 只有 thinking」。
func TestAnthropicResponseHasRenderableContent(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"空内容数组", `{"type":"message","content":[],"stop_reason":"end_turn"}`, false},
		{"只有思考块", `{"type":"message","content":[{"type":"thinking","thinking":"嗯"}],"stop_reason":"end_turn"}`, false},
		{"正文加思考", `{"type":"message","content":[{"type":"thinking","thinking":"嗯"},{"type":"text","text":"答案"}]}`, true},
		{"只有工具调用", `{"type":"message","content":[{"type":"tool_use","id":"tu_1","name":"Read","input":{}}]}`, true},
		{"content 缺失", `{"type":"message","id":"msg_1"}`, true},
		{"content 不是数组", `{"type":"message","content":"不是数组"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := anthropicResponseHasRenderableContent([]byte(tc.body)); got != tc.want {
				t.Fatalf("anthropicResponseHasRenderableContent(%s) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// 空回答换号错误必须带 SafeToFailoverAfterWrite —— 判定成立的前提就是客户端
// 一个语义字节都没收到，同流换号安全；漏掉这个标志上层会拒绝对已提交响应换号。
func TestNewEmptyAnthropicContentFailoverError(t *testing.T) {
	err := newEmptyAnthropicContentFailoverError()
	if err == nil {
		t.Fatal("expected non-nil failover error")
	}
	if !err.SafeToFailoverAfterWrite {
		t.Fatal("SafeToFailoverAfterWrite must be true: nothing semantic was written to the client")
	}
	if err.StatusCode != 502 {
		t.Fatalf("StatusCode = %d, want 502", err.StatusCode)
	}
}

// 记录一个事实：thinking-only 流经 CC 转换链后是否会产生「客户端可见的正文字节」。
// 这条决定了 /v1/chat/completions 入站能不能在 finalize 阶段判空并换号。
func TestCCConversionThinkingOnlyProducesNoTextChunks(t *testing.T) {
	events := []string{
		`{"type":"message_start","message":{"id":"msg_1","model":"spark-x2.6","role":"assistant","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"先想一想"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":12}}`,
		`{"type":"message_stop"}`,
	}

	anthState := apicompat.NewAnthropicEventToResponsesState()
	ccState := apicompat.NewResponsesEventToChatState()

	textBytes := 0
	consume := func(evt apicompat.AnthropicStreamEvent) {
		for _, resEvt := range apicompat.AnthropicEventToResponsesEvents(&evt, anthState) {
			for _, chunk := range apicompat.ResponsesEventToChatChunks(&resEvt, ccState) {
				for _, choice := range chunk.Choices {
					if choice.Delta.Content != nil {
						textBytes += len(*choice.Delta.Content)
					}
				}
			}
		}
	}
	for _, raw := range events {
		var evt apicompat.AnthropicStreamEvent
		if err := json.Unmarshal([]byte(raw), &evt); err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		consume(evt)
	}
	for _, resEvt := range apicompat.FinalizeAnthropicResponsesStream(anthState) {
		for _, chunk := range apicompat.ResponsesEventToChatChunks(&resEvt, ccState) {
			for _, choice := range chunk.Choices {
				if choice.Delta.Content != nil {
					textBytes += len(*choice.Delta.Content)
				}
			}
		}
	}
	if textBytes != 0 {
		t.Fatalf("thinking-only stream produced %d text bytes, expected 0 (gate would be useless)", textBytes)
	}
}

// CC 出站的同源判据：role 帧 / 思考 / finish / usage 都不算内容。
func TestChatChunkCarriesPayloadContent(t *testing.T) {
	str := func(s string) *string { return &s }
	cases := []struct {
		name  string
		chunk apicompat.ChatCompletionsChunk
		want  bool
	}{
		{
			name:  "只有 role",
			chunk: apicompat.ChatCompletionsChunk{Choices: []apicompat.ChatChunkChoice{{Delta: apicompat.ChatDelta{Role: "assistant"}}}},
			want:  false,
		},
		{
			name:  "只有思考",
			chunk: apicompat.ChatCompletionsChunk{Choices: []apicompat.ChatChunkChoice{{Delta: apicompat.ChatDelta{ReasoningContent: str("想一想")}}}},
			want:  false,
		},
		{
			name:  "只有 finish_reason",
			chunk: apicompat.ChatCompletionsChunk{Choices: []apicompat.ChatChunkChoice{{FinishReason: str("stop")}}},
			want:  false,
		},
		{
			name:  "空字符串正文不算",
			chunk: apicompat.ChatCompletionsChunk{Choices: []apicompat.ChatChunkChoice{{Delta: apicompat.ChatDelta{Content: str("")}}}},
			want:  false,
		},
		{
			name:  "有正文",
			chunk: apicompat.ChatCompletionsChunk{Choices: []apicompat.ChatChunkChoice{{Delta: apicompat.ChatDelta{Content: str("答案")}}}},
			want:  true,
		},
		{
			name:  "有工具调用",
			chunk: apicompat.ChatCompletionsChunk{Choices: []apicompat.ChatChunkChoice{{Delta: apicompat.ChatDelta{ToolCalls: []apicompat.ChatToolCall{{ID: "tu_1"}}}}}},
			want:  true,
		},
		{
			name:  "有拒答",
			chunk: apicompat.ChatCompletionsChunk{Choices: []apicompat.ChatChunkChoice{{Delta: apicompat.ChatDelta{Refusal: str("无法回答")}}}},
			want:  true,
		},
		{
			name:  "无 choices",
			chunk: apicompat.ChatCompletionsChunk{},
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chatChunkCarriesPayloadContent(tc.chunk); got != tc.want {
				t.Fatalf("chatChunkCarriesPayloadContent() = %v, want %v", got, tc.want)
			}
		})
	}
}

// 思考增量必须能放行缓冲：扣着会让「逐字出现的思考」变成长时间空白。
func TestChatChunkCarriesReasoning(t *testing.T) {
	str := func(s string) *string { return &s }
	cases := []struct {
		name  string
		chunk apicompat.ChatCompletionsChunk
		want  bool
	}{
		{"reasoning_content", apicompat.ChatCompletionsChunk{Choices: []apicompat.ChatChunkChoice{{Delta: apicompat.ChatDelta{ReasoningContent: str("想")}}}}, true},
		{"reasoning", apicompat.ChatCompletionsChunk{Choices: []apicompat.ChatChunkChoice{{Delta: apicompat.ChatDelta{Reasoning: str("想")}}}}, true},
		{"空思考不算", apicompat.ChatCompletionsChunk{Choices: []apicompat.ChatChunkChoice{{Delta: apicompat.ChatDelta{ReasoningContent: str("")}}}}, false},
		{"只有 role", apicompat.ChatCompletionsChunk{Choices: []apicompat.ChatChunkChoice{{Delta: apicompat.ChatDelta{Role: "assistant"}}}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chatChunkCarriesReasoning(tc.chunk); got != tc.want {
				t.Fatalf("chatChunkCarriesReasoning() = %v, want %v", got, tc.want)
			}
		})
	}
}
