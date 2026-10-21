//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type accountRepoStubForCompositeModelsList struct {
	accountRepoStub
	accounts []Account
	byGroup  map[int64][]Account
}

func (s *accountRepoStubForCompositeModelsList) ListSchedulableByGroupID(_ context.Context, groupID int64) ([]Account, error) {
	// 发现 3：支持按 groupID 返回不同账号集合（未配置 byGroup 时回退默认 accounts）。
	if s.byGroup != nil {
		if accs, ok := s.byGroup[groupID]; ok {
			return accs, nil
		}
	}
	return s.accounts, nil
}

func TestAdminService_CreateCompositeGroupCopiesAccountsFromConcreteGroups(t *testing.T) {
	var copiedFrom []int64
	var boundGroupID int64
	var boundAccountIDs []int64
	groupRepo := &groupRepoStubForAdmin{
		createID: 99,
		getByIDByID: map[int64]*Group{
			10: {ID: 10, Platform: PlatformOpenAI},
			20: {ID: 20, Platform: PlatformGemini},
		},
		getAccountIDsByGroupIDsFn: func(groupIDs []int64) ([]int64, error) {
			copiedFrom = append([]int64{}, groupIDs...)
			return []int64{101, 202}, nil
		},
		bindAccountsToGroupFn: func(groupID int64, accountIDs []int64) error {
			boundGroupID = groupID
			boundAccountIDs = append([]int64{}, accountIDs...)
			return nil
		},
	}
	svc := &adminServiceImpl{groupRepo: groupRepo}

	group, err := svc.CreateGroup(context.Background(), &CreateGroupInput{
		Name:                        "Composite",
		Platform:                    PlatformComposite,
		RateMultiplier:              1,
		MaxReasoningEffort:          "medium",
		MaxReasoningEffortOverLimit: ReasoningEffortOverLimitDeny,
		ReasoningEffortMappings: []ReasoningEffortMapping{
			{From: "max", To: "xhigh"},
		},
		CopyAccountsFromGroupIDs: []int64{10, 20, 10},
	})

	require.NoError(t, err)
	require.Equal(t, PlatformComposite, groupRepo.created.Platform)
	require.Equal(t, "medium", groupRepo.created.MaxReasoningEffort)
	require.Equal(t, ReasoningEffortOverLimitDeny, groupRepo.created.MaxReasoningEffortOverLimit)
	require.Equal(t, []ReasoningEffortMapping{{From: "max", To: "xhigh"}}, groupRepo.created.ReasoningEffortMappings)
	require.Equal(t, int64(99), group.ID)
	require.Equal(t, int64(2), group.AccountCount)
	require.ElementsMatch(t, []int64{10, 20}, copiedFrom)
	require.Equal(t, int64(99), boundGroupID)
	require.ElementsMatch(t, []int64{101, 202}, boundAccountIDs)
}

func TestAdminService_UpdateCompositeGroupCopiesAccountsFromConcreteGroups(t *testing.T) {
	var clearedGroupID int64
	var copiedFrom []int64
	var boundGroupID int64
	var boundAccountIDs []int64
	groupRepo := &groupRepoStubForAdmin{
		getByIDByID: map[int64]*Group{
			10: {ID: 10, Platform: PlatformOpenAI},
			20: {ID: 20, Platform: PlatformGrok},
			99: {ID: 99, Platform: PlatformComposite, RateMultiplier: 1, SubscriptionType: SubscriptionTypeStandard},
		},
		deleteAccountGroupsByGroupIDFn: func(groupID int64) (int64, error) {
			clearedGroupID = groupID
			return 2, nil
		},
		getAccountIDsByGroupIDsFn: func(groupIDs []int64) ([]int64, error) {
			copiedFrom = append([]int64{}, groupIDs...)
			return []int64{301, 302}, nil
		},
		bindAccountsToGroupFn: func(groupID int64, accountIDs []int64) error {
			boundGroupID = groupID
			boundAccountIDs = append([]int64{}, accountIDs...)
			return nil
		},
	}
	svc := &adminServiceImpl{groupRepo: groupRepo}
	maxReasoningEffort := "low"
	maxReasoningEffortOverLimit := ReasoningEffortOverLimitDeny
	reasoningEffortMappings := []ReasoningEffortMapping{{From: "max", To: "high"}}

	group, err := svc.UpdateGroup(context.Background(), 99, &UpdateGroupInput{
		MaxReasoningEffort:          &maxReasoningEffort,
		MaxReasoningEffortOverLimit: &maxReasoningEffortOverLimit,
		ReasoningEffortMappings:     &reasoningEffortMappings,
		CopyAccountsFromGroupIDs:    []int64{10, 20},
	})

	require.NoError(t, err)
	require.Equal(t, PlatformComposite, group.Platform)
	require.Equal(t, "low", group.MaxReasoningEffort)
	require.Equal(t, ReasoningEffortOverLimitDeny, group.MaxReasoningEffortOverLimit)
	require.Equal(t, reasoningEffortMappings, group.ReasoningEffortMappings)
	require.Equal(t, int64(99), clearedGroupID)
	require.ElementsMatch(t, []int64{10, 20}, copiedFrom)
	require.Equal(t, int64(99), boundGroupID)
	require.ElementsMatch(t, []int64{301, 302}, boundAccountIDs)
}

func TestAdminService_CreateAccountAllowsCompositeGroupAssignment(t *testing.T) {
	accountRepo := &accountRepoStubForBulkUpdate{createID: 7}
	groupRepo := &groupRepoStubForAdmin{
		getByIDByID: map[int64]*Group{
			99: {ID: 99, Platform: PlatformComposite},
		},
	}
	svc := &adminServiceImpl{accountRepo: accountRepo, groupRepo: groupRepo}

	account, err := svc.CreateAccount(context.Background(), &CreateAccountInput{
		Name:                  "OpenAI account",
		Platform:              PlatformOpenAI,
		Type:                  AccountTypeAPIKey,
		Concurrency:           1,
		GroupIDs:              []int64{99},
		SkipDefaultGroupBind:  true,
		SkipMixedChannelCheck: true,
	})

	require.NoError(t, err)
	require.Equal(t, int64(7), account.ID)
	require.Equal(t, PlatformOpenAI, accountRepo.createAccount.Platform)
	require.ElementsMatch(t, []int64{99}, accountRepo.bindGroupsByAccount[7])
}

func TestAdminService_UpdateAccountAllowsCompositeGroupAssignment(t *testing.T) {
	accountRepo := &accountRepoStubForBulkUpdate{
		getByIDAccounts: map[int64]*Account{
			7: {ID: 7, Platform: PlatformGemini, Type: AccountTypeAPIKey, Status: StatusActive, Extra: map[string]any{}},
		},
	}
	groupRepo := &groupRepoStubForAdmin{
		getByIDByID: map[int64]*Group{
			99: {ID: 99, Platform: PlatformComposite},
		},
	}
	svc := &adminServiceImpl{accountRepo: accountRepo, groupRepo: groupRepo}
	groupIDs := []int64{99}

	account, err := svc.UpdateAccount(context.Background(), 7, &UpdateAccountInput{
		GroupIDs:              &groupIDs,
		SkipMixedChannelCheck: true,
	})

	require.NoError(t, err)
	require.Equal(t, int64(7), account.ID)
	require.Len(t, accountRepo.updatedAccounts, 1)
	require.ElementsMatch(t, []int64{99}, accountRepo.bindGroupsByAccount[7])
}

// composite 分组候选 = 组内具体平台账号真实目录的并集；不适用账号（如别的平台
// 分组）不掺入；无平台默认目录注入（Grok/Antigravity 默认注入、composite 默认
// 并集、muse/codebuddy default 分支均不得注入候选，见 plan §5.2/§6）。
func TestAdminService_CompositeModelsListCandidatesUnionConcreteAccountCatalogs(t *testing.T) {
	reader := &stubGroupModelCatalogReader{
		byAccount: map[int64][]string{
			1: {"gpt-5.5"},
			2: {"gemini-2.5-flash"},
			3: {"kimi-k2"},
		},
	}
	accounts := []Account{
		{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{}},
		{ID: 2, Platform: PlatformGemini, Type: AccountTypeAPIKey, Credentials: map[string]any{}},
		{ID: 3, Platform: PlatformKimi, Type: AccountTypeAPIKey, Credentials: map[string]any{}},
	}
	svc := svcForCandidateTest(reader, nil, accounts)

	candidates, err := svc.GetGroupModelsListCandidates(context.Background(), 99, PlatformComposite)

	require.NoError(t, err)
	require.Contains(t, candidates, "gpt-5.5")
	require.Contains(t, candidates, "gemini-2.5-flash")
	require.Contains(t, candidates, "kimi-k2")
	// 无平台默认目录注入。
	require.NotContains(t, candidates, "gpt-4o")
	require.NotContains(t, candidates, "claude-sonnet-4-5")
}

// 独立 CN 分组候选不落回任何静态默认目录：账号真实目录为空、且无显式 alias 时，
// 候选必须为空（不得注入任何内置默认模型列表，见 plan §5.2/§6）。
func TestAdminService_CNProviderModelsListCandidatesHaveNoStaticDefaults(t *testing.T) {
	for _, platform := range []string{PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax} {
		reader := &stubGroupModelCatalogReader{byAccount: map[int64][]string{}}
		accounts := []Account{
			{ID: 1, Platform: platform, Type: AccountTypeAPIKey, Credentials: map[string]any{"model_mapping": map[string]any{}}},
		}
		svc := svcForCandidateTest(reader, nil, accounts)
		candidates, err := svc.GetGroupModelsListCandidates(context.Background(), 88, platform)
		require.NoError(t, err)
		require.Empty(t, candidates, "platform=%s must not inject static default candidates when catalog is empty", platform)
	}
}

// CN 分组候选只来自本平台（同平台）账号的真实目录 + 显式精确 alias；别平台账号
// （即便同组）不污染本组候选。
func TestAdminService_CNProviderGroupModelsListCandidatesUseAccountMappingsOnly(t *testing.T) {
	reader := &stubGroupModelCatalogReader{
		byAccount: map[int64][]string{
			1: {"deepseek-chat"},
			2: {"gpt-5"},
		},
	}
	accounts := []Account{
		{
			ID:       1,
			Platform: PlatformDeepseek,
			Type:     AccountTypeAPIKey,
			Credentials: map[string]any{
				"model_mapping": map[string]any{"deepseek-v4-flash": "deepseek-chat"},
			},
		},
		{
			ID:       2,
			Platform: PlatformOpenAI,
			Type:     AccountTypeAPIKey,
			Credentials: map[string]any{
				"model_mapping": map[string]any{"gpt-custom": "gpt-5"},
			},
		},
	}
	svc := svcForCandidateTest(reader, nil, accounts)

	candidates, err := svc.GetGroupModelsListCandidates(context.Background(), 88, PlatformDeepseek)

	require.NoError(t, err)
	// 仅 deepseek 账号（同平台）的显式 alias 命中目录 → 列出；openai 账号（别平台）不污染。
	require.Equal(t, []string{"deepseek-v4-flash"}, candidates)
}

// ListTempUnschedulableAccounts / SetTempUnschedulableReason 是健康探测恢复
// 接口补齐的 no-op 桩（AccountRepository 近期新增方法，本测试桩无需行为）。
func (r *accountRepoStubForCompositeModelsList) ListTempUnschedulableAccounts(_ context.Context, _ time.Time, _ int) ([]*Account, error) {
	return nil, nil
}

func (r *accountRepoStubForCompositeModelsList) SetTempUnschedulableReason(_ context.Context, _ int64, _ string) error {
	return nil
}

// 发现 3：从全链路入口 GetGroupModelsListCandidates 断言组 A 账号的目录不出现在组 B
// 的候选中（账号读取按 groupID 隔离；accountRepoStubForCompositeModelsList.byGroup
// 返回组专属账号集合）。Query 组 20 时即便组 10 含同平台（OpenAI）账号，其目录模型
// 也不应进入组 20 的候选；若账号读取隔离失效，gpt-a-only 会被组 10 的 OpenAI 账号
// 供给校验命中而混入，故 NotContains 是有效的隔离断言。
func TestAdminService_GetGroupModelsListCandidates_IsolatesOtherGroupCatalogs(t *testing.T) {
	groupRepo := &groupRepoStubForAdmin{
		getByIDByID: map[int64]*Group{
			10: {ID: 10, Platform: PlatformComposite},
			20: {ID: 20, Platform: PlatformComposite},
		},
	}
	accountRepo := &accountRepoStubForCompositeModelsList{
		byGroup: map[int64][]Account{
			10: {
				{ID: 101, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
					Credentials: map[string]any{"model_mapping": map[string]any{"gpt-a-only": "gpt-a-only"}}},
			},
			20: {
				{ID: 202, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
					Credentials: map[string]any{"model_mapping": map[string]any{"gpt-b-only": "gpt-b-only"}}},
			},
		},
	}
	reader := &stubGroupModelCatalogReader{
		byAccount: map[int64][]string{
			101: {"gpt-a-only"},
			202: {"gpt-b-only"},
		},
	}
	svc := &adminServiceImpl{
		groupRepo:               groupRepo,
		accountRepo:             accountRepo,
		groupModelCatalogReader: reader,
		compositeRouteRepo:      &compositeRouteRepoStubForAdmin{},
		compositeResolver:       NewCompositeRouteResolver(&compositeRouteRepoStubForAdmin{}),
	}

	candidates, err := svc.GetGroupModelsListCandidates(context.Background(), 20, PlatformComposite)
	require.NoError(t, err)
	// 组 20 自身目录（gpt-b-only）正常列出。
	require.Contains(t, candidates, "gpt-b-only")
	// 组 A（组 10）账号的目录模型不得出现在组 B（组 20）候选中。
	require.NotContains(t, candidates, "gpt-a-only")
}
