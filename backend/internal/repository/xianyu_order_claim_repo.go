package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
		SELECT c.order_no, r.code, c.account_id, c.item_id, c.buyer_id, c.amount,
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
		if err := rows.Scan(&c.OrderNo, &c.Code, &c.AccountID, &c.ItemID, &c.BuyerID, &amount,
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
		(order_no, redeem_code_id, account_id, item_id, buyer_id, amount,
		 product_id, pool_id, binding_source, delivery_status, attempt_count, last_attempt_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 1, NOW())`,
		claim.OrderID, codeID, claim.AccountID, claim.ItemID, claim.BuyerID, amount,
		productID, poolID, claim.BindingSource, service.XianyuDeliveryStatusPending); err != nil {
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

func (r *xianyuOrderClaimStateRepository) RecordDeliveryResult(ctx context.Context, result service.XianyuDeliveryStatusResult) error {
	if r == nil || r.db == nil {
		return errors.New("xianyu claim database is unavailable")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin xianyu delivery result transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1)::bigint)`, result.OrderNo); err != nil {
		return fmt.Errorf("lock xianyu delivery order: %w", err)
	}

	var currentStatus string
	err = tx.QueryRowContext(ctx, `SELECT delivery_status FROM xianyu_order_claims WHERE order_no = $1`, result.OrderNo).Scan(&currentStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrXianyuDeliveryClaimNotFound
	}
	if err != nil {
		return fmt.Errorf("lookup xianyu delivery claim: %w", err)
	}

	switch currentStatus {
	case service.XianyuDeliveryStatusSent:
		// sent 为终态；旧失败结果不能覆盖 sent。
		return service.ErrXianyuDeliveryAlreadySent
	case service.XianyuDeliveryStatusLegacyUnverified:
		return service.ErrXianyuDeliveryAlreadySent
	}

	nextStatus := service.XianyuDeliveryStatusPending
	if result.Success {
		if result.Confirmed {
			nextStatus = service.XianyuDeliveryStatusSent
		} else {
			// 返回成功但无法确认最终回执时保持 pending。
			nextStatus = service.XianyuDeliveryStatusPending
		}
	} else {
		nextStatus = service.XianyuDeliveryStatusFailed
	}

	var errMsg any
	if result.Error != nil && strings.TrimSpace(*result.Error) != "" {
		errMsg = strings.TrimSpace(*result.Error)
	} else {
		errMsg = nil
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE xianyu_order_claims
		SET delivery_status = $2, delivery_error = $3, last_attempt_at = NOW(), updated_at = NOW()
		WHERE order_no = $1`, result.OrderNo, nextStatus, errMsg)
	if err != nil {
		return fmt.Errorf("update xianyu delivery result: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit xianyu delivery result: %w", err)
	}
	return nil
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
		SELECT c.order_no, r.code, c.account_id, c.item_id, c.buyer_id, c.amount,
		       c.product_id, c.pool_id, c.binding_source, c.delivery_status, c.delivery_error,
		       c.attempt_count, c.last_attempt_at, c.created_at
		FROM xianyu_order_claims c
		JOIN redeem_codes r ON r.id = c.redeem_code_id
		WHERE c.order_no = $1`, orderNo).
		Scan(&c.OrderNo, &c.Code, &c.AccountID, &c.ItemID, &c.BuyerID, &amount,
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

func (r *xianyuOrderClaimStateRepository) ResendOriginalCode(ctx context.Context, orderNo string, systemUserID int64) (string, error) {
	if r == nil || r.db == nil {
		return "", errors.New("xianyu claim database is unavailable")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin xianyu resend transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1)::bigint)`, orderNo); err != nil {
		return "", fmt.Errorf("lock xianyu resend order: %w", err)
	}

	var currentStatus string
	var code string
	err = tx.QueryRowContext(ctx, `
		SELECT c.delivery_status, r.code
		FROM xianyu_order_claims c
		JOIN redeem_codes r ON r.id = c.redeem_code_id
		WHERE c.order_no = $1`, orderNo).Scan(&currentStatus, &code)
	if errors.Is(err, sql.ErrNoRows) {
		return "", service.ErrXianyuDeliveryClaimNotFound
	}
	if err != nil {
		return "", fmt.Errorf("lookup xianyu resend claim: %w", err)
	}

	switch currentStatus {
	case service.XianyuDeliveryStatusFailed:
		// 允许人工补发原码：从 failed 回到 pending，等待 Worker 重发。
	case service.XianyuDeliveryStatusPending:
		return "", service.ErrXianyuResendNotPending
	default:
		return "", service.ErrXianyuDeliveryAlreadySent
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE xianyu_order_claims
		SET delivery_status = $2, delivery_error = NULL,
		    attempt_count = attempt_count + 1, last_attempt_at = NOW(), updated_at = NOW()
		WHERE order_no = $1`, orderNo, service.XianyuDeliveryStatusPending)
	if err != nil {
		return "", fmt.Errorf("reset xianyu resend claim: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit xianyu resend: %w", err)
	}
	return code, nil
}
