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
// 官方错误码：1100 商品不存在 / 1101 商品不可用 / 1102 库存不足 / 1200 订单不存在 /
// 1201 下单参数错误 / 1202 下单金额低于成本价 / 1203 订单号已存在 / 1209 下单超时。
// 本实现中：
//   - 商品不存在 → 1100；商品不可用 → 1101；库存不足 → 1102；max_amount 超额 → 1202；
//   - 查单/退款订单不存在 → 1200；
//   - 下单必填参数缺失（order_no/goods_no/buy_quantity）→ 1201；
//   - 幂等重发不返回 1203，而是同步返回既有 card_items（资金红线：不重发卡）；
//   - 其余内部/参数错误在创建接口归一为 1209（下单超时，可重试语义）。

// XianguanjiaSupplyOrderService 是 handler 依赖的订单服务窄接口。
// *xianguanjia.SupplyOrderService 隐式满足。
type XianguanjiaSupplyOrderService interface {
	// CreateOrder 创建卡密订单（maxAmount 分：0=不校验）。
	CreateOrder(ctx context.Context, managerOrderNo, goodsNo string, buyQuantity int, maxAmount int64) (*xianguanjia.SupplyOrder, error)
	// GetOrder 查单：order_no 优先；为空时按 out_order_no（我方订单 id）查。
	GetOrder(ctx context.Context, orderNo, outOrderNo string) (*xianguanjia.SupplyOrder, error)
	// RefundNotify 退款通知：返回 (订单视图, 是否可撤单 agree)。
	RefundNotify(ctx context.Context, managerOrderNo string) (*xianguanjia.SupplyOrder, bool, error)
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

// createSupplyOrderBody 创建卡密订单请求体（官方字段名，严格单一命名）。
//
// 删除旧兼容键 manager_order_no/order_sn/quantity/num：官方强校验单一命名，
// 旧链归零。max_amount（分，可选）：0 表示不校验。
type createSupplyOrderBody struct {
	OrderNo     string `json:"order_no"`
	GoodsNo     string `json:"goods_no"`
	BuyQuantity int    `json:"buy_quantity"`
	NotifyURL   string `json:"notify_url"`
	BizOrderNo  string `json:"biz_order_no"`
	MaxAmount   int64  `json:"max_amount"`
	ProductID   int64  `json:"product_id"`
	ProductSKU  int64  `json:"product_sku"`
	ItemID      int64  `json:"item_id"`
}

// supplyOrderQueryBody 查单 / 退款申请请求体（官方字段名）。
// 查单：order_no 优先，out_order_no 兜底。退款申请在此基础上宽松解析退款字段。
type supplyOrderQueryBody struct {
	OrderType  int    `json:"order_type"`
	OrderNo    string `json:"order_no"`
	OutOrderNo string `json:"out_order_no"`
}

// supplyOrderRefundBody 退款申请请求体：查单字段 + 官方退款字段（宽松解析，
// 多余键忽略）。仅使用 order_no/out_order_no/apply_time；其余键按官方名保留解析。
type supplyOrderRefundBody struct {
	OrderType         int    `json:"order_type"`
	OrderNo           string `json:"order_no"`
	OutOrderNo        string `json:"out_order_no"`
	BizOrderNo        string `json:"biz_order_no"`
	RefundType        int    `json:"refund_type"`
	RefundAmount      int64  `json:"refund_amount"`
	RefundReason      string `json:"refund_reason"`
	RefundScene       string `json:"refund_scene"`
	ApplyTime         int64  `json:"apply_time"`
	RefundCallbackURL string `json:"refund_callback_url"`
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
// 成功同步返回官方订单视图（order_status=20 + card_items[]{card_no:"",card_pwd:兑换码}）；
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
	orderNo := strings.TrimSpace(body.OrderNo)
	goodsNo := strings.TrimSpace(body.GoodsNo)
	buyQuantity := body.BuyQuantity
	// 必填校验：order_no/goods_no 非空、buy_quantity>0；缺 → 1201（替换原 1209 误用）。
	if orderNo == "" || goodsNo == "" || buyQuantity <= 0 {
		c.JSON(http.StatusOK, supplyOrderEnvelope{
			Code: xianguanjia.SupplyCodeOrderParamError,
			Msg:  "order_no, goods_no and buy_quantity are required",
		})
		return
	}
	order, err := h.svc.CreateOrder(c.Request.Context(), orderNo, goodsNo, buyQuantity, body.MaxAmount)
	if err != nil {
		supplyOrderRespondErr(c, err, xianguanjia.SupplyCodeOrderTimeout)
		return
	}
	supplyOrderRespondOK(c, order)
}

// GetOrder 处理「查询订单详情」（POST，order_no 优先、out_order_no 兜底）。
func (h *XianguanjiaSupplyOrderHandler) GetOrder(c *gin.Context) {
	if h == nil || h.svc == nil {
		supplyOrderRespondErr(c, errors.New("supply order service unavailable"), xianguanjia.SupplyCodeOrderNotFound)
		return
	}
	orderNo := strings.TrimSpace(c.Query("order_no"))
	var body supplyOrderQueryBody
	if orderNo == "" {
		_ = readSupplyOrderBody(c, &body)
		orderNo = strings.TrimSpace(body.OrderNo)
	}
	outOrderNo := ""
	if orderNo == "" {
		outOrderNo = strings.TrimSpace(body.OutOrderNo)
	}
	if orderNo == "" && outOrderNo == "" {
		c.JSON(http.StatusOK, supplyOrderEnvelope{
			Code: xianguanjia.SupplyCodeOrderNotFound,
			Msg:  "order_no or out_order_no is required",
		})
		return
	}
	order, err := h.svc.GetOrder(c.Request.Context(), orderNo, outOrderNo)
	if err != nil {
		supplyOrderRespondErr(c, err, xianguanjia.SupplyCodeOrderNotFound)
		return
	}
	supplyOrderRespondOK(c, order)
}

// RefundNotify 处理「订单退款申请」（POST，官方 /goofish/order/refund/apply 语义）。
//
// 同步撤单：订单已是退款态（幂等重复）或本次全量作废成功 → 同意，
// 返回 200 信封 {result:"agree", refund_data:{apply_time, refund_status:20,
// refund_amount, refund_time}}；零或部分作废（卡已使用/已过期、无法整单撤回）→ 拒绝，
// 返回 {result:"refuse", remark:"卡密已使用或已过期，无法撤单"}。
func (h *XianguanjiaSupplyOrderHandler) RefundNotify(c *gin.Context) {
	if h == nil || h.svc == nil {
		supplyOrderRespondErr(c, errors.New("supply order service unavailable"), xianguanjia.SupplyCodeOrderTimeout)
		return
	}
	var body supplyOrderRefundBody
	if err := readSupplyOrderBody(c, &body); err != nil {
		supplyOrderRespondErr(c, err, xianguanjia.SupplyCodeOrderTimeout)
		return
	}
	orderNo := strings.TrimSpace(body.OrderNo)
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
	order, agree, err := h.svc.RefundNotify(c.Request.Context(), orderNo)
	if err != nil {
		supplyOrderRespondErr(c, err, xianguanjia.SupplyCodeOrderTimeout)
		return
	}
	if agree {
		// 可撤单：同意 + 退款数据（refund_status=20 成功；refund_amount=订单快照金额）。
		// refund_time 取订单终态时刻 order.EndTime（退款态=refunded_at，幂等重复时
		// 仍是原退款时刻，而非本次请求时刻），由 service 决策后 handler 组装信封。
		supplyOrderRespondOK(c, gin.H{
			"result": "agree",
			"refund_data": gin.H{
				"apply_time":    body.ApplyTime,
				"refund_status": 20,
				"refund_amount": order.OrderAmount,
				"refund_time":   order.EndTime,
			},
		})
		return
	}
	// 不可撤（卡已使用/已过期）：拒绝 + 原因。data 仅含 result/remark。
	supplyOrderRespondOK(c, gin.H{
		"result": "refuse",
		"remark": "卡密已使用或已过期，无法撤单",
	})
}
