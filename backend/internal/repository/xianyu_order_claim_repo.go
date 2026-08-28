package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type xianyuOrderClaimRepository struct {
	db *sql.DB
}

func NewXianyuOrderClaimRepository(db *sql.DB) service.XianyuDeliveryRepository {
	return &xianyuOrderClaimRepository{db: db}
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
	err = tx.QueryRowContext(ctx, `
		SELECT id, code
		FROM redeem_codes
		WHERE type = 'xianyu_delivery'
		  AND status = 'unused'
		  AND notes = $1
		  AND (expires_at IS NULL OR expires_at > NOW())
		ORDER BY id
		FOR UPDATE SKIP LOCKED
		LIMIT 1`, service.XianyuPoolNote(claim.Pool)).Scan(&codeID, &code)
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
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO xianyu_order_claims
		(order_no, redeem_code_id, account_id, item_id, buyer_id, amount)
		VALUES ($1, $2, $3, $4, $5, $6)`, claim.OrderID, codeID, claim.AccountID, claim.ItemID, claim.BuyerID, amount); err != nil {
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
