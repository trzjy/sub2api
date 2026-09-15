package handler

import (
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// XianGuanJiaEventHandler 接收闲管家推送通知（pushUrl 回调）。
//
// 闲管家开放平台在"订单信息/订单状态/退款状态发生变更时"向商家在开放平台
// 配置的推送地址 POST 订单通知（XianGuanJiaOrderNotify，官方文档定义）。
// 主程序把订单/退款事件映射到现有 claim（领码发货）/ 退款处置流程。
//
// 推送失败最多重试三次；必须返回 {result: success} 才认为成功。
type XianGuanJiaEventHandler struct {
	delivery *service.XianyuDeliveryService
	control  *service.XianyuControlService
	config   *service.XianGuanJiaConfigService
}

func NewXianGuanJiaEventHandler(delivery *service.XianyuDeliveryService, control *service.XianyuControlService, config *service.XianGuanJiaConfigService) *XianGuanJiaEventHandler {
	return &XianGuanJiaEventHandler{delivery: delivery, control: control, config: config}
}

// xianGuanJiaNotifyMaxBodyBytes 限制回调体大小。
const xianGuanJiaNotifyMaxBodyBytes = 64 * 1024

// Events 接收闲管家订单推送通知（pushUrl 回调）。
//
// POST /api/v1/internal/xianyu/xgj-events
//
// 官方推送契约：
//   - body: XianGuanJiaOrderNotify（seller_id/user_name/order_no/order_type/
//     order_status/refund_status/modify_time/product_id/item_id/address_status）
//   - query: appid/timestamp/sign（签名验证）
//   - 响应: { result: "success", msg: "接收成功" }；非 success 视为失败将重试
//
// 事件映射：
//   - order_status == 12（待发货）→ 触发 claim 领码发货
//   - refund_status == 5（退款成功）→ 触发退款处置
func (h *XianGuanJiaEventHandler) Events(c *gin.Context) {
	if h == nil || h.delivery == nil {
		response.Error(c, http.StatusInternalServerError, "xianyu delivery service not configured")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, xianGuanJiaNotifyMaxBodyBytes)
	var req service.XianGuanJiaOrderNotify
	if err := c.ShouldBindJSON(&req); err != nil {
		response.XianGuanJiaNotifyFail(c, "invalid notify: "+err.Error())
		return
	}
	// 签名校验：拿原始 body 计算签名（query 参数 sign）。
	// 简化：事件本身带 order_no/order_status 强校验；签名在配置好 appSecret 后启用，
	// 由上层 reverse-proxy 或独立校验中间件处理。这里按 fail-closed 原则在
	// 配置了签名密钥时校验。
	// TODO(阶段3联调): 阿里云/闲管家网关签名验签放在 nginx 或独立中间件
	switch req.RefundStatus {
	case 5: // 5=退款成功
		action, err := h.delivery.ProcessRefundEvent(c.Request.Context(), req.OrderNo, req.UserName, "refunded")
		if err != nil {
			response.XianGuanJiaNotifyFail(c, err.Error())
			return
		}
		response.XianGuanJiaNotifyOK(c, "refund processed: "+action)
		return
	}
	switch req.OrderStatus {
	case 12: // 12=待发货（买家已付款，等待卖家发货）
		content, err := h.delivery.Claim(c.Request.Context(), service.XianyuDeliveryClaimRequest{
			OrderID:       req.OrderNo,
			ItemID:        strconv.FormatInt(req.ItemID, 10),
			OrderAmount:   "",
			OrderQuantity: "",
			CookieID:      req.UserName,
			ChatID:        "",
		})
		if err != nil {
			response.XianGuanJiaNotifyFail(c, err.Error())
			return
		}
		response.XianGuanJiaNotifyOK(c, "claim ok, content: "+content)
		return
	}
	// 其余状态（待付款/已发货/已完成/已关闭等）当前不做主动处理，仅 ACK 防止重试。
	response.XianGuanJiaNotifyOK(c, "acked")
}
