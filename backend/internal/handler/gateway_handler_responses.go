package handler

import (
	"context"
	"errors"
	"fmt"
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

// Responses handles OpenAI Responses API endpoint for Anthropic platform groups.
// POST /v1/responses
// This converts Responses API requests to Anthropic format, forwards to Anthropic
// upstream, and converts responses back to Responses format.
func (h *GatewayHandler) Responses(c *gin.Context) {
	streamStarted := false

	requestStart := time.Now()

	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.responsesErrorResponse(c, http.StatusUnauthorized, "authentication_error", infraerrors.InvalidAPIKey)
		return
	}

	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.responsesErrorResponse(c, http.StatusInternalServerError, "api_error", infraerrors.UserContextNotFound)
		return
	}
	reqLog := requestLogger(
		c,
		"handler.gateway.responses",
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Any("group_id", apiKey.GroupID),
	)

	// Read request body
	body, err := readLenientJSONRequestBodyWithPrealloc(c.Request, h.cfg)
	if err != nil {
		if maxErr, ok := extractMaxBytesError(err); ok {
			h.responsesErrorResponse(c, http.StatusRequestEntityTooLarge, "invalid_request_error", buildBodyTooLargeMessage(maxErr.Limit))
			return
		}
		h.responsesErrorResponse(c, http.StatusBadRequest, "invalid_request_error", infraerrors.RequestBodyReadFailed)
		return
	}

	if len(body) == 0 {
		h.responsesErrorResponse(c, http.StatusBadRequest, "invalid_request_error", infraerrors.RequestBodyEmpty)
		return
	}

	setOpsRequestContext(c, "", false)

	// Validate JSON
	if !gjson.ValidBytes(body) {
		logRequestBodyParseFailure(reqLog, body, nil)
		h.responsesErrorResponse(c, http.StatusBadRequest, "invalid_request_error", infraerrors.RequestBodyParseFailed)
		return
	}

	// Extract model and stream using gjson (like OpenAI handler)
	modelResult := gjson.GetBytes(body, "model")
	if !modelResult.Exists() || modelResult.Type != gjson.String || modelResult.String() == "" {
		h.responsesErrorResponse(c, http.StatusBadRequest, "invalid_request_error", infraerrors.ModelRequired)
		return
	}
	reqModel := modelResult.String()
	bindRequestedReasoningEffort(c, body, reqModel)
	ensureCompositeTargetPlatform(c, apiKey, reqModel, h.gatewayService)
	if !compositeTargetPlatformResolved(c, apiKey, reqModel, h.gatewayService) {
		h.responsesErrorResponse(c, http.StatusNotFound, "model_not_found", fmt.Sprintf(infraerrors.ModelNotInGroup, reqModel))
		return
	}
	if rejectSystemOneOnlyPlatform(c, apiKey, h.responsesErrorResponse) {
		return
	}
	reqStream, ok := parseOpenAICompatibleStream(body)
	if !ok {
		h.responsesErrorResponse(c, http.StatusBadRequest, "invalid_request_error", invalidStreamFieldTypeMessage)
		return
	}
	reqLog = reqLog.With(zap.String("model", reqModel), zap.Bool("stream", reqStream))

	setOpsRequestContext(c, reqModel, reqStream)
	setOpsEndpointContext(c, "", int16(service.RequestTypeFromLegacy(reqStream, false)))
	requestCtx := c.Request.Context()
	// 定价上下文无条件装配：/v1/responses 是 token 计费端点，声明生图工具的
	// 混合请求同样按 token 计费（外加图片部分），其 token 利润保护不因请求体
	// 里的任何工具声明（含 Codex 被动 image_gen namespace）而关闭。生图意图
	// 仅用于能力路由与图片计费；独立图片/视频端点才在利润门范围之外。
	requestCtx, pricingAt := service.WithGatewayTokenRequestPricing(requestCtx)
	if service.IsImageGenerationIntentForPlatform("/v1/responses", reqModel, body, openAICompatibleRequestPlatform(c.Request.Context(), apiKey)) {
		requestCtx = service.WithOpenAIImageGenerationIntent(requestCtx)
	}
	c.Request = c.Request.WithContext(requestCtx)

	// 解析渠道级模型映射
	channelMapping, _ := h.gatewayService.ResolveChannelMappingAndRestrict(requestCtx, apiKey.GroupID, reqModel)
	// 将渠道映射后的上游模型名下传 ctx，供调度门按映射名判定模型级限流（详见 service.WithChannelMappedModel）。
	if channelMapping.Mapped && channelMapping.MappedModel != reqModel {
		requestCtx = service.WithChannelMappedModel(requestCtx, channelMapping.MappedModel)
		c.Request = c.Request.WithContext(requestCtx)
	}

	// Claude Code only restriction: /v1/responses is never a Claude Code
	// endpoint. With a fallback group the request continues and account selection
	// (checkClaudeCodeRestriction) schedules it in the fallback group; without one
	// it is rejected here.
	if apiKey.Group != nil && apiKey.Group.ClaudeCodeOnly && apiKey.Group.FallbackGroupID == nil {
		h.responsesErrorResponse(c, http.StatusForbidden, "permission_error",
			"This group is restricted to Claude Code clients (/v1/messages only)")
		return
	}
	if geminiImageGenerationRejected(c, apiKey, nil, reqModel, channelMapping.MappedModel) {
		h.responsesErrorResponse(c, http.StatusBadRequest, "invalid_request_error", service.ImageGenerationPermissionMessage())
		return
	}

	if decision := h.checkSecurityAudit(c, reqLog, apiKey, subject, service.ContentModerationProtocolOpenAIResponses, reqModel, body); decision != nil && !decision.AllowNextStage {
		h.responsesSecurityAuditError(c, decision)
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
	if serveCursorResumeIfRequested(c, h.gatewayService, service.CursorResumeProtocolResponses, body,
		cursorResumeIdentity(apiKey, subject.UserID), reqStream, func(status int, message string) {
			h.responsesErrorResponse(c, status, "invalid_request_error", message)
		}, "") {
		return
	}

	userReleaseFunc, err := h.concurrencyHelper.AcquireScopedUserSlot(c, scope, subject.UserID)
	if err != nil {
		reqLog.Warn("gateway.responses.user_slot_acquire_failed", zap.Error(err))
		h.handleConcurrencyError(c, err, scope.Kind, streamStarted)
		return
	}
	// §25.4：用户槽改为**可转交**句柄。Cursor 分支把释放所有权交给会话持有；
	// 其它供应商仍按原 HTTP 生命周期释放（语义不变，仍只 release 一次）。
	userSlot := newTransferableRelease(c.Request.Context(), "user_slot", userReleaseFunc)
	if userSlot != nil {
		userReleaseFunc = userSlot.Release
		defer userSlot.Release()
	}

	// 2. Re-check billing
	if err := h.billingCacheService.CheckBillingEligibility(requestCtx, apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(requestCtx, apiKey)); err != nil {
		reqLog.Info("gateway.responses.billing_check_failed", zap.Error(err))
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.responsesErrorResponse(c, status, code, message)
		return
	}

	// 余额模式在途预留：防止并发请求在预检时看到同一份余额而集体透支。
	inflightHandle, err := reserveTransferableInflightBalance(c, h.billingCacheService, h.gatewayService, apiKey, subscription, tokenInflightEstimate(reqModel, body))
	if err != nil {
		reqLog.Info("gateway.responses.inflight_reservation_rejected", zap.Error(err))
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.responsesErrorResponse(c, status, code, message)
		return
	}
	if inflightHandle != nil {
		defer inflightHandle.Release()
	}

	// Parse request for session hash
	bodyRef := service.NewRequestBodyRef(body)
	parsedReq, _ := service.ParseGatewayRequest(bodyRef, "responses")
	if parsedReq == nil {
		parsedReq = &service.ParsedRequest{Model: reqModel, Stream: reqStream, Body: bodyRef}
	}
	parsedReq.SessionContext = &service.SessionContext{
		ClientIP:  ip.GetClientIP(c),
		UserAgent: c.GetHeader("User-Agent"),
		APIKeyID:  apiKey.ID,
	}
	sessionHash := h.gatewayService.GenerateSessionHash(parsedReq)

	// 3. Account selection + failover loop
	fs := NewFailoverState(false).BindRequest(c)
	slotFullCount := 0
	ownershipRejectedCount := 0

	for {
		if failoverClientGone(c) {
			return
		}

		// A7b：跨链 responses（ServiceChain=anthropic，多平台供给且未收敛到具体目标
		// 平台）用 A4b selector 从 SupplyAccounts 选号；选中账号可能来自 OpenAI 或
		// Anthropic 执行链，按账号链分派既有 Forward。非跨链路径保持原选号逻辑不变。
		decision, decisionOK := service.CompositeRouteDecisionFromContext(requestCtx)
		crossChainResponses := decisionOK &&
			service.CompositeDecisionSupplyExists(decision) &&
			decision.ServiceChain == service.CompositeChainAnthropic &&
			decision.TargetPlatform == "" &&
			len(decision.SupplyAccounts) > 0

		var account *service.Account
		var accountReleaseFunc func()
		var accountSlot *transferableRelease
		var err error
		if crossChainResponses {
			groupID := int64(0)
			if apiKey.GroupID != nil {
				groupID = *apiKey.GroupID
			}
			selected, release, selErr := h.gatewayService.SelectCompositeCrossChainAccount(
				requestCtx, groupID, reqModel,
				service.CompositeEndpointResponses, decision.SupplyAccounts, fs.FailedAccountIDs,
			)
			if selErr != nil && errors.Is(selErr, service.ErrNoAvailableAccounts) && !concurrencySlotWaitExhausted(c) {
				waitTarget, waitErr := h.gatewayService.SelectCompositeCrossChainWaitTarget(
					requestCtx, groupID, reqModel, service.CompositeEndpointResponses,
					decision.SupplyAccounts, fs.FailedAccountIDs, sessionHash)
				if waitErr == nil && waitTarget != nil && waitTarget.Account != nil {
					waitedRelease, waitedSlotFull := h.concurrencyHelper.AcquireAccountSlotForSelection(
						c, waitTarget, concurrencySlotWaitExhausted(c), reqStream, &streamStarted)
					if !waitedSlotFull {
						selected = waitTarget.Account
						release = waitedRelease
						selErr = nil
					}
				}
			}
			if selErr != nil {
				// 与本入口既有"无可用账号"同口径：不得当成 502 上游错误。
				if failoverClientGone(c) {
					reqLog.Info("gateway.responses.cross_chain_select_aborted_client_disconnected", zap.Error(selErr))
					return
				}
				if len(fs.FailedAccountIDs) == 0 {
					cls := classifyNoAccountErrorFromGin(c, h.gatewayService, apiKey, reqModel, reqModel, effectiveAPIKeyPlatform(c, apiKey), selErr)
					if !cls.ModelNotFound {
						markOpsRoutingCapacityLimitedIfNoAvailable(c, selErr)
					}
					message := cls.Message
					h.responsesErrorResponse(c, cls.Status, cls.ErrType, message)
					return
				}
				action := fs.HandleSelectionExhausted(requestCtx)
				switch action {
				case FailoverContinue:
					continue
				case FailoverCanceled:
					failoverClientGone(c)
					return
				default:
					if fs.LastFailoverErr != nil && !lastFailureWasSlot(c) {
						h.handleResponsesFailoverExhausted(c, fs.LastFailoverErr, streamStarted)
					} else if NoAvailableBySlotOrOwnership(slotFullCount, ownershipRejectedCount) {
						// 槽位失败导致候选耗尽：仍按账号维度并发终态写出 503，
						// 不能落到 502 分支——那会破坏账号并发终点契约。
						h.handleConcurrencyError(c, &ConcurrencyError{SlotType: "account"}, "account", streamStarted)
					} else {
						h.responsesErrorResponse(c, http.StatusBadGateway, "server_error", infraerrors.AllAccountsExhausted)
					}
					return
				}
			}
			account = selected
			accountReleaseFunc = release
			// 与非跨链路径同型：账号槽登记为**可转交**句柄（Cursor 分支交给会话持有）。
			accountSlot = newTransferableRelease(requestCtx, "account_slot", accountReleaseFunc)
			if accountSlot != nil {
				accountReleaseFunc = accountSlot.Release
			}
			// 外审 F2：本入口在 :99 提前保存了局部 requestCtx，必须在此回填实际账号平台，
			// 使下方转发/结算快照/usage/failover 全部读到真实平台（QuotaPlatform 同步生效）。
			requestCtx = service.WithResolvedTargetPlatform(requestCtx, account.Platform)
			requestCtx = service.WithCompositeActualPlatformResolved(requestCtx)
			c.Request = c.Request.WithContext(requestCtx)
			setOpsSelectedAccount(c, account.ID, account.Platform)
			// selector 已按既有逻辑抢占并发槽，这里不再二次抢槽，也不写粘性。
			// 标准模式按实际账号平台补检 user×platform 配额；订阅模式跳过。
			if quotaErr := h.checkCompositeCrossChainPlatformQuota(c, apiKey, subscription, account); quotaErr != nil {
				if accountReleaseFunc != nil {
					accountReleaseFunc()
				}
				accountReleaseFunc = nil
				fs.FailedAccountIDs[account.ID] = struct{}{}
				continue
			}
		} else {
			selection, selErr := h.gatewayService.SelectAccountWithLoadAwareness(requestCtx, apiKey.GroupID, sessionHash, reqModel, fs.FailedAccountIDs, "", int64(0))
			err = selErr
			if err != nil {
				if failoverClientGone(c) {
					reqLog.Info("gateway.responses.account_select_aborted_client_disconnected", zap.Error(err))
					return
				}
				if len(fs.FailedAccountIDs) == 0 {
					cls := classifyNoAccountErrorFromGin(c, h.gatewayService, apiKey, reqModel, reqModel, effectiveAPIKeyPlatform(c, apiKey), err)
					if !cls.ModelNotFound {
						markOpsRoutingCapacityLimitedIfNoAvailable(c, err)
					}
					message := cls.Message
					h.responsesErrorResponse(c, cls.Status, cls.ErrType, message)
					return
				}
				action := fs.HandleSelectionExhausted(requestCtx)
				switch action {
				case FailoverContinue:
					continue
				case FailoverCanceled:
					failoverClientGone(c)
					return
				default:
					if fs.LastFailoverErr != nil && !lastFailureWasSlot(c) {
						h.handleResponsesFailoverExhausted(c, fs.LastFailoverErr, streamStarted)
					} else if NoAvailableBySlotOrOwnership(slotFullCount, ownershipRejectedCount) {
						// 槽位失败导致候选耗尽：仍按账号维度并发终态写出 503，
						// 不能落到 502 分支——那会破坏账号并发终点契约。
						h.handleConcurrencyError(c, &ConcurrencyError{SlotType: "account"}, "account", streamStarted)
					} else {
						h.responsesErrorResponse(c, http.StatusBadGateway, "server_error", infraerrors.AllAccountsExhausted)
					}
					return
				}
			}
			account = selection.Account
			setOpsSelectedAccount(c, account.ID, account.Platform)

			if h.concurrencyHelper.RejectCompositeIneligible(c, selection, fs.FailedAccountIDs, &ownershipRejectedCount) {
				continue
			}

			// 4. Acquire account concurrency slot via the unified entry (§4.3).
			// 统一入口接管选号已持槽的 ReleaseFunc（不重复占槽）；未持槽时先立即
			// 抢一次，仅当本次尚未等过且有等待计划才等待一次（换号后不再等待）。
			var slotFull bool
			accountReleaseFunc, slotFull = h.concurrencyHelper.AcquireAccountSlotForSelection(
				c, selection, slotFullCount > 0, reqStream, &streamStarted,
			)
			if slotFull {
				reqLog.Warn("gateway.responses.account_slot_acquire_failed", zap.Int64("account_id", account.ID))
				// 槽位等不到（响应尚未写出）：排除该账号，换同组下一个账号再试。
				// 候选耗尽由下一轮选号「无候选」出口按账号并发终态写出。
				if recordConcurrencySlotFailure(c, fs.FailedAccountIDs, account.ID, &slotFullCount) {
					continue
				}
				h.handleConcurrencyError(c, &ConcurrencyError{SlotType: "account"}, "account", streamStarted)
				return
			}
			// 利润终检必须使用选号结果携带的门：门安装在调度栈的局部
			// ctx 上（composite/fallback 还可能解析出与入口分组不同的门），直接用
			// requestCtx 会退化为空操作。
			admissionCtx := service.ContextWithSelectionProfitGate(requestCtx, selection)
			latest, vetoed, reason := h.gatewayService.GatewayProfitControlVetoLatest(admissionCtx, account)
			if vetoed {
				if accountReleaseFunc != nil {
					accountReleaseFunc()
				}
				reqLog.Debug("gateway.responses.account_slot_profit_vetoed", zap.Int64("account_id", account.ID), zap.String("reason", reason))
				if fs.RecordProfitVeto(account.ID) == FailoverExhausted {
					reqLog.Warn("gateway.responses.profit_veto_attempts_exhausted", zap.Int("profit_veto_count", fs.ProfitVetoCount()))
					h.responsesErrorResponse(c, http.StatusServiceUnavailable, "api_error", profitVetoExhaustedMessage)
					return
				}
				continue
			}
			account = latest
			selection.Account = latest
			// §25.4：账号槽同样改为**可转交**句柄（Cursor 分支交给会话持有）。
			accountSlot = newTransferableRelease(c.Request.Context(), "account_slot", accountReleaseFunc)
			if accountSlot != nil {
				accountReleaseFunc = accountSlot.Release
			}
		}

		// 5. Forward request
		// CF524（外审修复）：心跳基线须与 writerSizeBeforeForward 同时点捕获——判定用
		// 增量严格相等，前置心跳帧不会被重复计入（与 gateway_handler.go 同型）。
		writerSizeBeforeForward := c.Writer.Size()
		hbCommittedBaseline := 0
		if hbOwner, ok := service.UpstreamHeartbeatFromContext(requestCtx); ok && hbOwner != nil {
			hbCommittedBaseline = int(hbOwner.CommittedBytes())
		}
		forwardBody := body
		if channelMapping.Mapped {
			forwardBody = h.gatewayService.ReplaceModelInBody(body, channelMapping.MappedModel)
		}
		// §25.4/§25.5/§25.17.5(A06)：Cursor 账号把本 HTTP 已获取的用户槽/账号槽/余额
		// 预留转交给会话，并在**首次 Execute 与资源转交之前**以不可变值快照构造本 HTTP
		// 的唯一结算钩子（不闭包 gin.Context、旧 writer 或原正文）注册到转交物。会话
		// 终结时 helper 把终态真实 *ForwardResult 交给该钩子，由原 mandatory usage
		// worker 构造原 RecordUsage 输入并提交、至多一次；非 Cursor 账号不做任何转交、
		// 不注册结算。
		forwardCtx := requestCtx
		if isCursorPlatformAccount(account) {
			handover := newCursorResourceHandover(
				cursorHandoverResources(userSlot, accountSlot, inflightHandle),
				h.submitMandatoryUsageRecordTask,
			)
			handover.RegisterCursorSettlement(h.buildResponsesCursorSettlement(responsesCursorSettlementSnapshot{
				log:              reqLog,
				apiKey:           apiKey,
				user:             apiKey.User,
				account:          account,
				subscription:     subscription,
				pricingAt:        pricingAt,
				quotaPlatform:    service.QuotaPlatform(requestCtx, apiKey),
				requestHash:      service.HashUsageRequestPayload(body),
				userAgent:        c.GetHeader("User-Agent"),
				clientIP:         ip.GetClientIP(c),
				inboundEndpoint:  GetInboundEndpoint(c),
				upstreamEndpoint: GetUpstreamEndpoint(c, account.Platform),
				sessionID:        service.ExtractClientSessionID(c),
				requestedModel:   clientRequestedModel(c, reqModel),
				channelMapping:   channelMapping,
			}))
			forwardCtx = bindCursorHandoverForAttempt(requestCtx, account, handover)
			c.Request = c.Request.WithContext(forwardCtx)
		}
		var result *service.ForwardResult
		setActualUpstreamEndpoint(c, "")
		if crossChainResponses {
			// A7b：跨链 responses 按选中账号所属执行链分派；无匹配链时明确失败关闭，
			// 结果/错误进入下方既有 usage/failover 链（FailoverError 用 account.Platform）。
			result, err = h.forwardCompositeCrossChainResponses(requestCtx, c, account, forwardBody, parsedReq)
		} else {
			switch responsesAccountForwardPath(account) {
			case "devin":
				// Gemini 分组里的 Devin 账号没有 base_url：gatewayService.ForwardAsResponses
				// 的普通出口会打到 Devin 边缘并返回 403 "Request not allowed"，账号随后被置
				// error。Devin 的正确出口是 OpenAIGatewayService.Forward（内部含 Responses→Chat
				// 归一、identity 回桥与 schema 清洗）。服务未装配时失败关闭，收尾写法与
				// antigravity 兼容服务为 nil 的现有分支一致。
				if h.openAIGatewayService == nil {
					h.responsesErrorResponse(c, http.StatusBadGateway, "upstream_error", infraerrors.CompatServiceNotConfigured)
					if accountReleaseFunc != nil {
						accountReleaseFunc()
					}
					return
				}
				oaRes, oaErr := h.openAIGatewayService.Forward(forwardCtx, c, account, forwardBody)
				if oaRes != nil && strings.TrimSpace(oaRes.UpstreamEndpoint) != "" {
					setActualUpstreamEndpoint(c, oaRes.UpstreamEndpoint)
				}
				result = forwardResultFromOpenAIResult(oaRes)
				err = oaErr
			case "antigravity":
				if h.antigravityGatewayService == nil {
					h.responsesErrorResponse(c, http.StatusBadGateway, "upstream_error", infraerrors.CompatServiceNotConfigured)
					if accountReleaseFunc != nil {
						accountReleaseFunc()
					}
					return
				}
				setActualUpstreamEndpoint(c, EndpointAntigravityGenerateContent)
				result, err = h.antigravityGatewayService.ForwardAsResponses(forwardCtx, c, account, forwardBody, parsedReq)
			default: // gateway
				result, err = h.gatewayService.ForwardAsResponses(forwardCtx, c, account, forwardBody, parsedReq)
			}
		}

		if accountReleaseFunc != nil {
			accountReleaseFunc()
		}

		// 提交 usage 记录。成功路径与"流中断但 Forward 已观测到 usage 的部分结果"
		// 错误路径共用；failover 错误恒定 result=nil，不会重复计费。
		submitForwardUsage := func(result *service.ForwardResult) {
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
					reqLog.Error("gateway.responses.record_usage_failed",
						zap.Int64("account_id", account.ID),
						zap.Error(err),
					)
				}
			})
		}

		if err != nil {
			// Devin system/developer 图片拒绝是确定的入参不支持：立即返回，
			// 不进入 failover、不记账号失败（不调用 HandleFailoverError/冷却）。
			// 未提交写协议 400；已提交走流内终止错误事件，避免把 JSON 塞进已 flush 的 SSE 200。
			if st, m, ok := devinClientFacingError(err); ok {
				if gatewayResponseCommitted(c, streamStarted) {
					h.handleStreamingAwareError(c, st, "invalid_request_error", m, streamStarted)
				} else {
					h.responsesErrorResponse(c, st, "invalid_request_error", m)
				}
				return
			}
			var failoverErr *service.UpstreamFailoverError
			if errors.As(err, &failoverErr) {
				// Can't failover if streaming content already sent.
				// CF524（v5）：心跳注释帧非语义写出——防线改用心跳基线例外判定，
				// 心跳已提交时仍允许换号（无心跳时判定与现状完全一致）。
				if !heartbeatFailoverGuardStillClean(c, writerSizeBeforeForward, hbCommittedBaseline) {
					// 2026-10-20 实证：容量降载与账号强相关（同负载 231 零降载），
					// 中段流失败终止后客户端以同一 session_id 立刻重试，须清粘性+LRU 降权让其落到健康账号。
					if failoverErr != nil && failoverErr.IsOpenAICapacityShed() {
						h.openAIGatewayService.PenalizeSessionAccountForCapacityShed(c.Request.Context(), apiKey.GroupID, sessionHash, account.ID)
					}
					h.handleResponsesFailoverExhausted(c, failoverErr, true)
					return
				}
				action := fs.HandleFailoverError(requestCtx, h.gatewayService, account.ID, account.Platform, account.GetPoolModeRetryCount(), failoverErr)
				switch action {
				case FailoverContinue:
					continue
				case FailoverExhausted:
					// 2026-10-20 实证：容量降载与账号强相关（同负载 231 零降载），
					// 跨请求必须换号——清粘性+LRU 降权让客户端立刻重试落到健康账号。
					if fs.LastFailoverErr != nil && fs.LastFailoverErr.IsOpenAICapacityShed() {
						h.openAIGatewayService.PenalizeSessionAccountForCapacityShed(c.Request.Context(), apiKey.GroupID, sessionHash, account.ID)
					}
					h.handleResponsesFailoverExhausted(c, fs.LastFailoverErr, streamStarted)
					return
				case FailoverCanceled:
					failoverClientGone(c)
					return
				}
			}
			upstreamErrorAlreadyCommunicated := gatewayForwardErrorAlreadyCommunicated(c, writerSizeBeforeForward, err)
			wroteFallback := false
			if !upstreamErrorAlreadyCommunicated {
				wroteFallback = h.writeResponsesForwardErrorResponse(c, reqLog, account, err, streamStarted)
			}
			reqLog.Error("gateway.responses.forward_failed",
				zap.Int64("account_id", account.ID),
				zap.Bool("fallback_error_response_written", wroteFallback),
				zap.Bool("upstream_error_response_already_written", upstreamErrorAlreadyCommunicated),
				zap.Error(err),
			)
			if result != nil && !isCursorPlatformAccount(account) {
				// Cursor 的部分用量同样由会话结算钩子统一提交，此处不得另排一次账。
				submitForwardUsage(result)
			}
			return
		}

		// 绑定只在转发成功之后写入（方案 §3.3-0/1）：选号期不再写绑定，
		// 本次账号与既有绑定不一致时由利润门包装器覆盖。C25a：所有平台
		// （含 Cursor）转发成功后都写绑定；Cursor 结算返回前亦经过此写入点。
		if err := h.gatewayService.BindStickySessionAfterProfitAdmission(c.Request.Context(), apiKey.GroupID, sessionHash, account.ID); err != nil {
			reqLog.Warn("gateway.responses.bind_sticky_session_after_profit_admission_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		}

		// 6. Record usage
		if isCursorPlatformAccount(account) {
			// §25.17.5(A06)：Cursor 的计费已由首次 Execute 之前注册的唯一结算钩子在
			// 会话终结时提交（终态真实 usage、至多一次），本 HTTP 返回后**不得再为
			// Cursor 另排一次账**——既无晚绑定改投，也不重复提交 UsageRecordTask。
			return
		}
		submitForwardUsage(result)
		return
	}
}

// responsesAccountForwardPath 决定 /v1/responses 端口账号的转发出口。
// 它是 devin / antigravity / gateway 三分支分派的单一来源，避免再次漏掉某个平台分支。
// nil 账号按空安全处理，返回 "gateway"（交由既有 gatewayService 兜底）。
func responsesAccountForwardPath(account *service.Account) string {
	if account == nil {
		return "gateway"
	}
	switch account.Platform {
	case service.PlatformDevin:
		return "devin"
	case service.PlatformAntigravity:
		// 仅 OAuth 类型的 antigravity 账号走 OAuth 专用兼容通道；
		// API Key / setup-token / upstream 等非 OAuth 账号必须回落 gateway，
		// 否则会被 antigravity_gateway_compat 直接 400（基线行为必须保留）。
		if shouldUseAntigravityCompat(account) {
			return "antigravity"
		}
		return "gateway"
	default:
		return "gateway"
	}
}

// compositeCrossChainResponsesForwardKind 返回跨链 responses 请求应按哪个既有转发器分派。
// 依次以 openai→anthropic 调用 CompositeAccountChainPath（链顺序与 A1 selector 一致；
// gemini 链在 responses 端点不可服务，A1 对 gemini 账号返回 false，故不纳入候选），
// 命中首条可服务本端点的链即返回其 kind；两条链都不可时返回空串，调用方必须失败关闭，
// 不得静默回落到其它链。
func compositeCrossChainResponsesForwardKind(account *service.Account) string {
	if account == nil {
		return ""
	}
	for _, candidate := range [...]struct{ chain, kind string }{
		{service.CompositeChainOpenAI, "openai"},
		{service.CompositeChainAnthropic, "anthropic"},
	} {
		if _, ok := service.CompositeAccountChainPath(candidate.chain, service.CompositeEndpointResponses, account); !ok {
			continue
		}
		return candidate.kind
	}
	return ""
}

// forwardCompositeCrossChainResponses 按 compositeCrossChainResponsesForwardKind 的判定，
// 把跨链 responses 请求交给与账号所属执行链匹配的既有 Forward。结果按既有路径消费；
// 无匹配链或服务未注入时返回明确错误，绝不静默回落到其它链。
func (h *GatewayHandler) forwardCompositeCrossChainResponses(
	ctx context.Context,
	c *gin.Context,
	account *service.Account,
	forwardBody []byte,
	parsedReq *service.ParsedRequest,
) (*service.ForwardResult, error) {
	switch compositeCrossChainResponsesForwardKind(account) {
	case "openai":
		// 该服务已含 cursor/codebuddy/devin/web/CN 等 Responses 分支，
		// 结果经 forwardResultFromOpenAIResult 归一为 *ForwardResult。
		if h.openAIGatewayService == nil {
			return nil, fmt.Errorf("composite cross-chain responses: openai service not configured")
		}
		oaRes, oaErr := h.openAIGatewayService.Forward(ctx, c, account, forwardBody)
		return forwardResultFromOpenAIResult(oaRes), oaErr
	case "anthropic":
		if h.gatewayService == nil {
			return nil, fmt.Errorf("composite cross-chain responses: anthropic service not configured")
		}
		return h.gatewayService.ForwardAsResponses(ctx, c, account, forwardBody, parsedReq)
	default:
		platform := ""
		if account != nil {
			platform = account.Platform
		}
		return nil, fmt.Errorf("composite cross-chain responses: no forwarding chain for platform %q", platform)
	}
}

// writeResponsesForwardErrorResponse 在 Forward 返回非 failover 错误、且尚未向客户端写出错误时补写错误响应。
//
// Cursor 账号且 service.CursorUserFacingError 命中时，按其状态码 + 中文提示写回（方案 §2.2.2、§2.3 M3）；
// 4xx 用 invalid_request_error，其余沿用原出口的 upstream_error。前置判定与 ensureForwardErrorResponse 一致：
// 客户端已断开不写、响应已提交不写、Writer 已写过则只写流内错误帧（不重写状态码）。
// 非 Cursor 账号或未命中映射时走原 ensureForwardErrorResponse（502 兜底）。
func (h *GatewayHandler) writeResponsesForwardErrorResponse(c *gin.Context, reqLog *zap.Logger, account *service.Account, err error, streamStarted bool) bool {
	if !isCursorPlatformAccount(account) || c == nil || c.Writer == nil || c.Request == nil {
		return h.ensureForwardErrorResponse(c, streamStarted)
	}
	status, code, message, ok := service.CursorUserFacingError(c.Request.Context(), err)
	if !ok {
		return h.ensureForwardErrorResponse(c, streamStarted)
	}
	if errors.Is(c.Request.Context().Err(), context.Canceled) {
		failoverClientGone(c)
		return false
	}
	if service.IsResponseCommitted(c) {
		return false
	}
	// 不再就地同步 streamStarted：下游 handleStreamingAwareErrorWithCode 用唯一事实源
	// gatewayResponseCommitted 自行判定 c.Writer.Written()。
	errType := "upstream_error"
	if status >= 400 && status < 500 {
		errType = "invalid_request_error"
	}
	reqLog.Warn("cursor_user_error", zap.String("error_code", code), zap.Int("status", status))
	h.handleStreamingAwareError(c, status, errType, message, streamStarted)
	return true
}

// responsesErrorResponse writes an error in OpenAI Responses API format.
func (h *GatewayHandler) responsesErrorResponse(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{
		"error": gin.H{
			"code":    code,
			"message": message,
		},
	})
}

// handleResponsesFailoverExhausted writes a failover-exhausted error in Responses format.
func (h *GatewayHandler) handleResponsesFailoverExhausted(c *gin.Context, lastErr *service.UpstreamFailoverError, streamStarted bool) {
	if lastErr != nil {
		copyFailoverRetryAfter(c, lastErr.ResponseHeaders)
	}
	// 2026-11-04 方案 §2.4-7：Cursor 账号池真耗尽统一中文终态 CUR-RES-11。
	// 无论是否已开始流式，都不得透出上游原文或内部 reason，也不得静默 EOF；
	// 状态码沿用换号错误自带值（cursor 额度耗尽为 429），error.type/code 保持原写法。
	if lastErr != nil && service.IsCursorQuotaExhaustedReason(lastErr.Reason) {
		message := cursorPoolExhaustedMessage(c)
		status := cursorPoolExhaustedStatus(lastErr)
		// 判据只认唯一事实源 gatewayResponseCommitted：CF524 心跳可能已提交 SSE 200，
		// 此时即使 streamStarted 为 false 也只能写流内终止事件，不能再走 JSON 错误体。
		if gatewayResponseCommitted(c, streamStarted) {
			service.MarkOpsStreamError(c, "server_error", message, status)
			writeResponsesFailedSSE(c, "server_error", infraerrors.CursorResPoolExhaustedCode, message)
			return
		}
		h.responsesErrorResponse(c, status, "server_error", message)
		return
	}
	statusCode := http.StatusBadGateway
	if lastErr != nil && lastErr.StatusCode > 0 {
		statusCode = lastErr.StatusCode
	}
	status, code, message := statusCode, "server_error", infraerrors.AllAccountsExhausted
	if lastErr != nil && lastErr.IsCredentialFailure() {
		status, message = credentialFailoverClientResponse(lastErr)
	} else if lastErr != nil && lastErr.IsOpenAICapacityShed() && strings.TrimSpace(lastErr.ClientMessage) != "" {
		status = lastErr.ClientStatusCode
		if status <= 0 {
			status = http.StatusServiceUnavailable
		}
		message = lastErr.ClientMessage
	} else if lastErr != nil && service.IsOpenAISilentRefusalErrorBody(lastErr.ResponseBody) {
		service.SetOpsUpstreamError(c, statusCode, service.OpenAISilentRefusalClientMessage(), "")
		status, code, message = http.StatusBadGateway, "upstream_error", service.OpenAISilentRefusalClientMessage()
	} else if lastErr != nil && lastErr.Reason == service.CursorQuotaExhaustedReason && strings.TrimSpace(lastErr.ClientMessage) != "" {
		status = lastErr.ClientStatusCode
		if status <= 0 {
			status = http.StatusTooManyRequests
		}
		message = lastErr.ClientMessage
	} else if lastErr != nil && statusCode == http.StatusTooManyRequests {
		status, code, message = http.StatusTooManyRequests, "rate_limit_error", infraerrors.AllAccountsRateLimited
	}
	// 判据只认唯一事实源 gatewayResponseCommitted：CF524 心跳可能已提交 SSE 200，
	// 此时即使 streamStarted 为 false 也只能写流内终止事件，不能再走 JSON 错误体。
	if gatewayResponseCommitted(c, streamStarted) {
		// A slot-wait heartbeat commits HTTP 200 before any upstream response.
		// In that case a terminal frame is still required; once any semantic or
		// official terminal bytes exist, preserve them without appending a second
		// generic response.failed.
		service.MarkOpsStreamError(c, code, message, status)
		// gatewayStreamHasOnlyHeartbeats 的口径已统一：handler 层准入 ping +
		// service 层 CF524 心跳字节两处记账相加（此前只算前者，CF524 心跳已写出
		// keep-alive 帧时会漏判，导致 Codex CLI 报 "stream closed before
		// response.completed"）。
		if c != nil && c.Writer != nil && (c.Writer.Size() <= 0 || gatewayStreamHasOnlyHeartbeats(c)) {
			writeResponsesFailedSSE(c, code, "", message)
		}
		return
	}
	h.responsesErrorResponse(c, status, code, message)
}

// responsesCursorSettlementSnapshot 是 /v1/responses Cursor 分支的唯一结算参数快照
// （§25.17.5 A06）。
//
// 在**首次 Execute 与资源转交之前**构造：user/API key/组/订阅/账号、pricingAt、
// quotaPlatform、原请求 hash、UA/IP 及请求侧审计参数全部以不可变值捕获，不闭包
// gin.Context、旧 writer 或原请求正文。指针字段取 Execute 前已稳定解析的引用对象
// （其后不再被改写），结算闭包只读这些快照值。
type responsesCursorSettlementSnapshot struct {
	log              *zap.Logger
	apiKey           *service.APIKey
	user             *service.User
	account          *service.Account
	subscription     *service.UserSubscription
	pricingAt        time.Time
	quotaPlatform    string
	requestHash      string
	userAgent        string
	clientIP         string
	inboundEndpoint  string
	upstreamEndpoint string
	sessionID        string
	requestedModel   string
	channelMapping   service.ChannelMappingResult
}

// recordUsageInput 用快照 + 会话终态结果构造原 RecordUsage 输入。
//
// APIKeyService 由调用方（结算闭包）填充：它来自 handler 的长期依赖，不属于请求级
// 快照。渠道字段用快照中的客户端请求模型与终态的真实 UpstreamModel 计算。
func (s responsesCursorSettlementSnapshot) recordUsageInput(final *service.ForwardResult) *service.RecordUsageInput {
	return &service.RecordUsageInput{
		Result:             final,
		QuotaPlatform:      s.quotaPlatform,
		APIKey:             s.apiKey,
		User:               s.user,
		Account:            s.account,
		Subscription:       s.subscription,
		PricingAt:          s.pricingAt,
		InboundEndpoint:    s.inboundEndpoint,
		UpstreamEndpoint:   s.upstreamEndpoint,
		UserAgent:          s.userAgent,
		IPAddress:          s.clientIP,
		SessionID:          s.sessionID,
		RequestPayloadHash: s.requestHash,
		ChannelUsageFields: s.channelMapping.ToUsageFields(s.requestedModel, final.UpstreamModel),
	}
}

// buildResponsesCursorSettlement 构造 /v1/responses Cursor 分支的唯一结算闭包。
//
// 会话终结时被 helper settle 以终态真实 *ForwardResult 调用（完整 usage 的判定已在
// helper/执行服务侧完成，缺完整 usage 时本闭包不会被调用，这里只透传）：用快照构造
// 原 RecordUsage 输入并调用原 mandatory usage worker 提交。生命周期内至多执行一次，
// 终态结果为空时只记录诊断日志、不造零输入。
func (h *GatewayHandler) buildResponsesCursorSettlement(snapshot responsesCursorSettlementSnapshot) CursorSettlementFunc {
	return func(ctx context.Context, final *service.ForwardResult) {
		if final == nil {
			snapshot.log.Warn("gateway.responses.cursor_settlement_nil_result")
			return
		}
		input := snapshot.recordUsageInput(final)
		input.APIKeyService = h.apiKeyService
		h.submitMandatoryUsageRecordTask(ctx, func(taskCtx context.Context) {
			if err := h.gatewayService.RecordUsage(taskCtx, input); err != nil {
				snapshot.log.Error("gateway.responses.record_usage_failed",
					zap.Int64("account_id", snapshot.account.ID),
					zap.Error(err),
				)
			}
		})
	}
}
