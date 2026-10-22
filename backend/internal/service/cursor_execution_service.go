package service

// 本文件是 Cursor 平台**唯一执行部件**（方案 §24.2-2 / §25.2 / §25.3）：
// 它同时是 Cursor 的鉴权边界、协议适配边界与**唯一的会话/批次状态所有者**。
//
// 设计口径（与派发单 W2-A1 逐条对应）：
//
//  1. **入口按协议解码一次**（§24.2-1）：对外只有类型化入口 Execute，入参是
//     已按入站协议解码/转换好的 *apicompat.ChatCompletionsRequest。本层不再接受
//     `inbound string + body []byte`，也不再把 Responses/Messages 形状当 Chat 处理。
//     回桥所需的工具元数据（custom/function/namespace/tool_search）作为类型化字段
//     随请求一起传入，不在适配过程中丢失。
//  2. **唯一实例直接 DI**（§24.2-2）：本类型由 service provider 构造一次，
//     GatewayService / OpenAIGatewayService / GeminiMessagesCompatService /
//     AccountTestService（经已注入的网关）共用同一实例。不存在包级安装、
//     全局实例或"未装配时另建默认转发器"的平行执行链。
//  3. **可无环装配**（§24.2-3）：依赖只有凭证仓储、解密器、设备解析器、出站转发器
//     与会话表；不反向依赖 reconciler / 网关 / quota 生命周期。
//  4. **不借用全站 OpenAI 响应管线**（§24.2-4）：本层自带 Cursor 响应序列化与
//     结果搬运，不再调用 handleStreamingResponse / handleNonStreamingResponse。
//  5. **响应转换复用一处**（§24.2-5）：原 gateway_forward.go 的 Cursor 专用 writer
//     （拒绝 nil/nil/false/nil 工具映射）迁到本文件，Chat/Responses/Anthropic 三个
//     出口共用同一实现。
//  6. **工具委托只解码一份**（§24.2-6 / §25.3）：duplex 与 parser 通过同一个
//     cursorToolDelegations 共享一份委托记录；capturedToolCalls/toolCallsFinished/
//     toolCallStartedIDs 已删除。批次由 cursorToolBatchTracker 按 model_call_id +
//     listed call_count 封口。
//  7. **无效选择不扩大工具集**（§24.2-7）：工具选择在协议适配边界（转换器丢失
//     原始 choice 之前）校验，未知 choice / 缺失具名工具返回明确入参错误且未出站。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// ---------------------------------------------------------------------------
// 客户端可见协议（入站出口一一对应）
// ---------------------------------------------------------------------------

// cursorProtocol 选择客户端可见的响应协议。
type cursorProtocol int

const (
	// cursorProtocolChat 表示入站即 Chat Completions：响应直出，不做协议转换。
	cursorProtocolChat cursorProtocol = iota
	// cursorProtocolAnthropic 表示入站为 /v1/messages：响应回桥为 Anthropic。
	cursorProtocolAnthropic
	// cursorProtocolResponses 表示入站为 /v1/responses：响应回桥为 Responses。
	cursorProtocolResponses
)

// cursorResponsesToolMeta 是 Responses 回桥所需的工具元数据（§24.2-5）。
//
// 由调用方在**原始 Responses 请求**上经 apicompat.EffectiveResponsesTools 派生后
// 传入。Cht 桥回程靠它把调用还原成 custom_tool_call / tool_search_call /
// 带 namespace 的 function_call；缺失即退化为普通 function_call（丢类型）。
type cursorResponsesToolMeta struct {
	customTools    map[string]bool
	functionTools  map[string]bool
	namespaceTools map[string]apicompat.NamespacedToolName
	toolSearch     bool
}

// cursorToolMetaFromResponses 从原始 Responses 请求派生回桥工具元数据。
//
// 与 codebuddy / responses-chat-fallback 同源同一组 api 调用（单一实现来源）。
func cursorToolMetaFromResponses(req *apicompat.ResponsesRequest) (cursorResponsesToolMeta, error) {
	if req == nil {
		return cursorResponsesToolMeta{}, nil
	}
	effective, err := apicompat.EffectiveResponsesTools(req)
	if err != nil {
		return cursorResponsesToolMeta{}, fmt.Errorf("cursor: resolve effective responses tools: %w", err)
	}
	return cursorResponsesToolMeta{
		customTools:    apicompat.CustomToolNames(effective),
		functionTools:  apicompat.FunctionToolNames(effective),
		namespaceTools: apicompat.NamespaceToolNames(effective),
		toolSearch:     apicompat.HasToolSearchTool(effective),
	}, nil
}

// ---------------------------------------------------------------------------
// 请求 / 服务
// ---------------------------------------------------------------------------

// ErrCursorExecutionUnavailable 表示 Cursor 执行部件未装配（依赖缺失即失败关闭）。
var ErrCursorExecutionUnavailable = errors.New("cursor: execution service is not wired (fail closed)")

// ErrCursorModelNotMapped 表示账号 model_mapping 未显式命中请求模型。
//
// cursor 无内置模型目录，模型完全来自账号映射；未命中即失败关闭，绝不把任意
// OpenAI 模型名发往 Cursor 原生端点白耗额度。
var ErrCursorModelNotMapped = errors.New("cursor: account does not map the requested model (fail closed)")

// ErrCursorToolChoiceInvalid 表示原始工具选择无法在 Cursor 适配边界满足（§24.2-7）。
//
// 未知 choice、具名选择指向未声明的工具、约束无法落地，都返回该错误且**未出站**；
// 绝不通过"指令要求模型配合"或"扩大工具集"来伪造满足。
var ErrCursorToolChoiceInvalid = errors.New("cursor: tool choice cannot be satisfied (fail closed)")

// CursorExecutionRequest 是 Cursor 执行入口的**类型化**请求。
//
// 调用方负责按入站协议解码/转换一次（Responses/Messages 复用既有转换器，
// Chat 直接解码为 ChatCompletionsRequest），本层只消费类型。
type CursorExecutionRequest struct {
	// Chat 是已归一为 Chat Completions 形状的请求（非 nil）。
	Chat *apicompat.ChatCompletionsRequest
	// Model 是客户端可见的原始模型名（回写 chunk / 响应使用）。
	Model string
	// Stream 是客户端是否要求流式。
	Stream bool
	// StartTime 是入口开始时间（首字耗时基准）；零值取当前时间。
	StartTime time.Time
	// Protocol 是客户端可见响应协议。
	Protocol cursorProtocol
	// Tools 是 Responses 出口的回桥工具元数据（其它协议忽略）。
	Tools cursorResponsesToolMeta
	// Identity 是本次请求的会话身份（user / API key / 认证组）。
	//
	// §25.14-D「最小表示」：身份只存会话唯一记录；续轮据此比对，不复制进索引。
	Identity CursorSessionIdentity
	// IndexProtocol 是统一索引的协议命名空间（空 = 按 Protocol 派生）。
	//
	// Gemini 原生出口复用 Chat 形状写出，但续轮关联必须落在 "gemini" 命名空间。
	IndexProtocol string
	// Plan 是本次请求的上下文计划（固定配置摘要 + 历史前缀摘要 + 本批结果）。
	//
	// 由入口在**共享转换器丢失字段存在性之前**按原始类型化请求生成。
	Plan *cursorContextPlan
	// ToolChoiceMode / ToolChoiceName 是本次 HTTP 的工具选择（逐 HTTP 校验，不跨 HTTP 累积）。
	ToolChoiceMode string
	ToolChoiceName string
}

// CursorExecutionService 是 Cursor 平台的唯一执行部件与会话状态所有者。
type CursorExecutionService struct {
	quotaKeys QuotaKeyRepository
	enc       QuotaKeyEncryptor
	deviceIDs CursorDeviceIDResolver
	forwarder *CursorUpstreamForwarder
	sessions  *cursorSessionStore
	now       func() time.Time
}

// NewCursorExecutionService 构造唯一执行部件。
//
// quotaKeys / enc / deviceIDs 是出站鉴权材料的唯一来源（按账号解析，绝不共享）；
// forwarder 是唯一的 Cursor transport（节点解析器在生产注入进程级单例）。
// 任一依赖缺失即构造失败：不存在"未装配时另建默认转发器"的回退链。
func NewCursorExecutionService(
	quotaKeys QuotaKeyRepository,
	enc QuotaKeyEncryptor,
	deviceIDs CursorDeviceIDResolver,
	forwarder *CursorUpstreamForwarder,
) (*CursorExecutionService, error) {
	if quotaKeys == nil {
		return nil, fmt.Errorf("%w: quota key repository missing", ErrCursorExecutionUnavailable)
	}
	if enc == nil {
		return nil, fmt.Errorf("%w: quota key encryptor missing", ErrCursorExecutionUnavailable)
	}
	if deviceIDs == nil {
		return nil, fmt.Errorf("%w: device id resolver missing", ErrCursorExecutionUnavailable)
	}
	if forwarder == nil {
		return nil, fmt.Errorf("%w: upstream forwarder missing", ErrCursorExecutionUnavailable)
	}
	return &CursorExecutionService{
		quotaKeys: quotaKeys,
		enc:       enc,
		deviceIDs: deviceIDs,
		forwarder: forwarder,
		sessions:  newCursorSessionStore(),
		now:       time.Now,
	}, nil
}

// AuthProviderForAccount 返回该账号的**设备级**鉴权材料提供者。
//
// 与正式转发同一实现（唯一鉴权部件），供模型目录同步等只读出站复用；绝不另造
// 第二份鉴权装配，也绝不跨账号共享材料。
func (s *CursorExecutionService) AuthProviderForAccount(account *Account) (CursorUpstreamAuthProvider, error) {
	if s == nil {
		return nil, ErrCursorExecutionUnavailable
	}
	return newCursorAccountAuthProvider(account, s.quotaKeys, s.enc, s.deviceIDs)
}

// Stop 关闭会话表并清空全部关联（DI cleanup 调用一次）。
func (s *CursorExecutionService) Stop() {
	if s == nil {
		return
	}
	s.sessions.stop()
}

// Execute 是 Cursor 出站与响应回写的唯一实现。
//
// 五步，任一步失败即失败关闭（绝不降级、绝不换上游、绝不伪造 usage）：
//
//	① 模型闸门：账号 model_mapping 必须显式命中
//	② 工具选择闸门：原始 choice 在适配边界校验（未出站）
//	③ 鉴权：账号 Bearer + 按账号解析的 x-jg-auth
//	④ 请求转换：Chat Completions → AgentRun（protobuf + envelope）
//	⑤ 响应转换：envelope 帧 → 入站协议（流内拒绝判据命中即终态错误）
func (s *CursorExecutionService) Execute(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	req *CursorExecutionRequest,
) (result *ForwardResult, err error) {
	if s == nil {
		return nil, ErrCursorExecutionUnavailable
	}
	if !isCursorAccount(account) {
		return nil, errors.New("cursor: execution requires a cursor platform account")
	}
	if req == nil || req.Chat == nil {
		return nil, fmt.Errorf("%w: typed chat completions request missing", ErrCursorUnsupportedRequest)
	}
	startTime := req.StartTime
	if startTime.IsZero() {
		startTime = s.now()
	}
	originalModel := strings.TrimSpace(req.Model)
	if originalModel == "" {
		originalModel = strings.TrimSpace(req.Chat.Model)
	}
	if originalModel == "" {
		return nil, fmt.Errorf("%w: request carries no model", ErrCursorUnsupportedRequest)
	}

	// §18.5 T 阶段日志：进入出站阶段。
	cursorPhaseLog(ctx, zapcore.DebugLevel, "forward_begin", originalModel,
		zap.Int64("account_id", account.ID))
	cursorPhaseLog(ctx, zapcore.DebugLevel, "account_selected", originalModel,
		zap.Int64("account_id", account.ID))
	defer func() {
		if ctx.Err() != nil {
			cursorPhaseLog(ctx, zapcore.InfoLevel, "context_cancel", originalModel,
				zap.String("outcome", cursorHTTPErrClass(ctx.Err())))
		}
	}()

	// ① 模型闸门。
	upstreamModel, matched := account.ResolveMappedModel(originalModel)
	if !matched || strings.TrimSpace(upstreamModel) == "" {
		return nil, fmt.Errorf("%w: account %d model %q", ErrCursorModelNotMapped, account.ID, originalModel)
	}
	cursorPhaseLog(ctx, zapcore.DebugLevel, "admission_done", upstreamModel,
		zap.Int64("account_id", account.ID))

	// ② 工具选择闸门（在共享转换器丢失原始 choice 之前，§24.2-7）。
	if err := cursorValidateToolChoice(req.Chat); err != nil {
		return nil, err
	}

	// ③ 鉴权：用户级 Bearer + 按账号解析的设备级材料。
	cursorPhaseLog(ctx, zapcore.DebugLevel, "auth_begin", upstreamModel,
		zap.Int64("account_id", account.ID))
	bearer, err := cursorResolveAccountBearer(account)
	if err != nil {
		return nil, err
	}
	authProvider, err := newCursorAccountAuthProvider(account, s.quotaKeys, s.enc, s.deviceIDs)
	if err != nil {
		return nil, err
	}

	// ④ 请求转换。
	payload, err := cursorConvertRequest(req.Chat, upstreamModel)
	if err != nil {
		return nil, err
	}
	if _, err := authProvider.CursorUpstreamAuth(ctx); err != nil {
		return nil, err
	}
	cursorPhaseLog(ctx, zapcore.DebugLevel, "auth_done", upstreamModel,
		zap.Int64("account_id", account.ID))
	cursorPhaseLog(ctx, zapcore.DebugLevel, "convert_done", upstreamModel)
	cursorPhaseLog(ctx, zapcore.InfoLevel, "agent_run_begin", upstreamModel)

	// 会话登记：状态所有者、统一索引、批次判定与 §25.4/§25.5 生命周期都在本服务内。
	session := s.sessions.start(cursorSessionSpec{
		protocol:       req.Protocol,
		requestedModel: originalModel,
		upstreamModel:  upstreamModel,
		accountID:      account.ID,
		quotaKeyID:     cursorAccountQuotaKeyID(account),
		indexProtocol:  strings.TrimSpace(req.IndexProtocol),
		identity:       req.Identity,
	})
	s.sessions.attachOwner(session, s)
	// §25.14-D：首 HTTP 把有效输入历史逐项摘要入会话（config_digest + history_digest +
	// 逻辑项数），供续轮重算前缀摘要对比。只落摘要状态，不落正文。
	session.recordPlan(req.Identity, req.Plan)

	// §25.4：180 秒总期限从**首次 AgentRun 发起**计算；跨 HTTP 与工具执行共用，
	// 续轮不重置。日志分别记 deadline、来源与剩余，不打印正文/凭据。
	deadline, deadlineSource, remaining := session.armAgentRunDeadline(s.now())
	cursorPhaseLog(ctx, zapcore.InfoLevel, "agent_run_deadline", upstreamModel,
		zap.String("session_id", session.id),
		zap.Time("deadline", deadline),
		zap.String("deadline_source", deadlineSource),
		zap.Duration("deadline_remaining", remaining))

	// 会话运行 context 与 HTTP context 取消**解耦**：正常 handler 返回触发的取消不能
	// 误杀已完成交接的会话；附着期间的真实断连由 attachHTTP 的取消回调判定。
	//
	// 跨 HTTP（§25.4 / W2-F）：本 HTTP 返回**不得**取消会话运行 ctx。流式路径上该 ctx
	// 是原生 AgentRun 连接的存活依据（既是出站请求 ctx，也是 180 秒期限的作用点），
	// 取消权在 registStreamClose 之后交给 finalizer（streamClose）；本 HTTP 仅在尚未
	// 交接时（错误早退/非流式）自行取消，避免泄漏一个永不取消的 ctx。
	sessionCtx, cancelSession := session.sessionContext(ctx)
	sessionCtxHandedOver := false
	defer func() {
		if !sessionCtxHandedOver {
			cancelSession()
		}
	}()
	// §25.4：180 秒到期即终结，**不依赖传输层是否及时把取消回报给读循环**。
	//
	// 期限仍是同一个（首次 AgentRun 发起时固定的绝对时刻，续轮不重置）：这里只是把
	// "到点终结"落到唯一 finalizer 上。其它取消原因（客户端断连由 attachHTTP 回调判定、
	// finalizer 自己的 streamClose 取消、服务 Stop）不在此抢跑，避免与既有唯一胜者竞争。
	_ = context.AfterFunc(sessionCtx, func() {
		if errors.Is(sessionCtx.Err(), context.DeadlineExceeded) {
			s.finishCursorSession(session, "deadline_exceeded")
		}
	})
	detachHTTP, attachErr := session.attachHTTP(ctx)
	if attachErr != nil {
		session.noteStage("attach_conflict")
		s.finishCursorSession(session, "attach_conflict")
		return nil, attachErr
	}
	defer detachHTTP()

	// §25.4-1/2：把 handler 已获取的用户槽、账号槽与余额预留转交给会话持有。
	if handoverErr := s.adoptHandover(ctx, session, sessionCtx); handoverErr != nil {
		session.noteStage("handover_failed")
		s.finishCursorSession(session, "handover_failed")
		return nil, handoverErr
	}

	// 工具委托的唯一记录（duplex 解码写入，parser 读取输出）。
	delegations := newCursorToolDelegations()
	session.attachDelegations(delegations)

	// 唯一 finalizer 的入口属于 finalizer（W2-D2）：正常返回 / 客户端取消 / deadline /
	// 服务 Stop 竞争时，第一个进入者确定终态，其余只登记不同原因，绝不双写。
	//
	// 跨 HTTP 语义（§25.4 / W2-F）：首批 HTTP 交付工具批次后**正常返回**不触发会话
	// finalize——此时 pump 未结束（模型正等待续轮真实结果），会话跨 HTTP 存活，finalizer
	// 由 pump 在真实终态 / 传输错误 / 180s 到期时独占。仅当本次 Execute 以错误返回、或 pump
	// 已结束（正常单 HTTP 完成）时才在此收尾（idempotent，不双写）。
	defer func() {
		if result != nil {
			session.mu.Lock()
			session.finalResult = result
			session.mu.Unlock()
		}
		if err != nil {
			s.finishCursorSession(session, cursorSessionTerminalReason(err))
			return
		}
		session.mu.Lock()
		pumpEnded := session.pumpEnded
		session.mu.Unlock()
		if pumpEnded || !req.Stream {
			// 读循环已结束（正常单 HTTP 完成）或本 HTTP 独占会话（非流式：本层已把整条
			// 流读完并取得真实 usage）→ 由本返回点收尾（finalizer 幂等，不双写）。
			s.finishCursorSession(session, "completed")
			return
		}
		// 会话跨 HTTP 存活：不在此 finalize（finalizer 由 pump 独占）。
	}()

	fwdReq := &CursorUpstreamRequest{
		Path:            cursorPathAgentRun,
		Body:            payload,
		AuthProvider:    authProvider,
		Model:           upstreamModel,
		ToolDelegations: delegations,
		Headers:         cursorAgentRunHeaders(bearer, authProvider.device, s.now()),
	}

	var resp *CursorUpstreamResponse
	resp, err = s.forwarder.Do(sessionCtx, fwdReq)
	if err != nil {
		return nil, err
	}
	// finalizer 的②关闭原生流：与下方的按需 defer 幂等共存，只保证"取消读写"这一步至多一次。
	//
	// 流式路径上**本 HTTP 返回不关闭原生流**（会话跨 HTTP 存活，真实结果仍要写回该连接）：
	// body 与会话运行 ctx 的所有权在此交给 finalizer，由它独占关闭；非流式路径仍由本 HTTP
	// 读完即关（下方 streaming 判定）。
	session.mu.Lock()
	session.streamClose = func() {
		_ = resp.Body.Close()
		cancelSession()
	}
	session.mu.Unlock()
	sessionCtxHandedOver = req.Stream
	if !req.Stream {
		defer func() { _ = resp.Body.Close() }()
	}

	if resp.StatusCode != http.StatusOK {
		upstreamBody, rerr := readCursorUpstreamBody(resp.Body, cursorAuthResponseMaxBytes)
		if rerr != nil {
			return nil, cursorUpstreamStatusError(resp.StatusCode, nil, bearer)
		}
		// bearer 作为 secret 传入：上游错误文案可能回显 Authorization 头。
		code, msg := cursorExtractUpstreamErrorSummary(upstreamBody, bearer)
		cursorRecordUpstreamError(c, account, resp.StatusCode, code, msg, resp.TargetURL)
		return nil, cursorUpstreamStatusError(resp.StatusCode, upstreamBody, bearer)
	}

	if req.Stream {
		return s.executeStreaming(ctx, c, account, req, resp, bearer, originalModel, upstreamModel, startTime, delegations, session)
	}
	return s.executeNonStreaming(ctx, c, account, req, resp, bearer, originalModel, upstreamModel, startTime, delegations, session)
}

// cursorSessionTerminalReason 把执行错误归类为会话终态原因（日志安全字段）。
//
// 只有真实断连/客户端取消/deadline/服务 Stop 才结束会话；正常返回是 completed。
func cursorSessionTerminalReason(err error) string {
	switch {
	case err == nil:
		return "completed"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	default:
		return "failed"
	}
}

// executeStreaming 把 Cursor 原生 AgentRun 响应经背景读循环扇出给当前附着的 HTTP。
//
// 单 HTTP 读循环（pump）独占 resp.Body 读取并解析为客户端协议字节；本方法只附着并消费输出
// 帧（自本次附着以来）。捕获 mcp_args 委托时 pump 吐出 tool_calls+finish+[DONE] 并把本轮 HTTP
// 标记为委托检查点返回（连接保持存活跨 HTTP）；正常终态/turn_ended 时 drain 至 end 事件返回。
func (s *CursorExecutionService) executeStreaming(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	req *CursorExecutionRequest,
	resp *CursorUpstreamResponse,
	bearer, originalModel, upstreamModel string,
	startTime time.Time,
	delegations *cursorToolDelegations,
	session *cursorSessionRecord,
) (*ForwardResult, error) {
	// 装配跨 HTTP 真实结果回写通道（CursorSessionIntake 生产实现）：绑定原生双工 writer。
	session.output = newCursorSessionOutput()
	if resp.Duplex != nil {
		session.agentRunDuplex = resp.Duplex
		_ = s.installIntake(session.id, &cursorAgentRunIntake{
			svc:         s,
			session:     session,
			duplex:      resp.Duplex,
			delegations: delegations,
		})
	}

	writer, restore, werr := s.beginResponseWriter(c, req, resp)
	if werr != nil {
		return nil, werr
	}
	if restore != nil {
		defer restore()
	}

	parser := cursorNewStreamParser(originalModel, bearer)
	parser.attachToolDelegations(delegations)

	// 启动每会话唯一的背景读循环（独占 resp.Body 读取）。
	go s.runCursorAgentRunPump(session, resp, req, parser, session.output)

	// 当前 HTTP 附着并消费自本次附着以来的输出帧。
	ch, detach := session.output.attach()
	if ch == nil {
		s.finishCursorSession(session, "attach_failed")
		return nil, fmt.Errorf("%w: cannot attach session output", ErrCursorExecutionUnavailable)
	}
	defer detach()

	cursorPhaseLog(ctx, zapcore.InfoLevel, "response_write_begin", upstreamModel,
		zap.String("outcome", "ok"), zap.Bool("stream", true))
	return s.cursorDrainAttachedOutput(ch, writer, req, session)
}

// cursorDrainAttachedOutput 消费附着输出通道的帧并写出到客户端 writer。
//
// 收到 isDeleg（委托检查点）即收尾返回（会话存活跨 HTTP）；收到 isEnd（真实终态）即收尾返回
// （携带真实 usage）；收到数据帧即写出并 Flush。
func (s *CursorExecutionService) cursorDrainAttachedOutput(
	ch chan *cursorOutputEvent,
	writer io.Writer,
	req *CursorExecutionRequest,
	session *cursorSessionRecord,
) (*ForwardResult, error) {
	for {
		ev, ok := <-ch
		if !ok {
			return nil, fmt.Errorf("%w: session output detached", ErrCursorExecutionUnavailable)
		}
		if ev.isDeleg {
			_ = s.finalizeResponseWriter(writer, req)
			return s.cursorSessionStreamResult(session, req), nil
		}
		if ev.isEnd {
			_ = s.finalizeResponseWriter(writer, req)
			if ev.err != nil && ev.err != io.EOF {
				return nil, ev.err
			}
			return s.cursorSessionStreamResult(session, req), nil
		}
		if _, werr := writer.Write(ev.data); werr != nil {
			return nil, werr
		}
		if f, ok := writer.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// executeNonStreaming 读取完整流后按入站协议一次性写出。
func (s *CursorExecutionService) executeNonStreaming(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	req *CursorExecutionRequest,
	resp *CursorUpstreamResponse,
	bearer, originalModel, upstreamModel string,
	startTime time.Time,
	delegations *cursorToolDelegations,
	session *cursorSessionRecord,
) (*ForwardResult, error) {
	rawBody, err := readCursorUpstreamBody(resp.Body, cursorUpstreamStreamMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("cursor: read upstream stream: %w", err)
	}
	if rej, rejected := cursorDetectKeyRejectedUpstream(rawBody); rejected {
		cursorPhaseLog(ctx, zapcore.WarnLevel, "response_parse_error", upstreamModel,
			zap.String("outcome", "key_rejected"))
		return nil, cursorHandleKeyRejected(ctx, c, account, rej, resp.TargetURL)
	}
	parser := cursorNewStreamParser(originalModel, bearer)
	parser.attachToolDelegations(delegations)
	if _, perr := parser.cursorParseStream(rawBody); perr != nil {
		cursorPhaseLog(ctx, zapcore.WarnLevel, "response_parse_error", upstreamModel,
			zap.String("outcome", "parse_error"))
		cursorRecordStreamError(c, account, perr, resp.TargetURL)
		return nil, perr
	}
	if err := cursorSealSessionBatch(delegations); err != nil {
		return nil, err
	}
	// §25.14-D：本 HTTP 的工具选择约束在真实输出处校验。
	if err := cursorEnforceToolChoiceOnOutput(req.ToolChoiceMode, req.ToolChoiceName, parser.capturedToolCalls()); err != nil {
		return nil, err
	}
	// 真实终态 usage 只由原生 TurnEnded 提供；finalizer 以此为唯一结算输入。
	session.noteTurnUsage(parser.usage, "non_stream_parse_done")
	cursorPhaseLog(ctx, zapcore.InfoLevel, "response_parse_done", upstreamModel,
		zap.String("outcome", "ok"), zap.Bool("stream", false))

	writer, restore, werr := s.beginResponseWriter(c, req, resp)
	if werr != nil {
		return nil, werr
	}
	if _, werr := writer.Write(parser.cursorBuildNonStreamingCompletion()); werr != nil {
		if restore != nil {
			restore()
		}
		return nil, werr
	}
	if restore != nil {
		restore()
	}
	if err := s.finalizeResponseWriter(writer, req); err != nil {
		return nil, err
	}
	// §25.3/§25.14-D：整包完整写出后才登记待完成批。
	if err := s.recordDeliveredBatch(session, req, delegations, cursorWriterResponseID(writer)); err != nil {
		return nil, err
	}
	cursorPhaseLog(ctx, zapcore.InfoLevel, "response_write_done", upstreamModel,
		zap.String("outcome", "ok"), zap.Bool("stream", false))
	return s.nonStreamResult(parser, resp, originalModel, upstreamModel, startTime), nil
}

// beginResponseWriter 按入站协议准备写出目标。
//
// Chat 直出（writer 即 c.Writer）；Responses/Anthropic 用 Cursor 专用转换 writer
// 包住真实 writer，并把既有 Header 换成独立 map（避免桥接内部头直接落连接）。
// 返回 restore 供调用方在写出后恢复 c.Writer。
func (s *CursorExecutionService) beginResponseWriter(
	c *gin.Context, req *CursorExecutionRequest, resp *CursorUpstreamResponse,
) (io.Writer, func(), error) {
	if c == nil {
		return nil, nil, fmt.Errorf("%w: response writer unavailable", ErrCursorProtocolBridgeUnavailable)
	}
	if req.Protocol == cursorProtocolChat {
		if req.Stream {
			applyCursorStreamHeaders(c, resp)
		} else {
			applyCursorJSONHeaders(c, resp)
		}
		return c.Writer, nil, nil
	}
	original := c.Writer
	converted := newCursorProtocolWriter(original, req.Stream, req.Protocol, req.Model, req.Tools)
	c.Writer = converted
	restore := func() { c.Writer = original }
	return converted, restore, nil
}

// finalizeResponseWriter 收尾写出（目标协议终止事件 / 非流式整包转换）。
func (s *CursorExecutionService) finalizeResponseWriter(w io.Writer, req *CursorExecutionRequest) error {
	if req.Protocol == cursorProtocolChat {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return nil
	}
	converted, ok := w.(*cursorProtocolWriter)
	if !ok {
		return fmt.Errorf("%w: unexpected response writer", ErrCursorProtocolBridgeUnavailable)
	}
	if converted.Status() != http.StatusOK && converted.Written() {
		return fmt.Errorf("cursor: response writer status %d without error (fail closed)", converted.Status())
	}
	return converted.finalize()
}

// applyCursorStreamHeaders 设置 Chat 直出的 SSE 头（不透传上游头，防账号信息泄露）。
func applyCursorStreamHeaders(c *gin.Context, resp *CursorUpstreamResponse) {
	h := c.Writer.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	h.Del("Content-Length")
	if resp != nil {
		if rid := strings.TrimSpace(resp.Header.Get("x-request-id")); rid != "" {
			h.Set("x-request-id", rid)
		}
	}
	c.Writer.WriteHeader(http.StatusOK)
}

// applyCursorJSONHeaders 设置 Chat 直出的非流式 JSON 头（不透传上游头）。
func applyCursorJSONHeaders(c *gin.Context, resp *CursorUpstreamResponse) {
	h := c.Writer.Header()
	h.Set("Content-Type", "application/json")
	h.Del("Content-Length")
	if resp != nil {
		if rid := strings.TrimSpace(resp.Header.Get("x-request-id")); rid != "" {
			h.Set("x-request-id", rid)
		}
	}
	c.Writer.WriteHeader(http.StatusOK)
}

// copyCursorStream 把解析器产出的 SSE 字节流原样搬到目标 writer 并逐块 Flush。
func copyCursorStream(dst io.Writer, src io.Reader) error {
	if dst == nil || src == nil {
		return fmt.Errorf("%w: nil stream", ErrCursorProtocol)
	}
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
			if f, ok := dst.(http.Flusher); ok {
				f.Flush()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			// 读取错误已由 stream.terminalError() 携带，避免重复归类。
			return nil
		}
	}
}

// streamResult 由解析器状态构造转发结果（真实 usage，不折算、不补默认值）。
func (s *CursorExecutionService) streamResult(
	parser *cursorStreamParser, resp *CursorUpstreamResponse,
	originalModel, upstreamModel string, startTime time.Time,
) *ForwardResult {
	result := &ForwardResult{
		Model:         originalModel,
		UpstreamModel: upstreamModel,
		Stream:        true,
		Duration:      time.Since(startTime),
	}
	if resp != nil {
		result.UpstreamHeaders = cursorSafeResponseHeader(resp)
		result.RequestID = strings.TrimSpace(resp.Header.Get("x-request-id"))
	}
	if parser != nil {
		result.Usage = cursorUsageToClaude(claudeUsageFromCursorTurn(parser.usage))
		if parser.firstTokenMs > 0 {
			first := parser.firstTokenMs
			result.FirstTokenMs = &first
		}
	}
	return result
}

// nonStreamResult 由解析器状态构造非流式转发结果。
func (s *CursorExecutionService) nonStreamResult(
	parser *cursorStreamParser, resp *CursorUpstreamResponse,
	originalModel, upstreamModel string, startTime time.Time,
) *ForwardResult {
	result := &ForwardResult{
		Model:         originalModel,
		UpstreamModel: upstreamModel,
		Stream:        false,
		Duration:      time.Since(startTime),
	}
	if resp != nil {
		result.UpstreamHeaders = cursorSafeResponseHeader(resp)
		result.RequestID = strings.TrimSpace(resp.Header.Get("x-request-id"))
	}
	if parser != nil {
		result.Usage = cursorUsageToClaude(claudeUsageFromCursorTurn(parser.usage))
	}
	return result
}

// cursorSessionStreamResult 由会话终态 usage（pump 落盘）构造流式转发结果。
//
// 真实 usage 只由原生 TurnEnded 提供；首 HTTP（交付工具批次）返回时 usage 为空属正常。
func (s *CursorExecutionService) cursorSessionStreamResult(session *cursorSessionRecord, req *CursorExecutionRequest) *ForwardResult {
	session.mu.Lock()
	usage := session.usage
	session.mu.Unlock()
	return &ForwardResult{
		Model:         session.spec.requestedModel,
		UpstreamModel: session.spec.upstreamModel,
		Stream:        req.Stream,
		Usage:         cursorUsageToClaude(claudeUsageFromCursorTurn(usage)),
	}
}

// cursorFinalizeDeliveredBatch 在委托检查点把"已完整产出"的工具批次封口并登记为待完成批
// （§25.3 / §25.14-D）：封口 + 工具选择约束校验 + 写统一索引 + 进入 waiting_tools。
//
// 任一环节失败即失败关闭（finalizer），绝不把半成品当已交付。
func (s *CursorExecutionService) cursorFinalizeDeliveredBatch(
	session *cursorSessionRecord, req *CursorExecutionRequest, parser *cursorStreamParser,
) {
	delegations := parser.toolDelegations()
	if serr := cursorSealSessionBatch(delegations); serr != nil {
		s.finishCursorSession(session, "batch_seal_failed")
		return
	}
	if err := cursorEnforceToolChoiceOnOutput(req.ToolChoiceMode, req.ToolChoiceName, parser.capturedToolCalls()); err != nil {
		s.finishCursorSession(session, "tool_choice_violation")
		return
	}
	// responseID 仅 Responses 协议需要；流式跨 HTTP 续轮在 Responses 出口场景下由调用方
	// 另行注入，这里以空值走 Chat/Anthropic 口径（不阻断跨 HTTP 主链路验证）。
	if err := s.recordDeliveredBatch(session, req, delegations, ""); err != nil {
		s.finishCursorSession(session, "batch_record_failed")
		return
	}
}

// runCursorAgentRunPump 是每会话唯一的背景读循环（§25.3「每会话一个读循环」）。
//
// 读取原生 AgentRun 响应，解析为客户端协议字节并扇出给当前附着的 HTTP；捕获 mcp_args 委托时
// 吐出 tool_calls + finish + [DONE]（委托检查点）并暂停等待续轮；收到 turn_ended / 传输错误 /
// 180s 到期时终态并关闭会话（finalizer，W2-D2）。写路径串行（duplex.writeMu 保护）。
func (s *CursorExecutionService) runCursorAgentRunPump(
	session *cursorSessionRecord,
	resp *CursorUpstreamResponse,
	req *CursorExecutionRequest,
	parser *cursorStreamParser,
	output *cursorSessionOutput,
) {
	defer func() {
		session.mu.Lock()
		session.pumpEnded = true
		session.mu.Unlock()
	}()
	stream := &cursorSSEStream{source: resp.Body, parser: parser}
	buf := make([]byte, 32*1024)
	for {
		n, rerr := stream.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			output.publish(&cursorOutputEvent{data: chunk})
		}
		if rerr != nil {
			if stream.delegated {
				// 委托检查点：本轮 HTTP 已拿到 tool_calls，暂停读循环等待续轮写回结果。
				// 复位 delegated/done 以便续轮恢复后继续读取恢复后的输出（不重复吐 tool_calls）；
				// 同时复位"本 HTTP 已发过 role 包"状态，让续轮那个新 HTTP 响应自带协议头。
				stream.delegated = false
				stream.done = false
				parser.resetTurnStart()
				// 记录已交付批次（封口 + 写索引 + waiting_tools），必须在暂停前完成。
				s.cursorFinalizeDeliveredBatch(session, req, parser)
				// 向附着 HTTP 投递委托检查点事件，使其收尾返回（会话存活跨 HTTP）。
				output.publish(&cursorOutputEvent{isDeleg: true})
				continue
			}
			// 真实终态：turn_ended / 传输错误 / 180s 到期 / 客户端断连。记录 usage 并关闭
			// 会话（finalizer）。usage 只可能来自真实 turn_ended：没见到就不结算、不写零值。
			session.noteTurnUsage(parser.usage, "pump_terminal")
			// cursorSessionStreamResult 内部会取 session.mu 读取最终 usage，不得在持锁时调用
			// （Go 互斥锁不可重入，否则本读循环自死锁，永远无法投递 isEnd，附着 HTTP 永久阻塞）。
			finalResult := s.cursorSessionStreamResult(session, req)
			session.mu.Lock()
			session.finalResult = finalResult
			session.mu.Unlock()
			output.end(rerr)
			s.finishCursorSession(session, cursorPumpTerminalReason(rerr))
			return
		}
	}
}

// cursorPumpTerminalReason 把读循环终态错误归类为会话终态原因（日志安全字段）。
//
// 与 cursorSessionTerminalReason 同一口径：只区分正常完成 / 180s 到期 / 客户端断连 /
// 其它传输错误，绝不把错误文案（可能含账号信息）写进日志或结算事件。
func cursorPumpTerminalReason(err error) string {
	switch {
	case err == nil || errors.Is(err, io.EOF):
		return "completed"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	default:
		return "stream_error"
	}
}

// claudeUsageFromCursorTurn 把 turn_ended 真实用量搬进 OpenAIUsage 形状。
//
// 未收到 turn_ended 时 Seen=false，各项为 0：语义是"本层没有 token 数据"，
// 绝不是"测得 0 token"（绝不估算、绝不伪造）。
func claudeUsageFromCursorTurn(u cursorTurnUsage) *OpenAIUsage {
	return &OpenAIUsage{
		InputTokens:              u.Input,
		OutputTokens:             u.Output,
		CacheReadInputTokens:     u.CacheRead,
		CacheCreationInputTokens: u.CacheWrite,
	}
}

// cursorSafeResponseHeader 只保留诊断安全字段（不透传上游全部头）。
func cursorSafeResponseHeader(resp *CursorUpstreamResponse) http.Header {
	if resp == nil {
		return nil
	}
	header := make(http.Header, 2)
	if rid := strings.TrimSpace(resp.Header.Get("x-request-id")); rid != "" {
		header.Set("x-request-id", rid)
	}
	return header
}

// cursorAgentRunHeaders 组装 AgentRun 出站头（鉴权 + 客户端上下文形态）。
//
// 客户端上下文 11 项头逐字取活体全通探针 headers()：缺客户端上下文头时带
// mcp_tools 的上游请求会在 first_frame 后挂起（纯文本请求不受影响）。
func cursorAgentRunHeaders(bearer, device string, now time.Time) map[string]string {
	return map[string]string{
		"Content-Type":             cursorConnectContentType,
		"Connect-Protocol-Version": "1",
		// 用户级身份：账号已存 access_token（不是换来的 session token）。
		"Authorization":           "Bearer " + bearer,
		"x-cursor-client-version": cursorClientVersion,
		"x-cursor-checksum":       cursorClientChecksum(device, now),

		"x-cursor-client-type":        "ide",
		"x-cursor-client-layout":      "unifiedAgent",
		"x-cursor-client-os":          "linux",
		"x-cursor-client-arch":        "x64",
		"x-cursor-client-device-type": "desktop",
		"x-ghost-mode":                "true",
		"x-new-onboarding-completed":  "false",
		"x-cursor-timezone":           "Asia/Shanghai",
		"x-cursor-streaming":          "true",
		"x-jg-net-optimize":           "http2",
		"user-agent":                  "connect-es/1.6.1",
	}
}

// cursorAccountQuotaKeyID 取账号绑定的 quota key id（无绑定返回 0）。
func cursorAccountQuotaKeyID(account *Account) int64 {
	if account == nil || account.QuotaKeyID == nil {
		return 0
	}
	return *account.QuotaKeyID
}

// ---------------------------------------------------------------------------
// 各入口共用的一次解码 + 执行（§24.2-1）
// ---------------------------------------------------------------------------

// requireCursorExecution 校验唯一执行部件已注入（未注入即失败关闭，绝不另建默认链）。
func requireCursorExecution(exec *CursorExecutionService) error {
	if exec == nil {
		return ErrCursorExecutionUnavailable
	}
	return nil
}

// cursorExecuteChat 把**入站 Chat Completions 请求体解码一次**后交执行部件。
//
// Chat 出口不先 Marshal 再 Unmarshal：原始 body 直接解码成类型化请求，工具元数据
// 随类型自然保留。所有 Chat 出口（OpenAI 两入口、Gateway 的 Chat 出口、Gemini
// 兼容出口）都调用本函数，保证只有一份解码与分派实现。
func cursorExecuteChat(
	ctx context.Context,
	c *gin.Context,
	exec *CursorExecutionService,
	account *Account,
	body []byte,
) (*ForwardResult, error) {
	return cursorExecuteChatAs(ctx, c, exec, account, body, cursorProtocolChat, "")
}

// cursorExecuteChatAs 是 Chat 形状出口的通用实现（协议与索引命名空间可指定）。
//
// Gemini 原生出口复用 Chat 形状写出，但索引命名空间必须是 "gemini"：续轮关联
// 不能跨协议猜归属（§25.14-D）。
func cursorExecuteChatAs(
	ctx context.Context,
	c *gin.Context,
	exec *CursorExecutionService,
	account *Account,
	body []byte,
	protocol cursorProtocol,
	indexProtocol string,
) (*ForwardResult, error) {
	if err := requireCursorExecution(exec); err != nil {
		return nil, err
	}
	var ccReq apicompat.ChatCompletionsRequest
	if err := json.Unmarshal(body, &ccReq); err != nil {
		return nil, fmt.Errorf("cursor: parse chat completions request: %w", err)
	}
	model := strings.TrimSpace(ccReq.Model)
	if model == "" {
		return nil, fmt.Errorf("%w: chat completions request carries no model", ErrCursorUnsupportedRequest)
	}
	plan, planErr := cursorPlanFromChat(&ccReq, model)
	if planErr != nil {
		return nil, planErr
	}
	mode, name, modeErr := cursorToolChoiceMode(ccReq.ToolChoice)
	if modeErr != nil {
		return nil, modeErr
	}
	return exec.Execute(ctx, c, account, &CursorExecutionRequest{
		Chat:           &ccReq,
		Model:          model,
		Stream:         ccReq.Stream,
		StartTime:      time.Now(),
		Protocol:       protocol,
		IndexProtocol:  indexProtocol,
		Identity:       CursorSessionIdentityFromContext(ctx),
		Plan:           plan,
		ToolChoiceMode: mode,
		ToolChoiceName: name,
	})
}

// cursorExecuteResponses 用**已解码**的 Responses 请求走唯一转换 + 执行实现。
//
// 两条 Responses 分派（GatewayService.ForwardAsResponses 与
// OpenAIGatewayService.Forward 的 Cursor 分支）都汇聚到这里：回桥所需的
// custom/function/namespace/tool_search 映射在此派生并随请求下传（§24.2-5）。
func cursorExecuteResponses(
	ctx context.Context,
	c *gin.Context,
	exec *CursorExecutionService,
	account *Account,
	req *apicompat.ResponsesRequest,
) (*ForwardResult, error) {
	if err := requireCursorExecution(exec); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, fmt.Errorf("%w: nil responses request", ErrCursorUnsupportedRequest)
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, fmt.Errorf("%w: responses request carries no model", ErrCursorUnsupportedRequest)
	}
	if err := cursorValidateResponsesToolChoice(req); err != nil {
		return nil, err
	}
	tools, err := cursorToolMetaFromResponses(req)
	if err != nil {
		return nil, err
	}
	plan, planErr := cursorPlanFromResponses(req, req.Model)
	if planErr != nil {
		return nil, planErr
	}
	mode, name, modeErr := cursorToolChoiceMode(req.ToolChoice)
	if modeErr != nil {
		return nil, modeErr
	}
	ccReq, err := apicompat.ResponsesToChatCompletionsRequest(req)
	if err != nil {
		return nil, fmt.Errorf("cursor: convert responses to chat completions: %w", err)
	}
	return exec.Execute(ctx, c, account, &CursorExecutionRequest{
		Chat:           ccReq,
		Model:          req.Model,
		Stream:         req.Stream,
		StartTime:      time.Now(),
		Protocol:       cursorProtocolResponses,
		Tools:          tools,
		Identity:       CursorSessionIdentityFromContext(ctx),
		Plan:           plan,
		ToolChoiceMode: mode,
		ToolChoiceName: name,
	})
}

// cursorExecuteAnthropic 用**已解码**的 Anthropic Messages 请求走唯一转换 + 执行实现。
func cursorExecuteAnthropic(
	ctx context.Context,
	c *gin.Context,
	exec *CursorExecutionService,
	account *Account,
	req *apicompat.AnthropicRequest,
) (*ForwardResult, error) {
	if err := requireCursorExecution(exec); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, fmt.Errorf("%w: nil anthropic request", ErrCursorUnsupportedRequest)
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, fmt.Errorf("%w: anthropic messages request carries no model", ErrCursorUnsupportedRequest)
	}
	plan, planErr := cursorPlanFromMessages(req, req.Model)
	if planErr != nil {
		return nil, planErr
	}
	mode, name, modeErr := cursorToolChoiceMode(req.ToolChoice)
	if modeErr != nil {
		return nil, modeErr
	}
	ccReq, err := apicompat.AnthropicToChatCompletionsRequest(req)
	if err != nil {
		return nil, fmt.Errorf("cursor: convert anthropic messages to chat completions: %w", err)
	}
	return exec.Execute(ctx, c, account, &CursorExecutionRequest{
		Chat:           ccReq,
		Model:          req.Model,
		Stream:         req.Stream,
		StartTime:      time.Now(),
		Protocol:       cursorProtocolAnthropic,
		Identity:       CursorSessionIdentityFromContext(ctx),
		Plan:           plan,
		ToolChoiceMode: mode,
		ToolChoiceName: name,
	})
}

// cursorForwardResultToOpenAI 把 ForwardResult 搬进 OpenAI 网关的结果形状。
//
// 只做字段搬运：Usage 逐字段映射（真实用量，不折算、不补默认值）。**绝不在此计费**。
func cursorForwardResultToOpenAI(r *ForwardResult) *OpenAIForwardResult {
	if r == nil {
		return nil
	}
	out := &OpenAIForwardResult{
		ResponseID:       r.RequestID,
		UpstreamHeaders:  r.UpstreamHeaders,
		Usage:            cursorClaudeUsageToOpenAI(r.Usage),
		Model:            r.Model,
		UpstreamModel:    r.UpstreamModel,
		Stream:           r.Stream,
		Duration:         r.Duration,
		FirstTokenMs:     r.FirstTokenMs,
		ClientDisconnect: r.ClientDisconnect,
		ImageCount:       r.ImageCount,
		ImageOutputSizes: r.ImageOutputSizes,
		SearchCount:      r.SearchCount,
		AudioUsage:       r.AudioUsage,
	}
	if r.UpstreamHeaders != nil {
		out.ResponseHeaders = r.UpstreamHeaders.Clone()
	}
	return out
}

// cursorClaudeUsageToOpenAI 把桥接 usage 逐字段映射回 OpenAI usage（真实用量，零折算）。
func cursorClaudeUsageToOpenAI(u ClaudeUsage) OpenAIUsage {
	return OpenAIUsage{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens,
		ImageOutputTokens:        u.ImageOutputTokens,
	}
}

// forwardCursorResponsesEntry 是 /v1/responses **真实入口**的 Cursor 分支。
//
// 这是 CR-C 锁定的缺陷修复点：入站是 Responses 形状，必须先按 Responses 协议解码，
// 再由共享转换器转成 Chat Completions（此前直接把 Responses body 当 Chat 解码，
// Messages 为空 → 转换为 no convertible messages → 有效请求 502 且未出站）。
func (s *OpenAIGatewayService) forwardCursorResponsesEntry(
	ctx context.Context, c *gin.Context, account *Account, body []byte,
) (*OpenAIForwardResult, error) {
	if err := requireCursorExecution(s.cursorExecution); err != nil {
		return nil, err
	}
	var req apicompat.ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("cursor: parse responses request: %w", err)
	}
	res, err := cursorExecuteResponses(ctx, c, s.cursorExecution, account, &req)
	if err != nil {
		return nil, err
	}
	return cursorForwardResultToOpenAI(res), nil
}

// forwardCursorChatCompletionsEntry 是 /v1/chat/completions 真实入口的 Cursor 分支。
func (s *OpenAIGatewayService) forwardCursorChatCompletionsEntry(
	ctx context.Context, c *gin.Context, account *Account, body []byte,
) (*OpenAIForwardResult, error) {
	res, err := cursorExecuteChat(ctx, c, s.cursorExecution, account, body)
	if err != nil {
		return nil, err
	}
	return cursorForwardResultToOpenAI(res), nil
}

// ---------------------------------------------------------------------------
// 工具选择闸门（§24.2-7）
// ---------------------------------------------------------------------------

// cursorValidateToolChoice 在 Cursor 协议适配边界校验原始工具选择。
//
// 规则：
//   - 未声明工具时只接受空 / "auto" / "none"；具名选择指向未声明工具 → 入参错误；
//   - 具名（对象形式）选择必须命中本次声明的工具名，且该工具确有声明；
//   - "required" 只有在确有声明工具时才被接受。
//
// 有效 auto / none / required / 具名 的选择约束由真实输出处校验（见批次封口），
// 本函数只负责"不扩大工具集、不静默忽略"这一层。
func cursorValidateToolChoice(req *apicompat.ChatCompletionsRequest) error {
	if req == nil {
		return nil
	}
	declared := make(map[string]struct{}, len(req.Tools))
	for _, tool := range req.Tools {
		name := strings.TrimSpace(tool.Function.Name)
		if name == "" {
			continue
		}
		declared[name] = struct{}{}
	}
	raw := bytes.TrimSpace(req.ToolChoice)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	// 字符串形式：auto / none / required（大小写不敏感，与 OpenAI 契约一致）。
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		switch strings.ToLower(strings.TrimSpace(asString)) {
		case "", "auto", "none":
			return nil
		case "required":
			if len(declared) == 0 {
				return fmt.Errorf("%w: tool_choice=required but no tools declared", ErrCursorToolChoiceInvalid)
			}
			return nil
		default:
			return fmt.Errorf("%w: unknown tool_choice %q", ErrCursorToolChoiceInvalid, asString)
		}
	}
	// 对象形式：{"type":"function","function":{"name":"x"}} 或 {"type":"tool","name":"x"}。
	var named struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &named); err != nil {
		return fmt.Errorf("%w: undecodable tool_choice: %v", ErrCursorToolChoiceInvalid, err)
	}
	name := strings.TrimSpace(named.Function.Name)
	if name == "" {
		name = strings.TrimSpace(named.Name)
	}
	if name == "" {
		return fmt.Errorf("%w: tool_choice carries no tool name", ErrCursorToolChoiceInvalid)
	}
	if _, ok := declared[name]; !ok {
		return fmt.Errorf("%w: tool_choice names undeclared tool %q", ErrCursorToolChoiceInvalid, name)
	}
	return nil
}

// cursorValidateResponsesToolChoice 在**原始 Responses 请求**上校验工具选择（§24.2-7）。
//
// 必须在共享转换器（Responses→Chat）之前校验：该转换器不携带 tool_choice 语义，
// 过了它就无法再区分"具名但未声明"与"未选择"。
func cursorValidateResponsesToolChoice(req *apicompat.ResponsesRequest) error {
	if req == nil {
		return nil
	}
	raw := bytes.TrimSpace(req.ToolChoice)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	tools, err := apicompat.EffectiveResponsesTools(req)
	if err != nil {
		return fmt.Errorf("%w: resolve tools: %v", ErrCursorToolChoiceInvalid, err)
	}
	declared := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if name := strings.TrimSpace(tool.Name); name != "" {
			declared[name] = struct{}{}
			for _, child := range append(append([]apicompat.ResponsesTool{}, tool.Tools...), tool.Children...) {
				childName := strings.TrimSpace(child.Name)
				if childName == "" {
					continue
				}
				declared[name+"."+childName] = struct{}{}
			}
		}
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		switch strings.ToLower(strings.TrimSpace(asString)) {
		case "", "auto", "none":
			return nil
		case "required":
			if len(declared) == 0 {
				return fmt.Errorf("%w: tool_choice=required but no tools declared", ErrCursorToolChoiceInvalid)
			}
			return nil
		default:
			return fmt.Errorf("%w: unknown tool_choice %q", ErrCursorToolChoiceInvalid, asString)
		}
	}
	var named struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &named); err != nil {
		return fmt.Errorf("%w: undecodable tool_choice: %v", ErrCursorToolChoiceInvalid, err)
	}
	name := strings.TrimSpace(named.Function.Name)
	if name == "" {
		name = strings.TrimSpace(named.Name)
	}
	switch strings.ToLower(strings.TrimSpace(named.Type)) {
	case "", "auto", "none":
		return nil
	}
	if name == "" {
		return fmt.Errorf("%w: tool_choice carries no tool name", ErrCursorToolChoiceInvalid)
	}
	if _, ok := declared[name]; !ok {
		return fmt.Errorf("%w: tool_choice names undeclared tool %q", ErrCursorToolChoiceInvalid, name)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 探测（AccountTestService 共用同一执行部件，§24.2-9）
// ---------------------------------------------------------------------------

// ProbeAgentRun 用本执行部件的同一 transport / 鉴权 / 转换跑一次最小 AgentRun，
// 供账号探测复用。调用方负责模型闸门与展示格式；本函数只做真实出站与解析。
//
// 返回：上游状态码、上游错误摘要（已脱敏）、解析出的真实文本。
func (s *CursorExecutionService) ProbeAgentRun(
	ctx context.Context,
	account *Account,
	upstreamModel string,
) (status int, errDetail, text string, err error) {
	if s == nil {
		return 0, "", "", ErrCursorExecutionUnavailable
	}
	if !isCursorAccount(account) {
		return 0, "", "", errors.New("cursor: probe requires a cursor platform account")
	}
	if strings.TrimSpace(upstreamModel) == "" {
		return 0, "", "", fmt.Errorf("%w: empty upstream model", ErrCursorUnsupportedRequest)
	}
	bearer, err := cursorResolveAccountBearer(account)
	if err != nil {
		return 0, "", "", err
	}
	authProvider, err := newCursorAccountAuthProvider(account, s.quotaKeys, s.enc, s.deviceIDs)
	if err != nil {
		return 0, "", "", err
	}
	maxTokens := 16
	chatReq := &apicompat.ChatCompletionsRequest{
		Model:     upstreamModel,
		MaxTokens: &maxTokens,
		Messages: []apicompat.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"hi"`)},
		},
	}
	payload, err := cursorConvertRequest(chatReq, upstreamModel)
	if err != nil {
		return 0, "", "", err
	}
	resp, err := s.forwarder.Do(ctx, &CursorUpstreamRequest{
		Path:         cursorPathAgentRun,
		Body:         payload,
		AuthProvider: authProvider,
		Model:        upstreamModel,
		Headers:      cursorAgentRunHeaders(bearer, authProvider.device, s.now()),
	})
	if err != nil {
		return 0, "", "", err
	}
	defer func() { _ = resp.Body.Close() }()

	rawBody, rerr := readCursorUpstreamBody(resp.Body, cursorUpstreamStreamMaxBytes)
	if rerr != nil {
		return resp.StatusCode, "", "", rerr
	}
	// 出站/响应均不回显 bearer。
	if resp.StatusCode != http.StatusOK {
		code, msg := cursorExtractUpstreamErrorSummary(rawBody, bearer)
		detail := fmt.Sprintf("cursor upstream status %d", resp.StatusCode)
		if code != "" || msg != "" {
			detail += fmt.Sprintf(" (%s %s)", code, msg)
		}
		return resp.StatusCode, detail, "", nil
	}
	if rej, rejected := cursorDetectKeyRejectedUpstream(rawBody); rejected {
		return resp.StatusCode, fmt.Sprintf("cursor key rejected by upstream (turn %s)", rej.TurnID), "", nil
	}
	parser := cursorNewStreamParser(upstreamModel, bearer)
	if _, perr := parser.cursorParseStream(rawBody); perr != nil {
		return resp.StatusCode, "", "", perr
	}
	return resp.StatusCode, "", parser.content, nil
}

// ---------------------------------------------------------------------------
// 响应回桥 writer（原 gateway_forward.go 的 Cursor 专用 writer，迁至 Cursor 边界）
// ---------------------------------------------------------------------------

// cursorProtocolWriter 把 Chat Completions 形状响应回桥为客户端协议。
//
// 只拦截 gin.ResponseWriter 的写入方法；其余方法透传。Header 使用独立 map，
// 避免桥接内部的 Content-Type / SSE 头直接落到真实连接。
type cursorProtocolWriter struct {
	gin.ResponseWriter
	header http.Header
	mode   cursorProtocol
	model  string
	stream bool
	tools  cursorResponsesToolMeta

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

// newCursorProtocolWriter 构造回桥 writer，并把工具元数据交给 Responses 状态机。
//
// §24.2-5：流式 state 与非流式转换**必须**带着 EffectiveResponsesTools 派生的
// CustomToolNames / FunctionToolNames / NamespaceToolNames / HasToolSearchTool，
// 不得再传 nil/nil/false/nil（那会丢 Codex 的 custom / namespace 工具类型）。
func newCursorProtocolWriter(
	parent gin.ResponseWriter,
	stream bool,
	mode cursorProtocol,
	model string,
	tools cursorResponsesToolMeta,
) *cursorProtocolWriter {
	w := &cursorProtocolWriter{
		ResponseWriter: parent,
		header:         make(http.Header),
		mode:           mode,
		model:          model,
		stream:         stream,
		tools:          tools,
	}
	switch mode {
	case cursorProtocolAnthropic:
		w.anthState = apicompat.NewChatCompletionsToAnthropicStreamState(model)
	case cursorProtocolResponses:
		w.respState = cursorResponsesStreamState(model, tools)
	}
	return w
}

// cursorResponsesStreamState 构造带工具映射的 Responses 流式状态机。
func cursorResponsesStreamState(model string, tools cursorResponsesToolMeta) *apicompat.ChatCompletionsToResponsesStreamState {
	state := apicompat.NewChatCompletionsToResponsesStreamState(model)
	state.CustomTools = tools.customTools
	state.FunctionTools = tools.functionTools
	state.NamespaceTools = tools.namespaceTools
	state.ToolSearchDeclared = tools.toolSearch
	return state
}

func (w *cursorProtocolWriter) Header() http.Header { return w.header }

func (w *cursorProtocolWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.status = code
	w.wroteHeader = true
}

func (w *cursorProtocolWriter) WriteHeaderNow() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
}

func (w *cursorProtocolWriter) Write(p []byte) (int, error) {
	w.WriteHeaderNow()
	w.written += len(p)
	if !w.stream {
		return w.completion.Write(p)
	}
	w.pending = append(w.pending, p...)
	return len(p), w.flushFrames()
}

func (w *cursorProtocolWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

func (w *cursorProtocolWriter) Flush() {
	if w.stream {
		_ = w.flushFrames()
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *cursorProtocolWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (w *cursorProtocolWriter) Size() int { return w.written }

func (w *cursorProtocolWriter) Written() bool { return w.wroteHeader || w.written > 0 }

// ensureStarted 在首次写客户端前设置目标协议的 SSE 头。
func (w *cursorProtocolWriter) ensureStarted() {
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
func (w *cursorProtocolWriter) flushFrames() error {
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
func (w *cursorProtocolWriter) writeSSEFrame(frame []byte) error {
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

func (w *cursorProtocolWriter) writeChunk(chunk *apicompat.ChatCompletionsChunk) error {
	w.ensureStarted()
	switch w.mode {
	case cursorProtocolAnthropic:
		for _, event := range apicompat.ChatCompletionsChunkToAnthropicEvents(chunk, w.anthState) {
			sse, err := apicompat.ResponsesAnthropicEventToSSE(event)
			if err != nil {
				return err
			}
			if _, err := io.WriteString(w.ResponseWriter, sse); err != nil {
				return err
			}
		}
	case cursorProtocolResponses:
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
func (w *cursorProtocolWriter) finalize() error {
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
	case cursorProtocolAnthropic:
		for _, event := range apicompat.FinalizeChatCompletionsAnthropicStream(w.anthState) {
			sse, err := apicompat.ResponsesAnthropicEventToSSE(event)
			if err != nil {
				return err
			}
			events = append(events, sse)
		}
	case cursorProtocolResponses:
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

func (w *cursorProtocolWriter) finalizeCompletion() error {
	body := w.completion.Bytes()
	var resp apicompat.ChatCompletionsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("cursor: decode bridged chat completion: %w", err)
	}
	resp.Usage = cursorBridgedChatUsage(body)

	var out any
	switch w.mode {
	case cursorProtocolAnthropic:
		out = apicompat.ChatCompletionsResponseToAnthropic(&resp, w.model)
	case cursorProtocolResponses:
		// 工具映射必须传齐（§24.2-5），否则 custom/namespace 工具类型丢失。
		out = apicompat.ChatCompletionsResponseToResponses(
			&resp, w.model,
			w.tools.customTools, w.tools.functionTools, w.tools.toolSearch, w.tools.namespaceTools,
		)
	default:
		return fmt.Errorf("cursor: unsupported non-stream response protocol %d", w.mode)
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

// ---------------------------------------------------------------------------
// 会话表 + 统一关联索引（§25.2：一个状态所有者）
// ---------------------------------------------------------------------------

// cursorSessionState 是会话状态机取值（running → waiting_tools → running → finalizing → closed）。
type cursorSessionState int

const (
	cursorSessionRunning cursorSessionState = iota
	cursorSessionWaitingTools
	cursorSessionFinalizing
	cursorSessionClosed
)

// cursorSessionSpec 是会话的不可变身份字段（不含任何请求正文）。
type cursorSessionSpec struct {
	protocol       cursorProtocol
	requestedModel string
	upstreamModel  string
	accountID      int64
	quotaKeyID     int64
	// indexProtocol 是统一索引的协议命名空间（§25.14-D「一份索引」）。
	//
	// 与 protocol 分开：protocol 决定**客户端可见响应形状**（Gemini 原生出口复用 Chat
	// 形状写出），indexProtocol 决定**续轮关联的键命名空间**（Gemini 原生出口是 "gemini"）。
	// 为空时按 protocol 派生，绝不跨协议猜归属。
	indexProtocol string
	// identity 是会话唯一身份（user / API key / 认证组）；续轮从会话唯一记录读取，
	// 不复制到索引（§25.14-D「最小表示」）。
	identity CursorSessionIdentity
}

// cursorSessionRecord 是一条进程内会话记录。
//
// 只保存完成本次生命周期所需的类型化字段：绝不保存原始请求正文、完整历史、
// gin.Context、ResponseWriter 或未解包的释放闭包。

// cursorOutputEvent 是背景读循环扇出给附着 HTTP 的一条事件。
//
// data 是客户端协议字节（SSE chunk）；isDeleg / isEnd 是控制事件（不携带正文）。
// §25.3：每个 HTTP 仅消费自其自身附着以来的事件；无附着时正文被丢弃（仅元数据）。
type cursorOutputEvent struct {
	data    []byte
	isDeleg bool
	isEnd   bool
	err     error
}

// cursorSessionOutput 是每会话上游帧扇出：背景读循环把"模型输出事件"推入，当前附着的
// HTTP 消费者按"自本次附着以来"的序列拉取；无附着时只保留元数据（不存正文，§25.3）。
//
// 同一时刻至多一个消费者（§25.3「当前附着的 HTTP」）。unattached 窗口的事件正文被丢弃，
// 符合"每个 HTTP 仅输出自其自身附着以来新增内容"。
type cursorSessionOutput struct {
	mu       sync.Mutex
	ch       chan *cursorOutputEvent
	closeCh  chan struct{}
	attached bool
	ended    bool
}

// newCursorSessionOutput 构造一条空的扇出器（尚未有 HTTP 附着）。
func newCursorSessionOutput() *cursorSessionOutput {
	return &cursorSessionOutput{}
}

// attach 注册当前附着 HTTP 消费者，返回其事件通道与幂等 detach。
//
// 若会话已结束或已有附着，返回 (nil, nil) 表示不可附着（调用方据此失败关闭）。
func (o *cursorSessionOutput) attach() (chan *cursorOutputEvent, func()) {
	o.mu.Lock()
	if o.attached || o.ended {
		o.mu.Unlock()
		return nil, nil
	}
	ch := make(chan *cursorOutputEvent, 256)
	closeCh := make(chan struct{})
	o.ch = ch
	o.closeCh = closeCh
	o.attached = true
	o.mu.Unlock()
	detach := func() {
		o.mu.Lock()
		if !o.attached {
			o.mu.Unlock()
			return
		}
		o.attached = false
		ch := o.ch
		closeCh := o.closeCh
		o.ch = nil
		o.closeCh = nil
		o.mu.Unlock()
		close(closeCh)
		// 排空剩余事件，避免发布方在已脱离的通道上阻塞。
		go func() {
			for range ch {
			}
		}()
	}
	return ch, detach
}

// publish 把一条输出事件推给当前附着的 HTTP（无附着时丢弃正文，仅元数据）。
func (o *cursorSessionOutput) publish(ev *cursorOutputEvent) {
	o.mu.Lock()
	ch := o.ch
	closeCh := o.closeCh
	attached := o.attached
	o.mu.Unlock()
	if !attached || ch == nil {
		return
	}
	select {
	case ch <- ev:
	case <-closeCh:
	}
}

// end 标记读循环结束：若有附着则投递终态事件，否则仅置位（后续附着者直接收尾）。
func (o *cursorSessionOutput) end(err error) {
	o.mu.Lock()
	if o.ended {
		o.mu.Unlock()
		return
	}
	o.ended = true
	ch := o.ch
	closeCh := o.closeCh
	attached := o.attached
	o.mu.Unlock()
	if attached && ch != nil {
		select {
		case ch <- &cursorOutputEvent{isEnd: true, err: err}:
		case <-closeCh:
		}
	}
}

type cursorSessionRecord struct {
	id   string
	spec cursorSessionSpec
	// identity 是会话唯一身份（与 spec.identity 同源；续轮比对用，不复制到索引）。
	identity CursorSessionIdentity

	mu          sync.Mutex
	state       cursorSessionState
	delegations *cursorToolDelegations
	// toolCallIDs 是已登记的对外工具调用 ID（供续轮关联与重复提交拒绝）。
	toolCallIDs map[string]struct{}

	// pumpEnded 标记背景读循环（每会话一个读循环）是否已结束（终态/传输错误/180s）。
	// Execute 返回时据此判定会话是否仍存活跨 HTTP：未结束则不触发 finalize。
	pumpEnded bool

	// ---- §25.4 并发槽 / 预留 / 180 秒 / 终止清理（全部字段在 mu 下读写）----

	// createdAt 是会话建立时刻；billingEventID 是**服务端生成、会话级不可变**的
	// 账单事件 ID（§25.5），建会话时固定，任何 HTTP request_id 都不得替代它。
	createdAt      time.Time
	billingEventID string
	// deadline 是 180 秒总期限的绝对时刻；在首次 AgentRun 发起时固定，续轮**不重置**。
	deadline       time.Time
	deadlineSource string
	agentRunAt     time.Time
	// resources 是本会话持有的可转交资源句柄（用户槽、账号槽、余额预留）；
	// 只有 finalizer 或交接失败回退路径会释放它们，且各自幂等。
	resources []CursorTransferableResource
	// inflight 是转交来的余额预留句柄（§25.5：HandlerDone 延后到结算提交之后）。
	inflight *InflightReservation
	// settlement 是唯一结算回调（handler 以不可变值快照构造，不闭包 gin.Context）。
	settlement CursorSessionSettlement
	// usage 是 finalizer 接纳的真实终态 usage；usageStage 记录最后协议阶段。
	usage      cursorTurnUsage
	usageStage string
	// finalResult 是最终一次真实转发结果（未取得完整 usage 时也保留中断信息）。
	finalResult *ForwardResult
	// outcome / interruptReason 是终态记载（日志安全字段，不含正文/凭据）。
	outcome         string
	interruptReason string
	// finishing 标记 finalizer 的唯一胜者已确定；steps 是终结守卫（每项至多一次）。
	finishing bool
	steps     cursorFinalizerSteps
	// streamClose 关闭原生流/取消读写（finalizer 调用至多一次）。
	streamClose func()
	// httpAttachStop 停止当前 HTTP 附着期取消回调；attached 表示是否有 HTTP 附着。
	httpAttachStop func() bool
	attached       bool
	// owner 是会话表的唯一所有者（finalizer/结算入口），只在取消回调里使用。
	owner *CursorExecutionService

	// ---- §25.14-D 摘要校验与续轮接管（全部字段在 mu 下读写）----

	// configDigest 是固定生成配置的摘要（模型/system/工具/schema/推理配置等）；
	// historyDigest 是**已交付**输入历史的链式摘要，logicalItems 即其逻辑项数。
	// 两者都按 CR-B 引擎生成：本层只提供 canonical 项，不改引擎语义。
	configDigest  *CursorHistoryDigest
	historyDigest *CursorHistoryDigest
	// exposedTools 是本会话已暴露给客户端的工具名（保持声明顺序），
	// 供 tool_choice 逐 HTTP 校验"暴露集合是否一致"。
	exposedTools []string
	// instructionsDeclared 表示首轮是否声明了非空 instructions/instructions 等价物。
	// §25.14-D：首轮非空 + 续轮省略 = 明确入参错误（不偷偷继承）。
	instructionsDeclared bool
	// pending 是**当前交付批次**（等待客户端真实结果）；同一时刻至多一个。
	pending *cursorPendingBatch
	// intake 是真实结果回写通道（由原生双工持有方安装）；nil 表示通道未接通，
	// 续轮必须失败关闭且零上游写入（绝不伪造成功）。
	intake CursorSessionIntake
	// agentRunDuplex 是原生 AgentRun 双向连接（读循环 + 回写 pipe）的持有方；
	// 跨 HTTP 真实结果回写通道（CursorSessionIntake 生产实现）经由它把真实结果写回原
	// exec 关联（§24.3 / W2-F）。
	agentRunDuplex *cursorAgentDuplex
	// output 是每会话上游帧扇出：背景读循环把"模型输出事件"推入，当前附着的 HTTP 消费者
	// 按"自本次附着以来"的序列号拉取；无附着时只保留元数据（不存正文）。
	output *cursorSessionOutput
	// pumpDone 在背景读循环结束时关闭（终态/传输错误/180s 到期），用于通知附着 HTTP 收尾。
	pumpDone chan struct{}
	// resumeCount 是本会话已接纳的续轮次数（日志/诊断安全字段）。
	resumeCount int
}

// cursorPendingBatch 是当前已交付、等待真实结果的工具批次（§25.3 / §25.14-D）。
type cursorPendingBatch struct {
	// batchID 是产生该批的 model_call_id（无批次时用 cursorSessionLevelBatchID）。
	batchID string
	// responseID 是 Responses 协议对外交付的响应 ID（previous_response_id 的目标）；
	// 其它协议为空。
	responseID string
	// callIDs 是交付顺序的对外工具调用 ID（唯一、不可猜、对应原生 exec id）。
	callIDs []string
	// delivered 表示该批内容已完整写出（部分写出按 §25.4 终止，不记为已交付）。
	delivered bool
	// consumed 表示已有唯一胜者接管该批结果（重复提交返回 409）。
	consumed bool
}

// cloneCallIDs 返回批次调用 ID 的拷贝（只读快照，防止调用方改写会话状态）。
func (b *cursorPendingBatch) cloneCallIDs() []string {
	if b == nil {
		return nil
	}
	out := make([]string, len(b.callIDs))
	copy(out, b.callIDs)
	return out
}

// attachDelegations 绑定本次会话的工具委托记录。
func (r *cursorSessionRecord) attachDelegations(d *cursorToolDelegations) {
	if r == nil || d == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.delegations = d
}

// cursorSessionStore 是进程内会话表 + 统一关联索引的唯一所有者。
//
// 索引的**唯一实现**是 CR-B 的 CursorSessionIndex（cursor_session_index.go）：
// 键 = (客户端协议, ID 种类, ID)，值 = session / batch / 消费标记。本层只做薄适配，
// 绝不另建第二份索引表、不按协议或 ID 种类分建 registry（方案 §25.14-D「一份索引」）。
type cursorSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*cursorSessionRecord
	// index 是跨会话关联的唯一索引（CR-B 实现，语义与 API 归 CR-B）。
	index   *CursorSessionIndex
	stopped bool
}

func newCursorSessionStore() *cursorSessionStore {
	return &cursorSessionStore{
		sessions: make(map[string]*cursorSessionRecord),
		index:    NewCursorSessionIndex(),
	}
}

// start 登记一条会话并返回记录。
//
// 建会话时即固定**会话级不可变账单事件 ID**（§25.5）：它与任何 HTTP request_id
// 无关，客户端复用 request ID 也不能把两次原生会话合并成一笔账。
func (s *cursorSessionStore) start(spec cursorSessionSpec) *cursorSessionRecord {
	now := time.Now()
	record := &cursorSessionRecord{
		id:             cursorNewUUID(),
		spec:           spec,
		identity:       spec.identity,
		state:          cursorSessionRunning,
		toolCallIDs:    make(map[string]struct{}),
		createdAt:      now,
		billingEventID: "",
	}
	record.billingEventID = CursorSessionBillingEventID(record.id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		// Stop 之后不再接受新会话：返回一条游离记录（不会被索引），调用方仍可完成
		// 本次请求，但不会留下可续轮的会话状态。
		return record
	}
	s.sessions[record.id] = record
	return record
}

// attachOwner 绑定会话表的唯一所有者（finalizer/结算入口；取消回调使用）。
func (s *cursorSessionStore) attachOwner(record *cursorSessionRecord, owner *CursorExecutionService) {
	if record == nil {
		return
	}
	record.mu.Lock()
	record.owner = owner
	record.mu.Unlock()
}

// close 收口会话并删除其全部关联（finalizer 口径：不只删当前 pending）。
func (s *cursorSessionStore) close(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
	// DeleteSession 是 CR-B 的整批清理入口：删除该 session 的全部 call/response/item
	// 关联（含历史批次），不是只删当前 pending。
	s.index.DeleteSession(id)
}

// stop 关闭全部会话（DI cleanup / 进程 Stop）。
//
// 服务 Stop 是 §25.4 的终止原因之一：每个存活会话都走**同一个 finalizer**（不另写清理链），
// 因为终结守卫与结算守卫保证每项最多一次。不持会话表锁执行终结（终结内部会重新入表）。
func (s *cursorSessionStore) stop() {
	s.mu.Lock()
	s.stopped = true
	records := make([]*cursorSessionRecord, 0, len(s.sessions))
	for _, record := range s.sessions {
		records = append(records, record)
	}
	s.mu.Unlock()
	for _, record := range records {
		if record.owner != nil {
			record.owner.finishCursorSession(record, "service_stop")
		}
	}
	// 兜底：清掉没有 owner 的残留（正常路径已由 finalizer 逐条 close）。
	s.mu.Lock()
	defer s.mu.Unlock()
	// 索引项的归属 session 必然在会话表内（registerToolCall 只在会话存续时登记，
	// Stop 之后不落表也不落索引），故逐个 DeleteSession 等价于全清。
	for id := range s.sessions {
		s.index.DeleteSession(id)
		delete(s.sessions, id)
	}
}

// registerToolCall 把对外工具调用 ID 登记进统一索引（键 = (协议, call, ID)）。
//
// batchID 是产生该调用的批次（model_call_id）；无批次的会话级登记用
// cursorSessionLevelBatchID 明确标注，不伪造 model_call_id。
func (s *cursorSessionStore) registerToolCall(sessionID, batchID, callID string) error {
	return s.registerSessionID(sessionID, batchID, CursorIDKindCall, callID)
}

// registerSessionID 把任意种类的对外 ID（call / response / item）登记进统一索引。
//
// §25.14-D：call/response/item 都在**同一索引**，不按协议或种类另建 registry。
func (s *cursorSessionStore) registerSessionID(sessionID, batchID string, kind CursorIDKind, id string) error {
	if s == nil {
		return nil
	}
	if sessionID == "" || id == "" {
		return fmt.Errorf("%w: register needs session id and id", ErrCursorSessionIndexInvalidKey)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.sessions[sessionID]
	if !ok {
		// 会话已收口或已 Stop：不留下游离索引条目，明确拒绝登记。
		return fmt.Errorf("%w: session %q is not active", ErrCursorSessionIndexInvalidKey, sessionID)
	}
	if batchID == "" {
		batchID = cursorSessionLevelBatchID
	}
	return s.index.Register(cursorSessionIndexKeyForSpec(record.spec, kind, id), sessionID, batchID)
}

// lookupSession 按 (协议, ID 种类, ID) 取会话（续轮关联用，协议为代码内枚举）。
//
// 协议是键的一部分：不同入口的同名 ID 互不串号，本层不跨协议猜测归属。
func (s *cursorSessionStore) lookupSession(protocol cursorProtocol, kind CursorIDKind, id string) *cursorSessionRecord {
	return s.lookupSessionByProtocolName(cursorProtocolIndexName(protocol), kind, id)
}

// lookupSessionByProtocolName 是按**索引命名空间**取会话的唯一实现。
func (s *cursorSessionStore) lookupSessionByProtocolName(protocol string, kind CursorIDKind, id string) *cursorSessionRecord {
	if s == nil || id == "" || protocol == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, err := s.index.Locate(CursorSessionIndexKey{Protocol: protocol, Kind: kind, ID: id})
	if err != nil {
		return nil
	}
	return s.sessions[entry.SessionID]
}

// sessionCount 返回当前会话数（测试与诊断用）。
func (s *cursorSessionStore) sessionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// indexCount 返回当前索引条目数（测试与诊断用；数值取自 CR-B 索引自身）。
func (s *cursorSessionStore) indexCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.index.Len()
}

// cursorSessionIndexKey 是执行服务 → CR-B 统一索引的**唯一**薄适配：
// 把本服务的 (客户端协议, ID 种类, ID) 映射为 CursorSessionIndexKey 三元组。
//
// 只做映射，不改 CR-B 的键语义与 API（方案 §25.14-D「一份索引」）。
func cursorSessionIndexKey(protocol cursorProtocol, kind CursorIDKind, id string) CursorSessionIndexKey {
	return CursorSessionIndexKey{Protocol: cursorProtocolIndexName(protocol), Kind: kind, ID: id}
}

// cursorSessionIndexKeyForSpec 按会话不可变 spec 的**索引命名空间**构造键。
//
// spec.indexProtocol 优先（Gemini 原生出口），否则按客户端可见协议派生。
func cursorSessionIndexKeyForSpec(spec cursorSessionSpec, kind CursorIDKind, id string) CursorSessionIndexKey {
	return CursorSessionIndexKey{Protocol: cursorSessionIndexProtocol(spec), Kind: kind, ID: id}
}

// cursorSessionIndexProtocol 返回会话的索引命名空间。
func cursorSessionIndexProtocol(spec cursorSessionSpec) string {
	if spec.indexProtocol != "" {
		return spec.indexProtocol
	}
	return cursorProtocolIndexName(spec.protocol)
}

// cursorProtocolIndexName 把入站协议枚举映射为索引用协议名（与现有入口同源）。
func cursorProtocolIndexName(p cursorProtocol) string {
	switch p {
	case cursorProtocolChat:
		return "chat"
	case cursorProtocolAnthropic:
		return "messages"
	case cursorProtocolResponses:
		return "responses"
	default:
		// 未知枚举返回空串：交给 CR-B 的键校验失败关闭，不猜协议、不静默归类。
		return ""
	}
}

// cursorSessionLevelBatchID 是**会话级**（无 model_call_id）登记使用的批次标记。
//
// 它明确表示"该关联不来自某个具体工具批次"，避免把会话级登记伪造成 model_call_id。
const cursorSessionLevelBatchID = "session"

// ---------------------------------------------------------------------------
// 工具批次（§25.3：按 model_call_id + listed call_count 封口）
// ---------------------------------------------------------------------------

// 批次封口的协议错误（明确失败，不静默补齐）。
var (
	// ErrCursorBatchIncomplete 表示当前批次未集齐：started 数不足 listed 数，
	// 或某个对外调用没有完整的 f11 委托与已解码参数。
	ErrCursorBatchIncomplete = errors.New("cursor: tool batch incomplete (fail closed)")
	// ErrCursorBatchAmbiguous 表示多个 model_call_id 交错且无法把无 ID 的 listed
	// 归属到唯一开放批次：不按最后一条猜测。
	ErrCursorBatchAmbiguous = errors.New("cursor: tool batch attribution ambiguous (fail closed)")
)

// cursorToolBatch 是一个工具批次：同一 model_call_id 的委托集合。
type cursorToolBatch struct {
	modelCallID string
	// listed 是 tool_requests_listed.call_count（ToolRequestsListedUpdate 内层 f1，uint32）；
	// listedSeen=false 表示本批未声明总数。
	listed     int
	listedSeen bool
	// started 是本批已 started 的**对外** call_id（顺序即到达顺序）。
	started []string
	// internalStarted 是本批已 started 的**内部**元工具数（get_mcp_tools 等）。
	internalStarted int
	// delegations 是本批已解码的 f11 委托。
	delegations []cursorToolCallCapture
	sealed      bool
}

// cursorToolDelegations 是一次 AgentRun 内工具委托的**唯一**记录。
//
// §24.2-6：duplex 解码 mcp_args 一次并写入这里，parser 只读取，不再二次解码。
// §25.3：批次按 tool_call_started.model_call_id + tool_requests_listed.call_count 封口；
// 已封口的批次保留在 all 中（供本轮输出），同 model_call_id 的新信号开新批，
// 绝不把上一批的 count 跨代复用（证据 29 陷阱）。
type cursorToolDelegations struct {
	mu   sync.Mutex
	all  []*cursorToolBatch
	open map[string]*cursorToolBatch
	// shared 表示本记录由 duplex 拥有解码权（parser 不再解码同一帧）。
	shared bool
}

// newCursorToolDelegations 构造由 duplex 解码、parser 读取的共享记录。
func newCursorToolDelegations() *cursorToolDelegations {
	return &cursorToolDelegations{
		open:   make(map[string]*cursorToolBatch),
		shared: true,
	}
}

// newLocalCursorToolDelegations 构造由 parser 自己解码的本地记录（无 duplex 的场景）。
func newLocalCursorToolDelegations() *cursorToolDelegations {
	return &cursorToolDelegations{open: make(map[string]*cursorToolBatch)}
}

// isShared 报告本记录的解码权是否在 duplex 侧。
func (d *cursorToolDelegations) isShared() bool { return d != nil && d.shared }

// batchFor 取/建当前开放批次；modelCallID 为空时要求开放批次唯一（否则含糊失败）。
func (d *cursorToolDelegations) batchFor(modelCallID string) (*cursorToolBatch, error) {
	if modelCallID != "" {
		if batch, ok := d.open[modelCallID]; ok && !batch.sealed {
			return batch, nil
		}
		batch := &cursorToolBatch{modelCallID: modelCallID}
		d.open[modelCallID] = batch
		d.all = append(d.all, batch)
		return batch, nil
	}
	var candidates []*cursorToolBatch
	for _, batch := range d.all {
		if !batch.sealed {
			candidates = append(candidates, batch)
		}
	}
	switch len(candidates) {
	case 0:
		batch := &cursorToolBatch{}
		d.open[""] = batch
		d.all = append(d.all, batch)
		return batch, nil
	case 1:
		return candidates[0], nil
	default:
		return nil, fmt.Errorf("%w: %d open batches for an unattributed signal", ErrCursorBatchAmbiguous, len(candidates))
	}
}

// noteToolRequestsListed 登记一次 tool_requests_listed（InteractionUpdate.f27）的 call_count。
//
// 字段依据（design-evidence.md / 02-native-schemas.json）：ToolRequestsListedUpdate 内层
// **只有** no:1 name:"call_count"（uint32），**没有** model_call_id——归属只能靠当前
// 开放批次上下文判定，三种情况各有明确处置（§25.3，不按最后一条猜测）：
//   - 恰好一个开放批 → 归属该批，把 count 记为"本批应有多少条"；
//   - 零个开放批 → 记录孤儿 listed（开一个未归属批，后续同一上下文的 started 落进来）；
//   - ≥2 个开放批 → ErrCursorBatchAmbiguous 明确协议错误，绝不按到达顺序猜。
//
// listed 可以先于最后一个 started 到达（证据 29）：本函数只登记"本批应有多少条"，
// 是否集齐由 seal 在流尾按集合判断，绝不按到达顺序或静默时间提前封口。
func (d *cursorToolDelegations) noteToolRequestsListed(modelCallID string, count int) error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if count < 0 {
		return fmt.Errorf("%w: negative tool_requests_listed count", ErrCursorProtocol)
	}
	batch, err := d.batchFor(modelCallID)
	if err != nil {
		return err
	}
	batch.listed = count
	batch.listedSeen = true
	return nil
}

// noteToolCallStarted 登记一次 tool_call_started（InteractionUpdate.f2）。
//
// external=true 表示 ToolCall 的 mcp_tool_call（f15）分支——客户端要执行的委托；
// false 表示内部元工具（如 get_mcp_tools），只参与同批完整性判断，绝不交给客户端。
func (d *cursorToolDelegations) noteToolCallStarted(modelCallID, callID string, external bool) error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	batch, err := d.batchFor(modelCallID)
	if err != nil {
		return err
	}
	if !external {
		batch.internalStarted++
		return nil
	}
	for _, existing := range batch.started {
		if existing == callID {
			// 同一 call_id 重复 started：不重复计数（幂等）。
			return nil
		}
	}
	batch.started = append(batch.started, callID)
	return nil
}

// noteDelegation 登记一条已解码的 f11 委托（**唯一**解码点，由 duplex 调用）。
func (d *cursorToolDelegations) noteDelegation(modelCallID string, tc cursorToolCallCapture) error {
	if d == nil {
		return nil
	}
	if strings.TrimSpace(tc.ID) == "" {
		return fmt.Errorf("%w: mcp_args without tool_call_id", ErrCursorProtocol)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	batch, err := d.batchFor(modelCallID)
	if err != nil {
		return err
	}
	for _, existing := range batch.delegations {
		if existing.ID == tc.ID {
			// 重复 ID：同一委托被重复提交，按冲突拒绝（§25.2）。
			return fmt.Errorf("%w: duplicate tool delegation %q", ErrCursorProtocol, tc.ID)
		}
	}
	batch.delegations = append(batch.delegations, tc)
	return nil
}

// snapshot 返回按到达顺序的全部委托（parser 输出 tool_calls 用）。
func (d *cursorToolDelegations) snapshot() []cursorToolCallCapture {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []cursorToolCallCapture
	for _, batch := range d.all {
		out = append(out, batch.delegations...)
	}
	return out
}

// sealable 是 seal 的**只读**版本：判定全部未封口批次当前是否已可封口（不改状态）。
//
// 用途（§25.3）：读循环在吐工具交接（tool_calls + finish）之前先问"本批是否已齐"，
// 未齐就继续等后续帧——绝不按第一个 mcp_args 提前交接半个批次（同名并行两工具、listed
// 晚到或 started/委托交错都会因此失败关闭或漏交）。
func (d *cursorToolDelegations) sealable() error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.checkSealableLocked()
}

// checkSealableLocked 在持锁状态下逐批判定封口条件（调用方必须已持 d.mu）。
func (d *cursorToolDelegations) checkSealableLocked() error {
	for _, batch := range d.all {
		if batch.sealed {
			continue
		}
		if batch.listedSeen {
			total := len(batch.started) + batch.internalStarted
			if total != batch.listed {
				return fmt.Errorf("%w: batch %q started=%d internal=%d listed=%d",
					ErrCursorBatchIncomplete, batch.modelCallID, len(batch.started), batch.internalStarted, batch.listed)
			}
		}
		delegated := make(map[string]struct{}, len(batch.delegations))
		for _, tc := range batch.delegations {
			delegated[tc.ID] = struct{}{}
		}
		for _, callID := range batch.started {
			if _, ok := delegated[callID]; !ok {
				return fmt.Errorf("%w: batch %q call %q has no complete mcp_args delegation",
					ErrCursorBatchIncomplete, batch.modelCallID, callID)
			}
		}
	}
	return nil
}

// seal 封口当前开放批次并返回本批全部对外委托。
//
// 封口条件（§25.3）：
//   - 若本批声明了 listed 总数，则外部 started 数 + 内部 started 数必须等齐；
//   - 每个对外 started 调用都必须有完整的 f11 委托与已解码参数；
//   - listed 可以先于最后一个 started，按集合判断，不按到达顺序。
//
// 不按首个 mcp_args 封口，也不按静默时间猜测结束。
func (d *cursorToolDelegations) seal() error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.checkSealableLocked(); err != nil {
		return err
	}
	for _, batch := range d.all {
		batch.sealed = true
	}
	return nil
}

// cursorSealSessionBatch 是 Execute 侧的封口调用点（失败即明确协议错误）。
func cursorSealSessionBatch(delegations *cursorToolDelegations) error {
	if delegations == nil {
		return nil
	}
	return delegations.seal()
}

// ---------------------------------------------------------------------------
// §25.4 / §25.5 会话生命周期：可转交句柄、180 秒总期限、唯一 finalizer、唯一结算
// ---------------------------------------------------------------------------

// cursorAgentRunTotalBudget 是上游 **180 秒总期限**（§25.4）。
//
// 它是既有出站契约 cursorUpstreamDefaultTimeout 的**具体作用点**：从首次 AgentRun
// 发起计算，跨 HTTP 与工具执行共用同一份预算，续轮不重置。本层不另设 120 秒、
// 工具等待窗口或无限保活；尚未发起 AgentRun 的鉴权/排队时限不在此改写。
const cursorAgentRunTotalBudget = cursorUpstreamDefaultTimeout

// cursorDeadlineSourceFirstAgentRun 是 deadline 来源标记（日志安全字段）。
const cursorDeadlineSourceFirstAgentRun = "first_agent_run"

// 会话生命周期错误。全部失败关闭：不猜归属、不交失效句柄、不重复结算。
var (
	// ErrCursorSessionResourceReleased 表示转交时发现句柄已取消/已释放：交接失败，
	// 不能把失效槽位交给 session（§25.4）。
	ErrCursorSessionResourceReleased = errors.New("cursor: session resource already released (handover failed)")
	// ErrCursorSessionResourceHanded 表示同一句柄被重复转交。
	ErrCursorSessionResourceHanded = errors.New("cursor: session resource already handed over")
	// ErrCursorSessionAttachConflict 表示同一会话已有 HTTP 附着：禁止并发写同一 AgentRun。
	ErrCursorSessionAttachConflict = errors.New("cursor: session already has an attached HTTP writer (409)")
	// ErrCursorSessionStopped 表示会话表已 Stop：孤儿续轮明确失效，不换号、不重执行工具。
	ErrCursorSessionStopped = errors.New("cursor: session store stopped (orphan continuation invalid)")
)

// CursorTransferableResource 是可把释放所有权转交给会话的资源句柄（§25.4）。
//
// 实现方必须保证：
//   - TransferTo 在**同一锁内**停掉原 HTTP 取消回调并检查是否已释放；已取消/已释放
//     返回错误（交接失败），绝不把失效句柄交给会话；
//   - TransferTo 成功后句柄绑定 sessionCtx：会话终结即释放；
//   - Release 幂等，整个生命周期最多真正释放一次。
//
// 服务层只消费该接口，不复制并发计数、不加第二套租约。
type CursorTransferableResource interface {
	// Name 是资源名（日志安全标识，如 user_slot / account_slot / inflight）。
	Name() string
	// TransferTo 把释放所有权转交给会话上下文。
	TransferTo(sessionCtx context.Context) error
	// Release 幂等释放（交接失败回退或会话终结时由唯一归属者调用）。
	Release()
}

// CursorSettlementEvent 是会话唯一结算的输入（§25.5）。
type CursorSettlementEvent struct {
	// SessionID 是会话标识（不是任何 HTTP request_id）。
	SessionID string
	// BillingEventID 是服务端生成、会话级不可变的账单事件 ID。
	BillingEventID string
	// Result 是最终一次真实转发结果（含真实 usage）。
	Result *ForwardResult
	// UsageComplete 表示已取得完整的真实 usage；false 时不得结算、不得写零 usage。
	UsageComplete bool
	// InterruptReason / LastProtocolStage 是未结算时的中断原因与最后协议阶段。
	InterruptReason   string
	LastProtocolStage string
	// UsageMissingReason 是"未取得完整真实 usage"的具体原因（字段缺失 / 未收到 turn_ended）。
	UsageMissingReason string
	// InflightRelease 在**结算任务提交之后**归还在途预留引用（至多一次）。
	//
	// 只有真正提交结算的一侧调用它；会话未结算（未取得完整 usage）时由 finalizer 直接调用，
	// 保证"不得在缓存扣减前先释放预留"，同时不留下无人归还的预留。
	InflightRelease func()
}

// CursorSessionSettlement 是唯一结算回调。
//
// handler 以**不可变值快照**构造（API key/user/account/subscription、模型、分组/渠道、
// QuotaPlatform、定价时刻、审计字段），绝不闭包 gin.Context；会话终结时至多调用一次。
type CursorSessionSettlement func(ctx context.Context, ev CursorSettlementEvent)

// CursorSessionHandover 是 handler 在选中 Cursor 账号后登记的转交物（§25.4/§25.5）。
//
// 资源在会话建立时被接纳并由 finalizer 释放；未选中 Cursor 的供应商不登记任何转交，
// 仍按原 HTTP 生命周期释放。
type CursorSessionHandover interface {
	// Resources 返回本 HTTP 已获取、可转交的资源句柄（用户槽、账号槽、预留）。
	Resources() []CursorTransferableResource
	// Settlement 返回唯一结算回调；nil 表示本会话不做服务端结算。
	Settlement() CursorSessionSettlement
}

// CursorSettlementOwner 是 Cursor 会话的结算所有者（由 handler 持有，§25.5）。
//
// 各 handler 用它识别"已由 session 结算"：不再为每个工具 HTTP 排一次 UsageRecordTask，
// 而是把该任务登记给会话唯一结算。
type CursorSettlementOwner interface {
	// AdoptSettlementTask 登记本 HTTP 的计费任务，由会话终结时至多执行一次。
	// 会话已终结则按被接纳的真实终态决定是否立刻执行（绝不重复、绝不写零 usage）。
	AdoptSettlementTask(task UsageRecordTask)
}

// cursorHandoverCtxKey 是本层私有的 context 键。
//
// 值用包装结构体承载，因此"清空"可以显式覆盖为 nil 转交物（handler 在 Cursor 尝试
// 失败后换号时必须清掉，避免把上一次的转交物错误地交给下一次尝试）。
type cursorHandoverCtxKey struct{}

// cursorHandoverCtxValue 是 context 上的转交物包装（nil = 已显式清空）。
type cursorHandoverCtxValue struct {
	handover CursorSessionHandover
}

// WithCursorSessionHandover 把转交物登记到请求 context（仅 Cursor 分支调用）。
func WithCursorSessionHandover(ctx context.Context, handover CursorSessionHandover) context.Context {
	if ctx == nil || handover == nil {
		return ctx
	}
	return context.WithValue(ctx, cursorHandoverCtxKey{}, cursorHandoverCtxValue{handover: handover})
}

// WithCursorSessionHandoverCleared 显式清空请求 context 上的转交物。
//
// 用于 Cursor 尝试失败后换号：下一次尝试不得复用上一次的转交物与结算所有者。
func WithCursorSessionHandoverCleared(ctx context.Context) context.Context {
	if ctx == nil {
		return ctx
	}
	return context.WithValue(ctx, cursorHandoverCtxKey{}, cursorHandoverCtxValue{})
}

// CursorSessionHandoverFromContext 读取请求 context 上的转交物（可能为 nil）。
func CursorSessionHandoverFromContext(ctx context.Context) CursorSessionHandover {
	if ctx == nil {
		return nil
	}
	value, ok := ctx.Value(cursorHandoverCtxKey{}).(cursorHandoverCtxValue)
	if !ok {
		return nil
	}
	return value.handover
}

// CursorSettlementOwnerFromContext 读取请求 context 上的结算所有者（可能为 nil）。
//
// 结算所有者与转交物是**同一个对象**（handler 的转交载体自身实现两个接口），
// 因此不存在"资源转交了但结算归属不同"的第二种状态。
func CursorSettlementOwnerFromContext(ctx context.Context) CursorSettlementOwner {
	handover := CursorSessionHandoverFromContext(ctx)
	if handover == nil {
		return nil
	}
	owner, _ := handover.(CursorSettlementOwner)
	return owner
}

// CursorSessionBillingEventID 是 Cursor **会话级**不可变账单事件 ID（§25.5）。
//
// 前缀明确命名（cursor_session:），与会话一一对应；它复用 resolveUsageBillingRequestID
// 既有 durable event 模式（同 web_search:/grok_* 的"强制 ID 优先"思路），但**不修改**
// 普通 HTTP 的优先级顺序，也不改变任何既有前缀语义。
func CursorSessionBillingEventID(sessionID string) string {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		id = cursorNewUUID()
	}
	return "cursor_session:" + id
}

// WithCursorSessionBillingEventID 把会话账单事件 ID 绑定到结算 ctx。
//
// 绑定走既有 ctxkey.ClientRequestID：resolveUsageBillingRequestID 会产出
// "client:cursor_session:<会话 ID>" 这一与会话一一对应、不可变、可重放幂等的键。
// 只用于 Cursor 会话结算，普通 HTTP 路径的 ID 解析优先级完全不变。
func WithCursorSessionBillingEventID(ctx context.Context, billingEventID string) context.Context {
	id := strings.TrimSpace(billingEventID)
	if ctx == nil || id == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxkey.ClientRequestID, id)
}

// CursorSettlementDurableRequestID 返回会话结算实际落库使用的 durable 键。
//
// 与 resolveUsageBillingRequestID 的 client 分支逐字一致；用同一函数产出，
// 避免结算侧与记录侧对"是否同一账"产生分歧。
func CursorSettlementDurableRequestID(billingEventID string) string {
	return resolveUsageBillingRequestID(WithCursorSessionBillingEventID(context.Background(), billingEventID), "")
}

// cursorUsageCompleteness 判定是否取得了"完整的真实 usage"（§25.5）。
//
// 只有原生 TurnEnded 出现过（Seen）**且**承载了至少一个真实计数字段或结束时刻时，
// 才认为拿到了完整真实 usage。空 TurnEnded 帧（字段全缺）在当前 CR-B 解析层与
// "真实 0"折叠成同一组 0，故本层以"全部为 0 且无结束时刻"判定为字段缺失：
// 不结算、不估算扣费、不写零 usage 假成功。
//
// 字段级存在性（缺失 vs 真实 0）需要 CR-B 解析层提供，本单不改其解析逻辑，
// 已登记为域外待办。
func cursorUsageCompleteness(u cursorTurnUsage) (bool, string) {
	if !u.Seen {
		return false, "turn_ended_missing"
	}
	if u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheWrite == 0 && u.EndedAtMS == 0 {
		return false, "turn_ended_fields_missing"
	}
	return true, ""
}

// cursorFinalizerSteps 是终结守卫：finalizer 的每一项在会话生命周期内最多执行一次（§25.4）。
type cursorFinalizerSteps struct {
	intakeStopped    bool
	streamClosed     bool
	settled          bool
	inflightReturned bool
	slotsReleased    bool
	indexRemoved     bool
	closed           bool
}

// beginStep 在锁内抢占一个终结步骤，返回是否由本次调用执行（每项至多一次）。
func (r *cursorSessionRecord) beginStep(target *bool) bool {
	if r == nil || target == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if *target {
		return false
	}
	*target = true
	return true
}

// BillingEventID 返回会话级不可变账单事件 ID（诊断/测试用）。
func (r *cursorSessionRecord) BillingEventID() string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.billingEventID
}

// SessionID 返回会话标识。
func (r *cursorSessionRecord) SessionID() string {
	if r == nil {
		return ""
	}
	return r.id
}

// Deadline 返回 180 秒总期限的绝对时刻（零值表示尚未发起 AgentRun）。
func (r *cursorSessionRecord) Deadline() time.Time {
	if r == nil {
		return time.Time{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deadline
}

// armAgentRunDeadline 在首次 AgentRun 发起时固定 180 秒总期限。
//
// **续轮不重置**：已固定的 deadline 原样返回，绝不因为新的 HTTP 附着而顺延
// （§25.4：跨 HTTP 和工具执行共用这 180 秒）。
func (r *cursorSessionRecord) armAgentRunDeadline(now time.Time) (deadline time.Time, source string, remaining time.Duration) {
	if r == nil {
		return time.Time{}, "", 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deadline.IsZero() {
		r.agentRunAt = now
		r.deadline = now.Add(cursorAgentRunTotalBudget)
		r.deadlineSource = cursorDeadlineSourceFirstAgentRun
	}
	remaining = time.Until(r.deadline)
	if r.deadlineSource != "" {
		remaining = r.deadline.Sub(now)
	}
	return r.deadline, r.deadlineSource, remaining
}

// sessionContext 由 HTTP context 派生出会话运行 context。
//
// 与 HTTP context 取消**解耦**（context.WithoutCancel）：正常 handler 返回触发的
// context 取消不能误杀已完成交接的会话；HTTP 附着期间的真实断连由 attachHTTP 的
// 取消回调判定（见 onHTTPAttachCancel）。deadline 取会话固定的 180 秒总期限。
func (r *cursorSessionRecord) sessionContext(httpCtx context.Context) (context.Context, context.CancelFunc) {
	base := context.WithoutCancel(httpCtx)
	if base == nil {
		base = context.Background()
	}
	r.mu.Lock()
	deadline := r.deadline
	r.mu.Unlock()
	if deadline.IsZero() {
		return context.WithCancel(base)
	}
	return context.WithDeadline(base, deadline)
}

// attachHTTP 把本 HTTP 注册为会话的当前附着端，返回幂等的 detach。
//
// 取消回调与交接竞争用**锁内状态**判定唯一胜者：detach 先把 attached 置 false 并停
// 回调；取消回调只在 attached 仍为 true 时才结束会话。禁止用 sleep 规避竞态。
func (r *cursorSessionRecord) attachHTTP(httpCtx context.Context) (func(), error) {
	if r == nil {
		return func() {}, nil
	}
	r.mu.Lock()
	if r.state == cursorSessionClosed || r.finishing {
		r.mu.Unlock()
		return nil, fmt.Errorf("%w: session %q is finalizing", ErrCursorSessionAttachConflict, r.id)
	}
	if r.attached {
		r.mu.Unlock()
		return nil, fmt.Errorf("%w: session %q", ErrCursorSessionAttachConflict, r.id)
	}
	r.attached = true
	if httpCtx != nil {
		if stop := r.httpAttachStop; stop != nil {
			_ = stop()
		}
		r.httpAttachStop = context.AfterFunc(httpCtx, r.onHTTPAttachCancel)
	}
	r.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			stop := r.httpAttachStop
			r.httpAttachStop = nil
			r.attached = false
			r.mu.Unlock()
			if stop != nil {
				_ = stop()
			}
		})
	}, nil
}

// onHTTPAttachCancel 是本 HTTP 附着期间的取消回调。
//
// 只有"正在输出/附着期间的真实断连/客户端取消"才结束会话；已 detach（正常返回）时
// 该回调是空操作，因此正常 handler 返回不会误杀会话。
func (r *cursorSessionRecord) onHTTPAttachCancel() {
	if r == nil {
		return
	}
	r.mu.Lock()
	stillAttached := r.attached && !r.finishing && r.state != cursorSessionClosed
	r.mu.Unlock()
	if !stillAttached {
		return
	}
	// 网络/状态操作在锁外：本回调不持锁调用 finalizer。
	if svc := r.owner; svc != nil {
		svc.finishCursorSession(r, "http_attach_cancelled")
	}
}

// noteStage 记录最后协议阶段（日志安全字段）。
func (r *cursorSessionRecord) noteStage(stage string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.usageStage = stage
}

// noteTurnUsage 记录真实终态 usage（finalizer 的唯一结算输入）。
func (r *cursorSessionRecord) noteTurnUsage(u cursorTurnUsage, stage string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if u.Seen {
		r.usage = u
	}
	r.usageStage = stage
}

// adoptHandover 接纳 handler 转交的资源与结算回调（§25.4/§25.5）。
//
// 任一资源交接失败即整体失败关闭：已成功的部分按原 HTTP 生命周期释放（幂等），
// 绝不把失效槽位交给会话，也不留下"半交接"状态。
func (s *CursorExecutionService) adoptHandover(ctx context.Context, record *cursorSessionRecord, sessionCtx context.Context) error {
	if s == nil || record == nil {
		return nil
	}
	handover := CursorSessionHandoverFromContext(ctx)
	if handover == nil {
		return nil
	}
	resources := handover.Resources()
	adopted := make([]CursorTransferableResource, 0, len(resources))
	for _, res := range resources {
		if res == nil {
			continue
		}
		if err := res.TransferTo(sessionCtx); err != nil {
			for _, done := range adopted {
				done.Release()
			}
			cursorPhaseLog(ctx, zapcore.WarnLevel, "session_handover_failed", record.spec.requestedModel,
				zap.String("session_id", record.id),
				zap.String("resource", res.Name()),
				zap.String("outcome", cursorHTTPErrClass(err)))
			return err
		}
		adopted = append(adopted, res)
	}
	record.mu.Lock()
	record.resources = adopted
	record.settlement = handover.Settlement()
	if holder, ok := handover.(interface{ CursorInflightReservation() *InflightReservation }); ok {
		record.inflight = holder.CursorInflightReservation()
	}
	record.mu.Unlock()
	cursorPhaseLog(ctx, zapcore.InfoLevel, "session_handover_done", record.spec.requestedModel,
		zap.String("session_id", record.id),
		zap.Int("resource_count", len(adopted)),
		zap.Bool("session_settlement", record.settlement != nil))
	return nil
}

// finishCursorSession 是**唯一 finalizer**（§25.4 末段）。
//
// 固定顺序：停止接新结果 → 关闭原生流/取消读写 → 已获完整真实 usage 则提交唯一结算
// → 归还/转交原预留 → 释放两槽 → 移除该 session 全部索引关联 → closed。
// 网络/计费操作全部在锁外；终结守卫保证每项最多一次；竞争（取消、deadline、服务 Stop、
// 正常返回）以**第一个进入 finalizer 的终态**为唯一胜者，不双写、不重复释放。
func (s *CursorExecutionService) finishCursorSession(record *cursorSessionRecord, reason string) {
	if s == nil || record == nil {
		return
	}
	record.mu.Lock()
	if record.finishing {
		accepted := record.interruptReason
		record.mu.Unlock()
		cursorPhaseLog(context.Background(), zapcore.DebugLevel, "session_finalize_duplicate", record.spec.requestedModel,
			zap.String("session_id", record.id),
			zap.String("terminal_reason", reason),
			zap.String("accepted_reason", accepted))
		return
	}
	record.finishing = true
	record.state = cursorSessionFinalizing
	record.interruptReason = reason
	deadline := record.deadline
	deadlineSource := record.deadlineSource
	record.mu.Unlock()

	cursorPhaseLog(context.Background(), zapcore.InfoLevel, "session_finalize_begin", record.spec.requestedModel,
		zap.String("session_id", record.id),
		zap.String("terminal_reason", reason),
		zap.Time("deadline", deadline),
		zap.String("deadline_source", deadlineSource),
		zap.Duration("deadline_remaining", time.Until(deadline)))

	// ① 停止接新结果：拒绝新的工具调用登记与新的 HTTP 附着。
	if record.beginStep(&record.steps.intakeStopped) {
		cursorPhaseLog(context.Background(), zapcore.DebugLevel, "session_finalize_intake_stopped", record.spec.requestedModel,
			zap.String("session_id", record.id))
	}

	// ② 关闭原生流/取消读写（网络操作在锁外）。
	record.mu.Lock()
	streamClose := record.streamClose
	record.streamClose = nil
	record.mu.Unlock()
	if record.beginStep(&record.steps.streamClosed) {
		if streamClose != nil {
			streamClose()
		}
		cursorPhaseLog(context.Background(), zapcore.DebugLevel, "session_finalize_stream_closed", record.spec.requestedModel,
			zap.String("session_id", record.id))
	}

	// ③ 唯一结算：只有被接纳的真实终态、且取得完整真实 usage 时才结算一次。
	settlementOwnsInflight := s.settleCursorSession(record, reason)

	// ④ 归还/转交原预留：结算提交之后才归还（§25.5 不得在扣减前先释放）。
	// 结算把归还委托给提交方时，这里不提前归还——否则会在缓存扣减前释放掉预留。
	if !settlementOwnsInflight {
		s.releaseCursorSessionInflight(record)
	}

	// ⑤ 释放两槽（用户槽 + 账号槽）。
	if record.beginStep(&record.steps.slotsReleased) {
		record.mu.Lock()
		resources := record.resources
		record.resources = nil
		record.mu.Unlock()
		for _, res := range resources {
			if res == nil {
				continue
			}
			res.Release()
			cursorPhaseLog(context.Background(), zapcore.DebugLevel, "session_finalize_resource_released", record.spec.requestedModel,
				zap.String("session_id", record.id), zap.String("resource", res.Name()))
		}
		cursorPhaseLog(context.Background(), zapcore.InfoLevel, "session_finalize_slots_released", record.spec.requestedModel,
			zap.String("session_id", record.id), zap.Int("resource_count", len(resources)))
	}

	// ⑥ 移除该 session 全部索引关联（统一索引用 CR-B 的整批清理入口）。
	if record.beginStep(&record.steps.indexRemoved) {
		s.sessions.close(record.id)
		cursorPhaseLog(context.Background(), zapcore.DebugLevel, "session_finalize_index_removed", record.spec.requestedModel,
			zap.String("session_id", record.id))
	}

	// ⑦ closed。
	record.mu.Lock()
	record.state = cursorSessionClosed
	record.outcome = reason
	record.mu.Unlock()
	if record.beginStep(&record.steps.closed) {
		cursorPhaseLog(context.Background(), zapcore.InfoLevel, "session_finalize_closed", record.spec.requestedModel,
			zap.String("session_id", record.id),
			zap.String("terminal_reason", reason))
	}
}

// settleCursorSession 提交**唯一结算**（至多一次，网络/计费在锁外）。
//
// 返回 true 表示"预留归还已委托给结算提交方"（结算任务提交之后才 SessionDone），
// finalizer 的④步骤据此决定是否立刻归还。
func (s *CursorExecutionService) settleCursorSession(record *cursorSessionRecord, reason string) bool {
	if record == nil || !record.beginStep(&record.steps.settled) {
		return false
	}
	record.mu.Lock()
	hook := record.settlement
	usage := record.usage
	stage := record.usageStage
	result := record.finalResult
	eventID := record.billingEventID
	sessionID := record.id
	inflight := record.inflight
	record.mu.Unlock()

	// 结算 ctx 是**会话级**的：Background 派生（不闭包 gin.Context/HTTP 生命周期），
	// 绑定会话级不可变账单事件 ID，并挂上预留句柄——计费任务复用既有 Acquire 交接，
	// 会话随后才 SessionDone（§25.5：不得在扣减前先释放预留）。
	settleCtx := WithCursorSessionBillingEventID(context.Background(), eventID)
	if inflight != nil {
		settleCtx = WithInflightReservation(settleCtx, inflight)
	}

	var inflightOnce sync.Once
	releaseInflight := func() {
		inflightOnce.Do(func() {
			if inflight != nil {
				inflight.SessionDone()
			}
		})
	}

	complete, missing := cursorUsageCompleteness(usage)
	ev := CursorSettlementEvent{
		InflightRelease:    releaseInflight,
		SessionID:          sessionID,
		BillingEventID:     eventID,
		Result:             result,
		UsageComplete:      complete,
		UsageMissingReason: missing,
		InterruptReason:    reason,
		LastProtocolStage:  stage,
	}
	if !complete {
		// 未取得完整真实 usage：不估算扣费、不写零 usage 假成功，只记录中断原因与
		// 最后协议阶段后释放资源（§25.5）。
		cursorPhaseLog(context.Background(), zapcore.WarnLevel, "session_settle_skipped", record.spec.requestedModel,
			zap.String("session_id", sessionID),
			zap.String("billing_event_id", eventID),
			zap.String("terminal_reason", reason),
			zap.String("usage_missing", missing),
			zap.String("last_protocol_stage", stage))
		if hook != nil {
			hook(settleCtx, ev)
		}
		releaseInflight()
		return false
	}
	if hook == nil {
		cursorPhaseLog(context.Background(), zapcore.WarnLevel, "session_settle_no_hook", record.spec.requestedModel,
			zap.String("session_id", sessionID),
			zap.String("billing_event_id", eventID))
		releaseInflight()
		return false
	}
	hook(settleCtx, ev)
	cursorPhaseLog(context.Background(), zapcore.InfoLevel, "session_settled", record.spec.requestedModel,
		zap.String("session_id", sessionID),
		zap.String("billing_event_id", eventID),
		zap.Int("input_tokens", usage.Input),
		zap.Int("output_tokens", usage.Output),
		zap.Int("cache_read_tokens", usage.CacheRead),
		zap.Int("cache_write_tokens", usage.CacheWrite),
		zap.String("last_protocol_stage", stage))
	return true
}

// releaseCursorSessionInflight 归还/转交原预留（至多一次，在结算提交之后）。
//
// §25.5：计费任务复用预留 Acquire 交接，HandlerDone 在提交之后调用；
// 绝不在缓存扣减前先释放预留，也不在 HTTP1 已 HandlerDone 之后假装保住续期。
func (s *CursorExecutionService) releaseCursorSessionInflight(record *cursorSessionRecord) {
	if record == nil || !record.beginStep(&record.steps.inflightReturned) {
		return
	}
	record.mu.Lock()
	inflight := record.inflight
	record.inflight = nil
	record.mu.Unlock()
	if inflight == nil {
		return
	}
	inflight.SessionDone()
	cursorPhaseLog(context.Background(), zapcore.DebugLevel, "session_finalize_inflight_returned", record.spec.requestedModel,
		zap.String("session_id", record.id))
}

// ===========================================================================
// §25.2 续轮识别 + §25.14-D 摘要校验与整批接管（派发单 W2-E）
// ===========================================================================
//
// 本段只做三件事，且顺序固定：
//
//	① 识别续轮：按各协议**最后一个未完成工具批次及其结果 ID**定位会话；
//	   完整历史里的旧工具 ID 不参与本次续轮判定。
//	② 校验摘要：固定生成配置摘要 + 重复前缀历史摘要 + 本批结果集合完整性；
//	   任一项不符即明确入参错误，**零上游写入**。
//	③ 整批接管：在同一服务锁内判唯一胜者（后者 409），锁外串行写真实结果。
//
// 摘要表示与算法全部来自 CR-B 的 cursor_session_digest.go（本层只提供 canonical 项，
// 不改其语义）；ID 关联全部来自 CR-B 的 cursor_session_index.go（本层只做薄适配）。
// 会话里只落类型化摘要与项数，**不落**原始正文/完整历史/gin.Context。

// CursorResumeProtocol 是客户端可见协议名，也是统一索引的协议命名空间。
type CursorResumeProtocol string

const (
	// CursorResumeProtocolChat 对应 /v1/chat/completions 入口。
	CursorResumeProtocolChat CursorResumeProtocol = "chat"
	// CursorResumeProtocolMessages 对应 /v1/messages 入口。
	CursorResumeProtocolMessages CursorResumeProtocol = "messages"
	// CursorResumeProtocolResponses 对应 /v1/responses 入口。
	CursorResumeProtocolResponses CursorResumeProtocol = "responses"
	// CursorResumeProtocolGemini 对应 Gemini 原生 v1beta 入口。
	CursorResumeProtocolGemini CursorResumeProtocol = "gemini"
)

// CursorSessionIdentity 是会话唯一身份（user / API key / 认证组）。
//
// §25.14-D「最小表示」：身份只存会话唯一记录，索引不复制；续轮从这里读取比对，
// 缺失字段（0）表示该维度未声明，不做"猜测补全"。
type CursorSessionIdentity struct {
	UserID   int64
	APIKeyID int64
	GroupID  int64
}

// compatibleWith 判断调用方身份是否与会话登记身份一致（缺失维度不参与比较）。
func (id CursorSessionIdentity) compatibleWith(incoming CursorSessionIdentity) bool {
	if id.UserID != 0 && incoming.UserID != id.UserID {
		return false
	}
	if id.APIKeyID != 0 && incoming.APIKeyID != id.APIKeyID {
		return false
	}
	if id.GroupID != 0 && incoming.GroupID != id.GroupID {
		return false
	}
	return true
}

// 续轮层的失败关闭错误（全部零上游写入；状态码由 CursorResumeHTTPStatus 给出）。
var (
	// ErrCursorResumeUnknownAssociation 表示提交的结果 ID 没有任何已登记关联
	// （含重启后的孤儿续轮）：明确 400，不猜最近会话、不留墓碑表。
	ErrCursorResumeUnknownAssociation = errors.New("cursor: no registered association for the submitted ids")
	// ErrCursorResumeMixedSessions 表示提交的 ID 分属不同会话（跨会话混批）。
	ErrCursorResumeMixedSessions = errors.New("cursor: submitted ids belong to different sessions")
	// ErrCursorResumeDuplicateResults 表示同一批里出现重复结果 ID。
	ErrCursorResumeDuplicateResults = errors.New("cursor: duplicate tool result ids in one batch")
	// ErrCursorResumeStaleBatch 表示 ID 不属于会话**当前**待完成批次（旧批/旧 response）。
	ErrCursorResumeStaleBatch = errors.New("cursor: submitted ids do not belong to the current pending batch")
	// ErrCursorResumeIdentityMismatch 表示会话所有者与本次鉴权身份不符。
	ErrCursorResumeIdentityMismatch = errors.New("cursor: session owner does not match the authenticated caller")
	// ErrCursorResumeConfigChanged 表示固定生成配置（system/模型/工具/schema/推理等）被改动。
	ErrCursorResumeConfigChanged = errors.New("cursor: fixed generation config changed in continuation")
	// ErrCursorResumeHistoryMismatch 表示重复前缀历史摘要或逻辑项数与已交付历史不符。
	ErrCursorResumeHistoryMismatch = errors.New("cursor: repeated history prefix does not match the delivered history")
	// ErrCursorResumeInstructionsWithdrawn 表示首轮非空 instructions/instructions 被省略，
	// 即"撤换原 instructions"：本适配不支持，明确入参错误（不偷偷继承）。
	ErrCursorResumeInstructionsWithdrawn = errors.New("cursor: instructions cannot be withdrawn by a continuation")
	// ErrCursorResumeToolSetChanged 表示工具声明集合与已暴露集合不一致（含扩集合）。
	ErrCursorResumeToolSetChanged = errors.New("cursor: tool declaration set changed in continuation")
	// ErrCursorResumeBatchIncomplete 表示提交的本批结果缺少部分已交付调用。
	ErrCursorResumeBatchIncomplete = errors.New("cursor: continuation is missing results for part of the delivered batch")
	// ErrCursorResumeAmbiguousResults 表示并行同名结果缺 ID，无法配对（不按名字猜）。
	ErrCursorResumeAmbiguousResults = errors.New("cursor: parallel tool results cannot be paired without ids")
	// ErrCursorResumeUnsupportedContent 表示续轮上下文含本站未接纳的内容类型。
	ErrCursorResumeUnsupportedContent = errors.New("cursor: unsupported content type in continuation context")
	// ErrCursorResumeNotWaiting 表示会话不在等待工具结果状态。
	ErrCursorResumeNotWaiting = errors.New("cursor: session is not waiting for tool results")
	// ErrCursorResumeBatchNotDelivered 表示该批未完整写出（部分写出按 §25.4 终止）。
	ErrCursorResumeBatchNotDelivered = errors.New("cursor: batch was not fully delivered, continuation rejected")
	// ErrCursorResumeAlreadyConsumed 表示该批结果已被消费（重复提交 → 409，不重放）。
	ErrCursorResumeAlreadyConsumed = errors.New("cursor: batch results already consumed (409)")
	// ErrCursorResumeSessionBusy 表示会话已有附着中的写端（并发写同一 AgentRun → 409）。
	ErrCursorResumeSessionBusy = errors.New("cursor: session already has an attached http writer (409)")
	// ErrCursorResumeIntakeUnavailable 表示上游真实结果回写通道未接通：失败关闭，
	// 零上游写入，绝不把"未写入"包装成成功。
	ErrCursorResumeIntakeUnavailable = errors.New("cursor: upstream tool result intake is not wired (fail closed)")
)

// CursorResumeError 携带续轮失败的明确 HTTP 状态与安全 reason（不含正文/凭据）。
type CursorResumeError struct {
	// Status 是应返回给客户端的 HTTP 状态（400 / 403 / 409 / 503）。
	Status int
	// Reason 是稳定枚举串（日志/测试断言用，不是给终端用户的正文）。
	Reason string
	// Err 是底层语义错误（errors.Is 可判定）。
	Err error
}

// Error 实现 error。
func (e *CursorResumeError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err == nil {
		return "cursor: resume rejected: " + e.Reason
	}
	return "cursor: resume rejected (" + e.Reason + "): " + e.Err.Error()
}

// Unwrap 暴露底层错误，支持 errors.Is。
func (e *CursorResumeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// newCursorResumeError 构造带状态的续轮错误。
func newCursorResumeError(status int, reason string, err error) *CursorResumeError {
	return &CursorResumeError{Status: status, Reason: reason, Err: err}
}

// CursorResumeHTTPStatus 返回续轮错误应使用的 HTTP 状态与安全 reason。
//
// 非续轮错误返回 (0, "")：调用方按既有上游错误链处理。
func CursorResumeHTTPStatus(err error) (int, string) {
	var resumeErr *CursorResumeError
	if errors.As(err, &resumeErr) {
		return resumeErr.Status, resumeErr.Reason
	}
	return 0, ""
}

// ---------------------------------------------------------------------------
// 上下文项与摘要计划（§25.14-D「最小表示 / 摘要算法」）
// ---------------------------------------------------------------------------

// cursorContextItem 是一条已规范化的逻辑上下文项。
//
// 只持有摘要类型与 canonical 字节（随即并入链式摘要），不留正文副本。
type cursorContextItem struct {
	itemType  string
	canonical []byte
}

// CursorToolResult 是一条客户端真实工具结果（§25.3 编码的输入形状）。
//
// Text 按顺序保留文本结果项；Structured 是结构化 JSON（可为空）；IsError 原样保留
// 客户端明确声明的错误语义（绝不把客户端失败写成成功）。
type CursorToolResult struct {
	CallID     string
	Text       []string
	Structured []byte
	IsError    bool
}

// CursorSessionIntake 是真实工具结果回写通道（由原生双工持有方安装）。
//
// 本层只在**锁外**串行调用它一次；未安装时续轮失败关闭（零上游写入）。
type CursorSessionIntake interface {
	// DeliverToolResults 把已整批校验的真实结果写回原 AgentRun 的 exec 关联。
	DeliverToolResults(ctx context.Context, sessionID, batchID string, results []CursorToolResult) error
}

// cursorContextPlan 是一次请求的上下文计划。
type cursorContextPlan struct {
	// config 是固定生成配置的链式摘要（§25.14-D「配置与省略规则」）。
	config *CursorHistoryDigest
	// history 是**重复前缀**（已交付历史）的链式摘要；本批新结果不在其中。
	history *CursorHistoryDigest
	// tools 是本次声明的工具名（保持声明顺序）。
	tools []string
	// results 是本批真实工具结果（按交付顺序），仅续轮请求非空。
	results []CursorToolResult
	// delivered 是本次交付的工具调用（首 HTTP 生成历史时追加，续轮为空）。
	delivered []cursorContextItem
	// instructions 记录本次是否声明了非空 instructions（Responses 用）。
	instructions bool
}

// cursorConfigBuilder 把固定生成配置逐项并入链式摘要。
type cursorConfigBuilder struct {
	d *CursorHistoryDigest
}

func newCursorConfigBuilder() *cursorConfigBuilder {
	return &cursorConfigBuilder{d: NewCursorHistoryDigest()}
}

// text 记录一个文本配置项。
func (b *cursorConfigBuilder) text(itemType, value string) error {
	return b.d.AppendText(itemType, value)
}

// presence 记录"字段是否声明 + 声明值"（省略与显式值不自行折叠）。
func (b *cursorConfigBuilder) presence(itemType string, raw json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return b.d.AppendText(itemType, "absent")
	}
	return b.d.AppendItem(itemType, append([]byte("present:"), trimmed...))
}

// floatPresence 记录 *float64 配置项（nil = 未声明）。
func (b *cursorConfigBuilder) floatPresence(itemType string, v *float64) error {
	if v == nil {
		return b.d.AppendText(itemType, "absent")
	}
	return b.d.AppendText(itemType, fmt.Sprintf("present:%v", *v))
}

// intPresence 记录 *int 配置项（nil = 未声明）。
func (b *cursorConfigBuilder) intPresence(itemType string, v *int) error {
	if v == nil {
		return b.d.AppendText(itemType, "absent")
	}
	return b.d.AppendText(itemType, fmt.Sprintf("present:%d", *v))
}

// boolPresence 记录 *bool 配置项（nil = 未声明）。
func (b *cursorConfigBuilder) boolPresence(itemType string, v *bool) error {
	if v == nil {
		return b.d.AppendText(itemType, "absent")
	}
	return b.d.AppendText(itemType, fmt.Sprintf("present:%t", *v))
}

// json 记录一个 JSON 配置项（空输入按"未声明"记录，不做等价折叠）。
func (b *cursorConfigBuilder) json(itemType string, raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return b.d.AppendText(itemType, "absent")
	}
	return b.d.AppendJSON(itemType, trimmed)
}

// digest 返回配置摘要。
func (b *cursorConfigBuilder) digest() *CursorHistoryDigest { return b.d }

// appendCursorContextItems 把一组项按顺序并入历史链。
func appendCursorContextItems(d *CursorHistoryDigest, items []cursorContextItem) error {
	for _, it := range items {
		if err := d.AppendItem(it.itemType, it.canonical); err != nil {
			return err
		}
	}
	return nil
}

// cursorCanonicalToolArgs 规范化工具参数 JSON（仅空白/键序差异视为同值）。
func cursorCanonicalToolArgs(args string) ([]byte, error) {
	return CursorCanonicalToolArgsJSON([]byte(args))
}

// cursorToolCallCanonical 生成一条工具调用项的 canonical JSON。
func cursorToolCallCanonical(id, name, args string) ([]byte, error) {
	canonicalArgs, err := cursorCanonicalToolArgs(args)
	if err != nil {
		return nil, err
	}
	payload := map[string]json.RawMessage{
		"id":        mustCursorJSON(id),
		"name":      mustCursorJSON(name),
		"arguments": canonicalArgs,
	}
	return CursorCanonicalJSON(mustCursorJSONValue(payload))
}

// mustCursorJSON 把字符串编码为 JSON 字面量（编码失败时退化为空串，绝不 panic）。
func mustCursorJSON(s string) json.RawMessage {
	encoded, err := json.Marshal(s)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return json.RawMessage(encoded)
}

// mustCursorJSONValue 把 map 编码为 JSON 字节（键序由 CursorCanonicalJSON 统一规范）。
func mustCursorJSONValue(v map[string]json.RawMessage) []byte {
	encoded, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return encoded
}

// cursorChatContentText 提取 Chat 消息正文（字符串或文本分段），不做 trim/换行改写。
func cursorChatContentText(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "", nil
	}
	var asString string
	if err := json.Unmarshal(trimmed, &asString); err == nil {
		return asString, nil
	}
	var parts []apicompat.ChatContentPart
	if err := json.Unmarshal(trimmed, &parts); err == nil {
		var b strings.Builder
		for _, part := range parts {
			switch part.Type {
			case "text", "input_text", "output_text", "":
				b.WriteString(part.Text)
			case "image_url", "file":
				// 图像/文件属于已接纳的输入语义：按出现顺序参与摘要（不丢语义字段）。
				encoded, err := json.Marshal(part)
				if err != nil {
					return "", fmt.Errorf("%w: chat content part marshal", ErrCursorResumeUnsupportedContent)
				}
				b.WriteString("\x00part:")
				b.Write(encoded)
			default:
				return "", fmt.Errorf("%w: chat content part %q", ErrCursorResumeUnsupportedContent, part.Type)
			}
		}
		return b.String(), nil
	}
	return "", fmt.Errorf("%w: undecodable chat content", ErrCursorResumeUnsupportedContent)
}

// cursorChatToolItem 生成一条工具声明项（顺序保留，不改写 schema）。
func cursorChatToolItem(tool apicompat.ChatTool) (cursorContextItem, string, error) {
	encoded, err := json.Marshal(tool)
	if err != nil {
		return cursorContextItem{}, "", fmt.Errorf("%w: chat tool marshal", ErrCursorResumeUnsupportedContent)
	}
	name := ""
	if tool.Function != nil {
		name = strings.TrimSpace(tool.Function.Name)
	}
	return cursorContextItem{itemType: "config.tool", canonical: encoded}, name, nil
}

// cursorPlanFromChat 按 §25.14-D「Chat」表示生成上下文计划。
//
// 规则：
//   - 固定配置：模型、system/developer 文本、工具声明（顺序保留）、sampling/推理/输出格式；
//   - 历史前缀：既有 messages 按角色/顺序/文本及结构化 tool_calls 规范化；
//     assistant 的空 content 与省略统一为空文本；
//   - 本批结果：**尾部连续**的 role=tool 消息，按 tool_call_id 匹配（不按工具名）。
func cursorPlanFromChat(req *apicompat.ChatCompletionsRequest, model string) (*cursorContextPlan, error) {
	if req == nil {
		return nil, fmt.Errorf("%w: nil chat request", ErrCursorUnsupportedRequest)
	}
	builder := newCursorConfigBuilder()
	if err := builder.text("config.model", model); err != nil {
		return nil, err
	}
	if err := builder.text("config.instructions", req.Instructions); err != nil {
		return nil, err
	}
	plan := &cursorContextPlan{config: builder.digest(), history: NewCursorHistoryDigest()}
	plan.instructions = strings.TrimSpace(req.Instructions) != ""

	tools := make([]string, 0, len(req.Tools))
	for _, tool := range req.Tools {
		item, name, err := cursorChatToolItem(tool)
		if err != nil {
			return nil, err
		}
		if err := plan.config.AppendItem(item.itemType, item.canonical); err != nil {
			return nil, err
		}
		if name != "" {
			tools = append(tools, name)
		}
	}
	plan.tools = tools
	if err := builder.json("config.response_format", req.ResponseFormat); err != nil {
		return nil, err
	}
	if err := builder.floatPresence("config.temperature", req.Temperature); err != nil {
		return nil, err
	}
	if err := builder.floatPresence("config.top_p", req.TopP); err != nil {
		return nil, err
	}
	if err := builder.intPresence("config.max_tokens", req.MaxTokens); err != nil {
		return nil, err
	}
	if err := builder.intPresence("config.max_completion_tokens", req.MaxCompletionTokens); err != nil {
		return nil, err
	}
	if err := builder.text("config.reasoning_effort", req.ReasoningEffort); err != nil {
		return nil, err
	}
	if err := builder.boolPresence("config.parallel_tool_calls", req.ParallelToolCalls); err != nil {
		return nil, err
	}
	if err := builder.json("config.stop", req.Stop); err != nil {
		return nil, err
	}
	// 配置项在工具声明之后继续并入同一链（顺序固定，两端同源）。
	plan.config = builder.digest()

	// 尾部连续工具结果 = 本批结果。
	start := len(req.Messages)
	for start > 0 {
		role := strings.ToLower(strings.TrimSpace(req.Messages[start-1].Role))
		if role != "tool" && role != "function" {
			break
		}
		start--
	}
	for _, msg := range req.Messages[start:] {
		callID := strings.TrimSpace(msg.ToolCallID)
		if callID == "" {
			return nil, fmt.Errorf("%w: chat tool result without tool_call_id", ErrCursorResumeAmbiguousResults)
		}
		text, err := cursorChatContentText(msg.Content)
		if err != nil {
			return nil, err
		}
		plan.results = append(plan.results, CursorToolResult{CallID: callID, Text: []string{text}})
	}
	for _, msg := range req.Messages[:start] {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		switch role {
		case "system", "developer":
			text, err := cursorChatContentText(msg.Content)
			if err != nil {
				return nil, err
			}
			if err := plan.config.AppendText("config.system", text); err != nil {
				return nil, err
			}
			continue
		case "user", "assistant", "tool", "function":
		default:
			return nil, fmt.Errorf("%w: chat role %q", ErrCursorResumeUnsupportedContent, msg.Role)
		}
		text, err := cursorChatContentText(msg.Content)
		if err != nil {
			return nil, err
		}
		if role == "user" {
			if err := plan.history.AppendText("chat.user", text); err != nil {
				return nil, err
			}
		} else {
			// assistant（与 tool/function 历史结果）：文本项 + 结构化 tool_calls。
			itemType := "chat.assistant.text"
			if role != "assistant" {
				itemType = "chat.tool_result"
			}
			if err := plan.history.AppendText(itemType, text); err != nil {
				return nil, err
			}
		}
		for _, call := range msg.ToolCalls {
			canonical, err := cursorToolCallCanonical(call.ID, call.Function.Name, call.Function.Arguments)
			if err != nil {
				return nil, err
			}
			if err := plan.history.AppendItem("chat.assistant.tool_call", canonical); err != nil {
				return nil, err
			}
		}
	}
	return plan, nil
}

// cursorAnthropicBlocks 解出 Anthropic content（字符串或块数组）。
func cursorAnthropicBlocks(raw json.RawMessage) ([]apicompat.AnthropicContentBlock, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, nil
	}
	var asString string
	if err := json.Unmarshal(trimmed, &asString); err == nil {
		return []apicompat.AnthropicContentBlock{{Type: "text", Text: asString}}, nil
	}
	var blocks []apicompat.AnthropicContentBlock
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return nil, fmt.Errorf("%w: undecodable anthropic content", ErrCursorResumeUnsupportedContent)
	}
	return blocks, nil
}

// cursorAnthropicBlockItem 把单个内容块规范化为历史项（保留类型与内容顺序）。
func cursorAnthropicBlockItem(role string, block apicompat.AnthropicContentBlock) (cursorContextItem, error) {
	prefix := "messages." + role
	switch block.Type {
	case "text", "":
		return cursorContextItem{itemType: prefix + ".text", canonical: CursorCanonicalText(block.Text)}, nil
	case "thinking", "redacted_thinking":
		encoded, err := json.Marshal(block)
		if err != nil {
			return cursorContextItem{}, fmt.Errorf("%w: anthropic thinking", ErrCursorResumeUnsupportedContent)
		}
		return cursorContextItem{itemType: prefix + ".thinking", canonical: encoded}, nil
	case "image":
		encoded, err := json.Marshal(block)
		if err != nil {
			return cursorContextItem{}, fmt.Errorf("%w: anthropic image", ErrCursorResumeUnsupportedContent)
		}
		return cursorContextItem{itemType: prefix + ".image", canonical: encoded}, nil
	case "tool_use":
		canonicalArgs, err := cursorCanonicalToolArgs(string(block.Input))
		if err != nil {
			// custom 文本输入不得当 JSON 强行改写：原文按字节参与摘要。
			canonicalArgs = CursorCanonicalText(string(block.Input))
		}
		payload := map[string]json.RawMessage{
			"id":    mustCursorJSON(block.ID),
			"name":  mustCursorJSON(block.Name),
			"input": canonicalArgs,
		}
		return cursorContextItem{itemType: prefix + ".tool_use", canonical: mustCursorJSONValue(payload)}, nil
	case "tool_result":
		content, err := cursorAnthropicBlocks(block.Content)
		if err != nil {
			return cursorContextItem{}, err
		}
		var text strings.Builder
		for _, inner := range content {
			switch inner.Type {
			case "text", "":
				text.WriteString(inner.Text)
			default:
				// 结果内的非文本内容原样参与摘要（不丢语义）。
				encoded, jerr := json.Marshal(inner)
				if jerr != nil {
					return cursorContextItem{}, fmt.Errorf("%w: anthropic tool_result block", ErrCursorResumeUnsupportedContent)
				}
				text.WriteString("\x00block:")
				text.Write(encoded)
			}
		}
		payload := map[string]json.RawMessage{
			"tool_use_id": mustCursorJSON(block.ToolUseID),
			"content":     mustCursorJSON(text.String()),
			"is_error":    mustCursorJSON(fmt.Sprintf("%t", block.IsError)),
		}
		return cursorContextItem{itemType: "messages.tool_result", canonical: mustCursorJSONValue(payload)}, nil
	default:
		return cursorContextItem{}, fmt.Errorf("%w: anthropic block %q", ErrCursorResumeUnsupportedContent, block.Type)
	}
}

// cursorPlanFromMessages 按 §25.14-D「Messages」表示生成上下文计划。
//
// 规则：
//   - 保留 system 及 content 块的类型/内容顺序；
//   - 最后一批 tool_result 按 tool_use_id 匹配；
//   - 仅为同一批结果的单/多条容器分组差异规范化（多容器按块顺序拼平）；
//   - 文本与 is_error 原样保留，不把新增用户文字吞进结果。
func cursorPlanFromMessages(req *apicompat.AnthropicRequest, model string) (*cursorContextPlan, error) {
	if req == nil {
		return nil, fmt.Errorf("%w: nil anthropic request", ErrCursorUnsupportedRequest)
	}
	builder := newCursorConfigBuilder()
	if err := builder.text("config.model", model); err != nil {
		return nil, err
	}
	plan := &cursorContextPlan{config: builder.digest(), history: NewCursorHistoryDigest()}
	systemBlocks, err := cursorAnthropicBlocks(req.System)
	if err != nil {
		return nil, err
	}
	for _, block := range systemBlocks {
		item, err := cursorAnthropicBlockItem("system", block)
		if err != nil {
			return nil, err
		}
		if err := plan.config.AppendItem(item.itemType, item.canonical); err != nil {
			return nil, err
		}
	}
	tools := make([]string, 0, len(req.Tools))
	for _, tool := range req.Tools {
		encoded, err := json.Marshal(tool)
		if err != nil {
			return nil, fmt.Errorf("%w: messages tool marshal", ErrCursorResumeUnsupportedContent)
		}
		if err := plan.config.AppendItem("config.tool", encoded); err != nil {
			return nil, err
		}
		if name := strings.TrimSpace(tool.Name); name != "" {
			tools = append(tools, name)
		}
	}
	plan.tools = tools
	if err := builder.floatPresence("config.temperature", req.Temperature); err != nil {
		return nil, err
	}
	if err := builder.floatPresence("config.top_p", req.TopP); err != nil {
		return nil, err
	}
	if err := builder.text("config.max_tokens", fmt.Sprintf("%d", req.MaxTokens)); err != nil {
		return nil, err
	}
	if req.Thinking != nil {
		encoded, err := json.Marshal(req.Thinking)
		if err != nil {
			return nil, fmt.Errorf("%w: messages thinking config", ErrCursorResumeUnsupportedContent)
		}
		if err := plan.config.AppendItem("config.thinking", encoded); err != nil {
			return nil, err
		}
	} else if err := builder.text("config.thinking", "absent"); err != nil {
		return nil, err
	}
	if req.OutputConfig != nil {
		encoded, err := json.Marshal(req.OutputConfig)
		if err != nil {
			return nil, fmt.Errorf("%w: messages output config", ErrCursorResumeUnsupportedContent)
		}
		if err := plan.config.AppendItem("config.output", encoded); err != nil {
			return nil, err
		}
	} else if err := builder.text("config.output", "absent"); err != nil {
		return nil, err
	}
	plan.config = builder.digest()

	// 尾部连续的"全部为 tool_result 的 user 消息" = 本批结果容器（单/多容器统一规范化）。
	start := len(req.Messages)
	for start > 0 {
		msg := req.Messages[start-1]
		if strings.ToLower(strings.TrimSpace(msg.Role)) != "user" {
			break
		}
		blocks, berr := cursorAnthropicBlocks(msg.Content)
		if berr != nil {
			return nil, berr
		}
		if len(blocks) == 0 {
			break
		}
		allResults := true
		for _, block := range blocks {
			if block.Type != "tool_result" {
				allResults = false
				break
			}
		}
		if !allResults {
			break
		}
		start--
	}
	for _, msg := range req.Messages[start:] {
		blocks, berr := cursorAnthropicBlocks(msg.Content)
		if berr != nil {
			return nil, berr
		}
		for _, block := range blocks {
			if strings.TrimSpace(block.ToolUseID) == "" {
				return nil, fmt.Errorf("%w: messages tool_result without tool_use_id", ErrCursorResumeAmbiguousResults)
			}
			inner, berr := cursorAnthropicBlocks(block.Content)
			if berr != nil {
				return nil, berr
			}
			texts := make([]string, 0, len(inner))
			for _, piece := range inner {
				switch piece.Type {
				case "text", "":
					texts = append(texts, piece.Text)
				default:
					return nil, fmt.Errorf("%w: tool_result block %q", ErrCursorResumeUnsupportedContent, piece.Type)
				}
			}
			if len(texts) == 0 {
				texts = append(texts, "")
			}
			plan.results = append(plan.results, CursorToolResult{
				CallID:  strings.TrimSpace(block.ToolUseID),
				Text:    texts,
				IsError: block.IsError,
			})
		}
	}
	for _, msg := range req.Messages[:start] {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		if role != "user" && role != "assistant" {
			return nil, fmt.Errorf("%w: messages role %q", ErrCursorResumeUnsupportedContent, msg.Role)
		}
		blocks, berr := cursorAnthropicBlocks(msg.Content)
		if berr != nil {
			return nil, berr
		}
		for _, block := range blocks {
			item, ierr := cursorAnthropicBlockItem(role, block)
			if ierr != nil {
				return nil, ierr
			}
			if err := plan.history.AppendItem(item.itemType, item.canonical); err != nil {
				return nil, err
			}
		}
	}
	return plan, nil
}

// cursorResponsesInputItems 解出 Responses input（字符串或条目数组）。
func cursorResponsesInputItems(raw json.RawMessage) ([]apicompat.ResponsesInputItem, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, nil
	}
	var asString string
	if err := json.Unmarshal(trimmed, &asString); err == nil {
		content, err := json.Marshal(asString)
		if err != nil {
			return nil, fmt.Errorf("%w: responses string input", ErrCursorResumeUnsupportedContent)
		}
		return []apicompat.ResponsesInputItem{{Type: "message", Role: "user", Content: content}}, nil
	}
	var items []apicompat.ResponsesInputItem
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, fmt.Errorf("%w: undecodable responses input", ErrCursorResumeUnsupportedContent)
	}
	return items, nil
}

// cursorResponsesItemIsOutput 判断条目是否为客户端工具结果（本批候选）。
func cursorResponsesItemIsOutput(item apicompat.ResponsesInputItem) bool {
	switch strings.ToLower(strings.TrimSpace(item.Type)) {
	case "function_call_output", "custom_tool_call_output", "local_shell_call_output",
		"mcp_call_output", "tool_search_output", "image_generation_call_output":
		return true
	}
	return false
}

// cursorResponsesItemItem 把单个 input 条目规范化为历史项（保留语义字段）。
func cursorResponsesItemItem(item apicompat.ResponsesInputItem) (cursorContextItem, error) {
	itemType := strings.ToLower(strings.TrimSpace(item.Type))
	switch itemType {
	case "", "message":
		text, err := cursorChatContentText(item.Content)
		if err != nil {
			return cursorContextItem{}, err
		}
		role := strings.ToLower(strings.TrimSpace(item.Role))
		return cursorContextItem{itemType: "responses.message." + role, canonical: CursorCanonicalText(text)}, nil
	case "function_call":
		canonical, err := cursorToolCallCanonical(item.CallID, item.Name, item.Arguments)
		if err != nil {
			return cursorContextItem{}, err
		}
		return cursorContextItem{itemType: "responses.function_call", canonical: canonical}, nil
	case "custom_tool_call":
		payload := map[string]json.RawMessage{
			"call_id": mustCursorJSON(item.CallID),
			"name":    mustCursorJSON(item.Name),
			// custom 文本输入不当 JSON 强行改写。
			"input": mustCursorJSON(item.Arguments),
		}
		return cursorContextItem{itemType: "responses.custom_tool_call", canonical: mustCursorJSONValue(payload)}, nil
	case "reasoning":
		payload := map[string]json.RawMessage{
			"encrypted_content": mustCursorJSON(item.EncryptedContent),
		}
		if summary := bytes.TrimSpace(item.Summary); len(summary) > 0 && string(summary) != "null" {
			canonical, err := CursorCanonicalJSON(summary)
			if err != nil {
				return cursorContextItem{}, err
			}
			payload["summary"] = canonical
		}
		return cursorContextItem{itemType: "responses.reasoning", canonical: mustCursorJSONValue(payload)}, nil
	default:
		encoded, err := json.Marshal(item)
		if err != nil {
			return cursorContextItem{}, fmt.Errorf("%w: responses item %q", ErrCursorResumeUnsupportedContent, item.Type)
		}
		return cursorContextItem{itemType: "responses.item." + cursorDigestTypeToken(itemType), canonical: encoded}, nil
	}
}

// cursorDigestTypeToken 把任意类型名收敛为合法摘要类型片段（只允许安全字符集）。
func cursorDigestTypeToken(raw string) string {
	if raw == "" {
		return "unknown"
	}
	out := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-'
		if !ok {
			c = '_'
		}
		out = append(out, c)
	}
	return string(out)
}

// cursorPlanFromResponses 按 §25.14-D「Responses」表示生成上下文计划。
//
// 规则：
//   - 完整 input：message/function_call/custom/namespace/tool_search 等按实际类型规范化，
//     保留已输出工具元数据与关联，**只剥离当前批 output**，前缀必须完整匹配；
//   - previous_response_id：可仅携带本批结果（省略历史）；若又提供完整重复历史，
//     必须整段摘要匹配（不把任意短前缀当完整历史）；
//   - instructions 不继承：首轮非空、续轮省略 => 明确入参错误（由 Resume 判定）。
func cursorPlanFromResponses(req *apicompat.ResponsesRequest, model string) (*cursorContextPlan, error) {
	if req == nil {
		return nil, fmt.Errorf("%w: nil responses request", ErrCursorUnsupportedRequest)
	}
	builder := newCursorConfigBuilder()
	if err := builder.text("config.model", model); err != nil {
		return nil, err
	}
	plan := &cursorContextPlan{config: builder.digest(), history: NewCursorHistoryDigest()}
	plan.instructions = strings.TrimSpace(req.Instructions) != ""
	if err := plan.config.AppendText("config.instructions", req.Instructions); err != nil {
		return nil, err
	}
	tools := make([]string, 0, len(req.Tools))
	for _, tool := range req.Tools {
		encoded, err := json.Marshal(tool)
		if err != nil {
			return nil, fmt.Errorf("%w: responses tool marshal", ErrCursorResumeUnsupportedContent)
		}
		if err := plan.config.AppendItem("config.tool", encoded); err != nil {
			return nil, err
		}
		if name := strings.TrimSpace(tool.Name); name != "" {
			tools = append(tools, name)
		}
	}
	plan.tools = tools
	if err := builder.intPresence("config.max_output_tokens", req.MaxOutputTokens); err != nil {
		return nil, err
	}
	if err := builder.floatPresence("config.temperature", req.Temperature); err != nil {
		return nil, err
	}
	if err := builder.floatPresence("config.top_p", req.TopP); err != nil {
		return nil, err
	}
	if err := builder.boolPresence("config.parallel_tool_calls", req.ParallelToolCalls); err != nil {
		return nil, err
	}
	if req.Reasoning != nil {
		encoded, err := json.Marshal(req.Reasoning)
		if err != nil {
			return nil, fmt.Errorf("%w: responses reasoning config", ErrCursorResumeUnsupportedContent)
		}
		if err := plan.config.AppendItem("config.reasoning", encoded); err != nil {
			return nil, err
		}
	} else if err := builder.text("config.reasoning", "absent"); err != nil {
		return nil, err
	}
	if req.Text != nil {
		encoded, err := json.Marshal(req.Text)
		if err != nil {
			return nil, fmt.Errorf("%w: responses text config", ErrCursorResumeUnsupportedContent)
		}
		if err := plan.config.AppendItem("config.text", encoded); err != nil {
			return nil, err
		}
	} else if err := builder.text("config.text", "absent"); err != nil {
		return nil, err
	}
	plan.config = builder.digest()

	items, err := cursorResponsesInputItems(req.Input)
	if err != nil {
		return nil, err
	}
	// 只剥离**尾部连续**的 output 条目（当前批），前缀必须完整匹配。
	start := len(items)
	for start > 0 {
		if !cursorResponsesItemIsOutput(items[start-1]) {
			break
		}
		start--
	}
	for _, item := range items[start:] {
		callID := strings.TrimSpace(item.CallID)
		if callID == "" {
			return nil, fmt.Errorf("%w: responses tool output without call_id", ErrCursorResumeAmbiguousResults)
		}
		plan.results = append(plan.results, CursorToolResult{CallID: callID, Text: []string{item.Output}})
	}
	for _, item := range items[:start] {
		digestItem, ierr := cursorResponsesItemItem(item)
		if ierr != nil {
			return nil, ierr
		}
		if err := plan.history.AppendItem(digestItem.itemType, digestItem.canonical); err != nil {
			return nil, err
		}
	}
	return plan, nil
}

// cursorGeminiContent 是 Gemini 原生 contents 条目的最小类型化表示。
type cursorGeminiContent struct {
	Role  string                       `json:"role"`
	Parts []map[string]json.RawMessage `json:"parts"`
}

// cursorGeminiPartText 取 part 的文本（无 text 字段返回空）。
func cursorGeminiPartText(part map[string]json.RawMessage) (string, bool) {
	raw, ok := part["text"]
	if !ok {
		return "", false
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return "", false
	}
	return text, true
}

// cursorGeminiPartTool 读 functionCall / functionResponse 的 id / name / 负载。
func cursorGeminiPartTool(part map[string]json.RawMessage, key string) (id, name string, payload json.RawMessage, ok bool) {
	raw, exists := part[key]
	if !exists {
		return "", "", nil, false
	}
	var typed struct {
		ID       string          `json:"id"`
		Name     string          `json:"name"`
		Args     json.RawMessage `json:"args"`
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(raw, &typed); err != nil {
		return "", "", nil, false
	}
	payload = typed.Args
	if key == "functionResponse" {
		payload = typed.Response
	}
	return strings.TrimSpace(typed.ID), strings.TrimSpace(typed.Name), payload, true
}

// cursorPlanFromGemini 按 §25.14-D「Gemini」表示生成上下文计划。
//
// 规则：
//   - 按 contents 角色及 parts 顺序规范化；
//   - functionCall/functionResponse 保留 ID/名称/参数；
//   - 只将本批并行结果的容器差异规范化（多容器按 parts 顺序拼平），按 call ID 配对；
//   - 同名并行缺 ID 明确拒绝歧义（不凭名字重排历史）。
func cursorPlanFromGemini(body []byte, model string) (*cursorContextPlan, error) {
	var doc struct {
		SystemInstruction json.RawMessage       `json:"systemInstruction"`
		Contents          []cursorGeminiContent `json:"contents"`
		GenerationConfig  json.RawMessage       `json:"generationConfig"`
		Tools             json.RawMessage       `json:"tools"`
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, fmt.Errorf("%w: empty gemini request", ErrCursorResumeUnsupportedContent)
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("%w: undecodable gemini request", ErrCursorResumeUnsupportedContent)
	}
	builder := newCursorConfigBuilder()
	if err := builder.text("config.model", model); err != nil {
		return nil, err
	}
	plan := &cursorContextPlan{config: builder.digest(), history: NewCursorHistoryDigest()}
	if len(bytes.TrimSpace(doc.SystemInstruction)) > 0 {
		var sys struct {
			Parts []map[string]json.RawMessage `json:"parts"`
		}
		if err := json.Unmarshal(doc.SystemInstruction, &sys); err == nil {
			for _, part := range sys.Parts {
				if text, ok := cursorGeminiPartText(part); ok {
					if err := plan.config.AppendText("config.system", text); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	if err := builder.json("config.generation", doc.GenerationConfig); err != nil {
		return nil, err
	}
	tools := make([]string, 0, 8)
	if len(bytes.TrimSpace(doc.Tools)) > 0 {
		if err := plan.config.AppendJSON("config.tools", doc.Tools); err != nil {
			return nil, err
		}
		var declarations []struct {
			FunctionDeclarations []struct {
				Name string `json:"name"`
			} `json:"functionDeclarations"`
		}
		if err := json.Unmarshal(doc.Tools, &declarations); err == nil {
			for _, entry := range declarations {
				for _, decl := range entry.FunctionDeclarations {
					if name := strings.TrimSpace(decl.Name); name != "" {
						tools = append(tools, name)
					}
				}
			}
		}
	}
	plan.tools = tools
	plan.config = builder.digest()

	// 尾部连续"全部为 functionResponse 的 content" = 本批结果容器。
	start := len(doc.Contents)
	for start > 0 {
		content := doc.Contents[start-1]
		if len(content.Parts) == 0 {
			break
		}
		allResponses := true
		for _, part := range content.Parts {
			if _, _, _, ok := cursorGeminiPartTool(part, "functionResponse"); !ok {
				allResponses = false
				break
			}
		}
		if !allResponses {
			break
		}
		start--
	}
	// 精确配对所需的 model turn 函数调用（用于同名并行缺 ID 的歧义判定）。
	nameCount := make(map[string]int, 8)
	if start > 0 {
		for _, part := range doc.Contents[start-1].Parts {
			if _, name, _, ok := cursorGeminiPartTool(part, "functionCall"); ok && name != "" {
				nameCount[name]++
			}
		}
	}
	for _, content := range doc.Contents[start:] {
		batchNames := make(map[string]int, 4)
		for _, part := range content.Parts {
			id, name, _, ok := cursorGeminiPartTool(part, "functionResponse")
			if !ok {
				continue
			}
			batchNames[name]++
			if id == "" {
				// 缺 ID：只有当同名调用唯一时才能唯一配对，否则明确拒绝歧义。
				if batchNames[name] > 1 || nameCount[name] > 1 {
					return nil, fmt.Errorf("%w: gemini parallel %q", ErrCursorResumeAmbiguousResults, name)
				}
				return nil, fmt.Errorf("%w: gemini functionResponse without id", ErrCursorResumeAmbiguousResults)
			}
			plan.results = append(plan.results, CursorToolResult{CallID: id, Text: []string{string(part["functionResponse"])}})
		}
	}
	for _, content := range doc.Contents[:start] {
		role := strings.ToLower(strings.TrimSpace(content.Role))
		if role == "" {
			role = "user"
		}
		if role != "user" && role != "model" && role != "assistant" && role != "system" {
			return nil, fmt.Errorf("%w: gemini role %q", ErrCursorResumeUnsupportedContent, content.Role)
		}
		if role == "assistant" {
			role = "model"
		}
		for _, part := range content.Parts {
			if text, ok := cursorGeminiPartText(part); ok {
				if err := plan.history.AppendText("gemini."+role+".text", text); err != nil {
					return nil, err
				}
				continue
			}
			if id, name, args, ok := cursorGeminiPartTool(part, "functionCall"); ok {
				canonical, err := cursorToolCallCanonical(id, name, string(args))
				if err != nil {
					return nil, err
				}
				if err := plan.history.AppendItem("gemini."+role+".function_call", canonical); err != nil {
					return nil, err
				}
				continue
			}
			if id, name, payload, ok := cursorGeminiPartTool(part, "functionResponse"); ok {
				encoded, err := json.Marshal(map[string]string{"id": id, "name": name, "response": string(payload)})
				if err != nil {
					return nil, fmt.Errorf("%w: gemini functionResponse", ErrCursorResumeUnsupportedContent)
				}
				if err := plan.history.AppendItem("gemini."+role+".function_response", encoded); err != nil {
					return nil, err
				}
				continue
			}
			encoded, err := json.Marshal(part)
			if err != nil {
				return nil, fmt.Errorf("%w: gemini part", ErrCursorResumeUnsupportedContent)
			}
			if err := plan.history.AppendItem("gemini."+role+".part", encoded); err != nil {
				return nil, err
			}
		}
	}
	return plan, nil
}

// ---------------------------------------------------------------------------
// 续轮探针：从原始请求体识别"本次结果调用 ID"（§25.2）
// ---------------------------------------------------------------------------

// CursorResumeProbe 是一次续轮候选（只含协议、ID 与已算好的上下文计划）。
type CursorResumeProbe struct {
	// Protocol 是客户端协议名（也是索引命名空间）。
	Protocol CursorResumeProtocol
	// Model 是客户端可见模型名。
	Model string
	// Stream 是本次 HTTP 的流式开关（输出选项，不参与摘要）。
	Stream bool
	// ResultCallIDs 是**本批**结果调用 ID（交付顺序）。
	ResultCallIDs []string
	// PreviousResponseID 是 Responses 的显式续接声明（其它协议为空）。
	PreviousResponseID string
	// Plan 是按协议规则算好的上下文计划（用于摘要校验；不含正文副本语义）。
	Plan *cursorContextPlan
}

// ClaimsContinuation 表示本次请求明确声明了续接（有结果 ID 或 previous_response_id）。
func (p *CursorResumeProbe) ClaimsContinuation() bool {
	if p == nil {
		return false
	}
	return len(p.ResultCallIDs) > 0 || strings.TrimSpace(p.PreviousResponseID) != ""
}

// CursorProbeResumeRequest 按协议从原始请求体解出续轮探针。
//
// 只做一次解码并立刻返回类型化探针；不保存正文、不写会话。
func CursorProbeResumeRequest(protocol CursorResumeProtocol, body []byte) (*CursorResumeProbe, error) {
	switch protocol {
	case CursorResumeProtocolChat, CursorResumeProtocolGemini:
		if protocol == CursorResumeProtocolGemini {
			var meta struct {
				Model string `json:"model"`
			}
			_ = json.Unmarshal(body, &meta)
			plan, err := cursorPlanFromGemini(body, meta.Model)
			if err != nil {
				return nil, err
			}
			probe := &CursorResumeProbe{Protocol: protocol, Model: meta.Model, Plan: plan}
			for _, result := range plan.results {
				probe.ResultCallIDs = append(probe.ResultCallIDs, result.CallID)
			}
			return probe, nil
		}
		var req apicompat.ChatCompletionsRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, fmt.Errorf("cursor: parse chat completions request: %w", err)
		}
		plan, err := cursorPlanFromChat(&req, req.Model)
		if err != nil {
			return nil, err
		}
		probe := &CursorResumeProbe{Protocol: protocol, Model: req.Model, Stream: req.Stream, Plan: plan}
		for _, result := range plan.results {
			probe.ResultCallIDs = append(probe.ResultCallIDs, result.CallID)
		}
		return probe, nil
	case CursorResumeProtocolMessages:
		var req apicompat.AnthropicRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, fmt.Errorf("cursor: parse anthropic request: %w", err)
		}
		plan, err := cursorPlanFromMessages(&req, req.Model)
		if err != nil {
			return nil, err
		}
		probe := &CursorResumeProbe{Protocol: protocol, Model: req.Model, Stream: req.Stream, Plan: plan}
		for _, result := range plan.results {
			probe.ResultCallIDs = append(probe.ResultCallIDs, result.CallID)
		}
		return probe, nil
	case CursorResumeProtocolResponses:
		var req apicompat.ResponsesRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, fmt.Errorf("cursor: parse responses request: %w", err)
		}
		plan, err := cursorPlanFromResponses(&req, req.Model)
		if err != nil {
			return nil, err
		}
		probe := &CursorResumeProbe{
			Protocol:           protocol,
			Model:              req.Model,
			Stream:             req.Stream,
			Plan:               plan,
			PreviousResponseID: strings.TrimSpace(req.PreviousResponseID),
		}
		for _, result := range plan.results {
			probe.ResultCallIDs = append(probe.ResultCallIDs, result.CallID)
		}
		return probe, nil
	default:
		return nil, fmt.Errorf("cursor: unknown resume protocol %q", string(protocol))
	}
}

// ---------------------------------------------------------------------------
// 续轮定位：统一索引（§25.14-D「一份索引」）
// ---------------------------------------------------------------------------

// CursorResumeTicket 是靠统一索引定位到的续轮目标（只含定位结果，不含正文）。
type CursorResumeTicket struct {
	SessionID  string
	AccountID  int64
	QuotaKeyID int64
	Model      string
	Protocol   CursorResumeProtocol
	// BatchID 是当前待完成批次（model_call_id）。
	BatchID string
	// ResponseID 是该批对外的 Responses 响应 ID（其它协议为空）。
	ResponseID string
	// CallIDs 是该批已交付的调用 ID（交付顺序）。
	CallIDs []string
	// Identity 是会话所有者身份（续轮用它比对，不泄露给客户端）。
	Identity CursorSessionIdentity

	session *cursorSessionRecord
	probe   *CursorResumeProbe
}

// IdentifyResume 用统一索引把探针定位到唯一会话与唯一待完成批次。
//
// 失败关闭语义（§25.2 / §25.14-D）：
//   - 提交的 ID 全部未登记（含重启后孤儿）→ **(nil, nil)**：无法在账号选择前判定归属，
//     按原契约继续普通新请求；绝不猜最近会话、不建墓碑表、也不把别人的请求当成续轮；
//   - 只要**有任一 ID** 命中本服务的会话，则整批必须一致：分属不同会话（跨会话混批）、
//     重复 ID、旧批/旧 response、与 previous_response_id 不一致 → 明确 400。
//
// 这样既满足"已登记 ID 的混批/旧批/重复必须拒绝"，又不会把非同源请求（例如另一个
// 供应商的 function_call_output / previous_response_id）在平台选择之前误判成 Cursor 续轮。
func (s *CursorExecutionService) IdentifyResume(probe *CursorResumeProbe) (*CursorResumeTicket, error) {
	if s == nil {
		return nil, ErrCursorExecutionUnavailable
	}
	if !probe.ClaimsContinuation() {
		return nil, nil
	}
	protocol := string(probe.Protocol)

	// ① 先定位 previous_response_id（未知时先记下，等 call ID 定位结果一起判定）。
	prevID := strings.TrimSpace(probe.PreviousResponseID)
	var prevRecord *cursorSessionRecord
	var prevBatchID string
	if prevID != "" {
		if record := s.sessions.lookupSessionByProtocolName(protocol, CursorIDKindResponse, prevID); record != nil {
			record.mu.Lock()
			pending := record.pending
			currentResponseID := ""
			currentBatchID := ""
			if pending != nil {
				currentResponseID = pending.responseID
				currentBatchID = pending.batchID
			}
			record.mu.Unlock()
			if pending == nil || currentResponseID != prevID {
				// 已登记但不是当前批的 response：旧 response 明确拒绝。
				return nil, newCursorResumeError(http.StatusBadRequest, "stale_previous_response_id",
					fmt.Errorf("%w: previous_response_id %q", ErrCursorResumeStaleBatch, prevID))
			}
			prevRecord = record
			prevBatchID = currentBatchID
		}
	}

	// ② 定位本批结果调用 ID：全部未命中时按普通新请求处理（见函数注释）。
	var ticketSession = prevRecord
	unknownCallID := ""
	var duplicateCallID string
	seen := make(map[string]struct{}, len(probe.ResultCallIDs))
	locatedCount := 0
	for _, id := range probe.ResultCallIDs {
		callID := strings.TrimSpace(id)
		if callID == "" {
			unknownCallID = ""
			continue
		}
		if _, dup := seen[callID]; dup {
			if duplicateCallID == "" {
				duplicateCallID = callID
			}
			continue
		}
		seen[callID] = struct{}{}
		record := s.sessions.lookupSessionByProtocolName(protocol, CursorIDKindCall, callID)
		if record == nil {
			if unknownCallID == "" {
				unknownCallID = callID
			}
			continue
		}
		locatedCount++
		if ticketSession != nil && ticketSession != record {
			return nil, newCursorResumeError(http.StatusBadRequest, "mixed_sessions",
				fmt.Errorf("%w: call id %q", ErrCursorResumeMixedSessions, callID))
		}
		ticketSession = record
	}
	if locatedCount == 0 && prevRecord == nil {
		// 没有任何已登记关联：既不是本服务的续轮，也不该在这里判死。
		return nil, nil
	}
	if duplicateCallID != "" {
		return nil, newCursorResumeError(http.StatusBadRequest, "duplicate_result_id",
			fmt.Errorf("%w: %q", ErrCursorResumeDuplicateResults, duplicateCallID))
	}
	if unknownCallID != "" {
		return nil, newCursorResumeError(http.StatusBadRequest, "unknown_association",
			fmt.Errorf("%w: call id %q", ErrCursorResumeUnknownAssociation, unknownCallID))
	}
	if prevID != "" && prevRecord == nil && locatedCount > 0 {
		// 结果 ID 是本服务的会话，但 previous_response_id 不是：明确拒绝，不猜。
		return nil, newCursorResumeError(http.StatusBadRequest, "unknown_previous_response_id",
			fmt.Errorf("%w: previous_response_id", ErrCursorResumeUnknownAssociation))
	}
	if ticketSession == nil {
		return nil, nil
	}

	ticketSession.mu.Lock()
	pending := ticketSession.pending
	spec := ticketSession.spec
	identity := ticketSession.identity
	var pendingBatchID string
	var pendingResponseID string
	var pendingCallIDs []string
	if pending != nil {
		pendingBatchID = pending.batchID
		pendingResponseID = pending.responseID
		pendingCallIDs = pending.cloneCallIDs()
	}
	ticketSession.mu.Unlock()

	if pending == nil {
		return nil, newCursorResumeError(http.StatusBadRequest, "no_pending_batch",
			fmt.Errorf("%w: session %q", ErrCursorResumeStaleBatch, spec.requestedModel))
	}
	if prevBatchID != "" && prevBatchID != pendingBatchID {
		return nil, newCursorResumeError(http.StatusBadRequest, "stale_batch",
			fmt.Errorf("%w: previous_response_id batch", ErrCursorResumeStaleBatch))
	}
	// ID 集合必须落在当前待完成批内（旧批 ID 明确拒绝，不猜）。
	for callID := range seen {
		found := false
		for _, pendingID := range pendingCallIDs {
			if pendingID == callID {
				found = true
				break
			}
		}
		if !found {
			return nil, newCursorResumeError(http.StatusBadRequest, "stale_call_id",
				fmt.Errorf("%w: call id %q", ErrCursorResumeStaleBatch, callID))
		}
	}
	return &CursorResumeTicket{
		SessionID:  ticketSession.id,
		AccountID:  spec.accountID,
		QuotaKeyID: spec.quotaKeyID,
		Model:      spec.requestedModel,
		Protocol:   probe.Protocol,
		BatchID:    pendingBatchID,
		ResponseID: pendingResponseID,
		CallIDs:    pendingCallIDs,
		Identity:   identity,
		session:    ticketSession,
		probe:      probe,
	}, nil
}

// ---------------------------------------------------------------------------
// 整批接管（§25.14-D「整批接管顺序」）
// ---------------------------------------------------------------------------

// CursorResumeServeInput 是续轮 HTTP 的输入。
//
// Body 只在本层解码一次用于摘要校验；不保存、不写会话、不落日志。
type CursorResumeServeInput struct {
	Body     []byte
	Stream   bool
	Identity CursorSessionIdentity
}

// beginResumeTakeover 在**同一服务锁**内判定唯一胜者并标记当前批接管。
//
// 会话表、统一索引与所有权切换共用这把锁（不另加索引锁/会话锁，避免反向锁序）；
// 锁外才会做网络/摘要/写入。第二个正确的并发请求在这里被明确拒绝为 409。
func (s *cursorSessionStore) beginResumeTakeover(
	ticket *CursorResumeTicket, httpCtx context.Context,
) (func(), error) {
	if s == nil || ticket == nil {
		return nil, newCursorResumeError(http.StatusBadRequest, "invalid_ticket", ErrCursorResumeUnknownAssociation)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.sessions[ticket.SessionID]
	if !ok {
		// 无任何已登记关联（含重启后孤儿）：明确 400，不留墓碑表。
		return nil, newCursorResumeError(http.StatusBadRequest, "unknown_association", ErrCursorResumeUnknownAssociation)
	}
	record.mu.Lock()
	defer record.mu.Unlock()
	if record.finishing || record.state == cursorSessionClosed {
		return nil, newCursorResumeError(http.StatusBadRequest, "session_finished", ErrCursorResumeNotWaiting)
	}
	// "已被消费"先于状态判定：并发正确请求的后者必须是明确 409，而不是状态错误。
	if record.pending != nil && record.pending.consumed {
		return nil, newCursorResumeError(http.StatusConflict, "already_consumed", ErrCursorResumeAlreadyConsumed)
	}
	if record.attached {
		return nil, newCursorResumeError(http.StatusConflict, "session_busy", ErrCursorResumeSessionBusy)
	}
	if record.state != cursorSessionWaitingTools {
		return nil, newCursorResumeError(http.StatusBadRequest, "not_waiting_tools", ErrCursorResumeNotWaiting)
	}
	if record.pending == nil || record.pending.batchID != ticket.BatchID {
		return nil, newCursorResumeError(http.StatusBadRequest, "stale_batch", ErrCursorResumeStaleBatch)
	}
	if !record.pending.delivered {
		return nil, newCursorResumeError(http.StatusBadRequest, "batch_not_delivered", ErrCursorResumeBatchNotDelivered)
	}
	for _, callID := range ticket.CallIDs {
		if _, err := s.index.MarkConsumed(cursorSessionIndexKeyForSpec(record.spec, CursorIDKindCall, callID)); err != nil {
			if errors.Is(err, ErrCursorSessionIndexAlreadyConsumed) {
				return nil, newCursorResumeError(http.StatusConflict, "already_consumed", err)
			}
			return nil, newCursorResumeError(http.StatusBadRequest, "unknown_association", err)
		}
	}
	record.pending.consumed = true
	record.state = cursorSessionRunning
	record.resumeCount++
	record.attached = true
	if httpCtx != nil {
		record.httpAttachStop = context.AfterFunc(httpCtx, record.onHTTPAttachCancel)
	}
	// detach 与 attachHTTP 同型：锁内置位、停回调，正常返回不误杀会话。
	var once sync.Once
	detach := func() {
		once.Do(func() {
			record.mu.Lock()
			stop := record.httpAttachStop
			record.httpAttachStop = nil
			record.attached = false
			record.mu.Unlock()
			if stop != nil {
				_ = stop()
			}
		})
	}
	return detach, nil
}

// ResumeCursorSession 是续轮的唯一执行入口（鉴权/所有权/摘要/接管/写入顺序固定）。
//
// 调用前提：调用方已完成正常鉴权、用户/key 有效性、组权限、模型/路由准入，并用
// ticket.AccountID 取回原账号（本层再复核平台与账号归属）。
func (s *CursorExecutionService) ResumeCursorSession(
	ctx context.Context,
	c *gin.Context,
	ticket *CursorResumeTicket,
	account *Account,
	in CursorResumeServeInput,
) (*ForwardResult, error) {
	if s == nil {
		return nil, ErrCursorExecutionUnavailable
	}
	if ticket == nil || ticket.session == nil {
		return nil, newCursorResumeError(http.StatusBadRequest, "invalid_ticket", ErrCursorResumeUnknownAssociation)
	}
	record := ticket.session

	// ① 身份 / 账号 / 协议 / 模型：任一不符即失败关闭（不泄露是否存在别人的会话）。
	if !ticket.Identity.compatibleWith(in.Identity) {
		return nil, newCursorResumeError(http.StatusForbidden, "identity_mismatch", ErrCursorResumeIdentityMismatch)
	}
	if account == nil || !isCursorAccount(account) {
		return nil, newCursorResumeError(http.StatusBadRequest, "account_unavailable", ErrCursorResumeStaleBatch)
	}
	if ticket.AccountID != 0 && account.ID != ticket.AccountID {
		// 续轮不换号：原账号失效即明确失败，不重选、不重执行工具。
		return nil, newCursorResumeError(http.StatusBadRequest, "account_changed", ErrCursorResumeStaleBatch)
	}

	record.mu.Lock()
	spec := record.spec
	// 摘要快照必须在锁内取（并发续轮时胜者会推进历史链）：比较的是**快照**，
	// 不是可被并发改写的活指针。
	storedConfig := record.configDigest.Clone()
	storedHistory := record.historyDigest.Clone()
	exposed := append([]string(nil), record.exposedTools...)
	instructionsDeclared := record.instructionsDeclared
	intake := record.intake
	state := record.state
	record.mu.Unlock()

	if state != cursorSessionWaitingTools && state != cursorSessionRunning {
		return nil, newCursorResumeError(http.StatusBadRequest, "not_waiting_tools", ErrCursorResumeNotWaiting)
	}
	if ticket.Protocol != "" && cursorResumeProtocolOfSpec(spec) != ticket.Protocol {
		return nil, newCursorResumeError(http.StatusBadRequest, "protocol_mismatch", ErrCursorResumeStaleBatch)
	}
	if strings.TrimSpace(ticket.Model) != "" && strings.TrimSpace(spec.requestedModel) != strings.TrimSpace(ticket.Model) {
		return nil, newCursorResumeError(http.StatusBadRequest, "model_changed", ErrCursorResumeConfigChanged)
	}
	// 已被消费的批**先于**摘要校验返回 409：并发重复提交不得因会话历史已推进而报成
	// 入参错误，也不得重放（§25.2「重复提交已经消费的结果返回冲突」）。
	record.mu.Lock()
	alreadyConsumed := record.pending != nil && record.pending.consumed
	record.mu.Unlock()
	if alreadyConsumed {
		return nil, newCursorResumeError(http.StatusConflict, "already_consumed", ErrCursorResumeAlreadyConsumed)
	}

	// ② 配置 / 前缀摘要 / 本批结果完整校验（锁外；失败零上游写入、不改状态）。
	bodyModel, modelErr := cursorRequestModel(ticket.Protocol, in.Body)
	if modelErr != nil {
		return nil, newCursorResumeError(http.StatusBadRequest, "undecodable_continuation", modelErr)
	}
	if strings.TrimSpace(bodyModel) != "" && strings.TrimSpace(bodyModel) != strings.TrimSpace(spec.requestedModel) {
		return nil, newCursorResumeError(http.StatusBadRequest, "model_changed", ErrCursorResumeConfigChanged)
	}
	plan, planErr := cursorResumePlan(ticket.Protocol, in, bodyModel)
	if planErr != nil {
		return nil, newCursorResumeError(http.StatusBadRequest, "undecodable_continuation", planErr)
	}
	if instructionsDeclared && !plan.instructions {
		// 首轮非空 instructions + 续轮省略 = 明确入参错误（不偷偷继承）。
		return nil, newCursorResumeError(http.StatusBadRequest, "instructions_withdrawn", ErrCursorResumeInstructionsWithdrawn)
	}
	if storedConfig != nil && !storedConfig.Equal(plan.config) {
		return nil, newCursorResumeError(http.StatusBadRequest, "config_changed", ErrCursorResumeConfigChanged)
	}
	if !cursorToolSetEqual(exposed, plan.tools) {
		return nil, newCursorResumeError(http.StatusBadRequest, "tool_set_changed", ErrCursorResumeToolSetChanged)
	}
	// previous_response_id 精确指向当前批时可**仅携带本批结果**省略历史；
	// 一旦提供重复历史就必须整段匹配（不把任意短前缀当完整历史）。
	historyOmitted := strings.TrimSpace(ticket.ResponseID) != "" && plan.history.ItemCount() == 0
	if !historyOmitted && storedHistory != nil && !storedHistory.Equal(plan.history) {
		// 并发窗口：胜者可能刚推进了历史并消费了本批 —— 后者必须是 409 而不是入参错误。
		if record.batchConsumed() {
			return nil, newCursorResumeError(http.StatusConflict, "already_consumed", ErrCursorResumeAlreadyConsumed)
		}
		return nil, newCursorResumeError(http.StatusBadRequest, "history_mismatch", ErrCursorResumeHistoryMismatch)
	}
	if err := cursorValidateResumeResults(ticket.CallIDs, plan.results); err != nil {
		if record.batchConsumed() {
			return nil, newCursorResumeError(http.StatusConflict, "already_consumed", ErrCursorResumeAlreadyConsumed)
		}
		return nil, err
	}
	if err := cursorValidateToolChoiceForSession(ticket.Protocol, in.Body, exposed); err != nil {
		return nil, err
	}
	if intake == nil {
		// 回写通道未接通：失败关闭、零上游写入，绝不伪造成功。
		return nil, newCursorResumeError(http.StatusServiceUnavailable, "intake_unavailable", ErrCursorResumeIntakeUnavailable)
	}

	// ③ 服务锁内接管：唯一胜者；后者 409。
	detach, takeoverErr := s.sessions.beginResumeTakeover(ticket, ctx)
	if takeoverErr != nil {
		return nil, takeoverErr
	}
	defer detach()

	// 续轮 drain（跨 HTTP 存活会话）：先附着输出通道，确保真实结果写回后恢复的输出帧
	// 自本 HTTP 附着起被完整捕获（不丢失首帧），再串行写回真实结果。无真实 pump
	// （测试 stub / 已终态会话 / c 为空）时 ch 为 nil，走既有返回契约，不破坏 W2-E。
	var drainCh chan *cursorOutputEvent
	var drainDetach func()
	if record.output != nil && c != nil {
		drainCh, drainDetach = record.output.attach()
	}
	if drainCh != nil {
		defer drainDetach()
	}

	// ④ 锁外串行写真实结果（整批一次；失败即终止，不回滚到可重放）。
	started := time.Now()
	if err := intake.DeliverToolResults(ctx, ticket.SessionID, ticket.BatchID, plan.results); err != nil {
		record.noteStage("resume_write_failed")
		s.finishCursorSession(record, "resume_write_failed")
		return nil, fmt.Errorf("cursor: deliver tool results: %w", err)
	}
	record.noteStage("resume_write_done")
	record.mu.Lock()
	if err := appendCursorDeliveredResultsToHistory(record.historyDigest, ticket.Protocol, plan.results); err != nil {
		record.mu.Unlock()
		s.finishCursorSession(record, "resume_history_failed")
		return nil, fmt.Errorf("cursor: append continuation history: %w", err)
	}
	record.mu.Unlock()
	cursorPhaseLog(ctx, zapcore.InfoLevel, "session_resume_accepted", spec.requestedModel,
		zap.String("session_id", record.id),
		zap.String("batch_id", ticket.BatchID),
		zap.Int("result_count", len(plan.results)),
		zap.Bool("stream", in.Stream))

	if drainCh != nil {
		// 真实跨 HTTP 会话：把恢复后的输出帧写出给当前 HTTP（自本附着起），直到真实终态。
		drainReq := &CursorExecutionRequest{
			Protocol: record.spec.protocol,
			Stream:   in.Stream,
			Model:    ticket.Model,
		}
		writer, restore, werr := s.beginResponseWriter(c, drainReq, nil)
		if werr != nil {
			return nil, werr
		}
		if restore != nil {
			defer restore()
		}
		return s.cursorDrainAttachedOutput(drainCh, writer, drainReq, record)
	}

	return &ForwardResult{
		Model:         ticket.Model,
		UpstreamModel: spec.upstreamModel,
		Stream:        in.Stream,
		Duration:      time.Since(started),
	}, nil
}

// cursorResumePlan 按协议重算续轮上下文计划（解码一次，不留正文）。
func cursorResumePlan(protocol CursorResumeProtocol, in CursorResumeServeInput, model string) (*cursorContextPlan, error) {
	switch protocol {
	case CursorResumeProtocolChat:
		var req apicompat.ChatCompletionsRequest
		if err := json.Unmarshal(in.Body, &req); err != nil {
			return nil, fmt.Errorf("%w: parse chat: %v", ErrCursorResumeUnsupportedContent, err)
		}
		return cursorPlanFromChat(&req, model)
	case CursorResumeProtocolMessages:
		var req apicompat.AnthropicRequest
		if err := json.Unmarshal(in.Body, &req); err != nil {
			return nil, fmt.Errorf("%w: parse messages: %v", ErrCursorResumeUnsupportedContent, err)
		}
		return cursorPlanFromMessages(&req, model)
	case CursorResumeProtocolResponses:
		var req apicompat.ResponsesRequest
		if err := json.Unmarshal(in.Body, &req); err != nil {
			return nil, fmt.Errorf("%w: parse responses: %v", ErrCursorResumeUnsupportedContent, err)
		}
		return cursorPlanFromResponses(&req, model)
	case CursorResumeProtocolGemini:
		return cursorPlanFromGemini(in.Body, model)
	default:
		return nil, fmt.Errorf("%w: unknown protocol %q", ErrCursorResumeUnsupportedContent, string(protocol))
	}
}

// cursorValidateResumeResults 校验本批结果集合与已交付调用集合完全一致。
//
// 缺失 → 明确失败；多出未交付的 ID → 明确失败；重复 → 明确失败（不部分消费）。
func cursorValidateResumeResults(delivered []string, results []CursorToolResult) error {
	if len(delivered) == 0 {
		return newCursorResumeError(http.StatusBadRequest, "no_delivered_batch", ErrCursorResumeStaleBatch)
	}
	if len(results) == 0 {
		return newCursorResumeError(http.StatusBadRequest, "missing_results", ErrCursorResumeBatchIncomplete)
	}
	seen := make(map[string]struct{}, len(results))
	for _, result := range results {
		callID := strings.TrimSpace(result.CallID)
		if _, dup := seen[callID]; dup {
			return newCursorResumeError(http.StatusBadRequest, "duplicate_result_id", ErrCursorResumeDuplicateResults)
		}
		seen[callID] = struct{}{}
		matched := false
		for _, deliveredID := range delivered {
			if deliveredID == callID {
				matched = true
				break
			}
		}
		if !matched {
			return newCursorResumeError(http.StatusBadRequest, "unexpected_result_id", ErrCursorResumeStaleBatch)
		}
	}
	if len(seen) != len(delivered) {
		return newCursorResumeError(http.StatusBadRequest, "missing_results", ErrCursorResumeBatchIncomplete)
	}
	return nil
}

// cursorToolSetEqual 判断两个工具名集合是否一致（顺序不参与比较，集合变化即不同）。
func cursorToolSetEqual(a, b []string) bool {
	setA := make(map[string]struct{}, len(a))
	for _, name := range a {
		setA[name] = struct{}{}
	}
	setB := make(map[string]struct{}, len(b))
	for _, name := range b {
		setB[name] = struct{}{}
	}
	if len(setA) != len(setB) {
		return false
	}
	for name := range setA {
		if _, ok := setB[name]; !ok {
			return false
		}
	}
	return true
}

// appendCursorDeliveredResultsToHistory 把本次接纳的真实结果推进已交付历史。
//
// 表示与各协议历史规范化**同源**（下次续轮重算前缀才能与这里对齐）。
func appendCursorDeliveredResultsToHistory(d *CursorHistoryDigest, protocol CursorResumeProtocol, results []CursorToolResult) error {
	if d == nil {
		return nil
	}
	for _, result := range results {
		switch protocol {
		case CursorResumeProtocolChat:
			if err := d.AppendText("chat.tool_result", strings.Join(result.Text, "")); err != nil {
				return err
			}
		case CursorResumeProtocolMessages:
			payload := map[string]json.RawMessage{
				"tool_use_id": mustCursorJSON(result.CallID),
				"content":     mustCursorJSON(strings.Join(result.Text, "")),
				"is_error":    mustCursorJSON(fmt.Sprintf("%t", result.IsError)),
			}
			if err := d.AppendItem("messages.tool_result", mustCursorJSONValue(payload)); err != nil {
				return err
			}
		case CursorResumeProtocolResponses:
			item := apicompat.ResponsesInputItem{
				Type:   "function_call_output",
				CallID: result.CallID,
				Output: strings.Join(result.Text, ""),
			}
			digestItem, err := cursorResponsesItemItem(item)
			if err != nil {
				return err
			}
			if err := d.AppendItem(digestItem.itemType, digestItem.canonical); err != nil {
				return err
			}
		case CursorResumeProtocolGemini:
			payload, err := json.Marshal(map[string]string{
				"id":       result.CallID,
				"response": strings.Join(result.Text, ""),
			})
			if err != nil {
				return err
			}
			if err := d.AppendItem("gemini.user.function_response", payload); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: unknown protocol %q", ErrCursorResumeUnsupportedContent, string(protocol))
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// tool_choice 逐 HTTP 校验（§25.14-D）
// ---------------------------------------------------------------------------

// cursorToolChoiceMode 解析原始 tool_choice 为 (mode, name)。
//
// mode ∈ {"", "auto", "none", "required", "named"}；无法解析即返回错误（不静默忽略）。
func cursorToolChoiceMode(raw json.RawMessage) (string, string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "", "", nil
	}
	var asString string
	if err := json.Unmarshal(trimmed, &asString); err == nil {
		switch strings.ToLower(strings.TrimSpace(asString)) {
		case "":
			return "", "", nil
		case "auto", "none", "required":
			return strings.ToLower(strings.TrimSpace(asString)), "", nil
		default:
			return "", "", fmt.Errorf("%w: unknown tool_choice %q", ErrCursorToolChoiceInvalid, asString)
		}
	}
	var named struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(trimmed, &named); err != nil {
		return "", "", fmt.Errorf("%w: undecodable tool_choice: %v", ErrCursorToolChoiceInvalid, err)
	}
	name := strings.TrimSpace(named.Function.Name)
	if name == "" {
		name = strings.TrimSpace(named.Name)
	}
	if name == "" {
		// 对象形式的 auto/none 等价于字符串形式。
		switch strings.ToLower(strings.TrimSpace(named.Type)) {
		case "", "auto", "none":
			return strings.ToLower(strings.TrimSpace(named.Type)), "", nil
		}
		return "", "", fmt.Errorf("%w: tool_choice carries no tool name", ErrCursorToolChoiceInvalid)
	}
	return "named", name, nil
}

// cursorValidateToolChoiceForSession 按会话已暴露工具集合做**逐 HTTP** 校验。
//
// 规则（§25.14-D）：
//   - required / auto / none 可在**不改变已暴露工具集合**时切换；
//   - 具名选择必须保持暴露集合一致，不能从一个工具静默扩展为全部工具；
//   - 改变集合的续轮明确拒绝。
//
// required 不跨 HTTP 永久套用：本函数只看本次 HTTP 的选择。
func cursorValidateToolChoiceForSession(
	protocol CursorResumeProtocol, body []byte, exposed []string,
) error {
	var rawChoice json.RawMessage
	switch protocol {
	case CursorResumeProtocolChat:
		var req apicompat.ChatCompletionsRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return newCursorResumeError(http.StatusBadRequest, "undecodable_continuation", ErrCursorResumeUnsupportedContent)
		}
		rawChoice = req.ToolChoice
		if !cursorToolSetEqual(exposed, cursorChatToolNames(&req)) {
			return newCursorResumeError(http.StatusBadRequest, "tool_set_changed", ErrCursorResumeToolSetChanged)
		}
	case CursorResumeProtocolMessages:
		var req apicompat.AnthropicRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return newCursorResumeError(http.StatusBadRequest, "undecodable_continuation", ErrCursorResumeUnsupportedContent)
		}
		rawChoice = req.ToolChoice
		names := make([]string, 0, len(req.Tools))
		for _, tool := range req.Tools {
			if name := strings.TrimSpace(tool.Name); name != "" {
				names = append(names, name)
			}
		}
		if !cursorToolSetEqual(exposed, names) {
			return newCursorResumeError(http.StatusBadRequest, "tool_set_changed", ErrCursorResumeToolSetChanged)
		}
	case CursorResumeProtocolResponses:
		var req apicompat.ResponsesRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return newCursorResumeError(http.StatusBadRequest, "undecodable_continuation", ErrCursorResumeUnsupportedContent)
		}
		rawChoice = req.ToolChoice
		names := make([]string, 0, len(req.Tools))
		for _, tool := range req.Tools {
			if name := strings.TrimSpace(tool.Name); name != "" {
				names = append(names, name)
			}
		}
		if !cursorToolSetEqual(exposed, names) {
			return newCursorResumeError(http.StatusBadRequest, "tool_set_changed", ErrCursorResumeToolSetChanged)
		}
	case CursorResumeProtocolGemini:
		// Gemini 原生没有 tool_choice 等价物：集合一致即可（已在摘要阶段校验）。
		return nil
	default:
		return newCursorResumeError(http.StatusBadRequest, "unknown_protocol", ErrCursorResumeUnsupportedContent)
	}
	mode, name, err := cursorToolChoiceMode(rawChoice)
	if err != nil {
		return newCursorResumeError(http.StatusBadRequest, "tool_choice_invalid", err)
	}
	if mode == "required" && len(exposed) == 0 {
		return newCursorResumeError(http.StatusBadRequest, "tool_choice_required_without_tools",
			fmt.Errorf("%w: required but no tools declared", ErrCursorToolChoiceInvalid))
	}
	if mode == "named" {
		for _, exposedName := range exposed {
			if exposedName == name {
				return nil
			}
		}
		return newCursorResumeError(http.StatusBadRequest, "tool_choice_undeclared",
			fmt.Errorf("%w: names unexposed tool %q", ErrCursorToolChoiceInvalid, name))
	}
	return nil
}

// cursorChatToolNames 返回 Chat 请求声明的工具名（保持声明顺序）。
func cursorChatToolNames(req *apicompat.ChatCompletionsRequest) []string {
	if req == nil {
		return nil
	}
	names := make([]string, 0, len(req.Tools))
	for _, tool := range req.Tools {
		if tool.Function == nil {
			continue
		}
		if name := strings.TrimSpace(tool.Function.Name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// cursorEnforceToolChoiceOnOutput 在**真实输出处**校验本 HTTP 的工具选择约束。
//
// 续轮（或首轮）仍指定 required/具名而本轮真实返回纯文本 → 明确契约失败，
// 绝不伪造调用、也绝不把普通文本当满足。
func cursorEnforceToolChoiceOnOutput(mode, name string, calls []cursorToolCallCapture) error {
	switch mode {
	case "required":
		if len(calls) == 0 {
			return fmt.Errorf("%w: required tool_choice was not satisfied by the real output", ErrCursorToolChoiceInvalid)
		}
	case "named":
		for _, call := range calls {
			if call.Name == name {
				return nil
			}
		}
		return fmt.Errorf("%w: named tool_choice %q was not satisfied by the real output", ErrCursorToolChoiceInvalid, name)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 交付登记：把"已交付的工具批次"写进会话与统一索引（首 HTTP 的 §25.14-D 落点）
// ---------------------------------------------------------------------------

// cursorDeliveredBatchSummary 是一个已封口批次的可登记快照。
type cursorDeliveredBatchSummary struct {
	ModelCallID string
	CallIDs     []string
}

// sealedBatchSummaries 返回已封口批次的 (model_call_id, 对外调用 ID 顺序) 快照。
func (d *cursorToolDelegations) sealedBatchSummaries() []cursorDeliveredBatchSummary {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]cursorDeliveredBatchSummary, 0, len(d.all))
	for _, batch := range d.all {
		if batch == nil || !batch.sealed {
			continue
		}
		summary := cursorDeliveredBatchSummary{ModelCallID: batch.modelCallID}
		for _, capture := range batch.delegations {
			if capture.ID == "" {
				continue
			}
			summary.CallIDs = append(summary.CallIDs, capture.ID)
		}
		if len(summary.CallIDs) == 0 {
			continue
		}
		out = append(out, summary)
	}
	return out
}

// recordPlan 把首次 HTTP 的固定配置摘要、已交付历史摘要、暴露工具集合与身份写入会话。
//
// 只存类型化摘要与项数（CR-B 引擎状态）；不存正文/完整历史。
func (r *cursorSessionRecord) recordPlan(identity CursorSessionIdentity, plan *cursorContextPlan) {
	if r == nil || plan == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.identity = identity
	r.configDigest = plan.config
	if plan.history != nil {
		r.historyDigest = plan.history.Clone()
	}
	r.exposedTools = append([]string(nil), plan.tools...)
	r.instructionsDeclared = plan.instructions
}

// installIntake 安装真实结果回写通道（由原生双工持有方在交付批次时安装）。
func (s *CursorExecutionService) installIntake(sessionID string, intake CursorSessionIntake) error {
	if s == nil {
		return ErrCursorExecutionUnavailable
	}
	s.sessions.mu.Lock()
	record := s.sessions.sessions[sessionID]
	s.sessions.mu.Unlock()
	if record == nil {
		return fmt.Errorf("%w: session %q is not active", ErrCursorSessionIndexInvalidKey, sessionID)
	}
	record.mu.Lock()
	record.intake = intake
	record.mu.Unlock()
	return nil
}

// cursorAgentRunIntake 是 CursorSessionIntake 的生产实现（W2-F / §24.3）。
//
// 把整批校验后的真实结果按 §25.3 四层编码写回原 AgentRun 的 exec 关联（经由原生双工
// writer，串行写、writeMu 保护）。is_error 走 McpResult.f2（错误语义，绝不把失败写成成功）；
// 结构化 JSON 走 McpSuccess.f2（structured_content 实际类型）。发送中传输失败 → 关闭会话
// （finalizer），禁止重连重放结果。
type cursorAgentRunIntake struct {
	svc         *CursorExecutionService
	session     *cursorSessionRecord
	duplex      *cursorAgentDuplex
	delegations *cursorToolDelegations
}

// DeliverToolResults 实现 CursorSessionIntake：整批一次、锁外串行写回每条真实结果。
func (it *cursorAgentRunIntake) DeliverToolResults(ctx context.Context, sessionID, batchID string, results []CursorToolResult) error {
	if it == nil || it.svc == nil || it.session == nil || it.duplex == nil {
		return ErrCursorResumeIntakeUnavailable
	}
	caps := it.delegations.snapshot()
	for _, r := range results {
		var cap cursorToolCallCapture
		found := false
		for _, c := range caps {
			if c.ID == r.CallID {
				cap = c
				found = true
				break
			}
		}
		if !found {
			// 提交的结果 ID 无任何已登记关联：失败关闭、零上游写入（不伪造）。
			it.svc.finishCursorSession(it.session, "resume_unknown_call")
			return fmt.Errorf("%w: call id %q", ErrCursorResumeUnknownAssociation, r.CallID)
		}
		frame := cursorEncodeMcpToolResult(cap.ExecID, cap.IDField, r.CallID, r.Text, r.Structured, r.IsError)
		if err := it.duplex.send(cursorFldClientMessage, frame); err != nil {
			// 发送中传输失败 → 关闭会话（finalizer），禁止重连重放。
			it.svc.finishCursorSession(it.session, "resume_write_transport_failure")
			return fmt.Errorf("cursor: deliver tool result transport failure: %w", err)
		}
		it.session.noteStage("resume_result_written")
	}
	return nil
}

// recordDeliveredBatch 把**已完整写出**的工具批次登记为待完成批（§25.3 / §25.14-D）。
//
// 只有整批内容完整写出才进入 waiting_tools；部分写出由 §25.4 终止，不记成已交付。
func (s *CursorExecutionService) recordDeliveredBatch(
	record *cursorSessionRecord,
	req *CursorExecutionRequest,
	delegations *cursorToolDelegations,
	responseID string,
) error {
	if s == nil || record == nil || req == nil {
		return nil
	}
	batches := delegations.sealedBatchSummaries()
	if len(batches) == 0 {
		return nil
	}
	current := batches[len(batches)-1]
	batchID := strings.TrimSpace(current.ModelCallID)
	if batchID == "" {
		batchID = cursorSessionLevelBatchID
	}
	for _, callID := range current.CallIDs {
		if err := s.sessions.registerSessionID(record.id, batchID, CursorIDKindCall, callID); err != nil {
			return err
		}
	}
	if strings.TrimSpace(responseID) != "" {
		if err := s.sessions.registerSessionID(record.id, batchID, CursorIDKindResponse, strings.TrimSpace(responseID)); err != nil {
			return err
		}
	}
	// 已交付的 assistant 输出并入历史：只追加**实际交付的逻辑输出**（类型/工具名/参数/公开 ID）。
	record.mu.Lock()
	if record.historyDigest != nil {
		firstCall := true
		for _, call := range current.CallIDs {
			var name, args string
			for _, candidate := range delegations.snapshot() {
				if candidate.ID == call {
					name, args = candidate.Name, candidate.ArgsJSON
				}
			}
			switch cursorResumeProtocolOfRequest(req) {
			case CursorResumeProtocolChat:
				canonical, err := cursorToolCallCanonical(call, name, args)
				if err != nil {
					record.mu.Unlock()
					return err
				}
				// assistant 的空 content 与省略统一为空文本：**每条 assistant 消息**一次文本项，
				// 其后按顺序追加该消息的 tool_call 项（与 Chat 历史规范化逐项对齐）。
				if firstCall {
					if err := record.historyDigest.AppendText("chat.assistant.text", ""); err != nil {
						record.mu.Unlock()
						return err
					}
					firstCall = false
				}
				if err := record.historyDigest.AppendItem("chat.assistant.tool_call", canonical); err != nil {
					record.mu.Unlock()
					return err
				}
			case CursorResumeProtocolMessages:
				payload := map[string]json.RawMessage{
					"id":    mustCursorJSON(call),
					"name":  mustCursorJSON(name),
					"input": CursorCanonicalText(args),
				}
				if err := record.historyDigest.AppendItem("messages.assistant.tool_use", mustCursorJSONValue(payload)); err != nil {
					record.mu.Unlock()
					return err
				}
			case CursorResumeProtocolResponses:
				canonical, err := cursorToolCallCanonical(call, name, args)
				if err != nil {
					record.mu.Unlock()
					return err
				}
				if err := record.historyDigest.AppendItem("responses.function_call", canonical); err != nil {
					record.mu.Unlock()
					return err
				}
			}
		}
	}
	record.pending = &cursorPendingBatch{
		batchID:    batchID,
		responseID: strings.TrimSpace(responseID),
		callIDs:    append([]string(nil), current.CallIDs...),
		delivered:  true,
	}
	record.state = cursorSessionWaitingTools
	record.mu.Unlock()
	cursorPhaseLog(context.Background(), zapcore.InfoLevel, "session_batch_delivered", req.Model,
		zap.String("session_id", record.id),
		zap.String("batch_id", batchID),
		zap.Int("call_count", len(current.CallIDs)),
		zap.Bool("has_response_id", strings.TrimSpace(responseID) != ""))
	return nil
}

// cursorWriterResponseID 取 Responses 回桥 writer 真实写出的 response ID（其它协议为空）。
func cursorWriterResponseID(w io.Writer) string {
	converted, ok := w.(*cursorProtocolWriter)
	if !ok || converted == nil || converted.respState == nil {
		return ""
	}
	return strings.TrimSpace(converted.respState.ResponseID)
}

// ---------------------------------------------------------------------------
// handler 侧的续轮入口（GatewayService / OpenAIGatewayService 共用同一实现）
// ---------------------------------------------------------------------------

// CursorResumeGateway 是 handler 侧唯一可用的续轮入口。
//
// 两个网关服务都以**同一实现**（同一 CursorExecutionService 实例）满足它，
// 不存在第二套续轮识别/校验/接管链。
type CursorResumeGateway interface {
	// IdentifyCursorResume 在资源获取前把探针定位到唯一会话与唯一待完成批。
	IdentifyCursorResume(probe *CursorResumeProbe) (*CursorResumeTicket, error)
	// ServeCursorResume 完成身份/账号/摘要校验、整批接管与真实结果写入。
	ServeCursorResume(ctx context.Context, c *gin.Context, ticket *CursorResumeTicket, in CursorResumeServeInput) (*ForwardResult, error)
}

// IdentifyCursorResume 实现 CursorResumeGateway。
func (s *GatewayService) IdentifyCursorResume(probe *CursorResumeProbe) (*CursorResumeTicket, error) {
	if s == nil || s.cursorExecution == nil {
		return nil, ErrCursorExecutionUnavailable
	}
	return s.cursorExecution.IdentifyResume(probe)
}

// ServeCursorResume 实现 CursorResumeGateway。
func (s *GatewayService) ServeCursorResume(
	ctx context.Context, c *gin.Context, ticket *CursorResumeTicket, in CursorResumeServeInput,
) (*ForwardResult, error) {
	if s == nil || s.cursorExecution == nil {
		return nil, ErrCursorExecutionUnavailable
	}
	if ticket == nil || s.accountRepo == nil {
		return nil, newCursorResumeError(http.StatusBadRequest, "account_unavailable", ErrCursorResumeStaleBatch)
	}
	account, err := s.accountRepo.GetByID(ctx, ticket.AccountID)
	if err != nil {
		return nil, fmt.Errorf("cursor: load resume account: %w", err)
	}
	return s.cursorExecution.ResumeCursorSession(ctx, c, ticket, account, in)
}

// IdentifyCursorResume 实现 CursorResumeGateway。
func (s *OpenAIGatewayService) IdentifyCursorResume(probe *CursorResumeProbe) (*CursorResumeTicket, error) {
	if s == nil || s.cursorExecution == nil {
		return nil, ErrCursorExecutionUnavailable
	}
	return s.cursorExecution.IdentifyResume(probe)
}

// ServeCursorResume 实现 CursorResumeGateway。
func (s *OpenAIGatewayService) ServeCursorResume(
	ctx context.Context, c *gin.Context, ticket *CursorResumeTicket, in CursorResumeServeInput,
) (*ForwardResult, error) {
	if s == nil || s.cursorExecution == nil {
		return nil, ErrCursorExecutionUnavailable
	}
	if ticket == nil || s.accountRepo == nil {
		return nil, newCursorResumeError(http.StatusBadRequest, "account_unavailable", ErrCursorResumeStaleBatch)
	}
	account, err := s.accountRepo.GetByID(ctx, ticket.AccountID)
	if err != nil {
		return nil, fmt.Errorf("cursor: load resume account: %w", err)
	}
	return s.cursorExecution.ResumeCursorSession(ctx, c, ticket, account, in)
}

// ---------------------------------------------------------------------------
// 会话身份在请求 ctx 上的传递（handler 在 Cursor 分支设置，服务层只读取）
// ---------------------------------------------------------------------------

// cursorIdentityCtxKey 是会话身份的唯一 ctx 键（私有类型，避免跨包冲突）。
type cursorIdentityCtxKey struct{}

// WithCursorSessionIdentity 把本请求的会话身份（user / API key / 组）挂到 ctx。
//
// handler 在鉴权完成、资源获取之前调用；服务层在建会话时记录、续轮时比对。
func WithCursorSessionIdentity(ctx context.Context, identity CursorSessionIdentity) context.Context {
	if ctx == nil {
		return nil
	}
	return context.WithValue(ctx, cursorIdentityCtxKey{}, identity)
}

// CursorSessionIdentityFromContext 读取请求 ctx 上的会话身份（缺失即零值）。
func CursorSessionIdentityFromContext(ctx context.Context) CursorSessionIdentity {
	if ctx == nil {
		return CursorSessionIdentity{}
	}
	identity, _ := ctx.Value(cursorIdentityCtxKey{}).(CursorSessionIdentity)
	return identity
}

// cursorResumeProtocolOfRequest 返回本次请求的续轮协议命名空间（索引 + 摘要表示）。
//
// 明确声明优先（Gemini 原生出口复用 Chat 形状写出，但续轮命名空间是 "gemini"）；
// 否则按客户端可见响应协议派生，绝不跨协议猜归属。
func cursorResumeProtocolOfRequest(req *CursorExecutionRequest) CursorResumeProtocol {
	if req == nil {
		return ""
	}
	if p := strings.TrimSpace(req.IndexProtocol); p != "" {
		return CursorResumeProtocol(p)
	}
	switch req.Protocol {
	case cursorProtocolChat:
		return CursorResumeProtocolChat
	case cursorProtocolAnthropic:
		return CursorResumeProtocolMessages
	case cursorProtocolResponses:
		return CursorResumeProtocolResponses
	default:
		return ""
	}
}

// cursorResumeProtocolOfSpec 返回会话的续轮协议命名空间。
func cursorResumeProtocolOfSpec(spec cursorSessionSpec) CursorResumeProtocol {
	if p := strings.TrimSpace(spec.indexProtocol); p != "" {
		return CursorResumeProtocol(p)
	}
	switch spec.protocol {
	case cursorProtocolChat:
		return CursorResumeProtocolChat
	case cursorProtocolAnthropic:
		return CursorResumeProtocolMessages
	case cursorProtocolResponses:
		return CursorResumeProtocolResponses
	default:
		return ""
	}
}

// cursorRequestModel 取续轮请求声明的客户端可见模型名（空 = 未声明）。
func cursorRequestModel(protocol CursorResumeProtocol, body []byte) (string, error) {
	switch protocol {
	case CursorResumeProtocolChat:
		var req apicompat.ChatCompletionsRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return "", fmt.Errorf("%w: parse chat: %v", ErrCursorResumeUnsupportedContent, err)
		}
		return strings.TrimSpace(req.Model), nil
	case CursorResumeProtocolMessages:
		var req apicompat.AnthropicRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return "", fmt.Errorf("%w: parse messages: %v", ErrCursorResumeUnsupportedContent, err)
		}
		return strings.TrimSpace(req.Model), nil
	case CursorResumeProtocolResponses:
		var req apicompat.ResponsesRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return "", fmt.Errorf("%w: parse responses: %v", ErrCursorResumeUnsupportedContent, err)
		}
		return strings.TrimSpace(req.Model), nil
	case CursorResumeProtocolGemini:
		var doc struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			return "", fmt.Errorf("%w: parse gemini: %v", ErrCursorResumeUnsupportedContent, err)
		}
		return strings.TrimSpace(doc.Model), nil
	default:
		return "", fmt.Errorf("%w: unknown protocol %q", ErrCursorResumeUnsupportedContent, string(protocol))
	}
}

// batchConsumed 报告当前待完成批是否已被消费（并发仲裁用）。
func (r *cursorSessionRecord) batchConsumed() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pending != nil && r.pending.consumed
}
