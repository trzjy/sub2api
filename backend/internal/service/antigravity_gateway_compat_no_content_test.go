package service

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const antigravityCompatNoContentEvents = "event: message_start\n" +
	"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"gemini-3.8-flash\",\"usage\":{\"input_tokens\":8,\"output_tokens\":0}}}\n\n" +
	"event: message_stop\n" +
	"data: {\"type\":\"message_stop\"}\n\n"

func newAntigravityCompatNoContentSession(t *testing.T) (*gin.Context, *httptest.ResponseRecorder, *antigravityCompatStreamSession) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	writer := newAntigravityClientWriter(c.Writer, recorder, "test")
	session := newAntigravityCompatStreamSession(
		"gemini-3.8-flash",
		time.Now(),
		newAntigravityChatStreamAdapter("gemini-3.8-flash", false),
		writer,
	)
	return c, recorder, session
}

// 上游返回合法的结构化 SSE 事件但没有文本正文时，必须按协议正常收尾，
// 不能当作账号故障触发 failover（否则健康账号会被短停调，连 /v1/messages 一起 503）。
func TestAntigravityCompatStructuredButTextlessStreamIsNotFailover(t *testing.T) {
	c, recorder, session := newAntigravityCompatNoContentSession(t)
	session.observedLines = 4
	session.consumeClaudeEvents([]byte(antigravityCompatNoContentEvents))

	require.True(t, session.sawStructuredEvent)
	require.False(t, session.hasMeaningfulData())

	result, err := handleAntigravityCompatEmptyStream(c, session)
	require.NoError(t, err)
	require.NotNil(t, result)

	body := recorder.Body.String()
	require.Contains(t, body, "data:")
	require.Contains(t, body, "[DONE]")
	require.NotContains(t, body, "empty_stream")
}

// 读流中断但已经收到结构化事件时，同样按正常收尾处理。
func TestAntigravityCompatReadErrorWithStructuredEventsIsNotFailover(t *testing.T) {
	svc := &AntigravityGatewayService{}
	c, recorder, session := newAntigravityCompatNoContentSession(t)
	session.consumeClaudeEvents([]byte(antigravityCompatNoContentEvents))

	result, err := svc.handleAntigravityCompatReadError(
		c, session, errors.New("upstream closed early"), defaultMaxLineSize, "test",
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Contains(t, recorder.Body.String(), "[DONE]")
}

// 上游一行有效数据都没给时，仍必须保留原有的 failover 语义。
func TestAntigravityCompatTrulyEmptyStreamStillFailsOver(t *testing.T) {
	c, _, session := newAntigravityCompatNoContentSession(t)

	result, err := handleAntigravityCompatEmptyStream(c, session)
	require.Nil(t, result)
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
}
