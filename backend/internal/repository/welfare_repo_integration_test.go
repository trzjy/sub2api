//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/welfarebalance"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/suite"
)

// WelfareRepoSuite 在真实 PostgreSQL（生产同款 SQL 迁移建表）上验证福利仓储全链路：
// 批次+卡密+分组勾选写入、勾选读取、余额入账/汇总/清零、同批次一人一卡。
// redeem_code_groups 的表结构与 ent 模型曾有漂移（缺 id 列），
// 只有真库集成测试才能暴露，fake 仓储单测抓不到。
type WelfareRepoSuite struct {
	suite.Suite
	ctx    context.Context
	client *dbent.Client
	repo   *welfareRepository
	codes  *redeemCodeRepository
}

func (s *WelfareRepoSuite) SetupTest() {
	// CreateWelfareBatch 内部自管事务（r.client.Tx），不能包在测试事务里，
	// 否则 ent 报嵌套事务错误；直接用全局 client，写入即提交，与生产行为一致。
	s.ctx = context.Background()
	s.client = testEntClient(s.T())
	s.repo = NewWelfareRepository(s.client).(*welfareRepository)
	s.codes = NewRedeemCodeRepository(s.client).(*redeemCodeRepository)
}

func TestWelfareRepoSuite(t *testing.T) {
	suite.Run(t, new(WelfareRepoSuite))
}

func (s *WelfareRepoSuite) createSubscriptionGroup(name string) *dbent.Group {
	g, err := s.client.Group.Create().
		SetName(name).
		SetSubscriptionType(service.SubscriptionTypeSubscription).
		Save(s.ctx)
	s.Require().NoError(err, "create subscription group")
	return g
}

// welfareCode35 构造与生产生成器一致的 35 字符带连字符码，后缀保证同批内唯一。
func welfareCode35(i int) string {
	return fmt.Sprintf("ABCD1234-EFGH5678-IJKL9012-MNOP34%02d", i)
}

// TestCreateWelfareBatch_FullChain 复刻生产创建路径：批次 + 卡密（35 字符带连字符格式）
// + 每卡×每分组勾选快照，随后读回校验。码长超列宽、redeem_code_groups 缺 id 列
// 两类漂移都只能在这里暴露。
func (s *WelfareRepoSuite) TestCreateWelfareBatch_FullChain() {
	g1 := s.createSubscriptionGroup("welfare-it-deepseek")
	g2 := s.createSubscriptionGroup("welfare-it-kimi")

	batch := &service.RedeemBatch{
		Name:      "it-福利批次-全链路",
		CodeCount: 3,
		Config:    map[string]any{"count": 3},
	}
	grants := []service.WelfareGroupGrant{
		{GroupID: g1.ID, ValidityDays: 1},
		{GroupID: g2.ID, ValidityDays: 7},
	}
	expiresAt := time.Now().Add(24 * time.Hour)
	codes := make([]service.RedeemCode, 0, 3)
	for i := 0; i < 3; i++ {
		codes = append(codes, service.RedeemCode{
			Code:      welfareCode35(i),
			Type:      service.RedeemTypeWelfare,
			Value:     1.5,
			Status:    service.StatusUnused,
			ExpiresAt: &expiresAt,
		})
	}

	s.Require().NoError(s.repo.CreateWelfareBatch(s.ctx, batch, codes, grants))
	s.Require().Positive(batch.ID, "批次应已落库并回填 ID")

	// 卡密：35 字符带连字符原样落库，welfare 类型 + 批次归属 + 24h 过期。
	stored, err := s.repo.ListCodesByBatch(s.ctx, batch.ID, "")
	s.Require().NoError(err)
	s.Require().Len(stored, 3)
	s.Require().Len(stored[0].Code, 35)
	s.Require().Equal(service.RedeemTypeWelfare, stored[0].Type)
	s.Require().NotNil(stored[0].BatchID)
	s.Require().Equal(batch.ID, *stored[0].BatchID)
	s.Require().NotNil(stored[0].ExpiresAt)

	// 分组勾选快照：每卡两行，天数读回一致。
	for _, code := range stored {
		got, err := s.repo.ListCodeGroups(s.ctx, code.ID)
		s.Require().NoError(err)
		s.Require().Len(got, 2)
		s.Require().Equal(1, got[0].ValidityDays)
		s.Require().Equal(7, got[1].ValidityDays)
	}

	// AttachCodeGroups（单卡补挂分组）写同一张表。
	g3 := s.createSubscriptionGroup("welfare-it-claude")
	s.Require().NoError(s.repo.AttachCodeGroups(s.ctx, stored[0].ID,
		[]service.WelfareGroupGrant{{GroupID: g3.ID, ValidityDays: 30}}))
	got, err := s.repo.ListCodeGroups(s.ctx, stored[0].ID)
	s.Require().NoError(err)
	s.Require().Len(got, 3)

	// 批次列表可读回。
	batches, page, err := s.repo.ListBatches(s.ctx, pagination.PaginationParams{Page: 1, PageSize: 10})
	s.Require().NoError(err)
	s.Require().GreaterOrEqual(len(batches), 1)
	s.Require().NotNil(page)
}

// TestOnePerUserAndBalances 同批次一人一卡 + 余额入账/汇总/最近到期/清零。
func (s *WelfareRepoSuite) TestOnePerUserAndBalances() {
	user, err := s.client.User.Create().
		SetEmail(fmt.Sprintf("welfare-it-%d@test.local", time.Now().UnixNano())).
		SetPasswordHash("x").
		Save(s.ctx)
	s.Require().NoError(err)

	batch := &service.RedeemBatch{Name: "it-一人一卡", CodeCount: 2, Config: map[string]any{}}
	codes := []service.RedeemCode{
		{Code: welfareCode35(90), Type: service.RedeemTypeWelfare, Status: service.StatusUnused},
		{Code: welfareCode35(91), Type: service.RedeemTypeWelfare, Status: service.StatusUnused},
	}
	s.Require().NoError(s.repo.CreateWelfareBatch(s.ctx, batch, codes, nil))

	first, err := s.repo.ListCodesByBatch(s.ctx, batch.ID, service.StatusUnused)
	s.Require().NoError(err)
	s.Require().Len(first, 2)

	// 首次兑换成功。
	s.Require().NoError(s.codes.Use(s.ctx, first[0].ID, user.ID))

	// 同批次第二张：唯一索引 idx_redeem_codes_batch_one_per_user 命中。
	err = s.codes.Use(s.ctx, first[1].ID, user.ID)
	s.Require().ErrorIs(err, service.ErrRedeemBatchOnePerUser)

	// 余额入账 + 汇总 + 最近到期。
	s.Require().NoError(s.repo.CreateWelfareBalance(s.ctx, &service.WelfareBalance{
		UserID:          user.ID,
		RedeemCodeID:    first[0].ID,
		BatchID:         batch.ID,
		AmountInitial:   1.5,
		AmountRemaining: 1.25,
		Status:          service.WelfareStatusActive,
		ExpiresAt:       time.Now().Add(7 * 24 * time.Hour),
	}))
	sum, err := s.repo.SumActiveRemainingByUser(s.ctx, user.ID)
	s.Require().NoError(err)
	s.InDelta(1.25, sum, 1e-9)

	nearest, err := s.repo.NearestActiveExpiry(s.ctx, user.ID)
	s.Require().NoError(err)
	s.Require().NotNil(nearest)

	// 清零任务：未到期不动。
	cleared, amount, err := s.repo.ExpireDueBalances(s.ctx)
	s.Require().NoError(err)
	s.Require().Zero(cleared)
	s.Require().Zero(amount)

	// 置为已过期后清零：只动 welfare_balances，行置 exhausted。
	wb, err := s.client.WelfareBalance.Query().
		Where(welfarebalance.UserIDEQ(user.ID)).
		Only(s.ctx)
	s.Require().NoError(err)
	_, err = s.client.WelfareBalance.UpdateOneID(wb.ID).
		SetExpiresAt(time.Now().Add(-time.Hour)).
		Save(s.ctx)
	s.Require().NoError(err)

	cleared, amount, err = s.repo.ExpireDueBalances(s.ctx)
	s.Require().NoError(err)
	s.Require().Equal(int64(1), cleared)
	s.InDelta(1.25, amount, 1e-9)

	sum, err = s.repo.SumActiveRemainingByUser(s.ctx, user.ID)
	s.Require().NoError(err)
	s.Require().Zero(sum)
}
