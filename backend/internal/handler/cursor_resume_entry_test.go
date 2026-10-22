//go:build unit

package handler

// 本文件是 **§25.2 续轮识别在资源获取之前** 的 handler 侧定向测试（派发单 W2-E）。
//
// 覆盖：
//  1. 普通请求（无结果 ID、无 previous_response_id）不被误判为续轮，直接放行普通路径；
//  2. 续轮先定位再接管：识别失败（未知关联）在**任何资源获取之前**明确 400；
//  3. 合法续轮由会话接管：ServeCursorResume 被调用一次，handler 直接返回；
//  4. 回写通道缺失：明确 503，绝不伪造成功；
//  5. 协议分流：Chat/Messages/Responses/Gemini 各走自己的探针与状态码。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// cursorResumeFakeGateway 是 CursorResumeGateway 的替身（记录调用与返回预设）。
type cursorResumeFakeGateway struct {
	identifyCalls int
	serveCalls    int
	ticket        *service.CursorResumeTicket
	identifyErr   error
	serveErr      error
}

// IdentifyCursorResume 实现 service.CursorResumeGateway。
func (g *cursorResumeFakeGateway) IdentifyCursorResume(*service.CursorResumeProbe) (*service.CursorResumeTicket, error) {
	g.identifyCalls++
	if g.identifyErr != nil {
		return nil, g.identifyErr
	}
	return g.ticket, nil
}

// ServeCursorResume 实现 service.CursorResumeGateway。
func (g *cursorResumeFakeGateway) ServeCursorResume(
	context.Context, *gin.Context, *service.CursorResumeTicket, service.CursorResumeServeInput,
) (*service.ForwardResult, error) {
	g.serveCalls++
	if g.serveErr != nil {
		return nil, g.serveErr
	}
	return &service.ForwardResult{Model: "cursor-test-model"}, nil
}

// newCursorResumeTestContext 构造最小 gin 上下文（只用于承载请求 ctx）。
func newCursorResumeTestContext(t *testing.T) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request = req
	return c
}

// captureReject 记录 reject 回调的状态码（断言"拒绝发生在资源获取之前"）。
type captureReject struct {
	called bool
	status int
	msg    string
}

func (r *captureReject) fn() func(int, string) {
	return func(status int, message string) {
		r.called = true
		r.status = status
		r.msg = message
	}
}

const cursorResumeHandlerChatContinuation = `{
  "model": "cursor-test-model",
  "messages": [
    {"role": "user", "content": "weather please"},
    {"role": "assistant", "content": "", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{}"}}]},
    {"role": "tool", "tool_call_id": "call_1", "content": "sunny"}
  ]
}`

// TestServeCursorResumePlainRequestPassesThrough 覆盖 1：普通请求不被拦截。
func TestServeCursorResumePlainRequestPassesThrough(t *testing.T) {
	c := newCursorResumeTestContext(t)
	gw := &cursorResumeFakeGateway{}
	reject := &captureReject{}
	handled := serveCursorResumeIfRequested(c, gw, service.CursorResumeProtocolChat,
		[]byte(`{"model": "m", "messages": [{"role": "user", "content": "hi"}]}`),
		service.CursorSessionIdentity{UserID: 1}, false, reject.fn())
	require.False(t, handled, "普通请求必须继续走原路径")
	require.False(t, reject.called)
	require.Zero(t, gw.identifyCalls, "无结果 ID 时不进入索引定位")
	require.Zero(t, gw.serveCalls)
	// 身份必须已挂到请求 ctx（首 HTTP 建会话时记录）。
	require.Equal(t, int64(1), service.CursorSessionIdentityFromContext(c.Request.Context()).UserID)
}

// TestServeCursorResumeUnknownAssociationRejectedBeforeResources 覆盖 2：识别失败先拒绝。
func TestServeCursorResumeUnknownAssociationRejectedBeforeResources(t *testing.T) {
	c := newCursorResumeTestContext(t)
	gw := &cursorResumeFakeGateway{
		identifyErr: &service.CursorResumeError{Status: http.StatusBadRequest, Reason: "unknown_association"},
	}
	reject := &captureReject{}
	handled := serveCursorResumeIfRequested(c, gw, service.CursorResumeProtocolChat,
		[]byte(cursorResumeHandlerChatContinuation), service.CursorSessionIdentity{UserID: 1}, false, reject.fn())
	require.True(t, handled, "识别失败必须由续轮分支终止请求")
	require.True(t, reject.called)
	require.Equal(t, http.StatusBadRequest, reject.status)
	require.Zero(t, gw.serveCalls, "识别失败不得进入接管/写入")
}

// TestServeCursorResumeServedBySession 覆盖 3：合法续轮由会话接管并直接返回。
func TestServeCursorResumeServedBySession(t *testing.T) {
	c := newCursorResumeTestContext(t)
	gw := &cursorResumeFakeGateway{ticket: &service.CursorResumeTicket{SessionID: "s-1", Model: "cursor-test-model"}}
	reject := &captureReject{}
	handled := serveCursorResumeIfRequested(c, gw, service.CursorResumeProtocolChat,
		[]byte(cursorResumeHandlerChatContinuation), service.CursorSessionIdentity{UserID: 1}, true, reject.fn())
	require.True(t, handled)
	require.False(t, reject.called)
	require.Equal(t, 1, gw.identifyCalls)
	require.Equal(t, 1, gw.serveCalls)
}

// TestServeCursorResumeIntakeUnavailable 覆盖 4：通道缺失明确 503（不伪造成功）。
func TestServeCursorResumeIntakeUnavailable(t *testing.T) {
	c := newCursorResumeTestContext(t)
	gw := &cursorResumeFakeGateway{
		ticket:   &service.CursorResumeTicket{SessionID: "s-1"},
		serveErr: &service.CursorResumeError{Status: http.StatusServiceUnavailable, Reason: "intake_unavailable"},
	}
	reject := &captureReject{}
	handled := serveCursorResumeIfRequested(c, gw, service.CursorResumeProtocolChat,
		[]byte(cursorResumeHandlerChatContinuation), service.CursorSessionIdentity{UserID: 1}, false, reject.fn())
	require.True(t, handled)
	require.True(t, reject.called)
	require.Equal(t, http.StatusServiceUnavailable, reject.status)
}

// TestServeCursorResumeProtocolRouting 覆盖 5：四协议各自走自己的探针。
func TestServeCursorResumeProtocolRouting(t *testing.T) {
	cases := []struct {
		name         string
		protocol     service.CursorResumeProtocol
		body         string
		wantProbeIDs int
	}{
		{
			name:         "chat",
			protocol:     service.CursorResumeProtocolChat,
			body:         cursorResumeHandlerChatContinuation,
			wantProbeIDs: 1,
		},
		{
			name:     "messages",
			protocol: service.CursorResumeProtocolMessages,
			body: `{
  "model": "m",
  "max_tokens": 8,
  "messages": [
    {"role": "user", "content": [{"type": "text", "text": "hi"}]},
    {"role": "assistant", "content": [{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {}}]},
    {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_1", "content": [{"type": "text", "text": "sunny"}]}]}
  ]
}`,
			wantProbeIDs: 1,
		},
		{
			name:     "responses",
			protocol: service.CursorResumeProtocolResponses,
			body: `{
  "model": "m",
  "previous_response_id": "resp_1",
  "input": [{"type": "function_call_output", "call_id": "call_1", "output": "sunny"}]
}`,
			wantProbeIDs: 1,
		},
		{
			name:     "gemini",
			protocol: service.CursorResumeProtocolGemini,
			body: `{
  "model": "m",
  "contents": [
    {"role": "user", "parts": [{"text": "hi"}]},
    {"role": "model", "parts": [{"functionCall": {"id": "call_1", "name": "get_weather", "args": {}}}]},
    {"role": "user", "parts": [{"functionResponse": {"id": "call_1", "name": "get_weather", "response": {"result": "sunny"}}}]}
  ]
}`,
			wantProbeIDs: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe, err := service.CursorProbeResumeRequest(tc.protocol, []byte(tc.body))
			require.NoError(t, err)
			require.NotNil(t, probe)
			require.Equal(t, tc.wantProbeIDs, len(probe.ResultCallIDs), "各协议必须各自识别本批结果 ID")
		})
	}
}
