package service

import (
	"context"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	CompositeRouteMatchExact  = "exact"
	CompositeRouteMatchPrefix = "prefix"

	CompositeRouteEndpointAny             = "any"
	CompositeRouteEndpointMessages        = "messages"
	CompositeRouteEndpointCountTokens     = "count_tokens"
	CompositeRouteEndpointResponses       = "responses"
	CompositeRouteEndpointChatCompletions = "chat_completions"
	CompositeRouteEndpointEmbeddings      = "embeddings"
	CompositeRouteEndpointImages          = "images"
	CompositeRouteEndpointGemini          = "gemini"

	CompositeRouteSourceExplicit = "route"
	CompositeRouteSourceDetector = "detector"
	CompositeRouteSourceAccount  = "account_model"
)

// CompositeOwnershipClaimant 是归属层保存的账号级声明方摘要，供跨链 selector 使用。
type CompositeOwnershipClaimant struct {
	AccountID     int64  `json:"account_id"`
	Platform      string `json:"platform"`
	Type          string `json:"type"`
	Priority      int    `json:"priority"`
	CursorConnect bool   `json:"cursor_connect"`
	WebAccessMode bool   `json:"web_access_mode"`
}

// CompositeModelOwnership identifies the concrete provider that exposes a
// public model through an account-level exact mapping.
type CompositeModelOwnership struct {
	TargetPlatform string
	Matched        bool
	Ambiguous      bool
	// Platforms 是暴露该公开模型的声明方平台集合，排序去重。
	// 本单不填充，由后续卡填充。
	Platforms []string
	// Claimants 是账号级声明方摘要，供跨链 selector 使用。
	// 本单不填充，由后续卡填充。
	Claimants []CompositeOwnershipClaimant
}

type CompositeModelOwnershipResolver func(context.Context, int64, string) (CompositeModelOwnership, error)

var (
	ErrCompositeRouteNotFound = infraerrors.NotFound("COMPOSITE_ROUTE_NOT_FOUND", "composite route not found")
	ErrCompositeRouteExists   = infraerrors.Conflict("COMPOSITE_ROUTE_EXISTS", "composite route already exists")
)

// CompositeModelRoute maps one public model identifier in a composite group to
// the concrete provider/model that should handle the request.
type CompositeModelRoute struct {
	ID             int64     `json:"id"`
	GroupID        int64     `json:"group_id"`
	PublicModel    string    `json:"public_model"`
	MatchType      string    `json:"match_type"`
	TargetPlatform string    `json:"target_platform"`
	UpstreamModel  string    `json:"upstream_model"`
	Endpoint       string    `json:"endpoint"`
	Priority       int       `json:"priority"`
	Enabled        bool      `json:"enabled"`
	Notes          string    `json:"notes"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type CompositeRoutePreviewRequest struct {
	Model    string `json:"model"`
	Endpoint string `json:"endpoint"`
}

type CompositeRouteDecision struct {
	Matched        bool                 `json:"matched"`
	Source         string               `json:"source"`
	GroupID        int64                `json:"group_id"`
	PublicModel    string               `json:"public_model"`
	TargetPlatform string               `json:"target_platform"`
	UpstreamModel  string               `json:"upstream_model"`
	Endpoint       string               `json:"endpoint"`
	Route          *CompositeModelRoute `json:"route,omitempty"`
	Reason         string               `json:"reason,omitempty"`
	// SupplyPlatforms 是本端点可服务的声明方平台集合；本单不填充。
	SupplyPlatforms []string `json:"supply_platforms,omitempty"`
	// ServiceChain 是多平台供给的入口/执行链：openai/anthropic/gemini；
	// 单平台/显式/detector 可留空。
	ServiceChain string `json:"service_chain,omitempty"`
	// QualifyingChains 是 eligible 账号涉及的链集合，诊断用，不要求交集。
	QualifyingChains []string `json:"qualifying_chains,omitempty"`
	// SupplyAccounts 是本端点可服务的账号摘要，selector 用；本单不填充。
	SupplyAccounts []CompositeOwnershipClaimant `json:"supply_accounts,omitempty"`
}

type CompositeRouteInput struct {
	PublicModel    string
	MatchType      string
	TargetPlatform string
	UpstreamModel  string
	Endpoint       string
	Priority       int
	Enabled        bool
	Notes          string
}

type CompositeModelRouteRepository interface {
	ListByGroup(ctx context.Context, groupID int64, includeDisabled bool) ([]CompositeModelRoute, error)
	Create(ctx context.Context, route *CompositeModelRoute) error
	Update(ctx context.Context, route *CompositeModelRoute) error
	Delete(ctx context.Context, id int64) error
	DeleteByGroup(ctx context.Context, groupID int64) error
}

func normalizeCompositeRouteEndpoint(endpoint string) string {
	endpoint = strings.ToLower(strings.TrimSpace(endpoint))
	if endpoint == "" {
		return CompositeRouteEndpointAny
	}
	switch endpoint {
	case CompositeRouteEndpointMessages,
		CompositeRouteEndpointCountTokens,
		CompositeRouteEndpointResponses,
		CompositeEndpointResponsesWS,
		CompositeRouteEndpointChatCompletions,
		CompositeRouteEndpointEmbeddings,
		CompositeRouteEndpointImages,
		CompositeRouteEndpointGemini,
		CompositeEndpointSystemOne,
		CompositeEndpointVideo,
		CompositeEndpointLive,
		CompositeEndpointAlphaSearch:
		return endpoint
	default:
		return CompositeRouteEndpointAny
	}
}

// CompositeEndpointForInboundPath 把真实请求路径归一为真实线上端点 CompositeEndpoint*，
// 仅供选号后归属核对 CompositeSelectedAccountServes 使用（alpha_search/live/systemone/video
// 表示真实线上命中的端点）。
// 管理端路由注册表的端点域是另一概念，由 routes/gateway.go 的
// compositeRouteEndpointForPath 维护（alpha/live 归 responses 域），供路由中间件匹配使用。
// 两者概念不同，禁止合并或互相复用。
func CompositeEndpointForInboundPath(path string) string {
	p := strings.TrimSpace(path)
	switch {
	case strings.Contains(p, "/messages/count_tokens"):
		return CompositeRouteEndpointCountTokens
	case strings.Contains(p, "/messages"):
		return CompositeRouteEndpointMessages
	case strings.Contains(p, "/systemone"):
		return CompositeEndpointSystemOne
	case strings.Contains(p, "/alpha/search"):
		return CompositeEndpointAlphaSearch
	case strings.Contains(p, "/realtime/calls"), strings.HasSuffix(strings.TrimRight(p, "/"), "/live"):
		return CompositeEndpointLive
	case strings.Contains(p, "/responses"):
		return CompositeRouteEndpointResponses
	case strings.Contains(p, "/chat/completions"):
		return CompositeRouteEndpointChatCompletions
	case strings.Contains(p, "/embeddings"):
		return CompositeRouteEndpointEmbeddings
	case strings.Contains(p, "/images/"):
		return CompositeRouteEndpointImages
	case strings.Contains(p, "/videos/"), strings.Contains(p, "/contents/generations/tasks"):
		return CompositeEndpointVideo
	case strings.Contains(p, "/v1beta/"):
		return CompositeRouteEndpointGemini
	default:
		return CompositeRouteEndpointAny
	}
}

func normalizeCompositeRouteMatchType(matchType string) string {
	matchType = strings.ToLower(strings.TrimSpace(matchType))
	switch matchType {
	case CompositeRouteMatchPrefix:
		return CompositeRouteMatchPrefix
	default:
		return CompositeRouteMatchExact
	}
}

func normalizeCompositeRouteInput(input CompositeRouteInput) CompositeRouteInput {
	input.PublicModel = strings.TrimSpace(input.PublicModel)
	input.MatchType = normalizeCompositeRouteMatchType(input.MatchType)
	input.TargetPlatform = strings.TrimSpace(input.TargetPlatform)
	input.UpstreamModel = strings.TrimSpace(input.UpstreamModel)
	input.Endpoint = normalizeCompositeRouteEndpoint(input.Endpoint)
	// 仅对 exact 路由把空 upstream_model 回填成 public_model：exact 命中时请求模型
	// 恒等于 public_model，回填只影响持久化/后台展示，保留原有契约不变。
	// prefix 路由留空则不回填——Resolve 会回退到具体请求模型，从而透传原始模型
	// （否则 public=deepseek-v4 的前缀路由会把 deepseek-v4-flash / deepseek-v4-pro
	// 都塌缩成固定的 deepseek-v4）。显式填写 upstream_model 时任何模式都原样固定转发。
	if input.UpstreamModel == "" && input.MatchType == CompositeRouteMatchExact {
		input.UpstreamModel = input.PublicModel
	}
	input.Notes = strings.TrimSpace(input.Notes)
	return input
}
