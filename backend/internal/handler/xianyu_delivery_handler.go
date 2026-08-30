package handler

import (
	"crypto/sha256"
	"crypto/subtle"
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
	if err := h.service.RecordDeliveryResult(c.Request.Context(), result); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "delivery result recorded"})
}

func constantTimeTokenMatch(got, expected string) bool {
	gotHash := sha256.Sum256([]byte(got))
	expectedHash := sha256.Sum256([]byte(expected))
	return expected != "" && subtle.ConstantTimeCompare(gotHash[:], expectedHash[:]) == 1
}
