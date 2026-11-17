//go:build integration

package repository

import (
	"context"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/suite"
)

// AggregateBindingSuite 验证 queryAccountsByGroup 的"绑定即准入"读取链：
// 有分组时按 account_groups 中间表返回组内全部绑定账号（含 codebuddy 账号），
// 不再按平台过滤，也不读取分组上的开关字段；无分组查询结果为空。
// 纯 helper 用例见 account_repo_aggregate_pool_test.go（unit 侧）。
type AggregateBindingSuite struct {
	suite.Suite
	ctx         context.Context
	client      *dbent.Client
	accountRepo *accountRepository
}

func (s *AggregateBindingSuite) SetupTest() {
	s.ctx = context.Background()
	tx := testEntTx(s.T())
	s.client = tx.Client()
	s.accountRepo = newAccountRepositoryWithSQL(s.client, tx, nil)
}

func TestAggregateBindingSuite(t *testing.T) {
	suite.Run(t, new(AggregateBindingSuite))
}

// TestQueryAccountsByGroupBindingAdmitsAllBoundAccounts 覆盖"绑定即准入"语义：
//   - 组内绑定了原生账号与 codebuddy 账号时，两者都进入候选（按优先级排序）；
//   - groupID 不存在（无绑定行）时查询结果为空，不报错。
func (s *AggregateBindingSuite) TestQueryAccountsByGroupBindingAdmitsAllBoundAccounts() {
	group := mustCreateGroup(s.T(), s.client, &service.Group{
		Name:     "agg-binding-suite",
		Platform: service.PlatformDeepseek,
	})
	native := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:     "agg-native-deepseek",
		Platform: service.PlatformDeepseek,
	})
	shadow := mustCreateAccount(s.T(), s.client, &service.Account{
		Name:     "agg-shadow-codebuddy",
		Platform: service.PlatformCodeBuddy,
	})
	mustBindAccountToGroup(s.T(), s.client, native.ID, group.ID, 10)
	mustBindAccountToGroup(s.T(), s.client, shadow.ID, group.ID, 20)

	opts := accountGroupQueryOptions{status: service.StatusActive, schedulable: true}

	// 绑定即准入：组内全部账号（含 codebuddy）都进入候选。
	got, err := s.accountRepo.queryAccountsByGroup(s.ctx, group.ID, opts)
	s.Require().NoError(err)
	s.Require().Len(got, 2)
	s.Require().Equal(native.ID, got[0].ID)
	s.Require().Equal(shadow.ID, got[1].ID)

	// groupID 不存在（无绑定行）：返回空候选。
	got, err = s.accountRepo.queryAccountsByGroup(s.ctx, group.ID+999999, opts)
	s.Require().NoError(err)
	s.Require().Empty(got)
}
