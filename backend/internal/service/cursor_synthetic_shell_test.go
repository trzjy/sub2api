package service

// 本组用例锁死「本站合成的降级壳不参与必填校验」的口径。
//
// 背景（11-03 活体）：Responses 侧的 custom 工具（Codex 的 apply_patch）与 tool_search
// 在转成 chat 协议时无法表达原始形态，被 apicompat 降级成普通 function 工具，参数
// schema 是本端自己编的壳（{"input":...} / {"query":...}，壳里带 required）。上游模型
// 看不到原始协议，实际只给自由文本或干脆发空对象 → 旧实现按壳里的 required 判
// missing_required → 一批正常 custom 调用被判"上游参数残缺" → 整条会话 502 终结。
//
// 修法：按 tool_name 取回 schema 后，若该 schema 归一化后等于本站合成的壳，就不消费
// required（其余形状投影照旧）；客户端真实声明的 schema 一律照旧严格校验。

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

// 合成壳识别：custom 壳与 tool_search 壳都必须被认出。
func TestCursorIsSyntheticDowngradeShell(t *testing.T) {
	for name, raw := range map[string]string{
		"custom":      apicompat.CustomToolInputSchema,
		"tool_search": apicompat.ToolSearchProxySchema,
	} {
		t.Run(name, func(t *testing.T) {
			normalized := cursorNormalizeIndexedSchema("", []byte(raw))
			require.NotEmpty(t, normalized, "壳必须可归一化")
			require.True(t, cursorIsSyntheticDowngradeShell(normalized),
				"本站合成的降级壳必须被识别")
		})
	}
}

// 客户端真实声明的 schema 绝不能被误认成壳（否则真缺必填就漏拦了）。
func TestCursorIsSyntheticDowngradeShellRejectsRealSchemas(t *testing.T) {
	for name, raw := range map[string]string{
		"bash":               `{"type":"object","properties":{"command":{"type":"string"},"description":{"type":"string"}},"required":["command"]}`,
		"input_not_matching": `{"type":"object","properties":{"input":{"type":"string"}},"required":["input"],"additionalProperties":false}`,
		"query_only":         `{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`,
		"empty":              `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			normalized := cursorNormalizeIndexedSchema("", []byte(raw))
			require.False(t, cursorIsSyntheticDowngradeShell(normalized),
				"真实 schema 不得被误判成合成壳")
		})
	}
}

// custom 工具给空参数（上游最常见形态）必须被放行——这正是 11-03 误判现场。
func TestRetypeArgsCustomShellAllowsEmptyArgs(t *testing.T) {
	def := cursorTestBuildToolDef(t, "apply_patch", apicompat.CustomToolInputSchema)
	out, err := cursorRetypeArgsByToolDefs("call_1", "apply_patch", `{}`, [][]byte{def}, nil)
	require.NoError(t, err, "custom 降级壳不得因 required 判上游残缺")
	require.JSONEq(t, `{}`, out)
}

// custom 工具给自由文本（上游另一种常见形态）也必须放行，且原样保留。
func TestRetypeArgsCustomShellKeepsFreeformArgs(t *testing.T) {
	def := cursorTestBuildToolDef(t, "apply_patch", apicompat.CustomToolInputSchema)
	out, err := cursorRetypeArgsByToolDefs("call_1", "apply_patch",
		`{"input":"*** Begin Patch\n*** End Patch"}`, [][]byte{def}, nil)
	require.NoError(t, err)
	require.Contains(t, out, "*** Begin Patch")
}

// tool_search 降级壳给空参数同样放行。
func TestRetypeArgsToolSearchShellAllowsEmptyArgs(t *testing.T) {
	def := cursorTestBuildToolDef(t, apicompat.ToolSearchProxyName, apicompat.ToolSearchProxySchema)
	out, err := cursorRetypeArgsByToolDefs("call_1", apicompat.ToolSearchProxyName, `{}`, [][]byte{def}, nil)
	require.NoError(t, err)
	require.JSONEq(t, `{}`, out)
}

// 回归护栏：普通 function 工具真缺必填仍必须拒绝（修复不得扩大成"一律不校验"）。
func TestRetypeArgsRealToolStillRejectsMissingRequired(t *testing.T) {
	def := cursorTestBuildToolDef(t, "Bash",
		`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"],"additionalProperties":false}`)
	var reason cursorRetypeErrReason
	_, err := cursorRetypeArgsByToolDefs("call_1", "Bash", `{}`, [][]byte{def},
		func(r cursorRetypeErrReason) { reason = r })
	require.Error(t, err, "真实工具缺必填必须仍然拒绝")
	require.Equal(t, cursorRetypeMissingRequired, reason)
	require.False(t, cursorRetypeDegradable(reason), "缺必填仍不可降级交付")
}

// 形状投影在合成壳路径上仍然生效：值形状照旧还原（只是不查 required）。
func TestRetypeArgsCustomShellStillProjectsShape(t *testing.T) {
	def := cursorTestBuildToolDef(t, "apply_patch", apicompat.CustomToolInputSchema)
	out, err := cursorRetypeArgsByToolDefs("call_1", "apply_patch", `{"input":"x","extra":1}`, [][]byte{def}, nil)
	require.NoError(t, err)
	require.True(t, strings.Contains(out, `"input":"x"`))
}
