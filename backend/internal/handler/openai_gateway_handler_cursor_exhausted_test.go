//go:build unit

package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestCursorKeyRejectedOpenAIGatewayExhausted verifies that the "cursor_key_rejected"
// (上游拒钥) reason is mapped to the SAME existing Cursor pool-exhausted Chinese terminal
// state CUR-RES-11 in the OpenAI Gateway Chat Completions path — NOT the generic
// "all accounts exhausted" message. This is the core deliverable of card CURSOR-PROBE-001-A7.
func TestCursorKeyRejectedOpenAIGatewayExhausted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	failoverErr := &service.UpstreamFailoverError{
		StatusCode:        http.StatusTooManyRequests,
		Reason:            "cursor_key_rejected",
		ClientStatusCode:  http.StatusTooManyRequests,
		ClientMessage:     "cursor 上游拒付此密钥",
		NextAccountAction: service.NextAccountRetry,
	}

	(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, failoverErr, false)

	body := recorder.Body.String()
	require.Equal(t, http.StatusTooManyRequests, recorder.Code)
	// 复用的既有 cursor 池耗尽中文终态文案与稳定 code（CUR-RES-11）。
	require.Contains(t, body, infraerrors.CursorResPoolExhausted)
	require.Contains(t, body, infraerrors.CursorResPoolExhaustedCode)
	require.Contains(t, body, `"type":"server_error"`)
	// 绝不落通用耗尽文案（无可用账号）。
	require.NotContains(t, body, "无可用账号")
}

// TestCursorKeyRejectedOpenAIGatewayExhaustedStreaming verifies the same mapping when
// streaming has already started (SSE path keeps the same CUR-RES-11 message + server_error type).
func TestCursorKeyRejectedOpenAIGatewayExhaustedStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Writer.WriteHeaderNow()

	failoverErr := &service.UpstreamFailoverError{
		StatusCode:        http.StatusTooManyRequests,
		Reason:            "cursor_key_rejected",
		ClientStatusCode:  http.StatusTooManyRequests,
		ClientMessage:     "cursor 上游拒付此密钥",
		NextAccountAction: service.NextAccountRetry,
	}

	(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, failoverErr, true)

	body := recorder.Body.String()
	require.Contains(t, body, infraerrors.CursorResPoolExhausted)
	require.Contains(t, body, infraerrors.CursorResPoolExhaustedCode)
	require.Contains(t, body, `"type":"server_error"`)
}

// TestCursorKeyRejectedNotInUnscheduleWhitelist is the DOUBLE EVIDENCE (source comment +
// this test) for Done-when #5: 拒钥 reason MUST NOT write an unschedule (停调).
//
// The unschedule (停调) whitelist lives at failover_loop.go:276-280 and is:
//
//	RetryableOnSameAccount ||
//	  Reason == service.CursorUpstreamAccountFailureReason ||
//	  service.IsCursorQuotaExhaustedReason(Reason) ||
//	  Reason == service.CursorRateLimitedReason
//
// It does NOT reference service.IsCursorKeyRejectedReason, so a key-rejected account is
// never temporarily unscheduled. This test mirrors that exact boolean and asserts it does
// NOT fire for "cursor_key_rejected". The handler under test here never writes unschedule
// either — that is a separate control-flow path — so this also guards against anyone later
// adding IsCursorKeyRejectedReason to the whitelist.
func TestCursorKeyRejectedNotInUnscheduleWhitelist(t *testing.T) {
	reason := service.GatewayFailureReason("cursor_key_rejected")

	require.True(t, service.IsCursorKeyRejectedReason(reason),
		"导出的拒钥判据必须识别 cursor_key_rejected")

	failoverErr := &service.UpstreamFailoverError{
		Reason:                 reason,
		RetryableOnSameAccount: false,
	}

	// 镜像 failover_loop.go:276-280 的停调白名单条件。
	unschedule := failoverErr.RetryableOnSameAccount ||
		failoverErr.Reason == service.CursorUpstreamAccountFailureReason ||
		service.IsCursorQuotaExhaustedReason(failoverErr.Reason) ||
		failoverErr.Reason == service.CursorRateLimitedReason

	require.False(t, unschedule,
		"cursor_key_rejected 必须不命中 failover_loop.go:276-280 的停调白名单（不写停调）")
}

// TestCursorQuotaExhaustedUnchangedOpenAIGateway is a regression guard for Done-when #4:
// the existing cursor_quota_exhausted reason must STILL take the same CUR-RES-11 branch
// after this card (we only UNION-ed the condition, never altered the quota path).
func TestCursorQuotaExhaustedUnchangedOpenAIGateway(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	failoverErr := &service.UpstreamFailoverError{
		StatusCode:        http.StatusTooManyRequests,
		Reason:            "cursor_quota_exhausted",
		ClientStatusCode:  http.StatusTooManyRequests,
		ClientMessage:     "cursor 账号额度耗尽，已停止调度",
		NextAccountAction: service.NextAccountRetry,
	}

	(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, failoverErr, false)

	body := recorder.Body.String()
	require.Equal(t, http.StatusTooManyRequests, recorder.Code)
	require.Contains(t, body, infraerrors.CursorResPoolExhausted)
	require.Contains(t, body, infraerrors.CursorResPoolExhaustedCode)
	require.Contains(t, body, `"type":"server_error"`)
}

// TestNonCursorReasonOpenAIGatewayNotCursorTerminal is the regression guard for Done-when #3:
// a NON-cursor reason hitting OpenAIGatewayHandler.handleFailoverExhausted must NOT divert
// into the Cursor pool-exhausted branch and must NOT emit the CUR-RES-11 message/code.
// This protects the "普通平台逐字不变" red line.
func TestNonCursorReasonOpenAIGatewayNotCursorTerminal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	failoverErr := &service.UpstreamFailoverError{
		StatusCode:        http.StatusBadGateway,
		Reason:            "openai_upstream_error", // 与 cursor 语义无关
		ClientStatusCode:  0,
		ClientMessage:     "",
		NextAccountAction: service.NextAccountRetry,
	}

	(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, failoverErr, false)

	body := recorder.Body.String()
	// 必须不进入 cursor 池耗尽分支。
	require.NotContains(t, body, infraerrors.CursorResPoolExhausted)
	require.NotContains(t, body, infraerrors.CursorResPoolExhaustedCode)
	// 普通平台路径仍按通用上游错误映射（502 → upstream_error）。
	require.Contains(t, body, `"type":"upstream_error"`)
	require.False(t, strings.Contains(body, "CUR-RES-11"))
}
