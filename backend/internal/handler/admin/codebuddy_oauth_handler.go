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

// GenerateAuthURL generates CodeBuddy OAuth authorization URL
// POST /api/v1/admin/codebuddy/oauth/auth-url
func (h *CodeBuddyOAuthHandler) GenerateAuthURL(c *gin.Context) {
	result, err := h.codeBuddyOAuthService.GenerateAuthURL(c.Request.Context())
	if err != nil {
		response.InternalError(c, "生成授权链接失败: "+err.Error())
		return
	}

	response.Success(c, result)
}

type CodeBuddyPollTokenRequest struct {
	State string `json:"state" binding:"required"`
}

// PollToken 轮询 CodeBuddy 登录结果（用户在浏览器完成登录后 token 才就绪）
// POST /api/v1/admin/codebuddy/oauth/poll
func (h *CodeBuddyOAuthHandler) PollToken(c *gin.Context) {
	var req CodeBuddyPollTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "请求无效: "+err.Error())
		return
	}

	tokenInfo, err := h.codeBuddyOAuthService.PollToken(c.Request.Context(), req.State)
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
}

// RefreshToken 用 refresh token 刷新并返回完整 token 信息
// POST /api/v1/admin/codebuddy/oauth/refresh-token
func (h *CodeBuddyOAuthHandler) RefreshToken(c *gin.Context) {
	var req CodeBuddyRefreshTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "请求无效: "+err.Error())
		return
	}

	tokenInfo, err := h.codeBuddyOAuthService.RefreshToken(c.Request.Context(), req.RefreshToken, req.UID, req.EnterpriseID, req.Domain)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, tokenInfo)
}
