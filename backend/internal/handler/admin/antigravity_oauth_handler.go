package admin

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// antigravityOAuthService 是 AntigravityOAuthHandler 依赖的凭证服务能力集。
// 生产实现为 *service.AntigravityOAuthService；抽成接口仅为让 handler 的审计
// 逻辑可在不发起上游网络请求的前提下单测。
type antigravityOAuthService interface {
	GenerateAuthURL(ctx context.Context, proxyID *int64) (*service.AntigravityAuthURLResult, error)
	ExchangeCode(ctx context.Context, input *service.AntigravityExchangeCodeInput) (*service.AntigravityTokenInfo, error)
	ValidateRefreshToken(ctx context.Context, refreshToken string, proxyID *int64) (*service.AntigravityTokenInfo, error)
}

type AntigravityOAuthHandler struct {
	antigravityOAuthService antigravityOAuthService
}

func NewAntigravityOAuthHandler(antigravityOAuthService *service.AntigravityOAuthService) *AntigravityOAuthHandler {
	return &AntigravityOAuthHandler{antigravityOAuthService: antigravityOAuthService}
}

// setAntigravityProxyAuditTarget 把请求选定的代理写入审计目标（nil 不写）。
func setAntigravityProxyAuditTarget(c *gin.Context, proxyID *int64) {
	if proxyID == nil {
		return
	}
	middleware.SetAuditExtra(c, map[string]any{"proxy_id": *proxyID})
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
	// 审计目标：本次登录选定的代理（nil = 直连）。
	setAntigravityProxyAuditTarget(c, req.ProxyID)

	result, err := h.antigravityOAuthService.GenerateAuthURL(c.Request.Context(), req.ProxyID)
	if err != nil {
		response.InternalError(c, "生成授权链接失败: "+err.Error())
		return
	}

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
	// 审计目标：state（授权会话标识）与代理；code 值由审计中间件脱敏处理。
	middleware.SetAuditExtra(c, map[string]any{"oauth_state": req.State})
	setAntigravityProxyAuditTarget(c, req.ProxyID)

	tokenInfo, err := h.antigravityOAuthService.ExchangeCode(c.Request.Context(), &service.AntigravityExchangeCodeInput{
		SessionID: req.SessionID,
		State:     req.State,
		Code:      req.Code,
		ProxyID:   req.ProxyID,
	})
	if err != nil {
		response.BadRequest(c, "Token 交换失败: "+err.Error())
		return
	}

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
	// 审计目标：本次刷新选定的代理（refresh_token 值由审计中间件脱敏处理）。
	setAntigravityProxyAuditTarget(c, req.ProxyID)

	tokenInfo, err := h.antigravityOAuthService.ValidateRefreshToken(c.Request.Context(), req.RefreshToken, req.ProxyID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, tokenInfo)
}
