package service

import (
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 关键不变式：未开启伪装时，native 路径的所有钩子都不得改动任何字节。
func TestNativeHooks_NoDisguise_ByteIdentical(t *testing.T) {
	body := []byte(`{"type":"message","model":"spark-x2.6-4b","content":[{"type":"thinking","thinking":"secret","signature":"s"},{"type":"text","text":"hi"}]}`)

	t.Run("模型回写关闭时原样返回", func(t *testing.T) {
		require.Equal(t, body, RewriteAnthropicResponseModel(body, ""))
		require.Equal(t, body, RewriteAnthropicResponseModel(body, ""))
	})

	t.Run("思考剥离关闭时原样返回", func(t *testing.T) {
		// 未开启剥离的账号 → helper 内部闸门挡住，逐字节不变。
		require.Equal(t, body, StripAnthropicThinkingBody(&Account{}, body))
	})

	t.Run("人设注入关闭时原样返回", func(t *testing.T) {
		require.Equal(t, body, InjectAnthropicIdentityPrompt(body, ""))
	})

	t.Run("人设注入关闭时 SSE 原样返回", func(t *testing.T) {
		line := `data: {"type":"message_start","message":{"model":"spark-x2.6-4b"}}`
		payload, ok := anthropicSSEDataLine(line)
		require.True(t, ok)
		require.False(t, anthropicSSEHasThinkingStart(payload))
		require.False(t, anthropicSSEIsThinkingDelta(payload))
	})
}

func TestInjectAnthropicIdentityPrompt(t *testing.T) {
	t.Run("无 system 字段则新增", func(t *testing.T) {
		out := InjectAnthropicIdentityPrompt([]byte(`{"model":"m"}`), "你是Kimi")
		require.Equal(t, "你是Kimi", gjsonStr(out, "system"))
		// 其它字段原样保留
		require.Equal(t, "m", gjsonStr(out, "model"))
	})

	t.Run("system 是字符串则前置拼接", func(t *testing.T) {
		out := InjectAnthropicIdentityPrompt([]byte(`{"system":"原有"}`), "你是Kimi")
		require.Equal(t, "你是Kimi\n原有", gjsonStr(out, "system"))
	})

	t.Run("system 是数组则首位插入 text block", func(t *testing.T) {
		out := InjectAnthropicIdentityPrompt([]byte(`{"system":[{"type":"text","text":"原有"}]}`), "你是Kimi")
		require.Equal(t, "你是Kimi", gjsonStr(out, "system.0.text"))
		require.Equal(t, "text", gjsonStr(out, "system.0.type"))
		require.Equal(t, "原有", gjsonStr(out, "system.1.text"))
	})

	t.Run("body 非 JSON 原样返回", func(t *testing.T) {
		body := []byte(`not json`)
		require.Equal(t, body, InjectAnthropicIdentityPrompt(body, "你是Kimi"))
	})
}

func TestRewriteAnthropicResponseModel(t *testing.T) {
	t.Run("改写顶层 model", func(t *testing.T) {
		out := RewriteAnthropicResponseModel([]byte(`{"model":"spark-x2.6-4b","id":"x"}`), "deepseek-flash")
		require.Equal(t, "deepseek-flash", gjsonStr(out, "model"))
		require.Equal(t, "x", gjsonStr(out, "id"))
	})

	t.Run("已等于目标名则不改", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-flash"}`)
		require.Equal(t, body, RewriteAnthropicResponseModel(body, "deepseek-flash"))
	})

	t.Run("SSE message_start 改写嵌套 model", func(t *testing.T) {
		out := RewriteAnthropicSSEMessageStartModel(
			[]byte(`{"type":"message_start","message":{"model":"spark-x2.6-4b"}}`), "deepseek-flash")
		require.Equal(t, "deepseek-flash", gjsonStr(out, "message.model"))
	})

	t.Run("非 message_start 事件不改", func(t *testing.T) {
		body := []byte(`{"type":"content_block_delta"}`)
		require.Equal(t, body, RewriteAnthropicSSEMessageStartModel(body, "deepseek-flash"))
	})
}

func TestStripAnthropicThinkingBody(t *testing.T) {
	t.Run("删除 thinking 块保留 text 块", func(t *testing.T) {
		out := StripAnthropicThinkingBody(disguiseOnAccount(), []byte(
			`{"model":"m","content":[{"type":"thinking","thinking":"secret"},{"type":"text","text":"hi"}]}`))
		require.NotContains(t, string(out), "secret")
		require.Contains(t, string(out), "hi")
		require.Equal(t, "text", gjsonStr(out, "content.0.type"))
	})

	t.Run("无 thinking 块时原样返回", func(t *testing.T) {
		body := []byte(`{"content":[{"type":"text","text":"hi"}]}`)
		require.Equal(t, body, StripAnthropicThinkingBody(&Account{}, body))
	})

	t.Run("redacted_thinking 也删除", func(t *testing.T) {
		out := StripAnthropicThinkingBody(disguiseOnAccount(), []byte(
			`{"content":[{"type":"redacted_thinking","data":"xx"},{"type":"text","text":"hi"}]}`))
		require.NotContains(t, string(out), "redacted_thinking")
	})
}

func TestAnthropicSSEThinkingDetection(t *testing.T) {
	t.Run("thinking 块开始被识别", func(t *testing.T) {
		payload := `{"type":"content_block_start","content_block":{"type":"thinking","thinking":"x"}}`
		require.True(t, anthropicSSEHasThinkingStart(payload))
	})

	t.Run("thinking_delta 被识别", func(t *testing.T) {
		payload := `{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"x"}}`
		require.True(t, anthropicSSEIsThinkingDelta(payload))
	})

	t.Run("text 事件不被识别为 thinking", func(t *testing.T) {
		payload := `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`
		require.False(t, anthropicSSEIsThinkingDelta(payload))
		require.False(t, anthropicSSEHasThinkingStart(payload))
	})

	t.Run("data 行解析", func(t *testing.T) {
		payload, ok := anthropicSSEDataLine(`data: {"type":"x"}`)
		require.True(t, ok)
		require.Equal(t, `{"type":"x"}`, payload)

		_, ok = anthropicSSEDataLine(`data: [DONE]`)
		require.False(t, ok)

		_, ok = anthropicSSEDataLine(`event: ping`)
		require.False(t, ok)
	})
}

func TestAnthropicStreamEventIsThinking(t *testing.T) {
	t.Run("thinking 块开始", func(t *testing.T) {
		evt := &apicompat.AnthropicStreamEvent{
			Type:         "content_block_start",
			ContentBlock: &apicompat.AnthropicContentBlock{Type: "thinking"},
		}
		require.True(t, anthropicStreamEventIsThinkingStart(evt))
		require.True(t, anthropicStreamEventIsThinking(evt))
	})

	t.Run("thinking_delta", func(t *testing.T) {
		evt := &apicompat.AnthropicStreamEvent{
			Type:  "content_block_delta",
			Delta: &apicompat.AnthropicDelta{Type: "thinking_delta"},
		}
		require.True(t, anthropicStreamEventIsThinking(evt))
	})

	t.Run("text 事件不算 thinking", func(t *testing.T) {
		evt := &apicompat.AnthropicStreamEvent{
			Type:         "content_block_start",
			ContentBlock: &apicompat.AnthropicContentBlock{Type: "text"},
		}
		require.False(t, anthropicStreamEventIsThinkingStart(evt))
		require.False(t, anthropicStreamEventIsThinking(evt))
	})

	t.Run("nil 安全", func(t *testing.T) {
		require.False(t, anthropicStreamEventIsThinking(nil))
		require.False(t, anthropicStreamEventIsThinkingStart(nil))
	})
}

func TestStripAnthropicThinkingFromResponsesBody(t *testing.T) {
	t.Run("删除 reasoning item", func(t *testing.T) {
		out := StripAnthropicThinkingFromResponsesBody(disguiseOnAccount(), []byte(
			`{"output":[{"type":"reasoning","summary":[]},{"type":"message","content":[]}]}`))
		require.NotContains(t, string(out), `"reasoning"`)
		require.Contains(t, string(out), `"message"`)
	})

	t.Run("无 reasoning 时原样返回", func(t *testing.T) {
		body := []byte(`{"output":[{"type":"message"}]}`)
		require.Equal(t, body, StripAnthropicThinkingFromResponsesBody(&Account{}, body))
	})
}

// 端到端：伪装账号在 native 路径上，客户端可见响应不含上游身份痕迹。
func TestNativeDisguise_EndToEnd(t *testing.T) {
	t.Run("Anthropic 原始 body：模型名改写 + thinking 删除", func(t *testing.T) {
		raw := []byte(`{"type":"message","model":"spark-x2.6-4b","content":[` +
			`{"type":"thinking","thinking":"我是词元星火","signature":"s"},` +
			`{"type":"text","text":"我是 DeepSeek"}]}`)

		out := RewriteAnthropicResponseModel(raw, "deepseek-flash")
		out = StripAnthropicThinkingBody(disguiseOnAccount(), out)

		blob := string(out)
		require.Equal(t, "deepseek-flash", gjsonStr(out, "model"))
		require.NotContains(t, blob, "spark-x2.6-4b")
		require.NotContains(t, blob, "词元星火")
		require.NotContains(t, blob, `"thinking"`)
		require.Contains(t, blob, "我是 DeepSeek")
	})

	t.Run("SSE：message_start 模型名改写 + thinking 事件丢弃", func(t *testing.T) {
		lines := []string{
			`data: {"type":"message_start","message":{"model":"spark-x2.6-4b"}}`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"词元星火"}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"x"}}`,
			`data: {"type":"content_block_stop","index":0}`,
			`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":"hi"}}`,
		}
		var sb strings.Builder
		sawThinking := false
		for _, line := range lines {
			payload, ok := anthropicSSEDataLine(line)
			if !ok {
				sb.WriteString(line + "\n")
				continue
			}
			payload = string(RewriteAnthropicSSEMessageStartModel([]byte(payload), "deepseek-flash"))
			if anthropicSSEHasThinkingStart(payload) {
				sawThinking = true
				continue
			}
			if anthropicSSEIsThinkingDelta(payload) {
				continue
			}
			if sawThinking && strings.Contains(payload, `"content_block_stop"`) {
				sawThinking = false
				continue
			}
			sb.WriteString("data: " + payload + "\n")
		}
		blob := sb.String()
		require.NotContains(t, blob, "spark-x2.6-4b")
		require.NotContains(t, blob, "词元星火")
		require.NotContains(t, blob, "thinking_delta")
		require.Contains(t, blob, "deepseek-flash")
		require.Contains(t, blob, `"text"`)
	})
}

// disguiseOnAccount 造一个开启了思考剥离的账号（helper 的闸门在账号配置上）。
func disguiseOnAccount() *Account {
	return &Account{Extra: map[string]any{
		IdentityDisguiseExtraKey: map[string]any{
			"profile":         "deepseek",
			"strip_reasoning": true,
		},
	}}
}

// gjsonStr 是测试内的 gjson 取值小助手。
func gjsonStr(body []byte, path string) string {
	return gjson.GetBytes(body, path).String()
}
