//go:build unit

package service

// E6 卡的响应解析单测：envelope 帧流 → OpenAI SSE / chat.completion。
//
// 关键断言：
//   - EndStream 帧**强制**（缺失即协议错误）
//   - `.text` 按 cursorTextIsDeltaAssumed 累加（假设本身在代码里已显式标注未验证）
//   - 首版范围外帧被丢弃但不算错误
//   - 绝不产出 usage（禁止伪造）

import (
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// cursorTestFrame 构造一个消息帧（flags=0）。
func cursorTestFrame(payload []byte) []byte { return cursorEncodeEnvelope(payload) }

// cursorTestEndStream 构造一个 EndStream 帧（flags=2）。
func cursorTestEndStream(t *testing.T, jsonPayload string) []byte {
	t.Helper()
	p := []byte(jsonPayload)
	frame := make([]byte, cursorEnvelopeHeaderSize+len(p))
	frame[0] = cursorEnvelopeFlagEndStream
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(p)))
	copy(frame[5:], p)
	return frame
}

// cursorTextFrame 构造一个 stream_unified_chat_response{1 text} 帧。
func cursorTextFrame(t *testing.T, text string) []byte {
	t.Helper()
	inner := cursorAppendStringField(nil, cursorFldRespText, text)
	outer := cursorAppendBytesField(nil, cursorFldStreamUnifiedChatResponse, inner)
	return cursorTestFrame(outer)
}

// cursorStreamStartFrame 构造一个 stream_start{} 帧。
func cursorStreamStartFrame(t *testing.T) []byte {
	t.Helper()
	return cursorTestFrame(cursorAppendBytesField(nil, cursorFldStreamStart, []byte{}))
}

// cursorToolCallFrame 构造一个 client_side_tool_v2_call 帧（首版应丢弃）。
func cursorToolCallFrame(t *testing.T, name string) []byte {
	t.Helper()
	inner := cursorAppendStringField(nil, 9, name) // 9 = name
	outer := cursorAppendBytesField(nil, cursorFldClientSideToolV2Call, inner)
	return cursorTestFrame(outer)
}

// sseJoined 把 SSE 行拼回一段文本便于断言。
func sseJoined(lines []string) string { return strings.Join(lines, "") }

// TestCursorParseStreamTextDelta 覆盖增量累加（cursorTextIsDeltaAssumed=true 的行为）。
func TestCursorParseStreamTextDelta(t *testing.T) {
	var stream []byte
	stream = append(stream, cursorStreamStartFrame(t)...)
	stream = append(stream, cursorTextFrame(t, "1")...)
	stream = append(stream, cursorTextFrame(t, " 2")...)
	stream = append(stream, cursorTextFrame(t, " 3")...)
	stream = append(stream, cursorTestEndStream(t, `{"metadata":{}}`)...)

	p := cursorNewStreamParser("gpt-4o")
	lines, err := p.cursorParseStream(stream)
	require.NoError(t, err)

	if cursorTextIsDeltaAssumed {
		require.Equal(t, "1 2 3", p.cursorContent(), "deltas must accumulate")
	} else {
		require.Equal(t, " 3", p.cursorContent(), "full text must overwrite")
	}

	out := sseJoined(lines)
	// 首包必须带 role（否则部分 OpenAI 客户端不会初始化流）。
	require.Contains(t, out, `"role":"assistant"`)
	require.Contains(t, out, `"content":"1"`)
	require.Contains(t, out, `"finish_reason":"stop"`)
	require.Contains(t, out, `"model":"gpt-4o"`)
	// 禁止伪造 usage。
	require.NotContains(t, out, `"usage"`)
}

// TestCursorParseStreamRequiresEndStream 钉死 EndStream 帧的强制性。
func TestCursorParseStreamRequiresEndStream(t *testing.T) {
	t.Run("缺 EndStream 报错", func(t *testing.T) {
		var stream []byte
		stream = append(stream, cursorTextFrame(t, "hello")...)
		// 无 EndStream —— 客户端同样视为 `protocol error: missing EndStreamResponse`。
		_, err := cursorNewStreamParser("m").cursorParseStream(stream)
		require.ErrorIs(t, err, ErrCursorMissingEndStream)
	})

	t.Run("空流缺 EndStream 也报错", func(t *testing.T) {
		_, err := cursorNewStreamParser("m").cursorParseStream(nil)
		require.ErrorIs(t, err, ErrCursorMissingEndStream)
	})

	t.Run("第二个 EndStream 报错", func(t *testing.T) {
		var stream []byte
		stream = append(stream, cursorTestEndStream(t, `{}`)...)
		stream = append(stream, cursorTestEndStream(t, `{}`)...)
		_, err := cursorNewStreamParser("m").cursorParseStream(stream)
		require.ErrorIs(t, err, ErrCursorProtocol)
	})

	t.Run("EndStream 之后还有消息帧报错", func(t *testing.T) {
		var stream []byte
		stream = append(stream, cursorTestEndStream(t, `{}`)...)
		stream = append(stream, cursorTextFrame(t, "late")...)
		_, err := cursorNewStreamParser("m").cursorParseStream(stream)
		require.ErrorIs(t, err, ErrCursorProtocol)
	})
}

// TestCursorParseStreamEndStreamError 覆盖流内终态错误的两种错误形状。
func TestCursorParseStreamEndStreamError(t *testing.T) {
	t.Run("Connect 标准业务错误", func(t *testing.T) {
		payload := `{"error":{"code":"unauthenticated","message":"User is unauthorized",` +
			`"details":[{"type":"aiserver.v1.ErrorDetails","debug":{"error":"ERROR_UNAUTHORIZED",` +
			`"details":{"title":"Unauthorized request.","detail":"User is unauthorized",` +
			`"isRetryable":false}}}]}}`
		var stream []byte
		stream = append(stream, cursorTestEndStream(t, payload)...)
		_, err := cursorNewStreamParser("m").cursorParseStream(stream)
		require.ErrorIs(t, err, ErrCursorUpstreamStream)
		require.Contains(t, err.Error(), "ERROR_UNAUTHORIZED")
		require.Contains(t, err.Error(), "User is unauthorized")
	})

	t.Run("中转自有错误形状", func(t *testing.T) {
		// 中转在鉴权失败时返回 `{"error":…}` 而非 `{code,message}`，
		// 两种都要能识别，否则会误判为协议损坏。
		code, msg := cursorExtractUpstreamErrorSummary(
			[]byte(`{"error":"Unauthorized","message":"鉴权失败: device_id_list 为空"}`))
		require.Equal(t, "Unauthorized", code)
		require.Contains(t, msg, "device_id_list")

		var stream []byte
		stream = append(stream, cursorTestEndStream(t,
			`{"error":{"code":"unauthenticated","message":"鉴权失败: device_id_list 为空"}}`)...)
		_, err := cursorNewStreamParser("m").cursorParseStream(stream)
		require.ErrorIs(t, err, ErrCursorUpstreamStream)
		require.Contains(t, err.Error(), "device_id_list")
	})

	t.Run("EndStream 非 JSON 视为协议错误", func(t *testing.T) {
		var stream []byte
		stream = append(stream, cursorTestEndStream(t, "not-json")...)
		_, err := cursorNewStreamParser("m").cursorParseStream(stream)
		require.ErrorIs(t, err, ErrCursorProtocol)
	})
}

// TestCursorParseStreamDropsOutOfScopeFrames 覆盖首版范围外帧被丢弃但不算错误。
func TestCursorParseStreamDropsOutOfScopeFrames(t *testing.T) {
	var stream []byte
	stream = append(stream, cursorToolCallFrame(t, "ReadFile")...)
	stream = append(stream, cursorTestFrame(
		cursorAppendBytesField(nil, cursorFldConversationSummary, cursorAppendStringField(nil, 1, "summary")))...)
	stream = append(stream, cursorTestFrame(
		cursorAppendBytesField(nil, cursorFldUserRules, cursorAppendStringField(nil, 1, "rules")))...)
	stream = append(stream, cursorTextFrame(t, "visible")...)
	stream = append(stream, cursorTestEndStream(t, `{}`)...)

	p := cursorNewStreamParser("m")
	lines, err := p.cursorParseStream(stream)
	require.NoError(t, err, "out-of-scope frames must be skipped, not fatal")
	out := sseJoined(lines)
	require.Contains(t, out, "visible")
	require.NotContains(t, out, "ReadFile")
	require.NotContains(t, out, "summary")
	require.NotContains(t, out, "rules")
}

// TestCursorParseStreamOnlyStartFrame 覆盖只有开场帧的流（合法但内容为空）。
func TestCursorParseStreamOnlyStartFrame(t *testing.T) {
	var stream []byte
	stream = append(stream, cursorStreamStartFrame(t)...)
	stream = append(stream, cursorTestEndStream(t, `{}`)...)

	p := cursorNewStreamParser("m")
	lines, err := p.cursorParseStream(stream)
	require.NoError(t, err)
	out := sseJoined(lines)
	require.Contains(t, out, `"role":"assistant"`)
	require.Contains(t, out, `"finish_reason":"stop"`)
}

// TestCursorParseStreamToSSE 覆盖完整 SSE 输出（含 [DONE] 哨兵）。
func TestCursorParseStreamToSSE(t *testing.T) {
	var stream []byte
	stream = append(stream, cursorStreamStartFrame(t)...)
	stream = append(stream, cursorTextFrame(t, `say "hi"`+"\n")...)
	stream = append(stream, cursorTestEndStream(t, `{}`)...)

	out, err := cursorParseStreamToSSE(stream, "gpt-4o")
	require.NoError(t, err)
	s := string(out)
	require.True(t, strings.HasSuffix(s, cursorSSEDoneSentinel), "must end with [DONE]")
	require.Equal(t, 1, strings.Count(s, cursorSSEDoneSentinel), "exactly one [DONE]")

	// 每一行 data: 必须是合法 JSON（否则客户端直接断流）。
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var chunk map[string]any
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk),
			"chunk must be valid JSON: %s", line)
		require.Equal(t, "chat.completion.chunk", chunk["object"])
	}
}

// TestCursorParseStreamToCompletion 覆盖非流式合成（含转义正确性）。
func TestCursorParseStreamToCompletion(t *testing.T) {
	var stream []byte
	stream = append(stream, cursorTextFrame(t, `He said "hi"`+"\n")...)
	stream = append(stream, cursorTextFrame(t, ` again`)...)
	stream = append(stream, cursorTestEndStream(t, `{}`)...)

	out, err := cursorParseStreamToCompletion(stream, "gpt-4o")
	require.NoError(t, err)

	var completion map[string]any
	require.NoError(t, json.Unmarshal(out, &completion))
	require.Equal(t, "chat.completion", completion["object"])
	require.Equal(t, "gpt-4o", completion["model"])
	// usage 必须是**显式零值**（非编造），且总 token 为 0。
	require.Contains(t, string(out), `"usage"`)
	require.Contains(t, string(out), `"total_tokens":0`,
		"usage must be explicit zeros, never invented counts")

	choices, ok := completion["choices"].([]any)
	require.True(t, ok)
	require.Len(t, choices, 1)
	choice := choices[0].(map[string]any)
	require.Equal(t, "stop", choice["finish_reason"])
	msg := choice["message"].(map[string]any)
	require.Equal(t, "assistant", msg["role"])
	require.Equal(t, `He said "hi"`+"\n"+` again`, msg["content"])
}

// TestCursorParseStreamFailsClosedOnTruncatedFrame 覆盖截断帧失败关闭。
func TestCursorParseStreamFailsClosedOnTruncatedFrame(t *testing.T) {
	var stream []byte
	stream = append(stream, cursorTextFrame(t, "hello")...)
	stream = append(stream, 0x00, 0x00, 0x10, 0x00, 0x41, 0x42) // 声明 4096 字节但只给 2

	_, err := cursorNewStreamParser("m").cursorParseStream(stream)
	require.ErrorIs(t, err, ErrCursorProtocol)
}

// TestCursorUpstreamStatusErrorShape 覆盖上游状态错误的分类。
func TestCursorUpstreamStatusErrorShape(t *testing.T) {
	t.Run("401 归为 relay auth rejected", func(t *testing.T) {
		err := cursorUpstreamStatusError(401,
			[]byte(`{"error":"Unauthorized","message":"鉴权失败: quota_key 已失效"}`))
		require.ErrorIs(t, err, ErrCursorRelayAuthRejected)
		require.Contains(t, err.Error(), "quota_key 已失效")
	})

	t.Run("非 JSON 体只报长度不回显内容", func(t *testing.T) {
		err := cursorUpstreamStatusError(500, []byte{0x01, 0x02, 0x03})
		require.Error(t, err)
		require.Contains(t, err.Error(), "non-JSON body")
		require.NotContains(t, err.Error(), "\x01")
	})

	t.Run("空体只报状态码", func(t *testing.T) {
		err := cursorUpstreamStatusError(503, nil)
		require.ErrorIs(t, err, ErrCursorRelayAuthRejected)
		require.Contains(t, err.Error(), "503")
	})
}
