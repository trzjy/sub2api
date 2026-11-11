package apicompat

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// This file implements a DIRECT bridge between Anthropic Messages and OpenAI
// Chat Completions, skipping the Responses API intermediate representation.
//
// The existing chat-fallback path (forwardAnthropicViaRawChatCompletions) chains
// two Responses-anchored bridges — Anthropic→Responses→ChatCompletions on the
// request side and CC→Responses→Anthropic on the response side — so every
// streaming token runs through two state machines. For force-chat accounts
// (third-party OpenAI-compatible upstreams that only speak /v1/chat/completions)
// the Responses layer is pure overhead: these upstreams never see Responses
// semantics, and the clients reaching them via /v1/messages use standard
// function tools (no custom/tool_search/namespace Codex constructs).
//
// The direct bridge collapses both directions into a single conversion each:
//
//	Request:  Anthropic Messages → Chat Completions
//	Response: CC chunk/response → Anthropic events/response
//
// Helper functions from the Responses bridges (anthropicImageToDataURI,
// extractAnthropicTextFromBlocks, fromResponsesCallID, sanitizeAnthropicToolUseInput,
// parseAnthropicSystemContentParts, isReasoningModel, mapAnthropicEffortToResponses,
// normalizeToolParameters) are reused so the conversion semantics stay identical.

// ---------------------------------------------------------------------------
// Request: AnthropicRequest → ChatCompletionsRequest
// ---------------------------------------------------------------------------

// AnthropicToChatCompletionsRequest converts an Anthropic Messages request
// directly into a Chat Completions request, without transiting the Responses
// API. It is semantically equivalent to composing AnthropicToResponses +
// ResponsesToChatCompletionsRequest but avoids materializing the intermediate
// ResponsesRequest and the extra marshal/unmarshal cycle.
func AnthropicToChatCompletionsRequest(req *AnthropicRequest) (*ChatCompletionsRequest, error) {
	return AnthropicToChatCompletionsRequestWithMinOutputTokens(req, DefaultMinMaxOutputTokens)
}

// AnthropicToChatCompletionsRequestWithMinOutputTokens converts an Anthropic
// Messages request using the supplied max_completion_tokens floor.
func AnthropicToChatCompletionsRequestWithMinOutputTokens(req *AnthropicRequest, minOutputTokens int) (*ChatCompletionsRequest, error) {
	if req == nil {
		return nil, fmt.Errorf("anthropic request is nil")
	}

	// DeepSeek thinking-mode 契约：请求带 tools 时，各 assistant 轮次（含纯文本
	// 轮，即无 tool_use 的轮次）已存在的 thinking 明文都必须折叠为 reasoning_content
	// 回传；否则后续请求漏传即 400。无 tools 请求行为不变（见
	// anthropicThinkingToReasoningContent 的 withTools 判据）。
	requestHasTools := len(req.Tools) > 0
	messages, err := anthropicToChatMessages(req.System, req.Messages, requestHasTools)
	if err != nil {
		return nil, err
	}

	out := &ChatCompletionsRequest{
		Model:    req.Model,
		Messages: messages,
		Stream:   req.Stream,
	}

	// Sampling params: reasoning models (gpt-5.x) reject temperature/top_p.
	if !isReasoningModel(req.Model) {
		out.Temperature = req.Temperature
		out.TopP = req.TopP
	}

	if req.MaxTokens > 0 {
		v := req.MaxTokens
		if v < minOutputTokens {
			v = minOutputTokens
		}
		out.MaxCompletionTokens = &v
	}

	// Tools: Anthropic input_schema is a JSON Schema, directly usable as Chat
	// function parameters. Server tools (web_search_*) have no Chat Completions
	// equivalent and are dropped (mirrors responsesToolsToChatTools).
	if len(req.Tools) > 0 {
		tools := anthropicToolsToChatTools(req.Tools)
		if len(tools) > 0 {
			out.Tools = tools
		}
	}

	// tool_choice is only forwarded when tools survived the conversion
	// (upstream rejects tool_choice without tools), and a named choice only when
	// it points at a declared tool — mirroring responsesToolChoiceToChatToolChoice,
	// chat upstreams 400 on tool_choice referencing an unknown tool.
	if len(out.Tools) > 0 && len(req.ToolChoice) > 0 {
		declared := make(map[string]bool, len(out.Tools))
		for _, tool := range out.Tools {
			if tool.Function != nil {
				declared[tool.Function.Name] = true
			}
		}
		tc, err := convertAnthropicToolChoiceToChat(req.ToolChoice, declared)
		if err != nil {
			return nil, fmt.Errorf("convert tool_choice: %w", err)
		}
		if len(tc) > 0 {
			out.ToolChoice = tc
		}
	}

	// Match the Responses bridge, including an explicit thinking disable.
	out.ReasoningEffort = anthropicReasoningEffort(req)

	parallelToolCalls := true
	out.ParallelToolCalls = &parallelToolCalls

	return out, nil
}

// anthropicToChatMessages converts the Anthropic system field + message list
// into Chat Completions messages. It mirrors convertAnthropicToResponsesInput +
// responsesInputToChatMessages but produces ChatMessage directly.
func anthropicToChatMessages(system json.RawMessage, msgs []AnthropicMessage, requestHasTools bool) ([]ChatMessage, error) {
	var messages []ChatMessage

	// System prompt → system message. parseAnthropicSystemContentParts handles
	// both string and []block forms and filters the billing header.
	if len(system) > 0 {
		sysParts, err := parseAnthropicSystemContentParts(system)
		if err != nil {
			return nil, err
		}
		if len(sysParts) > 0 {
			text := joinResponsesContentPartText(sysParts)
			if text != "" {
				content, _ := json.Marshal(text)
				messages = append(messages, ChatMessage{Role: "system", Content: content})
			}
		}
	}

	for _, m := range msgs {
		converted, err := anthropicMsgToChatMessages(m, requestHasTools)
		if err != nil {
			return nil, err
		}
		messages = append(messages, converted...)
	}

	return normalizeChatMessages(messages), nil
}

// anthropicMsgToChatMessages converts one Anthropic message into one or more
// Chat messages. tool_result blocks become standalone "tool" role messages
// (the Chat Completions convention); text/image blocks stay in a user message;
// assistant tool_use blocks become tool_calls on the assistant message.
func anthropicMsgToChatMessages(m AnthropicMessage, requestHasTools bool) ([]ChatMessage, error) {
	switch m.Role {
	case "system":
		parts, err := parseAnthropicSystemContentParts(m.Content)
		if err != nil {
			return nil, err
		}
		text := joinResponsesContentPartText(parts)
		if text == "" {
			return nil, nil
		}
		content, err := json.Marshal(text)
		if err != nil {
			return nil, err
		}
		return []ChatMessage{{Role: "system", Content: content}}, nil
	case "assistant":
		return anthropicAssistantToChatMessages(m.Content, requestHasTools)
	case "user":
		return anthropicUserToChatMessages(m.Content)
	default:
		// 未知角色（critic / narrator 之类客户端自定义）：**丢弃这条消息**继续转换。
		//
		// 2026-11-06 口径：客户把"有报错"等同于"你们模型不稳定"，而一条角色我们
		// 不认识，最坏后果只是少一轮上下文——不值得把整轮打成错误。这也是角色判据
		// 的唯一一处实现，上层（cursor 计划层/转换层）不再重复判定。
		return nil, nil
	}
}

// anthropicUserToChatMessages handles an Anthropic user message. Content may be
// a plain string or an array of blocks. tool_result blocks are extracted into
// standalone "tool" role messages; images inside tool_results are lifted into a
// follow-up user message as image_url parts (the Responses bridge does the same
// — function_call_output only accepts strings, so images must travel separately).
func anthropicUserToChatMessages(raw json.RawMessage) ([]ChatMessage, error) {
	// Plain string → single user message.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		content, _ := json.Marshal(s)
		return []ChatMessage{{Role: "user", Content: content}}, nil
	}

	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}

	var out []ChatMessage
	var toolResultImageParts []ChatContentPart

	// tool_result → "tool" role messages, text extracted; images deferred.
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		text, imageParts := convertToolResultOutput(b)
		content, _ := json.Marshal(text)
		out = append(out, ChatMessage{
			Role:       "tool",
			Content:    content,
			ToolCallID: b.ToolUseID,
		})
		for _, ip := range imageParts {
			toolResultImageParts = append(toolResultImageParts, ChatContentPart{
				Type:     "image_url",
				ImageURL: &ChatImageURL{URL: ip.ImageURL},
			})
		}
	}

	// Remaining text + image blocks → user message. The double-conversion path
	// (responsesContentPartsToChatContent) folds text-only content into a single
	// string joined with "\n\n" and only uses the parts-array form when an image
	// is present — strict chat upstreams reject array content — so the direct
	// bridge preserves that folding.
	var textParts []string
	var parts []ChatContentPart
	hasImage := false
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				textParts = append(textParts, b.Text)
				parts = append(parts, ChatContentPart{Type: "text", Text: b.Text})
			}
		case "image":
			if uri := anthropicImageToDataURI(b.Source); uri != "" {
				hasImage = true
				parts = append(parts, ChatContentPart{
					Type:     "image_url",
					ImageURL: &ChatImageURL{URL: uri},
				})
			}
		}
	}
	if len(toolResultImageParts) > 0 {
		hasImage = true
		parts = append(parts, toolResultImageParts...)
	}

	if !hasImage {
		if len(textParts) > 0 {
			content, _ := json.Marshal(strings.Join(textParts, "\n\n"))
			out = append(out, ChatMessage{Role: "user", Content: content})
		}
		return out, nil
	}

	content, err := json.Marshal(parts)
	if err != nil {
		return nil, err
	}
	out = append(out, ChatMessage{Role: "user", Content: content})

	return out, nil
}

// anthropicAssistantToChatMessages handles an Anthropic assistant message.
// Text content → assistant message content; tool_use blocks → tool_calls on the
// same assistant message; thinking blocks → reasoning_content (see
// anthropicThinkingToReasoningContent). The requestHasTools flag widens the
// reasoning passback scope: when the request declared tools (DeepSeek
// thinking-mode contract), thinking on plain-text assistant turns that follow a
// tool call is also folded back, not only turns that carry tool_calls themselves.
func anthropicAssistantToChatMessages(raw json.RawMessage, requestHasTools bool) ([]ChatMessage, error) {
	// Plain string → single assistant message.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		content, _ := json.Marshal(s)
		return []ChatMessage{{Role: "assistant", Content: content}}, nil
	}

	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}

	msg := ChatMessage{Role: "assistant"}
	text := extractAnthropicTextFromBlocks(blocks)
	if text != "" {
		content, _ := json.Marshal(text)
		msg.Content = content
	}

	for _, b := range blocks {
		if b.Type != "tool_use" {
			continue
		}
		args := "{}"
		if len(b.Input) > 0 {
			args = string(b.Input)
		}
		msg.ToolCalls = append(msg.ToolCalls, ChatToolCall{
			ID:   b.ID,
			Type: "function",
			Function: ChatFunctionCall{
				Name:      b.Name,
				Arguments: args,
			},
		})
	}

	msg.ReasoningContent = anthropicThinkingToReasoningContent(blocks, requestHasTools || len(msg.ToolCalls) > 0)

	return []ChatMessage{msg}, nil
}

// anthropicThinkingToReasoningContent folds thinking blocks back into the
// Chat Completions reasoning_content field.
//
// chatMessageToAnthropicBlocks emits the upstream's reasoning_content as a
// thinking block on the way out, so a multi-turn client echoes it back on the
// next request; dropping it here made the bridge lose exactly what it had just
// produced. DeepSeek's thinking mode requires the reasoning_content that
// produced a tool call to be replayed on that assistant message and answers
// 400 otherwise, which is why buildChatMessagesFromItems already carries
// pendingReasoning onto assistant tool-call messages in the Responses→Chat
// bridge.
//
// withTools keeps the scope aligned with that sibling: when the request
// declared tools (DeepSeek thinking-mode contract) every assistant turn's
// thinking — including plain-text turns that follow a tool call — is folded
// back; this is safe because DeepSeek ignores reasoning_content on
// non-tool turns. When withTools is false (no tools in the request AND the
// message carries no tool_calls) the thinking is still dropped, which is also
// safe: non-tool-turn reasoning_content is ignored by the upstream, and
// callers that don't speak DeepSeek thinking mode must not be sent it.
//
// redacted_thinking blocks and signature-only placeholders carry no plaintext
// and contribute nothing. Multiple blocks join with "\n", matching
// extractResponsesReasoningText.
func anthropicThinkingToReasoningContent(blocks []AnthropicContentBlock, withTools bool) string {
	if !withTools {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "thinking" && b.Thinking != "" {
			parts = append(parts, b.Thinking)
		}
	}
	return strings.Join(parts, "\n")
}

// anthropicToolsToChatTools maps Anthropic tool definitions to Chat Completions
// function tools. Server-side tools (web_search_*) are dropped — they have no
// Chat Completions equivalent.
func anthropicToolsToChatTools(tools []AnthropicTool) []ChatTool {
	var out []ChatTool
	for _, t := range tools {
		if strings.HasPrefix(t.Type, "web_search") {
			continue
		}
		out = append(out, ChatTool{
			Type: "function",
			Function: &ChatFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  normalizeToolParameters(t.InputSchema),
				Strict:      boolPtr(false),
			},
		})
	}
	return out
}

// convertAnthropicToolChoiceToChat maps Anthropic tool_choice to Chat
// Completions tool_choice. A nil result means the choice is dropped: like the
// double-conversion path (responsesToolChoiceToChatToolChoice), a named choice
// pointing at an undeclared tool or an unknown choice type is not forwarded,
// because chat upstreams reject it.
//
//	{"type":"auto"}            → "auto"
//	{"type":"any"}             → "required"
//	{"type":"none"}            → "none"
//	{"type":"tool","name":"X"} → {"type":"function","function":{"name":"X"}} (X declared)
func convertAnthropicToolChoiceToChat(raw json.RawMessage, declared map[string]bool) (json.RawMessage, error) {
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return nil, err
	}

	switch tc.Type {
	case "auto":
		return json.Marshal("auto")
	case "any":
		return json.Marshal("required")
	case "none":
		return json.Marshal("none")
	case "tool":
		if tc.Name == "" || !declared[tc.Name] {
			return nil, nil
		}
		return json.Marshal(map[string]any{
			"type":     "function",
			"function": map[string]string{"name": tc.Name},
		})
	default:
		return nil, nil
	}
}

// joinResponsesContentPartText concatenates the text of input_text parts. Used
// for the system prompt where parseAnthropicSystemContentParts returns
// ResponsesContentPart values.
func joinResponsesContentPartText(parts []ResponsesContentPart) string {
	var texts []string
	for _, p := range parts {
		if p.Type == "input_text" && p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n\n")
}

// ---------------------------------------------------------------------------
// Non-streaming response: ChatCompletionsResponse → AnthropicResponse
// ---------------------------------------------------------------------------

// ChatCompletionsResponseToAnthropic converts a Chat Completions response
// directly into an Anthropic Messages response, without materializing a
// ResponsesResponse. It is semantically equivalent to composing
// ChatCompletionsResponseToResponses + ResponsesToAnthropic.
func ChatCompletionsResponseToAnthropic(resp *ChatCompletionsResponse, model string) *AnthropicResponse {
	out := &AnthropicResponse{
		Type:  "message",
		Role:  "assistant",
		Model: model,
	}

	if resp != nil {
		out.ID = resp.ID
		if out.Model == "" {
			out.Model = resp.Model
		}

		if len(resp.Choices) > 0 {
			choice := resp.Choices[0]
			out.Content = chatMessageToAnthropicBlocks(choice.Message)
			out.StopReason = AnthropicStopReasonPtr(chatFinishReasonToAnthropicStopReason(choice.FinishReason, out.Content))
			// "length" → "max_tokens" is handled by chatFinishReasonToAnthropicStopReason;
			// Anthropic conveys max-tokens via stop_reason only, no incomplete_details field.
		}
		if resp.Usage != nil {
			out.Usage = chatUsageToAnthropicUsage(resp.Usage)
		}
	}

	if len(out.Content) == 0 {
		out.Content = []AnthropicContentBlock{{Type: "text", Text: ""}}
	}
	// Empty choices / nil response never enter the choices branch above; the
	// double-conversion path still reports a completed turn ("end_turn"), and
	// stop_reason null/"" is invalid for completed non-stream responses.
	if AnthropicStopReasonString(out.StopReason) == "" {
		out.StopReason = AnthropicStopReasonPtr(chatFinishReasonToAnthropicStopReason("", out.Content))
	}
	// The double-conversion path generates a response id when the upstream
	// omits one (ChatCompletionsResponseToResponses); clients treat it as required.
	if out.ID == "" {
		out.ID = generateResponsesID()
	}

	return out
}

// chatMessageToAnthropicBlocks converts a Chat Completions message into
// Anthropic content blocks. Reasoning content → thinking block; text content →
// text block; tool_calls → tool_use blocks. Mirrors chatMessageToResponsesOutput
// + the reasoning→thinking mapping in ResponsesToAnthropic.
func chatMessageToAnthropicBlocks(message ChatMessage) []AnthropicContentBlock {
	var blocks []AnthropicContentBlock
	reasoning := message.reasoningText()

	// DeepSeek 官方 DSML 方言：上游可能把工具调用写成 DSML 文本放在
	// reasoning_content / content 里。按官方规范还原成标准 tool_use 块，
	// 并把 DSML 原文摘干净（不让协议标签外泄给客户端）。
	dsmlCalls := ParseDSMLToolCalls(reasoning)
	if len(dsmlCalls) == 0 {
		dsmlCalls = ParseDSMLToolCalls(chatMessageContentText(message.Content))
	}
	if len(dsmlCalls) > 0 {
		reasoning = StripDSML(reasoning)
	}

	if reasoning != "" {
		blocks = append(blocks, AnthropicContentBlock{
			Type:     "thinking",
			Thinking: reasoning,
		})
	}

	text := chatMessageContentText(message.Content)
	text = StripDSML(text)
	// DeepSeek reasoning-only fallback: when there is no text and no tool calls,
	// surface the reasoning content as visible text so the turn isn't empty.
	//
	// 但绝不能把 reasoning 原样当正文（11-03 活体）：上游把推理内联成
	// "<thinking>(reasoning not retained)</thinking>" 时，客户端会把它当正文打印出来。
	// 占位串与推理标签都是网关内部产物，必须先清洗再交付。
	if text == "" && len(message.ToolCalls) == 0 && len(dsmlCalls) == 0 {
		text = sanitizeReasoningForVisibleText(reasoning)
	}
	if text != "" || (len(message.ToolCalls) == 0 && len(dsmlCalls) == 0) {
		blocks = append(blocks, AnthropicContentBlock{Type: "text", Text: text})
	}

	// 还原出来的 DSML 调用 → 标准 tool_use 块。
	for i, call := range dsmlCalls {
		blocks = append(blocks, AnthropicContentBlock{
			Type:  "tool_use",
			ID:    fromResponsesCallID("call_dsml_" + itoa(i)),
			Name:  call.Name,
			Input: sanitizeAnthropicToolUseInput(call.Name, call.Arguments),
		})
	}

	for _, toolCall := range message.ToolCalls {
		arguments := toolCall.Function.Arguments
		if strings.TrimSpace(arguments) == "" {
			arguments = "{}"
		}
		blocks = append(blocks, AnthropicContentBlock{
			Type:  "tool_use",
			ID:    fromResponsesCallID(toolCall.ID),
			Name:  toolCall.Function.Name,
			Input: sanitizeAnthropicToolUseInput(toolCall.Function.Name, arguments),
		})
	}

	return blocks
}

// reasoningVisiblePlaceholder 是网关在 DeepSeek thinking 轮次缺 reasoning 时注入的
// 缺失标记（见 service.messagesReasoningPlaceholder）。它只允许出现在发往上游的
// 请求体里，绝不能作为可见正文交付给客户端。
const reasoningVisiblePlaceholder = "(reasoning not retained)"

// reasoningTagPattern 匹配上游内联进正文的推理标签段（<think> / <thinking> 两种写法）。
var reasoningTagPattern = regexp.MustCompile(`(?is)<think(?:ing)?>.*?</think(?:ing)?>`)

// strayReasoningTagPattern 兜底清掉未成对的孤立标签。
var strayReasoningTagPattern = regexp.MustCompile(`(?is)</?think(?:ing)?>`)

// sanitizeReasoningForVisibleText 清洗"准备当正文输出"的 reasoning：
// 去掉网关占位串、剥离推理标签段与孤立标签、trim 空白。
// 清洗后为空表示这一轮没有可交付正文（调用方应保持空文本，而不是回退原始 reasoning）。
func sanitizeReasoningForVisibleText(reasoning string) string {
	if strings.TrimSpace(reasoning) == "" {
		return ""
	}
	s := reasoning
	s = strings.ReplaceAll(s, reasoningVisiblePlaceholder, "")
	s = reasoningTagPattern.ReplaceAllString(s, "")
	s = strayReasoningTagPattern.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// chatFinishReasonToAnthropicStopReason maps Chat Completions finish_reason to
// Anthropic stop_reason.
//
//	"length"     → "max_tokens"
//	"tool_calls" → "tool_use"
//	other        → "end_turn" (or "tool_use" if tool_use blocks present)
//
// "stop", "content_filter", and unknown reasons all map to a completed response
// in the double-conversion path, which then derives stop_reason from the blocks.
func chatFinishReasonToAnthropicStopReason(reason string, blocks []AnthropicContentBlock) string {
	switch reason {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		if containsAnthropicToolUseBlock(blocks) {
			return "tool_use"
		}
		return "end_turn"
	}
}

// chatUsageToAnthropicUsage converts Chat Completions token usage to Anthropic
// usage shape. Mirrors ChatUsageToResponsesUsage + anthropicUsageFromResponsesUsage.
func chatUsageToAnthropicUsage(usage *ChatUsage) AnthropicUsage {
	if usage == nil {
		return AnthropicUsage{}
	}

	cachedTokens := 0
	cacheCreationTokens := 0
	if usage.PromptTokensDetails != nil {
		cachedTokens = usage.PromptTokensDetails.CachedTokens
		// cache_write_tokens and cache_creation_tokens are alternate spellings of
		// the same quantity, not additive; the double-conversion path
		// (ChatUsageToResponsesUsage) prefers write and falls back to creation.
		if usage.PromptTokensDetails.CacheWriteTokens > 0 {
			cacheCreationTokens = usage.PromptTokensDetails.CacheWriteTokens
		} else {
			cacheCreationTokens = usage.PromptTokensDetails.CacheCreationTokens
		}
	}

	inputTokens := usage.PromptTokens - cachedTokens - cacheCreationTokens
	if inputTokens < 0 {
		inputTokens = 0
	}

	return AnthropicUsage{
		InputTokens:              inputTokens,
		OutputTokens:             usage.CompletionTokens,
		CacheReadInputTokens:     cachedTokens,
		CacheCreationInputTokens: cacheCreationTokens,
	}
}

// ---------------------------------------------------------------------------
// Streaming: ChatCompletionsChunk → []AnthropicStreamEvent (stateful converter)
// ---------------------------------------------------------------------------

// ChatCompletionsToAnthropicStreamState tracks state while converting Chat
// Completions SSE chunks directly into Anthropic SSE events. It collapses the
// ChatCompletionsToResponsesStreamState + ResponsesEventToAnthropicState pair
// into one state machine.
type ChatCompletionsToAnthropicStreamState struct {
	MessageStartSent bool
	MessageStopSent  bool

	// Current content block lifecycle.
	ContentBlockIndex   int
	ContentBlockOpen    bool
	CurrentBlockType    string // "text" | "thinking" | "tool_use"
	CurrentToolName     string
	CurrentToolHadDelta bool
	HasToolCall         bool

	// Tool calls keyed by the upstream tool_call index. The Anthropic block
	// index is assigned when the tool block is announced (content_block_start),
	// which is deferred until the tool's name has arrived. Argument fragments
	// and the call ID seen before the name are buffered and flushed with the
	// announcement; tools whose name never arrives are announced with an empty
	// name at finalize so their arguments are not lost.
	toolBlockIndex    map[int]int
	toolAnnounced     map[int]bool
	toolName          map[int]string
	pendingToolCallID map[int]string
	pendingToolArgs   map[int]string

	// Reasoning (DeepSeek-style): reasoning_content streamed before content.
	// No separate reasoning block index — it uses ContentBlockIndex like the
	// Responses bridge's ReasoningIndex, but since blocks are sequential we
	// reuse the single ContentBlockIndex counter.

	// thinkStripper 跨帧剥离正文 delta 里内联的 <think>/<thinking> 段，
	// 把思考内容转成 thinking_delta 事件而非透传到正文。
	thinkStripper InlineThinkStripper

	// dsmlBuffer 跨帧缓冲 DeepSeek 官方 DSML 工具调用块。
	// 上游（CodeBuddy /v2/chat/completions）把工具调用写成 DSML 文本放在
	// reasoning_content / content 里；这里按官方规范还原成标准 tool_use 块，
	// 并把 DSML 原文从可见内容里摘干净（不让协议标签外泄给客户端）。
	dsmlBuffer DSMLStreamBuffer

	// dsmlToolIndex 是还原出来的 DSML 工具调用的序号（tool_use index 分配用）。
	dsmlToolIndex int

	// dsmlRestored / dsmlDropped 统计本次流里 DSML 的还原与丢弃次数。
	// 由调用方（service 层）统一出口落日志 —— 转换层是纯函数、不打日志，
	// 也不在每个调用点各记一遍（那会散成 N 份口径）。
	dsmlRestored int
	dsmlDropped  int

	FinishReason string

	InputTokens              int
	OutputTokens             int
	CacheReadInputTokens     int
	CacheCreationInputTokens int

	ResponseID string
	Model      string
	Created    int64
}

// NewChatCompletionsToAnthropicStreamState returns an initialized stream state.
func NewChatCompletionsToAnthropicStreamState(model string) *ChatCompletionsToAnthropicStreamState {
	return &ChatCompletionsToAnthropicStreamState{
		ResponseID:        generateResponsesID(),
		Model:             model,
		Created:           time.Now().Unix(),
		toolBlockIndex:    make(map[int]int),
		toolAnnounced:     make(map[int]bool),
		toolName:          make(map[int]string),
		pendingToolCallID: make(map[int]string),
		pendingToolArgs:   make(map[int]string),
	}
}

// ChatCompletionsChunkToAnthropicEvents converts one Chat Completions stream
// chunk into zero or more Anthropic stream events, updating state as it goes.
func ChatCompletionsChunkToAnthropicEvents(
	chunk *ChatCompletionsChunk,
	state *ChatCompletionsToAnthropicStreamState,
) []AnthropicStreamEvent {
	if chunk == nil || state == nil {
		return nil
	}
	if chunk.ID != "" {
		state.ResponseID = chunk.ID
	}
	if state.Model == "" && chunk.Model != "" {
		state.Model = chunk.Model
	}

	// Usage in a streaming chunk (include_usage) arrives in its own chunk,
	// often with empty choices. Capture it for the finalize message_delta.
	if chunk.Usage != nil {
		u := chatUsageToAnthropicUsage(chunk.Usage)
		state.InputTokens = u.InputTokens
		state.OutputTokens = u.OutputTokens
		state.CacheReadInputTokens = u.CacheReadInputTokens
		state.CacheCreationInputTokens = u.CacheCreationInputTokens
	}

	var events []AnthropicStreamEvent
	events = append(events, ensureCCAnthropicMessageStart(state)...)

	for _, choice := range chunk.Choices {
		// Reasoning content → thinking block.
		//
		// DeepSeek 官方 DSML 方言：上游把工具调用写成 DSML 文本放在 reasoning_content
		// 里。先过 DSML 缓冲：还原出的调用转成标准 tool_use 块；DSML 原文不再透传。
		reasoning := choice.Delta.reasoningText()
		if reasoning != nil && *reasoning != "" {
			plain, calls := state.dsmlBuffer.Feed(*reasoning)
			events = append(events, state.dsmlToolCallEvents(calls)...)
			if plain != "" {
				events = append(events, ensureCCAnthropicThinkingBlock(state)...)
				events = append(events, ccAnthropicDelta(state, &AnthropicDelta{
					Type:     "thinking_delta",
					Thinking: plain,
				})...)
			}
		}

		// Text content → text block (closes any open thinking block first),
		// stripping any inline <think>/<thinking> segments along the way.
		if choice.Delta.Content != nil && *choice.Delta.Content != "" {
			events = append(events, handleCCAnthropicTextDelta(state, *choice.Delta.Content)...)
		}

		// Tool calls → tool_use blocks.
		for _, toolCall := range choice.Delta.ToolCalls {
			events = append(events, closeCCAnthropicBlockIfOpen(state, "thinking")...)
			events = append(events, handleCCAnthropicToolCall(state, &toolCall)...)
		}

		if choice.FinishReason != nil && *choice.FinishReason != "" {
			state.FinishReason = *choice.FinishReason
		}
	}

	return events
}

// dsmlToolCallEvents 把还原出来的 DSML 调用转成 Anthropic tool_use 事件。
//
// 官方 vLLM 的做法：缓冲到完整 invoke 块出现才一次性吐出结构化参数，
// 不逐帧贪心解析。本函数由 dsmlBuffer.Calls() 在凑齐时被调用。
func (s *ChatCompletionsToAnthropicStreamState) dsmlToolCallEvents(calls []DSMLToolCall) []AnthropicStreamEvent {
	if s == nil || len(calls) == 0 {
		return nil
	}
	var events []AnthropicStreamEvent
	for _, call := range calls {
		idx := s.dsmlToolIndex
		s.dsmlToolIndex++
		s.dsmlRestored++
		toolCall := DSMLToolCallToChatToolCall(call, idx)
		events = append(events, closeCCAnthropicBlockIfOpen(s, "thinking")...)
		events = append(events, handleCCAnthropicToolCall(s, &toolCall)...)
	}
	return events
}

// DSMLStats 返回本次流里 DSML 的还原与丢弃计数（供调用方落日志）。
func (s *ChatCompletionsToAnthropicStreamState) DSMLStats() (restored, dropped int) {
	if s == nil {
		return 0, 0
	}
	return s.dsmlRestored, s.dsmlDropped
}

// handleCCAnthropicTextDelta processes a Chat Completions text delta: it first
// strips any inline <think>/<thinking> segments via the cross-frame
// InlineThinkStripper so reasoning is routed to thinking_delta and the remaining
// body to text_delta. (Inline tool-call freezing is not part of this bridge's
// text path, so only the stripping + text emission branches apply here.)
func handleCCAnthropicTextDelta(state *ChatCompletionsToAnthropicStreamState, delta string) []AnthropicStreamEvent {
	if state == nil || delta == "" {
		return nil
	}

	// 先过 DSML 缓冲：上游可能把工具调用写成 DSML 文本放在 content 里。
	// 还原出的调用转成标准 tool_use 块；DSML 原文不会透传到正文。
	delta, dsmlCalls := state.dsmlBuffer.Feed(delta)
	events := state.dsmlToolCallEvents(dsmlCalls)
	if delta == "" {
		return events
	}

	// 先过 InlineThinkStripper 剥离内联 <think>/<thinking> 段。
	reasoning, text := state.thinkStripper.Feed(delta)

	// reasoning 部分 → thinking_delta 事件
	if reasoning != "" {
		events = append(events, ensureCCAnthropicThinkingBlock(state)...)
		events = append(events, ccAnthropicDelta(state, &AnthropicDelta{
			Type:     "thinking_delta",
			Thinking: reasoning,
		})...)
	}

	// text 部分 → 原有文本处理逻辑（关闭可能已开启的 thinking 块，再走文本）
	if text != "" {
		if reasoning != "" {
			events = append(events, closeCCAnthropicBlockIfOpen(state, "thinking")...)
		}
		events = append(events, ensureCCAnthropicTextBlock(state)...)
		events = append(events, ccAnthropicDelta(state, &AnthropicDelta{
			Type: "text_delta",
			Text: text,
		})...)
	}

	return events
}

// FinalizeChatCompletionsAnthropicStream emits terminal Anthropic events
// (close open blocks + message_delta + message_stop) when the stream ends.
func FinalizeChatCompletionsAnthropicStream(state *ChatCompletionsToAnthropicStreamState) []AnthropicStreamEvent {
	if state == nil || state.MessageStopSent {
		return nil
	}

	var events []AnthropicStreamEvent

	// 收口 DSML 缓冲：完整调用还原成 tool_use；残片直接丢弃（不外泄协议标签）。
	if calls, dropped := state.dsmlBuffer.Flush(); len(calls) > 0 || dropped > 0 {
		state.dsmlDropped += dropped
		events = append(events, state.dsmlToolCallEvents(calls)...)
	}

	// 冲出跨帧缓冲里残留的 think 尾段
	if r, t := state.thinkStripper.Flush(); r != "" || t != "" {
		if r != "" {
			events = append(events, ensureCCAnthropicThinkingBlock(state)...)
			events = append(events, ccAnthropicDelta(state, &AnthropicDelta{
				Type:     "thinking_delta",
				Thinking: r,
			})...)
			events = append(events, closeCCAnthropicBlockIfOpen(state, "thinking")...)
		}
		if t != "" {
			events = append(events, closeCCAnthropicBlockIfOpen(state, "thinking")...)
			events = append(events, ensureCCAnthropicTextBlock(state)...)
			events = append(events, ccAnthropicDelta(state, &AnthropicDelta{
				Type: "text_delta",
				Text: t,
			})...)
		}
	}

	if !state.MessageStartSent {
		events = append(events, ensureCCAnthropicMessageStart(state)...)
	}

	// Announce tools whose name never arrived so their buffered arguments are
	// not silently dropped. The double-conversion path announced these
	// immediately with an empty name; the deferred announcement keeps that data
	// preservation while still delivering correct names when they do arrive.
	if len(state.pendingToolCallID) > 0 {
		idxs := make([]int, 0, len(state.pendingToolCallID))
		for idx := range state.pendingToolCallID {
			idxs = append(idxs, idx)
		}
		sort.Ints(idxs)
		for _, idx := range idxs {
			callID := state.pendingToolCallID[idx]
			events = append(events, closeCCAnthropicBlock(state)...)
			events = append(events, announceCCAnthropicToolBlock(state, idx, callID, "")...)
		}
	}

	events = append(events, closeCCAnthropicBlock(state)...)

	stopReason := ccFinishReasonToAnthropicStopReason(state.FinishReason, state.HasToolCall)

	events = append(events,
		AnthropicStreamEvent{
			Type: "message_delta",
			Delta: &AnthropicDelta{
				StopReason: stopReason,
			},
			Usage: &AnthropicUsage{
				InputTokens:              state.InputTokens,
				OutputTokens:             state.OutputTokens,
				CacheReadInputTokens:     state.CacheReadInputTokens,
				CacheCreationInputTokens: state.CacheCreationInputTokens,
			},
		},
		AnthropicStreamEvent{Type: "message_stop"},
	)
	state.MessageStopSent = true
	return events
}

// ensureCCAnthropicMessageStart emits message_start on the first event.
func ensureCCAnthropicMessageStart(state *ChatCompletionsToAnthropicStreamState) []AnthropicStreamEvent {
	if state.MessageStartSent {
		return nil
	}
	state.MessageStartSent = true
	return []AnthropicStreamEvent{{
		Type: "message_start",
		Message: &AnthropicResponse{
			ID:         state.ResponseID,
			Type:       "message",
			Role:       "assistant",
			Content:    []AnthropicContentBlock{},
			Model:      state.Model,
			StopReason: nil, // JSON null; never ""
			Usage:      AnthropicUsage{InputTokens: 0, OutputTokens: 0},
		},
	}}
}

// ensureCCAnthropicThinkingBlock opens a thinking block if none is open.
func ensureCCAnthropicThinkingBlock(state *ChatCompletionsToAnthropicStreamState) []AnthropicStreamEvent {
	if state.ContentBlockOpen && state.CurrentBlockType == "thinking" {
		return nil
	}
	events := closeCCAnthropicBlock(state)
	idx := state.ContentBlockIndex
	state.ContentBlockOpen = true
	state.CurrentBlockType = "thinking"
	events = append(events, AnthropicStreamEvent{
		Type:  "content_block_start",
		Index: &idx,
		ContentBlock: &AnthropicContentBlock{
			Type:     "thinking",
			Thinking: "",
		},
	})
	return events
}

// ensureCCAnthropicTextBlock opens a text block if none is open.
func ensureCCAnthropicTextBlock(state *ChatCompletionsToAnthropicStreamState) []AnthropicStreamEvent {
	if state.ContentBlockOpen && state.CurrentBlockType == "text" {
		return nil
	}
	events := closeCCAnthropicBlock(state)
	idx := state.ContentBlockIndex
	state.ContentBlockOpen = true
	state.CurrentBlockType = "text"
	events = append(events, AnthropicStreamEvent{
		Type:  "content_block_start",
		Index: &idx,
		ContentBlock: &AnthropicContentBlock{
			Type: "text",
			Text: "",
		},
	})
	return events
}

// handleCCAnthropicToolCall processes one upstream tool_call delta. The
// content_block_start for a tool is deferred until its name has arrived (some
// upstreams stream id/arguments before the name); argument fragments seen
// before the announcement are buffered and flushed with it, later fragments
// stream as input_json_delta on the tool's block.
func handleCCAnthropicToolCall(state *ChatCompletionsToAnthropicStreamState, toolCall *ChatToolCall) []AnthropicStreamEvent {
	idx := 0
	if toolCall.Index != nil {
		idx = *toolCall.Index
	}

	var events []AnthropicStreamEvent

	if _, seen := state.toolAnnounced[idx]; !seen {
		// New tool call: it ends whatever block is currently streaming.
		events = append(events, closeCCAnthropicBlock(state)...)
		state.HasToolCall = true

		callID := toolCall.ID
		if callID == "" {
			callID = generateItemID()
		}
		if name := toolCall.Function.Name; name != "" {
			events = append(events, announceCCAnthropicToolBlock(state, idx, callID, name)...)
		} else {
			state.toolAnnounced[idx] = false
			state.pendingToolCallID[idx] = callID
		}
	} else if !state.toolAnnounced[idx] && toolCall.Function.Name != "" {
		// Deferred announcement: the name has arrived.
		callID := state.pendingToolCallID[idx]
		if toolCall.ID != "" {
			callID = toolCall.ID
		}
		events = append(events, closeCCAnthropicBlock(state)...)
		events = append(events, announceCCAnthropicToolBlock(state, idx, callID, toolCall.Function.Name)...)
	}

	// Argument fragment → input_json_delta on the tool's block once announced,
	// buffered until the deferred announcement otherwise.
	if toolCall.Function.Arguments != "" {
		if state.toolAnnounced[idx] {
			blockIdx := state.toolBlockIndex[idx]
			if state.ContentBlockOpen && blockIdx == state.ContentBlockIndex {
				state.CurrentToolHadDelta = true
			}
			events = append(events, AnthropicStreamEvent{
				Type:  "content_block_delta",
				Index: &blockIdx,
				Delta: &AnthropicDelta{
					Type:        "input_json_delta",
					PartialJSON: toolCall.Function.Arguments,
				},
			})
		} else {
			state.pendingToolArgs[idx] += toolCall.Function.Arguments
		}
	}

	return events
}

// announceCCAnthropicToolBlock assigns the next Anthropic block index to the
// tool, emits its content_block_start, and flushes any argument fragments
// buffered while the announcement was deferred.
func announceCCAnthropicToolBlock(state *ChatCompletionsToAnthropicStreamState, idx int, callID, name string) []AnthropicStreamEvent {
	blockIdx := state.ContentBlockIndex
	state.toolBlockIndex[idx] = blockIdx
	state.toolAnnounced[idx] = true
	state.toolName[idx] = name
	state.CurrentToolName = name
	state.CurrentToolHadDelta = false
	state.ContentBlockOpen = true
	state.CurrentBlockType = "tool_use"
	delete(state.pendingToolCallID, idx)

	events := []AnthropicStreamEvent{{
		Type:  "content_block_start",
		Index: &blockIdx,
		ContentBlock: &AnthropicContentBlock{
			Type:  "tool_use",
			ID:    fromResponsesCallID(callID),
			Name:  name,
			Input: json.RawMessage("{}"),
		},
	}}
	if pending := state.pendingToolArgs[idx]; pending != "" {
		delete(state.pendingToolArgs, idx)
		state.CurrentToolHadDelta = true
		events = append(events, AnthropicStreamEvent{
			Type:  "content_block_delta",
			Index: &blockIdx,
			Delta: &AnthropicDelta{
				Type:        "input_json_delta",
				PartialJSON: pending,
			},
		})
	}
	return events
}

// ccAnthropicDelta emits a content_block_delta on the current block.
func ccAnthropicDelta(state *ChatCompletionsToAnthropicStreamState, delta *AnthropicDelta) []AnthropicStreamEvent {
	if !state.ContentBlockOpen {
		return nil
	}
	idx := state.ContentBlockIndex
	return []AnthropicStreamEvent{{
		Type:  "content_block_delta",
		Index: &idx,
		Delta: delta,
	}}
}

// closeCCAnthropicBlockIfOpen closes the current block only if it matches the
// given type (used to close a thinking block before opening text/tool).
func closeCCAnthropicBlockIfOpen(state *ChatCompletionsToAnthropicStreamState, blockType string) []AnthropicStreamEvent {
	if !state.ContentBlockOpen || state.CurrentBlockType != blockType {
		return nil
	}
	return closeCCAnthropicBlock(state)
}

// closeCCAnthropicBlock closes the currently open content block. A tool_use
// block that streamed no argument delta gets a final input_json_delta "{}"
// first — the double-conversion path normalizes empty tool arguments to "{}",
// and some clients assemble tool input exclusively from deltas.
func closeCCAnthropicBlock(state *ChatCompletionsToAnthropicStreamState) []AnthropicStreamEvent {
	if !state.ContentBlockOpen {
		return nil
	}
	idx := state.ContentBlockIndex
	var events []AnthropicStreamEvent
	if state.CurrentBlockType == "tool_use" && !state.CurrentToolHadDelta {
		events = append(events, AnthropicStreamEvent{
			Type:  "content_block_delta",
			Index: &idx,
			Delta: &AnthropicDelta{
				Type:        "input_json_delta",
				PartialJSON: "{}",
			},
		})
	}
	state.ContentBlockOpen = false
	state.ContentBlockIndex++
	state.CurrentBlockType = ""
	state.CurrentToolName = ""
	state.CurrentToolHadDelta = false
	return append(events, AnthropicStreamEvent{
		Type:  "content_block_stop",
		Index: &idx,
	})
}

// ccFinishReasonToAnthropicStopReason maps a Chat Completions finish_reason
// (captured during streaming) to an Anthropic stop_reason for message_delta.
func ccFinishReasonToAnthropicStopReason(reason string, hasToolCall bool) string {
	switch reason {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "stop":
		if hasToolCall {
			return "tool_use"
		}
		return "end_turn"
	default:
		if hasToolCall {
			return "tool_use"
		}
		return "end_turn"
	}
}
