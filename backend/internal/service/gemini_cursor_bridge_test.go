//go:build unit

package service

// P1-C2：Gemini 三出口（Messages / Chat Completions / 原生 v1beta）对 cursor 账号
// 早分流的配对单测（配对文件：gemini_messages_compat_service.go /
// gemini_chat_completions_compat_service.go）。
//
// 每个出口都必须满足三条**客户端可见**性质：
//  1. 响应协议形状正确（Anthropic message / chat.completion / Gemini candidates）；
//  2. 流式以该协议的正规终态结束（message_stop / [DONE] / 带 finishReason 的末帧）；
//  3. 真实 usage 只出现在协议规定的唯一位置（不重复、不伪造）。
//
// 另外钉死：原生非文本 action（countTokens）明确报错且不打上游（不伪装成功）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// newGeminiCursorTestFixture 装配 cursor 上游替身 + 已安装桥接的网关实例。
func newGeminiCursorTestFixture(t *testing.T, chatBody []byte) (*GeminiMessagesCompatService, *cursorE2EUpstream, *ginContextRecorder) {
	t.Helper()
	upstream := &cursorE2EUpstream{chatBody: chatBody}
	// newCursorE2EService 会构造 OpenAIGatewayService 并在构造末尾安装包级桥接。
	_, c, w := newCursorE2EService(t, upstream)
	return &GeminiMessagesCompatService{}, upstream, &ginContextRecorder{c: c, w: w}
}

// ginContextRecorder 把 gin 上下文与响应记录器绑在一起，读断言更直接。
type ginContextRecorder struct {
	c *gin.Context
	w *httptest.ResponseRecorder
}

func decodeJSONMap(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(body, &out), "response must be JSON: %s", string(body))
	return out
}

// ---------------------------------------------------------------------------
// 出口一：Messages（Anthropic 形状）
// ---------------------------------------------------------------------------

// TestGeminiCursorMessagesForwardNonStreaming 断言 Messages 出口的客户端响应是
// Anthropic message 形状，且真实 usage 只出现一次。
func TestGeminiCursorMessagesForwardNonStreaming(t *testing.T) {
	svc, upstream, rec := newGeminiCursorTestFixture(t, cursorE2ESuccessStream(t, "Hello"))
	account := cursorE2EAccount()
	body := []byte(`{"model":"cursor-sonnet","max_tokens":64,"stream":false,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	res, err := svc.Forward(t.Context(), rec.c, account, body)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, 12601, res.Usage.InputTokens)
	require.Equal(t, 15, res.Usage.OutputTokens)

	require.Equal(t, http.StatusOK, rec.w.Code)
	out := decodeJSONMap(t, rec.w.Body.Bytes())
	require.Equal(t, "message", out["type"])
	require.Equal(t, "assistant", out["role"])
	require.Equal(t, "cursor-sonnet", out["model"])

	content, ok := out["content"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, content)
	block := content[0].(map[string]any)
	require.Equal(t, "text", block["type"])
	require.Equal(t, "Hello", block["text"])

	// usage 只有一个位置（message.usage），且是上游真实值。
	usage := out["usage"].(map[string]any)
	require.Equal(t, float64(12601), usage["input_tokens"])
	require.Equal(t, float64(15), usage["output_tokens"])
	require.Equal(t, 1, strings.Count(string(rec.w.Body.Bytes()), `"usage"`))

	// 出站必须是 Cursor 原生协议（protobuf envelope），不是 Anthropic/Gemini JSON。
	require.Equal(t, 1, upstream.count())
	req := upstream.requestAt(0)
	require.Equal(t, cursorPathStreamUnifiedChatWithTools, req.path)
	require.Equal(t, cursorConnectContentType, req.header.Get("Content-Type"))
	require.Contains(t, string(req.body), "claude-opus-4-8-low")
}

// TestGeminiCursorMessagesForwardStreamingTerminal 断言 Messages 流式响应以
// message_stop 收尾，且真实 usage 只写一次（message_delta）。
func TestGeminiCursorMessagesForwardStreamingTerminal(t *testing.T) {
	svc, _, rec := newGeminiCursorTestFixture(t, cursorE2ESuccessStream(t, "Hello"))
	account := cursorE2EAccount()
	body := []byte(`{"model":"cursor-sonnet","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	res, err := svc.Forward(t.Context(), rec.c, account, body)
	require.NoError(t, err)
	require.NotNil(t, res)

	require.Equal(t, http.StatusOK, rec.w.Code)
	require.Equal(t, "text/event-stream", rec.w.Header().Get("Content-Type"))
	out := rec.w.Body.String()
	require.Contains(t, out, "event: message_start")
	require.Contains(t, out, "Hello")
	require.True(t, strings.HasSuffix(out, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"),
		"Anthropic stream must end with message_stop, got tail: %q", tailOf(out, 160))

	// 真实 usage 只出现在 message_delta 一次（message_start 的 usage 是全零占位）。
	require.Equal(t, 1, strings.Count(out, `"input_tokens":12601`))
	require.Equal(t, 1, strings.Count(out, `"output_tokens":15`))
}

// ---------------------------------------------------------------------------
// 出口二：Chat Completions
// ---------------------------------------------------------------------------

// TestGeminiCursorChatCompletionsNonStreaming 断言 CC 出口返回 chat.completion 形状，
// usage 只在顶层出现一次。
func TestGeminiCursorChatCompletionsNonStreaming(t *testing.T) {
	svc, _, rec := newGeminiCursorTestFixture(t, cursorE2ESuccessStream(t, "Hello"))
	account := cursorE2EAccount()
	body := []byte(`{"model":"cursor-sonnet","stream":false,"messages":[{"role":"user","content":"hi"}]}`)

	res, err := svc.ForwardAsChatCompletions(t.Context(), rec.c, account, body)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, 12601, res.Usage.InputTokens)

	require.Equal(t, http.StatusOK, rec.w.Code)
	out := decodeJSONMap(t, rec.w.Body.Bytes())
	require.Equal(t, "chat.completion", out["object"])
	choices := out["choices"].([]any)
	message := choices[0].(map[string]any)["message"].(map[string]any)
	require.Equal(t, "Hello", message["content"])
	require.Equal(t, 1, strings.Count(string(rec.w.Body.Bytes()), `"usage"`))
}

// TestGeminiCursorChatCompletionsStreamingTerminal 断言 CC 流式以 [DONE] 收尾，
// 且 usage 只在终止 chunk 出现一次。
func TestGeminiCursorChatCompletionsStreamingTerminal(t *testing.T) {
	svc, _, rec := newGeminiCursorTestFixture(t, cursorE2ESuccessStream(t, "Hello"))
	account := cursorE2EAccount()
	body := []byte(`{"model":"cursor-sonnet","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	res, err := svc.ForwardAsChatCompletions(t.Context(), rec.c, account, body)
	require.NoError(t, err)
	require.NotNil(t, res)

	require.Equal(t, http.StatusOK, rec.w.Code)
	out := rec.w.Body.String()
	require.Contains(t, out, "Hello")
	require.True(t, strings.HasSuffix(out, cursorSSEDoneSentinel), "CC stream must end with [DONE]")
	require.Equal(t, 1, strings.Count(out, `"usage"`))
}

// ---------------------------------------------------------------------------
// 出口三：原生 v1beta
// ---------------------------------------------------------------------------

// TestGeminiCursorNativeNonStreaming 断言原生出口返回 Gemini candidates 形状，
// usageMetadata 只出现一次。
func TestGeminiCursorNativeNonStreaming(t *testing.T) {
	svc, upstream, rec := newGeminiCursorTestFixture(t, cursorE2ESuccessStream(t, "Hello"))
	account := cursorE2EAccount()
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"temperature":0.2,"maxOutputTokens":64}}`)

	res, err := svc.ForwardNative(t.Context(), rec.c, account, "cursor-sonnet", "generateContent", false, body)
	require.NoError(t, err)
	require.NotNil(t, res)

	require.Equal(t, http.StatusOK, rec.w.Code)
	out := decodeJSONMap(t, rec.w.Body.Bytes())
	candidates := out["candidates"].([]any)
	candidate := candidates[0].(map[string]any)
	require.Equal(t, "STOP", candidate["finishReason"])
	content := candidate["content"].(map[string]any)
	require.Equal(t, "model", content["role"])
	parts := content["parts"].([]any)
	require.Equal(t, "Hello", parts[0].(map[string]any)["text"])

	usage := out["usageMetadata"].(map[string]any)
	require.Equal(t, float64(15), usage["candidatesTokenCount"])
	require.Equal(t, 1, strings.Count(string(rec.w.Body.Bytes()), `"usageMetadata"`))

	require.Equal(t, 1, upstream.count())
}

// TestGeminiCursorNativeStreamingTerminal 断言原生流式以带 finishReason 的末帧收尾，
// usageMetadata 只出现在末帧一次，且没有 OpenAI 的 [DONE] 哨兵。
func TestGeminiCursorNativeStreamingTerminal(t *testing.T) {
	svc, _, rec := newGeminiCursorTestFixture(t, cursorE2ESuccessStream(t, "Hello"))
	account := cursorE2EAccount()
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)

	res, err := svc.ForwardNative(t.Context(), rec.c, account, "cursor-sonnet", "streamGenerateContent", true, body)
	require.NoError(t, err)
	require.NotNil(t, res)

	require.Equal(t, http.StatusOK, rec.w.Code)
	out := rec.w.Body.String()
	require.Contains(t, out, "Hello")
	require.NotContains(t, out, "[DONE]", "Gemini SSE has no [DONE] sentinel")
	require.Equal(t, 1, strings.Count(out, `"usageMetadata"`))

	frames := cursorTestSSEDataFrames(t, out)
	require.NotEmpty(t, frames)
	final := frames[len(frames)-1]
	require.Equal(t, "STOP", final["candidates"].([]any)[0].(map[string]any)["finishReason"])
	require.Contains(t, final, "usageMetadata")
}

// TestGeminiCursorNativeNonTextActionFailsClosed 断言 countTokens 等非文本动作
// 明确报错（不伪装成功、不打上游）。
func TestGeminiCursorNativeNonTextActionFailsClosed(t *testing.T) {
	svc, upstream, rec := newGeminiCursorTestFixture(t, cursorE2ESuccessStream(t, "Hello"))
	account := cursorE2EAccount()
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)

	res, err := svc.ForwardNative(t.Context(), rec.c, account, "cursor-sonnet", "countTokens", false, body)
	require.Error(t, err)
	require.Nil(t, res)
	require.Equal(t, http.StatusBadRequest, rec.w.Code)

	out := decodeJSONMap(t, rec.w.Body.Bytes())
	errObj := out["error"].(map[string]any)
	require.Equal(t, float64(http.StatusBadRequest), errObj["code"])
	require.Contains(t, errObj["message"], "countTokens")

	require.Equal(t, 0, upstream.count(), "unsupported native action must not hit upstream")
	require.Empty(t, upstream.paths())
}

// cursorTestSSEDataFrames 解析 Gemini SSE 的 data 帧（测试专用小工具）。
func cursorTestSSEDataFrames(t *testing.T, body string) []map[string]any {
	t.Helper()
	var frames []map[string]any
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" || payload == "[DONE]" {
				continue
			}
			var frame map[string]any
			require.NoError(t, json.Unmarshal([]byte(payload), &frame), "frame: %s", payload)
			frames = append(frames, frame)
		}
	}
	return frames
}

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
