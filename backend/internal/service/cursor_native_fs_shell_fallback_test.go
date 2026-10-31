//go:build unit

package service

// NSFS-02：原生文件工具缺席（客户端只声明 shell 类工具，没有原生 read/write/ls/grep/delete）
// 时，把上游下发的原生 exec（f3/f4/f5/f7/f8）转译成 shell 命令交给客户端执行，结果仍按原生
// 通道回写上游。本文件覆盖：五种 kind 的 shell 回退命令构造、原生工具优先、工具彻底缺席时
// 仍走既有 Absent 拒绝帧、以及回写必须落在原生 Result 帧（而非 ShellResult）。

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// cursorShellFallbackSchema 是 shell 类工具的 schema（command:string），用于"只声明 shell"场景。
const cursorShellFallbackSchema = `{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`

// shellFallbackCommandFromCapture 解构捕获结果里的 shell 命令（ArgsJSON 的 command 字段）。
func shellFallbackCommandFromCapture(t *testing.T, tc cursorToolCallCapture) string {
	t.Helper()
	var obj map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.ArgsJSON), &obj), "ArgsJSON 必须是合法 JSON")
	cmd, ok := obj["command"].(string)
	require.True(t, ok, "shell 回退的 ArgsJSON 必须含 command 字段: %s", tc.ArgsJSON)
	return cmd
}

// shellFallbackNewDuplex 构造只声明单个 shell 类工具的 duplex（模拟 Codex 只声明 shell 的生产实测）。
func shellFallbackNewDuplex(t *testing.T) *cursorAgentDuplex {
	defs := [][]byte{cursorTestBuildToolDef(t, "shell", cursorShellFallbackSchema)}
	return cursorShellTestNewDuplex(t, defs, "")
}

// TestCursorNativeFsShellFallbackRead：声明集只有 shell（schema command:string），收到 f7 read
// 委托（path /tmp/a.txt）→ 捕获成功、Name=="shell"、ArgsJSON 解出的 command 含
// `cat -- '/tmp/a.txt'`、ResultArgsJSON 含 /tmp/a.txt。
func TestCursorNativeFsShellFallbackRead(t *testing.T) {
	d := shellFallbackNewDuplex(t)

	sub := cursorAppendStringField(nil, 1, "/tmp/a.txt") // f1 path
	sub = cursorAppendStringField(sub, 2, "call_read_1") // f2 tool_call_id
	parts := cursorNativeFsTestExecParts(t, 81, []byte("exec-81"), cursorFldServerMsgReadArgs, sub)

	tc, err := d.cursorCaptureNativeFsArgs(parts, cursorFldServerMsgReadArgs, 81, []byte("exec-81"))
	require.NoError(t, err)
	require.Equal(t, cursorToolKindRead, tc.Kind)
	require.Equal(t, "shell", tc.Name, "无原生 read 工具时必须回退到 shell")
	cmd := shellFallbackCommandFromCapture(t, tc)
	require.Contains(t, cmd, "cat -- '/tmp/a.txt'", "read 必须转译为 cat 命令")
	require.Contains(t, tc.ResultArgsJSON, "/tmp/a.txt", "ResultArgsJSON 必须含原生 path 供回写")
}

// TestCursorNativeFsShellFallbackGrep：同上前提，f5 grep 委托（pattern TODO、path /repo）→
// command 形如 `grep -rn -- 'TODO' '/repo'`。
func TestCursorNativeFsShellFallbackGrep(t *testing.T) {
	d := shellFallbackNewDuplex(t)

	sub := cursorAppendStringField(nil, 1, "TODO")        // f1 pattern
	sub = cursorAppendStringField(sub, 2, "/repo")        // f2 path
	sub = cursorAppendStringField(sub, 14, "call_grep_1") // f14 tool_call_id
	parts := cursorNativeFsTestExecParts(t, 82, nil, cursorFldServerMsgGrepArgs, sub)

	tc, err := d.cursorCaptureNativeFsArgs(parts, cursorFldServerMsgGrepArgs, 82, nil)
	require.NoError(t, err)
	require.Equal(t, cursorToolKindGrep, tc.Kind)
	require.Equal(t, "shell", tc.Name)
	cmd := shellFallbackCommandFromCapture(t, tc)
	require.Contains(t, cmd, "grep -rn -- 'TODO' '/repo'", "grep 默认 content 形态必须是 -rn")
}

// TestCursorNativeFsShellFallbackLs：ls → `ls -la -- ...`。
func TestCursorNativeFsShellFallbackLs(t *testing.T) {
	d := shellFallbackNewDuplex(t)

	sub := cursorAppendStringField(nil, 1, "/repo")    // f1 path
	sub = cursorAppendStringField(sub, 3, "call_ls_1") // f3 tool_call_id
	parts := cursorNativeFsTestExecParts(t, 83, nil, cursorFldServerMsgLsArgs, sub)

	tc, err := d.cursorCaptureNativeFsArgs(parts, cursorFldServerMsgLsArgs, 83, nil)
	require.NoError(t, err)
	require.Equal(t, cursorToolKindLs, tc.Kind)
	require.Equal(t, "shell", tc.Name)
	cmd := shellFallbackCommandFromCapture(t, tc)
	require.Contains(t, cmd, "ls -la -- '/repo'", "ls 必须转译为 ls -la 命令")
}

// TestCursorNativeFsShellFallbackDelete：delete → `rm -f -- ...`。
func TestCursorNativeFsShellFallbackDelete(t *testing.T) {
	d := shellFallbackNewDuplex(t)

	sub := cursorAppendStringField(nil, 1, "/tmp/x")    // f1 path
	sub = cursorAppendStringField(sub, 2, "call_del_1") // f2 tool_call_id
	parts := cursorNativeFsTestExecParts(t, 84, nil, cursorFldServerMsgDeleteArgs, sub)

	tc, err := d.cursorCaptureNativeFsArgs(parts, cursorFldServerMsgDeleteArgs, 84, nil)
	require.NoError(t, err)
	require.Equal(t, cursorToolKindDelete, tc.Kind)
	require.Equal(t, "shell", tc.Name)
	cmd := shellFallbackCommandFromCapture(t, tc)
	require.Contains(t, cmd, "rm -f -- '/tmp/x'", "delete 必须转译为 rm -f 命令")
}

// TestCursorNativeFsShellFallbackWrite：write → 命令含 `base64 -d` 且**不含**原始文件内容明文。
func TestCursorNativeFsShellFallbackWrite(t *testing.T) {
	d := shellFallbackNewDuplex(t)

	sub := cursorAppendStringField(nil, 1, "/tmp/a.txt")          // f1 path
	sub = cursorAppendStringField(sub, 2, "SECRET_CONTENT_PLAIN") // f2 file_text
	sub = cursorAppendStringField(sub, 3, "call_write_1")         // f3 tool_call_id
	parts := cursorNativeFsTestExecParts(t, 85, nil, cursorFldServerMsgWriteArgs, sub)

	tc, err := d.cursorCaptureNativeFsArgs(parts, cursorFldServerMsgWriteArgs, 85, nil)
	require.NoError(t, err)
	require.Equal(t, cursorToolKindWrite, tc.Kind)
	require.Equal(t, "shell", tc.Name)
	cmd := shellFallbackCommandFromCapture(t, tc)
	require.Contains(t, cmd, "base64 -d", "write 必须经 base64 解码写入")
	require.NotContains(t, cmd, "SECRET_CONTENT_PLAIN", "write 内容必须 base64，绝不明文拼进命令")
}

// TestCursorNativeFsShellFallbackUnavailable：当既无原生 read 工具、也无 shell 类工具时，read
// 委托仍走 ErrCursorNativeFsToolAbsent（既有拒绝帧路径不变）。
//
// 注：卡面写"声明集只有 read_file"。但 read_file 是原生 read 候选：声明它且 schema 含 path 会走
// 原生通道（而非 shell 回退），缺 path 属性则走 ErrCursorProtocol 失败关闭（既有行为，不得改）。
// 要真正命中"无原生工具也无 shell"的 Absent 拒绝帧，需声明一个既非原生 read 候选、也非 shell 的
// 工具。此处用 get_weather 代表"无可用工具"场景，验证 Absent 拒绝帧路径不变。
func TestCursorNativeFsShellFallbackUnavailable(t *testing.T) {
	defs := [][]byte{cursorTestBuildToolDef(t, "get_weather",
		`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`)}
	idx := cursorToolSchemaIndex(defs)
	// 既无原生 read 候选、也无 shell 类工具 → cursorBuildNativeFsDelegation 必须返回
	// ErrCursorNativeFsToolAbsent（既有拒绝帧路径不变；捕获侧据此发拒绝帧、返回空 capture + nil 错误）。
	_, _, _, err := cursorBuildNativeFsDelegation(defs, cursorToolKindRead,
		[]cursorNativeFsParam{{candidates: cursorNativeFsPathProps, value: "/tmp/a.txt"}})
	require.ErrorIs(t, err, ErrCursorNativeFsToolAbsent, "无原生工具且无 shell 必须返回 Absent 哨兵")
	require.False(t, cursorClientDeclaresNativeFsTool(idx), "前置：get_weather 不是原生文件工具")
}

// TestCursorNativeFsNativeToolPreferred：同时声明 read_file 与 shell 时，必须选 read_file
// （原生优先，shell 仅作回退），且 ResultArgsJSON 为空（回写字节不变）。
func TestCursorNativeFsNativeToolPreferred(t *testing.T) {
	defs := [][]byte{
		cursorTestBuildToolDef(t, "read_file", cursorNativeFsTestReadSchema),
		cursorTestBuildToolDef(t, "shell", cursorShellFallbackSchema),
	}
	d := cursorShellTestNewDuplex(t, defs, "")

	sub := cursorAppendStringField(nil, 1, "/tmp/a.txt")
	sub = cursorAppendStringField(sub, 2, "call_read_1")
	parts := cursorNativeFsTestExecParts(t, 87, nil, cursorFldServerMsgReadArgs, sub)

	tc, err := d.cursorCaptureNativeFsArgs(parts, cursorFldServerMsgReadArgs, 87, nil)
	require.NoError(t, err)
	require.Equal(t, "read_file", tc.Name, "原生工具必须优先于 shell 回退")
	require.Equal(t, "", tc.ResultArgsJSON, "走原生工具时 ResultArgsJSON 必须为空（回写字节不变）")
}

// TestCursorShellQuote：表驱动 —— a → 'a'；a'b → 'a'\”b'；空串 → ”；含换行/中文照常包裹。
func TestCursorShellQuote(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"a", "'a'"},
		{"a'b", "'a'\\''b'"},
		{"", "''"},
		{"line1\nline2", "'line1\nline2'"},
		{"中文路径/文件", "'中文路径/文件'"},
		{"has 'single' quote", `'has '\''single'\'' quote'`},
	}
	for _, c := range cases {
		require.Equal(t, c.want, cursorShellQuote(c.in), "cursorShellQuote(%q)", c.in)
	}
}

// TestCursorNativeFsShellFallbackWriteBackUsesNativeResult：走 shell 回退的 read 委托，客户端回
// 结果文本后，回写帧必须是 ReadResult（f7），且其中的 path 等于原始委托 path（证明改动 E 生效，
// 而非空 path）。
func TestCursorNativeFsShellFallbackWriteBackUsesNativeResult(t *testing.T) {
	d := shellFallbackNewDuplex(t)

	sub := cursorAppendStringField(nil, 1, "/tmp/a.txt")
	sub = cursorAppendStringField(sub, 2, "call_read_wb")
	parts := cursorNativeFsTestExecParts(t, 88, nil, cursorFldServerMsgReadArgs, sub)

	tc, err := d.cursorCaptureNativeFsArgs(parts, cursorFldServerMsgReadArgs, 88, nil)
	require.NoError(t, err)
	require.Equal(t, "shell", tc.Name, "前置：本用例必须走 shell 回退")
	require.Contains(t, tc.ResultArgsJSON, "/tmp/a.txt", "前置：ResultArgsJSON 携带原生 path")

	frames, err := cursorEncodeNativeFsResult(tc, CursorToolResult{Text: []string{"hello world"}})
	require.NoError(t, err)
	require.Len(t, frames, 1)

	ecmFields, derr := cursorDecodeProtobuf(frames[0])
	require.NoError(t, derr)
	res := cursorNativeFsTestDecodeResult(t, ecmFields)
	require.Equal(t, cursorFldClientMsgReadResult, res.field, "回写必须是 ReadResult (f7)，而非 ShellResult")

	// 解出 ReadSuccess 的 path，必须等于原始委托 path（证明改动 E 用 ResultArgsJSON 取 path）。
	var success []byte
	for _, f := range res.oneof {
		if f.Number == cursorFldReadResultSuccess {
			success = f.Bytes
		}
	}
	require.NotNil(t, success, "必须是 ReadResult success 分支")
	sf, serr := cursorDecodeProtobuf(success)
	require.NoError(t, serr)
	pf, ok := cursorFindField(sf, cursorFldReadSuccessPath)
	require.True(t, ok, "ReadSuccess 必须含 path 字段")
	require.Equal(t, "/tmp/a.txt", pf.String(), "回写 path 必须等于原始委托 path")
}

// TestCursorNativeFsShellFallbackCommandDump：把五种 kind 实际生成的 shell 命令全文打印出来，
// 供完工报告贴出（也顺带做基础断言确保五种命令形态都正确）。
func TestCursorNativeFsShellFallbackCommandDump(t *testing.T) {
	defs := [][]byte{cursorTestBuildToolDef(t, "shell", cursorShellFallbackSchema)}
	d := cursorShellTestNewDuplex(t, defs, "")

	capture := func(field int, sub []byte) string {
		tc, err := d.cursorCaptureNativeFsArgs(cursorNativeFsTestExecParts(t, 90, nil, field, sub), field, 90, nil)
		require.NoError(t, err)
		return shellFallbackCommandFromCapture(t, tc)
	}

	readCmd := capture(cursorFldServerMsgReadArgs, func() []byte {
		b := cursorAppendStringField(nil, 1, "/tmp/a.txt")
		b = cursorAppendStringField(b, 2, "c1")
		return b
	}())
	t.Logf("READ  : %s", readCmd)
	require.Contains(t, readCmd, "cat -- '/tmp/a.txt'")

	lsCmd := capture(cursorFldServerMsgLsArgs, func() []byte {
		b := cursorAppendStringField(nil, 1, "/repo")
		b = cursorAppendStringField(b, 3, "c2")
		return b
	}())
	t.Logf("LS    : %s", lsCmd)
	require.Contains(t, lsCmd, "ls -la -- '/repo'")

	grepCmd := capture(cursorFldServerMsgGrepArgs, func() []byte {
		b := cursorAppendStringField(nil, 1, "TODO")
		b = cursorAppendStringField(b, 2, "/repo")
		b = cursorAppendStringField(b, 14, "c3")
		return b
	}())
	t.Logf("GREP  : %s", grepCmd)
	require.Contains(t, grepCmd, "grep -rn -- 'TODO' '/repo'")

	writeCmd := capture(cursorFldServerMsgWriteArgs, func() []byte {
		b := cursorAppendStringField(nil, 1, "/tmp/a.txt")
		b = cursorAppendStringField(b, 2, "line1\nline2\n")
		b = cursorAppendStringField(b, 3, "c4")
		return b
	}())
	t.Logf("WRITE : %s", writeCmd)
	require.Contains(t, writeCmd, "base64 -d")

	delCmd := capture(cursorFldServerMsgDeleteArgs, func() []byte {
		b := cursorAppendStringField(nil, 1, "/tmp/x")
		b = cursorAppendStringField(b, 2, "c5")
		return b
	}())
	t.Logf("DELETE: %s", delCmd)
	require.Contains(t, delCmd, "rm -f -- '/tmp/x'")
}
