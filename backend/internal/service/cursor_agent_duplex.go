package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Client 3.24.12 L_v/P_v and live Standard probes 01-version-checksum-only
// establish these headers. The machine identity stays scoped to the selected key.
const cursorClientVersion = "3.24.12"

// ---------------------------------------------------------------------------
// 四层 MCP 工具结果回写编码（依据 plan §21.1-4 / §25.3；本单 B4 修正内层字段号）。
//
// 真实结果回到原 AgentRun 的 exec 关联（W2-F CursorSessionIntake 生产实现），字节级形状：
//
//	ExecClientMessage{f1 id(exec_id, varint), f15 id(exec_id 原值, bytes), f11 = McpResult}
//	  McpResult{f1 success = McpSuccess | f2 error = McpError}
//	    McpSuccess / McpError{f1 content = repeated McpToolResultContentItem, f2 structured_content}
//	      McpToolResultContentItem{f1 text = McpTextContent}
//	        McpTextContent{f1 text(string)}
//
// 字段号双轨、绝对不可混用（本单 B4 证据）：
//   - cursorFldClientMessage (=2) 是**信封**里 ExecClientMessage 的消息类型字段
//     （与 context_response / mcp_state 回包一致，见 respond 中 d.send(2, ...)）；
//     本层以 it.duplex.send(cursorFldClientMessage, frame) 包裹 ExecClientMessage，外层恒为 2。
//   - cursorFldClientMsgMcpResult (=11) 才是 ExecClientMessage **内层**承载 McpResult 的字段号，
//     与 ExecServerMessage.f11 mcp_args 对称。生产实证（group 34 / claude-sonnet-5-5）：
//     内层若误用 2，上游不认该回包，模型会一直等待工具结果；
//     用 11 时，重构前实现（5e2dd5d08 cursorMcpArgsReply：f11 McpResult）与活体探针
//     （~/.sub2api-acceptance/sub2api-cursor-targeted-20261024/02-native-tools.cjs：fld(11,mcpResult)）
//     均能让上游在同一连接继续并正常 turn_ended。故内层 McpResult 必须为 11，绝非 2。
//
// McpResult.f1 成功 / f2 错误：客户端明确 is_error 的结果走 f2 保留错误语义，绝不把本机失败
// 写成成功。structured_content 用实际类型（f2）。
const (
	// cursorFldClientMessage 是**信封**里 ExecClientMessage 的消息类型字段（=2）。
	// 仅用于 it.duplex.send 包裹外层；不得用于 ExecClientMessage 内层承载 McpResult（见 B4）。
	cursorFldClientMessage = 2

	// cursorFldClientMsgMcpResult 是 ExecClientMessage **内层**承载 McpResult 的字段号（=11）。
	// 与 ExecServerMessage.f11 mcp_args 对称。本单 B4 修正：此前误用 cursorFldClientMessage(=2)
	// 作内层，导致上游不认回包、模型一直等待结果；证据：重构前实现与活体探针均用 11。
	cursorFldClientMsgMcpResult = 11

	// ExecClientMessage 字段。
	cursorFldClientMsgID     = 1  // f1 id（exec_id，varint）
	cursorFldClientMsgExecID = 15 // f15 id（exec_id 原值，bytes）

	// McpResult 字段。
	cursorFldMcpResultSuccess = 1 // f1 success
	cursorFldMcpResultError   = 2 // f2 error（is_error 语义）

	// McpSuccess / McpError 字段。
	cursorFldMcpSuccessContent           = 1 // f1 content（repeated McpToolResultContentItem）
	cursorFldMcpSuccessStructuredContent = 2 // f2 structured_content（实际结构化 JSON 类型）

	// McpToolResultContentItem 字段（oneof content）：
	//   f1 text  = McpTextContent
	//   f2 image = McpImageContent
	// 取证：Cursor 客户端产物 workbench.glass.main.js 内
	// `makeMessageType("agent.v1.McpToolResultContentItem", () => [
	//    {no:1,name:"text",kind:"message",T:McpTextContent,oneof:"content"},
	//    {no:2,name:"image",kind:"message",T:McpImageContent,oneof:"content"}])`。
	cursorFldContentItemText  = 1 // f1 text（McpTextContent）
	cursorFldContentItemImage = 2 // f2 image（McpImageContent）

	// McpTextContent 字段。
	cursorFldTextContentText = 1 // f1 text（string）

	// McpImageContent 字段（同一产物取证）：
	// `makeMessageType("agent.v1.McpImageContent", () => [
	//    {no:1,name:"data",kind:"scalar",T:12},      // bytes
	//    {no:2,name:"mime_type",kind:"scalar",T:9}]) // string
	cursorFldImageContentData     = 1 // f1 data（bytes，原始图片字节）
	cursorFldImageContentMimeType = 2 // f2 mime_type（string）

	// cursorFldClientControlMessage 是信封 AgentClientMessage.f5（exec_client_control_message）。
	cursorFldClientControlMessage = 5
	// cursorFldClientInteractionResponse 是信封 AgentClientMessage.f6 InteractionResponse。
	cursorFldClientInteractionResponse = 6
	// ExecClientControlMessage.f1 = stream_close（f2 throw / f3 heartbeat）。
	cursorFldControlStreamClose = 1
	// ExecClientStreamClose.f1 = id（uint32，varint）。
	cursorFldStreamCloseID = 1
)

// cursorEncodeMcpToolResult 把一条真实工具结果编码为 ExecClientMessage 字节。
//
// callID 仅用于日志/诊断（上游靠 ExecID + IDField 关联 exec 关联）；execID / idField 必须
// 是原 mcp_args 委托捕获时带回的原值，否则上游无法把结果关联到对应 exec。is_error 走
// McpResult.f2（错误语义），结构化 JSON 走 McpSuccess.f2（structured_content）。
// images 是本次工具结果携带的图片（如客户端工具读到图片文件）：每张图独立成一条
// McpSuccess.f1 内容项，走 McpToolResultContentItem.f2（McpImageContent{f1 data bytes,
// f2 mime_type}），排在全部文本项之后——与历史侧「先文本后图片」的顺序同一口径。
// 无图时字节形状与此前完全一致（零变化）。
func cursorEncodeMcpToolResult(
	execID uint64, idField []byte, callID string,
	texts []string, images []cursorTurnImage, structured []byte, isError bool,
) []byte {
	// 每条文本结果项独立成**一条** McpSuccess.f1（repeated McpToolResultContentItem）：
	// 每个 ContentItem 各自一个 f1 tag-value，**不得**把多条编码拼进同一个 length-delimited
	// 载荷（那样整段会被当作一个 ContentItem 解析，除最后一条外全部丢失）。
	var payload []byte
	if len(texts) == 0 && len(images) == 0 {
		// 空结果也保留一个空文本项，保证 ContentItem 形状完整（上游不接受缺 content）。
		textContent := cursorAppendStringField(nil, cursorFldTextContentText, "")
		item := cursorAppendBytesField(nil, cursorFldContentItemText, textContent)
		payload = cursorAppendBytesField(payload, cursorFldMcpSuccessContent, item)
	}
	for _, t := range texts {
		textContent := cursorAppendStringField(nil, cursorFldTextContentText, t)
		item := cursorAppendBytesField(nil, cursorFldContentItemText, textContent)
		payload = cursorAppendBytesField(payload, cursorFldMcpSuccessContent, item)
	}
	for _, img := range images {
		if len(img.data) == 0 {
			continue
		}
		var imageContent []byte
		imageContent = cursorAppendBytesField(imageContent, cursorFldImageContentData, img.data)
		if img.mimeType != "" {
			imageContent = cursorAppendStringField(imageContent, cursorFldImageContentMimeType, img.mimeType)
		}
		item := cursorAppendBytesField(nil, cursorFldContentItemImage, imageContent)
		payload = cursorAppendBytesField(payload, cursorFldMcpSuccessContent, item)
	}
	// McpSuccess / McpError{f1 content, f2 structured_content}。
	if len(structured) > 0 {
		payload = cursorAppendBytesField(payload, cursorFldMcpSuccessStructuredContent, structured)
	}
	// McpResult{f1 success} 或 {f2 error}。
	var result []byte
	if isError {
		result = cursorAppendBytesField(nil, cursorFldMcpResultError, payload)
	} else {
		result = cursorAppendBytesField(nil, cursorFldMcpResultSuccess, payload)
	}
	// ExecClientMessage{f1 id, f15 id(原值), f11 McpResult}：内层 McpResult 必须用 f11，
	// 绝非信封字段 cursorFldClientMessage(=2)（B4 修正；证据见常量块）。
	msg := cursorAppendVarintField(nil, cursorFldClientMsgID, execID)
	if len(idField) > 0 {
		msg = cursorAppendBytesField(msg, cursorFldClientMsgExecID, idField)
	}
	msg = cursorAppendBytesField(msg, cursorFldClientMsgMcpResult, result)
	return msg
}

// cursorEncodeExecStreamClose 把一个 exec 的终止控制帧编码为 ExecClientControlMessage 载荷。
// 真实客户端在每条 ExecServerMessage 的结果流写完后都会补发
// ExecClientControlMessage{f1 stream_close{f1 id=exec_id}}；缺该帧时上游认为 exec 仍未结束，
// 事件流（ShellStream）不会被收尾（McpResult 是单条自包含结果，故 mcp 路径不需要该帧）。
// 字节形：0x0a <len> { 0x08 <varint execID> }；外层由调用方以 AgentClientMessage.f5 包裹。
func cursorEncodeExecStreamClose(execID uint64) []byte {
	inner := cursorAppendVarintField(nil, cursorFldStreamCloseID, execID)
	return cursorAppendBytesField(nil, cursorFldControlStreamClose, inner)
}

func cursorClientChecksum(device string, now time.Time) string {
	stamp := uint32(now.UnixMilli() / 1_000_000)
	// JavaScript shifts use a 32-bit operand and mask the shift count to five bits.
	raw := []byte{byte(stamp >> 8), byte(stamp), byte(stamp >> 24), byte(stamp >> 16), byte(stamp >> 8), byte(stamp)}
	prev := byte(165)
	for i := range raw {
		raw[i] = (raw[i] ^ prev) + byte(i)
		prev = raw[i]
	}
	return fmt.Sprintf("%s%x", base64.StdEncoding.EncodeToString(raw), sha256.Sum256([]byte(device)))
}

// AgentRun is bidirectional: closing the request after run_request prevents the
// server from obtaining context and acknowledging its conversation blobs.
// Only the observed context and KV messages are implemented; tool execution fails.
type cursorAgentDuplex struct {
	reader *io.PipeReader
	writer *io.PipeWriter
	blobs  map[string][]byte
	bytes  int

	// §18.5 T 阶段日志：不可变标识在 Do 调用点捕获后写入，双工 goroutine 与
	// 读取 goroutine 只读取这些不可变字段，不跨 goroutine 读 gin.Context、不共享
	// 未加锁 map。lg 为请求级 logger（并发安全、不可变），直接落盘。
	lg    *zap.Logger
	reqID string
	model string
	// ctx 是请求级 context（不可变、并发安全，可跨 goroutine 只读 .Err()）。
	// 用于下游消费者中途断开时，在 Close / 读取错误分支判定 context_cancel，
	// 不记成功 usage。绝不通过它写 gin.Context 或共享可变状态。
	ctx context.Context

	// declaredToolDefs 是本请求通过 f4（McpTools）声明的每条 McpToolDefinition 的
	// **原始字节**（repeated 逐项原样保存，与请求侧 AgentRunRequest.f4 声明逐字节相同）。
	// 由 newCursorAgentDuplex 从 AgentRun 请求体 f4 解出，供 mcp_state 回包 f5 原样回传
	// （字节级保真，依据活体探针 T7：f5 内层即完整 McpToolDefinition 五字段，不可只回工具名）。
	//
	// 注意：卡 01 已经把 f4 里的工具名改成 wire name（撞 Cursor 内置名时加 "_"），所以
	// 这里的字节里是 wire name，不是客户端原名。
	declaredToolDefs [][]byte

	// declaredToolReverse 是「wire name → 客户端原名」的反查表，**只在本请求的声明集合内
	// 有效**（不同请求工具集不同，wire name 撞了也只在本请求内有意义）。由 newCursorAgentDuplex
	// 从本请求的**原始**工具名集合正向算 wire map（复用卡 01 的 cursorWireToolNamesFromNames）
	// 再反转得到。
	//
	// 用途（卡 03 方向 1）：上游按 wire name 回调工具时，cursorCaptureMcpArgs 据此把名字映射
	// 回客户端原名再交付——否则客户端会收到一个自己从没声明过的名字（卡 01 改名后，
	// Read → Read_，上游回调 Read_，客户端只认识 Read）。
	//
	// 反查唯一性：卡 01 的 taken（归一化键）保证两个不同的原名绝不会算出同一个 wire name，
	// 故反查不会多对一，是单射。即便极端出现多对一（正常不应发生），下面建表时取字典序
	// 最小的原名，保证结果确定。
	//
	// 反查为空（未注入原始名，例如独立解析路径 / 测试未注入）时，cursorCaptureMcpArgs
	// 保持原样（既有语义，不扩大改动）。
	declaredToolReverse map[string]string

	// delegations 是本轮工具委托的**唯一**记录（§24.2-6）：duplex 解码 mcp_args
	// 一次并写入这里，parser 只读取输出，绝不再各自保留一份列表。
	// 未注入时用本地记录（独立双工场景），保证任何路径都只有一个解码点。
	delegations *cursorToolDelegations

	// writeMu 串行化对原生 pipe writer 的所有写（读循环回包 + 跨 HTTP 真实结果回写
	// 共用同一连接，禁止并发写同一 pipe —— io.Pipe 不保证并发写安全）。
	writeMu sync.Mutex
}

// cursorWireNameReverse 由本请求的**原始**工具名集合正向算 wire map（复用卡 01 的
// cursorWireToolNamesFromNames）再反转为「wire name → 原名」反查表。
//
// 单射保证：卡 01 的 taken（归一化键）保证两个不同原名绝不会算出同一个归一化 wire 形态，
// 故 wire name 字符串本身也不重复，反查不会多对一。即便极端出现多对一（正常不应发生），
// 这里取字典序最小的原名，保证结果确定、与卡 01 去重口径一致。
//
// originals 为空时返回 nil（双工层据此退化为原样登记，保持既有语义）。
func cursorWireNameReverse(originals []string) map[string]string {
	if len(originals) == 0 {
		return nil
	}
	wireMap := cursorWireToolNamesFromNames(originals)
	reverse := make(map[string]string, len(wireMap))
	for orig, wire := range wireMap {
		if cur, ok := reverse[wire]; !ok || orig < cur {
			reverse[wire] = orig
		}
	}
	return reverse
}

// newCursorAgentDuplex 构造双工层。
func newCursorAgentDuplex(initial []byte, delegations *cursorToolDelegations) (*cursorAgentDuplex, error) {
	if delegations == nil {
		delegations = newLocalCursorToolDelegations()
	}
	frame, _, err := cursorReadEnvelopeFrame(initial, 0)
	if err != nil {
		return nil, err
	}
	fields, err := cursorDecodeProtobuf(frame.Data)
	if err != nil {
		return nil, err
	}
	run, ok := cursorFindField(fields, cursorFldAgentRunRequest)
	if !ok {
		return nil, cursorFrameErrorf(cursorFrameDuplex, "missing AgentRun request")
	}
	fields, err = cursorDecodeProtobuf(run.Bytes)
	if err != nil {
		return nil, err
	}
	d := &cursorAgentDuplex{blobs: make(map[string][]byte), delegations: delegations}
	// 从 AgentRun 请求体 f4（McpTools）解出本轮声明的每条 McpToolDefinition 的**原始字节**，
	// repeated 逐项原样保存，供 mcp_state 回包 f5 原样回传（字节级保真，依据活体探针 T7）。
	if mcpField, ok := cursorFindField(fields, cursorFldRunMcpTools); ok && len(mcpField.Bytes) > 0 {
		if defs, derr := cursorDecodeProtobuf(mcpField.Bytes); derr == nil {
			for _, def := range defs {
				if def.Number != cursorFldMcpToolsTool {
					continue
				}
				// 保留每条 McpToolDefinition 的原始字节（不重新解析、不改字段、不过滤）。
				// 零长条目（如 f4 为空 message）不计入，匹配"无声明工具则 f5 不追加"语义。
				if len(def.Bytes) == 0 {
					continue
				}
				raw := make([]byte, len(def.Bytes))
				copy(raw, def.Bytes)
				d.declaredToolDefs = append(d.declaredToolDefs, raw)
			}
		}
	}
	// The server may request history even when run_request already carries it as
	// pre_fetched_blobs. Seed the same store used for later KV set/get messages.
	for _, field := range fields {
		if field.Number != cursorFldRunPreFetchedBlobs {
			continue
		}
		blob, err := cursorDecodeProtobuf(field.Bytes)
		if err != nil {
			return nil, err
		}
		key, foundKey := cursorFindField(blob, cursorFldBlobID)
		value, foundValue := cursorFindField(blob, cursorFldBlobValue)
		if !foundKey || !foundValue {
			return nil, cursorFrameErrorf(cursorFrameDuplex, "malformed pre-fetched blob")
		}
		d.bytes += len(key.Bytes) + len(value.Bytes)
		if d.bytes > cursorUpstreamStreamMaxBytes {
			return nil, cursorFrameErrorf(cursorFrameDuplex, "KV storage exceeds limit")
		}
		d.blobs[string(key.Bytes)] = value.Bytes
	}
	r, w := io.Pipe()
	d.reader, d.writer = r, w
	return d, nil
}

// declaredToolNames 从 declaredToolDefs 提取每条工具定义的工具名，经 declaredToolReverse
// 反查回**客户端原名**后返回，用于拒绝消息和日志。
//
// 为什么必须反查：卡 01 之后 f4 里存的是 wire name（撞 Cursor 内置名时加 "_"，如 Read → Read_），
// 但客户端只认识自己声明的原名。拒绝文本点名要点的就是客户端认识的名字，日志同理——
// 否则运维看到 `Read_` 会误以为客户端声明了这个名字。
//
// 查不到反查表（nil / 该名未登记，例如独立解析路径没有原始名来源，或上游回调了未声明的名字）
// → 保持原样（wire name），既有语义不变、不报错。反查是单射（卡 03 保证），不会错配。
//
// 返回按声明顺序排列的工具名切片；解析失败的条目跳过（不中断提取）。
func (d *cursorAgentDuplex) declaredToolNames() []string {
	if len(d.declaredToolDefs) == 0 {
		return nil
	}
	names := make([]string, 0, len(d.declaredToolDefs))
	for _, raw := range d.declaredToolDefs {
		fields, err := cursorDecodeProtobuf(raw)
		if err != nil {
			continue
		}
		for _, f := range fields {
			if f.Number == cursorFldMcpToolDefToolName {
				name := f.String()
				if orig, ok := d.declaredToolReverse[name]; ok {
					name = orig
				}
				names = append(names, name)
				break
			}
		}
	}
	return names
}

func (d *cursorAgentDuplex) start(initial []byte) {
	go func() {
		if _, err := d.writer.Write(initial); err != nil {
			d.close(err)
		}
	}()
}

func (d *cursorAgentDuplex) close(err error) {
	// §18.5 T 终态事件：正常完成记 close(ok)；上游/上下文错误记 close(错误安全分类)。
	if err == nil {
		cursorPhaseLogFields(d.lg, zapcore.InfoLevel, "close", d.model, d.reqID,
			zap.String("outcome", "ok"))
	} else {
		cursorPhaseLogFields(d.lg, zapcore.WarnLevel, "close", d.model, d.reqID,
			zap.String("outcome", cursorHTTPErrClass(err)))
	}
	_ = d.writer.CloseWithError(err)
	_ = d.reader.CloseWithError(err)
}

// logPhase 是双工层阶段事件的统一落盘入口（logger 已在 Do 调用点捕获为不可变引用）。
func (d *cursorAgentDuplex) logPhase(lvl zapcore.Level, phase, outcome string, extra ...zap.Field) {
	cursorPhaseLogFields(d.lg, lvl, phase, d.model, d.reqID, extra...)
}

func (d *cursorAgentDuplex) responseBody(body io.ReadCloser) io.ReadCloser {
	return &cursorAgentResponseBody{source: body, duplex: d, start: time.Now()}
}

type cursorAgentResponseBody struct {
	source io.ReadCloser
	duplex *cursorAgentDuplex
	frame  *bytes.Reader
	done   bool
	bytes  int
	// §18.5 T 阶段日志辅助字段：start 为响应体开始读取时间（首帧耗时基准），
	// firstSeen 标记是否已记过首帧事件。两者只被读取 goroutine 自身写入/读取。
	start     time.Time
	firstSeen bool
}

func (b *cursorAgentResponseBody) Close() error {
	// §18.5 T：下游消费者中途断开（请求 ctx 已取消）时记 context_cancel 终态，
	// 绝不记成功 usage（turn_ended / end_stream）。这与生产 handler 行为一致：
	// 下游断开后 handler 的 defer resp.Body.Close() 会关闭上游响应体，进而中断
	// 阻塞中的读取。ctx 为不可变引用，只读 .Err() 并发安全。
	if b.duplex.ctx != nil && b.duplex.ctx.Err() != nil {
		b.duplex.logPhase(zapcore.InfoLevel, "context_cancel", cursorHTTPErrClass(context.Canceled))
		_ = b.duplex.writer.CloseWithError(context.Canceled)
		_ = b.duplex.reader.CloseWithError(context.Canceled)
	} else {
		b.duplex.close(nil)
	}
	return b.source.Close()
}

func (b *cursorAgentResponseBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.frame != nil && b.frame.Len() > 0 {
		return b.frame.Read(p)
	}
	if b.done {
		return 0, io.EOF
	}
	header := make([]byte, cursorEnvelopeHeaderSize)
	if _, err := io.ReadFull(b.source, header); err != nil {
		// 读取失败：按错误来源归类终态事件（上下文取消 / 上游提前关闭 / 其它传输错误）。
		// 绝不回显原始错误文案。ctx 取消优先判定为 context_cancel（下游断开）。
		switch {
		case errors.Is(err, io.EOF):
			b.duplex.logPhase(zapcore.WarnLevel, "eof_incomplete", "incomplete")
		case b.duplex.ctx != nil && b.duplex.ctx.Err() != nil:
			b.duplex.logPhase(zapcore.InfoLevel, "context_cancel", cursorHTTPErrClass(err))
		case errors.Is(err, context.Canceled):
			b.duplex.logPhase(zapcore.InfoLevel, "context_cancel", cursorHTTPErrClass(err))
		default:
			b.duplex.logPhase(zapcore.WarnLevel, "read_error", cursorHTTPErrClass(err))
		}
		b.duplex.close(err)
		return 0, err
	}
	n := int(binary.BigEndian.Uint32(header[1:]))
	b.bytes += len(header) + n
	if b.bytes > cursorUpstreamStreamMaxBytes {
		return 0, cursorFrameErrorf(cursorFrameDuplex, "AgentRun response exceeds limit")
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(b.source, payload); err != nil {
		return 0, err
	}
	// 无法解码的帧：这是"读到一半终止"的最常见来源之一（上游新增/变更字段布局），
	// 此前该错误直接冒泡到读循环、在终态日志里只剩 terminal_reason=stream_error，
	// 无从判断是"帧解不开"还是"exec 不认"。这里先按安全类别落一条，再原样返回。
	frame, _, err := cursorReadEnvelopeFrame(append(header, payload...), 0)
	if err != nil {
		b.duplex.logPhase(zapcore.WarnLevel, "frame_decode_failed", cursorStreamErrClass(err),
			zap.String("error_class", cursorStreamErrClass(err)),
			zap.Int("payload_bytes", len(payload)))
		return 0, err
	}
	if frame.cursorIsEndStream() {
		// 正常终态：上游显式 EndStream（区分于提前 EOF 的不完整终止）。
		b.duplex.logPhase(zapcore.InfoLevel, "end_stream", "ok",
			zap.Int64("elapsed_ms", time.Since(b.start).Milliseconds()))
		b.done = true
	} else {
		isFirst := !b.firstSeen
		b.firstSeen = true
		b.done, err = b.duplex.respond(frame.Data)
		if err != nil {
			// 帧已解码但处理失败（exec 分支不认 / KV 缺失 / 子消息解不开 / 内部状态）。
			// 只落安全枚举 + 该帧的顶层字段号：字段号是协议结构，不含任何业务内容，
			// 而它正是区分"真未知执行类型"与"已实现分支的内部错误"的唯一依据。
			b.duplex.logPhase(zapcore.WarnLevel, "frame_handle_failed", cursorStreamErrClass(err),
				zap.String("error_class", cursorStreamErrClass(err)),
				zap.Ints("fields", frameTopFieldNumbers(payload)))
			return 0, err
		}
		if isFirst {
			// 首帧事件：发生在 TurnEnded 之前，证明模型输出已开始流动。
			b.duplex.logPhase(zapcore.InfoLevel, "first_frame", "ok",
				zap.Int64("elapsed_ms", time.Since(b.start).Milliseconds()))
		}
	}
	if b.done {
		b.duplex.close(nil)
		_ = b.source.Close()
	}
	b.frame = bytes.NewReader(append(header, payload...))
	return b.frame.Read(p)
}

// cursorUnknownFieldReplyShape 是 unknown_field_replied 日志的 shape 取值（施工图 W7）。
// 未知 exec 字段这条链路只有「兜底形状」一种：回 {field:{f2:{f2:text}}} 通用错误结果帧
// （cursorEncodeExecGenericUnknownFieldReject）。不存在「精确形状」分支——精确形状只属于 11 类
// 原生 exec 拒绝（cursor_native_exec_reject.go，由别的卡负责），与未知字段链路无关。本常量仅用于
// 让日志字段与测试断言引用同一字面量，避免拼写漂移，不引入任何新机制。
const cursorUnknownFieldReplyShape = "兜底形状"

func (d *cursorAgentDuplex) respond(payload []byte) (bool, error) {
	fields, err := cursorDecodeProtobuf(payload)
	if err != nil {
		return false, err
	}
	// 诊断（临时）：记录每条 AgentServerMessage 的顶层字段号，用于钉死"回写结果后上游发了
	// 什么导致 stream_error"。只记字段号，不记任何业务内容。
	if update, ok := cursorFindField(fields, cursorFldServerInteractionUpdate); ok {
		parts, err := cursorDecodeProtobuf(update.Bytes)
		if err != nil {
			return false, err
		}
		_, ended := cursorFindField(parts, cursorFldUpdateTurnEnded)
		if ended {
			// 正常终态：上游 InteractionUpdate 携带 TurnEnded（含真实 usage）。
			d.logPhase(zapcore.InfoLevel, "turn_ended", "ok")
		}
		return ended, nil
	}
	if exec, ok := cursorFindField(fields, cursorFldServerExecMessage); ok {
		parts, err := cursorDecodeProtobuf(exec.Bytes)
		if err != nil {
			return false, err
		}
		execID := cursorVarintField(parts, 1)
		var idField []byte
		if id, ok := cursorFindField(parts, 15); ok {
			idField = id.Bytes
		}
		// 本轮裁定派发顺序（严格）：f10 → f36 → f11 → 11 类原生拒绝 → 已证 metadata → fail-close。
		// 命中即处理，不再往下走。原生 exec 一律拒绝（不执行服务器工具、不按工具名配对、
		// 不 FS→shell、不做 shell fallback）；只允许客户端声明的 f11 mcp_args 执行。
		nativeField, hasNative := cursorNativeExecRejectField(parts)
		switch {
		case cursorFindFieldOK(parts, 10):
			// 现有 context 应答（f10 既有处理，但不再伪造服务器环境，见 respondContext）。
			return d.respondContext(parts, execID, idField)
		case cursorFindFieldOK(parts, 36):
			// mcp_state 委托（f36 mcp_state_exec_args）：回 McpStateExecResult，
			// 枚举本请求声明的工具（每个工具单独一条 f5，repeated 逐项编码）。
			resp, e := d.cursorMcpStateReply(execID, idField)
			if e != nil {
				return false, e
			}
			serr := d.send(2, resp)
			d.logPhase(zapcore.DebugLevel, "mcp_state_reply", cursorHTTPErrClass(serr),
				zap.Uint64("exec_id", execID), zap.Int("tools", len(d.declaredToolDefs)))
			return false, serr
		case cursorFindFieldOK(parts, 11):
			// mcp_args 委托（f11 mcp_args）：只捕获工具调用并登记入开放批次
			// （W2-A1 批次封口），**绝不回 McpResult.error 诱导模型结束**（§24.3 旧链已斩）。
			// 模型在同一双向连接上等待真实 McpResult；本层在续轮把真实结果写回原 exec
			// 关联（W2-F CursorSessionIntake 生产实现）。连接保持存活至真实终态 /
			// 显式终止，会话进入 waiting_tools。
			tc, e := d.cursorCaptureMcpArgs(parts, execID, idField)
			if e != nil {
				return false, e
			}
			d.logPhase(zapcore.DebugLevel, "mcp_args_captured", "",
				zap.Uint64("exec_id", execID), zap.String("tool_name", tc.Name))
			return false, nil
		case hasNative:
			// 11 类原生 exec 字段（f2 shell / f3 write / f4 delete / f5 grep / f7 read /
			// f8 ls / f9 diagnostics / f14 shell_stream / f16 background_shell_spawn /
			// f20 fetch / f23 write_shell_stdin）：按本轮裁定一律按协议拒绝形状回写，
			// 不执行服务器工具、不按工具名配对、不 FS→shell、不做 shell fallback。
			// 拒绝入口在 cursor_native_exec_reject.go（由别的卡负责接线与修正），本卡只调用。
			if err := d.cursorRejectNativeExec(parts, nativeField, execID, idField); err != nil {
				return false, err
			}
			return false, nil
		default:
			// 未处理的 exec 子字段：先按字段号取证，再区分"纯元数据"与"真未知执行"。
			//
			// 只记字段号（不记内容）：真实客户端（CodeBuddy / Claude Code / Codex）会发起
			// 我们尚未实现的 exec 类型，没有这个字段号就只能盲猜。安全字段 = exec 内层
			// 全部 field number 与 wire type，不含任何业务数据。
			nums := make([]int, 0, len(parts))
			for _, f := range parts {
				nums = append(nums, f.Number)
			}
			sort.Ints(nums)
			if cursorExecImplementedSubfieldOK(parts) {
				// 已实现的 exec 子字段本应被上方 switch 命中；落到这里说明分支顺序被破坏，
				// 属内部逻辑错误，保持失败关闭（绝不静默忽略真实执行请求）。
				d.logPhase(zapcore.ErrorLevel, "exec_dispatch_internal_error", "",
					zap.Uint64("exec_id", execID), zap.Ints("field_numbers", nums))
				return false, cursorFrameErrorf(cursorFrameDuplex, "AgentRun exec dispatch ordering broken")
			}
			if cursorExecMetadataOnly(parts) {
				// 纯元数据伴随消息（仅服务端方向已证字段 f1/f15/f19/f55/f57）：不承载执行指令，
				// 忽略即可，连接保持、不失败（生产 [1,15,19,55] / [1,15,57] 即此类）。
				d.logPhase(zapcore.DebugLevel, "exec_metadata_only", "",
					zap.Uint64("exec_id", execID), zap.Ints("field_numbers", nums))
				return false, nil
			}
		d.logPhase(zapcore.WarnLevel, "exec_unsupported_fields", "",
			zap.Uint64("exec_id", execID), zap.Ints("field_numbers", nums))
		// 派发单 D 阶段 2：未知 exec 字段不再以 502 打死整轮，而是对每个「承载 message 的
		// 字段」（wire=2/bytes）回一个通用错误结果帧 {field:{f2:{f2:text}}}，让模型换条路继续；
		// varint 字段（wire=0）不承载 message，不回包（施工图 W1：只有承载 message 的字段必须
		// 回包）；元数据白名单字段已在上方 cursorExecMetadataOnly 分支忽略。无论哪种都继续处理
		// 本轮，不 502、不断流。绝不使用 sendExecStreamClose（关流会让上游永远等不到应答，
		// 把「本轮 502」换成「流卡死」，更糟）；stream_close 只在确实完成流式执行时才发。
		for _, f := range parts {
			if cursorExecMetadataFieldOK(f.Number) {
				continue
			}
			if f.Wire != cursorWireBytes {
				// varint 字段不承载 message，不回包，仅忽略（施工图 W1）。
				continue
			}
			// 施工图 W7 unknown_field_replied：未知字段被应答的观测点（判断「客户跑没跑」）。
			// 只记字段号 / wire type / 形状种类，绝不记字段字节内容（可能含客户数据）。
			// 未知字段这一路恒为「兜底形状」——精确形状只属于 11 类原生 exec 拒绝
			// （cursor_native_exec_reject.go，由别的卡负责），不在此链路内，故不制造分叉。
			d.logPhase(zapcore.InfoLevel, "unknown_field_replied", "",
				zap.Int("field", f.Number),
				zap.Int("wire_type", f.Wire),
				zap.String("shape", cursorUnknownFieldReplyShape))
			frame := cursorEncodeExecGenericUnknownFieldReject(execID, idField, f.Number, cursorGenericUnknownExecRejectText)
			if err := d.send(cursorFldClientMessage, frame); err != nil {
				return false, err
			}
		}
		return false, nil
		}
	}
	if q, ok := cursorFindField(fields, cursorFldServerInteractionQuery); ok {
		return d.respondInteractionQuery(q.Bytes)
	}
	if kv, ok := cursorFindField(fields, cursorFldServerKvMessage); ok {
		parts, err := cursorDecodeProtobuf(kv.Bytes)
		if err != nil {
			return false, err
		}
		response := cursorAppendVarintField(nil, 1, cursorVarintField(parts, 1))
		if set, ok := cursorFindField(parts, 3); ok {
			values, err := cursorDecodeProtobuf(set.Bytes)
			if err != nil {
				return false, err
			}
			key, foundKey := cursorFindField(values, 1)
			value, foundValue := cursorFindField(values, 2)
			if !foundKey || !foundValue {
				return false, cursorFrameErrorf(cursorFrameDuplex, "malformed KV set")
			}
			// §18.5 T：只记 KV 操作种类（kv_set）与耗时，不记 key/value 明文。
			d.logPhase(zapcore.DebugLevel, "kv_write_before", "",
				zap.String("kind", "kv_set"))
			d.bytes += len(key.Bytes) + len(value.Bytes)
			if d.bytes > cursorUpstreamStreamMaxBytes {
				return false, cursorFrameErrorf(cursorFrameDuplex, "KV storage exceeds limit")
			}
			d.blobs[string(key.Bytes)] = value.Bytes
			response = cursorAppendBytesField(response, 3, nil)
			serr := d.send(3, response)
			d.logPhase(zapcore.DebugLevel, "kv_write_after", cursorHTTPErrClass(serr),
				zap.String("kind", "kv_set"))
			return false, serr
		} else if get, ok := cursorFindField(parts, 2); ok {
			values, err := cursorDecodeProtobuf(get.Bytes)
			if err != nil {
				return false, err
			}
			// §18.5 T：get 同样只记种类（kv_get），不记 key/value 明文。
			d.logPhase(zapcore.DebugLevel, "kv_write_before", "",
				zap.String("kind", "kv_get"))
			key, found := cursorFindField(values, 1)
			value, exists := d.blobs[string(key.Bytes)]
			if !found || !exists {
				return false, cursorFrameErrorf(cursorFrameDuplex, "AgentRun requested unknown KV blob")
			}
			response = cursorAppendBytesField(response, 2, cursorAppendBytesField(nil, 1, value))
			serr := d.send(3, response)
			d.logPhase(zapcore.DebugLevel, "kv_write_after", cursorHTTPErrClass(serr),
				zap.String("kind", "kv_get"))
			return false, serr
		} else {
			return false, cursorFrameErrorf(cursorFrameDuplex, "unsupported KV operation")
		}
	}
	return false, nil
}

func (d *cursorAgentDuplex) send(field int, payload []byte) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	_, err := d.writer.Write(cursorEncodeEnvelope(cursorAppendBytesField(nil, field, payload)))
	return err
}

// agent.v1.InteractionQuery / InteractionResponse 字段号（证据 39b，Cursor 官方客户端 bundle）。
// 询问 oneof query 与应答 oneof result 同号。
const (
	cursorFldInteractionID = 1 // InteractionQuery.f1 / InteractionResponse.f1 id（uint32）

	cursorFldInteractionWebSearch    = 2  // f2 web_search_request_query / web_search_request_response
	cursorFldInteractionAskQuestion  = 3  // f3 ask_question_interaction_query / ask_question_interaction_response
	cursorFldInteractionSwitchMode   = 4  // f4 switch_mode_request_query / switch_mode_request_response
	cursorFldInteractionCreatePlan   = 7  // f7 create_plan_request_query / create_plan_request_response
	cursorFldInteractionSetupVM      = 8  // f8 setup_vm_environment_args / setup_vm_environment_result
	cursorFldInteractionWebFetch     = 9  // f9 web_fetch_request_query / web_fetch_request_response
	cursorFldInteractionPrManagement = 10 // f10 pr_management_request_query / pr_management_result
	cursorFldInteractionMcpAuth      = 11 // f11 mcp_auth_request_query / mcp_auth_request_response
	cursorFldInteractionReplaceEnv   = 13 // f13 replace_env_args / replace_env_result
	cursorFldInteractionConnectScm   = 14 // f14 connect_scm_request_query / connect_scm_request_response

	// McpAuthRequestQuery.f1 args → McpAuthArgs.f1 server_identifier
	cursorFldInteractionMcpAuthQueryArgs    = 1
	cursorFldInteractionMcpAuthArgsServerID = 1
	// WebSearch/WebFetch/SwitchMode/McpAuth/ConnectScm RequestResponse.f1 approved / f2 rejected
	cursorFldInteractionRespApproved = 1
	cursorFldInteractionRespRejected = 2
	// 上述各 *.Rejected.f1 reason
	cursorFldInteractionRejectedReason = 1
	// SetupVmEnvironmentResult.f1 success
	cursorFldInteractionSetupVMSuccess = 1
	// AskQuestionInteractionResponse.f1 result → AskQuestionResult.f3 rejected → AskQuestionRejected.f1 reason
	cursorFldInteractionAskQuestionResult   = 1
	cursorFldInteractionAskQuestionRejected = 3
	cursorFldInteractionAskQuestionReason   = 1
	// CreatePlanRequestResponse.f1 result → CreatePlanResult.f2 error → CreatePlanError.f1 error
	cursorFldInteractionCreatePlanResult = 1
	cursorFldInteractionCreatePlanError  = 2
	cursorFldInteractionCreatePlanErrMsg = 1
	// PrManagementResult.f2 error → PrManagementError.f1 error
	cursorFldInteractionPrManagementError  = 2
	cursorFldInteractionPrManagementErrMsg = 1
	// ReplaceEnvResult.f2 failure → ReplaceEnvFailure{f1 error_message, f2 setup_logs}
	cursorFldInteractionReplaceEnvFailure      = 2
	cursorFldInteractionReplaceEnvErrorMessage = 1
	cursorFldInteractionReplaceEnvSetupLogs    = 2
)

// cursorGenericUnknownInteractionRejectText 是「未知 interaction 类型」通用 rejected 应答文本
// （人话：这种交互在本环境不支持）。与已知交互类型的精确 rejected 文本互不干扰。
const cursorGenericUnknownInteractionRejectText = "This interaction type is not supported in this environment."

// respondInteractionQuery 按方案 §1.2 应答表回写 InteractionResponse（AgentClientMessage.f6）。
// 不支持的询问（f12 generate_image / 未知字段 / 缺 oneof）回通用 rejected 继续，不写帧、不 502。
// 日志只记 query_id / query_field / answer，不记 args 正文。
func (d *cursorAgentDuplex) respondInteractionQuery(payload []byte) (bool, error) {
	fields, err := cursorDecodeProtobuf(payload)
	if err != nil {
		return false, err
	}
	id := cursorVarintField(fields, cursorFldInteractionID)
	// oneof query：取 f1 以外的字段，按 protobuf oneof 语义 last-one-wins。
	var query cursorPBField
	for _, f := range fields {
		if f.Number != cursorFldInteractionID {
			query = f
		}
	}
	field := query.Number
	// 派发单 D 阶段 2：未知 interaction 类型不再以 502 打死整轮，而是回一个通用 rejected 应答
	// 让模型换条路继续（绝不 sendExecStreamClose 关流）。形状遵循已知类型的 rejected：
	// {field:{f2:{f1:reason}}}。元数据/已知类型仍按既有逻辑在下方分派。
	unsupported := func() (bool, error) {
		d.logPhase(zapcore.WarnLevel, "interaction_query_unsupported", "",
			zap.Uint64("query_id", id), zap.Int("query_field", field))
		result := cursorAppendBytesField(nil, cursorFldInteractionRespRejected,
			cursorAppendStringField(nil, cursorFldInteractionRejectedReason, cursorGenericUnknownInteractionRejectText))
		resp := cursorAppendVarintField(nil, cursorFldInteractionID, id)
		resp = cursorAppendBytesField(resp, field, result)
		serr := d.send(cursorFldClientInteractionResponse, resp)
		d.logPhase(zapcore.InfoLevel, "interaction_query_answered", cursorHTTPErrClass(serr),
			zap.Uint64("query_id", id), zap.Int("query_field", field), zap.String("answer", "rejected-unknown"))
		return false, serr
	}
	// 已知交互询问类型（10 类）全部登记在统一字段表中；查表判定而非各自写一份清单。
	// 未知类型走 unsupported（派发单 D 阶段 2：回通用 rejected，不再 502 失败关闭）。
	if cursorFieldClassOf(cursorDomainInteraction, field) != cursorFieldInteraction {
		return unsupported()
	}
	if query.Wire != cursorWireBytes {
		return false, cursorFrameErrorf(cursorFrameMessage, "interaction query field %d is not a message", field)
	}
	sub, err := cursorDecodeProtobuf(query.Bytes)
	if err != nil {
		return false, err
	}

	approved := func() []byte { return cursorAppendBytesField(nil, cursorFldInteractionRespApproved, nil) }
	rejected := func(reason string) []byte {
		return cursorAppendBytesField(nil, cursorFldInteractionRespRejected,
			cursorAppendStringField(nil, cursorFldInteractionRejectedReason, reason))
	}
	var result []byte
	var answer string
	// 本段是「已知交互类型各自怎么应答」，不是第二份判定清单：未知类型已在上方
	// （cursorFieldClassOf(cursorDomainInteraction, field) 判定段）统一拦截并走
	// unsupported 失败关闭，故这里只需按类型分派应答，不重复判定。
	switch field {
	case cursorFldInteractionWebSearch, cursorFldInteractionWebFetch:
		result, answer = approved(), "approved"
	case cursorFldInteractionMcpAuth:
		var serverID string
		if args, ok := cursorFindField(sub, cursorFldInteractionMcpAuthQueryArgs); ok {
			argFields, aerr := cursorDecodeProtobuf(args.Bytes)
			if aerr != nil {
				return false, aerr
			}
			if sid, ok := cursorFindField(argFields, cursorFldInteractionMcpAuthArgsServerID); ok {
				serverID = sid.String()
			}
		}
		if serverID == cursorMcpProviderIdentifier {
			result, answer = approved(), "approved"
		} else {
			result, answer = rejected(fmt.Sprintf("MCP server '%s' not found", serverID)), "rejected"
		}
	case cursorFldInteractionSetupVM:
		result = cursorAppendBytesField(nil, cursorFldInteractionSetupVMSuccess, nil)
		answer = "success"
	case cursorFldInteractionAskQuestion:
		reason := cursorAppendStringField(nil, cursorFldInteractionAskQuestionReason,
			"No interactive user is available in this environment. Ask the user directly in your reply text.")
		res := cursorAppendBytesField(nil, cursorFldInteractionAskQuestionRejected, reason)
		result = cursorAppendBytesField(nil, cursorFldInteractionAskQuestionResult, res)
		answer = "rejected"
	case cursorFldInteractionSwitchMode:
		result = rejected("Mode switching is not supported in this environment. Continue in the current agent mode.")
		answer = "rejected"
	case cursorFldInteractionCreatePlan:
		msg := cursorAppendStringField(nil, cursorFldInteractionCreatePlanErrMsg,
			"Plan files cannot be created in this environment. List the plan directly in your reply.")
		res := cursorAppendBytesField(nil, cursorFldInteractionCreatePlanError, msg)
		result = cursorAppendBytesField(nil, cursorFldInteractionCreatePlanResult, res)
		answer = "error"
	case cursorFldInteractionPrManagement:
		msg := cursorAppendStringField(nil, cursorFldInteractionPrManagementErrMsg,
			"PR management is only available in cloud agents")
		result = cursorAppendBytesField(nil, cursorFldInteractionPrManagementError, msg)
		answer = "error"
	case cursorFldInteractionReplaceEnv:
		failure := cursorAppendStringField(nil, cursorFldInteractionReplaceEnvErrorMessage,
			"Environment replacement is not supported in the IDE client")
		failure = cursorAppendStringField(failure, cursorFldInteractionReplaceEnvSetupLogs, "")
		result = cursorAppendBytesField(nil, cursorFldInteractionReplaceEnvFailure, failure)
		answer = "failure"
	case cursorFldInteractionConnectScm:
		result = rejected("Connecting source control is not available in this environment")
		answer = "rejected"
	}

	resp := cursorAppendVarintField(nil, cursorFldInteractionID, id)
	resp = cursorAppendBytesField(resp, field, result)
	serr := d.send(cursorFldClientInteractionResponse, resp)
	d.logPhase(zapcore.InfoLevel, "interaction_query_answered", cursorHTTPErrClass(serr),
		zap.Uint64("query_id", id), zap.Int("query_field", field), zap.String("answer", answer))
	return false, serr
}

// sendExecStreamClose 把 exec 终止控制帧写入当前 AgentRun（AgentClientMessage.f5）。
func (d *cursorAgentDuplex) sendExecStreamClose(execID uint64) error {
	return d.send(cursorFldClientControlMessage, cursorEncodeExecStreamClose(execID))
}

// cursorFindFieldOK 是 cursorFindField 的 bool 便捷版（仅判断字段是否存在）。
func cursorFindFieldOK(fields []cursorPBField, number int) bool {
	_, ok := cursorFindField(fields, number)
	return ok
}

// frameTopFieldNumbers 返回一条 AgentServerMessage 的顶层字段号（升序）。
//
// 纯协议结构信息：不含字段值、不含正文，可安全落日志；解码失败返回 nil
// （调用方此时不会再拿它判定业务语义）。
func frameTopFieldNumbers(payload []byte) []int {
	fields, err := cursorDecodeProtobuf(payload)
	if err != nil || len(fields) == 0 {
		return nil
	}
	nums := make([]int, 0, len(fields))
	for _, f := range fields {
		nums = append(nums, f.Number)
	}
	sort.Ints(nums)
	return nums
}

// ---------------------------------------------------------------------------
// ExecServerMessage 元数据字段白名单
//
// 依据生产日志 phase=exec_unsupported_fields field_numbers=[1,7,15,19,55] 与 bundle 原文：
//
//	ExecServerMessage:
//	  f1  id                              (varint)
//	  f15 exec_id                         (string)
//	  f57 machine_id                      (string, opt)
//	  f19 span_context                    (SpanContext, opt)
//	  f55 accept_hook_additional_contexts (bool, opt)
//
// agent-host 的 createExecInstance 每条 exec 都恒定构造 spanContext 与
// acceptHookAdditionalContexts（值可能 undefined，但字段常在）。因此存在
// "只有元数据、没有任何 exec 子字段"的消息（如 [1,15,19,55] / [1,15,57]）。
// 这类消息不承载执行指令，忽略即可，不得失败关闭整条会话。
// ---------------------------------------------------------------------------
const (
	// 服务端方向已证 metadata 字段（唯一可忽略的 exec 元数据白名单；客户端方向字段如
	// f39/f45 不在此列，未实现必须走 unsupported fail-close，不得被当作 metadata 忽略）。
	cursorFldExecMetaID          = 1  // f1  id
	cursorFldExecMetaExecID      = 15 // f15 exec_id
	cursorFldExecMetaSpanContext = 19 // f19 span_context
	cursorFldExecMetaAcceptHook  = 55 // f55 accept_hook_additional_contexts
	cursorFldExecMetaMachineID   = 57 // f57 machine_id
)

// cursorExecMetadataFieldOK 判断单个字段号是否属于可忽略的 exec 元数据白名单。
// **只有**白名单内的字段可被忽略；白名单外的未知字段一律失败关闭。
//
// 白名单已收口到统一字段表（cursor_field_table.go）：查 exec 域、判 cursorFieldMetadata
// 类，而非在此各自写一份 5 字段号的清单。未知字段查表命中 cursorFieldUnknown，返回 false。
func cursorExecMetadataFieldOK(number int) bool {
	return cursorFieldClassOf(cursorDomainExec, number) == cursorFieldMetadata
}

// cursorExecMetadataOnly 判断 ExecServerMessage 是否"只含元数据"（非空且全部字段在白名单内）。
//
// 逐字段查统一字段表判 cursorFieldMetadata 类；任一字段非元数据即返回 false。这保证未知字段
// 不会被误判成元数据，上层据此走 unsupported fail-close（见 respond 的 default 分支）。
func cursorExecMetadataOnly(parts []cursorPBField) bool {
	if len(parts) == 0 {
		return false
	}
	for _, f := range parts {
		if cursorFieldClassOf(cursorDomainExec, f.Number) != cursorFieldMetadata {
			return false
		}
	}
	return true
}

// cursorNativeExecRejectFieldOK 判断 ExecServerMessage 是否承载 11 类已裁定原生 exec 字段之一。
//
// 判定已收口到统一字段表：exec 域、cursorFieldNative 类即「已知原生工具字段」，
// 不再在此各自维护 11 个字段号的清单。
func cursorNativeExecRejectFieldOK(parts []cursorPBField) bool {
	for _, f := range parts {
		if cursorFieldClassOf(cursorDomainExec, f.Number) == cursorFieldNative {
			return true
		}
	}
	return false
}

// cursorExecImplementedSubfieldOK 判断是否出现任一已实现的 exec 子字段
// （f10 request_context / f36 mcp_state / f11 mcp_args 三类委托字段，或 11 类已裁定原生工具字段）。
//
// 判定已收口到统一字段表：exec 域的 cursorFieldHandler（委托字段）或 cursorFieldNative
// （已知原生工具字段）即属「已实现」。本函数纯查表，不再依赖 exec 派发 switch 的分支顺序；
// 因此旧注释「switch 已在 default 之前命中这些字段，故 default 中为 true 属内部逻辑错误」这一
// 前提已不成立——该「内部逻辑错误」的判定与失败关闭现在由调用方（exec 派发 default 分支）负责。
func cursorExecImplementedSubfieldOK(parts []cursorPBField) bool {
	for _, f := range parts {
		c := cursorFieldClassOf(cursorDomainExec, f.Number)
		if c == cursorFieldHandler || c == cursorFieldNative {
			return true
		}
	}
	return false
}

// respondContext 处理 exec 内层 f10（request_context）应答（既有逻辑逐字保留）。
func (d *cursorAgentDuplex) respondContext(parts []cursorPBField, execID uint64, idField []byte) (bool, error) {
	// §18.5 T：context/KV 应答写入前后各记一次事件，只记消息种类（context_response）
	// 与耗时，**绝不记任何键值**（key/value 为账号会话私有数据，严禁入日志）。
	d.logPhase(zapcore.DebugLevel, "context_write_before", "",
		zap.String("kind", "context_response"), zap.Uint64("exec_id", execID))
	// 不伪造客户端环境：未取得真实客户端环境时省略 context.environment，绝不回填服务器
	// GOOS / 时区 "UTC"（方案裁定：不能把服务器自身环境当客户端环境）。只保留字节外壳结构。
	ctx := cursorAppendBytesField(nil, 4, nil) // f4 environment 省略
	success := cursorAppendBytesField(nil, 1, ctx)
	result := cursorAppendBytesField(nil, 1, success)
	response := cursorAppendVarintField(nil, 1, execID)
	if len(idField) > 0 {
		response = cursorAppendBytesField(response, 15, idField)
	}
	response = cursorAppendBytesField(response, 10, result)
	serr := d.send(2, response)
	d.logPhase(zapcore.DebugLevel, "context_write_after", cursorHTTPErrClass(serr),
		zap.String("kind", "context_response"), zap.Uint64("exec_id", execID))
	return false, serr
}

// cursorMcpStateReply 构造 mcp_state 委托的回包（AgentClientMessage.f2 ExecClientMessage）。
//
// 结构（派发单 §2）：ExecClientMessage = {f1 id(exec_id), f15 id(原值),
// f36 = McpStateExecResult{f1 success = McpStateSuccess{f1 servers=[McpStateServer]}}}。
// McpStateServer = {f1 server_name:"sub2api", f2 server_identifier:"sub2api",
// f5 tools: 每个声明工具**单独**一条 f5 length-delimited（repeated 逐项，禁止拼接成单条）}。
//
// 字节级保真（依据活体探针 T7）：McpStateServer.f5 每条工具内层消息**原样回传请求侧
// AgentRunRequest.f4 的 McpToolDefinition 原始字节**（完整五字段：name / description /
// provider_identifier / tool_name / input_schema_json），不得只回工具名、不得重新编码。
// 无声明工具（f4 空）→ f5 不追加任何项。
func (d *cursorAgentDuplex) cursorMcpStateReply(execID uint64, idField []byte) ([]byte, error) {
	// McpStateServer.f5 是 repeated：每个声明工具**单独**一条 f5（内层即请求 f4 的
	// McpToolDefinition 原始字节），逐项 length-delimited 编码，**禁止**把多工具拼进同一条
	// f5（派发单 §2）。每条 definition **原样回传**（不重新编码、不改字段、不过滤）。
	serverMsg := cursorAppendStringField(nil, 1, cursorMcpProviderIdentifier)      // f1 server_name
	serverMsg = cursorAppendStringField(serverMsg, 2, cursorMcpProviderIdentifier) // f2 server_identifier
	for _, def := range d.declaredToolDefs {
		serverMsg = cursorAppendBytesField(serverMsg, 5, def)
	}
	success := cursorAppendBytesField(nil, 1, serverMsg)   // McpStateSuccess.f1 servers
	stateResult := cursorAppendBytesField(nil, 1, success) // McpStateExecResult.f1 success
	response := cursorAppendVarintField(nil, 1, execID)
	if len(idField) > 0 {
		response = cursorAppendBytesField(response, 15, idField)
	}
	response = cursorAppendBytesField(response, 36, stateResult) // ExecClientMessage.f36
	return response, nil
}

// 旧链（§24.3）已彻底删除：本层**不再存在**任何"用 McpResult.error 诱导模型结束"的
// 回包构造函数。工具委托由 cursorCaptureMcpArgs 写入唯一委托记录、会话进入
// waiting_tools；真实结果在续轮经由 CursorSessionIntake 生产实现（cursorAgentRunIntake）
// 按 §25.3 四层编码写回原 exec 关联。宁缺勿留：不保留任何"返回空回包"的占位函数，
// 避免旧链被再次接线。

// cursorCaptureMcpArgs 从 exec 内层 f11（mcp_args）捕获工具调用。
//
// 解析 McpArgs：f3 tool_call_id、f5 tool_name、f2 args（protobuf map<string,Value>，
// 每个 entry = {f1 key, f2 value}，Value：f1 null / f2 number / f3 string）。ArgsJSON
// 为对象 JSON 字符串。同时捕获承载本委托的 exec_id / idField（回写真实结果时原样带回）。
// **不终止连接**——写入唯一的委托记录供续轮回写通道收尾。
func (d *cursorAgentDuplex) cursorCaptureMcpArgs(parts []cursorPBField, execID uint64, idField []byte) (cursorToolCallCapture, error) {
	argsField, ok := cursorFindField(parts, 11)
	if !ok {
		return cursorToolCallCapture{}, cursorFrameErrorf(cursorFrameDelegation, "mcp_args without f11")
	}
	id, name, argsJSON, err := cursorDecodeMcpArgs(argsField.Bytes)
	if err != nil {
		return cursorToolCallCapture{}, err
	}
	// 按 schema 还原（r15 方案 §3）：schema 来源必须是 f4 原始字节（d.declaredToolDefs）。
	// declaredToolDefs 里是 **wire name**（卡 01 改名后），上游回传的 name 也是 wire name，
	// 所以这里的 name 仍用 wire name 去匹配 schema —— 不改。
	var failReason cursorRetypeErrReason
	retyped, rerr := cursorRetypeArgsByToolDefs(id, name, argsJSON, d.declaredToolDefs,
		func(reason cursorRetypeErrReason) {
			failReason = reason
			d.logPhase(zapcore.WarnLevel, "mcp_args_retype_failed", "",
				zap.String("retype_error", string(reason)),
				zap.String("tool_name", name),
				zap.String("call_id", id))
		})
	if rerr != nil {
		if !cursorRetypeDegradable(failReason) {
			// schema 侧不可解析（schema_absent / invalid / unresolvable /
			// projection_unresolvable）：与 parser 侧同口径，必须按上游失败终止本轮。
			// 注：缺必填（missing_required）派发单 D 阶段 2 起已纳入可降级，不再走到这里
			// （见 cursorRetypeDegradable）；诊断字段（missing_fields / present_fields /
			// missing_depth）只含字段名与深度，用于区分真空缺与命名错位，见 cursorArgsDiagFields。
			fields := []zap.Field{
				zap.String("retype_error", string(failReason)),
				zap.String("tool_name", name),
				zap.String("call_id", id),
			}
			fields = append(fields, cursorArgsDiagFields(rerr)...)
			d.logPhase(zapcore.ErrorLevel, "mcp_args_incomplete_rejected", "", fields...)
			return cursorToolCallCapture{}, fmt.Errorf("%w: tool %q args incomplete (%s)",
				ErrCursorArgsIncomplete, name, failReason)
		}
		// 与 parser 侧同口径：形状差异类重打失败不断流，降级交付上游原始参数。
		// fail-closed 会把一次参数形状不合升级成 502 + 会话终结（见 parser 侧注释）。
		d.logPhase(zapcore.WarnLevel, "mcp_args_retype_fallback", "",
			zap.String("tool_name", name),
			zap.String("call_id", id))
		retyped = argsJSON
	}
	// 方向 1（入站映射）：上游按 wire name 回调 → 映射回客户端原名（卡 03）。
	// 反查只在本请求的声明集合内进行（declaredToolReverse）；查不到（上游回调了没声明的
	// 名字）→ 保持原样（既有语义，不扩大改动）。
	//
	// 注意：交付给客户端的名字必须用原名；上面的 schema 还原已用 wire name 完成匹配，
	// 这里只在最后一步改写 tc.Name，绝不回头影响 schema 查找。
	origName := name
	if mapped, ok := d.declaredToolReverse[name]; ok {
		origName = mapped
	}
	tc := cursorToolCallCapture{ID: id, Name: origName, ArgsJSON: retyped, ExecID: execID, IDField: append([]byte(nil), idField...)}
	delegations := d.delegations
	if delegations == nil {
		delegations = newLocalCursorToolDelegations()
		d.delegations = delegations
	}
	if err := delegations.noteDelegation("", tc); err != nil {
		return cursorToolCallCapture{}, err
	}
	// 结构化日志（call_id/tool_name），不记 args 明文（可能含账号会话数据）。
	d.logPhase(zapcore.DebugLevel, "tool_call_captured", "",
		zap.String("tool_call_id", id), zap.String("tool_name", name))
	return tc, nil
}
