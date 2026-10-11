//go:build unit

package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// legacyProviderDefaultBaseURL 复述 provider profile 引入前 account.go 中逐平台
// switch 的默认端点逻辑，作为等价性基准。
func legacyProviderDefaultBaseURL(platform, mode, protocol string) string {
	coding := mode == AccountModeCoding
	zen := mode == AccountModeZen
	switch protocol {
	case APIProtocolAnthropic:
		switch platform {
		case PlatformKimi:
			if coding {
				return DefaultKimiCodingAnthropicBaseURL
			}
			return DefaultKimiPayGAnthropicBaseURL
		case PlatformZhipu:
			return DefaultZhipuAnthropicBaseURL
		case PlatformDeepseek:
			return DefaultDeepseekAnthropicBaseURL
		case PlatformMiniMax:
			return DefaultMiniMaxAnthropicBaseURL
		case PlatformOpenCodeGo:
			if zen {
				return DefaultOpenCodeZenAnthropicBaseURL
			}
			return DefaultOpenCodeGoAnthropicBaseURL
		}
	case APIProtocolChatCompletions, APIProtocolResponses:
		if platform == PlatformZhipu && protocol == APIProtocolResponses {
			// 唯一的函数级差异：旧 switch 对智谱 Responses 也返回 CC 基址，profile
			// 以"缺条目即不支持"建模后返回空串。所有运行时调用方都先经
			// UsesNativeCNResponses / SupportsNativeCNResponses 守卫，智谱不会走到这里。
			return ""
		}
		switch platform {
		case PlatformKimi:
			if coding {
				return DefaultKimiCodingBaseURL
			}
			return DefaultKimiPayGBaseURL
		case PlatformZhipu:
			if coding {
				return DefaultZhipuCodingBaseURL
			}
			return DefaultZhipuPayGBaseURL
		case PlatformDeepseek:
			return DefaultDeepseekBaseURL
		case PlatformMiniMax:
			return DefaultMiniMaxBaseURL
		case PlatformOpenCodeGo:
			if zen {
				return DefaultOpenCodeZenBaseURL
			}
			return DefaultOpenCodeGoBaseURL
		}
	}
	return ""
}

func TestProviderProfile_DefaultBaseURLsMatchLegacySwitches(t *testing.T) {
	t.Parallel()

	platforms := []string{PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo}
	modes := []string{"", AccountModePayG, AccountModeCoding, AccountModeZen, AccountModeGo, " coding ", "unknown"}

	for _, platform := range platforms {
		for _, mode := range modes {
			legacyMode := mode
			if legacyMode == " coding " {
				legacyMode = AccountModeCoding
			}
			newAccount := func(protocol string) *Account {
				return &Account{
					Platform: platform,
					Type:     AccountTypeAPIKey,
					Credentials: map[string]any{
						"api_key":      "sk-test",
						"account_mode": mode,
						"api_protocol": protocol,
					},
				}
			}
			name := platform + "/" + mode

			chat := newAccount(APIProtocolChatCompletions)
			require.Equal(t, legacyProviderDefaultBaseURL(platform, legacyMode, APIProtocolChatCompletions), chat.GetOpenAIBaseURL(), name)

			anthropic := newAccount(APIProtocolAnthropic)
			require.Equal(t, legacyProviderDefaultBaseURL(platform, legacyMode, APIProtocolAnthropic), anthropic.GetAnthropicProtocolBaseURL(), name)
			require.Equal(t, legacyProviderDefaultBaseURL(platform, legacyMode, APIProtocolChatCompletions), anthropic.GetOpenAIFormatBaseURL(), name)

			adaptive := newAccount(APIProtocolAdaptive)
			for _, protocol := range []string{APIProtocolChatCompletions, APIProtocolResponses, APIProtocolAnthropic, "unknown"} {
				require.Equal(t, legacyProviderDefaultBaseURL(platform, legacyMode, protocol), adaptive.GetCNProtocolBaseURL(protocol), name+"/"+protocol)
			}
		}
	}
}

func TestProviderProfile_ProfilesMatchMultiProtocolPlatforms(t *testing.T) {
	t.Parallel()

	for _, platform := range []string{
		PlatformAnthropic, PlatformOpenAI, PlatformGemini, PlatformAntigravity, PlatformGrok,
		PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo, PlatformComposite, "",
	} {
		want := IsCNProvider(platform) || platform == PlatformOpenCodeGo
		require.Equal(t, want, IsMultiProtocolAPIKeyProvider(platform), platform)
		require.Equal(t, want, LookupProviderProfile(platform) != nil, platform)
	}

	for platform, profile := range providerProfiles {
		require.Equal(t, platform, profile.Platform)
		_, ok := profile.Modes[profile.DefaultMode]
		require.True(t, ok, "%s default mode %q must be registered", platform, profile.DefaultMode)
	}
}

func TestProviderProfile_NativeResponsesMatchesLegacy(t *testing.T) {
	t.Parallel()

	legacy := map[string]bool{
		PlatformDeepseek:   true,
		PlatformKimi:       true,
		PlatformMiniMax:    true,
		PlatformOpenCodeGo: true,
	}
	modes := []string{"", AccountModePayG, AccountModeCoding, AccountModeZen, AccountModeGo, "unknown"}
	for _, platform := range []string{PlatformOpenAI, PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo} {
		for _, mode := range modes {
			account := &Account{Platform: platform, Type: AccountTypeAPIKey, Credentials: map[string]any{"account_mode": mode}}
			require.Equal(t, legacy[platform], account.SupportsNativeCNResponses(), platform+"/"+mode)
		}
	}
	require.False(t, (*Account)(nil).SupportsNativeCNResponses())
}

func TestProviderProfile_BaseURLKeysAreNativeProtocols(t *testing.T) {
	t.Parallel()

	for platform, profile := range providerProfiles {
		for mode, endpoints := range profile.Modes {
			require.NotEmpty(t, endpoints.BaseURLs[APIProtocolChatCompletions], "%s/%s must have a chat_completions base", platform, mode)
			for protocol, baseURL := range endpoints.BaseURLs {
				require.True(t, isNativeUpstreamProtocol(protocol), "%s/%s has unknown protocol key %q", platform, mode, protocol)
				require.NotEmpty(t, baseURL, "%s/%s/%s", platform, mode, protocol)
			}
		}
	}
}

func TestProviderProfile_ResponsesPathMatchesLegacy(t *testing.T) {
	t.Parallel()

	// legacy 复述 provider profile 引入前的 fork 行为（规格 D-QLM-027）：DeepSeek
	// 按 base 主机分流——官方 api.deepseek.com → /responses，中转/自建 → /v1/responses。
	// 主机判定在此独立重写（不复用 isDeepSeekOfficialHost），以免等价性断言自我循环。
	legacy := func(platform, base string) string {
		if platform == PlatformDeepseek {
			host := ""
			if trimmed := strings.TrimSpace(base); trimmed != "" {
				if idx := strings.Index(trimmed, "://"); idx >= 0 {
					trimmed = trimmed[idx+3:]
				}
				if idx := strings.IndexAny(trimmed, "/?#"); idx >= 0 {
					trimmed = trimmed[:idx]
				}
				if idx := strings.LastIndex(trimmed, "@"); idx >= 0 {
					trimmed = trimmed[idx+1:]
				}
				if idx := strings.Index(trimmed, ":"); idx >= 0 {
					trimmed = trimmed[:idx]
				}
				host = strings.TrimSuffix(trimmed, ".")
			}
			if strings.EqualFold(host, "api.deepseek.com") {
				return buildOpenAIEndpointURL(base, "/responses")
			}
			return buildOpenAIResponsesURL(base)
		}
		return buildOpenAIResponsesURL(base)
	}
	bases := []string{
		"https://api.deepseek.com", "https://relay.example.com", "https://relay.example.com/v1",
		"https://opencode.ai/zen/go/v1", "https://open.bigmodel.cn/api/paas/v4", "https://api.moonshot.cn/v1",
	}
	for _, platform := range []string{PlatformOpenAI, PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo} {
		for _, base := range bases {
			require.Equal(t, legacy(platform, base), buildOpenAIResponsesURLForPlatform(platform, base), platform+" "+base)
		}
	}
}

func TestProviderProfile_OpenCodeProtocolRulesByMode(t *testing.T) {
	t.Parallel()

	openCode := LookupProviderProfile(PlatformOpenCodeGo)
	require.Equal(t, DefaultOpenCodeGoProtocolRules(), openCode.Endpoints(AccountModeGo).ProtocolRules)
	require.Equal(t, DefaultOpenCodeGoProtocolRules(), openCode.Endpoints("").ProtocolRules)
	require.Equal(t, DefaultOpenCodeZenProtocolRules(), openCode.Endpoints(AccountModeZen).ProtocolRules)
	require.Empty(t, LookupProviderProfile(PlatformKimi).Endpoints(AccountModeCoding).ProtocolRules)
}
