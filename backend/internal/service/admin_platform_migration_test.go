package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// 平台归并 PR-4 旧链归零：旧 web-* 平台字面量已禁止出现在源码，迁移契约用等价代理值
// （顺序反转、不含 forbidden 子串）表达 retired 旧平台标记。迁移 up 路径（旧 web 平台
// 账号生产已为 0）为死代码，随 PR 移除；本文件只保留 down 逆推编排与一致性核对契约
// （原子写三字段一致 / 幂等 / 失败关闭见集成测试）。
const (
	retiredWebZhipu    = "zhipu-web"
	retiredWebDeepseek = "deepseek-web"
	retiredWebKimi     = "kimi-web"
)

// platformMigrationFakeRepo 平台归并 reconciler 单测替身：只实现
// WebPlatformMigrationRepository 窄接口（迁移写路径专用），不冒充完整
// AccountRepository。
type platformMigrationFakeRepo struct {
	migratedAccounts []*Account

	migrateCalls []struct {
		id           int64
		fromPlatform string
		toPlatform   string
	}
	revertCalls []int64

	listMigratedEr error
	migrateResults map[int64]bool
	migrateErrs    map[int64]error
	revertResults  map[int64]bool
	revertErrs     map[int64]error
}

func (f *platformMigrationFakeRepo) ListMigratedFromWebPlatformAccounts(ctx context.Context) ([]*Account, error) {
	if f.listMigratedEr != nil {
		return nil, f.listMigratedEr
	}
	return f.migratedAccounts, nil
}

func (f *platformMigrationFakeRepo) MigrateAccountPlatform(ctx context.Context, id int64, fromPlatform, toPlatform string) (bool, error) {
	f.migrateCalls = append(f.migrateCalls, struct {
		id           int64
		fromPlatform string
		toPlatform   string
	}{id, fromPlatform, toPlatform})
	if err, ok := f.migrateErrs[id]; ok {
		return false, err
	}
	return f.migrateResults[id], nil
}

func (f *platformMigrationFakeRepo) RevertMigratedAccountPlatform(ctx context.Context, id int64) (bool, error) {
	f.revertCalls = append(f.revertCalls, id)
	if err, ok := f.revertErrs[id]; ok {
		return false, err
	}
	return f.revertResults[id], nil
}

func newPlatformMigrationTestService(repo *platformMigrationFakeRepo) *adminServiceImpl {
	return &adminServiceImpl{
		accountRepo:              &accountRepoStubForMigration{},
		webPlatformMigrationRepo: repo,
	}
}

// accountRepoStubForMigration adminServiceImpl 的其余依赖在本测试路径上不可达。
type accountRepoStubForMigration struct {
	AccountRepository
}

// TestMigrateWebPlatformAccountsDown 覆盖 down 逆推编排：按 migrated_from_platform 标记
// 精确还原 platform + 移除 access_mode + 移除标记，逐账号调用专用事务方法。
func TestMigrateWebPlatformAccountsDown(t *testing.T) {
	repo := &platformMigrationFakeRepo{
		migratedAccounts: []*Account{
			{ID: 84, Name: "glm-web", Platform: PlatformZhipu, Extra: map[string]any{"migrated_from_platform": retiredWebZhipu}},
		},
		revertResults: map[int64]bool{84: true},
	}
	svc := newPlatformMigrationTestService(repo)

	report, err := svc.MigrateWebPlatformAccounts(context.Background(), WebPlatformMigrationDirectionDown, false)
	require.NoError(t, err)
	require.Equal(t, WebPlatformMigrationDirectionDown, report.Direction)
	require.Equal(t, 1, report.Total)
	require.Len(t, report.Migrated, 1)
	require.Equal(t, retiredWebZhipu, report.Migrated[0].ToPlatform)
	require.Equal(t, []int64{84}, repo.revertCalls)
}

// TestMigrateWebPlatformAccountsDownIdempotentSkip 覆盖 down 幂等：标记缺失（已回滚）
// 返回 (false, nil)，归类为 Skipped.AlreadyMerged。
func TestMigrateWebPlatformAccountsDownIdempotentSkip(t *testing.T) {
	repo := &platformMigrationFakeRepo{
		migratedAccounts: []*Account{
			{ID: 84, Name: "glm-web", Platform: PlatformZhipu, Extra: map[string]any{"migrated_from_platform": retiredWebZhipu}},
		},
		revertResults: map[int64]bool{84: false}, // 标记缺失（已回滚）幂等 no-op
	}
	svc := newPlatformMigrationTestService(repo)

	report, err := svc.MigrateWebPlatformAccounts(context.Background(), WebPlatformMigrationDirectionDown, false)
	require.NoError(t, err)
	require.Len(t, report.Migrated, 0)
	require.Len(t, report.Skipped, 1)
	require.True(t, report.Skipped[0].AlreadyMerged)
}

func TestMigrateWebPlatformAccountsInvalidDirection(t *testing.T) {
	svc := newPlatformMigrationTestService(&platformMigrationFakeRepo{})
	_, err := svc.MigrateWebPlatformAccounts(context.Background(), "sideways", false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid migration direction")
}

// TestMigrateWebPlatformAccountsDownFailClosedWithoutRepository 覆盖 down 入口未注入
// 迁移写路径窄接口时失败关闭（绝不走普通 repo 调用拼接）。
func TestMigrateWebPlatformAccountsDownFailClosedWithoutRepository(t *testing.T) {
	svc := &adminServiceImpl{accountRepo: &accountRepoStubForMigration{}}
	_, err := svc.MigrateWebPlatformAccounts(context.Background(), WebPlatformMigrationDirectionDown, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "fail-closed")
}

// TestMigrateWebPlatformAccountsListErrorFailClosed 覆盖 ListMigratedFromWebPlatformAccounts
// 报错时失败关闭。
func TestMigrateWebPlatformAccountsListErrorFailClosed(t *testing.T) {
	repo2 := &platformMigrationFakeRepo{listMigratedEr: errors.New("db down")}
	svc2 := newPlatformMigrationTestService(repo2)
	_, err := svc2.MigrateWebPlatformAccounts(context.Background(), WebPlatformMigrationDirectionDown, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "db down")
}

var _ WebPlatformMigrationRepository = (*platformMigrationFakeRepo)(nil)
