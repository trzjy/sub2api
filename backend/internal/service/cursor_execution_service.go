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
	"crypto/subtle"
	"encoding/base64"
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
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
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
	// cursorProtocolGemini 表示入站为 Gemini v1beta 原生形状：响应由 Gemini codec 写出。
	//
	// §25.17.2「Gemini 与 Messages 落点」：原生出口必须由**同一执行器的 Gemini writer**
	// 逐帧写出，不再先转 Chat 摘要、捕获整包再回放。
	cursorProtocolGemini
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
	// namespaceCustomTools 标记哪些摊平名属于 namespace 内的 custom 子工具。
	// 缺这一位时，摊平名一律按普通 function_call 交付；codex 收到 function_call
	// 而不是 custom_tool_call，会直接把这次工具调用当不认识的工具扔掉。
	namespaceCustomTools map[string]bool
	toolSearch           bool
}

// cursorToolMetaFromResponses 从原始 Responses 请求派生回桥工具元数据。
//
// 与下发声明同源同一组 api 调用（apicompat.EffectiveResponsesFlatTools）。
func cursorToolMetaFromResponses(req *apicompat.ResponsesRequest) (cursorResponsesToolMeta, error) {
	if req == nil {
		return cursorResponsesToolMeta{}, nil
	}
	effective, err := apicompat.EffectiveResponsesTools(req)
	if err != nil {
		return cursorResponsesToolMeta{}, fmt.Errorf("cursor: resolve effective responses tools: %w", err)
	}
	return cursorToolMetaFromTools(effective)
}

// cursorToolMetaFromTools 由**已提升的有效工具集**派生元数据。
//
// 内部先摊平 namespace：上行声明用的就是摊平名（见 cursorPlanFromResponses），
// 元数据只有记同一份名字，回程才认得上游回调的那个名字。摊平含 custom 子工具，
// 并把"哪些摊平名是 custom"单独标记出来，供回程判 custom_tool_call。
func cursorToolMetaFromTools(effective []apicompat.ResponsesTool) (cursorResponsesToolMeta, error) {
	flat, err := apicompat.FlattenResponsesToolList(effective)
	if err != nil {
		// 摊平撞名（顶层工具与 namespace 子工具同名）必须失败关闭：
		// 此时"猜一个名字"必然把调用挂到错的工具上。
		return cursorResponsesToolMeta{}, fmt.Errorf("%w: responses tools: %v", ErrCursorResumeUnsupportedContent, err)
	}
	return cursorResponsesToolMeta{
		customTools:          flat.CustomTools,
		functionTools:        flat.FunctionTools,
		namespaceTools:       flat.NamespaceTools,
		namespaceCustomTools: flat.NamespaceCustomTools,
		toolSearch:           flat.ToolSearch,
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

// ErrCursorContentWhileWaitingTools 表示会话在 waiting_tools 期间收到**新的模型正文**。
//
// §25.17.2-1 末段：原生阶段契约是"交付完整工具批 → 暂停 → 收到真实结果才继续"。等待期间
// 模型不得再产出正文；出现正文即违反阶段契约，按显式协议失败处理（不重放、不缓存、
// 不把这段正文交给任何客户端）。
var ErrCursorContentWhileWaitingTools = errors.New("cursor: model emitted new content while waiting for tool results (fail closed)")

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
	// httpResponseOwnerBinder 是 HTTP 续轮归属登记回调（由装配层注入 OpenAIGatewayService 的实现）。
	// Cursor 供给此前从不写 owner 绑定，导致 /v1/responses 带 previous_response_id 续轮必然 400。
	// 未注入（nil）时跳过登记，行为与本卡之前完全一致；登记失败只记 warn，绝不影响请求。
	httpResponseOwnerBinder func(c *gin.Context, responseID string) error
	// accountExhaustedHandler 是"账号额度耗尽/限流"回调（RL-STOP 派发单）。
	//
	// cursor 账号是"用完即丢"的一次性额度：上游一旦限流/额度耗尽，该账号必须立刻停止调度
	// （schedulable=false），不能靠重试自愈；同一请求应换到别的账号继续。本回调由装配层注入，
	// 负责把账号置为不可调度。未注入（nil）时安全 no-op，行为与本卡之前完全一致。
	accountExhaustedHandler func(ctx context.Context, accountID int64, reason string)
	// accountRateLimitedHandler 是「账号被上游限流」回调（2026-11-06 拆包）。
	//
	// 必须与 accountExhaustedHandler 分开：限流是秒/分钟级瞬时状态，额度耗尽要等到
	// 次日 8:00 重置。两者原先共用同一个回调，导致每一次 429 限流都被写成"当日额度
	// 耗尽"并按日停调——生产实证（2026-11-06 02:28）单码 15 个账号在 1 秒内被同一次
	// 429 全部摘出候选池，这是"号池没有可用账号"的直接成因。
	//
	// 未注入（nil）时退化为只换号、不停号。
	accountRateLimitedHandler func(ctx context.Context, accountID int64, reason string)
	// accountProtocolFailureHandler 是「上游协议污染」回调（2026-11-04 活体补装）。
	//
	// 与额度耗尽不同：这类故障不是账号没额度，而是账号背后的真实模型被上游随机
	// 轮换，某一轮把工具调用吐进正文、且救不回来。此时本轮已经无法交付有效结果，
	// 但账号本身没坏——用短冷却把它移出候选池，让下一次请求换号即可，不能像额度
	// 耗尽那样长时间摘号，也不能什么都不做（粘性会话会一直粘着它）。
	accountProtocolFailureHandler func(ctx context.Context, accountID int64, reason string)
	// accountKeyRejectedHandler 是「上游拒钥」回调（CURSOR-PROBE-001-A3）。
	//
	// 拒钥 = 上游明确不要这个号了（凭据过期/额度耗尽被拒，见 *CursorKeyRejectedError）。
	// 与"账号级上游故障"（临时停调等探针恢复）必须严格区分：拒钥只做永久停用
	// （schedulable=false，等管理员核实），不写任何停调、不进熔断计数、不被探针恢复。
	// 本回调由装配层注入 OpenAIGatewayService.tempUndisableCursorCredential 的实现
	// （内部只转接 accountRepo.SetError），执行层不持有仓储。未注入（nil）时安全 no-op。
	accountKeyRejectedHandler func(ctx context.Context, accountID int64, reason string)
}

// cursorWaitingToolsIdleTimeout 是 waiting_tools 状态允许的最长静默时间（方案 §2.5）。
//
// 交付完整工具批后，会话停在 waiting_tools 等客户端续轮；客户端按 esc 中断、直接退出、
// 或根本没打算续轮时，会话既等不到真实终态也等不到取消，用户槽/账号槽与余额预留会
// 一直被持有（§25.4 资源跟着会话走）。生产实证（2026-11-04）：3 个账号槽挂死 → 该账号
// 对**新**请求持续表现为"并发满" → 粘性会话又不换号 → 整条链路每 120 秒撞一次 429，
// 直到 Redis 侧 concurrency_slot_ttl（30 分钟）被动清理才恢复。
//
// 到期且仍处于 waiting_tools、本批结果仍未被消费（pending.consumed == false）时，走
// **同一个**唯一 finalizer 收尾（reason=waiting_tools_idle），结算口径不变：没有
// TurnEnded 就照旧 session_settle_skipped，不伪造 usage。
//
// 取值 10 分钟：方案 §2.5 原定 120 秒（用户 2026-11-01 裁定 B），2026-11-04 用户复
// 改为 10 分钟——120 秒会误杀「客户端正在跑长工具（大编译/长测试）」的正常会话，把续轮
// 打成 CUR-RES-03。10 分钟仍小于 Redis 侧 concurrency_slot_ttl（30 分钟），泄漏窗口
// 从 30 分钟压到 10 分钟。它与 180 秒总期限是两个独立条件、取先到者；后续到达的续轮按
// 既有 session_finished 拒绝（CUR-RES-03）。
const cursorWaitingToolsIdleTimeout = 10 * time.Minute

// SetAccountExhaustedHandler 注入"账号额度耗尽/限流"回调（与 SetHTTPResponseOwnerBinder 同型）。
//
// 未注入时安全 no-op（已通过 nil 守卫）。回调报错（含 panic）只记 Warn，绝不改变
// Execute 返回的错误类型与换号语义。
func (s *CursorExecutionService) SetAccountExhaustedHandler(fn func(ctx context.Context, accountID int64, reason string)) {
	if s == nil {
		return
	}
	s.accountExhaustedHandler = fn
}

// SetAccountRateLimitedHandler 注入「账号被上游限流」回调，与 SetAccountExhaustedHandler 同型。
//
// 两个回调必须分开：限流→分钟级冷却，额度耗尽→等次日 8:00 重置。混用会把好号按日摘掉。
func (s *CursorExecutionService) SetAccountRateLimitedHandler(fn func(ctx context.Context, accountID int64, reason string)) {
	if s == nil {
		return
	}
	s.accountRateLimitedHandler = fn
}

// SetAccountProtocolFailureHandler 注入「上游协议污染」回调。
//
// 触发场景：本轮产物里出现工具调用暗号但一个都解析不出来（InlineToolCallSuspected）。
// 这一刻客户端已经拿到了一轮"看起来正常、实际什么都没有"的结束，用户侧只会看到
// 模型答应了却没动静。粘性会话会把用户继续摁在同一个账号上反复复现，所以必须让
// 该账号短暂退出候选池。未注入时 no-op，行为与注入前一致。
func (s *CursorExecutionService) SetAccountProtocolFailureHandler(fn func(ctx context.Context, accountID int64, reason string)) {
	if s == nil {
		return
	}
	s.accountProtocolFailureHandler = fn
}

// SetAccountKeyRejectedHandler 注入「上游拒钥」回调，与 SetAccountExhaustedHandler 同型。
//
// 触发场景：普通路径 defer 与续轮路径 cursorResumeRebuildError 命中 *CursorKeyRejectedError。
// 回调报错（含 panic）只记 Warn，绝不改变返回的错误类型与换号语义。
func (s *CursorExecutionService) SetAccountKeyRejectedHandler(fn func(ctx context.Context, accountID int64, reason string)) {
	if s == nil {
		return
	}
	s.accountKeyRejectedHandler = fn
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

// Stop 关闭会话表并清空全部关联（DI cleanup 调用一次；重复调用不重复结算/释放）。
//
// §25.17.7 A09：现存会话走**同一个 finalizer**，Stop 随后在会话锁外等待每个会话的
// finalizeDone——即等唯一胜者完成结算提交、资源归还与索引清理；Stop 返回后新请求拿不到
// 会话（sessions.start 明确不可用，零 AgentRun），孤儿续轮关联也已失效。
func (s *CursorExecutionService) Stop() {
	if s == nil || s.sessions == nil {
		return
	}
	s.sessions.stop()
}

// SetHTTPResponseOwnerBinder 注入 HTTP 续轮归属登记回调（装配层在网关构造后一次性设置）。
func (s *CursorExecutionService) SetHTTPResponseOwnerBinder(fn func(*gin.Context, string) error) {
	if s == nil {
		return
	}
	s.httpResponseOwnerBinder = fn
}

// bindHTTPResponseOwner 是登记回调的唯一调用点：未注入 / c 为 nil / responseID 为空时静默返回；
// 登记失败只记 warn（与 openai 侧 bindHTTPResponseAccount 的失败语义一致），绝不改变请求结果。
func (s *CursorExecutionService) bindHTTPResponseOwner(c *gin.Context, responseID string) {
	// 非 Responses 路径（未注入 binder / c 为 nil）静默早退——那是正常情形。
	if s == nil || s.httpResponseOwnerBinder == nil || c == nil {
		return
	}
	// binder 已注入，但 responseID 为空：登记会无声失效，记一条 warn 以区分"未登记"。
	if strings.TrimSpace(responseID) == "" {
		var ctx context.Context
		if c.Request != nil {
			ctx = c.Request.Context()
		} else {
			ctx = context.Background()
		}
		cursorPhaseLog(ctx, zapcore.WarnLevel,
			"openai.http_bind_response_owner_skipped", "",
			zap.String("reason", "empty_response_id"))
		return
	}
	if err := s.httpResponseOwnerBinder(c, responseID); err != nil {
		truncated := responseID
		if len(truncated) > 64 {
			truncated = truncated[:64]
		}
		cursorPhaseLog(c.Request.Context(), zapcore.WarnLevel, "openai.http_bind_response_owner_failed_cursor", "",
			zap.String("response_id", truncated))
	}
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
		return nil, newCursorRequestError(infraerrors.CursorExecInternalErrorCode, http.StatusInternalServerError, "typed chat completions request missing")
	}

	// §25.17.4（A08）行 5 的准入断言在 2026-11-07 随五道闸退役：
	// Cursor 执行分支不再因为"声明了续轮却没有可重建的完整历史"返回 400。
	//
	// 原逻辑（保留说明，勿回滚）：请求在原协议层声明续轮（携带本批工具结果或
	// previous_response_id，见 cursorContextPlan.claimsContinuation），若既不来自
	// 有意重建（ctx 上的 CursorRebuildInfo），正文又不足以独立重建（historySupportsRebuild），
	// 就回 400 unknown_association（客户看到 CUR-RES-01）。
	//
	// 为什么退役：Responses 官方用法就是"previous_response_id + 尾部 function_call_output"，
	// 客户端**不重放** function_call —— 那种请求永远算不出"完整历史"，于是被这条闸恒定
	// 拒掉。这是对客户端正常行为的误判，不是真的失联。
	//
	// 现在只留痕，一律放行：有会话就按会话续轮，没有会话就按本请求正文新建
	// （缺的前文由 previous_response_id 语义与本次正文共同决定，不再拦在站点侧）。
	rebuildInfo, rebuildRequested := CursorRebuildFromContext(ctx)
	if req.Plan.claimsContinuation() && !req.Plan.resultsReplayHistory() {
		// 日志只记枚举/ID：正文与上游原文一律不落盘。
		protocol := string(cursorResumeProtocolOfRequest(req))
		if rebuildRequested {
			cursorPhaseLog(ctx, zapcore.InfoLevel, "cursor_session_rebuild", strings.TrimSpace(req.Model),
				zap.Int64("dead_account_id", rebuildInfo.DeadAccountID),
				zap.String("reason", string(rebuildInfo.Reason)),
				zap.String("protocol", protocol),
				zap.Bool("history_complete", req.Plan.historySupportsRebuild()))
		} else {
			cursorPhaseLog(ctx, zapcore.InfoLevel, "cursor_session_rebuild_orphan", strings.TrimSpace(req.Model),
				zap.String("protocol", protocol),
				zap.Bool("history_complete", req.Plan.historySupportsRebuild()))
		}
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
		return nil, newCursorRequestError(infraerrors.CursorReqMissingModelCode, http.StatusBadRequest, "request carries no model")
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

	var bearer string

	if bearer == "" {
		var err error
		bearer, err = cursorResolveAccountBearer(account)
		if err != nil {
			return nil, err
		}
	}

	authProvider, err := newCursorAccountAuthProvider(account, s.quotaKeys, s.enc, s.deviceIDs)
	if err != nil {
		return nil, err
	}

	// ④ 请求转换（带丢弃记账：本层不支持的形状一律丢弃并落日志，不整轮 400）。
	payload, err := cursorConvertRequestWithContext(ctx, req.Chat, upstreamModel)
	if err != nil {
		return nil, err
	}
	if _, err := authProvider.CursorUpstreamAuth(ctx); err != nil {
		return nil, err
	}
	// §25.17.3「A05首次来源指纹」：指纹随材料一同传出，取的是**本次生成头那一次**的
	// 来源行与解密结果。拿不到可靠指纹即失败关闭，绝不以空值登记进会话。
	sourceFP, ok := authProvider.SourceFingerprint()
	if !ok {
		return nil, fmt.Errorf("%w: account %d", ErrCursorSourceFingerprintUnavailable, account.ID)
	}
	cursorPhaseLog(ctx, zapcore.DebugLevel, "auth_done", upstreamModel,
		zap.Int64("account_id", account.ID))
	cursorPhaseLog(ctx, zapcore.DebugLevel, "convert_done", upstreamModel)
	cursorPhaseLog(ctx, zapcore.InfoLevel, "agent_run_begin", upstreamModel)

	// 会话登记：状态所有者、统一索引、批次判定与 §25.4/§25.5 生命周期都在本服务内。
	session := s.sessions.start(cursorSessionSpec{
		protocol:         req.Protocol,
		requestedModel:   originalModel,
		upstreamModel:    upstreamModel,
		accountID:        account.ID,
		quotaKeyID:       cursorAccountQuotaKeyID(account),
		sourceQuotaKeyID: sourceFP.QuotaKeyID,
		sourceCodeMAC:    sourceFP.CodeMAC,
		indexProtocol:    strings.TrimSpace(req.IndexProtocol),
		identity:         req.Identity,
	})
	s.sessions.attachOwner(session, s)
	// §25.17.7 A09：Stop 之后不再登记新会话，调用方拿到 nil 即"明确不可用"——
	// 必须在模型出站/AgentRun 之前失败关闭（零 AgentRun、零游离会话状态）。
	if session == nil {
		return nil, fmt.Errorf("%w: cursor execution service is stopped", ErrCursorExecutionUnavailable)
	}
	// 重建落点（2026-11-04 收口 M5）：只记 ID 与原因枚举，不记正文/凭据。
	// 排查"死号 → 新号"时必须能在日志里连起来，不能只看到重建开始看不到落到哪。
	if rebuildRequested {
		cursorPhaseLog(ctx, zapcore.InfoLevel, "cursor_session_rebuild_bound", originalModel,
			zap.Int64("dead_account_id", rebuildInfo.DeadAccountID),
			zap.Int64("new_account_id", account.ID),
			zap.String("reason", string(rebuildInfo.Reason)))
	}
	// §25.14-D：首 HTTP 把有效输入历史逐项摘要入会话（config_digest + history_digest +
	// 逻辑项数），供续轮重算前缀摘要对比。只落摘要状态，不落正文。
	session.recordPlan(req.Identity, req.Plan)
	// 本 HTTP 的逐请求选项（stream / tool_choice 不跨 HTTP 累积，§25.17.2-2）。
	session.setHTTPOptions(cursorHTTPOptions{
		stream:         req.Stream,
		model:          originalModel,
		toolChoiceMode: req.ToolChoiceMode,
		toolChoiceName: req.ToolChoiceName,
	})
	session.setLogContext(ctx)

	// RL-STOP：在所有错误出口统一分类一次（限流/额度耗尽 → 停号 + 换号）。
	//
	// 上游把账号限流等终态业务错误放在 EndStream 的 error 里（code
	// ERROR_PRO_USER_RATE_LIMIT_EXCEEDED）。本 defer 是唯一的分类点：无论错误来自 forwarder.Do
	// 还是 pump（executeHTTP 内部），都经 cursorUpstreamStreamErrorCode 取出 code 判定；
	// 非该 code 的错误一律 no-op，不改变任何既有行为与判定。动作 A（停号回调）无论是否已向
	// 客户端发布正文字节都调用一次；动作 B（替换为可换号 *UpstreamFailoverError）仅当本 HTTP
	// 尚未发布任何正文字节、且会话从未交付过工具批（外审 B3：防止续轮/ mid-stream 误换号）时执行。
	//
	// 模型暂不可用（方案 §2.4）：同一分类点、与限流平行判断。上游临时故障，不是账号问题，
	// 不调停号回调；换号判据与限流动作 B 相同，否则原样返回（由 CursorUserFacingError
	// 映射 CUR-EXEC-09）。
	defer func() {
		if err == nil {
			return
		}
		if code, ok := cursorUpstreamStreamErrorCode(err); ok && code == "ERROR_PRO_USER_RATE_LIMIT_EXCEEDED" {
			// 限流路径：Info 级一条（含 account_id 与 code），正常路径不额外写日志。
			//
			// 2026-11-06 拆包：这里原本走 accountExhaustedHandler，把"限流"当"当日额度
			// 耗尽"处理。生产实证 02:28 一次 429 让单码 15 个账号在 1 秒内全被摘出候选池
			// 并停到次日 8:05，而同码当天没有任何「今日额度已用完」记录。限流必须走短冷却，
			// 否则一次上游抖动就能把整个码清空。
			cursorPhaseLog(ctx, zapcore.InfoLevel, "cursor_account_rate_limited", originalModel,
				zap.Int64("account_id", account.ID), zap.String("code", code))

			// 动作 A：无论是否已发布正文字节，停号回调只调用一次；回调报错（含 panic）只记 Warn，
			// 绝不改变返回的错误类型与换号语义。
			if s.accountRateLimitedHandler != nil {
				func() {
					defer func() {
						if r := recover(); r != nil {
							cursorPhaseLog(ctx, zapcore.WarnLevel, "cursor_account_rate_limited_handler_panic", originalModel,
								zap.Int64("account_id", account.ID), zap.Any("panic", r))
						}
					}()
					s.accountRateLimitedHandler(ctx, account.ID, "cursor account rate limited (upstream stream code)")
				}()
			}
			// 动作 B（外审 B3 加固）：仅当本 HTTP 尚未发布正文字节，且会话从未交付过工具批
			// （deliveredBatchSeq==0，即非续轮/非 mid-stream）时，才替换为可换号 failover，让既有
			// handler 换号重试；否则错误原样返回，禁止 mid-stream 换号（锁定项 3）。
			if !session.bodyPublished && session.deliveredBatchSeq == 0 {
				err = &UpstreamFailoverError{
					StatusCode:        429,
					Stage:             GatewayFailureStage("upstream"),
					Scope:             GatewayFailureScopeAccount,
					Reason:            CursorRateLimitedReason,
					NextAccountAction: NextAccountRetry,
					ClientStatusCode:  429,
					ClientMessage:     "cursor 账号被上游限流，已切换账号",
				}
			}
			return
		}
		// 流内「今日额度已用完」模板（CursorQuotaExhaustedError）。
		//
		// 2026-11-06 定性更正：这句文案不是余额判定，是上游对单个账号状态异常的挡板话术
		// （三重排他证据见 downstream_messages.go CUR-EXEC-11 注释）。处置相应改为：
		// 走账号级短冷却（5 分钟）+ 换号，不再按日停调；对外文案不再声称"额度耗尽"。
		// 仍绝不把上游原文（含卡 ID）透传给客户端。
		var qe *CursorQuotaExhaustedError
		if errors.As(err, &qe) {
			cursorPhaseLog(ctx, zapcore.InfoLevel, "cursor_account_blocked_by_upstream", originalModel,
				zap.Int64("account_id", account.ID), zap.String("code", "in_stream_quota_template"),
				zap.String("turn_id", qe.TurnID))
			if s.accountExhaustedHandler != nil {
				func() {
					defer func() {
						if r := recover(); r != nil {
							cursorPhaseLog(ctx, zapcore.WarnLevel, "cursor_account_exhausted_handler_panic", originalModel,
								zap.Int64("account_id", account.ID), zap.Any("panic", r))
						}
					}()
					s.accountExhaustedHandler(ctx, account.ID, "cursor upstream claimed daily quota exhausted (in-stream template)")
				}()
			}
			if !session.bodyPublished && session.deliveredBatchSeq == 0 {
				err = &UpstreamFailoverError{
					StatusCode:        429,
					Stage:             GatewayFailureStage("upstream"),
					Scope:             GatewayFailureScopeAccount,
					Reason:            CursorQuotaExhaustedReason,
					NextAccountAction: NextAccountRetry,
					ClientStatusCode:  429,
					ClientMessage:     "cursor 账号暂时不可用，已切换账号",
				}
			}
			return
		}
		// 拒钥（上游明确不要这个号了）：独立 reason + 独立换号构造 + 永久停用回调。
		// 只认 *CursorKeyRejectedError（其 Unwrap 使 errors.Is(err, ErrCursorKeyRejectedUpstream)
		// 成立），绝不按 reason 字符串匹配。不得复用 CursorUpstreamAccountFailureReason
		// （那样会进 TempUnscheduleRetryableError 写探针 marker，进而被探针清回候选池、
		// 绕过管理员核实）；也不进任何停调白名单（failover_loop.go:276-280）⇒ 拒钥只做
		// 永久停用、不写停调。
		var keyRej *CursorKeyRejectedError
		if errors.As(err, &keyRej) {
			cursorPhaseLog(ctx, zapcore.WarnLevel, "cursor_key_rejected", originalModel,
				zap.Int64("account_id", account.ID),
				zap.String("turn_id", keyRej.TurnID))
			// 动作 A：永久停用回调（无论是否已发布正文字节，账号确实被拒，必须停用）。
			// 回调报错（含 panic）只记 Warn，绝不改变返回的错误类型与换号语义。
			if s.accountKeyRejectedHandler != nil {
				func() {
					defer func() {
						if r := recover(); r != nil {
							cursorPhaseLog(ctx, zapcore.WarnLevel, "cursor_account_key_rejected_handler_panic", originalModel,
								zap.Int64("account_id", account.ID), zap.Any("panic", r))
						}
					}()
					s.accountKeyRejectedHandler(ctx, account.ID, "cursor upstream rejected the key (expired or quota exhausted)")
				}()
			}
		// 动作 B（外审 B3 加固）：仅当本 HTTP 尚未发布正文字节且会话从未交付过工具批时，
		// 才叠加为可换号 failover，让既有 handler 换号重试；否则错误原样返回，禁止
		// mid-stream 换号（锁定项 3）。
		//
		// 关键契约（A8 卡修复）：换号构造**不得**用 `err = &UpstreamFailoverError{...}` 把原始
		// `*CursorKeyRejectedError` 整体替换掉 —— 那样会丢失 sentinel
		// （ErrCursorKeyRejectedUpstream，由 CursorKeyRejectedError.Unwrap 提供）与 rejected.TurnID，
		// 导致对外映射 CUR-EXEC-12 失败、既有契约 TestCursorProtocolRejectedKeyFailsClosed 等翻红。
		// 改用 errors.Join 把「可换号 failover」与「原始拒钥错误链」两条链同时保留：
		//   - errors.As(err, &fe) 仍能拿到 *UpstreamFailoverError（换号语义不变）；
		//   - errors.Is(err, ErrCursorKeyRejectedUpstream) 与 errors.As(err, &rejected) 仍成立
		//     （拒钥 sentinel + TurnID 不丢，对外映射 CUR-EXEC-12 保住）。
		// 不动 UpstreamFailoverError 的既有字段语义，亦不动其它 36 个构造点。
		if !session.bodyPublished && session.deliveredBatchSeq == 0 {
			err = errors.Join(&UpstreamFailoverError{
				StatusCode:        http.StatusBadGateway,
				Stage:             GatewayFailureStage("upstream"),
				Scope:             GatewayFailureScopeAccount,
				Reason:            cursorKeyRejectedReason,
				NextAccountAction: NextAccountRetry,
				ClientStatusCode:  http.StatusBadGateway,
				ClientMessage:     infraerrors.CursorExecAccountRejected,
			}, err)
		}
		return
		}
		// 上游对本账号回吐「非业务码」的终态错误（Connect 传输码：internal / unknown /
		// unavailable 等，典型报文 internal: Cannot read properties of undefined (reading
		// 'toString')）：这表示该账号当下无法服务，不是请求侧问题，因此不能把 502 直接
		// 甩给用户。与限流/模型不可用同一分类点、同一换号判据（未发布正文 && 未交付工具批），
		// 替换为可换号错误，由既有 failover 循环换到本组下一个 cursor 账号；本组 cursor 池
		// 轮询完后由既有选号器继续走组内备用渠道（anthropic 等）。
		// 业务码（ERROR_ 前缀，如 ERROR_PRO_USER_RATE_LIMIT_EXCEEDED / ERROR_BAD_MODEL_NAME）
		// 各自有专门语义，绝不在此吞掉。
		var mu *CursorModelUnavailableError
		if code, ok := cursorUpstreamStreamErrorCode(err); ok &&
			!isCursorBusinessErrorCode(code) && !errors.As(err, &mu) {
			// 透传上游 message（已脱敏，见 cursorParseEndStream + cursorRedactSecrets）：
			// 让「上游自身故障」与「我们的请求触发上游 bug」可区分。取不到时记空串，
			// 不填占位符 —— 「没有」与「有但是空的」要能区分开。
			upstreamMsg, _ := cursorUpstreamStreamErrorMessage(err)
			cursorPhaseLog(ctx, zapcore.WarnLevel, "cursor_upstream_account_failure", originalModel,
				zap.String("code", code),
				zap.String("upstream_message", truncateOpenAIWSLogValue(upstreamMsg, 240)),
				zap.Int64("account_id", account.ID),
				zap.Bool("body_published", session.bodyPublished),
				zap.Int("delivered_batches", int(session.deliveredBatchSeq)))
			if !session.bodyPublished && session.deliveredBatchSeq == 0 {
				err = &UpstreamFailoverError{
					StatusCode:        http.StatusBadGateway,
					Stage:             GatewayFailureStage("upstream"),
					Scope:             GatewayFailureScopeAccount,
					Reason:            CursorUpstreamAccountFailureReason,
					NextAccountAction: NextAccountRetry,
					ClientStatusCode:  http.StatusBadGateway,
					ClientMessage:     infraerrors.CursorExecUpstreamProtocol,
					// 上游错误码与原文摘要带到 handler 层：抽风判定日志要能区分
					// internal / unavailable 等传输码，并核对上游原文。原文复用既有的
					// 截断规则（与上方 cursor_upstream_account_failure 日志同口径），
					// handler 层直接打本字段即可，不必再截断一次。
					CursorUpstreamErrorCode:    code,
					CursorUpstreamErrorMessage: truncateOpenAIWSLogValue(upstreamMsg, 240),
				}
			}
			return
		}
		if !errors.As(err, &mu) {
			return
		}
		cursorPhaseLog(ctx, zapcore.WarnLevel, "cursor_upstream_model_unavailable", originalModel,
			zap.String("turn_id", mu.TurnID),
			zap.Int64("account_id", account.ID),
			zap.Bool("body_published", session.bodyPublished),
			zap.Int("delivered_batches", int(session.deliveredBatchSeq)))
		if !session.bodyPublished && session.deliveredBatchSeq == 0 {
			err = &UpstreamFailoverError{
				StatusCode:        503,
				Stage:             GatewayFailureStage("upstream"),
				Scope:             GatewayFailureScopeAccount,
				Reason:            GatewayFailureReason("cursor_model_unavailable"),
				NextAccountAction: NextAccountRetry,
				ClientStatusCode:  503,
				ClientMessage:     infraerrors.CursorExecModelUnavailable,
			}
		}
	}()

	// 会话运行 context 与 HTTP context 取消**解耦**：正常 handler 返回触发的取消不能
	// 误杀已完成交接的会话；附着期间的真实断连由 attachHTTP 的取消回调判定。
	//
	// 跨 HTTP（§25.4 / W2-F / §25.17.2-2）：本 HTTP 返回**不得**取消会话运行 ctx。该 ctx
	// 是原生 AgentRun 连接的存活依据，取消权
	// 在 registStreamClose 之后交给 finalizer（streamClose）。stream 与 nonstream 同口径：
	// 只要读循环仍在且会话未被收口（本 HTTP 在完整工具批检查点返回），就把 ctx 交给
	// finalizer；否则（错误早退 / 单 HTTP 完成）自行取消，避免泄漏一个永不取消的 ctx。
	sessionCtx, cancelSession := session.sessionContext(ctx)
	sessionCtxHandedOver := false
	defer func() {
		if sessionCtxHandedOver {
			return
		}
		session.mu.Lock()
		alive := !session.pumpEnded &&
			session.state != cursorSessionClosed && session.state != cursorSessionFinalizing
		session.mu.Unlock()
		if alive {
			return
		}
		cancelSession()
	}()
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
	// 由 pump 在真实终态 / 传输错误时独占。仅当本次 Execute 以错误返回、或 pump
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
		state := session.state
		session.mu.Unlock()
		// §25.17.2-2：stream 与 nonstream 同口径——只要读循环仍在（pumpEnded=false）且会话
		// 处于运行/等待工具（本次 HTTP 在完整工具批检查点交付后返回），会话就跨 HTTP 存活，
		// finalizer 由 pump 在真实终态/传输错误时独占。只有读循环已结束或会话已被
		// finalizer 收口时，才由本返回点收尾（幂等，不双写）。
		if pumpEnded || state == cursorSessionClosed {
			s.finishCursorSession(session, "completed")
			return
		}
		// 会话跨 HTTP 存活：不在此 finalize（finalizer 由 pump 独占）。
	}()

	headers := cursorAgentRunHeaders(bearer, authProvider.device, s.now())

	fwdReq := cursorBuildAgentRunUpstreamRequest(req, payload, authProvider, upstreamModel, delegations, headers)

	var resp *CursorUpstreamResponse
	resp, err = s.forwarder.Do(sessionCtx, fwdReq)
	if err != nil {
		return nil, err
	}
	// finalizer 的②关闭原生流：与下方的按需 defer 幂等共存，只保证"取消读写"这一步至多一次。
	//
	// §25.17.2-2：stream 与 nonstream 同口径——本 HTTP 返回**不关闭原生流**（会话跨 HTTP
	// 存活时真实结果仍要写回该连接），body 与会话运行 ctx 的所有权在此交给 finalizer；
	// 只有会话已被收口（单 HTTP 完成 / 失败早退）时才由本 HTTP 关闭。
	session.mu.Lock()
	session.streamClose = func() {
		_ = resp.Body.Close()
		cancelSession()
	}
	session.mu.Unlock()
	sessionCtxHandedOver = req.Stream
	defer func() {
		session.mu.Lock()
		alive := !session.pumpEnded &&
			session.state != cursorSessionClosed && session.state != cursorSessionFinalizing
		session.mu.Unlock()
		if alive {
			// 跨 HTTP 存活：原生流归 finalizer 独占关闭（不在此提前断开）。
			return
		}
		_ = resp.Body.Close()
	}()

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

	// stream / nonstream 走**同一条**读循环与同一个交付边界，只有本 HTTP 的写出方式不同
	// （§25.17.2-2：两条链不在各自路径里分裂）。
	return s.executeHTTP(ctx, c, account, req, resp, bearer, originalModel, upstreamModel, startTime, delegations, session)
}

// cursorSessionTerminalReason 把执行错误归类为会话终态原因（日志安全字段）。
//
// 只有真实断连/客户端取消/外部 deadline/服务 Stop 才结束会话；正常返回是 completed。
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

// executeHTTP 是**每会话同一条读循环 + 每 HTTP 一个协议 writer**的唯一执行实现
// （§25.17.2：stream 独立 pump / nonstream read-all 两条链在此收敛）。
//
// 职责划分：
//   - 原生读循环（pump，每会话唯一）只产出增量文本、完整工具批与真实终态/错误；
//   - 本函数只负责**当前 HTTP**：安装 intake、附着输出消费者、准备 writer 与输出标识，
//     然后把自本次附着以来的事件按本 HTTP 的 stream/nonstream 选项写出并交付确认。
//
// 顺序纪律（§25.17.2-1）：真实 intake、输出消费者、writer 与响应 ID 必须在开读之前装好，
// 首帧不会落在未附着窗口；nonstream 不提前提交 200 头（见 beginResponseWriter）。
func (s *CursorExecutionService) executeHTTP(
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
	// ① 跨 HTTP 真实结果回写通道（CursorSessionIntake 生产实现）：绑定原生双工 writer。
	session.output = newCursorSessionOutput()
	if resp.Duplex != nil {
		session.agentRunDuplex = resp.Duplex
		if err := s.installIntake(session.id, &cursorAgentRunIntake{
			svc:         s,
			session:     session,
			duplex:      resp.Duplex,
			delegations: delegations,
		}); err != nil {
			return nil, err
		}
	}

	// ② 本 HTTP 的输出标识与消费者：先附着（拿到唯一响应 ID），再开读。
	ch, detach := session.output.attach()
	if ch == nil {
		s.finishCursorSession(session, "attach_failed")
		return nil, fmt.Errorf("%w: cannot attach session output", ErrCursorExecutionUnavailable)
	}
	defer detach()
	responseID := session.output.responseID()

	s.bindHTTPResponseOwner(c, responseID)

	parser := cursorNewStreamParser(originalModel, bearer)
	parser.attachToolDelegations(delegations)
	if resp.Duplex != nil {
		parser.attachToolSchemas(resp.Duplex.declaredToolDefs)
	}
	// 本 HTTP 的 chunk 一律使用该 HTTP 的实际响应 ID（不是固定常量）。
	parser.attachOutputIDSource(session.output.responseID)

	writer, restore, werr := s.beginResponseWriter(c, req, resp, responseID, session)
	if werr != nil {
		return nil, werr
	}
	if restore != nil {
		defer restore()
	}

	// ③ 启动每会话唯一的背景读循环（独占 resp.Body 读取；跨 HTTP 存活）。
	go s.runCursorAgentRunPump(session, resp, req, parser, session.output)

	cursorPhaseLog(ctx, zapcore.InfoLevel, "response_write_begin", upstreamModel,
		zap.String("outcome", "ok"), zap.Bool("stream", req.Stream))
	return s.cursorDrainAttachedOutput(ch, writer, req, session, false)
}

// cursorDrainAttachedOutput 消费附着输出通道的帧并按**本 HTTP 选项**写出。
//
// stream：逐帧写出并 Flush；nonstream：只累计本 HTTP 的输出，收尾时聚合成一个合法 JSON。
// 委托检查点（isDeleg）与真实终态（isEnd）都收尾返回；区别只在终结语义：
//   - isDeleg：交付工具批成功 → 写出协议结束标记 → 服务锁内提交 pending.delivered；
//   - isEnd：真实终态。**携带错误时走唯一失败链**：不写成功终结标记、不 finalize。
//
// resumeRebuild=true 时（仅续轮 serve 路径）在失败终态上追加"能否换号重建"的分类：
// 只有**本 HTTP 尚未写出任何语义字节**且错误属账号级终态，才包装成
// *CursorResumeRebuildError；一旦写出过字节就保持原错误（锁定项 3：禁止流拼接腐化）。
func (s *CursorExecutionService) cursorDrainAttachedOutput(
	ch chan *cursorOutputEvent,
	writer io.Writer,
	req *CursorExecutionRequest,
	session *cursorSessionRecord,
	resumeRebuild bool,
) (*ForwardResult, error) {
	var aggregated bytes.Buffer
	// publishedThisHTTP 是**本 HTTP**（不是整个会话）的写出标记：首次成功写出数据帧后置位。
	// 续轮跨 HTTP 存活，session.bodyPublished 反映的是会话历史，不能用来判定本 HTTP。
	publishedThisHTTP := false
	for {
		ev, ok := <-ch
		if !ok {
			return nil, fmt.Errorf("%w: session output detached", ErrCursorExecutionUnavailable)
		}
		if ev.isDeleg {
			if ev.prepared == nil {
				// 委托检查点必须带着已准备好的批：缺失说明准备阶段未完成，绝不当作交付成功。
				return nil, fmt.Errorf("%w: delegation checkpoint without prepared batch", ErrCursorBatchIncomplete)
			}
			// 任何工具批字节写给 HTTP **之前**完成条目 ID 关联与冲突检查（§25.17.2-3/4，
			// 探针"首个工具字节写出前索引已登记"）：Responses 流式路径由协议 writer 在本批
			// 转换后、写 SSE 前先登记，这里为幂等兜底与 nonstream 聚合写出前的登记。一旦冲突
			// 即返回原错误，绝不写出终止帧（response.completed），实现"零成功字节"。条目 ID
			// 取自本 HTTP 已实际写出的内容（aggregated 字节，与 writer.collectedItemIDs 同源
			// 但更可靠：不依赖 writer 是否被包装、也不区分流式/非流式）。
			if err := s.commitDeliveredBatchItems(session, ev.prepared, cursorCollectDeliveredItemIDs(writer, aggregated.Bytes())); err != nil {
				return nil, err
			}
			// 写出本 HTTP 的协议结束标记（完整批已在此前的数据帧里完整写出）。
			if err := s.finishHTTPOutput(session.phaseContext(), writer, req, &aggregated, false); err != nil {
				return nil, err
			}
			cursorPhaseLog(session.phaseContext(), zapcore.InfoLevel, "response_write_done", req.Model,
				zap.String("outcome", "ok"), zap.Bool("stream", req.Stream))
			// 交付确认：只有完整写出成功后才在服务锁内提交 waiting_tools（§25.17.2-4）；
			// 条目 ID 已在写前登记，此处只推进状态。
			if err := s.commitDeliveredBatchState(session, ev.prepared); err != nil {
				return nil, err
			}
			return s.cursorSessionStreamResult(session, req), nil
		}
		if ev.isEnd {
			if ev.err != nil && !errors.Is(ev.err, io.EOF) {
				// 失败链：连接上的成功信号到此为止，绝不再写终结成功标记、也不提交批次。
				if resumeRebuild {
					if rebuildErr := s.cursorResumeRebuildError(session, ev.err, publishedThisHTTP); rebuildErr != nil {
						return nil, rebuildErr
					}
				}
				return nil, ev.err
			}
			if err := s.finishHTTPOutput(session.phaseContext(), writer, req, &aggregated, true); err != nil {
				return nil, err
			}
			cursorPhaseLog(session.phaseContext(), zapcore.InfoLevel, "response_write_done", req.Model,
				zap.String("outcome", "ok"), zap.Bool("stream", req.Stream))
			return s.cursorSessionStreamResult(session, req), nil
		}
		if req.Stream {
			if err := cursorWriteAll(writer, ev.data); err != nil {
				return nil, err
			}
			// 写出成功即置位：此后任何失败都不得再换号重建（锁定项 3）。
			publishedThisHTTP = true
			// 同时把已写出的 SSE 字节收进 aggregated：作为本 HTTP 实际交付内容，供检查点
			// 解析真实 tool item ID（与 writer.collectedItemIDs 同源，但不依赖 writer 是否被
			// 包装；非流式本来就用 aggregated）。见 cursorCollectToolItemIDs。
			aggregated.Write(ev.data)
			if f, ok := writer.(http.Flusher); ok {
				f.Flush()
			}
			continue
		}
		aggregated.Write(ev.data)
	}
}

// cursorResumeRebuildError 把续轮 serve 的失败终态分类为"可换号重建"哨兵
// （2026-11-04 方案 §2.4-1）。
//
// 仅当以下条件全部成立时返回非 nil：
//   - 本 HTTP 尚未向客户端写出任何语义字节（publishedThisHTTP=false）；
//   - 错误属账号级终态：流内当日额度耗尽模板、上游拒付模板、非业务码的上游终态错误。
//
// 拒付/额度同时触发同一个停号回调（§2.4-5：与 Execute 共用一处回调，不新建通道）；
// 非业务码上游故障不停号（与 Execute 的同名分类保持一致，只换号冷却）。
// 其余错误（协议/请求/已发布字节）一律返回 nil，调用方原样返回，禁止 mid-stream 换号。
func (s *CursorExecutionService) cursorResumeRebuildError(
	session *cursorSessionRecord,
	err error,
	publishedThisHTTP bool,
) *CursorResumeRebuildError {
	if session == nil || err == nil {
		return nil
	}
	var (
		reason     GatewayFailureReason
		stopReason string
		matched    bool
	)
	var quotaErr *CursorQuotaExhaustedError
	var keyErr *CursorKeyRejectedError
	switch {
	case errors.As(err, &quotaErr):
		reason = CursorQuotaExhaustedReason
		stopReason = "cursor upstream account daily quota exhausted (resume)"
		matched = true
	case errors.As(err, &keyErr):
		// 拒钥：独立 reason（不得复用 CursorUpstreamAccountFailureReason，否则会写探针 marker
		// 进而被探针清回候选池、绕过管理员核实）；独立永久停用动作由下方
		// reason==cursorKeyRejectedReason 分支触发，不走 notifyAccountExhausted 的停调冷却。
		reason = cursorKeyRejectedReason
		matched = true
	default:
		if code, ok := cursorUpstreamStreamErrorCode(err); ok && !isCursorBusinessErrorCode(code) {
			// 与 Execute 的非业务码分支同语义：账号当下无法服务，换号 + 冷却，不停号。
			reason = CursorUpstreamAccountFailureReason
			matched = true
		}
	}
	if !matched {
		return nil
	}

	session.mu.Lock()
	accountID := session.spec.accountID
	requestedModel := session.spec.requestedModel
	session.mu.Unlock()
	// 拒钥：永久停用回调（与是否已发布字节无关，账号确实被拒，必须停用）。
	// 独立于 quota 的 notifyAccountExhausted 停调冷却（拒钥不写任何停调）。
	if reason == cursorKeyRejectedReason && s.accountKeyRejectedHandler != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					cursorPhaseLog(session.phaseContext(), zapcore.WarnLevel, "cursor_account_key_rejected_handler_panic",
						requestedModel, zap.Int64("account_id", accountID), zap.Any("panic", r))
				}
			}()
			s.accountKeyRejectedHandler(session.phaseContext(), accountID, "cursor upstream rejected the key (expired or quota exhausted)")
		}()
	}
	// 停号补漏（P1）：续轮路径此前完全绕过 Execute 的 defer，账号 357 在额度耗尽后
	// 仍被粘性会话选中。这里复用同一个回调，调用一次；**与是否已发布字节无关**
	// （账号确实 exhausted，必须停），回调 panic 只记 Warn。
	if stopReason != "" {
		s.notifyAccountExhausted(session.phaseContext(), accountID, stopReason)
	}
	// 已发布字节 → 仍原样返回错误（锁定项 3：禁止流拼接腐化），只是不再重建。
	if publishedThisHTTP {
		return nil
	}
	cursorPhaseLog(session.phaseContext(), zapcore.InfoLevel, "cursor_resume_rebuild_allowed",
		requestedModel,
		zap.Int64("account_id", accountID),
		zap.String("reason", string(reason)),
		zap.String("error_class", cursorStreamErrClass(err)))
	// 2026-11-04 收口 M3：返回哨兵前**同步**收尾死亡会话，并等到 finalizeDone 关闭。
	// 不这么做的后果：pump 在另一个 goroutine 里收尾，handler 立刻拿重建决定去重新选号，
	// 旧会话的索引/槽位可能还没归还，偶发 409/identity 冲突被当成 CUR-RES-99。
	s.settleDeadSessionBeforeRebuild(session)
	return &CursorResumeRebuildError{DeadAccountID: accountID, Reason: reason, Err: err}
}

// cursorRebuildFinalizeWait 是重建前等待旧会话终结完成的上界。
//
// finalize 会做网络结算与资源归还，正常在毫秒级；给 5 秒是为了绝不把 HTTP 卡死——
// 超时后照样返回重建哨兵（新会话的登记不依赖旧索引，最坏只是多一条告警）。
const cursorRebuildFinalizeWait = 5 * time.Second

// settleDeadSessionBeforeRebuild 在重建前把死亡会话收尾，并等待终结真正完成（M3）。
//
// 幂等：pump 若已抢先进入 finalizer，本调用返回 false，改为等它的完成信号。
// 无论谁胜出，函数返回时（或超时后）旧会话的索引关联与两槽都已按唯一 finalizer 处理。
func (s *CursorExecutionService) settleDeadSessionBeforeRebuild(session *cursorSessionRecord) {
	if s == nil || session == nil {
		return
	}
	s.finishCursorSessionIf(session, "resume_rebuild", nil)
	signal := session.finalizeSignal()
	if signal == nil {
		return
	}
	timer := time.NewTimer(cursorRebuildFinalizeWait)
	defer timer.Stop()
	select {
	case <-signal:
	case <-timer.C:
		cursorPhaseLog(session.phaseContext(), zapcore.WarnLevel, "cursor_session_rebuild_finalize_wait_timeout",
			session.spec.requestedModel,
			zap.String("session_id", session.id),
			zap.String("wait", cursorRebuildFinalizeWait.String()))
	}
}

// resumeModelChangedOutcome 决定"同一会话换了模型来续轮"该怎么办。
//
// 历史沿革（重要，别照着旧注释把 400 加回来）：
//   - 最初型号变了直接 400 model_changed，理由是"同一次生成不能换模型"——上游那条会话
//     是用旧模型开的，喂进去的字节按旧契约编码；
//   - 2026-11-04 改为"有完整历史就按新模型重建"，但历史不完整仍 400；
//   - 2026-11-07 五道闸退役：型号变化**不再作为拒绝理由**。正文带完整历史时按新模型
//     重开一局（用户无感），历史不完整时**原地继续**（返回 nil，由调用方接着走本
//     会话的续轮流程），绝不再给客户回 model_changed。
//
// 重建路径仍然是"按新模型构造本轮"的唯一正确方式：新 AgentRun 用本请求正文的完整
// 历史 + 新模型创建，旧会话先终结，A 模型的结果不可能灌进 B 模型的会话（防串号）。
func (s *CursorExecutionService) resumeModelChangedOutcome(
	ctx context.Context,
	record *cursorSessionRecord,
	ticket *CursorResumeTicket,
	account *Account,
	storedModel, incomingModel, source string,
) error {
	var accountID int64
	if account != nil {
		accountID = account.ID
	}
	if ticket == nil || ticket.probe == nil || !ticket.probe.Plan.historySupportsRebuild() {
		// 历史不完整（典型：Responses 只给 previous_response_id）：没有可独立重建的正文，
		// 但会话本身还活着、本批结果照样能写回，直接原地继续，不再拒绝。
		cursorPhaseLog(ctx, zapcore.InfoLevel, "session_resume_model_kept_in_place", storedModel,
			zap.String("session_id", cursorSessionIDOf(record)),
			zap.String("skip_reason", "history_incomplete"),
			zap.String("model_stored", storedModel),
			zap.String("model_incoming", incomingModel),
			zap.String("model_source", source))
		return nil
	}
	cursorPhaseLog(ctx, zapcore.InfoLevel, "session_resume_model_rebuild", storedModel,
		zap.String("session_id", cursorSessionIDOf(record)),
		zap.String("batch_id", ticket.BatchID),
		zap.String("model_stored", storedModel),
		zap.String("model_incoming", incomingModel),
		zap.String("model_source", source),
		zap.Int64("account_id", accountID))
	// 与 P1 / 资格拒绝同一条收尾路径：同步 finalize 并等终结完成，避免重建撞上旧会话残留。
	s.settleDeadSessionBeforeRebuild(record)
	return &CursorResumeRebuildError{
		DeadAccountID: accountID,
		Reason:        CursorResumeModelChangedReason,
		KeepAccount:   true,
		Err:           newCursorResumeError(http.StatusBadRequest, "model_changed", ErrCursorResumeConfigChanged),
	}
}


// cursorSessionIDOf 取会话 ID（nil 安全，只用于日志）。
func cursorSessionIDOf(record *cursorSessionRecord) string {
	if record == nil {
		return ""
	}
	return record.id
}

// notifyAccountExhausted 调用装配层注入的停号回调（未注入时 no-op）。
// 回调 panic 只记录 Warn：停号是旁路副作用，任何异常都不得改变主链路的失败分类。
func (s *CursorExecutionService) notifyAccountExhausted(ctx context.Context, accountID int64, reason string) {
	if s == nil || s.accountExhaustedHandler == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			cursorPhaseLog(ctx, zapcore.WarnLevel, "cursor_account_exhausted_handler_panic", "",
				zap.Int64("account_id", accountID), zap.Any("panic", r))
		}
	}()
	s.accountExhaustedHandler(ctx, accountID, reason)
}

// finishHTTPOutput 收尾本 HTTP 的写出。
//
// stream：写出目标协议的终止事件；nonstream：把本 HTTP 累计的增量聚合成**一个**合法
// 响应 JSON 后整包写出（聚合器随本 HTTP 结束释放，不把全会话内容重播给下一 HTTP）。
func (s *CursorExecutionService) finishHTTPOutput(ctx context.Context, w io.Writer, req *CursorExecutionRequest, aggregated *bytes.Buffer, terminal bool) error {
	if req.Stream {
		return s.finalizeResponseWriter(w, req, terminal)
	}
	completion, err := cursorAggregateSSEToCompletion(ctx, aggregated.Bytes())
	if err != nil {
		return err
	}
	if err := cursorWriteAll(w, completion); err != nil {
		return err
	}
	return s.finalizeResponseWriter(w, req, terminal)
}

// cursorWriteAll 写完整块并把短写显式归为 io.ErrShortWrite（§25.17.2-6）。
func cursorWriteAll(w io.Writer, p []byte) error {
	if w == nil {
		return fmt.Errorf("%w: response writer unavailable", ErrCursorProtocolBridgeUnavailable)
	}
	n, err := w.Write(p)
	if err != nil {
		return err
	}
	if n != len(p) {
		return io.ErrShortWrite
	}
	return nil
}

// cursorAggregateSSEToCompletion 把本 HTTP 的 Chat 形状增量聚合成一条 chat.completion JSON。
//
// 只用**本 HTTP 实际写出的字节**（同一 writer 语义），因此不会把上一 HTTP 的正文或已交付
// 工具批重播；id 取本 HTTP 实际响应 ID（chunk 内已由解析器写入）。
//
// ctx 只用于落阶段事件（正文含开标记却认不出形态时的 warn），不参与聚合语义。
func cursorAggregateSSEToCompletion(ctx context.Context, sseBody []byte) ([]byte, error) {
	id, model := "", ""
	content := strings.Builder{}
	type callAcc struct {
		id, name, args string
	}
	calls := make([]*callAcc, 0, 2)
	finishReason := "stop"
	var usage json.RawMessage

	for _, payload := range cursorSSEDataPayloads(sseBody) {
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk apicompat.ChatCompletionsChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return nil, fmt.Errorf("cursor: decode bridged chat chunk: %w", err)
		}
		if chunk.ID != "" {
			id = chunk.ID
		}
		if chunk.Model != "" {
			model = chunk.Model
		}
		if raw := gjson.Get(payload, "usage"); raw.Exists() {
			usage = json.RawMessage(raw.Raw)
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != nil {
				content.WriteString(*choice.Delta.Content)
			}
			for _, tc := range choice.Delta.ToolCalls {
				index := 0
				if tc.Index != nil {
					index = *tc.Index
				}
				for len(calls) <= index {
					calls = append(calls, &callAcc{})
				}
				if tc.ID != "" {
					calls[index].id = tc.ID
				}
				if tc.Function.Name != "" {
					calls[index].name = tc.Function.Name
				}
				calls[index].args += tc.Function.Arguments
			}
			if choice.FinishReason != nil && strings.TrimSpace(*choice.FinishReason) != "" {
				finishReason = *choice.FinishReason
			}
		}
	}
	if id == "" {
		return nil, fmt.Errorf("%w: aggregated completion without response id", ErrCursorProtocolBridgeUnavailable)
	}

	body := content.String()
	inline, rest, recognized := cursorParseInlineToolCalls(body)
	if !recognized {
		// 含开标记却认不出形态：此前的行为是完全静默的「未识别 → 原文透传」，
		// 既不记归一成功也不记拒绝，线上看不到。这类样本最可能变成「工具静默
		// 丢失」（客户端只收到一段话），故补一条 warn（只记形态与开标记个数）。
		if marks := strings.Count(body, cursorInlineToolCallOpen); marks > 0 {
			cursorPhaseLogInlineToolCallUnrecognized(ctx, model, cursorInlineToolCallShapeOf(body), marks)
		}
	}
	if recognized && len(inline) == 0 {
		// 识别到内联形态但解析不出参数 → 失败关闭：不得把原文透传成正文
		// （静默透传正是本缺陷的成因：客户端拿不到 tool_calls，工具静默不执行）。
		return nil, fmt.Errorf("%w: inline tool call present but unparsable", ErrCursorProtocolBridgeUnavailable)
	}
	if len(calls) == 0 && len(inline) > 0 {
		// 结构化委托优先：仅在无结构化调用时用内联形态补位。
		for _, tc := range inline {
			calls = append(calls, &callAcc{id: tc.ID, name: tc.Name, args: tc.Arguments})
		}
		body = rest
	}

	message := map[string]any{"role": "assistant"}
	if len(calls) == 0 {
		message["content"] = body
	} else {
		// 有 tool_calls 时**不得丢正文**：客户端实际收到的 assistant 正文必须原样交付。
		// 交付侧历史摘要（cursorDeliverBatchCheckpoint → batchAssistantText）登记的正是这段
		// 正文；此处丢字会让 Messages / Responses 客户端回传的项数与交付侧不一致，续轮永久
		// 400 history_mismatch。正文为空（无前言）时保持 null，与 OpenAI 惯例一致。
		if text := body; text != "" {
			message["content"] = text
		} else {
			message["content"] = nil
		}
		rendered := make([]any, 0, len(calls))
		for _, call := range calls {
			args := strings.TrimSpace(call.args)
			if args == "" {
				args = "{}"
			}
			rendered = append(rendered, map[string]any{
				"id":   call.id,
				"type": "function",
				"function": map[string]any{
					"name":      call.name,
					"arguments": args,
				},
			})
		}
		message["tool_calls"] = rendered
	}
	completion := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": 0,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
	}
	if len(usage) > 0 {
		completion["usage"] = usage
	}
	out, err := json.Marshal(completion)
	if err != nil {
		return nil, fmt.Errorf("cursor: marshal aggregated completion: %w", err)
	}
	return out, nil
}

// 说明：旧的 executeNonStreaming（先 readCursorUpstreamBody 读完整个上游流再解析、再交付）
// 已按 §25.17.2 / A03 补查删除——它在完整工具批处不能提前交付，模型只能等一个永远不会
// 到达的结果，跨 HTTP 交接无从发生。nonstream 现在与 stream 共用同一条读循环与同一个
// 工具批检查点，只在写出方式上聚合本 HTTP 的输出（见 cursorDrainAttachedOutput）。

// beginResponseWriter 按入站协议准备写出目标。
//
// Chat 直出（writer 即 c.Writer）；Responses/Anthropic 用 Cursor 专用转换 writer
// 包住真实 writer，并把既有 Header 换成独立 map（避免桥接内部头直接落连接）。
// 返回 restore 供调用方在写出后恢复 c.Writer。
func (s *CursorExecutionService) beginResponseWriter(
	c *gin.Context, req *CursorExecutionRequest, resp *CursorUpstreamResponse, responseID string, session *cursorSessionRecord,
) (io.Writer, func(), error) {
	if c == nil {
		return nil, nil, fmt.Errorf("%w: response writer unavailable", ErrCursorProtocolBridgeUnavailable)
	}
	if req.Protocol == cursorProtocolChat {
		if req.Stream {
			applyCursorStreamHeaders(c, resp)
		} else {
			// §25.17.2-1：nonstream 不提前提交 200 头，待完整 JSON 可交付才写（首个 Write
			// 由 gin 自动提交）。这样写失败仍能沿错误出口给出真实状态。
			applyCursorJSONHeadersDeferred(c, resp)
		}
		return c.Writer, nil, nil
	}
	original := c.Writer
	converted := newCursorProtocolWriter(original, req.Stream, req.Protocol, req.Model, req.Tools, responseID, s, session)
	c.Writer = converted
	restore := func() { c.Writer = original }
	return converted, restore, nil
}

// finalizeResponseWriter 收尾写出（目标协议终止事件 / 非流式整包转换）。
//
// terminal 表示本 HTTP 已到达真实终态（turn_ended / 正常结束）；false 表示工具批交付
// 检查点。Gemini 出口据此只在真实终态写 usageMetadata（中间不造最终 token）。
func (s *CursorExecutionService) finalizeResponseWriter(w io.Writer, req *CursorExecutionRequest, terminal bool) error {
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
	converted.terminal = terminal
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

// applyCursorJSONHeaders 设置 Chat 直出的非流式 JSON 头并提交 200 状态。
func applyCursorJSONHeaders(c *gin.Context, resp *CursorUpstreamResponse) {
	applyCursorJSONHeadersDeferred(c, resp)
	c.Writer.WriteHeader(http.StatusOK)
}

// applyCursorJSONHeadersDeferred 只设置非流式 JSON 头，**不**提交状态码（§25.17.2-1）。
//
// 头在写出前设置好，但 200 由首个真实 Write 提交：完整 JSON 不能交付时仍可在错误出口
// 给出真实状态，不会出现"先 200 再失败"的假成功。
func applyCursorJSONHeadersDeferred(c *gin.Context, resp *CursorUpstreamResponse) {
	h := c.Writer.Header()
	h.Set("Content-Type", "application/json")
	h.Del("Content-Length")
	if resp != nil {
		if rid := strings.TrimSpace(resp.Header.Get("x-request-id")); rid != "" {
			h.Set("x-request-id", rid)
		}
	}
}

// copyCursorStream 把解析器产出的 SSE 字节流原样搬到目标 writer 并逐块 Flush。
func copyCursorStream(dst io.Writer, src io.Reader) error {
	if dst == nil || src == nil {
		return cursorFrameErrorf(cursorFrameDelegation, "nil stream")
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

// cursorDeliverBatchCheckpoint 在委托检查点做**所有可失败准备**（§25.17.2-3/4）。
//
// 顺序：封批（listed 等齐 + 每条对外委托有完整 f11）→ 本 HTTP tool_choice 校验 →
// 构造实际输出项及其 ID → 在摘要克隆上计算完整输出增量 → 服务锁内一次检查整批
// call/response/item 冲突并把它们写进同一索引，同时把 pending 置为 delivered=false。
// 任一步失败即返回错误：调用方**不得**发布工具批字节，也不得发任何成功结束标记。
func (s *CursorExecutionService) cursorDeliverBatchCheckpoint(
	session *cursorSessionRecord, parser *cursorStreamParser,
) (*cursorPreparedBatch, error) {
	delegations := parser.toolDelegations()
	if err := cursorSealSessionBatch(delegations); err != nil {
		return nil, err
	}
	opts := session.httpOptions()
	if err := cursorEnforceToolChoiceOnOutput(opts.toolChoiceMode, opts.toolChoiceName, parser.capturedToolCalls()); err != nil {
		return nil, err
	}
	// 本批 assistant 正文取**实际交付文本**（不是固定空文本）：续轮侧按客户端回传的
	// assistant 正文重算前缀摘要，交付侧必须登记同一段正文，否则两侧必然不对称。
	// 基线只在准备成功后推进（见 prepareDeliveredBatch），失败不得吞掉未交付的正文。
	text := session.batchAssistantText(parser.content)
	prepared, err := s.prepareDeliveredBatch(session, cursorResumeProtocolOfSpec(session.spec), opts.model,
		text, delegations, parser.chunkID())
	if err != nil {
		return nil, err
	}
	session.advanceBatchAssistantText(parser.content)
	return prepared, nil
}

// batchAssistantText 返回**本批**实际交付的 assistant 正文（只读，不推进基线）。
//
// 摘要对称性（§25.14-D）：续轮把客户端回传的 assistant 正文规范化进历史前缀，交付侧就
// 必须把本 HTTP 实际写出的那段正文按同一表示登记，否则同一逻辑内容两侧摘要不同。
// 基线按会话累计：上一批交付之后、下一批交付之前产生的正文只属于下一批，绝不回填。
func (r *cursorSessionRecord) batchAssistantText(content string) string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	base := r.assistantTextBase
	r.mu.Unlock()
	if base < 0 || base > len(content) {
		return content
	}
	return content[base:]
}

// advanceBatchAssistantText 在批次准备成功后把交付基线推进到当前正文末尾。
func (r *cursorSessionRecord) advanceBatchAssistantText(content string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.assistantTextBase = len(content)
	r.mu.Unlock()
}

// cursorContentWhileWaitingTools 判断本批字节是否构成"waiting_tools 期间的新模型正文"。
//
// 判定只看两件事，不新增状态机：
//  1. 会话当前阶段：只有 waiting_tools 期间才算违约（running 是本 HTTP 正常输出阶段）；
//  2. 本批字节里是否有**正文增量**：控制帧（finish/[DONE]）、用量帧与错误帧都不算正文，
//     因此"交付工具批本身的收尾帧"与"真实终态的 usage"不会被误判。
func cursorContentWhileWaitingTools(session *cursorSessionRecord, chunk []byte) bool {
	if session == nil || len(chunk) == 0 {
		return false
	}
	session.mu.Lock()
	waiting := session.state == cursorSessionWaitingTools
	session.mu.Unlock()
	if !waiting {
		return false
	}
	return cursorChunkCarriesModelContent(chunk)
}

// cursorChunkCarriesModelContent 判断一段客户端协议字节是否携带模型正文增量。
//
// 只认 choices[].delta.content 非空：role 头帧、tool_calls 增帧、finish 帧、usage 帧
// 都不含正文，不能算作"新模型正文"。
func cursorChunkCarriesModelContent(chunk []byte) bool {
	for _, payload := range cursorSSEDataPayloads(chunk) {
		if payload == "" || payload == cursorSSEDoneSentinel {
			continue
		}
		if content := gjson.Get(payload, "choices.0.delta.content"); content.Exists() && content.String() != "" {
			return true
		}
	}
	return false
}

// runCursorAgentRunPump 是每会话唯一的背景读循环（§25.3「每会话一个读循环」）。
//
// 读取原生 AgentRun 响应，解析为客户端协议字节并扇出给当前附着的 HTTP；捕获 mcp_args 委托时
// 吐出 tool_calls + finish + [DONE]（委托检查点）并暂停等待续轮；收到 turn_ended / 传输错误 /
// 终态时关闭会话（finalizer，W2-D2）。写路径串行（duplex.writeMu 保护）。
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
	// prepared 是本轮检查点已完成的待提交批（在**发布含工具批的字节之前**准备好）。
	var prepared *cursorPreparedBatch
	for {
		n, rerr := stream.Read(buf)
		if n > 0 {
			// §25.17.2-3：含工具批的字节发布之前先完成全部可失败准备（封批/约束/索引冲突）。
			// 失败时不发布该批字节（客户端永远收不到"正常的工具结束帧"），沿失败链退出。
			if stream.delegated && prepared == nil {
				batch, derr := s.cursorDeliverBatchCheckpoint(session, parser)
				if derr != nil {
					session.noteStage("batch_delivery_failed")
					output.end(derr)
					s.finishCursorSession(session, cursorPumpTerminalReason(derr))
					return
				}
				prepared = batch
			}
			chunk := append([]byte(nil), buf[:n]...)
			// §25.17.2-1 末段：waiting_tools 期间不得再出现模型正文。出现即阶段契约违约，
			// 沿唯一错误出口明确协议失败（不发布该正文、不缓存、不重放）。
			if cursorContentWhileWaitingTools(session, chunk) {
				protocolErr := fmt.Errorf("%w: session %q", ErrCursorContentWhileWaitingTools, session.id)
				session.noteStage("content_while_waiting_tools")
				output.end(protocolErr)
				s.finishCursorSession(session, "content_while_waiting_tools")
				return
			}
			output.publish(&cursorOutputEvent{data: chunk})
			// RL-STOP：首次发布正文字节即置位，供限流路径判定是否允许换号。
			session.mu.Lock()
			session.bodyPublished = true
			session.mu.Unlock()
		}
		if rerr != nil {
			if stream.delegated {
				// 委托检查点：本轮 HTTP 已拿到 tool_calls，暂停读循环等待续轮写回结果。
				// 复位 delegated/done 以便续轮恢复后继续读取恢复后的输出（不重复吐 tool_calls）；
				// 同时复位"本 HTTP 已发过 role 包"状态，让续轮那个新 HTTP 响应自带协议头。
				stream.delegated = false
				stream.done = false
				parser.resetTurnStart()
				// 向附着 HTTP 投递委托检查点事件（携带已准备的批）：drain 写完协议结束标记后
				// 才在服务锁内提交 waiting_tools（写完整写入 = 交付确认）。
				output.publish(&cursorOutputEvent{isDeleg: true, prepared: prepared})
				prepared = nil
				continue
			}
			// 真实终态：turn_ended / 传输错误 / 客户端断连。记录 usage 并关闭
			// 会话（finalizer）。usage 只可能来自真实 turn_ended：没见到就不结算、不写零值。
			session.noteTurnUsage(parser.usage, "pump_terminal")
			// 本 HTTP 的原生读取到此结束（stream 与 nonstream 同一条读循环）。
			// 外审建议 S1：把"最近一次封口失败原因"落到终态日志安全字段 seal_blocked_reason，
			// 供事故复盘区分"缺 listed / 计数不等 / 某 started 无委派"。原因已在 next() 的
			// 交接判定处记录（每帧覆盖、成功即清空），此处只搬运、不改控制流；无值则空串不输出。
			sealReason := stream.lastSealErrReason()
			session.mu.Lock()
			if sealReason != nil {
				session.sealBlockedReason = sealReason.Error()
			}
			session.mu.Unlock()

			opts := session.httpOptions()
			// outcome 必须如实反映终态：此前无条件写 "ok"，导致所有"读到一半终止"的
			// 会话在复盘时都显示成功解析，真因只能靠猜。这里给出稳定安全类别（正常
			// turn_ended 收尾仍记 "ok"，与既有语义一致），原始错误文案仍不落盘。
			termOutcome := "ok"
			if !errors.Is(rerr, io.EOF) {
				termOutcome = cursorStreamErrClass(rerr)
			}
			parseDoneFields := []zap.Field{
				zap.String("outcome", termOutcome),
				zap.Bool("stream", opts.stream),
			}
			if sealReason != nil {
				parseDoneFields = append(parseDoneFields, zap.String("seal_blocked_reason", sealReason.Error()))
			}
			// 帧级诊断（CUR-EXEC-03 根因定位）：终态是协议错误时，附带
			// protocol_frame_class / protocol_frame_index，直接把"哪一类帧、第几帧"写进
			// 终态日志；非帧级错误不追加任何字段。
			parseDoneFields = append(parseDoneFields, cursorFrameErrFields(rerr)...)
			cursorPhaseLog(session.phaseContext(), zapcore.InfoLevel, "response_parse_done", opts.model,
				parseDoneFields...)
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
// 与 cursorSessionTerminalReason 同一口径：只区分正常完成 / 客户端断连 /
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
		return nil, newCursorRequestError(infraerrors.CursorReqMissingModelCode, http.StatusBadRequest, "chat completions request carries no model")
	}
	plan, planErr := cursorPlanFromChat(&ccReq, model)
	if planErr != nil {
		if errors.Is(planErr, ErrCursorResumeUnsupportedContent) {
			return nil, newCursorResumeError(http.StatusBadRequest, "unsupported_content", planErr)
		}
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
		return nil, newCursorRequestError(infraerrors.CursorExecInternalErrorCode, http.StatusInternalServerError, "nil responses request")
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, newCursorRequestError(infraerrors.CursorReqMissingModelCode, http.StatusBadRequest, "responses request carries no model")
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
		if errors.Is(planErr, ErrCursorResumeUnsupportedContent) {
			return nil, newCursorResumeError(http.StatusBadRequest, "unsupported_content", planErr)
		}
		return nil, planErr
	}
	plan.logSkippedItems(ctx, req.Model)
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
		return nil, newCursorRequestError(infraerrors.CursorExecInternalErrorCode, http.StatusInternalServerError, "nil anthropic request")
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, newCursorRequestError(infraerrors.CursorReqMissingModelCode, http.StatusBadRequest, "anthropic messages request carries no model")
	}
	plan, planErr := cursorPlanFromMessages(req, req.Model)
	if planErr != nil {
		if errors.Is(planErr, ErrCursorResumeUnsupportedContent) {
			return nil, newCursorResumeError(http.StatusBadRequest, "unsupported_content", planErr)
		}
		return nil, planErr
	}
	mode, name, modeErr := cursorToolChoiceMode(req.ToolChoice)
	if modeErr != nil {
		return nil, modeErr
	}
	ccReq, err := apicompat.AnthropicToChatCompletionsRequest(req)
	if err != nil {
		return nil, fmt.Errorf("cursor: convert anthropic messages request: %w", err)
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

// cursorExecuteGeminiNative 是 Gemini v1beta 原生出口的**类型化**入口（§25.17.2）。
//
// 顺序固定：先从**原始 Gemini 请求**生成上下文计划与工具语义（tools.functionDeclarations、
// toolConfig.functionCallingConfig 模式），再转执行所需形状，最后交同一执行器的 Gemini
// writer 逐帧写出。工具选择无法表达时在这里就明确 400，零 AgentRun。
func cursorExecuteGeminiNative(
	ctx context.Context,
	c *gin.Context,
	exec *CursorExecutionService,
	account *Account,
	originalModel string,
	stream bool,
	body []byte,
) (*ForwardResult, error) {
	if err := requireCursorExecution(exec); err != nil {
		return nil, err
	}
	if strings.TrimSpace(originalModel) == "" {
		return nil, newCursorRequestError(infraerrors.CursorReqMissingModelCode, http.StatusBadRequest, "native request carries no model")
	}
	// ① 原始请求 → 计划（含工具名与 toolConfig 摘要）。
	plan, planErr := cursorPlanFromGemini(body, originalModel)
	if planErr != nil {
		return nil, planErr
	}
	// ② 工具选择语义映射（未知模式/未声明名称在此 400，未出站）。
	mode, name, choiceErr := cursorGeminiToolChoiceMode(body)
	if choiceErr != nil {
		return nil, choiceErr
	}
	// ③ 转执行所需形状（工具声明/调用/结果的真实进入转换，不只进摘要）。
	ccBody, convErr := cursorGeminiRequestToChatCompletions(body, originalModel, stream)
	if convErr != nil {
		return nil, newCursorRequestError(infraerrors.CursorReqUnsupportedMessageCode, http.StatusBadRequest, "%v", convErr)
	}
	var chat apicompat.ChatCompletionsRequest
	if err := json.Unmarshal(ccBody, &chat); err != nil {
		return nil, fmt.Errorf("cursor: decode converted gemini request: %w", err)
	}
	if err := cursorValidateToolChoice(&chat); err != nil {
		return nil, err
	}
	return exec.Execute(ctx, c, account, &CursorExecutionRequest{
		Chat:           &chat,
		Model:          originalModel,
		Stream:         stream,
		StartTime:      time.Now(),
		Protocol:       cursorProtocolGemini,
		IndexProtocol:  string(CursorResumeProtocolGemini),
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

// cursorKeyRejectedReason 标识「cursor 上游明确拒付此密钥（过期/额度耗尽）」的换号原因。
//
// 与 CursorUpstreamAccountFailureReason 必须严格区分：后者是"账号当下不可服务、临时停调等
// 探针恢复"，会进 TempUnscheduleRetryableError 写探针 marker；前者是"上游明确不要这个号了"，
// 只做永久停用（schedulable=false），不写任何停调、不进熔断计数、不被探针恢复。
// 它也不在 failover_loop.go:276-280 的停调白名单里 ⇒ 拒钥不会写停调。
const cursorKeyRejectedReason = GatewayFailureReason("cursor_key_rejected")

// IsCursorKeyRejectedReason 暴露给 handler 层的拒钥判据：当且仅当 reason 为
// cursorKeyRejectedReason（"cursor_key_rejected"）时为真。
//
// 语义边界（与 failover_loop.go:276-280 停调白名单严格隔离）：拒钥只做永久停用、不写停调，
// 因此本函数**绝不可**被加进停调白名单；它仅用于「换号耗尽 → 中文终态 CUR-RES-11」等
// 对外映射判定（见 openai_gateway_handler.go handleFailoverExhausted）。
//
// 注：该函数是 A7 卡在 handler 侧引用、却漏定义的导出入口；此处随 A8 卡在常量同文件补齐，
// 使 `go build ./...` 与 handler 测试恢复可编译，不改动任何禁区文件。
func IsCursorKeyRejectedReason(reason GatewayFailureReason) bool {
	return reason == cursorKeyRejectedReason
}

// resolveProbeMaxTokens 解析 ProbeAgentRun 的输出预算变参。
//
// 取值口径（写死）：len==0 ⇒ 16（与历史逐字一致，保持既有账号测试行为）；
// len>0 ⇒ 取第一个值；传 ≤0 ⇒ 回落 16（防传 0 导致探针无输出）。
func resolveProbeMaxTokens(maxTokens ...int) int {
	if len(maxTokens) > 0 && maxTokens[0] > 0 {
		return maxTokens[0]
	}
	return 16
}

// ProbeAgentRun 用本执行部件的同一 transport / 鉴权 / 转换跑一次最小 AgentRun，
// 供账号探测复用。调用方负责模型闸门与展示格式；本函数只做真实出站与解析。
//
// 返回：上游状态码、上游错误摘要（已脱敏）、解析出的真实文本。
func (s *CursorExecutionService) ProbeAgentRun(
	ctx context.Context,
	account *Account,
	upstreamModel string,
	maxTokens ...int,
) (status int, errDetail, text string, err error) {
	if s == nil {
		return 0, "", "", ErrCursorExecutionUnavailable
	}
	if !isCursorAccount(account) {
		return 0, "", "", errors.New("cursor: probe requires a cursor platform account")
	}
	if strings.TrimSpace(upstreamModel) == "" {
		return 0, "", "", newCursorRequestError(infraerrors.CursorExecModelMappingInvalidCode, http.StatusInternalServerError, "empty upstream model")
	}
	bearer, err := cursorResolveAccountBearer(account)
	if err != nil {
		return 0, "", "", err
	}
	authProvider, err := newCursorAccountAuthProvider(account, s.quotaKeys, s.enc, s.deviceIDs)
	if err != nil {
		return 0, "", "", err
	}
	// 输出预算：默认 16（与历史逐字一致，保持既有账号测试行为）；探针侧显式传 ≥256
	// 以避开思考型模型把预算先花在 reasoning 上导致"正文一个字都轮不到"的探针恒失败
	// （生产实证：16/24/32 → 502 upstream_empty_result，48~256 → 200）。
	// 传 ≤0 回落 16，防传 0 导致探针无输出。
	probeMaxTokens := resolveProbeMaxTokens(maxTokens...)
	chatReq := &apicompat.ChatCompletionsRequest{
		Model:     upstreamModel,
		MaxTokens: &probeMaxTokens,
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
	// responseID 是本 HTTP 的唯一输出响应 ID（§25.17.2「实际 ID」）。
	responseID string
	// terminal 表示本次 finalize 对应真实终态（而不是工具批交付检查点）。
	terminal bool
	// svc 与 session 供本 writer 在把工具批字节写给 HTTP **之前**完成 item
	// Register+冲突检查（§25.17.2-3/4：锁内准备 → 锁外写完整批 → 锁内提交 delivered）。
	// 任何工具批字节到达客户端之前，本批工具结果条目 ID 必须已登记进统一索引。
	svc     *CursorExecutionService
	session *cursorSessionRecord

	// 流式：未消费的 SSE 片段 + 目标协议状态机。
	pending   []byte
	anthState *apicompat.ChatCompletionsToAnthropicStreamState
	respState *apicompat.ChatCompletionsToResponsesStreamState
	started   bool
	// Gemini 出口的流式工具增量累加（按 index），在终结帧一次性写出 functionCall parts。
	geminiCalls []cursorGeminiStreamCall
	// geminiUsage 是本 HTTP 终止 chunk 携带的真实 usage（仅真实终态写 usageMetadata）。
	geminiUsage *apicompat.ChatUsage

	// 非流式：整条 chat.completion JSON。
	completion bytes.Buffer

	// itemIDs 是本 HTTP **实际交付**的 Responses 工具结果条目 ID（按出现顺序去重）。
	//
	// §25.17.2「实际ID」：item ID 只从实际写出的内容取——流式取已发出的 output_item
	// 事件、非流式取自**实际构建**的响应对象（不读 stream respState、不另生成一份）。
	// drain 在写出成功后把它们交给 commitDeliveredBatch 登记进同一索引。
	itemIDs []string
	// itemIDSeen 是 itemIDs 的去重集合（同一 item 会出现在 added/done 两个事件里）。
	itemIDSeen map[string]struct{}

	written     int
	status      int
	wroteHeader bool
}

// responsesToolItemTypes 是"承载对外工具结果"的 Responses 输出项类型。
//
// 只有这类条目的 ID 需要登记：客户端下一轮把它作为 input 条目回传（item id 锚点），
// message/reasoning 等正文项不是工具结果锚点。
var responsesToolItemTypes = map[string]struct{}{
	"function_call":    {},
	"custom_tool_call": {},
	"tool_search_call": {},
	"local_shell_call": {},
	"mcp_call":         {},
}

// noteToolItemID 记录一条实际交付的工具结果条目 ID（重复出现只记一次）。
func (w *cursorProtocolWriter) noteToolItemID(itemType, id string) {
	if w == nil {
		return
	}
	if _, ok := responsesToolItemTypes[strings.ToLower(strings.TrimSpace(itemType))]; !ok {
		return
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	if w.itemIDSeen == nil {
		w.itemIDSeen = make(map[string]struct{})
	}
	if _, dup := w.itemIDSeen[id]; dup {
		return
	}
	w.itemIDSeen[id] = struct{}{}
	w.itemIDs = append(w.itemIDs, id)
}

// noteResponsesEventsItemIDs 从**实际发出**的 Responses 事件里取工具结果条目 ID。
func (w *cursorProtocolWriter) noteResponsesEventsItemIDs(events []apicompat.ResponsesStreamEvent) {
	for _, event := range events {
		if event.Item != nil {
			w.noteToolItemID(event.Item.Type, event.Item.ID)
		}
	}
}

// collectedItemIDs 返回本 HTTP 实际交付的工具结果条目 ID 快照。
func (w *cursorProtocolWriter) collectedItemIDs() []string {
	if w == nil || len(w.itemIDs) == 0 {
		return nil
	}
	return append([]string(nil), w.itemIDs...)
}

// registerDeliveredItems 在本 HTTP 把工具批字节写给客户端**之前**完成已收集 item ID 的
// 统一索引登记与冲突检查（§25.17.2-3/4）。只对 Responses 出口（有 item ID 收集）有作用；
// 同一批已在先前 chunk 写前登记过的 ID 由 registerItemIDBatch 幂等跳过。冲突即返回原
// 错误：调用方（writeChunk）据此在任何含该 item 的 SSE 帧写出之前终止。
func (w *cursorProtocolWriter) registerDeliveredItems() error {
	if w == nil || w.svc == nil || w.session == nil {
		return nil
	}
	ids := w.collectedItemIDs()
	if len(ids) == 0 {
		return nil
	}
	return w.svc.registerLiveItemIDs(w.session, ids)
}

// cursorGeminiStreamCall 是 Gemini 流式出口累加的一条 functionCall。
type cursorGeminiStreamCall struct {
	ID   string
	Name string
	Args strings.Builder
}

// newCursorProtocolWriter 构造回桥 writer，并把工具元数据交给 Responses 状态机。
//
// §24.2-5：流式 state 与非流式转换**必须**带着 EffectiveResponsesTools 派生的
// CustomToolNames / FunctionToolNames / NamespaceToolNames / HasToolSearchTool，
// 不得再传 nil/nil/false/nil（那会丢 Codex 的 custom / namespace 工具类型）。
// responseID 是本 HTTP 的唯一输出响应 ID：目标协议状态机的初始 ID 也取它，
// 使 response.created/completed 与非流式 response.id 与实际交付一致。
func newCursorProtocolWriter(
	parent gin.ResponseWriter,
	stream bool,
	mode cursorProtocol,
	model string,
	tools cursorResponsesToolMeta,
	responseID string,
	svc *CursorExecutionService,
	session *cursorSessionRecord,
) *cursorProtocolWriter {
	w := &cursorProtocolWriter{
		ResponseWriter: parent,
		header:         make(http.Header),
		mode:           mode,
		model:          model,
		stream:         stream,
		tools:          tools,
		responseID:     strings.TrimSpace(responseID),
		svc:            svc,
		session:        session,
	}
	switch mode {
	case cursorProtocolAnthropic:
		w.anthState = apicompat.NewChatCompletionsToAnthropicStreamState(model)
		if w.responseID != "" {
			w.anthState.ResponseID = w.responseID
		}
	case cursorProtocolResponses:
		w.respState = cursorResponsesStreamState(model, tools)
		if w.responseID != "" {
			w.respState.ResponseID = w.responseID
		}
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
		events := apicompat.ChatCompletionsChunkToResponsesEvents(chunk, w.respState)
		// 实际交付的条目 ID 在写出之前就从本批事件里取：写出失败不会留下"未交付却登记"的 ID。
		w.noteResponsesEventsItemIDs(events)
		// §25.17.2-3/4：任何工具批字节写给客户端之前，先（服务锁内）完成 item Register
		// 与冲突检查。本批实际 item ID 已由上面的转换确定；冲突时返回原错误，绝不把含该
		// item 的任一 SSE 帧写给客户端（零成功字节），符合探针"首个工具字节写出前索引已登记"。
		if err := w.registerDeliveredItems(); err != nil {
			return err
		}
		for _, event := range events {
			sse, err := apicompat.ResponsesEventToSSE(event)
			if err != nil {
				return err
			}
			if _, err := io.WriteString(w.ResponseWriter, sse); err != nil {
				return err
			}
		}
	case cursorProtocolGemini:
		return w.writeGeminiChunk(chunk)
	}
	return nil
}

// writeGeminiChunk 把一条 Chat 增量写成 Gemini SSE 帧（每个可输出增量一帧并立即 Flush）。
//
// 工具增量按 index 累加，在终结帧一次性写成 functionCall parts（保留原 call ID/名称/args）。
func (w *cursorProtocolWriter) writeGeminiChunk(chunk *apicompat.ChatCompletionsChunk) error {
	w.ensureStarted()
	if chunk != nil && chunk.Usage != nil {
		w.geminiUsage = chunk.Usage
	}
	if chunk == nil || len(chunk.Choices) == 0 {
		return nil
	}
	choice := chunk.Choices[0]
	for _, tc := range choice.Delta.ToolCalls {
		index := 0
		if tc.Index != nil {
			index = *tc.Index
		}
		for len(w.geminiCalls) <= index {
			w.geminiCalls = append(w.geminiCalls, cursorGeminiStreamCall{})
		}
		if tc.ID != "" {
			w.geminiCalls[index].ID = tc.ID
		}
		if tc.Function.Name != "" {
			w.geminiCalls[index].Name = tc.Function.Name
		}
		w.geminiCalls[index].Args.WriteString(tc.Function.Arguments)
	}
	if choice.Delta.Content == nil || *choice.Delta.Content == "" {
		return nil
	}
	if err := w.writeGeminiFrame(map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{
				"role":  "model",
				"parts": []any{map[string]any{"text": *choice.Delta.Content}},
			},
			"index": 0,
		}},
		"modelVersion": w.model,
	}); err != nil {
		return err
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

// writeGeminiFrame 写出一帧 Gemini SSE data 帧。
func (w *cursorProtocolWriter) writeGeminiFrame(frame map[string]any) error {
	payload, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("cursor: marshal gemini stream frame: %w", err)
	}
	if _, err := io.WriteString(w.ResponseWriter, "data: "); err != nil {
		return err
	}
	if err := cursorWriteAll(w.ResponseWriter, payload); err != nil {
		return err
	}
	_, err = io.WriteString(w.ResponseWriter, "\n\n")
	return err
}

// finalizeGeminiStream 写出 Gemini 流式的收尾（functionCall parts + 带 finishReason 的末帧）。
//
// usageMetadata 只在**真实终态**写（工具批交付检查点不写，避免中间造最终 token）。
func (w *cursorProtocolWriter) finalizeGeminiStream() error {
	for _, call := range w.geminiCalls {
		args := strings.TrimSpace(call.Args.String())
		if args == "" {
			args = "{}"
		}
		functionCall := map[string]any{
			"name": call.Name,
			"args": json.RawMessage(args),
		}
		if call.ID != "" {
			functionCall["id"] = call.ID
		}
		if err := w.writeGeminiFrame(map[string]any{
			"candidates": []any{map[string]any{
				"content": map[string]any{
					"role":  "model",
					"parts": []any{map[string]any{"functionCall": functionCall}},
				},
				"index": 0,
			}},
			"modelVersion": w.model,
		}); err != nil {
			return err
		}
	}
	w.geminiCalls = nil
	final := map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"role": "model", "parts": []any{}},
			"finishReason": "STOP",
			"index":        0,
		}},
		"modelVersion": w.model,
	}
	if w.terminal && w.geminiUsage != nil {
		final["usageMetadata"] = cursorGeminiUsageMetadataFromChat(*w.geminiUsage)
	}
	if err := w.writeGeminiFrame(final); err != nil {
		return err
	}
	w.Flush()
	return nil
}

// finalizeGeminiCompletion 把整条 chat.completion 转成 Gemini generateContent 响应。
func (w *cursorProtocolWriter) finalizeGeminiCompletion() error {
	body := w.completion.Bytes()
	var resp apicompat.ChatCompletionsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("cursor: decode bridged chat completion: %w", err)
	}
	parts := make([]any, 0, 2)
	finishReason := ""
	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		finishReason = choice.FinishReason
		if text := cursorChatMessageText(choice.Message.Content); text != "" {
			parts = append(parts, map[string]any{"text": text})
		}
		for _, call := range choice.Message.ToolCalls {
			args := strings.TrimSpace(call.Function.Arguments)
			if args == "" {
				args = "{}"
			}
			functionCall := map[string]any{
				"name": call.Function.Name,
				"args": json.RawMessage(args),
			}
			if call.ID != "" {
				functionCall["id"] = call.ID
			}
			parts = append(parts, map[string]any{"functionCall": functionCall})
		}
	}
	out := map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"role": "model", "parts": parts},
			"finishReason": cursorGeminiFinishReason(finishReason),
			"index":        0,
		}},
		"modelVersion": w.model,
	}
	if resp.ID != "" {
		out["responseId"] = resp.ID
	}
	if usage := cursorBridgedChatUsage(body); usage != nil {
		out["usageMetadata"] = cursorGeminiUsageMetadataFromChat(*usage)
	}

	data, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("cursor: marshal gemini response: %w", err)
	}
	h := w.ResponseWriter.Header()
	h.Set("Content-Type", "application/json")
	if rid := strings.TrimSpace(w.header.Get("x-request-id")); rid != "" {
		h.Set("x-request-id", rid)
	}
	w.ResponseWriter.WriteHeader(http.StatusOK)
	return cursorWriteAll(w.ResponseWriter, data)
}

// cursorGeminiUsageMetadataFromChat 把真实 Chat usage 落到 Gemini usageMetadata 唯一位置。
func cursorGeminiUsageMetadataFromChat(u apicompat.ChatUsage) map[string]any {
	total := u.TotalTokens
	if total == 0 {
		total = u.PromptTokens + u.CompletionTokens
	}
	return map[string]any{
		"promptTokenCount":     u.PromptTokens,
		"candidatesTokenCount": u.CompletionTokens,
		"totalTokenCount":      total,
	}
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
		// DSML 观测：本次流里上游的 DSML 工具调用还原/丢弃情况（统一出口，见 dsml_restore_observability.go）。
		if restored, dropped := w.anthState.DSMLStats(); restored > 0 || dropped > 0 {
			logDSMLRestore(context.Background(), w.model, restored, dropped)
		}
		for _, event := range apicompat.FinalizeChatCompletionsAnthropicStream(w.anthState) {
			sse, err := apicompat.ResponsesAnthropicEventToSSE(event)
			if err != nil {
				return err
			}
			events = append(events, sse)
		}
	case cursorProtocolResponses:
		if restored, dropped := w.respState.DSMLStats(); restored > 0 || dropped > 0 {
			logDSMLRestore(context.Background(), w.model, restored, dropped)
		}
		respEvents := apicompat.FinalizeChatCompletionsResponsesStream(w.respState)
		w.noteResponsesEventsItemIDs(respEvents)
		for _, event := range respEvents {
			sse, err := apicompat.ResponsesEventToSSE(event)
			if err != nil {
				return err
			}
			events = append(events, sse)
		}
	case cursorProtocolGemini:
		return w.finalizeGeminiStream()
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
		built := apicompat.ChatCompletionsResponseToResponses(
			&resp, w.model,
			w.tools.customTools, w.tools.functionTools, w.tools.toolSearch, w.tools.namespaceTools,
		)
		// §25.17.2「实际ID」：非流式 item ID 从**实际构建的响应对象**取（不读 stream
		// respState，也不另生成一份），保证索引里登记的就是客户端真的收到的那个 ID。
		if built != nil {
			for _, item := range built.Output {
				w.noteToolItemID(item.Type, item.ID)
			}
			// §25.17.2-3/4：非流式 item ID 已从实际构建的响应对象 note 完毕，写出 200 之前
			// 先（服务锁内）完成统一索引登记与冲突检查；冲突时返回原错误，绝不写出成功响应体
			// （零成功字节）。与流式路径 writeChunk 的 registerDeliveredItems 同语义、幂等跳过同批。
			if err := w.registerDeliveredItems(); err != nil {
				return err
			}
		}
		out = built
	case cursorProtocolGemini:
		return w.finalizeGeminiCompletion()
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

	// ---- §25.17.3「A05首次来源指纹」：两个内部字段，进程内专用 ----
	//
	// sourceQuotaKeyID / sourceCodeMAC 取自**首次生成 x-jg-auth 那一次**的来源行与解密
	// 结果（经 cursorAccountAuthProvider 随材料一同传出），不是生成头之后另读一次来源
	// 反推。故换码夹在"生成 auth"与"建 session"之间时，本会话仍固定着旧头的指纹。
	//
	// 绝不进日志、DTO、工具 ID 或任何对外文本；后续资料读取拿不到可靠指纹时必须明确
	// 失败，不以空值代表同源（见 cursorSessionSourceFingerprint 的失败关闭）。
	sourceQuotaKeyID int64
	sourceCodeMAC    string
}

// cursorSessionSourceFingerprint 取会话固定的首次来源指纹。
//
// 指纹不可靠（未固定，或任一字段缺失）即返回明确错误：调用方**不得**以空值代表
// "与首次鉴权同源"，也不得退化为只看 QuotaKeyID 的弱比较。
func cursorSessionSourceFingerprint(spec cursorSessionSpec) (cursorSourceFingerprint, error) {
	fp := cursorSourceFingerprint{QuotaKeyID: spec.sourceQuotaKeyID, CodeMAC: spec.sourceCodeMAC}
	if !fp.valid() {
		return cursorSourceFingerprint{}, fmt.Errorf("%w: session has no fixed source fingerprint", ErrCursorSourceFingerprintUnavailable)
	}
	return fp, nil
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
	// prepared 是委托检查点已完成的服务锁内准备（交付确认后由 drain 提交）。
	prepared *cursorPreparedBatch
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
	// respID 是**当前附着 HTTP** 的输出响应 ID（§25.17.2「实际 ID」）。
	//
	// 每个 HTTP 一份唯一 ID：该 HTTP 的所有 chunk、response.created/completed 与非流式
	// response.id 共用它；不同会话、不同 HTTP 绝不复用（每次附着重新生成）。
	respID string
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
	// 每次附着生成一份新的输出响应 ID：不同 HTTP/会话绝不复用（§25.17.2 实际 ID）。
	o.respID = cursorNewUUID()
	o.mu.Unlock()
	detach := func() {
		o.mu.Lock()
		if !o.attached {
			o.mu.Unlock()
			return
		}
		o.attached = false
		o.respID = ""
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

// responseID 返回当前附着 HTTP 的输出响应 ID（无附着时为空）。
func (o *cursorSessionOutput) responseID() string {
	if o == nil {
		return ""
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.respID
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

	// deliveredToolCalls 是本会话**已交付**的 Responses 工具调用项语义（wireID → 语义）。
	//
	// 续轮登记结果（appendCursorDeliveredResultsToHistory）与历史登记
	// （appendCursorBatchAssistantHistory 的 Responses 分支）都按此选结果/历史条目类型；
	// finalizer 随会话清理。只存类型与名字，不存正文/参数（§25.14-D 最小表示）。
	// 键是发给客户端的 wireID（与客户端回传的 call_id 同源）。
	deliveredToolCalls map[string]cursorDeliveredToolCall
	// responsesTools 是首轮声明的 Responses 工具元数据（custom / function / namespace /
	// tool_search）；历史登记据此把交付侧捕获还原为正确的实际类型，不另造判定。
	responsesTools cursorResponsesToolMeta

	// pumpEnded 标记背景读循环（每会话一个读循环）是否已结束（终态/传输错误）。
	// Execute 返回时据此判定会话是否仍存活跨 HTTP：未结束则不触发 finalize。
	pumpEnded bool

	// bodyPublished 标记本 HTTP 是否已向客户端发布过任何正文字节（RL-STOP）。
	// 在 pump 首次 publish 正文数据时置位；用于限流路径判定是否允许换号——
	// 已发布字节则禁止 mid-stream 换号，错误原样返回。
	bodyPublished bool

	// ---- §25.4 并发槽 / 预留 / 终止清理（全部字段在 mu 下读写）----

	// createdAt 是会话建立时刻；billingEventID 是**服务端生成、会话级不可变**的
	// 账单事件 ID（§25.5），建会话时固定，任何 HTTP request_id 都不得替代它。
	createdAt      time.Time
	billingEventID string
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
	// sealBlockedReason 缓存读循环终态时"最近一次封口失败原因"（外审建议 S1 的终态
	// 可观测性）：由 runCursorAgentRunPump 在终止分支从 stream.lastSealErrReason()
	// 搬运而来（无则空串）。settleCursorSession 在 session_settle_skipped 时随同一安
	// 全字段写出，便于事故复盘区分"缺 listed / 计数不等 / 某 started 无委派"。
	// 纯观测数据，不改变任何控制流、不引入配置/新机制。
	sealBlockedReason string
	// finishing 标记 finalizer 的唯一胜者已确定；steps 是终结守卫（每项至多一次）。
	finishing bool
	steps     cursorFinalizerSteps
	// finalizeDone 是**会话建立即初始化**的一次性完成 channel（§25.17.7 A09）。
	//
	// 只有 finalizer 的唯一胜者在"任务提交 / 资源归还 / 索引清理"全部结束后关闭它；
	// 重复终结者（finishing 已置位的后来者）不关闭。Stop 在会话锁**外**等待该信号，
	// 因为 finishing=true 只表示胜者已确定，不表示结算已提交、槽位已归还。
	//
	// 该字段在 start 时固定，之后不再替换，故读取它本身不需要持 mu。
	finalizeDone chan struct{}
	// finalizeOnce 保证 finalizeDone 至多关闭一次（重复终结者不关闭）。
	finalizeOnce sync.Once
	// streamClose 关闭原生流/取消读写（finalizer 调用至多一次）。
	streamClose func()
	// httpAttachStop 停止当前 HTTP 附着期取消回调；attached 表示是否有 HTTP 附着。
	httpAttachStop func() bool
	attached       bool
	// waitingIdleTimer 是 waiting_tools 空闲释放计时器（§2.5，用户 2026-11-01 裁定 B）。
	//
	// 交付完整工具批后会话停在 waiting_tools 等客户端续轮；客户端按 esc 中断或退出时
	// 无人续轮，会话既等不到真实终态也等不到取消，用户槽/账号槽与余额预留会一直被持有
	// （2026-11-04 生产实证：3 个槽挂死 → 该账号每 2 分钟撞一次 429）。
	// 到期且 pending 仍未消费即走唯一 finalizer（reason=waiting_tools_idle）。
	// 全部读写都在 record.mu 下。
	waitingIdleTimer *time.Timer
	// owner 是会话表的唯一所有者（finalizer/结算入口），只在取消回调里使用。
	owner *CursorExecutionService
	// unavailable 标记会话在"建表后、绑定 owner 前"被 Stop 作废（§25.17.7 A09 登记空隙）。
	// 此时 Stop 已对无 owner 游离记录标记失效；attachHTTP 必须明确拒绝，避免 Stop 之后仍
	// 可被附着执行（零 AgentRun、零游离状态）。只删表会留下可被 attach 的游离记录（返回 nil）。
	unavailable bool

	// ---- §25.14-D 摘要校验与续轮接管（全部字段在 mu 下读写）----

	// configDigest 是**硬组**配置摘要（模型 + 工具声明/schema）；续轮不一致即拒绝。
	// configSystemDigest / configSamplingDigest 是**软组**摘要（system 文本 / 采样参数）：
	// 续轮只比对留痕（resume_config_drift），不拒绝——见 ResumeCursorSession 的判定说明。
	// historyDigest 是**已交付**输入历史的链式摘要，logicalItems 即其逻辑项数。
	// 全部按 CR-B 引擎生成：本层只提供 canonical 项，不改引擎语义。
	configDigest         *CursorHistoryDigest
	configSystemDigest   *CursorHistoryDigest
	configSamplingDigest *CursorHistoryDigest
	historyDigest        *CursorHistoryDigest
	// exposedTools 是本会话已暴露给客户端的工具名（保持声明顺序），
	// 供 tool_choice 逐 HTTP 校验"暴露集合是否一致"。
	exposedTools []string
	// instructionsDeclared 表示首轮是否声明了非空 instructions/instructions 等价物。
	// §25.14-D：首轮非空 + 续轮省略 = 明确入参错误（不偷偷继承）。
	instructionsDeclared bool
	// assistantTextBase 是本会话已并入历史摘要的 assistant 正文长度偏移。
	// 交付侧按它切出"本批实际交付正文"，与续轮前缀摘要保持同一表示（摘要对称）。
	assistantTextBase int
	// pending 是**当前交付批次**（等待客户端真实结果）；同一时刻至多一个。
	pending *cursorPendingBatch
	// deliveredBatchSeq 是**已确认交付**到的最大批次序号（cursorToolBatch.seq，初始 0）。
	//
	// 这是"是不是新批"的唯一会话侧判据：只有 seq > deliveredBatchSeq 的已封口批才算
	// 本次 HTTP 的新交付集合。绝不用 call_id / 索引已登记来过滤——真实客户端会跨轮
	// 复用同一个 tool_call_id（生产实测同一会话内 read/write 共用同一 ID），按 ID 过滤
	// 会把本轮新批整体吞掉，交付批为空 → 上游收不到结果 → stream_error。
	//
	// 只在 commitDeliveredBatchState（写出完成后）随 delivered=true 一起推进；准备失败
	// 绝不推进，未交付的批下一轮仍算新批。
	deliveredBatchSeq uint64
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
	// pumpDone 在背景读循环结束时关闭（终态/传输错误），用于通知附着 HTTP 收尾。
	pumpDone chan struct{}
	// resumeCount 是本会话已接纳的续轮次数（日志/诊断安全字段）。
	resumeCount int
	// httpOpt 是**当前 HTTP** 的逐请求选项（stream / model / tool_choice）。
	//
	// §25.17.2-2：续轮不沿用首轮 req.Stream / tool_choice / writer；每次附着都按本 HTTP
	// 重新设置。pump 在做批次可失败准备时读它（不能读首 HTTP 的 req）。
	httpOpt cursorHTTPOptions
	// logCtx 是**仅供阶段日志**使用的请求 ctx（读取 observer / request_id）。
	//
	// 不闭包 gin.Context、不用于任何取消或业务写入；pump/交付边界的阶段事件必须与
	// 本 HTTP 的观测上下文同源，否则阶段日志会落到 background 而丢失。
	logCtx context.Context
}

// cursorDeliveredToolCall 是一条**已交付**的 Responses 工具调用项的语义（不含正文/参数）。
//
// 只记录协议层"这是什么类型的调用、叫什么名字"，供历史登记与结果条目类型选择复用，
// 保证交付侧登记与续轮回传解析读同一套语义（§25.14-D「实际 ID」）。
type cursorDeliveredToolCall struct {
	Kind      string // "function_call" | "custom_tool_call" | "tool_search_call"
	Name      string // 客户端可见名（namespace 子工具为裸名）
	Namespace string // 仅 namespace 子工具非空
}

// cursorResponsesToolCallHistoryCanonical 把一条已交付的 Responses 工具调用项规范化为
// 历史项 canonical 字节。交付侧登记与续轮回传解析必须调用同一函数，保证两侧摘要逐字节
// 相等（§25.17.2「实际 ID」）。
//
// 分不清 kind 的调用 → 失败关闭 ErrCursorResumeUnsupportedContent（绝不回落到 function_call）。
func cursorResponsesToolCallHistoryCanonical(kind, callID, name, namespace, payload string) ([]byte, error) {
	switch kind {
	case "function_call":
		// 普通 function_call 的 arguments 必须是合法 JSON：键序/空白差异视为同值（经
		// cursorCanonicalToolArgs 规范化）；非法/截断 JSON 属于不支持内容，**失败关闭**
		// （绝不把非法 JSON 包成字符串继续摘要，否则会污染历史与续轮前缀比对）。
		canonicalArgs, argsErr := cursorCanonicalToolArgs(payload)
		if argsErr != nil {
			return nil, fmt.Errorf("%w: function_call arguments not valid JSON: %v", ErrCursorResumeUnsupportedContent, argsErr)
		}
		m := map[string]json.RawMessage{
			"call_id":   mustCursorJSON(callID),
			"name":      mustCursorJSON(name),
			"arguments": canonicalArgs,
		}
		if ns := strings.TrimSpace(namespace); ns != "" {
			m["namespace"] = mustCursorJSON(ns)
		}
		return CursorCanonicalJSON(mustCursorJSONValue(m))
	case "custom_tool_call":
		// input 是 custom/freeform 工具的文本输入：原样保留为 JSON 字符串字面量
		// （mustCursorJSON 当字符串编码，不把裸字节塞进 json.RawMessage 导致 marshal 失败）。
		m := map[string]json.RawMessage{
			"call_id": mustCursorJSON(callID),
			"name":    mustCursorJSON(name),
			"input":   mustCursorJSON(payload),
		}
		return CursorCanonicalJSON(mustCursorJSONValue(m))
	case "tool_search_call":
		// arguments 走 JSON 规范化；非合法 JSON 时按文本参与。
		canonicalArgs, argsErr := cursorCanonicalToolArgs(payload)
		if argsErr != nil {
			// 非合法 JSON：当字符串编码为合法 JSON 值。
			canonicalArgs = mustCursorJSON(payload)
		}
		m := map[string]json.RawMessage{
			"call_id":   mustCursorJSON(callID),
			"arguments": canonicalArgs,
		}
		return CursorCanonicalJSON(mustCursorJSONValue(m))
	default:
		return nil, fmt.Errorf("%w: responses tool kind %q", ErrCursorResumeUnsupportedContent, kind)
	}
}

// cursorResponsesToolResultCanonical 把一条 Responses 工具结果项规范化为历史项 canonical 字节。
//
// 与 cursorResponsesToolCallHistoryCanonical 同源：交付侧登记与续轮回传解析都经本函数，
// 保证前缀摘要与续轮登记逐字节相等。result 类型（function_call_output / custom_tool_call_output
// / tool_search_output）由调用方按 kind 选定后作为 itemType 传入。
func cursorResponsesToolResultCanonical(callID, output string) ([]byte, error) {
	m := map[string]json.RawMessage{
		"call_id": mustCursorJSON(callID),
		"output":  mustCursorJSON(output),
	}
	return CursorCanonicalJSON(mustCursorJSONValue(m))
}

// cursorResponsesDeliveredToolCall 从**实际交付捕获**与首轮工具元数据派生该调用的语义。
//
// 复用 apicompat 在回桥侧判定类型的同一组输入（customTools / functionTools /
// namespaceTools / toolSearch 与摊平名映射），不另造判定：与 announceChatToolItem 的
// 类型/名字/namespace 决策完全同源。tool_search 代理名与 apicompat 同源（"tool_search"）。
func cursorResponsesDeliveredToolCall(name string, meta cursorResponsesToolMeta) cursorDeliveredToolCall {
	// custom：customToolCallName 返回 (裸 custom 名, true) 时该调用为 custom_tool_call。
	if customName, ok := cursorCustomToolCallName(name, meta); ok {
		return cursorDeliveredToolCall{Kind: "custom_tool_call", Name: customName}
	}
	// tool_search：声明了 tool_search 服务端工具且名为代理名。
	if meta.toolSearch && name == "tool_search" {
		return cursorDeliveredToolCall{Kind: "tool_search_call", Name: name}
	}
	// namespace 子工具：摊平名命中 namespace 映射 → 还原为裸子工具名 + namespace。
	if ns, ok := meta.namespaceTools[name]; ok {
		return cursorDeliveredToolCall{Kind: "function_call", Name: ns.Name, Namespace: ns.Namespace}
	}
	// 普通 function_call：未命中任何已声明 custom / tool_search / namespace 子工具的名字，
	// 按 apicompat 既有回落视为普通 function_call（与 announceChatToolItem 类型决策逐字同源）。
	// 交付侧与登记侧必须同源，故此处**不失败关闭**——若在此拒绝，会出现"交付=function_call、
	// 登记=报错"的不一致并触发 502。未声明工具调用的拒付由客户端 / 既有链路处理，不在本层。
	return cursorDeliveredToolCall{Kind: "function_call", Name: name}
}

// cursorCustomToolCallName 与 apicompat.customToolCallName 同源：判定给定摊平名是否为
// custom 工具并还原其裸名。此处复制决策以保持服务包内可用（不修改 apicompat）。
func cursorCustomToolCallName(name string, meta cursorResponsesToolMeta) (string, bool) {
	if meta.functionTools[name] {
		return "", false
	}
	if meta.customTools[name] {
		return name, true
	}
	// namespace 子工具占用了这个摊平名：是 custom 子工具就还原成裸名并判 custom_tool_call，
	// 是 function 子工具则保持"真名优先、别名不生效"的原语义。
	if origin, ok := meta.namespaceTools[name]; ok {
		if meta.namespaceCustomTools[name] {
			return origin.Name, true
		}
		return "", false
	}
	match := ""
	for customName := range meta.customTools {
		for _, namespaceTool := range meta.namespaceTools {
			if apicompat.FlattenNamespaceToolName(namespaceTool.Namespace, customName) != name {
				continue
			}
			if match != "" && match != customName {
				return "", false
			}
			match = customName
		}
	}
	return match, match != ""
}

// setLogContext 登记阶段日志用的请求 ctx（不参与取消语义）。
func (r *cursorSessionRecord) setLogContext(ctx context.Context) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.logCtx = ctx
	r.mu.Unlock()
}

// phaseContext 返回该会话阶段日志使用的 ctx。
func (r *cursorSessionRecord) phaseContext() context.Context {
	if r == nil {
		return context.Background()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.logCtx != nil {
		return r.logCtx
	}
	return context.Background()
}

// cursorHTTPOptions 是本 HTTP 的逐请求选项（不跨 HTTP 累积）。
type cursorHTTPOptions struct {
	stream         bool
	model          string
	toolChoiceMode string
	toolChoiceName string
}

// setHTTPOptions 登记本 HTTP 的选项（附着时由 Execute / Resume 各自设置）。
func (r *cursorSessionRecord) setHTTPOptions(o cursorHTTPOptions) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.httpOpt = o
	r.mu.Unlock()
}

// httpOptions 返回本 HTTP 的选项快照。
func (r *cursorSessionRecord) httpOptions() cursorHTTPOptions {
	if r == nil {
		return cursorHTTPOptions{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.httpOpt
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
		// §25.17.7 A09：完成信号随会话建立即初始化，供 Stop 在锁外等待唯一 finalizer。
		finalizeDone: make(chan struct{}),
	}
	record.billingEventID = CursorSessionBillingEventID(record.id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		// §25.17.7 A09：Stop 之后**不再**返回可继续执行的游离记录——调用方拿到的必须是
		// 明确的"不可用"，从而在模型出站/AgentRun 之前终止（零 AgentRun、零游离状态）。
		return nil
	}
	s.sessions[record.id] = record
	return record
}

// finalizeSignal 返回 finalizer 完成信号（会话建立即初始化；nil 记录返回 nil）。
func (r *cursorSessionRecord) finalizeSignal() <-chan struct{} {
	if r == nil {
		return nil
	}
	return r.finalizeDone
}

// closeFinalizeDone 关闭完成信号：**只允许 finalizer 的唯一胜者**在结算提交、资源归还与
// 索引清理结束后调用（§25.17.7 A09）。
//
// 幂等（once）：重复终结者即便走到这里也不会重复关闭、不会重复结算/释放。
func (r *cursorSessionRecord) closeFinalizeDone() {
	if r == nil {
		return
	}
	r.finalizeOnce.Do(func() {
		ch := r.finalizeDone
		if ch == nil {
			return
		}
		close(ch)
	})
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
//
// §25.17.7 A09：终结之后**在锁外等待**每个会话的 finalizeDone，确保已在进行的唯一
// finalizer 完成结算提交 / 资源归还 / 索引清理后才返回——绝不以 finishing=true 当完成。
// 等待有界且如实记录未完成的会话数（不以超时日志包装成成功）；重复 Stop 不重复结算或释放。
func (s *cursorSessionStore) stop() {
	s.mu.Lock()
	s.stopped = true
	records := make([]*cursorSessionRecord, 0, len(s.sessions))
	signals := make([]<-chan struct{}, 0, len(s.sessions))
	for _, record := range s.sessions {
		records = append(records, record)
		// 仅收集有 owner 会话的完成信号：只有它们会走原 finalizer 并关闭 finalizeDone。
		// 无 owner 的会话（start 与 attachOwner 之间的窗口 / 测试残留）不会被终结，等待其
		// 信号永远不关闭，绝不能阻塞——改为标记失效，attachHTTP 据此明确拒绝。
		if record.owner != nil {
			signals = append(signals, record.finalizeSignal())
		} else {
			// §25.17.7 A09 登记空隙：start 与 attachOwner 两步之间被 Stop 的游离记录必须标记
			// 失效，否则 Stop 清表后它仍可被 attachHTTP 附着执行。标记后 attachHTTP 返回错误，
			// Execute 据此在模型出站 / AgentRun 之前失败关闭（零 AgentRun、零游离状态）。
			record.mu.Lock()
			record.unavailable = true
			record.mu.Unlock()
		}
	}
	s.mu.Unlock()
	for _, record := range records {
		if record.owner != nil {
			record.owner.finishCursorSession(record, "service_stop")
		}
	}
	// 锁外等待完成信号：finalizer 可能包含网络/计费提交，绝不能持会话表锁等待。
	waitCursorSessionFinalizers(signals)
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

// cursorStopFinalizeWaitTimeout 是 Stop 等待唯一 finalizer 完成的**上限**。
//
// 它不修改任何既有停止参数（main 的 HTTP Shutdown/defer app.Cleanup 预算不变），只保证
// Stop 本身不会无限挂住；超时按未完成如实记录，不把未完成当成功。
const cursorStopFinalizeWaitTimeout = 10 * time.Second

// waitCursorSessionFinalizers 在锁外等待各会话的完成信号（不设 10 秒上限）。
//
// §25.17.7 A09：对**已有 finalizer 在进行**的会话，Stop 必须等待其 finalizeDone 完成信号
// （任务提交 + 资源归还）完成后再返回，不设 10 秒上限——等待期间不持会话表锁。finalizeDone
// 由唯一 finalizer 在结算提交、槽位归还与索引清理结束后关闭；若 finalizer 受阻（如上游结算
// 阻塞），Stop 如实阻塞，绝不把"未完成"包装成成功（"清表即返回"的旧实现会漏掉结算）。
// finalizeDone 在终结完成后关闭；Stop 等待该信号以确保结算与资源归还完成。
func waitCursorSessionFinalizers(signals []<-chan struct{}) {
	for _, sig := range signals {
		if sig == nil {
			continue
		}
		<-sig
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

// resolveCallID 把客户端回传的 call id 解析为**索引里登记的键 + 归属会话**。
//
// 顺序（派发单 CALLID 要求）：先按原值查索引；未命中时，若该值是本会话的改写后 id
// （idRewrite 命中），用其**上游原 id** 再查一次（原 id 是早先那批交付时登记的键）。
// 两者都未命中返回 nil —— 调用方据此判 unknown（失败关闭，不猜、不跨批复用）。
//
// 说明：改写 id 只可能属于本进程内的某个活跃会话，因此回退按"原值 → 原 id"在**该会话
// 自己的** idRewrite 里查是安全的（不同会话的改写值互不可见）。
func (s *cursorSessionStore) resolveCallID(protocol string, clientID string) (*cursorSessionRecord, string) {
	if s == nil || clientID == "" || protocol == "" {
		return nil, ""
	}
	if record := s.lookupSessionByProtocolName(protocol, CursorIDKindCall, clientID); record != nil {
		return record, clientID
	}
	// 未命中：把 clientID 当"改写后的 id"反查上游原 id。
	record := s.lookupSessionOwningWireID(clientID)
	if record == nil {
		return nil, ""
	}
	record.mu.Lock()
	original, ok := record.delegations.upstreamCallIDLocked(clientID)
	record.mu.Unlock()
	if !ok {
		return nil, ""
	}
	// 归位后的原 id 必须仍在索引里登记（早先那批交付时登记过）；否则说明关联已清理，
	// 按未命中处理（不凭 idRewrite 凭空造出关联）。
	if found := s.lookupSessionByProtocolName(protocol, CursorIDKindCall, original); found == nil {
		return nil, ""
	}
	return record, original
}

// lookupSessionOwningWireID 找出 idRewrite 里登记了 wireID 的会话（只在本进程活跃会话内查）。
//
// 改写映射是会话私有的（cursorToolDelegations 随会话释放），因此这里只做一次受锁的表遍历，
// 不建第二份全局索引。
func (s *cursorSessionStore) lookupSessionOwningWireID(wireID string) *cursorSessionRecord {
	if s == nil || wireID == "" {
		return nil
	}
	s.mu.Lock()
	records := make([]*cursorSessionRecord, 0, len(s.sessions))
	for _, record := range s.sessions {
		records = append(records, record)
	}
	s.mu.Unlock()
	for _, record := range records {
		record.mu.Lock()
		delegations := record.delegations
		record.mu.Unlock()
		if delegations == nil {
			continue
		}
		delegations.mu.Lock()
		_, ok := delegations.idRewrite[wireID]
		delegations.mu.Unlock()
		if ok {
			return record
		}
	}
	return nil
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
	case cursorProtocolGemini:
		return "gemini"
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
	// seq 是**本会话内单调递增**的批次序号（由 cursorToolDelegations.newBatchLocked 从 1
	// 开始分配，永不重复、永不复用）。它是"是不是新批"的**唯一**判据：
	// 真实客户端会跨轮复用同一个 tool_call_id（生产实测同一会话内 read 与 write 共用
	// call-3466d322-...），因此 call_id 不能用来判定新批，只有 seq 免疫 ID 复用。
	seq uint64
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
	// rejected 是本批**本地已拒绝**的对外 call_id（客户端没有对应工具：shell/f14、原生文件
	// exec 等）。这类调用不会有委托，但上游的 started/listed 计数仍然包含它；若不在此登记，
	// checkSealableLocked 的"每条 started 都要有委托"会永假 → 批不封口
	// （外审 B1）。登记后封口谓词跳过它们，交付集合里也不含它们（它们从未写入 delegations）。
	rejected map[string]struct{}
	// wireDecided 是本批**已决定出站 id** 的委托数（batch.delegations 的前缀长度，由
	// assignWireIDsLocked 处理完整批后推进；delegations 只追加、不重排，因此前缀稳定）。
	//
	// 必须按批记录已决定的数量，而不能靠"WireID 非空"判定：首次交付的委托**不写** WireID
	// （capture 字节保持上游原样，出站 id 由 wireID() 回退到 ID），若只看 WireID，第二次
	// 调用（渲染路径与交付准备路径各一次）会把首次交付的 id 误判为"未决定"而改写成新 id，
	// 与已写出的字节不一致。
	wireDecided int
}

// cursorRejectionTombstone 是会话级"本地已拒绝"墓碑：拒绝先到、started 后到时，
// 让本地拒绝在 started 到达前也不丢；它在 started 到达时原子消费。
type cursorRejectionTombstone struct{}

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
	// responsesTools 是首轮声明的 Responses 工具元数据（custom / function / namespace /
	// tool_search）。prepareDeliveredBatch 在交付准备时写入本对象，供历史登记按实际交付
	// 语义分类（与 apicompat 回桥同源判定）。仅交付侧读，不参与续轮回传解析。
	responsesTools cursorResponsesToolMeta
	// nextSeq 是下一个待分配的批次序号；只有 newBatchLocked 在持有 d.mu 时推进它，
	// 保证 seq 在**本会话内**严格单调递增、从 1 开始、永不复用（免疫 call_id 跨轮复用）。
	nextSeq uint64
	// deliveredIDs 是**本会话内已交付给客户端**的原 id 集合（键为上游原 ID）。
	//
	// 生命周期：只在 prepareDeliveredBatch 交付前写入（assignWireIDsLocked），会话终结
	// （finalizer 清会话 ⇒ 本记录随会话释放）即整体消亡；绝不跨会话复用。
	// 判据：同一原 ID 第二次及以后交付时，客户端按 ID 去重会丢弃该调用，必须改写 id。
	deliveredIDs map[string]struct{}
	// idRewrite 是**新 id → 上游原 id** 的映射（客户端可见 id 反查上游锚点）。
	//
	// 生命周期：与 deliveredIDs 同步（同一处写入、同一处释放）。查询点只有两个：
	// IdentifyResume 的索引未命中回退，以及续轮结果写回前的 ID 归位。绝不清理单条映射
	// ——同一原 ID 可能被上游反复复用，历史映射必须一直可反查。
	idRewrite map[string]string
	// shared 表示本记录由 duplex 拥有解码权（parser 不再解码同一帧）。
	shared bool
	// rejectionTombstones 是会话级"本地已拒绝"墓碑集合：键=对外 call_id，值=登记时刻。
	// 解决"拒绝先到、started 后到"的竞态：noteRejectedCall 无条件登记墓碑，待
	// tool_call_started 到达时由 noteToolCallStarted 在持锁下原子消费（消费即删，
	// 不跨批/跨会话复用），把该 call 记入所属批的 rejected 集合，于是封口谓词跳过它。
	// 本集合随本会话的 delegations 一起释放，绝不跨会话复用；登记/消费都在同一把
	// d.mu 下完成，不留竞态窗口。
	rejectionTombstones map[string]cursorRejectionTombstone
}

// newCursorToolDelegations 构造由 duplex 解码、parser 读取的共享记录。
func newCursorToolDelegations() *cursorToolDelegations {
	return &cursorToolDelegations{
		open:                make(map[string]*cursorToolBatch),
		deliveredIDs:        make(map[string]struct{}),
		idRewrite:           make(map[string]string),
		shared:              true,
		rejectionTombstones: make(map[string]cursorRejectionTombstone),
	}
}

// newLocalCursorToolDelegations 构造由 parser 自己解码的本地记录（无 duplex 的场景）。
func newLocalCursorToolDelegations() *cursorToolDelegations {
	return &cursorToolDelegations{
		open:                make(map[string]*cursorToolBatch),
		deliveredIDs:        make(map[string]struct{}),
		idRewrite:           make(map[string]string),
		rejectionTombstones: make(map[string]cursorRejectionTombstone),
	}
}

// isShared 报告本记录的解码权是否在 duplex 侧。
func (d *cursorToolDelegations) isShared() bool { return d != nil && d.shared }

// newBatchLocked 是**唯一**的建批点：分配本会话内单调递增的 seq 并把批次登记进
// d.all（调用方必须已持 d.mu，且负责把批次放进对应的 d.open key）。
//
// 纪律：禁止在别处写 &cursorToolBatch{...} 直接建批——绕过这里会让批次没有 seq，
// 续轮"是否新批"的判定就退回按 call_id 猜（真实客户端跨轮复用 call_id，会吞掉新批）。
func (d *cursorToolDelegations) newBatchLocked(modelCallID string) *cursorToolBatch {
	d.nextSeq++
	batch := &cursorToolBatch{
		modelCallID: modelCallID,
		seq:         d.nextSeq,
	}
	d.all = append(d.all, batch)
	return batch
}

// batchFor 取/建当前开放批次；modelCallID 为空时要求开放批次唯一（否则含糊失败）。
func (d *cursorToolDelegations) batchFor(modelCallID string) (*cursorToolBatch, error) {
	if modelCallID != "" {
		if batch, ok := d.open[modelCallID]; ok && !batch.sealed {
			return batch, nil
		}
		batch := d.newBatchLocked(modelCallID)
		d.open[modelCallID] = batch
		return batch, nil
	}
	var candidates []*cursorToolBatch
	for _, batch := range d.all {
		if batch.sealed {
			continue
		}
		candidates = append(candidates, batch)
	}
	switch len(candidates) {
	case 0:
		batch := d.newBatchLocked("")
		d.open[""] = batch
		return batch, nil
	case 1:
		return candidates[0], nil
	default:
		return nil, fmt.Errorf("%w: %d open batches for an unattributed signal", ErrCursorBatchAmbiguous, len(candidates))
	}
}

// batchForDelegation 按 call_id 归批：遍历未封口批次，找出 started 列表含该 call_id
// 的批次；恰好命中 1 个则返回它。
//
// F3 整改（外审 must_fix）：call_id 命中 0 个时**不得**再无条件回退 batchFor("")——
// 否则一条没有对应 started 的委派会被并入某个已声明 started 的开放批，进而通过封口、
// 向客户端交出超出 listed 声明的调用。因此：
//   - call_id 为空 → 仍走 batchFor("")（既有行为不变，委托专属批 / writeback 路径依赖）；
//   - call_id 命中 0 个、且**没有任何**开放批携带 started → 仍回退 batchFor("")
//     （委托专属批，如 writeback 路径，委派先于 started 到达或根本没有 started）；
//   - call_id 命中 0 个、但存在携带 started 的开放批 → 该委派无对应 started，失败关闭；
//   - call_id 命中 ≥2 个 → 归批歧义，失败关闭。
//
// 绝不按"最近一批"猜测归属。
//
// 必须在已持有 d.mu 的调用上下文里使用（与 batchFor 同约定，不自行加锁）。B2 之前
// noteDelegation 拿不到 model_call_id，只能靠 call_id 与 started 硬链接归批。
func (d *cursorToolDelegations) batchForDelegation(callID string) (*cursorToolBatch, error) {
	if strings.TrimSpace(callID) == "" {
		return d.batchFor("")
	}
	var matched *cursorToolBatch
	matchedCount := 0
	// openWithStarted 统计携带 started 的开放批数量，用于区分"无匹配"的两种语义：
	// 有 started 的开放批存在却不命中 → 孤儿委派（F3，失败关闭）；
	// 全部开放批都没有 started → 委托专属批，允许回退 batchFor("")。
	openWithStarted := 0
	for _, batch := range d.all {
		if batch.sealed {
			continue
		}
		if len(batch.started) > 0 {
			openWithStarted++
		}
		for _, id := range batch.started {
			if id == callID {
				matched = batch
				matchedCount++
				break
			}
		}
	}
	if matchedCount == 1 {
		return matched, nil
	}
	if matchedCount >= 2 {
		// 多命中：归批歧义，失败关闭（沿用 batchFor("") 的歧义语义，绝不按"最近一批"猜）。
		return nil, fmt.Errorf("%w: delegation call_id %q matches %d open batches' started (ambiguous attribution)",
			ErrCursorBatchAmbiguous, callID, matchedCount)
	}
	// matchedCount == 0：call_id 未命中任何开放批的 started。
	if openWithStarted == 0 {
		// 没有任何开放批携带 started（委托专属批 / 空批，如 writeback 路径）→ 回退 batchFor("")。
		return d.batchFor("")
	}
	if openWithStarted == 1 {
		// 恰好一个开放批携带 started，但该 call_id 不在其中 → 孤儿委派，绝不能并入已声明 started 的批次。
		return nil, fmt.Errorf("%w: delegation call_id %q has no matching tool_call_started in the open batch (orphan delegation beyond listed)",
			ErrCursorBatchIncomplete, callID)
	}
	// ≥2 个开放批都携带 started 却不命中 → 归批歧义，失败关闭（沿用 batchFor("") 歧义语义）。
	return nil, fmt.Errorf("%w: delegation call_id %q matches no started across %d open batches (ambiguous attribution)",
		ErrCursorBatchAmbiguous, callID, openWithStarted)
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
		return cursorFrameErrorf(cursorFrameMessage, "negative tool_requests_listed count")
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
	// 墓碑消费必须在同一把锁内、在任何提前 return 之前完成：拒绝先到、started 后到时，
	// 这里把它原子消费（删即消费，不跨批/跨会话复用）并记入所属批 rejected；无法
	// 归属则失败关闭（错误含 call_id），绝不静默忽略。
	if _, ok := d.rejectionTombstones[callID]; ok {
		delete(d.rejectionTombstones, callID)
		if err != nil {
			// batchFor 无法把 started 归到唯一开放批（如多个无 ID 开放批）→ 墓碑无法归属。
			return fmt.Errorf("%w: rejection tombstone for call_id %q arrived but cannot be attributed to any batch (%s)",
				ErrCursorBatchAmbiguous, callID, modelCallID)
		}
		if batch.sealed {
			return fmt.Errorf("%w: rejection tombstone for call_id %q belongs to an already-sealed batch (unattributable)",
				ErrCursorBatchIncomplete, callID)
		}
		if batch.rejected == nil {
			batch.rejected = make(map[string]struct{}, 1)
		}
		batch.rejected[callID] = struct{}{}
	}
	if err != nil {
		return err
	}
	if !external {
		batch.internalStarted++
		return nil
	}
	alreadyStarted := false
	for _, existing := range batch.started {
		if existing == callID {
			// 同一 call_id 重复 started：不重复计数（幂等）。
			alreadyStarted = true
			break
		}
	}
	if !alreadyStarted {
		batch.started = append(batch.started, callID)
	}
	return nil
}

// noteDelegation 登记一条已解码的 f11 委托（**唯一**解码点，由 duplex 调用）。
//
// B2：归批以 call_id 为准——f11 委托携带 tool_call_id（与 tool_call_started 的
// call_id 同值），而 noteDelegation 拿不到 model_call_id，因此改用 batchForDelegation
// 按 call_id 在 started 列表里硬链接归批。modelCallID 形参保留以维持调用点签名不变
// （duplex / parser 仍传 ""），不再用于归批。其余逻辑、重复 ID 失败关闭、空 ID 校验
// 全部保持不变。
func (d *cursorToolDelegations) noteDelegation(modelCallID string, tc cursorToolCallCapture) error {
	if d == nil {
		return nil
	}
	if strings.TrimSpace(tc.ID) == "" {
		return cursorFrameErrorf(cursorFrameDelegation, "mcp_args without tool_call_id")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.noteDelegationLocked(tc)
}

// noteDelegationLocked 在已持有 d.mu 的前提下登记一条已解码的 f11 委托（与 noteDelegation
// 同语义，供持锁路径复用，避免重复加锁）。
func (d *cursorToolDelegations) noteDelegationLocked(tc cursorToolCallCapture) error {
	batch, err := d.batchForDelegation(tc.ID)
	if err != nil {
		return err
	}
	for _, existing := range batch.delegations {
		if existing.ID == tc.ID {
			// 重复 ID：同一委托被重复提交，按冲突拒绝（§25.2）。
			return cursorFrameErrorf(cursorFrameDelegation, "duplicate tool delegation %q", tc.ID)
		}
	}
	batch.delegations = append(batch.delegations, tc)
	return nil
}

// noteRejectedCall 登记"客户端无对应工具、本地已拒绝执行"的对外 call_id（外审 B1）。
//
// 分两步：
//  1. 若该 call_id 此刻已出现在某个未封口批的 started 列表里（"started 先到、拒绝后到"），
//     就地标记该批的 rejected 集合并返回，**不登记墓碑**——否则墓碑残留，同会话后续批
//     复用同一 call_id 时会被 noteToolCallStarted 消费，把新的真实调用误当成本地拒绝。
//  2. 遍历后仍未命中（"拒绝先到、started 后到"），才登记会话级墓碑：旧实现只在已
//     started 的批里标记，拒绝先到时落空，该批永远封不了口。墓碑随后
//     由 noteToolCallStarted 在持锁下原子消费（消费即删，不跨批/跨会话复用），把该 call
//     记入所属批的 rejected 集合。
//
// 不改 delegations、不改交付集合、不改计数字段：封口语义仍以 listed 为准。
func (d *cursorToolDelegations) noteRejectedCall(callID string) {
	if d == nil || callID == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	// 1. 若该 call_id 已出现在某个未封口批的 started 列表里，就地标记该批 rejected，不写墓碑。
	for _, batch := range d.all {
		if batch.sealed {
			continue
		}
		for _, id := range batch.started {
			if id != callID {
				continue
			}
			if batch.rejected == nil {
				batch.rejected = make(map[string]struct{}, 1)
			}
			batch.rejected[callID] = struct{}{}
			return
		}
	}
	// 2. 未命中任何未封口批的 started：登记墓碑，留待 started 到达时消费。
	if d.rejectionTombstones == nil {
		d.rejectionTombstones = make(map[string]cursorRejectionTombstone)
	}
	d.rejectionTombstones[callID] = cursorRejectionTombstone{}
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

// cursorWireIDPrefix 是交付侧改写后的 tool_call id 前缀（形状仍是 OpenAI 的 call_*）。
const cursorWireIDPrefix = "call_"

// cursorWireIDMaxAttempts 是生成改写 id 时允许的最大重试次数。
//
// 冲突概率来自 uuid 撞上"本会话已交付过的 id"，实际可忽略；但仍必须重试而不是直接
// 失败（极低概率撞车不该让整批交付失败），也不能无限重试（uuid 源异常时必须失败关闭）。
const cursorWireIDMaxAttempts = 8

// assignWireIDs 为全部尚未决定出站 id 的委托决定 WireID（**交付前**调用，幂等）。
//
// 触发条件（唯一的改写判据）：该 capture 的**上游原 id** 在本会话内已经交付过
// （deliveredIDs 命中，含同批多批一起交付时的重复）。未交付过 → **不写 WireID**（capture
// 字节保持不变，出站 id 由 wireID() 回退到 ID）；已交付过 → WireID = "call_" + uuid，
// 并记录 idRewrite[wire] = 上游原 id。
//
// 顺序纪律：按 d.all（批次到达顺序）+ 批内委托顺序遍历，与 snapshot() 的渲染顺序**一致**。
// 因此"渲染顺序里的第一次出现"总是拿到原 id，后续同 id 出现才被改写——这保证第一批发给
// 客户端的 id 与上游原 id 相同（续轮/日志口径不变），只有重复出现的那次才换新 id。
//
// 幂等：已有 WireID 的委托直接跳过（同一批可能先被渲染路径赋值、再被 prepareDeliveredBatch
// 复核），绝不二次改写（二次改写给客户端的 id 会与已写出的字节不一致）。
func (d *cursorToolDelegations) assignWireIDs() error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.assignWireIDsLocked()
}

// assignWireIDsLocked 是 assignWireIDs 的持锁实现（调用方必须已持 d.mu）。
func (d *cursorToolDelegations) assignWireIDsLocked() error {
	if d.deliveredIDs == nil {
		d.deliveredIDs = make(map[string]struct{}, len(d.all))
	}
	if d.idRewrite == nil {
		d.idRewrite = make(map[string]string, len(d.all))
	}
	for _, batch := range d.all {
		if batch == nil {
			continue
		}
		// 只处理尚未决定出站 id 的委托（delegations 只追加，前 wireDecided 条已决定）。
		for i := batch.wireDecided; i < len(batch.delegations); i++ {
			tc := &batch.delegations[i]
			if tc.ID == "" {
				continue
			}
			if _, delivered := d.deliveredIDs[tc.ID]; !delivered {
				// 首次交付该 id：**不写 WireID**，capture 字节保持上游原样（出站 id 由
				// wireID() 回退到 ID）。只登记已交付，供下次复用同一原 id 时触发改写。
				d.deliveredIDs[tc.ID] = struct{}{}
				continue
			}
			wire, err := d.newWireIDLocked()
			if err != nil {
				return err
			}
			tc.WireID = wire
			d.idRewrite[wire] = tc.ID
			d.deliveredIDs[wire] = struct{}{}
		}
		batch.wireDecided = len(batch.delegations)
	}
	return nil
}

// newWireIDLocked 生成一个**本会话内绝未出现过**的新 tool_call id（调用方必须已持 d.mu）。
//
// 冲突判定覆盖三类已占用值：已交付 id（deliveredIDs，含原 id 与历次改写值）、改写映射的
// 键、以及所有已知上游原 id（尚未交付的批也必须避开——否则那条原 id 在它自己交付时会被
// 误判为"已交付过"而被迫改写）。撞车即重新生成，绝不因极低概率冲突让整批交付失败。
func (d *cursorToolDelegations) newWireIDLocked() (string, error) {
	for attempt := 0; attempt < cursorWireIDMaxAttempts; attempt++ {
		candidate := cursorWireIDPrefix + cursorNewUUID()
		if candidate == cursorWireIDPrefix {
			continue
		}
		if _, clash := d.deliveredIDs[candidate]; clash {
			continue
		}
		if _, clash := d.idRewrite[candidate]; clash {
			continue
		}
		if d.upstreamIDKnownLocked(candidate) {
			continue
		}
		return candidate, nil
	}
	return "", cursorFrameErrorf(cursorFrameDelegation,
		"failed to allocate a unique wire tool_call_id after %d attempts", cursorWireIDMaxAttempts)
}

// upstreamIDKnownLocked 报告 candidate 是否是某个委托的上游原 id（调用方必须已持 d.mu）。
func (d *cursorToolDelegations) upstreamIDKnownLocked(candidate string) bool {
	for _, batch := range d.all {
		if batch == nil {
			continue
		}
		for _, tc := range batch.delegations {
			if tc.ID == candidate {
				return true
			}
		}
	}
	return false
}

// upstreamCallID 把**客户端可见 id**（WireID）反查回上游原 id（写回上游时使用）。
//
// 反查失败返回 ok=false：调用方必须失败关闭（不猜、不复用别的批）。未改写的委托
// WireID == ID，因此这里也直接覆盖"未改写过"的常规路径。
func (d *cursorToolDelegations) upstreamCallID(wireID string) (string, bool) {
	if d == nil || wireID == "" {
		return "", false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.upstreamCallIDLocked(wireID)
}

// upstreamCallIDLocked 是 upstreamCallID 的持锁实现（调用方必须已持 d.mu）。
func (d *cursorToolDelegations) upstreamCallIDLocked(wireID string) (string, bool) {
	if d == nil {
		return "", false
	}
	if original, ok := d.idRewrite[wireID]; ok {
		return original, true
	}
	for _, batch := range d.all {
		if batch == nil {
			continue
		}
		for _, tc := range batch.delegations {
			if tc.WireID == wireID || (tc.WireID == "" && tc.ID == wireID) {
				return tc.ID, true
			}
		}
	}
	return "", false
}

// deliveredCallArgs 是「客户端可见 call_id → 交付时登记的规范化参数」的只读查询。
//
// 与 upstreamCallIDLocked 同形状、同遍历（客户端可见 id 可能是改写后的 WireID，
// 也可能是上游原 id —— 上游会在会话内复用同一个 tool_call_id，故两种都要认）。
//
// 用途（2026-11-07 五道闸退役后拆出防篡改）：续轮回传的 arguments 必须与当初交付
// 给客户端时登记的一致 —— 同一个 call_id 换了参数是篡改，必须拒。
//
// 交付时没有参数的工具（部分工具无 arguments）→ 返回空串且 ok=false：
// 无基线可比，比对时跳过（不误拒）。
func (d *cursorToolDelegations) deliveredCallArgs(wireID string) (string, bool) {
	if d == nil {
		return "", false
	}
	wireID = strings.TrimSpace(wireID)
	if wireID == "" {
		return "", false
	}
	for _, batch := range d.all {
		if batch == nil {
			continue
		}
		for _, tc := range batch.delegations {
			if tc.WireID != wireID && !(tc.WireID == "" && tc.ID == wireID) {
				continue
			}
			args := strings.TrimSpace(tc.ArgsJSON)
			if args == "" {
				return "", false
			}
			// custom/freeform 工具的交付参数是**抽取后的自由文本**
			// （client 收到/回传的是 custom_tool_call 项的 input 字段），
			// 与写出侧 cursorResponsesToolCallHistoryCanonical 的取值口径完全一致
			// —— 两侧形态必须一样，否则永远比对不上。
			// 用与写出侧同一个判定（cursorResponsesDeliveredToolCall，同源工具元数据），
			// 不自己另写一份「怎么算 custom 工具」。
			if dt := cursorResponsesDeliveredToolCall(tc.Name, d.responsesTools); dt.Kind == "custom_tool_call" {
				args = strings.TrimSpace(apicompat.ExtractCustomToolCallInput(args))
				if args == "" {
					return "", false
				}
				return args, true
			}
			canonical, err := cursorCanonicalToolArgs(args)
			if err != nil {
				return "", false
			}
			return string(canonical), true
		}
	}
	return "", false
}

// cursorValidateResumeResultArgs 校验：续轮正文里每条工具调用的 parameters，
// 必须与当初交付给客户端时登记的 parameters 一致。
//
// 为什么要有这条（2026-11-07 五道闸退役后拆出）：「同一个 call_id 换了参数」是**篡改**
// （上游会把结果关联到某个具体调用，参数变了就等于让上游按另一组参数执行），
// 必须拒。但这条能力原先寄生在「历史骨架逐字节跨轮比对」上，而那道比对同时挡着
// 「客户端工具集变了 / 换模型 / 换配置摘要」（这些是**合法**行为，该退役）。
// 一并退役时把防篡改一起带走了 → 现在拆成**结果侧**校验：
// 只看「本批已交付记录」vs「本次正文」，与跨轮配置/工具集/历史骨架无关。
//
// 三种情况：
//   - 两侧都有参数且不一致 → 拒（篡改）。
//   - 交付时无参数（无基线）→ 跳过，不误拒。
//   - 本次正文里没有该调用的参数（客户端没回传）→ 跳过，不误拒
//     （Responses 官方用法本就不重放 function_call）。
func cursorValidateResumeResultArgs(
	plan *cursorContextPlan,
	delegations *cursorToolDelegations,
) error {
	if plan == nil || len(plan.historyCallArgs) == 0 || delegations == nil {
		return nil
	}
	for callID, requestArgs := range plan.historyCallArgs {
		deliveredArgs, ok := delegations.deliveredCallArgs(callID)
		if !ok {
			// 交付侧没有这条调用的参数（未交付 / 无参数 / 解析失败）→ 无基线可比，跳过。
			continue
		}
		if deliveredArgs != requestArgs {
			return newCursorResumeError(http.StatusBadRequest, "tool_args_changed", ErrCursorResumeHistoryMismatch)
		}
	}
	return nil
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
//
// 封口以 listed 为准（B2）：未收到 tool_requests_listed 的批次一律不得交接——
// 真实帧序列里 listed(N) 不晚于该批最后一个 tool_call_started，委派可能早于 listed
// 到达；若缺 listed 就允许交接，会在集齐前提前收口（三工具场景只交出 1 条，实测证据
// A3），其余工具丢失。因此无 listed 必须按 fail-closed 处理。
//
// 判定顺序：先查每条对外 started 是否都有完整 f11 委托（保留既有逻辑），再查 listed
// 是否已见到，最后按总数集合判定；不按到达顺序或静默时间猜测结束。
func (d *cursorToolDelegations) checkSealableLocked() error {
	for _, batch := range d.all {
		if batch.sealed {
			continue
		}
		// 每条对外 started 都必须有完整 mcp_args 委托（既有逻辑保留）。
		delegated := make(map[string]struct{}, len(batch.delegations))
		for _, tc := range batch.delegations {
			delegated[tc.ID] = struct{}{}
		}
		for _, callID := range batch.started {
			if _, ok := delegated[callID]; !ok {
				// 外审 B1：本地已拒绝（客户端无对应工具）的调用不会有委托，封口时按已满足处理。
				if _, rej := batch.rejected[callID]; rej {
					continue
				}
				return fmt.Errorf("%w: batch %q call %q has no complete mcp_args delegation",
					ErrCursorBatchIncomplete, batch.modelCallID, callID)
			}
		}
		// F3 整改（外审 must_fix）：反向检查——每条已登记委托都必须有对应的 started，
		// 否则该批交付集合会超出 listed 声明的调用。仅做 started→delegation 单向检查会让
		// "started(a)+delegation(b)+delegation(a)" 通过封口并交出 b（b 无对应 started）。
		//
		// 守卫：仅当本批存在对外 started 时才做反向检查。纯委托批（started 为空、如 writeback
		// 路径或仅含内部元工具）没有可对照的对外 started，不做该检查以避免误杀合法交付。
		if len(batch.started) > 0 {
			for _, tc := range batch.delegations {
				if tc.ID == "" {
					continue
				}
				found := false
				for _, id := range batch.started {
					if id == tc.ID {
						found = true
						break
					}
				}
				if !found {
					return fmt.Errorf("%w: batch %q delegation %q has no matching tool_call_started (delivery set exceeds listed)",
						ErrCursorBatchIncomplete, batch.modelCallID, tc.ID)
				}
			}
		}
		// 未收到 listed 不得交接（证据来源见函数注释）。
		if !batch.listedSeen {
			return fmt.Errorf("%w: batch %q started=%d internal=%d listed=absent",
				ErrCursorBatchIncomplete, batch.modelCallID, len(batch.started), batch.internalStarted)
		}
		total := len(batch.started) + batch.internalStarted
		if total != batch.listed {
			return fmt.Errorf("%w: batch %q started=%d internal=%d listed=%d",
				ErrCursorBatchIncomplete, batch.modelCallID, len(batch.started), batch.internalStarted, batch.listed)
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
// §25.4 / §25.5 会话生命周期：可转交句柄、唯一 finalizer、唯一结算
// ---------------------------------------------------------------------------

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

// cursorUsageCompleteness 判定是否取得了"完整的真实 usage"（§25.5 / §25.17.5 A06）。
//
// 判定完全委托给解析层存在性：billingFieldsComplete() 要求四个计费字段
// （input / output / cache_read / cache_write）逐一出现且合法——四项全 0 也是
// 真实完整用量，绝不因"非零总和"或结束时间戳反推存在性。缺任一字段即不完整：
// 不结算、不估算扣费、不写零 usage 假成功。
func cursorUsageCompleteness(u cursorTurnUsage) (bool, string) {
	if !u.Seen {
		return false, "turn_ended_missing"
	}
	if !u.billingFieldsComplete() {
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

// sessionContext 由 HTTP context 派生出会话运行 context。
// 正常 handler 返回触发的取消不能误杀已完成交接的会话；HTTP 附着期间的真实断连
// 由 attachHTTP 的取消回调判定。适配层不凭空增加会话总期限。
func (r *cursorSessionRecord) sessionContext(httpCtx context.Context) (context.Context, context.CancelFunc) {
	base := context.WithoutCancel(httpCtx)
	if base == nil {
		base = context.Background()
	}
	return context.WithCancel(base)
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
	if r.unavailable {
		r.mu.Unlock()
		return nil, fmt.Errorf("%w: session %q was stopped before activation", ErrCursorSessionAttachConflict, r.id)
	}
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

// armWaitingToolsIdleTimer 在会话进入 waiting_tools 时启动一次性空闲释放计时器（**必须持 record.mu 调用**）。
//
// 同一时刻只保留一个计时器：重复进入 waiting_tools 先停旧的再起新的，避免上一批的
// 计时器把刚接管的会话误判为"无人续轮"。
func (s *CursorExecutionService) armWaitingToolsIdleTimer(record *cursorSessionRecord) {
	if s == nil || record == nil {
		return
	}
	stopWaitingToolsIdleTimerLocked(record)
	record.waitingIdleTimer = time.AfterFunc(cursorWaitingToolsIdleTimeout, func() {
		s.onWaitingToolsIdle(record)
	})
}

// stopWaitingToolsIdleTimerLocked 停止空闲释放计时器（**必须持 record.mu 调用**；幂等）。
func stopWaitingToolsIdleTimerLocked(record *cursorSessionRecord) {
	if record == nil || record.waitingIdleTimer == nil {
		return
	}
	record.waitingIdleTimer.Stop()
	record.waitingIdleTimer = nil
}

// onWaitingToolsIdle 是空闲释放计时器的到期回调。
//
// 到期只代表"可能没人续轮了"，是否真的终结由 finalizer 的 precond 在 record.mu 下
// 原子复核：客户端恰好在此刻续轮（state 已回 running / pending 已消费）则放弃，
// 绝不误杀正在续跑的会话。
func (s *CursorExecutionService) onWaitingToolsIdle(record *cursorSessionRecord) {
	if s == nil || record == nil {
		return
	}
	record.mu.Lock()
	record.waitingIdleTimer = nil
	sessionID := record.id
	record.mu.Unlock()

	released := s.finishCursorSessionIf(record, "waiting_tools_idle", func() bool {
		return record.state == cursorSessionWaitingTools &&
			record.pending != nil && !record.pending.consumed
	})
	if released {
		cursorPhaseLog(context.Background(), zapcore.InfoLevel, "session_waiting_tools_idle_released",
			record.spec.requestedModel,
			zap.String("session_id", sessionID),
			zap.String("idle_timeout", cursorWaitingToolsIdleTimeout.String()))
	}
}

// finishCursorSession 是**唯一 finalizer**（§25.4 末段）。
//
// 固定顺序：停止接新结果 → 关闭原生流/取消读写 → 已获完整真实 usage 则提交唯一结算
// → 归还/转交原预留 → 释放两槽 → 移除该 session 全部索引关联 → closed。
// 网络/计费操作全部在锁外；终结守卫保证每项最多一次；竞争（取消、外部 deadline、服务 Stop、
// 正常返回）以**第一个进入 finalizer 的终态**为唯一胜者，不双写、不重复释放。
func (s *CursorExecutionService) finishCursorSession(record *cursorSessionRecord, reason string) {
	s.finishCursorSessionIf(record, reason, nil)
}

// finishCursorSessionIf 是带前置条件的唯一 finalizer 入口：precond 与"置 finishing"在同一把
// record.mu 下完成，判定与转换原子（方案 §2.5：到点复核仍为同批未消费的 waiting_tools 才终结）。
// precond 在持 record.mu 时调用：precond 只读 record 字段，不得加锁、不得阻塞。
// 返回 true 表示本调用成为唯一胜者并走完 finalizer；precond 不成立或已有胜者时返回 false。
func (s *CursorExecutionService) finishCursorSessionIf(record *cursorSessionRecord, reason string, precond func() bool) bool {
	if s == nil || record == nil {
		return false
	}
	record.mu.Lock()
	if record.finishing {
		accepted := record.interruptReason
		record.mu.Unlock()
		cursorPhaseLog(context.Background(), zapcore.DebugLevel, "session_finalize_duplicate", record.spec.requestedModel,
			zap.String("session_id", record.id),
			zap.String("terminal_reason", reason),
			zap.String("accepted_reason", accepted))
		return false
	}
	if precond != nil && !precond() {
		record.mu.Unlock()
		return false
	}
	record.finishing = true
	record.state = cursorSessionFinalizing
	record.interruptReason = reason
	// 已经进入 finalizer：空闲释放计时器没有意义，先停掉避免重复进入。
	stopWaitingToolsIdleTimerLocked(record)
	record.mu.Unlock()

	cursorPhaseLog(context.Background(), zapcore.InfoLevel, "session_finalize_begin", record.spec.requestedModel,
		zap.String("session_id", record.id),
		zap.String("terminal_reason", reason))

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

	// ⑧ 唯一胜者在这里关闭完成信号（§25.17.7 A09）：结算提交、资源归还与索引清理都已做完。
	// 重复终结者在 ①之前就返回，故不会关闭它；Stop 依此在锁外判断"确实完成"而不是
	// "finishing=true"。
	record.closeFinalizeDone()
	return true
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
		// 外审建议 S1：未封口即终止（usage_missing: turn_ended_missing 等）时带上同一
		// 安全字段 seal_blocked_reason，便于事故复盘区分封口阻塞类别（无则空串不输出）。
		skipFields := []zap.Field{
			zap.String("session_id", sessionID),
			zap.String("billing_event_id", eventID),
			zap.String("terminal_reason", reason),
			zap.String("usage_missing", missing),
			zap.String("last_protocol_stage", stage),
		}
		if record.sealBlockedReason != "" {
			skipFields = append(skipFields, zap.String("seal_blocked_reason", record.sealBlockedReason))
		}
		cursorPhaseLog(context.Background(), zapcore.WarnLevel, "session_settle_skipped", record.spec.requestedModel,
			skipFields...)
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
	// ErrCursorResumeUnknownCallID 表示续轮批里**已命中部分有效关联但混入未登记 call ID**
	// （混合关联）。它与 ErrCursorResumeUnknownAssociation（全未知）严格区分：混合关联直接
	// 400 拒绝，**绝不**复用全未知哨兵——那一类只交原路由判定供应方，会让混合续轮逃到普通/
	// 非 Cursor 供应方。全未知（locatedCount==0）才保留 ErrCursorResumeUnknownAssociation。
	ErrCursorResumeUnknownCallID = errors.New("cursor: mixed association with unknown tool call id")
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
	// Rebuild 非空表示：这次拒绝的原因是**账号侧容量/额度问题**（号被停、卡没额度、
	// 卡被换/被改、号被删、映射被改），换一个号就能接着干——因此不得回显给下游用户。
	// 调用方（guardResumeEligibility）据此终结旧会话并转成换号重建（2026-11-04 用户裁定：
	// "还有号就不该报错，换号重试是站点义务"）。空串 = 真用户错误（平台/分组不允许）。
	Rebuild GatewayFailureReason
}

// IsRebuild 判定该拒绝是否应转成换号重建而不是返回给用户。
func (e *CursorResumeError) IsRebuild() bool {
	return e != nil && e.Rebuild != ""
}

// newCursorResumeRebuildDenial 构造"账号侧问题、应换号重建"的拒绝。
//
// 仍然复用 CursorResumeError 的状态/原因串（日志与既有断言不变），只额外标记 Rebuild，
// 让调用方知道这条拒绝不能写回给客户端。status 只用于日志留痕，不会真的发给下游。
func newCursorResumeRebuildDenial(status int, reason string, rebuild GatewayFailureReason, err error) *CursorResumeError {
	return &CursorResumeError{Status: status, Reason: reason, Err: err, Rebuild: rebuild}
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

// CursorResumeModelChangedReason 表示续轮请求换了模型（同一会话、换了个模型继续）：
// 账号没问题，按新模型重新开一局即可（2026-11-04 用户裁定）。
//
// 单独一个原因串是为了线上可分辨：它既不该让账号进冷却（不是账号故障），也不该计入
// "换号重建"的统计口径——那是容量问题，这是客户端行为。
const CursorResumeModelChangedReason = GatewayFailureReason("cursor_resume_model_changed")

// CursorResumeRebuildError 表示续轮 serve 命中**账号级终态**（当日额度耗尽 / 上游拒付 /
// 非业务码的上游终态错误），且本 HTTP 尚未向客户端写出任何语义字节——可以终结旧会话后，
// 在**同一个 HTTP 请求内**换号、以请求正文的完整历史新建 AgentRun 继续服务
// （2026-11-04 方案 §2.3 / §2.4-1）。
//
// 它不是用户错误：CursorResumeHTTPStatus 不为其产出状态码；handler 侧只把它当作
// "继续走普通执行路径"的信号，并在请求 ctx 上挂 CursorRebuildInfo。
// Unwrap 保留原错误，既有 errors.Is / 日志分类不受影响。
type CursorResumeRebuildError struct {
	// DeadAccountID 是本次死亡会话所用的账号 ID（只用于日志与排除/重绑，不含凭据）。
	DeadAccountID int64
	// Reason 是换号原因（与既有 GatewayFailureReason 同一枚举，不新增通道）。
	Reason GatewayFailureReason
	// Err 是原始上游/协议错误（供 errors.Is 复核）。
	Err error
	// KeepAccount 表示**账号本身没坏**，只是这次请求换了模型（2026-11-04 用户裁定）：
	// 重建时不要把这个号排除（它还健康，新模型它可能也支持），也不因为它而换号。
	// false = 原来的语义：这个号已经不能用了，必须排除。
	KeepAccount bool
}

// Error 实现 error。
func (e *CursorResumeRebuildError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err == nil {
		return "cursor: resume can be rebuilt on another account"
	}
	return "cursor: resume can be rebuilt on another account: " + e.Err.Error()
}

// Unwrap 暴露原始错误，支持 errors.Is / errors.As。
func (e *CursorResumeRebuildError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// CursorRebuildInfo 是请求 ctx 上"续轮中途失败 → 换号重建"的决定（2026-11-04 方案 §2.4-2）。
//
// 只承载日志/重绑所需的稳定标识，不含请求正文或上游原文。
type CursorRebuildInfo struct {
	// DeadAccountID 是已终结会话所用的账号 ID。
	DeadAccountID int64
	// Reason 是换号原因。
	Reason GatewayFailureReason
	// KeepAccount 表示账号没坏（只是换了模型）：选号时不得排除它。
	KeepAccount bool
}

// WithCursorRebuild 把"续轮换号重建"决定挂到请求 ctx。
func WithCursorRebuild(ctx context.Context, info CursorRebuildInfo) context.Context {
	if ctx == nil {
		return nil
	}
	return context.WithValue(ctx, ctxkey.CursorRebuild, info)
}

// CursorRebuildFromContext 读取续轮换号重建决定（缺失返回 ok=false）。
func CursorRebuildFromContext(ctx context.Context) (CursorRebuildInfo, bool) {
	if ctx == nil {
		return CursorRebuildInfo{}, false
	}
	info, ok := ctx.Value(ctxkey.CursorRebuild).(CursorRebuildInfo)
	return info, ok
}

// CursorRebuildRequested 判定本请求是否已决定"续轮换号重建"。
//
// handler 的 sticky 绑定用它作为强制重绑判据：死亡账号的旧绑定必须被新账号覆盖，
// 否则下一次请求会被粘性会话又钉回已停用的账号。
func CursorRebuildRequested(ctx context.Context) bool {
	_, ok := CursorRebuildFromContext(ctx)
	return ok
}

// cursorRebuildExcludedIDs 把"续轮重建的死号"并入本次选号的排除集合（2026-11-04 收口 M2）。
//
// 方案 §2.4-4 要求失败账号进排除表；原实现只把 DeadAccountID 用于日志与重绑，导致
// 账号级终态里"非业务码上游故障"这一类（不停号、不冷却）可能被新一局重新选中，白撞一次。
//
// 放在选号入口而不是 7 个 handler 调用点：ctx 上的重建决定天然覆盖所有入口，
// 不新增平行链，也不会漏掉任何一个协议。**不改调用方传入的 map**（FailoverState 自己维护它）。
func cursorRebuildExcludedIDs(ctx context.Context, excludedIDs map[int64]struct{}) map[int64]struct{} {
	info, ok := CursorRebuildFromContext(ctx)
	if !ok || info.DeadAccountID == 0 {
		return excludedIDs
	}
	// 账号没坏（只是换了模型）：它可能照样支持新模型，白排除等于白白换号。
	if info.KeepAccount {
		return excludedIDs
	}
	if _, exists := excludedIDs[info.DeadAccountID]; exists {
		return excludedIDs
	}
	merged := make(map[int64]struct{}, len(excludedIDs)+1)
	for id := range excludedIDs {
		merged[id] = struct{}{}
	}
	merged[info.DeadAccountID] = struct{}{}
	return merged
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
// 续轮资格核验（§25.17.3「A05续轮顺序」）
// ---------------------------------------------------------------------------

// CursorResumeDenial 是续轮资格核验的唯一结果类型：nil = 资格通过；非 nil = 拒绝。
//
// 它是续轮错误链**同一个类型**的命名投影（type alias），不新增第二套错误分类：
// 调用方既可直接读 Status/Reason，也可把它当 error 交给既有 finalizer，或经
// CursorResumeHTTPStatus 取回 (status, reason)。
type CursorResumeDenial = CursorResumeError

// 续轮资格核验的失败关闭哨兵（与 CursorResumeDenial 同族；状态码由拒绝本身携带）。
var (
	// ErrCursorResumeAccountIneligible 表示原账号本体已不能承接本次续轮
	// （停用/临停/到期/模型级临停/退出原分组/平台不符）：403，零工具结果写入。
	ErrCursorResumeAccountIneligible = errors.New("cursor: original account is no longer eligible for this continuation")
	// ErrCursorResumeSourceBlocked 表示归属来源被 A04 共同来源门明确否决
	// （过期/观测失败/超时/耗尽）：403，账号本体不动，只是本次不予续轮。
	ErrCursorResumeSourceBlocked = errors.New("cursor: activation source is not available for this continuation")
	// ErrCursorResumeSourceChanged 表示原账号的激活码绑定或激活码本身已改变
	// （改绑定/换码）：400 失效关联，不换号、不重建 AgentRun。
	ErrCursorResumeSourceChanged = errors.New("cursor: original activation source changed (association invalid)")
	// ErrCursorResumeSourceUnavailable 表示资格核验所需来源读取/解密失败：503。
	// 这是"读不到"，不是"明确不合格"，不得折叠成 400/403 结论。
	ErrCursorResumeSourceUnavailable = errors.New("cursor: activation source read failed (fail closed)")
)

// verifyResumeEligibility 是续轮资格的**唯一只读核验**（§25.17.3「A05续轮顺序」）。
//
// 判定顺序固定（账号本体 → 组关系/平台 → 现有调度门 → 来源指纹 → A04 共同来源门 →
// 重新映射），任一步不合格即返回非 nil 拒绝；本函数**只判定**，不写任何状态：旧会话
// 的终结与资源释放仍由调用方沿既有 finalizer 语义处理。
//
//	① 原账号存在未删除且仍是 cursor 账号（403）
//	② 请求平台准入：本会话协议所属原生平台仍须允许 cursor 供给（403）
//	③ 当前组关系：本次读取的账号仍挂在原会话所属分组（403）
//	④ 现有调度门 Account.IsSchedulableForModelWithContext：停用/临停/到期/模型级临停（403）
//	⑤ 当前 QuotaKeyID 必须等于首次来源 QuotaKeyID（400，改绑定/换码）
//	⑥ 读该来源并比较首次 MAC（不等 400；读取/解密失败 503）
//	⑦ A04 共同来源门（明确不可用 403；读不到 503），与调度/水位/目录同一实现
//	⑧ 重新映射所得 upstreamModel 必须等于会话初始 upstreamModel（400）
//
// 绝不调用选号器、排队器、并发 Acquire 或余额预留：旧会话仍占着槽，不能用"并发已满"
// 否决自己仍占槽的合法续轮；也不重新计费、不重建 AgentRun。服务锁内只取 spec 快照，
// 来源/DB 读取全在锁外（§25.17.3「核验和DB读取在服务锁外」）。这里只承诺本次读取
// 时点的资格，不虚构跨数据库与原生写入的分布式原子性。
func (s *CursorExecutionService) verifyResumeEligibility(
	ctx context.Context,
	sess *cursorSessionRecord,
	account *Account,
) *CursorResumeDenial {
	if s == nil {
		return newCursorResumeError(http.StatusServiceUnavailable, "execution_unavailable", ErrCursorExecutionUnavailable)
	}
	if sess == nil {
		return newCursorResumeError(http.StatusBadRequest, "invalid_ticket", ErrCursorResumeUnknownAssociation)
	}
	sess.mu.Lock()
	spec := sess.spec
	sess.mu.Unlock()

	// ① 原账号：存在未删除 + 仍是 cursor 账号（软删账号由账号读取本身拒绝）。
	if !isCursorAccount(account) {
		// ① 号被删/不再是 cursor 号：站点侧资源变化，换号重建。
		return newCursorResumeRebuildDenial(http.StatusForbidden, "account_unavailable",
			CursorUpstreamAccountFailureReason, ErrCursorResumeAccountIneligible)
	}
	// ② 请求平台准入：只复用既有平台纯谓词，不按模型名猜归属。
	if platform := cursorResumeRequestPlatform(spec); platform == "" || !cursorNativeBridgePlatforms(platform) {
		return newCursorResumeError(http.StatusForbidden, "platform_not_admitted", ErrCursorResumeAccountIneligible)
	}
	// ③ 当前组关系：以**本次读取**的账号组为准，不用首 HTTP 缓存的 ctx 组关系替代。
	if groupID := spec.identity.GroupID; groupID > 0 && !cursorAccountInGroupID(account, groupID) {
		return newCursorResumeError(http.StatusForbidden, "group_not_allowed", ErrCursorResumeAccountIneligible)
	}
	// ④ 现有调度门（停用/临停/到期/模型级临停）：复用既有判定，不另写一套。
	if !account.IsSchedulableForModelWithContext(ctx, spec.requestedModel) {
		reason := "not_schedulable"
		if account.IsSchedulable() {
			// 账号本体可调度、仅该模型被熔断：与调度链同一原因串（model_rate_limited）。
			reason = "model_rate_limited"
		}
		return newCursorResumeRebuildDenial(http.StatusForbidden, reason,
			CursorUpstreamAccountFailureReason, ErrCursorResumeAccountIneligible)
	}
	// ⑤ 首次来源指纹必须可靠：缺失即失效关联，绝不以空值代表"与首次鉴权同源"。
	sourceFP, fpErr := cursorSessionSourceFingerprint(spec)
	if fpErr != nil {
		// ⑤ 首次来源指纹取不到：站内读数失败，换号重建。
		return newCursorResumeRebuildDenial(http.StatusBadRequest, "source_fingerprint_unavailable",
			CursorUpstreamAccountFailureReason, fpErr)
	}
	// ⑥ 当前绑定必须仍是那一个来源（改绑定/换码 = 失效关联，不换号重试）。
	if currentKeyID := cursorAccountQuotaKeyID(account); currentKeyID != sourceFP.QuotaKeyID {
		// ⑥ 号被换了绑卡（运维换卡）：站点侧变化，换号重建。
		return newCursorResumeRebuildDenial(http.StatusBadRequest, "source_changed",
			CursorUpstreamAccountFailureReason, ErrCursorResumeSourceChanged)
	}
	// ⑦ 读该来源并比较首次 MAC（同一解密 + 取号链同一 HMAC，不另算一份指纹）。
	codeMAC, macErr := s.resumeSourceCodeMAC(ctx, sourceFP.QuotaKeyID)
	if macErr != nil {
		// ⑦ 卡行读不到（DB/解密故障）：同样是"站点侧读不到"，与⑤⑧同处置——换号重建，
		// 绝不把站内故障说成"你的账号已不可用，请重新发起请求"。
		return newCursorResumeRebuildDenial(http.StatusServiceUnavailable, "source_unavailable",
			CursorUpstreamAccountFailureReason, macErr)
	}
	if subtle.ConstantTimeCompare([]byte(codeMAC), []byte(sourceFP.CodeMAC)) != 1 {
		// ⑦ 卡内容与首次记录不符（卡被改过）：站点侧变化，换号重建。
		return newCursorResumeRebuildDenial(http.StatusBadRequest, "source_changed",
			CursorUpstreamAccountFailureReason, ErrCursorResumeSourceChanged)
	}
	// ⑧ A04 共同来源门：与调度/粘性/DB 复核/水位/公开目录同一实现（不另加放行条件）。
	// 2026-11-04 用户裁定：号被停用/卡过期/卡额度用完/读数失败，全是站点侧问题——
	// 换一个还有额度的号就能接着干，绝不能让用户看到"账号已不可用，请重新发起请求"。
	veto, vetoErr := cursorSupplySourceAvailabilityVetoReason(ctx, account)
	if vetoErr != nil {
		return newCursorResumeRebuildDenial(http.StatusServiceUnavailable, "source_unavailable",
			CursorUpstreamAccountFailureReason, vetoErr)
	}
	if veto != "" {
		// 额度类否决按额度耗尽语义上抛：本请求内所有候选都这样时，才由 CUR-RES-11 说明容量。
		rebuildReason := CursorUpstreamAccountFailureReason
		if isCursorQuotaVetoReason(veto) {
			rebuildReason = CursorQuotaExhaustedReason
		}
		return newCursorResumeRebuildDenial(http.StatusForbidden, veto, rebuildReason, ErrCursorResumeSourceBlocked)
	}
	// ⑨ 重新映射：账号侧映射配置变了同样属于站点侧变化，换号重建（新号按自己的映射解析）。
	upstreamModel, matched := account.ResolveMappedModel(spec.requestedModel)
	if !matched || strings.TrimSpace(upstreamModel) != strings.TrimSpace(spec.upstreamModel) {
		return newCursorResumeRebuildDenial(http.StatusBadRequest, "model_mapping_changed",
			CursorUpstreamAccountFailureReason, ErrCursorResumeConfigChanged)
	}
	return nil
}

// isCursorQuotaVetoReason 判定来源门否决串是否真属"没额度了"。
//
// 只认两种：卡真实剩余低于调度下限（quota_key_usage_exhausted，含真实 0）与卡已过期
// （quota_key_expired）。这两种意味着"换任何号都还是没额度"，才是 CUR-RES-11「账号池暂无
// 可用额度」的语义。
//
// 其余否决串（未观测 / 观测失败 / 读数陈旧 / 字段不全 / 未绑卡 / 卡状态读不到）同样属于
// 站点侧问题、同样要换号重建，但它们说的是"我们不知道有没有额度"或"配置不完整"，不是
// "没额度"。混进来的后果是把运维配置问题对外说成容量问题，V3（池真耗尽）就判不准了。
//
// 用常量而不是字面量：这些串是来源门的对外契约，两边各写一份必然漂移。
func isCursorQuotaVetoReason(veto string) bool {
	switch veto {
	case quotaKeyUsageExhausted, quotaKeyExpired:
		return true
	default:
		return false
	}
}

// resumeSourceCodeMAC 读取指定来源行并返回其激活码的 MAC（取号链同一 HMAC）。
//
// 只读：不生成鉴权材料、不解析/落库 deviceId（那是出站鉴权路径的职责），故资格核验
// 不带任何写副作用。读取或解密失败如实上抛（调用方映射为 503），绝不返回空指纹。
func (s *CursorExecutionService) resumeSourceCodeMAC(ctx context.Context, quotaKeyID int64) (string, error) {
	if s == nil || s.quotaKeys == nil || s.enc == nil {
		return "", fmt.Errorf("%w: quota key repository/encryptor is not wired", ErrCursorResumeSourceUnavailable)
	}
	qk, err := s.quotaKeys.GetByID(ctx, quotaKeyID)
	if err != nil {
		return "", fmt.Errorf("%w: load quota key %d: %v", ErrCursorResumeSourceUnavailable, quotaKeyID, err)
	}
	code, err := cursorDecryptQuotaKeyCode(qk, s.enc)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrCursorResumeSourceUnavailable, err)
	}
	return quotaKeyMACOfPlaintext(code), nil
}

// cursorResumeRequestPlatform 把会话的续轮协议命名空间折算成**请求侧原生平台**。
//
// 只服务于资格核验的平台准入（§25.17.3）：无法折算（未知协议）返回空串，调用方据此
// 明确 403，不猜平台、不把未知协议的会话当作可续轮。
func cursorResumeRequestPlatform(spec cursorSessionSpec) string {
	switch cursorResumeProtocolOfSpec(spec) {
	case CursorResumeProtocolChat, CursorResumeProtocolResponses:
		return PlatformOpenAI
	case CursorResumeProtocolMessages:
		return PlatformAnthropic
	case CursorResumeProtocolGemini:
		return PlatformGemini
	default:
		return ""
	}
}

// cursorAccountInGroupID 判定**本次读取**的账号当前是否仍挂在给定分组。
//
// 与既有 isAccountInGroup 同源同义：只看账号本次读取带出的组关系（AccountGroups，
// 及同一次读取派生的 GroupIDs），绝不用首 HTTP 缓存的 ctx 组关系替代；本谓词不新增
// 组读取依赖（分组模型白名单读取留在既有窄只读入口/Serve 侧）。
func cursorAccountInGroupID(account *Account, groupID int64) bool {
	if account == nil || groupID <= 0 {
		return false
	}
	for _, ag := range account.AccountGroups {
		if ag.GroupID == groupID {
			return true
		}
	}
	for _, id := range account.GroupIDs {
		if id == groupID {
			return true
		}
	}
	return false
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
	CallID string
	Text   []string
	// Images 是本次工具结果携带的图片（工具读图场景）。按出现顺序出站为
	// McpToolResultContentItem.f2（McpImageContent），排在全部文本项之后。
	// 空表示本条结果不带图（出站字节与不支持图片时完全一致）。
	Images     []cursorTurnImage
	Structured []byte
	IsError    bool
	// ResultType 是客户端在续轮 input 中给出的结果项类型（仅 Responses 协议有意义，
	// 如 function_call_output / custom_tool_call_output / tool_search_output）。用于校验
	// 回传结果类型是否与已交付工具调用的实际语义（kind）一致（§25.14-D「实际 ID」）。
	ResultType string
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
	// config 是**硬组**配置的链式摘要（§25.14-D「配置与省略规则」）：模型 + 工具声明/
	// schema。续轮这一组不一致即拒绝（同一次生成的上游 wire 契约已变）。
	config *CursorHistoryDigest
	// configSystem 是 system / instructions 文本的摘要；configSampling 是采样参数
	// （temperature / top_p / max_tokens / thinking / reasoning / generation 等）的摘要。
	//
	// 这两组是**软组**：客户端每轮重新拼装正文与参数，环境快照、工具说明、重试丢字段
	// 都会让它们变化。因此续轮只比对留痕（resume_config_drift），不拒绝——它们存在的
	// 意义是让"CUR-RES-04 到底哪一项变了"可观测，而不是当准入闸门。
	configSystem   *CursorHistoryDigest
	configSampling *CursorHistoryDigest
	// history 是**重复前缀**（已交付历史）的链式摘要；本批新结果不在其中。
	history *CursorHistoryDigest
	// tools 是本次声明的工具名（保持声明顺序）。
	tools []string
	// results 是本批真实工具结果（按交付顺序），仅续轮请求非空。
	results []CursorToolResult
	// skippedItems 记录本次请求里被**丢弃**的 input 条目（类型 → 条数）。
	//
	// 丢弃而不是报错是产品口径（2026-11-06）：客户端每加一种新条目，若一律整轮 400，
	// 客户只会认为"你们模型不稳定"。旁支条目（联网搜索 / 压缩记录 / 引用 / 多智能体
	// 消息）与尚未接纳的新类型都丢弃并记账，靠这条日志观察客户端在演进什么。
	//
	// 类型是 cursorSkipLedger（见 cursor_skip_ledger.go）：chat/messages 路径与
	// Responses 路径共用同一个账本类型，避免两套"丢了多少"的口径。
	skippedItems cursorSkipLedger
	// delivered 是本次交付的工具调用（首 HTTP 生成历史时追加，续轮为空）。
	delivered []cursorContextItem
	// instructions 记录本次是否声明了非空 instructions（Responses 用）。
	instructions bool
	// responsesTools 是本次请求实际声明的 Responses 工具元数据（custom / function /
	// namespace / tool_search）。
	//
	// §24.2-5 / §25.14-D：续轮的写出侧必须用**本 HTTP 重新声明**的同一套映射（续轮要求
	// 工具声明集合与首轮一致），否则 custom_tool_call / namespace / tool_search_call 会被
	// 降级成普通 function_call —— 元数据跨 HTTP 丢失。
	responsesTools cursorResponsesToolMeta
	// previousResponseID 是 Responses 的显式续接声明（其它协议为空），取自原协议，
	// 供 Execute 的续轮准入断言判定"是否声明续轮"（§25.17.4 A08）。
	previousResponseID string
	// historyCallIDs 是本请求正文**历史前缀里已声明**的工具调用 ID（Responses 完整
	// input 回放用；其它协议为空）。
	//
	// §25.17.4 行 1「历史中已完成工具对不单独构成本批续轮」：当尾部结果 ID 的
	// function_call 条目也在同一正文中时，这是一个**客户端自带完整两端**的已完成工具对，
	// 不是在向本站声明某条待完成关联（见 resultsReplayHistory）。
	historyCallIDs map[string]struct{}
	// historyCallArgs 是本请求正文**历史前缀里已声明**的工具调用 ID → 其规范化参数
	// （canonical JSON 字符串）。
	//
	// 用途（2026-11-07 五道闸退役后拆出）：续轮回传的 arguments 必须与当初交付给
	// 客户端时登记的 arguments 一致 —— 同一个 call_id 换了参数是**篡改**，必须拒。
	// 这条能力原先寄生在「历史骨架逐字节跨轮比对」上，而那道比对同时挡着
	// 「客户端工具集变了」（合法行为）—— 一并退役时把防篡改一起带走了。
	// 现在拆成**结果侧**校验：只看本批已交付记录 vs 本次正文，与跨轮配置/工具集无关。
	//
	// 只收有参数的工具调用；无参数的（部分工具没有 arguments）不在此集合里，
	// 比对时跳过（无基线可比，不误拒）。
	historyCallArgs map[string]string
	// historyComplete 标记本次请求正文是否携带**可独立重建 AgentRun 的完整历史**
	// （2026-11-04 续轮额度耗尽换号重建方案 §2.3）。
	//
	// 判定口径只读本 HTTP 请求体、不改请求体：本批每条结果的 call ID 都必须在正文
	// **历史前缀**里找到对应调用（chat/messages/gemini 的 assistant tool_calls /
	// tool_use / functionCall，Responses 的 function_call）。只有结果没有对应调用的
	// 孤儿形状（含 Responses 只有 previous_response_id）→ false，禁止重建（会丢上下文）。
	//
	// 该标记只用于"死亡会话能否重建"的准入，不参与任何摘要校验。
	historyComplete bool
}

// claimsContinuation 判定本次请求是否**在原协议层声明了续轮**（携带本批工具结果或
// previous_response_id）。标记来自原协议 probe/plan，转换到执行形状后不得抹掉。
//
// §25.17.4（A08）行 1：**不含**本批工具结果和 previous_response_id 才是原普通请求；
// 历史中已完成工具对不单独构成本批续轮——因此这里只看本批结果/显式 previous_response_id。
func (p *cursorContextPlan) claimsContinuation() bool {
	if p == nil {
		return false
	}
	return len(p.results) > 0 || strings.TrimSpace(p.previousResponseID) != ""
}

// noteSkippedItem 记一笔"这条 input 被丢弃"（类型名统一小写，空类型按 message 记）。
func (p *cursorContextPlan) noteSkippedItem(typeName string) {
	if p == nil {
		return
	}
	if p.skippedItems == nil {
		p.skippedItems = make(cursorSkipLedger, 2)
	}
	p.skippedItems.note(typeName)
}

// logSkippedItems 把本次丢弃的条目类型与条数落一条日志（无丢弃时静默）。
//
// 这是"不要下抛报错"之后唯一的观测手段：哪一类条目在被丢、丢了多少，
// 新类型出现时先在这里看见，再决定要不要真的为它写摘要规则。
func (p *cursorContextPlan) logSkippedItems(ctx context.Context, model string) {
	if p == nil {
		return
	}
	p.skippedItems.log(ctx, "responses_items_skipped", model)
}

// resultsReplayHistory 判定本次携带的工具结果是否**全部属于正文内已完成的工具对**
// （§25.17.4 行 1：历史中已完成工具对不单独构成本批续轮）。
//
// 判定就三条，全部取自本 HTTP 请求体，不新增持久化表/墓碑/ID 前缀/最近会话猜测：
//   - 有显式 previous_response_id → 不是回放（那是明确的关联声明，必须核验）；
//   - 没有本批结果 → 不是回放（普通请求）；
//   - 每个结果 ID 的 function_call 条目也出现在同一正文的历史前缀里 → 客户端自带完整
//     两端的已完成工具对，不构成对本站待完成关联的声明。
//
// 反例（因此仍然会被拒绝）：只有 function_call_output 而正文里没有对应 function_call
// （含重启后孤儿、别家供应商的 ID），或夹带正文未声明的 ID —— 那是真的在声明一条本站
// 无法核验的关联。
func (p *cursorContextPlan) resultsReplayHistory() bool {
	if p == nil || len(p.results) == 0 || len(p.historyCallIDs) == 0 {
		return false
	}
	if strings.TrimSpace(p.previousResponseID) != "" {
		return false
	}
	for _, result := range p.results {
		if _, ok := p.historyCallIDs[strings.TrimSpace(result.CallID)]; !ok {
			return false
		}
	}
	return true
}

// historySupportsRebuild 返回本次请求正文是否足以独立重建一个新 AgentRun
// （2026-11-04 方案 §2.3「重建 ≠ 猜会话」）。
//
// 只读历史标记，不查最近会话、不复用死亡会话状态；false 时任何重建请求都必须
// 维持原有失败关闭（不与"省略历史"路径混用）。
func (p *cursorContextPlan) historySupportsRebuild() bool {
	return p != nil && p.historyComplete
}

// cursorConfigGroup 是配置摘要的分组。续轮对三组的准入策略不同（见 ResumeCursorSession）：
// 整链一串哈希只能给出一个 config_changed，既分不清到底哪一组变了，也没法对风险完全
// 不同的两组用同一把尺子。
type cursorConfigGroup int

const (
	// cursorConfigHard 是决定"还是不是同一次生成"的硬配置：模型 + 工具声明/schema。
	// 它一变，续轮的上游 wire 契约就不同了，必须拒绝。
	cursorConfigHard cursorConfigGroup = iota
	// cursorConfigSystem 是 system / instructions 文本：客户端每轮重新拼装（环境快照、
	// 工具说明都会变），只留痕不拒绝。
	cursorConfigSystem
	// cursorConfigSampling 是采样参数（temperature / top_p / max_tokens / thinking 等）：
	// 客户端重试时常丢字段，只留痕不拒绝。
	cursorConfigSampling
)

// cursorConfigBuilder 把固定生成配置逐项并入**分组**链式摘要。
type cursorConfigBuilder struct {
	hard     *CursorHistoryDigest
	system   *CursorHistoryDigest
	sampling *CursorHistoryDigest
	group    cursorConfigGroup
}

func newCursorConfigBuilder() *cursorConfigBuilder {
	return &cursorConfigBuilder{
		hard:     NewCursorHistoryDigest(),
		system:   NewCursorHistoryDigest(),
		sampling: NewCursorHistoryDigest(),
	}
}

// in 切换后续配置项归入的组。
func (b *cursorConfigBuilder) in(group cursorConfigGroup) { b.group = group }

// target 返回当前组链。
func (b *cursorConfigBuilder) target() *CursorHistoryDigest {
	switch b.group {
	case cursorConfigSystem:
		return b.system
	case cursorConfigSampling:
		return b.sampling
	default:
		return b.hard
	}
}

// item 记录一个已序列化的配置项（工具声明等）。
func (b *cursorConfigBuilder) item(itemType string, canonical []byte) error {
	return b.target().AppendItem(itemType, canonical)
}

// text 记录一个文本配置项。
func (b *cursorConfigBuilder) text(itemType, value string) error {
	return b.target().AppendText(itemType, value)
}

// presence 记录"字段是否声明 + 声明值"（省略与显式值不自行折叠）。
func (b *cursorConfigBuilder) presence(itemType string, raw json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return b.target().AppendText(itemType, "absent")
	}
	return b.target().AppendItem(itemType, append([]byte("present:"), trimmed...))
}

// floatPresence 记录 *float64 配置项（nil = 未声明）。
func (b *cursorConfigBuilder) floatPresence(itemType string, v *float64) error {
	if v == nil {
		return b.target().AppendText(itemType, "absent")
	}
	return b.target().AppendText(itemType, fmt.Sprintf("present:%v", *v))
}

// intPresence 记录 *int 配置项（nil = 未声明）。
func (b *cursorConfigBuilder) intPresence(itemType string, v *int) error {
	if v == nil {
		return b.target().AppendText(itemType, "absent")
	}
	return b.target().AppendText(itemType, fmt.Sprintf("present:%d", *v))
}

// boolPresence 记录 *bool 配置项（nil = 未声明）。
func (b *cursorConfigBuilder) boolPresence(itemType string, v *bool) error {
	if v == nil {
		return b.target().AppendText(itemType, "absent")
	}
	return b.target().AppendText(itemType, fmt.Sprintf("present:%t", *v))
}

// json 记录一个 JSON 配置项（空输入按"未声明"记录，不做等价折叠）。
func (b *cursorConfigBuilder) json(itemType string, raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return b.target().AppendText(itemType, "absent")
	}
	return b.target().AppendJSON(itemType, trimmed)
}

// plan 把三条组链挂到计划上（builder 原地追加，之后无需再次赋值）。
func (b *cursorConfigBuilder) plan() *cursorContextPlan {
	return &cursorContextPlan{
		config:         b.hard,
		configSystem:   b.system,
		configSampling: b.sampling,
		history:        NewCursorHistoryDigest(),
		// 账本必须在这里就分配：各协议的空镜化函数会把 plan.skippedItems 直接传下去
		// 记账，nil map 上的写入会静默丢失（丢了账就等于没有观测手段）。
		skippedItems: cursorSkipLedger{},
	}
}

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
//
// 口径（2026-11-06）：本函数只服务**历史摘要**，不承担准入职责。
// 读不出来的 part / 内容一律丢弃并记账——摘要是给"请求是否变了"做对比用的，
// 为它把整轮 400 掉是拿客户换内部一致性，不值。
func cursorChatContentText(raw json.RawMessage, skipped cursorSkipLedger) (string, error) {
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
				// 图像/文件属于已接纳的输入语义：按出现顺序参与摘要（不丢语义字段）——
				// 摘要保留"这里有张图"，不代表这张图一定要发给上游（转换层按落点决定）。
				encoded, err := json.Marshal(part)
				if err != nil {
					skipped.note(cursorSkipBadContent)
					continue
				}
				b.WriteString("\x00part:")
				b.Write(encoded)
			default:
				// 未知 part 类型：摘要里丢掉这个 part，不整轮拒。
				skipped.note(cursorSkipUnsupportedPart)
				continue
			}
		}
		return b.String(), nil
	}
	// 数字 / 布尔 / 对象等：摘要按空内容处理。
	skipped.note(cursorSkipBadContent)
	return "", nil
}

// 这里曾有一对 cursorValidateChatPlanContent / cursorMessageHasImagePart，
// 作用是"历史里非 user 消息带图就整轮拒"。2026-11-06 已删除：
// 它们与转换层各自实现了一遍准入判据，而计划层在转换层之前执行，导致
// 转换层改成"丢图保文本"之后客户仍然收到 400（同一规则两处实现 = 认知分裂）。
// 现在准入只有一处判据：协议没有落点的内容一律丢 + 记账（cursorSkipLedger）。

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
		return nil, newCursorRequestError(infraerrors.CursorExecInternalErrorCode, http.StatusInternalServerError, "nil chat request")
	}
	builder := newCursorConfigBuilder()
	if err := builder.text("config.model", model); err != nil {
		return nil, err
	}
	builder.in(cursorConfigSystem)
	if err := builder.text("config.instructions", req.Instructions); err != nil {
		return nil, err
	}
	plan := builder.plan()
	plan.instructions = strings.TrimSpace(req.Instructions) != ""

	builder.in(cursorConfigHard)
	tools := make([]string, 0, len(req.Tools))
	for _, tool := range req.Tools {
		item, name, err := cursorChatToolItem(tool)
		if err != nil {
			return nil, err
		}
		if err := builder.item(item.itemType, item.canonical); err != nil {
			return nil, err
		}
		if name != "" {
			tools = append(tools, name)
		}
	}
	plan.tools = tools
	builder.in(cursorConfigSampling)
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
		// 计划层只负责"这批结果是什么"，不做准入（2026-11-06 口径：能丢就丢）。
		// 读不出来的部分一律跳过记账；只有"结果缺 call_id、无法与本轮任何调用对上号"
		// 才放弃这一条（它本来就无法回写，留着只会污染摘要）。
		if len(msg.ToolCalls) > 0 || msg.FunctionCall != nil {
			// 结果消息里出现 assistant 专属的调用字段：丢弃这些字段，结果文本照用。
			plan.noteSkippedItem("chat_result_call_fields")
		}
		callID := strings.TrimSpace(msg.ToolCallID)
		if callID == "" {
			plan.noteSkippedItem("chat_result_without_call_id")
			continue
		}
		text, err := cursorChatContentText(msg.Content, plan.skippedItems)
		if err != nil {
			plan.noteSkippedItem("chat_result_undecodable")
			continue
		}
		plan.results = append(plan.results, CursorToolResult{CallID: callID, Text: []string{text}})
	}
	// historyCalls 只服务"正文是否携带完整历史"的判定；**不写入** plan.historyCallIDs
	// （后者是 Responses 的 resultsReplayHistory 专用口径，其它协议填空避免改变既有
	// "声明续轮"判定）。
	historyCalls := make(map[string]struct{}, 4)
	for _, msg := range req.Messages[:start] {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		switch role {
		case "system", "developer":
			text, err := cursorChatContentText(msg.Content, plan.skippedItems)
			if err != nil {
				plan.noteSkippedItem("chat_system_undecodable")
				continue
			}
			builder.in(cursorConfigSystem)
			if err := builder.text("config.system", text); err != nil {
				return nil, err
			}
			continue
		case "user", "assistant", "tool", "function":
		default:
			// 未知角色（critic / narrator 之类客户端自定义）：不进摘要，按「丢这一条」
			// 处理并记账（2026-11-06 口径：客户把 400 当成模型不稳定）。
			plan.noteSkippedItem("chat_role_" + role)
			continue
		}
		if role != "assistant" && len(msg.ToolCalls) > 0 {
			// 非 assistant 带 tool_calls：丢掉这些字段，文本照进摘要。
			plan.noteSkippedItem("chat_call_fields_outside_assistant")
		}
		if msg.FunctionCall != nil {
			// legacy function_call：旧字段，Cursor 侧无落点 → 丢字段，文本照进摘要。
			plan.noteSkippedItem("chat_legacy_function_call")
		}
		text, err := cursorChatContentText(msg.Content, plan.skippedItems)
		if err != nil {
			plan.noteSkippedItem("chat_message_undecodable")
			continue
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
		// tool_calls 只属于 assistant：挂在 user 上会被当成"用户发起的调用"，
		// 语义错得比丢掉更远。非 assistant 的调用字段已在上面记账丢弃。
		if role == "assistant" {
			for _, call := range msg.ToolCalls {
				canonical, err := cursorToolCallCanonical(call.ID, call.Function.Name, call.Function.Arguments)
				if err != nil {
					plan.noteSkippedItem("chat_tool_call_unparsable")
					continue
				}
				if err := plan.history.AppendItem("chat.assistant.tool_call", canonical); err != nil {
					return nil, err
				}
				// 历史前缀里真实出现的调用 ID：本批结果能在此找到对应调用，才证明正文携带
				// 完整历史（重建只把历史当上下文，不重执行工具）。
				if id := strings.TrimSpace(call.ID); id != "" {
					historyCalls[id] = struct{}{}
				}
				// 同时登记该调用的规范化参数：续轮回传的 arguments 必须与这里登记的
				// 一致（同一个 call_id 换了参数 = 篡改 → 拒）。见 noteHistoryCallArgs。
				plan.noteHistoryCallArgs(call.ID, call.Function.Arguments)
			}
		}
	}
	plan.historyComplete = cursorHistoryComplete(plan.results, historyCalls)
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

// cursorAnthropicImage 把 anthropic image 块解成 cursorTurnImage（原始字节 + mime）。
//
// 只接受 source.type=="base64"（客户端内联图）；远程 url 一律失败关闭（本层不服务端
// 抓取）。与 cursorDecodeImageURL 同一套护栏：mime 必须 image/*、base64 兼容
// Std/URL 两种字母表、解码后不超既有请求体上限。
func cursorAnthropicImage(block apicompat.AnthropicContentBlock) (cursorTurnImage, error) {
	src := block.Source
	if src == nil {
		return cursorTurnImage{}, fmt.Errorf("%w: anthropic image without source", ErrCursorResumeUnsupportedContent)
	}
	if strings.TrimSpace(src.Type) != "base64" {
		return cursorTurnImage{}, fmt.Errorf("%w: anthropic image source %q", ErrCursorResumeUnsupportedContent, strings.TrimSpace(src.Type))
	}
	mime := strings.TrimSpace(src.MediaType)
	if !strings.HasPrefix(mime, "image/") {
		return cursorTurnImage{}, fmt.Errorf("%w: anthropic image media type %q", ErrCursorResumeUnsupportedContent, mime)
	}
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r':
			return -1
		}
		return r
	}, src.Data)
	data, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		data, err = base64.URLEncoding.DecodeString(cleaned)
	}
	if err != nil {
		return cursorTurnImage{}, fmt.Errorf("%w: anthropic image base64: %v", ErrCursorResumeUnsupportedContent, err)
	}
	if len(data) == 0 {
		return cursorTurnImage{}, fmt.Errorf("%w: anthropic image payload is empty", ErrCursorResumeUnsupportedContent)
	}
	if len(data) > cursorUpstreamStreamMaxBytes {
		return cursorTurnImage{}, fmt.Errorf("%w: anthropic image payload exceeds request body size limit (%d bytes)", ErrCursorResumeUnsupportedContent, cursorUpstreamStreamMaxBytes)
	}
	return cursorTurnImage{data: data, mimeType: mime}, nil
}

// cursorToolResultContent 是 tool_result 内层块的**唯一**表示：文本按出现顺序拼接，
// 图片按出现顺序收集，非文本块在文本流里留一个 `\x00block:<json>` 定位标记。
//
// 两条路径必须共用它，否则「本次结果的文本」与「下次续轮重算的历史前缀」不一致，
// 续轮会以 history_mismatch 失败关闭：
//   - 历史项规范化（cursorAnthropicBlockItem 的 tool_result 分支，collectImages=false）；
//   - 本批结果生成（cursorPlanFromMessages 的尾部结果循环，collectImages=true）。
//
// collectImages=true 时图片块会被解成可出站字节（工具读图场景）。解码失败不 fail 整轮：
// 文本标记照样保留（历史前缀仍对齐），只是本次不带图出站——比整轮 400 更可控。
//
// 未知块类型的处置（2026-11-06 口径）：**序列化成定位标记**（历史与本批结果两侧
// 用同一表示，摘要才能逐字节对齐），绝不整轮拒。原来 strict=true 会在结果侧抛错，
// 那正是"客户只是多传了一种块，整轮就 400"的来源。
func cursorToolResultContent(blocks []apicompat.AnthropicContentBlock, collectImages bool, skipped cursorSkipLedger) (string, []cursorTurnImage, error) {
	var text strings.Builder
	var images []cursorTurnImage
	for _, inner := range blocks {
		switch inner.Type {
		case "text", "":
			text.WriteString(inner.Text)
		case "image":
			if collectImages {
				if img, err := cursorAnthropicImage(inner); err == nil {
					images = append(images, img)
				} else {
					skipped.note(cursorSkipImageInvalid)
				}
			}
			encoded, jerr := json.Marshal(inner)
			if jerr != nil {
				skipped.note(cursorSkipBadContent)
				continue
			}
			text.WriteString("\x00block:")
			text.Write(encoded)
		default:
			// 未知块类型：原样序列化成定位标记参与摘要，不整轮拒。
			skipped.note(cursorSkipUnsupportedPart)
			encoded, jerr := json.Marshal(inner)
			if jerr != nil {
				continue
			}
			text.WriteString("\x00block:")
			text.Write(encoded)
		}
	}
	return text.String(), images, nil
}

// cursorAnthropicBlockItem 把单个内容块规范化为历史项（保留类型与内容顺序）。
func cursorAnthropicBlockItem(role string, block apicompat.AnthropicContentBlock, skipped cursorSkipLedger) (cursorContextItem, error) {
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
		// 与结果侧（cursorPlanFromMessages 尾部循环）共用同一表示：历史前缀重算
		// 才能与本批结果的文本逐字节对齐。历史侧不收集图片（不出站），未知块按
		// 既有口径序列化成标记，不因新增图片支持而让已存在的历史变成整轮失败。
		text, _, err := cursorToolResultContent(content, false, skipped)
		if err != nil {
			return cursorContextItem{}, err
		}
		payload := map[string]json.RawMessage{
			"tool_use_id": mustCursorJSON(block.ToolUseID),
			"content":     mustCursorJSON(text),
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
		return nil, newCursorRequestError(infraerrors.CursorExecInternalErrorCode, http.StatusInternalServerError, "nil anthropic request")
	}
	builder := newCursorConfigBuilder()
	if err := builder.text("config.model", model); err != nil {
		return nil, err
	}
	plan := builder.plan()
	systemBlocks, err := cursorAnthropicBlocks(req.System)
	if err != nil {
		// system 读不出来：丢掉整段 system（不整轮拒），正文照发。
		plan.noteSkippedItem("messages_system_undecodable")
		systemBlocks = nil
	}
	builder.in(cursorConfigSystem)
	for _, block := range systemBlocks {
		item, ierr := cursorAnthropicBlockItem("system", block, plan.skippedItems)
		if ierr != nil {
			plan.noteSkippedItem("messages_system_block_" + strings.ToLower(strings.TrimSpace(block.Type)))
			continue
		}
		if err := builder.item(item.itemType, item.canonical); err != nil {
			return nil, err
		}
	}
	builder.in(cursorConfigHard)
	tools := make([]string, 0, len(req.Tools))
	for _, tool := range req.Tools {
		encoded, err := json.Marshal(tool)
		if err != nil {
			return nil, fmt.Errorf("%w: messages tool marshal", ErrCursorResumeUnsupportedContent)
		}
		if err := builder.item("config.tool", encoded); err != nil {
			return nil, err
		}
		if name := strings.TrimSpace(tool.Name); name != "" {
			tools = append(tools, name)
		}
	}
	plan.tools = tools
	builder.in(cursorConfigSampling)
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
		if err := builder.item("config.thinking", encoded); err != nil {
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
		if err := builder.item("config.output", encoded); err != nil {
			return nil, err
		}
	} else if err := builder.text("config.output", "absent"); err != nil {
		return nil, err
	}

	// 尾部连续的"全部为 tool_result 的 user 消息" = 本批结果容器（单/多容器统一规范化）。
	start := len(req.Messages)
	for start > 0 {
		msg := req.Messages[start-1]
		if strings.ToLower(strings.TrimSpace(msg.Role)) != "user" {
			break
		}
		blocks, berr := cursorAnthropicBlocks(msg.Content)
		if berr != nil {
			// 读不出来就当作"不是结果容器"，交给历史处理（2026-11-06 口径）。
			break
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
			plan.noteSkippedItem("messages_result_undecodable")
			continue
		}
		for _, block := range blocks {
			if strings.TrimSpace(block.ToolUseID) == "" {
				// 无 tool_use_id 的结果无法与本轮任何调用配对：丢这一条并记账，
				// 不整轮拒（客户少一条结果，也比整轮 400 好）。
				plan.noteSkippedItem("messages_result_without_tool_use_id")
				continue
			}
			inner, berr := cursorAnthropicBlocks(block.Content)
			if berr != nil {
				plan.noteSkippedItem("messages_result_content_undecodable")
				continue
			}
			// 与历史项规范化共用同一表示（cursorToolResultContent）：图片不再整轮
			// 失败，而是解成可出站字节随结果发给上游（工具读图场景），文本里保留
			// 与历史前缀完全一致的定位标记；未知块类型仍失败关闭。
			text, images, terr := cursorToolResultContent(inner, true, plan.skippedItems)
			if terr != nil {
				plan.noteSkippedItem("messages_result_content_undecodable")
				continue
			}
			plan.results = append(plan.results, CursorToolResult{
				CallID:  strings.TrimSpace(block.ToolUseID),
				Text:    []string{text},
				Images:  images,
				IsError: block.IsError,
			})
		}
	}
	// historyCalls 只服务"正文是否携带完整历史"的判定；不写入 plan.historyCallIDs
	// （后者仅供 Responses 的 resultsReplayHistory 使用）。
	historyCalls := make(map[string]struct{}, 4)
	for _, msg := range req.Messages[:start] {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		if role != "user" && role != "assistant" && role != "system" {
			// 未知角色：丢这一条（2026-11-06 口径），不整轮拒。
			plan.noteSkippedItem("messages_role_" + role)
			continue
		}
		blocks, berr := cursorAnthropicBlocks(msg.Content)
		if berr != nil {
			plan.noteSkippedItem("messages_message_undecodable")
			continue
		}
		for _, block := range blocks {
			if role == "system" && block.Type != "text" && block.Type != "" {
				plan.noteSkippedItem("messages_system_block_" + strings.ToLower(strings.TrimSpace(block.Type)))
				continue
			}
			item, ierr := cursorAnthropicBlockItem(role, block, plan.skippedItems)
			if ierr != nil {
				plan.noteSkippedItem("messages_block_" + strings.ToLower(strings.TrimSpace(block.Type)))
				continue
			}
			if role == "system" {
				if err := plan.configSystem.AppendItem(item.itemType, item.canonical); err != nil {
					return nil, err
				}
				continue
			}
			if err := plan.history.AppendItem(item.itemType, item.canonical); err != nil {
				return nil, err
			}
			// 历史前缀里真实出现的 tool_use ID：本批 tool_result 能配对才证明历史完整。
			if block.Type == "tool_use" {
				if id := strings.TrimSpace(block.ID); id != "" {
					historyCalls[id] = struct{}{}
				}
				// 同时登记该调用的规范化参数（防篡改，见 noteHistoryCallArgs）。
				plan.noteHistoryCallArgs(block.ID, string(block.Input))
			}
		}
	}
	plan.historyComplete = cursorHistoryComplete(plan.results, historyCalls)
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
//
// 分类口径归口 apicompat.ClassifyResponsesInputItemType（唯一事实源），
// 本文件不再维护第二份"我认识的类型"清单。
func cursorResponsesItemIsOutput(item apicompat.ResponsesInputItem) bool {
	return apicompat.IsResponsesInputResult(item.Type)
}

// cursorResponsesItemResultOutput 取工具结果条目的可摘要输出文本。
//
// tool_search_output 可能只有顶层 tools（无 output 字段）：复用 apicompat 的 tool_search
// 输出规范化方式（chatcompletions_responses_bridge 对 tools-only 回退到 tools 内容），绝不把
// tools-only 结果摘要成空字符串。普通结果直接取 item.Output。
func cursorResponsesItemResultOutput(item apicompat.ResponsesInputItem) string {
	if strings.TrimSpace(item.Output) != "" {
		return item.Output
	}
	if strings.EqualFold(strings.TrimSpace(item.Type), "tool_search_output") && len(bytes.TrimSpace(item.Tools)) > 0 {
		return string(item.Tools)
	}
	return item.Output
}

// cursorResponsesItemItem 把单个 input 条目规范化为历史项（保留语义字段）。
//
// 返回 error 只表示"这一条确实无法规范化"——**读侧调用方按「丢这一条」处理**
// （见 cursorPlanFromResponses 的 noteSkippedItem 分支），不整轮拒。
func cursorResponsesItemItem(item apicompat.ResponsesInputItem, skipped cursorSkipLedger) (cursorContextItem, error) {
	itemType := strings.ToLower(strings.TrimSpace(item.Type))
	switch itemType {
	case "", "message":
		role := strings.ToLower(strings.TrimSpace(item.Role))
		switch role {
		case "user", "assistant", "system", "developer":
		default:
			return cursorContextItem{}, fmt.Errorf("%w: responses message role %q", ErrCursorResumeUnsupportedContent, item.Role)
		}
		text, err := cursorChatContentText(item.Content, skipped)
		if err != nil {
			return cursorContextItem{}, err
		}
		return cursorContextItem{itemType: "responses.message." + role, canonical: CursorCanonicalText(text)}, nil
	case "function_call":
		// 与交付侧登记（appendCursorBatchAssistantHistory 的 Responses 分支）同源：
		// 同一套 canonical，保证两侧摘要逐字节相等（§25.17.2「实际 ID」）。
		canonical, err := cursorResponsesToolCallHistoryCanonical("function_call", item.CallID, item.Name, item.Namespace, item.Arguments)
		if err != nil {
			return cursorContextItem{}, err
		}
		return cursorContextItem{itemType: "responses.function_call", canonical: canonical}, nil
	case "custom_tool_call":
		// custom 用 item.Input（自由文本），不再用 item.Arguments；namespace 用 item.Namespace。
		canonical, err := cursorResponsesToolCallHistoryCanonical("custom_tool_call", item.CallID, item.Name, item.Namespace, item.Input)
		if err != nil {
			return cursorContextItem{}, err
		}
		return cursorContextItem{itemType: "responses.custom_tool_call", canonical: canonical}, nil
	case "tool_search_call":
		// arguments 对象原文经 CursorCanonicalJSON 规范化；非合法 JSON 时按文本参与。
		canonical, err := cursorResponsesToolCallHistoryCanonical("tool_search_call", item.CallID, item.Name, item.Namespace, item.Arguments)
		if err != nil {
			return cursorContextItem{}, err
		}
		return cursorContextItem{itemType: "responses.tool_search_call", canonical: canonical}, nil
	case "function_call_output", "custom_tool_call_output", "tool_search_output":
		// 结果条目：与交付侧历史登记同源（appendCursorDeliveredResultsToHistory）。
		// 只含 call_id / output，保证续轮前缀与结果登记逐字节相等。tool_search_output
		// 的 tools-only 结果回退到 tools 内容，避免空摘要。
		canonical, err := cursorResponsesToolResultCanonical(item.CallID, cursorResponsesItemResultOutput(item))
		if err != nil {
			return cursorContextItem{}, err
		}
		return cursorContextItem{itemType: "responses." + itemType, canonical: canonical}, nil
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
		return cursorContextItem{}, fmt.Errorf("%w: responses item %q", ErrCursorResumeUnsupportedContent, item.Type)
	}
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
		return nil, newCursorRequestError(infraerrors.CursorExecInternalErrorCode, http.StatusInternalServerError, "nil responses request")
	}
	builder := newCursorConfigBuilder()
	if err := builder.text("config.model", model); err != nil {
		return nil, err
	}
	plan := builder.plan()
	// 工具集是**有效工具集**（top-level tools + input 里的 additional_tools 声明）。
	// 再摊平 namespace：上游只接受扁平的 function/custom 声明，分组名原样下发等于
	// 没声明（codex 0.166 的工具全在 namespace 里）。摊平同时把客户端自起的名字
	// 加上 ns__ 前缀，顺带避开与 Cursor 原生 exec 通道同名。
	effectiveTools, toolsErr := apicompat.EffectiveResponsesTools(req)
	if toolsErr != nil {
		return nil, fmt.Errorf("%w: responses tools: %v", ErrCursorResumeUnsupportedContent, toolsErr)
	}
	flatTools, flatErr := apicompat.FlattenResponsesToolList(effectiveTools)
	if flatErr != nil {
		return nil, fmt.Errorf("%w: responses tools: %v", ErrCursorResumeUnsupportedContent, flatErr)
	}
	// 元数据与上行 config.tool 必须同源，否则声明只在其中一侧生效。
	toolMeta, metaErr := cursorToolMetaFromTools(effectiveTools)
	if metaErr != nil {
		return nil, metaErr
	}
	plan.responsesTools = toolMeta
	// §25.17.4（A08）：显式 previous_response_id 是本协议层的续轮声明，必须随计划带到执行
	// 入口（转换不得抹掉该标记）。
	plan.previousResponseID = strings.TrimSpace(req.PreviousResponseID)
	plan.instructions = strings.TrimSpace(req.Instructions) != ""
	builder.in(cursorConfigSystem)
	if err := builder.text("config.instructions", req.Instructions); err != nil {
		return nil, err
	}
	builder.in(cursorConfigHard)
	tools := make([]string, 0, len(flatTools.Tools))
	for _, tool := range flatTools.Tools {
		encoded, err := json.Marshal(tool)
		if err != nil {
			return nil, fmt.Errorf("%w: responses tool marshal", ErrCursorResumeUnsupportedContent)
		}
		if err := builder.item("config.tool", encoded); err != nil {
			return nil, err
		}
		if name := strings.TrimSpace(tool.Name); name != "" {
			tools = append(tools, name)
		}
	}
	plan.tools = tools
	builder.in(cursorConfigSampling)
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
		if err := builder.item("config.reasoning", encoded); err != nil {
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
		if err := builder.item("config.text", encoded); err != nil {
			return nil, err
		}
	} else if err := builder.text("config.text", "absent"); err != nil {
		return nil, err
	}

	items, err := cursorResponsesInputItems(req.Input)
	if err != nil {
		// input 既不是字符串也不是条目数组：丢掉整段 input 并记账，不整轮拒。
		// 如果丢掉之后确实没有可发送内容，会以 CUR-REQ-06 明说"缺什么"。
		plan.noteSkippedItem("responses_input_undecodable")
		items = nil
	}
	// 只剥离**尾部连续**的 output 条目（当前批），前缀必须完整匹配。
	start := len(items)
	for start > 0 {
		if !cursorResponsesItemIsOutput(items[start-1]) {
			break
		}
		start--
	}
	// 前缀里被丢弃的调用：结果条目要**跟着一起丢**，否则上游会看到
	// "有结果、找不到对应调用"的畸形历史（那才是真会让模型答错的东西）。
	skippedCalls := make(map[string]struct{}, 4)
	for _, item := range items[:start] {
		// 分类器是唯一口径（apicompat/responses_input_item_kind.go）：
		//   声明（additional_tools 等）不是对话内容，交付侧历史从未登记它 → 跳过；
		//   旁支（联网搜索 / 压缩记录 / 引用 / 多智能体消息）与尚未接纳的新类型
		//   → **丢弃并记账**，绝不整轮拒。客户把 400 当成"你们模型不稳定"，
		//   宁可少一点上下文也不能把报错丢给客户（2026-11-06 产品口径）。
		switch apicompat.ClassifyResponsesInputItem(item) {
		case apicompat.ResponsesInputItemDeclaration:
			continue
		case apicompat.ResponsesInputItemIgnorable, apicompat.ResponsesInputItemUnknown:
			plan.noteSkippedItem(item.Type)
			if callID := strings.TrimSpace(item.CallID); callID != "" {
				skippedCalls[callID] = struct{}{}
			}
			continue
		}
		if callID := cursorResponsesItemCallID(item); callID != "" {
			// §25.17.4 行 1：该调用的声明就在同一正文里 → 与之配对的尾部 output 属于
			// 「历史中已完成工具对」，不构成对本站待完成关联的声明。
			if plan.historyCallIDs == nil {
				plan.historyCallIDs = make(map[string]struct{}, 4)
			}
			plan.historyCallIDs[callID] = struct{}{}
			// 同时登记该调用的规范化参数（防篡改，见 noteHistoryCallArgs）。
			// Responses 的 arguments 与 input 互不覆盖（types.go：两者各取各的），
			// 哪个有值用哪个。
			if args := strings.TrimSpace(item.Arguments); args != "" {
				plan.noteHistoryCallArgs(callID, args)
			} else if len(item.Input) > 0 {
				plan.noteHistoryCallArgs(callID, string(item.Input))
			}
		}
		digestItem, ierr := cursorResponsesItemItem(item, plan.skippedItems)
		if ierr != nil {
			// 认得类型但内容畸形（role 不认识、content 不是文本等）：丢这一条，
			// 不整轮拒——一条畸形条目不该让整轮任务失败。
			plan.noteSkippedItem(item.Type)
			if callID := strings.TrimSpace(item.CallID); callID != "" {
				skippedCalls[callID] = struct{}{}
			}
			continue
		}
		if err := plan.history.AppendItem(digestItem.itemType, digestItem.canonical); err != nil {
			return nil, err
		}
	}
	// 本批结果：只收**本站真的会投递**的结果类型，且调用没被丢弃。
	for _, item := range items[start:] {
		callID := strings.TrimSpace(item.CallID)
		if !apicompat.IsResponsesInputResult(item.Type) || callID == "" {
			// 宿主工具结果（local_shell_call_output 等）本来就没有被转发过的调用；
			// 没有 call_id 的结果无从投递。一律丢弃记账，不报错。
			plan.noteSkippedItem(item.Type)
			continue
		}
		if _, skipped := skippedCalls[callID]; skipped {
			plan.noteSkippedItem(item.Type)
			continue
		}
		plan.results = append(plan.results, CursorToolResult{CallID: callID, Text: []string{cursorResponsesItemResultOutput(item)}, ResultType: item.Type})
	}
	// Responses 的"完整历史"必须能自证：本批每个结果 ID 的调用条目都出现在**同一正文**
	// 的历史前缀里（成对 function_call + function_call_output）。只有 previous_response_id
	// 而无历史时 false —— 重建会丢上下文，必须维持原失败关闭（方案 §2.3 / V6）。
	plan.historyComplete = cursorHistoryComplete(plan.results, plan.historyCallIDs)
	return plan, nil
}

// noteHistoryCallArgs 登记本请求正文历史前缀里某条工具调用的**规范化参数**。
//
// 用途（2026-11-07 五道闸退役后拆出防篡改）：续轮回传的 arguments 必须与当初
// 交付给客户端时登记的一致 —— 同一个 call_id 换了参数是篡改，必须拒。
//
// 规范化用 cursorCanonicalToolArgs（复用 CursorCanonicalToolArgsJSON）：
// 只把空白/键序差异视为同值，且**不用 float64**（大整数会丢精度）。
//
// args 为空（无参数的工具）→ 不登记：没有基线可比，比对时跳过，不误拒。
func (p *cursorContextPlan) noteHistoryCallArgs(callID, args string) {
	if p == nil {
		return
	}
	callID = strings.TrimSpace(callID)
	if callID == "" {
		return
	}
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return
	}
	// 两种形态：
	//   - function 类工具的 arguments 是 JSON 对象 → 规范化后比对
	//     （空白/键序差异视为同值；不用 float64，大整数不丢精度）。
	//   - custom/freeform 工具的 input 是**自由文本**（如 codex 的 exec："text(42)"），
	//     不是 JSON → 原样登记、原样比对。
	//     ⚠️ 不能对它做 JSON 规范化：自由文本不是合法 JSON，规范化会失败。
	canonical := trimmed
	if json.Valid([]byte(trimmed)) {
		if c, err := cursorCanonicalToolArgs(trimmed); err == nil {
			canonical = string(c)
		}
	}
	if p.historyCallArgs == nil {
		p.historyCallArgs = make(map[string]string, 4)
	}
	p.historyCallArgs[callID] = canonical
}

// cursorHistoryComplete 判定请求正文是否携带可独立重建的完整历史。
//
// 判据只读本次请求体：本批每条结果的 call ID 都必须能在正文历史前缀里找到对应调用。
// 四协议共用这一条（不只 Responses）：只有结果、没有对应调用的"孤儿 tool_result"
// 同样是历史不完整，不得凭它重建（方案 §2.3「重建 ≠ 猜会话」）。
func cursorHistoryComplete(results []CursorToolResult, historyCallIDs map[string]struct{}) bool {
	if len(results) == 0 {
		// 普通请求：没有待重建的本批结果，标记不参与任何判定。
		return true
	}
	if len(historyCallIDs) == 0 {
		return false
	}
	for _, result := range results {
		if _, ok := historyCallIDs[strings.TrimSpace(result.CallID)]; !ok {
			return false
		}
	}
	return true
}

// cursorResponsesItemCallID 取「客户端工具调用」条目的 call_id（非调用条目返回空）。
//
// 只认工具调用类条目（function_call / custom_tool_call / local_shell_call /
// tool_search_call / mcp_call / image_generation_call）；output 类条目是结果侧，
// 由 cursorResponsesItemIsOutput 判定，不在此列。
func cursorResponsesItemCallID(item apicompat.ResponsesInputItem) string {
	switch strings.ToLower(strings.TrimSpace(item.Type)) {
	case "function_call", "custom_tool_call", "tool_search_call":
		return strings.TrimSpace(item.CallID)
	}
	return ""
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
		// ToolConfig 进入固定配置摘要：工具选择模式/允许集合是生成配置的一部分，
		// 续轮改变它必须被摘要比较识别（不能只进响应转换）。
		ToolConfig json.RawMessage `json:"toolConfig"`
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
	plan := builder.plan()
	builder.in(cursorConfigSystem)
	if len(bytes.TrimSpace(doc.SystemInstruction)) > 0 {
		var sys struct {
			Parts []map[string]json.RawMessage `json:"parts"`
		}
		if err := json.Unmarshal(doc.SystemInstruction, &sys); err == nil {
			for _, part := range sys.Parts {
				if text, ok := cursorGeminiPartText(part); ok {
					if err := builder.text("config.system", text); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	builder.in(cursorConfigSampling)
	if err := builder.json("config.generation", doc.GenerationConfig); err != nil {
		return nil, err
	}
	if err := builder.json("config.tool_config", doc.ToolConfig); err != nil {
		return nil, err
	}
	tools := make([]string, 0, 8)
	builder.in(cursorConfigHard)
	if len(bytes.TrimSpace(doc.Tools)) > 0 {
		if err := builder.json("config.tools", doc.Tools); err != nil {
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
				// 缺 ID 时只有"同名调用唯一"才能配对；否则丢这一条结果并记账，
				// 不整轮拒（2026-11-06 口径）。
				plan.noteSkippedItem("gemini_function_response_without_id")
				continue
			}
			plan.results = append(plan.results, CursorToolResult{CallID: id, Text: []string{string(part["functionResponse"])}})
		}
	}
	// historyCalls 只服务"正文是否携带完整历史"的判定；不写入 plan.historyCallIDs
	// （后者仅供 Responses 的 resultsReplayHistory 使用）。
	historyCalls := make(map[string]struct{}, 4)
	for _, content := range doc.Contents[:start] {
		role := strings.ToLower(strings.TrimSpace(content.Role))
		if role == "" {
			role = "user"
		}
		if role != "user" && role != "model" && role != "assistant" && role != "system" {
			// 未知角色：丢这一条 content，不整轮拒（2026-11-06 口径）。
			plan.noteSkippedItem("gemini_role_" + role)
			continue
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
				// 历史前缀里真实出现的调用 ID：本批 functionResponse 能配对才证明历史完整。
				if strings.TrimSpace(id) != "" {
					historyCalls[strings.TrimSpace(id)] = struct{}{}
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
				plan.noteSkippedItem("gemini_part_undecodable")
				continue
			}
			if err := plan.history.AppendItem("gemini."+role+".part", encoded); err != nil {
				return nil, err
			}
		}
	}
	plan.historyComplete = cursorHistoryComplete(plan.results, historyCalls)
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
// 失败关闭语义（§25.2 / §25.14-D / §25.17.4 A08）：
//   - 请求**明确声称续轮**（携带本批工具结果或 previous_response_id）但提交的 ID
//     **全部未登记**（含重启后孤儿）→ **(nil, ErrCursorResumeUnknownAssociation)**：
//     可区分的「未知关联」哨兵。helper 只把这一类交回原路由判定供应方（非 Cursor 沿原契约
//     转发，不抢管不改请求体）；进入 Cursor 执行分支时由 Execute 的同一准入断言在模型
//     出站/建 session/转交资源之前 400。绝不猜最近会话、不建墓碑表、不换号重试；
//   - 例外只有一种（行 1「历史中已完成工具对不单独构成本批续轮」）：尾部结果 ID 的
//     function_call 条目也在同一正文里（Responses 完整 input 回放，见
//     cursorContextPlan.resultsReplayHistory）→ **(nil, nil)** 原普通请求；
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
		// 交付侧 id 重写（派发单 CALLID）：索引键登记的就是**发给客户端**的 id，因此先按
		// 原值查索引即可命中。未命中时再把它当作改写后的 id 反查上游原 id 重查一次（历史
		// 映射兜底）；两次都未命中才算 unknown（失败关闭，不猜、不跨批复用）。
		record, _ := s.sessions.resolveCallID(protocol, callID)
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
		// §25.17.4（A08）行 3：请求**明确声称续轮**（本批工具结果或 previous_response_id，
		// 上面已确认 ClaimsContinuation 为真），但全部 ID 都未命中统一索引（含重启后的孤儿）。
		// Identify 返回可区分的「未知关联」哨兵，不返回 (nil,nil)：
		//   - helper 只把这一类交回原路由判定供应方（非 Cursor 照原契约转发，不抢管）；
		//   - 进入 Cursor 执行分支时由 Execute 的续轮准入断言在模型出站/建 session/转交资源
		//     之前返回 400（见 Execute 顶部同一断言）。
		// 这样「全未知 → 开新 AgentRun」不再发生，也不猜最近会话、不换号、不重执行工具。
		//
		// 行 1 的例外：尾部结果的 function_call 也在同一正文里（Responses 完整 input 回放，
		// 客户端自带完整两端的已完成工具对）不构成对本站待完成关联的声明 → 原普通请求。
		if probe.Plan.resultsReplayHistory() {
			return nil, nil
		}
		return nil, newCursorResumeError(http.StatusBadRequest, "unknown_association",
			ErrCursorResumeUnknownAssociation)
	}
	if duplicateCallID != "" {
		return nil, newCursorResumeError(http.StatusBadRequest, "duplicate_result_id",
			fmt.Errorf("%w: %q", ErrCursorResumeDuplicateResults, duplicateCallID))
	}
	if unknownCallID != "" {
		// §25.17.4（A08）行 4：已命中至少一个有效关联，但混入未登记的 call ID —— 这是
		// "混合关联"，**不是**全未知。不得复用 ErrCursorResumeUnknownAssociation（那一类只
		// 交原路由判定供应方，会让混合续轮逃到普通/非 Cursor 供应方）。直接 400 拒绝，错误
		// 分类与全未知哨兵严格区分开。
		return nil, newCursorResumeError(http.StatusBadRequest, "unknown_call_id",
			fmt.Errorf("%w: call id %q", ErrCursorResumeUnknownCallID, unknownCallID))
	}
	if prevID != "" && prevRecord == nil && locatedCount > 0 {
		// 结果 ID 命中本服务会话，但 previous_response_id 不是当前批：混合关联，明确 400 拒绝，
		// 不猜、不复用全未知哨兵（与上面同口径，错误分类区分开）。
		return nil, newCursorResumeError(http.StatusBadRequest, "unknown_previous_response_id",
			fmt.Errorf("%w: previous_response_id", ErrCursorResumeStaleBatch))
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
	Model    string // 可选：URL 路径模型（Gemini 原生协议用），其它协议留空从 Body 解析
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
	// 客户端已续轮：作废 waiting_tools 空闲计时器，不得在续跑期间被回收。
	stopWaitingToolsIdleTimerLocked(record)
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
	// 身份核验去重：已前移到两个 ServeCursorResume（R2 先身份后资格），此处复用同一
	// helper 保持直接调用层的既有入参保护，不重复实现第二套身份判定。
	if denial := cursorResumeIdentityDenial(ticket, in.Identity); denial != nil {
		return nil, denial
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
	storedSystem := record.configSystemDigest.Clone()
	storedSampling := record.configSamplingDigest.Clone()
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
		// 型号变了：只留痕，不再拒绝（五道闸退役，2026-11-07）。有完整历史就按新模型
		// 另开一局；否则原地继续。
		cursorPhaseLog(ctx, zapcore.WarnLevel, "resume_model_changed", spec.requestedModel,
			zap.String("session_id", record.id),
			zap.String("model_stored", spec.requestedModel),
			zap.String("model_ticket", strings.TrimSpace(ticket.Model)))
		if modelErr := s.resumeModelChangedOutcome(ctx, record, ticket, account, spec.requestedModel, strings.TrimSpace(ticket.Model), "ticket"); modelErr != nil {
			return nil, modelErr
		}
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
	bodyModel, modelErr := cursorRequestModel(ticket.Protocol, in.Body, in.Model)
	if modelErr != nil {
		return nil, newCursorResumeError(http.StatusBadRequest, "undecodable_continuation", modelErr)
	}
	if strings.TrimSpace(bodyModel) != "" && strings.TrimSpace(bodyModel) != strings.TrimSpace(spec.requestedModel) {
		// 同上：正文声明的型号变了也只留痕，不拒绝。
		cursorPhaseLog(ctx, zapcore.WarnLevel, "resume_model_changed", spec.requestedModel,
			zap.String("session_id", record.id),
			zap.String("model_stored", spec.requestedModel),
			zap.String("model_ticket", strings.TrimSpace(bodyModel)),
			zap.String("model_source", "body"))
		if modelErr := s.resumeModelChangedOutcome(ctx, record, ticket, account, spec.requestedModel, strings.TrimSpace(bodyModel), "body"); modelErr != nil {
			return nil, modelErr
		}
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
		// 硬组（模型 + 工具声明/schema）变了：只留痕，不再拒绝（五道闸退役，2026-11-07）。
		// 本轮一律按**本次正文声明的**配置构造：客户端每轮重新拼工具声明是合法行为
		// （生产实测续轮比首轮少声明一个工具 22→21 / 24→23），跨轮比对只会把正常用户
		// 拒成 CUR-RES-04。防串号由结果侧 call_id 归属 + 结果类型 + 完整性校验承担。
		cursorLogResumeConfigHardMismatch(ctx, record, storedConfig, exposed, plan)
	}
	// 软组（system 文本 / 采样参数）**只留痕不拒绝**：这两组是客户端每轮重新拼出来的
	// 字节——system 里带环境快照、工具说明会变，重试时 thinking/温度字段会丢。逐字节比对
	// 必然把正常续轮拒成 CUR-RES-04，而它想挡的"篡改"已由下面的结果校验（编号归属 +
	// 完整性）挡住。留着当闸门等于拿大量正常用户的失败去拦一个不存在的威胁。
	cursorLogResumeConfigDrift(ctx, record, storedSystem, storedSampling, plan)
	// 工具集合不再跨轮比对（2026-11-07 五道闸退役）：客户端每轮重新声明的工具集
	// 本来就允许变化，本轮一律按本次声明的集合构造；结果侧 call_id 归属 + 类型 +
	// 完整性校验仍然照常执行。
	// previous_response_id 精确指向当前批时可**仅携带本批结果**省略历史；这里的
	// historyOmitted 只用于日志留痕，不再作为任何准入条件。
	historyOmitted := strings.TrimSpace(ticket.ResponseID) != "" && plan.history.ItemCount() == 0
	// 历史摘要不再跨轮比对（五道闸退役，2026-11-07）：客户端回传的历史可以裁剪 assistant
	// 可见正文、可以省略整段历史（只带 previous_response_id），本轮一律按**本次正文**
	// 构造。这里只留计数痕迹（不比较链值、不构成任何拒绝条件），供线上观察正文裁剪规模。
	if !historyOmitted && storedHistory != nil && storedHistory.ItemCount() != plan.history.ItemCount() {
		cursorPhaseLog(ctx, zapcore.InfoLevel, "resume_history_item_count_drift", spec.requestedModel,
			zap.String("session_id", record.id),
			zap.Int("history_items_stored", storedHistory.ItemCount()),
			zap.Int("history_items_request", plan.history.ItemCount()),
			zap.String("protocol", string(ticket.Protocol)))
	}
	// 2026-11-07 五道闸退役：**历史不再跨轮比对**（客户端裁剪 assistant 可见正文、
	// 换模型、换工具集都是合法行为），但**留痕必须保留** ——
	// 线上看到续轮异常时要能回答「谁改了什么」，这是可观测性能力，不是准入闸门。
	//
	// ⚠️ 跨轮比对退役时若把留痕一起删掉，线上就只剩一个 reason，
	// 「history_mismatch 是线上 CUR-RES-04 的老最大来源」这类问题将无从定位。
	// 这里恢复留痕，**只记不拒**。
	if !historyOmitted && storedHistory != nil && !storedHistory.Equal(plan.history) &&
		storedHistory.SkeletonEqual(plan.history) {
		// 骨架一致、但全链不一致 = 客户端裁剪/省略了 assistant 可见正文。
		// 这是**正常**行为，必须放行；同时留一条可统计的痕迹，让「正文裁剪到底有多普遍」
		// 在线���可观测（也为本修复提供生产侧取证）。
		cursorPhaseLog(ctx, zapcore.InfoLevel, "resume_history_prose_pruned", spec.requestedModel,
			zap.String("session_id", record.id),
			zap.Int("history_items_stored", storedHistory.ItemCount()),
			zap.Int("history_items_request", plan.history.ItemCount()),
			zap.Int("prose_items_pruned", storedHistory.ItemCount()-plan.history.ItemCount()),
			zap.String("protocol", string(ticket.Protocol)))
	}
	if !historyOmitted && storedHistory != nil && !storedHistory.Equal(plan.history) {
		cursorPhaseLog(ctx, zapcore.WarnLevel, "resume_history_mismatch", spec.requestedModel,
			zap.String("session_id", record.id),
			zap.Int("history_items_stored", storedHistory.ItemCount()),
			zap.Int("history_items_request", plan.history.ItemCount()),
			zap.Int("history_skeleton_items_stored", storedHistory.SkeletonItemCount()),
			zap.Int("history_skeleton_items_request", plan.history.SkeletonItemCount()),
			zap.String("history_digest_stored", cursorDigestPrefix(storedHistory)),
			zap.String("history_digest_request", cursorDigestPrefix(plan.history)),
			zap.String("history_skeleton_stored", cursorDigestSkeletonPrefix(storedHistory)),
			zap.String("history_skeleton_request", cursorDigestSkeletonPrefix(plan.history)),
			zap.String("history_item_types_stored", storedHistory.TraceString()),
			zap.String("history_item_types_request", plan.history.TraceString()),
			zap.String("history_first_divergence", storedHistory.FirstDivergence(plan.history)),
			zap.String("protocol", string(ticket.Protocol)))
	}
	if err := cursorValidateResumeResults(ticket.CallIDs, plan.results); err != nil {
		if record.batchConsumed() {
			return nil, newCursorResumeError(http.StatusConflict, "already_consumed", ErrCursorResumeAlreadyConsumed)
		}
		return nil, err
	}
	// 参数一致性（2026-11-07 五道闸退役后拆出）：本次正文里每条工具调用的 parameters
	// 必须与当初交付给客户端时登记的一致 —— 同一个 call_id 换了参数是篡改。
	// 这条能力原先寄生在「历史骨架逐字节跨轮比对」上，一并退役时被带走了。
	if err := cursorValidateResumeResultArgs(plan, record.delegations); err != nil {
		if record.batchConsumed() {
			return nil, newCursorResumeError(http.StatusConflict, "already_consumed", ErrCursorResumeAlreadyConsumed)
		}
		return nil, err
	}
	// 续轮回传的结果项类型必须与已交付工具调用的实际语义（kind）一致：
	// function_call_output ↔ function_call、custom_tool_call_output ↔ custom_tool_call、
	// tool_search_output ↔ tool_search_call。否则即"交付 custom 却回传 function_call_output"
	// 之类的跨类型篡改，必须失败关闭（绝不回落、绝不放宽 history_mismatch）。
	//
	// 省略历史（previous_response_id 路径）同样执行本校验（§7 校验矩阵）：省略历史只跳过
	// **摘要比对**（storedHistory 等价重放），但**不豁免** call_id 归属与结果类型校验——
	// 客户端仍可能把 custom/tool_search 的结果伪装成 function_call_output。本校验在 intake
	// 写入（DeliverToolResults）之前执行，保证零上游写入。
	if ticket.Protocol == CursorResumeProtocolResponses {
		if err := cursorValidateResponsesResultKinds(record, plan.results); err != nil {
			if record.batchConsumed() {
				return nil, newCursorResumeError(http.StatusConflict, "already_consumed", ErrCursorResumeAlreadyConsumed)
			}
			return nil, err
		}
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

	// 本 HTTP 的逐请求选项（§25.17.2-2：续轮用本 HTTP 的 stream / tool_choice，
	// 不沿用首轮值；工具还原映射来自本会话已接受配置，不在 drainReq 里丢成空值）。
	toolMode, toolName, choiceErr := cursorResumeToolChoice(ticket.Protocol, in.Body)
	if choiceErr != nil {
		return nil, choiceErr
	}
	record.setHTTPOptions(cursorHTTPOptions{
		stream:         in.Stream,
		model:          ticket.Model,
		toolChoiceMode: toolMode,
		toolChoiceName: toolName,
	})
	record.setLogContext(ctx)

	// 续轮 drain（跨 HTTP 存活会话）：**先**附着输出通道并准备好本 HTTP 的 writer 与输出
	// 标识，**再**写真实工具结果（§25.17.2-1：附着/writer 未就绪绝不写真实结果，避免
	// 首帧落在未附着窗口或写出时才失败）。无真实 pump（测试 stub / 已终态会话 / c 为空）
	// 时 ch 为 nil，走既有返回契约，不破坏 W2-E。
	var drainCh chan *cursorOutputEvent
	var drainDetach func()
	var drainReq *CursorExecutionRequest
	var drainWriter io.Writer
	if record.output != nil && c != nil {
		drainCh, drainDetach = record.output.attach()
	}
	if drainCh != nil {
		defer drainDetach()
		drainReq = &CursorExecutionRequest{
			Protocol: record.spec.protocol,
			Stream:   in.Stream,
			Model:    ticket.Model,
			// 工具还原映射取本 HTTP 重新声明的同一套工具：漏传会让
			// custom/namespace/tool_search 跨 HTTP 丢失。
			Tools:          plan.responsesTools,
			ToolChoiceMode: toolMode,
			ToolChoiceName: toolName,
		}
		responseID := record.output.responseID()
		s.bindHTTPResponseOwner(c, responseID)
		writer, restore, werr := s.beginResponseWriter(c, drainReq, nil, responseID, record)
		if werr != nil {
			s.finishCursorSession(record, "resume_writer_failed")
			return nil, werr
		}
		if restore != nil {
			defer restore()
		}
		drainWriter = writer
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
	if err := appendCursorDeliveredResultsToHistory(record.historyDigest, ticket.Protocol, plan.results, record); err != nil {
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
		// 只有正文携带可独立重建的完整历史时才允许把账号级失败包装成换号重建哨兵；
		// 历史不完整（Responses 只有 previous_response_id）保持原失败关闭（方案 §2.3 / V6）。
		return s.cursorDrainAttachedOutput(drainCh, drainWriter, drainReq, record, plan.historySupportsRebuild())
	}

	return &ForwardResult{
		Model:         ticket.Model,
		UpstreamModel: spec.upstreamModel,
		Stream:        in.Stream,
		Duration:      time.Since(started),
	}, nil
}

// cursorDigestPrefix 返回摘要的前 12 位十六进制（只用于日志比对，不参与任何判定）。
func cursorDigestPrefix(d *CursorHistoryDigest) string {
	if d == nil {
		return ""
	}
	hex := d.HexString()
	if len(hex) > 12 {
		return hex[:12]
	}
	return hex
}

// cursorDigestSkeletonPrefix 返回**骨架链**摘要的前 12 位十六进制（只用于日志）。
func cursorDigestSkeletonPrefix(d *CursorHistoryDigest) string {
	if d == nil {
		return ""
	}
	hex := d.SkeletonHexString()
	if len(hex) > 12 {
		return hex[:12]
	}
	return hex
}

// cursorLogResumeConfigHardMismatch 记录续轮**硬组**（模型 + 工具声明/schema）不一致。
//
// 2026-11-07 五道闸退役后，硬组不一致**不再拒绝**续轮（本轮按本次正文声明的配置构造），
// 但仍然留痕：线上只剩这个日志可回答"这一轮客户端到底改了什么"。把两组各自的项数与
// 摘要前缀钉进日志：只写计数与前缀，不含任何工具正文/参数值（安全字段，不可反推内容）。
func cursorLogResumeConfigHardMismatch(
	ctx context.Context,
	record *cursorSessionRecord,
	stored *CursorHistoryDigest,
	exposed []string,
	plan *cursorContextPlan,
) {
	if record == nil || plan == nil {
		return
	}
	cursorPhaseLog(ctx, zapcore.WarnLevel, "resume_config_hard_mismatch", record.spec.requestedModel,
		zap.String("session_id", record.id),
		zap.Int("hard_items_stored", stored.ItemCount()),
		zap.Int("hard_items_request", plan.config.ItemCount()),
		zap.String("hard_digest_stored", cursorDigestPrefix(stored)),
		zap.String("hard_digest_request", cursorDigestPrefix(plan.config)),
		zap.Int("tool_count_stored", len(exposed)),
		zap.Int("tool_count_request", len(plan.tools)))
}

// cursorLogResumeConfigDrift 记录续轮**软组**配置漂移（system 文本 / 采样参数）。
//
// 这两组不影响续轮准入（见 ResumeCursorSession 的判定说明），但它们是"CUR-RES-04
// 到底哪一项变了"的唯一可观测证据：此前整链哈希比较只能给出一个 config_changed，
// 无法区分客户端改了 system、还是重试时丢了 thinking。日志只写分组名、项数与摘要
// 前缀，不含任何正文/参数值（安全字段，不可反推内容）。
func cursorLogResumeConfigDrift(
	ctx context.Context,
	record *cursorSessionRecord,
	storedSystem, storedSampling *CursorHistoryDigest,
	plan *cursorContextPlan,
) {
	if record == nil || plan == nil {
		return
	}
	fields := []zap.Field{zap.String("session_id", record.id)}
	groups := make([]string, 0, 2)
	if storedSystem != nil && plan.configSystem != nil && !storedSystem.Equal(plan.configSystem) {
		groups = append(groups, "system")
		fields = append(fields,
			zap.Int("system_items_stored", storedSystem.ItemCount()),
			zap.Int("system_items_request", plan.configSystem.ItemCount()),
			zap.String("system_digest_stored", cursorDigestPrefix(storedSystem)),
			zap.String("system_digest_request", cursorDigestPrefix(plan.configSystem)))
	}
	if storedSampling != nil && plan.configSampling != nil && !storedSampling.Equal(plan.configSampling) {
		groups = append(groups, "sampling")
		fields = append(fields,
			zap.Int("sampling_items_stored", storedSampling.ItemCount()),
			zap.Int("sampling_items_request", plan.configSampling.ItemCount()),
			zap.String("sampling_digest_stored", cursorDigestPrefix(storedSampling)),
			zap.String("sampling_digest_request", cursorDigestPrefix(plan.configSampling)))
	}
	if len(groups) == 0 {
		return
	}
	fields = append(fields, zap.Strings("drift_groups", groups))
	cursorPhaseLog(ctx, zapcore.WarnLevel, "resume_config_drift", record.spec.requestedModel, fields...)
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

// cursorResponsesResultTypeKind 把 Responses 结果项类型映射回其对应工具调用的 kind。
//
// function_call_output ↔ function_call、custom_tool_call_output ↔ custom_tool_call、
// tool_search_output ↔ tool_search_call。未知/不支持的结果类型返回空串（由调用方按
// 失败关闭处理）。
func cursorResponsesResultTypeKind(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "function_call_output":
		return "function_call"
	case "custom_tool_call_output":
		return "custom_tool_call"
	case "tool_search_output":
		return "tool_search_call"
	default:
		return ""
	}
}

// cursorValidateResponsesResultKinds 校验续轮回传的每个结果项类型是否与已交付工具调用
// 的实际语义（kind）一致（§25.14-D「实际 ID」）。
//
// 结果项类型在交付侧由 appendCursorDeliveredResultsToHistory 按 kind 选定；续轮回传时
// 客户端给出结果项类型，两者必须同源。不一致（如交付 custom 却回传 function_call_output）
// 即视为跨类型篡改，失败关闭 ErrCursorResumeUnsupportedContent，绝不回落到 function_call。
// 仅在 Intake 写入之前调用，保证校验未通过时零上游写入。
//
// 未登记即失败关闭：无论是整个会话缺少已交付工具上下文，还是某个 call_id 没有交付记录，
// 都直接失败关闭（绝不跳过）。这样校验总在写上游之前发生，避免「上游已写入、随后才报错」
// 的不可回滚状态；deliveredToolCalls 的填充本就覆盖流式与非流式两条交付路径，无需跳过。
func cursorValidateResponsesResultKinds(record *cursorSessionRecord, results []CursorToolResult) error {
	if record == nil || record.deliveredToolCalls == nil {
		return newCursorResumeError(http.StatusBadRequest, "result_type_mismatch",
			fmt.Errorf("%w: responses result without delivered tool call context", ErrCursorResumeUnsupportedContent))
	}
	for _, result := range results {
		dt, ok := record.deliveredToolCalls[strings.TrimSpace(result.CallID)]
		if !ok || dt.Kind == "" {
			return newCursorResumeError(http.StatusBadRequest, "result_type_mismatch",
				fmt.Errorf("%w: result call_id %q has no delivered tool call", ErrCursorResumeUnsupportedContent, result.CallID))
		}
		wantKind := cursorResponsesResultTypeKind(result.ResultType)
		if wantKind == "" || wantKind != dt.Kind {
			return newCursorResumeError(http.StatusBadRequest, "result_type_mismatch",
				fmt.Errorf("%w: result type %q does not match delivered tool call kind %q", ErrCursorResumeUnsupportedContent, result.ResultType, dt.Kind))
		}
	}
	return nil
}

// appendCursorDeliveredResultsToHistory 把本次接纳的真实结果推进已交付历史。
//
// 表示与各协议历史规范化**同源**（下次续轮重算前缀才能与这里对齐）。
func appendCursorDeliveredResultsToHistory(d *CursorHistoryDigest, protocol CursorResumeProtocol, results []CursorToolResult, record *cursorSessionRecord) error {
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
			// 结果条目类型必须按**已交付调用的实际语义**选定（§25.14-D「实际 ID」）：
			// function_call → function_call_output、custom_tool_call → custom_tool_call_output、
			// tool_search_call → tool_search_output。查不到 kind → 失败关闭（绝不回落）。
			if record == nil || record.deliveredToolCalls == nil {
				return fmt.Errorf("%w: responses result without delivered tool call context", ErrCursorResumeUnsupportedContent)
			}
			dt, ok := record.deliveredToolCalls[strings.TrimSpace(result.CallID)]
			if !ok || dt.Kind == "" {
				return fmt.Errorf("%w: result call_id %q has no delivered tool call", ErrCursorResumeUnsupportedContent, result.CallID)
			}
			var resultType string
			switch dt.Kind {
			case "function_call":
				resultType = "function_call_output"
			case "custom_tool_call":
				resultType = "custom_tool_call_output"
			case "tool_search_call":
				resultType = "tool_search_output"
			default:
				return fmt.Errorf("%w: result call_id %q delivered kind %q not supported", ErrCursorResumeUnsupportedContent, result.CallID, dt.Kind)
			}
			item := apicompat.ResponsesInputItem{
				Type:   resultType,
				CallID: result.CallID,
				Output: strings.Join(result.Text, ""),
			}
			digestItem, err := cursorResponsesItemItem(item, nil)
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

// cursorResumeToolChoice 取本续轮 HTTP 的工具选择（模式 + 具名），用于本 HTTP 的输出约束。
//
// §25.17.2-2：续轮的 tool_choice 只能来自本 HTTP，不能沿用首轮；工具还原映射来自会话
// 已接受配置，不在这里丢成空值。
func cursorResumeToolChoice(protocol CursorResumeProtocol, body []byte) (string, string, error) {
	switch protocol {
	case CursorResumeProtocolChat:
		var req apicompat.ChatCompletionsRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return "", "", newCursorResumeError(http.StatusBadRequest, "undecodable_continuation", ErrCursorResumeUnsupportedContent)
		}
		mode, name, err := cursorToolChoiceMode(req.ToolChoice)
		if err != nil {
			return "", "", newCursorResumeError(http.StatusBadRequest, "tool_choice_invalid", err)
		}
		return mode, name, nil
	case CursorResumeProtocolMessages:
		var req apicompat.AnthropicRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return "", "", newCursorResumeError(http.StatusBadRequest, "undecodable_continuation", ErrCursorResumeUnsupportedContent)
		}
		mode, name, err := cursorToolChoiceMode(req.ToolChoice)
		if err != nil {
			return "", "", newCursorResumeError(http.StatusBadRequest, "tool_choice_invalid", err)
		}
		return mode, name, nil
	case CursorResumeProtocolResponses:
		var req apicompat.ResponsesRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return "", "", newCursorResumeError(http.StatusBadRequest, "undecodable_continuation", ErrCursorResumeUnsupportedContent)
		}
		mode, name, err := cursorToolChoiceMode(req.ToolChoice)
		if err != nil {
			return "", "", newCursorResumeError(http.StatusBadRequest, "tool_choice_invalid", err)
		}
		return mode, name, nil
	case CursorResumeProtocolGemini:
		mode, name, err := cursorGeminiToolChoiceMode(body)
		if err != nil {
			return "", "", newCursorResumeError(http.StatusBadRequest, "tool_choice_invalid", err)
		}
		return mode, name, nil
	default:
		return "", "", newCursorResumeError(http.StatusBadRequest, "unknown_protocol", ErrCursorResumeUnsupportedContent)
	}
}

// cursorGeminiToolChoiceMode 把 toolConfig.functionCallingConfig 映射为本站既有工具选择语义。
//
// AUTO → auto、NONE → none、ANY → required；allowedFunctionNames 必须全部在声明集合内
// （不凭名字扩大工具集），单名仍保持 required。未知模式/未声明名称明确 400（零 AgentRun）。
func cursorGeminiToolChoiceMode(body []byte) (string, string, error) {
	var doc struct {
		Tools      json.RawMessage `json:"tools"`
		ToolConfig *struct {
			FunctionCallingConfig *struct {
				Mode                 string   `json:"mode"`
				AllowedFunctionNames []string `json:"allowedFunctionNames"`
			} `json:"functionCallingConfig"`
		} `json:"toolConfig"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", "", fmt.Errorf("%w: undecodable gemini toolConfig", ErrCursorToolChoiceInvalid)
	}
	if doc.ToolConfig == nil || doc.ToolConfig.FunctionCallingConfig == nil {
		return "", "", nil
	}
	cfg := doc.ToolConfig.FunctionCallingConfig
	declared := cursorGeminiDeclaredToolNames(doc.Tools)
	declaredSet := make(map[string]struct{}, len(declared))
	for _, name := range declared {
		declaredSet[name] = struct{}{}
	}
	allowed := make([]string, 0, len(cfg.AllowedFunctionNames))
	for _, raw := range cfg.AllowedFunctionNames {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if _, ok := declaredSet[name]; !ok {
			return "", "", fmt.Errorf("%w: gemini allowedFunctionNames %q is not declared", ErrCursorToolChoiceInvalid, name)
		}
		allowed = append(allowed, name)
	}
	switch strings.ToUpper(strings.TrimSpace(cfg.Mode)) {
	case "", "MODE_UNSPECIFIED":
		if len(allowed) > 0 {
			return "", "", fmt.Errorf("%w: gemini allowedFunctionNames without ANY mode", ErrCursorToolChoiceInvalid)
		}
		return "", "", nil
	case "AUTO":
		if len(allowed) > 0 {
			return "", "", fmt.Errorf("%w: gemini allowedFunctionNames requires ANY", ErrCursorToolChoiceInvalid)
		}
		return "auto", "", nil
	case "NONE":
		if len(allowed) > 0 {
			return "", "", fmt.Errorf("%w: gemini allowedFunctionNames with NONE has no meaning", ErrCursorToolChoiceInvalid)
		}
		return "none", "", nil
	case "ANY":
		if len(declared) == 0 {
			return "", "", fmt.Errorf("%w: gemini ANY without declarations", ErrCursorToolChoiceInvalid)
		}
		// 单名 allowedFunctionNames 仍是 required（本站语义里 required 允许模型在该子集内选择）。
		return "required", "", nil
	default:
		return "", "", fmt.Errorf("%w: unknown gemini functionCallingConfig mode %q", ErrCursorToolChoiceInvalid, cfg.Mode)
	}
}

// cursorGeminiDeclaredToolNames 返回 tools[].functionDeclarations[].name（保持声明顺序）。
func cursorGeminiDeclaredToolNames(raw json.RawMessage) []string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var declarations []struct {
		FunctionDeclarations []struct {
			Name string `json:"name"`
		} `json:"functionDeclarations"`
	}
	if err := json.Unmarshal(raw, &declarations); err != nil {
		return nil
	}
	names := make([]string, 0, 4)
	for _, entry := range declarations {
		for _, decl := range entry.FunctionDeclarations {
			if name := strings.TrimSpace(decl.Name); name != "" {
				names = append(names, name)
			}
		}
	}
	return names
}

// cursorValidateToolChoiceForSession 按会话已暴露工具集合做**逐 HTTP** 校验。
//
// 规则（§25.14-D）：
//   - required / auto / none 可在**不改变已暴露工具集合**时切换；
//   - 具名选择必须落在**本 HTTP 声明的**工具集合内，不能指向未声明工具；
//   - 本 HTTP 的声明集合不再与首轮比对（五道闸退役，2026-11-07），客户端每轮
//     重新声明的工具集允许变化。
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
	case CursorResumeProtocolMessages:
		var req apicompat.AnthropicRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return newCursorResumeError(http.StatusBadRequest, "undecodable_continuation", ErrCursorResumeUnsupportedContent)
		}
		rawChoice = req.ToolChoice
	case CursorResumeProtocolResponses:
		var req apicompat.ResponsesRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return newCursorResumeError(http.StatusBadRequest, "undecodable_continuation", ErrCursorResumeUnsupportedContent)
		}
		rawChoice = req.ToolChoice
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
//
// 2026-11-07 五道闸退役后无调用方，保留注释说明退场原因：跨轮工具集比对
// 会把客户端合法变更（续轮少声明一个工具）误判为套件变更而拒绝，已不再执行。
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
	case "none":
		// tool_choice=none 表示本轮不得调用工具：真实输出出现任何工具调用即协议失败，
		// 绝不当成功交付（§25.17.2-3：可失败检查必须在发出工具成功批之前完成）。
		if len(calls) > 0 {
			return fmt.Errorf("%w: tool_choice none but the output carries %d tool call(s)", ErrCursorToolChoiceInvalid, len(calls))
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
	Seq         uint64
	CallIDs     []string
}

// sealedBatchSummaries 返回已封口批次的 (seq, model_call_id, 对外调用 ID 顺序) 快照。
//
// seq 为 0 的批次（只可能来自绕过 newBatchLocked 的测试夹具）在此按同一计数器补发：
// 生产路径的批次一律由 newBatchLocked 分配，不产生补齐分支。
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
		if batch.seq == 0 {
			d.nextSeq++
			batch.seq = d.nextSeq
		}
		summary := cursorDeliveredBatchSummary{ModelCallID: batch.modelCallID, Seq: batch.seq}
		for _, capture := range batch.delegations {
			wire := capture.wireID()
			if wire == "" {
				continue
			}
			summary.CallIDs = append(summary.CallIDs, wire)
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
	r.configDigest = plan.config.Clone()
	r.configSystemDigest = plan.configSystem.Clone()
	r.configSamplingDigest = plan.configSampling.Clone()
	if plan.history != nil {
		r.historyDigest = plan.history.Clone()
	}
	// 首轮工具元数据（custom / function / namespace / tool_search）随计划写入会话：
	// 续轮交付准备时据此把实际交付捕获还原为正确的工具类型（与 apicompat 回桥同源判定）。
	r.responsesTools = plan.responsesTools
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
		// 先按上游原 id 匹配（续轮已在 IdentifyResume/结果归位时把客户端 id 换回原 id）；
		// 再按客户端可见 id（WireID）兜底——绝不因为交付侧改写而把一条合法结果判成未知。
		for _, c := range caps {
			if c.ID == r.CallID {
				cap = c
				found = true
				break
			}
		}
		if !found {
			for _, c := range caps {
				if c.wireID() == r.CallID {
					cap = c
					found = true
					break
				}
			}
		}
		if !found {
			// 提交的结果 ID 无任何已登记关联：失败关闭、零上游写入（不伪造）。
			it.svc.finishCursorSession(it.session, "resume_unknown_call")
			return fmt.Errorf("%w: call id %q", ErrCursorResumeUnknownAssociation, r.CallID)
		}
		// callID 只用于日志/诊断（上游靠 ExecID + IDField 关联 exec）；这里用**上游原 id**，
		// 不用客户端回传的可见 id，避免日志与上游锚点口径不一致。
		frame := cursorEncodeMcpToolResult(cap.ExecID, cap.IDField, cap.ID, r.Text, r.Images, r.Structured, r.IsError)
		if err := it.duplex.send(cursorFldClientMessage, frame); err != nil {
			// 发送中传输失败 → 关闭会话（finalizer），禁止重连重放。
			it.svc.finishCursorSession(it.session, "resume_write_transport_failure")
			return newCursorRequestError(infraerrors.CursorExecResultWriteFailedCode, http.StatusBadGateway, "deliver tool result transport failure: %v", err)
		}
		it.session.noteStage("resume_result_written")
	}
	return nil
}

// cursorPreparedBatch 是"已准备但未确认交付"的工具批（§25.17.2-4）。
//
// 准备阶段只做：整批 ID 冲突检查 + 写同一索引 + 在摘要**克隆**上算好本批增量 +
// 把 pending 置为 delivered=false。正式历史与 waiting_tools 状态只在写出成功后提交。
type cursorPreparedBatch struct {
	batchID    string
	responseID string
	callIDs    []string
	// deliveredSeq 是本次交付批中**最大**的 cursorToolBatch.seq，提交时推进会话的
	// deliveredBatchSeq（准备阶段不推进）。
	deliveredSeq uint64
	// history 是已应用本批增量的历史摘要克隆（提交时才替换会话正式摘要）。
	history *CursorHistoryDigest
}

// prepareDeliveredBatch 在服务锁内完成整批可失败准备（仍 running、delivered=false）。
//
// "本次交付哪些调用"**只按批次序号 seq 判定**，不看 call_id、也不看索引是否已登记：
// 真实客户端跨轮复用同一个 tool_call_id（生产实测同一会话内 read 与 write 共用
// call-3466d322-...），按 ID 过滤会把新批整体吞掉（交付批为空 → 上游收不到结果 →
// stream_error）。seq 由 newBatchLocked 单调分配，免疫 ID 复用。
//
// 一旦失败，本 session 的全部关联由原 finalizer 清除；不会留下"半个批"。
func (s *CursorExecutionService) prepareDeliveredBatch(
	record *cursorSessionRecord,
	protocol CursorResumeProtocol,
	model string,
	assistantText string,
	delegations *cursorToolDelegations,
	responseID string,
) (*cursorPreparedBatch, error) {
	if s == nil || record == nil || delegations == nil {
		return nil, fmt.Errorf("%w: batch prepare without session", ErrCursorSessionIndexInvalidKey)
	}
	// 交付侧 id 重写（派发单 CALLID）：在**索引登记之前**为每个将要交付的 capture 决定
	// 出站 id。已被交付过的原 id → 换全新唯一 id 并记录 新id→原id；未交付过 → 沿用原值。
	// 必须在这里（而不是渲染时）完成：索引登记、pending.callIDs、摘要增量三处都要用同一个
	// 出站 id，且客户端只可能回传它。失败（id 源异常）→ 整批交付失败关闭，零成功字节。
	if err := delegations.assignWireIDs(); err != nil {
		return nil, err
	}
	batches := delegations.sealedBatchSummaries()
	if len(batches) == 0 {
		return nil, fmt.Errorf("%w: delegated checkpoint without sealed batch", ErrCursorBatchIncomplete)
	}
	current := batches[len(batches)-1]
	batchID := strings.TrimSpace(current.ModelCallID)
	if batchID == "" {
		batchID = cursorSessionLevelBatchID
	}
	responseID = strings.TrimSpace(responseID)

	// 与 beginResumeTakeover 同一锁序（会话表锁 → 会话锁）：不引入第二把锁。
	s.sessions.mu.Lock()
	defer s.sessions.mu.Unlock()
	live, ok := s.sessions.sessions[record.id]
	if !ok {
		return nil, fmt.Errorf("%w: session %q is not active", ErrCursorSessionIndexInvalidKey, record.id)
	}
	live.mu.Lock()
	defer live.mu.Unlock()
	if live.finishing || live.state == cursorSessionClosed {
		return nil, fmt.Errorf("%w: session finished before batch delivery", ErrCursorResumeNotWaiting)
	}
	// 本次 HTTP 真正交付给客户端的新增调用 = seq 大于已交付水位的已封口批的 call_id 并集。
	// 已封口批会跨轮保留在 d.all 里，历史轮已交付的批 seq <= deliveredBatchSeq，自然排除；
	// 同一 call_id 在新批中复用时因为批的 seq 是新的，仍会被正确计入（不被吞）。
	// 按 ID 去重（保序）：同一 call_id 若同时出现在两个新批里，客户端只可能回传一条结果，
	// pending.callIDs 必须与之一一对应，否则 cursorValidateResumeResults 会误判 missing_results。
	var deliveredCallIDs []string
	seenCallIDs := make(map[string]struct{})
	var deliveredSeq uint64
	for _, b := range batches {
		if b.Seq <= live.deliveredBatchSeq {
			continue
		}
		for _, callID := range b.CallIDs {
			if _, dup := seenCallIDs[callID]; dup {
				continue
			}
			seenCallIDs[callID] = struct{}{}
			deliveredCallIDs = append(deliveredCallIDs, callID)
		}
		if b.Seq > deliveredSeq {
			deliveredSeq = b.Seq
		}
	}
	// 没有任何新封口批 → 绝不构造空 pending（空批交付 = 上游收不到结果 → stream_error）。
	// 明确失败，让上层走失败链。
	if len(deliveredCallIDs) == 0 {
		return nil, fmt.Errorf("%w: batch checkpoint without new sealed batch", ErrCursorBatchIncomplete)
	}

	// F2 整改（外审 must_fix）：交付集合 = 登记集合。索引登记仍对**全部**已封口批的
	// call_id 做（不是只对新批）：对外输出可能包含多个已封口批的调用，全部登记才能保证
	// 续轮回传时能定位。同会话已登记（= 历史批或同 ID 复用）直接跳过以保持幂等；跨会话
	// 占用仍如实返回冲突错误。
	for _, b := range batches {
		for _, callID := range b.CallIDs {
			key := cursorSessionIndexKeyForSpec(live.spec, CursorIDKindCall, callID)
			if existing, le := s.sessions.index.Locate(key); le == nil && existing.SessionID == live.id {
				// 同会话已登记：幂等跳过（本批仍按 seq 计入 deliveredCallIDs，见上）。
				continue
			}
			if err := s.sessions.index.Register(key, live.id, batchID); err != nil {
				return nil, err
			}
		}
	}
	if responseID != "" {
		key := cursorSessionIndexKeyForSpec(live.spec, CursorIDKindResponse, responseID)
		if err := s.sessions.index.Register(key, live.id, batchID); err != nil {
			return nil, err
		}
	}
	// 摘要增量只在克隆上计算：提交前正式历史不推进（沿用最后一批 call_id 语义）。
	var history *CursorHistoryDigest
	// 把首轮工具元数据交给 delegations，供 appendCursorBatchAssistantHistory 的 Responses
	// 分支按实际交付语义分类（与 apicompat 回桥同源判定）。
	delegations.responsesTools = live.responsesTools
	if live.historyDigest != nil {
		history = live.historyDigest.Clone()
		if err := appendCursorBatchAssistantHistory(history, protocol, assistantText, current.CallIDs, delegations); err != nil {
			return nil, err
		}
	}
	// 登记本批已交付调用的实际语义（kind/name/namespace）：续轮结果条目类型选择
	// （appendCursorDeliveredResultsToHistory）与历史摘要同源复用，避免两侧摘要不一致。
	captures := delegations.snapshot()
	// 按**实际交付集合**（deliveredCallIDs，覆盖全部已封口批）逐个登记语义，而不只登记
	// 末批 current.CallIDs：续轮回传时每个已交付 call_id 都必须能查到 kind（否则
	// appendCursorDeliveredResultsToHistory 会判 result kind missing）。批次序号/幂等语义
	// 已在上方 deliveredCallIDs 去重与索引登记处保证，本循环只负责填充语义映射。
	for _, call := range deliveredCallIDs {
		var name string
		for _, c := range captures {
			if c.wireID() == call {
				name = c.Name
				break
			}
		}
		dt := cursorResponsesDeliveredToolCall(name, live.responsesTools)
		if live.deliveredToolCalls == nil {
			live.deliveredToolCalls = make(map[string]cursorDeliveredToolCall)
		}
		live.deliveredToolCalls[call] = dt
	}
	prepared := &cursorPreparedBatch{
		batchID:      batchID,
		responseID:   responseID,
		callIDs:      deliveredCallIDs,
		deliveredSeq: deliveredSeq,
		history:      history,
	}
	// 交付中：只能被识别为"已准备未交付"，不能据此接管或写结果（delivered=false）。
	live.pending = &cursorPendingBatch{
		batchID:    batchID,
		responseID: responseID,
		callIDs:    prepared.callIDs,
	}
	cursorPhaseLog(context.Background(), zapcore.InfoLevel, "session_batch_prepared", model,
		zap.String("session_id", live.id),
		zap.String("batch_id", batchID),
		zap.Int("call_count", len(prepared.callIDs)),
		zap.Bool("has_response_id", responseID != ""))
	return prepared, nil
}

// commitDeliveredBatch 在写出成功后（服务锁内）提交已交付批（§25.17.2-4/5）。
//
// 若 finalizer 已在锁内胜出，则**不得**再提交 waiting_tools：返回明确终止错误，
// 不追加成功帧、不恢复会话。
//
// 条目 ID 冲突检查（commitDeliveredBatchItems）必须在写出任何成功字节**之前**完成：
// Responses 流式路径由 cursorProtocolWriter.writeChunk 在本批转换后、写 SSE 前先登记
// （主）；此处是同一检查的幂等兜底与 nonstream 聚合写出前的登记。一旦冲突，返回原
// 错误、绝不写终止帧、不落 waiting_tools，保证失败即零成功字节（§25.17.2-3）。本方法
// 只做"写后提交 delivered 状态"，不再重复条目 ID 的 JSON 解码/ID 生成。
func (s *CursorExecutionService) commitDeliveredBatch(
	record *cursorSessionRecord, prepared *cursorPreparedBatch, itemIDs []string,
) error {
	if err := s.commitDeliveredBatchItems(record, prepared, itemIDs); err != nil {
		return err
	}
	return s.commitDeliveredBatchState(record, prepared)
}

// commitDeliveredBatchItems 把本 HTTP 实际交付的 Responses 工具结果条目 ID 与 call/response
// 用同一个 Register、同一把索引登记（键含 item 种类），并在写成功字节前完成冲突检查。
//
// 幂等语义：本 HTTP 的流式写出路径已在 cursorProtocolWriter.writeChunk 转换后、写 SSE 前
// 完成同一批 item 的登记（§25.17.2-3/4 锁内准备），此处对同一 session 同一批重复出现的
// 条目直接跳过，不把自己重复登记误判为其它会话的冲突；他人/他批占用仍视为冲突返回。
//
// 任一条冲突即返回错误（调用方据此在写出终止帧之前终止，实现"零成功字节"）。finalizer
// 清会话时随 DeleteSession 全清。无实际条目 ID（非 Responses 出口 / 空）时本步骤为空操作。
func (s *CursorExecutionService) commitDeliveredBatchItems(
	record *cursorSessionRecord, prepared *cursorPreparedBatch, itemIDs []string,
) error {
	if s == nil || record == nil || prepared == nil {
		return nil
	}
	record.mu.Lock()
	defer record.mu.Unlock()
	if record.finishing || record.state == cursorSessionClosed {
		return fmt.Errorf("%w: session finalized before item association", ErrCursorResumeNotWaiting)
	}
	return s.registerItemIDBatch(record, prepared.batchID, itemIDs)
}

// registerItemIDBatch 在 **调用方已持 record.mu** 时把本批工具结果条目 ID 登记进统一索引
// 并完成冲突检查。同一 session 同一批已登记的条目跳过；其它 session / 其它批占用即冲突。
func (s *CursorExecutionService) registerItemIDBatch(
	record *cursorSessionRecord, batchID string, itemIDs []string,
) error {
	for _, itemID := range itemIDs {
		id := strings.TrimSpace(itemID)
		if id == "" {
			continue
		}
		key := cursorSessionIndexKeyForSpec(record.spec, CursorIDKindItem, id)
		if entry, err := s.sessions.index.Locate(key); err == nil {
			if entry.OwnedBy(record.id) && entry.SameBatch(batchID) {
				continue
			}
			return fmt.Errorf("%w: key=%s (existing session=%s batch=%s)",
				ErrCursorSessionIndexConflict, key.String(), entry.SessionID, entry.BatchID)
		}
		if err := s.sessions.index.Register(key, record.id, batchID); err != nil {
			return err
		}
	}
	return nil
}

// registerLiveItemIDs 在本 HTTP 把**任何工具批字节写给客户端之前**（服务锁内）完成
// Responses 工具结果条目 ID 的登记与冲突检查（§25.17.2-3/4 锁内准备）。
//
// 供 cursorProtocolWriter.writeChunk 在转换出本批实际 item ID 后、写 SSE 之前调用：
// 一旦冲突即返回原错误，绝不把含该 item 的任一 SSE 帧写给客户端。batchID 取自本会话
// 已准备未交付的批（prepareDeliveredBatch 在发布工具批字节前已设置 session.pending）。
func (s *CursorExecutionService) registerLiveItemIDs(
	record *cursorSessionRecord, itemIDs []string,
) error {
	if s == nil || record == nil {
		return nil
	}
	record.mu.Lock()
	defer record.mu.Unlock()
	if record.finishing || record.state == cursorSessionClosed {
		return fmt.Errorf("%w: session finalized before item association", ErrCursorResumeNotWaiting)
	}
	batchID := ""
	if record.pending != nil {
		batchID = strings.TrimSpace(record.pending.batchID)
	}
	if batchID == "" {
		return fmt.Errorf("%w: item association without prepared batch", ErrCursorBatchIncomplete)
	}
	return s.registerItemIDBatch(record, batchID, itemIDs)
}

// commitDeliveredBatchState 在**完整写出成功**之后提交正式历史与 waiting_tools 状态。
//
// 只做已准备摘要替换与状态推进，不再次登记条目 ID（已在 commitDeliveredBatchItems 完成）。
// 批次序号水位 deliveredBatchSeq 也在**这里**随 delivered=true 一起推进：准备失败或写出
// 失败都不得推进，否则未真正交付的批会被永久跳过。
func (s *CursorExecutionService) commitDeliveredBatchState(
	record *cursorSessionRecord, prepared *cursorPreparedBatch,
) error {
	if s == nil || record == nil || prepared == nil {
		return nil
	}
	record.mu.Lock()
	defer record.mu.Unlock()
	if record.finishing || record.state == cursorSessionClosed {
		return fmt.Errorf("%w: session finalized before batch commit", ErrCursorResumeNotWaiting)
	}
	if prepared.history != nil {
		record.historyDigest = prepared.history
	}
	if prepared.deliveredSeq > record.deliveredBatchSeq {
		record.deliveredBatchSeq = prepared.deliveredSeq
	}
	if record.pending != nil && record.pending.batchID == prepared.batchID &&
		len(record.pending.callIDs) == len(prepared.callIDs) {
		record.pending.delivered = true
	} else {
		record.pending = &cursorPendingBatch{
			batchID:    prepared.batchID,
			responseID: prepared.responseID,
			callIDs:    append([]string(nil), prepared.callIDs...),
			delivered:  true,
		}
	}
	record.state = cursorSessionWaitingTools
	// §2.5：交付完整工具批后若客户端不续轮，会话会一直持有用户槽/账号槽与余额预留。
	// 进入 waiting_tools 即起表，到期且本批结果仍未消费即走唯一 finalizer 归还。
	s.armWaitingToolsIdleTimer(record)
	cursorPhaseLog(context.Background(), zapcore.InfoLevel, "session_batch_delivered", record.spec.requestedModel,
		zap.String("session_id", record.id),
		zap.String("batch_id", prepared.batchID),
		zap.Int("call_count", len(prepared.callIDs)),
		zap.Bool("has_response_id", prepared.responseID != ""))
	return nil
}

// recordDeliveredBatch 是"准备 + 提交"的同步组合（供无独立写出边界的调用方使用）。
//
// 该入口没有独立的写出边界，因此没有"实际写出内容"可取：只登记 call/response，
// 不伪造条目 ID（条目 ID 只能来自真实交付，§25.17.2「实际ID」）。
func (s *CursorExecutionService) recordDeliveredBatch(
	record *cursorSessionRecord,
	req *CursorExecutionRequest,
	delegations *cursorToolDelegations,
	responseID string,
) error {
	if s == nil || record == nil || req == nil {
		return nil
	}
	prepared, err := s.prepareDeliveredBatch(record, cursorResumeProtocolOfRequest(req), req.Model, "", delegations, responseID)
	if err != nil {
		return err
	}
	return s.commitDeliveredBatch(record, prepared, nil)
}

// appendCursorBatchAssistantHistory 把本批已交付的 assistant 输出并入**给定摘要**。
//
// assistantText 是本批**实际交付**的正文（见 cursorSessionRecord.batchAssistantText）。
// 它必须与续轮前缀摘要对同一逻辑内容的规范化完全同源，否则同一逻辑内容两侧摘要不同：
//
//	Chat      → chat.assistant.text（每条 assistant 消息一次，空正文同样登记）
//	Messages  → messages.assistant.text（仅非空；无文本块的消息不产生文本项）
//	Responses → responses.message.assistant（仅非空；无 message 条目不产生文本项）
//	Gemini    → gemini.model.text（仅非空；无文本 part 不产生文本项）
//
// 调用方传入的通常是克隆，提交前的失败不会污染正式历史。
func appendCursorBatchAssistantHistory(
	history *CursorHistoryDigest,
	protocol CursorResumeProtocol,
	assistantText string,
	callIDs []string,
	delegations *cursorToolDelegations,
) error {
	if history == nil {
		return nil
	}
	captures := delegations.snapshot()
	// 正文先于工具项：续轮规范化对一条 assistant 消息也是"文本项 → tool_call 项"。
	// Chat 例外仍按每条消息一次登记（含空正文），与 cursorPlanFromChat 逐项对齐。
	if protocol == CursorResumeProtocolChat {
		if err := history.AppendText("chat.assistant.text", assistantText); err != nil {
			return err
		}
	} else {
		if err := appendCursorBatchAssistantText(history, protocol, assistantText); err != nil {
			return err
		}
	}
	for _, call := range callIDs {
		// callIDs 是**发给客户端**的 id（WireID）。摘要必须按客户端可见 id 登记：续轮
		// 客户端回传的 assistant 消息里 tool_calls[].id 就是这个值，两侧必须同源，否则
		// 同一逻辑调用在交付侧与续轮侧会算出不同摘要（history_mismatch）。
		var name, args string
		for _, candidate := range captures {
			if candidate.wireID() == call {
				name, args = candidate.Name, candidate.ArgsJSON
			}
		}
		switch protocol {
		case CursorResumeProtocolChat:
			canonical, err := cursorToolCallCanonical(call, name, args)
			if err != nil {
				return err
			}
			if err := history.AppendItem("chat.assistant.tool_call", canonical); err != nil {
				return err
			}
		case CursorResumeProtocolMessages:
			// 与 cursorAnthropicBlockItem 的 tool_use 规范化同源：先按 JSON 规范化参数，
			// JSON 无法解析时原文按字节参与（custom 文本输入不改写）。两侧必须同源，
			// 否则键序/空白差异会让同一逻辑调用在交付侧与续轮侧算出不同摘要。
			canonicalArgs, argsErr := cursorCanonicalToolArgs(args)
			if argsErr != nil {
				canonicalArgs = CursorCanonicalText(args)
			}
			payload := map[string]json.RawMessage{
				"id":    mustCursorJSON(call),
				"name":  mustCursorJSON(name),
				"input": canonicalArgs,
			}
			if err := history.AppendItem("messages.assistant.tool_use", mustCursorJSONValue(payload)); err != nil {
				return err
			}
		case CursorResumeProtocolResponses:
			// 按本批实际交付项的语义分类登记（§25.14-D「实际 ID」）：function_call /
			// custom_tool_call / tool_search_call 用同一套 canonical，与续轮回传解析
			// （cursorResponsesItemItem）同源，保证两侧摘要逐字节相等。语义从实际交付
			// 捕获 + 首轮工具元数据派生（apicompat 同源判定），与 writer 写出侧一致。
			dt := cursorResponsesDeliveredToolCall(name, delegations.responsesTools)
			// custom 工具：客户端收到/回传的是**抽取后的自由文本 input**（custom_tool_call
			// 项的 input 字段），不是原始 arguments 包 {"input":"..."}。必须用与写出侧
			// 完全相同的抽取结果登记，否则两侧必然不同——生产实测该项内容差异，已由
			// history_first_divergence 定位（responses.custom_tool_call，idx=1/2）。
			payload := args
			if dt.Kind == "custom_tool_call" {
				payload = apicompat.ExtractCustomToolCallInput(args)
			}
			canonical, err := cursorResponsesToolCallHistoryCanonical(dt.Kind, call, dt.Name, dt.Namespace, payload)
			if err != nil {
				return err
			}
			if err := history.AppendItem("responses."+dt.Kind, canonical); err != nil {
				return err
			}
		case CursorResumeProtocolGemini:
			canonical, err := cursorToolCallCanonical(call, name, args)
			if err != nil {
				return err
			}
			if err := history.AppendItem("gemini.model.function_call", canonical); err != nil {
				return err
			}
		}
	}
	return nil
}

// appendCursorBatchAssistantText 按协议把非空 assistant 正文并入历史链。
//
// 只有非空才登记：续轮规范化在这些协议里不会为"没有文本块的消息/条目"产生文本项，
// 无条件登记空文本会造出反向不对称（交付侧多一项）。
func appendCursorBatchAssistantText(history *CursorHistoryDigest, protocol CursorResumeProtocol, text string) error {
	if text == "" {
		return nil
	}
	itemType := ""
	switch protocol {
	case CursorResumeProtocolMessages:
		itemType = "messages.assistant.text"
	case CursorResumeProtocolResponses:
		itemType = "responses.message.assistant"
	case CursorResumeProtocolGemini:
		itemType = "gemini.model.text"
	default:
		return nil
	}
	return history.AppendText(itemType, text)
}

// cursorWriterResponseID 取回桥 writer 实际交付的响应 ID（本 HTTP 的唯一输出 ID）。
func cursorWriterResponseID(w io.Writer) string {
	converted, ok := w.(*cursorProtocolWriter)
	if !ok || converted == nil {
		return ""
	}
	return strings.TrimSpace(converted.responseID)
}

// cursorCollectToolItemIDs 从本 HTTP 实际写出**的 SSE 字节里取"承载对外工具结果"的条目 ID。
//
// 覆盖非流式整包聚合场景：流式由协议 writer 直接登记（见 cursorCollectDeliveredItemIDs），
// 这里解析实际交付字节，作为非流式/被包装 writer 的兜底来源。
// 这保证 item 关联 Register+冲突检查在成功结束帧（response.completed）之前取到**真实** item
// ID，满足 §25.17.2-4「首个工具条目字节发出前完成关联登记」的反例（冲突返回时零成功字节）。
func cursorCollectToolItemIDs(sse []byte) []string {
	seen := make(map[string]struct{})
	var ids []string
	for _, line := range strings.Split(string(sse), "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev struct {
			Item *struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			} `json:"item"`
		}
		if json.Unmarshal([]byte(payload), &ev) != nil || ev.Item == nil {
			continue
		}
		if _, ok := responsesToolItemTypes[strings.ToLower(strings.TrimSpace(ev.Item.Type))]; !ok {
			continue
		}
		id := strings.TrimSpace(ev.Item.ID)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

// cursorCollectDeliveredItemIDs 汇合本 HTTP 实际交付的工具结果条目 ID：
//
//   - 流式：协议 writer 在写出 Responses 事件时通过 noteToolItemID 已登记（collectedItemIDs）；
//   - 非流式 / writer 被包装：从本 HTTP 实际写出字节（aggregated）解析兜底。
//
// 两者取并集去重，保证无论协议 writer 是否被外层 wrapper 包裹、无论流式还是非流式，
// item 关联 Register+冲突检查都能在成功结束帧之前取到真实 item ID（§25.17.2-4）。
func cursorCollectDeliveredItemIDs(w io.Writer, aggregated []byte) []string {
	seen := make(map[string]struct{})
	var ids []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if pw, ok := w.(*cursorProtocolWriter); ok {
		for _, id := range pw.collectedItemIDs() {
			add(id)
		}
	}
	for _, id := range cursorCollectToolItemIDs(aggregated) {
		add(id)
	}
	return ids
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
	// R2「先身份后资格」：在取回原账号后、任何会终结会话的资格处理之前证明请求所有权。
	// 外来身份（不同 user / key / 组）直接 403 identity_mismatch，零会话副作用——不得在
	// 身份核验前命中账号不合格分支（那条路径沿原 finalizer 终结原主人会话）。
	if denial := cursorResumeIdentityDenial(ticket, in.Identity); denial != nil {
		return nil, denial
	}
	// §25.17.3「A05续轮顺序」：定位到已登记会话并取回**本次读取**的原账号后、写工具
	// 结果之前做唯一只读资格核验；拒绝即零工具结果写入，沿原 finalizer 终结旧会话。
	// 此处的原主人账号不合格终结语义保留不变（身份已证明归属原主人）。
	if guardErr := s.cursorExecution.guardResumeEligibility(ctx, ticket, account); guardErr != nil {
		return nil, guardErr
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
	// R2「先身份后资格」：与 GatewayService 同一顺序——先证明请求所有权（外来身份 403、
	// 零会话副作用），再执行可终结会话的资格核验（两个入口只有一处实现，不另建平行链）。
	if denial := cursorResumeIdentityDenial(ticket, in.Identity); denial != nil {
		return nil, denial
	}
	// §25.17.3「A05续轮顺序」：与 GatewayService 同一核验、同一终结语义（两个入口只有
	// 一处实现，不另建平行链）。
	if guardErr := s.cursorExecution.guardResumeEligibility(ctx, ticket, account); guardErr != nil {
		return nil, guardErr
	}
	return s.cursorExecution.ResumeCursorSession(ctx, c, ticket, account, in)
}

// cursorResumeIdentityDenial 是续轮身份核验的唯一实现（R2「先身份后资格」）。
//
// 请求身份与会话登记身份不符（不同 user / key / 组，缺失维度不参与比较）即返回
// 403 identity_mismatch；nil = 归属匹配，可继续后续资格/接管。两个 ServeCursorResume
// 在**任何会终结会话的资格处理之前**调用它，外来身份绝不触发原主人账号不合格的
// 终结路径；ResumeCursorSession 也复用同一实现做入参层复核（不重复实现第二套逻辑）。
func cursorResumeIdentityDenial(ticket *CursorResumeTicket, in CursorSessionIdentity) error {
	if ticket == nil || ticket.Identity.compatibleWith(in) {
		return nil
	}
	return newCursorResumeError(http.StatusForbidden, "identity_mismatch", ErrCursorResumeIdentityMismatch)
}

// guardResumeEligibility 是**两个 ServeCursorResume 入口共用**的续轮资格接线
// （§25.17.3「A05续轮顺序」）。
//
// 位置固定在"已定位登记会话（ticket.session）+ 已取回本次读取的原账号"之后、
// "写工具结果"之前：核验本身在**服务锁外**（只读，不选号、不排队、不并发 Acquire、
// 不预留、不重计费），正式接管仍在既有服务锁内由 beginResumeTakeover 重查 state/batch。
//
// 拒绝即**零工具结果写入**：不进入接管、不写真实结果；旧会话一律沿**原 finalizer**
// 终结（唯一结算规则、预留归还、槽位释放与索引清理全部仍由 finishCursorSession 承担，
// 本函数不新增任何兜底或平行收尾）。拒绝本身沿既有续轮错误链返回，调用方用
// CursorResumeHTTPStatus 取回 403/400/503 与安全 reason。
func (s *CursorExecutionService) guardResumeEligibility(
	ctx context.Context, ticket *CursorResumeTicket, account *Account,
) error {
	if s == nil || ticket == nil || ticket.session == nil || account == nil {
		// 未定位到已登记会话/账号：交由 ResumeCursorSession 的既有入参错误链判定，
		// 本接线不重复、不替换那一层结论。
		return nil
	}
	denial := s.verifyResumeEligibility(ctx, ticket.session, account)
	if denial == nil {
		return nil
	}
	// 2026-11-04 用户裁定：账号侧容量/额度类拒绝（号被停、卡过期/额度用完/被换/被改、
	// 号被删、映射被改）一律不外显。终结旧会话后转成换号重建，用户在同一个 HTTP 里
	// 无缝接着跑；只有平台/分组类拒绝才作为用户错误返回。
	if denial.IsRebuild() {
		// 历史不完整（典型：Responses 只给 previous_response_id、没带正文历史）时不许重建：
		// 重开一局会丢上下文，比报错更糟（方案 §2.3）。这类仍按原拒绝返回。
		if ticket.probe == nil || !ticket.probe.Plan.historySupportsRebuild() {
			cursorPhaseLog(ctx, zapcore.WarnLevel, "session_resume_rebuild_skipped", ticket.Model,
				zap.String("session_id", ticket.SessionID),
				zap.String("deny_reason", denial.Reason),
				zap.String("skip_reason", "history_incomplete"))
			s.finishCursorSession(ticket.session, "resume_denied_"+denial.Reason)
			return denial
		}
		cursorPhaseLog(ctx, zapcore.InfoLevel, "session_resume_rebuild", ticket.Model,
			zap.String("session_id", ticket.SessionID),
			zap.String("batch_id", ticket.BatchID),
			zap.String("deny_reason", denial.Reason),
			zap.String("rebuild_reason", string(denial.Rebuild)),
			zap.Int64("dead_account_id", account.ID))
		// 与 P1 同一条收尾路径：同步 finalize 并等终结完成，避免重建撞上旧会话残留。
		s.settleDeadSessionBeforeRebuild(ticket.session)
		return &CursorResumeRebuildError{DeadAccountID: account.ID, Reason: denial.Rebuild, Err: denial}
	}
	cursorPhaseLog(ctx, zapcore.InfoLevel, "session_resume_denied", ticket.Model,
		zap.String("session_id", ticket.SessionID),
		zap.String("batch_id", ticket.BatchID),
		zap.Int("status", denial.Status),
		zap.String("deny_reason", denial.Reason))
	s.finishCursorSession(ticket.session, "resume_denied_"+denial.Reason)
	return denial
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
	case cursorProtocolGemini:
		return CursorResumeProtocolGemini
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
	case cursorProtocolGemini:
		return CursorResumeProtocolGemini
	default:
		return ""
	}
}

// cursorRequestModel 取续轮请求声明的客户端可见模型名（空 = 未声明）。
func cursorRequestModel(protocol CursorResumeProtocol, body []byte, inputModel string) (string, error) {
	// 优先使用传入的 URL 模型（Gemini 原生协议），否则从请求体解析。
	if strings.TrimSpace(inputModel) != "" {
		return strings.TrimSpace(inputModel), nil
	}
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

// cursorDeclaredOriginalToolNames 收集本请求客户端声明的**原始**工具名（保持声明顺序）。
//
// 出站 f4 里的工具名是 wire name（撞 Cursor 上游内置名时加了后缀，见
// cursorWireToolNameSet）。上游按 wire name 回调工具时，双工层需要把它映射回
// **客户端原名**再交付 —— 否则客户端收到一个自己从没声明过的名字。
//
// 这里给出原始名集合，双工层据此正向算 wire map 再反转（cursorWireNameReverse），
// 保证「改名」与「反查」用的是同一份规则（cursorWireToolNamesFromNames），
// 不会出现两边算出不同名字的情况。
//
// 没有工具、没有名字的工具都跳过（返回 nil）—— 双工层据此退化为原样登记，
// 保持既有语义。
func cursorDeclaredOriginalToolNames(chat *apicompat.ChatCompletionsRequest) []string {
	if chat == nil || len(chat.Tools) == 0 {
		return nil
	}
	out := make([]string, 0, len(chat.Tools))
	for _, t := range chat.Tools {
		if t.Function == nil {
			continue
		}
		if name := strings.TrimSpace(t.Function.Name); name != "" {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}


// cursorBuildAgentRunUpstreamRequest 构造 AgentRun 的出站请求描述。
//
// 除了转发所需的基本材料，它还带上**客户端声明的原始工具名**
// （cursorDeclaredOriginalToolNames）—— 出站 f4 里的工具名是 wire name
// （撞 Cursor 上游内置名时加了后缀），上游按 wire name 回调时，双工层要靠这份
// 原始名把它映射回客户端原名再交付。
//
// 单独成函数是为了让「原始名有没有真的被带上」这一条**可被单测直接覆盖**：
// mutation 掉这里的赋值，单测立刻红（2026-11-07 mutation 反证得出：
// 只测纯函数的话，生产路径漏填了测试照样绿）。
func cursorBuildAgentRunUpstreamRequest(
	req *CursorExecutionRequest,
	payload []byte,
	authProvider CursorUpstreamAuthProvider,
	upstreamModel string,
	delegations *cursorToolDelegations,
	headers map[string]string,
) *CursorUpstreamRequest {
	return &CursorUpstreamRequest{
		Path:              cursorPathAgentRun,
		Body:              payload,
		AuthProvider:      authProvider,
		Model:             upstreamModel,
		ToolDelegations:   delegations,
		Headers:           headers,
		DeclaredToolNames: cursorDeclaredOriginalToolNames(req.Chat),
	}
}
