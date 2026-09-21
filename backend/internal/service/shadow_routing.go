package service

import "strings"

// parentHealthyForShadow 报告影子账号的母账号凭据是否可用(影子据此可被调度)。
//
// 非影子账号直接返回 true（不受此检查约束）。
// lookup 将母账号 ID 解析为当前 Account（来自调度快照 map 或 repo）。
//
// 关键语义(F1 决策 A + 外审 D):母账号须仍是 OpenAI OAuth 或 CodeBuddy OAuth
// (fail-closed——否则透传凭据解析必失败,影子不应进调度候选),且凭据「可用」。
// IsCredentialUsableForShadow 检查:账号 active、OAuth token 未过期、且**未处于
// TempUnschedulableUntil 冷却期**——对 OpenAI/CodeBuddy 账号该字段由 401/token 刷新耗尽/
// transport·proxy 故障写入,代表共享凭据或传输坏死,故**连坐**影子。
//
// **刻意排除** global 维度的 RateLimitResetAt/OverloadUntil 与母账号手动 Schedulable 开关:
// 母账号 global 429 不得连坐影子,否则会重新耦合影子架构本应解耦的两条 429 道。
// 母账号未找到(nil)、非受支持的 OAuth 母账号、或凭据不可用时影子被挡。
func parentHealthyForShadow(account *Account, lookup func(int64) *Account) bool {
	if account == nil || !account.IsShadow() {
		return true
	}
	parent := lookup(*account.ParentAccountID)
	if parent == nil {
		return false
	}
	// 母账号支持 OpenAI OAuth / CodeBuddy OAuth / CodeBuddy APIKey（透传路径统一
	// 由 GetAccessToken 处理），凭据可用时影子可调度。
	isParent := parent.IsOpenAIOAuth() ||
		(parent.IsCodeBuddy() && (parent.Type == AccountTypeOAuth || parent.Type == AccountTypeAPIKey))
	return isParent && parent.IsCredentialUsableForShadow()
}

// sparkModelVariants 返回所有归一到 spark 的模型 ID（当前仅 base：spark 无 effort 变体）。
// 从 codexModelMap 派生，使集合与别名表单一来源、不漂移；若上游将来新增 spark 变体，
// 在 codexModelMap 注册后此处自动跟随。
func sparkModelVariants() []string {
	out := make([]string, 0, 1)
	for alias, target := range codexModelMap {
		if target == "gpt-5.3-codex-spark" {
			out = append(out, alias)
		}
	}
	return out
}

// defaultSparkShadowModelMapping 返回 spark 影子账号的默认 model_mapping。
//
// 恒等映射（key 映射到自身）把「只接 spark」限制落在 key 白名单上，模型零改写、
// 与空 mapping 透传行为一致。当前 spark 仅 base 一个模型（无 effort 变体）。
func defaultSparkShadowModelMapping() map[string]any {
	variants := sparkModelVariants()
	mapping := make(map[string]any, len(variants))
	for _, m := range variants {
		mapping[m] = m
	}
	return mapping
}

// defaultCodeBuddyShadowModelMapping 返回 codebuddy 影子创建时的默认 model_mapping。
//
// 影子的上游模型名（extra[ShadowModelExtraKey]，即 shadow_model）通常与目标平台的
// 官方公开模型 ID 不同（如官方 "deepseek-chat" vs 上游 "deepseek-v4.1-flash"）。
// 官方 ID 清单一律取自 DefaultWebModelIDs（代码内唯一 SSOT，覆盖向导支持的
// deepseek/zhipu/kimi），禁止手写硬编码清单：
//   - 每个官方 ID → shadow_model：官方名请求命中映射后被改写为上游模型名；
//   - shadow_model → shadow_model（identity）：直接请求上游模型名也能命中
//     （mapping 同时是调度白名单，写入后仅映射键可命中，identity 键保证
//     直呼 shadow_model 不被白名单挡掉）。
//
// 平台无代码内官方清单（如 minimax/other）或 shadow_model 为空时返回 nil，
// 调用方保持空 mapping=原样透传（旧行为），不臆造清单。
func defaultCodeBuddyShadowModelMapping(platform, shadowModel string) map[string]any {
	shadowModel = strings.TrimSpace(shadowModel)
	if shadowModel == "" {
		return nil
	}
	officialIDs := DefaultWebModelIDs(platform, AccountAccessModeWeb)
	if len(officialIDs) == 0 {
		return nil
	}
	mapping := make(map[string]any, len(officialIDs)+1)
	for _, id := range officialIDs {
		mapping[id] = shadowModel
	}
	mapping[shadowModel] = shadowModel
	return mapping
}

// inferCodeBuddyShadowPlatform 根据上游模型名推断 codebuddy 影子应落入的目标分组平台
// （一母多影：影子 platform=目标分组平台）。规则（方案 §2.2）：
//   - deepseek-* → deepseek
//   - glm-*      → zhipu
//   - kimi-*     → kimi
//   - minimax-*  → minimax
//   - 其余        → other
//
// openai 不在推断范围内——codebuddy 影子刻意排除 openai（避免与 OpenAI OAuth 语义混淆）。
// 调用方在推断后仍应显式拒绝 openai/codebuddy 平台（见 CreateShadow）。
func inferCodeBuddyShadowPlatform(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(m, "deepseek"):
		return PlatformDeepseek
	case strings.HasPrefix(m, "glm"):
		return PlatformZhipu
	case strings.HasPrefix(m, "kimi"):
		return PlatformKimi
	case strings.HasPrefix(m, "minimax"):
		return PlatformMiniMax
	default:
		return PlatformOther
	}
}

// shadowTargetsModel 报告 codebuddy 影子是否已服务于指定上游模型（用于一母多影去重）。
// 模型名以 Extra[ShadowModelExtraKey] 记录；未记录（直接 API 调用未传 model）一律视为不匹配，
// 由调用方的 opts.Model 空值守卫跳过去重。
func shadowTargetsModel(shadow *Account, model string) bool {
	if shadow == nil || strings.TrimSpace(model) == "" {
		return false
	}
	return strings.EqualFold(shadow.GetExtraString(ShadowModelExtraKey), model)
}
