package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// Task A (PR-1 平台归并重构)：B3 两级池 + B2 /v1/messages 过滤 + 显式非法 access_mode
// fail-closed。以下测试钉死 docs/platform-merge-refactor-plan.md §5.1/§5.2 的核心行为。

// --- 纯函数层（候选序单测，PR-1 必需） ---

func taskAMakeAccount(id int64, accessMode string, priority int) *Account {
	acc := &Account{
		ID:          id,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Priority:    priority,
		GroupIDs:    []int64{101},
	}
	if accessMode != "" {
		acc.Credentials = map[string]any{"access_mode": accessMode}
	}
	return acc
}

func TestTaskA_PartitionOpenAIAccessModePools(t *testing.T) {
	api := taskAMakeAccount(1, AccountAccessModeAPI, 0)
	web := taskAMakeAccount(2, AccountAccessModeWeb, 0)
	apiViaShape := taskAMakeAccount(3, "", 0) // 缺省 + 无 web 形状 → api
	pool := []*Account{web, api, apiViaShape}

	apiPool, webPool := partitionOpenAIAccessModePools(pool)
	require.Len(t, apiPool, 2)
	require.Len(t, webPool, 1)
	require.Equal(t, int64(2), webPool[0].ID)
	for _, a := range apiPool {
		require.False(t, a.IsWebAccessMode(), "api pool must not contain web accounts")
	}
}

func TestTaskA_TwoTierPoolOrdering(t *testing.T) {
	// priority 升序；同优先级内 API 池始终在 Web 池之前；池边界不被随机打乱破坏。
	accounts := []*Account{
		taskAMakeAccount(10, AccountAccessModeWeb, 5),
		taskAMakeAccount(11, AccountAccessModeAPI, 5),
		taskAMakeAccount(12, AccountAccessModeAPI, 1),
		taskAMakeAccount(13, AccountAccessModeWeb, 1),
		taskAMakeAccount(14, AccountAccessModeAPI, 3),
	}
	sortOpenAITwoTierByPriorityAndLastUsed(accounts, false)

	// 断言：整体 priority 非降序。
	for i := 1; i < len(accounts); i++ {
		require.LessOrEqual(t, accounts[i-1].Priority, accounts[i].Priority)
	}
	// 断言：同优先级内所有 API 账号出现在所有 Web 账号之前（池边界）。
	for prio := 1; prio <= 5; prio += 2 {
		seenWeb := false
		for _, a := range accounts {
			if a.Priority != prio {
				continue
			}
			if a.IsWebAccessMode() {
				seenWeb = true
			} else {
				require.False(t, seenWeb, "priority %d: API account must precede Web account", prio)
			}
		}
	}
}

func TestTaskA_IsBetterAccountTwoTier(t *testing.T) {
	svc := &OpenAIGatewayService{}
	// 优先级主导：高优先级 Web（priority 低值）击败低优先级 API（priority 高值）。
	webHigh := taskAMakeAccount(2, AccountAccessModeWeb, 1)
	apiLow := taskAMakeAccount(1, AccountAccessModeAPI, 9)
	require.True(t, svc.isBetterAccount(webHigh, apiLow), "higher priority (web) must win over lower priority (api)")
	// 同优先级：API 先于 Web（池边界）。
	apiSame := taskAMakeAccount(3, AccountAccessModeAPI, 5)
	webSame := taskAMakeAccount(4, AccountAccessModeWeb, 5)
	require.True(t, svc.isBetterAccount(apiSame, webSame), "same priority: API pool must precede Web pool")
	// 反向：低优先级 Web（priority 高值）不击败高优先级 API（priority 低值）。
	webLowerPrio := taskAMakeAccount(5, AccountAccessModeWeb, 9)
	apiHigherPrio := taskAMakeAccount(6, AccountAccessModeAPI, 1)
	require.False(t, svc.isBetterAccount(webLowerPrio, apiHigherPrio), "lower priority (web) must not win over higher priority (api)")
}

func TestTaskA_OpenAIAccessModeExcludeReason(t *testing.T) {
	api := taskAMakeAccount(1, AccountAccessModeAPI, 0)
	web := taskAMakeAccount(2, AccountAccessModeWeb, 0)
	bad := taskAMakeAccount(3, "bogus", 0)
	require.Equal(t, "", openAIAccessModeExcludeReason(api))
	require.Equal(t, "", openAIAccessModeExcludeReason(web))
	require.Equal(t, "access_mode_invalid", openAIAccessModeExcludeReason(bad))
}

func TestTaskA_MessagesDispatchContext(t *testing.T) {
	ctx := context.Background()
	require.False(t, isOpenAIMessagesDispatchContext(ctx))
	withFlag := WithOpenAIMessagesDispatchContext(ctx)
	require.True(t, isOpenAIMessagesDispatchContext(withFlag))
	// 标志须随 context 传递（handler 经 WithOpenAIRequestPricingContext 包裹后仍保留）。
	// 此处验证包裹派生 context 不丢失标志。
	derived := context.WithValue(withFlag, struct{}{}, nil)
	require.True(t, isOpenAIMessagesDispatchContext(derived))
}

// --- 行为层：legacy 路径（schedulerSnapshot=nil → selectBestAccount） ---

func taskALegacyService(accounts []Account) *OpenAIGatewayService {
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LoadBatchEnabled = false
	return &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}
}

func TestTaskA_LegacyTwoTierSelectsAPIBeforeWeb(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	svc := taskALegacyService([]Account{
		*taskAMakeAccount(1, AccountAccessModeWeb, 0),
		*taskAMakeAccount(2, AccountAccessModeAPI, 0),
	})
	sel, _, err := svc.SelectAccountWithSchedulerForCapability(
		context.Background(), taskAPtrInt64(101), "", "", "gpt-4o", nil,
		OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
		false, false, false, PlatformOpenAI,
	)
	require.NoError(t, err)
	require.NotNil(t, sel)
	require.NotNil(t, sel.Account)
	require.Equal(t, int64(2), sel.Account.ID, "same priority: API pool must be selected before Web pool")
}

func TestTaskA_LegacyTwoTierWebFallbackWhenAPIExhausted(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	api := taskAMakeAccount(1, AccountAccessModeAPI, 0)
	api.Schedulable = false // API 池不可选
	svc := taskALegacyService([]Account{*api, *taskAMakeAccount(2, AccountAccessModeWeb, 0)})
	sel, _, err := svc.SelectAccountWithSchedulerForCapability(
		context.Background(), taskAPtrInt64(101), "", "", "gpt-4o", nil,
		OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
		false, false, false, PlatformOpenAI,
	)
	require.NoError(t, err)
	require.NotNil(t, sel)
	require.NotNil(t, sel.Account)
	require.Equal(t, int64(2), sel.Account.ID, "Web pool must be selected when API pool is exhausted")
}

func TestTaskA_LegacyB2MessagesOnlyAPI(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	ctx := WithOpenAIMessagesDispatchContext(context.Background())

	// 混合分组：/v1/messages 只命中 API 账号。
	svc := taskALegacyService([]Account{
		*taskAMakeAccount(1, AccountAccessModeWeb, 0),
		*taskAMakeAccount(2, AccountAccessModeAPI, 0),
	})
	sel, _, err := svc.SelectAccountWithSchedulerForCapability(
		ctx, taskAPtrInt64(101), "", "", "gpt-4o", nil,
		OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
		false, false, false, PlatformOpenAI,
	)
	require.NoError(t, err)
	require.NotNil(t, sel)
	require.Equal(t, int64(2), sel.Account.ID, "messages must only match API accounts")

	// 仅 Web 账号：/v1/messages 下不可选。
	svcOnlyWeb := taskALegacyService([]Account{*taskAMakeAccount(3, AccountAccessModeWeb, 0)})
	_, _, err = svcOnlyWeb.SelectAccountWithSchedulerForCapability(
		ctx, taskAPtrInt64(101), "", "", "gpt-4o", nil,
		OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
		false, false, false, PlatformOpenAI,
	)
	require.Error(t, err, "messages must not select a Web-only account")
}

func TestTaskA_LegacyFailClosedInvalidAccessMode(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	svc := taskALegacyService([]Account{*taskAMakeAccount(1, "bogus", 0)})
	_, _, err := svc.SelectAccountWithSchedulerForCapability(
		context.Background(), taskAPtrInt64(101), "", "", "gpt-4o", nil,
		OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
		false, false, false, PlatformOpenAI,
	)
	require.Error(t, err, "explicitly invalid access_mode must be fail-closed (unschedulable)")
}

func TestTaskA_LegacyStickyNoWeb(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:sess-1": 2}}
	api := taskAMakeAccount(1, AccountAccessModeAPI, 0)
	web := taskAMakeAccount(2, AccountAccessModeWeb, 0)
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LoadBatchEnabled = false
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: []Account{*api, *web}},
		cache:              cache,
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}
	sel, _, err := svc.SelectAccountWithSchedulerForCapability(
		context.Background(), taskAPtrInt64(101), "", "sess-1", "gpt-4o", nil,
		OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
		false, false, false, PlatformOpenAI,
	)
	require.NoError(t, err)
	require.NotNil(t, sel)
	require.Equal(t, int64(1), sel.Account.ID, "sticky must not route to a Web account")
	_, deleted := cache.deletedSessions["openai:sess-1"]
	require.True(t, deleted, "Web sticky binding must be cleared")
}

// --- 行为层：advanced 路径（schedulerSnapshot=nil + 高级调度开启 → selectByLoadBalance） ---

func taskAAdvancedService(accounts []Account) *OpenAIGatewayService {
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LoadBatchEnabled = false
	return &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
	}
}

func TestTaskA_AdvancedTwoTierSelectsAPIBeforeWeb(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	svc := taskAAdvancedService([]Account{
		*taskAMakeAccount(1, AccountAccessModeWeb, 0),
		*taskAMakeAccount(2, AccountAccessModeAPI, 0),
	})
	sel, _, err := svc.SelectAccountWithSchedulerForCapability(
		context.Background(), taskAPtrInt64(101), "", "", "gpt-4o", nil,
		OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
		false, false, false, PlatformOpenAI,
	)
	require.NoError(t, err)
	require.NotNil(t, sel)
	require.NotNil(t, sel.Account)
	require.Equal(t, int64(2), sel.Account.ID, "advanced path: API pool must precede Web pool")
}

func TestTaskA_AdvancedTwoTierWebFallbackWhenAPIExhausted(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	api := taskAMakeAccount(1, AccountAccessModeAPI, 0)
	api.Schedulable = false
	svc := taskAAdvancedService([]Account{*api, *taskAMakeAccount(2, AccountAccessModeWeb, 0)})
	sel, _, err := svc.SelectAccountWithSchedulerForCapability(
		context.Background(), taskAPtrInt64(101), "", "", "gpt-4o", nil,
		OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
		false, false, false, PlatformOpenAI,
	)
	require.NoError(t, err)
	require.NotNil(t, sel)
	require.NotNil(t, sel.Account)
	require.Equal(t, int64(2), sel.Account.ID, "advanced path: Web pool tops up when API pool exhausted")
}

func TestTaskA_AdvancedB2MessagesOnlyAPI(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	ctx := WithOpenAIMessagesDispatchContext(context.Background())
	svc := taskAAdvancedService([]Account{
		*taskAMakeAccount(1, AccountAccessModeWeb, 0),
		*taskAMakeAccount(2, AccountAccessModeAPI, 0),
	})
	sel, _, err := svc.SelectAccountWithSchedulerForCapability(
		ctx, taskAPtrInt64(101), "", "", "gpt-4o", nil,
		OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
		false, false, false, PlatformOpenAI,
	)
	require.NoError(t, err)
	require.NotNil(t, sel)
	require.Equal(t, int64(2), sel.Account.ID, "advanced path: messages must only match API accounts")
}

func TestTaskA_AdvancedStickyNoWeb(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:sess-adv": 2}}
	api := taskAMakeAccount(1, AccountAccessModeAPI, 0)
	web := taskAMakeAccount(2, AccountAccessModeWeb, 0)
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LoadBatchEnabled = false
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: []Account{*api, *web}},
		cache:              cache,
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
	}
	sel, _, err := svc.SelectAccountWithSchedulerForCapability(
		context.Background(), taskAPtrInt64(101), "", "sess-adv", "gpt-4o", nil,
		OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
		false, false, false, PlatformOpenAI,
	)
	require.NoError(t, err)
	require.NotNil(t, sel)
	require.Equal(t, int64(1), sel.Account.ID, "advanced path: sticky must not route to a Web account")
	_, deleted := cache.deletedSessions["openai:sess-adv"]
	require.True(t, deleted, "advanced path: Web sticky binding must be cleared")
}

func taskAPtrInt64(v int64) *int64 { return &v }
