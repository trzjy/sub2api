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
	"runtime"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Client 3.24.12 L_v/P_v and live Standard probes 01-version-checksum-only
// establish these headers. The machine identity stays scoped to the selected key.
const cursorClientVersion = "3.24.12"

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
	declaredToolDefs [][]byte
	// capturedToolCalls 是本轮经 mcp_args 委托捕获的工具调用（权威信号见 §2）。
	capturedToolCalls []cursorToolCallCapture
	// toolCallsFinished 标记本轮已发生工具委托捕获，供响应构造方收尾参考。
	toolCallsFinished bool
}

func newCursorAgentDuplex(initial []byte) (*cursorAgentDuplex, error) {
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
		return nil, fmt.Errorf("%w: missing AgentRun request", ErrCursorProtocol)
	}
	fields, err = cursorDecodeProtobuf(run.Bytes)
	if err != nil {
		return nil, err
	}
	d := &cursorAgentDuplex{blobs: make(map[string][]byte)}
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
			return nil, fmt.Errorf("%w: malformed pre-fetched blob", ErrCursorProtocol)
		}
		d.bytes += len(key.Bytes) + len(value.Bytes)
		if d.bytes > cursorUpstreamStreamMaxBytes {
			return nil, fmt.Errorf("%w: KV storage exceeds limit", ErrCursorProtocol)
		}
		d.blobs[string(key.Bytes)] = value.Bytes
	}
	r, w := io.Pipe()
	d.reader, d.writer = r, w
	return d, nil
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
		return 0, fmt.Errorf("%w: AgentRun response exceeds limit", ErrCursorProtocol)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(b.source, payload); err != nil {
		return 0, err
	}
	frame, _, err := cursorReadEnvelopeFrame(append(header, payload...), 0)
	if err != nil {
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

func (d *cursorAgentDuplex) respond(payload []byte) (bool, error) {
	fields, err := cursorDecodeProtobuf(payload)
	if err != nil {
		return false, err
	}
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
		switch {
		case cursorFindFieldOK(parts, 10):
			// 现有 context 应答（逐字不动，f10 既有处理）。
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
			// mcp_args 委托（f11 mcp_args）：捕获工具调用，回 McpResult.error，
			// **不终止连接**——继续读流直到 turn_ended（usage 完整计费）。
			tc, e := d.cursorCaptureMcpArgs(parts)
			if e != nil {
				return false, e
			}
			resp, e := d.cursorMcpArgsReply(execID, idField, tc)
			if e != nil {
				return false, e
			}
			serr := d.send(2, resp)
			d.logPhase(zapcore.DebugLevel, "mcp_args_reply", cursorHTTPErrClass(serr),
				zap.Uint64("exec_id", execID), zap.String("tool_name", tc.Name))
			return false, serr
		default:
			return false, fmt.Errorf("%w: AgentRun requested unsupported execution", ErrCursorUnsupportedRequest)
		}
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
				return false, fmt.Errorf("%w: malformed KV set", ErrCursorProtocol)
			}
			// §18.5 T：只记 KV 操作种类（kv_set）与耗时，不记 key/value 明文。
			d.logPhase(zapcore.DebugLevel, "kv_write_before", "",
				zap.String("kind", "kv_set"))
			d.bytes += len(key.Bytes) + len(value.Bytes)
			if d.bytes > cursorUpstreamStreamMaxBytes {
				return false, fmt.Errorf("%w: KV storage exceeds limit", ErrCursorProtocol)
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
				return false, fmt.Errorf("%w: AgentRun requested unknown KV blob", ErrCursorProtocol)
			}
			response = cursorAppendBytesField(response, 2, cursorAppendBytesField(nil, 1, value))
			serr := d.send(3, response)
			d.logPhase(zapcore.DebugLevel, "kv_write_after", cursorHTTPErrClass(serr),
				zap.String("kind", "kv_get"))
			return false, serr
		} else {
			return false, fmt.Errorf("%w: unsupported KV operation", ErrCursorProtocol)
		}
	}
	return false, nil
}

func (d *cursorAgentDuplex) send(field int, payload []byte) error {
	_, err := d.writer.Write(cursorEncodeEnvelope(cursorAppendBytesField(nil, field, payload)))
	return err
}

// cursorFindFieldOK 是 cursorFindField 的 bool 便捷版（仅判断字段是否存在）。
func cursorFindFieldOK(fields []cursorPBField, number int) bool {
	_, ok := cursorFindField(fields, number)
	return ok
}

// respondContext 处理 exec 内层 f10（request_context）应答（既有逻辑逐字保留）。
func (d *cursorAgentDuplex) respondContext(parts []cursorPBField, execID uint64, idField []byte) (bool, error) {
	// §18.5 T：context/KV 应答写入前后各记一次事件，只记消息种类（context_response）
	// 与耗时，**绝不记任何键值**（key/value 为账号会话私有数据，严禁入日志）。
	d.logPhase(zapcore.DebugLevel, "context_write_before", "",
		zap.String("kind", "context_response"), zap.Uint64("exec_id", execID))
	// ExecClientMessage.context_response.success.result.context.environment.
	env := cursorAppendStringField(nil, 1, runtime.GOOS)
	env = cursorAppendStringField(env, 10, "UTC")
	ctx := cursorAppendBytesField(nil, 4, env)
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

// cursorMcpArgsReply 构造 mcp_args 委托的回包：McpResult.error（派发单 §21.4-1）。
//
// 模型收到该 error 后不重试、正常 turn_ended（活体证据 T9b）。回包 = ExecClientMessage
// {f1 id(exec_id), f15 id(原值), f11 = McpResult{f2 error = McpError{f1 error:"..."}}}。
func (d *cursorAgentDuplex) cursorMcpArgsReply(execID uint64, idField []byte, tc cursorToolCallCapture) ([]byte, error) {
	const msg = "Tool execution deferred: the client will submit the result in a follow-up request. Do not retry this call."
	mcpError := cursorAppendStringField(nil, 1, msg)      // McpError.f1 error
	mcpResult := cursorAppendBytesField(nil, 2, mcpError) // McpResult.f2 error
	response := cursorAppendVarintField(nil, 1, execID)
	if len(idField) > 0 {
		response = cursorAppendBytesField(response, 15, idField)
	}
	response = cursorAppendBytesField(response, 11, mcpResult) // ExecClientMessage.f11
	_ = tc
	return response, nil
}

// cursorCaptureMcpArgs 从 exec 内层 f11（mcp_args）捕获工具调用。
//
// 解析 McpArgs：f3 tool_call_id、f5 tool_name、f2 args（protobuf map<string,Value>，
// 每个 entry = {f1 key, f2 value}，Value：f1 null / f2 number / f3 string）。ArgsJSON
// 为对象 JSON 字符串。**不终止连接**——置 toolCallsFinished 供响应构造方收尾。
func (d *cursorAgentDuplex) cursorCaptureMcpArgs(parts []cursorPBField) (cursorToolCallCapture, error) {
	argsField, ok := cursorFindField(parts, 11)
	if !ok {
		return cursorToolCallCapture{}, fmt.Errorf("%w: mcp_args without f11", ErrCursorProtocol)
	}
	id, name, argsJSON, err := cursorDecodeMcpArgs(argsField.Bytes)
	if err != nil {
		return cursorToolCallCapture{}, err
	}
	tc := cursorToolCallCapture{ID: id, Name: name, ArgsJSON: argsJSON}
	d.capturedToolCalls = append(d.capturedToolCalls, tc)
	d.toolCallsFinished = true
	// 结构化日志（call_id/tool_name），不记 args 明文（可能含账号会话数据）。
	d.logPhase(zapcore.DebugLevel, "tool_call_captured", "",
		zap.String("tool_call_id", id), zap.String("tool_name", name))
	return tc, nil
}
