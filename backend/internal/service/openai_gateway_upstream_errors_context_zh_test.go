//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// 2026-11-06 用户裁定：上游「请求太大 / 上下文超限」的错误必须翻成中文再给下游。
// 这组用例锁两件事：① 中文文案真的会被选中；② 换号语义不被这次改动碰到。
func TestIsContextTooLargeForClientMessage(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		body string
		want bool
	}{
		{
			name: "Anthropic 原生 prompt is too long（生产 75 次主来源，msg 字段）",
			body: `{"code":11115,"msg":"prompt is too long: 100001 tokens \u003e 100000 maximum","requestId":"x"}`,
			want: true,
		},
		{
			name: "纯文本 prompt is too long（无 context 字样）",
			msg:  "prompt is too long: 1000001 tokens > 1000000 maximum",
			want: true,
		},
		{
			name: "context window 超限（原有口径，仍要认）",
			msg:  "Your input exceeds the context window of this model. Please adjust your input and try again.",
			want: true,
		},
		{
			name: "context_length_exceeded code（原有口径）",
			body: `{"error":{"code":"context_length_exceeded","message":"too big"}}`,
			want: true,
		},
		{
			name: "detail 风格 request is too large",
			body: `{"detail":"Request is too large for this model"}`,
			want: true,
		},
		{
			name: "普通 400 不得误判",
			msg:  "invalid model parameter",
			body: `{"error":{"message":"invalid model parameter"}}`,
			want: false,
		},
		{
			name: "限流文案不得误判",
			msg:  "Upstream rate limit exceeded, please retry later",
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isContextTooLargeForClientMessage(tc.msg, []byte(tc.body)))
		})
	}
}

func TestOpenAIUpstreamClientMessageChineseOnContextWindow(t *testing.T) {
	// 非掩码账号：普通错误保留上游原文（既有行为）。
	require.Equal(t,
		"invalid model parameter",
		openAIUpstreamClientMessage(nil, "invalid model parameter", []byte(`{"error":{"message":"invalid model parameter"}}`)),
	)

	// 超限：必须换成固定中文文案。
	got := openAIUpstreamClientMessage(nil, "", []byte(`{"code":11115,"msg":"prompt is too long: 100001 tokens > 100000 maximum"}`))
	require.Equal(t, infraerrors.UpstreamContextWindowExceeded, got)
	require.Contains(t, got, "请求内容太长")
	// 防泄露：不得回显厂商、内部模型映射或 token 数字。
	require.NotContains(t, got, "prompt is too long")
	require.NotContains(t, got, "100001")
}

// 换号语义锁：这次只改文案，isOpenAIContextWindowError 的匹配面必须保持不变，
// 否则生产上「换号后恢复成 200」的那批超限请求会退化成直接失败。
func TestContextWindowErrorMatchingUnchangedForFailover(t *testing.T) {
	// Anthropic 口径不进入换号判定（它换号能恢复，靠的是原有路径）。
	require.False(t, isOpenAIContextWindowError(
		"prompt is too long: 100001 tokens > 100000 maximum", nil,
	), "prompt is too long 不得进入 isOpenAIContextWindowError，否则会改变换号行为")

	// 原有口径保持不变。
	require.True(t, isOpenAIContextWindowError(
		"Your input exceeds the context window of this model", nil,
	))
}
