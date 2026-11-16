package service

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
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/geminicli"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
)

// ForwardAsChatCompletions serves OpenAI Chat Completions clients through
// Gemini accounts. It keeps the client-facing response in Chat Completions
// format while routing the upstream call through Gemini native endpoints.
func (s *GeminiMessagesCompatService) ForwardAsChatCompletions(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
) (*ForwardResult, error) {
	// Cursor 账号必须在 Gemini 转换链之前分流——落到通用路径会先把入站 Chat
	// Completions 转成 Gemini 形状再发往只支持 /chat/completions 的上游，必然失败。
	//   - OpenAI 兼容账号（platform=cursor + type=apikey，如 kn-cy）：出站本身就是
	//     OpenAI 兼容 /chat/completions 反代端点，必须以 Bearer api_key 直连转发，
	//     绝不能再经 Cursor 原生协议执行部件或 Gemini 转换链。
	//   - 其余 cursor 账号（OAuth Connect 号池）：沿用原 forwardCursorChatCompletions。
	if isCursorAccount(account) {
		if isCursorOpenAICompatibleAccount(account) {
			return s.forwardCursorOpenAICompatibleChat(ctx, c, account, body)
		}
		res, ferr := s.forwardCursorChatCompletions(ctx, c, account, body)
		if ferr != nil {
			// 出口先把错误按 Chat Completions 形状写出，避免 handler 回落到
			// 与入站协议无关的通用错误体；Cursor 路径不参与换号重试。
			return nil, s.writeCursorChatCompletionsError(c, account, ferr)
		}
		return res, nil
	}

	startTime := time.Now()

	var ccReq apicompat.ChatCompletionsRequest
	if err := json.Unmarshal(body, &ccReq); err != nil {
		return nil, s.writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", infraerrors.RequestBodyParseFailed)
	}
	if strings.TrimSpace(ccReq.Model) == "" {
		return nil, s.writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", infraerrors.ModelRequired)
	}

	originalModel := ccReq.Model
	clientStream := ccReq.Stream
	includeUsage := ccReq.StreamOptions != nil && ccReq.StreamOptions.IncludeUsage

	// 历史推理内容不回传给 Gemini：Gemini 的思考连续性由 thoughtSignature 承载。
	for i := range ccReq.Messages {
		ccReq.Messages[i].ReasoningContent = ""
		ccReq.Messages[i].Reasoning = ""
		if ccReq.Messages[i].Role == "assistant" {
			ccReq.Messages[i].Content = dropChatReasoningContentParts(ccReq.Messages[i].Content)
		}
	}

	responsesReq, err := chatCompletionsToResponses(s.cfg, &ccReq)
	if err != nil {
		return nil, s.writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
	}

	anthropicReq, err := apicompat.ResponsesToAnthropicRequest(responsesReq)
	if err != nil {
		return nil, s.writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
	}
	anthropicReq.Stream = clientStream

	claudeBody, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, fmt.Errorf("marshal chat completions compat request: %w", err)
	}

	return s.forwardClaudeBodyAsChatCompletions(ctx, c, account, claudeBody, originalModel, clientStream, includeUsage, startTime)
}

// forwardCursorOpenAICompatibleChat 处理 platform=cursor + type=apikey 的 OpenAI
// 兼容反代账号（kn-cy 等）。这类账号出站本身就是 OpenAI 兼容 /chat/completions
// 端点，入站已是 Chat Completions 形状，直接改 model 后以 Bearer api_key 转发，
// 响应原样回传客户端；绝不落入 Gemini 转换链或 Cursor 原生协议执行部件。
func (s *GeminiMessagesCompatService) forwardCursorOpenAICompatibleChat(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
) (*ForwardResult, error) {
	startTime := time.Now()

	var ccReq apicompat.ChatCompletionsRequest
	if err := json.Unmarshal(body, &ccReq); err != nil {
		return nil, s.writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", infraerrors.RequestBodyParseFailed)
	}
	originalModel := ccReq.Model
	if strings.TrimSpace(originalModel) == "" {
		return nil, s.writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", infraerrors.ModelRequired)
	}
	// 记录客户端是否显式要求 usage。流式分支会强制上游 include_usage 用于计费，
	// 但客户端未要求时不把纯 usage 帧回写，保持客户端可见的流形状不变。
	clientWantsUsage := ccReq.StreamOptions != nil && ccReq.StreamOptions.IncludeUsage

	apiKey := account.GetCredential("api_key")
	if strings.TrimSpace(apiKey) == "" {
		return nil, s.writeChatCompletionsError(c, http.StatusBadGateway, "upstream_error", infraerrors.AccountConfigUnsupported)
	}
	baseURL := strings.TrimSpace(account.GetOpenAIBaseURL())
	if baseURL == "" {
		return nil, s.writeChatCompletionsError(c, http.StatusBadGateway, "upstream_error", infraerrors.AccountConfigUnsupported)
	}
	normalizedBaseURL, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return nil, s.writeChatCompletionsError(c, http.StatusBadGateway, "upstream_error", err.Error())
	}
	fullURL := strings.TrimRight(normalizedBaseURL, "/") + "/chat/completions"

	mappedModel := originalModel
	if account.Type == AccountTypeAPIKey || account.Type == AccountTypeServiceAccount {
		mappedModel = account.GetMappedModel(originalModel)
	}
	ccReq.Model = mappedModel
	if ccReq.Stream {
		// 兼容上游只在请求显式带 stream_options.include_usage 时才在末帧回 usage。
		// 流式强制打开，否则本路径会把 usage 计成 0（少计费），口径同
		// openai_gateway_messages_chat_fallback.go:84。
		ccReq.StreamOptions = &apicompat.ChatStreamOptions{IncludeUsage: true}
	}
	upstreamBody, err := json.Marshal(&ccReq)
	if err != nil {
		return nil, s.writeChatCompletionsError(c, http.StatusBadGateway, "upstream_error", err.Error())
	}

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, bytes.NewReader(upstreamBody))
	if err != nil {
		return nil, s.writeChatCompletionsError(c, http.StatusBadGateway, "upstream_error", err.Error())
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		return nil, s.handleUpstreamTransportError(ctx, c, account, err)
	}
	defer resp.Body.Close()

	requestID := resp.Header.Get("x-request-id")
	if resp.StatusCode >= 400 {
		respBody := s.readUpstreamErrorBody(resp)
		return nil, s.writeChatCompletionsError(c, resp.StatusCode, "upstream_error", extractUpstreamErrorMessage(respBody))
	}

	var usage ClaudeUsage
	if ccReq.Stream {
		usage, err = s.writeCursorOpenAICompatibleChatStream(c, resp, !clientWantsUsage)
	} else {
		usage, err = s.writeCursorOpenAICompatibleChatNonStream(c, resp)
	}
	if err != nil {
		return nil, err
	}

	return &ForwardResult{
		RequestID:     requestID,
		Usage:         usage,
		Model:         originalModel,
		UpstreamModel: mappedModel,
		Stream:        ccReq.Stream,
		Duration:      time.Since(startTime),
	}, nil
}

// writeCursorOpenAICompatibleChatStream 逐帧透传 OpenAI Chat Completions SSE，
// 并从各帧 usage 累计出 ClaudeUsage。
// dropUsageOnlyChunk 为真（客户端未要求 usage）时，纯 usage 帧（choices 为空且
// usage 非空）不回写客户端，只累计 usage——避免把为计费而强制打开的 usage 帧
// 暴露给未请求的客户端。
func (s *GeminiMessagesCompatService) writeCursorOpenAICompatibleChatStream(
	c *gin.Context,
	resp *http.Response,
	dropUsageOnlyChunk bool,
) (ClaudeUsage, error) {
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return ClaudeUsage{}, s.writeChatCompletionsError(c, http.StatusInternalServerError, "api_error", infraerrors.UpstreamStreamReadFailed)
	}
	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	reader := bufio.NewReader(resp.Body)
	var usage ClaudeUsage
	for {
		line, readErr := reader.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(trimmed, "data:") {
				payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
				// 空 payload 不写 data: 帧，也不参与 usage 解析。
				if payload != "" {
					var chunk apicompat.ChatCompletionsChunk
					chunkParsed := payload != "[DONE]" && json.Unmarshal([]byte(payload), &chunk) == nil
					// 纯 usage 帧：choices 为空且 usage 非空。
					usageOnlyChunk := chunkParsed && len(chunk.Choices) == 0 && chunk.Usage != nil
					// 客户端未要求 usage 时丢弃纯 usage 帧，仅累计 usage，不回写客户端。
					if !(dropUsageOnlyChunk && usageOnlyChunk) {
						if _, werr := fmt.Fprintf(c.Writer, "data: %s\n\n", payload); werr != nil {
							break
						}
						flusher.Flush()
					}
					if chunkParsed && chunk.Usage != nil {
						u := cursorOpenAICompatibleChatUsage(chunk.Usage)
						usage.InputTokens += u.InputTokens
						usage.OutputTokens += u.OutputTokens
						usage.CacheReadInputTokens += u.CacheReadInputTokens
						usage.CacheCreationInputTokens += u.CacheCreationInputTokens
					}
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			break
		}
	}
	return usage, nil
}

// writeCursorOpenAICompatibleChatNonStream 把上游 chat.completion 响应体原样写回客户端，
// 并从 usage 字段解析出 ClaudeUsage。
func (s *GeminiMessagesCompatService) writeCursorOpenAICompatibleChatNonStream(
	c *gin.Context,
	resp *http.Response,
) (ClaudeUsage, error) {
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return ClaudeUsage{}, s.writeChatCompletionsError(c, http.StatusBadGateway, "upstream_error", infraerrors.UpstreamRequestFailed)
	}
	var usage ClaudeUsage
	var ccResp apicompat.ChatCompletionsResponse
	if json.Unmarshal(respBody, &ccResp) == nil {
		usage = cursorOpenAICompatibleChatUsage(ccResp.Usage)
	}
	c.Data(resp.StatusCode, "application/json", respBody)
	return usage, nil
}

// cursorOpenAICompatibleChatUsage 按 apicompat.chatUsageToAnthropicUsage 的口径把
// OpenAI Chat Completions usage 落到 ClaudeUsage：缓存读写 token 单列，InputTokens
// 扣除缓存命中与缓存写入，不折算、不补默认值。
func cursorOpenAICompatibleChatUsage(u *apicompat.ChatUsage) ClaudeUsage {
	if u == nil {
		return ClaudeUsage{}
	}
	cachedTokens := 0
	cacheCreationTokens := 0
	if u.PromptTokensDetails != nil {
		cachedTokens = u.PromptTokensDetails.CachedTokens
		// cache_write_tokens 与 cache_creation_tokens 是同一数量的两种拼写，
		// 优先 write、回落 creation，不相加（口径同 chatUsageToAnthropicUsage）。
		if u.PromptTokensDetails.CacheWriteTokens > 0 {
			cacheCreationTokens = u.PromptTokensDetails.CacheWriteTokens
		} else {
			cacheCreationTokens = u.PromptTokensDetails.CacheCreationTokens
		}
	}
	inputTokens := u.PromptTokens - cachedTokens - cacheCreationTokens
	if inputTokens < 0 {
		inputTokens = 0
	}
	return ClaudeUsage{
		InputTokens:              inputTokens,
		OutputTokens:             u.CompletionTokens,
		CacheReadInputTokens:     cachedTokens,
		CacheCreationInputTokens: cacheCreationTokens,
	}
}

func (s *GeminiMessagesCompatService) forwardClaudeBodyAsChatCompletions(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	claudeBody []byte,
	originalModel string,
	clientStream bool,
	includeUsage bool,
	startTime time.Time,
) (*ForwardResult, error) {
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(claudeBody, &req); err != nil {
		return nil, s.writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", infraerrors.RequestBodyParseFailed)
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, s.writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", infraerrors.ModelRequired)
	}

	mappedModel := req.Model
	if account.Type == AccountTypeAPIKey || account.Type == AccountTypeServiceAccount {
		mappedModel = account.GetMappedModel(req.Model)
	}

	geminiReq, err := convertClaudeMessagesToGeminiGenerateContent(claudeBody)
	if err != nil {
		return nil, s.writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
	}
	if geminiImageGenerationDenied(c, geminiReq, originalModel, req.Model, mappedModel) {
		return nil, s.writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", ImageGenerationPermissionMessage())
	}
	geminiReq = ensureGeminiFunctionCallThoughtSignatures(geminiReq)
	geminiReq = applyGeminiCompatThinkingConfig(geminiReq, claudeBody, mappedModel, clientStream)

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	useUpstreamStream := clientStream
	if account.Type == AccountTypeOAuth && !clientStream && strings.TrimSpace(account.GetCredential("project_id")) != "" {
		useUpstreamStream = true
	}

	buildReq, requestIDHeader := s.buildGeminiChatCompletionsUpstreamRequestFunc(
		account,
		mappedModel,
		geminiReq,
		clientStream,
		useUpstreamStream,
	)

	var resp *http.Response
	// checkedPolicy 是循环内对当前 resp 的错误策略检查结果，未检查时为 nil。
	var checkedPolicy *ErrorPolicyResult
	for attempt := 1; attempt <= geminiMaxRetries; attempt++ {
		upstreamReq, idHeader, err := buildReq(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			if failoverErr := s.handleVertexServiceAccountNotFound(ctx, c, account, err); failoverErr != nil {
				return nil, failoverErr
			}
			return nil, s.writeChatCompletionsError(c, http.StatusBadGateway, "upstream_error", err.Error())
		}
		requestIDHeader = idHeader

		resp, err = s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)
		if err != nil {
			return nil, s.handleUpstreamTransportError(ctx, c, account, err)
		}

		checked, rebuilt := s.checkErrorPolicyInLoop(ctx, account, resp, mappedModel)
		resp = rebuilt
		checkedPolicy = checked
		if checked != nil && *checked != ErrorPolicyNone {
			break
		}

		if resp.StatusCode >= 400 && s.shouldRetryGeminiUpstreamError(account, resp.StatusCode) {
			respBody := s.readUpstreamErrorBody(resp)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusForbidden && isGeminiInsufficientScope(resp.Header, respBody) {
				resp = &http.Response{
					StatusCode: resp.StatusCode,
					Header:     resp.Header.Clone(),
					Body:       io.NopCloser(bytes.NewReader(respBody)),
				}
				break
			}
			if resp.StatusCode == http.StatusTooManyRequests {
				s.handleGeminiUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody)
			}
			if attempt < geminiMaxRetries {
				upstreamReqID := resp.Header.Get(requestIDHeader)
				if upstreamReqID == "" {
					upstreamReqID = resp.Header.Get("x-goog-request-id")
				}
				upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(respBody))
				upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)
				appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
					ProxyID:            opsUpstreamProxyID(account),
					ProxyName:          opsUpstreamProxyName(account),
					Platform:           account.Platform,
					AccountID:          account.ID,
					AccountName:        account.Name,
					UpstreamStatusCode: resp.StatusCode,
					UpstreamRequestID:  upstreamReqID,
					Kind:               "retry",
					Message:            upstreamMsg,
				})
				logger.LegacyPrintf("service.gemini_chat_completions", "Gemini account %d: upstream status %d, retry %d/%d", account.ID, resp.StatusCode, attempt, geminiMaxRetries)
				sleepGeminiBackoff(attempt)
				continue
			}
			resp = &http.Response{
				StatusCode: resp.StatusCode,
				Header:     resp.Header.Clone(),
				Body:       io.NopCloser(bytes.NewReader(respBody)),
			}
			break
		}

		break
	}
	defer func() { _ = resp.Body.Close() }()

	requestID := resp.Header.Get(requestIDHeader)
	if requestID == "" {
		requestID = resp.Header.Get("x-goog-request-id")
	}
	if requestID != "" {
		c.Header("x-request-id", requestID)
	}

	reasoningEffort := extractGeminiReasoningEffortFromBody(geminiReq)

	if resp.StatusCode >= 400 {
		respBody := s.readUpstreamErrorBody(resp)
		policy := ErrorPolicyNone
		if s.rateLimitService != nil {
			policy = s.finalErrorPolicy(ctx, account, resp.StatusCode, respBody, mappedModel, checkedPolicy)
		}
		// 与 messages 兼容层一致：只有 None / Matched 才走账号状态处理。
		// Skipped（池模式、或自定义错误码未命中）与 TempUnscheduled 已由策略层裁决完毕。
		if policy == ErrorPolicyNone || policy == ErrorPolicyMatched {
			s.handleGeminiUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody)
		}
		evBody := unwrapIfNeeded(account.Type == AccountTypeOAuth, respBody)

		if s.shouldFailoverGeminiUpstreamError(resp.StatusCode) {
			upstreamMsg := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(evBody)))
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				ProxyID:            opsUpstreamProxyID(account),
				ProxyName:          opsUpstreamProxyName(account),
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: resp.StatusCode,
				UpstreamRequestID:  requestID,
				Kind:               "failover",
				Message:            upstreamMsg,
			})
			return nil, &UpstreamFailoverError{
				StatusCode:             resp.StatusCode,
				ResponseBody:           evBody,
				RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
			}
		}

		if policy == ErrorPolicySkipped && account.IsCustomErrorCodesEnabled() {
			return nil, s.writeGeminiCustomCodeSkippedError(c, account, resp.StatusCode, requestID, evBody, func() {
				_ = s.writeChatCompletionsError(c, http.StatusInternalServerError, "api_error", geminiCustomCodeSkippedClientMessage)
			})
		}
		return nil, s.writeGeminiChatCompletionsMappedError(c, account, resp.StatusCode, requestID, evBody)
	}

	var usage *ClaudeUsage
	var firstTokenMs *int
	clientDisconnect := false
	streamFinished := false
	if clientStream {
		streamRes, err := s.handleChatCompletionsStreamingResponseFromGemini(c, resp, startTime, originalModel, account.Type == AccountTypeOAuth, includeUsage)
		if err != nil {
			return nil, err
		}
		usage = streamRes.usage
		firstTokenMs = streamRes.firstTokenMs
		clientDisconnect = streamRes.clientDisconnect
		streamFinished = streamRes.finished
	} else if useUpstreamStream {
		collected, usageObj, err := collectGeminiSSE(resp.Body, account.Type == AccountTypeOAuth)
		if err != nil {
			return nil, s.writeChatCompletionsError(c, http.StatusBadGateway, "upstream_error", infraerrors.UpstreamStreamReadFailed)
		}
		collectedBytes, _ := json.Marshal(collected)
		chatResp, usageObj2, err := geminiResponseToChatCompletions(collected, originalModel, collectedBytes, usageObj)
		if err != nil {
			return nil, s.writeChatCompletionsError(c, http.StatusBadGateway, "upstream_error", infraerrors.UpstreamResponseParseFailed)
		}
		c.JSON(http.StatusOK, chatResp)
		usage = usageObj2
	} else {
		usageResp, err := s.handleChatCompletionsNonStreamingResponseFromGemini(c, resp, originalModel, account.Type == AccountTypeOAuth)
		if err != nil {
			return nil, err
		}
		usage = usageResp
	}

	if usage == nil {
		usage = &ClaudeUsage{}
	}

	imageCount := 0
	imageInputSize := s.extractImageInputSize(claudeBody)
	imageSize := normalizeOpenAIImageSizeTier(imageInputSize)
	if isImageGenerationModel(originalModel) {
		imageCount = 1
	}
	// 客户端在收到结束分片前断开时只计已送达的图片：Chat 流不向客户端转发图片。
	if clientDisconnect && !streamFinished {
		imageCount = 0
	}

	return &ForwardResult{
		RequestID:        requestID,
		UpstreamHeaders:  resp.Header,
		Usage:            *usage,
		Model:            originalModel,
		UpstreamModel:    mappedModel,
		Stream:           clientStream,
		Duration:         time.Since(startTime),
		FirstTokenMs:     firstTokenMs,
		ReasoningEffort:  reasoningEffort,
		ImageCount:       imageCount,
		ImageSize:        imageSize,
		ImageInputSize:   imageInputSize,
		ClientDisconnect: clientDisconnect,
	}, nil
}

func (s *GeminiMessagesCompatService) buildGeminiChatCompletionsUpstreamRequestFunc(
	account *Account,
	mappedModel string,
	geminiReq []byte,
	clientStream bool,
	useUpstreamStream bool,
) (func(context.Context) (*http.Request, string, error), string) {
	switch account.Type {
	case AccountTypeAPIKey:
		return func(ctx context.Context) (*http.Request, string, error) {
			apiKey := account.GetCredential("api_key")
			if strings.TrimSpace(apiKey) == "" {
				return nil, "", errors.New("gemini api_key not configured")
			}

			baseURL := account.GetGeminiBaseURL(geminicli.AIStudioBaseURL)
			normalizedBaseURL, err := s.validateUpstreamBaseURL(baseURL)
			if err != nil {
				return nil, "", err
			}

			action := "generateContent"
			if clientStream {
				action = "streamGenerateContent"
			}
			fullURL, err := buildGeminiAIStudioModelActionURL(normalizedBaseURL, mappedModel, action, clientStream)
			if err != nil {
				return nil, "", err
			}

			restGeminiReq := normalizeGeminiRequestForAIStudio(geminiReq)
			upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, bytes.NewReader(restGeminiReq))
			if err != nil {
				return nil, "", err
			}
			upstreamReq.Header.Set("Content-Type", "application/json")
			upstreamReq.Header.Set("x-goog-api-key", apiKey)
			return upstreamReq, "x-request-id", nil
		}, "x-request-id"

	case AccountTypeOAuth:
		return func(ctx context.Context) (*http.Request, string, error) {
			if s.tokenProvider == nil {
				return nil, "", errors.New("gemini token provider not configured")
			}
			accessToken, err := s.tokenProvider.GetAccessToken(ctx, account)
			if err != nil {
				return nil, "", err
			}

			projectID := strings.TrimSpace(account.GetCredential("project_id"))
			action := "generateContent"
			if useUpstreamStream {
				action = "streamGenerateContent"
			}

			if projectID != "" {
				baseURL, err := s.validateUpstreamBaseURL(geminicli.GeminiCliBaseURL)
				if err != nil {
					return nil, "", err
				}
				fullURL := fmt.Sprintf("%s/v1internal:%s", strings.TrimRight(baseURL, "/"), action)
				if useUpstreamStream {
					fullURL += "?alt=sse"
				}

				var inner any
				if err := json.Unmarshal(geminiReq, &inner); err != nil {
					return nil, "", fmt.Errorf("failed to parse gemini request: %w", err)
				}
				wrappedBytes, _ := json.Marshal(map[string]any{
					"model":   mappedModel,
					"project": projectID,
					"request": inner,
				})

				upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, bytes.NewReader(wrappedBytes))
				if err != nil {
					return nil, "", err
				}
				upstreamReq.Header.Set("Content-Type", "application/json")
				upstreamReq.Header.Set("Authorization", "Bearer "+accessToken)
				upstreamReq.Header.Set("User-Agent", geminicli.GeminiCLIUserAgent)
				return upstreamReq, "x-request-id", nil
			}

			baseURL := account.GetGeminiBaseURL(geminicli.AIStudioBaseURL)
			normalizedBaseURL, err := s.validateUpstreamBaseURL(baseURL)
			if err != nil {
				return nil, "", err
			}

			fullURL, err := buildGeminiAIStudioModelActionURL(normalizedBaseURL, mappedModel, action, useUpstreamStream)
			if err != nil {
				return nil, "", err
			}

			restGeminiReq := normalizeGeminiRequestForAIStudio(geminiReq)
			upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, bytes.NewReader(restGeminiReq))
			if err != nil {
				return nil, "", err
			}
			upstreamReq.Header.Set("Content-Type", "application/json")
			upstreamReq.Header.Set("Authorization", "Bearer "+accessToken)
			return upstreamReq, "x-request-id", nil
		}, "x-request-id"

	case AccountTypeServiceAccount:
		return func(ctx context.Context) (*http.Request, string, error) {
			action := "generateContent"
			if clientStream {
				action = "streamGenerateContent"
			}
			fullURL, err := buildVertexGeminiAccountURL(account, mappedModel, action, clientStream)
			if err != nil {
				return nil, "", err
			}

			restGeminiReq := normalizeGeminiRequestForAIStudio(geminiReq)
			upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, bytes.NewReader(restGeminiReq))
			if err != nil {
				return nil, "", err
			}
			upstreamReq.Header.Set("Content-Type", "application/json")
			if err := setVertexGeminiAuth(ctx, upstreamReq, account, s.tokenProvider); err != nil {
				return nil, "", err
			}
			return upstreamReq, "x-request-id", nil
		}, "x-request-id"

	default:
		return func(context.Context) (*http.Request, string, error) {
			return nil, "", fmt.Errorf("unsupported account type: %s", account.Type)
		}, "x-request-id"
	}
}

func (s *GeminiMessagesCompatService) handleChatCompletionsNonStreamingResponseFromGemini(
	c *gin.Context,
	resp *http.Response,
	originalModel string,
	isOAuth bool,
) (*ClaudeUsage, error) {
	respBody, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		return nil, err
	}
	if isOAuth {
		if unwrappedBody, uwErr := unwrapGeminiResponse(respBody); uwErr == nil {
			respBody = unwrappedBody
		}
	}

	var geminiResp map[string]any
	if err := json.Unmarshal(respBody, &geminiResp); err != nil {
		return nil, s.writeChatCompletionsError(c, http.StatusBadGateway, "upstream_error", infraerrors.UpstreamResponseParseFailed)
	}

	chatResp, usage, err := geminiResponseToChatCompletions(geminiResp, originalModel, respBody, nil)
	if err != nil {
		return nil, s.writeChatCompletionsError(c, http.StatusBadGateway, "upstream_error", infraerrors.UpstreamResponseParseFailed)
	}

	responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	c.JSON(http.StatusOK, chatResp)
	return usage, nil
}

func geminiResponseToChatCompletions(
	geminiResp map[string]any,
	originalModel string,
	rawData []byte,
	usageOverride *ClaudeUsage,
) (*apicompat.ChatCompletionsResponse, *ClaudeUsage, error) {
	claudeRespMap, usage := convertGeminiToClaudeMessage(geminiResp, originalModel, rawData, true)
	if usageOverride != nil && (usageOverride.InputTokens > 0 || usageOverride.OutputTokens > 0 || usageOverride.CacheReadInputTokens > 0) {
		usage = usageOverride
		if usageMap, ok := claudeRespMap["usage"].(map[string]any); ok {
			usageMap["input_tokens"] = usage.InputTokens
			usageMap["output_tokens"] = usage.OutputTokens
			usageMap["cache_read_input_tokens"] = usage.CacheReadInputTokens
		}
	}

	claudeBytes, err := json.Marshal(claudeRespMap)
	if err != nil {
		return nil, nil, err
	}
	var anthropicResp apicompat.AnthropicResponse
	if err := json.Unmarshal(claudeBytes, &anthropicResp); err != nil {
		return nil, nil, err
	}
	responsesResp := apicompat.AnthropicToResponsesResponse(&anthropicResp)
	return apicompat.ResponsesToChatCompletions(responsesResp, originalModel), usage, nil
}

func (s *GeminiMessagesCompatService) handleChatCompletionsStreamingResponseFromGemini(
	c *gin.Context,
	resp *http.Response,
	startTime time.Time,
	originalModel string,
	isOAuth bool,
	includeUsage bool,
) (*geminiStreamResult, error) {
	if s.responseHeaderFilter != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	}
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return nil, errors.New("streaming not supported")
	}

	anthState := apicompat.NewAnthropicEventToResponsesState()
	anthState.Model = originalModel
	ccState := apicompat.NewResponsesEventToChatState()
	ccState.Model = originalModel
	ccState.IncludeUsage = includeUsage

	var usage ClaudeUsage
	var firstTokenMs *int
	firstChunk := true
	finishReason := ""
	clientDisconnect := false
	streamResult := func() *geminiStreamResult {
		return &geminiStreamResult{usage: &usage, firstTokenMs: firstTokenMs, clientDisconnect: clientDisconnect, finished: finishReason != ""}
	}

	writeChatChunk := func(chunk apicompat.ChatCompletionsChunk) bool {
		sse, err := apicompat.ChatChunkToSSE(chunk)
		if err != nil {
			return false
		}
		if _, err := io.WriteString(c.Writer, sse); err != nil {
			clientDisconnect = true
			return true
		}
		return false
	}

	emitAnthropicEvent := func(evt *apicompat.AnthropicStreamEvent) bool {
		responsesEvents := apicompat.AnthropicEventToResponsesEvents(evt, anthState)
		for _, resEvt := range responsesEvents {
			chunks := apicompat.ResponsesEventToChatChunks(&resEvt, ccState)
			for _, chunk := range chunks {
				if disconnected := writeChatChunk(chunk); disconnected {
					return true
				}
			}
		}
		flusher.Flush()
		return false
	}

	messageID := generateAnthropicMsgID()
	if emitAnthropicEvent(&apicompat.AnthropicStreamEvent{
		Type: "message_start",
		Message: &apicompat.AnthropicResponse{
			ID:         messageID,
			Type:       "message",
			Role:       "assistant",
			Model:      originalModel,
			Content:    []apicompat.AnthropicContentBlock{},
			StopReason: nil, // JSON null
			Usage:      apicompat.AnthropicUsage{},
		},
	}) {
		return streamResult(), nil
	}

	sawToolUse := false
	nextBlockIndex := 0
	openBlockIndex := -1
	openBlockType := ""
	seenText := ""
	seenThought := ""
	openToolIndex := -1
	openToolName := ""
	seenToolJSON := ""

	closeOpenBlock := func() bool {
		if openBlockIndex < 0 {
			return false
		}
		disconnected := emitAnthropicEvent(&apicompat.AnthropicStreamEvent{Type: "content_block_stop"})
		openBlockIndex = -1
		openBlockType = ""
		return disconnected
	}
	closeOpenTool := func() bool {
		if openToolIndex < 0 {
			return false
		}
		disconnected := emitAnthropicEvent(&apicompat.AnthropicStreamEvent{Type: "content_block_stop"})
		openToolIndex = -1
		openToolName = ""
		seenToolJSON = ""
		return disconnected
	}

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(trimmed, "data:") {
				payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
				if payload != "" && payload != "[DONE]" {
					rawBytes := []byte(payload)
					if isOAuth {
						if innerBytes, uwErr := unwrapGeminiResponse(rawBytes); uwErr == nil {
							rawBytes = innerBytes
						}
					}

					var geminiResp map[string]any
					if err := json.Unmarshal(rawBytes, &geminiResp); err == nil {
						if firstChunk {
							firstChunk = false
							ms := int(time.Since(startTime).Milliseconds())
							firstTokenMs = &ms
						}
						if fr := extractGeminiFinishReason(geminiResp); fr != "" {
							finishReason = fr
						}
						if u := extractGeminiUsage(rawBytes); u != nil {
							usage = *u
						}

						for _, part := range extractGeminiParts(geminiResp) {
							if text, ok := part["text"].(string); ok && isGeminiThoughtPart(part) {
								delta, newSeen := computeGeminiTextDelta(seenThought, text)
								seenThought = newSeen
								if delta == "" {
									continue
								}
								if openToolIndex >= 0 {
									if closeOpenTool() {
										return streamResult(), nil
									}
								}
								if openBlockType != "thinking" {
									if closeOpenBlock() {
										return streamResult(), nil
									}
									idx := nextBlockIndex
									nextBlockIndex++
									openBlockIndex = idx
									openBlockType = "thinking"
									if emitAnthropicEvent(&apicompat.AnthropicStreamEvent{
										Type:         "content_block_start",
										Index:        &idx,
										ContentBlock: &apicompat.AnthropicContentBlock{Type: "thinking"},
									}) {
										return streamResult(), nil
									}
								}
								if emitAnthropicEvent(&apicompat.AnthropicStreamEvent{
									Type:  "content_block_delta",
									Delta: &apicompat.AnthropicDelta{Type: "thinking_delta", Thinking: delta},
								}) {
									return streamResult(), nil
								}
								continue
							}
							if text, ok := part["text"].(string); ok && text != "" {
								if openToolIndex >= 0 {
									if closeOpenTool() {
										return streamResult(), nil
									}
								}
								delta, newSeen := computeGeminiTextDelta(seenText, text)
								seenText = newSeen
								if delta == "" {
									continue
								}
								if openBlockType != "text" {
									if closeOpenBlock() {
										return streamResult(), nil
									}
									idx := nextBlockIndex
									nextBlockIndex++
									openBlockIndex = idx
									openBlockType = "text"
									if emitAnthropicEvent(&apicompat.AnthropicStreamEvent{
										Type:  "content_block_start",
										Index: &idx,
										ContentBlock: &apicompat.AnthropicContentBlock{
											Type: "text",
											Text: "",
										},
									}) {
										return streamResult(), nil
									}
								}
								if emitAnthropicEvent(&apicompat.AnthropicStreamEvent{
									Type: "content_block_delta",
									Delta: &apicompat.AnthropicDelta{
										Type: "text_delta",
										Text: delta,
									},
								}) {
									return streamResult(), nil
								}
								continue
							}

							if fc, ok := part["functionCall"].(map[string]any); ok && fc != nil {
								name, _ := fc["name"].(string)
								if strings.TrimSpace(name) == "" {
									name = "tool"
								}
								if closeOpenBlock() {
									return streamResult(), nil
								}
								if openToolIndex >= 0 && openToolName != name {
									if closeOpenTool() {
										return streamResult(), nil
									}
								}
								if openToolIndex < 0 {
									idx := nextBlockIndex
									nextBlockIndex++
									openToolIndex = idx
									openToolName = name
									sawToolUse = true
									if emitAnthropicEvent(&apicompat.AnthropicStreamEvent{
										Type:  "content_block_start",
										Index: &idx,
										ContentBlock: &apicompat.AnthropicContentBlock{
											Type:  "tool_use",
											ID:    "toolu_" + randomHex(8),
											Name:  name,
											Input: json.RawMessage(`{}`),
										},
									}) {
										return streamResult(), nil
									}
								}

								argsJSONText := "{}"
								switch v := fc["args"].(type) {
								case nil:
								case string:
									if strings.TrimSpace(v) != "" {
										argsJSONText = v
									}
								default:
									if b, err := json.Marshal(v); err == nil && len(b) > 0 {
										argsJSONText = string(b)
									}
								}
								delta, newSeen := computeGeminiTextDelta(seenToolJSON, argsJSONText)
								seenToolJSON = newSeen
								if delta != "" {
									if emitAnthropicEvent(&apicompat.AnthropicStreamEvent{
										Type: "content_block_delta",
										Delta: &apicompat.AnthropicDelta{
											Type:        "input_json_delta",
											PartialJSON: delta,
										},
									}) {
										return streamResult(), nil
									}
								}
							}
						}
					}
				}
			}
		}

		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if geminiStreamAbortedWithBillableOutput(c, err, &usage) {
				logger.LegacyPrintf("service.gemini_messages_compat", "[GeminiAPI] Client disconnected during chat completions stream, billing collected usage: %v", err)
				clientDisconnect = true
				return streamResult(), nil
			}
			return nil, fmt.Errorf("stream read error: %w", err)
		}
	}

	if closeOpenBlock() {
		return streamResult(), nil
	}
	if closeOpenTool() {
		return streamResult(), nil
	}

	stopReason := mapGeminiFinishReasonToClaudeStopReason(finishReason)
	if sawToolUse {
		stopReason = "tool_use"
	}
	anthState.InputTokens = usage.InputTokens
	anthState.CacheReadInputTokens = usage.CacheReadInputTokens
	if emitAnthropicEvent(&apicompat.AnthropicStreamEvent{
		Type: "message_delta",
		Delta: &apicompat.AnthropicDelta{
			Type:       "message_delta",
			StopReason: stopReason,
		},
		Usage: &apicompat.AnthropicUsage{
			InputTokens:          usage.InputTokens,
			OutputTokens:         usage.OutputTokens,
			CacheReadInputTokens: usage.CacheReadInputTokens,
		},
	}) {
		return streamResult(), nil
	}
	if emitAnthropicEvent(&apicompat.AnthropicStreamEvent{Type: "message_stop"}) {
		return streamResult(), nil
	}

	for _, resEvt := range apicompat.FinalizeAnthropicResponsesStream(anthState) {
		chunks := apicompat.ResponsesEventToChatChunks(&resEvt, ccState)
		for _, chunk := range chunks {
			if disconnected := writeChatChunk(chunk); disconnected {
				return streamResult(), nil
			}
		}
	}
	for _, chunk := range apicompat.FinalizeResponsesChatStream(ccState) {
		if disconnected := writeChatChunk(chunk); disconnected {
			return streamResult(), nil
		}
	}

	_, _ = io.WriteString(c.Writer, "data: [DONE]\n\n")
	flusher.Flush()

	return streamResult(), nil
}

func (s *GeminiMessagesCompatService) writeGeminiChatCompletionsMappedError(
	c *gin.Context,
	account *Account,
	upstreamStatus int,
	upstreamRequestID string,
	body []byte,
) error {
	upstreamMsg := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(body)))
	setOpsUpstreamError(c, upstreamStatus, upstreamMsg, "")
	if account != nil {
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			ProxyID:            opsUpstreamProxyID(account),
			ProxyName:          opsUpstreamProxyName(account),
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: upstreamStatus,
			UpstreamRequestID:  upstreamRequestID,
			Kind:               "http_error",
			Message:            upstreamMsg,
		})
	}

	if status, errType, errMsg, matched := applyErrorPassthroughRule(
		c,
		PlatformGemini,
		upstreamStatus,
		body,
		http.StatusBadGateway,
		"upstream_error",
		infraerrors.UpstreamRequestFailed,
	); matched {
		return s.writeChatCompletionsError(c, status, errType, errMsg)
	}

	statusCode := http.StatusBadGateway
	errType := "upstream_error"
	errMsg := infraerrors.UpstreamRequestFailed
	if mapped := mapGeminiErrorBodyToClaudeError(body); mapped != nil {
		if mapped.Type != "" {
			errType = mapped.Type
		}
		if mapped.Message != "" {
			errMsg = mapped.Message
		}
		if mapped.StatusCode > 0 {
			statusCode = mapped.StatusCode
		}
	}

	switch upstreamStatus {
	case http.StatusBadRequest:
		if statusCode == http.StatusBadGateway {
			statusCode = http.StatusBadRequest
		}
		if errType == "upstream_error" {
			errType = "invalid_request_error"
		}
		// 400 是确定性的请求错误：回传上游 message（已脱敏），客户端据此定位非法字段。
		if errMsg == infraerrors.UpstreamRequestFailed {
			if upstreamMsg != "" {
				errMsg = upstreamMsg
			} else {
				errMsg = infraerrors.InvalidRequest
			}
		}
	case http.StatusNotFound:
		statusCode = http.StatusNotFound
		if errType == "upstream_error" {
			errType = "not_found_error"
		}
		if errMsg == infraerrors.UpstreamRequestFailed {
			errMsg = infraerrors.ResourceNotFound
		}
	case http.StatusTooManyRequests:
		statusCode = http.StatusTooManyRequests
		if errType == "upstream_error" {
			errType = "rate_limit_error"
		}
		if errMsg == infraerrors.UpstreamRequestFailed {
			errMsg = infraerrors.UpstreamRateLimited
		}
	case 529:
		statusCode = http.StatusServiceUnavailable
		if errType == "upstream_error" {
			errType = "overloaded_error"
		}
		if errMsg == infraerrors.UpstreamRequestFailed {
			errMsg = infraerrors.UpstreamOverloaded
		}
	}

	if upstreamMsg != "" && errMsg == infraerrors.UpstreamRequestFailed {
		errMsg = upstreamMsg
	}
	return s.writeChatCompletionsError(c, statusCode, errType, errMsg)
}

// writeCursorChatCompletionsError 把 Cursor 执行错误写成 Chat Completions 形状的客户端错误。
//
// Cursor 账号且 CursorUserFacingError 命中时按其状态码 + 中文提示写回；响应已提交或已写出
// 时不走新逻辑（不在已提交响应后重写状态码），沿原写法收尾。
func (s *GeminiMessagesCompatService) writeCursorChatCompletionsError(c *gin.Context, account *Account, err error) error {
	if !IsResponseCommitted(c) && !c.Writer.Written() {
		if status, msg, ok := cursorCompatUserError(c, account, err); ok {
			errType := "upstream_error"
			if status >= 400 && status < 500 {
				errType = "invalid_request_error"
			}
			return s.writeChatCompletionsError(c, status, errType, msg)
		}
	}
	return s.writeChatCompletionsError(c, cursorCompatErrorStatus(), "upstream_error", err.Error())
}

func (s *GeminiMessagesCompatService) writeChatCompletionsError(c *gin.Context, status int, errType, message string) error {
	c.JSON(status, gin.H{
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
	return fmt.Errorf("%s", message)
}
