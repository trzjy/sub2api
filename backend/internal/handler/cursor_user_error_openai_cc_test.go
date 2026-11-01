//go:build unit

package handler

// 本文件钉死 OpenAIGatewayHandler.ChatCompletions（openai_chat_completions.go）Execute 出口的
// Cursor 用户报错（派发单 M3c，SSOT：docs-local/cursor-tool-failure-user-diagnosis-plan-20261101.md
// §2.2.2、§2.3 M3）：Cursor 账号的执行错误经 service.CursorUserFacingError 映射为正确状态码 +
// 中文提示（带错误码与请求号），并落一条 cursor_user_error Warn；非 Cursor 账号保持原路径不变。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

const openAICCCursorTestRequestID = "req-m3c-openai-cc"

func newOpenAICCCursorUserErrorContext(t *testing.T) (*gin.Context, *httptest.ResponseRecorder, *observer.ObservedLogs, *zap.Logger) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	core, logs := observer.New(zap.WarnLevel)
	ctx := context.WithValue(context.Background(), ctxkey.RequestID, openAICCCursorTestRequestID)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	return c, rec, logs, zap.New(core)
}

// openAICCForwardErrorWrite 复刻 ChatCompletions 出口在 Forward 返回非 failover 错误、
// 且上游错误尚未写给客户端时的写回顺序（Cursor 分支 → 流读错误 → 通用兜底）。
func openAICCForwardErrorWrite(h *OpenAIGatewayHandler, c *gin.Context, log *zap.Logger, account *service.Account, err error, streamStarted bool) bool {
	if h.writeOpenAIChatCursorUserError(c, log, account, err, streamStarted) {
		return true
	}
	if h.ensureOpenAIStreamReadErrorResponse(c, err, streamStarted) {
		return true
	}
	return h.ensureForwardErrorResponse(c, streamStarted)
}

func decodeOpenAICCErrorBody(t *testing.T, rec *httptest.ResponseRecorder) (errType, message string) {
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

func TestCursorUserErrorOpenAIChatMapsCursorErrors(t *testing.T) {
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
			code:    infraerrors.CursorResUnknownAssociationCode,
			zh:      infraerrors.CursorResUnknownAssociation,
		},
		{
			name: "request_invalid_image",
			err: &service.CursorRequestError{
				Code: infraerrors.CursorReqInvalidImageCode, Status: http.StatusBadRequest,
				Detail: "image data URI payload is empty",
			},
			status:  http.StatusBadRequest,
			errType: "invalid_request_error",
			code:    infraerrors.CursorReqInvalidImageCode,
			zh:      infraerrors.CursorReqInvalidImage,
		},
		{
			name:    "deadline_exceeded",
			err:     context.DeadlineExceeded,
			status:  http.StatusGatewayTimeout,
			errType: "upstream_error",
			code:    infraerrors.CursorExecDeadlineExceededCode,
			zh:      infraerrors.CursorExecDeadlineExceeded,
		},
	}
	account := &service.Account{ID: 9601, Platform: service.PlatformCursor}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, rec, logs, log := newOpenAICCCursorUserErrorContext(t)
			h := &OpenAIGatewayHandler{}

			require.True(t, openAICCForwardErrorWrite(h, c, log, account, tc.err, false))

			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			errType, msg := decodeOpenAICCErrorBody(t, rec)
			require.Equal(t, tc.errType, errType)
			require.Contains(t, msg, strings.TrimSuffix(tc.zh, "。"))
			require.Contains(t, msg, "错误码 "+tc.code)
			require.Contains(t, msg, "请求号 "+openAICCCursorTestRequestID)
			require.NotContains(t, msg, "image data URI", "英文细节不得给用户")

			entries := logs.FilterMessage("cursor_user_error").All()
			require.Len(t, entries, 1)
			fields := entries[0].ContextMap()
			require.Equal(t, tc.code, fields["error_code"])
			require.EqualValues(t, tc.status, fields["status"])
		})
	}
}

// 流已开始：不改状态码，只在流内错误帧里换成中文 message。
func TestCursorUserErrorOpenAIChatStreamStartedKeepsStatus(t *testing.T) {
	c, rec, _, log := newOpenAICCCursorUserErrorContext(t)
	h := &OpenAIGatewayHandler{}
	c.Status(http.StatusOK)
	_, _ = c.Writer.WriteString(": ping\n\n")
	c.Writer.Flush()

	account := &service.Account{ID: 9602, Platform: service.PlatformCursor}
	require.True(t, openAICCForwardErrorWrite(h, c, log, account, context.DeadlineExceeded, false))

	require.Equal(t, http.StatusOK, rec.Code, "已提交响应后不得重写状态码")
	out := rec.Body.String()
	require.Contains(t, out, "event: error")
	require.Contains(t, out, "错误码 "+infraerrors.CursorExecDeadlineExceededCode)
	require.Contains(t, out, "请求号 "+openAICCCursorTestRequestID)
}

// 非 Cursor 账号：同样的错误仍走原路径（502 upstream_error + 上游请求失败），不写 cursor_user_error。
func TestCursorUserErrorOpenAIChatNonCursorUnchanged(t *testing.T) {
	errs := []error{
		&service.CursorResumeError{Status: http.StatusBadRequest, Reason: "unknown_association"},
		&service.CursorRequestError{Code: infraerrors.CursorReqInvalidImageCode, Status: http.StatusBadRequest, Detail: "x"},
		context.DeadlineExceeded,
	}
	for _, platform := range []string{service.PlatformOpenAI, service.PlatformAnthropic} {
		for _, err := range errs {
			c, rec, logs, log := newOpenAICCCursorUserErrorContext(t)
			h := &OpenAIGatewayHandler{}
			account := &service.Account{ID: 9603, Platform: platform}

			require.False(t, h.writeOpenAIChatCursorUserError(c, log, account, err, false))
			require.True(t, openAICCForwardErrorWrite(h, c, log, account, err, false))

			require.Equal(t, http.StatusBadGateway, rec.Code)
			errType, msg := decodeOpenAICCErrorBody(t, rec)
			require.Equal(t, "upstream_error", errType)
			require.Equal(t, infraerrors.UpstreamRequestFailed, msg)
			require.Empty(t, logs.FilterMessage("cursor_user_error").All())
		}
	}
}

// Cursor 账号但 ok=false（非 Cursor 链路错误）：走原兜底。
func TestCursorUserErrorOpenAIChatUnmappedFallsBack(t *testing.T) {
	c, rec, logs, log := newOpenAICCCursorUserErrorContext(t)
	h := &OpenAIGatewayHandler{}
	account := &service.Account{ID: 9604, Platform: service.PlatformCursor}

	require.True(t, openAICCForwardErrorWrite(h, c, log, account, context.Canceled, false))

	require.Equal(t, http.StatusBadGateway, rec.Code)
	_, msg := decodeOpenAICCErrorBody(t, rec)
	require.Equal(t, infraerrors.UpstreamRequestFailed, msg)
	require.Empty(t, logs.FilterMessage("cursor_user_error").All())
}
