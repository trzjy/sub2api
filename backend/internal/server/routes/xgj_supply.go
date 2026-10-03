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
// 路径命名：kebab-case（platform-info/merchant-info/goods-list/goods-detail/
// order-create/order-detail/refund-notify）。
//
// 联调风险：管家侧网关 suffix 拼接规则未坐实（总单要求「先全注册」），
// 故每个接口同时注册 GET/POST 且各带「无尾斜杠 / 带尾斜杠」两种变体。
// 完整路径清单见 /root/d6-evidence/d6e.md。
//
// handler 未接线（如部分部署）时整组不注册，避免空指针。
func RegisterXgjSupplyRoutes(v1 *gin.RouterGroup, h *handler.Handlers) {
	if h == nil || h.XgjSupply == nil {
		return
	}
	grp := v1.Group("/xgj-supply")
	grp.Use(h.XgjSupply.SignMiddleware())

	register := func(path string, fn gin.HandlerFunc) {
		grp.GET(path, fn)
		grp.POST(path, fn)
		if !strings.HasSuffix(path, "/") {
			grp.GET(path+"/", fn)
			grp.POST(path+"/", fn)
		}
	}

	register("/platform-info", h.SupplyCatalog.PlatformInfo)
	register("/merchant-info", h.SupplyCatalog.MerchantInfo)
	register("/goods-list", h.SupplyCatalog.ListGoods)
	register("/goods-detail", h.SupplyCatalog.GoodsDetail)
	register("/order-create", h.XgjSupply.OrderCreate)
	register("/order-detail", h.XgjSupply.OrderDetail)
	register("/refund-notify", h.XgjSupply.RefundNotify)
}
