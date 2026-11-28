package service

// 本文件由 gateway_service.go 纯移动拆分而来：Anthropic APIKey 直通
// （passthrough）转发路径及其流式/非流式响应与 usage 解析。仅做代码搬迁，
// 无任何行为变更。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

type anthropicPassthroughForwardInput struct {
	Body          []byte
	Parsed        *ParsedRequest
	RequestModel  string
	OriginalModel string
	RequestStream bool
	StartTime     time.Time
}

const maxRecentAnthropicStreamEvents = 16

type anthropicStreamDiagnostics struct {
	startedAt                time.Time
	audit                    *upstreamCompletionAudit
	terminalEventSeen        bool
	terminalEventForwarded   bool
	terminalSignal           string
	terminationReason        string
	readErrorCategory        string
	emptyOutputAuditResult   string
	sseLineCount             int
	dataEventCount           int
	contentBlockStartEvents  int
	textDeltaEvents          int
	thinkingDeltaEvents      int
	toolInputJSONDeltaEvents int
	textOutputBytes          int
	toolInputJSONBytes       int
	eventTypeCounts          map[string]int
	recentEventTypes         []string
}

func newAnthropicStreamDiagnostics() *anthropicStreamDiagnostics {
	return &anthropicStreamDiagnostics{
		startedAt:              time.Now(),
		audit:                  &upstreamCompletionAudit{},
		terminationReason:      "handler_exit_without_outcome",
		readErrorCategory:      "none",
		emptyOutputAuditResult: "not_evaluated",
		eventTypeCounts:        make(map[string]int),
		recentEventTypes:       make([]string, 0, maxRecentAnthropicStreamEvents),
	}
}

func (d *anthropicStreamDiagnostics) observeData(data string) {
	d.dataEventCount++
	eventType := gjson.Get(data, "type").String()
	diagnosticType := anthropicStreamDiagnosticEventType(eventType)
	d.eventTypeCounts[diagnosticType]++
	if len(d.recentEventTypes) == maxRecentAnthropicStreamEvents {
		copy(d.recentEventTypes, d.recentEventTypes[1:])
		d.recentEventTypes[len(d.recentEventTypes)-1] = diagnosticType
	} else {
		d.recentEventTypes = append(d.recentEventTypes, diagnosticType)
	}

	if d.terminalSignal == "" {
		switch strings.TrimSpace(data) {
		case "[DONE]":
			d.terminalSignal = "done_marker"
		default:
			if eventType == "message_stop" {
				d.terminalSignal = "message_stop_payload"
			}
		}
	}

	switch eventType {
	case "content_block_start":
		d.contentBlockStartEvents++
	case "content_block_delta":
		switch gjson.Get(data, "delta.type").String() {
		case "text_delta":
			d.textDeltaEvents++
			d.textOutputBytes += len(gjson.Get(data, "delta.text").String())
		case "thinking_delta":
			d.thinkingDeltaEvents++
		case "input_json_delta":
			d.toolInputJSONDeltaEvents++
			d.toolInputJSONBytes += len(gjson.Get(data, "delta.partial_json").String())
		}
	}
	d.audit.observeAnthropicSSE(data)
}

func (d *anthropicStreamDiagnostics) observeEventName(eventName string) {
	if anthropicStreamEventIsTerminal(eventName, "") {
		if d.terminalSignal == "" {
			d.terminalSignal = "message_stop_event"
		}
	}
}

func (d *anthropicStreamDiagnostics) observeCompatLine(stream *anthropicCompatStream, line string) {
	d.sseLineCount++
	if eventName, ok := parseAnthropicSSEField(line, "event"); ok {
		d.observeEventName(eventName)
	}
	if data, ok := extractOpenAISSEDataLine(line); ok {
		data = strings.TrimSpace(data)
		d.observeData(data)
		if d.terminalSignal == "" && gjson.Get(data, "type").String() == "message_delta" && gjson.Get(data, "delta.stop_reason").String() != "" {
			d.terminalSignal = "message_delta_stop_reason"
		}
	}
	if stream.sawTerminal {
		d.terminalEventSeen = true
	}
}

func (d *anthropicStreamDiagnostics) finishCompatStream(stream *anthropicCompatStream) {
	d.terminalEventSeen = stream.sawTerminal
	switch {
	case stream.upstreamErrType != "":
		d.terminationReason = "upstream_sse_error_event"
	case errors.Is(stream.readErr, errAnthropicNativeStreamIdle):
		d.terminationReason = "stream_data_interval_timeout"
		d.readErrorCategory = "stream_idle_timeout"
	case stream.readErr != nil && !errors.Is(stream.readErr, io.EOF):
		d.readErrorCategory = anthropicStreamReadErrorCategory(stream.readErr)
		if stream.clientDisconnected {
			d.terminationReason = "upstream_read_error_after_client_disconnect"
		} else {
			d.terminationReason = "upstream_read_error"
		}
	case errors.Is(stream.readErr, io.EOF) && stream.sawTerminal:
		d.terminationReason = "upstream_eof_after_terminal_event"
	case errors.Is(stream.readErr, io.EOF):
		d.terminationReason = "eof_missing_terminal_event"
	case stream.clientDisconnected:
		d.terminationReason = "client_disconnect_before_upstream_end"
	}
}

func (d *anthropicStreamDiagnostics) log(
	ctx context.Context,
	c *gin.Context,
	resp *http.Response,
	account *Account,
	model string,
	upstreamModel string,
	forwardingMode string,
	mimicClaudeCode bool,
	usage *ClaudeUsage,
	firstTokenMs *int,
	clientDisconnected bool,
	partialEventPending bool,
) {
	responseBytes := -1
	responseStatus := 0
	requestMethod := ""
	requestPath := ""
	if c != nil && c.Writer != nil {
		responseBytes = c.Writer.Size()
		responseStatus = c.Writer.Status()
	}
	if c != nil && c.Request != nil {
		requestMethod = c.Request.Method
		if c.Request.URL != nil {
			requestPath = c.Request.URL.Path
		}
	}
	firstTokenSeen := firstTokenMs != nil
	firstTokenValue := 0
	if firstTokenSeen {
		firstTokenValue = *firstTokenMs
	}
	var accountID int64
	var accountPlatform string
	var accountType string
	if account != nil {
		accountID = account.ID
		accountPlatform = account.Platform
		accountType = account.Type
	}
	if ctx == nil {
		ctx = context.Background()
	}
	logger.FromContext(ctx).Info("anthropic_stream_termination",
		zap.Int64("account_id", accountID),
		zap.String("account_platform", accountPlatform),
		zap.String("account_type", accountType),
		zap.String("forwarding_mode", forwardingMode),
		zap.String("request_method", requestMethod),
		zap.String("request_path", requestPath),
		zap.String("model", model),
		zap.String("upstream_model", upstreamModel),
		zap.Bool("claude_code_client", IsClaudeCodeClient(ctx)),
		zap.Bool("mimic_claude_code", mimicClaudeCode),
		zap.Int("upstream_status", resp.StatusCode),
		zap.Int("client_response_status", responseStatus),
		zap.String("upstream_request_id", resp.Header.Get("x-request-id")),
		zap.String("termination_reason", d.terminationReason),
		zap.String("read_error_category", d.readErrorCategory),
		zap.String("empty_output_audit_result", d.emptyOutputAuditResult),
		zap.Bool("terminal_event_seen", d.terminalEventSeen),
		zap.Bool("terminal_event_forwarded", d.terminalEventForwarded),
		zap.String("terminal_signal", d.terminalSignal),
		zap.String("stop_reason", anthropicStreamDiagnosticStopReason(d.audit.stopReason)),
		zap.Int("sse_line_count", d.sseLineCount),
		zap.Int("data_event_count", d.dataEventCount),
		zap.Any("data_event_type_counts", d.eventTypeCounts),
		zap.Strings("recent_data_event_types", d.recentEventTypes),
		zap.Int("content_block_start_events", d.contentBlockStartEvents),
		zap.Int("text_delta_events", d.textDeltaEvents),
		zap.Int("text_output_bytes", d.textOutputBytes),
		zap.Int("tool_use_count", d.audit.toolUses),
		zap.Int("tool_input_json_delta_events", d.toolInputJSONDeltaEvents),
		zap.Int("tool_input_json_bytes", d.toolInputJSONBytes),
		zap.Int("thinking_delta_events", d.thinkingDeltaEvents),
		zap.Bool("thinking_seen", d.audit.thinkingSeen),
		zap.Int("usage_input_tokens", usage.InputTokens),
		zap.Int("usage_output_tokens", usage.OutputTokens),
		zap.Int("usage_cache_read_input_tokens", usage.CacheReadInputTokens),
		zap.Int("usage_cache_creation_input_tokens", usage.CacheCreationInputTokens),
		zap.Bool("client_disconnected", clientDisconnected),
		zap.Bool("partial_event_pending", partialEventPending),
		zap.Int("client_response_bytes", responseBytes),
		zap.Bool("first_token_seen", firstTokenSeen),
		zap.Int("first_token_ms", firstTokenValue),
		zap.Int64("stream_elapsed_ms", time.Since(d.startedAt).Milliseconds()),
	)
}

func (s *GatewayService) forwardAnthropicAPIKeyPassthrough(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	reqModel string,
	originalModel string,
	reqStream bool,
	startTime time.Time,
) (*ForwardResult, error) {
	return s.forwardAnthropicAPIKeyPassthroughWithInput(ctx, c, account, anthropicPassthroughForwardInput{
		Body:          body,
		RequestModel:  reqModel,
		OriginalModel: originalModel,
		RequestStream: reqStream,
		StartTime:     startTime,
	})
}

func (s *GatewayService) forwardAnthropicAPIKeyPassthroughWithInput(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	input anthropicPassthroughForwardInput,
) (result *ForwardResult, err error) {
	// G5 观测（单点埋点）：请求出口补发整条换号链未解析的 guard 终态关联事件（同
	// request_id）。无 tracker 时为 no-op。
	defer func() { cf524ResolveGuardOutcome(c, ctx, err) }()
	token, tokenType, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	if tokenType != "apikey" {
		return nil, fmt.Errorf("anthropic api key passthrough requires apikey token, got: %s", tokenType)
	}

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	logger.LegacyPrintf("service.gateway", "[Anthropic 自动透传] 命中 API Key 透传分支: account=%d name=%s model=%s stream=%v",
		account.ID, account.Name, input.RequestModel, input.RequestStream)

	if c != nil {
		c.Set("anthropic_passthrough", true)
	}
	// Pre-filter: strip empty text blocks (including nested in tool_result) to prevent upstream 400.
	input.Body = StripEmptyTextBlocks(input.Body)
	// Pre-filter: strip web-search history blocks the upstream cannot accept
	// (emulation-synthesized ones always; genuine ones additionally for
	// passback-required third-party upstreams such as GLM/Kimi/DeepSeek,
	// which reject server_tool_use with 400). input.RequestModel 已是映射后的模型 ID。
	input.Body = FilterWebSearchHistoryBlocks(input.Body, input.RequestModel)
	if input.Parsed != nil {
		// 透传分支也会改写实际 wire body，成功 usage hash 依赖这里同步当前 body。
		if err := input.Parsed.ReplaceBody(input.Body); err != nil {
			return nil, err
		}
	}

	var resp *http.Response
	retryStart := time.Now()

	// G2a 挂点 2：取 G1a 预算快照与 G2b 安装的心跳 owner（请求级，挂点只消费、禁止重建）。
	snapshot, hasSnapshot := RequestBudgetSnapshotFromContext(ctx)
	hbOwner, _ := UpstreamHeartbeatFromContext(ctx)
	// heartbeat_delay=0 语义为禁用（方案 D2）：owner 即便存在也不起搏。
	if hasSnapshot && snapshot.HeartbeatDelaySeconds <= 0 {
		hbOwner = nil
	}

	for attempt := 1; attempt <= maxRetryAttempts; attempt++ {
		upstreamCtx, releaseUpstreamCtx := detachStreamUpstreamContext(ctx, input.RequestStream)
		// G2a 挂点 2：接 G1a 首字节护栏。窗口从不可变截止时间取剩余（禁止本层重初始化计时，
		// 换号不重置）。guard 的 reqCtx 写入请求 context：窗口耗尽取消 reqCtx 中断 header 等待。
		var guard *upstreamFirstByteGuard
		if hasSnapshot {
			window := snapshot.AttemptWindow(time.Now())
			guard, upstreamCtx = newUpstreamFirstByteGuard(upstreamCtx, window)
		}
		upstreamReq, wireBody, err := s.buildUpstreamRequestAnthropicAPIKeyPassthrough(upstreamCtx, c, account, input.Body, token)
		releaseUpstreamCtx()
		if err != nil {
			if guard != nil {
				guard.Stop()
			}
			if hbOwner != nil && input.RequestStream {
				hbOwner.Stop()
			}
			return nil, err
		}
		if input.Parsed != nil && !bytes.Equal(wireBody, input.Body) {
			// build 阶段会按 beta 能力清理 body，发送前同步到 ParsedRequest 当前视图。
			if err := input.Parsed.ReplaceBody(wireBody); err != nil {
				return nil, err
			}
			input.Body = input.Parsed.Body.Bytes()
		}

		// 发送请求（统一经护栏 + 心跳生命周期编排）。必须用预声明 + "=" 赋值：resp 是
		// 循环外声明的接收变量，":=" 会在循环块内遮蔽它，导致循环后 resp 仍为 nil。
		// G5 观测：obs 非 nil 时共享执行器为唯一发射点。
		obs := &cf524GuardObservation{
			c:         c,
			ctx:       ctx,
			platform:  account.Platform,
			stream:    input.RequestStream,
			attempt:   attempt,
			startedAt: time.Now(),
			snapshot:  snapshot,
		}
		var guardTimedOut bool
		resp, err, guardTimedOut = s.cf524ExecuteUpstreamWithGuard(upstreamReq, proxyURL, account, s.tlsFPProfileService.ResolveTLSProfile(account), guard, hbOwner, input.RequestStream, obs)
		if guardTimedOut {
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			remaining := time.Duration(0)
			if hasSnapshot {
				remaining = snapshot.RemainingBudget(time.Now())
			}
			return nil, newUpstreamFirstByteTimeoutError(remaining)
		}
		if err != nil {
			return nil, s.handleUpstreamTransportError(ctx, c, account, err, OpsUpstreamErrorEvent{
				UpstreamURL: safeUpstreamURL(upstreamReq.URL.String()),
				Passthrough: true,
			})
		}

		// 透传分支禁止 400 请求体降级重试（该重试会改写请求体）
		if resp.StatusCode >= 400 && resp.StatusCode != 400 && s.shouldRetryUpstreamError(account, resp.StatusCode) {
			if attempt < maxRetryAttempts {
				elapsed := time.Since(retryStart)
				if elapsed >= maxRetryElapsed {
					break
				}

				delay := retryBackoffDelay(attempt)
				remaining := maxRetryElapsed - elapsed
				if delay > remaining {
					delay = remaining
				}
				if delay <= 0 {
					break
				}

				respBody, _ := s.readUpstreamErrorBody(resp)
				_ = resp.Body.Close()
				appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
					ProxyID:            opsUpstreamProxyID(account),
					ProxyName:          opsUpstreamProxyName(account),
					Platform:           account.Platform,
					AccountID:          account.ID,
					AccountName:        account.Name,
					UpstreamStatusCode: resp.StatusCode,
					UpstreamRequestID:  resp.Header.Get("x-request-id"),
					UpstreamURL:        safeUpstreamURL(upstreamReq.URL.String()),
					Passthrough:        true,
					Kind:               "retry",
					Message:            extractUpstreamErrorMessage(respBody),
					Detail: func() string {
						if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
							return truncateString(string(respBody), s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes)
						}
						return ""
					}(),
				})
				logger.LegacyPrintf("service.gateway", "Anthropic passthrough account %d: upstream error %d, retry %d/%d after %v (elapsed=%v/%v)",
					account.ID, resp.StatusCode, attempt, maxRetryAttempts, delay, elapsed, maxRetryElapsed)
				if err := sleepWithContext(ctx, delay); err != nil {
					return nil, err
				}
				continue
			}
			break
		}

		break
	}
	if resp == nil || resp.Body == nil {
		return nil, errors.New("upstream request failed: empty response")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 && s.shouldRetryUpstreamError(account, resp.StatusCode) {
		if s.shouldFailoverUpstreamError(account, resp.StatusCode, input.RequestModel) {
			respBody, _ := s.readUpstreamErrorBody(resp)
			_ = resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(respBody))

			logger.LegacyPrintf("service.gateway", "[Anthropic Passthrough] Upstream error (retry exhausted, failover): Account=%d(%s) Status=%d RequestID=%s Body=%s",
				account.ID, account.Name, resp.StatusCode, resp.Header.Get("x-request-id"), truncateString(string(respBody), 1000))

			s.handleRetryExhaustedSideEffects(ctx, resp, account, input.RequestModel)
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				ProxyID:            opsUpstreamProxyID(account),
				ProxyName:          opsUpstreamProxyName(account),
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: resp.StatusCode,
				UpstreamRequestID:  resp.Header.Get("x-request-id"),
				Passthrough:        true,
				Kind:               "retry_exhausted_failover",
				Message:            extractUpstreamErrorMessage(respBody),
				Detail: func() string {
					if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
						return truncateString(string(respBody), s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes)
					}
					return ""
				}(),
			})
			return nil, &UpstreamFailoverError{
				StatusCode:             resp.StatusCode,
				ResponseBody:           respBody,
				RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
			}
		}
		return s.handleRetryExhaustedError(ctx, resp, c, account, input.RequestModel)
	}

	if resp.StatusCode >= 400 && s.shouldFailoverUpstreamError(account, resp.StatusCode, input.RequestModel) {
		respBody, _ := s.readUpstreamErrorBody(resp)
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(respBody))

		logger.LegacyPrintf("service.gateway", "[Anthropic Passthrough] Upstream error (failover): Account=%d(%s) Status=%d RequestID=%s Body=%s",
			account.ID, account.Name, resp.StatusCode, resp.Header.Get("x-request-id"), truncateString(string(respBody), 1000))

		s.handleFailoverSideEffects(ctx, resp, account, input.RequestModel)
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			ProxyID:            opsUpstreamProxyID(account),
			ProxyName:          opsUpstreamProxyName(account),
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  resp.Header.Get("x-request-id"),
			Passthrough:        true,
			Kind:               "failover",
			Message:            extractUpstreamErrorMessage(respBody),
			Detail: func() string {
				if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
					return truncateString(string(respBody), s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes)
				}
				return ""
			}(),
		})
		return nil, &UpstreamFailoverError{
			StatusCode:             resp.StatusCode,
			ResponseBody:           respBody,
			RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
		}
	}

	if resp.StatusCode >= 400 {
		return s.handleErrorResponse(ctx, resp, c, account, input.RequestModel)
	}

	var usage *ClaudeUsage
	var firstTokenMs *int
	var clientDisconnect bool
	if input.RequestStream {
		streamResult, err := s.handleStreamingResponseAnthropicAPIKeyPassthrough(ctx, resp, c, account, input.StartTime, input.RequestModel)
		if err != nil {
			// 流中断时保留已观测到的 usage 与错误一起返回，避免上游已计量的请求
			// 完全漏记漏计费（issue #5148）。
			if partial := partialStreamUsageResult(c, resp, streamResult, input.OriginalModel, input.RequestModel, input.StartTime, err); partial != nil {
				return partial, err
			}
			return nil, err
		}
		usage = streamResult.usage
		firstTokenMs = streamResult.firstTokenMs
		clientDisconnect = streamResult.clientDisconnect
	} else {
		usage, err = s.handleNonStreamingResponseAnthropicAPIKeyPassthrough(ctx, resp, c, account)
		if err != nil {
			return nil, err
		}
	}
	if usage == nil {
		usage = &ClaudeUsage{}
	}

	return &ForwardResult{
		RequestID:                     resp.Header.Get("x-request-id"),
		UpstreamHeaders:               resp.Header,
		Usage:                         *usage,
		Model:                         input.OriginalModel,
		UpstreamModel:                 input.RequestModel,
		UpstreamResponseModel:         observedUpstreamResponseModel(c),
		UpstreamResponseModelConflict: observedUpstreamResponseModelConflict(c),
		UpstreamResponseServiceTier:   observedUpstreamResponseServiceTier(c),
		Stream:                        input.RequestStream,
		Duration:                      time.Since(input.StartTime),
		FirstTokenMs:                  firstTokenMs,
		ClientDisconnect:              clientDisconnect,
	}, nil
}

func (s *GatewayService) buildUpstreamRequestAnthropicAPIKeyPassthrough(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	token string,
) (*http.Request, []byte, error) {
	body = stripDeferredToolCacheControl(body)
	targetURL := claudeAPIURL
	baseURL := getAnthropicGatewayBaseURL(account)
	if baseURL != "" {
		validatedURL, err := s.validateUpstreamBaseURL(baseURL)
		if err != nil {
			return nil, nil, err
		}
		// 版本感知拼接（同源 upstream_endpoint_url.go）：base 已含版本段（如 /v1、/v4）时只追加
		// 相对路径，避免裸拼 "/v1/messages" 造成 /v1/v1 双段 404。
		targetURL = buildVersionedEndpointURL(endpointFamilyAnthropic, validatedURL, "/v1/messages") + "?beta=true"
	}

	// 能力维度 body sanitize：透传路径上 anthropic-beta header 原样透传客户端值，
	// 依此决定是否保留 body 中的 context_management。避免“客户端 body 带字段但
	// header 忘记带 beta token”的客户端 bug 在透传场景下让上游 400。
	clientBeta := ""
	if c != nil && c.Request != nil {
		clientBeta = getHeaderRaw(c.Request.Header, "anthropic-beta")
	}
	// 账号覆写了 anthropic-beta 时，覆写值即最终上游值：净化以覆写值为准
	if beta, ok := account.HeaderOverrideValue("anthropic-beta"); ok {
		clientBeta = beta
	}
	if sanitized, changed := sanitizeAnthropicBodyForBetaTokens(body, clientBeta); changed {
		body = sanitized
	}

	// Ollama Cloud DeepSeek 出站 max_tokens clamp：判定与上方 targetURL 的
	// base 取值同源，详见 helper 注释。
	body = clampOllamaCloudAnthropicMessagesMaxTokens(account, baseURL, body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}

	if c != nil && c.Request != nil {
		for key, values := range c.Request.Header {
			lowerKey := strings.ToLower(strings.TrimSpace(key))
			if !allowedHeaders[lowerKey] {
				continue
			}
			wireKey := resolveWireCasing(key)
			for _, v := range values {
				addHeaderRaw(req.Header, wireKey, v)
			}
		}
	}

	// 覆盖入站鉴权残留，并注入上游认证
	req.Header.Del("authorization")
	req.Header.Del("x-api-key")
	req.Header.Del("x-goog-api-key")
	req.Header.Del("cookie")
	// Ollama Cloud Anthropic 兼容端点按实际 base_url 强制 Bearer（同上方
	// targetURL 的 base 取值），其余保持 extra/default 行为。
	setAnthropicAPIKeyAuthHeader(req.Header, account, token, baseURL)

	if getHeaderRaw(req.Header, "content-type") == "" {
		setHeaderRaw(req.Header, "content-type", "application/json")
	}
	if getHeaderRaw(req.Header, "anthropic-version") == "" {
		setHeaderRaw(req.Header, "anthropic-version", "2023-06-01")
	}

	// 账号级请求头覆写（最终生效，覆盖上面所有来源的同名头）
	account.ApplyHeaderOverrides(req.Header)
	filterSonnet55ToolsetBetaHeader(req.Header, body, gjson.GetBytes(body, "model").String())

	return req, body, nil
}

func (s *GatewayService) handleStreamingResponseAnthropicAPIKeyPassthrough(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	startTime time.Time,
	model string,
) (*streamingResult, error) {
	diagnostics := newAnthropicStreamDiagnostics()
	usage := &ClaudeUsage{}
	audit := diagnostics.audit
	var firstTokenMs *int
	clientDisconnected := false
	eventBuffer := &contextWarnEventBuffer{}
	inPartialEvent := false
	defer func() {
		diagnostics.log(ctx, c, resp, account, model, model, "api_key_passthrough", false,
			usage, firstTokenMs, clientDisconnected, inPartialEvent || eventBuffer.active())
	}()

	observer := upstreamResponseModelObserverFromContext(c)
	if observer == nil {
		observer = beginUpstreamResponseModelObservation(c)
	}
	if s.rateLimitService != nil {
		s.rateLimitService.UpdateSessionWindow(ctx, account, resp.Header)
	}

	writeAnthropicPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)

	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "text/event-stream"
	}
	c.Header("Content-Type", contentType)
	if c.Writer.Header().Get("Cache-Control") == "" {
		c.Header("Cache-Control", "no-cache")
	}
	if c.Writer.Header().Get("Connection") == "" {
		c.Header("Connection", "keep-alive")
	}
	c.Header("X-Accel-Buffering", "no")
	if v := resp.Header.Get("x-request-id"); v != "" {
		c.Header("x-request-id", v)
	}

	w := c.Writer
	flusher, ok := w.(http.Flusher)
	if !ok {
		diagnostics.terminationReason = "streaming_unsupported"
		return nil, errors.New("streaming not supported")
	}

	// 提醒插在完整 message_delta 事件之前：只跟踪已见的最大 content block 索引与
	// 是否出现 tool_use。每个上游事件从首行起整段缓冲到事件边界空行后再判定，
	// 事件名或 data.type 任一命中 message_delta 即先写提醒再原样写出整段事件，
	// 兼容 event: 在 data: 之前、之后以及缺失 event: 三种行序；message_stop 不再注入。
	warnMaxIndex := -1
	warnSawToolUse := false
	// eventBufferDrained 使补出幂等：无论从哪个出口触发、触发多少次，都只补出一次。
	eventBufferDrained := false

	scanner := bufio.NewScanner(resp.Body)
	maxLineSize := defaultMaxLineSize
	if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.cfg.Gateway.MaxLineSize
	}
	scanBuf := getSSEScannerBuf64K()
	scanner.Buffer(scanBuf[:0], maxLineSize)

	type scanEvent struct {
		line string
		err  error
	}
	events := make(chan scanEvent, 16)
	done := make(chan struct{})
	sendEvent := func(ev scanEvent) bool {
		select {
		case events <- ev:
			return true
		case <-done:
			return false
		}
	}
	var lastReadAt int64
	atomic.StoreInt64(&lastReadAt, time.Now().UnixNano())
	go func(scanBuf *sseScannerBuf64K) {
		defer putSSEScannerBuf64K(scanBuf)
		defer close(events)
		for scanner.Scan() {
			atomic.StoreInt64(&lastReadAt, time.Now().UnixNano())
			if !sendEvent(scanEvent{line: scanner.Text()}) {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			_ = sendEvent(scanEvent{err: err})
		}
	}(scanBuf)
	defer close(done)

	streamInterval := time.Duration(0)
	if s.cfg != nil && s.cfg.Gateway.StreamDataIntervalTimeout > 0 {
		streamInterval = time.Duration(s.cfg.Gateway.StreamDataIntervalTimeout) * time.Second
	}
	var intervalTicker *time.Ticker
	if streamInterval > 0 {
		intervalTicker = time.NewTicker(streamInterval)
		defer intervalTicker.Stop()
	}
	var intervalCh <-chan time.Time
	if intervalTicker != nil {
		intervalCh = intervalTicker.C
	}

	keepaliveInterval := time.Duration(0)
	if s.cfg != nil && s.cfg.Gateway.StreamKeepaliveInterval > 0 {
		keepaliveInterval = time.Duration(s.cfg.Gateway.StreamKeepaliveInterval) * time.Second
	}
	var keepaliveTimer *time.Timer
	if keepaliveInterval > 0 {
		keepaliveTimer = time.NewTimer(keepaliveInterval)
		defer keepaliveTimer.Stop()
	}
	var keepaliveCh <-chan time.Time
	if keepaliveTimer != nil {
		keepaliveCh = keepaliveTimer.C
	}
	lastDataAt := time.Now()
	resetKeepaliveTimer := func() {
		if keepaliveTimer == nil {
			return
		}
		if !keepaliveTimer.Stop() {
			select {
			case <-keepaliveTimer.C:
			default:
			}
		}
		keepaliveTimer.Reset(keepaliveInterval)
	}
	// finishUpstreamStream 是「流正常结束」的唯一出口。
	// 先判「上游空完成」再当成功返回：判据与处置都只在 upstreamCompletionAudit 里实现，
	// 这里不允许再就地写一套「内容为空」的判断（否则就是同一规则的第二份实现）。
	finishUpstreamStream := func() (*streamingResult, error) {
		result := &streamingResult{usage: usage, firstTokenMs: firstTokenMs, clientDisconnect: clientDisconnected}
		if clientDisconnected {
			diagnostics.emptyOutputAuditResult = "skipped_client_disconnect"
			return result, nil
		}
		if audit.isEmptyCompletion(usage) {
			diagnostics.emptyOutputAuditResult = "empty_output"
			return result, s.noteEmptyUpstreamCompletion(ctx, account, model, usage, audit)
		}
		diagnostics.emptyOutputAuditResult = "non_empty_output"
		return result, nil
	}

	// drainEventBuffer 把未闭合的事件缓冲原样写出（不注入提醒），保证终止事件已
	// 识别但最后空行未到的截断出口不丢尾。函数级 defer 覆盖所有退出路径，分支内
	// 不再各自调用；eventBufferDrained 保证幂等，客户端断连或缓冲为空时为 no-op。
	// 写出失败静默返回，不改 clientDisconnected 状态，沿用调用方既有的错误/断连处置。
	drainEventBuffer := func() {
		if eventBufferDrained || clientDisconnected || !eventBuffer.active() {
			return
		}
		eventBufferDrained = true
		terminalEvent := eventBuffer.isTerminal()
		allLinesWritten := true
		for _, buffered := range eventBuffer.flush() {
			if _, werr := io.WriteString(w, buffered); werr != nil {
				allLinesWritten = false
				break
			}
			if _, werr := io.WriteString(w, "\n"); werr != nil {
				allLinesWritten = false
				break
			}
		}
		if allLinesWritten {
			if terminalEvent {
				diagnostics.terminalEventForwarded = true
			}
			flusher.Flush()
		}
	}
	// 唯一退出保证：挂在缓冲与补出函数定义之后、主循环之前，覆盖 EOF、读错误、
	// 超时、正常结束等全部 return 路径。
	defer drainEventBuffer()

	// writePassthroughLine 原样写出一行（含换行）；写出失败置 clientDisconnected
	// 并沿用既有日志，返回是否写出成功。
	writePassthroughLine := func(raw string) bool {
		if _, werr := io.WriteString(w, raw); werr != nil {
			clientDisconnected = true
			logger.LegacyPrintf("service.gateway", "[Anthropic passthrough] Client disconnected during streaming, continue draining upstream for usage: account=%d", account.ID)
			return false
		}
		if _, werr := io.WriteString(w, "\n"); werr != nil {
			clientDisconnected = true
			logger.LegacyPrintf("service.gateway", "[Anthropic passthrough] Client disconnected during streaming, continue draining upstream for usage: account=%d", account.ID)
			return false
		}
		return true
	}

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				if !clientDisconnected {
					// 兜底补刷，确保最后一个未以空行结尾的事件也能及时送达客户端。
					flusher.Flush()
				}
				if !diagnostics.terminalEventSeen {
					// 未闭合缓冲由函数级 defer 统一补出，不注入提醒。
					if clientDisconnected && streamInterval > 0 {
						lastRead := time.Unix(0, atomic.LoadInt64(&lastReadAt))
						if time.Since(lastRead) >= streamInterval {
							diagnostics.terminationReason = "client_disconnect_usage_timeout"
							return &streamingResult{usage: usage, firstTokenMs: firstTokenMs, clientDisconnect: true}, fmt.Errorf("stream usage incomplete after timeout")
						}
					}
					diagnostics.terminationReason = "eof_missing_terminal_event"
					return &streamingResult{usage: usage, firstTokenMs: firstTokenMs, clientDisconnect: clientDisconnected}, fmt.Errorf("stream usage incomplete: missing terminal event")
				}
				diagnostics.terminationReason = "upstream_eof_after_terminal_event"
				return finishUpstreamStream()
			}
			if ev.err != nil {
				diagnostics.readErrorCategory = anthropicStreamReadErrorCategory(ev.err)
				if diagnostics.terminalEventSeen {
					diagnostics.terminationReason = "upstream_read_error_after_terminal_event"
					return finishUpstreamStream()
				}
				// 未闭合缓冲由函数级 defer 统一补出，这里沿用既有错误处置。
				if clientDisconnected {
					diagnostics.terminationReason = "upstream_read_error_after_client_disconnect"
					return &streamingResult{usage: usage, firstTokenMs: firstTokenMs, clientDisconnect: true}, fmt.Errorf("stream usage incomplete after disconnect: %w", ev.err)
				}
				if errors.Is(ev.err, context.Canceled) || errors.Is(ev.err, context.DeadlineExceeded) {
					diagnostics.terminationReason = "upstream_context_canceled"
					if errors.Is(ev.err, context.DeadlineExceeded) {
						diagnostics.terminationReason = "upstream_context_deadline_exceeded"
					}
					return &streamingResult{usage: usage, firstTokenMs: firstTokenMs, clientDisconnect: true}, fmt.Errorf("stream usage incomplete: %w", ev.err)
				}
				if errors.Is(ev.err, bufio.ErrTooLong) {
					diagnostics.terminationReason = "sse_line_too_long"
					logger.LegacyPrintf("service.gateway", "[Anthropic passthrough] SSE line too long: account=%d max_size=%d error=%v", account.ID, maxLineSize, ev.err)
					return &streamingResult{usage: usage, firstTokenMs: firstTokenMs}, ev.err
				}
				diagnostics.terminationReason = "upstream_read_error"
				return &streamingResult{usage: usage, firstTokenMs: firstTokenMs}, fmt.Errorf("stream read error: %w", ev.err)
			}

			line := ev.line
			diagnostics.sseLineCount++
			if data, ok := extractAnthropicSSEDataLine(line); ok {
				trimmed := strings.TrimSpace(data)
				diagnostics.observeData(trimmed)
				observer.ObserveAnthropic([]byte(trimmed))
				if anthropicStreamEventIsTerminal("", trimmed) {
					diagnostics.terminalEventSeen = true
				}
				if firstTokenMs == nil && trimmed != "" && trimmed != "[DONE]" {
					ms := int(time.Since(startTime).Milliseconds())
					firstTokenMs = &ms
				}
				parseSSEUsagePassthrough(data, usage)
				typ := gjson.Get(data, "type").String()
				if typ == "content_block_start" {
					if idx := int(gjson.Get(data, "index").Int()); idx > warnMaxIndex {
						warnMaxIndex = idx
					}
					if gjson.Get(data, "content_block.type").String() == "tool_use" {
						warnSawToolUse = true
					}
				}
			} else {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "event:") {
					eventName := strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
					diagnostics.observeEventName(eventName)
					if anthropicStreamEventIsTerminal(eventName, "") {
						diagnostics.terminalEventSeen = true
					}
				}
			}

			if !clientDisconnected {
				// 整事件缓冲：每条上游行从事件首行起收进缓冲，直到边界空行再统一
				// 判定并写出，保证 data: 在 event: 之前/之后以及缺 event: 时都能在
				// 写出前命中提醒，且写出的事件内部行序与上游完全一致。
				if eventBuffer.add(line) {
					// 事件边界：事件名或 data.type 任一命中 message_delta 即在整段
					// 事件之前注入提醒；message_stop 及其余事件零改写原样写出。
					isMessageDelta := eventBuffer.eventName() == "message_delta" || eventBuffer.dataType() == "message_delta"
					if isMessageDelta && !warnSawToolUse {
						ctxTokens := usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
						d := DecideContextWarning(ctx, RequestSizeSessionHash(ctx), account.ID, ctxTokens)
						if d.Text != "" {
							if _, werr := io.WriteString(w, BuildContextWarningSSEAt(warnMaxIndex+1, d.Text)); werr != nil {
								clientDisconnected = true
								logger.LegacyPrintf("service.gateway", "[Anthropic passthrough] Client disconnected during context warning injection, continue draining upstream for usage: account=%d", account.ID)
							}
							flusher.Flush()
						}
					}
					terminalEvent := eventBuffer.isTerminal()
					allLinesWritten := true
					for _, buffered := range eventBuffer.flush() {
						if !writePassthroughLine(string(reverseToolNamesIfPresent(c, []byte(buffered)))) {
							allLinesWritten = false
							break
						}
					}
					if allLinesWritten && terminalEvent {
						diagnostics.terminalEventForwarded = true
					}
					if !clientDisconnected {
						flusher.Flush()
						lastDataAt = time.Now()
						resetKeepaliveTimer()
						inPartialEvent = false
					}
				} else {
					inPartialEvent = true
				}
			}

		case <-intervalCh:
			lastRead := time.Unix(0, atomic.LoadInt64(&lastReadAt))
			if time.Since(lastRead) < streamInterval {
				continue
			}
			// 未闭合缓冲由函数级 defer 统一补出，这里沿用既有超时处置。
			if clientDisconnected {
				diagnostics.terminationReason = "client_disconnect_usage_timeout"
				return &streamingResult{usage: usage, firstTokenMs: firstTokenMs, clientDisconnect: true}, fmt.Errorf("stream usage incomplete after timeout")
			}
			diagnostics.terminationReason = "stream_data_interval_timeout"
			logger.LegacyPrintf("service.gateway", "[Anthropic passthrough] Stream data interval timeout: account=%d model=%s interval=%s", account.ID, model, streamInterval)
			if s.rateLimitService != nil {
				s.rateLimitService.HandleStreamTimeout(ctx, account, model)
			}
			return &streamingResult{usage: usage, firstTokenMs: firstTokenMs}, fmt.Errorf("stream data interval timeout")

		case <-keepaliveCh:
			if clientDisconnected {
				continue
			}
			if inPartialEvent {
				resetKeepaliveTimer()
				continue
			}
			if time.Since(lastDataAt) < keepaliveInterval {
				resetKeepaliveTimer()
				continue
			}
			if _, err := fmt.Fprint(w, "event: ping\ndata: {\"type\": \"ping\"}\n\n"); err != nil {
				clientDisconnected = true
				logger.LegacyPrintf("service.gateway", "[Anthropic passthrough] Client disconnected during keepalive ping, continue draining upstream for usage: account=%d", account.ID)
				continue
			}
			flusher.Flush()
			lastDataAt = time.Now()
			resetKeepaliveTimer()
		}
	}
}

// contextWarnEventBuffer 是逐行驱动的整事件缓冲状态机：从事件首行起缓冲全部行，
// 直到事件边界空行一次性交还调用方判定。调用方按"事件名或 data.type 任一命中"
// 决定是否先注入提醒，再按原顺序写出整段事件。这样无论 data: 在 event: 之前、
// 之后，还是缺失 event: 行，都能在写出前完成判定，不拆散事件内部行序。
type contextWarnEventBuffer struct {
	buffering bool
	lines     []string
}

func anthropicStreamDiagnosticEventType(eventType string) string {
	switch eventType {
	case "message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop", "ping", "error":
		return eventType
	case "":
		return "untyped"
	default:
		return "other"
	}
}

func anthropicStreamDiagnosticStopReason(stopReason string) string {
	switch strings.TrimSpace(stopReason) {
	case "", "end_turn", "max_tokens", "stop_sequence", "tool_use", "refusal", "pause_turn", "model_context_window_exceeded":
		return strings.TrimSpace(stopReason)
	default:
		return "other"
	}
}

// add 收下一行；若当前不在缓冲中则以该行开启新事件缓冲。返回该行是否为事件
// 边界空行（此时整段事件已缓冲完毕，等待调用方判定与写出）。
func (g *contextWarnEventBuffer) add(line string) bool {
	if !g.buffering {
		g.buffering = true
		g.lines = nil
	}
	g.lines = append(g.lines, line)
	return line == ""
}

// active 报告当前是否有未闭合的事件缓冲。
func (g *contextWarnEventBuffer) active() bool { return g.buffering }

// eventName 返回缓冲行中首个 event: 行的事件名；不存在时返回空串。
func (g *contextWarnEventBuffer) eventName() string {
	for _, line := range g.lines {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "event:") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
		}
	}
	return ""
}

// dataType 返回缓冲行中首个 data 行的 type 字段；不存在时返回空串。
func (g *contextWarnEventBuffer) dataType() string {
	for _, line := range g.lines {
		if data, ok := extractAnthropicSSEDataLine(line); ok {
			return gjson.Get(data, "type").String()
		}
	}
	return ""
}

func (g *contextWarnEventBuffer) isTerminal() bool {
	return anthropicSSEEventLinesAreTerminal(g.lines)
}

func anthropicSSEEventLinesAreTerminal(lines []string) bool {
	var eventName string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
		}
		if data, ok := extractAnthropicSSEDataLine(line); ok && anthropicStreamEventIsTerminal("", data) {
			return true
		}
	}
	return anthropicStreamEventIsTerminal(eventName, "")
}

func anthropicStreamReadErrorCategory(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "context_deadline_exceeded"
	case errors.Is(err, bufio.ErrTooLong):
		return "sse_line_too_long"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_eof"
	default:
		return "other"
	}
}

// flush 返回缓冲的全部行并复位状态。
func (g *contextWarnEventBuffer) flush() []string {
	lines := g.lines
	g.buffering = false
	g.lines = nil
	return lines
}

func extractAnthropicSSEDataLine(line string) (string, bool) {
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	start := len("data:")
	for start < len(line) {
		if line[start] != ' ' && line[start] != '\t' {
			break
		}
		start++
	}
	return line[start:], true
}

// parseSSEUsagePassthrough 从 Anthropic SSE data 行提取 usage（包级函数：
// Anthropic 平台 passthrough 与国产供应商原生 Anthropic 直通共用）。
func parseSSEUsagePassthrough(data string, usage *ClaudeUsage) {
	if usage == nil || data == "" || data == "[DONE]" {
		return
	}

	parsed := gjson.Parse(data)
	switch parsed.Get("type").String() {
	case "message_start":
		msgUsage := parsed.Get("message.usage")
		if msgUsage.Exists() {
			usage.InputTokens = int(msgUsage.Get("input_tokens").Int())
			usage.CacheCreationInputTokens = int(msgUsage.Get("cache_creation_input_tokens").Int())
			usage.CacheReadInputTokens = int(msgUsage.Get("cache_read_input_tokens").Int())

			// 保持与通用解析一致：message_start 允许覆盖 5m/1h 明细（包括 0）。
			cc5m := msgUsage.Get("cache_creation.ephemeral_5m_input_tokens")
			cc1h := msgUsage.Get("cache_creation.ephemeral_1h_input_tokens")
			if cc5m.Exists() || cc1h.Exists() {
				usage.CacheCreation5mTokens = int(cc5m.Int())
				usage.CacheCreation1hTokens = int(cc1h.Int())
			}
		}
	case "message_delta":
		deltaUsage := parsed.Get("usage")
		if deltaUsage.Exists() {
			if v := deltaUsage.Get("input_tokens").Int(); v > 0 {
				usage.InputTokens = int(v)
			}
			if v := deltaUsage.Get("output_tokens").Int(); v > 0 {
				usage.OutputTokens = int(v)
			}
			if v := deltaUsage.Get("cache_creation_input_tokens").Int(); v > 0 {
				usage.CacheCreationInputTokens = int(v)
			}
			if v := deltaUsage.Get("cache_read_input_tokens").Int(); v > 0 {
				usage.CacheReadInputTokens = int(v)
			}

			cc5m := deltaUsage.Get("cache_creation.ephemeral_5m_input_tokens")
			cc1h := deltaUsage.Get("cache_creation.ephemeral_1h_input_tokens")
			if cc5m.Exists() {
				usage.CacheCreation5mTokens = int(cc5m.Int())
			}
			if cc1h.Exists() {
				usage.CacheCreation1hTokens = int(cc1h.Int())
			}
		}
	}

	if usage.CacheReadInputTokens == 0 {
		if cached := parsed.Get("message.usage.cached_tokens").Int(); cached > 0 {
			usage.CacheReadInputTokens = int(cached)
		}
		if cached := parsed.Get("usage.cached_tokens").Int(); usage.CacheReadInputTokens == 0 && cached > 0 {
			usage.CacheReadInputTokens = int(cached)
		}
	}
	if usage.CacheCreationInputTokens == 0 {
		cc5m := parsed.Get("message.usage.cache_creation.ephemeral_5m_input_tokens").Int()
		cc1h := parsed.Get("message.usage.cache_creation.ephemeral_1h_input_tokens").Int()
		if cc5m == 0 && cc1h == 0 {
			cc5m = parsed.Get("usage.cache_creation.ephemeral_5m_input_tokens").Int()
			cc1h = parsed.Get("usage.cache_creation.ephemeral_1h_input_tokens").Int()
		}
		total := cc5m + cc1h
		if total > 0 {
			usage.CacheCreationInputTokens = int(total)
		}
	}

	// Kimi's Anthropic-compatible stream uses input_tokens with two meanings:
	// message_start reports total prompt input, while message_delta reports only
	// uncached input. prompt_tokens remains the total in both events. Normalize
	// to ClaudeUsage's mutually-exclusive buckets so downstream billing does not
	// subtract cache tokens from an already-uncached value.
	usageNode := parsed.Get("usage")
	if parsed.Get("type").String() == "message_start" {
		usageNode = parsed.Get("message.usage")
	}
	normalizeAnthropicCompatiblePromptUsage(usageNode, usage)
}

// normalizeAnthropicCompatiblePromptUsage converts provider-native OpenAI-style
// prompt/cache fields into Claude's mutually-exclusive usage buckets. Native
// Anthropic responses do not expose these aliases and are left alone.
func normalizeAnthropicCompatiblePromptUsage(usageNode gjson.Result, usage *ClaudeUsage) bool {
	if usage == nil || !usageNode.Exists() {
		return false
	}
	promptTokens := usageNode.Get("prompt_tokens")
	promptCacheHitTokens := usageNode.Get("prompt_cache_hit_tokens")
	promptCacheMissTokens := usageNode.Get("prompt_cache_miss_tokens")
	if (!promptTokens.Exists() || promptTokens.Int() <= 0) &&
		!promptCacheHitTokens.Exists() && !promptCacheMissTokens.Exists() {
		return false
	}

	cacheReadTokens := usage.CacheReadInputTokens
	if v := usageNode.Get("cache_read_input_tokens"); v.Exists() {
		cacheReadTokens = int(v.Int())
	}
	if cacheReadTokens == 0 {
		if v := usageNode.Get("cached_tokens"); v.Exists() {
			cacheReadTokens = int(v.Int())
		}
	}
	if cacheReadTokens == 0 {
		if v := usageNode.Get("prompt_tokens_details.cached_tokens"); v.Exists() {
			cacheReadTokens = int(v.Int())
		}
	}
	if cacheReadTokens == 0 && promptCacheHitTokens.Exists() {
		cacheReadTokens = max(int(promptCacheHitTokens.Int()), 0)
	}

	cacheCreationTokens := usage.CacheCreationInputTokens
	if v := usageNode.Get("cache_creation_input_tokens"); v.Exists() {
		cacheCreationTokens = int(v.Int())
	}
	if cacheCreationTokens == 0 {
		cc5m := usageNode.Get("cache_creation.ephemeral_5m_input_tokens").Int()
		cc1h := usageNode.Get("cache_creation.ephemeral_1h_input_tokens").Int()
		if cc5m > 0 || cc1h > 0 {
			cacheCreationTokens = int(cc5m + cc1h)
		}
	}

	if promptCacheMissTokens.Exists() {
		usage.InputTokens = max(int(promptCacheMissTokens.Int()), 0)
	} else {
		usage.InputTokens = max(int(promptTokens.Int())-cacheReadTokens-cacheCreationTokens, 0)
	}
	usage.CacheReadInputTokens = cacheReadTokens
	usage.CacheCreationInputTokens = cacheCreationTokens
	return true
}

func parseClaudeUsageFromResponseBody(body []byte) *ClaudeUsage {
	usage := &ClaudeUsage{}
	if len(body) == 0 {
		return usage
	}

	parsed := gjson.ParseBytes(body)
	usageNode := parsed.Get("usage")
	if !usageNode.Exists() {
		return usage
	}

	usage.InputTokens = int(usageNode.Get("input_tokens").Int())
	usage.OutputTokens = int(usageNode.Get("output_tokens").Int())
	usage.CacheCreationInputTokens = int(usageNode.Get("cache_creation_input_tokens").Int())
	usage.CacheReadInputTokens = int(usageNode.Get("cache_read_input_tokens").Int())

	cc5m := usageNode.Get("cache_creation.ephemeral_5m_input_tokens").Int()
	cc1h := usageNode.Get("cache_creation.ephemeral_1h_input_tokens").Int()
	if cc5m > 0 || cc1h > 0 {
		usage.CacheCreation5mTokens = int(cc5m)
		usage.CacheCreation1hTokens = int(cc1h)
	}
	if usage.CacheCreationInputTokens == 0 && (cc5m > 0 || cc1h > 0) {
		usage.CacheCreationInputTokens = int(cc5m + cc1h)
	}
	if usage.CacheReadInputTokens == 0 {
		if cached := usageNode.Get("cached_tokens").Int(); cached > 0 {
			usage.CacheReadInputTokens = int(cached)
		}
	}
	normalizeAnthropicCompatiblePromptUsage(usageNode, usage)
	return usage
}

// invalidNonStreamingJSONFailoverError 把"上游 2xx 返回非 JSON body"归一为
// failover 错误（包级函数：Anthropic 平台 passthrough 与国产供应商原生
// Anthropic 直通共用）。
func invalidNonStreamingJSONFailoverError(
	ctx context.Context,
	rateLimitService *RateLimitService,
	resp *http.Response,
	account *Account,
	body []byte,
	parseErr error,
	requestedModel ...string,
) error {
	const statusCode = http.StatusBadGateway

	accountID := int64(0)
	accountName := ""
	retryableOnSameAccount := false
	if account != nil {
		accountID = account.ID
		accountName = account.Name
		retryableOnSameAccount = account.IsPoolMode() && account.IsPoolModeRetryableStatus(statusCode)
	}

	logger.LegacyPrintf(
		"service.gateway",
		"Account %d(%s): upstream returned non-JSON 2xx response, attempting failover: status=%d request_id=%s error=%v",
		accountID,
		accountName,
		resp.StatusCode,
		resp.Header.Get("x-request-id"),
		parseErr,
	)

	if rateLimitService != nil && account != nil {
		if len(requestedModel) > 0 {
			rateLimitService.HandleUpstreamError(ctx, account, statusCode, resp.Header, body, requestedModel[0])
		} else {
			rateLimitService.HandleUpstreamError(ctx, account, statusCode, resp.Header, body)
		}
	}

	return &UpstreamFailoverError{
		StatusCode:             statusCode,
		ResponseBody:           body,
		ResponseHeaders:        resp.Header,
		RetryableOnSameAccount: retryableOnSameAccount,
	}
}

func (s *GatewayService) handleNonStreamingResponseAnthropicAPIKeyPassthrough(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
) (*ClaudeUsage, error) {
	if s.rateLimitService != nil {
		s.rateLimitService.UpdateSessionWindow(ctx, account, resp.Header)
	}

	body, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, anthropicTooLargeError)
	if err != nil {
		return nil, err
	}
	observer := upstreamResponseModelObserverFromContext(c)
	if observer == nil {
		observer = beginUpstreamResponseModelObservation(c)
	}
	observer.ObserveAnthropic(body)

	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		var raw json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, invalidNonStreamingJSONFailoverError(ctx, s.rateLimitService, resp, account, body, err)
		}
	}

	usage := parseClaudeUsageFromResponseBody(body)
	// 非流式同样要判「上游空完成」：200 + 合法的空 content 数组同样是客户拿不到任何回答，
	// 与流式走同一份判据和同一处置（见 upstream_completion_audit.go）。
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		audit := &upstreamCompletionAudit{}
		audit.observeAnthropicBody(body)
		if audit.isEmptyCompletion(usage) {
			return nil, s.noteEmptyUpstreamCompletion(ctx, account, "", usage, audit)
		}
	}
	if IsForceCacheBilling(ctx) && usage.InputTokens > 0 {
		body, err = classifyAnthropicResponseInputAsCacheRead(body, usage)
		if err != nil {
			return nil, err
		}
	}

	// 水位提醒按"流结尾追加"语义追加到 2xx 响应 content 数组尾部；必须早于
	// reverseToolNamesIfPresent。含 tool_use 的回合直接跳过（不判定、不推进水位）；
	// append 失败记一条日志并保留原 body（水位已推进属可接受残余）。
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		hasToolUse := false
		for _, block := range gjson.GetBytes(body, "content").Array() {
			if block.Get("type").String() == "tool_use" {
				hasToolUse = true
				break
			}
		}
		if !hasToolUse {
			d := DecideContextWarning(ctx, RequestSizeSessionHash(ctx), account.ID,
				usage.InputTokens+usage.CacheReadInputTokens+usage.CacheCreationInputTokens)
			if d.Text != "" {
				if next, err := AppendContextWarningToJSON(body, d.Text); err == nil {
					body = next
				} else {
					logger.LegacyPrintf("service.gateway", "[Anthropic passthrough] Append context warning to JSON failed, keep original body: account=%d error=%v", account.ID, err)
				}
			}
		}
	}

	writeAnthropicPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "application/json"
	}
	body = reverseToolNamesIfPresent(c, body)
	c.Data(resp.StatusCode, contentType, body)
	return usage, nil
}

func classifyAnthropicResponseInputAsCacheRead(body []byte, usage *ClaudeUsage) ([]byte, error) {
	classified, err := sjson.SetBytes(body, "usage.input_tokens", 0)
	if err != nil {
		return nil, fmt.Errorf("classify forced cache billing input tokens: %w", err)
	}
	classified, err = sjson.SetBytes(classified, "usage.cache_read_input_tokens", usage.CacheReadInputTokens+usage.InputTokens)
	if err != nil {
		return nil, fmt.Errorf("classify forced cache billing cache read tokens: %w", err)
	}
	return classified, nil
}

func writeAnthropicPassthroughResponseHeaders(dst http.Header, src http.Header, filter *responseheaders.CompiledHeaderFilter) {
	if dst == nil || src == nil {
		return
	}
	if filter != nil {
		responseheaders.WriteFilteredHeaders(dst, src, filter)
		return
	}
	if v := strings.TrimSpace(src.Get("Content-Type")); v != "" {
		dst.Set("Content-Type", v)
	}
	if v := strings.TrimSpace(src.Get("x-request-id")); v != "" {
		dst.Set("x-request-id", v)
	}
}
