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

// webZhipuSSECompletionResponse 构造 200 + SSE 响应（通用 OpenAI 兼容形状增量，映射
// 函数按「通用 SSE JSON」解析；真实 chunk 结构待登录态实测补全）。
func webZhipuSSECompletionResponse() *http.Response {
	sse := strings.Join([]string{
		`data: {"id":"zp-1","choices":[{"delta":{"content":"hello"}}]}`,
		``,
		`data: {"id":"zp-1","choices":[{"delta":{"content":" world"}}]}`,
		``,
		`data: {"id":"zp-1","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`,
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

	_, err := svc.forwardWebZhipu(context.Background(), c, account, body, "glm-4", false, time.Now())
	return recorder, err
}

func webZhipuInboundBody(model string) []byte {
	return []byte(`{"model":"` + model + `","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
}

// TestForwardWebZhipu_RequestBuildAndNonStreamAggregate 覆盖：
//   - 出站端点 /chatglm/backend-api/v1/conversation、指纹头（Cookie 同串携带 /
//     X-Requested-With / Origin / Referer / UA）；
//   - 请求体实测已知字段（prompt / model）；
//   - base_url 凭证覆盖默认域名；
//   - SSE 回程聚合为单条 chat.completion JSON，模型名回填原始请求模型。
func TestForwardWebZhipu_RequestBuildAndNonStreamAggregate(t *testing.T) {
	account := webZhipuTestAccount(9001, map[string]any{
		"cookie":   "chatglm_token=tok-abc; acw_tc=cdn-xyz; cdn_sec_tc=sec-123",
		"base_url": "https://chatglm.example.com",
	})
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		webZhipuSSECompletionResponse(),
	}}

	recorder, err := runForwardWebZhipu(t, account, webZhipuInboundBody("glm-4"), upstream, &RateLimitService{})
	require.NoError(t, err)

	require.Len(t, upstream.requests, 1)
	chatReq := upstream.requests[0]
	require.Equal(t, "/chatglm/backend-api/v1/conversation", chatReq.URL.Path)
	require.Equal(t, "chatglm.example.com", chatReq.URL.Host, "credentials.base_url must override default base url")
	require.Equal(t, "https://chatglm.example.com", chatReq.Header.Get("Origin"))
	require.Equal(t, "https://chatglm.example.com/", chatReq.Header.Get("Referer"))
	require.Equal(t, "application/json", chatReq.Header.Get("Content-Type"))
	require.Equal(t, "XMLHttpRequest", chatReq.Header.Get("X-Requested-With"))
	require.Equal(t, webZhipuClientUA, chatReq.Header.Get("User-Agent"))

	// Cookie 同串携带：整串登录 Cookie（含 CDN Cookie）原样出现在 Cookie 头。
	cookieHeader := chatReq.Header.Get("Cookie")
	require.Contains(t, cookieHeader, "chatglm_token=tok-abc")
	require.Contains(t, cookieHeader, "acw_tc=cdn-xyz")
	require.Contains(t, cookieHeader, "cdn_sec_tc=sec-123")

	// 请求体：实测已知字段（prompt / model）。
	body := upstream.bodies[0]
	require.Equal(t, "hi", gjson.GetBytes(body, "prompt").String())
	require.Equal(t, "glm-4", gjson.GetBytes(body, "model").String())

	// 回程聚合：SSE → 单条 chat.completion JSON，模型回填原始请求模型，usage 提取。
	require.Equal(t, http.StatusOK, recorder.Code)
	var completion map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &completion))
	require.Equal(t, "chat.completion", completion["object"])
	require.Equal(t, "glm-4", completion["model"])
	choices, ok := completion["choices"].([]any)
	require.True(t, ok)
	first, ok := choices[0].(map[string]any)
	require.True(t, ok)
	message, ok := first["message"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "hello world", message["content"])
	usage, ok := completion["usage"].(map[string]any)
	require.True(t, ok)
	require.EqualValues(t, 3, usage["input_tokens"])
	require.EqualValues(t, 2, usage["output_tokens"])
}

// TestForwardWebZhipu_ModelMappingPassthrough 覆盖模型映射：未配置映射时默认透传
// （方案 §3.3 默认透传；真实模型名取值待登录态实测补全）。
// 注意：网页端 body 的 model 字段取（已映射的）模型，映射测试用 model_mapping 命中断言。
func TestForwardWebZhipu_ModelMappingPassthrough(t *testing.T) {
	account := webZhipuTestAccount(9002, nil)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{webZhipuSSECompletionResponse()}}
	_, err := runForwardWebZhipu(t, account, webZhipuInboundBody("glm-4"), upstream, &RateLimitService{})
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 1)
	require.Equal(t, "glm-4", gjson.GetBytes(upstream.bodies[0], "model").String())
}

// TestForwardWebZhipu_MissingCookieFailsClosed Cookie 缺失必须失败关闭，且不发出
// 任何上游请求；错误信息不得包含任何凭证值。
func TestForwardWebZhipu_MissingCookieFailsClosed(t *testing.T) {
	account := webZhipuTestAccount(9003, map[string]any{"cookie": ""})
	upstream := &httpUpstreamRecorder{}

	_, err := runForwardWebZhipu(t, account, webZhipuInboundBody("glm-4"), upstream, &RateLimitService{})
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
	recorder, err := runForwardWebZhipu(t, account, webZhipuInboundBody("glm-4"), upstream, rlSvc)
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
	recorder, err := runForwardWebZhipu(t, account, webZhipuInboundBody("glm-4"), upstream, rlSvc)
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
