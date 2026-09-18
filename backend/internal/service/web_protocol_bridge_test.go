package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// webBridgeRateLimitRepoStub 是 #2/#3 转发测试的账号 repo 桩：实现 SetRateLimited /
// SetError，供 deepseek 业务错误码（code=40002）归一 429 后触发冷却副作用时不致 nil panic。
type webBridgeRateLimitRepoStub struct {
	stubOpenAIAccountRepo
	setRateLimitedCalls int
}

func (r *webBridgeRateLimitRepoStub) SetRateLimited(_ context.Context, _ int64, _ time.Time) error {
	r.setRateLimitedCalls++
	return nil
}

func (r *webBridgeRateLimitRepoStub) SetError(_ context.Context, _ int64, _ string) error {
	r.setRateLimitedCalls++
	return nil
}

// webBridgeTestAccount 构造某网页逆向平台转发测试账号（base_url 强制指向官方域名，
// 便于 #2 跨协议回桥测试不需关心转发层 base_url 校验）。
func webBridgeTestAccount(platform string, id int64) *Account {
	switch platform {
	case PlatformWebDeepseek:
		return webDeepseekTestAccount(id, map[string]any{"base_url": "https://chat.deepseek.com"})
	case PlatformWebZhipu:
		return webZhipuTestAccount(id, map[string]any{"base_url": "https://chatglm.cn"})
	case PlatformWebKimi:
		return webKimiTestAccount(id, map[string]any{"base_url": "https://www.kimi.com"})
	}
	return nil
}

// webBridgeUpstream 按平台与是否流式返回对应的 mock 上游响应序列。
//   - deepseek 始终先消耗一个 PoW 挑战响应（未登录态 40002 形态），再给对话响应；
//   - zhipu / kimi 无 PoW，仅一个对话响应；
//   - kimi 非流式走 Connect RPC 单 JSON（正文落在 message.blocks），流式走 SSE。
func webBridgeUpstream(platform string, stream bool) []*http.Response {
	switch platform {
	case PlatformWebDeepseek:
		// 新协议（2026-09-18 登录态实测）：可解 PoW 挑战 → 自动建会话 → 实测 SSE fixture。
		return []*http.Response{
			webDeepseekSolvablePowChallengeResponse(),
			webDeepseekSessionCreateResponse(),
			webDeepseekFixtureSSEResponse(),
		}
	case PlatformWebZhipu:
		return []*http.Response{webZhipuSSECompletionResponse()}
	case PlatformWebKimi:
		// 新协议（2026-09-18 登录态实测）：Connect RPC envelope 流（流式/非流式同载荷）。
		if stream {
			return []*http.Response{webKimiStreamResponse()}
		}
		return []*http.Response{webKimiConnectResponse()}
	}
	return nil
}

func webBridgeModel(platform string) string {
	switch platform {
	case PlatformWebDeepseek:
		return "deepseek-chat"
	case PlatformWebZhipu:
		// 2026-09-17 实测目录：glm-5.3-flash（ValidateWebZhipuModel 对目录外模型失败关闭）。
		return "glm-5.3-flash"
	case PlatformWebKimi:
		return "kimi-k3"
	}
	return ""
}

// webBridgeExpectedContent 返回该平台在 mock 上游下应聚合出的正文（用于断言响应形态）。
func webBridgeExpectedContent(platform string, stream bool) string {
	switch platform {
	case PlatformWebDeepseek:
		// 实测 fixture（raw/deepseek-sse-decoded.txt 回放）RESPONSE fragment 全文。
		return "哈哈，我又好～ 你好我也好 😄  \n今天有什么想聊的，或者需要我帮忙的吗？"
	case PlatformWebZhipu:
		return "hello world"
	case PlatformWebKimi:
		// envelope fixture 载荷聚合正文（流式/非流式同载荷）。
		return "hi there"
	}
	return ""
}

// runWebForwardForMode 驱动一次指定出站协议（mode）的 forwardWeb*，返回 gin recorder。
// 在非流式与流式两种入站下都断言转发成功（无错误）。
func runWebForwardForMode(
	t *testing.T,
	platform string,
	account *Account,
	model string,
	stream bool,
	mode webResponseMode,
	upstream *httpUpstreamRecorder,
) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{}`)))

	svc := &OpenAIGatewayService{
		cfg: &config.Config{
			Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true}},
		},
		httpUpstream:     upstream,
		rateLimitService: NewRateLimitService(&webBridgeRateLimitRepoStub{}, nil, &config.Config{}, nil, nil),
	}

	body := []byte(`{"model":"` + model + `","stream":` + strconv.FormatBool(stream) + `,"messages":[{"role":"user","content":"hi"}]}`)
	ctx := context.Background()

	var err error
	switch platform {
	case PlatformWebDeepseek:
		_, err = svc.forwardWebDeepseek(ctx, c, account, body, model, stream, time.Now(), mode)
	case PlatformWebZhipu:
		_, err = svc.forwardWebZhipu(ctx, c, account, body, model, stream, time.Now(), mode)
	case PlatformWebKimi:
		_, err = svc.forwardWebKimi(ctx, c, account, body, model, stream, time.Now(), mode)
	}
	require.NoError(t, err, "forwardWeb* must succeed for a valid mock upstream")
	return recorder
}

// TestWebProtocolBridge_ResponseShape 覆盖 #2：三平台 × 三出站协议（chat / responses /
// anthropic）×（非流式 JSON 形态 + 流式事件形态）的响应形态断言。三个 forward 内部只
// 产出通用 chat.completion 包络，出站协议形态全部由 web_protocol_bridge.go 复用既有
// apicompat 回桥在写边界统一回桥。
func TestWebProtocolBridge_ResponseShape(t *testing.T) {
	platforms := []string{PlatformWebDeepseek, PlatformWebZhipu, PlatformWebKimi}
	modes := []struct {
		name string
		mode webResponseMode
	}{
		{"chat", webResponseModeChat},
		{"responses", webResponseModeResponses},
		{"anthropic", webResponseModeAnthropic},
	}

	for _, platform := range platforms {
		model := webBridgeModel(platform)
		for _, m := range modes {
			// 非流式：JSON 响应体形态断言。
			t.Run(platform+"/"+m.name+"/nonstream", func(t *testing.T) {
				account := webBridgeTestAccount(platform, 7001)
				rec := runWebForwardForMode(t, platform, account, model, false, m.mode,
					&httpUpstreamRecorder{responses: webBridgeUpstream(platform, false)})
				require.Equal(t, http.StatusOK, rec.Code)

				switch m.mode {
				case webResponseModeChat:
					var out map[string]any
					require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
					require.Equal(t, "chat.completion", out["object"], "chat mode must emit OpenAI chat.completion")
					require.Equal(t, model, out["model"])
				case webResponseModeResponses:
					var out map[string]any
					require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
					require.Equal(t, "response", out["object"], "responses mode must emit Responses API object")
					require.Equal(t, "completed", out["status"])
					require.Equal(t, model, out["model"])
				case webResponseModeAnthropic:
					var out map[string]any
					require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
					require.Equal(t, "message", out["type"], "anthropic mode must emit Anthropic message")
					require.Equal(t, "assistant", out["role"])
				}
				// 正文须正确透出（跨回桥不丢内容）：JSON 体按 JSON 转义形态比对。
				expectedJSON, err := json.Marshal(webBridgeExpectedContent(platform, false))
				require.NoError(t, err)
				require.Contains(t, rec.Body.String(), string(expectedJSON[1:len(expectedJSON)-1]),
					"aggregated content must survive the protocol bridge")
				// 非流式不得出现 SSE 帧前缀。
				require.NotContains(t, rec.Body.String(), "event:")
			})

			// 流式：SSE 事件形态断言。
			t.Run(platform+"/"+m.name+"/stream", func(t *testing.T) {
				account := webBridgeTestAccount(platform, 7002)
				rec := runWebForwardForMode(t, platform, account, model, true, m.mode,
					&httpUpstreamRecorder{responses: webBridgeUpstream(platform, true)})
				require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))

				body := rec.Body.String()
				switch m.mode {
				case webResponseModeChat:
					// chat 模式：标准 OpenAI 流终止于 data: [DONE]，无 Responses/Anthropic 事件。
					require.True(t, strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]"), "chat stream must end with [DONE], got: %s", body)
					require.Contains(t, body, `"object":"chat.completion.chunk"`)
					require.NotContains(t, body, "event:")
				case webResponseModeResponses:
					// responses 模式：首事件 response.created + 终止事件 response.completed，无 [DONE]。
					require.Contains(t, body, "event: response.created", "Responses stream must open with response.created")
					require.Contains(t, body, "event: response.completed", "Responses stream must close with response.completed")
					require.Contains(t, body, "event: response.output_text.delta")
					require.NotContains(t, body, "data: [DONE]", "Responses stream must not emit OpenAI [DONE]")
				case webResponseModeAnthropic:
					// anthropic 模式：首事件 message_start + 终止事件 message_stop，无 [DONE]。
					require.Contains(t, body, "event: message_start", "Anthropic stream must open with message_start")
					require.Contains(t, body, "event: message_stop", "Anthropic stream must close with message_stop")
					require.Contains(t, body, "event: content_block_delta")
					require.NotContains(t, body, "data: [DONE]", "Anthropic stream must not emit OpenAI [DONE]")
				}
				// 流式正文跨回桥仍以增量透出（分帧边界与上游一致，正文不丢不改序）：
				// 按出站协议逐帧提取 delta 并聚合后与期望正文全等。
				require.Equal(t, webBridgeExpectedContent(platform, true),
					webBridgeStreamAggregatedContent(t, m.mode, body))
			})
		}
	}
}


// webBridgeStreamAggregatedContent 按出站协议从 SSE 流帧中逐行提取正文增量并聚合：
//   - chat：choices[0].delta.content（OpenAI chat.completion.chunk）；
//   - responses：type=response.output_text.delta 的顶层 delta 字段；
//   - anthropic：type=content_block_delta 的 delta.text 字段。
func webBridgeStreamAggregatedContent(t *testing.T, mode webResponseMode, body string) string {
	t.Helper()
	var sb strings.Builder
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			continue
		}
		switch mode {
		case webResponseModeChat:
			if choices, ok := frame["choices"].([]any); ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]any); ok {
					if delta, ok := choice["delta"].(map[string]any); ok {
						if content, ok := delta["content"].(string); ok {
							sb.WriteString(content)
						}
					}
				}
			}
		case webResponseModeResponses:
			if typ, _ := frame["type"].(string); typ == "response.output_text.delta" {
				if delta, ok := frame["delta"].(string); ok {
					sb.WriteString(delta)
				}
			}
		case webResponseModeAnthropic:
			if typ, _ := frame["type"].(string); typ == "content_block_delta" {
				if delta, ok := frame["delta"].(map[string]any); ok {
					if text, ok := delta["text"].(string); ok {
						sb.WriteString(text)
					}
				}
			}
		}
	}
	return sb.String()
}

// --- #3：流式业务错误（裸 JSON 200 错误 / SSE 业务错误码）不得被漏判为正常流并伪 [DONE] ---

// runWebForwardForModeErr 驱动一次指定出站协议的 forwardWeb*，返回 recorder 与错误
// （错误路径下 forward 返回非 nil error）。
func runWebForwardForModeErr(
	t *testing.T,
	platform string,
	account *Account,
	model string,
	stream bool,
	mode webResponseMode,
	upstream *httpUpstreamRecorder,
) (*httptest.ResponseRecorder, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{}`)))

	svc := &OpenAIGatewayService{
		cfg: &config.Config{
			Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true}},
		},
		httpUpstream:     upstream,
		rateLimitService: NewRateLimitService(&webBridgeRateLimitRepoStub{}, nil, &config.Config{}, nil, nil),
	}

	body := []byte(`{"model":"` + model + `","stream":` + strconv.FormatBool(stream) + `,"messages":[{"role":"user","content":"hi"}]}`)
	ctx := context.Background()

	var err error
	switch platform {
	case PlatformWebDeepseek:
		_, err = svc.forwardWebDeepseek(ctx, c, account, body, model, stream, time.Now(), mode)
	case PlatformWebZhipu:
		_, err = svc.forwardWebZhipu(ctx, c, account, body, model, stream, time.Now(), mode)
	case PlatformWebKimi:
		_, err = svc.forwardWebKimi(ctx, c, account, body, model, stream, time.Now(), mode)
	}
	return recorder, err
}

// webBareJSONErrorResponse 构造「HTTP 200 + 裸 JSON 业务错误（无 data: 前缀）」响应
// （实测形态 {"code":40002,"msg":"..."}），原 parseWebDeepseekSSEFrame 会忽略此行 → 漏判。
func webBareJSONErrorResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"code":40002,"msg":"Missing Token"}`)),
	}
}

// webSSEFrameBusinessErrorResponse 构造「HTTP 200 + SSE 帧携带业务错误码」响应：首帧即
// data: {"code":40002,...}（顶层非 0 code 字段，与 mapWebXxxPayload 的错误码读取口径一致）。
func webSSEFrameBusinessErrorResponse() *http.Response {
	sse := strings.Join([]string{
		`data: {"id":"x-1","code":40002,"msg":"Missing Token","choices":[{"delta":{}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}
}

func TestWebStreamingBusinessError_NotFakeDone(t *testing.T) {
	// 三平台 ×（裸 JSON 200 错误 / 流帧业务错误码）均须走错误路径，不得伪 [DONE]。
	// DeepSeek 走 SSE data: 帧错误；Kimi 新协议（Connect envelope）对应为 envelope 错误帧。
	platforms := []string{PlatformWebDeepseek, PlatformWebZhipu, PlatformWebKimi}
	for _, platform := range platforms {
		model := webBridgeModel(platform)
		account := webBridgeTestAccount(platform, 7100)

		t.Run(platform+"/bare_json_200_error", func(t *testing.T) {
			// deepseek 需先消耗一个 PoW 挑战响应，再给裸 JSON 错误（对话端点 HTTP 200）。
			var upstream []*http.Response
			switch platform {
			case PlatformWebDeepseek:
				// 新协议三跳：可解 PoW → 建会话 → completion 返回 HTTP 200 裸 JSON 业务错误。
				upstream = []*http.Response{
					webDeepseekSolvablePowChallengeResponse(),
					webDeepseekSessionCreateResponse(),
					webBareJSONErrorResponse(),
				}
			case PlatformWebKimi:
				// Connect envelope 业务错误帧（数字 code 非 0，与 webKimiBareJSONError 同口径）。
				upstream = []*http.Response{webKimiEnvelopeResponse([]string{`{"code":40002,"message":"Missing Token"}`})}
			default:
				upstream = []*http.Response{webBareJSONErrorResponse()}
			}
			rec, err := runWebForwardForModeErr(t, platform, account, model, true, webResponseModeChat,
				&httpUpstreamRecorder{responses: upstream})
			require.Error(t, err, "bare-JSON HTTP 200 business error must enter the upstream error path")
			require.NotContains(t, rec.Body.String(), "data: [DONE]", "must NOT emit a fake [DONE] for a business error")
		})

		t.Run(platform+"/sse_frame_business_error_code", func(t *testing.T) {
			var upstream []*http.Response
			switch platform {
			case PlatformWebDeepseek:
				// 三跳前缀后，completion SSE 流首帧携带顶层非 0 code（解析器 applyDelta 双路径判定）。
				upstream = []*http.Response{
					webDeepseekSolvablePowChallengeResponse(),
					webDeepseekSessionCreateResponse(),
					webSSEFrameBusinessErrorResponse(),
				}
			case PlatformWebKimi:
				// Connect envelope 协议原生业务错误形态（字符串 code=unauthenticated，实测 401 对称形态）。
				upstream = []*http.Response{webKimiEnvelopeResponse([]string{`{"code":"unauthenticated","message":"login expired midstream"}`})}
			default:
				upstream = []*http.Response{webSSEFrameBusinessErrorResponse()}
			}
			rec, err := runWebForwardForModeErr(t, platform, account, model, true, webResponseModeChat,
				&httpUpstreamRecorder{responses: upstream})
			require.Error(t, err, "SSE frame carrying a non-zero business code must enter the upstream error path")
			require.NotContains(t, rec.Body.String(), "data: [DONE]", "must NOT emit a fake [DONE] for a business error")
		})
	}
}

// TestWebBareJSONErrorHelper 覆盖三个平台的 webXxxBareJSONError 纯函数：
//   - 非 0 数字的 code 字段 → 业务错误（isError=true）；
//   - 无 code / code=0 → 非错误（isError=false，纯内容裸 JSON 安全忽略）。
func TestWebBareJSONErrorHelper(t *testing.T) {
	require.True(t, isWebBareJSONError(`{"code":40002,"msg":"x"}`))
	require.False(t, isWebBareJSONError(`{"code":0}`), "code=0 is not a business error")
	require.False(t, isWebBareJSONError(`{"content":"hi"}`), "pure content JSON with no code is not an error")
	require.False(t, isWebBareJSONError(`data: {"code":40002}`), "SSE data: prefixed lines are not bare JSON")
	require.False(t, isWebBareJSONError(``))
}

// isWebBareJSONError 统一判定一行裸 JSON（非 SSE）是否为业务错误，覆盖三平台的同口径 helper。
func isWebBareJSONError(line string) bool {
	_, ok := webDeepseekBareJSONError(line)
	if ok {
		return true
	}
	if _, ok := webZhipuBareJSONError(line); ok {
		return true
	}
	if _, ok := webKimiBareJSONError(line); ok {
		return true
	}
	return false
}
