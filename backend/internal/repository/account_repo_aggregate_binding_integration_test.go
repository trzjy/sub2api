//go:build integration

package repository

import (
	"context"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/suite"
)

// AggregateBindingSuite 验证 queryAccountsByGroup 的聚合直绑开关真实读取链
// （B2 聚合开关批次，终审 P2 闭合）：分组行 aggregate_codebuddy_enabled 经
// 同一查询边界内的 Group.Get 读取，true 并入 codebuddy 候选、false 不并入、
// groups 行读取失败失败关闭。纯 helper 用例见
// account_repo_aggregate_pool_test.go（unit 侧）。
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

// TestQueryAccountsByGroupBindingFlipAndFailClosed 覆盖 true / false / 读取失败
// 三种结果：
//   - 开关=true：聚合分组候选含 codebuddy 账号与原生账号；
//   - 开关=false（经 UpdateGroup 同字段权威路径翻转）：候选仅原生账号；
//   - groupID 不存在（groups 行读取失败）：查询报错，失败关闭，不静默取默认。
func (s *AggregateBindingSuite) TestQueryAccountsByGroupBindingFlipAndFailClosed() {
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

	// 开关默认 false（迁移缺省值）：不并入 codebuddy。
	got, err := s.accountRepo.queryAccountsByGroup(s.ctx, group.ID, opts)
	s.Require().NoError(err)
	s.Require().Len(got, 1)
	s.Require().Equal(native.ID, got[0].ID)

	// 经权威路径翻转为 true：候选并入 codebuddy。
	_, err = s.client.Group.UpdateOneID(group.ID).
		SetAggregateCodebuddyEnabled(true).
		Save(s.ctx)
	s.Require().NoError(err)
	got, err = s.accountRepo.queryAccountsByGroup(s.ctx, group.ID, opts)
	s.Require().NoError(err)
	s.Require().Len(got, 2)

	// 翻回 false：恢复仅原生。
	_, err = s.client.Group.UpdateOneID(group.ID).
		SetAggregateCodebuddyEnabled(false).
		Save(s.ctx)
	s.Require().NoError(err)
	got, err = s.accountRepo.queryAccountsByGroup(s.ctx, group.ID, opts)
	s.Require().NoError(err)
	s.Require().Len(got, 1)
	s.Require().Equal(native.ID, got[0].ID)

	// groups 行读取失败（不存在的 groupID）：失败关闭报错。
	_, err = s.accountRepo.queryAccountsByGroup(s.ctx, group.ID+999999, opts)
	s.Require().Error(err, "开关读取失败必须失败关闭，不得静默取默认")
}
