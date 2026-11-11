package service

import "strings"

// 模型名别名族（2026-11-04 用户裁定）。
//
// 背景：客户端与账号映射对同一模型的写法不一致，导致明明有账号能服务，却被判成
// 「不在当前分组支持范围」。线上实测的写法分歧有两类：
//
//  1. Claude 家族版本号用点还是横线：`claude-fable-5.1`（75 个账号）与
//     `claude-fable-5-1`（15 个账号）并存，白名单里两个写法也都有。
//  2. DeepSeek 家族带不带尾部版本后缀：`deepseek-v4.1-flash`（101 个账号）与
//     `deepseek-v4.1-flash-1017`（26 个账号）并存；`deepseek-v4-pro` 与
//     `deepseek-v4-pro-0813` 同理。
//
// 用户裁定：
//   - Claude 版本号统一用 `-`，不用 `.`；
//   - DeepSeek 去掉尾部版本后缀，归到 `deepseek-v4-pro` / `deepseek-v4.1-flash`；
//   - `claude-sonnet-5`（缺版本段）一并归到 `claude-sonnet-5-5`。
//
// 关键设计：**只归一"用于查找的名字"，绝不改发给上游的 model**。
// 账号映射里配的上游真名（例如 `deepseek-v4.1-flash-1017`、`hy3`、
// `sensenova-6.8-flash-lite`）一个字都不动，也不做任何数据库改写。原因：
// 实测有 14 个账号同时配了 `deepseek-v4.1-flash` 与 `deepseek-v4.1-flash-1017`
// 两个键、10 个账号同时配了 `deepseek-v4-pro` 与 `deepseek-v4-pro-0813`，
// 其中部分账号两个键指向**不同的上游模型**。若在数据层合并键，会丢掉一条映射，
// 那几个账号会直接断供。所以在查找层做族匹配，等价效果、零数据风险。
//
// 明确不纳入别名的（避免改错）：
//   - Anthropic 官方日期快照名（`claude-opus-4-20250514`、`claude-sonnet-4-5-20250929`
//     等）：日期是名字的一部分，削掉就调不通上游；
//   - 参数后缀（`-thinking`、`-high/-low/-medium`、`[1M]`）：这是推理强度与长上下文
//     标记，已有单独处理路径，不是版本号；
//   - `claude-opus-5` 与 `claude-opus-5-5`：两者都有大量账号在服务（16 / 78），
//     是不同模型，不能合并（与 `claude-fable-5` / `claude-fable-5-1` 同理由，
//     用户已确认这两个也是不同模型）；
//   - `deepseek-v4-flash`（13 个账号）：v4 那一代，与 v4.1 不是同一模型；
//   - `deepseek-chat` / `deepseek-reasoner` / `deepseek-3.2`：官方通用名与老模型。
type modelAliasFamily struct {
	// canonical 是对外统一写法，也是查找时的首选定名。
	canonical string
	// aliases 是族内其它已确认等价的历史写法。
	aliases []string
}

var modelAliasFamilies = []modelAliasFamily{
	// Claude：版本号点号 → 横线（用户裁定：统一用 -）。
	{canonical: "claude-fable-5-1", aliases: []string{"claude-fable-5.1"}},
	{canonical: "claude-haiku-5-5", aliases: []string{"claude-haiku-5.5"}},
	// Claude：缺版本段（用户裁定：claude-sonnet-5 一并归到 claude-sonnet-5-5）。
	{canonical: "claude-sonnet-5-5", aliases: []string{"claude-sonnet-5"}},
	// Claude：缺分隔横线的写法。
	{canonical: "claude-fable-5", aliases: []string{"claude-fable5"}},
	// DeepSeek：去掉尾部版本后缀（用户裁定）；并入官方现行调用名。
	// 官方文档（api-docs.deepseek.com/quick_start/pricing，2026-11-05 核对）现只有
	// `deepseek-flash` 与 `deepseek-v4-pro` 两个调用名，`DeepSeek-V4.1-Flash-1017`
	// 是前者的版本号而非模型名。此处把官方名并进同一族：老写法继续可用，
	// 新写法直接命中既有账号映射键，出站名不受影响。
	{canonical: "deepseek-v4.1-flash", aliases: []string{
		"deepseek-v4.1-flash-1017",
		"deepseek-v4-1-flash",
		"deepseek-flash",
	}},
	{canonical: "deepseek-v4-pro", aliases: []string{
		"deepseek-v4-pro-0813",
		"deepseek-v4-pro-ga-260813",
	}},
}

// NormalizeModelName 返回模型名的对外统一写法；不在别名族内时原样返回。
//
// 用途：展示、比对、日志归档。**不要用它去替换发给上游的 model**。
func NormalizeModelName(name string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return ""
	}
	if family, ok := findModelAliasFamily(trimmed); ok {
		return family.canonical
	}
	return trimmed
}

// 对外展示名（用户裁定 2026-11-05）：族内旧写法对下游一律呈现为这里配置的
// 官方现行调用名。键是站内统一写法（NormalizeModelName 的结果），值是对外名。
// 只影响「下游看得见的列表与回显」，不影响准入判定、账号映射键与出站名。
//
// 2026-11-12 用户裁定补充：官方现行调用名只有 `deepseek-flash` 与 `deepseek-v4-pro`
// 两个（api-docs.deepseek.com/quick_start/pricing 核对：DeepSeek-V4.1-Flash-1017 与
// DeepSeek-V4-Pro-0813 是**版本号**，不是模型名）。故 pro 族也配对外名，
// 下游不再看到 `deepseek-v4-pro-0813` 这类带版本后缀的写法。
var publicModelNames = map[string]string{
	"deepseek-v4.1-flash": "deepseek-flash",
	"deepseek-v4-pro":     "deepseek-v4-pro",
}

// PublicModelName 返回对下游展示用的模型名。
// 族内旧写法归到对外名，族外/未配置的名字返回站内统一写法（即原行为）。
func PublicModelName(name string) string {
	normalized := NormalizeModelName(name)
	if normalized == "" {
		return ""
	}
	if public, ok := publicModelNames[strings.ToLower(normalized)]; ok {
		return public
	}
	return normalized
}

// internalModelNameForPublic 是 PublicModelName 的反向查找：
// 对外名 → 站内统一写法。列表层用它回查模型目录（目录键仍是站内写法）。
func internalModelNameForPublic(name string) (string, bool) {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return "", false
	}
	for internal, public := range publicModelNames {
		if strings.EqualFold(public, lower) {
			return internal, true
		}
	}
	return "", false
}

// responseModelName 决定"回显给客户端的模型名"。
//
// 与 NormalizeModelName 的区别：已配置对外展示名的族（deepseek）保留客户端自己的
// 书写（写官方名回官方名、写旧名回旧名），不再折回站内统一写法——这样新老用户
// 都能无缝更迭（用户裁定 2026-11-05）。已下线的名字一律折回，绝不外露。
// 未配置对外展示名的族（claude 等）维持原有归一行为，零变化。
func responseModelName(name string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return ""
	}
	normalized := NormalizeModelName(trimmed)
	if IsRetiredModelName(trimmed) {
		return normalized
	}
	if _, configured := publicModelNames[strings.ToLower(normalized)]; configured {
		return trimmed
	}
	return normalized
}

// responseModelForClient 在已知"客户端请求名"时决定回显名：
// 请求名与上游回显同族就用请求名，否则退回 responseModelName 的规则。
// 跨族映射（如 flash 被路由到别家模型）因此仍如实显示上游真实模型。
func responseModelForClient(echoed, requestedModel string) string {
	requested := strings.TrimSpace(requestedModel)
	if requested != "" && !IsRetiredModelName(requested) && SameModelNameFamily(echoed, requested) {
		return requested
	}
	return responseModelName(echoed)
}

// ModelNameAliases 返回该模型名所在别名族的全部写法，输入本身排第一。
// 不在族内时只返回输入本身（去空白后），调用方可安全地逐个尝试。
//
// 顺序约定：输入 → 统一写法 → 其它历史写法。查找时按此顺序命中，保证
// 「账号里配了客户端原写法」时优先用它，不被统一写法抢走。
func ModelNameAliases(name string) []string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return nil
	}
	family, ok := findModelAliasFamily(trimmed)
	if !ok {
		return []string{trimmed}
	}
	out := make([]string, 0, len(family.aliases)+2)
	out = append(out, trimmed)
	appendUniqueAlias := func(value string) {
		if value == "" {
			return
		}
		for _, existing := range out {
			if strings.EqualFold(existing, value) {
				return
			}
		}
		out = append(out, value)
	}
	appendUniqueAlias(family.canonical)
	for _, alias := range family.aliases {
		appendUniqueAlias(alias)
	}
	return out
}

// SameModelNameFamily 判断两个模型名是否属于同一别名族（大小写不敏感）。
// 族外名字一律返回 false——不做模糊匹配，避免把不同模型误判成同一个。
func SameModelNameFamily(a, b string) bool {
	familyA, okA := findModelAliasFamily(strings.TrimSpace(a))
	familyB, okB := findModelAliasFamily(strings.TrimSpace(b))
	if !okA || !okB {
		return false
	}
	return strings.EqualFold(familyA.canonical, familyB.canonical)
}

// findModelAliasFamily 按大小写不敏感查找所属别名族。
func findModelAliasFamily(name string) (modelAliasFamily, bool) {
	for _, family := range modelAliasFamilies {
		if strings.EqualFold(name, family.canonical) {
			return family, true
		}
		for _, alias := range family.aliases {
			if strings.EqualFold(name, alias) {
				return family, true
			}
		}
	}
	return modelAliasFamily{}, false
}
