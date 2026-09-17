package service

// web-zhipu 下游三协议输出测试（webResponseModeChat 为主）。
//
// 仅新增测试，不改动任何被测代码。覆盖回程链路（上游 SSE parts[].content[] 结构 →
// handleWebZhipuStreamingResponse / handleWebZhipuNonStreamingResponse → 客户端）的
// 关键不变量：累积文本去重、think 不进正文、无真实结束帧不产生正常终止、非流式聚合、
// 上游错误不泄露凭证、DeepSeek/Kimi 默认模型目录防误改锚点。
//
// 驱动方式参考 web_zhipu_gateway_forward_test.go 的
// TestForwardWebZhipu_StreamMidstreamBusinessError：直接调用 handler，复用同 package
// 既有 stub（webZhipuTestAccount 等）。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// webZhipuDownstreamHarness 构造流式/非流式 handler 驱动所需的 gin 上下文、recorder、
// service（仅含测试所需字段）。
func webZhipuDownstreamHarness(t *testing.T) (*httptest.ResponseRecorder, *gin.Context, *OpenAIGatewayService) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"glm-5.3-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	svc := &OpenAIGatewayService{
		cfg: &config.Config{
			Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true}},
		},
		rateLimitService: &RateLimitService{},
	}
	return recorder, c, svc
}

// webZhipuDownstreamSSE 把若干 SSE 帧 JSON 载荷包为 text/event-stream 响应。
// frameJSONs 为每行 data: 之后的 JSON（或 [DONE]）。
func webZhipuDownstreamSSE(respStatusCode int, frameJSONs ...string) *http.Response {
	var b strings.Builder
	for _, f := range frameJSONs {
		b.WriteString("data: ")
		b.WriteString(f)
		b.WriteString("\n")
	}
	return &http.Response{
		StatusCode: respStatusCode,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(b.String())),
	}
}

// webZhipuClientChunks 解析客户端收到的 SSE，返回每条 data 载荷帧（跳过 [DONE]）。
func webZhipuClientChunks(t *testing.T, out string) []map[string]any {
	t.Helper()
	var chunks []map[string]any
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(payload), &m))
		chunks = append(chunks, m)
	}
	return chunks
}

// webZhipuContentDeltas 从客户端 chunk 帧提取非空 content delta 序列。
func webZhipuContentDeltas(t *testing.T, chunks []map[string]any) []string {
	t.Helper()
	var deltas []string
	for _, ch := range chunks {
		choices, ok := ch["choices"].([]any)
		require.True(t, ok, "chunk must carry choices")
		choice, ok := choices[0].(map[string]any)
		require.True(t, ok)
		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			continue
		}
		if content, ok := delta["content"].(string); ok && content != "" {
			deltas = append(deltas, content)
		}
	}
	return deltas
}

// TestWebZhipuDownstream_StreamCumulativeTextNoDuplication 覆盖场景1：上游发三帧累积
// text（"A"/"AB"/"ABC"）+ finish 帧，客户端收到的 content delta 合计必须恰好是 "ABC"，
// 不得出现 "AABABC" 之类重复；终止帧为正常 finish_reason=stop，末行 data: [DONE]。
func TestWebZhipuDownstream_StreamCumulativeTextNoDuplication(t *testing.T) {
	recorder, c, svc := webZhipuDownstreamHarness(t)
	account := webZhipuTestAccount(9101, nil)
	resp := webZhipuDownstreamSSE(http.StatusOK,
		`{"id":"zp-1","parts":[{"role":"assistant","status":"init","content":[{"type":"text","text":"A"}]}],"status":"init"}`,
		`{"id":"zp-1","parts":[{"role":"assistant","status":"init","content":[{"type":"text","text":"AB"}]}],"status":"init"}`,
		`{"id":"zp-1","parts":[{"role":"assistant","status":"init","content":[{"type":"text","text":"ABC"}]}],"status":"init"}`,
		`{"id":"zp-1","parts":[{"role":"assistant","status":"finish","content":[{"type":"text","text":"ABC"}]}],"status":"finish"}`,
	)
	_, err := svc.handleWebZhipuStreamingResponse(
		context.Background(), resp, c, account, "glm-5.3-flash", "glm-5.3-flash", time.Now(), webResponseModeChat)
	require.NoError(t, err)

	out := recorder.Body.String()
	deltas := webZhipuContentDeltas(t, webZhipuClientChunks(t, out))
	// 累积全文形态：每帧只发增量，合计为全文。
	require.Equal(t, []string{"A", "B", "C"}, deltas, "each frame must emit only the incremental tail")
	require.Equal(t, "ABC", strings.Join(deltas, ""), "client content must be the exact full text, no duplication")
	require.NotContains(t, strings.Join(deltas, ""), "AABABC", "duplication must never occur")
	require.Contains(t, out, "data: [DONE]", "stream must terminate with [DONE]")
}

// TestWebZhipuDownstream_ThinkNotInBody 覆盖场景2：上游含 think 元素帧，客户端输出不得
// 含 think 内容；正文内容增量照常下发。
func TestWebZhipuDownstream_ThinkNotInBody(t *testing.T) {
	const thinkContent = "SUPER_SECRET_THINKING_PROCESS"
	recorder, c, svc := webZhipuDownstreamHarness(t)
	account := webZhipuTestAccount(9102, nil)
	resp := webZhipuDownstreamSSE(http.StatusOK,
		`{"id":"zp-1","parts":[{"role":"assistant","status":"init","content":[{"type":"think","think":"`+thinkContent+`"}]}],"status":"init"}`,
		`{"id":"zp-1","parts":[{"role":"assistant","status":"init","content":[{"type":"text","text":"hello"}]}],"status":"init"}`,
		`{"id":"zp-1","parts":[{"role":"assistant","status":"finish","content":[{"type":"text","text":"hello"}]}],"status":"finish"}`,
	)
	_, err := svc.handleWebZhipuStreamingResponse(
		context.Background(), resp, c, account, "glm-5.3-flash", "glm-5.3-flash", time.Now(), webResponseModeChat)
	require.NoError(t, err)

	out := recorder.Body.String()
	require.NotContains(t, out, thinkContent, "think content must never appear in client body")
	deltas := webZhipuContentDeltas(t, webZhipuClientChunks(t, out))
	require.Equal(t, []string{"hello"}, deltas, "text content must still be relayed")
}

// TestWebZhipuDownstream_NoFinishNoStopTerminal 覆盖场景3：上游永远不发 finish（帧仅
// text 且顶层 status 恒为 "init"），客户端流结束后不得出现 finish_reason=stop 的正常终止
// 帧；finish_reason 应为空串。
func TestWebZhipuDownstream_NoFinishNoStopTerminal(t *testing.T) {
	recorder, c, svc := webZhipuDownstreamHarness(t)
	account := webZhipuTestAccount(9103, nil)
	resp := webZhipuDownstreamSSE(http.StatusOK,
		`{"id":"zp-1","parts":[{"role":"assistant","status":"init","content":[{"type":"text","text":"A"}]}],"status":"init"}`,
		`{"id":"zp-1","parts":[{"role":"assistant","status":"init","content":[{"type":"text","text":"AB"}]}],"status":"init"}`,
		`{"id":"zp-1","parts":[{"role":"assistant","status":"init","content":[{"type":"text","text":"ABC"}]}],"status":"init"}`,
	)
	_, err := svc.handleWebZhipuStreamingResponse(
		context.Background(), resp, c, account, "glm-5.3-flash", "glm-5.3-flash", time.Now(), webResponseModeChat)
	require.NoError(t, err)

	out := recorder.Body.String()
	require.Contains(t, out, "data: [DONE]", "stream must still terminate with [DONE]")

	var stopFrames int
	for _, ch := range webZhipuClientChunks(t, out) {
		choices := ch["choices"].([]any)
		choice := choices[0].(map[string]any)
		fr, _ := choice["finish_reason"].(string)
		require.Equal(t, "", fr, "without a real finish frame, finish_reason must stay empty (no synthetic stop)")
		if fr == "stop" {
			stopFrames++
		}
	}
	require.Equal(t, 0, stopFrames, "no normal finish_reason=stop terminal frame may be produced")
	require.Equal(t, "ABC", strings.Join(webZhipuContentDeltas(t, webZhipuClientChunks(t, out)), ""),
		"content must still aggregate to the full text")
}

// TestWebZhipuDownstream_NonStreamAggregate 覆盖场景4：三帧累积 text → 单条 chat.completion，
// content == "ABC"，model 回填原始请求模型名。
func TestWebZhipuDownstream_NonStreamAggregate(t *testing.T) {
	recorder, c, svc := webZhipuDownstreamHarness(t)
	account := webZhipuTestAccount(9104, nil)
	resp := webZhipuDownstreamSSE(http.StatusOK,
		`{"id":"zp-1","parts":[{"role":"assistant","status":"init","content":[{"type":"text","text":"A"}]}],"status":"init"}`,
		`{"id":"zp-1","parts":[{"role":"assistant","status":"init","content":[{"type":"text","text":"AB"}]}],"status":"init"}`,
		`{"id":"zp-1","parts":[{"role":"assistant","status":"init","content":[{"type":"text","text":"ABC"}]}],"status":"init"}`,
		`{"id":"zp-1","parts":[{"role":"assistant","status":"finish","content":[{"type":"text","text":"ABC"}]}],"status":"finish"}`,
	)
	_, err := svc.handleWebZhipuNonStreamingResponse(
		context.Background(), resp, c, account, "glm-5.3-flash", "glm-5.3-flash", time.Now(), "hi", webResponseModeChat)
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, recorder.Code)
	var completion map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &completion))
	require.Equal(t, "chat.completion", completion["object"])
	require.Equal(t, "glm-5.3-flash", completion["model"], "model must be backfilled to original request model")
	choices, ok := completion["choices"].([]any)
	require.True(t, ok)
	first := choices[0].(map[string]any)
	message := first["message"].(map[string]any)
	require.Equal(t, "ABC", message["content"], "cumulative text must aggregate to exact full text, no duplication")
	require.Equal(t, "assistant", message["role"])
}

// TestWebZhipuDownstream_UpstreamErrorNoCredentialLeak 覆盖场景5：构造 StatusCode=400、
// Body 含 SECRET_COOKIE_VALUE 字样的响应，经 handleWebZhipuUpstreamError（用含该 cookie
// 凭证的账号）→ 输出不得含 SECRET_COOKIE_VALUE。
func TestWebZhipuDownstream_UpstreamErrorNoCredentialLeak(t *testing.T) {
	const secret = "SECRET_COOKIE_VALUE"
	recorder, c, svc := webZhipuDownstreamHarness(t)
	account := webZhipuTestAccount(9105, map[string]any{"cookie": secret})
	body := `{"message":"authentication failed for cookie ` + secret + `"}`
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	_, err := svc.handleWebZhipuUpstreamError(
		context.Background(), c, account, resp, []byte(body), "glm-5.3-flash")
	require.Error(t, err, "upstream 400 must surface as an error")
	require.NotContains(t, recorder.Body.String(), secret,
		"upstream error body must be redacted; credential cookie must not leak to client")
}

// TestWebZhipuDownstream_WebModelIDAnchor 覆盖场景6：DeepSeek/Kimi 默认模型目录防误改
// 锚点（纯新增测试文件，不改任何既有代码即天然满足）。
func TestWebZhipuDownstream_WebModelIDAnchor(t *testing.T) {
	require.Equal(t, []string{"deepseek-chat", "deepseek-reasoner"}, DefaultWebModelIDs(PlatformWebDeepseek),
		"web-deepseek default model catalogue must not regress")
	require.Equal(t, []string{"kimi-k3"}, DefaultWebModelIDs(PlatformWebKimi),
		"web-kimi default model catalogue must not regress")
}
