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
	// adminService 供管理端账号写入复用（kn-cy 旁路激活建号入口已于 KC-21 删除，
	// kn-cy 卡密改走既有号池链路建号；此依赖暂留以免改动 NewQuotaKeyHandler 签名与装配）。
	adminService service.AdminService
	// accountTestService 供 SyncUpstreamModels 借用该激活码名下账号做上游模型预览探测；
	// 可选依赖，未装配时 SyncUpstreamModels 返回 503。保持 NewQuotaKeyHandler 原 4 参签名不变。
	accountTestService *service.AccountTestService
}

// NewQuotaKeyHandler 构造激活码管理端点处理器。
// sweeper 为 nil 时 sweep_expired 端点返回 503（未装配清扫器），其余端点不受影响。
// autoEntry 为 nil 时 probe_quota 端点返回 503（未装配入库可配置服务）。
func NewQuotaKeyHandler(svc *service.QuotaKeyService, sweeper *service.QuotaKeySweeper, autoEntry *service.CursorAutoEntryService, adminService service.AdminService) *QuotaKeyHandler {
	return &QuotaKeyHandler{svc: svc, sweeper: sweeper, autoEntry: autoEntry, adminService: adminService}
}

// SetAccountTestService 装配上游模型预览探测服务（SyncUpstreamModels 依赖）。
// 与 NewQuotaKeyHandler 分离，避免改动既有 4 参构造签名、不破坏现有测试。
func (h *QuotaKeyHandler) SetAccountTestService(svc *service.AccountTestService) {
	h.accountTestService = svc
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
	// UsableCount 是「现在真的能跑」的账号数 = working 计数，但来源额度已耗尽或密钥无效时归零。
	// 与 working_count 并存而不是替换：working_count 回答"这个码手里有几个号"，
	// usable_count 回答"这几个号此刻能不能用"。两者不等就是故障现场的真实写照。
	UsableCount int64 `json:"usable_count"`
	// SourceBlockReason 非空时说明可用数为什么是 0（quota_exhausted / key_invalid），
	// 供前端直接出人话文案，不必自己再推一遍。
	SourceBlockReason string `json:"source_block_reason,omitempty"`
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

// maskedQuotaKey 是 service 状态 → dto.QuotaKey 的**唯一**投影点：
// 投影"行级完整展示元数据 + 脱敏激活码"（ID / code_masked / status / exhausted /
// expires_at / last_refresh_at / last_refresh_error / notes）。
//
// 列表（List）、详情（Get）与账号页顶层汇总（quota_key_summaries）共用同一份投影，
// 保证各端点拿到同一行记录的同批值；**禁止**任何调用点再内联复制字段子集
// （多轨投影正是本轮列表缺 expires_at/last_refresh_at/last_refresh_error/notes、
// 编辑弹窗回填把备注静默清空的展示漂移根因）。
//
// 绝不含激活码原文（F1），仅回放 code_value_masked。
// 纯投影：不接收计数、也不查询数据。account_count/working_count 由调用方（List）
// 在拿到服务层口径的聚合结果后覆盖到返回对象上，故 Create / Update 等复用点行为
// 不变（其计数保持零值，omitempty 下 JSON 形态与接入计数前完全一致）。
func maskedQuotaKey(st *service.QuotaKeyStatus) dto.QuotaKey {
	return dto.QuotaKey{
		ID:               st.ID,
		CodeMasked:       st.CodeValueMasked,
		Status:           st.Status,
		Exhausted:        st.Exhausted,
		ExpiresAt:        st.ExpiresAt,
		LastRefreshAt:    st.LastRefreshAt,
		LastRefreshError: st.LastRefreshError,
		Notes:            st.Notes,
		Priority:         st.Priority,
		UpstreamKey:      st.UpstreamKey,
		// B 批上移：码级并发数 / 负载因子 / 计费倍率 / 模型映射，与 service.QuotaKeyStatus
		// 同源回放，供前端编辑弹窗回填（禁多轨投影漂移）。
		Concurrency:    st.Concurrency,
		LoadFactor:     st.LoadFactor,
		RateMultiplier: st.RateMultiplier,
		ModelMapping:   st.ModelMapping,
		// GroupIDs 码级权威分组：与 service.QuotaKeyStatus 同源回放（nil=未指定，空切片=解绑全部）。
		GroupIDs: st.GroupIDs,
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
		// 来源额度折算：码没额度（或被判密钥无效）时可用数归零，并给出归因。
		item.SourceBlockReason = service.QuotaKeySourceBlockReason(st.Usage)
		item.UsableCount = service.UsableAccountsForSource(cnt.WorkingCount, st.Usage)
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
	// Priority 是调度优先级（数值越小越优先）；缺省 0 → 服务端按列默认 1 落库。
	// 该码名下账号的 priority 跟随此值。
	Priority int `json:"priority"`
	// UpstreamKey 是新建激活码所属上游的稳定标识（引用 quota_upstreams.key）。
	// 缺省（omitempty，空串）表示未指定，由仓储层落列默认值（'shareai'）。
	UpstreamKey string `json:"upstream_key"`

	// B 批上移：创建时一并落库的码级设置（并发数 / 负载因子 / 计费倍率 / 模型映射）。
	// 这些字段缺省表示不覆盖（沿用拉号上游默认实供）。model_mapping 仅在显式给出
	// 对象（含 {}）时设置 ModelMappingSet=true；空映射由服务层 400 拒绝落库。
	Concurrency    *int            `json:"concurrency"`
	LoadFactor     *int            `json:"load_factor"`
	RateMultiplier *float64        `json:"rate_multiplier"`
	ModelMapping   json.RawMessage `json:"model_mapping"`
	// GroupIDs 是新建激活码的码级权威分组：nil / null 表示不设置（GroupIDsSet=false，落 NULL）；
	// 数组（含 []）表示显式设置（GroupIDsSet=true，空切片 = 解绑全部）。
	GroupIDs json.RawMessage `json:"group_ids"`
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

	// 解析 model_mapping：仅在请求 JSON 显式给出对象（含 {}）时设置 ModelMappingSet=true；
	// 传 null / 未提供则不设置自定义映射（沿用上游默认实供）。
	var modelMappingSet bool
	var modelMapping map[string]string
	if len(req.ModelMapping) > 0 && string(req.ModelMapping) != "null" {
		if err := json.Unmarshal(req.ModelMapping, &modelMapping); err != nil {
			response.BadRequest(c, "Invalid model_mapping: "+err.Error())
			return
		}
		modelMappingSet = true
	}

	settings := &service.UpdateQuotaKeySettingsInput{
		ModelMappingSet: modelMappingSet,
		ModelMapping:    modelMapping,
	}
	if req.Concurrency != nil {
		c := *req.Concurrency
		cp := &c
		settings.Concurrency = &cp
	}
	if req.LoadFactor != nil {
		l := *req.LoadFactor
		lp := &l
		settings.LoadFactor = &lp
	}
	if req.RateMultiplier != nil {
		r := *req.RateMultiplier
		settings.RateMultiplier = &r
	}

	// 解析 group_ids：nil / null 表示不设置（GroupIDsSet=false，落 NULL）；数组（含 []）表示显式设置。
	var groupIDs *[]int64
	if len(req.GroupIDs) > 0 && string(req.GroupIDs) != "null" {
		var ids []int64
		if err := json.Unmarshal(req.GroupIDs, &ids); err != nil {
			response.BadRequest(c, "Invalid group_ids: "+err.Error())
			return
		}
		groupIDs = &ids
	}
	settings.GroupIDs = groupIDs

	st, err := h.svc.CreateWithSettings(c.Request.Context(), req.Code, req.ExpiresAt, req.Notes, req.Priority, req.UpstreamKey, settings)
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
	AccountCount int64 `json:"account_count"`
	WorkingCount int64 `json:"working_count"`
	// UsableCount / SourceBlockReason 见 quotaKeySummaryView 同名注释（同一口径，列表与详情同源）。
	UsableCount       int64                 `json:"usable_count"`
	SourceBlockReason string                `json:"source_block_reason,omitempty"`
	Accounts          []quotaKeyAccountItem `json:"accounts"`
	Total             int64                 `json:"total"`
	Page              int                   `json:"page"`
	PageSize          int                   `json:"page_size"`
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
	var usage *service.QuotaKeyUsageProjection
	if proj, usageErr := h.svc.QuotaKeyUsageProjectionFor(c.Request.Context(), id); usageErr != nil {
		slog.Warn("admin_quota_key_usage_projection_failed", "quota_key_id", id, "error", usageErr)
	} else {
		usage = proj
		status.Usage = proj
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
		QuotaKeyStatus:    status,
		AccountCount:      accountCount,
		WorkingCount:      workingCount,
		UsableCount:       service.UsableAccountsForSource(workingCount, usage),
		SourceBlockReason: service.QuotaKeySourceBlockReason(usage),
		Accounts:          items,
		Total:             total,
		Page:              page,
		PageSize:          pageSize,
	})
}

type updateQuotaKeyRequest struct {
	// 注意：status 不可编辑（§4.4）。请求体刻意不声明 status 字段，
	// 因此该维度在服务边界之外即无法被改写。
	Code      *string         `json:"code"`
	ExpiresAt json.RawMessage `json:"expires_at"`
	Notes     json.RawMessage `json:"notes"`
	// Priority 为 nil 表示本次不改优先级；给出时必须是正整数（数值越小越优先），
	// 服务端会在同一事务内把该码名下未删除账号的 priority 一并同步。
	Priority *int `json:"priority"`

	// B 批上移：编辑时一并更新的码级设置（并发数 / 负载因子 / 计费倍率 / 模型映射）。
	// concurrency / load_factor 用 json.RawMessage 表达三态（未传 / null / 值）；
	// rate_multiplier / model_mapping 同口径（model_mapping 显式出现即 ModelMappingSet=true）。
	Concurrency    json.RawMessage `json:"concurrency"`
	LoadFactor     json.RawMessage `json:"load_factor"`
	RateMultiplier *float64        `json:"rate_multiplier"`
	ModelMapping   json.RawMessage `json:"model_mapping"`
	// GroupIDs 三态（与 update 的 concurrency/load_factor 同型，但与 model_mapping 的 null 语义区分）：
	// 未传（nil）→ 不修改；null → 不修改（清空请传 []）；[] → 解绑全部；[…] → 覆盖。
	GroupIDs json.RawMessage `json:"group_ids"`
}

// newNilIntPointer 返回指向 nil *int 的指针（**int 的内层 nil），表达"清空/恢复默认"。
func newNilIntPointer() **int {
	var inner *int
	return &inner
}

// parseTriStateInt 解析并发数 / 负载因子的三态入参：
//   - raw == nil（未传）→ 返回 (nil, nil) 表示外层不修改；
//   - raw == "null" 或 值 <= 0 → 返回 (指向 nil 的 **int, nil) 表示清空；
//   - 正整数 → 返回 (指向该值的 **int, nil)；
//   - 解析失败 → 返回 (nil, err)。
//
// 超界（如 load_factor > 10000）此处不拦截，交由服务层 normalizeQuotaKeySettings 返回 400，
// 保证 handler 与 service 的校验口径唯一。
func parseTriStateInt(raw json.RawMessage) (**int, error) {
	if raw == nil {
		return nil, nil
	}
	if string(raw) == "null" {
		return newNilIntPointer(), nil
	}
	var val int
	if err := json.Unmarshal(raw, &val); err != nil {
		return nil, err
	}
	if val <= 0 {
		return newNilIntPointer(), nil
	}
	v := val
	inner := &v
	return &inner, nil
}

// Update 编辑激活码（code / expires_at / notes / 码级设置；status 不允许改）。
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

	settings := &service.UpdateQuotaKeySettingsInput{}
	concurrency, err := parseTriStateInt(req.Concurrency)
	if err != nil {
		response.BadRequest(c, "Invalid concurrency: "+err.Error())
		return
	}
	settings.Concurrency = concurrency
	loadFactor, err := parseTriStateInt(req.LoadFactor)
	if err != nil {
		response.BadRequest(c, "Invalid load_factor: "+err.Error())
		return
	}
	settings.LoadFactor = loadFactor
	if req.RateMultiplier != nil {
		rateMultiplier := *req.RateMultiplier
		settings.RateMultiplier = &rateMultiplier
	}
	// model_mapping：只要在请求 JSON 中出现（len > 0），一律反序列化为 map[string]string
	//（null 解析成空 map），并设 ModelMappingSet=true——按用户裁定"没有任何支持的模型就禁止
	// 落库"，服务层将拒绝空映射并返回 400。
	if len(req.ModelMapping) > 0 {
		var parsed map[string]string
		if string(req.ModelMapping) != "null" {
			if err := json.Unmarshal(req.ModelMapping, &parsed); err != nil {
				response.BadRequest(c, "Invalid model_mapping: "+err.Error())
				return
			}
		}
		if parsed == nil {
			parsed = map[string]string{}
		}
		settings.ModelMappingSet = true
		settings.ModelMapping = parsed
	}

	// 解析 group_ids 三态：未传（nil）/ null → 不修改（settings.GroupIDs 保持 nil）；
	// [] → 指向空切片（解绑全部）；[…] → 指向该切片。解析失败 → 400。
	var groupIDs *[]int64
	if req.GroupIDs != nil && string(req.GroupIDs) != "null" {
		var ids []int64
		if err := json.Unmarshal(req.GroupIDs, &ids); err != nil {
			response.BadRequest(c, "Invalid group_ids: "+err.Error())
			return
		}
		groupIDs = &ids
	}
	settings.GroupIDs = groupIDs

	st, err := h.svc.UpdateWithSettings(c.Request.Context(), id, req.Code, expiresAt, notes, req.Priority, settings)
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

// SetStatus 启用 / 停用激活码（仅 active ↔ disabled）。
// POST /api/v1/admin/quota_keys/:id/status
//
// body: {"status":"active"|"disabled"}；缺省或空 → 400 status is required。
// 调用 service.SetStatus 执行状态切换（含 R3a 过期禁止启用判定、名下账号 account_changed 入队），
// 错误走 respondQuotaKeyError 统一映射，成功回放 maskedQuotaKey(st)。
func (h *QuotaKeyHandler) SetStatus(c *gin.Context) {
	id, ok := parseQuotaKeyID(c)
	if !ok {
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if body.Status == "" {
		response.BadRequest(c, "status is required")
		return
	}
	st, err := h.svc.SetStatus(c.Request.Context(), id, body.Status)
	if err != nil {
		respondQuotaKeyError(c, err)
		return
	}
	response.Success(c, maskedQuotaKey(st))
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
	// 同一张激活码已有未软删记录（code_mac 唯一冲突）：返回 409 + 明确文案，
	// 不再让用户只看到 internal error（生产实证：重建已删/已存在的卡时无从判断原因）。
	if errors.Is(err, service.ErrQuotaKeyDuplicateCode) {
		response.ErrorFrom(c, infraerrors.Conflict("QUOTA_KEY_DUPLICATE_CODE", "该激活码已存在，请勿重复添加"))
		return
	}
	var upe *service.UpstreamPullError
	if errors.As(err, &upe) {
		response.ErrorFrom(c, infraerrors.New(http.StatusBadGateway, "QUOTA_KEY_UPSTREAM", upe.Message()))
		return
	}
	response.ErrorFrom(c, err)
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

// quotaKeyDailyStatsResponse 是每日额度统计的响应。
//
// 一次返回两张表，因为它们回答的是同一件事的两面：
//   - ByKey：每个激活码每天手里几个号、几个在干活、几个被挡、被挡后是否还能继续跑
//     （回答"400 被几个号消耗掉、这个码一天烧掉多少成本"）；
//   - ByAccount：每个号每天的用量、有没有被挡、以及被挡那刻的实测日容量
//     （回答"每个号每天到底有多少额度"）。
//
// 两张表都受 quota_key_id 过滤，口径一致，不会出现"明细筛了一个码、汇总还是全站"。
//
// 空数组而不是 null：前端不用为"没数据"多写一个分支。
type quotaKeyDailyStatsResponse struct {
	From      string                                `json:"from"`
	To        string                                `json:"to"`
	ByKey     []service.CursorQuotaKeyDailyBurn     `json:"by_key"`
	ByAccount []service.CursorAccountDailyQuotaStat `json:"by_account"`
}

// DailyStats 查询 Cursor 账号每日额度实测统计。
// GET /api/v1/admin/quota_keys/daily_stats?from=YYYY-MM-DD&to=YYYY-MM-DD&quota_key_id=N
//
// 数据来源是周期汇总表 cursor_account_daily_quota_stats（每小时汇总"昨天+今天"），
// 不在这里现算 usage_logs——后台点一下不该扫全表。
//
// 默认区间：最近 7 天（含今天）。to 允许等于今天，此时今天的数据是"截至上次汇总"的值。
func (h *QuotaKeyHandler) DailyStats(c *gin.Context) {
	now := time.Now()
	to := now
	from := now.AddDate(0, 0, -6)
	if raw := strings.TrimSpace(c.Query("to")); raw != "" {
		parsed, err := time.ParseInLocation("2006-01-02", raw, time.Local)
		if err != nil {
			response.BadRequest(c, "invalid to: want YYYY-MM-DD")
			return
		}
		to = parsed
	}
	if raw := strings.TrimSpace(c.Query("from")); raw != "" {
		parsed, err := time.ParseInLocation("2006-01-02", raw, time.Local)
		if err != nil {
			response.BadRequest(c, "invalid from: want YYYY-MM-DD")
			return
		}
		from = parsed
	}
	if from.After(to) {
		response.BadRequest(c, "from must not be after to")
		return
	}
	// 上限 92 天：再长就该走导出，不该让一个 GET 把后台拖住。
	if to.Sub(from) > 92*24*time.Hour {
		response.BadRequest(c, "range too large: max 92 days")
		return
	}

	var quotaKeyID *int64
	if raw := strings.TrimSpace(c.Query("quota_key_id")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "invalid quota_key_id")
			return
		}
		quotaKeyID = &id
	}

	byKey, err := h.svc.SummarizeCursorQuotaKeyDailyBurn(c.Request.Context(), from, to, quotaKeyID)
	if err != nil {
		respondQuotaKeyError(c, err)
		return
	}
	byAccount, err := h.svc.ListCursorAccountDailyQuotaStats(c.Request.Context(), from, to, quotaKeyID)
	if err != nil {
		respondQuotaKeyError(c, err)
		return
	}
	if byKey == nil {
		byKey = []service.CursorQuotaKeyDailyBurn{}
	}
	if byAccount == nil {
		byAccount = []service.CursorAccountDailyQuotaStat{}
	}
	response.Success(c, quotaKeyDailyStatsResponse{
		From:      from.Format("2006-01-02"),
		To:        to.Format("2006-01-02"),
		ByKey:     byKey,
		ByAccount: byAccount,
	})
}

// SyncUpstreamModels 借用该激活码名下账号做上游模型预览探测，返回 {models, warnings}，
// 不修改单账号 DB。仅预览：探测用的账号强制 ID=0，绝不回写 quota_keys / accounts。
// POST /api/v1/admin/quota_keys/:id/models/sync-upstream
func (h *QuotaKeyHandler) SyncUpstreamModels(c *gin.Context) {
	id, ok := parseQuotaKeyID(c)
	if !ok {
		return
	}
	// 依赖未装配（accountTestService / adminService）明确返回 503，不得静默走到上游探测。
	if h.accountTestService == nil || h.adminService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Quota key upstream model sync is not configured")
		return
	}
	// 选该激活码名下可用 cursor 账号（健康可调度优先），无可用账号则失败关闭（400）。
	accRow, err := h.svc.ResolvePreviewAccountForModelSync(c.Request.Context(), id)
	if err != nil {
		respondQuotaKeyError(c, err)
		return
	}
	// 加载解密凭据后的完整账号，用于上游模型清单探测。
	fullAccount, err := h.adminService.GetAccount(c.Request.Context(), accRow.ID)
	if err != nil {
		respondQuotaKeyError(c, err)
		return
	}
	// 强制 ID=0：只预览、不写单账号 DB（避免把探测账号落库为新建账号）。
	previewAcc := *fullAccount
	previewAcc.ID = 0
	catalog, err := h.accountTestService.SyncUpstreamModelCatalog(c.Request.Context(), &previewAcc)
	if err != nil {
		// 与 AccountHandler.SyncUpstreamModels（account_handler.go:3923-3938）同口径映射上游错误。
		var syncErr *service.UpstreamModelSyncError
		if errors.As(err, &syncErr) {
			switch syncErr.Kind {
			case service.UpstreamModelSyncErrorConfiguration, service.UpstreamModelSyncErrorUnsupported:
				response.BadRequest(c, syncErr.SafeMessage())
			case service.UpstreamModelSyncErrorCredential:
				response.Error(c, http.StatusUnauthorized, syncErr.SafeMessage())
			case service.UpstreamModelSyncErrorInternal:
				response.InternalError(c, syncErr.SafeMessage())
			default:
				slog.Warn("quota_key_sync_upstream_models_failed", "quota_key_id", id, "kind", syncErr.Kind)
				response.Error(c, http.StatusBadGateway, syncErr.SafeMessage())
			}
			return
		}
		slog.Warn("quota_key_sync_upstream_models_failed", "quota_key_id", id)
		response.Error(c, http.StatusBadGateway, "Failed to sync upstream models from upstream")
		return
	}
	response.Success(c, catalog)
}
