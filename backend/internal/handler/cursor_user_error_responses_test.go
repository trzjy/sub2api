//go:build unit

package handler

// 本文件钉死 /v1/responses（GatewayHandler.Responses）Execute 出口的 Cursor 用户报错（派发单 M3b，
// SSOT：docs-local/cursor-tool-failure-user-diagnosis-plan-20261101.md §2.2.2、§2.3 M3）：
// Cursor 账号的执行错误经 service.CursorUserFacingError 映射为正确状态码 + 中文提示（带错误码与请求号），
// 并落一条 cursor_user_error Warn 日志；非 Cursor 账号保持原 502 兜底不变。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

const cursorUserErrorResponsesRequestID = "m3b-req-id-1"

func newCursorUserErrorResponsesContext(t *testing.T) (*gin.Context, *httptest.ResponseRecorder, *observer.ObservedLogs, *zap.Logger) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	core, logs := observer.New(zap.WarnLevel)
	log := zap.New(core)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx := context.WithValue(req.Context(), ctxkey.RequestID, cursorUserErrorResponsesRequestID)
	c.Request = req.WithContext(logger.IntoContext(ctx, log))
	return c, rec, logs, log
}

func decodeCursorUserErrorResponsesBody(t *testing.T, rec *httptest.ResponseRecorder) (errType, message string) {
	t.Helper()
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "body=%s", rec.Body.String())
	return body.Error.Type, body.Error.Message
}

func TestCursorUserErrorResponsesMapsCursorErrors(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		status  int
		errType string
		code    string
		zh      string
	}{
		{
			name:    "resume_unknown_association",
			err:     &service.CursorResumeError{Status: http.StatusBadRequest, Reason: "unknown_association"},
			status:  http.StatusBadRequest,
			errType: "invalid_request_error",
			code:    pkgerrors.CursorResUnknownAssociationCode,
			zh:      pkgerrors.CursorResUnknownAssociation,
		},
		{
			name:    "request_invalid_image",
			err:     &service.CursorRequestError{Code: pkgerrors.CursorReqInvalidImageCode, Status: http.StatusBadRequest, Detail: "image data URI missing mime type"},
			status:  http.StatusBadRequest,
			errType: "invalid_request_error",
			code:    pkgerrors.CursorReqInvalidImageCode,
			zh:      pkgerrors.CursorReqInvalidImage,
		},
		{
			name:    "deadline_exceeded",
			err:     context.DeadlineExceeded,
			status:  http.StatusGatewayTimeout,
			errType: "upstream_error",
			code:    pkgerrors.CursorExecDeadlineExceededCode,
			zh:      pkgerrors.CursorExecDeadlineExceeded,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, rec, logs, log := newCursorUserErrorResponsesContext(t)
			h := &GatewayHandler{}
			account := &service.Account{ID: 1, Platform: service.PlatformCursor}

			wrote := h.writeResponsesForwardErrorResponse(c, log, account, tc.err, false)

			require.True(t, wrote)
			require.Equal(t, tc.status, rec.Code)
			errType, msg := decodeCursorUserErrorResponsesBody(t, rec)
			require.Equal(t, tc.errType, errType)
			require.True(t, strings.HasPrefix(msg, tc.zh), "message 必须以中文提示开头: %q", msg)
			require.Contains(t, msg, "错误码 "+tc.code)
			require.Contains(t, msg, "请求号 "+cursorUserErrorResponsesRequestID)

			entries := logs.FilterMessage("cursor_user_error").All()
			require.Len(t, entries, 1)
			fields := entries[0].ContextMap()
			require.Equal(t, tc.code, fields["error_code"])
			require.EqualValues(t, tc.status, fields["status"])
		})
	}
}

func TestCursorUserErrorResponsesNonCursorAccountUnchanged(t *testing.T) {
	errs := []error{
		&service.CursorResumeError{Status: http.StatusBadRequest, Reason: "unknown_association"},
		&service.CursorRequestError{Code: pkgerrors.CursorReqInvalidImageCode, Status: http.StatusBadRequest, Detail: "image data URI missing mime type"},
		context.DeadlineExceeded,
	}
	for _, err := range errs {
		c, rec, logs, log := newCursorUserErrorResponsesContext(t)
		h := &GatewayHandler{}
		account := &service.Account{ID: 2, Platform: service.PlatformAnthropic}

		wrote := h.writeResponsesForwardErrorResponse(c, log, account, err, false)

		require.True(t, wrote)
		require.Equal(t, http.StatusBadGateway, rec.Code)
		errType, msg := decodeCursorUserErrorResponsesBody(t, rec)
		require.Equal(t, "upstream_error", errType)
		require.Equal(t, pkgerrors.UpstreamRequestFailed, msg)
		require.Empty(t, logs.FilterMessage("cursor_user_error").All())
	}
}

func TestCursorUserErrorResponsesUnmappedCursorErrorFallsBack(t *testing.T) {
	c, rec, logs, log := newCursorUserErrorResponsesContext(t)
	h := &GatewayHandler{}
	account := &service.Account{ID: 3, Platform: service.PlatformCursor}

	wrote := h.writeResponsesForwardErrorResponse(c, log, account, context.Canceled, false)

	require.True(t, wrote)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	_, msg := decodeCursorUserErrorResponsesBody(t, rec)
	require.Equal(t, pkgerrors.UpstreamRequestFailed, msg)
	require.Empty(t, logs.FilterMessage("cursor_user_error").All())
}

func TestCursorUserErrorResponsesStreamStartedKeepsStatus(t *testing.T) {
	c, rec, _, log := newCursorUserErrorResponsesContext(t)
	h := &GatewayHandler{}
	account := &service.Account{ID: 4, Platform: service.PlatformCursor}
	c.Writer.WriteHeader(http.StatusOK)
	_, _ = c.Writer.Write([]byte(": ping\n\n"))

	wrote := h.writeResponsesForwardErrorResponse(c, log, account, context.DeadlineExceeded, false)

	require.True(t, wrote)
	require.Equal(t, http.StatusOK, rec.Code, "已提交响应后不得重写状态码")
	body := rec.Body.String()
	require.Contains(t, body, "event: response.failed")
	require.Contains(t, body, pkgerrors.CursorExecDeadlineExceeded)
	require.Contains(t, body, "错误码 "+pkgerrors.CursorExecDeadlineExceededCode)
	require.Contains(t, body, "请求号 "+cursorUserErrorResponsesRequestID)
}
