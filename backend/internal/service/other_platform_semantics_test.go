package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestOtherPlatformPredicateCoversSharedBaseFamily 锁定 UsesOpenAIProtocolSharedBaseURL：
// 覆盖 openai/CN/other，排除 grok/anthropic/gemini/composite。
func TestOtherPlatformPredicateCoversSharedBaseFamily(t *testing.T) {
	for _, p := range []string{PlatformOpenAI, PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformOther} {
		require.Truef(t, UsesOpenAIProtocolSharedBaseURL(p), "platform %q should use shared OpenAI base", p)
	}
	for _, p := range []string{PlatformGrok, PlatformAnthropic, PlatformGemini, PlatformComposite} {
		require.Falsef(t, UsesOpenAIProtocolSharedBaseURL(p), "platform %q must NOT use shared OpenAI base", p)
	}
}

// TestOtherPlatformEmptyBaseURLFailsClosed 锁定 other 平台空 base_url 不得回落官方 OpenAI：
// GetOpenAIBaseURL 对 other 返回 ""（为上层失败关闭提供依据，见 openai_gateway_cc_pipeline 等入口）。
func TestOtherPlatformEmptyBaseURLFailsClosed(t *testing.T) {
	acc := &Account{
		Platform:    PlatformOther,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{},
	}
	require.Equal(t, "", acc.GetOpenAIBaseURL())

	// 有自定义 base_url 时正常返回。
	acc.Credentials["base_url"] = "https://api.example-custom.com/v1"
	require.Equal(t, "https://api.example-custom.com/v1", acc.GetOpenAIBaseURL())
}

// TestOtherPlatformAPIKeyProtocolAccess 锁定 other APIKey 账号能经共享协议 getter 读密钥。
func TestOtherPlatformAPIKeyProtocolAccess(t *testing.T) {
	acc := &Account{
		Platform: PlatformOther,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-custom",
			"base_url": "https://api.example-custom.com/v1",
		},
	}
	require.Equal(t, "sk-custom", acc.GetOpenAIProtocolAPIKey())
	require.Equal(t, APIProtocolChatCompletions, acc.GetAPIProtocol())
}
