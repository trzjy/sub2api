package service

import (
	"net/http"
	"time"
)

// UpstreamFirstByteTimeoutType 是首字节护栏超时的错误类型标识（SSOT）。
//
// 其它卡（M1-b/c/d）在错误帧里携带该 type/code，使客户端能落到「任务被中断」
// （assistantMessage.interrupted）或「服务端错误兜底」（serviceErrorFallback）分支，
// 而不是 userCancelled 回落（方案文档 §1.4）。
const UpstreamFirstByteTimeoutType = "upstream_first_byte_timeout"

// GatewayFirstByteTimeoutReason 标记「本次失败是首字节/首包超时」——
// 用语义标记而非错误体字符串匹配，让 handler 在**任何**造该错误的路径上
// 都能识别（通用护栏、CN 家族首包超时等），不再依赖 ResponseBody 里的 type 字符串。
const GatewayFirstByteTimeoutReason GatewayFailureReason = "gateway_first_byte_timeout"

// UpstreamFirstByteTimeoutMessage 是首字节护栏超时（流尚未产出语义字节）时的
// 客户端可见文案（SSOT）。表达「上游未在时限内返回」，面向终端用户、可读，且不含任何
// 类取消触发词（abort / cancel 等，方案文档 §1.4 的 isCancelledLikeErrorMessage）。
const UpstreamFirstByteTimeoutMessage = "上游未在时限内返回首字节，请求尚未完成"

// UpstreamFirstByteTimeoutStreamStartedMessage 是流已开启（streamStarted=true）时的
// 客户端可见错误帧文案（SSOT）。表达「上游未在时限内返回、本次请求未能完成」，而不是
// 「请求被取消/被中止」，避免客户端落到 userCancelled 分支（方案文档 §1.4）。
const UpstreamFirstByteTimeoutStreamStartedMessage = "上游未在时限内返回首字节，本次请求未能完成，请稍后重试"

// upstreamFirstByteTimeoutBody 是护栏触发 failover 后的客户端可见错误体。
//
// 语义结构保持 {"error":{"type":...,"message":...}}；type 取自 UpstreamFirstByteTimeoutType，
// message 取自 UpstreamFirstByteTimeoutMessage（中文、不含类取消触发词）。本错误体的文案
// 升级为本机制的 SSOT，供其它卡引用（方案文档 §1.2 / §1.4）。
var upstreamFirstByteTimeoutBody = []byte(`{"error":{"type":"` + UpstreamFirstByteTimeoutType + `","message":"` + UpstreamFirstByteTimeoutMessage + `"}}`)

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
		Reason:                   GatewayFirstByteTimeoutReason,
	}
}
