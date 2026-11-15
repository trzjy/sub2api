//go:build unit

package service

import (
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestDevinForwardUsageMapping 覆盖 ModelUsageStats → ChatUsage → OpenAIUsage 的映射：
// input/output/cacheWrite/cacheRead 四方向一一对应，nil 输入返回全零。
func TestDevinForwardUsageMapping(t *testing.T) {
	stats := &ModelUsageStats{
		InputTokens:      120,
		OutputTokens:     45,
		CacheWriteTokens: 7,
		CacheReadTokens:  88,
	}
	chatUsage := devinUsageFromStats(stats)
	require.Equal(t, ChatUsage{Input: 120, Output: 45, CacheWrite: 7, CacheRead: 88}, chatUsage)

	got := DevinUsageToOpenAI(&chatUsage)
	require.Equal(t, OpenAIUsage{
		InputTokens:              120,
		OutputTokens:             45,
		CacheCreationInputTokens: 7,
		CacheReadInputTokens:     88,
	}, got)

	// nil 输入全零。
	require.Equal(t, ChatUsage{}, devinUsageFromStats(nil))
	zero := devinUsageFromStats(nil)
	require.Equal(t, OpenAIUsage{}, DevinUsageToOpenAI(&zero))
}

// TestDevinForwardChunkEnvelope 覆盖 chat.completion.chunk 包络构造：
// 固定字段、content/reasoning/tool_calls delta、finish_reason、可选 usage。
func TestDevinForwardChunkEnvelope(t *testing.T) {
	usage := &OpenAIUsage{InputTokens: 10, OutputTokens: 5}
	envelope := devinChunkEnvelope(
		"resp-1", "client-model",
		gin.H{
			"content":           "hello",
			"reasoning_content": "think",
			"tool_calls":        []gin.H{{"index": 0, "id": "call_1"}},
		},
		"tool_calls", usage,
	)

	require.Equal(t, "resp-1", envelope["id"])
	require.Equal(t, "chat.completion.chunk", envelope["object"])
	require.Equal(t, "client-model", envelope["model"])
	require.NotZero(t, envelope["created"])
	require.Same(t, usage, envelope["usage"])

	choices, ok := envelope["choices"].([]gin.H)
	require.True(t, ok)
	require.Len(t, choices, 1)
	require.Equal(t, 0, choices[0]["index"])
	require.Equal(t, "tool_calls", choices[0]["finish_reason"])

	delta, ok := choices[0]["delta"].(gin.H)
	require.True(t, ok)
	require.Equal(t, "hello", delta["content"])
	require.Equal(t, "think", delta["reasoning_content"])
	require.Contains(t, delta, "tool_calls")

	// usage 为 nil 时不写入 usage 键。
	without := devinChunkEnvelope("r", "m", gin.H{}, "", nil)
	_, hasUsage := without["usage"]
	require.False(t, hasUsage)
}

// TestDevinForwardUsageFrame 覆盖独立 usage 帧：choices 为空、携带 usage。
func TestDevinForwardUsageFrame(t *testing.T) {
	usage := &OpenAIUsage{InputTokens: 3, OutputTokens: 9}
	frame := devinUsageFrame("resp-x", "model-x", usage)
	require.Equal(t, "resp-x", frame["id"])
	require.Equal(t, "chat.completion.chunk", frame["object"])
	require.Same(t, usage, frame["usage"])
	choices, ok := frame["choices"].([]gin.H)
	require.True(t, ok)
	require.Empty(t, choices)
}

// TestDevinForwardShouldEmitUsage 覆盖三协议 usage 帧策略：include_usage 仅对 chat 生效，
// responses / anthropic 无条件回传 usage 帧供回桥填充（外审 R3 改动 3）。
func TestDevinForwardShouldEmitUsage(t *testing.T) {
	// chat：仅 include_usage=true 才写 usage 帧。
	require.True(t, devinShouldEmitUsage(webResponseModeChat, true))
	require.False(t, devinShouldEmitUsage(webResponseModeChat, false))

	// responses / anthropic：无论 include_usage 都为真。
	require.True(t, devinShouldEmitUsage(webResponseModeResponses, true))
	require.True(t, devinShouldEmitUsage(webResponseModeResponses, false))
	require.True(t, devinShouldEmitUsage(webResponseModeAnthropic, true))
	require.True(t, devinShouldEmitUsage(webResponseModeAnthropic, false))
}

// TestDevinForwardNonStreamMessage 覆盖非流式原文空白保留：content / reasoning 均为未
// Trim 原文，仅用 Trim 副本判断 reasoning 是否有实际内容（外审 R3 改动 4）。
func TestDevinForwardNonStreamMessage(t *testing.T) {
	msg := devinNonStreamMessage("  hello \n", "  think\n  ")
	require.Equal(t, "assistant", msg["role"])
	require.Equal(t, "  hello \n", msg["content"])
	require.Equal(t, "  think\n  ", msg["reasoning_content"])

	// reasoning 全为空白：不写入该键；content 原文空白依旧保留。
	blank := devinNonStreamMessage(" \tline \n", "  \n\t ")
	require.Equal(t, " \tline \n", blank["content"])
	require.NotContains(t, blank, "reasoning_content")
}

// TestDevinForwardTrailerError 覆盖 END_STREAM trailer 解析：数字/字符串 code、
// 正常 trailer、非法 JSON。
func TestDevinForwardTrailerError(t *testing.T) {
	code, msg := devinTrailerError([]byte(`{"error":{"code":"invalid_argument","message":"bad request"}}`))
	require.Equal(t, "invalid_argument", code)
	require.Equal(t, "bad request", msg)

	code, msg = devinTrailerError([]byte(`{"error":{"code":13,"message":"rate limited"}}`))
	require.Equal(t, "13", code)
	require.Equal(t, "rate limited", msg)

	// 数字 0 视为无错误。
	code, _ = devinTrailerError([]byte(`{"error":{"code":0,"message":"ok"}}`))
	require.Empty(t, code)

	// 正常 trailer（无 error 节点）。
	code, msg = devinTrailerError([]byte(`{"creditCost":5}`))
	require.Empty(t, code)
	require.Empty(t, msg)

	// 非法/空输入。
	code, msg = devinTrailerError([]byte(`not json`))
	require.Empty(t, code)
	require.Empty(t, msg)
	code, msg = devinTrailerError(nil)
	require.Empty(t, code)
	require.Empty(t, msg)
}

// TestDevinForwardReadResultFrame 覆盖数据帧/trailer 帧首字节标记。
func TestDevinForwardReadResultFrame(t *testing.T) {
	data := devinReadResultFrame([]byte("abc"), false)
	require.Equal(t, byte(0x00), data[0])
	require.Equal(t, []byte("abc"), data[1:])

	trailer := devinReadResultFrame([]byte(`{"error":{"code":"x"}}`), true)
	require.Equal(t, byte(0x01), trailer[0])
	require.Equal(t, []byte(`{"error":{"code":"x"}}`), trailer[1:])
}

// TestDevinForwardStreamingToolDelta 覆盖工具调用增量：首帧带 id/type/name，
// 后续只带 arguments，index 稳定。
func TestDevinForwardStreamingToolDelta(t *testing.T) {
	deltas := []ChatToolCallDelta{
		{ChatToolCall: ChatToolCall{ID: "call_1", Name: "get_weather", ArgumentsJSON: "{\"c"}, Index: 0},
		{ChatToolCall: ChatToolCall{ID: "call_1", ArgumentsJSON: "ity\":\"sz\"}"}, Index: 0},
	}
	sent := make(map[int]bool)
	out := devinStreamingToolDelta(deltas, sent)

	require.Len(t, out, 2)
	require.Equal(t, 0, out[0]["index"])
	require.Equal(t, "call_1", out[0]["id"])
	require.Equal(t, "function", out[0]["type"])
	firstFn := out[0]["function"].(gin.H)
	require.Equal(t, "get_weather", firstFn["name"])
	require.Equal(t, "{\"c", firstFn["arguments"])

	// 第二帧：无 id、function 无 name，仅 arguments 增量。
	require.NotContains(t, out[1], "id")
	secondFn := out[1]["function"].(gin.H)
	require.NotContains(t, secondFn, "name")
	require.Equal(t, "ity\":\"sz\"}", secondFn["arguments"])
	require.True(t, sent[0])
}

// TestDevinForwardAssembleToolCalls 覆盖非流式工具调用完整聚合（按 index 排序、
// 参数片段拼接），零参由调用方在 Finish 后补 {}（此处测聚合本身）。
func TestDevinForwardAssembleToolCalls(t *testing.T) {
	var calls []*devinAssembledToolCall
	appendDevinToolDeltas(&calls, []ChatToolCallDelta{
		{ChatToolCall: ChatToolCall{ID: "a", Name: "fn_a", ArgumentsJSON: "{\"x\":"}, Index: 0},
		{ChatToolCall: ChatToolCall{ID: "b", Name: "fn_b", ArgumentsJSON: "{}"}, Index: 1},
		{ChatToolCall: ChatToolCall{ArgumentsJSON: "1}"}, Index: 0},
	})
	require.Len(t, calls, 2)
	require.Equal(t, "a", calls[0].id)
	require.Equal(t, "fn_a", calls[0].name)
	require.Equal(t, "{\"x\":1}", calls[0].args.String())
	require.Equal(t, "b", calls[1].id)
	require.Equal(t, "{}", calls[1].args.String())
}
