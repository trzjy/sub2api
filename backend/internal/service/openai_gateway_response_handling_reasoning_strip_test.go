//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// newCodebuddyStripStreamService 构造最小可用的 OpenAIGatewayService（流式响应处理用）。
func newCodebuddyStripStreamService() *OpenAIGatewayService {
	return &OpenAIGatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				StreamDataIntervalTimeout: 0,
				MaxLineSize:               defaultMaxLineSize,
			},
		},
	}
}

// runCodebuddyStripStream 把给定 SSE 帧喂进 handleStreamingResponse，返回客户端收到的字节。
func runCodebuddyStripStream(t *testing.T, svc *OpenAIGatewayService, account *Account, frames string) string {
	t.Helper()
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	pr, pw := io.Pipe()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: pr}

	go func() {
		defer func() { _ = pw.Close() }()
		_, _ = pw.Write([]byte(frames))
	}()

	_, err := svc.handleStreamingResponse(context.Background(), resp, c, account, time.Now(), "deepseek-v4.1-flash-1017", "deepseek-v4.1-flash-1017")
	_ = pr.Close()
	require.NoError(t, err)
	return rec.Body.String()
}

// codebuddyStripFrames 上游 vLLM 形态的 SSE 帧：思考明文在 delta.reasoning。
const codebuddyStripFrames = `data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","reasoning":"UPSTREAM_REASON_PLAINTEXT"}}]}
data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"FINAL_ANSWER"}}]}
data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}
data: [DONE]
`

// TestCodebuddyStreamStripsReasoningForStrippedAccount 覆盖 codebuddy 流式路径的 L3 剥离：
// 开启 strip_reasoning_content 的账号，客户端可见的 SSE 数据行里不得残留 delta.reasoning。
func TestCodebuddyStreamStripsReasoningForStrippedAccount(t *testing.T) {
	svc := newCodebuddyStripStreamService()

	t.Run("开启 strip_reasoning_content → 思考明文被剥离", func(t *testing.T) {
		account := &Account{ID: 386, Platform: PlatformCodeBuddy, Extra: map[string]any{
			"mask_upstream_identity":  true,
			"strip_reasoning_content": true,
		}}
		out := runCodebuddyStripStream(t, svc, account, codebuddyStripFrames)
		require.NotContains(t, out, "UPSTREAM_REASON_PLAINTEXT", "客户端可见流不得含上游思考明文")
		require.Contains(t, out, "FINAL_ANSWER", "正文必须保留")
	})

	t.Run("未开启 strip_reasoning_content → 输出不变（思考明文保留）", func(t *testing.T) {
		account := &Account{ID: 386, Platform: PlatformCodeBuddy, Extra: map[string]any{
			"mask_upstream_identity": true,
		}}
		out := runCodebuddyStripStream(t, svc, account, codebuddyStripFrames)
		require.Contains(t, out, "UPSTREAM_REASON_PLAINTEXT", "未开启剥离的账号必须原样保留思考明文")
		require.Contains(t, out, "FINAL_ANSWER", "正文必须保留")
	})
}

// TestCodebuddyStreamStripsReasoningContentField 覆盖上游直接给 reasoning_content 的形态：
// 开启剥离后同样不得残留。
func TestCodebuddyStreamStripsReasoningContentField(t *testing.T) {
	svc := newCodebuddyStripStreamService()

	frames := `data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"UPSTREAM_REASON_CONTENT"}}]}
data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"ok"}}]}
data: [DONE]
`

	account := &Account{ID: 386, Platform: PlatformCodeBuddy, Extra: map[string]any{
		"mask_upstream_identity":  true,
		"strip_reasoning_content": true,
	}}
	out := runCodebuddyStripStream(t, svc, account, frames)
	require.NotContains(t, out, "UPSTREAM_REASON_CONTENT", "客户端可见流不得含 reasoning_content 思考明文")
	require.Contains(t, out, "ok", "正文必须保留")
}
