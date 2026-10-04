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

// ErrSupplyStockInsufficient 是「库存不足」的哨兵错误（code=1102）。
var ErrSupplyStockInsufficient = NewSupplyAPIError(SupplyCodeStockInsufficient, "库存不足")

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

// ---- 卡密池取卡仓储 ----

// SupplyPoolCard 是取自卡密池的一张卡（card_no=redeem_codes.code，card_pwd 由池内填充）。
type SupplyPoolCard struct {
	CardNo  string
	CardPwd string
}

// SupplyCardPool 卡密池取卡/作废仓储（D6d 生产实现基于 redeem_codes）。
//
// 接口化以便单测与 handler 解耦；DB 实现复用既有 redeem_codes 状态机语义。
type SupplyCardPool interface {
	// ClaimCardsForOrder 原子取卡：从池中取 quantity 张 unused 卡并标记为
	// delivered（渠道=闲管家），返回实际取到的卡（少于 quantity 即不足）。
	// 语义对应 `UPDATE redeem_codes SET status='delivered' WHERE status='unused'
	// AND ... LIMIT n RETURNING code`，单事务内完成，防并发双发。
	ClaimCardsForOrder(ctx context.Context, goodsNo string, quantity int) ([]SupplyPoolCard, error)

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
	pool       SupplyCardPool
	goods      SupplyGoodsSource
	pwdResolve CardPwdResolver
}

// NewSupplyOrderService 构造订单服务。store/pool/goods 必填（fail-closed）；
// pwdResolve 可为 nil（则 card_pwd 一律默认官方合规映射）。
//
// goods 仅在 CreateOrder 未命中幂等回读时用于取商品单价（分）与可用性校验；
// 查单/退款一律读订单行金额快照（goods_name/unit_price/order_amount），不再回源
// 商品（D6E-02R #2），杜绝历史订单金额漂移与退款 refund_amount=0 仍成功。
func NewSupplyOrderService(store SupplyOrderStore, pool SupplyCardPool, goods SupplyGoodsSource, pwdResolve CardPwdResolver) *SupplyOrderService {
	return &SupplyOrderService{store: store, pool: pool, goods: goods, pwdResolve: pwdResolve}
}

// CreateOrder 创建卡密订单：幂等取卡并同步返回 card_items。
//
// 金额来源：经 goods.GetGoods 取商品单价（分），OrderAmount = Price × buy_quantity。
// 商品不存在 → 1100；商品不可用（status=2）→ 1101；库存不足 → 1102。
//
// max_amount（分，可选）：>0 且 OrderAmount > max_amount → 1202（下单金额低于成本价），
// 且此校验先于取卡，不得发卡。0 表示不校验（官方未传时不校验）。
//
// 幂等（资金红线）：先按 manager_order_no 回读（步骤 0）；命中已有订单直接返回
// 原订单快照视图，完全不读商品源（即便商品下架/改价/更小 max_amount，重试也返回
// 原订单原卡，D6E-02R #1）。未命中则商品校验 → max_amount 校验（均未取卡）→ 取卡
// ＋ 写订单行（含金额快照，同一 DB 事务）；若并发下唯一键冲突，回滚本次取卡并
// 回读先到者的订单，返回同一批卡。
//
// 返回错误：库存不足/商品不存在/商品不可用/金额超额 → *SupplyAPIError；参数非法
// → 普通 error；其它为内部错误（handler 归一为 1209 下单超时）。
func (s *SupplyOrderService) CreateOrder(ctx context.Context, managerOrderNo, goodsNo string, buyQuantity int, maxAmount int64) (*SupplyOrder, error) {
	if s == nil || s.store == nil || s.pool == nil {
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
	//    （D6E-02R #1）。未命中才进入下面的商品校验与取卡。
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

	// 2) max_amount 校验（先于取卡）：官方未传（maxAmount=0）不校验；
	//    传入且下单金额 > max_amount → 1202，且不得取卡。
	if maxAmount > 0 && orderAmount > maxAmount {
		return nil, NewSupplyAPIError(SupplyCodeOrderAmountBelowCost, "下单金额低于成本价")
	}

	// 3) 取卡（原子）：不足即 1102，不写订单行。
	cards, err := s.pool.ClaimCardsForOrder(ctx, goodsNo, buyQuantity)
	if err != nil {
		return nil, fmt.Errorf("xianguanjia supply claim cards: %w", err)
	}
	if len(cards) < buyQuantity {
		// 取到不足：放回已取卡（best-effort），返回 1102。
		s.releaseClaimed(ctx, cards)
		return nil, ErrSupplyStockInsufficient
	}

	// 4) 写订单行（唯一键仲裁幂等），金额快照与取卡同事务落库（D6E-02R #2）。
	cardNos := make([]string, 0, len(cards))
	for _, c := range cards {
		cardNos = append(cardNos, c.CardNo)
	}

	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		s.releaseClaimed(ctx, cards)
		return nil, fmt.Errorf("xianguanjia supply create order begin tx: %w", err)
	}
	id, err := s.store.InsertCreating(ctx, tx, supplyOrderPlanned{
		ManagerOrderNo: managerOrderNo,
		GoodsNo:        goodsNo,
		Quantity:       buyQuantity,
		CardNos:        cardNos,
		Status:         supplyOrderStatusCreating,
		GoodsName:      goods.GoodsName,
		UnitPrice:      goods.Price,
		OrderAmount:    orderAmount,
	})
	if err != nil {
		_ = s.store.RollbackTx(ctx, tx)
		s.releaseClaimed(ctx, cards)
		if errors.Is(err, ErrSupplyOrderDup) {
			// 并发/重发：先到者已发卡，回读并返回同一批卡（不重发）。
			if existing, gerr := s.store.GetByManagerOrderNo(ctx, managerOrderNo); gerr == nil && existing != nil {
				return s.viewExisting(ctx, existing)
			}
		}
		return nil, fmt.Errorf("xianguanjia supply create order insert: %w", err)
	}
	if err := s.store.SetCardsAndStatusTx(ctx, tx, managerOrderNo, cardNos, supplyOrderStatusSuccess); err != nil {
		_ = s.store.RollbackTx(ctx, tx)
		s.releaseClaimed(ctx, cards)
		return nil, fmt.Errorf("xianguanjia supply create order finalize: %w", err)
	}
	if err := s.store.CommitTx(ctx, tx); err != nil {
		_ = s.store.RollbackTx(ctx, tx)
		s.releaseClaimed(ctx, cards)
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
	if s == nil || s.store == nil || s.pool == nil {
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

// releaseClaimed 尽力把已取但未成功下单的卡放回池（unused）。放回失败仅留痕：
// 订单未落库，卡泄漏风险由运维对账兜底（宁可不发，绝不重复发）。
func (s *SupplyOrderService) releaseClaimed(ctx context.Context, cards []SupplyPoolCard) {
	if len(cards) == 0 {
		return
	}
	nos := make([]string, 0, len(cards))
	for _, c := range cards {
		nos = append(nos, c.CardNo)
	}
	if rel, ok := s.pool.(SupplyCardReleaser); ok {
		if err := rel.ReleaseClaimedCards(ctx, nos); err != nil {
			slog.Error("xianguanjia supply order: release claimed cards failed",
				"cards", len(nos), "err", err)
		}
	}
}

// SupplyCardReleaser 是可选能力：把误取（未成功下单）的卡放回 unused。
// 生产 DB 实现满足；内存测试实现亦满足。不满足时 releaseClaimed 静默跳过。
type SupplyCardReleaser interface {
	ReleaseClaimedCards(ctx context.Context, cardNos []string) error
}

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

// NewSupplyCardPool 返回基于 PostgreSQL 的卡密池取卡/作废仓储。
func NewSupplyCardPool(db *sql.DB) *supplyCardPoolDB {
	return &supplyCardPoolDB{db: db}
}

// ClaimCardsForOrder 原子取卡：单条 UPDATE ... WHERE code IN (子查询 LIMIT n)
// RETURNING code，把 n 张 unused 卡置为 delivered 并返回卡号。
//
// 语义（对应派发单「UPDATE ... WHERE status='unused' LIMIT n 语义的原子取卡」）：
//
//	UPDATE redeem_codes SET status='delivered'
//	WHERE id IN (
//	    SELECT id FROM redeem_codes
//	    WHERE status='unused' [AND group_id=$goodsNo]
//	    ORDER BY id FOR UPDATE SKIP LOCKED LIMIT n
//	) RETURNING code;
//
// FOR UPDATE SKIP LOCKED 保证并发下单互不阻塞、不会取到同一张卡；
// group_id 过滤在 goodsNo 可解析为数字时启用（D6c 映射 goods_no=分组ID），
// 解析失败则不按分组过滤（兼容商品维度非分组的场景）。
func (p *supplyCardPoolDB) ClaimCardsForOrder(ctx context.Context, goodsNo string, quantity int) ([]SupplyPoolCard, error) {
	if p == nil || p.db == nil {
		return nil, fmt.Errorf("xianguanjia supply card pool unavailable")
	}
	if quantity <= 0 {
		return nil, nil
	}
	groupID, hasGroup := parseGoodsGroupID(goodsNo)

	query := `
		UPDATE redeem_codes SET status = 'delivered'
		WHERE id IN (
			SELECT id FROM redeem_codes
			WHERE status = 'unused'
	`
	args := []any{}
	if hasGroup {
		query += ` AND group_id = $1`
		args = append(args, groupID)
	}
	query += ` ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $` + fmt.Sprintf("%d", len(args)+1)
	args = append(args, quantity)
	query += ` ) RETURNING code`

	rows, err := p.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("xianguanjia claim cards: %w", err)
	}
	defer rows.Close()
	var cards []SupplyPoolCard
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, fmt.Errorf("xianguanjia scan claimed card: %w", err)
		}
		cards = append(cards, SupplyPoolCard{CardNo: code})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("xianguanjia iterate claimed cards: %w", err)
	}
	return cards, nil
}

// ReleaseClaimedCards 把误取（未成功下单）的卡从 delivered 放回 unused。
// 仅放回原为 delivered 且从未被兑换的卡；used/expired 不动（防损）。
func (p *supplyCardPoolDB) ReleaseClaimedCards(ctx context.Context, cardNos []string) error {
	if p == nil || p.db == nil {
		return fmt.Errorf("xianguanjia supply card pool unavailable")
	}
	for _, no := range cardNos {
		if _, err := p.db.ExecContext(ctx, `
			UPDATE redeem_codes SET status = 'unused'
			WHERE code = $1 AND status = 'delivered'`, no); err != nil {
			return fmt.Errorf("xianguanjia release claimed card: %w", err)
		}
	}
	return nil
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
}

// NewSupplyOrderStoreMemory 返回内存版订单仓储。
func NewSupplyOrderStoreMemory() *supplyOrderStoreMem {
	return &supplyOrderStoreMem{rows: make(map[string]*supplyOrderRow)}
}

func (s *supplyOrderStoreMem) BeginTx(ctx context.Context) (*sql.Tx, error)     { return nil, nil }
func (s *supplyOrderStoreMem) CommitTx(ctx context.Context, tx *sql.Tx) error   { return nil }
func (s *supplyOrderStoreMem) RollbackTx(ctx context.Context, tx *sql.Tx) error { return nil }

func (s *supplyOrderStoreMem) InsertCreating(ctx context.Context, tx *sql.Tx, row supplyOrderPlanned) (int64, error) {
	if s == nil {
		return 0, fmt.Errorf("xianguanjia supply order store unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rows[row.ManagerOrderNo]; ok {
		return 0, ErrSupplyOrderDup
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
	s.rows[row.ManagerOrderNo] = r
	return r.ID, nil
}

func (s *supplyOrderStoreMem) SetCardsAndStatusTx(ctx context.Context, tx *sql.Tx, managerOrderNo string, cardNos []string, status int) error {
	if s == nil {
		return fmt.Errorf("xianguanjia supply order store unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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

func (p *SupplyCardPoolMemory) ClaimCardsForOrder(ctx context.Context, goodsNo string, quantity int) ([]SupplyPoolCard, error) {
	if p == nil {
		return nil, fmt.Errorf("xianguanjia supply card pool unavailable")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]SupplyPoolCard, 0, quantity)
	for _, no := range sortedCardNos(p.cards) {
		if len(out) >= quantity {
			break
		}
		if p.cards[no] == "unused" {
			p.cards[no] = "delivered"
			out = append(out, SupplyPoolCard{CardNo: no, CardPwd: p.pwds[no]})
		}
	}
	return out, nil
}

func (p *SupplyCardPoolMemory) ReleaseClaimedCards(ctx context.Context, cardNos []string) error {
	if p == nil {
		return fmt.Errorf("xianguanjia supply card pool unavailable")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, no := range cardNos {
		if p.cards[no] == "delivered" {
			p.cards[no] = "unused"
		}
	}
	return nil
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

func sortedCardNos(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// 简单插入排序，稳定且无需额外依赖（卡量小）。
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
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
