package service

// Cursor 原生 exec 统一拒绝（R1 增量实现）。
//
// 本文件实现社区 pi-cursor-provider（offbynan/proxy.ts@63f2f29e）做法：Cursor 上游注入的
// 原生工具 exec（shell / 文件 / grep / fetch / shell_stdin 等）一律回协议规定的拒绝 / 错误形状，
// 理由文本指引模型改用客户端声明的工具（mcp_args，f11）。本片只负责"把拒绝形状编出来并写出"，
// 不接入派发 switch、不删除任何旧代码（下一片 R2 做）。
//
// 所有字段号均自带常量（cursorFldNativeReject*），严格取自方案「拒绝形状」表，绝不复用 / 改动
// 既有常量（cursorFldClientMsg* / cursorFldServerMsg* / cursorFldShellStream* 等）。

import (
	"strings"
	"unicode"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

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

// 结果消息（ExecClientMessage 各 oneof）内部承载 rejected / error 的子字段号。
const (
	cursorFldNativeRejectShellResultRejected          = 4 // ShellResult.f4 rejected
	cursorFldNativeRejectShellStreamRejected          = 5 // ShellStream.f5 rejected
	cursorFldNativeRejectBackgroundShellSpawnRejected = 3 // BackgroundShellSpawnResult.f3 rejected
	cursorFldNativeRejectWriteResultRejected          = 6 // WriteResult.f6 rejected
	cursorFldNativeRejectDeleteResultRejected         = 6 // DeleteResult.f6 rejected
	cursorFldNativeRejectReadResultRejected           = 3 // ReadResult.f3 rejected
	cursorFldNativeRejectLsResultRejected             = 3 // LsResult.f3 rejected
	cursorFldNativeRejectDiagnosticsResultRejected    = 3 // DiagnosticsResult.f3 rejected
	cursorFldNativeRejectGrepResultError              = 2 // GrepResult.f2 error
	cursorFldNativeRejectFetchResultError             = 2 // FetchResult.f2 error
	cursorFldNativeRejectWriteShellStdinResultError   = 2 // WriteShellStdinResult.f2 error
)

// 各 rejected / error 内层子字段号。
const (
	cursorFldNativeRejectRejectedCommand             = 1 // ShellRejected.f1 command
	cursorFldNativeRejectRejectedWorkingDirectory    = 2 // ShellRejected.f2 working_directory
	cursorFldNativeRejectRejectedPath                = 1 // *Rejected.f1 path
	cursorFldNativeRejectShellRejectedReason         = 3 // ShellRejected.f3 reason
	cursorFldNativeRejectWriteDeleteReadLsDiagReason = 2 // Write/Delete/Read/Ls/DiagnosticsRejected.f2 reason
	cursorFldNativeRejectErrorText                   = 1 // GrepError.f1 / FetchError.f2 / WriteShellStdinError.f1
	cursorFldNativeRejectFetchErrorURL               = 1 // FetchError.f1 url
	cursorFldNativeRejectFetchErrorText              = 2 // FetchError.f2 error
)

// 各 exec 字段内层 args 子字段号（command / working_directory / path / url / tool_call_id）。
const (
	cursorFldNativeRejectArgCommand               = 1  // ShellArgs.f1
	cursorFldNativeRejectArgWorkingDirectory      = 2  // ShellArgs.f2
	cursorFldNativeRejectArgPath                  = 1  // Write/Delete/Read/Ls/DiagnosticsArgs.f1
	cursorFldNativeRejectArgURL                   = 1  // FetchArgs.f1
	cursorFldNativeRejectArgToolCallIDShell       = 4  // ShellArgs / ShellStreamArgs.f4
	cursorFldNativeRejectArgToolCallIDBackground  = 3  // BackgroundShellSpawnArgs.f3
	cursorFldNativeRejectArgToolCallIDWrite       = 3  // WriteArgs.f3
	cursorFldNativeRejectArgToolCallIDDelete      = 2  // DeleteArgs.f2
	cursorFldNativeRejectArgToolCallIDRead        = 2  // ReadArgs.f2
	cursorFldNativeRejectArgToolCallIDLs          = 3  // LsArgs.f3
	cursorFldNativeRejectArgToolCallIDDiagnostics = 2  // DiagnosticsArgs.f2
	cursorFldNativeRejectArgToolCallIDGrep        = 14 // GrepArgs.f14
	cursorFldNativeRejectArgToolCallIDFetch       = 2  // FetchArgs.f2
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

// cursorNativeEquivalent 是「原生字段号 → 客户端等价工具名（已归一化为小写下划线形式）」表。
//
// 仅收录本卡已实现且 V2 实测有证据的 7 个字段：f2/f3/f4/f5/f7/f8/f14。
// 来源：社区 BUILTIN_EQUIVALENT（session.py:89）的等价别名列表。
//
// 刻意不收录：
//   - f29 / f52：V2 实测无证据（施工图 W12），本卡不做。
//   - f9(diagnostics) / f16(background_shell_spawn) / f20(fetch) / f23(write_shell_stdin)：
//     在我们的代码里对应「无客户端等价工具」场景，不收录（命中即退回列清单）。
//
// 等价名一律以小写下划线形式登记（community 给的就是这种形态），与客户端声明名经
// cursorNormaliseToolName 归一化后再比较——不归一就永远匹配不上（客户端工具名是大写开头驼峰）。
var cursorNativeEquivalent = map[int][]string{
	cursorFldNativeRejectShellArgs:       {"shell", "bash", "terminal", "run_command"}, // f2  shell_args
	cursorFldNativeRejectWriteArgs:       {"write", "write_file", "create_file", "edit"}, // f3  write_args
	cursorFldNativeRejectDeleteArgs:      {"remove", "delete_file"},                     // f4  delete_args
	// f5 grep_args：等价名**必须含 grep 自身** —— Claude Code 声明的工具就叫 grep
	// （生产实测声明集：question,bash,read,glob,grep,edit,write,…）。
	// 只列 search/ripgrep（社区表的形态，它假设客户端不叫 grep）会导致：
	// 客户端明明声明了 grep，我们却点不出它的名 → 拒绝文本退回「列出全部工具」，
	// 模型还是挑不出来 → 继续瞎试原生工具。
	cursorFldNativeRejectGrepArgs:        {"grep", "search", "ripgrep"},                 // f5  grep_args
	cursorFldNativeRejectReadArgs:        {"read", "read_file", "view", "cat"},          // f7  read_args
	// f8 ls_args：等价名含 ls 自身（同 f5 的理由：客户端可能就叫 ls）。
	cursorFldNativeRejectLsArgs:          {"ls", "list_dir", "glob", "list_files"},        // f8  ls_args
	cursorFldNativeRejectShellStreamArgs: {"shell", "bash", "terminal", "run_command"},  // f14 shell_stream_args
}

// cursorNormaliseToolName 把工具名归一化到小写下划线形式，用于与 cursorNativeEquivalent 比较：
//   - 驼峰 → 下划线：Read→read、WebFetch→web_fetch、myTool→my_tool
//   - 连字符 '-' 与下划线 '_' 都归一为 '_'
//   - 全小写
//
// 社区用 _normalise（session.py:82）做同样的事（Python）。我们是 Go，独立实现：社区等价名是
// 小写下划线，我们的客户端工具名是大写开头驼峰，不归一化永远匹配不上。
//
// 驼峰边界规则：在一个大写字母前，若它前面是「小写字母或数字」，则插入 '_'（只处理 ASCII
// 字母；非 ASCII 字母直接转小写，不改变分隔）。
func cursorNormaliseToolName(name string) string {
	var b strings.Builder
	b.Grow(len(name) + 4)
	prevLowerOrDigit := false
	for _, r := range name {
		switch {
		case r == '-' || r == '_':
			b.WriteByte('_')
			prevLowerOrDigit = false
		case r >= 'A' && r <= 'Z':
			if prevLowerOrDigit {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
			prevLowerOrDigit = false
		case r >= '0' && r <= '9':
			b.WriteRune(r)
			prevLowerOrDigit = true
		default:
			b.WriteRune(unicode.ToLower(r))
			prevLowerOrDigit = true
		}
	}
	return b.String()
}

// cursorNativeExecRejectReasonInfo 生成拒绝理由文本，并附带点名信息（供日志 native_exec_redirected
// 取证用）。
//
// 策略（施工图 §3 阶段 3 + S5-A 派发单改动点 1）：
//   - declared 为空：「Tool not available in this environment. The client declared no tools.」
//   - declared 非空，且被拒的原生字段 field 在等价表中命中了「某个客户端声明过的等价工具」
//     → 点名那一个（用 wire name，不是客户端原名）：
//       "This tool is not connected to the user's machine. Call the tool 'Read_' instead."
//   - 命中多个等价工具时，按客户端声明顺序点第一个（declared 切片已是声明顺序；见下方
//     遍历顺序，不另排序）。
//   - 没命中任何等价工具 → 退回列清单（清单里也用 wire name）：
//       "Tool not available in this environment. Use only the client-provided tools: A_, B_."
//
// 入参：
//   - declared：客户端**原名**切片（按声明顺序），仅用于等价表匹配（经 cursorNormaliseToolName
//     归一化后与 cursorNativeEquivalent 比较）。为什么用原名匹配：等价表登记的是语义名
//     （read/write/shell…），其归一化形态与「原名归一化」一致；而 wire name 撞内置名后会加
//     "_"（如 Read → Read_），归一化形态变成 read_，与表里 read 对不上——所以必须拿原名匹配。
//   - wireNames：与 declared **等长同序**的 wire name 切片（declared[i] 的 wire name =
//     wireNames[i]）。文本（点名 / 清单）一律用 wire name，因为模型在出站 f4 里看到的、
//     能点开的就是 wire name；点客户端原名（如 Read）模型工具表里只有 Read_，依然找不到。
//
// 铁律 1：本函数只生成文本，绝不执行任何上游要求的工具（本卡不引入任何执行能力）。
// 铁律 2「同一条规则只许有一处实现」：等价命中判定落在 cursorNativeExecRejectHit 一处，
// 本函数与日志取证共用它，不另写一份匹配循环。
//
// 返回：(reason 文本, 本次点名到的 wire name 未命中为空串, 是否命中等价表)。
func cursorNativeExecRejectReasonInfo(field int, declared, wireNames []string) (string, string, bool) {
	if len(declared) == 0 {
		return "Tool not available in this environment. The client declared no tools.", "", false
	}
	if idx, hit := cursorNativeExecRejectHit(field, declared); hit {
		pointed := ""
		if idx < len(wireNames) {
			pointed = wireNames[idx]
		}
		return "This tool is not connected to the user's machine. Call the tool '" + pointed + "' instead.",
			pointed, true
	}
	return "Tool not available in this environment. Use only the client-provided tools: " +
		strings.Join(wireNames, ", ") + ".", "", false
}

// cursorNativeExecRejectHit 返回被拒字段 field 在等价表中命中的声明工具下标，供点名文本与
// 日志共用（铁律 2：等价匹配只此一处）。declared 是客户端原名（用于归一化匹配）。
// 命中返回 (index, true)；未命中（等价表无此字段 / 无任一声明名归一化后命中）返回 (-1, false)。
func cursorNativeExecRejectHit(field int, declared []string) (int, bool) {
	if equiv, ok := cursorNativeEquivalent[field]; ok {
		set := make(map[string]struct{}, len(equiv))
		for _, e := range equiv {
			set[e] = struct{}{}
		}
		// 按客户端声明顺序，点第一个归一化后命中等价表的工具。
		for i, name := range declared {
			if _, hit := set[cursorNormaliseToolName(name)]; hit {
				return i, true
			}
		}
	}
	return -1, false
}

// cursorNativeExecRejectReason 是 cursorNativeExecRejectReasonInfo 只取文本的便捷版
// （测试 / 形状断言用：caller 通常只关心文本是否逐字节正确）。
func cursorNativeExecRejectReason(field int, declared, wireNames []string) string {
	reason, _, _ := cursorNativeExecRejectReasonInfo(field, declared, wireNames)
	return reason
}

// cursorNativeRejectArgString 从已解出的 args 字段集取回某个字符串子字段（不存在回 ""）。
func cursorNativeRejectArgString(fields []cursorPBField, n int) string {
	if f, ok := cursorFindField(fields, n); ok {
		return f.String()
	}
	return ""
}

// cursorEncodeNativeExecRejection 把一个原生 exec 字段的 args 编成对应拒绝 / 错误结果载荷。
//
// field 必须是 11 个受支持字段之一；args 是该字段的内层字节（用 cursorDecodeProtobuf 解码，
// 解码失败按协议错误返回）。字符串为空的子字段不写（proto3 默认省略）；ShellRejected 的
// is_readonly(f4) 不写。参数缺失（args 为空）照样编码出拒绝，不报错。
//
// 返回：resultField（与 field 相同，即 ExecClientMessage 结果 oneof 字段号）、payload（结果字段
// 内容：{oneof 子字段=内层 rejected/error 消息}）、toolCallID（从 args 解出的 tool_call_id，可能
// 为空）、err。
func cursorEncodeNativeExecRejection(field int, args []byte, reason string) (int, []byte, string, error) {
	sub, err := cursorDecodeProtobuf(args)
	if err != nil {
		return 0, nil, "", err
	}
	switch field {
	case cursorFldNativeRejectShellArgs, cursorFldNativeRejectShellStreamArgs, cursorFldNativeRejectBackgroundShellSpawnArgs:
		// ShellRejected{f1 command, f2 working_directory, f3 reason}；f4 is_readonly 不写。
		var inner []byte
		if cmd := cursorNativeRejectArgString(sub, cursorFldNativeRejectArgCommand); cmd != "" {
			inner = cursorAppendStringField(inner, cursorFldNativeRejectRejectedCommand, cmd)
		}
		if wd := cursorNativeRejectArgString(sub, cursorFldNativeRejectArgWorkingDirectory); wd != "" {
			inner = cursorAppendStringField(inner, cursorFldNativeRejectRejectedWorkingDirectory, wd)
		}
		inner = cursorAppendStringField(inner, cursorFldNativeRejectShellRejectedReason, reason)
		var rejectedField int
		switch field {
		case cursorFldNativeRejectShellArgs:
			rejectedField = cursorFldNativeRejectShellResultRejected
		case cursorFldNativeRejectShellStreamArgs:
			rejectedField = cursorFldNativeRejectShellStreamRejected
		case cursorFldNativeRejectBackgroundShellSpawnArgs:
			rejectedField = cursorFldNativeRejectBackgroundShellSpawnRejected
		}
		payload := cursorAppendBytesField(nil, rejectedField, inner)
		tcidField := cursorFldNativeRejectArgToolCallIDShell
		if field == cursorFldNativeRejectBackgroundShellSpawnArgs {
			tcidField = cursorFldNativeRejectArgToolCallIDBackground
		}
		return field, payload, cursorNativeRejectArgString(sub, tcidField), nil

	case cursorFldNativeRejectWriteArgs, cursorFldNativeRejectDeleteArgs,
		cursorFldNativeRejectReadArgs, cursorFldNativeRejectLsArgs, cursorFldNativeRejectDiagnosticsArgs:
		// *Rejected{f1 path, f2 reason}。
		inner := cursorAppendStringField(nil, cursorFldNativeRejectRejectedPath,
			cursorNativeRejectArgString(sub, cursorFldNativeRejectArgPath))
		inner = cursorAppendStringField(inner, cursorFldNativeRejectWriteDeleteReadLsDiagReason, reason)
		var rejectedField int
		switch field {
		case cursorFldNativeRejectWriteArgs:
			rejectedField = cursorFldNativeRejectWriteResultRejected
		case cursorFldNativeRejectDeleteArgs:
			rejectedField = cursorFldNativeRejectDeleteResultRejected
		case cursorFldNativeRejectReadArgs:
			rejectedField = cursorFldNativeRejectReadResultRejected
		case cursorFldNativeRejectLsArgs:
			rejectedField = cursorFldNativeRejectLsResultRejected
		case cursorFldNativeRejectDiagnosticsArgs:
			rejectedField = cursorFldNativeRejectDiagnosticsResultRejected
		}
		payload := cursorAppendBytesField(nil, rejectedField, inner)
		var tcidField int
		switch field {
		case cursorFldNativeRejectWriteArgs:
			tcidField = cursorFldNativeRejectArgToolCallIDWrite
		case cursorFldNativeRejectDeleteArgs:
			tcidField = cursorFldNativeRejectArgToolCallIDDelete
		case cursorFldNativeRejectReadArgs:
			tcidField = cursorFldNativeRejectArgToolCallIDRead
		case cursorFldNativeRejectLsArgs:
			tcidField = cursorFldNativeRejectArgToolCallIDLs
		case cursorFldNativeRejectDiagnosticsArgs:
			tcidField = cursorFldNativeRejectArgToolCallIDDiagnostics
		}
		return field, payload, cursorNativeRejectArgString(sub, tcidField), nil

	case cursorFldNativeRejectGrepArgs:
		// GrepResult.f2 error = GrepError{f1 error=reason}（GrepResult 无 rejected）。
		inner := cursorAppendStringField(nil, cursorFldNativeRejectErrorText, reason)
		payload := cursorAppendBytesField(nil, cursorFldNativeRejectGrepResultError, inner)
		return field, payload, cursorNativeRejectArgString(sub, cursorFldNativeRejectArgToolCallIDGrep), nil

	case cursorFldNativeRejectFetchArgs:
		// FetchResult.f2 error = FetchError{f1 url, f2 error=reason}。
		inner := cursorAppendStringField(nil, cursorFldNativeRejectFetchErrorURL,
			cursorNativeRejectArgString(sub, cursorFldNativeRejectArgURL))
		inner = cursorAppendStringField(inner, cursorFldNativeRejectFetchErrorText, reason)
		payload := cursorAppendBytesField(nil, cursorFldNativeRejectFetchResultError, inner)
		return field, payload, cursorNativeRejectArgString(sub, cursorFldNativeRejectArgToolCallIDFetch), nil

	case cursorFldNativeRejectWriteShellStdinArgs:
		// WriteShellStdinResult.f2 error = WriteShellStdinError{f1 error=reason}（无 tool_call_id）。
		inner := cursorAppendStringField(nil, cursorFldNativeRejectErrorText, reason)
		payload := cursorAppendBytesField(nil, cursorFldNativeRejectWriteShellStdinResultError, inner)
		return field, payload, "", nil

	default:
		return 0, nil, "", cursorFrameErrorf(cursorFrameDuplex,
			"unsupported native exec reject field %d", field)
	}
}

// cursorRejectNativeExec 把一个原生 exec 字段按拒绝形状写出结果帧（+ 仅 f14 的 stream_close），
// 并登记 noteRejectedCall / 记日志。
//
// 从 parts 取 field 的 Bytes → cursorEncodeNativeExecRejection（reason 用
// d.declaredToolNames() 原名匹配 + d.declaredWireToolNames() wire name 写文本）。
// 顺序：先 noteRejectedCall（发送前，仅 toolCallID 非空且 delegations 非 nil），再 send 结果帧，
// 再（仅当 field == f14 shell_stream 时）sendExecStreamClose；任一发送错误原样返回。
// 最后记两条 Info 级日志：native_exec_redirected（取证本次点名结果，S5-A 新增）与
// native_exec_rejected（含 exec_id / field / tool_call_id / declared_tool_names 原名 / count，运维视角）。
//
// 铁律（派发单 S5-A 改动点 2）：stream_close 只在 f14 发。社区对 f14 shell_stream 既回结果
// 又发 stream_close；其余字段只回结果帧。关流会让上游等不到应答，把「本轮 502」换成「流卡死」，更糟。
func (d *cursorAgentDuplex) cursorRejectNativeExec(parts []cursorPBField, field int, execID uint64, idField []byte) error {
	argsField, ok := cursorFindField(parts, field)
	if !ok {
		return cursorFrameErrorf(cursorFrameDuplex, "native reject target field %d missing in exec message", field)
	}
	// declared（原名）仅用于等价表匹配；wireNames（wire name）写进拒绝文本，模型才认识。
	declared := d.declaredToolNames()
	wireNames := d.declaredWireToolNames()
	reason, redirectedTool, equivalentHit := cursorNativeExecRejectReasonInfo(field, declared, wireNames)
	resultField, payload, toolCallID, err := cursorEncodeNativeExecRejection(field, argsField.Bytes, reason)
	if err != nil {
		return err
	}
	if toolCallID != "" && d.delegations != nil {
		d.delegations.noteRejectedCall(toolCallID)
	}
	frame := cursorEncodeExecClientResultFrame(execID, idField, resultField, payload)
	if err := d.send(cursorFldClientMessage, frame); err != nil {
		return err
	}
	// 改动点 2：stream_close 只在 f14 shell_stream 发；其余字段只发结果帧。
	if field == cursorFldNativeRejectShellStreamArgs {
		if err := d.sendExecStreamClose(execID); err != nil {
			return err
		}
	}
	// 改动点 3：native_exec_redirected 取证日志（S5-A 新增，W7 要求）。
	// 字段纪律：绝不记 command / path / url 值；reject_reason 仅含本卡生成的固定文案 + 工具名，
	// 无客户业务数据，可记。
	d.logPhase(zapcore.InfoLevel, "native_exec_redirected", "",
		zap.Int("field", field),
		zap.String("redirected_tool", redirectedTool),
		zap.Bool("equivalent_hit", equivalentHit),
		zap.Int("declared_tool_count", len(declared)),
		zap.String("reject_reason", reason),
		zap.Int("result_oneof_field", resultField))
	d.logPhase(zapcore.InfoLevel, "native_exec_rejected", "",
		zap.Uint64("exec_id", execID),
		zap.Int("field", field),
		zap.String("tool_call_id", toolCallID),
		zap.String("declared_tool_names", strings.Join(declared, ",")),
		zap.Int("declared_tool_name_count", len(declared)))
	return nil
}

// declaredWireToolNames 从 declaredToolDefs 直接提取每条工具定义的工具名（**不经过**
// declaredToolReverse）。declaredToolDefs 存的是 f4 原始字节，卡 01 改名后里面的工具名已经是
// wire name（撞 Cursor 内置名时加 "_"，如 Read → Read_）——这正是模型在出站 f4 里看到、
// 点名文本必须用的名字。
//
// 与 declaredToolNames 同序、等长（两者都按 declaredToolDefs 顺序逐项解码，解码失败都跳过，
// 同一份字节不会一个解出一个解不出），故 declared[i] 的 wire name == 本切片第 i 个。
func (d *cursorAgentDuplex) declaredWireToolNames() []string {
	if len(d.declaredToolDefs) == 0 {
		return nil
	}
	names := make([]string, 0, len(d.declaredToolDefs))
	for _, raw := range d.declaredToolDefs {
		fields, err := cursorDecodeProtobuf(raw)
		if err != nil {
			continue
		}
		for _, f := range fields {
			if f.Number == cursorFldMcpToolDefToolName {
				names = append(names, f.String())
				break
			}
		}
	}
	return names
}

// ---------------------------------------------------------------------------
// 通用兜底形状（派发单 D 阶段 2：未知 exec 字段必应答）
// ---------------------------------------------------------------------------

// cursorGenericUnknownExecRejectText 是「未知 exec 字段」通用兜底帧的错误文本（人话：
// 这个工具在本环境不可用）。与 11 类原生 exec 的精确拒绝文本互不干扰、不共享常量。
const cursorGenericUnknownExecRejectText = "This tool is not available in this environment."

// cursorEncodeExecGenericUnknownFieldReject 构造「未知 exec 字段」的通用兜底错误结果帧：
// {field:{f2:{f2:text}}} —— 外层 field = 未知字段号，内层 f2 是结果 oneof 子字段，
// 再内层 f2 是错误文本。
//
// 这是派发单 D 阶段 2 的「未知字段必应答」兜底：exec 里出现白名单外字段时，不再以
// CUR-EXEC-02 / 502 把整轮打死，而是回这个形状让模型换条路继续（绝不能 sendExecStreamClose
// 关流——关流会让上游永远等不到应答，把「本轮 502」换成「流卡死」，更糟）。
//
// 本函数只编形状、不接入派发 switch、不动任何既有精确拒绝形状（精确拒绝形状见上方
// cursorEncodeNativeExecRejection 及其常量）。
func cursorEncodeExecGenericUnknownFieldReject(execID uint64, idField []byte, field int, text string) []byte {
	payload := cursorAppendBytesField(nil, 2, cursorAppendStringField(nil, 2, text))
	return cursorEncodeExecClientResultFrame(execID, idField, field, payload)
}
