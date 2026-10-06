package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMuseGetOpenAIProtocolAPIKey 锚定 muse-12 修复点 1：muse 账号直读
// credentials.api_key，而非回落 GetOpenAIApiKey()（muse 非 openai 平台 → 原返回 ""）。
// 与 muse_models.go 模型目录同步的直读模式对齐。
func TestMuseGetOpenAIProtocolAPIKey(t *testing.T) {
	t.Parallel()

	// muse apikey 账号 → 返回 credentials.api_key。
	acc := &Account{
		Platform: PlatformMuse,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key": "muse-secret",
		},
	}
	require.Equal(t, "muse-secret", acc.GetOpenAIProtocolAPIKey(), "muse 必须直读 credentials.api_key")

	// 非 apikey 类型 → 返回空（与 CN/other 同分支守卫一致）。
	nonKey := &Account{
		Platform: PlatformMuse,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"api_key": "should-not-leak",
		},
	}
	require.Equal(t, "", nonKey.GetOpenAIProtocolAPIKey(), "muse 非 apikey 类型不得返回密钥")
}

// TestMuseGetOpenAIBaseURL 锚定 muse-12 修复点 3：muse 账号 credentials.base_url
// 优先；缺失时回落固定默认 DefaultMuseBaseURL（绝不静默回落官方 api.openai.com）。
func TestMuseGetOpenAIBaseURL(t *testing.T) {
	t.Parallel()

	// 含 base_url → 返回它。
	withBase := &Account{
		Platform: PlatformMuse,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://custom.example/v1",
		},
	}
	require.Equal(t, "https://custom.example/v1", withBase.GetOpenAIBaseURL(), "muse 必须优先返回 credentials.base_url")

	// 不含 base_url → 返回固定默认 DefaultMuseBaseURL。
	noBase := &Account{
		Platform: PlatformMuse,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key": "muse-secret",
		},
	}
	require.Equal(t, DefaultMuseBaseURL, noBase.GetOpenAIBaseURL(), "muse 缺失 base_url 必须回落 DefaultMuseBaseURL")
	require.NotEqual(t, "https://api.openai.com", noBase.GetOpenAIBaseURL(), "muse 不得静默回落官方 OpenAI 端点")
}

// TestMuseUsesSharedBaseURLExcluded 锁定 muse-11 禁区：UsesOpenAIProtocolSharedBaseURL
// 绝不加入 muse。GetOpenAIBaseURL 的 muse 放行必须且只能落在本函数内部（muse-12 改动），
// 不能靠改共享判定集合实现。
func TestMuseUsesSharedBaseURLExcluded(t *testing.T) {
	t.Parallel()

	require.False(t, UsesOpenAIProtocolSharedBaseURL(PlatformMuse),
		"muse 必须保持被 UsesOpenAIProtocolSharedBaseURL 排除（muse-11 禁区）")
}

// TestOpenAICNGetOpenAIProtocolAPIKeyBaseURLUnchanged 确认 muse-12 改动不回归
// openai / 国产 CN / other 既有行为：GetOpenAIProtocolAPIKey 与 GetOpenAIBaseURL
// 对既有平台返回值不变。
func TestOpenAICNGetOpenAIProtocolAPIKeyBaseURLUnchanged(t *testing.T) {
	t.Parallel()

	// openai apikey：密钥直读 + 默认官方 base。
	openaiAcc := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key": "oa",
		},
	}
	require.Equal(t, "oa", openaiAcc.GetOpenAIProtocolAPIKey())
	require.Equal(t, "https://api.openai.com", openaiAcc.GetOpenAIBaseURL())

	// kimi apikey 无 base_url → 默认 payg base；有 base_url → 返回它。
	kimi := &Account{Platform: PlatformKimi, Type: AccountTypeAPIKey, Credentials: map[string]any{}}
	require.Equal(t, DefaultKimiPayGBaseURL, kimi.GetOpenAIBaseURL())
	kimiB := &Account{
		Platform: PlatformKimi,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://kimi.custom/v1",
		},
	}
	require.Equal(t, "https://kimi.custom/v1", kimiB.GetOpenAIBaseURL())

	// other apikey：密钥直读 + base_url 直读（与 muse 同源分支）。
	other := &Account{
		Platform: PlatformOther,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "o",
			"base_url": "https://other/v1",
		},
	}
	require.Equal(t, "o", other.GetOpenAIProtocolAPIKey())
	require.Equal(t, "https://other/v1", other.GetOpenAIBaseURL())

	// grok 仍不走共享 base_url 族：GetOpenAIBaseURL 必须返回 ""（muse-12 门条件
	// 由 !Shared && Platform!=Muse 构成，grok 仍被挡在门外）。
	grok := &Account{Platform: PlatformGrok, Type: AccountTypeOAuth, Credentials: map[string]any{}}
	require.Equal(t, "", grok.GetOpenAIBaseURL(), "grok 不得因 muse-12 改动获得 base_url")
}
