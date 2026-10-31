//go:build unit

package service

// CURSOR-C 形状覆盖：11 类原生 exec 拒绝帧的逐字节协议形状 / 外层 f1·f15 回写 /
// stream_close / 拒绝文本逐字节 / 参数缺失仍拒绝 / 解码失败失败关闭 / 无声明工具文本 /
// 日志不泄露业务正文。
//
// 本文件刻意自成体系（不依赖 cursor_native_fs_*_test.go 等禁区测试辅助），只复用同包内
// 既有的可复用 helper：cursorTestBuildToolDef（cursor_mcpargs_retype_test.go）与
// captureDuplex（cursor_cross_http_writeback_test.go）。
//
// 所有期望字段号均取字面数字（取自派发单「已核实的协议事实」），不与实现常量互证。

import (
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// 业务正文特征串：只应出现在"协议回显"的结果帧里，绝不允许出现在日志中。
const (
	rejectSentinelCmd  = "ZZZCMD-AAA-ZZZ"
	rejectSentinelPath = "ZZZPATH-BBB-ZZZ"
	rejectSentinelURL  = "https://ZZZURL-CCC-ZZZ.example"
	rejectToolCallID   = "call-sentinel-real-1"
)

// cursorRejectObserveDuplex 构造带 observer logger 的 duplex（与 cursorNativeFsTestObserveDuplex
// 同形，但本文件自包含，不引用禁区测试辅助）。
func cursorRejectObserveDuplex(t *testing.T, defs [][]byte) (*cursorAgentDuplex, *observer.ObservedLogs, func() []byte) {
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

// cursorRejectExecParts 构造 ExecServerMessage 内层 parts：f1 exec_id + f15 id（有才写）+ 指定字段。
func cursorRejectExecParts(t *testing.T, execID uint64, idField []byte, field int, sub []byte) []cursorPBField {
	t.Helper()
	var parts []cursorPBField
	parts = append(parts, cursorPBField{Number: 1, Wire: cursorWireVarint, Varint: execID})
	if len(idField) > 0 {
		parts = append(parts, cursorPBField{Number: 15, Wire: cursorWireBytes, Bytes: idField})
	}
	parts = append(parts, cursorPBField{Number: field, Wire: cursorWireBytes, Bytes: sub})
	return parts
}

// cursorRejectSplitWire 把回写字节流切成逐条 envelope 的内层字段：ExecClientMessage 剥到内层
// 字段交回；ExecClientControlMessage（stream_close）按原字段交回。
func cursorRejectSplitWire(t *testing.T, wire []byte) [][]cursorPBField {
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

// cursorRejectLogContains 判断观测日志（message + 全部 context 字段）是否含子串。
func cursorRejectLogContains(logs *observer.ObservedLogs, substr string) bool {
	for _, e := range logs.All() {
		if strings.Contains(e.Message, substr) {
			return true
		}
		for _, f := range e.Context {
			if strings.Contains(f.String, substr) {
				return true
			}
		}
	}
	return false
}

// assertStreamClose 校验一条控制帧是 stream_close 且 id == execID。
func assertStreamClose(t *testing.T, ctrlFrame []cursorPBField, execID uint64) {
	t.Helper()
	cf, ok := cursorFindField(ctrlFrame, cursorFldClientControlMessage)
	require.True(t, ok, "控制帧必须含 f5 ExecClientControlMessage")
	inner, err := cursorDecodeProtobuf(cf.Bytes)
	require.NoError(t, err)
	sc, ok := cursorFindField(inner, cursorFldControlStreamClose)
	require.True(t, ok, "必须含 f1 stream_close")
	scInner, err := cursorDecodeProtobuf(sc.Bytes)
	require.NoError(t, err)
	require.EqualValues(t, execID, cursorVarintField(scInner, cursorFldStreamCloseID),
		"stream_close 的 id 必须等于 exec_id")
}

// 各 args 构造器（含业务正文特征串，用于验证"协议回显允许、日志禁止"）。
func rejectShellArgs(cmd, wd, tcid string, tcidField int) []byte {
	b := cursorAppendStringField(nil, 1, cmd)
	b = cursorAppendStringField(b, 2, wd)
	return cursorAppendStringField(b, tcidField, tcid)
}
func rejectFileArgs(path, tcid string, tcidField int) []byte {
	b := cursorAppendStringField(nil, 1, path)
	return cursorAppendStringField(b, tcidField, tcid)
}
func rejectFetchArgs(url, tcid string) []byte {
	b := cursorAppendStringField(nil, 1, url)
	return cursorAppendStringField(b, 2, tcid)
}
func rejectGrepArgs(tcid string) []byte {
	return cursorAppendStringField(nil, 14, tcid)
}

// rejectBackfill 是一条结果帧内层需要断言的"协议回显"字段。
type rejectBackfill struct {
	innerField int
	value      string
}

// rejectShapeCase 描述一类的期望形状。
type rejectShapeCase struct {
	name             string
	field            int // 输入/结果 oneof 同号
	oneofField       int // 结果消息内 rejected/error 子字段号
	reasonInnerField int // 内层 reason/error 文本字段号
	backfills        []rejectBackfill
	buildArgs        func() []byte
}

// TestCursorNativeExecRejectWireShape：11 类各自，逐字节断言外层 f1/f15、结果 oneof、子字段、
// 拒绝文本、回显字段、stream_close，并断言日志不泄露业务正文。
func TestCursorNativeExecRejectWireShape(t *testing.T) {
	declared := []string{"Bash", "read_file"}
	reason := cursorNativeExecRejectReason(declared)
	defs := [][]byte{
		cursorTestBuildToolDef(t, "Bash", `{"type":"object","properties":{"command":{"type":"string"}}}`),
		cursorTestBuildToolDef(t, "read_file", `{"type":"object","properties":{"path":{"type":"string"}}}`),
	}

	cases := []rejectShapeCase{
		{
			name: "shell_f2", field: 2, oneofField: 4, reasonInnerField: 3,
			backfills: []rejectBackfill{{1, rejectSentinelCmd}, {2, rejectSentinelPath}},
			buildArgs: func() []byte { return rejectShellArgs(rejectSentinelCmd, rejectSentinelPath, rejectToolCallID, 4) },
		},
		{
			name: "shell_stream_f14", field: 14, oneofField: 5, reasonInnerField: 3,
			backfills: []rejectBackfill{{1, rejectSentinelCmd}, {2, rejectSentinelPath}},
			buildArgs: func() []byte { return rejectShellArgs(rejectSentinelCmd, rejectSentinelPath, rejectToolCallID, 4) },
		},
		{
			name: "background_shell_spawn_f16", field: 16, oneofField: 3, reasonInnerField: 3,
			backfills: []rejectBackfill{{1, rejectSentinelCmd}, {2, rejectSentinelPath}},
			buildArgs: func() []byte { return rejectShellArgs(rejectSentinelCmd, rejectSentinelPath, rejectToolCallID, 3) },
		},
		{
			name: "write_f3", field: 3, oneofField: 6, reasonInnerField: 2,
			backfills: []rejectBackfill{{1, rejectSentinelPath}},
			buildArgs: func() []byte { return rejectFileArgs(rejectSentinelPath, rejectToolCallID, 3) },
		},
		{
			name: "delete_f4", field: 4, oneofField: 6, reasonInnerField: 2,
			backfills: []rejectBackfill{{1, rejectSentinelPath}},
			buildArgs: func() []byte { return rejectFileArgs(rejectSentinelPath, rejectToolCallID, 2) },
		},
		{
			name: "read_f7", field: 7, oneofField: 3, reasonInnerField: 2,
			backfills: []rejectBackfill{{1, rejectSentinelPath}},
			buildArgs: func() []byte { return rejectFileArgs(rejectSentinelPath, rejectToolCallID, 2) },
		},
		{
			name: "ls_f8", field: 8, oneofField: 3, reasonInnerField: 2,
			backfills: []rejectBackfill{{1, rejectSentinelPath}},
			buildArgs: func() []byte { return rejectFileArgs(rejectSentinelPath, rejectToolCallID, 3) },
		},
		{
			name: "diagnostics_f9", field: 9, oneofField: 3, reasonInnerField: 2,
			backfills: []rejectBackfill{{1, rejectSentinelPath}},
			buildArgs: func() []byte { return rejectFileArgs(rejectSentinelPath, rejectToolCallID, 2) },
		},
		{
			name: "grep_f5", field: 5, oneofField: 2, reasonInnerField: 1,
			backfills: nil,
			buildArgs: func() []byte { return rejectGrepArgs(rejectToolCallID) },
		},
		{
			name: "fetch_f20", field: 20, oneofField: 2, reasonInnerField: 2,
			backfills: []rejectBackfill{{1, rejectSentinelURL}},
			buildArgs: func() []byte { return rejectFetchArgs(rejectSentinelURL, rejectToolCallID) },
		},
		{
			name: "write_shell_stdin_f23", field: 23, oneofField: 2, reasonInnerField: 1,
			backfills: nil,
			buildArgs: func() []byte { return nil },
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			d, logs, stop := cursorRejectObserveDuplex(t, defs)
			execID := uint64(0xA1)
			idField := []byte("execid-bytes-01")
			parts := cursorRejectExecParts(t, execID, idField, tc.field, tc.buildArgs())
			require.NoError(t, d.cursorRejectNativeExec(parts, tc.field, execID, idField),
				"拒绝帧写出不得失败")
			wire := stop()
			frames := cursorRejectSplitWire(t, wire)

			var resultFrame, streamCloseFrame []cursorPBField
			for _, fr := range frames {
				if _, ok := cursorFindField(fr, cursorFldClientMsgID); ok {
					resultFrame = fr
				}
				if _, ok := cursorFindField(fr, cursorFldClientControlMessage); ok {
					streamCloseFrame = fr
				}
			}
			require.NotNil(t, resultFrame, "必须写出结果帧")

			// 外层 f1 = 原 exec_id（varint）。
			require.EqualValues(t, execID, cursorVarintField(resultFrame, cursorFldClientMsgID),
				"外层 f1 必须等于原 exec_id")
			// 外层 f15 = 原始 exec_id 字节（有才写，原样回写）。
			idF, ok := cursorFindField(resultFrame, cursorFldClientMsgExecID)
			require.True(t, ok, "外层 f15 必须原样回写")
			require.Equal(t, idField, idF.Bytes, "f15 必须逐字节等于原始 exec_id 字节")

			// 结果 oneof 字段号 == 输入字段号。
			rf, ok := cursorFindField(resultFrame, tc.field)
			require.True(t, ok, "结果帧必须含对应 oneof 字段 %d", tc.field)
			oneof, err := cursorDecodeProtobuf(rf.Bytes)
			require.NoError(t, err)
			require.Len(t, oneof, 1, "oneof 必须是单个子字段")
			require.Equal(t, tc.oneofField, oneof[0].Number, "oneof 子字段号必须等于协议事实")

			sub, err := cursorDecodeProtobuf(oneof[0].Bytes)
			require.NoError(t, err)
			rf2, ok := cursorFindField(sub, tc.reasonInnerField)
			require.True(t, ok, "内层必须含 reason/error 文本字段 %d", tc.reasonInnerField)
			require.Equal(t, reason, rf2.String(), "拒绝文本必须逐字节等于规定文本（含结尾句点）")

			for _, bf := range tc.backfills {
				bfF, ok := cursorFindField(sub, bf.innerField)
				require.True(t, ok, "内层必须回显字段 %d", bf.innerField)
				require.Equal(t, bf.value, bfF.String(), "回显值必须原样")
			}

			// 拒绝后必须紧跟 stream_close，且 id == exec_id。
			require.NotNil(t, streamCloseFrame, "拒绝后必须补 stream_close 帧")
			assertStreamClose(t, streamCloseFrame, execID)

			// 日志纪律：不得出现任何业务正文（command / path / url 的值）。
			require.False(t, cursorRejectLogContains(logs, rejectSentinelCmd),
				"日志不得含 command 值")
			require.False(t, cursorRejectLogContains(logs, rejectSentinelPath),
				"日志不得含 path 值")
			require.False(t, cursorRejectLogContains(logs, rejectSentinelURL),
				"日志不得含 url 值")
		})
	}
}

// TestCursorNativeExecRejectNoExecIDField：f15 为空时不得写 f15（有才写）。
func TestCursorNativeExecRejectNoExecIDField(t *testing.T) {
	defs := [][]byte{
		cursorTestBuildToolDef(t, "Bash", `{"type":"object","properties":{"command":{"type":"string"}}}`),
	}
	d, _, stop := cursorRejectObserveDuplex(t, defs)
	execID := uint64(7)
	parts := cursorRejectExecParts(t, execID, nil, 2, rejectShellArgs(rejectSentinelCmd, rejectSentinelPath, rejectToolCallID, 4))
	require.NoError(t, d.cursorRejectNativeExec(parts, 2, execID, nil))
	wire := stop()
	frames := cursorRejectSplitWire(t, wire)
	var resultFrame []cursorPBField
	for _, fr := range frames {
		if _, ok := cursorFindField(fr, cursorFldClientMsgID); ok {
			resultFrame = fr
		}
	}
	require.NotNil(t, resultFrame, "必须写出结果帧")
	_, ok := cursorFindField(resultFrame, cursorFldClientMsgExecID)
	require.False(t, ok, "f15 为空时不得写 f15")
}

// TestCursorNativeExecRejectMissingArgsStillRejects：参数缺失（无 tool_call_id / 无 command /
// 无 path）照样拒绝，不失败关闭，且结果帧 + stream_close 齐备。
func TestCursorNativeExecRejectMissingArgsStillRejects(t *testing.T) {
	defs := [][]byte{
		cursorTestBuildToolDef(t, "Bash", `{"type":"object","properties":{"command":{"type":"string"}}}`),
		cursorTestBuildToolDef(t, "read_file", `{"type":"object","properties":{"path":{"type":"string"}}}`),
	}
	declared := []string{"Bash", "read_file"}
	reason := cursorNativeExecRejectReason(declared)

	// ① 全部字段缺失：11 类各自以空 args 拒绝。
	for _, field := range []int{2, 3, 4, 5, 7, 8, 9, 14, 16, 20, 23} {
		field := field
		t.Run("empty_args_"+strconv.Itoa(field), func(t *testing.T) {
			d, _, stop := cursorRejectObserveDuplex(t, defs)
			execID := uint64(100 + uint64(field))
			parts := cursorRejectExecParts(t, execID, []byte("id"), field, nil)
			require.NoError(t, d.cursorRejectNativeExec(parts, field, execID, []byte("id")),
				"空 args 必须仍拒绝（不失败关闭）")
			wire := stop()
			frames := cursorRejectSplitWire(t, wire)
			var rf, sc []cursorPBField
			for _, fr := range frames {
				if _, ok := cursorFindField(fr, cursorFldClientMsgID); ok {
					rf = fr
				}
				if _, ok := cursorFindField(fr, cursorFldClientControlMessage); ok {
					sc = fr
				}
			}
			require.NotNil(t, rf, "空 args 也必须写出结果帧")
			require.NotNil(t, sc, "空 args 也必须补 stream_close")
			// 结果帧至少含 reason 文本（即使无 command/path/url 回显）。
			_, ok := cursorFindField(rf, field)
			require.True(t, ok, "结果帧必须含 oneof 字段 %d", field)
		})
	}

	// ② shell 缺 command（仍有 wd + tool_call_id）：仍拒绝，且内层不回显 command。
	t.Run("shell_missing_command", func(t *testing.T) {
		d, _, stop := cursorRejectObserveDuplex(t, defs)
		execID := uint64(200)
		// 只写 f2 wd 与 f4 tool_call_id，不写 f1 command。
		args := cursorAppendStringField(nil, 2, rejectSentinelPath)
		args = cursorAppendStringField(args, 4, rejectToolCallID)
		parts := cursorRejectExecParts(t, execID, []byte("id"), 2, args)
		require.NoError(t, d.cursorRejectNativeExec(parts, 2, execID, []byte("id")))
		wire := stop()
		frames := cursorRejectSplitWire(t, wire)
		var rf []cursorPBField
		for _, fr := range frames {
			if _, ok := cursorFindField(fr, cursorFldClientMsgID); ok {
				rf = fr
			}
		}
		require.NotNil(t, rf)
		res, ok := cursorFindField(rf, 2)
		require.True(t, ok, "结果帧必须含 f2 ShellResult")
		oneof, err := cursorDecodeProtobuf(res.Bytes)
		require.NoError(t, err)
		require.Len(t, oneof, 1, "ShellResult 必须仅含一个 oneof 子字段")
		sub, err := cursorDecodeProtobuf(oneof[0].Bytes)
		require.NoError(t, err)
		_, hasCmd := cursorFindField(sub, 1)
		require.False(t, hasCmd, "缺 command 时内层不得回显 command")
		rf2, ok := cursorFindField(sub, 3)
		require.True(t, ok)
		require.Equal(t, reason, rf2.String(), "缺参仍必须带拒绝文本")
	})

	// ③ write 缺 path（仍有 tool_call_id）：仍拒绝，且内层不回显 path。
	t.Run("write_missing_path", func(t *testing.T) {
		d, _, stop := cursorRejectObserveDuplex(t, defs)
		execID := uint64(300)
		args := cursorAppendStringField(nil, 3, rejectToolCallID) // 只写 f3 tool_call_id，不写 f1 path
		parts := cursorRejectExecParts(t, execID, []byte("id"), 3, args)
		require.NoError(t, d.cursorRejectNativeExec(parts, 3, execID, []byte("id")))
		wire := stop()
		frames := cursorRejectSplitWire(t, wire)
		var rf []cursorPBField
		for _, fr := range frames {
			if _, ok := cursorFindField(fr, cursorFldClientMsgID); ok {
				rf = fr
			}
		}
		require.NotNil(t, rf)
		res, ok := cursorFindField(rf, 3)
		require.True(t, ok, "结果帧必须含 f3 WriteResult")
		oneof, err := cursorDecodeProtobuf(res.Bytes)
		require.NoError(t, err)
		require.Len(t, oneof, 1, "WriteResult 必须仅含一个 oneof 子字段")
		sub, err := cursorDecodeProtobuf(oneof[0].Bytes)
		require.NoError(t, err)
		_, hasPath := cursorFindField(sub, 1)
		require.False(t, hasPath, "缺 path 时内层不得回显 path")
		rf2, ok := cursorFindField(sub, 2)
		require.True(t, ok)
		require.Equal(t, reason, rf2.String(), "缺参仍必须带拒绝文本")
	})
}

// TestCursorNativeExecRejectNoDeclaredTools：无声明工具时拒绝文本为规定文本，日志计数为 0。
func TestCursorNativeExecRejectNoDeclaredTools(t *testing.T) {
	d, logs, stop := cursorRejectObserveDuplex(t, nil)
	execID := uint64(400)
	parts := cursorRejectExecParts(t, execID, []byte("id"), 2, rejectShellArgs(rejectSentinelCmd, rejectSentinelPath, rejectToolCallID, 4))
	require.NoError(t, d.cursorRejectNativeExec(parts, 2, execID, []byte("id")))
	_ = stop()

	entries := logs.FilterField(zap.String("phase", "native_exec_rejected")).All()
	require.Len(t, entries, 1, "必须记一条拒绝日志")
	e := entries[0]
	require.Equal(t, "", e.ContextMap()["declared_tool_names"], "无声明工具时 declared_tool_names 为空")
	var count int
	for _, f := range e.Context {
		if f.Key == "declared_tool_name_count" {
			count = int(f.Integer)
		}
	}
	require.Equal(t, 0, count, "无声明工具时计数为 0")
}

// TestCursorNativeExecRejectDeclaredToolsComma：有声明工具时文本为逗号分隔的工具名。
func TestCursorNativeExecRejectDeclaredToolsComma(t *testing.T) {
	defs := [][]byte{
		cursorTestBuildToolDef(t, "Bash", `{"type":"object","properties":{"command":{"type":"string"}}}`),
		cursorTestBuildToolDef(t, "read_file", `{"type":"object","properties":{"path":{"type":"string"}}}`),
		cursorTestBuildToolDef(t, "Grep", `{"type":"object","properties":{"pattern":{"type":"string"}}}`),
	}
	d, logs, stop := cursorRejectObserveDuplex(t, defs)
	execID := uint64(500)
	parts := cursorRejectExecParts(t, execID, []byte("id"), 2, rejectShellArgs(rejectSentinelCmd, rejectSentinelPath, rejectToolCallID, 4))
	require.NoError(t, d.cursorRejectNativeExec(parts, 2, execID, []byte("id")))
	_ = stop()

	entries := logs.FilterField(zap.String("phase", "native_exec_rejected")).All()
	require.Len(t, entries, 1)
	e := entries[0]
	names, ok := e.ContextMap()["declared_tool_names"].(string)
	require.True(t, ok)
	require.Equal(t, "Bash,read_file,Grep", names, "声明工具名必须以逗号分隔、保序、去重")
}
