//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveWebConsoleQuotaProvider(t *testing.T) {
	tests := []struct {
		name     string
		account  Account
		expected string
	}{
		// --- 三家命中 ---
		{
			name: "compshare cp gateway hits",
			account: Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"base_url": "https://cp.compshare.cn/api/v1",
				},
			},
			expected: providerCompshare,
		},
		{
			name: "compshare console hits",
			account: Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"base_url": "https://console.compshare.cn",
				},
			},
			expected: providerCompshare,
		},
		{
			name: "compshare bare root hits",
			account: Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"base_url": "https://compshare.cn",
				},
			},
			expected: providerCompshare,
		},
		{
			name: "sensenova token hits",
			account: Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"base_url": "https://token.sensenova.cn/v1",
				},
			},
			expected: providerSensenova,
		},
		{
			name: "sensenova platform hits",
			account: Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"base_url": "https://platform.sensenova.cn",
				},
			},
			expected: providerSensenova,
		},
		{
			name: "longcat api hits",
			account: Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"base_url": "https://api.longcat.chat/v1",
				},
			},
			expected: providerLongcat,
		},
		{
			name: "longcat console hits",
			account: Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"base_url": "https://longcat.chat",
				},
			},
			expected: providerLongcat,
		},
		// --- 负例 ---
		{
			name: "evil suffix does not hit",
			account: Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"base_url": "https://compshare.cn.evil.com",
				},
			},
			expected: "",
		},
		{
			name: "notn subdomain still hits",
			account: Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"base_url": "https://notn.compshare.cn",
				},
			},
			expected: providerCompshare,
		},
		{
			name: "empty base_url does not hit",
			account: Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"base_url": "",
				},
			},
			expected: "",
		},
		{
			name: "unrelated deepseek does not hit",
			account: Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"base_url": "https://api.deepseek.com",
				},
			},
			expected: "",
		},
		{
			name: "unrelated openai does not hit",
			account: Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"base_url": "https://api.openai.com/v1",
				},
			},
			expected: "",
		},
		// --- 大小写混合命中 ---
		{
			name: "mixed case host hits",
			account: Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"base_url": "https://CP.Compshare.CN/api/v1",
				},
			},
			expected: providerCompshare,
		},
		// --- GetOpenAIBaseURL 为空时回退 GetBaseURL 命中 ---
		{
			name: "fallback to GetBaseURL when GetOpenAIBaseURL empty",
			account: Account{
				// Platform 非 OpenAI 兼容 → GetOpenAIBaseURL 返回 ""，回退 GetBaseURL。
				Type:     AccountTypeAPIKey,
				Platform: PlatformAnthropic,
				Credentials: map[string]any{
					"base_url": "https://api.longcat.chat/v1",
				},
			},
			expected: providerLongcat,
		},
		// --- nil 账号 ---
		{
			name:     "nil account does not hit",
			account:  Account{},
			expected: "",
		},
		// --- iflytek 命中（第四家）---
		{
			name: "iflytek maas api chat completions hits",
			account: Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"base_url": "https://maas-api.cn-huabei-1.xf-yun.com/v1/chat/completions",
				},
			},
			expected: providerIflytek,
		},
		{
			name: "iflytek maas api anthropic hits",
			account: Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"base_url": "https://maas-api.cn-huabei-1.xf-yun.com/anthropic/v1/messages",
				},
			},
			expected: providerIflytek,
		},
		{
			name: "iflytek maas coding api responses hits",
			account: Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"base_url": "https://maas-coding-api.cn-huabei-1.xf-yun.com/v1/responses",
				},
			},
			expected: providerIflytek,
		},
		{
			name: "iflytek evil suffix does not hit",
			account: Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"base_url": "https://xf-yun.com.evil.com/v1/chat/completions",
				},
			},
			expected: "",
		},
		{
			name: "iflytek xfyun cn open platform does not hit",
			account: Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"base_url": "https://www.xfyun.cn/",
				},
			},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var acc *Account
			if tt.name != "nil account does not hit" {
				acc = &tt.account
			}
			got := ResolveWebConsoleQuotaProvider(acc)
			require.Equal(t, tt.expected, got)
		})
	}
}

func TestWebConsoleHostMatches(t *testing.T) {
	tests := []struct {
		host     string
		root     string
		expected bool
	}{
		{"cp.compshare.cn", "compshare.cn", true},
		{"compshare.cn", "compshare.cn", true},
		{"compshare.cn.evil.com", "compshare.cn", false},
		{"notn.compshare.cn", "compshare.cn", true},
		{"CP.Compshare.CN", "compshare.cn", true},
		{"  compshare.cn  ", "compshare.cn", true},
		{"evilcompshare.cn", "compshare.cn", false},
		{"sensenova.cn", "sensenova.cn", true},
		{"token.sensenova.cn", "sensenova.cn", true},
		{"longcat.chat", "longcat.chat", true},
		{"api.longcat.chat", "longcat.chat", true},
	}
	for _, tt := range tests {
		require.Equal(t, tt.expected, webConsoleHostMatches(tt.host, tt.root),
			"webConsoleHostMatches(%q,%q)", tt.host, tt.root)
	}
}
