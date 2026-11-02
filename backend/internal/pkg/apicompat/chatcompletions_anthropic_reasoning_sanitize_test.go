package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// 网关注入的缺失标记与上游内联的推理标签都不得作为可见正文交付。
// 活体（11-03 04:32-04:37 Claude Code 会话）：客户端界面出现
// "<thinking></thinking>" 与 "<thinking>(reasoning not retained)</thinking>"。
func TestSanitizeReasoningForVisibleText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"占位符单独出现", "(reasoning not retained)", ""},
		{"空标签对", "<thinking></thinking>", ""},
		{"think 小写标签", "<think>内部推理</think>\n正文", "正文"},
		{"thinking 标签 + 占位", "<thinking>(reasoning not retained)</thinking>\n巡检完成：无异常", "巡检完成：无异常"},
		{"未闭合标签兜底", "正文<thinking>残留推理", "正文残留推理"},
		{"普通正文不动", "东京现在 14:30", "东京现在 14:30"},
		{"标签间夹正文", "<thinking>想一下</thinking>答案是 3293", "答案是 3293"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, sanitizeReasoningForVisibleText(c.in))
		})
	}
}

// reasoning-only 轮次的兜底文本必须清洗后再交付：既保住"不空轮"，也不泄漏内部标记。
func TestChatMessageToAnthropicBlocks_ReasoningFallbackSanitized(t *testing.T) {
	msg := ChatMessage{
		Role:             "assistant",
		ReasoningContent: "<thinking>(reasoning not retained)</thinking>\n巡检完成：AGG-UPSTREAM 正常",
	}
	blocks := chatMessageToAnthropicBlocks(msg)
	require.Len(t, blocks, 2, "thinking 块 + text 块")
	require.Equal(t, "thinking", blocks[0].Type)
	require.Equal(t, "text", blocks[1].Type)
	require.Equal(t, "巡检完成：AGG-UPSTREAM 正常", blocks[1].Text)
	require.NotContains(t, blocks[1].Text, "reasoning not retained")
	require.NotContains(t, blocks[1].Text, "<thinking>")
}

// 只有占位、没有真正正文时，不得把占位当正文输出（空轮好过泄漏）。
func TestChatMessageToAnthropicBlocks_PlaceholderOnlyYieldsEmptyText(t *testing.T) {
	msg := ChatMessage{Role: "assistant", ReasoningContent: "(reasoning not retained)"}
	blocks := chatMessageToAnthropicBlocks(msg)
	require.Len(t, blocks, 2)
	require.Equal(t, "", blocks[1].Text)
	raw, err := json.Marshal(blocks[1])
	require.NoError(t, err)
	require.NotContains(t, string(raw), "reasoning not retained")
}
