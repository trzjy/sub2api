package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
)

// codeBuddyChatCompletionsPath 是 CodeBuddy 的 Chat Completions 端点（§2.1）。
const codeBuddyChatCompletionsPath = "/v2/chat/completions"

// codeBuddyBalanceCooldownMinutes CodeBuddy 余额耗尽临时停调时长（分钟），对齐 CN 供应商的 2× 检测周期默认。
const codeBuddyBalanceCooldownMinutes = 20

// codeBuddyModelLimitFailoverReason 标记模型级限流（429+6004）转出的 failover 信号
// （写入 UpstreamFailoverError.Reason，配合 Scope=Request 供排障与消费方识别）。
const codeBuddyModelLimitFailoverReason = GatewayFailureReason("codebuddy_model_limit")

// codeBuddyModelLimitFallbackCooldown 是 6004 且模型非空但 reset 时间不可解析时的
// 模型级冷却兜底窗口（60s，与生产 429 兜底冷却同量级；R6-F1。先例：T5 硬编码常量，
// 不加设置项）。
const codeBuddyModelLimitFallbackCooldown = 60 * time.Second

// isCodeBuddyShadowAccount 判定账号是否为按母账号分发的 CodeBuddy 影子：
// 影子标记（ParentAccountID != nil）+ quota_dimension=codebuddy。这是
// /v1/chat/completions、/v1/responses、/v1/messages 三条入站路径 codebuddy 影子
// 分发的**唯一判定（SSOT）**——新增平台入口时必须复用，漏改即会把影子当普通账号
// 误路由到错误上游（实证：/v1/chat/completions 曾因此打到 chatgpt.com）。
func isCodeBuddyShadowAccount(account *Account) bool {
	return account != nil && account.IsShadow() && account.QuotaDimension == QuotaDimensionCodeBuddy
}

// forwardCodeBuddy 是 CodeBuddy 平台（Chat Completions 变体）的转发入口，挂在
// OpenAIGatewayService.Forward 的 platform 分支上（与 forwardGrokResponses 同构）。
//
// 复用点：凭证获取（getRequestCredential）、上游发送（doOpenAIUpstream）、流式/非流式
// 响应处理（handleStreamingResponse / handleNonStreamingResponse，OpenAI 兼容 SSE 聚合）、
// 错误分类（ClassifyCodeBuddyError，纯函数）与限流副作用（RateLimitService）。
func (s *OpenAIGatewayService) forwardCodeBuddy(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	originalModel string,
	reqStream bool,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	if account.Type != AccountTypeOAuth && account.Type != AccountTypeAPIKey {
		return nil, fmt.Errorf("codebuddy account type %s is not supported", account.Type)
	}

	// 解析凭据母账号：CodeBuddy 影子（platform=目标分组平台）凭证透传母账号——
	// 母账号的 access_token / site / uid / enterprise_id / domain 才是出站的真正身份。
	// 原生 CodeBuddy 账号 credAccount == account。模型映射与审计事件仍用 account（影子）。
	credAccount := account
	if account.IsShadow() {
		parent, perr := resolveCredentialAccount(ctx, s.accountRepo, account)
		if perr != nil {
			return nil, perr
		}
		credAccount = parent
	}

	upstreamModel := account.GetMappedModel(originalModel)
	if strings.TrimSpace(upstreamModel) == "" {
		upstreamModel = originalModel
	}

	opts := CodeBuddyRewriteOptions{
		Sanitize:         s.codeBuddySanitizeEnabled(),
		Model:            upstreamModel,
		SupportedEfforts: codeBuddyResolveSupportedEfforts(ctx, credAccount, upstreamModel),
	}

	// §2.5 规则 1-7：出站前改写（强制 stream:true、tool_choice 归一、developer 角色、
	// DeepSeek thinking、reasoning_effort 降级、reasoning_content 回填、system 指纹脱敏）。
	rewritten, err := PrepareCodeBuddyBody(body, opts)
	if err != nil {
		setOpsUpstreamError(c, http.StatusBadRequest, err.Error(), "")
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"type": "invalid_request_error", "message": err.Error(),
		}})
		return nil, err
	}

	token, _, err := s.getRequestCredential(ctx, c, credAccount)
	if err != nil {
		return nil, err
	}

	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	defer releaseUpstreamCtx()

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	var resp *http.Response
	var respErr error
	// 内容审核拦截（400 + 审核关键词）走 sanitize 降级重试一次：用强制开启脱敏的 body 重发。
	for attempt := 0; attempt < 2; attempt++ {
		sendBody := rewritten
		if attempt > 0 {
			// 审核拦截重试：在强制脱敏之外，整体替换/去除 system 消息（§2.6 行 6 最后手段）。
			forced := opts
			forced.Sanitize = true
			forced.ReplaceSystem = true
			if sb, serr := PrepareCodeBuddyBody(body, forced); serr == nil {
				sendBody = sb
			}
		}
		upstreamReq, buildErr := s.buildCodeBuddyChatRequest(upstreamCtx, c, credAccount, sendBody, token, upstreamModel)
		if buildErr != nil {
			return nil, buildErr
		}
		resp, respErr = s.doOpenAIUpstreamNoWatchdog(upstreamReq, proxyURL, account)
		SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(startTime).Milliseconds())
		if respErr != nil {
			return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, respErr, false, "")
		}
		if resp.StatusCode < 400 {
			break
		}
		respBody := s.readUpstreamErrorBody(resp)
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
		if attempt == 0 && ClassifyCodeBuddyError(resp.StatusCode, respBody) == CodeBuddyErrKindContentAudit {
			// 进入重试：以强制脱敏 body 再发一次。
			continue
		}
		return s.handleCodeBuddyUpstreamError(ctx, c, account, resp, respBody, originalModel, upstreamModel)
	}
	// B1：成功触达上游后计入 CodeBuddy 账号 RPM（平台默认兜底）。TOCTOU 与
	// Anthropic 侧一致：读计数与递增之间存在窗口，soft-limit 可接受少量超额。
	s.incrementCodeBuddyRPM(ctx, account)
	defer func() { _ = resp.Body.Close() }()

	// 成功路径：复用 OpenAI 兼容 SSE 处理（非流式入站时上游被强制为流式，由
	// handleNonStreamingResponse 的 SSE→JSON 聚合收口为单条 chat.completion）。
	if reqStream {
		streamResult, err := s.handleStreamingResponse(ctx, resp, c, account, startTime, originalModel, upstreamModel)
		if err != nil {
			return nil, err
		}
		return codeBuddyForwardResultFromStreaming(streamResult, originalModel, upstreamModel, startTime, resp.Header), nil
	}
	nonStreamResult, err := s.handleCodeBuddyNonStreamingResponse(ctx, resp, c, account, originalModel, upstreamModel)
	if err != nil {
		return nil, err
	}
	return codeBuddyForwardResultFromNonStreaming(nonStreamResult, originalModel, upstreamModel, startTime, resp.Header), nil
}

// handleCodeBuddyNonStreamingResponse 收口非流式入站：上游被 §2.5 规则 1 强制为
// stream:true，因此先把 chat.completion.chunk SSE 聚合为单条 chat.completion JSON，
// 再交给通用 JSON 路径解析（用量提取、响应头过滤、计费口径与其它平台保持一致）。
//
// 通用路径的 handleSSEToJSON 只覆盖 Codex/Responses 形状，对 chat.completion.chunk
// 会原样回写 SSE（活体验收 F9），故此处显式聚合。
func (s *OpenAIGatewayService) handleCodeBuddyNonStreamingResponse(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	originalModel string,
	upstreamModel string,
) (*openaiNonStreamingResult, error) {
	body, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		return nil, err
	}
	if aggregated, ok := aggregateOpenAIChatCompletionsSSE(body); ok {
		resp.Body = io.NopCloser(bytes.NewReader(aggregated))
		// 上游原为 SSE：改写为 JSON 后必须同步内容类型与长度，避免下游收到
		// text/event-stream 或与实际字节数不符的 Content-Length。
		resp.Header.Set("Content-Type", "application/json")
		resp.Header.Del("Content-Length")
		resp.Header.Del("Transfer-Encoding")
	} else {
		resp.Body = io.NopCloser(bytes.NewReader(body))
	}
	return s.handleNonStreamingResponse(ctx, resp, c, account, originalModel, upstreamModel)
}

// buildCodeBuddyChatRequest 构造 CodeBuddy 上游请求并写入 §2.4 指纹头。
//
// 安全红线：chat 请求绝不携带 X-Refresh-Token（该头仅用于 refresh 端点）。
func (s *OpenAIGatewayService) buildCodeBuddyChatRequest(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	token string,
	upstreamModel string,
) (*http.Request, error) {
	ep := codeBuddyEndpointsFor(account.CodeBuddySite())
	targetURL := strings.TrimRight(ep.UpstreamBase, "/") + codeBuddyChatCompletionsPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))

	ua := s.codeBuddyChatUserAgent()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", ep.OriginReferer)
	req.Header.Set("Referer", ep.OriginReferer+"/")
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Product", "SaaS")

	// §2.4 chat 追加头：空值用 X-No-*: 1 占位（避免上游对空 header 的歧义处理）。
	// 这些身份头来自母账号（调用方已把 account 解析为母账号；影子自身凭证为空）。
	uid := account.GetCredential("uid")
	enterpriseID := account.GetCredential("enterprise_id")
	domain := account.GetCredential("domain")
	if uid != "" {
		req.Header.Set("X-User-Id", uid)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if enterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", enterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	if domain != "" {
		req.Header.Set("X-Domain", domain)
	} else {
		req.Header.Set("X-No-Domain", "1")
	}

	// 账号级请求头覆写最后应用，使管理员配置优先。
	account.ApplyHeaderOverrides(req.Header)
	return req, nil
}

// codeBuddyChatUserAgent 返回出站 User-Agent（配置优先，回落内置常量）。
func (s *OpenAIGatewayService) codeBuddyChatUserAgent() string {
	if s != nil && s.cfg != nil && strings.TrimSpace(s.cfg.Gateway.CodeBuddy.ChatUserAgent) != "" {
		return strings.TrimSpace(s.cfg.Gateway.CodeBuddy.ChatUserAgent)
	}
	return CodeBuddyClientUA
}

// codeBuddySanitizeEnabled 返回 system 指纹脱敏开关（默认开启）。
func (s *OpenAIGatewayService) codeBuddySanitizeEnabled() bool {
	if s != nil && s.cfg != nil {
		return s.cfg.Gateway.CodeBuddy.SanitizeEnabled
	}
	return true
}

// codeBuddyFailoverSignal 把 CodeBuddy 上游 429 的两类限流（ModelLimit=429+6004、
// AccountSoftLimit=裸 429/限流文案）转换为 handler failover 循环可识别的
// *UpstreamFailoverError 信号；chat、/responses、/v1/messages 三入口共用的唯一转换点。
// 其余 kind 或非 429 状态一律返回 nil，调用方落回各自入口的既有错误处理链（行为与
// 今天逐字节一致）。
//
// 红线：
//   - 只认 429 状态码（R3-F1）：分类器行 4 会把「5xx+限流文案」也归为
//     AccountSoftLimit，不加状态门禁会把 5xx 纳入新 failover，超出「仅 CodeBuddy 429」
//     的批准边界；非 429 的既有通用 failover 行为由调用方 nil 回退保留。
//   - 不复用 failoverOpenAIUpstreamHTTPError（R1-F1）：该 helper 返回信号前会再调
//     handleOpenAIAccountUpstreamError 对同一账号二次处置，ModelLimit 的「仅该模型
//     冷却」会被升级为整账号冷却。账号处置由 applyCodeBuddyErrorSideEffects 唯一
//     权威完成，信号只转换、不再处置。
//   - ModelLimit 显式清零 RetryableOnSameAccount/RequestScopedTransient（R5-F2）：
//     构造器在响应含 overloaded 文案时自动置位两者（requestScopedCapacity），会把
//     模型级限流降级为同账号先重试（failover 循环的 RetryableOnSameAccount 分支），
//     与换号语义冲突。
//
// Scope 复用既有枚举语义「上游按模型容量降载：不计账号健康」：ModelLimit 信号
// Scope=Request 使熔断分类器既有 Request 排除行生效——零分类器改动、零新枚举值
// （R2-F2）；AccountSoftLimit 保持空 Scope，可被熔断分类计数（T1 放行的意义）。
func (s *OpenAIGatewayService) codeBuddyFailoverSignal(
	c *gin.Context,
	account *Account,
	kind CodeBuddyErrKind,
	resp *http.Response,
	respBody []byte,
	upstreamMsg string,
) *UpstreamFailoverError {
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		return nil
	}
	var foErr *UpstreamFailoverError
	switch kind {
	case CodeBuddyErrKindModelLimit:
		foErr = newOpenAIUpstreamFailoverError(resp.StatusCode, resp.Header, respBody, upstreamMsg, false)
		foErr.Scope = GatewayFailureScopeRequest
		foErr.Reason = codeBuddyModelLimitFailoverReason
		foErr.RetryableOnSameAccount = false
		foErr.RequestScopedTransient = false
	case CodeBuddyErrKindAccountSoftLimit:
		foErr = s.newOpenAIAccountFailoverError(account, resp.StatusCode, resp.Header, respBody, upstreamMsg, false, false)
	default:
		return nil
	}
	upstreamDetail := ""
	if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		upstreamDetail = truncateString(string(respBody), maxBytes)
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID:            opsUpstreamProxyID(account),
		ProxyName:          opsUpstreamProxyName(account),
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: resp.StatusCode,
		UpstreamRequestID:  resp.Header.Get("x-request-id"),
		Kind:               "failover",
		Message:            upstreamMsg,
		Detail:             upstreamDetail,
	})
	return foErr
}

// handleCodeBuddyUpstreamError 对 CodeBuddy 上游错误分类并施加账号健康副作用。
// 429 两类限流（ModelLimit/AccountSoftLimit）转换为 failover 信号优先返回，由
// handler failover 循环换号；其余 kind 维持既有终态处置——把上游错误原样回传给
// 客户端（由 handleErrorResponse 写 gin 上下文）。
func (s *OpenAIGatewayService) handleCodeBuddyUpstreamError(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	resp *http.Response,
	respBody []byte,
	originalModel string,
	upstreamModel string,
) (*OpenAIForwardResult, error) {
	kind := ClassifyCodeBuddyError(resp.StatusCode, respBody)
	// A2 泄漏面修复：上游错误文案可能携带账号 uid/nickname（如
	// "Offline user session for user 123456"）。handleErrorResponse 会把上游 message
	// 透传给下游（确定性 400 回写 / 错误透传规则 / failover 兜底），因此在进入透传
	// 链路前统一脱敏，并重置 resp.Body 使下游构造与 ops 事件都只能读到脱敏版。
	respBody = redactCodeBuddyUpstreamErrorBody(respBody, account)
	if resp != nil {
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
	}
	upstreamMsg := sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(respBody))
	if upstreamMsg == "" {
		upstreamMsg = fmt.Sprintf("codebuddy upstream returned status %d", resp.StatusCode)
	}

	s.applyCodeBuddyErrorSideEffects(ctx, account, resp, respBody, upstreamModel, kind, upstreamMsg)

	// T3 信号优先：命中（仅两类 429）即返回信号换号，ops 的 failover 事件由信号
	// 自带；未命中落既有 handleErrorResponse 终态（自记一条）——原先进出口处的
	// 无条件 http_error 追加已删除（R1-F3），每次尝试恰好一条 ops 事件。
	if foErr := s.codeBuddyFailoverSignal(c, account, kind, resp, respBody, upstreamMsg); foErr != nil {
		return nil, foErr
	}
	return s.handleErrorResponse(ctx, resp, c, account, respBody, upstreamModel)
}

// applyCodeBuddyErrorSideEffects 依据已分类的 CodeBuddy 上游错误施加账号健康副作用
// （余额耗尽 / 会话失效 / 模型限流 / 账号软限）。仅副作用，不写响应——供
// /v1/chat/completions（OpenAI 格式回传）与 /v1/messages（Anthropic 格式回传）共用，
// 保证两条入站路径对 CodeBuddy 账号的处置口径一致。
func (s *OpenAIGatewayService) applyCodeBuddyErrorSideEffects(
	ctx context.Context,
	account *Account,
	resp *http.Response,
	respBody []byte,
	upstreamModel string,
	kind CodeBuddyErrKind,
	upstreamMsg string,
) {
	switch kind {
	case CodeBuddyErrKindBalanceExhausted:
		s.rateLimitService.handleCodeBuddyInsufficientBalance(ctx, account, upstreamMsg)
	case CodeBuddyErrKindSessionDead:
		s.rateLimitService.handleCodeBuddySessionDead(ctx, account, upstreamMsg)
	case CodeBuddyErrKindModelLimit:
		if until, ok := parseCodeBuddyResetTime(respBody); ok {
			s.rateLimitService.handleCodeBuddyModelLimit(ctx, account, upstreamModel, until)
		} else if strings.TrimSpace(upstreamModel) != "" {
			// R6-F1：模型已知但 reset 时间不可解析——复用模型级冷却函数，until 换为
			// 兜底窗口（60s）。不得退化 handle429：T2 收窄后该路径会对 CodeBuddy 影子
			// 产生真实账号级冷却，违背 PR3 task 1.5「模型级限流不得账号级冷却」。
			s.rateLimitService.handleCodeBuddyModelLimit(ctx, account, upstreamModel, time.Now().Add(codeBuddyModelLimitFallbackCooldown))
		} else {
			// 缺模型信息无法按模型排除：退化为账号级短冷却（既有已接受语义，R4-F1 钉住不改）。
			s.rateLimitService.handle429(ctx, account, resp.Header, respBody)
		}
	case CodeBuddyErrKindAccountSoftLimit:
		s.rateLimitService.handle429(ctx, account, resp.Header, respBody)
	case CodeBuddyErrKindContentAudit, CodeBuddyErrKindRequestBody:
		// 不罚账号：审核拦截已重试过；请求体问题仍由调度轮转处理。
	case CodeBuddyErrKindUpstreamFault:
		// 5xx/404：上游故障，常规重试（不标记账号状态）。
	}
}

// applyCodeBuddyErrorSideEffectsFromBody 便捷入口：对被截断的原始上游错误体自行完成
// 分类 + 账号标识脱敏 + 生成 upstreamMsg，并把脱敏后的 body 回写 resp.Body（供后续
// Anthropic 错误回传读取），最后施加账号副作用。
func (s *OpenAIGatewayService) applyCodeBuddyErrorSideEffectsFromBody(
	ctx context.Context,
	account *Account,
	resp *http.Response,
	respBody []byte,
	upstreamModel string,
) {
	kind := ClassifyCodeBuddyError(resp.StatusCode, respBody)
	redacted := redactCodeBuddyUpstreamErrorBody(respBody, account)
	if resp != nil {
		resp.Body = io.NopCloser(bytes.NewReader(redacted))
	}
	upstreamMsg := sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(redacted))
	if upstreamMsg == "" {
		upstreamMsg = fmt.Sprintf("codebuddy upstream returned status %d", resp.StatusCode)
	}
	s.applyCodeBuddyErrorSideEffects(ctx, account, resp, redacted, upstreamModel, kind, upstreamMsg)
}

// forwardResponsesViaCodeBuddy 是 /v1/responses 入站 × CodeBuddy 影子账号的交叉协议
// 桥接：Responses 请求 → Chat Completions 请求 → CodeBuddy 上游（/v2/chat/completions）
// → CC 响应回桥为 Responses 形状写回客户端。与 /v1/messages 路径
// （forwardAnthropicViaRawChatCompletions 的 isCodeBuddyShadowAccount 分支）对称。
//
// 背景（2026-09-14 实测）：CodeBuddy 影子（platform=deepseek 等目标分组平台 + OAuth
// + quota_dimension=codebuddy）在 /v1/responses 入站时，旧路径把 Responses 形状 body
// 直接透传给上游 /v2/chat/completions，被上游以 code=11133 "the request parameters
// were rejected by the model provider" 拒绝；同账号 /v1/chat/completions 与 /v1/messages
// 因先转成 CC/Anthropic 形状而全部成功。此处补上遗漏的 Responses→CC 请求转换与
// CC→Responses 响应回桥。
func (s *OpenAIGatewayService) forwardResponsesViaCodeBuddy(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	originalModel string,
	clientStream bool,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	var responsesReq apicompat.ResponsesRequest
	if err := json.Unmarshal(body, &responsesReq); err != nil {
		setOpsUpstreamError(c, http.StatusBadRequest, "Failed to parse request body", "")
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"type": "invalid_request_error", "message": "Failed to parse request body",
		}})
		return nil, fmt.Errorf("parse responses request: %w", err)
	}
	if strings.TrimSpace(responsesReq.Model) == "" {
		setOpsUpstreamError(c, http.StatusBadRequest, "model is required", "")
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"type": "invalid_request_error", "message": "model is required",
		}})
		return nil, fmt.Errorf("missing model in request")
	}

	// 工具自适应：custom/function/tool_search/namespace 工具在 CC 形状下按名字转发，
	// 回程再按名字还原为对应 Responses item 类型（与 forwardResponsesViaRawChatCompletions 一致）。
	effectiveTools, err := apicompat.EffectiveResponsesTools(&responsesReq)
	if err != nil {
		setOpsUpstreamError(c, http.StatusBadRequest, err.Error(), "")
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"type": "invalid_request_error", "message": err.Error(),
		}})
		return nil, fmt.Errorf("resolve responses tools: %w", err)
	}
	customTools := apicompat.CustomToolNames(effectiveTools)
	functionTools := apicompat.FunctionToolNames(effectiveTools)
	toolSearch := apicompat.HasToolSearchTool(effectiveTools)
	namespaceTools := apicompat.NamespaceToolNames(effectiveTools)

	chatReq, err := apicompat.ResponsesToChatCompletionsRequestWithOptions(&responsesReq, &apicompat.ResponsesToChatOptions{
		ReasoningContentByID: s.reasoningContentByID,
	})
	if err != nil {
		setOpsUpstreamError(c, http.StatusBadRequest, err.Error(), "")
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"type": "invalid_request_error", "message": err.Error(),
		}})
		return nil, fmt.Errorf("convert responses to chat completions: %w", err)
	}

	billingModel := resolveOpenAIForwardModel(account, originalModel, "")
	upstreamModel := normalizeOpenAIModelForUpstream(account, billingModel)
	chatReq.Model = upstreamModel
	if clientStream {
		chatReq.StreamOptions = &apicompat.ChatStreamOptions{IncludeUsage: true}
	}
	chatBody, err := json.Marshal(chatReq)
	if err != nil {
		return nil, fmt.Errorf("marshal chat completions request: %w", err)
	}
	SetOpsUpstreamModel(c, upstreamModel)

	reasoningEffort := extractOpenAIReasoningEffortFromBody(body, upstreamModel, billingModel, originalModel)
	serviceTier := extractOpenAIServiceTierFromBody(body)

	resp, err := s.sendCodeBuddyChatUpstreamAsCC(ctx, c, account, chatBody, upstreamModel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody, upstreamMsg := s.readOpenAIUpstreamError(resp)
		kind := ClassifyCodeBuddyError(resp.StatusCode, respBody)
		s.applyCodeBuddyErrorSideEffectsFromBody(ctx, account, resp, respBody, upstreamModel)
		// T3 信号优先（R4-F2）：429 两类限流直接转 failover 信号换号；信号未认领的
		// 一切情形（含「5xx+限流文案」被 429 门禁挡下、其余 kind）回退既有通用
		// helper——本入口非 429 行为与今天逐字节一致（R2-F3）。
		if foErr := s.codeBuddyFailoverSignal(c, account, kind, resp, respBody, upstreamMsg); foErr != nil {
			return nil, foErr
		}
		if foErr := s.failoverOpenAIUpstreamHTTPError(ctx, c, account, resp, respBody, upstreamMsg, upstreamModel); foErr != nil {
			return nil, foErr
		}
		return s.handleErrorResponse(ctx, resp, c, account, chatBody, billingModel)
	}

	if clientStream {
		return s.streamChatCompletionsAsResponses(c, account, resp, originalModel, customTools, functionTools, toolSearch, namespaceTools, billingModel, upstreamModel, reasoningEffort, serviceTier, startTime)
	}
	// CodeBuddy 上游强制 stream:true（§2.5 规则 1），非流式入站时上游返回 SSE 流。
	// bufferChatCompletionsAsResponses 内部的 readCCUpstreamJSONResponse 假定纯 JSON，
	// 此处先聚合 SSE→JSON 再走缓冲路径（与 handleCodeBuddyNonStreamingResponse 一致）。
	respBody, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		return nil, err
	}
	if aggregated, ok := aggregateOpenAIChatCompletionsSSE(respBody); ok {
		resp.Body = io.NopCloser(bytes.NewReader(aggregated))
	} else {
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
	}
	return s.bufferChatCompletionsAsResponses(c, account, resp, originalModel, customTools, functionTools, toolSearch, namespaceTools, billingModel, upstreamModel, reasoningEffort, serviceTier, startTime)
}

// sendCodeBuddyChatUpstreamAsCC 把（已是 Chat Completions 形状的）body 经 CodeBuddy
// 出站管线发送一次，返回原始 *http.Response，供 /v1/messages（Anthropic 入站）路径复用
// CodeBuddy 出站语义：解析母账号凭证/site → §2.5 出站改写（含内容审核降级重试一次）→
// §2.4 指纹头（buildCodeBuddyChatRequest）→ doOpenAIUpstream。不写响应、不做错误分类
// 副作用（调用方负责 CC→Anthropic 回桥与错误处置）。
func (s *OpenAIGatewayService) sendCodeBuddyChatUpstreamAsCC(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	upstreamModel string,
) (*http.Response, error) {
	if account.Type != AccountTypeOAuth && account.Type != AccountTypeAPIKey {
		return nil, fmt.Errorf("codebuddy account type %s is not supported", account.Type)
	}
	credAccount := account
	if account.IsShadow() {
		parent, perr := resolveCredentialAccount(ctx, s.accountRepo, account)
		if perr != nil {
			return nil, perr
		}
		credAccount = parent
	}
	opts := CodeBuddyRewriteOptions{
		Sanitize:         s.codeBuddySanitizeEnabled(),
		Model:            upstreamModel,
		SupportedEfforts: codeBuddyResolveSupportedEfforts(ctx, credAccount, upstreamModel),
	}
	rewritten, err := PrepareCodeBuddyBody(body, opts)
	if err != nil {
		return nil, err
	}
	token, _, err := s.getRequestCredential(ctx, c, credAccount)
	if err != nil {
		return nil, err
	}
	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	defer releaseUpstreamCtx()
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	var resp *http.Response
	for attempt := 0; attempt < 2; attempt++ {
		sendBody := rewritten
		if attempt > 0 {
			forced := opts
			forced.Sanitize = true
			forced.ReplaceSystem = true
			if sb, serr := PrepareCodeBuddyBody(body, forced); serr == nil {
				sendBody = sb
			}
		}
		upstreamReq, buildErr := s.buildCodeBuddyChatRequest(upstreamCtx, c, credAccount, sendBody, token, upstreamModel)
		if buildErr != nil {
			return nil, buildErr
		}
		resp, err = s.doOpenAIUpstreamNoWatchdog(upstreamReq, proxyURL, account)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 400 {
			return resp, nil
		}
		respBody := s.readUpstreamErrorBody(resp)
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
		if attempt == 0 && ClassifyCodeBuddyError(resp.StatusCode, respBody) == CodeBuddyErrKindContentAudit {
			continue
		}
		return resp, nil
	}
	return resp, nil
}

// codeBuddyUpstreamUserIDPattern 兜底匹配错误文案中 "user 123456" / "user id: 123456"
// 形态的账号标识片段（存储凭证之外的未知值场景）。仅匹配 4 位以上数字，避免误伤
// 业务错误码等短数字。
var codeBuddyUpstreamUserIDPattern = regexp.MustCompile(`(?i)(\buser(?:[_ ]?id)?\b[\s:=#]*)(\d{4,})`)

// redactCodeBuddyUpstreamErrorBody 对 CodeBuddy 上游错误响应体做账号标识脱敏：
//  1. 精确替换该账号已知的 uid / nickname / enterprise_id（长度 >=4 才替换，过短的
//     值误伤面大，如 "1" 会替换掉所有 "1"）；
//  2. 兜底正则替换 "user <digits>" 形态的 uid 片段。
//
// 只动错误文本内容，不动 JSON 结构与状态码，下游拿到的错误类型语义不变。
func redactCodeBuddyUpstreamErrorBody(body []byte, account *Account) []byte {
	if len(body) == 0 {
		return body
	}
	text := string(body)
	if account != nil {
		for _, key := range []string{"uid", "nickname", "enterprise_id"} {
			if v := strings.TrimSpace(account.GetCredential(key)); len(v) >= 4 {
				text = strings.ReplaceAll(text, v, "***")
			}
		}
	}
	text = codeBuddyUpstreamUserIDPattern.ReplaceAllString(text, "${1}****")
	return []byte(text)
}

func codeBuddyForwardResultFromStreaming(r *openaiStreamingResult, originalModel, upstreamModel string, startTime time.Time, respHeader http.Header) *OpenAIForwardResult {
	if r == nil {
		return &OpenAIForwardResult{Model: originalModel, UpstreamModel: upstreamModel, Stream: true, Duration: time.Since(startTime)}
	}
	usage := r.usage
	if usage == nil {
		usage = &OpenAIUsage{}
	}
	return &OpenAIForwardResult{
		UpstreamHeaders:  respHeader,
		ResponseID:       strings.TrimSpace(r.responseID),
		Usage:            *usage,
		Model:            originalModel,
		UpstreamModel:    upstreamModel,
		Stream:           true,
		OpenAIWSMode:     false,
		ResponseHeaders:  respHeader.Clone(),
		Duration:         time.Since(startTime),
		FirstTokenMs:     r.firstTokenMs,
		SearchCount:      r.searchCount,
		ImageCount:       r.imageCount,
		ImageOutputSizes: r.imageOutputSizes,
	}
}

func codeBuddyForwardResultFromNonStreaming(r *openaiNonStreamingResult, originalModel, upstreamModel string, startTime time.Time, respHeader http.Header) *OpenAIForwardResult {
	if r == nil {
		return &OpenAIForwardResult{Model: originalModel, UpstreamModel: upstreamModel, Stream: false, Duration: time.Since(startTime)}
	}
	usage := r.OpenAIUsage
	if usage == nil {
		usage = &OpenAIUsage{}
	}
	return &OpenAIForwardResult{
		UpstreamHeaders:  respHeader,
		ResponseID:       strings.TrimSpace(r.responseID),
		Usage:            *usage,
		Model:            originalModel,
		UpstreamModel:    upstreamModel,
		Stream:           false,
		OpenAIWSMode:     false,
		ResponseHeaders:  respHeader.Clone(),
		Duration:         time.Since(startTime),
		SearchCount:      r.searchCount,
		ImageCount:       r.imageCount,
		ImageOutputSizes: r.imageOutputSizes,
	}
}
