//go:build unit

// KC-24：创建激活码时支持指定上游标识（upstream_key）的写入侧单测。
//
// 覆盖三条不变量：
//  1. 指定 kn_cy → 落库 upstream_key = 'kn_cy'；
//  2. 指定未知上游标识（unknown_vendor）→ 返回 ErrQuotaKeyUnknownUpstream，不静默落 shareai；
//  3. 指定空串 → 落库 upstream_key = 'shareai'（取列默认值）。
//
// 全程使用 sqlite（enttest），不依赖 quota_test Postgres 库，故可在无该库的机器上运行。
package service

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/enttest"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// fakeUpstreamRegistry 是 QuotaUpstreamRegistry 的测试替身：按既定集合判定上游标识是否存在。
type fakeUpstreamRegistry struct {
	// exists 把上游标识映射到"是否存在且未软删"。键不存在即视为不存在。
	exists map[string]bool
}

func (f *fakeUpstreamRegistry) ExistsByKey(_ context.Context, key string) (bool, error) {
	if f == nil || f.exists == nil {
		return false, nil
	}
	return f.exists[key], nil
}

// newUpstreamKeyTestClient 构造内存 sqlite ent 客户端（含 KC-16 的 upstream_key 列默认值与
// quota_upstreams 表），供本卡写入侧单测使用，避免依赖 quota_test Postgres 库。
func newUpstreamKeyTestClient(t *testing.T) *dbent.Client {
	t.Helper()
	db, err := sql.Open("sqlite", "file:quota_key_upstream_key?mode=memory&cache=shared&_fk=1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec("PRAGMA foreign_keys = ON")
	require.NoError(t, err)
	drv := entsql.OpenDB(dialect.SQLite, db)
	client := enttest.NewClient(t, enttest.WithOptions(dbent.Driver(drv)))
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestQuotaKeyService_Create_UpstreamKey_KnCyStored：指定 kn_cy → 落库 upstream_key='kn_cy'。
func TestQuotaKeyService_Create_UpstreamKey_KnCyStored(t *testing.T) {
	client := newUpstreamKeyTestClient(t)
	repo := &testDBRepo{client: client}
	svc := NewQuotaKeyService(repo, NewCredcryptEncryptor(), nil, client)
	svc.SetUpstreamRegistry(&fakeUpstreamRegistry{exists: map[string]bool{"kn_cy": true, "shareai": true}})

	st, err := svc.Create(context.Background(), "CODE-KNCY-1234", nil, nil, 0, "kn_cy")
	require.NoError(t, err)
	require.NotNil(t, st)

	got, err := client.QuotaKey.Get(context.Background(), st.ID)
	require.NoError(t, err)
	require.Equal(t, "kn_cy", got.UpstreamKey, "指定 kn_cy 必须原样落库")
}

// TestQuotaKeyService_Create_UpstreamKey_UnknownRejected：未知上游标识 → 失败关闭，不静默落 shareai。
func TestQuotaKeyService_Create_UpstreamKey_UnknownRejected(t *testing.T) {
	client := newUpstreamKeyTestClient(t)
	repo := &testDBRepo{client: client}
	svc := NewQuotaKeyService(repo, NewCredcryptEncryptor(), nil, client)
	// 注册表里只有 shareai，没有 unknown_vendor。
	svc.SetUpstreamRegistry(&fakeUpstreamRegistry{exists: map[string]bool{"shareai": true}})

	_, err := svc.Create(context.Background(), "CODE-UNKNOWN-1234", nil, nil, 0, "unknown_vendor")
	require.Error(t, err, "未知上游标识必须返回错误")
	require.True(t, errors.Is(err, ErrQuotaKeyUnknownUpstream),
		"错误必须归并为 ErrQuotaKeyUnknownUpstream，实际: %v", err)

	// 不静默落 shareai：库里不应留下任何行。
	count, err := client.QuotaKey.Query().Count(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, count, "未知标识不得被静默落成默认 shareai")
}

// TestQuotaKeyService_Create_UpstreamKey_EmptyDefaultsToShareAI：空串 → 落库 upstream_key='shareai'。
func TestQuotaKeyService_Create_UpstreamKey_EmptyDefaultsToShareAI(t *testing.T) {
	client := newUpstreamKeyTestClient(t)
	repo := &testDBRepo{client: client}
	svc := NewQuotaKeyService(repo, NewCredcryptEncryptor(), nil, client)
	svc.SetUpstreamRegistry(&fakeUpstreamRegistry{exists: map[string]bool{"shareai": true}})

	st, err := svc.Create(context.Background(), "CODE-EMPTY-1234", nil, nil, 0, "")
	require.NoError(t, err)
	require.NotNil(t, st)

	got, err := client.QuotaKey.Get(context.Background(), st.ID)
	require.NoError(t, err)
	require.Equal(t, "shareai", got.UpstreamKey, "空串必须落列默认值 shareai")
}

// TestQuotaKeyService_Create_UpstreamKey_MissingRegistryFailsClosed：注册表缺失时，非空
// upstream_key 必须失败关闭（绝不回落到静默落成 shareai）。
func TestQuotaKeyService_Create_UpstreamKey_MissingRegistryFailsClosed(t *testing.T) {
	client := newUpstreamKeyTestClient(t)
	repo := &testDBRepo{client: client}
	// 不注入 registry。
	svc := NewQuotaKeyService(repo, NewCredcryptEncryptor(), nil, client)

	_, err := svc.Create(context.Background(), "CODE-NORREG-1234", nil, nil, 0, "kn_cy")
	require.Error(t, err, "注册表缺失且指定上游标识时必须失败关闭")
}
