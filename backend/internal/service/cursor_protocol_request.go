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
// 当前范围（按第三方实际收发对齐）：
//   - 请求侧 `tools` / `tool_choice` 决定声明子集：每个真实工具一条原生
//     McpToolDefinition（原名 / 原 description / 原 schema 文本），不再造单入口包装。
//   - 历史按真实结构表达：assistant 的 tool_calls 写 tool-call 内容项，tool 消息写
//     tool-result 内容项；普通文本与图片沿用既有落点。
//   - 图片只在**当前轮 user** 有落点（selected_context）；历史带图 user 走 rich blob，
//     assistant/tool 无图片落点则**丢掉并记账**，绝不为一张图把整轮打成 400。

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
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/google/uuid"
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
	cursorFldStateTurns           = 8  // repeated bytes：native resume 的 turn blob id（SDK 1.5.0）

	// ConversationAction（x1）内：oneof action = 1 user_message_action | 2 resume_action | …
	cursorFldActionUserMessage  = 1
	cursorFldActionResumeAction = 2 // resume_action：零长 message，必须写出（不可省略）

	// ConversationTurnStructure.f1 → AgentConversationTurnStructure.f1 → UserMessage blob id
	// （native resume 的真实 user 锚点，SDK 1.5.0）。
	cursorFldTurnAgentConversationTurn = 1
	cursorFldAgentTurnUserMessageBlob  = 1

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
	cursorFldMcpToolsTool              = 1 // McpTools.f1 = repeated McpToolDefinition
	cursorFldMcpToolDefName            = 1 // McpToolDefinition.f1 = name
	cursorFldMcpToolDefDescription     = 2 // McpToolDefinition.f2 = description
	cursorFldMcpToolDefProvider        = 4 // McpToolDefinition.f4 = provider_identifier
	cursorFldMcpToolDefToolName        = 5 // McpToolDefinition.f5 = tool_name
	cursorFldMcpToolDefInputSchemaJSON = 6 // McpToolDefinition.f6 = input_schema_json 字符串
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
// 本函数是薄解码适配器：只把 Chat 消息解码成语义消息，随后交给唯一的语义 builder。
//
// upstreamModel 必须是 **wire ID**（如 `grok-4.7`）——填展示名会被上游拒
// ERROR_BAD_MODEL_NAME；本层不做任何"猜测式"模型名修正。
func cursorConvertRequest(req *apicompat.ChatCompletionsRequest, upstreamModel string) ([]byte, error) {
	sem, err := cursorDecodeRequestSemantic(req)
	if err != nil {
		return nil, err
	}
	body, _, err := cursorBuildAgentRunFromSemantic(sem, upstreamModel, req.Tools, req.ToolChoice, nil)
	return body, err
}

// cursorConvertRequestWithContext 是**生产入口**：转换请求，并把本次丢弃的内容
// （见 cursorSkipLedger）落一条日志。
//
// 与 cursorConvertRequest 的区别只有记账：后者给探针/单测用（不需要日志，也不关心丢了多少）。
func cursorConvertRequestWithContext(ctx context.Context, req *apicompat.ChatCompletionsRequest, upstreamModel string) ([]byte, error) {
	sem, err := cursorDecodeRequestSemantic(req)
	if err != nil {
		return nil, err
	}
	skipped := cursorSkipLedger{}
	body, _, err := cursorBuildAgentRunFromSemantic(sem, upstreamModel, req.Tools, req.ToolChoice, skipped)
	if err != nil {
		return nil, err
	}
	skipped.log(ctx, "chat_messages_dropped", upstreamModel)
	return body, nil
}

// cursorDecodeRequestSemantic 是 Chat 请求的解码薄入口：只做 nil 校验与语义解码，
// 不在这里做角色归类或容器猜测（归类统一在 cursorClassifySemanticRole）。
func cursorDecodeRequestSemantic(req *apicompat.ChatCompletionsRequest) ([]cursorSemanticMessage, error) {
	if req == nil {
		return nil, newCursorRequestError(http.StatusInternalServerError, infraerrors.CursorExecInternalError, "nil request")
	}
	return cursorDecodeSemanticMessages(req.Messages), nil
}

// cursorBuildAgentRun 是**旧 Chat 测试入口**：只做解码并调用唯一的语义 builder。
// 保留签名只为兼容既有调用点，本函数内不含任何 AgentRun 编码逻辑。
func cursorBuildAgentRun(req *apicompat.ChatCompletionsRequest, upstreamModel string, skipped cursorSkipLedger) ([]byte, string, error) {
	sem, err := cursorDecodeRequestSemantic(req)
	if err != nil {
		return nil, "", err
	}
	return cursorBuildAgentRunFromSemantic(sem, upstreamModel, req.Tools, req.ToolChoice, skipped)
}

// cursorBuildAgentRunFromSemantic 是**唯一的** AgentRun 语义 builder。
//
// 它只消费 []cursorSemanticMessage，不再直接吃 Chat 消息：Chat 解码入口与日后
// 的复合源协议解码器都把各自的消息喂给同一个 builder，不存在第二套编码实现。
//
// 回传 conversation_id 的目的：它是这一轮对话在上游的唯一标识，调用方可据此把
// 流内事件（含 P1-A1 的拒付哨兵）关联回具体请求。
//
// skipped 是丢弃账本（可为 nil）：本层所有「协议没有落点」的形状一律丢弃记账，
// 不再整轮 400——口径见 cursorSkipLedger 的注释。
func cursorBuildAgentRunFromSemantic(sem []cursorSemanticMessage, upstreamModel string, tools []apicompat.ChatTool, toolChoice json.RawMessage, skipped cursorSkipLedger) ([]byte, string, error) {
	if strings.TrimSpace(upstreamModel) == "" {
		return nil, "", newCursorRequestError(http.StatusInternalServerError, infraerrors.CursorExecModelMappingInvalid, "empty upstream model")
	}

	turns, err := cursorSemanticMessagesToTurns(sem, skipped)
	if err != nil {
		return nil, "", err
	}

	// 尾部的**空** assistant 是 prefill 最常见的形态：客户端先塞一条空的助手占位
	// （或把上一轮的空回复原样带回来）。它不含任何内容，丢掉本轮就能正常发出去。
	// 只削尾部、且保留至少一轮，所以不会把 [assistant, 空 assistant] 削成空对话。
	// 仅含工具调用（text 空）的 assistant 必须保留：工具调用是结构化内容，不是空占位。
	for len(turns) > 1 && turns[len(turns)-1].role == "assistant" &&
		strings.TrimSpace(turns[len(turns)-1].text) == "" && len(turns[len(turns)-1].toolCalls) == 0 {
		skipped.note(cursorSkipEmptyAssistantTail)
		turns = turns[:len(turns)-1]
	}

	if len(turns) == 0 {
		return nil, "", newCursorRequestError(http.StatusBadRequest, infraerrors.CursorReqNoContent, "no convertible messages")
	}

	// 尾部形状由唯一的语义 analyzer 判定（与执行服务的续轮计划共用同一判据）。
	// user 尾走原有黄金字节路径；完整历史的 tool 结果尾走 native resume；
	// 真 assistant 续写仍按 CUR-REQ-07 拒。
	last := turns[len(turns)-1]
	history := turns[:len(turns)-1]
	resume := last.role == "tool"
	if resume {
		last, err = cursorRebuildUserAnchor(turns, sem)
		if err != nil {
			return nil, "", err
		}
		// Completed results remain in the history; the anchor is not a new question.
		history = turns
	} else if last.role != "user" {
		// 以实质内容结尾的 assistant（prefill / 续写）没有协议落点：硬发出去等于把
		// "模型该续写的内容"当成"用户新指令"。CUR-REQ-07 文案说清如何修正。
		return nil, "", newCursorRequestError(http.StatusBadRequest, infraerrors.CursorReqPrefill, "conversation must end with a user message")
	}

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
	userMessage := cursorEncodeUserMessage(last.text, messageID, last.images)
	userMsgAction := cursorAppendBytesField(nil, cursorFldUserMsgActionMessage, userMessage)
	action := cursorAppendBytesField(nil, cursorFldActionUserMessage, userMsgAction)
	if resume {
		userID := cursorContentAddress(userMessage)
		agentTurn := cursorAppendBytesField(nil, cursorFldAgentTurnUserMessageBlob, userID)
		turnValue := cursorAppendBytesField(nil, cursorFldTurnAgentConversationTurn, agentTurn)
		turnID := cursorContentAddress(turnValue)
		blobs = append(blobs,
			cursorHistoryBlob{id: userID, value: userMessage},
			cursorHistoryBlob{id: turnID, value: turnValue})
		state = cursorAppendBytesField(state, cursorFldStateTurns, turnID)
		// A present, zero-length message selects the native resume oneof.
		action = cursorAppendBytesField(nil, cursorFldActionResumeAction, nil)
	}

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
	declared := cursorResolveDeclaredTools(&apicompat.ChatCompletionsRequest{Tools: tools, ToolChoice: toolChoice})
	mcpTools, err := cursorBuildMcpTools(declared, skipped)
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

// cursorEncodeUserMessage encodes the current-turn UserMessage wire bytes:
// text / message id / mode / optional selected_context images. Shared by the
// normal user tail and the native-resume user anchor so both use one encoding.
func cursorEncodeUserMessage(text, messageID string, images []cursorTurnImage) []byte {
	userMessage := cursorAppendStringField(nil, cursorFldUserMsgText, text)
	userMessage = cursorAppendStringField(userMessage, cursorFldUserMsgMessageID, messageID)
	userMessage = cursorAppendVarintField(userMessage, cursorFldUserMsgMode, cursorConversationModeAgent)
	// Empty images must not write f3: keep the plain-text outbound bytes unchanged.
	if len(images) > 0 {
		var selCtx []byte
		for _, img := range images {
			var selImg []byte
			selImg = cursorAppendBytesField(selImg, cursorFldSelectedImageData, img.data)
			selImg = cursorAppendStringField(selImg, cursorFldSelectedImageMimeType, img.mimeType)
			selCtx = cursorAppendBytesField(selCtx, cursorFldSelectedContextImages, selImg)
		}
		userMessage = cursorAppendBytesField(userMessage, cursorFldUserMsgSelectedContext, selCtx)
	}
	return userMessage
}

// cursorContentAddress returns the 32-byte sha256 of a blob value.
func cursorContentAddress(value []byte) []byte {
	sum := sha256.Sum256(value)
	return sum[:]
}

// cursorRebuildUserAnchor checks the existing full-history criterion and finds
// the original user for a native resume. It neither encodes a second AgentRun
// nor converts completed tool results into new user instructions.
func cursorRebuildUserAnchor(turns []cursorAgentTurn, sem []cursorSemanticMessage) (cursorAgentTurn, error) {
	reject := func(detail string) (cursorAgentTurn, error) {
		return cursorAgentTurn{}, newCursorRequestError(http.StatusBadRequest,
			infraerrors.CursorResUnknownAssociation, "%s", detail)
	}
	analysis := cursorAnalyzeSemanticMessages(sem)
	if len(analysis.results) == 0 {
		return reject("tool result tail carries no analyzable results")
	}
	// Call IDs of assistant tool_calls in the history prefix. Tail containers are
	// excluded so a result can never prove its own call.
	historyCallIDs := make(map[string]struct{})
	for _, m := range sem {
		if _, inTail := analysis.tailContainers[m.sourceContainerIndex]; inTail {
			continue
		}
		if m.role != cursorSemanticAssistant {
			continue
		}
		for _, call := range m.raw.ToolCalls {
			if id := strings.TrimSpace(call.ID); id != "" {
				historyCallIDs[id] = struct{}{}
			}
		}
	}
	batchResults := make([]CursorToolResult, 0, len(analysis.results))
	for _, ref := range analysis.results {
		batchResults = append(batchResults, CursorToolResult{CallID: ref.callID})
	}
	if !cursorHistoryComplete(batchResults, historyCallIDs) {
		return reject("tool result tail is missing its original assistant call")
	}
	// Most recent real user becomes the resume anchor; never fabricate one from results.
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].role == "user" {
			return turns[i], nil
		}
	}
	return reject("tool result tail has no real user anchor")
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

// cursorBuildMcpTools 把本轮最终有效工具子集逐条编码为 AgentRunRequest.f4（McpTools）。
//
// 每个真实工具单独一条 McpToolDefinition（不再拼成单入口包装）：f1 name=真实名、
// f2 description=原描述、f4 provider_identifier=cursorMcpProviderIdentifier、
// f5 tool_name=真实名、f6 input_schema_json=原 schema 文本。f6 用 cursorAppendStringField
// 写原字节，禁止经 map[string]any/float64 重序列化，number / null / 大整数一律保真。
//
// 非法 schema 的处置沿用既有口径：丢掉这一个工具、记 tool_schema_invalid，不整轮 400。
// 全部工具都无效（或工具集为空）时返回 nil，f4 保持空 message，与活体样本同形。
func cursorBuildMcpTools(tools []apicompat.ChatTool, skipped cursorSkipLedger) ([]byte, error) {
	var mcp []byte
	for _, t := range tools {
		if t.Function == nil || t.Function.Name == "" {
			continue
		}
		schema := strings.TrimSpace(string(t.Function.Parameters))
		if schema == "" || schema == "null" {
			schema = "{}"
		}
		if !json.Valid([]byte(schema)) {
			skipped.note(cursorSkipToolSchemaInvalid)
			continue
		}
		var def []byte
		def = cursorAppendStringField(def, cursorFldMcpToolDefName, t.Function.Name)
		def = cursorAppendStringField(def, cursorFldMcpToolDefDescription, t.Function.Description)
		def = cursorAppendStringField(def, cursorFldMcpToolDefProvider, cursorMcpProviderIdentifier)
		def = cursorAppendStringField(def, cursorFldMcpToolDefToolName, t.Function.Name)
		def = cursorAppendStringField(def, cursorFldMcpToolDefInputSchemaJSON, schema)
		mcp = cursorAppendBytesField(mcp, cursorFldMcpToolsTool, def)
	}
	if len(mcp) == 0 {
		return nil, nil
	}
	return mcp, nil
}

// cursorTurnImage 是一张已解出原始字节的图片（来自 content-part 的 data URI）。
type cursorTurnImage struct {
	data     []byte // 原始图片字节（由 data:<mime>;base64,<payload> 解出）
	mimeType string // 如 "image/png"
}

// cursorAgentToolCall 是 assistant 历史里的一条结构化工具调用。
// args 是原始参数 JSON **对象**（空/null 归一为 {}），不得再编码成字符串。
type cursorAgentToolCall struct {
	callID string
	name   string
	args   json.RawMessage
}

// cursorAgentToolResult 是 tool 历史里的一条结果：callID 与对应调用一致，text 为真实结果文本。
type cursorAgentToolResult struct {
	callID  string
	text    string
	isError bool
}

// cursorAgentTurn 是一条已归一化的对话轮次（角色 + 文本 + 可选图片 + 结构化工具调用/结果）。
type cursorAgentTurn struct {
	// role 取值："user" / "assistant" / "tool"。历史 blob 按真实客户端结构表达：
	// assistant 携带 tool-call 内容项，tool 携带 tool-result 内容项（不再是纯文本包装）。
	role string
	text string
	// images 是当前轮携带的图片（user/assistant/tool 均可能带图）。图片落点：
	//   - 当前轮（最后一轮，必为 user 角色）→ UserMessage.selected_context；
	//   - 历史轮 user → history blob 的 content 数组（改动 D）。
	// 纯文本轮 images 为空；system/developer 含图则失败关闭（无落点）。
	images []cursorTurnImage
	// toolCalls 仅 assistant 使用，按原顺序与 text 一起写入同一个 content 数组。
	toolCalls []cursorAgentToolCall
	// toolResults 仅 tool 使用，按原顺序写入 tool-result 内容项。
	toolResults []cursorAgentToolResult
}

// cursorHistoryBlob 是一条历史消息的 KV 载荷（内容寻址）。
type cursorHistoryBlob struct {
	// id 是 value 的 sha256（32 字节）。证据 63 的两条 blob 已逐字节复核：
	// sha256(value) == root_prompt_messages_json 里的 blob id。
	id []byte
	// value 是历史消息的 JSON 序列化（形状见 cursorHistoryUserBlob /
	// cursorHistoryAssistantBlob / cursorHistoryToolBlob）。
	value []byte
}

// cursorHistoryUserBlob 是用户侧历史消息的值形状（证据 62/63 活体样本）。
type cursorHistoryUserBlob struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// cursorHistoryCallDynamicToolName 是第三方已观察的内部历史操作名。它只出现在历史
// content 里，不向客户端暴露，也不新声明第二个可执行工具。
const cursorHistoryCallDynamicToolName = "CallDynamicTool"

// cursorHistoryAssistantBlob 是助手侧历史消息的值形状（真实客户端原件）：
//
//	{"role":"assistant","content":[{"type":"text","text":"…"},
//	 {"type":"tool-call","toolCallId":"call_…","toolName":"CallDynamicTool",
//	  "args":{"arguments":{ },"namespace":"<provider>","toolName":"Read"}}],"id":"1"}
type cursorHistoryAssistantBlob struct {
	Role    string                       `json:"role"`
	Content []cursorHistoryAssistantPart `json:"content"`
	ID      string                       `json:"id"`
}

// cursorHistoryAssistantPart 是 assistant content 数组里的一项：type 互斥。
// text 项仅在文本非空时写；tool-call 项按原顺序跟在 text 之后，多个调用各一项。
type cursorHistoryAssistantPart struct {
	Type       string                     `json:"type"`
	Text       string                     `json:"text,omitempty"`
	ToolCallID string                     `json:"toolCallId,omitempty"`
	ToolName   string                     `json:"toolName,omitempty"`
	Args       *cursorHistoryCallToolArgs `json:"args,omitempty"`
}

// cursorHistoryCallToolArgs 是 tool-call.args 的形状：arguments 是原参数 JSON 对象
// （RawMessage，不再编码成字符串），namespace 用现有 provider 常量，toolName 是真实名。
type cursorHistoryCallToolArgs struct {
	Arguments json.RawMessage `json:"arguments"`
	Namespace string          `json:"namespace"`
	ToolName  string          `json:"toolName"`
}

// cursorHistoryToolBlob 是工具结果历史消息的值形状（真实客户端原件）：
//
//	{"role":"tool","content":[{"type":"tool-result","toolCallId":"call_…",
//	 "toolName":"CallDynamicTool","result":"<文本>",
//	 "experimental_content":[{"type":"text","text":"<同一文本>"}]}],
//	 "id":"call_…","providerOptions":{"cursor":{"highLevelToolCallResult":{
//	 "output":{"success":{"content":[{"text":{"text":"<同一文本>"}}],"isError":true}},"isError":true}}}}
type cursorHistoryToolBlob struct {
	Role            string                        `json:"role"`
	Content         []cursorHistoryToolResultPart `json:"content"`
	ID              string                        `json:"id"`
	ProviderOptions cursorHistoryToolProviderOpts `json:"providerOptions"`
}

type cursorHistoryToolResultPart struct {
	Type                string                       `json:"type"`
	ToolCallID          string                       `json:"toolCallId"`
	ToolName            string                       `json:"toolName"`
	Result              string                       `json:"result"`
	ExperimentalContent []cursorHistoryAssistantPart `json:"experimental_content"`
}

type cursorHistoryToolProviderOpts struct {
	Cursor cursorHistoryToolCursorOpts `json:"cursor"`
}

type cursorHistoryToolCursorOpts struct {
	HighLevelToolCallResult cursorHistoryToolHighLevelResult `json:"highLevelToolCallResult"`
}

type cursorHistoryToolHighLevelResult struct {
	Output  cursorHistoryToolOutput `json:"output"`
	IsError bool                    `json:"isError"`
}

type cursorHistoryToolOutput struct {
	Success cursorHistoryToolSuccess `json:"success"`
}

type cursorHistoryToolSuccess struct {
	Content []cursorHistorySuccessContent `json:"content"`
	IsError bool                          `json:"isError,omitempty"`
}

type cursorHistorySuccessContent struct {
	Text cursorHistorySuccessText `json:"text"`
}

type cursorHistorySuccessText struct {
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
// assistant / tool 按真实客户端结构输出结构化 content；纯文本与带图 user 的形状不变。
func cursorHistoryBlobs(history []cursorAgentTurn, skipped cursorSkipLedger) ([]cursorHistoryBlob, error) {
	out := make([]cursorHistoryBlob, 0, len(history))
	assistantSeq := 0
	for _, turn := range history {
		var value []byte
		var err error
		switch {
		case turn.role == "assistant":
			// assistant 历史侧无 image 落点：带图时丢图保文本并记账。
			if len(turn.images) > 0 {
				skipped.note(cursorSkipImageOutsideUser)
			}
			assistantSeq++
			content := make([]cursorHistoryAssistantPart, 0, 1+len(turn.toolCalls))
			if turn.text != "" {
				content = append(content, cursorHistoryAssistantPart{Type: "text", Text: turn.text})
			}
			for _, call := range turn.toolCalls {
				content = append(content, cursorHistoryAssistantPart{
					Type:       "tool-call",
					ToolCallID: call.callID,
					ToolName:   cursorHistoryCallDynamicToolName,
					Args: &cursorHistoryCallToolArgs{
						Arguments: call.args,
						Namespace: cursorMcpProviderIdentifier,
						ToolName:  call.name,
					},
				})
			}
			value, err = cursorMarshalBlobJSON(cursorHistoryAssistantBlob{
				Role:    "assistant",
				Content: content,
				// id 取**助手消息的序号**（真实客户端样本里首条 assistant 的 id 为 "1"）。
				ID: strconv.Itoa(assistantSeq),
			})
		case turn.role == "tool":
			// tool 结果同样没有图片落点：丢图保结果文本并记账。
			if len(turn.images) > 0 {
				skipped.note(cursorSkipImageOutsideUser)
			}
			content := make([]cursorHistoryToolResultPart, 0, len(turn.toolResults))
			successContent := make([]cursorHistorySuccessContent, 0, len(turn.toolResults))
			resultID := ""
			isError := false
			for _, r := range turn.toolResults {
				content = append(content, cursorHistoryToolResultPart{
					Type:       "tool-result",
					ToolCallID: r.callID,
					ToolName:   cursorHistoryCallDynamicToolName,
					Result:     r.text,
					ExperimentalContent: []cursorHistoryAssistantPart{{
						Type: "text", Text: r.text,
					}},
				})
				successContent = append(successContent, cursorHistorySuccessContent{
					Text: cursorHistorySuccessText{Text: r.text},
				})
				isError = isError || r.isError
				if resultID == "" {
					resultID = r.callID
				}
			}
			value, err = cursorMarshalBlobJSON(cursorHistoryToolBlob{
				Role:    "tool",
				Content: content,
				// id 取原 call ID；legacy function 无 ID 时保持空，不自动配对。
				ID: resultID,
				ProviderOptions: cursorHistoryToolProviderOpts{
					Cursor: cursorHistoryToolCursorOpts{
						HighLevelToolCallResult: cursorHistoryToolHighLevelResult{
							Output: cursorHistoryToolOutput{
								Success: cursorHistoryToolSuccess{Content: successContent, IsError: isError},
							},
							IsError: isError,
						},
					},
				},
			})
		case len(turn.images) > 0:
			// 带图 user 轮 → rich blob（content 数组），纯文本路径永不到这。
			value, err = cursorMarshalBlobJSON(cursorHistoryUserRichBlobOf(turn))
		default:
			value, err = cursorMarshalBlobJSON(cursorHistoryUserBlob{Role: "user", Content: turn.text})
		}
		if err != nil {
			return nil, newCursorRequestError(http.StatusInternalServerError, infraerrors.CursorExecInternalError, "marshal history blob: %v", err)
		}
		out = append(out, cursorHistoryBlob{id: cursorContentAddress(value), value: value})
	}
	return out, nil
}

// cursorMarshalBlobJSON 序列化历史 blob 值，**关闭 HTML 转义**（< > & 保持字面量）。
//
// 协议 blob 是 Cursor 会 JSON 解析的 bytes 载荷；关闭转义让工具调用参数里的
// < > & 以原字节出现，避免钉字节测试看到 \u003c 之类改写。
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

// cursorSemanticRole 是请求期消息的唯一规范角色集合。
//
// system / developer / instruction 统一归为 instruction：它们都是"对模型的指令"，
// 在 Cursor 协议里都没有独立落点，只能合入真实 user；除此之外的角色按既有语义归类。
const (
	cursorSemanticInstruction = "instruction"
	cursorSemanticUser        = "user"
	cursorSemanticAssistant   = "assistant"
	cursorSemanticTool        = "tool"
	cursorSemanticUnknown     = "unknown"
)

// cursorResultContainerKind 显式标记"原源容器"是否完全由工具结果组成。
//
// 标记来自解码输入（见 cursorDecodeSemanticMessages），**不是**对展开后的若干 Chat
// 消息做尾部扫描猜出来的。日后的复合源协议解码器必须能显式把一条语义消息标成
// 混合容器，避免先把结果块扁平化之后再误判成纯结果容器。
type cursorResultContainerKind uint8

const (
	// cursorContainerMixed 表示源容器可能含普通正文（默认）。
	cursorContainerMixed cursorResultContainerKind = iota
	// cursorContainerAllToolResults 表示源容器完全由工具结果组成。
	cursorContainerAllToolResults
)

// cursorSemanticMessage 是请求期的语义消息：规范角色 + 原始字段载体 + 消息顺序 +
// 源容器归属 + 源容器完整性标记。语义切片的切片下标承载消息顺序（语义序列中的先后）；
// sourceContainerIndex 承载原始容器归属，即该语义消息来自原请求的第几个源容器——二者
// 含义不同：多个展开后的语义消息仍可归属于同一个源容器。raw 保留解码前的原始字段，
// 任何下游解码器都能在不改变规范角色的前提下携带自己的专有字段。
type cursorSemanticMessage struct {
	role                 string
	raw                  apicompat.ChatMessage
	container            cursorResultContainerKind
	sourceContainerIndex int
}

// cursorClassifySemanticRole 是角色分类的唯一权威 helper。
//
// system/developer/instruction → instruction；user/human、assistant/ai、tool/function
// 按既有语义归类；其它角色 → unknown（由调用方按既有账本丢弃）。
func cursorClassifySemanticRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "system", "developer", "instruction":
		return cursorSemanticInstruction
	case "user", "human":
		return cursorSemanticUser
	case "assistant", "ai":
		return cursorSemanticAssistant
	case "tool", "function":
		return cursorSemanticTool
	default:
		return cursorSemanticUnknown
	}
}

// cursorDecodeSemanticMessages 是 Chat 消息的薄解码适配器：保留顺序与原始字段，
// 用唯一分类 helper 定规范角色，并把容器标记取自本条解码输入。
//
// Chat 的一条 tool/function 消息本身就是一个完整的结果容器，因此标记纯结果；
// 其它角色（包括"文本为空但带 tool_calls 的 assistant"）都是混合容器——绝不能
// 因为它是最后一条或内容形似结果块就反过来猜成纯结果容器。
// sourceContainerIndex 在解码时直接取自 msgs 的原始下标，每条 Chat 消息各自构成一个
// 源容器；该位置在展开或适配前即固定，后续扫描不得重算或猜测。
func cursorDecodeSemanticMessages(msgs []apicompat.ChatMessage) []cursorSemanticMessage {
	out := make([]cursorSemanticMessage, 0, len(msgs))
	for i, m := range msgs {
		sem := cursorSemanticMessage{
			role:                 cursorClassifySemanticRole(m.Role),
			raw:                  m,
			sourceContainerIndex: i,
		}
		if sem.role == cursorSemanticTool {
			sem.container = cursorContainerAllToolResults
		}
		out = append(out, sem)
	}
	return out
}

// cursorConvertMessages 是**旧 Chat 测试入口**：薄解码适配器，不含归一化逻辑。
func cursorConvertMessages(msgs []apicompat.ChatMessage, skipped cursorSkipLedger) ([]cursorAgentTurn, error) {
	return cursorSemanticMessagesToTurns(cursorDecodeSemanticMessages(msgs), skipped)
}

// cursorSemanticMessagesToTurns 把语义消息归一化为有序轮次（唯一的归一化实现）。
//
// **instruction 消息的处理（沿用 E6 卡的用户裁定：合并进 user）**——依据：
//  1. AgentRun 的 UserMessage 只有 user 一种落点，**无 instruction 角色**；历史 blob
//     则按真实客户端结构表达 user / assistant / tool 三种消息（tool 有独立角色）。
//  2. 丢弃会**静默改变语义**：绝大多数客户端把 instruction 当硬约束，丢了会得到完全
//     不同的回答，且用户无从察觉——比"格式略不理想"危险得多。
//
// 因此：instruction 文本按出现顺序拼到最近的一条真实 user 上，永不生成 user
// turn/action；若整段对话没有任何真实 user，则失败关闭（既有 CUR-REQ-06）。
func cursorSemanticMessagesToTurns(sem []cursorSemanticMessage, skipped cursorSkipLedger) ([]cursorAgentTurn, error) {
	turns := make([]cursorAgentTurn, 0, len(sem))
	var instructionParts []string

	for _, msg := range sem {
		m := msg.raw
		text, images, err := cursorMessageContent(m, skipped)
		if err != nil {
			// 内容层已改成「解析不了的只丢这一条」，正常路径不再有可预期错误；
			// 返回位留给真正的内部失败（不吞，避免把 bug 伪装成"丢了一条"）。
			return nil, err
		}
		switch msg.role {
		case cursorSemanticInstruction:
			// instruction 无图片落点 → 丢图保文本并记账（2026-11-06 口径）。
			if len(images) > 0 {
				skipped.note(cursorSkipImageOutsideUser)
				images = nil
			}
			if text != "" {
				instructionParts = append(instructionParts, text)
			}
		case cursorSemanticUser:
			// instruction 只进入最近的真实 user 上下文，永不生成新的 user action。
			if len(instructionParts) > 0 {
				text = cursorJoinSystem(instructionParts) + text
				instructionParts = nil
			}
			turns = append(turns, cursorAgentTurn{role: "user", text: text, images: images})
		case cursorSemanticAssistant:
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
			// assistant 的 tool_calls 登记为结构化调用（空/null 参数按 {}），与文本
			// 保序写入同一个 content 数组；仅含工具调用的 assistant 不是空占位。
			toolCalls := make([]cursorAgentToolCall, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				args := tc.Function.Arguments
				trimmedArgs := strings.TrimSpace(args)
				if trimmedArgs == "" || trimmedArgs == "null" {
					args = "{}"
				}
				toolCalls = append(toolCalls, cursorAgentToolCall{
					callID: tc.ID,
					name:   tc.Function.Name,
					args:   json.RawMessage(args),
				})
			}
			turns = append(turns, cursorAgentTurn{
				role: "assistant", text: text, images: images, toolCalls: toolCalls,
			})
		case cursorSemanticTool:
			// 容器标记取自解码输入：Chat 单条 tool/function 消息是完整结果容器，
			// 按 tool-result 内容项表达（call ID / 文本 / isError 全保序）。日后的
			// 复合源若显式标为混合容器，不会被当成纯结果块处理。
			if len(images) > 0 {
				skipped.note(cursorSkipImageOutsideUser)
				images = nil
			}
			if strings.EqualFold(strings.TrimSpace(m.Role), "function") && strings.TrimSpace(m.ToolCallID) == "" {
				// legacy function 消息没有 tool_call_id：无法与调用配对。
				// 既有记账保留，历史按空 id 表达，不自动配对、不整轮拒。
				skipped.note(cursorSkipLegacyFunctionNoID)
			}
			turns = append(turns, cursorAgentTurn{
				role:        "tool",
				text:        text,
				images:      images,
				toolResults: []cursorAgentToolResult{{callID: m.ToolCallID, text: text, isError: m.IsError}},
			})
		case cursorSemanticUnknown:
			// 空角色与未知角色都归为 unknown，但保留既有两笔不同的账。
			if strings.TrimSpace(m.Role) == "" {
				// 没有角色的消息：丢这一条，不整轮拒（2026-11-06 产品口径：
				// 客户把 400 当成"模型不稳定"，宁可少一条也不能整轮失败）。
				skipped.note(cursorSkipEmptyRole)
			} else {
				// 未知角色（critic / narrator 之类客户端自定义）：按 user 处理会把
				// assistant 语义搞错，按 assistant 处理又会篡改轮次；因此**丢掉这一条**
				// 继续本轮，而不是把整个请求 400 掉。
				skipped.note(cursorSkipUnknownRole)
			}
			continue
		}
	}

	// 收尾：尾部 instruction 合入最近一条真实 user；不新建 user action。
	if len(instructionParts) > 0 {
		merged := false
		for i := len(turns) - 1; i >= 0; i-- {
			if turns[i].role == "user" {
				turns[i].text = turns[i].text + cursorJoinSystem(instructionParts)
				merged = true
				break
			}
		}
		if !merged {
			// 没有真实 user 承载 instruction：不新建 user action（instruction
			// 永不成为用户提问），交给既有 CUR-REQ-06 明确拒绝，不静默丢文本。
			return nil, newCursorRequestError(http.StatusBadRequest,
				infraerrors.CursorReqNoContent, "instruction without a real user message")
		}
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
