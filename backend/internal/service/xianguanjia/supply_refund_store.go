package xianguanjia

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"

	dbent "github.com/Wei-Shaw/sub2api/ent"
)

// SupplyRefundRecord 是单次原子退款处置的结果（方案 §4.1）。
//
// Row 是处置后的订单行（已含 refunded_at，单源同值）；Clawed 是走了 clawback（used 码）
// 的码列表，供 service 在事务提交后逐个失效用户订阅/鉴权缓存。
type SupplyRefundRecord struct {
	Row    *supplyOrderRow
	Clawed []*service.RedeemCode
}

// SupplyRefundStore 货源订单退款仓库：单事务原子处置（行锁读订单 → 逐码分支 → 置退款态）。
//
// 与闲鱼退款链同款骨架（repository.xianyuRefundEventRepository.ProcessRefundAtomically）：
// ent.Tx + dbent.NewTxContext，裸 SQL 经事务内 client 执行，单事务全或无。
type SupplyRefundStore interface {
	// ProcessRefundAtomically 单事务原子处置退款。
	//
	// 返回契约（R6 #1）：持订单行锁后发现订单已退款（并发后到者）→ 返回原订单行 +
	// 原 refunded_at + 空 Clawed，事务内零写操作，由 service 统一走 viewExisting 回放
	// 原快照 agree，绝不落错误信封。
	// 任何一步失败 → 整体回滚，订单与码零残留，调用方可安全重试。
	ProcessRefundAtomically(ctx context.Context, managerOrderNo string) (*SupplyRefundRecord, error)
}

type supplyRefundStore struct {
	ent      *dbent.Client
	clawback service.XianyuRedeemClawback
}

// NewSupplyRefundStore 构造 ent 事务退款仓库。clawback 复用父包 service.XianyuRedeemClawback
// 单一接口（与闲鱼链同款能力），不另定义。
func NewSupplyRefundStore(ent *dbent.Client, clawback service.XianyuRedeemClawback) SupplyRefundStore {
	return &supplyRefundStore{ent: ent, clawback: clawback}
}

// ProcessRefundAtomically 单事务原子处置退款（方案 §4.1 + R1/R6 修正）。
//
// 步骤：
//  1. ent.Tx 开始；行锁读订单（FOR UPDATE）。持锁后发现已退款（R6 契约）→ 返回原订单行 +
//     原 refunded_at + 空 Clawed，事务内零写操作。
//  2. 码快照：SELECT ... WHERE code = ANY($1) FOR UPDATE（与买家兑换乐观锁互斥）。
//  3. 集合完整性校验（R1 must_fix，先于任何状态变更）：返回行与订单 CardNos 集合等价——
//     行数相等、code 无重复、无缺失；不等价 → 整体回滚 + 失败关闭。
//  4. 逐码分支处置（方案 §3 表）：delivered/unused → expired；used → clawback；
//     expired/disabled → 无操作；其它/未知 → 整体回滚 + 失败关闭。
//  5. 置退款态（COALESCE(refunded_at, $now)，与现有 MarkRefundedTx 同款 SQL 语义）；
//     同一 now 写回返回行的 RefundedAt（单源同值）。
//  6. Commit。任一步失败 → 整体回滚，订单与码零残留。
func (r *supplyRefundStore) ProcessRefundAtomically(ctx context.Context, managerOrderNo string) (*SupplyRefundRecord, error) {
	if r == nil || r.ent == nil {
		return nil, errors.New("supply refund store unavailable")
	}
	if r.clawback == nil {
		return nil, errors.New("supply refund clawback unavailable")
	}

	tx, err := r.ent.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin supply refund transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	txCtx := dbent.NewTxContext(ctx, tx)
	client := tx.Client()

	// 1) 行锁读订单。
	row, err := scanSupplyRefundOrderRow(txCtx, client, supplyOrderSelectForUpdateSQL, managerOrderNo)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSupplyOrderNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load supply order for refund: %w", err)
	}

	// R6 #1：持锁后发现订单已退款 → 返回原订单行 + 原 refunded_at + 空 Clawed，零写入。
	if row.Status == supplyOrderStatusRefunded {
		return &SupplyRefundRecord{Row: row, Clawed: nil}, nil
	}

	// 2) 码快照（FOR UPDATE，与买家兑换互斥，先提交者胜出）。
	codes, err := r.loadRedeemCodesForUpdate(txCtx, client, row.CardNos)
	if err != nil {
		return nil, err
	}

	// 2' 集合完整性校验（R1 must_fix，先于任何状态变更）。
	if err := verifyCodeSetEquivalence(row.CardNos, codes); err != nil {
		return nil, err
	}

	// 3) 逐码按 §3 表分支处置。
	clawed := make([]*service.RedeemCode, 0, len(codes))
	for _, c := range codes {
		switch c.Status {
		case domain.StatusDelivered, domain.StatusUnused:
			// 作废（expired 终态），与闲鱼链 XianyuRefundActionVoided 一致。
			if _, err := client.ExecContext(txCtx, `
				UPDATE redeem_codes SET status = 'expired'
				WHERE id = $1 AND status IN ('delivered', 'unused')`, c.ID); err != nil {
				return nil, fmt.Errorf("void redeem code on supply refund: %w", err)
			}
		case domain.StatusUsed:
			// 追回已核销权益（订阅扣天/取消；订阅已不存在视为追回完成），与闲鱼链一致。
			detail, cerr := r.clawback.ClawbackXianyuRedeemCodeTx(txCtx, c)
			if cerr != nil {
				// 整体回滚：码状态/订单/订阅均不残留中间态，可安全重报。
				return nil, fmt.Errorf("clawback redeem code on supply refund: %w", cerr)
			}
			_ = detail
			clawed = append(clawed, c)
		case domain.StatusExpired, domain.StatusDisabled:
			// 无操作：expired 已是终态（同 already_voided），disabled 同 ignored。
		default:
			// 未知状态：失败关闭，整体回滚 + 报错（不置退款态）。
			return nil, errors.New("supply refund: unknown redeem code status")
		}
	}

	// 4) 置退款态（与现有 MarkRefundedTx SQL 语义一致）；同一 now 写回返回行。
	now := time.Now()
	if _, err := client.ExecContext(txCtx, `
		UPDATE xianguanjia_supply_orders
		SET status = $1, refunded_at = COALESCE(refunded_at, $2)
		WHERE manager_order_no = $3`, supplyOrderStatusRefunded, now, managerOrderNo); err != nil {
		return nil, fmt.Errorf("mark supply order refunded: %w", err)
	}
	row.Status = supplyOrderStatusRefunded
	if row.RefundedAt == nil {
		t := now
		row.RefundedAt = &t
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit supply refund: %w", err)
	}
	slog.Info("xianguanjia supply order refund committed",
		"manager_order_no", managerOrderNo, "cards", len(row.CardNos), "clawed", len(clawed))
	return &SupplyRefundRecord{Row: row, Clawed: clawed}, nil
}

// loadRedeemCodesForUpdate 事务内 FOR UPDATE 快照订单卡对应的兑换码（与买家兑换乐观锁互斥）。
func (r *supplyRefundStore) loadRedeemCodesForUpdate(ctx context.Context, client *dbent.Client, cardNos []string) ([]*service.RedeemCode, error) {
	if len(cardNos) == 0 {
		return nil, nil
	}
	rows, err := client.QueryContext(ctx, `
		SELECT id, status, code, type, value, validity_days, group_id, used_by
		FROM redeem_codes WHERE code = ANY($1) FOR UPDATE`, pq.Array(cardNos))
	if err != nil {
		return nil, fmt.Errorf("load redeem codes for supply refund: %w", err)
	}
	defer rows.Close()

	codes := make([]*service.RedeemCode, 0, len(cardNos))
	for rows.Next() {
		var (
			id        int64
			status    string
			codeStr   string
			codeType  string
			value     float64
			vdays     int
			groupID   sql.NullInt64
			usedBy    sql.NullInt64
		)
		if err := rows.Scan(&id, &status, &codeStr, &codeType, &value, &vdays, &groupID, &usedBy); err != nil {
			return nil, fmt.Errorf("scan redeem code for supply refund: %w", err)
		}
		c := &service.RedeemCode{
			ID:           id,
			Code:         codeStr,
			Type:         codeType,
			Status:       status,
			Value:        value,
			ValidityDays: vdays,
		}
		if groupID.Valid {
			v := groupID.Int64
			c.GroupID = &v
		}
		if usedBy.Valid {
			v := usedBy.Int64
			c.UsedBy = &v
		}
		codes = append(codes, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate redeem codes for supply refund: %w", err)
	}
	return codes, nil
}

// verifyCodeSetEquivalence 校验快照行集合与订单 CardNos 集合等价（R1 must_fix）：
// 行数相等、code 无重复、无缺失。不等价 → 失败关闭（与未知状态同路径，整体回滚）。
// 必须在任何状态变更之前调用。
func verifyCodeSetEquivalence(cardNos []string, codes []*service.RedeemCode) error {
	if len(codes) != len(cardNos) {
		return fmt.Errorf("supply refund: redeem code set mismatch (got %d rows, want %d cards)", len(codes), len(cardNos))
	}
	seen := make(map[string]bool, len(codes))
	for _, c := range codes {
		if seen[c.Code] {
			return fmt.Errorf("supply refund: duplicate redeem code %q in snapshot", c.Code)
		}
		seen[c.Code] = true
	}
	for _, cn := range cardNos {
		if !seen[cn] {
			return fmt.Errorf("supply refund: missing redeem code %q in snapshot", cn)
		}
	}
	return nil
}

// scanSupplyRefundOrderRow 事务内（FOR UPDATE）读取并扫描订单行。
func scanSupplyRefundOrderRow(ctx context.Context, client *dbent.Client, query string, args ...any) (*supplyOrderRow, error) {
	rows, err := client.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, sql.ErrNoRows
	}
	var r supplyOrderRow
	var cardNosRaw []byte
	var refunded sql.NullTime
	if err := rows.Scan(&r.ID, &r.ManagerOrderNo, &r.GoodsNo, &r.Quantity, &r.Status,
		&cardNosRaw, &r.CreatedAt, &refunded, &r.GoodsName, &r.UnitPrice, &r.OrderAmount); err != nil {
		return nil, err
	}
	r.CardNos = decodeCardNos(cardNosRaw)
	if refunded.Valid {
		t := refunded.Time
		r.RefundedAt = &t
	}
	return &r, nil
}
