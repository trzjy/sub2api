package repository

// E12 定向测试（repository 层）：
//   - #1 候选粗筛 keyset 分页扫描：前 200 个含 model_rate_limits 的账号均非 TokenHarbor
//     免费档合格候选时，第 201 个合格账号仍能被收集到（分页闭合，旧 LIMIT 前置会永久遗漏）；
//   - #4 meta 损坏失败关闭：last_event_at 非法时 GetModelRateLimitMeta 返回明确错误，
//     写入口沿错误中止本次读改写、不提交；空 meta/"null"/空 last_event_at 正常路径不变。

import (
	"context"
	"database/sql/driver"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

// e12AccountRows 按 ent accounts 列序构造测试行；id 顺序即 ent 返回顺序。
func e12AccountRows(ids []int64, extra string) *sqlmock.Rows {
	now := time.Now()
	rows := sqlmock.NewRows(dbaccount.Columns)
	for _, id := range ids {
		rows.AddRow(
			id, now, now, nil, "test", nil, service.PlatformOpenAI, "", service.AccountTypeAPIKey,
			[]byte(`{"api_key":"sk-test"}`), nil, nil, []byte(extra), nil, nil, 1, nil, 1, 1.0,
			service.StatusActive, nil, nil, nil, false, true, nil, nil, nil, nil, nil, nil,
			nil, nil, nil, service.QuotaDimensionGlobal,
		)
	}
	return rows
}

// TestE12ListTokenHarborModelRateLimitedAccounts_PaginatesPastUnqualifiedBatch 验证
// E12 #1：旧实现 `... extra ? $1 LIMIT 200` 只取前 200 个含键账号，若它们都不是
// TokenHarbor 免费档合格候选，则其后合格账号被永久遗漏。keyset 分页必须继续向后扫描，
// 直到收集满 limit 个合格候选或扫描耗尽。
func TestE12ListTokenHarborModelRateLimitedAccounts_PaginatesPastUnqualifiedBatch(t *testing.T) {
	now := time.Now()

	// 非 TokenHarbor 合格（reason 前缀不符）：即便 reset_at 在未来也不合格。
	unqualifiedExtra := `{"model_rate_limits":{"m":{"reason":"other-cause","rate_limit_reset_at":"2999-01-01T00:00:00Z"}}}`
	// TokenHarbor 合格候选（reason 前缀 + 精确恢复标记）。
	qualifiedExtra := `{"model_rate_limits":{"deepseek:free":{"reason":"tokenharbor_free_tier_exhausted:x","rate_limit_reset_at":"2999-01-01T00:00:00Z","precise_reset":true}}}`

	firstBatch := make([]int64, 0, tokenHarborProbeScanBatchSize)
	for i := int64(1); i <= int64(tokenHarborProbeScanBatchSize); i++ {
		firstBatch = append(firstBatch, i)
	}
	qualifiedID := int64(tokenHarborProbeScanBatchSize) + 1

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })
	repo := newAccountRepositoryWithSQL(client, db, nil)

	// 第一批：满批的非合格账号 → 必须继续 keyset 向后扫描。
	idRows1 := sqlmock.NewRows([]string{"id"})
	for _, id := range firstBatch {
		idRows1.AddRow(id)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM accounts")).WillReturnRows(idRows1)
	mock.ExpectQuery(regexp.QuoteMeta(`FROM "accounts"`)).
		WillReturnRows(e12AccountRows(firstBatch, unqualifiedExtra))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM "account_groups"`)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	// 第二批：仅一个合格账号（不足批大小 → 扫描耗尽）。
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM accounts")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(qualifiedID))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM "accounts"`)).
		WillReturnRows(e12AccountRows([]int64{qualifiedID}, qualifiedExtra))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM "account_groups"`)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	got, err := repo.ListTokenHarborModelRateLimitedAccounts(context.Background(), now, 1)
	require.NoError(t, err)
	require.Len(t, got, 1, "靠后批次的合格候选必须被收集（分页闭合）")
	require.Equal(t, qualifiedID, got[0].ID)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestE12ListTokenHarborModelRateLimitedAccounts_NoQualifiedCandidateScanExhausts 验证
// 扫描耗尽路径：全部账号均非合格候选时返回空且不越界。
func TestE12ListTokenHarborModelRateLimitedAccounts_NoQualifiedCandidateScanExhausts(t *testing.T) {
	now := time.Now()
	unqualifiedExtra := `{"model_rate_limits":{"m":{"reason":"other-cause","rate_limit_reset_at":"2999-01-01T00:00:00Z"}}}`

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })
	repo := newAccountRepositoryWithSQL(client, db, nil)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM accounts")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)).AddRow(int64(2)))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM "accounts"`)).
		WillReturnRows(e12AccountRows([]int64{1, 2}, unqualifiedExtra))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM "account_groups"`)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	got, err := repo.ListTokenHarborModelRateLimitedAccounts(context.Background(), now, 5)
	require.NoError(t, err)
	require.Empty(t, got)
	require.NoError(t, mock.ExpectationsWereMet())
}

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
