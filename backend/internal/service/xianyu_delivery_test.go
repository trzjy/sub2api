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

func newXianyuDeliveryTestService(repo XianyuDeliveryRepository) *XianyuDeliveryService {
	return NewXianyuDeliveryService(repo, &config.Config{XianyuDelivery: config.XianyuDeliveryConfig{
		InternalToken: "secret",
		SystemUserID:  123,
		ItemPools:     map[string]string{"account:item": "standard"},
	}}, newXianyuSettingsStub(true))
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

func validXianyuRequest() XianyuDeliveryClaimRequest {
	return XianyuDeliveryClaimRequest{
		OrderID: "order-1", ItemID: "item", OrderAmount: "19.90", OrderQuantity: "1",
		CookieID: "account", BuyerID: "buyer", SpecName: "spec", SpecValue: "value",
	}
}

func TestXianyuDeliveryClaimValidatesAndDelegates(t *testing.T) {
	repo := &xianyuClaimRepoStub{result: "ABCD-1234"}
	svc := newXianyuDeliveryTestService(repo)

	got, err := svc.Claim(context.Background(), validXianyuRequest())

	require.NoError(t, err)
	require.Equal(t, "ABCD-1234", got)
	require.Equal(t, int64(123), repo.userID)
	require.Equal(t, "standard", repo.claim.Pool)
	require.Equal(t, "19.90", *repo.claim.Amount)
}

func TestXianyuDeliveryRejectsUnsupportedAndUnmappedRequests(t *testing.T) {
	svc := newXianyuDeliveryTestService(&xianyuClaimRepoStub{result: "unused"})

	request := validXianyuRequest()
	request.OrderQuantity = "2"
	_, err := svc.Claim(context.Background(), request)
	require.ErrorIs(t, err, ErrXianyuQuantityUnsupported)

	request = validXianyuRequest()
	request.ItemID = "other"
	_, err = svc.Claim(context.Background(), request)
	require.ErrorIs(t, err, ErrXianyuPoolNotMapped)
}

func TestXianyuDeliveryRejectsInvalidAmountAndMissingConfiguration(t *testing.T) {
	svc := newXianyuDeliveryTestService(&xianyuClaimRepoStub{result: "unused"})
	request := validXianyuRequest()
	request.OrderAmount = "not-a-number"
	_, err := svc.Claim(context.Background(), request)
	require.ErrorIs(t, err, ErrXianyuInvalidAmount)

	request = validXianyuRequest()
	request.OrderAmount = "1000000000000000000.00"
	_, err = svc.Claim(context.Background(), request)
	require.ErrorIs(t, err, ErrXianyuInvalidAmount)

	missing := NewXianyuDeliveryService(&xianyuClaimRepoStub{}, &config.Config{}, newXianyuSettingsStub(true))
	_, err = missing.Claim(context.Background(), validXianyuRequest())
	require.ErrorIs(t, err, ErrXianyuDeliveryNotConfigured)

	repoErr := errors.New("db unavailable")
	repo := &xianyuClaimRepoStub{err: repoErr}
	_, err = newXianyuDeliveryTestService(repo).Claim(context.Background(), validXianyuRequest())
	require.ErrorIs(t, err, repoErr)
}

func TestXianyuDeliveryRejectsOverlongIdentifiers(t *testing.T) {
	svc := newXianyuDeliveryTestService(&xianyuClaimRepoStub{result: "code"})
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

func TestXianyuDeliveryValidateStartupRejectsUnavailableSystemUser(t *testing.T) {
	cfg := &config.Config{XianyuDelivery: config.XianyuDeliveryConfig{
		InternalToken: "secret", SystemUserID: 123,
	}}
	svc := NewXianyuDeliveryService(&xianyuClaimRepoStub{}, cfg, newXianyuSettingsStub(true))
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
		InternalToken: "secret", SystemUserID: 123, ItemPools: map[string]string{"account:item": "standard"},
	}}
	disabled := NewXianyuDeliveryService(repo, cfg, newXianyuSettingsStub(false))
	_, err := disabled.Claim(context.Background(), validXianyuRequest())
	require.ErrorIs(t, err, ErrXianyuDeliveryNotConfigured)

	now := time.Now()
	reader := &systemUserReaderStub{users: map[int64]*User{123: {ID: 123, Status: StatusActive}}}
	require.NoError(t, disabled.ValidateStartup(context.Background(), reader))

	deletedUser := &systemUserReaderStub{users: map[int64]*User{123: {ID: 123, Status: StatusActive, DeletedAt: &now}}}
	enabled := NewXianyuDeliveryService(repo, cfg, newXianyuSettingsStub(true))
	err = enabled.ValidateStartup(context.Background(), deletedUser)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unavailable")
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
