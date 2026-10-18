package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/tidwall/gjson"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// 重试相关常量
const (
	// 最大尝试次数（包含首次请求）。过多重试会导致请求堆积与资源耗尽。
	maxRetryAttempts = 5

	// 指数退避：第 N 次失败后的等待 = retryBaseDelay * 2^(N-1)，并且上限为 retryMaxDelay。
	retryBaseDelay = 300 * time.Millisecond
	retryMaxDelay  = 3 * time.Second

	// 最大重试耗时（包含请求本身耗时 + 退避等待时间）。
	// 用于防止极端情况下 goroutine 长时间堆积导致资源耗尽。
	maxRetryElapsed = 10 * time.Second
)

func (s *GatewayService) shouldRetryUpstreamError(account *Account, statusCode int) bool {
	// OAuth/Setup Token 账号：仅 403 重试
	if account.IsOAuth() {
		return statusCode == 403
	}

	// API Key 账号：未配置的错误码重试
	return !account.ShouldHandleErrorCode(statusCode)
}

// shouldFailoverUpstreamError determines whether an upstream error should trigger account failover.
// 402 与 OpenAI 网关口径一致：上游"余额/额度耗尽"即该账号当前不可服务，应切换而非透传。
func (s *GatewayService) shouldFailoverUpstreamError(statusCode int) bool {
	switch statusCode {
	case 401, 402, 403, 405, 429, 529:
		return true
	default:
		return statusCode >= 500
	}
}

func retryBackoffDelay(attempt int) time.Duration {
	// attempt 从 1 开始，表示第 attempt 次请求刚失败，需要等待后进行第 attempt+1 次请求。
	if attempt <= 0 {
		return retryBaseDelay
	}
	delay := retryBaseDelay * time.Duration(1<<(attempt-1))
	if delay > retryMaxDelay {
		return retryMaxDelay
	}
	return delay
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ---------------------------------------------------------------------------
// Cursor 文本桥接（P1-C1）：Claude 组三出口的早分流
// ---------------------------------------------------------------------------
//
// 固定语义（方案 §6.1 / §15.4-I4 / §16.1）：
//   - cursor 账号在 GatewayService 三个 Claude 出口的**最前面**分流，绝不落到
//     Anthropic 通用路径；
//   - 入站先由既有 apicompat 转换器归一为 Chat Completions，再走契约 0.1 的
//     包级入口 CursorForwardInbound("openai_chat")；
//   - 桥接返回的 *ForwardResult 沿既有 usage 字段承载真实用量，由 handler 既有
//     RecordUsage 链计费一次；本层**不计费、不二次记 usage**；
//   - 客户端可见响应必须回到入站协议：Messages → Anthropic SSE/JSON，
//     Responses → Responses 事件/JSON，Chat Completions 直出。
//
// 桥接实现（P1-A2）会把 OpenAI 形状的响应直接写到 gin writer。为了让
// Messages / Responses 两个出口保持各自协议，这里用一个**逐帧转换 writer**
// 包住真实 writer：流式帧在写入的同时转换为目标协议，客户端仍实时收到
// 增量（不整段缓冲）；非流式响应在结束时整包转换。

// cursorBridgeMode 选择客户端可见响应协议，与入站出口一一对应。
type cursorBridgeMode int

const (
	cursorBridgeModeChat cursorBridgeMode = iota
	cursorBridgeModeAnthropic
	cursorBridgeModeResponses
)

// cursorBridgeProtocolWriter 把桥接写出的 OpenAI 形状响应回桥为客户端协议。
//
// 只拦截 gin.ResponseWriter 的写入方法；其余方法透传。Header 使用独立 map，
// 避免桥接内部的 Content-Type / SSE 头直接落到真实连接（最终由本 writer 设置）。
type cursorBridgeProtocolWriter struct {
	gin.ResponseWriter
	header http.Header
	mode   cursorBridgeMode
	model  string
	stream bool

	// 流式：未消费的 SSE 片段 + 目标协议状态机。
	pending   []byte
	anthState *apicompat.ChatCompletionsToAnthropicStreamState
	respState *apicompat.ChatCompletionsToResponsesStreamState
	started   bool

	// 非流式：整条 chat.completion JSON。
	completion bytes.Buffer

	written     int
	status      int
	wroteHeader bool
}

func newCursorBridgeProtocolWriter(parent gin.ResponseWriter, stream bool, mode cursorBridgeMode, model string) *cursorBridgeProtocolWriter {
	w := &cursorBridgeProtocolWriter{
		ResponseWriter: parent,
		header:         make(http.Header),
		mode:           mode,
		model:          model,
		stream:         stream,
	}
	switch mode {
	case cursorBridgeModeAnthropic:
		w.anthState = apicompat.NewChatCompletionsToAnthropicStreamState(model)
	case cursorBridgeModeResponses:
		w.respState = apicompat.NewChatCompletionsToResponsesStreamState(model)
	}
	return w
}

func (w *cursorBridgeProtocolWriter) Header() http.Header { return w.header }

func (w *cursorBridgeProtocolWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.status = code
	w.wroteHeader = true
}

func (w *cursorBridgeProtocolWriter) WriteHeaderNow() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
}

func (w *cursorBridgeProtocolWriter) Write(p []byte) (int, error) {
	w.WriteHeaderNow()
	w.written += len(p)
	if !w.stream {
		return w.completion.Write(p)
	}
	w.pending = append(w.pending, p...)
	return len(p), w.flushFrames()
}

func (w *cursorBridgeProtocolWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

func (w *cursorBridgeProtocolWriter) Flush() {
	if w.stream {
		_ = w.flushFrames()
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *cursorBridgeProtocolWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (w *cursorBridgeProtocolWriter) Size() int { return w.written }

func (w *cursorBridgeProtocolWriter) Written() bool { return w.wroteHeader || w.written > 0 }

// ensureStarted 在首次写客户端前设置目标协议的 SSE 头。
func (w *cursorBridgeProtocolWriter) ensureStarted() {
	if w.started {
		return
	}
	w.started = true
	h := w.ResponseWriter.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	if rid := strings.TrimSpace(w.header.Get("x-request-id")); rid != "" {
		h.Set("x-request-id", rid)
	}
}

// flushFrames 消费 pending 中的完整 SSE 帧（空行分隔），逐帧回桥。
func (w *cursorBridgeProtocolWriter) flushFrames() error {
	for {
		idx, sepLen := -1, 0
		if i := bytes.Index(w.pending, []byte("\n\n")); i >= 0 {
			idx, sepLen = i, 2
		} else if i := bytes.Index(w.pending, []byte("\r\n\r\n")); i >= 0 {
			idx, sepLen = i, 4
		}
		if idx < 0 {
			return nil
		}
		frame := w.pending[:idx]
		w.pending = w.pending[idx+sepLen:]
		if err := w.writeSSEFrame(frame); err != nil {
			return err
		}
	}
}

// writeSSEFrame 把一帧 OpenAI SSE 转换为目标协议事件并立即写给客户端。
func (w *cursorBridgeProtocolWriter) writeSSEFrame(frame []byte) error {
	data := cursorBridgeSSEData(frame)
	if data == "" {
		// 上游 keepalive / 空注释帧：已开始的目标协议流透传为空注释帧。
		if w.started {
			_, err := io.WriteString(w.ResponseWriter, ":\n\n")
			return err
		}
		return nil
	}
	if data == "[DONE]" {
		return nil
	}

	var chunk apicompat.ChatCompletionsChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return fmt.Errorf("cursor: decode bridged chat chunk: %w", err)
	}
	chunk.Usage = cursorBridgedChatUsage([]byte(data))
	return w.writeChunk(&chunk)
}

func (w *cursorBridgeProtocolWriter) writeChunk(chunk *apicompat.ChatCompletionsChunk) error {
	w.ensureStarted()
	switch w.mode {
	case cursorBridgeModeAnthropic:
		for _, event := range apicompat.ChatCompletionsChunkToAnthropicEvents(chunk, w.anthState) {
			sse, err := apicompat.ResponsesAnthropicEventToSSE(event)
			if err != nil {
				return err
			}
			if _, err := io.WriteString(w.ResponseWriter, sse); err != nil {
				return err
			}
		}
	case cursorBridgeModeResponses:
		for _, event := range apicompat.ChatCompletionsChunkToResponsesEvents(chunk, w.respState) {
			sse, err := apicompat.ResponsesEventToSSE(event)
			if err != nil {
				return err
			}
			if _, err := io.WriteString(w.ResponseWriter, sse); err != nil {
				return err
			}
		}
	}
	return nil
}

// finalize 写出目标协议的终止事件（流式）或整包响应（非流式）。
func (w *cursorBridgeProtocolWriter) finalize() error {
	if !w.stream {
		return w.finalizeCompletion()
	}

	if len(w.pending) > 0 {
		rest := w.pending
		w.pending = nil
		if err := w.writeSSEFrame(rest); err != nil {
			return err
		}
	}
	w.ensureStarted()

	var events []string
	switch w.mode {
	case cursorBridgeModeAnthropic:
		for _, event := range apicompat.FinalizeChatCompletionsAnthropicStream(w.anthState) {
			sse, err := apicompat.ResponsesAnthropicEventToSSE(event)
			if err != nil {
				return err
			}
			events = append(events, sse)
		}
	case cursorBridgeModeResponses:
		for _, event := range apicompat.FinalizeChatCompletionsResponsesStream(w.respState) {
			sse, err := apicompat.ResponsesEventToSSE(event)
			if err != nil {
				return err
			}
			events = append(events, sse)
		}
	}
	for _, sse := range events {
		if _, err := io.WriteString(w.ResponseWriter, sse); err != nil {
			return err
		}
	}
	w.Flush()
	return nil
}

func (w *cursorBridgeProtocolWriter) finalizeCompletion() error {
	body := w.completion.Bytes()
	var resp apicompat.ChatCompletionsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("cursor: decode bridged chat completion: %w", err)
	}
	resp.Usage = cursorBridgedChatUsage(body)

	var out any
	switch w.mode {
	case cursorBridgeModeAnthropic:
		out = apicompat.ChatCompletionsResponseToAnthropic(&resp, w.model)
	case cursorBridgeModeResponses:
		out = apicompat.ChatCompletionsResponseToResponses(&resp, w.model, nil, nil, false, nil)
	default:
		return fmt.Errorf("cursor: unsupported non-stream response mode %d", w.mode)
	}
	data, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("cursor: marshal %d protocol response: %w", w.mode, err)
	}

	h := w.ResponseWriter.Header()
	h.Set("Content-Type", "application/json")
	if rid := strings.TrimSpace(w.header.Get("x-request-id")); rid != "" {
		h.Set("x-request-id", rid)
	}
	w.ResponseWriter.WriteHeader(http.StatusOK)
	_, err = w.ResponseWriter.Write(data)
	return err
}

// cursorBridgeSSEData 从一帧 SSE 中取出 data 字段（多行按 SSE 规范拼接）。
func cursorBridgeSSEData(frame []byte) string {
	var b strings.Builder
	for _, rawLine := range bytes.Split(frame, []byte("\n")) {
		line := strings.TrimSpace(string(rawLine))
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
	}
	return b.String()
}

// forwardCursorAsAnthropicMessages 处理 /v1/messages 入站：Anthropic Messages
// → Chat Completions → 契约 0.1 文本桥接 → Anthropic 响应。
func (s *GatewayService) forwardCursorAsAnthropicMessages(
	ctx context.Context, c *gin.Context, account *Account, parsed *ParsedRequest,
) (*ForwardResult, error) {
	var req apicompat.AnthropicRequest
	if err := json.Unmarshal(parsed.Body.Bytes(), &req); err != nil {
		return nil, fmt.Errorf("cursor: parse anthropic messages request: %w", err)
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, errors.New("cursor: anthropic messages request carries no model (fail closed)")
	}
	ccReq, err := apicompat.AnthropicToChatCompletionsRequest(&req)
	if err != nil {
		return nil, fmt.Errorf("cursor: convert anthropic messages to chat completions: %w", err)
	}
	ccBody, err := json.Marshal(ccReq)
	if err != nil {
		return nil, fmt.Errorf("cursor: marshal chat completions request: %w", err)
	}
	return s.forwardCursorAsProtocol(ctx, c, account, ccBody, req.Stream, cursorBridgeModeAnthropic, req.Model)
}

// forwardCursorAsResponses 处理 /v1/responses 入站：Responses → Chat Completions
// → 契约 0.1 文本桥接 → Responses 响应。
func (s *GatewayService) forwardCursorAsResponses(
	ctx context.Context, c *gin.Context, account *Account, body []byte,
) (*ForwardResult, error) {
	var req apicompat.ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("cursor: parse responses request: %w", err)
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, errors.New("cursor: responses request carries no model (fail closed)")
	}
	ccReq, err := apicompat.ResponsesToChatCompletionsRequest(&req)
	if err != nil {
		return nil, fmt.Errorf("cursor: convert responses to chat completions: %w", err)
	}
	ccBody, err := json.Marshal(ccReq)
	if err != nil {
		return nil, fmt.Errorf("cursor: marshal chat completions request: %w", err)
	}
	return s.forwardCursorAsProtocol(ctx, c, account, ccBody, req.Stream, cursorBridgeModeResponses, req.Model)
}

// forwardCursorAsProtocol 调用契约 0.1 桥接，并按 mode 把桥接输出实时回写为
// 入站协议。
//
// 返回的 *ForwardResult 直接来自桥接（真实 usage）；本函数只做协议形状搬运，
// 不写 usage、不计费。
func (s *GatewayService) forwardCursorAsProtocol(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	ccBody []byte,
	stream bool,
	mode cursorBridgeMode,
	clientModel string,
) (*ForwardResult, error) {
	if mode == cursorBridgeModeChat || c == nil {
		return CursorForwardInbound(ctx, c, account, cursorInboundOpenAIChat, ccBody)
	}

	writer := newCursorBridgeProtocolWriter(c.Writer, stream, mode, clientModel)
	originalWriter := c.Writer
	c.Writer = writer
	res, err := CursorForwardInbound(ctx, c, account, cursorInboundOpenAIChat, ccBody)
	c.Writer = originalWriter
	if err != nil {
		return nil, err
	}
	if writer.Status() != http.StatusOK {
		return nil, fmt.Errorf("cursor: text bridge returned status %d without error (fail closed)", writer.Status())
	}
	if err := writer.finalize(); err != nil {
		return nil, err
	}
	return res, nil
}

// cursorBridgedChatUsage 把桥接输出的 usage 归一为 apicompat.ChatUsage。
//
// Cursor 产出的键名是 input_tokens / output_tokens / cache_*_input_tokens
// （与网关内部 OpenAIUsage 口径一致）；apicompat 回桥按 OpenAI 官方的
// prompt/completion + prompt_tokens_details 语义读取。这里做一次无损搬运：
// prompt_tokens 取 input + cache_read + cache_creation，使回桥后的
// Anthropic input_tokens / Responses input_tokens 与桥接 ForwardResult 完全一致。
func cursorBridgedChatUsage(raw []byte) *apicompat.ChatUsage {
	usage := gjson.GetBytes(raw, "usage")
	if !usage.Exists() || !usage.IsObject() {
		return nil
	}

	cacheRead := int(usage.Get("cache_read_input_tokens").Int())
	cacheCreation := int(usage.Get("cache_creation_input_tokens").Int())
	if details := usage.Get("prompt_tokens_details"); details.Exists() {
		if v := details.Get("cached_tokens"); v.Exists() {
			cacheRead = int(v.Int())
		}
		if v := details.Get("cache_creation_tokens"); v.Exists() && cacheCreation == 0 {
			cacheCreation = int(v.Int())
		}
		if v := details.Get("cache_write_tokens"); v.Exists() && cacheCreation == 0 {
			cacheCreation = int(v.Int())
		}
	}

	prompt := int(usage.Get("prompt_tokens").Int())
	if prompt == 0 {
		prompt = int(usage.Get("input_tokens").Int()) + cacheRead + cacheCreation
	}
	completion := int(usage.Get("completion_tokens").Int())
	if completion == 0 {
		completion = int(usage.Get("output_tokens").Int())
	}
	total := int(usage.Get("total_tokens").Int())
	if total == 0 {
		total = prompt + completion
	}

	return &apicompat.ChatUsage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      total,
		PromptTokensDetails: &apicompat.ChatTokenDetails{
			CachedTokens:        cacheRead,
			CacheCreationTokens: cacheCreation,
		},
	}
}

// Forward 转发请求到Claude API
func (s *GatewayService) Forward(ctx context.Context, c *gin.Context, account *Account, parsed *ParsedRequest) (result *ForwardResult, err error) {
	startTime := time.Now()
	if parsed == nil {
		return nil, fmt.Errorf("parse request: empty request")
	}
	// Cursor 账号前置分流：出站为 Cursor 原生协议，绝不能落到 Anthropic 通用路径。
	if isCursorAccount(account) {
		return s.forwardCursorAsAnthropicMessages(ctx, c, account, parsed)
	}
	// API-key mappings and OAuth native IDs are resolved before mimicry.
	validationModel := parsed.Model
	if account != nil {
		if account.IsBedrock() {
			if resolved, ok := ResolveBedrockModelID(account, validationModel); ok {
				validationModel = resolved
			}
		} else if account.Type == AccountTypeAPIKey {
			validationModel = account.GetMappedModel(validationModel)
		}
	}
	if account != nil && account.Platform == PlatformAnthropic {
		if err := validateClaude55Request(parsed.Body.Bytes(), validationModel); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"type": "error", "error": gin.H{"type": "invalid_request_error", "message": err.Error()}})
			return nil, err
		}
	}
	// G5 观测（单点埋点）：请求出口补发整条换号链未解析的 guard 终态关联事件（同
	// request_id）。无 tracker（未命中四挂点/OpenAI 原生路径）时为 no-op。
	defer func() { cf524ResolveGuardOutcome(c, ctx, err) }()
	// Anthropic Fast is requested with speed=fast rather than OpenAI's
	// service_tier. Attach it at this shared boundary so passthrough, OAuth and
	// partial-stream results all use the same billing and usage-log path.
	defer func() {
		if result != nil {
			if tier := anthropicSpeedServiceTier(account, parsed.Speed, anthropicSpeedModel(parsed, result)); tier != nil {
				result.ServiceTier = tier
			}
		}
	}()
	beginUpstreamResponseModelObservation(c)

	// Web Search 模拟：纯 web_search 请求时，直接调用搜索 API 构造响应
	if account != nil && s.shouldEmulateWebSearch(ctx, account, parsed.GroupID, parsed.Body.Bytes()) {
		return s.handleWebSearchEmulation(ctx, c, account, parsed)
	}

	if account != nil && account.IsAnthropicAPIKeyPassthroughEnabled() {
		passthroughBody := parsed.Body.Bytes()
		passthroughModel := parsed.Model
		if passthroughModel != "" {
			if mappedModel := account.GetMappedModel(passthroughModel); mappedModel != passthroughModel {
				passthroughBody = s.replaceModelInBody(passthroughBody, mappedModel)
				logger.LegacyPrintf("service.gateway", "Passthrough model mapping: %s -> %s (account: %s)", parsed.Model, mappedModel, account.Name)
				passthroughModel = mappedModel
			}
		}
		return s.forwardAnthropicAPIKeyPassthroughWithInput(ctx, c, account, anthropicPassthroughForwardInput{
			Body:          passthroughBody,
			Parsed:        parsed,
			RequestModel:  passthroughModel,
			OriginalModel: parsed.Model,
			RequestStream: parsed.Stream,
			StartTime:     startTime,
		})
	}

	if account != nil && account.IsBedrock() {
		return s.forwardBedrock(ctx, c, account, parsed, startTime)
	}

	// Beta policy: evaluate once; block check + cache filter set for buildUpstreamRequest.
	// Always overwrite the cache to prevent stale values from a previous retry with a different account.
	if account.Platform == PlatformAnthropic && c != nil {
		policy := s.evaluateBetaPolicy(ctx, c.GetHeader("anthropic-beta"), account, parsed.Model)
		if policy.blockErr != nil {
			return nil, policy.blockErr
		}
		filterSet := policy.filterSet
		if filterSet == nil {
			filterSet = map[string]struct{}{}
		}
		c.Set(betaPolicyFilterSetKey, filterSet)
	}

	body := parsed.Body.Bytes()
	replaceBody := func(next []byte) error {
		if err := parsed.ReplaceBody(next); err != nil {
			return fmt.Errorf("rewrite request body: %w", err)
		}
		body = parsed.Body.Bytes()
		return nil
	}
	reqModel := parsed.Model
	reqStream := parsed.Stream
	originalModel := reqModel

	// === DEBUG: 打印客户端原始请求（headers + body 摘要）===
	if c != nil {
		s.debugLogGatewaySnapshot("CLIENT_ORIGINAL", c.Request.Header, body, map[string]string{
			"account":      fmt.Sprintf("%d(%s)", account.ID, account.Name),
			"account_type": string(account.Type),
			"model":        reqModel,
			"stream":       strconv.FormatBool(reqStream),
		})
	}

	// Claude Code 客户端判定：UA 匹配 claude-cli/* 且携带 metadata.user_id。
	// 真正的 Claude Code 客户端自带完整的 system prompt、cache_control 断点和 header，
	// 不需要代理做任何 body 级别的 mimicry；强行替换反而会破坏客户端的缓存策略
	// （长 system prompt 被替换为 ~45 tokens 的短 prompt，低于 Anthropic 1024 token
	// 最低缓存门槛，导致系统级缓存失效）。
	//
	// 对于非 Claude Code 的第三方客户端（opencode 等），仍然走完整 mimicry。
	var clientUserAgent string
	if c != nil {
		clientUserAgent = c.GetHeader("User-Agent")
	}
	isClaudeCode := IsClaudeCodeClient(ctx) || isClaudeCodeClient(clientUserAgent, parsed.MetadataUserID)

	// 补充判定：上游 API 网关（如 new-api）转发真实 Claude Code 流量时，
	// UA 会变成 Go-http-client 但 body 保留了完整的 Claude Code 特征
	// （billing attribution block + metadata.user_id）。此时如果仍走 mimicry
	// 重写 system prompt，会破坏 Anthropic prompt cache 的前缀匹配——
	// 导致 messages 级缓存永远 miss、cache_creation 每轮全量重写。
	// 通过检查 body 中的 billing attribution block 来识别被代理的真实 CC 流量。
	if !isClaudeCode && parsed.MetadataUserID != "" {
		isClaudeCode = systemHasBillingAttributionBlock(body)
	}

	shouldMimicClaudeCode := account.IsOAuth() && !isClaudeCode

	if shouldMimicClaudeCode {
		// 与 Parrot 对齐：OAuth 账号无条件重写 system（即使客户端已发了 Claude Code
		// 风格的 system prompt）。原因：第三方工具（opencode 等）会发 "You are Claude
		// Code..." system prompt 但缺少 billing attribution block，导致 Anthropic
		// 检测到"有 CC prompt 但无 billing block"的不一致而判为 third-party。
		// Parrot 的 transform_request 从不检查客户端 system 内容，直接覆盖。
		systemRaw, _ := parsed.SystemValue()
		systemPromptInjectionEnabled, systemPrompt, systemPromptBlocks := s.claudeOAuthSystemPromptInjectionSettings(ctx)
		if systemPromptInjectionEnabled {
			if err := replaceBody(rewriteSystemForNonClaudeCodeWithPromptBlocks(body, systemRaw, systemPrompt, systemPromptBlocks)); err != nil {
				return nil, err
			}
		}

		normalizeOpts := claudeOAuthNormalizeOptions{}
		if s.identityService != nil && c != nil {
			fp, err := s.identityService.GetOrCreateFingerprint(ctx, account.ID, c.Request.Header)
			if err == nil && fp != nil {
				// metadata 透传开启时跳过 metadata 注入
				_, mimicMPT, _ := s.settingService.GetGatewayForwardingSettings(ctx)
				if !mimicMPT {
					if metadataUserID := s.buildOAuthMetadataUserID(parsed, account, fp); metadataUserID != "" {
						normalizeOpts.injectMetadata = true
						normalizeOpts.metadataUserID = metadataUserID
					}
				}
			}
		}

		var normalizedBody []byte
		normalizedBody, reqModel = normalizeClaudeOAuthRequestBody(body, reqModel, normalizeOpts)
		if err := replaceBody(normalizedBody); err != nil {
			return nil, err
		}

		// D/E/F: 可选 messages cache 策略 + 工具名混淆 + tools[-1] 断点
		// 与 forward_as_chat_completions / forward_as_responses 路径对齐，
		// 原生 /v1/messages 路径也走同一套可配置字段级改写。
		if err := replaceBody(s.rewriteMessageCacheControlIfEnabled(ctx, body)); err != nil {
			return nil, err
		}
		if rw := buildToolNameRewriteFromBody(body); rw != nil {
			if err := replaceBody(applyToolNameRewriteToBody(body, rw)); err != nil {
				return nil, err
			}
			if c != nil {
				c.Set(toolNameRewriteKey, rw)
			}
		} else {
			if err := replaceBody(applyToolsLastCacheBreakpoint(body)); err != nil {
				return nil, err
			}
		}
	}

	// 客户端 dateline 归一化：仅对 Anthropic OAuth/SetupToken 账号生效。
	// 抹除 "Today's date is …" 语句里可能被注入的隐写指纹（4 种撇号 × 2 种日期
	// 分隔符），还原为 ASCII 撇号 + "-" 分隔符。运行在 mimicry 分支之外，
	// 保证真实 Claude Code 客户端注入的指纹同样被清洗。
	if next, ok := s.normalizeClientDatelineIfEnabled(ctx, account, body); ok {
		if err := replaceBody(next); err != nil {
			return nil, err
		}
	}

	// 强制执行 cache_control 块数量限制（最多 4 个）
	if err := replaceBody(enforceCacheControlLimit(body)); err != nil {
		return nil, err
	}

	// 应用模型映射：
	// - APIKey 账号：使用账号级别的显式映射（如果配置），否则透传原始模型名
	// - OAuth/SetupToken 账号：使用 Anthropic 标准映射（短ID → 长ID）
	mappedModel := reqModel
	mappingSource := ""
	if account.Type == AccountTypeAPIKey {
		mappedModel = account.GetMappedModel(reqModel)
		if mappedModel != reqModel {
			mappingSource = "account"
		}
	}
	if mappingSource == "" && account.Platform == PlatformAnthropic && account.Type == AccountTypeServiceAccount {
		if candidate, matched := account.ResolveMappedModel(reqModel); matched {
			mappedModel = candidate
			mappingSource = "account"
		} else {
			normalized := normalizeVertexAnthropicModelID(claude.NormalizeModelID(reqModel))
			if normalized != reqModel {
				mappedModel = normalized
				mappingSource = "vertex"
			}
		}
	}
	if mappingSource == "" && account.Platform == PlatformAnthropic && account.Type != AccountTypeAPIKey {
		normalized := claude.NormalizeModelID(reqModel)
		if normalized != reqModel {
			mappedModel = normalized
			mappingSource = "prefix"
		}
	}
	if mappedModel != reqModel {
		// 替换请求体中的模型名
		if err := replaceBody(s.replaceModelInBody(body, mappedModel)); err != nil {
			return nil, err
		}
		reqModel = mappedModel
		parsed.Model = mappedModel
		logger.LegacyPrintf("service.gateway", "Model mapping applied: %s -> %s (account: %s, source=%s)", originalModel, mappedModel, account.Name, mappingSource)
	}

	if s.shouldInjectAnthropicCacheTTL1h(ctx, account) {
		if err := replaceBody(injectAnthropicCacheControlTTL1h(body)); err != nil {
			return nil, err
		}
	}

	// 获取凭证
	token, tokenType, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}

	// 获取代理URL（自定义 base URL 模式下，proxy 通过 buildCustomRelayURL 作为查询参数传递）
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		if !account.IsCustomBaseURLEnabled() || account.GetCustomBaseURL() == "" {
			proxyURL = account.Proxy.URL()
		}
	}

	// 解析 TLS 指纹 profile（同一请求生命周期内不变，避免重试循环中重复解析）
	tlsProfile := s.tlsFPProfileService.ResolveTLSProfile(account)

	// 调试日志：记录即将转发的账号信息
	logger.LegacyPrintf("service.gateway", "[Forward] Using account: ID=%d Name=%s Platform=%s Type=%s TLSFingerprint=%v Proxy=%s",
		account.ID, account.Name, account.Platform, account.Type, tlsProfile, proxyURL)
	// Pre-filter: strip empty text blocks (including nested in tool_result) to prevent upstream 400.
	if err := replaceBody(StripEmptyTextBlocks(body)); err != nil {
		return nil, err
	}
	// Pre-filter: strip web-search history blocks the upstream cannot accept
	// (emulation-synthesized server_tool_use / web_search_tool_result always;
	// genuine ones additionally for passback-required upstreams). See
	// FilterWebSearchHistoryBlocks. reqModel 此时已是映射后的模型 ID。
	if err := replaceBody(FilterWebSearchHistoryBlocks(body, reqModel)); err != nil {
		return nil, err
	}
	// Pre-filter: remove thinking blocks with missing/invalid signatures before forwarding.
	// Clients (e.g. Claude Code) sometimes send multi-turn conversations where a historical
	// assistant message contains a thinking block that is missing the required "signature" field,
	// causing upstream to reject the request with 400 "thinking.signature: Field required".
	// FilterThinkingBlocks removes only the invalid blocks; thinking blocks with valid signatures
	// are preserved. This avoids relying solely on the post-error retry path, which can time out
	// (maxRetryElapsed = 10s) for long conversations before the retry budget is exhausted.
	//
	// 仅 anthropic-strict 模型族执行此过滤；passback-required 上游 (DeepSeek/Kimi/GLM 等)
	// 要求历史 thinking block 原样回传，过滤反而制造 400。reqModel 此时已是映射后的模型 ID。
	if err := replaceBody(FilterThinkingBlocks(body, reqModel)); err != nil {
		return nil, err
	}
	// Chinese LLM thinking.type 协议差异补正（如 MiniMax 只接受 adaptive；Anthropic-SDK
	// 客户端默认发 enabled）。仅对 passback-required 上游生效（claude-* 不会进来）。
	if ResolveThinkingProtocol(reqModel) == ThinkingProtocolPassbackRequired {
		if rewritten, applied := NormalizeChineseLLMThinking(body, reqModel); applied {
			if err := replaceBody(rewritten); err != nil {
				return nil, err
			}
			logger.LegacyPrintf("service.gateway", "Account %d: rewrote thinking.type for %s (Anthropic-SDK default 'enabled' -> vendor-specific)", account.ID, reqModel)
		}
	}

	// 重试循环
	var resp *http.Response
	lastWireBody := body
	retryStart := time.Now()

	// G2a 挂点 1：取 G1a 预算快照与 G2b 安装的心跳 owner（请求级，挂点只消费、禁止重建）。
	// 无快照/无 owner 时按方案定义不兜底，走既有无护栏旧路径。
	snapshot, hasSnapshot := RequestBudgetSnapshotFromContext(ctx)
	hbOwner, _ := UpstreamHeartbeatFromContext(ctx)
	// heartbeat_delay=0 语义为禁用（方案 D2）：owner 即便存在也不起搏，预算已按
	// 无心跳流式（guard+25s）墙内收敛，避免 delay=0 被组件解释为"立即启动"。
	if hasSnapshot && snapshot.HeartbeatDelaySeconds <= 0 {
		hbOwner = nil
	}

	for attempt := 1; attempt <= maxRetryAttempts; attempt++ {
		// 构建上游请求（每次重试需要重新构建，因为请求体需要重新读取）
		upstreamCtx, releaseUpstreamCtx := detachStreamUpstreamContext(ctx, reqStream)
		// G2a 挂点 1：接 G1a 首字节护栏。窗口从不可变截止时间取剩余（禁止本层重初始化计时，
		// 换号不重置）。guard 的 reqCtx 写入请求 context：窗口耗尽取消 reqCtx 中断 header 等待。
		var guard *upstreamFirstByteGuard
		if hasSnapshot {
			window := snapshot.AttemptWindow(time.Now())
			guard, upstreamCtx = newUpstreamFirstByteGuard(upstreamCtx, window)
		}
		upstreamReq, wireBody, err := s.buildUpstreamRequest(upstreamCtx, c, account, body, token, tokenType, reqModel, reqStream, shouldMimicClaudeCode)
		releaseUpstreamCtx()
		if err != nil {
			if guard != nil {
				guard.Stop()
			}
			if hbOwner != nil && reqStream {
				hbOwner.Stop()
			}
			return nil, err
		}
		// 记录本次实际发送的 wire body；只有请求成功后才写回 ParsedRequest，避免 400 retry 基于已签名 CCH 再改写。
		lastWireBody = wireBody

		// 发送请求（统一经护栏 + 心跳生命周期编排）。此处必须用预声明 + "=" 赋值：
		// resp 是循环外声明的接收变量，若用 ":=" 会在循环块内新建同名局部变量造成
		// 遮蔽，导致循环结束后 resp 仍为 nil（"empty response"）。
		// G5 观测：obs 非 nil 时共享执行器为唯一发射点（attempt 事件/心跳事件/响应头时刻）。
		obs := &cf524GuardObservation{
			c:         c,
			ctx:       ctx, // 原始请求 context（非护栏 reqCtx），避免护栏取消误判 client_gone
			platform:  account.Platform,
			stream:    reqStream,
			attempt:   attempt,
			startedAt: time.Now(),
			snapshot:  snapshot,
		}
		var guardTimedOut bool
		resp, err, guardTimedOut = s.cf524ExecuteUpstreamWithGuard(upstreamReq, proxyURL, account, tlsProfile, guard, hbOwner, reqStream, obs)
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
			})
		}

		// 优先检测thinking block签名错误（400）并重试一次
		if resp.StatusCode == 400 {
			respBody, readErr := s.readUpstreamErrorBody(resp)
			if readErr == nil {
				_ = resp.Body.Close()

				if s.shouldRectifySignatureError(ctx, account, respBody, reqModel) {
					appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
						ProxyID:            opsUpstreamProxyID(account),
						ProxyName:          opsUpstreamProxyName(account),
						Platform:           account.Platform,
						AccountID:          account.ID,
						AccountName:        account.Name,
						UpstreamStatusCode: resp.StatusCode,
						UpstreamRequestID:  resp.Header.Get("x-request-id"),
						UpstreamURL:        safeUpstreamURL(upstreamReq.URL.String()),
						Kind:               "signature_error",
						Message:            extractUpstreamErrorMessage(respBody),
						Detail: func() string {
							if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
								return truncateString(string(respBody), s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes)
							}
							return ""
						}(),
					})

					looksLikeToolSignatureError := func(msg string) bool {
						m := strings.ToLower(msg)
						return strings.Contains(m, "tool_use") ||
							strings.Contains(m, "tool_result") ||
							strings.Contains(m, "functioncall") ||
							strings.Contains(m, "function_call") ||
							strings.Contains(m, "functionresponse") ||
							strings.Contains(m, "function_response")
					}

					// 避免在重试预算已耗尽时再发起额外请求
					if time.Since(retryStart) >= maxRetryElapsed {
						resp.Body = io.NopCloser(bytes.NewReader(respBody))
						break
					}
					logger.LegacyPrintf("service.gateway", "[warn] Account %d: thinking blocks have invalid signature, retrying with filtered blocks", account.ID)

					// Conservative two-stage fallback:
					// 1) Disable thinking + thinking->text (preserve content)
					// 2) Only if upstream still errors AND error message points to tool/function signature issues:
					//    also downgrade tool_use/tool_result blocks to text.

					filteredBody := FilterThinkingBlocksForRetry(body, reqModel)
					retryCtx, releaseRetryCtx := detachStreamUpstreamContext(ctx, reqStream)
					retryReq, retryWireBody, buildErr := s.buildUpstreamRequest(retryCtx, c, account, filteredBody, token, tokenType, reqModel, reqStream, shouldMimicClaudeCode)
					releaseRetryCtx()
					if buildErr == nil {
						retryResp, retryErr := s.httpUpstream.DoWithTLS(retryReq, proxyURL, account.ID, account.Concurrency, tlsProfile)
						if retryErr == nil {
							if retryResp.StatusCode < 400 {
								// 重试请求被上游接受后同步 ParsedRequest，保证 usage/日志看到真实请求体。
								lastWireBody = retryWireBody
								if err := replaceBody(retryWireBody); err != nil {
									_ = retryResp.Body.Close()
									return nil, err
								}
								logger.LegacyPrintf("service.gateway", "Account %d: thinking block retry succeeded (blocks downgraded)", account.ID)
								resp = retryResp
								break
							}

							retryRespBody, retryReadErr := s.readUpstreamErrorBody(retryResp)
							_ = retryResp.Body.Close()
							if retryReadErr == nil && retryResp.StatusCode == 400 && s.isSignatureErrorPattern(ctx, account, retryRespBody) {
								appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
									ProxyID:            opsUpstreamProxyID(account),
									ProxyName:          opsUpstreamProxyName(account),
									Platform:           account.Platform,
									AccountID:          account.ID,
									AccountName:        account.Name,
									UpstreamStatusCode: retryResp.StatusCode,
									UpstreamRequestID:  retryResp.Header.Get("x-request-id"),
									UpstreamURL:        safeUpstreamURL(retryReq.URL.String()),
									Kind:               "signature_retry_thinking",
									Message:            extractUpstreamErrorMessage(retryRespBody),
									Detail: func() string {
										if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
											return truncateString(string(retryRespBody), s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes)
										}
										return ""
									}(),
								})
								msg2 := extractUpstreamErrorMessage(retryRespBody)
								if looksLikeToolSignatureError(msg2) && time.Since(retryStart) < maxRetryElapsed {
									logger.LegacyPrintf("service.gateway", "Account %d: signature retry still failing and looks tool-related, retrying with tool blocks downgraded", account.ID)
									filteredBody2 := FilterSignatureSensitiveBlocksForRetry(body, reqModel)
									retryCtx2, releaseRetryCtx2 := detachStreamUpstreamContext(ctx, reqStream)
									retryReq2, retryWireBody2, buildErr2 := s.buildUpstreamRequest(retryCtx2, c, account, filteredBody2, token, tokenType, reqModel, reqStream, shouldMimicClaudeCode)
									releaseRetryCtx2()
									if buildErr2 == nil {
										retryResp2, retryErr2 := s.httpUpstream.DoWithTLS(retryReq2, proxyURL, account.ID, account.Concurrency, tlsProfile)
										if retryErr2 == nil {
											if retryResp2.StatusCode < 400 {
												// 二阶段工具块降级成功时也必须更新当前 body。
												lastWireBody = retryWireBody2
												if err := replaceBody(retryWireBody2); err != nil {
													_ = retryResp2.Body.Close()
													return nil, err
												}
											}
											resp = retryResp2
											break
										}
										if retryResp2 != nil && retryResp2.Body != nil {
											_ = retryResp2.Body.Close()
										}
										appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
											ProxyID:            opsUpstreamProxyID(account),
											ProxyName:          opsUpstreamProxyName(account),
											Platform:           account.Platform,
											AccountID:          account.ID,
											AccountName:        account.Name,
											UpstreamStatusCode: 0,
											UpstreamURL:        safeUpstreamURL(retryReq2.URL.String()),
											Kind:               "signature_retry_tools_request_error",
											Message:            sanitizeUpstreamErrorMessage(retryErr2.Error()),
										})
										logger.LegacyPrintf("service.gateway", "Account %d: tool-downgrade signature retry failed: %v", account.ID, retryErr2)
									} else {
										logger.LegacyPrintf("service.gateway", "Account %d: tool-downgrade signature retry build failed: %v", account.ID, buildErr2)
									}
								}
							}

							// Fall back to the original retry response context.
							resp = &http.Response{
								StatusCode: retryResp.StatusCode,
								Header:     retryResp.Header.Clone(),
								Body:       io.NopCloser(bytes.NewReader(retryRespBody)),
							}
							break
						}
						if retryResp != nil && retryResp.Body != nil {
							_ = retryResp.Body.Close()
						}
						logger.LegacyPrintf("service.gateway", "Account %d: signature error retry failed: %v", account.ID, retryErr)
					} else {
						logger.LegacyPrintf("service.gateway", "Account %d: signature error retry build request failed: %v", account.ID, buildErr)
					}

					// Retry failed: restore original response body and continue handling.
					resp.Body = io.NopCloser(bytes.NewReader(respBody))
					break
				}
				// 不是签名错误（或整流器已关闭），继续检查 budget 约束
				errMsg := extractUpstreamErrorMessage(respBody)
				if isThinkingBudgetConstraintError(errMsg) && s.settingService.IsBudgetRectifierEnabled(ctx) {
					appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
						ProxyID:            opsUpstreamProxyID(account),
						ProxyName:          opsUpstreamProxyName(account),
						Platform:           account.Platform,
						AccountID:          account.ID,
						AccountName:        account.Name,
						UpstreamStatusCode: resp.StatusCode,
						UpstreamRequestID:  resp.Header.Get("x-request-id"),
						UpstreamURL:        safeUpstreamURL(upstreamReq.URL.String()),
						Kind:               "budget_constraint_error",
						Message:            errMsg,
						Detail: func() string {
							if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
								return truncateString(string(respBody), s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes)
							}
							return ""
						}(),
					})

					rectifiedBody, applied := RectifyThinkingBudget(body)
					if applied && time.Since(retryStart) < maxRetryElapsed {
						logger.LegacyPrintf("service.gateway", "Account %d: detected budget_tokens constraint error, retrying with rectified budget (budget_tokens=%d, max_tokens=%d)", account.ID, BudgetRectifyBudgetTokens, BudgetRectifyMaxTokens)
						budgetRetryCtx, releaseBudgetRetryCtx := detachStreamUpstreamContext(ctx, reqStream)
						budgetRetryReq, budgetWireBody, buildErr := s.buildUpstreamRequest(budgetRetryCtx, c, account, rectifiedBody, token, tokenType, reqModel, reqStream, shouldMimicClaudeCode)
						releaseBudgetRetryCtx()
						if buildErr == nil {
							budgetRetryResp, retryErr := s.httpUpstream.DoWithTLS(budgetRetryReq, proxyURL, account.ID, account.Concurrency, tlsProfile)
							if retryErr == nil {
								if budgetRetryResp.StatusCode < 400 {
									// budget 修正请求成功后，ParsedRequest 也要描述被接受的修正版。
									lastWireBody = budgetWireBody
									if err := replaceBody(budgetWireBody); err != nil {
										_ = budgetRetryResp.Body.Close()
										return nil, err
									}
								}
								resp = budgetRetryResp
								break
							}
							if budgetRetryResp != nil && budgetRetryResp.Body != nil {
								_ = budgetRetryResp.Body.Close()
							}
							logger.LegacyPrintf("service.gateway", "Account %d: budget rectifier retry failed: %v", account.ID, retryErr)
						} else {
							logger.LegacyPrintf("service.gateway", "Account %d: budget rectifier retry build failed: %v", account.ID, buildErr)
						}
					}
				}

				resp.Body = io.NopCloser(bytes.NewReader(respBody))
			}
		}

		// 检查是否需要通用重试（排除400，因为400已经在上面特殊处理过了）
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
					Kind:               "retry",
					Message:            extractUpstreamErrorMessage(respBody),
					Detail: func() string {
						if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
							return truncateString(string(respBody), s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes)
						}
						return ""
					}(),
				})
				logger.LegacyPrintf("service.gateway", "Account %d: upstream error %d, retry %d/%d after %v (elapsed=%v/%v)",
					account.ID, resp.StatusCode, attempt, maxRetryAttempts, delay, elapsed, maxRetryElapsed)
				if err := sleepWithContext(ctx, delay); err != nil {
					return nil, err
				}
				continue
			}
			// 最后一次尝试也失败，跳出循环处理重试耗尽
			break
		}

		// 不需要重试（成功或不可重试的错误），跳出循环
		// DEBUG: 输出响应 headers（用于检测 rate limit 信息）
		if account.Platform == PlatformGemini && resp.StatusCode < 400 && s.cfg != nil && s.cfg.Gateway.GeminiDebugResponseHeaders {
			logger.LegacyPrintf("service.gateway", "[DEBUG] Gemini API Response Headers for account %d:", account.ID)
			for k, v := range resp.Header {
				logger.LegacyPrintf("service.gateway", "[DEBUG]   %s: %v", k, v)
			}
		}
		break
	}
	if resp == nil || resp.Body == nil {
		return nil, errors.New("upstream request failed: empty response")
	}
	defer func() { _ = resp.Body.Close() }()

	// 处理重试耗尽的情况
	if resp.StatusCode >= 400 && s.shouldRetryUpstreamError(account, resp.StatusCode) {
		if s.shouldFailoverUpstreamError(resp.StatusCode) {
			respBody, _ := s.readUpstreamErrorBody(resp)
			_ = resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(respBody))

			// 调试日志：打印重试耗尽后的错误响应
			logger.LegacyPrintf("service.gateway", "[Forward] Upstream error (retry exhausted, failover): Account=%d(%s) Status=%d RequestID=%s Body=%s",
				account.ID, account.Name, resp.StatusCode, resp.Header.Get("x-request-id"), truncateString(string(respBody), 1000))

			s.handleRetryExhaustedSideEffects(ctx, resp, account)
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				ProxyID:            opsUpstreamProxyID(account),
				ProxyName:          opsUpstreamProxyName(account),
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: resp.StatusCode,
				UpstreamRequestID:  resp.Header.Get("x-request-id"),
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
		return s.handleRetryExhaustedError(ctx, resp, c, account)
	}

	// 处理可切换账号的错误
	if resp.StatusCode >= 400 && s.shouldFailoverUpstreamError(resp.StatusCode) {
		respBody, _ := s.readUpstreamErrorBody(resp)
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(respBody))

		// 调试日志：打印上游错误响应
		logger.LegacyPrintf("service.gateway", "[Forward] Upstream error (failover): Account=%d(%s) Status=%d RequestID=%s Body=%s",
			account.ID, account.Name, resp.StatusCode, resp.Header.Get("x-request-id"), truncateString(string(respBody), 1000))

		s.handleFailoverSideEffects(ctx, resp, account, reqModel)
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			ProxyID:            opsUpstreamProxyID(account),
			ProxyName:          opsUpstreamProxyName(account),
			Platform:           account.Platform,
			AccountID:          account.ID,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  resp.Header.Get("x-request-id"),
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
		// 可选：对部分 400 触发 failover（默认关闭以保持语义）
		if resp.StatusCode == 400 && s.cfg != nil && s.cfg.Gateway.FailoverOn400 {
			respBody, readErr := s.readUpstreamErrorBody(resp)
			if readErr != nil {
				// ReadAll failed, fall back to normal error handling without consuming the stream
				return s.handleErrorResponse(ctx, resp, c, account, reqModel)
			}
			_ = resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(respBody))

			if s.shouldFailoverOn400(respBody) {
				upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(respBody))
				upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)
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
					Kind:               "failover_on_400",
					Message:            upstreamMsg,
					Detail:             upstreamDetail,
				})

				if s.cfg.Gateway.LogUpstreamErrorBody {
					logger.LegacyPrintf("service.gateway",
						"Account %d: 400 error, attempting failover: %s",
						account.ID,
						truncateForLog(respBody, s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes),
					)
				} else {
					logger.LegacyPrintf("service.gateway", "Account %d: 400 error, attempting failover", account.ID)
				}
				s.handleFailoverSideEffects(ctx, resp, account, reqModel)
				return nil, &UpstreamFailoverError{StatusCode: resp.StatusCode, ResponseBody: respBody}
			}
		}
		return s.handleErrorResponse(ctx, resp, c, account, reqModel)
	}

	// 处理正常响应

	if !bytes.Equal(lastWireBody, body) {
		// 成功后再同步最终 wire body，避免失败重试从已签名 CCH 的 body 继续派生。
		if err := replaceBody(lastWireBody); err != nil {
			return nil, err
		}
	}

	// 触发上游接受回调（提前释放串行锁，不等流完成）
	if parsed.OnUpstreamAccepted != nil {
		parsed.OnUpstreamAccepted()
	}

	var usage *ClaudeUsage
	var firstTokenMs *int
	var clientDisconnect bool
	if reqStream {
		writerSizeBeforeStream := c.Writer.Size()
		streamResult, err := s.handleStreamingResponse(ctx, resp, c, account, startTime, originalModel, reqModel, shouldMimicClaudeCode)
		if err != nil {
			var sseErr *sseStreamErrorEventError
			if errors.As(err, &sseErr) {
				// 上游 HTTP 200 + SSE 流体内出现 event:error 帧。
				body := []byte(sseErr.RawData)
				semanticStatus := http.StatusForbidden
				if c.Writer.Size() == writerSizeBeforeStream && gjson.GetBytes(body, "error.type").String() == "overloaded_error" {
					semanticStatus = 529
					syntheticResp := &http.Response{
						StatusCode: semanticStatus,
						Header:     resp.Header.Clone(),
						Body:       io.NopCloser(bytes.NewReader(body)),
					}
					s.handleFailoverSideEffects(ctx, syntheticResp, account, reqModel)
				}

				upstreamMsg := sanitizeUpstreamErrorMessage(
					strings.TrimSpace(extractUpstreamErrorMessage(body)),
				)

				upstreamDetail := ""
				if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
					maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
					if maxBytes <= 0 {
						maxBytes = 2048
					}
					upstreamDetail = truncateString(sseErr.RawData, maxBytes)
				}

				appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
					ProxyID:            opsUpstreamProxyID(account),
					ProxyName:          opsUpstreamProxyName(account),
					Platform:           account.Platform,
					AccountID:          account.ID,
					AccountName:        account.Name,
					UpstreamStatusCode: semanticStatus,
					UpstreamRequestID:  resp.Header.Get("x-request-id"),
					Kind:               "stream_error",
					Message:            upstreamMsg,
					Detail:             upstreamDetail,
				})

				logger.LegacyPrintf("service.gateway",
					"[Forward] SSE error event in stream: Account=%d(%s) RequestID=%s Body=%s",
					account.ID, account.Name, resp.Header.Get("x-request-id"),
					truncateString(sseErr.RawData, 1000),
				)

				return nil, &UpstreamFailoverError{
					StatusCode:   semanticStatus,
					ResponseBody: body,
				}
			}
			// 流中断（缺失 terminal 事件、读错误、数据间隔超时等）时保留已观测到的
			// usage 与错误一起返回，handler 在错误处理完成后照常提交 usage 记录。
			if partial := partialStreamUsageResult(c, resp, streamResult, originalModel, mappedModel, startTime, err); partial != nil {
				return partial, err
			}
			return nil, err
		}
		usage = streamResult.usage
		firstTokenMs = streamResult.firstTokenMs
		clientDisconnect = streamResult.clientDisconnect
	} else {
		usage, err = s.handleNonStreamingResponse(ctx, resp, c, account, originalModel, reqModel)
		if err != nil {
			return nil, err
		}
	}

	return &ForwardResult{
		RequestID:                     resp.Header.Get("x-request-id"),
		UpstreamHeaders:               resp.Header,
		Usage:                         *usage,
		Model:                         originalModel, // 使用原始模型用于计费和日志
		UpstreamModel:                 mappedModel,
		UpstreamResponseModel:         observedUpstreamResponseModel(c),
		UpstreamResponseModelConflict: observedUpstreamResponseModelConflict(c),
		UpstreamResponseServiceTier:   observedUpstreamResponseServiceTier(c),
		Stream:                        reqStream,
		Duration:                      time.Since(startTime),
		FirstTokenMs:                  firstTokenMs,
		ClientDisconnect:              clientDisconnect,
	}, nil
}

// cf524ExecuteUpstreamWithGuard 在挂点统一执行"首字节护栏 + 流式心跳生命周期"的上游请求。
//
// 调用方须在构建 upstreamReq 时已将 guard 的 reqCtx（若有）写入请求 context：窗口耗尽
// 时护栏取消 reqCtx 中断 header 等待；guard 为 nil 表示无快照、走旧路径。心跳 owner（hb）
// 由 G2b 在 handler 安装、请求级持久；本函数只消费：clientStream 为真且 guard 有效时
// 在发送前 Resume（首次 Start / 换号后重启），响应头到达或 guard 超时后按 D2 状态机
// stop-and-wait。
//
// 返回 timedOut 表示护栏已裁决超时（调用方据 snapshot 构造 newUpstreamFirstByteTimeoutError
// 交 handler 换号/耗尽）。err 非 nil 且 timedOut=false 表示真实传输错误（护栏未触发）。
//
// G5 观测（单点埋点）：obs 非 nil 时，本函数是四个非 OpenAI 挂点唯一的观测发射点——
// 护栏超时发 `gateway_first_byte_guard_triggered` attempt 事件、心跳已提交且此前未发
// 则发 `gateway_upstream_heartbeat_started`、响应头到达则记录判别器时刻
// `upstream_headers_received_ms`（经既有请求级完成日志输出，零新事件流）。obs 为 nil 时
// 全部为 no-op（测试/无埋点调用方零影响）。
func (s *GatewayService) cf524ExecuteUpstreamWithGuard(
	upstreamReq *http.Request,
	proxyURL string,
	account *Account,
	tlsProfile *tlsfingerprint.Profile,
	guard *upstreamFirstByteGuard,
	hb *UpstreamHeartbeat,
	clientStream bool,
	obs *cf524GuardObservation,
) (resp *http.Response, err error, timedOut bool) {
	// 心跳仅在有护栏（=有预算快照）且客户端流式时起搏：与 G2b 安装契约一致，owner
	// 与快照同进同退；无快照则无 owner，绝不单独起搏心跳导致无超时保护的悬挂。
	if hb != nil && clientStream && guard != nil {
		// Resume 同时覆盖"首次启动"与"换号后重启"两种形态（未启动则等价于 Start）。
		// 失败可见性：Resume 错误（缺 http.Flusher）只记 warn 日志（path/request_id 可关联），
		// 继续既有流程，零语义变化——心跳是可关闭优化（hb=0 是合法一等状态），
		// 硬边界是 guard，不在此处终止请求。
		if resumeErr := hb.Resume(); resumeErr != nil {
			requestID, _ := upstreamReq.Context().Value(ctxkey.RequestID).(string)
			logger.FromContext(upstreamReq.Context()).Warn("gateway.cf524_heartbeat_resume_failed",
				zap.String("path", upstreamReq.URL.Path),
				zap.String("request_id", requestID),
				zap.Int64("account_id", account.ID),
				zap.Error(resumeErr),
			)
		}
	}
	resp, err = s.httpUpstream.DoWithTLS(upstreamReq, proxyURL, account.ID, account.Concurrency, tlsProfile)
	if guard != nil && guard.TimedOut() {
		// 状态机优先级②（D2）：guard 终态先于一切 → OnGuardDecided 落闸 + stop-and-wait
		// 心跳（单一 writer 所有权）后返回 failover 错误，绝不先于心跳停等就换号。
		if hb != nil {
			hb.OnGuardDecided()
			hb.Stop()
		}
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		obs.noteHeartbeatIfCommitted(hb)
		obs.observeGuardTriggered()
		return nil, nil, true
	}
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		// 真实传输错误：停表（零取消收尾，释放定时器）+ 心跳 stop-and-wait，
		// 避免泄漏 keep-alive goroutine；随后交调用方处理。
		if guard != nil {
			guard.Stop()
		}
		if hb != nil {
			hb.Stop()
		}
		obs.noteHeartbeatIfCommitted(hb)
		return nil, err, false
	}
	// 响应头到达（未超时）：护栏停表零取消；心跳按晚到契约裁决（Rejected/AfterCommit）。
	// 单一 writer 所有权（D2 硬约束）：进入真实转发/错误通道前必须同步 stop-and-wait，
	// 等心跳 goroutine 退出后才把 ResponseWriter 交还调用方，避免注释帧与语义流交错。
	if guard != nil {
		guard.Stop()
	}
	if hb != nil {
		hb.OnUpstreamHeaderArrived()
		hb.Stop()
	}
	obs.noteHeartbeatIfCommitted(hb)
	obs.markUpstreamHeadersReceived()
	return resp, nil, false
}

func anthropicSpeedModel(parsed *ParsedRequest, result *ForwardResult) string {
	if result != nil {
		if upstreamModel := strings.TrimSpace(result.UpstreamModel); upstreamModel != "" {
			return upstreamModel
		}
	}
	if parsed == nil {
		return ""
	}
	return parsed.Model
}

// anthropicSpeedServiceTier 把 Anthropic 的 speed=fast 归一成可计费的 "fast" tier。
//
// Fast mode 目前只在 Claude Opus 5 / Opus 4.8 上存在，且不支持 Bedrock 等第三方
// 承载（Opus 4.7 的 fast mode 已被移除，传 speed=fast 会直接报错）。这里按模型和
// 平台收紧，避免上游根本没跑 fast 时仍然按 2x 计费——宁可漏收也不能多收。
//
// 这里只决定请求侧的档位；上游响应里的 usage.speed 由 UpstreamResponseServiceTier
// 带回，用量记录时经 ResolveBillingServiceTier 只降不升（usage.speed=standard 则按
// 标准价计费）。
func anthropicSpeedServiceTier(account *Account, speed, model string) *string {
	if account == nil || account.Platform != PlatformAnthropic || speed != "fast" {
		return nil
	}
	if account.IsBedrock() || !modelSupportsAnthropicFastMode(model) {
		return nil
	}
	tier := "fast"
	return &tier
}

// modelSupportsAnthropicFastMode 判断模型是否属于支持 fast mode 的 Opus 5 / Opus 4.8。
func modelSupportsAnthropicFastMode(model string) bool {
	modelLower := strings.ToLower(strings.TrimSpace(model))
	if !strings.Contains(modelLower, "opus") {
		return false
	}
	// "opus-5" 必须先判：不能用裸 "5" 匹配，否则 claude-opus-4-5 会被误判。
	if strings.Contains(modelLower, "opus-5") || strings.Contains(modelLower, "opus5") {
		return true
	}
	return strings.Contains(modelLower, "4.8") || strings.Contains(modelLower, "4-8")
}

// ResolveChannelMapping 委托渠道服务解析模型映射
func (s *GatewayService) ResolveChannelMapping(ctx context.Context, groupID int64, model string) ChannelMappingResult {
	if s.channelService == nil {
		return ChannelMappingResult{MappedModel: model}
	}
	return s.channelService.ResolveChannelMapping(ctx, groupID, model)
}

// ReplaceModelInBody 替换请求体中的模型名（导出供 handler 使用）
func (s *GatewayService) ReplaceModelInBody(body []byte, newModel string) []byte {
	return ReplaceModelInBody(body, newModel)
}

// IsModelRestricted 检查模型是否被渠道限制
func (s *GatewayService) IsModelRestricted(ctx context.Context, groupID int64, model string) bool {
	if s.channelService == nil {
		return false
	}
	return s.channelService.IsModelRestricted(ctx, groupID, model)
}

// ResolveChannelMappingAndRestrict 解析渠道映射。
// 模型限制检查已移至调度阶段（checkChannelPricingRestriction），restricted 始终返回 false。
func (s *GatewayService) ResolveChannelMappingAndRestrict(ctx context.Context, groupID *int64, model string) (ChannelMappingResult, bool) {
	if s.channelService == nil {
		return ChannelMappingResult{MappedModel: model}, false
	}
	return s.channelService.ResolveChannelMappingAndRestrict(ctx, groupID, model)
}

// checkChannelPricingRestriction 根据渠道计费基准检查模型是否受定价列表限制。
// 供调度阶段预检查（requested / channel_mapped）。
// upstream 需逐账号检查，此处返回 false。
func (s *GatewayService) checkChannelPricingRestriction(ctx context.Context, groupID *int64, requestedModel string) bool {
	if groupID == nil || s.channelService == nil || requestedModel == "" {
		return false
	}
	mapping := s.channelService.ResolveChannelMapping(ctx, *groupID, requestedModel)
	billingModel := billingModelForRestriction(mapping.BillingModelSource, requestedModel, mapping.MappedModel)
	if billingModel == "" {
		return false
	}
	return s.channelService.IsModelRestricted(ctx, *groupID, billingModel)
}

// billingModelForRestriction 根据计费基准确定限制检查使用的模型。
// upstream 返回空（需逐账号检查）。
func billingModelForRestriction(source, requestedModel, channelMappedModel string) string {
	switch source {
	case BillingModelSourceRequested:
		return requestedModel
	case BillingModelSourceUpstream:
		return ""
	case BillingModelSourceResponse:
		// The response is not available during dispatch; use mapped pricing
		// for restriction prechecks and decide billing after the response.
		return channelMappedModel
	case BillingModelSourceChannelMapped:
		return channelMappedModel
	default:
		return channelMappedModel
	}
}

// isUpstreamModelRestrictedByChannel 检查账号映射后的上游模型是否受渠道定价限制。
// 仅在 BillingModelSource="upstream" 且 RestrictModels=true 时由调度循环调用。
func (s *GatewayService) isUpstreamModelRestrictedByChannel(ctx context.Context, groupID int64, account *Account, requestedModel string) bool {
	if s.channelService == nil {
		return false
	}
	upstreamModel := resolveAccountUpstreamModel(account, requestedModel)
	if upstreamModel == "" {
		return false
	}
	return s.channelService.IsModelRestricted(ctx, groupID, upstreamModel)
}

// resolveAccountUpstreamModel 确定账号将请求模型映射为什么上游模型。
func resolveAccountUpstreamModel(account *Account, requestedModel string) string {
	if account.Platform == PlatformAntigravity {
		return mapAntigravityModel(account, requestedModel)
	}
	return account.GetMappedModel(requestedModel)
}

// needsUpstreamChannelRestrictionCheck 判断是否需要在调度循环中逐账号检查上游模型的渠道限制。
func (s *GatewayService) needsUpstreamChannelRestrictionCheck(ctx context.Context, groupID *int64) bool {
	if groupID == nil || s.channelService == nil {
		return false
	}
	ch, err := s.channelService.GetChannelForGroup(ctx, *groupID)
	if err != nil {
		slog.Warn("failed to check channel upstream restriction", "group_id", *groupID, "error", err)
		return false
	}
	if ch == nil || !ch.RestrictModels {
		return false
	}
	return ch.BillingModelSource == BillingModelSourceUpstream
}

// isStickyAccountUpstreamRestricted 检查粘性会话命中的账号是否受 upstream 渠道限制。
// 合并 needsUpstreamChannelRestrictionCheck + isUpstreamModelRestrictedByChannel 两步调用，
// 供 sticky session 条件链使用，避免内联多个函数调用导致行过长。
func (s *GatewayService) isStickyAccountUpstreamRestricted(ctx context.Context, groupID *int64, account *Account, requestedModel string) bool {
	if groupID == nil {
		return false
	}
	if !s.needsUpstreamChannelRestrictionCheck(ctx, groupID) {
		return false
	}
	return s.isUpstreamModelRestrictedByChannel(ctx, *groupID, account, requestedModel)
}
