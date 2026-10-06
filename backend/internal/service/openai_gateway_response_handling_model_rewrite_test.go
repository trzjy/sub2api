package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// qk-S1: 掩码账号身份回写别名匹配。
//
// 上游以 kimi-k3 名义供给 TH 免费模型，回显 "qwen3.8-flash"（丢失发送名
// "qwen3.8-flash:free" 的 :free 后缀）；既有回写精确匹配落空 → 泄漏。
// 本组用例覆盖别名集回写修复，并以"非掩码账号保持原样"作为对照。

func TestReplaceModelInSSELineAliased(t *testing.T) {
	svc := &OpenAIGatewayService{}
	const (
		mappedModel = "qwen3.8-flash:free" // 上游发送名（含 :free）
		clientModel = "kimi-k3"            // 客户端请求名
	)
	aliases := IdentityRewriteAliases(mappedModel) // ["qwen3.8-flash:free", "qwen3.8-flash"]

	tests := []struct {
		name     string
		line     string
		from     string
		to       string
		useAlias bool
		expected string
	}{
		{
			// 别名集精确匹配回写：回显等于发送名 → 改写。
			name:     "别名集精确匹配回写",
			line:     `data: {"model":"qwen3.8-flash:free","choices":[]}`,
			from:     mappedModel,
			to:       clientModel,
			useAlias: true,
			expected: `data: {"model":"kimi-k3","choices":[]}`,
		},
		{
			// 去后缀别名匹配回写（掩码账号）：上游回显丢失 :free 后缀仍改写。
			name:     "去后缀别名匹配回写",
			line:     `data: {"model":"qwen3.8-flash","choices":[]}`,
			from:     mappedModel,
			to:       clientModel,
			useAlias: true,
			expected: `data: {"model":"kimi-k3","choices":[]}`,
		},
		{
			// 对照（第6轮 #2）：非掩码账号回显去后缀名保持原样（不传别名）。
			name:     "非掩码账号去后缀名保持原样",
			line:     `data: {"model":"qwen3.8-flash","choices":[]}`,
			from:     mappedModel,
			to:       clientModel,
			useAlias: false,
			expected: `data: {"model":"qwen3.8-flash","choices":[]}`,
		},
		{
			// 嵌套 response.model 去后缀别名匹配回写（掩码账号）。
			name:     "嵌套 response.model 去后缀别名匹配",
			line:     `data: {"response":{"model":"qwen3.8-flash"},"output":[]}`,
			from:     mappedModel,
			to:       clientModel,
			useAlias: true,
			expected: `data: {"response":{"model":"kimi-k3"},"output":[]}`,
		},
		{
			// 内容文本含模型名不改写：model 字段不匹配，content 中出现模型名不动。
			name:     "内容文本含模型名不改写",
			line:     `data: {"model":"other-model","choices":[{"delta":{"content":"routed via qwen3.8-flash backend"}}]}`,
			from:     mappedModel,
			to:       clientModel,
			useAlias: true,
			expected: `data: {"model":"other-model","choices":[{"delta":{"content":"routed via qwen3.8-flash backend"}}]}`,
		},
		{
			// 无 model 字段不改写。
			name:     "无 model 字段不改写",
			line:     `data: {"id":"x","choices":[{"delta":{"content":"hi"}}]}`,
			from:     mappedModel,
			to:       clientModel,
			useAlias: true,
			expected: `data: {"id":"x","choices":[{"delta":{"content":"hi"}}]}`,
		},
		{
			// 未知 model 值不改写（不在别名集）。
			name:     "未知 model 值不改写",
			line:     `data: {"model":"gpt-4o","choices":[]}`,
			from:     mappedModel,
			to:       clientModel,
			useAlias: true,
			expected: `data: {"model":"gpt-4o","choices":[]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			if tt.useAlias {
				got = svc.replaceModelInSSELine(tt.line, tt.from, tt.to, aliases...)
			} else {
				got = svc.replaceModelInSSELine(tt.line, tt.from, tt.to)
			}
			require.Equal(t, tt.expected, got)
		})
	}
}

func TestReplaceModelInSSEBodyAliased(t *testing.T) {
	svc := &OpenAIGatewayService{}
	const (
		mappedModel = "qwen3.8-flash:free"
		clientModel = "kimi-k3"
	)
	aliases := IdentityRewriteAliases(mappedModel)

	tests := []struct {
		name     string
		body     string
		from     string
		to       string
		useAlias bool
		expected string
	}{
		{
			name:     "多行 SSE 去后缀别名匹配回写",
			body:     "data: {\"model\":\"qwen3.8-flash\",\"choices\":[]}\n\ndata: {\"model\":\"qwen3.8-flash:free\"}\n\ndata: [DONE]\n",
			from:     mappedModel,
			to:       clientModel,
			useAlias: true,
			expected: "data: {\"model\":\"kimi-k3\",\"choices\":[]}\n\ndata: {\"model\":\"kimi-k3\"}\n\ndata: [DONE]\n",
		},
		{
			name:     "非掩码账号多行保持原样",
			body:     "data: {\"model\":\"qwen3.8-flash\"}\n\ndata: [DONE]\n",
			from:     mappedModel,
			to:       clientModel,
			useAlias: false,
			expected: "data: {\"model\":\"qwen3.8-flash\"}\n\ndata: [DONE]\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			if tt.useAlias {
				got = svc.replaceModelInSSEBody(tt.body, tt.from, tt.to, aliases...)
			} else {
				got = svc.replaceModelInSSEBody(tt.body, tt.from, tt.to)
			}
			require.Equal(t, tt.expected, got)
		})
	}
}

func TestReplaceModelInResponseBodyAliased(t *testing.T) {
	svc := &OpenAIGatewayService{}
	const (
		mappedModel = "qwen3.8-flash:free"
		clientModel = "kimi-k3"
	)
	aliases := IdentityRewriteAliases(mappedModel)

	tests := []struct {
		name     string
		body     string
		from     string
		to       string
		useAlias bool
		expected string
	}{
		{
			name:     "去后缀别名匹配回写",
			body:     `{"id":"chatcmpl-1","model":"qwen3.8-flash","choices":[]}`,
			from:     mappedModel,
			to:       clientModel,
			useAlias: true,
			expected: `{"id":"chatcmpl-1","model":"kimi-k3","choices":[]}`,
		},
		{
			name:     "非掩码账号去后缀名保持原样",
			body:     `{"id":"chatcmpl-1","model":"qwen3.8-flash","choices":[]}`,
			from:     mappedModel,
			to:       clientModel,
			useAlias: false,
			expected: `{"id":"chatcmpl-1","model":"qwen3.8-flash","choices":[]}`,
		},
		{
			name:     "无 model 字段不改写",
			body:     `{"id":"chatcmpl-1","choices":[]}`,
			from:     mappedModel,
			to:       clientModel,
			useAlias: true,
			expected: `{"id":"chatcmpl-1","choices":[]}`,
		},
		{
			name:     "未知 model 值不改写",
			body:     `{"id":"chatcmpl-1","model":"gpt-4o","choices":[]}`,
			from:     mappedModel,
			to:       clientModel,
			useAlias: true,
			expected: `{"id":"chatcmpl-1","model":"gpt-4o","choices":[]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []byte
			if tt.useAlias {
				got = svc.replaceModelInResponseBody([]byte(tt.body), tt.from, tt.to, aliases...)
			} else {
				got = svc.replaceModelInResponseBody([]byte(tt.body), tt.from, tt.to)
			}
			require.Equal(t, tt.expected, string(got))
		})
	}
}

func TestIdentityMaskDetection(t *testing.T) {
	tests := []struct {
		name string
		acct *Account
		want bool
	}{
		{name: "nil 账号", acct: nil, want: false},
		{name: "Extra 为空", acct: &Account{}, want: false},
		{name: "缺省标志", acct: &Account{Extra: map[string]any{}}, want: false},
		{name: "标志为 false", acct: &Account{Extra: map[string]any{"mask_upstream_identity": false}}, want: false},
		{name: "标志为 true", acct: &Account{Extra: map[string]any{"mask_upstream_identity": true}}, want: true},
		{name: "字符串 true", acct: &Account{Extra: map[string]any{"mask_upstream_identity": "true"}}, want: true},
		{name: "字符串 1", acct: &Account{Extra: map[string]any{"mask_upstream_identity": "1"}}, want: true},
		{name: "字符串 false", acct: &Account{Extra: map[string]any{"mask_upstream_identity": "false"}}, want: false},
		{name: "其他类型", acct: &Account{Extra: map[string]any{"mask_upstream_identity": 1}}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, IsIdentityMaskedAccount(tt.acct))
		})
	}
}

func TestIdentityMaskAliases(t *testing.T) {
	tests := []struct {
		name        string
		mappedModel string
		want        []string
	}{
		{name: "含 :free 后缀", mappedModel: "qwen3.8-flash:free", want: []string{"qwen3.8-flash:free", "qwen3.8-flash"}},
		{name: "无冒号", mappedModel: "gpt-4o", want: []string{"gpt-4o"}},
		{name: "空串", mappedModel: "", want: []string{}},
		{name: "前导冒号不剥离", mappedModel: ":free", want: []string{":free"}},
		{name: "多冒号取最后一段", mappedModel: "org:team:model", want: []string{"org:team:model", "org:team"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, IdentityRewriteAliases(tt.mappedModel))
		})
	}
}
