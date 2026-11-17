//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

// devinUpstreamStatsStub 实现窄接口 upstreamModelStatsProvider。
// 嵌入 UsageLogRepository 仅用于满足 s.usageLogRepo 的完整接口约束，
// 其余方法在测试中不会被调用。
type devinUpstreamStatsStub struct {
	UsageLogRepository
	stats     []usagestats.ModelStat
	err       error
	gotSource string
	gotStart  time.Time
	gotEnd    time.Time
	gotAcctID int64
}

func (s *devinUpstreamStatsStub) GetModelStatsWithFiltersBySource(
	ctx context.Context, startTime, endTime time.Time,
	userID, apiKeyID, accountID, groupID int64,
	requestType *int16, stream *bool, billingType *int8, source string,
) ([]usagestats.ModelStat, error) {
	s.gotStart = startTime
	s.gotEnd = endTime
	s.gotAcctID = accountID
	s.gotSource = source
	return s.stats, s.err
}

// TestDevinLocalCostPriceLookup 验证官方单价查表：
//  1. gemini-4-hydrogen-xhigh 命中官方单价 input1.5 / cached0.08 / output7.5；
//     （内嵌快照必须与该官方单价一致，防止 embed/解析出错）
//  2. 免费模型（价格为 0）仍视为可计价，费用为 0；
//  3. 不存在的模型返回 ok=false。
func TestDevinLocalCostPriceLookup(t *testing.T) {
	// 用注入的价格表覆盖免费模型场景：快照当前未收录零价模型，
	// 但查表逻辑对“价目存在且为 0”的免费模型必须返回 ok=true 且全 0。
	table := devinPriceTable{
		"gemini-4-hydrogen-xhigh": {Input: 1.5, CachedInput: 0.08, Output: 7.5},
		"swe-2-max":               {Input: 0, CachedInput: 0, Output: 0},
	}

	// 1) 官方单价查表
	p, ok := table.lookup("gemini-4-hydrogen-xhigh")
	require.True(t, ok)
	require.InDelta(t, 1.5, p.Input, 1e-9)
	require.InDelta(t, 0.08, p.CachedInput, 1e-9)
	require.InDelta(t, 7.5, p.Output, 1e-9)

	// 内嵌快照必须给出同样的官方单价
	real, ok := devinLocalPrices.lookup("gemini-4-hydrogen-xhigh")
	require.True(t, ok)
	require.InDelta(t, 1.5, real.Input, 1e-9)
	require.InDelta(t, 0.08, real.CachedInput, 1e-9)
	require.InDelta(t, 7.5, real.Output, 1e-9)

	// 2) 免费模型：价格为 0 但可计价
	free, ok := table.lookup("swe-2-max")
	require.True(t, ok)
	require.Zero(t, free.Input)
	require.Zero(t, free.CachedInput)
	require.Zero(t, free.Output)

	// 3) 不存在的模型
	_, ok = table.lookup("definitely-not-a-real-model")
	require.False(t, ok)
}

// TestDevinLocalCostForStats 验证费用纯计算：
// USD 精确值、免费模型计入 PricedModels 且费用 0、缺失价格计入 UnpricedModels 并跳过。
func TestDevinLocalCostForStats(t *testing.T) {
	lookup := func(model string) (devinPrice, bool) {
		switch model {
		case "gemini-4-hydrogen-xhigh":
			return devinPrice{Input: 1.5, CachedInput: 0.08, Output: 7.5}, true
		case "swe-2-max":
			return devinPrice{}, true // 免费模型
		default:
			return devinPrice{}, false
		}
	}

	stats := []usagestats.ModelStat{
		{
			Model:               "gemini-4-hydrogen-xhigh",
			InputTokens:         1_000_000,
			OutputTokens:        1_000_000,
			CacheReadTokens:     1_000_000,
			CacheCreationTokens: 1_000_000,
		},
		{Model: "swe-2-max", InputTokens: 999, OutputTokens: 999}, // 免费 → 费用 0
		{Model: "unknown-model", InputTokens: 1_000_000},          // 无价格 → 跳过
	}

	got := devinLocalCostForStats(stats, lookup)

	// 1*1.5(input) + 1*7.5(output) + 1*0.08(cache_read) + 1*1.5(cache_creation at input price) = 10.58
	require.InDelta(t, 10.58, got.USD, 1e-9)
	require.Equal(t, []string{"gemini-4-hydrogen-xhigh", "swe-2-max"}, got.PricedModels)
	require.Equal(t, []string{"unknown-model"}, got.UnpricedModels)
}

// TestDevinLocalCostUsesUpstreamModel 验证 r22 must_fix #1：按 upstream_model 计价。
// 构造“requested 名与 upstream 名不同”的反例：账号 model_mapping 会把 upstream
// 真名 claude-5-fable-high($10/M) 改写成廉价的 gemini-4-hydrogen-xhigh($1.5/M)。
// 正确实现必须忽略该映射、直接用 upstream 名计价（$10），且 source 必须为 "upstream"。
func TestDevinLocalCostUsesUpstreamModel(t *testing.T) {
	stub := &devinUpstreamStatsStub{
		stats: []usagestats.ModelStat{
			{Model: "claude-5-fable-high", InputTokens: 1_000_000},
		},
	}
	account := &Account{
		ID: 123,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"claude-5-fable-high": "gemini-4-hydrogen-xhigh",
			},
		},
	}
	reset := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	svc := &AccountUsageService{usageLogRepo: stub}
	beforeCall := time.Now().UTC()

	lc, err := svc.computeDevinLocalCost(context.Background(), account, &DevinAccountStatus{DailyResetAt: &reset})
	require.NoError(t, err)
	require.NotNil(t, lc)

	// 聚合维度必须是 upstream，且按 accountID 过滤。
	require.Equal(t, "upstream", stub.gotSource)
	require.Equal(t, int64(123), stub.gotAcctID)

	// 计价用 upstream 返回的模型名，而非被映射后的 requested 名。
	require.Equal(t, []string{"claude-5-fable-high"}, lc.PricedModels)
	require.InDelta(t, 10.0, lc.USD, 1e-9) // 若误按 gemini 价则为 1.5

	// 窗口：起点 = reset-24h，终点 = now（不是 reset），且传给 repo 的窗口与结果一致。
	require.Equal(t, reset.Add(-24*time.Hour), lc.WindowStart)
	require.False(t, lc.WindowEnd.Before(beforeCall.Add(-time.Minute)), "windowEnd must be ~now, not reset")
	require.False(t, lc.WindowEnd.After(time.Now().UTC().Add(time.Minute)))
	require.Equal(t, lc.WindowStart, stub.gotStart)
	require.Equal(t, lc.WindowEnd, stub.gotEnd)
}

// TestDevinLocalCostQueryErrorPropagates 验证 r22 must_fix #2：
// 用量库查询失败必须返回 error（且结果为空），由上层负责对外暴露。
func TestDevinLocalCostQueryErrorPropagates(t *testing.T) {
	stub := &devinUpstreamStatsStub{err: errors.New("usage db down")}
	svc := &AccountUsageService{usageLogRepo: stub}

	lc, err := svc.computeDevinLocalCost(context.Background(), &Account{ID: 1}, &DevinAccountStatus{})
	require.Error(t, err)
	require.Nil(t, lc)
	require.Contains(t, err.Error(), "usage db down")
}

// TestDevinLocalCostWindowWithoutDailyReset 验证 DailyResetAt=nil 时窗口恰好为 24h，
// 不得对 now 双重减 24h（旧缺陷会算成 48h）。
func TestDevinLocalCostWindowWithoutDailyReset(t *testing.T) {
	stub := &devinUpstreamStatsStub{
		stats: []usagestats.ModelStat{{Model: "claude-5-fable-high", InputTokens: 1_000}},
	}
	svc := &AccountUsageService{usageLogRepo: stub}
	before := time.Now().UTC().Add(-24 * time.Hour)

	lc, err := svc.computeDevinLocalCost(context.Background(), &Account{ID: 1}, &DevinAccountStatus{})
	require.NoError(t, err)
	require.NotNil(t, lc)

	// 跨度 ≈ 24h（双重减 24h 会变成 48h）。
	require.InDelta(t, float64(24*time.Hour), float64(lc.WindowEnd.Sub(lc.WindowStart)), float64(time.Second))
	// 起点不得早于“调用前 now-24h”一分钟以上，进一步排除多减一日。
	require.False(t, lc.WindowStart.Before(before.Add(-time.Minute)),
		"window start %s is more than a day before now", lc.WindowStart)
	require.Equal(t, lc.WindowStart, stub.gotStart)
	require.Equal(t, lc.WindowEnd, stub.gotEnd)
}
