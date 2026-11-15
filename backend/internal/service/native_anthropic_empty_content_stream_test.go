//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 「上游 200 但空白回答」的客户链路回归。
//
// 背景：984（讯飞 spark-x2.6）在思考把输出预算吃光后会正常收流（message_stop）、
// 但不吐任何正文。旧行为是把这个空流当成功透传给客户 —— 客户看到「200 + 空白」，
// 既不知道怎么错的、也没法重试。正确行为是在客户还没收到任何字节时判成上游空
// 回答，冷却该账号并换号重试（客户无感）。

func thinkingOnlyStreamResponse(stopReason string) *http.Response {
	sse := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"spark-x2.6","content":[],"stop_reason":null,"usage":{"input_tokens":6169,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"这份文档很长，我需要先梳理结构"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"` + stopReason + `"},"usage":{"output_tokens":900}}

event: message_stop
data: {"type":"message_stop"}

`
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}
}

func emptyBufferedAnthropicResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"msg_1","type":"message","role":"assistant","model":"spark-x2.6",` +
				`"content":[],"stop_reason":"end_turn","usage":{"input_tokens":6169,"output_tokens":3}}`,
		)),
	}
}

func nativeEmptyContentTestContext(path string, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, recorder
}

func streamMessagesBody() []byte {
	return []byte(`{"model":"spark-x2.6","max_tokens":1024,"stream":true,` +
		`"messages":[{"role":"user","content":"hi"}]}`)
}

// 只有思考、没有正文 → 必须判成上游空回答：返回可换号的 failover 错误，
// 且客户端一个字节都没收到（否则上层不允许换号）。
func TestNativeAnthropicStreamEmptyContentTriggersFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := streamMessagesBody()
	upstream := &httpUpstreamRecorder{resp: thinkingOnlyStreamResponse("end_turn")}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
	c, recorder := nativeEmptyContentTestContext("/v1/messages", body)

	_, err := svc.ForwardAsAnthropic(context.Background(), c, nativeAnthropicTestAccount(), body, "", "")
	require.Error(t, err)

	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr), "应为可换号的 UpstreamFailoverError，实际：%v", err)
	require.True(t, failoverErr.SafeToFailoverAfterWrite, "客户端未收到语义字节，允许同流换号")
	require.Equal(t, 0, recorder.Body.Len(), "空白回答不得写出任何字节给客户端")
}

// stop_reason=max_tokens 时的空正文是客户端输出预算不足的预期截断，不是账号故障：
// 不得换号、不得冷却账号，按原语义透传。
func TestNativeAnthropicStreamEmptyContentWithMaxTokensPassesThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := streamMessagesBody()
	upstream := &httpUpstreamRecorder{resp: thinkingOnlyStreamResponse("max_tokens")}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
	c, recorder := nativeEmptyContentTestContext("/v1/messages", body)

	_, err := svc.ForwardAsAnthropic(context.Background(), c, nativeAnthropicTestAccount(), body, "", "")
	require.NoError(t, err, "max_tokens 截断不是账号故障，不应换号")
	require.Contains(t, recorder.Body.String(), "message_stop")
}

// 非流式：content 是空数组 → 同样判空回答并换号，且不把空 JSON 写出去。
func TestNativeAnthropicBufferedEmptyContentTriggersFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"spark-x2.6","max_tokens":1024,"stream":false,` +
		`"messages":[{"role":"user","content":"hi"}]}`)
	upstream := &httpUpstreamRecorder{resp: emptyBufferedAnthropicResponse()}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
	c, recorder := nativeEmptyContentTestContext("/v1/messages", body)

	_, err := svc.ForwardAsAnthropic(context.Background(), c, nativeAnthropicTestAccount(), body, "", "")
	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr), "应为可换号的 UpstreamFailoverError，实际：%v", err)
	require.Equal(t, 0, recorder.Body.Len(), "空白回答不得写出任何字节给客户端")
}

// 有正文的普通流不得被闸门误杀（防回归）。
func TestNativeAnthropicStreamWithTextNotTreatedAsEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := streamMessagesBody()
	upstream := &httpUpstreamRecorder{resp: nativeAnthropicStreamResponse()}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
	c, recorder := nativeEmptyContentTestContext("/v1/messages", body)

	_, err := svc.ForwardAsAnthropic(context.Background(), c, nativeAnthropicTestAccount(), body, "", "")
	require.NoError(t, err)
	require.Contains(t, recorder.Body.String(), "pong")
}
