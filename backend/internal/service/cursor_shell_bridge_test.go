//go:build unit

package service

// Cursor 原生 shell 执行桥接（ExecServerMessage.f14 → 下游 tool_call → 回写 ShellStream）
// 定向单测。全部走真实生产函数：
//   - 委托捕获：d.cursorCaptureShellArgs（duplex.go）
//   - 工具名/schema 选择 + 参数转换：cursorBuildShellDelegation
//   - 续轮回写编码：cursorEncodeShellToolResult / cursorParseShellResult
//   - mcp 回写对照：cursorEncodeMcpToolResult + decodeMcpResultFrame（复用既有断言，验证字节级不变）
//
// 覆盖任务单"单测至少覆盖" 8 条 + 方案硬约束 #1-#5。所有断言均不直接依赖外部网络或真实上游。

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// cursorShellTestBuildExecParts 构造 ExecServerMessage 内层 parts：f1 exec_id(varint) +
// f15 id(bytes) + f14 ShellArgs(sub)。cursorCaptureShellArgs 直接从 parts 取 f14 解码。
func cursorShellTestBuildExecParts(t *testing.T, execID uint64, idField, sub []byte) []cursorPBField {
	t.Helper()
	var parts []cursorPBField
	parts = append(parts, cursorPBField{Number: 1, Wire: cursorWireVarint, Varint: execID})
	if len(idField) > 0 {
		parts = append(parts, cursorPBField{Number: 15, Wire: cursorWireBytes, Bytes: idField})
	}
	parts = append(parts, cursorPBField{Number: 14, Wire: cursorWireBytes, Bytes: sub})
	return parts
}

// cursorShellTestBuildShellArgs 构造 ShellArgs 子消息：f1 command / f2 working_directory /
// f4 tool_call_id / f15 description。
func cursorShellTestBuildShellArgs(t *testing.T, command, wd, tcid, desc string) []byte {
	t.Helper()
	var sub []byte
	sub = cursorAppendStringField(sub, 1, command)
	if wd != "" {
		sub = cursorAppendStringField(sub, 2, wd)
	}
	sub = cursorAppendStringField(sub, 4, tcid)
	sub = cursorAppendStringField(sub, 15, desc)
	return sub
}

// cursorShellTestNewDuplex 构造带本地委托记录与声明工具的 duplex（显式登记一个对外 started）。
func cursorShellTestNewDuplex(t *testing.T, defs [][]byte, startedCallID string) *cursorAgentDuplex {
	t.Helper()
	d := &cursorAgentDuplex{delegations: newLocalCursorToolDelegations(), declaredToolDefs: defs}
	if startedCallID != "" {
		require.NoError(t, d.delegations.noteToolCallStarted("", startedCallID, true))
	}
	return d
}

// shellSchemaArray 是声明为 command=array 的 shell 工具 schema（与方案 §4 实证一致）。
const cursorShellTestSchemaArray = `{"type":"object","properties":{"command":{"type":"array","items":{"type":"string"}},"working_directory":{"type":"string"}},"required":["command"]}`

// ---------------------------------------------------------------------------
// 1. 正常 f14 → 委托（Kind=shell、ID=started.call_id、args 按 array schema 转 ["bash","-lc",cmd]）
// ---------------------------------------------------------------------------
func TestCursorShellBridgeDelegationNormal(t *testing.T) {
	defs := [][]byte{cursorTestBuildToolDef(t, "shell", cursorShellTestSchemaArray)}
	d := cursorShellTestNewDuplex(t, defs, "call_shell_1")

	sub := cursorShellTestBuildShellArgs(t, "ls -la", "/work", "call_shell_1", "list files")
	parts := cursorShellTestBuildExecParts(t, 7, []byte("exec-orig-7"), sub)

	tc, err := d.cursorCaptureShellArgs(parts, 7, []byte("exec-orig-7"))
	require.NoError(t, err, "正常 f14 必须成功委托")
	require.Equal(t, "shell", tc.Kind, "Kind 必须为 shell")
	require.Equal(t, "call_shell_1", tc.ID, "ID 必须来自 ShellArgs.tool_call_id（=已登记 started）")
	require.Equal(t, "shell", tc.Name, "工具名须按优先级选中 shell")
	require.EqualValues(t, 7, tc.ExecID)
	require.Equal(t, []byte("exec-orig-7"), tc.IDField)
	// command 目标是 array → 按 schema 转 ["bash","-lc",cmd]；workdir 取 working_directory。
	require.JSONEq(t, `{"command":["bash","-lc","ls -la"],"working_directory":"/work"}`, tc.ArgsJSON)

	// 委托已登记入批次（复用既有封口谓词）。
	snap := d.delegations.snapshot()
	require.Len(t, snap, 1)
	require.Equal(t, "call_shell_1", snap[0].ID)
	require.Equal(t, "shell", snap[0].Kind)
}

// 2. command 为空 → 失败关闭
func TestCursorShellBridgeEmptyCommand(t *testing.T) {
	defs := [][]byte{cursorTestBuildToolDef(t, "shell", cursorShellTestSchemaArray)}
	d := cursorShellTestNewDuplex(t, defs, "call_shell_1")

	// command 空：即便 tool_call_id 与 started 匹配，也必须在 command 校验处失败关闭。
	sub := cursorShellTestBuildShellArgs(t, "", "/work", "call_shell_1", "list files")
	parts := cursorShellTestBuildExecParts(t, 7, []byte("exec-orig-7"), sub)

	_, err := d.cursorCaptureShellArgs(parts, 7, []byte("exec-orig-7"))
	require.Error(t, err, "command 空必须失败关闭")
	require.True(t, errors.Is(err, ErrCursorProtocol), "失败必须落到 ErrCursorProtocol")
}

// 3. 未声明 shell 类工具 → 失败关闭（不许用 "shell" 兜底）
func TestCursorShellBridgeNoShellTool(t *testing.T) {
	// 只声明 get_weather（非 shell 类，且 properties 无 command），无 shell/bash/... 工具。
	defs := [][]byte{cursorTestBuildToolDef(t, "get_weather", `{"type":"object","properties":{"city":{"type":"string"}}}`)}
	d := cursorShellTestNewDuplex(t, defs, "call_x")

	sub := cursorShellTestBuildShellArgs(t, "ls -la", "", "call_x", "desc")
	parts := cursorShellTestBuildExecParts(t, 9, []byte("exec-9"), sub)

	_, err := d.cursorCaptureShellArgs(parts, 9, []byte("exec-9"))
	require.Error(t, err, "无可用 shell 类工具必须失败关闭")
	require.True(t, errors.Is(err, ErrCursorProtocol), "失败必须落到 ErrCursorProtocol（不许 shell 兜底）")
}

// 4. tool_call_id 与 started 不匹配 → 失败关闭（不得新建批次、不得当作新调用）
func TestCursorShellBridgeMismatchedCallID(t *testing.T) {
	defs := [][]byte{cursorTestBuildToolDef(t, "shell", cursorShellTestSchemaArray)}
	// 已登记 started 是 call_a；ShellArgs.tool_call_id 给 call_b。
	d := cursorShellTestNewDuplex(t, defs, "call_a")

	sub := cursorShellTestBuildShellArgs(t, "ls -la", "", "call_b", "desc")
	parts := cursorShellTestBuildExecParts(t, 11, []byte("exec-11"), sub)

	_, err := d.cursorCaptureShellArgs(parts, 11, []byte("exec-11"))
	require.Error(t, err, "tool_call_id 与 started 不匹配必须失败关闭")
	require.True(t, errors.Is(err, ErrCursorProtocol), "失败必须落到 ErrCursorProtocol")
	// 不得新建/追加委托：批次仍只有 started、无 delegation。
	require.Empty(t, d.delegations.snapshot(), "不匹配不得产生委托记录")
}

// 5. 回写字节：stdout/exit 的字段号与嵌套层级正确（逐字节断言）
func TestCursorShellBridgeWritebackBytes(t *testing.T) {
	// stderr 非空 → 帧序 [stderr, stdout, exit]；外层统一 f1 exec_id + f15 id + f14 ShellStream。
	frames := cursorEncodeShellToolResult(7, []byte("exec-7"), "OUT", "ERR", 2, "/cwd")
	require.Len(t, frames, 3, "stderr+stdout+exit 共 3 帧")

	// 帧 0：stderr（f14 ShellStream{f2 ShellStreamStderr{f1 data}}）
	outer, err := cursorDecodeProtobuf(frames[0])
	require.NoError(t, err)
	require.EqualValues(t, 7, cursorVarintField(outer, cursorFldClientMsgID))
	idf, ok := cursorFindField(outer, cursorFldClientMsgExecID)
	require.True(t, ok)
	require.Equal(t, []byte("exec-7"), idf.Bytes)
	ss, ok := cursorFindField(outer, cursorFldClientMsgShellStream)
	require.True(t, ok, "必须承载 f14 ShellStream")
	require.Equal(t, 14, ss.Number)
	ssf, _ := cursorDecodeProtobuf(ss.Bytes)
	stderrF, ok := cursorFindField(ssf, cursorFldShellStreamStderr)
	require.True(t, ok, "stderr 帧内层必须是 f2")
	inner, _ := cursorDecodeProtobuf(stderrF.Bytes)
	dataF, ok := cursorFindField(inner, cursorFldShellStreamData)
	require.True(t, ok)
	require.Equal(t, "ERR", dataF.String())

	// 帧 1：stdout（f14 ShellStream{f1 ShellStreamStdout{f1 data}}）
	outer, _ = cursorDecodeProtobuf(frames[1])
	ss, ok = cursorFindField(outer, cursorFldClientMsgShellStream)
	require.True(t, ok)
	ssf, _ = cursorDecodeProtobuf(ss.Bytes)
	stdoutF, ok := cursorFindField(ssf, cursorFldShellStreamStdout)
	require.True(t, ok, "stdout 帧内层必须是 f1")
	inner, _ = cursorDecodeProtobuf(stdoutF.Bytes)
	dataF, ok = cursorFindField(inner, cursorFldShellStreamData)
	require.True(t, ok)
	require.Equal(t, "OUT", dataF.String())

	// 帧 2：exit（f14 ShellStream{f3 ShellStreamExit{f1 code, f2 cwd}}）
	outer, _ = cursorDecodeProtobuf(frames[2])
	ss, ok = cursorFindField(outer, cursorFldClientMsgShellStream)
	require.True(t, ok)
	ssf, _ = cursorDecodeProtobuf(ss.Bytes)
	exitF, ok := cursorFindField(ssf, cursorFldShellStreamExit)
	require.True(t, ok, "exit 帧内层必须是 f3")
	exitInner, _ := cursorDecodeProtobuf(exitF.Bytes)
	require.EqualValues(t, 2, cursorVarintField(exitInner, cursorFldShellStreamExitCode), "exit.code 必须位于 f1")
	exitCwd, ok := cursorFindField(exitInner, cursorFldShellStreamExitCwd)
	require.True(t, ok)
	require.Equal(t, "/cwd", exitCwd.String(), "exit.cwd 必须位于 f2")

	// 无 stderr 时只剩 [stdout, exit] 两帧，且 stdout 在前。
	frames2 := cursorEncodeShellToolResult(3, nil, "only", "", 0, "")
	require.Len(t, frames2, 2, "无 stderr 时只有 stdout+exit")
	o2, _ := cursorDecodeProtobuf(frames2[0])
	s2, ok := cursorFindField(o2, cursorFldClientMsgShellStream)
	require.True(t, ok)
	s2f, _ := cursorDecodeProtobuf(s2.Bytes)
	_, hasStdout := cursorFindField(s2f, cursorFldShellStreamStdout)
	require.True(t, hasStdout, "首帧必须是 stdout")
	_, hasStderr := cursorFindField(s2f, cursorFldShellStreamStderr)
	require.False(t, hasStderr)
	o3, _ := cursorDecodeProtobuf(frames2[1])
	s3, ok := cursorFindField(o3, cursorFldClientMsgShellStream)
	require.True(t, ok)
	s3f, _ := cursorDecodeProtobuf(s3.Bytes)
	_, hasExit := cursorFindField(s3f, cursorFldShellStreamExit)
	require.True(t, hasExit, "次帧必须是 exit")
}

// 6. JSON 结果与纯文本结果两种解析
func TestCursorShellBridgeResultParsing(t *testing.T) {
	// JSON 含 stdout/stderr/exit_code → 用字段值。
	jsonRes := CursorToolResult{Text: []string{`{"stdout":"o","stderr":"e","exit_code":3}`}}
	stdout, stderr, code := cursorParseShellResult(jsonRes)
	require.Equal(t, "o", stdout)
	require.Equal(t, "e", stderr)
	require.Equal(t, 3, code)

	// Structured 亦可作为 JSON 来源。
	structuredRes := CursorToolResult{Structured: []byte(`{"stdout":"xs"}`)}
	s2, _, c2 := cursorParseShellResult(structuredRes)
	require.Equal(t, "xs", s2)
	require.Equal(t, 0, c2)

	// 纯文本 → 整段作 stdout，exit=0。
	plain := CursorToolResult{Text: []string{"just text\nsecond line"}}
	ps, _, pc := cursorParseShellResult(plain)
	require.Equal(t, "just text\nsecond line", ps)
	require.Equal(t, 0, pc)

	// 纯文本 + is_error → exit=1。
	plainErr := CursorToolResult{Text: []string{"boom"}, IsError: true}
	_, _, pe := cursorParseShellResult(plainErr)
	require.Equal(t, 1, pe)
}

// 7. mcp 委托回写不变（复用既有断言 decodeMcpResultFrame）
func TestCursorShellBridgeMcpWritebackUnchanged(t *testing.T) {
	// mcp 路径字节级结构不变：内层 f11 McpResult、f1 exec_id、f15 id 原值保留。
	execMsg := cursorEncodeMcpToolResult(42, []byte("exec-orig-42"), "call_x",
		[]string{"hello world"}, []byte(`{"k":1}`), false)
	frame := decodeMcpResultFrame(t, execMsg)
	require.EqualValues(t, 42, frame.execID)
	require.Equal(t, []byte("exec-orig-42"), frame.idField)
	require.False(t, frame.isError)
	require.Equal(t, []string{"hello world"}, frame.texts)

	// is_error 仍走 f2（错误语义）。
	errMsg := cursorEncodeMcpToolResult(9, []byte("exec-orig-9"), "call_err",
		[]string{"boom"}, nil, true)
	errFrame := decodeMcpResultFrame(t, errMsg)
	require.True(t, errFrame.isError)
	require.Equal(t, []string{"boom"}, errFrame.texts)
}

// 8. shell + mcp 混合批次可封口
func TestCursorShellBridgeMixedBatchSealable(t *testing.T) {
	d := newLocalCursorToolDelegations()
	// 同一 model_call_id 下两个对外 started：一个 mcp、一个 shell。
	require.NoError(t, d.noteToolCallStarted("mc", "mcp_call", true))
	require.NoError(t, d.noteToolCallStarted("mc", "shell_call", true))
	require.NoError(t, d.noteToolRequestsListed("mc", 2))

	// mcp 委托（Kind 空）与 shell 委托（Kind="shell"）各一条。
	mcpCap := cursorToolCallCapture{ID: "mcp_call", Name: "get_weather", ArgsJSON: `{"city":"x"}`}
	shellCap := cursorToolCallCapture{ID: "shell_call", Name: "shell", ArgsJSON: `{"command":["bash","-lc","ls"]}`, Kind: "shell"}
	require.NoError(t, d.noteDelegation("", mcpCap))
	require.NoError(t, d.noteDelegation("", shellCap))

	// 混合批次可封口（两个 started 都已解码委托、listed 等齐）。
	require.NoError(t, d.sealable(), "shell + mcp 混合批次必须可封口")

	// 封口后快照两条委托的 Kind 各就各位。
	snap := d.snapshot()
	require.Len(t, snap, 2)
	kinds := map[string]string{}
	for _, c := range snap {
		kinds[c.ID] = c.Kind
	}
	require.Equal(t, "", kinds["mcp_call"], "mcp 委托 Kind 必须为空（字节级行为不变）")
	require.Equal(t, "shell", kinds["shell_call"], "shell 委托 Kind 必须为 shell")
}

// 9. f14 先到 → 不报错、进入 pending；随后 started 到达 → 委托出现在该 started 所在批、pending 清空
func TestCursorShellBridgeF14FirstThenStarted(t *testing.T) {
	defs := [][]byte{cursorTestBuildToolDef(t, "shell", cursorShellTestSchemaArray)}
	// f14 先到：无任何 started 已登记。
	d := cursorShellTestNewDuplex(t, defs, "")

	sub := cursorShellTestBuildShellArgs(t, "ls -la", "/work", "call_shell_1", "list files")
	parts := cursorShellTestBuildExecParts(t, 7, []byte("exec-orig-7"), sub)

	tc, err := d.cursorCaptureShellArgs(parts, 7, []byte("exec-orig-7"))
	require.NoError(t, err, "f14 先到：不得报错、不得失败关闭")
	require.Equal(t, "call_shell_1", tc.ID)
	// 进入 pending，不建批、不进 snapshot。
	require.Len(t, d.delegations.pending, 1, "f14 先到必须进入 pending")
	require.Contains(t, d.delegations.pending, "call_shell_1")
	require.Empty(t, d.delegations.snapshot(), "pending 委托不得出现在批次快照里")

	// started 随后到达：同一把锁内并入该 started 所在批。
	require.NoError(t, d.delegations.noteToolCallStarted("", "call_shell_1", true))
	require.Empty(t, d.delegations.pending, "started 到达后 pending 必须清空")

	// 委托出现在该 started 所在批（snapshot 命中，Kind=shell）。
	snap := d.delegations.snapshot()
	require.Len(t, snap, 1)
	require.Equal(t, "call_shell_1", snap[0].ID)
	require.Equal(t, "shell", snap[0].Kind)

	// 补 listed=1 → 批次可封口（验证完整链路：f14 先到也能正确收口）。
	require.NoError(t, d.delegations.noteToolRequestsListed("", 1))
	require.NoError(t, d.delegations.sealable(), "started 到达并入后批次必须可封口")
}

// 10. started 先到 → 原有路径仍通过（不进 pending、直接入批）
func TestCursorShellBridgeStartedFirstOrder(t *testing.T) {
	defs := [][]byte{cursorTestBuildToolDef(t, "shell", cursorShellTestSchemaArray)}
	// started 已先登记（生产另一种顺序）。
	d := cursorShellTestNewDuplex(t, defs, "call_shell_1")

	sub := cursorShellTestBuildShellArgs(t, "ls -la", "/work", "call_shell_1", "list files")
	parts := cursorShellTestBuildExecParts(t, 7, []byte("exec-orig-7"), sub)

	tc, err := d.cursorCaptureShellArgs(parts, 7, []byte("exec-orig-7"))
	require.NoError(t, err, "started 先到：原有路径必须仍通过")
	require.Empty(t, d.delegations.pending, "started 先到：不进 pending")
	require.Len(t, d.delegations.snapshot(), 1)
	require.Equal(t, "call_shell_1", tc.ID)
	require.Equal(t, "shell", snapKind(t, d.delegations, "call_shell_1"))
}

// snapKind 是测试辅助：从快照取某 ID 委托的 Kind。
func snapKind(t *testing.T, d *cursorToolDelegations, id string) string {
	t.Helper()
	for _, c := range d.snapshot() {
		if c.ID == id {
			return c.Kind
		}
	}
	return "<missing>"
}

// 11. pending 未配对时封口被拒，reason=shell_pending_unmatched
func TestCursorShellBridgePendingUnmatchedSealRejected(t *testing.T) {
	defs := [][]byte{cursorTestBuildToolDef(t, "shell", cursorShellTestSchemaArray)}
	// f14 先到，无任何 started。
	d := cursorShellTestNewDuplex(t, defs, "")

	sub := cursorShellTestBuildShellArgs(t, "ls -la", "", "call_shell_1", "desc")
	parts := cursorShellTestBuildExecParts(t, 7, []byte("exec-7"), sub)
	_, err := d.cursorCaptureShellArgs(parts, 7, []byte("exec-7"))
	require.NoError(t, err, "f14 先到：进入 pending，不报错")
	require.Len(t, d.delegations.pending, 1)

	// 即便其它信号到位，pending 未配对也阻止封口，reason=shell_pending_unmatched。
	require.NoError(t, d.delegations.noteToolRequestsListed("", 1))
	sealErr := d.delegations.sealable()
	require.Error(t, sealErr, "pending 未配对必须封口被拒")
	require.ErrorIs(t, sealErr, ErrCursorBatchIncomplete)
	require.Contains(t, sealErr.Error(), "shell_pending_unmatched", "失败原因必须为 shell_pending_unmatched")
}

// 12. mcp 委托路径回归：Kind=="" 走 noteDelegation，逐字节不变、不进 pending
func TestCursorShellBridgeMcpPathUnaffected(t *testing.T) {
	d := newLocalCursorToolDelegations()
	require.NoError(t, d.noteToolCallStarted("mc", "mcp_call", true))
	require.NoError(t, d.noteToolRequestsListed("mc", 1))

	// mcp 委托（Kind 空）直接 noteDelegation，不进 shell pending。
	mcpCap := cursorToolCallCapture{ID: "mcp_call", Name: "get_weather", ArgsJSON: `{"city":"x"}`}
	require.NoError(t, d.noteDelegation("", mcpCap))
	require.Empty(t, d.pending, "mcp 委托不得进入 shell pending")

	// mcp 路径封口语义不变（字节级行为不变，无 pending 干扰）。
	require.NoError(t, d.sealable(), "mcp 委托路径封口语义不变")
	snap := d.snapshot()
	require.Len(t, snap, 1)
	require.Equal(t, "", snap[0].Kind, "mcp 委托 Kind 必须仍为空（字节级行为不变）")
}
