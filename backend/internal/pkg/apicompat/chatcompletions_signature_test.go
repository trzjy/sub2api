package apicompat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A1: 响应方向的 thinking 块必须恒带 signature 键（值可空串），对齐先例 412718edf5。
// 非 DeepSeek/Kira 上游若不下发 thinking 块则字节不变；下发 thinking 块时补 signature。
func TestAnthropicContentBlock_ThinkingAlwaysEmitsSignature(t *testing.T) {
	// 带明文的 thinking 块：signature 必须出现（即使为空串）。
	withThinking := AnthropicContentBlock{Type: "thinking", Thinking: "let me think"}
	b, err := json.Marshal(withThinking)
	require.NoError(t, err)
	require.Contains(t, string(b), `"signature"`, "thinking 块必须含 signature 键")
	require.Contains(t, string(b), `"thinking":"let me think"`)

	// 无 signature 字段的 thinking 块：仍必须输出 signature 键（空串占位，不伪造非空）。
	noSig := AnthropicContentBlock{Type: "thinking", Thinking: "x"}
	b2, err := json.Marshal(noSig)
	require.NoError(t, err)
	require.Contains(t, string(b2), `"signature":""`, "缺 signature 时必须补空串占位，不得伪造")

	// 非 thinking 块不应受影响（回归：text 块字节不变）。
	textBlock := AnthropicContentBlock{Type: "text", Text: "hello"}
	tb, err := json.Marshal(textBlock)
	require.NoError(t, err)
	require.NotContains(t, string(tb), "signature", "text 块不得引入 signature 键")
	require.Equal(t, `{"text":"hello","type":"text"}`, strings.ReplaceAll(string(tb), " ", ""))
}

// A1: 流式 content_block_start 的 thinking 块也走同一 MarshalJSON，必须带 signature。
func TestAnthropicStreamThinkingBlockStart_EmitsSignature(t *testing.T) {
	state := NewChatCompletionsToAnthropicStreamState("m")
	// 先喂一条 reasoning delta 打开 thinking 块。
	evs := ChatCompletionsChunkToAnthropicEvents(&ChatCompletionsChunk{
		Choices: []ChatChunkChoice{{Delta: ChatDelta{ReasoningContent: strPtr("r")}}},
	}, state)
	require.NotEmpty(t, evs)

	var foundThinkingStart bool
	for _, ev := range evs {
		if ev.Type == "content_block_start" && ev.ContentBlock != nil && ev.ContentBlock.Type == "thinking" {
			foundThinkingStart = true
			raw, err := json.Marshal(ev.ContentBlock)
			require.NoError(t, err)
			require.Contains(t, string(raw), `"signature"`, "流式 thinking content_block_start 必须含 signature 键")
		}
	}
	require.True(t, foundThinkingStart, "应产出 thinking 的 content_block_start")
}

// A1 回归：整条 Anthropic 响应（仅 text，无 thinking）序列化字节与思考块无关字段不变。
func TestAnthropicResponse_NoThinkingBlockUnchanged(t *testing.T) {
	resp := AnthropicResponse{
		ID:   "msg_1",
		Type: "message",
		Role: "assistant",
		Content: []AnthropicContentBlock{
			{Type: "text", Text: "plain answer"},
		},
		Model:      "some-model",
		StopReason: strPtr("end_turn"),
		Usage:      AnthropicUsage{InputTokens: 1, OutputTokens: 2},
	}
	b, err := json.Marshal(resp)
	require.NoError(t, err)
	require.NotContains(t, string(b), "signature", "无 thinking 块时不得出现 signature 键")
}

// A1 增强（外审 #2 建议）：thinking 块带非空 signature 时必须原样保留（不覆盖、不伪造）。
func TestAnthropicContentBlock_ThinkingPreservesNonEmptySignature(t *testing.T) {
	withSig := AnthropicContentBlock{Type: "thinking", Thinking: "let me think", Signature: "sig_real_abc"}
	b, err := json.Marshal(withSig)
	require.NoError(t, err)
	require.Contains(t, string(b), `"signature":"sig_real_abc"`, "非空 signature 必须原样保留，不得清空或替换")
	require.Contains(t, string(b), `"thinking":"let me think"`)
}
