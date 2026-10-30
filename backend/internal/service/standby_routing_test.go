//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// 主力渠道备用切换第 2b 片（L14 档位豁免 + L15 置顶）的定向用例。
//
// 生产形状（2026-10-30 只读实测）：codebuddy 备用账号 extra.mask_upstream_identity=true，
// credentials.model_mapping 含客户端模型键（deepseek-v4.1-flash-1017 / kimi-k3），
// 因此大档请求会先在 openAIAccountEligibleForTierPool / TierRoutingPool 过滤处被判
// tier_pool_mismatch，走不到置顶。L14/L14' 只豁免这一处，其余准入条件不变；
// L15 只把「codebuddy ∩ model_mapping 命中 ∩ standby open」的账号前置。

const standbyTestModel = "deepseek-v4.1-flash-1017"

// standbyTestAccount 构造生产形状的 codebuddy 备用账号：mask_upstream_identity=true，
// model_mapping 含 standbyTestModel → 上游 hy3，并带目录快照（codebuddy 平台走逐模型
// 目录闸门）。masked=false 时去掉伪装标记（真实线）。
func standbyTestAccount(id int64, priority int, masked bool) Account {
	account := Account{
		ID:          id,
		Platform:    PlatformCodeBuddy,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 2,
		Priority:    priority,
		Credentials: map[string]any{
			"model_mapping": map[string]any{standbyTestModel: "hy3"},
		},
		Extra: map[string]any{},
	}
	account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{
		Models: map[string]UpstreamModelMetadata{"hy3": {ID: "hy3"}},
	})
	if masked {
		account.Extra["mask_upstream_identity"] = true
	}
	return account
}

// standbyTestPassthroughAccount 是 codebuddy 平台上"未被置顶"的对照候选：无 model_mapping
// （legacy 准入按空映射放行），因此不属于 standby 集合，排序只按原 priority 规则。
func standbyTestPassthroughAccount(id int64, priority int) Account {
	return Account{
		ID:          id,
		Platform:    PlatformCodeBuddy,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 2,
		Priority:    priority,
		Extra:       map[string]any{},
	}
}

// standbyTestWildcardAccount 构造 codebuddy 平台上"未进入备用集合"的可选对照候选：通配
// 映射（*→hy3）让它通过逐模型目录闸门，但 model_mapping 不含该模型字面键，故不被置顶。
func standbyTestWildcardAccount(id int64, priority int) Account {
	account := Account{
		ID:          id,
		Platform:    PlatformCodeBuddy,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 2,
		Priority:    priority,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"*": "hy3"},
		},
		Extra: map[string]any{},
	}
	account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{
		Models: map[string]UpstreamModelMetadata{"hy3": {ID: "hy3"}},
	})
	return account
}

// standbyTestForeignAccount 构造非 codebuddy 候选（deepseek / kimi 主力账号）。
func standbyTestForeignAccount(id int64, platform string, priority int, masked bool) Account {
	account := Account{
		ID:          id,
		Platform:    platform,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 2,
		Priority:    priority,
		Credentials: map[string]any{
			"model_mapping": map[string]any{standbyTestModel: "hy3"},
		},
		Extra: map[string]any{},
	}
	if masked {
		account.Extra["mask_upstream_identity"] = true
	}
	return account
}

// installStandbyRegistry 安装一个只含指定 open 模型的进程级注册表，并在用例结束后还原，
// 避免污染其他用例（读取入口是进程级单例）。
func installStandbyRegistry(t *testing.T, openModels ...string) *StandbyRegistry {
	t.Helper()
	prev := DefaultStandbyRegistry()
	registry := NewStandbyRegistry()
	for _, model := range openModels {
		registry.set(model, StandbyStatus{
			Model:    model,
			Platform: PlatformDeepseek,
			Open:     true,
			Verdict:  StandbyVerdictDegraded,
			Reason:   "ttft_p50_above_enter",
		})
	}
	setDefaultStandbyRegistry(registry)
	t.Cleanup(func() { setDefaultStandbyRegistry(prev) })
	return registry
}

// TestStandbyRegistryDefaultEntry 覆盖进程级单例与执行层判定入口：
// 默认 closed（重启即 closed 的安全默认）；只有 codebuddy 且 model_mapping 命中且该模型
// open 三条件同时成立才进入备用集合。
func TestStandbyRegistryDefaultEntry(t *testing.T) {
	require.NotNil(t, DefaultStandbyRegistry(), "初始化为空表且永不为 nil")
	require.False(t, standbyRoutingOpen(standbyTestModel), "空表 = 全部 closed")

	masked := standbyTestAccount(1, 100, true)
	realLine := standbyTestAccount(2, 100, false)
	noMapping := standbyTestAccount(3, 100, true)
	noMapping.Credentials = map[string]any{}
	nonCodeBuddy := standbyTestAccount(4, 100, true)
	nonCodeBuddy.Platform = PlatformKimi

	require.False(t, standbyRoutingAccountPromoted(&masked, standbyTestModel), "closed 态不属于备用集合")

	installStandbyRegistry(t, standbyTestModel)
	require.True(t, standbyRoutingOpen(standbyTestModel))
	require.True(t, standbyRoutingAccountPromoted(&masked, standbyTestModel))
	require.True(t, standbyRoutingAccountPromoted(&realLine, standbyTestModel), "真实线同属备用集合")
	require.False(t, standbyRoutingAccountPromoted(&noMapping, standbyTestModel), "model_mapping 未命中不进入集合")
	require.False(t, standbyRoutingAccountPromoted(&nonCodeBuddy, standbyTestModel), "非 codebuddy 不进入集合")
	require.False(t, standbyRoutingAccountPromoted(nil, standbyTestModel))
	require.False(t, standbyRoutingAccountPromoted(&masked, ""))
	require.False(t, standbyRoutingAccountPromoted(&masked, "kimi-k3"), "其他模型的 open 不外溢")
}

// TestStandbyTierPoolExemptionGateway 覆盖 L14（gateway 准入）：
//   - open + 大档：codebuddy 备用账号不再 tier_pool_mismatch，其余准入条件不变；
//   - open + 小档：masked 池成员不变（伪装号仍准入、非伪装号仍排除）；
//   - 非 codebuddy / 未命中 mapping 的账号在大档仍被剔除；
//   - closed：与现状逐条一致（回归护栏）。
func TestStandbyTierPoolExemptionGateway(t *testing.T) {
	groupID := int64(12001)
	group := &Group{ID: groupID, Platform: PlatformCodeBuddy}
	baseCtx := context.WithValue(context.Background(), openAIGroupPrivacyRequirementContextKey{},
		openAIGroupPrivacyRequirement{groupID: groupID, group: group})

	masked := standbyTestAccount(12101, 1, true)
	realLine := standbyTestAccount(12102, 1, false)
	noMapping := standbyTestAccount(12103, 1, true)
	noMapping.Credentials = map[string]any{}
	foreign := standbyTestAccount(12104, 1, true)
	foreign.Platform = PlatformKimi

	reason := func(ctx context.Context, account *Account) string {
		return openAICompatibleAccountEligibilityFailureReasonBeforeProfit(
			ctx, account, PlatformCodeBuddy, standbyTestModel, false, OpenAIEndpointCapabilityChatCompletions)
	}

	t.Run("closed_matches_current_behavior", func(t *testing.T) {
		installStandbyRegistry(t)
		require.Equal(t, "tier_pool_mismatch", reason(WithTierRouting(baseCtx, TierRoutingBig), &masked))
		require.Empty(t, reason(WithTierRouting(baseCtx, TierRoutingSmall), &masked))
		require.Equal(t, "tier_pool_mismatch", reason(WithTierRouting(baseCtx, TierRoutingSmall), &realLine))
		require.Empty(t, reason(WithTierRouting(baseCtx, TierRoutingBig), &realLine))
		require.Equal(t, "tier_pool_mismatch", reason(WithTierRouting(baseCtx, TierRoutingBig), &foreign))
	})

	t.Run("open_big_pool_admits_codebuddy_standby", func(t *testing.T) {
		installStandbyRegistry(t, standbyTestModel)
		require.Empty(t, reason(WithTierRouting(baseCtx, TierRoutingBig), &masked),
			"L14：备用开启后 codebuddy 备用账号在大档不再被档位门剔除")
		require.Empty(t, reason(WithTierRouting(baseCtx, TierRoutingBig), &realLine))
	})

	t.Run("open_small_pool_unchanged", func(t *testing.T) {
		installStandbyRegistry(t, standbyTestModel)
		require.Empty(t, reason(WithTierRouting(baseCtx, TierRoutingSmall), &masked), "伪装号在小档仍准入")
		require.Equal(t, "tier_pool_mismatch", reason(WithTierRouting(baseCtx, TierRoutingSmall), &realLine),
			"L14 不得把非伪装号抬进 masked 小档池")
	})

	t.Run("open_does_not_exempt_foreign_or_unmapped", func(t *testing.T) {
		installStandbyRegistry(t, standbyTestModel)
		require.Equal(t, "tier_pool_mismatch", reason(WithTierRouting(baseCtx, TierRoutingBig), &foreign),
			"非 codebuddy 账号的档位判定不变")
		require.Equal(t, "tier_pool_mismatch", reason(WithTierRouting(baseCtx, TierRoutingBig), &noMapping),
			"model_mapping 未命中的 codebuddy 账号不享受豁免")
	})

	t.Run("open_other_model_stays_closed", func(t *testing.T) {
		installStandbyRegistry(t, "kimi-k3")
		require.Equal(t, "tier_pool_mismatch", reason(WithTierRouting(baseCtx, TierRoutingBig), &masked),
			"只有本模型 open 才豁免")
	})
}

// TestStandbyTierPoolExemptionAdvancedScheduler 覆盖 L14'（高级调度候选过滤）：
// 同一豁免语义，且 masked 小档池成员不变。
func TestStandbyTierPoolExemptionAdvancedScheduler(t *testing.T) {
	scheduler := &defaultOpenAIAccountScheduler{}
	masked := standbyTestAccount(12201, 1, true)
	realLine := standbyTestAccount(12202, 1, false)

	upstreamReq := OpenAIAccountScheduleRequest{
		Platform:        PlatformCodeBuddy,
		RequestedModel:  standbyTestModel,
		TierRoutingPool: tierRoutingPoolUpstream,
	}
	maskedReq := upstreamReq
	maskedReq.TierRoutingPool = tierRoutingPoolMasked

	t.Run("closed_rejected_by_tier_pool", func(t *testing.T) {
		installStandbyRegistry(t)
		ok, reason := scheduler.isAccountRequestCompatibleReason(context.Background(), &masked, upstreamReq)
		require.False(t, ok)
		require.Equal(t, "kimi_tier_pool_excluded", reason)
	})

	t.Run("open_big_pool_admits_codebuddy_standby", func(t *testing.T) {
		installStandbyRegistry(t, standbyTestModel)
		ok, reason := scheduler.isAccountRequestCompatibleReason(context.Background(), &masked, upstreamReq)
		require.True(t, ok, "L14'：备用开启后大档不再 kimi_tier_pool_excluded，reason=%s", reason)
		require.Empty(t, reason)
	})

	t.Run("open_small_pool_unchanged", func(t *testing.T) {
		installStandbyRegistry(t, standbyTestModel)
		ok, _ := scheduler.isAccountRequestCompatibleReason(context.Background(), &masked, maskedReq)
		require.True(t, ok, "伪装号在小档仍准入")
		ok, reason := scheduler.isAccountRequestCompatibleReason(context.Background(), &realLine, maskedReq)
		require.False(t, ok, "L14' 不得把非伪装号抬进 masked 小档池")
		require.Equal(t, "kimi_tier_pool_excluded", reason)
	})
}

// standbyAggregateRepo 模拟生产聚合快照的分桶：分组桶里同时含主力平台账号与已绑定的
// codebuddy 账号（生产由快照 join 完成，本地桩需要显式返回全部候选）。
type standbyAggregateRepo struct {
	schedulerTestOpenAIAccountRepo
}

func (r standbyAggregateRepo) ListSchedulableByGroupIDAndPlatform(ctx context.Context, _ int64, _ string) ([]Account, error) {
	return append([]Account(nil), r.accounts...), nil
}

// standbyAdvancedService 构造启用高级调度器的网关服务（与 tierRoutingTestService 同构，
// 仅仓储桩换成聚合桶形态）。
func standbyAdvancedService(t *testing.T, accounts []Account) *OpenAIGatewayService {
	t.Helper()
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	return &OpenAIGatewayService{
		accountRepo:        standbyAggregateRepo{schedulerTestOpenAIAccountRepo{accounts: accounts}},
		cache:              &schedulerTestGatewayCache{},
		cfg:                &config.Config{},
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}
}

// standbyLegacySelectCtx 走 legacy 负载感知选号（selectAccountWithLoadAwareness），
// 返回选号结果与错误（用于断言 closed 态的档位门拒绝）。
func standbyLegacySelectCtx(t *testing.T, ctx context.Context, platform string, accounts ...Account) (*AccountSelectionResult, error) {
	t.Helper()
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}
	groupID := int64(12301)
	return svc.selectAccountWithLoadAwareness(ctx, &groupID, platform, "",
		standbyTestModel, nil, false, OpenAIEndpointCapabilityChatCompletions, false)
}

func standbyLegacySelect(t *testing.T, platform string, accounts ...Account) *AccountSelectionResult {
	t.Helper()
	selection, err := standbyLegacySelectCtx(t, context.Background(), platform, accounts...)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	return selection
}

// TestStandbyLegacyOrderingPromotesStandby 覆盖 L15 legacy 排序闭包：
// open 时备用账号（priority=100）压过 priority=1 的非备用候选；closed 时回落原 priority
// 排序；非 codebuddy 候选在 open 时排序不变；置顶不改写 Priority 字段。
func TestStandbyLegacyOrderingPromotesStandby(t *testing.T) {
	standby := standbyTestAccount(12401, 100, true)
	mainLine := standbyTestPassthroughAccount(12402, 1)

	t.Run("open_standby_first", func(t *testing.T) {
		installStandbyRegistry(t, standbyTestModel)
		selection := standbyLegacySelect(t, PlatformCodeBuddy, standby, mainLine)
		require.Equal(t, standby.ID, selection.Account.ID, "L15：备用账号置于候选首位（先于 priority 比较）")
		require.Equal(t, 100, selection.Account.Priority, "置顶不改写 Priority 字段")
	})

	t.Run("closed_priority_order_unchanged", func(t *testing.T) {
		installStandbyRegistry(t)
		selection := standbyLegacySelect(t, PlatformCodeBuddy, standby, mainLine)
		require.Equal(t, mainLine.ID, selection.Account.ID, "closed：等价于现状（priority 升序）")
		require.Equal(t, 1, selection.Account.Priority)
	})

	t.Run("open_non_codebuddy_order_unchanged", func(t *testing.T) {
		installStandbyRegistry(t, standbyTestModel)
		mainA := standbyTestForeignAccount(12403, PlatformKimi, 1, true)
		mainB := standbyTestForeignAccount(12404, PlatformKimi, 2, true)
		selection := standbyLegacySelect(t, PlatformKimi, mainB, mainA)
		require.Equal(t, mainA.ID, selection.Account.ID, "非 codebuddy 候选排序不受 standby 影响")
	})
}

// TestStandbyTierPoolExemptionSelectsStandby 覆盖 L14/L14' 端到端：同一账号在 closed 态
// 被档位门剔除（tier_pool_mismatch / kimi_tier_pool_excluded），open 态可被选中。平台取
// codebuddy 自身，以隔离跨平台准入（该准入不属于本片范围）。
func TestStandbyTierPoolExemptionSelectsStandby(t *testing.T) {
	standby := standbyTestAccount(12701, 100, true)
	bigCtx := WithTierRouting(context.Background(), TierRoutingBig)
	groupID := int64(12701)

	t.Run("legacy", func(t *testing.T) {
		installStandbyRegistry(t)
		selection, err := standbyLegacySelectCtx(t, bigCtx, PlatformCodeBuddy, standby)
		require.Error(t, err, "closed：大档把 codebuddy 备用账号判 tier_pool_mismatch")
		require.Nil(t, selection)
		require.Contains(t, err.Error(), "tier_pool_mismatch")

		installStandbyRegistry(t, standbyTestModel)
		selection, err = standbyLegacySelectCtx(t, bigCtx, PlatformCodeBuddy, standby)
		require.NoError(t, err)
		require.NotNil(t, selection)
		require.Equal(t, standby.ID, selection.Account.ID, "open：L14 放行后该账号可被选中")
	})

	t.Run("advanced", func(t *testing.T) {
		installStandbyRegistry(t)
		svc := standbyAdvancedService(t, []Account{standby})
		selection, _, err := svc.SelectAccountWithSchedulerForCapability(
			bigCtx, &groupID, "", "", standbyTestModel, nil,
			OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
			false, false, false, false, PlatformCodeBuddy,
		)
		require.Error(t, err, "closed：大档把 codebuddy 备用账号判 kimi_tier_pool_excluded")
		require.Nil(t, selection)

		installStandbyRegistry(t, standbyTestModel)
		svc = standbyAdvancedService(t, []Account{standby})
		selection, _, err = svc.SelectAccountWithSchedulerForCapability(
			bigCtx, &groupID, "", "", standbyTestModel, nil,
			OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
			false, false, false, false, PlatformCodeBuddy,
		)
		require.NoError(t, err)
		require.NotNil(t, selection)
		require.Equal(t, standby.ID, selection.Account.ID, "open：L14' 放行后该账号可被选中")
	})
}

// TestStandbyAdvancedOrderingPromotesStandby 覆盖 L15 高级评分路径的固定顶分：
// open 时备用账号（priority=100）压过 priority=1 的非备用候选；closed 时评分与排序逐字段
// 等于现状（无顶分、按分值降序）；置顶不改写 Priority 字段。
func TestStandbyAdvancedOrderingPromotesStandby(t *testing.T) {
	standby := standbyTestAccount(12501, 100, true)
	mainLine := standbyTestWildcardAccount(12502, 1)
	groupID := int64(12501)

	selectAccount := func(t *testing.T, accounts []Account) *AccountSelectionResult {
		t.Helper()
		svc := standbyAdvancedService(t, accounts)
		selection, _, err := svc.SelectAccountWithSchedulerForCapability(
			context.Background(), &groupID, "", "", standbyTestModel, nil,
			OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
			false, false, false, false, PlatformCodeBuddy,
		)
		require.NoError(t, err)
		require.NotNil(t, selection)
		require.NotNil(t, selection.Account)
		return selection
	}

	t.Run("open_standby_first", func(t *testing.T) {
		installStandbyRegistry(t, standbyTestModel)
		selection := selectAccount(t, []Account{mainLine, standby})
		require.Equal(t, standby.ID, selection.Account.ID, "L15：固定顶分让备用账号排在首选位")
		require.Equal(t, 100, selection.Account.Priority, "置顶不改写 Priority 字段")
	})

	t.Run("closed_plan_fields_unchanged", func(t *testing.T) {
		// closed 的评分与排序必须与其他模型 open（与本请求无关）时逐字段一致：即备用集合
		// 为空时既不注入顶分，也不改变任何候选的顺序。
		buildPlan := func(t *testing.T) openAIAccountLoadPlan {
			t.Helper()
			svc := standbyAdvancedService(t, []Account{mainLine, standby})
			scheduler := &defaultOpenAIAccountScheduler{service: svc}
			filtered := []*Account{&mainLine, &standby}
			loadMap := map[int64]*AccountLoadInfo{
				mainLine.ID: {AccountID: mainLine.ID},
				standby.ID:  {AccountID: standby.ID},
			}
			return scheduler.buildOpenAIAccountLoadPlan(context.Background(), OpenAIAccountScheduleRequest{
				Platform:           PlatformCodeBuddy,
				RequestedModel:     standbyTestModel,
				RequiredCapability: OpenAIEndpointCapabilityChatCompletions,
			}, filtered, loadMap)
		}

		installStandbyRegistry(t)
		closedPlan := buildPlan(t)
		installStandbyRegistry(t, "kimi-k3")
		unrelatedOpenPlan := buildPlan(t)

		// 高级路径的候选序列按分值加权随机（同分不同序），故逐字段比对以账号为键：
		// 每个候选的分值、优先级字段必须与"无关模型 open"时完全一致，且都不含顶分。
		require.Len(t, closedPlan.selectionOrder, 2, "closed：候选集合与现状一致")
		require.Len(t, unrelatedOpenPlan.selectionOrder, len(closedPlan.selectionOrder))
		scoreByID := func(plan openAIAccountLoadPlan) map[int64]float64 {
			out := make(map[int64]float64, len(plan.selectionOrder))
			for _, item := range plan.selectionOrder {
				out[item.account.ID] = item.score
				require.Less(t, item.score, openAISchedulingStandbyPromotedBonus,
					"closed：候选不得带 standby 顶分")
				require.Equal(t, item.account.Priority, item.priority,
					"closed：评分优先级字段等于账号 Priority")
			}
			return out
		}
		require.Equal(t, scoreByID(unrelatedOpenPlan), scoreByID(closedPlan), "closed：候选评分逐字段不变")
	})

	t.Run("open_non_codebuddy_order_unchanged", func(t *testing.T) {
		// 非 codebuddy 候选不进入备用集合：plan 中不带顶分，且 open/closed 两次评分逐字段相同。
		buildPlan := func(t *testing.T) openAIAccountLoadPlan {
			t.Helper()
			accounts := []Account{
				standbyTestForeignAccount(12503, PlatformKimi, 2, true),
				standbyTestForeignAccount(12504, PlatformKimi, 1, true),
			}
			svc := standbyAdvancedService(t, accounts)
			scheduler := &defaultOpenAIAccountScheduler{service: svc}
			filtered := []*Account{&accounts[0], &accounts[1]}
			loadMap := map[int64]*AccountLoadInfo{
				accounts[0].ID: {AccountID: accounts[0].ID},
				accounts[1].ID: {AccountID: accounts[1].ID},
			}
			return scheduler.buildOpenAIAccountLoadPlan(context.Background(), OpenAIAccountScheduleRequest{
				Platform:           PlatformKimi,
				RequestedModel:     standbyTestModel,
				RequiredCapability: OpenAIEndpointCapabilityChatCompletions,
			}, filtered, loadMap)
		}
		installStandbyRegistry(t)
		closedPlan := buildPlan(t)
		installStandbyRegistry(t, standbyTestModel)
		openPlan := buildPlan(t)

		require.Len(t, openPlan.selectionOrder, len(closedPlan.selectionOrder))
		scoreByID := func(plan openAIAccountLoadPlan) map[int64]float64 {
			out := make(map[int64]float64, len(plan.selectionOrder))
			for _, item := range plan.selectionOrder {
				out[item.account.ID] = item.score
			}
			return out
		}
		require.Equal(t, scoreByID(closedPlan), scoreByID(openPlan), "非 codebuddy 候选评分不因 standby 改变")
	})
}
