package admin

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// 伪装配置（identity disguise）的管理接口。
//
// 独立文件而非塞进 account_handler.go（已 3800+ 行）—— 与
// grok_media_eligibility_handler.go 同一组织形态（同一 AccountHandler 类型，
// 独立文件承载一组接口）。
//
// 写入走 adminService.UpdateAccountExtra（JSONB key 级合并），
// **只写 extra.identity_disguise 一个键**，不碰其它 extra 键、不碰老键。

type identityDisguiseProfileOption struct {
	ID    service.IdentityDisguiseProfileID `json:"id"`
	Label string                            `json:"label"`
	// NeedsForbiddenNames 表示该模板的禁词句需要按账号填充。
	NeedsForbiddenNames bool `json:"needs_forbidden_names"`
}

type identityDisguiseResponse struct {
	AccountID int64  `json:"account_id"`
	Profile   string `json:"profile"`
	// ForbiddenNames 是当前生效的禁词列表。
	ForbiddenNames []string `json:"forbidden_names"`
	RewriteModel   bool     `json:"rewrite_model"`
	StripReasoning bool     `json:"strip_reasoning"`
	// Prompt 是当前生效的人设正文（供运营核对）。
	Prompt string `json:"prompt"`
	// UsesLegacyKeys 报告该账号是否仍在用老键写法。
	UsesLegacyKeys bool `json:"uses_legacy_keys"`
}

type updateIdentityDisguiseRequest struct {
	// Profile 是目标身份（none = 不伪装）。
	Profile string `json:"profile"`
	// ForbiddenNames 是禁词列表；与 auto_derive_forbidden_names 二选一。
	ForbiddenNames []string `json:"forbidden_names"`
	// AutoDeriveForbidden 为真时按账号 model_mapping 自动推导禁词。
	AutoDeriveForbidden bool `json:"auto_derive_forbidden_names"`
	// RewriteModel / StripReasoning 是两个伪装面的开关。
	RewriteModel   bool `json:"rewrite_model"`
	StripReasoning bool `json:"strip_reasoning"`
}

// GetIdentityDisguiseProfiles 返回可选的身份模板目录（前端下拉用）。
//
// 刻意**不含模板正文** —— 正文是后端的唯一事实源，前端只拿 id + label，
// 不硬编码任何文案（否则模板改一处就要改两处）。
// GET /api/v1/admin/accounts/identity-disguise/profiles
func (h *AccountHandler) GetIdentityDisguiseProfiles(c *gin.Context) {
	opts := service.IdentityDisguiseProfileOptions()
	out := make([]identityDisguiseProfileOption, 0, len(opts))
	for _, o := range opts {
		out = append(out, identityDisguiseProfileOption{
			ID:                 o.ID,
			Label:              o.Label,
			NeedsForbiddenNames: o.NeedsForbiddenNames,
		})
	}
	response.Success(c, out)
}

// GetAccountIdentityDisguise 返回账号当前的伪装配置。
// GET /api/v1/admin/accounts/:id/identity-disguise
func (h *AccountHandler) GetAccountIdentityDisguise(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	state, err := h.adminService.GetAccountIdentityDisguise(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, buildIdentityDisguiseResponse(accountID, state))
}

// UpdateAccountIdentityDisguise 更新账号的伪装配置。
//
// 只写 extra.identity_disguise（JSONB key 级合并），老键与其它 extra 键不受影响。
// PUT /api/v1/admin/accounts/:id/identity-disguise
func (h *AccountHandler) UpdateAccountIdentityDisguise(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}

	var req updateIdentityDisguiseRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	update := service.IdentityDisguiseUpdate{
		Profile:             service.IdentityDisguiseProfileID(req.Profile),
		ForbiddenNames:      req.ForbiddenNames,
		AutoDeriveForbidden: req.AutoDeriveForbidden,
		RewriteModel:        req.RewriteModel,
		StripReasoning:      req.StripReasoning,
	}
	if err := service.ValidateIdentityDisguiseUpdate(update); err != nil {
		response.ErrorFrom(c, err)
		return
	}

	state, err := h.adminService.SetAccountIdentityDisguise(c.Request.Context(), accountID, update)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, buildIdentityDisguiseResponse(accountID, state))
}

// buildIdentityDisguiseResponse 把服务层状态转成接口响应。
func buildIdentityDisguiseResponse(accountID int64, state *service.IdentityDisguiseState) identityDisguiseResponse {
	out := identityDisguiseResponse{AccountID: accountID}
	if state == nil {
		return out
	}
	out.Profile = string(state.Profile)
	out.ForbiddenNames = state.ForbiddenNames
	out.RewriteModel = state.RewriteModel
	out.StripReasoning = state.StripReasoning
	out.Prompt = state.Prompt
	out.UsesLegacyKeys = state.UsesLegacyKeys
	return out
}
