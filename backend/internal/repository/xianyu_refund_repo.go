package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type xianyuRefundEventRepository struct {
	ent *dbent.Client
}

// NewXianyuRefundEventRepository 创建闲鱼退款处置仓库。
// 处置全程在单个数据库事务内完成；事务内裸 SQL 通过 ent 事务上下文执行（与
// ApplyRedeemBalanceAdjustment 等追回原语同事务，保证全或无）。
func NewXianyuRefundEventRepository(ent *dbent.Client) service.XianyuRefundEventRepository {
	return &xianyuRefundEventRepository{ent: ent}
}

// ProcessRefundAtomically 原子处置闲鱼退款成功事件（幂等去重锚点：refund_handled_at）。
//
// 事务步骤：
//  1. CAS 认领：UPDATE claims WHERE order_no=$1 AND account_id=$2 AND refund_handled_at IS NULL
//     （行锁 + EvalPlanQual：并发重复事件阻塞后重评估，先提交者胜出，后者回放终态 action）；
//  2. 码快照：SELECT ... FOR UPDATE，与买家兑换（StatusIn(unused,delivered) 乐观锁）互斥；
//  3. 分支处置：delivered/unused → 原子作废(expired)；used → 回调追回（同一事务）；
//  4. 写入处置终态（refund_action/detail/handled_at），随事务一并提交。
//
// 任一步失败整体回滚：码状态与 claim 审计列都不会残留中间态，Worker 可安全重报。
func (r *xianyuRefundEventRepository) ProcessRefundAtomically(ctx context.Context, orderNo, accountID string, clawback service.XianyuRedeemClawback) (*service.XianyuRefundOutcome, error) {
	if r == nil || r.ent == nil {
		return nil, errors.New("xianyu refund repository is unavailable")
	}
	if clawback == nil {
		return nil, errors.New("xianyu refund clawback is unavailable")
	}

	tx, err := r.ent.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin refund transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	txCtx := dbent.NewTxContext(ctx, tx)
	client := clientFromContext(txCtx, r.ent)

	// 1) CAS 认领处置权：已处置（refund_handled_at 非 NULL）或账号不匹配的订单不认领。
	var codeID int64
	err = r.scanOne(client, txCtx, `
		UPDATE xianyu_order_claims
		SET refund_action = NULL, refund_detail = NULL, refund_handled_at = NULL
		WHERE order_no = $1 AND account_id = $2 AND refund_handled_at IS NULL
		RETURNING redeem_code_id`, []any{&codeID}, orderNo, accountID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// 区分：无领取记录 / 账号不匹配 / 此前已处置（幂等重入回放）。
		return r.resolveUnclaimed(txCtx, client, orderNo, accountID)
	case err != nil:
		return nil, fmt.Errorf("acquire refund handling: %w", err)
	}

	// 2) 码快照（FOR UPDATE 与买家兑换互斥，先提交者胜出）。
	var (
		codeStatus   string
		codeType     string
		codeStr      string
		value        float64
		validityDays int
		groupID      sql.NullInt64
		usedBy       sql.NullInt64
	)
	err = r.scanOne(client, txCtx, `
		SELECT status, type, code, value, validity_days, group_id, used_by
		FROM redeem_codes WHERE id = $1 FOR UPDATE`,
		[]any{&codeStatus, &codeType, &codeStr, &value, &validityDays, &groupID, &usedBy}, codeID)
	if err != nil {
		return nil, fmt.Errorf("load redeem code for refund: %w", err)
	}

	outcome := &service.XianyuRefundOutcome{}
	switch codeStatus {
	case service.StatusDelivered, service.StatusUnused:
		// 作废 = 与管理端「作废」一致的 expired 终态；行锁保护下条件更新必命中。
		if _, err := client.ExecContext(txCtx, `
			UPDATE redeem_codes SET status = 'expired'
			WHERE id = $1 AND status IN ('delivered', 'unused')`, codeID); err != nil {
			return nil, fmt.Errorf("void redeem code on refund: %w", err)
		}
		outcome.Action = service.XianyuRefundActionVoided
		outcome.Detail = "退款成功，兑换码已作废"
	case service.StatusExpired:
		outcome.Action = service.XianyuRefundActionAlreadyVoid
		outcome.Detail = "兑换码已是作废状态"
	case service.StatusDisabled:
		outcome.Action = service.XianyuRefundActionIgnored
		outcome.Detail = "兑换码已禁用，无需处置"
	case service.StatusUsed:
		code := &service.RedeemCode{
			ID: codeID, Code: codeStr, Type: codeType, Value: value, Status: codeStatus,
			ValidityDays: validityDays,
		}
		if groupID.Valid {
			v := groupID.Int64
			code.GroupID = &v
		}
		if usedBy.Valid {
			v := usedBy.Int64
			code.UsedBy = &v
		}
		detail, cerr := clawback.ClawbackXianyuRedeemCodeTx(txCtx, code)
		if cerr != nil {
			// 整体回滚：claim 审计列与码状态不残留中间态，Worker 可安全重报。
			return nil, fmt.Errorf("clawback redeem code: %w", cerr)
		}
		outcome.Action = service.XianyuRefundActionClawedBack
		outcome.Detail = detail
		outcome.Code = code
	default:
		return nil, service.ErrXianyuRefundCodeStatusUnknown
	}

	// 3) 处置终态随事务提交（本事务已持有 claim 行锁，守卫必命中；0 行视为异常）。
	result, err := client.ExecContext(txCtx, `
		UPDATE xianyu_order_claims
		SET refund_action = $2, refund_detail = $3, refund_handled_at = NOW()
		WHERE order_no = $1 AND refund_handled_at IS NULL`, orderNo, outcome.Action, outcome.Detail)
	if err != nil {
		return nil, fmt.Errorf("record refund terminal state: %w", err)
	}
	if affected, aerr := result.RowsAffected(); aerr == nil && affected == 0 {
		return nil, errors.New("refund terminal state update affected no rows")
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit refund handling: %w", err)
	}
	slog.Info("xianyu refund handled", "order_no", orderNo, "action", outcome.Action, "detail", outcome.Detail)
	return outcome, nil
}

// resolveUnclaimed 在 CAS 认领落空后区分三种情况并给出对应结果。
func (r *xianyuRefundEventRepository) resolveUnclaimed(txCtx context.Context, client *dbent.Client, orderNo, accountID string) (*service.XianyuRefundOutcome, error) {
	var (
		claimAccount string
		handledAt    sql.NullTime
		action       sql.NullString
	)
	err := r.scanOne(client, txCtx, `
		SELECT account_id, refund_handled_at, refund_action
		FROM xianyu_order_claims WHERE order_no = $1`,
		[]any{&claimAccount, &handledAt, &action}, orderNo)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrXianyuRefundClaimNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup refund claim: %w", err)
	}
	if claimAccount != accountID {
		return nil, service.ErrXianyuRefundAccountMismatch
	}
	if handledAt.Valid && action.Valid {
		// 已处置：幂等回放当时的 action（Worker 拿到 2xx 后才置位上报去重标记）。
		return &service.XianyuRefundOutcome{Action: action.String, Detail: "退款事件重复上报，回放既往处置结果"}, nil
	}
	// 账号匹配但未处置却认领失败：不应发生（并发场景下认领者要么已提交终态、
	// 要么仍持有行锁使本事务阻塞），防御性报错等待 Worker 重报。
	return nil, errors.New("refund claim acquire failed unexpectedly")
}

// scanOne 把 ent config 的 QueryContext 适配为 QueryRow 语义（单行 Scan / ErrNoRows）。
func (r *xianyuRefundEventRepository) scanOne(client *dbent.Client, ctx context.Context, query string, dest []any, args ...any) error {
	rows, err := client.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	return rows.Scan(dest...)
}
