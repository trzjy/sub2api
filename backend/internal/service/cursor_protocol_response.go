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
}

func (s *cursorSSEStream) Close() error { return s.source.Close() }

// terminalError may be read after the gateway's timeout path returns while its
// scanner is still unwinding. Parser state belongs exclusively to that scanner.
func (s *cursorSSEStream) terminalError() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
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

func (s *cursorSSEStream) next() error {
	header := make([]byte, cursorEnvelopeHeaderSize)
	if _, err := io.ReadFull(s.source, header); err != nil {
		if err == io.EOF {
			return ErrCursorMissingEndStream
		}
		return fmt.Errorf("%w: read envelope: %w", ErrCursorProtocol, err)
	}
	n := int(binary.BigEndian.Uint32(header[1:]))
	if n > cursorUpstreamStreamMaxBytes {
		return fmt.Errorf("%w: oversized envelope", ErrCursorProtocol)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(s.source, payload); err != nil {
		return fmt.Errorf("%w: read payload: %w", ErrCursorProtocol, err)
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
			return fmt.Errorf("%w: %s: %s", ErrCursorUpstreamStream, code, msg)
		}
		s.done = true
	} else {
		ev, err := s.parser.cursorParseMessageFrame(frame.Data)
		if err != nil {
			return err
		}
		if ev.Kind == cursorEventText {
			s.parser.content += ev.Text
			if rejected := cursorMatchKeyRejected(s.parser.content); rejected != nil {
				return rejected
			}
			if !s.parser.started {
				s.buffer.WriteString(s.parser.cursorSSERoleChunk())
				s.parser.started = true
			}
			s.parser.markFirstToken()
			s.buffer.WriteString(s.parser.cursorSSETextChunk(ev.Text))
		}
		s.done = s.parser.usage.Seen
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
			if err := s.parser.sealableDelegations(); err == nil {
				s.delegated = true
				s.done = true
				handoffThisFinish = true
			}
		}
	}
	if s.done {
		if !s.parser.started {
			s.buffer.WriteString(s.parser.cursorSSERoleChunk())
			s.parser.started = true
		}
		// 工具委托捕获：在 finish 前吐 tool_calls delta（首个含 id/type/function.name，
		// 随后一块 arguments），再发 finish_reason:"tool_calls" 的收尾 chunk。
		// 续轮恢复后的终态不再重复吐已交付的 tool_calls，只发 finish。
		if calls := s.parser.capturedToolCalls(); len(calls) > s.emittedCalls {
			for i := s.emittedCalls; i < len(calls); i++ {
				s.buffer.WriteString(s.parser.cursorSSEToolCallNameChunk(i, calls[i]))
				s.buffer.WriteString(s.parser.cursorSSEToolCallArgsChunk(i, calls[i]))
			}
			s.emittedCalls = len(calls)
			handoffThisFinish = true
		}
		if handoffThisFinish {
			s.buffer.WriteString(s.parser.cursorSSEFinishChunkWithReason("tool_calls"))
		} else {
			s.buffer.WriteString(s.parser.cursorSSEFinishChunk())
		}
		s.buffer.WriteString("data: [DONE]\n\n")
	}
	return nil
}

// agent.v1.AgentServerMessage（32 号证据：KW）的 oneof 字段号。
const (
	cursorFldServerInteractionUpdate = 1 // InteractionUpdate —— 文本与 usage 的唯一落点
	cursorFldServerExecMessage       = 2 // ExecServerMessage —— 首版不响应（无执行通道）
	cursorFldServerCheckpoint        = 3 // ConversationStateStructure —— 丢弃
	cursorFldServerKvMessage         = 4 // KvServerMessage —— 丢弃（单向 POST 无法应答 KV 问答）
	cursorFldServerExecControl       = 5 // ExecServerControlMessage —— 丢弃
	cursorFldServerInteractionQuery  = 7 // InteractionQuery —— 丢弃
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

// agent.v1.ToolCallStartedUpdate（32 号证据 Vyt）内：
//
// f15 = ToolCall（mcp_tool_call 分支，活体 tool_call_started 样本携带）。
const (
	cursorFldToolCallStartedToolCall = 15 // ToolCall 消息
	// cursorFldToolCallID 是 ToolCall.mcp_tool_call 内的 call_id（= OpenAI tool_calls[].id）。
	cursorFldToolCallID = 1
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
)

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

// cursorMatchKeyRejected 在一段文本里匹配固定拒付模板，命中即返回结构化错误。
func cursorMatchKeyRejected(text string) *CursorKeyRejectedError {
	idx := strings.Index(text, cursorUpstreamRejectTemplate)
	if idx < 0 {
		return nil
	}
	fragment := text[idx:]
	if nl := strings.IndexByte(fragment, '\n'); nl >= 0 {
		fragment = fragment[:nl]
	}
	turnID := ""
	if open := strings.IndexByte(fragment, '('); open >= 0 {
		if close := strings.IndexByte(fragment[open:], ')'); close > 0 {
			turnID = strings.TrimSpace(fragment[open+1 : open+close])
		}
	}
	return &CursorKeyRejectedError{Template: strings.TrimSpace(fragment), TurnID: turnID}
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
type cursorTurnUsage struct {
	// Seen 表示是否收到过 turn_ended 帧。
	Seen bool
	// Input / Output / CacheRead / CacheWrite 直接对应 TurnEndedUpdate 的 f1..f4。
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
	// content 是累加后的当前完整文本（TextDeltaUpdate 是**增量**，见下）。
	content string
	// usage 是 turn_ended 给出的真实用量（未收到则 Seen=false，各项为 0）。
	usage cursorTurnUsage
	// rejected 命中流内拒付模板后的结构化错误（非 nil 即终态失败）。
	rejected *CursorKeyRejectedError

	// delegations 是本轮工具委托的**唯一**记录（§24.2-6）。
	//
	// duplex 解码 mcp_args 后写入这里，parser 只读取并输出，不再对同一帧二次解码；
	// 批次的 started/listed/封口判定也都在该记录内（§25.3）。
	delegations *cursorToolDelegations
	// firstTokenMs 是首帧文本的耗时（毫秒，自解析开始计时），供转发结果诊断。
	firstTokenMs int
	parseStart   time.Time
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
}

// cursorContent 返回拼出的完整响应文本（非流式入站用）。
func (p *cursorStreamParser) cursorContent() string { return p.content }

// cursorNewStreamParser 构造解析器（model 为空时 chunk 内 model 字段留空）。
//
// secrets 是需要从错误文案里抹掉的凭证值（如本次对话用的 session_token）。
func cursorNewStreamParser(model string, secrets ...string) *cursorStreamParser {
	return &cursorStreamParser{model: model, secrets: secrets, parseStart: time.Now()}
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
				return nil, fmt.Errorf("%w: received extra EndStreamResponse", ErrCursorProtocol)
			}
			p.endStreamSeen = true
			code, msg, err := cursorParseEndStream(frame.Data, p.secrets...)
			if err != nil {
				return nil, err
			}
			if code != "" {
				return nil, fmt.Errorf("%w: %s: %s", ErrCursorUpstreamStream, code, msg)
			}
			// **不 break**：EndStream 之后若还有任何帧都是协议违例（客户端文案
			// `received extra message after EndStreamResponse`），必须继续解析才能
			// 发现。提前 break 会让这类畸形流被当成正常流收尾。
			continue
		}
		if p.endStreamSeen {
			return nil, fmt.Errorf("%w: received extra message after EndStreamResponse", ErrCursorProtocol)
		}

		ev, err := p.cursorParseMessageFrame(frame.Data)
		if err != nil {
			return nil, err
		}
		if ev.Kind != cursorEventText {
			// 首版范围外：丢弃（用户裁定），不算协议错误。
			continue
		}
		if !p.started {
			p.started = true
			sseLines = append(sseLines, p.cursorSSERoleChunk())
		}
		p.content += ev.Text
		p.markFirstToken()
		sseLines = append(sseLines, p.cursorSSETextChunk(ev.Text))
		// 拒付模板可能跨帧：每收到一段就在**累加结果**上匹配一次。
		if rejected := cursorMatchKeyRejected(p.content); rejected != nil {
			p.rejected = rejected
		}
	}

	// 流内拒付是终态失败：**优先于**任何成功收尾判定。
	if p.rejected != nil {
		return nil, p.rejected
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
	// 工具委托捕获：在 finish 前吐 tool_calls delta（流式与 cursorParseStreamToSSE 同口径）。
	if calls := p.capturedToolCalls(); len(calls) > 0 {
		for i, tc := range calls {
			sseLines = append(sseLines, p.cursorSSEToolCallNameChunk(i, tc))
			sseLines = append(sseLines, p.cursorSSEToolCallArgsChunk(i, tc))
		}
	}
	sseLines = append(sseLines, p.cursorSSEFinishChunk())
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
				id, name, argsJSON, derr := cursorDecodeMcpArgs(argsField.Bytes)
				if derr != nil {
					return cursorStreamEvent{}, derr
				}
				if derr := delegations.noteDelegation("", cursorToolCallCapture{
					ID: id, Name: name, ArgsJSON: argsJSON,
				}); derr != nil {
					return cursorStreamEvent{}, derr
				}
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
		p.usage = cursorTurnUsage{
			Seen:       true,
			Input:      cursorIntField(turnFields, cursorFldTurnEndedInputTokens),
			Output:     cursorIntField(turnFields, cursorFldTurnEndedOutputTokens),
			CacheRead:  cursorIntField(turnFields, cursorFldTurnEndedCacheReadTokens),
			CacheWrite: cursorIntField(turnFields, cursorFldTurnEndedCacheWriteTokens),
			Reasoning:  cursorIntField(turnFields, cursorFldTurnEndedReasoningTokens),
			EndedAtMS:  cursorVarintField(turnFields, cursorFldTurnEndedEndedAtMS),
		}
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
	return cursorStreamEvent{Kind: cursorEventText, Text: text}, nil
}

// cursorIntField 取 varint 字段的 int 值（字段缺失时为 0）。
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
// 对外/内部的区分依据：ToolCall 的 mcp_tool_call 分支（f15）才带客户端要执行的
// call_id；不带 f15 的是内部元工具（如 get_mcp_tools），只计入同批完整性判断。
func (p *cursorStreamParser) noteToolCallStarted(b []byte) error {
	fields, err := cursorDecodeProtobuf(b)
	if err != nil {
		return err
	}
	delegations := p.toolDelegations()
	toolCall, ok := cursorFindField(fields, cursorFldToolCallStartedToolCall)
	if !ok {
		return delegations.noteToolCallStarted("", "", false)
	}
	inner, err := cursorDecodeProtobuf(toolCall.Bytes)
	if err != nil {
		return err
	}
	callID := ""
	if id, ok := cursorFindField(inner, cursorFldToolCallID); ok {
		callID = strings.TrimSpace(id.String())
	}
	return delegations.noteToolCallStarted("", callID, callID != "")
}

// noteToolRequestsListed 解析 InteractionUpdate.f27 tool_requests_listed 的 call_count
// 并交给批次记录归属（§25.3）。
//
// 字段号依据 design-evidence.md：ToolRequestsListedUpdate 内层**只有** f1 call_count
// （uint32），**没有** model_call_id 字段；上游帧只携带这一个数。故本层只把
// (call_count) 上抛，归属由 `cursorToolDelegations.noteToolRequestsListed` 按
// "当前开放批次上下文"判定：恰好一个开放批 → 归属；≥2 个 → ErrCursorBatchAmbiguous。
//
// 纪律：call_count 缺失或线型不是 varint 即明确协议错误（失败关闭），不猜数值、
// 不退回"只登记出现"的降级路径；本层不打印任何工具参数（脱敏）。
func (p *cursorStreamParser) noteToolRequestsListed(b []byte) error {
	fields, err := cursorDecodeProtobuf(b)
	if err != nil {
		return err
	}
	f, ok := cursorFindField(fields, cursorFldToolRequestsListedCallCount)
	if !ok {
		return fmt.Errorf("%w: tool_requests_listed without call_count", ErrCursorProtocol)
	}
	if f.Wire != cursorWireVarint {
		return fmt.Errorf("%w: tool_requests_listed.call_count has wrong wire type", ErrCursorProtocol)
	}
	if f.Varint > math.MaxUint32 {
		return fmt.Errorf("%w: tool_requests_listed.call_count overflows uint32", ErrCursorProtocol)
	}
	// listed 不带 ID（证据：无 model_call_id 字段）：以空归属上抛，由批次记录按
	// "恰好一个开放批"规则归属。
	return p.toolDelegations().noteToolRequestsListed("", int(f.Varint))
}

// cursorDecodeMcpArgs 解析 exec 内层 f11（mcp_args）的 McpArgs 消息。
//
// 字段号（派发单 §2）：f3 tool_call_id、f5 tool_name、f2 args。
// args 是 protobuf map<string, Value>：在 McpArgs 内编码为 repeated 字段 2，每条
// f2 字段**本身就是一条 map entry**（f1 key(string) 与 f2 value(Value 消息) 直内联，
// 不是"f2 的列表"）。上游实拍字节 `0a0463697479 12091a074265696a696e67` 即是单条
// entry：f1="city"、f2=Value{f3:"Beijing"}。
// Value oneof（派发单）：f1 null / f2 number / f3 string；未知类型失败关闭。
// 返回 (tool_call_id, tool_name, argsJSON, error)。
func cursorDecodeMcpArgs(payload []byte) (string, string, string, error) {
	fields, err := cursorDecodeProtobuf(payload)
	if err != nil {
		return "", "", "", err
	}
	idF, _ := cursorFindField(fields, 3)
	nameF, _ := cursorFindField(fields, 5)
	// 顶层全部 f2 字段，每条即一条 map entry（cursorFindField 只取最后一条，
	// 这里必须逐条取——map<string,Value> 在 protobuf 里是 repeated 的 entry）。
	obj := make(map[string]json.RawMessage)
	for _, f := range fields {
		if f.Number != 2 {
			continue
		}
		entryInner, e := cursorDecodeProtobuf(f.Bytes)
		if e != nil {
			return "", "", "", e
		}
		keyF, ok := cursorFindField(entryInner, 1)
		if !ok {
			continue
		}
		valF, ok := cursorFindField(entryInner, 2)
		if !ok {
			continue
		}
		v, e := cursorDecodeMcpValue(valF.Bytes)
		if e != nil {
			return "", "", "", e
		}
		obj[keyF.String()] = json.RawMessage(v)
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return "", "", "", fmt.Errorf("%w: marshal mcp args: %v", ErrCursorProtocol, err)
	}
	return idF.String(), nameF.String(), string(b), nil
}

// cursorDecodeMcpValue 解析 McpArgs args 的单个 Value（局部解码，支持 null / number
// (整数或浮点) / string；未知 wire type 失败关闭）。与全局 cursorDecodeProtobuf 不同，
// 这里额外支持 double/float 的定长 wire（Value.f2 number 可能用 fixed64）。
func cursorDecodeMcpValue(b []byte) (string, error) {
	i := 0
	for i < len(b) {
		key, n, err := cursorConsumeVarint(b, i)
		if err != nil {
			return "", err
		}
		i = n
		field := int(key >> 3)
		wire := int(key & 0x7)
		switch wire {
		case cursorWireVarint:
			v, n, err := cursorConsumeVarint(b, i)
			if err != nil {
				return "", err
			}
			i = n
			if field == 2 {
				return strconv.FormatInt(int64(v), 10), nil
			}
		case cursorWireBytes:
			ln, n, err := cursorConsumeVarint(b, i)
			if err != nil {
				return "", err
			}
			i = n
			if ln > uint64(len(b)-i) {
				return "", fmt.Errorf("%w: mcp value overflow", ErrCursorProtocol)
			}
			data := b[i : i+int(ln)]
			i += int(ln)
			if field == 3 {
				return cursorJSONString(string(data)), nil
			}
		case 5: // fixed64（double）
			if field == 2 && i+8 <= len(b) {
				bits := binary.LittleEndian.Uint64(b[i : i+8])
				i += 8
				return strconv.FormatFloat(math.Float64frombits(bits), 'g', -1, 64), nil
			}
			i += 8
		case 1: // fixed32（float）
			if field == 2 && i+4 <= len(b) {
				bits := binary.LittleEndian.Uint32(b[i : i+4])
				i += 4
				return strconv.FormatFloat(float64(math.Float32frombits(bits)), 'g', -1, 32), nil
			}
			i += 4
		default:
			return "", fmt.Errorf("%w: unsupported mcp value wire %d", ErrCursorProtocol, wire)
		}
	}
	// 无明确值字段（f1 null 或未设置）→ JSON null。
	return "null", nil
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
		return "", "", fmt.Errorf("%w: EndStream payload is not JSON: %v", ErrCursorProtocol, err)
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

// cursorSSEToolCallNameChunk 返回 tool_calls 首个 delta（含 id/type/function.name）。
func (p *cursorStreamParser) cursorSSEToolCallNameChunk(index int, tc cursorToolCallCapture) string {
	return "data: " + `{"id":"cursor-chat","object":"chat.completion.chunk","created":0,` +
		`"model":` + cursorJSONString(p.model) + `,` +
		`"choices":[{"index":0,"delta":{"tool_calls":[{"index":` + strconv.Itoa(index) +
		`,"id":` + cursorJSONString(tc.ID) + `,"type":"function","function":{"name":` +
		cursorJSONString(tc.Name) + `,"arguments":""}}]},"finish_reason":null}]}` + "\n\n"
}

// cursorSSEToolCallArgsChunk 返回 tool_calls 的 arguments delta（一块完整 arguments）。
func (p *cursorStreamParser) cursorSSEToolCallArgsChunk(index int, tc cursorToolCallCapture) string {
	args := tc.ArgsJSON
	if strings.TrimSpace(args) == "" || args == "null" {
		args = "{}"
	}
	return "data: " + `{"id":"cursor-chat","object":"chat.completion.chunk","created":0,` +
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
	return "data: " + `{"id":"cursor-chat","object":"chat.completion.chunk","created":0,` +
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
	return `{"id":"cursor-chat","object":"chat.completion.chunk","created":0,` +
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
	calls := p.capturedToolCalls()
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
			msg += `{"id":` + cursorJSONString(tc.ID) +
				`,"type":"function","function":{"name":` + cursorJSONString(tc.Name) +
				`,"arguments":` + cursorJSONString(args) + `}}`
		}
		msg += `]`
	} else {
		msg = `"role":"assistant","content":` + cursorJSONString(p.content)
	}
	out := `{"id":"cursor-chat","object":"chat.completion","created":0,` +
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
	return p.cursorBuildNonStreamingCompletion(), nil
}
