package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

// PricingAdminService 价格管理中心服务：
// 聚合同步状态、全局价格目录（来源分层）、未覆盖模型双通道扫描与生效价试算。
type PricingAdminService struct {
	pricing      *PricingService
	billing      *BillingService
	custom       *CustomModelPricingService
	resolver     *ModelPricingResolver
	groupService *GroupService
	usageRepo    UsageLogRepository
}

func NewPricingAdminService(
	pricing *PricingService,
	billing *BillingService,
	custom *CustomModelPricingService,
	resolver *ModelPricingResolver,
	groupService *GroupService,
	usageRepo UsageLogRepository,
) *PricingAdminService {
	return &PricingAdminService{
		pricing:      pricing,
		billing:      billing,
		custom:       custom,
		resolver:     resolver,
		groupService: groupService,
		usageRepo:    usageRepo,
	}
}

// --- 同步状态 ---

// StatusResponse 同步状态总览（含线上计费缺口）。
type StatusResponse struct {
	Sync     PricingSyncStatus  `json:"sync"`
	Custom   CustomLayerSummary `json:"custom"`
	LiveGaps []PricingGapEntry  `json:"live_gaps"`
}

// CustomLayerSummary 自定义价格层概览。
type CustomLayerSummary struct {
	Entries int `json:"entries"`
	Enabled int `json:"enabled"`
}

// GetStatus 返回同步状态与线上缺口。
func (s *PricingAdminService) GetStatus() *StatusResponse {
	resp := &StatusResponse{Sync: s.pricing.SyncStatusSnapshot(), LiveGaps: s.pricing.PricingGaps()}
	for _, entry := range s.custom.Snapshot() {
		resp.Custom.Entries++
		if entry.Enabled {
			resp.Custom.Enabled++
		}
	}
	return resp
}

// SyncNow 立即触发远程价格表同步。
func (s *PricingAdminService) SyncNow(ctx context.Context) error {
	return s.pricing.SyncNow(ctx)
}

// --- 价格目录 ---

// CatalogEntry 价格目录条目：单一模型名在全局层（custom/remote/builtin）的生效价。
type CatalogEntry struct {
	Model              string  `json:"model"`
	Source             string  `json:"source"` // custom / remote / builtin
	CustomID           int64   `json:"custom_id,omitempty"`
	BillingMode        string  `json:"billing_mode,omitempty"`
	InputPerMTok       float64 `json:"input_per_mtok"`
	OutputPerMTok      float64 `json:"output_per_mtok"`
	CacheWritePerMTok  float64 `json:"cache_write_per_mtok"`
	CacheReadPerMTok   float64 `json:"cache_read_per_mtok"`
	TokenPricingAbsent bool    `json:"token_pricing_absent,omitempty"`
}

// CatalogResponse 目录分页结果。
type CatalogResponse struct {
	Items []CatalogEntry `json:"items"`
	Total int            `json:"total"`
}

// GetCatalog 全局价格目录：custom（精确名）覆盖 remote 覆盖 builtin，同名取最高层。
func (s *PricingAdminService) GetCatalog(search, source string, page, pageSize int) *CatalogResponse {
	// custom 层按精确模型名建索引（通配模式不进目录，在"自定义价格"页管理）
	customByName := map[string]*CustomModelPricing{}
	customSnapshot := s.custom.Snapshot()
	for i := range customSnapshot {
		entry := &customSnapshot[i]
		if !entry.Enabled {
			continue
		}
		for _, m := range entry.Models {
			if !strings.HasSuffix(m, "*") {
				customByName[strings.ToLower(m)] = entry
			}
		}
	}

	byName := map[string]CatalogEntry{}
	// remote
	for _, e := range s.pricing.CatalogEntries() {
		byName[e.Model] = CatalogEntry{
			Model:              e.Model,
			Source:             PricingSourceLiteLLM,
			BillingMode:        e.Mode,
			InputPerMTok:       e.InputCostPerToken * 1_000_000,
			OutputPerMTok:      e.OutputCostPerToken * 1_000_000,
			CacheWritePerMTok:  e.CacheWritePerToken * 1_000_000,
			CacheReadPerMTok:   e.CacheReadPerToken * 1_000_000,
			TokenPricingAbsent: e.TokenPricingAbsent,
		}
	}
	// builtin 兜底（remote 未覆盖的名字才补进来）
	for _, e := range s.billing.BuiltinFallbackEntries() {
		if _, ok := byName[e.Model]; ok {
			continue
		}
		byName[e.Model] = CatalogEntry{
			Model:              e.Model,
			Source:             PricingSourceFallback,
			InputPerMTok:       e.InputCostPerToken * 1_000_000,
			OutputPerMTok:      e.OutputCostPerToken * 1_000_000,
			CacheWritePerMTok:  e.CacheWritePerToken * 1_000_000,
			CacheReadPerMTok:   e.CacheReadPerToken * 1_000_000,
			TokenPricingAbsent: e.TokenPricingAbsent,
		}
	}
	// custom 最高优先级
	for lower, entry := range customByName {
		cat := CatalogEntry{
			Model:              lower,
			Source:             PricingSourceCustom,
			CustomID:           entry.ID,
			BillingMode:        string(entry.BillingMode),
			TokenPricingAbsent: entry.InputPrice == nil && entry.OutputPrice == nil,
		}
		if entry.InputPrice != nil {
			cat.InputPerMTok = *entry.InputPrice * 1_000_000
		}
		if entry.OutputPrice != nil {
			cat.OutputPerMTok = *entry.OutputPrice * 1_000_000
		}
		if entry.CacheWritePrice != nil {
			cat.CacheWritePerMTok = *entry.CacheWritePrice * 1_000_000
		}
		if entry.CacheReadPrice != nil {
			cat.CacheReadPerMTok = *entry.CacheReadPrice * 1_000_000
		}
		byName[lower] = cat
	}

	// 过滤 + 分页
	searchLower := strings.ToLower(strings.TrimSpace(search))
	sourceLower := strings.ToLower(strings.TrimSpace(source))
	items := make([]CatalogEntry, 0, len(byName))
	for _, entry := range byName {
		if searchLower != "" && !strings.Contains(strings.ToLower(entry.Model), searchLower) {
			continue
		}
		if sourceLower != "" && entry.Source != sourceLower {
			continue
		}
		items = append(items, entry)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Model < items[j].Model })

	total := len(items)
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 200 {
		pageSize = 50
	}
	start := (page - 1) * pageSize
	if start >= total {
		return &CatalogResponse{Items: []CatalogEntry{}, Total: total}
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	return &CatalogResponse{Items: items[start:end], Total: total}
}

// --- 未覆盖模型扫描 ---

// UncoveredEntry 未覆盖（或覆盖情况可疑）的已上线模型。
type UncoveredEntry struct {
	Model        string                `json:"model"`
	References   []string              `json:"references"`               // 引用来源：分组名 / usage
	Usage        *usagestats.ModelStat `json:"usage,omitempty"`          // 来自 usage 扫描时附用量
	ZeroCostOnly bool                  `json:"zero_cost_only,omitempty"` // 有 token 流量但实际扣费为 0
}

// UncoveredResponse 扫描结果。
type UncoveredResponse struct {
	Items     []UncoveredEntry `json:"items"`
	Scanned   int              `json:"scanned"` // 候选模型总数
	Window    string           `json:"window"`  // usage 扫描窗口，如 "720h"
	ScannedAt time.Time        `json:"scanned_at"`
}

// ScanUncovered 双通道扫描未覆盖模型：
//   - 配置通道：各分组 ModelsListConfig（启用时）与分组模型价格中声明的模型；
//   - 用量通道：近 windowDays 天 usage_logs 实际计费模型（对账标记 tokens>0 且 actual_cost=0）。
//
// 判定复用运行时查价链（分组价 → custom → 全局表含家族模糊匹配），杜绝误报。
func (s *PricingAdminService) ScanUncovered(ctx context.Context, windowDays int) (*UncoveredResponse, error) {
	if windowDays <= 0 {
		windowDays = 30
	}
	window := time.Duration(windowDays) * 24 * time.Hour

	type candidate struct {
		references []string
		usage      *usagestats.ModelStat
	}
	candidates := map[string]*candidate{}

	addRef := func(model, ref string) {
		model = strings.TrimSpace(model)
		if model == "" || strings.HasSuffix(model, "*") {
			return // 通配模式无法判定具体模型，跳过
		}
		c, ok := candidates[model]
		if !ok {
			c = &candidate{}
			candidates[model] = c
		}
		c.references = append(c.references, ref)
	}

	// 通道一：分组配置
	groups, err := s.groupService.ListActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	for i := range groups {
		g := &groups[i]
		if g.ModelsListConfig.Enabled {
			for _, m := range g.ModelsListConfig.Models {
				addRef(m, g.Name)
			}
		}
		for j := range g.ModelPricing {
			for _, m := range g.ModelPricing[j].Models {
				addRef(m, g.Name)
			}
		}
	}

	// 通道二：实际用量
	start := time.Now().Add(-window)
	stats, err := s.usageRepo.GetModelStatsWithFilters(ctx, start, time.Now(), 0, 0, 0, 0, nil, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("scan usage models: %w", err)
	}
	for i := range stats {
		st := stats[i]
		addRef(st.Model, "usage")
		if c, ok := candidates[st.Model]; ok {
			u := st
			c.usage = &u
		}
	}

	// 覆盖判定（复用运行时查价语义；分组列表只查一次）
	items := make([]UncoveredEntry, 0)
	for model, c := range candidates {
		if s.isModelCovered(groups, model) {
			continue
		}
		entry := UncoveredEntry{Model: model, References: c.references, Usage: c.usage}
		if c.usage != nil && c.usage.TotalTokens > 0 && c.usage.ActualCost == 0 {
			entry.ZeroCostOnly = true
		}
		items = append(items, entry)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].ZeroCostOnly != items[j].ZeroCostOnly {
			return items[i].ZeroCostOnly // 漏费风险排前
		}
		ti, tj := int64(0), int64(0)
		if items[i].Usage != nil {
			ti = items[i].Usage.TotalTokens
		}
		if items[j].Usage != nil {
			tj = items[j].Usage.TotalTokens
		}
		return ti > tj
	})

	return &UncoveredResponse{
		Items:     items,
		Scanned:   len(candidates),
		Window:    window.String(),
		ScannedAt: time.Now(),
	}, nil
}

// isModelCovered 判定模型在运行时链上是否可解析出 token 价格。
// 与网关计费一致：分组价 → custom → 全局表（LiteLLM + 内置兜底，含家族模糊匹配）。
func (s *PricingAdminService) isModelCovered(groups []Group, model string) bool {
	for i := range groups {
		if matchGroupModelPricing(&groups[i], model) != nil {
			return true
		}
	}
	if s.custom.MatchCustomModelPricing(model) != nil {
		return true
	}
	pricing, err := s.billing.GetModelPricing(model)
	return err == nil && pricing != nil
}

// --- 生效价试算 ---

// PreviewRequest 试算请求。
type PreviewRequest struct {
	Model   string `json:"model"`
	GroupID int64  `json:"group_id"`
}

// PreviewResponse 试算结果：解析链来源 + 生效单价 + 样例费用（1M 输入 / 1M 输出）。
type PreviewResponse struct {
	Model          string  `json:"model"`
	Source         string  `json:"source"`
	BillingMode    string  `json:"billing_mode"`
	GroupID        int64   `json:"group_id"`
	GroupName      string  `json:"group_name"`
	RateMultiplier float64 `json:"rate_multiplier"`

	InputPerMTok      float64 `json:"input_per_mtok"`
	OutputPerMTok     float64 `json:"output_per_mtok"`
	CacheWritePerMTok float64 `json:"cache_write_per_mtok"`
	CacheReadPerMTok  float64 `json:"cache_read_per_mtok"`

	SampleInputCost  float64 `json:"sample_input_cost"`
	SampleOutputCost float64 `json:"sample_output_cost"`
}

// GetPreview 生效价试算：完整解析链 + 分组倍率叠加后的最终单价。
func (s *PricingAdminService) GetPreview(ctx context.Context, model string, groupID int64) (*PreviewResponse, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, fmt.Errorf("model is required")
	}

	var group *Group
	if groupID > 0 {
		g, err := s.groupService.GetByID(ctx, groupID)
		if err != nil {
			return nil, fmt.Errorf("get group: %w", err)
		}
		group = g
	}

	input := PricingInput{Model: model}
	if group != nil {
		input.GroupID = &groupID
		input.Group = group
	}
	if s.resolver == nil {
		return nil, fmt.Errorf("pricing resolver unavailable")
	}
	resolved := s.resolver.Resolve(ctx, input)

	pricing := s.resolver.GetIntervalPricing(resolved, 1)
	if pricing == nil {
		return nil, fmt.Errorf("no pricing available for model: %s", model)
	}
	pricing = s.billing.ApplyModelSpecificPricingPolicy(model, pricing)

	multiplier := 1.0
	resp := &PreviewResponse{
		Model:       model,
		Source:      resolved.Source,
		BillingMode: string(resolved.Mode),
	}
	if group != nil {
		multiplier = group.RateMultiplier
		resp.GroupID = group.ID
		resp.GroupName = group.Name
		resp.RateMultiplier = multiplier
	}

	resp.InputPerMTok = pricing.InputPricePerToken * 1_000_000 * multiplier
	resp.OutputPerMTok = pricing.OutputPricePerToken * 1_000_000 * multiplier
	resp.CacheWritePerMTok = pricing.CacheCreationPricePerToken * 1_000_000 * multiplier
	resp.CacheReadPerMTok = pricing.CacheReadPricePerToken * 1_000_000 * multiplier
	resp.SampleInputCost = resp.InputPerMTok
	resp.SampleOutputCost = resp.OutputPerMTok
	return resp, nil
}
