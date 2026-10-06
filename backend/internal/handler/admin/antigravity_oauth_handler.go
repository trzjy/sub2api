package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type AntigravityOAuthHandler struct {
	antigravityOAuthService *service.AntigravityOAuthService
}

func NewAntigravityOAuthHandler(antigravityOAuthService *service.AntigravityOAuthService) *AntigravityOAuthHandler {
	return &AntigravityOAuthHandler{antigravityOAuthService: antigravityOAuthService}
}

type AntigravityGenerateAuthURLRequest struct {
	ProxyID *int64 `json:"proxy_id"`
}

// GenerateAuthURL generates Google OAuth authorization URL
// POST /api/v1/admin/antigravity/oauth/auth-url
func (h *AntigravityOAuthHandler) GenerateAuthURL(c *gin.Context) {
	var req AntigravityGenerateAuthURLRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "请求无效: "+err.Error())
		return
	}

	result, err := h.antigravityOAuthService.GenerateAuthURL(c.Request.Context(), req.ProxyID)
	if err != nil {
		middleware.SetAuditExtra(c, map[string]any{"result": "failed"})
		response.InternalError(c, "生成授权链接失败: "+err.Error())
		return
	}

	// 审计：只记录动作与目标 session/state（公开 nonce），不记录授权 URL 之外的材料。
	middleware.SetAuditExtra(c, map[string]any{"result": "success", "session_id": result.SessionID, "state": result.State})
	response.Success(c, result)
}

type AntigravityExchangeCodeRequest struct {
	SessionID string `json:"session_id" binding:"required"`
	State     string `json:"state" binding:"required"`
	Code      string `json:"code" binding:"required"`
	ProxyID   *int64 `json:"proxy_id"`
}

// ExchangeCode 用 authorization code 交换 token
// POST /api/v1/admin/antigravity/oauth/exchange-code
func (h *AntigravityOAuthHandler) ExchangeCode(c *gin.Context) {
	var req AntigravityExchangeCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "请求无效: "+err.Error())
		return
	}

	tokenInfo, err := h.antigravityOAuthService.ExchangeCode(c.Request.Context(), &service.AntigravityExchangeCodeInput{
		SessionID: req.SessionID,
		State:     req.State,
		Code:      req.Code,
		ProxyID:   req.ProxyID,
	})
	if err != nil {
		middleware.SetAuditExtra(c, map[string]any{"result": "failed", "session_id": req.SessionID, "state": req.State})
		response.BadRequest(c, "Token 交换失败: "+err.Error())
		return
	}

	// 审计：记动作与目标 session/state；authorization code 与换得的 token 绝不入审计。
	middleware.SetAuditExtra(c, map[string]any{"result": "success", "session_id": req.SessionID, "state": req.State})
	response.Success(c, tokenInfo)
}

// AntigravityRefreshTokenRequest represents the request for validating Antigravity refresh token
type AntigravityRefreshTokenRequest struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
	ProxyID      *int64 `json:"proxy_id"`
}

// RefreshToken validates an Antigravity refresh token and returns full token info
// POST /api/v1/admin/antigravity/oauth/refresh-token
func (h *AntigravityOAuthHandler) RefreshToken(c *gin.Context) {
	var req AntigravityRefreshTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "请求无效: "+err.Error())
		return
	}

	tokenInfo, err := h.antigravityOAuthService.ValidateRefreshToken(c.Request.Context(), req.RefreshToken, req.ProxyID)
	if err != nil {
		middleware.SetAuditExtra(c, map[string]any{"result": "failed"})
		response.ErrorFrom(c, err)
		return
	}

	// 审计：只记录结果；refresh_token 与返回的 tokenInfo 绝不入审计。
	middleware.SetAuditExtra(c, map[string]any{"result": "success"})
	response.Success(c, tokenInfo)
}
