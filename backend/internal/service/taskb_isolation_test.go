package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Task B：平台归并重构隔离红线单测（docs/platform-merge-refactor-plan.md §2.4/§5.1/§6）。
// 钉住：判定源为 Account.IsWebAccessMode()（PR-4 旧链归零后平台值判定已退役）；web 账号
// 不被吸入 CN 余额/额度/403/429 冷却链；cookie 清洗豁免；凭证写入校验与模式切换重验。

// 1. cookie 清洗豁免：access_mode=web 账号 cookie 不被剥离；旧平台值仍豁免；api 账号被剥离。
func TestTaskBCookieSanitizeExemptsWebAccessMode(t *testing.T) {
	cookie := "sessionid=abc; chatglm_token=xyz"

	// 官方平台 + access_mode=web：cookie 豁免（按账号接入模式判定，PR-4 旧链归零）。
	merged := map[string]any{"cookie": cookie, "access_mode": AccountAccessModeWeb}
	require.Equal(t, cookie, SanitizeStoredCredentials(PlatformZhipu, merged)["cookie"],
		"official platform + access_mode=web must keep cookie")

	// 官方平台 + access_mode=api：cookie 被剥离（api 账号不持有 web cookie）。
	apiCreds := map[string]any{"cookie": cookie, "access_mode": AccountAccessModeAPI, "api_key": "sk-x"}
	require.NotContains(t, SanitizeStoredCredentials(PlatformZhipu, apiCreds), "cookie",
		"api access mode must strip cookie")

	// 官方平台 + 无 access_mode：cookie 被剥离（视为 api）。
	plain := map[string]any{"cookie": cookie}
	require.NotContains(t, SanitizeStoredCredentials(PlatformZhipu, plain), "cookie",
		"plain official platform must strip cookie")
}

// 2. validateWebAccountCredential：官方平台 + access_mode=web 组合生效；非法 access_mode 拒绝。
func TestTaskBValidateWebAccountCredentialOfficialWebAccessMode(t *testing.T) {
	// zhipu + web：要求非空 cookie。
	require.NoError(t, validateWebAccountCredential(PlatformZhipu, AccountTypeAPIKey,
		map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "c"}))
	require.Error(t, validateWebAccountCredential(PlatformZhipu, AccountTypeAPIKey,
		map[string]any{"access_mode": AccountAccessModeWeb}), "zhipu+web requires cookie")

	// kimi + web：要求非空 access_token。
	require.NoError(t, validateWebAccountCredential(PlatformKimi, AccountTypeAPIKey,
		map[string]any{"access_mode": AccountAccessModeWeb, "access_token": "at"}))
	require.Error(t, validateWebAccountCredential(PlatformKimi, AccountTypeAPIKey,
		map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "c"}), "kimi+web requires access_token")

	// deepseek + web：要求非空 cookie。
	require.NoError(t, validateWebAccountCredential(PlatformDeepseek, AccountTypeAPIKey,
		map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "c"}))

	// web 接入模式仅支持 apikey 类型。
	require.Error(t, validateWebAccountCredential(PlatformZhipu, AccountTypeOAuth,
		map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "c"}), "web requires apikey")

	// 非法 access_mode 显式值拒绝持久化。
	require.Error(t, validateWebAccountCredential(PlatformZhipu, AccountTypeAPIKey,
		map[string]any{"access_mode": "bogus", "api_key": "sk-x"}), "illegal access_mode rejected")

	// 官方平台无 access_mode / 非 web：不触发 web 校验，api_key 由调用方其它链路保证。
	require.NoError(t, validateWebAccountCredential(PlatformZhipu, AccountTypeAPIKey,
		map[string]any{"api_key": "sk-x"}))
}

// 3. 模式切换重验：web 要求 cookie/access_token 非空；api 要求 api_key 非空；缺失即拒绝。
func TestTaskBAccessModeSwitchRevalidation(t *testing.T) {
	// 切到 web：缺 cookie 拒绝。
	require.Error(t, validateAccessModeCredential(PlatformZhipu, AccountTypeAPIKey,
		map[string]any{"access_mode": AccountAccessModeWeb}), "switch to web without cookie rejected")
	require.NoError(t, validateAccessModeCredential(PlatformZhipu, AccountTypeAPIKey,
		map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "c"}), "switch to web with cookie ok")

	// 切到 web（kimi）：缺 access_token 拒绝。
	require.Error(t, validateAccessModeCredential(PlatformKimi, AccountTypeAPIKey,
		map[string]any{"access_mode": AccountAccessModeWeb}), "switch to web kimi without access_token rejected")

	// 切到 api：缺 api_key 拒绝。
	require.Error(t, validateAccessModeCredential(PlatformZhipu, AccountTypeAPIKey,
		map[string]any{"access_mode": AccountAccessModeAPI}), "switch to api without api_key rejected")
	require.NoError(t, validateAccessModeCredential(PlatformZhipu, AccountTypeAPIKey,
		map[string]any{"access_mode": AccountAccessModeAPI, "api_key": "sk-x"}), "switch to api with api_key ok")

	// 凭证形状隐式归属的兼容期已关闭（2026-09-19 用户裁定）：web 登录平台携带网页登录形状凭证
	// （cookie/access_token）但缺显式 access_mode → fail-closed 拒绝；api 形状凭证
	// 仍经 GetAccessMode 默认 "api"，不强制显式（既有官方平台 API 账号语义）。
	require.Error(t, validateAccessModeCredential(PlatformZhipu, AccountTypeAPIKey,
		map[string]any{"cookie": "c"}), "web-shaped credential without explicit access_mode rejected")
	require.Error(t, validateAccessModeCredential(PlatformKimi, AccountTypeAPIKey,
		map[string]any{"access_token": "at"}), "kimi web-shaped credential without explicit access_mode rejected")
	require.NoError(t, validateAccessModeCredential(PlatformZhipu, AccountTypeAPIKey,
		map[string]any{"api_key": "sk-x"}), "api-shaped credential without explicit access_mode not forced")
	require.NoError(t, validateAccessModeCredential(PlatformOpenAI, AccountTypeAPIKey,
		map[string]any{"cookie": "residue"}), "non-web platform unaffected")
}

// 3b. 凭证形状隐式归属兼容期关闭回归（创建链路）：web 形状凭证缺显式 access_mode 建号被拒；
// 显式 access_mode=web 正常；api 形状无显式 access_mode 不受影响。
func TestTaskBWebShapeCredentialCreateRejected(t *testing.T) {
	build := func(creds map[string]any) error {
		_, err := buildAccountForCreate(&CreateAccountInput{
			Name:        "shape-compat-closed",
			Platform:    PlatformZhipu,
			Type:        AccountTypeAPIKey,
			Credentials: creds,
		}, map[string]any{})
		return err
	}
	// cookie 非空但无 access_mode：拒绝。
	require.Error(t, build(map[string]any{"cookie": "c"}), "web-shaped create without access_mode rejected")
	// 显式 access_mode=web + cookie：正常。
	require.NoError(t, build(map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "c"}), "explicit web create ok")
	// api 形状无显式 access_mode：不受影响（GetAccessMode 默认 api）。
	require.NoError(t, build(map[string]any{"api_key": "sk-x", "base_url": "https://open.bigmodel.cn/api/paas/v4"}), "api-shaped create without access_mode ok")
}

// 4. web 接入模式账号不进 CN 余额/额度/403/429 冷却链。
func TestTaskBIsCNCoolingEligibleExcludesWeb(t *testing.T) {
	// 官方 zhipu + api：进入 CN 冷却链。
	apiZhipu := &Account{Platform: PlatformZhipu, Credentials: map[string]any{"access_mode": AccountAccessModeAPI, "api_key": "sk-x"}}
	require.True(t, isCNCoolingEligible(apiZhipu), "api zhipu enters CN cooling")

	// 官方 zhipu + web：排除在 CN 冷却链之外。
	webZhipu := &Account{Platform: PlatformZhipu, Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "c"}}
	require.False(t, isCNCoolingEligible(webZhipu), "web zhipu excluded from CN cooling")
	require.True(t, webZhipu.IsCNProvider(), "web zhipu is still a CN provider by platform")
	require.True(t, webZhipu.IsWebAccessMode(), "web zhipu is web access mode")

	// openai 不受影响。
	openaiAcct := &Account{Platform: PlatformOpenAI, Credentials: map[string]any{"api_key": "sk-x"}}
	require.False(t, isCNCoolingEligible(openaiAcct), "openai not CN cooling")
}

// 5. 探活分派按接入模式：web 接入模式账号走 web 探活；api 账号走 CN 探活。
func TestTaskBWebTestDispatchByAccessMode(t *testing.T) {
	webAcct := &Account{Platform: PlatformZhipu, Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "c"}}
	require.True(t, webAcct.IsWebAccessMode(), "web account dispatched to web probe")

	// ResolveWebPlatform 把官方平台 + web 模式归一到网页目录键（即官方平台值本身，PR-4）。
	require.Equal(t, PlatformZhipu, ResolveWebPlatform(webAcct), "official zhipu+web resolves to zhipu")
	require.Equal(t, PlatformKimi, ResolveWebPlatform(&Account{
		Platform: PlatformKimi, Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "access_token": "at"}}))
	require.Equal(t, PlatformDeepseek, ResolveWebPlatform(&Account{
		Platform: PlatformDeepseek, Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "c"}}))

	// api 账号不归一到 web 平台值。
	apiAcct := &Account{Platform: PlatformZhipu, Credentials: map[string]any{"access_mode": AccountAccessModeAPI, "api_key": "sk-x"}}
	require.False(t, apiAcct.IsWebAccessMode(), "api account not dispatched to web probe")
	require.Equal(t, "", ResolveWebPlatform(apiAcct), "api account does not resolve to web platform")
}

// 6. WebModelCatalogPlatform：支持网页登录的官方平台返回平台值本身（网页目录键）；其余为空。
func TestTaskBWebModelCatalogPlatform(t *testing.T) {
	require.Equal(t, PlatformZhipu, WebModelCatalogPlatform(PlatformZhipu))
	require.Equal(t, PlatformKimi, WebModelCatalogPlatform(PlatformKimi))
	require.Equal(t, PlatformDeepseek, WebModelCatalogPlatform(PlatformDeepseek))
	require.Equal(t, "", WebModelCatalogPlatform(PlatformOpenAI), "non-web platform returns empty")
}
