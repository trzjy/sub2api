//go:build unit

package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// messagesOversizeTestBody 构造长度精确为 totalBytes 的 /v1/messages 入站 body
// （填充在 user content 里，模拟长会话历史被一次性带回）。
func messagesOversizeTestBody(t *testing.T, totalBytes int) []byte {
	t.Helper()
	const prefix = `{"model":"gpt-5.4","max_tokens":32,"messages":[{"role":"user","content":"`
	const suffix = `"}],"stream":false}`
	padLen := totalBytes - len(prefix) - len(suffix)
	require.Greater(t, padLen, 0, "padding must stay positive")
	body := prefix + strings.Repeat("a", padLen) + suffix
	require.Len(t, body, totalBytes)
	return []byte(body)
}

func messagesSmallTestBody() []byte {
	return []byte(`{"model":"gpt-5.4","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
}

func messagesCompletedSSEUpstream() *http.Response {
	payload := strings.Join([]string{
		`data: {"type":"response.completed","response":{"id":"resp_oversize_guard","object":"response","model":"gpt-5.4","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(payload)),
	}
}

// 用例 1（A）：body > 5 MiB 时不转发上游，直接返回 400 + 中文文案。
func TestForwardAsAnthropicMessages_OversizeBodyRejectedBeforeUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := messagesOversizeTestBody(t, messagesOversizeRequestMaxBytes+16)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: messagesCompletedSSEUpstream()}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}

	_, err := svc.ForwardAsAnthropic(context.Background(), c, rawChatCompletionsTestAccount(), body, "", "")

	require.Error(t, err)
	require.Empty(t, upstream.requests, "oversize request must be rejected before any upstream call")
	require.Empty(t, upstream.bodies, "oversize request must not build an upstream body")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "invalid_request_error", gjson.Get(rec.Body.String(), "error.type").String())
	require.Equal(t, messagesOversizeClientMessage, gjson.Get(rec.Body.String(), "error.message").String())
}

// 用例 1 补（A 边界）：刚好等于 5 MiB 不触发预检（阈值是严格大于）。
func TestForwardAsAnthropicMessages_BodyExactlyAtLimitIsNotRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := messagesOversizeTestBody(t, messagesOversizeRequestMaxBytes)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: messagesCompletedSSEUpstream()}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}

	_, err := svc.ForwardAsAnthropic(context.Background(), c, rawChatCompletionsTestAccount(), body, "", "")

	require.NoError(t, err)
	require.Len(t, upstream.requests, 1, "body at the limit must still be forwarded")
}

// 用例 2（B）：上游 4xx 且未给出任何可读 message 时，客户端收到中文文案，
// 状态码与错误类型语义不变。
func TestForwardAsAnthropicMessages_UpstreamBare4xxUsesOversizeClientMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := messagesSmallTestBody()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	// 上游只回状态码，无任何 error.message（生产实证的裸 400 形态）。
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{}`)),
	}}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}

	_, err := svc.ForwardAsAnthropic(context.Background(), c, rawChatCompletionsTestAccount(), body, "", "")

	require.Error(t, err)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, http.StatusBadRequest, rec.Code, "status code semantics must stay unchanged")
	require.Equal(t, "invalid_request_error", gjson.Get(rec.Body.String(), "error.type").String())
	require.Equal(t, messagesOversizeClientMessage, gjson.Get(rec.Body.String(), "error.message").String())
	require.NotContains(t, rec.Body.String(), "Upstream error: 400")
	require.Contains(t, err.Error(), "upstream error: 400", "internal error must keep the upstream status")
}

// 用例 2 补（B）：上游给出了可读 message 的 4xx 必须原样透传，不能被兜底文案覆盖。
func TestForwardAsAnthropicMessages_UpstreamReadable4xxKeepsUpstreamMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := messagesSmallTestBody()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"max_tokens is too large"}}`)),
	}}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}

	_, err := svc.ForwardAsAnthropic(context.Background(), c, rawChatCompletionsTestAccount(), body, "", "")

	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "max_tokens is too large", gjson.Get(rec.Body.String(), "error.message").String())
}

// 用例 3（不误杀）：普通（< 5 MiB）请求行为零变化——照常转发上游并正常返回。
func TestForwardAsAnthropicMessages_NormalSizeBodyStillForwardsToUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := messagesSmallTestBody()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: messagesCompletedSSEUpstream()}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, rawChatCompletionsTestAccount(), body, "", "")

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, "resp_oversize_guard", result.ResponseID)
	require.Contains(t, rec.Body.String(), "ok")
	require.NotContains(t, rec.Body.String(), messagesOversizeClientMessage)
}

// 用例 4：A 与 B 共用同一文案常量，且文案满足「说清发生了什么 + 可执行动作 + 无内部术语」。
func TestMessagesOversizeClientMessageSharedByPrecheckAndUpstreamFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)

	run := func(body []byte, upstreamResp *http.Response) string {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		svc := &OpenAIGatewayService{
			cfg:          rawChatCompletionsTestConfig(),
			httpUpstream: &httpUpstreamRecorder{resp: upstreamResp},
		}
		_, err := svc.ForwardAsAnthropic(context.Background(), c, rawChatCompletionsTestAccount(), body, "", "")
		require.Error(t, err)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		return gjson.Get(rec.Body.String(), "error.message").String()
	}

	precheckMsg := run(messagesOversizeTestBody(t, messagesOversizeRequestMaxBytes+16), messagesCompletedSSEUpstream())
	fallbackMsg := run(messagesSmallTestBody(), &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{}`)),
	})

	require.Equal(t, messagesOversizeClientMessage, precheckMsg, "A must use the shared constant")
	require.Equal(t, messagesOversizeClientMessage, fallbackMsg, "B must use the shared constant")
	require.Equal(t, precheckMsg, fallbackMsg, "A and B must emit the identical message")

	// 文案自检：原因 + 可执行动作
	require.Contains(t, messagesOversizeClientMessage, "请求体过大")
	require.Contains(t, messagesOversizeClientMessage, "125 万 token 上限")
	require.Contains(t, messagesOversizeClientMessage, "新建一个会话")
	require.Contains(t, messagesOversizeClientMessage, "清理/压缩较早的历史消息")
	require.Contains(t, messagesOversizeClientMessage, "分批提交")
	// 文案自检：不出现内部术语
	for _, term := range []string{"账号", "分组", "中转", "节点", "ID", "account", "group"} {
		require.NotContains(t, messagesOversizeClientMessage, term)
	}
}

// 回归用例（B 收窄）：直接调用 handleCompatErrorResponse，验证兜底文案只对上游裸 400 生效。
// 其它 4xx（429 限流 / 401 鉴权 / 403 / 404）即便返回空 message，也不得套用「请求体过大」
// 文案——否则会把限流/鉴权问题误导成上下文超限，让用户去清会话。
// 注意：走完整 ForwardAsAnthropic 时 429/401/403 会先进入 failover 路径而非此分支，
// 故此处直接命中 B 分支做断言，与生产非 failover 终写路径一致。
func TestHandleCompatErrorResponse_Non400EmptyMsgKeepsUpstreamError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name   string
		status int
	}{
		{"400_bare", http.StatusBadRequest},
		{"429_rate_limit", http.StatusTooManyRequests},
		{"401_unauthorized", http.StatusUnauthorized},
		{"404_not_found", http.StatusNotFound},
		{"403_forbidden", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

			// 上游回对应 4xx 但 body 为空（无可读 message）——正是 B 收窄前会误套文案的形态。
			resp := &http.Response{
				StatusCode: tc.status,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{}`)),
			}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}

			_, err := svc.handleCompatErrorResponse(resp, c, rawChatCompletionsTestAccount(), writeAnthropicError)

			require.Error(t, err)
			require.Equal(t, tc.status, rec.Code, "status code semantics must stay unchanged")
			msg := gjson.Get(rec.Body.String(), "error.message").String()
			if tc.status == http.StatusBadRequest {
				require.Equal(t, messagesOversizeClientMessage, msg, "bare 400 must use the oversize hint")
			} else {
				require.NotEqual(t, messagesOversizeClientMessage, msg,
					"non-400 4xx must not be rewritten to the oversize hint")
				require.Equal(t, fmt.Sprintf("Upstream error: %d", tc.status), msg,
					"non-400 4xx with empty message keeps the generic upstream message")
			}
		})
	}
}
