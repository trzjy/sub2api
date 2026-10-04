package xianguanjia

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

// D6d：闲管家「虚拟货源」卡密订单接口的服务实现。
//
// 资金红线（最高优先级）：
//   1) 同一管家订单号只能发一次卡——任何路径（含并发重发）都不得对同一
//      manager_order_no 二次返回不同的 card_items。实现上以
//      xianguanjia_supply_orders.manager_order_no UNIQUE 约束为最终仲裁：
//      取卡成功后在同一事务内写订单行，写冲突（唯一键）即回滚并回读已发卡，
//      从而保证「先到者发卡、后到者拿同一批卡」。
//   2) 作废只允许 delivered→expired（复用 D2 语义），不得触碰 used/unused。
//
// 与 D4b 的取卡语义一致：渠道卡（货给闲管家）状态机为 unused→delivered；
// 本服务取卡时直接把 unused 条件更新为 delivered，即「已发给闲管家订单」。
// 退款通知把该订单的卡 delivered→expired（复用 CodeCardVoidRepository，
// 只作废 delivered/unused，不误伤 used/expired）。

// 货源订单状态（内部存储用；对外 order_status 见 supplyOrderStatusSuccess）。
const (
	// supplyOrderStatusCreating 内部占位态：仅存在于「创建订单」写入事务的
	// 极短窗口（取卡→写行的同一事务内）；事务提交后即为 Success。不回读为成功。
	supplyOrderStatusCreating = 10
	// supplyOrderStatusSuccess 官方 order_status=20：卡密订单创建成功。
	supplyOrderStatusSuccess = 20
	// supplyOrderStatusRefunded 已退款：退款通知到达后置位，同时作废卡。
	// 对外订单枚举无退款态，须归一为 20（见 supplyOrderStatusToOfficial）。
	supplyOrderStatusRefunded = 30
)

// 下单业务错误码（官方 schema，D6E-02c 新增；不使用 supply_types.go 既有项）。
const (
	SupplyCodeOrderParamError      = 1201 // 下单参数错误（替换原 1209 误用）
	SupplyCodeOrderAmountBelowCost = 1202 // 下单金额低于成本价（max_amount 超额）
)

// ErrSupplyOrderNotFound 是「订单不存在」的哨兵错误（code=1200）。
var ErrSupplyOrderNotFound = NewSupplyAPIError(SupplyCodeOrderNotFound, "订单不存在")

// ---- 对外 DTO ----

// SupplyOrderCardItem 是同步返回给闲管家的一张卡密（card_no/card_pwd）。
type SupplyOrderCardItem struct {
	CardNo  string `json:"card_no"`
	CardPwd string `json:"card_pwd"`
}

// SupplyOrder 是货源订单视图（查单/创建共用），字段严格对齐官方 schema
// （data additionalProperties:false，不得多返回字段）。
//
// 官方订单枚举无「退款态」：内部 30（已退款）对外归一为 20（成功）——退款事实
// 由 /goofish/order/refund/apply 承载（方案 §3 决策 4）。
//
// 金额 OrderAmount 单位为分（int64），= 商品单价(分) × 购买数量。
// 卡密 card_items：官方注释「无卡号或无法区分卡号/密码的，用 card_pwd 字段返回」，
// 故我方映射为 card_no=""、card_pwd=兑换码（redeem code 本身）；CardPwdResolver
// 提供时覆盖 card_pwd 取值（官方出处：创建卡密订单接口 data.card_items 注释）。
// EndTime 仅终态（成功/退款）返回：成功=下单时刻 order_time，退款=作废时刻。
type SupplyOrder struct {
	OrderNo     string                `json:"order_no"`     // 管家订单号
	OutOrderNo  string                `json:"out_order_no"` // 我方订单 id 十进制串
	OrderStatus int                   `json:"order_status"` // 10/20/30 官方枚举
	OrderAmount int64                 `json:"order_amount"` // 分
	GoodsName   string                `json:"goods_name"`
	BuyQuantity int                   `json:"buy_quantity"`
	OrderTime   int64                 `json:"order_time"` // created_at 秒
	EndTime     int64                 `json:"end_time"`   // 终态完成时刻秒
	CardItems   []SupplyOrderCardItem `json:"card_items"`
	Remark      string                `json:"remark,omitempty"`
}

// ---- 存储 DTO ----

// supplyOrderRow 是 xianguanjia_supply_orders 的一行（服务内部使用）。
//
// 金额快照三列（goods_name/unit_price/order_amount）由 D6E-02R #2 引入：下单时
// 与取卡同事务落库，之后查单/退款一律取快照，杜绝「按当前商品实时重建金额」导致的
// 历史订单金额漂移与退款 refund_amount=0 仍成功（外审依据）。
type supplyOrderRow struct {
	ID             int64
	ManagerOrderNo string
	GoodsNo        string
	Quantity       int
	Status         int
	CardNos        []string
	CreatedAt      time.Time
	RefundedAt     *time.Time
	GoodsName      string // 金额快照：下单时商品名（TEXT）
	UnitPrice      int64  // 金额快照：单价（分）
	OrderAmount    int64  // 金额快照：订单金额（分）= unit_price × quantity
}

// supplyOrderToView 把存储行转为对外视图。
func supplyOrderToView(row *supplyOrderRow, items []SupplyOrderCardItem, orderAmount int64, goodsName string) *SupplyOrder {
	if row == nil {
		return nil
	}
	out := &SupplyOrder{
		OrderNo:     row.ManagerOrderNo,
		OutOrderNo:  strconv.FormatInt(row.ID, 10),
		OrderStatus: supplyOrderStatusToOfficial(row.Status),
		OrderAmount: orderAmount,
		GoodsName:   goodsName,
		BuyQuantity: row.Quantity,
		OrderTime:   row.CreatedAt.Unix(),
		CardItems:   items,
	}
	// 终态补 end_time（成功=下单时刻；退款=作废时刻）。
	if row.Status == supplyOrderStatusRefunded && row.RefundedAt != nil {
		out.EndTime = row.RefundedAt.Unix()
	} else {
		out.EndTime = row.CreatedAt.Unix()
	}
	return out
}

// supplyOrderStatusToOfficial 把内部存储态归一为官方订单枚举。
// 内部 30（已退款）对外为 20（成功）：官方订单枚举无退款态，退款事实由
// refund/apply 承载（方案 §3 决策 4）。
func supplyOrderStatusToOfficial(internal int) int {
	if internal == supplyOrderStatusRefunded {
		return supplyOrderStatusSuccess
	}
	return internal
}

// ---- 现场生成卡密仓储（D6F-A：下单现场生成兑换码，废弃池取卡） ----

// SupplyCardGenerator 在订单同一 DB 事务内现场生成并插入兑换码（D6F-A：无限库存）。
//
// 生成器复用父包 service.GenerateRandomRedeemCode 生成码、service.PSResolveGroupValidityDays
// 换算有效期，保证「code 生成」与「validity 换算」均为父包单一实现，xianguanjia 仅作编排。
// 唯一约束冲突（码碰撞）由实现内部重试（每张上限 3 次）吸收，仍失败则报错。
type SupplyCardGenerator interface {
	// GenerateCardsTx 在给定事务内为 groupID 生成 quantity 张兑换码并插入
	// redeem_codes（type='subscription'、status='delivered'、group_id、validity_days、
	// notes=note），返回生成的码列表。任何失败（含无套餐分组 validity 解析失败）
	// 均返回 error，由调用方整体回滚事务。
	GenerateCardsTx(ctx context.Context, tx *sql.Tx, groupID int64, quantity int, note string) ([]string, error)
}

// SupplyCardPool 卡密作废仓储（D6F-A：取卡链归零，仅保留退款作废）。
//
// 接口化以便单测与 handler 解耦；DB 实现复用既有 redeem_codes 状态机语义。
type SupplyCardPool interface {
	// VoidCardsForOrder 作废某订单已发的卡：把命中卡 delivered/unused → expired，
	// 返回「本次实际由非 expired 转为 expired」的张数（幂等：已 expired 不计数）。
	VoidCardsForOrder(ctx context.Context, cardNos []string) (int, error)

	// VoidCardsForOrderTx 事务内作废（D6E-02R #3 退款原子化）：与 MarkRefundedTx
	// 同属一个 *sql.Tx，整体提交/回滚。内存实现直接改状态，回滚由 UndoVoidTx 补偿。
	VoidCardsForOrderTx(ctx context.Context, tx *sql.Tx, cardNos []string) (int, error)
	// UndoVoidTx 撤回本次 VoidCardsForOrderTx 的作废（内存实现把 expired 复位为
	// delivered），用于「作废成功但置态失败」时整体回滚，使卡保持 delivered。
	// DB 实现随事务回滚自动丢弃，返回 nil（no-op）。
	UndoVoidTx(ctx context.Context, tx *sql.Tx, cardNos []string) error
}

// ---- 订单仓储 ----

// SupplyOrderStore 货源订单仓储（生产实现基于 xianguanjia_supply_orders）。
//
// 幂等核心：InsertCreating 依赖 manager_order_no UNIQUE 约束；
// 冲突返回 ErrSupplyOrderDup，调用方回滚取卡并回读已存在订单。
type SupplyOrderStore interface {
	// InsertCreating 在事务内插入占位订单行（status=10 creating），并写入金额快照。
	// manager_order_no 已存在时返回 ErrSupplyOrderDup。
	InsertCreating(ctx context.Context, tx *sql.Tx, row supplyOrderPlanned) (int64, error)
	// GetByManagerOrderNo 按管家订单号读取订单行；不存在返回 (nil, nil)。
	GetByManagerOrderNo(ctx context.Context, managerOrderNo string) (*supplyOrderRow, error)
	// GetByID 按主键 id 读取订单行（out_order_no 查单）；不存在返回 (nil, nil)。
	GetByID(ctx context.Context, id int64) (*supplyOrderRow, error)
	// MarkRefunded 幂等地把订单置为退款态并写 refunded_at；返回是否本次变更。
	MarkRefunded(ctx context.Context, managerOrderNo string, at time.Time) (bool, error)

	// BeginTx 开启事务（DB 实现返回 *sql.Tx；内存实现返回 nil, nil，语义仍由实现保证）。
	BeginTx(ctx context.Context) (*sql.Tx, error)
	// CommitTx/RollbackTx 结束事务（内存实现为 no-op）。
	CommitTx(ctx context.Context, tx *sql.Tx) error
	RollbackTx(ctx context.Context, tx *sql.Tx) error
	// SetCardsAndStatusTx 事务内写入卡的映射与最终状态（创建成功时 status=20）。
	SetCardsAndStatusTx(ctx context.Context, tx *sql.Tx, managerOrderNo string, cardNos []string, status int) error

	// GetByManagerOrderNoTx 事务内（FOR UPDATE 行锁）按管家订单号读取订单行；
	// 用于退款原子化，串行化并发退款（D6E-02R #3）。内存实现退化为克隆。
	GetByManagerOrderNoTx(ctx context.Context, tx *sql.Tx, managerOrderNo string) (*supplyOrderRow, error)
	// MarkRefundedTx 事务内幂等置退款态并写 refunded_at；返回是否本次变更（D6E-02R #3）。
	MarkRefundedTx(ctx context.Context, tx *sql.Tx, managerOrderNo string, at time.Time) (bool, error)
}

// supplyOrderPlanned 是插入订单行的计划载荷（含金额快照，D6E-02R #2）。
type supplyOrderPlanned struct {
	ManagerOrderNo string
	GoodsNo        string
	Quantity       int
	CardNos        []string
	Status         int
	GoodsName      string // 金额快照：下单时商品名
	UnitPrice      int64  // 金额快照：单价（分）
	OrderAmount    int64  // 金额快照：订单金额（分）
}

// ErrSupplyOrderDup 是订单号已存在的内部哨兵错误（幂等冲突信号，非对外错误码）。
var ErrSupplyOrderDup = errors.New("xianguanjia supply order duplicated")

// ---- 服务 ----

// CardPwdResolver 解析/覆盖 card_items 的 card_pwd（D6d 联调校正点）。
//
// 官方合规默认映射：card_no="" 且 card_pwd=兑换码（redeem code 本身，见
// 创建卡密订单接口 data.card_items 注释「无卡号或无法区分卡号/密码的，用
// card_pwd 字段返回」）。本接口提供时「覆盖」默认映射：对解析命中的卡号返回
// 的 card_pwd 优先于默认兑换码；未命中或空串则退化为默认映射。pwdResolve 为
// nil 时一律走默认官方合规映射。
type CardPwdResolver interface {
	// ResolveCardPwd 批量解析 card_no → card_pwd；未命中 key 视为空串（即不覆盖）。
	ResolveCardPwd(ctx context.Context, cardNos []string) (map[string]string, error)
}

// SupplyOrderService 货源卡密订单服务（创建/查单/退款）。
type SupplyOrderService struct {
	store      SupplyOrderStore
	gen        SupplyCardGenerator
	pool       SupplyCardPool
	goods      SupplyGoodsSource
	pwdResolve CardPwdResolver
}

// NewSupplyOrderService 构造订单服务。store/gen/pool/goods 必填（fail-closed）；
// pwdResolve 可为 nil（则 card_pwd 一律默认官方合规映射）。
//
// goods 仅在 CreateOrder 未命中幂等回读时用于取商品单价（分）与可用性校验；
// 查单/退款一律读订单行金额快照（goods_name/unit_price/order_amount），不再回源
// 商品（D6E-02R #2），杜绝历史订单金额漂移与退款 refund_amount=0 仍成功。
func NewSupplyOrderService(store SupplyOrderStore, gen SupplyCardGenerator, pool SupplyCardPool, goods SupplyGoodsSource, pwdResolve CardPwdResolver) *SupplyOrderService {
	return &SupplyOrderService{store: store, gen: gen, pool: pool, goods: goods, pwdResolve: pwdResolve}
}

// CreateOrder 创建卡密订单：幂等现场生成兑换码并同步返回 card_items。
//
// 金额来源：经 goods.GetGoods 取商品单价（分），OrderAmount = Price × buy_quantity。
// 商品不存在 → 1100；商品不可用（status=2）→ 1101。
//
// max_amount（分，可选）：>0 且 OrderAmount > max_amount → 1202（下单金额低于成本价），
// 且此校验先于生成，不得发卡。0 表示不校验（官方未传时不校验）。
//
// 幂等（资金红线）：先按 manager_order_no 回读（步骤 0）；命中已有订单直接返回
// 原订单快照视图，完全不读商品源（即便商品下架/改价/更小 max_amount，重试也返回
// 原订单原卡，D6E-02R #1）。未命中则商品校验 → max_amount 校验（均未生成）→
// 单事务（BeginTx → InsertCreating → GenerateCardsTx → SetCardsAndStatusTx → Commit）
// 现场生成兑换码并落库；若并发唯一键冲突，回滚生成（卡随事务丢弃）→ 回读先到者的
// 订单，返回同一批卡（不重发）。生成失败/任何失败路径 → 整体回滚，无放回逻辑。
//
// 返回错误：商品不存在/商品不可用/金额超额 → *SupplyAPIError；参数非法 → 普通 error；
// 其它为内部错误（handler 归一为 1209 下单超时）。
func (s *SupplyOrderService) CreateOrder(ctx context.Context, managerOrderNo, goodsNo string, buyQuantity int, maxAmount int64) (*SupplyOrder, error) {
	if s == nil || s.store == nil || s.gen == nil || s.pool == nil {
		return nil, fmt.Errorf("xianguanjia supply order service unavailable")
	}
	managerOrderNo = strings.TrimSpace(managerOrderNo)
	goodsNo = strings.TrimSpace(goodsNo)
	if managerOrderNo == "" {
		return nil, fmt.Errorf("xianguanjia supply create order: manager_order_no is required")
	}
	if buyQuantity <= 0 {
		return nil, fmt.Errorf("xianguanjia supply create order: buy_quantity must be > 0")
	}

	// 0) 幂等前置回读（最高优先级，资金红线）：已有订单直接返回原订单快照视图，
	//    完全不读商品源——即便商品已下架/改价/更小 max_amount，重试也返回原订单原卡
	//    （D6E-02R #1）。未命中才进入下面的商品校验与生成。
	if existing, err := s.store.GetByManagerOrderNo(ctx, managerOrderNo); err != nil {
		return nil, fmt.Errorf("xianguanjia supply create order lookup: %w", err)
	} else if existing != nil {
		return s.viewExisting(ctx, existing)
	}

	// 1) 取商品（金额来源 + 可用性校验）：不存在→1100、下架→1101。
	goods, gerr := s.getGoodsOrFail(ctx, goodsNo)
	if gerr != nil {
		return nil, gerr
	}
	orderAmount := goods.Price * int64(buyQuantity)

	// 2) max_amount 校验（先于生成）：官方未传（maxAmount=0）不校验；
	//    传入且下单金额 > max_amount → 1202，且不得生成卡。
	if maxAmount > 0 && orderAmount > maxAmount {
		return nil, NewSupplyAPIError(SupplyCodeOrderAmountBelowCost, "下单金额低于成本价")
	}

	// 3) 单事务：BeginTx → InsertCreating → GenerateCardsTx → SetCardsAndStatusTx → Commit。
	//    goods_no 即分组 ID（D6c 映射 goods_no=分组ID 的十进制串）；解析失败视为参数错误。
	groupID, hasGroup := parseGoodsGroupID(goodsNo)
	if !hasGroup {
		return nil, fmt.Errorf("xianguanjia supply create order: goods_no %q is not a valid group id", goodsNo)
	}
	note := fmt.Sprintf("xianguanjia supply order %s", managerOrderNo)

	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("xianguanjia supply create order begin tx: %w", err)
	}
	// 占位订单行（status=10 creating），金额快照与生成卡同事务落库（D6E-02R #2）。
	id, err := s.store.InsertCreating(ctx, tx, supplyOrderPlanned{
		ManagerOrderNo: managerOrderNo,
		GoodsNo:        goodsNo,
		Quantity:       buyQuantity,
		CardNos:        nil,
		Status:         supplyOrderStatusCreating,
		GoodsName:      goods.GoodsName,
		UnitPrice:      goods.Price,
		OrderAmount:    orderAmount,
	})
	if err != nil {
		_ = s.store.RollbackTx(ctx, tx)
		if errors.Is(err, ErrSupplyOrderDup) {
			// 并发/重发：先到者已落库，回读并返回同一批卡（不重发）。
			if existing, gerr := s.store.GetByManagerOrderNo(ctx, managerOrderNo); gerr == nil && existing != nil {
				return s.viewExisting(ctx, existing)
			}
		}
		return nil, fmt.Errorf("xianguanjia supply create order insert: %w", err)
	}

	// 现场生成卡密（事务内插入 redeem_codes）；任何失败 → 回滚（生成卡随事务丢弃）。
	cardNos, err := s.gen.GenerateCardsTx(ctx, tx, groupID, buyQuantity, note)
	if err != nil {
		_ = s.store.RollbackTx(ctx, tx)
		return nil, fmt.Errorf("xianguanjia supply create order generate cards: %w", err)
	}

	if err := s.store.SetCardsAndStatusTx(ctx, tx, managerOrderNo, cardNos, supplyOrderStatusSuccess); err != nil {
		_ = s.store.RollbackTx(ctx, tx)
		return nil, fmt.Errorf("xianguanjia supply create order finalize: %w", err)
	}
	if err := s.store.CommitTx(ctx, tx); err != nil {
		_ = s.store.RollbackTx(ctx, tx)
		return nil, fmt.Errorf("xianguanjia supply create order commit: %w", err)
	}

	items, err := s.buildCardItems(ctx, cardNos)
	if err != nil {
		// 卡已发且订单已落库，但卡密解析（非关键字段）失败：不阻断发卡，
		// 下发空 card_pwd 并留痕（联调校正点）。
		slog.Warn("xianguanjia supply create order: resolve card pwd failed",
			"manager_order_no", managerOrderNo, "err", err)
		items = emptyCardItems(cardNos)
	}
	now := time.Now()
	slog.Info("xianguanjia supply order created",
		"manager_order_no", managerOrderNo, "goods_no", goodsNo, "cards", len(cardNos))
	return &SupplyOrder{
		OrderNo:     managerOrderNo,
		OutOrderNo:  strconv.FormatInt(id, 10),
		OrderStatus: supplyOrderStatusSuccess,
		OrderAmount: orderAmount,
		GoodsName:   goods.GoodsName,
		BuyQuantity: buyQuantity,
		OrderTime:   now.Unix(),
		EndTime:     now.Unix(),
		CardItems:   items,
	}, nil
}

// GetOrder 查询订单详情（幂等查询）：order_no 优先；order_no 为空时按 out_order_no
// （我方订单 id 十进制串）查单。订单不存在 → ErrSupplyOrderNotFound（code=1200）。
func (s *SupplyOrderService) GetOrder(ctx context.Context, orderNo, outOrderNo string) (*SupplyOrder, error) {
	if s == nil || s.store == nil {
		return nil, fmt.Errorf("xianguanjia supply order service unavailable")
	}
	orderNo = strings.TrimSpace(orderNo)
	if orderNo == "" {
		// order_no 为空时按 out_order_no（我方订单 id 十进制串）查单。
		outOrderNo = strings.TrimSpace(outOrderNo)
		if outOrderNo == "" {
			return nil, fmt.Errorf("xianguanjia supply get order: order_no or out_order_no is required")
		}
		id, err := strconv.ParseInt(outOrderNo, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("xianguanjia supply get order: invalid out_order_no %q", outOrderNo)
		}
		row, err := s.store.GetByID(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("xianguanjia supply get order by id: %w", err)
		}
		if row == nil {
			return nil, ErrSupplyOrderNotFound
		}
		return s.viewExisting(ctx, row)
	}
	row, err := s.store.GetByManagerOrderNo(ctx, orderNo)
	if err != nil {
		return nil, fmt.Errorf("xianguanjia supply get order: %w", err)
	}
	if row == nil {
		return nil, ErrSupplyOrderNotFound
	}
	return s.viewExisting(ctx, row)
}

// RefundNotify 处理退款通知：单事务原子化「作废卡 + 置退款态」（D6E-02R #3）。
//
// 状态转换判定：已退款幂等 agree；未退款仅全量作废（voided==卡数）agree，
// 零或部分作废（卡已用/已过期）均整体回滚 refuse：
//  1. 订单已退款 → 幂等 agree（refund_data 用快照金额 + refunded_at，不再作废）；
//  2. 未退款：事务内作废卡；voided != 卡数（零或部分）→ 回滚 + refuse；
//  3. 全量作废 → 同事务 MarkRefunded → 提交 → agree。
//     任何一步失败 → 整体回滚（卡保持 delivered、订单保持原态，重试可完整再来）。
//
// 并发语义：GetByManagerOrderNoTx 以 FOR UPDATE 行锁串行化；后到请求要么读到已退款
// （agree 幂等返回），要么参与同一行锁串行化，不得出现「读到旧状态 → 误 refuse/重复作废」。
//
// 订单不存在 → ErrSupplyOrderNotFound（code=1200，handler 归一）。
func (s *SupplyOrderService) RefundNotify(ctx context.Context, managerOrderNo string) (*SupplyOrder, bool, error) {
	if s == nil || s.store == nil || s.gen == nil || s.pool == nil {
		return nil, false, fmt.Errorf("xianguanjia supply order service unavailable")
	}
	managerOrderNo = strings.TrimSpace(managerOrderNo)
	if managerOrderNo == "" {
		return nil, false, fmt.Errorf("xianguanjia supply refund notify: manager_order_no is required")
	}

	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("xianguanjia supply refund begin tx: %w", err)
	}
	// 行锁读取：串行化并发退款，保证后到者读到一致状态。
	row, err := s.store.GetByManagerOrderNoTx(ctx, tx, managerOrderNo)
	if err != nil {
		_ = s.store.RollbackTx(ctx, tx)
		return nil, false, fmt.Errorf("xianguanjia supply refund notify lookup: %w", err)
	}
	if row == nil {
		_ = s.store.RollbackTx(ctx, tx)
		return nil, false, ErrSupplyOrderNotFound
	}

	// 1) 已退款：幂等 agree，refund_data 用快照金额 + refunded_at，不再作废。
	if row.Status == supplyOrderStatusRefunded {
		_ = s.store.RollbackTx(ctx, tx)
		order, verr := s.viewExisting(ctx, row)
		if verr != nil {
			return nil, false, verr
		}
		return order, true, nil
	}

	// 2) 事务内作废卡。
	voided, err := s.pool.VoidCardsForOrderTx(ctx, tx, row.CardNos)
	if err != nil {
		_ = s.store.RollbackTx(ctx, tx)
		return nil, false, fmt.Errorf("xianguanjia supply refund void cards for order %s: %w", managerOrderNo, err)
	}
	// 未整体作废（voided==0 或 1..n-1 部分卡已用/已过期）→ 整体回滚 + refuse：
	// 不得按全额 agree 置退款（部分已用卡保持有效，资金损失）。
	if voided != len(row.CardNos) {
		// 回滚本轮已作废的卡（仅本集合，见 SupplyCardPoolMemory.lastVoided 与 UndoVoidTx）。
		_ = s.pool.UndoVoidTx(ctx, tx, row.CardNos)
		_ = s.store.RollbackTx(ctx, tx)
		order, verr := s.viewExisting(ctx, row)
		if verr != nil {
			return nil, false, verr
		}
		return order, false, nil
	}

	// 3) 同事务置退款态；置态失败 → 撤回作废（卡复位 delivered）+ 整体回滚。
	// 置态前只取一次 now，同一值既传 MarkRefundedTx（持久化 refunded_at）又赋给
	// 本地 row.RefundedAt（首次 agree 的 refund_time），保证二者单源同值。
	now := time.Now()
	if _, err := s.store.MarkRefundedTx(ctx, tx, managerOrderNo, now); err != nil {
		_ = s.pool.UndoVoidTx(ctx, tx, row.CardNos)
		_ = s.store.RollbackTx(ctx, tx)
		return nil, false, fmt.Errorf("xianguanjia supply refund mark order %s: %w", managerOrderNo, err)
	}
	if err := s.store.CommitTx(ctx, tx); err != nil {
		_ = s.pool.UndoVoidTx(ctx, tx, row.CardNos)
		_ = s.store.RollbackTx(ctx, tx)
		return nil, false, fmt.Errorf("xianguanjia supply refund commit %s: %w", managerOrderNo, err)
	}

	// 本地 row 置退款态以便视图计算 end_time（refunded_at），复用上面同源 now。
	row.Status = supplyOrderStatusRefunded
	if row.RefundedAt == nil {
		row.RefundedAt = &now
	}
	slog.Info("xianguanjia supply order refund committed",
		"manager_order_no", managerOrderNo, "cards", len(row.CardNos), "voided", voided, "agree", true)

	order, err := s.viewExisting(ctx, row)
	if err != nil {
		return nil, false, err
	}
	return order, true, nil
}

// viewExisting 把存储行转为对外视图：金额/商品名一律取订单行快照（D6E-02R #2），
// 绝不读商品源（goods 源读取失败不再可能被吞，杜绝金额漂移）。
func (s *SupplyOrderService) viewExisting(ctx context.Context, row *supplyOrderRow) (*SupplyOrder, error) {
	items, err := s.buildCardItems(ctx, row.CardNos)
	if err != nil {
		slog.Warn("xianguanjia supply order: resolve card pwd failed",
			"manager_order_no", row.ManagerOrderNo, "err", err)
		items = emptyCardItems(row.CardNos)
	}
	return supplyOrderToView(row, items, row.OrderAmount, row.GoodsName), nil
}

// getGoodsOrFail 取商品并做可用性校验：不存在→1100、下架(status=2)→1101。
func (s *SupplyOrderService) getGoodsOrFail(ctx context.Context, goodsNo string) (*SupplyGoods, error) {
	if s == nil || s.goods == nil {
		return nil, fmt.Errorf("xianguanjia supply goods source unavailable")
	}
	g, err := s.goods.GetGoods(ctx, goodsNo)
	if err != nil {
		return nil, fmt.Errorf("xianguanjia supply get goods %s: %w", goodsNo, err)
	}
	if g == nil {
		return nil, ErrSupplyGoodsNotFound
	}
	if g.Status == SupplyGoodsStatusOffSale {
		return nil, NewSupplyAPIError(SupplyCodeGoodsUnavailable, "商品不可用")
	}
	return g, nil
}

// buildCardItems 组装 card_items（card_no + card_pwd）。
//
// 官方合规默认映射：card_no=""、card_pwd=兑换码（redeem code 本身）。
// CardPwdResolver 提供时覆盖 card_pwd 取值（命中非空才覆盖，否则退化为默认映射）。
func (s *SupplyOrderService) buildCardItems(ctx context.Context, cardNos []string) ([]SupplyOrderCardItem, error) {
	pwds := map[string]string{}
	if s.pwdResolve != nil && len(cardNos) > 0 {
		m, err := s.pwdResolve.ResolveCardPwd(ctx, cardNos)
		if err != nil {
			return nil, err
		}
		pwds = m
	}
	items := make([]SupplyOrderCardItem, 0, len(cardNos))
	for _, no := range cardNos {
		// 默认 card_pwd = 兑换码（redeem code 本身）；resolver 命中非空则覆盖。
		pwd := no
		if v, ok := pwds[no]; ok && v != "" {
			pwd = v
		}
		items = append(items, SupplyOrderCardItem{CardNo: "", CardPwd: pwd})
	}
	return items, nil
}

// emptyCardItems 把卡号列表映射为官方合规默认 card_items（card_no 留空、
// card_pwd 承载兑换码）。
func emptyCardItems(cardNos []string) []SupplyOrderCardItem {
	items := make([]SupplyOrderCardItem, 0, len(cardNos))
	for _, no := range cardNos {
		// 默认官方合规映射：card_no 留空，card_pwd 承载兑换码。
		items = append(items, SupplyOrderCardItem{CardNo: "", CardPwd: no})
	}
	return items
}

// ---- DB 实现：订单仓储（xianguanjia_supply_orders） ----

type supplyOrderStoreDB struct {
	db *sql.DB
}

// NewSupplyOrderStore 返回基于 PostgreSQL 的货源订单仓储。
func NewSupplyOrderStore(db *sql.DB) *supplyOrderStoreDB {
	return &supplyOrderStoreDB{db: db}
}

func (s *supplyOrderStoreDB) BeginTx(ctx context.Context) (*sql.Tx, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("xianguanjia supply order store unavailable")
	}
	return s.db.BeginTx(ctx, nil)
}

func (s *supplyOrderStoreDB) CommitTx(ctx context.Context, tx *sql.Tx) error {
	if tx == nil {
		return nil
	}
	return tx.Commit()
}

func (s *supplyOrderStoreDB) RollbackTx(ctx context.Context, tx *sql.Tx) error {
	if tx == nil {
		return nil
	}
	return tx.Rollback()
}

func (s *supplyOrderStoreDB) InsertCreating(ctx context.Context, tx *sql.Tx, row supplyOrderPlanned) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("xianguanjia supply order store unavailable")
	}
	var id int64
	err := execQueryRow(ctx, tx, s.db, `
		INSERT INTO xianguanjia_supply_orders
			(manager_order_no, goods_no, quantity, status, card_nos, goods_name, unit_price, order_amount)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id`,
		row.ManagerOrderNo, row.GoodsNo, row.Quantity, row.Status, encodeCardNos(row.CardNos),
		row.GoodsName, row.UnitPrice, row.OrderAmount).Scan(&id)
	if err != nil {
		if supplyIsUniqueViolation(err) {
			return 0, ErrSupplyOrderDup
		}
		return 0, err
	}
	return id, nil
}

func (s *supplyOrderStoreDB) SetCardsAndStatusTx(ctx context.Context, tx *sql.Tx, managerOrderNo string, cardNos []string, status int) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("xianguanjia supply order store unavailable")
	}
	_, err := execNonQuery(ctx, tx, s.db, `
		UPDATE xianguanjia_supply_orders
		SET card_nos = $1, status = $2
		WHERE manager_order_no = $3`,
		encodeCardNos(cardNos), status, managerOrderNo)
	return err
}

func (s *supplyOrderStoreDB) GetByManagerOrderNo(ctx context.Context, managerOrderNo string) (*supplyOrderRow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("xianguanjia supply order store unavailable")
	}
	return scanSupplyOrder(s.db.QueryRowContext(ctx, supplyOrderSelectSQL, managerOrderNo))
}

func (s *supplyOrderStoreDB) GetByManagerOrderNoTx(ctx context.Context, tx *sql.Tx, managerOrderNo string) (*supplyOrderRow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("xianguanjia supply order store unavailable")
	}
	return scanSupplyOrder(execQueryRow(ctx, tx, s.db, supplyOrderSelectForUpdateSQL, managerOrderNo))
}

func (s *supplyOrderStoreDB) GetByID(ctx context.Context, id int64) (*supplyOrderRow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("xianguanjia supply order store unavailable")
	}
	return scanSupplyOrder(s.db.QueryRowContext(ctx, supplyOrderSelectByIDSQL, id))
}

func (s *supplyOrderStoreDB) MarkRefunded(ctx context.Context, managerOrderNo string, at time.Time) (bool, error) {
	return s.MarkRefundedTx(ctx, nil, managerOrderNo, at)
}

func (s *supplyOrderStoreDB) MarkRefundedTx(ctx context.Context, tx *sql.Tx, managerOrderNo string, at time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("xianguanjia supply order store unavailable")
	}
	res, err := execNonQuery(ctx, tx, s.db, `
		UPDATE xianguanjia_supply_orders
		SET status = $1, refunded_at = COALESCE(refunded_at, $2)
		WHERE manager_order_no = $3 AND status <> $1`,
		supplyOrderStatusRefunded, at, managerOrderNo)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

const supplyOrderSelectSQL = `
	SELECT id, manager_order_no, goods_no, quantity, status, card_nos, created_at, refunded_at, goods_name, unit_price, order_amount
	FROM xianguanjia_supply_orders WHERE manager_order_no = $1`

const supplyOrderSelectForUpdateSQL = `
	SELECT id, manager_order_no, goods_no, quantity, status, card_nos, created_at, refunded_at, goods_name, unit_price, order_amount
	FROM xianguanjia_supply_orders WHERE manager_order_no = $1 FOR UPDATE`

const supplyOrderSelectByIDSQL = `
	SELECT id, manager_order_no, goods_no, quantity, status, card_nos, created_at, refunded_at, goods_name, unit_price, order_amount
	FROM xianguanjia_supply_orders WHERE id = $1`

func scanSupplyOrder(row *sql.Row) (*supplyOrderRow, error) {
	var r supplyOrderRow
	var cardNosRaw []byte
	var refunded sql.NullTime
	if err := row.Scan(&r.ID, &r.ManagerOrderNo, &r.GoodsNo, &r.Quantity, &r.Status,
		&cardNosRaw, &r.CreatedAt, &refunded, &r.GoodsName, &r.UnitPrice, &r.OrderAmount); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	r.CardNos = decodeCardNos(cardNosRaw)
	if refunded.Valid {
		t := refunded.Time
		r.RefundedAt = &t
	}
	return &r, nil
}

func execQueryRow(ctx context.Context, tx *sql.Tx, db *sql.DB, q string, args ...any) *sql.Row {
	if tx != nil {
		return tx.QueryRowContext(ctx, q, args...)
	}
	return db.QueryRowContext(ctx, q, args...)
}

func execNonQuery(ctx context.Context, tx *sql.Tx, db *sql.DB, q string, args ...any) (sql.Result, error) {
	if tx != nil {
		return tx.ExecContext(ctx, q, args...)
	}
	return db.ExecContext(ctx, q, args...)
}

// supplyIsUniqueViolation 判定 Postgres 唯一约束冲突（SQLSTATE 23505）。
// 包内自带实现，避免跨包耦合既有 repository 的同名函数。
func supplyIsUniqueViolation(err error) bool {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return pqErr.Code == "23505"
	}
	return false
}

func encodeCardNos(cardNos []string) string {
	if cardNos == nil {
		cardNos = []string{}
	}
	b, err := json.Marshal(cardNos)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func decodeCardNos(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// ---- DB 实现：卡密池取卡/作废（redeem_codes） ----

type supplyCardPoolDB struct {
	db *sql.DB
}

// NewSupplyCardPool 返回基于 PostgreSQL 的卡密作废仓储（D6F-A：仅退款作废，取卡链归零）。
func NewSupplyCardPool(db *sql.DB) *supplyCardPoolDB {
	return &supplyCardPoolDB{db: db}
}

// ---- 现场生成卡密仓储：DB 实现（D6F-A） ----

type supplyCardGeneratorDB struct {
	db *sql.DB
}

// NewSupplyCardGenerator 返回基于 PostgreSQL 的现场生成卡密仓储。
func NewSupplyCardGenerator(db *sql.DB) *supplyCardGeneratorDB {
	return &supplyCardGeneratorDB{db: db}
}

// GenerateCardsTx 在事务内为 groupID 现场生成 quantity 张订阅兑换码并插入 redeem_codes。
//
// 字段：code（父包 service.GenerateRandomRedeemCode 单一实现）、type='subscription'、
// status='delivered'、group_id、validity_days（父包 service.PSResolveGroupValidityDays 单一
// 换算，无套餐分组→fail-closed 错误）、notes=note、value=0。其余列取 schema 默认值。
// 唯一约束冲突（码碰撞）重试生成（每张上限 3 次），仍失败则整体报错由调用方回滚。
func (g *supplyCardGeneratorDB) GenerateCardsTx(ctx context.Context, tx *sql.Tx, groupID int64, quantity int, note string) ([]string, error) {
	if g == nil || g.db == nil {
		return nil, fmt.Errorf("xianguanjia supply card generator unavailable")
	}
	if quantity <= 0 {
		return nil, nil
	}
	// 经 tx 解析有效期，使套餐读取与 redeem_codes 插入共享同一事务快照（D6F-R 修复#1）。
	validityDays, err := service.PSResolveGroupValidityDaysTx(ctx, tx, groupID)
	if err != nil {
		return nil, err // fail-closed：无套餐分组等
	}
	codes := make([]string, 0, quantity)
	for i := 0; i < quantity; i++ {
		var lastErr error
		inserted := false
		for attempt := 0; attempt < 3; attempt++ {
			code, cerr := service.GenerateRandomRedeemCode()
			if cerr != nil {
				return nil, fmt.Errorf("xianguanjia generate redeem code: %w", cerr)
			}
			_, ierr := execNonQuery(ctx, tx, g.db, `
				INSERT INTO redeem_codes (code, type, status, group_id, validity_days, notes, value)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				code, domain.RedeemTypeSubscription, domain.StatusDelivered, groupID, validityDays, note, 0)
			if ierr != nil {
				if supplyIsUniqueViolation(ierr) {
					lastErr = ierr
					continue // 码碰撞：换码重试
				}
				return nil, fmt.Errorf("xianguanjia insert generated card: %w", ierr)
			}
			codes = append(codes, code)
			inserted = true
			break
		}
		if !inserted {
			return nil, fmt.Errorf("xianguanjia supply generate card: uniqueness retry exhausted after 3 attempts: %w", lastErr)
		}
	}
	return codes, nil
}

// VoidCardsForOrder 作废某订单已发的卡：delivered/unused → expired。
// 复用 D2 语义（CodeCardVoidRepository.VoidCodesByCardNos 同样只处理
// delivered/unused，不动 used/expired）。返回本次实际作废张数。
func (p *supplyCardPoolDB) VoidCardsForOrder(ctx context.Context, cardNos []string) (int, error) {
	return p.VoidCardsForOrderTx(ctx, nil, cardNos)
}

// VoidCardsForOrderTx 事务内作废（D6E-02R #3）：与订单置态同属一个 *sql.Tx，
// 整体提交/回滚。复用 D2 语义，返回本次实际作废张数。
func (p *supplyCardPoolDB) VoidCardsForOrderTx(ctx context.Context, tx *sql.Tx, cardNos []string) (int, error) {
	if p == nil || p.db == nil {
		return 0, fmt.Errorf("xianguanjia supply card pool unavailable")
	}
	total := 0
	for _, no := range cardNos {
		res, err := execNonQuery(ctx, tx, p.db, `
			UPDATE redeem_codes SET status = 'expired'
			WHERE code = $1 AND status IN ('delivered', 'unused')`, no)
		if err != nil {
			return total, fmt.Errorf("xianguanjia void card for order: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			total += int(n)
		}
	}
	return total, nil
}

// UndoVoidTx 撤回本次 VoidCardsForOrderTx 的作废。DB 实现随事务回滚自动丢弃，
// 此处为 no-op（D6E-02R #3 退款原子化回滚）。
func (p *supplyCardPoolDB) UndoVoidTx(ctx context.Context, tx *sql.Tx, cardNos []string) error {
	return nil
}

// parseGoodsGroupID 把 goods_no 解析为分组 ID（D6c 映射 goods_no=分组ID 的十进制串）。
// 解析失败返回 hasGroup=false，取卡退化为不按分组过滤。
func parseGoodsGroupID(goodsNo string) (int64, bool) {
	goodsNo = strings.TrimSpace(goodsNo)
	if goodsNo == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(goodsNo, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// ---- 内存实现（单测 / handler 测试用，含并发仲裁） ----

// supplyOrderStoreMem 内存订单仓储：以 manager_order_no 唯一性模拟 DB UNIQUE 约束。
// 进程内用互斥锁保证并发同订单号仅一次成功插入。
type supplyOrderStoreMem struct {
	mu     sync.Mutex
	rows   map[string]*supplyOrderRow
	nextID int64
	// pending 是 BeginTx→CommitTx 之间的事务缓冲：按事务句柄（*sql.Tx 哨兵）隔离，
	// InsertCreating/SetCardsAndStatusTx 写入 pending[tx]，CommitTx 落盘、RollbackTx
	// 仅丢弃 pending[tx]（即本事务自己的缓冲），从而忠实模拟 DB「每事务独立、回滚
	// 仅影响自身」的原子性。内存实现 tx 为各自独立的 &sql.Tx{} 哨兵，天然作为 map key。
	pending map[*sql.Tx]*supplyOrderRow

	// commitHook/rollbackHook 是事务生命周期钩子（仅包内测试可设，不改构造函数签名，
	// D6F-R 修复#2）：CommitTx 在落盘前调 commitHook（非 nil 时），返回 error → 本事务
	// 落盘中止、pending 缓冲按回滚处理并透传错误（模拟 DB 提交失败）；RollbackTx 调
	// rollbackHook（非 nil 时，错误忽略仅继续回滚）。生产 DB 实现无此钩子，纯内存替身用。
	commitHook   func(tx *sql.Tx) error
	rollbackHook func(tx *sql.Tx) error
}

// NewSupplyOrderStoreMemory 返回内存版订单仓储。
func NewSupplyOrderStoreMemory() *supplyOrderStoreMem {
	return &supplyOrderStoreMem{
		rows:    make(map[string]*supplyOrderRow),
		pending: make(map[*sql.Tx]*supplyOrderRow),
	}
}

// WireCardGeneratorLifecycle 把内存生成器接进本 store 的事务生命周期：
// CommitTx 落盘前调 gen.CommitGenerated（错误则本事务落盘中止并透传），
// RollbackTx 时调 gen.RollbackGenerated 丢弃生成缓冲。gen 需提供
// CommitGenerated(*sql.Tx) error 与 RollbackGenerated(*sql.Tx) error。
func (s *supplyOrderStoreMem) WireCardGeneratorLifecycle(gen interface {
	CommitGenerated(tx *sql.Tx) error
	RollbackGenerated(tx *sql.Tx) error
}) {
	s.commitHook = gen.CommitGenerated
	s.rollbackHook = gen.RollbackGenerated
}

// BeginTx 内存实现：返回一个独立的 &sql.Tx{} 哨兵作为本事务句柄（仅作 pending map 的 key，
// 不被解引用），从而在内存中隔离各并发调用的事务缓冲，等价于 DB 的独立事务。
func (s *supplyOrderStoreMem) BeginTx(ctx context.Context) (*sql.Tx, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &sql.Tx{}, nil
}
func (s *supplyOrderStoreMem) CommitTx(ctx context.Context, tx *sql.Tx) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 落盘前调 commitHook（如生成器提交其 pending 缓冲到已生成集合与卡池）。
	// 返回 error → 本事务落盘中止、pending 缓冲按回滚处理（不落盘）并透传错误，
	// 模拟 DB 提交失败：订单不落盘、生成卡按回滚丢弃（D6F-R 修复#2）。
	if s.commitHook != nil {
		if err := s.commitHook(tx); err != nil {
			delete(s.pending, tx)
			return err
		}
	}
	if p, ok := s.pending[tx]; ok {
		s.rows[p.ManagerOrderNo] = p
		delete(s.pending, tx)
	}
	return nil
}
func (s *supplyOrderStoreMem) RollbackTx(ctx context.Context, tx *sql.Tx) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 回滚钩子（如生成器丢弃其 pending 缓冲）。错误忽略，仅继续回滚（D6F-R 修复#2）。
	if s.rollbackHook != nil {
		_ = s.rollbackHook(tx)
	}
	// 仅丢弃本事务自己的 pending：并发下失败方的回滚不会误清获胜方尚未提交的缓冲，
	// 保证「先到者发卡」语义（DB 中两事务各自独立，无此问题）。
	delete(s.pending, tx)
	return nil
}

func (s *supplyOrderStoreMem) InsertCreating(ctx context.Context, tx *sql.Tx, row supplyOrderPlanned) (int64, error) {
	if s == nil {
		return 0, fmt.Errorf("xianguanjia supply order store unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rows[row.ManagerOrderNo]; ok {
		return 0, ErrSupplyOrderDup
	}
	// 同一 manager_order_no 已有在途事务（pending）也判为并发 dup（幂等仲裁）：
	// 后到者即使尚未提交，也不得再插入，从而把生成卡密的机会收口给先到事务。
	for _, p := range s.pending {
		if p.ManagerOrderNo == row.ManagerOrderNo {
			return 0, ErrSupplyOrderDup
		}
	}
	s.nextID++
	r := &supplyOrderRow{
		ID:             s.nextID,
		ManagerOrderNo: row.ManagerOrderNo,
		GoodsNo:        row.GoodsNo,
		Quantity:       row.Quantity,
		Status:         row.Status,
		CardNos:        append([]string(nil), row.CardNos...),
		CreatedAt:      time.Now(),
		GoodsName:      row.GoodsName,
		UnitPrice:      row.UnitPrice,
		OrderAmount:    row.OrderAmount,
	}
	// 写入本事务 pending 缓冲（CommitTx 才落盘），模拟事务未提交前对并发读者不可见。
	s.pending[tx] = r
	return r.ID, nil
}

func (s *supplyOrderStoreMem) SetCardsAndStatusTx(ctx context.Context, tx *sql.Tx, managerOrderNo string, cardNos []string, status int) error {
	if s == nil {
		return fmt.Errorf("xianguanjia supply order store unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// 写本事务 pending 缓冲（创建流程）；否则直接写已提交行（退款置态复用此接口）。
	if p, ok := s.pending[tx]; ok {
		p.CardNos = append([]string(nil), cardNos...)
		p.Status = status
		return nil
	}
	r, ok := s.rows[managerOrderNo]
	if !ok {
		return fmt.Errorf("xianguanjia supply order %s not found", managerOrderNo)
	}
	r.CardNos = append([]string(nil), cardNos...)
	r.Status = status
	return nil
}

func (s *supplyOrderStoreMem) GetByManagerOrderNo(ctx context.Context, managerOrderNo string) (*supplyOrderRow, error) {
	if s == nil {
		return nil, fmt.Errorf("xianguanjia supply order store unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneSupplyOrderRow(s.rows[managerOrderNo]), nil
}

// GetByManagerOrderNoTx 内存实现：退化为克隆（无真实行锁；并发仲裁由单测覆盖路径保证）。
func (s *supplyOrderStoreMem) GetByManagerOrderNoTx(ctx context.Context, tx *sql.Tx, managerOrderNo string) (*supplyOrderRow, error) {
	return s.GetByManagerOrderNo(ctx, managerOrderNo)
}

func (s *supplyOrderStoreMem) GetByID(ctx context.Context, id int64) (*supplyOrderRow, error) {
	if s == nil {
		return nil, fmt.Errorf("xianguanjia supply order store unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rows {
		if r.ID == id {
			return cloneSupplyOrderRow(r), nil
		}
	}
	return nil, nil
}

func (s *supplyOrderStoreMem) MarkRefunded(ctx context.Context, managerOrderNo string, at time.Time) (bool, error) {
	return s.MarkRefundedTx(ctx, nil, managerOrderNo, at)
}

func (s *supplyOrderStoreMem) MarkRefundedTx(ctx context.Context, tx *sql.Tx, managerOrderNo string, at time.Time) (bool, error) {
	if s == nil {
		return false, fmt.Errorf("xianguanjia supply order store unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[managerOrderNo]
	if !ok {
		return false, nil
	}
	changed := r.Status != supplyOrderStatusRefunded
	r.Status = supplyOrderStatusRefunded
	if r.RefundedAt == nil {
		t := at
		r.RefundedAt = &t
	}
	return changed, nil
}

func cloneSupplyOrderRow(r *supplyOrderRow) *supplyOrderRow {
	if r == nil {
		return nil
	}
	out := *r
	out.CardNos = append([]string(nil), r.CardNos...)
	if r.RefundedAt != nil {
		t := *r.RefundedAt
		out.RefundedAt = &t
	}
	return &out
}

// SupplyCardPoolMemory 内存卡密池：模拟 redeem_codes 的 unused/delivered/expired 状态机。
// cards: card_no -> 状态。互斥锁保证并发取卡不重发。
type SupplyCardPoolMemory struct {
	mu    sync.Mutex
	cards map[string]string // card_no -> status
	pwds  map[string]string // card_no -> card_pwd
	// lastVoided 记录上次 VoidCardsForOrderTx 实际作废（delivered/unused → expired）的卡集合。
	// 仅内存替身单测试场景用，供 UndoVoidTx 精确回滚：只复位本集合，避免误复活本轮之前
	// 已 expired 的卡（偏离 DB 事务回滚语义）。DB 实现随事务回滚自动丢弃，无需此字段。
	lastVoided []string
}

// NewSupplyCardPoolMemory 返回内存版卡密池。cardNos 全部初始化为 unused；
// cardPwds 可为 nil（card_pwd 空串）。
func NewSupplyCardPoolMemory(cardNos []string, cardPwds map[string]string) *SupplyCardPoolMemory {
	cards := make(map[string]string, len(cardNos))
	for _, no := range cardNos {
		cards[no] = "unused"
	}
	pwds := make(map[string]string, len(cardPwds))
	for k, v := range cardPwds {
		pwds[k] = v
	}
	return &SupplyCardPoolMemory{cards: cards, pwds: pwds}
}

// StatusOf 返回内存池中某卡状态（测试断言用）。
func (p *SupplyCardPoolMemory) StatusOf(cardNo string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cards[cardNo]
}

// CountByStatus 统计内存池中某状态的卡数（测试断言用）。
func (p *SupplyCardPoolMemory) CountByStatus(status string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, st := range p.cards {
		if st == status {
			n++
		}
	}
	return n
}

// SetCardStatusForTest 直接置某卡状态（仅供单测模拟「已使用/已过期」场景，
// 以触发退款 refuse 分支；生产代码不得调用）。
func (p *SupplyCardPoolMemory) SetCardStatusForTest(cardNo, status string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.cards[cardNo]; ok {
		p.cards[cardNo] = status
	}
}

func (p *SupplyCardPoolMemory) VoidCardsForOrder(ctx context.Context, cardNos []string) (int, error) {
	return p.VoidCardsForOrderTx(ctx, nil, cardNos)
}

// VoidCardsForOrderTx 内存实现：与 DB 语义一致（tx 为 nil，无真实事务）。
// 记录本次实际作废集合到 lastVoided，供 UndoVoidTx 精确回滚（仅复位本集合）。
func (p *SupplyCardPoolMemory) VoidCardsForOrderTx(ctx context.Context, tx *sql.Tx, cardNos []string) (int, error) {
	if p == nil {
		return 0, fmt.Errorf("xianguanjia supply card pool unavailable")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	p.lastVoided = p.lastVoided[:0]
	for _, no := range cardNos {
		switch p.cards[no] {
		case "delivered", "unused":
			p.cards[no] = "expired"
			n++
			p.lastVoided = append(p.lastVoided, no)
		}
	}
	return n, nil
}

// UndoVoidTx 撤回本次 VoidCardsForOrderTx 的作废（内存：expired → delivered），
// 用于「作废成功但置态失败」时整体回滚，使卡保持 delivered（D6E-02R #3）。
// 仅复位 lastVoided（本轮实际作废集合），不再遍历 cardNos 全量恢复，避免误复活
// 本轮之前已 expired 的卡（差于 DB 事务回滚语义）。DB 实现随事务回滚自动丢弃为 no-op。
func (p *SupplyCardPoolMemory) UndoVoidTx(ctx context.Context, tx *sql.Tx, cardNos []string) error {
	if p == nil {
		return fmt.Errorf("xianguanjia supply card pool unavailable")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, no := range p.lastVoided {
		if p.cards[no] == "expired" {
			p.cards[no] = "delivered"
		}
	}
	return nil
}

// ---- 现场生成卡密仓储：内存实现（单测用，含字段记录便于断言） ----

// generatedCardRecord 是内存生成器成功生成的单张卡记录（用于测试断言落库字段）。
type generatedCardRecord struct {
	Code         string
	Type         string
	Status       string
	GroupID      int64
	ValidityDays int
	Note         string
}

// supplyCardGeneratorMem 内存生成器：复用父包 service.GenerateRandomRedeemCode 生成码，
// validity_days 由注入 resolver 提供（测试可模拟无套餐 fail-closed）；遵守外层事务生命周期
// （D6F-R 修复#2）：GenerateCardsTx 只把记录暂存 pending[tx]，不在本方法内直接落库
// g.generated 与 pool.cards；待调用方事务提交（CommitTx 触发 commitHook → CommitGenerated）
// 时才落库，事务回滚（rollbackHook → RollbackGenerated）时丢弃，从而与 DB 实现的事务语义一致。
type supplyCardGeneratorMem struct {
	mu       sync.Mutex
	pool     *SupplyCardPoolMemory
	resolver func(groupID int64) (int, error)
	// generated 记录截至当前已「提交」的卡（仅 CommitGenerated 写入）；GeneratedCards()
	// 只反映已提交卡，未提交/已回滚的 pending 不计入（模拟事务未提交不可见）。
	generated []generatedCardRecord
	// pending 是 GenerateCardsTx 的事务缓冲：按事务句柄 *sql.Tx 隔离。GenerateCardsTx
	// 只写 pending[tx]；CommitGenerated 落库并删除该键，RollbackGenerated 仅删除该键。
	pending map[*sql.Tx][]generatedCardRecord
	// failGen 注入：生成第一张后整体失败（测试「生成失败整体回滚」）。
	failGen bool
}

// NewSupplyCardGeneratorMemory 返回内存版现场生成器。resolver 解析分组有效天数，
// 返回 error 即模拟「无套餐分组」fail-closed。pool 可为 nil（仅测试字段断言时）。
func NewSupplyCardGeneratorMemory(pool *SupplyCardPoolMemory, resolver func(groupID int64) (int, error)) *supplyCardGeneratorMem {
	return &supplyCardGeneratorMem{pool: pool, resolver: resolver, pending: make(map[*sql.Tx][]generatedCardRecord)}
}

// GenerateCardsTx 内存实现：生成码并暂存 pending[tx]（不落库、不碰 pool.cards），
// 全部成功才返回码列表；任一失败（含无套餐 resolver 失败）整体报错且不落库，由调用方
// 回滚丢弃 pending（模拟事务回滚）。落库动作统一收口到 CommitGenerated（D6F-R 修复#2）。
func (g *supplyCardGeneratorMem) GenerateCardsTx(ctx context.Context, tx *sql.Tx, groupID int64, quantity int, note string) ([]string, error) {
	if g == nil {
		return nil, fmt.Errorf("xianguanjia supply card generator unavailable")
	}
	if quantity <= 0 {
		return nil, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	validityDays, err := g.resolver(groupID)
	if err != nil {
		return nil, err // 无套餐分组 → fail-closed
	}
	codes := make([]string, 0, quantity)
	pending := make([]generatedCardRecord, 0, quantity)
	for i := 0; i < quantity; i++ {
		code, cerr := service.GenerateRandomRedeemCode()
		if cerr != nil {
			return nil, fmt.Errorf("xianguanjia generate redeem code: %w", cerr)
		}
		rec := generatedCardRecord{
			Code:         code,
			Type:         domain.RedeemTypeSubscription,
			Status:       domain.StatusDelivered,
			GroupID:      groupID,
			ValidityDays: validityDays,
			Note:         note,
		}
		pending = append(pending, rec)
		codes = append(codes, code)
		if g.failGen {
			return nil, errors.New("injected generate failure")
		}
	}
	// 暂存 pending[tx]：不在此处落库，交由 CommitGenerated（事务提交）落库（D6F-R 修复#2）。
	g.pending[tx] = append(g.pending[tx], pending...)
	return codes, nil
}

// CommitGenerated 在事务提交时落库：把 pending[tx] 追加进 g.generated 并逐条写入
// pool.cards（status 取记录值），随后删除该 tx 键。对未知 tx 键幂等 no-op（D6F-R 修复#2）。
func (g *supplyCardGeneratorMem) CommitGenerated(tx *sql.Tx) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	pending, ok := g.pending[tx]
	if !ok {
		return nil // 幂等 no-op：未知 tx 键
	}
	g.generated = append(g.generated, pending...)
	for _, r := range pending {
		if g.pool != nil {
			g.pool.mu.Lock()
			g.pool.cards[r.Code] = r.Status
			g.pool.mu.Unlock()
		}
	}
	delete(g.pending, tx)
	return nil
}

// RollbackGenerated 在事务回滚时丢弃 pending[tx]，不落库。对未知 tx 键幂等 no-op
// （D6F-R 修复#2）。
func (g *supplyCardGeneratorMem) RollbackGenerated(tx *sql.Tx) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.pending, tx) // 幂等 no-op：未知 tx 键
	return nil
}

// GeneratedCards 返回截至当前已成功生成的卡记录快照（测试断言落库字段用）。
func (g *supplyCardGeneratorMem) GeneratedCards() []generatedCardRecord {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]generatedCardRecord, len(g.generated))
	copy(out, g.generated)
	return out
}

// supplyCardPwdResolverMem 基于内存池解析 card_pwd（测试用）。
type supplyCardPwdResolverMem struct {
	pool *SupplyCardPoolMemory
}

// NewSupplyCardPwdResolverMemory 返回内存版 card_pwd 解析器。
func NewSupplyCardPwdResolverMemory(pool *SupplyCardPoolMemory) *supplyCardPwdResolverMem {
	return &supplyCardPwdResolverMem{pool: pool}
}

func (r *supplyCardPwdResolverMem) ResolveCardPwd(ctx context.Context, cardNos []string) (map[string]string, error) {
	out := make(map[string]string, len(cardNos))
	if r == nil || r.pool == nil {
		return out, nil
	}
	r.pool.mu.Lock()
	defer r.pool.mu.Unlock()
	for _, no := range cardNos {
		out[no] = r.pool.pwds[no]
	}
	return out, nil
}

// ensure CardPwdResolver 的 DB 兑现说明：
// 生产环境如需真实 card_pwd，集成者在 D6e 接线时提供 CardPwdResolver 实现
// （当前 redeem_codes 无独立卡密列）；默认 nil 时 card_pwd 为兑换码（联调校正点）。
