package handler

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type XianyuDeliveryHandler struct {
	service *service.XianyuDeliveryService
	token   string
}

func NewXianyuDeliveryHandler(deliveryService *service.XianyuDeliveryService, cfg *config.Config) *XianyuDeliveryHandler {
	h := &XianyuDeliveryHandler{service: deliveryService}
	if cfg != nil {
		h.token = strings.TrimSpace(cfg.XianyuDelivery.InternalToken)
	}
	return h
}

const xianyuClaimMaxBodyBytes = 16 * 1024

// Claim handles the private xianyu-auto-reply delivery endpoint.
// POST /api/v1/internal/xianyu/redeem-codes/claim
func (h *XianyuDeliveryHandler) Claim(c *gin.Context) {
	if h == nil || h.service == nil || !constantTimeTokenMatch(c.GetHeader("X-Internal-Token"), h.token) {
		response.Error(c, http.StatusUnauthorized, "invalid internal token")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, xianyuClaimMaxBodyBytes)
	var req service.XianyuDeliveryClaimRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request: "+err.Error())
		return
	}
	content, err := h.service.Claim(c.Request.Context(), req)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"content": content})
}

// DeliveryResult handles the worker → main delivery result callback.
// POST /api/v1/internal/xianyu/delivery-results
func (h *XianyuDeliveryHandler) DeliveryResult(c *gin.Context) {
	if h == nil || h.service == nil || !constantTimeTokenMatch(c.GetHeader("X-Internal-Token"), h.token) {
		response.Error(c, http.StatusUnauthorized, "invalid internal token")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, xianyuClaimMaxBodyBytes)
	var req struct {
		OrderNo      string  `json:"order_no"`
		Success      bool    `json:"success"`
		Confirmed    *bool   `json:"confirmed"`
		Error        *string `json:"error"`
		Attempt      int     `json:"attempt"`
		QuantitySent *int    `json:"quantity_sent"`
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
	confirmed := req.Confirmed != nil && *req.Confirmed
	// quantity_sent 早期校验：负数直接 400（避免 Repository 内层被绕过写入脏值）。
	// 上限校验（<= 该订单的 quantity）在 Repository 内 SQL 原子完成（需先查 quantity）。
	// 缺省 / 0 / nil 都视为 0（保留主程序默认值，不写入 quantity_sent 字段）。
	quantitySent := 0
	if req.QuantitySent != nil {
		if *req.QuantitySent < 0 {
			response.BadRequest(c, "quantity_sent must be non-negative")
			return
		}
		quantitySent = *req.QuantitySent
	}
	result := service.XianyuDeliveryStatusResult{
		OrderNo:      req.OrderNo,
		Success:      req.Success,
		Confirmed:    confirmed,
		Error:        req.Error,
		Attempt:      req.Attempt,
		QuantitySent: quantitySent,
	}
	// 优先按 order_no 路由到 Worker 发货记录（Worker 自动发货路径经 EnsureWorkerDeliveryRecord 创建）；
	// 池卡券订单同时存在 claim 行（领码时创建），worker 行更新后同步推进 claim 行，
	// 避免发货记录永久 pending、统计与告警失真。两者各自幂等，无 claim 行的订单忽略。
	if err := h.service.RecordWorkerDeliveryResult(c.Request.Context(), result); err == nil {
		if err := h.service.RecordDeliveryResult(c.Request.Context(), result); err != nil && !errors.Is(err, service.ErrXianyuDeliveryClaimNotFound) {
			response.ErrorFrom(c, err)
			return
		}
		response.Success(c, gin.H{"message": "delivery result recorded"})
		return
	} else if !errors.Is(err, service.ErrXianyuDeliveryClaimNotFound) {
		response.ErrorFrom(c, err)
		return
	}
	if err := h.service.RecordDeliveryResult(c.Request.Context(), result); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "delivery result recorded"})
}

// EnsureWorkerDeliveryRecord 处理 Worker 自动发货的订单级记录注册（幂等）。
// POST /api/v1/internal/xianyu/worker-deliveries
func (h *XianyuDeliveryHandler) EnsureWorkerDeliveryRecord(c *gin.Context) {
	if h == nil || h.service == nil || !constantTimeTokenMatch(c.GetHeader("X-Internal-Token"), h.token) {
		response.Error(c, http.StatusUnauthorized, "invalid internal token")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, xianyuClaimMaxBodyBytes)
	var req struct {
		OrderNo      string `json:"order_no"`
		DeliveryKind string `json:"delivery_kind"`
		Quantity     int    `json:"quantity"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request: "+err.Error())
		return
	}
	d := service.XianyuWorkerDelivery{
		OrderNo:      strings.TrimSpace(req.OrderNo),
		DeliveryKind: strings.TrimSpace(req.DeliveryKind),
		Quantity:     req.Quantity,
	}
	if d.OrderNo == "" {
		response.BadRequest(c, "order_no is required")
		return
	}
	if d.Quantity <= 0 {
		d.Quantity = 1
	}
	if d.DeliveryKind == "" {
		d.DeliveryKind = "auto"
	}
	if err := h.service.EnsureWorkerDeliveryRecord(c.Request.Context(), d); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "worker delivery record ensured"})
}

// RefundEvent 处理 Worker 上报的闲鱼退款成功事件（幂等）。
// POST /api/v1/internal/xianyu/refund-events
func (h *XianyuDeliveryHandler) RefundEvent(c *gin.Context) {
	if h == nil || h.service == nil || !constantTimeTokenMatch(c.GetHeader("X-Internal-Token"), h.token) {
		response.Error(c, http.StatusUnauthorized, "invalid internal token")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, xianyuClaimMaxBodyBytes)
	var req struct {
		OrderNo   string `json:"order_no"`
		AccountID string `json:"account_id"`
		Status    string `json:"status"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request: "+err.Error())
		return
	}
	action, err := h.service.ProcessRefundEvent(c.Request.Context(), req.OrderNo, req.AccountID, strings.TrimSpace(req.Status))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"action": action, "message": "refund event processed"})
}

func constantTimeTokenMatch(got, expected string) bool {
	gotHash := sha256.Sum256([]byte(got))
	expectedHash := sha256.Sum256([]byte(expected))
	return expected != "" && subtle.ConstantTimeCompare(gotHash[:], expectedHash[:]) == 1
}
