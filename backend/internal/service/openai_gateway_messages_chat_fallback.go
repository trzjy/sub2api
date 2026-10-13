package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
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

	convertedEffort := chatReq.ReasoningEffort
	reasoningEffort := &convertedEffort
	reasoningEffort = ApplyThinkingEnabledFallback(reasoningEffort, body, billingModel)
	serviceTier := extractOpenAIServiceTierFromBody(body)

	// F1 读侧（DeepSeek thinking 契约）：客户端未回传 thinking 的工具轮，按 tool_call
	// id 回查网关侧缓存回注真实明文；全部未命中则失败关闭（不发出上游请求、不伪造、
	// 不空格占位）。门控外路径零变化。
	if err := s.injectCachedMessagesReasoning(ctx, c, chatReq, account, upstreamModel); err != nil {
		return nil, err
	}

	chatBody, err := json.Marshal(chatReq)
	if err != nil {
		return nil, fmt.Errorf("marshal chat completions request: %w", err)
	}
	// P1 临时取证插桩（非生产基线，定位后移除）：仅 DeepSeek 模型打印脱敏结构。
	if strings.Contains(strings.ToLower(upstreamModel), "deepseek") {
		probeReqID := c.GetString("request_id")
		if probeReqID == "" {
			probeReqID = c.GetHeader("x-request-id")
		}
		logMessagesReasoningProbe(probeReqID, &anthropicReq, chatReq)
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
			if effectiveEffort := strings.TrimSpace(gjson.GetBytes(chatBody, "reasoning_effort").String()); effectiveEffort != "" {
				reasoningEffort = &effectiveEffort
			}
		}
	}
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
	//    CodeBuddy 影子：出站改走 CodeBuddy 上游（母账号凭证 + §2.5 改写 + §2.4 指纹头），
	//    其余账号走通用 CC 上游（账号自身 api_key/base_url）。
	var resp *http.Response
	var sendErr error
	if isCodeBuddyShadowAccount(account) {
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
		if isCodeBuddyShadowAccount(account) {
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

	// F1 写侧：把本轮 reasoning 明文按 tool_call id 写入既有 GatewayCache，供后续
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
// F1：/v1/messages 路径 reasoning_content 缓存回注（DeepSeek thinking 400 根治）
//
// 根因（P1 插桩实证）：客户端回放历史时工具轮不带 thinking 明文，网关无明文可折叠
// → 出站 reasoning_content 为空 → DeepSeek thinking mode 400。网关无法强迫客户端
// 回传、亦不得伪造，故与 Responses 兄弟路径对齐：网关侧缓存 reasoning，下一轮按
// tool_call id 回注真实明文；回注不了就失败关闭。
// ---------------------------------------------------------------------------

// messagesReasoningCacheTTL 与 Responses 路径（responsesReasoningCacheTTL）同口径：
// 会话可能跨多天恢复历史，取 7 天。
const messagesReasoningCacheTTL = 7 * 24 * time.Hour

// messagesToolCallReasoningKey 给 Messages 路径的 reasoning 缓存键加命名空间前缀。
// Responses 路径按 reasoning item id 索引，Messages 路径按 tool_call id 索引，两者
// 共用既有 GatewayCache 的 reasoning_content: 键空间，加前缀避免碰撞。
func messagesToolCallReasoningKey(toolCallID string) string {
	return "msg_tool:" + toolCallID
}

// messagesDeepSeekThinkingGate 是 F1 的唯一门控点：仅对 DeepSeek-thinking 上游启用
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

// injectCachedMessagesReasoning 在发送上游前，对「role==assistant 且 ToolCalls 非空
// 且 ReasoningContent 为空」的历史轮，按 tool_call id 逐个回查网关缓存：命中即注入
// 真实明文；全部未命中（或缓存不可用）则失败关闭——写出明确错误并返回 error，不发
// 出上游请求，不伪造、不空格占位。
func (s *OpenAIGatewayService) injectCachedMessagesReasoning(
	ctx context.Context,
	c *gin.Context,
	chatReq *apicompat.ChatCompletionsRequest,
	account *Account,
	upstreamModel string,
) error {
	if chatReq == nil || !messagesDeepSeekThinkingGate(account, upstreamModel) {
		return nil
	}
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
		if cached == "" {
			ids := make([]string, 0, len(msg.ToolCalls))
			for _, call := range msg.ToolCalls {
				ids = append(ids, call.ID)
			}
			writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error",
				"DeepSeek thinking mode requires reasoning_content on assistant tool-call turns; "+
					"the client replayed this turn without its thinking block and the gateway has no "+
					"cached reasoning for tool_calls ["+strings.Join(ids, ",")+"]")
			return fmt.Errorf("openai messages: missing reasoning_content for assistant tool-call turn (tool_calls [%s], uncached)", strings.Join(ids, ","))
		}
		msg.ReasoningContent = cached
	}
	return nil
}

// getMessagesToolCallReasoning 按 tool_call id 回查缓存的 reasoning 明文。
func (s *OpenAIGatewayService) getMessagesToolCallReasoning(ctx context.Context, toolCallID string) (string, error) {
	if s == nil || s.cache == nil {
		return "", errors.New("gateway cache unavailable")
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	content, err := s.cache.GetReasoningContent(lookupCtx, messagesToolCallReasoningKey(toolCallID))
	if err != nil {
		return "", err
	}
	return content, nil
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

	// F1 写侧采集：本轮 reasoning 明文（thinking_delta 拼接）与本轮 tool_call id
	// （content_block_start 的 tool_use ID，即客户端下一轮会回传的 ID）。
	var reasoningBuf strings.Builder
	var toolCallIDs []string

	// 与 responses 兄弟不同：客户端断开后仍继续做事件转换（喂 anthropicState），
	// 仅跳过写出，保证 finalize 阶段的 usage 汇总不受断开影响。
	emitChunk := func(chunk *apicompat.ChatCompletionsChunk) {
		// CC chunk → Anthropic events (direct, single state machine)
		anthropicEvents := apicompat.ChatCompletionsChunkToAnthropicEvents(chunk, anthropicState)
		// 采集先于断开判定：断连后仍在 drain 上游流，此处缓存是后续轮次回注的唯一来源。
		for _, aEvt := range anthropicEvents {
			if aEvt.Type == "content_block_delta" && aEvt.Delta != nil &&
				aEvt.Delta.Type == "thinking_delta" && aEvt.Delta.Thinking != "" {
				reasoningBuf.WriteString(aEvt.Delta.Thinking)
				continue
			}
			if aEvt.Type == "content_block_start" && aEvt.ContentBlock != nil &&
				aEvt.ContentBlock.Type == "tool_use" && aEvt.ContentBlock.ID != "" {
				toolCallIDs = append(toolCallIDs, aEvt.ContentBlock.ID)
			}
		}
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
	// F1 写侧：本轮结束才写缓存（reasoning 与 tool_call id 此时才完整）。
	s.cacheMessagesToolCallReasoning(account, upstreamModel, reasoningBuf.String(), toolCallIDs)

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

// messagesReasoningProbeMaxMsgs 限制单侧打印的 assistant 轮次数，避免长会话打爆
// 日志；取证只需判形状，超限时以 *_truncated 标记。
const messagesReasoningProbeMaxMsgs = 32

// logMessagesReasoningProbe 是 P1 临时取证插桩（非生产基线，定位后移除）：
// DeepSeek thinking-mode 400 根因取证——并排打印「客户端回传的入站 assistant 块
// 结构」与「网关转换后的出站 reasoning_content / tool_calls 结构」，据以判定丢失
// 发生在客户端未回传 / 网关转换丢失 / 上游契约三者中哪一处。
//
// 只输出脱敏结构：块类型计数、长度数字、tool id 列表。严禁输出 thinking /
// reasoning 正文、content 正文、任何密钥或完整请求体。
func logMessagesReasoningProbe(reqID string, areq *apicompat.AnthropicRequest, creq *apicompat.ChatCompletionsRequest) {
	if areq == nil || creq == nil {
		return
	}
	fields := make([]zap.Field, 0, 16)
	fields = append(fields,
		zap.String("request_id", reqID),
		zap.String("upstream_model", creq.Model),
		zap.Bool("request_has_tools", len(areq.Tools) > 0),
	)

	// 入站：每个 assistant 轮的 content block 结构（不含任何正文）。
	loggedIn := 0
	for i, m := range areq.Messages {
		if !strings.EqualFold(strings.TrimSpace(m.Role), "assistant") {
			continue
		}
		if loggedIn >= messagesReasoningProbeMaxMsgs {
			fields = append(fields, zap.Bool("inbound_truncated", true))
			break
		}
		loggedIn++
		prefix := "in." + strconv.Itoa(i) + "."

		var blocks []apicompat.AnthropicContentBlock
		if len(m.Content) == 0 || json.Unmarshal(m.Content, &blocks) != nil || len(blocks) == 0 {
			// content 为纯字符串或空：无 thinking/tool_use 结构可统计。
			fields = append(fields, zap.String(prefix+"content_kind", "non_block"))
			continue
		}

		var thinking, thinkingWithText, redacted, sigOnly, toolUse int
		var toolIDs []string
		for _, b := range blocks {
			switch b.Type {
			case "thinking":
				thinking++
				if b.Thinking != "" {
					thinkingWithText++
				} else if b.Signature != "" {
					// 只有 signature 占位、无明文：上游不会被回传明文，但客户端
					// 回放契约要求它存在。
					sigOnly++
				}
			case "redacted_thinking":
				redacted++
			case "tool_use":
				toolUse++
				if b.ID != "" {
					toolIDs = append(toolIDs, b.ID)
				}
			}
		}
		fields = append(fields,
			zap.String(prefix+"content_kind", "blocks"),
			zap.Int(prefix+"blocks", len(blocks)),
			zap.Int(prefix+"thinking", thinking),
			zap.Int(prefix+"thinking_with_text", thinkingWithText),
			zap.Int(prefix+"redacted_thinking", redacted),
			zap.Int(prefix+"signature_only", sigOnly),
			zap.Int(prefix+"tool_use", toolUse),
			zap.Strings(prefix+"tool_use_ids", toolIDs),
		)
	}

	// 出站：每个 assistant 轮的推理字段长度（只打长度，不打内容）。
	loggedOut := 0
	for i, m := range creq.Messages {
		if !strings.EqualFold(strings.TrimSpace(m.Role), "assistant") {
			continue
		}
		if loggedOut >= messagesReasoningProbeMaxMsgs {
			fields = append(fields, zap.Bool("outbound_truncated", true))
			break
		}
		loggedOut++
		prefix := "out." + strconv.Itoa(i) + "."

		toolCallIDs := make([]string, 0, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			if tc.ID != "" {
				toolCallIDs = append(toolCallIDs, tc.ID)
			}
		}
		fields = append(fields,
			zap.Int(prefix+"reasoning_content_len", len(m.ReasoningContent)),
			zap.Int(prefix+"reasoning_len", len(m.Reasoning)),
			zap.Int(prefix+"tool_calls", len(m.ToolCalls)),
			zap.Strings(prefix+"tool_call_ids", toolCallIDs),
			zap.Int(prefix+"content_len", len(m.Content)),
		)
	}

	logger.L().Warn("MESSAGES_REASONING_PROBE", fields...)
}
