//go:build unit

package service

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/enttest"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// newSweepNowSQLiteClient 构造 sqlite 内存实例，仅为 SweepNow 提供真实事务
// （begin/commit）。生产为 PostgreSQL；这里只验证 SweepNow 的事务编排与返回值，
// 不依赖任何 PG 专有语法（sweepOnce 本身只走 repo 方法）。
func newSweepNowSQLiteClient(t *testing.T) *ent.Client {
	t.Helper()
	db, err := sql.Open("sqlite", "file:quota_key_sweepnow?mode=memory&cache=shared&_pragma=foreign_keys(1)&_fk=1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	drv := entsql.OpenDB(dialect.SQLite, db)
	client := enttest.NewClient(t, enttest.WithOptions(ent.Driver(drv)))
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestQuotaKeySweep_SweepNowMarksExpired 验证 admin 手动触发口 SweepNow：
// 复用既有 sweepOnce，返回本轮实际改写条数，并把过期激活码标为 expired。
func TestQuotaKeySweep_SweepNowMarksExpired(t *testing.T) {
	client := newSweepNowSQLiteClient(t)
	repo := newFakeQuotaKeyRepo()
	repo.byID[1] = &ent.QuotaKey{ID: 1, Status: "active", ExpiresAt: timePtr(time.Now().Add(-time.Hour))}
	repo.byID[2] = &ent.QuotaKey{ID: 2, Status: "active", ExpiresAt: timePtr(time.Now().Add(time.Hour))}

	sweeper := NewQuotaKeySweeper(repo, nil, client)
	n, err := sweeper.SweepNow(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n, "只标记到期的那条，未到期的保持不动")

	require.Equal(t, "expired", repo.byID[1].Status)
	require.Equal(t, "active", repo.byID[2].Status)
}

// TestQuotaKeySweep_SweepNowIdempotent 第二轮触发不重复计数（幂等跳过已 expired）。
func TestQuotaKeySweep_SweepNowIdempotent(t *testing.T) {
	client := newSweepNowSQLiteClient(t)
	repo := newFakeQuotaKeyRepo()
	repo.byID[1] = &ent.QuotaKey{ID: 1, Status: "active", ExpiresAt: timePtr(time.Now().Add(-time.Hour))}

	sweeper := NewQuotaKeySweeper(repo, nil, client)
	first, err := sweeper.SweepNow(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, first)

	second, err := sweeper.SweepNow(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, second, "已是 expired 的幂等跳过，第二轮计数为 0")
}

// TestQuotaKeySweep_SweepNowRequiresClient 未装配 ent client 时必须显式报错，
// 禁止静默返回 0（否则 admin 端点会把"没清扫成功"误报为成功）。
func TestQuotaKeySweep_SweepNowRequiresClient(t *testing.T) {
	sweeper := NewQuotaKeySweeper(newFakeQuotaKeyRepo(), nil, nil)
	n, err := sweeper.SweepNow(context.Background())
	require.Error(t, err)
	require.Equal(t, 0, n)
}
