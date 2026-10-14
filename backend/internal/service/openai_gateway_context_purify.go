package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultRawCCPurifyTriggerTokens = 16000
	defaultRawCCPurifyRedlineTokens = 64000

	rawCCPurifyRecentRounds       = 4
	rawCCPurifyBudget             = 50 * time.Millisecond
	rawCCPurifyWorkDeadline       = 45 * time.Millisecond
	rawCCPurifyMaxBodyBytes       = 4 * 1024 * 1024
	rawCCPurifyToolTextMaxBytes   = 4 * 1024
	rawCCPurifyAttachmentMinBytes = 8 * 1024
	rawCCPurifyHistoryMinBytes    = 6 * 1024
)

type rawCCPurifyMessage map[string]json.RawMessage

type rawCCPurifyTextTransform func(string) (string, bool)

// purifyRawCCMessages 对 raw Chat Completions 的 messages 做纯本地、抽取式提纯。
// 它不删除消息，因此 tool_calls/tool_call_id 配对和消息顺序保持不变；system/developer
// 指令、最近几轮以及每个旧 user 消息的首尾核心内容也不会被丢弃。
func purifyRawCCMessages(body []byte, account *Account, model string) (purifiedBody []byte, rejected bool, err error) {
	startedAt := time.Now()
	if len(body) > rawCCPurifyMaxBodyBytes {
		// 病态大请求不进入 JSON/tokenizer 全量扫描，避免网关被本地提纯阻塞。
		return body, false, nil
	}

	triggerTokens, redlineTokens := rawCCPurifyThresholds(account)
	estimatedTokens, err := EstimateTierRoutingInputTokens(model, TierRoutingProtocolChatCompletions, body)
	if err != nil {
		return nil, false, fmt.Errorf("estimate raw chat completions input tokens: %w", err)
	}
	if estimatedTokens < triggerTokens {
		return body, false, nil
	}
	if rawCCPurifyOutOfTime(startedAt) {
		return rawCCPurifyTimeoutFallback(body, estimatedTokens, redlineTokens)
	}

	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, false, fmt.Errorf("parse raw chat completions request for context purification: %w", err)
	}
	messagesRaw, ok := request["messages"]
	if !ok {
		return nil, false, fmt.Errorf("raw chat completions request has no messages")
	}
	var messages []rawCCPurifyMessage
	if err := json.Unmarshal(messagesRaw, &messages); err != nil {
		return nil, false, fmt.Errorf("parse raw chat completions messages for context purification: %w", err)
	}
	if rawCCPurifyOutOfTime(startedAt) {
		return rawCCPurifyTimeoutFallback(body, estimatedTokens, redlineTokens)
	}

	recentStart := rawCCPurifyRecentStart(messages, rawCCPurifyRecentRounds)
	changed := false

	// 先记录同名附件的最后版本，后续旧版本只保留文件名和抽取片段。
	latestAttachment := make(map[string]int)
	for i := len(messages) - 1; i >= 0; i-- {
		if rawCCPurifyOutOfTime(startedAt) {
			return body, false, nil
		}
		name := strings.ToLower(rawCCPurifyMessageAttachmentName(messages[i]))
		if name == "" {
			continue
		}
		if _, exists := latestAttachment[name]; !exists {
			latestAttachment[name] = i
		}
	}

	// 1. 最低风险：仅截断最近 K 轮以外的冗长 tool/function 结果；配对 ID 不动。
	for i := range messages {
		if rawCCPurifyOutOfTime(startedAt) {
			return body, false, nil
		}
		role := rawCCPurifyMessageRole(messages[i])
		if i >= recentStart || rawCCPurifyIsInstructionRole(role) || (role != "tool" && role != "function") {
			continue
		}
		if rawCCPurifyTransformMessageContent(messages[i], rawCCPurifyToolText) {
			changed = true
		}
	}

	// 2. 超长附件只保留文件名、首尾片段和命中约束/需求/接口等关键词的原文行。
	for i := range messages {
		if rawCCPurifyOutOfTime(startedAt) {
			return body, false, nil
		}
		role := rawCCPurifyMessageRole(messages[i])
		if i >= recentStart || rawCCPurifyIsInstructionRole(role) {
			continue
		}
		if rawCCPurifyTransformMessageContent(messages[i], func(text string) (string, bool) {
			name, isAttachment := rawCCPurifyAttachmentName(text)
			if !isAttachment {
				return text, false
			}
			superseded := name != "" && latestAttachment[strings.ToLower(name)] > i
			return rawCCPurifyAttachmentText(text, name, superseded), true
		}) {
			changed = true
		}
	}

	// 3. 精确重复的旧 assistant 内容用占位行代替；不碰 user 锚点和 tool 调用消息。
	seenAssistantContent := make(map[[sha256.Size]byte]struct{})
	for i := len(messages) - 1; i >= 0; i-- {
		if rawCCPurifyOutOfTime(startedAt) {
			return body, false, nil
		}
		message := messages[i]
		if rawCCPurifyMessageRole(message) != "assistant" || len(bytes.TrimSpace(message["tool_calls"])) > 0 {
			continue
		}
		content := bytes.TrimSpace(message["content"])
		if len(content) < 512 {
			continue
		}
		digest := sha256.Sum256(content)
		if i < recentStart {
			if _, duplicate := seenAssistantContent[digest]; duplicate {
				replacement, _ := json.Marshal("[重复内容已省略：与后续 assistant 消息一致]")
				message["content"] = replacement
				changed = true
				continue
			}
		}
		seenAssistantContent[digest] = struct{}{}
	}

	// 4. 对更早轮次做抽取式压缩，保留首尾和决策/约束/需求/接口原文行。
	for i := range messages {
		if rawCCPurifyOutOfTime(startedAt) {
			return body, false, nil
		}
		role := rawCCPurifyMessageRole(messages[i])
		if i >= recentStart || rawCCPurifyIsInstructionRole(role) || role == "tool" || role == "function" {
			continue
		}
		if rawCCPurifyTransformMessageContent(messages[i], rawCCPurifyHistoryText) {
			changed = true
		}
	}

	result := body
	if changed {
		if rawCCPurifyOutOfTime(startedAt) {
			return body, false, nil
		}
		encodedMessages, err := json.Marshal(messages)
		if err != nil {
			return nil, false, fmt.Errorf("marshal purified raw chat completions messages: %w", err)
		}
		request["messages"] = encodedMessages
		result, err = json.Marshal(request)
		if err != nil {
			return nil, false, fmt.Errorf("marshal purified raw chat completions request: %w", err)
		}
	}

	if rawCCPurifyOutOfTime(startedAt) {
		return rawCCPurifyTimeoutFallback(body, estimatedTokens, redlineTokens)
	}
	purifiedTokens := estimatedTokens
	if changed {
		purifiedTokens, err = EstimateTierRoutingInputTokens(model, TierRoutingProtocolChatCompletions, result)
		if err != nil {
			return nil, false, fmt.Errorf("estimate purified raw chat completions input tokens: %w", err)
		}
	}
	return result, purifiedTokens > redlineTokens, nil
}

func rawCCPurifyThresholds(account *Account) (int, int) {
	triggerTokens := defaultRawCCPurifyTriggerTokens
	redlineTokens := defaultRawCCPurifyRedlineTokens
	if account == nil {
		return triggerTokens, redlineTokens
	}
	if configured := account.getExtraInt("purify_trigger_tokens"); configured > 0 {
		triggerTokens = configured
	}
	if configured := account.getExtraInt("purify_redline_tokens"); configured > 0 {
		redlineTokens = configured
	}
	return triggerTokens, redlineTokens
}

func rawCCPurifyOutOfTime(startedAt time.Time) bool {
	return time.Since(startedAt) >= rawCCPurifyWorkDeadline
}

func rawCCPurifyTimeoutFallback(body []byte, estimatedTokens, redlineTokens int) ([]byte, bool, error) {
	if estimatedTokens <= redlineTokens {
		return body, false, nil
	}
	// 已经得到同一量尺的超红线结果，但预算不足以安全完成全量规则时：若请求里
	// 连最低风险的 tool/附件候选都没有，body 未变，初次估算即可作为提纯后估算；
	// 否则直接放行，避免在不完整扫描下误删有用内容。
	lowerBody := bytes.ToLower(body)
	for _, marker := range [][]byte{
		[]byte(`"tool"`), []byte(`"function"`), []byte(`"filename"`), []byte(`"file_name"`),
		[]byte("attachment"), []byte("begin file"), []byte("文件名"), []byte("附件"),
	} {
		if bytes.Contains(lowerBody, marker) {
			return body, false, nil
		}
	}
	return body, true, nil
}

func rawCCPurifyRecentStart(messages []rawCCPurifyMessage, rounds int) int {
	if rounds <= 0 {
		return len(messages)
	}
	remaining := rounds
	for i := len(messages) - 1; i >= 0; i-- {
		if rawCCPurifyMessageRole(messages[i]) != "user" {
			continue
		}
		remaining--
		if remaining == 0 {
			return i
		}
	}
	return 0
}

func rawCCPurifyMessageRole(message rawCCPurifyMessage) string {
	var role string
	_ = json.Unmarshal(message["role"], &role)
	return strings.ToLower(strings.TrimSpace(role))
}

func rawCCPurifyIsInstructionRole(role string) bool {
	return role == "system" || role == "developer"
}

func rawCCPurifyTransformMessageContent(message rawCCPurifyMessage, transform rawCCPurifyTextTransform) bool {
	content, exists := message["content"]
	if !exists {
		return false
	}

	var text string
	if err := json.Unmarshal(content, &text); err == nil {
		updated, changed := transform(text)
		if !changed {
			return false
		}
		encoded, _ := json.Marshal(updated)
		message["content"] = encoded
		return true
	}

	var parts []json.RawMessage
	if err := json.Unmarshal(content, &parts); err != nil {
		return false
	}
	changed := false
	for i, part := range parts {
		var plainText string
		if err := json.Unmarshal(part, &plainText); err == nil {
			updated, partChanged := transform(plainText)
			if partChanged {
				parts[i], _ = json.Marshal(updated)
				changed = true
			}
			continue
		}

		var object map[string]json.RawMessage
		if err := json.Unmarshal(part, &object); err != nil {
			continue
		}
		objectChanged := false
		for _, field := range []string{"text", "content", "output"} {
			var fieldText string
			if err := json.Unmarshal(object[field], &fieldText); err != nil {
				continue
			}
			updated, fieldChanged := transform(fieldText)
			if !fieldChanged {
				continue
			}
			object[field], _ = json.Marshal(updated)
			objectChanged = true
		}
		if objectChanged {
			parts[i], _ = json.Marshal(object)
			changed = true
		}
	}
	if !changed {
		return false
	}
	message["content"], _ = json.Marshal(parts)
	return true
}

func rawCCPurifyToolText(text string) (string, bool) {
	if len(text) <= rawCCPurifyToolTextMaxBytes {
		return text, false
	}
	head := rawCCPurifyPrefix(text, 2304)
	tail := rawCCPurifySuffix(text, 768)
	return fmt.Sprintf("%s\n[工具结果已截断：原文 %d 字节，仅保留首尾片段]\n%s", head, len(text), tail), true
}

func rawCCPurifyMessageAttachmentName(message rawCCPurifyMessage) string {
	content := message["content"]
	var text string
	if err := json.Unmarshal(content, &text); err == nil {
		name, _ := rawCCPurifyAttachmentName(text)
		return name
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(content, &parts); err != nil {
		return ""
	}
	for _, part := range parts {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(part, &object); err != nil {
			continue
		}
		for _, field := range []string{"filename", "file_name", "name"} {
			if err := json.Unmarshal(object[field], &text); err == nil && strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
		}
		for _, field := range []string{"text", "content"} {
			if err := json.Unmarshal(object[field], &text); err == nil {
				if name, attachment := rawCCPurifyAttachmentName(text); attachment && name != "" {
					return name
				}
			}
		}
	}
	return ""
}

func rawCCPurifyAttachmentName(text string) (string, bool) {
	if len(text) < rawCCPurifyAttachmentMinBytes {
		return "", false
	}
	head := rawCCPurifyPrefix(text, 4096)
	lowerHead := strings.ToLower(head)
	markers := []string{
		"filename:", "file name:", "attachment", "begin file", "<file",
		"文件名：", "文件名:", "附件", "文件全文",
	}
	attachment := false
	for _, marker := range markers {
		if strings.Contains(lowerHead, marker) {
			attachment = true
			break
		}
	}
	if !attachment {
		return "", false
	}

	nameMarkers := []string{"filename:", "file name:", "文件名：", "文件名:", "path:", "路径：", "路径:", "file:"}
	for _, marker := range nameMarkers {
		index := strings.Index(lowerHead, marker)
		if index < 0 {
			continue
		}
		value := strings.TrimSpace(head[index+len(marker):])
		if lineEnd := strings.IndexByte(value, '\n'); lineEnd >= 0 {
			value = value[:lineEnd]
		}
		value = strings.Trim(strings.TrimSpace(value), "`'\"<>[]()")
		if value != "" && len(value) <= 512 {
			return value, true
		}
	}
	return "", true
}

func rawCCPurifyAttachmentText(text, name string, superseded bool) string {
	if name == "" {
		name = "（未提供）"
	}
	status := "超长附件已抽取式压缩"
	if superseded {
		status = "旧附件版本已被后续同名文件覆盖"
	}
	critical := rawCCPurifyCriticalLines(text, 12)
	return fmt.Sprintf(
		"[附件提纯]\n文件名: %s\n摘要: %s；原文 %d 字节，仅保留关键原文片段。\n--- 开头片段 ---\n%s\n--- 关键行 ---\n%s\n--- 结尾片段 ---\n%s",
		name,
		status,
		len(text),
		rawCCPurifyPrefix(text, 1536),
		critical,
		rawCCPurifySuffix(text, 768),
	)
}

func rawCCPurifyHistoryText(text string) (string, bool) {
	if len(text) <= rawCCPurifyHistoryMinBytes || strings.HasPrefix(strings.TrimSpace(text), "[附件提纯]") {
		return text, false
	}
	critical := rawCCPurifyCriticalLines(text, 12)
	return fmt.Sprintf(
		"[旧轮次抽取式压缩：原文 %d 字节]\n--- 核心开头 ---\n%s\n--- 决策/约束/需求/接口行 ---\n%s\n--- 核心结尾 ---\n%s",
		len(text),
		rawCCPurifyPrefix(text, 1536),
		critical,
		rawCCPurifySuffix(text, 768),
	), true
}

func rawCCPurifyCriticalLines(text string, maxLines int) string {
	if maxLines <= 0 {
		return "（无额外关键词行）"
	}
	samples := []string{rawCCPurifyPrefix(text, 64*1024)}
	if len(text) > 64*1024 {
		samples = append(samples, rawCCPurifySuffix(text, 64*1024))
	}
	keywords := []string{
		"decision", "decided", "constraint", "must", "require", "requirement", "interface", "acceptance", "error", "failed", "forbid",
		"决定", "决策", "约束", "必须", "需求", "接口", "验收", "错误", "失败", "禁止",
	}
	seen := make(map[string]struct{})
	lines := make([]string, 0, maxLines)
	for _, sample := range samples {
		for len(sample) > 0 && len(lines) < maxLines {
			line := sample
			if end := strings.IndexByte(sample, '\n'); end >= 0 {
				line = sample[:end]
				sample = sample[end+1:]
			} else {
				sample = ""
			}
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			lower := strings.ToLower(trimmed)
			important := false
			for _, keyword := range keywords {
				if strings.Contains(lower, keyword) {
					important = true
					break
				}
			}
			if !important {
				continue
			}
			trimmed = rawCCPurifyPrefix(trimmed, 320)
			if _, duplicate := seen[trimmed]; duplicate {
				continue
			}
			seen[trimmed] = struct{}{}
			lines = append(lines, trimmed)
		}
	}
	if len(lines) == 0 {
		return "（无额外关键词行）"
	}
	return strings.Join(lines, "\n")
}

func rawCCPurifyPrefix(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end]
}

func rawCCPurifySuffix(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	start := len(text) - maxBytes
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return text[start:]
}
