package service

import (
	"net/http"
	"slices"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

// GroupModelAllowlist 是 service 层的分组模型白名单（与 domain.GroupModelAllowlist
// 字段一致，ent 持久化用 domain 类型，边界处显式转换）。
type GroupModelAllowlist struct {
	Enabled bool     `json:"enabled"`
	Models  []string `json:"models,omitempty"`
}

// retiredModelNames 是对下游下线的模型名（2026-11-05 用户裁定）。
//
// `deepseek-v4.1-flash-1017` 只是上游真名，产品上对下游只提供
// `deepseek-v4.1-flash`：账号映射负责把下游标准名翻译成上游真名
// （`deepseek-v4.1-flash` → `deepseek-v4.1-flash-1017`），响应侧把上游真名
// 归一回标准名。别名族（model_name_alias.go）保留该别名用于映射查找与响应归一
// ——那些链路只读名字，不构成"对下游提供"。
//
// 因此本名单只作用于**下游入口**：白名单准入（Allows）与模型列表（FilterForListing）。
// 不删别名族，是因为它同时被账号映射查找与响应归一复用，删掉会让存量账号的
// 1017 映射键直接失效。
var retiredModelNames = map[string]struct{}{
	"deepseek-v4.1-flash-1017": {},
	// 2026-11-05 二次裁定：`deepseek-v4-flash` 是官方已退役的 legacy 名（模型已停，
	// 由 V4.1-Flash-1017 承接）。爹要求「哪怕上游还提供，也不再请求、从清单永久删除」。
	// 实测：全站近 7 天零调用、分组 60 零 key，下线零影响。
	"deepseek-v4-flash": {},
	// 2026-11-05 三次裁定：视觉变体也一并停止提供。实测近 7 天零调用、无分组白名单。
	// 注意：账号映射里的上游侧配置不动（爹明确「对上游的请求不变，只改下游出口」）。
	"deepseek-v4-flash-vision-exp": {},
}

// IsRetiredModelName 报告该模型名是否已对下游下线。大小写不敏感。
func IsRetiredModelName(name string) bool {
	_, ok := retiredModelNames[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

// filterRetiredModelNames 去掉列表里的已下线模型名（保持原顺序）。
func filterRetiredModelNames(models []string) []string {
	if len(models) == 0 {
		return models
	}
	out := make([]string, 0, len(models))
	for _, model := range models {
		if IsRetiredModelName(model) {
			continue
		}
		out = append(out, model)
	}
	return out
}

// presentModelNamesForListing 生成"下游可见"的模型名列表：
// 先剔除已下线的名字，再把族内旧写法统一换成对外展示名（PublicModelName），
// 最后按小写去重保持原顺序。列表是下游唯一能看见模型名的入口，所以新老更迭
// 只在这里收口：老写法继续可调，但新用户从列表里只会拿到官方现行调用名。
//
// **唯一事实源**：分组「实供候选」侧（admin_group_model_candidates.go 的
// collectGroupModelAllowlistCandidates）也必须走本函数，不得自建第二套归一逻辑
// ——否则会出现"已保存规则显示 deepseek-flash、实供候选却显示
// deepseek-v4.1-flash-1017"的口径分裂。
func presentModelNamesForListing(models []string) []string {
	if len(models) == 0 {
		return models
	}
	out := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range filterRetiredModelNames(models) {
		presented := PublicModelName(model)
		if presented == "" {
			continue
		}
		key := strings.ToLower(presented)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, presented)
	}
	return out
}

// DomainGroupModelAllowlist 把 service 白名单转换为 ent 持久化使用的 domain 类型。
func DomainGroupModelAllowlist(cfg GroupModelAllowlist) domain.GroupModelAllowlist {
	return domain.GroupModelAllowlist{Enabled: cfg.Enabled, Models: cfg.Models}
}

// GroupModelAllowlistFromDomain 把 ent 读出的 domain 白名单转换为 service 类型。
func GroupModelAllowlistFromDomain(cfg domain.GroupModelAllowlist) GroupModelAllowlist {
	return GroupModelAllowlist{Enabled: cfg.Enabled, Models: cfg.Models}
}

// supplementUnmappedOpenAIModels ensures a partial mapping catalog does not
// hide models from unmapped or passthrough OpenAI accounts (passthrough routing
// ignores model_mapping, so it serves the same default set as an unmapped
// account). An empty catalog is left unchanged so callers retain their existing
// discovery fallback.
func supplementUnmappedOpenAIModels(accounts []Account, models []string) []string {
	if len(models) == 0 {
		return models
	}
	for i := range accounts {
		account := &accounts[i]
		if account.Platform == PlatformOpenAI && (account.IsOpenAIPassthroughEnabled() || len(account.GetModelMapping()) == 0) {
			return dedupeAndSortModelIDs(slices.Concat(models, openai.DefaultModelIDs()))
		}
	}
	return models
}

// normalizeGroupModelAllowlist 归一化管理端提交的分组模型白名单：
// 条目 TrimSpace、按小写去重保序；`*` 可出现在任意位置；
// enabled=true 且列表为空视为配置错误，返回 400 而不是运行时静默放行/拒绝。
func normalizeGroupModelAllowlist(cfg GroupModelAllowlist) (GroupModelAllowlist, error) {
	out := GroupModelAllowlist{Enabled: cfg.Enabled}
	if len(cfg.Models) == 0 {
		if out.Enabled {
			return out, infraerrors.New(http.StatusBadRequest, "INVALID_MODEL_ALLOWLIST", "model allowlist cannot be enabled with an empty model list")
		}
		return out, nil
	}

	seen := make(map[string]struct{}, len(cfg.Models))
	out.Models = make([]string, 0, len(cfg.Models))
	for _, model := range cfg.Models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		key := strings.ToLower(model)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out.Models = append(out.Models, model)
	}
	if len(out.Models) == 0 {
		if out.Enabled {
			return out, infraerrors.New(http.StatusBadRequest, "INVALID_MODEL_ALLOWLIST", "model allowlist cannot be enabled with an empty model list")
		}
		out.Models = nil
	}
	return out, nil
}

// ModelAllowlistEnabled 报告该分组是否启用了模型白名单。
// 开启后所有携带模型的网关请求与模型列表接口都受白名单约束。
func (g *Group) ModelAllowlistEnabled() bool {
	return g != nil && g.ModelAllowlist.Enabled
}

// Allows 判断客户端请求的模型是否命中白名单。
// 准入只看客户端书写的模型名，与账号映射、渠道映射、合成路由改写无关；
// 候选形式覆盖代码中已有的模型名等价规则（Gemini models/ 前缀、
// Antigravity/Claude -thinking 宽容规则、OpenAI 推理后缀），不做模糊匹配。
func (a GroupModelAllowlist) Allows(model string) bool {
	// 已对下游下线的模型名一律拒绝：白名单未开启也要拦（见 retiredModelNames）。
	// 必须先于别名族展开判断，否则 `deepseek-v4.1-flash-1017` 会以
	// `deepseek-v4.1-flash` 的别名身份混进白名单。
	if IsRetiredModelName(model) {
		return false
	}
	if !a.Enabled {
		return true
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return true
	}
	candidates := groupModelAllowlistCandidates(model)
	for _, entry := range a.Models {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		for _, candidate := range candidates {
			if groupAllowlistPatternMatches(entry, candidate) {
				return true
			}
		}
	}
	return false
}

// groupAllowlistPatternMatches treats only * as a wildcard, including in the middle
// or at either end. Matching is anchored and case-insensitive.
func groupAllowlistPatternMatches(pattern, model string) bool {
	pattern = strings.ToLower(pattern)
	model = strings.ToLower(model)
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == model
	}
	if !strings.HasPrefix(model, parts[0]) {
		return false
	}
	model = model[len(parts[0]):]
	for _, part := range parts[1 : len(parts)-1] {
		index := strings.Index(model, part)
		if index < 0 {
			return false
		}
		model = model[index+len(part):]
	}
	return strings.HasSuffix(model, parts[len(parts)-1])
}

// groupModelAllowlistCandidates 返回客户端模型名在白名单匹配中的候选形式（均已小写）。
func groupModelAllowlistCandidates(model string) []string {
	candidates := make([]string, 0, 4)
	add := func(value string) {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			return
		}
		for _, existing := range candidates {
			if existing == value {
				return
			}
		}
		candidates = append(candidates, value)
	}

	for _, candidateModel := range []string{model, normalizeClaudeCodeLongContextModel(model)} {
		// 模型名别名族（见 model_name_alias.go）：白名单里写 claude-fable-5-1、
		// 客户端发 claude-fable-5.1（或 DeepSeek 带不带尾部版本后缀）时，
		// 两者应视为同一模型，否则准入与账号映射的判定会打架。
		for _, alias := range ModelNameAliases(candidateModel) {
			add(alias)
		}
		add(strings.TrimPrefix(candidateModel, "models/"))
		add(claude.NormalizeModelID(strings.TrimSuffix(candidateModel, "-thinking")))
		add(NormalizeOpenAICompatRequestedModel(candidateModel))
	}
	return candidates
}

// FilterForListing 按白名单条目顺序生成模型列表输出。
// 精确条目沿用既有规则：只有出现在 source（账号映射键 ∪ 平台默认列表）的模式
// 集合中才输出；通配条目展开为 source 中所有匹配项并保持 source 顺序；全局去重。
// 当 source 是前缀通配模式且白名单模式的固定前缀位于该前缀之内时，
// 可直接输出白名单模式：它的所有实例都属于 source。其他未处理的无限
// 交集不会被枚举为模型 ID，列表可能省略仍可请求的模型。
func (a GroupModelAllowlist) FilterForListing(source []string) []string {
	if !a.Enabled {
		// 白名单未开启时列表直接透传 source（账号映射键），同样不得把已下线的
		// 模型名暴露给下游，也不得把旧写法暴露给新用户（统一换成对外展示名）。
		return presentModelNamesForListing(source)
	}
	if len(a.Models) == 0 {
		// 开启但为空的配置在管理端已被拒绝；对遗留脏数据保持“看到什么 = 能调什么”，
		// 列表与准入同时返回空。
		return nil
	}

	patterns := make([]string, 0, len(source))
	for _, pattern := range source {
		pattern = strings.TrimSpace(pattern)
		if pattern != "" {
			patterns = append(patterns, pattern)
		}
	}
	if len(patterns) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(a.Models)+len(patterns))
	filtered := make([]string, 0, len(patterns))
	add := func(model string) {
		if IsRetiredModelName(model) {
			return
		}
		// 列表是下游唯一能"看见"模型名的地方：族内旧写法一律换成对外展示名，
		// 新用户因此看不到任何旧名字（用户裁定 2026-11-05）。准入与映射不受影响。
		model = PublicModelName(model)
		key := strings.ToLower(model)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		filtered = append(filtered, model)
	}

	for _, entry := range a.Models {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "*") {
			for _, pattern := range patterns {
				if groupAllowlistPatternMatches(entry, pattern) {
					add(pattern)
				} else if strings.HasSuffix(pattern, "*") &&
					!strings.Contains(strings.TrimSuffix(pattern, "*"), "*") &&
					strings.HasPrefix(strings.ToLower(entry), strings.ToLower(strings.TrimSuffix(pattern, "*"))) {
					add(entry)
				}
			}
			continue
		}
		if allowlistSourcePatternAllowsModel(patterns, entry) {
			add(entry)
		}
	}
	return filtered
}

// allowlistSourcePatternAllowsModel 沿用 filterModelsByCustomList 时代的匹配规则：
// 精确相等、source 通配模式的前缀匹配，以及 Claude 归一化（-thinking 后缀）
// 后的精确匹配。比较与 Allows 一律大小写不敏感，保证「准入允许 ⇒ 列表可见」。
func allowlistSourcePatternAllowsModel(patterns []string, model string) bool {
	for _, pattern := range patterns {
		if strings.EqualFold(pattern, model) {
			return true
		}
		// 别名族等价：白名单条目与账号映射键写法不同但属同一模型时，
		// 列表可见性必须与准入一致（「准入允许 ⇒ 列表可见」）。
		if SameModelNameFamily(pattern, model) {
			return true
		}
		if strings.HasSuffix(pattern, "*") && strings.HasPrefix(strings.ToLower(model), strings.ToLower(strings.TrimSuffix(pattern, "*"))) {
			return true
		}
	}
	normalizedClaudeModel := claude.NormalizeModelID(strings.TrimSuffix(model, "-thinking"))
	if !strings.EqualFold(normalizedClaudeModel, model) {
		for _, pattern := range patterns {
			if strings.EqualFold(pattern, normalizedClaudeModel) {
				return true
			}
		}
	}
	return false
}
