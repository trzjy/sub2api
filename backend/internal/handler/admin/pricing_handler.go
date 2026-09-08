package admin

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// PricingHandler 价格管理中心：同步状态/手动同步/价格目录/未覆盖扫描/试算/自定义价格 CRUD。
// 写操作经 admin 组审计中间件自动留痕。
type PricingHandler struct {
	adminService *service.PricingAdminService
	custom       *service.CustomModelPricingService
}

func NewPricingHandler(adminService *service.PricingAdminService, custom *service.CustomModelPricingService) *PricingHandler {
	return &PricingHandler{adminService: adminService, custom: custom}
}

// GetStatus 同步状态总览
// GET /api/v1/admin/pricing/status
func (h *PricingHandler) GetStatus(c *gin.Context) {
	response.Success(c, h.adminService.GetStatus())
}

// SyncNow 立即触发远程价格表同步
// POST /api/v1/admin/pricing/sync
func (h *PricingHandler) SyncNow(c *gin.Context) {
	if err := h.adminService.SyncNow(c.Request.Context()); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, h.adminService.GetStatus())
}

// GetCatalog 全局价格目录（搜索/来源筛选/分页）
// GET /api/v1/admin/pricing/catalog?search=&source=&page=&page_size=
func (h *PricingHandler) GetCatalog(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "50"))
	response.Success(c, h.adminService.GetCatalog(c.Query("search"), c.Query("source"), page, pageSize))
}

// GetUncovered 未覆盖模型双通道扫描
// GET /api/v1/admin/pricing/uncovered?days=30
func (h *PricingHandler) GetUncovered(c *gin.Context) {
	days, _ := strconv.Atoi(c.DefaultQuery("days", "30"))
	result, err := h.adminService.ScanUncovered(c.Request.Context(), days)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

// GetPreview 生效价试算
// GET /api/v1/admin/pricing/preview?model=x&group_id=1
func (h *PricingHandler) GetPreview(c *gin.Context) {
	groupID, _ := strconv.ParseInt(c.DefaultQuery("group_id", "0"), 10, 64)
	result, err := h.adminService.GetPreview(c.Request.Context(), c.Query("model"), groupID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

// --- 自定义价格 CRUD ---

// CreateCustomModelPricingRequest 创建自定义定价请求。
// 价格字段单位：每 token USD；未配置的字段沿用更低价层（合并语义）。
type CreateCustomModelPricingRequest struct {
	Models           []string                  `json:"models" binding:"required"`
	BillingMode      service.BillingMode       `json:"billing_mode"`
	InputPrice       *float64                  `json:"input_price"`
	OutputPrice      *float64                  `json:"output_price"`
	CacheWritePrice  *float64                  `json:"cache_write_price"`
	CacheReadPrice   *float64                  `json:"cache_read_price"`
	FastMultiplier   *float64                  `json:"fast_multiplier"`
	FlexMultiplier   *float64                  `json:"flex_multiplier"`
	ImageInputPrice  *float64                  `json:"image_input_price"`
	ImageOutputPrice *float64                  `json:"image_output_price"`
	PerRequestPrice  *float64                  `json:"per_request_price"`
	Intervals        []service.PricingInterval `json:"intervals"`
	Enabled          *bool                     `json:"enabled"`
	Remark           string                    `json:"remark"`
}

// UpdateCustomModelPricingRequest 更新自定义定价请求（整体替换语义）。
type UpdateCustomModelPricingRequest struct {
	Models           []string                  `json:"models" binding:"required"`
	BillingMode      service.BillingMode       `json:"billing_mode"`
	InputPrice       *float64                  `json:"input_price"`
	OutputPrice      *float64                  `json:"output_price"`
	CacheWritePrice  *float64                  `json:"cache_write_price"`
	CacheReadPrice   *float64                  `json:"cache_read_price"`
	FastMultiplier   *float64                  `json:"fast_multiplier"`
	FlexMultiplier   *float64                  `json:"flex_multiplier"`
	ImageInputPrice  *float64                  `json:"image_input_price"`
	ImageOutputPrice *float64                  `json:"image_output_price"`
	PerRequestPrice  *float64                  `json:"per_request_price"`
	Intervals        []service.PricingInterval `json:"intervals"`
	Enabled          *bool                     `json:"enabled"`
	Remark           string                    `json:"remark"`
}

func (h *PricingHandler) ListCustom(c *gin.Context) {
	entries, err := h.custom.List(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, entries)
}

func (h *PricingHandler) GetCustom(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "invalid id")
		return
	}
	entry, err := h.custom.GetByID(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, entry)
}

func (h *PricingHandler) CreateCustom(c *gin.Context) {
	var req CreateCustomModelPricingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	entry := customEntryFromRequest(req.Models, req.BillingMode, req.InputPrice, req.OutputPrice,
		req.CacheWritePrice, req.CacheReadPrice, req.FastMultiplier, req.FlexMultiplier,
		req.ImageInputPrice, req.ImageOutputPrice, req.PerRequestPrice, req.Intervals, req.Enabled, req.Remark)
	if subject, ok := middleware.GetAuthSubjectFromContext(c); ok {
		entry.CreatedBy = subject.UserID
	}
	if err := h.custom.Create(c.Request.Context(), entry); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Created(c, entry)
}

func (h *PricingHandler) UpdateCustom(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "invalid id")
		return
	}
	var req UpdateCustomModelPricingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	entry := customEntryFromRequest(req.Models, req.BillingMode, req.InputPrice, req.OutputPrice,
		req.CacheWritePrice, req.CacheReadPrice, req.FastMultiplier, req.FlexMultiplier,
		req.ImageInputPrice, req.ImageOutputPrice, req.PerRequestPrice, req.Intervals, req.Enabled, req.Remark)
	entry.ID = id
	if err := h.custom.Update(c.Request.Context(), entry); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, entry)
}

func (h *PricingHandler) DeleteCustom(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "invalid id")
		return
	}
	if err := h.custom.Delete(c.Request.Context(), id); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "deleted"})
}

// customEntryFromRequest 组装条目（Create/Update 共用字段映射）。
func customEntryFromRequest(
	models []string, billingMode service.BillingMode,
	input, output, cacheWrite, cacheRead, fast, flex, imageInput, imageOutput, perRequest *float64,
	intervals []service.PricingInterval, enabled *bool, remark string,
) *service.CustomModelPricing {
	entry := &service.CustomModelPricing{
		Models:           models,
		BillingMode:      billingMode,
		InputPrice:       input,
		OutputPrice:      output,
		CacheWritePrice:  cacheWrite,
		CacheReadPrice:   cacheRead,
		FastMultiplier:   fast,
		FlexMultiplier:   flex,
		ImageInputPrice:  imageInput,
		ImageOutputPrice: imageOutput,
		PerRequestPrice:  perRequest,
		Intervals:        intervals,
		Enabled:          true,
		Remark:           remark,
	}
	if enabled != nil {
		entry.Enabled = *enabled
	}
	return entry
}
