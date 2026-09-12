package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

// fakeWelfareRepository 记录调用，用于服务层单测（不触库）。
type fakeWelfareRepository struct {
	createdBatch  *RedeemBatch
	createdCodes  []RedeemCode
	createdGrants []WelfareGroupGrant
	createErr     error

	codeGroups      []WelfareGroupGrant
	createdBalances []WelfareBalance
	activeSum       float64
	sumErr          error
	expiredRows     int64
	expiredAmount   float64
	listCodes       []RedeemCode
	lastListBatchID int64
	lastListStatus  string
}

func (f *fakeWelfareRepository) CreateWelfareBatch(_ context.Context, batch *RedeemBatch, codes []RedeemCode, grants []WelfareGroupGrant) error {
	if f.createErr != nil {
		return f.createErr
	}
	batch.ID = 42
	f.createdBatch = batch
	f.createdCodes = codes
	f.createdGrants = grants
	return nil
}

func (f *fakeWelfareRepository) ListBatches(_ context.Context, _ pagination.PaginationParams) ([]RedeemBatch, *pagination.PaginationResult, error) {
	return nil, nil, nil
}

func (f *fakeWelfareRepository) AttachCodeGroups(_ context.Context, _ int64, _ []WelfareGroupGrant) error {
	return nil
}

func (f *fakeWelfareRepository) ListCodeGroups(_ context.Context, _ int64) ([]WelfareGroupGrant, error) {
	return f.codeGroups, nil
}

func (f *fakeWelfareRepository) CreateWelfareBalance(_ context.Context, wb *WelfareBalance) error {
	wb.ID = int64(len(f.createdBalances) + 1)
	f.createdBalances = append(f.createdBalances, *wb)
	return nil
}

func (f *fakeWelfareRepository) SumActiveRemainingByUser(_ context.Context, _ int64) (float64, error) {
	return f.activeSum, f.sumErr
}

func (f *fakeWelfareRepository) NearestActiveExpiry(_ context.Context, _ int64) (*time.Time, error) {
	return nil, nil
}

func (f *fakeWelfareRepository) ExpireDueBalances(_ context.Context) (int64, float64, error) {
	return f.expiredRows, f.expiredAmount, nil
}

func (f *fakeWelfareRepository) ListCodesByBatch(_ context.Context, batchID int64, status string) ([]RedeemCode, error) {
	f.lastListBatchID = batchID
	f.lastListStatus = status
	return f.listCodes, nil
}

func TestRandomAmountInRange(t *testing.T) {
	t.Run("min==max 返回固定值", func(t *testing.T) {
		v, err := randomAmountInRange(2.5, 2.5)
		require.NoError(t, err)
		require.InDelta(t, 2.5, v, 1e-9)
	})

	t.Run("区间随机且保留两位小数", func(t *testing.T) {
		seen := make(map[float64]struct{})
		for i := 0; i < 500; i++ {
			v, err := randomAmountInRange(1, 3)
			require.NoError(t, err)
			require.GreaterOrEqual(t, v, 1.0)
			require.LessOrEqual(t, v, 3.0)
			// 两位小数：v*100 应为整数
			require.InDelta(t, float64(int64(v*100+0.5)), v*100, 1e-6)
			seen[v] = struct{}{}
		}
		// 500 次抽样应产生多个不同值（随机性成立）
		require.Greater(t, len(seen), 20)
	})

	t.Run("支持几毛钱的小数区间", func(t *testing.T) {
		v, err := randomAmountInRange(0.1, 0.3)
		require.NoError(t, err)
		require.GreaterOrEqual(t, v, 0.1)
		require.LessOrEqual(t, v, 0.3)
	})
}

func TestGenerateWelfareBatch_Validation(t *testing.T) {
	newSvc := func(repo WelfareRepository) *RedeemService {
		s := &RedeemService{}
		s.SetWelfareRepository(repo)
		return s
	}

	t.Run("仓储未配置", func(t *testing.T) {
		s := &RedeemService{}
		_, _, err := s.GenerateWelfareBatch(context.Background(), &GenerateWelfareBatchRequest{Count: 1, AmountMax: 5})
		require.Error(t, err)
	})

	t.Run("数量为 0 或超限", func(t *testing.T) {
		s := newSvc(&fakeWelfareRepository{})
		_, _, err := s.GenerateWelfareBatch(context.Background(), &GenerateWelfareBatchRequest{Count: 0, AmountMax: 5})
		require.Error(t, err)
		_, _, err = s.GenerateWelfareBatch(context.Background(), &GenerateWelfareBatchRequest{Count: 1001, AmountMax: 5})
		require.Error(t, err)
	})

	t.Run("分组与金额区间全空", func(t *testing.T) {
		s := newSvc(&fakeWelfareRepository{})
		_, _, err := s.GenerateWelfareBatch(context.Background(), &GenerateWelfareBatchRequest{Count: 1})
		require.Error(t, err)
	})

	t.Run("金额区间非法", func(t *testing.T) {
		s := newSvc(&fakeWelfareRepository{})
		_, _, err := s.GenerateWelfareBatch(context.Background(), &GenerateWelfareBatchRequest{Count: 1, AmountMin: -1, AmountMax: 5})
		require.Error(t, err)
		_, _, err = s.GenerateWelfareBatch(context.Background(), &GenerateWelfareBatchRequest{Count: 1, AmountMin: 5, AmountMax: 3})
		require.Error(t, err)
		// min>0 但 max 为 0：不得静默当作纯订阅卡
		_, _, err = s.GenerateWelfareBatch(context.Background(), &GenerateWelfareBatchRequest{Count: 1, AmountMin: 2, AmountMax: 0})
		require.Error(t, err)
	})

	t.Run("重复分组", func(t *testing.T) {
		s := newSvc(&fakeWelfareRepository{})
		_, _, err := s.GenerateWelfareBatch(context.Background(), &GenerateWelfareBatchRequest{
			Count:  1,
			Groups: []WelfareGroupGrant{{GroupID: 1, ValidityDays: 7}, {GroupID: 1, ValidityDays: 30}},
		})
		require.Error(t, err)
	})

	t.Run("有效天数非法", func(t *testing.T) {
		s := newSvc(&fakeWelfareRepository{})
		_, _, err := s.GenerateWelfareBatch(context.Background(), &GenerateWelfareBatchRequest{
			Count:  1,
			Groups: []WelfareGroupGrant{{GroupID: 1, ValidityDays: 0}},
		})
		require.Error(t, err)
	})
}

func TestGenerateWelfareBatch_Success(t *testing.T) {
	repo := &fakeWelfareRepository{}
	s := &RedeemService{}
	s.SetWelfareRepository(repo)

	batch, codes, err := s.GenerateWelfareBatch(context.Background(), &GenerateWelfareBatchRequest{
		Name:      "测试批次",
		Groups:    []WelfareGroupGrant{{GroupID: 7, ValidityDays: 7}},
		AmountMin: 1,
		AmountMax: 3,
		Count:     10,
		CreatedBy: 100,
	})
	require.NoError(t, err)
	require.Equal(t, int64(42), batch.ID)
	require.Len(t, codes, 10)

	seen := make(map[string]struct{})
	for i := range codes {
		require.Equal(t, RedeemTypeWelfare, codes[i].Type)
		require.Equal(t, StatusUnused, codes[i].Status)
		require.NotEmpty(t, codes[i].Code)
		// 卡 24 小时内可兑换
		require.NotNil(t, codes[i].ExpiresAt)
		require.WithinDuration(t, time.Now().Add(WelfareCodeValidity), *codes[i].ExpiresAt, time.Minute)
		// 随机金额落在区间
		require.GreaterOrEqual(t, codes[i].Value, 1.0)
		require.LessOrEqual(t, codes[i].Value, 3.0)
		seen[codes[i].Code] = struct{}{}
	}
	require.Len(t, seen, 10, "卡密不得重复")
}

func TestRedeemWelfare_MissingBatchID(t *testing.T) {
	s := &RedeemService{}
	s.SetWelfareRepository(&fakeWelfareRepository{})
	err := s.redeemWelfare(context.Background(), 1, &RedeemCode{Type: RedeemTypeWelfare})
	require.Error(t, err)
	require.Contains(t, err.Error(), "batch_id")
}

func TestListWelfareBatchCodes_Validation(t *testing.T) {
	repo := &fakeWelfareRepository{}
	s := &RedeemService{}
	s.SetWelfareRepository(repo)

	_, err := s.ListWelfareBatchCodes(context.Background(), 0, "")
	require.Error(t, err)

	_, err = s.ListWelfareBatchCodes(context.Background(), 1, "bogus")
	require.Error(t, err)

	_, err = s.ListWelfareBatchCodes(context.Background(), 1, StatusUnused)
	require.NoError(t, err)
	require.Equal(t, int64(1), repo.lastListBatchID)
	require.Equal(t, StatusUnused, repo.lastListStatus)
}

func TestWelfareExpiryService_RunOnce(t *testing.T) {
	repo := &fakeWelfareRepository{expiredRows: 3, expiredAmount: 7.5}
	svc := NewWelfareBalanceExpiryService(repo, time.Minute)
	require.NotPanics(t, func() { svc.runOnce() })

	// 仓储错误仅记录日志，不 panic
	repo2 := &fakeWelfareRepository{sumErr: errors.New("db down")}
	svc2 := NewWelfareBalanceExpiryService(repo2, time.Minute)
	svc2.welfareRepo = &errorExpiryRepo{fakeWelfareRepository: repo2}
	require.NotPanics(t, func() { svc2.runOnce() })
}

type errorExpiryRepo struct {
	*fakeWelfareRepository
}

func (e *errorExpiryRepo) ExpireDueBalances(_ context.Context) (int64, float64, error) {
	return 0, 0, errors.New("db down")
}
