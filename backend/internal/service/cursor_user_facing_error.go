package service

import (
	"context"
	"errors"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// CursorUserFacingError 把 Cursor 链路错误映射为面向用户的 HTTP 状态与中文提示。
//
// 按唯一顺序判定，第一条命中即返回；ok=false 表示不是 Cursor 链路错误，调用方走原路径：
//  1. *CursorResumeError → 按其 reason 返回已有提示，status 用其自身 Status。
//  2. *CursorRequestError → 其 Message / Status。
//  3. context.DeadlineExceeded → 超时提示，504。
//  4. 上游认证类哨兵 → 认证提示，502。
//  5. 上游模型、工具调用、协议错误 → 对应已有提示和状态。
//  6. 执行服务 / 来源指纹不可用 → 服务暂时不可用提示，503。
//
// 请求号取 ctx 中的 ctxkey.RequestID。
func CursorUserFacingError(ctx context.Context, err error) (status int, message string, ok bool) {
	if err == nil {
		return 0, "", false
	}
	if ctx == nil {
		ctx = context.Background()
	}

	var zh string
	var resumeErr *CursorResumeError
	var reqErr *CursorRequestError
	switch {
	case errors.As(err, &resumeErr) && resumeErr != nil:
		status = resumeErr.Status
		zh = infraerrors.CursorResumeReasonMessage(resumeErr.Reason)
	case errors.As(err, &reqErr) && reqErr != nil:
		status, zh = reqErr.Status, reqErr.Message
	case errors.Is(err, context.DeadlineExceeded):
		status, zh = http.StatusGatewayTimeout, infraerrors.CursorExecDeadlineExceeded
	case errors.Is(err, ErrCursorKeyRejectedUpstream):
		// 上游正文注入「密钥已过期或额度耗尽，请续费后使用」——这是**可信的额度/凭据
		// 耗尽信号**（带"请续费"语义、密钥级终态），2026-11-06 起单独给 CUR-EXEC-12，
		// 与那句挡板话术（CUR-EXEC-11）区分开。绝不透传上游原文（含账号卡 ID）。
		status, zh = http.StatusBadGateway, infraerrors.CursorExecAccountRejected
	case errors.Is(err, ErrCursorRelayAuthRejected),
		errors.Is(err, ErrCursorAuthMaterialUnavailable),
		errors.Is(err, ErrCursorQuotaKeyUnresolved),
		errors.Is(err, ErrCursorAccountBearerMissing):
		status, zh = http.StatusBadGateway, infraerrors.CursorExecUpstreamAuthFailed
	case errors.Is(err, ErrCursorUpstreamModelUnavailable):
		status, zh = http.StatusBadGateway, infraerrors.CursorExecModelUnavailable
	case errors.Is(err, ErrCursorQuotaExhaustedInStream):
		// 流内「今日额度已用完」模板：账号当日额度耗尽。绝不透传上游原文（含卡 ID）。
		status, zh = http.StatusTooManyRequests, infraerrors.CursorExecQuotaExhausted
	case errors.Is(err, ErrCursorBatchIncomplete), errors.Is(err, ErrCursorBatchAmbiguous):
		status, zh = http.StatusBadGateway, infraerrors.CursorExecBatchIncomplete
	case errors.Is(err, ErrCursorArgsIncomplete):
		// 上游交来残缺参数（缺 required 必填）：按上游失败终止，绝不降级交付残调用。
		status, zh = http.StatusBadGateway, infraerrors.CursorExecArgsIncomplete
	case errors.Is(err, ErrCursorProtocol),
		errors.Is(err, ErrCursorMissingEndStream),
		errors.Is(err, ErrCursorUpstreamStream):
		status, zh = http.StatusBadGateway, infraerrors.CursorExecUpstreamProtocol
	case errors.Is(err, ErrCursorExecutionUnavailable), errors.Is(err, ErrCursorSourceFingerprintUnavailable):
		status, zh = http.StatusServiceUnavailable, infraerrors.CursorResServiceUnavailable
	case errors.Is(err, ErrCursorResumeNotWaiting), errors.Is(err, ErrCursorContentWhileWaitingTools):
		// 上游在工具结果关联前结束会话（2026-11-22 用户裁定：不兜底，如实返回）。
		status, zh = http.StatusBadGateway, infraerrors.CursorExecSessionNotWaiting
	default:
		return 0, "", false
	}

	requestID, _ := ctx.Value(ctxkey.RequestID).(string)
	return status, infraerrors.CursorUserMessage(zh, requestID), true
}
