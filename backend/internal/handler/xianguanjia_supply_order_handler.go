package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service/xianguanjia"
	"github.com/gin-gonic/gin"
)

// D6d：闲管家「虚拟货源」卡密订单接口的 gin handler（三个）。
//
// 路由注册归 D6e；本文件只提供 handler 方法。响应统一官方信封 {code,msg,data}，
// HTTP 状态固定 200（官方以信封 code 判定成败）。
//
// 官方错误码：1102 库存不足 / 1200 订单不存在 / 1203 订单号已存在 /
// 1209 下单超时。本实现中：
//   - 库存不足 → 1102；
//   - 查单/退款订单不存在 → 1200；
//   - 幂等重发不返回 1203，而是同步返回既有 card_items（资金红线：不重发卡）；
//   - 其余内部/参数错误在创建接口归一为 1209（下单超时，可重试语义）。

// XianguanjiaSupplyOrderService 是 handler 依赖的订单服务窄接口。
// *xianguanjia.SupplyOrderService 隐式满足。
type XianguanjiaSupplyOrderService interface {
	CreateOrder(ctx context.Context, managerOrderNo, goodsNo string, quantity int) (*xianguanjia.SupplyOrder, error)
	GetOrder(ctx context.Context, managerOrderNo string) (*xianguanjia.SupplyOrder, error)
	RefundNotify(ctx context.Context, managerOrderNo string) (*xianguanjia.SupplyOrder, error)
}

// XianguanjiaSupplyOrderHandler 卡密订单处理器。
type XianguanjiaSupplyOrderHandler struct {
	svc XianguanjiaSupplyOrderService
}

// NewXianguanjiaSupplyOrderHandler 构造订单处理器。svc 必填。
func NewXianguanjiaSupplyOrderHandler(svc XianguanjiaSupplyOrderService) *XianguanjiaSupplyOrderHandler {
	return &XianguanjiaSupplyOrderHandler{svc: svc}
}

// supplyOrderMaxBodyBytes 限制请求 body 大小（卡密订单体量小）。
const supplyOrderMaxBodyBytes = 64 * 1024

// supplyOrderEnvelope 是官方响应信封 {code,msg,data}。
type supplyOrderEnvelope struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data any    `json:"data,omitempty"`
}

// supplyOrderRespondOK 输出成功信封（code=0）。
func supplyOrderRespondOK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, supplyOrderEnvelope{Code: xianguanjia.SupplyCodeOK, Msg: "success", Data: data})
}

// supplyOrderRespondErr 输出失败信封：优先取业务错误码（*SupplyAPIError），
// 其余归为 fallbackCode（调用方按接口语义指定，创建接口用 1209 下单超时）。
func supplyOrderRespondErr(c *gin.Context, err error, fallbackCode int) {
	var apiErr *xianguanjia.SupplyAPIError
	if errors.As(err, &apiErr) && apiErr.Code != xianguanjia.SupplyCodeOK {
		c.JSON(http.StatusOK, supplyOrderEnvelope{Code: apiErr.Code, Msg: apiErr.Msg})
		return
	}
	// 内部错误：不回显细节，留痕后按 fallback 码返回（可重试语义）。
	slog.Error("xianguanjia supply order handler internal error",
		"path", c.FullPath(), "err", err)
	c.JSON(http.StatusOK, supplyOrderEnvelope{Code: fallbackCode, Msg: "internal error"})
}

// createSupplyOrderBody 创建卡密订单请求体。
//
// 官方字段名以 doc-4985015 schema 为准；本仓无该文档，按语义 + 常见命名实现，
// 并对管家订单号做别名兼容（order_no / manager_order_no / order_sn）。
type createSupplyOrderBody struct {
	OrderNo        string `json:"order_no"`
	ManagerOrderNo string `json:"manager_order_no"`
	OrderSn        string `json:"order_sn"`
	GoodsNo        string `json:"goods_no"`
	Quantity       int    `json:"quantity"`
	Num            int    `json:"num"`
	NotifyURL      string `json:"notify_url"`
}

func (b createSupplyOrderBody) managerOrderNo() string {
	for _, v := range []string{b.OrderNo, b.ManagerOrderNo, b.OrderSn} {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func (b createSupplyOrderBody) quantity() int {
	if b.Quantity > 0 {
		return b.Quantity
	}
	return b.Num
}

// supplyOrderOrderNoBody 查单/退款请求体（只取管家订单号）。
type supplyOrderOrderNoBody struct {
	OrderNo        string `json:"order_no"`
	ManagerOrderNo string `json:"manager_order_no"`
	OrderSn        string `json:"order_sn"`
}

func (b supplyOrderOrderNoBody) managerOrderNo() string {
	for _, v := range []string{b.OrderNo, b.ManagerOrderNo, b.OrderSn} {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// readSupplyOrderBody 读取并解析 JSON body（限长）。
func readSupplyOrderBody(c *gin.Context, dst any) error {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, supplyOrderMaxBodyBytes))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	return json.Unmarshal(body, dst)
}

// CreateOrder 处理「创建卡密订单」（POST）。
// 成功同步返回 order_status=20 + card_items[]{card_no,card_pwd}；
// 幂等重发返回同一批 card_items（不重发卡）。
func (h *XianguanjiaSupplyOrderHandler) CreateOrder(c *gin.Context) {
	if h == nil || h.svc == nil {
		supplyOrderRespondErr(c, errors.New("supply order service unavailable"), xianguanjia.SupplyCodeOrderTimeout)
		return
	}
	var body createSupplyOrderBody
	if err := readSupplyOrderBody(c, &body); err != nil {
		supplyOrderRespondErr(c, err, xianguanjia.SupplyCodeOrderTimeout)
		return
	}
	orderNo := body.managerOrderNo()
	goodsNo := strings.TrimSpace(body.GoodsNo)
	quantity := body.quantity()
	if orderNo == "" || quantity <= 0 {
		// 参数非法：创建接口按 1209（下单超时）语义返回，提示可重试修正。
		c.JSON(http.StatusOK, supplyOrderEnvelope{
			Code: xianguanjia.SupplyCodeOrderTimeout,
			Msg:  "order_no and quantity are required",
		})
		return
	}
	order, err := h.svc.CreateOrder(c.Request.Context(), orderNo, goodsNo, quantity)
	if err != nil {
		supplyOrderRespondErr(c, err, xianguanjia.SupplyCodeOrderTimeout)
		return
	}
	supplyOrderRespondOK(c, order)
}

// GetOrder 处理「查询订单详情」（GET/POST，管家订单号可来自 query 或 body）。
func (h *XianguanjiaSupplyOrderHandler) GetOrder(c *gin.Context) {
	if h == nil || h.svc == nil {
		supplyOrderRespondErr(c, errors.New("supply order service unavailable"), xianguanjia.SupplyCodeOrderNotFound)
		return
	}
	orderNo := strings.TrimSpace(c.Query("order_no"))
	if orderNo == "" {
		orderNo = strings.TrimSpace(c.Query("manager_order_no"))
	}
	if orderNo == "" {
		var body supplyOrderOrderNoBody
		if err := readSupplyOrderBody(c, &body); err == nil {
			orderNo = body.managerOrderNo()
		}
	}
	if orderNo == "" {
		c.JSON(http.StatusOK, supplyOrderEnvelope{
			Code: xianguanjia.SupplyCodeOrderNotFound,
			Msg:  "order_no is required",
		})
		return
	}
	order, err := h.svc.GetOrder(c.Request.Context(), orderNo)
	if err != nil {
		supplyOrderRespondErr(c, err, xianguanjia.SupplyCodeOrderNotFound)
		return
	}
	supplyOrderRespondOK(c, order)
}

// RefundNotify 处理「订单退款通知」（POST）：订单置退款态并作废对应卡。
// 成功返回 result=success（官方退款通知以成功/失败语义消费）。
func (h *XianguanjiaSupplyOrderHandler) RefundNotify(c *gin.Context) {
	if h == nil || h.svc == nil {
		supplyOrderRespondErr(c, errors.New("supply order service unavailable"), xianguanjia.SupplyCodeOrderTimeout)
		return
	}
	var body supplyOrderOrderNoBody
	if err := readSupplyOrderBody(c, &body); err != nil {
		supplyOrderRespondErr(c, err, xianguanjia.SupplyCodeOrderTimeout)
		return
	}
	orderNo := body.managerOrderNo()
	if orderNo == "" {
		orderNo = strings.TrimSpace(c.Query("order_no"))
	}
	if orderNo == "" {
		c.JSON(http.StatusOK, supplyOrderEnvelope{
			Code: xianguanjia.SupplyCodeOrderNotFound,
			Msg:  "order_no is required",
		})
		return
	}
	order, err := h.svc.RefundNotify(c.Request.Context(), orderNo)
	if err != nil {
		supplyOrderRespondErr(c, err, xianguanjia.SupplyCodeOrderTimeout)
		return
	}
	// 官方退款通知成功语义：code=0 且 data.result=success。
	supplyOrderRespondOK(c, gin.H{
		"result":       "success",
		"order_status": order.OrderStatus,
	})
}
