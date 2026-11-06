package apicompat

import "strings"

// 工具声明的"身份"判定与跨站点合并，只在这里实现一次。
//
// 同一份工具集可以在两处声明：顶层 tools，以及 input 里的
// {"type":"additional_tools","tools":[...]} 条目。两者是同一份工具集的两种载体，
// 不是两个工具集——同一条工具在两处都写时，工具集里仍然只有一个工具。
//
// 反例（2026-11-06 实测，两个下游都踩到）：
//   - chat 桥把两份都发给上游 → 上游收到两条同名定义，直接 502 或回空正文；
//   - cursor 续轮按工具数比对会话配置 → "存了 1 个、这次 2 个" 误判成换了工具集，重建会话。
//
// 所以合并规则必须由所有载体共用，不允许各调用点自行 append。

// responsesToolIdentityKey 是"两条工具声明是不是同一个工具"的唯一判定。
//
// 身份 = 类型 + 名字。namespace 工具以 namespace 名为 name（子工具在 children 里），
// 内置工具大多无名字、按类型唯一，故空名字的内置工具天然合并为一。
//
// 只比身份不比 schema：向上游提交两份同名定义本身就是无效请求，能落地的只有一份；
// 真要报冲突由下游的显式检查负责（见 responsesToolsToChatTools）。
func responsesToolIdentityKey(toolType, name string) string {
	return strings.ToLower(strings.TrimSpace(toolType)) + "|" + strings.TrimSpace(name)
}

// responsesToolIdentity 是 typed 路径（apicompat.ResponsesTool）的身份取值。
func responsesToolIdentity(tool ResponsesTool) string {
	return responsesToolIdentityKey(tool.Type, tool.Name)
}

func responsesToolIdentities(tools []ResponsesTool) map[string]struct{} {
	out := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		out[responsesToolIdentity(tool)] = struct{}{}
	}
	return out
}

// responsesToolIdentityOfRaw 是原始 JSON 路径（map[string]any）的身份取值，
// 与 typed 路径共用一个判定函数，避免两条路各自理解"什么是同一条工具"。
func responsesToolIdentityOfRaw(raw any) string {
	tool, ok := raw.(map[string]any)
	if !ok {
		return ""
	}
	return responsesToolIdentityKey(stringValue(tool["type"]), stringValue(tool["name"]))
}

// MergeDeclaredRawResponsesTools 把 extra 里的声明并进 base，按身份跨批去重，先见者为准。
//
// 只在**批之间**去重：extra 内部自身的重复原样保留，交给下游的冲突检查去显式报错，
// 静默保留首个会把客户端的定义错误藏起来。
// 返回合并结果，以及是否真的丢弃了重复项（供调用方决定要不要重写请求体）。
func MergeDeclaredRawResponsesTools(base, extra []any) ([]any, bool) {
	if len(extra) == 0 {
		return base, false
	}
	declared := make(map[string]struct{}, len(base)+len(extra))
	for _, raw := range base {
		declared[responsesToolIdentityOfRaw(raw)] = struct{}{}
	}
	merged := make([]any, 0, len(base)+len(extra))
	merged = append(merged, base...)
	dropped := false
	keep := make([]any, 0, len(extra))
	for _, raw := range extra {
		if _, dup := declared[responsesToolIdentityOfRaw(raw)]; dup {
			dropped = true
			continue
		}
		keep = append(keep, raw)
	}
	// 整批筛选完再登记身份，批内重复才能一起留下来。
	for _, raw := range keep {
		declared[responsesToolIdentityOfRaw(raw)] = struct{}{}
	}
	merged = append(merged, keep...)
	return merged, dropped
}
