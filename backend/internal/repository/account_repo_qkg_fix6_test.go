//go:build unit

package repository

import (
	"context"
	"database/sql"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/ent/accountgroup"
	"github.com/Wei-Shaw/sub2api/ent/quotakey"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// QKG-FIX-6：账号分组绑定收进码级锁事务。
// 本文件在 repository 包内复用真实 accountRepository / quotaKeyRepository 与真实
// admin 服务（service.NewAdminService），端到端验证：
//   1) 并发「建号（带 g2）」与「把激活码 A 的 group_ids 改为 g3」后，
//      quota_keys.group_ids 与名下账号的 account_groups 必须原子一致、不分叉；
//   2) 未绑码 / 非 cursor 账号建号·编辑仍按请求分组绑定（事务外路径不变）；
//   3) 事务内绑定失败 → 账号整体不落库（回滚）；
//   4) 仓储 BindGroups 事务感知：命中外部事务则复用其 client 读+写且不自开/不提交
//      （outbox 也落在同一事务），未命中维持自开事务+提交。
// 测试库与 service 包（默认 quota_test）隔离：本文件直接使用 QUOTA_TEST_DSN
// 注入的隔离库（外部已建表），与 quota_key_repository_test.go 同一基建。
// ---------------------------------------------------------------------------

var (
	qkgRepoTestOnce sync.Once
	qkgRepoClient   *dbent.Client
	qkgRepoDB       *sql.DB
	qkgRepoInitErr  error
)

// qkgRepoTestEntClient 返回连接 QUOTA_TEST_DSN 隔离库的 ent client（懒初始化，幂等建表）。
// 照抄同包 quota_key_repository_test.go 的基建：QUOTA_TEST_DSN 直接注入（含目标库），
// 不重写路径；Schema.Create 只补 ent 模型管理的表/列，scheduler_outbox 等 raw 表
// 由外部迁移建好，测试以 raw SQL 访问。
func qkgRepoTestEntClient(t *testing.T) *dbent.Client {
	t.Helper()
	qkgRepoTestOnce.Do(func() {
		dsn := os.Getenv("QUOTA_TEST_DSN")
		if dsn == "" {
			dsn = "postgres://quota_test:quota_test_pw@127.0.0.1:5432/quota_test?sslmode=disable"
		}
		db, oerr := sql.Open("postgres", dsn)
		if oerr != nil {
			qkgRepoInitErr = oerr
			return
		}
		if pingErr := db.Ping(); pingErr != nil {
			_ = db.Close()
			qkgRepoInitErr = pingErr
			return
		}
		drv := entsql.OpenDB(dialect.Postgres, db)
		client := dbent.NewClient(dbent.Driver(drv))
		if sErr := client.Schema.Create(context.Background()); sErr != nil {
			_ = db.Close()
			qkgRepoInitErr = sErr
			return
		}
		qkgRepoDB = db
		qkgRepoClient = client
	})
	if qkgRepoInitErr != nil {
		t.Fatalf("qkg repo test DB init failed: %v", qkgRepoInitErr)
	}
	return qkgRepoClient
}

// qkgRepoTestSQLDB 返回与 qkgRepoTestEntClient 同源的 *sql.DB（供 accountRepository 的 r.sql）。
func qkgRepoTestSQLDB(t *testing.T) *sql.DB {
	t.Helper()
	_ = qkgRepoTestEntClient(t) // 确保初始化完成（设置 qkgRepoDB）
	if qkgRepoDB == nil {
		t.Fatalf("qkg repo test sql db not initialized")
	}
	return qkgRepoDB
}

// noOpUpstream 仅在非拉号路径使用；不触发 PullBatch。
type noOpUpstream struct{}

func (noOpUpstream) PullBatch(context.Context, *dbent.QuotaKey) (*service.UpstreamPullResult, error) {
	return &service.UpstreamPullResult{}, nil
}
func (noOpUpstream) RealSupplyModelsFor(string) ([]string, error) { return nil, nil }
func (noOpUpstream) TargetAccountsPerKeyFor(string) (int, error)  { return 0, nil }

// qkgRealAccountService 构造带真实 Postgres + 真实仓储的 admin 服务与激活码服务。
// cfg 传 nil：ValidateAccountGroupBindings 在 cfg==nil 时跳过（见生产代码），
// 与 service 包单元测试同款用法。
func qkgRealAccountService(t *testing.T) (service.AdminService, *service.QuotaKeyService, *dbent.Client) {
	t.Helper()
	client := qkgRepoTestEntClient(t)
	sqlDB := qkgRepoTestSQLDB(t)
	// 补齐 group repository：UpdateAccount 路径在 admin_account.go:1449 无条件调用
	// validateGroupIDsExist，groupRepo 为 nil 时会报 "group repository not configured"。
	// 沿用同包 NewAdminGroupRepository（与 NewAdminAccountRepository 同基建）。
	groupRepo := NewAdminGroupRepository(client, sqlDB)
	repo := NewAdminAccountRepository(client, sqlDB, nil)
	adminSvc := service.NewAdminService(
		nil, nil, groupRepo, repo, // cfg, userRepo, groupRepo, accountRepo
		nil, nil, nil, nil, nil, nil, nil, nil, nil, // proxyRepo..authCacheInvalidator
		client,                                                     // entClient
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, // settingService..groupModelCatalogReader
	)
	qkRepo := NewQuotaKeyRepository(client)
	qkSvc := service.NewQuotaKeyService(qkRepo, service.NewCredcryptEncryptor(), &noOpUpstream{}, client)
	return adminSvc, qkSvc, client
}

func qkgMustCreateGroup(t *testing.T, client *dbent.Client, name string) int64 {
	t.Helper()
	g, err := client.Group.Create().SetName(name).Save(context.Background())
	require.NoError(t, err)
	return g.ID
}

func qkgMustCreateQuotaKeyWithGroups(t *testing.T, client *dbent.Client, groupIDs []int64) *dbent.QuotaKey {
	t.Helper()
	qk, err := client.QuotaKey.Create().
		SetCodeEncrypted("enc:v1:test").
		SetStatus(domain.QuotaKeyStatusActive).
		SetGroupIds(groupIDs).
		Save(context.Background())
	require.NoError(t, err)
	return qk
}

func qkgMustCreateAccount(t *testing.T, client *dbent.Client) int64 {
	t.Helper()
	a, err := client.Account.Create().
		SetName("qkg-repo-acc-" + strconv.FormatInt(time.Now().UnixNano(), 10)).
		SetPlatform(domain.PlatformOpenAI).
		SetType("apikey").
		SetCredentials(map[string]any{"api_key": "sk-test"}).
		SetSchedulable(true).
		SetStatus(domain.StatusActive).
		Save(context.Background())
	require.NoError(t, err)
	return a.ID
}

func qkgAccountGroupIDs(t *testing.T, client *dbent.Client, accountID int64) []int64 {
	t.Helper()
	rows, err := client.AccountGroup.Query().
		Where(accountgroup.AccountIDEQ(accountID)).
		Order(dbent.Asc("priority")).
		All(context.Background())
	require.NoError(t, err)
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.GroupID)
	}
	return ids
}

func qkgQuotaKeyGroupIDs(t *testing.T, client *dbent.Client, qkID int64) []int64 {
	t.Helper()
	qk, err := client.QuotaKey.Query().
		Where(quotakey.IDEQ(qkID)).
		Only(context.Background())
	require.NoError(t, err)
	if qk.GroupIds == nil {
		return []int64{}
	}
	return qk.GroupIds
}

func qkgAssertSameSet(t *testing.T, a, b []int64) {
	t.Helper()
	require.ElementsMatch(t, a, b, "quota_keys.group_ids 与 account_groups 必须一致（不得分叉）")
}

// TestAdminAccountQuotaKeyGroupBindingNoDivergence 并发验证 QKG-FIX-6 核心不变量：
// 激活码 A 绑定分组 g1；并发执行「建号请求带 g2」与「把 A 的 group_ids 改为 g3」后，
// 最终 quota_keys.group_ids 与名下账号的 account_groups 必须一致；多轮并允许两种线性化结果。
func TestAdminAccountQuotaKeyGroupBindingNoDivergence(t *testing.T) {
	ctx := context.Background()
	adminSvc, qkSvc, client := qkgRealAccountService(t)

	g1 := qkgMustCreateGroup(t, client, "qkg-repo-g1")
	g2 := qkgMustCreateGroup(t, client, "qkg-repo-g2")
	g3 := qkgMustCreateGroup(t, client, "qkg-repo-g3")

	const rounds = 20
	for r := 0; r < rounds; r++ {
		qk := qkgMustCreateQuotaKeyWithGroups(t, client, []int64{g1})
		accountName := "qkg-repo-create-" + strconv.Itoa(r)

		var createErr, updateErr error
		var wg sync.WaitGroup
		wg.Add(2)
		// op1：建号请求带 g2（码级权威会覆盖为 A 当前 group_ids）。
		go func() {
			defer wg.Done()
			in := &service.CreateAccountInput{
				Name:                  accountName,
				Platform:              domain.PlatformCursor,
				Type:                  service.AccountTypeAPIKey,
				Credentials:           map[string]any{"api_key": "sk-test"},
				SkipDefaultGroupBind:  true,
				SkipMixedChannelCheck: true,
				QuotaKeyID:            &qk.ID,
				GroupIDs:              []int64{g2},
			}
			_, createErr = adminSvc.CreateAccount(ctx, in)
		}()
		// op2：把 A 的 group_ids 改为 g3（事务内同步名下账号 account_groups）。
		go func() {
			defer wg.Done()
			newGroups := []int64{g3}
			_, updateErr = qkSvc.UpdateWithSettings(ctx, qk.ID, nil, nil, nil, nil,
				&service.UpdateQuotaKeySettingsInput{GroupIDs: &newGroups})
		}()
		wg.Wait()

		require.NoError(t, createErr, "round %d: create account must succeed", r)
		require.NoError(t, updateErr, "round %d: update quota key must succeed", r)

		finalQKGroups := qkgQuotaKeyGroupIDs(t, client, qk.ID)
		acc, err := client.Account.Query().
			Where(account.QuotaKeyIDEQ(qk.ID), account.DeletedAtIsNil()).
			Only(ctx)
		require.NoError(t, err, "round %d: account under A must exist", r)
		finalAccGroups := qkgAccountGroupIDs(t, client, acc.ID)
		qkgAssertSameSet(t, finalQKGroups, finalAccGroups)
	}
}

// TestCreateAccountQKGFix6UnboundBindsRequestedGroups 回归：未绑码 / 非 cursor 账号建号
// 仍按请求分组绑定（事务外路径不变）。
func TestCreateAccountQKGFix6UnboundBindsRequestedGroups(t *testing.T) {
	ctx := context.Background()
	adminSvc, _, client := qkgRealAccountService(t)

	gA := qkgMustCreateGroup(t, client, "qkg-repo-ra")
	gB := qkgMustCreateGroup(t, client, "qkg-repo-rb")

	created, err := adminSvc.CreateAccount(ctx, &service.CreateAccountInput{
		Name:                  "qkg-repo-unbound-create",
		Platform:              domain.PlatformOpenAI,
		Type:                  service.AccountTypeAPIKey,
		Credentials:           map[string]any{"api_key": "sk-test"},
		SkipDefaultGroupBind:  true,
		SkipMixedChannelCheck: true,
		GroupIDs:              []int64{gA, gB},
	})
	require.NoError(t, err)
	require.Nil(t, created.QuotaKeyID, "unbound account must not carry a quota key")

	qkgAssertSameSet(t, []int64{gA, gB}, qkgAccountGroupIDs(t, client, created.ID))
}

// TestCreateAccountQKGFix6UnboundUpdateBindsRequestedGroups 回归：未绑码账号编辑仍按请求
// 分组绑定（事务外路径不变）。
func TestCreateAccountQKGFix6UnboundUpdateBindsRequestedGroups(t *testing.T) {
	ctx := context.Background()
	adminSvc, _, client := qkgRealAccountService(t)

	gA := qkgMustCreateGroup(t, client, "qkg-repo-ua")
	gB := qkgMustCreateGroup(t, client, "qkg-repo-ub")
	gC := qkgMustCreateGroup(t, client, "qkg-repo-uc")

	created, err := adminSvc.CreateAccount(ctx, &service.CreateAccountInput{
		Name:                  "qkg-repo-unbound-update",
		Platform:              domain.PlatformOpenAI,
		Type:                  service.AccountTypeAPIKey,
		Credentials:           map[string]any{"api_key": "sk-test"},
		SkipDefaultGroupBind:  true,
		SkipMixedChannelCheck: true,
		GroupIDs:              []int64{gA, gB},
	})
	require.NoError(t, err)

	newGroups := []int64{gC}
	updated, err := adminSvc.UpdateAccount(ctx, created.ID, &service.UpdateAccountInput{
		GroupIDs:              &newGroups,
		SkipMixedChannelCheck: true,
	})
	require.NoError(t, err)
	require.Equal(t, []int64{gC}, updated.GroupIDs, "UpdateAccount 返回应反映请求分组")

	qkgAssertSameSet(t, []int64{gC}, qkgAccountGroupIDs(t, client, created.ID))
}

// TestCursorCreateAccountQKGFix6InTxBindFailureRollsBack 验证事务内绑定失败必须整体回滚：
// 激活码 A 的 group_ids 指向不存在的分组 → 事务内 BindGroups 在 lockLiveGroups 阶段失败，
// 建号事务整体回滚，账号不落库。
func TestCursorCreateAccountQKGFix6InTxBindFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	adminSvc, _, client := qkgRealAccountService(t)

	missingGroup := int64(9_999_999)
	qk := qkgMustCreateQuotaKeyWithGroups(t, client, []int64{missingGroup})

	_, err := adminSvc.CreateAccount(ctx, &service.CreateAccountInput{
		Name:                  "qkg-repo-rollback",
		Platform:              domain.PlatformCursor,
		Type:                  service.AccountTypeAPIKey,
		Credentials:           map[string]any{"api_key": "sk-test"},
		SkipDefaultGroupBind:  true,
		SkipMixedChannelCheck: true,
		QuotaKeyID:            &qk.ID,
		GroupIDs:              []int64{missingGroup},
	})
	require.Error(t, err, "in-tx bind failure must surface as error")

	count, qErr := client.Account.Query().
		Where(account.QuotaKeyIDEQ(qk.ID), account.DeletedAtIsNil()).
		Count(ctx)
	require.NoError(t, qErr)
	require.Equal(t, 0, count, "事务内绑定失败必须回滚，账号不得落库")
}

// TestBindGroups_TransactionAwareReusesCallerTx 验证生产 BindGroups 的事务感知：
// 命中外部事务时复用其 client 读+写，不自开/不提交；outbox 也落在同一事务（未提交前对其它连接不可见）。
func TestBindGroups_TransactionAwareReusesCallerTx(t *testing.T) {
	ctx := context.Background()
	client := qkgRepoTestEntClient(t)
	sqlDB := qkgRepoTestSQLDB(t)
	repo := NewAccountRepository(client, sqlDB, nil)

	accID := qkgMustCreateAccount(t, client)
	g1 := qkgMustCreateGroup(t, client, "qkg-repo-tx1")
	g2 := qkgMustCreateGroup(t, client, "qkg-repo-tx2")

	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	// 在调用方事务内绑定：必须复用 tx client，且不提交。
	require.NoError(t, repo.BindGroups(dbent.NewTxContext(ctx, tx), accID, []int64{g1, g2}))

	// 未提交前，其它连接（独立查询）看不到 account_groups 与 scheduler_outbox 的写入。
	require.Empty(t, qkgAccountGroupIDs(t, client, accID), "in-tx 绑定在提交前对其它连接不可见")
	require.Equal(t, 0, qkgOutboxCount(t, sqlDB, accID), "in-tx outbox 在提交前对其它连接不可见")

	require.NoError(t, tx.Commit())
	require.ElementsMatch(t, []int64{g1, g2}, qkgAccountGroupIDs(t, client, accID), "提交后绑定可见")
	require.Equal(t, 1, qkgOutboxCount(t, sqlDB, accID), "提交后 outbox 可见（落在同一事务）")
}

// TestBindGroups_NoTxSelfCommits 验证未命中外部事务时 BindGroups 自开事务并立即提交（既有行为不变）。
func TestBindGroups_NoTxSelfCommits(t *testing.T) {
	ctx := context.Background()
	client := qkgRepoTestEntClient(t)
	sqlDB := qkgRepoTestSQLDB(t)
	repo := NewAccountRepository(client, sqlDB, nil)

	accID := qkgMustCreateAccount(t, client)
	g3 := qkgMustCreateGroup(t, client, "qkg-repo-notx")

	require.NoError(t, repo.BindGroups(ctx, accID, []int64{g3}))
	// 没有外部事务：自开事务已提交，写入立即可见（既有行为）。
	require.ElementsMatch(t, []int64{g3}, qkgAccountGroupIDs(t, client, accID))
	require.Equal(t, 1, qkgOutboxCount(t, sqlDB, accID))
}

// qkgOutboxCount 统计 scheduler_outbox 中与该账号相关的事件数（用于验证 outbox 与绑定同事务）。
// scheduler_outbox 无 ent 实体，直接用 raw SQL 查原表（enqueueSchedulerOutbox 写入的真实表）。
func qkgOutboxCount(t *testing.T, sqlDB *sql.DB, accountID int64) int {
	t.Helper()
	var n int
	err := sqlDB.QueryRowContext(context.Background(),
		"SELECT count(*) FROM scheduler_outbox WHERE account_id = $1", accountID).Scan(&n)
	require.NoError(t, err)
	return n
}
