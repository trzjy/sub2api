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
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// cursorSSEStream converts frames as they arrive; it never buffers the complete
// model reply before exposing text. The AgentRun transport handles context/KV.
type cursorSSEStream struct {
	source io.ReadCloser
	parser *cursorStreamParser
	buffer bytes.Buffer
	done   bool
	errMu  sync.Mutex
	err    error
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
			s.buffer.WriteString(s.parser.cursorSSETextChunk(ev.Text))
		}
		s.done = s.parser.usage.Seen
	}
	if s.done {
		if !s.parser.started {
			s.buffer.WriteString(s.parser.cursorSSERoleChunk())
			s.parser.started = true
		}
		s.buffer.WriteString(s.parser.cursorSSEFinishChunk())
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
}

// cursorContent 返回拼出的完整响应文本（非流式入站用）。
func (p *cursorStreamParser) cursorContent() string { return p.content }

// cursorNewStreamParser 构造解析器（model 为空时 chunk 内 model 字段留空）。
//
// secrets 是需要从错误文案里抹掉的凭证值（如本次对话用的 session_token）。
func cursorNewStreamParser(model string, secrets ...string) *cursorStreamParser {
	return &cursorStreamParser{model: model, secrets: secrets}
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
	sseLines = append(sseLines, p.cursorSSEFinishChunk())
	return sseLines, nil
}

// cursorParseMessageFrame 解析一帧 agent.v1.AgentServerMessage。
func (p *cursorStreamParser) cursorParseMessageFrame(payload []byte) (cursorStreamEvent, error) {
	fields, err := cursorDecodeProtobuf(payload)
	if err != nil {
		return cursorStreamEvent{}, err
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

// cursorSSEFinishChunk 返回收尾 chunk（**带真实 usage**）。
//
// usage 只可能来自 turn_ended：收到就按实测值写出（input/output/cache_read/
// cache_write），没收到就写**显式零值**。语义是"本层没有 token 数据、计 0"，
// 而不是"测得 0 个 token"——**绝不估算、绝不伪造**（伪造会直接污染 usage_logs
// 的按 token 计费口径）。
func (p *cursorStreamParser) cursorSSEFinishChunk() string {
	// usage 在 **chunk 顶层**（不是 choices[0] 内）：既有 usage 提取链
	// （parseSSEUsageBytesWithType → extractOpenAIUsageFromJSONBytes）只认顶层的
	// `usage` 路径，塞进 choices 里会被整条丢掉、计费拿不到真实用量。
	return "data: " + `{"id":"cursor-chat","object":"chat.completion.chunk","created":0,` +
		`"model":` + cursorJSONString(p.model) +
		`,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],` +
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
func (p *cursorStreamParser) cursorBuildNonStreamingCompletion() []byte {
	out := `{"id":"cursor-chat","object":"chat.completion","created":0,` +
		`"model":` + cursorJSONString(p.model) + `,` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":` + cursorJSONString(p.content) + `},` +
		`"finish_reason":"stop"}],` +
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
