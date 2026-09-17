package service

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
	"github.com/tidwall/gjson"
)

// webZhipuTestAccount 构造 web-zhipu 转发测试账号：整串 Cookie 落 credentials
// （含阿里 CDN Cookie，与线上同串携带口径一致），base_url 覆盖默认域名便于断言出站目标。
func webZhipuTestAccount(id int64, credentials map[string]any) *Account {
	if credentials == nil {
		credentials = map[string]any{}
	}
	acc := &Account{
		ID:          id,
		Name:        "web-zhipu-test",
		Platform:    PlatformWebZhipu,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: credentials,
	}
	if _, ok := credentials["cookie"]; !ok {
		acc.Credentials["cookie"] = "chatglm_token=tok-abc; acw_tc=cdn-xyz; cdn_sec_tc=sec-123"
	}
	return acc
}

// webZhipuSSECompletionResponse 构造 200 + SSE 响应（2026-09-17 实测 parts[].content[]
// 形态：text 元素为累积全文，think 元素不进正文，末帧顶层 status=finish）。
func webZhipuSSECompletionResponse() *http.Response {
	sse := strings.Join([]string{
		`data: {"id":"zp-1","parts":[],"status":"init"}`,
		``,
		`data: {"id":"zp-1","parts":[{"role":"assistant","status":"init","content":[{"type":"think","think":"思考"}],"model":"moe_5"}],"status":"init"}`,
		``,
		`data: {"id":"zp-1","parts":[{"role":"assistant","status":"init","content":[{"type":"text","text":"hello"}],"model":"moe_5"}],"status":"init"}`,
		``,
		`data: {"id":"zp-1","parts":[{"role":"assistant","status":"init","content":[{"type":"text","text":"hello world"}],"model":"moe_5"}],"status":"init"}`,
		``,
		`data: {"id":"zp-1","parts":[{"role":"assistant","status":"finish","content":[{"type":"text","text":"hello world"}],"model":"moe_5"}],"status":"finish"}`,
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

// webZhipuRateLimitRepoStub 记录 SetRateLimited（429 兜底冷却写入点）。
type webZhipuRateLimitRepoStub struct {
	stubOpenAIAccountRepo
	setRateLimitedCalls int
}

func (r *webZhipuRateLimitRepoStub) SetRateLimited(_ context.Context, _ int64, _ time.Time) error {
	r.setRateLimitedCalls++
	return nil
}

func (r *webZhipuRateLimitRepoStub) SetError(_ context.Context, _ int64, _ string) error {
	r.setRateLimitedCalls++
	return nil
}

// runForwardWebZhipu 驱动一次 forwardWebZhipu（非流式入站）。
func runForwardWebZhipu(
	t *testing.T,
	account *Account,
	body []byte,
	upstream *httpUpstreamRecorder,
	rlSvc *RateLimitService,
) (*httptest.ResponseRecorder, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))

	svc := &OpenAIGatewayService{
		cfg: &config.Config{
			Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true}},
		},
		httpUpstream:     upstream,
		rateLimitService: rlSvc,
	}

	_, err := svc.forwardWebZhipu(context.Background(), c, account, body, "glm-5.3-flash", false, time.Now(), webResponseModeChat)
	return recorder, err
}

func webZhipuInboundBody(model string) []byte {
	return []byte(`{"model":"` + model + `","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
}

// TestForwardWebZhipu_RequestBuildAndNonStreamAggregate 覆盖：
//   - 出站端点 /chatglm/backend-api/assistant/stream（2026-09-17 实测）；
//   - 认证双载体（Cookie 同串 + Authorization Bearer 从 chatglm_token 提取）+ 指纹头；
//   - 请求体实测结构（assistant_id / chat_type / meta_data.selected_model，无 model 字段）；
//   - base_url 凭证覆盖默认域名；
//   - SSE 回程聚合为单条 chat.completion JSON，模型名回填原始请求模型。
func TestForwardWebZhipu_RequestBuildAndNonStreamAggregate(t *testing.T) {
	account := webZhipuTestAccount(9001, map[string]any{
		"cookie":   "chatglm_token=tok-abc; acw_tc=cdn-xyz; cdn_sec_tc=sec-123",
		"base_url": "https://chatglm.cn",
	})
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		webZhipuSSECompletionResponse(),
	}}

	recorder, err := runForwardWebZhipu(t, account, webZhipuInboundBody("glm-5.3-flash"), upstream, &RateLimitService{})
	require.NoError(t, err)

	require.Len(t, upstream.requests, 1)
	chatReq := upstream.requests[0]
	require.Equal(t, "/chatglm/backend-api/assistant/stream", chatReq.URL.Path)
	require.Equal(t, "chatglm.cn", chatReq.URL.Host, "credentials.base_url must override default base url")
	require.Equal(t, "https://chatglm.cn", chatReq.Header.Get("Origin"))
	require.Equal(t, "https://chatglm.cn/", chatReq.Header.Get("Referer"))
	require.Equal(t, "application/json", chatReq.Header.Get("Content-Type"))
	require.Equal(t, "text/event-stream", chatReq.Header.Get("Accept"))
	require.Equal(t, "chatglm", chatReq.Header.Get("app-name"))
	require.Equal(t, "pc", chatReq.Header.Get("X-App-Platform"))
	require.Equal(t, webZhipuClientUA, chatReq.Header.Get("User-Agent"))

	// Cookie 同串携带：整串登录 Cookie（含 CDN Cookie）原样出现在 Cookie 头。
	cookieHeader := chatReq.Header.Get("Cookie")
	require.Contains(t, cookieHeader, "chatglm_token=tok-abc")
	require.Contains(t, cookieHeader, "acw_tc=cdn-xyz")
	require.Contains(t, cookieHeader, "cdn_sec_tc=sec-123")

	// 请求体：2026-09-17 实测结构（无 model 字段，selected_model + assistant_id 选模型）。
	body := upstream.bodies[0]
	require.False(t, gjson.GetBytes(body, "model").Exists(), "legacy model field must not be sent")
	require.Equal(t, "hi", gjson.GetBytes(body, "messages.0.content.0.text").String())
	require.Equal(t, "user", gjson.GetBytes(body, "messages.0.role").String())
	require.Equal(t, "text", gjson.GetBytes(body, "messages.0.content.0.type").String())
	require.Equal(t, "glm-5.3-flash", gjson.GetBytes(body, "meta_data.selected_model").String())
	require.Equal(t, webZhipuDefaultAssistantID, gjson.GetBytes(body, "assistant_id").String())
	require.Equal(t, "user_chat", gjson.GetBytes(body, "chat_type").String())

	// 回程聚合：SSE → 单条 chat.completion JSON，模型回填原始请求模型。
	// 实测响应无 usage 字段 → 本地估算兜底（D2）。
	require.Equal(t, http.StatusOK, recorder.Code)
	var completion map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &completion))
	require.Equal(t, "chat.completion", completion["object"])
	require.Equal(t, "glm-5.3-flash", completion["model"])
	choices, ok := completion["choices"].([]any)
	require.True(t, ok)
	first, ok := choices[0].(map[string]any)
	require.True(t, ok)
	message, ok := first["message"].(map[string]any)
	require.True(t, ok)
	// think 增量不得混入正文；text 累积全文聚合为完整回答。
	require.Equal(t, "hello world", message["content"])
}

// TestForwardWebZhipu_ModelMappingPassthrough 覆盖模型映射：未配置映射时默认透传。
// 注意：网页端 body 的 selected_model 取（已映射的）模型，映射测试用 model_mapping 命中断言。
func TestForwardWebZhipu_ModelMappingPassthrough(t *testing.T) {
	account := webZhipuTestAccount(9002, nil)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{webZhipuSSECompletionResponse()}}
	_, err := runForwardWebZhipu(t, account, webZhipuInboundBody("glm-5.3-flash"), upstream, &RateLimitService{})
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 1)
	require.Equal(t, "glm-5.3-flash", gjson.GetBytes(upstream.bodies[0], "meta_data.selected_model").String())
}

// TestForwardWebZhipu_StreamMidstreamBusinessError 覆盖流式中段业务错误收口（Codex 审查
// #1）：首帧已写出正文后，上游在流中抛出业务错误码（非 0 code）时，必须向客户端写明确 error
// 标记并终结 SSE，且不得伪造正常 finish_reason+usage 终止帧。
func TestForwardWebZhipu_StreamMidstreamBusinessError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(webZhipuInboundBody("glm-5.3-flash")))
	svc := &OpenAIGatewayService{
		cfg: &config.Config{
			Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true}},
		},
	}
	sse := strings.Join([]string{
		`data: {"id":"zp-1","parts":[{"role":"assistant","status":"init","content":[{"type":"text","text":"hello"}]}],"status":"init"}`,
		``,
		`data: {"code":40002,"msg":"midstream business error"}`,
		``,
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}
	account := webZhipuTestAccount(9020, nil)
	result, err := svc.handleWebZhipuStreamingResponse(
		context.Background(), resp, c, account, "glm-5.3-flash", "glm-5.3-flash", time.Now(), webResponseModeChat)
	require.NoError(t, err, "midstream error is conveyed in-stream, not as a Go error")
	require.True(t, result.Stream)

	out := recorder.Body.String()
	require.Contains(t, out, `"content":"hello"`, "first content frame must be relayed before the error")
	require.Contains(t, out, `"error"`, "midstream error must be marked in-stream")
	require.Contains(t, out, `"upstream_error"`)
	// 错误帧必须是 [DONE] 之前的最后一帧业务帧：中间没有任何正常 finish_reason+usage 终止帧。
	require.Contains(t, out,
		`data: {"error":{"message":"midstream business error","type":"upstream_error"}}`+"\n\n"+`data: [DONE]`,
		"error frame must immediately precede [DONE], with no normal terminal frame in between")
	require.NotContains(t, out, `"usage"`, "midstream error must NOT emit a normal usage terminal frame")
}

// TestForwardWebZhipu_MissingCookieFailsClosed Cookie 缺失必须失败关闭，且不发出
// 任何上游请求；错误信息不得包含任何凭证值。
func TestForwardWebZhipu_MissingCookieFailsClosed(t *testing.T) {
	account := webZhipuTestAccount(9003, map[string]any{"cookie": ""})
	upstream := &httpUpstreamRecorder{}

	_, err := runForwardWebZhipu(t, account, webZhipuInboundBody("glm-5.3-flash"), upstream, &RateLimitService{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "cookie")
	require.Len(t, upstream.requests, 0, "no upstream request may be made without a login cookie")
}

// TestForwardWebZhipu_AuthErrorCoolsAccount 401（实测 Cookie 过期形态）→ 冷却账号，
// 无刷新重试（Zhipu 无刷新机制，分析文档 §3.5），凭证不出现在错误响应。
func TestForwardWebZhipu_AuthErrorCoolsAccount(t *testing.T) {
	repo := &webZhipuRateLimitRepoStub{}
	rlSvc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	const secretCookie = "chatglm_token=SECRETVALUE123456; acw_tc=CDNSECRET1"
	account := webZhipuTestAccount(9004, map[string]any{"cookie": secretCookie})
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		{
			StatusCode: http.StatusUnauthorized,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"message":"You need to be authenticated `+secretCookie+`"}`)),
		},
	}}
	recorder, err := runForwardWebZhipu(t, account, webZhipuInboundBody("glm-5.3-flash"), upstream, rlSvc)
	require.Error(t, err)
	require.Len(t, upstream.requests, 1, "no refresh retry: zhipu has no refresh mechanism")
	require.Equal(t, 1, repo.setRateLimitedCalls, "401 must cool the account via RateLimitService")
	// 凭证脱敏：错误响应不得回显 Cookie（上游错误体回显片段须被脱敏）。
	require.NotContains(t, recorder.Body.String(), "SECRETVALUE123456")
	require.NotContains(t, recorder.Body.String(), "CDNSECRET1")
}

// TestForwardWebZhipu_RateLimitClassification 覆盖 429 分类接入
// RateLimitService.HandleUpstreamError（CN 供应商语义：冷却账号）。
func TestForwardWebZhipu_RateLimitClassification(t *testing.T) {
	repo := &webZhipuRateLimitRepoStub{}
	rlSvc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	account := webZhipuTestAccount(9005, nil)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"message":"rate limited"}`)),
		},
	}}
	recorder, err := runForwardWebZhipu(t, account, webZhipuInboundBody("glm-5.3-flash"), upstream, rlSvc)
	require.Error(t, err)
	require.Equal(t, 1, repo.setRateLimitedCalls, "429 must cool the account via RateLimitService")
	require.NotContains(t, recorder.Body.String(), "tok-abc")
}

// TestForwardWebZhipu_UnitClassify 纯函数分类单测。
func TestForwardWebZhipu_UnitClassify(t *testing.T) {
	require.Equal(t, webZhipuErrKindAuth, classifyWebZhipuUpstreamError(401, nil))
	require.Equal(t, webZhipuErrKindRateLimited, classifyWebZhipuUpstreamError(429, nil))
	require.Equal(t, webZhipuErrKindOther, classifyWebZhipuUpstreamError(500, []byte(`{"message":"boom"}`)))
	require.Equal(t, webZhipuErrKindOther, classifyWebZhipuUpstreamError(200, []byte(`{"message":"ok"}`)))
}
