//go:build unit

package service

// NSFS-01 只读观测验收：Cursor 原生文件工具委托"客户端未声明工具"路径（cursorRejectNativeFsNoTool）
// 必须把本请求 f4 声明集解析出的全部工具名随拒绝帧一并记录（declared_tool_names /
// declared_tool_name_count）。本卡只观测、不改匹配规则、不改失败帧 / 封口 / 返回值。
//
// 覆盖：
//   ① TestCursorNativeFsRejectLogsDeclaredNames：构造声明集 ["Bash","Read","Grep"]，触发工具缺席
//      路径，断言拒绝日志字段含全部三个名字（用 zap observer 捕获）。
//   ② TestCursorDeclaredToolNamesParsing：直接测 declaredToolNames() helper——3 条声明返回
//      ["Bash","Read","Grep"]、空声明集返回空切片、重复名去重、顺序保持。

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

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

// cursorNativeFsObserveNamesFromContext 从一条观测日志里取出 declared_tool_names 字段。
// 生产侧以逗号拼接的 string 写入（工具名均为不含逗号的标识符），测试侧按逗号拆回。
func cursorNativeFsObserveNamesFromContext(t *testing.T, e observer.LoggedEntry) []string {
	t.Helper()
	raw, ok := e.ContextMap()["declared_tool_names"].(string)
	require.True(t, ok, "declared_tool_names 必须是字符串字段")
	if raw == "" {
		return []string{}
	}
	return strings.Split(raw, ",")
}

// TestCursorNativeFsRejectLogsDeclaredNames：触发"工具缺席"路径并断言拒绝日志带齐声明工具名。
//
// 注意：声明集含 "Read" 与 "Grep"——若用 read / grep 委托，这两个名字会命中各自候选名单
// （read: Read；grep: Grep）但因 schema 缺关键属性而落到 ErrCursorProtocol（非缺席哨兵），
// 不会进入 cursorRejectNativeFsNoTool。故这里用 write 委托：write 候选
// （write_file/Write/write）在声明集中全无，必然走 ErrCursorNativeFsToolAbsent → 拒绝处理器，
// 从而观测到声明集里含 "Read" 的真实声明名。断言目标仍为卡片指定的三名字 ["Bash","Read","Grep"]。
func TestCursorNativeFsRejectLogsDeclaredNames(t *testing.T) {
	defs := [][]byte{
		cursorTestBuildToolDef(t, "Bash", `{"type":"object","properties":{"command":{"type":"string"}}}`),
		cursorTestBuildToolDef(t, "Read", `{"type":"object","properties":{"path":{"type":"string"}}}`),
		cursorTestBuildToolDef(t, "Grep", `{"type":"object","properties":{"pattern":{"type":"string"}}}`),
	}
	d, logs, stop := cursorNativeFsTestObserveDuplex(t, defs)

	// write 委托：path / file_text / tool_call_id 齐，但声明集无 write 类候选 → 工具缺席。
	sub := cursorAppendStringField(nil, 1, "/repo/a.txt")
	sub = cursorAppendStringField(sub, 2, "hello-content")
	sub = cursorAppendStringField(sub, 3, "call_w_1")
	parts := cursorNativeFsTestExecParts(t, 91, []byte("exec-91"), cursorFldServerMsgWriteArgs, sub)

	tc, err := d.cursorCaptureNativeFsArgs(parts, cursorFldServerMsgWriteArgs, 91, []byte("exec-91"))
	require.NoError(t, err, "工具缺席必须回失败帧而非整请求报错")
	require.Equal(t, cursorToolCallCapture{}, tc, "工具缺席返回空 capture")
	_ = stop() // drain 回写字节（fail frame + stream_close），避免阻塞写 goroutine 泄漏

	entries := logs.FilterField(zap.String("phase", "native_fs_reject_declared_tool_names")).All()
	require.Len(t, entries, 1, "必须记一条携带 declared_tool_names 的拒绝观测日志")

	names := cursorNativeFsObserveNamesFromContext(t, entries[0])
	require.ElementsMatch(t, []string{"Bash", "Read", "Grep"},
		names, "拒绝观测日志必须含声明集全部三个工具名")

	// 计数从 Context 取（zap.Int 的值在 f.Integer，ContextMap 不回显）。
	var count int
	for _, f := range entries[0].Context {
		if f.Key == "declared_tool_name_count" {
			count = int(f.Integer)
		}
	}
	require.Equal(t, 3, count, "工具名计数必须等于声明集大小")
}

// TestCursorDeclaredToolNamesParsing：直接测 declaredToolNames() helper 的解析 / 去重 / 顺序 / 空集。
func TestCursorDeclaredToolNamesParsing(t *testing.T) {
	defs := [][]byte{
		cursorTestBuildToolDef(t, "Bash", `{"type":"object","properties":{"command":{"type":"string"}}}`),
		cursorTestBuildToolDef(t, "Read", `{"type":"object","properties":{"path":{"type":"string"}}}`),
		cursorTestBuildToolDef(t, "Grep", `{"type":"object","properties":{"pattern":{"type":"string"}}}`),
	}
	d := &cursorAgentDuplex{declaredToolDefs: defs}
	require.Equal(t, []string{"Bash", "Read", "Grep"}, d.declaredToolNames(),
		"必须按声明顺序返回全部工具名（f5 tool_name 回落 f1 name）")

	// 空声明集返回空切片。
	empty := &cursorAgentDuplex{declaredToolDefs: [][]byte{}}
	require.Empty(t, empty.declaredToolNames(), "空声明集必须返回空切片")

	// 重复名去重，且保持首次出现顺序；数量上限不被破坏。
	dup := &cursorAgentDuplex{declaredToolDefs: [][]byte{
		cursorTestBuildToolDef(t, "Bash", `{"type":"object","properties":{"command":{"type":"string"}}}`),
		cursorTestBuildToolDef(t, "Bash", `{"type":"object","properties":{"command":{"type":"string"}}}`),
		cursorTestBuildToolDef(t, "Read", `{"type":"object","properties":{"path":{"type":"string"}}}`),
	}}
	require.Equal(t, []string{"Bash", "Read"}, dup.declaredToolNames(),
		"重复名必须去重，且保持首次出现顺序")
}
