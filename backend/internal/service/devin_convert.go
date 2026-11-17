package service

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/tidwall/gjson"
)

// ChatUsage 是 Devin 响应用量的归一表示，四个计数分别对应上游 usage 的
// input / output / cacheWrite / cacheRead，由 DevinUsageToOpenAI 转成 OpenAIUsage。
type ChatUsage struct {
	Input      int64
	Output     int64
	CacheWrite int64
	CacheRead  int64
}

// BuildDevinChatParams 把 OpenAI Chat Completions 请求体转成 Devin 请求参数。
// cascadeID 为空时用 crypto/rand 生成 v4 UUID。
func BuildDevinChatParams(body []byte, modelUID, cascadeID string) (*DevinChatParams, error) {
	if !gjson.ValidBytes(body) {
		return nil, fmt.Errorf("devin: invalid chat body JSON")
	}
	if cascadeID == "" {
		id, err := newDevinCascadeID()
		if err != nil {
			return nil, err
		}
		cascadeID = id
	}

	params := &DevinChatParams{
		ModelUID:    modelUID,
		CascadeID:   cascadeID,
		Stop:        parseDevinStop(gjson.GetBytes(body, "stop")),
		MaxTokens:   parseDevinOptionalInt(body),
		Temperature: parseDevinOptionalFloat(body, "temperature"),
		TopP:        parseDevinOptionalFloat(body, "top_p"),
	}

	// messages：system/developer 拼进 SystemPrompt，不写入 prompts；其余角色生成 prompt。
	var systemParts []string
	if msgs := gjson.GetBytes(body, "messages"); msgs.IsArray() {
		for i, m := range msgs.Array() {
			role := m.Get("role").String()
			switch role {
			case "system", "developer":
				// system/developer 只拼入 SystemPrompt（纯文本），无 ChatMessagePrompt 承载图片。
				text, _, err := devinMessageText(m)
				if err != nil {
					return nil, err
				}
				if text != "" {
					systemParts = append(systemParts, text)
				}
			case "user":
				text, images, err := devinMessageText(m)
				if err != nil {
					return nil, err
				}
				params.Prompts = append(params.Prompts, ChatMessagePrompt{
					MessageID: devinMessageID(cascadeID, i, role),
					Source:    ChatMessageSourceUser,
					Prompt:    text,
					Images:    images,
				})
			case "assistant":
				prompt, err := devinAssistantPrompt(m, cascadeID, i, role)
				if err != nil {
					return nil, err
				}
				params.Prompts = append(params.Prompts, prompt)
			case "tool":
				text, images, err := devinMessageText(m)
				if err != nil {
					return nil, err
				}
				params.Prompts = append(params.Prompts, ChatMessagePrompt{
					MessageID:         devinMessageID(cascadeID, i, role),
					Source:            ChatMessageSourceTool,
					Prompt:            text,
					ToolCallId:        m.Get("tool_call_id").String(),
					ToolResultIsError: devinBoolField(m, "is_error", "tool_result_is_error"),
					Images:            images,
				})
			}
		}
	}
	params.SystemPrompt = strings.Join(systemParts, "\n\n")

	// tools：parameters 序列化为字符串 JSON；缺省 {} 时用 {"type":"object"}。
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() {
		for _, t := range tools.Array() {
			schema, err := devinCanonicalJSON(t.Get("function.parameters"), `{"type":"object"}`)
			if err != nil {
				return nil, fmt.Errorf("devin: invalid tool parameters: %w", err)
			}
			params.Tools = append(params.Tools, ChatToolDefinition{
				Name:             t.Get("function.name").String(),
				Description:      t.Get("function.description").String(),
				JSONSchemaString: string(schema),
				Strict:           false,
			})
		}
	}

	// tool_choice：字符串 required→any，其余原样；对象取 function.name。
	if tc := gjson.GetBytes(body, "tool_choice"); tc.Exists() && tc.Type != gjson.Null {
		choice := &ChatToolChoice{}
		switch {
		case tc.Type == gjson.String:
			if tc.String() == "required" {
				choice.OptionName = "any"
			} else {
				choice.OptionName = tc.String()
			}
		case tc.IsObject():
			choice.ToolName = tc.Get("function.name").String()
		}
		params.ToolChoice = choice
	}

	return params, nil
}

// devinAssistantPrompt 构造 assistant prompt：文本内容、thinking 与 tool_calls。
func devinAssistantPrompt(m gjson.Result, cascadeID string, index int, role string) (ChatMessagePrompt, error) {
	text, images, err := devinMessageText(m)
	if err != nil {
		return ChatMessagePrompt{}, err
	}
	prompt := ChatMessagePrompt{
		MessageID: "bot-" + devinMessageID(cascadeID, index, role),
		Source:    ChatMessageSourceSystem,
		Prompt:    text,
		Images:    images,
		Thinking:  m.Get("reasoning_content").String(),
	}
	if prompt.Thinking == "" {
		prompt.Thinking = m.Get("thinking").String()
	}
	if tcs := m.Get("tool_calls"); tcs.IsArray() {
		for _, tc := range tcs.Array() {
			argsRaw := tc.Get("function.arguments").String()
			if argsRaw == "" {
				argsRaw = "{}"
			}
			var parsed any
			if err := json.Unmarshal([]byte(argsRaw), &parsed); err != nil {
				return ChatMessagePrompt{}, fmt.Errorf("devin: invalid tool call arguments: %w", err)
			}
			argsJSON, err := json.Marshal(parsed)
			if err != nil {
				return ChatMessagePrompt{}, fmt.Errorf("devin: marshal tool call arguments: %w", err)
			}
			prompt.ToolCalls = append(prompt.ToolCalls, ChatToolCall{
				ID:            tc.Get("id").String(),
				Name:          tc.Get("function.name").String(),
				ArgumentsJSON: string(argsJSON),
			})
		}
	}
	return prompt, nil
}

// devinMessageText 提取消息文本与图片。content 为数组时拼接所有 type=="text" 的片段；
// data URI 图片（OpenAI image_url 形状或 Anthropic base64 source 形状）解析为 ImageData；
// 远程 URL（http/https）与非 data URI 图片失败关闭，不静默丢弃。
func devinMessageText(m gjson.Result) (text string, images []ImageData, err error) {
	content := m.Get("content")
	if !content.Exists() || content.Type == gjson.Null {
		return "", nil, nil
	}
	if content.Type == gjson.String {
		return content.String(), nil, nil
	}
	if content.IsArray() {
		var b strings.Builder
		for _, part := range content.Array() {
			switch part.Get("type").String() {
			case "text":
				b.WriteString(part.Get("text").String())
			case "image_url":
				img, imgErr := devinImageFromURL(part.Get("image_url.url").String())
				if imgErr != nil {
					return "", nil, imgErr
				}
				images = append(images, img)
			case "image":
				img, imgErr := devinImageFromAnthropic(part)
				if imgErr != nil {
					return "", nil, imgErr
				}
				images = append(images, img)
			default:
				if u := part.Get("image_url.url"); u.Exists() {
					img, imgErr := devinImageFromURL(u.String())
					if imgErr != nil {
						return "", nil, imgErr
					}
					images = append(images, img)
				} else if part.Get("image_url").Exists() || part.Get("image").Exists() || part.Get("source.type").Exists() {
					return "", nil, fmt.Errorf("devin: image input is not supported")
				}
			}
		}
		return b.String(), images, nil
	}
	// 非字符串非数组（如对象）按原始 JSON 文本透传，与参考实现 tool 分支一致。
	return content.Raw, nil, nil
}

// devinImageFromURL 解析 OpenAI image_url.url：仅接受 data:<mime>;base64,<b64>；
// http/https 远程 URL 与其他形状一律失败关闭，不静默丢弃。
func devinImageFromURL(raw string) (ImageData, error) {
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return ImageData{}, fmt.Errorf("devin: remote image url is not supported (only base64 data uris)")
	}
	if strings.HasPrefix(raw, "data:") {
		rest := strings.TrimPrefix(raw, "data:")
		if i := strings.Index(rest, ";base64,"); i >= 0 {
			mime, b64 := rest[:i], rest[i+len(";base64,"):]
			if mime != "" && b64 != "" {
				return ImageData{Base64Data: b64, MimeType: mime}, nil
			}
		}
	}
	return ImageData{}, fmt.Errorf("devin: image url is not a base64 data uri")
}

// devinImageFromAnthropic 解析 Anthropic image 块：source.type=="base64" 时取
// media_type 与 data；远程 url source 失败关闭。
func devinImageFromAnthropic(part gjson.Result) (ImageData, error) {
	src := part.Get("source")
	if src.Get("type").String() == "base64" {
		mime := src.Get("media_type").String()
		data := src.Get("data").String()
		if mime != "" && data != "" {
			return ImageData{Base64Data: data, MimeType: mime}, nil
		}
	}
	if u := src.Get("url").String(); strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return ImageData{}, fmt.Errorf("devin: remote image url is not supported (only base64 data uris)")
	}
	return ImageData{}, fmt.Errorf("devin: image source is not a base64 data uri")
}

// devinMessageID 用 FNV-1a 32 位生成稳定 id：seed=cascadeID\0index\0role。
func devinMessageID(cascadeID string, index int, role string) string {
	h := fnv.New32a()
	seed := cascadeID + "\x00" + strconv.Itoa(index) + "\x00" + role
	_, _ = h.Write([]byte(seed))
	return fmt.Sprintf("%08x-0000-0000-0000-000000000000", h.Sum32())
}

// newDevinCascadeID 用 crypto/rand 生成标准 v4 UUID；随机数不可用则报错，绝不用时间戳拼接。
func newDevinCascadeID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("devin: generate cascade id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func parseDevinStop(r gjson.Result) []string {
	if !r.Exists() || r.Type == gjson.Null {
		return nil
	}
	if r.IsArray() {
		out := make([]string, 0, len(r.Array()))
		for _, s := range r.Array() {
			out = append(out, s.String())
		}
		return out
	}
	return []string{r.String()}
}

// parseDevinOptionalInt 优先 max_completion_tokens，其次 max_tokens。
func parseDevinOptionalInt(body []byte) *int {
	for _, key := range []string{"max_completion_tokens", "max_tokens"} {
		r := gjson.GetBytes(body, key)
		if r.Exists() && r.Type != gjson.Null {
			v := int(r.Int())
			return &v
		}
	}
	return nil
}

// parseDevinOptionalFloat 区分未设置（含 null）与显式 0：未设置返回 nil，显式 0 返回指向 0 的指针。
func parseDevinOptionalFloat(body []byte, key string) *float64 {
	r := gjson.GetBytes(body, key)
	if !r.Exists() || r.Type == gjson.Null {
		return nil
	}
	v := r.Float()
	return &v
}

func devinBoolField(m gjson.Result, keys ...string) bool {
	for _, key := range keys {
		if r := m.Get(key); r.Exists() && r.Type != gjson.Null {
			return r.Bool()
		}
	}
	return false
}

// devinCanonicalJSON 把字段的原始 JSON 规范化（parse 再 stringify），字段缺失或为 null 时用 fallback。
func devinCanonicalJSON(r gjson.Result, fallback string) ([]byte, error) {
	if !r.Exists() || r.Type == gjson.Null {
		return []byte(fallback), nil
	}
	var v any
	if err := json.Unmarshal([]byte(r.Raw), &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// AnthropicBodyToChatBody 把 /v1/messages 的 Anthropic 请求体归一为 OpenAI Chat Completions 请求体。
func AnthropicBodyToChatBody(body []byte) ([]byte, error) {
	var req apicompat.AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("devin: parse anthropic body: %w", err)
	}
	cc, err := apicompat.AnthropicToChatCompletionsRequest(&req)
	if err != nil {
		return nil, fmt.Errorf("devin: convert anthropic body: %w", err)
	}
	out, err := json.Marshal(cc)
	if err != nil {
		return nil, fmt.Errorf("devin: marshal chat body: %w", err)
	}
	return out, nil
}

// ChatToolCallDelta 是工具调用增量输出，Index 在本响应内稳定且从 0 开始。
type ChatToolCallDelta struct {
	ChatToolCall
	Index int
}

type devinPendingCall struct {
	call          ChatToolCallDelta
	emittedLength int
}

// ToolCallAccumulator 逐行移植 ref/tool-calls.ts：匿名片段归当前活跃调用，
// 有 name 无 id 且当前已有 name 时开启新调用；输出增量只含本次新增的 arguments 字节。
type ToolCallAccumulator struct {
	calls  []*devinPendingCall
	byID   map[string]*devinPendingCall
	active *devinPendingCall
}

func (a *ToolCallAccumulator) Add(deltas []ChatToolCall) ([]ChatToolCallDelta, error) {
	out := make([]ChatToolCallDelta, 0, len(deltas))
	for _, delta := range deltas {
		if delta.InvalidJSONErr != "" {
			return nil, fmt.Errorf("devin: upstream tool arguments are invalid")
		}
		var pending *devinPendingCall
		if delta.ID != "" {
			pending = a.byID[delta.ID]
		} else {
			pending = a.active
		}
		// 带 name 无 id 的片段若当前调用已有 name，则开启一个独立调用。
		if delta.ID == "" && delta.Name != "" && pending != nil && pending.call.Name != "" {
			pending = nil
		}
		if pending == nil && (delta.ID != "" || delta.Name != "") {
			pending = &devinPendingCall{
				call: ChatToolCallDelta{
					ChatToolCall: ChatToolCall{ID: delta.ID},
					Index:        len(a.calls),
				},
			}
			a.calls = append(a.calls, pending)
			if delta.ID != "" {
				if a.byID == nil {
					a.byID = make(map[string]*devinPendingCall)
				}
				a.byID[delta.ID] = pending
			}
		}
		if pending == nil {
			return nil, fmt.Errorf("devin: tool argument fragment has no call header")
		}
		a.active = pending

		if delta.Name != "" {
			if pending.call.Name != "" && pending.call.Name != delta.Name {
				return nil, fmt.Errorf("devin: tool call name changed")
			}
			pending.call.Name = delta.Name
		}
		if delta.IsCustomToolCall {
			pending.call.IsCustomToolCall = true
		}
		pending.call.ArgumentsJSON += delta.ArgumentsJSON
		if pending.call.Name != "" {
			next := pending.call
			next.ArgumentsJSON = pending.call.ArgumentsJSON[pending.emittedLength:]
			out = append(out, next)
			pending.emittedLength = len(pending.call.ArgumentsJSON)
		}
	}
	return out, nil
}

// Finish 校验每个调用：必须有 name，arguments 必须是合法 JSON 对象；
// 零参调用补发唯一的 "{}" 字节，且不重放已发出的片段。
func (a *ToolCallAccumulator) Finish() ([]ChatToolCallDelta, error) {
	out := make([]ChatToolCallDelta, 0)
	for _, pending := range a.calls {
		call := pending.call
		if call.Name == "" {
			return nil, fmt.Errorf("devin: tool call has no name")
		}
		args := call.ArgumentsJSON
		if args == "" {
			args = "{}"
		}
		var v any
		if err := json.Unmarshal([]byte(args), &v); err != nil {
			return nil, fmt.Errorf("devin: incomplete or invalid tool arguments")
		}
		if _, ok := v.(map[string]any); !ok {
			return nil, fmt.Errorf("devin: tool arguments must be a JSON object")
		}
		if call.ArgumentsJSON == "" {
			call.ArgumentsJSON = args
			out = append(out, call)
		}
	}
	return out, nil
}

// DevinStopReasonToOpenAI 映射停止原因：有工具调用→tool_calls，reason==3→length，其余 stop。
func DevinStopReasonToOpenAI(reason int32, hasToolCalls bool) string {
	if hasToolCalls {
		return "tool_calls"
	}
	if reason == 3 {
		return "length"
	}
	return "stop"
}

// DevinUsageToOpenAI 把 Devin 用量转成 OpenAIUsage；nil 输入返回零值。
func DevinUsageToOpenAI(u *ChatUsage) OpenAIUsage {
	if u == nil {
		return OpenAIUsage{}
	}
	return OpenAIUsage{
		InputTokens:              int(u.Input),
		OutputTokens:             int(u.Output),
		CacheCreationInputTokens: int(u.CacheWrite),
		CacheReadInputTokens:     int(u.CacheRead),
	}
}
