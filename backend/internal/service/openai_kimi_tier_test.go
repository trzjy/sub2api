//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// TestKimiTierClassForEstimate 覆盖分层路由边界语义：估算 input ≤1000 判小
// （含 =1000 边界），>1000 判大（派发单 B2，Done when #1 / #5）。
func TestKimiTierClassForEstimate(t *testing.T) {
	require.Equal(t, KimiTierSmall, KimiTierClassForEstimate(0))
	require.Equal(t, KimiTierSmall, KimiTierClassForEstimate(999))
	require.Equal(t, KimiTierSmall, KimiTierClassForEstimate(1000))
	require.Equal(t, KimiTierBig, KimiTierClassForEstimate(1001))
	require.Equal(t, KimiTierBig, KimiTierClassForEstimate(5000))
}

// TestKimiTierEstimateInputTokens 覆盖既有估算链导出入口：合法 chat completions
// 请求体估算成功返回非负值；非法请求体（无法解析/非标）返回错误——这是 fail-closed
// 的前置：估算失败禁止默认大档（派发单 B2，Done when #2）。
func TestKimiTierEstimateInputTokens(t *testing.T) {
	valid := []byte(`{"model":"kimi-k3","messages":[{"role":"user","content":"hello world"}]}`)
	est, err := EstimateKimiTierInputTokens("kimi-k3", KimiTierProtocolChatCompletions, valid)
	require.NoError(t, err)
	require.GreaterOrEqual(t, est, 0)

	// 非法 JSON：解析失败必须返回错误（调用方 fail-closed）。
	_, err = EstimateKimiTierInputTokens("kimi-k3", KimiTierProtocolChatCompletions, []byte("not-json"))
	require.Error(t, err)

	// 不支持的协议：必须返回错误而非默认大档。
	_, err = EstimateKimiTierInputTokens("kimi-k3", KimiTierRequestProtocol("bogus"), valid)
	require.Error(t, err)
}

// kimiTierTestService 构造一个启用高级调度器的 OpenAIGatewayService，复用既有
// 测试桩（schedulerTestOpenAIAccountRepo / schedulerTestGatewayCache /
// newOpenAIAdvancedSchedulerRateLimitService），使 selectAccountWithSchedulerOnce
// 真正走候选过滤（含 KimiTierPool 约束）与跨档重试路径。
func kimiTierTestService(t *testing.T, accounts []Account) *OpenAIGatewayService {
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

func kimiTierMaskedAccount(id int64) Account {
	return Account{
		ID:          id,
		Platform:    PlatformKimi,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Extra:       map[string]any{"mask_upstream_identity": true},
	}
}

func kimiTierUpstreamAccount(id int64) Account {
	return Account{
		ID:          id,
		Platform:    PlatformKimi,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
	}
}

// TestKimiTierSelectAccountRouting 覆盖分层路由选号主语义与职责隔离（派发单 B2，
// Done when #1 / #3）：
//   - ≤1000（small）→ 仅落 qwen 伪装线池（masked 账号）；
//   - >1000（big）→ 仅落星思池（非 masked 账号）；
//   - qwen 池整体无可用 → 小请求跨档走星思池（告警日志路径在 selectAccountWithScheduler 内）；
//   - 双侧均无可用 → fail-closed 返回 ErrNoAvailableAccounts，无 infer-kimi 隐式落侧。
func TestKimiTierSelectAccountRouting(t *testing.T) {
	groupID := int64(102001)
	model := "kimi-k3"
	ctx := context.Background()

	t.Run("small_only_masked_pool", func(t *testing.T) {
		svc := kimiTierTestService(t, []Account{
			kimiTierMaskedAccount(4001),
			kimiTierUpstreamAccount(4002),
		})
		selCtx := WithKimiTierRouting(ctx, KimiTierSmall)
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
		svc := kimiTierTestService(t, []Account{
			kimiTierMaskedAccount(4011),
			kimiTierUpstreamAccount(4012),
		})
		selCtx := WithKimiTierRouting(ctx, KimiTierBig)
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
		svc := kimiTierTestService(t, []Account{
			kimiTierUpstreamAccount(4022),
		})
		selCtx := WithKimiTierRouting(ctx, KimiTierSmall)
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
		svc := kimiTierTestService(t, nil)
		selCtx := WithKimiTierRouting(ctx, KimiTierSmall)
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
