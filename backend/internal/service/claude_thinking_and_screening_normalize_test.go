//go:build unit

package service

// 2026-11-04 用户裁定的三项「别把内部契约冲突甩给用户」定向测试：
//  1. thinking 参数按模型契约自动翻译（enabled → adaptive），而不是回 400；
//  2. 上游内容审核拒绝必须识别（进换号）并给中文终态文案，不许透传英文；
//  3. 国产厂商越界 max_tokens 夹到 [1, 65536]，别让上游回 "field MaxTokens invalid"。

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNormalizeClaude55ThinkingRewritesLegacyEnabled(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5-5","thinking":{"type":"enabled","budget_tokens":4096},"max_tokens":64}`)
	out := normalizeClaude55Thinking(body, "claude-opus-5-5")

	require.Equal(t, "adaptive", gjson.GetBytes(out, "thinking.type").String())
	require.False(t, gjson.GetBytes(out, "thinking.budget_tokens").Exists(), "adaptive 不接受 budget_tokens")
	require.Equal(t, "high", gjson.GetBytes(out, "output_config.effort").String(), "缺 effort 时补默认值")
	require.Equal(t, `{"model":"claude-opus-5-5","thinking":{"type":"enabled","budget_tokens":4096},"max_tokens":64}`, string(body),
		"不得就地修改入参")
}

func TestNormalizeClaude55ThinkingKeepsSupportedShapes(t *testing.T) {
	cases := []struct {
		name  string
		model string
		body  string
	}{
		{"sonnet between_tools 是合法模式", "claude-sonnet-5-5", `{"thinking":{"type":"between_tools","display":"x"}}`},
		{"已经是 adaptive", "claude-opus-5-5", `{"thinking":{"type":"adaptive"},"output_config":{"effort":"low"}}`},
		{"不是 5.5 系列", "claude-sonnet-4-5", `{"thinking":{"type":"enabled","budget_tokens":2048}}`},
		{"没有 thinking 字段", "claude-opus-5-5", `{"model":"x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := []byte(tc.body)
			out := normalizeClaude55Thinking(in, tc.model)
			require.Equal(t, string(in), string(out), "不需要改写的请求必须逐字节不变")
		})
	}
}

func TestNormalizeClaude55ThinkingRewritesUnsupportedBetweenTools(t *testing.T) {
	body := []byte(`{"thinking":{"type":"between_tools"}}`)
	out := normalizeClaude55Thinking(body, "claude-opus-5-5")
	require.Equal(t, "adaptive", gjson.GetBytes(out, "thinking.type").String())
	require.Equal(t, "high", gjson.GetBytes(out, "output_config.effort").String())
}

func TestIsOpenAIContentScreeningRejection(t *testing.T) {
	cases := []struct {
		name   string
		status int
		msg    string
		body   string
		want   bool
	}{
		{
			name:   "商汤结构体 safety check failed",
			status: 400,
			body:   `{"error":{"code":"9","message":"safety check failed","type":"failed_precondition_error"}}`,
			want:   true,
		},
		{
			name:   "DeepSeek 英文审核话术",
			status: 400,
			body:   `{"error":{"message":"Inappropriate input/output rejected for security reasons","type":"invalid_request_error"}}`,
			want:   true,
		},
		{
			name:   "纯文本错误体",
			status: 400,
			body:   `safety check failed`,
			want:   true,
		},
		{
			name:   "普通参数错误不得误判",
			status: 400,
			msg:    "model not found",
			body:   `{"error":{"message":"model not found"}}`,
			want:   false,
		},
		{
			name:   "非 400 不算审核拒绝",
			status: 429,
			msg:    "safety check failed",
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, IsOpenAIContentScreeningRejection(tc.status, tc.msg, []byte(tc.body)))
		})
	}
}

func TestOpenAIContentScreeningClientMessageIsChinese(t *testing.T) {
	msg := OpenAIContentScreeningClientMessage()
	require.NotEmpty(t, msg)
	require.Contains(t, msg, "审核", "终态文案必须是中文，不能让用户看到英文原文")
}

func TestClampCNUpstreamMaxTokens(t *testing.T) {
	// 第三方中转（生产上 deepseek 账号的主要形态，61 个账号走 tokenharbor.ai）：
	// 这类上游上限就是 65536，越界值必须夹取。
	account := &Account{ID: 1, Platform: PlatformDeepseek, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "https://tokenharbor.ai"}}
	cases := []struct {
		name string
		body string
		want int64
		key  string
	}{
		{"超上限夹到 65536", `{"max_tokens":999999}`, 65536, "max_tokens"},
		{"负数夹到 1", `{"max_tokens":-5}`, 1, "max_tokens"},
		{"零夹到 1", `{"max_completion_tokens":0}`, 1, "max_completion_tokens"},
		{"合法值不动", `{"max_tokens":4096}`, 4096, "max_tokens"},
		{"字段缺失不动", `{"model":"m"}`, 0, "max_tokens"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := clampCNUpstreamMaxTokens(account, []byte(tc.body))
			require.True(t, json.Valid(out))
			require.Equal(t, tc.want, gjson.GetBytes(out, tc.key).Int())
		})
	}
}

func TestClampCNUpstreamMaxTokensSkipsForeignPlatform(t *testing.T) {
	body := []byte(`{"max_tokens":999999}`)
	out := clampCNUpstreamMaxTokens(&Account{ID: 1, Platform: PlatformOpenAI}, body)
	require.Equal(t, string(body), string(out), "非国产平台不得改写（各模型上限不同，不能一律夹 65536）")
}
