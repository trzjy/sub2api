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
// **未知类型仍然由调用方失败关闭**——静默丢弃客户端内容比报错危险。

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
