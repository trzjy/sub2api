package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// xianyuWorkerDeliveryRepository 实现 service.XianyuWorkerDeliveryRepository。
type xianyuWorkerDeliveryRepository struct {
	db *sql.DB
}

// NewXianyuWorkerDeliveryRepository 创建 Worker 发货记录仓库。
func NewXianyuWorkerDeliveryRepository(db *sql.DB) service.XianyuWorkerDeliveryRepository {
	return &xianyuWorkerDeliveryRepository{db: db}
}

// EnsureWorkerDeliveryRecord 按 order_no 幂等创建/合并 Worker 发货记录。
func (r *xianyuWorkerDeliveryRepository) EnsureWorkerDeliveryRecord(ctx context.Context, d service.XianyuWorkerDelivery) error {
	if r == nil || r.db == nil {
		return errors.New("xianyu worker delivery database is unavailable")
	}
	// 幂等：不存在则插入（pending）；已存在不覆盖状态/错误，仅当传入 quantity 更大时更新，
	// 避免请求重试重复创建或把已更新状态重置回初始。
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO xianyu_worker_deliveries (order_no, delivery_kind, quantity, quantity_sent, delivery_status)
		VALUES ($1, $2, $3, 0, 'pending')
		ON CONFLICT (order_no) DO UPDATE SET
			quantity = CASE WHEN EXCLUDED.quantity > xianyu_worker_deliveries.quantity
				THEN EXCLUDED.quantity ELSE xianyu_worker_deliveries.quantity END,
			updated_at = NOW()`,
		d.OrderNo, d.DeliveryKind, d.Quantity)
	if err != nil {
		return fmt.Errorf("ensure xianyu worker delivery record: %w", err)
	}
	return nil
}

// RecordWorkerDeliveryResult 按 order_no 更新 Worker 发货记录状态（与 xianyu_order_claims 语义对齐）。
func (r *xianyuWorkerDeliveryRepository) RecordWorkerDeliveryResult(ctx context.Context, orderNo string, result service.XianyuDeliveryStatusResult) error {
	if r == nil || r.db == nil {
		return errors.New("xianyu worker delivery database is unavailable")
	}
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

	// quantity_sent 原子校验：必须 0 <= quantity_sent <= quantity。
	// SQL 表达式在 WHERE 子句做硬性约束，超限不命中任何行；然后由下方
	// "无行命中" 分支区分"记录不存在 / 已是终态 / quantity_sent 越界"三类情况。
	var quantitySentArg interface{}
	needQuantitySentCheck := false
	if nextStatus == service.XianyuDeliveryStatusSent && result.QuantitySent > 0 {
		quantitySentArg = result.QuantitySent
		needQuantitySentCheck = true
	}

	// 状态单调性：sent/failed 为终态；legacy_unverified 可被明确结果覆盖；
	// unknown（Success=true, Confirmed=false → pending）仅作用于 pending，不得复活 failed。
	statusFilter := "delivery_status IN ('pending', 'legacy_unverified')"
	if nextStatus == service.XianyuDeliveryStatusPending {
		statusFilter = "delivery_status = 'pending'"
	}

	query := "UPDATE xianyu_worker_deliveries SET delivery_status = $1, updated_at = NOW()"
	args := []any{nextStatus, orderNo}
	if errMsg != nil {
		args = append(args, *errMsg)
		query += fmt.Sprintf(", delivery_error = $%d", len(args))
	} else {
		query += ", delivery_error = NULL"
	}
	// quantity_sent 仅在 sent 状态写入，且按 Worker 回传的实发份数（非 quantity）覆盖；
	// 这样支持多数量订单部分成功（quantity 保留原始购买数量，quantity_sent=实际成功份数）。
	if needQuantitySentCheck {
		args = append(args, quantitySentArg)
		query += fmt.Sprintf(", quantity_sent = $%d", len(args))
	}
	query += fmt.Sprintf(" WHERE order_no = $2 AND %s", statusFilter)
	// 原子上限：quantity_sent 写入时必须 0 <= quantity_sent <= quantity；超限不命中任何行。
	if needQuantitySentCheck {
		query += " AND quantity_sent <= quantity AND quantity_sent >= 0"
	}

	res, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("record xianyu worker delivery result: %w", err)
	}
	if affected, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("check xianyu worker delivery update: %w", err)
	} else if affected == 0 {
		// 无行命中：可能记录不存在 / 已是终态 / quantity_sent 越界。逐一区分。
		var exists bool
		if err := r.db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM xianyu_worker_deliveries WHERE order_no = $1)`, orderNo).Scan(&exists); err != nil {
			return fmt.Errorf("check xianyu worker delivery existence: %w", err)
		}
		if !exists {
			return service.ErrXianyuDeliveryClaimNotFound
		}
		// 记录存在但本次 UPDATE 未命中。区分终态（幂等忽略）与 quantity_sent 越界（明确错误）。
		if needQuantitySentCheck {
			var currentQty int
			if err := r.db.QueryRowContext(ctx,
				`SELECT quantity FROM xianyu_worker_deliveries WHERE order_no = $1`, orderNo).Scan(&currentQty); err != nil {
				return fmt.Errorf("check xianyu worker delivery quantity: %w", err)
			}
			if result.QuantitySent < 0 || result.QuantitySent > currentQty {
				return fmt.Errorf("%w: quantity_sent=%d exceeds order quantity=%d",
					service.ErrXianyuDeliveryQuantitySentOutOfRange, result.QuantitySent, currentQty)
			}
		}
		// 已存在但终态/状态不匹配：幂等忽略（2xx ack），不报错。
	}
	return nil
}

// ListWorkerDeliveries 分页列出 Worker 发货记录。
func (r *xianyuWorkerDeliveryRepository) ListWorkerDeliveries(ctx context.Context, filter service.XianyuDeliveryFilter) ([]service.XianyuWorkerDelivery, int, error) {
	if r == nil || r.db == nil {
		return nil, 0, errors.New("xianyu worker delivery database is unavailable")
	}
	where := "WHERE 1=1"
	args := make([]any, 0, 4)
	if filter.Status != "" {
		args = append(args, filter.Status)
		where += fmt.Sprintf(" AND delivery_status = $%d", len(args))
	}
	if filter.Search != "" {
		args = append(args, "%"+filter.Search+"%")
		where += fmt.Sprintf(" AND (order_no ILIKE $%d OR delivery_kind ILIKE $%d)", len(args), len(args))
	}
	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM xianyu_worker_deliveries `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count xianyu worker deliveries: %w", err)
	}
	limit := filter.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	args = append(args, limit, filter.Offset)
	rows, err := r.db.QueryContext(ctx, `
		SELECT order_no, delivery_kind, quantity, quantity_sent, delivery_status,
		       COALESCE(delivery_error, ''), created_at, updated_at
		FROM xianyu_worker_deliveries `+where+`
		ORDER BY updated_at DESC
		LIMIT $`+fmt.Sprintf("%d", len(args)-1)+` OFFSET $`+fmt.Sprintf("%d", len(args)), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list xianyu worker deliveries: %w", err)
	}
	defer rows.Close()
	var out []service.XianyuWorkerDelivery
	for rows.Next() {
		var d service.XianyuWorkerDelivery
		var errText string
		if err := rows.Scan(&d.OrderNo, &d.DeliveryKind, &d.Quantity, &d.QuantitySent,
			&d.DeliveryStatus, &errText, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, 0, fmt.Errorf("scan xianyu worker delivery: %w", err)
		}
		if errText != "" {
			d.DeliveryError = &errText
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate xianyu worker deliveries: %w", err)
	}
	return out, total, nil
}
