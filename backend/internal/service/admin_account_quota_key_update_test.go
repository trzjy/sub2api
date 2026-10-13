package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

// newQuotaKeyUpdateRepo 构造更新路径用的假 repo：预置一条账号（GetByID 返回副本、
// Update 写回），足以驱动 UpdateAccount 的归属三态分支。
func newQuotaKeyUpdateRepo(account *Account) *upstreamBillingProbeAccountRepo {
	return &upstreamBillingProbeAccountRepo{
		accounts: map[int64]*Account{account.ID: account},
	}
}

// TestUpdateAccountCursorKeepsQuotaKeyWhenNotProvided 回归 #3：cursor 账号的"不改归属
// 的编辑"必须成功。请求未提供 quota_key_id 时沿用现有归属，不得按"缺归属"判 400。
func TestUpdateAccountCursorKeepsQuotaKeyWhenNotProvided(t *testing.T) {
	accountID := int64(9101)
	qkID := int64(42)
	repo := newQuotaKeyUpdateRepo(&Account{
		ID:         accountID,
		Name:       "cursor-acc",
		Platform:   domain.PlatformCursor,
		Type:       AccountTypeAPIKey,
		Status:     StatusActive,
		QuotaKeyID: &qkID,
	})
	svc := &adminServiceImpl{accountRepo: repo}

	updated, err := svc.UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Name: "cursor-acc-renamed",
	})
	require.NoError(t, err, "editing a cursor account without touching quota_key_id must succeed")
	require.Equal(t, "cursor-acc-renamed", updated.Name)
	require.NotNil(t, updated.QuotaKeyID, "existing quota key binding must be preserved")
	require.Equal(t, int64(42), *updated.QuotaKeyID)
}

// TestUpdateAccountCursorExplicitClearRejected 锁定：cursor 账号"显式清空归属"必须 400。
func TestUpdateAccountCursorExplicitClearRejected(t *testing.T) {
	accountID := int64(9102)
	qkID := int64(42)
	repo := newQuotaKeyUpdateRepo(&Account{
		ID:         accountID,
		Name:       "cursor-acc",
		Platform:   domain.PlatformCursor,
		Type:       AccountTypeAPIKey,
		Status:     StatusActive,
		QuotaKeyID: &qkID,
	})
	svc := &adminServiceImpl{accountRepo: repo}

	_, err := svc.UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Name:          "cursor-acc-unbound",
		QuotaKeyIDSet: true,
		QuotaKeyID:    nil,
	})
	require.Error(t, err, "explicitly clearing quota_key_id on a cursor account must be rejected")
	require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
	require.Equal(t, "ACCOUNT_QUOTA_KEY_REQUIRED", infraerrors.Reason(err))

	// 未清空：原归属仍在（拒绝发生在落库前）。
	stored := repo.accounts[accountID]
	require.NotNil(t, stored.QuotaKeyID)
	require.Equal(t, int64(42), *stored.QuotaKeyID)
}

// TestUpdateAccountNonCursorExplicitClearAllowed 锁定：非 cursor 平台允许清空归属。
func TestUpdateAccountNonCursorExplicitClearAllowed(t *testing.T) {
	accountID := int64(9103)
	qkID := int64(42)
	repo := newQuotaKeyUpdateRepo(&Account{
		ID:         accountID,
		Name:       "openai-acc",
		Platform:   PlatformOpenAI,
		Type:       AccountTypeAPIKey,
		Status:     StatusActive,
		QuotaKeyID: &qkID,
	})
	svc := &adminServiceImpl{accountRepo: repo}

	updated, err := svc.UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		QuotaKeyIDSet: true,
		QuotaKeyID:    nil,
	})
	require.NoError(t, err, "non-cursor account must be allowed to clear its quota key binding")
	require.Nil(t, updated.QuotaKeyID)
}

// TestUpdateAccountProvidedQuotaKeyApplied 锁定：显式提供新 key 时写入该归属。
// （存在性校验需 ent 客户端，单测替身下跳过；此处锁定"提供值 → 落库"这一段。）
func TestUpdateAccountProvidedQuotaKeyApplied(t *testing.T) {
	accountID := int64(9104)
	oldQK := int64(42)
	repo := newQuotaKeyUpdateRepo(&Account{
		ID:         accountID,
		Name:       "cursor-acc",
		Platform:   domain.PlatformCursor,
		Type:       AccountTypeAPIKey,
		Status:     StatusActive,
		QuotaKeyID: &oldQK,
	})
	svc := &adminServiceImpl{accountRepo: repo}

	newQK := int64(99)
	updated, err := svc.UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		QuotaKeyIDSet: true,
		QuotaKeyID:    &newQK,
	})
	require.NoError(t, err)
	require.NotNil(t, updated.QuotaKeyID)
	require.Equal(t, int64(99), *updated.QuotaKeyID)
}

// TestResolveUpdateQuotaKeyIDTristate 直接锁定三态解析语义。
func TestResolveUpdateQuotaKeyIDTristate(t *testing.T) {
	existing := int64(7)

	// 未提供 → 沿用现有归属
	got, provided := resolveUpdateQuotaKeyID(&existing, &UpdateAccountInput{})
	require.False(t, provided)
	require.NotNil(t, got)
	require.Equal(t, int64(7), *got)

	// 显式清空 → 清空
	got, provided = resolveUpdateQuotaKeyID(&existing, &UpdateAccountInput{QuotaKeyIDSet: true})
	require.True(t, provided)
	require.Nil(t, got)

	// 提供值 → 该值
	newID := int64(11)
	got, provided = resolveUpdateQuotaKeyID(&existing, &UpdateAccountInput{QuotaKeyIDSet: true, QuotaKeyID: &newID})
	require.True(t, provided)
	require.NotNil(t, got)
	require.Equal(t, int64(11), *got)
}
