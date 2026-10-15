package apicompat

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// 缺陷3：json_object 模式下 codex system 文本被重复注入（"X\n\nX"）。
//
// 生产可达链（本文件逐段复现，不用 mock）：
//  1. service/openai_codex_transform.go extractSystemMessagesFromInput 对
//     role=="system" item：文本 append 进 systemTexts → 合并写入
//     reqBody["instructions"]，同时把 role 改写为 developer 并保留在 input 中
//     （omitPromoted==false 时才从 input 删除）。
//  2. omitPromoted 取值见 service/openai_gateway_forward.go：
//     text.format.type=="json_object" → false（生产可达的 Responses JSON 模式）。
//  3. 本包 ResponsesToChatCompletionsRequest 把 instructions 放进 messages[0]，
//     再经 mergeLeadingSystemMessages 与 hoisted 的 developer 文本无条件
//     "\n\n" 拼接。
//
// 失效形态是静默的：上游 vLLM 只拒绝「位置错误」的 system，不拒绝超长/重复的
// system，于是请求成功、语义被悄悄改变。

// applyCodexSystemPromotion 复刻 extractSystemMessagesFromInput 在
// omitPromoted==false 时的行为：文本提升进 instructions，item 保留并改写为
// developer。这是 json_object 模式下的真实写点。
func applyCodexSystemPromotion(t *testing.T, reqBody map[string]any) {
	t.Helper()
	input, ok := reqBody["input"].([]any)
	require.True(t, ok, "input must be a list")
	var systemTexts []string
	for _, item := range input {
		m, ok := item.(map[string]any)
		if !ok || m["role"] != "system" {
			continue
		}
		parts, _ := m["content"].([]any)
		var sb strings.Builder
		for _, p := range parts {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := pm["text"].(string); ok {
				sb.WriteString(text)
			}
		}
		if text := sb.String(); text != "" {
			systemTexts = append(systemTexts, text)
		}
		m["role"] = "developer"
	}
	if len(systemTexts) == 0 {
		return
	}
	extracted := strings.Join(systemTexts, "\n\n")
	if existing, ok := reqBody["instructions"].(string); ok && strings.TrimSpace(existing) != "" {
		reqBody["instructions"] = extracted + "\n\n" + existing
	} else {
		reqBody["instructions"] = extracted
	}
}

// convertPromoted 走完整链路：codex promotion → 本包转换。
func convertPromoted(t *testing.T, reqBody map[string]any) []ChatMessage {
	t.Helper()
	raw, err := json.Marshal(reqBody)
	require.NoError(t, err)
	var req ResponsesRequest
	require.NoError(t, json.Unmarshal(raw, &req))
	msgs, err := ResponsesToChatCompletionsRequest(&req)
	require.NoError(t, err)
	return msgs.Messages
}

func promotedJSONObjectBody(systemText string) map[string]any {
	return map[string]any{
		"model": "gpt-5.5",
		"text":  map[string]any{"format": map[string]any{"type": "json_object"}},
		"input": []any{
			map[string]any{
				"type": "message", "role": "system",
				"content": []any{map[string]any{"type": "input_text", "text": systemText}},
			},
			map[string]any{
				"type": "message", "role": "user",
				"content": []any{map[string]any{"type": "input_text", "text": "hi"}},
			},
		},
	}
}

// 核心回归：json_object + role:"system" 项 X → system 必须是 "X"，不是 "X\n\nX"。
func TestSystemTextDedup_CodexJSONObjectModeNoDuplicateInjection(t *testing.T) {
	for _, systemText := range []string{
		"You are Codex.",
		"Answer strictly as JSON.",
		"多字节系统提示：必须严格输出 JSON。",
	} {
		t.Run(systemText, func(t *testing.T) {
			body := promotedJSONObjectBody(systemText)
			applyCodexSystemPromotion(t, body)

			msgs := convertPromoted(t, body)
			assertChatInvariants(t, msgs)

			require.Equal(t, []string{systemText}, systemTextsOf(msgs),
				"codex 的 promotion 是无损拷贝，提升文本与 instructions 完全相同，"+
					"合并时必须去重，否则最终 system 变成 X\\n\\nX")
			require.NotContains(t, systemTextsOf(msgs)[0], "\n\n",
				"不得出现重复注入产生的分隔符")
		})
	}
}

// 去重只认「与 base 精确相等」：真正不同的通知仍按序拼接。
func TestSystemTextDedup_DistinctNoticesStillConcatenate(t *testing.T) {
	msgs, err := responsesInputToChatMessages("You are a helpful assistant.", json.RawMessage(`[
		{"type":"message","role":"developer","content":[{"type":"input_text","text":"first"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]},
		{"type":"message","role":"developer","content":[{"type":"input_text","text":"second"}]}
	]`))
	require.NoError(t, err)
	assertChatInvariants(t, msgs)
	require.Equal(t, []string{"You are a helpful assistant.\n\nfirst\n\nsecond"}, systemTextsOf(msgs),
		"内容不同的通知必须全部保留并保持相对顺序")
}

// 无 instructions（base 为空）时不得因去重丢内容。
func TestSystemTextDedup_NoBaseKeepsEveryHoistedText(t *testing.T) {
	msgs, err := responsesInputToChatMessages("", json.RawMessage(`[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]},
		{"type":"message","role":"developer","content":[{"type":"input_text","text":"notice"}]}
	]`))
	require.NoError(t, err)
	assertChatInvariants(t, msgs)
	require.Equal(t, []string{"notice"}, systemTextsOf(msgs))
}

// 长度上限：多个 developer 通知按项数线性膨胀且原先无上限，必须被截断。
func TestSystemTextLengthCap_TruncatesAndStaysValidUTF8(t *testing.T) {
	// 每项 100 KiB，8 项 ≈ 800 KiB，远超 256 KiB 上限。
	item := strings.Repeat("a", 100*1024)
	items := make([]any, 0, 9)
	for i := 0; i < 8; i++ {
		items = append(items, map[string]any{
			"type": "message", "role": "developer",
			"content": []any{map[string]any{"type": "input_text", "text": item}},
		})
	}
	items = append(items, map[string]any{
		"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_text", "text": "q"}},
	})
	input, err := json.Marshal(items)
	require.NoError(t, err)

	msgs, err := responsesInputToChatMessages("base", input)
	require.NoError(t, err)
	assertChatInvariants(t, msgs)

	texts := systemTextsOf(msgs)
	require.Len(t, texts, 1)
	require.LessOrEqual(t, len(texts[0]), maxMergedSystemTextBytes,
		"合并后的 system 文本必须受总长度上限约束")
	require.True(t, utf8.ValidString(texts[0]),
		"截断必须落在 rune 边界上，不得产出半个多字节字符")
	require.Greater(t, len(texts[0]), 0)
}

// 截断辅助函数的边界：恰好等于上限不截断；超一字节即截断且不切坏 rune。
func TestTruncateMergedSystemTextBoundaries(t *testing.T) {
	require.Equal(t, "abc", truncateMergedSystemText("abc"))
	require.Len(t, truncateMergedSystemText(strings.Repeat("x", maxMergedSystemTextBytes)), maxMergedSystemTextBytes)

	over := truncateMergedSystemText(strings.Repeat("x", maxMergedSystemTextBytes+1))
	require.Len(t, over, maxMergedSystemTextBytes)
	require.True(t, utf8.ValidString(over))

	// 上限落在多字节字符中间时，向前回退到合法边界。
	multi := strings.Repeat("x", maxMergedSystemTextBytes-1) + "漢"
	got := truncateMergedSystemText(multi)
	require.True(t, utf8.ValidString(got), "不得切出半个 rune")
	require.LessOrEqual(t, len(got), maxMergedSystemTextBytes)
}
