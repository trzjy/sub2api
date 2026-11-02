//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
)

// 参数重打失败（典型活体：客户端 schema 声明 additionalProperties:false，上游仍多给
// 一个未声明字段）**不得**升级为协议错误：fail-closed 会把一次参数形状不合变成
// 502 + 会话终结（11-03 03:42：Write → additional_property_present →
// frame_handle_failed → CUR-RES-99/502，随后 6 次工具结果回填全被 CUR-RES-01 拒绝）。
//
// 契约：两条路径都降级交付**上游原始参数**，由客户端自行决定接受或报错。
func TestCursorRetypeFallbackKeepsStreamAlive(t *testing.T) {
	def := cursorTestBuildToolDef(t, "Write",
		`{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path","content"],"additionalProperties":false}`)
	defs := [][]byte{def}

	entries := map[string]*structpb.Value{
		"file_path": structpb.NewStringValue("/tmp/a.txt"),
		"content":   structpb.NewStringValue("hi"),
		"mode":      structpb.NewStringValue("overwrite"), // schema 未声明 → 重打必失败
	}
	mcpArgs := cursorTestBuildMcpArgsOfficial(t, "call_fb", "Write", entries)

	// ── 双工路径 ──
	execMsg := cursorAppendBytesField(nil, 11, mcpArgs)
	execParts, err := cursorDecodeProtobuf(execMsg)
	require.NoError(t, err)
	duplex := &cursorAgentDuplex{declaredToolDefs: defs}
	tc, err := duplex.cursorCaptureMcpArgs(execParts, 1, []byte("idf"))
	require.NoError(t, err, "重打失败必须降级，不能返回错误")
	require.JSONEq(t, `{"file_path":"/tmp/a.txt","content":"hi","mode":"overwrite"}`, tc.ArgsJSON,
		"降级时必须交付上游原始参数（不能被改成空参数）")

	// ── 独立解析路径 ──
	serverMsg := cursorAppendBytesField(nil, cursorFldServerExecMessage, execMsg)
	parser := cursorNewStreamParser("m")
	parser.attachToolSchemas(defs)
	_, err = parser.cursorParseMessageFrame(serverMsg)
	require.NoError(t, err, "解析路径同样必须降级，不能把帧判成失败")
	calls := parser.capturedToolCalls()
	require.Len(t, calls, 1)
	require.JSONEq(t, `{"file_path":"/tmp/a.txt","content":"hi","mode":"overwrite"}`, calls[0].ArgsJSON)
}
