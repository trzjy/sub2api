package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// claimTransitionOutcome 是原子状态转换（CAS）的结果分类。
type claimTransitionOutcome int

const (
	// claimTransitionApplied 表示 UPDATE 命中，状态已按 spec 转换。
	claimTransitionApplied claimTransitionOutcome = iota
	// claimTransitionIdempotent 表示记录已处于幂等终态且 attempt 一致（重复回执等）。
	claimTransitionIdempotent
	// claimTransitionConflict 表示状态/attempt 与 spec 不一致（旧回执/并发），不得改变。
	claimTransitionConflict
	// claimTransitionNotFound 表示订单不存在。
	claimTransitionNotFound
)

// claimReadback 是转换后/回读到的记录当前状态（code 在 redeem_codes 表，不由本原语返回）。
type claimReadback struct {
	status  string
	attempt int
}

// claimTransitionSpec 声明一次原子状态转换（SQL WHERE/SET 的唯一来源）。
// 所有会修改 xianyu_order_claims.delivery_status 的操作都必须收敛到 applyClaimTransition，
// 由它统一承担 advisory lock、attempt CAS 与幂等/冲突分类，避免各处复刻分散的边界处理。
type claimTransitionSpec struct {
	orderNo string
	// toStatus 目标状态。
	toStatus string
	// fromStatuses CAS 前置状态集合；空表示不限（当前仅 ResendOriginalCode 预读后使用）。
	fromStatuses []string
	// expectedAttempt 期望的 attempt_count；配合 requireAttempt 启用 CAS。
	expectedAttempt int
	// requireAttempt 是否启用 attempt_count = expected 条件。
	requireAttempt bool
	// bumpAttempt 是否 attempt_count = attempt_count + 1（补发发起）。
	bumpAttempt bool
	// setError 写入 delivery_error（非空时优先于 clearError）。
	setError *string
	// clearError 将 delivery_error 置 NULL。
	clearError bool
	// touchLastAttempt 是否更新 last_attempt_at = NOW()。
	touchLastAttempt bool
	// idempotentStatuses 回读 status 属于该集合且 attempt 一致时视为幂等成功。
	idempotentStatuses []string
}

// lockXianyuOrder 对订单取 PostgreSQL advisory 排他事务锁（同一订单所有状态写串行化）。
func lockXianyuOrder(ctx context.Context, tx *sql.Tx, orderNo string) error {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1)::bigint)`, orderNo); err != nil {
		return fmt.Errorf("lock xianyu order: %w", err)
	}
	return nil
}

// applyClaimTransition 是状态转换的公开入口：自管理 BeginTx + advisory lock + Commit/Rollback。
func (r *xianyuOrderClaimStateRepository) applyClaimTransition(ctx context.Context, spec claimTransitionSpec) (claimTransitionOutcome, *claimReadback, error) {
	if r == nil || r.db == nil {
		return 0, nil, errors.New("xianyu claim database is unavailable")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("begin xianyu claim transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockXianyuOrder(ctx, tx, spec.orderNo); err != nil {
		return 0, nil, err
	}
	outcome, rb, err := r.applyClaimTransitionTx(ctx, tx, spec)
	if err != nil {
		return 0, nil, err
	}
	switch outcome {
	case claimTransitionApplied, claimTransitionIdempotent:
		if err := tx.Commit(); err != nil {
			return 0, nil, fmt.Errorf("commit xianyu claim transition: %w", err)
		}
	default:
		_ = tx.Rollback()
	}
	return outcome, rb, nil
}

// applyClaimTransitionTx 在给定事务内执行状态转换，供需要同事务预读/多步操作的调用方复用。
func (r *xianyuOrderClaimStateRepository) applyClaimTransitionTx(ctx context.Context, tx *sql.Tx, spec claimTransitionSpec) (claimTransitionOutcome, *claimReadback, error) {
	var sets []string
	args := []any{spec.toStatus}
	sets = append(sets, fmt.Sprintf("delivery_status = $%d", len(args)))
	if spec.setError != nil {
		args = append(args, *spec.setError)
		sets = append(sets, fmt.Sprintf("delivery_error = $%d", len(args)))
	} else if spec.clearError {
		sets = append(sets, "delivery_error = NULL")
	}
	if spec.bumpAttempt {
		sets = append(sets, "attempt_count = attempt_count + 1")
	}
	if spec.touchLastAttempt {
		sets = append(sets, "last_attempt_at = NOW()")
	}
	sets = append(sets, "updated_at = NOW()")

	where := []string{fmt.Sprintf("order_no = $%d", len(args)+1)}
	args = append(args, spec.orderNo)

	if len(spec.fromStatuses) > 0 {
		ph := make([]string, len(spec.fromStatuses))
		for i, s := range spec.fromStatuses {
			args = append(args, s)
			ph[i] = fmt.Sprintf("$%d", len(args))
		}
		where = append(where, fmt.Sprintf("delivery_status IN (%s)", strings.Join(ph, ",")))
	}
	if spec.requireAttempt {
		args = append(args, spec.expectedAttempt)
		where = append(where, fmt.Sprintf("attempt_count = $%d", len(args)))
	}

	query := fmt.Sprintf(
		"UPDATE xianyu_order_claims SET %s WHERE %s RETURNING attempt_count",
		strings.Join(sets, ", "), strings.Join(where, " AND "),
	)

	var rb claimReadback
	err := tx.QueryRowContext(ctx, query, args...).Scan(&rb.attempt)
	if err == nil {
		rb.status = spec.toStatus
		return claimTransitionApplied, &rb, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, nil, fmt.Errorf("apply xianyu claim transition: %w", err)
	}

	// 无行命中：同事务回读当前状态，分类幂等/冲突/不存在。
	err = tx.QueryRowContext(ctx,
		`SELECT delivery_status, attempt_count FROM xianyu_order_claims WHERE order_no = $1`, spec.orderNo).
		Scan(&rb.status, &rb.attempt)
	if errors.Is(err, sql.ErrNoRows) {
		return claimTransitionNotFound, nil, nil
	}
	if err != nil {
		return 0, nil, fmt.Errorf("readback xianyu claim: %w", err)
	}
	if spec.requireAttempt && rb.attempt != spec.expectedAttempt {
		return claimTransitionConflict, &rb, nil
	}
	for _, s := range spec.idempotentStatuses {
		if rb.status == s {
			return claimTransitionIdempotent, &rb, nil
		}
	}
	return claimTransitionConflict, &rb, nil
}
