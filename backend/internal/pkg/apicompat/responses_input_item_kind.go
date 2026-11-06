package apicompat

import "strings"

// 本文件是 Responses input 条目类型的**唯一事实源**。
//
// 背景（2026-11-06 线上故障）：同一份客户端 input 里混装了三类东西——
//   1) 对话内容（消息 / 工具调用 / 思考 / 结果）；
//   2) 请求级声明（新版 Codex 的 {"type":"additional_tools","tools":[...]}）；
//   3) 本批要回传的工具结果。
//
// 此前各处各自维护"我认识的类型"清单：工具集提升认识 additional_tools，
// 续轮历史解析不认识 → 同一份请求一边当声明提升、一边当未知内容整轮拒绝，
// 表现为"首轮能用工具、第二轮 400"。
//
// 归口后：任何按 input 逐条处理的代码都必须先用本分类器归类，再决定
// 丢弃 / 纳入摘要 / 提升为工具声明；不允许再写 type == "xxx" 的字面量分支。
//
// **归类不是准入门槛**（2026-11-06 产品口径修正）：客户端每加一种新条目类型，
// 如果一律整轮拒绝，我们就会跟着变成"每次客户端升级就全站 400"——客户看到的是
// "模型不稳定"，而不是"你少给我一个字段"。因此旁支条目（Ignorable）与尚未接纳的
// 新类型（Unknown）都由调用方**丢弃并记账**：宁可少一点上下文，也不能把报错丢给客户。
// 唯一必须严格的是客户端的工具调用链（*_call 与配对 *_output）：丢掉一侧会造出
// "有结果没有调用"的畸形历史，那种情况由调用方成对丢弃，而不是只丢一半。

// ResponsesInputItemKind 是 Responses input 条目的归类结果。
type ResponsesInputItemKind int

const (
	// ResponsesInputItemUnknown 表示本站未接纳的条目类型（调用方必须失败关闭）。
	ResponsesInputItemUnknown ResponsesInputItemKind = iota
	// ResponsesInputItemConversation 是对话内容，参与上下文/历史摘要。
	ResponsesInputItemConversation
	// ResponsesInputItemDeclaration 是请求级工具声明，不参与对话摘要。
	ResponsesInputItemDeclaration
	// ResponsesInputItemResult 是本批工具结果（回传给上游的 result 帧）。
	ResponsesInputItemResult
	// ResponsesInputItemIgnorable 是本站明确不转发、也不参与摘要的旁支条目：
	// 宿主工具调用（联网搜索 / 生图 / 本地命令 / MCP）、压缩记录、多智能体消息、
	// 条目前向引用等。它们不影响客户端工具调用链，丢弃只损失少量上下文。
	ResponsesInputItemIgnorable
)

// String 便于日志定位（未知类型必须能在日志里看见原始类型名）。
func (k ResponsesInputItemKind) String() string {
	switch k {
	case ResponsesInputItemConversation:
		return "conversation"
	case ResponsesInputItemDeclaration:
		return "declaration"
	case ResponsesInputItemResult:
		return "result"
	case ResponsesInputItemIgnorable:
		return "ignorable"
	default:
		return "unknown"
	}
}

// ClassifyResponsesInputItemType 按条目类型名归类（大小写、空白不敏感）。
//
// 空类型视为 message（Responses 允许省略 type）。
func ClassifyResponsesInputItemType(typeName string) ResponsesInputItemKind {
	switch strings.ToLower(strings.TrimSpace(typeName)) {
	case "", "message", "function_call", "custom_tool_call", "tool_search_call", "reasoning":
		return ResponsesInputItemConversation
	case "additional_tools":
		return ResponsesInputItemDeclaration
	case "function_call_output", "custom_tool_call_output", "tool_search_output":
		return ResponsesInputItemResult
	case "web_search_call", "x_search_call", "image_generation_call",
		"local_shell_call", "local_shell_call_output",
		"mcp_tool_call", "mcp_tool_call_output", "mcp_list_tools",
		"agent_message", "compaction", "compaction_summary", "compaction_trigger",
		"item_reference":
		// 宿主工具调用（联网搜索 / 生图 / 本地命令 / MCP）、会话压缩记录、多智能体消息、
		// 条目前向引用：本站从不把它们转发给上游，续轮回放时也不参与历史摘要。
		// 丢弃只损失少量旁支上下文，不影响客户端工具调用链。
		return ResponsesInputItemIgnorable
	default:
		return ResponsesInputItemUnknown
	}
}

// ClassifyResponsesInputItem 是类型名版本的封装。
func ClassifyResponsesInputItem(item ResponsesInputItem) ResponsesInputItemKind {
	return ClassifyResponsesInputItemType(item.Type)
}

// IsResponsesInputDeclaration 判断条目是否只是工具声明（对话摘要不得包含它）。
func IsResponsesInputDeclaration(typeName string) bool {
	return ClassifyResponsesInputItemType(typeName) == ResponsesInputItemDeclaration
}

// IsResponsesInputResult 判断条目是否为本批工具结果。
func IsResponsesInputResult(typeName string) bool {
	return ClassifyResponsesInputItemType(typeName) == ResponsesInputItemResult
}

// IsResponsesInputIgnorable 判断条目是否属于"本站故意丢弃"的旁支条目。
//
// 与 Unknown 的区别只在记账口径：Ignorable 是已知的旁支（联网搜索、压缩记录等），
// Unknown 是还没见过的新类型（出现即说明客户端又加东西了，值得盯）。
// 两者的处理一致：丢弃 + 记账，都不整轮拒。
func IsResponsesInputIgnorable(typeName string) bool {
	return ClassifyResponsesInputItemType(typeName) == ResponsesInputItemIgnorable
}
