//go:build integration

package repository

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 平台归并 PR-2（docs/platform-merge-refactor-plan.md §5.8）：迁移 reconciler
// 事务路径的 DB 集成验证——单事务原子性（platform / credentials["access_mode"] /
// extra["migrated_from_platform"] / MAC / scheduler outbox）、幂等、down 精确逆推、
// FOR NO KEY UPDATE 并发互斥。
//
// 佐证单元测试的 fake（单元层只验证编排），本文件用真实 PG 验证 SQL 事务语义。

func (s *AccountRepoSuite) newMigrationAccount(name, platform string) *service.Account {
	account := &service.Account{
		Name:        name,
		Platform:    platform,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Credentials: map[string]any{"api_key": "migration-test-key-" + name, "cookie": "c=" + name},
		Extra:       map[string]any{},
		Concurrency: 1,
		Priority:    50,
		Schedulable: true,
	}
	require.NoError(s.T(), s.repo.Create(s.ctx, account))
	return account
}

// up 单事务三字段一致：platform + access_mode + marker 原子落库，MAC 重算。
func (s *AccountRepoSuite) TestMigrationUpAtomicThreeFields() {
	account := s.newMigrationAccount("mig-up", service.PlatformWebZhipu)

	migrated, err := s.repo.MigrateAccountPlatform(s.ctx, account.ID, service.PlatformWebZhipu, service.PlatformZhipu)
	require.NoError(s.T(), err)
	require.True(s.T(), migrated)

	got, err := s.repo.GetByID(s.ctx, account.ID)
	require.NoError(s.T(), err)
	require.Equal(s.T(), service.PlatformZhipu, got.Platform)
	require.Equal(s.T(), service.AccountAccessModeWeb, got.GetCredential("access_mode"))
	require.Equal(s.T(), service.PlatformWebZhipu, got.GetExtraString("migrated_from_platform"))
	// MAC 一致性：凭证 MAC 列非空（应用层重算后落库，凭证写路径 A3-E2）。
	// 注意 suite 的账号建立在 testEntTx 未提交事务内，integrationDB（连接池）
	// 看不到，必须走 s.client 同事务查询。
	rows, err := s.client.QueryContext(s.ctx,
		"SELECT count(*) FROM accounts WHERE id = $1 AND credentials_mac IS NOT NULL", account.ID)
	require.NoError(s.T(), err)
	require.True(s.T(), rows.Next())
	var macCount int
	require.NoError(s.T(), rows.Scan(&macCount))
	require.NoError(s.T(), rows.Close())
	require.Equal(s.T(), 1, macCount, "credentials_mac must be recomputed")
	// 列表验证：不再出现在 legacy 列表，出现在已迁移列表。
	legacy, err := s.repo.ListLegacyWebPlatformAccounts(s.ctx)
	require.NoError(s.T(), err)
	for _, a := range legacy {
		require.NotEqual(s.T(), account.ID, a.ID)
	}
	migratedList, err := s.repo.ListMigratedFromWebPlatformAccounts(s.ctx)
	require.NoError(s.T(), err)
	found := false
	for _, a := range migratedList {
		if a.ID == account.ID {
			found = true
		}
	}
	require.True(s.T(), found, "expected account in migrated list")
}

// up 幂等：已迁移账号续跑返回 (false, nil)，三字段不变。
func (s *AccountRepoSuite) TestMigrationUpIdempotent() {
	account := s.newMigrationAccount("mig-up-idem", service.PlatformWebDeepseek)
	require.NoError(s.T(), s.repo.Update(s.ctx, func() *service.Account { return account }()))

	_, err := s.repo.MigrateAccountPlatform(s.ctx, account.ID, service.PlatformWebDeepseek, service.PlatformDeepseek)
	require.NoError(s.T(), err)
	migrated, err := s.repo.MigrateAccountPlatform(s.ctx, account.ID, service.PlatformWebDeepseek, service.PlatformDeepseek)
	require.NoError(s.T(), err)
	require.False(s.T(), migrated, "second up must be an idempotent no-op")

	got, err := s.repo.GetByID(s.ctx, account.ID)
	require.NoError(s.T(), err)
	require.Equal(s.T(), service.PlatformDeepseek, got.Platform)
	require.Equal(s.T(), service.AccountAccessModeWeb, got.GetCredential("access_mode"))
}

// up 失败关闭：platform 与 fromPlatform 不一致即报错，绝不半写。
func (s *AccountRepoSuite) TestMigrationUpMismatchFailClosed() {
	account := s.newMigrationAccount("mig-mismatch", service.PlatformWebKimi)

	_, err := s.repo.MigrateAccountPlatform(s.ctx, account.ID, service.PlatformWebZhipu, service.PlatformZhipu)
	require.Error(s.T(), err, "platform mismatch must fail closed")

	got, err := s.repo.GetByID(s.ctx, account.ID)
	require.NoError(s.T(), err)
	// 三字段零写入。
	require.Equal(s.T(), service.PlatformWebKimi, got.Platform)
	require.Empty(s.T(), got.GetCredential("access_mode"))
	require.Empty(s.T(), got.GetExtraString("migrated_from_platform"))
}

// down 精确逆推：按 marker 还原 platform + 移除 access_mode 键 + 移除标记，三字段一致。
func (s *AccountRepoSuite) TestMigrationDownReversesExactly() {
	account := s.newMigrationAccount("mig-down", service.PlatformWebZhipu)

	_, err := s.repo.MigrateAccountPlatform(s.ctx, account.ID, service.PlatformWebZhipu, service.PlatformZhipu)
	require.NoError(s.T(), err)
	reverted, err := s.repo.RevertMigratedAccountPlatform(s.ctx, account.ID)
	require.NoError(s.T(), err)
	require.True(s.T(), reverted)

	got, err := s.repo.GetByID(s.ctx, account.ID)
	require.NoError(s.T(), err)
	require.Equal(s.T(), service.PlatformWebZhipu, got.Platform)
	require.Empty(s.T(), got.GetCredential("access_mode"))
	require.Empty(s.T(), got.GetExtraString("migrated_from_platform"))
}

// down 幂等：无 marker（未迁移/已回滚）返回 (false, nil)。
func (s *AccountRepoSuite) TestMigrationDownIdempotent() {
	account := s.newMigrationAccount("mig-down-idem", service.PlatformWebKimi)

	reverted, err := s.repo.RevertMigratedAccountPlatform(s.ctx, account.ID)
	require.NoError(s.T(), err)
	require.False(s.T(), reverted, "down without marker must be an idempotent no-op")
}

// 并发互斥：两路 up 同时迁移同一账号，FOR NO KEY UPDATE 保证只有一个写成功，
// 最终三字段一致（锁等待后第二个走幂等跳过）。
// 真实并发需要两个独立事务：走连接池（testEntTx 单事务内自锁不互斥），
// 账号用完即清理。
func (s *AccountRepoSuite) TestMigrationConcurrentUpSerialized() {
	poolRepo := newAccountRepositoryWithSQL(integrationEntClient, integrationDB, nil)
	account := &service.Account{
		Name:     "mig-concurrent",
		Platform: service.PlatformWebZhipu,
		Type:     service.AccountTypeAPIKey,
		Status:   service.StatusActive,
		Credentials: map[string]any{
			"api_key": "migration-test-key-mig-concurrent",
			"cookie":  "c=mig-concurrent",
		},
		Extra:       map[string]any{},
		Concurrency: 1,
		Priority:    50,
		Schedulable: true,
	}
	require.NoError(s.T(), poolRepo.Create(s.ctx, account))
	s.T().Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(),
			"DELETE FROM scheduler_outbox WHERE account_id = $1", account.ID)
		_, _ = integrationDB.ExecContext(context.Background(),
			"DELETE FROM accounts WHERE id = $1", account.ID)
	})

	var wg sync.WaitGroup
	results := make(chan bool, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 独立 repo 实例（各自连接池事务），模拟生产并发入口。
			repo := newAccountRepositoryWithSQL(integrationEntClient, integrationDB, nil)
			migrated, err := repo.MigrateAccountPlatform(s.ctx, account.ID, service.PlatformWebZhipu, service.PlatformZhipu)
			results <- migrated
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	succeeded := 0
	for migrated := range results {
		if migrated {
			succeeded++
		}
	}
	for err := range errs {
		require.NoError(s.T(), err)
	}
	// 两路都不失败；至多一路真实写（另一路锁等待后幂等跳过）。
	require.LessOrEqual(s.T(), succeeded, 1, "FOR NO KEY UPDATE must serialize concurrent ups")

	got, err := poolRepo.GetByID(s.ctx, account.ID)
	require.NoError(s.T(), err)
	require.Equal(s.T(), service.PlatformZhipu, got.Platform)
	require.Equal(s.T(), service.AccountAccessModeWeb, got.GetCredential("access_mode"))
	require.Equal(s.T(), service.PlatformWebZhipu, got.GetExtraString("migrated_from_platform"))
}

// outbox 失败回滚：scheduler outbox 入队失败时整个事务回滚，三字段零写入。
// 迁移写路径的 UPDATE 与 outbox 入队都走 tx.Client()（ent driver），不经过
// r.sql——因此必须在 driver 层注入失败，而不是包一层 r.sql 执行器。
//
// suite 的 testEntTx 未提交事务里无法让独立迁移事务看到账号，因此本用例
// 整体走连接池真实事务（提交/回滚真实发生），账号用完即清理。
func (s *AccountRepoSuite) TestMigrationOutboxFailureRollsBack() {
	poolRepo := newAccountRepositoryWithSQL(integrationEntClient, integrationDB, nil)
	account := &service.Account{
		Name:     "mig-outbox",
		Platform: service.PlatformWebZhipu,
		Type:     service.AccountTypeAPIKey,
		Status:   service.StatusActive,
		Credentials: map[string]any{
			"api_key": "migration-test-key-mig-outbox",
			"cookie":  "c=mig-outbox",
		},
		Extra:       map[string]any{},
		Concurrency: 1,
		Priority:    50,
		Schedulable: true,
	}
	require.NoError(s.T(), poolRepo.Create(s.ctx, account))
	// Create 自身会经 outbox 入队一条 account_changed（提交）；清除后，
	// 下方计数只反映本次迁移事务的写入。
	_, _ = integrationDB.ExecContext(s.ctx,
		"DELETE FROM scheduler_outbox WHERE account_id = $1", account.ID)
	s.T().Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(),
			"DELETE FROM scheduler_outbox WHERE account_id = $1", account.ID)
		_, _ = integrationDB.ExecContext(context.Background(),
			"DELETE FROM accounts WHERE id = $1", account.ID)
	})

	failingClient := dbent.NewClient(dbent.Driver(&failOutboxDriver{Driver: integrationEntClient.Driver().(*entsql.Driver)}))
	failing := newAccountRepositoryWithSQL(failingClient, failingClient, nil)
	_, err := failing.MigrateAccountPlatform(s.ctx, account.ID, service.PlatformWebZhipu, service.PlatformZhipu)
	require.Error(s.T(), err, "outbox failure must fail closed")
	require.False(s.T(), strings.Contains(err.Error(), "no rows"), "migration must see the committed account")

	got, err := poolRepo.GetByID(s.ctx, account.ID)
	require.NoError(s.T(), err)
	require.Equal(s.T(), service.PlatformWebZhipu, got.Platform, "platform must roll back")
	require.Empty(s.T(), got.GetCredential("access_mode"), "credentials must roll back")
	require.Empty(s.T(), got.GetExtraString("migrated_from_platform"), "marker must roll back")
	// UPDATE + outbox 同事务：outbox 失败后 outbox 表也不得残留事件。
	var outboxCount int
	require.NoError(s.T(), integrationDB.QueryRowContext(s.ctx,
		"SELECT count(*) FROM scheduler_outbox WHERE account_id = $1", account.ID).Scan(&outboxCount))
	require.Zero(s.T(), outboxCount)
}

// failOutboxDriver 包装 entsql driver，拦截 scheduler_outbox INSERT（含 tx driver
// 派生路径）使其失败，其余语句原样放行。
type failOutboxDriver struct {
	*entsql.Driver
}

func (d *failOutboxDriver) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if strings.Contains(query, "INSERT INTO scheduler_outbox") {
		return nil, errors.New("simulated scheduler outbox enqueue failure")
	}
	return d.Driver.ExecContext(ctx, query, args...)
}

func (d *failOutboxDriver) Tx(ctx context.Context) (dialect.Tx, error) {
	tx, err := d.Driver.Tx(ctx)
	if err != nil {
		return nil, err
	}
	return failOutboxTx{Tx: tx}, nil
}

type failOutboxTx struct {
	dialect.Tx
}

func (t failOutboxTx) Commit() error   { return t.Tx.Commit() }
func (t failOutboxTx) Rollback() error { return t.Tx.Rollback() }
func (t failOutboxTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	// dialect.Tx 嵌入的 ExecQuerier 只暴露 Exec/Query（dialect 语义），底层
	// entsql.Tx 的 ExecContext/QueryContext 需经类型断言取回标准 sql 语义。
	q, ok := t.Tx.(interface {
		QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	})
	if !ok {
		return nil, errors.New("underlying tx driver does not support QueryContext")
	}
	return q.QueryContext(ctx, query, args...)
}

func (t failOutboxTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if strings.Contains(query, "INSERT INTO scheduler_outbox") {
		return nil, errors.New("simulated scheduler outbox enqueue failure")
	}
	q, ok := t.Tx.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	if !ok {
		return nil, errors.New("underlying tx driver does not support ExecContext")
	}
	return q.ExecContext(ctx, query, args...)
}
