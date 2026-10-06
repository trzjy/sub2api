package service

import (
	"net/http"
	"time"
)

// upstreamFirstByteTimeoutBody 是护栏触发 failover 后的客户端可见错误体。
var upstreamFirstByteTimeoutBody = []byte(`{"error":{"type":"upstream_first_byte_timeout","message":"upstream produced no response headers before the deadline"}}`)

// upstreamFirstByteMinViableWindow 是换号最小可行窗口（v5 更正，零新计数器）。
// 剩余预算 ≥ 该阈值才允许换号；低于则视为预算耗尽，交由既有 FailoverExhausted
// 路径消费（handleFailoverExhausted(streamStarted)），与已接受设计一致。
const upstreamFirstByteMinViableWindow = 5 * time.Second

// newUpstreamFirstByteTimeoutError 产出平台无关的首字节护栏超时错误。
//
// 语义对齐既有 newOpenAIFirstOutputTimeoutError，但不依赖 OpenAI 命名/配置/
// handler 换号计数——通用服务层共享组件。产出 UpstreamFailoverError 且
// SafeToFailoverAfterWrite=true（仅写出 SSE 注释等非语义字节时仍可在同一客户端
// 流中切换账号）。
//
// 换号语义（v5 更正，零新计数器）：ShouldRetryNextAccount 由剩余预算判定——
//   - 剩余 ≥ 最小可行窗口（5s）→ NextAccountAction = NextAccountRetry
//     → ShouldRetryNextAccount()=true → 既有 FailoverContinue 换号；
//   - 剩余 < 5s → NextAccountAction = NextAccountStop
//     → ShouldRetryNextAccount()=false → 既有 FailoverExhausted
//     → handleFailoverExhausted(streamStarted)（stream→SSE error 帧；非流式→JSON）。
func newUpstreamFirstByteTimeoutError(remaining time.Duration) *UpstreamFailoverError {
	action := NextAccountRetry
	if remaining < upstreamFirstByteMinViableWindow {
		action = NextAccountStop
	}
	return &UpstreamFailoverError{
		StatusCode:               http.StatusGatewayTimeout,
		ResponseBody:             upstreamFirstByteTimeoutBody,
		ResponseHeaders:          http.Header{},
		SafeToFailoverAfterWrite: true,
		NextAccountAction:        action,
	}
}
