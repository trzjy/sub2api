package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// forwardAnthropicViaRawChatCompletions serves /v1/messages clients through
// an OpenAI-compatible upstream that only supports /v1/chat/completions.
//
// Conversion chain (direct, no Responses intermediary):
//
//	Request:  Anthropic Messages → Chat Completions (AnthropicToChatCompletionsRequest)
//	Response: CC chunk/response → Anthropic events/response (direct bridge)
//
// This is the /v1/messages counterpart of forwardResponsesViaRawChatCompletions
// (which serves /v1/responses clients). Unlike the Responses path, the direct
// bridge skips the Responses API intermediate representation entirely — every
// streaming token runs through a single state machine instead of two.
func (s *OpenAIGatewayService) forwardAnthropicViaRawChatCompletions(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	defaultMappedModel string,
) (*OpenAIForwardResult, error) {
	startTime := time.Now()

	// 1. Parse Anthropic request
	var anthropicReq apicompat.AnthropicRequest
	if err := json.Unmarshal(body, &anthropicReq); err != nil {
		writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return nil, fmt.Errorf("parse anthropic request: %w", err)
	}
	originalModel := anthropicReq.Model
	if strings.TrimSpace(originalModel) == "" {
		writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return nil, fmt.Errorf("missing model in request")
	}
	applyOpenAICompatModelNormalization(&anthropicReq)
	clientStream := anthropicReq.Stream

	// 2. Anthropic → Chat Completions (direct, no Responses intermediary)
	chatReq, err := apicompat.AnthropicToChatCompletionsRequest(&anthropicReq)
	if err != nil {
		writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, fmt.Errorf("convert anthropic to chat completions: %w", err)
	}

	billingModel := resolveOpenAIForwardModel(account, anthropicReq.Model, defaultMappedModel)
	upstreamModel := normalizeOpenAIModelForUpstream(account, billingModel)
	if err := validateGPT61SolCompatRequest(body, upstreamModel); err != nil {
		writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, err
	}
	if openai.IsGPT61SolModelSpelling(upstreamModel) && len(anthropicReq.Tools) > 0 {
		err := fmt.Errorf("gpt-6.1-sol requires Responses for tool calls; this account only supports Chat Completions")
		writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, err
	}
	chatReq.Model = upstreamModel
	chatReq.ReasoningEffort = openAICompatAnthropicReasoningEffort(&anthropicReq, upstreamModel, chatReq.ReasoningEffort)
	// 非流式→流式转换（方案 D2）：非官方池类上游的非流式请求改走健康流式通道，
	// 上游强制 stream=true + include_usage，规避非流式响应头挂死（CF524 遗留）。
	// converted 仅用于响应分支选路，reqStream 口径（handler 安装点）零改动。
	converted := !clientStream && s.shouldConvertNonstreamToStream(account)
	chatReq.Stream = clientStream || converted
	if clientStream || converted {
		chatReq.StreamOptions = &apicompat.ChatStreamOptions{IncludeUsage: true}
	}

	serviceTier := extractOpenAIServiceTierFromBody(body)

	// 读侧（DeepSeek thinking 契约）：客户端未回传 thinking 的工具轮，按 tool_call
	// id 回查网关侧缓存回注真实明文；未命中注入占位后照常放行（失败开放 + 占位兜底，
	// 见 injectCachedMessagesReasoning）。门控外路径零变化。
	if err := s.injectCachedMessagesReasoning(ctx, &anthropicReq, chatReq, account, upstreamModel); err != nil {
		return nil, err
	}

	chatBody, err := json.Marshal(chatReq)
	if err != nil {
		return nil, fmt.Errorf("marshal chat completions request: %w", err)
	}
	if normalizedBody, normalized := NormalizeGLMOpenAIReasoningEffort(chatBody, upstreamModel); normalized {
		chatBody = normalizedBody
	}
	if account.Platform == PlatformOpenAI {
		policyBody, changed, policyErr := ApplyOpenAIReasoningEffortPolicyFromContext(ctx, chatBody)
		if policyErr != nil {
			if IsReasoningEffortPolicyDenied(policyErr) {
				MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalPolicyDenied)
				writeAnthropicError(c, http.StatusForbidden, "forbidden_error", policyErr.Error())
			}
			return nil, policyErr
		}
		if changed {
			chatBody = policyBody
		}
	}
	// Provider normalization and policy caps can both change the converted effort.
	// Use the final outbound value for usage logs and billing.
	reasoningEffort := extractOpenAIReasoningEffortFromBody(chatBody, upstreamModel, billingModel, originalModel)
	reasoningEffort = ApplyThinkingEnabledFallback(reasoningEffort, chatBody, upstreamModel)
	// Unlike forwardResponsesViaRawChatCompletions, applyOpenAIFastPolicyToBody
	// is intentionally skipped: Anthropic Messages bodies carry no service_tier,
	// so the converted Chat Completions body never contains one and the policy
	// would always be a no-op on this path.

	logger.L().Debug("openai messages: forwarding via raw chat completions",
		zap.Int64("account_id", account.ID),
		zap.String("original_model", originalModel),
		zap.String("billing_model", billingModel),
		zap.String("upstream_model", upstreamModel),
		zap.Bool("stream", clientStream),
	)

	// 3. Build and send upstream request via the shared CC pipeline.
	//    CodeBuddy（影子 ∪ 直绑 platform=codebuddy）：出站改走 CodeBuddy 上游
	//    （母账号凭证 + §2.5 改写 + §2.4 指纹头；直绑账号凭证即自身，
	//    sendCodeBuddyChatUpstreamAsCC 内部按 IsShadow 决定是否回查母账号），
	//    其余账号走通用 CC 上游（账号自身 api_key/base_url）。
	//    判定口径与 ForwardAsAnthropic 的入口分流保持一致：直绑账号缺此判定会落到
	//    下方 resolveCCFallbackTarget，打到 ChatGPT/Codex 后端被 403 阻断页拒绝。
	var resp *http.Response
	var sendErr error
	if isCodeBuddyPlatformAccount(account) || isCodeBuddyShadowAccount(account) {
		resp, sendErr = s.sendCodeBuddyChatUpstreamAsCC(ctx, c, account, chatBody, upstreamModel)
	} else {
		apiKey, targetURL, terr := s.resolveCCFallbackTarget(account)
		if terr != nil {
			return nil, terr
		}
		resp, sendErr = s.sendCCUpstreamRequest(ctx, c, account, targetURL, chatBody, clientStream || converted, apiKey, account.GetOpenAIUserAgent(), "", upstreamModel)
	}
	if sendErr != nil {
		return nil, sendErr
	}
	defer func() { _ = resp.Body.Close() }()

	// 4. Handle error responses
	if resp.StatusCode >= 400 {
		respBody, upstreamMsg := s.readOpenAIUpstreamError(resp)
		if isCodeBuddyPlatformAccount(account) || isCodeBuddyShadowAccount(account) {
			// 与 /v1/chat/completions 路径一致的 CodeBuddy 错误副作用（余额/会话/模型冷却），
			// 内部完成分类 + 脱敏 + 回写 resp.Body；响应本身仍以 Anthropic 格式回传。
			s.applyCodeBuddyErrorSideEffectsFromBody(ctx, account, resp, respBody, upstreamModel)
			// T3 信号优先（R5-F1）：429 两类限流直接转 failover 信号换号，避免落入下方
			// 通用 helper 对同一账号二次处置（T2 收窄后 429+6004 会被升级为账号级冷却，
			// 违背「仅模型冷却」）；信号未认领的一切情形 nil 回退，既有 helper 原样保留。
			if foErr := s.codeBuddyFailoverSignal(c, account, ClassifyCodeBuddyError(resp.StatusCode, respBody), resp, respBody, upstreamMsg); foErr != nil {
				return nil, foErr
			}
		}
		if foErr := s.failoverOpenAIUpstreamHTTPError(ctx, c, account, resp, respBody, upstreamMsg, upstreamModel); foErr != nil {
			return nil, foErr
		}
		// Non-failover error: return Anthropic-formatted error to client via the
		// shared compat handler (passthrough rules, ops recording, cyber_policy).
		return s.handleAnthropicErrorResponse(resp, c, account, billingModel)
	}

	// 5. Convert response
	if clientStream {
		return s.streamChatCompletionsAsAnthropic(c, account, resp, originalModel, billingModel, upstreamModel, reasoningEffort, serviceTier, startTime)
	}
	if converted {
		return s.convertedChatCompletionsAsAnthropic(c, account, resp, originalModel, billingModel, upstreamModel, reasoningEffort, serviceTier, startTime)
	}
	return s.bufferChatCompletionsAsAnthropic(c, account, resp, originalModel, billingModel, upstreamModel, reasoningEffort, serviceTier, startTime)
}

// convertedChatCompletionsAsAnthropic 是非流式客户端经非流式→流式转换后的响应
// 尾段（方案 D4.1）：聚合强制流式上游的 CC SSE 为完整 chat.completion 响应后，
// 复用与 bufferChatCompletionsAsAnthropic 完全同构的既有 CC→Anthropic 转换尾段
// 写出，不新建第二条转换实现。
func (s *OpenAIGatewayService) convertedChatCompletionsAsAnthropic(
	c *gin.Context,
	account *Account,
	resp *http.Response,
	originalModel string,
	billingModel string,
	upstreamModel string,
	reasoningEffort *string,
	serviceTier *string,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	ccResp, usage, err := s.collectCCStreamAsResponse(resp, c, "openai messages chat fallback")
	if err != nil {
		// 聚合期零字节写客户端（响应头未提交）：CN 首包超时走既有透明 failover
		// 分支（冷却+换号，带模型，与缓冲路径同一分支，无新机制）；其余错误按
		// 原 endpoint 契约回写 Anthropic 规范错误（writeAnthropicError，与缓冲
		// 路径同一错误格式）。
		if isOpenAICNFirstByteTimeout(err) {
			return nil, s.failoverOpenAICNFirstByteTimeout(context.Background(), account, upstreamModel)
		}
		recordAggregatedStreamUpstreamError(c, account, resp, err)
		writeAnthropicError(c, http.StatusBadGateway, "api_error", err.Error())
		return nil, err
	}
	// 上游被强制流式后 Content-Type=text/event-stream，经 WriteFilteredHeaders
	// 透传会污染非流式响应，forceJSON=true 覆盖回 application/json。
	return s.writeChatCompletionsAsAnthropicResult(c, account, resp, ccResp, usage, originalModel, billingModel, upstreamModel, reasoningEffort, serviceTier, startTime, true)
}

func (s *OpenAIGatewayService) bufferChatCompletionsAsAnthropic(
	c *gin.Context,
	account *Account,
	resp *http.Response,
	originalModel string,
	billingModel string,
	upstreamModel string,
	reasoningEffort *string,
	serviceTier *string,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	ccResp, usage, err := s.readCCUpstreamJSONResponse(resp, c, writeAnthropicError)
	if err != nil {
		// CN 首包超时：缓冲路径响应头尚未提交，可安全透明切换（冷却+换号，带模型）。
		// 读取错误未写客户端响应（readCCUpstreamJSONResponse 对内部超时原样返回），
		// 此处优先映射内部超时为 failover；其余错误已按原 endpoint 契约
		// （writeAnthropicError 格式）回写，直接上抛即可。
		if isOpenAICNFirstByteTimeout(err) {
			return nil, s.failoverOpenAICNFirstByteTimeout(context.Background(), account, upstreamModel)
		}
		return nil, err
	}
	return s.writeChatCompletionsAsAnthropicResult(c, account, resp, ccResp, usage, originalModel, billingModel, upstreamModel, reasoningEffort, serviceTier, startTime, false)
}

// writeChatCompletionsAsAnthropicResult 是 buffer 与 converted 两个非流式分支共用的
// CC→Anthropic 转换尾段（方案 D4.1）：同一转换函数（ChatCompletionsResponseToAnthropic）
// + 同一写出路径（WriteFilteredHeaders + c.JSON + ForwardResult 组装，Stream:false），
// 不新建第二条转换实现。forceJSON 仅 converted 分支置 true（覆盖上游透传的 SSE
// Content-Type）；buffer 分支上游为原生 JSON，置 false 保持行为零变化。
func (s *OpenAIGatewayService) writeChatCompletionsAsAnthropicResult(
	c *gin.Context,
	account *Account,
	resp *http.Response,
	ccResp *apicompat.ChatCompletionsResponse,
	usage OpenAIUsage,
	originalModel string,
	billingModel string,
	upstreamModel string,
	reasoningEffort *string,
	serviceTier *string,
	startTime time.Time,
	forceJSON bool,
) (*OpenAIForwardResult, error) {
	requestID := resp.Header.Get("x-request-id")
	anthropicResp := apicompat.ChatCompletionsResponseToAnthropic(ccResp, originalModel)

	// 写侧：把本轮 reasoning 明文按 tool_call id 写入既有 GatewayCache，供后续
	// 轮次客户端不回传 thinking 时回注。仅记日志，不影响转发。
	if ccResp != nil && len(ccResp.Choices) > 0 {
		msg := ccResp.Choices[0].Message
		reasoning := msg.ReasoningContent
		if reasoning == "" {
			reasoning = msg.Reasoning
		}
		toolCallIDs := make([]string, 0, len(msg.ToolCalls))
		for _, call := range msg.ToolCalls {
			toolCallIDs = append(toolCallIDs, call.ID)
		}
		s.cacheMessagesToolCallReasoning(account, upstreamModel, reasoning, toolCallIDs)
	}

	if s.responseHeaderFilter != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	}
	if forceJSON {
		c.Writer.Header().Set("Content-Type", "application/json")
	}
	c.JSON(http.StatusOK, anthropicResp)

	return &OpenAIForwardResult{
		RequestID:                   requestID,
		UpstreamHeaders:             resp.Header,
		Usage:                       usage,
		Model:                       originalModel,
		BillingModel:                billingModel,
		UpstreamModel:               upstreamModel,
		ReasoningEffort:             reasoningEffort,
		UpstreamResponseServiceTier: observedUpstreamResponseServiceTier(c),
		ServiceTier:                 resolvedOpenAIUpstreamServiceTier(c, serviceTier),
		Stream:                      false,
		Duration:                    time.Since(startTime),
	}, nil
}

// ---------------------------------------------------------------------------
// /v1/messages 路径 reasoning_content 缓存回注（DeepSeek thinking 契约）
//
// 客户端（如 Claude Code）回放历史时，assistant 工具轮常常不带 thinking 明文，
// 而 DeepSeek thinking mode 要求这些轮次回传 reasoning_content。网关无法强迫
// 客户端回传、亦不得伪造，故与 Responses 兄弟路径对齐：网关侧把自己服务过的轮次
// 的 reasoning 明文按 tool_call id 缓存下来，下一轮按 id 回注真实明文。
//
// 失败开放（F2 事故复盘）：缓存只能覆盖网关自身服务过的轮次，客户端重放的历史轮
// 注定回注不到。F1 在未命中时失败关闭（400），把它放大成长会话 100% 死锁；F2 改
// 为未命中即原样放行，由上游自行裁决。
//
// F3 现状（占位兜底，2026-10-14 用户裁定）：命中缓存注入真实明文（行为不变）；
// 未命中（含缓存不可用）注入 messagesReasoningPlaceholder 并照常发往上游——不拦截、
// 不报错、不重试，客户端零感知。缘起是 F2 把死锁降成约 95% 成功但未归零：剩余 400
// 来自存量长会话的历史工具轮（早于 F2 上线产生，网关侧无明文可回注）叠加上游对缺失
// 字段的偶发严格校验（account 163 实测同期 122 成功 vs 6 失败）。
// ---------------------------------------------------------------------------

// messagesReasoningPlaceholder 是「缓存未命中」时填入出站 reasoning_content 的占位文本。
// 语义是诚实的缺失标记：该轮 reasoning 未被客户端回传、网关侧也没有缓存——**不是伪造
// 模型的思考内容**，不含任何推断或编造的思考文本。
//
// 为什么允许占位（两条实测依据，证据目录见派发单背景）：
//   - 上游只做非空校验：占位串 / 空格 / 句点全部返回 200，且原样收纳，不校验内容真伪；
//   - 完全不带该字段时上游偶发严格（account 163 上 122 成功 vs 6 失败），正是要消除的 400。
//
// 客户端零感知：占位只进入网关发往上游的请求体，不写入任何返回给客户端的内容。
const messagesReasoningPlaceholder = "(reasoning not retained)"

// messagesReasoningCacheTTL 与 Responses 路径（responsesReasoningCacheTTL）同口径：
// 会话可能跨多天恢复历史，取 7 天。
const messagesReasoningCacheTTL = 7 * 24 * time.Hour

// messagesToolCallReasoningKey 给 Messages 路径的 reasoning 缓存键加命名空间前缀。
// Responses 路径按 reasoning item id 索引，Messages 路径按 tool_call id 索引，两者
// 共用既有 GatewayCache 的 reasoning_content: 键空间，加前缀避免碰撞。
func messagesToolCallReasoningKey(toolCallID string) string {
	return "msg_tool:" + toolCallID
}

// messagesDeepSeekThinkingGate 是唯一的门控点：仅对 DeepSeek-thinking 上游启用
// reasoning 缓存/回注，其余路径零变化。判定 = 模型名（去 opencode 前缀后）以
// deepseek 开头，或账号 platform 为 deepseek。
func messagesDeepSeekThinkingGate(account *Account, upstreamModel string) bool {
	if account != nil && account.Platform == PlatformDeepseek {
		return true
	}
	model := strings.ToLower(strings.TrimSpace(upstreamModel))
	model = strings.TrimPrefix(model, "opencode/")
	return strings.HasPrefix(model, "deepseek")
}

// cacheMessagesToolCallReasoning 把本轮 assistant 的 reasoning 明文按 tool_call id
// 写入既有 GatewayCache。多工具轮：为该轮每个 tool_call id 写同一条 reasoning。
// 使用 detached ctx：客户端断连后仍在 drain 上游流，此处的 reasoning 是后续轮次
// 回注所依赖的，不能随请求 ctx 取消。失败仅记日志，不影响转发。
func (s *OpenAIGatewayService) cacheMessagesToolCallReasoning(account *Account, upstreamModel, reasoning string, toolCallIDs []string) {
	if s == nil || s.cache == nil {
		return
	}
	reasoning = strings.TrimSpace(reasoning)
	if reasoning == "" || len(toolCallIDs) == 0 {
		return
	}
	if !messagesDeepSeekThinkingGate(account, upstreamModel) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, toolCallID := range toolCallIDs {
		toolCallID = strings.TrimSpace(toolCallID)
		if toolCallID == "" {
			continue
		}
		if err := s.cache.SetReasoningContent(ctx, messagesToolCallReasoningKey(toolCallID), reasoning, messagesReasoningCacheTTL); err != nil {
			logger.L().Warn("openai messages chat fallback: cache reasoning content failed",
				zap.String("tool_call_id", toolCallID),
				zap.Error(err),
			)
		}
	}
}

// messagesToolTurnCollector 采集一轮 assistant 输出的 reasoning 明文与 tool_call id。
// 流式路径有两条互相独立的宣布通道：scan 期间经 emitChunk 的事件，以及 Finalize
// 阶段补宣布的 tool_use（上游 tool name 迟到/缺失时走这条，不经 emitChunk）。采集
// 器把两条通道合并，并允许在任意退出边界 flush，避免早退路径丢掉本轮已采集到的
// 内容。
type messagesToolTurnCollector struct {
	reasoning   strings.Builder
	toolCallIDs []string
}

// observe 从一批 Anthropic 事件里抽取 thinking 增量与 tool_use id。
func (col *messagesToolTurnCollector) observe(events []apicompat.AnthropicStreamEvent) {
	for _, evt := range events {
		if evt.Type == "content_block_delta" && evt.Delta != nil &&
			evt.Delta.Type == "thinking_delta" && evt.Delta.Thinking != "" {
			col.reasoning.WriteString(evt.Delta.Thinking)
			continue
		}
		if evt.Type == "content_block_start" && evt.ContentBlock != nil &&
			evt.ContentBlock.Type == "tool_use" && evt.ContentBlock.ID != "" {
			col.toolCallIDs = append(col.toolCallIDs, evt.ContentBlock.ID)
		}
	}
}

// flush 把已采集到的 reasoning 按已采集到的 tool_call id 写缓存，并清空 id 集合，
// 使同一轮内多次 flush（scan 结束一次 + Finalize 补采一次）不重复写同一个键。
func (col *messagesToolTurnCollector) flush(s *OpenAIGatewayService, account *Account, upstreamModel string) {
	if len(col.toolCallIDs) == 0 {
		return
	}
	s.cacheMessagesToolCallReasoning(account, upstreamModel, col.reasoning.String(), col.toolCallIDs)
	col.toolCallIDs = nil
}

// messagesTurnThinkingShape 描述一轮 assistant 工具轮里 thinking 块的形态，仅供
// 脱敏日志使用：hasThinkingText=带 thinking 明文；signatureOnly=只有 signature 无
// 明文。两者互斥；只落布尔，不落正文与签名。
type messagesTurnThinkingShape struct {
	hasThinkingText bool
	signatureOnly   bool
}

// messagesTurnThinkingShapes 按 tool_use id 索引原始 Anthropic 请求里各轮 thinking
// 块的形态。按 id 索引而非按下标：Anthropic→CC 转换会把 tool_result 拆成独立的
// tool 轮、并可能前置 system 轮，两侧下标并不对齐。
func messagesTurnThinkingShapes(req *apicompat.AnthropicRequest) map[string]messagesTurnThinkingShape {
	out := make(map[string]messagesTurnThinkingShape)
	if req == nil {
		return out
	}
	for _, m := range req.Messages {
		if m.Role != "assistant" || len(m.Content) == 0 {
			continue
		}
		var blocks []apicompat.AnthropicContentBlock
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			continue
		}
		var shape messagesTurnThinkingShape
		for _, b := range blocks {
			if b.Type != "thinking" {
				continue
			}
			if b.Thinking != "" {
				shape.hasThinkingText = true
			} else if b.Signature != "" {
				shape.signatureOnly = true
			}
		}
		for _, b := range blocks {
			if b.Type == "tool_use" && b.ID != "" {
				out[b.ID] = shape
			}
		}
	}
	return out
}

// injectCachedMessagesReasoning 在发送上游前，对「role==assistant 且 ToolCalls 非空
// 且 ReasoningContent 为空」的历史轮，按 tool_call id 逐个回查网关缓存：命中即注入
// 真实明文。
//
// 失败开放 + 占位兜底（F3）：未命中（含缓存不可用）是正常情况——缓存只能覆盖网关自身
// 服务过的轮次，客户端重放的历史轮次注定回注不到。此时注入 messagesReasoningPlaceholder
// 并照常发往上游：不写错误响应、不 return error、不重试、不给客户端任何提示（客户端
// 零感知）；仅打一条 debug 级脱敏日志。
//
// 回归线（F1 事故）：任何形式的未命中拦截/报错都是 blocker，这里只返回 nil。
func (s *OpenAIGatewayService) injectCachedMessagesReasoning(
	ctx context.Context,
	anthropicReq *apicompat.AnthropicRequest,
	chatReq *apicompat.ChatCompletionsRequest,
	account *Account,
	upstreamModel string,
) error {
	if chatReq == nil || !messagesDeepSeekThinkingGate(account, upstreamModel) {
		return nil
	}
	// 懒加载：仅真的出现未命中时才解析原始请求取 thinking 块形态。
	var shapes map[string]messagesTurnThinkingShape
	for i := range chatReq.Messages {
		msg := &chatReq.Messages[i]
		if msg.Role != "assistant" || len(msg.ToolCalls) == 0 || msg.ReasoningContent != "" {
			continue
		}
		cached := ""
		for _, call := range msg.ToolCalls {
			if strings.TrimSpace(call.ID) == "" {
				continue
			}
			content, err := s.getMessagesToolCallReasoning(ctx, call.ID)
			if err != nil {
				logger.L().Warn("openai messages chat fallback: lookup cached reasoning failed",
					zap.String("tool_call_id", call.ID),
					zap.Error(err),
				)
				continue
			}
			if content != "" {
				cached = content
				break
			}
		}
		if cached != "" {
			msg.ReasoningContent = cached
			continue
		}
		if shapes == nil {
			shapes = messagesTurnThinkingShapes(anthropicReq)
		}
		var shape messagesTurnThinkingShape
		for _, call := range msg.ToolCalls {
			if found, ok := shapes[call.ID]; ok {
				shape = found
				break
			}
		}
		logger.L().Debug("openai messages chat fallback: cached reasoning miss, injecting placeholder",
			zap.Int("tool_call_count", len(msg.ToolCalls)),
			zap.Bool("has_thinking_text", shape.hasThinkingText),
			zap.Bool("signature_only", shape.signatureOnly),
		)
		// 未命中（含缓存不可用）注入诚实的缺失标记占位，照常发往上游：不拦截、不报错。
		msg.ReasoningContent = messagesReasoningPlaceholder
	}
	return nil
}

// getMessagesToolCallReasoning 按 tool_call id 回查缓存的 reasoning 明文。缓存不可
// 用按未命中处理（失败开放）：不阻断转发。
func (s *OpenAIGatewayService) getMessagesToolCallReasoning(ctx context.Context, toolCallID string) (string, error) {
	if s == nil || s.cache == nil {
		return "", nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return s.cache.GetReasoningContent(lookupCtx, messagesToolCallReasoningKey(toolCallID))
}

func (s *OpenAIGatewayService) streamChatCompletionsAsAnthropic(
	c *gin.Context,
	account *Account,
	resp *http.Response,
	originalModel string,
	billingModel string,
	upstreamModel string,
	reasoningEffort *string,
	serviceTier *string,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	requestID := resp.Header.Get("x-request-id")
	writeStreamHeaders := s.newStreamHeaderWriter(c, resp.Header)

	anthropicState := apicompat.NewChatCompletionsToAnthropicStreamState(originalModel)
	clientDisconnected := false
	outputStarted := false

	// 写侧采集：本轮 reasoning 明文（thinking_delta 拼接）与本轮 tool_call id
	// （content_block_start 的 tool_use ID，即客户端下一轮会回传的 ID）。
	var turnCollector messagesToolTurnCollector

	// 与 responses 兄弟不同：客户端断开后仍继续做事件转换（喂 anthropicState），
	// 仅跳过写出，保证 finalize 阶段的 usage 汇总不受断开影响。
	emitChunk := func(chunk *apicompat.ChatCompletionsChunk) {
		// CC chunk → Anthropic events (direct, single state machine)
		anthropicEvents := apicompat.ChatCompletionsChunkToAnthropicEvents(chunk, anthropicState)
		// 采集先于断开判定：断连后仍在 drain 上游流，此处缓存是后续轮次回注的唯一来源。
		turnCollector.observe(anthropicEvents)
		if clientDisconnected {
			return
		}
		for _, aEvt := range anthropicEvents {
			sse, err := apicompat.ResponsesAnthropicEventToSSE(aEvt)
			if err != nil {
				continue
			}
			writeStreamHeaders()
			outputStarted = true
			if _, err := fmt.Fprint(c.Writer, sse); err != nil {
				clientDisconnected = true
				break
			}
		}
		if !clientDisconnected && len(anthropicEvents) > 0 {
			c.Writer.Flush()
		}
	}

	scan := s.scanCCStream(c, resp, "openai messages chat fallback", requestID, startTime, emitChunk)
	usage := scan.Usage

	// 写侧：先落一次再判早退。下方 scan.Err / CN 首包超时分支会直接 return，
	// 本轮已采集到的 reasoning 与 tool_call id 不能在早退时整轮丢弃。
	turnCollector.flush(s, account, upstreamModel)

	if scan.Err != nil {
		// CN 首包超时（watchdog 转译错误）：响应头未提交（首事件写出前）时透明
		// 切换（冷却+换号，带模型）；已写出则按既有流中断语义处理。
		if isOpenAICNFirstByteTimeout(scan.Err) && !outputStarted {
			return nil, s.failoverOpenAICNFirstByteTimeout(context.Background(), account, upstreamModel)
		}
		// Broken upstream read: skip finalization so no synthetic message_stop
		// masks the truncation, and surface the error to flag usage incomplete
		// (mirrors forwardResponsesViaRawChatCompletions).
		return &OpenAIForwardResult{
			RequestID:                   requestID,
			UpstreamHeaders:             resp.Header,
			Usage:                       usage,
			Model:                       originalModel,
			BillingModel:                billingModel,
			UpstreamModel:               upstreamModel,
			ReasoningEffort:             reasoningEffort,
			UpstreamResponseServiceTier: observedUpstreamResponseServiceTier(c),
			ServiceTier:                 resolvedOpenAIUpstreamServiceTier(c, serviceTier),
			Stream:                      true,
			Duration:                    time.Since(startTime),
			FirstTokenMs:                scan.FirstTokenMs,
			ClientDisconnect:            clientDisconnected,
		}, fmt.Errorf("stream usage incomplete: %w", scan.Err)
	}

	// Finalize: close open blocks + emit message_delta/message_stop.
	finalEvents := apicompat.FinalizeChatCompletionsAnthropicStream(anthropicState)
	// 写侧：Finalize 会补宣布「上游 tool name 迟到/缺失」的 tool_use，这些 id 不
	// 经过 emitChunk，必须在此补采后才能落缓存。
	turnCollector.observe(finalEvents)
	turnCollector.flush(s, account, upstreamModel)
	if !clientDisconnected {
		for _, aEvt := range finalEvents {
			sse, err := apicompat.ResponsesAnthropicEventToSSE(aEvt)
			if err != nil {
				continue
			}
			writeStreamHeaders()
			if _, err := fmt.Fprint(c.Writer, sse); err != nil {
				clientDisconnected = true
				break
			}
		}
		c.Writer.Flush()
	}
	if !scan.SawDone {
		logCCStreamMissingDoneSentinel("openai messages chat fallback", requestID)
	}

	return &OpenAIForwardResult{
		RequestID:                   requestID,
		UpstreamHeaders:             resp.Header,
		Usage:                       usage,
		Model:                       originalModel,
		BillingModel:                billingModel,
		UpstreamModel:               upstreamModel,
		ReasoningEffort:             reasoningEffort,
		UpstreamResponseServiceTier: observedUpstreamResponseServiceTier(c),
		ServiceTier:                 resolvedOpenAIUpstreamServiceTier(c, serviceTier),
		Stream:                      true,
		Duration:                    time.Since(startTime),
		FirstTokenMs:                scan.FirstTokenMs,
		ClientDisconnect:            clientDisconnected,
	}, nil
}
