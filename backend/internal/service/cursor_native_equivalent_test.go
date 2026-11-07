//go:build unit

package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCursorNativeEquivalentCoversBuiltinNames 锁定「等价表必须含内置名自身」。
//
// 背景（2026-11-07 生产实测）：客户端（Claude Code）声明的工具就叫 `grep`、
// `read`、`write`、`edit`、`glob`。社区的 BUILTIN_EQUIVALENT 只列了 search/ripgrep
// （它假设客户端不叫 grep）→ 客户端明明声明了 grep，我们却**点不出它的名** →
// 拒绝文本退回「列出全部工具名」（实测 34 个）→ 模型还是挑不出来 → 继续瞎试原生工具。
//
// 规则：每个有内置名自身的字段，等价表必须含该内置名，否则点名会失效。
func TestCursorNativeEquivalentCoversBuiltinNames(t *testing.T) {
	cases := []struct {
		field  int
		builtin string
	}{
		{cursorFldNativeRejectShellArgs, "shell"},
		{cursorFldNativeRejectWriteArgs, "write"},
		{cursorFldNativeRejectGrepArgs, "grep"},
		{cursorFldNativeRejectReadArgs, "read"},
		{cursorFldNativeRejectLsArgs, "ls"},
		{cursorFldNativeRejectShellStreamArgs, "shell"},
	}
	for _, tc := range cases {
		equiv, ok := cursorNativeEquivalent[tc.field]
		require.True(t, ok, "字段 %d 必须在等价表里", tc.field)
		require.Contains(t, equiv, tc.builtin,
			"字段 %d 的等价表必须含内置名 %q 自身，否则客户端声明它时点不出名", tc.field, tc.builtin)
	}
}

// TestCursorNativePointNameUsesClientOriginalName 锁定点名用的是**客户端原名**。
func TestCursorNativePointNameUsesClientOriginalName(t *testing.T) {
	// 真实客户端声明集（生产实测）。
	declared := []string{"question", "bash", "read", "glob", "grep", "edit", "write"}
	cases := map[int]string{
		cursorFldNativeRejectShellArgs:       "bash",
		cursorFldNativeRejectWriteArgs:       "edit",
		cursorFldNativeRejectGrepArgs:        "grep",
		cursorFldNativeRejectReadArgs:        "read",
		cursorFldNativeRejectLsArgs:          "glob",
		cursorFldNativeRejectShellStreamArgs: "bash",
	}
	for field, want := range cases {
		txt := cursorNativeExecRejectReason(field, declared)
		require.True(t, strings.Contains(txt, "Call the tool '"+want+"' instead."),
			"字段 %d 必须点名客户端原名 %q，实际文本: %s", field, want, txt)
	}
}
