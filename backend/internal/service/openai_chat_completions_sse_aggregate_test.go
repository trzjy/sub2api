package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAggregateOpenAIChatCompletionsSSE 覆盖 chat.completion.chunk SSE 的聚合：
// 正文分片、reasoning_content 分片、tool_calls 按 index 分片（首片带 id/name、
// 后续片仅追加 arguments）、usage 取最后一帧、finish_reason 取最后一个非空值。
func TestAggregateOpenAIChatCompletionsSSE(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"c1","object":"chat.completion.chunk","created":1700000000,"model":"hy3","choices":[{"index":0,"delta":{"role":"assistant","content":"","reasoning_content":"","tool_calls":[]},"finish_reason":""}],"usage":null}`,
		`data: {"id":"c1","object":"chat.completion.chunk","model":"hy3","choices":[{"index":0,"delta":{"content":"Hel","reasoning_content":"think "},"finish_reason":""}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","model":"hy3","choices":[{"index":0,"delta":{"content":"lo","reasoning_content":"more"},"finish_reason":""}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","model":"hy3","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":""}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","model":"hy3","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"","arguments":"{\"location\":\"Paris\"}"}}]},"finish_reason":""}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","model":"hy3","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		`data: [DONE]`,
		``,
	}, "\n")

	out, ok := aggregateOpenAIChatCompletionsSSE([]byte(sse))
	require.True(t, ok)

	var got map[string]any
	require.NoError(t, json.Unmarshal(out, &got))
	require.Equal(t, "chat.completion", got["object"])
	require.Equal(t, "c1", got["id"])
	require.Equal(t, "hy3", got["model"])
	require.Equal(t, float64(1700000000), got["created"])

	choice := got["choices"].([]any)[0].(map[string]any)
	require.Equal(t, "tool_calls", choice["finish_reason"])
	message := choice["message"].(map[string]any)
	require.Equal(t, "Hello", message["content"])
	require.Equal(t, "think more", message["reasoning_content"])

	calls := message["tool_calls"].([]any)
	require.Len(t, calls, 1)
	call := calls[0].(map[string]any)
	require.Equal(t, "call_1", call["id"])
	require.Equal(t, "function", call["type"])
	fn := call["function"].(map[string]any)
	require.Equal(t, "get_weather", fn["name"])
	require.Equal(t, `{"location":"Paris"}`, fn["arguments"])

	require.Equal(t, float64(15), got["usage"].(map[string]any)["total_tokens"])
}

// TestAggregateOpenAIChatCompletionsSSE_RejectsOtherShapes 验证不越界接管：
// Codex/Responses 形状 SSE 与非 SSE 都必须返回 false，继续由既有路径处理。
func TestAggregateOpenAIChatCompletionsSSE_RejectsOtherShapes(t *testing.T) {
	_, ok := aggregateOpenAIChatCompletionsSSE([]byte("event: ping\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n"))
	require.False(t, ok, "Codex/Responses 形状不应被本聚合器接管")

	_, ok = aggregateOpenAIChatCompletionsSSE([]byte(`{"object":"chat.completion","choices":[]}`))
	require.False(t, ok, "非 SSE 不应被本聚合器接管")

	// 多候选（n>1）无法安全合并为单条 message，应放弃接管。
	multi := `data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"a"}},{"index":1,"delta":{"content":"b"}}]}` + "\n\n"
	_, ok = aggregateOpenAIChatCompletionsSSE([]byte(multi))
	require.False(t, ok, "多候选流不应被本聚合器接管")
}
