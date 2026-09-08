package service

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

// PricingAdminService 价格管理中心服务：
// 聚合同步状态、全局价格目录（来源分层）、未覆盖模型双通道扫描与生效价试算。
type PricingAdminService struct {
	pricing        *PricingService
	billing        *BillingService
	custom         *CustomModelPricingService
	resolver       *ModelPricingResolver
	groupService   *GroupService
	channelService *ChannelService
	usageRepo      UsageLogRepository

	// 在用模型候选集缓存（目录视图用）
	scanMu       sync.Mutex
	scanCache    []string
	scanCachedAt time.Time
}

func NewPricingAdminService(
	pricing *PricingService,
	billing *BillingService,
	custom *CustomModelPricingService,
	resolver *ModelPricingResolver,
	groupService *GroupService,
	channelService *ChannelService,
	usageRepo UsageLogRepository,
) *PricingAdminService {
	return &PricingAdminService{
		pricing:        pricing,
		billing:        billing,
		custom:         custom,
		resolver:       resolver,
		groupService:   groupService,
		channelService: channelService,
		usageRepo:      usageRepo,
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

// 目录来源补充标识（与 PricingSource* 并列，仅目录视图使用）。
const (
	// CatalogSourceFuzzy 无确切条目、仅靠系列/子串兜底匹配出近似价的在用模型。
	CatalogSourceFuzzy = "fuzzy"
	// CatalogSourceNone 完全无价的在用模型（按 $0 记账）。
	CatalogSourceNone = "none"
)

// CatalogEntry 价格目录条目：单一模型名的生效价。
// 来源除四层（custom/remote/builtin/group）外，还有在用模型的两种补齐档：
// fuzzy（无确切条目、按系列/子串兜底计价）与 none（完全无价）。
type CatalogEntry struct {
	Model              string  `json:"model"`
	Source             string  `json:"source"`
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

// catalogScanCacheTTL 在用模型候选集的缓存时长：目录搜索走缓存，
// 避免每次搜索都聚合 usage_logs。
const catalogScanCacheTTL = time.Minute

// cachedInUseModels 返回在用/已声明模型名列表（TTL 缓存）。
func (s *PricingAdminService) cachedInUseModels(ctx context.Context) []string {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	if s.scanCachedAt.IsZero() || time.Since(s.scanCachedAt) > catalogScanCacheTTL {
		candidates, _, err := s.collectScanCandidates(ctx, 30)
		if err != nil {
			// 收集失败时沿用旧缓存（如有），不让目录搜索直接报错
			slog.Warn("pricing catalog in-use scan failed", "error", err)
		} else {
			names := make([]string, 0, len(candidates))
			for model := range candidates {
				names = append(names, model)
			}
			sort.Strings(names)
			s.scanCache = names
			s.scanCachedAt = time.Now()
		}
	}
	return s.scanCache
}

// GetCatalog 全局价格目录：custom（精确名）覆盖 remote 覆盖 builtin，同名取最高层；
// 在用/已声明但无确切条目的模型以 fuzzy/none 来源补齐，保证目录覆盖所有模型。
func (s *PricingAdminService) GetCatalog(ctx context.Context, search, source string, page, pageSize int) *CatalogResponse {
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

	// 在用/已声明模型（usage + 分组声明 + 渠道模型）：凡是不在上面任何层以确切
	// 条目存在的名字也进目录，展示它实际被计费的价格档——
	//   fuzzy：仅靠系列/子串兜底匹配出近似价（如 glm-5.3 → glm-5 兜底价）；
	//   none：完全无价，按 $0 记账。
	// 候选集走 TTL 缓存，避免每次搜索都扫 usage_logs。
	for _, model := range s.cachedInUseModels(ctx) {
		lower := strings.ToLower(model)
		if _, ok := byName[lower]; ok {
			continue
		}
		p, err := s.billing.GetModelPricing(model)
		if err != nil || p == nil {
			byName[lower] = CatalogEntry{Model: lower, Source: CatalogSourceNone}
			continue
		}
		source := CatalogSourceFuzzy
		if s.billing.HasIdentifiedTokenPricing(model) {
			// 不在全局键名清单里却能确定性识别（如远程表对带日期后缀键的去后缀
			// 识别）——按识别来源标注
			source = PricingSourceLiteLLM
		}
		byName[lower] = CatalogEntry{
			Model:             lower,
			Source:            source,
			InputPerMTok:      p.InputPricePerToken * 1_000_000,
			OutputPerMTok:     p.OutputPricePerToken * 1_000_000,
			CacheWritePerMTok: p.CacheCreationPricePerToken * 1_000_000,
			CacheReadPerMTok:  p.CacheReadPricePerToken * 1_000_000,
		}
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

// 覆盖判定结论。
const (
	// VerdictUncovered 无任何价格可循：请求按 $0 记账，存在漏费。
	VerdictUncovered = "uncovered"
	// VerdictFuzzy 仅能通过系列/子串兜底匹配出近似价（如 glm-5.3 → glm-5 兜底价）：
	// 有计费但可能偏离真实定价，列出供管理员审查补价。
	VerdictFuzzy = "fuzzy"
)

// UncoveredEntry 覆盖情况需要关注的已上线模型。
type UncoveredEntry struct {
	Model         string                `json:"model"`
	Verdict       string                `json:"verdict"`                   // uncovered / fuzzy
	References    []string              `json:"references"`                // 引用来源：分组名 / 渠道名 / usage
	Usage         *usagestats.ModelStat `json:"usage,omitempty"`           // 来自 usage 扫描时附用量
	ZeroCostOnly  bool                  `json:"zero_cost_only,omitempty"`  // 有 token 流量但实际扣费为 0
	InputPerMTok  float64               `json:"input_per_mtok,omitempty"`  // fuzzy：当前按近似价计费的输入单价
	OutputPerMTok float64               `json:"output_per_mtok,omitempty"` // fuzzy：近似输出单价
}

// UncoveredResponse 扫描结果。
type UncoveredResponse struct {
	Items     []UncoveredEntry `json:"items"`
	Scanned   int              `json:"scanned"` // 候选模型总数
	Window    string           `json:"window"`  // usage 扫描窗口，如 "720h"
	ScannedAt time.Time        `json:"scanned_at"`
}

// scanCandidate 扫描候选：一个模型名 + 引用来源 + 近窗用量。
type scanCandidate struct {
	references []string
	usage      *usagestats.ModelStat
}

// collectScanCandidates 多通道收集候选模型：
//   - 配置通道：各分组 ModelsListConfig（启用时）、分组模型价格、渠道 SupportedModels
//     （模型映射 ∪ 渠道定价，含零调用的已配置模型）；
//   - 用量通道：近 windowDays 天 usage_logs 实际计费模型。
func (s *PricingAdminService) collectScanCandidates(ctx context.Context, windowDays int) (map[string]*scanCandidate, []Group, error) {
	if windowDays <= 0 {
		windowDays = 30
	}
	window := time.Duration(windowDays) * 24 * time.Hour

	candidates := map[string]*scanCandidate{}
	addRef := func(model, ref string) {
		model = strings.TrimSpace(model)
		if model == "" || strings.HasSuffix(model, "*") {
			return // 通配模式无法判定具体模型，跳过
		}
		c, ok := candidates[model]
		if !ok {
			c = &scanCandidate{}
			candidates[model] = c
		}
		c.references = append(c.references, ref)
	}

	// 通道一：分组配置
	var groups []Group
	if s.groupService != nil {
		var err error
		groups, err = s.groupService.ListActive(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("list groups: %w", err)
		}
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

	// 通道一（续）：渠道 SupportedModels（模型映射 ∪ 渠道定价），覆盖零调用的已配置模型
	if s.channelService != nil {
		channels, _, err := s.channelService.List(ctx, pagination.PaginationParams{Page: 1, PageSize: 500}, "", "")
		if err != nil {
			return nil, nil, fmt.Errorf("list channels: %w", err)
		}
		for i := range channels {
			ch := &channels[i]
			for _, sm := range ch.SupportedModels() {
				addRef(sm.Name, "渠道 "+ch.Name)
			}
		}
	}

	// 通道二：实际用量
	start := time.Now().Add(-window)
	if s.usageRepo != nil {
		stats, err := s.usageRepo.GetModelStatsWithFilters(ctx, start, time.Now(), 0, 0, 0, 0, nil, nil, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("scan usage models: %w", err)
		}
		for i := range stats {
			st := stats[i]
			addRef(st.Model, "usage")
			if c, ok := candidates[st.Model]; ok {
				u := st
				c.usage = &u
			}
		}
	}
	return candidates, groups, nil
}

// ScanUncovered 多通道扫描覆盖情况存疑的已上线模型，判定区分三档：
//  1. 精确覆盖（分组价 / 自定义价 / 价格表确定性识别出确切型号）→ 不列出；
//  2. 模糊覆盖（仅能按系列/子串兜底匹配出近似价，如 glm-5.3 → glm-5 兜底价）→ 列出，verdict=fuzzy；
//  3. 无价可循（按 $0 记账）→ 列出，verdict=uncovered。
func (s *PricingAdminService) ScanUncovered(ctx context.Context, windowDays int) (*UncoveredResponse, error) {
	if windowDays <= 0 {
		windowDays = 30
	}
	window := time.Duration(windowDays) * 24 * time.Hour

	candidates, groups, err := s.collectScanCandidates(ctx, windowDays)
	if err != nil {
		return nil, err
	}

	// 覆盖判定（分组列表只查一次）：精确覆盖跳过，模糊/无价列出
	items := make([]UncoveredEntry, 0)
	for model, c := range candidates {
		entry, include := s.judgeCoverage(groups, model, c)
		if include {
			items = append(items, entry)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		// 未覆盖（$0 漏费）优先于模糊覆盖；同类内按用量降序
		pi, pj := items[i].Verdict == VerdictUncovered, items[j].Verdict == VerdictUncovered
		if pi != pj {
			return pi
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

// judgeCoverage 判定单个模型的覆盖档位。返回 (条目, 是否需要列出)。
func (s *PricingAdminService) judgeCoverage(groups []Group, model string, c *scanCandidate) (UncoveredEntry, bool) {
	entry := UncoveredEntry{Model: model, References: c.references, Usage: c.usage}

	// 1. 管理员显式配置的分组价 → 精确覆盖
	for i := range groups {
		if matchGroupModelPricing(&groups[i], model) != nil {
			return entry, false
		}
	}
	// 2. 自定义价格层 → 精确覆盖
	if s.custom.MatchCustomModelPricing(model) != nil {
		return entry, false
	}
	// 3. 价格表确定性识别（远程表确切型号 / 代码内置确切型号，拒绝子串猜系列）→ 精确覆盖
	if s.billing.HasIdentifiedTokenPricing(model) {
		return entry, false
	}
	// 4. 仅能按系列/子串兜底匹配出近似价 → 模糊覆盖，列出供审查
	if pricing, err := s.billing.GetModelPricing(model); err == nil && pricing != nil {
		entry.Verdict = VerdictFuzzy
		entry.InputPerMTok = pricing.InputPricePerToken * 1_000_000
		entry.OutputPerMTok = pricing.OutputPricePerToken * 1_000_000
		if c.usage != nil && c.usage.TotalTokens > 0 && c.usage.ActualCost == 0 {
			entry.ZeroCostOnly = true
		}
		return entry, true
	}
	// 5. 无价可循 → 未覆盖
	entry.Verdict = VerdictUncovered
	if c.usage != nil && c.usage.TotalTokens > 0 && c.usage.ActualCost == 0 {
		entry.ZeroCostOnly = true
	}
	return entry, true
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
		// 查无此价是正常的查询结果（而非服务端错误）：结构化返回无价档，
		// 前端据此提示"按 $0 计费"，而非 internal error。
		resp := &PreviewResponse{
			Model:       model,
			Source:      CatalogSourceNone,
			BillingMode: string(BillingModeToken),
		}
		if group != nil {
			resp.GroupID = group.ID
			resp.GroupName = group.Name
			resp.RateMultiplier = group.RateMultiplier
		}
		return resp, nil
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
