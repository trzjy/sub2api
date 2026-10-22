package admin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// QuotaKeyHandler 是 Cursor 激活码（quota key）的 admin 管理面（方案 §4.4，8 端点）。
//
// 严格约束：
//   - 全部端点复用既有 QuotaKeyService（List/Create/Update/GetWithAccounts/
//     DeleteWithAccounts/RefreshAccounts/ClearExhausted），不新增 service 方法；
//   - 明文激活码只进不出（F1）：接收一次即加密落库，任何响应只回 code_masked；
//   - 删除唯一路径是 DeleteWithAccounts（软删，F5），禁物理删除账号行；
//   - 不引入新中间件 / 新分页机制 / 新响应信封：复用 response.ParsePagination
//     与既有分页字段（items/total/page/page_size/pages）。
type QuotaKeyHandler struct {
	svc       *service.QuotaKeyService
	sweeper   *service.QuotaKeySweeper
	autoEntry *service.CursorAutoEntryService
}

// NewQuotaKeyHandler 构造激活码管理端点处理器。
// sweeper 为 nil 时 sweep_expired 端点返回 503（未装配清扫器），其余端点不受影响。
// autoEntry 为 nil 时 auto_entry_config / probe_quota 端点返回 503（未装配入库可配置服务）。
func NewQuotaKeyHandler(svc *service.QuotaKeyService, sweeper *service.QuotaKeySweeper, autoEntry *service.CursorAutoEntryService) *QuotaKeyHandler {
	return &QuotaKeyHandler{svc: svc, sweeper: sweeper, autoEntry: autoEntry}
}

// quotaKeySummaryView 是激活码的展示投影：既有脱敏标量 + 来源额度快照投影
// （方案 §25.6 消费闭包⑤⑥ / §25.7-B）。
//
// 顶层 quota_key_summaries 与列表 items 使用同一类型、同一份值：
// 因此 quota key 列表/详情/probe 与账号页拿到的快照值天然一致（§25.8 lite/full 同值）。
// Usage 为 nil 时按 omitempty 省略——无快照不等于 0，前端不得按 0 渲染。
type quotaKeySummaryView struct {
	dto.QuotaKey
	Usage *service.QuotaKeyUsageProjection `json:"usage,omitempty"`
}

// quotaKeySummaryEnvelope 复用既有分页信封（response.PaginatedData），在顶层追加
// quota_key_summaries（§4.4 / §4.5）。omitempty 保证无汇总时 JSON 形态与既有完全一致。
type quotaKeySummaryEnvelope struct {
	response.PaginatedData
	QuotaKeySummaries []quotaKeySummaryView `json:"quota_key_summaries,omitempty"`
}

// respondQuotaKeySummaries 输出带顶层 quota_key_summaries 的分页响应。
// 汇总一次性由调用方给出（杜绝 N+1：与页数、条目数无关的常数次查询）。
func respondQuotaKeySummaries(c *gin.Context, items any, total int64, page, pageSize int, summaries []quotaKeySummaryView) {
	if pageSize < 1 {
		pageSize = 1
	}
	pages := int(math.Ceil(float64(total) / float64(pageSize)))
	if pages < 1 {
		pages = 1
	}
	response.Success(c, quotaKeySummaryEnvelope{
		PaginatedData: response.PaginatedData{
			Items:    items,
			Total:    total,
			Page:     page,
			PageSize: pageSize,
			Pages:    pages,
		},
		QuotaKeySummaries: summaries,
	})
}

// maskedQuotaKey 把 service 层脱敏状态投影成只含安全子集的展示对象（绝不含原文）。
//
// 纯投影：不接收计数、也不查询数据。计数由调用方（List）在拿到服务层口径的聚合结果后
// 覆盖到返回对象上，故 Create / Update 等复用点行为不变（其计数保持零值，omitempty 下
// JSON 形态与接入计数前完全一致）。
func maskedQuotaKey(st *service.QuotaKeyStatus) dto.QuotaKey {
	return dto.QuotaKey{
		ID:         st.ID,
		CodeMasked: st.CodeValueMasked,
		Status:     st.Status,
		Exhausted:  st.Exhausted,
	}
}

// List 列出全部激活码（仅 code_masked，绝不返原文），并回填每个激活码的账号计数
// （account_count / working_count）。
// GET /api/v1/admin/quota_keys
//
// 计数走 service.CountAccountsByQuotaKeys（一次批量查询 + 内存分组），与详情端点 Get
// 同源同谓词（未软删 DeletedAt==nil；working = Schedulable && Status=="active"），
// 不在此处另写一份计数循环，也杜绝逐激活码查询的 N+1。
func (h *QuotaKeyHandler) List(c *gin.Context) {
	// 一次仓储查询同时取脱敏状态与额度快照投影（同一行记录解析，常量次查询、无网络探测）。
	statuses, err := h.svc.ListWithUsageProjections(c.Request.Context())
	if err != nil {
		respondQuotaKeyError(c, err)
		return
	}
	ids := make([]int64, 0, len(statuses))
	for _, st := range statuses {
		ids = append(ids, st.ID)
	}
	// 计数查询失败按既有错误路径返回，绝不吞错填 0（禁兜底条款）。
	counts, err := h.svc.CountAccountsByQuotaKeys(c.Request.Context(), ids)
	if err != nil {
		respondQuotaKeyError(c, err)
		return
	}
	items := make([]quotaKeySummaryView, 0, len(statuses))
	for _, st := range statuses {
		item := quotaKeySummaryView{QuotaKey: maskedQuotaKey(st), Usage: st.Usage}
		// service 保证对每个入参 id 都给出一个条目；直接取用即 service 口径，不设零值兜底分支。
		cnt := counts[st.ID]
		item.AccountCount = cnt.AccountCount
		item.WorkingCount = cnt.WorkingCount
		items = append(items, item)
	}
	// items 与 quota_key_summaries 传的是同一切片，天然同批对象、计数与快照一致。
	respondQuotaKeySummaries(c, items, int64(len(items)), 1, len(items), items)
}

type createQuotaKeyRequest struct {
	// Code 明文激活码，仅本次请求进站后即加密落库；返回值中绝不再出现。
	Code      string     `json:"code" binding:"required"`
	ExpiresAt *time.Time `json:"expires_at"`
	Notes     *string    `json:"notes"`
}

// Create 新建激活码：接 code 原文一次 → 加密落库 → 返回脱敏结果。
// POST /api/v1/admin/quota_keys
func (h *QuotaKeyHandler) Create(c *gin.Context) {
	var req createQuotaKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	req.Code = strings.TrimSpace(req.Code)
	if req.Code == "" {
		response.BadRequest(c, "code is required")
		return
	}
	st, err := h.svc.Create(c.Request.Context(), req.Code, req.ExpiresAt, req.Notes)
	if err != nil {
		respondQuotaKeyError(c, err)
		return
	}
	response.Created(c, maskedQuotaKey(st))
}

// quotaKeyAccountItem 是详情端点里账号分页项的最小投影。
// 刻意不内嵌完整账号/凭据对象：这里只需要展示归属关系与状态（F1/F7）。
type quotaKeyAccountItem struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Platform  string    `json:"platform"`
	Type      string    `json:"type"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

// quotaKeyDetailResponse 是详情端点的响应：脱敏 status + 账号分页。
// 不嵌套完整 QuotaKey 聚合账号列表以外的字段，也不回放 code 原文。
type quotaKeyDetailResponse struct {
	*service.QuotaKeyStatus
	// AccountCount = 未软删归属账号总数；WorkingCount = 其中可调度（schedulable+active）
	// 数。用户验收口径（2026-10-15）：详情弹窗"账号数/可用账号数"必须真实反映，缺省 0 误导。
	AccountCount int64                 `json:"account_count"`
	WorkingCount int64                 `json:"working_count"`
	Accounts     []quotaKeyAccountItem `json:"accounts"`
	Total        int64                 `json:"total"`
	Page         int                   `json:"page"`
	PageSize     int                   `json:"page_size"`
}

// Get 读取单个激活码详情（脱敏 status + 账号分页）。
// GET /api/v1/admin/quota_keys/:id
func (h *QuotaKeyHandler) Get(c *gin.Context) {
	id, ok := parseQuotaKeyID(c)
	if !ok {
		return
	}
	status, accounts, err := h.svc.GetWithAccounts(c.Request.Context(), id)
	if err != nil {
		respondQuotaKeyError(c, err)
		return
	}
	// §25.6⑤：详情与列表/probe 展示同一份**已提交**快照投影（只读，不发网络查询）。
	// 读取失败仅降级省略该字段：不填 0、不伪造"无额度"（缺失不是 0）。
	usage, usageErr := h.svc.QuotaKeyUsageProjectionFor(c.Request.Context(), id)
	if usageErr != nil {
		slog.Warn("admin_quota_key_usage_projection_failed", "quota_key_id", id, "error", usageErr)
	} else {
		status.Usage = usage
	}
	page, pageSize := response.ParsePagination(c)
	total := int64(len(accounts))
	start := int64((page - 1) * pageSize)
	end := start + int64(pageSize)
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}
	items := make([]quotaKeyAccountItem, 0, end-start)
	for _, acc := range accounts[start:end] {
		items = append(items, quotaKeyAccountItem{
			ID:        acc.ID,
			Name:      acc.Name,
			Platform:  acc.Platform,
			Type:      acc.Type,
			Status:    acc.Status,
			CreatedAt: acc.CreatedAt,
		})
	}
	// 计数口径收敛到 service helper（未软删 + working 谓词），此处不再自持第二份循环。
	accountCount, workingCount := service.CountQuotaKeyAccountStats(accounts)
	response.Success(c, quotaKeyDetailResponse{
		QuotaKeyStatus: status,
		AccountCount:   accountCount,
		WorkingCount:   workingCount,
		Accounts:       items,
		Total:          total,
		Page:           page,
		PageSize:       pageSize,
	})
}

type updateQuotaKeyRequest struct {
	// 注意：status 不可编辑（§4.4）。请求体刻意不声明 status 字段，
	// 因此该维度在服务边界之外即无法被改写。
	Code      *string         `json:"code"`
	ExpiresAt json.RawMessage `json:"expires_at"`
	Notes     json.RawMessage `json:"notes"`
}

// Update 编辑激活码（code / expires_at / notes；status 不允许改）。
// PUT /api/v1/admin/quota_keys/:id
func (h *QuotaKeyHandler) Update(c *gin.Context) {
	id, ok := parseQuotaKeyID(c)
	if !ok {
		return
	}
	var req updateQuotaKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if req.Code != nil {
		trimmed := strings.TrimSpace(*req.Code)
		if trimmed == "" {
			response.BadRequest(c, "code must not be empty")
			return
		}
		req.Code = &trimmed
	}
	var expiresAt **time.Time
	if req.ExpiresAt != nil {
		var value *time.Time
		if err := json.Unmarshal(req.ExpiresAt, &value); err != nil {
			response.BadRequest(c, "Invalid expires_at: "+err.Error())
			return
		}
		expiresAt = &value
	}
	var notes **string
	if req.Notes != nil {
		var value *string
		if err := json.Unmarshal(req.Notes, &value); err != nil {
			response.BadRequest(c, "Invalid notes: "+err.Error())
			return
		}
		notes = &value
	}
	st, err := h.svc.Update(c.Request.Context(), id, req.Code, expiresAt, notes)
	if err != nil {
		respondQuotaKeyError(c, err)
		return
	}
	response.Success(c, maskedQuotaKey(st))
}

// Delete 删除激活码及其全部归属账号：走 DeleteWithAccounts（软删唯一路径，F5）。
// DELETE /api/v1/admin/quota_keys/:id
func (h *QuotaKeyHandler) Delete(c *gin.Context) {
	id, ok := parseQuotaKeyID(c)
	if !ok {
		return
	}
	if err := h.svc.DeleteWithAccounts(c.Request.Context(), id); err != nil {
		respondQuotaKeyError(c, err)
		return
	}
	response.Success(c, gin.H{"deleted": true})
}

// RefreshAccounts 手动拉号（刷新该激活码下的账号池）。
// POST /api/v1/admin/quota_keys/:id/refresh
func (h *QuotaKeyHandler) RefreshAccounts(c *gin.Context) {
	id, ok := parseQuotaKeyID(c)
	if !ok {
		return
	}
	if err := h.svc.RefreshAccounts(c.Request.Context(), id); err != nil {
		respondQuotaKeyError(c, err)
		return
	}
	response.Success(c, gin.H{"refreshed": true})
}

// ClearExhausted 仅解除 exhausted 标记，不触发拉号、不写审计。
// POST /api/v1/admin/quota_keys/:id/clear_exhausted
func (h *QuotaKeyHandler) ClearExhausted(c *gin.Context) {
	id, ok := parseQuotaKeyID(c)
	if !ok {
		return
	}
	if err := h.svc.ClearExhausted(c.Request.Context(), id); err != nil {
		respondQuotaKeyError(c, err)
		return
	}
	response.Success(c, gin.H{"cleared": true})
}

// SweepExpired 手动触发一轮到期清扫（过期激活码标 expired），返回本轮改写条数。
// POST /api/v1/admin/quota_keys/sweep_expired
func (h *QuotaKeyHandler) SweepExpired(c *gin.Context) {
	if h.sweeper == nil {
		response.Error(c, http.StatusServiceUnavailable, "Quota key sweeper unavailable")
		return
	}
	n, err := h.sweeper.SweepNow(c.Request.Context())
	if err != nil {
		respondQuotaKeyError(c, err)
		return
	}
	response.Success(c, gin.H{"swept": n})
}

// parseQuotaKeyID 解析并校验路径参数 :id；非法时自行返回 400。
func parseQuotaKeyID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid quota key id")
		return 0, false
	}
	return id, true
}

// respondQuotaKeyError 统一映射激活码领域的错误到 HTTP。
// 不存在（软删行不可见）规范映射到 404，其余沿用既有 response.ErrorFrom。
//
// R1-1：手动刷新上游失败/终态结果（*service.UpstreamPullError）须把上游中文 msg 原文
// 逐字回显到响应 message（HTTP 502，无英文前缀/映射；原文仍是错误信息、非 success）。
// 不得改 response 包 / 不得改 FromError 全局行为——仅在此 handler 层做一次性翻译。
// 其他错误走默认映射（FromError 对非 ApplicationError 替换 message 为 "internal error" 的
// 既有行为不受影响）。
func respondQuotaKeyError(c *gin.Context, err error) {
	if errors.Is(err, service.ErrQuotaKeyNotFound) || ent.IsNotFound(err) {
		response.ErrorFrom(c, infraerrors.NotFound("QUOTA_KEY_NOT_FOUND", "quota key not found"))
		return
	}
	var upe *service.UpstreamPullError
	if errors.As(err, &upe) {
		response.ErrorFrom(c, infraerrors.New(http.StatusBadGateway, "QUOTA_KEY_UPSTREAM", upe.Message()))
		return
	}
	response.ErrorFrom(c, err)
}

// GetAutoEntryConfig 读取 cursor 入库可配置（厂商勾选 + 模型清单 + 入库参数）。
// GET /api/v1/admin/quota_keys/auto_entry_config
func (h *QuotaKeyHandler) GetAutoEntryConfig(c *gin.Context) {
	if h.autoEntry == nil {
		response.Error(c, http.StatusServiceUnavailable, "Cursor auto-entry service unavailable")
		return
	}
	resp, err := h.autoEntry.GetConfigResponse(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, resp)
}

type updateAutoEntryConfigRequest struct {
	Vendors       map[string]bool             `json:"vendors"`
	Models        []service.CursorModelRef    `json:"models"`
	EntryDefaults service.CursorEntryDefaults `json:"entry_defaults"`
}

// UpdateAutoEntryConfig 校验并持久化 cursor 入库可配置。
// PUT /api/v1/admin/quota_keys/auto_entry_config
func (h *QuotaKeyHandler) UpdateAutoEntryConfig(c *gin.Context) {
	if h.autoEntry == nil {
		response.Error(c, http.StatusServiceUnavailable, "Cursor auto-entry service unavailable")
		return
	}
	var req updateAutoEntryConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	cfg := &service.CursorAutoEntryConfig{
		Vendors:       req.Vendors,
		Models:        req.Models,
		EntryDefaults: req.EntryDefaults,
	}
	updated, err := h.autoEntry.UpdateConfig(c.Request.Context(), cfg)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, updated)
}

// probeQuotaResponse 是 probe 响应：既有 lookup 摘要 + 同一份快照投影（§25.6⑤）。
//
// 嵌入既有 *service.KeyQuotaLookupInfo 保持原有 JSON 形态不变，仅新增 usage；
// usage 取自**已提交**的 quota_keys.usage_snapshot（本单只读：快照写入者由后续单落地），
// 因此 probe 返回与列表/详情/账号页看到的是同一个值，不存在"探测结果直接当已提交事实"。
type probeQuotaResponse struct {
	*service.KeyQuotaLookupInfo
	Usage *service.QuotaKeyUsageProjection `json:"usage,omitempty"`
}

// ProbeQuota 手动触发某激活码的 key 级用量查询（lookup），并把额度快照写入其全部账号 extra。
// 返回 key_info 摘要（状态/额度/设备/到期等）+ 已提交快照投影。
// POST /api/v1/admin/quota_keys/:id/probe_quota
func (h *QuotaKeyHandler) ProbeQuota(c *gin.Context) {
	if h.autoEntry == nil {
		response.Error(c, http.StatusServiceUnavailable, "Cursor auto-entry service unavailable")
		return
	}
	id, ok := parseQuotaKeyID(c)
	if !ok {
		return
	}
	info, err := h.autoEntry.ProbeKeyQuota(c.Request.Context(), id)
	if err != nil {
		respondQuotaKeyError(c, err)
		return
	}
	body := probeQuotaResponse{KeyQuotaLookupInfo: info}
	if h.svc != nil {
		usage, usageErr := h.svc.QuotaKeyUsageProjectionFor(c.Request.Context(), id)
		if usageErr != nil {
			slog.Warn("admin_quota_key_probe_usage_projection_failed", "quota_key_id", id, "error", usageErr)
		} else {
			body.Usage = usage
		}
	}
	response.Success(c, body)
}
