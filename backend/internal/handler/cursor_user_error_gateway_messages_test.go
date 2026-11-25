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

// 本文件覆盖方案 §2.3 M3（M3f）：/v1/messages（GatewayHandler.Messages）走 Cursor 账号时，
// Forward 返回的 Cursor 链路错误按 service.CursorUserFacingError 写回中文、错误码、
// 请求号与正确状态码。
//
// 2026-11-16 订正：本文件原先两条用例都走端到端（真实选号 + 真实链路，输入是「孤儿
// tool_result」），但产品口径 2026-11-06 起改成「能丢就丢」——续轮历史里对不上的
// tool_result 直接丢弃并记账（chat 路径 chat_messages_dropped / Responses 路径
// responses_items_skipped），不再整轮拒绝。允许的拒绝只剩 CUR-REQ-01 缺 model、
// CUR-REQ-06 整轮无可发送内容、CUR-REQ-07 以助手消息结尾的续写。
//
// 输入源消失后，请求会真的出站、打不通上游、回裸 502，于是断言长期红着，报错信息
// 却指向「翻译官坏了」——实际是断言过期。
//
// 因此改为直接对写回函数断言：验的是稳定契约（翻译 + 写回 + 状态码），不验易变输入。
// 这样以后再调一次丢弃策略，本文件不用跟着改。

const (
	m3fAccountID = int64(781011)
	m3fRequestID = "req-m3f-messages"
)

// Cursor 账号：续轮错误必须翻成中文 + 错误码 + 请求号，并按错误自身状态码写出，
// 不能落回通用的 502「上游请求失败」。
func TestCursorUserErrorGatewayMessagesUnknownAssociation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &GatewayHandler{}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil).
		WithContext(context.WithValue(context.Background(), ctxkey.RequestID, m3fRequestID))
	account := &service.Account{ID: m3fAccountID, Platform: service.PlatformCursor, Type: service.AccountTypeAPIKey}

	wrote := h.writeCCCursorUserError(c, zap.NewNop(), account,
		&service.CursorResumeError{Status: http.StatusBadRequest, Reason: "unknown_association"}, false)
	require.True(t, wrote, "Cursor 账号的续轮错误必须由写回函数处理，不能落回通用 502")

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "invalid_request_error", body.Error.Type)
	wantZh := infraerrors.CursorResumeReasonMessage("unknown_association")
	require.Contains(t, body.Error.Message, strings.TrimSuffix(wantZh, "。"))
	require.NotContains(t, body.Error.Message, "CUR-")
	require.Contains(t, body.Error.Message, "请求号 "+m3fRequestID)
	require.NotEqual(t, infraerrors.UpstreamRequestFailed, body.Error.Message)
}

// 非 Cursor 账号：同一错误仍走原 502 兜底，不得被 Cursor 文案接管。
func TestCursorUserErrorGatewayMessagesNonCursorUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	err := &service.CursorResumeError{Status: http.StatusBadRequest, Reason: "unknown_association"}
	for _, platform := range []string{service.PlatformAnthropic, service.PlatformAntigravity} {
		h := &GatewayHandler{}
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil).
			WithContext(context.WithValue(context.Background(), ctxkey.RequestID, m3fRequestID))
		account := &service.Account{ID: 781020, Platform: platform}

		wrote := h.writeCCCursorUserError(c, zap.NewNop(), account, err, false)
		require.False(t, wrote)
		require.True(t, h.ensureForwardErrorResponse(c, false))

		require.Equal(t, http.StatusBadGateway, rec.Code)
		require.Contains(t, rec.Body.String(), infraerrors.UpstreamRequestFailed)
	}
}
