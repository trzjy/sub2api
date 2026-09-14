package admin

import (
	"strconv"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

const promoIntelMaxPageSize = 100

// infraBadRequestPromoIntel 构造参数类业务错误。
func infraBadRequestPromoIntel(msg string) error {
	return infraerrors.BadRequest("PROMO_INTEL_VALIDATION_ERROR", msg)
}

// PromoIntelHandler 优惠情报管理后台 handler。
type PromoIntelHandler struct {
	intelService *service.PromoIntelService
}

// NewPromoIntelHandler 创建 handler。
func NewPromoIntelHandler(intelService *service.PromoIntelService) *PromoIntelHandler {
	return &PromoIntelHandler{intelService: intelService}
}

// FeatureGuard 特性开关守卫：promo_intel_enabled 关闭时拒绝全部路由。
func (h *PromoIntelHandler) FeatureGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		if h == nil || h.intelService == nil ||
			!h.intelService.GetPromoIntelRuntime(c.Request.Context()).Enabled {
			response.ErrorFrom(c, service.ErrPromoIntelDisabled)
			c.Abort()
			return
		}
		c.Next()
	}
}

// --- Request / Response ---

type promoIntelSourceCreateRequest struct {
	Name                 string `json:"name" binding:"required,max=100"`
	Vendor               string `json:"vendor" binding:"required,max=50"`
	Category             string `json:"category" binding:"omitempty,max=32"`
	URL                  string `json:"url" binding:"required,max=2048"`
	FetchIntervalMinutes int    `json:"fetch_interval_minutes"`
	Enabled              *bool  `json:"enabled"`
	LLMExtract           *bool  `json:"llm_extract"`
	Notes                string `json:"notes" binding:"max=2000"`
}

type promoIntelSourceUpdateRequest struct {
	Name                 *string `json:"name" binding:"omitempty,max=100"`
	Vendor               *string `json:"vendor" binding:"omitempty,max=50"`
	Category             *string `json:"category" binding:"omitempty,max=32"`
	URL                  *string `json:"url" binding:"omitempty,max=2048"`
	FetchIntervalMinutes *int    `json:"fetch_interval_minutes"`
	Enabled              *bool   `json:"enabled"`
	LLMExtract           *bool   `json:"llm_extract"`
	Notes                *string `json:"notes" binding:"omitempty,max=2000"`
}

type promoIntelSourceResponse struct {
	ID                   int64  `json:"id"`
	Name                 string `json:"name"`
	Vendor               string `json:"vendor"`
	Category             string `json:"category"`
	URL                  string `json:"url"`
	FetchIntervalMinutes int    `json:"fetch_interval_minutes"`
	Enabled              bool   `json:"enabled"`
	LLMExtract           bool   `json:"llm_extract"`
	Notes                string `json:"notes"`
	LastFetchedAt        string `json:"last_fetched_at"`
	LastStatus           string `json:"last_status"`
	LastError            string `json:"last_error"`
	CreatedAt            string `json:"created_at"`
	UpdatedAt            string `json:"updated_at"`
}

type promoIntelItemResponse struct {
	ID            int64  `json:"id"`
	SourceID      int64  `json:"source_id"`
	SourceName    string `json:"source_name"`
	Vendor        string `json:"vendor"`
	Category      string `json:"category"`
	Title         string `json:"title"`
	Summary       string `json:"summary"`
	Details       string `json:"details"`
	DiscountInfo  string `json:"discount_info"`
	ValidUntil    string `json:"valid_until"`
	URL           string `json:"url"`
	Relevance     string `json:"relevance"`
	Status        string `json:"status"`
	ExtractStatus string `json:"extract_status"`
	DigestDate    string `json:"digest_date"`
	RawExcerpt    string `json:"raw_excerpt"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

type promoIntelItemStatusRequest struct {
	Status string `json:"status" binding:"required,oneof=pending useful ignored"`
}

type promoIntelSettingsRequest struct {
	Enabled      *bool   `json:"enabled"`
	Source       *string `json:"source" binding:"omitempty,oneof=self external"`
	Protocol     *string `json:"protocol" binding:"omitempty,oneof=openai anthropic"`
	SelfAPIKeyID *int64  `json:"self_api_key_id"`
	SelfModel    *string `json:"self_model" binding:"omitempty,max=200"`
	LLMBaseURL   *string `json:"llm_base_url" binding:"omitempty,max=500"`
	LLMAPIKey    *string `json:"llm_api_key" binding:"omitempty,max=500"`
	LLMModel     *string `json:"llm_model" binding:"omitempty,max=200"`
}

// --- Handlers ---

// ListSources GET /admin/promo-intel/sources
func (h *PromoIntelHandler) ListSources(c *gin.Context) {
	page, pageSize := response.ParsePagination(c)
	if pageSize > promoIntelMaxPageSize {
		pageSize = promoIntelMaxPageSize
	}
	params := service.PromoIntelSourceListParams{
		Page:     page,
		PageSize: pageSize,
		Vendor:   strings.TrimSpace(c.Query("vendor")),
		Enabled:  parseListEnabled(c.Query("enabled")),
		Search:   strings.TrimSpace(c.Query("search")),
	}
	items, total, err := h.intelService.ListSources(c.Request.Context(), params)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	out := make([]*promoIntelSourceResponse, 0, len(items))
	for _, src := range items {
		out = append(out, promoIntelSourceToResponse(src))
	}
	response.Paginated(c, out, total, page, pageSize)
}

// CreateSource POST /admin/promo-intel/sources
func (h *PromoIntelHandler) CreateSource(c *gin.Context) {
	var req promoIntelSourceCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	subject, _ := middleware2.GetAuthSubjectFromContext(c)
	src, err := h.intelService.CreateSource(c.Request.Context(), service.PromoIntelSourceCreateParams{
		Name:                 req.Name,
		Vendor:               req.Vendor,
		Category:             req.Category,
		URL:                  req.URL,
		FetchIntervalMinutes: req.FetchIntervalMinutes,
		Enabled:              req.Enabled,
		LLMExtract:           req.LLMExtract,
		Notes:                req.Notes,
		CreatedBy:            subject.UserID,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Created(c, promoIntelSourceToResponse(src))
}

// UpdateSource PUT /admin/promo-intel/sources/:id
func (h *PromoIntelHandler) UpdateSource(c *gin.Context) {
	id, ok := parsePromoIntelID(c)
	if !ok {
		return
	}
	var req promoIntelSourceUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	src, err := h.intelService.UpdateSource(c.Request.Context(), id, service.PromoIntelSourceUpdateParams{
		Name:                 req.Name,
		Vendor:               req.Vendor,
		Category:             req.Category,
		URL:                  req.URL,
		FetchIntervalMinutes: req.FetchIntervalMinutes,
		Enabled:              req.Enabled,
		LLMExtract:           req.LLMExtract,
		Notes:                req.Notes,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, promoIntelSourceToResponse(src))
}

// DeleteSource DELETE /admin/promo-intel/sources/:id
func (h *PromoIntelHandler) DeleteSource(c *gin.Context) {
	id, ok := parsePromoIntelID(c)
	if !ok {
		return
	}
	if err := h.intelService.DeleteSource(c.Request.Context(), id); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, nil)
}

// FetchSourceNow POST /admin/promo-intel/sources/:id/fetch
func (h *PromoIntelHandler) FetchSourceNow(c *gin.Context) {
	id, ok := parsePromoIntelID(c)
	if !ok {
		return
	}
	result, err := h.intelService.FetchSourceNow(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	var src *promoIntelSourceResponse
	if result != nil && result.Source != nil {
		// ProcessSource 内会回写抓取状态；重新读取以返回最新状态。
		if fresh, err := h.intelService.GetSource(c.Request.Context(), id); err == nil {
			src = promoIntelSourceToResponse(fresh)
		} else {
			src = promoIntelSourceToResponse(result.Source)
		}
	}
	response.Success(c, gin.H{
		"source":          src,
		"content_changed": result.ContentChanged,
		"items_created":   result.ItemsCreated,
		"items_updated":   result.ItemsUpdated,
		"skipped_reason":  result.SkippedReason,
	})
}

// ListItems GET /admin/promo-intel/items
func (h *PromoIntelHandler) ListItems(c *gin.Context) {
	page, pageSize := response.ParsePagination(c)
	if pageSize > promoIntelMaxPageSize {
		pageSize = promoIntelMaxPageSize
	}
	params := service.PromoIntelItemListParams{
		Page:       page,
		PageSize:   pageSize,
		Vendor:     strings.TrimSpace(c.Query("vendor")),
		Category:   strings.TrimSpace(c.Query("category")),
		Relevance:  strings.TrimSpace(c.Query("relevance")),
		Status:     strings.TrimSpace(c.Query("status")),
		DigestDate: strings.TrimSpace(c.Query("digest_date")),
		Search:     strings.TrimSpace(c.Query("search")),
	}
	items, total, err := h.intelService.ListItems(c.Request.Context(), params)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	out := make([]*promoIntelItemResponse, 0, len(items))
	for _, item := range items {
		out = append(out, promoIntelItemToResponse(item))
	}
	response.Paginated(c, out, total, page, pageSize)
}

// UpdateItemStatus PUT /admin/promo-intel/items/:id/status
func (h *PromoIntelHandler) UpdateItemStatus(c *gin.Context) {
	id, ok := parsePromoIntelID(c)
	if !ok {
		return
	}
	var req promoIntelItemStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	item, err := h.intelService.UpdateItemStatus(c.Request.Context(), id, req.Status)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, promoIntelItemToResponse(item))
}

// GetBriefing GET /admin/promo-intel/briefing?date=YYYY-MM-DD
func (h *PromoIntelHandler) GetBriefing(c *gin.Context) {
	briefing, err := h.intelService.GetBriefing(c.Request.Context(), strings.TrimSpace(c.Query("date")), c.Query("refresh") == "1")
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	items := make([]*promoIntelItemResponse, 0, len(briefing.Items))
	for _, item := range briefing.Items {
		items = append(items, promoIntelItemToResponse(item))
	}
	response.Success(c, gin.H{
		"date":        briefing.Date,
		"total":       briefing.Total,
		"high_count":  briefing.HighCount,
		"pending":     briefing.Pending,
		"by_vendor":   briefing.ByVendor,
		"by_category": briefing.ByCategory,
		"items":       items,
	})
}

// ListAPIKeys GET /admin/promo-intel/api-keys
// 自选模式（self）可用管理员 API Key 列表（仅 ID + 名称，不回明文）。
func (h *PromoIntelHandler) ListAPIKeys(c *gin.Context) {
	keys, err := h.intelService.ListAPIKeyRefs(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"items": keys})
}

// GetSettings GET /admin/promo-intel/settings
func (h *PromoIntelHandler) GetSettings(c *gin.Context) {
	response.Success(c, promoIntelRuntimeToResponse(h.intelService.GetPromoIntelRuntime(c.Request.Context())))
}

// UpdateSettings PUT /admin/promo-intel/settings
func (h *PromoIntelHandler) UpdateSettings(c *gin.Context) {
	var req promoIntelSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	rt, err := h.intelService.UpdateLLMSettings(c.Request.Context(), service.PromoIntelSettingsUpdate{
		Enabled:      req.Enabled,
		Source:       req.Source,
		Protocol:     req.Protocol,
		SelfAPIKeyID: req.SelfAPIKeyID,
		SelfModel:    req.SelfModel,
		LLMBaseURL:   req.LLMBaseURL,
		LLMAPIKey:    req.LLMAPIKey,
		LLMModel:     req.LLMModel,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, promoIntelRuntimeToResponse(rt))
}

// TestSettings POST /admin/promo-intel/settings/test
func (h *PromoIntelHandler) TestSettings(c *gin.Context) {
	message, err := h.intelService.TestLLMSettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": message})
}

// --- helpers ---

func parsePromoIntelID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.ErrorFrom(c, infraBadRequestPromoIntel("invalid promo intel id"))
		return 0, false
	}
	return id, true
}

func promoIntelSourceToResponse(src *service.PromoIntelSource) *promoIntelSourceResponse {
	if src == nil {
		return nil
	}
	lastFetched := ""
	if src.LastFetchedAt != nil {
		lastFetched = src.LastFetchedAt.UTC().Format(time.RFC3339)
	}
	return &promoIntelSourceResponse{
		ID:                   src.ID,
		Name:                 src.Name,
		Vendor:               src.Vendor,
		Category:             src.Category,
		URL:                  src.URL,
		FetchIntervalMinutes: src.FetchIntervalMinutes,
		Enabled:              src.Enabled,
		LLMExtract:           src.LLMExtract,
		Notes:                src.Notes,
		LastFetchedAt:        lastFetched,
		LastStatus:           src.LastStatus,
		LastError:            src.LastError,
		CreatedAt:            src.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:            src.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func promoIntelItemToResponse(item *service.PromoIntelItem) *promoIntelItemResponse {
	if item == nil {
		return nil
	}
	digestDate := ""
	if item.DigestDate != nil {
		digestDate = item.DigestDate.Format("2006-01-02")
	}
	return &promoIntelItemResponse{
		ID:            item.ID,
		SourceID:      item.SourceID,
		SourceName:    item.SourceName,
		Vendor:        item.Vendor,
		Category:      item.Category,
		Title:         item.Title,
		Summary:       item.Summary,
		Details:       item.Details,
		DiscountInfo:  item.DiscountInfo,
		ValidUntil:    item.ValidUntil,
		URL:           item.URL,
		Relevance:     item.Relevance,
		Status:        item.Status,
		ExtractStatus: item.ExtractStatus,
		DigestDate:    digestDate,
		RawExcerpt:    item.RawExcerpt,
		CreatedAt:     item.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:     item.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func promoIntelRuntimeToResponse(rt service.PromoIntelRuntime) gin.H {
	return gin.H{
		"enabled":           rt.Enabled,
		"source":            rt.Source,
		"protocol":          rt.Protocol,
		"self_api_key_id":   rt.SelfAPIKeyID,
		"self_api_key_name": rt.SelfAPIKeyName,
		"self_model":        rt.SelfModel,
		"llm_configured":    rt.HasLLM(),
		"llm_base_url":      rt.LLMBaseURL,
		"llm_api_key_set":   rt.LLMAPIKeySet,
		"llm_api_key_mask":  rt.LLMAPIKeyMasked,
		"llm_model":         rt.LLMModel,
	}
}
