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

func constantTimeTokenMatch(got, expected string) bool {
	gotHash := sha256.Sum256([]byte(got))
	expectedHash := sha256.Sum256([]byte(expected))
	return expected != "" && subtle.ConstantTimeCompare(gotHash[:], expectedHash[:]) == 1
}
