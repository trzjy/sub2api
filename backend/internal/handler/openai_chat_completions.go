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
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// syncStreamStartedIfResponseCommitted 在 failover 耗尽前把 streamStarted 与
// 「响应是否已提交」对齐：心跳组件已提交 SSE 200（c.Writer.Written() 为 true）而并发
// 槽位等待（acquireResponsesAccountSlot）尚未将其置为 true 时，置 streamStarted 为 true。
//
// 这是 F2-a 的唯一改动，不触及 failover 判定 / 换号 / 账号选择 / 计费相关逻辑。
// 调用点：本文件 failover 耗尽分支（h.handleFailoverExhausted 调用之前）。这样所有依赖
// streamStarted 的分支（如 handleStreamingAwareError 的流内/非流式分流）一并正确——不是
// 只修护栏一处。同文件既有先例见 openai_gateway_handler.go 的 c.Writer.Written() 同步写法。
func syncStreamStartedIfResponseCommitted(c *gin.Context, streamStarted *bool) {
	if c.Writer.Written() {
		*streamStarted = true
	}
}

// ChatCompletions handles OpenAI Chat Completions API requests.
// POST /v1/chat/completions
func (h *OpenAIGatewayHandler) ChatCompletions(c *gin.Context) {
	streamStarted := false
	defer h.recoverResponsesPanic(c, &streamStarted)

	requestStart := time.Now()

	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", infraerrors.InvalidAPIKey)
		return
	}

	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", infraerrors.UserContextNotFound)
		return
	}
	reqLog := requestLogger(
		c,
		"handler.openai_gateway.chat_completions",
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Any("group_id", apiKey.GroupID),
	)

	if !h.ensureResponsesDependencies(c, reqLog) {
		return
	}

	body, err := readLenientJSONRequestBodyWithPrealloc(c.Request, h.cfg)
	if err != nil {
		if maxErr, ok := extractMaxBytesError(err); ok {
			h.errorResponse(c, http.StatusRequestEntityTooLarge, "invalid_request_error", buildBodyTooLargeMessage(maxErr.Limit))
			return
		}
		logRequestBodyReadFailure(reqLog, c.Request, err)
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", infraerrors.RequestBodyReadFailed)
		return
	}
	if len(body) == 0 {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", infraerrors.RequestBodyEmpty)
		return
	}

	if !gjson.ValidBytes(body) {
		logRequestBodyParseFailure(reqLog, body, nil)
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", infraerrors.RequestBodyParseFailed)
		return
	}

	modelResult := gjson.GetBytes(body, "model")
	if !modelResult.Exists() || modelResult.Type != gjson.String || modelResult.String() == "" {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", infraerrors.ModelRequired)
		return
	}
	reqModel := modelResult.String()
	ensureCompositeTargetPlatform(c, apiKey, reqModel, h.coreGatewayService)
	if !openAICompatibleTextTargetAllowed(c, apiKey, reqModel, h.coreGatewayService) {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", infraerrors.ModelNotSupportedByComposite)
		return
	}
	if cappedBody, changed, err := applyOpenAIReasoningEffortPolicyForRequest(c, apiKey, body); err != nil {
		respondOpenAIReasoningEffortPolicyError(c, err, h.errorResponse)
		return
	} else if changed {
		body = cappedBody
	}
	reqStream, ok := parseOpenAICompatibleStream(body)
	if !ok {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", invalidStreamFieldTypeMessage)
		return
	}
	if _, err := service.ValidateOpenAIServiceTierField(body); err != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if service.IsGPTImageGenerationModel(reqModel) {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", infraerrors.ModelNotSupportedOnChatCompletio)
		return
	}

	reqLog = reqLog.With(zap.String("model", reqModel), zap.Bool("stream", reqStream))

	// 请求含图检测：结果存入 request context，供后续调度按 RequireVision 过滤读取（派发单 C）。
	service.SetOpenAIHasImageInputHint(c, service.HasOpenAIInputImage(body))

	setOpsRequestContext(c, reqModel, reqStream)
	setOpsEndpointContext(c, "", int16(service.RequestTypeFromLegacy(reqStream, false)))

	if decision := h.checkSecurityAudit(c, reqLog, apiKey, subject, service.ContentModerationProtocolOpenAIChat, reqModel, body); decision != nil && !decision.AllowNextStage {
		h.openAISecurityAuditError(c, decision)
		return
	}
	if h.rejectIfCyberSessionBlocked(c, apiKey, body, reqModel, cyberBlockFormatChat) {
		return
	}

	// 解析渠道级模型映射
	channelMapping, _ := h.gatewayService.ResolveChannelMappingAndRestrict(c.Request.Context(), apiKey.GroupID, reqModel)
	forwardModel := openAIChannelForwardModel(channelMapping, reqModel)

	if h.errorPassthroughService != nil {
		service.BindErrorPassthroughService(c, h.errorPassthroughService)
	}

	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	requestPlatform := openAICompatibleRequestPlatform(c.Request.Context(), apiKey)

	service.SetOpsLatencyMs(c, service.OpsAuthLatencyMsKey, time.Since(requestStart).Milliseconds())
	routingStart := time.Now()

	// §25.2/§25.14-D：续轮识别与整批接管必须发生在**用户槽/预留/选号之前**。
	if serveCursorResumeIfRequested(c, h.gatewayService, service.CursorResumeProtocolChat, body,
		cursorResumeIdentity(apiKey, subject.UserID), reqStream, func(status int, message string) {
			h.errorResponse(c, status, "invalid_request_error", message)
		}) {
		return
	}

	userReleaseFunc, acquired := h.acquireResponsesUserSlot(c, subject.UserID, reqStream, &streamStarted, reqLog)
	if !acquired {
		return
	}
	if userReleaseFunc != nil {
		defer userReleaseFunc()
	}

	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
		reqLog.Info("openai_chat_completions.billing_eligibility_check_failed", zap.Error(err))
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.handleStreamingAwareError(c, status, code, message, streamStarted)
		return
	}

	// 余额模式在途预留：防止并发请求在预检时看到同一份余额而集体透支。
	inflightHandle, err := reserveTransferableInflightBalance(c, h.billingCacheService, h.gatewayService, apiKey, subscription, tokenInflightEstimate(reqModel, body))
	if err != nil {
		reqLog.Info("openai_chat_completions.inflight_reservation_rejected", zap.Error(err))
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.handleStreamingAwareError(c, status, code, message, streamStarted)
		return
	}
	if inflightHandle != nil {
		defer inflightHandle.Release()
	}

	sessionHash := h.gatewayService.GenerateSessionHash(c, body)
	promptCacheKey := h.gatewayService.ExtractSessionID(c, body)

	switchCount := 0
	profitVetoCount := 0
	failedAccountIDs := make(map[int64]struct{})
	sameAccountRetryCount := make(map[int64]int)
	var lastFailoverErr *service.UpstreamFailoverError
	var oauth429FailoverState service.OpenAIOAuth429FailoverState

	// 分组利润控制：chat completions 文本入口请求级装门并固定 pricingAt。
	// 带图请求沿用生图惯例跳过利润门（方案 §3.6）：先打 suppressed 标记再装门，
	// 视觉分流目标账号不被利润门 veto；不含图请求路径零改动。
	ccPricingCtx, pricingAt := h.gatewayService.WithOpenAIRequestPricingContext(
		h.withOpenAIProfitSuppressedForImage(c.Request.Context(), body), apiKey.GroupID)
	c.Request = c.Request.WithContext(ccPricingCtx)

	// CF524 P2-B 入口安装点（方案 §3.1）：认证后、选号循环前安装请求级预算快照 +
	// 心跳 owner。安装 helper 落在 *OpenAIGatewayHandler（收敛清理：P2-C 统一接收者版本，P2-A 的
	// *GatewayHandler 版已删）。CC 链无 compact keepalive（状态 key 恒不存在）→ 正常安装，
	// 无需互斥分支。
	cf524StopHeartbeat := h.cf524InstallUpstreamBudgetAndHeartbeatOpenAI(c, requestStart, reqStream)
	defer cf524StopHeartbeat()

	// 分层路由泛化（派发单 OC-T1）：选号前估算 input 分档并装门——仅当请求模型
	// 属于目标集合且动态门控开启；其余（非目标模型 / 门未开）零行为变化。
	// 门控/目标判定与 token 估算共用请求侧模型（forwardModel 即请求侧名）。
	gateOpen := func(m string) (bool, error) {
		return h.gatewayService.TierRoutingGateOpen(c.Request.Context(), apiKey.GroupID, requestPlatform, m)
	}
	tierCtx, err := applyTierRouting(c, c.Request.Context(), reqLog, forwardModel, forwardModel, service.TierRoutingProtocolChatCompletions, body, gateOpen)
	if err != nil {
		reqLog.Warn("openai_chat_completions.kimi_tier_estimate_failed", zap.Error(err))
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", infraerrors.TierRoutingEstimateFailed)
		return
	}
	c.Request = c.Request.WithContext(tierCtx)

	// 订阅组大请求动态排除自建账号（派发单 D-SHS-001）：选号循环前将自建站点账号
	// 并入 failedAccountIDs；估算 <= 阈值或估算失败（失败开放）时不排除任何账号。
	if adds := h.applySelfHostSizeExclusion(c.Request.Context(), reqLog, apiKey.GroupID, requestPlatform, forwardModel, service.TierRoutingProtocolChatCompletions, body); len(adds) > 0 {
		for id := range adds {
			failedAccountIDs[id] = struct{}{}
		}
	}

	for {
		if failoverClientGone(c) {
			return
		}
		reqLog.Debug("openai_chat_completions.account_selecting", zap.Int("excluded_account_count", len(failedAccountIDs)))
		// 请求含图时要求账号支持视觉：读取请求级含图 hint（缺失视为不含图）。
		hasImageInput, _ := service.GetOpenAIHasImageInputHint(c)
		selection, scheduleDecision, err := h.gatewayService.SelectAccountWithSchedulerForCapability(
			c.Request.Context(),
			apiKey.GroupID,
			"",
			sessionHash,
			forwardModel,
			failedAccountIDs,
			service.OpenAIUpstreamTransportAny,
			service.OpenAIEndpointCapabilityChatCompletions,
			false,
			false,
			true,
			hasImageInput,
			requestPlatform,
		)
		if err != nil {
			if failoverClientGone(c) {
				reqLog.Info("openai_chat_completions.account_select_aborted_client_disconnected", zap.Error(err))
				return
			}
			reqLog.Warn("openai_chat_completions.account_select_failed",
				zap.Error(openAICompatibleSelectionErrorForLog(err, requestPlatform)),
				zap.Int("excluded_account_count", len(failedAccountIDs)),
			)
			if len(failedAccountIDs) == 0 {
				cls := classifyOpenAICompatibleNoAccountErrorFromGin(c, h.gatewayService, apiKey, reqModel, reqModel)
				cls = classifySelectionFailureError(err, cls)
				if !cls.ModelNotFound {
					markOpsRoutingCapacityLimitedIfNoAvailable(c, err)
				}
				h.handleStreamingAwareError(c, cls.Status, cls.ErrType, cls.Message, streamStarted)
				return
			} else {
				if lastFailoverErr != nil {
					// F2-b：与下方耗尽分支同型——响应已提交时先对齐 streamStarted，
					// 否则会把 JSON 错误体塞进已开的 SSE 流。
					syncStreamStartedIfResponseCommitted(c, &streamStarted)
					h.handleFailoverExhausted(c, lastFailoverErr, streamStarted)
				} else {
					h.handleStreamingAwareError(c, http.StatusBadGateway, "api_error", infraerrors.UpstreamRequestFailed, streamStarted)
				}
				return
			}
		}
		if selection == nil || selection.Account == nil {
			cls := classifyOpenAICompatibleNoAccountErrorFromGin(c, h.gatewayService, apiKey, reqModel, reqModel)
			if !cls.ModelNotFound {
				markOpsRoutingCapacityLimited(c)
			}
			h.handleStreamingAwareError(c, cls.Status, cls.ErrType, cls.Message, streamStarted)
			return
		}
		account := selection.Account
		sessionHash = ensureOpenAIPoolModeSessionHash(sessionHash, account)
		reqLog.Debug("openai_chat_completions.account_selected", zap.Int64("account_id", account.ID), zap.String("account_name", account.Name))
		_ = scheduleDecision
		setOpsSelectedAccount(c, account.ID, account.Platform)

		accountReleaseFunc, slotResult := h.acquireResponsesAccountSlot(c, apiKey.GroupID, sessionHash, selection, reqStream, &streamStarted, reqLog)
		if slotResult == openAISlotAcquireProfitVetoed {
			// 利润终检否决：排除该账号重新选号；否决次数达上限则按无可用账号终止。
			if !recordOpenAIProfitVeto(failedAccountIDs, account.ID, &profitVetoCount) {
				h.handleOpenAIProfitVetoExhausted(c, streamStarted, reqLog, profitVetoCount)
				return
			}
			continue
		}
		if slotResult != openAISlotAcquireOK {
			return
		}

		service.SetOpsLatencyMs(c, service.OpsRoutingLatencyMsKey, time.Since(routingStart).Milliseconds())
		forwardStart := time.Now()

		forwardBody := body
		if channelMapping.Mapped {
			forwardBody = h.gatewayService.ReplaceModelInBody(body, channelMapping.MappedModel)
		}
		writerSizeBeforeForward := c.Writer.Size()
		// CF524 P2-B 防线统一（§3.3，踩坑 #1）：与 writerSizeBeforeForward 同时点捕获
		// 心跳已提交字节基线；owner 不存在（非流式/心跳禁用/未装快照）时为 0。CC 链无
		// compact keepalive，无需额外归一（compact 口径冲突不适用于本链，调用方无需改口径）。
		hbAtForwardStart, _ := service.UpstreamHeartbeatFromContext(c.Request.Context())
		hbCommittedBaseline := 0
		if hbAtForwardStart != nil {
			hbCommittedBaseline = int(hbAtForwardStart.CommittedBytes())
		}
		// §25.4/§25.5：Cursor 账号把本 HTTP 已获取的用户槽/账号槽/余额预留转交给会话，
		// 并把本 HTTP 的计费任务交给会话唯一结算；非 Cursor 账号显式清空转交物。
		// §25.17.5 A06：晚绑定链已删除——构造不再接收 worker 提交函数（helper 保留该
		// 参数仅为不动其它 6 处 handler 构造调用点），结算输入只来自下方注册的
		// CursorSettlementFunc，不得再据旧参数另排账务任务。
		attemptHandover := newCursorResourceHandover(
			cursorHandoverResources(
				transferableSlot(c, cursorSlotHandleUserKey),
				transferableSlot(c, cursorSlotHandleAccountKey),
				inflightHandle,
			),
			nil,
		)
		// §25.17.5 A06：首次 Execute 与资源转交之前注册**唯一结算**——不可变值快照
		// （user/API key/组/订阅/账号、pricingAt、quotaPlatform、原请求 hash、UA/IP 及
		// 请求侧审计参数，值捕获，不闭包 gin.Context/旧 writer/原正文）。会话终结时由
		// helper settle 把终态真实 *ForwardResult 交回；缺完整 usage 不提交的判定在
		// helper/执行服务侧完成，这里只构造原 RecordUsage 输入并调用原 mandatory
		// usage worker（至多一次）。
		if isCursorPlatformAccount(account) {
			snapshot := captureOpenAIChatCursorSettlement(
				c, apiKey, account, subscription, channelMapping, reqModel, pricingAt, body,
			)
			attemptHandover.RegisterCursorSettlement(func(ctx context.Context, final *service.ForwardResult) {
				submitOpenAIChatCursorSettlement(h, snapshot, ctx, final)
			})
		}
		c.Request = c.Request.WithContext(bindCursorHandoverForAttempt(c.Request.Context(), account, attemptHandover))
		result, err := func() (*service.OpenAIForwardResult, error) {
			defer func() {
				if accountReleaseFunc != nil {
					accountReleaseFunc()
				}
			}()
			return h.gatewayService.ForwardAsChatCompletions(c.Request.Context(), c, account, forwardBody, promptCacheKey, "")
		}()
		var cyberBlockBodyChat []byte
		if service.GetOpsCyberPolicy(c) != nil {
			cyberBlockBodyChat = body
		}
		h.recordCyberPolicyIfMarked(c, apiKey, account, subscription, reqModel, err != nil, cyberBlockBodyChat, clientRequestedUsageFields(c, channelMapping, reqModel, ""), service.HashUsageRequestPayload(body))

		forwardDurationMs := time.Since(forwardStart).Milliseconds()
		upstreamLatencyMs, _ := getContextInt64(c, service.OpsUpstreamLatencyMsKey)
		responseLatencyMs := forwardDurationMs
		if upstreamLatencyMs > 0 && forwardDurationMs > upstreamLatencyMs {
			responseLatencyMs = forwardDurationMs - upstreamLatencyMs
		}
		service.SetOpsLatencyMs(c, service.OpsResponseLatencyMsKey, responseLatencyMs)
		if err == nil && result != nil && result.FirstTokenMs != nil {
			service.SetOpsLatencyMs(c, service.OpsTimeToFirstTokenMsKey, int64(*result.FirstTokenMs))
		}
		// #5148 对齐：错误返回携带的部分 result（流中断前上游已计量的 usage）照常
		// 入账；failover 错误恒定 result=nil，不会重复计费。
		submitChatUsage := func(res *service.OpenAIForwardResult) {
			// §25.17.5 A06：Cursor 分支不再调用——本 HTTP 的计费任务已由会话唯一结算
			// 承载（上方注册的 CursorSettlementFunc 收到终态真实 usage 后至多提交一次），
			// 原"Execute 返回后捕获 result 的晚绑定链"已删除；首/最终 HTTP 返回后不得
			// 再为 Cursor 另排一次账。非 Cursor 路径保持原样（含 #5148 部分 result 入账）。
			if res == nil || isCursorPlatformAccount(account) {
				return
			}
			stampOpenAIRequestedReasoningEffort(res, c)
			userAgent := c.GetHeader("User-Agent")
			clientIP := ip.GetClientIP(c)
			inboundEndpoint := GetInboundEndpoint(c)
			upstreamEndpoint := resolveOpenAIUpstreamEndpoint(c, account, res)
			quotaPlatform := service.QuotaPlatform(c.Request.Context(), apiKey)
			sessionID := service.ExtractClientSessionID(c)
			cyberBlocked := service.GetOpsCyberPolicy(c) != nil
			h.submitOpenAIUsageRecordTask(c.Request.Context(), res, func(ctx context.Context) {
				if err := h.gatewayService.RecordUsage(ctx, &service.OpenAIRecordUsageInput{
					Result:             res,
					APIKey:             apiKey,
					User:               apiKey.User,
					Account:            account,
					Subscription:       subscription,
					InboundEndpoint:    inboundEndpoint,
					UpstreamEndpoint:   upstreamEndpoint,
					UserAgent:          userAgent,
					IPAddress:          clientIP,
					APIKeyService:      h.apiKeyService,
					QuotaPlatform:      quotaPlatform,
					SessionID:          sessionID,
					ChannelUsageFields: clientRequestedUsageFields(c, channelMapping, reqModel, res.UpstreamModel),
					PricingAt:          pricingAt,
					CyberBlocked:       cyberBlocked,
				}); err != nil {
					logger.L().With(
						zap.String("component", "handler.openai_gateway.chat_completions"),
						zap.Int64("user_id", subject.UserID),
						zap.Int64("api_key_id", apiKey.ID),
						zap.Any("group_id", apiKey.GroupID),
						zap.String("model", reqModel),
						zap.Int64("account_id", account.ID),
					).Error("openai_chat_completions.record_usage_failed", zap.Error(err))
				}
			})
		}
		if err != nil {
			if result != nil && result.ImageCount > 0 {
				reqLog.Warn("openai_chat_completions.forward_partial_error_with_image_result",
					zap.Int64("account_id", account.ID),
					zap.Int("image_count", result.ImageCount),
					zap.Error(err),
				)
			} else {
				var failoverErr *service.UpstreamFailoverError
				if errors.As(err, &failoverErr) {
					if failoverClientGone(c) {
						reqLog.Info("openai_chat_completions.failover_aborted_client_disconnected",
							zap.Int64("account_id", account.ID),
							zap.Int("upstream_status", failoverErr.StatusCode),
						)
						return
					}
					if !cf524OpenAIHeartbeatFailoverGuardStillClean(c, writerSizeBeforeForward, c.Writer.Size(), hbCommittedBaseline) {
						h.gatewayService.ObserveOpenAIAccountHealthFailure(c.Request.Context(), account, err)
						// 2026-10-20 实证：容量降载与账号强相关（同负载 231 零降载），
						// 终止后须清粘性+LRU 降权，客户端同会话立刻重试才能落到健康账号。
						if failoverErr != nil && failoverErr.IsOpenAICapacityShed() {
							h.gatewayService.PenalizeSessionAccountForCapacityShed(c.Request.Context(), apiKey.GroupID, sessionHash, account.ID)
						}
						h.handleFailoverExhausted(c, failoverErr, true)
						return
					}
					if failoverErr.ShouldReportAccountScheduleFailure() {
						h.gatewayService.ReportOpenAIAccountScheduleResult(account, openAIAccountScheduleModel(c, account, reqModel, false, nil), false, nil, err)
					}
					if !failoverErr.ShouldRetryNextAccount() {
						// 2026-10-20 实证：容量降载与账号强相关（同负载 231 零降载），
						// 终止后须清粘性+LRU 降权，客户端同会话立刻重试才能落到健康账号。
						if failoverErr != nil && failoverErr.IsOpenAICapacityShed() {
							h.gatewayService.PenalizeSessionAccountForCapacityShed(c.Request.Context(), apiKey.GroupID, sessionHash, account.ID)
						}
						// F2-a：failover 耗尽前把 streamStarted 与「响应是否已提交」对齐。
						// 心跳已提交 SSE 200 而 streamStarted 仍为 false 时置 true，确保写入
						// 流内 SSE error 帧而非塞进已开 SSE 流的 JSON 体。
						syncStreamStartedIfResponseCommitted(c, &streamStarted)
						h.handleFailoverExhausted(c, failoverErr, streamStarted)
						return
					}
					// Pool mode: retry on the same account
					if failoverErr.RetryableOnSameAccount {
						retryLimit := effectiveSameAccountRetryLimit(failoverErr, account)
						if sameAccountRetryAllowed(failoverErr, sameAccountRetryCount[account.ID], retryLimit) {
							sameAccountRetryCount[account.ID]++
							retryDelay := sameAccountRetryDelayFor(failoverErr, sameAccountRetryCount[account.ID])
							reqLog.Warn("openai_chat_completions.pool_mode_same_account_retry",
								zap.Int64("account_id", account.ID),
								zap.Int("upstream_status", failoverErr.StatusCode),
								zap.Int("retry_limit", retryLimit),
								zap.Int("retry_count", sameAccountRetryCount[account.ID]),
								zap.Duration("retry_delay", retryDelay),
							)
							select {
							case <-c.Request.Context().Done():
								return
							case <-time.After(retryDelay):
							}
							continue
						}
					}
					h.gatewayService.RecordOpenAIAccountSwitch()
					// 2026-10-20 实证：容量降载与账号强相关（同负载 231 零降载），
					// 终止后须清粘性+LRU 降权，客户端同会话立刻重试才能落到健康账号。
					if failoverErr != nil && failoverErr.IsOpenAICapacityShed() {
						h.gatewayService.PenalizeSessionAccountForCapacityShed(c.Request.Context(), apiKey.GroupID, sessionHash, account.ID)
					}
					failedAccountIDs[account.ID] = struct{}{}
					lastFailoverErr = failoverErr
					switchCount++
					if h.gatewayService.ShouldStopOpenAIOAuth429Failover(account, failoverErr.StatusCode, switchCount, &oauth429FailoverState) {
						// 2026-10-20 实证：容量降载与账号强相关（同负载 231 零降载），
						// 终止后须清粘性+LRU 降权，客户端同会话立刻重试才能落到健康账号。
						if failoverErr != nil && failoverErr.IsOpenAICapacityShed() {
							h.gatewayService.PenalizeSessionAccountForCapacityShed(c.Request.Context(), apiKey.GroupID, sessionHash, account.ID)
						}
						// F2-b：与上方 !ShouldRetryNextAccount() 分支同型——响应已提交时必须先对齐
						// streamStarted，否则耗尽写回会把 JSON 错误体塞进已开的 SSE 流。
						syncStreamStartedIfResponseCommitted(c, &streamStarted)
						h.handleFailoverExhausted(c, failoverErr, streamStarted)
						return
					}
					reqLog.Warn("openai_chat_completions.upstream_failover_switching",
						zap.Int64("account_id", account.ID),
						zap.Int("upstream_status", failoverErr.StatusCode),
						zap.Int("switch_count", switchCount),
					)
					continue
				}
				// 2026-10-20：mid-stream 容量降载在 reader 已输出语义字节后只返回普通 error
				//（不构造 failover error，上面的 failover 分支够不着），按错误语义兜底：
				//清粘性+LRU 降权，客户端同会话立刻重试才能落到健康账号。
				if service.IsOpenAICapacityShedError(err) {
					h.gatewayService.PenalizeSessionAccountForCapacityShed(c.Request.Context(), apiKey.GroupID, sessionHash, account.ID)
				}
				h.gatewayService.ReportOpenAIAccountScheduleResult(account, openAIAccountScheduleModel(c, account, reqModel, false, nil), false, nil, err)
				upstreamErrorAlreadyCommunicated := openAIForwardErrorAlreadyCommunicated(c, writerSizeBeforeForward, err)
				wroteFallback := false
				if !upstreamErrorAlreadyCommunicated {
					wroteFallback = h.ensureOpenAIStreamReadErrorResponse(c, err, streamStarted)
					if !wroteFallback {
						wroteFallback = h.ensureForwardErrorResponse(c, streamStarted)
					}
				}
				reqLog.Warn("openai_chat_completions.forward_failed",
					zap.Int64("account_id", account.ID),
					zap.Bool("fallback_error_response_written", wroteFallback),
					zap.Bool("upstream_error_response_already_written", upstreamErrorAlreadyCommunicated),
					zap.Error(err),
				)
				submitChatUsage(result)
				return
			}
		}
		if result != nil {
			h.gatewayService.ReportOpenAIAccountScheduleResult(account, openAIAccountScheduleModel(c, account, reqModel, false, result), true, result.FirstTokenMs)
		} else {
			h.gatewayService.ReportOpenAIAccountScheduleResult(account, openAIAccountScheduleModel(c, account, reqModel, false, result), true, nil)
		}

		submitChatUsage(result)
		reqLog.Debug("openai_chat_completions.request_completed",
			zap.Int64("account_id", account.ID),
			zap.Int("switch_count", switchCount),
		)
		return
	}
}

// resolveOpenAIUpstreamEndpoint returns the actual upstream endpoint for an
// OpenAI-compatible account. A forwarding result is authoritative because a
// single inbound route may choose raw Chat or a Responses bridge at runtime.
// The account-based derivation remains as a fallback for existing callers and
// forwarding paths that do not report their endpoint yet.
func resolveOpenAIUpstreamEndpoint(c *gin.Context, account *service.Account, result *service.OpenAIForwardResult) string {
	if result != nil {
		if endpoint := strings.TrimSpace(result.UpstreamEndpoint); endpoint != "" {
			return endpoint
		}
	}
	if endpoint := service.GetActualOpenAIUpstreamEndpoint(c); endpoint != "" {
		return endpoint
	}
	if account != nil && account.Type == service.AccountTypeAPIKey &&
		!openai_compat.ShouldUseResponsesAPI(account.Extra) {
		return EndpointChatCompletions
	}
	return GetUpstreamEndpoint(c, account.Platform)
}

// cursorChatSettlementSnapshot 是 Cursor 账号分支唯一结算的**不可变值快照**（§25.17.5 A06）。
//
// 在首次 Execute 与资源转交之前捕获：user/API key/组/订阅/账号、pricingAt、
// quotaPlatform、原请求 hash、UA/IP、inbound/upstream endpoint、会话标识及请求侧审计
// 参数全部以值落到结构体，闭包不再引用 gin.Context、旧 writer 或原正文。
type cursorChatSettlementSnapshot struct {
	apiKey                   *service.APIKey
	user                     *service.User
	account                  *service.Account
	subscription             *service.UserSubscription
	pricingAt                time.Time
	quotaPlatform            string
	requestPayloadHash       string // 原请求 hash（HashUsageRequestPayload(body)）
	userAgent                string
	clientIP                 string
	inboundEndpoint          string
	upstreamEndpoint         string // 实际出站信息：终态 ForwardResult 无端点字段，快照 c 上值
	sessionID                string
	cyberBlocked             bool
	requestedPublicModel     string  // 渠道归因用的客户端请求模型名
	requestedReasoningEffort *string // 审计参数：stampOpenAIRequestedReasoningEffort 的等价值
	channelMapping           service.ChannelMappingResult
	reqModel                 string // 日志用
}

// captureOpenAIChatCursorSettlement 捕获唯一结算所需的不可变值快照。
//
// 只在此处（HTTP 生命周期内）读取 gin.Context，之后结算闭包只接受该快照与终态
// *ForwardResult。upstreamEndpoint 用 nil result 求 c 上实际出站端点：Cursor 终态
// ForwardResult 不携带 UpstreamEndpoint 字段，其结算值等价于快照时刻的 c 探测值。
func captureOpenAIChatCursorSettlement(
	c *gin.Context,
	apiKey *service.APIKey,
	account *service.Account,
	subscription *service.UserSubscription,
	channelMapping service.ChannelMappingResult,
	reqModel string,
	pricingAt time.Time,
	body []byte,
) *cursorChatSettlementSnapshot {
	return &cursorChatSettlementSnapshot{
		apiKey:                   apiKey,
		user:                     apiKey.User,
		account:                  account,
		subscription:             subscription,
		pricingAt:                pricingAt,
		quotaPlatform:            service.QuotaPlatform(c.Request.Context(), apiKey),
		requestPayloadHash:       service.HashUsageRequestPayload(body),
		userAgent:                c.GetHeader("User-Agent"),
		clientIP:                 ip.GetClientIP(c),
		inboundEndpoint:          GetInboundEndpoint(c),
		upstreamEndpoint:         resolveOpenAIUpstreamEndpoint(c, account, nil),
		sessionID:                service.ExtractClientSessionID(c),
		cyberBlocked:             service.GetOpsCyberPolicy(c) != nil,
		requestedPublicModel:     clientRequestedModel(c, reqModel),
		requestedReasoningEffort: service.RequestedReasoningEffortFromContext(c.Request.Context()),
		channelMapping:           channelMapping,
		reqModel:                 reqModel,
	}
}

// submitOpenAIChatCursorSettlement 由 helper settle 在会话终结时调用（至多一次）。
//
// §25.17.5 A06：收到终态真实 *ForwardResult 后构造原 RecordUsage 输入，调用原
// mandatory usage worker 提交。缺完整真实 usage 时不提交的判定在 helper/执行服务侧
// 已完成（helper 只在 UsageComplete 且终态结果非 nil 才调用本函数），闭包只透传终态：
// 最终模型/真实 usage/时长取自终态，实际出站信息与审计参数来自不可变快照。
func submitOpenAIChatCursorSettlement(
	h *OpenAIGatewayHandler,
	snap *cursorChatSettlementSnapshot,
	ctx context.Context,
	final *service.ForwardResult,
) {
	if snap == nil || h == nil || final == nil || snap.apiKey == nil {
		return
	}
	result := cursorChatFinalToOpenAIResult(final)
	if result.RequestedReasoningEffort == nil {
		result.RequestedReasoningEffort = snap.requestedReasoningEffort
	}
	h.submitMandatoryUsageRecordTask(ctx, func(recordCtx context.Context) {
		if err := h.gatewayService.RecordUsage(recordCtx, &service.OpenAIRecordUsageInput{
			Result:             result,
			APIKey:             snap.apiKey,
			User:               snap.user,
			Account:            snap.account,
			Subscription:       snap.subscription,
			InboundEndpoint:    snap.inboundEndpoint,
			UpstreamEndpoint:   snap.upstreamEndpoint,
			UserAgent:          snap.userAgent,
			IPAddress:          snap.clientIP,
			SessionID:          snap.sessionID,
			RequestPayloadHash: snap.requestPayloadHash,
			APIKeyService:      h.apiKeyService,
			QuotaPlatform:      snap.quotaPlatform,
			PricingAt:          snap.pricingAt,
			CyberBlocked:       snap.cyberBlocked,
			ChannelUsageFields: snap.channelMapping.ToUsageFields(snap.requestedPublicModel, final.UpstreamModel),
		}); err != nil {
			logger.L().With(
				zap.String("component", "handler.openai_gateway.chat_completions"),
				zap.Int64("user_id", snap.user.ID),
				zap.Int64("api_key_id", snap.apiKey.ID),
				zap.Any("group_id", snap.apiKey.GroupID),
				zap.String("model", snap.reqModel),
				zap.Int64("account_id", snap.account.ID),
			).Error("openai_chat_completions.record_usage_failed", zap.Error(err))
		}
	})
}

// cursorChatFinalToOpenAIResult 把 Cursor 会话终态 ForwardResult 搬进 OpenAI 网关的
// 结果形状（与 service 侧 cursorForwardResultToOpenAI 同口径：usage 逐字段真实映射，
// 零折算、不补默认值，**不在此计费**）。
func cursorChatFinalToOpenAIResult(final *service.ForwardResult) *service.OpenAIForwardResult {
	if final == nil {
		return nil
	}
	out := &service.OpenAIForwardResult{
		ResponseID:      final.RequestID,
		UpstreamHeaders: final.UpstreamHeaders,
		Usage: service.OpenAIUsage{
			InputTokens:              final.Usage.InputTokens,
			OutputTokens:             final.Usage.OutputTokens,
			CacheCreationInputTokens: final.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     final.Usage.CacheReadInputTokens,
			ImageOutputTokens:        final.Usage.ImageOutputTokens,
		},
		Model:            final.Model,
		UpstreamModel:    final.UpstreamModel,
		Stream:           final.Stream,
		Duration:         final.Duration,
		FirstTokenMs:     final.FirstTokenMs,
		ClientDisconnect: final.ClientDisconnect,
		ImageCount:       final.ImageCount,
		ImageOutputSizes: final.ImageOutputSizes,
		SearchCount:      final.SearchCount,
		AudioUsage:       final.AudioUsage,
	}
	if final.UpstreamHeaders != nil {
		out.ResponseHeaders = final.UpstreamHeaders.Clone()
	}
	return out
}
