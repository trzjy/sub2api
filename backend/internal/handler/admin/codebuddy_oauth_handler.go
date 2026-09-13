package admin

import (
	"context"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// codeBuddyOAuthService 是 CodeBuddyOAuthHandler 依赖的凭证服务能力集。
// 生产实现为 *service.CodeBuddyOAuthService；抽成接口仅为让 handler 的审计
// 逻辑可在不发起上游网络请求的前提下单测（与 GrokOAuthHandler 注入 oauthClient 同思路）。
type codeBuddyOAuthService interface {
	GenerateAuthURL(ctx context.Context) (*service.CodeBuddyAuthURLResult, error)
	PollToken(ctx context.Context, state string) (*service.CodeBuddyTokenInfo, error)
	RefreshToken(ctx context.Context, refreshToken, uid, enterpriseID, domain string) (*service.CodeBuddyTokenInfo, error)
}

type CodeBuddyOAuthHandler struct {
	codeBuddyOAuthService codeBuddyOAuthService
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
	// 审计目标：上游签发的 state（后续 poll 的凭据句柄）。只记标识，不记 token 值。
	middleware.SetAuditExtra(c, map[string]any{"oauth_state": result.State})

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
	// 审计目标：本次轮询的 state。成功/失败/未完成均记录。
	middleware.SetAuditExtra(c, map[string]any{"oauth_state": req.State})

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
	// 审计目标：被刷新的账号 uid（refresh_token 值由审计中间件的请求体脱敏处理）。
	if uid := req.UID; uid != "" {
		middleware.SetAuditExtra(c, map[string]any{"target_uid": uid})
	}

	tokenInfo, err := h.codeBuddyOAuthService.RefreshToken(c.Request.Context(), req.RefreshToken, req.UID, req.EnterpriseID, req.Domain)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, tokenInfo)
}
