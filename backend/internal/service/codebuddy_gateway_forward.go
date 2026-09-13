package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// codeBuddyChatCompletionsPath 是 CodeBuddy 的 Chat Completions 端点（§2.1）。
const codeBuddyChatCompletionsPath = "/v2/chat/completions"

// codeBuddyBalanceCooldownMinutes CodeBuddy 余额耗尽临时停调时长（分钟），对齐 CN 供应商的 2× 检测周期默认。
const codeBuddyBalanceCooldownMinutes = 20

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

	upstreamModel := account.GetMappedModel(originalModel)
	if strings.TrimSpace(upstreamModel) == "" {
		upstreamModel = originalModel
	}

	opts := CodeBuddyRewriteOptions{
		Sanitize:         s.codeBuddySanitizeEnabled(),
		Model:            upstreamModel,
		SupportedEfforts: codeBuddyResolveSupportedEfforts(ctx, account, upstreamModel),
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

	token, _, err := s.getRequestCredential(ctx, c, account)
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
		upstreamReq, buildErr := s.buildCodeBuddyChatRequest(upstreamCtx, c, account, sendBody, token, upstreamModel)
		if buildErr != nil {
			return nil, buildErr
		}
		resp, respErr = s.doOpenAIUpstream(upstreamReq, proxyURL, account)
		SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(startTime).Milliseconds())
		if respErr != nil {
			return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, respErr, false)
		}
		captureCodeBuddyUpstreamResponse(upstreamCtx, upstreamReq, sendBody, resp)
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
	nonStreamResult, err := s.handleNonStreamingResponse(ctx, resp, c, account, originalModel, upstreamModel)
	if err != nil {
		return nil, err
	}
	return codeBuddyForwardResultFromNonStreaming(nonStreamResult, originalModel, upstreamModel, startTime, resp.Header), nil
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
	targetURL := strings.TrimRight(CodeBuddyUpstreamBaseURL, "/") + codeBuddyChatCompletionsPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))

	ua := s.codeBuddyChatUserAgent()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", CodeBuddyOriginReferer)
	req.Header.Set("Referer", CodeBuddyOriginReferer+"/")
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Product", "SaaS")

	// §2.4 chat 追加头：空值用 X-No-*: 1 占位（避免上游对空 header 的歧义处理）。
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
	captureCodeBuddyOutboundRequest(req, body)
	return req, nil
}

// captureCodeBuddyOutboundRequest 是临时插桩（活体验收抓包定位专用，不入正式代码）：
// 打印实际出站请求的最终头与体，Authorization 仅打长度。
func captureCodeBuddyOutboundRequest(req *http.Request, body []byte) {
	if req == nil {
		return
	}
	hdr := make(map[string]string, len(req.Header))
	for k, v := range req.Header {
		joined := strings.Join(v, ",")
		if strings.EqualFold(k, "Authorization") {
			joined = fmt.Sprintf("<bearer len=%d>", len(strings.TrimPrefix(joined, "Bearer ")))
		}
		hdr[k] = joined
	}
	b := string(body)
	if len(b) > 800 {
		b = b[:800] + "...(truncated)"
	}
	slog.Warn("cb_capture_outbound", "method", req.Method, "url", req.URL.String(), "headers", hdr, "body", b)
}

// captureCodeBuddyUpstreamResponse 是临时插桩（同批，不入正式代码）：打印上游响应头，
// 并在 403 时用全新默认传输（http.DefaultClient）重放同一请求一次，以隔离传输层差异。
func captureCodeBuddyUpstreamResponse(ctx context.Context, req *http.Request, body []byte, resp *http.Response) {
	if resp == nil {
		return
	}
	hdr := make(map[string]string, len(resp.Header))
	for k, v := range resp.Header {
		hdr[k] = strings.Join(v, ",")
	}
	slog.Warn("cb_capture_response", "status", resp.StatusCode, "proto", resp.Proto, "headers", hdr)
	if resp.StatusCode != http.StatusForbidden || req == nil {
		return
	}
	diagReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL.String(), bytes.NewReader(body))
	if err != nil {
		slog.Warn("cb_capture_plain_retry_err", "error", err.Error())
		return
	}
	diagReq.Header = req.Header.Clone()
	diagResp, err := http.DefaultClient.Do(diagReq) //nolint:gosec // 临时诊断用固定上游 URL
	if err != nil {
		slog.Warn("cb_capture_plain_retry_err", "error", err.Error())
		return
	}
	defer func() { _ = diagResp.Body.Close() }()
	db, _ := io.ReadAll(io.LimitReader(diagResp.Body, 2048))
	slog.Warn("cb_capture_plain_retry_result", "status", diagResp.StatusCode, "proto", diagResp.Proto, "body", string(db))
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

// handleCodeBuddyUpstreamError 对 CodeBuddy 上游错误分类并施加账号健康副作用，
// 最后把上游错误原样回传给客户端（由 handleErrorResponse 写 gin 上下文）。
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
	upstreamMsg := sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(respBody))
	if upstreamMsg == "" {
		upstreamMsg = fmt.Sprintf("codebuddy upstream returned status %d", resp.StatusCode)
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: resp.StatusCode,
		UpstreamRequestID:  firstNonEmpty(resp.Header.Get("x-request-id"), resp.Header.Get("trace-id")),
		Kind:               "http_error",
		Message:            upstreamMsg,
	})

	switch kind {
	case CodeBuddyErrKindBalanceExhausted:
		s.rateLimitService.handleCodeBuddyInsufficientBalance(ctx, account, upstreamMsg)
	case CodeBuddyErrKindSessionDead:
		s.rateLimitService.handleCodeBuddySessionDead(ctx, account, upstreamMsg)
	case CodeBuddyErrKindModelLimit:
		if until, ok := parseCodeBuddyResetTime(respBody); ok {
			s.rateLimitService.handleCodeBuddyModelLimit(ctx, account, upstreamModel, until)
		} else {
			// 无法解析重置时间时退化为账号级短冷却（不标记 error）。
			s.rateLimitService.handle429(ctx, account, resp.Header, respBody)
		}
	case CodeBuddyErrKindAccountSoftLimit:
		s.rateLimitService.handle429(ctx, account, resp.Header, respBody)
	case CodeBuddyErrKindContentAudit, CodeBuddyErrKindRequestBody:
		// 不罚账号：审核拦截已重试过；请求体问题仍由调度轮转处理。
	case CodeBuddyErrKindUpstreamFault:
		// 5xx/404：上游故障，常规重试（不标记账号状态）。
	}

	return s.handleErrorResponse(ctx, resp, c, account, respBody, upstreamModel)
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
