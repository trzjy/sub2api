//go:build unit

package handler

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
)

// 本文件覆盖方案 §2.3 M3（M3a）：ChatCompletions 入口 Cursor 账号的 Execute 错误
// 按 service.CursorUserFacingError 写回中文提示与正确状态码；非 Cursor 账号保持原 502。

const ccCursorTestRequestID = "req-m3a-cc"

func newCCCursorUserErrorContext(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	ctx := context.WithValue(context.Background(), ctxkey.RequestID, ccCursorTestRequestID)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	return c, rec
}

// ccForwardErrorWrite 复刻 ChatCompletions 出口在 Forward 返回非 failover 错误、
// 且上游错误尚未写给客户端时的写回顺序。
func ccForwardErrorWrite(h *GatewayHandler, c *gin.Context, account *service.Account, err error, streamStarted bool) bool {
	if h.writeCCCursorUserError(c, zap.NewNop(), account, err, streamStarted) {
		return true
	}
	return h.ensureForwardErrorResponse(c, streamStarted)
}

type ccErrorBody struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func TestCursorUserErrorChatCompletions(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantType   string
		wantZh     string
		wantCode   string
	}{
		{
			name:       "resume unknown_association",
			err:        &service.CursorResumeError{Status: http.StatusBadRequest, Reason: "unknown_association"},
			wantStatus: http.StatusBadRequest,
			wantType:   "invalid_request_error",
			wantCode:   infraerrors.CursorResUnknownAssociationCode,
		},
		{
			name: "request error CUR-REQ-02",
			err: &service.CursorRequestError{
				Code: infraerrors.CursorReqInvalidImageCode, Status: http.StatusBadRequest,
				Detail: "image data URI payload is empty",
			},
			wantStatus: http.StatusBadRequest,
			wantType:   "invalid_request_error",
			wantZh:     infraerrors.CursorReqInvalidImage,
			wantCode:   infraerrors.CursorReqInvalidImageCode,
		},
		{
			name:       "deadline exceeded",
			err:        context.DeadlineExceeded,
			wantStatus: http.StatusGatewayTimeout,
			wantType:   "upstream_error",
			wantZh:     infraerrors.CursorExecDeadlineExceeded,
			wantCode:   infraerrors.CursorExecDeadlineExceededCode,
		},
	}
	cursorAccount := &service.Account{ID: 9501, Platform: service.PlatformCursor}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &GatewayHandler{}
			c, rec := newCCCursorUserErrorContext(t)

			require.True(t, ccForwardErrorWrite(h, c, cursorAccount, tc.err, false))

			require.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
			var body ccErrorBody
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.Equal(t, tc.wantType, body.Error.Type)
			wantZh := tc.wantZh
			if wantZh == "" {
				_, wantZh = infraerrors.CursorResumeReasonCode("unknown_association")
			}
			require.Contains(t, body.Error.Message, strings.TrimSuffix(wantZh, "。"))
			require.Contains(t, body.Error.Message, "错误码 "+tc.wantCode)
			require.Contains(t, body.Error.Message, "请求号 "+ccCursorTestRequestID)
			require.NotContains(t, body.Error.Message, "image data URI", "英文细节不得给用户")
		})
	}
}

// 流已开始：不改状态码，只在流内错误帧里换成中文 message。
func TestCursorUserErrorChatCompletionsStreamStarted(t *testing.T) {
	h := &GatewayHandler{}
	c, rec := newCCCursorUserErrorContext(t)
	c.Status(http.StatusOK)
	_, _ = c.Writer.WriteString(": ping\n\n")
	c.Writer.Flush()

	require.True(t, ccForwardErrorWrite(h, c, &service.Account{ID: 9502, Platform: service.PlatformCursor}, context.DeadlineExceeded, true))

	require.Equal(t, http.StatusOK, rec.Code)
	out := rec.Body.String()
	require.Contains(t, out, `"type":"error"`)
	require.Contains(t, out, "错误码 "+infraerrors.CursorExecDeadlineExceededCode)
	require.Contains(t, out, "请求号 "+ccCursorTestRequestID)
}

// 非 Cursor 账号：同样的错误仍走原路径（502 upstream_error + 上游请求失败）。
func TestCursorUserErrorChatCompletionsNonCursorUnchanged(t *testing.T) {
	errs := []error{
		&service.CursorResumeError{Status: http.StatusBadRequest, Reason: "unknown_association"},
		&service.CursorRequestError{Code: infraerrors.CursorReqInvalidImageCode, Status: http.StatusBadRequest, Detail: "x"},
		context.DeadlineExceeded,
	}
	for _, platform := range []string{service.PlatformAnthropic, service.PlatformGemini} {
		for _, err := range errs {
			h := &GatewayHandler{}
			c, rec := newCCCursorUserErrorContext(t)

			require.False(t, h.writeCCCursorUserError(c, zap.NewNop(), &service.Account{ID: 9503, Platform: platform}, err, false))
			require.True(t, ccForwardErrorWrite(h, c, &service.Account{ID: 9503, Platform: platform}, err, false))

			require.Equal(t, http.StatusBadGateway, rec.Code)
			var body ccErrorBody
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.Equal(t, "upstream_error", body.Error.Type)
			require.Equal(t, infraerrors.UpstreamRequestFailed, body.Error.Message)
		}
	}
}

// Cursor 账号但不是 Cursor 链路错误（ok=false）：走原路径。
func TestCursorUserErrorChatCompletionsUnmappedFallsBack(t *testing.T) {
	h := &GatewayHandler{}
	c, rec := newCCCursorUserErrorContext(t)
	account := &service.Account{ID: 9504, Platform: service.PlatformCursor}
	err := ccPlainError("some other failure")

	require.False(t, h.writeCCCursorUserError(c, zap.NewNop(), account, err, false))
	require.True(t, ccForwardErrorWrite(h, c, account, err, false))
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Contains(t, rec.Body.String(), infraerrors.UpstreamRequestFailed)
}

type ccPlainError string

func (e ccPlainError) Error() string { return string(e) }
