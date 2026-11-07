//go:build unit

package service

// 派发单 D 阶段 2 新增定向单测：未知字段「必须永远应答、不再打死整轮」。
//
// 覆盖施工图 §4 W1 / §13 与 §10 E 的契约翻转：
//   - 未知 exec 字段（wire=2）必须回通用错误结果帧 {field:{f2:{f2:text}}}，且整轮继续；
//   - 未知 exec 字段（wire=0 / varint）不回帧（只承载 message 的字段才回包），但整轮继续；
//   - 元数据字段（f1/f15/f19/f55/f57）不回帧、不报错（维持现状）；
//   - 未知 interaction query 必须回通用 rejected，整轮继续，不 502；
//   - 工具参数缺必填必须原样把上游原始参数交付客户端，整轮继续，不 502，且不补默认值。
//
// 既有把「不许应答 / 必须 502 / 缺必填必须失败关闭」写成契约的断言已按「契约先行」在对应
// 测试文件里改掉（见 cursor_exec_metadata_test.go / cursor_duplex_request_codes_test.go /
// cursor_agent_duplex_test.go / cursor_retype_required_test.go / cursor_synthetic_shell_test.go）。

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// cursorStage2ExpectGenericReject 定点校验 buf 中写出了一个 ExecClientMessage 通用错误结果帧，
// 形状为 {field:{f2:{f2:text}}}（外层 field = 未知 exec 字段号），错误文本等于 text。
func cursorStage2ExpectGenericReject(t *testing.T, buf *bytes.Buffer, field int, text string) {
	t.Helper()
	require.NotZero(t, buf.Len(), "未知 exec 字段必须写出通用错误结果帧")
	frame, _, err := cursorReadEnvelopeFrame(buf.Bytes(), 0)
	require.NoError(t, err)
	env, err := cursorDecodeProtobuf(frame.Data)
	require.NoError(t, err)
	clientMsg, ok := cursorFindField(env, cursorFldClientMessage)
	require.True(t, ok, "结果帧必须是 ExecClientMessage（f%d）", cursorFldClientMessage)
	msgFields, err := cursorDecodeProtobuf(clientMsg.Bytes)
	require.NoError(t, err)
	result, ok := cursorFindField(msgFields, field)
	require.True(t, ok, "结果帧必须含字段 %d", field)
	payload, err := cursorDecodeProtobuf(result.Bytes)
	require.NoError(t, err)
	inner, ok := cursorFindField(payload, 2)
	require.True(t, ok, "结果字段必须含 f2")
	inner2, err := cursorDecodeProtobuf(inner.Bytes)
	require.NoError(t, err)
	txt, ok := cursorFindField(inner2, 2)
	require.True(t, ok, "内层必须含 f2:text")
	require.Equal(t, text, txt.String(), "错误文本必须一致")
}

// cursorStage2ExpectInteractionRejected 定点校验 buf 中写出了一个 InteractionResponse 通用
// rejected 应答，形状为 {query_field:{f2:{f1:reason}}}。
func cursorStage2ExpectInteractionRejected(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	require.NotZero(t, buf.Len(), "未知 interaction 类型必须写出通用 rejected 应答")
	frame, _, err := cursorReadEnvelopeFrame(buf.Bytes(), 0)
	require.NoError(t, err)
	env, err := cursorDecodeProtobuf(frame.Data)
	require.NoError(t, err)
	resp, ok := cursorFindField(env, cursorFldClientInteractionResponse)
	require.True(t, ok, "应答帧必须是 InteractionResponse（f%d）", cursorFldClientInteractionResponse)
	fields, err := cursorDecodeProtobuf(resp.Bytes)
	require.NoError(t, err)
	var sawRejected bool
	for _, f := range fields {
		if f.Number == cursorFldInteractionID {
			continue
		}
		sub, err := cursorDecodeProtobuf(f.Bytes)
		require.NoError(t, err)
		if rejected, ok := cursorFindField(sub, cursorFldInteractionRespRejected); ok {
			rejFields, err := cursorDecodeProtobuf(rejected.Bytes)
			require.NoError(t, err)
			if reason, ok := cursorFindField(rejFields, cursorFldInteractionRejectedReason); ok {
				require.Equal(t, cursorGenericUnknownInteractionRejectText, reason.String())
				sawRejected = true
			}
		}
	}
	require.True(t, sawRejected, "未知 interaction 应答必须含 rejected + reason")
}

// ① 未知 exec 字段（wire=2，f40）→ 通用错误结果帧 + 整轮继续（后续帧仍能处理）。
func TestCursorStage2UnknownExecFieldWire2RepliesGeneric(t *testing.T) {
	d, buf, logs, cleanup := cursorExecRejectTestDuplex(t)
	defer cleanup()

	execMsg := cursorAppendVarintField(nil, cursorFldExecMetaID, 40)
	execMsg = cursorAppendBytesField(execMsg, cursorFldExecMetaExecID, []byte("exec-40"))
	execMsg = cursorAppendBytesField(execMsg, 40, []byte("unknown-exec-bytes"))

	done, err := d.respond(cursorExecMetaTestPayload(execMsg))
	require.NoError(t, err, "未知 exec 字段（wire=2）不得 502")
	require.False(t, done, "未知 exec 字段不是终态")
	require.Equal(t, 1, phaseCount(logs, "exec_unsupported_fields"))

	// 整轮继续：紧接着一条纯元数据消息必须仍能正常处理（连接未被打死）。
	meta := cursorAppendVarintField(nil, cursorFldExecMetaID, 40)
	meta = cursorAppendBytesField(meta, cursorFldExecMetaExecID, []byte("exec-40"))
	meta = cursorAppendBytesField(meta, cursorFldExecMetaSpanContext, cursorExecMetaTestSpanContext())
	done2, err2 := d.respond(cursorExecMetaTestPayload(meta))
	require.NoError(t, err2, "未知 exec 帧之后整轮必须继续处理后续帧")
	require.False(t, done2)

	// 必须在读取 buf 前关闭 pipe 并等待排干完成（保证断言 buf 时已落盘全部字节）。
	cleanup()
	cursorStage2ExpectGenericReject(t, buf, 40, cursorGenericUnknownExecRejectText)
}

// ② 未知 exec 字段（wire=0，varint）→ 不回结果帧（施工图 W1：只承载 message 的字段才回包），
// 但整轮继续、不 502。
func TestCursorStage2UnknownExecFieldWire0NoFrame(t *testing.T) {
	d, buf, logs, cleanup := cursorExecRejectTestDuplex(t)
	defer cleanup()

	execMsg := cursorAppendVarintField(nil, cursorFldExecMetaID, 40)
	execMsg = cursorAppendBytesField(execMsg, cursorFldExecMetaExecID, []byte("exec-40"))
	execMsg = cursorAppendVarintField(execMsg, 40, 123) // wire=0 / varint 未知字段

	done, err := d.respond(cursorExecMetaTestPayload(execMsg))
	require.NoError(t, err, "未知 exec 字段（wire=0）不得 502")
	require.False(t, done)
	require.Equal(t, 1, phaseCount(logs, "exec_unsupported_fields"))
	require.Zero(t, buf.Len(), "wire=0 未知字段不回任何结果帧（只承载 message 的字段才回包）")
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

// ④ 未知 interaction query → 通用 rejected，整轮继续，不 502。
func TestCursorStage2UnknownInteractionRepliesRejected(t *testing.T) {
	d, buf, logs, cleanup := cursorExecRejectTestDuplex(t)
	defer cleanup()

	payload := cursorIQTestQuery(5, 12, cursorIQTestArgs("draw"))
	_, err := d.respond(payload)
	require.NoError(t, err, "未知 interaction 类型不得 502（派发单 D：回通用 rejected，整轮继续）")
	require.Equal(t, 0, phaseCount(logs, "close"), "连接不得被终止")
	// 必须在读取 buf 前关闭 pipe 并等待排干完成（保证断言 buf 时已落盘全部字节）。
	cleanup()
	cursorStage2ExpectInteractionRejected(t, buf)
}

// ⑤ 工具参数缺必填 → 原样把上游原始参数交付客户端，整轮继续，不 502，不补默认值。
func TestCursorStage2MissingRequiredDegradesToClient(t *testing.T) {
	schema := `{"type":"object","properties":{"command":{"type":"string"}},"required":["command"],"additionalProperties":false}`
	defs := [][]byte{cursorTestBuildToolDef(t, "bash", schema)}
	d, _, logs, cleanup := cursorExecRejectTestDuplex(t, defs...)
	defer cleanup()

	// 上游只交来空参数（缺必填 command）：不得打死整轮，必须原样交付 {}.
	mcpArgs := cursorTestBuildMcpArgsOfficial(t, "call-1", "bash", nil)
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
	require.Equal(t, 1, phaseCount(logs, "mcp_args_retype_fallback"), "缺必填必须走 mcp_args_retype_fallback 降级通道")
	require.Equal(t, 0, phaseCount(logs, "mcp_args_incomplete_rejected"), "缺必填不得走 rejected 拒绝")
}
