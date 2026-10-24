//go:build unit

package service

// 本文件是 **派发单 B4** 的定向单测：验证 MCP 结果回写的 ExecClientMessage 内层字段号
// 已从误用的信封字段（2）修正为内层 McpResult 字段（11）。
//
// 生产实证（group 34 / claude-sonnet-5-5）：内层若误用 2，上游不认该回包、模型停在等结果
// 状态直到 180s deadline_exceeded / turn_ended_missing；用 11 时，重构前实现
// （5e2dd5d08 cursorMcpArgsReply：f11 McpResult）与活体探针
// （sub2api-cursor-targeted-20261024/02-native-tools.cjs：fld(11,mcpResult)）均能让上游在同一
// 连接继续并正常 turn_ended。
//
// 全部走真实生产编码函数 cursorEncodeMcpToolResult（§25.3 四层编码），逐层解码断言：
//   - D1：内层含 f11、不含 f2；f1（varint exec id）与 f15（id 原值）保持；四层结构逐层断言。
//   - D2：外层信封字段仍为 2（与 it.duplex.send(cursorFldClientMessage, frame) 一致）。
//
// 不得改动 McpResult 内部四层结构（f1 success / f2 error / content / structured），
// 不得改动 context(f10)、mcp_state(f36) 回包。

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// cursorMcpResultFrame 是 ExecClientMessage 的逐层解码结果（仅本测试使用）。
type cursorMcpResultFrame struct {
	execID  uint64
	idField []byte
	isError bool
	texts   []string
}

// decodeMcpResultFrame 把 cursorEncodeMcpToolResult 的返回值（裸 ExecClientMessage）按
// §25.3 四层编码逐层解码，并断言内层 McpResult 必须位于 f11、绝不在信封字段 f2。
//
// 字节级层次：ExecClientMessage{f1 exec_id, f15 id, f11 McpResult}
//
//	→ McpResult{f1 success | f2 error}
//	→ McpSuccess/Error{f1 content(repeated ContentItem), f2 structured_content}
//	→ ContentItem{f1 text=McpTextContent} → McpTextContent{f1 text(string)}
func decodeMcpResultFrame(t *testing.T, execMsg []byte) cursorMcpResultFrame {
	t.Helper()
	outer, err := cursorDecodeProtobuf(execMsg)
	require.NoError(t, err, "ExecClientMessage 必须是合法 protobuf")

	out := cursorMcpResultFrame{execID: cursorVarintField(outer, cursorFldClientMsgID)}

	// f15 id 原值必须保持。
	if id, ok := cursorFindField(outer, cursorFldClientMsgExecID); ok {
		out.idField = append([]byte(nil), id.Bytes...)
	}

	// 内层 McpResult 必须位于 f11（B4 修正点）。
	mcp, ok := cursorFindField(outer, cursorFldClientMsgMcpResult)
	require.True(t, ok, "ExecClientMessage 内层 McpResult 必须位于 f11")
	require.Equal(t, 11, mcp.Number, "内层 McpResult 字段号必须为 11")

	// 顶层 ExecClientMessage 绝不得再承载信封字段 f2（B4：以前误用 2 的位置）。
	_, hasEnvelopeField := cursorFindField(outer, cursorFldClientMessage)
	require.False(t, hasEnvelopeField, "ExecClientMessage 顶层不得把 McpResult 放在信封字段 f2")
	for _, f := range outer {
		require.NotEqual(t, cursorFldClientMessage, f.Number,
			"ExecClientMessage 顶层字段号不得为信封字段 2")
	}

	// 第二层：McpResult{f1 success | f2 error}。
	mcpFields, err := cursorDecodeProtobuf(mcp.Bytes)
	require.NoError(t, err)
	succ, hasSucc := cursorFindField(mcpFields, cursorFldMcpResultSuccess)
	errField, hasErr := cursorFindField(mcpFields, cursorFldMcpResultError)
	require.True(t, hasSucc != hasErr, "McpResult 必须且只能携带 success 或 error 之一")
	out.isError = hasErr

	payload := succ
	if hasErr {
		payload = errField
	}

	// 第三层：McpSuccess/Error{f1 content(repeated), f2 structured_content}。
	inner, err := cursorDecodeProtobuf(payload.Bytes)
	require.NoError(t, err)
	for _, f := range inner {
		if f.Number != cursorFldMcpSuccessContent {
			continue
		}
		// 第四层：ContentItem{f1 text} → 第五层：McpTextContent{f1 text(string)}。
		itemFields, ierr := cursorDecodeProtobuf(f.Bytes)
		require.NoError(t, ierr)
		tf, ok := cursorFindField(itemFields, cursorFldContentItemText)
		require.True(t, ok, "ContentItem 必须携带 f1 text")
		textFields, terr := cursorDecodeProtobuf(tf.Bytes)
		require.NoError(t, terr)
		textF, ok := cursorFindField(textFields, cursorFldTextContentText)
		require.True(t, ok, "McpTextContent 必须携带 f1 text")
		out.texts = append(out.texts, string(textF.Bytes))
	}
	return out
}

// TestCursorMcpResultFrameInnerFieldIsEleven 覆盖 D1：内层字段号修正为 11 且四层结构完整。
func TestCursorMcpResultFrameInnerFieldIsEleven(t *testing.T) {
	// 成功 + 文本 + 结构化：f11 承载 McpResult，四层结构逐层成立。
	execMsg := cursorEncodeMcpToolResult(42, []byte("exec-orig-42"), "call_x",
		[]string{"hello world"}, []byte(`{"k":1}`), false)
	frame := decodeMcpResultFrame(t, execMsg)
	require.EqualValues(t, 42, frame.execID, "必须原样带回原 exec_id（f1 varint）")
	require.Equal(t, []byte("exec-orig-42"), frame.idField, "必须原样带回原 f15 id 字节")
	require.False(t, frame.isError, "成功结果不得走错误分支")
	require.Equal(t, []string{"hello world"}, frame.texts, "四层解码须得到正确文本")

	// is_error：必须走 McpResult.f2（错误语义），内层 McpResult 仍在 f11。
	errMsg := cursorEncodeMcpToolResult(9, []byte("exec-orig-9"), "call_err",
		[]string{"boom"}, nil, true)
	errFrame := decodeMcpResultFrame(t, errMsg)
	require.True(t, errFrame.isError, "is_error 结果必须保留错误语义（McpResult.f2）")
	require.Equal(t, []string{"boom"}, errFrame.texts)

	// 多文本项：每条文本独立成一条 ContentItem（repeated），顺序保留，内层仍在 f11。
	multiMsg := cursorEncodeMcpToolResult(3, nil, "call_multi",
		[]string{"line-a", "line-b"}, nil, false)
	multiFrame := decodeMcpResultFrame(t, multiMsg)
	require.False(t, multiFrame.isError)
	require.Equal(t, []string{"line-a", "line-b"}, multiFrame.texts,
		"多文本必须逐项独立编码并顺序保留")
	require.Empty(t, multiFrame.idField, "无 f15 原值时不得凭空造一个 id")
}

// TestCursorMcpResultFrameEnvelopeFieldStillTwo 覆盖 D2：外层信封字段仍是 2。
//
// 生产写路径是 it.duplex.send(cursorFldClientMessage, execMsg) → cursorEncodeEnvelope(
// cursorAppendBytesField(nil, 2, execMsg))：外层信封恒为 bytes_field(2, ExecClientMessage)，
// 与 B4 修正前一致；B4 仅修正内层（2 → 11），不动外层信封。
func TestCursorMcpResultFrameEnvelopeFieldStillTwo(t *testing.T) {
	execMsg := cursorEncodeMcpToolResult(7, []byte("exec-orig-7"), "call_1",
		[]string{"sunny"}, []byte(`{"temp":25}`), false)

	// 复刻 send 的外层包裹：cursorEncodeEnvelope(cursorAppendBytesField(nil, 2, execMsg))。
	envelope := cursorEncodeEnvelope(cursorAppendBytesField(nil, cursorFldClientMessage, execMsg))
	frame, _, err := cursorReadEnvelopeFrame(envelope, 0)
	require.NoError(t, err, "回写字节必须是合法 envelope")

	outer, err := cursorDecodeProtobuf(frame.Data)
	require.NoError(t, err, "envelope payload 必须是合法 protobuf")

	// 外层信封字段必须是 2（bytes_field(2, ExecClientMessage)）。
	ecm, ok := cursorFindField(outer, cursorFldClientMessage)
	require.True(t, ok, "envelope payload 必须是 bytes_field(2, ExecClientMessage)")

	// 剥掉外层 f2 后，内层 ExecClientMessage 顶层不得再出现信封字段 2（B4 修正点）。
	innerFields, err := cursorDecodeProtobuf(ecm.Bytes)
	require.NoError(t, err)
	_, innerHasF2 := cursorFindField(innerFields, cursorFldClientMessage)
	require.False(t, innerHasF2, "内层 ExecClientMessage 不得把 McpResult 放在信封字段 f2")
}
