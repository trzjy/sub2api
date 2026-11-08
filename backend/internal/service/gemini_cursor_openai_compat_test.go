package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestIsCursorOpenAICompatibleAccountPredicate 锁定 KC-27 的分派谓词口径
// （与 openai_gateway_chat_completions.go:143 一致）：cursor 平台靠账号类型区分
// —— OAuth（Connect）账号仍需走 Connect 出站，APIKey（OpenAI 兼容反代）账号绕过。
func TestIsCursorOpenAICompatibleAccountPredicate(t *testing.T) {
	oauth := &Account{Platform: PlatformCursor, Type: AccountTypeOAuth}
	apikey := &Account{Platform: PlatformCursor, Type: AccountTypeAPIKey}
	gemini := &Account{Platform: PlatformGemini, Type: AccountTypeAPIKey}

	require.True(t, isCursorAccount(oauth))
	require.True(t, isCursorAccount(apikey))
	require.False(t, isCursorAccount(gemini))

	require.False(t, isCursorOpenAICompatibleAccount(oauth))
	require.True(t, isCursorOpenAICompatibleAccount(apikey))
	require.False(t, isCursorOpenAICompatibleAccount(gemini))

	// 分派前置谓词：仅「cursor 且非 OpenAI 兼容账号」进 Connect 出站。
	require.True(t, isCursorAccount(oauth) && !isCursorOpenAICompatibleAccount(oauth))
	require.False(t, isCursorAccount(apikey) && !isCursorOpenAICompatibleAccount(apikey))
}

// TestGeminiNativeCursorAPIKeyRoutesToOpenAICompatible 覆盖 KC-27 核心验收：
// gemini 原生入口（ForwardNative）+ cursor apikey 账号，不再进 Connect（无
// ErrCursorAccountBearerMissing / CUR-EXEC-06），出站为 OpenAI 兼容形状且带
// Authorization: Bearer <api_key>，并把 OpenAI 响应回转成 Gemini 原生形状。
func TestGeminiNativeCursorAPIKeyRoutesToOpenAICompatible(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamBody := `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hello from kn-cy"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	httpStub := &geminiCompatHTTPUpstreamStub{
		response: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(upstreamBody)),
		},
	}
	svc := &GeminiMessagesCompatService{
		httpUpstream: httpStub,
		cfg:          &config.Config{},
	}
	account := &Account{
		ID:       548,
		Platform: PlatformCursor,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "kncy-secret-key",
			"base_url": "https://kn-cy.com/v1",
		},
		Concurrency: 1,
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"temperature":0.5}}`)

	result, err := svc.ForwardNative(context.Background(), c, account, "gemini-4-argon", "generateContent", false, body)
	require.NoError(t, err)
	require.NotNil(t, result)

	// Done when #3：不再产生 Connect 凭证缺失错误。
	require.NotErrorIs(t, err, ErrCursorAccountBearerMissing)

	// Done when #4：出站是 OpenAI 兼容形状且带 Bearer api_key。
	require.NotNil(t, httpStub.lastReq)
	require.Equal(t, http.MethodPost, httpStub.lastReq.Method)
	require.True(t, strings.HasSuffix(httpStub.lastReq.URL.String(), "/chat/completions"),
		"outbound must target OpenAI-compatible /chat/completions, got %s", httpStub.lastReq.URL.String())
	require.Equal(t, "Bearer kncy-secret-key", httpStub.lastReq.Header.Get("Authorization"))
	require.Empty(t, httpStub.lastReq.Header.Get("x-goog-api-key"))

	sentBody, rErr := io.ReadAll(httpStub.lastReq.Body)
	require.NoError(t, rErr)
	sent := string(sentBody)
	require.Contains(t, sent, `"messages"`, "outbound body must be OpenAI Chat Completions shape")
	require.Contains(t, sent, `"model"`)

	// 响应被回转成 Gemini 原生形状。
	require.Contains(t, rec.Body.String(), `"candidates"`)
	require.Contains(t, rec.Body.String(), "hello from kn-cy")
}

// TestGeminiNativeCursorAPIKeyStreamRoutesToOpenAICompatible 覆盖流式形态：
// 同一分派下，Gemini 原生流（streamGenerateContent）也应打到 OpenAI 兼容 /chat/completions
// 并以 Bearer 鉴权，OpenAI SSE 被回转成 Gemini 原生 SSE。
func TestGeminiNativeCursorAPIKeyStreamRoutesToOpenAICompatible(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamBody := `data: {"choices":[{"delta":{"role":"assistant","content":"he"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"content":"llo"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}` + "\n\n" +
		"data: [DONE]\n\n"
	httpStub := &geminiCompatHTTPUpstreamStub{
		response: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(upstreamBody)),
		},
	}
	svc := &GeminiMessagesCompatService{
		httpUpstream: httpStub,
		cfg:          &config.Config{},
	}
	account := &Account{
		ID:       548,
		Platform: PlatformCursor,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "kncy-secret-key",
			"base_url": "https://kn-cy.com/v1",
		},
		Concurrency: 1,
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)

	result, err := svc.ForwardNative(context.Background(), c, account, "gemini-4-argon", "streamGenerateContent", true, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Stream)

	require.NotNil(t, httpStub.lastReq)
	require.True(t, strings.HasSuffix(httpStub.lastReq.URL.String(), "/chat/completions"))
	require.Equal(t, "Bearer kncy-secret-key", httpStub.lastReq.Header.Get("Authorization"))

	// 回写的是 Gemini 原生 SSE（data: {...} 携带 candidates）。
	out := rec.Body.String()
	require.Contains(t, out, "data: ")
	require.Contains(t, out, `"candidates"`)
	// 流式逐块回转：两段增量分别回写。
	require.Contains(t, out, `"text":"he"`)
	require.Contains(t, out, `"text":"llo"`)
	require.Contains(t, out, `"finishReason":"STOP"`)
}

// TestGeminiNativeCursorOAuthStillConnect 覆盖 Done when #2：cursor + OAuth（Connect）
// 账号在 gemini 原生入口仍走 Connect 出站（即绕过 OpenAI 兼容分支，不会打到 /chat/completions）。
// 无 cursorExecution 时 Connect 路径在出站前即失败关闭，可据此断言未进入 OpenAI 兼容分支。
func TestGeminiNativeCursorOAuthStillConnect(t *testing.T) {
	gin.SetMode(gin.TestMode)

	httpStub := &geminiCompatHTTPUpstreamStub{
		response: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{}`)),
		},
	}
	svc := &GeminiMessagesCompatService{
		// cursorExecution 故意留空：Connect 路径在出站前 fail-closed。
		httpUpstream: httpStub,
		cfg:          &config.Config{},
	}
	account := &Account{
		ID:       700,
		Platform: PlatformCursor,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "ya29.connect",
		},
		Concurrency: 1,
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)

	_, err := svc.ForwardNative(context.Background(), c, account, "gemini-4-argon", "generateContent", false, body)
	// Connect 路径因 cursorExecution 缺失而失败关闭——这恰好证明 OAuth 仍走 Connect 分支，
	// 而非 OpenAI 兼容分支（后者不依赖 cursorExecution）。
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrCursorAccountBearerMissing)
	// 未进入 OpenAI 兼容分支：无任何 /chat/completions 出站。
	require.Nil(t, httpStub.lastReq, "OAuth account must not route to OpenAI-compatible /chat/completions")
}

// TestGeminiForwardAsChatCompletionsCursorAPIKeyNotConnect 覆盖 Done when #3 在 Chat
// Completions 入口：cursor + apikey 账号不再进 Connect（无 ErrCursorAccountBearerMissing），
// 落到通用 Gemini 转发管线（出站为 Gemini 上游，而非 OpenAI 兼容端点）。
func TestGeminiForwardAsChatCompletionsCursorAPIKeyNotConnect(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamBody := `data: {"candidates":[{"content":{"parts":[{"text":"hello from gemini"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3}}` + "\n\n" +
		"data: [DONE]\n\n"
	httpStub := &geminiCompatHTTPUpstreamStub{
		response: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(upstreamBody)),
		},
	}
	svc := &GeminiMessagesCompatService{
		httpUpstream: httpStub,
		cfg:          &config.Config{},
	}
	account := &Account{
		ID:       548,
		Platform: PlatformCursor,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key": "kncy-secret-key",
		},
		Concurrency: 1,
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"hi"}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body)
	// Done when #3：cursor + apikey 账号在 Chat Completions 入口不再进 Connect
	// （无 ErrCursorAccountBearerMissing）。该入口在生产由 OpenAI 网关服务，此处仅验证
	// 分派口径：落到通用 Gemini 转发管线（x-goog-api-key），而非 Connect 分支。
	require.NotErrorIs(t, err, ErrCursorAccountBearerMissing)
	require.NotNil(t, httpStub.lastReq)
	require.Contains(t, httpStub.lastReq.Header.Get("x-goog-api-key"), "kncy-secret-key")
	require.Empty(t, httpStub.lastReq.Header.Get("Authorization"))
}
