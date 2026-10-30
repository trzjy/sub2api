//go:build unit

package service

// 派发单 TC-02A「非流式出站：正文内联 tool call 归一」的验收测试。
//
// 背景：上游偶尔把工具调用写成**正文文本**而不是结构化委托帧 → 站点响应体
// choices[0].message 只有 content、无 tool_calls → 客户端拿不到可执行工具 →
// 该轮工具不执行 → 助手空转等用户手动「继续」。
//
// 本卡只覆盖**非流式**路径（cursorAggregateSSEToCompletion）；流式侧归 TC-02B。
//
// 覆盖矩阵：
//   - 形态 A（站点自身 blob 形态，JSON 合法）：正文内联 → 归一成结构化 tool_calls，
//     且正文里不再残留 blob 段。
//   - 形态 B（上游私有标记形态，JSON 非法）：同样归一成结构化 tool_calls。
//   - 正常正文回归：无内联形态 → content 原样、message 无 tool_calls。
//   - 结构化优先回归：既有结构化 tool_calls 又有正文内联 → 以结构化为准。
//   - 失败关闭：识别到内联形态但解析不出参数 → 返回错误（**不是**透传正文）。
//
// 标记常量按**码点转义**拼出（与 TC-01R2 同一写法）：源码里没有位置可以藏进
// 零宽字符，字节一旦写错就必然对不上（这正是 TC-01 的原始缺陷）。

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// tc02aOpen / tc02aClose / tc02aSep 是按**生产实测码点表**重建的标记字节：
//
//	开标记   = U+003C + "tool_call" + U+003E
//	闭标记   = U+003C + U+002F + "tool_call" + U+003E
//	私有分隔 = U+003C + U+005D + "minimax" + U+005B + U+003E
const (
	tc02aOpen  = "\x3ctool_call\x3e"
	tc02aClose = "\x3c/tool_call\x3e"
	tc02aSep   = "\x3c]minimax[\x3e"
)

// tc02aFormA 是形态 A 的完整片段（站点自身 blob 形态，JSON 合法）。
const tc02aFormA = tc02aOpen + "\n" +
	`{"id":"call_c3a4d6c4a7f54a5e9f5c2b8d","name":"REPL","arguments":{"code":"const out = {};"}}` +
	"\n" + tc02aClose

// tc02aFormB 是形态 B 的完整片段（上游私有标记形态，JSON 非法）。
//
// 首段 `{"id":"CallDynamicTool">]` 的引号/括号是错位的（不能 Unmarshal）；
// 字段以标签形式出现，真正承载入参的是 <code> 段。
const tc02aFormB = tc02aOpen + `{"id":"CallDynamicTool">]` + tc02aSep +
	`<arguments>]` + tc02aSep +
	`<code>const out = {};` + tc02aSep +
	`<toolName>REPL` + tc02aClose

// tc02aChunkJSON 把一个 Chat 形状增量 chunk 序列化成一个 SSE data 载荷。
//
// 用 json.Marshal 而不是手拼字符串：正文里含换行/引号/尖括号，手拼极易漏转义。
func tc02aChunkJSON(t *testing.T, id string, content *string, toolCalls []map[string]any) string {
	t.Helper()
	delta := map[string]any{}
	if content != nil {
		delta["content"] = *content
	}
	if toolCalls != nil {
		delta["tool_calls"] = toolCalls
	}
	chunk := map[string]any{
		"id":     id,
		"object": "chat.completion.chunk",
		"model":  "gpt-test",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": nil,
		}},
	}
	raw, err := json.Marshal(chunk)
	require.NoError(t, err, "chunk 序列化失败")
	return string(raw)
}

// tc02aSSE 把若干 chunk 载荷拼成 SSE 字节流。
func tc02aSSE(chunks ...string) []byte {
	var sb strings.Builder
	for _, chunk := range chunks {
		sb.WriteString("data: ")
		sb.WriteString(chunk)
		sb.WriteString("\n\n")
	}
	sb.WriteString("data: [DONE]\n\n")
	return []byte(sb.String())
}

// tc02aMessage 是聚合结果里 choices[0].message 的投影。
type tc02aMessage struct {
	Role      string           `json:"role"`
	Content   *string          `json:"content"`
	ToolCalls []tc02aToolCall `json:"tool_calls"`
}

// tc02aToolCall 是一条归一后的结构化工具调用。
type tc02aToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// tc02aAggregate 跑一次聚合并解出 message（同时保留原始 map 供键存在性断言）。
func tc02aAggregate(t *testing.T, sse []byte) (tc02aMessage, map[string]any) {
	t.Helper()
	raw, err := cursorAggregateSSEToCompletion(sse)
	require.NoError(t, err, "聚合失败")
	var out struct {
		Choices []struct {
			Message tc02aMessage `json:"message"`
		} `json:"choices"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Len(t, out.Choices, 1, "聚合结果必须恰好一条 choice")
	var messageMap map[string]any
	require.NoError(t, json.Unmarshal(raw, &messageMap))
	var choice0 map[string]any
	choices, ok := messageMap["choices"].([]any)
	require.True(t, ok)
	choice0, ok = choices[0].(map[string]any)
	require.True(t, ok)
	messageMap, ok = choice0["message"].(map[string]any)
	require.True(t, ok)
	return out.Choices[0].Message, messageMap
}

// TestCursorInlineToolCallResponseNonStreamingFormA 非流式-形态A：
// 正文里出现裸字节形态 A 的内联 tool call → 归一成结构化 tool_calls。
func TestCursorInlineToolCallResponseNonStreamingFormA(t *testing.T) {
	// 字节护栏：测试用的标记必须与生产常量逐字节一致（裸字节、无零宽字符）。
	require.Equal(t, tc02aOpen, cursorInlineToolCallOpen)
	require.Equal(t, tc02aClose, cursorInlineToolCallClose)
	require.Equal(t, tc02aSep, cursorInlineToolCallFieldSep)
	require.NotContains(t, tc02aOpen, "\u200b", "标记常量不得含零宽字符")

	content := "我先看一下这个文件。" + tc02aFormA + "稍等我确认结果。"
	sse := tc02aSSE(tc02aChunkJSON(t, "chatcmpl_tc02a_a", &content, nil))

	message, messageMap := tc02aAggregate(t, sse)

	require.Len(t, message.ToolCalls, 1, "正文内联形态 A 必须归一成一条结构化 tool_calls")
	call := message.ToolCalls[0]
	require.Equal(t, "call_c3a4d6c4a7f54a5e9f5c2b8d", call.ID)
	require.Equal(t, "function", call.Type)
	require.Equal(t, "REPL", call.Function.Name)
	require.JSONEq(t, `{"code":"const out = {};"}`, call.Function.Arguments)

	require.NotNil(t, message.Content, "有前言时正文必须保留")
	require.NotContains(t, *message.Content, tc02aOpen, "正文不得残留内联开标记")
	require.NotContains(t, *message.Content, tc02aClose, "正文不得残留内联闭标记")
	require.NotContains(t, *message.Content, "tool_call", "正文不得残留内联 blob 段")
	require.Contains(t, *message.Content, "我先看一下这个文件。")
	require.Contains(t, *message.Content, "稍等我确认结果。")

	_, hasToolCalls := messageMap["tool_calls"]
	require.True(t, hasToolCalls, "message 必须带 tool_calls 键")
}

// TestCursorInlineToolCallResponseNonStreamingFormB 非流式-形态B：
// 正文里出现裸字节形态 B（私有分隔 + 标签字段）→ 同样归一成结构化 tool_calls。
func TestCursorInlineToolCallResponseNonStreamingFormB(t *testing.T) {
	content := "先执行一下。" + tc02aFormB
	sse := tc02aSSE(tc02aChunkJSON(t, "chatcmpl_tc02a_b", &content, nil))

	message, _ := tc02aAggregate(t, sse)

	require.Len(t, message.ToolCalls, 1, "正文内联形态 B 必须归一成一条结构化 tool_calls")
	call := message.ToolCalls[0]
	require.Equal(t, "REPL", call.Function.Name)
	require.JSONEq(t, `{"code":"const out = {};"}`, call.Function.Arguments)
	require.True(t, strings.HasPrefix(call.ID, "call_inline_"),
		"形态 B 的 id 由正文派生，必须带 call_inline_ 前缀，实际=%q", call.ID)

	require.NotNil(t, message.Content)
	require.NotContains(t, *message.Content, tc02aOpen, "正文不得残留内联开标记")
	require.NotContains(t, *message.Content, "minimax", "正文不得残留形态 B 私有分隔符")
	require.Contains(t, *message.Content, "先执行一下。")
}

// TestCursorInlineToolCallResponseNonStreamingPlainContent 非流式-正常正文回归：
// 正文里没有任何内联形态 → content 原样、message 不带 tool_calls。
func TestCursorInlineToolCallResponseNonStreamingPlainContent(t *testing.T) {
	content := "这是一段普通回答，没有工具调用。"
	sse := tc02aSSE(tc02aChunkJSON(t, "chatcmpl_tc02a_plain", &content, nil))

	message, messageMap := tc02aAggregate(t, sse)

	require.NotNil(t, message.Content)
	require.Equal(t, content, *message.Content, "正常正文必须原样交付")
	require.Empty(t, message.ToolCalls)
	_, hasToolCalls := messageMap["tool_calls"]
	require.False(t, hasToolCalls, "无内联形态时 message 不得出现 tool_calls 键")
}

// TestCursorInlineToolCallResponseNonStreamingStructuredWins 非流式-结构化优先回归：
// SSE 里既有结构化 tool_calls 又有正文内联 → 以结构化那条为准。
//
// 注意正文侧的语义：结构化优先时本卡**不消费**内联片段（内联只在「无结构化
// 调用」时补位），因此正文里内联片段仍原样保留 —— 断言的是「结构化优先」，
// 而不是「内联片段被剥离」。
func TestCursorInlineToolCallResponseNonStreamingStructuredWins(t *testing.T) {
	content := "这是正文。" + tc02aFormA
	structured := []map[string]any{{
		"index": 0,
		"id":    "call_structured_1",
		"type":  "function",
		"function": map[string]any{
			"name":      "structured_tool",
			"arguments": `{"path":"a.txt"}`,
		},
	}}
	sse := tc02aSSE(tc02aChunkJSON(t, "chatcmpl_tc02a_struct", &content, structured))

	message, _ := tc02aAggregate(t, sse)

	require.Len(t, message.ToolCalls, 1, "结构化与内联并存时必须只有一条 tool_calls")
	call := message.ToolCalls[0]
	require.Equal(t, "call_structured_1", call.ID, "必须以结构化那条为准")
	require.Equal(t, "structured_tool", call.Function.Name)
	require.JSONEq(t, `{"path":"a.txt"}`, call.Function.Arguments)

	require.NotNil(t, message.Content, "结构化优先时正文仍必须交付")
	require.Contains(t, *message.Content, "这是正文。")
}

// TestCursorInlineToolCallResponseNonStreamingFailsClosed 非流式-失败关闭：
// 识别到内联形态但解析不出参数 → 返回错误，**不**把原文透传成正文。
func TestCursorInlineToolCallResponseNonStreamingFailsClosed(t *testing.T) {
	content := "我来处理。" + tc02aOpen + "这里既不是合法 JSON，也没有工具名标签。" + tc02aClose
	sse := tc02aSSE(tc02aChunkJSON(t, "chatcmpl_tc02a_bad", &content, nil))

	out, err := cursorAggregateSSEToCompletion(sse)
	require.Error(t, err, "识别到内联形态但解析不出参数必须失败关闭")
	require.True(t, errors.Is(err, ErrCursorProtocolBridgeUnavailable),
		"失败关闭的错误必须包裹 ErrCursorProtocolBridgeUnavailable，实际=%v", err)
	require.Nil(t, out, "失败关闭时不得输出任何内容（尤其不得透传正文）")
}
