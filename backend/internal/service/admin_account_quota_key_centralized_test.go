//go:build unit

package service

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	dbaccountgroup "github.com/Wei-Shaw/sub2api/ent/accountgroup"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/pkg/credcrypt"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// D-CURSOR-034（用户裁定 A）：≤15 强制集中化后的三条写路径验证。
// 三条路径（拉号插入 / admin 建号 / admin 改绑）必须共用同一
// advisory 互斥域与同一事务：判定（未软删 + active + working < 15）与写入同事务提交。
// ---------------------------------------------------------------------------

// newQuotaKeyCentralizedService 构造带真实 Postgres 的 admin 服务 + 拉号服务，
// 二者共用同一 ent 客户端，从而共用同一 advisory 锁互斥域。
func newQuotaKeyCentralizedService(t *testing.T, upstream QuotaKeyUpstreamClient) (*adminServiceImpl, *QuotaKeyService, *ent.Client, *quotaCapRealInsertRepo) {
	t.Helper()
	client := quotaKeyTestEntClient(t)
	// R6-2：公共 setup 安装固定测试主密钥，激活加密写路径；测试结束复原未配置态。
	_, err := credcrypt.Configure(quotaKeyTestCredcryptKeyB64, "")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = credcrypt.Configure("", "") })
	repo := &quotaCapRealInsertRepo{client: client}
	repo.accounts = map[int64]*Account{}
	adminSvc := &adminServiceImpl{accountRepo: repo, entClient: client}
	pullSvc := NewQuotaKeyService(&testDBRepo{client: client}, NewCredcryptEncryptor(), upstream, client)
	return adminSvc, pullSvc, client, repo
}

// TestQuotaKeyCentralizedCapacity_PullCreateRebindConcurrentExactlyOneSucceeds 锁定"三条写路径
// 共用同一集中入口"：在 MaxAccountsPerQuotaKey-1（14/15）处并发 拉号插入 / admin 建号 /
// admin 改绑，三者争抢最后一个名额——结果必须恰有一次写入成功，真实 DB 恒为 15。
func TestQuotaKeyCentralizedCapacity_PullCreateRebindConcurrentExactlyOneSucceeds(t *testing.T) {
	ctx := context.Background()
	adminSvc, pullSvc, client, repo := newQuotaKeyCentralizedService(t, &fakeUpstreamClient{
		delay: 50 * time.Millisecond,
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "pulled-edge", Platform: domain.PlatformCursor, Type: AccountTypeAPIKey, Credentials: map[string]any{}},
		}},
	})

	// 目标激活码：14/15；源激活码：承载待改绑账号（自身计数不计入目标）。
	target := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	mustCreateAccounts(t, client, target.ID, MaxAccountsPerQuotaKey-1)
	source := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	existing := mustCreateAccountForRebind(t, client, source.ID)
	repo.upstreamBillingProbeAccountRepo.accounts[existing.ID] = &Account{
		ID:         existing.ID,
		Name:       existing.Name,
		Platform:   domain.PlatformCursor,
		Type:       AccountTypeAPIKey,
		Status:     StatusActive,
		QuotaKeyID: &source.ID,
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		pullErr  error
		createOK bool
		rebindOK bool
	)
	wg.Add(3)
	// 路径一：拉号插入。
	go func() {
		defer wg.Done()
		err := pullSvc.PullAccounts(ctx, target.ID)
		mu.Lock()
		pullErr = err
		mu.Unlock()
	}()
	// 路径二：admin 建号。
	go func() {
		defer wg.Done()
		_, err := adminSvc.CreateAccount(ctx, cursorCreateInput(target.ID))
		mu.Lock()
		createOK = err == nil
		mu.Unlock()
	}()
	// 路径三：admin 改绑到目标 key（净增目标 key 计数）。
	go func() {
		defer wg.Done()
		_, err := adminSvc.UpdateAccount(ctx, existing.ID, &UpdateAccountInput{
			QuotaKeyIDSet: true,
			QuotaKeyID:    &target.ID,
		})
		mu.Lock()
		rebindOK = err == nil
		mu.Unlock()
	}()
	wg.Wait()

	require.NoError(t, pullErr, "拉号到上限/已非 active 时按既有语义返回 nil，不得上抛错误")

	// 恰一次写入成功：三条路径各自观察到的"真实写入"合计为 1。
	pulledCount, err := client.Account.Query().
		Where(account.QuotaKeyID(target.ID), account.NameEQ("pulled-edge")).
		Count(ctx)
	require.NoError(t, err)
	reboundCount, err := client.Account.Query().
		Where(account.IDIn(existing.ID), account.QuotaKeyID(target.ID)).
		Count(ctx)
	require.NoError(t, err)
	createdCount, err := client.Account.Query().
		Where(account.QuotaKeyID(target.ID), account.NameEQ("cursor-created")).
		Count(ctx)
	require.NoError(t, err)

	written := pulledCount + reboundCount + createdCount
	require.Equal(t, 1, written,
		"exactly one of pull/create/rebind must win the last slot (pulled=%d rebound=%d created=%d)",
		pulledCount, reboundCount, createdCount)
	require.Equal(t, 1, boolToInt(createOK)+boolToInt(rebindOK)+boolToInt(pulledCount > 0),
		"the winner must be the single path that reports success")

	// 硬不变量：无论如何交错，目标激活码的账号数恒为 15。
	total, err := client.Account.Query().Where(account.QuotaKeyID(target.ID)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, MaxAccountsPerQuotaKey, total, "≤15 must be unbreachable across all three write paths")
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// mustCreateAccountForRebind 在真实 DB 建一条可改绑的账号行（归属 sourceKey）。
func mustCreateAccountForRebind(t *testing.T, client *ent.Client, sourceKey int64) *ent.Account {
	t.Helper()
	acc, err := client.Account.Create().
		SetName("rebind-candidate").
		SetPlatform(domain.PlatformCursor).
		SetType(AccountTypeAPIKey).
		SetCredentials(map[string]any{"api_key": "sk-src"}).
		SetQuotaKeyID(sourceKey).
		Save(context.Background())
	require.NoError(t, err)
	return acc
}

// TestUpdateAccountRebindToFullQuotaKeyRejected 锁定 r4 #1：把账号改绑到已满的激活码
// （15/15）必须被拒——400 QUOTA_KEY_ACCOUNT_LIMIT_REACHED，且归属保持原样。
func TestUpdateAccountRebindToFullQuotaKeyRejected(t *testing.T) {
	ctx := context.Background()
	adminSvc, _, client, repo := newQuotaKeyCentralizedService(t, &fakeUpstreamClient{err: errUpstreamUnused})

	source := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	target := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	mustCreateAccounts(t, client, target.ID, MaxAccountsPerQuotaKey)
	existing := mustCreateAccountForRebind(t, client, source.ID)
	repo.accounts[existing.ID] = &Account{
		ID:         existing.ID,
		Name:       existing.Name,
		Platform:   domain.PlatformCursor,
		Type:       AccountTypeAPIKey,
		Status:     StatusActive,
		QuotaKeyID: &source.ID,
	}

	_, err := adminSvc.UpdateAccount(ctx, existing.ID, &UpdateAccountInput{
		QuotaKeyIDSet: true,
		QuotaKeyID:    &target.ID,
	})
	require.Error(t, err, "rebinding into a full quota key must be rejected")
	require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
	require.Equal(t, "QUOTA_KEY_ACCOUNT_LIMIT_REACHED", infraerrors.Reason(err))

	// 目标 key 仍是 15（没有第 16 个），且被改绑账号仍归属原 key。
	targetCount, err := client.Account.Query().Where(account.QuotaKeyID(target.ID)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, MaxAccountsPerQuotaKey, targetCount, "rebind into a full key must not be persisted")

	stored, err := client.Account.Get(ctx, existing.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.QuotaKeyID)
	require.Equal(t, source.ID, *stored.QuotaKeyID, "rejected rebind must leave the binding untouched")
}

// TestUpdateAccountRebindBelowCapAllowed 回归：目标 key 未达上限时改绑放行并落库。
func TestUpdateAccountRebindBelowCapAllowed(t *testing.T) {
	ctx := context.Background()
	adminSvc, _, client, repo := newQuotaKeyCentralizedService(t, &fakeUpstreamClient{err: errUpstreamUnused})

	source := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	target := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	mustCreateAccounts(t, client, target.ID, MaxAccountsPerQuotaKey-1)
	existing := mustCreateAccountForRebind(t, client, source.ID)
	repo.accounts[existing.ID] = &Account{
		ID:         existing.ID,
		Name:       existing.Name,
		Platform:   domain.PlatformCursor,
		Type:       AccountTypeAPIKey,
		Status:     StatusActive,
		QuotaKeyID: &source.ID,
	}

	updated, err := adminSvc.UpdateAccount(ctx, existing.ID, &UpdateAccountInput{
		QuotaKeyIDSet: true,
		QuotaKeyID:    &target.ID,
	})
	require.NoError(t, err, "rebinding below the cap must succeed")
	require.NotNil(t, updated.QuotaKeyID)
	require.Equal(t, target.ID, *updated.QuotaKeyID)

	targetCount, err := client.Account.Query().Where(account.QuotaKeyID(target.ID)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, MaxAccountsPerQuotaKey, targetCount, "successful rebind occupies exactly one slot")
}

// TestUpdateAccountRebindSoftDeletedQuotaKeyRejected 锁定"软删校验移入锁内"：
// 改绑到已软删的激活码必须被拒（400 QUOTA_KEY_NOT_FOUND），不得因为"读在锁外"而漏过。
func TestUpdateAccountRebindSoftDeletedQuotaKeyRejected(t *testing.T) {
	ctx := context.Background()
	adminSvc, _, client, repo := newQuotaKeyCentralizedService(t, &fakeUpstreamClient{err: errUpstreamUnused})

	source := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	target := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	existing := mustCreateAccountForRebind(t, client, source.ID)
	repo.accounts[existing.ID] = &Account{
		ID:         existing.ID,
		Name:       existing.Name,
		Platform:   domain.PlatformCursor,
		Type:       AccountTypeAPIKey,
		Status:     StatusActive,
		QuotaKeyID: &source.ID,
	}

	// 先插入后再软删，模拟"改绑请求与删除交错"：必须在锁内发现目标 key 已软删。
	_, err := client.QuotaKey.UpdateOneID(target.ID).SetDeletedAt(time.Now()).Save(ctx)
	require.NoError(t, err)

	_, rebindErr := adminSvc.UpdateAccount(ctx, existing.ID, &UpdateAccountInput{
		QuotaKeyIDSet: true,
		QuotaKeyID:    &target.ID,
	})
	require.Error(t, rebindErr, "rebinding onto a soft-deleted quota key must be rejected")
	require.Equal(t, http.StatusBadRequest, infraerrors.Code(rebindErr))
	require.Equal(t, "QUOTA_KEY_NOT_FOUND", infraerrors.Reason(rebindErr))
}

// TestCursorCreateAccountSoftDeletedQuotaKeyRejected 锁定建号路径的同一锁内校验：
// 归属已软删激活码建号必须 400 QUOTA_KEY_NOT_FOUND（"是否正常路径"两侧的边界）。
func TestCursorCreateAccountSoftDeletedQuotaKeyRejected(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	svc := &adminServiceImpl{accountRepo: &upstreamBillingProbeAccountRepo{}, entClient: client}

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	_, err := client.QuotaKey.UpdateOneID(qk.ID).SetDeletedAt(time.Now()).Save(ctx)
	require.NoError(t, err)

	_, err = svc.CreateAccount(ctx, cursorCreateInput(qk.ID))
	require.Error(t, err, "creating under a soft-deleted quota key must be rejected")
	require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
	require.Equal(t, "QUOTA_KEY_NOT_FOUND", infraerrors.Reason(err))
}

// errUpstreamUnused 是这些用例未走拉号时的占位上游错误（不得返回假账号）。
var errUpstreamUnused = ErrUpstreamNotConnected

// TestQuotaKeyPullInsertsAtCapBoundaryRespectsCentralizedEntry 回归自动补位截断：1/2 时拉号
// 上游返回 2 条，集中入口按当前自动目标 CursorAutoPullAccountsPerKey(2) 截断，只落 1 条
// （总数恰好 2，溢出丢弃）。
func TestQuotaKeyPullInsertsAtCapBoundaryRespectsCentralizedEntry(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	_, pullSvc, _, _ := newQuotaKeyCentralizedService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "edge-1", Platform: domain.PlatformCursor, Type: AccountTypeAPIKey, Credentials: map[string]any{}},
			{Name: "edge-2", Platform: domain.PlatformCursor, Type: AccountTypeAPIKey, Credentials: map[string]any{}},
		}},
	})

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	mustCreateAccounts(t, client, qk.ID, CursorAutoPullAccountsPerKey-1)

	require.NoError(t, pullSvc.PullAccounts(ctx, qk.ID))

	total, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, CursorAutoPullAccountsPerKey, total, "pull through the centralized entry must truncate to the auto-pull target")
	overflow, err := client.Account.Query().
		Where(account.QuotaKeyID(qk.ID), account.NameEQ("edge-2")).
		Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, overflow, "overflowing upstream accounts must be dropped")
}

// ---------------------------------------------------------------------------
// 派发单 c1p1-B2-admin-account：人工建号 / 改绑 / 编辑 cursor 账号继承并跟随激活码设置。
// 受码管辖的设置（priority / concurrency / load_factor / rate_multiplier /
// expires_at / 码级非空 model_mapping）必须随激活码，防止账号侧写路径覆盖飘移。
// ---------------------------------------------------------------------------

// quotaCapManagedSettingsRepo 在假 repo 的内存落库之外，把账号的全部受码管辖列
// （priority / concurrency / load_factor / rate_multiplier / expires_at /
// credentials[model_mapping]）同步写入真实 Postgres，使新用例能断言"继承并跟随激活码"
// 的设置真实落库（不止内存对象）。
type quotaCapManagedSettingsRepo struct {
	upstreamBillingProbeAccountRepo
	client *ent.Client
}

func (r *quotaCapManagedSettingsRepo) Create(ctx context.Context, a *Account) error {
	return r.persistManagedAccount(ctx, nil, a)
}

func (r *quotaCapManagedSettingsRepo) CreateWithTx(ctx context.Context, tx *ent.Tx, a *Account) error {
	return r.persistManagedAccount(ctx, tx, a)
}

func (r *quotaCapManagedSettingsRepo) Update(ctx context.Context, a *Account) error {
	r.upstreamBillingProbeAccountRepo.Update(ctx, a)
	if a.QuotaKeyID == nil {
		return nil
	}
	execClient := r.client
	if tx := ent.TxFromContext(ctx); tx != nil {
		execClient = tx.Client()
	}
	update := execClient.Account.UpdateOneID(a.ID).SetQuotaKeyID(*a.QuotaKeyID)
	update.SetPriority(a.Priority)
	update.SetConcurrency(a.Concurrency)
	if a.LoadFactor != nil {
		update.SetLoadFactor(*a.LoadFactor)
	}
	if a.RateMultiplier != nil {
		update.SetRateMultiplier(*a.RateMultiplier)
	}
	if a.ExpiresAt != nil {
		update.SetExpiresAt(*a.ExpiresAt)
	}
	update.SetCredentials(a.Credentials)
	_, err := update.Save(ctx)
	return err
}

func (r *quotaCapManagedSettingsRepo) persistManagedAccount(ctx context.Context, tx *ent.Tx, a *Account) error {
	if err := r.upstreamBillingProbeAccountRepo.Create(ctx, a); err != nil {
		return err
	}
	if a.QuotaKeyID == nil {
		return nil
	}
	execClient := r.client
	if tx != nil {
		execClient = tx.Client()
	}
	create := execClient.Account.Create().
		SetName(a.Name).
		SetPlatform(a.Platform).
		SetType(a.Type).
		SetCredentials(a.Credentials).
		SetQuotaKeyID(*a.QuotaKeyID).
		SetPriority(a.Priority).
		SetConcurrency(a.Concurrency)
	if a.LoadFactor != nil {
		create.SetLoadFactor(*a.LoadFactor)
	}
	if a.RateMultiplier != nil {
		create.SetRateMultiplier(*a.RateMultiplier)
	}
	if a.ExpiresAt != nil {
		create.SetExpiresAt(*a.ExpiresAt)
	}
	if _, err := create.Save(ctx); err != nil {
		return err
	}
	return nil
}

// mustCreateManagedQuotaKey 创建带完整受码管辖设置的激活码（供继承测试）。
func mustCreateManagedQuotaKey(t *testing.T, client *ent.Client, status string, priority, concurrency, loadFactor int, rateMultiplier float64, expiresAt time.Time, modelMapping map[string]string) *ent.QuotaKey {
	t.Helper()
	qk, err := client.QuotaKey.Create().
		SetCodeEncrypted("enc:v1:test").
		SetStatus(status).
		SetPriority(priority).
		SetConcurrency(concurrency).
		SetLoadFactor(loadFactor).
		SetRateMultiplier(rateMultiplier).
		SetExpiresAt(expiresAt).
		SetModelMapping(modelMapping).
		Save(context.Background())
	require.NoError(t, err)
	return qk
}

func managedSettingsQKAdminService(t *testing.T) (*adminServiceImpl, *quotaCapManagedSettingsRepo, *ent.Client) {
	t.Helper()
	client := quotaKeyTestEntClient(t)
	repo := &quotaCapManagedSettingsRepo{client: client}
	repo.upstreamBillingProbeAccountRepo.accounts = map[int64]*Account{}
	return &adminServiceImpl{accountRepo: repo, entClient: client}, repo, client
}

// TestCursorCreateAccountInheritsQuotaKeyManagedSettings 锁定人工建号：绑定激活码后新账号
// 自动继承激活码管辖的 6 项设置（priority / concurrency / load_factor / rate_multiplier /
// expires_at / model_mapping），且真实落库。
func TestCursorCreateAccountInheritsQuotaKeyManagedSettings(t *testing.T) {
	ctx := context.Background()
	svc, _, client := managedSettingsQKAdminService(t)

	expiresAt := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	qk := mustCreateManagedQuotaKey(t, client, domain.QuotaKeyStatusActive, 4, 6, 80, 0.5, expiresAt, map[string]string{"gpt-6": "gpt-6"})

	created, err := svc.CreateAccount(ctx, cursorCreateInput(qk.ID))
	require.NoError(t, err, "creating under an active quota key must succeed")
	require.NotNil(t, created.QuotaKeyID)
	require.Equal(t, qk.ID, *created.QuotaKeyID)

	// 内存对象即应继承激活码管辖的 6 项设置。
	require.Equal(t, 4, created.Priority)
	require.Equal(t, 6, created.Concurrency)
	require.NotNil(t, created.LoadFactor)
	require.Equal(t, 80, *created.LoadFactor)
	require.NotNil(t, created.RateMultiplier)
	require.Equal(t, 0.5, *created.RateMultiplier)
	require.NotNil(t, created.ExpiresAt)
	require.Equal(t, expiresAt.Unix(), created.ExpiresAt.Unix())
	require.Equal(t, map[string]any{"gpt-6": "gpt-6"}, created.Credentials["model_mapping"])

	// 真实 DB 行也应落库这些值。
	dbAcc, err := client.Account.Query().
		Where(account.QuotaKeyIDEQ(qk.ID), account.NameEQ("cursor-created")).
		Only(ctx)
	require.NoError(t, err)
	require.Equal(t, 4, dbAcc.Priority)
	require.Equal(t, 6, dbAcc.Concurrency)
	require.NotNil(t, dbAcc.LoadFactor)
	require.Equal(t, 80, *dbAcc.LoadFactor)
	require.Equal(t, 0.5, dbAcc.RateMultiplier)
	require.NotNil(t, dbAcc.ExpiresAt)
	require.Equal(t, expiresAt.Unix(), dbAcc.ExpiresAt.Unix())
	require.Equal(t, map[string]any{"gpt-6": "gpt-6"}, dbAcc.Credentials["model_mapping"])
}

// TestCursorRebindInheritsTargetQuotaKeyManagedSettings 锁定人工改绑：改绑到另一激活码后
// 自动继承目标激活码管辖的 6 项设置（含 expires_at），且真实落库。
func TestCursorRebindInheritsTargetQuotaKeyManagedSettings(t *testing.T) {
	ctx := context.Background()
	svc, repo, client := managedSettingsQKAdminService(t)

	sourceExpiresAt := time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC)
	source := mustCreateManagedQuotaKey(t, client, domain.QuotaKeyStatusActive, 1, 1, 10, 0.1, sourceExpiresAt, map[string]string{"gpt-src": "gpt-src"})
	targetExpiresAt := time.Date(2031, 5, 6, 7, 8, 9, 0, time.UTC)
	target := mustCreateManagedQuotaKey(t, client, domain.QuotaKeyStatusActive, 7, 9, 55, 2.0, targetExpiresAt, map[string]string{"gpt-7": "gpt-7"})

	existing := mustCreateAccountForRebind(t, client, source.ID)
	repo.upstreamBillingProbeAccountRepo.accounts[existing.ID] = &Account{
		ID:         existing.ID,
		Name:       existing.Name,
		Platform:   domain.PlatformCursor,
		Type:       AccountTypeAPIKey,
		Status:     StatusActive,
		QuotaKeyID: &source.ID,
	}

	updated, err := svc.UpdateAccount(ctx, existing.ID, &UpdateAccountInput{
		QuotaKeyIDSet: true,
		QuotaKeyID:    &target.ID,
	})
	require.NoError(t, err, "rebinding below the cap must succeed")
	require.NotNil(t, updated.QuotaKeyID)
	require.Equal(t, target.ID, *updated.QuotaKeyID)

	// 改绑应继承目标激活码管辖的 6 项设置（含 expires_at）。
	require.Equal(t, 7, updated.Priority)
	require.Equal(t, 9, updated.Concurrency)
	require.NotNil(t, updated.LoadFactor)
	require.Equal(t, 55, *updated.LoadFactor)
	require.NotNil(t, updated.RateMultiplier)
	require.Equal(t, 2.0, *updated.RateMultiplier)
	require.NotNil(t, updated.ExpiresAt)
	require.Equal(t, targetExpiresAt.Unix(), updated.ExpiresAt.Unix())
	require.Equal(t, map[string]any{"gpt-7": "gpt-7"}, updated.Credentials["model_mapping"])

	// 真实 DB 行落库值应随目标激活码。
	dbAcc, err := client.Account.Get(ctx, existing.ID)
	require.NoError(t, err)
	require.Equal(t, target.ID, *dbAcc.QuotaKeyID)
	require.Equal(t, 7, dbAcc.Priority)
	require.Equal(t, 9, dbAcc.Concurrency)
	require.NotNil(t, dbAcc.LoadFactor)
	require.Equal(t, 55, *dbAcc.LoadFactor)
	require.Equal(t, 2.0, dbAcc.RateMultiplier)
	require.NotNil(t, dbAcc.ExpiresAt)
	require.Equal(t, targetExpiresAt.Unix(), dbAcc.ExpiresAt.Unix())
	require.Equal(t, map[string]any{"gpt-7": "gpt-7"}, dbAcc.Credentials["model_mapping"])
}

// TestAdminAccountQuotaKeyBoundUpdateFollowsManagedSettings 锁定已绑定激活码的 cursor 账号
// 编辑：即便客户端提交了与激活码冲突的 expires_at / priority / rate_multiplier，落库值仍随
// 激活码不变（受码管辖设置防覆盖飘移）。
func TestAdminAccountQuotaKeyBoundUpdateFollowsManagedSettings(t *testing.T) {
	ctx := context.Background()
	svc, repo, client := managedSettingsQKAdminService(t)

	keyExpiresAt := time.Date(2030, 3, 4, 5, 6, 7, 0, time.UTC)
	qk := mustCreateManagedQuotaKey(t, client, domain.QuotaKeyStatusActive, 4, 6, 80, 0.5, keyExpiresAt, map[string]string{"gpt-6": "gpt-6"})

	existing := mustCreateAccountForRebind(t, client, qk.ID)
	repo.upstreamBillingProbeAccountRepo.accounts[existing.ID] = &Account{
		ID:         existing.ID,
		Name:       existing.Name,
		Platform:   domain.PlatformCursor,
		Type:       AccountTypeAPIKey,
		Status:     StatusActive,
		QuotaKeyID: &qk.ID,
	}

	// 客户端企图写入与激活码冲突的值。
	inputExpiresAt := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	prio := 99
	exp := inputExpiresAt.Unix()
	rm := 9.9
	input := &UpdateAccountInput{
		Priority:       &prio,
		ExpiresAt:      &exp,
		RateMultiplier: &rm,
	}
	updated, err := svc.UpdateAccount(ctx, existing.ID, input)
	require.NoError(t, err, "updating a bound cursor account must succeed")

	// 即便客户端提交了旧字段，内存对象仍随激活码不变。
	require.Equal(t, 4, updated.Priority, "priority must follow the quota key")
	require.NotNil(t, updated.ExpiresAt)
	require.Equal(t, keyExpiresAt.Unix(), updated.ExpiresAt.Unix(), "expires_at must follow the quota key")
	require.NotNil(t, updated.RateMultiplier)
	require.Equal(t, 0.5, *updated.RateMultiplier, "rate_multiplier must follow the quota key")

	// 真实 DB 行落库值仍随激活码不变。
	dbAcc, err := client.Account.Get(ctx, existing.ID)
	require.NoError(t, err)
	require.Equal(t, 4, dbAcc.Priority)
	require.NotNil(t, dbAcc.ExpiresAt)
	require.Equal(t, keyExpiresAt.Unix(), dbAcc.ExpiresAt.Unix())
	require.Equal(t, 0.5, dbAcc.RateMultiplier)
}

// TestCursorRebindToUnmappedKeyClearsOldMapping 锁定 P1：带 model_mapping 的 cursor 账号
// 改绑到【未配置】model_mapping（NULL/空）的目标激活码后，账号 credentials 中旧来源码的
// model_mapping 限制必须被显式清除，重置为无码级限制状态（与目标码语义一致），内存对象与
// 真实 DB 行均不得残留。
func TestCursorRebindToUnmappedKeyClearsOldMapping(t *testing.T) {
	ctx := context.Background()
	svc, repo, client := managedSettingsQKAdminService(t)

	source := mustCreateManagedQuotaKey(t, client, domain.QuotaKeyStatusActive, 1, 1, 10, 0.1, time.Time{}, map[string]string{"gpt-src": "gpt-src"})
	// 目标激活码：未配置 model_mapping（NULL）。
	target := mustCreateManagedQuotaKey(t, client, domain.QuotaKeyStatusActive, 7, 9, 55, 2.0, time.Time{}, nil)

	// 账号原归属 source，凭据残留来源码的模型映射限制。
	existing, err := client.Account.Create().
		SetName("rebind-candidate").
		SetPlatform(domain.PlatformCursor).
		SetType(AccountTypeAPIKey).
		SetCredentials(map[string]any{"api_key": "sk-src", "model_mapping": map[string]any{"gpt-src": "gpt-src"}}).
		SetQuotaKeyID(source.ID).
		Save(ctx)
	require.NoError(t, err)
	repo.upstreamBillingProbeAccountRepo.accounts[existing.ID] = &Account{
		ID:         existing.ID,
		Name:       existing.Name,
		Platform:   domain.PlatformCursor,
		Type:       AccountTypeAPIKey,
		Status:     StatusActive,
		QuotaKeyID: &source.ID,
	}

	updated, err := svc.UpdateAccount(ctx, existing.ID, &UpdateAccountInput{
		QuotaKeyIDSet: true,
		QuotaKeyID:    &target.ID,
	})
	require.NoError(t, err, "rebinding to an unmapped quota key must succeed")
	require.NotNil(t, updated.QuotaKeyID)
	require.Equal(t, target.ID, *updated.QuotaKeyID)

	// 改绑后账号内存对象的 model_mapping 必须被清除（不再残留来源码限制）。
	_, stillHasMapping := updated.Credentials["model_mapping"]
	require.False(t, stillHasMapping, "model_mapping must be cleared from the account after rebinding to an unmapped quota key")

	// 真实 DB 行也应清除 model_mapping。
	dbAcc, err := client.Account.Get(ctx, existing.ID)
	require.NoError(t, err)
	_, dbHasMapping := dbAcc.Credentials["model_mapping"]
	require.False(t, dbHasMapping, "model_mapping must be cleared in the persisted row")

	// 反向回归：目标码确实未配 model_mapping（确认不是因为目标码自己也配了）。
	targetQK, err := client.QuotaKey.Get(ctx, target.ID)
	require.NoError(t, err)
	require.Equal(t, 0, len(targetQK.ModelMapping), "target quota key must remain unmapped")
}

// TestAdminAccountQuotaKeyBoundUpdateFollowsManagedSettingsConcurrentLock 锁定 P2：已绑定
// 激活码的 cursor 账号走普通编辑（非改绑）时，写入被包裹在激活码锁事务内——锁内重读激活码
// 当前权威值并应用，再于同一事务落库，杜绝 TOCTOU 竞态：管理员并发更新激活码设置时，编辑
// 不得拿锁外旧设置写回、覆盖刚刚同步的新值。
//
// 该用例以"先持同一 advisory 锁更新激活码 priority=99 并提交"模拟并发管理员更新；编辑事务
// 在锁内重读，必须读到新值 99 并落库，而非把并发更新覆盖回旧值 4。
func TestAdminAccountQuotaKeyBoundUpdateFollowsManagedSettingsConcurrentLock(t *testing.T) {
	ctx := context.Background()
	svc, repo, client := managedSettingsQKAdminService(t)

	keyExpiresAt := time.Date(2030, 3, 4, 5, 6, 7, 0, time.UTC)
	qk := mustCreateManagedQuotaKey(t, client, domain.QuotaKeyStatusActive, 4, 6, 80, 0.5, keyExpiresAt, map[string]string{"gpt-6": "gpt-6"})

	existing := mustCreateAccountForRebind(t, client, qk.ID)
	repo.upstreamBillingProbeAccountRepo.accounts[existing.ID] = &Account{
		ID:         existing.ID,
		Name:       existing.Name,
		Platform:   domain.PlatformCursor,
		Type:       AccountTypeAPIKey,
		Status:     StatusActive,
		QuotaKeyID: &qk.ID,
	}

	// 并发：复用同一 advisory 互斥锁先更新激活码设置（priority 4 -> 99）并提交释放；
	// 编辑在锁外无法读取到该更新，必须于锁内重读才拿得到新值。
	errCh := make(chan error, 1)
	go func() {
		errCh <- withQuotaKeyLockTx(ctx, ctx, client, qk.ID, func(tx *ent.Tx) error {
			_, e := tx.QuotaKey.UpdateOneID(qk.ID).SetPriority(99).Save(ctx)
			return e
		})
	}()
	// 让并发更新先持锁并提交（advisory 锁串行化：编辑事务在此阻塞直到更新提交）。
	time.Sleep(100 * time.Millisecond)

	// 普通编辑，不提交任何冲突字段：必须跟随锁内重读到的激活码新设置。
	updated, err := svc.UpdateAccount(ctx, existing.ID, &UpdateAccountInput{})
	require.NoError(t, err, "updating a bound cursor account must succeed")
	require.NoError(t, <-errCh, "concurrent quota key update must commit")

	// 编辑在锁内重读到 priority=99，落库值应随新设置，而非把并发更新覆盖回旧值 4。
	require.Equal(t, 99, updated.Priority, "edit must pick up the concurrent quota key update inside the lock")
	dbAcc, err := client.Account.Get(ctx, existing.ID)
	require.NoError(t, err)
	require.Equal(t, 99, dbAcc.Priority, "persisted priority must reflect the concurrent quota key update")

	// 校验激活码侧 priority 确实已被并发更新改为 99（确认不是编辑自己改的）。
	qkAfter, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.Equal(t, 99, qkAfter.Priority)
}

// TestAdminAccountQuotaKeyBoundUpdateAppliesClearedConcurrencyDefault3 回归 c1p1-fix-review-r2 P2：
// 当激活码码级并发数被清空（quota_keys.concurrency = NULL）并把名下账号同步恢复为默认并发 3
// 后，若有并发的 UpdateAccount 编辑在锁外先读了旧账号快照（Concurrency=7），随后在
// withQuotaKeyLockTx 锁内虽重读到 qk.Concurrency == nil，applyQuotaKeyManagedAccountSettings
// 必须显式把 account.Concurrency 重置为默认 3，绝不能把旧值 7 写回 DB，撤销清空操作。
func TestAdminAccountQuotaKeyBoundUpdateAppliesClearedConcurrencyDefault3(t *testing.T) {
	ctx := context.Background()
	svc, repo, client := managedSettingsQKAdminService(t)

	// 激活码先带并发数 7（此时账号随码继承 7），随后管理员清空码级并发数为 NULL。
	qk := mustCreateManagedQuotaKey(t, client, domain.QuotaKeyStatusActive, 4, 7, 80, 0.5, time.Time{}, nil)

	// 账号原快照：Concurrency=7（锁外旧读）；DB 行同样为 7（清空前的同步值）。
	existing, err := client.Account.Create().
		SetName("concurrency-cleared-account").
		SetPlatform(domain.PlatformCursor).
		SetType(AccountTypeAPIKey).
		SetCredentials(map[string]any{"api_key": "sk-old"}).
		SetQuotaKeyID(qk.ID).
		SetConcurrency(7).
		Save(ctx)
	require.NoError(t, err)
	repo.upstreamBillingProbeAccountRepo.accounts[existing.ID] = &Account{
		ID:          existing.ID,
		Name:        existing.Name,
		Platform:    domain.PlatformCursor,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		QuotaKeyID:  &qk.ID,
		Concurrency: 7, // 锁外旧快照：编辑前账号仍持旧并发 7。
	}

	// 模拟激活码清空码级并发数（NULL）：UpdateSettingsAndSyncAccounts 已将该码名下账号
	// DB 同步为默认并发 3（这里直接清空码并发以刻画"锁内读到 NULL"的权威状态）。
	_, err = client.QuotaKey.UpdateOneID(qk.ID).ClearConcurrency().Save(ctx)
	require.NoError(t, err)

	// 并发编辑（如改备注）：必须跟随锁内重读到的"码级并发已清空"，把账号并发重置为默认 3。
	notes := "tweak notes only"
	updated, err := svc.UpdateAccount(ctx, existing.ID, &UpdateAccountInput{Notes: &notes})
	require.NoError(t, err, "editing a bound cursor account after concurrency cleared must succeed")
	require.Equal(t, 3, updated.Concurrency, "cleared quota key concurrency must reset account to default 3, not write back old 7")
	require.NotNil(t, updated.Notes, "notes edit must be preserved")
	require.Equal(t, notes, *updated.Notes, "notes edit must be preserved")

	// 真实 DB 行落库并发必须为 3（旧值 7 绝不可写回）。
	dbAcc, err := client.Account.Get(ctx, existing.ID)
	require.NoError(t, err)
	require.Equal(t, 3, dbAcc.Concurrency, "persisted concurrency must be the default 3 after code-level concurrency cleared")

	// 反向校验：激活码侧 concurrency 确实为 NULL（确认不是编辑把值写成非零）。
	qkAfter, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.Nil(t, qkAfter.Concurrency, "quota key concurrency must remain NULL (cleared)")
}

// ---------------------------------------------------------------------------
// 派发单 QKG-C3：批量编辑禁止写穿码级受管字段。
// 单账号路径（R3b）已用覆盖而非 400 的方式守住码级分组权威；批量路径命中受管字段必须整笔
// 400 拒绝，且守卫在任何仓储写入之前返回（仓储写计数为 0）。
// ---------------------------------------------------------------------------

// BindGroups 让 quotaCapManagedSettingsRepo 满足 AccountRepository 接口，并把分组绑定真实落库
// （删除旧绑定 + 建新绑定），使 QKG-C3 单账号用例能断言"落库分组 = 码级权威 group_ids"。
func (r *quotaCapManagedSettingsRepo) BindGroups(ctx context.Context, accountID int64, groupIDs []int64) error {
	execClient := r.client
	if tx := ent.TxFromContext(ctx); tx != nil {
		execClient = tx.Client()
	}
	if _, err := execClient.AccountGroup.Delete().Where(dbaccountgroup.AccountIDEQ(accountID)).Exec(ctx); err != nil {
		return err
	}
	if len(groupIDs) == 0 {
		return nil
	}
	builders := make([]*ent.AccountGroupCreate, 0, len(groupIDs))
	for i, gid := range groupIDs {
		builders = append(builders, execClient.AccountGroup.Create().
			SetAccountID(accountID).
			SetGroupID(gid).
			SetPriority(i+1))
	}
	if _, err := execClient.AccountGroup.CreateBulk(builders...).Save(ctx); err != nil {
		return err
	}
	return nil
}

// quotaKeyTestGroupRepo 是 GroupRepository 的最小测试实现：仅 ExistsByIDs 返回全部存在
// （让"请求带 group_ids=[g9]"通过分组存在性校验），其余方法返回零值。
type quotaKeyTestGroupRepo struct{}

func (r *quotaKeyTestGroupRepo) Create(context.Context, *Group) error { return nil }
func (r *quotaKeyTestGroupRepo) GetByID(context.Context, int64) (*Group, error) {
	return &Group{}, nil
}
func (r *quotaKeyTestGroupRepo) GetByIDLite(context.Context, int64) (*Group, error) {
	return &Group{}, nil
}
func (r *quotaKeyTestGroupRepo) Update(context.Context, *Group) error { return nil }
func (r *quotaKeyTestGroupRepo) Delete(context.Context, int64) error  { return nil }
func (r *quotaKeyTestGroupRepo) DeleteCascade(context.Context, int64) ([]int64, error) {
	return nil, nil
}
func (r *quotaKeyTestGroupRepo) List(context.Context, pagination.PaginationParams) ([]Group, *pagination.PaginationResult, error) {
	return nil, &pagination.PaginationResult{}, nil
}
func (r *quotaKeyTestGroupRepo) ListWithFilters(context.Context, pagination.PaginationParams, string, string, string, *bool) ([]Group, *pagination.PaginationResult, error) {
	return nil, &pagination.PaginationResult{}, nil
}
func (r *quotaKeyTestGroupRepo) ListActive(context.Context) ([]Group, error) { return nil, nil }
func (r *quotaKeyTestGroupRepo) ListActiveByPlatform(context.Context, string) ([]Group, error) {
	return nil, nil
}
func (r *quotaKeyTestGroupRepo) ExistsByName(context.Context, string) (bool, error) {
	return false, nil
}
func (r *quotaKeyTestGroupRepo) GetAccountCount(context.Context, int64) (int64, int64, error) {
	return 0, 0, nil
}
func (r *quotaKeyTestGroupRepo) DeleteAccountGroupsByGroupID(context.Context, int64) (int64, error) {
	return 0, nil
}
func (r *quotaKeyTestGroupRepo) GetAccountIDsByGroupIDs(context.Context, []int64) ([]int64, error) {
	return nil, nil
}
func (r *quotaKeyTestGroupRepo) BindAccountsToGroup(context.Context, int64, []int64) error {
	return nil
}
func (r *quotaKeyTestGroupRepo) UpdateSortOrders(context.Context, []GroupSortOrderUpdate) error {
	return nil
}
func (r *quotaKeyTestGroupRepo) ExistsByIDs(_ context.Context, ids []int64) (map[int64]bool, error) {
	m := make(map[int64]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m, nil
}

// TestBulkUpdateAccounts_QuotaKeyBoundCursorManagedFieldRejected 锁定派发单 QKG-C3：
// 批量目标含 Cursor 绑定码账号且请求命中码级受管字段（concurrency）时，必须整笔 400 拒绝，
// 且守卫在任何仓储写入之前返回（BulkUpdate 调用计数 0）。
func TestBulkUpdateAccounts_QuotaKeyBoundCursorManagedFieldRejected(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{}
	svc := &adminServiceImpl{accountRepo: repo}

	qkID := int64(42)
	repo.getByIDsAccounts = []*Account{
		{ID: 1, Platform: PlatformCursor, QuotaKeyID: &qkID},
	}
	conc := 5
	input := &BulkUpdateAccountsInput{
		AccountIDs:  []int64{1},
		Concurrency: &conc,
	}

	_, err := svc.BulkUpdateAccounts(context.Background(), input)
	require.Error(t, err, "bulk edit of a quota-key-bound cursor account managed field must be rejected")
	require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
	require.Equal(t, "CURSOR_QUOTA_KEY_MANAGED_BULK_EDIT_FORBIDDEN", infraerrors.Reason(err))
	require.Equal(t, 0, repo.bulkUpdateCalls, "guard must reject before any repository write")
	require.Equal(t, 0, len(repo.bindGroupsCalls), "no group binding may occur on rejection")
}

// TestBulkUpdateAccounts_QuotaKeyBoundCursorNonManagedAllowed 锁定派发单 QKG-C3 反向用例：
// 同一 Cursor 绑定码账号，仅改非受管字段（Status / Schedulable / ProxyID）时批量必须成功，
// 守卫不得误伤。
func TestBulkUpdateAccounts_QuotaKeyBoundCursorNonManagedAllowed(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{}
	svc := &adminServiceImpl{accountRepo: repo}

	qkID := int64(42)
	repo.getByIDsAccounts = []*Account{
		{ID: 1, Platform: PlatformCursor, QuotaKeyID: &qkID},
	}
	status := StatusActive
	schedulable := true
	proxyID := int64(7)
	input := &BulkUpdateAccountsInput{
		AccountIDs:  []int64{1},
		Status:      status,
		Schedulable: &schedulable,
		ProxyID:     &proxyID,
	}

	result, err := svc.BulkUpdateAccounts(context.Background(), input)
	require.NoError(t, err, "bulk edit of non-managed fields on a quota-key-bound cursor account must succeed")
	require.Equal(t, 1, repo.bulkUpdateCalls, "non-managed bulk edit must reach the repository write")
	require.Equal(t, 1, result.Success)
}

// TestBulkUpdateAccounts_NonCursorConcurrencyAllowed 锁定派发单 QKG-C3 不误伤：
// 目标全是非 Cursor 账号（如 openai）且请求 concurrency 时批量必须成功，守卫不得误伤。
func TestBulkUpdateAccounts_NonCursorConcurrencyAllowed(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{}
	svc := &adminServiceImpl{accountRepo: repo}

	repo.getByIDsAccounts = []*Account{
		{ID: 1, Platform: "openai", QuotaKeyID: nil},
		{ID: 2, Platform: "openai", QuotaKeyID: nil},
	}
	conc := 5
	input := &BulkUpdateAccountsInput{
		AccountIDs:  []int64{1, 2},
		Concurrency: &conc,
	}

	result, err := svc.BulkUpdateAccounts(context.Background(), input)
	require.NoError(t, err, "bulk concurrency edit on non-cursor accounts must succeed (no false positive)")
	require.Equal(t, 1, repo.bulkUpdateCalls)
	require.Equal(t, 2, result.Success)
}

// TestAdminAccountQuotaKeyBoundUpdateOverridesRequestGroupIDs 锁定派发单 QKG-C3 单账号路径
// （外审 R3b）：绑定激活码的 Cursor 账号编辑时，即便请求带 group_ids=[g9]，落库分组必须等于
// 激活码级权威 group_ids，不得出现 g9——码级分组权威以"覆盖"而非 400 方式守住，避免误伤普通账号编辑。
func TestAdminAccountQuotaKeyBoundUpdateOverridesRequestGroupIDs(t *testing.T) {
	ctx := context.Background()
	client := quotaKeyTestEntClient(t)

	// 创建请求里的 g9 分组（使"分组存在性"校验放行）与激活码级权威分组。
	g9, err := client.Group.Create().
		SetName("qkg-c3-g9").
		SetModelAllowlist(domain.GroupModelAllowlist{}).
		Save(ctx)
	require.NoError(t, err)
	authGrp, err := client.Group.Create().
		SetName("qkg-c3-auth").
		SetModelAllowlist(domain.GroupModelAllowlist{}).
		Save(ctx)
	require.NoError(t, err)

	// 激活码带码级分组（权威值）。
	qk := mustCreateManagedQuotaKey(t, client, domain.QuotaKeyStatusActive, 4, 6, 80, 0.5, time.Time{}, nil)
	_, err = client.QuotaKey.UpdateOneID(qk.ID).SetGroupIds([]int64{authGrp.ID}).Save(ctx)
	require.NoError(t, err)

	// 账号绑定该激活码（真实 DB 行 + 内存镜像）。
	existing := mustCreateAccountForRebind(t, client, qk.ID)

	repo := &quotaCapManagedSettingsRepo{client: client}
	repo.upstreamBillingProbeAccountRepo.accounts = map[int64]*Account{
		existing.ID: {
			ID:         existing.ID,
			Name:       existing.Name,
			Platform:   domain.PlatformCursor,
			Type:       AccountTypeAPIKey,
			Status:     StatusActive,
			QuotaKeyID: &qk.ID,
		},
	}
	svc := &adminServiceImpl{accountRepo: repo, entClient: client, groupRepo: &quotaKeyTestGroupRepo{}}

	// 请求带 group_ids=[g9]，企图改写绑定。
	reqG9 := []int64{g9.ID}
	_, err = svc.UpdateAccount(ctx, existing.ID, &UpdateAccountInput{GroupIDs: &reqG9})
	require.NoError(t, err, "editing a bound cursor account with group_ids must succeed (override, not 400)")

	// 落库分组必须等于码级 group_ids（authGrp.ID），且不得出现 g9。
	bound, err := client.AccountGroup.Query().Where(dbaccountgroup.AccountIDEQ(existing.ID)).All(ctx)
	require.NoError(t, err)
	got := make([]int64, 0, len(bound))
	for _, ag := range bound {
		got = append(got, ag.GroupID)
	}
	require.ElementsMatch(t, []int64{authGrp.ID}, got, "persisted groups must equal the quota-key authoritative group_ids")
	require.NotContains(t, got, g9.ID, "request group g9 must be overridden by the quota-key group_ids")
}
