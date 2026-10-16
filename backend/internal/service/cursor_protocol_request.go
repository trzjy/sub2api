package service

// 本文件实现 **OpenAI Chat Completions 请求 → Cursor 原生 protobuf 请求**的转换
// （E6 卡，方案 docs-local/cursor-platform-fix-plan.md §1.5 / §2「下一张卡裁定」）。
//
// 取证来源（勿凭记忆改字段号）：
//   - 传输框架：02-request-body.md §1（Connect RPC v1 + 5 字节 envelope + 二进制 protobuf）
//   - 字段号：12-schema-StreamUnifiedChatRequestWithTools.txt / 10-schema-StreamUnifiedChatRequest.txt
//   - 活体确认：08-live-u2-frames.md §4（请求体形状已离线回读自检）
//
// 首版范围（用户 2026-10-16 裁定，**纯文本**）：
//   - 请求侧 `tools` / `tool_choice` **丢弃**（Cursor 无对等结构，唯一形似的是 mcp_tools，
//     自造映射风险高于收益——见 99-blockers.md B-2(b)）。
//   - **不支持多模态**：`ConversationMessage.text` 是 string 而非 content-parts 数组
//     （99-blockers.md B-3），带 content-parts 的消息直接失败关闭而非静默丢图。
//   - 响应侧 `client_side_tool_v2_call` 丢弃（见 cursor_protocol_response.go）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

// Cursor 协议字段号（全部来自已取证 schema，**不得臆造**）。
const (
	// cursorFldStreamUnifiedChatRequest = StreamUnifiedChatRequestWithTools.1
	// （oneof=request 的首分支；另有 2 client_side_tool_v2_result / 3 abort / 4 close）
	cursorFldStreamUnifiedChatRequest = 1

	// StreamUnifiedChatRequest 内：
	cursorFldConversation       = 1  // repeated ConversationMessage
	cursorFldModelDetails       = 5  // ModelDetails（**是 5 不是 3**，见 99-blockers.md B-1 §2）
	cursorFldIsChat             = 22 // bool，是否纯聊天
	cursorFldUnifiedMode        = 46 // enum，1=CHAT
	cursorFldShouldDisableTools = 48 // bool [opt]，关掉服务端工具下发

	// ConversationMessage 内：
	cursorFldConvText = 1 // string（**不是 content-parts**，见 B-3）
	cursorFldConvType = 2 // MessageType 枚举

	// ModelDetails 内：
	cursorFldModelName = 1 // string [opt]，**必须填 f36 wire ID**
)

// Cursor ConversationMessage.MessageType 枚举值（99-blockers.md B-4，已取证）。
// 只有 HUMAN(1)/AI(2) 两值可用——**无 system 落点**，故 system 须合并（见下）。
const (
	cursorMsgTypeHuman = 1
	cursorMsgTypeAI    = 2
)

// cursorUnifiedModeChat 是 StreamUnifiedChatRequest.UnifiedMode 的 CHAT 值。
const cursorUnifiedModeChat = 1

// ErrCursorUnsupportedRequest 表示入站形状落在首版范围外（失败关闭，不静默降级）。
var ErrCursorUnsupportedRequest = errors.New("cursor: request shape not supported in v1 (fail closed)")

// cursorConvertRequest 把 OpenAI Chat Completions 请求转换为 Cursor 原生请求体
// （**已含 envelope**，可直接作为 CursorUpstreamRequest.Body）。
//
// upstreamModel 必须是 **f36 wire ID**（如 `grok-4.7`）——填 f1 展示名会被上游拒
// `ERROR_BAD_MODEL_NAME`（09-live-models.md §4 活体实证）。
func cursorConvertRequest(req *apicompat.ChatCompletionsRequest, upstreamModel string) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("%w: nil request", ErrCursorUnsupportedRequest)
	}
	if strings.TrimSpace(upstreamModel) == "" {
		return nil, fmt.Errorf("%w: empty upstream model", ErrCursorUnsupportedRequest)
	}

	conversations, err := cursorConvertMessages(req.Messages)
	if err != nil {
		return nil, err
	}
	if len(conversations) == 0 {
		return nil, fmt.Errorf("%w: no convertible messages", ErrCursorUnsupportedRequest)
	}

	// StreamUnifiedChatRequest
	inner := make([]byte, 0, 256)
	for _, conv := range conversations {
		inner = cursorAppendBytesField(inner, cursorFldConversation, conv)
	}
	// ModelDetails{1 model_name}
	inner = cursorAppendBytesField(inner, cursorFldModelDetails, cursorAppendStringField(nil, cursorFldModelName, upstreamModel))
	inner = cursorAppendVarintField(inner, cursorFldIsChat, 1)
	inner = cursorAppendVarintField(inner, cursorFldUnifiedMode, cursorUnifiedModeChat)
	if len(req.Tools) > 0 || len(req.Functions) > 0 {
		// 首版丢弃 tools（用户裁定）。**同时显式关掉服务端工具开关**，否则上游可能
		// 自作主张发起 client_side_tool_v2_call，而本版会丢弃它 → 客户端看到空响应。
		inner = cursorAppendVarintField(inner, cursorFldShouldDisableTools, 1)
	}

	// StreamUnifiedChatRequestWithTools{1 stream_unified_chat_request}
	outer := cursorAppendBytesField(nil, cursorFldStreamUnifiedChatRequest, inner)
	return cursorEncodeEnvelope(outer), nil
}

// cursorConvertMessages 把 OpenAI messages 转成 repeated ConversationMessage 字节序列。
//
// **system 消息的处理（首版裁定：合并进首个 HUMAN）**——依据：
//  1. Cursor 的 MessageType 只有 HUMAN(1)/AI(2)，**无 system 落点**（B-4 已取证）。
//  2. 丢弃会**静默改变语义**：绝大多数客户端把 system 当硬约束，丢了会得到完全不同
//     的回答，且用户无从察觉——比"格式略不理想"危险得多。
//  3. Cursor 客户端自身的做法也是把指令拼进首条用户消息（编辑器上下文整体作为
//     一条 HUMAN），所以"合并进首个 HUMAN"与上游原生行为同构。
//
// 因此：system 文本按出现顺序拼到**第一条 HUMAN** 消息的开头，标记行分隔；
// 若整段对话没有任何 HUMAN（纯 system+assistant），则合到最后一条 AI 之前不存在落点，
// 此时新建一条 HUMAN 承载（否则请求会被上游判为空对话）。
func cursorConvertMessages(msgs []apicompat.ChatMessage) ([][]byte, error) {
	converted := make([][]byte, 0, len(msgs))
	var systemParts []string
	sawHuman := false

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
			// 首条 HUMAN 承载此前累积的 system（时序最自然：约束在最前）。
			if !sawHuman && len(systemParts) > 0 {
				text = cursorJoinSystem(systemParts) + text
				systemParts = nil
			}
			converted = append(converted, cursorBuildConversationMessage(text, cursorMsgTypeHuman))
			sawHuman = true
		case "assistant", "ai":
			converted = append(converted, cursorBuildConversationMessage(text, cursorMsgTypeAI))
		case "tool", "function":
			// 首版无工具通道（B-2(b)）：工具结果无处可去。**丢弃而非报错**——
			// 请求侧本就丢弃了 tools，正常不会产生 tool 消息；出现时（客户端硬塞）
			// 报错会打断本可用的一次纯文本对话。
		case "":
			return nil, fmt.Errorf("%w: message without role", ErrCursorUnsupportedRequest)
		default:
			// 未知角色：按 human 处理会把 assistant 语义搞错，宁可失败关闭。
			return nil, fmt.Errorf("%w: unsupported role %q", ErrCursorUnsupportedRequest, m.Role)
		}
	}

	// 收尾：system 出现在最后（如 [user, assistant, system]）时无处可合并。
	// 此时新建一条 HUMAN 承载——保证 system 文本不丢，且不篡改既有消息顺序。
	if len(systemParts) > 0 {
		converted = append(converted, cursorBuildConversationMessage(cursorJoinSystem(systemParts), cursorMsgTypeHuman))
	}
	return converted, nil
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

// cursorBuildConversationMessage 编码一条 ConversationMessage{1 text, 2 type}。
func cursorBuildConversationMessage(text string, msgType int) []byte {
	out := cursorAppendStringField(nil, cursorFldConvText, text)
	out = cursorAppendVarintField(out, cursorFldConvType, uint64(msgType))
	return out
}

// cursorMessageText 从一条 OpenAI 消息取纯文本。
//
// 失败关闭：content 是数组（content-parts）时报错，**不静默丢弃图片**——
// 静默丢图会让模型在缺图的情况下自信作答，比报错有害得多（99-blockers.md B-3）。
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
