package admin

import (
	"context"
	"strings"

	"github.com/Wei-Shaw/sub2api/ent"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"

	"github.com/gin-gonic/gin"
)

// QuotaUpstreamHandler 管理 quota_upstreams 注册表的 admin 端点（CARD-KC-18）：
//
//   - GET  /api/v1/admin/quota_upstreams       列出全部上游条目（标识 + 显示名 + 类型 + 续费 URL）
//   - PUT  /api/v1/admin/quota_upstreams/:key  仅允许改显示名 / 续费 URL / 备注
//
// 严格约束：
//   - 复用 KC-16 提供的 repository.QuotaUpstreamRepository（最小 CRUD），不在此写 ent 查询；
//   - 上游标识 key 与类型 type 不可改（断 quota_keys.upstream_key 引用 / 改变拉号客户端语义），
//     请求体若携带这两个字段一律拒绝；
//   - 响应体不返回任何凭据（上游条目里没有凭据，本 handler 也不新增）。
//
// 路由注册由主会话在 routes/admin.go 接线（本卡为避免与 KC-21 跨文件冲突，不改动该文件）。
type QuotaUpstreamHandler struct {
	// repo 抽象 KC-16 提供的仓储接口（仅声明本 handler 实际用到的方法），
	// 便于单测以替身注入，也避免依赖 repository 包内未导出的具体类型。
	repo quotaUpstreamRepository
}

// quotaUpstreamRepository 是本 handler 对 KC-16 仓储的最小依赖面。
// 具体类型 *repository.QuotaUpstreamRepository 满足该接口（方法集一致）。
type quotaUpstreamRepository interface {
	GetByKey(ctx context.Context, key string) (*ent.QuotaUpstream, error)
	List(ctx context.Context) ([]*ent.QuotaUpstream, error)
	Update(ctx context.Context, id int64, name string, renewalURL **string, notes **string) error
}

// NewQuotaUpstreamHandler 构造上游条目管理端点处理器。
func NewQuotaUpstreamHandler(repo quotaUpstreamRepository) *QuotaUpstreamHandler {
	return &QuotaUpstreamHandler{repo: repo}
}

// quotaUpstreamView 是上游条目的展示投影：标识 + 显示名 + 类型 + 续费 URL（+ 备注）。
// 不含任何凭据。
type quotaUpstreamView struct {
	ID         int64   `json:"id"`
	Key        string  `json:"key"`
	Name       string  `json:"name"`
	Type       string  `json:"type"`
	RenewalURL *string `json:"renewal_url,omitempty"`
	Notes      *string `json:"notes,omitempty"`
}

func toQuotaUpstreamView(u *ent.QuotaUpstream) quotaUpstreamView {
	return quotaUpstreamView{
		ID:         u.ID,
		Key:        u.Key,
		Name:       u.Name,
		Type:       u.Type,
		RenewalURL: u.RenewalURL,
		Notes:      u.Notes,
	}
}

// List 列出全部上游条目（标识 + 显示名 + 类型 + 续费 URL）。
// GET /api/v1/admin/quota_upstreams
func (h *QuotaUpstreamHandler) List(c *gin.Context) {
	items, err := h.repo.List(c.Request.Context())
	if err != nil {
		respondQuotaUpstreamError(c, err)
		return
	}
	views := make([]quotaUpstreamView, 0, len(items))
	for _, u := range items {
		views = append(views, toQuotaUpstreamView(u))
	}
	response.Success(c, gin.H{"items": views})
}

// updateQuotaUpstreamRequest 是 PUT /quota_upstreams/:key 的请求体。
//
// 仅 name / renewal_url / notes 可写；key 与 type 为防御性字段——
// 请求体若携带即拒绝，绝不允许改标识或类型。
type updateQuotaUpstreamRequest struct {
	Name       *string `json:"name"`
	RenewalURL *string `json:"renewal_url"`
	Notes      *string `json:"notes"`
	// 以下两个字段不可改：出现即视为非法改写企图。
	Key  *string `json:"key"`
	Type *string `json:"type"`
}

// Update 编辑上游条目：仅允许改显示名 / 续费 URL / 备注。
// 标识 key 与类型 type 不可改（断引用 / 语义漂移）。
// PUT /api/v1/admin/quota_upstreams/:key
func (h *QuotaUpstreamHandler) Update(c *gin.Context) {
	key := strings.TrimSpace(c.Param("key"))
	if key == "" {
		response.BadRequest(c, "Invalid upstream key")
		return
	}
	var req updateQuotaUpstreamRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	// 标识与类型不可改：任何改写企图都拒绝，避免打断 quota_keys.upstream_key 引用
	// 或改变拉号客户端语义（R2/R3 裁定）。
	if req.Key != nil || req.Type != nil {
		response.BadRequest(c, "upstream key and type are immutable")
		return
	}
	if req.Name == nil {
		response.BadRequest(c, "name is required")
		return
	}
	name := strings.TrimSpace(*req.Name)
	if name == "" {
		response.BadRequest(c, "name must not be empty")
		return
	}

	u, err := h.repo.GetByKey(c.Request.Context(), key)
	if err != nil {
		respondQuotaUpstreamError(c, err)
		return
	}

	// renewal_url / notes：外层指针为 nil 表示不改；非 nil 时内层 nil 表示显式清空
	// （仅 renewal_url / notes 可空）。
	var renewalURL **string
	if req.RenewalURL != nil {
		v := req.RenewalURL
		renewalURL = &v
	}
	var notes **string
	if req.Notes != nil {
		v := req.Notes
		notes = &v
	}

	if err := h.repo.Update(c.Request.Context(), u.ID, name, renewalURL, notes); err != nil {
		respondQuotaUpstreamError(c, err)
		return
	}
	// 回读最新快照，使前端编辑后立刻拿到新名字（含激活码列表里的显示名）。
	updated, err := h.repo.GetByKey(c.Request.Context(), key)
	if err != nil {
		respondQuotaUpstreamError(c, err)
		return
	}
	response.Success(c, toQuotaUpstreamView(updated))
}

// respondQuotaUpstreamError 统一映射上游管理领域的错误到 HTTP。
// 不存在（软删行不可见）规范映射到 404，其余沿用既有 response.ErrorFrom。
func respondQuotaUpstreamError(c *gin.Context, err error) {
	if ent.IsNotFound(err) {
		response.ErrorFrom(c, infraerrors.NotFound("QUOTA_UPSTREAM_NOT_FOUND", "quota upstream not found"))
		return
	}
	response.ErrorFrom(c, err)
}
