//go:build unit

package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestCursorQuotaExhaustedChatCompletions verifies that cursor_quota_exhausted
// returns CUR-RES-11 (Chinese pool-exhausted terminal message) in Chat Completions protocol.
func TestCursorQuotaExhaustedChatCompletions(t *testing.T) {
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

	(&GatewayHandler{}).handleCCFailoverExhausted(c, failoverErr, false)

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.Contains(t, recorder.Body.String(), infraerrors.CursorResPoolExhausted)
	require.Contains(t, recorder.Body.String(), `"type":"server_error"`)
}

// TestCursorQuotaExhaustedChatCompletionsDefaultsTo429 verifies that
// cursor_quota_exhausted defaults to 429 when ClientStatusCode is unset.
func TestCursorQuotaExhaustedChatCompletionsDefaultsTo429(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	failoverErr := &service.UpstreamFailoverError{
		StatusCode:        http.StatusBadGateway,
		Reason:            "cursor_quota_exhausted",
		ClientStatusCode:  0,
		ClientMessage:     "cursor 账号额度耗尽，已停止调度",
		NextAccountAction: service.NextAccountRetry,
	}

	(&GatewayHandler{}).handleCCFailoverExhausted(c, failoverErr, false)

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.Contains(t, recorder.Body.String(), infraerrors.CursorResPoolExhausted)
}

// TestCursorQuotaExhaustedResponses verifies that cursor_quota_exhausted
// returns CUR-RES-11 in Responses protocol.
func TestCursorQuotaExhaustedResponses(t *testing.T) {
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

	(&GatewayHandler{}).handleResponsesFailoverExhausted(c, failoverErr, false)

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.Contains(t, recorder.Body.String(), infraerrors.CursorResPoolExhausted)
	require.Contains(t, recorder.Body.String(), `"code":"server_error"`)
}

// TestCursorQuotaExhaustedResponsesDefaultsTo429 verifies that
// cursor_quota_exhausted defaults to 429 when ClientStatusCode is unset.
func TestCursorQuotaExhaustedResponsesDefaultsTo429(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	failoverErr := &service.UpstreamFailoverError{
		StatusCode:        http.StatusBadGateway,
		Reason:            "cursor_quota_exhausted",
		ClientStatusCode:  0,
		ClientMessage:     "cursor 账号额度耗尽，已停止调度",
		NextAccountAction: service.NextAccountRetry,
	}

	(&GatewayHandler{}).handleResponsesFailoverExhausted(c, failoverErr, false)

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.Contains(t, recorder.Body.String(), infraerrors.CursorResPoolExhausted)
}

// TestCursorQuotaExhaustedOpenAIGateway verifies that cursor_quota_exhausted
// returns CUR-RES-11 in OpenAI Gateway Messages protocol.
func TestCursorQuotaExhaustedOpenAIGateway(t *testing.T) {
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

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.Contains(t, recorder.Body.String(), infraerrors.CursorResPoolExhausted)
	require.Contains(t, recorder.Body.String(), `"type":"server_error"`)
}

// TestCursorQuotaExhaustedOpenAIGatewayDefaultsTo429 verifies that
// cursor_quota_exhausted defaults to 429 when ClientStatusCode is unset.
func TestCursorQuotaExhaustedOpenAIGatewayDefaultsTo429(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	failoverErr := &service.UpstreamFailoverError{
		StatusCode:        http.StatusBadGateway,
		Reason:            "cursor_quota_exhausted",
		ClientStatusCode:  0,
		ClientMessage:     "cursor 账号额度耗尽，已停止调度",
		NextAccountAction: service.NextAccountRetry,
	}

	(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, failoverErr, false)

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.Contains(t, recorder.Body.String(), infraerrors.CursorResPoolExhausted)
}

// TestCursorQuotaExhaustedResponsesStreaming verifies that cursor_quota_exhausted
// emits CUR-RES-11 when streaming has started in Responses protocol.
func TestCursorQuotaExhaustedResponsesStreaming(t *testing.T) {
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

	(&GatewayHandler{}).handleResponsesFailoverExhausted(c, failoverErr, true)

	require.Contains(t, recorder.Body.String(), infraerrors.CursorResPoolExhausted)
	require.Contains(t, recorder.Body.String(), "event: response.failed")
}

// TestCursorQuotaExhaustedOpenAIGatewayStreaming verifies that cursor_quota_exhausted
// emits CUR-RES-11 when streaming has started in OpenAI Gateway protocol.
func TestCursorQuotaExhaustedOpenAIGatewayStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Writer.WriteHeaderNow()

	failoverErr := &service.UpstreamFailoverError{
		StatusCode:        http.StatusTooManyRequests,
		Reason:            "cursor_quota_exhausted",
		ClientStatusCode:  http.StatusTooManyRequests,
		ClientMessage:     "cursor 账号额度耗尽，已停止调度",
		NextAccountAction: service.NextAccountRetry,
	}

	(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, failoverErr, true)

	require.Contains(t, recorder.Body.String(), infraerrors.CursorResPoolExhausted)
	require.Contains(t, recorder.Body.String(), `"type":"server_error"`)
}
