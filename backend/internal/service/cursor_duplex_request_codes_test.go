//go:build unit

package service

// 派发单 D 阶段 2：cursor_agent_duplex.go 两处「未知字段 → CUR-EXEC-02 / 502 失败关闭」产生点
// 已改为「回通用应答、整轮继续、不 502」。本文件把原「带码 CursorRequestError」定向测试改为
// 断言新契约：未知 exec 字段回 {field:{f2:{f2:text}}} 通用错误帧，未知 interaction 类型回
// 通用 rejected，二者都不得 502、不得返回 error（施工图 §10 E：契约从「必须失败关闭」翻转为
// 「必须永远应答」）。

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

		_, err := d.respond(cursorExecMetaTestPayload(unknownExec))
		// 派发单 D：未知 exec 字段不得 502、不得返回 error；必须回通用错误结果帧继续。
		require.NoError(t, err, "未知 exec 字段不得 502（派发单 D：回通用错误帧，整轮继续）")
		// 必须在读取 buf 前关闭 pipe 并等待排干完成（保证断言 buf 时已落盘全部字节）。
		cleanup()
		cursorStage2ExpectGenericReject(t, buf, 99, cursorGenericUnknownExecRejectText)
	})

	t.Run("interaction_query_unsupported_field", func(t *testing.T) {
		d, buf, _, cleanup := cursorExecRejectTestDuplex(t)
		defer cleanup()

		_, err := d.respond(cursorIQTestQuery(5, 12, cursorIQTestArgs("draw")))
		// 派发单 D：未知 interaction 类型不得 502、不得返回 error；必须回通用 rejected 继续。
		require.NoError(t, err, "未知 interaction 类型不得 502（派发单 D：回通用 rejected，整轮继续）")
		// 必须在读取 buf 前关闭 pipe 并等待排干完成（保证断言 buf 时已落盘全部字节）。
		cleanup()
		cursorStage2ExpectInteractionRejected(t, buf)
	})
}
