package apicompat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// B (Done when #1c, 透传部分): Responses→Chat 方向，function_call 携带的
// thought_signature 必须贯通到 ChatMessage.ToolCalls[].ThoughtSignature，并随后在
// Chat→Responses 出站 JSON 里原样回写 thought_signature 键与值。
func TestThoughtSignature_PassthroughResponsesToChatToResponses(t *testing.T) {
	// 1) Responses→Chat：输入 function_call 带 thought_signature。
	req := &ResponsesRequest{
		Model: "kira-gemini",
		Input: json.RawMessage(`[
			{"type":"function_call","call_id":"call_1","name":"search","arguments":"{}","thought_signature":"sig_kira_123"},
			{"type":"function_call_output","call_id":"call_1","output":"ok"}
		]`),
	}
	out, err := ResponsesToChatCompletionsRequest(req)
	require.NoError(t, err)
	require.Len(t, out.Messages, 2)
	require.NotEmpty(t, out.Messages[0].ToolCalls)
	require.Equal(t, "sig_kira_123", out.Messages[0].ToolCalls[0].ThoughtSignature,
		"Responses→Chat 必须捕获 thought_signature")

	// 2) Chat→Responses：出站 JSON 原样回写 thought_signature。
	chatResp := &ChatCompletionsResponse{
		ID:      "cc_1",
		Model:   "kira-gemini",
		Choices: []ChatChoice{{Message: out.Messages[0], FinishReason: "tool_calls"}},
	}
	responsesResp := ChatCompletionsResponseToResponses(chatResp, "kira-gemini", nil, nil, false, nil)
	require.NotEmpty(t, responsesResp.Output)
	var fc *ResponsesOutput
	for i := range responsesResp.Output {
		if responsesResp.Output[i].Type == "function_call" {
			fc = &responsesResp.Output[i]
			break
		}
	}
	require.NotNil(t, fc, "应存在 function_call 项")
	require.Equal(t, "sig_kira_123", fc.ThoughtSignature, "Chat→Responses 必须保留 thought_signature")

	body, err := json.Marshal(responsesResp)
	require.NoError(t, err)
	require.Contains(t, string(body), `"thought_signature":"sig_kira_123"`,
		"出站 Responses JSON 必须含 thought_signature 原值")
}

// B 回归：非 Kira/Gemini 路径的 function_call 不含 thought_signature，出站字节零变化。
func TestThoughtSignature_NonKiraOmitted(t *testing.T) {
	req := &ResponsesRequest{
		Model: "gpt-4o",
		Input: json.RawMessage(`[
			{"type":"function_call","call_id":"call_1","name":"search","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"ok"}
		]`),
	}
	out, err := ResponsesToChatCompletionsRequest(req)
	require.NoError(t, err)
	require.Empty(t, out.Messages[0].ToolCalls[0].ThoughtSignature)

	chatResp := &ChatCompletionsResponse{
		ID:      "cc_2",
		Model:   "gpt-4o",
		Choices: []ChatChoice{{Message: out.Messages[0], FinishReason: "tool_calls"}},
	}
	responsesResp := ChatCompletionsResponseToResponses(chatResp, "gpt-4o", nil, nil, false, nil)
	body, err := json.Marshal(responsesResp)
	require.NoError(t, err)
	require.NotContains(t, string(body), "thought_signature", "非 Kira 路径不得产出 thought_signature 键")
}

// B 回归：ChatToolCall / ChatDelta 带 thought_signature 时序列化为 thought_signature 键。
func TestThoughtSignature_FieldRoundTrips(t *testing.T) {
	tc := ChatToolCall{ID: "c1", Type: "function", Function: ChatFunctionCall{Name: "f", Arguments: "{}"}, ThoughtSignature: "ts_1"}
	b, err := json.Marshal(tc)
	require.NoError(t, err)
	require.True(t, strings.Contains(string(b), `"thought_signature":"ts_1"`))

	var back ChatToolCall
	require.NoError(t, json.Unmarshal(b, &back))
	require.Equal(t, "ts_1", back.ThoughtSignature)

	// 缺省时 omitempty 省略。
	tc2 := ChatToolCall{ID: "c2", Type: "function", Function: ChatFunctionCall{Name: "f", Arguments: "{}"}}
	b2, err := json.Marshal(tc2)
	require.NoError(t, err)
	require.NotContains(t, string(b2), "thought_signature")
}
