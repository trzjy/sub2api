package service

import (
	"encoding/json"
	"strings"
)

// CodeBuddy 出站请求体改写管线（§2.5）。
//
// 上游 copilot.tencent.com 是 Chat Completions 变体，但有几处与标准 OpenAI 不兼容：
//   - 拒绝非流式（必须本地聚合 SSE，见 forwardCodeBuddy）
//   - tool_choice 对象形式会 400（code=11101）
//   - developer 角色、reasoning_effort 档位、DeepSeek 系 thinking、system 指纹审核
//     都有各自的特殊处理。
//
// 本文件只做「纯函数改写」，不依赖网络/账号上下文，便于单测逐规则覆盖。

// CodeBuddySanitizePattern 是一条 system 指纹脱敏规则：命中 Substring 时按
// Replacement 替换。Replacement 为空串表示直接删除该片段。
type CodeBuddySanitizePattern struct {
	Substring   string
	Replacement string
}

// CodeBuddyRewriteOptions 控制 PrepareCodeBuddyBody 的改写行为。
type CodeBuddyRewriteOptions struct {
	// Sanitize 开启后清洗 system 消息中的指纹字段（默认开启，见 §2.5 规则 7）。
	Sanitize bool
	// ReplaceSystem 为最后手段：在 sanitize 之后再把 system 消息内容整体清空，
	// 彻底移除可能触发上游审核的指纹（见 §2.6 行 6 审核拦截的降级重试语义）。
	// 仅用于内容审核重试，不应在首轮请求启用。
	ReplaceSystem bool
	// Model 当前请求的模型名（用于 DeepSeek 系 thinking 注入等模型相关规则）。
	Model string
	// SupportedEfforts 模型支持的 reasoning_effort 档位；空表示未知，按全档位处理（不降级）。
	SupportedEfforts []string
	// SanitizePatterns 自定义脱敏规则；为空且 Sanitize=true 时使用内置默认规则。
	SanitizePatterns []CodeBuddySanitizePattern
}

// codeBuddyDefaultSanitizePatterns 是内置默认脱敏规则。CodeBuddy 上游对来自其它
// AI 助手（Claude Code / Codex / 官方 boilerplate）的「自报身份 / 注入模板」类指纹
// 有内容审核。此列表为保守起步集，待 PR2 验收用真实 CodeBuddy 审计反馈持续调优。
//
// 设计原则：只命中高度特异的框架指纹短语，避免误伤正常业务 system 内容。
var codeBuddyDefaultSanitizePatterns = []CodeBuddySanitizePattern{
	{Substring: "I'm Claude", Replacement: ""},
	{Substring: "I am Claude", Replacement: ""},
	{Substring: "As an AI assistant created by Anthropic", Replacement: ""},
	{Substring: "Claude Code is an AI assistant", Replacement: ""},
	{Substring: "You are a helpful assistant powered by Codex", Replacement: ""},
	{Substring: "This session is being recorded", Replacement: ""},
	{Substring: "Anthropic", Replacement: "the service provider"},
	{Substring: "OpenAI", Replacement: "the model vendor"},
}

// PrepareCodeBuddyBody 按 §2.5 顺序执行改写规则（含 Phase 0 校准新增的
// 「首条消息必须为 system」，D6），返回改写后的请求体。
// 任何单条规则失败时返回错误（不部分改写），保证调用方可安全替换出站 body。
func PrepareCodeBuddyBody(src []byte, opts CodeBuddyRewriteOptions) ([]byte, error) {
	if !json.Valid(src) {
		return nil, errCodeBuddyBadBody
	}
	var req map[string]any
	dec := json.NewDecoder(strings.NewReader(string(src)))
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil {
		return nil, errCodeBuddyBadBody
	}

	changed := false

	// 规则 1：强制 stream:true（上游拒绝非流式，非流式请求由 forwardCodeBuddy 本地聚合）。
	if v, ok := req["stream"]; !ok || v != true {
		req["stream"] = true
		changed = true
	}

	// 规则 2：tool_choice 归一化为字符串（对象形式 → 400 code=11101）。
	if c, ok := normalizeCodeBuddyToolChoice(req); ok {
		req["tool_choice"] = c
		changed = true
	}

	// 规则 3：developer 角色归一为上游认可角色（→ system）。
	if normalizeCodeBuddyDeveloperRole(req) {
		changed = true
	}

	// 规则 3b：保证首条消息为 system（Phase 0 D6）。
	if ensureCodeBuddySystemFirst(req) {
		changed = true
	}

	// 规则 4：DeepSeek 系模型注入 thinking.type=enabled（缺档补默认档）。
	if injectCodeBuddyThinking(req, opts.Model) {
		changed = true
	}

	// 规则 5：reasoning_effort 按模型 supportedEfforts 降级。
	if downgradeCodeBuddyReasoningEffort(req, opts.SupportedEfforts) {
		changed = true
	}

	// 规则 6：assistant 消息带 reasoning 痕迹时回填 reasoning_content（多轮一致性）。
	if backfillCodeBuddyReasoningContent(req) {
		changed = true
	}

	// 规则 7：system 指纹脱敏（默认开启，可配置关闭）。
	if opts.Sanitize {
		patterns := opts.SanitizePatterns
		if len(patterns) == 0 {
			patterns = codeBuddyDefaultSanitizePatterns
		}
		if sanitizeCodeBuddySystem(req, patterns) {
			changed = true
		}
	}

	// 规则 8（最后手段，仅审核重试）：整体清空 system 消息内容，彻底移除指纹。
	if opts.ReplaceSystem {
		if replaceCodeBuddySystemMessages(req) {
			changed = true
		}
	}

	if !changed {
		// 未改写时原样返回（保留调用方字节，避免重排空白）。
		return src, nil
	}
	out, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// normalizeCodeBuddyToolChoice 将对象形式的 tool_choice 归一为字符串形式。
// 返回 (value, true) 表示发生了改写。
//
// 转换规则：
//   - {type:"function", function:{name:"X"}} → "X"（保留「强制某工具」语义）
//   - {type:"function"}（无名）→ "auto"
//   - {type:"auto"|"none"|"required"} → 对应字符串
func normalizeCodeBuddyToolChoice(req map[string]any) (string, bool) {
	tc, ok := req["tool_choice"]
	if !ok {
		return "", false
	}
	m, ok := tc.(map[string]any)
	if !ok {
		// 已是字符串或标量，无需改写。
		return "", false
	}
	t, _ := m["type"].(string)
	switch t {
	case "function":
		if fn, ok := m["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok && strings.TrimSpace(name) != "" {
				return strings.TrimSpace(name), true
			}
		}
		return "auto", true
	case "auto", "none", "required":
		return t, true
	default:
		if t != "" {
			return t, true
		}
		return "auto", true
	}
}

// normalizeCodeBuddyDeveloperRole 将 messages 中的 developer 角色归一为 system。
func normalizeCodeBuddyDeveloperRole(req map[string]any) bool {
	msgs, ok := req["messages"].([]any)
	if !ok {
		return false
	}
	changed := false
	for _, raw := range msgs {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if strings.EqualFold(fmtSprint(msg["role"]), "developer") {
			msg["role"] = "system"
			changed = true
		}
	}
	return changed
}

// codeBuddyDefaultSystemPrompt 是首条 system 缺失时前置的最小 system 内容。
// 内容保持中性，避免引入任何上游指纹审核敏感词。
const codeBuddyDefaultSystemPrompt = "You are a helpful assistant."

// ensureCodeBuddySystemFirst 保证 messages[0] 为 system 角色。
//
// Phase 0 校准（D6）：intl 站点对首条非 system 的请求返回 400
// `{"code":11128,"msg":"first message is not system prompt"}`（见
// docs/evidence/codebuddy-intl）。此处缺失时前置一条最小 system，不改变原有消息
// 相对顺序；CN 是否同样强制由 Phase 2 cn 回归比对，暂两站点统一处理避免分叉。
func ensureCodeBuddySystemFirst(req map[string]any) bool {
	msgs, ok := req["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return false
	}
	if first, ok := msgs[0].(map[string]any); ok &&
		strings.EqualFold(fmtSprint(first["role"]), "system") {
		return false
	}
	sys := map[string]any{"role": "system", "content": codeBuddyDefaultSystemPrompt}
	req["messages"] = append([]any{sys}, msgs...)
	return true
}

// injectCodeBuddyThinking 对 DeepSeek 系模型注入 thinking.type=enabled。
// 模型名（不区分大小写）包含 "deepseek" 时触发；若已存在 thinking 但 type 缺失/空，补 enabled。
func injectCodeBuddyThinking(req map[string]any, model string) bool {
	if !strings.Contains(strings.ToLower(model), "deepseek") {
		return false
	}
	thinking, ok := req["thinking"].(map[string]any)
	if !ok {
		req["thinking"] = map[string]any{"type": "enabled"}
		return true
	}
	if t, _ := thinking["type"].(string); strings.TrimSpace(t) == "" {
		thinking["type"] = "enabled"
		return true
	}
	return false
}

// codeBuddyEffortRank 返回 reasoning_effort 档位的有序权重（off<minimal<low<medium<high<xhigh<max）。
// 未知档位返回 -1。
func codeBuddyEffortRank(e string) int {
	switch strings.ToLower(strings.TrimSpace(e)) {
	case "off":
		return 0
	case "minimal":
		return 1
	case "low":
		return 2
	case "medium":
		return 3
	case "high":
		return 4
	case "xhigh", "extrahigh":
		return 5
	case "max", "ultra":
		return 6
	default:
		return -1
	}
}

// normalizeCodeBuddyReasoningEffortValue 将单档位值按 supported 降级，返回 (值, 是否保留)。
// supported 为空表示未知（全档位均支持），原样保留；请求档位不在 supported 中时
// 降到不超过它的最高支持档位，若所有支持档位都高于请求档位则丢弃该字段。
func normalizeCodeBuddyReasoningEffortValue(value string, supported []string) (string, bool) {
	rank := codeBuddyEffortRank(value)
	if rank < 0 {
		return "", false
	}
	if len(supported) == 0 {
		return value, true
	}
	supportedRanks := make(map[int]string, len(supported))
	for _, s := range supported {
		if r := codeBuddyEffortRank(s); r >= 0 {
			supportedRanks[r] = s
		}
	}
	if _, ok := supportedRanks[rank]; ok {
		return value, true
	}
	bestRank := -1
	for r := rank; r >= 0; r-- {
		if _, ok := supportedRanks[r]; ok {
			bestRank = r
			break
		}
	}
	if bestRank < 0 {
		return "", false
	}
	// 还原成该档位的标准书写形式。
	for _, s := range supported {
		if codeBuddyEffortRank(s) == bestRank {
			return s, true
		}
	}
	return "", false
}

// downgradeCodeBuddyReasoningEffort 处理 reasoning_effort（snake/camel）与 reasoning.effort。
func downgradeCodeBuddyReasoningEffort(req map[string]any, supported []string) bool {
	changed := false
	for _, key := range []string{"reasoning_effort", "reasoningEffort"} {
		if v, ok := req[key].(string); ok && v != "" {
			norm, keep := normalizeCodeBuddyReasoningEffortValue(v, supported)
			if !keep {
				delete(req, key)
				changed = true
			} else if norm != v {
				req[key] = norm
				changed = true
			}
		}
	}
	if r, ok := req["reasoning"].(map[string]any); ok {
		if v, ok := r["effort"].(string); ok && v != "" {
			norm, keep := normalizeCodeBuddyReasoningEffortValue(v, supported)
			if !keep {
				delete(r, "effort")
				changed = true
			} else if norm != v {
				r["effort"] = norm
				changed = true
			}
		}
	}
	return changed
}

// backfillCodeBuddyReasoningContent assistant 消息若缺 reasoning_content 但带 reasoning
// 痕迹（reasoning 字段），回填 reasoning_content 以兼容上游多轮一致性要求。
func backfillCodeBuddyReasoningContent(req map[string]any) bool {
	msgs, ok := req["messages"].([]any)
	if !ok {
		return false
	}
	changed := false
	for _, raw := range msgs {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if !strings.EqualFold(fmtSprint(msg["role"]), "assistant") {
			continue
		}
		if _, ok := msg["reasoning_content"]; ok {
			continue
		}
		// reasoning 可能是字符串或嵌套结构；仅当为字符串时回填。
		if r, ok := msg["reasoning"].(string); ok && strings.TrimSpace(r) != "" {
			msg["reasoning_content"] = r
			changed = true
		}
	}
	return changed
}

// replaceCodeBuddySystemMessages 是 §2.6 行 6 审核拦截「替换/去除 system」的最后手段：
// 将全部 system 消息的 content 置空，彻底清除可能触发上游指纹审核的内容。
// 返回是否发生了改动。
func replaceCodeBuddySystemMessages(req map[string]any) bool {
	msgs, ok := req["messages"].([]any)
	if !ok {
		return false
	}
	changed := false
	for _, raw := range msgs {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if !strings.EqualFold(fmtSprint(msg["role"]), "system") {
			continue
		}
		if cur, _ := msg["content"].(string); cur != "" {
			msg["content"] = ""
			changed = true
		}
	}
	return changed
}

// sanitizeCodeBuddySystem 对 system 角色消息做指纹脱敏。
func sanitizeCodeBuddySystem(req map[string]any, patterns []CodeBuddySanitizePattern) bool {
	msgs, ok := req["messages"].([]any)
	if !ok || len(patterns) == 0 {
		return false
	}
	changed := false
	for _, raw := range msgs {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if !strings.EqualFold(fmtSprint(msg["role"]), "system") {
			continue
		}
		if sanitizeCodeBuddyContent(msg, patterns) {
			changed = true
		}
	}
	return changed
}

// sanitizeCodeBuddyContent 改写单条消息的 content（支持字符串与多 part 数组两种形态）。
func sanitizeCodeBuddyContent(msg map[string]any, patterns []CodeBuddySanitizePattern) bool {
	changed := false
	switch content := msg["content"].(type) {
	case string:
		if out, c := applyCodeBuddySanitizePatterns(content, patterns); c {
			msg["content"] = out
			changed = true
		}
	case []any:
		for _, part := range content {
			p, ok := part.(map[string]any)
			if !ok {
				continue
			}
			text, ok := p["text"].(string)
			if !ok {
				continue
			}
			if out, c := applyCodeBuddySanitizePatterns(text, patterns); c {
				p["text"] = out
				changed = true
			}
		}
	}
	return changed
}

func applyCodeBuddySanitizePatterns(text string, patterns []CodeBuddySanitizePattern) (string, bool) {
	out := text
	changed := false
	for _, p := range patterns {
		if p.Substring == "" {
			continue
		}
		if strings.Contains(out, p.Substring) {
			out = strings.ReplaceAll(out, p.Substring, p.Replacement)
			changed = true
		}
	}
	return out, changed
}

// fmtSprint 安全地将任意 JSON 值格式化为字符串（用于 role 等字段的非字符串兜底）。
func fmtSprint(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return strings.TrimSpace(jsonStringify(v))
	}
}

func jsonStringify(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// errCodeBuddyBadBody 表示请求体不是合法 JSON。
var errCodeBuddyBadBody = &codeBuddyRewriteError{msg: "codebuddy: request body is not valid JSON"}

type codeBuddyRewriteError struct{ msg string }

func (e *codeBuddyRewriteError) Error() string { return e.msg }
