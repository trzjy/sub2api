package service

// Cursor 原生 exec 字段号常量与字段识别。
//
// 意外 native / 未知 exec 一律由 CUR-SVC2 改为协议错误；本文件只保留统一字段表
// cursor_field_table.go 直接引用的 11 个外层字段号常量，以及按 cursorExecNativeRejectOrder
// 识别首个命中字段的函数。不再编造拒绝帧、等价表或任何拒绝文本。

// ---------------------------------------------------------------------------
// 字段号常量（全部以 cursorFldNativeReject 开头，取自方案「拒绝形状」表）
// ---------------------------------------------------------------------------

// 外层 ExecServerMessage 原生 exec 字段号 = ExecClientMessage 结果字段号（resultField == field）。
const (
	cursorFldNativeRejectShellArgs                = 2  // shell_args            → ShellResult
	cursorFldNativeRejectWriteArgs                = 3  // write_args            → WriteResult
	cursorFldNativeRejectDeleteArgs               = 4  // delete_args           → DeleteResult
	cursorFldNativeRejectGrepArgs                 = 5  // grep_args             → GrepResult
	cursorFldNativeRejectReadArgs                 = 7  // read_args             → ReadResult
	cursorFldNativeRejectLsArgs                   = 8  // ls_args               → LsResult
	cursorFldNativeRejectDiagnosticsArgs          = 9  // diagnostics_args      → DiagnosticsResult
	cursorFldNativeRejectShellStreamArgs          = 14 // shell_stream_args     → ShellStream
	cursorFldNativeRejectBackgroundShellSpawnArgs = 16 // background_shell_spawn_args → BackgroundShellSpawnResult
	cursorFldNativeRejectFetchArgs                = 20 // fetch_args            → FetchResult
	cursorFldNativeRejectWriteShellStdinArgs      = 23 // write_shell_stdin_args→ WriteShellStdinResult
)

// cursorNativeExecRejectField 按方案优先级顺序（2,3,4,5,7,8,9,14,16,20,23）返回第一个出现在
// parts 中的 ExecServerMessage 原生 exec 字段号。一个都不在 → (0, false)。
//
// 优先级顺序的唯一来源是统一字段表的 cursorExecNativeRejectOrder（CARD-01），
// 本函数不再维护第二份清单：既有清单与字段表曾各写一份，改一处漏一处就会让
// 「拒绝哪种形状」与「表里登记了什么」对不上。
func cursorNativeExecRejectField(parts []cursorPBField) (int, bool) {
	for _, n := range cursorExecNativeRejectOrder {
		if _, ok := cursorFindField(parts, n); ok {
			return n, true
		}
	}
	return 0, false
}
