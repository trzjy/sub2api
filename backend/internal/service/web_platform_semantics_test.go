package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestValidateWebAccountCredential_BaseURL 覆盖 #4：validateWebAccountCredential 在
// credentials.base_url 非空时必须校验官方域名后缀（走既有 ValidateWebBaseURL），
// 非法值（明文 http / 非官方主机 / 内网 IP）保存即拒绝；合法官方域名或空值放行。
// 不影响其它 / 通用平台分支（validateOtherAccountCredential）的既有逻辑。
func TestValidateWebAccountCredential_BaseURL(t *testing.T) {
	// 合法官方域名覆盖：放行（不变量：仅校验形态，不校验可达性）。
	require.NoError(t, validateWebAccountCredential(PlatformWebDeepseek, AccountTypeAPIKey,
		map[string]any{"cookie": "sessionid=abc", "base_url": "https://chat.deepseek.com"}))
	require.NoError(t, validateWebAccountCredential(PlatformWebZhipu, AccountTypeAPIKey,
		map[string]any{"cookie": "x", "base_url": "https://chatglm.cn"}))
	require.NoError(t, validateWebAccountCredential(PlatformWebKimi, AccountTypeAPIKey,
		map[string]any{"access_token": "at", "base_url": "https://www.kimi.com"}))

	// 空 base_url：回落平台默认，放行。
	require.NoError(t, validateWebAccountCredential(PlatformWebDeepseek, AccountTypeAPIKey,
		map[string]any{"cookie": "sessionid=abc", "base_url": ""}))
	require.NoError(t, validateWebAccountCredential(PlatformWebKimi, AccountTypeAPIKey,
		map[string]any{"access_token": "at"}))

	// 非法：明文 http → 拒绝（fail-closed 拦截脏数据）。
	require.Error(t, validateWebAccountCredential(PlatformWebDeepseek, AccountTypeAPIKey,
		map[string]any{"cookie": "sessionid=abc", "base_url": "http://chat.deepseek.com"}))

	// 非法：非官方主机 → 拒绝（杜绝凭证外送 / SSRF）。
	require.Error(t, validateWebAccountCredential(PlatformWebZhipu, AccountTypeAPIKey,
		map[string]any{"cookie": "x", "base_url": "https://evil.example.com"}))
	require.Error(t, validateWebAccountCredential(PlatformWebKimi, AccountTypeAPIKey,
		map[string]any{"access_token": "at", "base_url": "https://not-kimi.com"}))

	// 非法：内网 / 环回 IP 字面量 → 拒绝（SSRF 防护）。
	require.Error(t, validateWebAccountCredential(PlatformWebKimi, AccountTypeAPIKey,
		map[string]any{"access_token": "at", "base_url": "https://127.0.0.1"}))

	// 非法 base_url 不能绕过 cookie / access_token 必填校验：错误文案须指向 base_url。
	err := validateWebAccountCredential(PlatformWebDeepseek, AccountTypeAPIKey,
		map[string]any{"cookie": "sessionid=abc", "base_url": "https://evil.example.com"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "base_url")
}

// TestTestWebAccountConnection_InvalidBaseURLErrors 覆盖 #4：testWebAccountConnection 在
// 账号 base_url 非法时显式报错（"invalid base_url"），不静默回落平台默认假通过；上游
// 即便返回 2xx 也不应被探活（错误校验在出站请求之前）。
func TestTestWebAccountConnection_InvalidBaseURLErrors(t *testing.T) {
	account := webDeepseekTestAccount(9930, map[string]any{
		"cookie":   "ds_session_id=sess-abc; HWWAFSESID=waf-xyz",
		"base_url": "https://evil.example.com",
	})
	// 即便上游返回 200，因 base_url 非法，不得判为 test_complete / success。
	body := runWebProbe(t, account, refreshResponse(http.StatusOK, "ok"))
	require.Contains(t, body, "invalid base_url")
	require.NotContains(t, body, "test_complete", "illegal base_url must NOT false-pass the connection test")
	require.NotContains(t, body, "success")

	// 对照组：合法 base_url（含上游 200）正常判为成功。
	good := webDeepseekTestAccount(9931, map[string]any{
		"cookie":   "ds_session_id=sess-abc; HWWAFSESID=waf-xyz",
		"base_url": "https://chat.deepseek.com",
	})
	goodBody := runWebProbe(t, good, refreshResponse(http.StatusOK, "ok"))
	require.Contains(t, goodBody, "test_complete")
	require.Contains(t, goodBody, `"success":true`)
}

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
