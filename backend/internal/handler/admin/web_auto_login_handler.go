package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// webAutoLoginCookieKey 是网页版账号凭据中存放整串 Cookie 头的键名（与
// web_deepseek_gateway_forward / web_zhipu_gateway_forward 读取的键一致）。
const webAutoLoginCookieKey = "cookie"

// webPlatformAutoLoginService 是网页版平台自动登录服务的本地接口，便于测试替换。
// 其方法签名与 service.WebPlatformAutoLoginService 对齐。
type webPlatformAutoLoginService interface {
	LoginByEmail(ctx context.Context, account *service.Account) (string, error)
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
	_, err = a.svc.UpdateAccount(ctx, id, &service.UpdateAccountInput{Credentials: merged, FromWebLogin: true})
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
	// zhipu 人工挑战需要国家码作为 phone_code（helper 侧 client.Start 对 PlatformZhipu
	// 校验非空，否则返回 "GLM challenge requires phone_code" → context_gap 501）；
	// kimi 不需要，保持空串。SplitSMSPhone 支持 "86-138..."/"+86 138..."/纯 11 位默认 "86"。
	helperPhoneCode := ""
	// Lane G（任务卡 2026-09-22）：zhipu 发码由 helper 在同会话内执行，sdk-start body
	// 下发签名三件套（webZhipuComputeSign 已取证实现；kimi 传零值，body 不含对应键，
	// 流程零改动）。三件套一次性即时生成，不缓存。
	signParams := service.LocalCaptchaHelperSignParams{}
	if platform == service.PlatformZhipu {
		cc, _ := service.SplitSMSPhone(phone)
		helperPhoneCode = cc
		ts, nonce, sign := service.WebZhipuComputeSignForHelper()
		signParams = service.LocalCaptchaHelperSignParams{XTimestamp: ts, XNonce: nonce, XSign: sign}
	}
	helperSession, err := h.webLoginCaptchaHelper.Start(c.Request.Context(), platform, sess.ID, phone, helperPhoneCode, signParams)
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

// localCaptchaResultChallenge 把 helper result.Data 组装为服务端 challenge 值。
// Lane G（任务卡 2026-09-22）zhipu 扩展字段消费：
//   - send_status/send_body_status（int 形态字符串）：helper 同会话发码结果；
//     send_status 存在即以 helper 发码为准（后端不再重复发码），status==0 成功；
//     字段缺失（Lane H 未上线）保持 nil → 后端直发路径，绝不因字段缺失而失败；
//   - cookies/device_id：同会话匿名 Cookie 串与站点 device_id，仅存 challenge session
//     内存（TTL 同既有），登录步复用；一旦存在必须使用。
//
// 零凭据红线：字段值只进内存会话，绝不写日志；解析失败按字段缺失处理（不失败）。
func localCaptchaResultChallenge(platform string, data map[string]string) (service.WebSMSChallenge, error) {
	challenge := service.WebSMSChallenge{}
	switch platform {
	case service.PlatformZhipu:
		challenge.ZhipuCaptchaRid = strings.TrimSpace(data["rid"])
		challenge.ZhipuCaptchaMD5 = strings.TrimSpace(data["md5"])
		challenge.ZhipuPhoneCode = strings.TrimSpace(data["phone_code"])
		// md5 可选（2026-09-22 chatglm.cn 线上取证：官方滑块 onSuccess 仅回调
		// {rid, pass}，md5 是落地链接 query 可选参数，正常滑块流不带）；
		// rid 必填失败关闭不变。
		if challenge.ZhipuCaptchaRid == "" || challenge.ZhipuPhoneCode == "" {
			return service.WebSMSChallenge{}, errors.New("incomplete zhipu helper result")
		}
		// Lane G 扩展消费：send_status/send_body_status（宽容解析，失败按缺失处理）。
		if v, ok := localCaptchaIntField(data, "send_status"); ok {
			challenge.ZhipuHelperSendStatus = &v
		}
		if v, ok := localCaptchaIntField(data, "send_body_status"); ok {
			challenge.ZhipuHelperSendBodyStatus = &v
		}
		challenge.ZhipuHelperSendMessage = strings.TrimSpace(data["send_message"])
		challenge.ZhipuSessionCookie = strings.TrimSpace(data["cookies"])
		challenge.ZhipuDeviceID = strings.TrimSpace(data["device_id"])
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

// localCaptchaIntField 从 helper result data（map[string]string）宽容解析整数字段：
// 空串/非数字按字段缺失处理（Lane H 未上线兼容，绝不因字段形态异常而失败）。
func localCaptchaIntField(data map[string]string, key string) (int, bool) {
	raw := strings.TrimSpace(data[key])
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return n, true
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
				FromWebLogin: true,
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
			RateMultiplier: accountDraft.RateMultiplier, LoadFactor: accountDraft.LoadFactor,
			AutoPauseOnExpired: accountDraft.AutoPauseOnExpired, ProbeEnabled: accountDraft.ProbeEnabled,
			FromWebLogin: true,
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
//     提取 chatglm_refresh_token；强制要求：内核侧缺失即失败关闭，无 cookie 兜底语义）；
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
	_, err = h.adminService.UpdateAccount(ctx, id, &service.UpdateAccountInput{Credentials: merged, FromWebLogin: true})
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

// ---------------------------------------------------------------------------
// DeepSeek 邮箱验证码注册自动接入（deepseek-email-register-plan.md §1.2，任务卡 T1）
// POST /api/v1/admin/accounts/web-register-email-code
// POST /api/v1/admin/accounts/web-register
//
// 两个端点的出站与建号全程包在 executeAdminIdempotent 幂等闭包内（外审 F1）；
// 登录会话去重在幂等闭包内显式调用 duplicateWebCredentialAccount（外审 F2）；
// 代理显式解析 Proxy 对象取 URL，查不到即 400，不静默回退（外审 F3）；注册成功后的
// 任何失败统一 F4 文案，不得让用户重试注册（上游会返回 EMAIL_EXISTS）。
// 安全：成功响应仅返回 success/account_id/send_window_secs；凭据不回传客户端。
// ---------------------------------------------------------------------------

// webDeepseekRegisterService 是 DeepSeek 邮箱注册链所需的 service 能力（真实现为
// *service.WebPlatformAutoLoginService）。以接口断言获取，避免改动既有
// webPlatformAutoLoginService 本地接口（其既有测试桩位于本任务白名单之外的文件）。
type webDeepseekRegisterService interface {
	SendRegisterEmailCode(ctx context.Context, email, locale, deviceID, scenario, proxyURL string) (int64, error)
	RegisterByEmail(ctx context.Context, email, code, password, region, deviceID, proxyURL string) error
}

// webDeepseekRegisterSvc 从注入的自动登录服务断言出注册链能力；未注入或未实现时返回 nil
// （handler 统一 503 失败关闭，与 WebLoginPassword 同口径）。
func (h *AccountHandler) webDeepseekRegisterSvc() webDeepseekRegisterService {
	if h.webPlatformAutoLogin == nil {
		return nil
	}
	if s, ok := h.webPlatformAutoLogin.(webDeepseekRegisterService); ok {
		return s
	}
	return nil
}

// webRegisterPostSuccessMessage F4 统一文案（方案 §1.2-4）：上游注册已成功之后的失败
// 一律返回本指引，不得让用户重试注册。
const webRegisterPostSuccessMessage = "账号已在 DeepSeek 注册成功，请使用邮箱+密码在『密码登录』入口完成创建"

// 外审 2026-09-22 P1-2：上游注册 biz_code=0 之后的失败（LoginByEmail / 去重检查 /
// CreateAccount）是终态——上游账号已存在，重试绝不能再打上游注册。因此闭包不把它
// 作为 error 返回（那会被协调器记 failed_retryable，退避后重试会重打上游拿到
// EMAIL_EXISTS），而是编码为可持久化的成功结果 webRegisterPostSuccessResult（走
// 协调器 succeeded 持久化路径）；闭包外由 respondWebRegisterPostSuccess 统一映射为
// F4 文案（HTTP 400）或 409 去重响应。同键重试时协调器直接重放该终态，不触上游。

// webRegisterPostSuccessResultKind 区分 post-success 终态结果的两类形态。
type webRegisterPostSuccessResultKind string

const (
	webRegisterPostSuccessKindFailure   webRegisterPostSuccessResultKind = "post_success_failure"   // F4：HTTP 400 + detail
	webRegisterPostSuccessKindDuplicate webRegisterPostSuccessResultKind = "post_success_duplicate" // 409 去重命中
	// webRegisterPostSuccessKindOutcomeUnknown 注册结果不明（外审 R3-P2）：注册 POST
	// 已出站但响应丢失，上游可能已建号。同样走终态持久化（HTTP 400 + post_success_register
	// 标记），同键重试直接重放，不自动重打注册。
	webRegisterPostSuccessKindOutcomeUnknown webRegisterPostSuccessResultKind = "post_success_outcome_unknown"
)

// webRegisterPostSuccessResult 幂等闭包的终态结果（走协调器 succeeded 持久化路径，
// 可重放）。Kind/DupAccountID/ErrorReason 固定键编码，重放解码后语义不变。
type webRegisterPostSuccessResult struct {
	Kind         webRegisterPostSuccessResultKind `json:"kind"`
	DupAccountID string                           `json:"dup_account_id,omitempty"`
	ErrorReason  string                           `json:"error_reason,omitempty"`
}

// validateWebRegisterEmail 邮箱基础格式校验（任务卡 §2.2-1：含 @，长度上限 254）。
func validateWebRegisterEmail(email string) bool {
	return strings.Contains(email, "@") && len(email) <= 254
}

// validateWebRegisterPassword 密码 ≥8 位且含字母+数字（上游规则，方案 §1.2-4 预校验前移）。
func validateWebRegisterPassword(password string) bool {
	if len(password) < 8 {
		return false
	}
	hasLetter, hasDigit := false, false
	for _, r := range password {
		switch {
		case unicode.IsLetter(r):
			hasLetter = true
		case unicode.IsDigit(r):
			hasDigit = true
		}
	}
	return hasLetter && hasDigit
}

// respondWebRegisterFailure 注册阶段（上游 biz_code=0 之前）失败的响应映射：
// webLoginHTTPError → 分类文案透传（人机验证/邮箱域名/地域门控等自带中文文案；
// WAF 用既有统一标题并附 needs_challenge）；其余错误走 ErrorFrom。
func respondWebRegisterFailure(c *gin.Context, err error) {
	kind, code, ok := service.WebLoginErrorKind(err)
	if !ok {
		response.ErrorFrom(c, err)
		return
	}
	title := err.Error()
	details := map[string]string{"detail": err.Error()}
	if kind == service.WebLoginKindWAF {
		title, _ = service.WebPlatformErrorDetail(service.PlatformDeepseek, code, kind)
		details["needs_challenge"] = "true"
	}
	response.ErrorWithDetails(c, http.StatusBadRequest, title, "", details)
}

// respondWebRegisterIdempotentError 两个注册端点共用的幂等闭包错误映射：
// 注册阶段（上游 biz_code=0 之前）失败按分类透传。post-success 终态不再走 error 路径
// （外审 2026-09-22 P1-2：编码为 webRegisterPostSuccessResult 成功结果持久化可重放），
// 由 respondWebRegisterOutcome 在闭包外统一映射 HTTP 响应。
func respondWebRegisterIdempotentError(c *gin.Context, err error) {
	if retryAfter := service.RetryAfterSecondsFromError(err); retryAfter > 0 {
		c.Header("Retry-After", strconv.Itoa(retryAfter))
	}
	respondWebRegisterFailure(c, err)
}

// respondWebRegisterPostSuccess 把 post-success 终态结果映射为 HTTP 响应：
// F4 文案（HTTP 400，metadata.detail 携带原始失败原因 + message 携带 F4 指引，
// 外审 P1-3：前端两个通道都要可读）；409 去重命中按既有文案。
func respondWebRegisterPostSuccess(c *gin.Context, res webRegisterPostSuccessResult) {
	switch res.Kind {
	case webRegisterPostSuccessKindDuplicate:
		respondWebCredentialDuplicate(c, res.DupAccountID)
	case webRegisterPostSuccessKindFailure, webRegisterPostSuccessKindOutcomeUnknown:
		response.ErrorWithDetails(c, http.StatusBadRequest, webRegisterPostSuccessMessage, "",
			map[string]string{"detail": res.ErrorReason, "post_success_register": "true"})
	default:
		response.ErrorWithDetails(c, http.StatusBadRequest, webRegisterPostSuccessMessage, "",
			map[string]string{"detail": "未知终态结果", "post_success_register": "true"})
	}
}

// WebRegisterEmailCode 处理 DeepSeek 邮箱注册发码（任务卡 §2.2-1；外审 R3-P4 契约收敛：
// 与方案 §1.2-3/验收 5 对齐——account_draft.proxy_id 非空时发码出站走该 Proxy 对象 URL，
// 查不到即 400 失败关闭；为空走默认出站（当前即香港服务器））。前端从 accountDraft 取
// proxy_id 随请求传入（发码时账号尚未创建，无法服务端自查绑定）。
// body {platform:"deepseek", email, proxy_id?} → {success:true, send_window_secs:N, device_id}；
// device_id 生成稳定 UUID，仅本次请求内使用并回传给前端不落库（建号时由 web-register
// 端点再生成并落 login_device_id）。
func (h *AccountHandler) WebRegisterEmailCode(c *gin.Context) {
	var req struct {
		Platform string `json:"platform" binding:"required"`
		Email    string `json:"email" binding:"required"`
		ProxyID  *int64 `json:"proxy_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	svc := h.webDeepseekRegisterSvc()
	if svc == nil {
		response.Error(c, http.StatusServiceUnavailable, "web auto-login service unavailable")
		return
	}

	// R6-P1：handler 层强制幂等键。发码是不可幂等外呼的上游写操作，而全局幂等协调器
	// 默认 ObserveOnly=true（idempotency.go: Execute 中 RequireKey 只在非 ObserveOnly
	// 时拒绝），缺键会直接执行且不保存结果——重试会重复触发上游发码。非法字符校验由
	// 协调器内 NormalizeIdempotencyKey 承担，此处仅拦截空串/纯空白。
	if strings.TrimSpace(c.GetHeader("Idempotency-Key")) == "" {
		response.ErrorFrom(c, service.ErrIdempotencyKeyRequired)
		return
	}

	result, err := executeAdminIdempotent(c, "admin.accounts.web-register-email-code", req,
		service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
			platform := strings.ToLower(strings.TrimSpace(req.Platform))
			if platform != service.PlatformDeepseek {
				return nil, infraerrors.BadRequest("WEB_REGISTER_PLATFORM_UNSUPPORTED", "platform must be deepseek")
			}
			email := strings.TrimSpace(req.Email)
			if !validateWebRegisterEmail(email) {
				return nil, infraerrors.BadRequest("WEB_REGISTER_EMAIL_INVALID", "邮箱格式不正确（需包含 @ 且长度不超过 254）")
			}
			deviceID := uuid.NewString()
			// 外审 R3-P4：发码与注册/登录同口径——proxy_id 非空时显式解析 Proxy 对象，
			// 查不到即 400 失败关闭（不静默回退默认出站，避免地域门控 REGISTER_FROM_MAINLAND）。
			proxyURL := ""
			if req.ProxyID != nil && *req.ProxyID != 0 {
				proxy, perr := h.adminService.GetProxy(ctx, *req.ProxyID)
				if perr != nil || proxy == nil {
					return nil, infraerrors.BadRequest("WEB_REGISTER_PROXY_NOT_FOUND",
						fmt.Sprintf("指定的代理不存在（proxy_id=%d），不回退默认出站", *req.ProxyID))
				}
				proxyURL = proxy.URL()
			}
			secs, sendErr := svc.SendRegisterEmailCode(ctx, email, "en", deviceID, "register", proxyURL)
			if sendErr != nil {
				return nil, sendErr
			}
			return gin.H{"success": true, "send_window_secs": secs, "device_id": deviceID}, nil
		})
	if err != nil {
		respondWebRegisterIdempotentError(c, err)
		return
	}
	if result != nil && result.Replayed {
		c.Header("X-Idempotency-Replayed", "true")
	}
	response.Success(c, result.Data)
}

// WebRegister 处理 DeepSeek 邮箱验证码注册 + 登录 + 建号一体化（任务卡 §2.2-2）。
// body {platform:"deepseek", email, email_verification_code, password, account_draft}
// → {success:true, account_id}。全程包在 executeAdminIdempotent 闭包内（外审 F1）。
func (h *AccountHandler) WebRegister(c *gin.Context) {
	var req struct {
		Platform              string                `json:"platform" binding:"required"`
		Email                 string                `json:"email" binding:"required"`
		EmailVerificationCode string                `json:"email_verification_code" binding:"required"`
		Password              string                `json:"password" binding:"required"`
		AccountDraft          *CreateAccountRequest `json:"account_draft" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	svc := h.webDeepseekRegisterSvc()
	if svc == nil {
		response.Error(c, http.StatusServiceUnavailable, "web auto-login service unavailable")
		return
	}

	// R6-P1：handler 层强制幂等键。注册是不可幂等外呼的上游写操作（重试会重复触发
	// 上游注册），不受全局幂等协调器 ObserveOnly=true 豁免（idempotency.go: RequireKey
	// 只在非 ObserveOnly 时拒绝，缺键会直接执行且不保存结果）。非法字符校验由协调器内
	// NormalizeIdempotencyKey 承担，此处仅拦截空串/纯空白。
	if strings.TrimSpace(c.GetHeader("Idempotency-Key")) == "" {
		response.ErrorFrom(c, service.ErrIdempotencyKeyRequired)
		return
	}

	result, err := executeAdminIdempotent(c, "admin.accounts.web-register", req,
		service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
			// a. 预校验前移（方案 §1.2-4）：密码强度 / account_draft 基本字段不通过直接
			// 400，不触上游。
			platform := strings.ToLower(strings.TrimSpace(req.Platform))
			if platform != service.PlatformDeepseek {
				return nil, infraerrors.BadRequest("WEB_REGISTER_PLATFORM_UNSUPPORTED", "platform must be deepseek")
			}
			email := strings.TrimSpace(req.Email)
			if !validateWebRegisterEmail(email) {
				return nil, infraerrors.BadRequest("WEB_REGISTER_EMAIL_INVALID", "邮箱格式不正确（需包含 @ 且长度不超过 254）")
			}
			verifyCode := strings.TrimSpace(req.EmailVerificationCode)
			if verifyCode == "" {
				return nil, infraerrors.BadRequest("WEB_REGISTER_CODE_REQUIRED", "email_verification_code 不能为空")
			}
			if !validateWebRegisterPassword(req.Password) {
				return nil, infraerrors.BadRequest("WEB_REGISTER_PASSWORD_WEAK", "密码需至少 8 位且包含字母和数字")
			}
			draft := req.AccountDraft
			if strings.TrimSpace(draft.Name) == "" {
				return nil, infraerrors.BadRequest("WEB_REGISTER_DRAFT_INVALID", "account_draft.name 不能为空")
			}
			if draft.Type != service.AccountTypeAPIKey {
				return nil, infraerrors.BadRequest("WEB_REGISTER_DRAFT_INVALID", "account_draft.type 必须为 apikey")
			}
			// R6-P2：draft 平台必须与请求 platform 一致（契约对齐 WebLoginPassword
			// 既有检查：account_draft.platform must match platform）。
			draftPlatform := strings.ToLower(strings.TrimSpace(draft.Platform))
			if draftPlatform != platform {
				return nil, infraerrors.BadRequest("WEB_REGISTER_DRAFT_INVALID", "account_draft.platform 必须与 platform 一致")
			}

			// b. 代理解析（外审 F3）：proxy_id 非空时显式查 Proxy 对象取 URL，查不到即 400，
			// 不静默回退；为空走默认出站（当前即香港服务器）。
			proxyURL := ""
			var proxyObj *service.Proxy
			if draft.ProxyID != nil && *draft.ProxyID != 0 {
				proxy, perr := h.adminService.GetProxy(ctx, *draft.ProxyID)
				if perr != nil || proxy == nil {
					return nil, infraerrors.BadRequest("WEB_REGISTER_PROXY_NOT_FOUND",
						fmt.Sprintf("指定的代理不存在（proxy_id=%d），不回退默认出站", *draft.ProxyID))
				}
				proxyURL = proxy.URL()
				proxyObj = proxy
			}

			// c. device_id：本次注册生成，注册与紧随的登录复用同一 device_id，落 login_device_id。
			deviceID := uuid.NewString()

			// d. 上游注册（region 固定 "HK"，方案 §1.2-1/§2 取证 7）；PoW 由 service 层
			// 一次一换（每次上游请求前实时取挑战求解，禁止缓存）。非 0 业务码透传文案。
			// 外审 2026-09-22 R3-P2：注册 POST 出站后响应丢失 → RegisterOutcomeUnknownError，
			// 结果不明（上游可能已建号）。不得作为 error 交给协调器（failed_retryable 自动
			// 重试会二次打上游拿 EMAIL_EXISTS），转终态结果路径返回密码登录恢复指引；
			// 同键重试由协调器直接重放本终态，不再触上游。
			if rerr := svc.RegisterByEmail(ctx, email, verifyCode, req.Password, "HK", deviceID, proxyURL); rerr != nil {
				var unknown *service.RegisterOutcomeUnknownError
				if errors.As(rerr, &unknown) {
					return webRegisterPostSuccessResult{
						Kind:        webRegisterPostSuccessKindOutcomeUnknown,
						ErrorReason: rerr.Error(),
					}, nil
				}
				return nil, rerr
			}

			// e. 注册成功 → 立即用同一邮箱密码走既有 LoginByEmail 取 Cookie（内含
			// verifyDeepseekCookie 验证，不因上游行为放宽）。以下任何失败统一 F4 文案（外审 F4）。
			loginAccount := &service.Account{
				Platform: service.PlatformDeepseek,
				Type:     service.AccountTypeAPIKey,
				Credentials: map[string]any{
					"access_mode":                service.AccountAccessModeWeb,
					service.CredKeyLoginEmail:    email,
					service.CredKeyLoginPassword: req.Password,
					service.CredKeyLoginDeviceID: deviceID,
				},
				ProxyID:     draft.ProxyID,
				Proxy:       proxyObj,
				Concurrency: draft.Concurrency,
			}
			cookie, lerr := h.webPlatformAutoLogin.LoginByEmail(ctx, loginAccount)
			if lerr != nil {
				return webRegisterPostSuccessResult{Kind: webRegisterPostSuccessKindFailure, ErrorReason: lerr.Error()}, nil
			}

			// f. 登录会话去重（外审 F2）：以取得的 Cookie 在幂等闭包内显式查重
			//（excludeAccountID=0），命中返回既有 409，不落库。
			dupCreds := map[string]any{"cookie": cookie, "access_mode": service.AccountAccessModeWeb}
			existing, derr := h.duplicateWebCredentialAccount(ctx, service.PlatformDeepseek, dupCreds, 0)
			if derr != nil {
				return webRegisterPostSuccessResult{Kind: webRegisterPostSuccessKindFailure, ErrorReason: derr.Error()}, nil
			}
			if existing != "" {
				return webRegisterPostSuccessResult{Kind: webRegisterPostSuccessKindDuplicate, DupAccountID: existing}, nil
			}

			// g. 建号落库（FromWebLogin:true）：凭据五键齐（cookie/login_email/login_password/
			// login_device_id/access_mode=web，服务端强制兜底 access_mode=web），其余字段取
			// account_draft。
			createdCreds := mergeWebLoginCredentials(draft.Credentials, nil)
			createdCreds[webAutoLoginCookieKey] = cookie
			createdCreds[service.CredKeyLoginEmail] = email
			createdCreds[service.CredKeyLoginPassword] = req.Password
			createdCreds[service.CredKeyLoginDeviceID] = deviceID
			createdCreds["access_mode"] = service.AccountAccessModeWeb
			created, cerr := h.adminService.CreateAccount(ctx, &service.CreateAccountInput{
				Name: draft.Name, Notes: draft.Notes, Platform: service.PlatformDeepseek,
				Type: draft.Type, Credentials: createdCreds,
				Extra: draft.Extra, ProxyID: draft.ProxyID, Concurrency: draft.Concurrency,
				Priority: draft.Priority, RateMultiplier: draft.RateMultiplier, LoadFactor: draft.LoadFactor,
				GroupIDs: draft.GroupIDs, ExpiresAt: draft.ExpiresAt,
				AutoPauseOnExpired: draft.AutoPauseOnExpired, ProbeEnabled: draft.ProbeEnabled,
				FromWebLogin: true,
			})
			if cerr != nil {
				return webRegisterPostSuccessResult{Kind: webRegisterPostSuccessKindFailure, ErrorReason: cerr.Error()}, nil
			}
			return gin.H{"success": true, "account_id": created.ID}, nil
		})
	if err != nil {
		respondWebRegisterIdempotentError(c, err)
		return
	}
	if result != nil && result.Replayed {
		c.Header("X-Idempotency-Replayed", "true")
	}
	// post-success 终态结果（外审 P1-2）：闭包以成功结果持久化，此处映射为 F4/409
	// 响应。直接执行与同键重放都会落到这里，语义一致。
	if result != nil {
		if res, ok := result.Data.(webRegisterPostSuccessResult); ok {
			respondWebRegisterPostSuccess(c, res)
			return
		}
		// 重放路径经 JSON 解码为 map[string]any，按固定键还原终态。
		if m, ok := result.Data.(map[string]any); ok {
			if res, ok := webRegisterPostSuccessResultFromMap(m); ok {
				respondWebRegisterPostSuccess(c, res)
				return
			}
		}
	}
	response.Success(c, result.Data)
}

// webRegisterPostSuccessResultFromMap 从重放解码的 map 还原 post-success 终态
// （固定键 kind / dup_account_id / error_reason，与 webRegisterPostSuccessResult JSON
// 标签一致）。
func webRegisterPostSuccessResultFromMap(m map[string]any) (webRegisterPostSuccessResult, bool) {
	kindRaw, _ := m["kind"].(string)
	switch webRegisterPostSuccessResultKind(kindRaw) {
	case webRegisterPostSuccessKindDuplicate:
		dupID, _ := m["dup_account_id"].(string)
		return webRegisterPostSuccessResult{Kind: webRegisterPostSuccessKindDuplicate, DupAccountID: dupID}, true
	case webRegisterPostSuccessKindFailure:
		reason, _ := m["error_reason"].(string)
		return webRegisterPostSuccessResult{Kind: webRegisterPostSuccessKindFailure, ErrorReason: reason}, true
	case webRegisterPostSuccessKindOutcomeUnknown:
		reason, _ := m["error_reason"].(string)
		return webRegisterPostSuccessResult{Kind: webRegisterPostSuccessKindOutcomeUnknown, ErrorReason: reason}, true
	default:
		return webRegisterPostSuccessResult{}, false
	}
}
