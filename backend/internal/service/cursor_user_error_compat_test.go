//go:build unit

package service

// M3e：Gemini 兼容服务三个 Cursor 出口（Messages / Chat Completions / 原生 v1beta）
// 在 Cursor 账号上按 CursorUserFacingError 写回中文提示与对应状态码（方案 §2.2.2、§2.3 M3）；
// 非 Cursor 账号保持改前行为。直接驱动三个写回函数（出口的唯一错误写回点）。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const cursorUserErrorCompatRequestID = "rid-m3e-compat"

type cursorUserErrorCompatExit struct {
	name  string
	write func(s *GeminiMessagesCompatService, c *gin.Context, account *Account, err error) error
	// message / errType 从响应体里取出
	parse func(t *testing.T, body []byte) (message, errType string)
}

func cursorUserErrorCompatExits() []cursorUserErrorCompatExit {
	return []cursorUserErrorCompatExit{
		{
			name: "messages",
			write: func(s *GeminiMessagesCompatService, c *gin.Context, a *Account, err error) error {
				return s.writeCursorMessagesError(c, a, err)
			},
			parse: func(t *testing.T, body []byte) (string, string) {
				var out struct {
					Error struct{ Type, Message string } `json:"error"`
				}
				require.NoError(t, json.Unmarshal(body, &out), string(body))
				return out.Error.Message, out.Error.Type
			},
		},
		{
			name: "google",
			write: func(s *GeminiMessagesCompatService, c *gin.Context, a *Account, err error) error {
				return s.writeCursorGoogleError(c, a, err)
			},
			parse: func(t *testing.T, body []byte) (string, string) {
				var out struct {
					Error struct{ Message string } `json:"error"`
				}
				require.NoError(t, json.Unmarshal(body, &out), string(body))
				return out.Error.Message, ""
			},
		},
		{
			name: "chat",
			write: func(s *GeminiMessagesCompatService, c *gin.Context, a *Account, err error) error {
				return s.writeCursorChatCompletionsError(c, a, err)
			},
			parse: func(t *testing.T, body []byte) (string, string) {
				var out struct {
					Error struct{ Type, Message string } `json:"error"`
				}
				require.NoError(t, json.Unmarshal(body, &out), string(body))
				return out.Error.Message, out.Error.Type
			},
		},
	}
}

func newCursorUserErrorCompatContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request = req.WithContext(context.WithValue(req.Context(), ctxkey.RequestID, cursorUserErrorCompatRequestID))
	return c, w
}

type cursorUserErrorCompatCase struct {
	name    string
	err     error
	status  int
	code    string
	zh      string
	msgType string // Messages / Chat 出口 4xx 期望的 error.type
}

func cursorUserErrorCompatCases() []cursorUserErrorCompatCase {
	return []cursorUserErrorCompatCase{
		{
			name:   "resume_unknown_association",
			err:    &CursorResumeError{Status: http.StatusBadRequest, Reason: "unknown_association"},
			status: http.StatusBadRequest, code: infraerrors.CursorResUnknownAssociationCode, zh: infraerrors.CursorResUnknownAssociation,
		},
		{
			name:   "request_invalid_image",
			err:    newCursorRequestError(infraerrors.CursorReqInvalidImageCode, http.StatusBadRequest, "image data URI payload is empty"),
			status: http.StatusBadRequest, code: infraerrors.CursorReqInvalidImageCode, zh: infraerrors.CursorReqInvalidImage,
		},
		{
			name:   "deadline_exceeded",
			err:    context.DeadlineExceeded,
			status: http.StatusGatewayTimeout, code: infraerrors.CursorExecDeadlineExceededCode, zh: infraerrors.CursorExecDeadlineExceeded,
		},
	}
}

// TestCursorUserErrorCompatCursorAccount：Cursor 账号三出口按映射写回状态码 + 中文 + 错误码 + 请求号。
func TestCursorUserErrorCompatCursorAccount(t *testing.T) {
	account := &Account{ID: 1, Platform: PlatformCursor}
	for _, exit := range cursorUserErrorCompatExits() {
		for _, tc := range cursorUserErrorCompatCases() {
			t.Run(exit.name+"/"+tc.name, func(t *testing.T) {
				c, w := newCursorUserErrorCompatContext()
				err := exit.write(&GeminiMessagesCompatService{}, c, account, tc.err)
				require.Error(t, err)
				require.Equal(t, tc.status, w.Code)
				msg, errType := exit.parse(t, w.Body.Bytes())
				require.Contains(t, msg, tc.zh)
				require.Contains(t, msg, "错误码 "+tc.code)
				require.Contains(t, msg, "请求号 "+cursorUserErrorCompatRequestID)
				require.NotContains(t, msg, "cursor:", "英文内部细节不得给用户")
				switch exit.name {
				case "messages":
					if tc.status >= 500 {
						require.Equal(t, "api_error", errType)
					} else {
						require.Equal(t, "invalid_request_error", errType)
					}
				case "chat":
					if tc.status >= 500 {
						require.Equal(t, "upstream_error", errType)
					} else {
						require.Equal(t, "invalid_request_error", errType)
					}
				}
			})
		}
	}
}

// TestCursorUserErrorCompatNonCursorAccount：非 Cursor 账号同一错误保持改前行为（状态与英文 message 不变）。
func TestCursorUserErrorCompatNonCursorAccount(t *testing.T) {
	account := &Account{ID: 2, Platform: PlatformGemini}
	for _, exit := range cursorUserErrorCompatExits() {
		for _, tc := range cursorUserErrorCompatCases() {
			t.Run(exit.name+"/"+tc.name, func(t *testing.T) {
				c, w := newCursorUserErrorCompatContext()
				_ = exit.write(&GeminiMessagesCompatService{}, c, account, tc.err)
				// 改前：Messages / Google 出口续轮错误用其自身状态，其余 502；Chat 出口固定 502。
				want := http.StatusBadGateway
				if exit.name != "chat" {
					if st, _ := CursorResumeHTTPStatus(tc.err); st != 0 {
						want = st
					}
				}
				require.Equal(t, want, w.Code)
				msg, _ := exit.parse(t, w.Body.Bytes())
				require.Equal(t, tc.err.Error(), msg)
			})
		}
	}
}

// TestCursorUserErrorCompatCommittedResponse：Messages / Google 出口响应已提交时不重写状态码与错误体。
func TestCursorUserErrorCompatCommittedResponse(t *testing.T) {
	account := &Account{ID: 1, Platform: PlatformCursor}
	for _, exit := range cursorUserErrorCompatExits()[:2] {
		t.Run(exit.name, func(t *testing.T) {
			c, w := newCursorUserErrorCompatContext()
			MarkResponseCommitted(c)
			err := exit.write(&GeminiMessagesCompatService{}, c, account, context.DeadlineExceeded)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.Empty(t, w.Body.Bytes())
		})
	}
}
