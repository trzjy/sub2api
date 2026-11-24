//go:build unit

package service

// 派发单 CUR-TFIX6：原先两个子用例断言「未知 exec / 未知 interaction 回猜测拒绝帧继续」，
// 该契约已翻转。生产现改为返回协议错误、零回包、不登记委托（失败关闭），原先依赖的
// 通用拒绝助手与通用拒绝文本均已删除。本文件把两个子用例改为失败关闭断言。

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTestCursorDuplexRequestErrorCodes(t *testing.T) {
	unknownExec := cursorAppendVarintField(nil, cursorFldExecMetaID, 99)
	unknownExec = cursorAppendBytesField(unknownExec, cursorFldExecMetaExecID, []byte("exec-99"))
	unknownExec = cursorAppendBytesField(unknownExec, 99, []byte("unknown-exec"))

	t.Run("exec_unsupported_field", func(t *testing.T) {
		d, buf, _, cleanup := cursorExecRejectTestDuplex(t)
		defer cleanup()

		done, err := d.respond(cursorExecMetaTestPayload(unknownExec))
		// 失败关闭：返回协议错误、不得标记终态、不得登记 MCP 委托。
		require.Error(t, err, "未知 exec 字段必须失败关闭返回协议错误")
		require.ErrorContains(t, err, "unsupported exec fields")
		require.False(t, done, "失败关闭不由本层标记终态")
		require.Empty(t, d.delegations.snapshot(), "失败关闭不得登记 MCP 委托")
		// 必须在读取 buf 前关闭 pipe 并等待排干完成（保证断言 buf 时已落盘全部字节）。
		cleanup()
		require.Zero(t, buf.Len(), "失败关闭不得写任何回包")
	})

	t.Run("interaction_query_unsupported_field", func(t *testing.T) {
		d, buf, _, cleanup := cursorExecRejectTestDuplex(t)
		defer cleanup()

		done, err := d.respond(cursorIQTestQuery(5, 12, cursorIQTestArgs("draw")))
		// 失败关闭：返回协议错误、零回包，不再回通用 rejected 帧。
		require.Error(t, err, "未知 interaction 字段必须失败关闭返回协议错误")
		require.ErrorContains(t, err, "unsupported interaction query field")
		require.False(t, done, "失败关闭不由本层标记终态")
		require.Empty(t, d.delegations.snapshot(), "失败关闭不得登记 MCP 委托")
		// 必须在读取 buf 前关闭 pipe 并等待排干完成（保证断言 buf 时已落盘全部字节）。
		cleanup()
		require.Zero(t, buf.Len(), "失败关闭不得写任何回包")
	})
}
