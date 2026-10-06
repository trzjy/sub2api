package handler

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/service/xianguanjia"
	"github.com/gin-gonic/gin"
)

// D6e: 货源模式（虚拟货源提卡）公开接口处理器（验签中间件宿主）。
//
// 网关契约：闲管家 → POST https://corealgos.com/api/v1/xgj-supply/goofish/...，
// 验签由 xianguanjia.SupplySignMiddleware 在路由组层完成（本 handler 不再验签）。
// 响应信封为官方 {code,msg,data}（code==0 成功），与面板 response.Success 不同。
//
// 依赖说明：平台信息 / 商户信息由本 handler 基于 SupplyConfigStore 直接实现
// （数据自足）；目录接口由 D6c XianguanjiaSupplyHandler 提供，订单接口由
// D6d XianguanjiaSupplyOrderHandler 提供（路由接线见 routes/xgj_supply.go）。
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
