package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCodeBuddyAccountIsOpenAICompatible 回归保护：CodeBuddy 必须被判定为
// OpenAI 协议族账号，否则 OpenAI 选号资格检查会以 platform_mismatch 将其全部
// 排除（PR2 曾漏加，导致 codebuddy 账号实际选不到号）。
func TestCodeBuddyAccountIsOpenAICompatible(t *testing.T) {
	account := &Account{
		ID:          1,
		Platform:    PlatformCodeBuddy,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
	}

	require.True(t, account.IsOpenAICompatible(), "CodeBuddy 必须属于 OpenAI 协议族")
	require.True(t, account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityChatCompletions),
		"CodeBuddy 必须支持 Chat Completions 能力")

	reason := openAICompatibleAccountEligibilityFailureReasonBeforeProfit(
		context.Background(), account, PlatformCodeBuddy, "", false, OpenAIEndpointCapabilityChatCompletions,
	)
	require.Empty(t, reason, "CodeBuddy 账号不应因平台或能力被选号资格拒绝")

	// 其它平台判定不受影响。
	require.False(t, (&Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth}).IsOpenAICompatible())
	require.False(t, (&Account{Platform: PlatformGemini, Type: AccountTypeOAuth}).IsOpenAICompatible())
	require.True(t, (&Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}).IsOpenAICompatible())
	require.True(t, (&Account{Platform: PlatformKimi, Type: AccountTypeAPIKey}).IsOpenAICompatible())
}
