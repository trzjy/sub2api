//go:build unit

package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

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
