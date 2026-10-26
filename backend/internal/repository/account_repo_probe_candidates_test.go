package repository

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

// captureAllQueries 依序记录每个被执行的查询 SQL，便于逐个断言查询形态。
type captureAllQueries struct {
	queries *[]string
}

// Match 记录查询并总是匹配成功（配合 captureAllQueries 做纯捕获式断言）。
func (m captureAllQueries) Match(_, actual string) error {
	*m.queries = append(*m.queries, actual)
	return nil
}

// probeCandidateAccountRow 构造一行「停调中」的账号行，值序必须与 dbaccount.Columns 一致。
// schedulable 与 tempUnschedulableUntil 由调用方指定，用于区分探针候选的纳入/排除场景。
func probeCandidateAccountRow(id int64, schedulable bool, tempUnschedulableUntil *time.Time) *sqlmock.Rows {
	now := time.Now()
	return sqlmock.NewRows(dbaccount.Columns).AddRow(
		id, now, now, nil, "test", nil, "openai", "", "api-key",
		[]byte(`{"api_key":"sk-test"}`), nil, nil, []byte(`{}`), nil, nil, nil, 1, nil, 1, 1.0,
		"active", nil, nil, nil, false, schedulable, nil, nil, nil, tempUnschedulableUntil, nil,
		nil, nil, nil, nil, "global",
	)
}

// TestListTempUnschedulableAccounts_ExcludesSchedulableFalseAccount 锁定探针候选排除语义
// （派发单 C5）：schedulable=false 是管理员显式关闭调度（「不参与调度」），语义上不属于
// 「等待恢复」，不得被健康探针选为候选。
//
// 场景：DB 中有一个 schedulable=false 且停调未过期的账号。真实 DB 执行本查询生成的 SELECT
// 时，WHERE 里的 schedulable 过滤条件会把它排除，结果集不会出现该账号。本测试用 sqlmock
// 忠实模拟「过滤后的空结果集」，并断言主查询 SQL 确实携带 schedulable 过滤条件——若代码
// 回归（去掉 SchedulableEQ(true)），该断言即失败。
func TestListTempUnschedulableAccounts_ExcludesSchedulableFalseAccount(t *testing.T) {
	var queries []string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(captureAllQueries{queries: &queries}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })
	repo := newAccountRepositoryWithSQL(client, db, nil)

	// 主查询：schedulable=false 的行被 WHERE 条件排除（模拟 DB 过滤后无行返回）。
	mock.ExpectQuery("probe candidates main query").
		WillReturnRows(sqlmock.NewRows(dbaccount.Columns))

	accounts, err := repo.ListTempUnschedulableAccounts(context.Background(), time.Now(), 10)
	require.NoError(t, err)
	require.Empty(t, accounts, "schedulable=false（管理员手动关闭调度）的账号不得出现在探针候选结果中")
	require.NoError(t, mock.ExpectationsWereMet())

	// 主查询是唯一执行的查询，其 WHERE 必须包含 schedulable 过滤条件。
	require.Len(t, queries, 1)
	mainSQL := normalizeSQLWhitespace(queries[0])
	_, whereClause, found := strings.Cut(mainSQL, " WHERE ")
	require.True(t, found, "expected WHERE clause in main query: %s", mainSQL)
	whereClause, _, _ = strings.Cut(whereClause, " ORDER BY ")
	require.Contains(t, whereClause, `"accounts"."schedulable"`,
		"探针候选主查询必须携带 schedulable 过滤条件（ent 对 SchedulableEQ(true) 生成布尔列直接引用），否则手动关闭调度的账号会被无谓探测")
	require.Contains(t, whereClause, `"accounts"."temp_unschedulable_until" IS NOT NULL`,
		"仍须限定停调中（until 非空）")
	require.Contains(t, whereClause, `"accounts"."temp_unschedulable_until" > $`,
		"仍须限定停调未过期（until > now）")
	require.Contains(t, whereClause, `"accounts"."deleted_at" IS NULL`,
		"仍须排除软删除账号")
	_, orderClause, hasOrder := strings.Cut(mainSQL, " ORDER BY ")
	require.True(t, hasOrder, "candidates must be ordered by expiry: %s", mainSQL)
	require.Contains(t, orderClause, `"temp_unschedulable_until" ASC`,
		"候选按停调最早到期升序取出")
}

// TestListTempUnschedulableAccounts_IncludesSchedulableTrueAccount 锁定正向场景：
// schedulable=true 且停调未过期的账号必须出现在探针候选结果中（健康的恢复探针对象）。
func TestListTempUnschedulableAccounts_IncludesSchedulableTrueAccount(t *testing.T) {
	var queries []string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(captureAllQueries{queries: &queries}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })
	repo := newAccountRepositoryWithSQL(client, db, nil)

	until := time.Now().Add(5 * time.Minute)

	// 主查询返回一行 schedulable=true、停调未过期的账号（该行能通过全部 WHERE 条件）。
	mock.ExpectQuery("probe candidates main query").
		WillReturnRows(probeCandidateAccountRow(int64(3001), true, &until))
	// accountsToService 会加载该账号的 account_groups 关联（返回空）。
	mock.ExpectQuery("account groups load").
		WillReturnRows(sqlmock.NewRows(nil))

	got, err := repo.ListTempUnschedulableAccounts(context.Background(), time.Now(), 10)
	require.NoError(t, err)
	require.Len(t, got, 1, "schedulable=true 且停调未过期的账号应出现在探针候选结果中")
	require.Equal(t, int64(3001), got[0].ID)
	require.True(t, got[0].Schedulable)
	require.NotNil(t, got[0].TempUnschedulableUntil)
	require.Equal(t, until, *got[0].TempUnschedulableUntil)
	require.NoError(t, mock.ExpectationsWereMet())

	// 第一条查询是探针候选主查询，WHERE 必须携带 schedulable 过滤条件。
	require.GreaterOrEqual(t, len(queries), 1)
	mainSQL := normalizeSQLWhitespace(queries[0])
	_, whereClause, found := strings.Cut(mainSQL, " WHERE ")
	require.True(t, found, "expected WHERE clause in main query: %s", mainSQL)
	whereClause, _, _ = strings.Cut(whereClause, " ORDER BY ")
	require.Contains(t, whereClause, `"accounts"."schedulable"`,
		"探针候选主查询必须携带 schedulable 过滤条件（ent 对 SchedulableEQ(true) 生成布尔列直接引用）")
	require.Contains(t, whereClause, `"accounts"."temp_unschedulable_until" IS NOT NULL`)
	require.Contains(t, whereClause, `"accounts"."temp_unschedulable_until" > $`)
	require.Contains(t, whereClause, `"accounts"."deleted_at" IS NULL`)
	require.Contains(t, mainSQL, `"temp_unschedulable_until" ASC`,
		"候选按停调最早到期升序取出")
}
