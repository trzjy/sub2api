//go:build unit

package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 上游把推理内联进正文（"<think>\n…\n</think>\n\n正文"）时，对外只交付正文。
// 这些用例锁定剥离的四个边界：完整段、跨帧切分、未闭合、结尾半个标签。
func TestCursorThinkStrip_FullSegment(t *testing.T) {
	p := cursorNewStreamParser("claude-sonnet-5-5")
	got := p.filterThinkDelta("<think>\n推理内容\n</think>\n\n正文答案")
	require.Equal(t, "正文答案", got, "剥离后残留的前导空行也必须在首段去掉")
	require.False(t, p.inThink)
	require.Empty(t, p.flushThinkTail())
	require.Contains(t, p.rawContent, "<think>", "原始文本必须保留，供拒付模板匹配")
}

// 标签与正文被 SSE 帧切开时（"<thi" + "nk>"），绝不能把半个标签吐给客户端。
func TestCursorThinkStrip_CrossFrame(t *testing.T) {
	p := cursorNewStreamParser("claude-sonnet-5-5")
	var out strings.Builder
	for _, chunk := range []string{"<thi", "nk>\n思考", "中</thi", "nk>\n\nOK"} {
		out.WriteString(p.filterThinkDelta(chunk))
	}
	out.WriteString(p.flushThinkTail())
	require.Equal(t, "OK", out.String())
	require.False(t, p.inThink)
}

// 未闭合的推理段不是正文：其后内容一律不输出，收尾也不吐出。
func TestCursorThinkStrip_Unclosed(t *testing.T) {
	p := cursorNewStreamParser("claude-sonnet-5-5")
	require.Equal(t, "开头", p.filterThinkDelta("开头<think>还在想"))
	require.Empty(t, p.filterThinkDelta("继续想"))
	require.Empty(t, p.flushThinkTail())
	require.True(t, p.inThink)
}

// 正文结尾恰好是标签前缀时，缓冲到收尾再吐，不丢字符、不多吐。
func TestCursorThinkStrip_TrailingPartialTag(t *testing.T) {
	p := cursorNewStreamParser("claude-sonnet-5-5")
	require.Equal(t, "abc", p.filterThinkDelta("abc<th"))
	require.Equal(t, "<th", p.flushThinkTail())
	require.Empty(t, p.flushThinkTail(), "尾段只吐一次")
}

// 不含标签的普通正文必须原样透传（不引入任何字符损失）。
func TestCursorThinkStrip_NoTagPassthrough(t *testing.T) {
	p := cursorNewStreamParser("claude-sonnet-5-5")
	require.Equal(t, "hello world", p.filterThinkDelta("hello world"))
	require.Empty(t, p.flushThinkTail())
}
