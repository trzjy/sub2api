package service

import (
	"context"
	"strings"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// PlazaOfficialOverride 单模型官方参考价覆盖（USD per MTok）。
// 仅用于模型广场"官方参考价"列的展示，不参与计费；未覆盖的模型回落计费目录。
type PlazaOfficialOverride struct {
	InputPrice      float64 `json:"input_price"`
	OutputPrice     float64 `json:"output_price"`
	CacheReadPrice  float64 `json:"cache_read_price"`
	CacheWritePrice float64 `json:"cache_write_price"`
}

// PlazaOfficialOverrideEntry 覆盖列表条目（管理端展示/保存）。
type PlazaOfficialOverrideEntry struct {
	Model string                `json:"model"`
	Price PlazaOfficialOverride `json:"price"`
}

// plazaOverrideCacheTTL 官方参考价覆盖的缓存时长。
const plazaOverrideCacheTTL = time.Minute

var (
	plazaCachedOverridesMu  sync.Mutex
	plazaCachedOverrides    map[string]PlazaOfficialOverride
	plazaCachedOverridesAt  time.Time
)

// GetModelPlazaOfficialPricingOverrides 读取官方参考价覆盖（键为模型名，USD/MTok）。
func (s *SettingService) GetModelPlazaOfficialPricingOverrides(ctx context.Context) (map[string]PlazaOfficialOverride, error) {
	vals, err := s.settingRepo.GetMultiple(ctx, []string{SettingKeyModelPlazaOfficialPricing})
	if err != nil {
		return nil, fmt.Errorf("get model plaza official pricing: %w", err)
	}
	raw := vals[SettingKeyModelPlazaOfficialPricing]
	out := map[string]PlazaOfficialOverride{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("parse model plaza official pricing: %w", err)
	}
	return out, nil
}

// SaveModelPlazaOfficialPricingOverrides 全量保存官方参考价覆盖。
func (s *SettingService) SaveModelPlazaOfficialPricingOverrides(ctx context.Context, overrides map[string]PlazaOfficialOverride) error {
	if overrides == nil {
		overrides = map[string]PlazaOfficialOverride{}
	}
	data, err := json.Marshal(overrides)
	if err != nil {
		return fmt.Errorf("marshal model plaza official pricing: %w", err)
	}
	return s.settingRepo.SetMultiple(ctx, map[string]string{
		SettingKeyModelPlazaOfficialPricing: string(data),
	})
}

// sortedPlazaOverrideModels 返回排序后的覆盖模型名（稳定输出）。
func sortedPlazaOverrideModels(overrides map[string]PlazaOfficialOverride) []string {
	names := make([]string, 0, len(overrides))
	for name := range overrides {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ListPlazaOverrideEntries 排序后的覆盖列表（管理端展示）。
func ListPlazaOverrideEntries(overrides map[string]PlazaOfficialOverride) []PlazaOfficialOverrideEntry {
	entries := make([]PlazaOfficialOverrideEntry, 0, len(overrides))
	for _, name := range sortedPlazaOverrideModels(overrides) {
		entries = append(entries, PlazaOfficialOverrideEntry{Model: name, Price: overrides[name]})
	}
	return entries
}

// cachedOfficialOverrides 广场热路径用的覆盖缓存（60s TTL，读失败沿用旧值）。
func (s *ModelPlazaService) cachedOfficialOverrides(ctx context.Context) map[string]PlazaOfficialOverride {
	if s == nil || s.settingService == nil {
		return nil
	}
	plazaCachedOverridesMu.Lock()
	defer plazaCachedOverridesMu.Unlock()
	if plazaCachedOverrides != nil && time.Since(plazaCachedOverridesAt) < plazaOverrideCacheTTL {
		return plazaCachedOverrides
	}
	overrides, err := s.settingService.GetModelPlazaOfficialPricingOverrides(ctx)
	if err != nil {
		return plazaCachedOverrides
	}
	plazaCachedOverrides = overrides
	plazaCachedOverridesAt = time.Now()
	return plazaCachedOverrides
}

// GetPlazaOfficialPricingOverrides 管理端读取官方参考价覆盖。
func (s *ModelPlazaService) GetPlazaOfficialPricingOverrides(ctx context.Context) (map[string]PlazaOfficialOverride, error) {
	return s.settingService.GetModelPlazaOfficialPricingOverrides(ctx)
}

// SavePlazaOfficialPricingOverrides 管理端保存官方参考价覆盖并失效缓存。
func (s *ModelPlazaService) SavePlazaOfficialPricingOverrides(ctx context.Context, overrides map[string]PlazaOfficialOverride) error {
	if s == nil || s.settingService == nil {
		return fmt.Errorf("model plaza settings unavailable")
	}
	if err := s.settingService.SaveModelPlazaOfficialPricingOverrides(ctx, overrides); err != nil {
		return err
	}
	s.InvalidatePlazaOverrideCache()
	return nil
}

// InvalidatePlazaOverrideCache 管理端保存后即时失效缓存。
func (s *ModelPlazaService) InvalidatePlazaOverrideCache() {
	plazaCachedOverridesMu.Lock()
	plazaCachedOverrides = nil
	plazaCachedOverridesAt = time.Time{}
	plazaCachedOverridesMu.Unlock()
}
