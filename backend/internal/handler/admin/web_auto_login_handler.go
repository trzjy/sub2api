package admin

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// webAutoLoginCookieKey 是网页版账号凭据中存放整串 Cookie 头的键名（与
// web_deepseek_gateway_forward / web_zhipu_gateway_forward 读取的键一致）。
const webAutoLoginCookieKey = "cookie"

// webPlatformAutoLoginService 是网页版平台自动登录服务的本地接口，便于测试替换。
// 其方法签名与 service.WebPlatformAutoLoginService 对齐。
type webPlatformAutoLoginService interface {
	LoginByEmail(ctx context.Context, account *service.Account) (string, error)
	RefreshToken(ctx context.Context, account *service.Account) error
	RecoverAccount(ctx context.Context, account *service.Account) service.WebRecoverResult
	SendSmsCode(ctx context.Context, platform, phone string, challenge service.WebSMSChallenge, account *service.Account) (string, error)
	VerifySmsCode(ctx context.Context, platform, phone, code string, challenge service.WebSMSChallenge, account *service.Account) (*service.SMSLoginResult, error)
}

// adminAutoLoginStoreAdapter 将 service.AdminService 适配为自动登录服务所需的
// AutoLoginAccountStore 接口（ListAccounts / UpdateAccountCredentials /
// UpdateAccountStatus）。仅实现自动登录所需的三个方法，其余走 AdminService。
type adminAutoLoginStoreAdapter struct {
	svc service.AdminService
}

var _ service.AutoLoginAccountStore = (*adminAutoLoginStoreAdapter)(nil)

// NewAutoLoginStoreAdapter 暴露给 wiring 层：将 AdminService 适配为自动登录服务所需的
// AutoLoginAccountStore 接口。
func NewAutoLoginStoreAdapter(svc service.AdminService) service.AutoLoginAccountStore {
	return &adminAutoLoginStoreAdapter{svc: svc}
}

// ListAccounts 拉取全部账号（忽略分页，自动登录恢复用）。
func (a *adminAutoLoginStoreAdapter) ListAccounts(ctx context.Context) ([]*service.Account, error) {
	const pageSize = 100000
	accounts, _, err := a.svc.ListAccounts(ctx, 1, pageSize, "", "", "", "", 0, "", "", "")
	if err != nil {
		return nil, err
	}
	out := make([]*service.Account, 0, len(accounts))
	for i := range accounts {
		out = append(out, &accounts[i])
	}
	return out, nil
}

// UpdateAccountCredentials 将凭据合并写入账号（保留既有 credentials）。
func (a *adminAutoLoginStoreAdapter) UpdateAccountCredentials(ctx context.Context, id int64, creds map[string]any) error {
	account, err := a.svc.GetAccount(ctx, id)
	if err != nil {
		return err
	}
	merged := make(map[string]any, len(account.Credentials)+len(creds))
	for k, v := range account.Credentials {
		merged[k] = v
	}
	for k, v := range creds {
		merged[k] = v
	}
	_, err = a.svc.UpdateAccount(ctx, id, &service.UpdateAccountInput{Credentials: merged})
	return err
}

// UpdateAccountStatus 设置账号状态；errMsg 非空则写入错误，否则在置为 active 时清错。
func (a *adminAutoLoginStoreAdapter) UpdateAccountStatus(ctx context.Context, id int64, status string, errMsg string) error {
	if _, err := a.svc.UpdateAccount(ctx, id, &service.UpdateAccountInput{Status: status}); err != nil {
		return err
	}
	if errMsg != "" {
		return a.svc.SetAccountError(ctx, id, errMsg)
	}
	if status == service.StatusActive {
		if _, err := a.svc.ClearAccountError(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// WebLoginPassword 单账号自动登录入口（CreateAccountModal 未建账号时用）。
// POST /api/v1/admin/accounts/web-login-password
//
//	body {platform, login_email, login_password, login_phone?, account_id?}
//
// 安全：成功响应会回传本次登录得到的整串 Cookie（属一次性操作结果，非账号导出）；
// 仅在此成功响应返回，绝不写入日志/错误响应。
func (h *AccountHandler) WebLoginPassword(c *gin.Context) {
	var req struct {
		Platform      string `json:"platform" binding:"required"`
		LoginEmail    string `json:"login_email"`
		LoginPhone    string `json:"login_phone"`
		LoginPassword string `json:"login_password"`
		AccountID     *int64 `json:"account_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if !service.IsWebLoginPlatform(req.Platform) {
		title, hint := service.WebPlatformErrorDetail(req.Platform, 0, "platform")
		response.ErrorWithDetails(c, http.StatusBadRequest, title, "", map[string]string{"hint": hint})
		return
	}

	ctx := c.Request.Context()
	if h.webPlatformAutoLogin == nil {
		response.Error(c, http.StatusServiceUnavailable, "web auto-login service unavailable")
		return
	}

	loginCreds := map[string]any{}
	if req.LoginEmail != "" {
		loginCreds["login_email"] = req.LoginEmail
	}
	if req.LoginPhone != "" {
		loginCreds["login_phone"] = req.LoginPhone
	}
	if req.LoginPassword != "" {
		loginCreds["login_password"] = req.LoginPassword
	}

	account := &service.Account{Platform: req.Platform, Credentials: loginCreds}
	if req.AccountID != nil {
		existing, err := h.adminService.GetAccount(ctx, *req.AccountID)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		// 单账号入口已取到完整账号对象：归并后同平台可共存普通 API 账号与 Web
		// 账号，故此处按账号级 access_mode 隔离，拒绝把普通 API 账号当 Web 账号登录。
		if existing != nil && !existing.IsWebAccessMode() {
			response.ErrorWithDetails(c, http.StatusBadRequest, "not a web access-mode account", "",
				map[string]string{"hint": "web login is only supported for web access-mode accounts"})
			return
		}
		// 复用既有凭据中的登录标识（若存在），避免覆盖。
		if existing != nil && existing.Credentials != nil {
			for _, k := range []string{"login_email", "login_phone", "login_password"} {
				if _, ok := loginCreds[k]; !ok {
					if v, ok2 := existing.Credentials[k]; ok2 {
						loginCreds[k] = v
					}
				}
			}
		}
		account = existing
		if account.Credentials == nil {
			account.Credentials = map[string]any{}
		}
		for k, v := range loginCreds {
			account.Credentials[k] = v
		}
	}

	switch req.Platform {
	case service.PlatformDeepseek:
		cookie, err := h.webPlatformAutoLogin.LoginByEmail(ctx, account)
		if err != nil {
			title, hint := service.WebPlatformErrorDetail(req.Platform, 0, "login")
			_ = hint
			response.ErrorWithDetails(c, http.StatusBadRequest, title, "", map[string]string{"detail": err.Error()})
			return
		}
		// Cookie 仅在此成功响应一次性回传；不写日志。
		if req.AccountID != nil {
			updateCreds := map[string]any{webAutoLoginCookieKey: cookie}
			if req.LoginEmail != "" {
				updateCreds["login_email"] = req.LoginEmail
			}
			if req.LoginPhone != "" {
				updateCreds["login_phone"] = req.LoginPhone
			}
			if req.LoginPassword != "" {
				updateCreds["login_password"] = req.LoginPassword
			}
			if err := h.webPlatformAutoLoginStoreUpdateCreds(ctx, *req.AccountID, updateCreds); err != nil {
				response.ErrorFrom(c, err)
				return
			}
			if err := h.webPlatformAutoLoginStoreStatus(ctx, *req.AccountID, service.StatusActive, ""); err != nil {
				response.ErrorFrom(c, err)
				return
			}
			if _, err := h.adminService.ClearAccountError(ctx, *req.AccountID); err != nil {
				response.ErrorFrom(c, err)
				return
			}
		}
		response.Success(c, gin.H{"success": true, "cookie": cookie})
	case service.PlatformZhipu, service.PlatformKimi:
		// zhipu/kimi 官方网页端没有密码登录（手机号短信码，发码被数美滑块/易盾验证码
		// 保护）；唯一用户入口是手机号+短信码登录（WebLoginSMS）。
		title, hint := service.WebPlatformErrorDetail(req.Platform, 0, "login")
		response.ErrorWithDetails(c, http.StatusBadRequest, title, "",
			map[string]string{
				"detail": "该平台官方网页端不提供密码登录，请使用手机号+短信验证码登录",
				"hint":   hint,
			})
	default:
		response.BadRequest(c, "unsupported web platform: "+req.Platform)
	}
}

// ---------------------------------------------------------------------------
// WebLoginSMS 网页版平台手机号 + 短信码登录（zhipu / kimi 唯一用户入口）。
// POST /api/v1/admin/accounts/web-login-sms
//
//	action=send_code: {action, platform, phone, account_id?,
//	                   zhipu_captcha_rid?, zhipu_captcha_md5?, zhipu_phone_code?,
//	                   kimi_captcha_validate?}
//	  → {success:true}（发码成功无需回传客户端 token：zhipu 无 session_token 概念；
//	    kimi SendVerifyCodeResponse 为 proto 空消息，E0 取证确认无 session_token）
//	action=login:     {action, platform, phone, sms_code,
//	                   zhipu_captcha_rid?, zhipu_captcha_md5?, zhipu_phone_code?,
//	                   kimi_captcha_validate?, account_id?, name?}
//	  → {success:true, account_id, cookie?|access_token?, login_refresh_token?}
//
// 挑战处理（计划 §3）：zhipu/kimi 发码/登录受数美滑块/易盾验证码保护，所需求解值
// （zhipu: captcha_rid + captcha_md5 + phone_code；kimi: captcha_validate）由用户在
// 本机浏览器完成验证后，随同一次请求在 challeng 内回传、绑定本次请求（无状态短期会话，
// 不依赖外部打码助手、不新增维护池/常驻任务）。缺失或上游 WAF/PoW 时失败关闭，响应带
// needs_challenge 标记（Kind=WAF），提示前端经本机浏览器人工验证后回填挑战值再重试，
// 绝不伪造成功。
//
// 落库：登录成功后在 handler 中把本次所得凭据一次性写入账号（zhipu: 整串 cookie +
// login_refresh_token；kimi: access_token + login_refresh_token）；无 account_id 时
// 先校验平台必需凭证再 CreateAccount（access_mode=web/apikey），失败绝不落库。
// 安全：成功响应回传的凭据属一次性操作结果（非账号导出），绝不写入日志/错误响应。
// ---------------------------------------------------------------------------

// webLoginSMSRequest 是 POST /web-login-sms 的请求体。
type webLoginSMSRequest struct {
	Action    string `json:"action" binding:"required"`
	Platform  string `json:"platform" binding:"required"`
	Phone     string `json:"phone"`
	AccountID *int64 `json:"account_id"`
	Name      string `json:"name"`

	SMSCode string `json:"sms_code"`

	ZhipuCaptchaRid     string `json:"zhipu_captcha_rid"`
	ZhipuCaptchaMD5     string `json:"zhipu_captcha_md5"`
	ZhipuPhoneCode      string `json:"zhipu_phone_code"`
	KimiCaptchaValidate string `json:"kimi_captcha_validate"`
}

// challenge 把请求中的求解值组装为 service.WebSMSChallenge（空值即缺失，失败关闭口径）。
func (r *webLoginSMSRequest) challenge() service.WebSMSChallenge {
	return service.WebSMSChallenge{
		ZhipuCaptchaRid:     strings.TrimSpace(r.ZhipuCaptchaRid),
		ZhipuCaptchaMD5:     strings.TrimSpace(r.ZhipuCaptchaMD5),
		ZhipuPhoneCode:      strings.TrimSpace(r.ZhipuPhoneCode),
		KimiCaptchaValidate: strings.TrimSpace(r.KimiCaptchaValidate),
	}
}

// WebLoginSMS 处理 zhipu/kimi 手机号短信码登录的发码与提交。
func (h *AccountHandler) WebLoginSMS(c *gin.Context) {
	var req webLoginSMSRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if req.Platform != service.PlatformZhipu && req.Platform != service.PlatformKimi {
		response.BadRequest(c, "platform must be one of: zhipu, kimi")
		return
	}
	if req.Action != "send_code" && req.Action != "login" {
		response.BadRequest(c, "action must be one of: send_code, login")
		return
	}
	if strings.TrimSpace(req.Phone) == "" {
		response.BadRequest(c, "phone is required")
		return
	}

	ctx := c.Request.Context()
	if h.webPlatformAutoLogin == nil {
		response.Error(c, http.StatusServiceUnavailable, "web auto-login service unavailable")
		return
	}

	// 既有账号 / 临时账号：取到完整账号对象后按 access_mode 隔离，拒绝把普通 API
	// 账号当 Web 账号登录（与 WebLoginPassword 同语义）。
	account := &service.Account{
		Platform:    req.Platform,
		Credentials: map[string]any{"access_mode": service.AccountAccessModeWeb},
	}
	if req.AccountID != nil {
		existing, err := h.adminService.GetAccount(ctx, *req.AccountID)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		if existing != nil && !existing.IsWebAccessMode() {
			response.ErrorWithDetails(c, http.StatusBadRequest, "not a web access-mode account", "",
				map[string]string{"hint": "web login is only supported for web access-mode accounts"})
			return
		}
		if existing != nil {
			account = existing
		}
	}

	challenge := req.challenge()

	if req.Action == "send_code" {
		_, err := h.webPlatformAutoLogin.SendSmsCode(ctx, req.Platform, req.Phone, challenge, account)
		if err != nil {
			respondWebLoginSMSFailure(c, req.Platform, err)
			return
		}
		// 发码成功无需回传客户端 token（zhipu 无 session_token；kimi SendVerifyCodeResponse 空消息，E0 取证）。
		response.Success(c, gin.H{"success": true})
		return
	}

	// action == login
	res, err := h.webPlatformAutoLogin.VerifySmsCode(ctx, req.Platform, req.Phone, req.SMSCode, challenge, account)
	if err != nil {
		respondWebLoginSMSFailure(c, req.Platform, err)
		return
	}
	updates := buildSMSLoginCredentialUpdates(req.Platform, req.Phone, res)
	// 服务端再次执行平台凭证准入校验：未取得平台必需凭证（zhipu cookie / kimi
	// access_token）时失败关闭，绝不落库半成品网页账号（计划 §2/§5.2）。
	if err := service.ValidateWebAccountCredential(req.Platform, service.AccountTypeAPIKey, updates); err != nil {
		respondWebLoginSMSFailure(c, req.Platform, err)
		return
	}

	var accountID int64
	if req.AccountID != nil {
		accountID = *req.AccountID
		if err := h.webPlatformAutoLoginStoreUpdateCreds(ctx, accountID, updates); err != nil {
			response.ErrorFrom(c, err)
			return
		}
		if err := h.webPlatformAutoLoginStoreStatus(ctx, accountID, service.StatusActive, ""); err != nil {
			response.ErrorFrom(c, err)
			return
		}
	} else {
		name := strings.TrimSpace(req.Name)
		if name == "" {
			name = req.Platform + "-web-" + strings.TrimSpace(req.Phone)
		}
		created, err := h.adminService.CreateAccount(ctx, &service.CreateAccountInput{
			Name:        name,
			Platform:    req.Platform,
			Type:        service.AccountTypeAPIKey,
			Credentials: updates,
		})
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		accountID = created.ID
	}

	// 凭据仅在此成功响应一次性回传；不写日志。
	data := gin.H{"success": true, "account_id": accountID}
	if req.Platform == service.PlatformZhipu {
		if res.Cookie != "" {
			data["cookie"] = res.Cookie
		}
	} else if res.AccessToken != "" {
		data["access_token"] = res.AccessToken
	}
	if res.LoginRefreshToken != "" {
		data["login_refresh_token"] = res.LoginRefreshToken
	}
	response.Success(c, data)
}

// buildSMSLoginCredentialUpdates 组装短信登录成功的账号凭据更新（同一次写入）：
//   - zhipu：整串 cookie（webAutoLoginCookieKey）+ 显式 login_refresh_token（从 cookie
//     提取 chatglm_refresh_token；缺失时仅记 cookie，续期依赖 refreshZhipu 的 cookie 兜底）；
//   - kimi：access_token（既有转发键）+ login_refresh_token = RefreshToken（cookie 键不写）。
//
// refresh_token 为转发链既有业务键（forward 401 续期读取），与 login_* 簿记键一并写入。
func buildSMSLoginCredentialUpdates(platform, phone string, res *service.SMSLoginResult) map[string]any {
	updates := map[string]any{
		"access_mode":                    service.AccountAccessModeWeb,
		"login_phone":                    strings.TrimSpace(phone),
		service.CredKeyLoginLastAt:       time.Now().UTC().Format(time.RFC3339),
		service.CredKeyLoginFailCount:    0,
		service.CredKeyLoginLastError:    "",
		service.CredKeyLoginNonRetryable: "false",
	}
	if platform == service.PlatformZhipu {
		updates[webAutoLoginCookieKey] = res.Cookie
	} else {
		updates["access_token"] = res.AccessToken
	}
	if res.LoginRefreshToken != "" {
		updates[service.CredKeyLoginRefreshToken] = res.LoginRefreshToken
		updates["refresh_token"] = res.LoginRefreshToken
	}
	return updates
}

// respondWebLoginSMSFailure 把失败关闭错误映射为细化响应：Kind=WAF 时附
// needs_challenge=true，并补充分平台挑战回填提示（zhipu 需 captcha_rid/md5/phone_code；
// kimi 需 captcha_validate），提示前端经本机浏览器人工验证后随同一次请求回传挑战值。
// 绝不暴露凭据值。
func respondWebLoginSMSFailure(c *gin.Context, platform string, err error) {
	kind, code, _ := service.WebLoginErrorKind(err)
	title, hint := service.WebPlatformErrorDetail(platform, code, kind)
	if title == "" {
		title = "网页登录失败"
	}
	details := map[string]string{"detail": err.Error()}
	if hint != "" {
		details["hint"] = hint
	}
	if kind == service.WebLoginKindWAF {
		details["needs_challenge"] = "true"
		// 平台特定挑战回填提示：用户在本机浏览器完成验证后随请求回传。
		switch platform {
		case service.PlatformZhipu:
			details["challenge_hint"] = "请在浏览器完成数美滑块验证，回填 zhipu_captcha_rid、zhipu_captcha_md5、zhipu_phone_code 后重试"
		case service.PlatformKimi:
			details["challenge_hint"] = "请在浏览器完成易盾验证，回填 kimi_captcha_validate 后重试"
		}
	}
	response.ErrorWithDetails(c, http.StatusBadRequest, title, "", details)
}

// webPlatformAutoLoginStoreUpdateCreds / webPlatformAutoLoginStoreStatus 是对
// 注入的自动登录服务（或适配后的 AdminService）凭据/状态写入的薄封装，便于在
// handler 内统一调用而无需关心底层实现细节。
func (h *AccountHandler) webPlatformAutoLoginStoreUpdateCreds(ctx context.Context, id int64, creds map[string]any) error {
	// 优先走注入的自动登录服务（若其实现了凭据写入），否则由 handler 内的 adapter 完成。
	// 此处统一经由 adminService 适配写入，保证与 UpdateAccountStatus 一致。
	account, err := h.adminService.GetAccount(ctx, id)
	if err != nil {
		return err
	}
	merged := make(map[string]any, len(account.Credentials)+len(creds))
	for k, v := range account.Credentials {
		merged[k] = v
	}
	for k, v := range creds {
		merged[k] = v
	}
	_, err = h.adminService.UpdateAccount(ctx, id, &service.UpdateAccountInput{Credentials: merged})
	return err
}

func (h *AccountHandler) webPlatformAutoLoginStoreStatus(ctx context.Context, id int64, status, errMsg string) error {
	if _, err := h.adminService.UpdateAccount(ctx, id, &service.UpdateAccountInput{Status: status}); err != nil {
		return err
	}
	if errMsg != "" {
		return h.adminService.SetAccountError(ctx, id, errMsg)
	}
	if status == service.StatusActive {
		if _, err := h.adminService.ClearAccountError(ctx, id); err != nil {
			return err
		}
	}
	return nil
}
