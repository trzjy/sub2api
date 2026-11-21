package repository

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

// 批1 R4：调度候选必须把「until 已到期但仍有待确认探测」的账号排除。
// 这里捕获 ListSchedulableByGroupID 生成的 SQL，断言 probe_due_at 参与了谓词。
func TestListSchedulableByGroupID_ExcludesProbeDueAt(t *testing.T) {
	var capturedSQL string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(captureEntQueryMatcher{actual: &capturedSQL}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	driver := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(driver))
	t.Cleanup(func() { _ = client.Close() })
	repo := newAccountRepositoryWithSQL(client, db, nil)

	mock.ExpectQuery("schedulable accounts").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	accounts, err := repo.ListSchedulableByGroupID(context.Background(), 42)
	require.NoError(t, err)
	require.Empty(t, accounts)
	require.NoError(t, mock.ExpectationsWereMet())

	normalized := normalizeSQLWhitespace(capturedSQL)
	require.True(t, strings.Contains(normalized, "probe_due_at"),
		"调度候选 SQL 必须包含 probe_due_at 谓词：%s", normalized)
}

// quota 类停调不写 until，但 probe_due_at 非空同样必须被调度候选排除。
func TestListProbeDueAccounts_QueryUsesProbeDueAt(t *testing.T) {
	var capturedSQL string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(captureEntQueryMatcher{actual: &capturedSQL}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	driver := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(driver))
	t.Cleanup(func() { _ = client.Close() })
	repo := newAccountRepositoryWithSQL(client, db, nil)

	mock.ExpectQuery("probe due accounts").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	accounts, err := repo.ListProbeDueAccounts(context.Background(), time.Now(), 10)
	require.NoError(t, err)
	require.Empty(t, accounts)
	require.NoError(t, mock.ExpectationsWereMet())

	normalized := normalizeSQLWhitespace(capturedSQL)
	require.True(t, strings.Contains(normalized, "probe_due_at"), "候选查询必须按 probe_due_at 取数：%s", normalized)
	require.True(t, strings.Contains(normalized, "ORDER BY"), "候选查询必须按 probe_due_at 排序：%s", normalized)
}
