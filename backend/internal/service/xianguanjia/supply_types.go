package xianguanjia

import (
	"context"
	"errors"
	"fmt"
)

// 本文件是 D6「虚拟货源（被调接口）」方向的共享类型定义，由 D6c 建立，
// D6a（凭证读写）/ D6b（验签中间件）/ D6d（创建订单）复用。
//
// 契约权威：鲜管家供应链开放平台官方接口（Apifox shared-cf4d53fd，接口
// api-205398642 / api-208575910 / api-205405059 / api-205405060）。字段名与类型
// 严格对齐官方 schema（data 对象 additionalProperties:false，不得多返回字段）。
//
// 类型纪律（官方原话「否则会对接失败，整数就是整数」）：对外返回的 app_id / balance /
// stock / status / price(分) / update_time(秒) 一律为整数类型，不得用字符串承载数字。

// ---- 官方业务错误码 ----
//
// 出处：总单「错误码」段（doc-7646489）。签名类错误（401/408）由 D6b 验签中间件负责；
// 本文件定义业务类错误码，D6c/D6d 共用。信封 code==0 表示成功，非 0 时 msg 承载原因。
const (
	SupplyCodeOK                = 0    // 成功
	SupplyCodeNoConfig          = 1    // 货源未配置（我方内部 fail-closed，非官方表项）
	SupplyCodeSignError         = 401  // 签名错误（D6b 中间件）
	SupplyCodeTimestampExpired  = 408  // 时间戳过期（D6b 中间件）
	SupplyCodeMerchantNotFound  = 1000 // 商户不存在
	SupplyCodeGoodsNotFound     = 1100 // 商品不存在
	SupplyCodeGoodsUnavailable  = 1101 // 商品不可用
	SupplyCodeStockInsufficient = 1102 // 库存不足
	SupplyCodeOrderNotFound     = 1200 // 订单不存在
	SupplyCodeOrderDuplicated   = 1203 // 订单号已存在
	SupplyCodeOrderTimeout      = 1209 // 下单超时
)

// SupplyAPIError 是货源被调接口的业务错误：Code 为官方错误码，Msg 为对外提示。
// handler 用 errors.As 取出后写入信封 {code,msg}；非该类型错误统一按内部错误处理。
type SupplyAPIError struct {
	Code int
	Msg  string
}

func (e *SupplyAPIError) Error() string {
	return fmt.Sprintf("xianguanjia supply api error %d: %s", e.Code, e.Msg)
}

// NewSupplyAPIError 构造业务错误。
func NewSupplyAPIError(code int, msg string) *SupplyAPIError {
	return &SupplyAPIError{Code: code, Msg: msg}
}

// ErrSupplyGoodsNotFound 是「商品不存在」的哨兵错误（code=1100），
// 供 errors.Is 判定；对外 msg 走 handler 固定文案。
var ErrSupplyGoodsNotFound = NewSupplyAPIError(SupplyCodeGoodsNotFound, "商品不存在")

// ---- 货源配置 ----

// SupplyConfig 是虚拟货源方向的凭证与标识配置（D6a 实现读取）。
//
// 字段与 252 表 xianyu_xianguanjia_config 的对应关系（D6a 落地，见总单「凭证」段）：
//   - MchID / MchSecretEncrypted 复用 252 表 mch_id / mch_secret_encrypted 两列
//     （ERP 方向当初预留、无对应物；货源方向正好有对应物）；
//   - SupplyAppID / SupplyAppSecretEncrypted 为应用概况（AppKey/AppSecret）凭证，
//     D6a 以新增列或 settings KV 落地，本单元只读取、不关心存储细节。

// SupplyConfigReader 读取货源方向的 active 配置（只读侧）。D6a 提供 DB 实现。
// fail-closed 约定：无 active 配置时返回 (nil, nil)，由调用方拒绝服务。
type SupplyConfigReader interface {
	GetSupplyConfig(ctx context.Context) (*SupplyConfig, error)
}

// ---- 商品目录 DTO ----

// SupplyGoods 是「池聚合」出的商品视图。
//
// 映射关系（详见证据 d6c.md）：
//   - GoodsNo     = 分组 ID（groups.id）的十进制字符串；
//   - GoodsType   = 商品类型，我方货源全为卡密，恒为 2；
//   - GoodsName   = 分组名（groups.name）；
//   - Price       = 分组售价（subscription_plans.price_cny）换算为「分」的整数；
//   - Stock       = 组内未使用卡密数（redeem_codes WHERE group_id=? AND type='subscription'
//     AND status='unused'）；
//   - Status      = 商品状态：1=在架（分组 active 且未软删），2=下架；
//   - UpdateTime  = 分组更新时间（groups.updated_at）的 Unix 秒。
//
// JSON tag 严格对齐官方 schema（additionalProperties:false，不得多返回字段）。
type SupplyGoods struct {
	GoodsNo    string `json:"goods_no"`
	GoodsType  int    `json:"goods_type"`
	GoodsName  string `json:"goods_name"`
	Price      int64  `json:"price"`      // 分
	Stock      int    `json:"stock"`
	Status     int    `json:"status"`     // 1=在架 2=下架（官方枚举）
	UpdateTime int64  `json:"update_time"` // 秒
}

// 商品状态枚举（对外整数，官方 schema 1=在架 2=下架）。
const (
	SupplyGoodsStatusOnSale  = 1 // 在架（分组 active 且未软删）
	SupplyGoodsStatusOffSale = 2 // 下架（分组停用/软删）
)

// SupplyGoodsSource 是「池 → 商品」聚合数据源。DB 实现从 groups/subscription_plans/
// redeem_codes 聚合；内存实现供单测与 handler 测试使用。
type SupplyGoodsSource interface {
	// ListGoods 按 keyword 返回商品与总数。keyword 同时支持商品名称模糊与商品编码精准。
	// offset/limit 由实现完成分页；limit<=0 表示不限。
	ListGoods(ctx context.Context, keyword string, offset, limit int) ([]SupplyGoods, int, error)
	// GetGoods 按 goods_no 查询单个商品；不存在返回 (nil, nil)，由 service 归一为 1100。
	GetGoods(ctx context.Context, goodsNo string) (*SupplyGoods, error)
}

// ---- 平台 / 商户信息 DTO ----

// SupplyPlatformFeatures 是平台信息中的能力开关（官方 data.features）。
type SupplyPlatformFeatures struct {
	IsSupportOrderRefund  bool `json:"is_support_order_refund"`
	IsSupportGoodsNotify  bool `json:"is_support_goods_notify"`
	IsSupportLossPurchase bool `json:"is_support_loss_purchase"`
}

// SupplyPlatformInfo 是「查询平台信息」的返回体。
// app_id 必须为当前对接的应用概况 AppKey（整数），官方原话见总单「官方返回要求」。
type SupplyPlatformInfo struct {
	AppID    int64                 `json:"app_id"`
	Features SupplyPlatformFeatures `json:"features"`
}

// SupplyMerchantInfo 是「查询商户信息」的返回体。
// 官方 schema：data 仅含 balance（> 0 的整数）。自研系统固定返回即可（默认 999999）。
type SupplyMerchantInfo struct {
	Balance int64 `json:"balance"`
}

// ---- 商品列表入参 ----

// SupplyGoodsTypeKami 是卡密商品类型（官方 goods_type=2）。
const SupplyGoodsTypeKami = 2

// ListGoodsRequest 是「查询商品列表」的入参（分页 + 过滤）。
type ListGoodsRequest struct {
	Keyword   string
	GoodsType int
	PageNo    int
	PageSize  int
}

// ListGoodsResult 是「查询商品列表」的返回体。
// 官方 schema：data 仅含 {list, count}（count 即总数，并入原 total 语义）。
type ListGoodsResult struct {
	List  []SupplyGoods `json:"list"`
	Count int           `json:"count"`
}

// 分页边界（防御性默认）。官方 page_size 上限为 100。
const (
	supplyDefaultPageNo   = 1
	supplyDefaultPageSize = 20
	supplyMaxPageSize     = 100
)

// normalize 归一化分页入参：page_no<1 → 1；page_size<1 → 默认；超过上限则截断。
func (r ListGoodsRequest) normalize() (pageNo, pageSize int) {
	pageNo = r.PageNo
	if pageNo < 1 {
		pageNo = supplyDefaultPageNo
	}
	pageSize = r.PageSize
	if pageSize < 1 {
		pageSize = supplyDefaultPageSize
	}
	if pageSize > supplyMaxPageSize {
		pageSize = supplyMaxPageSize
	}
	return pageNo, pageSize
}

// isSupplyGoodsNotFound 判断 err 是否为「商品不存在」（错误链中任一环命中即真）。
func isSupplyGoodsNotFound(err error) bool {
	return errors.Is(err, ErrSupplyGoodsNotFound)
}
