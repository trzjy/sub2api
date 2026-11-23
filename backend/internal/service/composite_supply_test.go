//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// compositePathWant 是 account × chain × endpoint → 既有出站路径的期望表。
// 键为端点逻辑名；路径不存在表示该账号不能由该链服务本端点（ok=false）。
type compositePathWant map[string]string

// TestCompositeAccountChainPath 表驱动覆盖账号类型 × 三条执行链 × 关键端点，
// 逐条钉住方案 §4.4 的出站路径矩阵，并加入 openai/grok 账号覆盖专用端点。
func TestCompositeAccountChainPath(t *testing.T) {
	const (
		epChat        = CompositeEndpointChatCompletions
		epResponses   = CompositeEndpointResponses
		epMessages    = CompositeEndpointMessages
		epCountTokens = CompositeEndpointCountTokens
		epImages      = CompositeEndpointImages
		epSystemOne   = CompositeEndpointSystemOne
		epWS          = CompositeEndpointResponsesWS
	)

	accounts := []struct {
		name    string
		account *Account
		want    map[string]compositePathWant // chain → endpoint → path
	}{
		{
			name:    "zhipu APIKey",
			account: &Account{Platform: PlatformZhipu, Type: AccountTypeAPIKey},
			want: map[string]compositePathWant{
				CompositeChainOpenAI: {
					epChat: CompositePathOpenAI, epResponses: CompositePathOpenAI,
					epMessages: CompositePathOpenAI, epCountTokens: CompositePathOpenAI,
				},
				CompositeChainAnthropic: {},
				CompositeChainGemini:    {},
			},
		},
		{
			name: "web access mode",
			account: &Account{
				Platform: PlatformZhipu, Type: AccountTypeAPIKey,
				Credentials: map[string]any{"access_mode": "web"},
			},
			want: map[string]compositePathWant{
				// 三文本端点可服务，但 /v1/messages 被两条调度器排除。
				CompositeChainOpenAI: {
					epChat: CompositePathWeb, epResponses: CompositePathWeb,
					epCountTokens: CompositePathWeb,
				},
				CompositeChainAnthropic: {},
				CompositeChainGemini:    {},
			},
		},
		{
			name:    "codebuddy",
			account: &Account{Platform: PlatformCodeBuddy, Type: AccountTypeOAuth},
			want: map[string]compositePathWant{
				CompositeChainOpenAI: {
					epChat: CompositePathCodeBuddy, epResponses: CompositePathCodeBuddy,
					epMessages: CompositePathCodeBuddy, epCountTokens: CompositePathCodeBuddy,
				},
				CompositeChainAnthropic: {},
				CompositeChainGemini:    {},
			},
		},
		{
			name:    "cursor Connect",
			account: &Account{Platform: PlatformCursor, Type: AccountTypeOAuth},
			want: map[string]compositePathWant{
				CompositeChainOpenAI: {
					epChat: CompositePathCursor, epResponses: CompositePathCursor,
					epMessages: CompositePathCursor, epCountTokens: CompositePathCursor,
				},
				CompositeChainAnthropic: {
					epChat: CompositePathCursor, epResponses: CompositePathCursor,
					epMessages: CompositePathCursor,
				},
				CompositeChainGemini: {epMessages: CompositePathGeminiCompat},
			},
		},
		{
			name:    "cursor APIKey",
			account: &Account{Platform: PlatformCursor, Type: AccountTypeAPIKey},
			want: map[string]compositePathWant{
				CompositeChainOpenAI: {
					epChat: CompositePathOpenAI, epResponses: CompositePathOpenAI,
					epMessages: CompositePathOpenAI, epCountTokens: CompositePathOpenAI,
				},
				CompositeChainAnthropic: {},
				CompositeChainGemini:    {epMessages: CompositePathGeminiCompat},
			},
		},
		{
			name:    "devin",
			account: &Account{Platform: PlatformDevin, Type: AccountTypeOAuth},
			want: map[string]compositePathWant{
				CompositeChainOpenAI: {
					epChat: CompositePathDevin, epResponses: CompositePathDevin,
					epMessages: CompositePathDevin, epCountTokens: CompositePathDevin,
				},
				CompositeChainAnthropic: {
					epChat: CompositePathDevin, epResponses: CompositePathDevin,
					epMessages: CompositePathDevin,
				},
				CompositeChainGemini: {epChat: CompositePathDevin, epMessages: CompositePathDevin},
			},
		},
		{
			name:    "antigravity",
			account: &Account{Platform: PlatformAntigravity, Type: AccountTypeOAuth},
			want: map[string]compositePathWant{
				// 不进 OpenAI 链。
				CompositeChainOpenAI: {},
				CompositeChainAnthropic: {
					epChat: CompositePathAntigravity, epResponses: CompositePathAntigravity,
					epMessages: CompositePathAntigravity,
				},
				CompositeChainGemini: {
					epChat: CompositePathAntigravity, epMessages: CompositePathAntigravity,
				},
			},
		},
		{
			name:    "anthropic",
			account: &Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth},
			want: map[string]compositePathWant{
				CompositeChainOpenAI: {},
				CompositeChainAnthropic: {
					epChat: CompositePathGateway, epResponses: CompositePathGateway,
					epMessages: CompositePathGateway, epCountTokens: CompositePathGateway,
				},
				CompositeChainGemini: {},
			},
		},
		{
			name:    "typesafe",
			account: &Account{Platform: PlatformTypeSafe, Type: AccountTypeAPIKey},
			want: map[string]compositePathWant{
				CompositeChainOpenAI:    {},
				CompositeChainAnthropic: {epSystemOne: CompositePathTypeSafe},
				CompositeChainGemini:    {},
			},
		},
		{
			name:    "gemini",
			account: &Account{Platform: PlatformGemini, Type: AccountTypeOAuth},
			want: map[string]compositePathWant{
				// responses 三链皆不可。
				CompositeChainOpenAI: {},
				CompositeChainAnthropic: {
					epChat: CompositePathGemini,
				},
				CompositeChainGemini: {
					epChat: CompositePathGemini, epMessages: CompositePathGeminiCompat,
				},
			},
		},
		{
			name:    "unknown",
			account: &Account{Platform: "unknown-platform", Type: AccountTypeAPIKey},
			want: map[string]compositePathWant{
				CompositeChainOpenAI:    {},
				CompositeChainAnthropic: {},
				CompositeChainGemini:    {},
			},
		},
		{
			name:    "openai APIKey",
			account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
			want: map[string]compositePathWant{
				CompositeChainOpenAI: {
					epChat: CompositePathOpenAI, epResponses: CompositePathOpenAI,
					epMessages: CompositePathOpenAI, epCountTokens: CompositePathOpenAI,
					epImages: CompositePathOpenAI, epWS: CompositePathOpenAI,
				},
				CompositeChainAnthropic: {},
				CompositeChainGemini:    {},
			},
		},
		{
			name:    "grok APIKey",
			account: &Account{Platform: PlatformGrok, Type: AccountTypeAPIKey},
			want: map[string]compositePathWant{
				CompositeChainOpenAI: {
					epChat: CompositePathGrok, epResponses: CompositePathGrok,
					epMessages: CompositePathGrok, epCountTokens: CompositePathGrok,
					epWS: CompositePathGrok,
					// images 仅 PlatformOpenAI；video 仅 grok（见 §4.4 向量子测试）。
				},
				CompositeChainAnthropic: {},
				CompositeChainGemini:    {},
			},
		},
	}

	chains := []string{CompositeChainOpenAI, CompositeChainAnthropic, CompositeChainGemini}
	endpoints := []string{epChat, epResponses, epMessages, epCountTokens, epImages, epSystemOne, epWS}

	for _, tc := range accounts {
		for _, chain := range chains {
			chainWant := tc.want[chain]
			for _, endpoint := range endpoints {
				wantPath, wantOK := chainWant[endpoint]
				gotPath, gotOK := CompositeAccountChainPath(chain, endpoint, tc.account)
				require.Equalf(t, wantOK, gotOK,
					"%s × %s × %s: ok（wantPath=%q gotPath=%q）", tc.name, chain, endpoint, wantPath, gotPath)
				if wantOK {
					require.Equalf(t, wantPath, gotPath,
						"%s × %s × %s: path", tc.name, chain, endpoint)
				} else {
					require.Emptyf(t, gotPath,
						"%s × %s × %s: ok=false 时 path 应为空", tc.name, chain, endpoint)
				}
			}
		}
	}
}

// TestCompositeAccountChainPathPlanVectors 逐条钉住方案 §4.4 的测试向量，
// 与上面的整表互为正交断言；另外覆盖 nil、未知 chain/endpoint、video 与
// images/embeddings/live/alpha 的专用端点约束。
func TestCompositeAccountChainPathPlanVectors(t *testing.T) {
	zhipu := &Account{Platform: PlatformZhipu, Type: AccountTypeAPIKey}
	cursorConnect := &Account{Platform: PlatformCursor, Type: AccountTypeOAuth}
	cursorAPIKey := &Account{Platform: PlatformCursor, Type: AccountTypeAPIKey}
	antigravity := &Account{Platform: PlatformAntigravity, Type: AccountTypeOAuth}
	typesafe := &Account{Platform: PlatformTypeSafe, Type: AccountTypeAPIKey}
	gemini := &Account{Platform: PlatformGemini, Type: AccountTypeOAuth}
	openai := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	grok := &Account{Platform: PlatformGrok, Type: AccountTypeAPIKey}

	chains := []string{CompositeChainOpenAI, CompositeChainAnthropic, CompositeChainGemini}

	serviceable := func(account *Account, endpoint string, chain string) bool {
		_, ok := CompositeAccountChainPath(chain, endpoint, account)
		return ok
	}

	// zhipu 三文本端点仅 openai 链。
	for _, endpoint := range []string{
		CompositeEndpointChatCompletions, CompositeEndpointResponses, CompositeEndpointMessages,
	} {
		require.True(t, serviceable(zhipu, endpoint, CompositeChainOpenAI), "zhipu %s openai", endpoint)
		require.False(t, serviceable(zhipu, endpoint, CompositeChainAnthropic), "zhipu %s anthropic", endpoint)
		require.False(t, serviceable(zhipu, endpoint, CompositeChainGemini), "zhipu %s gemini", endpoint)
	}

	// cursor APIKey：chat 仅 openai；messages openai/gemini，不进 anthropic。
	require.True(t, serviceable(cursorAPIKey, CompositeEndpointChatCompletions, CompositeChainOpenAI))
	require.False(t, serviceable(cursorAPIKey, CompositeEndpointChatCompletions, CompositeChainAnthropic))
	require.False(t, serviceable(cursorAPIKey, CompositeEndpointChatCompletions, CompositeChainGemini))
	require.True(t, serviceable(cursorAPIKey, CompositeEndpointMessages, CompositeChainOpenAI))
	require.True(t, serviceable(cursorAPIKey, CompositeEndpointMessages, CompositeChainGemini))
	require.False(t, serviceable(cursorAPIKey, CompositeEndpointMessages, CompositeChainAnthropic))

	// cursor Connect：messages 三链皆可。
	for _, chain := range chains {
		require.True(t, serviceable(cursorConnect, CompositeEndpointMessages, chain), "cursor Connect messages %s", chain)
	}

	// antigravity 不进 openai（三文本端点）。
	for _, endpoint := range []string{
		CompositeEndpointChatCompletions, CompositeEndpointResponses, CompositeEndpointMessages,
	} {
		require.False(t, serviceable(antigravity, endpoint, CompositeChainOpenAI), "antigravity %s openai", endpoint)
	}

	// typesafe 仅 anthropic/systemone。
	for _, chain := range chains {
		for _, endpoint := range []string{
			CompositeEndpointChatCompletions, CompositeEndpointResponses, CompositeEndpointMessages,
			CompositeEndpointCountTokens, CompositeEndpointImages, CompositeEndpointSystemOne,
			CompositeEndpointResponsesWS,
		} {
			want := chain == CompositeChainAnthropic && endpoint == CompositeEndpointSystemOne
			require.Equal(t, want, serviceable(typesafe, endpoint, chain),
				"typesafe %s %s", endpoint, chain)
		}
	}

	// gemini responses 三链不可。
	for _, chain := range chains {
		require.False(t, serviceable(gemini, CompositeEndpointResponses, chain), "gemini responses %s", chain)
	}

	// WS 仅 openai/grok 账号，且仅 openai 链。
	for _, chain := range chains {
		require.Equal(t, chain == CompositeChainOpenAI,
			serviceable(openai, CompositeEndpointResponsesWS, chain), "openai WS %s", chain)
		require.Equal(t, chain == CompositeChainOpenAI,
			serviceable(grok, CompositeEndpointResponsesWS, chain), "grok WS %s", chain)
		require.False(t, serviceable(zhipu, CompositeEndpointResponsesWS, chain), "zhipu WS %s", chain)
	}

	// images / embeddings / live / alpha 仅 openai 账号且仅 openai 链。
	for _, endpoint := range []string{
		CompositeEndpointImages, CompositeEndpointEmbeddings, CompositeEndpointLive,
		CompositeEndpointAlphaSearch,
	} {
		_, ok := CompositeAccountChainPath(CompositeChainOpenAI, endpoint, openai)
		require.True(t, ok, "openai %s", endpoint)
		_, ok = CompositeAccountChainPath(CompositeChainOpenAI, endpoint, grok)
		require.False(t, ok, "grok %s", endpoint)
	}

	// video 仅 grok 账号且仅 openai 链。
	videoPath, ok := CompositeAccountChainPath(CompositeChainOpenAI, CompositeEndpointVideo, grok)
	require.True(t, ok)
	require.Equal(t, CompositePathGrok, videoPath)
	for _, account := range []*Account{openai, zhipu, cursorConnect, antigravity, gemini} {
		_, ok = CompositeAccountChainPath(CompositeChainOpenAI, CompositeEndpointVideo, account)
		require.False(t, ok)
		for _, chain := range []string{CompositeChainAnthropic, CompositeChainGemini} {
			_, ok = CompositeAccountChainPath(chain, CompositeEndpointVideo, account)
			require.False(t, ok)
		}
	}

	// nil 安全；未知 chain / endpoint 返回 ok=false 且 path 为空。
	var nilAccount *Account
	for _, chain := range chains {
		path, ok := CompositeAccountChainPath(chain, CompositeEndpointChatCompletions, nilAccount)
		require.False(t, ok)
		require.Empty(t, path)
	}
	for _, bad := range []struct{ chain, endpoint string }{
		{"unknown", CompositeEndpointChatCompletions},
		{CompositeChainOpenAI, "unknown_endpoint"},
		{"", ""},
	} {
		path, ok := CompositeAccountChainPath(bad.chain, bad.endpoint, openai)
		require.False(t, ok, "%+v", bad)
		require.Empty(t, path)
	}
}

// TestCompositeClaimantChainPath 用与 Account 版相同的账号向量构造 claimant 摘要，
// 逐格断言两版在三条链 × 全部端点上的结果完全一致（path 与 ok 均逐位比对）。
func TestCompositeClaimantChainPath(t *testing.T) {
	pairs := []struct {
		name     string
		claimant CompositeOwnershipClaimant
		account  *Account
	}{
		{
			name:     "zhipu APIKey",
			claimant: CompositeOwnershipClaimant{Platform: PlatformZhipu, Type: string(AccountTypeAPIKey)},
			account:  &Account{Platform: PlatformZhipu, Type: AccountTypeAPIKey},
		},
		{
			name:     "web mode",
			claimant: CompositeOwnershipClaimant{Platform: PlatformZhipu, Type: string(AccountTypeAPIKey), WebAccessMode: true},
			account:  &Account{Platform: PlatformZhipu, Type: AccountTypeAPIKey, Credentials: map[string]any{"access_mode": "web"}},
		},
		{
			name:     "codebuddy",
			claimant: CompositeOwnershipClaimant{Platform: PlatformCodeBuddy, Type: string(AccountTypeOAuth)},
			account:  &Account{Platform: PlatformCodeBuddy, Type: AccountTypeOAuth},
		},
		{
			name:     "cursor Connect",
			claimant: CompositeOwnershipClaimant{Platform: PlatformCursor, Type: string(AccountTypeOAuth), CursorConnect: true},
			account:  &Account{Platform: PlatformCursor, Type: AccountTypeOAuth},
		},
		{
			name:     "cursor APIKey",
			claimant: CompositeOwnershipClaimant{Platform: PlatformCursor, Type: string(AccountTypeAPIKey)},
			account:  &Account{Platform: PlatformCursor, Type: AccountTypeAPIKey},
		},
		{
			name:     "devin",
			claimant: CompositeOwnershipClaimant{Platform: PlatformDevin, Type: string(AccountTypeOAuth)},
			account:  &Account{Platform: PlatformDevin, Type: AccountTypeOAuth},
		},
		{
			name:     "antigravity OAuth",
			claimant: CompositeOwnershipClaimant{Platform: PlatformAntigravity, Type: string(AccountTypeOAuth)},
			account:  &Account{Platform: PlatformAntigravity, Type: AccountTypeOAuth},
		},
		{
			name:     "antigravity APIKey",
			claimant: CompositeOwnershipClaimant{Platform: PlatformAntigravity, Type: string(AccountTypeAPIKey)},
			account:  &Account{Platform: PlatformAntigravity, Type: AccountTypeAPIKey},
		},
		{
			name:     "anthropic",
			claimant: CompositeOwnershipClaimant{Platform: PlatformAnthropic, Type: string(AccountTypeOAuth)},
			account:  &Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth},
		},
		{
			name:     "typesafe",
			claimant: CompositeOwnershipClaimant{Platform: PlatformTypeSafe, Type: string(AccountTypeAPIKey)},
			account:  &Account{Platform: PlatformTypeSafe, Type: AccountTypeAPIKey},
		},
		{
			name:     "gemini",
			claimant: CompositeOwnershipClaimant{Platform: PlatformGemini, Type: string(AccountTypeOAuth)},
			account:  &Account{Platform: PlatformGemini, Type: AccountTypeOAuth},
		},
		{
			name:     "openai APIKey",
			claimant: CompositeOwnershipClaimant{Platform: PlatformOpenAI, Type: string(AccountTypeAPIKey)},
			account:  &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
		},
		{
			name:     "grok APIKey",
			claimant: CompositeOwnershipClaimant{Platform: PlatformGrok, Type: string(AccountTypeAPIKey)},
			account:  &Account{Platform: PlatformGrok, Type: AccountTypeAPIKey},
		},
		{
			name:     "unknown platform",
			claimant: CompositeOwnershipClaimant{Platform: "unknown-platform", Type: string(AccountTypeAPIKey)},
			account:  &Account{Platform: "unknown-platform", Type: AccountTypeAPIKey},
		},
		{
			name:     "other platform",
			claimant: CompositeOwnershipClaimant{Platform: PlatformOther, Type: string(AccountTypeAPIKey)},
			account:  &Account{Platform: PlatformOther, Type: AccountTypeAPIKey},
		},
	}

	chains := []string{CompositeChainOpenAI, CompositeChainAnthropic, CompositeChainGemini}
	endpoints := []string{
		CompositeEndpointChatCompletions, CompositeEndpointResponses, CompositeEndpointMessages,
		CompositeEndpointCountTokens, CompositeEndpointImages, CompositeEndpointEmbeddings,
		CompositeEndpointLive, CompositeEndpointAlphaSearch, CompositeEndpointSystemOne,
		CompositeEndpointResponsesWS, CompositeEndpointVideo,
	}

	for _, tc := range pairs {
		for _, chain := range chains {
			for _, endpoint := range endpoints {
				wantPath, wantOK := CompositeAccountChainPath(chain, endpoint, tc.account)
				gotPath, gotOK := CompositeClaimantChainPath(chain, endpoint, tc.claimant)
				require.Equalf(t, wantOK, gotOK, "%s × %s × %s: ok", tc.name, chain, endpoint)
				require.Equalf(t, wantPath, gotPath, "%s × %s × %s: path", tc.name, chain, endpoint)
			}
		}
	}

	// 额外钉住方案 §4.4 的关键语义，防止两版同时回归时逐格比对漏检。
	web := CompositeOwnershipClaimant{Platform: PlatformZhipu, Type: string(AccountTypeAPIKey), WebAccessMode: true}
	_, ok := CompositeClaimantChainPath(CompositeChainOpenAI, CompositeEndpointMessages, web)
	require.False(t, ok, "web mode messages 不可服务")

	cursorConnect := CompositeOwnershipClaimant{Platform: PlatformCursor, Type: string(AccountTypeOAuth), CursorConnect: true}
	for _, chain := range chains {
		_, ok = CompositeClaimantChainPath(chain, CompositeEndpointMessages, cursorConnect)
		require.True(t, ok, "cursor Connect messages 三链皆可：%s", chain)
	}

	antigravity := CompositeOwnershipClaimant{Platform: PlatformAntigravity, Type: string(AccountTypeOAuth)}
	_, ok = CompositeClaimantChainPath(CompositeChainOpenAI, CompositeEndpointChatCompletions, antigravity)
	require.False(t, ok, "antigravity 不进 openai")

	typesafe := CompositeOwnershipClaimant{Platform: PlatformTypeSafe, Type: string(AccountTypeAPIKey)}
	for _, chain := range chains {
		for _, endpoint := range []string{
			CompositeEndpointChatCompletions, CompositeEndpointResponses, CompositeEndpointMessages,
			CompositeEndpointCountTokens, CompositeEndpointImages, CompositeEndpointSystemOne,
			CompositeEndpointResponsesWS,
		} {
			want := chain == CompositeChainAnthropic && endpoint == CompositeEndpointSystemOne
			_, got := CompositeClaimantChainPath(chain, endpoint, typesafe)
			require.Equal(t, want, got, "typesafe %s %s", endpoint, chain)
		}
	}

	gemini := CompositeOwnershipClaimant{Platform: PlatformGemini, Type: string(AccountTypeOAuth)}
	for _, chain := range chains {
		_, ok = CompositeClaimantChainPath(chain, CompositeEndpointResponses, gemini)
		require.False(t, ok, "gemini responses 三链不可：%s", chain)
	}

	// 空摘要（字段全零）与未知 chain 走与 Account 版一致的默认拒绝路径。
	path, ok := CompositeClaimantChainPath(CompositeChainOpenAI, CompositeEndpointChatCompletions, CompositeOwnershipClaimant{})
	require.False(t, ok)
	require.Empty(t, path)
	path, ok = CompositeClaimantChainPath("unknown", CompositeEndpointChatCompletions, cursorConnect)
	require.False(t, ok)
	require.Empty(t, path)
}

// TestCompositeAccountChainPathGeminiEndpoint 钉住 /v1beta（CompositeEndpointGemini）
// 的实际账号准入与出口标识，逐条镜像 GeminiV1BetaModels 与 ForwardNative：
// devin 被明确排除；antigravity OAuth 走 AG 出口；gemini 走原生出口；
// cursor（Connect 会话与 OpenAI 兼容 apikey 反代）走 gemini compat 桥接；
// zhipu / anthropic / 未知平台失败关闭。
func TestCompositeAccountChainPathGeminiEndpoint(t *testing.T) {
	cases := []struct {
		name     string
		account  *Account
		wantPath string
		wantOK   bool
	}{
		{name: "gemini OAuth", account: &Account{Platform: PlatformGemini, Type: AccountTypeOAuth}, wantPath: CompositePathGemini, wantOK: true},
		{name: "gemini APIKey", account: &Account{Platform: PlatformGemini, Type: AccountTypeAPIKey}, wantPath: CompositePathGemini, wantOK: true},
		{name: "antigravity OAuth", account: &Account{Platform: PlatformAntigravity, Type: AccountTypeOAuth}, wantPath: CompositePathAntigravity, wantOK: true},
		{name: "antigravity APIKey", account: &Account{Platform: PlatformAntigravity, Type: AccountTypeAPIKey}, wantOK: false},
		{name: "cursor Connect", account: &Account{Platform: PlatformCursor, Type: AccountTypeOAuth}, wantPath: CompositePathGeminiCompat, wantOK: true},
		{name: "cursor APIKey", account: &Account{Platform: PlatformCursor, Type: AccountTypeAPIKey}, wantPath: CompositePathGeminiCompat, wantOK: true},
		{name: "devin excluded", account: &Account{Platform: PlatformDevin, Type: AccountTypeOAuth}, wantOK: false},
		{name: "zhipu", account: &Account{Platform: PlatformZhipu, Type: AccountTypeAPIKey}, wantOK: false},
		{name: "anthropic", account: &Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth}, wantOK: false},
		{name: "unknown", account: &Account{Platform: "unknown-platform", Type: AccountTypeAPIKey}, wantOK: false},
	}

	for _, tc := range cases {
		path, ok := CompositeAccountChainPath(CompositeChainGemini, CompositeEndpointGemini, tc.account)
		require.Equalf(t, tc.wantOK, ok, "%s: ok", tc.name)
		require.Equalf(t, tc.wantPath, path, "%s: path（ok=false 时应为空）", tc.name)
	}

	// /v1beta 只属于 Gemini 链：其它链不得因本端点而放宽既有判定。
	for _, chain := range []string{CompositeChainOpenAI, CompositeChainAnthropic} {
		for _, account := range []*Account{
			{Platform: PlatformGemini, Type: AccountTypeOAuth},
			{Platform: PlatformAntigravity, Type: AccountTypeOAuth},
			{Platform: PlatformCursor, Type: AccountTypeOAuth},
		} {
			path, ok := CompositeAccountChainPath(chain, CompositeEndpointGemini, account)
			require.Falsef(t, ok, "%s 链不应服务 /v1beta", chain)
			require.Emptyf(t, path, "%s 链 /v1beta path 应为空", chain)
		}
	}
}

// TestCompositeClaimantChainPathGeminiEndpoint 以 claimant 摘要复刻上表的账号向量，
// 断言 Account 版与 claimant 版在 /v1beta 端点上 path 与 ok 逐位一致。
func TestCompositeClaimantChainPathGeminiEndpoint(t *testing.T) {
	cases := []struct {
		name     string
		account  *Account
		claimant CompositeOwnershipClaimant
		wantPath string
		wantOK   bool
	}{
		{
			name:     "gemini OAuth",
			account:  &Account{Platform: PlatformGemini, Type: AccountTypeOAuth},
			claimant: CompositeOwnershipClaimant{Platform: PlatformGemini, Type: string(AccountTypeOAuth)},
			wantPath: CompositePathGemini, wantOK: true,
		},
		{
			name:     "gemini APIKey",
			account:  &Account{Platform: PlatformGemini, Type: AccountTypeAPIKey},
			claimant: CompositeOwnershipClaimant{Platform: PlatformGemini, Type: string(AccountTypeAPIKey)},
			wantPath: CompositePathGemini, wantOK: true,
		},
		{
			name:     "antigravity OAuth",
			account:  &Account{Platform: PlatformAntigravity, Type: AccountTypeOAuth},
			claimant: CompositeOwnershipClaimant{Platform: PlatformAntigravity, Type: string(AccountTypeOAuth)},
			wantPath: CompositePathAntigravity, wantOK: true,
		},
		{
			name:     "antigravity APIKey",
			account:  &Account{Platform: PlatformAntigravity, Type: AccountTypeAPIKey},
			claimant: CompositeOwnershipClaimant{Platform: PlatformAntigravity, Type: string(AccountTypeAPIKey)},
			wantOK:   false,
		},
		{
			name:     "cursor Connect",
			account:  &Account{Platform: PlatformCursor, Type: AccountTypeOAuth},
			claimant: CompositeOwnershipClaimant{Platform: PlatformCursor, Type: string(AccountTypeOAuth), CursorConnect: true},
			wantPath: CompositePathGeminiCompat, wantOK: true,
		},
		{
			name:     "cursor APIKey",
			account:  &Account{Platform: PlatformCursor, Type: AccountTypeAPIKey},
			claimant: CompositeOwnershipClaimant{Platform: PlatformCursor, Type: string(AccountTypeAPIKey)},
			wantPath: CompositePathGeminiCompat, wantOK: true,
		},
		{
			name:     "devin excluded",
			account:  &Account{Platform: PlatformDevin, Type: AccountTypeOAuth},
			claimant: CompositeOwnershipClaimant{Platform: PlatformDevin, Type: string(AccountTypeOAuth)},
			wantOK:   false,
		},
		{
			name:     "zhipu",
			account:  &Account{Platform: PlatformZhipu, Type: AccountTypeAPIKey},
			claimant: CompositeOwnershipClaimant{Platform: PlatformZhipu, Type: string(AccountTypeAPIKey)},
			wantOK:   false,
		},
		{
			name:     "anthropic",
			account:  &Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth},
			claimant: CompositeOwnershipClaimant{Platform: PlatformAnthropic, Type: string(AccountTypeOAuth)},
			wantOK:   false,
		},
		{
			name:     "unknown",
			account:  &Account{Platform: "unknown-platform", Type: AccountTypeAPIKey},
			claimant: CompositeOwnershipClaimant{Platform: "unknown-platform", Type: string(AccountTypeAPIKey)},
			wantOK:   false,
		},
	}

	for _, tc := range cases {
		accountPath, accountOK := CompositeAccountChainPath(CompositeChainGemini, CompositeEndpointGemini, tc.account)
		require.Equalf(t, tc.wantOK, accountOK, "%s: Account ok", tc.name)
		require.Equalf(t, tc.wantPath, accountPath, "%s: Account path", tc.name)

		claimantPath, claimantOK := CompositeClaimantChainPath(CompositeChainGemini, CompositeEndpointGemini, tc.claimant)
		require.Equalf(t, accountOK, claimantOK, "%s: claimant ok 与 Account 逐位一致", tc.name)
		require.Equalf(t, accountPath, claimantPath, "%s: claimant path 与 Account 逐位一致", tc.name)
	}
}

// TestCompositeAnyEndpointChainPath 钉住 any 端点（CompositeRouteEndpointAny）的
// 链级默认资格：账号属于该链时按该链默认文本端点同口径签发默认路径，不属于
// 该链则 false。any 供 handler 门禁与模型级调度使用，与具体端点判定正交。
// openai/anthropic 链取 chat_completions 口径，gemini 链取 messages 口径
// （默认出口 gemini-compat）。Account 版与 claimant 版逐格比对。
func TestCompositeAnyEndpointChainPath(t *testing.T) {
	const epAny = CompositeRouteEndpointAny

	cases := []struct {
		name     string
		account  *Account
		claimant CompositeOwnershipClaimant
		want     map[string]string // chain → 默认路径；缺省表示不属于该链（false）
	}{
		{
			name:     "zhipu APIKey",
			account:  &Account{Platform: PlatformZhipu, Type: AccountTypeAPIKey},
			claimant: CompositeOwnershipClaimant{Platform: PlatformZhipu, Type: string(AccountTypeAPIKey)},
			want:     map[string]string{CompositeChainOpenAI: CompositePathOpenAI},
		},
		{
			name:     "web access mode",
			account:  &Account{Platform: PlatformZhipu, Type: AccountTypeAPIKey, Credentials: map[string]any{"access_mode": "web"}},
			claimant: CompositeOwnershipClaimant{Platform: PlatformZhipu, Type: string(AccountTypeAPIKey), WebAccessMode: true},
			want:     map[string]string{CompositeChainOpenAI: CompositePathWeb},
		},
		{
			name:     "codebuddy",
			account:  &Account{Platform: PlatformCodeBuddy, Type: AccountTypeOAuth},
			claimant: CompositeOwnershipClaimant{Platform: PlatformCodeBuddy, Type: string(AccountTypeOAuth)},
			want:     map[string]string{CompositeChainOpenAI: CompositePathCodeBuddy},
		},
		{
			name:     "cursor Connect",
			account:  &Account{Platform: PlatformCursor, Type: AccountTypeOAuth},
			claimant: CompositeOwnershipClaimant{Platform: PlatformCursor, Type: string(AccountTypeOAuth), CursorConnect: true},
			want: map[string]string{
				CompositeChainOpenAI:    CompositePathCursor,
				CompositeChainAnthropic: CompositePathCursor,
				CompositeChainGemini:    CompositePathGeminiCompat,
			},
		},
		{
			name:     "cursor APIKey",
			account:  &Account{Platform: PlatformCursor, Type: AccountTypeAPIKey},
			claimant: CompositeOwnershipClaimant{Platform: PlatformCursor, Type: string(AccountTypeAPIKey)},
			want: map[string]string{
				CompositeChainOpenAI: CompositePathOpenAI,
				CompositeChainGemini: CompositePathGeminiCompat,
			},
		},
		{
			name:     "devin",
			account:  &Account{Platform: PlatformDevin, Type: AccountTypeOAuth},
			claimant: CompositeOwnershipClaimant{Platform: PlatformDevin, Type: string(AccountTypeOAuth)},
			want: map[string]string{
				CompositeChainOpenAI:    CompositePathDevin,
				CompositeChainAnthropic: CompositePathDevin,
				CompositeChainGemini:    CompositePathDevin,
			},
		},
		{
			name:     "antigravity OAuth",
			account:  &Account{Platform: PlatformAntigravity, Type: AccountTypeOAuth},
			claimant: CompositeOwnershipClaimant{Platform: PlatformAntigravity, Type: string(AccountTypeOAuth)},
			want: map[string]string{
				CompositeChainAnthropic: CompositePathAntigravity,
				CompositeChainGemini:    CompositePathAntigravity,
			},
		},
		{
			name:     "anthropic",
			account:  &Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth},
			claimant: CompositeOwnershipClaimant{Platform: PlatformAnthropic, Type: string(AccountTypeOAuth)},
			want:     map[string]string{CompositeChainAnthropic: CompositePathGateway},
		},
		{
			name:     "gemini",
			account:  &Account{Platform: PlatformGemini, Type: AccountTypeOAuth},
			claimant: CompositeOwnershipClaimant{Platform: PlatformGemini, Type: string(AccountTypeOAuth)},
			want: map[string]string{
				CompositeChainAnthropic: CompositePathGemini,
				CompositeChainGemini:    CompositePathGeminiCompat,
			},
		},
		{
			name:     "typesafe",
			account:  &Account{Platform: PlatformTypeSafe, Type: AccountTypeAPIKey},
			claimant: CompositeOwnershipClaimant{Platform: PlatformTypeSafe, Type: string(AccountTypeAPIKey)},
			want:     map[string]string{},
		},
		{
			name:     "openai APIKey",
			account:  &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
			claimant: CompositeOwnershipClaimant{Platform: PlatformOpenAI, Type: string(AccountTypeAPIKey)},
			want:     map[string]string{CompositeChainOpenAI: CompositePathOpenAI},
		},
		{
			name:     "grok APIKey",
			account:  &Account{Platform: PlatformGrok, Type: AccountTypeAPIKey},
			claimant: CompositeOwnershipClaimant{Platform: PlatformGrok, Type: string(AccountTypeAPIKey)},
			want:     map[string]string{CompositeChainOpenAI: CompositePathGrok},
		},
		{
			name:     "unknown",
			account:  &Account{Platform: "unknown-platform", Type: AccountTypeAPIKey},
			claimant: CompositeOwnershipClaimant{Platform: "unknown-platform", Type: string(AccountTypeAPIKey)},
			want:     map[string]string{},
		},
	}

	chains := []string{CompositeChainOpenAI, CompositeChainAnthropic, CompositeChainGemini}
	for _, tc := range cases {
		for _, chain := range chains {
			wantPath, wantOK := tc.want[chain]
			accountPath, accountOK := CompositeAccountChainPath(chain, epAny, tc.account)
			require.Equalf(t, wantOK, accountOK, "%s × %s any: Account ok（wantPath=%q gotPath=%q）", tc.name, chain, wantPath, accountPath)
			require.Equalf(t, wantPath, accountPath, "%s × %s any: Account path", tc.name, chain)

			claimantPath, claimantOK := CompositeClaimantChainPath(chain, epAny, tc.claimant)
			require.Equalf(t, wantOK, claimantOK, "%s × %s any: claimant ok", tc.name, chain)
			require.Equalf(t, wantPath, claimantPath, "%s × %s any: claimant path 与 Account 一致", tc.name, chain)
		}
	}
}
