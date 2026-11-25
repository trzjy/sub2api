//go:build unit

package service

// CUR-TFIX4：未知 exec（bytes / varint）与未知/缺 oneof interaction 一律失败关闭。
//
// 旧的「猜测拒绝帧 / 整轮继续」契约已翻转：未知 exec 字段（含 wire=2 bytes 与 wire=0
// varint）与未知/缺 oneof interaction 返回既有协议错误、零回包、不登记委托；纯元数据
// 字段（f1/f15/f19/f55/f57）仍忽略、不报错。依据施工 §4 W1 / §13、§10 E 与运输层
// 失败关闭验收。

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// cursorStage2AssertFailClosed 校验失败关闭路径的公共断言：respond 返回错误、done=false、
// pipe 零回包、无委托登记。调用方在 respond 后传入 done/err；本函数关闭 pipe 并等待排干
// 完成后再校验 buf，确保「零回包」断言已落盘全部字节。
func cursorStage2AssertFailClosed(t *testing.T, d *cursorAgentDuplex, buf *bytes.Buffer, cleanup func(), done bool, err error, errContains string) {
	t.Helper()
	require.Error(t, err, "失败关闭必须返回既有协议错误")
	require.Contains(t, err.Error(), errContains)
	require.False(t, done, "失败关闭不由本层标记终态")
	require.Empty(t, d.delegations.snapshot(), "失败关闭不得登记 MCP 委托")
	cleanup()
	require.Zero(t, buf.Len(), "失败关闭不得写任何回包")
}

// ① 未知 exec 字段（wire=2，f40）→ 失败关闭：协议错误、零回包、无委托。
func TestCursorStage2UnknownExecFieldWire2FailsClosed(t *testing.T) {
	d, buf, logs, cleanup := cursorExecRejectTestDuplex(t)
	defer cleanup()

	execMsg := cursorAppendVarintField(nil, cursorFldExecMetaID, 40)
	execMsg = cursorAppendBytesField(execMsg, cursorFldExecMetaExecID, []byte("exec-40"))
	execMsg = cursorAppendBytesField(execMsg, 40, []byte("unknown-exec-bytes"))

	done, err := d.respond(cursorExecMetaTestPayload(execMsg))
	cursorStage2AssertFailClosed(t, d, buf, cleanup, done, err, "unsupported exec fields")
	require.Equal(t, 1, phaseCount(logs, "exec_unsupported_fields"), "未知 exec 字段必须记一条取证日志")
	require.Zero(t, phaseCount(logs, "exec_metadata_only"), "含未知字段不得按元数据忽略")
}

// ② 未知 exec 字段（wire=0，varint）→ 同样失败关闭，不再静默忽略。
func TestCursorStage2UnknownExecFieldWire0FailsClosed(t *testing.T) {
	d, buf, logs, cleanup := cursorExecRejectTestDuplex(t)
	defer cleanup()

	execMsg := cursorAppendVarintField(nil, cursorFldExecMetaID, 40)
	execMsg = cursorAppendBytesField(execMsg, cursorFldExecMetaExecID, []byte("exec-40"))
	execMsg = cursorAppendVarintField(execMsg, 40, 123) // wire=0 / varint 未知字段

	done, err := d.respond(cursorExecMetaTestPayload(execMsg))
	cursorStage2AssertFailClosed(t, d, buf, cleanup, done, err, "unsupported exec fields")
	require.Equal(t, 1, phaseCount(logs, "exec_unsupported_fields"), "varint 未知字段必须失败关闭并记取证日志")
}

// ③ 元数据字段（f1/f15/f19/f55/f57）→ 不回帧、不报错（维持现状）。
func TestCursorStage2MetadataFieldsNoFrame(t *testing.T) {
	d, buf, logs, cleanup := cursorExecRejectTestDuplex(t)
	defer cleanup()

	execMsg := cursorAppendVarintField(nil, cursorFldExecMetaID, 1)
	execMsg = cursorAppendBytesField(execMsg, cursorFldExecMetaExecID, []byte("exec-meta"))
	execMsg = cursorAppendBytesField(execMsg, cursorFldExecMetaSpanContext, cursorExecMetaTestSpanContext())
	execMsg = cursorAppendVarintField(execMsg, cursorFldExecMetaAcceptHook, 0)
	execMsg = cursorAppendBytesField(execMsg, cursorFldExecMetaMachineID, []byte("machine-1"))

	done, err := d.respond(cursorExecMetaTestPayload(execMsg))
	require.NoError(t, err, "纯元数据消息不得失败关闭")
	require.False(t, done)
	require.Equal(t, 1, phaseCount(logs, "exec_metadata_only"))
	require.Zero(t, buf.Len(), "元数据字段不回任何结果帧")
}

// ④ 未知/缺 oneof interaction query → 失败关闭：协议错误、零回包、无委托。
func TestCursorStage2UnknownInteractionFailsClosed(t *testing.T) {
	cases := []struct {
		name        string
		query       []byte
		errContains string
	}{
		{
			"unknown_field",
			cursorAppendBytesField(cursorAppendVarintField(nil, cursorFldInteractionID, 11), 42, nil),
			"unsupported interaction query field 42",
		},
		{
			"missing_oneof",
			cursorAppendVarintField(nil, cursorFldInteractionID, 12),
			"unsupported interaction query field 0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, buf, logs, cleanup := cursorExecRejectTestDuplex(t)
			defer cleanup()

			payload := cursorAppendBytesField(nil, cursorFldServerInteractionQuery, tc.query)
			done, err := d.respond(payload)
			cursorStage2AssertFailClosed(t, d, buf, cleanup, done, err, tc.errContains)
			require.Equal(t, 1, phaseCount(logs, "interaction_query_unsupported"), "未知 interaction 必须记一条取证日志")
			require.Zero(t, phaseCount(logs, "interaction_query_answered"), "失败关闭不得写出应答日志")
		})
	}
}

// ⑤ 工具参数缺必填 → 原样把上游原始参数交付客户端，整轮继续，不 502，不补默认值。
// 新语义：运输边界只做语法校验，不按 schema 补默认值；上游原始参数逐字节交付。
func TestCursorStage2MissingRequiredDegradesToClient(t *testing.T) {
	defs := cursorTestTransportDefs(t, "bash")
	d, _, logs, cleanup := cursorExecRejectTestDuplex(t, defs...)
	defer cleanup()

	// 上游只交来空参数（缺必填 command）：不得打死整轮，必须原样交付 {}。
	mcpArgs := cursorToolTestBuildMcpArgs("call-1", "bash", nil)
	execMsg := cursorAppendVarintField(nil, cursorFldExecMetaID, 1)
	execMsg = cursorAppendBytesField(execMsg, cursorFldExecMetaExecID, []byte("exec-1"))
	execMsg = cursorAppendBytesField(execMsg, 11, mcpArgs)

	done, err := d.respond(cursorExecMetaTestPayload(execMsg))
	require.NoError(t, err, "缺必填不得打死整轮（派发单 D：原样降级交付客户端）")
	require.False(t, done, "降级交付不是终态")

	snap := d.delegations.snapshot()
	require.Len(t, snap, 1, "缺必填必须仍登记委托（降级交付，不是拒绝）")
	require.Equal(t, "bash", snap[0].Name)
	require.Equal(t, "{}", snap[0].ArgsJSON, "缺必填必须原样交付上游参数，绝不补默认值（command 不得补成空串）")
	// 旧事件已不存在：运输边界不按 schema 校验/补默认值，故既不产生降级日志，也不产生拒绝日志。
	require.Zero(t, phaseCount(logs, "mcp_args_retype_fallback"), "新语义不再产生 mcp_args_retype_fallback 降级日志")
	require.Zero(t, phaseCount(logs, "mcp_args_incomplete_rejected"), "新语义不再产生 mcp_args_incomplete_rejected 拒绝日志")
}
