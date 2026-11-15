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

// TestCreateTestPayloadSplitsMultiMessage 锁住「长上下文按多轮消息下发」这一形态。
//
// 为什么需要：真实客户的长上下文是多轮消息累积的；把它压成一条超长 user 消息
// 会与生产形态不符（2026-11-15 实测：984 单条 1 万字返回空正文，但生产 20 万 token
// 请求输出正常，说明差异来自消息形态而不是上下文容量）。
func TestCreateTestPayloadSplitsMultiMessage(t *testing.T) {
	sep := testPayloadMessageSeparator

	t.Run("不含分隔符仍是单条 user", func(t *testing.T) {
		payload, err := createTestPayload("m", "一段普通问题")
		require.NoError(t, err)
		msgs := messagesOf(t, payload)
		require.Len(t, msgs, 1)
		require.Equal(t, "user", msgs[0]["role"])
	})

	t.Run("含分隔符拆成多轮且以 user 结尾", func(t *testing.T) {
		prompt := "第一段" + sep + "第二段" + sep + "最后的提问"
		payload, err := createTestPayload("m", prompt)
		require.NoError(t, err)

		msgs := messagesOf(t, payload)
		require.Len(t, msgs, 5, "3 段应产生 user/assistant 交替共 5 条")
		require.Equal(t, "user", msgs[0]["role"])
		require.Equal(t, "assistant", msgs[1]["role"])
		require.Equal(t, "user", msgs[len(msgs)-1]["role"], "最后一条必须是 user")

		require.Equal(t, "第一段", textOf(msgs[0]))
		require.Equal(t, "最后的提问", textOf(msgs[len(msgs)-1]))
	})

	t.Run("连续分隔符产生的空段被丢弃", func(t *testing.T) {
		prompt := "A" + sep + sep + "B"
		payload, err := createTestPayload("m", prompt)
		require.NoError(t, err)
		require.Len(t, messagesOf(t, payload), 3, "空段应被过滤，A 与 B 两段")
	})

	t.Run("只有 cache 断点挂在最后一条 user 上", func(t *testing.T) {
		prompt := "第一段" + sep + "第二段"
		payload, err := createTestPayload("m", prompt)
		require.NoError(t, err)

		msgs := messagesOf(t, payload)
		for i, m := range msgs {
			content := m["content"].([]map[string]any)
			_, hasCache := content[0]["cache_control"]
			if i == len(msgs)-1 {
				require.True(t, hasCache, "最后一条 user 必须有 cache 断点")
			} else {
				require.False(t, hasCache, "第 %d 条不应有 cache 断点", i)
			}
		}
	})
}

func messagesOf(t *testing.T, payload map[string]any) []map[string]any {
	t.Helper()
	msgs, ok := payload["messages"].([]map[string]any)
	require.True(t, ok, "messages 必须是 []map[string]any")
	return msgs
}

func textOf(msg map[string]any) string {
	content := msg["content"].([]map[string]any)
	s, _ := content[0]["text"].(string)
	return s
}
