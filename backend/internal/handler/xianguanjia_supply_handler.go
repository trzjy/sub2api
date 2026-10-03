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

// D6c 商品目录被调接口 handler：查询平台信息 / 查询商户信息 / 查询商品列表 / 查询商品详情。
//
// 路由挂载归 D6e（本单元只产出 handler 函数）。验签中间件由 D6b 提供，
// 在路由层接入；本 handler 假定进入时请求已通过验签。
//
// 信封：货源被调接口统一使用 {code,msg,data}（与 ERP 方向响应结构不同——ERP 侧
// response 包用的是 {code,message,data}），code==0 成功。故此处不复用 response 包，
// 直接输出货源信封，字段名严格对齐官方（msg 而非 message）。

// supplyMaxBodyBytes 限制请求 body 大小（列表请求体很小，64KB 足够）。
const supplyMaxBodyBytes = 64 * 1024

// SupplyCatalogService 抽象目录服务能力，便于 handler 单测注入 fake。
// 由 *xianguanjia.SupplyCatalogService 隐式满足。
type SupplyCatalogService interface {
	PlatformInfo(ctx context.Context) (*xianguanjia.SupplyPlatformInfo, error)
	MerchantInfo(ctx context.Context) (*xianguanjia.SupplyMerchantInfo, error)
	ListGoods(ctx context.Context, req xianguanjia.ListGoodsRequest) (*xianguanjia.ListGoodsResult, error)
	GoodsDetail(ctx context.Context, goodsNo string) (*xianguanjia.SupplyGoods, error)
}

// XianguanjiaSupplyHandler 货源目录接口处理器。
type XianguanjiaSupplyHandler struct {
	svc SupplyCatalogService
}

// NewXianguanjiaSupplyHandler 构造处理器。svc 必填（fail-closed）。
func NewXianguanjiaSupplyHandler(svc SupplyCatalogService) *XianguanjiaSupplyHandler {
	return &XianguanjiaSupplyHandler{svc: svc}
}

// supplyEnvelope 是货源被调接口统一响应信封。
type supplyEnvelope struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data any    `json:"data,omitempty"`
}

// supplyOK 输出成功信封（code==0）。
func supplyOK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, supplyEnvelope{Code: xianguanjia.SupplyCodeOK, Msg: "success", Data: data})
}

// supplyFail 输出失败信封（HTTP 始终 200，以信封 code 判定成败）。
func supplyFail(c *gin.Context, code int, msg string) {
	c.JSON(http.StatusOK, supplyEnvelope{Code: code, Msg: msg})
}

// supplyFailFromError 把 service 错误转成失败信封：
//   - *SupplyAPIError（含 1100 商品不存在）：按其 Code/Msg 输出；
//   - 其它错误：内部错误，记录日志并返回 1200 之外的通用失败码（选用 1100 会误导，
//     此处用非官方保留码 500 表示服务端异常，具体见联调校正清单）。
func supplyFailFromError(c *gin.Context, err error) {
	var apiErr *xianguanjia.SupplyAPIError
	if errors.As(err, &apiErr) {
		supplyFail(c, apiErr.Code, apiErr.Msg)
		return
	}
	slog.Error("xianguanjia supply handler internal error", "err", err)
	supplyFail(c, http.StatusInternalServerError, "internal error")
}

// PlatformInfo 查询平台信息（POST /api/v1/xgj-supply/platform/info）。
// 无请求体；返回 app_id（应用概况 AppKey，整数）。
func (h *XianguanjiaSupplyHandler) PlatformInfo(c *gin.Context) {
	if h == nil || h.svc == nil {
		supplyFail(c, http.StatusInternalServerError, "supply service unavailable")
		return
	}
	info, err := h.svc.PlatformInfo(c.Request.Context())
	if err != nil {
		supplyFailFromError(c, err)
		return
	}
	supplyOK(c, info)
}

// MerchantInfo 查询商户信息（POST /api/v1/xgj-supply/merchant/info）。
// 无请求体；返回 mch_id 与 balance（> 0 的整数）。
func (h *XianguanjiaSupplyHandler) MerchantInfo(c *gin.Context) {
	if h == nil || h.svc == nil {
		supplyFail(c, http.StatusInternalServerError, "supply service unavailable")
		return
	}
	info, err := h.svc.MerchantInfo(c.Request.Context())
	if err != nil {
		supplyFailFromError(c, err)
		return
	}
	supplyOK(c, info)
}

// supplyListGoodsBody 是「查询商品列表」的请求体。
// 类型纪律：page_no/page_size/goods_type 严格为整数（官方 schema 为准）。
type supplyListGoodsBody struct {
	Keyword   string `json:"keyword"`
	GoodsType int    `json:"goods_type"`
	PageNo    int    `json:"page_no"`
	PageSize  int    `json:"page_size"`
}

// ListGoods 查询商品列表（POST /api/v1/xgj-supply/goods/list）。
// 入参 keyword/goods_type/page_no/page_size；返回 {list,total,page_no,page_size}。
func (h *XianguanjiaSupplyHandler) ListGoods(c *gin.Context) {
	if h == nil || h.svc == nil {
		supplyFail(c, http.StatusInternalServerError, "supply service unavailable")
		return
	}
	var body supplyListGoodsBody
	if !decodeSupplyBody(c, &body) {
		return
	}
	res, err := h.svc.ListGoods(c.Request.Context(), xianguanjia.ListGoodsRequest{
		Keyword:   body.Keyword,
		GoodsType: body.GoodsType,
		PageNo:    body.PageNo,
		PageSize:  body.PageSize,
	})
	if err != nil {
		supplyFailFromError(c, err)
		return
	}
	if res != nil && res.List == nil {
		// 保证空列表序列化为 []（而非 null），与官方商品数组类型一致。
		res.List = []xianguanjia.SupplyGoods{}
	}
	supplyOK(c, res)
}

// supplyGoodsDetailBody 是「查询商品详情」的请求体。
type supplyGoodsDetailBody struct {
	GoodsNo string `json:"goods_no"`
}

// GoodsDetail 查询商品详情（POST /api/v1/xgj-supply/goods/detail）。
// 入参 goods_no；不存在返回 code=1100。
func (h *XianguanjiaSupplyHandler) GoodsDetail(c *gin.Context) {
	if h == nil || h.svc == nil {
		supplyFail(c, http.StatusInternalServerError, "supply service unavailable")
		return
	}
	var body supplyGoodsDetailBody
	if !decodeSupplyBody(c, &body) {
		return
	}
	g, err := h.svc.GoodsDetail(c.Request.Context(), body.GoodsNo)
	if err != nil {
		supplyFailFromError(c, err)
		return
	}
	supplyOK(c, g)
}

// decodeSupplyBody 读取并解析 JSON 请求体。空 body 视为全零值（如列表接口可无 body）。
// 解析失败输出 400 信封并返回 false。
func decodeSupplyBody(c *gin.Context, dst any) bool {
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, supplyMaxBodyBytes))
	if err != nil {
		supplyFail(c, http.StatusBadRequest, "read body failed")
		return false
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return true
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		supplyFail(c, http.StatusBadRequest, "invalid body: "+err.Error())
		return false
	}
	return true
}

// 编译期断言：service 实现满足 handler 接口。
var _ SupplyCatalogService = (*xianguanjia.SupplyCatalogService)(nil)
