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

// TestTierRoutingClassForEstimate 覆盖分层路由边界语义：估算 input ≤10000 判小
// （含 =10000 边界），>10000 判大。
func TestTierRoutingClassForEstimate(t *testing.T) {
	require.Equal(t, TierRoutingSmall, TierRoutingClassForEstimate(0))
	require.Equal(t, TierRoutingSmall, TierRoutingClassForEstimate(9999))
	require.Equal(t, TierRoutingSmall, TierRoutingClassForEstimate(10000))
	require.Equal(t, TierRoutingBig, TierRoutingClassForEstimate(10001))
	require.Equal(t, TierRoutingBig, TierRoutingClassForEstimate(150000))
	require.Equal(t, TierRoutingBig, TierRoutingClassForEstimate(150001))
}

// TestTierRoutingThresholdCoversFormerSmallBand 锁死阈值沿革：2026-11-04 用户裁定把
// 阈值从 3000 提到 10000，让伪装线小档把 1 万以下的体量吃满（之前 3001~10000 这段
// 全部直落星思池，伪装线没发挥潜力）。
func TestTierRoutingThresholdCoversFormerSmallBand(t *testing.T) {
	require.Equal(t, 10000, TierRoutingInputThreshold, "阈值必须为 1 万")
	for _, est := range []int{1001, 3000, 3001, 8000, 10000} {
		require.Equal(t, TierRoutingSmall, TierRoutingClassForEstimate(est),
			"估算 %d 应判小档，交给伪装线小档池", est)
	}
}

// TestMaskedInputBandOf 覆盖 extra.masked_input_band 的解析容错：
//   - 未声明 / 非对象 / 缺 min+max / max<=0 / min>max → ok=false（按历史语义只参与小档）；
//   - {"min":a,"max":b} → 闭区间，两端都命中；
//   - 只给 min 或只给 max → 缺失侧取默认（0 / 无上限）。
func TestMaskedInputBandOf(t *testing.T) {
	bandAccount := func(raw any) *Account {
		return &Account{ID: 1, Extra: map[string]any{maskedInputBandKey: raw}}
	}

	_, ok := MaskedInputBandOf(&Account{ID: 1})
	require.False(t, ok, "未声明区间 → 不参与大档")
	_, ok = MaskedInputBandOf(bandAccount(map[string]any{}))
	require.False(t, ok, "空对象 → 视为未声明")
	_, ok = MaskedInputBandOf(bandAccount(map[string]any{"foo": 1}))
	require.False(t, ok, "缺 min/max → 视为未声明")
	_, ok = MaskedInputBandOf(bandAccount(map[string]any{"min": 10000, "max": 0}))
	require.False(t, ok, "max<=0 → 视为未声明")
	_, ok = MaskedInputBandOf(bandAccount(map[string]any{"min": 20000, "max": 10000}))
	require.False(t, ok, "min>max → 视为未声明")

	band, ok := MaskedInputBandOf(bandAccount(map[string]any{"min": float64(10000), "max": float64(150000)}))
	require.True(t, ok)
	require.Equal(t, 10000, band.Min)
	require.Equal(t, 150000, band.Max)
	require.False(t, band.Contains(9999), "下界外")
	require.True(t, band.Contains(10000), "下界含")
	require.True(t, band.Contains(150000), "上界含")
	require.False(t, band.Contains(150001), "上界外")

	minOnly, ok := MaskedInputBandOf(bandAccount(map[string]any{"min": 10000}))
	require.True(t, ok)
	require.Equal(t, 0+10000, minOnly.Min)
	require.True(t, minOnly.Contains(900000), "只声明 min 时无上限")

	maxOnly, ok := MaskedInputBandOf(bandAccount(map[string]any{"max": 8000}))
	require.True(t, ok)
	require.Equal(t, 0, maxOnly.Min)
	require.True(t, maxOnly.Contains(0))
	require.False(t, maxOnly.Contains(8001))

	// 导入路径可能带 JSON 字符串形态。
	fromString, ok := MaskedInputBandOf(bandAccount(`{"min":10000,"max":150000}`))
	require.True(t, ok, "JSON 字符串形态也应解析")
	require.Equal(t, 150000, fromString.Max)
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

// 测试用估算 input：分别落在小档（≤1 万）与大档（1 万~15 万）区间内。
const (
	tierRoutingSmallEstimate = 5000
	tierRoutingBigEstimate   = 50000
)

// tierRoutingTestEstimateFor 按档位返回一个落在该档内的估算 input。
func tierRoutingTestEstimateFor(class TierRoutingClass) int {
	if class == TierRoutingBig {
		return tierRoutingBigEstimate
	}
	return tierRoutingSmallEstimate
}

// tierRoutingMaskedAccount 构造伪装（mask_upstream_identity=true）账号，未声明
// masked_input_band——按历史语义只参与伪装线小档池。
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

// tierRoutingBandAccount 构造伪装线大档账号：mask_upstream_identity=true 且声明
// extra.masked_input_band=[minInput, maxInput]，其 model_mapping 含目标模型键。
func tierRoutingBandAccount(id int64, platform string, minInput, maxInput int) Account {
	a := tierRoutingMaskedAccountWithModel(id, platform, "kimi-k3", "kimi-k3.1")
	a.Extra[maskedInputBandKey] = map[string]any{"min": minInput, "max": maxInput}
	return a
}

// TestTierRoutingMaskedBandSplit 覆盖「按估算 input 把伪装线拆成小档池 / 大档池」：
//   - 小档（≤1 万）只收未声明区间的伪装号（qwen 伪装线）；声明了区间的大档专用号
//     （kiro 中转）不得抢小活，1 万以下由不花钱的 qwen 承接；
//   - 大档（1 万~15 万）只收声明了区间且估算命中区间的伪装号；
//   - 估算超过全局上限 15 万 → 直接回星思池，不进任何伪装号（kiro 上下文 20 万，超了静默截断）；
//   - 区间不命中（估算 12 万但账号上限只到 10 万）→ 也落星思池。
func TestTierRoutingMaskedBandSplit(t *testing.T) {
	groupID := int64(105001)
	model := "kimi-k3"

	selectWithEstimate := func(t *testing.T, svc *OpenAIGatewayService, estimated int) *AccountSelectionResult {
		t.Helper()
		class := TierRoutingClassForEstimate(estimated)
		sel, _, err := svc.SelectAccountWithSchedulerForCapability(
			WithTierRouting(context.Background(), class, estimated), &groupID, "", "", model, nil,
			OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
			false, false, false, false, PlatformKimi,
		)
		require.NoError(t, err)
		require.NotNil(t, sel)
		require.NotNil(t, sel.Account)
		return sel
	}

	newService := func(t *testing.T) *OpenAIGatewayService {
		return tierRoutingTestService(t, []Account{
			tierRoutingMaskedAccountWithModel(8101, PlatformKimi, model), // qwen 伪装线（小档专用）
			tierRoutingBandAccount(8102, PlatformKimi, 10000, 150000),    // kiro 中转（大档专用）
			tierRoutingUpstreamAccount(8103, PlatformKimi),               // 星思
		})
	}

	t.Run("small_lands_on_qwen_not_kiro", func(t *testing.T) {
		sel := selectWithEstimate(t, newService(t), 5000)
		require.Equal(t, int64(8101), sel.Account.ID, "小活必须落 qwen 伪装线，不能烧 kiro 积分")
	})

	t.Run("small_band_account_excluded_from_small_pool", func(t *testing.T) {
		// 只剩 qwen 伪装线不可用 + kiro 号可用：小活也不许落 kiro。
		qwen := tierRoutingMaskedAccountWithModel(8111, PlatformKimi, model)
		future := time.Now().Add(time.Hour)
		qwen.RateLimitResetAt = &future
		svc := tierRoutingTestService(t, []Account{
			qwen,
			tierRoutingBandAccount(8112, PlatformKimi, 10000, 150000),
			tierRoutingUpstreamAccount(8113, PlatformKimi),
		})
		sel := selectWithEstimate(t, svc, 5000)
		require.Equal(t, int64(8113), sel.Account.ID, "小档伪装池耗尽时跨档到星思，绝不落大档专用号")
	})

	t.Run("big_band_lands_on_kiro", func(t *testing.T) {
		for _, est := range []int{10001, 50000, 149999, 150000} {
			sel := selectWithEstimate(t, newService(t), est)
			require.Equal(t, int64(8102), sel.Account.ID, "估算 %d 落 kiro 大档池", est)
		}
	})

	t.Run("big_over_ceiling_goes_upstream", func(t *testing.T) {
		// 超过全局上限：直回星思，绝不进 kiro（20 万上下文之外静默截断）。
		sel := selectWithEstimate(t, newService(t), TierRoutingMaskedInputCeilingTokens+1)
		require.Equal(t, int64(8103), sel.Account.ID, "超过伪装线大档上限必须直回星思池")
	})

	t.Run("big_outside_account_band_goes_upstream", func(t *testing.T) {
		// 账号自己声明上限 10 万；估算 12 万不命中区间 → 落星思。
		svc := tierRoutingTestService(t, []Account{
			tierRoutingBandAccount(8122, PlatformKimi, 10000, 100000),
			tierRoutingUpstreamAccount(8123, PlatformKimi),
		})
		sel := selectWithEstimate(t, svc, 120000)
		require.Equal(t, int64(8123), sel.Account.ID, "估算超出账号声明区间时必须落星思池")
	})

	t.Run("big_below_band_min_goes_upstream", func(t *testing.T) {
		// 账号声明下限 3 万；估算恰好 1.5 万（仍属大档）不命中 → 落星思。
		svc := tierRoutingTestService(t, []Account{
			tierRoutingBandAccount(8132, PlatformKimi, 30000, 150000),
			tierRoutingUpstreamAccount(8133, PlatformKimi),
		})
		sel := selectWithEstimate(t, svc, 15000)
		require.Equal(t, int64(8133), sel.Account.ID, "估算低于账号声明下限时必须落星思池")
	})
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

	// 模型名归一化（用户裁定：上游的 -1017 一律归一成 flash 再对下游提供）：
	// 下游只会用统一写法请求，账号映射里配的可能是族内任一写法，门必须按族匹配，
	// 否则统一写法请求命中不了「只配了历史写法」的账号，被误判成门关而回落真上游。
	t.Run("gate_matches_model_name_alias_family", func(t *testing.T) {
		acct := tierRoutingMaskedAccountWithModel(5006, PlatformKimi, "deepseek-v4.1-flash-1017")
		require.True(t, tierRoutingGateOpenForAccounts([]Account{acct}, model),
			"账号只配族内历史写法 -1017 时，统一写法 flash 的请求也应开门")

		reverse := tierRoutingMaskedAccountWithModel(5007, PlatformKimi, model)
		require.True(t, tierRoutingGateOpenForAccounts([]Account{reverse}, "deepseek-v4.1-flash-1017"),
			"反向同理：账号只配统一写法时，族内历史写法请求也应开门")
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
			WithTierRouting(ctx, class, tierRoutingTestEstimateFor(class)), &groupID, "", "", model, nil,
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

	t.Run("big_band_pool_exhausted_crosses_with_alert", func(t *testing.T) {
		// 大档先试伪装线大档池（声明了 masked_input_band 的账号）；该账号运行态不可用
		// 时整池无可用 → 跨档到星思池并告警。
		unavailable := tierRoutingBandAccount(5221, PlatformKimi, 10000, TierRoutingMaskedInputCeilingTokens)
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
		require.Equal(t, int64(5222), sel.Account.ID, "大档伪装池无可用时跨档到上游池")
		require.Contains(t, buf.String(), "kimi_tier_cross_pool_failover", "大档跨档必须产生告警事件")
	})

	t.Run("big_never_falls_back_to_small_masked_pool", func(t *testing.T) {
		// 大档伪装池空、但存在未声明区间的小档伪装号：大请求绝不能落到它身上。
		svc := tierRoutingTestService(t, []Account{
			tierRoutingMaskedAccount(5231, PlatformKimi),
			tierRoutingUpstreamAccount(5232, PlatformKimi),
		})
		sel, err := selectFor(t, svc, TierRoutingBig)
		require.NoError(t, err)
		require.NotNil(t, sel)
		require.NotNil(t, sel.Account)
		require.Equal(t, int64(5232), sel.Account.ID, "大请求不得回落到小档伪装池")
	})
}

// TestTierRoutingSelectAccountRouting 覆盖分层路由选号主语义与职责隔离（派发单 OC-T1，
// 测试 #2 分档落池 + 测试 #4 故障路径）：
//   - ≤3000（small）→ 仅落 qwen 伪装线池（masked 账号）；
//   - >3000（big）→ 仅落星思池（非 masked 账号）；
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
		selCtx := WithTierRouting(ctx, TierRoutingSmall, tierRoutingSmallEstimate)
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
		selCtx := WithTierRouting(ctx, TierRoutingBig, tierRoutingBigEstimate)
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
		selCtx := WithTierRouting(ctx, TierRoutingSmall, tierRoutingSmallEstimate)
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
		selCtx := WithTierRouting(ctx, TierRoutingSmall, tierRoutingSmallEstimate)
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
//   - ≤3000（small）→ 仅落伪装线池（masked 账号）；
//   - >3000（big）→ 仅落非伪装池（upstream 账号）；
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
				WithTierRouting(context.Background(), TierRoutingSmall, tierRoutingSmallEstimate), &groupID, "", "", combo.model, nil,
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
				WithTierRouting(context.Background(), TierRoutingBig, tierRoutingBigEstimate), &groupID, "", "", combo.model, nil,
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
				WithTierRouting(context.Background(), TierRoutingSmall, tierRoutingSmallEstimate), &groupID, "", "", combo.model, nil,
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

	t.Run("snapshot_opens_when_masked_and_unmasked_coexist", func(t *testing.T) {
		// 复现生产事故场景：桶内同时含伪装号 241（mask_upstream_identity=true、model_mapping
		// 含 kimi-k3）与未伪装号 235。门控经 ListSchedulableAccounts 拿到与选号器同源的同一
		// 候选集，应识别 241 而开门——快照投影（filterSchedulerExtra）须保留 mask_upstream_identity。
		masked := tierRoutingMaskedAccountWithModel(241, PlatformKimi, model)
		unmasked := tierRoutingUpstreamAccount(235, PlatformKimi)
		svc := &OpenAIGatewayService{
			schedulerSnapshot: &SchedulerSnapshotService{cache: &openAISnapshotCacheStub{snapshotAccounts: []*Account{&masked, &unmasked}}},
		}
		open, err := svc.TierRoutingGateOpen(context.Background(), &groupID, PlatformKimi, model)
		require.NoError(t, err)
		require.True(t, open, "桶内存在伪装号时门控应开（与选号同源、投影保留伪装标记）")
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
