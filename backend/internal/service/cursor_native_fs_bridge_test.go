//go:build unit

package service

// Cursor 原生文件工具桥接（ExecServerMessage.f3/f4/f5/f7/f8）定向单测。
//
// 全部走真实生产函数：
//   - 委托捕获：d.cursorCaptureNativeFsArgs（cursor_native_fs_bridge.go）
//   - 工具名/schema 选择 + 参数转换：cursorBuildNativeFsDelegation
//   - 续轮回写：cursorEncodeNativeFsResult（经 cursorAgentRunIntake.DeliverToolResults 真实写路径）
//   - 封口：noteNativeFsDelegation（与 shell 同一"独立批立即可封口"语义）
//
// 覆盖五种 Kind 的捕获成功/失败路径与回写成功/失败路径；不依赖外部网络与 DB。

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

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

// cursorNativeFsTestResult 是一条原生结果帧的解码视图。
type cursorNativeFsTestResult struct {
	execID  uint64
	idField []byte
	field   int
	oneof   []cursorPBField
}

// cursorNativeFsTestDecodeResult 解出 ExecClientMessage 里的原生 result 字段（3/4/5/7/8）。
func cursorNativeFsTestDecodeResult(t *testing.T, fields []cursorPBField) cursorNativeFsTestResult {
	t.Helper()
	out := cursorNativeFsTestResult{execID: cursorVarintField(fields, cursorFldClientMsgID)}
	if id, ok := cursorFindField(fields, cursorFldClientMsgExecID); ok {
		out.idField = append([]byte(nil), id.Bytes...)
	}
	for _, n := range []int{cursorFldClientMsgWriteResult, cursorFldClientMsgDeleteResult,
		cursorFldClientMsgGrepResult, cursorFldClientMsgReadResult, cursorFldClientMsgLsResult} {
		if f, ok := cursorFindField(fields, n); ok {
			out.field = n
			inner, err := cursorDecodeProtobuf(f.Bytes)
			require.NoError(t, err)
			out.oneof = inner
			return out
		}
	}
	t.Fatalf("ExecClientMessage 未携带任何原生文件 result 字段：%+v", fields)
	return out
}

// cursorNativeFsTestIntake 构造一个只做真实写路径的 intake（svc/session 来自既有生命周期测试夹具）。
func cursorNativeFsTestIntake(t *testing.T, caps ...cursorToolCallCapture) (*cursorSessionRecord, *cursorAgentRunIntake, func() []byte) {
	t.Helper()
	svc := newCursorLifecycleTestService()
	record := startCursorLifecycleTestSession(t, svc)
	delegations := newCursorToolDelegations()
	for _, c := range caps {
		require.NoError(t, delegations.noteNativeFsDelegation(c.Kind, c.ID, c))
	}
	pr, pw := io.Pipe()
	d := newWritebackDuplex(pr, pw, delegations)
	require.NoError(t, svc.installIntake(record.id, &cursorAgentRunIntake{
		svc: svc, session: record, duplex: d, delegations: delegations,
	}))
	return record, record.intake.(*cursorAgentRunIntake), captureDuplex(t, d)
}

// 各 Kind 的下游工具 schema（属性名刻意与原生 Args 不同，验证按 schema 改名映射）。
const (
	cursorNativeFsTestReadSchema  = `{"type":"object","properties":{"file_path":{"type":"string"},"offset":{"type":"number"},"limit":{"type":"number"}},"required":["file_path"]}`
	cursorNativeFsTestWriteSchema = `{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path","content"]}`
	cursorNativeFsTestLsSchema    = `{"type":"object","properties":{"path":{"type":"string"},"ignore":{"type":"array","items":{"type":"string"}}}}`
	cursorNativeFsTestGrepSchema  = `{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"},"glob":{"type":"string"},"output_mode":{"type":"string"},"case_insensitive":{"type":"boolean"}},"required":["pattern"]}`
	cursorNativeFsTestDelSchema   = `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`
)

// ---------------------------------------------------------------------------
// 1. 捕获：五种字段号各自解码 → 选下游同名工具 → 按 schema 映射参数
// ---------------------------------------------------------------------------

func TestCursorNativeFSCaptureRead(t *testing.T) {
	defs := [][]byte{
		cursorTestBuildToolDef(t, "read_file", cursorNativeFsTestReadSchema),
		cursorTestBuildToolDef(t, "write_file", cursorNativeFsTestWriteSchema),
	}
	d := cursorShellTestNewDuplex(t, defs, "")

	sub := cursorAppendStringField(nil, 1, "/repo/main.go")
	sub = cursorAppendStringField(sub, 2, "call_read_1")
	sub = cursorAppendVarintField(sub, 4, 10) // offset
	sub = cursorAppendVarintField(sub, 5, 20) // limit
	parts := cursorNativeFsTestExecParts(t, 71, []byte("exec-71"), cursorFldServerMsgReadArgs, sub)

	tc, err := d.cursorCaptureNativeFsArgs(parts, cursorFldServerMsgReadArgs, 71, []byte("exec-71"))
	require.NoError(t, err)
	require.Equal(t, cursorToolKindRead, tc.Kind)
	require.Equal(t, "call_read_1", tc.ID)
	require.Equal(t, "read_file", tc.Name)
	require.EqualValues(t, 71, tc.ExecID)
	require.Equal(t, []byte("exec-71"), tc.IDField)
	// 原生 f1 path → 目标 schema 的 file_path；offset/limit 透传。
	require.JSONEq(t, `{"file_path":"/repo/main.go","offset":10,"limit":20}`, tc.ArgsJSON)
}

func TestCursorNativeFSCaptureLs(t *testing.T) {
	defs := [][]byte{cursorTestBuildToolDef(t, "ls", cursorNativeFsTestLsSchema)}
	d := cursorShellTestNewDuplex(t, defs, "")

	sub := cursorAppendStringField(nil, 1, "/repo")
	sub = cursorAppendStringField(sub, 2, "node_modules")
	sub = cursorAppendStringField(sub, 2, ".git")
	sub = cursorAppendStringField(sub, 3, "call_ls_1")
	parts := cursorNativeFsTestExecParts(t, 72, nil, cursorFldServerMsgLsArgs, sub)

	tc, err := d.cursorCaptureNativeFsArgs(parts, cursorFldServerMsgLsArgs, 72, nil)
	require.NoError(t, err)
	require.Equal(t, cursorToolKindLs, tc.Kind)
	require.Equal(t, "ls", tc.Name)
	require.JSONEq(t, `{"path":"/repo","ignore":["node_modules",".git"]}`, tc.ArgsJSON)
}

func TestCursorNativeFSCaptureGrep(t *testing.T) {
	defs := [][]byte{cursorTestBuildToolDef(t, "grep", cursorNativeFsTestGrepSchema)}
	d := cursorShellTestNewDuplex(t, defs, "")

	sub := cursorAppendStringField(nil, 1, "TODO")
	sub = cursorAppendStringField(sub, 2, "/repo")
	sub = cursorAppendStringField(sub, 3, "*.go")
	sub = cursorAppendStringField(sub, 4, "content")
	sub = cursorAppendVarintField(sub, 8, 1) // case_insensitive
	sub = cursorAppendStringField(sub, 14, "call_grep_1")
	parts := cursorNativeFsTestExecParts(t, 73, nil, cursorFldServerMsgGrepArgs, sub)

	tc, err := d.cursorCaptureNativeFsArgs(parts, cursorFldServerMsgGrepArgs, 73, nil)
	require.NoError(t, err)
	require.Equal(t, cursorToolKindGrep, tc.Kind)
	require.Equal(t, "grep", tc.Name)
	require.JSONEq(t,
		`{"pattern":"TODO","path":"/repo","glob":"*.go","output_mode":"content","case_insensitive":true}`,
		tc.ArgsJSON)
}

func TestCursorNativeFSCaptureWrite(t *testing.T) {
	defs := [][]byte{cursorTestBuildToolDef(t, "write_file", cursorNativeFsTestWriteSchema)}
	d := cursorShellTestNewDuplex(t, defs, "")

	sub := cursorAppendStringField(nil, 1, "/repo/a.txt")
	sub = cursorAppendStringField(sub, 2, "line1\nline2\n")
	sub = cursorAppendStringField(sub, 3, "call_write_1")
	parts := cursorNativeFsTestExecParts(t, 74, nil, cursorFldServerMsgWriteArgs, sub)

	tc, err := d.cursorCaptureNativeFsArgs(parts, cursorFldServerMsgWriteArgs, 74, nil)
	require.NoError(t, err)
	require.Equal(t, cursorToolKindWrite, tc.Kind)
	require.Equal(t, "write_file", tc.Name)
	require.JSONEq(t, `{"file_path":"/repo/a.txt","content":"line1\nline2\n"}`, tc.ArgsJSON)
}

func TestCursorNativeFSCaptureWriteFromFileBytes(t *testing.T) {
	defs := [][]byte{cursorTestBuildToolDef(t, "write_file", cursorNativeFsTestWriteSchema)}
	d := cursorShellTestNewDuplex(t, defs, "")

	// file_text 为空时回落到 f5 file_bytes（真实客户端二选一）。
	sub := cursorAppendStringField(nil, 1, "/repo/b.txt")
	sub = cursorAppendBytesField(sub, 5, []byte("from-bytes\n"))
	sub = cursorAppendStringField(sub, 3, "call_write_2")
	parts := cursorNativeFsTestExecParts(t, 75, nil, cursorFldServerMsgWriteArgs, sub)

	tc, err := d.cursorCaptureNativeFsArgs(parts, cursorFldServerMsgWriteArgs, 75, nil)
	require.NoError(t, err)
	require.JSONEq(t, `{"file_path":"/repo/b.txt","content":"from-bytes\n"}`, tc.ArgsJSON)
}

func TestCursorNativeFSCaptureDelete(t *testing.T) {
	defs := [][]byte{cursorTestBuildToolDef(t, "delete_file", cursorNativeFsTestDelSchema)}
	d := cursorShellTestNewDuplex(t, defs, "")

	sub := cursorAppendStringField(nil, 1, "/repo/old.txt")
	sub = cursorAppendStringField(sub, 2, "call_del_1")
	parts := cursorNativeFsTestExecParts(t, 76, nil, cursorFldServerMsgDeleteArgs, sub)

	tc, err := d.cursorCaptureNativeFsArgs(parts, cursorFldServerMsgDeleteArgs, 76, nil)
	require.NoError(t, err)
	require.Equal(t, cursorToolKindDelete, tc.Kind)
	require.Equal(t, "delete_file", tc.Name)
	require.JSONEq(t, `{"path":"/repo/old.txt"}`, tc.ArgsJSON)
}

// ---------------------------------------------------------------------------
// 2. 捕获失败关闭路径
// ---------------------------------------------------------------------------

// 声明集里没有对应工具 → 失败关闭（绝不回落到自造工具名）。
func TestCursorNativeFSCaptureNoMatchingTool(t *testing.T) {
	defs := [][]byte{cursorTestBuildToolDef(t, "get_weather", `{"type":"object","properties":{"city":{"type":"string"}}}`)}
	d := cursorShellTestNewDuplex(t, defs, "")
	sub := cursorAppendStringField(nil, 1, "/repo/main.go")
	sub = cursorAppendStringField(sub, 2, "call_read_x")
	parts := cursorNativeFsTestExecParts(t, 77, nil, cursorFldServerMsgReadArgs, sub)

	_, err := d.cursorCaptureNativeFsArgs(parts, cursorFldServerMsgReadArgs, 77, nil)
	require.ErrorIs(t, err, ErrCursorProtocol, "无 read 类工具必须失败关闭")
	require.Empty(t, d.delegations.snapshot(), "失败关闭时不得登记任何委托")
}

// 工具名匹配但 schema 不含关键属性（path）→ 失败关闭。
func TestCursorNativeFSCaptureToolWithoutPathProperty(t *testing.T) {
	defs := [][]byte{cursorTestBuildToolDef(t, "read_file", `{"type":"object","properties":{"content":{"type":"string"}}}`)}
	d := cursorShellTestNewDuplex(t, defs, "")
	sub := cursorAppendStringField(nil, 1, "/repo/main.go")
	sub = cursorAppendStringField(sub, 2, "call_read_y")
	parts := cursorNativeFsTestExecParts(t, 78, nil, cursorFldServerMsgReadArgs, sub)

	_, err := d.cursorCaptureNativeFsArgs(parts, cursorFldServerMsgReadArgs, 78, nil)
	require.ErrorIs(t, err, ErrCursorProtocol)
}

// 关键参数为空（read path 空）→ 失败关闭。
func TestCursorNativeFSCaptureEmptyPath(t *testing.T) {
	defs := [][]byte{cursorTestBuildToolDef(t, "read_file", cursorNativeFsTestReadSchema)}
	d := cursorShellTestNewDuplex(t, defs, "")
	sub := cursorAppendStringField(nil, 2, "call_read_z")
	parts := cursorNativeFsTestExecParts(t, 79, nil, cursorFldServerMsgReadArgs, sub)

	_, err := d.cursorCaptureNativeFsArgs(parts, cursorFldServerMsgReadArgs, 79, nil)
	require.ErrorIs(t, err, ErrCursorProtocol)
}

// 目标 schema 的 required 字段无法由原生 Args 填充 → 失败关闭（不猜默认值）。
func TestCursorNativeFSCaptureRequiredUnfillable(t *testing.T) {
	defs := [][]byte{cursorTestBuildToolDef(t, "write_file",
		`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"},"reason":{"type":"string"}},"required":["path","content","reason"]}`)}
	d := cursorShellTestNewDuplex(t, defs, "")
	sub := cursorAppendStringField(nil, 1, "/repo/a.txt")
	sub = cursorAppendStringField(sub, 2, "x")
	sub = cursorAppendStringField(sub, 3, "call_write_z")
	parts := cursorNativeFsTestExecParts(t, 80, nil, cursorFldServerMsgWriteArgs, sub)

	_, err := d.cursorCaptureNativeFsArgs(parts, cursorFldServerMsgWriteArgs, 80, nil)
	require.ErrorIs(t, err, ErrCursorProtocol)
}

// tool_call_id 为空 → 失败关闭（与 shell 同语义）。
func TestCursorNativeFSCaptureMissingCallID(t *testing.T) {
	defs := [][]byte{cursorTestBuildToolDef(t, "ls", cursorNativeFsTestLsSchema)}
	d := cursorShellTestNewDuplex(t, defs, "")
	sub := cursorAppendStringField(nil, 1, "/repo")
	parts := cursorNativeFsTestExecParts(t, 81, nil, cursorFldServerMsgLsArgs, sub)

	_, err := d.cursorCaptureNativeFsArgs(parts, cursorFldServerMsgLsArgs, 81, nil)
	require.ErrorIs(t, err, ErrCursorProtocol)
}

// ---------------------------------------------------------------------------
// 3. 封口：无 started 时建独立批并立即可封口（与 shell 同一模式）
// ---------------------------------------------------------------------------

func TestCursorNativeFSCaptureStandaloneBatchSealable(t *testing.T) {
	defs := [][]byte{cursorTestBuildToolDef(t, "read_file", cursorNativeFsTestReadSchema)}
	d := cursorShellTestNewDuplex(t, defs, "")
	sub := cursorAppendStringField(nil, 1, "/repo/main.go")
	sub = cursorAppendStringField(sub, 2, "call_read_solo")
	parts := cursorNativeFsTestExecParts(t, 82, nil, cursorFldServerMsgReadArgs, sub)

	tc, err := d.cursorCaptureNativeFsArgs(parts, cursorFldServerMsgReadArgs, 82, nil)
	require.NoError(t, err)
	require.Equal(t, 1, cursorShellTestShellOnlyBatchCount(t, d.delegations), "必须建立独立批")
	snap := d.delegations.snapshot()
	require.Len(t, snap, 1)
	require.Equal(t, tc.ID, snap[0].ID)
	require.NoError(t, d.delegations.sealable(), "独立批 ≥1 委托必须立即可封口（不等 started/listed）")
	require.NoError(t, d.delegations.seal())

	// started 先到的形态：走原批路径，不建独立批。
	d2 := cursorShellTestNewDuplex(t, defs, "call_read_started")
	sub2 := cursorAppendStringField(nil, 1, "/repo/main.go")
	sub2 = cursorAppendStringField(sub2, 2, "call_read_started")
	parts2 := cursorNativeFsTestExecParts(t, 83, nil, cursorFldServerMsgReadArgs, sub2)
	_, err = d2.cursorCaptureNativeFsArgs(parts2, cursorFldServerMsgReadArgs, 83, nil)
	require.NoError(t, err)
	require.Equal(t, 0, cursorShellTestShellOnlyBatchCount(t, d2.delegations), "有 started 时不建独立批")
}

// ---------------------------------------------------------------------------
// 4. 回写：read
// ---------------------------------------------------------------------------

func TestCursorNativeFSWritebackReadSuccess(t *testing.T) {
	content := "package main\n\nfunc main() {}\n"
	record, _, stop := cursorNativeFsTestIntake(t, cursorToolCallCapture{
		ID: "call_r", Name: "read_file", ArgsJSON: `{"file_path":"/repo/main.go"}`,
		ExecID: 91, IDField: []byte{0xb1}, Kind: cursorToolKindRead,
	})
	require.NoError(t, record.intake.DeliverToolResults(context.Background(), record.id, cursorSessionLevelBatchID,
		[]CursorToolResult{{CallID: "call_r", Text: []string{content}}}))
	frames := cursorNativeFsTestSplit(t, stop())
	require.Len(t, frames, 2, "结果帧 + stream_close 控制帧")

	res := cursorNativeFsTestDecodeResult(t, frames[0])
	require.Equal(t, cursorFldClientMsgReadResult, res.field, "read 结果必须走 f7")
	require.EqualValues(t, 91, res.execID)
	require.Equal(t, []byte{0xb1}, res.idField)
	success, ok := cursorFindField(res.oneof, cursorFldReadResultSuccess)
	require.True(t, ok, "成功结果必须走 f1 success")
	inner, err := cursorDecodeProtobuf(success.Bytes)
	require.NoError(t, err)
	require.Equal(t, "/repo/main.go", string(cursorFindFieldBytes(t, inner, cursorFldReadSuccessPath)))
	require.Equal(t, content, string(cursorFindFieldBytes(t, inner, cursorFldReadSuccessContent)))
	require.EqualValues(t, 3, cursorVarintField(inner, cursorFldReadSuccessTotalLines), "行数按 wc -l 语义（末尾换行不算新行）")
	require.EqualValues(t, len(content), cursorVarintField(inner, cursorFldReadSuccessFileSize))

	// 第二帧必须是 exec 终止控制帧（f5），否则上游不收尾。
	ctrl, ok := cursorFindField(frames[1], cursorFldClientControlMessage)
	require.True(t, ok, "结果写完后必须补 stream_close")
	ctrlInner, err := cursorDecodeProtobuf(ctrl.Bytes)
	require.NoError(t, err)
	sc, ok := cursorFindField(ctrlInner, cursorFldControlStreamClose)
	require.True(t, ok)
	scInner, err := cursorDecodeProtobuf(sc.Bytes)
	require.NoError(t, err)
	require.EqualValues(t, 91, cursorVarintField(scInner, cursorFldStreamCloseID))
}

// cursorNativeFsTestDecodeReadSuccess 走真实回写路径（DeliverToolResults），解出 ReadSuccess 内层字段。
func cursorNativeFsTestDecodeReadSuccess(t *testing.T, text string) (content []byte, lines, size uint64) {
	t.Helper()
	record, _, stop := cursorNativeFsTestIntake(t, cursorToolCallCapture{
		ID: "call_r", Name: "read_file", ArgsJSON: `{"file_path":"/repo/main.go"}`,
		ExecID: 96, Kind: cursorToolKindRead,
	})
	require.NoError(t, record.intake.DeliverToolResults(context.Background(), record.id,
		cursorSessionLevelBatchID, []CursorToolResult{{CallID: "call_r", Text: []string{text}}}))
	res := cursorNativeFsTestDecodeResult(t, cursorNativeFsTestSplit(t, stop())[0])
	require.Equal(t, cursorFldClientMsgReadResult, res.field)
	success, ok := cursorFindField(res.oneof, cursorFldReadResultSuccess)
	require.True(t, ok, "必须走 f1 success")
	inner, err := cursorDecodeProtobuf(success.Bytes)
	require.NoError(t, err)
	return cursorFindFieldBytes(t, inner, cursorFldReadSuccessContent),
		cursorVarintField(inner, cursorFldReadSuccessTotalLines),
		cursorVarintField(inner, cursorFldReadSuccessFileSize)
}

// ① 带行号展示格式 → content 剥成原文，total_lines / file_size 按原文算。
func TestCursorNativeFSWritebackReadStripsLineNumberGutter(t *testing.T) {
	display := "   1→def f():\n   2→    return 1\n   3→\n"
	want := "def f():\n    return 1\n\n"
	content, lines, size := cursorNativeFsTestDecodeReadSuccess(t, display)
	require.Equal(t, want, string(content), "展示行号必须剥掉，content 是文件原文")
	require.EqualValues(t, 3, lines, "行数按剥离后的原文算")
	require.EqualValues(t, len(want), size, "file_size 按剥离后的原文字节数算")
}

// ② 无行号前缀的原文 → 原样保留（字节不变，零命中不剥离）。
func TestCursorNativeFSWritebackReadKeepsPlainContent(t *testing.T) {
	plain := "package main\n\nfunc main() {}\n"
	content, lines, size := cursorNativeFsTestDecodeReadSuccess(t, plain)
	require.Equal(t, plain, string(content), "零命中时必须字节不变")
	require.EqualValues(t, 3, lines)
	require.EqualValues(t, len(plain), size)
}

// ③ 混合：部分行有行号前缀、部分没有 → 只剥匹配行。
func TestCursorNativeFSWritebackReadStripsMixedLines(t *testing.T) {
	mixed := "   1→a := 1\nraw line\n  12→b := 2\n"
	want := "a := 1\nraw line\nb := 2\n"
	content, lines, size := cursorNativeFsTestDecodeReadSuccess(t, mixed)
	require.Equal(t, want, string(content), "只剥匹配行，非匹配行原样保留")
	require.EqualValues(t, 3, lines)
	require.EqualValues(t, len(want), size)
}

// ④ 行号前缀形态变体：1→ / "  12→" / " 123→" 都能剥（可选前导空白 + 数字 + U+2192）。
func TestCursorNativeFSWritebackReadStripsLineNumberVariants(t *testing.T) {
	for _, tc := range []struct{ name, display, want string }{
		{"no_padding", "1→x\n", "x\n"},
		{"two_spaces", "  12→y\n", "y\n"},
		{"one_space", " 123→z\n", "z\n"},
		{"tab", "\t7→tabbed\n", "tabbed\n"},
		{"no_trailing_newline", "42→last", "last"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, cursorStripReadLineNumberPrefixes(tc.display))
		})
	}
}

func TestCursorNativeFSWritebackReadError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		result  CursorToolResult
		wantMsg string
	}{
		{"is_error", CursorToolResult{CallID: "call_r", Text: []string{"permission denied"}, IsError: true}, "permission denied"},
		{"error_prefix", CursorToolResult{CallID: "call_r", Text: []string{"ERROR: no such file"}}, "ERROR: no such file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record, _, stop := cursorNativeFsTestIntake(t, cursorToolCallCapture{
				ID: "call_r", Name: "read_file", ArgsJSON: `{"file_path":"/repo/main.go"}`,
				ExecID: 92, Kind: cursorToolKindRead,
			})
			require.NoError(t, record.intake.DeliverToolResults(context.Background(), record.id,
				cursorSessionLevelBatchID, []CursorToolResult{tc.result}))
			res := cursorNativeFsTestDecodeResult(t, cursorNativeFsTestSplit(t, stop())[0])
			errFrame, ok := cursorFindField(res.oneof, cursorFldReadResultError)
			require.True(t, ok, "失败结果必须走 f2 error")
			_, hasSuccess := cursorFindField(res.oneof, cursorFldReadResultSuccess)
			require.False(t, hasSuccess, "绝不可把失败写成 success")
			inner, err := cursorDecodeProtobuf(errFrame.Bytes)
			require.NoError(t, err)
			require.Equal(t, "/repo/main.go", string(cursorFindFieldBytes(t, inner, cursorFldReadErrorPath)))
			require.Equal(t, tc.wantMsg, string(cursorFindFieldBytes(t, inner, cursorFldReadErrorError)))
		})
	}
}

// ---------------------------------------------------------------------------
// 5. 回写：write
// ---------------------------------------------------------------------------

func TestCursorNativeFSWritebackWrite(t *testing.T) {
	content := "a\nb\n"
	record, _, stop := cursorNativeFsTestIntake(t, cursorToolCallCapture{
		ID: "call_w", Name: "write_file", ArgsJSON: `{"file_path":"/repo/a.txt","content":"a\nb\n"}`,
		ExecID: 93, Kind: cursorToolKindWrite,
	})
	require.NoError(t, record.intake.DeliverToolResults(context.Background(), record.id, cursorSessionLevelBatchID,
		[]CursorToolResult{{CallID: "call_w", Text: []string{"Written 2 lines"}}}))
	res := cursorNativeFsTestDecodeResult(t, cursorNativeFsTestSplit(t, stop())[0])
	require.Equal(t, cursorFldClientMsgWriteResult, res.field, "write 结果必须走 f3")
	success, ok := cursorFindField(res.oneof, cursorFldWriteResultSuccess)
	require.True(t, ok)
	inner, err := cursorDecodeProtobuf(success.Bytes)
	require.NoError(t, err)
	require.Equal(t, "/repo/a.txt", string(cursorFindFieldBytes(t, inner, cursorFldWriteSuccessPath)))
	require.EqualValues(t, 2, cursorVarintField(inner, cursorFldWriteSuccessLinesCreate))
	require.EqualValues(t, len(content), cursorVarintField(inner, cursorFldWriteSuccessFileSize))
}

func TestCursorNativeFSWritebackWriteError(t *testing.T) {
	record, _, stop := cursorNativeFsTestIntake(t, cursorToolCallCapture{
		ID: "call_w", Name: "write_file", ArgsJSON: `{"file_path":"/repo/a.txt","content":"x"}`,
		ExecID: 94, Kind: cursorToolKindWrite,
	})
	require.NoError(t, record.intake.DeliverToolResults(context.Background(), record.id, cursorSessionLevelBatchID,
		[]CursorToolResult{{CallID: "call_w", Text: []string{"disk full"}, IsError: true}}))
	res := cursorNativeFsTestDecodeResult(t, cursorNativeFsTestSplit(t, stop())[0])
	errFrame, ok := cursorFindField(res.oneof, cursorFldWriteResultError)
	require.True(t, ok, "失败必须走 f5 error")
	inner, err := cursorDecodeProtobuf(errFrame.Bytes)
	require.NoError(t, err)
	require.Equal(t, "/repo/a.txt", string(cursorFindFieldBytes(t, inner, cursorFldWriteErrorPath)))
	require.Equal(t, "disk full", string(cursorFindFieldBytes(t, inner, cursorFldWriteErrorError)))
}

// ---------------------------------------------------------------------------
// 6. 回写：delete
// ---------------------------------------------------------------------------

func TestCursorNativeFSWritebackDelete(t *testing.T) {
	record, _, stop := cursorNativeFsTestIntake(t, cursorToolCallCapture{
		ID: "call_d", Name: "delete_file", ArgsJSON: `{"path":"/repo/old.txt"}`,
		ExecID: 95, Kind: cursorToolKindDelete,
	})
	require.NoError(t, record.intake.DeliverToolResults(context.Background(), record.id, cursorSessionLevelBatchID,
		[]CursorToolResult{{CallID: "call_d", Text: []string{"File deleted"}}}))
	res := cursorNativeFsTestDecodeResult(t, cursorNativeFsTestSplit(t, stop())[0])
	require.Equal(t, cursorFldClientMsgDeleteResult, res.field, "delete 结果必须走 f4")
	success, ok := cursorFindField(res.oneof, cursorFldDeleteResultSuccess)
	require.True(t, ok)
	inner, err := cursorDecodeProtobuf(success.Bytes)
	require.NoError(t, err)
	require.Equal(t, "/repo/old.txt", string(cursorFindFieldBytes(t, inner, cursorFldDeleteSuccessPath)))
	// 未经验证的 deleted_file/file_size/prev_content 一律不填（不编造）。
	_, hasFiles := cursorFindField(inner, 2)
	require.False(t, hasFiles)
}

func TestCursorNativeFSWritebackDeleteError(t *testing.T) {
	record, _, stop := cursorNativeFsTestIntake(t, cursorToolCallCapture{
		ID: "call_d", Name: "delete_file", ArgsJSON: `{"path":"/repo/old.txt"}`,
		ExecID: 96, Kind: cursorToolKindDelete,
	})
	require.NoError(t, record.intake.DeliverToolResults(context.Background(), record.id, cursorSessionLevelBatchID,
		[]CursorToolResult{{CallID: "call_d", Text: []string{"ERROR: not found"}}}))
	res := cursorNativeFsTestDecodeResult(t, cursorNativeFsTestSplit(t, stop())[0])
	errFrame, ok := cursorFindField(res.oneof, cursorFldDeleteResultError)
	require.True(t, ok, "失败必须走 f7 error")
	inner, err := cursorDecodeProtobuf(errFrame.Bytes)
	require.NoError(t, err)
	require.Equal(t, "ERROR: not found", string(cursorFindFieldBytes(t, inner, cursorFldDeleteErrorError)))
}

// ---------------------------------------------------------------------------
// 7. 回写：ls
// ---------------------------------------------------------------------------

func TestCursorNativeFSWritebackLs(t *testing.T) {
	record, _, stop := cursorNativeFsTestIntake(t, cursorToolCallCapture{
		ID: "call_l", Name: "ls", ArgsJSON: `{"path":"/repo"}`,
		ExecID: 97, Kind: cursorToolKindLs,
	})
	require.NoError(t, record.intake.DeliverToolResults(context.Background(), record.id, cursorSessionLevelBatchID,
		[]CursorToolResult{{CallID: "call_l", Text: []string{"src/\nREADME.md\nmain.go\n"}}}))
	res := cursorNativeFsTestDecodeResult(t, cursorNativeFsTestSplit(t, stop())[0])
	require.Equal(t, cursorFldClientMsgLsResult, res.field, "ls 结果必须走 f8")
	success, ok := cursorFindField(res.oneof, cursorFldLsResultSuccess)
	require.True(t, ok)
	inner, err := cursorDecodeProtobuf(success.Bytes)
	require.NoError(t, err)
	root, ok := cursorFindField(inner, cursorFldLsSuccessRoot)
	require.True(t, ok, "LsSuccess 必须携带 f1 directory_tree_root")
	node, err := cursorDecodeProtobuf(root.Bytes)
	require.NoError(t, err)
	require.Equal(t, "/repo", string(cursorFindFieldBytes(t, node, cursorFldLsNodeAbsPath)))
	require.EqualValues(t, 1, cursorVarintField(node, cursorFldLsNodeChildrenProcessed))
	require.EqualValues(t, 2, cursorVarintField(node, cursorFldLsNodeNumFiles))
	var dirs, files []string
	for _, f := range node {
		switch f.Number {
		case cursorFldLsNodeChildrenDirs:
			child, cerr := cursorDecodeProtobuf(f.Bytes)
			require.NoError(t, cerr)
			dirs = append(dirs, string(cursorFindFieldBytes(t, child, cursorFldLsNodeAbsPath)))
		case cursorFldLsNodeChildrenFiles:
			child, cerr := cursorDecodeProtobuf(f.Bytes)
			require.NoError(t, cerr)
			files = append(files, string(cursorFindFieldBytes(t, child, cursorFldLsFileNodePath)))
		}
	}
	require.Equal(t, []string{"src"}, dirs)
	require.Equal(t, []string{"README.md", "main.go"}, files)
}

func TestCursorNativeFSWritebackLsError(t *testing.T) {
	record, _, stop := cursorNativeFsTestIntake(t, cursorToolCallCapture{
		ID: "call_l", Name: "ls", ArgsJSON: `{"path":"/nope"}`,
		ExecID: 98, Kind: cursorToolKindLs,
	})
	require.NoError(t, record.intake.DeliverToolResults(context.Background(), record.id, cursorSessionLevelBatchID,
		[]CursorToolResult{{CallID: "call_l", Text: []string{"no such directory"}, IsError: true}}))
	res := cursorNativeFsTestDecodeResult(t, cursorNativeFsTestSplit(t, stop())[0])
	errFrame, ok := cursorFindField(res.oneof, cursorFldLsResultError)
	require.True(t, ok, "失败必须走 f2 error")
	inner, err := cursorDecodeProtobuf(errFrame.Bytes)
	require.NoError(t, err)
	require.Equal(t, "/nope", string(cursorFindFieldBytes(t, inner, cursorFldLsErrorPath)))
	require.Equal(t, "no such directory", string(cursorFindFieldBytes(t, inner, cursorFldLsErrorError)))
}

// ---------------------------------------------------------------------------
// 8. 回写：grep（真实 GrepResult.f1 success → f4 workspace_results → f3 content）
// ---------------------------------------------------------------------------

// cursorNativeFsTestGrepFrame 解出 grep 结果帧里的 GrepSuccess 与 GrepContentResult。
type cursorNativeFsTestGrepFrame struct {
	success   []cursorPBField
	workspace string
	content   []cursorPBField
}

// cursorNativeFsTestDecodeGrep 逐层解开 grep 结果帧：
// f1 success → f4 workspace_results(map entry) → f2 value(GrepUnionResult) → f3 content。
func cursorNativeFsTestDecodeGrep(t *testing.T, oneof []cursorPBField) cursorNativeFsTestGrepFrame {
	t.Helper()
	successField, ok := cursorFindField(oneof, cursorFldGrepResultSuccess)
	require.True(t, ok, "成功结果必须走 f1 success")
	success, err := cursorDecodeProtobuf(successField.Bytes)
	require.NoError(t, err)

	entryField, ok := cursorFindField(success, cursorFldGrepSuccessWorkspaceResults)
	require.True(t, ok, "GrepSuccess 必须携带 f4 workspace_results")
	entry, err := cursorDecodeProtobuf(entryField.Bytes)
	require.NoError(t, err)
	key, ok := cursorFindField(entry, cursorFldGrepWorkspaceKey)
	require.True(t, ok, "map entry 必须携带 f1 key")
	valueField, ok := cursorFindField(entry, cursorFldGrepWorkspaceValue)
	require.True(t, ok, "map entry 必须携带 f2 value")

	union, err := cursorDecodeProtobuf(valueField.Bytes)
	require.NoError(t, err)
	contentField, ok := cursorFindField(union, cursorFldGrepUnionContent)
	require.True(t, ok, "GrepUnionResult 必须走 f3 content")
	_, hasCount := cursorFindField(union, cursorFldGrepUnionCount)
	require.False(t, hasCount, "oneof 只能有一个分支")
	_, hasFiles := cursorFindField(union, cursorFldGrepUnionFiles)
	require.False(t, hasFiles, "oneof 只能有一个分支")

	content, err := cursorDecodeProtobuf(contentField.Bytes)
	require.NoError(t, err)
	return cursorNativeFsTestGrepFrame{success: success, workspace: key.String(), content: content}
}

// cursorNativeFsTestGrepFile 是一组 GrepFileMatch 的解码视图。
type cursorNativeFsTestGrepFile struct {
	file    string
	matches [][]cursorPBField
}

// cursorNativeFsTestGrepMatches 返回 GrepContentResult.f1 matches 的分组视图。
func cursorNativeFsTestGrepMatches(t *testing.T, content []cursorPBField) []cursorNativeFsTestGrepFile {
	t.Helper()
	var out []cursorNativeFsTestGrepFile
	for _, f := range content {
		if f.Number != cursorFldGrepContentMatches {
			continue
		}
		fm, err := cursorDecodeProtobuf(f.Bytes)
		require.NoError(t, err)
		fileField, ok := cursorFindField(fm, cursorFldGrepFileMatchFile)
		require.True(t, ok, "GrepFileMatch 必须携带 f1 file")
		group := cursorNativeFsTestGrepFile{file: string(fileField.Bytes)}
		for _, mf := range fm {
			if mf.Number != cursorFldGrepFileMatchMatches {
				continue
			}
			m, merr := cursorDecodeProtobuf(mf.Bytes)
			require.NoError(t, merr)
			group.matches = append(group.matches, m)
		}
		out = append(out, group)
	}
	return out
}

func TestCursorNativeFSWritebackGrep(t *testing.T) {
	record, _, stop := cursorNativeFsTestIntake(t, cursorToolCallCapture{
		ID: "call_g", Name: "grep",
		ArgsJSON: `{"pattern":"TODO","path":"/repo","output_mode":"content"}`,
		ExecID:   99, Kind: cursorToolKindGrep,
	})
	out := "main.go:12:// TODO: fix\nmain.go:34:// TODO: ship\nother.go:7:TODO here"
	require.NoError(t, record.intake.DeliverToolResults(context.Background(), record.id, cursorSessionLevelBatchID,
		[]CursorToolResult{{CallID: "call_g", Text: []string{out}}}))
	res := cursorNativeFsTestDecodeResult(t, cursorNativeFsTestSplit(t, stop())[0])
	require.Equal(t, cursorFldClientMsgGrepResult, res.field, "grep 结果必须走 f5")
	_, hasErr := cursorFindField(res.oneof, cursorFldGrepResultError)
	require.False(t, hasErr, "成功结果绝不可走 f2 error")

	g := cursorNativeFsTestDecodeGrep(t, res.oneof)
	require.Equal(t, "TODO", string(cursorFindFieldBytes(t, g.success, cursorFldGrepSuccessPattern)))
	require.Equal(t, "/repo", string(cursorFindFieldBytes(t, g.success, cursorFldGrepSuccessPath)))
	require.Equal(t, "content", string(cursorFindFieldBytes(t, g.success, cursorFldGrepSuccessOutputMode)))
	require.Equal(t, "/repo", g.workspace, "map key = 委托 path")
	require.EqualValues(t, 3, cursorVarintField(g.content, cursorFldGrepContentTotalLines))
	require.EqualValues(t, 3, cursorVarintField(g.content, cursorFldGrepContentTotalMatchedLines))

	groups := cursorNativeFsTestGrepMatches(t, g.content)
	require.Len(t, groups, 2, "同 file 连续行归一组，换文件另起一组")
	require.Equal(t, "main.go", groups[0].file)
	require.Equal(t, "other.go", groups[1].file)

	m0 := groups[0].matches
	require.Len(t, m0, 2)
	require.EqualValues(t, 12, cursorVarintField(m0[0], cursorFldGrepContentMatchLineNumber))
	require.Equal(t, "// TODO: fix", string(cursorFindFieldBytes(t, m0[0], cursorFldGrepContentMatchContent)))
	require.EqualValues(t, 34, cursorVarintField(m0[1], cursorFldGrepContentMatchLineNumber))
	require.Equal(t, "// TODO: ship", string(cursorFindFieldBytes(t, m0[1], cursorFldGrepContentMatchContent)))
}

func TestCursorNativeFSWritebackGrepError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		result  CursorToolResult
		wantMsg string
	}{
		{"is_error", CursorToolResult{CallID: "call_g", Text: []string{"rg: no such file"}, IsError: true}, "rg: no such file"},
		{"error_prefix", CursorToolResult{CallID: "call_g", Text: []string{"ERROR: bad pattern"}}, "ERROR: bad pattern"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record, _, stop := cursorNativeFsTestIntake(t, cursorToolCallCapture{
				ID: "call_g", Name: "grep", ArgsJSON: `{"pattern":"TODO","path":"/repo"}`,
				ExecID: 102, Kind: cursorToolKindGrep,
			})
			require.NoError(t, record.intake.DeliverToolResults(context.Background(), record.id,
				cursorSessionLevelBatchID, []CursorToolResult{tc.result}))
			res := cursorNativeFsTestDecodeResult(t, cursorNativeFsTestSplit(t, stop())[0])
			errFrame, ok := cursorFindField(res.oneof, cursorFldGrepResultError)
			require.True(t, ok, "失败必须走 f2 error")
			_, hasSuccess := cursorFindField(res.oneof, cursorFldGrepResultSuccess)
			require.False(t, hasSuccess, "绝不可把失败写成 success")
			inner, err := cursorDecodeProtobuf(errFrame.Bytes)
			require.NoError(t, err)
			require.Equal(t, tc.wantMsg, string(cursorFindFieldBytes(t, inner, cursorFldGrepErrorError)))
		})
	}
}

// cursorGrepParseWantMatch 是一条期望命中。
type cursorGrepParseWantMatch struct {
	line    int64
	content string
}

// cursorGrepParseWantGroup 是一组期望的 GrepFileMatch。
type cursorGrepParseWantGroup struct {
	file    string
	matches []cursorGrepParseWantMatch
}

// TestCursorNativeFSGrepParse 覆盖文本 → GrepContentResult 的解析规则。
func TestCursorNativeFSGrepParse(t *testing.T) {
	for _, tc := range []struct {
		name        string
		text        string
		wantGroups  []cursorGrepParseWantGroup
		wantTotal   int
		wantMatched int
	}{
		{
			name: "with_line_numbers",
			text: "a.go:3:hit one\nb.go:9:hit two",
			wantGroups: []cursorGrepParseWantGroup{
				{"a.go", []cursorGrepParseWantMatch{{3, "hit one"}}},
				{"b.go", []cursorGrepParseWantMatch{{9, "hit two"}}},
			},
			wantTotal: 2, wantMatched: 2,
		},
		{
			name: "without_line_numbers",
			text: "a.go:hit one\nb.go:hit two",
			wantGroups: []cursorGrepParseWantGroup{
				{"a.go", []cursorGrepParseWantMatch{{0, "hit one"}}},
				{"b.go", []cursorGrepParseWantMatch{{0, "hit two"}}},
			},
			wantTotal: 2, wantMatched: 2,
		},
		{
			name:       "files_only",
			text:       "a.go\nb.go\nc.go",
			wantGroups: []cursorGrepParseWantGroup{{file: "a.go"}, {file: "b.go"}, {file: "c.go"}},
			wantTotal:  3, wantMatched: 0,
		},
		{
			name: "unparsable_kept_verbatim",
			text: "a.go:1:hit\n---- some banner with spaces ----\nb.go:2:hit",
			wantGroups: []cursorGrepParseWantGroup{
				{"a.go", []cursorGrepParseWantMatch{{1, "hit"}}},
				{"", []cursorGrepParseWantMatch{{0, "---- some banner with spaces ----"}}},
				{"b.go", []cursorGrepParseWantMatch{{2, "hit"}}},
			},
			wantTotal: 3, wantMatched: 3,
		},
		{
			name:       "empty",
			text:       "",
			wantGroups: nil,
			wantTotal:  0, wantMatched: 0,
		},
		{
			name: "multi_file_grouping_and_trailing_newline",
			text: "a.go:1:x\na.go:2:y\nb.go:5:z\na.go:8:w\n",
			wantGroups: []cursorGrepParseWantGroup{
				{"a.go", []cursorGrepParseWantMatch{{1, "x"}, {2, "y"}}},
				{"b.go", []cursorGrepParseWantMatch{{5, "z"}}},
				{"a.go", []cursorGrepParseWantMatch{{8, "w"}}},
			},
			wantTotal: 4, wantMatched: 4,
		},
		{
			name: "content_with_colon_in_payload",
			text: "a.go:3:key: value",
			wantGroups: []cursorGrepParseWantGroup{
				{"a.go", []cursorGrepParseWantMatch{{3, "key: value"}}},
			},
			wantTotal: 1, wantMatched: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content, err := cursorDecodeProtobuf(cursorParseGrepContent(tc.text))
			require.NoError(t, err)
			require.EqualValues(t, tc.wantTotal, cursorVarintField(content, cursorFldGrepContentTotalLines))
			require.EqualValues(t, tc.wantMatched, cursorVarintField(content, cursorFldGrepContentTotalMatchedLines))

			groups := cursorNativeFsTestGrepMatches(t, content)
			require.Len(t, groups, len(tc.wantGroups))
			for i, want := range tc.wantGroups {
				require.Equal(t, want.file, groups[i].file, "第 %d 组文件名", i)
				require.Len(t, groups[i].matches, len(want.matches), "第 %d 组命中数", i)
				for j, wm := range want.matches {
					m := groups[i].matches[j]
					require.EqualValues(t, wm.line, cursorVarintField(m, cursorFldGrepContentMatchLineNumber))
					require.Equal(t, wm.content, string(cursorFindFieldBytes(t, m, cursorFldGrepContentMatchContent)))
				}
			}
		})
	}
}

// TestCursorNativeFSGrepOutputModeDefaults 校验 output_mode 与 map key 的兜底。
func TestCursorNativeFSGrepOutputModeDefaults(t *testing.T) {
	// ArgsJSON 无 output_mode、无 path：mode 填 "content"，key 用 "/"。
	res := cursorEncodeGrepResult(`{"pattern":"TODO"}`, "a.go:1:hit", false)
	oneof, err := cursorDecodeProtobuf(res)
	require.NoError(t, err)
	g := cursorNativeFsTestDecodeGrep(t, oneof)
	require.Equal(t, "/", g.workspace, "path 为空时 map key 用 \"/\"")
	require.Equal(t, "content", string(cursorFindFieldBytes(t, g.success, cursorFldGrepSuccessOutputMode)),
		"output_mode 为空时按约定填 content")
	_, hasPath := cursorFindField(g.success, cursorFldGrepSuccessPath)
	require.False(t, hasPath, "空 path 不写字段（proto3 默认值省略）")

	// 显式 output_mode 原样透传。
	res = cursorEncodeGrepResult(`{"pattern":"TODO","path":"/repo","output_mode":"files_with_matches"}`, "a.go", false)
	oneof, err = cursorDecodeProtobuf(res)
	require.NoError(t, err)
	g = cursorNativeFsTestDecodeGrep(t, oneof)
	require.Equal(t, "files_with_matches",
		string(cursorFindFieldBytes(t, g.success, cursorFldGrepSuccessOutputMode)))

	// 空输出：f1 success 仍存在（不是 f2 error），content 无 matches / totals。
	res = cursorEncodeGrepResult(`{"pattern":"TODO","path":"/repo"}`, "", false)
	oneof, err = cursorDecodeProtobuf(res)
	require.NoError(t, err)
	g = cursorNativeFsTestDecodeGrep(t, oneof)
	require.Empty(t, g.content, "空输出不产生任何 matches / totals")
}

// ---------------------------------------------------------------------------
// 9. 失败关闭：未知 Kind / 无匹配 call_id
// ---------------------------------------------------------------------------

func TestCursorNativeFSWritebackUnknownKindFailsClosed(t *testing.T) {
	_, err := cursorEncodeNativeFsResult(cursorToolCallCapture{Kind: "bogus", ExecID: 1}, CursorToolResult{CallID: "x"})
	require.ErrorIs(t, err, ErrCursorProtocol, "未知 Kind 必须失败关闭")
}

func TestCursorNativeFSWritebackUnknownCallID(t *testing.T) {
	record, _, _ := cursorNativeFsTestIntake(t, cursorToolCallCapture{
		ID: "call_r", Name: "read_file", ArgsJSON: `{"path":"/a"}`, ExecID: 100, Kind: cursorToolKindRead,
	})
	err := record.intake.DeliverToolResults(context.Background(), record.id, cursorSessionLevelBatchID,
		[]CursorToolResult{{CallID: "call_unknown", Text: []string{"x"}}})
	require.ErrorIs(t, err, ErrCursorResumeUnknownAssociation)
}

// ---------------------------------------------------------------------------
// 10. mcp 通道不受影响（Kind=="" 仍走 f11 McpResult，字节级不变）
// ---------------------------------------------------------------------------

func TestCursorNativeFSMcpChannelUnaffected(t *testing.T) {
	record, _, stop := cursorNativeFsTestIntake(t, cursorToolCallCapture{
		ID: "call_mcp", Name: "get_weather", ArgsJSON: `{"city":"x"}`, ExecID: 101,
	})
	require.NoError(t, record.intake.DeliverToolResults(context.Background(), record.id, cursorSessionLevelBatchID,
		[]CursorToolResult{{CallID: "call_mcp", Text: []string{"sunny"}}}))
	wire := stop()
	frames := cursorNativeFsTestSplit(t, wire)
	require.Len(t, frames, 1)
	_, ok := cursorFindField(frames[0], cursorFldClientMsgMcpResult)
	require.True(t, ok, "Kind 为空必须仍走 f11 McpResult")
	_, hasControl := cursorFindField(frames[0], cursorFldClientControlMessage)
	require.False(t, hasControl, "mcp 路径不补 stream_close（既有行为不变）")
	// 字节级：与既有编码器输出逐字节相等（mcp 行为零变化）。
	require.Equal(t, cursorEncodeMcpToolResult(101, nil, "call_mcp", []string{"sunny"}, nil, false),
		splitWritebackEnvelopes(t, wire)[0])
}

// ---------------------------------------------------------------------------
// 10. duplex respond 分发：五个字段号各自命中捕获分支（不落 exec_unsupported_fields）
// ---------------------------------------------------------------------------

func TestCursorNativeFSDuplexRespondDispatch(t *testing.T) {
	readSchema := cursorNativeFsTestReadSchema
	lsSchema := cursorNativeFsTestLsSchema
	grepSchema := cursorNativeFsTestGrepSchema
	writeSchema := cursorNativeFsTestWriteSchema
	delSchema := cursorNativeFsTestDelSchema

	cases := []struct {
		name      string
		field     int
		defs      [][]byte
		kind      string
		toolName  string
		sub       []byte
		wantInArg string
	}{
		{
			name: "read_f7", field: cursorFldServerMsgReadArgs,
			defs: [][]byte{cursorTestBuildToolDef(t, "read_file", readSchema)},
			kind: cursorToolKindRead, toolName: "read_file",
			sub: func() []byte {
				b := cursorAppendStringField(nil, 1, "/repo/a.go")
				return cursorAppendStringField(b, 2, "call_r")
			}(),
			wantInArg: "/repo/a.go",
		},
		{
			name: "ls_f8", field: cursorFldServerMsgLsArgs,
			defs: [][]byte{cursorTestBuildToolDef(t, "ls", lsSchema)},
			kind: cursorToolKindLs, toolName: "ls",
			sub: func() []byte {
				b := cursorAppendStringField(nil, 1, "/repo")
				return cursorAppendStringField(b, 3, "call_l")
			}(),
			wantInArg: "/repo",
		},
		{
			name: "grep_f5", field: cursorFldServerMsgGrepArgs,
			defs: [][]byte{cursorTestBuildToolDef(t, "grep", grepSchema)},
			kind: cursorToolKindGrep, toolName: "grep",
			sub: func() []byte {
				b := cursorAppendStringField(nil, 1, "TODO")
				return cursorAppendStringField(b, 14, "call_g")
			}(),
			wantInArg: "TODO",
		},
		{
			name: "write_f3", field: cursorFldServerMsgWriteArgs,
			defs: [][]byte{cursorTestBuildToolDef(t, "write_file", writeSchema)},
			kind: cursorToolKindWrite, toolName: "write_file",
			sub: func() []byte {
				b := cursorAppendStringField(nil, 1, "/repo/a.txt")
				b = cursorAppendStringField(b, 2, "hi")
				return cursorAppendStringField(b, 3, "call_w")
			}(),
			wantInArg: "/repo/a.txt",
		},
		{
			name: "delete_f4", field: cursorFldServerMsgDeleteArgs,
			defs: [][]byte{cursorTestBuildToolDef(t, "delete_file", delSchema)},
			kind: cursorToolKindDelete, toolName: "delete_file",
			sub: func() []byte {
				b := cursorAppendStringField(nil, 1, "/repo/old.txt")
				return cursorAppendStringField(b, 2, "call_d")
			}(),
			wantInArg: "/repo/old.txt",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &cursorAgentDuplex{delegations: newLocalCursorToolDelegations(), declaredToolDefs: c.defs, lg: zap.NewNop()}
			execMsg := cursorAppendVarintField(nil, 1, 55)
			execMsg = cursorAppendBytesField(execMsg, 15, []byte("exec-55"))
			execMsg = cursorAppendBytesField(execMsg, c.field, c.sub)
			payload := cursorAppendBytesField(nil, cursorFldServerExecMessage, execMsg)

			done, err := d.respond(payload)
			require.NoError(t, err, "字段 f%d 必须被捕获分支处理，不得落 default 失败关闭", c.field)
			require.False(t, done)
			snap := d.delegations.snapshot()
			require.Len(t, snap, 1, "必须登记一条委托")
			require.Equal(t, c.kind, snap[0].Kind)
			require.Equal(t, c.toolName, snap[0].Name)
			require.EqualValues(t, 55, snap[0].ExecID)
			require.Equal(t, []byte("exec-55"), snap[0].IDField)
			require.Contains(t, snap[0].ArgsJSON, c.wantInArg)
		})
	}

	// 五种之外的 exec 子字段仍走 default 失败关闭（取证日志 + ErrCursorUnsupportedRequest）。
	d := &cursorAgentDuplex{delegations: newLocalCursorToolDelegations(), lg: zap.NewNop()}
	unknown := cursorAppendVarintField(nil, 1, 56)
	unknown = cursorAppendBytesField(unknown, 99, []byte("x"))
	_, err := d.respond(cursorAppendBytesField(nil, cursorFldServerExecMessage, unknown))
	require.ErrorIs(t, err, ErrCursorUnsupportedRequest, "未实现字段仍必须失败关闭")
}

// cursorFindFieldBytes 取字段字节（测试辅助，缺失即失败）。
func cursorFindFieldBytes(t *testing.T, fields []cursorPBField, number int) []byte {
	t.Helper()
	f, ok := cursorFindField(fields, number)
	require.True(t, ok, "必须存在字段 %d", number)
	return f.Bytes
}
