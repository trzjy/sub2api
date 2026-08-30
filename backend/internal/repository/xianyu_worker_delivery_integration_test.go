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

	// 明确成功回执 → sent，quantity_sent = quantity。
	err = repo.RecordWorkerDeliveryResult(ctx, "wd-1", service.XianyuDeliveryStatusResult{
		OrderNo: "wd-1", Success: true, Confirmed: true,
	})
	require.NoError(t, err)
	list, _, _ = repo.ListWorkerDeliveries(ctx, service.XianyuDeliveryFilter{Limit: 10})
	require.Equal(t, "sent", list[0].DeliveryStatus)
	require.Equal(t, 3, list[0].QuantitySent)

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
