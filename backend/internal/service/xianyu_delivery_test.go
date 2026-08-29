package service

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

type xianyuClaimRepoStub struct {
	claim     XianyuDeliveryClaim
	userID    int64
	result    string
	err       error
	callCount int
}

func (r *xianyuClaimRepoStub) Claim(_ context.Context, claim XianyuDeliveryClaim, userID int64) (string, error) {
	r.claim = claim
	r.userID = userID
	r.callCount++
	return r.result, r.err
}

// xianyuControlStub 是控制面仓库桩，用于断言 Claim 的账号/商品解析。
type xianyuControlStub struct {
	workerCfg      *XianyuWorkerConfig
	account        *XianyuAccount
	product        *XianyuProduct
	accounts       []XianyuAccount
	products       []XianyuProduct
	pools          []XianyuItemPool
	rules          []XianyuBindingRule
	accountErr     error
	productErr     error
	createdCfg     *XianyuWorkerConfig
	createdPool    *XianyuItemPool
	createdAccount *XianyuAccount
	createdProduct *XianyuProduct
	bindCalls      int
}

func newXianyuControlStub() *xianyuControlStub {
	poolID := int64(1)
	return &xianyuControlStub{
		workerCfg: &XianyuWorkerConfig{
			ID: 1, BaseURL: "http://xianyu-worker-backend:8089", Status: XianyuWorkerStatusActive,
			HealthStatus: XianyuWorkerHealthHealthy, APITokenEncrypted: "enc",
		},
		account: &XianyuAccount{
			ID: 11, WorkerConfigID: 1, AccountID: "account", Status: XianyuAccountStatusEnabled,
		},
		product: &XianyuProduct{
			ID: 21, AccountPK: 11, AccountID: "account", ItemID: "item",
			BindingStatus: XianyuBindingStatusMapped, BindingSource: XianyuBindingSourceManual,
			PoolID: &poolID, Status: XianyuProductStatusActive,
		},
	}
}

func (s *xianyuControlStub) ListWorkerConfigs(context.Context) ([]XianyuWorkerConfig, error) {
	if s.workerCfg == nil {
		return nil, nil
	}
	return []XianyuWorkerConfig{*s.workerCfg}, nil
}
func (s *xianyuControlStub) CreateWorkerConfig(_ context.Context, cfg XianyuWorkerConfig) (*XianyuWorkerConfig, error) {
	s.createdCfg = &cfg
	return &cfg, nil
}
func (s *xianyuControlStub) UpdateWorkerConfig(_ context.Context, cfg XianyuWorkerConfig) (*XianyuWorkerConfig, error) {
	return &cfg, nil
}
func (s *xianyuControlStub) GetActiveWorkerConfig(context.Context) (*XianyuWorkerConfig, error) {
	if s.workerCfg == nil {
		return nil, ErrXianyuWorkerConfigNotFound
	}
	return s.workerCfg, nil
}
func (s *xianyuControlStub) ListAccounts(_ context.Context, workerConfigID int64) ([]XianyuAccount, error) {
	if s.accounts != nil {
		return s.accounts, nil
	}
	if s.account == nil {
		return nil, nil
	}
	return []XianyuAccount{*s.account}, nil
}
func (s *xianyuControlStub) GetAccountByWorkerAndAccountID(_ context.Context, workerConfigID int64, accountID string) (*XianyuAccount, error) {
	if s.accountErr != nil {
		return nil, s.accountErr
	}
	if s.account == nil || s.account.AccountID != accountID {
		return nil, ErrXianyuAccountNotFound
	}
	return s.account, nil
}
func (s *xianyuControlStub) UpsertAccount(_ context.Context, a XianyuAccount) (*XianyuAccount, error) {
	s.createdAccount = &a
	return &a, nil
}
func (s *xianyuControlStub) UpdateAccount(_ context.Context, a XianyuAccount) (*XianyuAccount, error) {
	s.account = &a
	return &a, nil
}
func (s *xianyuControlStub) ListItemPools(context.Context) ([]XianyuItemPool, error) {
	return s.pools, nil
}
func (s *xianyuControlStub) GetItemPoolByID(_ context.Context, id int64) (*XianyuItemPool, error) {
	for i := range s.pools {
		if s.pools[i].ID == id {
			return &s.pools[i], nil
		}
	}
	return nil, ErrXianyuItemPoolNotFound
}
func (s *xianyuControlStub) GetItemPoolBySlug(_ context.Context, slug string) (*XianyuItemPool, error) {
	for i := range s.pools {
		if s.pools[i].Slug == slug {
			return &s.pools[i], nil
		}
	}
	return nil, ErrXianyuItemPoolNotFound
}
func (s *xianyuControlStub) CreateItemPool(_ context.Context, p XianyuItemPool) (*XianyuItemPool, error) {
	s.createdPool = &p
	return &p, nil
}
func (s *xianyuControlStub) UpdateItemPool(_ context.Context, p XianyuItemPool) (*XianyuItemPool, error) {
	return &p, nil
}
func (s *xianyuControlStub) PoolStockCounts(context.Context, string) (int, int, int, error) {
	return 5, 2, 1, nil
}
func (s *xianyuControlStub) DeliveryStats(context.Context, time.Time) (int, int, error) {
	return 3, 1, nil
}
func (s *xianyuControlStub) PendingDeliveryCount(context.Context) (int, error) {
	return 2, nil
}
func (s *xianyuControlStub) ListProducts(context.Context) ([]XianyuProduct, error) {
	if s.products != nil {
		return s.products, nil
	}
	if s.product == nil {
		return nil, nil
	}
	return []XianyuProduct{*s.product}, nil
}
func (s *xianyuControlStub) ListProductsByAccount(_ context.Context, accountPK int64) ([]XianyuProduct, error) {
	if s.products != nil {
		return s.products, nil
	}
	if s.product == nil {
		return nil, nil
	}
	return []XianyuProduct{*s.product}, nil
}
func (s *xianyuControlStub) GetProductByIdentity(_ context.Context, accountPK int64, itemID, specName, specValue string) (*XianyuProduct, error) {
	if s.productErr != nil {
		return nil, s.productErr
	}
	if s.product == nil || s.product.ItemID != itemID {
		return nil, ErrXianyuProductNotFound
	}
	return s.product, nil
}
func (s *xianyuControlStub) UpsertProduct(_ context.Context, p XianyuProduct) (*XianyuProduct, error) {
	s.createdProduct = &p
	return &p, nil
}
func (s *xianyuControlStub) UpdateProduct(_ context.Context, p XianyuProduct) (*XianyuProduct, error) {
	s.product = &p
	return &p, nil
}
func (s *xianyuControlStub) UpdateProductBinding(_ context.Context, productID int64, bindingStatus, bindingSource string, poolID *int64) error {
	s.bindCalls++
	if s.product != nil {
		s.product.BindingStatus = bindingStatus
		s.product.BindingSource = bindingSource
		s.product.PoolID = poolID
	}
	return nil
}
func (s *xianyuControlStub) ListBindingRules(context.Context) ([]XianyuBindingRule, error) {
	return s.rules, nil
}
func (s *xianyuControlStub) CreateBindingRule(_ context.Context, r XianyuBindingRule) (*XianyuBindingRule, error) {
	return &r, nil
}
func (s *xianyuControlStub) UpdateBindingRule(_ context.Context, r XianyuBindingRule) (*XianyuBindingRule, error) {
	return &r, nil
}

type xianyuStateStub struct {
	result    *XianyuDeliveryStatusResult
	claim     *XianyuOrderClaim
	resend    string
	recordErr error
	resendErr error
}

func (s *xianyuStateStub) RecordDeliveryResult(_ context.Context, r XianyuDeliveryStatusResult) error {
	if s.recordErr != nil {
		return s.recordErr
	}
	s.result = &r
	return nil
}
func (s *xianyuStateStub) GetDeliveryClaim(_ context.Context, orderNo string) (*XianyuOrderClaim, error) {
	if s.claim == nil && s.resend != "" {
		return &XianyuOrderClaim{
			OrderNo: orderNo, Code: s.resend, AccountID: "account", ItemID: "item",
			BuyerID: "buyer", DeliveryStatus: XianyuDeliveryStatusFailed,
		}, nil
	}
	return s.claim, nil
}
func (s *xianyuStateStub) ResendOriginalCode(_ context.Context, orderNo string, userID int64) (string, error) {
	if s.resendErr != nil {
		return "", s.resendErr
	}
	s.claim = &XianyuOrderClaim{OrderNo: orderNo, Code: s.resend, DeliveryStatus: XianyuDeliveryStatusPending}
	return s.resend, nil
}

type xianyuSettingsStub struct {
	enabled bool
}

func newXianyuSettingsStub(enabled bool) *xianyuSettingsStub {
	return &xianyuSettingsStub{enabled: enabled}
}

func (s *xianyuSettingsStub) GetXianyuDeliveryRuntime(context.Context) XianyuDeliveryRuntime {
	return XianyuDeliveryRuntime{Enabled: s.enabled}
}

func newXianyuDeliveryTestService(control XianyuControlRepository, repo XianyuDeliveryRepository, state XianyuDeliveryStateUpdater) *XianyuDeliveryService {
	svc := NewXianyuDeliveryService(repo, control, state, &config.Config{XianyuDelivery: config.XianyuDeliveryConfig{
		InternalToken: "secret",
		SystemUserID:  123,
	}}, newXianyuSettingsStub(true), nil)
	svc.resender = func(context.Context, *XianyuOrderClaim) error { return nil }
	return svc
}

func validXianyuRequest() XianyuDeliveryClaimRequest {
	return XianyuDeliveryClaimRequest{
		OrderID: "order-1", ItemID: "item", OrderAmount: "19.90", OrderQuantity: "1",
		CookieID: "account", BuyerID: "buyer", SpecName: "spec", SpecValue: "value",
	}
}

func TestXianyuDeliveryClaimValidatesAndDelegates(t *testing.T) {
	control := newXianyuControlStub()
	repo := &xianyuClaimRepoStub{result: "ABCD-1234"}
	svc := newXianyuDeliveryTestService(control, repo, nil)

	got, err := svc.Claim(context.Background(), validXianyuRequest())

	require.NoError(t, err)
	require.Equal(t, "ABCD-1234", got)
	require.Equal(t, int64(123), repo.userID)
	require.Equal(t, int64(11), repo.claim.AccountPK)
	require.Equal(t, int64(21), repo.claim.ProductID)
	require.Equal(t, int64(1), repo.claim.PoolID)
	require.Equal(t, XianyuBindingSourceManual, repo.claim.BindingSource)
	require.Equal(t, "19.90", *repo.claim.Amount)
}

func TestXianyuDeliveryRejectsUnsupportedAndUnmappedRequests(t *testing.T) {
	svc := newXianyuDeliveryTestService(newXianyuControlStub(), &xianyuClaimRepoStub{result: "unused"}, nil)

	request := validXianyuRequest()
	request.OrderQuantity = "2"
	_, err := svc.Claim(context.Background(), request)
	require.ErrorIs(t, err, ErrXianyuQuantityUnsupported)

	control := newXianyuControlStub()
	control.product = nil
	svc = newXianyuDeliveryTestService(control, &xianyuClaimRepoStub{result: "unused"}, nil)
	request = validXianyuRequest()
	request.ItemID = "other"
	_, err = svc.Claim(context.Background(), request)
	require.ErrorIs(t, err, ErrXianyuPoolNotMapped)
}

func TestXianyuDeliveryRejectsInvalidAmountAndMissingConfiguration(t *testing.T) {
	svc := newXianyuDeliveryTestService(newXianyuControlStub(), &xianyuClaimRepoStub{result: "unused"}, nil)
	request := validXianyuRequest()
	request.OrderAmount = "not-a-number"
	_, err := svc.Claim(context.Background(), request)
	require.ErrorIs(t, err, ErrXianyuInvalidAmount)

	request = validXianyuRequest()
	request.OrderAmount = "1000000000000000000.00"
	_, err = svc.Claim(context.Background(), request)
	require.ErrorIs(t, err, ErrXianyuInvalidAmount)

	missing := NewXianyuDeliveryService(&xianyuClaimRepoStub{}, &xianyuControlStub{}, nil, &config.Config{}, newXianyuSettingsStub(true), nil)
	_, err = missing.Claim(context.Background(), validXianyuRequest())
	require.ErrorIs(t, err, ErrXianyuDeliveryNotConfigured)

	repoErr := errors.New("db unavailable")
	repo := &xianyuClaimRepoStub{err: repoErr}
	_, err = newXianyuDeliveryTestService(newXianyuControlStub(), repo, nil).Claim(context.Background(), validXianyuRequest())
	require.ErrorIs(t, err, repoErr)
}

func TestXianyuDeliveryRejectsOverlongIdentifiers(t *testing.T) {
	svc := newXianyuDeliveryTestService(newXianyuControlStub(), &xianyuClaimRepoStub{result: "code"}, nil)
	request := validXianyuRequest()
	request.OrderID = strings.Repeat("o", 65)
	_, err := svc.Claim(context.Background(), request)
	require.ErrorIs(t, err, ErrXianyuOrderTooLong)
	request.OrderID = "order-1"
	request.ItemID = strings.Repeat("i", 65)
	_, err = svc.Claim(context.Background(), request)
	require.ErrorIs(t, err, ErrXianyuItemTooLong)
	request.ItemID = "item"
	request.CookieID = strings.Repeat("a", 81)
	_, err = svc.Claim(context.Background(), request)
	require.ErrorIs(t, err, ErrXianyuAccountTooLong)
	request.CookieID = "account"
	request.BuyerID = strings.Repeat("b", 81)
	_, err = svc.Claim(context.Background(), request)
	require.ErrorIs(t, err, ErrXianyuBuyerTooLong)
}

func TestXianyuDeliveryRejectsDisabledAccountAndUnmappedProduct(t *testing.T) {
	control := newXianyuControlStub()
	control.account.Status = XianyuAccountStatusDisabled
	svc := newXianyuDeliveryTestService(control, &xianyuClaimRepoStub{result: "code"}, nil)
	_, err := svc.Claim(context.Background(), validXianyuRequest())
	require.ErrorIs(t, err, ErrXianyuAccountDisabled)

	control = newXianyuControlStub()
	poolID := int64(9)
	control.product = &XianyuProduct{
		ID: 21, AccountPK: 11, ItemID: "item", BindingStatus: XianyuBindingStatusUnmapped,
		BindingSource: XianyuBindingSourceAutoNew, PoolID: &poolID, Status: XianyuProductStatusActive,
	}
	svc = newXianyuDeliveryTestService(control, &xianyuClaimRepoStub{result: "code"}, nil)
	_, err = svc.Claim(context.Background(), validXianyuRequest())
	require.ErrorIs(t, err, ErrXianyuProductUnmapped)
}

func TestXianyuDeliveryValidateStartupRejectsUnavailableSystemUser(t *testing.T) {
	cfg := &config.Config{XianyuDelivery: config.XianyuDeliveryConfig{
		InternalToken: "secret", SystemUserID: 123,
	}}
	svc := NewXianyuDeliveryService(&xianyuClaimRepoStub{}, &xianyuControlStub{}, nil, cfg, newXianyuSettingsStub(true), nil)
	now := time.Now()
	reader := &systemUserReaderStub{users: map[int64]*User{
		123: {ID: 123, Status: StatusActive},
		456: {ID: 456, Status: StatusDisabled},
		789: {ID: 789, Status: StatusActive, DeletedAt: &now},
	}}
	require.NoError(t, svc.ValidateStartup(context.Background(), reader))

	missing := &systemUserReaderStub{err: sql.ErrNoRows}
	err := svc.ValidateStartup(context.Background(), missing)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")

	err = svc.ValidateStartup(context.Background(), &systemUserReaderStub{users: map[int64]*User{123: reader.users[456]}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unavailable")
}

func TestXianyuDeliveryUsesPanelToggleFailClosed(t *testing.T) {
	repo := &xianyuClaimRepoStub{result: "code"}
	cfg := &config.Config{XianyuDelivery: config.XianyuDeliveryConfig{
		InternalToken: "secret", SystemUserID: 123,
	}}
	disabled := NewXianyuDeliveryService(repo, newXianyuControlStub(), nil, cfg, newXianyuSettingsStub(false), nil)
	_, err := disabled.Claim(context.Background(), validXianyuRequest())
	require.ErrorIs(t, err, ErrXianyuDeliveryNotConfigured)

	now := time.Now()
	reader := &systemUserReaderStub{users: map[int64]*User{123: {ID: 123, Status: StatusActive}}}
	require.NoError(t, disabled.ValidateStartup(context.Background(), reader))

	deletedUser := &systemUserReaderStub{users: map[int64]*User{123: {ID: 123, Status: StatusActive, DeletedAt: &now}}}
	enabled := NewXianyuDeliveryService(repo, newXianyuControlStub(), nil, cfg, newXianyuSettingsStub(true), nil)
	err = enabled.ValidateStartup(context.Background(), deletedUser)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unavailable")
}

func TestXianyuDeliveryRecordsResultAndResendsOriginalCode(t *testing.T) {
	state := &xianyuStateStub{resend: "ORIGINAL-CODE"}
	svc := newXianyuDeliveryTestService(newXianyuControlStub(), &xianyuClaimRepoStub{}, state)

	err := svc.RecordDeliveryResult(context.Background(), XianyuDeliveryStatusResult{OrderNo: "o1", Success: true, Confirmed: true})
	require.NoError(t, err)
	require.NotNil(t, state.result)
	require.True(t, state.result.Success)
	require.True(t, state.result.Confirmed)

	code, err := svc.ResendOriginalCode(context.Background(), "o1", 123)
	require.NoError(t, err)
	require.Equal(t, "ORIGINAL-CODE", code)
}

type xianyuResendWorkerStub struct {
	claims []*XianyuOrderClaim
	err    error
}

func (s *xianyuResendWorkerStub) ResendDelivery(_ context.Context, claim *XianyuOrderClaim) error {
	if s.err != nil {
		return s.err
	}
	s.claims = append(s.claims, claim)
	return nil
}

func TestXianyuDeliveryResendCallsWorkerBeforeMarkingPending(t *testing.T) {
	state := &xianyuStateStub{resend: "ORIGINAL-CODE"}
	worker := &xianyuResendWorkerStub{}
	svc := newXianyuDeliveryTestService(newXianyuControlStub(), &xianyuClaimRepoStub{}, state)
	svc.resender = worker.ResendDelivery

	code, err := svc.ResendOriginalCode(context.Background(), "order-1", 123)
	require.NoError(t, err)
	require.Equal(t, "ORIGINAL-CODE", code)
	require.Len(t, worker.claims, 1)
	require.Equal(t, "ORIGINAL-CODE", worker.claims[0].Code)
	require.Equal(t, "account", worker.claims[0].AccountID)

	worker.err = errors.New("worker rejected resend")
	_, err = svc.ResendOriginalCode(context.Background(), "order-1", 123)
	require.Error(t, err)
}

func TestRedeemRejectsXianyuDeliveryCode(t *testing.T) {
	redeemRepo := &redeemRejectRepo{
		code: RedeemCode{ID: 1, Code: "XY-001", Type: RedeemTypeXianyuDelivery, Status: StatusUnused},
	}
	redeemService := NewRedeemService(redeemRepo, nil, nil, nil, nil, nil, nil, nil)
	_, err := redeemService.Redeem(context.Background(), 2, redeemRepo.code.Code)
	require.Error(t, err)
	require.Equal(t, "REDEEM_CODE_UNSUPPORTED_TYPE", infraerrors.Reason(err))
}

func TestXianyuDeliveryCodeCreationRequiresZeroValue(t *testing.T) {
	redeemService := NewRedeemService(&redeemRejectRepo{}, nil, nil, nil, nil, nil, nil, nil)

	_, err := redeemService.GenerateCodes(context.Background(), GenerateCodesRequest{
		Count: 1, Type: RedeemTypeXianyuDelivery, Value: 1,
	})
	require.EqualError(t, err, "value must be zero for xianyu_delivery codes")

	err = redeemService.CreateCode(context.Background(), &RedeemCode{
		Code: "XY-NONZERO", Type: RedeemTypeXianyuDelivery, Value: 1, Status: StatusUnused,
	})
	require.EqualError(t, err, "value must be zero for xianyu_delivery codes")
}

func TestGenerateXianyuDeliveryCodesMarksPool(t *testing.T) {
	repo := &xianyuGenerateRepo{}
	redeemService := NewRedeemService(repo, nil, nil, nil, nil, nil, nil, nil)

	codes, err := redeemService.GenerateCodes(context.Background(), GenerateCodesRequest{
		Count: 2, Type: RedeemTypeXianyuDelivery, Value: 0, Pool: "standard",
	})
	require.NoError(t, err)
	require.Len(t, repo.created, 2)
	require.Len(t, codes, 2)
	for _, code := range repo.created {
		require.Equal(t, XianyuPoolNote("standard"), code.Notes)
	}
}

func TestGenerateAndCreateXianyuDeliveryRejectsMissingPool(t *testing.T) {
	repo := &xianyuGenerateRepo{}
	redeemService := NewRedeemService(repo, nil, nil, nil, nil, nil, nil, nil)

	_, err := redeemService.GenerateCodes(context.Background(), GenerateCodesRequest{
		Count: 1, Type: RedeemTypeXianyuDelivery, Value: 0,
	})
	require.EqualError(t, err, "pool is required for xianyu_delivery codes")

	err = redeemService.CreateCode(context.Background(), &RedeemCode{
		Code: "XY-NO-POOL", Type: RedeemTypeXianyuDelivery, Value: 0, Status: StatusUnused,
	})
	require.EqualError(t, err, "pool note is required for xianyu_delivery codes")

	err = redeemService.CreateCode(context.Background(), &RedeemCode{
		Code: "XY-BAD-NOTE", Type: RedeemTypeXianyuDelivery, Value: 0, Status: StatusUnused, Notes: "other",
	})
	require.EqualError(t, err, "xianyu_delivery codes must use the xianyu_pool note")

	err = redeemService.CreateCode(context.Background(), &RedeemCode{
		Code: "XY-OK", Type: RedeemTypeXianyuDelivery, Value: 0, Status: StatusUnused, Notes: XianyuPoolNote("standard"),
	})
	require.NoError(t, err)
	require.Len(t, repo.created, 1)
	require.Equal(t, XianyuPoolNote("standard"), repo.created[0].Notes)
}

type xianyuGenerateRepo struct {
	created []*RedeemCode
}

func (r *xianyuGenerateRepo) GetByID(ctx context.Context, id int64) (*RedeemCode, error) {
	panic("unexpected GetByID call")
}

func (r *xianyuGenerateRepo) GetByCode(ctx context.Context, code string) (*RedeemCode, error) {
	panic("unexpected GetByCode call")
}

func (r *xianyuGenerateRepo) Update(ctx context.Context, code *RedeemCode) error {
	panic("unexpected Update call")
}

func (r *xianyuGenerateRepo) BatchUpdate(ctx context.Context, ids []int64, fields RedeemCodeBatchUpdateFields) (int64, error) {
	panic("unexpected BatchUpdate call")
}

func (r *xianyuGenerateRepo) Delete(ctx context.Context, id int64) error {
	panic("unexpected Delete call")
}

func (r *xianyuGenerateRepo) Use(ctx context.Context, id, userID int64) error {
	panic("unexpected Use call")
}

func (r *xianyuGenerateRepo) List(ctx context.Context, params pagination.PaginationParams) ([]RedeemCode, *pagination.PaginationResult, error) {
	panic("unexpected List call")
}

func (r *xianyuGenerateRepo) ListWithFilters(ctx context.Context, params pagination.PaginationParams, codeType, status, search string) ([]RedeemCode, *pagination.PaginationResult, error) {
	panic("unexpected ListWithFilters call")
}

func (r *xianyuGenerateRepo) ListByUser(ctx context.Context, userID int64, limit int) ([]RedeemCode, error) {
	panic("unexpected ListByUser call")
}

func (r *xianyuGenerateRepo) ListByUserPaginated(ctx context.Context, userID int64, params pagination.PaginationParams, codeType string) ([]RedeemCode, *pagination.PaginationResult, error) {
	panic("unexpected ListByUserPaginated call")
}

func (r *xianyuGenerateRepo) SumPositiveBalanceByUser(ctx context.Context, userID int64) (float64, error) {
	panic("unexpected SumPositiveBalanceByUser call")
}

func (r *xianyuGenerateRepo) Create(ctx context.Context, code *RedeemCode) error {
	r.created = append(r.created, code)
	return nil
}

func (r *xianyuGenerateRepo) CreateBatch(ctx context.Context, codes []RedeemCode) error {
	for i := range codes {
		r.created = append(r.created, &codes[i])
	}
	return nil
}

type systemUserReaderStub struct {
	users map[int64]*User
	err   error
}

func (r *systemUserReaderStub) GetByIDIncludeDeleted(_ context.Context, id int64) (*User, error) {
	if r.err != nil {
		return nil, r.err
	}
	user := r.users[id]
	if user == nil {
		return nil, sql.ErrNoRows
	}
	return user, nil
}
