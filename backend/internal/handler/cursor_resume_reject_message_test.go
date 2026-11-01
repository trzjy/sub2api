//go:build unit

package handler

// 本文件钉死续轮拒绝出口（派发单 M2，SSOT：docs-local/cursor-tool-failure-user-diagnosis-plan-20261101.md
// §2.1、§2.3）：probe / identify / serve 三处拒绝写给客户端的 message 必须是中文用户提示
// （带错误码与请求号），不得外泄英文内部 reason；同时每次拒绝落一条 cursor_resume_rejected
// Warn 日志供定位，HTTP 状态码不变。

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	pkgerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

const cursorResumeRejectTestRequestID = "m2-req-id-1"

// newCursorResumeRejectTestContext 在最小 gin 上下文上挂请求号与 observer logger。
func newCursorResumeRejectTestContext(t *testing.T) (*gin.Context, *observer.ObservedLogs) {
	t.Helper()
	c := newCursorResumeTestContext(t)
	core, logs := observer.New(zap.WarnLevel)
	ctx := context.WithValue(c.Request.Context(), ctxkey.RequestID, cursorResumeRejectTestRequestID)
	c.Request = c.Request.WithContext(logger.IntoContext(ctx, zap.New(core)))
	return c, logs
}

// requireCursorResumeRejectLogged 断言恰好一条 cursor_resume_rejected 且字段正确。
func requireCursorResumeRejectLogged(t *testing.T, logs *observer.ObservedLogs, code, reason string, status int, stage string) {
	t.Helper()
	entries := logs.FilterMessage("cursor_resume_rejected").All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	require.Equal(t, code, fields["error_code"])
	require.Equal(t, reason, fields["reason"])
	require.EqualValues(t, status, fields["status"])
	require.Equal(t, stage, fields["stage"])
}

// requireCursorResumeRejectMessage 断言中文提示格式、错误码、请求号，且不含英文 reason。
func requireCursorResumeRejectMessage(t *testing.T, msg, code, zh, reason string) {
	t.Helper()
	require.True(t, strings.HasPrefix(msg, zh), "message 必须以中文提示开头: %q", msg)
	require.Contains(t, msg, "错误码 "+code)
	require.Contains(t, msg, "请求号 "+cursorResumeRejectTestRequestID)
	require.NotContains(t, msg, reason, "不得外泄英文内部 reason")
}

func TestServeCursorResumeRejectMessageIdentifyStage(t *testing.T) {
	c, logs := newCursorResumeRejectTestContext(t)
	gw := &cursorResumeFakeGateway{
		identifyErr: &service.CursorResumeError{Status: http.StatusBadRequest, Reason: "unknown_association"},
	}
	reject := &captureReject{}
	handled := serveCursorResumeIfRequested(c, gw, service.CursorResumeProtocolChat,
		[]byte(cursorResumeHandlerChatContinuation), service.CursorSessionIdentity{UserID: 1}, false, reject.fn(), "")

	require.True(t, handled)
	require.True(t, reject.called)
	require.Equal(t, http.StatusBadRequest, reject.status, "状态码不变")
	requireCursorResumeRejectMessage(t, reject.msg,
		pkgerrors.CursorResUnknownAssociationCode, pkgerrors.CursorResUnknownAssociation, "unknown_association")
	requireCursorResumeRejectLogged(t, logs,
		pkgerrors.CursorResUnknownAssociationCode, "unknown_association", http.StatusBadRequest, "identify")
}

func TestServeCursorResumeRejectMessageServeFallback(t *testing.T) {
	c, logs := newCursorResumeRejectTestContext(t)
	gw := &cursorResumeFakeGateway{
		ticket:   &service.CursorResumeTicket{SessionID: "s-1", Model: "cursor-test-model"},
		serveErr: errors.New("plain serve failure"),
	}
	reject := &captureReject{}
	handled := serveCursorResumeIfRequested(c, gw, service.CursorResumeProtocolChat,
		[]byte(cursorResumeHandlerChatContinuation), service.CursorSessionIdentity{UserID: 1}, false, reject.fn(), "")

	require.True(t, handled)
	require.True(t, reject.called)
	require.Equal(t, http.StatusBadGateway, reject.status, "status 0 兜底仍为 502")
	requireCursorResumeRejectMessage(t, reject.msg,
		pkgerrors.CursorResUnknownCode, pkgerrors.CursorResUnknown, "resume_failed")
	requireCursorResumeRejectLogged(t, logs,
		pkgerrors.CursorResUnknownCode, "resume_failed", http.StatusBadGateway, "serve")
}

func TestServeCursorResumeRejectMessageServeTypedError(t *testing.T) {
	c, logs := newCursorResumeRejectTestContext(t)
	gw := &cursorResumeFakeGateway{
		ticket: &service.CursorResumeTicket{SessionID: "s-1", Model: "cursor-test-model"},
		serveErr: &service.CursorResumeError{
			Status: http.StatusForbidden, Reason: "not_schedulable", Err: service.ErrCursorResumeAccountIneligible,
		},
	}
	reject := &captureReject{}
	handled := serveCursorResumeIfRequested(c, gw, service.CursorResumeProtocolChat,
		[]byte(cursorResumeHandlerChatContinuation), service.CursorSessionIdentity{UserID: 1}, false, reject.fn(), "")

	require.True(t, handled)
	require.Equal(t, http.StatusForbidden, reject.status)
	requireCursorResumeRejectMessage(t, reject.msg,
		pkgerrors.CursorResAccountUnavailableCode, pkgerrors.CursorResAccountUnavailable, "not_schedulable")
	requireCursorResumeRejectLogged(t, logs,
		pkgerrors.CursorResAccountUnavailableCode, "not_schedulable", http.StatusForbidden, "serve")
}

// TestCursorResumeRejectMessageProbeStage：现有探针解码路径不产出 CursorResumeError，
// 无法经 serveCursorResumeIfRequested 构造 probe 拒绝，故直接钉死 probe 出口所用的渲染函数。
func TestCursorResumeRejectMessageProbeStage(t *testing.T) {
	c, logs := newCursorResumeRejectTestContext(t)
	msg := cursorResumeRejectMessage(c, "probe", http.StatusBadRequest, "undecodable_continuation")
	requireCursorResumeRejectMessage(t, msg,
		pkgerrors.CursorResUndecodableCode, pkgerrors.CursorResUndecodable, "undecodable_continuation")
	requireCursorResumeRejectLogged(t, logs,
		pkgerrors.CursorResUndecodableCode, "undecodable_continuation", http.StatusBadRequest, "probe")
}

func TestCursorResumeRejectMessageMissingRequestID(t *testing.T) {
	c := newCursorResumeTestContext(t)
	msg := cursorResumeRejectMessage(c, "identify", http.StatusBadRequest, "mixed_sessions")
	require.Contains(t, msg, "请求号 未知")
	require.Contains(t, msg, "错误码 "+pkgerrors.CursorResInvalidResultsCode)
}
