package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
)

// WebPlatformAutoLoginService 实现三官方平台（zhipu / deepseek / kimi）网页接入统一
// 自动登录内核 + 错误细化。
//
// 设计要点（与仓库既有约定一致）：
//   - 出站请求统一经注入的 HTTPUpstream 接口；目标 URL 经 config.Config.Security.
//     URLAllowlist 校验（复用 urlvalidator，与 account_test_service.go 的
//     validateUpstreamBaseURL 同口径）。
//   - 凭据全部落在既有 Account.Credentials map（不新增 DB 列）；更新采用「读-改-写」原子
//     合并，保留 cookie / access_token / model_mapping 等既有键，只更新 login_* 与业务键。
//   - 安全红线：日志与错误信息绝不出现 Cookie / token / 密码值。
type WebPlatformAutoLoginService struct {
	store        AutoLoginAccountStore
	httpUpstream HTTPUpstream
	cfg          *config.Config
	logger       *slog.Logger
}

// AutoLoginAccountStore 是自动登录服务所需的账号持久化能力（由 handler / 仓储层实现）。
// 仅暴露自动登录所需的三个原子操作，避免与既有 service 内部接口耦合。
type AutoLoginAccountStore interface {
	ListAccounts(ctx context.Context) ([]*Account, error)
	UpdateAccountCredentials(ctx context.Context, id int64, creds map[string]any) error
	UpdateAccountStatus(ctx context.Context, id int64, status string, errMsg string) error
}

// NewWebPlatformAutoLoginService 构造自动登录服务。
// store 提供账号列举与凭据/状态原子更新；upstream 为注入的出站 HTTP 接口；
// cfg 用于 URL 白名单校验（传 nil 时跳过校验，仅用于测试）。
func NewWebPlatformAutoLoginService(store AutoLoginAccountStore, upstream HTTPUpstream, cfg *config.Config) *WebPlatformAutoLoginService {
	return &WebPlatformAutoLoginService{
		store:        store,
		httpUpstream: upstream,
		cfg:          cfg,
		logger:       slog.Default(),
	}
}

// ---------------------------------------------------------------------------
// 内部常量与类型
// ---------------------------------------------------------------------------

// webDeepseekLoginUA 与转发链（web_deepseek_gateway_forward.go 的 webDeepseekClientUA）
// 保持一致，避免上游按 UA 风控。此处就近定义一份同值常量，避免跨文件符号依赖。
const webDeepseekLoginUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

// webKimiAuthBaseURL 是 kimi refresh 的真实认证域（与 www.kimi.com 网页域不同）。
const webKimiAuthBaseURL = "https://auth.kimi.com"

// webLoginHTTPError 携带可机读的错误码/类别，供 RecoverAccount 做可重试性分类。
// 其 Error() 文案只含码与摘要，绝不携带 Cookie/密码/响应体。
type webLoginHTTPError struct {
	Platform string
	Code     int64
	Kind     string // WebLoginKind*
	Msg      string
}

func (e *webLoginHTTPError) Error() string { return e.Msg }

// WebLoginErrorKind 提取 *webLoginHTTPError 的类别与业务码；非该类型返回 ok=false。
// 供 handler 把失败关闭错误映射为 needs_challenge（Kind==WebLoginKindWAF）与细化文案。
func WebLoginErrorKind(err error) (kind string, code int64, ok bool) {
	var le *webLoginHTTPError
	if errors.As(err, &le) {
		return le.Kind, le.Code, true
	}
	return "", 0, false
}

// isWebLoginWAFError 报告 err 是否为 WAF/PoW 类失败（Kind==WebLoginKindWAF）。
func isWebLoginWAFError(err error) bool {
	kind, _, ok := WebLoginErrorKind(err)
	return ok && kind == WebLoginKindWAF
}

// webLoginErrorClass 标记一次失败的可重试性。
type webLoginErrorClass int

const (
	webLoginRetryable    webLoginErrorClass = iota // 可自动重试
	webLoginNonRetryable                           // 彻底不可重试（banned/密码错/WAF/PoW）
)

// webLoginErrorInfo 描述一次登录/续期失败的细化分类与可重试性。
type webLoginErrorInfo struct {
	Title        string
	Hint         string
	Retryable    bool
	NonRetryable bool
}

// ---------------------------------------------------------------------------
// 出站请求辅助
// ---------------------------------------------------------------------------

// validateUpstreamURL 依据 config.Security.URLAllowlist 校验出站 URL（与
// account_test_service.validateUpstreamBaseURL 同口径）。
func (s *WebPlatformAutoLoginService) validateUpstreamURL(raw string) (string, error) {
	if s.cfg == nil {
		return raw, nil
	}
	if !s.cfg.Security.URLAllowlist.Enabled {
		return urlvalidator.ValidateURLFormat(raw, s.cfg.Security.URLAllowlist.AllowInsecureHTTP)
	}
	return urlvalidator.ValidateHTTPSURL(raw, urlvalidator.ValidationOptions{
		AllowedHosts:     s.cfg.Security.URLAllowlist.UpstreamHosts,
		RequireAllowlist: true,
		AllowPrivate:     s.cfg.Security.URLAllowlist.AllowPrivateHosts,
	})
}

// mergeCredentials 读-改-写原子合并：复制既有 Credentials 后叠加 updates，绝不丢既有键。
func (s *WebPlatformAutoLoginService) mergeCredentials(account *Account, updates map[string]any) map[string]any {
	merged := make(map[string]any, len(account.Credentials)+len(updates))
	for k, v := range account.Credentials {
		merged[k] = v
	}
	for k, v := range updates {
		merged[k] = v
	}
	if strings.TrimSpace(fmt.Sprint(merged[CredKeyLoginDeviceID])) != "" {
		delete(merged, "device_id")
	}
	return merged
}

// combineSetCookies 把响应中多个 Set-Cookie 头合并为「name=value」对（取首段，去属性），
// 以分号连接成整串 Cookie 头。绝不保留 Path/Domain/Expires 等属性，避免把控制属性当凭据。
func combineSetCookies(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	var parts []string
	for _, sc := range resp.Header.Values("Set-Cookie") {
		pair := sc
		if i := strings.Index(sc, ";"); i >= 0 {
			pair = sc[:i]
		}
		pair = strings.TrimSpace(pair)
		if pair != "" {
			parts = append(parts, pair)
		}
	}
	return strings.Join(parts, "; ")
}

// ---------------------------------------------------------------------------
// deepseek：无头密码登录
// ---------------------------------------------------------------------------

type webDeepseekLoginRequest struct {
	Email    string `json:"email"`
	Mobile   string `json:"mobile"`
	Password string `json:"password"`
	AreaCode string `json:"area_code"`
	DeviceID string `json:"device_id"`
	OS       string `json:"os"`
}

type webDeepseekLoginResponse struct {
	Code int64  `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		BizCode int64  `json:"biz_code"`
		BizMsg  string `json:"biz_msg"`
	} `json:"data"`
}

// LoginByEmail 执行 deepseek web 无头密码登录。成功返回整串 Cookie 头（由调用方写入
// 凭据 cookie 键），失败返回 *webLoginHTTPError（含业务码供分类）。本方法不落库，
// 落库由 RecoverAccount 统一编排，保证状态与凭据一致。
//
// deepseek 官方网页端无密码登录之外的挑战关卡（证据：login 接口无 captcha 字段），
// 故遇 WAF（HTTP 403）直接失败关闭，绝不调用外部打码助手、绝不伪造成功。
func (s *WebPlatformAutoLoginService) LoginByEmail(ctx context.Context, account *Account) (string, error) {
	return s.loginByEmailRaw(ctx, account)
}

// loginByEmailRaw 执行 deepseek web 无头密码登录的原始请求。
func (s *WebPlatformAutoLoginService) loginByEmailRaw(ctx context.Context, account *Account) (string, error) {
	base := strings.TrimRight(account.GetWebBaseURL(), "/")
	if base == "" {
		return "", &webLoginHTTPError{
			Platform: PlatformDeepseek, Code: -1, Kind: WebLoginKindLogin,
			Msg: "deepseek web 账号缺少可用的 web base_url（不可重试）",
		}
	}
	email := strings.TrimSpace(account.GetCredential(CredKeyLoginEmail))
	password := account.GetCredential(CredKeyLoginPassword)
	if email == "" || password == "" {
		return "", &webLoginHTTPError{
			Platform: PlatformDeepseek, Code: -1, Kind: WebLoginKindLogin,
			Msg: "缺少 login_email 或 login_password 凭据（不可重试）",
		}
	}

	// device_id：稳定 UUID，首次生成后复用（内存回写，后续由合并写回凭据）。
	deviceID := strings.TrimSpace(account.GetCredential(CredKeyLoginDeviceID))
	if deviceID == "" {
		deviceID = uuid.New().String()
		if account.Credentials == nil {
			account.Credentials = map[string]any{}
		}
		account.Credentials[CredKeyLoginDeviceID] = deviceID
	}

	target := base + WebDeepseekLoginEndpoint
	if _, err := s.validateUpstreamURL(target); err != nil {
		return "", fmt.Errorf("deepseek web 登录目标被 URL 白名单拒绝: %w", err)
	}

	payload, err := json.Marshal(webDeepseekLoginRequest{
		Email: email, Mobile: "", Password: password, AreaCode: "", DeviceID: deviceID, OS: "web",
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", webDeepseekLoginUA)

	resp, err := s.httpUpstream.Do(req, accountProxyURL(account), account.ID, account.Concurrency)
	if err != nil {
		return "", fmt.Errorf("deepseek web 登录网络错误: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("deepseek web 登录响应读取失败: %w", err)
	}
	if resp.StatusCode >= 400 {
		// 403 按 WAF 分类（可能为人机验证）：直接失败关闭，由前端提示人工处理，绝不伪造成功。
		if resp.StatusCode == http.StatusForbidden {
			return "", &webLoginHTTPError{
				Platform: PlatformDeepseek, Code: int64(resp.StatusCode), Kind: WebLoginKindWAF,
				Msg: "deepseek web 登录被安全拦截（HTTP 403，可能需人机验证）",
			}
		}
		return "", &webLoginHTTPError{
			Platform: PlatformDeepseek, Code: int64(resp.StatusCode), Kind: WebLoginKindLogin,
			Msg: fmt.Sprintf("deepseek web 登录返回 HTTP %d", resp.StatusCode),
		}
	}

	var parsed webDeepseekLoginResponse
	_ = json.Unmarshal(raw, &parsed)
	code := parsed.Data.BizCode
	if code == 0 {
		code = parsed.Code // 兜底：少数形态顶层 code 携带业务码
	}
	if code != 0 {
		return "", &webLoginHTTPError{
			Platform: PlatformDeepseek, Code: code, Kind: WebLoginKindLogin,
			Msg: fmt.Sprintf("deepseek web 登录失败 biz_code=%d", code),
		}
	}

	cookie := combineSetCookies(resp)
	if cookie == "" {
		return "", &webLoginHTTPError{
			Platform: PlatformDeepseek, Code: code, Kind: WebLoginKindLogin,
			Msg: "deepseek web 登录成功但未返回 Set-Cookie（不可重试）",
		}
	}
	if err := s.verifyDeepseekCookie(ctx, account, base, cookie); err != nil {
		return "", err
	}
	return cookie, nil
}

// verifyDeepseekCookie 用已取证的 PoW challenge 端点验证新 Cookie 确实建立了登录态。
// 未登录态会返回 40002/Missing Token 且无 challenge；验证请求复用账号代理、UA 和 login_device_id。
func (s *WebPlatformAutoLoginService) verifyDeepseekCookie(ctx context.Context, account *Account, base, cookie string) error {
	target := strings.TrimRight(base, "/") + webDeepseekPoWChallengePath
	if _, err := s.validateUpstreamURL(target); err != nil {
		return fmt.Errorf("deepseek web 登录验证目标被 URL 白名单拒绝: %w", err)
	}
	body := []byte(`{"target_path":"` + webDeepseekChatCompletionPath + `"}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", webDeepseekOriginFromURL(base))
	req.Header.Set("Referer", webDeepseekOriginFromURL(base)+"/")
	req.Header.Set("User-Agent", webDeepseekLoginUA)
	req.Header.Set("Cookie", cookie)
	webDeepseekApplyRequestHeaders(req, account)

	resp, err := s.httpUpstream.Do(req, accountProxyURL(account), account.ID, account.Concurrency)
	if err != nil {
		return fmt.Errorf("deepseek web 登录验证网络错误: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("deepseek web 登录验证响应读取失败: %w", err)
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusAccepted || resp.Header.Get("x-amzn-waf-action") != "" {
		return &webLoginHTTPError{Platform: PlatformDeepseek, Code: int64(resp.StatusCode), Kind: WebLoginKindWAF, Msg: "deepseek web 登录验证被安全拦截"}
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return &webLoginHTTPError{Platform: PlatformDeepseek, Code: WebLoginCodeHTTPTooMany, Kind: WebLoginKindLogin, Msg: "deepseek web 登录验证触发限流"}
	}
	code, isErr := webDeepseekEffectiveErrorCode(resp.StatusCode, raw)
	if resp.StatusCode >= 400 || isErr {
		return &webLoginHTTPError{Platform: PlatformDeepseek, Code: code, Kind: WebLoginKindLogin, Msg: "deepseek web 登录 Cookie 验证失败"}
	}
	challenge, ok := webDeepseekExtractPoWChallenge(raw)
	if !ok {
		return &webLoginHTTPError{Platform: PlatformDeepseek, Code: -1, Kind: WebLoginKindLogin, Msg: "deepseek web 登录验证响应缺少已取证的 PoW challenge"}
	}
	if _, err := webDeepseekSolvePoW(challenge, webDeepseekChatCompletionPath); err != nil {
		return &webLoginHTTPError{Platform: PlatformDeepseek, Code: WebLoginCodePoW1, Kind: WebLoginKindWAF, Msg: "deepseek web 登录验证 PoW challenge 不可解"}
	}
	return nil
}

// ---------------------------------------------------------------------------
// zhipu / kimi：refresh token 续期
// ---------------------------------------------------------------------------

type webKimiRefreshRequest struct {
	RefreshToken string `json:"refreshToken"`
}

type webKimiRefreshResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
}

// RefreshToken 对 zhipu / kimi 网页接入执行 refresh token 续期。成功时内部原子更新凭据
// （refresh_token / 新 cookie / access_token），失败返回 *webLoginHTTPError。
func (s *WebPlatformAutoLoginService) RefreshToken(ctx context.Context, account *Account) error {
	ctx, cancel := context.WithTimeout(ctx, webSMSRequestTimeout)
	defer cancel()
	switch webAutoLoginPlatformKey(account) {
	case PlatformZhipu:
		return s.refreshZhipu(ctx, account)
	case PlatformKimi:
		return s.refreshKimi(ctx, account)
	default:
		return fmt.Errorf("RefreshToken 不支持平台 %q", account.Platform)
	}
}

func (s *WebPlatformAutoLoginService) refreshZhipu(ctx context.Context, account *Account) error {
	refreshToken := strings.TrimSpace(account.GetCredential(CredKeyLoginRefreshToken))
	if refreshToken == "" {
		refreshToken = strings.TrimSpace(account.GetCredential("refresh_token"))
	}
	// cookie 兜底：与转发侧 refreshWebZhipuAccessToken（web_zhipu_gateway_forward.go:557）
	// 同口径——显式键缺失时从账号已保存的整串 Cookie 解析 chatglm_refresh_token，
	// 使两条续期路径凭据口径一致。
	if refreshToken == "" {
		if cookie := strings.TrimSpace(account.GetCredential("cookie")); cookie != "" {
			refreshToken = webZhipuExtractCookieField(cookie, "chatglm_refresh_token")
		}
	}
	if refreshToken == "" {
		return &webLoginHTTPError{
			Platform: PlatformZhipu, Code: -1, Kind: WebLoginKindRefresh,
			Msg: "缺少 refresh_token（需半自动短信登录）",
		}
	}
	base := strings.TrimRight(account.GetWebBaseURL(), "/")
	if base == "" {
		base = DefaultWebZhipuBaseURL
	}
	target := base + WebZhipuRefreshEndpoint
	if _, err := s.validateUpstreamURL(target); err != nil {
		return fmt.Errorf("zhipu web 续期目标被 URL 白名单拒绝: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", "chatglm_refresh_token="+refreshToken)

	resp, err := s.httpUpstream.Do(req, accountProxyURL(account), account.ID, account.Concurrency)
	if err != nil {
		return fmt.Errorf("zhipu web 续期网络错误: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 读取并丢弃响应体（保证连接复用），业务结果以 Set-Cookie 为准。
	if _, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)); err != nil {
		return fmt.Errorf("zhipu web 续期响应读取失败: %w", err)
	}
	if resp.StatusCode >= 400 {
		return &webLoginHTTPError{
			Platform: PlatformZhipu, Code: int64(resp.StatusCode), Kind: WebLoginKindRefresh,
			Msg: fmt.Sprintf("zhipu web 续期返回 HTTP %d", resp.StatusCode),
		}
	}

	newCookie := combineSetCookies(resp)
	chatGLMToken := extractCookieValue(newCookie, "chatglm_token")
	if chatGLMToken == "" {
		return &webLoginHTTPError{
			Platform: PlatformZhipu, Code: -1, Kind: WebLoginKindRefresh,
			Msg: "zhipu web 续期响应缺少 chatglm_token（不可重试）",
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	updates := map[string]any{
		// 业务键：与转发链既有键保持一致，forward 方可直接消费。
		"cookie":                 newCookie,
		"chatglm_token":          chatGLMToken,
		"refresh_token":          refreshToken,
		CredKeyLoginRefreshToken: refreshToken,
		CredKeyLoginLastAt:       now,
		CredKeyLoginFailCount:    0,
		CredKeyLoginLastError:    "",
		CredKeyLoginNonRetryable: "false",
	}
	return s.persistRecoveredCredentials(ctx, account, updates)
}

func (s *WebPlatformAutoLoginService) refreshKimi(ctx context.Context, account *Account) error {
	refreshToken := strings.TrimSpace(account.GetCredential(CredKeyLoginRefreshToken))
	if refreshToken == "" {
		refreshToken = strings.TrimSpace(account.GetCredential("refresh_token"))
	}
	if refreshToken == "" {
		return &webLoginHTTPError{
			Platform: PlatformKimi, Code: -1, Kind: WebLoginKindRefresh,
			Msg: "缺少 refresh_token（需半自动短信登录）",
		}
	}
	return smsContextGapErrorKind(PlatformKimi, WebLoginKindRefresh, "RefreshToken 成功响应结构尚未取证")

	target := strings.TrimRight(webKimiAuthBaseURL, "/") + WebKimiRefreshEndpoint
	if _, err := s.validateUpstreamURL(target); err != nil {
		return fmt.Errorf("kimi web 续期目标被 URL 白名单拒绝: %w", err)
	}

	body, err := json.Marshal(webKimiRefreshRequest{RefreshToken: refreshToken})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpUpstream.Do(req, accountProxyURL(account), account.ID, account.Concurrency)
	if err != nil {
		return fmt.Errorf("kimi web 续期网络错误: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("kimi web 续期响应读取失败: %w", err)
	}
	if resp.StatusCode >= 400 {
		return &webLoginHTTPError{
			Platform: PlatformKimi, Code: int64(resp.StatusCode), Kind: WebLoginKindRefresh,
			Msg: fmt.Sprintf("kimi web 续期返回 HTTP %d", resp.StatusCode),
		}
	}

	var parsed webKimiRefreshResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return &webLoginHTTPError{
			Platform: PlatformKimi, Code: -1, Kind: WebLoginKindRefresh,
			Msg: "kimi web 续期响应解析失败",
		}
	}
	if parsed.AccessToken == "" || parsed.RefreshToken == "" {
		return &webLoginHTTPError{
			Platform: PlatformKimi, Code: -1, Kind: WebLoginKindRefresh,
			Msg: "kimi web 续期响应缺少 accessToken/refreshToken（不可重试）",
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	updates := map[string]any{
		// 业务键：与转发链既有键保持一致。
		"access_token":           parsed.AccessToken,
		"refresh_token":          parsed.RefreshToken,
		CredKeyLoginRefreshToken: parsed.RefreshToken,
		// kimi refresh 同时回换新 refresh，记录过期基准。
		CredKeyLoginRefreshExpiresAt: time.Now().UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339),
		CredKeyLoginLastAt:           now,
		CredKeyLoginFailCount:        0,
		CredKeyLoginLastError:        "",
		CredKeyLoginNonRetryable:     "false",
	}
	return s.persistRecoveredCredentials(ctx, account, updates)
}

// persistRecoveredCredentials 原子合并写回凭据（保留既有键），并同步更新内存对象。
func (s *WebPlatformAutoLoginService) persistRecoveredCredentials(ctx context.Context, account *Account, updates map[string]any) error {
	merged := s.mergeCredentials(account, updates)
	if err := s.store.UpdateAccountCredentials(ctx, account.ID, merged); err != nil {
		return err
	}
	account.Credentials = merged
	return nil
}

// extractCookieValue 从整串 Cookie 中提取指定键的值（用于把 refresh 返回的新 token
// 同步到 forward 既有的单键字段）。
func extractCookieValue(cookie, key string) string {
	for _, part := range strings.Split(cookie, ";") {
		part = strings.TrimSpace(part)
		if i := strings.Index(part, "="); i > 0 {
			if strings.TrimSpace(part[:i]) == key {
				return strings.TrimSpace(part[i+1:])
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 两级恢复
// ---------------------------------------------------------------------------

// WebRecoverResult 是 RecoverAccount 的结果。
type WebRecoverResult struct {
	Recovered     bool   // 第一级恢复成功
	NeedsSemiAuto bool   // 需半自动（短信码）
	Detail        string // 细化错误摘要（不含凭据）
}

// RecoverAccount 对单个 web-* 账号执行两级恢复：
//   - deepseek：第一级 LoginByEmail（全自动）；
//   - zhipu/kimi：第一级 RefreshToken，失败 → 返回需半自动（短信码）。
//
// 恢复成功 → StatusActive + 清 ErrorMessage；失败 → 保留 StatusError +
// 写 ErrorMessage=细化摘要（同时更新凭据内 login_last_error）。状态与凭据原子落库。
func (s *WebPlatformAutoLoginService) RecoverAccount(ctx context.Context, account *Account) WebRecoverResult {
	if account == nil {
		return WebRecoverResult{Detail: "nil account"}
	}
	switch webAutoLoginPlatformKey(account) {
	case PlatformDeepseek:
		return s.recoverDeepseek(ctx, account)
	case PlatformZhipu, PlatformKimi:
		return s.recoverZhipuKimi(ctx, account)
	default:
		return WebRecoverResult{Detail: "不支持的自动登录平台：" + account.Platform}
	}
}

func (s *WebPlatformAutoLoginService) recoverDeepseek(ctx context.Context, account *Account) WebRecoverResult {
	cookie, err := s.LoginByEmail(ctx, account)
	if err == nil {
		now := time.Now().UTC().Format(time.RFC3339)
		updates := map[string]any{
			"cookie":                 cookie,
			CredKeyLoginDeviceID:     account.GetCredential(CredKeyLoginDeviceID),
			CredKeyLoginLastAt:       now,
			CredKeyLoginFailCount:    0,
			CredKeyLoginLastError:    "",
			CredKeyLoginNonRetryable: "false",
		}
		merged := s.mergeCredentials(account, updates)
		if err := s.store.UpdateAccountCredentials(ctx, account.ID, merged); err != nil {
			return s.failAccount(ctx, account, fmt.Errorf("deepseek 登录凭据写入失败: %w", err))
		}
		account.Credentials = merged
		if err := s.store.UpdateAccountStatus(ctx, account.ID, StatusActive, ""); err != nil {
			return s.failAccount(ctx, account, fmt.Errorf("deepseek 登录状态写入失败: %w", err))
		}
		account.Status = StatusActive
		account.ErrorMessage = ""
		return WebRecoverResult{Recovered: true}
	}
	return s.failAccount(ctx, account, err)
}

func (s *WebPlatformAutoLoginService) recoverZhipuKimi(ctx context.Context, account *Account) WebRecoverResult {
	err := s.RefreshToken(ctx, account)
	if err == nil {
		if err := s.store.UpdateAccountStatus(ctx, account.ID, StatusActive, ""); err != nil {
			return s.failAccount(ctx, account, fmt.Errorf("网页登录态状态写入失败: %w", err))
		}
		account.Status = StatusActive
		account.ErrorMessage = ""
		return WebRecoverResult{Recovered: true}
	}
	// refresh 失败 → 需半自动（短信码）；同时落库错误与不可重试标记。
	return s.failAccount(ctx, account, err)
}

// failAccount 统一处理恢复失败：写细化摘要、累加 fail_count、置不可重试标记（视错误类别）、
// 保持 StatusError。返回 WebRecoverResult（zhipu/kimi refresh 失败固定 NeedsSemiAuto）。
func (s *WebPlatformAutoLoginService) failAccount(ctx context.Context, account *Account, err error) WebRecoverResult {
	code := int64(0)
	kind := ""
	var le *webLoginHTTPError
	if errors.As(err, &le) {
		code = le.Code
		kind = le.Kind
	}
	info := classifyWebLoginError(account.Platform, code, kind)

	// zhipu/kimi 的 refresh 是其唯一自动恢复手段；一旦失败即需半自动短信登录，
	// 标记不可重试，不再自动退避重试。
	isSemiAutoPlatform := account.Platform == PlatformZhipu || account.Platform == PlatformKimi
	if isSemiAutoPlatform {
		info.NonRetryable = true
	}

	now := time.Now().UTC().Format(time.RFC3339)
	detail := info.Title
	if info.Hint != "" {
		detail = info.Title + "：" + info.Hint
	}
	if detail == "" {
		detail = "自动登录/续期失败：" + err.Error()
	}

	updates := map[string]any{
		CredKeyLoginLastAt:    now,
		CredKeyLoginLastError: detail,
	}
	fc := account.GetCredentialAsInt64(CredKeyLoginFailCount)
	updates[CredKeyLoginFailCount] = fc + 1
	if info.NonRetryable {
		updates[CredKeyLoginNonRetryable] = "true"
	}

	merged := s.mergeCredentials(account, updates)
	if writeErr := s.store.UpdateAccountCredentials(ctx, account.ID, merged); writeErr != nil {
		return WebRecoverResult{Recovered: false, Detail: "自动登录失败：凭据写入失败"}
	}
	account.Credentials = merged
	if writeErr := s.store.UpdateAccountStatus(ctx, account.ID, StatusError, detail); writeErr != nil {
		return WebRecoverResult{Recovered: false, Detail: "自动登录失败：状态写入失败"}
	}
	account.Status = StatusError
	account.ErrorMessage = detail

	result := WebRecoverResult{Recovered: false, Detail: detail}
	// zhipu/kimi 的 refresh 是第一级（也是唯一自动级），失败即需半自动短信登录。
	if account.Platform == PlatformZhipu || account.Platform == PlatformKimi {
		result.NeedsSemiAuto = true
	}
	return result
}

// ---------------------------------------------------------------------------
// 错误细化（三平台共用）
// ---------------------------------------------------------------------------

// WebPlatformErrorDetail 返回指定平台/码/类别的中文标题与提示。文案绝不包含凭据。
// kind 为 WebLoginKind*；当 kind==WebLoginKindWAF 时忽略 code 直接返回 WAF 文案。
func WebPlatformErrorDetail(platform string, code int64, kind string) (title, hint string) {
	if kind == WebLoginKindWAF {
		return "触发安全拦截（WAF）", "请求被平台风控（WAF）拦截，请更换网络环境或稍后重试。"
	}
	switch code {
	case WebLoginCodeBadCredential:
		return "登录失败：邮箱或密码错误", "请核对账号邮箱与密码；若确认正确但仍失败，可能需改用半自动短信登录。"
	case WebLoginCodeBanned:
		return "账号已被封禁", "该账号已被平台封禁，无法自动登录，请联系管理员处理。"
	case WebLoginCodeAuthExpired, WebLoginCodeAuthExpired2:
		return "登录态已失效", "凭证已过期，正在尝试自动续期登录态。"
	case WebLoginCodeRateLimited, WebLoginCodeHTTPTooMany:
		return "请求过于频繁", "触发上游限流，请稍后重试。"
	case WebLoginCodePoW1, WebLoginCodePoW2:
		return "需要人机验证", "触发 PoW 验证，无法自动通过，请人工处理。"
	case WebLoginCodeSuccess:
		return "", ""
	default:
		return "自动登录失败", "发生未知错误，请稍后重试或改用半自动登录。"
	}
}

// classifyWebLoginError 将错误码/类别映射为可重试性分类（含细化文案）。
func classifyWebLoginError(platform string, code int64, kind string) webLoginErrorInfo {
	title, hint := WebPlatformErrorDetail(platform, code, kind)
	var cls webLoginErrorClass
	switch {
	case kind == WebLoginKindWAF:
		cls = webLoginNonRetryable
	case code == WebLoginCodeBanned, code == WebLoginCodeBadCredential:
		cls = webLoginNonRetryable
	case code == WebLoginCodePoW1, code == WebLoginCodePoW2:
		cls = webLoginNonRetryable
	default:
		cls = webLoginRetryable
	}
	return webLoginErrorInfo{
		Title:        title,
		Hint:         hint,
		Retryable:    cls == webLoginRetryable,
		NonRetryable: cls == webLoginNonRetryable,
	}
}

// ---------------------------------------------------------------------------
// 平台键归一
// ---------------------------------------------------------------------------

// webAutoLoginPlatformKey 返回账号自动登录的平台键（平台归并后唯一口径）：
// 官方平台（zhipu/deepseek/kimi）+ access_mode=web 命中；返回归一后的官方平台键，
// 非 web 接入账号返回空串。
func webAutoLoginPlatformKey(account *Account) string {
	if account == nil {
		return ""
	}
	if account.IsWebAccessMode() {
		switch account.Platform {
		case PlatformDeepseek, PlatformZhipu, PlatformKimi:
			return account.Platform
		}
	}
	return ""
}
