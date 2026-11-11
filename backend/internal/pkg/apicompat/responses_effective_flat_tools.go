package apicompat

import (
	"fmt"
	"strings"
)

// ResponsesFlatToolSet 是"真正下发给上游"的工具集唯一形态。
//
// 同一份 Responses 请求里的工具可以出现在三个位置：顶层 tools、
// input[].additional_tools（codex 新版把运行时工具放这里，顶层 tools 常为空）、
// 以及 namespace 分组的子工具。上游只接受扁平的 function/custom 声明，
// 所以**所有出口都必须经过本函数**取工具集，不能各自读 req.Tools ——
// 各读各的正是 2026-11-06 工具整批丢失（模型无工具可用后乱试触发 CUR-EXEC-02）
// 的根因。
type ResponsesFlatToolSet struct {
	// Tools 是摊平后的声明列表：namespace 子工具已按 FlattenNamespaceToolName 改名。
	Tools []ResponsesTool
	// CustomTools / FunctionTools 是工具名字集合。
	//   - CustomTools 记的是**裸名**（顶层 custom 名，以及 namespace 内 custom 子工具的原名）：
	//     回程判定 custom_tool_call 时用的是客户端声明的名字，不是上游的摊平名。
	//   - FunctionTools 记的是**摊平名**：上游按摊平名回调，判定普通 function_call 直接比对它。
	CustomTools   map[string]bool
	FunctionTools map[string]bool
	// NamespaceTools 记录 摊平名 → (namespace, 子工具原名)，
	// 供回程把上游的调用还原成带 namespace 的形式。
	NamespaceTools map[string]NamespacedToolName
	// NamespaceCustomTools 标记哪些摊平名对应的是 namespace 内的 **custom** 子工具
	// （而不是 function）。没有这一位，回程只能把摊平名当普通 function_call 交付，
	// codex 收到 function_call 而不是 custom_tool_call 会直接不认这个工具调用。
	NamespaceCustomTools map[string]bool
	// ToolSearch 表示声明了服务端 tool_search。
	ToolSearch bool
}

// EffectiveResponsesFlatTools 解析出本次请求真正生效、且已摊平的工具集。
// 这是"工具集"的唯一事实源：顶层 tools + additional_tools 提升 + namespace 摊平。
func EffectiveResponsesFlatTools(req *ResponsesRequest) (ResponsesFlatToolSet, error) {
	effective, err := EffectiveResponsesTools(req)
	if err != nil {
		return ResponsesFlatToolSet{}, err
	}
	return FlattenResponsesToolList(effective)
}

// FlattenResponsesToolList 把 namespace 分组的子工具摊平成顶层声明。
//
// 与 chat 桥的 FlattenResponsesNamespaces 同一条命名规则（FlattenNamespaceToolName）
// 和同一套冲突语义，区别只在于输入是类型化列表而不是 map。
//
// 子工具类型 function 与 custom 都摊平：只摊 function 会让 codex 的
// custom exec（custom + grammar）永远下不去，模型也就永远调不到它。
func FlattenResponsesToolList(tools []ResponsesTool) (ResponsesFlatToolSet, error) {
	set := ResponsesFlatToolSet{NamespaceTools: make(map[string]NamespacedToolName)}

	topLevel := make(map[string]bool, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Name)
		if (tool.Type == "function" || tool.Type == "custom") && name != "" {
			topLevel[name] = true
		}
	}

	flat := make([]ResponsesTool, 0, len(tools))
	for _, tool := range tools {
		if strings.TrimSpace(tool.Type) != "namespace" {
			flat = append(flat, tool)
			continue
		}
		namespace := strings.TrimSpace(tool.Name)
		if namespace == "" {
			continue
		}
		children := tool.Tools
		if len(children) == 0 {
			children = tool.Children
		}
		for _, child := range children {
			childType := strings.TrimSpace(child.Type)
			childName := strings.TrimSpace(child.Name)
			if childName == "" || (childType != "function" && childType != "custom") {
				continue
			}
			name := FlattenNamespaceToolName(namespace, childName)
			if topLevel[name] {
				return ResponsesFlatToolSet{}, fmt.Errorf(
					"namespace tool %q/%q flattens to %q which conflicts with a top-level tool of the same name; this upstream cannot disambiguate them, rename one of the tools",
					namespace, childName, name)
			}
			if prev, exists := set.NamespaceTools[name]; exists && (prev.Namespace != namespace || prev.Name != childName) {
				return ResponsesFlatToolSet{}, fmt.Errorf(
					"namespace tools %q/%q and %q/%q both flatten to %q; this upstream cannot disambiguate them, rename one of the tools",
					prev.Namespace, prev.Name, namespace, childName, name)
			}
			// 完全相同的 (namespace, 子工具) 重复声明不构成歧义：去重后只下发一份。
			// （旧 chat 桥的 namespaceChildrenToChatTools 就是这个语义，收敛到唯一事实源
			//   时不能把它丢掉——丢掉会让出站 f4 里出现两份同名工具定义。）
			if prev, exists := set.NamespaceTools[name]; exists && prev.Namespace == namespace && prev.Name == childName {
				continue
			}
			set.NamespaceTools[name] = NamespacedToolName{
				Namespace: namespace,
				Name:      childName,
				// IsCustom 必须与 NamespaceToolNames（chatcompletions_responses_bridge.go）
				// 同源填法：**namespace 内的 custom 子工具在这里也要标出来**。
				//
				// 漏填的后果（2026-11-12 生产实证）：交付侧判定（customToolCallName 读
				// origin.IsCustom）把 namespace 内的 custom 判成 function → 交付
				// function_call；而登记侧（service.cursorCustomToolCallName 读另一个
				// NamespaceCustomTools map）判成 custom → 登记 custom_tool_call。
				// 两侧结论不一致 → codex 续轮时按 function 语义回放，本站按 custom 校验
				// → result_type_mismatch → CUR-RES-02（codex 外审通道全线失败）。
				//
				// 同一份 tools 派生出的两份映射（NamespaceToolNames / 本函数）必须逐项一致。
				IsCustom: childType == "custom",
			}
			if childType == "custom" {
				if set.NamespaceCustomTools == nil {
					set.NamespaceCustomTools = make(map[string]bool)
				}
				set.NamespaceCustomTools[name] = true
			}
			child.Type = childType
			child.Name = name
			flat = append(flat, child)
		}
	}

	set.Tools = flat
	// 名字集合按"回程怎么用"来记，而不是统一记摊平名：
	//   - function 按摊平名比对（上游就是按这个名字回调的）；
	//   - custom 按裸名记（回程要还原成客户端的 custom_tool_call 名字），
	//     摊平名到裸名的关系另由 NamespaceTools / NamespaceCustomTools 承载。
	for _, tool := range flat {
		name := strings.TrimSpace(tool.Name)
		switch strings.TrimSpace(tool.Type) {
		case "custom":
			if name != "" {
				if set.CustomTools == nil {
					set.CustomTools = make(map[string]bool)
				}
				set.CustomTools[name] = true
			}
		case "function":
			if name != "" {
				if set.FunctionTools == nil {
					set.FunctionTools = make(map[string]bool)
				}
				set.FunctionTools[name] = true
			}
		case "tool_search":
			set.ToolSearch = true
		}
	}
	// namespace 内的 custom 子工具：把**裸名**补进 custom 集合（摊平名已在上面被当成裸名记过一次，
	// 需要替换成原文的名字），并标记该摊平名的类型，供回程区分 custom / function。
	for flatName, origin := range set.NamespaceTools {
		if set.NamespaceCustomTools[flatName] {
			delete(set.CustomTools, flatName)
			set.CustomTools[origin.Name] = true
		}
	}
	if len(set.NamespaceTools) == 0 {
		set.NamespaceTools = nil
	}
	if len(set.NamespaceCustomTools) == 0 {
		set.NamespaceCustomTools = nil
	}
	return set, nil
}
