//go:build unit

package service

// CUR-SVC2：运输层失败关闭验收。
//
// 意外 native exec、未知 exec 字段、未知/缺 oneof interaction 一律返回既有协议错误，
// 由原 owner 终止；本层不写任何猜测结果/拒绝/stream_close 帧。前三个用例故意让 pipe
// 对端无人读取：若 respond 误触发 d.send，io.Pipe 的写会永久阻塞，故用 goroutine + 超时
// 证明 respond 立即返回、未尝试发送。

import (
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// cursorFailClosedDuplex 构造一个 pipe 对端**无人读取**的 duplex，专门验证失败关闭路径
// 不写帧（写帧会阻塞在 io.Pipe）。失败关闭不登记委托，declaredToolDefs 可为空。
func cursorFailClosedDuplex(t *testing.T) (*cursorAgentDuplex, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zap.DebugLevel)
	pr, pw := io.Pipe()
	d := &cursorAgentDuplex{
		reader:      pr,
		writer:      pw,
		delegations: newLocalCursorToolDelegations(),
		lg:          zap.New(core),
	}
	t.Cleanup(func() {
		_ = pw.Close()
		_ = pr.Close()
	})
	return d, logs
}

// cursorFailClosedRespond 在独立 goroutine 里调用 respond，并断言它在超时前立即返回错误；
// 若返回 nil 或超时（阻塞在 d.send）即判失败。
func cursorFailClosedRespond(t *testing.T, d *cursorAgentDuplex, payload []byte, errContains string, phase string, logs *observer.ObservedLogs) {
	t.Helper()
	type outcome struct {
		done bool
		err  error
	}
	ch := make(chan outcome, 1)
	go func() {
		done, err := d.respond(payload)
		ch <- outcome{done: done, err: err}
	}()
	select {
	case got := <-ch:
		require.Error(t, got.err, "失败关闭必须返回既有协议错误")
		require.Contains(t, got.err.Error(), errContains)
		require.False(t, got.done, "失败关闭不由本层标记终态")
	case <-time.After(2 * time.Second):
		t.Fatal("respond 阻塞：失败关闭路径不得调用 d.send（pipe 对端无人读取）")
	}
	if phase != "" {
		require.Equal(t, 1, phaseCount(logs, phase), "必须记一条 %s 取证日志", phase)
	}
}

// TestCursorSDKTransportUnexpectedNativeFailsClosed 证明意外 native exec（f2 shell）返回错误、
// 不写任何拒绝/结果帧，且只记 exec_id/field（不含参数）。
func TestCursorSDKTransportUnexpectedNativeFailsClosed(t *testing.T) {
	d, logs := cursorFailClosedDuplex(t)

	execMsg := cursorAppendVarintField(nil, 1, 7)
	execMsg = cursorAppendBytesField(execMsg, 15, []byte("exec-native"))
	execMsg = cursorAppendBytesField(execMsg, 2, []byte("sensitive-shell-args-must-not-be-logged"))
	payload := cursorAppendBytesField(nil, cursorFldServerExecMessage, execMsg)

	cursorFailClosedRespond(t, d, payload, "unexpected native exec field 2", "native_exec_unexpected", logs)
	require.Empty(t, d.delegations.snapshot(), "native exec 不得登记 MCP 委托")
	// 日志只含 exec_id / field，不得含参数正文。
	for _, e := range logs.All() {
		if e.ContextMap()["phase"] == "native_exec_unexpected" {
			require.NotContains(t, e.Message, "sensitive-shell-args-must-not-be-logged")
			require.Contains(t, e.ContextMap(), "exec_id")
			require.Contains(t, e.ContextMap(), "field")
		}
	}
}

// TestCursorSDKTransportUnknownExecFailsClosed 证明未知 exec 字段（bytes 与 varint 两种 wire）
// 一律返回错误、零回包，不再回通用未知字段帧、不再静默忽略 varint 未知字段。
func TestCursorSDKTransportUnknownExecFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field []byte
	}{
		{"bytes", cursorAppendBytesField(nil, 17, []byte("list-mcp-resources-args"))},
		{"varint", cursorAppendVarintField(nil, 40, 123)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, logs := cursorFailClosedDuplex(t)

			execMsg := cursorAppendVarintField(nil, 1, 7)
			execMsg = cursorAppendBytesField(execMsg, 15, []byte("exec-unknown"))
			execMsg = append(execMsg, tc.field...)
			payload := cursorAppendBytesField(nil, cursorFldServerExecMessage, execMsg)

			cursorFailClosedRespond(t, d, payload, "unsupported exec fields", "exec_unsupported_fields", logs)
		})
	}
}

// TestCursorSDKTransportUnknownInteractionFailsClosed 证明未知 oneof 字段与缺 oneof 的
// interaction query 都返回既有协议错误、不写任何通用 rejected 帧。
func TestCursorSDKTransportUnknownInteractionFailsClosed(t *testing.T) {
	cases := []struct {
		name  string
		query []byte
		err   string
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
			d, logs := cursorFailClosedDuplex(t)
			payload := cursorAppendBytesField(nil, cursorFldServerInteractionQuery, tc.query)
			cursorFailClosedRespond(t, d, payload, tc.err, "interaction_query_unsupported", logs)
		})
	}
}

// TestCursorSDKTransportMcpStateEchoesF4Bytes 证明 f36 mcp_state 回包 f5 逐字节回显
// 请求 f4 的单入口 McpToolDefinition 原始字节（字节级保真，不经重新编码）。
func TestCursorSDKTransportMcpStateEchoesF4Bytes(t *testing.T) {
	initBody, err := cursorConvertRequest(&apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools: []apicompat.ChatTool{{Type: "function", Function: &apicompat.ChatFunction{
			Name:       "get_weather",
			Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}}},
	}, "grok-4.7")
	require.NoError(t, err)

	d, err := newCursorAgentDuplex(initBody, nil)
	require.NoError(t, err)
	defer d.close(nil)

	// f4 单入口原始字节 == duplex 保存的 declaredToolDefs。
	run := cursorAgentRunOf(t, initBody)
	mcp, ok := cursorFindField(run, cursorFldRunMcpTools)
	require.True(t, ok)
	mcpFields, err := cursorDecodeProtobuf(mcp.Bytes)
	require.NoError(t, err)
	defs := cursorAllFields(mcpFields, cursorFldMcpToolsTool)
	require.Len(t, defs, 1, "单入口运输：f4 只有一条 McpToolDefinition")
	require.Equal(t, defs[0].Bytes, d.declaredToolDefs[0])

	// f36 mcp_state 帧（respond 吃 envelope 剥离后的 payload）。
	exec := cursorAppendVarintField(nil, 1, 7)
	exec = cursorAppendBytesField(exec, 15, []byte("exec-mcp-state"))
	exec = cursorAppendBytesField(exec, 36, nil)
	payload := cursorAppendBytesField(nil, cursorFldServerExecMessage, exec)

	// 本用例会写帧，必须并发读取 pipe（否则 d.send 阻塞）。
	replyCh := cursorDuplexReadReplyAsync(d)
	done, err := d.respond(payload)
	require.NoError(t, err)
	require.False(t, done)
	rep := <-replyCh
	require.NoError(t, rep.err)

	outer, err := cursorDecodeProtobuf(rep.b)
	require.NoError(t, err)
	msg, ok := cursorFindField(outer, cursorFldClientMessage)
	require.True(t, ok)
	msgFields, err := cursorDecodeProtobuf(msg.Bytes)
	require.NoError(t, err)
	state, ok := cursorFindField(msgFields, 36)
	require.True(t, ok)
	stateFields, err := cursorDecodeProtobuf(state.Bytes)
	require.NoError(t, err)
	success, ok := cursorFindField(stateFields, 1)
	require.True(t, ok)
	successFields, err := cursorDecodeProtobuf(success.Bytes)
	require.NoError(t, err)
	server, ok := cursorFindField(successFields, 1)
	require.True(t, ok)
	serverFields, err := cursorDecodeProtobuf(server.Bytes)
	require.NoError(t, err)
	tools := cursorAllFields(serverFields, 5)
	require.Len(t, tools, 1)
	require.Equal(t, d.declaredToolDefs[0], tools[0].Bytes,
		"f5 必须逐字节回显 f4 的 McpToolDefinition 原始字节")
}
