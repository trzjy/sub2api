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
		h.chatCompletionsErrorResponse(c, http.StatusNotFound, "model_not_found", fmt.Sprintf(infraerrors.ModelNotInGroup, reqModel))
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
	if geminiImageGenerationRejected(c, apiKey, nil, reqModel, channelMapping.MappedModel) {
		h.chatCompletionsErrorResponse(c, http.StatusBadRequest, "invalid_request_error", service.ImageGenerationPermissionMessage())
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
		}, "") {
		return
	}

	userReleaseFunc, err := h.concurrencyHelper.AcquireScopedUserSlot(c, scope, subject.UserID)
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
	fs := newChatCompletionsFailoverState(c, groupPlatform, h.maxAccountSwitchesGemini)
	slotFullCount := 0
	ownershipRejectedCount := 0

	for {
		if failoverClientGone(c) {
			return
		}

		// A7：跨链 chat（ServiceChain=anthropic，多平台供给且未收敛到具体目标平台）
		// 用 A4b selector 从 SupplyAccounts 选号；选中账号可能来自 OpenAI/Anthropic/
		// Gemini 任一执行链，按账号链分派既有 Forward。非跨链路径保持原选号逻辑不变。
		decision, decisionOK := service.CompositeRouteDecisionFromContext(c.Request.Context())
		crossChainChat := decisionOK &&
			service.CompositeDecisionSupplyExists(decision) &&
			decision.ServiceChain == service.CompositeChainAnthropic &&
			decision.TargetPlatform == "" &&
			len(decision.SupplyAccounts) > 0

		var account *service.Account
		var accountReleaseFunc func()
		var accountSlot *transferableRelease
		var err error
		if crossChainChat {
			groupID := int64(0)
			if apiKey.GroupID != nil {
				groupID = *apiKey.GroupID
			}
			selected, release, selErr := h.gatewayService.SelectCompositeCrossChainAccount(
				c.Request.Context(), groupID, reqModel,
				service.CompositeEndpointChatCompletions, decision.SupplyAccounts, fs.FailedAccountIDs,
			)
			// G19：跨链全满（无可用账号）且本请求尚未等过时，按 G17 同型补一次等待。
			// 仅在选号确认全满时进入；等待/抢槽失败保持本入口原有错误出口不变。
			if selErr != nil && errors.Is(selErr, service.ErrNoAvailableAccounts) && !concurrencySlotWaitExhausted(c) {
				waitTarget, waitErr := h.gatewayService.SelectCompositeCrossChainWaitTarget(
					c.Request.Context(), groupID, reqModel,
					service.CompositeEndpointChatCompletions, decision.SupplyAccounts, fs.FailedAccountIDs, selectionSessionHash,
				)
				if waitErr == nil && waitTarget != nil {
					waitRelease, slotFull := h.concurrencyHelper.AcquireAccountSlotForSelection(
						c, waitTarget, concurrencySlotWaitExhausted(c), reqStream, &streamStarted)
					if !slotFull && waitTarget.Account != nil {
						selected = waitTarget.Account
						release = waitRelease
						selErr = nil
					} else if slotFull {
						recordConcurrencySlotFailure(c, fs.FailedAccountIDs, waitTarget.Account.ID, &slotFullCount)
					}
				}
			}
			if selErr != nil {
				// 与本入口既有"无可用账号"同口径：不得当成 502 上游错误。
				if failoverClientGone(c) {
					reqLog.Info("gateway.cc.cross_chain_select_aborted_client_disconnected", zap.Error(selErr))
					return
				}
				if len(fs.FailedAccountIDs) == 0 {
					cls := classifyNoAccountErrorFromGin(c, h.gatewayService, apiKey, reqModel, reqModel, groupPlatform, selErr)
					if !cls.ModelNotFound {
						markOpsRoutingCapacityLimitedIfNoAvailable(c, selErr)
					}
					h.handleStreamingAwareError(c, cls.Status, cls.ErrType, cls.Message, streamStarted)
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
					if fs.LastFailoverErr != nil && !lastFailureWasSlot(c) {
						h.handleCCFailoverExhausted(c, fs.LastFailoverErr, streamStarted)
					} else if NoAvailableBySlotOrOwnership(slotFullCount, ownershipRejectedCount) {
						// 槽位失败导致候选耗尽：仍按账号维度并发终态写出 503，
						// 不能落到 502 分支——那会破坏账号并发终点契约。
						h.handleConcurrencyError(c, &ConcurrencyError{SlotType: "account"}, "account", streamStarted)
					} else {
						h.chatCompletionsErrorResponse(c, http.StatusBadGateway, "server_error", infraerrors.AllAccountsExhausted)
					}
					return
				}
			}
			account = selected
			accountReleaseFunc = release
			// 与非跨链路径同型：账号槽同样登记为**可转交**句柄（Cursor 分支交给会话持有）。
			accountSlot = newTransferableRelease(c.Request.Context(), "account_slot", accountReleaseFunc)
			if accountSlot != nil {
				accountReleaseFunc = accountSlot.Release
			}
			// ctx 实际平台回填必须先于 usage/quota 捕获：选到账号后立刻盖章，
			// 使下方 submitForwardUsage 的 QuotaPlatform(ctx, apiKey) 计量到真实平台。
			requestCtx := service.WithResolvedTargetPlatform(c.Request.Context(), account.Platform)
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
			selection, selErr := h.gatewayService.SelectAccountWithLoadAwareness(c.Request.Context(), apiKey.GroupID, selectionSessionHash, reqModel, fs.FailedAccountIDs, "", int64(0))
			err = selErr
			if err != nil {
				if failoverClientGone(c) {
					reqLog.Info("gateway.cc.account_select_aborted_client_disconnected", zap.Error(err))
					return
				}
				if len(fs.FailedAccountIDs) == 0 {
					cls := classifyNoAccountErrorFromGin(c, h.gatewayService, apiKey, reqModel, reqModel, groupPlatform, err)
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
					if fs.LastFailoverErr != nil && !lastFailureWasSlot(c) {
						h.handleCCFailoverExhausted(c, fs.LastFailoverErr, streamStarted)
					} else if NoAvailableBySlotOrOwnership(slotFullCount, ownershipRejectedCount) {
						// 槽位失败导致候选耗尽：仍按账号维度并发终态写出 503，
						// 不能落到 502 分支——那会破坏账号并发终点契约。
						h.handleConcurrencyError(c, &ConcurrencyError{SlotType: "account"}, "account", streamStarted)
					} else {
						h.chatCompletionsErrorResponse(c, http.StatusBadGateway, "server_error", infraerrors.AllAccountsExhausted)
					}
					return
				}
			}
			account = selection.Account
			setOpsSelectedAccount(c, account.ID, account.Platform)

			// 归属不合格的复合候选在抢槽/转发前排除：释放选号器已占槽、排除出本请求候选、
			// 登记独立计数；不递增 slotFullCount、不消耗本次请求唯一一次等待。
			if h.concurrencyHelper.RejectCompositeIneligible(c, selection, fs.FailedAccountIDs, &ownershipRejectedCount) {
				continue
			}
			// 3. 获取账号并发槽位（统一入口，方案 §4.3）。
			// 已持槽直接接管、未持槽立即抢一次、未等过且有等待计划时等一次的判定全部
			// 收敛到 AcquireAccountSlotForSelection，调用点不再自写等待分支。
			var slotFull bool
			accountReleaseFunc, slotFull = h.concurrencyHelper.AcquireAccountSlotForSelection(
				c, selection, slotFullCount > 0, reqStream, &streamStarted)
			if slotFull {
				reqLog.Warn("gateway.cc.account_slot_acquire_failed", zap.Int64("account_id", account.ID))
				// 槽位等不到（响应尚未写出）：排除该账号，换同组下一个账号再试。
				// 上限见 maxConcurrencySlotFailoverAttempts；用尽时仍走既有 503 出口。
				if recordConcurrencySlotFailure(c, fs.FailedAccountIDs, account.ID, &slotFullCount) {
					continue
				}
				h.handleConcurrencyError(c, &ConcurrencyError{SlotType: "account"}, "account", streamStarted)
				return
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
			// §3.3/§4.4：选号与抢槽阶段不写绑定；绑定统一推迟到转发成功之后。
			// §25.4：账号槽同样改为**可转交**句柄（Cursor 分支交给会话持有）。
			accountSlot = newTransferableRelease(c.Request.Context(), "account_slot", accountReleaseFunc)
			if accountSlot != nil {
				accountReleaseFunc = accountSlot.Release
			}

			// gemini 分组只放行 gemini 平台自身、Cursor 与 antigravity OAuth 账号。
			// antigravity 必须放行：分组平台是 gemini 时，本文件下方 §5 的
			// shouldUseAntigravityCompat 分支正是为这类账号准备的（转发
			// streamGenerateContent），不在此放行会让该分支成为不可达代码，
			// 表现为「账号健康、/v1/messages 200，但 /v1/chat/completions 直接
			// 502 该分组所有账号均请求失败」且无任何上游错误记录。
			// /v1/messages 入口没有同款平台白名单，两者行为必须一致。
			if !geminiGroupAdmitsAccount(groupPlatform, account) {
				if accountReleaseFunc != nil {
					accountReleaseFunc()
				}
				fs.FailedAccountIDs[account.ID] = struct{}{}
				continue
			}
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
		var result *service.ForwardResult
		setActualUpstreamEndpoint(c, "")
		if isCursorPlatformAccount(account) {
			// §25.17.5 A06：在首次 Execute 与资源转交之前，值捕获不可变计费参数快照并注册
			// 唯一结算闭包——不闭包 gin.Context/旧 writer/原正文；会话终结时由 settle 把
			// 终态真实 *ForwardResult 交给闭包，闭包只构造原 RecordUsage 输入并调用原
			// mandatory usage worker（缺完整 usage 的不提交判定已在 helper/执行服务侧完成）。
			userAgent := c.GetHeader("User-Agent")
			clientIP := ip.GetClientIP(c)
			requestPayloadHash := service.HashUsageRequestPayload(body)
			inboundEndpoint := GetInboundEndpoint(c)
			upstreamEndpoint := GetUpstreamEndpoint(c, account.Platform)
			quotaPlatform := service.QuotaPlatform(c.Request.Context(), apiKey)
			sessionID := service.ExtractClientSessionID(c)
			requestedModel := clientRequestedModel(c, reqModel)
			requestedEffort := service.RequestedReasoningEffortFromContext(c.Request.Context())
			handover := newCursorResourceHandover(
				cursorHandoverResources(userSlot, accountSlot, inflightHandle),
				h.submitMandatoryUsageRecordTask,
			)
			handover.RegisterCursorSettlement(func(ctx context.Context, final *service.ForwardResult) {
				if final == nil {
					return
				}
				stampForwardRequestedReasoningEffort(final, requestedEffort)
				h.submitMandatoryUsageRecordTask(ctx, func(ctx context.Context) {
					if err := h.gatewayService.RecordUsage(ctx, &service.RecordUsageInput{
						Result:             final,
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
						ChannelUsageFields: channelMapping.ToUsageFields(requestedModel, final.UpstreamModel),
					}); err != nil {
						reqLog.Error("gateway.cc.record_usage_failed",
							zap.Int64("account_id", account.ID),
							zap.Error(err),
						)
					}
				})
			})
			c.Request = c.Request.WithContext(bindCursorHandoverForAttempt(c.Request.Context(), account, handover))
		}
		if crossChainChat {
			result, err = h.forwardCompositeCrossChainChat(c.Request.Context(), c, account, forwardBody, parsedReq)
		} else {
			switch chatCompletionsAccountForwardPath(account) {
			case "devin":
				// devin 出站为 Connect+Protobuf，只有 OpenAIGatewayService 实现了正确的
				// 鉴权与协议（forwardDevin）。沿用兼容分支的失败关闭口径：服务未注入时
				// 立即 502 并释放账号槽，绝不回落到通用 gatewayService（后者会用错误
				// 协议/鉴权打上游，实证 401 invalid x-api-key）。
				if h.openAIGatewayService == nil {
					h.chatCompletionsErrorResponse(c, http.StatusBadGateway, "upstream_error", infraerrors.CompatServiceNotConfigured)
					if accountReleaseFunc != nil {
						accountReleaseFunc()
					}
					return
				}
				oaRes, oaErr := h.openAIGatewayService.ForwardAsChatCompletions(c.Request.Context(), c, account, forwardBody, "", "")
				if oaErr == nil && oaRes != nil && oaRes.UpstreamEndpoint != "" {
					setActualUpstreamEndpoint(c, oaRes.UpstreamEndpoint)
				}
				result = forwardResultFromOpenAIResult(oaRes)
				err = oaErr
			case "gemini":
				if h.geminiCompatService == nil {
					h.chatCompletionsErrorResponse(c, http.StatusBadGateway, "upstream_error", infraerrors.CompatServiceNotConfigured)
					if accountReleaseFunc != nil {
						accountReleaseFunc()
					}
					return
				}
				result, err = h.geminiCompatService.ForwardAsChatCompletions(c.Request.Context(), c, account, forwardBody)
			case "antigravity":
				if h.antigravityGatewayService == nil {
					h.chatCompletionsErrorResponse(c, http.StatusBadGateway, "upstream_error", infraerrors.CompatServiceNotConfigured)
					if accountReleaseFunc != nil {
						accountReleaseFunc()
					}
					return
				}
				setActualUpstreamEndpoint(c, EndpointAntigravityGenerateContent)
				result, err = h.antigravityGatewayService.ForwardAsChatCompletions(c.Request.Context(), c, account, forwardBody, parsedReq)
			default:
				result, err = h.gatewayService.ForwardAsChatCompletions(c.Request.Context(), c, account, forwardBody, parsedReq)
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
					reqLog.Error("gateway.cc.record_usage_failed",
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
					h.chatCompletionsErrorResponse(c, st, "invalid_request_error", m)
				}
				return
			}
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
				if h.writeCCCursorUserError(c, reqLog, account, err, streamStarted) {
					wroteFallback = true
				} else {
					wroteFallback = h.ensureForwardErrorResponse(c, streamStarted)
				}
			}
			reqLog.Error("gateway.cc.forward_failed",
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

		// 6. Record usage
		// §3.3/§4.4：绑定只在转发成功之后写入，选号期与失败转发均不写绑定；
		// 本次服务账号写入会话绑定（与既有绑定不同则覆盖）。Cursor 分支之外统一执行。
		if err := h.gatewayService.BindStickySessionAfterProfitAdmission(c.Request.Context(), apiKey.GroupID, selectionSessionHash, account.ID); err != nil {
			reqLog.Warn("gateway.cc.bind_sticky_session_after_profit_admission_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		}
		// §25.17.5 A06：Cursor 的唯一结算已在上方注册到会话，首次/最终 HTTP 返回后不得再
		// 为 Cursor 另排一次账；非 Cursor 走统一计费闭包。
		if !isCursorPlatformAccount(account) {
			submitForwardUsage(result)
		}
		return
	}
}

// compositeCrossChainChatForwardKind 返回跨链 chat 请求应按哪个既有转发器分派。
// 依次以 openai→anthropic→gemini 执行链调用 CompositeAccountChainPath（链顺序与 A1
// selector 一致），命中首个可服务本端点的链即返回其 kind；三条链都不可时返回空串，
// 调用方必须失败关闭，不得静默回落到其它链。
//
// 特例：Anthropic 链的 chat 分支对原生 Gemini 账号亦放行，但返回的出口标识为
// CompositePathGemini（即 gemini 转发器），此时归到 "gemini"，避免把 Gemini 账号
// 交给通用 gateway 转发器重复处理。
func compositeCrossChainChatForwardKind(account *service.Account) string {
	if account == nil {
		return ""
	}
	for _, candidate := range [...]struct{ chain, kind string }{
		{service.CompositeChainOpenAI, "openai"},
		{service.CompositeChainAnthropic, "anthropic"},
		{service.CompositeChainGemini, "gemini"},
	} {
		path, ok := service.CompositeAccountChainPath(candidate.chain, service.CompositeEndpointChatCompletions, account)
		if !ok {
			continue
		}
		if candidate.chain == service.CompositeChainAnthropic && path == service.CompositePathGemini {
			return "gemini"
		}
		return candidate.kind
	}
	return ""
}

// forwardCompositeCrossChainChat 按 compositeCrossChainChatForwardKind 的判定，把跨链
// chat 请求交给与账号所属执行链匹配的既有 Forward。结果按既有路径消费；无匹配链或
// 服务未注入时返回明确错误，绝不静默回落到其它链。
func (h *GatewayHandler) forwardCompositeCrossChainChat(
	ctx context.Context,
	c *gin.Context,
	account *service.Account,
	forwardBody []byte,
	parsedReq *service.ParsedRequest,
) (*service.ForwardResult, error) {
	switch compositeCrossChainChatForwardKind(account) {
	case "openai":
		if h.openAIGatewayService == nil {
			return nil, fmt.Errorf("composite cross-chain chat: openai service not configured")
		}
		oaRes, oaErr := h.openAIGatewayService.ForwardAsChatCompletions(ctx, c, account, forwardBody, "", "")
		return forwardResultFromOpenAIResult(oaRes), oaErr
	case "anthropic":
		if h.gatewayService == nil {
			return nil, fmt.Errorf("composite cross-chain chat: anthropic service not configured")
		}
		return h.gatewayService.ForwardAsChatCompletions(ctx, c, account, forwardBody, parsedReq)
	case "gemini":
		if h.geminiCompatService == nil {
			return nil, fmt.Errorf("composite cross-chain chat: gemini service not configured")
		}
		return h.geminiCompatService.ForwardAsChatCompletions(ctx, c, account, forwardBody)
	default:
		platform := ""
		if account != nil {
			platform = account.Platform
		}
		return nil, fmt.Errorf("composite cross-chain chat: no forwarding chain for platform %q", platform)
	}
}

// checkCompositeCrossChainPlatformQuota 在跨链 chat 选号成功后，按实际账号平台补检
// user×platform 配额（C10a 同口径）。标准/余额模式补检；订阅组 + 订阅用户跳过；
// billingCacheService/user 缺失时降级放行，绝不 panic。
func (h *GatewayHandler) checkCompositeCrossChainPlatformQuota(
	c *gin.Context,
	apiKey *service.APIKey,
	subscription *service.UserSubscription,
	account *service.Account,
) error {
	if c == nil || c.Request == nil || apiKey == nil || account == nil {
		return nil
	}
	if apiKey.Group != nil && apiKey.Group.IsSubscriptionType() && subscription != nil {
		return nil
	}
	if h.billingCacheService == nil || apiKey.User == nil {
		return nil
	}
	return h.billingCacheService.CheckUserPlatformQuotaEligibility(
		c.Request.Context(), apiKey.User.ID, account.Platform,
	)
}

// chatCompletionsAccountForwardPath 选择账号级 chat completions 转发路径。
// devin 优先判定并走 OpenAIGatewayService（其内部按 PlatformDevin 分流到
// forwardDevin 的 Connect 出口）；漏判会落到通用 gatewayService，用错误协议/
// 鉴权打上游（实证：同一 devin 账号 /v1/messages 200 但本端点 401
// invalid x-api-key 并被 account_disabled_auth_error 置 error）。
// gemini/antigravity 判定与历史条件逐字一致，nil 与其余平台走 gateway。
func chatCompletionsAccountForwardPath(account *service.Account) string {
	if account == nil {
		return "gateway"
	}
	if account.Platform == service.PlatformDevin {
		return "devin"
	}
	if account.Platform == service.PlatformGemini {
		return "gemini"
	}
	if shouldUseAntigravityCompat(account) {
		return "antigravity"
	}
	return "gateway"
}

// geminiGroupAdmitsAccount 判定 gemini 分组是否放行该账号进入转发阶段。
// 非 gemini 分组不受此闸约束（保持历史行为）；gemini 分组放行 gemini 平台自身、
// Cursor、Devin 等聚合平台，以及 antigravity OAuth 账号——后者由本文件 §5 的
// shouldUseAntigravityCompat 分支以 streamGenerateContent 转发，与 /v1/messages
// 入口的放行范围保持一致。聚合平台（Cursor/Devin）的准入与调度层
// accountAdmissionDecision 保持同口径，避免 handler 层误拒调度层已放行的账号。
func geminiGroupAdmitsAccount(groupPlatform string, account *service.Account) bool {
	if groupPlatform != service.PlatformGemini {
		return true
	}
	if account == nil {
		return false
	}
	switch account.Platform {
	case service.PlatformGemini, service.PlatformCursor, service.PlatformDevin:
		return true
	}
	return shouldUseAntigravityCompat(account)
}

// newChatCompletionsFailoverState 构造 ChatCompletions 入口的 failover 状态：
// gemini 平台使用带固定切换上限的 NewFailoverStateCapped（默认 3、配置
// max_account_switches_gemini 覆盖，闸②终审整改恢复上限）；其余平台使用无
// 固定上限的 NewFailoverState（候选集合边界判定耗尽）。
func newChatCompletionsFailoverState(c *gin.Context, groupPlatform string, maxSwitchesGemini int) *FailoverState {
	if groupPlatform == service.PlatformGemini {
		return NewFailoverStateCapped(maxSwitchesGemini, false).BindRequest(c)
	}
	return NewFailoverState(false).BindRequest(c)
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

// isAlreadyRelayedUpstreamErrorEvent 判定 failover 体是否为「上游自己发出的错误事件」。
//
// 形状：{"error":{"type":"overloaded_error",...}} 或流内的 {"type":"error","error":{...}}。
// 这类错误在流内已经原样写给客户端，网关不应该再补第二条错误帧。
//
// 首字节超时是网关自己合成的（error.type=upstream_first_byte_timeout），不算上游错误事件，
// 它的错误体必须继续由 M1 帧分支写出，故这里显式排除。
func isAlreadyRelayedUpstreamErrorEvent(body []byte) bool {
	if len(strings.TrimSpace(string(body))) == 0 {
		return false
	}
	if !gjson.GetBytes(body, "error").Exists() {
		return false
	}
	return gjson.GetBytes(body, "error.type").String() != service.UpstreamFirstByteTimeoutType
}

// handleCCFailoverExhausted writes a failover-exhausted error in CC format.
func (h *GatewayHandler) handleCCFailoverExhausted(c *gin.Context, lastErr *service.UpstreamFailoverError, streamStarted bool) {
	// 判据只认唯一事实源 gatewayResponseCommitted：CF524 心跳可能已提交 SSE 200，
	// 此时即使 streamStarted 为 false 也只能写流内错误帧，不能再走 JSON 错误体。
	if gatewayResponseCommitted(c, streamStarted) {
		// 2026-11-06 修：上游过载等错误事件在流内已经透传给客户端了，此时再补一条网关
		// 错误帧会让客户端收到两个终止事件；而且网关只能靠猜原因——这里原先一律写
		// "首字节超时"，把"上游过载"硬说成"超时"，既误导排查，也让计费/重试判定失真。
		// 已经写出去的错误内容就是客户端的终止事件，网关不再画蛇添足。
		if lastErr != nil && c.Writer.Size() > 0 && isAlreadyRelayedUpstreamErrorEvent(lastErr.ResponseBody) {
			return
		}
		// 流已开始（心跳已提交 SSE 200）：预算耗尽不得静默 EOF，必须写出协议合规的
		// 流内错误帧，使客户端识别为错误而非"用户主动取消"（方案文档 §1.4）。
		// 文案与 type 取自 SSOT（M1-a / M1-e），不含 abort/cancel 类触发词，且携带可区分
		// 错误类型，使客户端落到「任务被中断」/「服务端错误兜底」分支，而非 userCancelled 回落。
		// 帧形状与 flush 方式沿用 handleStreamingAwareErrorWithCode 既有实现，不写
		// content/delta 字段，避免污染 assistant 内容。
		service.MarkOpsStreamError(c, service.UpstreamFirstByteTimeoutType, infraerrors.UpstreamFirstByteTimeoutStreamFrame, http.StatusGatewayTimeout)
		if flusher, ok := c.Writer.(http.Flusher); ok {
			// 与 handleStreamingAwareErrorWithCode 同型：SSE 错误帧固定 schema，直拼避免 Marshal。
			errorEvent := `data: {"type":"error","error":{"type":` +
				strconv.Quote(service.UpstreamFirstByteTimeoutType) +
				`,"message":` + strconv.Quote(infraerrors.UpstreamFirstByteTimeoutStreamFrame) + `}}` + "\n\n"
			if _, err := fmt.Fprint(c.Writer, errorEvent); err != nil {
				_ = c.Error(err)
			}
			flusher.Flush()
		}
		return
	}
	if lastErr != nil {
		copyFailoverRetryAfter(c, lastErr.ResponseHeaders)
	}
	// 2026-11-04 方案 §2.4-7：Cursor 账号池真耗尽（全是额度耗尽/拒付账号）统一中文终态
	// CUR-RES-11，不再透出上游原文或内部 reason。
	if lastErr != nil && service.IsCursorPoolUnavailableReason(lastErr.Reason) {
		message := cursorPoolExhaustedMessage(c)
		status := cursorPoolExhaustedStatus(lastErr)
		service.SetOpsUpstreamError(c, status, message, "")
		h.chatCompletionsErrorResponse(c, status, "server_error", message)
		return
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
	if lastErr != nil && lastErr.Reason == service.CursorQuotaExhaustedReason && strings.TrimSpace(lastErr.ClientMessage) != "" {
		status := lastErr.ClientStatusCode
		if status <= 0 {
			status = http.StatusTooManyRequests
		}
		h.chatCompletionsErrorResponse(c, status, "server_error", lastErr.ClientMessage)
		return
	}
	finalMessage := infraerrors.AllAccountsExhausted
	if statusCode == http.StatusNotFound {
		finalMessage = infraerrors.UpstreamModelNotFound
	}
	h.chatCompletionsErrorResponse(c, statusCode, "server_error", finalMessage)
}

// writeCCCursorUserError 在 Cursor 账号的 Execute 错误上按 service.CursorUserFacingError
// 写回中文提示与对应状态码（方案 §2.3 M3）；返回 false 表示未处理，调用方走原兜底路径。
// 客户端已断开或响应已提交时不处理，交回 ensureForwardErrorResponse 按原规则收尾；
// Writer 已写过（ping 已 flush）时只发流内错误帧，不改状态码。
func (h *GatewayHandler) writeCCCursorUserError(c *gin.Context, reqLog *zap.Logger, account *service.Account, err error, streamStarted bool) bool {
	if !isCursorPlatformAccount(account) || c == nil || c.Writer == nil || c.Request == nil {
		return false
	}
	if errors.Is(c.Request.Context().Err(), context.Canceled) || service.IsResponseCommitted(c) {
		return false
	}
	status, message, ok := service.CursorUserFacingError(c.Request.Context(), err)
	if !ok {
		return false
	}
	fields := []zap.Field{
		zap.Int("status", status),
		zap.String("error_class", service.CursorStreamErrClassForLog(err)),
	}
	fields = append(fields, service.CursorFrameErrFieldsForLog(err)...)
	reqLog.Warn("cursor_user_error", fields...)
	errType := "upstream_error"
	if status >= 400 && status < 500 {
		errType = "invalid_request_error"
	}
	// 不再就地同步 streamStarted：下游 handleStreamingAwareErrorWithCode 用唯一事实源
	// gatewayResponseCommitted 自行判定 c.Writer.Written()。
	h.handleStreamingAwareError(c, status, errType, message, streamStarted)
	return true
}
