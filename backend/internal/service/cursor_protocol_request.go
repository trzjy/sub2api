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
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
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

// cursorConversationModeChat 是 ConversationMode 的取值。
//
// 33 / 63 两份活体请求样本里，ConversationStateStructure.mode 与 UserMessage.mode
// **恒为 2**，且该组合被上游接受并返回真实文本，故此处按取证值原样写 2；
// 未取证的其它枚举值一律不猜。
const cursorConversationModeChat = 2

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
	state := cursorAppendVarintField(nil, cursorFldStateMode, cursorConversationModeChat)
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
	userMessage = cursorAppendVarintField(userMessage, cursorFldUserMsgMode, cursorConversationModeChat)
	userMsgAction := cursorAppendBytesField(nil, cursorFldUserMsgActionMessage, userMessage)
	action := cursorAppendBytesField(nil, cursorFldActionUserMessage, userMsgAction)

	// 3) 其余字段按活体样本的**出现顺序**编码（33/63 均为
	//    conversation_state → action → model_details → mcp_tools →
	//    conversation_id → pre_fetched_blobs…）。
	run := cursorAppendBytesField(nil, cursorFldRunConversationState, state)
	run = cursorAppendBytesField(run, cursorFldRunAction, action)
	run = cursorAppendBytesField(run, cursorFldRunModelDetails,
		cursorAppendStringField(nil, cursorFldModelDetailsID, upstreamModel))
	// mcp_tools：活体样本里是空 message 但**字段存在**（f4，长度 0）。本版不发工具，
	// 保持同形以避免上游按"字段缺失"走另一条分支。
	run = cursorAppendBytesField(run, cursorFldRunMcpTools, nil)
	run = cursorAppendStringField(run, cursorFldRunConversationID, conversationID)
	for _, b := range blobs {
		blob := cursorAppendBytesField(nil, cursorFldBlobID, b.id)
		blob = cursorAppendBytesField(blob, cursorFldBlobValue, b.value)
		run = cursorAppendBytesField(run, cursorFldRunPreFetchedBlobs, blob)
	}

	outer := cursorAppendBytesField(nil, cursorFldAgentRunRequest, run)
	return cursorEncodeEnvelope(outer), conversationID, nil
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
			value, err = json.Marshal(cursorHistoryAssistantBlob{
				Role:    "assistant",
				Content: []cursorHistoryAssistantTextPart{{Type: "text", Text: turn.text}},
				// id 取**助手消息的序号**（活体样本里首条 assistant 的 id 为 "1"）。
				ID: strconv.Itoa(assistantSeq),
			})
		} else {
			value, err = json.Marshal(cursorHistoryUserBlob{Role: "user", Content: turn.text})
		}
		if err != nil {
			return nil, fmt.Errorf("%w: marshal history blob: %v", ErrCursorUnsupportedRequest, err)
		}
		sum := sha256.Sum256(value)
		out = append(out, cursorHistoryBlob{id: sum[:], value: value})
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
			turns = append(turns, cursorAgentTurn{role: "assistant", text: text})
		case "tool", "function":
			// 首版无工具通道：工具结果无处可去。**丢弃而非报错**——
			// 请求侧本就丢弃了 tools，正常不会产生 tool 消息；出现时（客户端硬塞）
			// 报错会打断本可用的一次纯文本对话。
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
