//go:build unit

// E7 定向测试：探测链三缺陷整改（终审重放 #1/#5/#6）。
//   1. 探测错误不得默认成功（runTokenHarborProbe 返回错误时零值结果不得当 Success）；
//   2. 预算拒绝候选进入调度快照即冻结（TryAcquire 判定之前），拒绝不影响冻结值；
//   3. 到期剔除以 precise_reset 为唯一判别（E42 校正，D5 锁定）：precise_reset=true
//      的到期条目照常剔除；precise_reset=false/缺省（无信号哨兵）到期不吃剔除，维持持续受限。
package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ===================== E7 #1：错误路径不得默认成功 =====================

// runTokenHarborProbe 返回错误时，其零值结果（ProbeOutcomeSuccess=iota=0）不得被当
// 成功：必须回退 Unclassified（仅置 attempted_at），且不得 clearCandidateBound——
// 否则零值结果触发 clearCandidateBound + 写入口清除真实限流。
func TestE7ProbeError_DoesNotClearBoundNorEntry(t *testing.T) {
	now := time.Unix(15000, 0)
	repo := newD2ProbeObsRepo()
	rl := &RateLimitService{accountRepo: repo}
	const acct int64 = 71
	const scope = "deepseek:free"

	// 预置受限条目 + 已冻结上界（模拟该候选已进入调度并冻结）。
	repo.presetEntry(acct, scope, map[string]any{
		"rate_limit_reset_at": now.Add(time.Hour).UTC().Format(time.RFC3339),
		"reason":              tokenHarborFreeTierReasonPrefix + ":x",
	})
	svc := &AccountHealthRecoveryProbeService{
		nowFunc:      func() time.Time { return now },
		rateLimit:    rl,
		accountRepo:  repo,
		frozenBounds: make(map[string]int),
	}
	svc.freezeCandidateBound(acct, scope, 5)

	// 探测覆盖返回错误（连带零值/成功结果）——真实场景如传输未配置、URL 校验失败。
	svc.probeTokenHarborOverride = func(_ context.Context, _ *Account, _ string) (ProbeOutcome, error) {
		return ProbeOutcomeSuccess, errors.New("simulated transport failure")
	}

	svc.probeOneTokenHarbor(context.Background(), &Account{ID: acct}, tokenHarborCandidate{modelKey: scope})

	// 冻结上界不得被清除（错误路径不是权威恢复成功）。
	require.Equal(t, 5, svc.frozenCandidateBound(acct, scope), "错误路径不得清除冻结上界")

	// 写入口：条目不得被清除（错误路径不得误当 Success 清除真实限流）。
	_, ok := repo.entryFor(acct, scope)
	require.True(t, ok, "错误路径不得清除限流条目")

	// 结果分类：必须为 Unclassified（仅置 attempted_at），不得记 Success。
	stats, ok := svc.GetProbeObservationStats(acct)
	require.True(t, ok)
	require.Equal(t, int64(1), stats.Unclassified, "错误路径应计为 Unclassified")
	require.Zero(t, stats.Success, "错误路径不得计为 Success")
	require.Equal(t, int64(1), stats.Sends, "错误路径仍算一次实际发出")
}

// ===================== E7 #5：预算拒绝候选也冻结 =====================

// 候选进入本调度快照即冻结候选总数（TryAcquire 判定之前），预算拒绝/在途去重不影响
// 冻结值——否则被拒候选下轮首获准时按更小总数冻结，阈值被错误缩短。
func TestE7FreezeBound_AppliesBeforeBudgetAcquire(t *testing.T) {
	now := time.Unix(16000, 0)
	svc := &AccountHealthRecoveryProbeService{
		nowFunc:      func() time.Time { return now },
		budget:       newProbeBudgetManager(func() time.Time { return now }),
		frozenBounds: make(map[string]int),
	}
	const acct int64 = 72

	// 先占满该账号 60 预算窗口：所有候选 TryAcquire 均被拒绝。
	for i := 0; i < probeBudgetPerMinute; i++ {
		require.True(t, svc.budget.TryAcquire(acct, fmt.Sprintf("filler-%d", i)), "filler 应获准直到窗口满")
	}

	acc := &Account{ID: acct}
	scopes := []string{"A", "B", "C"}
	svc.dispatchAccountTokenHarbor(context.Background(), acc, scopes, false)

	// 预算拒绝不影响冻结：三候选均按候选总数 3 冻结。
	require.Equal(t, 3, svc.frozenCandidateBound(acct, "A"), "预算拒绝候选仍应有冻结值")
	require.Equal(t, 3, svc.frozenCandidateBound(acct, "B"), "预算拒绝候选仍应有冻结值")
	require.Equal(t, 3, svc.frozenCandidateBound(acct, "C"), "预算拒绝候选仍应有冻结值")

	// 预算拒绝计数已记录，无实际发送。
	stats, ok := svc.GetProbeObservationStats(acct)
	require.True(t, ok)
	require.Equal(t, int64(3), stats.BudgetRejects, "三个候选均被预算拒绝")
	require.Zero(t, stats.Sends, "预算拒绝不产生实际发送")
}

// 在途去重命中（同模型候选已在途）同样冻结：去重候选进入快照即冻结，不因未获准而无值。
func TestE7FreezeBound_AppliesOnInflightDedup(t *testing.T) {
	now := time.Unix(16100, 0)
	svc := &AccountHealthRecoveryProbeService{
		nowFunc:      func() time.Time { return now },
		budget:       newProbeBudgetManager(func() time.Time { return now }),
		frozenBounds: make(map[string]int),
	}
	const acct int64 = 73

	// 占用 M 的在途位（模拟上一轮未释放）→ 本轮 TryAcquire 被在途去重拒绝。
	require.True(t, svc.budget.TryAcquire(acct, "M"), "占用在途位")

	acc := &Account{ID: acct}
	// 同轮内 M 仍在途，再次分派 M → 在途去重拒绝。
	svc.dispatchAccountTokenHarbor(context.Background(), acc, []string{"M"}, false)

	require.Equal(t, 1, svc.frozenCandidateBound(acct, "M"), "在途去重候选仍应有冻结值")
	stats, ok := svc.GetProbeObservationStats(acct)
	require.True(t, ok)
	require.Equal(t, int64(1), stats.BudgetRejects, "在途去重计入预算拒绝")
	require.Zero(t, stats.Sends, "在途去重不产生实际发送")
}

// ===================== E7 #6：到期剔除以 precise_reset 为唯一判别（E42 校正） =====================

// E42 落地后 E7 #6 旧语义被取代（终审第九轮 must_fix，packet v9 #2）：
// 到期剔除不再由 precise=true 跳过，而是仅 precise_reset=true 的权威恢复标记按 reset_at
// 到期照常剔除；precise_reset=false/缺省（无信号哨兵）的 reset_at 仅为"持续受限"占位值，
// 到期不吃剔除、维持持续受限直至复探写入口清除。未到期条目（含 D5 哨兵）本就纳入。
// 本测试即 ActiveTokenHarborFreeTierScopes 同包同函数消费方，需随 E42 最终态反转。
func TestE7ActiveScopes_ExpiryEvictionSemantics(t *testing.T) {
	now := time.Unix(17000, 0)
	svc := &AccountHealthRecoveryProbeService{}
	acc := &Account{ID: 81, Extra: map[string]any{
		modelRateLimitsKey: map[string]any{
			// precise=true 且已到期 → 照常剔除（E42 校正：权威恢复标记按 reset_at 到期）。
			"m-precise-expired": map[string]any{
				"reason":              tokenHarborFreeTierReasonPrefix + ":x",
				"rate_limit_reset_at": now.Add(-time.Hour).UTC().Format(time.RFC3339),
				"precise_reset":       true,
			},
			// precise=false 且已到期（无信号哨兵）→ 不吃剔除，仍纳入候选。
			"m-legacy-expired": map[string]any{
				"reason":              tokenHarborFreeTierReasonPrefix + ":x",
				"rate_limit_reset_at": now.Add(-time.Hour).UTC().Format(time.RFC3339),
				"precise_reset":       false,
			},
			// 无 precise_reset 字段（存量旧条目，等价 false）且已到期 → 不吃剔除，纳入。
			"m-noflag-expired": map[string]any{
				"reason":              tokenHarborFreeTierReasonPrefix + ":x",
				"rate_limit_reset_at": now.Add(-time.Hour).UTC().Format(time.RFC3339),
			},
			// precise=false + 远期 reset_at（D5 哨兵）→ 未到期本就纳入。
			"m-sentinel-future": map[string]any{
				"reason":              tokenHarborFreeTierReasonPrefix + ":x",
				"rate_limit_reset_at": now.Add(time.Hour).UTC().Format(time.RFC3339),
				"precise_reset":       false,
			},
			// precise=true 且未到期 → 正常纳入。
			"m-precise-future": map[string]any{
				"reason":              tokenHarborFreeTierReasonPrefix + ":x",
				"rate_limit_reset_at": now.Add(time.Hour).UTC().Format(time.RFC3339),
				"precise_reset":       true,
			},
			// 非 TokenHarbor 前缀 → 仍排除（不受到期逻辑影响）。
			"m-other": map[string]any{
				"reason":              "some_other_limit",
				"rate_limit_reset_at": now.Add(-time.Hour).UTC().Format(time.RFC3339),
			},
		},
	}}

	got := svc.activeTokenHarborFreeTierScopes(acc, now)
	require.ElementsMatch(t,
		[]string{"m-legacy-expired", "m-noflag-expired", "m-sentinel-future", "m-precise-future"},
		got, "precise=true 到期剔除；precise=false/缺省（无信号哨兵）到期不吃剔除仍纳入；未到期（含 D5 哨兵）正常纳入")
}
