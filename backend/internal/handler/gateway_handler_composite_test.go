//go:build unit

package handler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// TestCompositeCrossChainChatForwardKind 钉住跨链 chat 按选中账号所属执行链分派的 kind：
// 依次以 openai→anthropic→gemini 调用 CompositeAccountChainPath（与 A1 selector 链顺序
// 一致），首条可服务本端点的链胜出；三条链都不可时返回空串，绝不静默回落到其它链。
// 跨链分派的完整转发需真实服务，本单豁免，仅覆盖纯判定函数。
func TestCompositeCrossChainChatForwardKind(t *testing.T) {
	cases := []struct {
		name    string
		account *service.Account
		want    string
	}{
		{
			name:    "zhipu goes openai",
			account: &service.Account{Platform: service.PlatformZhipu, Type: service.AccountTypeAPIKey},
			want:    "openai",
		},
		{
			name:    "anthropic goes anthropic",
			account: &service.Account{Platform: service.PlatformAnthropic, Type: service.AccountTypeOAuth},
			want:    "anthropic",
		},
		{
			// Anthropic 链 chat 分支对原生 gemini 账号返回 CompositePathGemini 出口，
			// 归到 gemini 转发器，避免交给通用 gateway 重复处理。
			name:    "gemini goes gemini",
			account: &service.Account{Platform: service.PlatformGemini, Type: service.AccountTypeOAuth},
			want:    "gemini",
		},
		{
			// 按 A1 顺序：cursor Connect 在 openai 链 chat 已可服务（CompositePathCursor）。
			name:    "cursor connect goes openai per A1 order",
			account: &service.Account{Platform: service.PlatformCursor, Type: service.AccountTypeOAuth},
			want:    "openai",
		},
		{
			// 按 A1 顺序：antigravity OAuth 不进 openai 链，首条可服务链为 anthropic。
			name:    "antigravity oauth goes anthropic per A1 order",
			account: &service.Account{Platform: service.PlatformAntigravity, Type: service.AccountTypeOAuth},
			want:    "anthropic",
		},
		{
			name:    "unknown goes empty",
			account: &service.Account{Platform: "unknown-platform", Type: service.AccountTypeAPIKey},
			want:    "",
		},
		{
			name:    "nil goes empty",
			account: nil,
			want:    "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, compositeCrossChainChatForwardKind(tc.account))
		})
	}
}

// TestCompositeCrossChainResponsesForwardKind 钉住跨链 responses 按选中账号所属执行链
// 分派的 kind：依次以 openai→anthropic 调用 CompositeAccountChainPath（与 A1 selector
// 链顺序一致），首条可服务本端点的链胜出；gemini 在 responses 端点不可服务（A1 返回
// false），必须返回空串，不得映射为 gemini；两条链都不可时也返回空串，绝不静默回落。
// 跨链分派的完整转发需真实服务，本单豁免，仅覆盖纯判定函数。
func TestCompositeCrossChainResponsesForwardKind(t *testing.T) {
	cases := []struct {
		name    string
		account *service.Account
		want    string
	}{
		{
			name:    "zhipu APIKey goes openai",
			account: &service.Account{Platform: service.PlatformZhipu, Type: service.AccountTypeAPIKey},
			want:    "openai",
		},
		{
			name:    "cursor APIKey goes openai",
			account: &service.Account{Platform: service.PlatformCursor, Type: service.AccountTypeAPIKey},
			want:    "openai",
		},
		{
			// 按 A1 顺序：cursor Connect 在 openai 链 responses 已可服务。
			name:    "cursor connect goes openai per A1 order",
			account: &service.Account{Platform: service.PlatformCursor, Type: service.AccountTypeOAuth},
			want:    "openai",
		},
		{
			name:    "anthropic goes anthropic",
			account: &service.Account{Platform: service.PlatformAnthropic, Type: service.AccountTypeOAuth},
			want:    "anthropic",
		},
		{
			// 按 A1 顺序：antigravity OAuth 不进 openai 链，首条可服务链为 anthropic。
			name:    "antigravity oauth goes anthropic per A1 order",
			account: &service.Account{Platform: service.PlatformAntigravity, Type: service.AccountTypeOAuth},
			want:    "anthropic",
		},
		{
			// responses 端点 gemini 三链均不可服务（anthropic 链显式排除、gemini 链返回 false）。
			name:    "gemini goes empty",
			account: &service.Account{Platform: service.PlatformGemini, Type: service.AccountTypeOAuth},
			want:    "",
		},
		{
			name:    "unknown goes empty",
			account: &service.Account{Platform: "unknown-platform", Type: service.AccountTypeAPIKey},
			want:    "",
		},
		{
			name:    "nil goes empty",
			account: nil,
			want:    "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, compositeCrossChainResponsesForwardKind(tc.account))
		})
	}
}
