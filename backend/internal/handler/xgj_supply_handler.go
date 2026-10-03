package handler

import (
	"context"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/service/xianguanjia"
	"github.com/gin-gonic/gin"
)

// D6e: 货源模式（虚拟货源提卡）公开接口处理器。
//
// 网关契约：闲管家 → POST/GET https://corealgos.com/api/v1/xgj-supply/<接口>，
// 验签由 xianguanjia.SupplySignMiddleware 在路由组层完成（本 handler 不再验签）。
// 响应信封为官方 {code,msg,data}（code==0 成功），与面板 response.Success 不同。
//
// 依赖说明：完整业务逻辑由 D6c（目录：平台/商户/商品列表/详情）与
// D6d（订单：创建/详情/退款通知）提供。D6e 为集成前哨，D6c/D6d 未落地时：
//   - 平台信息 / 商户信息：本 handler 基于 SupplyConfigStore 直接实现（数据自足）；
//   - 商品列表/详情、订单创建/详情、退款通知：返回占位信封，联调前替换为真实服务。
//     （集成点见 /root/d6-evidence/d6e.md）
type XgjSupplyHandler struct {
	store   *xianguanjia.SupplyConfigStore
	gateway string
}

// NewXgjSupplyHandler 构造处理器。store 为 nil 时平台/商户信息 fail-closed 返回 1000。
func NewXgjSupplyHandler(store *xianguanjia.SupplyConfigStore, gateway string) *XgjSupplyHandler {
	return &XgjSupplyHandler{store: store, gateway: gateway}
}

// SignMiddleware 返回货源组入站验签中间件（六段签名，见 xianguanjia.SupplySignMiddleware）。
func (h *XgjSupplyHandler) SignMiddleware() gin.HandlerFunc {
	if h == nil {
		return xianguanjia.SupplySignMiddleware(nil)
	}
	return xianguanjia.SupplySignMiddleware(supplyConfigReaderAdapter{store: h.store})
}

// supplyOK 输出官方成功信封。
// supplyFail 输出官方失败信封。
// supplyGoodsListQuery 商品列表分页参数（官方：keyword/goods_type/page_no/page_size）。
type supplyGoodsListQuery struct {
	Keyword   string `form:"keyword"`
	GoodsType string `form:"goods_type"`
	PageNo    int    `form:"page_no"`
	PageSize  int    `form:"page_size"`
}

// PlatformInfo 查询平台信息：返回应用概况 AppKey（官方要求整数）。
//
// GET /api/v1/xgj-supply/platform-info
// MerchantInfo 查询商户信息：返回 balance（官方要求大于 0 的整数；自研系统固定返回）。
//
// GET /api/v1/xgj-supply/merchant-info
// GoodsList 查询商品列表（分页）。
//
// GET /api/v1/xgj-supply/goods-list?keyword=&goods_type=&page_no=&page_size=
//
// 【集成点 D6c】暂返回空列表占位；接 D6c supply_catalog.go 池聚合后替换。
// GoodsDetail 查询商品详情（goods_no）。
//
// GET /api/v1/xgj-supply/goods-detail?goods_no=
//
// 【集成点 D6c】暂返回 1100（商品不存在）占位；接 D6c 后返回真实商品。
// OrderCreate 创建卡密订单（核心：同步返回 card_items）。
//
// POST /api/v1/xgj-supply/order-create
//
// 【集成点 D6d】暂返回占位；接 D6d supply_order.go 取卡/生成后返回
// {order_status:20, card_items:[{card_no,card_pwd}]}。
func (h *XgjSupplyHandler) OrderCreate(c *gin.Context) {
	supplyFail(c, 1000, "下单接口未接线（D6d 待集成）")
}

// OrderDetail 查询订单详情（管家查单）。
//
// GET /api/v1/xgj-supply/order-detail?order_no=
//
// 【集成点 D6d】暂返回 1200（订单不存在）占位。
func (h *XgjSupplyHandler) OrderDetail(c *gin.Context) {
	supplyFail(c, 1200, "订单不存在")
}

// RefundNotify 订单退款通知：作废对应卡（复用 D2 作废语义）并返回 result=success。
//
// POST /api/v1/xgj-supply/refund-notify
//
// 【集成点 D6d】暂返回 result=success 防水位（不落回执/不作废）；接 D6d 后
// 复用 RefundCardVoider 完成作废。
func (h *XgjSupplyHandler) RefundNotify(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"result": "success", "msg": "ok"})
}

// supplyConfigReaderAdapter 把 D6a 的 *SupplyConfigStore（Get 明文版）适配为
// D6b 验签中间件所需的 SupplyConfigReader（GetSupplyConfig）。
type supplyConfigReaderAdapter struct {
	store *xianguanjia.SupplyConfigStore
}

func (a supplyConfigReaderAdapter) GetSupplyConfig(ctx context.Context) (*xianguanjia.SupplyConfig, error) {
	if a.store == nil {
		return nil, xianguanjia.ErrSupplyNoConfig
	}
	return a.store.Get(ctx)
}
