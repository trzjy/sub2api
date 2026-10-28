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
//   - **不支持多模态**：历史 blob 的 content 只有 text 一种落点，带 content-parts
//     非文本的消息直接失败关闭而非静默丢图。
//   - 不新增任何非文本 action（工具/执行/多模态一律不发）。

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/google/uuid"
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
	cursorFldRunModelDetails      = 3  // ModelDetails
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
	cursorFldUserMsgText      = 1 // string
	cursorFldUserMsgMessageID = 2 // string
	cursorFldUserMsgMode      = 4 // enum ConversationMode

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
	body, _, err := cursorBuildAgentRun(req, upstreamModel)
	return body, err
}

// cursorBuildAgentRun 构造 AgentRun 请求体，并回传本次的 conversation_id。
//
// 回传 conversation_id 的目的：它是这一轮对话在上游的唯一标识，调用方可据此把
// 流内事件（含 P1-A1 的拒付哨兵）关联回具体请求。
func cursorBuildAgentRun(req *apicompat.ChatCompletionsRequest, upstreamModel string) ([]byte, string, error) {
	if req == nil {
		return nil, "", fmt.Errorf("%w: nil request", ErrCursorUnsupportedRequest)
	}
	if strings.TrimSpace(upstreamModel) == "" {
		return nil, "", fmt.Errorf("%w: empty upstream model", ErrCursorUnsupportedRequest)
	}

	turns, err := cursorConvertMessages(req.Messages)
	if err != nil {
		return nil, "", err
	}
	if len(turns) == 0 {
		return nil, "", fmt.Errorf("%w: no convertible messages", ErrCursorUnsupportedRequest)
	}

	// AgentRun 的已取证入口只有 user_message_action：最后一轮必须是用户消息，
	// 否则上游无处接收"当前这一轮的提问"。以 assistant 结尾的入站（prefill）
	// 本层不支持，**失败关闭**而不是悄悄把 assistant 文本当用户话发出去
	// （那会把模型续写当成用户指令，语义彻底错乱）。
	last := turns[len(turns)-1]
	if last.role != "user" {
		return nil, "", fmt.Errorf("%w: conversation must end with a user message", ErrCursorUnsupportedRequest)
	}
	history := turns[:len(turns)-1]

	conversationID := cursorNewUUID()
	messageID := cursorNewUUID()

	// 1) conversation_state：mode + 历史消息的 blob id（content 走 KV 侧信道）。
	//
	// 证据 63：conversation_state = {mode:2, root_prompt_messages_json:[blobId…]}，
	// 每个 blobId 32 字节 = sha256(blobValue)，blobValue 由 pre_fetched_blobs 携带。
	state := cursorAppendVarintField(nil, cursorFldStateMode, cursorConversationModeAgent)
	blobs, err := cursorHistoryBlobs(history)
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
	mcpTools, err := cursorBuildMcpTools(declared)
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

// cursorBuildMcpTools 把一组 function 工具编码为 AgentRunRequest.f4（McpTools）内容。
//
// 每个函数生成一条 McpToolDefinition（McpTools.f1 repeated），五个字段都要写：
// f1 name / f2 description / f4 provider_identifier="sub2api" / f5 tool_name /
// f3 input_schema（google.protobuf.Value，由 function.parameters 的 JSON 经
// structpb + proto.Marshal 编码，与 AgentRun 真实出站同形；schema 缺失补空 Struct，
// 非法 JSON 失败关闭，绝不静默丢工具）。
// 返回空切片表示"无声明"（f4 保持空 message，与活体样本同形）。
func cursorBuildMcpTools(tools []apicompat.ChatTool) ([]byte, error) {
	var mcp []byte
	for _, t := range tools {
		if t.Function == nil || t.Function.Name == "" {
			continue
		}
		schema := string(t.Function.Parameters)
		if strings.TrimSpace(schema) == "" || schema == "null" {
			schema = "{}"
		}
		// schema 必须是合法 JSON 对象，才能编码进 google.protobuf.Value；
		// 非法则**失败关闭**（不静默丢工具、不猜宽松 fallback）。
		value := &structpb.Value{}
		if err := protojson.Unmarshal([]byte(schema), value); err != nil {
			return nil, fmt.Errorf(
				"%w: tool %q: invalid input_schema JSON: %v", ErrCursorUnsupportedRequest, t.Function.Name, err)
		}
		valBytes, err := proto.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf(
				"%w: tool %q: marshal input_schema Value: %v", ErrCursorUnsupportedRequest, t.Function.Name, err)
		}
		var def []byte
		def = cursorAppendStringField(def, cursorFldMcpToolDefName, t.Function.Name)
		def = cursorAppendStringField(def, cursorFldMcpToolDefDescription, t.Function.Description)
		def = cursorAppendStringField(def, cursorFldMcpToolDefProvider, cursorMcpProviderIdentifier)
		def = cursorAppendStringField(def, cursorFldMcpToolDefToolName, t.Function.Name)
		def = cursorAppendBytesField(def, cursorFldMcpToolDefInputSchemaValue, valBytes)
		mcp = cursorAppendBytesField(mcp, cursorFldMcpToolsTool, def)
	}
	return mcp, nil
}

// cursorAgentTurn 是一条已归一化的对话轮次（角色 + 纯文本）。
type cursorAgentTurn struct {
	// role 只有两种取值："user" / "assistant"（AgentRun 的历史 blob 只有这两种
	// role 落点，见 62-kv-value-shapes.json 的活体值形状）。
	role string
	text string
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

// cursorHistoryBlobs 把历史轮次序列化为内容寻址的 KV 载荷（保序）。
func cursorHistoryBlobs(history []cursorAgentTurn) ([]cursorHistoryBlob, error) {
	out := make([]cursorHistoryBlob, 0, len(history))
	assistantSeq := 0
	for _, turn := range history {
		var value []byte
		var err error
		if turn.role == "assistant" {
			assistantSeq++
			value, err = cursorMarshalBlobJSON(cursorHistoryAssistantBlob{
				Role:    "assistant",
				Content: []cursorHistoryAssistantTextPart{{Type: "text", Text: turn.text}},
				// id 取**助手消息的序号**（活体样本里首条 assistant 的 id 为 "1"）。
				ID: strconv.Itoa(assistantSeq),
			})
		} else {
			value, err = cursorMarshalBlobJSON(cursorHistoryUserBlob{Role: "user", Content: turn.text})
		}
		if err != nil {
			return nil, fmt.Errorf("%w: marshal history blob: %v", ErrCursorUnsupportedRequest, err)
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
func cursorConvertMessages(msgs []apicompat.ChatMessage) ([]cursorAgentTurn, error) {
	turns := make([]cursorAgentTurn, 0, len(msgs))
	var systemParts []string
	sawUser := false

	for _, m := range msgs {
		text, err := cursorMessageText(m)
		if err != nil {
			return nil, err
		}
		switch strings.ToLower(strings.TrimSpace(m.Role)) {
		case "system", "developer":
			if text != "" {
				systemParts = append(systemParts, text)
			}
		case "user", "human":
			// 首条 user 承载此前累积的 system（时序最自然：约束在最前）。
			if !sawUser && len(systemParts) > 0 {
				text = cursorJoinSystem(systemParts) + text
				systemParts = nil
			}
			turns = append(turns, cursorAgentTurn{role: "user", text: text})
			sawUser = true
		case "assistant", "ai":
			// assistant 可能携带 tool_calls：原 content（可空）+ 每块一个
			// <tool_call> blob（见 §4 续轮文本化）。
			turns = append(turns, cursorAgentTurn{role: "assistant", text: cursorAssistantText(m, text)})
		case "tool", "function":
			// 工具结果 → user turn（Cursor 历史仅 user/assistant 两种落点，见 §4）。
			// 文本化失败关闭（含非文本 part 时与多模态口径一致）。
			tt, err := cursorToolResultText(m)
			if err != nil {
				return nil, err
			}
			turns = append(turns, cursorAgentTurn{role: "user", text: tt})
		case "":
			return nil, fmt.Errorf("%w: message without role", ErrCursorUnsupportedRequest)
		default:
			// 未知角色：按 user 处理会把 assistant 语义搞错，宁可失败关闭。
			return nil, fmt.Errorf("%w: unsupported role %q", ErrCursorUnsupportedRequest, m.Role)
		}
	}

	// 收尾：system 出现在最后（如 [user, assistant, system]）时无处可合并。
	// 此时新建一条 user 承载——保证 system 文本不丢，且不篡改既有轮次顺序。
	if len(systemParts) > 0 {
		turns = append(turns, cursorAgentTurn{role: "user", text: cursorJoinSystem(systemParts)})
	}
	return turns, nil
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
func cursorAssistantText(m apicompat.ChatMessage, base string) string {
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
		b.WriteString("\n<tool_call>\n")
		b.WriteString(`{"id":` + cursorJSONString(tc.ID) +
			`,"name":` + cursorJSONString(tc.Function.Name) +
			`,"arguments":` + args + `}`)
		b.WriteString("\n</tool_call>")
	}
	return b.String()
}

// cursorToolResultText 把 tool / function 角色消息文本化为一条 user turn。
//
// 续轮文本化（§4）：Cursor 历史仅 user/assistant 两种落点，工具结果无处可去，
// 故包成 `<tool_result tool_call_id="..." name="...">\n<content>\n...\n</tool_result>`
// 落为 user。content 为字符串或纯 text part 拼接；含非文本 part → 失败关闭
// （与多模态口径一致）。
func cursorToolResultText(m apicompat.ChatMessage) (string, error) {
	content, err := cursorMessageText(m)
	if err != nil {
		return "", err
	}
	id := m.ToolCallID
	name := m.Name
	if name == "" && m.FunctionCall != nil {
		name = m.FunctionCall.Name
	}
	var b strings.Builder
	b.WriteString("<tool_result tool_call_id=")
	b.WriteString(cursorJSONString(id))
	b.WriteString(" name=")
	b.WriteString(cursorJSONString(name))
	b.WriteString(">\n<content>\n")
	b.WriteString(content)
	b.WriteString("\n</tool_result>")
	return b.String(), nil
}

// cursorMessageText 从一条 OpenAI 消息取纯文本。
//
// 失败关闭：content 是数组（content-parts）且含非文本 part 时报错，**不静默丢弃图片**——
// 静默丢图会让模型在缺图的情况下自信作答，比报错有害得多。
func cursorMessageText(m apicompat.ChatMessage) (string, error) {
	raw := strings.TrimSpace(string(m.Content))
	if raw == "" || raw == "null" {
		// 兼容 content 缺省与显式 null；tool_calls 型消息正文为空是合法的。
		return "", nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			return "", fmt.Errorf("%w: message content is not a string: %v", ErrCursorUnsupportedRequest, err)
		}
		return s, nil
	}
	if raw[0] == '[' {
		// 尝试判定是否"纯文本 parts"——只有 text part 时可安全降级为字符串。
		var parts []apicompat.ChatContentPart
		if err := json.Unmarshal([]byte(raw), &parts); err != nil {
			return "", fmt.Errorf("%w: message content is not a valid parts array: %v", ErrCursorUnsupportedRequest, err)
		}
		var b strings.Builder
		for _, p := range parts {
			if p.Type != "text" || strings.TrimSpace(p.Text) == "" {
				return "", fmt.Errorf(
					"%w: multi-modal content (part type %q) is not supported by cursor protocol", ErrCursorUnsupportedRequest, p.Type)
			}
			b.WriteString(p.Text)
		}
		return b.String(), nil
	}
	// 数字 / 布尔 / 对象等：Cursor 侧只接受 string，失败关闭。
	return "", fmt.Errorf("%w: message content must be a string or content-parts array", ErrCursorUnsupportedRequest)
}
