//go:build unit

package service

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

// quotaCapRealInsertRepo 在假 repo 的内存落库之外，同步把账号行写入真实 Postgres，
// 使"计数"查询（真实 DB）能观察到本次插入。这样并发用例才能验证"计数 + 插入"在
// 同一 advisory 锁临界区内原子完成：后到的并发请求必须看到先到者的插入，而非越过上限。
type quotaCapRealInsertRepo struct {
	upstreamBillingProbeAccountRepo
	client *ent.Client
}

func (r *quotaCapRealInsertRepo) Create(ctx context.Context, a *Account) error {
	if err := r.upstreamBillingProbeAccountRepo.Create(ctx, a); err != nil {
		return err
	}
	if a.QuotaKeyID == nil {
		return nil
	}
	create := r.client.Account.Create().
		SetName(a.Name).
		SetPlatform(a.Platform).
		SetType(a.Type).
		SetCredentials(a.Credentials).
		SetQuotaKeyID(*a.QuotaKeyID)
	if _, err := create.Save(ctx); err != nil {
		return err
	}
	return nil
}

// newQuotaKeyCapAdminServiceWithRepo 用指定账号仓储构造 admin 服务（计数走真实 DB）。
func newQuotaKeyCapAdminServiceWithRepo(t *testing.T, repo AccountRepository) *adminServiceImpl {
	t.Helper()
	client := quotaKeyTestEntClient(t)
	return &adminServiceImpl{accountRepo: repo, entClient: client}
}

// TestCursorCreateAccountConcurrentTwoAtCapBoundary 锁定终审 #1：在 MaxAccountsPerQuotaKey-1
// （14/15）处并发两请求，恰一个成功、一个 400（QUOTA_KEY_ACCOUNT_LIMIT_REACHED），
// 且真实 DB 中该激活码账号数恒为 15（不可突破）。
func TestCursorCreateAccountConcurrentTwoAtCapBoundary(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	mustCreateAccounts(t, client, qk.ID, MaxAccountsPerQuotaKey-1)

	repo := &quotaCapRealInsertRepo{client: client}
	svc := newQuotaKeyCapAdminServiceWithRepo(t, repo)

	const n = 2
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := svc.CreateAccount(context.Background(), cursorCreateInput(qk.ID))
			results[idx] = err
		}(i)
	}
	wg.Wait()

	success, rejected := 0, 0
	for _, err := range results {
		if err == nil {
			success++
			continue
		}
		require.Equal(t, http.StatusBadRequest, infraerrors.Code(err), "concurrent over-cap create must be rejected with 400")
		require.Equal(t, "QUOTA_KEY_ACCOUNT_LIMIT_REACHED", infraerrors.Reason(err))
		rejected++
	}
	require.Equal(t, 1, success, "exactly one of the two concurrent requests must succeed")
	require.Equal(t, 1, rejected, "exactly one of the two concurrent requests must be rejected")

	realCount, err := client.Account.Query().Where(account.QuotaKeyIDEQ(qk.ID)).Count(context.Background())
	require.NoError(t, err)
	require.Equal(t, MaxAccountsPerQuotaKey, realCount, "concurrent create must not breach the 15-account cap")
}

// TestCursorCreateAccountSequentialFillsToCapThenRejects 锁定：从空号池顺序建号，
// 满 15 后第 16 个必被拒（400），真实 DB 恒为 15。
func TestCursorCreateAccountSequentialFillsToCapThenRejects(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)

	repo := &quotaCapRealInsertRepo{client: client}
	svc := newQuotaKeyCapAdminServiceWithRepo(t, repo)
	ctx := context.Background()

	for i := 0; i < MaxAccountsPerQuotaKey; i++ {
		created, err := svc.CreateAccount(ctx, cursorCreateInput(qk.ID))
		require.NoError(t, err, "creating below the cap must succeed")
		require.NotNil(t, created)
	}

	_, err := svc.CreateAccount(ctx, cursorCreateInput(qk.ID))
	require.Error(t, err, "the 16th account under a full quota key must be rejected")
	require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
	require.Equal(t, "QUOTA_KEY_ACCOUNT_LIMIT_REACHED", infraerrors.Reason(err))

	realCount, err := client.Account.Query().Where(account.QuotaKeyIDEQ(qk.ID)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, MaxAccountsPerQuotaKey, realCount, "no account may be inserted beyond the cap")

	// 顺带证明锁随事务释放：上限满后锁已释放，不影响同库其它激活码独立建号。
	other := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	mustCreateAccounts(t, client, other.ID, MaxAccountsPerQuotaKey-1)
	created, err := svc.CreateAccount(ctx, cursorCreateInput(other.ID))
	require.NoError(t, err, "a different quota key must not be blocked by the first key's cap")
	require.NotNil(t, created)
}
