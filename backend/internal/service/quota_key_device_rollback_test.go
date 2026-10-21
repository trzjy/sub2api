//go:build unit

package service

// ---------------------------------------------------------------------------
// U7FIX-R12 / O01：device_id 首写移至容量事务前的回滚回归（常驻测试，非 overlay）。
// 连本地 quota_test 库（QUOTA_TEST_DSN）。
//
// 背景：旧码在 pull 容量事务内（FOR UPDATE 持 quota_keys 行锁）首写 device_id；上游
// 成功后若挂组解析/配置读取/链接写入/提交任一失败 → 事务回滚 → device_id 回 NULL →
// 下次重试生成第二份 ID，违反「新码只生成一份稳定值」。
//
// O01 把首写前置到事务外（无行锁、独立提交，经 DeviceIDFor 走全局连接 SetDeviceIDIfAbsent）。
// 本文件验证：
//   - 场景 A：挂组解析注入失败使容量事务回滚后，device_id 仍已提交保留，且上游收到的
//     device-id 与 DB 值一致；号不入库。
//   - 场景 B：同 key 再次 pull（挂组正常）成功，上游第二次收到的 device-id == 第一次
//     （身份稳定，未生成第二份）。
// ---------------------------------------------------------------------------

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/ent/accountgroup"
	entgroup "github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// TestQuotaKeyDevice_RollbackKeepsDeviceIDAndStable：O01 回滚回归（场景 A + 场景 B）。
func TestQuotaKeyDevice_RollbackKeepsDeviceIDAndStable(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	// 覆盖组：composite 聚合组 + 白名单启用且覆盖 claude-sonnet-5-5（U7 目标组），
	// 供场景 B 恢复真实挂载解析器后成功挂载落号。
	grp := mustCreateCompositeGroup(t, client, domain.GroupModelAllowlist{
		Enabled: true,
		Models:  []string{"claude-sonnet-5-5"},
	})
	t.Cleanup(func() {
		c := context.Background()
		_, _ = client.AccountGroup.Delete().Where(accountgroup.GroupIDEQ(grp.ID)).Exec(c)
		_, _ = client.Group.Delete().Where(entgroup.IDEQ(grp.ID)).Exec(c)
	})

	// 记录上游每次 PullBatch 收到的 device-id（qk.DeviceID 在调用时刻的值）。
	var mu sync.Mutex
	var seen []string
	up := &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "dev-rollback", Platform: "cursor", Type: "oauth", Credentials: map[string]any{"k": "v"}},
		}},
		onPull: func(qk *ent.QuotaKey) {
			mu.Lock()
			defer mu.Unlock()
			if qk.DeviceID != nil {
				seen = append(seen, *qk.DeviceID)
			}
		},
	}
	svc, _ := qkNewTestService(t, up)

	// 新码：active、device_id 为空、无账号。
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)

	// 场景 A：挂组解析注入失败 → 容量事务回滚（号不入库）。
	svc.cursorGroupResolver = func(_ context.Context, _ *ent.Tx) ([]int64, error) {
		return nil, errors.New("injected mount resolve failure")
	}
	err := svc.PullAccountsForModel(ctx, qk.ID, "claude-sonnet-5-5")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrQuotaKeyMountFailed),
		"injected mount resolve failure must surface as ErrQuotaKeyMountFailed")

	// 事务回滚后 device_id 仍已提交保留（首写前置到事务外、独立提交）。
	got, gerr := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, gerr)
	require.NotNil(t, got.DeviceID, "device_id must persist after capacity tx rollback (pre-tx first-write)")
	require.NotEqual(t, "", *got.DeviceID, "device_id must be non-empty after rollback")

	// 上游记录的 device-id 与 DB 一致。
	mu.Lock()
	require.Len(t, seen, 1, "PullBatch must be called exactly once before rollback")
	require.Equal(t, *got.DeviceID, seen[0], "upstream received device-id must match DB value")
	mu.Unlock()

	// 号不入库（回滚）。
	n, _ := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).Count(ctx)
	require.Equal(t, 0, n, "rolled-back pull must not persist the account")

	// 场景 B：同 key 再次 pull，挂组正常（恢复真实解析器）→ 成功落号。
	svc.cursorGroupResolver = svc.findCursorPlatformGroups
	require.NoError(t, svc.PullAccountsForModel(ctx, qk.ID, "claude-sonnet-5-5"))

	// 第二次上游收到的 device-id 必须等于第一次（身份稳定，未生成第二份）。
	mu.Lock()
	require.Len(t, seen, 2, "PullBatch must be called twice across retry")
	require.Equal(t, seen[0], seen[1], "device-id must be stable across retry (no second generation)")
	mu.Unlock()

	// 账号落库且 device_id 不变。
	accs, _ := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).All(ctx)
	require.Len(t, accs, 1, "retry pull must persist the account")
	got2, _ := client.QuotaKey.Get(ctx, qk.ID)
	require.Equal(t, *got.DeviceID, *got2.DeviceID, "device_id must remain unchanged after successful retry")
}

// TestQuotaKeyDevice_PreTxWriteSkippedForExistingAccount：O01 语义保持——存量有账号
// （HasAccount==true）的新码不写不迁移（保持既有派生值语义）；此处验证首写前置路径对
// 存量派生值不触发 DeviceIDFor 覆盖。
func TestQuotaKeyDevice_PreTxWriteSkippedForExistingAccount(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	// 覆盖组（供正常挂载成功）。
	grp := mustCreateCompositeGroup(t, client, domain.GroupModelAllowlist{
		Enabled: true,
		Models:  []string{"claude-sonnet-5-5"},
	})
	t.Cleanup(func() {
		c := context.Background()
		_, _ = client.AccountGroup.Delete().Where(accountgroup.GroupIDEQ(grp.ID)).Exec(c)
		_, _ = client.Group.Delete().Where(entgroup.IDEQ(grp.ID)).Exec(c)
	})

	// 已存在账号的 key（存量，device_id 为空由派生语义处理，首写路径不得生成覆盖值）。
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	mustCreateAccounts(t, client, qk.ID, 1)

	var mu sync.Mutex
	var calls int
	var upstreamDeviceID string
	up := &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "dev-existing", Platform: "cursor", Type: "oauth", Credentials: map[string]any{"k": "v"}},
		}},
		onPull: func(qk *ent.QuotaKey) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			if qk.DeviceID != nil {
				upstreamDeviceID = *qk.DeviceID
			}
		},
	}
	svc, _ := qkNewTestService(t, up)

	// 存量有账号 → 首写前置路径跳过 DeviceIDFor（不写不迁移）。
	require.NoError(t, svc.PullAccountsForModel(ctx, qk.ID, "claude-sonnet-5-5"))

	mu.Lock()
	require.Equal(t, 1, calls, "PullBatch must be called once")
	mu.Unlock()

	// 存量派生身份语义：device_id 仍为空（首写路径未触碰），upstream 收到的也是空值。
	got, _ := client.QuotaKey.Get(ctx, qk.ID)
	require.Nil(t, got.DeviceID, "existing-account key must NOT get a first-write device_id (preserve derived semantics)")
	require.Equal(t, "", upstreamDeviceID, "upstream must receive empty device_id for existing-account key")
}
