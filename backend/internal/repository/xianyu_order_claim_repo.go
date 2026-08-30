package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type xianyuOrderClaimRepository struct {
	db *sql.DB
}

func NewXianyuOrderClaimRepository(db *sql.DB) service.XianyuDeliveryRepository {
	return &xianyuOrderClaimRepository{db: db}
}

// xianyuOrderClaimStateRepository 同时实现状态更新接口，供适配端点与人工补发使用。
type xianyuOrderClaimStateRepository struct {
	db *sql.DB
}

// NewXianyuOrderClaimStateRepository 创建发货状态更新仓库。
func NewXianyuOrderClaimStateRepository(db *sql.DB) service.XianyuDeliveryStateUpdater {
	return &xianyuOrderClaimStateRepository{db: db}
}

// xianyuDeliveryListRepository 实现发货记录查询。
type xianyuDeliveryListRepository struct {
	db *sql.DB
}

// NewXianyuDeliveryListRepository 创建发货记录列表仓库。
func NewXianyuDeliveryListRepository(db *sql.DB) service.XianyuDeliveryListRepository {
	return &xianyuDeliveryListRepository{db: db}
}

func (r *xianyuDeliveryListRepository) ListDeliveryClaims(ctx context.Context, filter service.XianyuDeliveryFilter) ([]service.XianyuOrderClaim, int, error) {
	where := "WHERE 1=1"
	args := make([]any, 0, 4)
	if filter.Status != "" {
		args = append(args, filter.Status)
		where += fmt.Sprintf(" AND c.delivery_status = $%d", len(args))
	}
	if filter.Search != "" {
		args = append(args, "%"+filter.Search+"%")
		where += fmt.Sprintf(" AND (c.order_no ILIKE $%d OR c.account_id ILIKE $%d OR r.code ILIKE $%d)", len(args), len(args), len(args))
	}
	var total int
	if err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM xianyu_order_claims c
		JOIN redeem_codes r ON r.id = c.redeem_code_id
		`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count xianyu delivery claims: %w", err)
	}
	limit := filter.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}
	args = append(args, limit, filter.Offset)
	rows, err := r.db.QueryContext(ctx, `
		SELECT c.order_no, r.code, c.account_id, c.item_id, c.buyer_id, c.chat_id, c.amount,
		       c.product_id, c.pool_id, c.binding_source, c.delivery_status, c.delivery_error,
		       c.attempt_count, c.last_attempt_at, c.created_at
		FROM xianyu_order_claims c
		JOIN redeem_codes r ON r.id = c.redeem_code_id
		`+where+`
		ORDER BY c.created_at DESC
		LIMIT $`+strconv.Itoa(len(args)-1)+` OFFSET $`+strconv.Itoa(len(args)),
		args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list xianyu delivery claims: %w", err)
	}
	defer rows.Close()
	var out []service.XianyuOrderClaim
	for rows.Next() {
		var c service.XianyuOrderClaim
		var amount, deliveryError sql.NullString
		var productID, poolID sql.NullInt64
		var bindingSource sql.NullString
		var lastAttemptAt sql.NullTime
		if err := rows.Scan(&c.OrderNo, &c.Code, &c.AccountID, &c.ItemID, &c.BuyerID, &c.ChatID, &amount,
			&productID, &poolID, &bindingSource, &c.DeliveryStatus, &deliveryError,
			&c.AttemptCount, &lastAttemptAt, &c.CreatedAt); err != nil {
			return nil, 0, fmt.Errorf("scan xianyu delivery claim: %w", err)
		}
		if amount.Valid {
			c.Amount = &amount.String
		}
		if productID.Valid {
			c.ProductID = &productID.Int64
		}
		if poolID.Valid {
			c.PoolID = &poolID.Int64
		}
		if bindingSource.Valid {
			c.BindingSource = &bindingSource.String
		}
		if deliveryError.Valid {
			c.DeliveryError = &deliveryError.String
		}
		if lastAttemptAt.Valid {
			c.LastAttemptAt = &lastAttemptAt.Time
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}

func (r *xianyuOrderClaimRepository) Claim(ctx context.Context, claim service.XianyuDeliveryClaim, systemUserID int64) (string, error) {
	if r == nil || r.db == nil {
		return "", errors.New("xianyu claim database is unavailable")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin xianyu claim transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Serialize requests for the same external order before checking the claim
	// row. This avoids allocating two inventory codes for a concurrent first call.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1)::bigint)`, claim.OrderID); err != nil {
		return "", fmt.Errorf("lock xianyu order: %w", err)
	}

	var existingCode string
	err = tx.QueryRowContext(ctx, `
		SELECT r.code
		FROM xianyu_order_claims c
		JOIN redeem_codes r ON r.id = c.redeem_code_id
		WHERE c.order_no = $1`, claim.OrderID).Scan(&existingCode)
	if err == nil {
		if err := tx.Commit(); err != nil {
			return "", fmt.Errorf("commit existing xianyu claim: %w", err)
		}
		return existingCode, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("lookup xianyu claim: %w", err)
	}

	var codeID int64
	var code string
	var poolSlug string
	if err := tx.QueryRowContext(ctx, `SELECT slug FROM xianyu_item_pools WHERE id = $1`, claim.PoolID).Scan(&poolSlug); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", service.ErrXianyuItemPoolNotFound
		}
		return "", fmt.Errorf("select xianyu item pool: %w", err)
	}
	err = tx.QueryRowContext(ctx, `
		SELECT id, code
		FROM redeem_codes
		WHERE type = 'xianyu_delivery'
		  AND status = 'unused'
		  AND notes = $1
		  AND (expires_at IS NULL OR expires_at > NOW())
		ORDER BY id
		FOR UPDATE SKIP LOCKED
		LIMIT 1`, service.XianyuPoolNote(poolSlug)).Scan(&codeID, &code)
	if errors.Is(err, sql.ErrNoRows) {
		return "", service.ErrXianyuInventoryEmpty
	}
	if err != nil {
		return "", fmt.Errorf("select xianyu redeem code: %w", err)
	}

	var amount any
	if claim.Amount != nil {
		amount = *claim.Amount
	}
	var productID, poolID any
	if claim.ProductID > 0 {
		productID = claim.ProductID
	}
	if claim.PoolID > 0 {
		poolID = claim.PoolID
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO xianyu_order_claims
		(order_no, redeem_code_id, account_id, item_id, buyer_id, chat_id, amount,
		 product_id, pool_id, binding_source, delivery_status, attempt_count, last_attempt_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 0, NOW())`,
		claim.OrderID, codeID, claim.AccountID, claim.ItemID, claim.BuyerID,
		claim.ChatID, amount, productID, poolID, claim.BindingSource, service.XianyuDeliveryStatusPending); err != nil {
		return "", fmt.Errorf("insert xianyu claim: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE redeem_codes
		SET status = 'used', used_by = $1, used_at = NOW()
		WHERE id = $2 AND status = 'unused'`, systemUserID, codeID)
	if err != nil {
		return "", fmt.Errorf("mark xianyu redeem code used: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		if err != nil {
			return "", fmt.Errorf("check xianyu redeem code update: %w", err)
		}
		return "", errors.New("xianyu redeem code was concurrently consumed")
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit xianyu claim: %w", err)
	}
	return code, nil
}

// RecordDeliveryResult 是唯一的回执状态入口（自动发货回执 / 补发成功与回滚都收敛于此）。
// attempt 条件隔离：回执仅作用于 attempt_count 与自身一致的记录（attempt=0 匹配初始自动发货，
// attempt=N 匹配第 N 次补发）；旧代次回执不改变新代次状态。对 sent/legacy 终态或 attempt 不匹配
// 的迟到回执按幂等忽略（2xx ack），避免触发回传重试循环；冲突信息经结构化告警输出。
func (r *xianyuOrderClaimStateRepository) RecordDeliveryResult(ctx context.Context, result service.XianyuDeliveryStatusResult) error {
	nextStatus := service.XianyuDeliveryStatusPending
	if result.Success && result.Confirmed {
		nextStatus = service.XianyuDeliveryStatusSent
	} else if !result.Success {
		nextStatus = service.XianyuDeliveryStatusFailed
	}
	var errMsg *string
	if result.Error != nil && strings.TrimSpace(*result.Error) != "" {
		trimmed := strings.TrimSpace(*result.Error)
		errMsg = &trimmed
	}
	// 未知回执（Success=true, Confirmed=false → pending）只允许作用于 pending，
	// 不得把明确 failed 复活为 pending（破坏明确失败状态单调性）。
	fromStatuses := []string{service.XianyuDeliveryStatusPending, service.XianyuDeliveryStatusFailed}
	if nextStatus == service.XianyuDeliveryStatusPending {
		fromStatuses = []string{service.XianyuDeliveryStatusPending}
	}
	spec := claimTransitionSpec{
		orderNo:            result.OrderNo,
		toStatus:           nextStatus,
		fromStatuses:       fromStatuses,
		expectedAttempt:    result.Attempt,
		requireAttempt:     true,
		setError:           errMsg,
		clearError:         errMsg == nil,
		touchLastAttempt:   true,
		idempotentStatuses: []string{nextStatus, service.XianyuDeliveryStatusSent, service.XianyuDeliveryStatusLegacyUnverified},
	}
	outcome, rb, err := r.applyClaimTransition(ctx, spec)
	if err != nil {
		return err
	}
	switch outcome {
	case claimTransitionApplied, claimTransitionIdempotent:
		return nil
	case claimTransitionNotFound:
		return service.ErrXianyuDeliveryClaimNotFound
	default: // conflict：旧回执/状态终态不匹配，按隔离要求忽略，记录告警。
		slog.Warn("xianyu delivery result ignored due to attempt/status mismatch",
			"order_no", result.OrderNo, "request_attempt", result.Attempt,
			"current_status", rb.status, "current_attempt", rb.attempt, "success", result.Success)
		return nil
	}
}

func (r *xianyuOrderClaimStateRepository) GetDeliveryClaim(ctx context.Context, orderNo string) (*service.XianyuOrderClaim, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("xianyu claim database is unavailable")
	}
	var c service.XianyuOrderClaim
	var amount, deliveryError sql.NullString
	var productID, poolID sql.NullInt64
	var bindingSource sql.NullString
	var lastAttemptAt sql.NullTime
	err := r.db.QueryRowContext(ctx, `
		SELECT c.order_no, r.code, c.account_id, c.item_id, c.buyer_id, c.chat_id, c.amount,
		       c.product_id, c.pool_id, c.binding_source, c.delivery_status, c.delivery_error,
		       c.attempt_count, c.last_attempt_at, c.created_at
		FROM xianyu_order_claims c
		JOIN redeem_codes r ON r.id = c.redeem_code_id
		WHERE c.order_no = $1`, orderNo).
		Scan(&c.OrderNo, &c.Code, &c.AccountID, &c.ItemID, &c.BuyerID, &c.ChatID, &amount,
			&productID, &poolID, &bindingSource, &c.DeliveryStatus, &deliveryError,
			&c.AttemptCount, &lastAttemptAt, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrXianyuDeliveryClaimNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get xianyu delivery claim: %w", err)
	}
	if amount.Valid {
		c.Amount = &amount.String
	}
	if productID.Valid {
		c.ProductID = &productID.Int64
	}
	if poolID.Valid {
		c.PoolID = &poolID.Int64
	}
	if bindingSource.Valid {
		c.BindingSource = &bindingSource.String
	}
	if deliveryError.Valid {
		c.DeliveryError = &deliveryError.String
	}
	if lastAttemptAt.Valid {
		c.LastAttemptAt = &lastAttemptAt.Time
	}
	return &c, nil
}

// ResendOriginalCode 人工补发原码：failed → pending，attempt_count+1（返回递增后代次供回执关联）。
// 同事务预读 + CAS（attempt 与 status 都作为条件），避免盲写任意状态。
func (r *xianyuOrderClaimStateRepository) ResendOriginalCode(ctx context.Context, orderNo string) (string, int, error) {
	if r == nil || r.db == nil {
		return "", 0, errors.New("xianyu claim database is unavailable")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, fmt.Errorf("begin xianyu resend transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := lockXianyuOrder(ctx, tx, orderNo); err != nil {
		return "", 0, err
	}

	var currentStatus string
	var code string
	var curAttempt int
	err = tx.QueryRowContext(ctx, `
		SELECT c.delivery_status, r.code, c.attempt_count
		FROM xianyu_order_claims c
		JOIN redeem_codes r ON r.id = c.redeem_code_id
		WHERE c.order_no = $1`, orderNo).Scan(&currentStatus, &code, &curAttempt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, service.ErrXianyuDeliveryClaimNotFound
	}
	if err != nil {
		return "", 0, fmt.Errorf("lookup xianyu resend claim: %w", err)
	}

	switch currentStatus {
	case service.XianyuDeliveryStatusFailed:
		// 允许人工补发原码：从 failed 回到 pending，等待 Worker 重发。
	case service.XianyuDeliveryStatusPending:
		return "", 0, service.ErrXianyuResendNotPending
	default:
		return "", 0, service.ErrXianyuDeliveryAlreadySent
	}

	spec := claimTransitionSpec{
		orderNo:          orderNo,
		toStatus:         service.XianyuDeliveryStatusPending,
		fromStatuses:     []string{service.XianyuDeliveryStatusFailed},
		expectedAttempt:  curAttempt,
		requireAttempt:   true,
		bumpAttempt:      true,
		clearError:       true,
		touchLastAttempt: true,
	}
	outcome, rb, err := r.applyClaimTransitionTx(ctx, tx, spec)
	if err != nil {
		return "", 0, err
	}
	switch outcome {
	case claimTransitionApplied:
		// 返回递增后的 attempt 代次（旧 attempt 回执不得改变新状态）。
		newAttempt := curAttempt + 1
		if err := tx.Commit(); err != nil {
			return "", 0, fmt.Errorf("commit xianyu resend: %w", err)
		}
		return code, newAttempt, nil
	case claimTransitionNotFound:
		return "", 0, service.ErrXianyuDeliveryClaimNotFound
	default:
		if rb != nil && rb.status == service.XianyuDeliveryStatusPending {
			return "", 0, service.ErrXianyuResendNotPending
		}
		return "", 0, service.ErrXianyuDeliveryAlreadySent
	}
}
