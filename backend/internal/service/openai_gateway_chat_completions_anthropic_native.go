package service

// 国产供应商 Anthropic 协议账号的 CC 入站反向路径。
//
// 客户端说 OpenAI Chat Completions、上游是供应商原生 Anthropic 端点
// （api_protocol=anthropic）时的交叉组合：请求 CC→Responses→Anthropic 转换，
// 响应 Anthropic→Responses→CC 转换。转换链与 Anthropic 平台的
// gateway_forward_as_chat_completions.go 完全一致（复用同一组 apicompat
// 状态机），仅上游发送/错误处理对齐 OpenAI 网关语义。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// forwardChatCompletionsViaNativeAnthropic serves OpenAI /v1/chat/completions
// clients through a CN provider's native Anthropic endpoint.
//
// Conversion chain:
//
//	Request:  Chat Completions → Responses → Anthropic (chained)
//	Response: Anthropic events → Responses events → CC chunks (chained state machines)
func (s *OpenAIGatewayService) forwardChatCompletionsViaNativeAnthropic(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	defaultMappedModel string,
) (*OpenAIForwardResult, error) {
	startTime := time.Now()

	// 1. Parse Chat Completions request
	var ccReq apicompat.ChatCompletionsRequest
	if err := json.Unmarshal(body, &ccReq); err != nil {
		writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", infraerrors.RequestBodyParseFailed)
		return nil, fmt.Errorf("parse chat completions request: %w", err)
	}
	originalModel := ccReq.Model
	if strings.TrimSpace(originalModel) == "" {
		writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", infraerrors.ModelRequired)
		return nil, fmt.Errorf("missing model in request")
	}
	clientStream := ccReq.Stream
	includeUsage := ccReq.StreamOptions != nil && ccReq.StreamOptions.IncludeUsage

	// 2. Convert CC → Responses → Anthropic (chained conversion)
	responsesReq, err := chatCompletionsToResponses(s.cfg, &ccReq)
	if err != nil {
		writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", infraerrors.RequestConvertFailed)
		return nil, fmt.Errorf("convert chat completions to responses: %w", err)
	}
	// Resolve the mapped model before choosing its thinking/tool protocol.
	billingModel := resolveOpenAIForwardModel(account, originalModel, defaultMappedModel)
	upstreamModel := normalizeOpenAIModelForUpstream(account, billingModel)
	// thinking 契约翻译必须在校验之前（见 normalizeClaude55Thinking）。
	body = normalizeClaude55Thinking(body, upstreamModel)
	if err := validateClaude55Request(body, upstreamModel); err != nil {
		writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, err
	}
	responsesReq.Model = upstreamModel
	anthropicReq, err := apicompat.ResponsesToAnthropicRequest(responsesReq)
	if err != nil {
		writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, fmt.Errorf("convert responses to anthropic: %w", err)
	}

	// 4. Force upstream streaming（客户端原始终决定响应格式；
	// 上游恒为流式，非流式由缓冲路径组装）。
	anthropicReq.Stream = true
	reqStream := true

	logger.L().Debug("openai chat_completions: forwarding via native anthropic endpoint",
		zap.Int64("account_id", account.ID),
		zap.String("original_model", originalModel),
		zap.String("billing_model", billingModel),
		zap.String("upstream_model", upstreamModel),
		zap.Bool("client_stream", clientStream),
	)

	anthropicBody, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, fmt.Errorf("marshal anthropic request: %w", err)
	}

	// 与 /v1/messages 直通路径相同的 pre-filter。
	anthropicBody = StripEmptyTextBlocks(anthropicBody)
	anthropicBody = FilterWebSearchHistoryBlocks(anthropicBody, upstreamModel)
	anthropicBody = enforceCacheControlLimit(anthropicBody)

	apiKey := strings.TrimSpace(account.GetOpenAIProtocolAPIKey())
	if apiKey == "" {
		return nil, fmt.Errorf("account %d missing api_key", account.ID)
	}
	targetURL, err := s.nativeAnthropicTargetURL(account)
	if err != nil {
		return nil, err
	}

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	upstreamCtx, releaseUpstreamCtx := detachStreamUpstreamContext(ctx, reqStream)
	upstreamReq, forwardedBody, err := s.buildNativeAnthropicUpstreamRequest(upstreamCtx, c, account, anthropicBody, apiKey, targetURL, body)
	releaseUpstreamCtx()
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}

	resp, err := s.doOpenAIUpstreamWithGuard(c, ctx, upstreamReq, proxyURL, account)
	if err != nil {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, true, upstreamModel)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody, upstreamMsg := s.readOpenAIUpstreamError(resp)
		if foErr := s.failoverOpenAIUpstreamHTTPError(ctx, c, account, resp, respBody, upstreamMsg, upstreamModel, originalModel); foErr != nil {
			return nil, foErr
		}
		writeChatCompletionsError(c, mapUpstreamStatusCode(resp.StatusCode), "server_error", upstreamMsg)
		return nil, fmt.Errorf("upstream error: %d %s", resp.StatusCode, upstreamMsg)
	}

	// Bill the Anthropic effort actually sent upstream (for example xhigh → max).
	reasoningEffort := NormalizeClaudeOutputEffort(gjson.GetBytes(forwardedBody, "output_config.effort").String())
	reasoningEffort = ApplyThinkingEnabledFallback(reasoningEffort, forwardedBody, upstreamModel)

	if clientStream {
		return s.handleCCStreamingFromNativeAnthropic(ctx, account, resp, c, originalModel, billingModel, upstreamModel, reasoningEffort, startTime, includeUsage)
	}
	return s.handleCCBufferedFromNativeAnthropic(account, resp, c, originalModel, billingModel, upstreamModel, reasoningEffort, startTime)
}

// handleCCBufferedFromNativeAnthropic reads Anthropic SSE events, assembles the
// full response, then converts Anthropic → Responses → Chat Completions.
func (s *OpenAIGatewayService) handleCCBufferedFromNativeAnthropic(
	account *Account,
	resp *http.Response,
	c *gin.Context,
	originalModel string,
	billingModel string,
	upstreamModel string,
	reasoningEffort *string,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	requestID := resp.Header.Get("x-request-id")

	scanner := bufio.NewScanner(resp.Body)
	maxLineSize := defaultMaxLineSize
	if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.cfg.Gateway.MaxLineSize
	}
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)

	var finalResp *apicompat.AnthropicResponse
	var usage ClaudeUsage

	// 读间隔上限：上游挂住 SSE 时中止组装（缓冲路径尚未提交响应头，可回 502）。
	streamInterval := anthropicNativeStreamInterval(s.cfg)
	pump := newAnthropicNativeLinePump(scanner, streamInterval)
	defer pump.stop()

	logReadErr := func(err error) {
		if !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			logger.L().Warn("openai cc via native anthropic buffered: read error",
				zap.Error(err),
				zap.String("request_id", requestID),
			)
		}
	}
	onIdle := func() (*OpenAIForwardResult, error) {
		_ = resp.Body.Close()
		logger.L().Warn("openai cc via native anthropic buffered: data interval timeout",
			zap.String("request_id", requestID),
			zap.Duration("interval", streamInterval),
		)
		writeChatCompletionsError(c, http.StatusBadGateway, "server_error", infraerrors.UpstreamStreamIntervalTimeout)
		return nil, fmt.Errorf("stream data interval timeout")
	}

	for {
		line, rerr := pump.next()
		if rerr != nil {
			if errors.Is(rerr, errAnthropicNativeStreamIdle) {
				return onIdle()
			}
			logReadErr(rerr)
			// CN 首包超时：buffered 路径响应头尚未提交，可安全透明切换（冷却+换号）。
			if isOpenAICNFirstByteTimeout(rerr) {
				return nil, s.failoverOpenAICNFirstByteTimeout(context.Background(), account, upstreamModel)
			}
			break
		}
		// SSE 规范允许 `event:xxx`（冒号后无空格）：Kimi 等 Anthropic 兼容上游
		// 返回紧凑格式，严格匹配 "event: " 会丢弃全部事件（#4653 同根因）。
		if _, ok := extractOpenAISSEEventLine(line); !ok {
			continue
		}

		dataLine, rerr := pump.next()
		if rerr != nil {
			if errors.Is(rerr, errAnthropicNativeStreamIdle) {
				return onIdle()
			}
			logReadErr(rerr)
			// CN 首包超时：buffered 路径响应头尚未提交，可安全透明切换（冷却+换号）。
			if isOpenAICNFirstByteTimeout(rerr) {
				return nil, s.failoverOpenAICNFirstByteTimeout(context.Background(), account, upstreamModel)
			}
			break
		}
		payload, ok := extractOpenAISSEDataLine(dataLine)
		if !ok {
			continue
		}

		var event apicompat.AnthropicStreamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}

		if event.Type == "message_start" && event.Message != nil {
			finalResp = event.Message
			mergeAnthropicUsage(&usage, event.Message.Usage)
		}
		if event.Type == "message_delta" {
			if event.Usage != nil {
				mergeAnthropicUsage(&usage, *event.Usage)
			}
			if event.Delta != nil && event.Delta.StopReason != "" && finalResp != nil {
				finalResp.StopReason = apicompat.AnthropicStopReasonPtr(event.Delta.StopReason)
			}
		}
		if event.Type == "content_block_start" && event.ContentBlock != nil && finalResp != nil {
			finalResp.Content = append(finalResp.Content, *event.ContentBlock)
		}
		if event.Type == "content_block_delta" && event.Delta != nil && finalResp != nil && event.Index != nil {
			idx := *event.Index
			if idx >= 0 && idx < len(finalResp.Content) {
				switch event.Delta.Type {
				case "text_delta":
					finalResp.Content[idx].Text += event.Delta.Text
				case "thinking_delta":
					finalResp.Content[idx].Thinking += event.Delta.Thinking
				case "input_json_delta":
					finalResp.Content[idx].Input = appendRawJSON(finalResp.Content[idx].Input, event.Delta.PartialJSON)
				}
			}
		}
	}

	if finalResp == nil {
		writeChatCompletionsError(c, http.StatusBadGateway, "server_error", "Upstream stream ended without a response")
		return nil, fmt.Errorf("upstream stream ended without response")
	}

	if usage.InputTokens > 0 || usage.OutputTokens > 0 {
		finalResp.Usage = apicompat.AnthropicUsage{
			InputTokens:              usage.InputTokens,
			OutputTokens:             usage.OutputTokens,
			CacheCreationInputTokens: usage.CacheCreationInputTokens,
			CacheReadInputTokens:     usage.CacheReadInputTokens,
		}
	}

	responsesResp := apicompat.AnthropicToResponsesResponse(finalResp)
	ccResp := apicompat.ResponsesToChatCompletions(responsesResp, originalModel)

	if s.responseHeaderFilter != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	}
	// 非流式响应必须是 application/json（上游被强制流式，透传头会污染）。
	c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	if respBytes, err := json.Marshal(ccResp); err == nil {
		respBytes = reverseToolNamesIfPresent(c, respBytes)
		// 伪装 L3 思考剥离：上游 Anthropic 的 thinking 块经 apicompat 转换后
		// 落在 CC 的 reasoning_content 字段 —— 复用既有剥离实现，不另写一份。
		// 模型名无需在此处理：ResponsesToChatCompletions 已用 originalModel 覆盖。
		if _, _, stripReasoning := AccountIdentityDisguiseEffective(account); stripReasoning {
			respBytes = StripAnthropicThinkingFromCCBody(account, respBytes)
		}
		c.Data(http.StatusOK, "application/json; charset=utf-8", respBytes)
	} else {
		c.JSON(http.StatusOK, ccResp)
	}

	return &OpenAIForwardResult{
		RequestID:        requestID,
		UpstreamHeaders:  resp.Header,
		Usage:            claudeUsageToOpenAIUsage(&usage),
		Model:            originalModel,
		BillingModel:     billingModel,
		UpstreamModel:    upstreamModel,
		UpstreamEndpoint: "/v1/messages",
		ReasoningEffort:  reasoningEffort,
		Stream:           false,
		Duration:         time.Since(startTime),
	}, nil
}

// handleCCStreamingFromNativeAnthropic reads Anthropic SSE events, converts each
// to Responses events, then to Chat Completions chunks, and writes them.
func (s *OpenAIGatewayService) handleCCStreamingFromNativeAnthropic(
	ctx context.Context,
	account *Account,
	resp *http.Response,
	c *gin.Context,
	originalModel string,
	billingModel string,
	upstreamModel string,
	reasoningEffort *string,
	startTime time.Time,
	includeUsage bool,
) (*OpenAIForwardResult, error) {
	requestID := resp.Header.Get("x-request-id")

	// CN 首包超时整改（闸②第 2 轮 block 1）：首字节前不提交响应头，延迟到
	// 首个 SSE 事件写出时才提交（惰性 writer），使 CN 路径在首字节前仍处
	// 可切换窗口——读到内部超时错误时返回 failover 错误供上层换号，而不是
	// 提交 200 后让客户端拿到成功空流。响应头已提交后才到的超时按既有流
	// 中断语义处理（发错误事件，不 finalize 冒充成功）。
	clientOutputStarted := false
	upstreamStopReason := ""
	writeStreamHeaders := func() {
		if clientOutputStarted {
			return
		}
		clientOutputStarted = true
		if s.responseHeaderFilter != nil {
			responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
		}
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.Header().Set("X-Accel-Buffering", "no")
		c.Writer.WriteHeader(http.StatusOK)
	}

	anthState := apicompat.NewAnthropicEventToResponsesState()
	anthState.Model = originalModel
	ccState := apicompat.NewResponsesEventToChatState()
	ccState.Model = originalModel

	// 伪装 L3 思考剥离（流式）：开关在循环外取一次（每帧都会走到）。
	_, _, stripReasoning := AccountIdentityDisguiseEffective(account)
	// sawThinkingBlock：是否已见过 thinking 块的 content_block_start ——
	// 见过则其后配对的 content_block_stop 也要一并丢弃。
	sawThinkingBlock := false
	// 出口 usage 帧不按客户端 stream_options.include_usage 播种（与 Anthropic 平台
	// CC 出口 ForwardAsChatCompletions 同口径）：本路径转换链与该出口完全一致，
	// 末帧 usage 只由「上游是否给出权威 usage」决定。客户端显式请求而上游从未给
	// usage 时不补零值 usage 帧，上游给了 usage 而客户端未请求时照常透出（网关
	// 是计费链路一环，避免级联代理计费为 0）。includeUsage 形参保留仅为调用点
	// 形制一致，见下方循环内权威判定。

	var usage ClaudeUsage
	var firstTokenMs *int
	firstChunk := true
	clientDisconnected := false

	scanner := bufio.NewScanner(resp.Body)
	maxLineSize := defaultMaxLineSize
	if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.cfg.Gateway.MaxLineSize
	}
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)

	resultWithUsage := func() *OpenAIForwardResult {
		return &OpenAIForwardResult{
			RequestID:        requestID,
			UpstreamHeaders:  resp.Header,
			Usage:            claudeUsageToOpenAIUsage(&usage),
			Model:            originalModel,
			BillingModel:     billingModel,
			UpstreamModel:    upstreamModel,
			UpstreamEndpoint: "/v1/messages",
			ReasoningEffort:  reasoningEffort,
			Stream:           true,
			Duration:         time.Since(startTime),
			FirstTokenMs:     firstTokenMs,
			ClientDisconnect: clientDisconnected,
		}
	}

	// 读间隔上限：上游挂住 SSE（不发数据也不断连）时结束排水。上游 ctx 为
	// WithoutCancel 且 http.Client 无整体 Timeout，无此界限则客户端断开后
	// scanner.Scan() 永久阻塞（见 anthropic native pump 文件注释）。
	streamInterval := anthropicNativeStreamInterval(s.cfg)
	pump := newAnthropicNativeLinePump(scanner, streamInterval)
	defer pump.stop()

	logReadErr := func(err error) {
		if !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			logger.L().Warn("openai cc via native anthropic stream: read error",
				zap.Error(err),
				zap.String("request_id", requestID),
			)
		}
	}
	// onIdle 关闭上游连接（解除阻塞的读、归还连接池位），并按已累计 usage
	// 返回——与 messages 主路径 "stream usage incomplete after timeout" 同语义。
	onIdle := func() (*OpenAIForwardResult, error) {
		_ = resp.Body.Close()
		if !clientDisconnected {
			logger.L().Warn("openai cc via native anthropic stream: data interval timeout",
				zap.String("request_id", requestID),
				zap.Duration("interval", streamInterval),
			)
		}
		return resultWithUsage(), fmt.Errorf("stream data interval timeout")
	}

	writeChunkRaw := func(out string) bool {
		if clientDisconnected {
			return false
		}
		// 首个 SSE 事件写出前才提交响应头（CN 首包超时整改：首字节前不提交，
		// 保持可切换窗口）。
		writeStreamHeaders()
		if _, err := fmt.Fprint(c.Writer, out); err != nil {
			clientDisconnected = true
			return false
		}
		return false
	}

	// 空回答闸门（流式）：上游可能回 200、流也正常收尾，却没有任何正文（只有
	// 思考）。此时若已把 role 帧写出去，响应头就提交了，客户只能收到一个空白
	// 回答。这里把「还没有可渲染增量」的 chunk 先扣在手里不写出，保持可换号
	// 窗口；一旦出现正文/工具调用/拒答就放行缓冲转直通。
	// 缓冲超上限即放弃换号窗口直接放行，避免长思考流把内存撑住。
	const maxPendingCCChunks = 64
	var pendingCC []string
	payloadContentSeen := false
	emptyGateGivenUp := false
	flushPendingCC := func() {
		for _, out := range pendingCC {
			writeChunkRaw(out)
		}
		pendingCC = nil
	}
	writeChunk := func(chunk apicompat.ChatCompletionsChunk) bool {
		if clientDisconnected {
			return false // 已断开：不再写客户端，只排水上游累计 usage
		}
		sse, err := apicompat.ChatChunkToSSE(chunk)
		if err != nil {
			return false
		}
		out := string(reverseToolNamesIfPresent(c, []byte(sse)))
		if payloadContentSeen || emptyGateGivenUp {
			return writeChunkRaw(out)
		}
		if chatChunkCarriesPayloadContent(chunk) {
			payloadContentSeen = true
			flushPendingCC()
			return writeChunkRaw(out)
		}
		if chatChunkCarriesReasoning(chunk) {
			// 上游已经在吐思考内容：客户端能看到进展了，不能继续扣着（扣着会把
			// 「逐字出现的思考」变成「长时间空白后一次性刷出」）。放行缓冲并放弃
			// 换号窗口 —— 此时响应头提交，换号已不可能。
			emptyGateGivenUp = true
			flushPendingCC()
			return writeChunkRaw(out)
		}
		pendingCC = append(pendingCC, out)
		if len(pendingCC) >= maxPendingCCChunks {
			emptyGateGivenUp = true
			flushPendingCC()
		}
		return false
	}

	processAnthropicEvent := func(event *apicompat.AnthropicStreamEvent) bool {
		if firstChunk {
			firstChunk = false
			ms := int(time.Since(startTime).Milliseconds())
			firstTokenMs = &ms
		}

		// usage 恒累计（含客户端断开后的排水阶段，payg 上游照常计费）。
		if event.Type == "message_delta" && event.Usage != nil {
			mergeAnthropicUsage(&usage, *event.Usage)
		}
		if event.Type == "message_delta" && event.Delta != nil && event.Delta.StopReason != "" {
			upstreamStopReason = event.Delta.StopReason
		}
		if event.Type == "message_start" && event.Message != nil {
			mergeAnthropicUsage(&usage, event.Message.Usage)
		}

		// Keep the outward Responses/Chat usage on the same normalized buckets used
		// for billing, including converter handlers that consume event usage.
		syncAnthropicResponsesUsage(anthState, usage)
		normalizeAnthropicEventUsageForResponses(event, usage)

		// 客户端已断开：跳过转换与写出，继续读上游直到流结束（usage 完整、
		// 连接及时归还），不再提前 return。
		if clientDisconnected {
			return false
		}

		// 伪装 L3 思考剥离：在**事件进入转换之前**丢掉 thinking 相关事件 ——
		// 上游 Anthropic 的 thinking 块经 AnthropicEventToResponsesEvents 会变成
		// reasoning item，再转成 CC 的 reasoning_content，客户端就能看到思考内容。
		// 在此层剥离，两条下游转换链（→Responses / →CC）都自动干净，无需各写一份。
		if stripReasoning && anthropicStreamEventIsThinking(event) {
			if anthropicStreamEventIsThinkingStart(event) {
				sawThinkingBlock = true
			}
			return false
		}
		if stripReasoning && sawThinkingBlock && event.Type == "content_block_stop" {
			sawThinkingBlock = false
			return false
		}

		responsesEvents := apicompat.AnthropicEventToResponsesEvents(event, anthState)
		for _, resEvt := range responsesEvents {
			ccChunks := apicompat.ResponsesEventToChatChunks(&resEvt, ccState)
			for _, chunk := range ccChunks {
				writeChunk(chunk)
			}
		}
		if len(responsesEvents) > 0 {
			c.Writer.Flush()
		}
		return false
	}

	for {
		line, rerr := pump.next()
		if rerr != nil {
			if errors.Is(rerr, errAnthropicNativeStreamIdle) {
				return onIdle()
			}
			logReadErr(rerr)
			// CN 首包超时（闸②整改）：首字节前返回既有 failover 错误（切换窗口
			// 仍在，冷却链携带请求 ctx）；响应头已提交后返回流中断语义错误，不做
			// 透明切换。
			if isOpenAICNFirstByteTimeout(rerr) {
				if !clientOutputStarted {
					return nil, s.failoverOpenAICNFirstByteTimeout(ctx, account, upstreamModel)
				}
				return s.abortCCNativeStreamAfterHeaders(c, resultWithUsage, rerr)
			}
			break
		}
		if _, ok := extractOpenAISSEEventLine(line); !ok {
			continue
		}

		dataLine, rerr := pump.next()
		if rerr != nil {
			if errors.Is(rerr, errAnthropicNativeStreamIdle) {
				return onIdle()
			}
			// EOF / 读错误：事件行后流终止，进入 finalize。
			logReadErr(rerr)
			// CN 首包超时（闸②整改）：同上。
			if isOpenAICNFirstByteTimeout(rerr) {
				if !clientOutputStarted {
					return nil, s.failoverOpenAICNFirstByteTimeout(ctx, account, upstreamModel)
				}
				return s.abortCCNativeStreamAfterHeaders(c, resultWithUsage, rerr)
			}
			break
		}
		payload, ok := extractOpenAISSEDataLine(dataLine)
		if !ok {
			continue
		}

		var event apicompat.AnthropicStreamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}

		// Forward received usage regardless of the client stream_options.
		// 与 Anthropic 平台 CC 出口（ForwardAsChatCompletions）同语义：本路径的
		// 转换链与之完全一致，网关是计费链路的一环，下游 usage 输出不能绑定到
		// 客户端是否显式请求 include_usage，否则级联代理计费为 0。
		// 必须在 processAnthropicEvent 之前按原始事件判断：其内部的
		// normalizeAnthropicEventUsageForResponses 会补出合成 usage，之后再判断
		// 就无法区分「上游给了 usage」与「转换器补的零值 usage」。
		ccState.IncludeUsage = ccState.IncludeUsage || anthropicChatStreamHasUsage(&event, payload)

		if processAnthropicEvent(&event) {
			return resultWithUsage(), nil
		}
	}

	// Finalize both state machines（客户端已断开时仍执行，保证 usage 汇总完整）。
	finalResEvents := apicompat.FinalizeAnthropicResponsesStream(anthState)
	for _, resEvt := range finalResEvents {
		ccChunks := apicompat.ResponsesEventToChatChunks(&resEvt, ccState)
		for _, chunk := range ccChunks {
			writeChunk(chunk) //nolint:errcheck
		}
	}
	finalCCChunks := apicompat.FinalizeResponsesChatStream(ccState)
	for _, chunk := range finalCCChunks {
		writeChunk(chunk) //nolint:errcheck
	}

	// 收尾闸门：整条流没有任何可渲染内容 → 交给 reportEmptyContent 统一处置
	// （记账 + 停调 + 能换号时换号）。stop_reason=max_tokens 属客户端输出预算不足，
	// 由执法点放行，不判账号故障。
	//
	// 这里刻意不看 emptyGateGivenUp：它只表示「换号窗口已关」（思考已放行或缓冲
	// 溢出），不代表不记账——客户照样拿到空白回答，后台必须留痕。能不能换号由
	// CanFailover（真实提交状态）决定。
	if !clientDisconnected && !payloadContentSeen {
		// WriteHeaders 会把本地 clientOutputStarted 置位；下面命中提示后立即 return，
		// 不会再走到 flushPendingCC / 补 [DONE]，故该标志变化无副作用。
		noticeWritten, err := s.reportEmptyContent(ctx, c, emptyContentReport{
			Account:           account,
			Resp:              resp,
			UpstreamModel:     upstreamModel,
			UpstreamRequestID: requestID,
			LogPrefix:         "CC→CN Anthropic 直通",
			Stage:             "chat stream",
			SawContent:        payloadContentSeen,
			FinishReason:      upstreamStopReason,
			CanFailover:       !clientOutputStarted,
			WriteHeaders:      writeStreamHeaders,
		})
		if err != nil {
			return nil, err
		}
		if noticeWritten {
			return resultWithUsage(), nil
		}
	}
	flushPendingCC()

	if !clientDisconnected {
		// 空流正常收尾也要提交响应头（上游未发任何事件即 EOF 时 [DONE] 即首字节）。
		writeStreamHeaders()
		fmt.Fprint(c.Writer, "data: [DONE]\n\n") //nolint:errcheck
		c.Writer.Flush()
	}

	return resultWithUsage(), nil
}

// chatChunkCarriesPayloadContent 判断一个 Chat Completions chunk 是否携带客户端
// 能看见的内容（正文 / 工具调用 / 拒答）。
//
// 只有 role 帧、reasoning_content（思考）、finish_reason、usage 的 chunk 都不算：
// 它们在「上游只思考没正文」时会全部发出，但客户端看到的是一个空白回答。
func chatChunkCarriesPayloadContent(chunk apicompat.ChatCompletionsChunk) bool {
	for _, choice := range chunk.Choices {
		if choice.Delta.Content != nil && *choice.Delta.Content != "" {
			return true
		}
		if choice.Delta.Refusal != nil && *choice.Delta.Refusal != "" {
			return true
		}
		if len(choice.Delta.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// chatChunkCarriesReasoning 判断 chunk 是否携带思考增量（reasoning_content /
// reasoning）。它不算「内容」（客户端不该据此认为答案来了），但只要它在流动，
// 就说明上游在正常产出，扣着不发给用户看是不对的。
func chatChunkCarriesReasoning(chunk apicompat.ChatCompletionsChunk) bool {
	for _, choice := range chunk.Choices {
		if choice.Delta.ReasoningContent != nil && *choice.Delta.ReasoningContent != "" {
			return true
		}
		if choice.Delta.Reasoning != nil && *choice.Delta.Reasoning != "" {
			return true
		}
	}
	return false
}
