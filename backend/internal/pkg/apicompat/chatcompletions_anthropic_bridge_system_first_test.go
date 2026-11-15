package apicompat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Anthropic 客户端会在 messages 数组中间插入 role=system（工具轮之间的系统通知、
// 切换模型提示等）。转换到 Chat 时必须把它们收敛到开头或降级，否则 Qwen / vLLM 系
// 严格上游会回 400 "System message must be at the beginning."。
//
// 生产背景（2026-11-15，账号 980 近 3 天 398 次该错误，全部在 /v1/messages 路径）：
// 上游回 400 而客户端已收到 200 流头，客户看到的是任务失败。
func TestAnthropicToChatMessages_HoistsMidConversationSystem(t *testing.T) {
	system := json.RawMessage(`"you are claude"`)
	msgs := []AnthropicMessage{
		{Role: "user", Content: json.RawMessage(`"hi"`)},
		{Role: "assistant", Content: json.RawMessage(`"hello"`)},
		{Role: "system", Content: json.RawMessage(`"MODEL SWITCH NOTICE"`)},
		{Role: "user", Content: json.RawMessage(`"continue"`)},
	}

	out, err := anthropicToChatMessages(system, msgs, false)
	require.NoError(t, err)
	require.NotEmpty(t, out)

	// 只能有一条 system，且必须在最前面。
	systemCount := 0
	firstSystem := -1
	for i, m := range out {
		if m.Role == "system" || m.Role == "developer" {
			systemCount++
			if firstSystem < 0 {
				firstSystem = i
			}
		}
	}
	require.Equal(t, 1, systemCount, "转换后只允许一条指令消息，否则严格上游会 400")
	require.Equal(t, 0, firstSystem, "system 必须位于消息列表开头")

	// 中途那条 system 的文本不能丢，降级为 user 后仍应出现在输出里。
	found := false
	for _, m := range out {
		if m.Role == "user" && stringsContainText(m.Content, "MODEL SWITCH NOTICE") {
			found = true
		}
	}
	require.True(t, found, "中途 system 的文本必须保留（降级为 user），不能静默丢弃")
}

func stringsContainText(content json.RawMessage, want string) bool {
	if len(content) == 0 {
		return false
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return strings.Contains(strings.ToLower(s), strings.ToLower(want))
	}
	return strings.Contains(strings.ToLower(string(content)), strings.ToLower(want))
}
