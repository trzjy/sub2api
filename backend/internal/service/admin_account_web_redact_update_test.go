package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// 回归（2026-09-21）：web 账号编辑走「全对象 PUT + 脱敏响应」模式，前端上送的
// input.Credentials 已被 RedactCredentials 剥离敏感键（kimi 无 access_token）。
// 更新路径若对 input.Credentials 预合并校验 validateWebAccountCredential，必然误伤
// （报 platform kimi requires a non-empty access_token）。正确语义：校验只能发生在
// MergePreservingSensitiveCreds 合并后（validateAccessModeCredential），敏感键缺省保留。
type webRedactUpdateAdminRepo struct {
	AccountRepository
	mu       chan struct{}
	accounts map[int64]*Account
}

func newWebRedactUpdateAdminRepo(accounts ...*Account) *webRedactUpdateAdminRepo {
	m := make(map[int64]*Account, len(accounts))
	for _, a := range accounts {
		m[a.ID] = a
	}
	return &webRedactUpdateAdminRepo{mu: make(chan struct{}, 1), accounts: m}
}

func (r *webRedactUpdateAdminRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	a, ok := r.accounts[id]
	if !ok {
		return nil, ErrAccountNotFound
	}
	cp := *a
	return &cp, nil
}

func (r *webRedactUpdateAdminRepo) Update(_ context.Context, account *Account) error {
	r.accounts[account.ID] = account
	return nil
}

func (r *webRedactUpdateAdminRepo) ListShadowsByParent(context.Context, int64) ([]*Account, error) {
	return nil, nil
}

func TestUpdateAccountKimiWebRedactedCredentialsKeepAccessToken(t *testing.T) {
	accountID := int64(9001)
	repo := newWebRedactUpdateAdminRepo(&Account{
		ID:       accountID,
		Name:     "kimi-web",
		Platform: PlatformKimi,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Credentials: map[string]any{
			"access_mode":         AccountAccessModeWeb,
			"access_token":        "at-secret",
			"refresh_token":       "rt-secret",
			"login_refresh_token": "lrt-secret",
			"login_phone":         "13800000000",
		},
	})
	svc := &adminServiceImpl{accountRepo: repo}

	// 模拟前端编辑：脱敏后 credentials（无 access_token/refresh_token/login_refresh_token）
	// + 强制 access_mode=web + 可选 base_url（EditAccountModal.vue isWebEditAccount 分支）。
	updated, err := svc.UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Name: "kimi-web-renamed",
		Credentials: map[string]any{
			"access_mode": AccountAccessModeWeb,
			"base_url":    "https://www.kimi.com",
			"login_phone": "13800000000",
		},
	})
	require.NoError(t, err)
	require.Equal(t, "kimi-web-renamed", updated.Name)

	// 合并后敏感键全部保留（登录态不被编辑动作清空）。
	require.Equal(t, "at-secret", updated.Credentials["access_token"])
	require.Equal(t, "rt-secret", updated.Credentials["refresh_token"])
	require.Equal(t, "lrt-secret", updated.Credentials["login_refresh_token"])
	require.Equal(t, AccountAccessModeWeb, updated.Credentials["access_mode"])
	require.Equal(t, "https://www.kimi.com", updated.Credentials["base_url"])
}

func TestUpdateAccountKimiWebRedactedStillRejectsTypeChange(t *testing.T) {
	accountID := int64(9002)
	repo := newWebRedactUpdateAdminRepo(&Account{
		ID:       accountID,
		Name:     "kimi-web",
		Platform: PlatformKimi,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Credentials: map[string]any{
			"access_mode":  AccountAccessModeWeb,
			"access_token": "at-secret",
		},
	})
	svc := &adminServiceImpl{accountRepo: repo}

	// web 账号类型守卫仍生效（只允许 apikey）。
	_, err := svc.UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_mode": AccountAccessModeWeb},
	})
	require.Error(t, err)
}

func TestUpdateAccountKimiWebExplicitEmptyAccessTokenStillRejected(t *testing.T) {
	accountID := int64(9003)
	repo := newWebRedactUpdateAdminRepo(&Account{
		ID:       accountID,
		Name:     "kimi-web",
		Platform: PlatformKimi,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Credentials: map[string]any{
			"access_mode":  AccountAccessModeWeb,
			"access_token": "at-secret",
		},
	})
	svc := &adminServiceImpl{accountRepo: repo}

	// 显式上送空 access_token（键存在且为空串）：合并语义是"incoming 显式提供则覆盖"，
	// 覆盖后登录态为空 → 合并后校验必须仍然拒绝（fail-closed 不丢不变量）。
	_, err := svc.UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Credentials: map[string]any{
			"access_mode":  AccountAccessModeWeb,
			"access_token": "",
		},
	})
	require.Error(t, err)
}

func TestUpdateAccountKimiWebWrongBaseURLStillRejected(t *testing.T) {
	accountID := int64(9004)
	repo := newWebRedactUpdateAdminRepo(&Account{
		ID:       accountID,
		Name:     "kimi-web",
		Platform: PlatformKimi,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Credentials: map[string]any{
			"access_mode":  AccountAccessModeWeb,
			"access_token": "at-secret",
		},
	})
	svc := &adminServiceImpl{accountRepo: repo}

	// 脱敏回写 + 非法 base_url：合并后校验仍须拒绝（base_url 非敏感键，incoming 决定）。
	_, err := svc.UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Credentials: map[string]any{
			"access_mode": AccountAccessModeWeb,
			"base_url":    "https://evil.example.com",
		},
	})
	require.Error(t, err)
}
