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
	"github.com/tidwall/gjson"

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

// extractCookieValue 从整串 Cookie 中提取指定键的值。web_sms_login.go 的 zhipu
// 短信登录响应用它解析 chatglm_token / chatglm_refresh_token。
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
//   - zhipu/kimi：无自动 refresh（2026-09-21 撤销 Web 后台保活后，refresh 自动恢复
//     已删除；转发链 refreshWebKimiAccessToken / refreshWebZhipuAccessToken 是唯一
//     refresh 实现）→ 直接返回需半自动（短信登录重新授权）。
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
		return WebRecoverResult{
			NeedsSemiAuto: true,
			Detail:        "请通过短信登录重新授权",
		}
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

// ---------------------------------------------------------------------------
// deepseek 邮箱注册自动接入（deepseek-email-register-plan.md §1.2/§1.3）
// ---------------------------------------------------------------------------

// webDeepseekGuestPoWChallengePath guest PoW 挑战端点（方案 §1.3/§2 取证 2：注册链路
// 用 create_guest_challenge，target_path 指向待调用端点自身；与登录态转发链的
// create_pow_challenge 是不同端点，不得混用）。
const webDeepseekGuestPoWChallengePath = "/api/v0/users/create_guest_challenge"

// WebDeepseekEmailCodeSendPath 发送邮箱验证码端点（方案 §1.3 取证钉死）。
const WebDeepseekEmailCodeSendPath = "/api/v0/users/create_email_verification_code"

// WebDeepseekRegisterPath 邮箱注册端点（方案 §1.3 取证钉死）。
const WebDeepseekRegisterPath = "/api/v0/users/register"

// webDeepseekRegisterErrorCode 枚举（方案 §2 取证 5，main.js REGISTER_ERROR_CODE）。
const (
	webDeepseekRegisterCodeEmailExists     int64 = 1
	webDeepseekRegisterCodeInvalidPassword int64 = 4
	webDeepseekRegisterCodeTooManyAttempts int64 = 5
	webDeepseekRegisterCodeFromMainland    int64 = 6
	webDeepseekRegisterCodeEmailExpired    int64 = 7
	webDeepseekRegisterCodePasscodeFailed  int64 = 8
	webDeepseekRegisterCodeDomainNotSupp   int64 = 9
)

// webDeepseekEmailCodeBizCode 发码业务码（方案 §2 取证 4）。
const (
	webDeepseekEmailCodeBizCaptcha     int64 = 2 // RECAPTCHA_VERIFY_FAILED（人机验证拦截）
	webDeepseekEmailCodeBizDomainNotSu int64 = 9 // EMAIL_DOMAIN_NOT_SUPPORTED
)

// webDeepseekRegisterGuestContext 一次 guest PoW 出站所需的公共参数。
type webDeepseekRegisterGuestContext struct {
	baseURL  string
	proxyURL string
	deviceID string
}

// webDeepseekPostGuestJSON 以注册链路公共头族 POST JSON body，返回响应。
// 公共头（方案 §1.2/§1.3）：Content-Type/Accept: application/json、Origin/Referer
// 官方域、User-Agent=webDeepseekLoginUA、x-client-platform=web、x-client-version=1.1、
// x-client-locale=en、x-device-id、X-DS-Guest-PoW-Response。
func (s *WebPlatformAutoLoginService) webDeepseekPostGuestJSON(ctx context.Context, guest webDeepseekRegisterGuestContext, path, powHeader string, body any) (*http.Response, []byte, error) {
	target := guest.baseURL + path
	if _, err := s.validateUpstreamURL(target); err != nil {
		return nil, nil, fmt.Errorf("deepseek web 注册链路目标被 URL 白名单拒绝: %w", err)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	origin := webDeepseekOriginFromURL(guest.baseURL)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", webDeepseekLoginUA)
	req.Header.Set("x-client-platform", "web")
	req.Header.Set("x-client-version", "1.1")
	req.Header.Set("x-client-locale", "en")
	req.Header.Set("x-device-id", guest.deviceID)
	if powHeader != "" {
		req.Header.Set("X-DS-Guest-PoW-Response", powHeader)
	}

	resp, err := s.httpUpstream.Do(req, guest.proxyURL, 0, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("deepseek web 注册链路网络错误: %w", err)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	closeErr := resp.Body.Close()
	if err != nil {
		return nil, nil, fmt.Errorf("deepseek web 注册链路响应读取失败: %w", err)
	}
	if closeErr != nil {
		return nil, nil, closeErr
	}
	return resp, raw, nil
}

// RegisterOutcomeUnknownError 标记「注册 POST 已出站，但响应在服务端丢失/不可解析前
// 即失败」的错误（外审 2026-09-22 R3-P2）。注册是非幂等出站：此时上游账号可能已创建，
// 调用方（web-register 端点幂等闭包）不得把本错误交给协调器走 failed_retryable 自动
// 重试（会二次打上游拿 EMAIL_EXISTS），必须转终态结果路径返回密码登录恢复指引。
type RegisterOutcomeUnknownError struct {
	Err error
}

func (e *RegisterOutcomeUnknownError) Error() string {
	return fmt.Sprintf("deepseek web 注册结果不明（出站后响应丢失，上游账号可能已创建）: %v", e.Err)
}

func (e *RegisterOutcomeUnknownError) Unwrap() error { return e.Err }

// webDeepseekGuestChallengeResponse create_guest_challenge 响应（方案 §2 取证 2：
// data.biz_data.guest_challenge，字段结构与登录态 challenge 一致）。
type webDeepseekGuestChallengeResponse struct {
	Code int64 `json:"code"`
	Data struct {
		BizCode int64 `json:"biz_code"`
		BizData struct {
			GuestChallenge *webDeepseekPowChallenge `json:"guest_challenge"`
		} `json:"biz_data"`
	} `json:"data"`
}

// webDeepseekSolveGuestPoW 实时取 guest 挑战并本地求解，返回 X-DS-Guest-PoW-Response 头值。
// PoW 单次有效（方案 §2 取证 6）：每次上游请求前实时「取挑战→求解→立即用」，禁止缓存——
// 因此不提供任何挑战缓存，调用方每次调用本函数都是一次全新出站。挑战缺失 / 算法不符 /
// 求解失败一律失败关闭（WAF 类，与登录链同分类），绝不伪造应答。
func (s *WebPlatformAutoLoginService) webDeepseekSolveGuestPoW(ctx context.Context, guest webDeepseekRegisterGuestContext, targetPath string) (string, error) {
	resp, raw, err := s.webDeepseekPostGuestJSON(ctx, guest, webDeepseekGuestPoWChallengePath, "", map[string]string{"target_path": targetPath})
	if err != nil {
		return "", &webLoginHTTPError{
			Platform: PlatformDeepseek, Code: -1, Kind: WebLoginKindWAF,
			Msg: "deepseek web guest PoW 挑战获取失败: " + err.Error(),
		}
	}
	if resp.StatusCode >= 400 || resp.StatusCode == http.StatusForbidden || resp.Header.Get("x-amzn-waf-action") != "" {
		return "", &webLoginHTTPError{
			Platform: PlatformDeepseek, Code: int64(resp.StatusCode), Kind: WebLoginKindWAF,
			Msg: fmt.Sprintf("deepseek web guest PoW 挑战被安全拦截（HTTP %d）", resp.StatusCode),
		}
	}
	var parsed webDeepseekGuestChallengeResponse
	_ = json.Unmarshal(raw, &parsed)
	if parsed.Data.BizData.GuestChallenge == nil {
		return "", &webLoginHTTPError{
			Platform: PlatformDeepseek, Code: -1, Kind: WebLoginKindWAF,
			Msg: "deepseek web guest PoW 挑战缺失（响应无 data.biz_data.guest_challenge）",
		}
	}
	header, err := webDeepseekSolvePoW(*parsed.Data.BizData.GuestChallenge, targetPath)
	if err != nil {
		return "", &webLoginHTTPError{
			Platform: PlatformDeepseek, Code: WebLoginCodePoW1, Kind: WebLoginKindWAF,
			Msg: "deepseek web guest PoW 求解失败: " + err.Error(),
		}
	}
	return header, nil
}

// webDeepseekGuestUpstreamError 把 HTTP 层失败统一映射为失败关闭错误
// （WAF/429 按既有分类；HTTP>=400 复用 webDeepseekEffectiveErrorCode 双路径判定取码）。
// 响应体业务码（data.biz_code）的专项枚举映射由各调用方在本函数之后完成——本函数
// 不得提前消费 body 业务码，否则专项文案（人机验证/邮箱域名/注册枚举）永远走不到。
func (s *WebPlatformAutoLoginService) webDeepseekGuestUpstreamError(resp *http.Response, raw []byte, action string) error {
	if resp.StatusCode == http.StatusForbidden || resp.Header.Get("x-amzn-waf-action") != "" {
		return &webLoginHTTPError{
			Platform: PlatformDeepseek, Code: int64(resp.StatusCode), Kind: WebLoginKindWAF,
			Msg: "deepseek web " + action + "被安全拦截（HTTP 403，可能需人机验证）",
		}
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return &webLoginHTTPError{
			Platform: PlatformDeepseek, Code: WebLoginCodeHTTPTooMany, Kind: WebLoginKindLogin,
			Msg: "deepseek web " + action + "触发限流",
		}
	}
	if resp.StatusCode >= 400 {
		code, _ := webDeepseekEffectiveErrorCode(resp.StatusCode, raw)
		return &webLoginHTTPError{
			Platform: PlatformDeepseek, Code: code, Kind: WebLoginKindLogin,
			Msg: fmt.Sprintf("deepseek web %s失败 biz_code=%d", action, code),
		}
	}
	return nil
}

// CreateGuestPoWHeader 取 guest 挑战并求解，返回 X-DS-Guest-PoW-Response 头值。
// 每次调用都是一次全新出站（PoW 一次一换，方案 §2 取证 6），不做任何缓存。
func (s *WebPlatformAutoLoginService) CreateGuestPoWHeader(ctx context.Context, baseURL, proxyURL, deviceID, targetPath string) (string, error) {
	guest := webDeepseekRegisterGuestContext{
		baseURL:  strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		proxyURL: proxyURL,
		deviceID: deviceID,
	}
	if guest.baseURL == "" {
		return "", &webLoginHTTPError{
			Platform: PlatformDeepseek, Code: -1, Kind: WebLoginKindLogin,
			Msg: "deepseek web 注册链路缺少可用的 web base_url（不可重试）",
		}
	}
	if guest.deviceID == "" {
		return "", &webLoginHTTPError{
			Platform: PlatformDeepseek, Code: -1, Kind: WebLoginKindLogin,
			Msg: "deepseek web 注册链路缺少 device_id（不可重试）",
		}
	}
	return s.webDeepseekSolveGuestPoW(ctx, guest, targetPath)
}

// webDeepseekEmailCodeSendRequest 发码请求体。shumei_verification 按 2026-09-23 生产
// 实测矩阵（方案 §2 取证 4）：省略/null → biz_code=2 人机验证文案；空串/假串 →
// 上游 422 格式拒绝（不透明错误路径）。服务端无真实风控 token，固定发 nil（null）。
type webDeepseekEmailCodeSendRequest struct {
	Email              string `json:"email"`
	TurnstileToken     string `json:"turnstile_token"`
	Locale             string `json:"locale"`
	ShumeiVerification *string `json:"shumei_verification"`
	HcaptchaToken      string `json:"hcaptcha_token"`
	DeviceID           string `json:"device_id"`
	Scenario           string `json:"scenario"`
}

// webDeepseekEmailCodeSendResponse 发码响应（biz_data.send_window_secs 为发送窗口秒数）。
type webDeepseekEmailCodeSendResponse struct {
	Code int64 `json:"code"`
	Data struct {
		BizCode int64  `json:"biz_code"`
		BizMsg  string `json:"biz_msg"`
		BizData struct {
			SendWindowSecs int64 `json:"send_window_secs"`
		} `json:"biz_data"`
	} `json:"data"`
}

// webDeepseekJSONNumberIsZero 判定 raw 中 path 位置的值是否为「明确的数值零」
// （外审 2026-09-22 R3-P1）：gjson 只验字段存在不够——`{"biz_code":null}` 经
// json.Unmarshal 会把 Go 整数字段留作零值，无法与真实 0 区分；此处直接在原始
// JSON 上要求 JSONNumber 类型且值等于 0，null/字符串/缺失/对象一律不算。
func webDeepseekJSONNumberIsZero(raw []byte, path string) bool {
	res := gjson.GetBytes(raw, path)
	return res.Type == gjson.Number && res.Int() == 0
}

// SendRegisterEmailCode 调上游发码接口：先取 guest PoW（target_path=发码自身路径）→
// POST create_email_verification_code → biz_code=0 返回 send_window_secs（缺失返回 0）；
// 2 → RECAPTCHA 人机验证文案；9 → 邮箱域不支持文案；其余按既有分类失败关闭。
func (s *WebPlatformAutoLoginService) SendRegisterEmailCode(ctx context.Context, email, locale, deviceID, scenario, proxyURL string) (int64, error) {
	base := strings.TrimRight(DefaultWebDeepseekBaseURL, "/")
	guest := webDeepseekRegisterGuestContext{baseURL: base, proxyURL: proxyURL, deviceID: deviceID}

	// PoW 一次一换：发码请求前实时取挑战求解，禁止缓存（方案 §1.2/§2 取证 6）。
	powHeader, err := s.webDeepseekSolveGuestPoW(ctx, guest, WebDeepseekEmailCodeSendPath)
	if err != nil {
		return 0, err
	}

	resp, raw, err := s.webDeepseekPostGuestJSON(ctx, guest, WebDeepseekEmailCodeSendPath, powHeader, webDeepseekEmailCodeSendRequest{
		Email:              email,
		TurnstileToken:     "",
		Locale:             locale,
		ShumeiVerification: nil, // 2026-09-23 实测：空串/假串上游 422；null → biz_code=2 人机验证文案（方案 §2 取证 4）
		HcaptchaToken:      "",
		DeviceID:           deviceID,
		Scenario:           scenario,
	})
	if err != nil {
		return 0, err
	}
	if err := s.webDeepseekGuestUpstreamError(resp, raw, "发码"); err != nil {
		return 0, err
	}

	var parsed webDeepseekEmailCodeSendResponse
	uerr := json.Unmarshal(raw, &parsed)
	// 外审 2026-09-22 R2-P1 + R3-P1：顶层 code 与 data.biz_code 必须是「明确的数值零」
	// （gjson Number 且 ==0）才允许判为已发送；字段缺失、JSON null（Unmarshal 后零值
	// 不可分辨）、字符串、格式异常一律失败关闭。明确的正值数值业务码放行到下方枚举
	// 映射（人机验证/域不支持等）；其余异常失败关闭。
	bizCodeIsZero := webDeepseekJSONNumberIsZero(raw, "data.biz_code")
	topCodeIsZero := webDeepseekJSONNumberIsZero(raw, "code")
	bizCodeValue := gjson.GetBytes(raw, "data.biz_code")
	passToBizEnum := uerr == nil && topCodeIsZero && bizCodeValue.Type == gjson.Number && bizCodeValue.Int() > 0
	if uerr != nil || (!passToBizEnum && (!topCodeIsZero || !bizCodeIsZero)) {
		return 0, &webLoginHTTPError{
			Platform: PlatformDeepseek, Kind: WebLoginKindLogin,
			Msg: fmt.Sprintf("deepseek web 发码响应异常，失败关闭（unmarshal=%v top_code_num0=%v biz_code_num0=%v）",
				uerr, topCodeIsZero, bizCodeIsZero),
		}
	}
	switch parsed.Data.BizCode {
	case 0:
		return parsed.Data.BizData.SendWindowSecs, nil
	case webDeepseekEmailCodeBizCaptcha:
		return 0, &webLoginHTTPError{
			Platform: PlatformDeepseek, Code: parsed.Data.BizCode, Kind: WebLoginKindLogin,
			Msg: "上游要求人机验证（RECAPTCHA_VERIFY_FAILED），请改用密码登录入口或稍后再试",
		}
	case webDeepseekEmailCodeBizDomainNotSu:
		return 0, &webLoginHTTPError{
			Platform: PlatformDeepseek, Code: parsed.Data.BizCode, Kind: WebLoginKindLogin,
			Msg: "该邮箱域名不被上游支持（EMAIL_DOMAIN_NOT_SUPPORTED），请更换邮箱后重试",
		}
	default:
		return 0, &webLoginHTTPError{
			Platform: PlatformDeepseek, Code: parsed.Data.BizCode, Kind: WebLoginKindLogin,
			Msg: fmt.Sprintf("deepseek web 发码失败 biz_code=%d", parsed.Data.BizCode),
		}
	}
}

// webDeepseekRegisterRequest 注册请求体（方案 §1.3 取证钉死：payload 内层 + os=web）。
type webDeepseekRegisterRequest struct {
	Locale  string `json:"locale"`
	Region  string `json:"region"`
	Payload struct {
		Email                 string `json:"email"`
		EmailVerificationCode string `json:"email_verification_code"`
		Password              string `json:"password"`
	} `json:"payload"`
	DeviceID string `json:"device_id"`
	OS       string `json:"os"`
}

// webDeepseekRegisterResponse 注册响应（仅 biz_code 参与判定）。
type webDeepseekRegisterResponse struct {
	Code int64 `json:"code"`
	Data struct {
		BizCode int64  `json:"biz_code"`
		BizMsg  string `json:"biz_msg"`
	} `json:"data"`
}

// webDeepseekRegisterBizCodeMessage 把注册业务码映射为中文文案（方案 §2 取证 5 枚举）。
func webDeepseekRegisterBizCodeMessage(code int64) string {
	switch code {
	case webDeepseekRegisterCodeEmailExists:
		return "该邮箱已在 DeepSeek 注册（EMAIL_EXISTS），请改用密码登录入口"
	case webDeepseekRegisterCodeInvalidPassword:
		return "密码不符合上游要求（INVALID_PASSWORD），需至少 8 位且包含字母和数字"
	case webDeepseekRegisterCodeTooManyAttempts:
		return "验证码尝试次数过多（EMAIL_VERIFY_TOO_MANY_ATTEMPTS），请稍后重试"
	case webDeepseekRegisterCodeFromMainland:
		return "注册被上游地域门控拒绝（REGISTER_FROM_MAINLAND）：出口 IP 需为非大陆（当前服务端为香港出口，请检查代理配置）"
	case webDeepseekRegisterCodeEmailExpired:
		return "邮箱验证码已过期（EMAIL_EXPIRED），请重新发送验证码"
	case webDeepseekRegisterCodePasscodeFailed:
		return "邮箱验证码错误（EMAIL_PASSCODE_FAILED），请核对后重试"
	case webDeepseekRegisterCodeDomainNotSupp:
		return "该邮箱域名不被上游支持（EMAIL_DOMAIN_NOT_SUPPORTED），请更换邮箱后重试"
	default:
		return fmt.Sprintf("deepseek web 注册失败 biz_code=%d", code)
	}
}

// RegisterByEmail 调上游注册接口：先取 guest PoW（target_path=/api/v0/users/register）→
// POST register → biz_code=0 即成功；1/4/5/6/7/8/9 按 §2 取证 5 枚举映射中文文案；
// WAF/429 按既有分类失败关闭。本方法不落库、不回传任何 Cookie。
func (s *WebPlatformAutoLoginService) RegisterByEmail(ctx context.Context, email, code, password, region, deviceID, proxyURL string) error {
	base := strings.TrimRight(DefaultWebDeepseekBaseURL, "/")
	guest := webDeepseekRegisterGuestContext{baseURL: base, proxyURL: proxyURL, deviceID: deviceID}

	// PoW 一次一换：注册请求前实时取挑战求解，禁止缓存（方案 §1.2/§2 取证 6）。
	powHeader, err := s.webDeepseekSolveGuestPoW(ctx, guest, WebDeepseekRegisterPath)
	if err != nil {
		return err
	}

	body := webDeepseekRegisterRequest{
		Locale: "en", Region: region, DeviceID: deviceID, OS: "web",
	}
	body.Payload.Email = email
	body.Payload.EmailVerificationCode = code
	body.Payload.Password = password

	resp, raw, err := s.webDeepseekPostGuestJSON(ctx, guest, WebDeepseekRegisterPath, powHeader, body)
	if err != nil {
		// 外审 2026-09-22 R3-P2：注册 POST 已出站但响应在服务端丢失/读取失败 → 结果
		// 不明（上游可能已创建账号），用哨兵错误标记，调用方不得自动重试注册。
		// 注意 WAF/429 类错误是上游明确拒绝（响应可见，注册未发生），不在此包装。
		return &RegisterOutcomeUnknownError{Err: err}
	}
	if err := s.webDeepseekGuestUpstreamError(resp, raw, "注册"); err != nil {
		// 外审 2026-09-22 R5-P1：注册出站后仅 HTTP 5xx 无法证明「注册未发生」（网关
		// 可能在上游处理完成后才失败，账号可能已创建）→ 并入结果不明终态。明确拒绝
		// （WAF 403 / 429 限流 / 其余 4xx，注册未发生）保留原分类文案。
		if resp.StatusCode >= 500 {
			return &RegisterOutcomeUnknownError{Err: err}
		}
		return err
	}

	var parsed webDeepseekRegisterResponse
	uerr := json.Unmarshal(raw, &parsed)
	// 外审 2026-09-22 R2-P1 + R3-P1：成功判定收紧——顶层 code 与 data.biz_code 必须
	// 都是「明确的数值零」（gjson Number 且 ==0）。顶层 code 非零（如
	// {"code":500,"data":{"biz_code":0}}）、biz_code 缺失、JSON null（Unmarshal 后
	// 与真实 0 不可分辨，gjson.Exists 只证明字段存在）、字符串/格式异常一律失败关闭，
	// 不得推断成功。正值业务码走枚举映射文案。
	bizCodeIsZero := webDeepseekJSONNumberIsZero(raw, "data.biz_code")
	topCodeIsZero := webDeepseekJSONNumberIsZero(raw, "code")
	bizCodeValue := gjson.GetBytes(raw, "data.biz_code")
	if uerr == nil && topCodeIsZero && bizCodeValue.Type == gjson.Number && bizCodeValue.Int() > 0 {
		// 可解析且为明确业务码错误（正值）→ 走枚举映射文案（明确拒绝，响应可见）。
		return &webLoginHTTPError{
			Platform: PlatformDeepseek, Code: bizCodeValue.Int(), Kind: WebLoginKindLogin,
			Msg: webDeepseekRegisterBizCodeMessage(bizCodeValue.Int()),
		}
	}
	if uerr != nil || !topCodeIsZero || !bizCodeIsZero {
		// 外审 2026-09-22 R4-P1：注册 POST 已出站，但响应为空体/截断 JSON/缺业务码等
		// 无法确认业务结果的形态——上游账号可能已创建。不得作为普通错误交协调器
		// failed_retryable（同键重试会重打上游拿 EMAIL_EXISTS），并入结果不明哨兵终态；
		// 明确的业务拒绝（上方正值分支）不在此列。
		return &RegisterOutcomeUnknownError{Err: fmt.Errorf(
			"deepseek web 注册响应异常，失败关闭（unmarshal=%v top_code_num0=%v biz_code_num0=%v）",
			uerr, topCodeIsZero, bizCodeIsZero)}
	}
	return nil
}
