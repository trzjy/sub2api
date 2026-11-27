package service

// 本文件实现 **Cursor AgentRun 响应帧 → OpenAI Chat Completions SSE** 的转换
// （P1-A1 卡，方案 §15.2 / §15.4）。取证来源（tapgpu 本机可回读）：
//   - /root/cursor-p1-evidence/32-agent-protocol-schemas.json：agent.v1.AgentServerMessage
//     / InteractionUpdate / TextDeltaUpdate 的字段号（真实客户端提取，**字段号以此为准**）
//   - 40-extra-schemas.json / 41-turn-ended-decoded.json：TurnEndedUpdate 的 usage 字段
//     （活体解码：33 号样本 input=12601 / output=15 / cache_read=128 / reasoning=11）
//   - 33/37/45/47/51/53/54/63-current-protocol-results.json：真实回包事件序列
//     （exec_request → context_response → kv_set×N → text → interaction(25) →
//      server_message(8) → interaction(17) → kv_set×N → turn_ended）
//
// ⚠️ **框架差异（已被活体证伪的坑）**：对话响应**带** envelope
// （`Content-Type: application/connect+proto`），而模型列表响应是**裸 protobuf**
// （`Content-Type: application/proto`）——**两者不可共用解析器**。
// 本文件**只**处理对话流的 envelope 形态。
//
// 本文件三条硬红线：
//  1. **usage 只有一个来源**：InteractionUpdate.turn_ended 的 input/output/
//     cache_read/cache_write tokens。拿不到就发**显式零值**，绝不估算、绝不伪造。
//  2. **流内拒付不得当成功**：HTTP 200 正常帧里出现固定模板
//     「密钥已过期或额度耗尽，请续费后使用」（证据 53 / 125 / 134）时，返回
//     ErrCursorKeyRejectedUpstream 哨兵，绝不产出 usage、绝不回写成功响应。
//  3. **取消 / 终态错误不得固定为成功**：流没有终态标记即失败关闭。

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// cursorSSEStream converts frames as they arrive; it never buffers the complete
// model reply before exposing text. The AgentRun transport handles context/KV.
type cursorSSEStream struct {
	source io.ReadCloser
	parser *cursorStreamParser
	buffer bytes.Buffer
	done   bool
	// delegated 标记本轮已吐出 tool_calls + finish + [DONE] 的委托检查点（模型正等待
	// 真实结果）；读循环据此暂停于本次 HTTP，待续轮写回结果后恢复。仅在"本批已可封口
	// 且还有未吐出的委托"时置位，续轮恢复后不会重复吐已交付的 tool_calls。
	delegated bool
	// emittedCalls 是已经吐给客户端的对外委托条数（跨多轮工具批次累计）。
	//
	// 用它（而不是一个一次性 bool）判定"是否还有未交接的委托"：会话可能是
	// running → waiting_tools → running 多轮循环，第二轮的新批委托必须能再次交接，
	// 而上一轮已交付的委托绝不重播（§25.3「每次 HTTP 只输出自本次附着以来的新内容」）。
	emittedCalls int
	errMu        sync.Mutex
	err          error

	// lastSealErr 缓存最近一次"交接判定处" sealableDelegations() 返回的非 nil 错误
	// （封口失败的具体原因：缺 listed / 计数不等 / 某 started 无完整 f11 委托等）。
	//
	// 交接判定**不改变任何控制流**：仍不交接、不 dountil、不提前结束。next() 在交接
	// 检查点把 sealableDelegations() 的非 nil 错误**记录**到这里，供终态可观测性使用——
	// 上层原本只能看到 deadline_exceeded，无法从日志区分"缺 listed / 计数
	// 不等 / 某 started 无委派"（外审建议 S1）。
	//
	// 语义：每帧覆盖为最新值即可，无需历史队列；交接成功（sealable 通过）时清空。
	// 与 err/errMu 复用同一把锁。
	lastSealErr error

	// frameIndex 是当前已成功读到的帧计数（从 1 开始），仅用于给**读失败**的帧级错误
	// 标注"第几帧坏了"。next() 每次进入前把"本次尝试读取的帧序号"写进错误，
	// 生产日志因此能给出 protocol_frame_index=N（见 cursor_frame_error.go）。
	frameIndex int
}

func (s *cursorSSEStream) Close() error { return s.source.Close() }

// terminalError may be read after the gateway's timeout path returns while its
// scanner is still unwinding. Parser state belongs exclusively to that scanner.
func (s *cursorSSEStream) terminalError() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

// lastSealErrReason 返回最近一次"交接判定处"封口失败的原因（线程安全；nil 表示无/已清空）。
//
// 见 lastSealErr 字段注释：它缓存 sealableDelegations() 在交接检查点返回的非 nil 错误，
// 供外审建议 S1 的终态可观测性使用。与 err/errMu 复用同一把锁。
func (s *cursorSSEStream) lastSealErrReason() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.lastSealErr
}

// setLastSealErr 记录（或清空）最近一次封口失败原因：每帧覆盖为最新值，传入 nil 表示
// 本次交接判定通过、阻塞原因已消除。与 err/errMu 复用同一把锁。
func (s *cursorSSEStream) setLastSealErr(err error) {
	s.errMu.Lock()
	s.lastSealErr = err
	s.errMu.Unlock()
}

func (s *cursorSSEStream) Read(dst []byte) (int, error) {
	if len(dst) == 0 {
		return 0, nil
	}
	for s.buffer.Len() == 0 && !s.done && s.terminalError() == nil {
		if err := s.next(); err != nil {
			s.errMu.Lock()
			s.err = err
			s.errMu.Unlock()
		}
	}
	if s.buffer.Len() > 0 {
		return s.buffer.Read(dst)
	}
	if err := s.terminalError(); err != nil {
		return 0, err
	}
	return 0, io.EOF
}

func (s *cursorSSEStream) next() (err error) {
	// 帧序号：本次尝试读取的是第 frameIndex+1 帧。读成功才递增；失败时把序号
	// 标注进帧级错误（只写整数，不含帧内容），生产日志据此给出 protocol_frame_index。
	attempt := s.frameIndex + 1
	defer func() {
		if err != nil {
			err = cursorAnnotateFrame(err, attempt)
			return
		}
		s.frameIndex = attempt
	}()
	header := make([]byte, cursorEnvelopeHeaderSize)
	if _, err := io.ReadFull(s.source, header); err != nil {
		if err == io.EOF {
			return ErrCursorMissingEndStream
		}
		return cursorFrameErrorWrapf(cursorFrameEnvelope, err, "read envelope: %v", err)
	}
	n := int(binary.BigEndian.Uint32(header[1:]))
	if n > cursorUpstreamStreamMaxBytes {
		return cursorFrameErrorf(cursorFrameEnvelope, "oversized envelope")
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(s.source, payload); err != nil {
		return cursorFrameErrorWrapf(cursorFrameEnvelope, err, "read payload: %v", err)
	}
	frame, _, err := cursorReadEnvelopeFrame(append(header, payload...), 0)
	if err != nil {
		return err
	}
	if frame.cursorIsEndStream() {
		code, msg, err := cursorParseEndStream(frame.Data, s.parser.secrets...)
		if err != nil {
			return err
		}
		if code != "" {
			return &cursorUpstreamStreamError{Code: code, Message: msg}
		}
		s.done = true
	} else {
		ev, err := s.parser.cursorParseMessageFrame(frame.Data)
		if err != nil {
			return err
		}
		if ev.Kind == cursorEventText {
			// content 只收**归一后**的可见正文：CallDynamicTool 私有块在文本帧边界
			// 被扣留并转成结构化调用，不再作为正文交付。rawContent 仍收原始文本，
			// 保证上游注入的拒付/模型暂不可用模板判定灵敏度不变。
			safeText, inlineErr := s.parser.consumeInlineText(ev.Text)
			if inlineErr != nil {
				return inlineErr
			}
			s.parser.content += safeText
			if err := cursorMatchKeyRejected(s.parser.rawContent); err != nil {
				return err
			}
			// 双通道发射：思考走 reasoning_content，正文走 content，二者都即时到达。
			if ev.Reasoning != "" {
				s.emitStreamReasoning(ev.Reasoning)
			}
			if safeText != "" {
				s.emitStreamText(safeText)
			}
		}
		s.done = s.parser.usage.Seen
	}
	if s.done {
		if err := cursorMatchParameterUnsupported(s.parser.httpRawContent()); err != nil {
			return err
		}
	}
	// 委托检查点（§25.3）：模型在本轮 HTTP 内发出 mcp_args 委托并暂停等待真实结果
	// （usage 尚未见到，故 s.done 原本为 false）。此时必须**主动**吐出 tool_calls +
	// finish + [DONE] 给当前 HTTP（不再依赖旧链回 McpResult.error 诱导模型结束），并把
	// 本轮 HTTP 标记为 delegated 以便读循环暂停、连接保持存活至续轮。
	//
	// 两个前置条件缺一不可：
	//  1. 还有**未吐出的**对外委托（emittedCalls 计跨轮累计，已交付的不重播）；
	//  2. 本批**已可封口**（listed 数等齐、每个对外 started 都已解码出完整 f11 委托）。
	//     未齐时继续等后续帧——绝不按第一个 mcp_args 提前交接半个批次（同名并行两工具、
	//     listed 晚到、started 与委托交错都靠这一步）。
	handoffThisFinish := false
	if !s.done {
		if calls := s.parser.capturedToolCalls(); len(calls) > s.emittedCalls {
			if sealErr := s.parser.sealableDelegations(); sealErr == nil {
				// 交接成功：清空封口失败原因（每帧覆盖语义，成功即无阻塞）。
				s.setLastSealErr(nil)
				s.delegated = true
				s.done = true
				handoffThisFinish = true
				// 代际复位（外审 F1）：本次 HTTP 已向下游交接工具委托并暂停，后续帧
				// 属于新一代 model call（真实续轮是新的 AgentRun 代），旧批归属计数
				// （尤其是 listedSeen）**不得跨代沿用**——否则同 model_call_id 的续轮
				// 会在 noteToolRequestsListed 看到 listedSeen==true 且计数已齐，把新
				// listed 挂起为 pending → 新批永远拿不到 listed → 封口谓词永不通过 →
				// 挂死。只清解析器本地归属计数，不碰 cursorToolDelegations（封口
				// 记录按代维护、续轮开新批，见 TestCursorBatchSealedBatchNotReused）。
				s.parser.resetBatchGeneration()
				s.parser.beginHTTPTurn()
			} else {
				// 交接判定处 sealable 失败：仅记录原因、不改变任何控制流——仍不交接、不
				// dountil、不提前结束，继续等后续帧。外审建议 S1：原本此处把错误直接丢弃，
				// 上层只能看到 deadline_exceeded，无法从日志区分"缺 listed /
				// 计数不等 / 某 started 无完整 f11 委托"。原因缓存到 lastSealErr，由终态日志
				// 以安全字段（如 seal_blocked_reason）写出（只含错误类别与计数，不含正文）。
				s.setLastSealErr(sealErr)
			}
		}
	}
	if s.done {
		// 归一器的扣留尾段先收口：开标记已识别但闭标记始终未到 → 失败关闭，
		// 绝不把半成品控制块当正文交付，也不正常 end_turn。
		inlineTail, inlineErr := s.parser.finishInlineText()
		if inlineErr != nil {
			return inlineErr
		}
		// 收尾尾段：<think> 剥离的跨帧缓冲（双通道）在这里吐完。
		// 必须在 finish 之前写出，否则正文/思考末尾几个字符会丢。
		// 顺序：先 reasoning 尾巴再 text 尾巴（二者通常互斥，这里按通道分别落盘）。
		rTail, tTail := s.parser.flushThinkTail()
		if inlineTail != "" {
			tTail += inlineTail
			s.parser.content += inlineTail
		}
		if rTail != "" || tTail != "" {
			if !s.parser.started {
				s.buffer.WriteString(s.parser.cursorSSERoleChunk())
				s.parser.started = true
			}
			if rTail != "" {
				s.parser.markFirstToken()
				s.buffer.WriteString(s.parser.cursorSSEReasoningChunk(rTail))
			}
			if tTail != "" {
				s.parser.markFirstToken()
				s.buffer.WriteString(s.parser.cursorSSETextChunk(tTail))
			}
		}
		// 交付侧 id 重写（派发单 CALLID）：在写出**任何** tool_calls 字节之前决定出站 id。
		// 失败即返回错误（本 HTTP 不交出带 id 的工具批），绝不发原 id 冒充新调用。
		if err := s.parser.assignToolCallWireIDs(); err != nil {
			return err
		}
		if !s.parser.started {
			s.buffer.WriteString(s.parser.cursorSSERoleChunk())
			s.parser.started = true
		}
		// 工具委托捕获：在 finish 前吐 tool_calls delta（首个含 id/type/function.name，
		// 随后一块 arguments），再发 finish_reason:"tool_calls" 的收尾 chunk。
		// 续轮恢复后的终态不再重复吐已交付的 tool_calls，只发 finish。
		//
		// 序号纪律：tool_calls[].index 必须**按本 HTTP 从 0 重排**，不得沿用全局累计
		// 序号（emittedCalls）。每个 HTTP 响应是客户端眼中的一条独立 assistant 消息，
		// 序号是"本条消息内第几个工具调用"。若续轮沿用全局序号（第 2 个调用发 index=1
		// 而本 HTTP 没有 index=0），非流式聚合会为 index 0 补一条空壳调用，客户端收到
		// `{id:"",name:"",arguments:"{}"}` 的幽灵工具调用——既无法回传结果，又会让续轮
		// 摘要与交付侧不一致（history_mismatch）。
		allCalls := s.parser.capturedToolCalls()
		nativeThisHTTP := len(allCalls) - s.emittedCalls
		if nativeThisHTTP > 0 {
			for i := 0; i < nativeThisHTTP; i++ {
				s.buffer.WriteString(s.parser.cursorSSEToolCallNameChunk(i, allCalls[s.emittedCalls+i]))
				s.buffer.WriteString(s.parser.cursorSSEToolCallArgsChunk(i, allCalls[s.emittedCalls+i]))
			}
			s.emittedCalls = len(allCalls)
			handoffThisFinish = true
		}
		// 内联调用与原生调用共用本 HTTP 的 tool_choice 输出闸门；纯内联响应不会
		// 经过原生委托检查点，必须在这里校验，否则 none/具名选择会被绕过。
		if err := s.parser.enforceInlineToolChoice(allCalls); err != nil {
			return err
		}
		// 正文内联归一出的调用：与普通委托同形输出结构化 tool_calls，但**不进入**
		// cursorToolDelegations（无 ExecID/IDField，不假装有原生结果回写锚点）。
		// 下一轮由客户端真实 tool_use + tool_result 完整历史走既有重建/续轮。
		inlineChunks := s.parser.cursorInlineToolCallChunks(nativeThisHTTP)
		for _, chunk := range inlineChunks {
			s.buffer.WriteString(chunk)
		}
		if len(inlineChunks) > 0 {
			handoffThisFinish = true
		}
		if handoffThisFinish {
			s.buffer.WriteString(s.parser.cursorSSEFinishChunkWithReason("tool_calls"))
		} else {
			// 显式归因：续轮终态即使历史批里有委托也必须是 "stop"（已交付的不重播）。
			s.buffer.WriteString(s.parser.cursorSSEFinishChunkWithReason("stop"))
		}
		s.buffer.WriteString("data: [DONE]\n\n")
	}
	return nil
}

// emitStreamText 写出正文增量：正文就是正文，即时全额写出，不扣留、不改写。
// 首包带 role、markFirstToken 记录首 token 耗时，空增量直接跳过。
func (s *cursorSSEStream) emitStreamText(delta string) {
	if delta == "" {
		return
	}
	if !s.parser.started {
		s.buffer.WriteString(s.parser.cursorSSERoleChunk())
		s.parser.started = true
	}
	s.parser.markFirstToken()
	s.buffer.WriteString(s.parser.cursorSSETextChunk(delta))
}

// emitStreamReasoning 写出思考增量 chunk（reasoning_content 通道）。
//
// 与 emitStreamText 同构：首包带 role、markFirstToken 记录首 token 耗时，但不扣留
// 末尾字节（思考内容必须即时到达，否则下游感知为"思考卡死"）。空增量直接跳过。
func (s *cursorSSEStream) emitStreamReasoning(delta string) {
	if delta == "" {
		return
	}
	if !s.parser.started {
		s.buffer.WriteString(s.parser.cursorSSERoleChunk())
		s.parser.started = true
	}
	s.parser.markFirstToken()
	s.buffer.WriteString(s.parser.cursorSSEReasoningChunk(delta))
}

// agent.v1.AgentServerMessage（32 号证据：KW）的 oneof 字段号。
const (
	cursorFldServerInteractionUpdate = 1 // InteractionUpdate —— 文本与 usage 的唯一落点
	cursorFldServerExecMessage       = 2 // ExecServerMessage —— 首版不响应（无执行通道）
	cursorFldServerCheckpoint        = 3 // ConversationStateStructure —— 丢弃
	cursorFldServerKvMessage         = 4 // KvServerMessage —— 丢弃（单向 POST 无法应答 KV 问答）
	cursorFldServerExecControl       = 5 // ExecServerControlMessage —— 丢弃
	cursorFldServerInteractionQuery  = 7 // InteractionQuery —— 由 duplex 应答，见 respondInteractionQuery
	cursorFldServerTTFTBreakdown     = 8 // TtftBreakdown —— 丢弃（可能含定长字段，故**不解码**其内容）
)

// agent.v1.InteractionUpdate（32 号证据：Yb）的 oneof 字段号。
const (
	cursorFldUpdateTextDelta = 1  // TextDeltaUpdate —— 增量文本
	cursorFldUpdateTurnEnded = 14 // TurnEndedUpdate —— 终态 + usage

	// cursorFldUpdateToolCallStarted 是 InteractionUpdate.tool_call_started（f2）。
	//
	// 依据：32 号证据 InteractionUpdate schema 逐字段 dump（no:2 name:"tool_call_started"）。
	cursorFldUpdateToolCallStarted = 2
	// cursorFldUpdateToolRequestsListed 是 InteractionUpdate.tool_requests_listed（f27）。
	//
	// 依据：32 号证据 InteractionUpdate schema 逐字段 dump（no:27
	// name:"tool_requests_listed"）。批次封口以它为"本批总数"的唯一来源（§25.3）。
	cursorFldUpdateToolRequestsListed = 27
	// cursorFldUpdatePartialToolCall 是 InteractionUpdate.partial_tool_call（f7）。
	//
	// 依据：32 号证据 schema dump（no:7 name:"partial_tool_call"）。按 §25.3 它只承载
	// 增量内容，**不是**完整参数、不可当作可执行委托，故本层不解码、不作为封口依据。
	cursorFldUpdatePartialToolCall = 7
	// cursorFldUpdateToolCallCompleted 是 InteractionUpdate.tool_call_completed（f3）。
	cursorFldUpdateToolCallCompleted = 3
)

// agent.v1.ToolRequestsListedUpdate（design-evidence.md / 02-native-schemas.json）内：
//
// 内层**只有** `no:1 name:"call_count" kind:"scalar" T:13`（uint32），**没有**
// model_call_id 字段——归属只能靠当前开放批次上下文判定（§25.3）。
const (
	// cursorFldToolRequestsListedCallCount 是 ToolRequestsListedUpdate.call_count（f1, uint32）。
	cursorFldToolRequestsListedCallCount = 1
)

// agent.v1.ToolCallStartedUpdate（活体 tool_call_started 真实帧）字段号：
//
//   - f1  call_id（string）—— 对外委托 ID（= OpenAI tool_calls[].id）；
//   - f2  ToolCall 消息 —— 承载 mcp_tool_call / get_mcp_tools 分支；
//   - f3  model_call_id（string）—— 本批归属键（真实帧才有）；
//   - ToolCall 内层 f15 = mcp_tool_call（**对外**委托）；f44 = get_mcp_tools（**内部**
//     元工具，只参与同批完整性判断，绝不交给客户端）。
//
// 仅认真实帧布局（派发单 B1b：旧合成帧布局已随 fcToolStarted 一并归零，双模兼容分支删除）。
const (
	// cursorFldToolCallID 是 call_id 字段号（f1）：真实帧在 started.f1。
	cursorFldToolCallID = 1
	// cursorFldToolCallStartedToolCallMsg 是 started.f2（ToolCall 消息）。
	cursorFldToolCallStartedToolCallMsg = 2
	// cursorFldToolCallStartedModelCallID 是 started.f3（model_call_id）。
	cursorFldToolCallStartedModelCallID = 3
	// cursorFldToolCallStartedToolCall 是 **字段号 15**：ToolCall 内层的 mcp_tool_call
	// 分支（对外）。解析器据此判定 external=true。
	cursorFldToolCallStartedToolCall = 15
	// cursorFldToolCallGetMcpTools 是 ToolCall 内层的 get_mcp_tools 分支（f44，内部元工具）。
	cursorFldToolCallGetMcpTools = 44
)

// agent.v1.TextDeltaUpdate（32 号证据：Gyt）内：
const (
	cursorFldTextDeltaText           = 1 // string
	cursorFldTextDeltaIsServerNotice = 2 // bool [opt]
)

// agent.v1.TurnEndedUpdate（40 号证据 / 41 号活体解码）内：
const (
	cursorFldTurnEndedInputTokens      = 1 // int64 [opt]
	cursorFldTurnEndedOutputTokens     = 2 // int64 [opt]
	cursorFldTurnEndedCacheReadTokens  = 3 // int64 [opt]
	cursorFldTurnEndedCacheWriteTokens = 4 // int64 [opt]
	cursorFldTurnEndedReasoningTokens  = 5 // int64 [opt]
	cursorFldTurnEndedEndedAtMS        = 6 // uint64 [opt]
)

// Cursor 上游错误（业务级与传输级）。
var (
	// ErrCursorUpstreamStream 表示 Cursor 流本身报告了终态错误（EndStream.error）。
	ErrCursorUpstreamStream = errors.New("cursor: upstream stream reported an error")

	// ErrCursorKeyRejectedUpstream 是**流内拒付哨兵**（契约 0.2，供 P1-A2 / P1-S 消费）。
	//
	// 触发条件：HTTP 200 + 正常帧序列（text_delta + turn_ended）中出现固定模板
	// 「密钥已过期或额度耗尽，请续费后使用」（证据 53 / 125-expiry-recheck /
	// 134-current-token-expired-key，三份样本的模板完全一致，仅后缀轮次标识不同）。
	// 命中即**终态错误**：不得当模型成功、不得伪造 usage。
	ErrCursorKeyRejectedUpstream = errors.New("cursor: upstream rejected the key (expired or quota exhausted)")

	// ErrCursorUpstreamModelUnavailable 是**流内模型暂不可用哨兵**（方案 §2.4）。
	//
	// 触发条件：正文累计内容出现固定模板「当前模型暂不可用，请发送 继续 或新开对话」。
	// 与拒付区分：拒付是账号额度问题（要停号），本错误是上游临时故障（不停号）。
	// 命中即终态错误：不得当模型成功、不得伪造 usage。
	ErrCursorUpstreamModelUnavailable = errors.New("cursor: upstream model temporarily unavailable")

	// ErrCursorQuotaExhaustedInStream 是**流内「今日额度已用完」模板**哨兵（RL-STOP 同族）。
	//
	// 触发条件：正文累计内容出现固定模板「今日额度已用完」（2026-11-03 生产活体）。
	//
	// 2026-11-06 定性更正：这个模板**不是余额判定**，是上游对单个账号状态异常的挡板话术。
	// 生产三重排他证据（见 downstream_messages.go CUR-EXEC-11 与
	// antigravity_gateway_retry.go cursorQuotaExhaustedCooldown）：
	//   - 命中时累计用量 0 ～ 405 万 token 不等，其中一次 24 小时零请求也命中；
	//   - 同码同日 340 号跑 49 次从未命中，343 号只跑 3 次就命中；
	//   - 343 命中那一刻，同码 340 号正在连续成功。
	//
	// 故处置改为：短冷却（5 分钟）+ 换号，**不再按日停调**，对外文案也不再声称额度耗尽。
	// 真·额度/凭据耗尽走拒付模板 ErrCursorKeyRejectedUpstream（CUR-EXEC-12）。
	// 命中仍是终态错误：不得当模型成功、不得伪造 usage、不得把上游原文（含卡 ID）透传。
	ErrCursorQuotaExhaustedInStream = errors.New("cursor: upstream claimed account daily quota exhausted (unverified)")

	// ErrCursorUpstreamParameterUnsupported 表示上游完整回复了固定的模型参数拒绝模板。
	// 该终态不能继续当作成功回复或进入正常结算。
	ErrCursorUpstreamParameterUnsupported = errors.New("cursor: upstream model parameters unsupported")
)

// cursorUpstreamStreamError 是 ErrCursorUpstreamStream 的结构化载体，携带上游 EndStream 的
// 业务错误 code 与脱敏 message，供调用方（如 RL-STOP 限流停号）精确判定错误类别。
// 通过 Unwrap 保持与 ErrCursorUpstreamStream 的 errors.Is 兼容（cursorStreamErrClass 等既有判定不受影响）。
type cursorUpstreamStreamError struct {
	// Code 是上游 EndStream.error.details[].debug.error（如 ERROR_PRO_USER_RATE_LIMIT_EXCEEDED）。
	Code string
	// Message 是上游回吐的脱敏 message（不回显原始文案）。
	Message string
}

func (e *cursorUpstreamStreamError) Error() string {
	return "cursor: upstream stream reported an error: " + e.Code + ": " + e.Message
}

func (e *cursorUpstreamStreamError) Unwrap() error { return ErrCursorUpstreamStream }

// cursorUpstreamStreamErrorCode 从上游流终态错误中提取业务错误 code。
// 非 ErrCursorUpstreamStream 错误返回 ("", false)。供 RL-STOP 限流停号判定使用。
func cursorUpstreamStreamErrorCode(err error) (string, bool) {
	var e *cursorUpstreamStreamError
	if errors.As(err, &e) {
		return e.Code, true
	}
	return "", false
}

// cursorUpstreamStreamErrorMessage 从上游流终态错误中提取已脱敏的 message。
// 非 ErrCursorUpstreamStream 错误返回 ("", false)。
//
// 用途：cursor_upstream_account_failure 日志透传上游原文（如
// "Cannot read properties of undefined (reading 'toString')"），
// 让「上游自身故障」与「我们的请求触发上游 bug」可区分 ——
// 此前 message 只在 Error() 里被拼进字符串，无人读取，线上完全看不到。
func cursorUpstreamStreamErrorMessage(err error) (string, bool) {
	var e *cursorUpstreamStreamError
	if errors.As(err, &e) {
		return e.Message, true
	}
	return "", false
}

// CursorUpstreamAccountFailureReason 标识「cursor 上游对本账号返回非业务码终态错误」的
// 换号原因。handler 侧据此对账号做临时停调冷却（见 GatewayService.TempUnscheduleRetryableError），
// 与其他平台的换号原因共用同一分类点与同一停号原语，不另建通道。
const CursorUpstreamAccountFailureReason = GatewayFailureReason("cursor_upstream_account_failure")

// isCursorBusinessErrorCode 判定上游终态错误 code 是否为**业务码**（`ERROR_` 前缀，例如
// ERROR_PRO_USER_RATE_LIMIT_EXCEEDED / ERROR_BAD_MODEL_NAME / ERROR_UNAUTHORIZED）。
//
// 业务码各自有专门语义与用户文案，必须由调用方单独处理；其余形态是 Connect 传输码
// （internal / unknown / unavailable / deadline_exceeded …），表示上游/账号当下不可服务，
// 属账号级故障，走换号 + 冷却。空串按传输码处理（cursorParseEndStream 会归一为 unknown）。
func isCursorBusinessErrorCode(code string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(code)), "ERROR_")
}

// cursorUpstreamRejectTemplate 是上游回吐的**固定**拒付模板（不含动态后缀）。
//
// 动态后缀形如 ` (1792310906551-b924f5ef)`：毫秒时间戳 + 十六进制轮次标识，
// 每次请求都不同，故匹配只认固定前缀，片段原样留存供诊断。
const cursorUpstreamRejectTemplate = "密钥已过期或额度耗尽，请续费后使用"

// CursorKeyRejectedError 是 ErrCursorKeyRejectedUpstream 的结构化载体。
//
// 结构化字段的用途：调用方（P1-A2 的桥接层）要把"这次失败到底是上游拒付还是
// 协议/网络问题"写进 ops 事件与账号处置，光靠 error 文案不足以做判定与去重。
type CursorKeyRejectedError struct {
	// Template 是命中的原始模板片段（含动态后缀），原样留存，不改写。
	Template string
	// TurnID 是模板后缀里上游给的轮次标识（如 `1792310906551-b924f5ef`）；
	// 上游未带后缀时为空串。
	TurnID string
}

// Error 实现 error。
func (e *CursorKeyRejectedError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.TurnID != "" {
		return "cursor: upstream rejected the key (turn " + e.TurnID + "): " + e.Template
	}
	return "cursor: upstream rejected the key: " + e.Template
}

// Unwrap 让 errors.Is(err, ErrCursorKeyRejectedUpstream) 成立（契约 0.2）。
func (e *CursorKeyRejectedError) Unwrap() error { return ErrCursorKeyRejectedUpstream }

// CursorParameterUnsupportedError 是 ErrCursorUpstreamParameterUnsupported 的结构化载体。
type CursorParameterUnsupportedError struct {
	Template string
	EventID  string
}

func (e *CursorParameterUnsupportedError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return "cursor: upstream model parameters unsupported: " + e.Template
}

func (e *CursorParameterUnsupportedError) Unwrap() error {
	return ErrCursorUpstreamParameterUnsupported
}

// cursorUpstreamModelUnavailableTemplate 是上游注入的**固定**"模型暂不可用"模板（不含动态后缀）。
//
// 来源证据：55-toolerr-r5-turn5.raw、sub2api-cursor-inline-toolcall-20261030 外审第 8 次
// （10-30、11-01 两次，均为长会话第 3 轮以后）。上游以普通 TextDelta 下发、无
// is_server_notice 位，随后正常 turn_ended，故只能按正文模板识别。完整形如
// `[当前模型暂不可用，请发送 继续 或新开对话 (1793521393111-0d01dd7b)]`。
const cursorUpstreamModelUnavailableTemplate = "当前模型暂不可用，请发送 继续 或新开对话"

// CursorModelUnavailableError 是 ErrCursorUpstreamModelUnavailable 的结构化载体（方案 §2.4）。
type CursorModelUnavailableError struct {
	// Template 是命中的原始模板片段（含动态后缀），原样留存，不改写。
	Template string
	// TurnID 是模板后缀里上游给的轮次标识；上游未带后缀时为空串。
	TurnID string
}

// Error 实现 error。
func (e *CursorModelUnavailableError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.TurnID != "" {
		return "cursor: upstream model temporarily unavailable (turn " + e.TurnID + "): " + e.Template
	}
	return "cursor: upstream model temporarily unavailable: " + e.Template
}

// Unwrap 让 errors.Is(err, ErrCursorUpstreamModelUnavailable) 成立。
func (e *CursorModelUnavailableError) Unwrap() error { return ErrCursorUpstreamModelUnavailable }

// cursorUpstreamQuotaExhaustedTemplate 是上游注入的**固定**"当日额度耗尽"模板（不含动态后缀）。
//
// 来源证据：2026-11-03 生产活体（Claude Code 客户端原样收到含卡 ID 的完整原文，
// 账号未停调度、请求不换号，同账号反复撞墙）。完整形如
// `今日额度已用完，每日早上8点重置（时长卡每日用量有上限，…）（1793676349583-clard21b）`：
// 上游以普通 TextDelta 下发、无 is_server_notice 位（与模型暂不可用模板同族），且
// EndStream 不带 ERROR_PRO_USER_RATE_LIMIT_EXCEEDED 等 code，故只能按正文模板识别。
const cursorUpstreamQuotaExhaustedTemplate = "今日额度已用完"

// CursorQuotaExhaustedError 是 ErrCursorQuotaExhaustedInStream 的结构化载体。
//
// 语义与 EndStream 的 ERROR_PRO_USER_RATE_LIMIT_EXCEEDED 同级：**账号当日额度耗尽**
// （时长卡每日用量上限），属账号级终态，走 RL-STOP 同款处置（停号回调 + 换号）。
type CursorQuotaExhaustedError struct {
	// Template 是命中的原始模板片段（含动态后缀），原样留存，不改写。
	Template string
	// TurnID 是模板后缀里上游给的轮次标识；上游未带后缀时为空串。
	TurnID string
}

// Error 实现 error。
func (e *CursorQuotaExhaustedError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.TurnID != "" {
		return "cursor: upstream account daily quota exhausted (turn " + e.TurnID + "): " + e.Template
	}
	return "cursor: upstream account daily quota exhausted: " + e.Template
}

// Unwrap 让 errors.Is(err, ErrCursorQuotaExhaustedInStream) 成立。
func (e *CursorQuotaExhaustedError) Unwrap() error { return ErrCursorQuotaExhaustedInStream }

// cursorMatchKeyRejected 在一段文本里匹配上游固定注入模板，命中即返回结构化错误，未命中返回 nil。
//
// 按序匹配：先拒付模板（*CursorKeyRejectedError），再模型暂不可用模板
// （*CursorModelUnavailableError），最后当日额度耗尽模板（*CursorQuotaExhaustedError）。
func cursorMatchKeyRejected(text string) error {
	if fragment, turnID, ok := cursorMatchInjectedTemplate(text, cursorUpstreamRejectTemplate); ok {
		return &CursorKeyRejectedError{Template: fragment, TurnID: turnID}
	}
	if fragment, turnID, ok := cursorMatchInjectedTemplate(text, cursorUpstreamModelUnavailableTemplate); ok {
		return &CursorModelUnavailableError{Template: fragment, TurnID: turnID}
	}
	if fragment, turnID, ok := cursorMatchInjectedTemplate(text, cursorUpstreamQuotaExhaustedTemplate); ok {
		return &CursorQuotaExhaustedError{Template: fragment, TurnID: turnID}
	}
	return nil
}

// cursorMatchParameterUnsupported 只接受整条回复为固定模板及其合法后缀。
// 普通正文中的引用或子串命中不得改变成功语义。
func cursorMatchParameterUnsupported(text string) error {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || !strings.HasPrefix(trimmed, cursorParameterNoticeTemplate) {
		return nil
	}
	eventID, ok := cursorParameterNoticeEventID(strings.TrimPrefix(trimmed, cursorParameterNoticeTemplate))
	if !ok {
		return nil
	}
	return &CursorParameterUnsupportedError{Template: trimmed, EventID: eventID}
}

// cursorMatchInjectedTemplate 定位固定模板，截取到换行为止的片段，并解析括号内的轮次标识。
//
// 轮次标识括号兼容半角 (…) 与全角（…）：2026-11-03 生产活体的额度耗尽文案
// （今日额度已用完…）后缀即全角括号，旧解析只认半角会漏取标识。
// 标识取**最后一个**括号组：额度耗尽文案行中还有说明性括号内容（如时长卡说明）。
func cursorMatchInjectedTemplate(text, template string) (fragment, turnID string, ok bool) {
	idx := strings.Index(text, template)
	if idx < 0 {
		return "", "", false
	}
	fragment = text[idx:]
	if nl := strings.IndexByte(fragment, '\n'); nl >= 0 {
		fragment = fragment[:nl]
	}
	fragment = strings.TrimSpace(fragment)
	if turnID == "" {
		turnID = cursorTurnIDInParens(fragment, '(', ')')
	}
	if turnID == "" {
		turnID = cursorTurnIDInRunes(fragment, '（', '）')
	}
	return fragment, turnID, true
}

// cursorTurnIDInParens 取片段内**最后一个**半角括号组的内容。
func cursorTurnIDInParens(fragment string, open, close byte) string {
	idx := strings.LastIndexByte(fragment, open)
	if idx < 0 {
		return ""
	}
	tail := fragment[idx+1:]
	end := strings.IndexByte(tail, close)
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(tail[:end])
}

// cursorTurnIDInRunes 同 cursorTurnIDInParens，但按 rune 匹配（全角括号是多字节字符）。
func cursorTurnIDInRunes(fragment string, open, close rune) string {
	runes := []rune(fragment)
	idx := -1
	for i := len(runes) - 1; i >= 0; i-- {
		if runes[i] == open {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ""
	}
	for j := idx + 1; j < len(runes); j++ {
		if runes[j] == close {
			return strings.TrimSpace(string(runes[idx+1 : j]))
		}
	}
	return ""
}

// cursorConnectContentType 是 Connect RPC 二进制 protobuf 的请求内容类型。
//
// ⚠️ 与 Unary 端点的 `application/proto`（**裸 protobuf、无 envelope**）不同——
// 两者不可混用（09-live-models.md §1 活体证伪）。
const cursorConnectContentType = "application/connect+proto"

// cursorUnaryContentType 是 Cursor **Unary** 端点的请求内容类型。
//
// Unary 端点（如 AuthService.GetSessionToken、模型列表）请求体走**裸 protobuf**，
// 无 5 字节 envelope，须用 `application/proto`；streaming 端点（如
// AgentService/Run）才有 Connect envelope，须用 `cursorConnectContentType`。
// 两者不可混用——生产实测 Unary 端点收到 `application/connect+proto` 的裸
// protobuf 体直接 415。
const cursorUnaryContentType = "application/proto"

// cursorAuthResponseMaxBytes 是换 token 响应的读取上限（防无界读）。
//
// 正常响应只有几百字节；给 1 MiB 上限足以容纳异常大的错误体，又不至于让
// 失控上游把内存吃满。
const cursorAuthResponseMaxBytes = 1 << 20

// readCursorUpstreamBody 有界读取上游响应体。
func readCursorUpstreamBody(r io.Reader, limit int) ([]byte, error) {
	if limit <= 0 {
		limit = cursorAuthResponseMaxBytes
	}
	return io.ReadAll(io.LimitReader(r, int64(limit)))
}

// cursorUpstreamStatusError 把非 200 响应转成带**已脱敏**信息的错误。
//
// 上游错误体有两种形状（99-blockers.md B-7）：
//  1. Connect 标准：`{"code":…,"message":…,"details":[…]}` —— 业务级错误；
//  2. 中转自有：`{"error":"Unauthorized","message":"鉴权失败: …"}` —— 鉴权层错误。
//
// 两种都要能识别，否则会把鉴权失败误判为协议损坏。只回传 code/message 摘要，
// **不回显 details[].value**（base64 嵌套 protobuf，可能含账号标识），
// 且传入的 secrets（凭证值）会被抹掉。
func cursorUpstreamStatusError(status int, body []byte, secrets ...string) error {
	code, msg := cursorExtractUpstreamErrorSummary(body, secrets...)
	if code == "" && msg == "" {
		return fmt.Errorf("%w: status %d", ErrCursorRelayAuthRejected, status)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return fmt.Errorf("%w: status %d %s: %s", ErrCursorRelayAuthRejected, status, code, msg)
	}
	return fmt.Errorf("cursor: upstream status %d %s: %s", status, code, msg)
}

// cursorRedactSecrets 把已知的凭证值从文本中抹掉（替换为 "***"）。
//
// **为什么需要**：上游错误文案可能回显请求里带过的凭证（如把 Authorization
// 头的值原样写进 message）。我们的错误会进 ops 事件与客户端回传，不抹就等于
// 把会话 token 写进日志与 usage 记录。与 codebuddy 的
// redactCodeBuddyUpstreamErrorBody 同类问题、同类处置。
//
// 过短的值（< 8 字节）不抹——"1"、"TOK" 这类值误伤面大且本身不是凭证。
func cursorRedactSecrets(text string, secrets ...string) string {
	if text == "" {
		return text
	}
	for _, s := range secrets {
		if len(s) < 8 {
			continue
		}
		text = strings.ReplaceAll(text, s, "***")
	}
	return text
}

// cursorExtractUpstreamErrorSummary 从错误体提取 (code, message)，兼容两种形状。
//
// 形状 1（Connect 标准）：`{"code":…,"message":…}`
// 形状 2（中转自有）：  `{"error":…,"message":…}`
//
// 两者同时命中 `message`，故必须**先判 error 字段是否非空**，否则形状 2 会落进
// 形状 1 分支并丢掉 "Unauthorized" 这个真正的 code。
//
// secrets 里的凭证值会被抹掉（见 cursorRedactSecrets）。
func cursorExtractUpstreamErrorSummary(body []byte, secrets ...string) (string, string) {
	if len(body) == 0 {
		return "", ""
	}
	// 先按"两个字段都认"的宽结构解一次。
	var both struct {
		Code    string `json:"code"`
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &both) == nil {
		switch {
		case both.Error != "":
			// 形状 2 优先：`error` 是字符串型 code。
			return cursorRedactSecrets(both.Error, secrets...),
				cursorRedactSecrets(strings.TrimSpace(both.Message), secrets...)
		case both.Code != "" || both.Message != "":
			return cursorRedactSecrets(both.Code, secrets...),
				cursorRedactSecrets(strings.TrimSpace(both.Message), secrets...)
		}
	}
	// 非 JSON（或结构不符）：只回长度，不回内容（可能是二进制或含敏感信息）。
	return "", fmt.Sprintf("<non-JSON body, %d bytes>", len(body))
}

// cursorStreamEvent 是解析出的一帧的语义化结果。
type cursorStreamEvent struct {
	// Kind 见下方 cursorEventKind* 常量。
	Kind string
	// Text 仅在 Kind == cursorEventText 时有意义（**可能为空**：模型可发空 delta）。
	Text string
	// Reasoning 是上游内联 `<think>…</think>` 推理段剥离出的思考内容（可能为空）。
	//
	// 与 Text 是**双通道**：完整标签被彻底吞噬（既不进 Reasoning 也不进 Text），
	// 思考内容经此字段作为 OpenAI `delta.reasoning_content` 交付，正文进 Text，
	// 二者互不污染。未闭合的推理段在流式过程中随帧增量吐出，消除假死卡死。
	Reasoning string
}

// 事件种类。
const (
	// cursorEventText 是增量文本帧（唯一会产出 OpenAI delta.content 的帧）。
	cursorEventText = "text"
	// cursorEventSkip 是首版范围外的帧（工具/执行/KV/思考/摘要/计时）：
	// **丢弃但不算错误**。
	cursorEventSkip = "skip"
)

// cursorTurnUsage 是 turn_ended 帧携带的真实用量（usage 的唯一来源）。
//
// §25.17.5 A06①：proto optional 的**存在性**必须逐字段保留——"字段缺失"与
// "真实 0"是两种不同事实，不得折叠成同一组 0（27/28 活体原件四项全部显式出现，
// 含 cache_write=0，故缺失绝不等于 0）。故每个计数字段配一个 Seen 标志：
//
//	Seen=true  → 该字段在 wire 上出现且通过校验（wire 类型正确、数值非负、
//	              可安全转入现有整数类型）；
//	Seen=false → 该字段未出现（其数值无意义，不得当 0 参与结算判定）。
//
// 判定见 billingFieldsComplete（结算必需的四个计费字段）。
type cursorTurnUsage struct {
	// Seen 表示是否收到过 turn_ended 帧（**帧级**存在性，与逐字段存在性分开：
	// 帧出现过不等于四个计费字段都出现过）。
	Seen bool
	// Input / Output / CacheRead / CacheWrite 直接对应 TurnEndedUpdate 的 f1..f4。
	// 取值只在对应 Seen=true 时有意义。
	Input      int
	Output     int
	CacheRead  int
	CacheWrite int
	// Reasoning 是 f5（thinking tokens）。**不并入 Output**：证据 33 同时给出
	// output=15 与 reasoning=11，二者是否重叠未取证，并入即可能重复计费。
	// 单独保留仅供诊断，不参与计费口径。
	Reasoning int
	// EndedAtMS 是 f6（上游轮次结束时间戳）。
	EndedAtMS uint64

	// 逐字段存在性（proto optional presence，见上）。
	F1InputSeen      bool
	F2OutputSeen     bool
	F3CacheReadSeen  bool
	F4CacheWriteSeen bool
	F5ReasoningSeen  bool
	F6EndedAtSeen    bool
}

// billingFieldsComplete 判定本份真实用量是否具备结算必需的四个计费字段
// （f1 input / f2 output / f3 cache_read / f4 cache_write）。
//
// 四个字段**逐一出现**（存在性）且取值合法即成立：四项全 0 也是真实完整用量。
// 缺任一字段 → 不完整。**绝不用"非零总和"或结束时间戳反推存在性**——
// proto optional 不等于本站可以补 0，缺失一律按"无完整 usage"处理。
func (u cursorTurnUsage) billingFieldsComplete() bool {
	if !u.F1InputSeen || !u.F2OutputSeen || !u.F3CacheReadSeen || !u.F4CacheWriteSeen {
		return false
	}
	// 合法性：计费字段取值必须非负（解析层已拒绝负数与越界值，故解析产出的
	// 用量恒合法；此处再对直接构造的用法做同口径判定）。
	return u.Input >= 0 && u.Output >= 0 && u.CacheRead >= 0 && u.CacheWrite >= 0
}

// cursorDecodeTurnUsage 严格解码一层 TurnEndedUpdate 字段（f1–f6），逐字段保留
// proto optional **存在性**（§25.17.5 A06①）。
//
// Seen 为 true 的三重条件：字段在 wire 上出现 + wire 类型正确（varint）+ 取值可
// 安全转入该字段现有的整数类型（token 计数为非负 int，结束时间为 uint64）。
// 任一已出现字段不合法（错误 wire / 负数 / 越界）都是**解析失败**，绝不为该字段
// 补 0 继续——给损坏值补 0 会让结算把"没拿到用量"当成"真实 0"。
func cursorDecodeTurnUsage(fields []cursorPBField) (cursorTurnUsage, error) {
	var (
		u   = cursorTurnUsage{Seen: true}
		err error
	)
	if u.Input, u.F1InputSeen, err = cursorTurnTokenField(fields, cursorFldTurnEndedInputTokens); err != nil {
		return cursorTurnUsage{}, err
	}
	if u.Output, u.F2OutputSeen, err = cursorTurnTokenField(fields, cursorFldTurnEndedOutputTokens); err != nil {
		return cursorTurnUsage{}, err
	}
	if u.CacheRead, u.F3CacheReadSeen, err = cursorTurnTokenField(fields, cursorFldTurnEndedCacheReadTokens); err != nil {
		return cursorTurnUsage{}, err
	}
	if u.CacheWrite, u.F4CacheWriteSeen, err = cursorTurnTokenField(fields, cursorFldTurnEndedCacheWriteTokens); err != nil {
		return cursorTurnUsage{}, err
	}
	// f5 reasoning 只作诊断（不并入 output、不作结算必填），存在性同样如实保留。
	if u.Reasoning, u.F5ReasoningSeen, err = cursorTurnTokenField(fields, cursorFldTurnEndedReasoningTokens); err != nil {
		return cursorTurnUsage{}, err
	}
	// f6 结束时间不是 token 完整性的必填项，但出现时仍须是合法 varint。
	if u.EndedAtMS, u.F6EndedAtSeen, err = cursorTurnEndedAtField(fields, cursorFldTurnEndedEndedAtMS); err != nil {
		return cursorTurnUsage{}, err
	}
	return u, nil
}

// cursorTurnTokenField 读取一个 token 计数字段（f1–f5），返回取值与是否出现。
//
// 缺失 → (0, false, nil)：缺失不置 Seen，取值无意义（proto optional ≠ 本站可补 0）。
// 出现则必须 wire=varint 且能安全转为非负 int：proto int64 的负数按 10 字节
// varint（补码）编码，落到 uint64 上即 ≥ 1<<63，超出 int 范围，一律按解析失败。
func cursorTurnTokenField(fields []cursorPBField, number int) (int, bool, error) {
	f, ok := cursorFindField(fields, number)
	if !ok {
		return 0, false, nil
	}
	if f.Wire != cursorWireVarint {
		return 0, false, cursorFrameErrorf(cursorFrameMessage,
			"turn_ended field %d has wire type %d, want varint", number, f.Wire)
	}
	if f.Varint > uint64(math.MaxInt) {
		return 0, false, cursorFrameErrorf(cursorFrameMessage,
			"turn_ended field %d value %d is not a non-negative int", number, f.Varint)
	}
	return int(f.Varint), true, nil
}

// cursorTurnEndedAtField 读取 f6 结束时间戳（uint64），返回取值与是否出现。
//
// 缺失 → (0, false, nil)。出现则必须 wire=varint；uint64 全域都可安全承载，
// 故除 wire 外不做取值裁剪（时间戳不参与 token 完整性与计费公式）。
func cursorTurnEndedAtField(fields []cursorPBField, number int) (uint64, bool, error) {
	f, ok := cursorFindField(fields, number)
	if !ok {
		return 0, false, nil
	}
	if f.Wire != cursorWireVarint {
		return 0, false, cursorFrameErrorf(cursorFrameMessage,
			"turn_ended field %d has wire type %d, want varint", number, f.Wire)
	}
	return f.Varint, true, nil
}

// cursorOpenAIUsage 把 turn_ended 用量映射成计费口径的 OpenAIUsage。
func (u cursorTurnUsage) cursorOpenAIUsage() OpenAIUsage {
	return OpenAIUsage{
		InputTokens:              u.Input,
		OutputTokens:             u.Output,
		CacheReadInputTokens:     u.CacheRead,
		CacheCreationInputTokens: u.CacheWrite,
	}
}

// cursorStreamParser 逐帧解析 Cursor envelope 流并产出 OpenAI SSE chunk。
type cursorStreamParser struct {
	// model 是回写到 OpenAI chunk 的模型名（用**入站原始模型名**，与其它平台一致：
	// 客户端看到的应是它请求的模型，而非上游 wire ID）。
	model string
	// secrets 是需要在错误文案里抹掉的凭证值（见 cursorRedactSecrets）。
	secrets []string
	// started 表示是否已发出 role:assistant 首包。
	started bool
	// endStreamSeen 表示是否已收到 EndStream 帧（收到后禁止再收消息帧）。
	endStreamSeen bool
	// content 是累加后的当前完整**可见**文本（TextDeltaUpdate 是**增量**，见下）。
	// 上游把推理内联进正文（<think>…</think>），故 content 只累加剥离后的正文：
	// 它同时是 httpText() 对外交付与内联 tool call 形状判定的依据。
	content string
	// rawContent 是累加后的原始文本（含 <think> 段），只用于上游注入模板匹配
	// （拒付 / 模型暂不可用）：那类模板可能出现在 think 段之后，用可见文本判定会漏。
	rawContent string
	// trimmedLeading 表示已交付过第一段可见正文：剥离 <think> 后残留的换行只在
	// 首段被去掉（正文开头的空行是剥离副产物，不该让客户端看到两行空白）。
	trimmedLeading bool
	// thinkStripper 跨帧有状态地剥离内联思考标签（<think>/<thinking>），
	// 状态由 apicompat.InlineThinkStripper 内部维护（D2 迁移，去掉本地副本）。
	thinkStripper apicompat.InlineThinkStripper
	// usage 是 turn_ended 给出的真实用量（未收到则 Seen=false，各项为 0）。
	usage cursorTurnUsage
	// rejected 命中流内注入模板（拒付 / 模型暂不可用）后的结构化错误（非 nil 即终态失败）。
	rejected error
	// delegations 是本轮工具委托的**唯一**记录（§24.2-6）。
	//
	// duplex 解码 mcp_args 后写入这里，parser 只读取并输出，不再对同一帧二次解码；
	// 批次的 started/listed/封口判定也都在该记录内（§25.3）。
	delegations *cursorToolDelegations

	// declaredToolDefs 是本请求 f4（McpTools）的每条 McpToolDefinition 原始字节，
	// 由 attachToolSchemas 注入（来源必须是请求体 f4 原始字节，与 duplex 路径同源）。
	// 供独立解析路径（非 duplex）从真实 f4 的 f5 tool_name 取出已声明工具名；
	// 为空表示本路径拿不到 f4 原始字节 → 装配缺陷，失败关闭。
	declaredToolDefs [][]byte

	// inlineNorm 是 Cursor 文本帧边界唯一的 CallDynamicTool 私有块归一器。
	// 流式（cursorSSEStream.next）与缓冲/独立解析（cursorParseStream）共用它，
	// 绝不维护两套规则。declared 来源必须是本次 HTTP 实际声明的工具。
	inlineNorm *cursorCDTNormalizer
	// inlineCalls 是本轮从正文归一出的结构化调用；它们**不进入** cursorToolDelegations，
	// 也没有 ExecID/IDField，按完整历史续轮消费，不伪造原生结果回写。
	inlineCalls []cursorInlineCDTCall
	// toolChoiceMode / toolChoiceName 是本次 HTTP 的工具选择，与原生委托共用同一
	// 输出校验（cursorEnforceToolChoiceOnOutput）。来源只取本 HTTP 的 session 选项。
	toolChoiceMode string
	toolChoiceName string

	// lg 是解析器阶段事件的结构化 logger（不可变、并发安全）。可为 nil，
	// 底层落盘函数会在 nil 时回退到全局 logger。
	lg *zap.Logger
	// firstTokenMs 是首帧文本的耗时（毫秒，自解析开始计时），供转发结果诊断。
	firstTokenMs int
	parseStart   time.Time

	// ---- §25.17.2「实际 ID」与「每 HTTP 只输出自本次附着以来的内容」----

	// idSource 返回**当前 HTTP**的输出响应 ID（由会话扇出器在附着时提供）。
	//
	// 每个 HTTP 一份唯一响应 ID：本 HTTP 的全部 chunk、response.created/completed 与
	// 非流式 response.id 共用它；未附着（无 HTTP 消费者）时用本解析器自己的 fallback。
	// 绝不使用固定常量或上游 request_id。
	idSource func() string
	// fallbackID 是未提供 idSource 时（独立解析场景）本解析器自用的唯一 ID。
	fallbackID string
	// httpTextBase / httpRawTextBase / httpCallBase 是本 HTTP 附着的起点
	// （可见正文偏移 / 原始正文偏移 / 已捕获调用条数）。
	//
	// 只用于**非流式**当前 HTTP 的聚合：续轮的第二个 HTTP 不能把上一 HTTP 的正文/调用
	// 再输出一遍（§25.17.2「聚合器在交接后释放，不重播」）。
	httpTextBase    int
	httpRawTextBase int
	httpCallBase    int

	// ---- §25.3 listed 归属判定用的本地计数（仅用于归属，不参与封口/输出）----
	//
	// lastModelCallID 是最近一次 tool_call_started 的 model_call_id（真实帧才有；
	// 空串表示尚无 started 上下文，或合成帧未携带 f3）。
	lastModelCallID string
	// pendingListed 是"已收 listed 但暂无法归属"的挂起计数：无 started 上下文时挂起，
	// 由下一个 started 消费并归属到新批（派发单 B1 §4.3）。
	pendingListed *int
	// batchCounts 按 model_call_id 记录每批的 started/internal 计数与 listedSeen，
	// 仅用于 listed 归属判定（4.3 的"该批 !listedSeen 或 started+internal < count"）。
	batchCounts map[string]*cursorListedCounts
}

// cursorListedCounts 是单个 model_call_id 批次的本地归属计数（parser 私有，
// 仅用于 listed 归属判定，**不进入** cursorToolDelegations 的封口/输出逻辑）。
type cursorListedCounts struct {
	started    int  // 对外 started 数
	internal   int  // 内部元工具 started 数
	listedSeen bool // 本批是否已归属过 listed
}

// attachOutputIDSource 绑定"当前 HTTP 输出响应 ID"的读取源（§25.17.2 实际 ID）。
func (p *cursorStreamParser) attachOutputIDSource(src func() string) {
	if p == nil {
		return
	}
	p.idSource = src
}

// beginHTTPTurn 标记一个新 HTTP 附着的起点（内容偏移 + 已捕获调用条数）。
func (p *cursorStreamParser) beginHTTPTurn() {
	if p == nil {
		return
	}
	p.httpTextBase = len(p.content)
	p.httpRawTextBase = len(p.rawContent)
	p.httpCallBase = len(p.capturedToolCalls())
	p.started = false
}

// chunkID 返回本 HTTP 实际输出响应 ID（所有 chunk / 非流式 id 共用同一值）。
func (p *cursorStreamParser) chunkID() string {
	if p == nil {
		return ""
	}
	if p.idSource != nil {
		if id := strings.TrimSpace(p.idSource()); id != "" {
			return id
		}
	}
	if strings.TrimSpace(p.fallbackID) == "" {
		p.fallbackID = cursorNewUUID()
	}
	return p.fallbackID
}

// httpContent 返回**本 HTTP** 产生的文本（非流式聚合用；不重播上一 HTTP 正文）。
func (p *cursorStreamParser) httpContent() string {
	if p == nil {
		return ""
	}
	if p.httpTextBase <= 0 {
		return p.content
	}
	if p.httpTextBase >= len(p.content) {
		return ""
	}
	return p.content[p.httpTextBase:]
}

// httpRawContent 返回本 HTTP 产生的原始文本，用于严格的整条回复 matcher。
// 原始文本必须按 HTTP 边界切片，否则跨 HTTP 续轮会把上一轮正文拼进当前回复，
// 使完整参数拒绝模板被误判为普通正文。
func (p *cursorStreamParser) httpRawContent() string {
	if p == nil {
		return ""
	}
	if p.httpRawTextBase <= 0 {
		return p.rawContent
	}
	if p.httpRawTextBase >= len(p.rawContent) {
		return ""
	}
	return p.rawContent[p.httpRawTextBase:]
}

// httpToolCalls 返回**本 HTTP**捕获的对外工具调用（不重播已交付批次）。
func (p *cursorStreamParser) httpToolCalls() []cursorToolCallCapture {
	calls := p.capturedToolCalls()
	if p == nil || p.httpCallBase <= 0 || p.httpCallBase >= len(calls) {
		return calls
	}
	return calls[p.httpCallBase:]
}

// attachToolDelegations 绑定本轮共享的工具委托记录。
//
// 绑定后本解析器**不再解码** mcp_args（解码权在 duplex 侧），只在收尾时读取结果。
func (p *cursorStreamParser) attachToolDelegations(d *cursorToolDelegations) {
	if p == nil {
		return
	}
	p.delegations = d
}

// toolDelegations 返回本解析器使用的委托记录（未绑定时用本地记录，保证单一解码点）。
func (p *cursorStreamParser) toolDelegations() *cursorToolDelegations {
	if p.delegations == nil {
		p.delegations = newLocalCursorToolDelegations()
	}
	return p.delegations
}

// capturedToolCalls 返回本轮对外工具调用（输出用）。
func (p *cursorStreamParser) capturedToolCalls() []cursorToolCallCapture {
	return p.toolDelegations().snapshot()
}

// assignToolCallWireIDs 在**任何 tool_calls 字节写出之前**为待交付委托决定出站 id。
//
// 交付侧 id 重写（派发单 CALLID）：上游会在同一会话内复用同一个 tool_call_id，
// 客户端按 id 去重会丢弃第二条调用 → 上游等不到结果 → stream_error。本方法把"已交付过
// 的原 id"换成全新的唯一 id（幂等，已决定的委托不二次改写），并登记 新id→原id 映射。
func (p *cursorStreamParser) assignToolCallWireIDs() error {
	if p == nil {
		return nil
	}
	return p.toolDelegations().assignWireIDs()
}

// sealableDelegations 报告"本批是否已可封口"（只读判定，不改批次状态）。
//
// 读循环用它决定是否到工具交接检查点：listed 数未等齐或某条对外 started 还没有完整
// f11 委托时返回 ErrCursorBatchIncomplete，调用方据此继续等后续帧（§25.3：不按第一个
// mcp_args 提前封口，也不按静默时间猜测结束）。
func (p *cursorStreamParser) sealableDelegations() error {
	return p.toolDelegations().sealable()
}

// resetTurnStart 复位"本 HTTP 已发过 role 包"的标记。
//
// 跨 HTTP（§25.3）：续轮附着是**新的 HTTP 响应**，必须各自从 role 包开始，不复用上一个
// HTTP 的协议头状态；同时已交付的 tool_calls 条数由 cursorSSEStream.emittedCalls 保持，
// 不会被重播。
func (p *cursorStreamParser) resetTurnStart() { p.started = false }

// resetBatchGeneration 在开始新一代 model call 前清空解析器的"批次代际归属计数"。
//
// 触发点：cursorSSEStream.next() 的工具交接检查点（s.delegated=true 且 s.done=true）。
// 本次 HTTP 已向下游交接了工具委托并暂停，后续帧属于新一代 model call（真实续轮是新的
// AgentRun 代），旧批的归属计数（尤其是 listedSeen）**不得跨代沿用**——
// 否则同 model_call_id 的续轮会在 noteToolRequestsListed 里看到 listedSeen==true 且
// started+internal>=count，把新 listed 挂起为 pending → 新批永远拿不到 listed →
// checkSealableLocked 永不通过 → 请求无法完成（外审 F1）。
//
// 纪律：只清解析器本地归属计数，不碰 cursorToolDelegations（封口记录由该结构按代维护，
// 续轮会开新批，见 TestCursorBatchSealedBatchNotReused）；不新增计时/兜底机制；同一 HTTP
// 段内出现第二个 model_call_id 的批（未交接）时绝不复位（仅在交接检查点复位，D2）。
func (p *cursorStreamParser) resetBatchGeneration() {
	if p == nil {
		return
	}
	p.batchCounts = nil
	p.pendingListed = nil
	p.lastModelCallID = ""
}

// cursorToolCallCapture 是一条经 mcp_args 委托捕获的工具调用。
type cursorToolCallCapture struct {
	// ID 是上游给的 tool_call_id（OpenAI tool_calls[].id）。
	ID string
	// Name 是工具名（OpenAI tool_calls[].function.name）。
	Name string
	// ArgsJSON 是工具参数的对象 JSON 字符串（OpenAI tool_calls[].function.arguments）。
	ArgsJSON string
	// ExecID 是承载本委托的 ExecServerMessage.f1（回写真实结果时必须原样带回，
	// 否则上游无法把 McpResult 关联到对应 exec）。
	ExecID uint64
	// IDField 是承载本委托的 ExecServerMessage.f15（原值，回写时一并带回）。
	IDField []byte
	// WireID 是**发给客户端的** tool_call id（OpenAI tool_calls[].id）。
	//
	// 上游会在同一会话内对不同工具复用同一个 tool_call_id（生产实测 read 与 write 共用
	// call-3466d322-...）。OpenAI 协议里 tool_call id 是一次性唯一锚点，客户端按 ID 去重/
	// 覆盖，复用会让第二条调用被客户端丢弃（不执行、不写回 → 上游等不到结果 → stream_error）。
	// 因此交付侧在"该原 ID 本会话内已交付过（含同批重复）"时必须换一个全新唯一 id，
	// 并把映射（新 id → 原 id）记进 cursorToolDelegations.idRewrite，供续轮反查。
	//
	// 语义纪律：对外输出（SSE / 非流式）一律用 wireID()（为空回退 ID）；上游写回始终用
	// ID + ExecID（原值），绝不把改写后的 id 回写给上游。
	WireID string
}

// wireID 返回发给客户端的 tool_call id：有交付侧改写值时用改写值，否则沿用上游原值。
//
// 唯一的对外 id 出口：所有 SSE / 非流式渲染都必须走它，绝不直接读 c.ID
// （直接读会把上游复用的 id 原样发给客户端，触发客户端按 ID 去重丢弃调用）。
func (c cursorToolCallCapture) wireID() string {
	if c.WireID != "" {
		return c.WireID
	}
	return c.ID
}

// cursorContent 返回拼出的完整响应文本（非流式入站用）。
func (p *cursorStreamParser) cursorContent() string { return p.content }

// parseThinkDeltas 把增量正文切成**两个通道**（委托 apicompat.InlineThinkStripper，
// D2 迁移：去掉本地副本 earliestThinkOpen / maxTrailingPartialThinkOpen / trailingPartialTag）：
//   - reasoning：处于 <think>…</think>（含 `<thinking>`…`</thinking>` 变体）段内、
//     被完整开闭标签夹住的内容；
//   - text：段外可见正文（已去除标签；首段前导空白由 finalizeVisible 在本方法内清理）。
//
// 双通道纪律（用户裁定：思考必须作为 reasoning 交付，且标签绝对不能外泄）由剥离器保证：
//  1. 完整开/闭标签一旦匹配即**彻底吞噬**——既不进 reasoning 也不进 text；
//  2. 标签前缀（"<"、`<t`、"</"、`</th` 等）被剥离器严格扣留，跨帧切碎的标签 100% 吞噬；
//  3. 段外首段正文的前导空白/换行由 finalizeVisible 清理（剥标签留下的 "\n\n" 副产物）。
//
// rawContent 始终累加原始增量（含 <think> / <thinking> 段），供流内拒付 / 模型暂不可用
// 模板匹配使用——剥离动作不会改变这些终态判定的灵敏度。
func (p *cursorStreamParser) parseThinkDeltas(delta string) (reasoning string, text string) {
	if delta == "" {
		return "", ""
	}
	p.rawContent += delta
	r, t := p.thinkStripper.Feed(delta)
	return r, p.finalizeVisible(t)
}

// maybeFinalizeReasoning 是 reasoning 通道的归一钩子（当前与 text 通道不同：
// 思考内容保留其自身空白，不去除首段前导空白，避免截断模型首字符前的合法缩进）。
func (p *cursorStreamParser) maybeFinalizeReasoning(raw string) string {
	return raw
}

// finalizeVisible 归一本次可交付文本：剥离 <think> 后残留的前导空白只在**首段**
// 去掉——否则客户端会先看到两行空行（活体：剥离 "<think>…</think>\n\n3293" 后
// 正文变成 "\n\n3293"）。纯空白的首段直接丢弃。
func (p *cursorStreamParser) finalizeVisible(raw string) string {
	if raw == "" || p.trimmedLeading {
		return raw
	}
	trimmed := strings.TrimLeft(raw, "\r\n\t ")
	if trimmed == "" {
		return ""
	}
	p.trimmedLeading = true
	return trimmed
}

// flushThinkTail 在流收尾时吐出**双通道**缓冲尾段（委托 apicompat.InlineThinkStripper，
// D2 迁移：去掉本地副本；只吐一次由剥离器内部 flushed 守卫保证）：
//   - reasoning：处于推理段内、尚未随帧吐出的思考尾巴；
//   - text：段外残留的可见正文尾巴（如结尾恰好是 "<thi" 的半截开标签前缀，如实交付，
//     不丢字节；完整 <think>/</think> 段早已被吞噬，不会泄露）。
//
// 两个返回值的语义分别对应 emitStreamReasoning 与 emitStreamText，由调用方决定各自如何写出。
func (p *cursorStreamParser) flushThinkTail() (string, string) {
	r, t := p.thinkStripper.Flush()
	return r, t
}

// cursorNewStreamParser 构造解析器（model 为空时 chunk 内 model 字段留空）。
//
// secrets 是需要从错误文案里抹掉的凭证值（如本次对话用的 session_token）。
func cursorNewStreamParser(model string, secrets ...string) *cursorStreamParser {
	return &cursorStreamParser{
		model:      model,
		secrets:    secrets,
		lg:         logger.L(),
		parseStart: time.Now(),
	}
}

// attachToolSchemas 注入本请求 f4 原始字节（每条 McpToolDefinition 原始字节），
// 供独立解析路径（非 duplex）按 tool_name 取回 input_schema 做 schema 还原。
// 来源必须是请求体 f4 原始字节（与 duplex 路径同源），不得改用 req.Tools 重新序列化。
func (p *cursorStreamParser) attachToolSchemas(defs [][]byte) {
	if p == nil {
		return
	}
	p.declaredToolDefs = defs
}

// attachInlineDeclaredTools 注入本次 HTTP 实际声明的工具（含参数 schema）。
// 来源必须是当前请求的 Chat.Tools，绝不用历史工具、账号默认值或模型名猜测。
func (p *cursorStreamParser) attachInlineDeclaredTools(tools []apicompat.ChatTool) {
	if p == nil {
		return
	}
	p.inlineNorm = newCursorCDTNormalizer(tools)
}

// consumeInlineText 在文本帧边界归一 CallDynamicTool 私有块。
// 流式与缓冲路径共用：返回可安全交付的正文；识别到损坏块/未声明工具/schema
// 不符时返回错误，调用方必须失败关闭。
func (p *cursorStreamParser) consumeInlineText(delta string) (string, error) {
	if p == nil {
		return delta, nil
	}
	if p.inlineNorm == nil {
		p.inlineNorm = newCursorCDTNormalizer(nil)
	}
	safe, calls, err := p.inlineNorm.consume(delta)
	if err != nil {
		return "", err
	}
	p.inlineCalls = append(p.inlineCalls, calls...)
	return safe, nil
}

// finishInlineText 在流收尾时交付归一器扣留的正文尾段。
func (p *cursorStreamParser) finishInlineText() (string, error) {
	if p == nil {
		return "", nil
	}
	if p.inlineNorm == nil {
		p.inlineNorm = newCursorCDTNormalizer(nil)
	}
	return p.inlineNorm.finish()
}

// attachInlineToolChoice 注入本次 HTTP 的工具选择，供内联调用交付前与原生调用
// 一起校验。缺省（空 mode）即 auto，对既有调用方零行为变化。
func (p *cursorStreamParser) attachInlineToolChoice(mode, name string) {
	if p == nil {
		return
	}
	p.toolChoiceMode = mode
	p.toolChoiceName = name
}

// enforceInlineToolChoice 在**写出内联 tool_calls 之前**按本 HTTP 的 tool_choice
// 校验原生 + 内联调用的整体集合。纯内联响应没有原生委托批，不会经过
// cursorDeliverBatchCheckpoint，因此必须在这里补上同一条输出闸门。
func (p *cursorStreamParser) enforceInlineToolChoice(nativeCalls []cursorToolCallCapture) error {
	if p == nil || len(p.inlineCalls) == 0 {
		return nil
	}
	all := append([]cursorToolCallCapture(nil), nativeCalls...)
	for _, call := range p.inlineCalls {
		all = append(all, cursorToolCallCapture{ID: call.ID, WireID: call.ID, Name: call.Name})
	}
	return cursorEnforceToolChoiceOnOutput(p.toolChoiceMode, p.toolChoiceName, all)
}

// cursorInlineToolCallChunks 把归一出的调用渲染成普通 Chat tool_calls delta。
// startIndex 必须是**本 HTTP 已输出的原生调用条数**：聚合器与 Anthropic 流式
// 转换器都按 index 归并，两类调用共用序号会把 ID/名称覆盖、参数拼在一起。
func (p *cursorStreamParser) cursorInlineToolCallChunks(startIndex int) []string {
	if p == nil || len(p.inlineCalls) == 0 {
		return nil
	}
	out := make([]string, 0, 2*len(p.inlineCalls))
	for i, call := range p.inlineCalls {
		tc := cursorToolCallCapture{ID: call.ID, WireID: call.ID, Name: call.Name, ArgsJSON: call.ArgsJSON}
		out = append(out, p.cursorSSEToolCallNameChunk(startIndex+i, tc), p.cursorSSEToolCallArgsChunk(startIndex+i, tc))
	}
	return out
}

// logPhase 是解析器阶段事件的统一落盘入口（logger 不可变、并发安全，直接落盘）。
func (p *cursorStreamParser) logPhase(lvl zapcore.Level, phase, outcome string, extra ...zap.Field) {
	cursorPhaseLogFields(p.lg, lvl, phase, p.model, "", extra...)
}

// markFirstToken 记录首帧文本耗时（只记第一次，供 ForwardResult 诊断）。
func (p *cursorStreamParser) markFirstToken() {
	if p == nil || p.firstTokenMs != 0 || p.parseStart.IsZero() {
		return
	}
	p.firstTokenMs = int(time.Since(p.parseStart).Milliseconds())
	if p.firstTokenMs <= 0 {
		p.firstTokenMs = 1
	}
}

// cursorParseStream 解析完整响应体，产出按序的 OpenAI SSE 行（**不含** `[DONE]`，
// 由调用方在成功收尾后追加）。
//
// 失败关闭：
//   - 非法帧结构 → ErrCursorProtocol
//   - 流内拒付模板 → ErrCursorKeyRejectedUpstream（结构化，见 CursorKeyRejectedError）
//   - EndStream 带 error → ErrCursorUpstreamStream（携带**已脱敏**的 code/message）
//   - 流结束但既无 turn_ended 也无 EndStream → ErrCursorMissingEndStream
//     （取消 / 截断的流不按成功收尾，也不猜 usage）
func (p *cursorStreamParser) cursorParseStream(body []byte) ([]string, error) {
	var sseLines []string
	off := 0
	for off < len(body) {
		frame, next, err := cursorReadEnvelopeFrame(body, off)
		if err != nil {
			return nil, err
		}
		off = next

		if frame.cursorIsEndStream() {
			if p.endStreamSeen {
				return nil, cursorFrameErrorf(cursorFrameMessage, "received extra EndStreamResponse")
			}
			p.endStreamSeen = true
			code, msg, err := cursorParseEndStream(frame.Data, p.secrets...)
			if err != nil {
				return nil, err
			}
			if code != "" {
				return nil, &cursorUpstreamStreamError{Code: code, Message: msg}
			}
			// **不 break**：EndStream 之后若还有任何帧都是协议违例（客户端文案
			// `received extra message after EndStreamResponse`），必须继续解析才能
			// 发现。提前 break 会让这类畸形流被当成正常流收尾。
			continue
		}
		if p.endStreamSeen {
			return nil, cursorFrameErrorf(cursorFrameMessage, "received extra message after EndStreamResponse")
		}

		ev, err := p.cursorParseMessageFrame(frame.Data)
		if err != nil {
			return nil, err
		}
		if ev.Kind != cursorEventText {
			// 首版范围外：丢弃（用户裁定），不算协议错误。
			continue
		}
		safeText, inlineErr := p.consumeInlineText(ev.Text)
		if inlineErr != nil {
			return nil, inlineErr
		}
		if !p.started {
			p.started = true
			sseLines = append(sseLines, p.cursorSSERoleChunk())
		}
		p.content += safeText
		p.markFirstToken()
		// 双通道发射：思考先走 reasoning_content，正文走 content，二者互不污染。
		if ev.Reasoning != "" {
			sseLines = append(sseLines, p.cursorSSEReasoningChunk(ev.Reasoning))
		}
		if safeText != "" {
			sseLines = append(sseLines, p.cursorSSETextChunk(safeText))
		}
		// 拒付模板可能跨帧：每收到一段就在**累加结果**上匹配一次。
		// 判定用原始文本（含 <think> 段），与流式侧同口径。
		if err := cursorMatchKeyRejected(p.rawContent); err != nil {
			p.rejected = err
		}
	}

	// 流内拒付是终态失败：**优先于**任何成功收尾判定。
	if p.rejected != nil {
		return nil, p.rejected
	}
	if err := cursorMatchParameterUnsupported(p.rawContent); err != nil {
		return nil, err
	}
	if !p.usage.Seen && !p.endStreamSeen {
		// 既没有 turn_ended 也没有 EndStream：被取消或截断的流。绝不按成功收尾
		// （那会回写一条 0 usage 的"成功"响应并误导计费）。
		return nil, ErrCursorMissingEndStream
	}
	if !p.started {
		// 全 skip 帧：仍要发出 role 包，让客户端拿到合法空流。
		sseLines = append(sseLines, p.cursorSSERoleChunk())
		p.started = true
	}
	// 归一器尾段先收口：已识别开标记但无闭标记 → 失败关闭，不把控制块泄漏为正文。
	inlineTail, inlineErr := p.finishInlineText()
	if inlineErr != nil {
		return nil, inlineErr
	}
	// 收尾尾段：<think> 剥离的双通道缓冲在这里吐完（reasoning 与 text 各自收口）。
	rTail, tTail := p.flushThinkTail()
	if inlineTail != "" {
		tTail += inlineTail
		// 只并入 tTail，由下方唯一的 p.content += tTail 落账；
		// 这里再追加一次会把尾段在 parser.content 里记两遍。
	}
	if rTail != "" || tTail != "" {
		if !p.started {
			sseLines = append(sseLines, p.cursorSSERoleChunk())
			p.started = true
		}
		p.markFirstToken()
		if rTail != "" {
			sseLines = append(sseLines, p.cursorSSEReasoningChunk(rTail))
		}
		if tTail != "" {
			p.content += tTail
			sseLines = append(sseLines, p.cursorSSETextChunk(tTail))
		}
	}
	// 工具委托捕获：在 finish 前吐 tool_calls delta（流式与 cursorParseStreamToSSE 同口径）。
	calls := p.capturedToolCalls()
	if len(calls) > 0 {
		// 交付侧 id 重写：本地解析路径（无 duplex 的共享记录）同样必须先决定出站 id。
		if err := p.assignToolCallWireIDs(); err != nil {
			return nil, err
		}
		calls = p.capturedToolCalls()
		for i, tc := range calls {
			sseLines = append(sseLines, p.cursorSSEToolCallNameChunk(i, tc))
			sseLines = append(sseLines, p.cursorSSEToolCallArgsChunk(i, tc))
		}
	}
	if err := p.enforceInlineToolChoice(calls); err != nil {
		return nil, err
	}
	// 正文内联归一出的调用：同形输出结构化 tool_calls，但不进委托记录。
	// 序号从原生调用条数之后起，避免两类调用在 index 上互相覆盖。
	sseLines = append(sseLines, p.cursorInlineToolCallChunks(len(calls))...)
	if len(p.inlineCalls) > 0 {
		sseLines = append(sseLines, p.cursorSSEFinishChunkWithReason("tool_calls"))
	} else {
		sseLines = append(sseLines, p.cursorSSEFinishChunk())
	}
	return sseLines, nil
}

// cursorParseMessageFrame 解析一帧 agent.v1.AgentServerMessage。
func (p *cursorStreamParser) cursorParseMessageFrame(payload []byte) (cursorStreamEvent, error) {
	fields, err := cursorDecodeProtobuf(payload)
	if err != nil {
		return cursorStreamEvent{}, err
	}
	// 工具委托（exec.mcp_args，f11）：§24.2-6 只解码/保存一次。
	//
	// 解码权归共享记录的所有者：duplex 存在时由 duplex 解码并写入（parser 只读），
	// 无 duplex 的独立解析场景由本解析器解码并写入同一记录。两条路径共用同一份
	// cursorToolDelegations，绝不各自保留一份列表。
	if exec, ok := cursorFindField(fields, cursorFldServerExecMessage); ok {
		execParts, e := cursorDecodeProtobuf(exec.Bytes)
		if e != nil {
			return cursorStreamEvent{}, e
		}
		if argsField, ok := cursorFindField(execParts, 11); ok {
			delegations := p.toolDelegations()
			if !delegations.isShared() {
				// 同一 exec 帧补齐关联字段：f1 exec_id、f15 id 原值（回写真实结果时用）。
				execID := cursorVarintField(execParts, 1)
				var idField []byte
				if idF, exists := cursorFindField(execParts, 15); exists {
					idField = append([]byte(nil), idF.Bytes...)
				}
				id, name, argsJSON, derr := cursorDecodeMcpArgs(argsField.Bytes, p.declaredToolDefs)
				if derr != nil {
					return cursorStreamEvent{}, derr
				}
				// 无损交付：argsJSON 由原生 Value 逐项标准解码后组装的对象 JSON，
				// 不再做 map[string]any 往返 / schema retype / 旧格式 fallback。
				if derr := delegations.noteDelegation("", cursorToolCallCapture{
					ID: id, Name: name, ArgsJSON: argsJSON,
					ExecID: execID, IDField: idField,
				}); derr != nil {
					return cursorStreamEvent{}, derr
				}
			}
		} else if !cursorFindFieldOK(execParts, 10) && !cursorFindFieldOK(execParts, 36) {
			// 不含 f11 mcp_args 的 exec 帧按统一字段表分类（方案 §8.2），不得静默跳过
			// 未实现的执行：
			//   - f10 request_context / f36 mcp_state 由 duplex 应答，独立 parser 无应答
			//     能力，按原职责忽略（上面已排除，继续向下找 InteractionUpdate）；
			//   - 意外 native exec 明确失败（零事件、零委托登记）；
			//   - 未知执行字段明确失败；
			//   - 纯元数据不承载执行指令，忽略。
			if n, ok := cursorNativeExecRejectField(execParts); ok {
				return cursorStreamEvent{}, cursorFrameErrorf(cursorFrameMessage,
					"unexpected native exec field %d", n)
			}
			if !cursorExecMetadataOnly(execParts) {
				return cursorStreamEvent{}, cursorFrameErrorf(cursorFrameMessage, "unsupported exec fields")
			}
		}
	}
	// 只有 interaction_update（f1）承载文本与 usage；其余分支（exec / kv /
	// checkpoint / 计时）一律丢弃，且**不解码其内容**——例如 ttft_breakdown 可能
	// 含定长（wire 1/5）字段，解码会触发本层的失败关闭。
	f, ok := cursorFindField(fields, cursorFldServerInteractionUpdate)
	if !ok {
		return cursorStreamEvent{Kind: cursorEventSkip}, nil
	}
	update, err := cursorDecodeProtobuf(f.Bytes)
	if err != nil {
		return cursorStreamEvent{}, err
	}

	// InteractionUpdate.f2 tool_call_started：登记到批次（§25.3 封口依据之一）。
	//
	// ToolCall 的 mcp_tool_call（f15）分支是**对外**委托（客户端要执行）；其余分支是
	// 内部元工具（如 get_mcp_tools），只参与同批完整性判断，绝不交给客户端。
	if tcStarted, ok := cursorFindField(update, cursorFldUpdateToolCallStarted); ok {
		if err := p.noteToolCallStarted(tcStarted.Bytes); err != nil {
			return cursorStreamEvent{}, err
		}
	}
	// InteractionUpdate.f27 tool_requests_listed：本批总数声明（call_count，可先于最后一个 started）。
	if listed, ok := cursorFindField(update, cursorFldUpdateToolRequestsListed); ok {
		if err := p.noteToolRequestsListed(listed.Bytes); err != nil {
			return cursorStreamEvent{}, err
		}
	}

	if turn, ok := cursorFindField(update, cursorFldUpdateTurnEnded); ok {
		turnFields, err := cursorDecodeProtobuf(turn.Bytes)
		if err != nil {
			return cursorStreamEvent{}, err
		}
		// §25.17.5 A06①：存在性逐字段保留（缺失 / 真实 0 / 非法各不相同），
		// 非法值由解码器按解析失败上报，绝不悄悄当 0 写进 usage。
		usage, err := cursorDecodeTurnUsage(turnFields)
		if err != nil {
			return cursorStreamEvent{}, err
		}
		p.usage = usage
		return cursorStreamEvent{Kind: cursorEventSkip}, nil
	}

	delta, ok := cursorFindField(update, cursorFldUpdateTextDelta)
	if !ok {
		return cursorStreamEvent{Kind: cursorEventSkip}, nil
	}
	deltaFields, err := cursorDecodeProtobuf(delta.Bytes)
	if err != nil {
		return cursorStreamEvent{}, err
	}
	text := ""
	if tf, ok := cursorFindField(deltaFields, cursorFldTextDeltaText); ok && tf.Wire == cursorWireBytes {
		text = tf.String()
	}
	// 增量语义依据：更新类型名即 text_**delta**_update（32 号证据 Gyt），且
	// InteractionUpdate 另有独立的 summary / thinking_delta 承担"中间态/全量"语义。
	//
	// 出口纪律：上游把推理内联进正文（<think>…</think>），此处统一双通道切分——
	// 思考进 Reasoning，正文进 Text，完整标签被彻底吞噬。本帧两个通道都空时返回 skip
	// （空 delta 不得产出空 content / 空 reasoning block）。
	reasoning, visible := p.parseThinkDeltas(text)
	if visible == "" && reasoning == "" {
		return cursorStreamEvent{Kind: cursorEventSkip}, nil
	}
	return cursorStreamEvent{Kind: cursorEventText, Reasoning: reasoning, Text: visible}, nil
}

// cursorIntField 取 varint 字段的 int 值（字段缺失时为 0）。
//
// ⚠️ **不得用于 usage 字段**：它把"缺失"与"错误 wire"都折成 0，正是 §25.17.5 A06①
// 要消除的折叠。turn_ended 的 f1–f6 一律走 cursorDecodeTurnUsage（保留存在性 + 校验）。
func cursorIntField(fields []cursorPBField, number int) int {
	f, ok := cursorFindField(fields, number)
	if !ok || f.Wire != cursorWireVarint {
		return 0
	}
	return int(f.Varint)
}

// cursorVarintField 取 varint 字段的原始值（字段缺失时为 0）。
func cursorVarintField(fields []cursorPBField, number int) uint64 {
	f, ok := cursorFindField(fields, number)
	if !ok || f.Wire != cursorWireVarint {
		return 0
	}
	return f.Varint
}

// noteToolCallStarted 把 InteractionUpdate.f2 tool_call_started 登记进当前批次。
//
// 真实帧结构（活体取证，派发单 B1 事实依据；B1b 起为唯一被认布局）：
//
//	started.f1 (string) call_id        —— 对外委托 ID
//	started.f2 (msg)    ToolCall
//	   f15 (msg) mcp_tool_call   → 对外（客户端要执行）
//	   f44 (msg) get_mcp_tools  → 内部元工具（只参与同批完整性）
//	started.f3 (string) model_call_id  —— 本批归属键
//
// 解析：call_id←started.f1、model_call_id←started.f3、external←ToolCall 含 f15（且不含 f44）。
// 旧合成帧布局（call_id 在 ToolCall.f1、ToolCall 整体在 started.f15）的双模兼容分支已删除
// （派发单 B1b）：解析器只认真实帧。
//
// 成功路径顺带维护本地归属计数（lastModelCallID / batchCounts / 消费 pendingListed），
// 这些**只用于 listed 归属判定**，不参与封口、不影响任何输出字节（派发单 B1 §4.2）。
func (p *cursorStreamParser) noteToolCallStarted(b []byte) error {
	fields, err := cursorDecodeProtobuf(b)
	if err != nil {
		return err
	}
	delegations := p.toolDelegations()

	// call_id：真实帧在 started.f1。
	callID := ""
	if f, ok := cursorFindField(fields, cursorFldToolCallID); ok {
		callID = strings.TrimSpace(f.String())
	}

	// model_call_id：真实帧在 started.f3。
	modelCallID := ""
	if f, ok := cursorFindField(fields, cursorFldToolCallStartedModelCallID); ok {
		modelCallID = strings.TrimSpace(f.String())
	}

	// external：真实帧看 started.f2（ToolCall 消息）内层是否含 f15（mcp_tool_call）；
	// 含 f44（get_mcp_tools）或其它分支即内部。started.f2 缺失按内部。
	external := false
	if tc, ok := cursorFindField(fields, cursorFldToolCallStartedToolCallMsg); ok {
		if inner, e := cursorDecodeProtobuf(tc.Bytes); e == nil {
			_, hasMcp := cursorFindField(inner, cursorFldToolCallStartedToolCall) // f15
			_, hasGetMcp := cursorFindField(inner, cursorFldToolCallGetMcpTools)  // f44
			external = hasMcp && !hasGetMcp
		}
	}

	if err := delegations.noteToolCallStarted(modelCallID, callID, external && callID != ""); err != nil {
		return err
	}

	// ---- 本地归属计数维护（仅 listed 归属用）----
	p.lastModelCallID = modelCallID
	if p.batchCounts == nil {
		p.batchCounts = make(map[string]*cursorListedCounts)
	}
	bc := p.batchCounts[modelCallID]
	if bc == nil {
		bc = &cursorListedCounts{}
		p.batchCounts[modelCallID] = bc
	}
	if external && callID != "" {
		bc.started++
	} else {
		bc.internal++
	}
	// 消费挂起的 listed（先于本 started 到达），归属到当前新批。
	if p.pendingListed != nil {
		if e := p.toolDelegations().noteToolRequestsListed(modelCallID, *p.pendingListed); e != nil {
			return e
		}
		bc.listedSeen = true
		p.pendingListed = nil
	}
	return nil
}

// noteToolRequestsListed 解析 InteractionUpdate.f27 tool_requests_listed 的 call_count
// 并按上下文归属到批次（§25.3 / 派发单 B1 §4.3）。
//
// 字段号依据 design-evidence.md：ToolRequestsListedUpdate 内层**只有** f1 call_count
// （uint32），**没有** model_call_id 字段——归属只能靠当前开放批上下文判定。
//
// 归属规则（listed 无 ID，按上下文；未匹配则挂起给下一个 started 新批）：
//   - lastModelCallID != "" 且该批 `!listedSeen || started+internal < count`
//     → 归属当前批（delegations.noteToolRequestsListed(lastModelCallID, count)），
//     并标记该批 listedSeen=true；
//   - 否则（当前批已 listed 且计数已齐，或根本无 started 上下文）→ 挂起为 pendingListed，
//     返回 nil：**不建批、不报歧义、不失败**。
//
// 纪律：call_count 缺失 / wire 非 varint / 超 uint32 三种情况全部失败关闭（保留原有
// 三处校验），不猜数值、不退回"只登记出现"的降级路径；本层不打印任何工具参数（脱敏）。
func (p *cursorStreamParser) noteToolRequestsListed(b []byte) error {
	fields, err := cursorDecodeProtobuf(b)
	if err != nil {
		return err
	}
	f, ok := cursorFindField(fields, cursorFldToolRequestsListedCallCount)
	if !ok {
		return cursorFrameErrorf(cursorFrameMessage, "tool_requests_listed without call_count")
	}
	if f.Wire != cursorWireVarint {
		return cursorFrameErrorf(cursorFrameMessage, "tool_requests_listed.call_count has wrong wire type")
	}
	if f.Varint > math.MaxUint32 {
		return cursorFrameErrorf(cursorFrameMessage, "tool_requests_listed.call_count overflows uint32")
	}
	count := int(f.Varint)

	// 归属：listed 无 model_call_id。有最近 started 且该批尚未 listed、或本批已收
	// started 数 < count（尚未收齐，仍归当前批）→ 归属当前批；其余挂起 pending。
	if p.lastModelCallID != "" {
		if p.batchCounts == nil {
			p.batchCounts = make(map[string]*cursorListedCounts)
		}
		bc := p.batchCounts[p.lastModelCallID]
		if bc == nil {
			bc = &cursorListedCounts{}
			p.batchCounts[p.lastModelCallID] = bc
		}
		if !bc.listedSeen || bc.started+bc.internal < count {
			if e := p.toolDelegations().noteToolRequestsListed(p.lastModelCallID, count); e != nil {
				return e
			}
			bc.listedSeen = true
			return nil
		}
	}
	// 无 started 上下文，或当前批已 listed 且计数已齐：挂起，不建批、不报歧义、不失败。
	p.pendingListed = &count
	return nil
}

// cursorDecodeMcpArgs 按第三方真实格式读取 exec 内层 f11（mcp_args）：
//   - f3 = tool_call_id（string）；
//   - f5 = 真实工具名（string，客户端原工具，不再限定任何本站包装工具名）；
//   - 顶层每条 f2 = 一条 map entry（f1 key string、f2 google.protobuf.Value）。
//
// 每个 Value 逐条经 cursorDecodeMcpValue 标准解码为 JSON 字节，收进
// map[string]json.RawMessage，再用 json.Marshal 组装完整对象（空参数集为合法 {}）；
// 不为组装把参数解成 map[string]any/float64，不猜类型、不补值、不做旧壳兼容。
// 重复 key 明确失败关闭，绝不静默覆盖。
//
// 声明校验：declaredToolDefs 每条 McpToolDefinition 的 f5 是真实工具名；集合为空
// 或本次 f5 不在集合内一律失败关闭。duplex 与独立 parser 共用本实现。
func cursorDecodeMcpArgs(payload []byte, declaredToolDefs [][]byte) (string, string, string, error) {
	fields, err := cursorDecodeProtobuf(payload)
	if err != nil {
		return "", "", "", err
	}
	idF, _ := cursorFindField(fields, 3)
	if idF.Wire != cursorWireBytes {
		return "", "", "", cursorFrameErrorf(cursorFrameMcpArgs, "mcp args call id has wrong wire type")
	}
	nameF, _ := cursorFindField(fields, 5)
	if nameF.Wire != cursorWireBytes {
		return "", "", "", cursorFrameErrorf(cursorFrameMcpArgs, "mcp args tool name has wrong wire type")
	}
	toolName := nameF.String()

	// 原生参数：顶层全部 f2 字段，每条即一条 map entry（{f1 key, f2 Value}）。
	args := make(map[string]json.RawMessage)
	for _, f := range fields {
		if f.Number != 2 {
			continue
		}
		entryInner, e := cursorDecodeProtobuf(f.Bytes)
		if e != nil {
			return "", "", "", e
		}
		keyF, ok := cursorFindField(entryInner, 1)
		if !ok || keyF.Wire != cursorWireBytes {
			return "", "", "", cursorFrameErrorf(cursorFrameMcpArgs, "mcp args entry has no key")
		}
		valF, ok := cursorFindField(entryInner, 2)
		if !ok || valF.Wire != cursorWireBytes {
			return "", "", "", cursorFrameErrorf(cursorFrameMcpArgs, "mcp args entry has no value")
		}
		key := keyF.String()
		if _, exists := args[key]; exists {
			return "", "", "", cursorFrameErrorf(cursorFrameMcpArgs, "duplicate mcp arg %q", key)
		}
		v, e := cursorDecodeMcpValue(valF.Bytes)
		if e != nil {
			return "", "", "", e
		}
		args[key] = json.RawMessage(v)
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return "", "", "", cursorFrameErrorWrapf(cursorFrameMcpArgs, err, "marshal mcp args: %v", err)
	}

	// 声明校验：集合为空或本次真实工具名未被声明都失败关闭。
	declared := make(map[string]bool, len(declaredToolDefs))
	for _, def := range declaredToolDefs {
		defFields, e := cursorDecodeProtobuf(def)
		if e != nil {
			return "", "", "", cursorFrameErrorWrapf(cursorFrameMcpArgs, e, "decode tool definition: %v", e)
		}
		if name, ok := cursorFindField(defFields, cursorFldMcpToolDefToolName); ok {
			declared[name.String()] = true
		}
	}
	if !declared[toolName] {
		return "", "", "", cursorFrameErrorf(cursorFrameMcpArgs, "undeclared tool name %q", toolName)
	}
	return idF.String(), toolName, string(argsJSON), nil
}

// cursorDecodeMcpValue 解析 McpArgs args 的单个 Value。
//
// 改用仓库已有的 protobuf 标准库（google.golang.org/protobuf）解码，不再手写 wire
// 循环：proto.Unmarshal → structpb.Value →（kind 未设置 / 非有限数值校验）→
// protojson.Marshal → json.Compact（去除 protojson 的随机空格）。任何错误路径都返回
// ErrCursorProtocol 且返回空串——绝不把畸形/未设置 kind 补成 JSON null，也绝不把
// NaN/±Inf 当成字符串写入。
//
// Value oneof（派发单 §2）：f1 null / f2 number(double) / f3 string / f4 bool /
// f5 Struct / f6 ListValue，六种分支都按 google.protobuf.Value 标准语义解码。
func cursorDecodeMcpValue(b []byte) (string, error) {
	value := &structpb.Value{}
	if err := proto.Unmarshal(b, value); err != nil {
		return "", cursorFrameErrorWrapf(cursorFrameMcpArgs, err, "unmarshal mcp value: %v", err)
	}
	// kind 未设置（等价于 WhichOneof("kind")==nil）与"合法 null（Value_NullValue）"
	// 不是一回事：前者说明这条 Value 没有写任何 oneof 分支，必须明确失败，绝不允许
	// 落到 JSON null（那样会静默吞掉畸形/未设置的参数）。
	if value.Kind == nil {
		return "", cursorFrameErrorf(cursorFrameMcpArgs, "mcp value has no kind set")
	}
	// 非有限数值必须明确失败：protojson 会把 NaN/±Inf 写成字符串 "NaN"/"Infinity"，
	// 等于把数字静默改成字符串（与手写循环把数字读错的同类错误）。任意深度（嵌套
	// Struct 字段、ListValue 元素）都要检查。
	if err := cursorCheckFiniteValue(value); err != nil {
		return "", err
	}
	out, err := protojson.Marshal(value)
	if err != nil {
		return "", cursorFrameErrorWrapf(cursorFrameMcpArgs, err, "marshal mcp value: %v", err)
	}
	// protojson 对单行输出在逗号后随机插空格（detrand），同一输入会产出不同字节；
	// 序列化后必须用 json.Compact 归一经压缩结果再返回（只去除无语义空白，不改语义）。
	var buf bytes.Buffer
	if err := json.Compact(&buf, out); err != nil {
		return "", cursorFrameErrorWrapf(cursorFrameMcpArgs, err, "compact mcp value: %v", err)
	}
	return buf.String(), nil
}

// cursorCheckFiniteValue 递归校验 structpb.Value 内的全部数值是否为有限值。
//
// NumberValue 为 NaN/±Inf、或任意嵌套层级（StructValue 的字段、ListValue 的元素）
// 含非有限数值，均返回 ErrCursorProtocol。
func cursorCheckFiniteValue(v *structpb.Value) error {
	switch k := v.Kind.(type) {
	case *structpb.Value_NumberValue:
		if math.IsNaN(k.NumberValue) || math.IsInf(k.NumberValue, 0) {
			return cursorFrameErrorf(cursorFrameMcpArgs, "mcp value number is not finite")
		}
	case *structpb.Value_StructValue:
		for _, fv := range k.StructValue.GetFields() {
			if err := cursorCheckFiniteValue(fv); err != nil {
				return err
			}
		}
	case *structpb.Value_ListValue:
		for _, ev := range k.ListValue.GetValues() {
			if err := cursorCheckFiniteValue(ev); err != nil {
				return err
			}
		}
	}
	return nil
}

// cursorParseEndStream 解析 EndStream 帧 payload。
//
// EndStream 的 payload 是 **JSON**（不是 protobuf）。无 error 时返回两个空串。
//
// secrets 里的凭证值会被抹掉（见 cursorRedactSecrets）——EndStream 的 message
// 同样可能回显 Authorization 头的值。
func cursorParseEndStream(payload []byte, secrets ...string) (string, string, error) {
	if len(payload) == 0 {
		return "", "", nil
	}
	var env struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details []struct {
				Type  string `json:"type"`
				Debug *struct {
					Error   string `json:"error"`
					Details *struct {
						Title  string `json:"title"`
						Detail string `json:"detail"`
					} `json:"details"`
				} `json:"debug"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return "", "", cursorFrameErrorWrapf(cursorFrameEndStreamPayload, err, "EndStream payload is not JSON: %v", err)
	}
	if env.Error == nil {
		return "", "", nil
	}
	// 只取上游的业务错误码与文案；**不回显 details[].value**（base64 嵌套 protobuf，
	// 可能含账号标识），也不落盘、不进日志。
	//
	// code 取值优先级：`details[].debug.error`（业务码，如 ERROR_UNAUTHORIZED /
	// ERROR_BAD_MODEL_NAME）> 顶层 `code`（Connect 传输码，如 unauthenticated）。
	// 业务码更有诊断价值——转换层要靠它区分「模型名错」与「鉴权错」。
	var businessCode, msg string
	for _, d := range env.Error.Details {
		if d.Debug == nil {
			continue
		}
		if businessCode == "" {
			businessCode = d.Debug.Error
		}
		if d.Debug.Details != nil && msg == "" {
			msg = strings.TrimSpace(strings.TrimSpace(d.Debug.Details.Detail) + " " + strings.TrimSpace(d.Debug.Details.Title))
		}
	}
	code := businessCode
	if code == "" {
		code = env.Error.Code
	}
	if msg == "" {
		msg = env.Error.Message
	}
	if code == "" {
		code = "unknown"
	}
	return cursorRedactSecrets(code, secrets...), cursorRedactSecrets(strings.TrimSpace(msg), secrets...), nil
}

// ---------------------------------------------------------------------------
// OpenAI SSE chunk 生成
// ---------------------------------------------------------------------------

// cursorSSERoleChunk 返回携带 role 的首包（OpenAI 形状）。
func (p *cursorStreamParser) cursorSSERoleChunk() string {
	return "data: " + p.cursorChunk(`"delta":{"role":"assistant","content":""},"finish_reason":null`) + "\n\n"
}

// cursorSSETextChunk 返回携带增量文本的 chunk。
func (p *cursorStreamParser) cursorSSETextChunk(text string) string {
	return "data: " + p.cursorChunk(`"delta":{"content":`+cursorJSONString(text)+`},"finish_reason":null`) + "\n\n"
}

// cursorSSEReasoningChunk 返回携带增量思考内容的 chunk（OpenAI reasoning_content）。
//
// 思考内容与正文走两条独立通道：客户端（尤其带思考态 UI 的）需要单独消费 reasoning，
// 不能混进 content。标签（<think>/</think>）已在 parseThinkDeltas 被彻底吞噬，永不出现
// 在本 chunk 里。
func (p *cursorStreamParser) cursorSSEReasoningChunk(reasoning string) string {
	return "data: " + p.cursorChunk(`"delta":{"reasoning_content":`+cursorJSONString(reasoning)+`},"finish_reason":null`) + "\n\n"
}

// cursorChunkIDJSON 渲染本 HTTP 实际输出响应 ID 的 JSON 字符串字面量。
//
// §25.17.2「实际 ID」：同一 HTTP 的所有 chunk 必须共用同一个响应 ID，且不能是固定
// 常量（旧的固定 chat 常量会经共享回桥泄入 Responses 回包，导致 previous_response_id
// 永远无法续接）。
func (p *cursorStreamParser) cursorChunkIDJSON() string { return cursorJSONString(p.chunkID()) }

// cursorSSEToolCallNameChunk 返回 tool_calls 首个 delta（含 id/type/function.name）。
func (p *cursorStreamParser) cursorSSEToolCallNameChunk(index int, tc cursorToolCallCapture) string {
	return "data: " + `{"id":` + p.cursorChunkIDJSON() + `,"object":"chat.completion.chunk","created":0,` +
		`"model":` + cursorJSONString(p.model) + `,` +
		`"choices":[{"index":0,"delta":{"tool_calls":[{"index":` + strconv.Itoa(index) +
		`,"id":` + cursorJSONString(tc.wireID()) + `,"type":"function","function":{"name":` +
		cursorJSONString(tc.Name) + `,"arguments":""}}]},"finish_reason":null}]}` + "\n\n"
}

// cursorSSEToolCallArgsChunk 返回 tool_calls 的 arguments delta（一块完整 arguments）。
func (p *cursorStreamParser) cursorSSEToolCallArgsChunk(index int, tc cursorToolCallCapture) string {
	args := tc.ArgsJSON
	if strings.TrimSpace(args) == "" || args == "null" {
		args = "{}"
	}
	return "data: " + `{"id":` + p.cursorChunkIDJSON() + `,"object":"chat.completion.chunk","created":0,` +
		`"model":` + cursorJSONString(p.model) + `,` +
		`"choices":[{"index":0,"delta":{"tool_calls":[{"index":` + strconv.Itoa(index) +
		`,"function":{"arguments":` + cursorJSONString(args) + `}}]},"finish_reason":null}]}` + "\n\n"
}

// cursorSSEFinishChunk 返回收尾 chunk（**带真实 usage**）。
//
// usage 只可能来自 turn_ended：收到就按实测值写出（input/output/cache_read/
// cache_write），没收到就写**显式零值**。语义是"本层没有 token 数据、计 0"，
// 而不是"测得 0 个 token"——**绝不估算、绝不伪造**（伪造会直接污染 usage_logs
// 的按 token 计费口径）。
//
// finish_reason：本轮捕获了工具委托（tool_calls）则为 "tool_calls"，否则 "stop"。
func (p *cursorStreamParser) cursorSSEFinishChunk() string {
	finishReason := "stop"
	if len(p.capturedToolCalls()) > 0 {
		finishReason = "tool_calls"
	}
	return p.cursorSSEFinishChunkWithReason(finishReason)
}

// cursorSSEFinishChunkWithReason 按**显式**归因渲染收尾 chunk（**带真实 usage**）。
//
// 流式跨 HTTP 的交接/终态必须显式给归因：工具交接是 "tool_calls"，而续轮恢复后的真实
// 终态即使历史批里有委托也必须是 "stop"（已交付的 tool_calls 不在本轮重播）。
func (p *cursorStreamParser) cursorSSEFinishChunkWithReason(finishReason string) string {
	// usage 在 **chunk 顶层**（不是 choices[0] 内）：既有 usage 提取链
	// （parseSSEUsageBytesWithType → extractOpenAIUsageFromJSONBytes）只认顶层的
	// `usage` 路径，塞进 choices 里会被整条丢掉、计费拿不到真实用量。
	return "data: " + `{"id":` + p.cursorChunkIDJSON() + `,"object":"chat.completion.chunk","created":0,` +
		`"model":` + cursorJSONString(p.model) +
		`,"choices":[{"index":0,"delta":{},"finish_reason":"` + finishReason + `"}],` +
		`"usage":` + p.cursorUsageJSON() + `}` + "\n\n"
}

// cursorUsageJSON 渲染 usage 对象（OpenAI 兼容键名，供既有 usage 提取链消费）。
//
// 键名取 `input_tokens` / `output_tokens` / `cache_read_input_tokens` /
// `cache_creation_input_tokens`：与 openAIUsageFromGJSON 的读取口径一致；
// `total_tokens` 为四项之和，给只认 total 的客户端兜底。
func (p *cursorStreamParser) cursorUsageJSON() string {
	u := p.usage
	total := u.Input + u.Output + u.CacheRead + u.CacheWrite
	return `{"input_tokens":` + strconv.Itoa(u.Input) +
		`,"output_tokens":` + strconv.Itoa(u.Output) +
		`,"cache_read_input_tokens":` + strconv.Itoa(u.CacheRead) +
		`,"cache_creation_input_tokens":` + strconv.Itoa(u.CacheWrite) +
		`,"total_tokens":` + strconv.Itoa(total) + `}`
}

// cursorChunk 渲染一条 chat.completion.chunk 的 JSON 本体（不含 `data: ` 前缀）。
func (p *cursorStreamParser) cursorChunk(choices string) string {
	return `{"id":` + p.cursorChunkIDJSON() + `,"object":"chat.completion.chunk","created":0,` +
		`"model":` + cursorJSONString(p.model) + `,"choices":[{"index":0,` + choices + `}]}`
}

// cursorSSEDoneSentinel 是 OpenAI 的流终止哨兵。
//
// Cursor 侧**没有**这个哨兵，但 OpenAI 兼容客户端普遍以它判定流结束，故由转换层
// 补上——这是**协议适配**，不是伪造业务数据（与"禁止伪造 usage"是两条不同的红线）。
const cursorSSEDoneSentinel = "data: [DONE]\n\n"

// cursorJSONString 把任意字符串编码为 JSON 字符串字面量（含转义与引号）。
//
// 不用 encoding/json 是为了避免每帧一次分配；这里的转义规则与
// json.Marshal 对字符串的行为一致（含 <>& 的默认转义不需要，JSON 不要求）。
func cursorJSONString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// ---------------------------------------------------------------------------
// 非流式入站：合成为单条 chat.completion
// ---------------------------------------------------------------------------

// cursorBuildNonStreamingCompletion 把解析结果渲染成一条 OpenAI
// `chat.completion` JSON（供 handleNonStreamingResponse 消费）。
//
// usage 与流式同口径：有 turn_ended 就写实测值，没有就写**显式零值**——
// 绝非编造。零值而非省略 usage 字段是必须的：通用非流式路径要求 usage 是对象，
// 缺失会以 `parse response: invalid json response` 拒绝整条响应。
//
// 本轮捕获了工具委托（tool_calls）时：assistant message 附 tool_calls 且
// finish_reason="tool_calls"；content 为 null。否则 content=文本、finish_reason="stop"。
func (p *cursorStreamParser) cursorBuildNonStreamingCompletion() []byte {
	var msg string
	finishReason := "stop"
	// 只聚合**本 HTTP** 捕获的调用与正文：续轮第二个 HTTP 不重播上一批（§25.17.2）。
	// 出站 id 由调用方在渲染前通过 assignToolCallWireIDs 决定（见 cursorParseStream 与
	// cursorParseStreamToCompletion）；这里只按 wireID() 渲染。
	calls := p.httpToolCalls()
	if len(calls) > 0 {
		finishReason = "tool_calls"
		msg = `"role":"assistant","content":null,"tool_calls":[`
		for i, tc := range calls {
			if i > 0 {
				msg += ","
			}
			args := tc.ArgsJSON
			if strings.TrimSpace(args) == "" || args == "null" {
				args = "{}"
			}
			msg += `{"id":` + cursorJSONString(tc.wireID()) +
				`,"type":"function","function":{"name":` + cursorJSONString(tc.Name) +
				`,"arguments":` + cursorJSONString(args) + `}}`
		}
		msg += `]`
	} else {
		msg = `"role":"assistant","content":` + cursorJSONString(p.httpContent())
	}
	out := `{"id":` + p.cursorChunkIDJSON() + `,"object":"chat.completion","created":0,` +
		`"model":` + cursorJSONString(p.model) + `,` +
		`"choices":[{"index":0,"message":{` + msg + `},` +
		`"finish_reason":"` + finishReason + `"}],` +
		`"usage":` + p.cursorUsageJSON() + `}`
	return []byte(out)
}

// cursorParseStreamToSSE 一步到位：解析 Cursor 流并返回完整 OpenAI SSE 响应体
// （含 `[DONE]` 哨兵）。
func cursorParseStreamToSSE(body []byte, model string, secrets ...string) ([]byte, error) {
	p := cursorNewStreamParser(model, secrets...)
	lines, err := p.cursorParseStream(body)
	if err != nil {
		return nil, err
	}
	var buf []byte
	for _, l := range lines {
		buf = append(buf, l...)
	}
	buf = append(buf, cursorSSEDoneSentinel...)
	return buf, nil
}

// cursorParseStreamToCompletion 一步到位：解析 Cursor 流并返回单条
// chat.completion JSON（非流式入站）。
//
// 调用方若还要计费口径的 OpenAIUsage，直接用 cursorNewStreamParser +
// cursorParseStream 后取 p.usage.cursorOpenAIUsage()（与这里写进 JSON 的
// 是同一份真实用量，不会二次取值）。
func cursorParseStreamToCompletion(body []byte, model string, secrets ...string) ([]byte, error) {
	p := cursorNewStreamParser(model, secrets...)
	if _, err := p.cursorParseStream(body); err != nil {
		return nil, err
	}
	// 交付侧 id 重写：非流式整包渲染前先决定每个调用的出站 id；失败即整包失败关闭
	// （绝不把上游复用的原 id 原样交给客户端）。
	if err := p.assignToolCallWireIDs(); err != nil {
		return nil, err
	}
	return p.cursorBuildNonStreamingCompletion(), nil
}
