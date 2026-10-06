package repository

// E12 定向测试（repository 层）：
//   - #4 meta 损坏失败关闭：last_event_at 非法时 GetModelRateLimitMeta 返回明确错误，
//     写入口沿错误中止本次读改写、不提交；空 meta/"null"/空 last_event_at 正常路径不变。
//
// 退役说明（D-QL-003B/E）：TokenHarbor 免费层候选粗筛方法已随免费层探针链
// 退役整体删除，原 #1/#2 分页语义用例与本文件原「合格/非合格候选」用例一并移除。

import (
	"context"
	"database/sql/driver"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// TestE12GetModelRateLimitMeta_CorruptLastEventAtFailsClosed 验证 E12 #4：meta
// last_event_at 非法时返回明确错误（不得静默当"无 meta"以 rev=0 应用旧事件）。
func TestE12GetModelRateLimitMeta_CorruptLastEventAtFailsClosed(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectQuery(regexp.QuoteMeta("SELECT extra->'model_rate_limits_meta'->$1")).
		WithArgs("deepseek:free", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"extra"}).AddRow([]byte(`{"last_event_at":"not-a-time","revision":3}`)))

	repo := newAccountRepositoryWithSQL(nil, db, nil)
	_, _, ok, err := repo.GetModelRateLimitMeta(context.Background(), 42, "deepseek:free")
	require.Error(t, err, "损坏 meta 必须失败关闭，不得静默返回 ok=false")
	require.False(t, ok)
	require.Contains(t, err.Error(), "last_event_at")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestE12SetModelRateLimitWithPreciseReset_CorruptMetaAbortsWithoutWrite 验证 E12 #4：
// 写入口裁决读取遇到损坏 meta 时沿错误中止本次读改写，不发 UPDATE（不重置 revision）。
func TestE12SetModelRateLimitWithPreciseReset_CorruptMetaAbortsWithoutWrite(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectQuery(regexp.QuoteMeta("SELECT extra->'model_rate_limits_meta'->$1")).
		WithArgs("deepseek:free", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"extra"}).AddRow([]byte(`{"last_event_at":"!!!corrupt","revision":7}`)))
	// 无 UPDATE / outbox 期望：损坏 meta 必须在提交前中止。

	repo := newAccountRepositoryWithSQL(nil, db, nil)
	err = repo.SetModelRateLimitWithPreciseReset(context.Background(), 42, "deepseek:free", time.Now().Add(time.Hour), true, "reason")
	require.Error(t, err)
	require.Contains(t, err.Error(), "last_event_at")
	require.NoError(t, mock.ExpectationsWereMet(), "损坏 meta 时不得发出任何写语句")
}

// TestE12GetModelRateLimitMeta_NormalAbsentPathUnchanged 验证 E12 #4 的"正常不存在"
// 路径不变：空值 / 字面 "null" / 空 last_event_at 仍返回 (zero,0,false,nil)。
func TestE12GetModelRateLimitMeta_NormalAbsentPathUnchanged(t *testing.T) {
	cases := []struct {
		name string
		row  driver.Value
	}{
		{name: "empty_bytes", row: []byte{}},
		{name: "json_null", row: []byte("null")},
		{name: "empty_last_event_at", row: []byte(`{"last_event_at":"","revision":0}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			mock.ExpectQuery(regexp.QuoteMeta("SELECT extra->'model_rate_limits_meta'->$1")).
				WithArgs("deepseek:free", int64(42)).
				WillReturnRows(sqlmock.NewRows([]string{"extra"}).AddRow(tc.row))
			repo := newAccountRepositoryWithSQL(nil, db, nil)
			_, _, ok, err := repo.GetModelRateLimitMeta(context.Background(), 42, "deepseek:free")
			require.NoError(t, err, "正常不存在路径不得报错")
			require.False(t, ok)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
