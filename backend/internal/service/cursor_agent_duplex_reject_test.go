//go:build unit

package service

// CURSOR-F 任务 2：11 类原生 exec 命中拒绝路径的直接断言。
//
// 与基线已存在的 cursor_exec_metadata_test.go::TestCursorExecNativeRejectAllElevenFields 互补：
// 那个用例只校验「写出非空帧 + 拒绝日志」。本用例进一步解码 pipe 收到的字节，断言拒绝结果帧的
// ExecClientMessage 内层确实携带了与请求字段号一致的 result 字段号（拒绝形状正确），
// 且全程无 MCP 委托登记（原生 exec 不进入客户端工具执行 / mcp_args 委托路径）。
//
// 复用基线 helper（同包、同 //go:build unit，不重定义）：
//   - cursorExecRejectTestDuplex：构造 duplex 并起 goroutine 持续排干 pipe writer，避免写帧阻塞挂起；
//   - cursorExecMetaTestPayload：把 ExecServerMessage 字节包进 AgentServerMessage.f2 信封；
//   - cursorExecNativeRejectFields：11 类原生 exec 字段号集合 {2,3,4,5,7,8,9,14,16,20,23}。

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// cursorAgentDuplexRejectResultFieldPresent 检查 pipe 收集到的字节里，是否至少有任一帧的
// ExecClientMessage 内层携带了给定的 result 字段号。拒绝路径的回写形状为
// ExecClientMessage{f1 id, f15 id, <field> result}，其中 <field> 与请求的 11 类原生 exec
// 字段号相同（cursorEncodeNativeExecRejection 的 resultField == field）。
//
// 信封是 {f2 ExecClientMessage}；ExecClientMessage 内层（f1/f15/result）需要再解一层，故这里
// 逐信封解两次 protobuf。返回 true 表示在任意一帧的内层找到了该 result 字段号。
func cursorAgentDuplexRejectResultFieldPresent(bufBytes []byte, field int) bool {
	off := 0
	for {
		frame, next, err := cursorReadEnvelopeFrame(bufBytes, off)
		if err != nil {
			break
		}
		off = next
		envFields, derr := cursorDecodeProtobuf(frame.Data)
		if derr != nil {
			continue
		}
		msg, ok := cursorFindField(envFields, cursorFldClientMessage)
		if !ok {
			continue
		}
		msgFields, derr := cursorDecodeProtobuf(msg.Bytes)
		if derr != nil {
			continue
		}
		if cursorFindFieldOK(msgFields, field) {
			return true
		}
	}
	return false
}

// TestCursorAgentDuplexNativeExecRejectWritesResultField 对 11 类原生 exec 字段各自断言：
//   - respond 不返回 error（原生 exec 是「拒绝」，不是失败关闭）；
//   - delegations.snapshot() 为空（原生 exec 不进入客户端工具执行 / mcp_args 委托路径）；
//   - pipe 收到的字节里包含对应 result 字段号（拒绝结果帧形状正确）。
func TestCursorAgentDuplexNativeExecRejectWritesResultField(t *testing.T) {
	for _, field := range cursorExecNativeRejectFields {
		field := field
		t.Run(fmt.Sprintf("f%d", field), func(t *testing.T) {
			d, buf, _, cleanup := cursorExecRejectTestDuplex(t)
			defer cleanup()

			execMsg := cursorAppendVarintField(nil, cursorFldExecMetaID, 1)
			execMsg = cursorAppendBytesField(execMsg, cursorFldExecMetaExecID, []byte("exec-1"))
			execMsg = cursorAppendBytesField(execMsg, field, nil)

			done, err := d.respond(cursorExecMetaTestPayload(execMsg))
			require.NoError(t, err, "原生 exec 必须按协议拒绝，不得失败关闭")
			require.False(t, done, "拒绝不是终态")
			require.Empty(t, d.delegations.snapshot(), "原生 exec 拒绝不得登记任何 MCP 委托")
			// 必须在读取 buf 前关闭 pipe 并等待排干完成（保证断言 buf 时已落盘全部字节）。
			cleanup()
			require.True(t, cursorAgentDuplexRejectResultFieldPresent(buf.Bytes(), field),
				"拒绝结果帧的 ExecClientMessage 内层必须携带 result 字段号 %d", field)
		})
	}
}
