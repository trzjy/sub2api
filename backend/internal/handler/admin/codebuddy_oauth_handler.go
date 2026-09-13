package admin

import (
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
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
		response.ErrorFrom(c, err)
		return
	}

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
			response.BadRequest(c, "登录未完成，请先在浏览器完成登录后再重试")
			return
		}
		response.InternalError(c, "获取 token 失败: "+err.Error())
		return
	}

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
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, tokenInfo)
}
