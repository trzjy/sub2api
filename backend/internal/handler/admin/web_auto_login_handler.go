package admin

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
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
// WebLoginChallenge：管理员人工挑战会话。
// ---------------------------------------------------------------------------

type webLoginChallengeBindingRequest struct {
	Platform     string                `json:"platform" binding:"required"`
	Phone        string                `json:"phone" binding:"required"`
	Stage        string                `json:"stage"`
	AccountID    *int64                `json:"account_id"`
	AccountDraft *CreateAccountRequest `json:"account_draft"`
}

func (h *AccountHandler) WebLoginChallengeStart(c *gin.Context) {
	var req webLoginChallengeBindingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "请求参数不完整或格式错误")
		return
	}
	adminID, ok := webLoginChallengeAdminID(c)
	if !ok {
		return
	}
	platform, phone, stage, proxyID, ok := h.validateWebLoginChallengeBinding(c, req)
	if !ok {
		return
	}
	if h.webLoginChallengeStore == nil {
		response.Error(c, http.StatusServiceUnavailable, "web login challenge service unavailable")
		return
	}
	sess, err := h.webLoginChallengeStore.Create(service.WebLoginChallengeCreateInput{
		AdminID: adminID, Platform: platform, Phone: phone, Stage: stage, AccountID: req.AccountID,
		AccountDraft: req.AccountDraft, ProxyID: proxyID,
	})
	if err != nil {
		respondWebLoginChallengeError(c, http.StatusBadRequest, err)
		return
	}
	if h.webLoginCaptchaHelper == nil {
		_ = h.webLoginChallengeStore.SetStatus(sess.ID, adminID, "context_gap")
		respondWebLoginChallengeContextGap(c, sess)
		return
	}
	helperSession, err := h.webLoginCaptchaHelper.Start(c.Request.Context(), platform, sess.ID, phone, "")
	if err != nil || strings.TrimSpace(helperSession.ID) == "" {
		_ = h.webLoginChallengeStore.SetStatus(sess.ID, adminID, "context_gap")
		respondWebLoginChallengeContextGap(c, sess)
		return
	}
	if err := h.webLoginChallengeStore.SetHelperSessionID(sess.ID, adminID, helperSession.ID); err != nil {
		respondWebLoginChallengeError(c, http.StatusBadRequest, err)
		return
	}
	response.Success(c, gin.H{"success": true, "session_id": sess.ID, "status": "pending"})
}

func respondWebLoginChallengeContextGap(c *gin.Context, sess *service.WebLoginChallengeSession) {
	response.ErrorWithDetails(c, http.StatusNotImplemented, "人工挑战上下文不足", "challenge_session", map[string]string{
		"context_gap": "true", "status": "context_gap", "session_id": sess.ID,
		"platform": sess.Platform, "stage": sess.Stage, "expires_at": sess.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

func (h *AccountHandler) WebLoginChallengeStatus(c *gin.Context) {
	adminID, ok := webLoginChallengeAdminID(c)
	if !ok {
		return
	}
	if h.webLoginChallengeStore == nil {
		response.Error(c, http.StatusServiceUnavailable, "web login challenge service unavailable")
		return
	}
	sess, err := h.webLoginChallengeStore.Status(c.Param("session_id"), adminID)
	if err != nil {
		respondWebLoginChallengeError(c, http.StatusNotFound, err)
		return
	}
	if h.webLoginCaptchaHelper != nil && sess.HelperSessionID != "" && sess.Status == "pending" {
		helperStatus, helperErr := h.webLoginCaptchaHelper.Status(c.Request.Context(), service.LocalCaptchaHelperSession{ID: sess.HelperSessionID, LoginSessionID: sess.ID}, sess.Platform, sess.ID, sess.Phone)
		if helperErr != nil {
			_ = h.webLoginChallengeStore.SetStatus(sess.ID, adminID, "context_gap")
			respondWebLoginChallengeContextGap(c, sess)
			return
		}
		mapped := mapLocalCaptchaStatus(helperStatus)
		if mapped == "context_gap" {
			_ = h.webLoginChallengeStore.SetStatus(sess.ID, adminID, mapped)
			respondWebLoginChallengeContextGap(c, sess)
			return
		}
		if mapped != sess.Status {
			if err := h.webLoginChallengeStore.SetStatus(sess.ID, adminID, mapped); err != nil {
				respondWebLoginChallengeError(c, http.StatusBadRequest, err)
				return
			}
			sess.Status = mapped
		}
	}
	response.Success(c, gin.H{
		"session_id": sess.ID, "platform": sess.Platform, "status": sess.Status,
		"context_gap": sess.Status == "context_gap", "expires_at": sess.ExpiresAt,
	})
}

func localCaptchaResultChallenge(platform string, data map[string]string) (service.WebSMSChallenge, error) {
	challenge := service.WebSMSChallenge{}
	switch platform {
	case service.PlatformZhipu:
		challenge.ZhipuCaptchaRid = strings.TrimSpace(data["rid"])
		challenge.ZhipuCaptchaMD5 = strings.TrimSpace(data["md5"])
		challenge.ZhipuPhoneCode = strings.TrimSpace(data["phone_code"])
		if challenge.ZhipuCaptchaRid == "" || challenge.ZhipuCaptchaMD5 == "" || challenge.ZhipuPhoneCode == "" {
			return service.WebSMSChallenge{}, errors.New("incomplete zhipu helper result")
		}
	case service.PlatformKimi:
		challenge.KimiCaptchaValidate = strings.TrimSpace(data["validate"])
		if challenge.KimiCaptchaValidate == "" {
			return service.WebSMSChallenge{}, errors.New("incomplete kimi helper result")
		}
	default:
		return service.WebSMSChallenge{}, errors.New("unsupported helper platform")
	}
	return challenge, nil
}

func mapLocalCaptchaStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "pending", "running":
		return "pending"
	case "ok", "succeeded":
		return "succeeded"
	case "context_gap", "failed", "expired":
		return "context_gap"
	default:
		return "context_gap"
	}
}

func (h *AccountHandler) WebLoginChallengeConsume(c *gin.Context) {
	var req webLoginChallengeBindingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	adminID, ok := webLoginChallengeAdminID(c)
	if !ok {
		return
	}
	if h.webLoginChallengeStore == nil {
		response.Error(c, http.StatusServiceUnavailable, "web login challenge service unavailable")
		return
	}
	platform := strings.ToLower(strings.TrimSpace(req.Platform))
	phone := strings.TrimSpace(req.Phone)
	if platform != service.PlatformZhipu && platform != service.PlatformKimi {
		response.BadRequest(c, "platform must be one of: zhipu, kimi")
		return
	}
	if phone == "" {
		response.BadRequest(c, "phone is required")
		return
	}
	sessionID := c.Param("session_id")
	claim, err := h.webLoginChallengeStore.BeginConsume(sessionID, adminID, platform, phone)
	if err != nil {
		respondWebLoginChallengeError(c, http.StatusBadRequest, err)
		return
	}
	claimFinished := false
	defer func() {
		if !claimFinished {
			_ = h.webLoginChallengeStore.FinishConsume(sessionID, claim.Token, false)
		}
	}()
	if h.webLoginCaptchaHelper == nil || strings.TrimSpace(claim.HelperSessionID) == "" {
		_ = h.webLoginChallengeStore.CloseConsumeContextGap(sessionID, claim.Token)
		claimFinished = true
		respondWebLoginChallengeContextGap(c, &service.WebLoginChallengeSession{ID: sessionID})
		return
	}
	result, err := h.webLoginCaptchaHelper.Result(c.Request.Context(), service.LocalCaptchaHelperSession{ID: claim.HelperSessionID, LoginSessionID: sessionID}, platform, sessionID, phone)
	if err != nil || mapLocalCaptchaStatus(result.Status) != "succeeded" {
		_ = h.webLoginChallengeStore.CloseConsumeContextGap(sessionID, claim.Token)
		claimFinished = true
		respondWebLoginChallengeContextGap(c, &service.WebLoginChallengeSession{ID: sessionID})
		return
	}
	challenge, err := localCaptchaResultChallenge(platform, result.Data)
	if err != nil {
		_ = h.webLoginChallengeStore.CloseConsumeContextGap(sessionID, claim.Token)
		claimFinished = true
		respondWebLoginChallengeContextGap(c, &service.WebLoginChallengeSession{ID: sessionID})
		return
	}
	if err := h.webLoginChallengeStore.SetConsumeResult(sessionID, claim.Token, challenge); err != nil {
		respondWebLoginChallengeError(c, http.StatusBadRequest, err)
		return
	}
	if err := h.webLoginChallengeStore.FinishConsume(sessionID, claim.Token, true); err != nil {
		respondWebLoginChallengeError(c, http.StatusBadRequest, err)
		return
	}
	claimFinished = true
	response.Success(c, gin.H{"success": true, "session_id": sessionID, "status": "succeeded"})
}

func webLoginChallengeAdminID(c *gin.Context) (int64, bool) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		response.Error(c, http.StatusUnauthorized, "authenticated administrator required")
		return 0, false
	}
	return subject.UserID, true
}

func (h *AccountHandler) validateWebLoginChallengeBinding(c *gin.Context, req webLoginChallengeBindingRequest) (string, string, string, *int64, bool) {
	platform := strings.ToLower(strings.TrimSpace(req.Platform))
	if platform != service.PlatformZhipu && platform != service.PlatformKimi {
		response.BadRequest(c, "platform must be one of: zhipu, kimi")
		return "", "", "", nil, false
	}
	phone := strings.TrimSpace(req.Phone)
	if phone == "" {
		response.BadRequest(c, "phone is required")
		return "", "", "", nil, false
	}
	stage := strings.ToLower(strings.TrimSpace(req.Stage))
	if stage == "" {
		stage = "send_code"
	}
	if stage != "send_code" && stage != "login" {
		response.BadRequest(c, "stage must be one of: send_code, login")
		return "", "", "", nil, false
	}
	if req.AccountID == nil && req.AccountDraft == nil {
		response.BadRequest(c, "account_draft is required when account_id is omitted")
		return "", "", "", nil, false
	}
	if req.AccountDraft != nil && strings.TrimSpace(req.AccountDraft.Platform) != "" && req.AccountDraft.Platform != platform {
		response.BadRequest(c, "account_draft.platform must match platform")
		return "", "", "", nil, false
	}
	var proxyID *int64
	if req.AccountID != nil && h.adminService != nil {
		account, err := h.adminService.GetAccount(c.Request.Context(), *req.AccountID)
		if err != nil {
			response.ErrorFrom(c, err)
			return "", "", "", nil, false
		}
		if account == nil || account.Platform != platform || !account.IsWebAccessMode() {
			response.BadRequest(c, "account platform and access mode must match challenge")
			return "", "", "", nil, false
		}
		proxyID = account.ProxyID
	} else if req.AccountDraft != nil {
		proxyID = req.AccountDraft.ProxyID
	}
	return platform, phone, stage, proxyID, true
}

func respondWebLoginChallengeError(c *gin.Context, fallbackStatus int, err error) {
	status := fallbackStatus
	metadata := map[string]string{"detail": err.Error()}
	switch {
	case errors.Is(err, service.ErrWebLoginChallengeContext):
		status = http.StatusNotImplemented
		metadata["context_gap"] = "true"
		metadata["status"] = "context_gap"
	case errors.Is(err, service.ErrWebLoginChallengeExpired), errors.Is(err, service.ErrWebLoginChallengeNotFound):
		status = http.StatusNotFound
	case errors.Is(err, service.ErrWebLoginChallengeConsumed), errors.Is(err, service.ErrWebLoginChallengeMismatch), errors.Is(err, service.ErrWebLoginChallengeNotReady), errors.Is(err, service.ErrWebLoginChallengeInFlight):
		status = http.StatusBadRequest
	}
	response.ErrorWithDetails(c, status, "人工挑战失败", "challenge_session", metadata)
}

// ---------------------------------------------------------------------------
// WebLoginPassword 单账号自动登录入口（CreateAccountModal 未建账号时用）。
// POST /api/v1/admin/accounts/web-login-password
//
//	body {platform, login_email, login_password, login_phone?, account_id?}
//
// 安全：成功响应仅返回 success/account_id；登录凭据只写入账号，不回传给客户端。
func (h *AccountHandler) WebLoginPassword(c *gin.Context) {
	var req struct {
		Platform      string                `json:"platform" binding:"required"`
		LoginEmail    string                `json:"login_email"`
		LoginPhone    string                `json:"login_phone"`
		LoginPassword string                `json:"login_password"`
		AccountID     *int64                `json:"account_id"`
		AccountDraft  *CreateAccountRequest `json:"account_draft"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	req.Platform = strings.ToLower(strings.TrimSpace(req.Platform))
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
		loginCreds[service.CredKeyLoginEmail] = req.LoginEmail
	}
	if req.LoginPhone != "" {
		loginCreds["login_phone"] = req.LoginPhone
	}
	if req.LoginPassword != "" {
		loginCreds[service.CredKeyLoginPassword] = req.LoginPassword
	}

	var account *service.Account
	if req.AccountID == nil {
		if req.AccountDraft == nil {
			response.BadRequest(c, "account_draft is required when account_id is omitted")
			return
		}
		draftPlatform := strings.ToLower(strings.TrimSpace(req.AccountDraft.Platform))
		if draftPlatform != req.Platform {
			response.BadRequest(c, "account_draft.platform must match platform")
			return
		}
		account = &service.Account{
			Platform:    draftPlatform,
			Type:        req.AccountDraft.Type,
			Credentials: mergeWebLoginCredentials(req.AccountDraft.Credentials, loginCreds),
			Extra:       cloneWebLoginMap(req.AccountDraft.Extra),
			ProxyID:     req.AccountDraft.ProxyID,
			Concurrency: req.AccountDraft.Concurrency,
		}
		// 服务端强制兜底：不依赖前端 draft 携带 access_mode，保证 LoginByEmail 内
		// GetWebBaseURL()（依赖 IsWebAccessMode）总能取到默认官方域名。
		account.Credentials["access_mode"] = service.AccountAccessModeWeb
	} else {
		existing, err := h.adminService.GetAccount(ctx, *req.AccountID)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		// 单账号入口已取到完整账号对象：归并后同平台可共存普通 API 账号与 Web
		// 账号，故此处按账号级 access_mode 隔离，拒绝把普通 API 账号当 Web 账号登录。
		if existing == nil || strings.ToLower(strings.TrimSpace(existing.Platform)) != req.Platform {
			response.BadRequest(c, "account platform must match platform")
			return
		}
		if !existing.IsWebAccessMode() {
			response.ErrorWithDetails(c, http.StatusBadRequest, "not a web access-mode account", "",
				map[string]string{"hint": "web login is only supported for web access-mode accounts"})
			return
		}
		accountCopy := *existing
		accountCopy.Platform = req.Platform
		accountCopy.Credentials = mergeWebLoginCredentials(existing.Credentials, loginCreds)
		account = &accountCopy
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
		if req.AccountID != nil {
			updateCreds := map[string]any{webAutoLoginCookieKey: cookie}
			if deviceID := strings.TrimSpace(account.GetCredential(service.CredKeyLoginDeviceID)); deviceID != "" {
				updateCreds[service.CredKeyLoginDeviceID] = deviceID
			}
			if req.LoginEmail != "" {
				updateCreds[service.CredKeyLoginEmail] = req.LoginEmail
			}
			if req.LoginPhone != "" {
				updateCreds["login_phone"] = req.LoginPhone
			}
			if req.LoginPassword != "" {
				updateCreds[service.CredKeyLoginPassword] = req.LoginPassword
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
		if req.AccountID == nil {
			createdCreds := mergeWebLoginCredentials(account.Credentials, nil)
			createdCreds[webAutoLoginCookieKey] = cookie
			if deviceID := strings.TrimSpace(account.GetCredential(service.CredKeyLoginDeviceID)); deviceID != "" {
				createdCreds[service.CredKeyLoginDeviceID] = deviceID
			}
			created, err := h.adminService.CreateAccount(ctx, &service.CreateAccountInput{
				Name: req.AccountDraft.Name, Notes: req.AccountDraft.Notes, Platform: req.Platform,
				Type: req.AccountDraft.Type, Credentials: createdCreds,
				Extra: req.AccountDraft.Extra, ProxyID: req.AccountDraft.ProxyID, Concurrency: req.AccountDraft.Concurrency,
				Priority: req.AccountDraft.Priority, RateMultiplier: req.AccountDraft.RateMultiplier, LoadFactor: req.AccountDraft.LoadFactor,
				GroupIDs: req.AccountDraft.GroupIDs, ExpiresAt: req.AccountDraft.ExpiresAt,
				AutoPauseOnExpired: req.AccountDraft.AutoPauseOnExpired, ProbeEnabled: req.AccountDraft.ProbeEnabled,
			})
			if err != nil {
				response.ErrorFrom(c, err)
				return
			}
			c.JSON(http.StatusOK, gin.H{"success": true, "account_id": created.ID})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "account_id": *req.AccountID})
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

func mergeWebLoginCredentials(base, login map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(login))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range login {
		out[k] = v
	}
	return out
}

func cloneWebLoginMap(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------------------
// WebLoginSMS 网页版平台手机号 + 短信码登录（zhipu / kimi 唯一用户入口）。
// POST /api/v1/admin/accounts/web-login-sms
//
//	action=send_code: {action, platform, phone, challenge_session_id}
//	  → {success:true}
//	action=login:     {action, platform, phone, sms_code, challenge_session_id}
//	  → {success:true, account_id}
//
// 挑战处理：客户端只提交 opaque challenge_session_id。挑战值由服务端会话绑定管理员、平台和手机号，
// 通过一次性原子取出提供给平台登录服务；原始 rid/md5/validate 字段不进入请求 DTO。
// 缺失会话、绑定不匹配或上游失败均失败关闭，绝不伪造成功。
//
// 落库：登录成功后在 handler 中把本次所得凭据一次性写入账号；无 account_id 时先校验平台必需凭证
// 再 CreateAccount，失败绝不落库。
// 安全：成功响应仅返回 success/account_id，绝不回传 cookie、access_token 或 refresh token。
// ---------------------------------------------------------------------------

// webLoginSMSRequest 是 POST /web-login-sms 的请求体。
type webLoginSMSRequest struct {
	Action   string `json:"action" binding:"required"`
	Platform string `json:"platform" binding:"required"`
	Phone    string `json:"phone"`

	SMSCode            string `json:"sms_code"`
	ChallengeSessionID string `json:"challenge_session_id" binding:"required"`
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

	adminID, ok := webLoginChallengeAdminID(c)
	if !ok {
		return
	}
	if h.webLoginChallengeStore == nil {
		response.Error(c, http.StatusServiceUnavailable, "web login challenge service unavailable")
		return
	}
	challengeSessionID := strings.TrimSpace(req.ChallengeSessionID)

	var claim *service.WebLoginChallengeClaim
	var err error
	if req.Action == "send_code" {
		claim, err = h.webLoginChallengeStore.BeginSendCode(challengeSessionID, adminID, req.Platform, req.Phone)
	} else {
		claim, err = h.webLoginChallengeStore.BeginLogin(challengeSessionID, adminID, req.Platform, req.Phone)
	}
	if err != nil {
		respondWebLoginChallengeError(c, http.StatusBadRequest, err)
		return
	}
	claimFinished := false
	defer func() {
		if claimFinished {
			return
		}
		if req.Action == "send_code" {
			_ = h.webLoginChallengeStore.FinishSendCode(challengeSessionID, claim.Token, false)
		} else {
			_ = h.webLoginChallengeStore.FinishLogin(challengeSessionID, claim.Token, false)
		}
	}()

	// 账号目标只取自 challenge start 时绑定的服务端会话，SMS 请求中的多余字段不会参与选择。
	account := &service.Account{
		Platform:    req.Platform,
		Credentials: map[string]any{"access_mode": service.AccountAccessModeWeb},
		ProxyID:     claim.ProxyID,
	}
	var accountDraft *CreateAccountRequest
	if claim.AccountID != nil {
		existing, getErr := h.adminService.GetAccount(ctx, *claim.AccountID)
		if getErr != nil {
			response.ErrorFrom(c, getErr)
			return
		}
		if existing == nil || existing.Platform != req.Platform || !existing.IsWebAccessMode() {
			response.ErrorWithDetails(c, http.StatusBadRequest, "not a web access-mode account", "",
				map[string]string{"hint": "web login is only supported for web access-mode accounts"})
			return
		}
		account = existing
	} else {
		accountDraft, ok = claim.AccountDraft.(*CreateAccountRequest)
		if !ok || accountDraft == nil {
			respondWebLoginChallengeError(c, http.StatusBadRequest, service.ErrWebLoginChallengeMismatch)
			return
		}
		account.Name = accountDraft.Name
		account.Type = accountDraft.Type
		account.Credentials = mergeWebLoginCredentials(accountDraft.Credentials, nil)
		account.Extra = accountDraft.Extra
		account.ProxyID = claim.ProxyID
		account.Concurrency = accountDraft.Concurrency
	}

	if req.Action == "send_code" {
		if _, err := h.webPlatformAutoLogin.SendSmsCode(ctx, req.Platform, req.Phone, claim.Challenge, account); err != nil {
			respondWebLoginSMSFailure(c, req.Platform, err)
			return
		}
		if err := h.webLoginChallengeStore.FinishSendCode(challengeSessionID, claim.Token, true); err != nil {
			respondWebLoginChallengeError(c, http.StatusBadRequest, err)
			return
		}
		claimFinished = true
		// 发码成功无需回传客户端 token（zhipu 无 session_token；kimi SendVerifyCodeResponse 空消息，E0 取证）。
		response.Success(c, gin.H{"success": true})
		return
	}

	res, err := h.webPlatformAutoLogin.VerifySmsCode(ctx, req.Platform, req.Phone, req.SMSCode, claim.Challenge, account)
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
	if claim.AccountID != nil {
		accountID = *claim.AccountID
		if err := h.webPlatformAutoLoginStoreUpdateCreds(ctx, accountID, updates); err != nil {
			response.ErrorFrom(c, err)
			return
		}
		if err := h.webPlatformAutoLoginStoreStatus(ctx, accountID, service.StatusActive, ""); err != nil {
			response.ErrorFrom(c, err)
			return
		}
	} else {
		createdCreds := mergeWebLoginCredentials(accountDraft.Credentials, updates)
		created, err := h.adminService.CreateAccount(ctx, &service.CreateAccountInput{
			Name: accountDraft.Name, Notes: accountDraft.Notes, Platform: accountDraft.Platform,
			Type: accountDraft.Type, Credentials: createdCreds,
			Extra: accountDraft.Extra, ProxyID: claim.ProxyID, Concurrency: accountDraft.Concurrency,
			Priority: accountDraft.Priority, GroupIDs: accountDraft.GroupIDs, ExpiresAt: accountDraft.ExpiresAt,
		})
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		accountID = created.ID
	}

	if err := h.webLoginChallengeStore.FinishLogin(challengeSessionID, claim.Token, true); err != nil {
		respondWebLoginChallengeError(c, http.StatusBadRequest, err)
		return
	}
	claimFinished = true
	c.JSON(http.StatusOK, gin.H{"success": true, "account_id": accountID})
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

// respondWebLoginSMSFailure 将失败关闭错误映射为细化响应：Kind=WAF 时附 needs_challenge=true，
// 但不提示客户端回传原始挑战字段；客户端应重新创建 challenge session。绝不暴露凭据值。
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
		details["challenge_hint"] = "请重新创建 challenge session 后重试"
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
