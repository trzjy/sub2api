package routes

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/handler"

	"github.com/gin-gonic/gin"
)

// D6e: 货源模式（虚拟货源提卡）公开路由组。
//
// 前缀：/api/v1/xgj-supply（总单约定）。套六段验签中间件
// （xianguanjia.SupplySignMiddleware，见 handler.XgjSupplyHandler.SignMiddleware）。
//
// 路径契约：官方 Apifox 文档（shared-cf4d53fd），路径 = 接口网关 + /goofish/... 后缀，
// 生产证据为闲管家保存配置时真实调用 POST /api/v1/xgj-supply/goofish/open/info。
// 仅注册 POST（官方契约不含读方法），每条路径保留「无尾斜杠 / 带尾斜杠」两个变体。
//
// handler 未接线（如部分部署）时整组不注册，避免空指针。
func RegisterXgjSupplyRoutes(v1 *gin.RouterGroup, h *handler.Handlers) {
	if h == nil || h.XgjSupply == nil {
		return
	}
	grp := v1.Group("/xgj-supply")
	grp.Use(h.XgjSupply.SignMiddleware())

	register := func(path string, fn gin.HandlerFunc) {
		grp.POST(path, fn)
		if !strings.HasSuffix(path, "/") {
			grp.POST(path+"/", fn)
		}
	}

	register("/goofish/open/info", h.SupplyCatalog.PlatformInfo)
	register("/goofish/user/info", h.SupplyCatalog.MerchantInfo)
	register("/goofish/goods/list", h.SupplyCatalog.ListGoods)
	register("/goofish/goods/detail", h.SupplyCatalog.GoodsDetail)
	register("/goofish/order/purchase/create", h.XgjSupplyOrder.CreateOrder)
	register("/goofish/order/detail", h.XgjSupplyOrder.GetOrder)
	register("/goofish/order/refund/apply", h.XgjSupplyOrder.RefundNotify)
	register("/goofish/order/refund/notify", h.XgjSupplyOrder.RefundResultNotify)
}
