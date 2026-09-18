package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestValidateWebAccountCredential_BaseURL 覆盖 #4：validateWebAccountCredential 在
// credentials.base_url 非空时必须校验官方域名后缀（走既有 ValidateWebBaseURL），
// 非法值（明文 http / 非官方主机 / 内网 IP）保存即拒绝；合法官方域名或空值放行。
// 归并后网页接入由官方平台账号级 access_mode=web 承载（credentials 须显式带
// access_mode="web" 方进入网页校验分支）。
func TestValidateWebAccountCredential_BaseURL(t *testing.T) {
	// 合法官方域名覆盖：放行（不变量：仅校验形态，不校验可达性）。
	require.NoError(t, validateWebAccountCredential(PlatformDeepseek, AccountTypeAPIKey,
		map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "sessionid=abc", "base_url": "https://chat.deepseek.com"}))
	require.NoError(t, validateWebAccountCredential(PlatformZhipu, AccountTypeAPIKey,
		map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "x", "base_url": "https://chatglm.cn"}))
	require.NoError(t, validateWebAccountCredential(PlatformKimi, AccountTypeAPIKey,
		map[string]any{"access_mode": AccountAccessModeWeb, "access_token": "at", "base_url": "https://www.kimi.com"}))

	// 空 base_url：回落平台默认，放行。
	require.NoError(t, validateWebAccountCredential(PlatformDeepseek, AccountTypeAPIKey,
		map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "sessionid=abc", "base_url": ""}))
	require.NoError(t, validateWebAccountCredential(PlatformKimi, AccountTypeAPIKey,
		map[string]any{"access_mode": AccountAccessModeWeb, "access_token": "at"}))

	// 非法：明文 http → 拒绝（fail-closed 拦截脏数据）。
	require.Error(t, validateWebAccountCredential(PlatformDeepseek, AccountTypeAPIKey,
		map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "sessionid=abc", "base_url": "http://chat.deepseek.com"}))

	// 非法：非官方主机 → 拒绝（杜绝凭证外送 / SSRF）。
	require.Error(t, validateWebAccountCredential(PlatformZhipu, AccountTypeAPIKey,
		map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "x", "base_url": "https://evil.example.com"}))
	require.Error(t, validateWebAccountCredential(PlatformKimi, AccountTypeAPIKey,
		map[string]any{"access_mode": AccountAccessModeWeb, "access_token": "at", "base_url": "https://not-kimi.com"}))

	// 非法：内网 / 环回 IP 字面量 → 拒绝（SSRF 防护）。
	require.Error(t, validateWebAccountCredential(PlatformKimi, AccountTypeAPIKey,
		map[string]any{"access_mode": AccountAccessModeWeb, "access_token": "at", "base_url": "https://127.0.0.1"}))

	// 非法 base_url 不能绕过 cookie / access_token 必填校验：错误文案须指向 base_url。
	err := validateWebAccountCredential(PlatformDeepseek, AccountTypeAPIKey,
		map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "sessionid=abc", "base_url": "https://evil.example.com"})
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

	// 对照组：合法 base_url（含上游 200）正常判为成功（新协议三跳：PoW → 建会话 → completion）。
	good := webDeepseekTestAccount(9931, map[string]any{
		"cookie":   "ds_session_id=sess-abc; HWWAFSESID=waf-xyz",
		"base_url": "https://chat.deepseek.com",
	})
	goodBody := runWebProbeWithResponses(t, good,
		webDeepseekSolvablePowChallengeResponse(),
		webDeepseekSessionCreateResponse(),
		refreshResponse(http.StatusOK, "ok"),
	)
	require.Contains(t, goodBody, "test_complete")
	require.Contains(t, goodBody, `"success":true`)
}

// TestValidateWebAccountCredential_BaseURLNonStringRejected 覆盖 #4 回归：base_url 键存在
// 但类型非 string（数字 / 数组 / 对象）时，validateWebAccountCredential 必须 fail-closed
// 拒绝（"base_url must be a string"），不再因类型断言失败静默跳过保存脏数据。
//   - base_url=12345 / []string / map → 拒绝；
//   - base_url 合法 https 官方域名 / 空串 / 缺失 → 仍放行；
//   - 其余必填字段（cookie / access_token）已满足，确保只校验 base_url 类型。
func TestValidateWebAccountCredential_BaseURLNonStringRejected(t *testing.T) {
	// 数字 base_url → 拒绝。
	require.Error(t, validateWebAccountCredential(PlatformDeepseek, AccountTypeAPIKey,
		map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "sessionid=abc", "base_url": 12345}))
	require.Error(t, validateWebAccountCredential(PlatformKimi, AccountTypeAPIKey,
		map[string]any{"access_mode": AccountAccessModeWeb, "access_token": "at", "base_url": 12345}))

	// 数组 base_url → 拒绝。
	require.Error(t, validateWebAccountCredential(PlatformZhipu, AccountTypeAPIKey,
		map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "x", "base_url": []string{"https://chatglm.cn"}}))

	// 对象 base_url → 拒绝。
	require.Error(t, validateWebAccountCredential(PlatformDeepseek, AccountTypeAPIKey,
		map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "sessionid=abc", "base_url": map[string]any{"url": "https://chat.deepseek.com"}}))

	// nil base_url → 拒绝（类型断言失败，非空串语义）。
	require.Error(t, validateWebAccountCredential(PlatformKimi, AccountTypeAPIKey,
		map[string]any{"access_mode": AccountAccessModeWeb, "access_token": "at", "base_url": nil}))

	// 错误文案须指向 base_url 且不得含任何凭证值。
	err := validateWebAccountCredential(PlatformDeepseek, AccountTypeAPIKey,
		map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "sessionid=abc", "base_url": 12345})
	require.Error(t, err)
	require.Contains(t, err.Error(), "base_url must be a string")
	require.NotContains(t, err.Error(), "sessionid=abc")

	// 对照：合法 https 官方域名 / 空串 / 缺失 → 仍放行。
	require.NoError(t, validateWebAccountCredential(PlatformKimi, AccountTypeAPIKey,
		map[string]any{"access_mode": AccountAccessModeWeb, "access_token": "at", "base_url": "https://www.kimi.com"}))
	require.NoError(t, validateWebAccountCredential(PlatformDeepseek, AccountTypeAPIKey,
		map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "sessionid=abc", "base_url": ""}))
	require.NoError(t, validateWebAccountCredential(PlatformZhipu, AccountTypeAPIKey,
		map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "x"}))
}

// TestIsWebLoginPlatformCoversWebReversePlatforms 锁定网页登录平台集合判定（方案 §5.5）。
func TestIsWebLoginPlatformCoversWebReversePlatforms(t *testing.T) {
	for _, p := range []string{PlatformZhipu, PlatformDeepseek, PlatformKimi} {
		require.Truef(t, IsWebLoginPlatform(p), "platform %q should be a web login platform", p)
	}
	for _, p := range []string{PlatformOpenAI, PlatformOther, PlatformComposite, "", "web-other"} {
		require.Falsef(t, IsWebLoginPlatform(p), "platform %q must NOT be a web login platform", p)
	}
}

// TestWebLoginPlatformInCNProviderFamily 锁定网页登录平台（官方 zhipu/deepseek/kimi）归并后
// 作为国产 CN 供应商，复用共享 OpenAI 兼容 base_url 族并纳入用户×平台额度白名单
// （方案 §5.5：网页接入由官方平台 access_mode=web 承载，不再有独立 web-* 平台值；
// 旧「web 平台排除共享 base_url / 额度」契约随平台归并撤销）。
func TestWebLoginPlatformInCNProviderFamily(t *testing.T) {
	for _, p := range []string{PlatformZhipu, PlatformDeepseek, PlatformKimi} {
		require.Truef(t, UsesOpenAIProtocolSharedBaseURL(p), "web login platform %q must use shared OpenAI base", p)
		require.Truef(t, IsAllowedQuotaPlatform(p), "web login platform %q must allow user quota", p)
	}
}

// TestValidateWebAccountCredential 锁定网页平台建号不变量：仅 apikey 类型；
// deepseek / zhipu 须有非空整串 cookie；kimi 须有非空 access_token；
// base_url 可选。字段口径见 docs/web-reverse-embedded-login-plan.md §3.2。
func TestValidateWebAccountCredential(t *testing.T) {
	// 非网页平台不受影响。
	require.NoError(t, validateWebAccountCredential(PlatformOpenAI, AccountTypeOAuth, nil))

	// 仅接受 apikey 类型。
	for _, p := range []string{PlatformDeepseek, PlatformZhipu, PlatformKimi} {
		require.Error(t, validateWebAccountCredential(p, AccountTypeOAuth, map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "c"}), p)
	}

	// cookie 平台：非空整串 cookie 必填。
	require.Error(t, validateWebAccountCredential(PlatformDeepseek, AccountTypeAPIKey, map[string]any{"access_mode": AccountAccessModeWeb}))
	require.Error(t, validateWebAccountCredential(PlatformDeepseek, AccountTypeAPIKey, map[string]any{"access_mode": AccountAccessModeWeb, "cookie": ""}))
	require.Error(t, validateWebAccountCredential(PlatformDeepseek, AccountTypeAPIKey, map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "   "}))
	require.Error(t, validateWebAccountCredential(PlatformZhipu, AccountTypeAPIKey, map[string]any{"access_mode": AccountAccessModeWeb, "cookie": nil}))
	require.NoError(t, validateWebAccountCredential(PlatformDeepseek, AccountTypeAPIKey, map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "sessionid=abc"}))
	require.NoError(t, validateWebAccountCredential(PlatformZhipu, AccountTypeAPIKey, map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "chatglm_token=x", "base_url": "https://chatglm.cn"}))

	// kimi 平台：access_token 必填，refresh_token / user_id / base_url 可选。
	require.Error(t, validateWebAccountCredential(PlatformKimi, AccountTypeAPIKey, map[string]any{"access_mode": AccountAccessModeWeb}))
	require.Error(t, validateWebAccountCredential(PlatformKimi, AccountTypeAPIKey, map[string]any{"access_mode": AccountAccessModeWeb, "access_token": ""}))
	require.Error(t, validateWebAccountCredential(PlatformKimi, AccountTypeAPIKey, map[string]any{"access_mode": AccountAccessModeWeb, "refresh_token": "rt"}))
	require.NoError(t, validateWebAccountCredential(PlatformKimi, AccountTypeAPIKey, map[string]any{"access_mode": AccountAccessModeWeb, "access_token": "at"}))
	require.NoError(t, validateWebAccountCredential(PlatformKimi, AccountTypeAPIKey, map[string]any{"access_mode": AccountAccessModeWeb, "access_token": "at", "refresh_token": "rt", "user_id": "u-1"}))
}

// TestSanitizeStoredCredentials_KeepsCookieForWebProviders 锁定清洗链路的网页接入账号例外：
// cookie 就是网页接入账号的登录态凭证本身，账号级 access_mode=web 下必须保留；空平台标签
// （批量路径）仍剥离，防止 session-jar 残留落盘。
func TestSanitizeStoredCredentials_KeepsCookieForWebProviders(t *testing.T) {
	for _, platform := range []string{PlatformZhipu, PlatformDeepseek, PlatformKimi} {
		creds := map[string]any{
			"access_mode":  AccountAccessModeWeb,
			"cookie":       "session",
			"access_token": "at",
			"password":     "x",
			"sso_token":    "sso",
		}
		out := SanitizeStoredCredentials(platform, creds)
		require.Equalf(t, "session", out["cookie"], "web access mode %q must keep cookie", platform)
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
