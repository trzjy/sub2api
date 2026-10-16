//go:build unit

package service

// E6 卡的请求转换单测：OpenAI → Cursor 原生 protobuf。
//
// 断言以**字节级回读**为主（不只断言"不报错"），确保字段号与嵌套层级真的写对了
// ——否则单测会在实现整体写错字段号时仍然全绿。

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

// cursorDecodeRequest 拆出 StreamUnifiedChatRequestWithTools 的结构，
// 返回 (外层字段, StreamUnifiedChatRequest 字段)。
func cursorDecodeRequest(t *testing.T, body []byte) ([]cursorPBField, []cursorPBField) {
	t.Helper()
	require.Equal(t, byte(0), body[0], "envelope flags must be 0")

	env := cursorEnvelopeFrame{Flags: body[0]}
	_ = env
	outer, err := cursorDecodeProtobuf(body[cursorEnvelopeHeaderSize:])
	require.NoError(t, err)

	chatReq, ok := cursorFindField(outer, cursorFldStreamUnifiedChatRequest)
	require.True(t, ok, "outer must carry field 1 (stream_unified_chat_request)")
	inner, err := cursorDecodeProtobuf(chatReq.Bytes)
	require.NoError(t, err)
	return outer, inner
}

// cursorConversationsOf 取出内层的 conversation[] 列表（已解码）。
func cursorConversationsOf(t *testing.T, inner []cursorPBField) [][2]string {
	t.Helper()
	var out [][2]string
	for _, f := range inner {
		if f.Number != cursorFldConversation {
			continue
		}
		fields, err := cursorDecodeProtobuf(f.Bytes)
		require.NoError(t, err)
		text, _ := cursorFindField(fields, cursorFldConvText)
		typ, ok := cursorFindField(fields, cursorFldConvType)
		require.True(t, ok, "conversation message must carry type")
		out = append(out, [2]string{text.String(), string(rune(typ.Varint))})
	}
	return out
}

// cursorModelNameOf 取出 model_details.model_name。
func cursorModelNameOf(t *testing.T, inner []cursorPBField) string {
	t.Helper()
	md, ok := cursorFindField(inner, cursorFldModelDetails)
	require.True(t, ok, "inner must carry field 5 (model_details) — B-1: it is 5, not 3")
	fields, err := cursorDecodeProtobuf(md.Bytes)
	require.NoError(t, err)
	name, ok := cursorFindField(fields, cursorFldModelName)
	require.True(t, ok)
	return name.String()
}

// TestCursorConvertRequestBasic 覆盖基本映射：messages → conversation、model → model_details。
func TestCursorConvertRequestBasic(t *testing.T) {
	req := &apicompat.ChatCompletionsRequest{
		Model: "gpt-4o",
		Messages: []apicompat.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"what is 2+2?"`)},
			{Role: "assistant", Content: json.RawMessage(`"4"`)},
			{Role: "user", Content: json.RawMessage(`"and 3+3?"`)},
		},
	}
	body, err := cursorConvertRequest(req, "grok-4.7")
	require.NoError(t, err)

	_, inner := cursorDecodeRequest(t, body)
	require.Equal(t, "grok-4.7", cursorModelNameOf(t, inner), "model_name must carry the f36 wire ID")

	isChat, ok := cursorFindField(inner, cursorFldIsChat)
	require.True(t, ok, "is_chat (field 22) must be set")
	require.Equal(t, uint64(1), isChat.Varint)

	mode, ok := cursorFindField(inner, cursorFldUnifiedMode)
	require.True(t, ok, "unified_mode (field 46) must be set to CHAT")
	require.Equal(t, uint64(cursorUnifiedModeChat), mode.Varint)

	convs := cursorConversationsOf(t, inner)
	require.Len(t, convs, 3)
	require.Equal(t, "what is 2+2?", convs[0][0])
	require.Equal(t, string(rune(cursorMsgTypeHuman)), convs[0][1])
	require.Equal(t, "4", convs[1][0])
	require.Equal(t, string(rune(cursorMsgTypeAI)), convs[1][1])
	require.Equal(t, "and 3+3?", convs[2][0])
	require.Equal(t, string(rune(cursorMsgTypeHuman)), convs[2][1])
}

// TestCursorConvertRequestSystemMerge 覆盖 system 合并策略（首版裁定：并入首个 HUMAN）。
//
// 这是本卡最需要被钉死的行为：system 文本**绝不能丢**（丢 = 静默改变语义）。
func TestCursorConvertRequestSystemMerge(t *testing.T) {
	t.Run("单条 system 前置合并进首条 user", func(t *testing.T) {
		req := &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{
				{Role: "system", Content: json.RawMessage(`"You are terse."`)},
				{Role: "user", Content: json.RawMessage(`"hi"`)},
			},
		}
		body, err := cursorConvertRequest(req, "grok-4.7")
		require.NoError(t, err)
		_, inner := cursorDecodeRequest(t, body)
		convs := cursorConversationsOf(t, inner)
		require.Len(t, convs, 1, "system must not become its own message (MessageType has no system)")
		require.Contains(t, convs[0][0], "You are terse.")
		require.Contains(t, convs[0][0], "hi")
	})

	t.Run("多条 system 保序合并并加边界标记", func(t *testing.T) {
		req := &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{
				{Role: "system", Content: json.RawMessage(`"FIRST"`)},
				{Role: "system", Content: json.RawMessage(`"SECOND"`)},
				{Role: "user", Content: json.RawMessage(`"q"`)},
			},
		}
		body, err := cursorConvertRequest(req, "grok-4.7")
		require.NoError(t, err)
		_, inner := cursorDecodeRequest(t, body)
		convs := cursorConversationsOf(t, inner)
		require.Len(t, convs, 1)
		text := convs[0][0]
		require.Less(t, indexOfSub(text, "FIRST"), indexOfSub(text, "SECOND"), "order must be preserved")
		require.Contains(t, text, "<system>", "multi-part system must carry explicit boundaries")
	})

	t.Run("developer 角色等同 system", func(t *testing.T) {
		req := &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{
				{Role: "developer", Content: json.RawMessage(`"DEV"`)},
				{Role: "user", Content: json.RawMessage(`"q"`)},
			},
		}
		body, err := cursorConvertRequest(req, "grok-4.7")
		require.NoError(t, err)
		_, inner := cursorDecodeRequest(t, body)
		convs := cursorConversationsOf(t, inner)
		require.Len(t, convs, 1)
		require.Contains(t, convs[0][0], "DEV")
	})

	t.Run("尾部 system 新建 HUMAN 承载而非丢弃", func(t *testing.T) {
		req := &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{
				{Role: "user", Content: json.RawMessage(`"q"`)},
				{Role: "assistant", Content: json.RawMessage(`"a"`)},
				{Role: "system", Content: json.RawMessage(`"TAIL"`)},
			},
		}
		body, err := cursorConvertRequest(req, "grok-4.7")
		require.NoError(t, err)
		_, inner := cursorDecodeRequest(t, body)
		convs := cursorConversationsOf(t, inner)
		require.Len(t, convs, 3)
		require.Equal(t, "q", convs[0][0], "existing messages must keep their order")
		require.Equal(t, "a", convs[1][0])
		require.Contains(t, convs[2][0], "TAIL", "trailing system must not be dropped")
	})
}

// indexOfSub 是 strings.Index 的本地副本（避免为一个断言引入 import）。
func indexOfSub(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestCursorConvertRequestDropsOutOfScopeFields 覆盖首版范围外字段被丢弃。
func TestCursorConvertRequestDropsOutOfScopeFields(t *testing.T) {
	t.Run("请求侧 tools 被丢弃并关闭服务端工具开关", func(t *testing.T) {
		req := &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
			Tools: []apicompat.ChatTool{{
				Type:     "function",
				Function: &apicompat.ChatFunction{Name: "get_weather"},
			}},
		}
		body, err := cursorConvertRequest(req, "grok-4.7")
		require.NoError(t, err)
		_, inner := cursorDecodeRequest(t, body)

		disable, ok := cursorFindField(inner, cursorFldShouldDisableTools)
		require.True(t, ok, "tools present ⇒ should_disable_tools must be set (else upstream may emit tool calls)")
		require.Equal(t, uint64(1), disable.Varint)

		// 不得把工具名写进任何字段。
		require.NotContains(t, string(body), "get_weather", "tools must be dropped, not translated")
	})

	t.Run("无 tools 时不写 should_disable_tools", func(t *testing.T) {
		req := &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		}
		body, err := cursorConvertRequest(req, "grok-4.7")
		require.NoError(t, err)
		_, inner := cursorDecodeRequest(t, body)
		_, ok := cursorFindField(inner, cursorFldShouldDisableTools)
		require.False(t, ok)
	})

	t.Run("tool 结果消息被丢弃而非报错", func(t *testing.T) {
		req := &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{
				{Role: "user", Content: json.RawMessage(`"hi"`)},
				{Role: "tool", ToolCallID: "call_1", Content: json.RawMessage(`"result"`)},
				{Role: "assistant", Content: json.RawMessage(`"done"`)},
			},
		}
		body, err := cursorConvertRequest(req, "grok-4.7")
		require.NoError(t, err)
		_, inner := cursorDecodeRequest(t, body)
		convs := cursorConversationsOf(t, inner)
		require.Len(t, convs, 2)
		require.NotContains(t, string(body), "result")
	})
}

// TestCursorConvertRequestFailsClosed 覆盖首版范围外形状一律失败关闭。
func TestCursorConvertRequestFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		req  *apicompat.ChatCompletionsRequest
	}{
		{"nil request", nil},
		{"no messages", &apicompat.ChatCompletionsRequest{}},
		{"unknown role", &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{{Role: "wizard", Content: json.RawMessage(`"hi"`)}}}},
		{"missing role", &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{{Content: json.RawMessage(`"hi"`)}}}},
		{"image content part", &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(
				`[{"type":"image_url","image_url":{"url":"http://x/y.png"}}]`)}}}},
		{"numeric content", &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`42`)}}}},
		{"object content", &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`{"a":1}`)}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := cursorConvertRequest(c.req, "grok-4.7")
			require.ErrorIs(t, err, ErrCursorUnsupportedRequest)
		})
	}
}

// TestCursorConvertRequestTextPartsAccepted 覆盖纯文本 content-parts 数组降级为字符串。
func TestCursorConvertRequestTextPartsAccepted(t *testing.T) {
	req := &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(
			`[{"type":"text","text":"part one "},{"type":"text","text":"part two"}]`)}},
	}
	body, err := cursorConvertRequest(req, "grok-4.7")
	require.NoError(t, err)
	_, inner := cursorDecodeRequest(t, body)
	convs := cursorConversationsOf(t, inner)
	require.Len(t, convs, 1)
	require.Equal(t, "part one part two", convs[0][0])
}

// TestCursorConvertRequestModelGateRejectsF1 钉死 f1 展示名不被静默改写：
// 调用方必须显式传 f36，本层不做任何"猜测式"模型名修正。
func TestCursorConvertRequestModelGateRejectsF1(t *testing.T) {
	req := &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	// 传入 f1 展示名：原样编码（上游会拒 ERROR_BAD_MODEL_NAME，那是**预期行为**，
	// 本层的职责是不篡改而不是替调用方猜 wire ID）。
	body, err := cursorConvertRequest(req, "gpt-5.5")
	require.NoError(t, err)
	_, inner := cursorDecodeRequest(t, body)
	require.Equal(t, "gpt-5.5", cursorModelNameOf(t, inner),
		"conversion must not rewrite the model name")

	// 空模型名则失败关闭（不得发出 model_details 缺失的请求）。
	_, err = cursorConvertRequest(req, "   ")
	require.ErrorIs(t, err, ErrCursorUnsupportedRequest)
}
