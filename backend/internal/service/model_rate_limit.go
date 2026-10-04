package service

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

// WithChannelMappedModel 将渠道映射解析出的上游模型名装入 ctx，供调度门
// （modelRateLimitKeysForRequest）追加为额外判定 key，修复信道映射后模型名与
// 429 写入 scope 不一致导致的漏判熔断。ctx 无值时行为与现状完全一致。
// key 使用 ctxkey.ChannelMappedModel 常量（与仓库其他 context key 一致）。
func WithChannelMappedModel(ctx context.Context, mapped string) context.Context {
	return context.WithValue(ctx, ctxkey.ChannelMappedModel, mapped)
}

// ChannelMappedModelFromContext 取出 ctx 中的渠道映射上游模型名（未设置则空字符串）。
func ChannelMappedModelFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxkey.ChannelMappedModel).(string); ok {
		return v
	}
	return ""
}

const (
	modelRateLimitsKey                 = "model_rate_limits"
	antigravityGeminiModelRateLimitKey = "antigravity:gemini"
	openAIImageGenerationRateLimitKey  = "openai:image_generation"
	openAICodexSparkRateLimitReason    = "openai_codex_spark_rate_limit"
	// anthropicFableRateLimitKey 是 Anthropic 7d_oi（Fable 专属 7d 窗口）限流的
	// 家族级 scope：命中后所有 Fable 变体（含 [1m] 等后缀）都不再调度到该账号。
	anthropicFableRateLimitKey = "claude-fable-5"
)

// isRateLimitActiveForKey 检查指定 key 的限流是否生效
func (a *Account) isRateLimitActiveForKey(key string) bool {
	resetAt := a.modelRateLimitResetAt(key)
	return resetAt != nil && time.Now().Before(*resetAt)
}

// getRateLimitRemainingForKey 获取指定 key 的限流剩余时间，0 表示未限流或已过期
func (a *Account) getRateLimitRemainingForKey(key string) time.Duration {
	resetAt := a.modelRateLimitResetAt(key)
	if resetAt == nil {
		return 0
	}
	remaining := time.Until(*resetAt)
	if remaining > 0 {
		return remaining
	}
	return 0
}

func (a *Account) isModelRateLimitedWithContext(ctx context.Context, requestedModel string) bool {
	for _, key := range a.modelRateLimitKeysForRequest(ctx, requestedModel) {
		if a.isRateLimitActiveForKey(key) {
			return true
		}
	}
	return false
}

// GetModelRateLimitRemainingTime 获取模型限流剩余时间
// 返回 0 表示未限流或已过期
func (a *Account) GetModelRateLimitRemainingTime(requestedModel string) time.Duration {
	return a.GetModelRateLimitRemainingTimeWithContext(context.Background(), requestedModel)
}

func (a *Account) GetModelRateLimitRemainingTimeWithContext(ctx context.Context, requestedModel string) time.Duration {
	remaining := time.Duration(0)
	for _, key := range a.modelRateLimitKeysForRequest(ctx, requestedModel) {
		if keyRemaining := a.getRateLimitRemainingForKey(key); keyRemaining > remaining {
			remaining = keyRemaining
		}
	}
	return remaining
}

func (a *Account) modelRateLimitKeysForRequest(ctx context.Context, requestedModel string) []string {
	if a == nil {
		return nil
	}

	modelKey := a.GetMappedModel(requestedModel)
	if a.Platform == PlatformAntigravity {
		modelKey = resolveFinalAntigravityModelKey(ctx, a, requestedModel)
	}
	modelKey = strings.TrimSpace(modelKey)
	if modelKey == "" {
		return nil
	}

	keys := []string{modelKey}
	switch a.Platform {
	case PlatformAntigravity:
		if isAntigravityGeminiModel(modelKey) && modelKey != antigravityGeminiModelRateLimitKey {
			keys = append(keys, antigravityGeminiModelRateLimitKey)
		}
	case PlatformOpenAI:
		if openAIImageGenerationRateLimitApplies(ctx, requestedModel, modelKey) && modelKey != openAIImageGenerationRateLimitKey {
			keys = append(keys, openAIImageGenerationRateLimitKey)
		}
	case PlatformAnthropic:
		if isAnthropicFableModel(modelKey) && modelKey != anthropicFableRateLimitKey {
			keys = append(keys, anthropicFableRateLimitKey)
		}
	}
	// 渠道映射后的上游模型名：429 写入与探测恢复链用的就是映射后的 scope，但渠道
	// 映射发生在选号之后，调度深度无法重新解析。handler 在既有解析点把已解析值装入
	// ctx 下传（单次解析，避免二次解析与配置刷新之间的并发窗口）。此处追加为额外判定
	// key，与转发改写消费同一值，保证"调度门判定身份 == 转发实际身份"。
	if mapped := ChannelMappedModelFromContext(ctx); mapped != "" {
		if !slices.Contains(keys, mapped) {
			keys = append(keys, mapped)
		}
	}
	return keys
}

// isAnthropicFableModel 判断是否为 Fable 模型家族（claude-fable-5、claude-fable-5[1m] 等变体）
func isAnthropicFableModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "fable")
}

func openAIImageGenerationRateLimitApplies(ctx context.Context, requestedModel, modelKey string) bool {
	if isOpenAIImageGenerationModel(requestedModel) || isOpenAIImageGenerationModel(modelKey) {
		return true
	}
	return OpenAIImageGenerationIntentFromContext(ctx)
}

func WithOpenAIImageGenerationIntent(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, ctxkey.OpenAIImageGenerationIntent, true)
}

func OpenAIImageGenerationIntentFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	enabled, ok := ctx.Value(ctxkey.OpenAIImageGenerationIntent).(bool)
	return ok && enabled
}

// WithOpenAIImagesEndpoint 标记请求从 /v1/images/* 专用生图端点入站。
func WithOpenAIImagesEndpoint(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, ctxkey.OpenAIImagesEndpoint, true)
}

// OpenAIImagesEndpointFromContext 报告请求是否来自 /v1/images/*。
func OpenAIImagesEndpointFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	enabled, ok := ctx.Value(ctxkey.OpenAIImagesEndpoint).(bool)
	return ok && enabled
}

func resolveFinalAntigravityModelKey(ctx context.Context, account *Account, requestedModel string) string {
	modelKey := mapAntigravityModel(account, requestedModel)
	if modelKey == "" {
		return ""
	}
	// thinking 会影响 Antigravity 最终模型名（例如 claude-sonnet-4-5 -> claude-sonnet-4-5-thinking）
	if enabled, ok := ThinkingEnabledFromContext(ctx); ok {
		modelKey = applyThinkingModelSuffix(modelKey, enabled)
	}
	return modelKey
}

func isAntigravityGeminiModel(model string) bool {
	return strings.HasPrefix(normalizeAntigravityModelName(model), "gemini-")
}

func antigravityModelRateLimitKeys(model string) []string {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	keys := []string{model}
	if isAntigravityGeminiModel(model) && model != antigravityGeminiModelRateLimitKey {
		keys = append(keys, antigravityGeminiModelRateLimitKey)
	}
	return keys
}

// ActiveTokenHarborFreeTierScopes 是 TokenHarbor 免费档模型级限流候选判定的**权威导出
// 入口**：reason 前缀 tokenharbor_free_tier_exhausted（复用同包权威常量
// tokenHarborFreeTierReasonPrefix，不复制任何字面量/时间语义），供 repository 的候选
// 粗筛（account_repo.ListTokenHarborModelRateLimitedAccounts）在 Go 侧过滤时复用，
// 避免把 tokenharbor 判定语义复制进 repository 包。
//
// 与探测链内部判定 AccountHealthRecoveryProbeService.activeTokenHarborFreeTierScopes
// 同源同义（同一批常量、同一到期语叉），并由 service 层定向测试
// TestE12ActiveTokenHarborFreeTierScopes_MatchesProbePredicate 锁定"逐位等价"，防止漂移；
// 后续若允许改动探测服务文件，应让探测链方法委托本函数以收敛为单一实现。
//
// E42 无信号哨兵不吃到期剔除（precise_reset 标记为唯一判别依据，D5 锁定）：
//   - precise_reset=false（无精准恢复信号哨兵）：reset_at 仅为"持续受限"占位值（如固定
//     now+365d），恢复时刻未知，须由主动复探成功触发清除；按 reset_at 到期剔除会令账号
//     持续未恢复超过占位期限后被候选筛选永久排除、前端不再展示，主动复探无法再触发。故
//     跳过到期剔除，持续受限直至复探写入口清除。
//   - precise_reset=true（上游权威可恢复标记）：reset_at 是真实恢复时刻，到期照常剔除
//     （标准行为，与前端展示语义一致）。
//
// 未到期的条目（含 D5 哨兵条目 precise_reset=false + 远期 reset_at）本就纳入，不受影响。
// 注：E7 #6 曾对 precise=true 也跳过到期剔除；E42 校正为仅无信号哨兵（precise=false）
// 不吃到期剔除，precise=true 恢复为标准到期剔除，以与前端展示平行路径保持一致。
func ActiveTokenHarborFreeTierScopes(extra map[string]any, now time.Time) []string {
	if extra == nil {
		return nil
	}
	limits, ok := extra[modelRateLimitsKey].(map[string]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(limits))
	for scope, raw := range limits {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		reason, _ := entry["reason"].(string)
		if !strings.HasPrefix(reason, tokenHarborFreeTierReasonPrefix) {
			continue
		}
		resetAtRaw, _ := entry["rate_limit_reset_at"].(string)
		if resetAtRaw == "" {
			continue
		}
		resetAt, perr := time.Parse(time.RFC3339, resetAtRaw)
		if perr != nil {
			continue
		}
		// 无信号哨兵（precise_reset=false）不吃到期剔除（E42，D5 锁定）：reset_at 仅为
		// "持续受限"占位值，恢复时刻未知，须经主动复探确认；仅 precise_reset=true 的权威
		// 恢复标记按 reset_at 到期照常剔除，precise_reset=false（含缺省）一律持续受限。
		precise, _ := entry["precise_reset"].(bool)
		if precise && !resetAt.After(now) {
			continue
		}
		out = append(out, scope)
	}
	return out
}

func (a *Account) modelRateLimitResetAt(scope string) *time.Time {
	if a == nil || a.Extra == nil || scope == "" {
		return nil
	}
	rawLimits, ok := a.Extra[modelRateLimitsKey].(map[string]any)
	if !ok {
		return nil
	}
	rawLimit, ok := rawLimits[scope].(map[string]any)
	if !ok {
		return nil
	}
	resetAtRaw, ok := rawLimit["rate_limit_reset_at"].(string)
	if !ok || strings.TrimSpace(resetAtRaw) == "" {
		return nil
	}
	resetAt, err := time.Parse(time.RFC3339, resetAtRaw)
	if err != nil {
		return nil
	}
	return &resetAt
}

func setAccountModelRateLimitSnapshot(account *Account, scope string, resetAt time.Time, reason string, now time.Time) {
	if account == nil || strings.TrimSpace(scope) == "" {
		return
	}
	if account.Extra == nil {
		account.Extra = make(map[string]any)
	}
	limits, ok := account.Extra[modelRateLimitsKey].(map[string]any)
	if !ok {
		limits = make(map[string]any)
		account.Extra[modelRateLimitsKey] = limits
	}
	payload := map[string]any{
		"rate_limited_at":     now.UTC().Format(time.RFC3339),
		"rate_limit_reset_at": resetAt.UTC().Format(time.RFC3339),
	}
	if reason = strings.TrimSpace(reason); reason != "" {
		payload["reason"] = reason
	}
	limits[scope] = payload
}
