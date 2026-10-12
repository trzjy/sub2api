//go:build unit

package service

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// TestTierRoutingClassForEstimate 覆盖分层路由边界语义：估算 input ≤1000 判小
// （含 =1000 边界），>1000 判大（派发单 OC-T1，测试 #2 三档中的 =1000 判小）。
func TestTierRoutingClassForEstimate(t *testing.T) {
	require.Equal(t, TierRoutingSmall, TierRoutingClassForEstimate(0))
	require.Equal(t, TierRoutingSmall, TierRoutingClassForEstimate(999))
	require.Equal(t, TierRoutingSmall, TierRoutingClassForEstimate(1000))
	require.Equal(t, TierRoutingBig, TierRoutingClassForEstimate(1001))
	require.Equal(t, TierRoutingBig, TierRoutingClassForEstimate(5000))
}

// TestTierRoutingEstimateInputTokens 覆盖既有估算链导出入口：合法 chat completions
// 请求体估算成功返回非负值；非法请求体（无法解析/非标）返回错误——这是 fail-closed
// 的前置：估算失败禁止默认大档（派发单 OC-T1，测试 #3）。
func TestTierRoutingEstimateInputTokens(t *testing.T) {
	valid := []byte(`{"model":"kimi-k3","messages":[{"role":"user","content":"hello world"}]}`)
	est, err := EstimateTierRoutingInputTokens("kimi-k3", TierRoutingProtocolChatCompletions, valid)
	require.NoError(t, err)
	require.GreaterOrEqual(t, est, 0)

	// 非法 JSON：解析失败必须返回错误（调用方 fail-closed）。
	_, err = EstimateTierRoutingInputTokens("kimi-k3", TierRoutingProtocolChatCompletions, []byte("not-json"))
	require.Error(t, err)

	// 不支持的协议：必须返回错误而非默认大档。
	_, err = EstimateTierRoutingInputTokens("kimi-k3", TierRoutingRequestProtocol("bogus"), valid)
	require.Error(t, err)
}

// tierRoutingTestService 构造一个启用高级调度器的 OpenAIGatewayService，复用既有
// 测试桩（schedulerTestOpenAIAccountRepo / schedulerTestGatewayCache /
// newOpenAIAdvancedSchedulerRateLimitService），使 selectAccountWithSchedulerOnce
// 真正走候选过滤（含 TierRoutingPool 约束）与跨档重试路径。
func tierRoutingTestService(t *testing.T, accounts []Account) *OpenAIGatewayService {
	t.Helper()
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	return &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                &config.Config{},
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}
}

func tierRoutingMaskedAccount(id int64, platform string) Account {
	return Account{
		ID:          id,
		Platform:    platform,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Extra:       map[string]any{"mask_upstream_identity": true},
	}
}

func tierRoutingUpstreamAccount(id int64, platform string) Account {
	return Account{
		ID:          id,
		Platform:    platform,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
	}
}

// tierRoutingMaskedAccountWithModel 构造伪装（mask_upstream_identity=true）账号，其
// model_mapping 含指定目标模型键（动态门控要求「掩码 且 model_mapping 含目标模型」）。
func tierRoutingMaskedAccountWithModel(id int64, platform string, models ...string) Account {
	mapping := make(map[string]any, len(models))
	for _, m := range models {
		mapping[m] = m + "-upstream"
	}
	return Account{
		ID:          id,
		Platform:    platform,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Extra:       map[string]any{"mask_upstream_identity": true},
		Credentials: map[string]any{"model_mapping": mapping},
	}
}

// TestTierRoutingGateOpenForAccounts 覆盖动态门控判定纯函数（派发单 OC-T1，测试 #1 动态门
// 三态 + 运行态不关门）：
//   - ① 无 mask 账号 → 门关；
//   - ② mask 账号管理态 schedulable=false → 门关；
//   - ③ mask 账号管理态 schedulable=true 且 model_mapping 含目标模型 → 门开；
//   - ④ mask 账号 schedulable=true 但运行态不可用（熔断/配额暂停/临时停调）→ 门仍开；
//   - ⑤ mask 账号 model_mapping 不含目标模型键 → 门关；
//   - ⑥ 账号非 active → 门关。
func TestTierRoutingGateOpenForAccounts(t *testing.T) {
	const model = "deepseek-v4.1-flash"
	future := time.Now().Add(time.Hour)

	t.Run("no_mask_account_gate_closed", func(t *testing.T) {
		require.False(t, tierRoutingGateOpenForAccounts([]Account{tierRoutingUpstreamAccount(5001, PlatformKimi)}, model))
		require.False(t, tierRoutingGateOpenForAccounts(nil, model))
	})

	t.Run("mask_schedulable_false_gate_closed", func(t *testing.T) {
		acct := tierRoutingMaskedAccountWithModel(5002, PlatformKimi, model)
		acct.Schedulable = false
		require.False(t, tierRoutingGateOpenForAccounts([]Account{acct}, model))
	})

	t.Run("mask_schedulable_true_gate_open", func(t *testing.T) {
		acct := tierRoutingMaskedAccountWithModel(5003, PlatformKimi, model)
		require.True(t, tierRoutingGateOpenForAccounts([]Account{acct}, model))
	})

	t.Run("mask_runtime_unavailable_gate_still_open", func(t *testing.T) {
		acct := tierRoutingMaskedAccountWithModel(5004, PlatformKimi, model)
		// 运行态不可用标记：熔断（OverloadUntil）、配额暂停（RateLimitResetAt）、临时停调
		// （TempUnschedulableUntil）——均不改管理态 Schedulable，故门仍开。
		acct.OverloadUntil = &future
		acct.RateLimitResetAt = &future
		acct.TempUnschedulableUntil = &future
		acct.TempUnschedulableReason = "circuit_breaker"
		require.True(t, tierRoutingGateOpenForAccounts([]Account{acct}, model),
			"运行态不可用不关门（只看管理态 Schedulable）")
	})

	t.Run("mask_without_model_key_gate_closed", func(t *testing.T) {
		acct := tierRoutingMaskedAccountWithModel(5005, PlatformKimi, "glm-5.3-flash")
		require.False(t, tierRoutingGateOpenForAccounts([]Account{acct}, model),
			"model_mapping 不含目标模型键不装门")
	})

	t.Run("inactive_account_gate_closed", func(t *testing.T) {
		acct := tierRoutingMaskedAccountWithModel(5006, PlatformKimi, model)
		acct.Status = StatusDisabled
		require.False(t, tierRoutingGateOpenForAccounts([]Account{acct}, model))
	})
}

// tierRoutingGateRepoStub 是动态门控查询的仓库桩：仅实现 ListModelAvailabilityCandidates，
// 返回预置账号（模拟管理态查询，忽略运行态瞬态）。
type tierRoutingGateRepoStub struct {
	AccountRepository
	accounts []Account
	err      error
}

func (r *tierRoutingGateRepoStub) ListModelAvailabilityCandidates(_ context.Context, _ *int64, _ []string, _ bool) ([]Account, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.accounts, nil
}

// TestTierRoutingGateOpenQuery 覆盖动态门控的查询入口（派发单 OC-T1，测试 #1）：
// 经既有管理态候选查询取账号后判定；查询自身失败原样上抛（由调用方按关门保守处理）。
func TestTierRoutingGateOpenQuery(t *testing.T) {
	const model = "kimi-k3"
	groupID := int64(7001)
	future := time.Now().Add(time.Hour)

	t.Run("query_opens_on_management_masked_account", func(t *testing.T) {
		acct := tierRoutingMaskedAccountWithModel(5101, PlatformKimi, model)
		acct.RateLimitResetAt = &future // 运行态不可用不影响开门
		svc := &OpenAIGatewayService{accountRepo: &tierRoutingGateRepoStub{accounts: []Account{acct}}}
		open, err := svc.TierRoutingGateOpen(context.Background(), &groupID, PlatformKimi, model)
		require.NoError(t, err)
		require.True(t, open)
	})

	t.Run("query_no_mask_account_gate_closed", func(t *testing.T) {
		// 仅非伪装账号（无 mask 标记）→ 门关（原路径）。
		svc := &OpenAIGatewayService{accountRepo: &tierRoutingGateRepoStub{accounts: []Account{tierRoutingUpstreamAccount(5102, PlatformKimi)}}}
		open, err := svc.TierRoutingGateOpen(context.Background(), &groupID, PlatformKimi, model)
		require.NoError(t, err)
		require.False(t, open)
	})

	t.Run("query_error_propagates", func(t *testing.T) {
		svc := &OpenAIGatewayService{accountRepo: &tierRoutingGateRepoStub{err: errors.New("boom")}}
		open, err := svc.TierRoutingGateOpen(context.Background(), &groupID, PlatformKimi, model)
		require.Error(t, err)
		require.False(t, open)
	})

	t.Run("nil_repo_gate_closed", func(t *testing.T) {
		svc := &OpenAIGatewayService{}
		open, err := svc.TierRoutingGateOpen(context.Background(), &groupID, PlatformKimi, model)
		require.NoError(t, err)
		require.False(t, open)
	})
}

// TestTierRoutingSelectAccountFaultPaths 覆盖分层路由故障路径（派发单 OC-T1，测试 #4）：
//   - 单 mask 账号运行态不可用 → 先耗尽同档候选（仍选同档可用 mask 账号），不跨档；
//   - mask 整池运行态不可用 → 仅 small 跨档到上游池且产生跨档告警事件；
//   - mask 整池运行态不可用 → big 任何重试不进 mask 池（只落上游池、无跨档告警）。
func TestTierRoutingSelectAccountFaultPaths(t *testing.T) {
	groupID := int64(103001)
	model := "kimi-k3"
	ctx := context.Background()
	future := time.Now().Add(time.Hour)

	selectFor := func(t *testing.T, svc *OpenAIGatewayService, class TierRoutingClass) (*AccountSelectionResult, error) {
		t.Helper()
		sel, _, err := svc.SelectAccountWithSchedulerForCapability(
			WithTierRouting(ctx, class), &groupID, "", "", model, nil,
			OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
			false, false, false, false, PlatformKimi,
		)
		return sel, err
	}

	t.Run("single_masked_unavailable_exhausts_same_tier_first", func(t *testing.T) {
		unavailable := tierRoutingMaskedAccount(5201, PlatformKimi)
		unavailable.RateLimitResetAt = &future // 运行态不可用（同档候选之一）
		svc := tierRoutingTestService(t, []Account{
			unavailable,
			tierRoutingMaskedAccount(5202, PlatformKimi), // 同档可用
			tierRoutingUpstreamAccount(5203, PlatformKimi),
		})
		sel, err := selectFor(t, svc, TierRoutingSmall)
		require.NoError(t, err)
		require.NotNil(t, sel)
		require.NotNil(t, sel.Account)
		require.Equal(t, int64(5202), sel.Account.ID, "同档候选耗尽前不得跨档")
	})

	t.Run("masked_pool_exhausted_small_crosses_with_alert", func(t *testing.T) {
		unavailable1 := tierRoutingMaskedAccount(5211, PlatformKimi)
		unavailable1.RateLimitResetAt = &future
		unavailable2 := tierRoutingMaskedAccount(5212, PlatformKimi)
		unavailable2.OverloadUntil = &future
		svc := tierRoutingTestService(t, []Account{
			unavailable1, unavailable2, tierRoutingUpstreamAccount(5213, PlatformKimi),
		})

		var buf syncBuffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
		t.Cleanup(func() { slog.SetDefault(prev) })

		sel, err := selectFor(t, svc, TierRoutingSmall)
		require.NoError(t, err)
		require.NotNil(t, sel)
		require.NotNil(t, sel.Account)
		require.Equal(t, int64(5213), sel.Account.ID, "伪装整池无可用时小档跨档到上游池")
		require.Contains(t, buf.String(), "kimi_tier_cross_pool_failover", "小档跨档必须产生告警事件")
	})

	t.Run("masked_pool_exhausted_big_never_enters_masked", func(t *testing.T) {
		unavailable := tierRoutingMaskedAccount(5221, PlatformKimi)
		unavailable.RateLimitResetAt = &future
		svc := tierRoutingTestService(t, []Account{unavailable, tierRoutingUpstreamAccount(5222, PlatformKimi)})

		var buf syncBuffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
		t.Cleanup(func() { slog.SetDefault(prev) })

		sel, err := selectFor(t, svc, TierRoutingBig)
		require.NoError(t, err)
		require.NotNil(t, sel)
		require.NotNil(t, sel.Account)
		require.Equal(t, int64(5222), sel.Account.ID, "大档只落上游池，任何重试不进伪装池")
		require.NotContains(t, buf.String(), "kimi_tier_cross_pool_failover", "大档不跨档（无跨档告警）")
	})
}

// TestTierRoutingSelectAccountRouting 覆盖分层路由选号主语义与职责隔离（派发单 OC-T1，
// 测试 #2 分档落池 + 测试 #4 故障路径）：
//   - ≤1000（small）→ 仅落 qwen 伪装线池（masked 账号）；
//   - >1000（big）→ 仅落星思池（非 masked 账号）；
//   - qwen 池整体无可用 → 小请求跨档走星思池（告警日志路径在 selectAccountWithScheduler 内）；
//   - 双侧均无可用 → fail-closed 返回 ErrNoAvailableAccounts，无 infer-kimi 隐式落侧。
func TestTierRoutingSelectAccountRouting(t *testing.T) {
	groupID := int64(102001)
	model := "kimi-k3"
	ctx := context.Background()

	t.Run("small_only_masked_pool", func(t *testing.T) {
		svc := tierRoutingTestService(t, []Account{
			tierRoutingMaskedAccount(4001, PlatformKimi),
			tierRoutingUpstreamAccount(4002, PlatformKimi),
		})
		selCtx := WithTierRouting(ctx, TierRoutingSmall)
		selection, _, err := svc.SelectAccountWithSchedulerForCapability(
			selCtx, &groupID, "", "", model, nil,
			OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
			false, false, false, false, PlatformKimi,
		)
		require.NoError(t, err)
		require.NotNil(t, selection)
		require.NotNil(t, selection.Account)
		require.Equal(t, int64(4001), selection.Account.ID, "small request must land on masked (qwen) pool only")
	})

	t.Run("big_only_upstream_pool", func(t *testing.T) {
		svc := tierRoutingTestService(t, []Account{
			tierRoutingMaskedAccount(4011, PlatformKimi),
			tierRoutingUpstreamAccount(4012, PlatformKimi),
		})
		selCtx := WithTierRouting(ctx, TierRoutingBig)
		selection, _, err := svc.SelectAccountWithSchedulerForCapability(
			selCtx, &groupID, "", "", model, nil,
			OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
			false, false, false, false, PlatformKimi,
		)
		require.NoError(t, err)
		require.NotNil(t, selection)
		require.NotNil(t, selection.Account)
		require.Equal(t, int64(4012), selection.Account.ID, "big request must land on upstream (xingsi) pool only")
	})

	t.Run("qwen_pool_empty_cross_to_upstream", func(t *testing.T) {
		// 仅星思池有账号；小请求在 qwen 整池无可用后跨档落星思（职责隔离：换号不出档失败才跨档）。
		svc := tierRoutingTestService(t, []Account{
			tierRoutingUpstreamAccount(4022, PlatformKimi),
		})
		selCtx := WithTierRouting(ctx, TierRoutingSmall)
		selection, _, err := svc.SelectAccountWithSchedulerForCapability(
			selCtx, &groupID, "", "", model, nil,
			OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
			false, false, false, false, PlatformKimi,
		)
		require.NoError(t, err)
		require.NotNil(t, selection)
		require.NotNil(t, selection.Account)
		require.Equal(t, int64(4022), selection.Account.ID, "small request must cross-pool to upstream when qwen pool exhausted")
	})

	t.Run("both_pools_empty_fail_closed", func(t *testing.T) {
		// 双侧整池无可用 → fail-closed 返回可见错误，禁止隐式转 infer-kimi / 第三回退源。
		svc := tierRoutingTestService(t, nil)
		selCtx := WithTierRouting(ctx, TierRoutingSmall)
		selection, _, err := svc.SelectAccountWithSchedulerForCapability(
			selCtx, &groupID, "", "", model, nil,
			OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
			false, false, false, false, PlatformKimi,
		)
		require.Error(t, err)
		require.ErrorIs(t, err, ErrNoAvailableAccounts)
		require.Nil(t, selection)
	})
}

// TestTierRoutingSelectAccountPlatformCombinations 验证分层路由（派发单 OC-T1 泛化）在真实多平台
// 组合下的一致性（FIX-3）：kimi-k3+PlatformKimi / deepseek-v4.1-flash+PlatformDeepseek /
// glm-5.3-flash+PlatformZhipu，各验：
//   - ≤1000（small）→ 仅落伪装线池（masked 账号）；
//   - >1000（big）→ 仅落非伪装池（upstream 账号）；
//   - 伪装线整池无可用 → 小请求跨档落星思池。
func TestTierRoutingSelectAccountPlatformCombinations(t *testing.T) {
	groupID := int64(104001)
	combos := []struct {
		model    string
		platform string
	}{
		{"kimi-k3", PlatformKimi},
		{"deepseek-v4.1-flash", PlatformDeepseek},
		{"glm-5.3-flash", PlatformZhipu},
	}
	for _, combo := range combos {
		combo := combo
		t.Run(combo.model+"_small_only_masked_pool", func(t *testing.T) {
			svc := tierRoutingTestService(t, []Account{
				tierRoutingMaskedAccount(7001, combo.platform),
				tierRoutingUpstreamAccount(7002, combo.platform),
			})
			sel, _, err := svc.SelectAccountWithSchedulerForCapability(
				WithTierRouting(context.Background(), TierRoutingSmall), &groupID, "", "", combo.model, nil,
				OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
				false, false, false, false, combo.platform,
			)
			require.NoError(t, err)
			require.NotNil(t, sel)
			require.NotNil(t, sel.Account)
			require.Equal(t, int64(7001), sel.Account.ID, "small request must land on masked pool only")
		})
		t.Run(combo.model+"_big_only_upstream_pool", func(t *testing.T) {
			svc := tierRoutingTestService(t, []Account{
				tierRoutingMaskedAccount(7011, combo.platform),
				tierRoutingUpstreamAccount(7012, combo.platform),
			})
			sel, _, err := svc.SelectAccountWithSchedulerForCapability(
				WithTierRouting(context.Background(), TierRoutingBig), &groupID, "", "", combo.model, nil,
				OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
				false, false, false, false, combo.platform,
			)
			require.NoError(t, err)
			require.NotNil(t, sel)
			require.NotNil(t, sel.Account)
			require.Equal(t, int64(7012), sel.Account.ID, "big request must land on upstream pool only")
		})
		t.Run(combo.model+"_small_masked_pool_exhausted_cross_to_upstream", func(t *testing.T) {
			svc := tierRoutingTestService(t, []Account{
				tierRoutingUpstreamAccount(7022, combo.platform),
			})
			sel, _, err := svc.SelectAccountWithSchedulerForCapability(
				WithTierRouting(context.Background(), TierRoutingSmall), &groupID, "", "", combo.model, nil,
				OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
				false, false, false, false, combo.platform,
			)
			require.NoError(t, err)
			require.NotNil(t, sel)
			require.NotNil(t, sel.Account)
			require.Equal(t, int64(7022), sel.Account.ID, "small request must cross-pool to upstream when masked pool exhausted")
		})
	}
}

// TestTierRoutingGateOpenViaSchedulerSnapshot 覆盖 FIX-A：门控查询优先走调度快照的
// ListSchedulableAccounts（与选号路径同源，snapshot 已带 mask/mapping，hasForcePlatform
// 取选号同款 false）；snapshot 为 nil 时回退到 repo 查询 ListModelAvailabilityCandidates
// （与选号器 nil-snapshot 回退同构）。纯函数判定语义在两路径下保持一致、不回归。
func TestTierRoutingGateOpenViaSchedulerSnapshot(t *testing.T) {
	const model = "kimi-k3"
	groupID := int64(7101)

	t.Run("snapshot_open_on_management_masked_account", func(t *testing.T) {
		acct := tierRoutingMaskedAccountWithModel(7101, PlatformKimi, model)
		future := time.Now().Add(time.Hour)
		acct.RateLimitResetAt = &future // 运行态不可用不影响开门
		svc := &OpenAIGatewayService{
			schedulerSnapshot: &SchedulerSnapshotService{cache: &openAISnapshotCacheStub{snapshotAccounts: []*Account{&acct}}},
		}
		open, err := svc.TierRoutingGateOpen(context.Background(), &groupID, PlatformKimi, model)
		require.NoError(t, err)
		require.True(t, open, "snapshot 注入路径应经 ListSchedulableAccounts 命中 mask+mapping 账号而开门")
	})

	t.Run("snapshot_closed_without_mapping_key", func(t *testing.T) {
		// model_mapping 仅含 glm-5.3-flash，不含目标键 kimi-k3 → 不装门。
		acct := tierRoutingMaskedAccountWithModel(7102, PlatformKimi, "glm-5.3-flash")
		svc := &OpenAIGatewayService{
			schedulerSnapshot: &SchedulerSnapshotService{cache: &openAISnapshotCacheStub{snapshotAccounts: []*Account{&acct}}},
		}
		open, err := svc.TierRoutingGateOpen(context.Background(), &groupID, PlatformKimi, model)
		require.NoError(t, err)
		require.False(t, open, "snapshot 账号 model_mapping 不含目标键不装门")
	})

	t.Run("snapshot_nil_falls_back_to_repo", func(t *testing.T) {
		acct := tierRoutingMaskedAccountWithModel(7103, PlatformKimi, model)
		svc := &OpenAIGatewayService{
			accountRepo: &tierRoutingGateRepoStub{accounts: []Account{acct}},
		}
		open, err := svc.TierRoutingGateOpen(context.Background(), &groupID, PlatformKimi, model)
		require.NoError(t, err)
		require.True(t, open, "nil-snapshot 回退 repo 路径不回归（与选号器同构）")
	})
}
