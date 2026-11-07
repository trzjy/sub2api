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

// TestCursorNativePointNameUsesWireName 锁定：点名用的是**出站 f4 里的 wire name**（模型认识），
// 不是客户端原名。撞内置名时原名 Read → wire name Read_，点名必须写 Read_ 而非 Read，否则模型
// 工具表里只有 Read_ 找不到 Read，只能继续瞎试原生工具（生产实测的连环拒绝根因）。
func TestCursorNativeEquivalentPointNameUsesWireName(t *testing.T) {
	// declared（客户端原名）与 wire（wire name）逐个对应：Read→Read_、Write→Write_、
	// Grep→Grep_、Bash 不撞名→bash、Glob→Glob_。
	declared := []string{"Read", "Write", "Grep", "Bash", "Glob"}
	wire := []string{"Read_", "Write_", "Grep_", "bash", "Glob_"}
	cases := map[int]string{
		cursorFldNativeRejectShellArgs:       "bash",
		cursorFldNativeRejectWriteArgs:       "Write_",
		cursorFldNativeRejectGrepArgs:        "Grep_",
		cursorFldNativeRejectReadArgs:        "Read_",
		cursorFldNativeRejectLsArgs:          "Glob_",
		cursorFldNativeRejectShellStreamArgs: "bash",
	}
	for field, want := range cases {
		txt, redirected, hit := cursorNativeExecRejectReasonInfo(field, declared, wire)
		require.True(t, hit, "字段 %d 必须命中等价表", field)
		require.Equal(t, want, redirected, "字段 %d 点名必须是 wire name %q", field, want)
		require.Contains(t, txt, "Call the tool '"+want+"' instead.",
			"字段 %d 文本必须点名 wire name %q，实际: %s", field, want, txt)
	}
}

// TestCursorNativePointNameReadCollisionUsesWireName：派发单 Done when 1 的精确形式——
// 客户端声明 Read（撞内置名，wire name = Read_），断言拒绝文本出现 Read_、且**不出现**
// 'Read' instead（带引号闭合，避免 Read_ 的子串 Read 误判）。
func TestCursorNativeEquivalentPointNameReadCollisionUsesWireName(t *testing.T) {
	declared := []string{"Read"}
	wire := []string{"Read_"}
	txt, redirected, hit := cursorNativeExecRejectReasonInfo(cursorFldNativeRejectReadArgs, declared, wire)
	require.True(t, hit)
	require.Equal(t, "Read_", redirected)
	require.Contains(t, txt, "Call the tool 'Read_' instead.")
	require.False(t, strings.Contains(txt, "'Read' instead"),
		"拒绝文本不得点名原名 Read（模型工具表只有 Read_），实际: %s", txt)
}

// TestCursorNativeRejectFallbackListUsesWireName：派发单 Done when 2——等价表没命中时，
// 退回列清单，清单里的名字必须用 wire name（模型认识的），不是客户端原名。
func TestCursorNativeEquivalentFallbackListUsesWireName(t *testing.T) {
	// f9 diagnostics 不在等价表中 → 必走列清单分支。客户端声明 Read（原名）→ wire name Read_。
	declared := []string{"Read"}
	wire := []string{"Read_"}
	txt := cursorNativeExecRejectReason(cursorFldNativeRejectDiagnosticsArgs, declared, wire)
	require.Contains(t, txt, "Use only the client-provided tools: Read_.",
		"列清单必须用 wire name Read_")
	require.False(t, strings.Contains(txt, "Use only the client-provided tools: Read."),
		"列清单不得用原名 Read")
}
