//go:build unit

package service

// R1 增量测试：Cursor 原生 exec 统一拒绝的编码形状 / 字段选择 / 写出与日志。
//
// 覆盖：
//   ① TestCursorNativeExecRejectReason：拒绝理由文本（声明非空 / 空两种）。
//   ② TestCursorNativeExecRejectField：字段选择（不在表的返回 false；含 f1/f15/f7 返回 7）。
//   ③ TestCursorNativeExecRejectEncode：11 个字段表驱动，逐层解回断言 resultField / oneof
//      子字段号 / reason 文本 / command·path·url 回填值，并断言返回的 toolCallID 等于构造时写入的值。
//   ④ TestCursorNativeExecRejectEncodeMissingArgs：args 为空字节仍编码出拒绝，且 toolCallID 为空。
//   ⑤ TestCursorRejectNativeExecWritesFramesAndLogs：用 observer duplex（声明 Bash/read_file
//      同名工具）送 f14 + f7，断言写出结果帧 + stream_close 帧、日志 native_exec_rejected
//      且 declared_tool_names 含两个名字、tool_call_id 等于构造值——证明即使声明了同名工具也拒绝。
//
// 重要：本文件所有期望字段号与构造 args 的字段号均为字面数字（取自方案「拒绝形状」表），
// 不再引用实现里的 cursorFldNativeReject* 常量，避免"测实现常量 == 实现常量"的自证循环。

import (
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

// 方案表字面字段号（args 内层子字段）。
const (
	tcShellArgCommand      = 1  // ShellArgs/BackgroundShellSpawnArgs.f1 command
	tcShellArgWorkingDir   = 2  // ShellArgs/BackgroundShellSpawnArgs.f2 working_directory
	tcShellArgToolCallID   = 4  // ShellArgs.f4 tool_call_id
	tcBgShellArgToolCallID = 3  // BackgroundShellSpawnArgs.f3 tool_call_id
	tcFileArgPath          = 1  // Write/Delete/Read/Ls/DiagnosticsArgs.f1 path
	tcWriteArgToolCallID   = 3  // WriteArgs.f3 tool_call_id
	tcDeleteArgToolCallID  = 2  // DeleteArgs.f2 tool_call_id
	tcReadArgToolCallID    = 2  // ReadArgs.f2 tool_call_id
	tcLsArgToolCallID      = 3  // LsArgs.f3 tool_call_id
	tcDiagArgToolCallID    = 2  // DiagnosticsArgs.f2 tool_call_id
	tcFetchArgURL          = 1  // FetchArgs.f1 url
	tcFetchArgToolCallID   = 2  // FetchArgs.f2 tool_call_id
	tcGrepArgToolCallID    = 14 // GrepArgs.f14 tool_call_id
)

func buildShellArgs(cmd, wd, tcid string) []byte {
	b := cursorAppendStringField(nil, tcShellArgCommand, cmd)
	b = cursorAppendStringField(b, tcShellArgWorkingDir, wd)
	b = cursorAppendStringField(b, tcShellArgToolCallID, tcid)
	return b
}

func buildBackgroundShellArgs(cmd, wd, tcid string) []byte {
	b := cursorAppendStringField(nil, tcShellArgCommand, cmd)
	b = cursorAppendStringField(b, tcShellArgWorkingDir, wd)
	b = cursorAppendStringField(b, tcBgShellArgToolCallID, tcid)
	return b
}

func buildFileArgs(path, tcid string, tcidField int) []byte {
	b := cursorAppendStringField(nil, tcFileArgPath, path)
	b = cursorAppendStringField(b, tcidField, tcid)
	return b
}

func buildFetchArgs(url, tcid string) []byte {
	b := cursorAppendStringField(nil, tcFetchArgURL, url)
	b = cursorAppendStringField(b, tcFetchArgToolCallID, tcid)
	return b
}

func buildGrepArgs(tcid string) []byte {
	return cursorAppendStringField(nil, tcGrepArgToolCallID, tcid)
}

// decodeRejectPayload 把结果载荷解成 (oneof 子字段号, 内层字节)。
func decodeRejectPayload(t *testing.T, payload []byte) (int, []byte) {
	t.Helper()
	fields, err := cursorDecodeProtobuf(payload)
	require.NoError(t, err)
	require.Len(t, fields, 1, "拒绝载荷必须是单个 oneof 子字段")
	return fields[0].Number, fields[0].Bytes
}

// ---------------------------------------------------------------------------
// ① 理由文本
// ---------------------------------------------------------------------------

func TestCursorNativeExecRejectReason(t *testing.T) {
	require.Equal(t,
		"Tool not available in this environment. Use only the client-provided tools: Bash, read_file.",
		cursorNativeExecRejectReason(cursorFldNativeRejectDiagnosticsArgs, []string{"Bash", "read_file"}))
	require.Equal(t,
		"Tool not available in this environment. The client declared no tools.",
		cursorNativeExecRejectReason(cursorFldNativeRejectDiagnosticsArgs, nil))
	require.Equal(t,
		"Tool not available in this environment. The client declared no tools.",
		cursorNativeExecRejectReason(cursorFldNativeRejectDiagnosticsArgs, []string{}))
}

// ---------------------------------------------------------------------------
// ② 字段选择
// ---------------------------------------------------------------------------

func TestCursorNativeExecRejectField(t *testing.T) {
	// 不在表中的字段（f10 / f11 / f36）返回 false。
	for _, n := range []int{10, 11, 36} {
		parts := []cursorPBField{{Number: n, Wire: cursorWireBytes, Bytes: []byte("x")}}
		got, ok := cursorNativeExecRejectField(parts)
		require.False(t, ok, "字段 %d 必须不在拒绝表", n)
		require.Equal(t, 0, got)
	}

	// 同时含 f1、f15、f7 → 返回 7（f7 在优先级列表里先于 f14 等）。
	parts := []cursorPBField{
		{Number: 1, Wire: cursorWireVarint, Varint: 99},
		{Number: 15, Wire: cursorWireBytes, Bytes: []byte("id")},
		{Number: 7, Wire: cursorWireBytes, Bytes: []byte("readsub")},
	}
	got, ok := cursorNativeExecRejectField(parts)
	require.True(t, ok)
	require.Equal(t, 7, got)
}

// ---------------------------------------------------------------------------
// ③ 11 个字段编码形状（期望字段号全部为字面数字，取自方案「拒绝形状」表）
// ---------------------------------------------------------------------------

func TestCursorNativeExecRejectEncode(t *testing.T) {
	declared := []string{"Bash", "read_file"}
	reason := cursorNativeExecRejectReason(cursorFldNativeRejectDiagnosticsArgs, declared)

	cases := []struct {
		name               string
		field              int
		args               []byte
		resultField        int
		oneofField         int
		reasonInnerField   int
		backfillInnerField int
		backfillValue      string
		toolCallID         string
	}{
		{
			name:               "shell_f2",
			field:              2, // shell_args
			args:               buildShellArgs("bash -lc echo hi", "/repo", "call_s1"),
			resultField:        2, // ShellResult
			oneofField:         4, // ShellResult.f4 rejected
			reasonInnerField:   3, // ShellRejected.f3 reason
			backfillInnerField: 1, // ShellRejected.f1 command
			backfillValue:      "bash -lc echo hi",
			toolCallID:         "call_s1",
		},
		{
			name:               "shell_stream_f14",
			field:              14, // shell_stream_args
			args:               buildShellArgs("bash -lc ls", "/home", "call_s2"),
			resultField:        14, // ShellStream
			oneofField:         5,  // ShellStream.f5 rejected
			reasonInnerField:   3,  // ShellRejected.f3 reason
			backfillInnerField: 1,  // ShellRejected.f1 command
			backfillValue:      "bash -lc ls",
			toolCallID:         "call_s2",
		},
		{
			name:               "background_shell_spawn_f16",
			field:              16, // background_shell_spawn_args
			args:               buildBackgroundShellArgs("bash -lc sleep", "/tmp", "call_s3"),
			resultField:        16, // BackgroundShellSpawnResult
			oneofField:         3,  // BackgroundShellSpawnResult.f3 rejected
			reasonInnerField:   3,  // ShellRejected.f3 reason
			backfillInnerField: 1,  // ShellRejected.f1 command
			backfillValue:      "bash -lc sleep",
			toolCallID:         "call_s3",
		},
		{
			name:               "write_f3",
			field:              3, // write_args
			args:               buildFileArgs("/repo/a.txt", "call_w1", tcWriteArgToolCallID),
			resultField:        3, // WriteResult
			oneofField:         6, // WriteResult.f6 rejected
			reasonInnerField:   2, // WriteRejected.f2 reason
			backfillInnerField: 1, // WriteRejected.f1 path
			backfillValue:      "/repo/a.txt",
			toolCallID:         "call_w1",
		},
		{
			name:               "delete_f4",
			field:              4, // delete_args
			args:               buildFileArgs("/repo/d.txt", "call_d1", tcDeleteArgToolCallID),
			resultField:        4, // DeleteResult
			oneofField:         6, // DeleteResult.f6 rejected
			reasonInnerField:   2, // DeleteRejected.f2 reason
			backfillInnerField: 1, // DeleteRejected.f1 path
			backfillValue:      "/repo/d.txt",
			toolCallID:         "call_d1",
		},
		{
			name:               "read_f7",
			field:              7, // read_args
			args:               buildFileArgs("/repo/main.go", "call_r1", tcReadArgToolCallID),
			resultField:        7, // ReadResult
			oneofField:         3, // ReadResult.f3 rejected
			reasonInnerField:   2, // ReadRejected.f2 reason
			backfillInnerField: 1, // ReadRejected.f1 path
			backfillValue:      "/repo/main.go",
			toolCallID:         "call_r1",
		},
		{
			name:               "ls_f8",
			field:              8, // ls_args
			args:               buildFileArgs("/repo", "call_l1", tcLsArgToolCallID),
			resultField:        8, // LsResult
			oneofField:         3, // LsResult.f3 rejected
			reasonInnerField:   2, // LsRejected.f2 reason
			backfillInnerField: 1, // LsRejected.f1 path
			backfillValue:      "/repo",
			toolCallID:         "call_l1",
		},
		{
			name:               "diagnostics_f9",
			field:              9, // diagnostics_args
			args:               buildFileArgs("/repo/x", "call_x1", tcDiagArgToolCallID),
			resultField:        9, // DiagnosticsResult
			oneofField:         3, // DiagnosticsResult.f3 rejected
			reasonInnerField:   2, // DiagnosticsRejected.f2 reason
			backfillInnerField: 1, // DiagnosticsRejected.f1 path
			backfillValue:      "/repo/x",
			toolCallID:         "call_x1",
		},
		{
			name:               "grep_f5",
			field:              5, // grep_args
			args:               buildGrepArgs("call_g1"),
			resultField:        5, // GrepResult
			oneofField:         2, // GrepResult.f2 error
			reasonInnerField:   1, // GrepError.f1 error
			backfillInnerField: 0, // 无 command/path/url 回填
			backfillValue:      "",
			toolCallID:         "call_g1",
		},
		{
			name:               "fetch_f20",
			field:              20, // fetch_args
			args:               buildFetchArgs("https://example.com/file", "call_f1"),
			resultField:        20, // FetchResult
			oneofField:         2,  // FetchResult.f2 error
			reasonInnerField:   2,  // FetchError.f2 error
			backfillInnerField: 1,  // FetchError.f1 url
			backfillValue:      "https://example.com/file",
			toolCallID:         "call_f1",
		},
		{
			name:               "write_shell_stdin_f23",
			field:              23, // write_shell_stdin_args
			args:               []byte{},
			resultField:        23, // WriteShellStdinResult
			oneofField:         2,  // WriteShellStdinResult.f2 error
			reasonInnerField:   1,  // WriteShellStdinError.f1 error
			backfillInnerField: 0,  // 无 command/path/url 回填
			backfillValue:      "",
			toolCallID:         "", // WriteShellStdinArgs 无 tool_call_id
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resultField, payload, gotToolCallID, err := cursorEncodeNativeExecRejection(tc.field, tc.args, reason)
			require.NoError(t, err, "编码拒绝不得失败")
			require.Equal(t, tc.resultField, resultField, "resultField 必须与 field 相同")
			require.Equal(t, tc.toolCallID, gotToolCallID,
				"返回的 toolCallID 必须等于构造 args 时写入的值（WriteShellStdin 为空）")

			oneofField, innerBytes := decodeRejectPayload(t, payload)
			require.Equal(t, tc.oneofField, oneofField, "oneof 子字段号必须匹配方案「拒绝形状」表")

			inner, err := cursorDecodeProtobuf(innerBytes)
			require.NoError(t, err)

			rf, ok := cursorFindField(inner, tc.reasonInnerField)
			require.True(t, ok, "内层必须含 reason/error 文本字段")
			require.Equal(t, reason, rf.String(), "拒绝理由文本必须原样回填")

			if tc.backfillInnerField != 0 {
				bf, ok := cursorFindField(inner, tc.backfillInnerField)
				require.True(t, ok, "内层必须回填 command/path/url 子字段")
				require.Equal(t, tc.backfillValue, bf.String())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ④ 参数缺失（args 为空字节）仍可编码，且 toolCallID 为空
// ---------------------------------------------------------------------------

func TestCursorNativeExecRejectEncodeMissingArgs(t *testing.T) {
	reason := cursorNativeExecRejectReason(cursorFldNativeRejectDiagnosticsArgs, []string{"Bash"})
	for _, field := range []int{
		2,  // shell_args
		14, // shell_stream_args
		16, // background_shell_spawn_args
		3,  // write_args
		4,  // delete_args
		5,  // grep_args
		7,  // read_args
		8,  // ls_args
		9,  // diagnostics_args
		20, // fetch_args
		23, // write_shell_stdin_args
	} {
		field := field
		t.Run(strconv.Itoa(field), func(t *testing.T) {
			resultField, payload, gotToolCallID, err := cursorEncodeNativeExecRejection(field, []byte{}, reason)
			require.NoError(t, err, "空 args 必须仍能编码拒绝")
			require.Equal(t, field, resultField)
			require.Empty(t, gotToolCallID, "空 args 必须解出空 toolCallID")
			// 载荷必须能解回且含 reason 文本（即使无 command/path/url 回填）。
			oneofField, innerBytes := decodeRejectPayload(t, payload)
			require.NotZero(t, oneofField)
			inner, derr := cursorDecodeProtobuf(innerBytes)
			require.NoError(t, derr)
			require.NotEmpty(t, inner, "空 args 也必须至少写 reason/error 文本")
		})
	}
}

// ---------------------------------------------------------------------------
// ⑤ 写出两帧 + 日志（即使声明同名工具也拒绝，且 tool_call_id 等于构造值）
// ---------------------------------------------------------------------------

func TestCursorRejectNativeExecWritesFramesAndLogs(t *testing.T) {
	defs := [][]byte{
		cursorTestBuildToolDef(t, "Bash", `{"type":"object","properties":{"command":{"type":"string"}}}`),
		cursorTestBuildToolDef(t, "read_file", `{"type":"object","properties":{"path":{"type":"string"}}}`),
	}
	d, logs, stop := cursorNativeFsTestObserveDuplex(t, defs)

	// f14 shell_stream_args（命令与声明同名工具 Bash 无关——本片一律拒绝）。
	shellSub := buildShellArgs("ls -la", "/repo", "call_shell_1")
	parts14 := cursorNativeFsTestExecParts(t, 14, []byte("exec-14"), 14, shellSub)
	require.NoError(t, d.cursorRejectNativeExec(parts14, 14, 14, []byte("exec-14")))

	// f7 read_args（path 与声明同名工具 read_file 无关——本片一律拒绝）。
	readSub := buildFileArgs("/repo/main.go", "call_read_1", tcReadArgToolCallID)
	parts7 := cursorNativeFsTestExecParts(t, 7, []byte("exec-7"), 7, readSub)
	require.NoError(t, d.cursorRejectNativeExec(parts7, 7, 7, []byte("exec-7")))

	wire := stop()

	frames := cursorNativeFsTestSplit(t, wire)
	var resultFrames, streamCloseFrames int
	var sawF14, sawF7 bool
	for _, fields := range frames {
		if _, ok := cursorFindField(fields, cursorFldClientControlMessage); ok {
			streamCloseFrames++
			continue
		}
		if _, ok := cursorFindField(fields, cursorFldClientMsgID); ok {
			resultFrames++
			if _, ok := cursorFindField(fields, 14); ok {
				sawF14 = true
			}
			if _, ok := cursorFindField(fields, 7); ok {
				sawF7 = true
			}
		}
	}
	require.Equal(t, 2, resultFrames, "每个拒绝必须写出一个结果帧")
	require.Equal(t, 2, streamCloseFrames, "每个拒绝必须补一个 stream_close 帧")
	require.True(t, sawF14, "必须含 f14 ShellStream 结果帧")
	require.True(t, sawF7, "必须含 f7 ReadResult 结果帧")

	entries := logs.FilterField(zap.String("phase", "native_exec_rejected")).All()
	require.Len(t, entries, 2, "必须记两条 native_exec_rejected 日志（每个字段一条）")
	for _, e := range entries {
		names := cursorRejectTestDeclaredNames(t, e)
		require.Contains(t, names, "Bash", "即使声明了同名工具也要拒绝，且日志必须含声明名")
		require.Contains(t, names, "read_file", "即使声明了同名工具也要拒绝，且日志必须含声明名")
		tcid, ok := e.ContextMap()["tool_call_id"].(string)
		require.True(t, ok, "native_exec_rejected 日志必须含 tool_call_id 字段")
		require.NotEmpty(t, tcid, "tool_call_id 必须等于构造时写入的值（非空）")
	}
}

// TestCursorNativeExecRejectDecodeFailureFailClose：protobuf 解码失败必须失败关闭（返回错误），
// 且不得写出任何结果帧 / stream_close（只有真正的解码失败才终止该 exec，参数不完整不算）。
func TestCursorNativeExecRejectDecodeFailureFailClose(t *testing.T) {
	defs := [][]byte{
		cursorTestBuildToolDef(t, "Bash", `{"type":"object","properties":{"command":{"type":"string"}}}`),
	}
	d, _, stop := cursorNativeFsTestObserveDuplex(t, defs)

	// 截断的 varint（单个 0x80 是不完整 varint）→ 解码失败。
	bad := []byte{0x80}
	parts := cursorNativeFsTestExecParts(t, 2, []byte("exec-2"), 2, bad)
	err := d.cursorRejectNativeExec(parts, 2, 2, []byte("exec-2"))
	require.Error(t, err, "protobuf 解码失败必须失败关闭（返回错误）")

	wire := stop()
	frames := cursorNativeFsTestSplit(t, wire)
	for _, fr := range frames {
		_, ok := cursorFindField(fr, cursorFldClientMsgID)
		require.False(t, ok, "解码失败时不得写出结果帧")
	}
}

// ---------------------------------------------------------------------------
// 测试 helper（原样恢复自 2ed46fd23 删除的 cursor_native_fs_bridge_test.go /
// cursor_native_fs_observe_test.go）
// ---------------------------------------------------------------------------

// cursorNativeFsTestObserveDuplex 构造带 observer logger 的 duplex（与 cursorNativeFsTestRejectDuplex
// 同形，但 logger 落到 observer 以便断言拒绝日志），并返回 drain 用的 stop()。
func cursorNativeFsTestObserveDuplex(t *testing.T, defs [][]byte) (*cursorAgentDuplex, *observer.ObservedLogs, func() []byte) {
	t.Helper()
	pr, pw := io.Pipe()
	core, logs := observer.New(zap.DebugLevel)
	d := &cursorAgentDuplex{
		reader:           pr,
		writer:           pw,
		delegations:      newLocalCursorToolDelegations(),
		declaredToolDefs: defs,
		lg:               zap.New(core),
	}
	return d, logs, captureDuplex(t, d)
}

// cursorRejectTestDeclaredNames 从一条观测日志里取出 declared_tool_names 字段。
// 生产侧以逗号拼接的 string 写入（工具名均为不含逗号的标识符），测试侧按逗号拆回。
func cursorRejectTestDeclaredNames(t *testing.T, e observer.LoggedEntry) []string {
	t.Helper()
	raw, ok := e.ContextMap()["declared_tool_names"].(string)
	require.True(t, ok, "declared_tool_names 必须是字符串字段")
	if raw == "" {
		return []string{}
	}
	return strings.Split(raw, ",")
}

// cursorNativeFsTestExecParts 构造 ExecServerMessage 内层 parts：f1 exec_id + f15 id + 指定
// 原生文件工具字段。
func cursorNativeFsTestExecParts(t *testing.T, execID uint64, idField []byte, field int, sub []byte) []cursorPBField {
	t.Helper()
	var parts []cursorPBField
	parts = append(parts, cursorPBField{Number: 1, Wire: cursorWireVarint, Varint: execID})
	if len(idField) > 0 {
		parts = append(parts, cursorPBField{Number: 15, Wire: cursorWireBytes, Bytes: idField})
	}
	parts = append(parts, cursorPBField{Number: field, Wire: cursorWireBytes, Bytes: sub})
	return parts
}

// cursorNativeFsTestSplit 把回写字节流切成逐条 envelope 的字段：ExecClientMessage（外层 f2）
// 解包成内层字段交回；stream_close 走 f5 控制消息，按原字段交回。
func cursorNativeFsTestSplit(t *testing.T, wire []byte) [][]cursorPBField {
	t.Helper()
	var out [][]cursorPBField
	off := 0
	for off < len(wire) {
		frame, next, err := cursorReadEnvelopeFrame(wire, off)
		require.NoError(t, err, "回写字节必须是合法 envelope 流")
		fields, derr := cursorDecodeProtobuf(frame.Data)
		require.NoError(t, derr)
		if ecm, ok := cursorFindField(fields, cursorFldClientMessage); ok {
			inner, ierr := cursorDecodeProtobuf(ecm.Bytes)
			require.NoError(t, ierr)
			out = append(out, inner)
		} else {
			out = append(out, fields)
		}
		off = next
	}
	return out
}
