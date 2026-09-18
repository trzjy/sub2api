package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// platformMigrationFakeRepo 平台归并 PR-2 reconciler 单测替身：只实现
// WebPlatformMigrationRepository 窄接口（迁移写路径专用），不冒充完整
// AccountRepository。
type platformMigrationFakeRepo struct {
	legacyAccounts   []*Account
	migratedAccounts []*Account

	migrateCalls []struct {
		id           int64
		fromPlatform string
		toPlatform   string
	}
	revertCalls []int64

	listLegacyErr  error
	listMigratedEr error
	migrateResults map[int64]bool
	migrateErrs    map[int64]error
	revertResults  map[int64]bool
	revertErrs     map[int64]error
}

func (f *platformMigrationFakeRepo) ListLegacyWebPlatformAccounts(ctx context.Context) ([]*Account, error) {
	if f.listLegacyErr != nil {
		return nil, f.listLegacyErr
	}
	return f.legacyAccounts, nil
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

func TestMigrateWebPlatformAccountsUpHappyPath(t *testing.T) {
	repo := &platformMigrationFakeRepo{
		legacyAccounts: []*Account{
			{ID: 84, Name: "glm-web", Platform: PlatformWebZhipu},
			{ID: 85, Name: "ds-web", Platform: PlatformWebDeepseek},
		},
		migrateResults: map[int64]bool{84: true, 85: true},
	}
	svc := newPlatformMigrationTestService(repo)

	report, err := svc.MigrateWebPlatformAccounts(context.Background(), WebPlatformMigrationDirectionUp, false)
	require.NoError(t, err)
	require.False(t, report.DryRun)
	require.Equal(t, 2, report.Total)
	require.Len(t, report.Migrated, 2)
	require.Len(t, report.Skipped, 0)
	require.Equal(t, PlatformWebZhipu, report.Migrated[0].FromPlatform)
	require.Equal(t, PlatformZhipu, report.Migrated[0].ToPlatform)
	require.Equal(t, PlatformWebDeepseek, report.Migrated[1].FromPlatform)
	require.Equal(t, PlatformDeepseek, report.Migrated[1].ToPlatform)
	// 逐账号调用专用事务方法，from/to 与账号当前平台一致。
	require.Len(t, repo.migrateCalls, 2)
	require.Equal(t, int64(84), repo.migrateCalls[0].id)
	require.Equal(t, PlatformWebZhipu, repo.migrateCalls[0].fromPlatform)
	require.Equal(t, PlatformZhipu, repo.migrateCalls[0].toPlatform)
}

func TestMigrateWebPlatformAccountsUpIdempotentSkip(t *testing.T) {
	repo := &platformMigrationFakeRepo{
		legacyAccounts: []*Account{
			{ID: 84, Name: "glm-web", Platform: PlatformWebZhipu},
		},
		migrateResults: map[int64]bool{84: false}, // 已迁移（幂等 no-op）
	}
	svc := newPlatformMigrationTestService(repo)

	report, err := svc.MigrateWebPlatformAccounts(context.Background(), WebPlatformMigrationDirectionUp, false)
	require.NoError(t, err)
	require.Equal(t, 1, report.Total)
	require.Len(t, report.Migrated, 0)
	require.Len(t, report.Skipped, 1)
	require.True(t, report.Skipped[0].AlreadyMerged)
}

func TestMigrateWebPlatformAccountsUpFailClosed(t *testing.T) {
	repo := &platformMigrationFakeRepo{
		legacyAccounts: []*Account{
			{ID: 84, Name: "glm-web", Platform: PlatformWebZhipu},
			{ID: 90, Name: "bad", Platform: PlatformWebKimi},
		},
		migrateResults: map[int64]bool{84: true},
		migrateErrs:    map[int64]error{90: errors.New("tx conflict")},
	}
	svc := newPlatformMigrationTestService(repo)

	_, err := svc.MigrateWebPlatformAccounts(context.Background(), WebPlatformMigrationDirectionUp, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "account 90")
	// 失败前已按序处理账号 84。
	require.Len(t, repo.migrateCalls, 2)
}

func TestMigrateWebPlatformAccountsUpDryRun(t *testing.T) {
	repo := &platformMigrationFakeRepo{
		legacyAccounts: []*Account{
			{ID: 84, Name: "glm-web", Platform: PlatformWebZhipu, Credentials: map[string]any{"access_mode": AccountAccessModeWeb}},
			{ID: 85, Name: "ds-web", Platform: PlatformWebDeepseek},
		},
	}
	svc := newPlatformMigrationTestService(repo)

	report, err := svc.MigrateWebPlatformAccounts(context.Background(), WebPlatformMigrationDirectionUp, true)
	require.NoError(t, err)
	require.True(t, report.DryRun)
	require.Equal(t, 2, report.Total)
	// dry-run 不写库。
	require.Len(t, repo.migrateCalls, 0)
	// 显式 access_mode=web 归入 skipped，形状待迁移的归入 migrated。
	require.Len(t, report.Skipped, 1)
	require.Equal(t, int64(84), report.Skipped[0].ID)
	require.Len(t, report.Migrated, 1)
	require.Equal(t, int64(85), report.Migrated[0].ID)
}

func TestMigrateWebPlatformAccountsDown(t *testing.T) {
	repo := &platformMigrationFakeRepo{
		migratedAccounts: []*Account{
			{ID: 84, Name: "glm-web", Platform: PlatformZhipu, Extra: map[string]any{"migrated_from_platform": PlatformWebZhipu}},
		},
		revertResults: map[int64]bool{84: true},
	}
	svc := newPlatformMigrationTestService(repo)

	report, err := svc.MigrateWebPlatformAccounts(context.Background(), WebPlatformMigrationDirectionDown, false)
	require.NoError(t, err)
	require.Equal(t, WebPlatformMigrationDirectionDown, report.Direction)
	require.Equal(t, 1, report.Total)
	require.Len(t, report.Migrated, 1)
	require.Equal(t, PlatformWebZhipu, report.Migrated[0].ToPlatform)
	require.Equal(t, []int64{84}, repo.revertCalls)
}

func TestMigrateWebPlatformAccountsDownIdempotentSkip(t *testing.T) {
	repo := &platformMigrationFakeRepo{
		migratedAccounts: []*Account{
			{ID: 84, Name: "glm-web", Platform: PlatformZhipu, Extra: map[string]any{"migrated_from_platform": PlatformWebZhipu}},
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

func TestMigrateWebPlatformAccountsFailClosedWithoutRepository(t *testing.T) {
	svc := &adminServiceImpl{accountRepo: &accountRepoStubForMigration{}}
	_, err := svc.MigrateWebPlatformAccounts(context.Background(), WebPlatformMigrationDirectionUp, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "fail-closed")
}

func TestMigrateWebPlatformAccountsListErrorFailClosed(t *testing.T) {
	repo := &platformMigrationFakeRepo{listLegacyErr: errors.New("db down")}
	svc := newPlatformMigrationTestService(repo)
	_, err := svc.MigrateWebPlatformAccounts(context.Background(), WebPlatformMigrationDirectionUp, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "db down")

	repo2 := &platformMigrationFakeRepo{listMigratedEr: errors.New("db down")}
	svc2 := newPlatformMigrationTestService(repo2)
	_, err = svc2.MigrateWebPlatformAccounts(context.Background(), WebPlatformMigrationDirectionDown, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "db down")
}

func TestMigrateWebPlatformAccountsUnmappedLegacyPlatform(t *testing.T) {
	repo := &platformMigrationFakeRepo{
		legacyAccounts: []*Account{
			{ID: 99, Name: "mystery", Platform: PlatformWebZhipu + "-x"},
		},
	}
	svc := newPlatformMigrationTestService(repo)
	_, err := svc.MigrateWebPlatformAccounts(context.Background(), WebPlatformMigrationDirectionUp, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unmapped legacy web platform")
}

var _ WebPlatformMigrationRepository = (*platformMigrationFakeRepo)(nil)
