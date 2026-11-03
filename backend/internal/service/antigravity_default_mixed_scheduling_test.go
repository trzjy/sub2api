//go:build unit

package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestApplyAntigravityDefaultMixedScheduling(t *testing.T) {
	t.Run("antigravity 缺省补 true", func(t *testing.T) {
		got := applyAntigravityDefaultMixedScheduling(PlatformAntigravity, map[string]any{"privacy_mode": "privacy_set"})
		require.Equal(t, true, got[AntigravityMixedSchedulingExtraKey])
		require.Equal(t, "privacy_set", got["privacy_mode"])
	})

	t.Run("nil extra 也补", func(t *testing.T) {
		got := applyAntigravityDefaultMixedScheduling(PlatformAntigravity, nil)
		require.Equal(t, true, got[AntigravityMixedSchedulingExtraKey])
	})

	t.Run("显式 false 不覆盖", func(t *testing.T) {
		got := applyAntigravityDefaultMixedScheduling(PlatformAntigravity, map[string]any{AntigravityMixedSchedulingExtraKey: false})
		require.Equal(t, false, got[AntigravityMixedSchedulingExtraKey])
	})

	t.Run("显式 true 保持", func(t *testing.T) {
		got := applyAntigravityDefaultMixedScheduling(PlatformAntigravity, map[string]any{AntigravityMixedSchedulingExtraKey: true})
		require.Equal(t, true, got[AntigravityMixedSchedulingExtraKey])
	})

	t.Run("非 antigravity 原样返回", func(t *testing.T) {
		in := map[string]any{"privacy_mode": "privacy_set"}
		got := applyAntigravityDefaultMixedScheduling(PlatformGemini, in)
		require.NotContains(t, got, AntigravityMixedSchedulingExtraKey)
	})

	t.Run("不修改调用方传入的 map", func(t *testing.T) {
		in := map[string]any{"privacy_mode": "privacy_set"}
		_ = applyAntigravityDefaultMixedScheduling(PlatformAntigravity, in)
		require.NotContains(t, in, AntigravityMixedSchedulingExtraKey)
	})
}

// TestConvertClaudeMessagesToGeminiGenerateContentForAntigravity 固化 Antigravity
// Gemini 转发链路的入参形状：Anthropic 形状进来，必须是 Gemini generateContent
// 形状出去（否则上游报 Unknown name "max_tokens" at 'request'）。
func TestConvertClaudeMessagesToGeminiGenerateContentForAntigravity(t *testing.T) {
	body := []byte(`{
		"model": "gemini-4-argon",
		"max_tokens": 32,
		"messages": [{"role": "user", "content": "hi"}]
	}`)

	converted, err := ConvertClaudeMessagesToGeminiGenerateContent(body)
	require.NoError(t, err)

	var out map[string]any
	require.NoError(t, json.Unmarshal(converted, &out))

	require.Contains(t, out, "contents", "转换结果必须是 Gemini generateContent 形状")
	require.NotContains(t, out, "messages", "Anthropic 的 messages 不得残留")
	require.NotContains(t, out, "max_tokens", "Anthropic 的 max_tokens 不得残留")

	gen, ok := out["generationConfig"].(map[string]any)
	require.True(t, ok, "generationConfig 必须存在")
	require.EqualValues(t, 32, gen["maxOutputTokens"])
}
