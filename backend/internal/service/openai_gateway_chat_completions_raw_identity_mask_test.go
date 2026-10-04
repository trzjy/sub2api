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

// qk-R1: kimi CC raw 路径（api_protocol=chat_completions）三个成功面写出点
// 接上掩码账号 model 回写。S1 只覆盖了协议转换路径（openai_gateway_response_handling.go），
// 但 kimi apikey 账号走 raw 路径，原样透传上游 body，导致客户端看到上游回显
// 模型名（生产实测：T1 非流式 / T2 流式 均泄漏 qwen3.8-flash）。本测试镜像
// openai_gateway_upstream_errors_identity_mask_test.go 的 newMaskedTestAccount
// 夹具风格，并对三个写出点逐一断言。

// --- 字节级对比辅助 ---

// sseBodyNormalized 把 SSE 体按行归一化（去掉末尾空行），使 scanner 重排后的
// 逐行写出与上游原始体在“内容字节”层等价可比，用于非掩码账号的零变化断言。
func sseBodyNormalized(s string) string {
	lines := strings.Split(s, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// --- 用例 1：掩码账号非流式：上游回显 model=qwen3.8-flash → 客户端体为 kimi-k3 ---

func TestRawChatCompletions_MaskedNonStreamingRewritesModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	const upstreamJSON = `{"id":"cmpl_1","object":"chat.completion","model":"qwen3.8-flash","created":1,"choices":[{"index":0,"message":{"role":"assistant","content":"hi","reasoning_content":"thought"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(upstreamJSON)),
	}

	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
	account := newMaskedTestAccount(187, map[string]any{"kimi-k3": "qwen3.8-flash"})

	result, err := svc.bufferRawChatCompletions(c, resp, account, "kimi-k3", "qwen3.8-flash", "qwen3.8-flash", nil, nil, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)

	downstream := rec.Body.String()
	// 回写：上游回显 qwen3.8-flash → 客户端看到 kimi-k3
	require.Equal(t, "kimi-k3", gjson.Get(downstream, "model").String(), "掩码回写后 model 应为客户端请求名")
	// reasoning_content / usage 不动
	require.Equal(t, "thought", gjson.Get(downstream, "choices.0.message.reasoning_content").String())
	require.Equal(t, "hi", gjson.Get(downstream, "choices.0.message.content").String())
	require.Equal(t, 5, int(gjson.Get(downstream, "usage.total_tokens").Int()))
	// 观察/计费链仍看原始回显（不改 observer 语义）
	require.Equal(t, "qwen3.8-flash", observedUpstreamResponseModel(c))
	require.Equal(t, "qwen3.8-flash", result.UpstreamModel)
}

// --- 用例 2：掩码账号流式：每帧 model 回写为 kimi-k3 ---

func TestRawChatCompletions_MaskedStreamingRewritesModelPerFrame(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	upstreamBody := strings.Join([]string{
		`data: {"id":"cmpl_1","object":"chat.completion.chunk","model":"qwen3.8-flash","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		"",
		`data: {"id":"cmpl_1","object":"chat.completion.chunk","model":"qwen3.8-flash","choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		"",
		`data: {"id":"cmpl_1","object":"chat.completion.chunk","model":"qwen3.8-flash","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
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
	account := newMaskedTestAccount(187, map[string]any{"kimi-k3": "qwen3.8-flash"})

	// requestBodyLen=0 关闭静默拒绝缓冲，确保输出立即写出。
	result, err := svc.streamRawChatCompletions(c, resp, account, "kimi-k3", "qwen3.8-flash", "qwen3.8-flash", nil, nil, time.Now(), 0)
	require.NoError(t, err)
	require.NotNil(t, result)

	downstream := rec.Body.String()
	require.Contains(t, downstream, "data: [DONE]")
	// 逐帧断言：每个 data 帧的 model 都被回写为 kimi-k3。
	frameCount := 0
	for _, line := range strings.Split(downstream, "\n") {
		payload, ok := extractOpenAISSEDataLine(line)
		if !ok {
			continue
		}
		trimmed := strings.TrimSpace(payload)
		if trimmed == "" || trimmed == "[DONE]" {
			continue
		}
		frameCount++
		require.Equal(t, "kimi-k3", gjson.Get(payload, "model").String(), "流式每帧 model 必须回写为客户端请求名: %s", line)
	}
	require.Equal(t, 3, frameCount, "应有 3 个含 model 的 data 帧")
	// usage 透传不变（usage 帧 completion_tokens=2）
	require.Equal(t, 2, result.Usage.OutputTokens)
	require.Equal(t, "qwen3.8-flash", observedUpstreamResponseModel(c))
}

// --- 用例 3：converted 聚合路径：聚合体 model 回写 ---

func TestRawChatCompletions_MaskedConvertedAggregateRewritesModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	upstreamBody := strings.Join([]string{
		`data: {"id":"cvt_1","object":"chat.completion.chunk","model":"qwen3.8-flash","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		"",
		`data: {"id":"cvt_1","object":"chat.completion.chunk","model":"qwen3.8-flash","choices":[{"index":0,"delta":{"content":"hello world"}}]}`,
		"",
		`data: {"id":"cvt_1","object":"chat.completion.chunk","model":"qwen3.8-flash","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"",
		`data: {"id":"cvt_1","object":"chat.completion.chunk","model":"qwen3.8-flash","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
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
	account := newMaskedTestAccount(187, map[string]any{"kimi-k3": "qwen3.8-flash"})

	result, err := svc.convertedChatCompletionsAsRawCC(c, resp, account, "kimi-k3", "qwen3.8-flash", "qwen3.8-flash", nil, nil, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)

	downstream := rec.Body.String()
	require.Equal(t, http.StatusOK, rec.Code)
	require.False(t, result.Stream, "converted 路径为非流式 ForwardResult")
	// 聚合体 model 回写为 kimi-k3
	require.Equal(t, "kimi-k3", gjson.Get(downstream, "model").String(), "聚合体 model 必须回写")
	// 内容与 usage 聚合正确
	require.Equal(t, "chat.completion", gjson.Get(downstream, "object").String())
	require.Equal(t, "hello world", gjson.Get(downstream, "choices.0.message.content").String())
	require.Equal(t, "stop", gjson.Get(downstream, "choices.0.finish_reason").String())
	require.Equal(t, 5, int(gjson.Get(downstream, "usage.total_tokens").Int()))
	require.Equal(t, "qwen3.8-flash", observedUpstreamResponseModel(c))
}

// --- 用例 4：非掩码账号：三路径字节级原样透传（零变化） ---

func TestRawChatCompletions_PlainAccountNoRewriteThreePaths(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := newPlainTestAccount(187, map[string]any{"kimi-k3": "qwen3.8-flash"})

	const upstreamJSON = `{"id":"cmpl_1","object":"chat.completion","model":"qwen3.8-flash","choices":[{"index":0,"message":{"role":"assistant","content":"hi","reasoning_content":"thought"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`

	// 4a. 非流式：字节级原样
	t.Run("nonstreaming", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(upstreamJSON)),
		}
		svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
		_, err := svc.bufferRawChatCompletions(c, resp, account, "kimi-k3", "qwen3.8-flash", "qwen3.8-flash", nil, nil, time.Now())
		require.NoError(t, err)
		require.Equal(t, upstreamJSON, rec.Body.String(), "非掩码账号非流式必须字节级原样透传")
	})

	// 4b. 流式：字节级原样（逐行归一化后等价）
	streamBody := strings.Join([]string{
		`data: {"id":"cmpl_1","object":"chat.completion.chunk","model":"qwen3.8-flash","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		"",
		`data: {"id":"cmpl_1","object":"chat.completion.chunk","model":"qwen3.8-flash","choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		"",
		`data: {"id":"cmpl_1","object":"chat.completion.chunk","model":"qwen3.8-flash","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"",
		`data: {"id":"cmpl_1","object":"chat.completion.chunk","model":"qwen3.8-flash","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	t.Run("streaming", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(streamBody)),
		}
		svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
		_, err := svc.streamRawChatCompletions(c, resp, account, "kimi-k3", "qwen3.8-flash", "qwen3.8-flash", nil, nil, time.Now(), 0)
		require.NoError(t, err)
		require.Equal(t, sseBodyNormalized(streamBody), sseBodyNormalized(rec.Body.String()), "非掩码账号流式必须字节级原样透传")
	})

	// 4c. converted 聚合：model 与内容保持不变（未触发回写）
	t.Run("converted", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(streamBody)),
		}
		svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
		_, err := svc.convertedChatCompletionsAsRawCC(c, resp, account, "kimi-k3", "qwen3.8-flash", "qwen3.8-flash", nil, nil, time.Now())
		require.NoError(t, err)
		downstream := rec.Body.String()
		require.Equal(t, "qwen3.8-flash", gjson.Get(downstream, "model").String(), "非掩码账号 converted 不得改写 model")
		require.Equal(t, "hi", gjson.Get(downstream, "choices.0.message.content").String())
	})
}

// --- 用例 5：别名语义：mapped=foo:free、回显 foo → 客户端看到客户端请求名 ---

func TestRawChatCompletions_MaskedAliasRewrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := newMaskedTestAccount(187, map[string]any{"client-req": "foo:free"})
	const clientModel = "client-req"
	const mappedModel = "foo:free" // 上游发送名（含 :free）
	// 上游回显丢失 :free 后缀 → foo（别名匹配场景）
	const upstreamEcho = "foo"

	t.Run("nonstreaming", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		upstreamJSON := `{"id":"a","object":"chat.completion","model":"` + upstreamEcho + `","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(upstreamJSON)),
		}
		svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
		_, err := svc.bufferRawChatCompletions(c, resp, account, clientModel, mappedModel, mappedModel, nil, nil, time.Now())
		require.NoError(t, err)
		require.Equal(t, clientModel, gjson.Get(rec.Body.String(), "model").String(), "别名匹配：回显 foo 应改写为客户端请求名")
	})

	t.Run("streaming", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		upstreamBody := strings.Join([]string{
			`data: {"id":"a","object":"chat.completion.chunk","model":"` + upstreamEcho + `","choices":[{"index":0,"delta":{"content":"ok"}}]}`,
			"",
			`data: {"id":"a","object":"chat.completion.chunk","model":"` + upstreamEcho + `","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
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
		_, err := svc.streamRawChatCompletions(c, resp, account, clientModel, mappedModel, mappedModel, nil, nil, time.Now(), 0)
		require.NoError(t, err)
		for _, line := range strings.Split(rec.Body.String(), "\n") {
			payload, ok := extractOpenAISSEDataLine(line)
			if !ok {
				continue
			}
			trimmed := strings.TrimSpace(payload)
			if trimmed == "" || trimmed == "[DONE]" {
				continue
			}
			require.Equal(t, clientModel, gjson.Get(payload, "model").String(), "别名匹配流式帧必须改写为客户端请求名")
		}
	})
}

// --- 用例 6：originalModel == upstreamModel 或空值：不触发替换（字节不变） ---

func TestRawChatCompletions_MaskGateNoRewriteWhenEqualOrEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const upstreamJSON = `{"id":"cmpl_1","object":"chat.completion","model":"qwen3.8-flash","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

	tests := []struct {
		name          string
		originalModel string
		upstreamModel string
	}{
		{name: "上下游同名", originalModel: "qwen3.8-flash", upstreamModel: "qwen3.8-flash"},
		{name: "空 originalModel", originalModel: "", upstreamModel: "qwen3.8-flash"},
		{name: "空 upstreamModel", originalModel: "kimi-k3", upstreamModel: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 掩码账号 + 边界取值 → 闸门 False，字节不变
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(upstreamJSON)),
			}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
			account := newMaskedTestAccount(187, map[string]any{"kimi-k3": "qwen3.8-flash"})
			_, err := svc.bufferRawChatCompletions(c, resp, account, tt.originalModel, tt.upstreamModel, tt.upstreamModel, nil, nil, time.Now())
			require.NoError(t, err)
			require.Equal(t, upstreamJSON, rec.Body.String(), "边界取值不得触发替换")
			require.Equal(t, "qwen3.8-flash", gjson.Get(rec.Body.String(), "model").String())
		})
	}
}

// 掩码门谓词单测：仅掩码账号且非空且不同名时成立。
func TestRawChatCompletions_MaskGatePredicate(t *testing.T) {
	masked := newMaskedTestAccount(187, map[string]any{"kimi-k3": "qwen3.8-flash"})
	plain := newPlainTestAccount(187, map[string]any{"kimi-k3": "qwen3.8-flash"})

	require.True(t, rawChatCompletionsMaskGate(masked, "kimi-k3", "qwen3.8-flash"))
	require.False(t, rawChatCompletionsMaskGate(plain, "kimi-k3", "qwen3.8-flash"), "非掩码账号不触发")
	require.False(t, rawChatCompletionsMaskGate(masked, "kimi-k3", "kimi-k3"), "同名不触发")
	require.False(t, rawChatCompletionsMaskGate(masked, "", "qwen3.8-flash"), "空 originalModel 不触发")
	require.False(t, rawChatCompletionsMaskGate(masked, "kimi-k3", ""), "空 upstreamModel 不触发")
	require.False(t, rawChatCompletionsMaskGate(nil, "kimi-k3", "qwen3.8-flash"), "nil 账号不触发")
}
