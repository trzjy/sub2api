package service

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"go.uber.org/zap"
)

// Cursor 原生文件工具桥接（ExecServerMessage.f3/f4/f5/f7/f8 → 下游 tool_call → 回写对应 Result）。
//
// 背景（生产实测，已钉死）：CodeBuddy / Claude Code 这类真实客户端会声明 read / write / ls /
// grep / delete 类工具，上游不走 mcp_args(f11)，而走 ExecServerMessage 原生通道：
//
//	phase=exec_unsupported_fields model=claude-opus-5-5 field_numbers=[1,7,15,19,55]
//
// 本组函数与 shell 桥接（f14）同一套模式：解码 Args → 按声明 schema 选下游同名工具并转参数
// → 走同一套"独立批立即可封口"登记 → 续轮按 Kind 编码对应 result 帧 + stream_close。
// mcp(f11) 与 shell(f14) 的既有字节级行为一律不变。
//
// 字段号来源：派发单给出的 bundle（workbench.desktop.main.js 内 makeMessageType("agent.v1.<Name>")
// 逐字段 dump）。**本机无该 bundle 原件，字段号未能在交付环境内二次复核**（见交付报告
// "字段号对照" 一节）；若后续拿到原件复核出差异，只需改本文件的常量与编码函数。

// ---------------------------------------------------------------------------
// 字段号
// ---------------------------------------------------------------------------

const (
	// ExecServerMessage 原生文件工具入参字段号（与 ExecClientMessage 回包字段号对称）。
	cursorFldServerMsgWriteArgs  = 3
	cursorFldServerMsgDeleteArgs = 4
	cursorFldServerMsgGrepArgs   = 5
	cursorFldServerMsgReadArgs   = 7
	cursorFldServerMsgLsArgs     = 8

	// ExecClientMessage 原生文件工具回包字段号。
	cursorFldClientMsgWriteResult  = 3
	cursorFldClientMsgDeleteResult = 4
	cursorFldClientMsgGrepResult   = 5
	cursorFldClientMsgReadResult   = 7
	cursorFldClientMsgLsResult     = 8
)

// 委托 Kind 取值（cursorToolCallCapture.Kind）：空为 mcp，其余为原生 exec 通道。
const (
	cursorToolKindShell  = "shell"
	cursorToolKindRead   = "read"
	cursorToolKindLs     = "ls"
	cursorToolKindGrep   = "grep"
	cursorToolKindWrite  = "write"
	cursorToolKindDelete = "delete"
)

// ErrCursorNativeFsToolAbsent 是专用哨兵：原生文件工具的"候选名单（priority）全部不匹配、
// 客户端未声明该 kind 对应工具"这一种情况返回它。与 ErrCursorProtocol（schema 解析失败、
// required 无法填充、unknown kind 等真正的协议失败关闭）区分开 —— 工具缺席不是协议错误，
// 而是"客户端本来就不可用"，应当回一条失败的 Result 帧让流继续，而非整请求 502。
var ErrCursorNativeFsToolAbsent = errors.New("cursor: native fs tool not declared by client")

// result oneof 内层字段号（ReadResult / WriteResult / LsResult / GrepResult / DeleteResult）。
const (
	// ReadResult: f1 success | f2 error | f3 rejected | f4 file_not_found | f5 permission_denied | f6 invalid_file
	cursorFldReadResultSuccess = 1
	cursorFldReadResultError   = 2

	// WriteResult: f1 success | f3 permission_denied | f4 no_space | f5 error | f6 rejected
	cursorFldWriteResultSuccess = 1
	cursorFldWriteResultError   = 5

	// DeleteResult: f1 success | f2 file_not_found | f3 not_file | f4 permission_denied | f5 file_busy | f6 rejected | f7 error
	cursorFldDeleteResultSuccess = 1
	cursorFldDeleteResultError   = 7

	// LsResult: f1 success | f2 error | f3 rejected | f4 timeout
	cursorFldLsResultSuccess = 1
	cursorFldLsResultError   = 2

	// GrepResult: f1 success | f2 error
	cursorFldGrepResultSuccess = 1
	cursorFldGrepResultError   = 2
)

// ReadSuccess / ReadError 内层字段号。
const (
	cursorFldReadSuccessPath       = 1 // f1 path
	cursorFldReadSuccessContent    = 2 // f2 content（oneof output）
	cursorFldReadSuccessTotalLines = 3 // f3 total_lines(int32)
	cursorFldReadSuccessFileSize   = 4 // f4 file_size(int64)
	cursorFldReadErrorPath         = 1 // f1 path
	cursorFldReadErrorError        = 2 // f2 error
)

// WriteSuccess / WriteError 内层字段号。
const (
	cursorFldWriteSuccessPath        = 1 // f1 path
	cursorFldWriteSuccessLinesCreate = 2 // f2 lines_created
	cursorFldWriteSuccessFileSize    = 3 // f3 file_size
	cursorFldWriteErrorPath          = 1 // f1 path
	cursorFldWriteErrorError         = 2 // f2 error
)

// DeleteSuccess / DeleteError 内层字段号。
const (
	cursorFldDeleteSuccessPath = 1 // f1 path
	cursorFldDeleteErrorPath   = 1 // f1 path
	cursorFldDeleteErrorError  = 2 // f2 error
)

// LsSuccess / LsError / LsDirectoryTreeNode 内层字段号。
const (
	cursorFldLsSuccessRoot           = 1 // f1 directory_tree_root
	cursorFldLsErrorPath             = 1 // f1 path
	cursorFldLsErrorError            = 2 // f2 error
	cursorFldLsNodeAbsPath           = 1 // f1 abs_path
	cursorFldLsNodeChildrenDirs      = 2 // f2 children_dirs(repeated LsDirectoryTreeNode)
	cursorFldLsNodeChildrenFiles     = 3 // f3 children_files(repeated)
	cursorFldLsNodeChildrenProcessed = 4 // f4 children_were_processed(bool)
	cursorFldLsNodeNumFiles          = 6 // f6 num_files
	cursorFldLsFileNodePath          = 1 // 文件子节点承载路径的字段（见 cursorEncodeLsResult 注释）
)

// GrepResult error 内层字段号。
const cursorFldGrepErrorError = 1 // f1 error

// GrepSuccess / GrepUnionResult / GrepContentResult 等内层字段号（来自 bundle 逐字段 dump）。
//
// GrepSuccess 的 f5 active_editor_result、GrepUnionResult 的 f1 count / f2 files 本桥接不编码
// （客户端 grep 结果只回一段文本，无编辑器上下文与计数摘要可言，填任一都是编造）。
const (
	cursorFldGrepSuccessPattern          = 1 // f1 pattern
	cursorFldGrepSuccessPath             = 2 // f2 path
	cursorFldGrepSuccessOutputMode       = 3 // f3 output_mode
	cursorFldGrepSuccessWorkspaceResults = 4 // f4 workspace_results(map<string, GrepUnionResult>)

	// GrepUnionResult（oneof result）: f1 count | f2 files | f3 content。本桥接只写 f3 content；
	// f1/f2 需要客户端未提供的计数/文件清单，不臆造（常量仅用于测试断言 oneof 互斥）。
	cursorFldGrepUnionCount   = 1
	cursorFldGrepUnionFiles   = 2
	cursorFldGrepUnionContent = 3

	// GrepContentResult: f1 matches(repeated GrepFileMatch) | f2 total_lines |
	// f3 total_matched_lines | f4 client_truncated | f5 ripgrep_truncated |
	// f6 head_limit_applied(opt) | f7 offset_applied(opt)。
	cursorFldGrepContentMatches           = 1
	cursorFldGrepContentTotalLines        = 2
	cursorFldGrepContentTotalMatchedLines = 3

	// GrepFileMatch: f1 file | f2 matches(repeated GrepContentMatch)。
	cursorFldGrepFileMatchFile    = 1
	cursorFldGrepFileMatchMatches = 2

	// GrepContentMatch: f1 line_number | f2 content | f3 content_truncated | f4 is_context_line。
	// 本桥接只填 f1/f2：客户端结果文本里没有可判定"是否上下文行"的信息，不臆造 f4。
	cursorFldGrepContentMatchLineNumber = 1
	cursorFldGrepContentMatchContent    = 2

	// workspace_results 的 map entry（protobuf map< string, GrepUnionResult > 展开为重复 entry）。
	cursorFldGrepWorkspaceKey   = 1 // f1 key(string)
	cursorFldGrepWorkspaceValue = 2 // f2 value(GrepUnionResult)
)

// ---------------------------------------------------------------------------
// 入参解码
// ---------------------------------------------------------------------------

// cursorReadArgs 是 ExecServerMessage.f7（ReadArgs）的解出结构。
type cursorReadArgs struct {
	Path         string
	ToolCallID   string
	Offset       int64
	Limit        int64
	EncodingHint string
	DigestOnly   bool
}

// cursorLsArgs 是 ExecServerMessage.f8（LsArgs）的解出结构。
type cursorLsArgs struct {
	Path       string
	Ignore     []string
	ToolCallID string
	TimeoutMS  int64
}

// cursorGrepArgs 是 ExecServerMessage.f5（GrepArgs）的解出结构。
type cursorGrepArgs struct {
	Pattern         string
	Path            string
	Glob            string
	OutputMode      string
	ContextBefore   int64
	ContextAfter    int64
	Context         int64
	CaseInsensitive bool
	Type            string
	HeadLimit       int64
	Multiline       bool
	Sort            string
	SortAscending   bool
	ToolCallID      string
	Offset          int64
}

// cursorWriteArgs 是 ExecServerMessage.f3（WriteArgs）的解出结构。
type cursorWriteArgs struct {
	Path         string
	FileText     string
	ToolCallID   string
	FileBytes    []byte
	EncodingHint string
	// ReturnFileContentAfterWrite 是 f4（bool，opt）：客户端要求回包携带写后内容。
	ReturnFileContentAfterWrite bool
}

// cursorDeleteArgs 是 ExecServerMessage.f4（DeleteArgs）的解出结构。
type cursorDeleteArgs struct {
	Path       string
	ToolCallID string
}

// cursorParseReadArgs 逐字段解出 ReadArgs（f1 path / f2 tool_call_id / f4 offset /
// f5 limit / f6 encoding_hint / f7 digest_only）。
func cursorParseReadArgs(sub []cursorPBField) cursorReadArgs {
	var a cursorReadArgs
	if f, ok := cursorFindField(sub, 1); ok {
		a.Path = f.String()
	}
	if f, ok := cursorFindField(sub, 2); ok {
		a.ToolCallID = f.String()
	}
	a.Offset = int64(int32(cursorVarintField(sub, 4)))
	a.Limit = int64(uint32(cursorVarintField(sub, 5)))
	if f, ok := cursorFindField(sub, 6); ok {
		a.EncodingHint = f.String()
	}
	if f, ok := cursorFindField(sub, 7); ok && f.Wire == cursorWireVarint {
		a.DigestOnly = f.Varint != 0
	}
	return a
}

// cursorParseLsArgs 逐字段解出 LsArgs（f1 path / f2 ignore(repeated) / f3 tool_call_id /
// f5 timeout_ms）。
func cursorParseLsArgs(sub []cursorPBField) cursorLsArgs {
	var a cursorLsArgs
	if f, ok := cursorFindField(sub, 1); ok {
		a.Path = f.String()
	}
	for _, f := range sub {
		if f.Number == 2 {
			a.Ignore = append(a.Ignore, f.String())
		}
	}
	if f, ok := cursorFindField(sub, 3); ok {
		a.ToolCallID = f.String()
	}
	a.TimeoutMS = int64(cursorVarintField(sub, 5))
	return a
}

// cursorParseGrepArgs 逐字段解出 GrepArgs（f1 pattern / f2 path / f3 glob / f4 output_mode /
// f5 context_before / f6 context_after / f7 context / f8 case_insensitive / f9 type /
// f10 head_limit / f11 multiline / f12 sort / f13 sort_ascending / f14 tool_call_id / f16 offset）。
func cursorParseGrepArgs(sub []cursorPBField) cursorGrepArgs {
	var a cursorGrepArgs
	str := func(n int) string {
		if f, ok := cursorFindField(sub, n); ok {
			return f.String()
		}
		return ""
	}
	a.Pattern = str(1)
	a.Path = str(2)
	a.Glob = str(3)
	a.OutputMode = str(4)
	a.ContextBefore = int64(int32(cursorVarintField(sub, 5)))
	a.ContextAfter = int64(int32(cursorVarintField(sub, 6)))
	a.Context = int64(int32(cursorVarintField(sub, 7)))
	if f, ok := cursorFindField(sub, 8); ok && f.Wire == cursorWireVarint {
		a.CaseInsensitive = f.Varint != 0
	}
	a.Type = str(9)
	a.HeadLimit = int64(int32(cursorVarintField(sub, 10)))
	if f, ok := cursorFindField(sub, 11); ok && f.Wire == cursorWireVarint {
		a.Multiline = f.Varint != 0
	}
	a.Sort = str(12)
	if f, ok := cursorFindField(sub, 13); ok && f.Wire == cursorWireVarint {
		a.SortAscending = f.Varint != 0
	}
	a.ToolCallID = str(14)
	a.Offset = int64(int32(cursorVarintField(sub, 16)))
	return a
}

// cursorParseWriteArgs 逐字段解出 WriteArgs（f1 path / f2 file_text / f3 tool_call_id /
// f4 return_file_content_after_write / f5 file_bytes / f6 encoding_hint）。
func cursorParseWriteArgs(sub []cursorPBField) cursorWriteArgs {
	var a cursorWriteArgs
	if f, ok := cursorFindField(sub, 1); ok {
		a.Path = f.String()
	}
	if f, ok := cursorFindField(sub, 2); ok {
		a.FileText = f.String()
	}
	if f, ok := cursorFindField(sub, 3); ok {
		a.ToolCallID = f.String()
	}
	if f, ok := cursorFindField(sub, 4); ok && f.Wire == cursorWireVarint {
		a.ReturnFileContentAfterWrite = f.Varint != 0
	}
	if f, ok := cursorFindField(sub, 5); ok && len(f.Bytes) > 0 {
		a.FileBytes = append([]byte(nil), f.Bytes...)
	}
	if f, ok := cursorFindField(sub, 6); ok {
		a.EncodingHint = f.String()
	}
	// file_text 与 file_bytes 二选一（真实客户端只填其中一个）；两者都空 → 调用方失败关闭。
	if a.FileText == "" && len(a.FileBytes) > 0 {
		a.FileText = string(a.FileBytes)
	}
	return a
}

// cursorParseDeleteArgs 逐字段解出 DeleteArgs（f1 path / f2 tool_call_id）。
func cursorParseDeleteArgs(sub []cursorPBField) cursorDeleteArgs {
	var a cursorDeleteArgs
	if f, ok := cursorFindField(sub, 1); ok {
		a.Path = f.String()
	}
	if f, ok := cursorFindField(sub, 2); ok {
		a.ToolCallID = f.String()
	}
	return a
}

// ---------------------------------------------------------------------------
// 下游工具选择与参数转换
// ---------------------------------------------------------------------------

// cursorNativeFsPathProps / cursorNativeFsContentProps 等是逻辑参数 → 目标 schema 属性名的
// 候选表（按顺序取 schema 中第一个实际存在的属性名）。与 shell 桥接的 workdirProp 同策略：
// 只填目标 schema 认识的属性名，绝不硬塞目标工具不认识的键。
var (
	cursorNativeFsPathProps    = []string{"path", "file_path", "filepath", "file", "dir", "directory"}
	cursorNativeFsContentProps = []string{"content", "file_text", "text", "new_str"}
	cursorNativeFsPatternProps = []string{"pattern", "query", "q"}
	cursorNativeFsGlobProps    = []string{"glob", "include"}
	cursorNativeFsOutputProps  = []string{"output_mode", "output"}
	cursorNativeFsIgnoreProps  = []string{"ignore", "ignore_patterns", "exclude"}
	cursorNativeFsOffsetProps  = []string{"offset"}
	cursorNativeFsLimitProps   = []string{"limit", "max_lines"}
	cursorNativeFsCaseProps    = []string{"case_insensitive", "ignore_case", "-i"}
	cursorNativeFsTypeProps    = []string{"type"}
	cursorNativeFsHeadProps    = []string{"head_limit"}
	cursorNativeFsMultiline    = []string{"multiline"}
	cursorNativeFsSortProps    = []string{"sort"}
	cursorNativeFsSortAscProps = []string{"sort_ascending"}
	cursorNativeFsCtxProps     = []string{"context", "-C"}
	cursorNativeFsCtxBefore    = []string{"context_before", "-B"}
	cursorNativeFsCtxAfter     = []string{"context_after", "-A"}
)

// cursorNativeFsParam 是一个逻辑参数：候选属性名 + 值（值已按 Go 类型给出，序列化为 JSON）。
type cursorNativeFsParam struct {
	candidates []string
	value      any
}

// cursorNativeFsKindSpec 描述一种原生文件工具的下游工具名优先级与"关键参数"。
type cursorNativeFsKindSpec struct {
	// priority 是工具名选择优先级（按顺序取第一个在声明集中、schema 可用且含关键属性的）。
	priority []string
	// keyProps 是关键参数的候选属性名；schema 里一个都没有 → 该候选工具不可用。
	keyProps []string
}

// cursorNativeFsKinds 是五种原生文件工具的选择规则（派发单"下游工具选择与参数转换"）。
// 找不到可用工具 → 失败关闭（不自造工具名）。
var cursorNativeFsKinds = map[string]cursorNativeFsKindSpec{
	cursorToolKindRead: {
		priority: []string{"read_file", "Read", "read"},
		keyProps: cursorNativeFsPathProps,
	},
	cursorToolKindWrite: {
		priority: []string{"write_file", "Write", "write"},
		keyProps: cursorNativeFsPathProps,
	},
	cursorToolKindLs: {
		priority: []string{"ls", "LS", "list_dir", "List"},
		keyProps: cursorNativeFsPathProps,
	},
	cursorToolKindGrep: {
		priority: []string{"grep", "search", "Grep"},
		keyProps: cursorNativeFsPatternProps,
	},
	cursorToolKindDelete: {
		priority: []string{"delete_file", "delete", "Delete"},
		keyProps: cursorNativeFsPathProps,
	},
}

// cursorBuildNativeFsDelegation 从本请求 f4 声明集中按 kind 选择下游工具，并按其 input_schema
// 把原生 Args 转成 ArgsJSON（对象 JSON，与 mcp/shell 同形）。
//
// 三路判定（顺序固定，NSFS-03 逐 kind 回退）：
//  1. 有原生同名工具（现有逻辑，最高优先）→ 原样返回，resultArgsJSON 留空（回写沿用 ArgsJSON，字节不变）；
//  2. 本 kind 无原生工具（chosen==""），且候选名单里确实没有该 kind 的工具名（hasName==false，
//     即客户端根本没声明本 kind 的原生工具）→ 若声明集里有可用 shell 类工具（复用
//     cursorShellToolPriority 与 cursorSchemaHasProperty(sj, "command") 的既有判定）→ 返回 shell
//     工具名 + shell 命令参数，并让 resultArgsJSON 设为原生参数 JSON（供回写取 path/pattern）；
//     这样每个 kind 各自决定是否回退，Claude Code 声明 Read/Write/Bash 却无 ls/grep 的 ls/grep 委托
//     即可回退到 Bash，而缺失 kind 又没有任何 shell 可退时仍走拒绝帧。
//  3. 都没有（chosen=="" 且（hasName==true 走到 ErrCursorProtocol 失败关闭，或 hasName==false 且
//     shell 也不可用））→ 仍返回 ErrCursorNativeFsToolAbsent（既有拒绝帧路径，行为不变）。
func cursorBuildNativeFsDelegation(defs [][]byte, kind string, params []cursorNativeFsParam) (toolName, argsJSON, resultArgsJSON string, err error) {
	spec, ok := cursorNativeFsKinds[kind]
	if !ok {
		return "", "", "", fmt.Errorf("%w: unknown native fs kind %q", ErrCursorProtocol, kind)
	}
	idx := cursorToolSchemaIndex(defs)
	var chosen string
	var schemaJSON []byte
	// hasName 记录是否有候选工具名（不论其 schema 是否含关键属性）出现在声明集中：用于区分
	// "工具彻底缺席"（无任何候选名 → 专用哨兵，回失败帧）与"工具名声明了但 schema 缺关键属性"
	// （无法填充关键参数 → 仍是协议层失败关闭，不自造工具名）。
	hasName := false
	for _, name := range spec.priority {
		sj, ok := idx[name]
		if !ok || len(sj) == 0 {
			continue
		}
		hasName = true
		if !cursorSchemaHasAnyProperty(sj, spec.keyProps) {
			continue
		}
		chosen = name
		schemaJSON = sj
		break
	}
	if chosen != "" {
		// 路径 1：有原生同名工具 → 原样返回，resultArgsJSON 留空（回写沿用 ArgsJSON，字节不变）。
		var schema map[string]any
		if err := json.Unmarshal(schemaJSON, &schema); err != nil {
			return "", "", "", fmt.Errorf("%w: parse %s tool schema: %v", ErrCursorProtocol, kind, err)
		}
		props, _ := schema["properties"].(map[string]any)
		obj := map[string]any{}
		for _, p := range params {
			prop := cursorFirstSchemaProp(props, p.candidates)
			if prop == "" {
				continue
			}
			obj[prop] = p.value
		}
		// required 校验：凡本桥接无法填充的 required 字段 → 失败关闭（不猜默认值）。
		if reqRaw, ok := schema["required"].([]any); ok {
			for _, r := range reqRaw {
				req, ok := r.(string)
				if !ok {
					continue
				}
				if _, filled := obj[req]; !filled {
					return "", "", "", fmt.Errorf("%w: %s tool %q requires field %q which native %s args cannot fill",
						ErrCursorProtocol, kind, chosen, req, kind)
				}
			}
		}
		b, err := json.Marshal(obj)
		if err != nil {
			return "", "", "", fmt.Errorf("%w: marshal %s args: %v", ErrCursorProtocol, kind, err)
		}
		return chosen, string(b), "", nil
	}

	// 路径 2：本 kind 无原生工具 → 只要该 kind 的候选名单里确实没有对应工具名（hasName==false，
	// 即客户端根本没声明本 kind 的原生工具），就尝试回退到 shell（逐 kind 回退，NSFS-03）。
	// 若候选名单里有该 kind 的工具名（hasName==true，但 schema 缺关键属性 → 既有 ErrCursorProtocol
	// 失败关闭），或 shell 也不可用，则落到路径 3 仍走 Absent 拒绝帧，绝不改变既有拒绝语义。
	if !hasName {
		if shellName, shellSchema := cursorPickShellTool(idx); shellName != "" {
			cmd, cerr := cursorNativeFsShellCommand(kind, params)
			if cerr != nil {
				return "", "", "", cerr
			}
			obj := map[string]any{"command": cmd}
			// 与 cursorBuildShellDelegation 同口径：command 为 array 类型时包成 ["bash","-lc",<cmd>]
			// （command 恒有值，required 校验恒过）。
			if sp, ok := shellSchema["properties"].(map[string]any); ok {
				if cp, ok := sp["command"].(map[string]any); ok {
					if ct, _ := cp["type"].(string); ct == "array" {
						obj["command"] = []any{"bash", "-lc", cmd}
					}
				}
			}
			b, merr := json.Marshal(obj)
			if merr != nil {
				return "", "", "", fmt.Errorf("%w: marshal shell fallback args: %v", ErrCursorProtocol, merr)
			}
			return shellName, string(b), cursorNativeFsParamsToJSON(params), nil
		}
	}

	// 路径 3：都没有 → 仍返回 ErrCursorNativeFsToolAbsent（既有拒绝帧路径，行为不变）。
	if hasName {
		// 工具名声明了但 schema 缺关键属性：无法填充关键参数 → 失败关闭（协议层，非工具缺席），
		// 沿用既有 ErrCursorProtocol 文案，绝不自造工具名。
		return "", "", "", fmt.Errorf("%w: no %s-like tool (%s) with %s property declared",
			ErrCursorProtocol, kind, strings.Join(spec.priority, "/"), strings.Join(spec.keyProps, "/"))
	}
	// 候选名单全部不匹配（工具彻底缺席，而非协议错误）：返回专用哨兵，由捕获侧回失败帧。
	return "", "", "", fmt.Errorf("%w: no %s-like tool (%s) declared",
		ErrCursorNativeFsToolAbsent, kind, strings.Join(spec.priority, "/"))
}

// cursorPickShellTool 从 schema 索引里选第一个可用的 shell 类工具（复用 cursorShellToolPriority
// 与 cursorSchemaHasProperty(sj, "command") 的既有判定）。无则 ("", nil)。
func cursorPickShellTool(idx map[string][]byte) (string, map[string]any) {
	for _, name := range cursorShellToolPriority {
		sj, ok := idx[name]
		if !ok || sj == nil {
			continue
		}
		if !cursorSchemaHasProperty(sj, "command") {
			continue
		}
		var schema map[string]any
		if err := json.Unmarshal(sj, &schema); err != nil {
			continue
		}
		return name, schema
	}
	return "", nil
}

// cursorShellQuote 用单引号包裹并把内部 ' 转成 '\”，POSIX sh 下安全。
func cursorShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// cursorNativeFsShellParam 取 params 中属于给定候选属性组的值（按各 param 的候选项首名匹配）；
// 无则 (nil, false)。供 shell 回退时从已解析参数里取 path/pattern/content 等。
func cursorNativeFsShellParam(params []cursorNativeFsParam, candidates []string) (any, bool) {
	for _, p := range params {
		if len(p.candidates) == 0 {
			continue
		}
		for _, c := range candidates {
			if p.candidates[0] == c {
				return p.value, true
			}
		}
	}
	return nil, false
}

// cursorNativeFsShellParamString 是 cursorNativeFsShellParam 的 string 便捷形态（缺省空串）。
func cursorNativeFsShellParamString(params []cursorNativeFsParam, candidates []string) string {
	v, ok := cursorNativeFsShellParam(params, candidates)
	if !ok {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// cursorNativeFsShellParamInt 是 cursorNativeFsShellParam 的 int64 便捷形态（缺省 0）。
func cursorNativeFsShellParamInt(params []cursorNativeFsParam, candidates []string) (int64, bool) {
	v, ok := cursorNativeFsShellParam(params, candidates)
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case float64:
		return int64(n), true
	}
	return 0, false
}

// cursorNativeFsShellCommand 把原生文件工具委托翻译为一条 shell 命令。
//
// 路径/模式一律用单引号转义（' → '\”）；内容类（write）用 base64 传递，绝不做字符串拼接转义。
// 这是 NSFS-02 的安全边界：当客户端只声明了 shell 类工具、没有原生 read/write/ls/grep/delete
// 时，把上游下发的原生 exec（f3/f4/f5/f7/f8）转译成 shell 命令交给客户端执行，结果仍按原生
// 通道回写上游。
func cursorNativeFsShellCommand(kind string, params []cursorNativeFsParam) (string, error) {
	switch kind {
	case cursorToolKindRead:
		p := cursorNativeFsShellParamString(params, cursorNativeFsPathProps)
		off, hasOff := cursorNativeFsShellParamInt(params, cursorNativeFsOffsetProps)
		lim, hasLim := cursorNativeFsShellParamInt(params, cursorNativeFsLimitProps)
		if hasOff || hasLim {
			start := off + 1
			end := off + lim
			return fmt.Sprintf("sed -n %s,%sp -- %s",
				cursorShellQuote(strconv.FormatInt(start, 10)),
				cursorShellQuote(strconv.FormatInt(end, 10)),
				cursorShellQuote(p)), nil
		}
		return fmt.Sprintf("cat -- %s", cursorShellQuote(p)), nil
	case cursorToolKindLs:
		p := cursorNativeFsShellParamString(params, cursorNativeFsPathProps)
		return fmt.Sprintf("ls -la -- %s", cursorShellQuote(p)), nil
	case cursorToolKindGrep:
		pattern := cursorNativeFsShellParamString(params, cursorNativeFsPatternProps)
		p := cursorNativeFsShellParamString(params, cursorNativeFsPathProps)
		outputMode := cursorNativeFsShellParamString(params, cursorNativeFsOutputProps)
		glob := cursorNativeFsShellParamString(params, cursorNativeFsGlobProps)
		flag := "-rn"
		switch outputMode {
		case "files_with_matches":
			flag = "-rl"
		case "count":
			flag = "-rc"
		case "content":
			flag = "-rn"
		default:
			flag = "-rn"
		}
		if p == "" {
			p = "."
		}
		cmd := fmt.Sprintf("grep %s -- %s %s", flag, cursorShellQuote(pattern), cursorShellQuote(p))
		if glob != "" {
			cmd += fmt.Sprintf(" --include=%s", cursorShellQuote(glob))
		}
		return cmd, nil
	case cursorToolKindWrite:
		p := cursorNativeFsShellParamString(params, cursorNativeFsPathProps)
		content := cursorNativeFsShellParamString(params, cursorNativeFsContentProps)
		dir := path.Dir(p)
		b64 := base64.StdEncoding.EncodeToString([]byte(content))
		// 先 mkdir 再写；base64 只含 [A-Za-z0-9+/=] 字符，单引号包裹绝对安全，绝不做字符串拼接转义。
		return fmt.Sprintf("mkdir -p -- %s && printf %%s %s | base64 -d > -- %s",
			cursorShellQuote(dir), cursorShellQuote(b64), cursorShellQuote(p)), nil
	case cursorToolKindDelete:
		p := cursorNativeFsShellParamString(params, cursorNativeFsPathProps)
		return fmt.Sprintf("rm -f -- %s", cursorShellQuote(p)), nil
	}
	return "", fmt.Errorf("%w: cannot build shell command for native fs kind %q", ErrCursorProtocol, kind)
}

// cursorFirstSchemaProp 返回 properties 中第一个存在的候选属性名（都没有 → 空串）。
func cursorFirstSchemaProp(props map[string]any, candidates []string) string {
	if props == nil {
		return ""
	}
	for _, cand := range candidates {
		if _, ok := props[cand]; ok {
			return cand
		}
	}
	return ""
}

// cursorSchemaHasAnyProperty 报告 schema 是否声明了候选属性名中的任意一个。
func cursorSchemaHasAnyProperty(schemaJSON []byte, candidates []string) bool {
	var schema map[string]any
	if err := json.Unmarshal(schemaJSON, &schema); err != nil {
		return false
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return false
	}
	return cursorFirstSchemaProp(props, candidates) != ""
}

// ---------------------------------------------------------------------------
// 委托捕获（duplex 侧）
// ---------------------------------------------------------------------------

// cursorNativeFsExecField 返回 ExecServerMessage 内承载原生文件工具入参的字段号
// （f3 write / f4 delete / f5 grep / f7 read / f8 ls）；五种都不存在 → false。
func cursorNativeFsExecField(parts []cursorPBField) (int, bool) {
	for _, n := range []int{
		cursorFldServerMsgReadArgs,
		cursorFldServerMsgLsArgs,
		cursorFldServerMsgGrepArgs,
		cursorFldServerMsgWriteArgs,
		cursorFldServerMsgDeleteArgs,
	} {
		if cursorFindFieldOK(parts, n) {
			return n, true
		}
	}
	return 0, false
}

// cursorNativeFsExecFieldOK 判断 ExecServerMessage 是否承载原生文件工具入参
// （f3/f4/f5/f7/f8 任一存在）。
func cursorNativeFsExecFieldOK(parts []cursorPBField) bool {
	_, ok := cursorNativeFsExecField(parts)
	return ok
}

// cursorNativeFsKindFromField 把承载原生文件工具入参的字段号映射回委托 Kind
// （read/ls/grep/write/delete）。非受支持字段号 → 空串（不应用于拒绝分支）。
func cursorNativeFsKindFromField(field int) string {
	switch field {
	case cursorFldServerMsgReadArgs:
		return cursorToolKindRead
	case cursorFldServerMsgLsArgs:
		return cursorToolKindLs
	case cursorFldServerMsgGrepArgs:
		return cursorToolKindGrep
	case cursorFldServerMsgWriteArgs:
		return cursorToolKindWrite
	case cursorFldServerMsgDeleteArgs:
		return cursorToolKindDelete
	}
	return ""
}

// cursorCaptureNativeFsArgs 是 ExecServerMessage.f3/f4/f5/f7/f8 的统一捕获入口。
//
// 流程（与 cursorCaptureShellArgs 同构）：解 Args → 关键参数为空失败关闭 → 按声明 schema
// 选工具并转参数（找不到失败关闭）→ 构造 capture{Kind:<kind>} 走 noteNativeFsDelegation
// 接入与 shell 相同的"独立批立即可封口"登记（不等 started/listed）。
func (d *cursorAgentDuplex) cursorCaptureNativeFsArgs(parts []cursorPBField, field int, execID uint64, idField []byte) (cursorToolCallCapture, error) {
	f, ok := cursorFindField(parts, field)
	if !ok {
		return cursorToolCallCapture{}, fmt.Errorf("%w: native fs exec without f%d", ErrCursorProtocol, field)
	}
	sub, derr := cursorDecodeProtobuf(f.Bytes)
	if derr != nil {
		return cursorToolCallCapture{}, derr
	}

	var kind, callID, path string
	var params []cursorNativeFsParam
	switch field {
	case cursorFldServerMsgReadArgs:
		a := cursorParseReadArgs(sub)
		kind, callID, path = cursorToolKindRead, a.ToolCallID, a.Path
		params = append(params, cursorNativeFsParam{candidates: cursorNativeFsPathProps, value: a.Path})
		if a.Offset > 0 {
			params = append(params, cursorNativeFsParam{candidates: cursorNativeFsOffsetProps, value: a.Offset})
		}
		if a.Limit > 0 {
			params = append(params, cursorNativeFsParam{candidates: cursorNativeFsLimitProps, value: a.Limit})
		}
	case cursorFldServerMsgLsArgs:
		a := cursorParseLsArgs(sub)
		kind, callID, path = cursorToolKindLs, a.ToolCallID, a.Path
		params = append(params, cursorNativeFsParam{candidates: cursorNativeFsPathProps, value: a.Path})
		if len(a.Ignore) > 0 {
			params = append(params, cursorNativeFsParam{candidates: cursorNativeFsIgnoreProps, value: a.Ignore})
		}
	case cursorFldServerMsgGrepArgs:
		a := cursorParseGrepArgs(sub)
		// 客户端 empty-pattern fallback 语义：仅 glob（无 pattern）的检索以 "." 重新执行同一 grep
		// （安装包 main.js 的 empty-pattern-fallback）。只有 output_mode 与 glob 共同决定这是一次
		// "按 glob 找文件"的检索，此时把委托给下游的 pattern 置 "." 而非失败关闭；不改 a.OutputMode / a.Glob 透传。
		if a.Pattern == "" && strings.TrimSpace(a.Glob) != "" {
			a.Pattern = "."
		}
		kind, callID, path = cursorToolKindGrep, a.ToolCallID, a.Pattern
		params = append(params, cursorNativeFsParam{candidates: cursorNativeFsPatternProps, value: a.Pattern})
		if a.Path != "" {
			params = append(params, cursorNativeFsParam{candidates: cursorNativeFsPathProps, value: a.Path})
		}
		if a.Glob != "" {
			params = append(params, cursorNativeFsParam{candidates: cursorNativeFsGlobProps, value: a.Glob})
		}
		if a.OutputMode != "" {
			params = append(params, cursorNativeFsParam{candidates: cursorNativeFsOutputProps, value: a.OutputMode})
		}
		if a.ContextBefore > 0 {
			params = append(params, cursorNativeFsParam{candidates: cursorNativeFsCtxBefore, value: a.ContextBefore})
		}
		if a.ContextAfter > 0 {
			params = append(params, cursorNativeFsParam{candidates: cursorNativeFsCtxAfter, value: a.ContextAfter})
		}
		if a.Context > 0 {
			params = append(params, cursorNativeFsParam{candidates: cursorNativeFsCtxProps, value: a.Context})
		}
		if a.CaseInsensitive {
			params = append(params, cursorNativeFsParam{candidates: cursorNativeFsCaseProps, value: true})
		}
		if a.Type != "" {
			params = append(params, cursorNativeFsParam{candidates: cursorNativeFsTypeProps, value: a.Type})
		}
		if a.HeadLimit > 0 {
			params = append(params, cursorNativeFsParam{candidates: cursorNativeFsHeadProps, value: a.HeadLimit})
		}
		if a.Multiline {
			params = append(params, cursorNativeFsParam{candidates: cursorNativeFsMultiline, value: true})
		}
		if a.Sort != "" {
			params = append(params, cursorNativeFsParam{candidates: cursorNativeFsSortProps, value: a.Sort})
		}
		if a.SortAscending {
			params = append(params, cursorNativeFsParam{candidates: cursorNativeFsSortAscProps, value: true})
		}
		if a.Offset > 0 {
			params = append(params, cursorNativeFsParam{candidates: cursorNativeFsOffsetProps, value: a.Offset})
		}
	case cursorFldServerMsgWriteArgs:
		a := cursorParseWriteArgs(sub)
		kind, callID, path = cursorToolKindWrite, a.ToolCallID, a.Path
		params = append(params, cursorNativeFsParam{candidates: cursorNativeFsPathProps, value: a.Path})
		params = append(params, cursorNativeFsParam{candidates: cursorNativeFsContentProps, value: a.FileText})
	case cursorFldServerMsgDeleteArgs:
		a := cursorParseDeleteArgs(sub)
		kind, callID, path = cursorToolKindDelete, a.ToolCallID, a.Path
		params = append(params, cursorNativeFsParam{candidates: cursorNativeFsPathProps, value: a.Path})
	default:
		return cursorToolCallCapture{}, fmt.Errorf("%w: unsupported native fs exec field f%d", ErrCursorProtocol, field)
	}

	// 关键参数为空（含纯空白）→ 失败关闭（shell 的 command 空同语义）：不发起无路径/无模式的下游调用。
	if strings.TrimSpace(path) == "" {
		return cursorToolCallCapture{}, fmt.Errorf("%w: %s args key field empty", ErrCursorProtocol, kind)
	}

	name, argsJSON, resultArgsJSON, berr := cursorBuildNativeFsDelegation(d.declaredToolDefs, kind, params)
	if berr != nil {
		// 工具缺席（专用哨兵）：不失败关闭、不发委托，改回一条失败 Result 帧让流继续。
		// 其余错误（schema 解析失败 / required 无法填充 / unknown kind）仍按协议错误失败关闭。
		if errors.Is(berr, ErrCursorNativeFsToolAbsent) {
			// 外审 B1：该 call_id 可能已在本批 started 里，登记"本地已拒绝"让封口谓词跳过它。
			if d.delegations != nil {
				d.delegations.noteRejectedCall(callID)
			}
			return d.cursorRejectNativeFsNoTool(execID, idField, kind, params)
		}
		return cursorToolCallCapture{}, berr
	}
	tc := cursorToolCallCapture{
		ID:             callID,
		Name:           name,
		ArgsJSON:       argsJSON,
		ExecID:         execID,
		IDField:        append([]byte(nil), idField...),
		Kind:           kind,
		ResultArgsJSON: resultArgsJSON,
	}
	// NSFS-02 观测：走了 shell 回退（resultArgsJSON 非空）时额外记一条日志（只记命令长度，
	// 不记命令正文——命令里可能含文件内容）。
	if resultArgsJSON != "" {
		d.logPhase(zap.InfoLevel, "native_fs_shell_fallback", "",
			zap.String("kind", kind), zap.String("tool_name", name),
			zap.String("call_id", callID), zap.Int("cmd_len", len(argsJSON)))
	}
	delegations := d.delegations
	if delegations == nil {
		delegations = newLocalCursorToolDelegations()
		d.delegations = delegations
	}
	if err := delegations.noteNativeFsDelegation(kind, callID, tc); err != nil {
		return cursorToolCallCapture{}, err
	}
	return tc, nil
}

// cursorDeclaredToolNameMaxLen 是 declared_tool_names 单条工具名截断长度（防超长名撑爆日志）。
const cursorDeclaredToolNameMaxLen = 64

// cursorDeclaredToolNameMaxCount 是 declared_tool_names 字段的工具名总数上限（防声明集过大撑爆日志）。
const cursorDeclaredToolNameMaxCount = 64

// cursorToolDefName 从单条 McpToolDefinition 原始字节解出工具名：优先 f5 tool_name，回落 f1 name。
// 只取名字，**不**读取 description / input_schema / 参数值等任何内容。解码失败或两者皆空返回空串。
func cursorToolDefName(raw []byte) string {
	fields, err := cursorDecodeProtobuf(raw)
	if err != nil {
		return ""
	}
	var f1, f5 string
	for _, f := range fields {
		switch f.Number {
		case cursorFldMcpToolDefName:
			f1 = f.String()
		case cursorFldMcpToolDefToolName:
			f5 = f.String()
		}
	}
	if f5 != "" {
		return f5
	}
	return f1
}

// declaredToolNames 从本请求 f4 声明集（declaredToolDefs）解析出全部工具名（有序、去重）。
//
// 只读、无副作用、不缓存；每条声明集只取名字（f5 回落 f1），**不**读取 description / schema /
// 参数值。单条名超 cursorDeclaredToolNameMaxLen 截断，总数不超过 cursorDeclaredToolNameMaxCount
// （超出丢弃，避免日志膨胀）。供 NSFS-01 只读观测：定位生产"客户端到底声明了哪些工具名"这一未决疑问。
func (d *cursorAgentDuplex) declaredToolNames() []string {
	if len(d.declaredToolDefs) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(d.declaredToolDefs))
	out := make([]string, 0, len(d.declaredToolDefs))
	for _, raw := range d.declaredToolDefs {
		name := cursorToolDefName(raw)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		if r := []rune(name); len(r) > cursorDeclaredToolNameMaxLen {
			name = string(r[:cursorDeclaredToolNameMaxLen])
		}
		out = append(out, name)
		if len(out) >= cursorDeclaredToolNameMaxCount {
			break
		}
	}
	return out
}

// cursorRejectNativeFsNoTool 处理"客户端未声明该 kind 对应工具"这一专用哨兵：
// 不发委托、不建批，直接写出该 kind 对应的失败 Result 帧（error 文本固定
// "client has no <kind> tool declared"），并补 stream_close 让 exec 流正常终止，
// 避免整请求 502。返回空 capture 与 nil 错误（工具缺席不是失败关闭，连接继续）。
//
// cap 携带 ExecID / IDField / Kind / ArgsJSON：ArgsJSON 用已解析参数序列化（无属性时给 "{}"），
// 仅供失败帧回填路径/模式。不登记任何委托，故续轮不会有 orphan 回写。
func (d *cursorAgentDuplex) cursorRejectNativeFsNoTool(execID uint64, idField []byte, kind string, params []cursorNativeFsParam) (cursorToolCallCapture, error) {
	// NSFS-01 只读观测：把本请求声明的全部工具名（有序、去重、截断封顶）随拒绝帧一并记录，
	// 供定位生产"客户端到底声明了哪些工具名"这一未决疑问。只读观测，绝不改变返回值 / 失败帧 /
	// 封口判定，也绝不记录 description / schema / 参数值。
	declared := d.declaredToolNames()
	d.logPhase(zap.InfoLevel, "native_fs_reject_declared_tool_names", "",
		zap.Uint64("exec_id", execID),
		zap.String("kind", kind),
		zap.String("declared_tool_names", strings.Join(declared, ",")),
		zap.Int("declared_tool_name_count", len(declared)))

	argsJSON := cursorNativeFsParamsToJSON(params)
	text := fmt.Sprintf("client has no %s tool declared", kind)

	var field int
	var result []byte
	switch kind {
	case cursorToolKindRead:
		field, result = cursorFldClientMsgReadResult, cursorEncodeReadResult(argsJSON, text, true)
	case cursorToolKindWrite:
		field, result = cursorFldClientMsgWriteResult, cursorEncodeWriteResult(argsJSON, text, true)
	case cursorToolKindDelete:
		field, result = cursorFldClientMsgDeleteResult, cursorEncodeDeleteResult(argsJSON, text, true)
	case cursorToolKindLs:
		field, result = cursorFldClientMsgLsResult, cursorEncodeLsResult(argsJSON, text, true)
	case cursorToolKindGrep:
		field, result = cursorFldClientMsgGrepResult, cursorEncodeGrepResult(argsJSON, text, true)
	default:
		// 不应到达：kind 必来自受支持的五个字段号之一。
		return cursorToolCallCapture{}, fmt.Errorf("%w: cannot reject unknown native fs kind %q", ErrCursorProtocol, kind)
	}
	frame := cursorEncodeExecClientResultFrame(execID, idField, field, result)
	if serr := d.send(cursorFldClientMessage, frame); serr != nil {
		return cursorToolCallCapture{}, serr
	}
	if serr := d.sendExecStreamClose(execID); serr != nil {
		return cursorToolCallCapture{}, serr
	}
	return cursorToolCallCapture{}, nil
}

// cursorNativeFsParamsToJSON 把已解析的原生参数序列化为对象 JSON（按候选属性名取第一个）。
// 无参数（例如 grep 的 pattern 缺失已被上游失败关闭，正常不会为空）时回退 "{}"。
func cursorNativeFsParamsToJSON(params []cursorNativeFsParam) string {
	obj := map[string]any{}
	for _, p := range params {
		if len(p.candidates) == 0 {
			continue
		}
		obj[p.candidates[0]] = p.value
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// 结果回写编码
// ---------------------------------------------------------------------------

// cursorEncodeExecClientResultFrame 把一段 result 载荷包成完整 ExecClientMessage：
// {f1 id(exec_id, varint), f15 id(原值, bytes), <field> result}。
func cursorEncodeExecClientResultFrame(execID uint64, idField []byte, field int, result []byte) []byte {
	msg := cursorAppendVarintField(nil, cursorFldClientMsgID, execID)
	if len(idField) > 0 {
		msg = cursorAppendBytesField(msg, cursorFldClientMsgExecID, idField)
	}
	return cursorAppendBytesField(msg, field, result)
}

// cursorNativeFsResultText 取客户端返回的文本（多条文本按顺序拼接；无文本时用结构化 JSON）。
func cursorNativeFsResultText(r CursorToolResult) string {
	text := strings.Join(r.Text, "")
	if text == "" && len(r.Structured) > 0 {
		text = string(r.Structured)
	}
	return text
}

// cursorNativeFsResultFailed 判定客户端结果是否为失败（is_error 或 "ERROR:" 前缀）。
//
// 只做语义判定，不改写文本：失败一律走对应 Result 的 error 分支，绝不把失败写成成功。
func cursorNativeFsResultFailed(r CursorToolResult, text string) bool {
	return r.IsError || strings.HasPrefix(strings.TrimSpace(text), "ERROR:")
}

// cursorNativeFsArgString 从委托 ArgsJSON 取回某个属性值（按候选名顺序取第一个存在的）。
func cursorNativeFsArgString(argsJSON string, candidates ...string) string {
	if argsJSON == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &m); err != nil {
		return ""
	}
	for _, cand := range candidates {
		if v, ok := m[cand].(string); ok {
			return v
		}
	}
	return ""
}

// cursorCountLines 统计文本行数（空文本记 0 行）。
func cursorCountLines(text string) int {
	if text == "" {
		return 0
	}
	n := strings.Count(text, "\n")
	if !strings.HasSuffix(text, "\n") {
		n++
	}
	return n
}

// cursorEncodeNativeFsResult 按 cap.Kind 编码原生文件工具的结果帧（每 Kind 一帧）。
//
// 未知 Kind → 返回错误（调用方按失败关闭处理，不写零帧冒充成功）。
func cursorEncodeNativeFsResult(cap cursorToolCallCapture, r CursorToolResult) ([][]byte, error) {
	field, result, err := cursorNativeFsResultPayload(cap, r)
	if err != nil {
		return nil, err
	}
	return [][]byte{cursorEncodeExecClientResultFrame(cap.ExecID, cap.IDField, field, result)}, nil
}

// cursorNativeFsResultArgs 返回回写原生结果帧时用于取 path/pattern/output_mode 的参数 JSON。
//
// NSFS-02：走了 shell 回退时 ArgsJSON 是 {"command":...}（不含 path/pattern），必须用
// ResultArgsJSON（原生参数 JSON）；走原生工具时 ResultArgsJSON 为空，沿用 ArgsJSON（字节不变）。
// 绝不在 shell 回退时改成回写 ShellResult——上游下发的是原生 exec，回写必须是同一 exec 的原生
// Result 帧，否则上游会一直等。
func cursorNativeFsResultArgs(cap cursorToolCallCapture) string {
	if cap.ResultArgsJSON != "" {
		return cap.ResultArgsJSON
	}
	return cap.ArgsJSON
}

// cursorNativeFsResultPayload 返回 (ExecClientMessage 字段号, result 载荷)。
func cursorNativeFsResultPayload(cap cursorToolCallCapture, r CursorToolResult) (int, []byte, error) {
	text := cursorNativeFsResultText(r)
	failed := cursorNativeFsResultFailed(r, text)
	args := cursorNativeFsResultArgs(cap)
	switch cap.Kind {
	case cursorToolKindRead:
		return cursorFldClientMsgReadResult, cursorEncodeReadResult(args, text, failed), nil
	case cursorToolKindWrite:
		return cursorFldClientMsgWriteResult, cursorEncodeWriteResult(args, text, failed), nil
	case cursorToolKindDelete:
		return cursorFldClientMsgDeleteResult, cursorEncodeDeleteResult(args, text, failed), nil
	case cursorToolKindLs:
		return cursorFldClientMsgLsResult, cursorEncodeLsResult(args, text, failed), nil
	case cursorToolKindGrep:
		return cursorFldClientMsgGrepResult, cursorEncodeGrepResult(args, text, failed), nil
	default:
		return 0, nil, fmt.Errorf("%w: unknown native fs delegation kind %q", ErrCursorProtocol, cap.Kind)
	}
}

// cursorReadLineNumberPrefixRe 匹配下游 Read 工具回包里的展示行号前缀：可选前导空白 + 数字 + "→"。
var cursorReadLineNumberPrefixRe = regexp.MustCompile(`^\s*[0-9]+\x{2192}`)

// cursorStripReadLineNumberPrefixes 剥掉客户端 Read 回包里的展示行号前缀，还原文件原文。
//
// CodeBuddy / Claude Code 的 Read 工具回的是**展示文本**（"   1→def add(a, b):"），而 Cursor 原生
// ReadSuccess.content 的语义是**文件原文**。原样透传会让模型以为文件里真有行号，写回时把行号
// 写进文件（真机验收已复现 calc.py 被污染）。
//
// 只剥 `^\s*\d+→` 形态的前缀，非匹配行原样保留（真实文件可能本来就有别的格式）。若整段
// **零命中**，说明客户端回的就是原文，直接原样返回（字节不变，绝不改动真实内容）——这是
// "宁可不改也不误伤" 的判据：没有任何展示行号的证据时不做任何猜测性改写。
func cursorStripReadLineNumberPrefixes(text string) string {
	if text == "" || !strings.Contains(text, "\u2192") {
		return text
	}
	lines := strings.SplitAfter(text, "\n")
	out := make([]string, 0, len(lines))
	hit := false
	for _, line := range lines {
		if loc := cursorReadLineNumberPrefixRe.FindStringIndex(line); loc != nil {
			hit = true
			line = line[loc[1]:]
		}
		out = append(out, line)
	}
	if !hit {
		return text
	}
	return strings.Join(out, "")
}

// cursorEncodeReadResult 编码 ReadResult{f1 success | f2 error}。
//
// 成功：f1 path（回填委托路径）、f2 content（客户端返回的正文，已剥展示行号）、f3 total_lines、
// f4 file_size（= 剥行号后正文的字节数）。失败：f2 error{f1 path, f2 error}。
func cursorEncodeReadResult(argsJSON, text string, failed bool) []byte {
	path := cursorNativeFsArgString(argsJSON, cursorNativeFsPathProps...)
	if failed {
		inner := cursorAppendStringField(nil, cursorFldReadErrorPath, path)
		inner = cursorAppendStringField(inner, cursorFldReadErrorError, text)
		return cursorAppendBytesField(nil, cursorFldReadResultError, inner)
	}
	content := cursorStripReadLineNumberPrefixes(text)
	success := cursorAppendStringField(nil, cursorFldReadSuccessPath, path)
	// content 是 oneof output 的成员：空内容也必须显式出现（proto3 oneof 的 presence 语义），
	// 否则客户端可能认为 ReadSuccess 没有任何 output 而解析失败。
	success = cursorAppendBytesField(success, cursorFldReadSuccessContent, []byte(content))
	success = cursorAppendVarintField(success, cursorFldReadSuccessTotalLines, uint64(cursorCountLines(content)))
	success = cursorAppendVarintField(success, cursorFldReadSuccessFileSize, uint64(len(content)))
	return cursorAppendBytesField(nil, cursorFldReadResultSuccess, success)
}

// cursorEncodeWriteResult 编码 WriteResult{f1 success | f5 error}。
//
// 成功：f1 path、f2 lines_created、f3 file_size —— 三者都以本次委托请求写入的正文为准
// （客户端已确认写入成功，行数/字节数是该正文的确定属性，不是猜测）。
// 失败：f5 error{f1 path, f2 error}。
func cursorEncodeWriteResult(argsJSON, text string, failed bool) []byte {
	path := cursorNativeFsArgString(argsJSON, cursorNativeFsPathProps...)
	content := cursorNativeFsArgString(argsJSON, cursorNativeFsContentProps...)
	if failed {
		inner := cursorAppendStringField(nil, cursorFldWriteErrorPath, path)
		inner = cursorAppendStringField(inner, cursorFldWriteErrorError, text)
		return cursorAppendBytesField(nil, cursorFldWriteResultError, inner)
	}
	success := cursorAppendStringField(nil, cursorFldWriteSuccessPath, path)
	success = cursorAppendVarintField(success, cursorFldWriteSuccessLinesCreate, uint64(cursorCountLines(content)))
	success = cursorAppendVarintField(success, cursorFldWriteSuccessFileSize, uint64(len(content)))
	return cursorAppendBytesField(nil, cursorFldWriteResultSuccess, success)
}

// cursorEncodeDeleteResult 编码 DeleteResult{f1 success | f7 error}。
//
// success 只编码 f1 path：dump 给出的 f2 deleted_file / f3 file_size / f4 prev_content 的
// 取值来源在客户端结果里没有对应物（客户端只回一句文本），填任何一个都是编造，故不填。
// 失败：f7 error{f1 path, f2 error}。
func cursorEncodeDeleteResult(argsJSON, text string, failed bool) []byte {
	path := cursorNativeFsArgString(argsJSON, cursorNativeFsPathProps...)
	if failed {
		inner := cursorAppendStringField(nil, cursorFldDeleteErrorPath, path)
		inner = cursorAppendStringField(inner, cursorFldDeleteErrorError, text)
		return cursorAppendBytesField(nil, cursorFldDeleteResultError, inner)
	}
	success := cursorAppendStringField(nil, cursorFldDeleteSuccessPath, path)
	return cursorAppendBytesField(nil, cursorFldDeleteResultSuccess, success)
}

// cursorEncodeLsResult 编码 LsResult{f1 success | f2 error}。
//
// 成功：success.f1 = 目录树根节点：
//
//	f1 abs_path、f2 children_dirs（每个都是同构 LsDirectoryTreeNode，仅填 f1 abs_path）、
//	f3 children_files（每个只填 f1 = 路径）、f4 children_were_processed=true、
//	f6 num_files。
//
// ls 的输出本身**就是路径清单**，故文本可以被结构完整表达，无需编造。
// 注意：dump 未给出 children_files 的元素消息定义，此处按"路径节点"最小形状 {f1 path}
// 编码（与 children_dirs 的 f1 abs_path 同语义）；该假设需 bundle 原件复核（见交付报告）。
func cursorEncodeLsResult(argsJSON, text string, failed bool) []byte {
	root := cursorNativeFsArgString(argsJSON, cursorNativeFsPathProps...)
	if failed {
		inner := cursorAppendStringField(nil, cursorFldLsErrorPath, root)
		inner = cursorAppendStringField(inner, cursorFldLsErrorError, text)
		return cursorAppendBytesField(nil, cursorFldLsResultError, inner)
	}
	dirs, files := cursorParseLsListing(text)
	node := cursorAppendStringField(nil, cursorFldLsNodeAbsPath, root)
	for _, dir := range dirs {
		child := cursorAppendStringField(nil, cursorFldLsNodeAbsPath, dir)
		node = cursorAppendBytesField(node, cursorFldLsNodeChildrenDirs, child)
	}
	for _, file := range files {
		child := cursorAppendStringField(nil, cursorFldLsFileNodePath, file)
		node = cursorAppendBytesField(node, cursorFldLsNodeChildrenFiles, child)
	}
	node = cursorAppendVarintField(node, cursorFldLsNodeChildrenProcessed, 1)
	node = cursorAppendVarintField(node, cursorFldLsNodeNumFiles, uint64(len(files)))
	success := cursorAppendBytesField(nil, cursorFldLsSuccessRoot, node)
	return cursorAppendBytesField(nil, cursorFldLsResultSuccess, success)
}

// cursorParseLsListing 把 ls 的文本输出拆成目录与文件两组（绝对/相对路径原样保留）。
//
// 规则：逐行 trim；跳过空行、"total N" 汇总行与 "." / ".."；以 "/" 结尾的行视为目录
// （去掉尾斜杠）；其余视为文件。文本为空 → 两组皆空（不伪造条目）。
func cursorParseLsListing(text string) (dirs, files []string) {
	for _, line := range strings.Split(text, "\n") {
		entry := strings.TrimSpace(line)
		if entry == "" || entry == "." || entry == ".." {
			continue
		}
		if strings.HasPrefix(entry, "total ") {
			continue
		}
		if strings.HasSuffix(entry, "/") {
			dir := strings.TrimRight(entry, "/")
			if dir != "" {
				dirs = append(dirs, dir)
			}
			continue
		}
		files = append(files, entry)
	}
	return dirs, files
}

// cursorEncodeGrepResult 编码 GrepResult{f1 success | f2 error}。
//
// 失败（is_error / "ERROR:" 前缀）：f2 error{f1 error} 原样回传客户端文本。
//
// 成功：f1 success：
//
//	f1 pattern      ← 委托 ArgsJSON 的 pattern
//	f2 path         ← 委托 ArgsJSON 的 path（可空，空则不写）
//	f3 output_mode  ← 委托 ArgsJSON 的 output_mode（可空，空则填 "content"）
//	f4 workspace_results：map 一条 entry，key = 委托 path（空则 "/"），
//	   value = GrepUnionResult{f3 content = GrepContentResult{...}}
//
// grep 的正文是逐行文本，可以完整映射进 GrepContentResult，故不再降级为 error。
func cursorEncodeGrepResult(argsJSON, text string, failed bool) []byte {
	if failed {
		inner := cursorAppendStringField(nil, cursorFldGrepErrorError, text)
		return cursorAppendBytesField(nil, cursorFldGrepResultError, inner)
	}
	pattern := cursorNativeFsArgString(argsJSON, cursorNativeFsPatternProps...)
	path := cursorNativeFsArgString(argsJSON, cursorNativeFsPathProps...)
	outputMode := cursorNativeFsArgString(argsJSON, cursorNativeFsOutputProps...)
	if outputMode == "" {
		outputMode = "content"
	}

	// 按 output_mode 选择 GrepUnionResult 的 oneof 分支（客户端按 mode 取不同字段）：
	//   files_with_matches → f2 files、count → f1 count、content/空/未知 → f3 content（逐字节行为不变）。
	var union []byte
	switch outputMode {
	case "files_with_matches":
		filesInner, drift := cursorEncodeGrepFilesResult(text)
		// 失败关闭（非兜底）：files_with_matches 形态里出现了非纯路径行（行内含非盘符冒号，
		// 典型是下游回了 content 形态文本）。一旦有漂移行，整段不可信，必须如实报错而非把
		// 格式漂移谎报成命中文件清单。
		if drift > 0 {
			errInner := cursorAppendStringField(nil, cursorFldGrepErrorError,
				"cursor: grep files output is not a plain path list (fail closed)")
			return cursorAppendBytesField(nil, cursorFldGrepResultError, errInner)
		}
		union = filesInner
	case "count":
		countInner, parsed, drift := cursorEncodeGrepCountResult(text)
		// 失败关闭（非兜底）：count 模式下游回了一段非空但**不是** <file>:<count> 形态的文本
		// （典型为工具能力不匹配，回了 content 形态）。此时若仍回 success，会把"格式漂移"
		// 谎报成"零命中"，下游据此以为什么都没匹配到——这是失败关闭问题，必须如实报错。
		// 判据分两路：
		//   - 漂移数 > 0：出现非 <file>:<count> 形态行（路径含非盘符冒号或右侧非纯数字）；
		//   - 漂移数 == 0 但解析条数为 0 且原文非空：整段都不是 <file>:<count> 形态
		//     （"非空"排除"真的零命中"这一合法 success 情形；一旦解析到 ≥1 条仍按现状回 success）。
		if drift > 0 {
			errInner := cursorAppendStringField(nil, cursorFldGrepErrorError,
				"cursor: grep count output is not <file>:<count> form (fail closed)")
			return cursorAppendBytesField(nil, cursorFldGrepResultError, errInner)
		}
		if parsed == 0 && strings.TrimSpace(text) != "" {
			errInner := cursorAppendStringField(nil, cursorFldGrepErrorError,
				"cursor: grep count output is not <file>:<count> form (fail closed)")
			return cursorAppendBytesField(nil, cursorFldGrepResultError, errInner)
		}
		union = countInner
	default:
		union = cursorAppendBytesField(nil, cursorFldGrepUnionContent, cursorParseGrepContent(text))
	}

	workspace := path
	if strings.TrimSpace(workspace) == "" {
		workspace = "/"
	}
	entry := cursorAppendStringField(nil, cursorFldGrepWorkspaceKey, workspace)
	entry = cursorAppendBytesField(entry, cursorFldGrepWorkspaceValue, union)

	success := cursorAppendStringField(nil, cursorFldGrepSuccessPattern, pattern)
	success = cursorAppendStringField(success, cursorFldGrepSuccessPath, path)
	success = cursorAppendStringField(success, cursorFldGrepSuccessOutputMode, outputMode)
	success = cursorAppendBytesField(success, cursorFldGrepSuccessWorkspaceResults, entry)
	return cursorAppendBytesField(nil, cursorFldGrepResultSuccess, success)
}

// cursorEncodeGrepFilesResult 编码 files_with_matches 形态下的 GrepUnionResult.f2 files：
// 文本逐行（TrimSpace 后去尾随 \r、跳过空行）作为 GrepFilesResult.f1 files（repeated），
// f2 total_files = 条数，f3 client_truncated / f4 ripgrep_truncated 均写 0（客户端未提供截断信息，
// 不臆造）。字段号复用 GrepUnionResult 的 cursorFldGrepUnionFiles 常量，GrepFilesResult 内层用字面量。
//
// 第二个返回值是**被判定为漂移的非空行数**（而非扫描总行数），专供上层失败关闭判定：
// 凡行内含非盘符冒号（cursorGrepColonsArePathOnly 为假，典型是下游回的是 content 形态文本而非
// 纯路径清单）即计入漂移数，且该行不写入 files。任一非空行被判为漂移 → 上层据此整段失败关闭，
// 绝不把格式漂移谎报为命中文件清单。该计数**不改变任何输出字节**——同步内 GrepFilesResult 的编码
// 与改动前逐字节一致。
func cursorEncodeGrepFilesResult(text string) ([]byte, int) {
	var inner []byte
	var total int
	drift := 0
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSuffix(strings.TrimSpace(raw), "\r")
		if line == "" {
			continue
		}
		// 非纯路径（行内含非盘符冒号）→ 计入漂移、不写入 files，由上层整段失败关闭。
		if !cursorGrepColonsArePathOnly(line) {
			drift++
			continue
		}
		inner = cursorAppendBytesField(inner, 1, []byte(line))
		total++
	}
	inner = cursorAppendVarintField(inner, 2, uint64(total))
	inner = cursorAppendVarintField(inner, 3, 0)
	inner = cursorAppendVarintField(inner, 4, 0)
	return cursorAppendBytesField(nil, cursorFldGrepUnionFiles, inner), drift
}

// cursorEncodeGrepCountResult 编码 count 形态下的 GrepUnionResult.f1 count：
// 逐行以**最后一个** ':' 切出 <file>:<count>，路径部分只允许盘符冒号（出现其它冒号即判为漂移
// 并失败关闭），右侧必须全为十进制数字（否则该行计入漂移数）；每条作为 GrepFileCount（f1 file /
// f2 count）写入 GrepCountResult.f1 counts（repeated），f2 total_files = 文件条数，f3 total_matches
// = 计数之和，f4/f5 截断位写 0。
//
// 返回值三个：(inner 字节, parsed 实际解析条目数, drift 被判为漂移的非空行数)，均为专供上层失败关闭
// 判定之用：parsed 为 0 且原文非空、或 drift > 0 时，说明整段并不是 <file>:<count> 形态（典型是下游
// 回了 content 形态），上层据此走失败帧而非把格式漂移谎报为零命中。两者都**不改变任何输出字节**——
// 同行内 GrepCountResult 的编码与改动前逐字节一致。
func cursorEncodeGrepCountResult(text string) ([]byte, int, int) {
	var inner []byte
	var totalFiles, totalMatches int64
	parsed := 0
	drift := 0
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSuffix(strings.TrimSpace(raw), "\r")
		if line == "" {
			continue
		}
		idx := strings.LastIndexByte(line, ':')
		if idx <= 0 {
			// 没有 ':' 或冒号在最前面：保持现状的跳过语义，既不计入漂移也不计数。
			continue
		}
		// 路径部分含非盘符冒号（如 "a.go:3:123" 的第二个冒号）或右侧非纯数字 → 判为漂移。
		if !cursorGrepColonsArePathOnly(line[:idx]) || !cursorAllDecimal(line[idx+1:]) {
			drift++
			continue
		}
		file, numStr := line[:idx], line[idx+1:]
		count, perr := strconv.ParseInt(numStr, 10, 64)
		if perr != nil {
			continue
		}
		var fc []byte
		fc = cursorAppendBytesField(fc, 1, []byte(file))
		fc = cursorAppendVarintField(fc, 2, uint64(count))
		inner = cursorAppendBytesField(inner, 1, fc)
		totalFiles++
		totalMatches += count
		parsed++
	}
	inner = cursorAppendVarintField(inner, 2, uint64(totalFiles))
	inner = cursorAppendVarintField(inner, 3, uint64(totalMatches))
	inner = cursorAppendVarintField(inner, 4, 0)
	inner = cursorAppendVarintField(inner, 5, 0)
	return cursorAppendBytesField(nil, cursorFldGrepUnionCount, inner), parsed, drift
}

// cursorAllDecimal 报告 s 是否全为十进制数字（空串返回 false）。用于 count 右侧计数合法性判断。
func cursorAllDecimal(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// cursorGrepColonsArePathOnly 报告 s 中的 ':' 是否只可能来自 Windows 盘符前缀（形如 "C:"）。
// 规则：没有 ':' → true；恰有一个 ':' 且位于下标 1、下标 0 为 ASCII 字母 → true；其余 → false。
// 供 grep 编码侧判断"这一行到底是不是纯路径"——路径里若出现非盘符冒号（例如 "a.go:3:hit" 的
// 行号/内容冒号），说明下游回的是 content 形态文本而不是纯路径清单，应当判为漂移并失败关闭。
func cursorGrepColonsArePathOnly(s string) bool {
	colon := -1
	count := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			if colon < 0 {
				colon = i
			}
			count++
		}
	}
	if count == 0 {
		return true
	}
	if count == 1 && colon == 1 {
		c := s[0]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
			return true
		}
	}
	return false
}

// cursorGrepContentMatch 是一条命中（或无法解析）行的解析结果。
type cursorGrepContentMatch struct {
	lineNumber int64
	content    string
}

// cursorGrepFileMatch 是同一文件的连续命中集合。
type cursorGrepFileMatch struct {
	file    string
	matches []cursorGrepContentMatch
}

// cursorParseGrepContent 把 grep 标准输出解析成 GrepContentResult 的编码字节。
//
// 行规则（按顺序，见 cursorParseGrepLine）：
//   - `<file>:<line_number>:<content>` → 行号 + 内容；
//   - `<file>:<content>`（冒号后不是行号）→ line_number=0；
//   - `<file>` 单独一行（files_with_matches 模式）→ 该文件，matches 为空；
//   - 其余无法解析的行**不丢弃**：整行原样作为 content、line_number=0。
//
// 分组：同 file 的连续行归同一 GrepFileMatch（文件再次出现时另起一条，保出现顺序）。
// total_lines = 总行数，total_matched_lines = 命中行数（纯文件行不计入）。
func cursorParseGrepContent(text string) []byte {
	var groups []cursorGrepFileMatch
	var totalLines, matchedLines int
	if text != "" {
		lines := strings.Split(text, "\n")
		// 末尾换行产生的空尾元素不算一行。
		if strings.HasSuffix(text, "\n") {
			lines = lines[:len(lines)-1]
		}
		for _, raw := range lines {
			totalLines++
			line := strings.TrimSuffix(raw, "\r")
			file, m, ok := cursorParseGrepLine(line)
			if !ok {
				// 无法解析：整行原样保留（不丢信息）。
				file, m = "", &cursorGrepContentMatch{lineNumber: 0, content: line}
			}
			if m != nil {
				matchedLines++
			}
			if n := len(groups); n > 0 && groups[n-1].file == file {
				if m != nil {
					groups[n-1].matches = append(groups[n-1].matches, *m)
				}
			} else {
				g := cursorGrepFileMatch{file: file}
				if m != nil {
					g.matches = []cursorGrepContentMatch{*m}
				}
				groups = append(groups, g)
			}
		}
	}

	result := make([]byte, 0, len(text)+16)
	for _, g := range groups {
		// file 用 bytes 字段显式写出（即便为空）：无法解析的行没有文件名，若省略该字段，
		// 客户端可能把该组当成"没有文件的兜底条目"而丢弃这些行。
		fm := cursorAppendBytesField(nil, cursorFldGrepFileMatchFile, []byte(g.file))
		for _, m := range g.matches {
			var cm []byte
			if m.lineNumber != 0 {
				cm = cursorAppendVarintField(cm, cursorFldGrepContentMatchLineNumber, uint64(m.lineNumber))
			}
			cm = cursorAppendStringField(cm, cursorFldGrepContentMatchContent, m.content)
			fm = cursorAppendBytesField(fm, cursorFldGrepFileMatchMatches, cm)
		}
		result = cursorAppendBytesField(result, cursorFldGrepContentMatches, fm)
	}
	if totalLines > 0 {
		result = cursorAppendVarintField(result, cursorFldGrepContentTotalLines, uint64(totalLines))
	}
	if matchedLines > 0 {
		result = cursorAppendVarintField(result, cursorFldGrepContentTotalMatchedLines, uint64(matchedLines))
	}
	return result
}

// cursorParseGrepLine 解析一行 grep 输出。
//
// 返回 (file, match, 是否识别为 grep 行)；match 为 nil 表示"纯文件行"
// （files_with_matches 形态），该文件不产生 match 条目。
func cursorParseGrepLine(line string) (string, *cursorGrepContentMatch, bool) {
	// `<file>:<line>:<content>` / `<file>:<content>`：以第一个 ':' 切出文件，再看剩余部分是
	// 否以 "<数字>:" 开头判定有无行号。
	if idx := strings.IndexByte(line, ':'); idx > 0 {
		file, rest := line[:idx], line[idx+1:]
		if num, content, ok := cursorSplitLineNumber(rest, ':'); ok {
			return file, &cursorGrepContentMatch{lineNumber: num, content: content}, true
		}
		return file, &cursorGrepContentMatch{lineNumber: 0, content: rest}, true
	}
	// 单独一行路径（files_with_matches 模式）：无任何空白字符即视为路径。
	if line != "" && !strings.ContainsAny(line, " \t") {
		return line, nil, true
	}
	return "", nil, false
}

// cursorSplitLineNumber 判定 s 是否以 "<数字><sep>" 开头（或整串就是数字），
// 是则返回行号与去掉该前缀后的正文。
func cursorSplitLineNumber(s string, sep byte) (int64, string, bool) {
	i := 0
	var n int64
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		n = n*10 + int64(s[i]-'0')
		i++
	}
	if i == 0 {
		return 0, "", false
	}
	if i == len(s) {
		return n, "", true
	}
	if s[i] != sep {
		return 0, "", false
	}
	return n, s[i+1:], true
}
