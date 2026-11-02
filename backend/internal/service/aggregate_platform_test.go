//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 聚合平台候选模型：账号侧按平台类型放行、模型侧按家族匹配（fail closed）。

func TestIsAggregatePlatformAccount(t *testing.T) {
	tests := []struct {
		platform string
		expected bool
	}{
		{PlatformAntigravity, true},
		{PlatformCursor, true},
		{PlatformCodeBuddy, true},
		{PlatformMuse, true},
		{PlatformOpenCodeGo, true},
		{PlatformCommandCode, true},
		{PlatformCline, true},
		{PlatformOther, true},
		{PlatformAnthropic, false},
		{PlatformOpenAI, false},
		{PlatformGemini, false},
		{PlatformGrok, false},
		{PlatformKimi, false},
		{PlatformZhipu, false},
		{PlatformDeepseek, false},
		{PlatformComposite, false},
		{"unknown", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.platform, func(t *testing.T) {
			got := isAggregatePlatformAccount(tt.platform)
			require.Equal(t, tt.expected, got, "platform=%s", tt.platform)
		})
	}
}

func TestMixedListingAccountAllowed(t *testing.T) {
	tests := []struct {
		name          string
		groupPlatform string
		account       *Account
		expected      bool
	}{
		{
			name:          "antigravity account allowed for anthropic group",
			groupPlatform: PlatformAnthropic,
			account:       &Account{Platform: PlatformAntigravity},
			expected:      true,
		},
		{
			name:          "antigravity account allowed for gemini group",
			groupPlatform: PlatformGemini,
			account:       &Account{Platform: PlatformAntigravity},
			expected:      true,
		},
		{
			name:          "cursor account allowed for openai group",
			groupPlatform: PlatformOpenAI,
			account:       &Account{Platform: PlatformCursor},
			expected:      true,
		},
		{
			name:          "anthropic account not allowed for gemini group",
			groupPlatform: PlatformGemini,
			account:       &Account{Platform: PlatformAnthropic},
			expected:      false,
		},
		{
			name:          "openai account not allowed for anthropic group",
			groupPlatform: PlatformAnthropic,
			account:       &Account{Platform: PlatformOpenAI},
			expected:      false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mixedListingAccountAllowed(tt.groupPlatform, tt.account)
			require.Equal(t, tt.expected, got)
		})
	}
}

func TestMixedListingModelAllowed(t *testing.T) {
	tests := []struct {
		name          string
		groupPlatform string
		model         string
		expected      bool
	}{
		{
			name:          "claude model allowed in anthropic group",
			groupPlatform: PlatformAnthropic,
			model:         "claude-opus-5-5",
			expected:      true,
		},
		{
			name:          "gemini model allowed in gemini group",
			groupPlatform: PlatformGemini,
			model:         "gemini-4-argon",
			expected:      true,
		},
		{
			name:          "gpt model allowed in openai group",
			groupPlatform: PlatformOpenAI,
			model:         "gpt-5.6-sol",
			expected:      true,
		},
		{
			name:          "claude model not allowed in gemini group",
			groupPlatform: PlatformGemini,
			model:         "claude-opus-5-5",
			expected:      false,
		},
		{
			name:          "gemini model not allowed in anthropic group",
			groupPlatform: PlatformAnthropic,
			model:         "gemini-4-argon",
			expected:      false,
		},
		{
			name:          "unknown model not allowed in any group",
			groupPlatform: PlatformAnthropic,
			model:         "unknown-model-xyz",
			expected:      false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mixedListingModelAllowed(tt.groupPlatform, tt.model)
			require.Equal(t, tt.expected, got)
		})
	}
}
