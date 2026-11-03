//go:build unit

package service

// 派发单 TC-02B「流式出站：命中内联 tool call 失败关闭 + 可观测性 phase 事件」的验收测试。
//
// 覆盖矩阵：
//   - 流式-命中失败关闭：正文含**裸字节**形态 A（站点 blob 形态）→ next() 返回
//     协议错误（errors.Is ErrCursorProtocol），且**缓冲区没有写出**含 blob 的 chunk
//     （失败关闭发生在写 chunk 之前，客户端拿到的是明确错误而不是一段不可执行的正文）；
//   - 流式-正常文本回归：普通正文 → 正常写出文本 chunk、不返回错误；
//   - 流式-误判防护回归：正文只出现 `tool_call` 字样、或只出现**落单开标记**
//     （没有配对闭标记）→ 都不触发失败关闭（宁可漏判，绝不误判）；
//   - 可观测性 phase 事件：流式命中落 `inline_tool_call_rejected`（reason=stream_hit），
//     归一成功落 `inline_tool_call_normalized`；两者的字段只含稳定短串，
//     **不含 args 明文 / 工具返回内容 / 正文原文**。
//
// 断言只依赖本卡允许改动的文件与 TC-01R2 已交付的解析器（只读引用，不改动）。

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// cursorInlineStreamOpen / cursorInlineStreamClose 是**裸字节**的内联标记
// （U+003C + "tool_call" + U+003E / U+003C + U+002F + "tool_call" + U+003E）。
//
// 刻意用码点转义拼出来而不是写字面量：源码里**没有位置**可以藏进零宽字符，
// 字节一旦写错就必然对不上（零宽字符会让标记永远不命中上游正文）。
const (
	cursorInlineStreamOpen  = "\x3ctool_call\x3e"
	cursorInlineStreamClose = "\x3c/tool_call\x3e"
)

// cursorInlineStreamFormA 是形态 A（站点自身 blob 形态，JSON 合法）的完整片段。
const cursorInlineStreamFormA = cursorInlineStreamOpen + "\n" +
	`{"id":"call_c3a4d6c4a7f54a5e9f5c2b8d","name":"REPL","arguments":{"code":"const out = {};"}}` +
	"\n" + cursorInlineStreamClose

// cursorInlineStreamTextFrame 构造 interaction_update{1 text_delta{1 text}} 帧
// （外层套 envelope，供 next() 逐帧读取）。
func cursorInlineStreamTextFrame(t *testing.T, text string) []byte {
	t.Helper()
	delta := cursorAppendStringField(nil, cursorFldTextDeltaText, text)
	update := cursorAppendBytesField(nil, cursorFldUpdateTextDelta, delta)
	return cursorEncodeEnvelope(cursorAppendBytesField(nil, cursorFldServerInteractionUpdate, update))
}

// cursorInlineStreamPhaseEvents 取出落盘事件里指定 phase 的全部条目。
func cursorInlineStreamPhaseEvents(t *testing.T, logs *observer.ObservedLogs, phase string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, entry := range logs.All() {
		fields := entry.ContextMap()
		if fields["phase"] == phase {
			out = append(out, fields)
		}
	}
	return out
}

// TestInlineToolCallStreamNormalizesInlineToolCall 流式-内联归一：
//
// 正文含裸字节形态 A → **不报错**、不写出含 blob 的正文；体面收尾（turn_ended）时
// 还原成结构化 tool_calls，与非流式 cursorAggregateSSEToCompletion 同口径。
//
// 历史：本用例原名 ...StreamFailsClosedOnInlineToolCall，断言流式命中即失败关闭。
// 2026-11-04 改为归一——失败关闭对客户端同样是「拿不到工具」，只是换了个报错方式，
// 属把问题从静默变成显式、并未真正修好。
func TestInlineToolCallStreamNormalizesInlineToolCall(t *testing.T) {
	// 第一帧：正文（前言 + 内联调用 + 尾语）；第二帧：turn_ended（usage 齐全 → done）。
	frames := cursorInlineStreamTextFrame(t, "前言。"+cursorInlineStreamFormA+"后语。")
	frames = append(frames, cursorTurnEndedFrame(t,
		[2]uint64{1, 10}, [2]uint64{2, 5}, [2]uint64{3, 0}, [2]uint64{4, 0})...)

	core, logs := observer.New(zapcore.DebugLevel)
	s := &cursorSSEStream{
		source: io.NopCloser(bytes.NewReader(frames)),
		parser: cursorNewStreamParser("m"),
	}
	s.parser.lg = zap.New(core)

	require.NoError(t, s.next(), "流式正文内联 tool call 必须归一，不得失败关闭")
	require.NoError(t, s.next(), "turn_ended 收尾必须写出 tool_calls")

	// 交付侧绝不出现内联原文：客户端要么拿到正文、要么拿到 tool_calls。
	out := s.buffer.String()
	require.NotContains(t, out, cursorInlineStreamOpen, "缓冲区不得写出内联开标记")
	require.NotContains(t, out, cursorInlineStreamClose, "缓冲区不得写出内联闭标记")

	// 归一结果：结构化 tool_calls + finish_reason=tool_calls，id / 名称 / 参数完整交付。
	require.Contains(t, out, `"tool_calls"`, "必须吐出结构化 tool_calls")
	require.Contains(t, out, `"call_c3a4d6c4a7f54a5e9f5c2b8d"`, "调用 id 必须原样交付")
	require.Contains(t, out, `"name":"REPL"`, "工具名必须还原")
	require.Contains(t, out, `"finish_reason":"tool_calls"`, "收尾原因必须是 tool_calls")

	// 可观测性：归一成功落 inline_tool_call_normalized，且不再落 rejected。
	normalized := cursorInlineStreamPhaseEvents(t, logs, cursorPhaseInlineToolCallNormalized)
	require.Len(t, normalized, 1, "归一成功必须落且只落一次 inline_tool_call_normalized")
	require.Equal(t, cursorInlineShapeSiteBlob, normalized[0]["inline_shape"])
	require.Equal(t, int64(1), normalized[0]["inline_tool_count"])
	require.Empty(t, cursorInlineStreamPhaseEvents(t, logs, cursorPhaseInlineToolCallRejected),
		"归一成功不得落 rejected")
}

// TestInlineToolCallStreamHalfClosedBatchNormalizes 流式-半闭合形态归一：
//
// 并行连发只写开标记、漏写闭标记（线上实证形态）在流式下同样必须还原成多条
// tool_calls，而不是报错或透传。
func TestInlineToolCallStreamHalfClosedBatchNormalizes(t *testing.T) {
	text := ""
	for i := 0; i < 3; i++ {
		text += cursorInlineStreamOpen + "\n" +
			`{"id":"call_` + strconv.Itoa(i) + `","name":"Bash","arguments":{"command":"ls"}}` + "\n"
	}
	text += "\nLet me consolidate. I'll wait for results.\n"

	frames := cursorInlineStreamTextFrame(t, text)
	frames = append(frames, cursorTurnEndedFrame(t,
		[2]uint64{1, 10}, [2]uint64{2, 5}, [2]uint64{3, 0}, [2]uint64{4, 0})...)

	core, logs := observer.New(zapcore.DebugLevel)
	s := &cursorSSEStream{
		source: io.NopCloser(bytes.NewReader(frames)),
		parser: cursorNewStreamParser("m"),
	}
	s.parser.lg = zap.New(core)

	require.NoError(t, s.next())
	require.NoError(t, s.next())

	out := s.buffer.String()
	require.NotContains(t, out, cursorInlineStreamOpen)
	require.Contains(t, out, `"tool_calls"`)
	for i := 0; i < 3; i++ {
		require.Contains(t, out, `"call_`+strconv.Itoa(i)+`"`, "三条调用必须全部交付")
	}
	require.Contains(t, out, `"finish_reason":"tool_calls"`)

	normalized := cursorInlineStreamPhaseEvents(t, logs, cursorPhaseInlineToolCallNormalized)
	require.Len(t, normalized, 1)
	require.Equal(t, int64(3), normalized[0]["inline_tool_count"])
}

// TestInlineToolCallStreamPlainTextPasses 流式-正常文本回归：普通正文正常写出 chunk。
func TestInlineToolCallStreamPlainTextPasses(t *testing.T) {
	frames := cursorInlineStreamTextFrame(t, "这是一段普通正文，没有工具调用。")

	core, logs := observer.New(zapcore.DebugLevel)
	s := &cursorSSEStream{
		source: io.NopCloser(bytes.NewReader(frames)),
		parser: cursorNewStreamParser("m"),
	}
	s.parser.lg = zap.New(core)

	require.NoError(t, s.next(), "普通正文不得触发失败关闭")
	out := s.buffer.String()
	require.Contains(t, out, `"role":"assistant"`, "正常文本必须先发 role 包")
	require.Contains(t, out, "这是一段普通正文", "正常文本必须写出文本 chunk")

	require.Empty(t, cursorInlineStreamPhaseEvents(t, logs, cursorPhaseInlineToolCallRejected),
		"正常正文不得落 inline_tool_call_rejected")
	require.Empty(t, cursorInlineStreamPhaseEvents(t, logs, cursorPhaseInlineToolCallNormalized),
		"正常正文不得落 inline_tool_call_normalized")
}

// TestInlineToolCallStreamToolCallWordDoesNotFailClosed 流式-误判防护回归：
// 正文含 `tool_call` 字样、或只出现落单开标记（无配对闭标记）→ 都不失败关闭。
func TestInlineToolCallStreamToolCallWordDoesNotFailClosed(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{name: "只有 tool_call 字样", text: "我们稍后调用 tool_call 来执行这一步。"},
		{name: "只有落单开标记", text: "正文里提到了 " + cursorInlineStreamOpen + " 这个标记。"},
		{name: "字样与开标记混杂但无闭标记", text: "tool_call " + cursorInlineStreamOpen + " 悬空"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frames := cursorInlineStreamTextFrame(t, tc.text)

			core, logs := observer.New(zapcore.DebugLevel)
			s := &cursorSSEStream{
				source: io.NopCloser(bytes.NewReader(frames)),
				parser: cursorNewStreamParser("m"),
			}
			s.parser.lg = zap.New(core)

			require.NoError(t, s.next(),
				"未构成内联形态（标记不成对）必须按未识别处理，不得失败关闭")
			require.Contains(t, s.buffer.String(), "tool_call",
				"未识别的正文必须原样透出，不得被吞掉")
			require.Empty(t, cursorInlineStreamPhaseEvents(t, logs, cursorPhaseInlineToolCallRejected),
				"未识别的正文不得落 inline_tool_call_rejected")
		})
	}
}

// TestInlineToolCallStreamPhaseEventContract 可观测性 phase 事件的契约：
// 两个 phase 名、字段集与落盘通道就位（TC-02A 的非流式归一落点依赖本契约）。
//
// 同时钉死脱敏口径：事件只含稳定短串与计数，绝不含 args 明文 / 工具返回内容 / 正文原文。
func TestInlineToolCallStreamPhaseEventContract(t *testing.T) {
	require.Equal(t, "inline_tool_call_normalized", cursorPhaseInlineToolCallNormalized)
	require.Equal(t, "inline_tool_call_rejected", cursorPhaseInlineToolCallRejected)
	require.Equal(t, "site_blob", cursorInlineShapeSiteBlob)
	require.Equal(t, "upstream_private", cursorInlineShapeUpstreamPrivate)
	require.Equal(t, "unparsable", cursorInlineRejectReasonUnparsable)
	require.Equal(t, "stream_hit", cursorInlineRejectReasonStreamHit)

	core, logs := observer.New(zapcore.DebugLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core))

	// 归一成功事件（TC-02A 在非流式侧调用同一通道）。
	cursorPhaseLogInlineToolCallNormalized(ctx, "m", cursorInlineShapeSiteBlob, 1)
	normalized := cursorInlineStreamPhaseEvents(t, logs, cursorPhaseInlineToolCallNormalized)
	require.Len(t, normalized, 1, "归一成功必须落且只落一次 inline_tool_call_normalized")
	require.Equal(t, cursorInlineShapeSiteBlob, normalized[0]["inline_shape"])
	require.Equal(t, int64(1), normalized[0]["inline_tool_count"])
	require.Equal(t, "cursor.upstream", normalized[0]["component"])

	// 被拒绝事件（解析失败）。
	cursorPhaseLogInlineToolCallRejected(ctx, "m", cursorInlineShapeUpstreamPrivate,
		cursorInlineRejectReasonUnparsable)
	rejected := cursorInlineStreamPhaseEvents(t, logs, cursorPhaseInlineToolCallRejected)
	require.Len(t, rejected, 1, "被拒绝必须落且只落一次 inline_tool_call_rejected")
	require.Equal(t, cursorInlineShapeUpstreamPrivate, rejected[0]["inline_shape"])
	require.Equal(t, cursorInlineRejectReasonUnparsable, rejected[0]["inline_reason"])

	// 脱敏：两个事件的字段集合里不得出现参数 / 返回内容 / 正文相关的键。
	for _, fields := range append(normalized, rejected...) {
		for key := range fields {
			lower := strings.ToLower(key)
			require.NotContains(t, lower, "argument")
			require.NotContains(t, lower, "args")
			require.NotContains(t, lower, "content")
			require.NotContains(t, lower, "result")
			require.NotContains(t, lower, "text")
		}
	}
}
