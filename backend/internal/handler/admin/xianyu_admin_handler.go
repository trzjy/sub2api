package admin

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// XianyuAdminHandler 处理闲鱼发货控制面管理请求。
type XianyuAdminHandler struct {
	control   *service.XianyuControlService
	adminOnly func(c *gin.Context) bool
}

// NewXianyuAdminHandler 创建控制面管理 handler。
func NewXianyuAdminHandler(control *service.XianyuControlService) *XianyuAdminHandler {
	return &XianyuAdminHandler{
		control: control,
		adminOnly: func(c *gin.Context) bool {
			role, _ := middleware.GetUserRoleFromContext(c)
			return role == service.RoleAdmin
		},
	}
}

// Overview 概览页数据。
func (h *XianyuAdminHandler) Overview(c *gin.Context) {
	data, err := h.control.GetOverview(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, data)
}

// CanManage 判断当前管理员是否可管理闲鱼发货；管理员角色默认授权。
func (h *XianyuAdminHandler) CanManage(c *gin.Context) bool {
	if h == nil {
		return false
	}
	if h.adminOnly != nil && !h.adminOnly(c) {
		return false
	}
	return true
}

// Access 返回当前管理员是否有闲鱼发货管理权限（前端导航门控用）。
func (h *XianyuAdminHandler) Access(c *gin.Context) {
	response.Success(c, gin.H{"can_manage": h.CanManage(c)})
}

// GetSettings 读取控制面设置。
func (h *XianyuAdminHandler) GetSettings(c *gin.Context) {
	settings, err := h.control.GetSettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}

// SaveSettings 保存控制面设置。
func (h *XianyuAdminHandler) SaveSettings(c *gin.Context) {
	var req service.XianyuSettings
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request: "+err.Error())
		return
	}
	if err := h.control.SaveSettings(c.Request.Context(), req); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "settings saved"})
}

// WorkerConfigs 列出 Worker 配置。
func (h *XianyuAdminHandler) WorkerConfigs(c *gin.Context) {
	cfgs, err := h.control.ListWorkerConfigs(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, cfgs)
}

type saveWorkerConfigRequest struct {
	ID       int64  `json:"id"`
	BaseURL  string `json:"base_url"`
	APIToken string `json:"api_token"`
	Status   string `json:"status"`
}

func (h *XianyuAdminHandler) SaveWorkerConfig(c *gin.Context) {
	var req saveWorkerConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request: "+err.Error())
		return
	}
	cfg := service.XianyuWorkerConfig{
		ID:                req.ID,
		BaseURL:           req.BaseURL,
		APITokenEncrypted: req.APIToken,
		Status:            req.Status,
	}
	saved, err := h.control.SaveWorkerConfig(c.Request.Context(), cfg)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	saved.APITokenEncrypted = ""
	response.Success(c, saved)
}

// Accounts 账号列表。
func (h *XianyuAdminHandler) Accounts(c *gin.Context) {
	accounts, err := h.control.ListAccounts(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, accounts)
}

type accountActionRequest struct {
	AccountID string `json:"account_id"`
}

func (h *XianyuAdminHandler) EnableAccount(c *gin.Context) {
	var req accountActionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request: "+err.Error())
		return
	}
	if err := h.control.EnableAccount(c.Request.Context(), req.AccountID); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "account enabled"})
}

func (h *XianyuAdminHandler) DisableAccount(c *gin.Context) {
	var req accountActionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request: "+err.Error())
		return
	}
	if err := h.control.DisableAccount(c.Request.Context(), req.AccountID); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "account disabled"})
}

func (h *XianyuAdminHandler) RefreshCookie(c *gin.Context) {
	var req accountActionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request: "+err.Error())
		return
	}
	saved, err := h.control.RefreshCookie(c.Request.Context(), req.AccountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, saved)
}

// ClearCredentials 退出/清除凭证：停止任务并删除 Worker 侧凭证。
func (h *XianyuAdminHandler) ClearCredentials(c *gin.Context) {
	var req accountActionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request: "+err.Error())
		return
	}
	if err := h.control.ClearCredentials(c.Request.Context(), req.AccountID); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "account credentials cleared"})
}

// CreateLoginSession 创建扫码会话。
func (h *XianyuAdminHandler) CreateLoginSession(c *gin.Context) {
	var req accountActionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request: "+err.Error())
		return
	}
	session, err := h.control.CreateLoginSession(c.Request.Context(), req.AccountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, session)
}

// QueryLoginSession 查询扫码会话状态。
func (h *XianyuAdminHandler) QueryLoginSession(c *gin.Context) {
	sessionID := c.Param("session_id")
	session, err := h.control.QueryLoginSession(c.Request.Context(), sessionID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, session)
}

// CheckHealth 立即健康检查。
func (h *XianyuAdminHandler) CheckHealth(c *gin.Context) {
	if err := h.control.CheckHealth(c.Request.Context()); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "health checked"})
}

// SyncAccounts 立即同步账号。
func (h *XianyuAdminHandler) SyncAccounts(c *gin.Context) {
	if err := h.control.SyncAccounts(c.Request.Context()); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "accounts synced"})
}

// SyncProducts 立即刷新商品。
func (h *XianyuAdminHandler) SyncProducts(c *gin.Context) {
	if err := h.control.SyncProducts(c.Request.Context()); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "products synced"})
}

// Products 商品列表。
func (h *XianyuAdminHandler) Products(c *gin.Context) {
	products, err := h.control.ListProducts(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, products)
}

type bindProductRequest struct {
	ProductID int64  `json:"product_id"`
	PoolID    *int64 `json:"pool_id"`
}

func (h *XianyuAdminHandler) BindProduct(c *gin.Context) {
	var req bindProductRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request: "+err.Error())
		return
	}
	if req.ProductID <= 0 {
		response.BadRequest(c, "product_id is required")
		return
	}
	source := service.XianyuBindingSourceManual
	if req.PoolID == nil {
		// 解绑
	}
	if err := h.control.BindProduct(c.Request.Context(), req.ProductID, req.PoolID, source); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "product bound"})
}

// BindingRules 绑定规则列表。
func (h *XianyuAdminHandler) BindingRules(c *gin.Context) {
	rules, err := h.control.ListBindingRules(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, rules)
}

type saveBindingRuleRequest struct {
	ID        int64  `json:"id"`
	Priority  int    `json:"priority"`
	AccountPK int64  `json:"account_pk"`
	MatchType string `json:"match_type"`
	Keyword   string `json:"keyword"`
	PoolID    int64  `json:"pool_id"`
	Status    string `json:"status"`
}

func (h *XianyuAdminHandler) SaveBindingRule(c *gin.Context) {
	var req saveBindingRuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request: "+err.Error())
		return
	}
	saved, err := h.control.SaveBindingRule(c.Request.Context(), service.XianyuBindingRule{
		ID:        req.ID,
		Priority:  req.Priority,
		AccountPK: req.AccountPK,
		MatchType: req.MatchType,
		Keyword:   req.Keyword,
		PoolID:    req.PoolID,
		Status:    req.Status,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, saved)
}

// ItemPools 商品池列表。
func (h *XianyuAdminHandler) ItemPools(c *gin.Context) {
	pools, err := h.control.ListItemPools(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, pools)
}

type saveItemPoolRequest struct {
	ID                int64  `json:"id"`
	Name              string `json:"name"`
	Slug              string `json:"slug"`
	Description       string `json:"description"`
	LowStockThreshold int    `json:"low_stock_threshold"`
	Status            string `json:"status"`
}

func (h *XianyuAdminHandler) SaveItemPool(c *gin.Context) {
	var req saveItemPoolRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request: "+err.Error())
		return
	}
	saved, err := h.control.SaveItemPool(c.Request.Context(), service.XianyuItemPool{
		ID:                req.ID,
		Name:              req.Name,
		Slug:              req.Slug,
		Description:       req.Description,
		LowStockThreshold: req.LowStockThreshold,
		Status:            req.Status,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, saved)
}

// Deliveries 发货记录列表。
func (h *XianyuAdminHandler) Deliveries(c *gin.Context) {
	page, pageSize := response.ParsePagination(c)
	status := strings.TrimSpace(c.Query("status"))
	search := strings.TrimSpace(c.Query("search"))
	if len(search) > 100 {
		search = search[:100]
	}
	filter := service.XianyuDeliveryFilter{
		Status: status,
		Search: search,
		Offset: (page - 1) * pageSize,
		Limit:  pageSize,
	}
	claims, total, err := h.control.ListDeliveryClaims(c.Request.Context(), filter)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Paginated(c, claims, int64(total), page, pageSize)
}

// WorkerDeliveries Worker 自动发货记录列表（订单级汇总）。
func (h *XianyuAdminHandler) WorkerDeliveries(c *gin.Context) {
	page, pageSize := response.ParsePagination(c)
	status := strings.TrimSpace(c.Query("status"))
	search := strings.TrimSpace(c.Query("search"))
	if len(search) > 100 {
		search = search[:100]
	}
	filter := service.XianyuDeliveryFilter{
		Status: status,
		Search: search,
		Offset: (page - 1) * pageSize,
		Limit:  pageSize,
	}
	items, total, err := h.control.ListWorkerDeliveries(c.Request.Context(), filter)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Paginated(c, items, int64(total), page, pageSize)
}

type resendRequest struct {
	OrderNo string `json:"order_no"`
}

// ResendDelivery 人工补发原码。
func (h *XianyuAdminHandler) ResendDelivery(c *gin.Context) {
	var req resendRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request: "+err.Error())
		return
	}
	req.OrderNo = strings.TrimSpace(req.OrderNo)
	if req.OrderNo == "" {
		response.BadRequest(c, "order_no is required")
		return
	}
	code, err := h.control.ResendOriginalCode(c.Request.Context(), req.OrderNo)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"code": code})
}
