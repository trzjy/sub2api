//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// --- 改动 1.2：IsResponseIdentityMaskedAccount 谓词 ---

func TestIsResponseIdentityMaskedAccount(t *testing.T) {
	t.Run("旧开关 mask_upstream_identity=true 仍生效", func(t *testing.T) {
		a := &Account{Extra: map[string]any{"mask_upstream_identity": true}}
		require.True(t, IsResponseIdentityMaskedAccount(a), "旧开关应为 true（既有账号行为不变）")
	})

	t.Run("新开关 response_identity_mask=true 生效", func(t *testing.T) {
		a := &Account{Extra: map[string]any{"response_identity_mask": true}}
		require.True(t, IsResponseIdentityMaskedAccount(a), "新开关应为 true")
	})

	t.Run("两者都缺 → false", func(t *testing.T) {
		a := &Account{Extra: map[string]any{"other": 1}}
		require.False(t, IsResponseIdentityMaskedAccount(a))
		require.False(t, IsResponseIdentityMaskedAccount(&Account{Extra: map[string]any{}}))
		require.False(t, IsResponseIdentityMaskedAccount(&Account{}))
	})

	t.Run("新开关字符串 true/1 → true", func(t *testing.T) {
		require.True(t, IsResponseIdentityMaskedAccount(&Account{Extra: map[string]any{"response_identity_mask": "true"}}))
		require.True(t, IsResponseIdentityMaskedAccount(&Account{Extra: map[string]any{"response_identity_mask": "1"}}))
	})

	t.Run("新开关非法值 → false", func(t *testing.T) {
		require.False(t, IsResponseIdentityMaskedAccount(&Account{Extra: map[string]any{"response_identity_mask": "false"}}))
		require.False(t, IsResponseIdentityMaskedAccount(&Account{Extra: map[string]any{"response_identity_mask": "0"}}))
		require.False(t, IsResponseIdentityMaskedAccount(&Account{Extra: map[string]any{"response_identity_mask": "yes"}}))
		// 数值 1 是非 bool 非上述字符串，按零影响处理 → false
		require.False(t, IsResponseIdentityMaskedAccount(&Account{Extra: map[string]any{"response_identity_mask": 1}}))
	})

	t.Run("nil 账号 → false（nil 安全）", func(t *testing.T) {
		require.False(t, IsResponseIdentityMaskedAccount(nil))
	})

	t.Run("nil Extra → false（nil 安全）", func(t *testing.T) {
		require.False(t, IsResponseIdentityMaskedAccount(&Account{Extra: nil}))
	})
}

// --- 改动 2：StripReasoningFromSSELine（覆盖 reasoning 字段名 + 全部 choices） ---

func TestStripReasoningFromSSELine_ReasoningField(t *testing.T) {
	t.Run("单 choice 带 reasoning → 删 reasoning 保留 content 等其它字段", func(t *testing.T) {
		line := `data: {"model":"sensenova","choices":[{"index":0,"delta":{"role":"assistant","content":"hi","reasoning":"think"}}]}`
		out := StripReasoningFromSSELine(line)
		require.NotContains(t, out, "think", "delta.reasoning 必须被删除")
		require.False(t, gjson.Get(out, "choices.0.delta.reasoning").Exists(), "reasoning 字段必须不存在")
		require.Equal(t, "hi", gjson.Get(out, "choices.0.delta.content").String(), "content 必须保留")
		require.Equal(t, "assistant", gjson.Get(out, "choices.0.delta.role").String(), "role 必须保留")
		require.Equal(t, "sensenova", gjson.Get(out, "model").String(), "model 必须保留")
	})

	t.Run("多 choices 且 index 与数组下标不同 → 两个 reasoning 都删", func(t *testing.T) {
		// 数组下标 0 的元素 index=1，下标 1 的元素 index=0（与下标不一致，验证用 index 字段定位）
		line := `data: {"choices":[{"index":1,"delta":{"reasoning":"r1"}},{"index":0,"delta":{"reasoning":"r0"}}]}`
		out := StripReasoningFromSSELine(line)
		require.NotContains(t, out, "r1", "choice index=1 的 reasoning 必须被删除")
		require.NotContains(t, out, "r0", "choice index=0 的 reasoning 必须被删除")
		require.False(t, gjson.Get(out, "choices.1.delta.reasoning").Exists())
		require.False(t, gjson.Get(out, "choices.0.delta.reasoning").Exists())
		require.Len(t, gjson.Get(out, "choices").Array(), 2, "choices 数量保持不变，绝不丢行")
	})

	t.Run("多 choices 且都缺 index → 按各自数组下标定位并删除", func(t *testing.T) {
		// 覆盖「只处理 choices.0」的旧缺陷：两个 choice 都没有 index 字段，
		// 必须分别按数组下标 0 和 1 定位并删除。
		line := `data: {"choices":[{"delta":{"reasoning":"r0"}},{"delta":{"reasoning":"r1"}}]}`
		out := StripReasoningFromSSELine(line)
		require.NotContains(t, out, "r0", "下标 0 的 reasoning 必须被删除")
		require.NotContains(t, out, "r1", "下标 1 的 reasoning 必须被删除")
		require.False(t, gjson.Get(out, "choices.0.delta.reasoning").Exists())
		require.False(t, gjson.Get(out, "choices.1.delta.reasoning").Exists())
		require.Len(t, gjson.Get(out, "choices").Array(), 2, "choices 数量保持不变，绝不丢行")
	})
	t.Run("choice 缺 index 字段 → 按数组下标定位并删除", func(t *testing.T) {
		line := `data: {"choices":[{"delta":{"reasoning":"x"}}]}`
		out := StripReasoningFromSSELine(line)
		require.NotContains(t, out, "x", "缺 index 时按数组下标 0 删除")
		require.False(t, gjson.Get(out, "choices.0.delta.reasoning").Exists())
		require.Len(t, gjson.Get(out, "choices").Array(), 1)
	})

	t.Run("无 reasoning 字段 → 输出与输入逐字节相同", func(t *testing.T) {
		line := `data: {"model":"gpt","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`
		require.Equal(t, line, StripReasoningFromSSELine(line), "无目标字段必须零影响、逐字节相同")
		// 子串命中但无真实目标字段（模型名含 reasoning 字样）同样逐字节不变
		line2 := `data: {"model":"no-reasoning-model","choices":[{"index":0,"delta":{"content":"hi"}}]}`
		require.Equal(t, line2, StripReasoningFromSSELine(line2))
	})

	t.Run("幂等/零影响：旧 reasoning_content 行为不回归", func(t *testing.T) {
		line := `data: {"choices":[{"index":0,"delta":{"content":"hi","reasoning_content":"old"}}]}`
		out := StripReasoningFromSSELine(line)
		require.NotContains(t, out, "old", "reasoning_content 必须仍被删除")
		require.False(t, gjson.Get(out, "choices.0.delta.reasoning_content").Exists())
		require.Equal(t, "hi", gjson.Get(out, "choices.0.delta.content").String())
	})
}

// --- 改动 2：StripReasoningFromResponseBody（message 路径版本） ---

func TestStripReasoningFromResponseBody_ReasoningField(t *testing.T) {
	t.Run("单 choice 带 reasoning → 删 message.reasoning 保留 content", func(t *testing.T) {
		body := []byte(`{"model":"sensenova","choices":[{"index":0,"message":{"role":"assistant","content":"hi","reasoning":"think"}}],"usage":{"total_tokens":5}}`)
		out := StripReasoningFromResponseBody(body)
		require.NotContains(t, string(out), "think", "message.reasoning 必须被删除")
		require.False(t, gjson.GetBytes(out, "choices.0.message.reasoning").Exists())
		require.Equal(t, "hi", gjson.GetBytes(out, "choices.0.message.content").String())
		require.Equal(t, "sensenova", gjson.GetBytes(out, "model").String())
		require.Equal(t, int64(5), gjson.GetBytes(out, "usage.total_tokens").Int())
	})

	t.Run("多 choices 且 index 与数组下标不同 → 两个 reasoning 都删", func(t *testing.T) {
		body := []byte(`{"choices":[{"index":1,"message":{"reasoning":"r1"}},{"index":0,"message":{"reasoning":"r0"}}]}`)
		out := StripReasoningFromResponseBody(body)
		require.NotContains(t, string(out), "r1")
		require.NotContains(t, string(out), "r0")
		require.False(t, gjson.GetBytes(out, "choices.1.message.reasoning").Exists())
		require.False(t, gjson.GetBytes(out, "choices.0.message.reasoning").Exists())
		require.Len(t, gjson.GetBytes(out, "choices").Array(), 2)
	})

	t.Run("choice 缺 index 字段 → 按数组下标定位并删除", func(t *testing.T) {
		body := []byte(`{"choices":[{"message":{"reasoning":"x"}}]}`)
		out := StripReasoningFromResponseBody(body)
		require.NotContains(t, string(out), "x")
		require.False(t, gjson.GetBytes(out, "choices.0.message.reasoning").Exists())
		require.Len(t, gjson.GetBytes(out, "choices").Array(), 1)
	})

	t.Run("无 reasoning 字段 → 输出与输入逐字节相同", func(t *testing.T) {
		body := []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}]}`)
		require.Equal(t, string(body), string(StripReasoningFromResponseBody(body)))
		body2 := []byte(`{"model":"no-reasoning-model","choices":[{"index":0,"message":{"content":"hi"}}]}`)
		require.Equal(t, string(body2), string(StripReasoningFromResponseBody(body2)))
	})

	t.Run("幂等/零影响：旧 reasoning_content 行为不回归", func(t *testing.T) {
		body := []byte(`{"choices":[{"index":0,"message":{"content":"hi","reasoning_content":"old"}}]}`)
		out := StripReasoningFromResponseBody(body)
		require.NotContains(t, string(out), "old")
		require.False(t, gjson.GetBytes(out, "choices.0.message.reasoning_content").Exists())
		require.Equal(t, "hi", gjson.GetBytes(out, "choices.0.message.content").String())
	})
}
