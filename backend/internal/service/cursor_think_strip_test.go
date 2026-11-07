//go:build unit

package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assertNoThinkTag 断言输出里**完整** <think>/</think> 及 <thinking>/</thinking>
// 标签出现次数恒为 0（用户裁定：标签绝对不能外泄打印到正文）。
func assertNoThinkTag(t *testing.T, s string) {
	t.Helper()
	require.Equal(t, 0, strings.Count(s, "<think>"), "完整 <think> 标签不得出现在输出: %q", s)
	require.Equal(t, 0, strings.Count(s, "</think>"), "完整 </think> 标签不得出现在输出: %q", s)
	require.Equal(t, 0, strings.Count(s, "<thinking>"), "完整 <thinking> 标签不得出现在输出: %q", s)
	require.Equal(t, 0, strings.Count(s, "</thinking>"), "完整 </thinking> 标签不得出现在输出: %q", s)
}

// TestCursorThinkStrip_FullSegment 完整段切分：<think>思考</think>正文 →
// 思考进 reasoning，正文进 text，标签零泄露；rawContent 仍保留原始（供拒付模板匹配）。
func TestCursorThinkStrip_FullSegment(t *testing.T) {
	p := cursorNewStreamParser("claude-sonnet-5-5")
	reasoning, text := p.parseThinkDeltas("<think>思考</think>正文")
	require.Equal(t, "思考", reasoning, "思考段应输出到 reasoning 通道")
	require.Equal(t, "正文", text, "剥离后残留的前导空行也必须在首段去掉")
	rTail, tTail := p.flushThinkTail()
	require.Empty(t, rTail)
	require.Empty(t, tTail)
	require.Contains(t, p.rawContent, "<think>", "原始文本必须保留，供拒付模板匹配")
	assertNoThinkTag(t, reasoning+text)
}

// TestCursorThinkStrip_CrossFrame 跨帧碎片（"<thi"+"nk>思考"+"</th"+"ink>正文"）：
// 标签 100% 吞噬，思考与正文完整提取，零泄露。
func TestCursorThinkStrip_CrossFrame(t *testing.T) {
	p := cursorNewStreamParser("claude-sonnet-5-5")
	var r, txt strings.Builder
	for _, chunk := range []string{"<thi", "nk>思考", "</th", "ink>正文"} {
		reasoning, text := p.parseThinkDeltas(chunk)
		r.WriteString(reasoning)
		txt.WriteString(text)
	}
	rTail, tTail := p.flushThinkTail()
	r.WriteString(rTail)
	txt.WriteString(tTail)
	require.Equal(t, "思考", r.String(), "跨帧切分的思考内容应完整提取")
	require.Equal(t, "正文", txt.String(), "跨帧切分的正文应完整提取")
	assertNoThinkTag(t, r.String()+txt.String())
}

// TestCursorThinkStrip_Unclosed 未闭合思考段：思考内容正常流出（随帧增量交付），
// 无标签泄露；处于思考态时收尾尾段为空（已随帧吐出，不重复）。
func TestCursorThinkStrip_Unclosed(t *testing.T) {
	p := cursorNewStreamParser("claude-sonnet-5-5")
	r0, t0 := p.parseThinkDeltas("开头<think>还在想")
	require.Equal(t, "开头", t0)
	require.Equal(t, "还在想", r0)
	r1, t1 := p.parseThinkDeltas("继续想")
	require.Equal(t, "继续想", r1)
	require.Empty(t, t1)
	rTail, tTail := p.flushThinkTail()
	require.Empty(t, rTail)
	require.Empty(t, tTail)
	assertNoThinkTag(t, r0+r1+t0+t1)
}

// TestCursorThinkStrip_TrailingPartialTag 正文结尾恰好是开标签前缀：缓冲到收尾再吐，
// 不丢字符、不重复。
func TestCursorThinkStrip_TrailingPartialTag(t *testing.T) {
	p := cursorNewStreamParser("claude-sonnet-5-5")
	r, txt := p.parseThinkDeltas("abc<th")
	require.Empty(t, r)
	require.Equal(t, "abc", txt)
	_, tTail := p.flushThinkTail()
	require.Equal(t, "<th", tTail)
	_, tTail2 := p.flushThinkTail()
	require.Empty(t, tTail2, "尾段只吐一次")
}

// TestCursorThinkStrip_NoTagPassthrough 不含标签的普通正文必须原样透传（不引入任何字符损失）。
func TestCursorThinkStrip_NoTagPassthrough(t *testing.T) {
	p := cursorNewStreamParser("claude-sonnet-5-5")
	reasoning, text := p.parseThinkDeltas("hello world")
	require.Empty(t, reasoning)
	require.Equal(t, "hello world", text)
	rTail, tTail := p.flushThinkTail()
	require.Empty(t, rTail)
	require.Empty(t, tTail)
}

// TestCursorThinkStrip_NonStreamingReasoning 非流式（批量）解析同样把思考作为
// reasoning_content 交付，正文纯净、标签零泄露。
func TestCursorThinkStrip_NonStreamingReasoning(t *testing.T) {
	// 构造一段 TextDeltaUpdate 帧 + 空 EndStream 收尾帧。
	body := cursorEncodeEnvelope(buildCursorTextDelta("开始<think>推理中</think>答案"))
	body = append(body, 0x02, 0, 0, 0, 0) // flags=EndStream, length=0

	p := cursorNewStreamParser("claude-sonnet-5-5")
	lines, err := p.cursorParseStream(body)
	require.NoError(t, err)

	var reasoning, content strings.Builder
	for _, line := range lines {
		reasoning.WriteString(extractSSEField(line, "reasoning_content"))
		content.WriteString(extractSSEField(line, "content"))
	}
	require.Equal(t, "推理中", reasoning.String(), "思考应作为 reasoning_content 交付")
	require.Equal(t, "开始答案", content.String(), "正文应剔除思考段")
	for _, line := range lines {
		assertNoThinkTag(t, line)
	}
}

// buildCursorTextDelta 构造一段 TextDeltaUpdate（仅测试用）。
func buildCursorTextDelta(text string) []byte {
	td := cursorAppendStringField(nil, cursorFldTextDeltaText, text)
	iu := cursorAppendBytesField(nil, cursorFldUpdateTextDelta, td)
	return cursorAppendBytesField(nil, cursorFldServerInteractionUpdate, iu)
}

// extractSSEField 从一条 `data: {...}` SSE 行里抽取 choices[0].delta 的指定字段值。
func extractSSEField(line, key string) string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "data:")
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	var obj struct {
		Choices []struct {
			Delta map[string]json.RawMessage `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		return ""
	}
	if len(obj.Choices) == 0 {
		return ""
	}
	raw, ok := obj.Choices[0].Delta[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// TestParseThinkDeltas_ThinkingVariant 验证 <thinking>…</thinking> 变体的剥离行为。
func TestParseThinkDeltas_ThinkingVariant(t *testing.T) {
	t.Run("单帧完整<thinking>", func(t *testing.T) {
		p := &cursorStreamParser{}
		r, txt := p.parseThinkDeltas("<thinking>推理内容</thinking>正文")
		assert.Equal(t, "推理内容", r)
		assert.Equal(t, "正文", txt)
		assertNoThinkTag(t, r+txt)
	})

	t.Run("<thinking>跨帧切割", func(t *testing.T) {
		p := &cursorStreamParser{}
		chunks := []string{"<think", "ing>推理", "内容</think", "ing>正文"}
		var rb, tb strings.Builder
		for _, chunk := range chunks {
			r, txt := p.parseThinkDeltas(chunk)
			rb.WriteString(r)
			tb.WriteString(txt)
		}
		assert.Equal(t, "推理内容", rb.String())
		assert.Equal(t, "正文", tb.String())
		assertNoThinkTag(t, rb.String()+tb.String())
	})

	t.Run("<thinking>前缀扣留不泄漏", func(t *testing.T) {
		p := &cursorStreamParser{}
		// 第一帧只发 "<thi"（是 <think> 和 <thinking> 的共同前缀）
		r0, t0 := p.parseThinkDeltas("<thi")
		assert.Empty(t, r0, "前缀段应被扣留，不输出 reasoning")
		assert.Empty(t, t0, "前缀段应被扣留，不输出 text")
		// 第二帧补全为 <thinking> 并闭合
		r1, t1 := p.parseThinkDeltas("nking>思考</thinking>答案")
		assert.Equal(t, "思考", r1)
		assert.Equal(t, "答案", t1)
		assertNoThinkTag(t, r0+r1+t0+t1)
	})

	t.Run("两段不同变体各自独立", func(t *testing.T) {
		p := &cursorStreamParser{}
		// 第一段 <think>，第二段 <thinking>
		r0, t0 := p.parseThinkDeltas("<think>思考A</think>文本A")
		r1, t1 := p.parseThinkDeltas("<thinking>思考B</thinking>文本B")
		assert.Equal(t, "思考A", r0)
		assert.Equal(t, "文本A", t0)
		assert.Equal(t, "思考B", r1)
		assert.Equal(t, "文本B", t1)
		assertNoThinkTag(t, r0+r1+t0+t1)
	})
}
