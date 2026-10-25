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
	"io"
	"testing"

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

// cursorSealDiagBuildStream 用给定**原始消息帧**序列构造一个 cursorSSEStream：
// next() 按 envelope（5 字节头 + payload）读取，故每条消息帧都先包一层 cursorAgentFrame。
func cursorSealDiagBuildStream(frames []byte) *cursorSSEStream {
	return &cursorSSEStream{source: io.NopCloser(bytes.NewReader(frames)), parser: cursorNewStreamParser("m")}
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

	s := cursorSealDiagBuildStream(frames)
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

	s := cursorSealDiagBuildStream(frames)
	require.NoError(t, s.next())
	require.NoError(t, s.next())
	// 补 listed 帧：sealable 通过 → 交接发生、lastSealErr 清空。
	require.NoError(t, s.next())

	require.Nil(t, s.lastSealErrReason(), "交接成功后封口失败原因必须清空")
	require.True(t, s.delegated, "listed 等齐后必须发生工具交接")
}
