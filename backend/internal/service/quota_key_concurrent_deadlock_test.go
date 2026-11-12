//go:build unit

// 外部测试包：本文件位于 package service_test，以便在不触发 service↔repository 导入环的
// 前提下，使用真实 repository.NewQuotaKeyRepository 装配 QuotaKeyService，从而让
// UpdateWithSettings 走真实的 UpdateSettingsAndSyncAccounts（含 lockLiveGroupsForUpdate）
// 全路径。既有 package service 的测试用 testDBRepo/qkgDBSettingRepo 是替身，不调用真实的
// lockLiveGroups，无法复现/验证本卡的死锁修复，故必须经由真实仓储跑全路径。
package service_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

var (
	extOnce    sync.Once
	extClient  *ent.Client
	extInitErr error
	extQKSeq   int64
)

// extEntClient 返回连接本地 quota_test 库的 ent client（懒初始化，幂等建表）。
// DSN 可通过 QUOTA_TEST_DSN 覆盖；默认连 127.0.0.1:5432/quota_test。
func extEntClient(t *testing.T) *ent.Client {
	t.Helper()
	extOnce.Do(func() {
		dsn := os.Getenv("QUOTA_TEST_DSN")
		if dsn == "" {
			dsn = "postgres://quota_test:quota_test_pw@127.0.0.1:5432/quota_test?sslmode=disable"
		}
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			extInitErr = fmt.Errorf("open sql: %w", err)
			return
		}
		if err := db.Ping(); err != nil {
			_ = db.Close()
			extInitErr = fmt.Errorf("ping quota_test db (set QUOTA_TEST_DSN if different): %w", err)
			return
		}
		drv := entsql.OpenDB(dialect.Postgres, db)
		c := ent.NewClient(ent.Driver(drv))
		if err := c.Schema.Create(context.Background()); err != nil {
			_ = db.Close()
			extInitErr = fmt.Errorf("schema create: %w", err)
			return
		}
		// scheduler_outbox 不是 ent 实体（由 migrations 管理），真实仓储的
		// UpdateSettingsAndSyncAccounts 会向它写入事件；测试显式建表（与仓库单测同源 DDL）。
		for _, stmt := range []string{
			`CREATE TABLE IF NOT EXISTS scheduler_outbox (
				id BIGSERIAL PRIMARY KEY,
				event_type TEXT NOT NULL,
				account_id BIGINT NULL,
				group_id BIGINT NULL,
				payload JSONB NULL,
				created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
			)`,
			`CREATE INDEX IF NOT EXISTS idx_scheduler_outbox_created_at ON scheduler_outbox (created_at)`,
			`ALTER TABLE scheduler_outbox ADD COLUMN IF NOT EXISTS dedup_key TEXT`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_scheduler_outbox_pending_dedup_key ON scheduler_outbox (dedup_key) WHERE dedup_key IS NOT NULL`,
		} {
			if _, err := c.ExecContext(context.Background(), stmt); err != nil {
				_ = db.Close()
				extInitErr = fmt.Errorf("scheduler_outbox ddl: %w", err)
				return
			}
		}
		extClient = c
	})
	if extInitErr != nil {
		t.Fatalf("ext test DB init failed: %v", extInitErr)
	}
	return extClient
}

// extCreateQKWithAccount 创建激活码并绑定指定分组，且至少带一个未软删账号（避免
// UpdateSettingsAndSyncAccounts 在分组锁步骤前提前返回），返回激活码 ID。
func extCreateQKWithAccount(t *testing.T, client *ent.Client, groupIDs []int64) int64 {
	t.Helper()
	ctx := context.Background()
	seq := atomic.AddInt64(&extQKSeq, 1)
	qk, err := client.QuotaKey.Create().
		SetCodeEncrypted(fmt.Sprintf("enc-%d", seq)).
		SetCodeMAC(fmt.Sprintf("mac-%d", seq)).
		SetStatus(domain.QuotaKeyStatusActive).
		SetGroupIds(groupIDs).
		Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.QuotaKey.DeleteOneID(qk.ID).Exec(ctx) })

	_, err = client.Account.Create().
		SetName(fmt.Sprintf("acc-%d", seq)).
		SetPlatform(domain.PlatformCursor).SetType("oauth").
		SetCredentials(map[string]any{"access_token": fmt.Sprintf("t-%d", seq)}).
		SetCredentialsMAC(fmt.Sprintf("m-%d", seq)).
		SetQuotaKeyID(qk.ID).SetStatus(domain.StatusActive).SetSchedulable(true).Save(ctx)
	require.NoError(t, err)
	return qk.ID
}

// TestUpdateWithSettingsConcurrentNoDeadlockNoLostUpdate 是外审点名缺口的覆盖用例：
// 两个不同激活码绑定同一分组，各自并发调用 QuotaKeyService.UpdateWithSettings（请求含
// GroupIDs 与不同 ModelMapping）。验证：
//  1. 两笔都必须成功（无锁升级导致的死锁）；
//  2. 最终该分组的 model_allowlist.models 同时包含两个模型名（无丢更新）。
//
// 关键：经由真实 repository 走全路径——UpdateSettingsAndSyncAccounts 对同一批分组先取写锁
// （lockLiveGroupsForUpdate，FOR UPDATE），随后 appendModelsToBoundGroupAllowlists 再次取
// FOR UPDATE；同事务内"写锁→写锁"不构成锁升级，故两个激活码的事务不会死锁。多轮重复放大竞争。
func TestUpdateWithSettingsConcurrentNoDeadlockNoLostUpdate(t *testing.T) {
	client := extEntClient(t)
	ctx := context.Background()

	// 共享分组：composite 平台 + 启用模型白名单，使任意家族的源模型名都会被追加。
	g, err := client.Group.Create().
		SetName(fmt.Sprintf("qkg-fix5-g-%d", time.Now().UnixNano())).
		SetStatus(domain.StatusActive).
		SetPlatform(service.PlatformComposite).
		SetModelAllowlist(domain.GroupModelAllowlist{Enabled: true, Models: []string{}}).
		Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Group.DeleteOneID(g.ID).Exec(ctx) })

	// 两个不同激活码，各带一个账号，都绑定同一分组。
	qk1 := extCreateQKWithAccount(t, client, []int64{g.ID})
	qk2 := extCreateQKWithAccount(t, client, []int64{g.ID})

	// 真实仓储：让 UpdateWithSettings 走真实的 UpdateSettingsAndSyncAccounts（含
	// lockLiveGroupsForUpdate），而非 testDBRepo/qkgDBSettingRepo 的替身。
	repo := repository.NewQuotaKeyRepository(client)
	svc := service.NewQuotaKeyService(repo, service.NewCredcryptEncryptor(), nil, client)

	const rounds = 30
	modelA := "gpt-6.1-sol"       // OpenAI 家族，composite 分组会追加
	modelB := "claude-sonnet-4-6" // Anthropic 家族，composite 分组会追加

	// 启动屏障：让所有 goroutine 几乎同时开跑，最大化分组行锁的交错竞争。
	start := make(chan struct{})
	var ready sync.WaitGroup
	var wg sync.WaitGroup
	errs := make(chan error, rounds*2)

	for r := 0; r < rounds; r++ {
		wg.Add(2)
		ready.Add(2)
		go func() {
			defer wg.Done()
			ready.Done()
			<-start
			_, e := svc.UpdateWithSettings(ctx, qk1, nil, nil, nil, nil,
				&service.UpdateQuotaKeySettingsInput{
					GroupIDs:        &[]int64{g.ID},
					ModelMappingSet: true,
					ModelMapping:    map[string]string{modelA: modelA},
				})
			errs <- e
		}()
		go func() {
			defer wg.Done()
			ready.Done()
			<-start
			_, e := svc.UpdateWithSettings(ctx, qk2, nil, nil, nil, nil,
				&service.UpdateQuotaKeySettingsInput{
					GroupIDs:        &[]int64{g.ID},
					ModelMappingSet: true,
					ModelMapping:    map[string]string{modelB: modelB},
				})
			errs <- e
		}()
	}
	ready.Wait()
	close(start)
	wg.Wait()
	close(errs)

	// 1) 无死锁：每笔都必须成功。
	failed := 0
	for e := range errs {
		if e != nil {
			failed++
			t.Logf("UpdateWithSettings returned error: %v", e)
		}
	}
	require.Zero(t, failed, "并发 UpdateWithSettings 不得出现死锁/失败（锁升级应已消除）")

	// 2) 无丢更新：分组 allowlist 同时包含两个模型名。appendModelsToBoundGroupAllowlists
	// 的 FOR UPDATE 串行化了读写，后提交者读到前者已追加的结果，不会相互覆盖。
	got, err := client.Group.Get(ctx, g.ID)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{modelA, modelB}, got.ModelAllowlist.Models,
		"两个激活码并发追加后，分组 allowlist 须同时包含两个模型（无丢更新）")
}
