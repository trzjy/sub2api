package service

// 本文件提供 cursor 出站链路的一次性阶段事件日志（方案 §18.5 T）。
//
// 设计约束（硬性）：
//   - 复用现有结构化 logger（logger.FromContext → 全局 logger），事件在发生时直接落盘；
//     不缓冲、不新增日志库/表、不引入全局可变 trace 状态、不启动后台 goroutine。
//   - 只记录不可变标识（request_id / 模型 / 阶段）、耗时与结果安全分类；
//     绝不记录 Bearer、key、device、headers、prompt 正文、KV 键值或上游原始错误体。
//   - httptrace 回调与双工 goroutine 只使用调用点捕获的不可变标识与起始时间，
//     不跨 goroutine 读 gin.Context、不共享未加锁 map。

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// cursorPhaseComponent 是阶段事件固定 component 字段，便于在现有运行日志里聚合检索。
const cursorPhaseComponent = "cursor.upstream"

// cursorPhaseLog 记录一次阶段事件：从 ctx 取请求级 logger 与 request_id，
// 同步调用点（forwardCursor / Do 主流程 / duplex.respond）直接传入 request ctx 即可。
func cursorPhaseLog(ctx context.Context, lvl zapcore.Level, phase, model string, extra ...zap.Field) {
	reqID, _ := ctx.Value(ctxkey.RequestID).(string)
	cursorPhaseLogFields(logger.FromContext(ctx), lvl, phase, model, reqID, extra...)
}

// cursorPhaseLogFields 是底层落盘：调用方已捕获不可变标识（logger / model / request_id），
// 供 httptrace 回调与双工 goroutine 使用，避免在这些并发回调里再读 ctx/gin.Context。
func cursorPhaseLogFields(lg *zap.Logger, lvl zapcore.Level, phase, model, reqID string, extra ...zap.Field) {
	if lg == nil {
		lg = logger.L()
	}
	fields := make([]zap.Field, 0, 4+len(extra))
	fields = append(fields,
		zap.String("component", cursorPhaseComponent),
		zap.String("phase", phase),
	)
	if model != "" {
		fields = append(fields, zap.String("model", model))
	}
	if reqID != "" {
		fields = append(fields, zap.String("request_id", reqID))
	}
	fields = append(fields, extra...)
	lg.Check(lvl, "cursor_phase_event").Write(fields...)
}

// cursorHTTPErrClass 把 HTTP 传输错误归到安全类别，不回显原始错误文案
// （避免把上游错误体写进日志）。只产出稳定枚举值，绝不含错误文本。
func cursorHTTPErrClass(err error) string {
	if err == nil {
		return "ok"
	}
	if errors.Is(err, context.Canceled) {
		return "context_canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "context_deadline"
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		switch netErr.Op {
		case "dial", "read", "write", "remoteaddr":
			return "connection_error"
		}
		return "net_op_error"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns_error"
	}
	var tlsErr *tls.CertificateVerificationError
	if errors.As(err, &tlsErr) {
		return "tls_cert_error"
	}
	// httptrace TLS 握手错误常以 *tls.CertificateVerificationError 之外的形态返回，
	// 这里仅按前缀做安全归类（不输出原始错误文本）。
	msg := err.Error()
	switch {
	case cursorErrContainsAny(msg, "tls", "x509", "certificate"):
		return "tls_error"
	case cursorErrContainsAny(msg, "timeout", "i/o timeout"):
		return "timeout"
	case cursorErrContainsAny(msg, "connection refused", "no route", "network is unreachable"):
		return "connection_error"
	}
	return "upstream_error"
}

// cursorStreamErrClass 把读循环终态错误归到**稳定安全类别**，只输出枚举值，
// 绝不回显原始错误文案（错误链里可能带上游 EndStream 的 message 原文）。
//
// 与 cursorHTTPErrClass 的分工：后者是传输层分类（httptrace / 网络），本函数覆盖
// 协议层与业务层哨兵——这两类是 cursor 会话"读到一半终止"的真正来源，此前在
// response_parse_done 上被统一写成 outcome=ok，导致事故复盘只能看到
// terminal_reason=stream_error，无法区分是"上游 EndStream 报错""缺 EndStream"
// "exec 消息不认""批次封口失败"还是"结果回写后上游不再推进"。
//
// 归类顺序固定（先具体后一般），新增哨兵时必须在此登记，否则退化为 protocol_other。
func cursorStreamErrClass(err error) string {
	if err == nil {
		return "ok"
	}
	switch {
	case errors.Is(err, io.EOF):
		return "eof"
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "context_deadline"
	case errors.Is(err, ErrCursorKeyRejectedUpstream):
		return "key_rejected"
	case errors.Is(err, ErrCursorUpstreamStream):
		return "upstream_end_stream_error"
	case errors.Is(err, ErrCursorMissingEndStream):
		return "missing_end_stream"
	case errors.Is(err, ErrCursorUnsupportedRequest):
		return "unsupported_execution"
	case errors.Is(err, ErrCursorContentWhileWaitingTools):
		return "content_while_waiting_tools"
	case errors.Is(err, ErrCursorBatchIncomplete):
		return "batch_incomplete"
	case errors.Is(err, ErrCursorBatchAmbiguous):
		return "batch_ambiguous"
	case errors.Is(err, ErrCursorProtocol):
		return "protocol_frame"
	}
	return "protocol_other"
}

// cursorErrContainsAny 是大小写不敏感的多关键字匹配（仅用于错误安全归类，不输出文本）。
func cursorErrContainsAny(s string, keys ...string) bool {
	lower := cursorErrToLower(s)
	for _, k := range keys {
		if cursorErrContains(lower, cursorErrToLower(k)) {
			return true
		}
	}
	return false
}

// cursorErrToLower / cursorErrContains 是局部小工具，避免为错误归类引入 strings 依赖的歧义。
func cursorErrToLower(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}

func cursorErrContains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
