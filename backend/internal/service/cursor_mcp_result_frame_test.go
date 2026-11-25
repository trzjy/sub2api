//go:build unit

package service

// 本文件是 **派发单 B4 + CUR-ALIGN-R2** 的定向单测：
//   - B4：验证 MCP 结果回写的 ExecClientMessage 内层字段号已从误用的信封字段（2）修正为
//     内层 McpResult 字段（11）。
//   - CUR-ALIGN-R2：按第三方真实字段定义验证回写形状——结果载体恒为 McpResult.f1 success，
//     执行失败由 McpSuccess.f2 bool is_error 表达（不再塞进 McpResult.f2/McpError），
//     structured_content 是 McpSuccess.f3 的 protobuf Struct（非空非法 JSON 返回错误）。
//
// 生产实证（group 34 / claude-sonnet-5-5）：内层若误用 2，上游不认该回包、模型停在等结果
// 状态直到上游终态；用 11 时，重构前实现
// （5e2dd5d08 cursorMcpArgsReply：f11 McpResult）与活体探针
// （sub2api-cursor-targeted-20261024/02-native-tools.cjs：fld(11,mcpResult)）均能让上游在同一
// 连接继续并正常 turn_ended。
//
// 全部走真实生产编码函数 cursorEncodeMcpToolResult（§25.3 四层编码），逐层解码断言：
//   - D1：内层含 f11、不含 f2；f1（varint exec id）与 f15（id 原值）保持；四层结构逐层断言。
//   - D2：外层信封字段仍为 2（与 it.duplex.send(cursorFldClientMessage, frame) 一致）。
//
// 不得改动 context(f10)、mcp_state(f36) 回包。

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// cursorTestDecodeStruct 把 McpSuccess.f3 的原始字节反解为 protobuf Struct（标准库）。
func cursorTestDecodeStruct(t *testing.T, raw []byte) *structpb.Struct {
	t.Helper()
	st := &structpb.Struct{}
	require.NoError(t, proto.Unmarshal(raw, st), "McpSuccess.f3 必须是合法 Struct 消息")
	return st
}

// cursorTestStructJSON 把 Struct 字节按标准 protojson 还原为 JSON 文本，便于 JSONEq 断言。
func cursorTestStructJSON(t *testing.T, raw []byte) string {
	t.Helper()
	b, err := protojson.Marshal(cursorTestDecodeStruct(t, raw))
	require.NoError(t, err)
	return string(b)
}

// cursorMcpResultFrame 是 ExecClientMessage 的逐层解码结果（仅本测试使用）。
type cursorMcpResultFrame struct {
	execID        uint64
	idField       []byte
	hasErrorField bool // McpResult.f2（McpError）是否存在——恒应为 false
	isErrorFlag   bool // McpSuccess.f2 是否存在
	isErrorRaw    uint64
	structured    []byte // McpSuccess.f3 原始 Struct 字节
	texts         []string
}

// decodeMcpResultFrame 把 cursorEncodeMcpToolResult 的返回值（裸 ExecClientMessage）按
// §25.3 四层编码逐层解码，并断言内层 McpResult 必须位于 f11、绝不在信封字段 f2。
//
// 字节级层次：ExecClientMessage{f1 exec_id, f15 id, f11 McpResult}
//
//	→ McpResult{f1 success}（f2 McpError 不得承载工具结果）
//	→ McpSuccess{f1 content(repeated), f2 bool is_error, f3 Struct structured_content}
//	→ ContentItem{f1 text} → McpTextContent{f1 text(string)}
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

	// 第二层：McpResult 必须且只能用 f1 success 承载结果；f2 是另一类 McpError。
	mcpFields, err := cursorDecodeProtobuf(mcp.Bytes)
	require.NoError(t, err)
	succ, hasSucc := cursorFindField(mcpFields, cursorFldMcpResultSuccess)
	_, hasErr := cursorFindField(mcpFields, cursorFldMcpResultError)
	require.True(t, hasSucc, "结果载体必须恒为 McpResult.f1 success")
	require.False(t, hasErr, "McpResult.f2(McpError) 不得承载工具执行结果")
	out.hasErrorField = hasErr

	// 第三层：McpSuccess{f1 content(repeated), f2 bool is_error, f3 Struct structured_content}。
	inner, err := cursorDecodeProtobuf(succ.Bytes)
	require.NoError(t, err)
	_, out.isErrorFlag = cursorFindField(inner, cursorFldMcpSuccessIsError)
	out.isErrorRaw = cursorVarintField(inner, cursorFldMcpSuccessIsError)
	for _, f := range inner {
		switch f.Number {
		case cursorFldMcpSuccessContent:
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
		case cursorFldMcpSuccessStructuredContent:
			out.structured = append([]byte(nil), f.Bytes...)
		}
	}
	return out
}

// TestCursorMcpResultFrameInnerFieldIsEleven 覆盖 D1：内层字段号修正为 11 且四层结构完整。
func TestCursorMcpResultFrameInnerFieldIsEleven(t *testing.T) {
	// 成功 + 文本 + 结构化：f11 承载 McpResult，四层结构逐层成立。
	execMsg, err := cursorEncodeMcpToolResult(42, []byte("exec-orig-42"), "call_x",
		[]string{"hello world"}, nil, []byte(`{"k":1}`), false)
	require.NoError(t, err)
	frame := decodeMcpResultFrame(t, execMsg)
	require.EqualValues(t, 42, frame.execID, "必须原样带回原 exec_id（f1 varint）")
	require.Equal(t, []byte("exec-orig-42"), frame.idField, "必须原样带回原 f15 id 字节")
	require.False(t, frame.hasErrorField, "错误不得走 McpResult.f2")
	require.False(t, frame.isErrorFlag, "成功结果不得写 McpSuccess.f2")
	require.Equal(t, []string{"hello world"}, frame.texts, "四层解码须得到正确文本")
	require.JSONEq(t, `{"k":1}`, cursorTestStructJSON(t, frame.structured),
		"合法 JSON 必须编码为 McpSuccess.f3 且可反解为 Struct")

	// is_error：结果载体仍是 McpResult.f1 success，失败语义由 McpSuccess.f2=true 表达。
	errMsg, err := cursorEncodeMcpToolResult(9, []byte("exec-orig-9"), "call_err",
		[]string{"boom"}, nil, nil, true)
	require.NoError(t, err)
	errFrame := decodeMcpResultFrame(t, errMsg)
	require.False(t, errFrame.hasErrorField, "McpResult.f2 必须不存在")
	require.True(t, errFrame.isErrorFlag, "McpSuccess.f2 必须存在")
	require.EqualValues(t, 1, errFrame.isErrorRaw, "McpSuccess.f2 必须是 bool true")
	require.Equal(t, []string{"boom"}, errFrame.texts)
	require.Empty(t, errFrame.structured, "错误结果本例不携带 structured_content")

	// 多文本项：每条文本独立成一条 ContentItem（repeated），顺序保留，内层仍在 f11。
	multiMsg, err := cursorEncodeMcpToolResult(3, nil, "call_multi",
		[]string{"line-a", "line-b"}, nil, nil, false)
	require.NoError(t, err)
	multiFrame := decodeMcpResultFrame(t, multiMsg)
	require.False(t, multiFrame.isErrorFlag)
	require.Equal(t, []string{"line-a", "line-b"}, multiFrame.texts,
		"多文本必须逐项独立编码并顺序保留")
	require.Empty(t, multiFrame.idField, "无 f15 原值时不得凭空造一个 id")
}

// TestCursorMcpResultStructuredField 覆盖结构化载荷：合法 JSON 落到 f3 Struct；非法 JSON
// 直接返回错误，不补空 Struct、不继续发送。
func TestCursorMcpResultStructuredField(t *testing.T) {
	valid, err := cursorEncodeMcpToolResult(1, nil, "call_struct",
		[]string{"text"}, nil, []byte(`{"nested":{"a":[1,true,null]}}`), false)
	require.NoError(t, err)
	frame := decodeMcpResultFrame(t, valid)
	require.NotEmpty(t, frame.structured, "McpSuccess.f3 必须存在")
	require.JSONEq(t, `{"nested":{"a":[1,true,null]}}`, cursorTestStructJSON(t, frame.structured))

	// 非法 JSON 必须报错：不得静默补空 Struct 或伪造成功帧。
	bad, err := cursorEncodeMcpToolResult(2, nil, "call_bad",
		[]string{"text"}, nil, []byte(`{"broken":`), false)
	require.Error(t, err, "非法 structured JSON 必须返回错误")
	require.Nil(t, bad, "编码失败时不得产出任何帧")
	require.ErrorContains(t, err, "structured_content")

	// 非对象 JSON（数组）也不是 Struct：protojson.Unmarshal 必须拒绝。
	arr, err := cursorEncodeMcpToolResult(3, nil, "call_arr",
		nil, nil, []byte(`[1,2]`), false)
	require.Error(t, err)
	require.Nil(t, arr)
}

// TestCursorMcpResultFrameEnvelopeFieldStillTwo 覆盖 D2：外层信封字段仍是 2。
//
// 生产写路径是 it.duplex.send(cursorFldClientMessage, execMsg) → cursorEncodeEnvelope(
// cursorAppendBytesField(nil, 2, execMsg))：外层信封恒为 bytes_field(2, ExecClientMessage)，
// 与 B4 修正前一致；B4 仅修正内层（2 → 11），不动外层信封。
func TestCursorMcpResultFrameEnvelopeFieldStillTwo(t *testing.T) {
	execMsg, err := cursorEncodeMcpToolResult(7, []byte("exec-orig-7"), "call_1",
		[]string{"sunny"}, nil, []byte(`{"temp":25}`), false)
	require.NoError(t, err)

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
