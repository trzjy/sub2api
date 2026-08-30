package repository

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestRecordWorkerDeliveryResultRejectsOutOfRangeQuantitySent 覆盖修复项 G：
// 0 <= quantity_sent <= quantity 原子校验。
// 场景：订单 quantity=1，回传 quantity_sent=3 必须返回明确错误，
// 数据库行 quantity_sent 保持原值。
func TestRecordWorkerDeliveryResultRejectsOutOfRangeQuantitySent(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	repo := &xianyuWorkerDeliveryRepository{db: db}

	// 1) UPDATE 尝试写入 quantity_sent=3，因 quantity_sent > quantity 不命中任何行（0 rows affected）
	// 参数顺序按 SQL 中 $N 出现顺序：$1=delivery_status, $2=order_no, $3=quantity_sent
	mock.ExpectExec(regexp.QuoteMeta("UPDATE xianyu_worker_deliveries")).
		WithArgs("sent", "order-qs", 3).
		WillReturnResult(sqlmock.NewResult(0, 0))

	// 2) 探测"记录是否存在"
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM xianyu_worker_deliveries")).
		WithArgs("order-qs").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	// 3) 探测"当前 quantity"以判断是否越界
	mock.ExpectQuery(regexp.QuoteMeta("SELECT quantity FROM xianyu_worker_deliveries")).
		WithArgs("order-qs").
		WillReturnRows(sqlmock.NewRows([]string{"quantity"}).AddRow(1))

	err = repo.RecordWorkerDeliveryResult(context.Background(), "order-qs", service.XianyuDeliveryStatusResult{
		OrderNo:      "order-qs",
		Success:      true,
		Confirmed:    true,
		QuantitySent: 3,
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, service.ErrXianyuDeliveryQuantitySentOutOfRange),
		"expected ErrXianyuDeliveryQuantitySentOutOfRange, got %v", err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestRecordWorkerDeliveryResultAllowsQuantitySentWithinRange:
// 场景：quantity=3, quantity_sent=2 必须成功。
func TestRecordWorkerDeliveryResultAllowsQuantitySentWithinRange(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	repo := &xianyuWorkerDeliveryRepository{db: db}

	mock.ExpectExec(regexp.QuoteMeta("UPDATE xianyu_worker_deliveries")).
		WithArgs("sent", "order-qs-ok", 2).
		WillReturnResult(sqlmock.NewResult(0, 1))

	require.NoError(t, repo.RecordWorkerDeliveryResult(context.Background(), "order-qs-ok", service.XianyuDeliveryStatusResult{
		OrderNo:      "order-qs-ok",
		Success:      true,
		Confirmed:    true,
		QuantitySent: 2,
	}))
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestRecordWorkerDeliveryResultIgnoresQuantitySentForFailedStatus:
// 场景：非 sent 状态（failed/pending）quantity_sent 不写入，
// 即使上游传 99 也不报错（quantity_sent 字段保持原值 0）。
func TestRecordWorkerDeliveryResultIgnoresQuantitySentForFailedStatus(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	repo := &xianyuWorkerDeliveryRepository{db: db}

	// failed 状态不写 quantity_sent；SQL 不带 quantity_sent 约束
	mock.ExpectExec(regexp.QuoteMeta("UPDATE xianyu_worker_deliveries")).
		WithArgs("failed", "order-qs-fail", "pre-send fail").
		WillReturnResult(sqlmock.NewResult(0, 1))

	require.NoError(t, repo.RecordWorkerDeliveryResult(context.Background(), "order-qs-fail", service.XianyuDeliveryStatusResult{
		OrderNo:      "order-qs-fail",
		Success:      false,
		Confirmed:    false,
		Error:        strPtr("pre-send fail"),
		QuantitySent: 99, // 任何值都应被忽略
	}))
	require.NoError(t, mock.ExpectationsWereMet())
}

func strPtr(s string) *string { return &s }

// 确保导入 sql（防止未使用警告，sqlmock 的 db 是 *sql.DB）
var _ = (*sql.DB)(nil)
