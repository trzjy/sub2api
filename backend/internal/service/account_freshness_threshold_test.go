//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestFreshnessThreshold_ThreeDimSameFormula 验证三维有效阈值由同一公式产出：
// 账号+模型 / 账号级 / 渠道 三者对相同的（基线, 上界）输入给出相同结果，不各算各的。
func TestFreshnessThreshold_ThreeDimSameFormula(t *testing.T) {
	baseline := 120 * time.Second
	// 上界来自冻结候选总数 = 130 -> ceil(130/60)=3 分钟 + 60 + 15 + 60 = 315s。
	ub := AccountSideUpperBound(130, probeRoundPeriod, probeRequestHardTimeout, probeStartupCooldown)
	require.Equal(t, 315*time.Second, ub)

	// 三维分别调用各自的封装，结果都必须等于 EffectiveFreshnessThreshold(baseline, ub)。
	want := EffectiveFreshnessThreshold(baseline, ub)
	require.Equal(t, want, AccountModelFreshnessThreshold(130))
	require.Equal(t, want, AccountLevelFreshnessThreshold(130))
	require.Equal(t, want, ComputeChannelFreshnessThreshold(baseline, ub))
}

// TestFreshnessThreshold_AccountModelVsAccountLevelSharedFormula 验证账号+模型与账号级
// 复用同一上界公式（仅 modelKey 不同而共用冻结窗口），对相同候选总数产出一致的上界。
func TestFreshnessThreshold_AccountModelVsAccountLevelSharedFormula(t *testing.T) {
	// 普通场景：候选总数 <= 60，无额外预算等待项，仅 1×轮次周期 + 硬超时 + 冷启动。
	small := AccountSideUpperBound(40, probeRoundPeriod, probeRequestHardTimeout, probeStartupCooldown)
	wantSmall := probeRoundPeriod + probeRequestHardTimeout + probeStartupCooldown
	require.Equal(t, wantSmall, small)

	// 最坏场景：候选总数 > 60，预算等待项 = ceil(总数/60) 分钟。
	big := AccountSideUpperBound(100, probeRoundPeriod, probeRequestHardTimeout, probeStartupCooldown)
	require.Equal(t, 2*time.Minute+wantSmall, big)

	// 账号+模型与账号级对相同总数产出一致（同公式）。
	require.Equal(t, AccountModelFreshnessThreshold(100), AccountLevelFreshnessThreshold(100))
}

// TestFreshnessThreshold_CandidateOver120NoPermanentAlert 候选数 > 120 场景：
// 阈值随候选总数放大，合法等待窗口内的观测不被误报为陈旧（不产生永久告警）。
func TestFreshnessThreshold_CandidateOver120NoPermanentAlert(t *testing.T) {
	// ceil(121/60) = 3 分钟 + 60 + 15 + 60 = 315s。
	th := AccountLevelFreshnessThreshold(121)
	require.Equal(t, 315*time.Second, th)

	now := time.Now()
	// 候选在阈值内（差 1s）合法等待 -> 不陈旧。
	within := now.Add(-314 * time.Second)
	require.False(t, now.Sub(within) > th, "候选在阈值内合法等待不应报陈旧")
	// 超过阈值 -> 陈旧（可被告警，但这是真实陈旧而非误报）。
	beyond := now.Add(-316 * time.Second)
	require.True(t, now.Sub(beyond) > th)
}

// TestFreshnessThreshold_AccountLevelTailOfMixedQueueNoFalseAlert 账号级候选位于混合队列
// 尾（与模型级候选共用同一冻结窗口），合法等待数分钟不误告警。
func TestFreshnessThreshold_AccountLevelTailOfMixedQueueNoFalseAlert(t *testing.T) {
	// 混合候选总数 = 130（含模型级 + 1 账号级），账号级冻结值 = 130。
	th := AccountLevelFreshnessThreshold(130)
	require.Equal(t, 315*time.Second, th)

	now := time.Now()
	// 账号级候选在冻结窗口内（差 1s）的观测 -> 不陈旧。
	within := now.Add(-314 * time.Second)
	require.False(t, now.Sub(within) > th)
}

// TestFreshnessThreshold_ChannelEmptySetIsBaseline 渠道空集合（无账号异常候选）语义固定 =
// 配置基线，不得退化为零值；且「渠道 error 且无账号异常候选」同源计算。
func TestFreshnessThreshold_ChannelEmptySetIsBaseline(t *testing.T) {
	baseline := channelFreshnessBaselineDefault
	// 空集合（ub <= 0）返回基线。
	require.Equal(t, baseline, ComputeChannelFreshnessThreshold(baseline, 0))
	require.Equal(t, baseline, ComputeChannelFreshnessThreshold(baseline, -1))

	// 「渠道 error 且无账号异常候选」：accountSideWorstUpperBound 为空集 -> 基线。
	noCandidates := ComputeChannelFreshnessThreshold(baseline, 0)
	require.Equal(t, baseline, noCandidates)
	require.NotZero(t, noCandidates, "空集合不得退化为零值")
}

// recordingBounds 记录 provider 收到的 modelKey，验证入口归一后的查询键。
type recordingBounds struct {
	key string
	ub  time.Duration
}

func (r *recordingBounds) AccountFreshnessUpperBound(_ int64, modelKey string) time.Duration {
	r.key = modelKey
	return r.ub
}

// TestAccountFreshnessThreshold_AccountLevelScopeNormalizedToEmptyBoundKey E13 回归：
// 账号级候选由调度器以空 modelKey 冻结（dispatchAccountTokenHarbor 中 accountLevel 候选
// modelKey=""），故 AccountFreshnessThreshold 收到账号级占位 scope 时必须归一到空串
// 再查冻结上界；否则 provider 查不到该键返回 0，账号级阈值退化为纯基线，与管理端展示、
// 告警评估（EvaluateAccountLevelFreshness 传 ""）三处不一致。
func TestAccountFreshnessThreshold_AccountLevelScopeNormalizedToEmptyBoundKey(t *testing.T) {
	probe := &AccountHealthRecoveryProbeService{frozenBounds: make(map[string]int)}
	// 调度器冻结账号级候选用空 modelKey，冻结候选总数 130。
	probe.freezeCandidateBound(1, "", 130)
	rl := &RateLimitService{}
	rl.SetFreshnessBoundsProvider(probe)

	got := rl.AccountFreshnessThreshold(1, tokenHarborAccountLevelProbeScope)
	require.Equal(t, 315*time.Second, got,
		"账号级 scope 必须归一到空键查冻结上界，返回冻结参与的阈值（非纯基线）")
	require.NotEqual(t, accountLevelFreshnessBaseline, got, "不得退化为纯基线")

	// 直接钉住 provider 收到的查询键为空串（归一契约）。
	rec := &recordingBounds{ub: 315 * time.Second}
	rl2 := &RateLimitService{}
	rl2.SetFreshnessBoundsProvider(rec)
	require.Equal(t, 315*time.Second, rl2.AccountFreshnessThreshold(1, tokenHarborAccountLevelProbeScope))
	require.Equal(t, "", rec.key, "账号级 scope 必须归一到空串传给 provider")
}

// TestAccountFreshnessThreshold_ModelScopeUnchanged E13 回归：模型级 scope 与 modelKey
// 同值，入口归一为空操作，行为不变（仍以 scope 原值作为上界查询键）。
func TestAccountFreshnessThreshold_ModelScopeUnchanged(t *testing.T) {
	probe := &AccountHealthRecoveryProbeService{frozenBounds: make(map[string]int)}
	probe.freezeCandidateBound(2, "gpt-4", 130)
	rl := &RateLimitService{}
	rl.SetFreshnessBoundsProvider(probe)

	require.Equal(t, 315*time.Second, rl.AccountFreshnessThreshold(2, "gpt-4"),
		"模型级 scope 行为不变：按 scope 原值查冻结上界")

	rec := &recordingBounds{ub: 315 * time.Second}
	rl2 := &RateLimitService{}
	rl2.SetFreshnessBoundsProvider(rec)
	require.Equal(t, 315*time.Second, rl2.AccountFreshnessThreshold(2, "gpt-4"))
	require.Equal(t, "gpt-4", rec.key, "模型级 scope 原样传 provider")

	// 未冻结的模型级 scope：空集合 → 基线（不退化为零值）。
	require.Equal(t, accountModelFreshnessBaseline, rl.AccountFreshnessThreshold(2, "never-scheduled"))
}
