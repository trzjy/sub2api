package service

import (
	"encoding/json"
	"sort"
	"strings"
)

// codeBuddyUpstreamCatalogBody 把 CodeBuddy 动态模型列表（FetchModels 结果）
// 映射为通用上游目录报文（{"data":[{id,name,reasoning,...,context_window,
// max_output_tokens}]}），从而复用 extractUpstreamModelCatalog 的能力元数据解析
// 与 SyncUpstreamModelCatalog 的清单快照落库（F5）。
//
// 返回映射后的报文与「启用模型」的 ID 列表（字典序，去重）。disabled 模型不参与
// 聚合路由，直接剔除；MaxInputTokens 缺失时以 MaxOutputTokens 兜底，保证元数据
// 完整、快照不被完整性过滤丢弃。
func codeBuddyUpstreamCatalogBody(models []CodeBuddyModel) ([]byte, []string, error) {
	type catalogEntry struct {
		ID                       string   `json:"id"`
		Name                     string   `json:"name,omitempty"`
		Reasoning                bool     `json:"reasoning"`
		SupportedReasoningLevels []string `json:"supported_reasoning_levels,omitempty"`
		DefaultReasoningLevel    string   `json:"default_reasoning_level,omitempty"`
		InputModalities          []string `json:"input_modalities"`
		ContextWindow            int64    `json:"context_window,omitempty"`
		MaxOutputTokens          int64    `json:"max_output_tokens,omitempty"`
	}

	payload := struct {
		Data []catalogEntry `json:"data"`
	}{Data: make([]catalogEntry, 0, len(models))}
	ids := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))

	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" || model.Disabled {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}

		contextWindow := model.MaxInputTokens
		if contextWindow <= 0 {
			contextWindow = model.MaxOutputTokens
		}
		entry := catalogEntry{
			ID:              id,
			Name:            strings.TrimSpace(model.Name),
			InputModalities: []string{"text"},
			ContextWindow:   contextWindow,
			MaxOutputTokens: model.MaxOutputTokens,
		}
		// reasoning efforts 直接对号；仅 "none" 一档视为无推理（与目录解析语义一致）。
		if levels := normalizeReasoningLevels(model.EffortLevels()); len(levels) > 0 {
			entry.Reasoning = len(levels) != 1 || levels[0] != "none"
			if entry.Reasoning {
				entry.SupportedReasoningLevels = levels
				entry.DefaultReasoningLevel = levels[0]
			}
		}

		payload.Data = append(payload.Data, entry)
		ids = append(ids, id)
	}

	sort.Strings(ids)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}
	return body, ids, nil
}
