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
func cursorNativeExecRejectField(parts []cursorPBField) (int, bool) {
	for _, n := range []int{
		cursorFldNativeRejectShellArgs,
		cursorFldNativeRejectWriteArgs,
		cursorFldNativeRejectDeleteArgs,
		cursorFldNativeRejectGrepArgs,
		cursorFldNativeRejectReadArgs,
		cursorFldNativeRejectLsArgs,
		cursorFldNativeRejectDiagnosticsArgs,
		cursorFldNativeRejectShellStreamArgs,
		cursorFldNativeRejectBackgroundShellSpawnArgs,
		cursorFldNativeRejectFetchArgs,
		cursorFldNativeRejectWriteShellStdinArgs,
	} {
		if _, ok := cursorFindField(parts, n); ok {
			return n, true
		}
	}
	return 0, false
}

// cursorNativeExecRejectReason 生成拒绝理由文本（唯一，取自方案）。
//
// declared 非空：「Tool not available in this environment. Use only the client-provided tools: 」
// + 逗号拼接的声明工具名 + "."
// declared 为空：「Tool not available in this environment. The client declared no tools.」
func cursorNativeExecRejectReason(declared []string) string {
	if len(declared) > 0 {
		return "Tool not available in this environment. Use only the client-provided tools: " +
			strings.Join(declared, ", ") + "."
	}
	return "Tool not available in this environment. The client declared no tools."
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

// cursorRejectNativeExec 把一个原生 exec 字段按拒绝形状写出结果帧 + stream_close，并登记
// noteRejectedCall / 记日志。
//
// 从 parts 取 field 的 Bytes → cursorEncodeNativeExecRejection（reason 用 d.declaredToolNames()）。
// 顺序：先 noteRejectedCall（发送前，仅 toolCallID 非空且 delegations 非 nil），再 send 结果帧，
// 再 sendExecStreamClose；任一发送错误原样返回。最后记 Info 级 native_exec_rejected（含
// exec_id / field / tool_call_id / declared_tool_names / declared_tool_name_count，绝不记
// command / path / url 的值）。
func (d *cursorAgentDuplex) cursorRejectNativeExec(parts []cursorPBField, field int, execID uint64, idField []byte) error {
	argsField, ok := cursorFindField(parts, field)
	if !ok {
		return cursorFrameErrorf(cursorFrameDuplex, "native reject target field %d missing in exec message", field)
	}
	reason := cursorNativeExecRejectReason(d.declaredToolNames())
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
	if err := d.sendExecStreamClose(execID); err != nil {
		return err
	}
	declared := d.declaredToolNames()
	d.logPhase(zapcore.InfoLevel, "native_exec_rejected", "",
		zap.Uint64("exec_id", execID),
		zap.Int("field", field),
		zap.String("tool_call_id", toolCallID),
		zap.String("declared_tool_names", strings.Join(declared, ",")),
		zap.Int("declared_tool_name_count", len(declared)))
	return nil
}
