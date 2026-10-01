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

// TestMuseAccountIsOpenAICompatible 回归保护：muse 必须被判定为 OpenAI 协议族账号
// （muse-11 白名单整改）。muse 是 OpenCode Go 上游 Meta Muse Spark Contributor
// （非国产供应商），上游为 Responses 协议、转发链与 openai responses 账号同轨；若漏加
// 会在 openai_account_scheduler.go:718/1673 与 openai_gateway_scheduling.go:397 被兼容
// 过滤排除，导致 muse 组 chat/completions 呼叫 pool=0 的 503。
func TestMuseAccountIsOpenAICompatible(t *testing.T) {
	account := &Account{
		ID:          1,
		Platform:    PlatformMuse,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
	}

	// muse 与 kimi/zhipu/codebuddy 同轨，归一必须保留原值（muse-11 白名单整改）。
	// 此断言在 non-build-tag 文件内，确保验证白名单（不带 -tags unit）也能覆盖。
	require.Equal(t, PlatformMuse, NormalizeOpenAICompatiblePlatform(PlatformMuse),
		"NormalizeOpenAICompatiblePlatform(muse) 必须保留 muse 原值，不得归并为 openai")

	require.True(t, account.IsOpenAICompatible(), "muse 必须属于 OpenAI 协议族")
	require.True(t, account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityChatCompletions),
		"muse 必须支持 Chat Completions 能力")

	reason := openAICompatibleAccountEligibilityFailureReasonBeforeProfit(
		context.Background(), account, PlatformMuse, "", false, OpenAIEndpointCapabilityChatCompletions,
	)
	require.Empty(t, reason, "muse 账号不应因平台或能力被选号资格拒绝")

	// 既有平台判定不受影响（含归一/国产/其它）。
	require.False(t, (&Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth}).IsOpenAICompatible())
	require.False(t, (&Account{Platform: PlatformGemini, Type: AccountTypeOAuth}).IsOpenAICompatible())
	require.True(t, (&Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}).IsOpenAICompatible())
	require.True(t, (&Account{Platform: PlatformKimi, Type: AccountTypeAPIKey}).IsOpenAICompatible())
}
