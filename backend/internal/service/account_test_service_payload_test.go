package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCreateTestPayloadUsesCallerPrompt 锁住「探活内容可外部指定」这一行为。
//
// 背景：anthropic 原生探活此前恒发 "hi"，导致这类账号无法做内容级对照实验。
// 2026-11-15 就用它误判过两次（984 回欢迎语被当成「模型跑偏」、137 粘欢迎语被当成
// 「上游脏数据」），实际都是这个固定 "hi" 造成的探活噪声。
func TestCreateTestPayloadUsesCallerPrompt(t *testing.T) {
	t.Run("传入 prompt 时原样下发", func(t *testing.T) {
		payload, err := createTestPayload("spark-x2.6", "把这三个字倒过来：甲乙丙")
		require.NoError(t, err)

		text := firstUserText(t, payload)
		require.Equal(t, "把这三个字倒过来：甲乙丙", text)
	})

	t.Run("空 prompt 退化为 hi，保持既有调用点行为不变", func(t *testing.T) {
		for _, empty := range []string{"", "   ", "\n\t"} {
			payload, err := createTestPayload("spark-x2.6", empty)
			require.NoError(t, err)
			require.Equal(t, "hi", firstUserText(t, payload),
				"空 prompt %q 必须退化为 hi", empty)
		}
	})

	t.Run("模型名与流式开关不受影响", func(t *testing.T) {
		payload, err := createTestPayload("Qwen3.8-27B", "x")
		require.NoError(t, err)
		require.Equal(t, "Qwen3.8-27B", payload["model"])
		require.Equal(t, true, payload["stream"])
	})
}

// firstUserText 从测试 payload 里取出第一条用户消息的文本。
func firstUserText(t *testing.T, payload map[string]any) string {
	t.Helper()

	messages, ok := payload["messages"].([]map[string]any)
	require.True(t, ok, "messages 必须是 []map[string]any")
	require.NotEmpty(t, messages, "messages 不能为空")

	content, ok := messages[0]["content"].([]map[string]any)
	require.True(t, ok, "content 必须是 []map[string]any")
	require.NotEmpty(t, content, "content 不能为空")

	text, ok := content[0]["text"].(string)
	require.True(t, ok, "text 必须是 string")
	return text
}
