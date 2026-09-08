package admin

import (
	"context"
	"strconv"
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

// GetDeliveryTemplate 读取 Worker 全局发货模板（买家收到的发货消息格式，对所有卡券统一生效）。
func (h *XianyuAdminHandler) GetDeliveryTemplate(c *gin.Context) {
	template, err := h.control.GetDeliveryTemplate(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"template": template})
}

type updateDeliveryTemplateRequest struct {
	Template string `json:"template"`
}

// UpdateDeliveryTemplate 更新 Worker 全局发货模板。
func (h *XianyuAdminHandler) UpdateDeliveryTemplate(c *gin.Context) {
	var req updateDeliveryTemplateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request: "+err.Error())
		return
	}
	if len(req.Template) > 2000 {
		response.BadRequest(c, "template too long (max 2000)")
		return
	}
	if err := h.control.UpdateDeliveryTemplate(c.Request.Context(), strings.TrimSpace(req.Template)); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "delivery template updated"})
}

// DeleteItemPool 删除库存池（存在绑定商品/剩余库存码/引用规则时拒绝）。
func (h *XianyuAdminHandler) DeleteItemPool(c *gin.Context) {
	poolID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || poolID <= 0 {
		response.BadRequest(c, "invalid pool id")
		return
	}
	if err := h.control.DeleteItemPool(c.Request.Context(), poolID); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "item pool deleted"})
}

// DeleteBindingRule 删除绑定规则。
func (h *XianyuAdminHandler) DeleteBindingRule(c *gin.Context) {
	ruleID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || ruleID <= 0 {
		response.BadRequest(c, "invalid rule id")
		return
	}
	if err := h.control.DeleteBindingRule(c.Request.Context(), ruleID); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "binding rule deleted"})
}

// UpdateAccountRemark 更新账号运营备注（主程序侧，区分多账号用途）。
func (h *XianyuAdminHandler) UpdateAccountRemark(c *gin.Context) {
	accountPK, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountPK <= 0 {
		response.BadRequest(c, "invalid account id")
		return
	}
	var req struct {
		Remark string `json:"remark"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request: "+err.Error())
		return
	}
	remark := strings.TrimSpace(req.Remark)
	if len(remark) > 200 {
		response.BadRequest(c, "remark too long (max 200)")
		return
	}
	if err := h.control.UpdateAccountRemark(c.Request.Context(), accountPK, remark); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "account remark updated", "remark": remark})
}

// MarkDeliverySent 管理员确认待处理发货记录的卡密已线下送达，标记为已发送。
func (h *XianyuAdminHandler) MarkDeliverySent(c *gin.Context) {
	var req struct {
		OrderNo string `json:"order_no"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request: "+err.Error())
		return
	}
	req.OrderNo = strings.TrimSpace(req.OrderNo)
	if req.OrderNo == "" {
		response.BadRequest(c, "order_no is required")
		return
	}
	if err := h.control.MarkDeliveryClaimSent(c.Request.Context(), req.OrderNo); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "delivery marked as sent"})
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
	CodeType          string `json:"code_type"`
	GroupID           *int64 `json:"group_id"`
	ValidityDays      int    `json:"validity_days"`
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
		CodeType:          req.CodeType,
		GroupID:           req.GroupID,
		ValidityDays:      req.ValidityDays,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, saved)
}

type stockItemPoolRequest struct {
	Count         int `json:"count"`
	ExpiresInDays int `json:"expires_in_days"`
}

// StockItemPool 库存池补货：按池的发码规格生成真实可兑换的订阅码。
func (h *XianyuAdminHandler) StockItemPool(c *gin.Context) {
	poolID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || poolID <= 0 {
		response.BadRequest(c, "invalid pool id")
		return
	}
	var req stockItemPoolRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request: "+err.Error())
		return
	}
	executeAdminIdempotentJSON(c, "admin.xianyu.pool_stock", req, service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
		created, remaining, err := h.control.StockItemPool(ctx, poolID, service.XianyuPoolStockInput{
			Count:         req.Count,
			ExpiresInDays: req.ExpiresInDays,
		})
		if err != nil {
			return nil, err
		}
		return gin.H{"created": created, "remaining": remaining}, nil
	})
}

type createSpecBindingRequest struct {
	ProductID int64  `json:"product_id"`
	SpecName  string `json:"spec_name"`
	SpecValue string `json:"spec_value"`
	PoolID    int64  `json:"pool_id"`
}

// CreateProductSpecBinding 同一商品多档次：手动添加规格绑定（规格文案需与闲鱼商品规格一致）。
func (h *XianyuAdminHandler) CreateProductSpecBinding(c *gin.Context) {
	var req createSpecBindingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request: "+err.Error())
		return
	}
	if req.ProductID <= 0 || req.PoolID <= 0 {
		response.BadRequest(c, "product_id and pool_id are required")
		return
	}
	if err := h.control.CreateProductSpecBinding(c.Request.Context(), req.ProductID, req.SpecName, req.SpecValue, req.PoolID); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "spec binding created"})
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
