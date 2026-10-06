package service

import (
	"regexp"
	"sort"
	"strings"
)

// upstreamPlatformModelPatterns 平台 → 上游模型 ID 词根匹配表。
//
// 火山 Coding/Agent Plan 官方套餐天然聚合多厂商模型（deepseek/glm/kimi/doubao/
// minimax…），订阅号同步在写入 model_mapping 前按账号平台过滤跨平台模型。词根与
// 前端 frontend/src/constants/platforms.ts 的 PLATFORM_MODEL_PATTERNS 保持同源，
// 两处新增平台必须同步修改。火山模型 ID 为官方裸名（deepseek-v4-flash），聚合上游
// 可能带厂商前缀，因此按子串匹配；openai 的 o 系列加词边界避免误伤同类命名。
var upstreamPlatformModelPatterns = map[string]*regexp.Regexp{
	"anthropic":   regexp.MustCompile(`claude`),
	"openai":      regexp.MustCompile(`gpt|chatgpt|codex|text-embedding|whisper|dall-e|davinci|(?:^|[^a-z0-9])o[134](?:[^0-9]|$)`),
	"gemini":      regexp.MustCompile(`gemini`),
	"antigravity": regexp.MustCompile(`gemini`),
	"grok":        regexp.MustCompile(`grok`),
	"kimi":        regexp.MustCompile(`kimi|moonshot`),
	"zhipu":       regexp.MustCompile(`glm|zhipu|chatglm|bigmodel`),
	"deepseek":    regexp.MustCompile(`deepseek`),
	"minimax":     regexp.MustCompile(`minimax|abab`),
}

// modelMatchesUpstreamPlatform 判断上游模型 ID 是否属于平台模型族。
// 平台无词根定义（other/未知/空）时一律视为匹配（不过滤）。
func modelMatchesUpstreamPlatform(modelID, platform string) bool {
	pattern := upstreamPlatformModelPatterns[strings.ToLower(strings.TrimSpace(platform))]
	if pattern == nil {
		return true
	}
	return pattern.MatchString(modelID)
}

// filterModelsForPlatform 按平台过滤模型 ID 列表，返回 (保留, 被平台过滤, 是否启用过滤)。
// 平台无词根定义时原样返回且 filtered 为 nil。kept 保序；filtered 按字典序排列，
// 便于直接落库展示。
func filterModelsForPlatform(models []string, platform string) (kept, filtered []string, applied bool) {
	pattern := upstreamPlatformModelPatterns[strings.ToLower(strings.TrimSpace(platform))]
	if pattern == nil {
		return models, nil, false
	}
	kept = make([]string, 0, len(models))
	filtered = []string{}
	for _, m := range models {
		if pattern.MatchString(m) {
			kept = append(kept, m)
		} else {
			filtered = append(filtered, m)
		}
	}
	sort.Strings(filtered)
	return kept, filtered, true
}
