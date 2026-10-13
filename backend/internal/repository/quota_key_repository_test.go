//go:build unit

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// quotaKeyRepositoryTestClient 返回连接本地 quota_test 库的 ent client（懒初始化，幂等建表）。
// 与 service 包测试同型（C5a 之前 repository 无单测；此处沿用既有 DB 集成测试形态）。
func quotaKeyRepositoryTestClient(t *testing.T) *ent.Client {
	t.Helper()
	repoTestOnce.Do(func() {
		dsn := os.Getenv("QUOTA_TEST_DSN")
		if dsn == "" {
			dsn = "postgres://quota_test:quota_test_pw@127.0.0.1:5432/quota_test?sslmode=disable"
		}
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			repoTestInitErr = fmt.Errorf("open sql: %w", err)
			return
		}
		if err := db.Ping(); err != nil {
			_ = db.Close()
			repoTestInitErr = fmt.Errorf("ping quota_test db: %w", err)
			return
		}
		drv := entsql.OpenDB(dialect.Postgres, db)
		client := ent.NewClient(ent.Driver(drv))
		if err := client.Schema.Create(context.Background()); err != nil {
			_ = db.Close()
			repoTestInitErr = fmt.Errorf("schema create: %w", err)
			return
		}
		repoTestClient = client
	})
	if repoTestInitErr != nil {
		t.Fatalf("quota_key repository test DB init failed: %v", repoTestInitErr)
	}
	return repoTestClient
}

var (
	repoTestOnce    sync.Once
	repoTestClient  *ent.Client
	repoTestInitErr error
)

// TestQuotaKeyRepository_UpdateMeta：UpdateMeta 仅改 expires_at / notes，
// 不碰 code_encrypted / code_mac / code_last4 / status。
func TestQuotaKeyRepository_UpdateMeta(t *testing.T) {
	client := quotaKeyRepositoryTestClient(t)
	ctx := context.Background()
	repo := NewQuotaKeyRepository(client)

	// 前置：创建一条激活码（含 code / status 等既有字段）。
	uid := fmt.Sprintf("%d", time.Now().UnixNano())
	qk, err := client.QuotaKey.Create().
		SetCodeEncrypted("enc:v1:orig-" + uid).
		SetCodeLast4("abcd").
		SetStatus("active").
		Save(ctx)
	require.NoError(t, err)
	createdID := qk.ID

	// 执行 UpdateMeta：改 expires_at / notes，不动 code / status。
	expiresAt := time.Now().Add(48 * time.Hour)
	notes := "updated note " + uid
	expiresAtValue := &expiresAt
	notesValue := &notes
	require.NoError(t, repo.UpdateMeta(ctx, createdID, &expiresAtValue, &notesValue))

	// 读回，验证元数据生效。
	got, err := client.QuotaKey.Get(ctx, createdID)
	require.NoError(t, err)
	require.NotNil(t, got.ExpiresAt)
	require.WithinDuration(t, expiresAt, *got.ExpiresAt, time.Second)
	require.NotNil(t, got.Notes)
	require.Equal(t, notes, *got.Notes)

	// 不碰 code / status（核心不变量）。
	require.Equal(t, "enc:v1:orig-"+uid, got.CodeEncrypted, "UpdateMeta must NOT touch code_encrypted")
	require.Equal(t, "abcd", got.CodeLast4, "UpdateMeta must NOT touch code_last4")
	require.Equal(t, "active", got.Status, "UpdateMeta must NOT touch status")

	// expires_at / notes 仅传其一时的分支：只改 notes。
	notes2 := "only notes " + uid
	notes2Value := &notes2
	require.NoError(t, repo.UpdateMeta(ctx, createdID, nil, &notes2Value))
	got2, err := client.QuotaKey.Get(ctx, createdID)
	require.NoError(t, err)
	require.NotNil(t, got2.Notes)
	require.Equal(t, notes2, *got2.Notes)
	require.NotNil(t, got2.ExpiresAt, "expires_at must remain when only notes is updated")
	require.WithinDuration(t, expiresAt, *got2.ExpiresAt, time.Second)

	// 显式 null 清空两个可空字段。
	var clearExpiresAt *time.Time
	var clearNotes *string
	require.NoError(t, repo.UpdateMeta(ctx, createdID, &clearExpiresAt, &clearNotes))
	got3, err := client.QuotaKey.Get(ctx, createdID)
	require.NoError(t, err)
	require.Nil(t, got3.ExpiresAt)
	require.Nil(t, got3.Notes)

	// 清理（软删口径：删除由 SoftDeleteMixin 拦截为置 deleted_at，行保留）。
	err = client.QuotaKey.DeleteOneID(createdID).Exec(ctx)
	require.NoError(t, err)
}
