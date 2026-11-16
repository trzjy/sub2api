package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// cursorCompatUpstreamStub 以嵌入 HTTPUpstream 的方式只覆写 Do（对齐
// handler/gemini_client_cancel_test.go 的写入手法），记录出站请求与请求体。
type cursorCompatUpstreamStub struct {
	HTTPUpstream
	resp     *http.Response
	err      error
	calls    int
	lastReq  *http.Request
	sentBody []byte
}

func (u *cursorCompatUpstreamStub) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls++
	u.lastReq = req
	if req.Body != nil {
		u.sentBody, _ = io.ReadAll(req.Body)
	}
	if u.err != nil {
		return nil, u.err
	}
	if u.resp == nil {
		return nil, fmt.Errorf("missing stub response")
	}
	resp := *u.resp
	return &resp, nil
}

func newCursorOpenAICompatibleAccount() *Account {
	return &Account{
		ID:       548,
		Platform: PlatformCursor,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":       "kncy-secret-key",
			"base_url":      "https://kn-cy.example/v1",
			"model_mapping": map[string]any{"gpt-6.1-sol": "upstream-model-x"},
		},
		Concurrency: 1,
	}
}

// 非流式：出站直连 OpenAI 兼容 /chat/completions（无 /v1beta/）、Bearer 鉴权、
// model 按 GetMappedModel 改写；上游 chat.completion 原样回传，usage 按口径落地。
func TestForwardCursorOpenAICompatibleChat_NonStream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamJSON := `{"id":"chatcmpl-1","object":"chat.completion","model":"upstream-model-x","choices":[{"index":0,"message":{"role":"assistant","content":"hello from kn-cy"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":10,"total_tokens":1010,"prompt_tokens_details":{"cached_tokens":200,"cache_write_tokens":300}}}`
	stub := &cursorCompatUpstreamStub{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"X-Request-Id": []string{"req-123"},
		},
		Body: io.NopCloser(strings.NewReader(upstreamJSON)),
	}}
	svc := &GeminiMessagesCompatService{httpUpstream: stub, cfg: &config.Config{}}
	account := newCursorOpenAICompatibleAccount()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"hi"}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)

	// ① 出站 URL 为 <base>/chat/completions，绝不包含 /v1beta/
	require.Equal(t, 1, stub.calls)
	require.NotNil(t, stub.lastReq)
	require.Equal(t, "https://kn-cy.example/v1/chat/completions", stub.lastReq.URL.String())
	require.NotContains(t, stub.lastReq.URL.String(), "/v1beta/")
	// ② Authorization: Bearer 头存在
	require.Equal(t, "Bearer kncy-secret-key", stub.lastReq.Header.Get("Authorization"))
	require.Equal(t, "application/json", stub.lastReq.Header.Get("Content-Type"))
	// ③ 请求体 model 已按 GetMappedModel 映射
	require.Equal(t, "upstream-model-x", account.GetMappedModel("gpt-6.1-sol"))
	var sent struct {
		Model string `json:"model"`
	}
	require.NoError(t, json.Unmarshal(stub.sentBody, &sent))
	require.Equal(t, "upstream-model-x", sent.Model)

	// ④ 非流式原样回传，ForwardResult.Usage 与上游 usage 按口径一致
	require.Equal(t, upstreamJSON, rec.Body.String())
	require.Equal(t, "req-123", result.RequestID)
	require.Equal(t, "gpt-6.1-sol", result.Model)
	require.Equal(t, "upstream-model-x", result.UpstreamModel)
	require.False(t, result.Stream)
	require.Equal(t, ClaudeUsage{
		InputTokens:              500, // 1000 - cached 200 - cache_write 300
		OutputTokens:             10,
		CacheReadInputTokens:     200,
		CacheCreationInputTokens: 300,
	}, result.Usage)
}

// 流式 + 客户端未带 stream_options：出站强制 include_usage=true；上游末帧 usage
// 计入 result.Usage，但纯 usage 帧不回写客户端，其余帧与 [DONE] 原样透传。
func TestForwardCursorOpenAICompatibleChat_Stream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamSSE := `data: {"id":"chunk-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"he"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"chunk-2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"llo"},"finish_reason":"stop"}]}` + "\n\n" +
		`data: {"id":"chunk-3","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":8,"total_tokens":28,"prompt_tokens_details":{"cached_tokens":4,"cache_creation_tokens":3}}}` + "\n\n" +
		`data: [DONE]` + "\n\n"
	stub := &cursorCompatUpstreamStub{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamSSE)),
	}}
	svc := &GeminiMessagesCompatService{httpUpstream: stub, cfg: &config.Config{}}
	account := newCursorOpenAICompatibleAccount()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Stream)
	require.Equal(t, 1, stub.calls)
	require.NotNil(t, stub.lastReq)
	require.Equal(t, "https://kn-cy.example/v1/chat/completions", stub.lastReq.URL.String())
	require.NotContains(t, stub.lastReq.URL.String(), "/v1beta/")
	require.Equal(t, "Bearer kncy-secret-key", stub.lastReq.Header.Get("Authorization"))
	require.Equal(t, "text/event-stream; charset=utf-8", rec.Header().Get("Content-Type"))
	require.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))

	// 客户端未带 stream_options 时，出站 body 仍强制带上 include_usage=true。
	require.Contains(t, string(stub.sentBody), `"include_usage":true`)

	out := rec.Body.String()
	require.Contains(t, out, `"content":"he"`)
	require.Contains(t, out, `"content":"llo"`)
	require.Contains(t, out, "data: [DONE]")
	// 纯 usage 帧（chunk-3）不回写客户端。
	require.NotContains(t, out, "chunk-3")
	require.NotContains(t, out, `"prompt_tokens":20`)

	// ⑤ 流式结束后 Usage 有值（取末帧 usage，口径同非流式）
	require.Equal(t, ClaudeUsage{
		InputTokens:              13, // 20 - cached 4 - cache_creation 3
		OutputTokens:             8,
		CacheReadInputTokens:     4,
		CacheCreationInputTokens: 3,
	}, result.Usage)
}

// 流式 + 客户端带 include_usage=true：出站同样强制 include_usage=true，
// 且上游纯 usage 帧原样透传给客户端，Usage 也计入 result.Usage。
func TestForwardCursorOpenAICompatibleChat_StreamWithIncludeUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamSSE := `data: {"id":"chunk-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"he"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"chunk-2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"llo"},"finish_reason":"stop"}]}` + "\n\n" +
		`data: {"id":"usage-chunk","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":8,"total_tokens":28,"prompt_tokens_details":{"cached_tokens":4,"cache_creation_tokens":3}}}` + "\n\n" +
		`data: [DONE]` + "\n\n"
	stub := &cursorCompatUpstreamStub{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamSSE)),
	}}
	svc := &GeminiMessagesCompatService{httpUpstream: stub, cfg: &config.Config{}}
	account := newCursorOpenAICompatibleAccount()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Stream)
	require.Equal(t, 1, stub.calls)

	require.Contains(t, string(stub.sentBody), `"include_usage":true`)

	out := rec.Body.String()
	require.Contains(t, out, `"content":"he"`)
	require.Contains(t, out, `"content":"llo"`)
	// 客户端明确要求 usage 时，纯 usage 帧原样透传。
	require.Contains(t, out, "usage-chunk")
	require.Contains(t, out, `"prompt_tokens":20`)
	require.Contains(t, out, "data: [DONE]")

	require.Equal(t, ClaudeUsage{
		InputTokens:              13,
		OutputTokens:             8,
		CacheReadInputTokens:     4,
		CacheCreationInputTokens: 3,
	}, result.Usage)
}

// 非兼容 cursor 账号（OAuth Connect）仍走原 forwardCursorChatCompletions，
// 不进入新增的 OpenAI 兼容出站分支。无 cursorExecution 时原分支失败关闭，
// 因此不会有任何 /chat/completions 出站。
func TestForwardCursorOpenAICompatibleChat_NonCompatibleStillUsesCursorPath(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stub := &cursorCompatUpstreamStub{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{}`)),
	}}
	// cursorExecution 故意留空：Connect 路径在出站前失败关闭。
	svc := &GeminiMessagesCompatService{httpUpstream: stub, cfg: &config.Config{}}
	account := &Account{
		ID:          700,
		Platform:    PlatformCursor,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "ya29.connect"},
		Concurrency: 1,
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"hi"}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrCursorAccountBearerMissing)
	require.Zero(t, stub.calls, "non-compatible cursor account must not use OpenAI-compatible outbound")
	require.Nil(t, stub.lastReq)
}

// 缺少 api_key 时按契约写 502 upstream_error，且不发出任何上游请求。
func TestForwardCursorOpenAICompatibleChat_MissingAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stub := &cursorCompatUpstreamStub{}
	svc := &GeminiMessagesCompatService{httpUpstream: stub, cfg: &config.Config{}}
	account := &Account{
		ID:          548,
		Platform:    PlatformCursor,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "https://kn-cy.example/v1"},
		Concurrency: 1,
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"hi"}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body)
	require.Error(t, err)
	require.Zero(t, stub.calls)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Contains(t, rec.Body.String(), "upstream_error")
	require.Contains(t, rec.Body.String(), "该请求不支持当前账号配置")
}
