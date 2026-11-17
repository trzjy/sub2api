package service

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
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

// upstreamModelStatsProvider 是按 upstream_model 维度聚合用量的窄接口。
// UsageLogRepository 本身未暴露 GetModelStatsWithFiltersBySource，因此这里对
// 具体仓库实现做类型断言；断言失败即失败关闭，绝不回落到 requested 口径。
type upstreamModelStatsProvider interface {
	GetModelStatsWithFiltersBySource(ctx context.Context, startTime, endTime time.Time, userID, apiKeyID, accountID, groupID int64, requestType *int16, stream *bool, billingType *int8, source string) ([]usagestats.ModelStat, error)
}

// computeDevinLocalCost 查询本网关在统计窗口内的 token 用量并按官方单价折算费用。
// 统计按日志的 upstream_model 维度聚合，返回的模型名即 upstream 真名（SQL 侧
// COALESCE 到 model），直接用于计价，不再经过账号 model_mapping 重映射。
// 任何失败都返回 error，由调用方经 UsageInfo.Error 通道对外暴露（失败关闭）。
func (s *AccountUsageService) computeDevinLocalCost(ctx context.Context, account *Account, status *DevinAccountStatus) (*DevinLocalCost, error) {
	if s == nil || s.usageLogRepo == nil || account == nil {
		return nil, fmt.Errorf("usage repository or account unavailable")
	}
	provider, ok := s.usageLogRepo.(upstreamModelStatsProvider)
	if !ok {
		return nil, fmt.Errorf("usage repo does not implement upstream model stats")
	}

	now := time.Now().UTC()
	windowEnd := now
	// 窗口起点：优先用官方 DailyResetAt 减 24h；缺失时退化为 now-24h。
	// 注意只在缺失分支算一次 -24h，禁止对 now 双重减 24h（否则窗口变 48h）。
	windowStart := now.Add(-24 * time.Hour)
	if status != nil && status.DailyResetAt != nil {
		windowStart = status.DailyResetAt.Add(-24 * time.Hour)
	}

	stats, err := provider.GetModelStatsWithFiltersBySource(ctx, windowStart, windowEnd, 0, 0, account.ID, 0, nil, nil, nil, "upstream")
	if err != nil {
		return nil, fmt.Errorf("query upstream model stats: %w", err)
	}

	// stats 中的 Model 已是日志的 upstream_model，直接查表计价即可；
	// 不得再经 account.GetMappedModel 映射，否则会按 requested 名算错价。
	result := devinLocalCostForStats(stats, devinLocalPrices.lookup)
	result.WindowStart = windowStart
	result.WindowEnd = windowEnd
	return result, nil
}
