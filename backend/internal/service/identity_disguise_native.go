package service

import (
	"encoding/json"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Anthropic 原生出站链路（api_protocol=anthropic）的伪装钩子。
//
// 背景：该链路是**字节级透传**（/v1/messages 直通上游 body，只经
// reverseToolNamesIfPresent），此前没有任何伪装钩子 —— 账号 extra 里的
// identity_prompt / mask_upstream_identity / response_identity_mask /
// strip_reasoning_content 四个键在这条链路上**全部失效**（模型回写、人设注入、
// 思考剥离一个都不做）。
//
// 本文件提供该链路的三个钩子：
//   1. InjectAnthropicIdentityPrompt：请求侧把身份人设注入 Anthropic body 的 system 字段；
//   2. RewriteAnthropicResponseModel：响应侧把上游回显的模型名改写回客户端请求名
//      （仅 /v1/messages 直通需要；CC / Responses 入站路径已由 apicompat 转换层
//      用 originalModel 覆盖，无需重复处理）；
//   3. StripAnthropicThinking：响应侧剥离客户端可见的思考内容
//      （三种响应形态：Anthropic 原始 body / Anthropic SSE / 已转成 CC 或 Responses 的结构）。
//
// 所有钩子都以「该账号是否开启对应伪装面」为闸门 —— 未开启时**逐字节不变**。

// InjectAnthropicIdentityPrompt 把身份人设注入 Anthropic 请求体的 system 字段。
//
// Anthropic 协议的 system 字段有两种形态：字符串、或 content block 数组。
// 规则（与 CC 路径 injectAccountIdentitySystemMessage 的语义对齐）：
//   - body 非 JSON / 无 system 字段 → 新增 system 字段；
//   - system 是字符串 → 前置拼接人设 + 原字符串；
//   - system 是数组 → 在数组首位插入一个 text block 承载人设。
//
// 未开启人设注入时原样返回 body（逐字节不变）。
func InjectAnthropicIdentityPrompt(body []byte, prompt string) []byte {
	if strings.TrimSpace(prompt) == "" {
		return body
	}
	if !gjson.ValidBytes(body) {
		return body
	}
	sys := gjson.GetBytes(body, "system")
	switch {
	case !sys.Exists():
		// 无 system 字段 → 新增。
		out, err := sjson.SetRawBytes(body, "system", []byte(mustMarshalString(prompt)))
		if err != nil {
			return body
		}
		return out
	case sys.Type == gjson.String:
		// 字符串形态 → 前置拼接。
		merged := prompt + "\n" + sys.String()
		out, err := sjson.SetBytes(body, "system", merged)
		if err != nil {
			return body
		}
		return out
	case sys.IsArray():
		// block 数组形态 → 在数组**首位**插入一个 text block。
		// 注意：不能用 sjson.SetRawBytes(body, "system.0", ...) —— 那是**覆盖**
		// 第 0 个元素（会把原首个块顶掉），不是插入。改为整体重写数组。
		block := map[string]any{"type": "text", "text": prompt}
		raw, err := json.Marshal(block)
		if err != nil {
			return body
		}
		items := sys.Array()
		merged := make([]json.RawMessage, 0, len(items)+1)
		merged = append(merged, json.RawMessage(raw))
		for _, it := range items {
			merged = append(merged, json.RawMessage(it.Raw))
		}
		arrRaw, err := json.Marshal(merged)
		if err != nil {
			return body
		}
		out, err := sjson.SetRawBytes(body, "system", arrRaw)
		if err != nil {
			return body
		}
		return out
	default:
		return body
	}
}

// mustMarshalString 把字符串序列化为 JSON 字符串字面量。
func mustMarshalString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// RewriteAnthropicResponseModel 把 Anthropic 响应体里的 model 字段改写为客户端请求名。
//
// 用于 /v1/messages 直通路径：上游回显的是上游真名（如 spark-x2.6-4b），
// 客户端应当只看到自己请求的名字（如 deepseek-flash）。
//
// toModel 为空时原样返回（不改写）。
func RewriteAnthropicResponseModel(body []byte, toModel string) []byte {
	if strings.TrimSpace(toModel) == "" {
		return body
	}
	if !gjson.ValidBytes(body) {
		return body
	}
	echoed := gjson.GetBytes(body, "model")
	if !echoed.Exists() || echoed.Type != gjson.String {
		return body
	}
	if echoed.String() == toModel {
		return body
	}
	out, err := sjson.SetBytes(body, "model", toModel)
	if err != nil {
		return body
	}
	return out
}

// RewriteAnthropicSSEMessageStartModel 改写 Anthropic SSE 的 message_start 事件里的模型名。
//
// message_start 的载荷形态是 {"type":"message_start","message":{"model":"..."}} ——
// 模型名在嵌套的 message 对象里，与普通响应体的顶层 model 不同。
//
// 非 message_start 事件、或没有嵌套 model 时原样返回。
func RewriteAnthropicSSEMessageStartModel(payload []byte, toModel string) []byte {
	if strings.TrimSpace(toModel) == "" || !gjson.ValidBytes(payload) {
		return payload
	}
	if gjson.GetBytes(payload, "type").String() != "message_start" {
		return payload
	}
	echoed := gjson.GetBytes(payload, "message.model")
	if !echoed.Exists() || echoed.Type != gjson.String || echoed.String() == toModel {
		return payload
	}
	out, err := sjson.SetBytes(payload, "message.model", toModel)
	if err != nil {
		return payload
	}
	return out
}

// StripAnthropicThinkingBody 从 Anthropic 非流式响应体里删除 thinking 内容块。
//
// Anthropic 的思考内容在 content[] 里表现为 {"type":"thinking","thinking":"...","signature":"..."}。
// 删除整块（不是清空 thinking 字段）—— 留下空块会让客户端看到无意义的空 thinking 块，
// 且部分严格客户端会因此报错。
//
// 采集与剥离的顺序要求：调用方必须**先**完成 tool 轮的 reasoning 明文采集
// （cacheMessagesToolCallReasoning），**再**调用本函数剥离 —— 否则回注会丢真实明文。
func StripAnthropicThinkingBody(account *Account, body []byte) []byte {
	if !identityDisguiseStripReasoning(account) {
		return body
	}
	if !gjson.ValidBytes(body) {
		return body
	}
	content := gjson.GetBytes(body, "content")
	if !content.IsArray() {
		return body
	}
	blocks := content.Array()
	if len(blocks) == 0 {
		return body
	}
	hasThinking := false
	for _, b := range blocks {
		if b.Get("type").String() == "thinking" || b.Get("type").String() == "redacted_thinking" {
			hasThinking = true
			break
		}
	}
	if !hasThinking {
		return body
	}
	kept := make([]any, 0, len(blocks))
	for _, b := range blocks {
		t := b.Get("type").String()
		if t == "thinking" || t == "redacted_thinking" {
			continue
		}
		kept = append(kept, b.Value())
	}
	raw, err := json.Marshal(kept)
	if err != nil {
		return body
	}
	out, err := sjson.SetRawBytes(body, "content", raw)
	if err != nil {
		return body
	}
	return out
}

// StripAnthropicThinkingFromCCBody 从已转成 Chat Completions 形态的响应体里剥离思考内容。
//
// 复用既有的 StripReasoningFromResponseBody（剥 choices[].message.reasoning_content
// 与 reasoning 两个字段名，覆盖全部 choices 索引）—— 不另写一份剥离逻辑。
func StripAnthropicThinkingFromCCBody(account *Account, body []byte) []byte {
	if !identityDisguiseStripReasoning(account) {
		return body
	}
	return StripReasoningFromResponseBody(body)
}

// StripAnthropicThinkingFromResponsesBody 从已转成 Responses 形态的响应体里剥离 reasoning item。
//
// Responses 形态的思考内容在 output[] 里表现为 {"type":"reasoning", ...}。
func StripAnthropicThinkingFromResponsesBody(account *Account, body []byte) []byte {
	if !identityDisguiseStripReasoning(account) {
		return body
	}
	if !gjson.ValidBytes(body) {
		return body
	}
	output := gjson.GetBytes(body, "output")
	if !output.IsArray() {
		return body
	}
	items := output.Array()
	hasReasoning := false
	for _, it := range items {
		if it.Get("type").String() == "reasoning" {
			hasReasoning = true
			break
		}
	}
	if !hasReasoning {
		return body
	}
	kept := make([]any, 0, len(items))
	for _, it := range items {
		if it.Get("type").String() == "reasoning" {
			continue
		}
		kept = append(kept, it.Value())
	}
	raw, err := json.Marshal(kept)
	if err != nil {
		return body
	}
	out, err := sjson.SetRawBytes(body, "output", raw)
	if err != nil {
		return body
	}
	return out
}

// anthropicBodyHasThinking 报告 Anthropic 响应体是否含 thinking 块（供单测与流式状态初始化）。
func anthropicBodyHasThinking(body []byte) bool {
	content := gjson.GetBytes(body, "content")
	if !content.IsArray() {
		return false
	}
	for _, b := range content.Array() {
		t := b.Get("type").String()
		if t == "thinking" || t == "redacted_thinking" {
			return true
		}
	}
	return false
}

// anthropicSSEDataLine 判断一行是否为 SSE 的 data 行（并取出其 JSON 载荷）。
func anthropicSSEDataLine(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "data:") {
		return "", false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" || payload == "[DONE]" {
		return "", false
	}
	return payload, true
}

// anthropicSSEHasThinkingStart 判断该 SSE 载荷是否是 thinking 块的开始事件。
func anthropicSSEHasThinkingStart(payload string) bool {
	if !gjson.Valid(payload) {
		return false
	}
	if gjson.Get(payload, "type").String() != "content_block_start" {
		return false
	}
	ct := gjson.Get(payload, "content_block.type").String()
	return ct == "thinking" || ct == "redacted_thinking"
}

// anthropicSSEIsThinkingDelta 判断该 SSE 载荷是否是 thinking 的增量事件。
func anthropicSSEIsThinkingDelta(payload string) bool {
	if !gjson.Valid(payload) {
		return false
	}
	if gjson.Get(payload, "type").String() != "content_block_delta" {
		return false
	}
	dt := gjson.Get(payload, "delta.type").String()
	return dt == "thinking_delta" || dt == "signature_delta"
}


// nativeStreamDisguiseFlags 返回该账号在 Anthropic 原生流式路径上的两个伪装开关。
//
// 独立小函数（而非在流式循环里反复调 AccountIdentityDisguiseEffective）：
// 流式路径每行都会走到，开关必须在循环外取一次 —— 否则每行都读一次 extra 快照。
func nativeStreamDisguiseFlags(account *Account) (rewriteModel bool, stripReasoning bool) {
	_, rewriteModel, stripReasoning = AccountIdentityDisguiseEffective(account)
	return rewriteModel, stripReasoning
}

// anthropicStreamEventIsThinkingStart 报告该 Anthropic 流事件是否是 thinking 块的开始。
func anthropicStreamEventIsThinkingStart(event *apicompat.AnthropicStreamEvent) bool {
	if event == nil || event.Type != "content_block_start" || event.ContentBlock == nil {
		return false
	}
	t := event.ContentBlock.Type
	return t == "thinking" || t == "redacted_thinking"
}

// anthropicStreamEventIsThinking 报告该 Anthropic 流事件是否应被思考剥离丢弃。
//
// 丢弃两类：thinking 块开始、thinking 增量。
// 配对的 content_block_stop 由调用方按状态位单独处理（它本身不带类型信息）。
func anthropicStreamEventIsThinking(event *apicompat.AnthropicStreamEvent) bool {
	if event == nil {
		return false
	}
	if anthropicStreamEventIsThinkingStart(event) {
		return true
	}
	if event.Type == "content_block_delta" && event.Delta != nil {
		return event.Delta.Type == "thinking_delta" || event.Delta.Type == "signature_delta"
	}
	return false
}

// identityDisguiseStripReasoning 报告该账号是否开启思考剥离（L3）。
//
// 闸门收口在 helper 内部（而非调用点）：调用点若漏判，helper 仍会自己挡住，
// 不会出现「某个调用点忘了判断 → 思考内容直接泄漏给客户端」。
func identityDisguiseStripReasoning(account *Account) bool {
	_, _, strip := AccountIdentityDisguiseEffective(account)
	return strip
}
