package service

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	// CompositeRouteReasonAmbiguous marks a model whose explicit account
	// mappings span several provider platforms.
	CompositeRouteReasonAmbiguous = "model is exposed by multiple provider platforms"
)

type CompositeRouteResolver struct {
	repo                   CompositeModelRouteRepository
	modelOwnershipResolver CompositeModelOwnershipResolver
}

func NewCompositeRouteResolver(repo CompositeModelRouteRepository) *CompositeRouteResolver {
	return &CompositeRouteResolver{repo: repo}
}

func (r *CompositeRouteResolver) SetModelOwnershipResolver(resolver CompositeModelOwnershipResolver) {
	if r != nil {
		r.modelOwnershipResolver = resolver
	}
}

// ListExactPublicModels returns enabled, concrete route IDs suitable for a model catalog.
func (r *CompositeRouteResolver) ListExactPublicModels(ctx context.Context, groupID int64, endpoint string, includeSystemOne bool) ([]string, error) {
	if r == nil || r.repo == nil || groupID <= 0 {
		return nil, nil
	}
	routes, err := r.repo.ListByGroup(ctx, groupID, false)
	if err != nil {
		return nil, err
	}
	models := make([]string, 0, len(routes))
	for _, route := range routes {
		if route.Enabled && route.MatchType == CompositeRouteMatchExact &&
			(endpoint == "" || normalizeCompositeRouteEndpoint(route.Endpoint) == CompositeRouteEndpointAny || normalizeCompositeRouteEndpoint(route.Endpoint) == endpoint) {
			if model := strings.TrimSpace(route.PublicModel); model != "" {
				models = append(models, model)
			}
		}
	}
	if !includeSystemOne {
		models = filterSystemOneRouteModels(routes, models, endpoint)
	}
	return models, nil
}

func (r *CompositeRouteResolver) FilterCodexModels(ctx context.Context, groupID int64, models []string) ([]string, error) {
	if r == nil || r.repo == nil || groupID <= 0 {
		return models, nil
	}
	routes, err := r.repo.ListByGroup(ctx, groupID, false)
	if err != nil {
		return nil, err
	}
	return filterSystemOneRouteModels(routes, models, CompositeRouteEndpointResponses), nil
}

func filterSystemOneRouteModels(routes []CompositeModelRoute, models []string, endpoint string) []string {
	filtered := make([]string, 0, len(models))
	for _, model := range models {
		route, matched := matchCompositeRoute(routes, model, normalizeCompositeRouteEndpoint(endpoint))
		if !matched || route.TargetPlatform != PlatformTypeSafe {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

// compositeDefaultChain 返回平台默认的执行/入口链：按 domain 登记的 Gateway 映射，
// antigravity 归入 anthropic；other 与未知平台默认落 openai。
func compositeDefaultChain(platform string) string {
	spec, ok := domain.LookupPlatform(platform)
	if !ok {
		return CompositeChainOpenAI
	}
	switch spec.Gateway {
	case domain.PlatformGatewayAnthropic, domain.PlatformGatewayAntigravity:
		return CompositeChainAnthropic
	case domain.PlatformGatewayGemini:
		return CompositeChainGemini
	default:
		return CompositeChainOpenAI
	}
}

// compositeEntryChainForEndpoint 返回多链歧义时按端点选择的入口链：messages /
// count_tokens 与 systemone 走 anthropic，gemini 走 gemini；chat_completions /
// responses 若 eligible 链含 anthropic 或 gemini 则走 anthropic（OpenAI handler
// 未装配 gemini 转发服务，无法承接 Gemini 账号），否则走 openai；其余端点走 openai。
func compositeEntryChainForEndpoint(endpoint string, chains []string) string {
	switch endpoint {
	case CompositeEndpointMessages, CompositeEndpointCountTokens, CompositeEndpointSystemOne:
		return CompositeChainAnthropic
	case CompositeEndpointGemini:
		return CompositeChainGemini
	case CompositeEndpointChatCompletions, CompositeEndpointResponses:
		for _, chain := range chains {
			if chain == CompositeChainAnthropic || chain == CompositeChainGemini {
				return CompositeChainAnthropic
			}
		}
		return CompositeChainOpenAI
	default:
		return CompositeChainOpenAI
	}
}

// resolveOwnershipSupply 按账号 × 端点资格从 ownership 的声明方摘要中筛出本端点
// 可服务的账号，并据此决定入口链与目标平台。每个账号按 openai/anthropic/gemini
// 顺序取第一条可服务的链；不要求所有账号共享同一条链（允许混链）。
func resolveOwnershipSupply(decision CompositeRouteDecision, endpoint string, ownership CompositeModelOwnership) CompositeRouteDecision {
	eligible := make([]CompositeOwnershipClaimant, 0, len(ownership.Claimants))
	chainsSeen := map[string]struct{}{}
	platformsSeen := map[string]struct{}{}
	chains := []string{}
	eligiblePlatforms := []string{}
	for _, claimant := range ownership.Claimants {
		var matchedChain string
		for _, chain := range []string{CompositeChainOpenAI, CompositeChainAnthropic, CompositeChainGemini} {
			if _, ok := CompositeClaimantChainPath(chain, endpoint, claimant); ok {
				matchedChain = chain
				break
			}
		}
		if matchedChain == "" {
			continue
		}
		eligible = append(eligible, claimant)
		if _, seen := chainsSeen[matchedChain]; !seen {
			chainsSeen[matchedChain] = struct{}{}
			chains = append(chains, matchedChain)
		}
		if _, seen := platformsSeen[claimant.Platform]; !seen {
			platformsSeen[claimant.Platform] = struct{}{}
			eligiblePlatforms = append(eligiblePlatforms, claimant.Platform)
		}
	}
	if len(eligible) == 0 {
		decision.Matched = false
		decision.TargetPlatform = ""
		decision.SupplyPlatforms = ownership.Platforms
		decision.Reason = "no declared account can serve this endpoint"
		decision.SupplyAccounts = nil
		return decision
	}
	sort.Strings(eligiblePlatforms)
	sort.Strings(chains)

	decision.Matched = true
	decision.Source = CompositeRouteSourceAccount
	decision.UpstreamModel = decision.PublicModel
	decision.SupplyPlatforms = eligiblePlatforms
	decision.SupplyAccounts = eligible
	decision.QualifyingChains = chains

	switch {
	case len(eligiblePlatforms) == 1:
		decision.TargetPlatform = eligiblePlatforms[0]
		decision.ServiceChain = compositeDefaultChain(eligiblePlatforms[0])
		decision.Reason = ""
	case len(chains) == 1:
		switch chains[0] {
		case CompositeChainAnthropic:
			decision.TargetPlatform = PlatformAnthropic
		case CompositeChainGemini:
			decision.TargetPlatform = PlatformGemini
		default:
			decision.TargetPlatform = PlatformOpenAI
		}
		decision.ServiceChain = chains[0]
		decision.Reason = CompositeRouteReasonAmbiguous
	default:
		decision.TargetPlatform = ""
		decision.ServiceChain = compositeEntryChainForEndpoint(endpoint, chains)
		decision.Reason = CompositeRouteReasonAmbiguous
	}
	return decision
}

func (r *CompositeRouteResolver) Resolve(ctx context.Context, groupID int64, model, endpoint string) (CompositeRouteDecision, error) {
	model = strings.TrimSpace(model)
	endpoint = normalizeCompositeRouteEndpoint(endpoint)
	decision := CompositeRouteDecision{
		GroupID:     groupID,
		PublicModel: model,
		Endpoint:    endpoint,
	}
	if model == "" {
		decision.Reason = infraerrors.ModelRequired
		return decision, nil
	}

	if r != nil && r.repo != nil && groupID > 0 {
		routes, err := r.repo.ListByGroup(ctx, groupID, false)
		if err != nil {
			return decision, fmt.Errorf("list composite routes: %w", err)
		}
		if route, ok := matchCompositeRoute(routes, model, endpoint); ok {
			upstreamModel := strings.TrimSpace(route.UpstreamModel)
			if upstreamModel == "" {
				upstreamModel = model
			}
			return CompositeRouteDecision{
				Matched:         true,
				Source:          CompositeRouteSourceExplicit,
				GroupID:         groupID,
				PublicModel:     model,
				TargetPlatform:  route.TargetPlatform,
				UpstreamModel:   upstreamModel,
				Endpoint:        endpoint,
				Route:           &route,
				SupplyPlatforms: []string{route.TargetPlatform},
				ServiceChain:    compositeDefaultChain(route.TargetPlatform),
			}, nil
		}
	}

	if r != nil && r.modelOwnershipResolver != nil && groupID > 0 {
		ownership, err := r.modelOwnershipResolver(ctx, groupID, model)
		if err != nil {
			// A recognizable model can still use the existing detector when the
			// account catalog is temporarily unavailable. Unknown aliases cannot.
			if _, detectable := DetectModelPlatform(model); !detectable {
				return decision, fmt.Errorf("resolve account model ownership: %w", err)
			}
		} else if ownership.Ambiguous || ownership.Matched {
			return resolveOwnershipSupply(decision, endpoint, ownership), nil
		}
	}

	if platform, ok := DetectModelPlatform(model); ok {
		return CompositeRouteDecision{
			Matched:         true,
			Source:          CompositeRouteSourceDetector,
			GroupID:         groupID,
			PublicModel:     model,
			TargetPlatform:  platform,
			UpstreamModel:   model,
			Endpoint:        endpoint,
			SupplyPlatforms: []string{platform},
			ServiceChain:    compositeDefaultChain(platform),
		}, nil
	}
	decision.Reason = "no explicit route or built-in detector match"
	return decision, nil
}

// ResolveGeminiWithDefault 是 gemini 文本端点的薄共用入口（§18.4 W 第 5 条）：复用现有
// Resolve，若未命中任何 route/ownership/detector，则按 gemini middleware 既有语义"未匹配补
// Gemini"——仅把 target 置为 PlatformGemini 并返回 defaulted=true，绝不伪造 Matched=true 或
// 账号 ownership 来源（不设置 Route、不覆盖 Source）。Resolve 报错时不补全、直接传播错误。
// middleware 与水位共用此方法，原 body 改写与 context 盖章顺序保持不变。
func (r *CompositeRouteResolver) ResolveGeminiWithDefault(ctx context.Context, groupID int64, model string) (CompositeRouteDecision, bool, error) {
	decision, err := r.Resolve(ctx, groupID, model, CompositeRouteEndpointGemini)
	if err != nil {
		return decision, false, err
	}
	if decision.Matched {
		return decision, false, nil
	}
	// 未匹配：middleware 现有"补 Gemini"语义——只补 target，不伪造 Matched/ownership。
	defaulted := decision
	defaulted.TargetPlatform = PlatformGemini
	defaulted.PublicModel = strings.TrimSpace(model)
	defaulted.Endpoint = CompositeRouteEndpointGemini
	defaulted.GroupID = groupID
	return defaulted, true, nil
}

func matchCompositeRoute(routes []CompositeModelRoute, model, endpoint string) (CompositeModelRoute, bool) {
	if len(routes) == 0 {
		return CompositeModelRoute{}, false
	}

	type candidate struct {
		route          CompositeModelRoute
		matchStrength  int
		endpointWeight int
		prefixLen      int
	}
	candidates := make([]candidate, 0, len(routes))
	for _, route := range routes {
		route.Endpoint = normalizeCompositeRouteEndpoint(route.Endpoint)
		if route.Endpoint != endpoint && route.Endpoint != CompositeRouteEndpointAny {
			continue
		}
		route.MatchType = normalizeCompositeRouteMatchType(route.MatchType)
		publicModel := strings.TrimSpace(route.PublicModel)
		if publicModel == "" {
			continue
		}

		matchStrength := 0
		prefixLen := len(publicModel)
		switch route.MatchType {
		case CompositeRouteMatchExact:
			if publicModel != model {
				continue
			}
			matchStrength = 2
		case CompositeRouteMatchPrefix:
			if !strings.HasPrefix(model, publicModel) {
				continue
			}
			matchStrength = 1
		default:
			continue
		}
		endpointWeight := 0
		if route.Endpoint == endpoint {
			endpointWeight = 1
		}
		candidates = append(candidates, candidate{
			route:          route,
			matchStrength:  matchStrength,
			endpointWeight: endpointWeight,
			prefixLen:      prefixLen,
		})
	}
	if len(candidates) == 0 {
		return CompositeModelRoute{}, false
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.matchStrength != b.matchStrength {
			return a.matchStrength > b.matchStrength
		}
		if a.endpointWeight != b.endpointWeight {
			return a.endpointWeight > b.endpointWeight
		}
		if a.prefixLen != b.prefixLen {
			return a.prefixLen > b.prefixLen
		}
		if a.route.Priority != b.route.Priority {
			return a.route.Priority < b.route.Priority
		}
		return a.route.ID < b.route.ID
	})
	return candidates[0].route, true
}
