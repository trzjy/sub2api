//go:build unit

package handler

// 本文件钉死 OpenAIGatewayHandler 两处 Execute 出口的 Cursor 用户报错（派发单 M3d，
// SSOT：docs-local/cursor-tool-failure-user-diagnosis-plan-20261101.md §2.2.2、§2.3 M3）：
//   - /v1/responses（OpenAIGatewayHandler.Responses）：Cursor 分支 → 流读错误 → ensureForwardErrorResponse；
//   - /v1/messages（OpenAIGatewayHandler.Messages，OpenAI 平台转 Anthropic 形状）：
//     Cursor 分支 → 流读错误 → ensureAnthropicErrorResponse。
// Cursor 账号的执行错误经 service.CursorUserFacingError 映射为正确状态码 + 中文提示，并落一条
// cursor_user_error Warn；非 Cursor 账号与未命中映射的错误保持原路径不变。

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

const openAIGatewayCursorTestRequestID = "req-m3d-openai-gateway"

func newOpenAIGatewayCursorUserErrorContext(t *testing.T, path string) (*gin.Context, *httptest.ResponseRecorder, *observer.ObservedLogs, *zap.Logger) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	core, logs := observer.New(zap.WarnLevel)
	ctx := context.WithValue(context.Background(), ctxkey.RequestID, openAIGatewayCursorTestRequestID)
	c.Request = httptest.NewRequest(http.MethodPost, path, nil).WithContext(ctx)
	return c, rec, logs, zap.New(core)
}

// openAIResponsesForwardErrorWrite 复刻 Responses 出口在 Forward 返回非 failover 错误、
// 且上游错误尚未写给客户端时的写回顺序。
func openAIResponsesForwardErrorWrite(h *OpenAIGatewayHandler, c *gin.Context, log *zap.Logger, account *service.Account, err error, streamStarted bool) bool {
	if h.writeOpenAIResponsesCursorUserError(c, log, account, err, streamStarted) {
		return true
	}
	if h.ensureOpenAIStreamReadErrorResponse(c, err, streamStarted) {
		return true
	}
	return h.ensureForwardErrorResponse(c, streamStarted)
}

// openAIMessagesForwardErrorWrite 复刻 Messages 出口的写回顺序。
func openAIMessagesForwardErrorWrite(h *OpenAIGatewayHandler, c *gin.Context, log *zap.Logger, account *service.Account, err error, streamStarted bool) bool {
	if h.writeOpenAIMessagesCursorUserError(c, log, account, err, streamStarted) {
		return true
	}
	if code, message, ok := service.OpenAIUpstreamStreamReadErrorDetails(err); ok {
		if h.ensureAnthropicStreamReadErrorResponse(c, code, message, streamStarted) {
			return true
		}
	}
	return h.ensureAnthropicErrorResponse(c, streamStarted)
}

func decodeOpenAIGatewayErrorBody(t *testing.T, rec *httptest.ResponseRecorder) (errType, message string) {
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

type openAIGatewayCursorErrCase struct {
	name   string
	err    error
	status int
	code   string
	zh     string
}

func openAIGatewayCursorErrCases() []openAIGatewayCursorErrCase {
	return []openAIGatewayCursorErrCase{
		{
			name:   "resume_unknown_association",
			err:    &service.CursorResumeError{Status: http.StatusBadRequest, Reason: "unknown_association"},
			status: http.StatusBadRequest,
			code:   infraerrors.CursorResUnknownAssociationCode,
			zh:     infraerrors.CursorResUnknownAssociation,
		},
		{
			name: "request_invalid_image",
			err: &service.CursorRequestError{
				Code: infraerrors.CursorReqInvalidImageCode, Status: http.StatusBadRequest,
				Detail: "image data URI payload is empty",
			},
			status: http.StatusBadRequest,
			code:   infraerrors.CursorReqInvalidImageCode,
			zh:     infraerrors.CursorReqInvalidImage,
		},
		{
			name:   "deadline_exceeded",
			err:    context.DeadlineExceeded,
			status: http.StatusGatewayTimeout,
			code:   infraerrors.CursorExecDeadlineExceededCode,
			zh:     infraerrors.CursorExecDeadlineExceeded,
		},
	}
}

func openAIGatewayNonCursorErrs() []error {
	return []error{
		&service.CursorResumeError{Status: http.StatusBadRequest, Reason: "unknown_association"},
		&service.CursorRequestError{Code: infraerrors.CursorReqInvalidImageCode, Status: http.StatusBadRequest, Detail: "x"},
		context.DeadlineExceeded,
	}
}

func requireOpenAIGatewayCursorMessage(t *testing.T, msg string, tc openAIGatewayCursorErrCase) {
	t.Helper()
	require.Contains(t, msg, strings.TrimSuffix(tc.zh, "。"))
	require.Contains(t, msg, "错误码 "+tc.code)
	require.Contains(t, msg, "请求号 "+openAIGatewayCursorTestRequestID)
	require.NotContains(t, msg, "image data URI", "英文细节不得给用户")
}

func requireOpenAIGatewayCursorLog(t *testing.T, logs *observer.ObservedLogs, tc openAIGatewayCursorErrCase) {
	t.Helper()
	entries := logs.FilterMessage("cursor_user_error").All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	require.Equal(t, tc.code, fields["error_code"])
	require.EqualValues(t, tc.status, fields["status"])
}

func TestCursorUserErrorOpenAIGatewayResponsesMapsCursorErrors(t *testing.T) {
	account := &service.Account{ID: 9701, Platform: service.PlatformCursor}
	for _, tc := range openAIGatewayCursorErrCases() {
		t.Run(tc.name, func(t *testing.T) {
			c, rec, logs, log := newOpenAIGatewayCursorUserErrorContext(t, "/v1/responses")
			h := &OpenAIGatewayHandler{}

			require.True(t, openAIResponsesForwardErrorWrite(h, c, log, account, tc.err, false))

			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			errType, msg := decodeOpenAIGatewayErrorBody(t, rec)
			wantType := "upstream_error"
			if tc.status < 500 {
				wantType = "invalid_request_error"
			}
			require.Equal(t, wantType, errType)
			requireOpenAIGatewayCursorMessage(t, msg, tc)
			requireOpenAIGatewayCursorLog(t, logs, tc)
		})
	}
}

// 流已开始：不改状态码，只在 response.failed 终止事件里换成中文 message。
func TestCursorUserErrorOpenAIGatewayResponsesStreamStartedKeepsStatus(t *testing.T) {
	c, rec, _, log := newOpenAIGatewayCursorUserErrorContext(t, "/v1/responses")
	h := &OpenAIGatewayHandler{}
	c.Status(http.StatusOK)
	_, _ = c.Writer.WriteString(": ping\n\n")
	c.Writer.Flush()

	account := &service.Account{ID: 9702, Platform: service.PlatformCursor}
	require.True(t, openAIResponsesForwardErrorWrite(h, c, log, account, context.DeadlineExceeded, false))

	require.Equal(t, http.StatusOK, rec.Code, "已提交响应后不得重写状态码")
	out := rec.Body.String()
	require.Contains(t, out, "response.failed")
	require.Contains(t, out, "错误码 "+infraerrors.CursorExecDeadlineExceededCode)
	require.Contains(t, out, "请求号 "+openAIGatewayCursorTestRequestID)
}

// 非 Cursor 账号：同样的错误仍走原路径（502 upstream_error + 上游请求失败），不写 cursor_user_error。
func TestCursorUserErrorOpenAIGatewayResponsesNonCursorUnchanged(t *testing.T) {
	for _, platform := range []string{service.PlatformOpenAI, service.PlatformAnthropic} {
		for _, err := range openAIGatewayNonCursorErrs() {
			c, rec, logs, log := newOpenAIGatewayCursorUserErrorContext(t, "/v1/responses")
			h := &OpenAIGatewayHandler{}
			account := &service.Account{ID: 9703, Platform: platform}

			require.False(t, h.writeOpenAIResponsesCursorUserError(c, log, account, err, false))
			require.True(t, openAIResponsesForwardErrorWrite(h, c, log, account, err, false))

			require.Equal(t, http.StatusBadGateway, rec.Code)
			errType, msg := decodeOpenAIGatewayErrorBody(t, rec)
			require.Equal(t, "upstream_error", errType)
			require.Equal(t, infraerrors.UpstreamRequestFailed, msg)
			require.Empty(t, logs.FilterMessage("cursor_user_error").All())
		}
	}
}

// Cursor 账号但 ok=false（非 Cursor 链路错误）：走原兜底。
func TestCursorUserErrorOpenAIGatewayResponsesUnmappedFallsBack(t *testing.T) {
	c, rec, logs, log := newOpenAIGatewayCursorUserErrorContext(t, "/v1/responses")
	h := &OpenAIGatewayHandler{}
	account := &service.Account{ID: 9704, Platform: service.PlatformCursor}

	require.True(t, openAIResponsesForwardErrorWrite(h, c, log, account, context.Canceled, false))

	require.Equal(t, http.StatusBadGateway, rec.Code)
	_, msg := decodeOpenAIGatewayErrorBody(t, rec)
	require.Equal(t, infraerrors.UpstreamRequestFailed, msg)
	require.Empty(t, logs.FilterMessage("cursor_user_error").All())
}

func TestCursorUserErrorOpenAIGatewayMessagesMapsCursorErrors(t *testing.T) {
	account := &service.Account{ID: 9711, Platform: service.PlatformCursor}
	for _, tc := range openAIGatewayCursorErrCases() {
		t.Run(tc.name, func(t *testing.T) {
			c, rec, logs, log := newOpenAIGatewayCursorUserErrorContext(t, "/v1/messages")
			h := &OpenAIGatewayHandler{}

			require.True(t, openAIMessagesForwardErrorWrite(h, c, log, account, tc.err, false))

			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			errType, msg := decodeOpenAIGatewayErrorBody(t, rec)
			wantType := "api_error"
			if tc.status < 500 {
				wantType = "invalid_request_error"
			}
			require.Equal(t, wantType, errType)
			requireOpenAIGatewayCursorMessage(t, msg, tc)
			requireOpenAIGatewayCursorLog(t, logs, tc)
		})
	}
}

// 流已开始：不改状态码，只在 Anthropic event: error 帧里换成中文 message。
func TestCursorUserErrorOpenAIGatewayMessagesStreamStartedKeepsStatus(t *testing.T) {
	c, rec, _, log := newOpenAIGatewayCursorUserErrorContext(t, "/v1/messages")
	h := &OpenAIGatewayHandler{}
	c.Status(http.StatusOK)
	_, _ = c.Writer.WriteString(": ping\n\n")
	c.Writer.Flush()

	account := &service.Account{ID: 9712, Platform: service.PlatformCursor}
	require.True(t, openAIMessagesForwardErrorWrite(h, c, log, account, context.DeadlineExceeded, false))

	require.Equal(t, http.StatusOK, rec.Code, "已提交响应后不得重写状态码")
	out := rec.Body.String()
	require.Contains(t, out, "event: error")
	require.Contains(t, out, `"type":"api_error"`)
	require.Contains(t, out, "错误码 "+infraerrors.CursorExecDeadlineExceededCode)
	require.Contains(t, out, "请求号 "+openAIGatewayCursorTestRequestID)
}

// 非 Cursor 账号：仍走原 ensureAnthropicErrorResponse（502 api_error + 上游请求失败）。
func TestCursorUserErrorOpenAIGatewayMessagesNonCursorUnchanged(t *testing.T) {
	for _, platform := range []string{service.PlatformOpenAI, service.PlatformAnthropic} {
		for _, err := range openAIGatewayNonCursorErrs() {
			c, rec, logs, log := newOpenAIGatewayCursorUserErrorContext(t, "/v1/messages")
			h := &OpenAIGatewayHandler{}
			account := &service.Account{ID: 9713, Platform: platform}

			require.False(t, h.writeOpenAIMessagesCursorUserError(c, log, account, err, false))
			require.True(t, openAIMessagesForwardErrorWrite(h, c, log, account, err, false))

			require.Equal(t, http.StatusBadGateway, rec.Code)
			errType, msg := decodeOpenAIGatewayErrorBody(t, rec)
			require.Equal(t, "api_error", errType)
			require.Equal(t, infraerrors.UpstreamRequestFailed, msg)
			require.Empty(t, logs.FilterMessage("cursor_user_error").All())
		}
	}
}

// Cursor 账号但 ok=false：走原兜底。
func TestCursorUserErrorOpenAIGatewayMessagesUnmappedFallsBack(t *testing.T) {
	c, rec, logs, log := newOpenAIGatewayCursorUserErrorContext(t, "/v1/messages")
	h := &OpenAIGatewayHandler{}
	account := &service.Account{ID: 9714, Platform: service.PlatformCursor}

	require.True(t, openAIMessagesForwardErrorWrite(h, c, log, account, context.Canceled, false))

	require.Equal(t, http.StatusBadGateway, rec.Code)
	_, msg := decodeOpenAIGatewayErrorBody(t, rec)
	require.Equal(t, infraerrors.UpstreamRequestFailed, msg)
	require.Empty(t, logs.FilterMessage("cursor_user_error").All())
}
