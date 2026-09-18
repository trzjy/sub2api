package admin

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"

	"github.com/gin-gonic/gin"
	"golang.org/x/sync/errgroup"
)

// webAutoLoginCookieKey 是网页版账号凭据中存放整串 Cookie 头的键名（与
// web_deepseek_gateway_forward / web_zhipu_gateway_forward 读取的键一致）。
const webAutoLoginCookieKey = "cookie"

// webPlatformAutoLoginService 是网页版平台自动登录服务的本地接口，便于测试替换。
// 其方法签名与 service.WebPlatformAutoLoginService 对齐（由并行任务实现）。
type webPlatformAutoLoginService interface {
	LoginByEmail(ctx context.Context, account *service.Account) (string, error)
	RefreshToken(ctx context.Context, account *service.Account) error
	RecoverAccount(ctx context.Context, account *service.Account) service.WebRecoverResult
	Start(ctx context.Context)
	Stop()
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
		Platform     string `json:"platform" binding:"required"`
		LoginEmail   string `json:"login_email"`
		LoginPhone   string `json:"login_phone"`
		LoginPassword string `json:"login_password"`
		AccountID    *int64 `json:"account_id"`
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
	h.ensureWebAutoLoginStarted()
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
		// zhipu/kimi 官方网页端没有密码登录（微信扫码/手机号短信码，发码被数美滑块/
		// 易盾验证码保护），账号密码自动登录不适用；登录态经由浏览器登录（登录代理
		// iframe 人工完成滑块/短信验证）捕获回传。
		title, hint := service.WebPlatformErrorDetail(req.Platform, 0, "login")
		response.ErrorWithDetails(c, http.StatusBadRequest, title, "",
			map[string]string{
				"detail": "该平台官方网页端不提供密码登录，请使用浏览器登录（人工完成滑块/短信验证）",
				"hint":   hint,
			})
	default:
		response.BadRequest(c, "unsupported web platform: "+req.Platform)
	}
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

// BatchLogin 按平台分派的批量自动登录/恢复。
// POST /api/v1/admin/accounts/batch-login
//
//	body {ids:[]int64}
//
// deepseek web 账号调 RecoverAccount（第一级自动恢复）；
// zhipu/kimi 调 RefreshToken（失败 → NeedsSemiAuto）。并发 ≤3。
func (h *AccountHandler) BatchLogin(c *gin.Context) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if len(req.IDs) == 0 {
		response.BadRequest(c, "ids is required")
		return
	}

	ctx := c.Request.Context()
	h.ensureWebAutoLoginStarted()
	if h.webPlatformAutoLogin == nil {
		response.Error(c, http.StatusServiceUnavailable, "web auto-login service unavailable")
		return
	}

	accounts, err := h.adminService.GetAccountsByIDs(ctx, req.IDs)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	byID := make(map[int64]*service.Account, len(accounts))
	for _, acc := range accounts {
		if acc != nil {
			byID[acc.ID] = acc
		}
	}

	const maxConcurrency = 3
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxConcurrency)

	var mu sync.Mutex
	results := make([]gin.H, 0, len(req.IDs))
	var successCount, failedCount int

	for _, id := range req.IDs {
		accountID := id
		g.Go(func() error {
			acc := byID[accountID]
			if acc == nil {
				mu.Lock()
				results = append(results, gin.H{
					"id":       accountID,
					"recovered": false,
					"needs_sms": false,
					"detail":   "account not found",
				})
				failedCount++
				mu.Unlock()
				return nil
			}
			// 归并后同平台可共存普通 API 账号与 Web 账号，必须按账号级 access_mode
			// 隔离：非 web access_mode 账号不得进入自动登录，作为失败项返回。
			if !acc.IsWebAccessMode() {
				mu.Lock()
				results = append(results, gin.H{
					"id":        accountID,
					"recovered": false,
					"needs_sms": false,
					"detail":    "not a web access-mode account",
				})
				failedCount++
				mu.Unlock()
				return nil
			}
			var res gin.H
			switch acc.Platform {
			case service.PlatformDeepseek:
				outcome := h.webPlatformAutoLogin.RecoverAccount(gctx, acc)
				res = gin.H{
					"id":        accountID,
					"recovered": outcome.Recovered,
					"needs_sms": outcome.NeedsSemiAuto,
					"detail":    outcome.Detail,
				}
			case service.PlatformZhipu, service.PlatformKimi:
				if refreshErr := h.webPlatformAutoLogin.RefreshToken(gctx, acc); refreshErr != nil {
					title, _ := service.WebPlatformErrorDetail(acc.Platform, 0, "refresh")
					res = gin.H{
						"id":        accountID,
						"recovered": false,
						"needs_sms": true,
						"detail":    title,
					}
				} else {
					res = gin.H{
						"id":        accountID,
						"recovered": true,
						"needs_sms": false,
						"detail":    "",
					}
				}
			default:
				res = gin.H{
					"id":        accountID,
					"recovered": false,
					"needs_sms": false,
					"detail":    "unsupported platform",
				}
			}
			mu.Lock()
			results = append(results, res)
			if v, ok := res["recovered"].(bool); ok && v {
				successCount++
			} else {
				failedCount++
			}
			mu.Unlock()
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		response.ErrorFrom(c, err)
		return
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i]["id"].(int64) < results[j]["id"].(int64)
	})

	response.Success(c, gin.H{
		"results": results,
		"summary": gin.H{"success": successCount, "failed": failedCount},
	})
}

// BatchTest 对选中账号逐个复用既有 AccountTestService 测试内核（不走 SSE）。
// POST /api/v1/admin/accounts/batch-test
//
//	body {ids:[]int64}
func (h *AccountHandler) BatchTest(c *gin.Context) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if len(req.IDs) == 0 {
		response.BadRequest(c, "ids is required")
		return
	}
	if h.accountTestService == nil {
		response.Error(c, http.StatusServiceUnavailable, "account test service unavailable")
		return
	}

	ctx := c.Request.Context()

	accounts, err := h.adminService.GetAccountsByIDs(ctx, req.IDs)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	byID := make(map[int64]*service.Account, len(accounts))
	for _, acc := range accounts {
		if acc != nil {
			byID[acc.ID] = acc
		}
	}

	const maxConcurrency = 3
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxConcurrency)

	var mu sync.Mutex
	results := make([]gin.H, 0, len(req.IDs))

	for _, id := range req.IDs {
		accountID := id
		g.Go(func() error {
			acc := byID[accountID]
			if acc == nil {
				mu.Lock()
				results = append(results, gin.H{
					"id":      accountID,
					"success": false,
					"error":   "account not found",
				})
				mu.Unlock()
				return nil
			}
			// 同平台可共存普通 API 账号与 Web 账号，按账号级 access_mode 隔离：
			// 非 web access_mode 账号不发起测试，作为失败项返回。
			if !acc.IsWebAccessMode() {
				mu.Lock()
				results = append(results, gin.H{
					"id":      accountID,
					"success": false,
					"error":   "not a web access-mode account",
				})
				mu.Unlock()
				return nil
			}
			outcome, testErr := h.accountTestService.RunTestBackground(gctx, accountID, "")
			mu.Lock()
			defer mu.Unlock()
			if testErr != nil {
				results = append(results, gin.H{
					"id":      accountID,
					"success": false,
					"error":   testErr.Error(),
				})
				return nil
			}
			success := outcome.Status == "success"
			errMsg := outcome.ErrorMessage
			if success && errMsg == "" {
				errMsg = ""
			}
			results = append(results, gin.H{
				"id":      accountID,
				"success": success,
				"error":   errMsg,
			})
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		response.ErrorFrom(c, err)
		return
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i]["id"].(int64) < results[j]["id"].(int64)
	})

	response.Success(c, gin.H{"results": results})
}

// webBanKeywords 是判定账号是否被上游封禁（含"封禁(10)"）的关键词集合。
// 命中 ErrorMessage 或凭据 login_last_error 即视为待清理候选。
var webBanKeywords = []string{"封禁", "banned"}

// isBanCandidate 判定账号是否命中封禁（安全红线只做识别，不做任何凭据导出）。
func isBanCandidate(account *service.Account) (bool, string) {
	if account == nil {
		return false, ""
	}
	var texts []string
	if account.ErrorMessage != "" {
		texts = append(texts, account.ErrorMessage)
	}
	if account.Credentials != nil {
		if v, ok := account.Credentials["login_last_error"].(string); ok && v != "" {
			texts = append(texts, v)
		}
	}
	lower := strings.ToLower(strings.Join(texts, " "))
	for _, kw := range webBanKeywords {
		if strings.Contains(lower, strings.ToLower(kw)) {
			return true, strings.TrimSpace(strings.Join(texts, "; "))
		}
	}
	return false, ""
}

// BatchDeleteBanned 批量清理被封禁（10）账号。
// POST /api/v1/admin/accounts/batch-delete-banned
//
//	body {confirm:bool}
//
// 默认（confirm=false）仅返回待删清单，不删；confirm=true 走删除逻辑。
func (h *AccountHandler) BatchDeleteBanned(c *gin.Context) {
	var req struct {
		Confirm bool `json:"confirm"`
	}
	// 允许空 body。
	_ = c.ShouldBindJSON(&req)

	ctx := c.Request.Context()

	webPlatforms := []string{service.PlatformDeepseek, service.PlatformZhipu, service.PlatformKimi}
	var all []service.Account
	for _, p := range webPlatforms {
		accounts, _, err := h.adminService.ListAccounts(ctx, 1, 100000, p, "", "", "", 0, "", "", "")
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		all = append(all, accounts...)
	}

	candidates := make([]gin.H, 0)
	for i := range all {
		acc := &all[i]
		// 平台列举可能带回同平台的普通 API 账号（如 zhipu + api_key）：它们即使
		// 错误文本命中封禁词也不得进入删除候选，必须先按账号级 access_mode 隔离。
		if !acc.IsWebAccessMode() {
			continue
		}
		if ok, reason := isBanCandidate(acc); ok {
			candidates = append(candidates, gin.H{
				"id":     acc.ID,
				"name":   acc.Name,
				"reason": reason,
			})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i]["id"].(int64) < candidates[j]["id"].(int64)
	})

	if !req.Confirm {
		response.Success(c, gin.H{"candidates": candidates, "deleted": 0})
		return
	}

	var mu sync.Mutex
	deleted := 0
	var deleteErrors []gin.H
	for _, cand := range candidates {
		id := cand["id"].(int64)
		if err := h.adminService.DeleteAccount(ctx, id); err != nil {
			mu.Lock()
			deleteErrors = append(deleteErrors, gin.H{"account_id": id, "error": err.Error()})
			mu.Unlock()
			continue
		}
		mu.Lock()
		deleted++
		mu.Unlock()
	}

	response.Success(c, gin.H{"candidates": candidates, "deleted": deleted, "errors": deleteErrors})
}

// BatchStatus 批量置账号状态（仅 active/disabled）。
// POST /api/v1/admin/accounts/batch-status
//
//	body {ids:[]int64, status:"active"|"disabled"}
func (h *AccountHandler) BatchStatus(c *gin.Context) {
	var req struct {
		IDs    []int64 `json:"ids"`
		Status string  `json:"status" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if req.Status != service.StatusActive && req.Status != service.StatusDisabled {
		response.BadRequest(c, "status must be one of: active, disabled")
		return
	}
	if len(req.IDs) == 0 {
		response.BadRequest(c, "ids is required")
		return
	}

	ctx := c.Request.Context()

	accounts, err := h.adminService.GetAccountsByIDs(ctx, req.IDs)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	byID := make(map[int64]*service.Account, len(accounts))
	for _, acc := range accounts {
		if acc != nil {
			byID[acc.ID] = acc
		}
	}

	const maxConcurrency = 3
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxConcurrency)

	var mu sync.Mutex
	var successCount, failedCount int
	var errorsList []gin.H

	for _, id := range req.IDs {
		accountID := id
		g.Go(func() error {
			acc := byID[accountID]
			if acc == nil {
				mu.Lock()
				failedCount++
				errorsList = append(errorsList, gin.H{"account_id": accountID, "error": "account not found"})
				mu.Unlock()
				return nil
			}
			// 同平台可共存普通 API 账号与 Web 账号，按账号级 access_mode 隔离：
			// 非 web access_mode 账号不得被改状态，作为失败项返回。
			if !acc.IsWebAccessMode() {
				mu.Lock()
				failedCount++
				errorsList = append(errorsList, gin.H{"account_id": accountID, "error": "not a web access-mode account"})
				mu.Unlock()
				return nil
			}
			if _, err := h.adminService.UpdateAccount(gctx, accountID, &service.UpdateAccountInput{Status: req.Status}); err != nil {
				mu.Lock()
				failedCount++
				errorsList = append(errorsList, gin.H{"account_id": accountID, "error": err.Error()})
				mu.Unlock()
				return nil
			}
			mu.Lock()
			successCount++
			mu.Unlock()
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, gin.H{
		"total":   len(req.IDs),
		"success": successCount,
		"failed":  failedCount,
		"errors":  errorsList,
	})
}

// ExportedWebAccount 是账号导出 DTO（安全红线：绝不导出凭据）。
type ExportedWebAccount struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	Platform        string `json:"platform"`
	EmailOrPhone    string `json:"email_or_phone"`
	Status          string `json:"status"`
	LoginLastAt     string `json:"login_last_at"`
	LoginLastError  string `json:"login_last_error"`
}

// ExportWebAccounts 导出网页版账号清单（ID/名称/邮箱或手机号/状态/last_at/last_error）。
// GET /api/v1/admin/accounts/export?platform=deepseek
//
// 安全红线：凭据（cookie / token / password 等）绝不出现在响应中。
func (h *AccountHandler) ExportWebAccounts(c *gin.Context) {
	platform := c.Query("platform")
	if platform == "" {
		response.BadRequest(c, "platform is required")
		return
	}
	if !service.IsWebLoginPlatform(platform) {
		response.BadRequest(c, "platform must be a web provider")
		return
	}

	ctx := c.Request.Context()
	accounts, _, err := h.adminService.ListAccounts(ctx, 1, 100000, platform, "", "", "", 0, "", "", "")
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	out := make([]ExportedWebAccount, 0, len(accounts))
	for i := range accounts {
		acc := &accounts[i]
		// ListAccounts 按平台取回，可能混入同平台的普通 API 账号：导出清单只允许
		// 含账号级 access_mode == web 的账号，普通 API 账号不得出现在导出 JSON 中。
		if !acc.IsWebAccessMode() {
			continue
		}
		emailOrPhone := ""
		loginLastAt := ""
		loginLastError := acc.ErrorMessage
		if acc.Credentials != nil {
			if v, ok := acc.Credentials["login_email"].(string); ok && v != "" {
				emailOrPhone = v
			} else if v, ok := acc.Credentials["login_phone"].(string); ok && v != "" {
				emailOrPhone = v
			}
			if v, ok := acc.Credentials["login_last_at"].(string); ok {
				loginLastAt = v
			}
			if v, ok := acc.Credentials["login_last_error"].(string); ok && v != "" {
				loginLastError = v
			}
		}
		out = append(out, ExportedWebAccount{
			ID:             acc.ID,
			Name:           acc.Name,
			Platform:       acc.Platform,
			EmailOrPhone:   emailOrPhone,
			Status:         acc.Status,
			LoginLastAt:    loginLastAt,
			LoginLastError: loginLastError,
		})
	}

	response.Success(c, gin.H{"accounts": out, "total": len(out)})
}
