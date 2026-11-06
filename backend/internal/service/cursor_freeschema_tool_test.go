//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// cursorTestBuildFreeformToolDef 构造一条**没有 input_schema** 的工具定义：
// custom / freeform 工具（codex 的 exec）在生产里就是这样——参数是一段自由文本，
// 天然没有 parameters，因此不会有 f3/f6。
func cursorTestBuildFreeformToolDef(toolName string) []byte {
	var def []byte
	def = cursorAppendStringField(def, cursorFldMcpToolDefName, toolName)
	def = cursorAppendStringField(def, cursorFldMcpToolDefDescription, "desc-"+toolName)
	def = cursorAppendStringField(def, cursorFldMcpToolDefProvider, cursorMcpProviderIdentifier)
	def = cursorAppendStringField(def, cursorFldMcpToolDefToolName, toolName)
	return def
}

// 回归（2026-11-06 线上 CUR-EXEC-10）：custom/freeform 工具没有参数 schema 是**正常形态**，
// 参数必须原样交付。修复前这里走 schema_absent → 被判"参数残缺"→ 整轮终止，
// 表现为 codex 每次 exec 调用都失败。
func TestCursorRetypeDeclaredToolWithoutSchemaPassesThrough(t *testing.T) {
	defs := [][]byte{
		cursorTestBuildFreeformToolDef("functions__exec"),
		cursorTestBuildToolDef(t, "functions__wait", `{"type":"object","properties":{}}`),
	}
	const args = `const r = await tools.exec_command({cmd: "echo hi"}); text(JSON.stringify(r));`

	out, err := cursorRetypeArgsByToolDefs("call_1", "functions__exec", args, defs, nil)
	require.NoError(t, err, "无 schema 的自由文本工具必须原样交付，不得按参数残缺拒绝")
	require.Equal(t, args, out, "参数必须逐字保留（自由文本不能被改写）")
}

// 边界：真正没被声明的工具名仍然失败关闭（不能被上面那条放宽成"什么都放过"）。
func TestCursorRetypeUndeclaredToolStillFailsClosed(t *testing.T) {
	defs := [][]byte{cursorTestBuildFreeformToolDef("functions__exec")}

	_, err := cursorRetypeArgsByToolDefs("call_1", "shell", `{"command":"ls"}`, defs, nil)
	require.ErrorIs(t, err, ErrCursorProtocol)
}
