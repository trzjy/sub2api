//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/Wei-Shaw/sub2api/internal/config"
)

// newNS2SAccount 构造一个最小可用账号用于资格矩阵测试。
func newNS2SAccount(platform, accountType, baseURL, accessMode string) *Account {
	cred := map[string]any{}
	if baseURL != "" {
		cred["base_url"] = baseURL
	}
	if accessMode != "" {
		cred["access_mode"] = accessMode
	}
	return &Account{
		Platform:    platform,
		Type:        accountType,
		Credentials: cred,
	}
}

func enabledService() *OpenAIGatewayService {
	// 零值 GatewayConfig = 启用（NonstreamToStreamDisabled 零值 false），
	// 显式省略该字段即落在启用侧；不写 Disabled:false 以免重复语义。
	return &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{}}}
}

func disabledService() *OpenAIGatewayService {
	return &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{NonstreamToStreamDisabled: true}}}
}

// TestShouldConvertNonstreamToStream 资格矩阵：覆盖各个平台/账号形态的转换判定。
func TestShouldConvertNonstreamToStream(t *testing.T) {
	const proxyBaseURL = "https://tokenharbor.ai/v1"

	cases := []struct {
		name     string
		account  *Account
		expected bool
	}{
		// 四平台 apikey + 第三方中转 base_url = 转换
		{"deepseek apikey + 中转", newNS2SAccount(PlatformDeepseek, AccountTypeAPIKey, proxyBaseURL, ""), true},
		{"kimi apikey + 中转", newNS2SAccount(PlatformKimi, AccountTypeAPIKey, proxyBaseURL, ""), true},
		{"zhipu apikey + 中转", newNS2SAccount(PlatformZhipu, AccountTypeAPIKey, proxyBaseURL, ""), true},
		{"minimax apikey + 中转", newNS2SAccount(PlatformMiniMax, AccountTypeAPIKey, proxyBaseURL, ""), true},

		// 四平台 apikey + 官方域 base_url = 不转换
		{"deepseek apikey + 官方", newNS2SAccount(PlatformDeepseek, AccountTypeAPIKey, DefaultDeepseekBaseURL, ""), false},
		{"kimi apikey + 官方", newNS2SAccount(PlatformKimi, AccountTypeAPIKey, DefaultKimiPayGBaseURL, ""), false},
		{"zhipu apikey + 官方coding", newNS2SAccount(PlatformZhipu, AccountTypeAPIKey, DefaultZhipuCodingBaseURL, ""), false},
		{"minimax apikey + 国内官方", newNS2SAccount(PlatformMiniMax, AccountTypeAPIKey, DefaultMiniMaxBaseURL, ""), false},
		{"minimax apikey + 国际官方", newNS2SAccount(PlatformMiniMax, AccountTypeAPIKey, "https://api.minimax.io/v1", ""), false},

		// 官方域大小写变体（host 比对前小写化）→ 仍不转换
		{"deepseek 官方大写直连", newNS2SAccount(PlatformDeepseek, AccountTypeAPIKey, "HTTPS://API.DEEPSEEK.COM/v1", ""), false},
		{"kimi 官方大小写混合", newNS2SAccount(PlatformKimi, AccountTypeAPIKey, "https://Api.Moonshot.CN/v1", ""), false},

		// other apikey（任意自定义 base_url）= 转换
		{"other apikey + 自定义中转", newNS2SAccount(PlatformOther, AccountTypeAPIKey, proxyBaseURL, ""), true},
		{"other apikey + 自定义官方形似", newNS2SAccount(PlatformOther, AccountTypeAPIKey, "https://api.openai.com/v1", ""), true},

		// web 接入账号 = 不转换
		{"zhipu web 接入", newNS2SAccount(PlatformZhipu, AccountTypeAPIKey, proxyBaseURL, AccountAccessModeWeb), false},
		{"deepseek web 接入", newNS2SAccount(PlatformDeepseek, AccountTypeAPIKey, proxyBaseURL, AccountAccessModeWeb), false},
		{"other web 接入", newNS2SAccount(PlatformOther, AccountTypeAPIKey, proxyBaseURL, AccountAccessModeWeb), false},

		// openai OAuth / grok / codebuddy = 不转换
		{"openai OAuth", newNS2SAccount(PlatformOpenAI, AccountTypeOAuth, "", ""), false},
		{"openai apikey", newNS2SAccount(PlatformOpenAI, AccountTypeAPIKey, "", ""), false},
		{"grok apikey", newNS2SAccount(PlatformGrok, AccountTypeAPIKey, "", ""), false},
		{"codebuddy apikey", newNS2SAccount(PlatformCodeBuddy, AccountTypeAPIKey, "", ""), false},

		// 非 apikey 类型（upstream / oauth）一律不转换
		{"deepseek upstream", newNS2SAccount(PlatformDeepseek, AccountTypeUpstream, proxyBaseURL, ""), false},

		// nil 账号
		{"nil 账号", nil, false},
	}

	svc := enabledService()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, svc.shouldConvertNonstreamToStream(tc.account))
		})
	}
}

// TestShouldConvertNonstreamToStream_Disabled 开关 false = 一律 false。
func TestShouldConvertNonstreamToStream_Disabled(t *testing.T) {
	svc := disabledService()
	candidates := []*Account{
		newNS2SAccount(PlatformDeepseek, AccountTypeAPIKey, "https://tokenharbor.ai/v1", ""),
		newNS2SAccount(PlatformKimi, AccountTypeAPIKey, "https://tokenharbor.ai/v1", ""),
		newNS2SAccount(PlatformZhipu, AccountTypeAPIKey, "https://tokenharbor.ai/v1", ""),
		newNS2SAccount(PlatformMiniMax, AccountTypeAPIKey, "https://tokenharbor.ai/v1", ""),
		newNS2SAccount(PlatformOther, AccountTypeAPIKey, "https://tokenharbor.ai/v1", ""),
	}
	for _, acc := range candidates {
		require.False(t, svc.shouldConvertNonstreamToStream(acc), "开关关闭时任何账号都不应转换")
	}
}

// TestIsOfficialCNUpstream 官方 host 集合判定（仅由既有常量 + api.minimax.io 派生）。
func TestIsOfficialCNUpstream(t *testing.T) {
	official := []string{
		DefaultDeepseekBaseURL,
		DefaultKimiPayGBaseURL,
		DefaultKimiCodingBaseURL,
		DefaultZhipuPayGBaseURL,
		DefaultZhipuCodingBaseURL,
		DefaultMiniMaxBaseURL,
		"https://api.minimax.io/v1",
	}
	for _, u := range official {
		t.Run("official:"+u, func(t *testing.T) {
			require.True(t, isOfficialCNUpstreamBaseURL(newNS2SAccount(PlatformDeepseek, AccountTypeAPIKey, u, "")),
				"官方域应判定为官方直连")
		})
	}

	nonOfficial := []string{
		"https://tokenharbor.ai/v1",
		"https://my-proxy.example.com/v1",
		"http://localhost:8080/v1",
	}
	for _, u := range nonOfficial {
		t.Run("proxy:"+u, func(t *testing.T) {
			require.False(t, isOfficialCNUpstreamBaseURL(newNS2SAccount(PlatformDeepseek, AccountTypeAPIKey, u, "")),
				"第三方中转域不应判定为官方直连")
		})
	}

	// 带端口的官方域也应命中（Hostname 去端口）。
	require.True(t, isOfficialCNUpstreamBaseURL(newNS2SAccount(PlatformDeepseek, AccountTypeAPIKey, "https://api.deepseek.com:443", "")))

	// other 无官方端点：恒返回 false。
	require.False(t, isOfficialCNUpstreamBaseURL(newNS2SAccount(PlatformOther, AccountTypeAPIKey, "https://api.deepseek.com", "")))
	require.False(t, isOfficialCNUpstreamBaseURL(newNS2SAccount(PlatformOther, AccountTypeAPIKey, "", "")))
}
