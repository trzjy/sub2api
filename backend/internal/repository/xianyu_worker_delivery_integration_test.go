//go:build integration

package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestXianyuWorkerDeliveryRecordEnsureIdempotentAndResultTransitions(t *testing.T) {
	ctx := context.Background()
	db := integrationDB

	_, err := db.ExecContext(ctx, `DELETE FROM xianyu_worker_deliveries`)
	require.NoError(t, err)

	repo := NewXianyuWorkerDeliveryRepository(db)

	// Ensure：首次创建 pending 记录。
	err = repo.EnsureWorkerDeliveryRecord(ctx, service.XianyuWorkerDelivery{
		OrderNo: "wd-1", DeliveryKind: "auto", Quantity: 2,
	})
	require.NoError(t, err)

	// Ensure 幂等：重复调用不重置状态；更大的 quantity 合并。
	err = repo.EnsureWorkerDeliveryRecord(ctx, service.XianyuWorkerDelivery{
		OrderNo: "wd-1", DeliveryKind: "redelivery", Quantity: 3,
	})
	require.NoError(t, err)

	// 未知回执（Success=true, Confirmed=false）→ 保持 pending。
	err = repo.RecordWorkerDeliveryResult(ctx, "wd-1", service.XianyuDeliveryStatusResult{
		OrderNo: "wd-1", Success: true, Confirmed: false,
	})
	require.NoError(t, err)

	list, total, err := repo.ListWorkerDeliveries(ctx, service.XianyuDeliveryFilter{Limit: 10})
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, list, 1)
	require.Equal(t, "pending", list[0].DeliveryStatus)
	require.Equal(t, 3, list[0].Quantity, "quantity should merge to larger value")

	// 明确成功回执但未回传 quantity_sent（=0）→ sent，quantity_sent 字段不写入，
	// 保留主程序默认值 0（不再等于 quantity；quantity_sent 只由 Worker 实发份数决定）。
	err = repo.RecordWorkerDeliveryResult(ctx, "wd-1", service.XianyuDeliveryStatusResult{
		OrderNo: "wd-1", Success: true, Confirmed: true,
	})
	require.NoError(t, err)
	list, _, _ = repo.ListWorkerDeliveries(ctx, service.XianyuDeliveryFilter{Limit: 10})
	require.Equal(t, "sent", list[0].DeliveryStatus)
	require.Equal(t, 0, list[0].QuantitySent, "未回传 quantity_sent 时保留默认 0")

	// 终态不降级：已 sent 的记录不被未知回执改回 pending。
	err = repo.RecordWorkerDeliveryResult(ctx, "wd-1", service.XianyuDeliveryStatusResult{
		OrderNo: "wd-1", Success: true, Confirmed: false,
	})
	require.NoError(t, err)
	list, _, _ = repo.ListWorkerDeliveries(ctx, service.XianyuDeliveryFilter{Limit: 10})
	require.Equal(t, "sent", list[0].DeliveryStatus, "sent terminal state must not be downgraded")

	// 另一订单：明确失败回执 → failed + 错误信息。
	err = repo.EnsureWorkerDeliveryRecord(ctx, service.XianyuWorkerDelivery{
		OrderNo: "wd-2", DeliveryKind: "auto", Quantity: 1,
	})
	require.NoError(t, err)
	reason := "CSI_FORBID blocked"
	err = repo.RecordWorkerDeliveryResult(ctx, "wd-2", service.XianyuDeliveryStatusResult{
		OrderNo: "wd-2", Success: false, Error: &reason,
	})
	require.NoError(t, err)

	// 不存在的订单 → ErrXianyuDeliveryClaimNotFound（供 delivery-results 端点路由判断）。
	err = repo.RecordWorkerDeliveryResult(ctx, "no-such-order", service.XianyuDeliveryStatusResult{
		OrderNo: "no-such-order", Success: true, Confirmed: true,
	})
	require.ErrorIs(t, err, service.ErrXianyuDeliveryClaimNotFound)
	require.True(t, errors.Is(err, service.ErrXianyuDeliveryClaimNotFound))

	// 状态筛选。
	failedList, _, err := repo.ListWorkerDeliveries(ctx, service.XianyuDeliveryFilter{Status: "failed", Limit: 10})
	require.NoError(t, err)
	require.Len(t, failedList, 1)
	require.Equal(t, "wd-2", failedList[0].OrderNo)
}

// TestXianyuWorkerDeliveryQuantitySentUpperBoundEnforcedBySQL 用真实 PostgreSQL 证明
// quantity_sent 上限是 SQL 原子约束，而不是应用层事后补救。
//
// 之所以必须跑真实库：sqlmock 的"零行受影响"是人为编排的返回值，
// 无法证明 WHERE 表达式真的把越界参数挡在外面。旧写法
// `AND quantity_sent <= quantity` 在 UPDATE 的 WHERE 里读的是列的旧值（0），
// 恒成立，在真实库上会把越界值写进去——这个测试就是抓这个的。
func TestXianyuWorkerDeliveryQuantitySentUpperBoundEnforcedBySQL(t *testing.T) {
	ctx := context.Background()
	db := integrationDB

	_, err := db.ExecContext(ctx, `DELETE FROM xianyu_worker_deliveries`)
	require.NoError(t, err)

	repo := NewXianyuWorkerDeliveryRepository(db)

	// quantity=1 的订单。
	require.NoError(t, repo.EnsureWorkerDeliveryRecord(ctx, service.XianyuWorkerDelivery{
		OrderNo: "wd-bound", DeliveryKind: "auto", Quantity: 1,
	}))

	// 越界回传 quantity_sent=3 > quantity=1：必须零行更新并返回越界错误。
	err = repo.RecordWorkerDeliveryResult(ctx, "wd-bound", service.XianyuDeliveryStatusResult{
		OrderNo: "wd-bound", Success: true, Confirmed: true, QuantitySent: 3,
	})
	require.Error(t, err)
	require.ErrorIs(t, err, service.ErrXianyuDeliveryQuantitySentOutOfRange)

	// 数据库行必须完全没被改动：仍是 pending，quantity_sent 仍是 0。
	var status string
	var quantitySent int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT delivery_status, quantity_sent FROM xianyu_worker_deliveries WHERE order_no = $1`,
		"wd-bound").Scan(&status, &quantitySent))
	require.Equal(t, "pending", status, "越界回传不得推进状态")
	require.Equal(t, 0, quantitySent, "越界回传不得写入 quantity_sent")

	// 边界内（quantity_sent == quantity）必须成功写入。
	require.NoError(t, repo.RecordWorkerDeliveryResult(ctx, "wd-bound", service.XianyuDeliveryStatusResult{
		OrderNo: "wd-bound", Success: true, Confirmed: true, QuantitySent: 1,
	}))
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT delivery_status, quantity_sent FROM xianyu_worker_deliveries WHERE order_no = $1`,
		"wd-bound").Scan(&status, &quantitySent))
	require.Equal(t, "sent", status)
	require.Equal(t, 1, quantitySent)

	// 多数量订单部分成功：quantity=3, quantity_sent=1 必须写入（部分发货语义）。
	require.NoError(t, repo.EnsureWorkerDeliveryRecord(ctx, service.XianyuWorkerDelivery{
		OrderNo: "wd-partial", DeliveryKind: "auto", Quantity: 3,
	}))
	require.NoError(t, repo.RecordWorkerDeliveryResult(ctx, "wd-partial", service.XianyuDeliveryStatusResult{
		OrderNo: "wd-partial", Success: true, Confirmed: true, QuantitySent: 1,
	}))
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT delivery_status, quantity_sent FROM xianyu_worker_deliveries WHERE order_no = $1`,
		"wd-partial").Scan(&status, &quantitySent))
	require.Equal(t, "sent", status)
	require.Equal(t, 1, quantitySent)
}
