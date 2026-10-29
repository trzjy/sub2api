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
	"io"
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
	frames := cursorEncodeShellToolResult(7, []byte("exec-7"), "OUT", "ERR", 2, "/cwd", nil)
	require.Len(t, frames, 4, "start+stderr+stdout+exit 共 4 帧")

	// 帧 0：start（f14 ShellStream{f4 ShellStreamStart{}}）——告知上游执行已开始。
	startOuter, err := cursorDecodeProtobuf(frames[0])
	require.NoError(t, err)
	startSS, ok := cursorFindField(startOuter, cursorFldClientMsgShellStream)
	require.True(t, ok)
	startInner, _ := cursorDecodeProtobuf(startSS.Bytes)
	_, hasStart := cursorFindField(startInner, cursorFldShellStreamStart)
	require.True(t, hasStart, "首帧必须是 f4 start")

	// 帧 1：stderr（f14 ShellStream{f2 ShellStreamStderr{f1 data}}）
	outer, err := cursorDecodeProtobuf(frames[1])
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

	// 帧 2：stdout（f14 ShellStream{f1 ShellStreamStdout{f1 data}}）
	outer, _ = cursorDecodeProtobuf(frames[2])
	ss, ok = cursorFindField(outer, cursorFldClientMsgShellStream)
	require.True(t, ok)
	ssf, _ = cursorDecodeProtobuf(ss.Bytes)
	stdoutF, ok := cursorFindField(ssf, cursorFldShellStreamStdout)
	require.True(t, ok, "stdout 帧内层必须是 f1")
	inner, _ = cursorDecodeProtobuf(stdoutF.Bytes)
	dataF, ok = cursorFindField(inner, cursorFldShellStreamData)
	require.True(t, ok)
	require.Equal(t, "OUT", dataF.String())

	// 帧 3：exit（f14 ShellStream{f3 ShellStreamExit{f1 code, f2 cwd}}）
	outer, _ = cursorDecodeProtobuf(frames[3])
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

	// 无 stderr 时为 [start, stdout, exit] 三帧，start 在前、stdout 次之。
	frames2 := cursorEncodeShellToolResult(3, nil, "only", "", 0, "", nil)
	require.Len(t, frames2, 3, "无 stderr 时是 start+stdout+exit")
	o2, _ := cursorDecodeProtobuf(frames2[1])
	s2, ok := cursorFindField(o2, cursorFldClientMsgShellStream)
	require.True(t, ok)
	s2f, _ := cursorDecodeProtobuf(s2.Bytes)
	_, hasStdout := cursorFindField(s2f, cursorFldShellStreamStdout)
	require.True(t, hasStdout, "首帧必须是 stdout")
	_, hasStderr := cursorFindField(s2f, cursorFldShellStreamStderr)
	require.False(t, hasStderr)
	o3, _ := cursorDecodeProtobuf(frames2[2])
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

//  9. f14 先到（无 started）→ 建 shellOnly 专属批并立即可封口（不挂 pending、不等 started/listed）；
//     随后 started 到达 → 委托并入该 started 批（回到原路径按原谓词封口）。
func TestCursorShellBridgeF14FirstThenStarted(t *testing.T) {
	defs := [][]byte{cursorTestBuildToolDef(t, "shell", cursorShellTestSchemaArray)}
	// f14 先到：无任何 started 已登记。
	d := cursorShellTestNewDuplex(t, defs, "")

	sub := cursorShellTestBuildShellArgs(t, "ls -la", "/work", "call_shell_1", "list files")
	parts := cursorShellTestBuildExecParts(t, 7, []byte("exec-orig-7"), sub)

	tc, err := d.cursorCaptureShellArgs(parts, 7, []byte("exec-orig-7"))
	require.NoError(t, err, "f14 先到（无 started）：不得报错、不得失败关闭")
	require.Equal(t, "call_shell_1", tc.ID)

	// 进入 shellOnly 专属批：snapshot 含该委托，且没有长期 pending。
	require.Equal(t, 1, cursorShellTestShellOnlyBatchCount(t, d.delegations), "f14 先到必须建立 shellOnly 专属批")
	snap := d.delegations.snapshot()
	require.Len(t, snap, 1, "shellOnly 批必须含该委托")
	require.Equal(t, "call_shell_1", snap[0].ID)
	require.Equal(t, "shell", snap[0].Kind)
	// shellOnly 批 ≥1 委托即立即可封口，无需 started/listed（区别于常规批挂起）。
	require.NoError(t, d.delegations.sealable(), "shellOnly 批 ≥1 委托必须立即可封口（不等 started/listed）")

	// started 随后到达：同 call_id 的 shellOnly 委托并入该 started 批，shellOnly 批移除。
	require.NoError(t, d.delegations.noteToolCallStarted("", "call_shell_1", true))
	require.Equal(t, 0, cursorShellTestShellOnlyBatchCount(t, d.delegations), "started 到达后 shellOnly 批必须并入并移除")

	// 委托出现在该 started 所在批（snapshot 命中，Kind=shell），且不再有独立 shellOnly 批。
	snap = d.delegations.snapshot()
	require.Len(t, snap, 1)
	require.Equal(t, "call_shell_1", snap[0].ID)
	require.Equal(t, "shell", snap[0].Kind)

	// 补 listed=1 → 该 started 批按原谓词可封口（验证完整链路：f14 先到也能正确收口）。
	require.NoError(t, d.delegations.noteToolRequestsListed("", 1))
	require.NoError(t, d.delegations.sealable(), "started 到达并入后批次必须可封口")
}

// cursorShellTestShellOnlyBatchCount 是测试辅助：统计未封口的 shellOnly 批数量。
func cursorShellTestShellOnlyBatchCount(t *testing.T, d *cursorToolDelegations) int {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, b := range d.all {
		if !b.sealed && b.shellOnly {
			n++
		}
	}
	return n
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
	require.Equal(t, 0, cursorShellTestShellOnlyBatchCount(t, d.delegations), "started 先到：不建 shellOnly 批（走原路径）")
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

// 11. 连续两个 f14（无 started）→ 各自建立独立 shellOnly 批、各自立即可封口、互不吞并。
func TestCursorShellBridgeTwoF14Independent(t *testing.T) {
	defs := [][]byte{cursorTestBuildToolDef(t, "shell", cursorShellTestSchemaArray)}
	// 两个 f14 都先到，无任何 started 已登记。
	d := cursorShellTestNewDuplex(t, defs, "")

	// 第一个 f14（call_a）。
	subA := cursorShellTestBuildShellArgs(t, "ls -la", "/work", "call_a", "list a")
	partsA := cursorShellTestBuildExecParts(t, 7, []byte("exec-a"), subA)
	tcA, err := d.cursorCaptureShellArgs(partsA, 7, []byte("exec-a"))
	require.NoError(t, err, "第一个 f14 必须成功")
	require.Equal(t, "call_a", tcA.ID)

	// 第二个 f14（call_b）：前批已因交付而 sealed 才会新建；此处尚未 seal，故同 call_id
	// 不会重复建批——用不同 call_id 模拟两条独立 shell 执行请求。
	subB := cursorShellTestBuildShellArgs(t, "pwd", "/work", "call_b", "list b")
	partsB := cursorShellTestBuildExecParts(t, 8, []byte("exec-b"), subB)
	tcB, err := d.cursorCaptureShellArgs(partsB, 8, []byte("exec-b"))
	require.NoError(t, err, "第二个 f14 必须成功")
	require.Equal(t, "call_b", tcB.ID)

	// 两个独立 shellOnly 批，各自含一条委托，互不吞并。
	require.Equal(t, 2, cursorShellTestShellOnlyBatchCount(t, d.delegations), "两个 f14 必须各自建立独立 shellOnly 批")
	snap := d.delegations.snapshot()
	require.Len(t, snap, 2, "snapshot 必须含两条委托，互不吞并")
	kinds := map[string]string{}
	for _, c := range snap {
		kinds[c.ID] = c.Kind
	}
	require.Equal(t, "shell", kinds["call_a"])
	require.Equal(t, "shell", kinds["call_b"])
	// 各自立即可封口（无需 started/listed）。
	require.NoError(t, d.delegations.sealable(), "两条 shellOnly 批分别 ≥1 委托，必须立即可封口")

	// 模拟交付：seal 逐批封口（shellOnly 与常规批隔离，不影响彼此）。
	require.NoError(t, d.delegations.seal(), "两条 shellOnly 批必须可整体封口交付")
}

// 12. shellOnly 批不得把后续 mcp 委托吞进已封口/独立批（mcp 与 shell 隔离）。
func TestCursorShellBridgeShellOnlyNotSwallowMcp(t *testing.T) {
	defs := [][]byte{
		cursorTestBuildToolDef(t, "shell", cursorShellTestSchemaArray),
		cursorTestBuildToolDef(t, "get_weather", `{"type":"object","properties":{"city":{"type":"string"}}}`),
	}
	// 先有一个 shellOnly 批（f14 先到，无 started）。
	d := cursorShellTestNewDuplex(t, defs, "")
	sub := cursorShellTestBuildShellArgs(t, "ls -la", "", "call_shell_1", "desc")
	parts := cursorShellTestBuildExecParts(t, 7, []byte("exec-7"), sub)
	_, err := d.cursorCaptureShellArgs(parts, 7, []byte("exec-7"))
	require.NoError(t, err, "shell f14 先到必须成功")
	require.Equal(t, 1, cursorShellTestShellOnlyBatchCount(t, d.delegations), "必须先有一个 shellOnly 批")
	// shellOnly 批单独出现时立即可封口（无 mcp 竞争批时独立交付，不等 started/listed）。
	require.NoError(t, d.delegations.sealable(), "仅 shellOnly 批时立即可封口（独立交付）")

	// 随后到达一条 mcp 委托（Kind 空，call_mcp），走 noteDelegation 原路径。
	mcpCap := cursorToolCallCapture{ID: "call_mcp", Name: "get_weather", ArgsJSON: `{"city":"x"}`}
	require.NoError(t, d.delegations.noteDelegation("", mcpCap), "mcp 委托必须走原路径")
	require.Equal(t, 1, cursorShellTestShellOnlyBatchCount(t, d.delegations), "mcp 委托不得被吞进 shellOnly 批（批数不变）")

	// mcp 委托在独立批（"" key），不在任何 shellOnly 批内。
	snap := d.delegations.snapshot()
	require.Len(t, snap, 2, "snapshot 必须含 shell + mcp 两条委托")
	kinds := map[string]string{}
	for _, c := range snap {
		kinds[c.ID] = c.Kind
	}
	require.Equal(t, "shell", kinds["call_shell_1"], "shell 委托仍在 shellOnly 批")
	require.Equal(t, "", kinds["call_mcp"], "mcp 委托 Kind 必须为空（字节级行为不变）")
	// mcp 委托不属于任何 shellOnly 批：以 open 映射核验。
	d.delegations.mu.Lock()
	_, mcpInShellOnly := d.delegations.open["call_mcp"]
	d.delegations.mu.Unlock()
	require.False(t, mcpInShellOnly, "mcp 委托不得落在 shellOnly 批的 key 上")
	// 二者隔离：shellOnly 批仍是独立的一批，未被 mcp 合并且未被 mcp 阻断其自身封口判定输入。
	require.Equal(t, 2, len(d.delegations.all), "必须存在两个独立批（shellOnly + mcp），互不吞并")
}

// 12. mcp 委托路径回归：Kind=="" 走 noteDelegation，逐字节不变、不建 shellOnly 批
func TestCursorShellBridgeMcpPathUnaffected(t *testing.T) {
	d := newLocalCursorToolDelegations()
	require.NoError(t, d.noteToolCallStarted("mc", "mcp_call", true))
	require.NoError(t, d.noteToolRequestsListed("mc", 1))

	// mcp 委托（Kind 空）直接 noteDelegation，不建/不落入 shellOnly 批。
	mcpCap := cursorToolCallCapture{ID: "mcp_call", Name: "get_weather", ArgsJSON: `{"city":"x"}`}
	require.NoError(t, d.noteDelegation("", mcpCap))
	require.Equal(t, 0, cursorShellTestShellOnlyBatchCount(t, d), "mcp 委托不得建立/落入 shellOnly 批")

	// mcp 路径封口语义不变（字节级行为不变，无 pending 干扰）。
	require.NoError(t, d.sealable(), "mcp 委托路径封口语义不变")
	snap := d.snapshot()
	require.Len(t, snap, 1)
	require.Equal(t, "", snap[0].Kind, "mcp 委托 Kind 必须仍为空（字节级行为不变）")
}

// 13. exec 终止控制帧（AgentClientMessage.f5 stream_close）编码器：字节级相等 + 反向解码 + 常量值守卫
func TestCursorShellBridgeStreamCloseFrame(t *testing.T) {
	// 逐字节相等：execID=7 → 0x0a 0x02 0x08 0x07
	require.Equal(t, []byte{0x0a, 0x02, 0x08, 0x07}, cursorEncodeExecStreamClose(7),
		"execID=7 的终止控制帧字节必须等于 {0x0a,0x02,0x08,0x07}")
	// execID=300（varint 两字节 0xAC 0x02）→ 0x0a 0x03 0x08 0xAC 0x02
	require.Equal(t, []byte{0x0a, 0x03, 0x08, 0xAC, 0x02}, cursorEncodeExecStreamClose(300),
		"execID=300 的终止控制帧字节必须等于 {0x0a,0x03,0x08,0xAC,0x02}")

	// 反向按字段解码：外层 cursorDecodeProtobuf → cursorFindField(payload, cursorFldControlStreamClose)
	// 存在且 Number == 1；其内层 cursorFindField(..., cursorFldStreamCloseID) 的 varint 等于传入 execID。
	for _, execID := range []uint64{7, 300} {
		payload := cursorEncodeExecStreamClose(execID)
		fields, err := cursorDecodeProtobuf(payload)
		require.NoError(t, err)
		ctrl, ok := cursorFindField(fields, cursorFldControlStreamClose)
		require.True(t, ok, "外层必须含 f1 stream_close")
		require.Equal(t, 1, ctrl.Number, "stream_close 字段号必须为 1")
		innerFields, err := cursorDecodeProtobuf(ctrl.Bytes)
		require.NoError(t, err)
		idF, ok := cursorFindField(innerFields, cursorFldStreamCloseID)
		require.True(t, ok, "内层必须含 f1 id")
		require.EqualValues(t, execID, idF.Varint, "内层 id 的 varint 必须等于传入 execID")
	}

	// 三个常量的值断言（防止后续被误改）。
	require.Equal(t, 5, cursorFldClientControlMessage, "cursorFldClientControlMessage 必须为 5")
	require.Equal(t, 1, cursorFldControlStreamClose, "cursorFldControlStreamClose 必须为 1")
	require.Equal(t, 1, cursorFldStreamCloseID, "cursorFldStreamCloseID 必须为 1")
}

// 14. sendExecStreamClose 经 d.send(5, …) 生成的外层 AgentClientMessage 信封字段号回归断言。
//
// 历史上本项目曾因"内层误用信封字段号"发生过 180s 挂死，故外层字段号必须有确定性断言：
// 必须写出且仅写出一帧 envelope；其 Data 必须是 AgentClientMessage.f5 控制消息（绝非 f2 ExecClientMessage）；
// 内层必须是 ExecClientControlMessage.f1 stream_close，且不夹带 f2 throw / f3 heartbeat；id 必须正确回传；
// 整段字节逐字节相等（最硬的守卫）。
func TestCursorShellBridgeSendStreamCloseEnvelope(t *testing.T) {
	pr, pw := io.Pipe()
	d := newWritebackDuplex(pr, pw, newCursorToolDelegations())
	stop := captureDuplex(t, d)
	require.NoError(t, d.sendExecStreamClose(7))
	wire := stop()

	// a. wire 恰好是一帧 envelope。
	frame, next, err := cursorReadEnvelopeFrame(wire, 0)
	require.NoError(t, err)
	require.Equal(t, len(wire), next, "只应写出一帧")

	// b. 外层字段号必须是 5（AgentClientMessage.f5 控制消息），绝非 f2 ExecClientMessage。
	payload, _ := cursorDecodeProtobuf(frame.Data)
	f5, ok := cursorFindField(payload, 5)
	require.True(t, ok, "外层必须是 f5 AgentClientMessage 控制消息")
	_, hasF2 := cursorFindField(payload, 2)
	require.False(t, hasF2, "外层必须是 f5 控制消息，绝非 f2 ExecClientMessage")

	// c. 内层必须是 f1 stream_close；且不得夹带 f2 throw / f3 heartbeat 等误用信封字段。
	ctrl, _ := cursorDecodeProtobuf(f5.Bytes)
	sc, ok := cursorFindField(ctrl, 1)
	require.True(t, ok, "ExecClientControlMessage.f1 = stream_close")
	_, hasThrow := cursorFindField(ctrl, 2)
	require.False(t, hasThrow, "不得夹带 f2 throw")
	_, hasHb := cursorFindField(ctrl, 3)
	require.False(t, hasHb, "不得夹带 f3 heartbeat")

	// d. id 回传正确。
	closeInner, _ := cursorDecodeProtobuf(sc.Bytes)
	require.EqualValues(t, 7, cursorVarintField(closeInner, 1))

	// e. 整段字节逐字节相等（最硬的守卫）。
	require.Equal(t, []byte{0x2a, 0x04, 0x0a, 0x02, 0x08, 0x07}, frame.Data)
	require.Equal(t, byte(0), frame.Flags, "envelope flags 必须为 0（不压缩）")
}

// ---------------------------------------------------------------------------
// BASH-ALIAS（2026-10-29）：真实客户端（CodeBuddy CLI / Claude Code CLI，UA
// CLI/2.178.1 CodeBuddy/2.178.1）声明的 shell 工具名为大写 "Bash"（input_schema
// properties.command 为 string，required=["command"]）。原名单仅有小写 "bash"，
// 精确匹配失败 → ErrCursorProtocol → 整条 HTTP 502。本组验证名单增项后的行为。
// ---------------------------------------------------------------------------

//  15. 仅声明 Bash（command=string）→ 捕获成功，Name=="Bash"，
//     ArgsJSON=="{\"command\":\"<原命令>\"}"（string 原样、无 workdir 省略）。
func TestCursorShellBridgeBashUppercaseSelection(t *testing.T) {
	defs := [][]byte{cursorTestBuildToolDef(t, "Bash", `{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)}
	d := cursorShellTestNewDuplex(t, defs, "call_bash_1")

	sub := cursorShellTestBuildShellArgs(t, "echo hello", "", "call_bash_1", "run echo")
	parts := cursorShellTestBuildExecParts(t, 13, []byte("exec-13"), sub)

	tc, err := d.cursorCaptureShellArgs(parts, 13, []byte("exec-13"))
	require.NoError(t, err, "仅声明 Bash 必须成功委托（大小写敏感匹配名单新增项）")
	require.Equal(t, "Bash", tc.Name, "工具名须按优先级选中 Bash（大写）")
	// command 目标是 string → 原字符串；schema 无 workdir 属性 → 省略。
	require.Equal(t, `{"command":"echo hello"}`, tc.ArgsJSON, "ArgsJSON 必须为原样 string command、无 workdir")
	require.Equal(t, "call_bash_1", tc.ID)
	require.Equal(t, "shell", tc.Kind)
	require.EqualValues(t, 13, tc.ExecID)
	require.Equal(t, []byte("exec-13"), tc.IDField)
}

// 16. 同时声明 bash 与 Bash → 选 bash（加名不夺位：小写优先于大写，优先级次序不变）。
func TestCursorShellBridgeBashLowerWinsOverUpper(t *testing.T) {
	defs := [][]byte{
		cursorTestBuildToolDef(t, "bash", `{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
		cursorTestBuildToolDef(t, "Bash", `{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
	}
	d := cursorShellTestNewDuplex(t, defs, "call_bash_1")

	sub := cursorShellTestBuildShellArgs(t, "echo hi", "", "call_bash_1", "run echo")
	parts := cursorShellTestBuildExecParts(t, 14, []byte("exec-14"), sub)

	tc, err := d.cursorCaptureShellArgs(parts, 14, []byte("exec-14"))
	require.NoError(t, err, "同时声明 bash 与 Bash 必须成功委托")
	require.Equal(t, "bash", tc.Name, "小写 bash 仍优先于大写 Bash（加名不夺位）")
	require.Equal(t, `{"command":"echo hi"}`, tc.ArgsJSON)
}

// 17. 声明 Bash 但 properties 无 command → 仍失败关闭，错误含既有 "no shell-like tool" 文案。
func TestCursorShellBridgeBashNoCommandPropertyFailClosed(t *testing.T) {
	// properties 无 command（只有 cmd），故即便名为 Bash 也不被选中 → 名单空 → 失败关闭。
	defs := [][]byte{cursorTestBuildToolDef(t, "Bash", `{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}`)}
	d := cursorShellTestNewDuplex(t, defs, "call_bash_1")

	sub := cursorShellTestBuildShellArgs(t, "echo hi", "", "call_bash_1", "run echo")
	parts := cursorShellTestBuildExecParts(t, 15, []byte("exec-15"), sub)

	_, err := d.cursorCaptureShellArgs(parts, 15, []byte("exec-15"))
	require.Error(t, err, "Bash 声明但无 command property 必须失败关闭（schema 校验不变）")
	require.True(t, errors.Is(err, ErrCursorProtocol), "失败必须落到 ErrCursorProtocol")
	require.Contains(t, err.Error(), "no shell-like tool", "错误须含既有 'no shell-like tool' 文案")
}
