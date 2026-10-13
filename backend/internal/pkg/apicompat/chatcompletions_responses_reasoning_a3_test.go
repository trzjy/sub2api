package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// A3 (Done when #1a): 纯文本 assistant 轮（无 tool_use）也必须回传上一 reasoning
// 轮的明文 reasoning_content。DeepSeek thinking mode 要求每一 assistant 轮都带
// reasoning_content，纯文本轮漏传也会被 400。这里断言出站 Chat 请求的 JSON 里该
// 轮 reasoning_content 字段与值都正确。
func TestResponsesToChat_PlainTextTurnCarriesReasoningContent(t *testing.T) {
	req := &ResponsesRequest{
		Model: "deepseek-reasoner",
		Input: json.RawMessage(`[
			{"type":"reasoning","id":"item_r1","summary":[{"type":"summary_text","text":"my secret reasoning"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"final answer"}]}
		]`),
	}

	out, err := ResponsesToChatCompletionsRequest(req)
	require.NoError(t, err)

	var assistant *ChatMessage
	for i := range out.Messages {
		if out.Messages[i].Role == "assistant" && len(out.Messages[i].ToolCalls) == 0 {
			assistant = &out.Messages[i]
			break
		}
	}
	require.NotNil(t, assistant, "应存在纯文本 assistant 轮")
	require.Equal(t, "my secret reasoning", assistant.ReasoningContent)

	// 断言最终出站 JSON（非内存对象）含 reasoning_content 且值正确。
	body, err := json.Marshal(out)
	require.NoError(t, err)
	require.Contains(t, string(body), `"reasoning_content":"my secret reasoning"`,
		"出站 JSON 必须含 reasoning_content 明文")

	// 非 DeepSeek 上游（无 reasoning 项）字节不受影响：同结构但不含 thinking。
	nonDS := &ResponsesRequest{
		Model: "gpt-4o",
		Input: json.RawMessage(`[
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}
		]`),
	}
	out2, err := ResponsesToChatCompletionsRequest(nonDS)
	require.NoError(t, err)
	body2, err := json.Marshal(out2)
	require.NoError(t, err)
	require.NotContains(t, string(body2), "reasoning_content", "非 thinking 上游不得注入 reasoning_content")
}

// A3: 缓存命中分支——encrypted-only reasoning 项经 ReasoningContentByID 回注真实明文。
func TestResponsesToChat_CacheHitInjectsRealPlaintext(t *testing.T) {
	req := &ResponsesRequest{
		Model: "deepseek-reasoner",
		Input: json.RawMessage(`[
			{"type":"reasoning","id":"cached_item","summary":[],"encrypted_content":"opaque"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}
		]`),
	}
	out, err := ResponsesToChatCompletionsRequestWithOptions(req, &ResponsesToChatOptions{
		ReasoningContentByID: func(id string) string {
			if id == "cached_item" {
				return "real cached thinking"
			}
			return ""
		},
	})
	require.NoError(t, err)
	var assistant *ChatMessage
	for i := range out.Messages {
		if out.Messages[i].Role == "assistant" && len(out.Messages[i].ToolCalls) == 0 {
			assistant = &out.Messages[i]
		}
	}
	require.NotNil(t, assistant)
	require.Equal(t, "real cached thinking", assistant.ReasoningContent)
	body, err := json.Marshal(out)
	require.NoError(t, err)
	require.Contains(t, string(body), `"reasoning_content":"real cached thinking"`)
}
