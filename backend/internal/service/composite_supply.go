package service

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/domain"
)

// 本文件提供 composite 跨链 selector / 门禁共用的「账号 × 端点 × 执行链」资格纯函数。
// 这里的每条分支都逐条镜像既有出站分派，不引入任何新的平台清单或兜底分支：
//   - OpenAI 链：Account.IsOpenAICompatible() 与 OpenAIGatewayService.Forward
//     的平台分流（grok / codebuddy / cursor / devin / web / 通用 OpenAI）；
//     文本端点之外的专用端点按 routes/gateway.go 的入口平台过滤复刻。
//   - Anthropic 链：handler 的 messagesNonGeminiForwardPath /
//     chatCompletionsAccountForwardPath / responsesAccountForwardPath 与
//     GatewayService.Forward{AsChatCompletions,AsResponses} 的 cursor Connect 前置分支。
//   - Gemini 链：handler 的 messagesAccountForwardPath 与 gemini 分组准入
//     geminiGroupAdmitsAccount，以及 chatCompletionsAccountForwardPath 的
//     devin / gemini / antigravity 分支（responses 无 gemini 分支）；/v1beta
//     端点按 GeminiV1BetaModels 与 ForwardNative 的实际账号准入实现。
//
// path 返回值即上述既有分支的出口标识，供上层选择器直接映射到既有转发器。

// 执行链标识（与 domain.PlatformGateway 的 anthropic/openai/gemini 对齐）。
const (
	CompositeChainOpenAI    = "openai"
	CompositeChainAnthropic = "anthropic"
	CompositeChainGemini    = "gemini"
)

// 端点标识（逻辑名，与 composite 路由端点和网关入口一一对应）。
const (
	CompositeEndpointChatCompletions = "chat_completions"
	CompositeEndpointResponses       = "responses"
	CompositeEndpointMessages        = "messages"
	CompositeEndpointCountTokens     = "count_tokens"
	CompositeEndpointImages          = "images"
	CompositeEndpointEmbeddings      = "embeddings"
	CompositeEndpointLive            = "live"
	CompositeEndpointAlphaSearch     = "alpha_search"
	CompositeEndpointSystemOne       = "systemone"
	CompositeEndpointResponsesWS     = "responses_ws"
	CompositeEndpointVideo           = "video"
	CompositeEndpointGemini          = "gemini" // /v1beta（Gemini 原生端点）
)

// 既有出站出口标识（与 handler 分支返回的字符串及 OpenAI 网关转发分支同名）。
const (
	CompositePathOpenAI       = "openai"        // OpenAIGatewayService 通用路径
	CompositePathGrok         = "grok"          // forwardGrokResponses / Grok 媒体
	CompositePathCodeBuddy    = "codebuddy"     // forwardCodeBuddy / forwardResponsesViaCodeBuddy
	CompositePathCursor       = "cursor"        // Cursor 原生协议执行部件（仅 Connect 会话账号）
	CompositePathDevin        = "devin"         // forwardDevin（OpenAIGatewayService 的 devin 分支）
	CompositePathWeb          = "web"           // forwardWebZhipu / forwardWebDeepseek / forwardWebKimi
	CompositePathGateway      = "gateway"       // GatewayService 通用路径
	CompositePathAntigravity  = "antigravity"   // antigravityGatewayService / shouldUseAntigravityCompat
	CompositePathGemini       = "gemini"        // Gemini 原生出口
	CompositePathGeminiCompat = "gemini-compat" // messagesAccountForwardPath 默认出口
	CompositePathTypeSafe     = "typesafe"      // TypeSafe /v1/systemone 出口
)

// compositeAccountFacts 是路径判据消费的事实结构：把账号（或其 claimant 摘要）
// 压成判定所需的五个字段，使 Account 版与 claimant 版共用同一份判定逻辑。
// 字段语义与下文两个包装的构造处一一对应。
type compositeAccountFacts struct {
	Platform        string
	Type            string // string(Account.Type)
	OpenAIGatewayOK bool   // Account.IsOpenAICompatible()
	CursorConnect   bool   // Account.IsCursorConnectSessionAccount()
	WebAccessMode   bool   // Account.IsWebAccessMode()
}

// CompositeAccountChainPath 返回账号在指定执行链/端点下的既有出站路径；
// ok=false 表示该账号不能由该链服务本端点。
//
// chain ∈ {openai, anthropic, gemini}，endpoint 见上方 CompositeEndpoint* 常量；
// 未知 chain/endpoint 与 nil 账号统一返回 ("", false)。
func CompositeAccountChainPath(chain, endpoint string, account *Account) (path string, ok bool) {
	if account == nil {
		return "", false
	}
	facts := compositeAccountFacts{
		Platform:        account.Platform,
		Type:            string(account.Type),
		OpenAIGatewayOK: account.IsOpenAICompatible(),
		CursorConnect:   account.IsCursorConnectSessionAccount(),
		WebAccessMode:   account.IsWebAccessMode(),
	}
	return compositeFactsChainPath(chain, endpoint, facts)
}

// CompositeClaimantChainPath 是 CompositeAccountChainPath 的 claimant 摘要版本：
// OpenAIGatewayOK 由 platform 推导（domain.UsesOpenAIGateway(platform) ||
// platform == PlatformOther），其余取摘要布尔值。语义与 Account 版逐位一致。
func CompositeClaimantChainPath(chain, endpoint string, claimant CompositeOwnershipClaimant) (path string, ok bool) {
	facts := compositeAccountFacts{
		Platform:        claimant.Platform,
		Type:            claimant.Type,
		OpenAIGatewayOK: domain.UsesOpenAIGateway(claimant.Platform) || claimant.Platform == PlatformOther,
		CursorConnect:   claimant.CursorConnect,
		WebAccessMode:   claimant.WebAccessMode,
	}
	return compositeFactsChainPath(chain, endpoint, facts)
}

// compositeFactsChainPath 是两条包装共用的核心：归一化 chain/endpoint 后分派到链。
func compositeFactsChainPath(chain, endpoint string, facts compositeAccountFacts) (path string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(chain)) {
	case CompositeChainOpenAI:
		return compositeOpenAIChainPath(strings.ToLower(strings.TrimSpace(endpoint)), facts)
	case CompositeChainAnthropic:
		return compositeAnthropicChainPath(strings.ToLower(strings.TrimSpace(endpoint)), facts)
	case CompositeChainGemini:
		return compositeGeminiChainPath(strings.ToLower(strings.TrimSpace(endpoint)), facts)
	default:
		return "", false
	}
}

// compositeOpenAIChainPath 镜像 OpenAIGatewayService.Forward 的平台分流与
// routes/gateway.go 的入口平台过滤：
//   - 文本端点（chat/responses/messages/count_tokens）以 IsOpenAICompatible 为基，
//     但 /v1/messages 的 web access mode 账号被两条调度器排除，不能服务该端点。
//   - images 由 Images/GrokImages 两条入口分派，仅 openai / grok（grok 走
//     handleGrokMedia，openai 走通用 Images）；embeddings/live/alpha 仅
//     PlatformOpenAI（embeddings 由 isOpenAIOnlyEndpointGatewayPlatform 闸控）。
//   - responses_ws 仅 openai / grok；systemone 不可；video 仅 grok。
func compositeOpenAIChainPath(endpoint string, facts compositeAccountFacts) (string, bool) {
	platform := facts.Platform
	switch endpoint {
	case CompositeEndpointSystemOne:
		return "", false
	case CompositeEndpointVideo:
		if platform == PlatformGrok {
			return CompositePathGrok, true
		}
		return "", false
	case CompositeEndpointResponsesWS:
		// WS 仅 openai / grok：grok 落到 forwardGrokResponses，openai 走通用 WS 管线。
		if platform == PlatformGrok {
			return CompositePathGrok, true
		}
		if platform == PlatformOpenAI {
			return CompositePathOpenAI, true
		}
		return "", false
	case CompositeEndpointImages:
		// images 由 Images/GrokImages 两条入口分派，仅 openai / grok：
		// grok 落到 handleGrokMedia，openai 走通用 Images。
		if platform == PlatformGrok {
			return CompositePathGrok, true
		}
		if platform == PlatformOpenAI {
			return CompositePathOpenAI, true
		}
		return "", false
	case CompositeEndpointEmbeddings, CompositeEndpointLive, CompositeEndpointAlphaSearch:
		if platform == PlatformOpenAI {
			return CompositePathOpenAI, true
		}
		return "", false
	case CompositeEndpointChatCompletions, CompositeEndpointResponses,
		CompositeEndpointMessages, CompositeEndpointCountTokens:
		if !facts.OpenAIGatewayOK {
			return "", false
		}
		// WebAccessMode 是独立于平台值的判定源；现有两条调度器排除
		// （openai_account_scheduler.go 的 isOpenAIMessagesDispatchContext 两处）
		// 令网页逆向账号无法进入 /v1/messages 选号。
		if endpoint == CompositeEndpointMessages && facts.WebAccessMode {
			return "", false
		}
		return compositeOpenAIForwardBranch(facts), true
	case CompositeRouteEndpointAny:
		// any 端点用于门禁与模型级调度：OpenAI 兼容账号即视为本链可服务，
		// 按 chat_completions 同口径签发默认转发分支，不附加 /v1/messages
		// 的 WebAccessMode 排除（网页账号仍可服务通用文本端点）。
		if !facts.OpenAIGatewayOK {
			return "", false
		}
		return compositeOpenAIForwardBranch(facts), true
	default:
		return "", false
	}
}

// compositeOpenAIForwardBranch 按 OpenAIGatewayService.Forward 中的分流顺序返回出口。
// 判据分别对应 source 中的 isCodeBuddyPlatformAccount、isCursorAccount、
// PlatformDevin、isWebZhipuAccount/isWebDeepseekAccount/isWebKimiAccount 分支；
// cursor APIKey 走通用 raw CC 管线，与 Connect 会话账号不同。
func compositeOpenAIForwardBranch(facts compositeAccountFacts) string {
	switch {
	case facts.Platform == PlatformGrok:
		return CompositePathGrok
	case facts.Platform == PlatformCodeBuddy:
		return CompositePathCodeBuddy
	case facts.CursorConnect:
		return CompositePathCursor
	case facts.Platform == PlatformDevin:
		return CompositePathDevin
	case facts.WebAccessMode:
		return CompositePathWeb
	default:
		// openai / grok / CN 供应商 / other / muse / 多协议 API Key / cursor APIKey
		// 均落到通用 OpenAI 兼容管线（forwardGrokResponses 的 grok 特化不影响资格）。
		return CompositePathOpenAI
	}
}

// compositeAnthropicChainPath 镜像 Anthropic 网关的账号级分流：
//   - messagesNonGeminiForwardPath（devin / antigravity / gateway）
//   - chatCompletionsAccountForwardPath（devin / gemini / antigravity / gateway）
//   - responsesAccountForwardPath（devin / antigravity / gateway；gemini 无分支）
//   - GatewayService.Forward{AsChatCompletions,AsResponses} 与 Forward 的
//     IsCursorConnectSessionAccount 前置分支
//   - TypeSafe 仅 /v1/systemone；专用 OpenAI 端点不在本链。
func compositeAnthropicChainPath(endpoint string, facts compositeAccountFacts) (string, bool) {
	platform := facts.Platform

	// typesafe 仅 systemone，systemone 也仅由 typesafe 服务。
	if endpoint == CompositeEndpointSystemOne {
		if platform == PlatformTypeSafe {
			return CompositePathTypeSafe, true
		}
		return "", false
	}
	if platform == PlatformTypeSafe {
		return "", false
	}
	// OpenAI 链专用端点不属于 Anthropic 链。
	switch endpoint {
	case CompositeEndpointImages, CompositeEndpointEmbeddings, CompositeEndpointLive,
		CompositeEndpointAlphaSearch, CompositeEndpointResponsesWS, CompositeEndpointVideo:
		return "", false
	}
	// responsesAccountForwardPath / chatCompletionsAccountForwardPath 均不受理
	// gemini 账号的 responses；messagesNonGeminiForwardPath 名为「非 Gemini」。
	if endpoint == CompositeEndpointResponses && platform == PlatformGemini {
		return "", false
	}
	if !isAnthropicChainPlatform(facts) {
		return "", false
	}

	switch endpoint {
	case CompositeEndpointChatCompletions:
		return compositeAnthropicChatPath(facts)
	case CompositeEndpointMessages:
		return compositeAnthropicMessagesPath(facts)
	case CompositeEndpointResponses:
		return compositeAnthropicResponsesPath(facts)
	case CompositeEndpointCountTokens:
		// countTokensHandler 的非 OpenAI 兼容平台落到 Gateway.CountTokens。
		if platform == PlatformAnthropic {
			return CompositePathGateway, true
		}
		return "", false
	case CompositeRouteEndpointAny:
		// any 端点按 chat_completions 同口径签发默认路径：chat 分支覆盖本链
		// 全部准入平台（anthropic/antigravity/devin/gemini/cursor Connect），
		// 平台准入已在上方 isAnthropicChainPlatform 完成。
		return compositeAnthropicChatPath(facts)
	default:
		return "", false
	}
}

// isAnthropicChainPlatform 界定 Anthropic 网关可承接的账号：
// GatewayService 原生平台（anthropic）、TypeSafe（systemone 已在调用方处理）、
// 以及 compat 分支持有的 antigravity / devin、cursor Connect 会话账号、
// chat 分支独有的 gemini 账号。cursor APIKey、openai 族与 web 均不在其中。
func isAnthropicChainPlatform(facts compositeAccountFacts) bool {
	switch facts.Platform {
	case PlatformAnthropic, PlatformAntigravity, PlatformDevin, PlatformGemini:
		return true
	}
	return facts.CursorConnect
}

// compositeAnthropicChatPath 逐条镜像 chatCompletionsAccountForwardPath 与
// GatewayService.ForwardAsChatCompletions 的 cursor Connect 前置分支。
func compositeAnthropicChatPath(facts compositeAccountFacts) (string, bool) {
	if facts.CursorConnect {
		return CompositePathCursor, true
	}
	switch {
	case facts.Platform == PlatformDevin:
		return CompositePathDevin, true
	case facts.Platform == PlatformGemini:
		return CompositePathGemini, true
	case facts.Platform == PlatformAntigravity && facts.Type == string(AccountTypeOAuth):
		return CompositePathAntigravity, true
	case facts.Platform == PlatformAnthropic, facts.Platform == PlatformAntigravity:
		return CompositePathGateway, true
	default:
		return "", false
	}
}

// compositeAnthropicMessagesPath 逐条镜像 messagesNonGeminiForwardPath
// （antigravity 判据为 PlatformAntigravity 且非 APIKey）与 Forward 的
// cursor Connect 前置分支；gemini 分组走 Gemini 链，不在本链 messages 内。
func compositeAnthropicMessagesPath(facts compositeAccountFacts) (string, bool) {
	if facts.CursorConnect {
		return CompositePathCursor, true
	}
	switch {
	case facts.Platform == PlatformDevin:
		return CompositePathDevin, true
	case facts.Platform == PlatformAntigravity && facts.Type != string(AccountTypeAPIKey):
		return CompositePathAntigravity, true
	case facts.Platform == PlatformAnthropic, facts.Platform == PlatformAntigravity:
		return CompositePathGateway, true
	default:
		return "", false
	}
}

// compositeAnthropicResponsesPath 逐条镜像 responsesAccountForwardPath
// （antigravity 仅 OAuth compat 走 antigravity，其余回落 gateway）与
// ForwardAsResponses 的 cursor Connect 前置分支。
func compositeAnthropicResponsesPath(facts compositeAccountFacts) (string, bool) {
	if facts.CursorConnect {
		return CompositePathCursor, true
	}
	switch {
	case facts.Platform == PlatformDevin:
		return CompositePathDevin, true
	case facts.Platform == PlatformAntigravity && facts.Type == string(AccountTypeOAuth):
		return CompositePathAntigravity, true
	case facts.Platform == PlatformAnthropic, facts.Platform == PlatformAntigravity:
		return CompositePathGateway, true
	default:
		return "", false
	}
}

// compositeGeminiChainPath 镜像 Gemini 链的分流：
//   - messagesAccountForwardPath（devin / antigravity / gemini-compat，
//     准入范围按 geminiGroupAdmitsAccount：gemini/cursor/devin + antigravity OAuth）
//   - chatCompletionsAccountForwardPath 的 devin / gemini / antigravity 分支；
//     cursor 在该函数落到 gateway，不属 Gemini 出口，故 chat 仅 openai。
//   - responses 无 gemini 分支；/v1beta（CompositeEndpointGemini）按
//     GeminiV1BetaModels 与 ForwardNative 实际准入：devin 排除，antigravity
//     OAuth 走 AG 出口，gemini 走原生出口，cursor 走 gemini compat 桥接；
//     其余专用端点不属于 Gemini 链。
func compositeGeminiChainPath(endpoint string, facts compositeAccountFacts) (string, bool) {
	if !isGeminiChainPlatform(facts) {
		return "", false
	}
	switch endpoint {
	case CompositeEndpointGemini:
		// 镜像 GeminiV1BetaModels（POST /v1beta/models/{model}:generateContent |
		// streamGenerateContent | countTokens）与 ForwardNative 的实际账号准入和
		// 分流。平台准入已由 isGeminiChainPlatform 完成，但 devin 在该准入中为
		// true，需在此按 isDevinExcludedFromGeminiNative 排除。
		switch {
		case facts.Platform == PlatformDevin:
			// devin（Cascade）只支持 Connect 的 chat/anthropic/responses 出口；
			// Gemini 原生 /v1beta 会拿其 session token 打 AI Studio 得 401，
			// handler 在选号阶段即持续排除，故失败关闭。
			return "", false
		case facts.Platform == PlatformAntigravity:
			// 非 APIKey 账号走 antigravityGatewayService.ForwardGemini
			//（准入已保证 antigravity 仅 OAuth 形态）。
			return CompositePathAntigravity, true
		case facts.Platform == PlatformGemini:
			// ForwardNative 的通用 AI Studio 原生出口。
			return CompositePathGemini, true
		case facts.Platform == PlatformCursor:
			// ForwardNative 内的 Cursor 桥接（Connect 会话账号经 forwardCursorNative，
			// OpenAI 兼容 apikey 反代经 forwardCursorOpenAICompatibleNative），
			// 统一归到既有 gemini compat 出口标识。
			return CompositePathGeminiCompat, true
		default:
			return "", false
		}
	case CompositeEndpointMessages:
		switch {
		case facts.Platform == PlatformDevin:
			return CompositePathDevin, true
		case facts.Platform == PlatformAntigravity:
			return CompositePathAntigravity, true
		case facts.Platform == PlatformGemini, facts.Platform == PlatformCursor:
			return CompositePathGeminiCompat, true
		default:
			return "", false
		}
	case CompositeEndpointChatCompletions:
		switch {
		case facts.Platform == PlatformDevin:
			return CompositePathDevin, true
		case facts.Platform == PlatformGemini:
			return CompositePathGemini, true
		case facts.Platform == PlatformAntigravity && facts.Type == string(AccountTypeOAuth):
			return CompositePathAntigravity, true
		default:
			return "", false
		}
	case CompositeRouteEndpointAny:
		// any 端点按 messages 同口径按平台分流：gemini/cursor 走默认出口
		// gemini-compat，antigravity 走 AG，devin 走 devin。平台准入已由
		// isGeminiChainPlatform 完成（antigravity 仅 OAuth）。
		switch {
		case facts.Platform == PlatformDevin:
			return CompositePathDevin, true
		case facts.Platform == PlatformAntigravity:
			return CompositePathAntigravity, true
		case facts.Platform == PlatformGemini, facts.Platform == PlatformCursor:
			return CompositePathGeminiCompat, true
		default:
			return "", false
		}
	default:
		// responses / count_tokens / images / embeddings / live / alpha_search /
		// systemone / responses_ws / video 均无 Gemini 出口。
		return "", false
	}
}

// isGeminiChainPlatform 复刻 geminiGroupAdmitsAccount 的账号准入：
// gemini 平台自身、Cursor（Connect 与 APIKey）、Devin 聚合平台，以及
// antigravity OAuth 兼容账号。
func isGeminiChainPlatform(facts compositeAccountFacts) bool {
	switch facts.Platform {
	case PlatformGemini, PlatformCursor, PlatformDevin:
		return true
	case PlatformAntigravity:
		return facts.Type == string(AccountTypeOAuth)
	}
	return false
}
