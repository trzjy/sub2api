package service

import (
	"context"
	_ "embed"
	"encoding/json"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

// devinPricingSnapshot 是父会话从 `devin models list --format json` 抓取的官方单价快照。
// 快照只读；解析在 init 中一次性完成，失败立即 panic（fail closed，绝不猜价格）。
//
//go:embed devin_pricing_snapshot.json
var devinPricingSnapshot []byte

// devinPrice 是某模型每 1M token 的官方美元单价。
type devinPrice struct {
	Input       float64 `json:"input"`
	CachedInput float64 `json:"cached_input"`
	Output      float64 `json:"output"`
}

// devinPriceTable 是模型名 → 官方单价的查表。
type devinPriceTable map[string]devinPrice

// devinLocalPrices 是从内嵌快照解析出的官方单价表。
var devinLocalPrices devinPriceTable

func init() {
	var snap struct {
		Prices devinPriceTable `json:"prices"`
	}
	if err := json.Unmarshal(devinPricingSnapshot, &snap); err != nil {
		panic("devin pricing snapshot: parse failed: " + err.Error())
	}
	if len(snap.Prices) == 0 {
		panic("devin pricing snapshot: empty prices")
	}
	devinLocalPrices = snap.Prices
}

// lookup 在价格表中按模型名查找官方单价。
func (t devinPriceTable) lookup(model string) (devinPrice, bool) {
	p, ok := t[model]
	return p, ok
}

// DevinLocalCost 是我们网关按官方单价 × 本网关 token 折算出的用量费用。
type DevinLocalCost struct {
	WindowStart    time.Time `json:"window_start"`
	WindowEnd      time.Time `json:"window_end"`
	USD            float64   `json:"usd"`
	PricedModels   []string  `json:"priced_models"`
	UnpricedModels []string  `json:"unpriced_models"`
}

// devinLocalCostForStats 是纯计算函数：按 priceLookup 给出的官方单价把各模型 token 折算成费用。
// priceLookup 返回 ok=false 的模型记入 UnpricedModels 并跳过（不猜价格）；
// 快照中价格为 0 的免费模型仍视为已计价（计入 PricedModels），费用为 0。
func devinLocalCostForStats(stats []usagestats.ModelStat, priceLookup func(string) (devinPrice, bool)) *DevinLocalCost {
	result := &DevinLocalCost{}
	for _, st := range stats {
		price, ok := priceLookup(st.Model)
		if !ok {
			result.UnpricedModels = append(result.UnpricedModels, st.Model)
			continue
		}
		// 缓存写入（cache_creation）按输入价计：Devin 官方价目只给 input / cached_input / output
		// 三档，缓存写入无独立单价，按惯例归入输入价。
		cost := float64(st.InputTokens)*price.Input/1e6 +
			float64(st.OutputTokens)*price.Output/1e6 +
			float64(st.CacheReadTokens)*price.CachedInput/1e6 +
			float64(st.CacheCreationTokens)*price.Input/1e6
		result.USD += cost
		result.PricedModels = append(result.PricedModels, st.Model)
	}
	return result
}

// computeDevinLocalCost 查询本网关在统计窗口内的 token 用量并按官方单价折算费用。
// 任何失败都返回 nil（不影响 Devin 状态查询主流程，LocalCost 为 omitempty）。
func (s *AccountUsageService) computeDevinLocalCost(ctx context.Context, account *Account, status *DevinAccountStatus) *DevinLocalCost {
	if s == nil || s.usageLogRepo == nil || account == nil {
		return nil
	}

	now := time.Now().UTC()
	// 窗口起点基准：优先用官方 DailyResetAt，缺失时退化为 now-24h；
	// 再减 24h 作为统计窗口起点，窗口终点为 now。
	base := now.Add(-24 * time.Hour)
	if status != nil && status.DailyResetAt != nil {
		base = *status.DailyResetAt
	}
	windowStart := base.Add(-24 * time.Hour)

	stats, err := s.usageLogRepo.GetModelStatsWithFilters(ctx, windowStart, now, 0, 0, account.ID, 0, nil, nil, nil)
	if err != nil {
		return nil
	}

	// 模型名先按原名查；查不到时用账号级 model_mapping 映射后再查。
	resolved := make([]usagestats.ModelStat, 0, len(stats))
	for _, st := range stats {
		if _, ok := devinLocalPrices.lookup(st.Model); !ok {
			st.Model = account.GetMappedModel(st.Model)
		}
		resolved = append(resolved, st)
	}

	result := devinLocalCostForStats(resolved, devinLocalPrices.lookup)
	result.WindowStart = windowStart
	result.WindowEnd = now
	return result
}
