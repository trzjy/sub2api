//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCompositeEndpointForInboundPath 锁定真实入站路径到 CompositeEndpoint* 的唯一映射，
// 与 routes 中间件和选号后核对共用同一份归一化结果。
func TestCompositeEndpointForInboundPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{"count_tokens", "/v1/messages/count_tokens", CompositeRouteEndpointCountTokens},
		{"messages", "/v1/messages", CompositeRouteEndpointMessages},
		{"systemone", "/v1/systemone", CompositeEndpointSystemOne},
		{"chat_completions", "/v1/chat/completions", CompositeRouteEndpointChatCompletions},
		{"responses", "/v1/responses", CompositeRouteEndpointResponses},
		{"embeddings", "/v1/embeddings", CompositeRouteEndpointEmbeddings},
		{"images", "/v1/images/generations", CompositeRouteEndpointImages},
		{"video", "/v1/videos/generations", CompositeEndpointVideo},
		{"gemini", "/v1beta/models/x:generateContent", CompositeRouteEndpointGemini},
		{"live", "/v1/live", CompositeEndpointLive},
		{"realtime_calls", "/v1/realtime/calls", CompositeEndpointLive},
		{"unknown", "/v1/does-not-exist", CompositeRouteEndpointAny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, CompositeEndpointForInboundPath(tt.path))
		})
	}
}

// TestCompositeSelectedAccountServes 覆盖「选号后统一核对一次」的归属判定：
// 无决策放行、账号归属的 SupplyAccounts 白名单、显式路由/detector 的平台核对、
// 多平台同链按真实端点跨链命中、systemone 仅 TypeSafe 服务等失败关闭语义。
func TestCompositeSelectedAccountServes(t *testing.T) {
	const (
		anthropicAccountID   = int64(101)
		otherAccountID       = int64(999)
		antigravityAccountID = int64(202)
		systemOneAntID       = int64(303)
		systemOneSafeID      = int64(304)
	)

	anthropicAccount := &Account{ID: anthropicAccountID, Platform: PlatformAnthropic, Type: AccountTypeOAuth}
	openAIAccount := &Account{ID: 102, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	antigravityAccount := &Account{ID: antigravityAccountID, Platform: PlatformAntigravity, Type: AccountTypeOAuth}
	systemOneAntAccount := &Account{ID: systemOneAntID, Platform: PlatformAnthropic, Type: AccountTypeOAuth}
	systemOneSafeAccount := &Account{ID: systemOneSafeID, Platform: PlatformTypeSafe}

	noDecisionCtx := context.Background()

	accountOwnedHitCtx := WithCompositeRouteDecision(context.Background(), CompositeRouteDecision{
		Matched:        true,
		Source:         CompositeRouteSourceAccount,
		TargetPlatform: PlatformAnthropic,
		SupplyAccounts: []CompositeOwnershipClaimant{
			{AccountID: anthropicAccountID, Platform: PlatformAnthropic, Type: AccountTypeOAuth},
		},
	})
	accountOwnedMissCtx := WithCompositeRouteDecision(context.Background(), CompositeRouteDecision{
		Matched:        true,
		Source:         CompositeRouteSourceAccount,
		TargetPlatform: PlatformAnthropic,
		SupplyAccounts: []CompositeOwnershipClaimant{
			{AccountID: otherAccountID, Platform: PlatformAnthropic, Type: AccountTypeOAuth},
		},
	})
	explicitAnthropicCtx := WithCompositeRouteDecision(context.Background(), CompositeRouteDecision{
		Matched:        true,
		Source:         CompositeRouteSourceExplicit,
		TargetPlatform: PlatformAnthropic,
	})
	detectorAnthropicCtx := WithCompositeRouteDecision(context.Background(), CompositeRouteDecision{
		Matched:        true,
		Source:         CompositeRouteSourceDetector,
		TargetPlatform: PlatformAnthropic,
	})

	multiChainCtx := WithCompositeRouteDecision(context.Background(), CompositeRouteDecision{
		Matched:        true,
		Source:         CompositeRouteSourceAccount,
		TargetPlatform: PlatformAnthropic, // 链代表平台，非账号真实平台。
		SupplyAccounts: []CompositeOwnershipClaimant{
			{AccountID: antigravityAccountID, Platform: PlatformAntigravity, Type: AccountTypeOAuth},
		},
	})
	systemOneAntCtx := WithCompositeRouteDecision(context.Background(), CompositeRouteDecision{
		Matched:        true,
		Source:         CompositeRouteSourceAccount,
		TargetPlatform: PlatformAnthropic,
		SupplyAccounts: []CompositeOwnershipClaimant{
			{AccountID: systemOneAntID, Platform: PlatformAnthropic, Type: AccountTypeOAuth},
		},
	})
	systemOneSafeCtx := WithCompositeRouteDecision(context.Background(), CompositeRouteDecision{
		Matched:        true,
		Source:         CompositeRouteSourceAccount,
		TargetPlatform: PlatformAnthropic,
		SupplyAccounts: []CompositeOwnershipClaimant{
			{AccountID: systemOneSafeID, Platform: PlatformTypeSafe},
		},
	})

	tests := []struct {
		name         string
		ctx          context.Context
		endpoint     string
		account      *Account
		wantPath     string
		wantOK       bool
		nonEmptyPath bool
	}{
		{
			name:     "无决策放行",
			ctx:      noDecisionCtx,
			endpoint: CompositeRouteEndpointMessages,
			account:  anthropicAccount,
			wantPath: "",
			wantOK:   true,
		},
		{
			name:         "账号归属-SupplyAccounts命中-anthropic messages",
			ctx:          accountOwnedHitCtx,
			endpoint:     CompositeRouteEndpointMessages,
			account:      anthropicAccount,
			wantPath:     CompositePathGateway,
			wantOK:       true,
			nonEmptyPath: true,
		},
		{
			name:     "账号归属-SupplyAccounts未命中漏拒",
			ctx:      accountOwnedMissCtx,
			endpoint: CompositeRouteEndpointMessages,
			account:  anthropicAccount,
			wantPath: "",
			wantOK:   false,
		},
		{
			name:     "显式路由-平台不符拒绝",
			ctx:      explicitAnthropicCtx,
			endpoint: CompositeRouteEndpointMessages,
			account:  openAIAccount,
			wantPath: "",
			wantOK:   false,
		},
		{
			name:     "显式路由-平台相符放行",
			ctx:      explicitAnthropicCtx,
			endpoint: CompositeRouteEndpointMessages,
			account:  anthropicAccount,
			wantPath: CompositePathGateway,
			wantOK:   true,
		},
		{
			name:     "detector-平台不符拒绝",
			ctx:      detectorAnthropicCtx,
			endpoint: CompositeRouteEndpointMessages,
			account:  openAIAccount,
			wantPath: "",
			wantOK:   false,
		},
		{
			name:     "detector-平台相符放行",
			ctx:      detectorAnthropicCtx,
			endpoint: CompositeRouteEndpointMessages,
			account:  anthropicAccount,
			wantPath: CompositePathGateway,
			wantOK:   true,
		},
		{
			name:     "多平台同链-Antigravity OAuth 走 gemini 链不得误拒",
			ctx:      multiChainCtx,
			endpoint: CompositeRouteEndpointGemini,
			account:  antigravityAccount,
			wantPath: CompositePathAntigravity,
			wantOK:   true,
		},
		{
			name:     "systemone-SupplyAccounts 仅 Anthropic 账号-拒绝",
			ctx:      systemOneAntCtx,
			endpoint: CompositeEndpointSystemOne,
			account:  systemOneAntAccount,
			wantPath: "",
			wantOK:   false,
		},
		{
			name:     "systemone-SupplyAccounts 含 TypeSafe 账号-放行",
			ctx:      systemOneSafeCtx,
			endpoint: CompositeEndpointSystemOne,
			account:  systemOneSafeAccount,
			wantPath: CompositePathTypeSafe,
			wantOK:   true,
		},
		{
			name:     "systemone-真实端点优先于 TargetPlatform-anthropic 不匹配放行",
			ctx:      explicitAnthropicCtx,
			endpoint: CompositeEndpointSystemOne,
			account:  anthropicAccount,
			wantPath: "",
			wantOK:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, ok := CompositeSelectedAccountServes(tt.ctx, tt.endpoint, tt.account)
			require.Equal(t, tt.wantOK, ok)
			require.Equal(t, tt.wantPath, path)
			if tt.nonEmptyPath {
				require.NotEmpty(t, path)
			}
		})
	}
}
