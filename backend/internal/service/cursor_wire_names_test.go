//go:build unit

package service

// 工具名重名隔离的等价物（wire name）单测，覆盖派发单 W6 的 6 类用例。
//
// 本卡只做「出站声明改名」与「出站历史改名」两个方向；wire name 的**规则**必须与
// 另一张卡（落在 cursor_agent_duplex.go 的入站映射）逐字一致，否则回程映射对不齐。
//
// **W3 选择 (b)：排序后去重**。wire name 只依赖「工具名集合」、与声明顺序无关——
// 排序键是工具名原始字符串的字节序升序（Go 默认 < 比较）。下单卡必须用同一排序键。

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

// toolNamed 造一个带 Function.Name 的 ChatTool，便于喂给 cursorWireToolNameSet。
func toolNamed(name string) apicompat.ChatTool {
	return apicompat.ChatTool{
		Type:     "function",
		Function: &apicompat.ChatFunction{Name: name},
	}
}

// 用例 1：撞内置名 → 原名后加 "_"。
func TestCursorWireToolNameBuiltinCollision(t *testing.T) {
	cases := map[string]string{
		"Read":     "Read_",
		"Write":    "Write_",
		"Glob":     "Glob_",
		"Grep":     "Grep_",
		"WebFetch": "WebFetch_",
		"shell":    "shell_",
		"ls":       "ls_",
		"delete":   "delete_",
		"read":     "read_",
		"web_search": "web_search_",
	}
	for name, want := range cases {
		require.Equal(t, want, cursorWireToolName(name),
			"撞内置名必须加下划线避开：%q", name)
	}
}

// 用例 2：归一化后仍撞（被本批其它工具的 wire name 占住）→ 继续加 "_"，直到不撞。
func TestCursorWireToolNameSecondaryCollision(t *testing.T) {
	// "Read" 先占住 wire name "Read_"（归一化 read_）；
	// "Read_"（原名）的纯函数候选是 "Read_"（已不撞内置），但被 "Read" 的 wire 占住，
	// 于是继续加 "_" → "Read__"。证明循环会一直加直到脱离冲突，不会卡死或复算成同名。
	out := cursorWireToolNamesFromNames([]string{"Read", "Read_"})
	require.Equal(t, "Read_", out["Read"], "Read 的 wire name")
	require.Equal(t, "Read__", out["Read_"], "Read_ 与 Read 的 wire 冲突，必须再追加下划线")
	require.NotEqual(t, out["Read"], out["Read_"], "二次撞名后必须错开")

	// 三层叠加：Read / Read_ / read__ 三者归一化都落到 read_ 体系，必须各自错开。
	out3 := cursorWireToolNamesFromNames([]string{"Read", "Read_", "read__"})
	seen := map[string]bool{}
	for _, w := range out3 {
		require.False(t, seen[cursorNormaliseToolName(w)], "归一化后的 wire name 不允许重复：%q", w)
		seen[cursorNormaliseToolName(w)] = true
	}
	require.Equal(t, "Read_", out3["Read"])
	require.Equal(t, "Read__", out3["Read_"])
	require.Equal(t, "read___", out3["read__"])
}

// 用例 3：同一批工具、声明顺序不同 → wire name 集合相同（选择 (b) 的核心不变量）。
func TestCursorWireToolNameOrderIndependent(t *testing.T) {
	names := []string{"Read", "Write", "Glob", "WebFetch", "get_weather"}
	// 三种不同声明顺序。
	a := cursorWireToolNamesFromNames(append([]string(nil), names...))
	rev := make([]string, len(names))
	for i, n := range names {
		rev[len(names)-1-i] = n
	}
	b := cursorWireToolNamesFromNames(rev)
	shuf := []string{"WebFetch", "get_weather", "Read", "Glob", "Write"}
	c := cursorWireToolNamesFromNames(shuf)
	require.Equal(t, a, b, "逆序声明必须得到同一份 wire name 映射")
	require.Equal(t, a, c, "乱序声明必须得到同一份 wire name 映射")
	// 不撞内置名的工具必须保持原名。
	require.Equal(t, "get_weather", a["get_weather"])
	// 撞内置名的工具都加下划线。
	require.Equal(t, "Read_", a["Read"])
	require.Equal(t, "Write_", a["Write"])
	require.Equal(t, "Glob_", a["Glob"])
	require.Equal(t, "WebFetch_", a["WebFetch"])
}

// 用例 4：空工具集不 panic，返回空 map。
func TestCursorWireToolNameEmpty(t *testing.T) {
	require.NotPanics(t, func() {
		out := cursorWireToolNameSet(nil)
		require.Empty(t, out)
	})
	require.NotPanics(t, func() {
		out := cursorWireToolNamesFromNames([]string{})
		require.Empty(t, out)
	})
	// 没有工具的 ChatTool 列表也要安全返回空。
	require.Empty(t, cursorWireToolNameSet([]apicompat.ChatTool{}))
}

// 用例 5：不撞内置名 → wire name 严格等于原名（绝大多数工具走这条，行为不变）。
func TestCursorWireToolNameNoCollision(t *testing.T) {
	cases := []string{
		"get_weather", "noop", "a", "b", "my_tool", "fetchUser", "list_files",
	}
	for _, name := range cases {
		require.Equal(t, name, cursorWireToolName(name),
			"不撞内置名必须保持原名：%q", name)
	}
	// 集合形式也要一致。
	out := cursorWireToolNamesFromNames(cases)
	for _, name := range cases {
		require.Equal(t, name, out[name])
	}
}

// 用例 6：与本批其它工具不重复 —— 两个不同原名不能算出同一个 wire name。
func TestCursorWireToolNameNoDuplicateInBatch(t *testing.T) {
	// 两个原名若纯函数会落到同一归一化 wire name（Read 与 Read_），必须错开。
	out := cursorWireToolNamesFromNames([]string{"Read", "Read_"})
	require.NotEqual(t, out["Read"], out["Read_"], "不同原名不得算出同一 wire name")

	// 一般多工具集合：所有 wire name 的归一化形态必须两两不同。
	names := []string{"Read", "Write", "Glob", "Grep", "Bash", "WebFetch",
		"get_weather", "Read_", "write", "GLOB"}
	out = cursorWireToolNamesFromNames(names)
	normSeen := map[string]string{}
	for orig, w := range out {
		n := cursorNormaliseToolName(w)
		other, dup := normSeen[n]
		require.False(t, dup, "wire name 归一化冲突：%q 与 %q 都映射到 %q", orig, other, n)
		normSeen[n] = orig
	}
	require.Len(t, out, len(names), "每个原名都应有一个 wire name")
}

// 归一化函子本身的等价性（与社区 _normalise 语义对齐）：camelCase→snake_case、'-'→'_'、小写。
func TestCursorNormaliseToolName(t *testing.T) {
	cases := map[string]string{
		"Read":         "read",
		"WebFetch":     "web_fetch",
		"ReadFile":     "read_file",
		"web-search":   "web_search",
		"Glob":         "glob",
		"my_tool":      "my_tool",
		"Already_Snake": "already_snake",
	}
	for in, want := range cases {
		require.Equal(t, want, cursorNormaliseToolName(in), "归一化：%q", in)
	}
}

// cursorWireToolNameSet 必须从 ChatTool 列表正确过滤出 Function.Name。
func TestCursorWireToolNameSetFromChatTools(t *testing.T) {
	tools := []apicompat.ChatTool{
		toolNamed("Read"),
		toolNamed("Write"),
		{Type: "function"}, // Function 为 nil，应被跳过
		{Type: "function", Function: &apicompat.ChatFunction{Name: ""}}, // 空名跳过
	}
	out := cursorWireToolNameSet(tools)
	require.Equal(t, "Read_", out["Read"])
	require.Equal(t, "Write_", out["Write"])
	require.Len(t, out, 2, "只应有 Read / Write 两个有效 wire name")
}

// cursorWireToolNameOf：history 重放统一入口——有 wire map 取 map，缺失则回落纯函数。
func TestCursorWireToolNameOf(t *testing.T) {
	m := cursorWireToolNamesFromNames([]string{"Read"})
	require.Equal(t, "Read_", cursorWireToolNameOf(m, "Read"))
	// 不在本批（极端：声明时该工具已无）回落纯函数，仍给出确定结果。
	require.Equal(t, "Write_", cursorWireToolNameOf(m, "Write"))
	// nil map 也不 panic，直接走纯函数。
	require.Equal(t, "Read_", cursorWireToolNameOf(nil, "Read"))
}
