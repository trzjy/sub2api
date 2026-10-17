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
	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
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

// TestQuotaKeyPullInsertsAtCapBoundaryRespectsCentralizedEntry 回归正常路径：14/15 时拉号上游
// 返回 2 条，集中入口把我们限制到只落 1 条（总数恰好 15，溢出丢弃）。
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
	mustCreateAccounts(t, client, qk.ID, MaxAccountsPerQuotaKey-1)

	require.NoError(t, pullSvc.PullAccounts(ctx, qk.ID))

	total, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, MaxAccountsPerQuotaKey, total, "pull through the centralized entry must truncate to the cap")
	overflow, err := client.Account.Query().
		Where(account.QuotaKeyID(qk.ID), account.NameEQ("edge-2")).
		Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, overflow, "overflowing upstream accounts must be dropped")
}
