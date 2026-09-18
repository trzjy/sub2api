package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

// WithResolvedTargetPlatform stores the concrete provider chosen for a request
// made through a composite group.
func WithResolvedTargetPlatform(ctx context.Context, platform string) context.Context {
	platform = strings.TrimSpace(platform)
	if ctx == nil || platform == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxkey.ResolvedTargetPlatform, platform)
}

// ResolvedTargetPlatformFromContext returns the concrete provider chosen for
// the current request, if one was resolved.
func ResolvedTargetPlatformFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	platform, ok := ctx.Value(ctxkey.ResolvedTargetPlatform).(string)
	platform = strings.TrimSpace(platform)
	if !ok || platform == "" {
		return "", false
	}
	return platform, true
}

func WithCompositeRouteDecision(ctx context.Context, decision CompositeRouteDecision) context.Context {
	if ctx == nil || !decision.Matched {
		return ctx
	}
	ctx = WithResolvedTargetPlatform(ctx, decision.TargetPlatform)
	if model := strings.TrimSpace(decision.UpstreamModel); model != "" {
		ctx = context.WithValue(ctx, ctxkey.ResolvedUpstreamModel, model)
	}
	if model := strings.TrimSpace(decision.PublicModel); model != "" {
		ctx = context.WithValue(ctx, ctxkey.RequestedPublicModel, model)
	}
	if source := strings.TrimSpace(decision.Source); source != "" {
		ctx = context.WithValue(ctx, ctxkey.CompositeRouteSource, source)
	}
	return ctx
}

func ResolvedUpstreamModelFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	model, ok := ctx.Value(ctxkey.ResolvedUpstreamModel).(string)
	model = strings.TrimSpace(model)
	if !ok || model == "" {
		return "", false
	}
	return model, true
}

func RequestedPublicModelFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	model, ok := ctx.Value(ctxkey.RequestedPublicModel).(string)
	model = strings.TrimSpace(model)
	if !ok || model == "" {
		return "", false
	}
	return model, true
}

func CompositeRouteSourceFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	source, ok := ctx.Value(ctxkey.CompositeRouteSource).(string)
	source = strings.TrimSpace(source)
	if !ok || source == "" {
		return "", false
	}
	return source, true
}

// DetectModelPlatform maps common public model IDs to the concrete provider
// platform used by sub2api. It intentionally returns false for ambiguous model
// names so composite groups fail closed instead of guessing.
func DetectModelPlatform(model string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(model))
	if normalized == "" {
		return "", false
	}

	normalized = strings.TrimPrefix(normalized, "models/")
	if slash := strings.IndexByte(normalized, '/'); slash > 0 {
		provider := strings.TrimSpace(normalized[:slash])
		rest := strings.TrimSpace(normalized[slash+1:])
		switch provider {
		case "anthropic", "claude":
			return PlatformAnthropic, true
		case "openai", "chatgpt":
			return PlatformOpenAI, true
		case "google", "google-ai-studio", "gemini":
			return PlatformGemini, true
		case "xai", "x-ai", "grok":
			return PlatformGrok, true
		case "kimi", "moonshot":
			return PlatformKimi, true
		case "zhipu", "glm", "bigmodel":
			return PlatformZhipu, true
		case "deepseek":
			return PlatformDeepseek, true
		case "minimax":
			return PlatformMiniMax, true
		}
		if rest != "" {
			normalized = strings.TrimPrefix(rest, "models/")
		}
	}

	switch {
	case strings.HasPrefix(normalized, "anthropic.claude-"),
		strings.HasPrefix(normalized, "claude-"):
		return PlatformAnthropic, true
	case strings.HasPrefix(normalized, "gpt-"),
		strings.HasPrefix(normalized, "chatgpt-"),
		strings.HasPrefix(normalized, "codex-"),
		strings.HasPrefix(normalized, "text-embedding-"),
		strings.HasPrefix(normalized, "text-moderation-"),
		strings.HasPrefix(normalized, "omni-moderation-"),
		strings.HasPrefix(normalized, "dall-e-"),
		strings.HasPrefix(normalized, "gpt-image-"),
		strings.HasPrefix(normalized, "tts-"),
		strings.HasPrefix(normalized, "whisper-"),
		hasOpenAISeriesPrefix(normalized):
		return PlatformOpenAI, true
	case strings.HasPrefix(normalized, "gemini-"),
		strings.HasPrefix(normalized, "learnlm-"):
		return PlatformGemini, true
	case normalized == "grok" || strings.HasPrefix(normalized, "grok-"):
		return PlatformGrok, true
	case normalized == "k3",
		normalized == "k3-256k",
		strings.HasPrefix(normalized, "kimi-"),
		strings.HasPrefix(normalized, "moonshot-"):
		return PlatformKimi, true
	case strings.HasPrefix(normalized, "glm-"):
		return PlatformZhipu, true
	case strings.HasPrefix(normalized, "deepseek-"):
		return PlatformDeepseek, true
	case strings.HasPrefix(normalized, "minimax-"),
		strings.HasPrefix(normalized, "abab5"),
		strings.HasPrefix(normalized, "abab6"),
		strings.HasPrefix(normalized, "abab7"):
		return PlatformMiniMax, true
	default:
		return "", false
	}
}

func hasOpenAISeriesPrefix(model string) bool {
	for _, prefix := range []string{"o1", "o3", "o4", "o5"} {
		if model == prefix || strings.HasPrefix(model, prefix+"-") {
			return true
		}
	}
	return false
}

func (s *GatewayService) resolveCompositeRouteDecision(ctx context.Context, group *Group, requestedModel, endpoint string) (CompositeRouteDecision, bool, error) {
	if group == nil || group.Platform != PlatformComposite {
		return CompositeRouteDecision{}, false, nil
	}
	if platform, ok := ResolvedTargetPlatformFromContext(ctx); ok {
		upstreamModel := requestedModel
		if resolvedModel, modelOK := ResolvedUpstreamModelFromContext(ctx); modelOK {
			upstreamModel = resolvedModel
		}
		source := CompositeRouteSourceDetector
		if resolvedSource, sourceOK := CompositeRouteSourceFromContext(ctx); sourceOK {
			source = resolvedSource
		}
		return CompositeRouteDecision{
			Matched:        true,
			Source:         source,
			GroupID:        group.ID,
			PublicModel:    requestedModel,
			TargetPlatform: platform,
			UpstreamModel:  upstreamModel,
			Endpoint:       normalizeCompositeRouteEndpoint(endpoint),
		}, true, nil
	}
	decision, err := s.compositeResolver.Resolve(ctx, group.ID, requestedModel, endpoint)
	if err != nil {
		return decision, false, err
	}
	return decision, decision.Matched, nil
}

// isConcreteRequestPlatform：composite 目标平台白名单。平台归并后（PR-4）网页接入
// 挂在官方平台 zhipu/deepseek/kimi 账号级 access_mode 上，web-* 不再是目标平台。
func isConcreteRequestPlatform(platform string) bool {
	switch platform {
	case PlatformAnthropic, PlatformOpenAI, PlatformGemini, PlatformAntigravity, PlatformGrok,
		PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax:
		return true
	default:
		return false
	}
}

// DefaultWebModelIDs 返回各官方平台网页接入（access_mode=web）的默认模型目录
// （方案 §3.3 模型映射表）。
// 空 model_mapping 时，公开 /models 列表与账号默认模型集回落到此表，而非误回落到
// Claude 默认模型。集中定义于此，供 gateway_handler 与 admin account_handler 共用，
// 避免两处各写一份导致漂移。
//
// 归并后签名（docs/platform-merge-refactor-plan.md §5.6，PR-4 旧链归零）：键为官方
// 平台值（zhipu/deepseek/kimi）+ 显式 access_mode="web" 双键；失败关闭——缺省 mode
// 或 mode 非 "web" 一律返回 nil，调用方必须显式声明 web 语境，杜绝「只按平台、
// 不按接入模式」的模糊判定（本轮重构红线）。
//
// 公开模型 ID 固定无前缀（glm-5.3-flash / deepseek-chat / deepseek-reasoner / kimi-k3），
// 前缀标注仅限管理端标签，不影响公开 ID、请求路由或调度匹配。
func DefaultWebModelIDs(platform string, mode ...string) []string {
	// 失败关闭：仅显式 access_mode=web 返回 web 目录；缺省或其他值返回 nil。
	if len(mode) == 0 || mode[0] != AccountAccessModeWeb {
		return nil
	}
	switch platform {
	case PlatformZhipu:
		// 2026-09-17 官网登录态抓包实测：请求体 meta_data.selected_model 原值
		// "glm-5.3-flash"（GLM-Flash 极致，assistant_id=webZhipuDefaultAssistantID）。
		// 旧 glm-4.7/glm-4.7-flash 目录无当前官网依据，已删除。
		return []string{"glm-5.3-flash"}
	case PlatformDeepseek:
		return []string{"deepseek-chat", "deepseek-reasoner"}
	case PlatformKimi:
		return []string{"kimi-k3"}
	default:
		return nil
	}
}

// ValidateWebModel 校验指定官方平台的 web 接入模式账号入站模型是否在默认目录内；
// model 为空或不在允许集合内时返回非 nil error，使未知模型失败关闭
// （docs/platform-merge-refactor-plan.md §5.6：Web 账号不得回落 API 模型目录、不猜测）。
// 错误信息仅含模型名与平台，不泄露任何凭证。
//
// 各 web 适配器接受的公开 ID（与适配器映射一致；kimi 额外接受 agent-ultra 内部变体，
// 公开目录以固定 ID kimi-k3 为准）：
//   - zhipu:    glm-5.3-flash
//   - deepseek: deepseek-chat, deepseek-reasoner
//   - kimi:     kimi-k3, kimi-k3-agent-ultra
func ValidateWebModel(provider, model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return fmt.Errorf("%s web model is required but was empty", provider)
	}
	var allowed []string
	switch provider {
	case PlatformZhipu:
		allowed = []string{"glm-5.3-flash"}
	case PlatformDeepseek:
		allowed = []string{"deepseek-chat", "deepseek-reasoner"}
	case PlatformKimi:
		allowed = []string{"kimi-k3", "kimi-k3-agent-ultra"}
	default:
		allowed = DefaultWebModelIDs(provider, AccountAccessModeWeb)
	}
	for _, a := range allowed {
		if model == a {
			return nil
		}
	}
	return fmt.Errorf("%s web model %q is not supported", provider, model)
}

// MergeAndDedupModelIDs 合并多个模型目录源并对同名 ID 去重，保留首次出现顺序。用于
// /models 聚合输出：web 账号与 API 账号同组时，deepseek-chat / deepseek-reasoner 等同名
// 单条目只出现一次（docs/platform-merge-refactor-plan.md §5.6 B4 同名模型去重）。
func MergeAndDedupModelIDs(sources ...[]string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, src := range sources {
		for _, m := range src {
			if _, ok := seen[m]; ok {
				continue
			}
			seen[m] = struct{}{}
			out = append(out, m)
		}
	}
	return out
}

// ValidateWebZhipuModel 校验 zhipu 网页接入（access_mode=web）入站模型是否在默认目录内；
// model 为空或不在 DefaultWebModelIDs(PlatformZhipu, AccountAccessModeWeb) 内时返回非 nil
// error，使未知模型失败关闭（避免误回落到未实测模型）。错误信息仅含模型名与平台，
// 不泄露任何凭证或其他敏感信息。
func ValidateWebZhipuModel(model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return fmt.Errorf("zhipu web model is required but was empty")
	}
	for _, allowed := range DefaultWebModelIDs(PlatformZhipu, AccountAccessModeWeb) {
		if model == allowed {
			return nil
		}
	}
	return fmt.Errorf("zhipu web model %q is not supported", model)
}
