package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

// 兑换码删除/作废的拦截与提示测试。原本位于 admin_service_delete_test.go
// （//go:build unit），该标签下其他历史测试文件存在编译漂移导致本组用例长期
// 未被默认 `go test ./...` 执行；移入无标签文件保证随默认构建运行。

type redeemRepoStub struct {
	deleteErrByID map[int64]error
	deletedIDs    []int64
	statusByID    map[int64]string
	getByIDErr    error
	updatedCodes  []RedeemCode
	updateErr     error

	batchUpdateIDs    []int64
	batchUpdateFields RedeemCodeBatchUpdateFields
	batchUpdateResult int64
	batchUpdateErr    error
	batchUpdateCalled bool
}

func (s *redeemRepoStub) Create(ctx context.Context, code *RedeemCode) error {
	panic("unexpected Create call")
}

func (s *redeemRepoStub) CreateBatch(ctx context.Context, codes []RedeemCode) error {
	panic("unexpected CreateBatch call")
}

func (s *redeemRepoStub) GetByID(ctx context.Context, id int64) (*RedeemCode, error) {
	if s.getByIDErr != nil {
		return nil, s.getByIDErr
	}
	status := s.statusByID[id]
	if status == "" {
		status = StatusUnused
	}
	return &RedeemCode{ID: id, Status: status}, nil
}

func (s *redeemRepoStub) GetByCode(ctx context.Context, code string) (*RedeemCode, error) {
	panic("unexpected GetByCode call")
}

func (s *redeemRepoStub) Update(ctx context.Context, code *RedeemCode) error {
	s.updatedCodes = append(s.updatedCodes, *code)
	return s.updateErr
}

func (s *redeemRepoStub) BatchUpdate(ctx context.Context, ids []int64, fields RedeemCodeBatchUpdateFields) (int64, error) {
	s.batchUpdateCalled = true
	s.batchUpdateIDs = append([]int64(nil), ids...)
	s.batchUpdateFields = fields
	if s.batchUpdateErr != nil {
		return 0, s.batchUpdateErr
	}
	if s.batchUpdateResult != 0 {
		return s.batchUpdateResult, nil
	}
	return int64(len(ids)), nil
}

func (s *redeemRepoStub) Delete(ctx context.Context, id int64) error {
	s.deletedIDs = append(s.deletedIDs, id)
	if s.deleteErrByID != nil {
		if err, ok := s.deleteErrByID[id]; ok {
			return err
		}
	}
	return nil
}

func (s *redeemRepoStub) Use(ctx context.Context, id, userID int64) error {
	panic("unexpected Use call")
}

func (s *redeemRepoStub) List(ctx context.Context, params pagination.PaginationParams) ([]RedeemCode, *pagination.PaginationResult, error) {
	panic("unexpected List call")
}

func (s *redeemRepoStub) ListWithFilters(ctx context.Context, params pagination.PaginationParams, codeType, status, search, poolSlug string, value *float64) ([]RedeemCode, *pagination.PaginationResult, error) {
	panic("unexpected ListWithFilters call")
}

func (s *redeemRepoStub) ListDistinctValues(ctx context.Context, codeType string) ([]float64, error) {
	panic("unexpected ListDistinctValues call")
}

func (s *redeemRepoStub) ListByUser(ctx context.Context, userID int64, limit int) ([]RedeemCode, error) {
	panic("unexpected ListByUser call")
}

func (s *redeemRepoStub) ListByUserPaginated(ctx context.Context, userID int64, params pagination.PaginationParams, codeType string) ([]RedeemCode, *pagination.PaginationResult, error) {
	panic("unexpected ListByUserPaginated call")
}

func (s *redeemRepoStub) SumPositiveBalanceByUser(ctx context.Context, userID int64) (float64, error) {
	panic("unexpected SumPositiveBalanceByUser call")
}

func TestAdminService_DeleteRedeemCode_Success(t *testing.T) {
	repo := &redeemRepoStub{}
	svc := &adminServiceImpl{redeemCodeRepo: repo}

	err := svc.DeleteRedeemCode(context.Background(), 10)
	require.NoError(t, err)
	require.Equal(t, []int64{10}, repo.deletedIDs)
}

func TestAdminService_DeleteRedeemCode_Idempotent(t *testing.T) {
	repo := &redeemRepoStub{}
	svc := &adminServiceImpl{redeemCodeRepo: repo}

	err := svc.DeleteRedeemCode(context.Background(), 999)
	require.NoError(t, err)
	require.Equal(t, []int64{999}, repo.deletedIDs)
}

func TestAdminService_DeleteRedeemCode_Error(t *testing.T) {
	deleteErr := errors.New("delete failed")
	repo := &redeemRepoStub{deleteErrByID: map[int64]error{1: deleteErr}}
	svc := &adminServiceImpl{redeemCodeRepo: repo}

	err := svc.DeleteRedeemCode(context.Background(), 1)
	require.ErrorIs(t, err, deleteErr)
	require.Equal(t, []int64{1}, repo.deletedIDs)
}

func TestAdminService_BatchDeleteRedeemCodes_Success(t *testing.T) {
	repo := &redeemRepoStub{}
	svc := &adminServiceImpl{redeemCodeRepo: repo}

	deleted, err := svc.BatchDeleteRedeemCodes(context.Background(), []int64{1, 2, 3})
	require.NoError(t, err)
	require.Equal(t, int64(3), deleted)
	require.Equal(t, []int64{1, 2, 3}, repo.deletedIDs)
}

func TestAdminService_BatchDeleteRedeemCodes_PartialFailures(t *testing.T) {
	// 哨兵必须在 stub 与断言间复用同一个实例：errors.Is 按身份比较，每次
	// errors.New 都是新实例必然失配。
	dbErr := errors.New("db error")
	repo := &redeemRepoStub{
		deleteErrByID: map[int64]error{
			2: dbErr,
		},
	}
	svc := &adminServiceImpl{redeemCodeRepo: repo}

	deleted, err := svc.BatchDeleteRedeemCodes(context.Background(), []int64{1, 2, 3})
	require.Error(t, err)
	require.ErrorIs(t, err, dbErr)
	require.Equal(t, int64(1), deleted)
	require.Equal(t, []int64{1, 2}, repo.deletedIDs)
}

func TestAdminService_DeleteRedeemCode_UsedBlocked(t *testing.T) {
	repo := &redeemRepoStub{statusByID: map[int64]string{7: StatusUsed}}
	svc := &adminServiceImpl{redeemCodeRepo: repo}

	err := svc.DeleteRedeemCode(context.Background(), 7)
	require.Error(t, err)
	require.Contains(t, err.Error(), "已被使用")
	require.Empty(t, repo.deletedIDs, "已使用的兑换码不应进入删除")
}

func TestAdminService_DeleteRedeemCode_DeliveredBlocked(t *testing.T) {
	repo := &redeemRepoStub{statusByID: map[int64]string{8: StatusDelivered}}
	svc := &adminServiceImpl{redeemCodeRepo: repo}

	err := svc.DeleteRedeemCode(context.Background(), 8)
	require.Error(t, err)
	require.Contains(t, err.Error(), "已随订单发货")
	require.Contains(t, err.Error(), "作废")
	require.Empty(t, repo.deletedIDs, "已发货的兑换码不应进入删除")
}

func TestAdminService_DeleteRedeemCode_OrderClaimFKTranslated(t *testing.T) {
	// 已作废的兑换码仍被 xianyu_order_claims 引用：ent 只报 constraint failed，
	// 必须转换为可读的拦截原因而不是原始 500。
	fkErr := errors.New(`ent: constraint failed: pq: update or delete on table "redeem_codes" violates foreign key constraint "fk_xianyu_order_claims_redeem_code" on table "xianyu_order_claims"`)
	repo := &redeemRepoStub{
		statusByID:    map[int64]string{9: StatusExpired},
		deleteErrByID: map[int64]error{9: fkErr},
	}
	svc := &adminServiceImpl{redeemCodeRepo: repo}

	err := svc.DeleteRedeemCode(context.Background(), 9)
	require.Error(t, err)
	require.Contains(t, err.Error(), "已随订单发货")
	require.Contains(t, err.Error(), "作废")
}

func TestAdminService_BatchDeleteRedeemCodes_StopsAtBlockedCode(t *testing.T) {
	repo := &redeemRepoStub{statusByID: map[int64]string{2: StatusDelivered}}
	svc := &adminServiceImpl{redeemCodeRepo: repo}

	deleted, err := svc.BatchDeleteRedeemCodes(context.Background(), []int64{1, 2, 3})
	require.Error(t, err)
	require.Contains(t, err.Error(), "已随订单发货")
	require.Equal(t, int64(1), deleted, "拦截前已删除的计数应保留")
	require.Equal(t, []int64{1}, repo.deletedIDs, "被拦截的码及其后的码都不应删除")
}

func TestAdminService_ExpireRedeemCode_UsedBlocked(t *testing.T) {
	repo := &redeemRepoStub{statusByID: map[int64]string{11: StatusUsed}}
	svc := &adminServiceImpl{redeemCodeRepo: repo}

	_, err := svc.ExpireRedeemCode(context.Background(), 11)
	require.Error(t, err)
	require.Contains(t, err.Error(), "已被使用")
	require.Contains(t, err.Error(), "不能作废")
	require.Empty(t, repo.updatedCodes, "已使用的兑换码不应被改写")
}

func TestAdminService_ExpireRedeemCode_ExpiredIdempotent(t *testing.T) {
	repo := &redeemRepoStub{statusByID: map[int64]string{12: StatusExpired}}
	svc := &adminServiceImpl{redeemCodeRepo: repo}

	code, err := svc.ExpireRedeemCode(context.Background(), 12)
	require.NoError(t, err)
	require.Equal(t, StatusExpired, code.Status)
	require.Empty(t, repo.updatedCodes, "重复作废不应产生写操作")
}

func TestAdminService_ExpireRedeemCode_DeliveredVoided(t *testing.T) {
	repo := &redeemRepoStub{statusByID: map[int64]string{13: StatusDelivered}}
	svc := &adminServiceImpl{redeemCodeRepo: repo}

	code, err := svc.ExpireRedeemCode(context.Background(), 13)
	require.NoError(t, err)
	require.Equal(t, StatusExpired, code.Status)
	require.Len(t, repo.updatedCodes, 1, "已发货的兑换码应被作废")
	require.Equal(t, StatusExpired, repo.updatedCodes[0].Status)
}
