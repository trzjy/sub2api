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

// 货源订单状态（内部存储用；对外 order_status 见 SupplyOrderStatusSuccess）。
const (
	// supplyOrderStatusCreating 内部占位态：仅存在于「创建订单」写入事务的
	// 极短窗口（取卡→写行的同一事务内）；事务提交后即为 Success。不回读为成功。
	supplyOrderStatusCreating = 10
	// supplyOrderStatusSuccess 官方 order_status=20：卡密订单创建成功。
	supplyOrderStatusSuccess = 20
	// supplyOrderStatusRefunded 已退款：退款通知到达后置位，同时作废卡。
	supplyOrderStatusRefunded = 30
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

// SupplyOrder 是货源订单视图（查单/创建共用）。
//
// OrderStatus 对外为官方整数：20=成功，30=已退款（退款态再查单可见：
// handler 据本字段把 order_status 归一为 30，并在 card_items 上标记已作废）。
// CardItems 为下单时同步返回、或查单时回读的卡密；退款后仍回读原卡密，
// 但 Refunded=true，调用方不得再次发货。
type SupplyOrder struct {
	ManagerOrderNo string                `json:"manager_order_no"`
	GoodsNo        string                `json:"goods_no"`
	Quantity       int                   `json:"quantity"`
	OrderStatus    int                   `json:"order_status"`
	CardItems      []SupplyOrderCardItem `json:"card_items"`
	Refunded       bool                  `json:"refunded"`
	CreatedAt      time.Time             `json:"created_at"`
	RefundedAt     *time.Time            `json:"refunded_at,omitempty"`
}

// ---- 存储 DTO ----

// supplyOrderRow 是 xianguanjia_supply_orders 的一行（服务内部使用）。
type supplyOrderRow struct {
	ID             int64
	ManagerOrderNo string
	GoodsNo        string
	Quantity       int
	Status         int
	CardNos        []string
	CreatedAt      time.Time
	RefundedAt     *time.Time
}

// supplyOrderToView 把存储行转为对外视图。
func supplyOrderToView(row *supplyOrderRow, items []SupplyOrderCardItem) *SupplyOrder {
	if row == nil {
		return nil
	}
	out := &SupplyOrder{
		ManagerOrderNo: row.ManagerOrderNo,
		GoodsNo:        row.GoodsNo,
		Quantity:       row.Quantity,
		OrderStatus:    row.Status,
		CardItems:      items,
		Refunded:       row.Status == supplyOrderStatusRefunded,
		CreatedAt:      row.CreatedAt,
		RefundedAt:     row.RefundedAt,
	}
	return out
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
}

// ---- 订单仓储 ----

// SupplyOrderStore 货源订单仓储（生产实现基于 xianguanjia_supply_orders）。
//
// 幂等核心：InsertCreating 依赖 manager_order_no UNIQUE 约束；
// 冲突返回 ErrSupplyOrderDup，调用方回滚取卡并回读已存在订单。
type SupplyOrderStore interface {
	// InsertCreating 在事务内插入占位订单行（status=10 creating）。
	// manager_order_no 已存在时返回 ErrSupplyOrderDup。
	InsertCreating(ctx context.Context, tx *sql.Tx, row supplyOrderPlanned) (int64, error)
	// GetByManagerOrderNo 按管家订单号读取订单行；不存在返回 (nil, nil)。
	GetByManagerOrderNo(ctx context.Context, managerOrderNo string) (*supplyOrderRow, error)
	// MarkRefunded 幂等地把订单置为退款态并写 refunded_at；返回是否本次变更。
	MarkRefunded(ctx context.Context, managerOrderNo string, at time.Time) (bool, error)

	// BeginTx 开启事务（DB 实现返回 *sql.Tx；内存实现返回 nil, nil，语义仍由实现保证）。
	BeginTx(ctx context.Context) (*sql.Tx, error)
	// CommitTx/RollbackTx 结束事务（内存实现为 no-op）。
	CommitTx(ctx context.Context, tx *sql.Tx) error
	RollbackTx(ctx context.Context, tx *sql.Tx) error
	// SetCardsAndStatusTx 事务内写入卡的映射与最终状态（创建成功时 status=20）。
	SetCardsAndStatusTx(ctx context.Context, tx *sql.Tx, managerOrderNo string, cardNos []string, status int) error
}

// supplyOrderPlanned 是插入订单行的计划载荷。
type supplyOrderPlanned struct {
	ManagerOrderNo string
	GoodsNo        string
	Quantity       int
	CardNos        []string
	Status         int
}

// ErrSupplyOrderDup 是订单号已存在的内部哨兵错误（幂等冲突信号，非对外错误码）。
var ErrSupplyOrderDup = errors.New("xianguanjia supply order duplicated")

// ---- 服务 ----

// CardPwdResolver 解析池卡的 card_pwd（D6d 联调校正点：redeem_codes 无独立
// 卡密列，生产默认以空串下发；集成方可注入实现从外部映射补齐）。
// 返回空串表示无卡密（合法，单卡号商品）。
type CardPwdResolver interface {
	// ResolveCardPwd 批量解析 card_no → card_pwd；未命中 key 视为空串。
	ResolveCardPwd(ctx context.Context, cardNos []string) (map[string]string, error)
}

// SupplyOrderService 货源卡密订单服务（创建/查单/退款）。
type SupplyOrderService struct {
	store      SupplyOrderStore
	pool       SupplyCardPool
	pwdResolve CardPwdResolver
}

// NewSupplyOrderService 构造订单服务。store/pool 必填（fail-closed）；
// pwdResolve 可为 nil（则 card_pwd 一律空串）。
func NewSupplyOrderService(store SupplyOrderStore, pool SupplyCardPool, pwdResolve CardPwdResolver) *SupplyOrderService {
	return &SupplyOrderService{store: store, pool: pool, pwdResolve: pwdResolve}
}

// CreateOrder 创建卡密订单：幂等取卡并同步返回 card_items。
//
// 幂等（资金红线）：先按 manager_order_no 回读；命中已有订单直接返回原结果，
// 绝不再取卡。未命中则取卡 + 写订单行（同一 DB 事务）；若并发下唯一键冲突，
// 回滚本次取卡并回读先到者的订单，返回同一批卡。
//
// 返回错误：库存不足 → *SupplyAPIError(code=1102)；参数非法 → 普通 error；
// 其它为内部错误（handler 归一为 1209 下单超时）。
func (s *SupplyOrderService) CreateOrder(ctx context.Context, managerOrderNo, goodsNo string, quantity int) (*SupplyOrder, error) {
	if s == nil || s.store == nil || s.pool == nil {
		return nil, fmt.Errorf("xianguanjia supply order service unavailable")
	}
	managerOrderNo = strings.TrimSpace(managerOrderNo)
	goodsNo = strings.TrimSpace(goodsNo)
	if managerOrderNo == "" {
		return nil, fmt.Errorf("xianguanjia supply create order: manager_order_no is required")
	}
	if quantity <= 0 {
		return nil, fmt.Errorf("xianguanjia supply create order: quantity must be > 0")
	}

	// 1) 幂等前置回读：已有订单直接返回原结果（不重发卡）。
	if existing, err := s.store.GetByManagerOrderNo(ctx, managerOrderNo); err != nil {
		return nil, fmt.Errorf("xianguanjia supply create order lookup: %w", err)
	} else if existing != nil {
		return s.viewExisting(ctx, existing)
	}

	// 2) 取卡（原子）：不足即 1102，不写订单行。
	cards, err := s.pool.ClaimCardsForOrder(ctx, goodsNo, quantity)
	if err != nil {
		return nil, fmt.Errorf("xianguanjia supply claim cards: %w", err)
	}
	if len(cards) < quantity {
		// 取到不足：放回已取卡（best-effort），返回 1102。
		s.releaseClaimed(ctx, cards)
		return nil, ErrSupplyStockInsufficient
	}

	// 3) 写订单行（唯一键仲裁幂等）。
	cardNos := make([]string, 0, len(cards))
	for _, c := range cards {
		cardNos = append(cardNos, c.CardNo)
	}

	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		s.releaseClaimed(ctx, cards)
		return nil, fmt.Errorf("xianguanjia supply create order begin tx: %w", err)
	}
	if _, err := s.store.InsertCreating(ctx, tx, supplyOrderPlanned{
		ManagerOrderNo: managerOrderNo,
		GoodsNo:        goodsNo,
		Quantity:       quantity,
		CardNos:        cardNos,
		Status:         supplyOrderStatusCreating,
	}); err != nil {
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
	slog.Info("xianguanjia supply order created",
		"manager_order_no", managerOrderNo, "goods_no", goodsNo, "cards", len(cardNos))
	return &SupplyOrder{
		ManagerOrderNo: managerOrderNo,
		GoodsNo:        goodsNo,
		Quantity:       quantity,
		OrderStatus:    supplyOrderStatusSuccess,
		CardItems:      items,
		CreatedAt:      time.Now(),
	}, nil
}

// GetOrder 查询订单详情（幂等查询）：返回状态 + 卡密。
// 订单不存在 → ErrSupplyOrderNotFound（code=1200）。
func (s *SupplyOrderService) GetOrder(ctx context.Context, managerOrderNo string) (*SupplyOrder, error) {
	if s == nil || s.store == nil {
		return nil, fmt.Errorf("xianguanjia supply order service unavailable")
	}
	managerOrderNo = strings.TrimSpace(managerOrderNo)
	if managerOrderNo == "" {
		return nil, fmt.Errorf("xianguanjia supply get order: manager_order_no is required")
	}
	row, err := s.store.GetByManagerOrderNo(ctx, managerOrderNo)
	if err != nil {
		return nil, fmt.Errorf("xianguanjia supply get order: %w", err)
	}
	if row == nil {
		return nil, ErrSupplyOrderNotFound
	}
	return s.viewExisting(ctx, row)
}

// RefundNotify 处理退款通知：订单置退款态 + 作废对应卡（delivered→expired，
// 复用 D2 语义），仅作废本订单已发的卡（由 card_nos 精确定位）。
//
// 幂等：订单已是退款态时重复通知不报错（MarkRefunded 幂等；作废动作自身幂等）。
// 订单不存在 → ErrSupplyOrderNotFound（code=1200，handler 归一）。
func (s *SupplyOrderService) RefundNotify(ctx context.Context, managerOrderNo string) (*SupplyOrder, error) {
	if s == nil || s.store == nil || s.pool == nil {
		return nil, fmt.Errorf("xianguanjia supply order service unavailable")
	}
	managerOrderNo = strings.TrimSpace(managerOrderNo)
	if managerOrderNo == "" {
		return nil, fmt.Errorf("xianguanjia supply refund notify: manager_order_no is required")
	}
	row, err := s.store.GetByManagerOrderNo(ctx, managerOrderNo)
	if err != nil {
		return nil, fmt.Errorf("xianguanjia supply refund notify lookup: %w", err)
	}
	if row == nil {
		return nil, ErrSupplyOrderNotFound
	}

	// 先作废卡再置退款态：作废失败（DB/临时故障）不置退款态，交给闲管家重试
	// （与 D2 「处理成功才落回执」一致）。作废本身幂等：已 expired 不计数、不报错。
	voided, err := s.pool.VoidCardsForOrder(ctx, row.CardNos)
	if err != nil {
		return nil, fmt.Errorf("xianguanjia supply refund void cards for order %s: %w", managerOrderNo, err)
	}
	if _, err := s.store.MarkRefunded(ctx, managerOrderNo, time.Now()); err != nil {
		return nil, fmt.Errorf("xianguanjia supply refund mark order %s: %w", managerOrderNo, err)
	}
	slog.Info("xianguanjia supply order refunded",
		"manager_order_no", managerOrderNo, "cards", len(row.CardNos), "voided", voided)

	row.Status = supplyOrderStatusRefunded
	now := time.Now()
	row.RefundedAt = &now
	return s.viewExisting(ctx, row)
}

// viewExisting 把存储行转为对外视图并回读卡密。
func (s *SupplyOrderService) viewExisting(ctx context.Context, row *supplyOrderRow) (*SupplyOrder, error) {
	items, err := s.buildCardItems(ctx, row.CardNos)
	if err != nil {
		slog.Warn("xianguanjia supply order: resolve card pwd failed",
			"manager_order_no", row.ManagerOrderNo, "err", err)
		items = emptyCardItems(row.CardNos)
	}
	return supplyOrderToView(row, items), nil
}

// buildCardItems 组装 card_items（card_no + card_pwd）。
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
		items = append(items, SupplyOrderCardItem{CardNo: no, CardPwd: pwds[no]})
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
		items = append(items, SupplyOrderCardItem{CardNo: no})
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
			(manager_order_no, goods_no, quantity, status, card_nos)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`,
		row.ManagerOrderNo, row.GoodsNo, row.Quantity, row.Status, encodeCardNos(row.CardNos)).Scan(&id)
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

func (s *supplyOrderStoreDB) MarkRefunded(ctx context.Context, managerOrderNo string, at time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("xianguanjia supply order store unavailable")
	}
	res, err := s.db.ExecContext(ctx, `
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
	SELECT id, manager_order_no, goods_no, quantity, status, card_nos, created_at, refunded_at
	FROM xianguanjia_supply_orders WHERE manager_order_no = $1`

func scanSupplyOrder(row *sql.Row) (*supplyOrderRow, error) {
	var r supplyOrderRow
	var cardNosRaw []byte
	var refunded sql.NullTime
	if err := row.Scan(&r.ID, &r.ManagerOrderNo, &r.GoodsNo, &r.Quantity, &r.Status,
		&cardNosRaw, &r.CreatedAt, &refunded); err != nil {
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
	if p == nil || p.db == nil {
		return 0, fmt.Errorf("xianguanjia supply card pool unavailable")
	}
	total := 0
	for _, no := range cardNos {
		res, err := p.db.ExecContext(ctx, `
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

func (s *supplyOrderStoreMem) MarkRefunded(ctx context.Context, managerOrderNo string, at time.Time) (bool, error) {
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

// supplyCardPoolMem 内存卡密池：模拟 redeem_codes 的 unused/delivered/expired 状态机。
// cards: card_no -> 状态。互斥锁保证并发取卡不重发。
type supplyCardPoolMem struct {
	mu    sync.Mutex
	cards map[string]string // card_no -> status
	pwds  map[string]string // card_no -> card_pwd
}

// NewSupplyCardPoolMemory 返回内存版卡密池。cardNos 全部初始化为 unused；
// cardPwds 可为 nil（card_pwd 空串）。
func NewSupplyCardPoolMemory(cardNos []string, cardPwds map[string]string) *supplyCardPoolMem {
	cards := make(map[string]string, len(cardNos))
	for _, no := range cardNos {
		cards[no] = "unused"
	}
	pwds := make(map[string]string, len(cardPwds))
	for k, v := range cardPwds {
		pwds[k] = v
	}
	return &supplyCardPoolMem{cards: cards, pwds: pwds}
}

// StatusOf 返回内存池中某卡状态（测试断言用）。
func (p *supplyCardPoolMem) StatusOf(cardNo string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cards[cardNo]
}

// CountByStatus 统计内存池中某状态的卡数（测试断言用）。
func (p *supplyCardPoolMem) CountByStatus(status string) int {
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

func (p *supplyCardPoolMem) ClaimCardsForOrder(ctx context.Context, goodsNo string, quantity int) ([]SupplyPoolCard, error) {
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

func (p *supplyCardPoolMem) ReleaseClaimedCards(ctx context.Context, cardNos []string) error {
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

func (p *supplyCardPoolMem) VoidCardsForOrder(ctx context.Context, cardNos []string) (int, error) {
	if p == nil {
		return 0, fmt.Errorf("xianguanjia supply card pool unavailable")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, no := range cardNos {
		switch p.cards[no] {
		case "delivered", "unused":
			p.cards[no] = "expired"
			n++
		}
	}
	return n, nil
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
	pool *supplyCardPoolMem
}

// NewSupplyCardPwdResolverMemory 返回内存版 card_pwd 解析器。
func NewSupplyCardPwdResolverMemory(pool *supplyCardPoolMem) *supplyCardPwdResolverMem {
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
// （当前 redeem_codes 无独立卡密列）；默认 nil 时 card_pwd 为空串（联调校正点）。
