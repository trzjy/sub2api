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
		OrderNo   string  `json:"order_no"`
		Success   bool    `json:"success"`
		Confirmed *bool   `json:"confirmed"`
		Error     *string `json:"error"`
		Attempt   int     `json:"attempt"`
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
	result := service.XianyuDeliveryStatusResult{
		OrderNo:   req.OrderNo,
		Success:   req.Success,
		Confirmed: confirmed,
		Error:     req.Error,
		Attempt:   req.Attempt,
	}
	// 优先按 order_no 路由到 Worker 发货记录（Worker 自动发货路径经 EnsureWorkerDeliveryRecord 创建）；
	// 该订单无 Worker 记录时再更新主程序库存发货记录（xianyu_order_claims）。两者互斥，幂等关联同一 order_no。
	if err := h.service.RecordWorkerDeliveryResult(c.Request.Context(), result); err == nil {
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

func constantTimeTokenMatch(got, expected string) bool {
	gotHash := sha256.Sum256([]byte(got))
	expectedHash := sha256.Sum256([]byte(expected))
	return expected != "" && subtle.ConstantTimeCompare(gotHash[:], expectedHash[:]) == 1
}
