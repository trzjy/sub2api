package admin

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// VisionCapabilityHandler 把 D 单落地的视觉能力检测 / 覆盖服务接到 admin API。
//
// 三端点（API 契约见 docs/capability-routing-plan.md §3.5/§3.8，主会话已裁定字段名）：
//   - POST /api/admin/accounts/:id/vision-capability/detect   单账号检测
//   - POST /api/admin/accounts/vision-capability/detect        批量检测
//   - PUT  /api/admin/accounts/:id/vision-capability           人工覆盖（source=manual）
//
// 落库与缓存失效广播复用 AccountModelCapabilityService 既有链路；
// detect_failed / manual_review 不落库（判定逻辑在 service 实现，本 handler 只透传）。
type VisionCapabilityHandler struct {
	detect    visionDetector
	capability *service.AccountModelCapabilityService
}

// visionDetector 是 VisionDetectService 的方法子集，便于测试注入。
// 生产接线传入具体的 *service.VisionDetectService。
type visionDetector interface {
	DetectCapability(ctx context.Context, accountID int64, upstreamModel, protocol string) (*service.VisionDetectResult, error)
	DetectCapabilities(ctx context.Context, accountID int64, models []string, protocol string) ([]*service.VisionDetectResult, error)
}

// NewVisionCapabilityHandler 创建视觉能力 admin handler。
func NewVisionCapabilityHandler(
	detect *service.VisionDetectService,
	capability *service.AccountModelCapabilityService,
) *VisionCapabilityHandler {
	return &VisionCapabilityHandler{
		detect:     detect,
		capability: capability,
	}
}

// visionDetectRequest 单账号检测请求体。
type visionDetectRequest struct {
	Model    string `json:"model" binding:"required"`
	Protocol string `json:"protocol"`
}

// visionDetectResponse 单账号检测响应。
type visionDetectResponse struct {
	Status         string `json:"status"`
	SupportsVision *bool  `json:"supports_vision"`
}

// visionBatchRequest 批量检测请求体。
type visionBatchRequest struct {
	AccountIDs []int64 `json:"account_ids" binding:"required"`
	Items      []struct {
		Model    string `json:"model"`
		Protocol string `json:"protocol"`
	} `json:"items" binding:"required"`
}

// visionBatchItem 批量检测单条结果。
type visionBatchItem struct {
	AccountID      int64  `json:"account_id"`
	Model          string `json:"model"`
	Protocol       string `json:"protocol"`
	Status         string `json:"status"`
	SupportsVision *bool  `json:"supports_vision"`
}

// visionBatchResponse 批量检测响应。
type visionBatchResponse struct {
	Results []visionBatchItem `json:"results"`
}

// visionOverrideRequest 人工覆盖请求体（落库 source=manual，优先级最高）。
type visionOverrideRequest struct {
	Model          string `json:"model" binding:"required"`
	Protocol       string `json:"protocol"`
	SupportsVision bool   `json:"supports_vision"`
}

// Detect 处理单账号视觉能力检测：POST /api/admin/accounts/:id/vision-capability/detect
func (h *VisionCapabilityHandler) Detect(c *gin.Context) {
	accountID, ok := parsePositiveIDParam(c, "id")
	if !ok {
		return
	}
	var req visionDetectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	result, err := h.detect.DetectCapability(c.Request.Context(), accountID, req.Model, req.Protocol)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, visionDetectResponse{
		Status:         result.Result,
		SupportsVision: visionSupportsVision(result),
	})
}

// DetectBatch 处理批量视觉能力检测：POST /api/admin/accounts/vision-capability/detect
func (h *VisionCapabilityHandler) DetectBatch(c *gin.Context) {
	var req visionBatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	results := make([]visionBatchItem, 0, len(req.AccountIDs)*len(req.Items))
	for _, accountID := range req.AccountIDs {
		for _, item := range req.Items {
			protocol := item.Protocol
			result, err := h.detect.DetectCapability(c.Request.Context(), accountID, item.Model, protocol)
			if err != nil {
				// 单账号失败视为该条的传输层失败（不中断其余账号）。
				results = append(results, visionBatchItem{
					AccountID: accountID,
					Model:     item.Model,
					Protocol:  protocol,
					Status:    service.VisionDetectFailed,
				})
				continue
			}
			results = append(results, visionBatchItem{
				AccountID:      accountID,
				Model:          item.Model,
				Protocol:       result.Protocol,
				Status:         result.Result,
				SupportsVision: visionSupportsVision(result),
			})
		}
	}

	response.Success(c, visionBatchResponse{Results: results})
}

// Override 处理人工覆盖：PUT /api/admin/accounts/:id/vision-capability
// 落库 source=manual，优先级最高。
func (h *VisionCapabilityHandler) Override(c *gin.Context) {
	accountID, ok := parsePositiveIDParam(c, "id")
	if !ok {
		return
	}
	var req visionOverrideRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	protocol := req.Protocol
	if protocol == "" {
		protocol = model.CapabilityProtocolChatCompletions
	}

	saved, err := h.capability.UpsertCapability(c.Request.Context(), &model.AccountModelCapability{
		AccountID:      accountID,
		UpstreamModel:  req.Model,
		Protocol:       protocol,
		SupportsVision: req.SupportsVision,
		Source:         model.CapabilitySourceManual,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, saved)
}

// visionSupportsVision 由检测四态推导 supports_vision：
// supported→true，unsupported→false，detect_failed/manual_review→null（未知）。
func visionSupportsVision(result *service.VisionDetectResult) *bool {
	switch result.Result {
	case service.VisionDetectSupported:
		v := true
		return &v
	case service.VisionDetectUnsupported:
		v := false
		return &v
	default:
		return nil
	}
}
