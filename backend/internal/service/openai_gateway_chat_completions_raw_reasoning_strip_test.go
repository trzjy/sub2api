//go:build unit

package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 思考过程剥离（strip_reasoning_content）单测：镜像 raw_identity_mask_test.go 的
// 夹具与三路径写出点断言风格。覆盖 helper 解析、两个 Strip 函数、raw 路径三写出点。

func newReasoningStrippedTestAccount(id int64, modelMapping map[string]any) *Account {
	return &Account{
		ID:          id,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"model_mapping": modelMapping},
		Extra:       map[string]any{"strip_reasoning_content": true},
	}
}

// --- IsReasoningStrippedAccount 解析 ---

func TestIsReasoningStrippedAccount_Parsing(t *testing.T) {
	require.False(t, IsReasoningStrippedAccount(nil))
	require.False(t, IsReasoningStrippedAccount(&Account{Extra: nil}))
	require.False(t, IsReasoningStrippedAccount(&Account{Extra: map[string]any{}}))
	require.True(t, IsReasoningStrippedAccount(&Account{Extra: map[string]any{"strip_reasoning_content": true}}))
	require.False(t, IsReasoningStrippedAccount(&Account{Extra: map[string]any{"strip_reasoning_content": false}}))
	// 字符串宽容解析
	require.True(t, IsReasoningStrippedAccount(&Account{Extra: map[string]any{"strip_reasoning_content": "true"}}))
	require.True(t, IsReasoningStrippedAccount(&Account{Extra: map[string]any{"strip_reasoning_content": "1"}}))
	require.False(t, IsReasoningStrippedAccount(&Account{Extra: map[string]any{"strip_reasoning_content": "false"}}))
	// 其他类型视为关闭（零影响）
	require.False(t, IsReasoningStrippedAccount(&Account{Extra: map[string]any{"strip_reasoning_content": 1}}))
	require.False(t, IsReasoningStrippedAccount(&Account{Extra: map[string]any{"strip_reasoning_content": []any{}}}))
}

// --- StripReasoningFromSSELine ---

func TestStripReasoningFromSSELine(t *testing.T) {
	// 纯思考 chunk：delta 只有 reasoning_content → 删除后 delta 为空 {}
	line := `data: {"model":"qwen3.8-flash","choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}`
	out := StripReasoningFromSSELine(line)
	require.NotContains(t, out, "think", "reasoning_content 必须被删除")
	require.Equal(t, "{}", gjson.Get(out, "choices.0.delta").Raw, "删除后 delta 应为空")

	// delta 同时有 content 与 reasoning_content → 只删 reasoning，content 保留
	line2 := `data: {"model":"qwen3.8-flash","choices":[{"index":0,"delta":{"content":"hi","reasoning_content":"think"}}]}`
	out2 := StripReasoningFromSSELine(line2)
	require.NotContains(t, out2, "think")
	require.Equal(t, "hi", gjson.Get(out2, "choices.0.delta.content").String())

	// 无 reasoning_content → 原样返回
	line3 := `data: {"model":"qwen3.8-flash","choices":[{"index":0,"delta":{"content":"hi"}}]}`
	require.Equal(t, line3, StripReasoningFromSSELine(line3))
}

// --- StripReasoningFromResponseBody ---

func TestStripReasoningFromResponseBody(t *testing.T) {
	body := []byte(`{"model":"qwen3.8-flash","choices":[{"index":0,"message":{"role":"assistant","content":"hi","reasoning_content":"thought"}}],"usage":{"total_tokens":5}}`)
	out := StripReasoningFromResponseBody(body)
	require.NotContains(t, string(out), "thought", "message.reasoning_content 必须被删除")
	require.Equal(t, "hi", gjson.GetBytes(out, "choices.0.message.content").String())
	require.Equal(t, int64(5), gjson.GetBytes(out, "usage.total_tokens").Int())

	// 无 reasoning_content → 原样返回
	plain := []byte(`{"choices":[{"message":{"content":"hi"}}]}`)
	require.Equal(t, string(plain), string(StripReasoningFromResponseBody(plain)))
}

// --- 用例：开启账号 非流式写出点剥除 ---

func TestRawChatCompletions_StrippedNonStreamingRemovesReasoning(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	const upstreamJSON = `{"id":"cmpl_1","object":"chat.completion","model":"kimi-k3","created":1,"choices":[{"index":0,"message":{"role":"assistant","content":"hi","reasoning_content":"thought"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(upstreamJSON)),
	}

	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
	account := newReasoningStrippedTestAccount(249, map[string]any{"kimi-k3": "kimi-k3"})

	_, err := svc.bufferRawChatCompletions(c, resp, account, "kimi-k3", "kimi-k3", "kimi-k3", nil, nil, time.Now())
	require.NoError(t, err)

	downstream := rec.Body.String()
	require.Empty(t, gjson.Get(downstream, "choices.0.message.reasoning_content").String(), "开启账号 content.reasoning_content 必须为空")
	require.Equal(t, "hi", gjson.Get(downstream, "choices.0.message.content").String(), "content 必须保留")
	require.Equal(t, int64(5), gjson.Get(downstream, "usage.total_tokens").Int(), "usage 必须保留")
}

// --- 用例：开启账号 流式写出点剥除 ---

func TestRawChatCompletions_StrippedStreamingRemovesReasoning(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	upstreamBody := strings.Join([]string{
		`data: {"id":"cmpl_1","object":"chat.completion.chunk","model":"kimi-k3","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		"",
		`data: {"id":"cmpl_1","object":"chat.completion.chunk","model":"kimi-k3","choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}`,
		"",
		`data: {"id":"cmpl_1","object":"chat.completion.chunk","model":"kimi-k3","choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		"",
		`data: {"id":"cmpl_1","object":"chat.completion.chunk","model":"kimi-k3","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}

	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
	account := newReasoningStrippedTestAccount(249, map[string]any{"kimi-k3": "kimi-k3"})

	_, err := svc.streamRawChatCompletions(c, resp, account, "kimi-k3", "kimi-k3", "kimi-k3", nil, nil, time.Now(), 0)
	require.NoError(t, err)

	downstream := rec.Body.String()
	require.Contains(t, downstream, "data: [DONE]")
	for _, line := range strings.Split(downstream, "\n") {
		payload, ok := extractOpenAISSEDataLine(line)
		if !ok {
			continue
		}
		trimmed := strings.TrimSpace(payload)
		if trimmed == "" || trimmed == "[DONE]" {
			continue
		}
		require.Empty(t, gjson.Get(payload, "choices.0.delta.reasoning_content").String(),
			"流式每帧 delta.reasoning_content 必须为空: %s", line)
	}
	// 纯思考帧删除后 delta 应为空 {}，但 content 帧保留
	require.Contains(t, downstream, `"content":"hi"`)
}

// --- 用例：开启账号 converted 聚合写出点剥除 ---

func TestRawChatCompletions_StrippedConvertedAggregateRemovesReasoning(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	upstreamBody := strings.Join([]string{
		`data: {"id":"cvt_1","object":"chat.completion.chunk","model":"kimi-k3","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		"",
		`data: {"id":"cvt_1","object":"chat.completion.chunk","model":"kimi-k3","choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}`,
		"",
		`data: {"id":"cvt_1","object":"chat.completion.chunk","model":"kimi-k3","choices":[{"index":0,"delta":{"content":"hello world"}}]}`,
		"",
		`data: {"id":"cvt_1","object":"chat.completion.chunk","model":"kimi-k3","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"",
		`data: {"id":"cvt_1","object":"chat.completion.chunk","model":"kimi-k3","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}

	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
	account := newReasoningStrippedTestAccount(249, map[string]any{"kimi-k3": "kimi-k3"})

	_, err := svc.convertedChatCompletionsAsRawCC(c, resp, account, "kimi-k3", "kimi-k3", "kimi-k3", nil, nil, time.Now())
	require.NoError(t, err)

	downstream := rec.Body.String()
	require.Empty(t, gjson.Get(downstream, "choices.0.message.reasoning_content").String(), "聚合体 reasoning_content 必须为空")
	require.Equal(t, "hello world", gjson.Get(downstream, "choices.0.message.content").String(), "content 必须保留")
	require.Equal(t, int64(5), gjson.Get(downstream, "usage.total_tokens").Int())
}

// --- 用例：未开启账号 三路径不剥除（零影响回归） ---

func TestRawChatCompletions_PlainAccountKeepsReasoning(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := newPlainTestAccount(249, map[string]any{"kimi-k3": "kimi-k3"})

	const upstreamJSON = `{"choices":[{"message":{"role":"assistant","content":"hi","reasoning_content":"thought"}}],"usage":{"total_tokens":5}}`

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(upstreamJSON)),
	}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
	_, err := svc.bufferRawChatCompletions(c, resp, account, "kimi-k3", "kimi-k3", "kimi-k3", nil, nil, time.Now())
	require.NoError(t, err)
	require.Equal(t, "thought", gjson.Get(rec.Body.String(), "choices.0.message.reasoning_content").String(),
		"未开启账号必须保留 reasoning_content")
}
