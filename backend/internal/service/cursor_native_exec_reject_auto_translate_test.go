//go:build unit

package service

// 候选 A：原生工具自动翻译成客户端工具（mcp_args / f11）验收测试。
//
// 覆盖：
//   - 11 个参数映射函数（每类原生 exec 一个），断言 tool_call_id 与参数键值对；
//     对缺少关键语义参数的 3 类（write / grep / write_shell_stdin）断言降级错误。
//   - McpArgs 载荷构造：用既有 cursorDecodeMcpArgs 解回，验证 f2/f3/f5 形状对称。
//   - 1 个集成测试：命中 + 声明 → 发出 f11 mcp_args 帧 + 登记委派 + 记
//     native_exec_auto_translated；无声明 → 降级既有拒绝路径。
//
// 所有构造字段号均为字面数字（取自方案「拒绝形状」表 / McpArgs 协议），
// 不引用实现里的 cursorFldNativeReject* 常量，避免自证循环。

import (
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// 各原生 args 内层子字段号（字面值）。
const (
	atCommand    = 1  // ShellArgs.f1 command
	atWorkingDir = 2  // ShellArgs.f2 working_directory
	atShellTCID  = 4  // ShellArgs.f4 tool_call_id
	atBgTCID     = 3  // BackgroundShellSpawnArgs.f3 tool_call_id
	atPath       = 1  // Write/Delete/Read/Ls/DiagnosticsArgs.f1 path
	atWriteTCID  = 3  // WriteArgs.f3 tool_call_id
	atDeleteTCID = 2  // DeleteArgs.f2 tool_call_id
	atReadTCID   = 2  // ReadArgs.f2 tool_call_id
	atLsTCID     = 3  // LsArgs.f3 tool_call_id
	atDiagTCID   = 2  // DiagnosticsArgs.f2 tool_call_id
	atFetchURL   = 1  // FetchArgs.f1 url
	atFetchTCID  = 2  // FetchArgs.f2 tool_call_id
	atGrepTCID   = 14 // GrepArgs.f14 tool_call_id
)

func atShellArgs(cmd, wd, tcid string) []byte {
	b := cursorAppendStringField(nil, atCommand, cmd)
	b = cursorAppendStringField(b, atWorkingDir, wd)
	return cursorAppendStringField(b, atShellTCID, tcid)
}

func atBgShellArgs(cmd, wd, tcid string) []byte {
	b := cursorAppendStringField(nil, atCommand, cmd)
	b = cursorAppendStringField(b, atWorkingDir, wd)
	return cursorAppendStringField(b, atBgTCID, tcid)
}

func atFileArgs(path, tcid string, tcidField int) []byte {
	b := cursorAppendStringField(nil, atPath, path)
	return cursorAppendStringField(b, tcidField, tcid)
}

func atFetchArgs(url, tcid string) []byte {
	b := cursorAppendStringField(nil, atFetchURL, url)
	return cursorAppendStringField(b, atFetchTCID, tcid)
}

// ---------------------------------------------------------------------------
// 11 个参数映射函数单元测试
// ---------------------------------------------------------------------------

// ① f2 shell_args → command + working_directory。
func TestCursorNativeExecAutoTranslateShell(t *testing.T) {
	id, params, err := cursorTranslateShellArgs(atShellArgs("ls -la", "/repo", "call_s1"))
	require.NoError(t, err)
	require.Equal(t, "call_s1", id)
	require.Equal(t, [][2]string{{"command", "ls -la"}, {"working_directory", "/repo"}}, params)

	// 同一次调用验证 McpArgs 载荷构造：用既有解码器解回，断言 f2/f3/f5 形状对称。
	mcp := cursorTranslateBuildMcpArgs(id, "Bash", params)
	gotID, gotName, gotArgs, derr := cursorDecodeMcpArgs(mcp)
	require.NoError(t, derr)
	require.Equal(t, "call_s1", gotID)
	require.Equal(t, "Bash", gotName)
	require.JSONEq(t, `{"command":"ls -la","working_directory":"/repo"}`, gotArgs)
}

// ② f14 shell_stream_args → 与 f2 同形。
func TestCursorNativeExecAutoTranslateShellStream(t *testing.T) {
	id, params, err := cursorTranslateShellStreamArgs(atShellArgs("pwd", "/work", "call_s2"))
	require.NoError(t, err)
	require.Equal(t, "call_s2", id)
	require.Equal(t, [][2]string{{"command", "pwd"}, {"working_directory", "/work"}}, params)
}

// ③ f16 background_shell_spawn_args → command + working_directory（tcid 在 f3）。
func TestCursorNativeExecAutoTranslateBackgroundShellSpawn(t *testing.T) {
	id, params, err := cursorTranslateBackgroundShellSpawnArgs(atBgShellArgs("sleep 5", "/tmp", "call_s3"))
	require.NoError(t, err)
	require.Equal(t, "call_s3", id)
	require.Equal(t, [][2]string{{"command", "sleep 5"}, {"working_directory", "/tmp"}}, params)
}

// ④ f3 write_args → 缺少 content，必须降级（不向客户端发空写调用）。
func TestCursorNativeExecAutoTranslateWrite(t *testing.T) {
	id, params, err := cursorTranslateWriteArgs(atFileArgs("/repo/a.txt", "call_w1", atWriteTCID))
	require.Error(t, err, "缺 content 时必须降级")
	require.Contains(t, err.Error(), "not safely translatable")
	require.Empty(t, id)
	require.Nil(t, params)
}

// ⑤ f4 delete_args → path。
func TestCursorNativeExecAutoTranslateDelete(t *testing.T) {
	id, params, err := cursorTranslateDeleteArgs(atFileArgs("/repo/d.txt", "call_d1", atDeleteTCID))
	require.NoError(t, err)
	require.Equal(t, "call_d1", id)
	require.Equal(t, [][2]string{{"path", "/repo/d.txt"}}, params)
}

// ⑥ f5 grep_args → 缺少 pattern，必须降级。
func TestCursorNativeExecAutoTranslateGrep(t *testing.T) {
	id, params, err := cursorTranslateGrepArgs(cursorAppendStringField(nil, atGrepTCID, "call_g1"))
	require.Error(t, err, "缺 pattern 时必须降级")
	require.Contains(t, err.Error(), "not safely translatable")
	require.Empty(t, id)
	require.Nil(t, params)
}

// ⑦ f7 read_args → path。
func TestCursorNativeExecAutoTranslateRead(t *testing.T) {
	id, params, err := cursorTranslateReadArgs(atFileArgs("/repo/main.go", "call_r1", atReadTCID))
	require.NoError(t, err)
	require.Equal(t, "call_r1", id)
	require.Equal(t, [][2]string{{"path", "/repo/main.go"}}, params)
}

// ⑧ f8 ls_args → path。
func TestCursorNativeExecAutoTranslateLs(t *testing.T) {
	id, params, err := cursorTranslateLsArgs(atFileArgs("/repo", "call_l1", atLsTCID))
	require.NoError(t, err)
	require.Equal(t, "call_l1", id)
	require.Equal(t, [][2]string{{"path", "/repo"}}, params)
}

// ⑨ f9 diagnostics_args → path（候选 A 补全字段）。
func TestCursorNativeExecAutoTranslateDiagnostics(t *testing.T) {
	id, params, err := cursorTranslateDiagnosticsArgs(atFileArgs("/repo/x", "call_x1", atDiagTCID))
	require.NoError(t, err)
	require.Equal(t, "call_x1", id)
	require.Equal(t, [][2]string{{"path", "/repo/x"}}, params)
}

// ⑩ f20 fetch_args → url（候选 A 补全字段）。
func TestCursorNativeExecAutoTranslateFetch(t *testing.T) {
	id, params, err := cursorTranslateFetchArgs(atFetchArgs("https://example.com/f", "call_f1"))
	require.NoError(t, err)
	require.Equal(t, "call_f1", id)
	require.Equal(t, [][2]string{{"url", "https://example.com/f"}}, params)
}

// ⑪ f23 write_shell_stdin_args → 无内容 / 无 tool_call_id，必须降级（候选 A 补全字段）。
func TestCursorNativeExecAutoTranslateWriteShellStdin(t *testing.T) {
	id, params, err := cursorTranslateWriteShellStdinArgs([]byte{})
	require.Error(t, err, "缺内容与 tool_call_id 时必须降级")
	require.Contains(t, err.Error(), "not safely translatable")
	require.Empty(t, id)
	require.Nil(t, params)
}

// ---------------------------------------------------------------------------
// 集成测试：翻译路径 vs 降级路径
// ---------------------------------------------------------------------------

// cursorAutoTranslateDuplex 构造带 observer 的 duplex；shared=true 用生产会话级委托记录
// （翻译路径只对 shared 启用），shared=false 用 local 记录（既有拒绝形状测试口径）。
func cursorAutoTranslateDuplex(t *testing.T, defs [][]byte, shared bool) (*cursorAgentDuplex, *observer.ObservedLogs, func() []byte) {
	t.Helper()
	pr, pw := io.Pipe()
	core, logs := observer.New(zap.DebugLevel)
	delegations := newLocalCursorToolDelegations()
	if shared {
		delegations = newCursorToolDelegations()
	}
	d := &cursorAgentDuplex{
		reader:           pr,
		writer:           pw,
		delegations:      delegations,
		declaredToolDefs: defs,
		lg:               zap.New(core),
	}
	return d, logs, captureDuplex(t, d)
}

func cursorAutoTranslateExecParts(execID uint64, idField []byte, field int, sub []byte) []cursorPBField {
	parts := []cursorPBField{{Number: 1, Wire: cursorWireVarint, Varint: execID}}
	if len(idField) > 0 {
		parts = append(parts, cursorPBField{Number: 15, Wire: cursorWireBytes, Bytes: idField})
	}
	parts = append(parts, cursorPBField{Number: field, Wire: cursorWireBytes, Bytes: sub})
	return parts
}

// 集成：命中 + 声明 → f11 mcp_args 帧 + 委派登记 + native_exec_auto_translated；
// 无声明 → 既有拒绝路径。
func TestCursorNativeExecAutoTranslateIntegration(t *testing.T) {
	defs := [][]byte{
		cursorTestBuildToolDef(t, "Bash", `{"type":"object","properties":{"command":{"type":"string"},"working_directory":{"type":"string"}}}`),
	}

	// ① 命中 + 声明：f2 shell_args → Bash 工具调用。
	d, logs, stop := cursorAutoTranslateDuplex(t, defs, true)
	execID, idField := uint64(0xA1), []byte("exec-at-01")
	parts := cursorAutoTranslateExecParts(execID, idField, 2, atShellArgs("ls -la", "/repo", "call-at-1"))
	require.NoError(t, d.cursorRejectNativeExec(parts, 2, execID, idField))
	wire := stop()

	// 日志：native_exec_auto_translated 一条，字段齐全，且不泄露 command 业务值。
	entries := logs.FilterField(zap.String("phase", "native_exec_auto_translated")).All()
	require.Len(t, entries, 1, "必须记一条自动翻译日志")
	cm := entries[0].ContextMap()
	require.EqualValues(t, 2, cm["field"])
	require.Equal(t, "shell_args", cm["from_tool"])
	require.Equal(t, "Bash", cm["to_tool"])
	require.Equal(t, "call-at-1", cm["tool_call_id"])
	for _, e := range entries {
		for _, f := range e.Context {
			require.NotContains(t, f.String, "ls -la", "日志不得含 command 业务值")
		}
	}

	// 委派登记：call_id / 工具名 / 参数 JSON 均保留。
	require.Len(t, d.delegations.all, 1)
	require.Len(t, d.delegations.all[0].delegations, 1)
	tc := d.delegations.all[0].delegations[0]
	require.Equal(t, "call-at-1", tc.ID, "必须保留 tool_call_id")
	require.Equal(t, "Bash", tc.Name)
	require.JSONEq(t, `{"command":"ls -la","working_directory":"/repo"}`, tc.ArgsJSON)
	require.Equal(t, execID, tc.ExecID, "结果回写锚点必须是原 exec_id")

	// 发出 f11 mcp_args 帧（不再回拒绝结果帧）。
	var sawMcpArgs bool
	off := 0
	for off < len(wire) {
		frame, next, ferr := cursorReadEnvelopeFrame(wire, off)
		require.NoError(t, ferr)
		top, derr := cursorDecodeProtobuf(frame.Data)
		require.NoError(t, derr)
		if exec, ok := cursorFindField(top, cursorFldServerExecMessage); ok {
			inner, ierr := cursorDecodeProtobuf(exec.Bytes)
			require.NoError(t, ierr)
			if ma, ok := cursorFindField(inner, 11); ok {
				fid, fname, fargs, merr := cursorDecodeMcpArgs(ma.Bytes)
				require.NoError(t, merr)
				require.Equal(t, "call-at-1", fid)
				require.Equal(t, "Bash", fname)
				require.JSONEq(t, `{"command":"ls -la","working_directory":"/repo"}`, fargs)
				sawMcpArgs = true
			}
		}
		off = next
	}
	require.True(t, sawMcpArgs, "必须发出一条 f11 mcp_args 帧给客户端执行")

	// ② 无声明工具：等价表无从命中 → 降级既有拒绝路径。
	d2, logs2, stop2 := cursorAutoTranslateDuplex(t, nil, true)
	execID2 := uint64(0xB2)
	parts2 := cursorAutoTranslateExecParts(execID2, nil, 2, atShellArgs("echo hi", "/tmp", "call-at-2"))
	require.NoError(t, d2.cursorRejectNativeExec(parts2, 2, execID2, nil))
	_ = stop2()
	require.Len(t, logs2.FilterField(zap.String("phase", "native_exec_rejected")).All(), 1,
		"无声明工具时必须降级到既有拒绝路径")
	require.Empty(t, logs2.FilterField(zap.String("phase", "native_exec_auto_translated")).All(),
		"降级路径不得记自动翻译日志")
}
