package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// ChatCompletions handles OpenAI Chat Completions API endpoint for Anthropic platform groups.
// POST /v1/chat/completions
// This converts Chat Completions requests to Anthropic format (via Responses format chain),
// forwards to Anthropic upstream, and converts responses back to Chat Completions format.
func (h *GatewayHandler) ChatCompletions(c *gin.Context) {
	streamStarted := false

	requestStart := time.Now()

	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.chatCompletionsErrorResponse(c, http.StatusUnauthorized, "authentication_error", infraerrors.InvalidAPIKey)
		return
	}

	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.chatCompletionsErrorResponse(c, http.StatusInternalServerError, "api_error", infraerrors.UserContextNotFound)
		return
	}
	reqLog := requestLogger(
		c,
		"handler.gateway.chat_completions",
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Any("group_id", apiKey.GroupID),
	)

	// Read request body
	body, err := readLenientJSONRequestBodyWithPrealloc(c.Request, h.cfg)
	if err != nil {
		if maxErr, ok := extractMaxBytesError(err); ok {
			h.chatCompletionsErrorResponse(c, http.StatusRequestEntityTooLarge, "invalid_request_error", buildBodyTooLargeMessage(maxErr.Limit))
			return
		}
		h.chatCompletionsErrorResponse(c, http.StatusBadRequest, "invalid_request_error", infraerrors.RequestBodyReadFailed)
		return
	}

	if len(body) == 0 {
		h.chatCompletionsErrorResponse(c, http.StatusBadRequest, "invalid_request_error", infraerrors.RequestBodyEmpty)
		return
	}

	setOpsRequestContext(c, "", false)

	// Validate JSON
	if !gjson.ValidBytes(body) {
		logRequestBodyParseFailure(reqLog, body, nil)
		h.chatCompletionsErrorResponse(c, http.StatusBadRequest, "invalid_request_error", infraerrors.RequestBodyParseFailed)
		return
	}

	// Extract model and stream
	modelResult := gjson.GetBytes(body, "model")
	if !modelResult.Exists() || modelResult.Type != gjson.String || modelResult.String() == "" {
		h.chatCompletionsErrorResponse(c, http.StatusBadRequest, "invalid_request_error", infraerrors.ModelRequired)
		return
	}
	reqModel := modelResult.String()
	bindRequestedReasoningEffort(c, body, reqModel)
	ensureCompositeTargetPlatform(c, apiKey, reqModel, h.gatewayService)
	if !compositeTargetPlatformResolved(c, apiKey, reqModel, h.gatewayService) {
		h.chatCompletionsErrorResponse(c, http.StatusBadRequest, "invalid_request_error", infraerrors.ModelNotSupportedByComposite)
		return
	}
	if rejectSystemOneOnlyPlatform(c, apiKey, h.chatCompletionsErrorResponse) {
		return
	}
	reqStream, ok := parseOpenAICompatibleStream(body)
	if !ok {
		h.chatCompletionsErrorResponse(c, http.StatusBadRequest, "invalid_request_error", invalidStreamFieldTypeMessage)
		return
	}
	if service.IsGPTImageGenerationModel(reqModel) {
		h.chatCompletionsErrorResponse(c, http.StatusBadRequest, "invalid_request_error", infraerrors.ModelNotSupportedOnChatCompletio)
		return
	}
	reqLog = reqLog.With(zap.String("model", reqModel), zap.Bool("stream", reqStream))

	setOpsRequestContext(c, reqModel, reqStream)
	setOpsEndpointContext(c, "", int16(service.RequestTypeFromLegacy(reqStream, false)))
	pricingCtx, pricingAt := service.WithGatewayTokenRequestPricing(c.Request.Context())
	c.Request = c.Request.WithContext(pricingCtx)

	// 解析渠道级模型映射
	channelMapping, _ := h.gatewayService.ResolveChannelMappingAndRestrict(c.Request.Context(), apiKey.GroupID, reqModel)
	// 将渠道映射后的上游模型名下传 ctx，供调度门按映射名判定模型级限流（详见 service.WithChannelMappedModel）。
	if channelMapping.Mapped && channelMapping.MappedModel != reqModel {
		c.Request = c.Request.WithContext(service.WithChannelMappedModel(c.Request.Context(), channelMapping.MappedModel))
	}

	// Claude Code only restriction: /v1/chat/completions is never a Claude Code
	// endpoint. With a fallback group the request continues and account selection
	// (checkClaudeCodeRestriction) schedules it in the fallback group; without one
	// it is rejected here.
	if apiKey.Group != nil && apiKey.Group.ClaudeCodeOnly && apiKey.Group.FallbackGroupID == nil {
		h.chatCompletionsErrorResponse(c, http.StatusForbidden, "permission_error",
			"This group is restricted to Claude Code clients (/v1/messages only)")
		return
	}

	if decision := h.checkSecurityAudit(c, reqLog, apiKey, subject, service.ContentModerationProtocolOpenAIChat, reqModel, body); decision != nil && !decision.AllowNextStage {
		h.openAISecurityAuditError(c, decision)
		return
	}

	// Error passthrough binding
	if h.errorPassthroughService != nil {
		service.BindErrorPassthroughService(c, h.errorPassthroughService)
	}

	subscription, _ := middleware2.GetSubscriptionFromContext(c)

	service.SetOpsLatencyMs(c, service.OpsAuthLatencyMsKey, time.Since(requestStart).Milliseconds())

	// CF524 G2b 入口安装点：认证/鉴权完成后、并发槽位等待与账号选择前，创建请求级
	// 不可变预算快照（入口单调时间 requestStart）并安装请求级心跳 owner；出口必然 Stop。
	cf524StopHeartbeat := h.cf524InstallUpstreamBudgetAndHeartbeat(c, requestStart, reqStream)
	defer cf524StopHeartbeat()

	scope := resolveConcurrencyScope(c, subject, subscription)

	// §25.2/§25.14-D：续轮识别与整批接管必须发生在**用户槽/预留/选号之前**。
	if serveCursorResumeIfRequested(c, h.gatewayService, service.CursorResumeProtocolChat, body,
		cursorResumeIdentity(apiKey, subject.UserID), reqStream, func(status int, message string) {
			h.chatCompletionsErrorResponse(c, status, "invalid_request_error", message)
		}) {
		return
	}

	userReleaseFunc, err := h.concurrencyHelper.AcquireScopedUserSlotWithWait(c, scope, subject.UserID, reqStream, &streamStarted)
	if err != nil {
		reqLog.Warn("gateway.cc.user_slot_acquire_failed", zap.Error(err))
		h.handleConcurrencyError(c, err, scope.Kind, streamStarted)
		return
	}
	// §25.4：用户槽改为**可转交**句柄（Cursor 分支交给会话持有，其它供应商语义不变）。
	userSlot := newTransferableRelease(c.Request.Context(), "user_slot", userReleaseFunc)
	if userSlot != nil {
		userReleaseFunc = userSlot.Release
		defer userSlot.Release()
	}

	// 2. Re-check billing
	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
		reqLog.Info("gateway.cc.billing_check_failed", zap.Error(err))
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.chatCompletionsErrorResponse(c, status, code, message)
		return
	}

	// 余额模式在途预留：防止并发请求在预检时看到同一份余额而集体透支。
	inflightHandle, err := reserveTransferableInflightBalance(c, h.billingCacheService, h.gatewayService, apiKey, subscription, tokenInflightEstimate(reqModel, body))
	if err != nil {
		reqLog.Info("gateway.cc.inflight_reservation_rejected", zap.Error(err))
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.chatCompletionsErrorResponse(c, status, code, message)
		return
	}
	if inflightHandle != nil {
		defer inflightHandle.Release()
	}

	// Parse request for session hash
	bodyRef := service.NewRequestBodyRef(body)
	parsedReq, _ := service.ParseGatewayRequest(bodyRef, "chat_completions")
	if parsedReq == nil {
		parsedReq = &service.ParsedRequest{Model: reqModel, Stream: reqStream, Body: bodyRef}
	}
	parsedReq.SessionContext = &service.SessionContext{
		ClientIP:  ip.GetClientIP(c),
		UserAgent: c.GetHeader("User-Agent"),
		APIKeyID:  apiKey.ID,
	}
	sessionHash := h.gatewayService.GenerateSessionHash(parsedReq)
	groupPlatform := effectiveAPIKeyPlatform(c, apiKey)
	selectionSessionHash := sessionHash
	if groupPlatform == service.PlatformGemini && selectionSessionHash != "" {
		selectionSessionHash = "gemini:" + selectionSessionHash
	}
	// 3. Account selection + failover loop
	fs := newChatCompletionsFailoverState(groupPlatform, h.maxAccountSwitchesGemini)

	for {
		if failoverClientGone(c) {
			return
		}
		selection, err := h.gatewayService.SelectAccountWithLoadAwareness(c.Request.Context(), apiKey.GroupID, selectionSessionHash, reqModel, fs.FailedAccountIDs, "", int64(0))
		if err != nil {
			if failoverClientGone(c) {
				reqLog.Info("gateway.cc.account_select_aborted_client_disconnected", zap.Error(err))
				return
			}
			if len(fs.FailedAccountIDs) == 0 {
				cls := classifyNoAccountErrorFromGin(c, h.gatewayService, apiKey, reqModel, reqModel, groupPlatform)
				cls = classifySelectionFailureError(err, cls)
				if !cls.ModelNotFound {
					markOpsRoutingCapacityLimitedIfNoAvailable(c, err)
				}
				message := cls.Message
				h.chatCompletionsErrorResponse(c, cls.Status, cls.ErrType, message)
				return
			}
			action := fs.HandleSelectionExhausted(c.Request.Context())
			switch action {
			case FailoverContinue:
				continue
			case FailoverCanceled:
				failoverClientGone(c)
				return
			default:
				if fs.LastFailoverErr != nil {
					h.handleCCFailoverExhausted(c, fs.LastFailoverErr, streamStarted)
				} else {
					h.chatCompletionsErrorResponse(c, http.StatusBadGateway, "server_error", infraerrors.AllAccountsExhausted)
				}
				return
			}
		}
		account := selection.Account
		setOpsSelectedAccount(c, account.ID, account.Platform)

		// 4. Acquire account concurrency slot
		accountReleaseFunc := selection.ReleaseFunc
		if !selection.Acquired {
			if selection.WaitPlan == nil {
				markOpsRoutingCapacityLimited(c)
				h.chatCompletionsErrorResponse(c, http.StatusServiceUnavailable, "api_error", infraerrors.NoAvailableAccounts)
				return
			}
			accountReleaseFunc, err = h.concurrencyHelper.AcquireAccountSlotWithWaitTimeout(
				c,
				account.ID,
				selection.WaitPlan.MaxConcurrency,
				selection.WaitPlan.Timeout,
				reqStream,
				&streamStarted,
			)
			if err != nil {
				reqLog.Warn("gateway.cc.account_slot_acquire_failed", zap.Int64("account_id", account.ID), zap.Error(err))
				h.handleConcurrencyError(c, err, "account", streamStarted)
				return
			}
		}
		// 终检与准入后绑定使用选号结果携带的门（见 responses 同名注释）。
		admissionCtx := service.ContextWithSelectionProfitGate(c.Request.Context(), selection)
		latest, vetoed, reason := h.gatewayService.GatewayProfitControlVetoLatest(admissionCtx, account)
		if vetoed {
			if accountReleaseFunc != nil {
				accountReleaseFunc()
			}
			reqLog.Debug("gateway.cc.account_slot_profit_vetoed", zap.Int64("account_id", account.ID), zap.String("reason", reason))
			if fs.RecordProfitVeto(account.ID) == FailoverExhausted {
				reqLog.Warn("gateway.cc.profit_veto_attempts_exhausted", zap.Int("profit_veto_count", fs.ProfitVetoCount()))
				h.chatCompletionsErrorResponse(c, http.StatusServiceUnavailable, "api_error", profitVetoExhaustedMessage)
				return
			}
			continue
		}
		account = latest
		selection.Account = latest
		if selection.ProfitGateActive() {
			if err := h.gatewayService.BindStickySessionAfterProfitAdmission(admissionCtx, apiKey.GroupID, selectionSessionHash, account.ID); err != nil {
				reqLog.Warn("gateway.cc.bind_sticky_session_after_profit_admission_failed", zap.Int64("account_id", account.ID), zap.Error(err))
			}
		}
		// §25.4：账号槽同样改为**可转交**句柄（Cursor 分支交给会话持有）。
		accountSlot := newTransferableRelease(c.Request.Context(), "account_slot", accountReleaseFunc)
		if accountSlot != nil {
			accountReleaseFunc = accountSlot.Release
		}

		if groupPlatform == service.PlatformGemini && account.Platform != service.PlatformGemini && account.Platform != service.PlatformCursor {
			if accountReleaseFunc != nil {
				accountReleaseFunc()
			}
			fs.FailedAccountIDs[account.ID] = struct{}{}
			continue
		}

		// 5. Forward request
		// CF524（外审修复）：心跳基线须与 writerSizeBeforeForward 同时点捕获——判定用
		// 增量严格相等，前置心跳帧不会被重复计入（与 gateway_handler.go 同型）。
		writerSizeBeforeForward := c.Writer.Size()
		hbCommittedBaseline := 0
		if hbOwner, ok := service.UpstreamHeartbeatFromContext(c.Request.Context()); ok && hbOwner != nil {
			hbCommittedBaseline = int(hbOwner.CommittedBytes())
		}
		forwardBody := body
		if channelMapping.Mapped {
			forwardBody = h.gatewayService.ReplaceModelInBody(body, channelMapping.MappedModel)
		}
		// §25.4/§25.5：Cursor 账号把本 HTTP 已获取的用户槽/账号槽/余额预留转交给会话，
		// 并把本 HTTP 的计费任务交给会话唯一结算；非 Cursor 账号不做任何转交。
		if isCursorPlatformAccount(account) {
			handover := newCursorResourceHandover(
				cursorHandoverResources(userSlot, accountSlot, inflightHandle),
				h.submitMandatoryUsageRecordTask,
			)
			c.Request = c.Request.WithContext(bindCursorHandoverForAttempt(c.Request.Context(), account, handover))
		}
		var result *service.ForwardResult
		setActualUpstreamEndpoint(c, "")
		if account.Platform == service.PlatformGemini {
			if h.geminiCompatService == nil {
				h.chatCompletionsErrorResponse(c, http.StatusBadGateway, "upstream_error", infraerrors.CompatServiceNotConfigured)
				if accountReleaseFunc != nil {
					accountReleaseFunc()
				}
				return
			}
			result, err = h.geminiCompatService.ForwardAsChatCompletions(c.Request.Context(), c, account, forwardBody)
		} else if shouldUseAntigravityCompat(account) {
			if h.antigravityGatewayService == nil {
				h.chatCompletionsErrorResponse(c, http.StatusBadGateway, "upstream_error", infraerrors.CompatServiceNotConfigured)
				if accountReleaseFunc != nil {
					accountReleaseFunc()
				}
				return
			}
			setActualUpstreamEndpoint(c, EndpointAntigravityGenerateContent)
			result, err = h.antigravityGatewayService.ForwardAsChatCompletions(c.Request.Context(), c, account, forwardBody, parsedReq)
		} else {
			result, err = h.gatewayService.ForwardAsChatCompletions(c.Request.Context(), c, account, forwardBody, parsedReq)
		}

		if accountReleaseFunc != nil {
			accountReleaseFunc()
		}

		if err != nil {
			var failoverErr *service.UpstreamFailoverError
			if errors.As(err, &failoverErr) {
				// CF524（v5）：心跳注释帧非语义写出——防线改用心跳基线例外判定，
				// 心跳已提交时仍允许换号（无心跳时判定与现状完全一致）。
				if !heartbeatFailoverGuardStillClean(c, writerSizeBeforeForward, hbCommittedBaseline) {
					// 2026-10-20 实证：容量降载与账号强相关（同负载 231 零降载），
					// 中段流失败终止后客户端以同一 session_id 立刻重试，须清粘性+LRU 降权让其落到健康账号。
					if failoverErr != nil && failoverErr.IsOpenAICapacityShed() {
						h.openAIGatewayService.PenalizeSessionAccountForCapacityShed(c.Request.Context(), apiKey.GroupID, selectionSessionHash, account.ID)
					}
					h.handleCCFailoverExhausted(c, failoverErr, true)
					return
				}
				action := fs.HandleFailoverError(c.Request.Context(), h.gatewayService, account.ID, account.Platform, account.GetPoolModeRetryCount(), failoverErr)
				switch action {
				case FailoverContinue:
					continue
				case FailoverExhausted:
					// 2026-10-20 实证：容量降载与账号强相关（同负载 231 零降载），
					// 跨请求必须换号——清粘性+LRU 降权让客户端立刻重试落到健康账号。
					if fs.LastFailoverErr != nil && fs.LastFailoverErr.IsOpenAICapacityShed() {
						h.openAIGatewayService.PenalizeSessionAccountForCapacityShed(c.Request.Context(), apiKey.GroupID, selectionSessionHash, account.ID)
					}
					h.handleCCFailoverExhausted(c, fs.LastFailoverErr, streamStarted)
					return
				case FailoverCanceled:
					failoverClientGone(c)
					return
				}
			}
			upstreamErrorAlreadyCommunicated := gatewayForwardErrorAlreadyCommunicated(c, writerSizeBeforeForward, err)
			wroteFallback := false
			if !upstreamErrorAlreadyCommunicated {
				wroteFallback = h.ensureForwardErrorResponse(c, streamStarted)
			}
			reqLog.Error("gateway.cc.forward_failed",
				zap.Int64("account_id", account.ID),
				zap.Bool("fallback_error_response_written", wroteFallback),
				zap.Bool("upstream_error_response_already_written", upstreamErrorAlreadyCommunicated),
				zap.Error(err),
			)
			return
		}

		// 6. Record usage
		userAgent := c.GetHeader("User-Agent")
		clientIP := ip.GetClientIP(c)
		requestPayloadHash := service.HashUsageRequestPayload(body)
		inboundEndpoint := GetInboundEndpoint(c)
		upstreamEndpoint := GetUpstreamEndpoint(c, account.Platform)

		quotaPlatform := service.QuotaPlatform(c.Request.Context(), apiKey)
		sessionID := service.ExtractClientSessionID(c)
		stampForwardRequestedReasoningEffort(result, service.RequestedReasoningEffortFromContext(c.Request.Context()))
		h.submitUsageRecordTask(c.Request.Context(), func(ctx context.Context) {
			if err := h.gatewayService.RecordUsage(ctx, &service.RecordUsageInput{
				Result:             result,
				QuotaPlatform:      quotaPlatform,
				APIKey:             apiKey,
				User:               apiKey.User,
				Account:            account,
				Subscription:       subscription,
				PricingAt:          pricingAt,
				InboundEndpoint:    inboundEndpoint,
				UpstreamEndpoint:   upstreamEndpoint,
				UserAgent:          userAgent,
				IPAddress:          clientIP,
				RequestPayloadHash: requestPayloadHash,
				APIKeyService:      h.apiKeyService,
				SessionID:          sessionID,
				ChannelUsageFields: clientRequestedUsageFields(c, channelMapping, reqModel, result.UpstreamModel),
			}); err != nil {
				reqLog.Error("gateway.cc.record_usage_failed",
					zap.Int64("account_id", account.ID),
					zap.Error(err),
				)
			}
		})
		return
	}
}

// newChatCompletionsFailoverState 构造 ChatCompletions 入口的 failover 状态：
// gemini 平台使用带固定切换上限的 NewFailoverStateCapped（默认 3、配置
// max_account_switches_gemini 覆盖，闸②终审整改恢复上限）；其余平台使用无
// 固定上限的 NewFailoverState（候选集合边界判定耗尽）。
func newChatCompletionsFailoverState(groupPlatform string, maxSwitchesGemini int) *FailoverState {
	if groupPlatform == service.PlatformGemini {
		return NewFailoverStateCapped(maxSwitchesGemini, false)
	}
	return NewFailoverState(false)
}

// chatCompletionsErrorResponse writes an error in OpenAI Chat Completions format.
func (h *GatewayHandler) chatCompletionsErrorResponse(c *gin.Context, status int, errType, message string) {
	c.JSON(status, gin.H{
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}

// handleCCFailoverExhausted writes a failover-exhausted error in CC format.
func (h *GatewayHandler) handleCCFailoverExhausted(c *gin.Context, lastErr *service.UpstreamFailoverError, streamStarted bool) {
	if streamStarted {
		return
	}
	if lastErr != nil {
		copyFailoverRetryAfter(c, lastErr.ResponseHeaders)
	}
	if lastErr != nil && lastErr.IsCredentialFailure() {
		status, message := credentialFailoverClientResponse(lastErr)
		h.chatCompletionsErrorResponse(c, status, "server_error", message)
		return
	}
	if lastErr != nil && lastErr.IsOpenAICapacityShed() && strings.TrimSpace(lastErr.ClientMessage) != "" {
		status := lastErr.ClientStatusCode
		if status <= 0 {
			status = http.StatusServiceUnavailable
		}
		h.chatCompletionsErrorResponse(c, status, "server_error", lastErr.ClientMessage)
		return
	}
	statusCode := http.StatusBadGateway
	if lastErr != nil && lastErr.StatusCode > 0 {
		statusCode = lastErr.StatusCode
	}
	if lastErr != nil && service.IsOpenAISilentRefusalErrorBody(lastErr.ResponseBody) {
		service.SetOpsUpstreamError(c, statusCode, service.OpenAISilentRefusalClientMessage(), "")
		h.chatCompletionsErrorResponse(c, http.StatusBadGateway, "upstream_error", service.OpenAISilentRefusalClientMessage())
		return
	}
	h.chatCompletionsErrorResponse(c, statusCode, "server_error", infraerrors.AllAccountsExhausted)
}
