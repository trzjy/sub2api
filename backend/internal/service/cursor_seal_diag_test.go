//go:build unit

package service

// 派发单 B8（外审建议 S1）：交接未齐时把封口失败原因保留到终态可观测性，而不是
// 在 next() 的交接判定处把 sealableDelegations() 的错误直接丢弃。
//
// 根因：只收到 listed 缺失/计数不等等具体原因时，上层在 180s 到期只能看到
// deadline_exceeded，无法从日志区分"缺 listed / 计数不等 / 某 started 无委派"。
//
// 本文件只验证 D1：cursorSSEStream 在交接判定处把 sealable 失败原因**记录**到
// lastSealErr（每帧覆盖），并在交接成功时**清空**——不改动任何控制流（仍不交接、
// 不提前结束）。§3.2 的"把该原因写进终态日志安全字段"依赖 pump 终态落点，属于
// 禁区文件 cursor_execution_service.go，本派发单不允许修改，故不在本文件落日志。

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

//	cursorSealDiagStartedFrame 构造 interaction_update{2 tool_call_started{
//	  1 call_id, 2 ToolCall{15 mcp_tool_call}, [3 model_call_id]}} 帧。
//
// 含 f15（mcp_tool_call 分支，即使为空字段也置位）→ external=true，使该 started 计入
// 批次的外部 started 列表，并带 model_call_id 以便 listed 归属到同一批。字段号依据
// cursor_protocol_response.go 内的常量（活体取证）。
func cursorSealDiagStartedFrame(t *testing.T, callID, modelCallID string) []byte {
	t.Helper()
	// ToolCall 消息（f2）内层含 f15 mcp_tool_call → 触发 external=true（内容可空）。
	toolCall := cursorAppendBytesField(nil, cursorFldToolCallStartedToolCall, []byte{})
	started := cursorAppendStringField(nil, cursorFldToolCallID, callID)
	started = cursorAppendBytesField(started, cursorFldToolCallStartedToolCallMsg, toolCall)
	if modelCallID != "" {
		started = cursorAppendStringField(started, cursorFldToolCallStartedModelCallID, modelCallID)
	}
	update := cursorAppendBytesField(nil, cursorFldUpdateToolCallStarted, started)
	return cursorAppendBytesField(nil, cursorFldServerInteractionUpdate, update)
}

// cursorSealDiagDeclaredToolDefs 取与帧内 tool_name 对应的**原始 f4 tool definition
// 字节**，供独立解析路径 attachToolSchemas 注入。
//
// 来源与 duplex 同源：复用 cursorTestDeclaredToolDefs（内部走 cursorConvertRequest，取
// 请求体里实际发出的 f4），不复制第二套编码器；也不放宽 schema_absent——本路径若拿不到
// 原始 f4 字节，f11 mcp_args 的 schema 还原必须继续失败关闭。
func cursorSealDiagDeclaredToolDefs(t *testing.T) [][]byte {
	t.Helper()
	return cursorTestDeclaredToolDefs(t, &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools: []apicompat.ChatTool{{Type: "function", Function: &apicompat.ChatFunction{
			Name: "get_weather", Parameters: json.RawMessage(`{"type":"object"}`)}}},
	})
}

// cursorSealDiagBuildStream 用给定**原始消息帧**序列构造一个 cursorSSEStream：
// next() 按 envelope（5 字节头 + payload）读取，故每条消息帧都先包一层 cursorAgentFrame。
//
// 独立解析路径（非 duplex）必须挂载本请求的 f4 原始字节：否则携带 f11 mcp_args 的 exec
// 帧按 tool_name 取不到 input_schema，schema 还原以 schema_absent 失败关闭，next() 直接
// 返回协议错误，交接判定处的 sealable 分支（本文件要验证的对象）根本不会被走到。
func cursorSealDiagBuildStream(t *testing.T, frames []byte) *cursorSSEStream {
	t.Helper()
	parser := cursorNewStreamParser("m")
	parser.attachToolSchemas(cursorSealDiagDeclaredToolDefs(t))
	return &cursorSSEStream{source: io.NopCloser(bytes.NewReader(frames)), parser: parser}
}

// cursorSealDiagEnvelope 把一条原始消息帧封装为 next() 期望的 envelope（flags=0 + 长度）。
func cursorSealDiagEnvelope(raw []byte) []byte { return cursorAgentFrame(raw) }

// TestCursorSealDiagRecordsFailureReasonWhenListedMissing 覆盖 D1 前段：
// "started 有委派但缺 listed" → next() 在交接判定处把 sealable 失败原因记录到
// lastSealErr，且断言其错误文案含 "listed" 关键词（缺 listed 是具体阻塞类别）。
//
// 此时仍不交接：delegated 必须为 false，控制流未被改变。
func TestCursorSealDiagRecordsFailureReasonWhenListedMissing(t *testing.T) {
	argsA := cursorToolTestBuildMcpArgs("call_a", "get_weather", []cursorToolTestMcpArgEntry{
		{key: "city", kind: "string", val: "Beijing"}})

	var frames []byte
	frames = append(frames, cursorSealDiagEnvelope(cursorSealDiagStartedFrame(t, "call_a", "mc1"))...)
	frames = append(frames, cursorSealDiagEnvelope(cursorToolTestExecFrame(true, argsA))...)

	s := cursorSealDiagBuildStream(t, frames)
	// 处理 started 帧（无新委托，sealable 不触发）。
	require.NoError(t, s.next())
	// 处理 exec(mcp_args) 帧：此时 capturedToolCalls>emittedCalls → sealable 触发，
	// 因缺 listed 返回 ErrCursorBatchIncomplete（listed=absent）。
	require.NoError(t, s.next())

	require.NotNil(t, s.lastSealErrReason(), "封口失败原因必须被记录，而非被丢弃")
	require.Contains(t, s.lastSealErrReason().Error(), "listed",
		"缺 listed 的具体阻塞类别必须出现在原因里，供终态日志区分")
	require.ErrorIs(t, s.lastSealErrReason(), ErrCursorBatchIncomplete)
	require.False(t, s.delegated, "控制流不得改变：缺 listed 仍不交接")
}

// TestCursorSealDiagClearsReasonOnSuccessfulHandoff 覆盖 D1 后段：
// 在"started 有委派但缺 listed"基础上补一帧 listed 使其可封口 → 交接发生，且
// lastSealErr 被清空（每帧覆盖语义，成功即无阻塞）。
func TestCursorSealDiagClearsReasonOnSuccessfulHandoff(t *testing.T) {
	argsA := cursorToolTestBuildMcpArgs("call_a", "get_weather", []cursorToolTestMcpArgEntry{
		{key: "city", kind: "string", val: "Beijing"}})

	var frames []byte
	frames = append(frames, cursorSealDiagEnvelope(cursorSealDiagStartedFrame(t, "call_a", "mc1"))...)
	frames = append(frames, cursorSealDiagEnvelope(cursorToolTestExecFrame(true, argsA))...)    // 有委派、缺 listed
	frames = append(frames, cursorSealDiagEnvelope(cursorTestToolRequestsListedFrame(t, 1))...) // 补 listed=1 → 可封口

	s := cursorSealDiagBuildStream(t, frames)
	require.NoError(t, s.next())
	require.NoError(t, s.next())
	// 补 listed 帧：sealable 通过 → 交接发生、lastSealErr 清空。
	require.NoError(t, s.next())

	require.Nil(t, s.lastSealErrReason(), "交接成功后封口失败原因必须清空")
	require.True(t, s.delegated, "listed 等齐后必须发生工具交接")
}
