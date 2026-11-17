//go:build unit

package handler

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestDevinClientFacingError 覆盖 devinClientFacingError 的映射边界：
// 仅 ErrDevinSystemImageUnsupported（含被包裹形态）映射为 400，其它错误不接管。
func TestDevinClientFacingError(t *testing.T) {
	t.Run("sentinel maps to 400", func(t *testing.T) {
		status, msg, ok := devinClientFacingError(service.ErrDevinSystemImageUnsupported)
		require.True(t, ok)
		require.Equal(t, http.StatusBadRequest, status)
		require.Equal(t, service.ErrDevinSystemImageUnsupported.Error(), msg)
	})

	t.Run("wrapped sentinel maps to 400", func(t *testing.T) {
		wrapped := fmt.Errorf("forward devin: %w", service.ErrDevinSystemImageUnsupported)
		status, msg, ok := devinClientFacingError(wrapped)
		require.True(t, ok)
		require.Equal(t, http.StatusBadRequest, status)
		require.Equal(t, wrapped.Error(), msg)
	})

	t.Run("unrelated error is not handled", func(t *testing.T) {
		status, msg, ok := devinClientFacingError(errors.New("some upstream error"))
		require.False(t, ok)
		require.Equal(t, 0, status)
		require.Empty(t, msg)
	})

	t.Run("nil error is not handled", func(t *testing.T) {
		status, msg, ok := devinClientFacingError(nil)
		require.False(t, ok)
		require.Equal(t, 0, status)
		require.Empty(t, msg)
	})
}

// newDevinCommittedCtx 造一个最小 gin 测试上下文；started=true 时用唯一事实源
// service.MarkResponseCommitted 标记响应已提交（等价于心跳已 flush SSE 200）。
func newDevinCommittedCtx(endpoint string, started bool) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, endpoint, nil)
	if started {
		service.MarkResponseCommitted(c)
	}
	return c, w
}

// TestDevinClientFacingErrorCommittedGoesToStream 外审 r28：四个入口命中 sentinel
// 且响应已提交时，必须走各 handler 的 committed-aware 出口把错误写进流内终止事件，
// 不得再写 400 JSON（那会破坏已固化的 SSE 200）。这里直接驱动各入口选用的出口函数，
// 锁死「未提交→400、已提交→流内」的分流契约。
func TestDevinClientFacingErrorCommittedGoesToStream(t *testing.T) {
	st, msg, ok := devinClientFacingError(service.ErrDevinSystemImageUnsupported)
	require.True(t, ok)
	require.Equal(t, http.StatusBadRequest, st)

	t.Run("gateway messages: not committed -> JSON 400", func(t *testing.T) {
		c, w := newDevinCommittedCtx(EndpointMessages, false)
		require.False(t, gatewayResponseCommitted(c, false))
		h := &GatewayHandler{}
		h.handleStreamingAwareError(c, st, "invalid_request_error", msg, false)
		require.Equal(t, http.StatusBadRequest, w.Code)
		require.Contains(t, w.Body.String(), `"type":"invalid_request_error"`)
	})

	t.Run("gateway messages: committed -> SSE error frame, status stays 200", func(t *testing.T) {
		c, w := newDevinCommittedCtx(EndpointMessages, true)
		require.True(t, gatewayResponseCommitted(c, false))
		h := &GatewayHandler{}
		h.handleStreamingAwareError(c, st, "invalid_request_error", msg, false)
		require.NotEqual(t, http.StatusBadRequest, w.Code, "已提交不得再写 400 JSON")
		require.Contains(t, w.Body.String(), `"type":"error"`)
		require.Contains(t, w.Body.String(), msg)
	})

	t.Run("gateway responses: committed -> response.failed", func(t *testing.T) {
		c, w := newDevinCommittedCtx(EndpointResponses, true)
		h := &GatewayHandler{}
		h.handleStreamingAwareError(c, st, "invalid_request_error", msg, false)
		require.Contains(t, w.Body.String(), "event: response.failed")
		require.Contains(t, w.Body.String(), msg)
	})

	t.Run("openai chat: committed -> in-stream error event", func(t *testing.T) {
		c, w := newDevinCommittedCtx(EndpointChatCompletions, true)
		require.True(t, gatewayResponseCommittedAsSSE(c, false))
		h := &OpenAIGatewayHandler{}
		h.handleStreamingAwareError(c, st, "invalid_request_error", msg, false)
		require.NotEqual(t, http.StatusBadRequest, w.Code, "已提交不得再写 400 JSON")
		require.Contains(t, w.Body.String(), "event: error")
		require.Contains(t, w.Body.String(), msg)
	})
}
