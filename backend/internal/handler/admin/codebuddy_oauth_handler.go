package admin

import (
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type CodeBuddyOAuthHandler struct {
	codeBuddyOAuthService *service.CodeBuddyOAuthService
}

func NewCodeBuddyOAuthHandler(codeBuddyOAuthService *service.CodeBuddyOAuthService) *CodeBuddyOAuthHandler {
	return &CodeBuddyOAuthHandler{codeBuddyOAuthService: codeBuddyOAuthService}
}

// CodeBuddyGenerateAuthURLRequest 生成授权链接请求。proxy_id 为本次登录选定的
// 代理（可选）：登录/poll 都应从该代理出口，避免登录 IP 与日常调用 IP 不一致。
type CodeBuddyGenerateAuthURLRequest struct {
	ProxyID *int64 `json:"proxy_id"`
}

// GenerateAuthURL generates CodeBuddy OAuth authorization URL
// POST /api/v1/admin/codebuddy/oauth/auth-url
func (h *CodeBuddyOAuthHandler) GenerateAuthURL(c *gin.Context) {
	var req CodeBuddyGenerateAuthURLRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		req = CodeBuddyGenerateAuthURLRequest{}
	}
	result, err := h.codeBuddyOAuthService.GenerateAuthURL(c.Request.Context(), req.ProxyID)
	if err != nil {
		middleware.SetAuditExtra(c, map[string]any{"result": "failed"})
		response.ErrorFrom(c, err)
		return
	}

	// 审计：只记录动作与目标 state（公开 nonce），绝不记录 URL 之外的任何凭证材料。
	middleware.SetAuditExtra(c, map[string]any{"result": "success", "state": result.State})
	response.Success(c, result)
}

type CodeBuddyPollTokenRequest struct {
	State   string `json:"state" binding:"required"`
	ProxyID *int64 `json:"proxy_id"`
}

// PollToken 轮询 CodeBuddy 登录结果（用户在浏览器完成登录后 token 才就绪）
// POST /api/v1/admin/codebuddy/oauth/poll
func (h *CodeBuddyOAuthHandler) PollToken(c *gin.Context) {
	var req CodeBuddyPollTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "请求无效: "+err.Error())
		return
	}

	tokenInfo, err := h.codeBuddyOAuthService.PollToken(c.Request.Context(), req.State, req.ProxyID)
	if err != nil {
		if errors.Is(err, service.ErrCodeBuddyLoginPending) {
			middleware.SetAuditExtra(c, map[string]any{"result": "pending", "state": req.State})
			response.BadRequest(c, "登录未完成，请先在浏览器完成登录后再重试")
			return
		}
		middleware.SetAuditExtra(c, map[string]any{"result": "failed", "state": req.State})
		response.InternalError(c, "获取 token 失败: "+err.Error())
		return
	}

	// 审计：记动作与目标 state；tokenInfo 中的 access/refresh token 绝不入审计。
	middleware.SetAuditExtra(c, map[string]any{"result": "success", "state": req.State})
	response.Success(c, tokenInfo)
}

type CodeBuddyRefreshTokenRequest struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterprise_id"`
	Domain       string `json:"domain"`
	ProxyID      *int64 `json:"proxy_id"`
}

// RefreshToken 用 refresh token 刷新并返回完整 token 信息
// POST /api/v1/admin/codebuddy/oauth/refresh-token
func (h *CodeBuddyOAuthHandler) RefreshToken(c *gin.Context) {
	var req CodeBuddyRefreshTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "请求无效: "+err.Error())
		return
	}

	tokenInfo, err := h.codeBuddyOAuthService.RefreshToken(c.Request.Context(), req.RefreshToken, req.UID, req.EnterpriseID, req.Domain, req.ProxyID)
	if err != nil {
		setCodeBuddyRefreshAuditExtra(c, "failed", &req)
		response.ErrorFrom(c, err)
		return
	}

	// 审计：记动作与目标账号标识（uid/enterprise_id）；refresh_token 与返回的
	// tokenInfo 绝不入审计。
	setCodeBuddyRefreshAuditExtra(c, "success", &req)
	response.Success(c, tokenInfo)
}

// setCodeBuddyRefreshAuditExtra 记录 refresh-token 操作的结果与目标账号标识。
// 只收集非空标量，空字段不写入审计 Extra。
func setCodeBuddyRefreshAuditExtra(c *gin.Context, result string, req *CodeBuddyRefreshTokenRequest) {
	extra := map[string]any{"result": result}
	if req.UID != "" {
		extra["uid"] = req.UID
	}
	if req.EnterpriseID != "" {
		extra["enterprise_id"] = req.EnterpriseID
	}
	middleware.SetAuditExtra(c, extra)
}
