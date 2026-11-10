//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"entgo.io/ent/dialect"
	entql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

// newLargeRequestOnlyEntClient 构造 sqlmock 驱动的 ent client，供事务/SQL 路径单测
// （口径与 cmd/quota-state-retire/main_test.go 的注入式 sqlmock 一致：语句形态 + 序列，
// 真实 Postgres 行锁/jsonb 语义仍需集成验证）。
func newLargeRequestOnlyEntClient(t *testing.T) (*dbent.Client, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	drv := entql.OpenDB(dialect.Postgres, db)
	return dbent.NewClient(dbent.Driver(drv)), mock
}

func TestUpdateAccountExtraDropsLargeRequestOnlyKey(t *testing.T) {
	accountID := int64(203)
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: {ID: accountID, Platform: PlatformOpenAI},
	}}

	err := (&adminServiceImpl{accountRepo: repo}).UpdateAccountExtra(context.Background(), accountID, map[string]any{
		"custom":                 "value",
		LargeRequestOnlyExtraKey: true,
	})

	require.NoError(t, err)
	require.Equal(t, "value", repo.accounts[accountID].Extra["custom"])
	require.NotContains(t, repo.accounts[accountID].Extra, LargeRequestOnlyExtraKey)
}

func TestUpdateAccountExtraLoneLargeRequestOnlyKeyNoWrite(t *testing.T) {
	accountID := int64(204)
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: {ID: accountID, Platform: PlatformOpenAI},
	}}

	err := (&adminServiceImpl{accountRepo: repo}).UpdateAccountExtra(context.Background(), accountID, map[string]any{
		LargeRequestOnlyExtraKey: false,
	})

	require.NoError(t, err)
	require.Nil(t, repo.updates[accountID], "仅有受管键时 update 应被清空、不落库")
	require.NotContains(t, repo.accounts[accountID].Extra, LargeRequestOnlyExtraKey)
}

// TestBuildAccountForCreateDropsLargeRequestOnlyKey 覆盖建号路径：CreateAccountInput.Extra
// 携带 large_request_only 时必须在入库存盘前被丢弃。开关只能建号后经
// SetAccountLargeRequestOnly 设置（该入口有池覆盖校验），建号链不得绕过。
func TestBuildAccountForCreateDropsLargeRequestOnlyKey(t *testing.T) {
	account, err := buildAccountForCreate(&CreateAccountInput{
		Name:        "lro-create",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "https://api.openai.com", "api_key": "sk-test"},
	}, map[string]any{
		"custom":                 "value",
		LargeRequestOnlyExtraKey: true,
	})

	require.NoError(t, err)
	require.NotNil(t, account)
	require.Equal(t, "value", account.Extra["custom"], "非受管键必须原样保留")
	require.NotContains(t, account.Extra, LargeRequestOnlyExtraKey, "建号不得携带仅接大请求开关")
}

func TestBulkUpdateAccountsDropsLargeRequestOnlyKey(t *testing.T) {
	repo := &upstreamBillingProbeAccountRepo{}
	svc := &adminServiceImpl{accountRepo: repo}
	input := &BulkUpdateAccountsInput{
		AccountIDs: []int64{1},
		Extra: map[string]any{
			"custom":                 "value",
			LargeRequestOnlyExtraKey: true,
		},
	}

	result, err := svc.BulkUpdateAccounts(context.Background(), input)

	require.NoError(t, err)
	require.Equal(t, 1, result.Success)
	require.Len(t, repo.bulkUpdates, 1)
	require.Equal(t, "value", repo.bulkUpdates[0].Extra["custom"])
	require.NotContains(t, repo.bulkUpdates[0].Extra, LargeRequestOnlyExtraKey)
}

func TestUpdateAccountDropsLargeRequestOnlyKey(t *testing.T) {
	accountID := int64(205)
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: {ID: accountID, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive},
	}}

	_, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Extra: map[string]any{
			"custom":                 "value",
			LargeRequestOnlyExtraKey: true,
		},
	})

	require.NoError(t, err)
	require.Equal(t, "value", repo.accounts[accountID].Extra["custom"])
	require.NotContains(t, repo.accounts[accountID].Extra, LargeRequestOnlyExtraKey)
}

// TestUpdateAccountPreservesLargeRequestOnly 覆盖终审 F2：UpdateAccount 的全量 extra 编辑是
// 整体替换语义，必须在拒绝请求值后把库中原值带回，否则已启用账号的开关会被静默清除。
func TestUpdateAccountPreservesLargeRequestOnly(t *testing.T) {
	newRepo := func(accountID int64) *upstreamBillingProbeAccountRepo {
		return &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
			accountID: {
				ID:       accountID,
				Platform: PlatformOpenAI,
				Type:     AccountTypeAPIKey,
				Status:   StatusActive,
				Extra:    map[string]any{LargeRequestOnlyExtraKey: true},
			},
		}}
	}

	t.Run("MissingKeyKeepsStoredTrue", func(t *testing.T) {
		accountID := int64(207)
		repo := newRepo(accountID)

		_, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
			Extra: map[string]any{"custom": "value"},
		})

		require.NoError(t, err)
		require.Equal(t, "value", repo.accounts[accountID].Extra["custom"])
		require.Equal(t, true, repo.accounts[accountID].Extra[LargeRequestOnlyExtraKey], "未提及该键时不得清除已启用开关")
	})

	t.Run("RequestedFalseRejected", func(t *testing.T) {
		accountID := int64(208)
		repo := newRepo(accountID)

		_, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
			Extra: map[string]any{
				"custom":                 "value",
				LargeRequestOnlyExtraKey: false,
			},
		})

		require.NoError(t, err)
		require.Equal(t, "value", repo.accounts[accountID].Extra["custom"])
		require.Equal(t, true, repo.accounts[accountID].Extra[LargeRequestOnlyExtraKey], "请求值 false 必须被拒绝，保留库中 true")
	})
}

func TestUpdateAccountNilExtraBranchUnaffected(t *testing.T) {
	// input.Extra == nil 分支不构造 normalizedExtra，delete 不执行，
	// 该路径应正常完成且不产生 extra 写入。
	accountID := int64(206)
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: {ID: accountID, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive},
	}}

	_, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Name: "renamed",
	})

	require.NoError(t, err)
	require.Equal(t, "renamed", repo.accounts[accountID].Name)
	require.NotContains(t, repo.accounts[accountID].Extra, LargeRequestOnlyExtraKey)
}

func TestSetAccountLargeRequestOnlyNotFound(t *testing.T) {
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{}}
	_, err := (&adminServiceImpl{accountRepo: repo}).SetAccountLargeRequestOnly(context.Background(), 999, true)
	require.ErrorIs(t, err, ErrAccountNotFound)
}

func TestSetAccountLargeRequestOnlyNilAccountReturnsNotFound(t *testing.T) {
	repo := &nilAccountRepo{}
	_, err := (&adminServiceImpl{accountRepo: repo}).SetAccountLargeRequestOnly(context.Background(), 1, true)
	require.ErrorIs(t, err, infraerrors.NotFound("ACCOUNT_NOT_FOUND", "account not found"))
}

func TestSetAccountLargeRequestOnlyEnableNilEntClientFailsClosed(t *testing.T) {
	accountID := int64(301)
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: {ID: accountID, Platform: PlatformOpenAI, GroupIDs: []int64{1}},
	}}

	svc := &adminServiceImpl{accountRepo: repo} // entClient == nil
	_, err := svc.SetAccountLargeRequestOnly(context.Background(), accountID, true)

	require.Error(t, err, "entClient 缺失必须失败关闭，不得静默跳过校验")
	require.NotContains(t, repo.accounts[accountID].Extra, LargeRequestOnlyExtraKey)
}

func TestSetAccountLargeRequestOnlyDisableWritesFalse(t *testing.T) {
	accountID := int64(302)
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: {ID: accountID, Platform: PlatformOpenAI, Extra: map[string]any{LargeRequestOnlyExtraKey: true}},
	}}

	result, err := (&adminServiceImpl{accountRepo: repo}).SetAccountLargeRequestOnly(context.Background(), accountID, false)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, false, repo.accounts[accountID].Extra[LargeRequestOnlyExtraKey])
	require.Equal(t, false, result.Extra[LargeRequestOnlyExtraKey])
	require.Len(t, repo.updates[accountID], 1, "enabled=false 应直接走 UpdateExtra 写入 false")
	require.Equal(t, false, repo.updates[accountID][0][LargeRequestOnlyExtraKey])
}

func TestSetAccountLargeRequestOnlyEnablePoolExhausted(t *testing.T) {
	accountID := int64(303)
	groupID := int64(7)
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: {ID: accountID, Platform: PlatformOpenAI, GroupIDs: []int64{groupID}},
	}}
	entClient, mock := newLargeRequestOnlyEntClient(t)
	svc := &adminServiceImpl{accountRepo: repo, entClient: entClient}

	mock.ExpectBegin()
	mock.ExpectQuery(setAccountLargeRequestOnlyLockSQL).
		WithArgs(groupID, string(PlatformOpenAI)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(accountID)))
	mock.ExpectQuery(setAccountLargeRequestOnlyCountSQL).
		WithArgs(groupID, string(PlatformOpenAI), accountID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))
	mock.ExpectRollback()

	_, err := svc.SetAccountLargeRequestOnly(context.Background(), accountID, true)

	require.Error(t, err)
	var appErr *infraerrors.ApplicationError
	require.True(t, errors.As(err, &appErr), "应返回 ApplicationError，实际：%T %v", err, err)
	require.Equal(t, int32(http.StatusBadRequest), appErr.Code)
	require.Equal(t, "LARGE_REQUEST_ONLY_POOL_EXHAUSTED", appErr.Reason)
	require.NotContains(t, repo.accounts[accountID].Extra, LargeRequestOnlyExtraKey, "池耗尽回滚后不得写入开关")
	require.Nil(t, repo.updates[accountID], "池耗尽回滚后不得调用 UpdateExtra")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSetAccountLargeRequestOnlyEnableSuccess(t *testing.T) {
	accountID := int64(304)
	groupID := int64(8)
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: {ID: accountID, Platform: PlatformOpenAI, GroupIDs: []int64{groupID}},
	}}
	entClient, mock := newLargeRequestOnlyEntClient(t)
	svc := &adminServiceImpl{accountRepo: repo, entClient: entClient}

	mock.ExpectBegin()
	mock.ExpectQuery(setAccountLargeRequestOnlyLockSQL).
		WithArgs(groupID, string(PlatformOpenAI)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(accountID)))
	mock.ExpectQuery(setAccountLargeRequestOnlyCountSQL).
		WithArgs(groupID, string(PlatformOpenAI), accountID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(1)))
	mock.ExpectCommit()

	result, err := svc.SetAccountLargeRequestOnly(context.Background(), accountID, true)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, true, repo.accounts[accountID].Extra[LargeRequestOnlyExtraKey])
	require.Equal(t, true, result.Extra[LargeRequestOnlyExtraKey])
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSetAccountLargeRequestOnlyEnableNoGroupsSkipsValidation(t *testing.T) {
	accountID := int64(305)
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: {ID: accountID, Platform: PlatformOpenAI},
	}}
	entClient, mock := newLargeRequestOnlyEntClient(t)
	svc := &adminServiceImpl{accountRepo: repo, entClient: entClient}

	// GroupIDs 为空 → 无校验 SQL，仅 BEGIN/COMMIT 空事务。
	mock.ExpectBegin()
	mock.ExpectCommit()

	result, err := svc.SetAccountLargeRequestOnly(context.Background(), accountID, true)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, true, repo.accounts[accountID].Extra[LargeRequestOnlyExtraKey])
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestDuplicateAccountExtraDropsLargeRequestOnly 覆盖复制账号路径：源账号已启用
// 仅接大请求时，副本不得继承该开关，否则复制即绕过池覆盖校验。非受管键仍需原样保留。
func TestDuplicateAccountExtraDropsLargeRequestOnly(t *testing.T) {
	cloned, err := duplicateAccountExtra(map[string]any{
		"custom":                 "value",
		LargeRequestOnlyExtraKey: true,
	})

	require.NoError(t, err)
	require.Equal(t, "value", cloned["custom"], "非受管键必须原样保留")
	require.NotContains(t, cloned, LargeRequestOnlyExtraKey, "复制账号不得继承仅接大请求开关")

	// 源 map 不应被修改：duplicateAccountExtra 走深拷贝后删除。
	source := map[string]any{LargeRequestOnlyExtraKey: true}
	original := source[LargeRequestOnlyExtraKey]
	_, err = duplicateAccountExtra(source)
	require.NoError(t, err)
	require.Equal(t, original, source[LargeRequestOnlyExtraKey], "复制不得改动源 extra")
}

// nilAccountRepo 用于覆盖 account == nil 分支（stub GetByID 返回 (nil, nil)）。
type nilAccountRepo struct {
	AccountRepository
}

func (r *nilAccountRepo) GetByID(_ context.Context, _ int64) (*Account, error) {
	return nil, nil
}
