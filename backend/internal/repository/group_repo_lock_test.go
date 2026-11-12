//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// TestLockLiveGroups 验证 lockLiveGroups 与其写锁变体 lockLiveGroupsForUpdate 共享同一
// 存在性校验契约：对未软删分组成功、对缺失/已软删分组返回 ErrGroupNotFound、空输入直接返回 nil。
// 二者唯一区别是锁强度（FOR SHARE vs FOR UPDATE），存在性语义必须完全一致（QKG-FIX-5）。
//
// 真实隔离 PostgreSQL（quotaKeyRepositoryTestClient，QUOTA_TEST_DSN 覆盖），不走假仓储。
func TestLockLiveGroups(t *testing.T) {
	client := quotaKeyRepositoryTestClient(t)
	ctx := context.Background()

	live, err := client.Group.Create().
		SetName("lock-live-" + time.Now().Format("20060102150405.000")).
		SetStatus("active").
		SetPlatform("cursor").
		Save(ctx)
	require.NoError(t, err)
	defer func() { _ = client.Group.DeleteOneID(live.ID).Exec(ctx) }()

	// 已软删分组：行存在但 deleted_at 非空，应判"未找到"（与 live 路径区分）。
	soft, err := client.Group.Create().
		SetName("lock-soft-" + time.Now().Format("20060102150405.000")).
		SetStatus("active").
		SetPlatform("cursor").
		Save(ctx)
	require.NoError(t, err)
	_, err = client.Group.UpdateOneID(soft.ID).SetDeletedAt(time.Now()).Save(ctx)
	require.NoError(t, err)
	defer func() { _ = client.Group.DeleteOneID(soft.ID).Exec(ctx) }()

	const missingID = int64(9_999_999_999)

	t.Run("lockLiveGroups live succeeds", func(t *testing.T) {
		require.NoError(t, lockLiveGroups(ctx, client, []int64{live.ID}))
	})
	t.Run("lockLiveGroups soft-deleted is not found", func(t *testing.T) {
		require.ErrorIs(t, lockLiveGroups(ctx, client, []int64{soft.ID}), service.ErrGroupNotFound)
	})
	t.Run("lockLiveGroups missing is not found", func(t *testing.T) {
		require.ErrorIs(t, lockLiveGroups(ctx, client, []int64{missingID}), service.ErrGroupNotFound)
	})
	t.Run("lockLiveGroups empty is no-op", func(t *testing.T) {
		require.NoError(t, lockLiveGroups(ctx, client, nil))
	})

	t.Run("lockLiveGroupsForUpdate live succeeds", func(t *testing.T) {
		require.NoError(t, lockLiveGroupsForUpdate(ctx, client, []int64{live.ID}))
	})
	t.Run("lockLiveGroupsForUpdate soft-deleted is not found", func(t *testing.T) {
		require.ErrorIs(t, lockLiveGroupsForUpdate(ctx, client, []int64{soft.ID}), service.ErrGroupNotFound)
	})
	t.Run("lockLiveGroupsForUpdate missing is not found", func(t *testing.T) {
		require.ErrorIs(t, lockLiveGroupsForUpdate(ctx, client, []int64{missingID}), service.ErrGroupNotFound)
	})
	t.Run("lockLiveGroupsForUpdate empty is no-op", func(t *testing.T) {
		require.NoError(t, lockLiveGroupsForUpdate(ctx, client, nil))
	})
}
