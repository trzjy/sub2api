package service

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// 纯委托红线：web 三平台续期实现各只有一处——kimi 在
// OpenAIGatewayService.refreshWebKimiAccessToken（web_kimi_gateway_forward.go）、
// zhipu 在 OpenAIGatewayService.refreshWebZhipuAccessToken、deepseek 在
// WebPlatformAutoLoginService.RecoverAccount（web_platform_auto_login.go）。
// 本文件只做调用与簿记，不含任何 HTTP 请求构造（无 http.NewRequest）。
//
// 三个类型实现 TokenRefresher 四件套（CanRefresh/NeedsRefresh/Refresh/CacheKey），
// 结构照抄 codebuddy_token_refresher.go；本文件不 import/注册 token_refresh_service
// 的注册表（那是波次2的事），先落地为独立类型 + 构造函数 + 单测。

// webKimiRefreshInterval 从配置取 kimi/zhipu 保活间隔（<=0 禁用语义由调用方判定）。
func webKimiRefreshInterval(cfg *config.TokenRefreshConfig) time.Duration {
	if cfg == nil {
		return 72 * time.Hour
	}
	return time.Duration(cfg.WebRefreshMinIntervalHours * float64(time.Hour))
}

func webReloginInterval(cfg *config.TokenRefreshConfig) time.Duration {
	if cfg == nil {
		return 168 * time.Hour
	}
	return time.Duration(cfg.WebReloginMinIntervalHours * float64(time.Hour))
}

// webRefreshDue 共用间隔判定：簿记键缺失或解析失败 → true（从未刷过/状态不可信，
// 保活兜底）；解析成功 → 距上次成功超过 minInterval 才需要刷。
func webRefreshDue(account *Account, minInterval time.Duration) bool {
	if minInterval <= 0 {
		return false
	}
	last := account.GetCredentialAsTime(CredKeyWebRefreshLastOKAt)
	if last == nil {
		return true
	}
	return time.Since(*last) > minInterval
}

// webProxyURL 取账号代理 URL（照抄 web_kimi_gateway_forward.go 的取法）：
// ProxyID 与 Proxy 都非空时返回 Proxy.URL()，否则空串。
func webProxyURL(account *Account) string {
	if account.ProxyID != nil && account.Proxy != nil {
		return account.Proxy.URL()
	}
	return ""
}

// mergeWebRefreshBookkeeping 就地合并后台保活簿记键：写 web_refresh_last_ok_at
// 为当前 UTC 时间（RFC3339）。仿 web_platform_auto_login.go mergeCredentials 的读改写风格。
func mergeWebRefreshBookkeeping(account *Account) {
	if account.Credentials == nil {
		account.Credentials = make(map[string]any)
	}
	account.Credentials[CredKeyWebRefreshLastOKAt] = time.Now().UTC().Format(time.RFC3339)
}

// cloneWebCredentials 浅拷贝账号凭据 map 返回（防调用方改动内部 map）。
func cloneWebCredentials(account *Account) map[string]any {
	out := make(map[string]any, len(account.Credentials))
	for k, v := range account.Credentials {
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------------------
// WebKimiTokenRefresher
// ---------------------------------------------------------------------------

// WebKimiTokenRefresher 委托 OpenAIGatewayService.refreshWebKimiAccessToken 完成
// kimi 网页账号的 access_token 后台续期。
type WebKimiTokenRefresher struct {
	gateway *OpenAIGatewayService
	cfg     *config.TokenRefreshConfig
}

func NewWebKimiTokenRefresher(gateway *OpenAIGatewayService, cfg *config.TokenRefreshConfig) *WebKimiTokenRefresher {
	return &WebKimiTokenRefresher{gateway: gateway, cfg: cfg}
}

// CacheKey 返回分布式锁缓存键。
func (r *WebKimiTokenRefresher) CacheKey(account *Account) string {
	return "webkimi:account:" + strconv.FormatInt(account.ID, 10)
}

// CanRefresh：kimi 平台 + web 接入 + refresh_token 非空。
func (r *WebKimiTokenRefresher) CanRefresh(account *Account) bool {
	return account.Platform == PlatformKimi &&
		account.IsWebAccessMode() &&
		account.GetCredential("refresh_token") != ""
}

// NeedsRefresh：距上次后台续期成功超过保活间隔（缺簿记键即视为需要）。
func (r *WebKimiTokenRefresher) NeedsRefresh(account *Account, _ time.Duration) bool {
	if !r.CanRefresh(account) {
		return false
	}
	return webRefreshDue(account, webKimiRefreshInterval(r.cfg))
}

// Refresh 执行 kimi 网页续期（纯委托）。
func (r *WebKimiTokenRefresher) Refresh(ctx context.Context, account *Account) (map[string]any, error) {
	if got := r.gateway.refreshWebKimiAccessToken(ctx, account, webProxyURL(account)); got == "" {
		return nil, errors.New("kimi web refresh failed")
	}
	mergeWebRefreshBookkeeping(account)
	return cloneWebCredentials(account), nil
}

// ---------------------------------------------------------------------------
// WebZhipuTokenRefresher
// ---------------------------------------------------------------------------

// WebZhipuTokenRefresher 委托 OpenAIGatewayService.refreshWebZhipuAccessToken 完成
// zhipu 网页账号的 chatglm_refresh_token 静默续期。
type WebZhipuTokenRefresher struct {
	gateway *OpenAIGatewayService
	cfg     *config.TokenRefreshConfig
}

func NewWebZhipuTokenRefresher(gateway *OpenAIGatewayService, cfg *config.TokenRefreshConfig) *WebZhipuTokenRefresher {
	return &WebZhipuTokenRefresher{gateway: gateway, cfg: cfg}
}

// CacheKey 返回分布式锁缓存键。
func (r *WebZhipuTokenRefresher) CacheKey(account *Account) string {
	return "webzhipu:account:" + strconv.FormatInt(account.ID, 10)
}

// CanRefresh：zhipu 平台 + web 接入 +（cookie 含 chatglm_refresh_token= 或显式 refresh_token 非空）。
func (r *WebZhipuTokenRefresher) CanRefresh(account *Account) bool {
	if account.Platform != PlatformZhipu || !account.IsWebAccessMode() {
		return false
	}
	if account.GetCredential("refresh_token") != "" {
		return true
	}
	cookie := account.GetCredential("cookie")
	return webZhipuExtractCookieField(cookie, "chatglm_refresh_token") != ""
}

// NeedsRefresh：距上次后台续期成功超过保活间隔（缺簿记键即视为需要）。
func (r *WebZhipuTokenRefresher) NeedsRefresh(account *Account, _ time.Duration) bool {
	if !r.CanRefresh(account) {
		return false
	}
	return webRefreshDue(account, webKimiRefreshInterval(r.cfg))
}

// Refresh 执行 zhipu 网页续期（纯委托）。
func (r *WebZhipuTokenRefresher) Refresh(ctx context.Context, account *Account) (map[string]any, error) {
	if got := r.gateway.refreshWebZhipuAccessToken(ctx, account); got == "" {
		return nil, errors.New("zhipu web refresh failed")
	}
	mergeWebRefreshBookkeeping(account)
	return cloneWebCredentials(account), nil
}

// ---------------------------------------------------------------------------
// WebDeepseekTokenRefresher
// ---------------------------------------------------------------------------

// WebDeepseekTokenRefresher 委托 WebPlatformAutoLoginService.RecoverAccount 完成
// deepseek 网页账号的后台密码无头重登。
type WebDeepseekTokenRefresher struct {
	autoLogin *WebPlatformAutoLoginService
	cfg       *config.TokenRefreshConfig

	// recoverFn 可注入的恢复函数（默认 = autoLogin.RecoverAccount，仅测试注入 fake）。
	recoverFn func(ctx context.Context, account *Account) WebRecoverResult
}

func NewWebDeepseekTokenRefresher(autoLogin *WebPlatformAutoLoginService, cfg *config.TokenRefreshConfig) *WebDeepseekTokenRefresher {
	return &WebDeepseekTokenRefresher{
		autoLogin: autoLogin,
		cfg:       cfg,
		recoverFn: autoLogin.RecoverAccount,
	}
}

// CacheKey 返回分布式锁缓存键。
func (r *WebDeepseekTokenRefresher) CacheKey(account *Account) string {
	return "webdeepseek:account:" + strconv.FormatInt(account.ID, 10)
}

// CanRefresh：deepseek 平台 + web 接入 + login_email / login_password 均非空。
func (r *WebDeepseekTokenRefresher) CanRefresh(account *Account) bool {
	return account.Platform == PlatformDeepseek &&
		account.IsWebAccessMode() &&
		account.GetCredential(CredKeyLoginEmail) != "" &&
		account.GetCredential(CredKeyLoginPassword) != ""
}

// NeedsRefresh：先判 login_non_retryable（banned/密码错等不可重试，防撞登录端点），
// 再按"距上次重登成功超过间隔"判定。
func (r *WebDeepseekTokenRefresher) NeedsRefresh(account *Account, _ time.Duration) bool {
	if !r.CanRefresh(account) {
		return false
	}
	if account.GetCredential(CredKeyLoginNonRetryable) == "true" {
		return false
	}
	return webRefreshDue(account, webReloginInterval(r.cfg))
}

// Refresh 执行 deepseek 后台重登（纯委托）。
func (r *WebDeepseekTokenRefresher) Refresh(ctx context.Context, account *Account) (map[string]any, error) {
	result := r.recoverFn(ctx, account)
	if !result.Recovered {
		return nil, errors.New(result.Detail)
	}
	mergeWebRefreshBookkeeping(account)
	return cloneWebCredentials(account), nil
}
