package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestIsWebProviderCoversWebReversePlatforms 锁定网页逆向平台集合判定（方案 §3.1）。
func TestIsWebProviderCoversWebReversePlatforms(t *testing.T) {
	for _, p := range []string{PlatformWebDeepseek, PlatformWebZhipu, PlatformWebKimi} {
		require.Truef(t, IsWebProvider(p), "platform %q should be a web provider", p)
	}
	for _, p := range []string{PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformOpenAI, PlatformOther, PlatformComposite, "", "web-other"} {
		require.Falsef(t, IsWebProvider(p), "platform %q must NOT be a web provider", p)
	}
}

// TestWebProviderExcludedFromOpenAICompatFamily 锁定网页平台不使用共享 OpenAI
// base_url 族与用户×平台额度白名单（不从 API 平台复制额度语义——方案 §0
// forbidden_inferences）。调度族成员资格见 account.go IsOpenAICompatible（W6 已含 web，
// 经 web-* 专用适配器转发），共享 base_url 与额度语义仍排除。
func TestWebProviderExcludedFromOpenAICompatFamily(t *testing.T) {
	for _, p := range []string{PlatformWebDeepseek, PlatformWebZhipu, PlatformWebKimi} {
		require.Falsef(t, UsesOpenAIProtocolSharedBaseURL(p), "platform %q must NOT use shared OpenAI base", p)
		require.Falsef(t, IsAllowedQuotaPlatform(p), "platform %q must NOT allow user quota", p)
	}
}

// TestValidateWebAccountCredential 锁定网页平台建号不变量：仅 apikey 类型；
// web-deepseek / web-zhipu 须有非空整串 cookie；web-kimi 须有非空 access_token；
// base_url 可选。字段口径见 docs/web-reverse-embedded-login-plan.md §3.2。
func TestValidateWebAccountCredential(t *testing.T) {
	// 非网页平台不受影响。
	require.NoError(t, validateWebAccountCredential(PlatformOpenAI, AccountTypeOAuth, nil))

	// 仅接受 apikey 类型。
	for _, p := range []string{PlatformWebDeepseek, PlatformWebZhipu, PlatformWebKimi} {
		require.Error(t, validateWebAccountCredential(p, AccountTypeOAuth, map[string]any{"cookie": "c"}), p)
	}

	// cookie 平台：非空整串 cookie 必填。
	require.Error(t, validateWebAccountCredential(PlatformWebDeepseek, AccountTypeAPIKey, nil))
	require.Error(t, validateWebAccountCredential(PlatformWebDeepseek, AccountTypeAPIKey, map[string]any{}))
	require.Error(t, validateWebAccountCredential(PlatformWebDeepseek, AccountTypeAPIKey, map[string]any{"cookie": ""}))
	require.Error(t, validateWebAccountCredential(PlatformWebDeepseek, AccountTypeAPIKey, map[string]any{"cookie": "   "}))
	require.Error(t, validateWebAccountCredential(PlatformWebZhipu, AccountTypeAPIKey, map[string]any{"cookie": nil}))
	require.NoError(t, validateWebAccountCredential(PlatformWebDeepseek, AccountTypeAPIKey, map[string]any{"cookie": "sessionid=abc"}))
	require.NoError(t, validateWebAccountCredential(PlatformWebZhipu, AccountTypeAPIKey, map[string]any{"cookie": "chatglm_token=x", "base_url": "https://chatglm.cn"}))

	// kimi 平台：access_token 必填，refresh_token / user_id / base_url 可选。
	require.Error(t, validateWebAccountCredential(PlatformWebKimi, AccountTypeAPIKey, map[string]any{}))
	require.Error(t, validateWebAccountCredential(PlatformWebKimi, AccountTypeAPIKey, map[string]any{"access_token": ""}))
	require.Error(t, validateWebAccountCredential(PlatformWebKimi, AccountTypeAPIKey, map[string]any{"refresh_token": "rt"}))
	require.NoError(t, validateWebAccountCredential(PlatformWebKimi, AccountTypeAPIKey, map[string]any{"access_token": "at"}))
	require.NoError(t, validateWebAccountCredential(PlatformWebKimi, AccountTypeAPIKey, map[string]any{"access_token": "at", "refresh_token": "rt", "user_id": "u-1"}))
}

// TestSanitizeStoredCredentials_KeepsCookieForWebProviders 锁定清洗链路的网页平台例外：
// cookie 就是网页平台的登录态凭证本身，显式平台标签下必须保留；空平台标签
// （批量路径）仍剥离，防止 session-jar 残留落盘。
func TestSanitizeStoredCredentials_KeepsCookieForWebProviders(t *testing.T) {
	for _, platform := range []string{PlatformWebDeepseek, PlatformWebZhipu, PlatformWebKimi} {
		creds := map[string]any{
			"cookie":       "session",
			"access_token": "at",
			"password":     "x",
			"sso_token":    "sso",
		}
		out := SanitizeStoredCredentials(platform, creds)
		require.Equalf(t, "session", out["cookie"], "web platform %q must keep cookie", platform)
		require.Equalf(t, "at", out["access_token"], platform)
		require.NotContainsf(t, out, "password", platform)
		require.NotContainsf(t, out, "sso_token", platform)
	}

	// 空平台标签（批量路径）与 API 平台仍剥离 cookie。
	for _, platform := range []string{"", PlatformOpenAI, PlatformGrok} {
		creds := map[string]any{"cookie": "jar", "api_key": "k"}
		out := SanitizeStoredCredentials(platform, creds)
		require.NotContainsf(t, out, "cookie", platform)
		require.Equalf(t, "k", out["api_key"], platform)
	}
}
