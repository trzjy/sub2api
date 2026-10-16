package service

// 本文件实现 **Cursor 原生响应帧 → OpenAI Chat Completions SSE** 的转换
// （E6 卡）。取证来源：
//   - 帧格式与 EndStream 语义：03-response-frames.md（客户端 @14763878–14765200 / @14784495）
//   - oneof 五分支与字段号：11-schema-StreamUnifiedChatResponseWithTools.txt
//   - 活体：07/08（错误帧实测；**成功帧至今 0 帧**，见下）
//
// ⚠️ **框架差异（已被活体证伪的坑）**：对话响应**带** envelope
// （`Content-Type: application/connect+proto`），而模型列表响应是**裸 protobuf**
// （`Content-Type: application/proto`）——**两者不可共用解析器**（09-live-models.md §1）。
// 本文件**只**处理对话流的 envelope 形态。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// StreamUnifiedChatResponseWithTools 的 oneof 字段号（03-response-frames.md §3）。
const (
	cursorFldClientSideToolV2Call      = 1 // ClientSideToolV2Call —— **首版丢弃**
	cursorFldStreamUnifiedChatResponse = 2 // StreamUnifiedChatResponse —— 增量文本落点
	cursorFldConversationSummary       = 3 // ConversationSummary
	cursorFldUserRules                 = 4 // UserRules
	cursorFldStreamStart               = 5 // StreamStart（开场帧，仅用于 TTFT 计时）
)

// StreamUnifiedChatResponse 内：
const (
	cursorFldRespText             = 1  // string，增量文本（**增量还是全量未实测**，见下）
	cursorFldRespIntermediateTxt  = 7  // string [opt]，思考/中间文本
	cursorFldRespShouldBreakAIMsg = 14 // bool [opt]
)

// cursorTextIsDeltaAssumed 标注 `.text` 的**未验证语义假设**。
//
// ════════════════════════════════════════════════════════════════════════════
// **这是一个假设，不是既成事实。** 截至本卡实现时，`.text` 的真实语义
// （增量片段 vs 累积全量）**至今 0 帧实测**——B-13 用户级鉴权未打通前，
// 上游只返回错误帧，从未产出过成功帧（见
// ~/.sub2api-acceptance/cursor-20261016-protocol/08-live-u2-frames.md §3）。
//
// 取"增量"的理由（仅为倾向性依据，不构成证明）：
//   - 字段名是 `stream_unified_chat_response`（"流式响应"）且协议为流式；
//   - 另有独立的 `intermediate_text`(f7) 与 `thinking`(f25) 字段承担"中间态"语义，
//     若 `text` 本身是全量重发则这些字段的设计冗余；
//   - 全量重发在长回答上带宽开销与 O(n²) 传输，官方客户端不会这样设计。
//
// **若实测证明是全量，此常量必须改为 false**——改错方向会输出整段重复文本。
// 判定方法见 08-live-u2-frames.md §4：连续取若干帧的 `.text`，若后帧恒为前帧前缀
// 则为全量（`content = text`），否则为增量（`content += text`）。
// ════════════════════════════════════════════════════════════════════════════
const cursorTextIsDeltaAssumed = true

// Cursor 上游错误（业务级与传输级）。
var (
	// ErrCursorUpstreamStream 表示 Cursor 流本身报告了终态错误（EndStream.error）。
	ErrCursorUpstreamStream = errors.New("cursor: upstream stream reported an error")
)

// cursorConnectContentType 是 Connect RPC 二进制 protobuf 的请求内容类型。
//
// ⚠️ 与 Unary 端点的 `application/proto`（**裸 protobuf、无 envelope**）不同——
// 两者不可混用（09-live-models.md §1 活体证伪）。
const cursorConnectContentType = "application/connect+proto"

// cursorUnaryContentType 是 Cursor **Unary** 端点的请求内容类型。
//
// Unary 端点（如 AuthService.GetSessionToken、模型列表）请求体走**裸 protobuf**，
// 无 5 字节 envelope，须用 `application/proto`；streaming 端点（如
// StreamUnifiedChatWithTools）才有 Connect envelope，须用
// `cursorConnectContentType`。两者不可混用——生产实测 Unary 端点收到
// `application/connect+proto` 的裸 protobuf 体直接 415。
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
	// cursorEventStart 是 stream_start 开场帧：产出 `role: assistant` 首包，
	// 使 OpenAI 客户端能正确初始化（否则部分客户端会等到首字才建流）。
	cursorEventStart = "start"
	// cursorEventSkip 是首版范围外的帧（工具调用 / 会话摘要 / 用户规则 /
	// thinking 帧）：**丢弃但不算错误**。
	cursorEventSkip = "skip"
)

// cursorStreamParser 逐帧解析 Cursor envelope 流并产出 OpenAI SSE chunk。
//
// 状态：累计文本（供增量累加/全量覆盖两种假设共用）+ 首包是否已发 + EndStream 是否已收。
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
	// content 是按 cursorTextIsDeltaAssumed 累加/覆盖后的当前完整文本。
	content string
	// sawText 表示是否收到过至少一个文本帧（用于判定"空响应"）。
	sawText bool
}

// cursorContent 返回按当前语义假设拼出的完整响应文本（非流式入站用）。
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
//   - 流结束但缺 EndStream → ErrCursorMissingEndStream（客户端同样视为协议错误）
//   - EndStream 带 error → ErrCursorUpstreamStream（携带**已脱敏**的 code/message）
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
		switch ev.Kind {
		case cursorEventStart:
			if !p.started {
				p.started = true
				sseLines = append(sseLines, p.cursorSSERoleChunk())
			}
		case cursorEventText:
			if !p.started {
				p.started = true
				sseLines = append(sseLines, p.cursorSSERoleChunk())
			}
			if cursorTextIsDeltaAssumed {
				p.content += ev.Text
			} else {
				p.content = ev.Text
			}
			p.sawText = true
			sseLines = append(sseLines, p.cursorSSETextChunk(ev.Text))
		case cursorEventSkip:
			// 首版范围外：丢弃（用户裁定），不算协议错误。
		}
	}

	if !p.endStreamSeen {
		return nil, ErrCursorMissingEndStream
	}
	if !p.started {
		// 只有 stream_start 或全 skip 帧：仍要发出 role 包，让客户端拿到合法空流。
		sseLines = append(sseLines, p.cursorSSERoleChunk())
		p.started = true
	}
	sseLines = append(sseLines, p.cursorSSEFinishChunk())
	return sseLines, nil
}

// cursorParseMessageFrame 解析一帧 StreamUnifiedChatResponseWithTools。
func (p *cursorStreamParser) cursorParseMessageFrame(payload []byte) (cursorStreamEvent, error) {
	fields, err := cursorDecodeProtobuf(payload)
	if err != nil {
		return cursorStreamEvent{}, err
	}
	// oneof：按"哪个字段出现"判别帧类型（03 §3）。顺序固定：工具调用优先于文本，
	// 与官方客户端的 oneof 语义一致（同帧出现属上游异常，取靠前分支并继续）。
	if f, ok := cursorFindField(fields, cursorFldClientSideToolV2Call); ok {
		_ = f
		// 首版丢弃 client_side_tool_v2_call（B-2(b)）。
		return cursorStreamEvent{Kind: cursorEventSkip}, nil
	}
	if f, ok := cursorFindField(fields, cursorFldConversationSummary); ok {
		_ = f
		return cursorStreamEvent{Kind: cursorEventSkip}, nil
	}
	if f, ok := cursorFindField(fields, cursorFldUserRules); ok {
		_ = f
		return cursorStreamEvent{Kind: cursorEventSkip}, nil
	}
	if _, ok := cursorFindField(fields, cursorFldStreamStart); ok {
		return cursorStreamEvent{Kind: cursorEventStart}, nil
	}
	if f, ok := cursorFindField(fields, cursorFldStreamUnifiedChatResponse); ok {
		inner, derr := cursorDecodeProtobuf(f.Bytes)
		if derr != nil {
			return cursorStreamEvent{}, derr
		}
		text := ""
		if tf, ok := cursorFindField(inner, cursorFldRespText); ok && tf.Wire == cursorWireBytes {
			text = tf.String()
		}
		// intermediate_text / thinking 属"思考中"内容，首版不透出
		// （无 OpenAI 侧的稳定落点，且与 reasoning_content 语义混淆风险大）。
		return cursorStreamEvent{Kind: cursorEventText, Text: text}, nil
	}
	// 只有 tracing_context / event_id 的帧：无内容，跳过。
	return cursorStreamEvent{Kind: cursorEventSkip}, nil
}

// cursorParseEndStream 解析 EndStream 帧 payload。
//
// EndStream 的 payload 是 **JSON**（不是 protobuf，03 §2）。无 error 时返回两个空串。
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
	// 业务码更有诊断价值——转换层要靠它区分「模型名错」与「鉴权错」
	// （09-live-models.md §4）。
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

// cursorSSEFinishChunk 返回收尾 chunk。
//
// ⚠️ **不带 usage**：Cursor 的 EndStream 未提供可解析的 token 计数，OpenAI 兼容
// 客户端允许缺省 usage。**严禁伪造 usage**——伪造会污染计费（usage_logs 按
// token 计费），比缺字段危险得多。
func (p *cursorStreamParser) cursorSSEFinishChunk() string {
	return "data: " + p.cursorChunk(`"delta":{},"finish_reason":"stop"`) + "\n\n"
}

// cursorChunk 渲染一条 chat.completion.chunk 的 JSON 本体（不含 `data: ` 前缀）。
func (p *cursorStreamParser) cursorChunk(choices string) string {
	return `{"id":"cursor-chat","object":"chat.completion.chunk","created":0,` +
		`"model":` + cursorJSONString(p.model) + `,"choices":[{"index":0,` + choices + `}]}`
}

// cursorSSEDoneSentinel 是 OpenAI 的流终止哨兵。
//
// Cursor 侧**没有**这个哨兵（结束由 EndStream 帧表达，03-response-frames.md §4），
// 但 OpenAI 兼容客户端普遍以它判定流结束，故由转换层补上——这是**协议适配**，
// 不是伪造业务数据（与"禁止伪造 usage"是两条不同的红线）。
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
// 关于 usage：Cursor 的 EndStream 未提供可解析的 token 计数，**严禁编造**。
// 此处发的是**显式零值**——语义是"本层没有 token 数据、计 0"，而不是
// "测得 0 个 token"。二者都是 0，区别只在人的解读，故写明如下：
//   - 比**编一个非零数**安全得多（后者会直接污染 usage_logs 的计费口径）；
//   - 比**整个省略 usage 字段**可行：通用非流式路径要求 usage 是对象，
//     缺失会以 `parse response: invalid json response` 拒绝整条响应
//     （extractOpenAIUsageFromJSONBytes 要求 value.IsObject()）。
//
// ⚠️ 由此产生的已知后果：cursor 账号的 usage_logs 记 0 token，按 token 计费时
// 该账号成本显示为 0。补齐真实 token 数需要 Cursor 侧提供计数（未取证），
// 在此之前**不得**用估算值填充。
func (p *cursorStreamParser) cursorBuildNonStreamingCompletion() []byte {
	out := `{"id":"cursor-chat","object":"chat.completion","created":0,` +
		`"model":` + cursorJSONString(p.model) + `,` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":` + cursorJSONString(p.content) + `},` +
		`"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`
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
func cursorParseStreamToCompletion(body []byte, model string, secrets ...string) ([]byte, error) {
	p := cursorNewStreamParser(model, secrets...)
	if _, err := p.cursorParseStream(body); err != nil {
		return nil, err
	}
	return p.cursorBuildNonStreamingCompletion(), nil
}
