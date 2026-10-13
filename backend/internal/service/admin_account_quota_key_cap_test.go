//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

// CreateWithTx 是 ≤15 集中化后 cursor 建号的事务写入口。测试替身在此以既有 Create
// 的语义实现（同一介质落库），供 service 在同一 advisory 事务内调用。
func (r *upstreamBillingProbeAccountRepo) CreateWithTx(ctx context.Context, _ *dbent.Tx, a *Account) error {
	return r.Create(ctx, a)
}

// newQuotaKeyCapAdminService 构造带真实 ent 客户端的 admin 服务（号池计数走真实 DB），
// 账号落库走假 repo（本用例只关心建号前的上限/存在性校验）。
func newQuotaKeyCapAdminService(t *testing.T) (*adminServiceImpl, func() *Account) {
	t.Helper()
	client := quotaKeyTestEntClient(t)
	repo := &upstreamBillingProbeAccountRepo{}
	return &adminServiceImpl{accountRepo: repo, entClient: client}, func() *Account {
		return repo.accounts[1]
	}
}

func cursorCreateInput(quotaKeyID int64) *CreateAccountInput {
	return &CreateAccountInput{
		Name:                  "cursor-created",
		Platform:              domain.PlatformCursor,
		Type:                  AccountTypeAPIKey,
		Credentials:           map[string]any{"api_key": "sk-test"},
		SkipDefaultGroupBind:  true,
		SkipMixedChannelCheck: true,
		QuotaKeyID:            &quotaKeyID,
	}
}

// TestCursorCreateAccountRejectedAtQuotaKeyCap 锁定 #2b：cursor 建号复用拉号侧 ≤15 上限。
// 该激活码已有 15 个 working 账号时，admin 建号必须被拒（不得建出第 16 个）。
func TestCursorCreateAccountRejectedAtQuotaKeyCap(t *testing.T) {
	svc, _ := newQuotaKeyCapAdminService(t)
	client := svc.entClient
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	mustCreateAccounts(t, client, qk.ID, MaxAccountsPerQuotaKey)

	_, err := svc.CreateAccount(context.Background(), cursorCreateInput(qk.ID))
	require.Error(t, err, "must not create a 16th account under a full quota key")
	require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
	require.Equal(t, "QUOTA_KEY_ACCOUNT_LIMIT_REACHED", infraerrors.Reason(err))

	n, err := client.Account.Query().Where(account.QuotaKeyIDEQ(qk.ID)).Count(context.Background())
	require.NoError(t, err)
	require.Equal(t, MaxAccountsPerQuotaKey, n, "no account must be inserted beyond the cap")
}

// TestCursorCreateAccountAllowedBelowQuotaKeyCap 锁定：未达上限（14/15）时 admin 建号放行。
func TestCursorCreateAccountAllowedBelowQuotaKeyCap(t *testing.T) {
	svc, _ := newQuotaKeyCapAdminService(t)
	client := svc.entClient
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	mustCreateAccounts(t, client, qk.ID, MaxAccountsPerQuotaKey-1)

	created, err := svc.CreateAccount(context.Background(), cursorCreateInput(qk.ID))
	require.NoError(t, err, "creating below the cap must be allowed")
	require.NotNil(t, created)
	require.NotNil(t, created.QuotaKeyID)
	require.Equal(t, qk.ID, *created.QuotaKeyID)
}

// TestCursorCreateAccountCapCountsOnlyWorkingAccounts 锁定计数口径：软删账号不计入
// （§3.2 working 谓词与 QuotaKeyRepository.CountWorkingAccounts 一致）。
func TestCursorCreateAccountCapCountsOnlyWorkingAccounts(t *testing.T) {
	svc, _ := newQuotaKeyCapAdminService(t)
	client := svc.entClient
	ctx := context.Background()
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	mustCreateAccounts(t, client, qk.ID, MaxAccountsPerQuotaKey)

	ids, err := client.Account.Query().Where(account.QuotaKeyIDEQ(qk.ID)).IDs(ctx)
	require.NoError(t, err)
	require.Len(t, ids, MaxAccountsPerQuotaKey)
	_, err = client.Account.UpdateOneID(ids[0]).SetDeletedAt(time.Now()).Save(ctx)
	require.NoError(t, err)

	// 15 行中有 1 个已软删 → working=14 < 15，允许建号。
	_, err = svc.CreateAccount(ctx, cursorCreateInput(qk.ID))
	require.NoError(t, err, "soft-deleted accounts must not count toward the working cap")
}

// TestCreateAccountRejectsUnknownQuotaKey 锁定"提供值 → 校验该 key 存在"：
// 指向不存在的激活码必须 400，而不是让 FK 违规退化成 500。
func TestCreateAccountRejectsUnknownQuotaKey(t *testing.T) {
	svc, _ := newQuotaKeyCapAdminService(t)
	client := svc.entClient
	ctx := context.Background()

	missing := int64(0)
	ids, err := client.QuotaKey.Query().IDs(ctx)
	require.NoError(t, err)
	for _, id := range ids {
		if id > missing {
			missing = id
		}
	}
	missing += 100000

	_, err = svc.CreateAccount(ctx, cursorCreateInput(missing))
	require.Error(t, err, "creating with an unknown quota_key_id must be rejected")
	require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
	require.Equal(t, "QUOTA_KEY_NOT_FOUND", infraerrors.Reason(err))
}

// TestNonCursorCreateAccountSkipsWorkingCap 锁定：≤15 上限只约束 cursor 建号，
// 其它平台带归属建号不受号池上限影响。
func TestNonCursorCreateAccountSkipsWorkingCap(t *testing.T) {
	svc, _ := newQuotaKeyCapAdminService(t)
	client := svc.entClient
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	mustCreateAccounts(t, client, qk.ID, MaxAccountsPerQuotaKey)

	_, err := svc.CreateAccount(context.Background(), &CreateAccountInput{
		Name:                  "openai-created",
		Platform:              PlatformOpenAI,
		Type:                  AccountTypeAPIKey,
		Credentials:           map[string]any{"api_key": "sk-test"},
		SkipDefaultGroupBind:  true,
		SkipMixedChannelCheck: true,
		QuotaKeyID:            &qk.ID,
	})
	require.NoError(t, err, "non-cursor accounts are not subject to the cursor quota key cap")
}
