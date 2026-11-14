package service

// 本文件实现 **OpenAI Chat Completions 请求 → Cursor AgentRun 出站请求体**的转换
// （P1-A1 卡，方案 §15.2 / §16.1）。
//
// 出站 RPC：agent.v1.AgentService/Run（双向 Connect 流）。客户端侧每帧是
// agent.v1.AgentClientMessage，oneof 的 run_request 分支承载一次对话请求。
//
// 取证来源（**勿凭记忆改字段号**，全部 tapgpu 本机可回读）：
//   - /root/cursor-p1-evidence/32-agent-protocol-schemas.json：agent.v1 全量 schema
//     （AgentClientMessage / AgentRunRequest / UserMessage / ModelDetails /
//     ConversationStateStructure / PreFetchedBlob …）
//   - 33-agent-run-request.bin：单轮活体请求（149 字节，活体 200，回包 "OK"）
//   - 63-agent-run-request.bin：多轮历史活体请求（500 字节，活体 200，回包
//     "ORCHID" —— 证明第二轮确实消费了历史）
//   - 62-kv-value-shapes.json / 62-turn-blob-schemas.json：KV blob 的值形状，
//     且 blob id = sha256(value)（已用 63 号样本的两条 blob 逐字节复核一致）
//   - 40-extra-schemas.json / 41-turn-ended-decoded.json：TurnEnded usage 字段号
//
// 首版范围（沿用 E6 卡的用户裁定，**纯文本**）：
//   - 请求侧 `tools` / `tool_choice` **丢弃**（Cursor 无对等结构；AgentRun 侧唯一
//     形似的是 mcp_tools，自造映射风险高于收益）。
//   - 多模态：历史 blob 的 content 只有 text 一种落点，图片只在**当前轮 user**
//     有落点（selected_context）。协议没有落点的图**丢掉并记账**，绝不为一张图
//     把整轮打成 400（口径见 cursorSkipLedger）。
//   - 不新增任何非文本 action（工具/执行/多模态一律不发）。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// Cursor AgentRun 协议字段号（全部来自已取证 schema，**不得臆造**）。
const (
	// AgentClientMessage（32-agent-protocol-schemas.json: rPe）:
	// oneof message = 1 run_request | 2 exec_client_message | 3 kv_client_message |
	//                 4 conversation_action | 5 exec_client_control_message |
	//                 6 interaction_response | 7 client_heartbeat | 8 prewarm_request
	cursorFldAgentRunRequest = 1

	// AgentRunRequest（rjs）内：
	cursorFldRunConversationState = 1  // ConversationStateStructure
	cursorFldRunAction            = 2  // ConversationAction
	cursorFldRunModelDetails      = 3  // ModelDetails（旧；上游真实客户端 2026-10-26 抓包用 f9）
	cursorFldRunMcpTools          = 4  // McpTools（活体样本里是**空 message**，但仍出现）
	cursorFldRunConversationID    = 5  // string [opt]
	cursorFldRunPreFetchedBlobs   = 17 // repeated PreFetchedBlob（多轮历史的 KV 载荷）

	// ConversationStateStructure（Hm）内：
	cursorFldStateRootPromptBlobs = 1  // repeated bytes：**历史消息的 blob id**
	cursorFldStateMode            = 10 // enum ConversationMode

	// ConversationAction（x1）内：oneof action = 1 user_message_action | …
	cursorFldActionUserMessage = 1

	// UserMessageAction（PMe）内：
	cursorFldUserMsgActionMessage = 1 // UserMessage

	// UserMessage（I1）内：
	cursorFldUserMsgText            = 1 // string
	cursorFldUserMsgMessageID       = 2 // string
	cursorFldUserMsgMode            = 4 // enum ConversationMode
	cursorFldUserMsgSelectedContext = 3 // SelectedContext（当前轮图片落点）

	// SelectedContext（UserMessage.f3）与 SelectedImage 字段号（全部来自已取证
	// schema：agent.v1.SelectedContext.f1 = selected_images repeated SelectedImage；
	// SelectedImage.f8 = data bytes / f7 = mime_type）。
	cursorFldSelectedContextImages = 1 // repeated SelectedImage
	cursorFldSelectedImageData     = 8 // bytes（原始图片字节，由 data URI 解出）
	cursorFldSelectedImageMimeType = 7 // string

	// ModelDetails（ode）内：
	cursorFldModelDetailsID = 1 // string，**必须填 wire ID**（活体填展示名会被拒）

	// PreFetchedBlob（ijs）内：
	cursorFldBlobID    = 1 // bytes，32 字节 = sha256(value)
	cursorFldBlobValue = 2 // bytes
)

// Cursor AgentRun 工具声明（McpTools / McpToolDefinition）字段号。
//
// 字段号**全部来自派发单显式规格**（04/05/06 号 schema dump 在本环境缺失，
// 以派发单为准；缺失 name 或 f5 会导致上游显示 unnamed tool，五个字段都要写）。
const (
	cursorFldMcpToolsTool          = 1 // McpTools.f1 = repeated McpToolDefinition
	cursorFldMcpToolDefName        = 1 // McpToolDefinition.f1 = name
	cursorFldMcpToolDefDescription = 2 // McpToolDefinition.f2 = description
	cursorFldMcpToolDefProvider    = 4 // McpToolDefinition.f4 = provider_identifier
	cursorFldMcpToolDefToolName    = 5 // McpToolDefinition.f5 = tool_name
	// cursorFldMcpToolDefInputSchemaValue 是 McpToolDefinition.f3：input_schema 以
	// google.protobuf.Value 承载（AgentRun 真实出站同形）。新生产构造**只写 f3**，
	// 不再写 f6 字符串。
	cursorFldMcpToolDefInputSchemaValue = 3
	// cursorFldMcpToolDefInputSchemaLegacy 是旧字段 f6（input_schema_json 字符串）。
	// 仅供 cursorToolSchemaIndex 解析既有历史夹具（按 f6 编码的字节）时回落读取；
	// 新生产路径不写 f6。
	cursorFldMcpToolDefInputSchemaLegacy = 6
)

// cursorMcpProviderIdentifier 是本层声明 MCP 工具时固定的 provider 标识。
const cursorMcpProviderIdentifier = "sub2api"

// cursorConversationModeAgent 是 ConversationMode 的取值（= agent.v1.AgentMode）。
//
// 依据：Cursor 安装包 `workbench.desktop.main.js` 里 agent.v1.AgentMode 的完整枚举 dump：
//
//	no:0 AGENT_MODE_UNSPECIFIED / no:1 AGENT_MODE_AGENT / no:2 AGENT_MODE_ASK /
//	no:3 AGENT_MODE_PLAN / no:4 AGENT_MODE_DEBUG / no:5 AGENT_MODE_TRIAGE /
//	no:7 AGENT_MODE_MULTITASK / no:8 AGENT_MODE_CUSTOM
//
// ConversationStateStructure.f10 mode 与 UserMessage.f4 mode 都引用该枚举。
//
// 历史：早期按 33/63 号纯文本活体样本取值为 2（= ASK）。ASK 是只读问答模式，
// 上游会明确拒绝非只读工具（shell / 写文件），模型返回 "I'm in Ask mode, so I can't …"。
// 纯文本与只读函数工具在 ASK 下可用，所以当时样本看不出差异；一旦要跑真实编程任务
// （读文件→改文件→执行）就必然失败。故改为 1（AGENT），与工具执行语义一致。
const cursorConversationModeAgent = 1

// ErrCursorUnsupportedRequest 表示入站形状落在首版范围外（失败关闭，不静默降级）。
var ErrCursorUnsupportedRequest = errors.New("cursor: request shape not supported in v1 (fail closed)")

// cursorNewUUID 生成会话/消息标识。单测可替换以获得确定性字节。
var cursorNewUUID = func() string { return uuid.NewString() }

// cursorConvertRequest 把 OpenAI Chat Completions 请求转换为 AgentRun 出站请求体
// （**已含 envelope**，可直接作为 CursorUpstreamRequest.Body）。
//
// upstreamModel 必须是 **wire ID**（如 `grok-4.7`）——填展示名会被上游拒
// ERROR_BAD_MODEL_NAME；本层不做任何"猜测式"模型名修正。
func cursorConvertRequest(req *apicompat.ChatCompletionsRequest, upstreamModel string) ([]byte, error) {
	body, _, err := cursorBuildAgentRun(req, upstreamModel, nil)
	return body, err
}

// cursorConvertRequestWithContext 是**生产入口**：转换请求，并把本次丢弃的内容
// （见 cursorSkipLedger）落一条日志。
//
// 与 cursorConvertRequest 的区别只有记账：后者给探针/单测用（不需要日志，也不关心丢了多少）。
func cursorConvertRequestWithContext(ctx context.Context, req *apicompat.ChatCompletionsRequest, upstreamModel string) ([]byte, error) {
	skipped := cursorSkipLedger{}
	body, _, err := cursorBuildAgentRun(req, upstreamModel, skipped)
	if err != nil {
		return nil, err
	}
	skipped.log(ctx, "chat_messages_dropped", upstreamModel)
	// W7 观测缺口：记一条 wire_name_renamed，让线上能区分"修好了"和"客户跑了"——
	// 现有 native_exec_rejected 只记客户端原名与计数，看不出改名条数与原名↔wire 对照。
	// 改名规则的唯一来源仍是 cursorWireToolNameSet / cursorWireToolNameOf，本函数只负责
	// 把"本次实际声明并编码上线的工具"与它们的 wire name 对照算出来落盘，不另写一份规则。
	cursorLogWireNameRenamed(ctx, req, upstreamModel)
	return body, nil
}

// cursorPhaseWireNameRenamed 是"工具名重名隔离改名对照"的 phase 名（施工图 W7 要求新增的
// 三个日志点之一）。一次出站请求只记一条（即使多个工具被改名）。
const cursorPhaseWireNameRenamed = "wire_name_renamed"

// cursorLogWireNameRenamed 记一条 wire_name_renamed 观测日志（W7 缺口）。
//
// 改名规则的唯一来源是 cursorWireToolNameSet / cursorWireToolNameOf（铁律 2：规则只有一个
// 来源，不为日志另写一份改名规则）。本函数只复用那张 wire map 把"本次实际声明并编码进 f4
// 的工具"与它们的 wire name 对照算出来并落盘；一次请求一条。
//
// 字段（与施工图 §2.4 W7 一致，且只记工具名、不记 description / schema / 参数值）：
//   - renamed_count：原名 != wire name 的工具条数；
//   - renamed_tools：改名前后对照，格式 `原名→wire名` 用 `,` 连接，只列被改名的；
//   - total_count：本次实际声明并编码的工具总数（空 Function.Name 的工具不计入编码，也不计入）。
//
// renamed_count == 0 也要记一条（renamed_tools 为空串），以便用"日志存在"确认没有改名发生，
// 而不是靠日志缺失来猜。
//
// 落点说明：cursorBuildMcpTools 是纯函数、无 logger 句柄，且其签名被既有测试锁定（不可改）；
// 同样 cursorBuildAgentRun 的签名也被 cursor_image_input_test.go 等锁定。故日志放在持有 ctx
// 的生产入口 cursorConvertRequestWithContext（与 skipped.log 同处），用传入的 req.Tools 复用
// 同一份 wire map 规则算出对照——这是"选项 (b)：在调用方用 wireMap 算出对照并记日志"的落点。
func cursorLogWireNameRenamed(ctx context.Context, req *apicompat.ChatCompletionsRequest, model string) {
	if ctx == nil {
		ctx = context.Background()
	}
	// 复用与 cursorBuildAgentRun 完全相同的规则算 wire map，保证对照与实际上线字节一致。
	wireMap := cursorWireToolNameSet(req.Tools)
	declared := cursorResolveDeclaredTools(req)
	var renamed []string
	total := 0
	for _, t := range declared {
		if t.Function == nil || t.Function.Name == "" {
			continue
		}
		total++
		wireName := cursorWireToolNameOf(wireMap, t.Function.Name)
		if wireName != t.Function.Name {
			renamed = append(renamed, t.Function.Name+"→"+wireName)
		}
	}
	// 排序保证日志可对比、用例可稳定断言（对照顺序不依赖工具声明顺序，与 wire map 的
	// 排序键口径一致——顺序只影响日志文本，不影响计算结果）。
	sort.Strings(renamed)
	cursorPhaseLog(ctx, zapcore.InfoLevel, cursorPhaseWireNameRenamed, model,
		zap.Int("renamed_count", len(renamed)),
		zap.String("renamed_tools", strings.Join(renamed, ",")),
		zap.Int("total_count", total),
	)
}

// cursorBuildAgentRun 构造 AgentRun 请求体，并回传本次的 conversation_id。
//
// 回传 conversation_id 的目的：它是这一轮对话在上游的唯一标识，调用方可据此把
// 流内事件（含 P1-A1 的拒付哨兵）关联回具体请求。
//
// skipped 是丢弃账本（可为 nil）：本层所有「协议没有落点」的形状一律丢弃记账，
// 不再整轮 400——口径见 cursorSkipLedger 的注释。
func cursorBuildAgentRun(req *apicompat.ChatCompletionsRequest, upstreamModel string, skipped cursorSkipLedger) ([]byte, string, error) {
	if req == nil {
		return nil, "", newCursorRequestError(infraerrors.CursorExecInternalErrorCode, http.StatusInternalServerError, "nil request")
	}
	if strings.TrimSpace(upstreamModel) == "" {
		return nil, "", newCursorRequestError(infraerrors.CursorExecModelMappingInvalidCode, http.StatusInternalServerError, "empty upstream model")
	}

	// wire map（原名 → wire name）在「选子集用原名匹配」之前算好，供历史重放（上面这行
	// cursorConvertMessages）与下面的声明改名共用，保证同一份规则、回程（入站映射那张卡）
	// 才能对得齐。顺序绝不能倒（W4）：cursorResolveDeclaredTools 仍用原名匹配 tool_choice
	// 选子集，改名只在编码与历史重放时发生。
	wireMap := cursorWireToolNameSet(req.Tools)

	turns, err := cursorConvertMessages(req.Messages, wireMap, skipped)
	if err != nil {
		return nil, "", err
	}

	// 尾部的**空** assistant 是 prefill 最常见的形态：客户端先塞一条空的助手占位
	// （或把上一轮的空回复原样带回来）。它不含任何内容，丢掉本轮就能正常发出去。
	// 只削尾部、且保留至少一轮，所以不会把 [assistant, 空 assistant] 削成空对话。
	for len(turns) > 1 && turns[len(turns)-1].role == "assistant" && strings.TrimSpace(turns[len(turns)-1].text) == "" {
		skipped.note(cursorSkipEmptyAssistantTail)
		turns = turns[:len(turns)-1]
	}

	if len(turns) == 0 {
		return nil, "", newCursorRequestError(infraerrors.CursorReqNoContentCode, http.StatusBadRequest, "no convertible messages")
	}

	// AgentRun 的已取证入口只有 user_message_action：最后一轮必须是用户消息，
	// 否则上游无处接收"当前这一轮的提问"。
	//
	// **这是整条链路上唯一必须拒的入站形状**：以实质内容结尾的 assistant
	// （prefill / 续写请求）在我们这边的协议里没有落点。硬把它当用户话发出去，
	// 等于把"模型该续写的内容"当成"用户新下的指令"，语义彻底错乱——客户拿到的是
	// 答非所问，比一个明确的拒绝更难排查。
	//
	// 所以这里拒，但**拒要说清**：CUR-REQ-06 的对客文案直接写明"最后一条是助手消息、
	// 本站不支持从助手消息续写、请在末尾补一条用户消息"，不让客户去猜。
	last := turns[len(turns)-1]
	if last.role != "user" {
		return nil, "", newCursorRequestError(infraerrors.CursorReqPrefillCode, http.StatusBadRequest, "conversation must end with a user message")
	}
	history := turns[:len(turns)-1]

	conversationID := cursorNewUUID()
	messageID := cursorNewUUID()

	// 1) conversation_state：mode + 历史消息的 blob id（content 走 KV 侧信道）。
	//
	// 证据 63：conversation_state = {mode:2, root_prompt_messages_json:[blobId…]}，
	// 每个 blobId 32 字节 = sha256(blobValue)，blobValue 由 pre_fetched_blobs 携带。
	state := cursorAppendVarintField(nil, cursorFldStateMode, cursorConversationModeAgent)
	blobs, err := cursorHistoryBlobs(history, skipped)
	if err != nil {
		return nil, "", err
	}
	for _, b := range blobs {
		state = cursorAppendBytesField(state, cursorFldStateRootPromptBlobs, b.id)
	}

	// 2) action.user_message_action.user_message：当前这一轮的提问。
	userMessage := cursorAppendStringField(nil, cursorFldUserMsgText, last.text)
	userMessage = cursorAppendStringField(userMessage, cursorFldUserMsgMessageID, messageID)
	userMessage = cursorAppendVarintField(userMessage, cursorFldUserMsgMode, cursorConversationModeAgent)
	// 当前轮图片（若有）→ UserMessage.f3 selected_context（SelectedContext）。
	// 空 images 时不写 f3，保持纯文本路径出站字节逐字节不变。
	if len(last.images) > 0 {
		var selCtx []byte
		for _, img := range last.images {
			var selImg []byte
			selImg = cursorAppendBytesField(selImg, cursorFldSelectedImageData, img.data)
			selImg = cursorAppendStringField(selImg, cursorFldSelectedImageMimeType, img.mimeType)
			selCtx = cursorAppendBytesField(selCtx, cursorFldSelectedContextImages, selImg)
		}
		userMessage = cursorAppendBytesField(userMessage, cursorFldUserMsgSelectedContext, selCtx)
	}
	userMsgAction := cursorAppendBytesField(nil, cursorFldUserMsgActionMessage, userMessage)
	action := cursorAppendBytesField(nil, cursorFldActionUserMessage, userMsgAction)

	// 3) 其余字段按活体样本的**出现顺序**编码（33/63 均为
	//    conversation_state → action → model_details → mcp_tools →
	//    conversation_id → pre_fetched_blobs…）。
	run := cursorAppendBytesField(nil, cursorFldRunConversationState, state)
	run = cursorAppendBytesField(run, cursorFldRunAction, action)
	run = cursorAppendBytesField(run, cursorFldRunModelDetails,
		cursorAppendStringField(nil, cursorFldModelDetailsID, upstreamModel))
	// mcp_tools：请求携带 tools 且 tool_choice != "none" 时，把 function 工具编码进
	// f4（McpTools）声明给上游；否则保持空 message（f4 字段存在、长度 0，与活体样本
	// 同形——既满足"无 tools 时零变化"的纯文本链回归保护，也保住 33/63 号黄金字节）。
	declared := cursorResolveDeclaredTools(req)
	// W4 顺序护栏：声明子集仍用原名匹配（上面 cursorResolveDeclaredTools），wire map
	// 已在函数顶部算好，这里直接复用、只负责改名编码。
	mcpTools, err := cursorBuildMcpTools(declared, wireMap, skipped)
	if err != nil {
		return nil, "", err
	}
	run = cursorAppendBytesField(run, cursorFldRunMcpTools, mcpTools)
	run = cursorAppendStringField(run, cursorFldRunConversationID, conversationID)
	for _, b := range blobs {
		blob := cursorAppendBytesField(nil, cursorFldBlobID, b.id)
		blob = cursorAppendBytesField(blob, cursorFldBlobValue, b.value)
		run = cursorAppendBytesField(run, cursorFldRunPreFetchedBlobs, blob)
	}

	outer := cursorAppendBytesField(nil, cursorFldAgentRunRequest, run)
	return cursorEncodeEnvelope(outer), conversationID, nil
}

// cursorResolveDeclaredTools 按 §21.4-2 统一语义决定本轮要声明的工具子集。
//
// 返回 nil 表示"不发送工具声明"（等价无 tools）。规则：
//   - 无 tools            → nil
//   - tool_choice 缺省/"auto" → 全部 function 工具
//   - "none"             → nil（不声明，但历史里的 tool 消息仍按 §4 文本化）
//   - "required"         → 全部 function 工具（模型是否真调用按实际，不伪造 finish）
//   - {"type":"function","function":{"name":X}} → 仅声明 X；X 不在列表时回落全部
//     （不凭空声明不存在的工具，也不静默丢掉其它已声明工具）
//   - 其它无法识别的形状  → 回落全部声明（不失败关闭，纯文本链回归保护）
func cursorResolveDeclaredTools(req *apicompat.ChatCompletionsRequest) []apicompat.ChatTool {
	if len(req.Tools) == 0 {
		return nil
	}
	choice := strings.TrimSpace(string(req.ToolChoice))
	switch choice {
	case "", `"auto"`:
		return cursorFunctionTools(req.Tools)
	case `"none"`:
		return nil
	case `"required"`:
		return cursorFunctionTools(req.Tools)
	}
	// 指定函数：{"type":"function","function":{"name":"X"}}
	var tc struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(req.ToolChoice, &tc); err == nil && tc.Type == "function" && tc.Function.Name != "" {
		for _, t := range req.Tools {
			if t.Function != nil && t.Function.Name == tc.Function.Name {
				return []apicompat.ChatTool{t}
			}
		}
		// 指定函数不在声明列表：回落全部，避免凭空声明/丢工具。
		return cursorFunctionTools(req.Tools)
	}
	// 无法识别：回落全部声明，不失败关闭。
	return cursorFunctionTools(req.Tools)
}

// cursorFunctionTools 过滤出 type 为 function（或空 type）的工具。
func cursorFunctionTools(tools []apicompat.ChatTool) []apicompat.ChatTool {
	out := make([]apicompat.ChatTool, 0, len(tools))
	for _, t := range tools {
		if t.Type == "" || t.Type == "function" {
			out = append(out, t)
		}
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// 工具名重名隔离（出站侧 + 出站历史侧）。本卡只做这两个方向；入站映射（wire name →
// 原名）由另一张卡落在 cursor_agent_duplex.go，两卡必须用**同一份规则**算 wire name，
// 否则入站映射回来对不齐。**规则必须与另一张卡逐字一致**。
// ─────────────────────────────────────────────────────────────────────────────

// cursorBuiltinToolNames 是 Cursor 上游自带的内置工具名（归一化后比较）。
// 客户端工具名撞上这些名字 → 上游拒绝整个请求（社区实测），必须改名避开。
// 取值来自社区 cursor2api/session.py 的 BUILTIN_TOOLS：
//
//	read write ls delete grep glob shell web_search web_fetch
var cursorBuiltinToolNames = map[string]bool{
	"read": true, "write": true, "ls": true, "delete": true, "grep": true,
	"glob": true, "shell": true, "web_search": true, "web_fetch": true,
}

// 工具名归一化（camelCase → snake_case、'-'/'_' → '_'、全小写）的**唯一实现**
// 在 cursor_native_exec_reject.go 的 cursorNormaliseToolName。
//
// 本文件（改名路径）与那处（点名路径）必须用**同一份**归一化规则：
// 两者都在比较「工具名是否等于 Cursor 内置名/客户端等价名」，规则一旦分叉，
// 就会出现「改名时算撞名、点名时算不撞」这种对不上的情况。
// 社区同名函数是 _normalise（cursor2api/session.py:82）。

// cursorWireToolName 返回该工具名发给上游时用的 wire name（**纯函数**）。
//
// 规则（施工图 W3，与入站映射那张卡逐字一致）：
//
//	归一化后不撞内置名 → wire name == 原名（绝大多数工具走这条）；
//	撞内置名 → 原名后加 "_"，重复直到归一化后不撞内置名。
//
// 注意：本纯函数只处理「撞内置名」这一条；「与本批其它工具不重复」的去重（W3 规则 2
// 的第二段）依赖整批工具集合，由 cursorWireToolNameSet 负责。历史重放与声明改名都从
// 同一份 wire map 取数（cursorWireToolNameOf），保证回程对得齐。
func cursorWireToolName(name string) string {
	w := name
	for cursorBuiltinToolNames[cursorNormaliseToolName(w)] {
		w += "_"
	}
	return w
}

// cursorWireToolNameSet 计算一批工具名各自的 wire name（**选择 (b)：排序后去重**）。
//
// **W3 选择 (b)**：先把整批工具名按「原始字符串字节序升序」排序，再依序累加 taken 集合
// 去重，所以**最终每个工具名的 wire name 只依赖「工具名集合」、与声明顺序无关**。
// 历史重放时只要工具名集合相同，算出的 wire name 就与当初声明时一致，入站映射（落在
// cursor_agent_duplex.go 的另一张卡）用同一套排序键即可对齐。
//
// **排序键**：按工具名原始字符串的 Go 默认 < 比较（即字节序升序，非归一化、非大小写
// 折叠）。两卡都必须用这一排序键。
//
// 去重规则：依序处理，每个工具先取 cursorWireToolName(原名) 作为候选；若该候选的归一化
// 形态已在 taken（撞内置名或撞本批其它工具的 wire name），就继续在末尾加 "_" 直到归一化
// 形态既不在内置集、也不在 taken。taken 以**归一化形态**为键（大小写不敏感，与 Cursor
// 归一化比较口径一致），保证两个不同原名不会算出同一个 wire name。
//
// 入参是工具列表（按 Function.Name 过滤空名）；空集合返回空 map、不 panic。
func cursorWireToolNameSet(tools []apicompat.ChatTool) map[string]string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		if t.Function != nil && t.Function.Name != "" {
			names = append(names, t.Function.Name)
		}
	}
	return cursorWireToolNamesFromNames(names)
}

// cursorWireToolNamesFromNames 是 cursorWireToolNameSet 的纯名字重载（便于单测复用）。
func cursorWireToolNamesFromNames(names []string) map[string]string {
	out := make(map[string]string, len(names))
	if len(names) == 0 {
		return out
	}
	sorted := append([]string(nil), names...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	taken := map[string]bool{}
	for _, name := range sorted {
		w := cursorWireToolName(name)
		for {
			norm := cursorNormaliseToolName(w)
			if !cursorBuiltinToolNames[norm] && !taken[norm] {
				taken[norm] = true
				break
			}
			w += "_"
		}
		out[name] = w
	}
	return out
}

// cursorWireToolNameOf 按原名从 wire map 取 wire name（声明改名 / 历史重放统一入口）。
// wire map 缺失该原名（极端情况：声明时已无此工具）时回落到纯函数 cursorWireToolName，
// 保证任何情况下都能给出一个确定、可复算的 wire name。
func cursorWireToolNameOf(wireMap map[string]string, name string) string {
	if w, ok := wireMap[name]; ok {
		return w
	}
	return cursorWireToolName(name)
}

// cursorBuildMcpTools 把一组 function 工具编码为 AgentRunRequest.f4（McpTools）内容。
//
// 每个函数生成一条 McpToolDefinition（McpTools.f1 repeated），五个字段都要写：
// f1 name / f2 description / f4 provider_identifier="sub2api" / f5 tool_name /
// f3 input_schema（google.protobuf.Value，由 function.parameters 的 JSON 经
// structpb + proto.Marshal 编码，与 AgentRun 真实出站同形；schema 缺失补空 Struct）。
// 返回空切片表示"无声明"（f4 保持空 message，与活体样本同形）。
//
// **方向 1（改名）**：f1（name）与 f5（tool_name）写 **wire name**（cursorWireToolNameOf
// 取数），f2 description 与 f4 provider 不变。原名若撞 Cursor 内置名（如 Read/Write/Glob/
// Grep/Bash/WebFetch…），上游会拒绝整个请求（社区实测），必须改成不撞的 wire name 发出；
// 回程由另一张卡（cursor_agent_duplex.go）把 wire name 映射回原名。
//
// 非法 schema 的处置（2026-11-06 口径修正）：**丢掉这一个工具并记账**，不再整轮 400。
// 客户端的 schema 写错是它自己的 bug；但为一个坏 schema 把整轮打成"模型不稳定"，
// 是更坏的交换——少一个工具本轮还能答，400 则什么都答不了。
func cursorBuildMcpTools(tools []apicompat.ChatTool, wireMap map[string]string, skipped cursorSkipLedger) ([]byte, error) {
	var mcp []byte
	for _, t := range tools {
		if t.Function == nil || t.Function.Name == "" {
			continue
		}
		// 方向 1：f1/f5 用 wire name，避免撞 Cursor 内置名被整请求拒绝。
		wireName := cursorWireToolNameOf(wireMap, t.Function.Name)
		var def []byte
		def = cursorAppendStringField(def, cursorFldMcpToolDefName, wireName)
		def = cursorAppendStringField(def, cursorFldMcpToolDefDescription, t.Function.Description)
		def = cursorAppendStringField(def, cursorFldMcpToolDefProvider, cursorMcpProviderIdentifier)
		def = cursorAppendStringField(def, cursorFldMcpToolDefToolName, wireName)
		// custom/freeform 工具（如 codex 的 exec）：有降级壳 schema 时必须写进 f3。
		//
		// 2026-11-14 根因修复：原实现一律不写 f3，上游模型没有形状约束 →
		// 自行发明 {"code":"..."} 包装 → 回程抽不出裸文本 → codex 的 exec 报
		// SyntaxError（见 apicompat.responsesToolsToChatTools 同源注释）。
		// 仍保留"schema 为空则不写 f3"的分支：客户端真的没给 schema 时，
		// 硬写一个空 schema 反而会误导模型。
		if t.Custom {
			schema := ""
			if t.Function != nil {
				schema = string(t.Function.Parameters)
			}
			if strings.TrimSpace(schema) == "" || schema == "null" {
				mcp = cursorAppendBytesField(mcp, cursorFldMcpToolsTool, def)
				continue
			}
			value := &structpb.Value{}
			if err := protojson.Unmarshal([]byte(schema), value); err != nil {
				skipped.note(cursorSkipToolSchemaInvalid)
				continue
			}
			valBytes, err := proto.Marshal(value)
			if err != nil {
				skipped.note(cursorSkipToolSchemaInvalid)
				continue
			}
			def = cursorAppendBytesField(def, cursorFldMcpToolDefInputSchemaValue, valBytes)
			mcp = cursorAppendBytesField(mcp, cursorFldMcpToolsTool, def)
			continue
		}
		schema := string(t.Function.Parameters)
		if strings.TrimSpace(schema) == "" || schema == "null" {
			schema = "{}"
		}
		// schema 必须是合法 JSON 对象，才能编码进 google.protobuf.Value。
		value := &structpb.Value{}
		if err := protojson.Unmarshal([]byte(schema), value); err != nil {
			skipped.note(cursorSkipToolSchemaInvalid)
			continue
		}
		valBytes, err := proto.Marshal(value)
		if err != nil {
			skipped.note(cursorSkipToolSchemaInvalid)
			continue
		}
		def = cursorAppendBytesField(def, cursorFldMcpToolDefInputSchemaValue, valBytes)
		mcp = cursorAppendBytesField(mcp, cursorFldMcpToolsTool, def)
	}
	return mcp, nil
}

// cursorTurnImage 是一张已解出原始字节的图片（来自 content-part 的 data URI）。
type cursorTurnImage struct {
	data     []byte // 原始图片字节（由 data:<mime>;base64,<payload> 解出）
	mimeType string // 如 "image/png"
}

// cursorAgentTurn 是一条已归一化的对话轮次（角色 + 纯文本 + 可选图片）。
type cursorAgentTurn struct {
	// role 只有两种取值："user" / "assistant"（AgentRun 的历史 blob 只有这两种
	// role 落点，见 62-kv-value-shapes.json 的活体值形状）。
	role string
	text string
	// images 是当前轮携带的图片（user/assistant/tool 均可能带图）。图片落点：
	//   - 当前轮（最后一轮，必为 user 角色）→ UserMessage.selected_context；
	//   - 历史轮 user → history blob 的 content 数组（改动 D）。
	// 纯文本轮 images 为空；system/developer 含图则失败关闭（无落点）。
	images []cursorTurnImage
}

// cursorHistoryBlob 是一条历史消息的 KV 载荷（内容寻址）。
type cursorHistoryBlob struct {
	// id 是 value 的 sha256（32 字节）。证据 63 的两条 blob 已逐字节复核：
	// sha256(value) == root_prompt_messages_json 里的 blob id。
	id []byte
	// value 是历史消息的 JSON 序列化（形状见 cursorHistoryUserBlob /
	// cursorHistoryAssistantBlob）。
	value []byte
}

// cursorHistoryUserBlob 是用户侧历史消息的值形状（证据 62/63 活体样本）。
type cursorHistoryUserBlob struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// cursorHistoryAssistantBlob 是助手侧历史消息的值形状（证据 62/63 活体样本：
// `{"role":"assistant","content":[{"type":"text","text":"…"}],"id":"1"}`）。
type cursorHistoryAssistantBlob struct {
	Role    string                           `json:"role"`
	Content []cursorHistoryAssistantTextPart `json:"content"`
	ID      string                           `json:"id"`
}

type cursorHistoryAssistantTextPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// cursorHistoryUserRichBlob 是带图用户侧历史消息的值形状（按需，仅当该轮含图时
// 才用，保持纯文本路径的字节稳定——见 cursorHistoryBlobs）。
//
// content 改为数组：text 项（text 非空时）在前，其后按 images 顺序排 image 项，
// 与客户端 coreMessage 序列化方言同形（type 互斥，camelCase 的 mimeType）：
//
//	[{"type":"text","text":"…"},{"type":"image","data":"<base64>","mimeType":"image/png"}]
type cursorHistoryUserRichBlob struct {
	Role    string                      `json:"role"`
	Content []cursorHistoryUserRichPart `json:"content"`
}

// cursorHistoryUserRichPart 是 rich blob content 数组里的一项：用 type 字段区分
// text / image（与客户端核心 schema 及既有 assistant blob 同方言）。text 项携带
// Text，image 项携带 Data（base64）+ MimeType（camelCase，不是 mime_type）。
type cursorHistoryUserRichPart struct {
	Type     string `json:"type"`               // "text" | "image"
	Text     string `json:"text,omitempty"`     // type=text
	Data     string `json:"data,omitempty"`     // type=image（base64）
	MimeType string `json:"mimeType,omitempty"` // type=image（camelCase，不是 mime_type）
}

// cursorHistoryUserRichBlobOf 把带图 user turn 组装成 rich blob：先写（文本非空时的）
// text 项，再按序写 image 项。结构与客户端 coreMessage 序列化方言一致
// （type 互斥，mimeType 用 camelCase）。
func cursorHistoryUserRichBlobOf(turn cursorAgentTurn) cursorHistoryUserRichBlob {
	blob := cursorHistoryUserRichBlob{Role: "user"}
	if turn.text != "" {
		blob.Content = append(blob.Content, cursorHistoryUserRichPart{
			Type: "text",
			Text: turn.text,
		})
	}
	for _, img := range turn.images {
		blob.Content = append(blob.Content, cursorHistoryUserRichPart{
			Type:     "image",
			Data:     base64.StdEncoding.EncodeToString(img.data),
			MimeType: img.mimeType,
		})
	}
	return blob
}

// cursorHistoryBlobs 把历史轮次序列化为内容寻址的 KV 载荷（保序）。
func cursorHistoryBlobs(history []cursorAgentTurn, skipped cursorSkipLedger) ([]cursorHistoryBlob, error) {
	out := make([]cursorHistoryBlob, 0, len(history))
	assistantSeq := 0
	for _, turn := range history {
		var value []byte
		var err error
		if turn.role == "assistant" {
			// assistant 历史侧无 image 落点（agent.v1.ConversationHistoryAssistantContent
			// 仅 text / reasoning / redacted_reasoning / tool_call）。
			//
			// 2026-11-06 口径修正：带图时**丢图保文本**并记账，不再整轮 400。
			// "协议没有图片落点"是我们这一侧的限制，不该让客户为此收到一个错误——
			// 图丢了只是少一点上下文，400 是整轮白跑。
			if len(turn.images) > 0 {
				skipped.note(cursorSkipImageOutsideUser)
				turn.images = nil
			}
			assistantSeq++
			value, err = cursorMarshalBlobJSON(cursorHistoryAssistantBlob{
				Role:    "assistant",
				Content: []cursorHistoryAssistantTextPart{{Type: "text", Text: turn.text}},
				// id 取**助手消息的序号**（活体样本里首条 assistant 的 id 为 "1"）。
				ID: strconv.Itoa(assistantSeq),
			})
		} else if len(turn.images) > 0 {
			// 带图 user 轮 → rich blob（content 数组），纯文本路径永不到这。
			value, err = cursorMarshalBlobJSON(cursorHistoryUserRichBlobOf(turn))
		} else {
			value, err = cursorMarshalBlobJSON(cursorHistoryUserBlob{Role: "user", Content: turn.text})
		}
		if err != nil {
			return nil, newCursorRequestError(infraerrors.CursorExecInternalErrorCode, http.StatusInternalServerError, "marshal history blob: %v", err)
		}
		sum := sha256.Sum256(value)
		out = append(out, cursorHistoryBlob{id: sum[:], value: value})
	}
	return out, nil
}

// cursorMarshalBlobJSON 序列化历史 blob 值，**关闭 HTML 转义**（< > & 保持字面量）。
//
// 协议 blob 是 Cursor 会 JSON 解析的 bytes 载荷；§4 续轮文本化的
// <tool_call>/<tool_result> 标记必须以字面量出现（而非 \u003c…），与派发单边界一致；
// 两侧 JSON 解析功能等价，但钉字节测试要求字面量，且避免把标记变成不可读的转义序列。
func cursorMarshalBlobJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	if len(out) > 0 && out[len(out)-1] == '\n' {
		out = out[:len(out)-1]
	}
	return out, nil
}

// cursorConvertMessages 把 OpenAI messages 归一化成有序轮次。
//
// **system 消息的处理（沿用 E6 卡的用户裁定：合并进首个 user）**——依据：
//  1. AgentRun 的历史 blob 与 UserMessage 都只有 user / assistant 两种落点，
//     **无 system 角色**（32 号 schema：ConversationHistory 只有
//     user / assistant / tool 三种消息）。
//  2. 丢弃会**静默改变语义**：绝大多数客户端把 system 当硬约束，丢了会得到完全不同
//     的回答，且用户无从察觉——比"格式略不理想"危险得多。
//
// 因此：system 文本按出现顺序拼到**第一条 user** 消息的开头，标记行分隔；
// 若整段对话没有任何 user（纯 system+assistant），则新建一条 user 承载
// （否则请求会被上游判为空对话）。
func cursorConvertMessages(msgs []apicompat.ChatMessage, wireMap map[string]string, skipped cursorSkipLedger) ([]cursorAgentTurn, error) {
	turns := make([]cursorAgentTurn, 0, len(msgs))
	var systemParts []string
	sawUser := false

	for _, m := range msgs {
		text, images, err := cursorMessageContent(m, skipped)
		if err != nil {
			// 内容层已改成「解析不了的只丢这一条」，正常路径不再有可预期错误；
			// 返回位留给真正的内部失败（不吞，避免把 bug 伪装成"丢了一条"）。
			return nil, err
		}
		switch strings.ToLower(strings.TrimSpace(m.Role)) {
		case "system", "developer":
			// system/developer 无图片落点 → 丢图保文本并记账（2026-11-06 口径）。
			if len(images) > 0 {
				skipped.note(cursorSkipImageOutsideUser)
				images = nil
			}
			if text != "" {
				systemParts = append(systemParts, text)
			}
		case "user", "human":
			// 首条 user 承载此前累积的 system（时序最自然：约束在最前）。
			if !sawUser && len(systemParts) > 0 {
				text = cursorJoinSystem(systemParts) + text
				systemParts = nil
			}
			turns = append(turns, cursorAgentTurn{role: "user", text: text, images: images})
			sawUser = true
		case "assistant", "ai":
			if m.FunctionCall != nil {
				// legacy function_call：旧版 OpenAI 的 deprecated 字段，Cursor 协议里
				// 没有对应落点。2026-11-06 口径：**丢掉这个字段、保留本条文本**并记账，
				// 不整轮 400（存量客户端早已不用它，还在发的多是从别的格式转过来的）。
				skipped.note(cursorSkipLegacyFunctionCall)
			}
			// 图片落点仅在 user 侧：assistant 历史 blob 没有任何 image 字段
			// （agent.v1.ConversationHistoryAssistantContent 只含
			// text / reasoning / redacted_reasoning / tool_call）→ 丢图保文本。
			if len(images) > 0 {
				skipped.note(cursorSkipImageOutsideUser)
				images = nil
			}
			// assistant 可能携带 tool_calls：原 content（可空）+ 每块一个
			// <tool_call> blob（见 §4 续轮文本化）。
			turns = append(turns, cursorAgentTurn{role: "assistant", text: cursorAssistantText(m, text, wireMap), images: images})
		case "tool", "function":
			// 工具结果 → user turn（Cursor 历史仅 user/assistant 两种落点，见 §4）。
			// 文本化失败关闭（含非文本 part 时与多模态口径一致）；图片挂到该
			// user turn 上（若有，走 history rich blob）。
			tt, tImages, err := cursorToolResultText(m, wireMap, skipped)
			if err != nil {
				return nil, err
			}
			turns = append(turns, cursorAgentTurn{role: "user", text: tt, images: tImages})
		case "":
			// 没有角色的消息：丢这一条，不整轮拒（2026-11-06 产品口径：
			// 客户把 400 当成"模型不稳定"，宁可少一条也不能整轮失败）。
			skipped.note(cursorSkipEmptyRole)
			continue
		default:
			// 未知角色（critic / narrator 之类客户端自定义）：按 user 处理会把
			// assistant 语义搞错，按 assistant 处理又会篡改轮次；因此**丢掉这一条**
			// 继续本轮，而不是把整个请求 400 掉。
			skipped.note(cursorSkipUnknownRole)
			continue
		}
	}

	// 收尾：system 出现在最后（如 [user, assistant, system]）时无处可合并。
	// 此时新建一条 user 承载——保证 system 文本不丢，且不篡改既有轮次顺序。
	if len(systemParts) > 0 {
		turns = append(turns, cursorAgentTurn{role: "user", text: cursorJoinSystem(systemParts)})
	}

	// 丢弃可能把某条消息变成"空轮"（文本空 + 无图）：发上去等于一个空提问，
	// 上游要么回空要么报错。它们不含任何信息，剔掉只会让请求更干净——记账即可。
	kept := turns[:0]
	for _, turn := range turns {
		if turn.role == "user" && strings.TrimSpace(turn.text) == "" && len(turn.images) == 0 {
			skipped.note(cursorSkipEmptyMessage)
			continue
		}
		kept = append(kept, turn)
	}
	return kept, nil
}

// cursorJoinSystem 把多条 system 文本拼成一条，保留顺序并加显式边界。
//
// 分隔行用 `<system>` 包裹每一段：Cursor 无 system 角色，若不加标记，多段指令的
// 边界会丢失，模型可能把后一段当用户的新问题。
func cursorJoinSystem(parts []string) string {
	if len(parts) == 1 {
		return parts[0]
	}
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("<system>\n")
		b.WriteString(p)
		b.WriteString("\n</system>")
	}
	return b.String()
}

// cursorAssistantText 把 assistant 消息（含可选 tool_calls）拼成一段文本。
//
// 续轮文本化（§4）：原 content（可空）+ 每个 tool_call 一块
// `\n<tool_call>\n{"id":...,"name":...,"arguments":{...}}\n</tool_call>`，
// arguments 保留原 JSON 字符串（空则补 "{}"）。
//
// **方向 2（历史重放改名）**：tool_call 的 "name" 必须写 **wire name**（cursorWireToolNameOf
// 取数），与当初出站声明一致——否则上游会看到一个它从未声明过的工具名，回程映射对不齐。
// 工具名不撞内置名时 wire name == 原名，行为不变。
func cursorAssistantText(m apicompat.ChatMessage, base string, wireMap map[string]string) string {
	text := base
	if len(m.ToolCalls) == 0 {
		return text
	}
	var b strings.Builder
	if text != "" {
		b.WriteString(text)
	}
	for _, tc := range m.ToolCalls {
		args := tc.Function.Arguments
		if strings.TrimSpace(args) == "" || args == "null" {
			args = "{}"
		}
		b.WriteString("\n" + cursorInlineToolCallOpen + "\n")
		b.WriteString(`{"id":` + cursorJSONString(tc.ID) +
			`,"name":` + cursorJSONString(cursorWireToolNameOf(wireMap, tc.Function.Name)) +
			`,"arguments":` + args + `}`)
		b.WriteString("\n" + cursorInlineToolCallClose)
	}
	return b.String()
}

// cursorToolResultText 把 tool / function 角色消息文本化为一条 user turn。
//
// 续轮文本化（§4）：Cursor 历史仅 user/assistant 两种落点，工具结果无处可去，
// 故包成 `<tool_result tool_call_id="..." name="...">\n<content>\n...\n</tool_result>`
// 落为 user。content 为字符串或纯 text part 拼接；image_url（data URI）解出图片后
// **丢弃并记账**（tool result 无图片落点，见下），远程 URL / file / 未知 part
// 在内容层已按「丢这个 part」处理，不再上抛。
//
// **方向 2（历史重放改名）**：name 属性必须写 **wire name**（cursorWireToolNameOf 取数），
// 与当初出站声明一致——否则上游会看到一个它从未声明过的工具名，回程映射对不齐。
func cursorToolResultText(m apicompat.ChatMessage, wireMap map[string]string, skipped cursorSkipLedger) (string, []cursorTurnImage, error) {
	content, images, err := cursorMessageContent(m, skipped)
	if err != nil {
		return "", nil, err
	}
	if len(images) > 0 {
		// tool 结果无图片落点：协议里 tool result 不能承载图片，丢图保文本
		// （2026-11-06 口径：图丢了只是少一点上下文，整轮 400 是白跑）。
		skipped.note(cursorSkipImageOutsideUser)
		images = nil
	}
	id := m.ToolCallID
	name := m.Name
	if strings.EqualFold(strings.TrimSpace(m.Role), "function") && strings.TrimSpace(id) == "" {
		// legacy function 消息没有 tool_call_id：无法与本轮工具调用对上号。
		// 2026-11-06 口径：不拒，落成一条**不带 id 的结果文本**（模型仍能读到内容），
		// 记账即可——拒掉整轮换来的只是"客户看到 400"。
		skipped.note(cursorSkipLegacyFunctionNoID)
	}
	if name == "" && m.FunctionCall != nil {
		name = m.FunctionCall.Name
	}
	// 方向 2：name 用 wire name（不撞内置名时 == 原名，行为不变）。
	wireName := cursorWireToolNameOf(wireMap, name)
	var b strings.Builder
	b.WriteString("<tool_result tool_call_id=")
	b.WriteString(cursorJSONString(id))
	b.WriteString(" name=")
	b.WriteString(cursorJSONString(wireName))
	b.WriteString(">\n<content>\n")
	b.WriteString(content)
	b.WriteString("\n</tool_result>")
	return b.String(), images, nil
}

// cursorMessageContent 从一条 OpenAI 消息取（文本 + 图片）。
//
// 口径（2026-11-06 修正，替代原「失败关闭」）：**能丢就丢，丢了记账**。
//
//   - content 是字符串 → text 原样，images 空；
//   - content 是 parts 数组 → 逐 part 处理：
//     type=="text" 拼接；type=="image_url" 解 data URI（解不出就丢这张图）；
//     其它 type（file / 音频 / 未知）→ 丢这个 part，保留文本；
//   - content 是数字 / 布尔 / 对象 / 坏 JSON → 本条消息按「无内容」处理并记账
//     （调用方随后会把空轮剔掉，整轮真没东西才拒）。
//
// 唯一不上抛的例外是内部编程错误，不在这里。
func cursorMessageContent(m apicompat.ChatMessage, skipped cursorSkipLedger) (string, []cursorTurnImage, error) {
	raw := strings.TrimSpace(string(m.Content))
	if raw == "" || raw == "null" {
		// 兼容 content 缺省与显式 null；tool_calls 型消息正文为空是合法的。
		return "", nil, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			skipped.note(cursorSkipBadContent)
			return "", nil, nil
		}
		return s, nil, nil
	}
	if raw[0] == '[' {
		var parts []apicompat.ChatContentPart
		if err := json.Unmarshal([]byte(raw), &parts); err != nil {
			skipped.note(cursorSkipBadContent)
			return "", nil, nil
		}
		var b strings.Builder
		images := make([]cursorTurnImage, 0, len(parts))
		for _, p := range parts {
			switch p.Type {
			case "text":
				// 空白 text 跳过：它不含信息，拼进去只会多出无意义换行。
				if strings.TrimSpace(p.Text) == "" {
					continue
				}
				b.WriteString(p.Text)
			case "image_url":
				if p.ImageURL == nil {
					skipped.note(cursorSkipImageInvalid)
					continue
				}
				img, ok := cursorDecodeImageURL(p.ImageURL.URL, skipped)
				if !ok {
					continue
				}
				images = append(images, img)
			default:
				// file / 音频 / 未知 part：本协议没有落点 → 丢这个 part 保文本。
				skipped.note(cursorSkipUnsupportedPart)
				continue
			}
		}
		return b.String(), images, nil
	}
	// 数字 / 布尔 / 对象等：Cursor 侧只接受 string → 本条按「无内容」处理。
	skipped.note(cursorSkipBadContent)
	return "", nil, nil
}

// cursorDecodeImageURL 解析 image_url 的 URL 为 cursorTurnImage。
//
// 仅接受 data URI（`data:<mime>;base64,<payload>`，或带额外参数如
// `data:image/svg+xml;charset=utf-8;base64,<payload>`）。护栏：
//   - MIME 必须 image/ 前缀（防 data:text/html 等透传）；
//   - base64 解码前剔除空白，依次尝试 StdEncoding → URLEncoding（URL-safe 字母表）；
//   - 解码后字节数超过既有请求体上限（cursorUpstreamStreamMaxBytes）。
//
// **所有不合规分支一律「丢这张图」并记账**（2026-11-06 口径）：本层不服务端抓取
// 远程图片、也不接受非 base64，但"这张图用不了"不等于"整轮不能答"——
// 丢掉图、保留文字，客户至少还能得到回答；返回 400 只会让客户觉得模型坏了。
// ok=false 表示这张图被丢弃（调用方继续处理同一消息的其它部分）。
func cursorDecodeImageURL(u string, skipped cursorSkipLedger) (cursorTurnImage, bool) {
	const prefix = "data:"
	if !strings.HasPrefix(u, prefix) {
		// 远程 URL（http/https 等）：本层不抓取。丢弃时**不把 URL 写进任何输出**
		// （签名参数 / 凭据会泄露），只记账。
		skipped.note(cursorSkipImageInvalid)
		return cursorTurnImage{}, false
	}
	rest := u[len(prefix):]
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		skipped.note(cursorSkipImageInvalid)
		return cursorTurnImage{}, false
	}
	meta := rest[:comma]
	payload := rest[comma+1:]

	// 按 ';' 拆参数，遍历找 base64 标记；其余参数（如 charset）仅作为 mime 说明。
	// 例：data:image/svg+xml;charset=utf-8;base64,… 必须能过。
	params := strings.Split(meta, ";")
	mime := strings.TrimSpace(params[0])
	isBase64 := false
	for _, p := range params[1:] {
		if strings.EqualFold(strings.TrimSpace(p), "base64") {
			isBase64 = true
		}
	}
	if !isBase64 {
		skipped.note(cursorSkipImageInvalid)
		return cursorTurnImage{}, false
	}
	if mime == "" {
		skipped.note(cursorSkipImageInvalid)
		return cursorTurnImage{}, false
	}
	// MIME 必须是 image/ 前缀（否则 data:text/html 等会被透传，危险）。
	if !strings.HasPrefix(mime, "image/") {
		skipped.note(cursorSkipImageInvalid)
		return cursorTurnImage{}, false
	}

	// base64 健壮性：解码前剔除空白字符（换行/空格/制表/回车），依次尝试
	// StdEncoding → URLEncoding（-/_ 字母表）。
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r':
			return -1
		}
		return r
	}, payload)
	data, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		data, err = base64.URLEncoding.DecodeString(cleaned)
	}
	if err != nil {
		skipped.note(cursorSkipImageInvalid)
		return cursorTurnImage{}, false
	}
	if len(data) == 0 {
		skipped.note(cursorSkipImageInvalid)
		return cursorTurnImage{}, false
	}
	// 体积护栏：解码后字节数超过既有请求体上限 → 丢弃这张图（复用既有常量，不新增阈值）。
	if len(data) > cursorUpstreamStreamMaxBytes {
		skipped.note(cursorSkipImageInvalid)
		return cursorTurnImage{}, false
	}
	return cursorTurnImage{data: data, mimeType: mime}, true
}

// cursorSafeImageRefForLog 把图片引用安全地回显到日志/错误信息，避免泄露敏感内容。
//
//   - 远程 URL（http/https 等）→ 走 cursorSafeURLForLog（只留 host+path 并截断）；
//   - data URI → 只回显 "data:<mime>,<N> bytes"（N = 逗号后 payload 的字符数），
//     绝不把整段 base64 / 内联 SVG 写进日志。cursorSafeURLForLog 对 data URI 解析不出
//     host 会原样返回，故这里单独分支处理。
func cursorSafeImageRefForLog(u string) string {
	const prefix = "data:"
	if strings.HasPrefix(u, prefix) {
		rest := u[len(prefix):]
		if comma := strings.IndexByte(rest, ','); comma >= 0 {
			return fmt.Sprintf("data:%s,%d bytes", rest[:comma], len(rest[comma+1:]))
		}
		return "data:?"
	}
	return cursorSafeURLForLog(u)
}

// cursorSafeURLForLog 只取 URL 的 host+path（去掉 scheme、userinfo、query、fragment），
// 并做长度截断，避免把签名参数、凭据等敏感信息写进日志/错误信息。
//
// 实现改用 net/url 解析（不再手工切字符串）：userinfo / query / fragment 由解析器负责，
// 保留 port（属于 host 语义）。无法解析出 host 的输入原样返回（无 host 语义可提取）。
// 非法 UTF-8 字节会被丢弃，因此输出长度 ≤ 输入长度，不做字节偏移对齐。
func cursorSafeURLForLog(u string) string {
	// 规则 1：能解析出 host 的输入（http/https/ftp 等带 scheme 的合法 URL），
	// 直接取 host + path；userinfo / query / fragment 已被解析器剥离，port 保留。
	if parsed, err := url.Parse(u); err == nil && parsed.Host != "" {
		return cursorSafeURLTruncate(parsed.Host + parsed.Path)
	}
	// 规则 2：无 scheme 形态（如 "//host/path"、"user:pass@host"）→ 补上 "//"
	// 让解析器按 authority 处理，剥离 userinfo 后取 host + path。
	if parsed, err := url.Parse("//" + strings.TrimPrefix(u, "//")); err == nil && parsed.Host != "" {
		return cursorSafeURLTruncate(parsed.Host + parsed.Path)
	}
	// 规则 3：解析不出 host（data URI、空 host 的 "https:///a@b"、普通文本等）
	// 原样返回——无 host 语义可提取，也不应 panic。
	return cursorSafeURLTruncate(u)
}

// cursorSafeURLTruncate 统一把结果清洗为合法 UTF-8，并按 ≤120 字节截断、回退到合法
// UTF-8 边界，绝不切断多字节字符，也绝不把非法字节写进日志/错误串。
func cursorSafeURLTruncate(result string) string {
	const maxLen = 120
	result = strings.ToValidUTF8(result, "")
	if len(result) > maxLen {
		result = result[:maxLen]
		for !utf8.ValidString(result) {
			result = result[:len(result)-1]
		}
	}
	return result
}
