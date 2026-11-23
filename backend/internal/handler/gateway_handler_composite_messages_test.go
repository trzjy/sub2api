//go:build unit

package handler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// TestCompositeCrossChainMessagesForwardKind 钉住跨链 messages 按选中账号所属执行链
// 分派的 kind：依次以 openai→anthropic→gemini 调用 CompositeAccountChainPath（与 A1
// selector 链顺序一致），首条可服务本端点的链胜出；三条链都不可时返回空串，绝不静默
// 回落。gemini 账号在 anthropic 链 messages 分支被排除，落到 gemini 链，故返回
// "gemini"。跨链分派的完整转发需真实服务，本单豁免，仅覆盖纯判定函数。
func TestCompositeCrossChainMessagesForwardKind(t *testing.T) {
	cases := []struct {
		name    string
		account *service.Account
		want    string
	}{
		{
			name:    "openai family goes openai",
			account: &service.Account{Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey},
			want:    "openai",
		},
		{
			name:    "zhipu CN goes openai",
			account: &service.Account{Platform: service.PlatformZhipu, Type: service.AccountTypeAPIKey},
			want:    "openai",
		},
		{
			name:    "cursor APIKey goes openai",
			account: &service.Account{Platform: service.PlatformCursor, Type: service.AccountTypeAPIKey},
			want:    "openai",
		},
		{
			// cursor Connect 在 messages 端点三链皆可，按 A1 顺序首条 openai 胜出。
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
			// antigravity OAuth 不进 openai 链，首条可服务链为 anthropic。
			name:    "antigravity oauth goes anthropic per A1 order",
			account: &service.Account{Platform: service.PlatformAntigravity, Type: service.AccountTypeOAuth},
			want:    "anthropic",
		},
		{
			// gemini 在 anthropic 链 messages 分支被排除，落到 gemini 链。
			name:    "gemini goes gemini",
			account: &service.Account{Platform: service.PlatformGemini, Type: service.AccountTypeOAuth},
			want:    "gemini",
		},
		{
			// typesafe 仅 systemone，messages 三链均不可服务。
			name:    "typesafe goes empty",
			account: &service.Account{Platform: service.PlatformTypeSafe, Type: service.AccountTypeAPIKey},
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
			require.Equal(t, tc.want, compositeCrossChainMessagesForwardKind(tc.account))
		})
	}
}

// TestFilterCompositeMessagesSupply 钉住跨链 messages 供给过滤的 messages 权限口径：
// group 为 nil 或 AllowMessagesDispatch=true 时原样返回；false 时剔除"OpenAI 链且
// 平台不在豁免集合"的 claimant，豁免集合与 C9 openAIMessagesDispatchExemptPlatform
// 同口径（grok/CN/other/muse/codebuddy/devin/多协议），非 OpenAI 链 claimant 保留。
func TestFilterCompositeMessagesSupply(t *testing.T) {
	claim := func(id int64, platform string, typ string) service.CompositeOwnershipClaimant {
		return service.CompositeOwnershipClaimant{AccountID: id, Platform: platform, Type: typ}
	}
	supply := []service.CompositeOwnershipClaimant{
		claim(1, service.PlatformOpenAI, service.AccountTypeAPIKey),   // OpenAI 链、非豁免 → 应剔除
		claim(2, service.PlatformGrok, service.AccountTypeAPIKey),     // OpenAI 链、豁免 → 保留
		claim(3, service.PlatformZhipu, service.AccountTypeAPIKey),    // CN 供应商、豁免 → 保留
		claim(4, service.PlatformCodeBuddy, service.AccountTypeOAuth), // codebuddy、豁免 → 保留
		claim(5, service.PlatformDevin, service.AccountTypeAPIKey),    // devin、豁免 → 保留
		claim(6, service.PlatformAnthropic, service.AccountTypeOAuth), // 非 OpenAI 链 → 保留
		claim(7, service.PlatformGemini, service.AccountTypeOAuth),    // 非 OpenAI 链 → 保留
		claim(8, service.PlatformOther, service.AccountTypeAPIKey),    // other、豁免 → 保留
		claim(9, service.PlatformMuse, service.AccountTypeAPIKey),     // muse、豁免 → 保留
	}
	decision := service.CompositeRouteDecision{
		Matched:        true,
		ServiceChain:   service.CompositeChainAnthropic,
		SupplyAccounts: supply,
	}

	t.Run("nil group returns original", func(t *testing.T) {
		got := filterCompositeMessagesSupply(decision, nil)
		require.Equal(t, supply, got)
	})

	t.Run("allow dispatch returns original", func(t *testing.T) {
		got := filterCompositeMessagesSupply(decision, &service.Group{AllowMessagesDispatch: true})
		require.Equal(t, supply, got)
	})

	t.Run("deny dispatch drops non-exempt openai chain only", func(t *testing.T) {
		got := filterCompositeMessagesSupply(decision, &service.Group{AllowMessagesDispatch: false})
		ids := make([]int64, 0, len(got))
		for _, c := range got {
			ids = append(ids, c.AccountID)
		}
		require.Equal(t, []int64{2, 3, 4, 5, 6, 7, 8, 9}, ids)
	})
}
