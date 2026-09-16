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

// webKimiTestAccount 构造 web-kimi 转发测试账号：access_token 落 credentials，
// base_url 覆盖默认域名便于断言出站目标。
func webKimiTestAccount(id int64, credentials map[string]any) *Account {
	if credentials == nil {
		credentials = map[string]any{}
	}
	acc := &Account{
		ID:          id,
		Name:        "web-kimi-test",
		Platform:    PlatformWebKimi,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: credentials,
	}
	if _, ok := credentials["access_token"]; !ok {
		acc.Credentials["access_token"] = "kimi-access-token-abc123"
	}
	return acc
}

// webKimiSSEResponse 构造 200 + SSE 响应（通用 OpenAI 兼容形状增量，映射函数按
// 「通用 SSE JSON」解析；真实 chunk 结构待登录态实测补全）。
func webKimiSSEResponse() *http.Response {
	sse := strings.Join([]string{
		`data: {"id":"kimi-1","choices":[{"delta":{"content":"hello"}}]}`,
		``,
		`data: {"id":"kimi-1","choices":[{"delta":{"content":" world"}}]}`,
		``,
		`data: {"id":"kimi-1","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`,
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

// webKimiConnectResponse 构造非流式 Connect RPC JSON 响应（正文落在已验证请求结构的
// 对称位置 message.blocks[].content.value.content；响应侧结构待登录态实测补全）。
func webKimiConnectResponse() *http.Response {
	body := `{"id":"kimi-2","message":{"blocks":[{"content":{"case":"text","value":{"$typeName":"kimi.chat.v1.TextBlock","content":"hi there"}}}]}}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// webKimiUnauthenticatedResponse 实测形态：401 unauthenticated。
func webKimiUnauthenticatedResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"code":"unauthenticated","message":"unauthenticated"}`)),
	}
}

// runForwardWebKimi 驱动一次 forwardWebKimi（非流式入站），originalModel 由调用方传入
// （与入站 body 的 model 字段保持一致），返回 gin recorder 与上游 recorder。
func runForwardWebKimi(
	t *testing.T,
	account *Account,
	body []byte,
	originalModel string,
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

	_, err := svc.forwardWebKimi(context.Background(), c, account, body, originalModel, false, time.Now())
	return recorder, err
}

func webKimiInboundBody(model string) []byte {
	return []byte(`{"model":"` + model + `","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
}

// TestForwardWebKimi_RequestBuildAndNonStreamAggregate 覆盖：
//   - 出站端点 /apiv2/kimi.chat.v1.ChatService/Chat、Connect RPC 头（Bearer / Origin / Referer / UA）；
//   - 请求体已验证结构（kimiPlusId=ok-computer、message.blocks text、options.model、role=1）；
//   - base_url 凭证覆盖默认域名；
//   - 非流式 Connect 响应聚合为单条 chat.completion JSON，模型名回填原始请求模型。
func TestForwardWebKimi_RequestBuildAndNonStreamAggregate(t *testing.T) {
	account := webKimiTestAccount(8901, map[string]any{
		"access_token": "kimi-access-token-abc123",
		"base_url":     "https://kimi.example.com",
	})
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		webKimiConnectResponse(),
	}}

	recorder, err := runForwardWebKimi(t, account, webKimiInboundBody("kimi-k3"), "kimi-k3", upstream, &RateLimitService{})
	require.NoError(t, err)

	require.Len(t, upstream.requests, 1)
	chatReq := upstream.requests[0]
	require.Equal(t, "/apiv2/kimi.chat.v1.ChatService/Chat", chatReq.URL.Path)
	require.Equal(t, "kimi.example.com", chatReq.URL.Host, "credentials.base_url must override default base url")
	require.Equal(t, "Bearer kimi-access-token-abc123", chatReq.Header.Get("Authorization"))
	require.Equal(t, "application/json", chatReq.Header.Get("Content-Type"))
	require.Equal(t, "https://www.kimi.com", chatReq.Header.Get("Origin"))
	require.Equal(t, webKimiClientUA, chatReq.Header.Get("User-Agent"))

	// 请求体：已验证结构（方案 §4.3）。
	body := upstream.bodies[0]
	require.Equal(t, "ok-computer", gjson.GetBytes(body, "kimiPlusId").String())
	require.Equal(t, "k3", gjson.GetBytes(body, "options.model").String())
	require.False(t, gjson.GetBytes(body, "options.thinking").Bool())
	require.Equal(t, float64(1), gjson.GetBytes(body, "message.role").Float())
	require.Equal(t, "hi", gjson.GetBytes(body, "message.blocks.0.content.value.content").String())
	require.Equal(t, "text", gjson.GetBytes(body, "message.blocks.0.content.case").String())
	require.True(t, gjson.GetBytes(body, "message.references.$typeName").Exists())

	// 回程聚合：Connect JSON → 单条 chat.completion，模型回填原始请求模型。
	require.Equal(t, http.StatusOK, recorder.Code)
	var completion map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &completion))
	require.Equal(t, "chat.completion", completion["object"])
	require.Equal(t, "kimi-k3", completion["model"])
	choices, ok := completion["choices"].([]any)
	require.True(t, ok)
	first, ok := choices[0].(map[string]any)
	require.True(t, ok)
	message, ok := first["message"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "hi there", message["content"])
}

// TestForwardWebKimi_ModelMappingPassthrough 覆盖模型映射（方案 §3.3 实测映射：
// kimi-k3 → k3、kimi-k3-agent-ultra → k3-agent-ultra；未配置映射时默认透传）。
func TestForwardWebKimi_ModelMappingPassthrough(t *testing.T) {
	cases := []struct {
		inbound  string
		expected string
	}{
		{"kimi-k3", "k3"},
		{"kimi-k3-agent-ultra", "k3-agent-ultra"},
		{"custom-model", "custom-model"},
	}
	for i, tc := range cases {
		account := webKimiTestAccount(int64(8910+i), nil)
		upstream := &httpUpstreamRecorder{responses: []*http.Response{webKimiConnectResponse()}}
		_, err := runForwardWebKimi(t, account, webKimiInboundBody(tc.inbound), tc.inbound, upstream, &RateLimitService{})
		require.NoError(t, err)
		require.Len(t, upstream.bodies, 1)
		require.Equal(t, tc.expected, gjson.GetBytes(upstream.bodies[0], "options.model").String())
	}
}

// TestForwardWebKimi_MissingTokenFailsClosed access_token 缺失必须失败关闭，且不发出
// 任何上游请求；错误信息不得包含任何凭证值。
func TestForwardWebKimi_MissingTokenFailsClosed(t *testing.T) {
	account := webKimiTestAccount(8920, map[string]any{"access_token": ""})
	upstream := &httpUpstreamRecorder{}

	_, err := runForwardWebKimi(t, account, webKimiInboundBody("kimi-k3"), "kimi-k3", upstream, &RateLimitService{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "access_token")
	require.Len(t, upstream.requests, 0, "no upstream request may be made without access_token")
}

// TestForwardWebKimi_RefreshOn401 覆盖 401 → refresh_token 刷新 → 重试一次：
//   - 刷新端点收到 refresh_token，成功提取新 access_token（常见位置，待实测收敛）后重试；
//   - 刷新失败（401）时按原 401 走冷却与错误路径，凭证不出现在错误响应。
func TestForwardWebKimi_RefreshOn401(t *testing.T) {
	t.Run("refresh_success_then_retry", func(t *testing.T) {
		account := webKimiTestAccount(8921, map[string]any{
			"access_token":  "old-token-abc12345",
			"refresh_token": "refresh-token-abc12345",
		})
		upstream := &httpUpstreamRecorder{responses: []*http.Response{
			webKimiUnauthenticatedResponse(), // 第一次对话：401
			{ // 刷新端点：返回新 access_token（常见位置，响应结构待实测收敛）
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"accessToken":"new-token-xyz98765"}`)),
			},
			webKimiConnectResponse(), // 重试：成功
		}}
		recorder, err := runForwardWebKimi(t, account, webKimiInboundBody("kimi-k3"), "kimi-k3", upstream, &RateLimitService{})
		require.NoError(t, err)
		require.Len(t, upstream.requests, 3, "chat 401 + refresh + chat retry")
		require.Equal(t, "/apiv2/kimi.chat.v1.ChatService/Chat", upstream.requests[0].URL.Path)
		require.Equal(t, "/api/account.gateway.v1.AuthService/RefreshToken", upstream.requests[1].URL.Path)
		require.Contains(t, string(upstream.bodies[1]), "refresh-token-abc12345")
		require.Equal(t, "Bearer new-token-xyz98765", upstream.requests[2].Header.Get("Authorization"), "retry must use refreshed access token")
		require.Equal(t, http.StatusOK, recorder.Code)
	})

	t.Run("refresh_failure_cools_account", func(t *testing.T) {
		repo := &webKimiRateLimitRepoStub{}
		rlSvc := newWebKimiTestRateLimitService(repo)
		account := webKimiTestAccount(8922, map[string]any{
			"access_token":  "old-token-abc12345",
			"refresh_token": "refresh-token-abc12345",
		})
		upstream := &httpUpstreamRecorder{responses: []*http.Response{
			webKimiUnauthenticatedResponse(), // 第一次对话：401
			webKimiUnauthenticatedResponse(), // 刷新端点：401
		}}
		recorder, err := runForwardWebKimi(t, account, webKimiInboundBody("kimi-k3"), "kimi-k3", upstream, rlSvc)
		require.Error(t, err)
		require.Len(t, upstream.requests, 2, "chat 401 + refresh attempt; no third request")
		require.Equal(t, 1, repo.setRateLimitedCalls, "refresh failure must fall back to the 401 error path and cool the account")
		// 凭证脱敏：错误响应不得回显 token。
		require.NotContains(t, recorder.Body.String(), "old-token-abc12345")
		require.NotContains(t, recorder.Body.String(), "refresh-token-abc12345")
	})
}

// TestForwardWebKimi_RateLimitClassification 覆盖 429 分类接入
// RateLimitService.HandleUpstreamError（CN 供应商语义：冷却账号）。
func TestForwardWebKimi_RateLimitClassification(t *testing.T) {
	repo := &webKimiRateLimitRepoStub{}
	rlSvc := newWebKimiTestRateLimitService(repo)
	account := webKimiTestAccount(8923, nil)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"code":"resource_exhausted","message":"rate limited"}`)),
		},
	}}
	recorder, err := runForwardWebKimi(t, account, webKimiInboundBody("kimi-k3"), "kimi-k3", upstream, rlSvc)
	require.Error(t, err)
	require.Equal(t, 1, repo.setRateLimitedCalls, "429 must cool the account via RateLimitService")
	require.NotContains(t, recorder.Body.String(), "kimi-access-token-abc123")
}

// TestForwardWebKimi_401WithoutRefreshToken 直接 401 且无 refresh_token：走错误路径冷却。
func TestForwardWebKimi_401WithoutRefreshToken(t *testing.T) {
	repo := &webKimiRateLimitRepoStub{}
	rlSvc := newWebKimiTestRateLimitService(repo)
	account := webKimiTestAccount(8924, nil)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		webKimiUnauthenticatedResponse(),
	}}
	_, err := runForwardWebKimi(t, account, webKimiInboundBody("kimi-k3"), "kimi-k3", upstream, rlSvc)
	require.Error(t, err)
	require.Len(t, upstream.requests, 1, "no refresh attempt without refresh_token")
	require.Equal(t, 1, repo.setRateLimitedCalls)
}

// TestForwardWebKimi_StreamingResponse 覆盖流式回程：SSE 增量重包为
// chat.completion.chunk 流 + 终止 [DONE]，usage 提取。
func TestForwardWebKimi_StreamingResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := webKimiInboundBody("kimi-k3")
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))

	svc := &OpenAIGatewayService{
		cfg: &config.Config{
			Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true}},
		},
	}
	account := webKimiTestAccount(8925, nil)
	result, err := svc.handleWebKimiStreamingResponse(
		context.Background(), webKimiSSEResponse(), c, account, "kimi-k3", "k3", time.Now())
	require.NoError(t, err)
	require.True(t, result.Stream)

	out := recorder.Body.String()
	require.Contains(t, out, `"content":"hello"`)
	require.Contains(t, out, `"content":" world"`)
	require.Contains(t, out, "chat.completion.chunk")
	require.Contains(t, out, "data: [DONE]")
}

// TestForwardWebKimi_UnrecognizedShapeFailsClosed 非 SSE 且结构不可识别：失败关闭，
// 不伪造成功响应。
func TestForwardWebKimi_UnrecognizedShapeFailsClosed(t *testing.T) {
	account := webKimiTestAccount(8926, nil)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"weird":"shape"}`)),
		},
	}}
	recorder, err := runForwardWebKimi(t, account, webKimiInboundBody("kimi-k3"), "kimi-k3", upstream, &RateLimitService{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unrecognized")
	_ = recorder
}

// TestForwardWebKimi_UnitClassify 纯函数分类单测。
func TestForwardWebKimi_UnitClassify(t *testing.T) {
	require.Equal(t, webKimiErrKindAuth, classifyWebKimiUpstreamError(401, nil))
	require.Equal(t, webKimiErrKindAuth, classifyWebKimiUpstreamError(200, []byte(`{"code":"unauthenticated"}`)))
	require.Equal(t, webKimiErrKindRateLimited, classifyWebKimiUpstreamError(429, nil))
	require.Equal(t, webKimiErrKindOther, classifyWebKimiUpstreamError(500, []byte(`{"code":"internal"}`)))
}

// webKimiRateLimitRepoStub 记录 SetRateLimited（冷却写入点）。
type webKimiRateLimitRepoStub struct {
	stubOpenAIAccountRepo
	setRateLimitedCalls int
}

func (r *webKimiRateLimitRepoStub) SetRateLimited(_ context.Context, _ int64, _ time.Time) error {
	r.setRateLimitedCalls++
	return nil
}

func (r *webKimiRateLimitRepoStub) SetError(_ context.Context, _ int64, _ string) error {
	r.setRateLimitedCalls++
	return nil
}

// newWebKimiTestRateLimitService 构造带 repo stub 的 RateLimitService（与其它 service
// 层测试同口径：仅注入账号 repo，其余依赖为空）。
func newWebKimiTestRateLimitService(repo *webKimiRateLimitRepoStub) *RateLimitService {
	return NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
}
