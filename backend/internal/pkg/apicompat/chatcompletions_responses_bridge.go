package apicompat

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	toolOutputMediaMarker      = "[Tool output media moved to the following user message]"
	toolOutputMediaAttribution = "[Tool output media for call %s]"
)

type toolOutputMediaByCallID map[string][]ChatContentPart

// ResponsesToChatOptions carries optional hooks for
// ResponsesToChatCompletionsRequestWithOptions. All fields are optional; a nil
// *ResponsesToChatOptions behaves exactly like ResponsesToChatCompletionsRequest.
type ResponsesToChatOptions struct {
	// ReasoningContentByID looks up the cached reasoning text for a reasoning
	// item id. Codex histories may carry reasoning items with no plaintext
	// summary (empty summary + opaque encrypted_content, e.g. after remote
	// compaction); DeepSeek's thinking mode rejects such histories with 400
	// "The `reasoning_content` in the thinking mode must be passed back to the
	// API". The gateway caches the reasoning text it streamed under the item
	// id, so the lookup restores the reasoning_content the client can no
	// longer provide. Return "" on a miss. A nil lookup keeps the original
	// behavior.
	ReasoningContentByID func(itemID string) string
}

// ResponsesToChatCompletionsRequest converts a Responses API request into a
// Chat Completions request for upstreams that only implement
// /v1/chat/completions.
func ResponsesToChatCompletionsRequest(req *ResponsesRequest) (*ChatCompletionsRequest, error) {
	return ResponsesToChatCompletionsRequestWithOptions(req, nil)
}

// ResponsesToChatCompletionsRequestWithOptions is ResponsesToChatCompletionsRequest
// with optional hooks (see ResponsesToChatOptions).
func ResponsesToChatCompletionsRequestWithOptions(req *ResponsesRequest, opts *ResponsesToChatOptions) (*ChatCompletionsRequest, error) {
	if req == nil {
		return nil, fmt.Errorf("responses request is nil")
	}

	messages, err := responsesInputToChatMessagesWithOptions(req.Instructions, req.Input, opts)
	if err != nil {
		return nil, err
	}

	out := &ChatCompletionsRequest{
		Model:               req.Model,
		Messages:            messages,
		MaxCompletionTokens: req.MaxOutputTokens,
		Temperature:         req.Temperature,
		TopP:                req.TopP,
		Stream:              req.Stream,
		ServiceTier:         req.ServiceTier,
		ParallelToolCalls:   req.ParallelToolCalls,
	}
	if req.Reasoning != nil {
		out.ReasoningEffort = req.Reasoning.Effort
	}
	effectiveTools, err := EffectiveResponsesTools(req)
	if err != nil {
		return nil, err
	}
	if len(effectiveTools) > 0 {
		tools, err := responsesToolsToChatTools(effectiveTools)
		if err != nil {
			return nil, err
		}
		out.Tools = tools
	}
	// tools 全部被丢弃（如仅含 web_search/image_generation 等服务端工具）时不再转发
	// tool_choice：上游会拒绝 "'tool_choice' is only allowed when 'tools' are specified"。
	// 指向被丢弃工具的选择项同理（见 responsesToolChoiceToChatToolChoice）。
	if len(out.Tools) > 0 && len(req.ToolChoice) > 0 {
		declared := make(map[string]bool, len(out.Tools))
		for _, tool := range out.Tools {
			if tool.Function != nil {
				declared[tool.Function.Name] = true
			}
			if strings.EqualFold(strings.TrimSpace(tool.Type), "x_search") {
				declared["x_search"] = true
			}
		}
		if tc := responsesToolChoiceToChatToolChoice(req.ToolChoice, declared); len(tc) > 0 {
			out.ToolChoice = tc
		}
	}
	if req.Text != nil {
		out.ResponseFormat = responsesTextFormatToChatResponseFormat(req.Text.Format)
	}

	return out, nil
}

// EffectiveResponsesTools returns every client-executable tool declared by a
// Responses request. Newer Codex clients place their runtime tools in an
// input item shaped as {"type":"additional_tools","tools":[...]} instead of
// the top-level tools field. Chat-only upstreams must receive both forms.
//
// 顶层 tools 与 input 里的 additional_tools 是**同一份工具集的两种声明站点**，
// 不是两个工具集：同一条工具在两处都出现时仍然只有一个工具。合并按工具身份
// （见 responsesToolIdentity）跨站点去重，先见者为准。
//
// 只在站点之间去重，**不吞站点内部的重复**：同一份清单里出现两个同名可执行工具
// 是客户端真实歧义，必须留给下游的冲突检查显式报错（见 responsesToolsToChatTools），
// 静默保留首个会把客户端的定义错误藏起来。
func EffectiveResponsesTools(req *ResponsesRequest) ([]ResponsesTool, error) {
	if req == nil {
		return nil, nil
	}

	tools := append([]ResponsesTool(nil), req.Tools...)
	inputRaw := bytesTrimSpace(req.Input)
	if len(inputRaw) == 0 || string(inputRaw) == "null" || inputRaw[0] != '[' {
		return tools, nil
	}

	// 已声明身份集合：跨批次累积。批**内部**重复不查集合，故原样保留给下游冲突检查。
	declared := responsesToolIdentities(tools)
	appendDeclared := func(incoming []ResponsesTool) {
		keep := make([]ResponsesTool, 0, len(incoming))
		for _, tool := range incoming {
			if _, dup := declared[responsesToolIdentity(tool)]; dup {
				continue
			}
			keep = append(keep, tool)
		}
		// 整批筛选完再登记身份，批内重复才能一起留下来。
		for _, tool := range keep {
			declared[responsesToolIdentity(tool)] = struct{}{}
		}
		tools = append(tools, keep...)
	}

	var items []json.RawMessage
	if err := json.Unmarshal(inputRaw, &items); err != nil {
		return nil, fmt.Errorf("parse responses input for additional tools: %w", err)
	}
	for _, raw := range items {
		raw = bytesTrimSpace(raw)
		if len(raw) == 0 || raw[0] != '{' {
			continue
		}
		var discriminator struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &discriminator); err != nil {
			return nil, fmt.Errorf("parse responses additional tools item: %w", err)
		}
		if !IsResponsesInputDeclaration(discriminator.Type) {
			continue
		}
		var item struct {
			Tools []ResponsesTool `json:"tools"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, fmt.Errorf("parse responses additional tools item: %w", err)
		}
		appendDeclared(item.Tools)
	}

	// A completed client tool search can introduce tools for the remainder of
	// the turn. Promote them before lowering to ChatCompletions, using the same
	// dedupe, conflict, namespace, and custom-tool rules as the native adapter.
	toolsRaw, err := json.Marshal(tools)
	if err != nil {
		return nil, fmt.Errorf("encode responses tools for discovery promotion: %w", err)
	}
	var rawTools, rawInput []any
	if err := json.Unmarshal(toolsRaw, &rawTools); err != nil {
		return nil, fmt.Errorf("decode responses tools for discovery promotion: %w", err)
	}
	if err := json.Unmarshal(inputRaw, &rawInput); err != nil {
		return nil, fmt.Errorf("parse responses input for discovery promotion: %w", err)
	}
	promoted, err := promotedResponsesToolSearchDiscoveries(rawTools, rawInput)
	if err != nil {
		return nil, err
	}
	if len(promoted) > 0 {
		promotedRaw, err := json.Marshal(promoted)
		if err != nil {
			return nil, fmt.Errorf("encode promoted responses tools: %w", err)
		}
		var discovered []ResponsesTool
		if err := json.Unmarshal(promotedRaw, &discovered); err != nil {
			return nil, fmt.Errorf("decode promoted responses tools: %w", err)
		}
		appendDeclared(discovered)
	}
	return tools, nil
}

// 身份判定与合并规则见 responses_tool_declaration_merge.go（typed / raw 两条路共用）。

// CustomToolNames 收集 Responses 请求中 custom/freeform 工具的名字。chat 桥回程时
// 需要据此把模型对这些工具的调用还原为 custom_tool_call 项（codex 只按该类型路由）。
func CustomToolNames(tools []ResponsesTool) map[string]bool {
	var out map[string]bool
	for _, tool := range tools {
		if tool.Type == "custom" && tool.Name != "" {
			if out == nil {
				out = make(map[string]bool)
			}
			out[tool.Name] = true
		}
	}
	return out
}

// FunctionToolNames collects explicitly declared top-level function tools.
func FunctionToolNames(tools []ResponsesTool) map[string]bool {
	var out map[string]bool
	for _, tool := range tools {
		if tool.Type == "function" && tool.Name != "" {
			if out == nil {
				out = make(map[string]bool)
			}
			out[tool.Name] = true
		}
	}
	return out
}

// NamespacedToolName 记录 namespace 子工具的原始归属（命名空间 + 裸子工具名）。
type NamespacedToolName struct {
	Namespace string
	Name      string
	// IsCustom 标记该子工具是 namespace 内的 **custom 类型**（而不是 function）。
	// 回程判定 custom_tool_call 时靠这一位把 namespace 内的 custom 还原成裸名并判
	// custom_tool_call；function 子工具保持「真名优先、别名不生效」的原语义，按
	// 带 namespace 字段的普通 function_call 交付。详见 customToolCallName。
	IsCustom bool
}

// newNamespacedToolName 构造 namespace 子工具的映射项。
//
// 「namespace 内子工具是不是 custom」这条判定**只此一处**：
// 本函数与 FlattenResponsesToolList 都调它，两处派生出的映射逐项一致。
//
// 2026-11-12：此前 FlattenResponsesToolList 自己写了一遍填法且**漏了 IsCustom**，
// 与本函数结论相反 → 交付侧判 function、登记侧判 custom → codex 续轮
// result_type_mismatch → CUR-RES-02（根因修复见提交 14662d995b）。
func newNamespacedToolName(namespace, name, childType string) NamespacedToolName {
	return NamespacedToolName{
		Namespace: namespace,
		Name:      name,
		IsCustom:  childType == "custom",
	}
}

// NamespaceToolNames 收集 Responses 请求中 namespace 子工具的摊平名 →（namespace,
// 子工具名）映射。chat 桥回程时需据此把模型对摊平工具的调用还原为带 namespace 字段
// 的 function_call 项：codex 按 namespace+name 路由，平铺名会被判为 unsupported
// call；摊平名超长时带截断哈希（见 FlattenNamespaceToolName），无法按字符串切分还原。
// 摊平名撞名的请求已在转换阶段被 FlattenResponsesToolList 显式拒绝，
// 此处映射不存在歧义。
func NamespaceToolNames(tools []ResponsesTool) map[string]NamespacedToolName {
	var out map[string]NamespacedToolName
	for _, tool := range tools {
		if tool.Type != "namespace" || tool.Name == "" {
			continue
		}
		children := tool.Tools
		if len(children) == 0 {
			children = tool.Children
		}
		for _, child := range children {
			if (child.Type != "function" && child.Type != "custom") || child.Name == "" {
				continue
			}
			if out == nil {
				out = make(map[string]NamespacedToolName)
			}
			out[FlattenNamespaceToolName(tool.Name, child.Name)] =
				newNamespacedToolName(tool.Name, child.Name, child.Type)
		}
	}
	return out
}

// 与 NamespaceToolNames 的关系：NamespaceToolNames 说「这个摊平名属于哪个 namespace 的
// 哪个子工具」，本集合说「这个摊平名的子工具是 custom 类型」——
// 回程要靠这一位把 namespace 内的 custom 判成 custom_tool_call 并还原成裸名。
// namespace 内 function 子工具不在本集合里。
//
// 为什么需要这一位：customToolCallName 的判定顺序里，namespace 命中即「不是 custom」
// （那是为 function 子工具定的语义）。若不区分 custom 与 function，namespace 内的
// custom 子工具会被一起判成普通 function_call —— codex 只认 custom_tool_call。
// cursor 侧的 cursorCustomToolCallName 用同样的四集合结构，见
// cursor_execution_service.go:3589。
//
// 本函数的回程判定已收敛到 NamespacedToolName.IsCustom 字段（见 customToolCallName），
// 故此处直接基于 NamespaceToolNames 的结果筛出 custom 子集，口径与下游 FlattenResponsesToolList
// 的 NamespaceCustomTools 一致（都收 function + custom，都按 FlattenNamespaceToolName 摊平）。
// customToolCallName restores both the exact downgraded custom-tool name and
// namespace-prefixed aliases that chat models sometimes infer from neighboring
// flattened namespace tools (for example functions__exec beside
// functions__wait). A real declared namespace child always owns its flattened
// name, and ambiguous aliases are left as ordinary function calls.
//
// 判定顺序与 cursor 侧的 cursorCustomToolCallName 同源：namespace 子工具占用了这个
// 摊平名时，是 custom 子工具就还原成裸名并判 custom_tool_call，是 function 子工具
// 则保持「真名优先、别名不生效」的原语义（返回 false，按普通 function_call 交付）。
// 这一位由 NamespacedToolName.IsCustom 承载（避免改动导出 API 签名，也避免动
// 本包以外的 cursor 侧代码）。
// CustomToolCallName 判定给定摊平名是否为 custom 工具，并还原其裸名。
//
// 这是「摊平名 → 是否 custom_tool_call」判定的**唯一实现**：
//   - chat 桥回程（announceChatToolItem）用它判交付项类型；
//   - cursor 协议路径（service.cursorResponsesDeliveredToolCall）登记调用语义时用它。
//
// 两侧必须同源：交付成 function_call 而登记成 custom_tool_call（或反之）会让
// codex 续轮时类型不匹配 → result_type_mismatch → CUR-RES-02。
//
// 2026-11-12 实证：cursor 侧曾维护一份**复制实现**（读另一份 NamespaceCustomTools
// map），与本实现（读 origin.IsCustom）结论不一致 —— 同一规则两处实现 = 认知分裂。
// 复制实现已删除，本函数是唯一口径。
func CustomToolCallName(name string, customTools, functionTools map[string]bool, namespaceTools map[string]NamespacedToolName) (string, bool) {
	return customToolCallName(name, customTools, functionTools, namespaceTools)
}

func customToolCallName(name string, customTools, functionTools map[string]bool, namespaceTools map[string]NamespacedToolName) (string, bool) {
	if functionTools[name] {
		return "", false
	}
	if customTools[name] {
		return name, true
	}
	// namespace 子工具占用了这个摊平名：是 custom 子工具就还原成裸名并判 custom_tool_call，
	// 是 function 子工具则保持"真名优先、别名不生效"的原语义。
	if origin, ok := namespaceTools[name]; ok {
		if origin.IsCustom {
			return origin.Name, true
		}
		return "", false
	}
	match := ""
	for customName := range customTools {
		for _, namespaceTool := range namespaceTools {
			if FlattenNamespaceToolName(namespaceTool.Namespace, customName) != name {
				continue
			}
			if match != "" && match != customName {
				return "", false
			}
			match = customName
		}
	}
	return match, match != ""
}

func customNameForStreamTool(state *ChatCompletionsToResponsesStreamState, name string) string {
	if customName, ok := customToolCallName(name, state.CustomTools, state.FunctionTools, state.NamespaceTools); ok {
		return customName
	}
	return name
}

// HasToolSearchTool 判断 Responses 请求是否声明了 tool_search 服务端工具。chat 桥
// 回程时需据此把模型对代理工具的调用还原为 tool_search_call 项：codex 只在该项类型
// 且 execution=client 时执行 tool search，同名 function_call 会因 payload 不匹配
// 触发 fatal 中止整个 turn。
func HasToolSearchTool(tools []ResponsesTool) bool {
	for _, tool := range tools {
		if tool.Type == "tool_search" {
			return true
		}
	}
	return false
}

// responsesInputToChatMessages converts a Responses request's instructions +
// input[] into Chat Completions messages. It is a three-stage pipeline:
//
//	parse   — instructions become a system message; input[] is split into items
//	build   — buildChatMessagesFromItems walks items, attaching reasoning to the
//	          assistant message that produced a tool call, merging parallel tool
//	          calls into one assistant message, folding system/developer messages
//	          into the leading system message, and skipping item types that have
//	          no Chat equivalent
//	normalize — normalizeChatMessages enforces the invariants DeepSeek requires
//
// The build + normalize split keeps every protocol rule in one place rather than
// scattered across per-item cases, and makes unknown future codex item types
// fail safe instead of leaking into the upstream request.
func responsesInputToChatMessages(instructions string, inputRaw json.RawMessage) ([]ChatMessage, error) {
	return responsesInputToChatMessagesWithOptions(instructions, inputRaw, nil)
}

// responsesInputToChatMessagesWithOptions is responsesInputToChatMessages with
// optional hooks (see ResponsesToChatOptions).
func responsesInputToChatMessagesWithOptions(instructions string, inputRaw json.RawMessage, opts *ResponsesToChatOptions) ([]ChatMessage, error) {
	var messages []ChatMessage
	if strings.TrimSpace(instructions) != "" {
		content, _ := json.Marshal(instructions)
		messages = append(messages, ChatMessage{Role: "system", Content: content})
	}

	inputRaw = bytesTrimSpace(inputRaw)
	if len(inputRaw) == 0 || string(inputRaw) == "null" {
		return messages, nil
	}

	// Bare string input is a single user turn.
	var inputText string
	if err := json.Unmarshal(inputRaw, &inputText); err == nil {
		content, _ := json.Marshal(inputText)
		messages = append(messages, ChatMessage{Role: "user", Content: content})
		return messages, nil
	}

	var rawItems []json.RawMessage
	if err := json.Unmarshal(inputRaw, &rawItems); err != nil {
		return nil, fmt.Errorf("parse responses input: %w", err)
	}

	built, mediaByCallID, err := buildChatMessagesFromItems(messages, rawItems, opts)
	if err != nil {
		return nil, err
	}
	normalized := normalizeChatMessagesWithToolOutputMedia(built, mediaByCallID)
	return normalizeResponsesDerivedChatMessageRoles(normalized), nil
}

// normalizeResponsesDerivedChatMessageRoles rewrites the Chat Completions
// message list produced by the Responses bridge so that strict upstreams which
// only accept system content at the very start of the conversation accept it.
//
// The bridge turns the Responses `instructions` field and every role:"developer"
// item into a system message. Codex always sends both instructions and a leading
// developer item, and it also injects developer notices into the middle of the
// message history (for example when the user switches models). Forwarded as-is
// that produces two leading system messages and mid-conversation system
// messages, which Qwen-family upstreams reject with
// "System message must be at the beginning." (HTTP 400).
//
// Leading system/developer messages are therefore merged into a single leading
// system message, while later ones keep their position but are downgraded to
// user messages so the same text still reaches the model.
func normalizeResponsesDerivedChatMessageRoles(messages []ChatMessage) []ChatMessage {
	isInstructionRole := func(role string) bool {
		return role == "system" || role == "developer"
	}

	leading := 0
	for leading < len(messages) && isInstructionRole(messages[leading].Role) {
		leading++
	}

	out := make([]ChatMessage, 0, len(messages))
	switch leading {
	case 0:
		// No leading instructions, nothing to merge.
	case 1:
		// A single leading prompt is already valid; keep its content byte for
		// byte instead of round-tripping it through the text merge below.
		out = append(out, messages[0])
	default:
		merged := make([]string, 0, leading)
		for _, m := range messages[:leading] {
			if text := strings.TrimSpace(chatMessageContentText(m.Content)); text != "" {
				merged = append(merged, text)
			}
		}
		if len(merged) > 0 {
			content, _ := json.Marshal(strings.Join(merged, "\n\n"))
			out = append(out, ChatMessage{Role: "system", Content: content})
		}
	}

	for _, m := range messages[leading:] {
		if isInstructionRole(m.Role) {
			m.Role = "user"
		}
		out = append(out, m)
	}
	return out
}

// buildChatMessagesFromItems walks the Responses input items and appends the
// corresponding Chat messages.
func buildChatMessagesFromItems(messages []ChatMessage, rawItems []json.RawMessage, opts *ResponsesToChatOptions) ([]ChatMessage, toolOutputMediaByCallID, error) {
	// pendingReasoning holds the reasoning text from a reasoning item until the
	// assistant message it belongs to is emitted. DeepSeek's thinking mode
	// requires the reasoning_content that produced a tool call to be passed back
	// on that assistant message; dropping it yields a 400. It only survives
	// across an assistant message (so a following tool call in the same turn
	// still receives it); any other role ends the thinking span.
	var pendingReasoning string
	// lastTurnReasoning is the most recent reasoning text of the current turn,
	// surviving tool outputs. DeepSeek emits reasoning only once per turn, so
	// chained tool calls (reasoning → call A → output A → call B) leave call B's
	// assistant message without reasoning_content and DeepSeek 400s the history;
	// replaying the turn's reasoning on B's message satisfies the contract. Only
	// a user-side item ends the turn and clears it.
	var lastTurnReasoning string
	// hoistedSystem collects the text of every system/developer message found in
	// input[] so it can be folded into the leading system message below.
	// Strict chat upstreams only accept system messages at the head of the
	// conversation — the self-hosted vLLM behind the kimi accounts answers
	// anything else with 400 "System message must be at the beginning." — and
	// codex injects a developer notice in the middle of tool execution ("Approved
	// command prefix saved", see the golden sample at
	// chatcompletions_responses_request_invariants_test.go), which the in-place
	// append would strand between an assistant tool_calls message and its tool
	// reply. Merging mirrors what the codex path already does for system items
	// (extractSystemMessagesFromInput promotes their text into instructions).
	var hoistedSystem []string
	mediaByCallID := make(toolOutputMediaByCallID)
	invalidFunctionCallIDs := make(map[string]struct{})
	invalidEmptyFunctionCallOutputs := 0

	reasoningForAssistant := func() string {
		if pendingReasoning != "" {
			return pendingReasoning
		}
		return lastTurnReasoning
	}

	for _, raw := range rawItems {
		raw = bytesTrimSpace(raw)
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}

		var item map[string]json.RawMessage
		if err := json.Unmarshal(raw, &item); err != nil {
			var text string
			if textErr := json.Unmarshal(raw, &text); textErr == nil {
				content, _ := json.Marshal(text)
				messages = append(messages, ChatMessage{Role: "user", Content: content})
				pendingReasoning = ""
				lastTurnReasoning = ""
				continue
			}
			return nil, nil, fmt.Errorf("parse responses input item: %w", err)
		}

		role := chatCompletionsBridgeRole(rawString(item["role"]))
		itemType := rawString(item["type"])
		switch itemType {
		case "reasoning":
			if txt := extractResponsesReasoningText(item); txt != "" {
				pendingReasoning = txt
			} else if opts != nil && opts.ReasoningContentByID != nil {
				// No plaintext summary (encrypted-only reasoning, e.g. after codex
				// remote compaction): fall back to the gateway-side cache keyed
				// by the reasoning item id, which always round-trips in history.
				if id := rawString(item["id"]); id != "" {
					if cached := opts.ReasoningContentByID(id); cached != "" {
						pendingReasoning = cached
					}
				}
			}
			if pendingReasoning != "" {
				lastTurnReasoning = pendingReasoning
			}
			continue
		case "function_call":
			arguments := rawString(item["arguments"])
			if strings.TrimSpace(arguments) == "" {
				arguments = "{}"
			}
			callID := rawString(item["call_id"])
			if !json.Valid([]byte(arguments)) {
				// A previous streamed turn can leave a truncated function_call in
				// Codex history (for example after an upstream SSE parse failure or
				// an output-limit interruption). Do not forward that item to a
				// Chat Completions provider, which rejects the entire request. Its
				// matching output is skipped below as well, allowing the next user
				// turn to self-heal instead of repeatedly replaying the poison.
				if callID != "" {
					invalidFunctionCallIDs[callID] = struct{}{}
				} else {
					invalidEmptyFunctionCallOutputs++
				}
				pendingReasoning = ""
				continue
			}
			name := rawString(item["name"])
			// namespace 子工具的历史调用带 namespace 字段，需与请求方向的摊平
			// 命名（FlattenNamespaceToolName）保持一致。
			if ns := rawString(item["namespace"]); ns != "" {
				name = FlattenNamespaceToolName(ns, name)
			}
			toolCall := ChatToolCall{
				ID:   callID,
				Type: "function",
				Function: ChatFunctionCall{
					Name:      name,
					Arguments: arguments,
				},
			}
			messages = appendAssistantToolCall(messages, toolCall, reasoningForAssistant())
			pendingReasoning = ""
			continue
		case "tool_search_call":
			// tool_search 调用的 arguments 是 JSON 对象（如 {"query": ...}），
			// 原文即为降级 function 调用的 arguments 字符串。
			arguments := strings.TrimSpace(string(bytesTrimSpace(item["arguments"])))
			if s := rawString(item["arguments"]); s != "" {
				arguments = s
			}
			if arguments == "" || arguments == "null" {
				arguments = "{}"
			}
			toolCall := ChatToolCall{
				ID:   rawString(item["call_id"]),
				Type: "function",
				Function: ChatFunctionCall{
					Name:      toolSearchProxyName,
					Arguments: arguments,
				},
			}
			messages = appendAssistantToolCall(messages, toolCall, reasoningForAssistant())
			pendingReasoning = ""
			continue
		case "custom_tool_call":
			// custom/freeform 工具的历史调用：input 自由文本包进降级 function 工具
			// 的 {"input": ...} 参数，与请求方向的工具降级（customToolInputSchema）
			// 保持一致，模型才能把历史与当前工具定义对上。
			arguments, _ := json.Marshal(map[string]string{"input": rawString(item["input"])})
			toolCall := ChatToolCall{
				ID:   rawString(item["call_id"]),
				Type: "function",
				Function: ChatFunctionCall{
					Name:      rawString(item["name"]),
					Arguments: string(arguments),
				},
			}
			messages = appendAssistantToolCall(messages, toolCall, reasoningForAssistant())
			pendingReasoning = ""
			continue
		case "function_call_output", "custom_tool_call_output", "tool_search_output":
			outputRaw := bytesTrimSpace(item["output"])
			if itemType == "tool_search_output" && (len(outputRaw) == 0 || string(outputRaw) == "null") {
				// Newer clients return discoveries in tools[] without a separate
				// output field. Keep that useful result in Chat tool history.
				outputRaw = bytesTrimSpace(item["tools"])
			}
			callID := rawString(item["call_id"])
			if callID == "" && invalidEmptyFunctionCallOutputs > 0 {
				invalidEmptyFunctionCallOutputs--
				pendingReasoning = ""
				continue
			}
			if _, skipped := invalidFunctionCallIDs[callID]; skipped {
				pendingReasoning = ""
				continue
			}
			delete(mediaByCallID, callID)

			outputText, media, rewritten := extractToolOutputMedia(outputRaw)
			if rewritten {
				if callID != "" {
					mediaByCallID[callID] = media
				}
			} else {
				outputText = rawString(outputRaw)
				if outputText == "" && len(outputRaw) > 0 && string(outputRaw) != "null" && string(outputRaw) != `""` {
					// 对象/数组形式的输出（如 tool_search 的结果列表）整体字符串化。
					outputText = string(outputRaw)
				}
			}
			content, _ := json.Marshal(outputText)
			messages = append(messages, ChatMessage{
				Role:       "tool",
				ToolCallID: callID,
				Content:    content,
			})
			pendingReasoning = ""
			continue
		case "agent_message":
			// Codex multi_agent_v2 用 agent_message 在父线程与子智能体之间传递任务和回复：
			// input_text 是信封（消息类型、任务名、发送者），正文放在 encrypted_content 片段里
			// （自定义 provider 下为明文）。chat 上游没有对应条目，按原顺序拼成一条 user 消息，
			// 否则子智能体收不到任务却仍返回 200。
			text := agentMessageText(item["content"])
			if text == "" {
				pendingReasoning = ""
				continue
			}
			content, _ := json.Marshal(text)
			messages = append(messages, ChatMessage{Role: "user", Content: content})
			pendingReasoning = ""
			lastTurnReasoning = ""
			continue
		case "input_text", "text":
			content, _ := json.Marshal(rawString(item["text"]))
			messages = append(messages, ChatMessage{Role: "user", Content: content})
			pendingReasoning = ""
			lastTurnReasoning = ""
			continue
		case "input_image":
			content, err := chatContentFromSingleResponsesPart(itemType, item)
			if err != nil {
				return nil, nil, err
			}
			messages = append(messages, ChatMessage{Role: "user", Content: content})
			pendingReasoning = ""
			lastTurnReasoning = ""
			continue
		}

		// Only genuine message items become chat messages. Codex emits other
		// Responses item types with no Chat equivalent (web_search_call,
		// local_shell_call, file_search_call, ...). Converting them via the
		// generic path would insert a spurious message between an assistant
		// tool_calls message and its tool reply, which DeepSeek rejects
		// ("insufficient tool messages following tool_calls message"). Skip them.
		if itemType != "" && itemType != "message" {
			pendingReasoning = ""
			continue
		}

		content := item["content"]
		if len(bytesTrimSpace(content)) == 0 {
			if text := rawString(item["text"]); text != "" {
				content, _ = json.Marshal(text)
			}
		}
		chatContent, err := responsesContentToChatContent(content, role)
		if err != nil {
			return nil, nil, err
		}
		msg := ChatMessage{Role: role, Content: chatContent}
		// DeepSeek thinking mode requires the reasoning_content from a prior
		// reasoning-only / plain-text assistant turn to be passed back on its
		// assistant message; dropping it yields 400 "The `reasoning_content` in
		// the thinking mode must be passed back to the API" on the next turn.
		// A following function_call in the same turn still receives it because
		// appendAssistantToolCall merges into this message and only fills
		// ReasoningContent when it is still empty.
		if role == "assistant" {
			msg.ReasoningContent = reasoningForAssistant()
			pendingReasoning = ""
		} else {
			pendingReasoning = ""
			lastTurnReasoning = ""
		}
		if role == "system" {
			// developer is mapped to system by chatCompletionsBridgeRole, so both
			// roles land here. Keep the text, drop the position: a system message
			// is only legal at the head of the conversation. A notice with no
			// text carries no instruction and is dropped rather than left to
			// strand the conversation.
			if text := chatMessageContentText(chatContent); strings.TrimSpace(text) != "" {
				hoistedSystem = append(hoistedSystem, text)
			}
			continue
		}
		messages = append(messages, msg)
	}

	return mergeLeadingSystemMessages(messages, hoistedSystem), mediaByCallID, nil
}

// maxMergedSystemTextBytes 是合并后 leading system 文本的字节上限。
//
// 无上限时的失效形态是静默的：多个 developer 通知按项数线性膨胀，且没有任何
// 报错——上游只拒绝位置错误的 system，不拒绝超长 system，于是请求照常成功、
// 语义却被悄悄改变。上限取 256 KiB：远大于任何真实客户端的 instructions +
// 通知总量（本仓内 codex 默认 instructions 为 KB 量级），又远小于任何上游的
// system 长度限制与 1M context 窗口，足以在撞限前截断。
const maxMergedSystemTextBytes = 256 * 1024

// mergeLeadingSystemMessages folds hoisted system/developer text, in order, into
// the leading system message — the one built from the request's instructions —
// creating that message when the request carried none. An empty hoist list
// returns the conversation untouched, so a request without system/developer
// items converts exactly as before.
//
// Two safeguards apply to the fold, both required by production shapes:
//
//  1. Exact-equality dedup against the base text. Codex's
//     extractSystemMessagesFromInput promotes role=="system" item text into
//     instructions **and** (when omitPromoted==false, i.e.
//     text.format.type=="json_object") keeps the same item in input with its
//     role rewritten to developer. The promotion is a lossless copy, so the
//     hoisted text and the base text are byte-identical and the naive
//     concatenation yields "X\n\nX". Upstream vLLM does not reject that — it
//     only rejects *misplaced* system messages — so the duplication silently
//     changes semantics instead of erroring. Exact equality is the right
//     predicate here precisely because the duplicate is a copy, not a
//     paraphrase; genuinely distinct notices still concatenate.
//
//  2. A total length cap. Hoisted notices accumulate per item with no bound,
//     so a long tool session can grow the system message without limit. The
//     merged text is truncated to maxMergedSystemTextBytes on a rune boundary.
func mergeLeadingSystemMessages(messages []ChatMessage, hoisted []string) []ChatMessage {
	if len(hoisted) == 0 {
		return messages
	}

	leading := len(messages) > 0 && messages[0].Role == "system"
	base := ""
	if leading {
		base = chatMessageContentText(messages[0].Content)
	}
	texts := make([]string, 0, len(hoisted)+1)
	if strings.TrimSpace(base) != "" {
		texts = append(texts, base)
	}
	for _, text := range hoisted {
		// Dedup only against the base: the codex promotion copies the base text
		// verbatim, so equality is the signature of that copy rather than of two
		// independent notices that happen to read the same.
		if text == base {
			continue
		}
		texts = append(texts, text)
	}
	merged := truncateMergedSystemText(strings.Join(texts, "\n\n"))
	content, _ := json.Marshal(merged)
	if leading {
		messages[0].Content = content
		return messages
	}
	return append([]ChatMessage{{Role: "system", Content: content}}, messages...)
}

// truncateMergedSystemText clamps s to maxMergedSystemTextBytes, cutting on a
// rune boundary so the result is always valid UTF-8 (a byte-wise cut would emit
// a replacement char mid-instruction).
func truncateMergedSystemText(s string) string {
	if len(s) <= maxMergedSystemTextBytes {
		return s
	}
	cut := s[:maxMergedSystemTextBytes]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// agentMessageText 按原顺序拼接 agent_message 里 input_text 与 encrypted_content 片段的文本。
func agentMessageText(raw json.RawMessage) string {
	raw = bytesTrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, part := range parts {
		switch rawString(part["type"]) {
		case "input_text", "text":
			_, _ = b.WriteString(rawString(part["text"]))
		case "encrypted_content":
			_, _ = b.WriteString(rawString(part["encrypted_content"]))
		}
	}
	return b.String()
}

// extractToolOutputMedia rewrites only recognized image nodes. Media-free
// outputs return rewritten=false so the caller can preserve their original
// bytes and prompt-cache prefix.
func extractToolOutputMedia(outputRaw json.RawMessage) (string, []ChatContentPart, bool) {
	outputRaw = bytesTrimSpace(outputRaw)
	if len(outputRaw) == 0 || string(outputRaw) == "null" {
		return "", nil, false
	}

	var outputString string
	if err := json.Unmarshal(outputRaw, &outputString); err == nil {
		if isToolOutputImageDataURL(outputString) {
			return toolOutputMediaMarker, []ChatContentPart{toolOutputImagePart(outputString)}, true
		}

		nested, ok := decodeToolOutputJSON([]byte(outputString))
		if !ok {
			return "", nil, false
		}
		rewritten, media, changed := rewriteToolOutputMediaValue(nested)
		if !changed {
			return "", nil, false
		}
		encoded, err := json.Marshal(rewritten)
		if err != nil {
			return "", nil, false
		}
		return string(encoded), media, true
	}

	value, ok := decodeToolOutputJSON(outputRaw)
	if !ok {
		return "", nil, false
	}
	rewritten, media, changed := rewriteToolOutputMediaValue(value)
	if !changed {
		return "", nil, false
	}
	encoded, err := json.Marshal(rewritten)
	if err != nil {
		return "", nil, false
	}
	return string(encoded), media, true
}

func decodeToolOutputJSON(raw []byte) (any, bool) {
	if !json.Valid(raw) {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	return value, true
}

func rewriteToolOutputMediaValue(value any) (any, []ChatContentPart, bool) {
	switch typed := value.(type) {
	case []any:
		var media []ChatContentPart
		changed := false
		for i, item := range typed {
			rewritten, itemMedia, itemChanged := rewriteToolOutputMediaValue(item)
			if !itemChanged {
				continue
			}
			typed[i] = rewritten
			media = append(media, itemMedia...)
			changed = true
		}
		return typed, media, changed
	case map[string]any:
		if imageURL, ok := recognizedToolOutputImageURL(typed); ok {
			return map[string]any{
				"type": "input_text",
				"text": toolOutputMediaMarker,
			}, []ChatContentPart{toolOutputImagePart(imageURL)}, true
		}

		content, ok := typed["content"]
		if !ok {
			return typed, nil, false
		}
		rewritten, media, changed := rewriteToolOutputMediaValue(content)
		if !changed {
			return typed, nil, false
		}
		typed["content"] = rewritten
		return typed, media, true
	default:
		return value, nil, false
	}
}

func recognizedToolOutputImageURL(value map[string]any) (string, bool) {
	partType, _ := value["type"].(string)
	if partType != "input_image" && partType != "image_url" {
		return "", false
	}

	switch imageURL := value["image_url"].(type) {
	case string:
		return imageURL, strings.TrimSpace(imageURL) != ""
	case map[string]any:
		url, _ := imageURL["url"].(string)
		return url, strings.TrimSpace(url) != ""
	default:
		return "", false
	}
}

func isToolOutputImageDataURL(value string) bool {
	const prefix = "data:image/"
	const separator = ";base64,"
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	separatorIndex := strings.Index(value[len(prefix):], separator)
	if separatorIndex <= 0 {
		return false
	}
	payloadIndex := len(prefix) + separatorIndex + len(separator)
	return payloadIndex < len(value)
}

func toolOutputImagePart(imageURL string) ChatContentPart {
	return ChatContentPart{
		Type:     "image_url",
		ImageURL: &ChatImageURL{URL: imageURL},
	}
}

// appendAssistantToolCall merges a tool call into the chat message list.
// Parallel tool calls arrive as consecutive *_call items and must share one
// assistant message; the matching tool replies then follow it. Merge into the
// immediately preceding assistant message.
func appendAssistantToolCall(messages []ChatMessage, toolCall ChatToolCall, pendingReasoning string) []ChatMessage {
	if n := len(messages); n > 0 && messages[n-1].Role == "assistant" {
		messages[n-1].ToolCalls = append(messages[n-1].ToolCalls, toolCall)
		if messages[n-1].ReasoningContent == "" {
			messages[n-1].ReasoningContent = pendingReasoning
		}
		return messages
	}
	return append(messages, ChatMessage{
		Role:             "assistant",
		ToolCalls:        []ChatToolCall{toolCall},
		ReasoningContent: pendingReasoning,
	})
}

// normalizeChatMessages is the single place that enforces the tool-call
// invariant the DeepSeek / OpenAI Chat Completions schema requires: an assistant
// message with tool_calls must be immediately followed by one tool message per
// tool_call_id, in order, with nothing in between.
//
// Codex histories violate this in several ways that the builder alone can't fix:
//   - a non-tool message lands between an assistant tool_calls message and its
//     tool replies (e.g. a user message the client interleaved mid
//     tool-execution; the "Approved command prefix saved" system notice codex
//     also injects there no longer reaches this stage, it is folded into the
//     leading system message by mergeLeadingSystemMessages);
//   - a parallel tool_call's sibling output never arrives, or a call is left
//     dangling by a mid-execution reconnect (unanswered tool_call);
//   - a tool reply has no announcing assistant tool_call (orphan).
//
// It rebuilds the sequence so each assistant's answered tool_calls are followed
// directly by their replies (in call order); unanswered tool_calls are dropped
// (and an assistant left with neither tool_calls nor content is dropped); orphan
// tool replies and intervening messages are emitted in their natural position
// but never between an assistant tool_calls message and its replies.
func normalizeChatMessages(messages []ChatMessage) []ChatMessage {
	return normalizeChatMessagesWithToolOutputMedia(messages, nil)
}

func normalizeChatMessagesWithToolOutputMedia(messages []ChatMessage, mediaByCallID toolOutputMediaByCallID) []ChatMessage {
	// Index every tool reply by its tool_call_id (last wins on duplicates).
	replies := make(map[string]ChatMessage)
	for _, m := range messages {
		if m.Role == "tool" && m.ToolCallID != "" {
			replies[m.ToolCallID] = m
		}
	}

	out := make([]ChatMessage, 0, len(messages))
	for _, m := range messages {
		switch {
		case m.Role == "tool":
			// A bare tool message with no tool_call_id is a direct Chat
			// Completions passthrough; keep it in place. A tool reply whose id is
			// announced by an assistant is emitted right after that assistant
			// (skip the standalone occurrence). Any other tool reply is an orphan
			// and is dropped.
			if m.ToolCallID == "" {
				out = append(out, m)
			}
			continue
		case len(m.ToolCalls) > 0:
			kept := make([]ChatToolCall, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				if tc.ID == "" {
					continue
				}
				if _, ok := replies[tc.ID]; ok {
					kept = append(kept, tc)
				}
			}
			if len(kept) == 0 {
				// No answered tool_calls left: keep as a plain message if it has
				// content, otherwise drop it entirely.
				if isBlankChatContent(m.Content) {
					continue
				}
				m.ToolCalls = nil
				out = append(out, m)
				continue
			}
			m.ToolCalls = kept
			out = append(out, m)
			for _, tc := range kept {
				out = append(out, replies[tc.ID])
			}

			var mediaParts []ChatContentPart
			for _, tc := range kept {
				media := mediaByCallID[tc.ID]
				if len(media) == 0 {
					continue
				}
				mediaParts = append(mediaParts, ChatContentPart{
					Type: "text",
					Text: fmt.Sprintf(toolOutputMediaAttribution, tc.ID),
				})
				mediaParts = append(mediaParts, media...)
			}
			if len(mediaParts) > 0 {
				content, _ := json.Marshal(mediaParts)
				out = append(out, ChatMessage{Role: "user", Content: content})
			}
		default:
			out = append(out, m)
		}
	}
	return out
}

// isBlankChatContent reports whether a chat message content holds no usable text.
func isBlankChatContent(raw json.RawMessage) bool {
	raw = bytesTrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" || string(raw) == `""` {
		return true
	}
	return chatMessageContentText(raw) == ""
}

// extractResponsesReasoningText pulls the reasoning text out of a Responses
// reasoning item. The Chat→Responses bridge writes the upstream reasoning_content
// verbatim into the summary_text parts (see closeChatReasoningItem), so codex
// round-trips it there; prefer summary[].text and fall back to content.
func extractResponsesReasoningText(item map[string]json.RawMessage) string {
	var parts []string
	collect := func(raw json.RawMessage) {
		raw = bytesTrimSpace(raw)
		if len(raw) == 0 || string(raw) == "null" {
			return
		}
		var arr []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &arr); err == nil {
			for _, p := range arr {
				if t := rawString(p["text"]); t != "" {
					parts = append(parts, t)
				}
			}
			return
		}
		if t := rawString(raw); t != "" {
			parts = append(parts, t)
		}
	}
	collect(item["summary"])
	if len(parts) == 0 {
		collect(item["content"])
	}
	return strings.Join(parts, "\n")
}

// ExtractResponsesReasoningItem parses a raw Responses input item and, when it
// is a reasoning item, returns its id and extractable plaintext (summary
// preferred, content fallback). ok is false for non-reasoning items. It exists
// for the gateway-side reasoning cache: items with plaintext get (re)cached so
// later encrypted-only replicas of the same item id can be restored.
func ExtractResponsesReasoningItem(raw json.RawMessage) (id string, text string, ok bool) {
	raw = bytesTrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return "", "", false
	}
	var item map[string]json.RawMessage
	if err := json.Unmarshal(raw, &item); err != nil {
		return "", "", false
	}
	if rawString(item["type"]) != "reasoning" {
		return "", "", false
	}
	return rawString(item["id"]), extractResponsesReasoningText(item), true
}

func chatCompletionsBridgeRole(role string) string {
	trimmed := strings.TrimSpace(role)
	if trimmed == "" {
		return "user"
	}
	if strings.EqualFold(trimmed, "developer") {
		return "system"
	}
	return role
}

func responsesContentToChatContent(raw json.RawMessage, role string) (json.RawMessage, error) {
	raw = bytesTrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		empty, _ := json.Marshal("")
		return empty, nil
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return raw, nil
	}

	var rawParts []json.RawMessage
	if err := json.Unmarshal(raw, &rawParts); err == nil {
		return responsesContentPartsToChatContent(rawParts, role)
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err == nil {
		return chatContentFromSingleResponsesPart(rawString(obj["type"]), obj)
	}

	return raw, nil
}

func responsesContentPartsToChatContent(rawParts []json.RawMessage, role string) (json.RawMessage, error) {
	var textParts []string
	var chatParts []ChatContentPart
	hasNonText := false

	for _, rawPart := range rawParts {
		var part map[string]json.RawMessage
		if err := json.Unmarshal(rawPart, &part); err != nil {
			continue
		}
		partType := rawString(part["type"])
		switch partType {
		case "input_text", "output_text", "text", "":
			text := rawString(part["text"])
			if text == "" {
				continue
			}
			textParts = append(textParts, text)
			chatParts = append(chatParts, ChatContentPart{Type: "text", Text: text})
		case "input_image", "image_url":
			imageURL := rawString(part["image_url"])
			if imageURL == "" {
				imageURL = rawNestedString(part["image_url"], "url")
			}
			if imageURL == "" {
				continue
			}
			hasNonText = true
			chatParts = append(chatParts, ChatContentPart{
				Type:     "image_url",
				ImageURL: &ChatImageURL{URL: imageURL},
			})
		}
	}

	if !hasNonText {
		joined, _ := json.Marshal(strings.Join(textParts, "\n\n"))
		return joined, nil
	}
	if role != "user" {
		joined, _ := json.Marshal(strings.Join(textParts, "\n\n"))
		return joined, nil
	}
	if len(chatParts) == 0 {
		empty, _ := json.Marshal("")
		return empty, nil
	}
	return json.Marshal(chatParts)
}

func chatContentFromSingleResponsesPart(partType string, part map[string]json.RawMessage) (json.RawMessage, error) {
	switch partType {
	case "input_image", "image_url":
		imageURL := rawString(part["image_url"])
		if imageURL == "" {
			imageURL = rawNestedString(part["image_url"], "url")
		}
		return json.Marshal([]ChatContentPart{{
			Type:     "image_url",
			ImageURL: &ChatImageURL{URL: imageURL},
		}})
	default:
		return json.Marshal(rawString(part["text"]))
	}
}

// customToolInputSchema 是 custom/freeform 工具降级为 function 工具时的参数 schema。
// chat 协议无法表达 custom 工具的自由文本输入（及其 grammar 约束），退化为单一
// input 字符串参数；回程时再从 arguments 的 input 字段还原（见
// extractCustomToolCallInput）。
//
// 注意：这是**本站自己编的降级壳**，不是客户端的真实参数契约。上游模型看不到
// custom 工具的原始自由文本协议，只会照着这个壳凑参数（也常常直接给 {"input": ...}
// 之外的空对象）。因此下游按 schema 校验"必填"时必须把这类合成壳排除在外，否则
// 一批正常的 custom 工具调用会被判成"参数残缺"——见 CustomToolInputSchema。
const CustomToolInputSchema = `{"type":"object","properties":{"input":{"type":"string","description":"The raw input for this tool, passed through verbatim."}},"required":["input"]}`

// customToolInputSchema 保留旧名（包内既有引用与测试沿用），值同 CustomToolInputSchema。
const customToolInputSchema = CustomToolInputSchema

func responsesToolsToChatTools(tools []ResponsesTool) ([]ChatTool, error) {
	// 顶层重复可执行工具名检查（保留原语义）：同名 function/custom 在原生 Responses
	// 上游按名字路由不冲突，但本上游无法区分，必须显式拒绝。
	topLevel := make(map[string]bool)
	for _, tool := range tools {
		if (tool.Type == "function" || tool.Type == "custom") && tool.Name != "" {
			if topLevel[tool.Name] {
				return nil, fmt.Errorf("duplicate top-level executable tool name %q; this upstream cannot disambiguate duplicate names, rename one of the tools", tool.Name)
			}
			topLevel[tool.Name] = true
		}
	}

	// 收敛到唯一事实源：所有 namespace 摊平（function 与 custom 子工具都摊平）都由
	// FlattenResponsesToolList 完成，出来的名字已是 ns__name。chat 桥不再各自实现
	// 一份 namespace 摊平——旧 namespaceChildrenToChatTools 只认 function、把 custom
	// 子工具整个 continue 丢掉，正是 codex 的 exec 从不下发给上游的根因。
	flat, err := FlattenResponsesToolList(tools)
	if err != nil {
		return nil, err
	}

	toolSearchDeclared := false
	out := make([]ChatTool, 0, len(flat.Tools))
	for _, tool := range flat.Tools {
		switch tool.Type {
		case "function":
			out = append(out, ChatTool{
				Type: "function",
				Function: &ChatFunction{
					Name:        tool.Name,
					Description: tool.Description,
					Parameters:  tool.Parameters,
					Strict:      tool.Strict,
				},
			})
		case "custom":
			// 顶层 custom 与 namespace 内 custom 的 f4 形态**故意不同**：
			//   - 顶层 custom 走 chat 桥降级，必须带 {"input":string} 降级壳：模型被引导
			//     发 {"input":"<自由文本>"}，回程 extractCustomToolCallInput 从中抽回纯文本
			//     （chatcompletions_responses_bridge.go 的 extractCustomToolCallInput）。
			//     没有这个壳，模型可能直接发裸文本或发别的形状，抽不出纯文本。
			//   - namespace 内 custom 由 Responses 摊平而来，本来就没有 input_schema
			//     （Responses 的 custom 类型无 parameters），按「无 schema」原样下发，
			//     回程走 cursorToolDeclaredWithoutSchema 原样交付。
			// 顶层 custom 不是本阶段的改动对象，保持既有形态（降级壳 + Custom=false）。
			//
			// 区分方式：flat.NamespaceTools[摊平名].IsCustom 为真即为 namespace 内 custom
			// 子工具（已摊平、名字是 ns__name）；顶层 custom 未经摊平、不在该映射里。
			// 不要靠名字形状猜类型。
			//
			// 读 IsCustom 而不是另一张 NamespaceCustomTools map：同一条判定规则
			// （「这个摊平名是不是 namespace 内的 custom」）只许有一处事实源
			// （NamespacedToolName.IsCustom），两处各读一张表迟早会不一致
			// （2026-11-12 生产实证：交付侧与登记侧结论相反 → 续轮 result_type_mismatch）。
			if nsOrigin, ok := flat.NamespaceTools[tool.Name]; ok && nsOrigin.IsCustom {
				// namespace 内 custom 子工具：无 schema，打 Custom 标记，下游不写 f3/f6。
				out = append(out, ChatTool{
					Type:   "function",
					Custom: true,
					Function: &ChatFunction{
						Name:        tool.Name,
						Description: tool.Description,
						Parameters:  nil,
					},
				})
				continue
			}
			// 顶层 custom（如 codex 0.14x 的 exec）：恢复降级壳，且不打 Custom 标记。
			out = append(out, ChatTool{
				Type: "function",
				Function: &ChatFunction{
					Name:        tool.Name,
					Description: tool.Description,
					Parameters:  json.RawMessage(customToolInputSchema),
				},
			})
		case "tool_search":
			// 代理不能改名（codex 的模型侧按 tool_search 这个名字调用），与客户端
			// 声明的同名工具无法区分——回程会把普通工具的调用劫持成 tool_search_call，
			// 必须显式拒绝；重复声明 type=tool_search 去重即可。
			if topLevel[toolSearchProxyName] {
				return nil, fmt.Errorf("built-in tool_search conflicts with a declared tool named %q; this upstream cannot disambiguate them, rename the tool", toolSearchProxyName)
			}
			if toolSearchDeclared {
				continue
			}
			toolSearchDeclared = true
			out = append(out, toolSearchProxyChatTool())
		case "x_search":
			out = append(out, ChatTool{
				Type:                     "x_search",
				AllowedXHandles:          tool.AllowedXHandles,
				ExcludedXHandles:         tool.ExcludedXHandles,
				FromDate:                 tool.FromDate,
				ToDate:                   tool.ToDate,
				EnableImageUnderstanding: tool.EnableImageUnderstanding,
				EnableVideoUnderstanding: tool.EnableVideoUnderstanding,
			})
		}
		// 其余类型（web_search、image_generation 等服务端工具）在 chat 上游没有
		// 对应能力，维持丢弃。
	}
	return out, nil
}

// ToolSearchProxyName 是 tool_search 服务端工具降级后的 function 工具名（导出供下游
// 识别本站合成的降级壳，见 CustomToolInputSchema 的同理说明）。
const ToolSearchProxyName = toolSearchProxyName

// toolSearchProxyName 是 tool_search 服务端工具降级后的 function 工具名。模型对
// 它的调用以同名 function_call 原样回传，由 codex 端路由。
const toolSearchProxyName = "tool_search"

// ToolSearchProxySchema 是 tool_search 降级壳的参数 schema（本站合成，非客户端契约）。
// 与 CustomToolInputSchema 同理：其中的 required 是本端为引导模型而编的，下游不得
// 拿它去判"上游给的参数残缺"。
const ToolSearchProxySchema = toolSearchProxySchema

const toolSearchProxySchema = `{"type":"object","properties":{"query":{"type":"string","description":"Search query for tools or connectors to load."},"limit":{"type":"integer","description":"Maximum number of tool groups to return."}},"required":["query"]}`

func toolSearchProxyChatTool() ChatTool {
	return ChatTool{
		Type: "function",
		Function: &ChatFunction{
			Name:        toolSearchProxyName,
			Description: "Search and load Codex tools, plugins, connectors, and MCP namespaces for the current task.",
			Parameters:  json.RawMessage(toolSearchProxySchema),
		},
	}
}

// chatToolNameMaxLen 是 Chat Completions function 工具名的通用长度上限。
const chatToolNameMaxLen = 64

// FlattenNamespaceToolName 生成 namespace 子工具的摊平名；超长时截断并追加
// sha256 短哈希保证唯一性。
func FlattenNamespaceToolName(namespace, name string) string {
	full := namespace + "__" + name
	if len(full) <= chatToolNameMaxLen {
		return full
	}
	sum := sha256.Sum256([]byte(full))
	suffix := "__" + hex.EncodeToString(sum[:4])
	prefixLen := chatToolNameMaxLen - len(suffix)
	var prefix strings.Builder
	for _, ch := range full {
		if prefix.Len()+len(string(ch)) > prefixLen {
			break
		}
		_, _ = prefix.WriteRune(ch)
	}
	return prefix.String() + suffix
}

// responsesToolChoiceToChatToolChoice 把 Responses 的 tool_choice 转为 chat 形态。
// declared 是转换后实际声明的 chat 工具名集合：具名选择项仅在目标工具幸存时转发，
// 服务端工具（web_search 等）的选择项随工具本身丢弃——指向未声明工具的 tool_choice
// 会被 chat 上游 400 拒绝。返回 nil 表示丢弃 tool_choice。
func responsesToolChoiceToChatToolChoice(raw json.RawMessage, declared map[string]bool) json.RawMessage {
	var choice map[string]json.RawMessage
	if err := json.Unmarshal(raw, &choice); err != nil {
		// "auto"/"none"/"required" 等字符串形式原样转发。
		return raw
	}
	var name string
	switch rawString(choice["type"]) {
	case "x_search":
		if !declared["x_search"] {
			return nil
		}
		out, err := json.Marshal(map[string]any{"type": "x_search"})
		if err != nil {
			return raw
		}
		return out
	case "tool_search":
		// tool_search 未被丢弃而是降级为同名 function 代理（见
		// responsesToolsToChatTools），强制选择它同样降级为 function 选择，
		// 静默丢弃会把强制搜索退化为自动选择。
		name = toolSearchProxyName
	case "function", "custom":
		// custom 工具已降级为 function 工具，指向它的 tool_choice 同样按 function 转换。
		name = rawString(choice["name"])
		if name == "" {
			name = rawNestedString(choice["function"], "name")
		}
		if name == "" {
			return raw
		}
	default:
		return nil
	}
	if !declared[name] {
		return nil
	}
	out, err := json.Marshal(map[string]any{
		"type": "function",
		"function": map[string]string{
			"name": name,
		},
	})
	if err != nil {
		return raw
	}
	return out
}

// extractCustomToolCallInput 从降级 function 调用的 arguments 中还原 custom 工具的
// 自由文本输入：优先取 {"input": "..."} 的 input 字段；模型未按 schema 输出时原样
// 回传，交由客户端校验、模型重试。
//
// ExtractCustomToolCallInput 是对外导出版本：服务层登记历史摘要时必须用**与客户端实际
// 收到的同一段文本**（custom_tool_call 项的 input 就是这个抽取结果），否则交付侧登记
// 原始 arguments、客户端回传抽取后的 input，两侧必然不同（生产实测 responses 的
// custom_tool_call 项内容差异，history_first_divergence 已定位到该项）。
func ExtractCustomToolCallInput(arguments string) string {
	return extractCustomToolCallInput(arguments)
}

// extractCustomToolCallInput 见 ExtractCustomToolCallInput 的说明。
func extractCustomToolCallInput(arguments string) string {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		return trimmed
	}
	if raw, ok := obj["input"]; ok {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
		return trimmed
	}
	if len(obj) == 0 {
		return ""
	}
	return trimmed
}

// ChatCompletionsResponseToResponses converts a non-streaming Chat Completions
// response into a Responses API response. customTools 是客户端请求中 custom 工具
// 的名字集合（见 CustomToolNames），命中的调用会还原为 custom_tool_call 项；
// toolSearch 表示客户端声明了 tool_search 工具（见 HasToolSearchTool），代理工具
// 的调用会还原为 tool_search_call 项；namespaceTools 是 namespace 子工具的摊平名
// 映射（见 NamespaceToolNames），命中的调用还原为带 namespace 字段的 function_call 项。
func ChatCompletionsResponseToResponses(resp *ChatCompletionsResponse, model string, customTools, functionTools map[string]bool, toolSearch bool, namespaceTools map[string]NamespacedToolName) *ResponsesResponse {
	id := ""
	if resp != nil {
		id = resp.ID
	}
	if id == "" {
		id = generateResponsesID()
	}

	// Carry the upstream's own creation timestamp when it sent one; otherwise
	// stamp now, same fallback shape as the generated id above.
	createdAt := int64(0)
	if resp != nil {
		createdAt = resp.Created
	}
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}

	out := &ResponsesResponse{
		ID:          id,
		Object:      "response",
		CreatedAt:   createdAt,
		Model:       model,
		Status:      "completed",
		ServiceTier: chatServiceTier(resp),
	}
	if resp == nil {
		out.Output = []ResponsesOutput{emptyResponsesMessageOutput()}
		return out
	}
	if out.Model == "" {
		out.Model = resp.Model
	}

	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		out.Output = chatMessageToResponsesOutput(choice.Message, customTools, functionTools, toolSearch, namespaceTools)
		if choice.FinishReason == "length" {
			out.Status = "incomplete"
			out.IncompleteDetails = &ResponsesIncompleteDetails{Reason: "max_output_tokens"}
		}
	}
	if len(out.Output) == 0 {
		out.Output = []ResponsesOutput{emptyResponsesMessageOutput()}
	}
	if resp.Usage != nil {
		out.Usage = ChatUsageToResponsesUsage(resp.Usage)
	}
	return out
}

func chatServiceTier(resp *ChatCompletionsResponse) string {
	if resp == nil {
		return ""
	}
	return resp.ServiceTier
}

// dsmlResponsesToolCallEvents 把还原出来的 DSML 调用转成 Responses 的
// function_call 输出事件（added → arguments.delta）。
//
// DSML 调用参数是完整的一份（官方解析器一次性还原），所以不走增量累积路径：
// 声明项时一次性带全参数。
func (s *ChatCompletionsToResponsesStreamState) dsmlResponsesToolCallEvents(calls []DSMLToolCall) []ResponsesStreamEvent {
	if s == nil || len(calls) == 0 {
		return nil
	}
	var events []ResponsesStreamEvent
	for _, call := range calls {
		idx := s.dsmlToolIndex
		s.dsmlToolIndex++
		s.dsmlRestored++
		toolCall := DSMLToolCallToChatToolCall(call, idx)
		// 工具调用先关闭 reasoning 项。
		events = append(events, closeChatReasoningItem(s)...)
		events = append(events, s.announceDSMLToolItem(&toolCall)...)
	}
	return events
}

// DSMLStats 返回本次流里 DSML 的还原与丢弃计数（供调用方落日志）。
func (s *ChatCompletionsToResponsesStreamState) DSMLStats() (restored, dropped int) {
	if s == nil {
		return 0, 0
	}
	return s.dsmlRestored, s.dsmlDropped
}

// announceDSMLToolItem 声明一个 DSML 还原出来的工具调用项并一次性下发其参数。
func (s *ChatCompletionsToResponsesStreamState) announceDSMLToolItem(toolCall *ChatToolCall) []ResponsesStreamEvent {
	if s == nil || toolCall == nil {
		return nil
	}
	idx := 0
	if toolCall.Index != nil {
		idx = *toolCall.Index
	}
	stored := *toolCall
	stored.Type = "function"
	if stored.ID == "" {
		stored.ID = generateItemID()
	}
	// DSML 还原出来的参数已是完整的一份；声明时一次性带全。
	s.ToolCalls[idx] = &stored
	s.ToolItemIDs[idx] = generateItemID()
	s.ToolOutputIndex[idx] = s.allocOutputIndex()
	return announceChatToolItem(s, idx, &stored, true)
}

func chatMessageToResponsesOutput(message ChatMessage, customTools, functionTools map[string]bool, toolSearch bool, namespaceTools map[string]NamespacedToolName) []ResponsesOutput {
	var outputs []ResponsesOutput
	reasoning := message.reasoningText()

	// DeepSeek 官方 DSML 方言：上游可能把工具调用写成 DSML 文本。按官方规范
	// 还原成标准工具调用输出，并把 DSML 原文摘干净（不让协议标签外泄）。
	dsmlCalls := ParseDSMLToolCalls(reasoning)
	if len(dsmlCalls) == 0 {
		dsmlCalls = ParseDSMLToolCalls(chatMessageContentText(message.Content))
	}
	if len(dsmlCalls) > 0 {
		reasoning = StripDSML(reasoning)
	}

	if reasoning != "" {
		outputs = append(outputs, ResponsesOutput{
			Type: "reasoning",
			ID:   generateItemID(),
			Summary: []ResponsesSummary{{
				Type: "summary_text",
				Text: reasoning,
			}},
		})
	}

	text := StripDSML(chatMessageContentText(message.Content))
	if text == "" && strings.TrimSpace(reasoning) != "" && len(message.ToolCalls) == 0 && len(dsmlCalls) == 0 {
		text = reasoning
	}
	if text != "" || (len(message.ToolCalls) == 0 && len(dsmlCalls) == 0) {
		outputs = append(outputs, ResponsesOutput{
			Type: "message",
			ID:   generateItemID(),
			Role: "assistant",
			Content: []ResponsesContentPart{{
				Type: "output_text",
				Text: text,
			}},
			Status: "completed",
		})
	}

	// 还原出来的 DSML 调用 → 标准 function_call 输出。
	for i, call := range dsmlCalls {
		outputs = append(outputs, ResponsesOutput{
			Type:   "function_call",
			ID:     generateItemID(),
			CallID: "call_dsml_" + itoa(i),
			Name:   call.Name,
			Arguments: call.Arguments,
			Status: "completed",
		})
	}

	for _, toolCall := range message.ToolCalls {
		arguments := toolCall.Function.Arguments
		if strings.TrimSpace(arguments) == "" {
			arguments = "{}"
		}
		if customName, ok := customToolCallName(toolCall.Function.Name, customTools, functionTools, namespaceTools); ok {
			outputs = append(outputs, ResponsesOutput{
				Type:   "custom_tool_call",
				ID:     generateItemID(),
				CallID: toolCall.ID,
				Name:   customName,
				Input:  extractCustomToolCallInput(arguments),
				Status: "completed",
			})
			continue
		}
		if toolSearch && toolCall.Function.Name == toolSearchProxyName {
			outputs = append(outputs, ResponsesOutput{
				Type:      "tool_search_call",
				ID:        generateItemID(),
				CallID:    toolCall.ID,
				Arguments: arguments,
				Status:    "completed",
			})
			continue
		}
		// Ordinary Responses function_call arguments must contain valid JSON.
		// Do not mark a truncated non-streaming Chat tool call as completed;
		// Codex would persist it and poison the next request in the same way as
		// the streaming variant guarded by ValidateToolCallArguments.
		if !json.Valid([]byte(arguments)) {
			continue
		}
		if ns, ok := namespaceTools[toolCall.Function.Name]; ok {
			outputs = append(outputs, ResponsesOutput{
				Type:      "function_call",
				ID:        generateItemID(),
				CallID:    toolCall.ID,
				Name:      ns.Name,
				Namespace: ns.Namespace,
				Arguments: arguments,
				Status:    "completed",
			})
			continue
		}
		outputs = append(outputs, ResponsesOutput{
			Type:      "function_call",
			ID:        generateItemID(),
			CallID:    toolCall.ID,
			Name:      toolCall.Function.Name,
			Arguments: arguments,
			Status:    "completed",
		})
	}

	return outputs
}

// toolSearchCallArgumentsJSON 把降级 function 调用累积的 arguments 字符串还原为
// tool_search_call 线上要求的 JSON 对象；模型未按 schema 输出（非法 JSON）时按
// 字符串值兜底，交由 codex 解析报错后让模型重试。
func toolSearchCallArgumentsJSON(arguments string) json.RawMessage {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return json.RawMessage(`{}`)
	}
	if json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	fallback, _ := json.Marshal(arguments)
	return fallback
}

func emptyResponsesMessageOutput() ResponsesOutput {
	return ResponsesOutput{
		Type:    "message",
		ID:      generateItemID(),
		Role:    "assistant",
		Content: []ResponsesContentPart{{Type: "output_text", Text: ""}},
		Status:  "completed",
	}
}

func chatMessageContentText(raw json.RawMessage) string {
	raw = bytesTrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var parts []ChatContentPart
	if err := json.Unmarshal(raw, &parts); err == nil {
		var texts []string
		for _, part := range parts {
			if part.Type == "text" && part.Text != "" {
				texts = append(texts, part.Text)
			}
		}
		return strings.Join(texts, "\n\n")
	}
	return ""
}

// ChatUsageToResponsesUsage converts Chat Completions token usage to Responses
// usage shape.
func ChatUsageToResponsesUsage(usage *ChatUsage) *ResponsesUsage {
	if usage == nil {
		return nil
	}
	out := &ResponsesUsage{
		InputTokens:  usage.PromptTokens,
		OutputTokens: usage.CompletionTokens,
		TotalTokens:  usage.TotalTokens,
	}
	if out.TotalTokens == 0 {
		out.TotalTokens = out.InputTokens + out.OutputTokens
	}
	if usage.PromptTokensDetails != nil && (usage.PromptTokensDetails.CachedTokens > 0 ||
		usage.PromptTokensDetails.CacheCreationTokens > 0 || usage.PromptTokensDetails.CacheWriteTokens > 0) {
		out.InputTokensDetails = &ResponsesInputTokensDetails{
			CachedTokens:        usage.PromptTokensDetails.CachedTokens,
			CacheCreationTokens: usage.PromptTokensDetails.CacheCreationTokens,
			CacheWriteTokens:    usage.PromptTokensDetails.CacheWriteTokens,
		}
		if usage.PromptTokensDetails.CacheWriteTokens > 0 {
			out.CacheCreationInputTokens = usage.PromptTokensDetails.CacheWriteTokens
		} else {
			out.CacheCreationInputTokens = usage.PromptTokensDetails.CacheCreationTokens
		}
	}
	return out
}

// ChatCompletionsToResponsesStreamState tracks state while converting Chat
// Completions SSE chunks into Responses SSE events.
type ChatCompletionsToResponsesStreamState struct {
	ResponseID     string
	Model          string
	Created        int64
	ServiceTier    string // upstream Chat chunk service_tier, echoed on response events
	SequenceNumber int
	CreatedSent    bool
	CompletedSent  bool

	// nextOutputIndex assigns sequential output_index values to items as they
	// are opened (reasoning, message, tool calls), so the streamed indices match
	// the order of items in the final response.output array.
	nextOutputIndex int

	// Reasoning item lifecycle. DeepSeek-style upstreams stream all
	// reasoning_content before any content, so reasoning is modeled as its own
	// "reasoning" output item that must be opened (output_item.added) before any
	// reasoning delta and closed before the message/tool items open.
	ReasoningItemID string
	ReasoningIndex  int
	ReasoningOpen   bool
	ReasoningDone   bool

	// Message item + output_text content-part lifecycle.
	MessageItemID string
	MessageIndex  int
	TextPartOpen  bool

	Text      strings.Builder
	Reasoning strings.Builder

	// Tool-call lifecycle, keyed by the upstream tool_call index.
	ToolCalls       map[int]*ChatToolCall
	ToolItemIDs     map[int]string
	ToolOutputIndex map[int]int

	// CustomTools 是客户端请求中 custom/freeform 工具的名字集合（见
	// CustomToolNames）。命中的调用按 custom_tool_call 生命周期下发，codex 才能
	// 路由回它注册的 custom 工具。
	CustomTools map[string]bool

	// FunctionTools is the set of explicitly declared top-level function tools.
	FunctionTools map[string]bool

	// ToolSearchDeclared 表示客户端请求声明了 tool_search 工具（见
	// HasToolSearchTool）。命中的代理调用按 tool_search_call 项还原，codex 只按
	// 该项类型（且 execution=client）执行 tool search。
	ToolSearchDeclared bool

	// NamespaceTools 是 namespace 子工具的摊平名 → 原始归属映射（见
	// NamespaceToolNames）。命中的调用还原为带 namespace 字段的 function_call 项，
	// codex 按 namespace+name 路由。
	NamespaceTools map[string]NamespacedToolName

	// toolIsCustom 记录每个工具调用宣告时的类型判定，保证 added/done 事件的
	// 项类型一致。
	toolIsCustom map[int]bool

	// dsmlBuffer 跨帧缓冲 DeepSeek 官方 DSML 工具调用块（见 dsml.go）。
	// 上游把工具调用写成 DSML 文本放在 reasoning_content / content 里；
	// 这里按官方规范还原成标准 function_call 输出，DSML 原文不外泄。
	dsmlBuffer DSMLStreamBuffer

	// dsmlToolIndex 是还原出来的 DSML 工具调用序号（tool_call index 分配用）。
	dsmlToolIndex int

	// dsmlRestored / dsmlDropped 统计本次流里 DSML 的还原与丢弃次数。
	// 由调用方（service 层）统一出口落日志 —— 转换层是纯函数、不打日志，
	// 也不在每个调用点各记一遍（那会散成 N 份口径）。
	dsmlRestored int
	dsmlDropped  int

	// toolIsToolSearch 记录工具调用是否判定为 tool_search 代理调用。
	toolIsToolSearch map[int]bool

	// toolNamespace 记录工具调用宣告时命中的 namespace 归属（见 NamespaceTools）。
	toolNamespace map[int]NamespacedToolName

	// toolAnnounced 记录 output_item.added 是否已发出。存在 custom 工具且名字
	// 尚未到达时延迟宣告，待名字可判定类型后再补发（见 announceChatToolItem）。
	toolAnnounced map[int]bool

	FinishReason string
	Usage        *ResponsesUsage
}

// NewChatCompletionsToResponsesStreamState returns an initialized stream state.
func NewChatCompletionsToResponsesStreamState(model string) *ChatCompletionsToResponsesStreamState {
	return &ChatCompletionsToResponsesStreamState{
		ResponseID:       generateResponsesID(),
		Model:            model,
		Created:          time.Now().Unix(),
		ToolCalls:        make(map[int]*ChatToolCall),
		ToolItemIDs:      make(map[int]string),
		ToolOutputIndex:  make(map[int]int),
		toolIsCustom:     make(map[int]bool),
		toolIsToolSearch: make(map[int]bool),
		toolNamespace:    make(map[int]NamespacedToolName),
		toolAnnounced:    make(map[int]bool),
	}
}

// ValidateToolCallArguments checks the accumulated function-call arguments
// before the stream is finalized. A tool call whose argument stream was
// truncated must not be emitted as a completed Responses item: Codex will
// persist it and replay it on the next turn, where a Chat Completions provider
// rejects the whole request.
func (state *ChatCompletionsToResponsesStreamState) ValidateToolCallArguments() error {
	if state == nil {
		return nil
	}
	for idx, toolCall := range state.ToolCalls {
		if toolCall == nil {
			continue
		}
		if state.toolIsCustom[idx] || state.toolIsToolSearch[idx] {
			continue
		}
		arguments := strings.TrimSpace(toolCall.Function.Arguments)
		if arguments == "" {
			continue
		}
		if !json.Valid([]byte(arguments)) {
			return fmt.Errorf("tool call %q (%s) arguments are invalid JSON", toolCall.ID, toolCall.Function.Name)
		}
	}
	return nil
}

func (state *ChatCompletionsToResponsesStreamState) allocOutputIndex() int {
	idx := state.nextOutputIndex
	state.nextOutputIndex++
	return idx
}

// ChatCompletionsChunkToResponsesEvents converts one Chat Completions stream
// chunk into zero or more Responses stream events.
func ChatCompletionsChunkToResponsesEvents(
	chunk *ChatCompletionsChunk,
	state *ChatCompletionsToResponsesStreamState,
) []ResponsesStreamEvent {
	if chunk == nil || state == nil {
		return nil
	}
	if chunk.ID != "" {
		state.ResponseID = chunk.ID
	}
	if state.Model == "" && chunk.Model != "" {
		state.Model = chunk.Model
	}
	if chunk.ServiceTier != "" {
		state.ServiceTier = chunk.ServiceTier
	}
	if chunk.Usage != nil {
		state.Usage = ChatUsageToResponsesUsage(chunk.Usage)
	}

	var events []ResponsesStreamEvent
	events = append(events, ensureChatToResponsesCreated(state)...)

	for _, choice := range chunk.Choices {
		// Reasoning is emitted as its own output item and must be opened
		// (output_item.added + reasoning_summary_part.added) before the first
		// delta, otherwise a strict client discards the delta. The leading
		// empty-string reasoning delta upstreams send is filtered out.
		reasoning := choice.Delta.reasoningText()
		if reasoning != nil && *reasoning != "" {
			// DeepSeek 官方 DSML 方言：还原出的调用转成标准 function_call 输出，
			// DSML 原文不再透传。
			plain, dsmlCalls := state.dsmlBuffer.Feed(*reasoning)
			events = append(events, state.dsmlResponsesToolCallEvents(dsmlCalls)...)
			if plain != "" {
				events = append(events, ensureChatReasoningItem(state)...)
				_, _ = state.Reasoning.WriteString(plain)
				events = append(events, chatToResponsesEvent(state, "response.reasoning_summary_text.delta", &ResponsesStreamEvent{
					OutputIndex:  state.ReasoningIndex,
					SummaryIndex: 0,
					Delta:        plain,
					ItemID:       state.ReasoningItemID,
				}))
			}
		}
		if choice.Delta.Content != nil && *choice.Delta.Content != "" {
			// 正文里也可能带 DSML（上游两种位置都出现过）。
			plain, dsmlCalls := state.dsmlBuffer.Feed(*choice.Delta.Content)
			events = append(events, state.dsmlResponsesToolCallEvents(dsmlCalls)...)
			if plain == "" {
				continue
			}
			// First real content closes the reasoning item, then opens the
			// message item and its output_text content part.
			events = append(events, closeChatReasoningItem(state)...)
			events = append(events, ensureChatToResponsesMessageItem(state)...)
			events = append(events, ensureChatToResponsesTextPart(state)...)
			_, _ = state.Text.WriteString(plain)
			events = append(events, chatToResponsesEvent(state, "response.output_text.delta", &ResponsesStreamEvent{
				OutputIndex:  state.MessageIndex,
				ContentIndex: 0,
				Delta:        plain,
				ItemID:       state.MessageItemID,
			}))
		}
		for _, toolCall := range choice.Delta.ToolCalls {
			idx := 0
			if toolCall.Index != nil {
				idx = *toolCall.Index
			}
			stored, ok := state.ToolCalls[idx]
			if !ok {
				// A tool call closes any open reasoning item first.
				events = append(events, closeChatReasoningItem(state)...)
				copyCall := toolCall
				if copyCall.ID == "" {
					copyCall.ID = generateItemID()
				}
				copyCall.Type = "function"
				// Arguments are accumulated by the shared block below so the
				// emitted delta and the stored value stay in sync. Some upstreams
				// (e.g. GLM/Zhipu) pack id+name+arguments into the first tool_call
				// chunk; without this reset the first chunk's arguments would be
				// counted twice (once from this copy, once from the += below),
				// producing a doubled, invalid JSON like {"a":1}{"a":1}.
				copyCall.Function.Arguments = ""
				state.ToolCalls[idx] = &copyCall
				stored = &copyCall
				state.ToolItemIDs[idx] = generateItemID()
				state.ToolOutputIndex[idx] = state.allocOutputIndex()
			} else {
				if toolCall.ID != "" {
					stored.ID = toolCall.ID
				}
				if toolCall.Function.Name != "" {
					stored.Function.Name = toolCall.Function.Name
				}
			}
			events = append(events, announceChatToolItem(state, idx, stored, false)...)
			if toolCall.Function.Arguments != "" {
				stored.Function.Arguments += toolCall.Function.Arguments
				// 未宣告（名字未到）时仅累积，宣告时统一补发；custom 调用的
				// arguments 是包裹 input 的 JSON 片段，无法增量还原为自由文本
				// 输入，缓冲整份 arguments 收尾时一次性下发（见 closeChatToolItems）；
				// tool_search 调用同样收尾时随 output_item.done 全量下发。
				if state.toolAnnounced[idx] && !state.toolIsCustom[idx] && !state.toolIsToolSearch[idx] {
					events = append(events, chatToResponsesEvent(state, "response.function_call_arguments.delta", &ResponsesStreamEvent{
						OutputIndex: state.ToolOutputIndex[idx],
						ItemID:      state.ToolItemIDs[idx],
						Delta:       toolCall.Function.Arguments,
						CallID:      stored.ID,
						Name:        stored.Function.Name,
					}))
				}
			}
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			state.FinishReason = *choice.FinishReason
		}
	}

	return events
}

// FinalizeChatCompletionsResponsesStream emits terminal Responses events.
func FinalizeChatCompletionsResponsesStream(state *ChatCompletionsToResponsesStreamState) []ResponsesStreamEvent {
	if state == nil || state.CompletedSent {
		return nil
	}
	var events []ResponsesStreamEvent
	events = append(events, ensureChatToResponsesCreated(state)...)

	// 收口 DSML 缓冲：完整调用还原成 function_call；残片丢弃（不外泄协议标签）。
	if calls, dropped := state.dsmlBuffer.Flush(); len(calls) > 0 || dropped > 0 {
		state.dsmlDropped += dropped
		events = append(events, state.dsmlResponsesToolCallEvents(calls)...)
	}

	// Close a reasoning item that never transitioned to content (reasoning-only
	// or empty completion).
	events = append(events, closeChatReasoningItem(state)...)
	events = append(events, synthesizeChatReasoningFallbackMessage(state)...)

	if state.MessageItemID != "" {
		if state.TextPartOpen {
			events = append(events, chatToResponsesEvent(state, "response.output_text.done", &ResponsesStreamEvent{
				OutputIndex:  state.MessageIndex,
				ContentIndex: 0,
				Text:         state.Text.String(),
				ItemID:       state.MessageItemID,
			}))
			events = append(events, chatToResponsesEvent(state, "response.content_part.done", &ResponsesStreamEvent{
				OutputIndex:  state.MessageIndex,
				ContentIndex: 0,
				ItemID:       state.MessageItemID,
				Part:         &ResponsesContentPart{Type: "output_text", Text: state.Text.String()},
			}))
		}
		events = append(events, chatToResponsesEvent(state, "response.output_item.done", &ResponsesStreamEvent{
			OutputIndex: state.MessageIndex,
			Item: &ResponsesOutput{
				Type:    "message",
				ID:      state.MessageItemID,
				Role:    "assistant",
				Content: []ResponsesContentPart{{Type: "output_text", Text: state.Text.String()}},
				Status:  "completed",
			},
		}))
	}

	// Close every function_call item opened during the stream. Codex finalizes a
	// tool call only after function_call_arguments.done + output_item.done for
	// that item; without them the call never completes and the session wedges.
	// Mirrors cc-switch's finalize_tools.
	events = append(events, closeChatToolItems(state)...)

	status := "completed"
	var incompleteDetails *ResponsesIncompleteDetails
	if state.FinishReason == "length" {
		status = "incomplete"
		incompleteDetails = &ResponsesIncompleteDetails{Reason: "max_output_tokens"}
	}

	state.CompletedSent = true
	events = append(events, chatToResponsesEvent(state, "response.completed", &ResponsesStreamEvent{
		Response: &ResponsesResponse{
			ID:                state.ResponseID,
			Object:            "response",
			CreatedAt:         state.Created,
			Model:             state.Model,
			Status:            status,
			ServiceTier:       state.ServiceTier,
			Output:            state.chatOutput(),
			Usage:             state.Usage,
			IncompleteDetails: incompleteDetails,
		},
	}))
	return events
}

func ensureChatToResponsesCreated(state *ChatCompletionsToResponsesStreamState) []ResponsesStreamEvent {
	if state.CreatedSent {
		return nil
	}
	state.CreatedSent = true
	return []ResponsesStreamEvent{chatToResponsesEvent(state, "response.created", &ResponsesStreamEvent{
		Response: &ResponsesResponse{
			ID:          state.ResponseID,
			Object:      "response",
			CreatedAt:   state.Created,
			Model:       state.Model,
			Status:      "in_progress",
			ServiceTier: state.ServiceTier,
			Output:      []ResponsesOutput{},
		},
	})}
}

// ensureChatReasoningItem opens the reasoning output item (output_item.added +
// reasoning_summary_part.added) before the first reasoning delta. Codex renders
// streaming reasoning only when this summary-part lifecycle is present.
func ensureChatReasoningItem(state *ChatCompletionsToResponsesStreamState) []ResponsesStreamEvent {
	if state.ReasoningOpen || state.ReasoningDone {
		return nil
	}
	state.ReasoningOpen = true
	state.ReasoningItemID = generateItemID()
	state.ReasoningIndex = state.allocOutputIndex()
	return []ResponsesStreamEvent{
		chatToResponsesEvent(state, "response.output_item.added", &ResponsesStreamEvent{
			OutputIndex: state.ReasoningIndex,
			Item:        &ResponsesOutput{Type: "reasoning", ID: state.ReasoningItemID, Status: "in_progress"},
		}),
		chatToResponsesEvent(state, "response.reasoning_summary_part.added", &ResponsesStreamEvent{
			OutputIndex:  state.ReasoningIndex,
			SummaryIndex: 0,
			ItemID:       state.ReasoningItemID,
			Part:         &ResponsesContentPart{Type: "summary_text"},
		}),
	}
}

// closeChatReasoningItem emits the reasoning item's terminal events
// (reasoning_summary_text.done + reasoning_summary_part.done + output_item.done).
func closeChatReasoningItem(state *ChatCompletionsToResponsesStreamState) []ResponsesStreamEvent {
	if !state.ReasoningOpen {
		return nil
	}
	state.ReasoningOpen = false
	state.ReasoningDone = true
	reasoning := state.Reasoning.String()
	return []ResponsesStreamEvent{
		chatToResponsesEvent(state, "response.reasoning_summary_text.done", &ResponsesStreamEvent{
			OutputIndex:  state.ReasoningIndex,
			SummaryIndex: 0,
			Text:         reasoning,
			ItemID:       state.ReasoningItemID,
		}),
		chatToResponsesEvent(state, "response.reasoning_summary_part.done", &ResponsesStreamEvent{
			OutputIndex:  state.ReasoningIndex,
			SummaryIndex: 0,
			ItemID:       state.ReasoningItemID,
			Part:         &ResponsesContentPart{Type: "summary_text", Text: reasoning},
		}),
		chatToResponsesEvent(state, "response.output_item.done", &ResponsesStreamEvent{
			OutputIndex: state.ReasoningIndex,
			Item: &ResponsesOutput{
				Type:    "reasoning",
				ID:      state.ReasoningItemID,
				Status:  "completed",
				Summary: []ResponsesSummary{{Type: "summary_text", Text: reasoning}},
			},
		}),
	}
}

func synthesizeChatReasoningFallbackMessage(state *ChatCompletionsToResponsesStreamState) []ResponsesStreamEvent {
	if state == nil ||
		state.MessageItemID != "" ||
		state.Text.Len() > 0 ||
		state.Reasoning.Len() == 0 ||
		len(state.ToolCalls) > 0 {
		return nil
	}

	text := state.Reasoning.String()
	if strings.TrimSpace(text) == "" {
		return nil
	}

	var events []ResponsesStreamEvent
	events = append(events, ensureChatToResponsesMessageItem(state)...)
	events = append(events, ensureChatToResponsesTextPart(state)...)
	_, _ = state.Text.WriteString(text)
	events = append(events, chatToResponsesEvent(state, "response.output_text.delta", &ResponsesStreamEvent{
		OutputIndex:  state.MessageIndex,
		ContentIndex: 0,
		Delta:        text,
		ItemID:       state.MessageItemID,
	}))
	return events
}

func ensureChatToResponsesMessageItem(state *ChatCompletionsToResponsesStreamState) []ResponsesStreamEvent {
	if state.MessageItemID != "" {
		return nil
	}
	state.MessageItemID = generateItemID()
	state.MessageIndex = state.allocOutputIndex()
	return []ResponsesStreamEvent{chatToResponsesEvent(state, "response.output_item.added", &ResponsesStreamEvent{
		OutputIndex: state.MessageIndex,
		Item: &ResponsesOutput{
			Type:    "message",
			ID:      state.MessageItemID,
			Role:    "assistant",
			Status:  "in_progress",
			Content: []ResponsesContentPart{{Type: "output_text"}},
		},
	})}
}

func ensureChatToResponsesTextPart(state *ChatCompletionsToResponsesStreamState) []ResponsesStreamEvent {
	if state.TextPartOpen {
		return nil
	}
	state.TextPartOpen = true
	return []ResponsesStreamEvent{chatToResponsesEvent(state, "response.content_part.added", &ResponsesStreamEvent{
		OutputIndex:  state.MessageIndex,
		ContentIndex: 0,
		ItemID:       state.MessageItemID,
		Part:         &ResponsesContentPart{Type: "output_text", Text: ""},
	})}
}

// announceChatToolItem 在类型可判定时发出工具调用的 output_item.added。custom
// 工具的判定依赖名字：名字未到且请求里存在 custom 工具时延迟宣告，避免 added/done
// 的项类型不一致；force 用于流收尾，名字始终未到时按 function_call 兜底。
func announceChatToolItem(
	state *ChatCompletionsToResponsesStreamState,
	idx int,
	stored *ChatToolCall,
	force bool,
) []ResponsesStreamEvent {
	if state.toolAnnounced[idx] {
		return nil
	}
	if !force && stored.Function.Name == "" && (len(state.CustomTools) > 0 || len(state.FunctionTools) > 0 || state.ToolSearchDeclared || len(state.NamespaceTools) > 0) {
		return nil
	}
	state.toolAnnounced[idx] = true
	customName, isCustom := customToolCallName(stored.Function.Name, state.CustomTools, state.FunctionTools, state.NamespaceTools)
	isToolSearch := !isCustom && state.ToolSearchDeclared && stored.Function.Name == toolSearchProxyName
	state.toolIsCustom[idx] = isCustom
	state.toolIsToolSearch[idx] = isToolSearch
	itemType := "function_call"
	if isCustom {
		itemType = "custom_tool_call"
	}
	if isToolSearch {
		itemType = "tool_search_call"
	}
	// namespace 子工具的调用仍按 function_call 生命周期下发，但 added/done 项要
	// 还原为裸子工具名 + namespace 字段（codex 按 namespace+name 路由）。
	itemName, itemNamespace := stored.Function.Name, ""
	if isCustom {
		itemName = customName
	}
	if ns, ok := state.NamespaceTools[stored.Function.Name]; ok && !isCustom && !isToolSearch {
		state.toolNamespace[idx] = ns
		itemName, itemNamespace = ns.Name, ns.Namespace
	}
	events := []ResponsesStreamEvent{chatToResponsesEvent(state, "response.output_item.added", &ResponsesStreamEvent{
		OutputIndex: state.ToolOutputIndex[idx],
		Item: &ResponsesOutput{
			Type:      itemType,
			ID:        state.ToolItemIDs[idx],
			CallID:    stored.ID,
			Name:      itemName,
			Namespace: itemNamespace,
			Status:    "in_progress",
		},
	})}
	// 迟到宣告时补发已累积的参数增量（custom/tool_search 的输入收尾统一下发，不补发）。
	if !isCustom && !isToolSearch && stored.Function.Arguments != "" {
		events = append(events, chatToResponsesEvent(state, "response.function_call_arguments.delta", &ResponsesStreamEvent{
			OutputIndex: state.ToolOutputIndex[idx],
			ItemID:      state.ToolItemIDs[idx],
			Delta:       stored.Function.Arguments,
			CallID:      stored.ID,
			Name:        stored.Function.Name,
		}))
	}
	return events
}

// closeChatToolItems emits function_call_arguments.done + output_item.done for
// every tool call opened during the stream, carrying the full call_id/name/
// arguments so codex can deserialize and execute the call. Mirrors cc-switch's
// finalize_tools.
func closeChatToolItems(state *ChatCompletionsToResponsesStreamState) []ResponsesStreamEvent {
	if len(state.ToolCalls) == 0 {
		return nil
	}
	var events []ResponsesStreamEvent
	for i := 0; i < len(state.ToolCalls); i++ {
		toolCall, ok := state.ToolCalls[i]
		if !ok || toolCall == nil {
			continue
		}
		itemID, opened := state.ToolItemIDs[i]
		if !opened {
			continue
		}
		// 名字始终未到导致尚未宣告的调用，收尾前按最终名字兜底宣告。
		events = append(events, announceChatToolItem(state, i, toolCall, true)...)
		arguments := toolCall.Function.Arguments
		if strings.TrimSpace(arguments) == "" {
			arguments = "{}"
		}
		outputIndex := state.ToolOutputIndex[i]
		if state.toolIsCustom[i] {
			// custom 调用按 custom_tool_call 生命周期收尾：input 在此处一次性下发
			// （流中不产出增量，见 ChatCompletionsChunkToResponsesEvents）。
			input := extractCustomToolCallInput(arguments)
			if input != "" {
				events = append(events, chatToResponsesEvent(state, "response.custom_tool_call_input.delta", &ResponsesStreamEvent{
					OutputIndex: outputIndex,
					ItemID:      itemID,
					Delta:       input,
				}))
			}
			events = append(events,
				chatToResponsesEvent(state, "response.custom_tool_call_input.done", &ResponsesStreamEvent{
					OutputIndex: outputIndex,
					ItemID:      itemID,
					CallID:      toolCall.ID,
					Name:        customNameForStreamTool(state, toolCall.Function.Name),
					Input:       input,
				}),
				chatToResponsesEvent(state, "response.output_item.done", &ResponsesStreamEvent{
					OutputIndex: outputIndex,
					Item: &ResponsesOutput{
						Type:   "custom_tool_call",
						ID:     itemID,
						CallID: toolCall.ID,
						Name:   customNameForStreamTool(state, toolCall.Function.Name),
						Input:  input,
						Status: "completed",
					},
				}),
			)
			continue
		}
		if state.toolIsToolSearch[i] {
			// tool_search 调用按 tool_search_call 项收尾：codex 从 output_item.done
			// 物化该调用（无参数增量事件），arguments 全量随项下发。
			events = append(events, chatToResponsesEvent(state, "response.output_item.done", &ResponsesStreamEvent{
				OutputIndex: outputIndex,
				Item: &ResponsesOutput{
					Type:      "tool_search_call",
					ID:        itemID,
					CallID:    toolCall.ID,
					Arguments: arguments,
					Status:    "completed",
				},
			}))
			continue
		}
		// namespace 子工具调用在宣告时已记录归属，收尾项同样带还原名与 namespace。
		name, namespace := toolCall.Function.Name, ""
		if ns, ok := state.toolNamespace[i]; ok {
			name, namespace = ns.Name, ns.Namespace
		}
		events = append(events,
			chatToResponsesEvent(state, "response.function_call_arguments.done", &ResponsesStreamEvent{
				OutputIndex: outputIndex,
				ItemID:      itemID,
				CallID:      toolCall.ID,
				Name:        name,
				Arguments:   arguments,
			}),
			chatToResponsesEvent(state, "response.output_item.done", &ResponsesStreamEvent{
				OutputIndex: outputIndex,
				Item: &ResponsesOutput{
					Type:      "function_call",
					ID:        itemID,
					CallID:    toolCall.ID,
					Name:      name,
					Namespace: namespace,
					Arguments: arguments,
					Status:    "completed",
				},
			}),
		)
	}
	return events
}

func (state *ChatCompletionsToResponsesStreamState) chatOutput() []ResponsesOutput {
	var outputs []ResponsesOutput
	if state.Reasoning.Len() > 0 {
		outputs = append(outputs, ResponsesOutput{
			Type: "reasoning",
			ID:   generateItemID(),
			Summary: []ResponsesSummary{{
				Type: "summary_text",
				Text: state.Reasoning.String(),
			}},
		})
	}
	if state.MessageItemID != "" || len(state.ToolCalls) == 0 {
		outputs = append(outputs, ResponsesOutput{
			Type: "message",
			ID:   nonEmpty(state.MessageItemID, generateItemID()),
			Role: "assistant",
			Content: []ResponsesContentPart{{
				Type: "output_text",
				Text: state.Text.String(),
			}},
			Status: "completed",
		})
	}
	for i := 0; i < len(state.ToolCalls); i++ {
		toolCall, ok := state.ToolCalls[i]
		if !ok || toolCall == nil {
			continue
		}
		arguments := toolCall.Function.Arguments
		if strings.TrimSpace(arguments) == "" {
			arguments = "{}"
		}
		// §25.17.2「实际 ID」：completed 输出数组必须复用流式阶段已分配的同一个条目 ID
		// （state.ToolItemIDs[i]），不得为同一 call 在 added/done 与 completed 里各生成一份
		// 新 ID（否则同一 HTTP 内同一工具条目出现两个 ID）。流式未分配时回退生成，保证非
		// 流式转换路径不受影响。
		itemID := nonEmpty(state.ToolItemIDs[i], generateItemID())
		if state.toolIsCustom[i] {
			outputs = append(outputs, ResponsesOutput{
				Type:   "custom_tool_call",
				ID:     itemID,
				CallID: toolCall.ID,
				Name:   customNameForStreamTool(state, toolCall.Function.Name),
				Input:  extractCustomToolCallInput(arguments),
				Status: "completed",
			})
			continue
		}
		if state.toolIsToolSearch[i] {
			outputs = append(outputs, ResponsesOutput{
				Type:      "tool_search_call",
				ID:        itemID,
				CallID:    toolCall.ID,
				Arguments: arguments,
				Status:    "completed",
			})
			continue
		}
		name, namespace := toolCall.Function.Name, ""
		if ns, ok := state.toolNamespace[i]; ok {
			name, namespace = ns.Name, ns.Namespace
		}
		outputs = append(outputs, ResponsesOutput{
			Type:      "function_call",
			ID:        itemID,
			CallID:    toolCall.ID,
			Name:      name,
			Namespace: namespace,
			Arguments: arguments,
			Status:    "completed",
		})
	}
	return outputs
}

func chatToResponsesEvent(
	state *ChatCompletionsToResponsesStreamState,
	eventType string,
	template *ResponsesStreamEvent,
) ResponsesStreamEvent {
	seq := state.SequenceNumber
	state.SequenceNumber++
	evt := *template
	evt.Type = eventType
	evt.SequenceNumber = seq
	return evt
}

func rawString(raw json.RawMessage) string {
	raw = bytesTrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}

func rawNestedString(raw json.RawMessage, key string) string {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	return rawString(obj[key])
}

func bytesTrimSpace(raw json.RawMessage) json.RawMessage {
	return json.RawMessage(strings.TrimSpace(string(raw)))
}

func nonEmpty(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
