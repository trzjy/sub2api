//go:build unit

package service

// P1-A1 卡的响应解析单测：agent.v1.AgentServerMessage 帧流 → OpenAI SSE /
// chat.completion。
//
// 关键断言：
//   - 增量文本按 TextDeltaUpdate 累加（证据 32：Gyt 名为 text_**delta**_update）
//   - usage **只**来自 turn_ended（证据 40/41 字段号），拿不到就显式零值
//   - 流内固定拒付模板 → ErrCursorKeyRejectedUpstream 哨兵（证据 53/125/134），
//     绝不当成功、绝不产 usage
//   - 取消/截断（无终态标记）失败关闭，绝不猜 usage
//   - 首版范围外帧（exec / kv / checkpoint / 计时）丢弃但不算错误

import (
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// cursorAgentFrame 构造一个消息帧（flags=0）。
func cursorAgentFrame(payload []byte) []byte { return cursorEncodeEnvelope(payload) }

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

// cursorTextDeltaFrame 构造 interaction_update{1 text_delta{1 text}} 帧。
func cursorTextDeltaFrame(t *testing.T, text string) []byte {
	t.Helper()
	delta := cursorAppendStringField(nil, cursorFldTextDeltaText, text)
	update := cursorAppendBytesField(nil, cursorFldUpdateTextDelta, delta)
	return cursorAgentFrame(cursorAppendBytesField(nil, cursorFldServerInteractionUpdate, update))
}

// cursorTurnEndedFrame 构造 interaction_update{14 turn_ended{…}} 帧。
// pairs 是 (字段号, 值) 序列，用于精确复刻活体样本（41-turn-ended-decoded.json）。
func cursorTurnEndedFrame(t *testing.T, pairs ...[2]uint64) []byte {
	t.Helper()
	inner := make([]byte, 0, 8*len(pairs))
	for _, p := range pairs {
		inner = cursorAppendVarintField(inner, int(p[0]), p[1])
	}
	update := cursorAppendBytesField(nil, cursorFldUpdateTurnEnded, inner)
	return cursorAgentFrame(cursorAppendBytesField(nil, cursorFldServerInteractionUpdate, update))
}

// cursorServerSkipFrame 构造一个"首版范围外"的服务端帧（exec / kv / checkpoint / 计时）。
func cursorServerSkipFrame(t *testing.T, field int, payload []byte) []byte {
	t.Helper()
	return cursorAgentFrame(cursorAppendBytesField(nil, field, payload))
}

// sseJoined 把 SSE 行拼回一段文本便于断言。
func sseJoined(lines []string) string { return strings.Join(lines, "") }

// TestCursorParseStreamTextDeltaAccumulates 覆盖增量累加与范围外帧丢弃。
func TestCursorProtocolResponseTextDeltaAccumulates(t *testing.T) {
	var stream []byte
	// 复刻活体事件序列（33 号样本）的前半段：exec_request → kv_set → 文本。
	stream = append(stream, cursorServerSkipFrame(t, cursorFldServerExecMessage,
		cursorAppendVarintField(nil, 1, 0))...)
	stream = append(stream, cursorServerSkipFrame(t, cursorFldServerKvMessage, []byte{0x01, 0x02})...)
	stream = append(stream, cursorTextDeltaFrame(t, "1")...)
	stream = append(stream, cursorTextDeltaFrame(t, " 2")...)
	stream = append(stream, cursorTextDeltaFrame(t, " 3")...)
	stream = append(stream, cursorServerSkipFrame(t, cursorFldServerTTFTBreakdown, []byte{0x00})...)
	stream = append(stream, cursorTurnEndedFrame(t,
		[2]uint64{cursorFldTurnEndedInputTokens, 12601},
		[2]uint64{cursorFldTurnEndedOutputTokens, 15},
		[2]uint64{cursorFldTurnEndedCacheReadTokens, 128},
		[2]uint64{cursorFldTurnEndedCacheWriteTokens, 0},
		[2]uint64{cursorFldTurnEndedReasoningTokens, 11},
		[2]uint64{cursorFldTurnEndedEndedAtMS, 1792309998331})...)

	p := cursorNewStreamParser("gpt-4o")
	lines, err := p.cursorParseStream(stream)
	require.NoError(t, err)
	require.Equal(t, "1 2 3", p.cursorContent(), "text deltas must accumulate")

	out := sseJoined(lines)
	require.Contains(t, out, `"role":"assistant"`)
	require.Contains(t, out, `"content":"1"`)
	require.Contains(t, out, `"finish_reason":"stop"`)
	require.Contains(t, out, `"model":"gpt-4o"`)
	// 范围外帧的内容绝不出现在回写给客户端的流里。
	require.NotContains(t, out, "kv")
}

// TestCursorParseStreamTurnEndedUsage 钉死 usage 的唯一来源与字段映射。
func TestCursorProtocolResponseTurnEndedUsage(t *testing.T) {
	var stream []byte
	stream = append(stream, cursorTextDeltaFrame(t, "OK")...)
	stream = append(stream, cursorTurnEndedFrame(t,
		[2]uint64{cursorFldTurnEndedInputTokens, 12601},
		[2]uint64{cursorFldTurnEndedOutputTokens, 15},
		[2]uint64{cursorFldTurnEndedCacheReadTokens, 128},
		[2]uint64{cursorFldTurnEndedCacheWriteTokens, 64},
		[2]uint64{cursorFldTurnEndedReasoningTokens, 11})...)

	p := cursorNewStreamParser("m")
	lines, err := p.cursorParseStream(stream)
	require.NoError(t, err)

	// 计费口径：reasoning 不并入 output（是否重叠未取证，并入即可能重复计费）。
	require.Equal(t, OpenAIUsage{
		InputTokens:              12601,
		OutputTokens:             15,
		CacheReadInputTokens:     128,
		CacheCreationInputTokens: 64,
	}, p.usage.cursorOpenAIUsage())
	require.Equal(t, 11, p.usage.Reasoning, "reasoning kept for diagnosis only")

	out := sseJoined(lines)
	require.Contains(t, out, `"input_tokens":12601`)
	require.Contains(t, out, `"output_tokens":15`)
	require.Contains(t, out, `"cache_read_input_tokens":128`)
	require.Contains(t, out, `"cache_creation_input_tokens":64`)
	require.Contains(t, out, `"total_tokens":12808`)
}

// TestCursorParseStreamZeroUsageWhenTurnEndedHasNoTokens 覆盖"有终态但无 token 数"
// 的收尾：显式零值，绝不估算。
func TestCursorProtocolResponseZeroUsageWhenNoTokens(t *testing.T) {
	var stream []byte
	stream = append(stream, cursorTextDeltaFrame(t, "OK")...)
	// 41 号证据里拒付样本的形状：turn_ended 只有 f6（ended_at_ms）。
	stream = append(stream, cursorTurnEndedFrame(t,
		[2]uint64{cursorFldTurnEndedEndedAtMS, 1792327776714})...)

	p := cursorNewStreamParser("m")
	_, err := p.cursorParseStream(stream)
	require.NoError(t, err)
	require.True(t, p.usage.Seen)
	require.Equal(t, OpenAIUsage{}, p.usage.cursorOpenAIUsage())
	require.Contains(t, p.cursorSSEFinishChunk(), `"input_tokens":0`)
}

// TestCursorParseStreamRejectTemplateFailsClosed 钉死流内拒付不当成功（契约 0.2）。
//
// 证据 53 / 125-expiry-recheck / 134-current-token-expired-key：HTTP 200、正常
// text_delta 帧、正常 turn_ended，但文本是固定拒付模板，usage 只有时间字段。
func TestCursorProtocolResponseRejectTemplateFailsClosed(t *testing.T) {
	const liveSample = "密钥已过期或额度耗尽，请续费后使用 (1792310906551-b924f5ef)"

	var stream []byte
	stream = append(stream, cursorTextDeltaFrame(t, liveSample)...)
	stream = append(stream, cursorTurnEndedFrame(t,
		[2]uint64{cursorFldTurnEndedEndedAtMS, 1792310906714})...)

	p := cursorNewStreamParser("m")
	lines, err := p.cursorParseStream(stream)
	require.Error(t, err)
	require.Nil(t, lines, "rejected stream must not produce any SSE chunk")
	require.ErrorIs(t, err, ErrCursorKeyRejectedUpstream)

	var rejected *CursorKeyRejectedError
	require.ErrorAs(t, err, &rejected)
	require.Equal(t, liveSample, rejected.Template, "原始模板片段必须原样留存")
	require.Equal(t, "1792310906551-b924f5ef", rejected.TurnID)

	// 一步到位的两条入口同样失败关闭，绝不回写成功响应。
	_, err = cursorParseStreamToSSE(stream, "m")
	require.ErrorIs(t, err, ErrCursorKeyRejectedUpstream)
	_, err = cursorParseStreamToCompletion(stream, "m")
	require.ErrorIs(t, err, ErrCursorKeyRejectedUpstream)
}

// TestCursorParseStreamRejectTemplateAcrossDeltas 覆盖模板跨帧到达仍被识别。
func TestCursorProtocolResponseRejectTemplateAcrossDeltas(t *testing.T) {
	var stream []byte
	stream = append(stream, cursorTextDeltaFrame(t, "前缀文本")...)
	stream = append(stream, cursorTextDeltaFrame(t, "密钥已过期或额度耗尽，")...)
	stream = append(stream, cursorTextDeltaFrame(t, "请续费后使用 (1-abcdef)")...)
	stream = append(stream, cursorTurnEndedFrame(t)...)

	p := cursorNewStreamParser("m")
	_, err := p.cursorParseStream(stream)
	require.ErrorIs(t, err, ErrCursorKeyRejectedUpstream)
	var rejected *CursorKeyRejectedError
	require.ErrorAs(t, err, &rejected)
	require.Equal(t, "1-abcdef", rejected.TurnID)
}

// TestCursorParseStreamNoDynamicSuffixStillRejected 覆盖无动态后缀时同样识别。
func TestCursorProtocolResponseRejectWithoutSuffix(t *testing.T) {
	var stream []byte
	stream = append(stream, cursorTextDeltaFrame(t, "密钥已过期或额度耗尽，请续费后使用")...)
	stream = append(stream, cursorTurnEndedFrame(t)...)

	_, err := cursorNewStreamParser("m").cursorParseStream(stream)
	require.ErrorIs(t, err, ErrCursorKeyRejectedUpstream)
	var rejected *CursorKeyRejectedError
	require.ErrorAs(t, err, &rejected)
	require.Equal(t, "密钥已过期或额度耗尽，请续费后使用", rejected.Template)
	require.Empty(t, rejected.TurnID)
}

// TestCursorParseStreamRequiresTerminalMarker 钉死终态标记的强制性
// （取消 / 截断的流绝不按成功收尾）。
func TestCursorProtocolResponseRequiresTerminalMarker(t *testing.T) {
	t.Run("只有文本帧（被取消/截断）报错", func(t *testing.T) {
		var stream []byte
		stream = append(stream, cursorTextDeltaFrame(t, "partial")...)
		_, err := cursorNewStreamParser("m").cursorParseStream(stream)
		require.ErrorIs(t, err, ErrCursorMissingEndStream)
	})

	t.Run("空流报错", func(t *testing.T) {
		_, err := cursorNewStreamParser("m").cursorParseStream(nil)
		require.ErrorIs(t, err, ErrCursorMissingEndStream)
	})

	t.Run("干净 EndStream 无 turn_ended 可收尾（usage 显式零值）", func(t *testing.T) {
		var stream []byte
		stream = append(stream, cursorTextDeltaFrame(t, "hi")...)
		stream = append(stream, cursorTestEndStream(t, `{}`)...)
		p := cursorNewStreamParser("m")
		_, err := p.cursorParseStream(stream)
		require.NoError(t, err)
		require.False(t, p.usage.Seen)
		require.Contains(t, p.cursorSSEFinishChunk(), `"input_tokens":0`)
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
		stream = append(stream, cursorTextDeltaFrame(t, "late")...)
		_, err := cursorNewStreamParser("m").cursorParseStream(stream)
		require.ErrorIs(t, err, ErrCursorProtocol)
	})
}

// TestCursorParseStreamEndStreamError 覆盖 EndStream 终态错误的两种错误形状。
func TestCursorProtocolResponseEndStreamError(t *testing.T) {
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
func TestCursorProtocolResponseDropsOutOfScopeFrames(t *testing.T) {
	var stream []byte
	stream = append(stream, cursorServerSkipFrame(t, cursorFldServerExecMessage,
		cursorAppendVarintField(nil, 1, 0))...)
	stream = append(stream, cursorServerSkipFrame(t, cursorFldServerKvMessage, []byte{0x2a})...)
	stream = append(stream, cursorServerSkipFrame(t, cursorFldServerCheckpoint, []byte{0x50, 0x02})...)
	stream = append(stream, cursorServerSkipFrame(t, cursorFldServerExecControl, []byte{0x01})...)
	stream = append(stream, cursorServerSkipFrame(t, cursorFldServerInteractionQuery, []byte{0x01})...)
	stream = append(stream, cursorServerSkipFrame(t, cursorFldServerTTFTBreakdown, []byte{0x01})...)
	stream = append(stream, cursorTextDeltaFrame(t, "visible")...)
	stream = append(stream, cursorTurnEndedFrame(t,
		[2]uint64{cursorFldTurnEndedInputTokens, 7},
		[2]uint64{cursorFldTurnEndedOutputTokens, 3})...)

	p := cursorNewStreamParser("m")
	lines, err := p.cursorParseStream(stream)
	require.NoError(t, err, "out-of-scope frames must be skipped, not fatal")
	out := sseJoined(lines)
	require.Contains(t, out, "visible")
	require.Equal(t, OpenAIUsage{InputTokens: 7, OutputTokens: 3}, p.usage.cursorOpenAIUsage())
}

// TestCursorParseStreamToSSE 覆盖完整 SSE 输出（含 [DONE] 哨兵与合法 JSON）。
func TestCursorProtocolResponseToSSE(t *testing.T) {
	var stream []byte
	stream = append(stream, cursorTextDeltaFrame(t, `say "hi"`+"\n")...)
	stream = append(stream, cursorTurnEndedFrame(t,
		[2]uint64{cursorFldTurnEndedInputTokens, 10},
		[2]uint64{cursorFldTurnEndedOutputTokens, 2})...)

	out, err := cursorParseStreamToSSE(stream, "gpt-4o")
	require.NoError(t, err)
	s := string(out)
	require.True(t, strings.HasSuffix(s, cursorSSEDoneSentinel), "must end with [DONE]")
	require.Equal(t, 1, strings.Count(s, cursorSSEDoneSentinel), "exactly one [DONE]")

	// 每一行 data: 必须是合法 JSON（否则客户端直接断流）。
	var sawUsage bool
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var chunk map[string]any
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk),
			"chunk must be valid JSON: %s", line)
		require.Equal(t, "chat.completion.chunk", chunk["object"])
		if _, ok := chunk["usage"]; ok {
			sawUsage = true
		}
	}
	require.True(t, sawUsage, "final chunk must carry real usage")
}

// TestCursorParseStreamToCompletion 覆盖非流式合成（含转义与真实 usage）。
func TestCursorProtocolResponseToCompletion(t *testing.T) {
	var stream []byte
	stream = append(stream, cursorTextDeltaFrame(t, `He said "hi"`+"\n")...)
	stream = append(stream, cursorTextDeltaFrame(t, ` again`)...)
	stream = append(stream, cursorTurnEndedFrame(t,
		[2]uint64{cursorFldTurnEndedInputTokens, 21},
		[2]uint64{cursorFldTurnEndedOutputTokens, 9},
		[2]uint64{cursorFldTurnEndedCacheReadTokens, 5})...)

	out, err := cursorParseStreamToCompletion(stream, "gpt-4o")
	require.NoError(t, err)

	var completion map[string]any
	require.NoError(t, json.Unmarshal(out, &completion))
	require.Equal(t, "chat.completion", completion["object"])
	require.Equal(t, "gpt-4o", completion["model"])
	require.Contains(t, string(out), `"input_tokens":21`)
	require.Contains(t, string(out), `"output_tokens":9`)
	require.Contains(t, string(out), `"cache_read_input_tokens":5`)
	require.Contains(t, string(out), `"total_tokens":35`)

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
func TestCursorProtocolResponseFailsClosedOnTruncatedFrame(t *testing.T) {
	var stream []byte
	stream = append(stream, cursorTextDeltaFrame(t, "hello")...)
	stream = append(stream, 0x00, 0x00, 0x10, 0x00, 0x41, 0x42) // 声明 4096 字节但只给 2

	_, err := cursorNewStreamParser("m").cursorParseStream(stream)
	require.ErrorIs(t, err, ErrCursorProtocol)
}

// TestCursorUpstreamStatusErrorShape 覆盖上游状态错误的分类。
func TestCursorProtocolResponseUpstreamStatusError(t *testing.T) {
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
