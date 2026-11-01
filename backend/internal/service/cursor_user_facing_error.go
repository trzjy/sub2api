package service

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// CursorUserFacingError 把 Cursor 链路错误映射为面向用户的 HTTP 状态、错误码与中文提示（方案 §2.2.2）。
//
// 按唯一顺序判定，第一条命中即返回；ok=false 表示不是 Cursor 链路错误，调用方走原路径：
//  1. *CursorResumeError → CursorResumeReasonCode(reason)，status 用其自身 Status。
//  2. *CursorRequestError → 其 Code / Status。
//  3. context.DeadlineExceeded → CUR-EXEC-01，504。
//  4. 上游认证类哨兵 → CUR-EXEC-06，502。
//  5. 批次封口类哨兵 → CUR-EXEC-04，502。
//  6. 工具结果写回传输失败：不在此单独判定。产生点（DeliverToolResults）由卡 S2b 改为
//     newCursorRequestError(CUR-EXEC-05, 502, …)，由第 2 条命中。
//  7. 上游协议类哨兵 → CUR-EXEC-03，502。
//  8. 执行服务 / 来源指纹不可用 → CUR-RES-10 文案（服务暂时不可用），503。
//  9. 其它带 "cursor" 前缀但未命中的错误 → CUR-EXEC-08，502，并写 Warn 日志 cursor_user_error_unmapped。
//
// message 由 infraerrors.CursorUserMessage 渲染，请求号取 ctx 中的 ctxkey.RequestID。
func CursorUserFacingError(ctx context.Context, err error) (status int, code, message string, ok bool) {
	if err == nil {
		return 0, "", "", false
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
		code, zh = infraerrors.CursorResumeReasonCode(resumeErr.Reason)
	case errors.As(err, &reqErr) && reqErr != nil:
		status, code = reqErr.Status, reqErr.Code
		zh = cursorRequestCodeMessage(reqErr.Code)
	case errors.Is(err, context.DeadlineExceeded):
		status, code, zh = http.StatusGatewayTimeout, infraerrors.CursorExecDeadlineExceededCode, infraerrors.CursorExecDeadlineExceeded
	case errors.Is(err, ErrCursorKeyRejectedUpstream),
		errors.Is(err, ErrCursorRelayAuthRejected),
		errors.Is(err, ErrCursorAuthMaterialUnavailable),
		errors.Is(err, ErrCursorQuotaKeyUnresolved),
		errors.Is(err, ErrCursorAccountBearerMissing):
		status, code, zh = http.StatusBadGateway, infraerrors.CursorExecUpstreamAuthFailedCode, infraerrors.CursorExecUpstreamAuthFailed
	case errors.Is(err, ErrCursorBatchIncomplete), errors.Is(err, ErrCursorBatchAmbiguous):
		status, code, zh = http.StatusBadGateway, infraerrors.CursorExecBatchIncompleteCode, infraerrors.CursorExecBatchIncomplete
	case errors.Is(err, ErrCursorProtocol),
		errors.Is(err, ErrCursorMissingEndStream),
		errors.Is(err, ErrCursorUpstreamStream):
		status, code, zh = http.StatusBadGateway, infraerrors.CursorExecUpstreamProtocolCode, infraerrors.CursorExecUpstreamProtocol
	case errors.Is(err, ErrCursorExecutionUnavailable), errors.Is(err, ErrCursorSourceFingerprintUnavailable):
		status, code, zh = http.StatusServiceUnavailable, infraerrors.CursorResServiceUnavailableCode, infraerrors.CursorResServiceUnavailable
	case strings.HasPrefix(err.Error(), "cursor"):
		// 识别本链路错误命名约定（"cursor:" 前缀），不是按内容猜类别；只记分类不记原文。
		logger.FromContext(ctx).Warn("cursor_user_error_unmapped", zap.String("error_class", cursorStreamErrClass(err)))
		status, code, zh = http.StatusBadGateway, infraerrors.CursorExecInternalErrorCode, infraerrors.CursorExecInternalError
	default:
		return 0, "", "", false
	}

	requestID, _ := ctx.Value(ctxkey.RequestID).(string)
	return status, code, infraerrors.CursorUserMessage(code, zh, requestID), true
}

// cursorRequestCodeMessage 返回 CursorRequestError.Code 对应的中文；未登记的码按内部错误文案兜底。
func cursorRequestCodeMessage(code string) string {
	switch code {
	case infraerrors.CursorReqMissingModelCode:
		return infraerrors.CursorReqMissingModel
	case infraerrors.CursorReqInvalidImageCode:
		return infraerrors.CursorReqInvalidImage
	case infraerrors.CursorReqImageOutsideUserCode:
		return infraerrors.CursorReqImageOutsideUser
	case infraerrors.CursorReqUnsupportedMessageCode:
		return infraerrors.CursorReqUnsupportedMessage
	case infraerrors.CursorReqInvalidToolSchemaCode:
		return infraerrors.CursorReqInvalidToolSchema
	case infraerrors.CursorExecUnsupportedCode:
		return infraerrors.CursorExecUnsupported
	case infraerrors.CursorExecResultWriteFailedCode:
		return infraerrors.CursorExecResultWriteFailed
	case infraerrors.CursorExecModelMappingInvalidCode:
		return infraerrors.CursorExecModelMappingInvalid
	default:
		return infraerrors.CursorExecInternalError
	}
}
