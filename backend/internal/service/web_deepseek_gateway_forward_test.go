package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
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

// webDeepseekTestAccount 构造 web-deepseek 转发测试账号：整串 Cookie 落 credentials
// （含 WAF Cookie，与线上同串携带口径一致），base_url 覆盖默认域名便于断言出站目标。
func webDeepseekTestAccount(id int64, credentials map[string]any) *Account {
	if credentials == nil {
		credentials = map[string]any{}
	}
	acc := &Account{
		ID:          id,
		Name:        "web-deepseek-test",
		Platform:    PlatformWebDeepseek,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: credentials,
	}
	if _, ok := credentials["cookie"]; !ok {
		acc.Credentials["cookie"] = "ds_session_id=sess-abc; HWWAFSESID=waf-xyz; HWWAFSESTIME=1726450000"
	}
	return acc
}

// webDeepseekSSECompletionBody 构造 SSE 回程体（通用 OpenAI 兼容形状增量；
// 真实 chunk 结构待登录态实测补全）。
func webDeepseekSSECompletionBody() string {
	return strings.Join([]string{
		`data: {"id":"ds-1","choices":[{"delta":{"content":"hello"}}]}`,
		``,
		`data: {"id":"ds-1","choices":[{"delta":{"content":" world"}}]}`,
		``,
		`data: {"id":"ds-1","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
}

// webDeepseekSSECompletionResponse 构造 200 + SSE 响应。
func webDeepseekSSECompletionResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(webDeepseekSSECompletionBody())),
	}
}

// webDeepseekMissingTokenResponse 实测形态：PoW 端点未登录态返回 200 + {"code":40002}。
func webDeepseekMissingTokenResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"code":40002,"msg":"Missing Token"}`)),
	}
}

// webDeepseekRateLimitRepoStub 记录 SetRateLimited（429 兜底冷却写入点）与 SetError
// （401 认证禁用写入点），用于断言 429 / code=40002 分类确实接入了
// RateLimitService.HandleUpstreamError（CN 供应商语义）。
type webDeepseekRateLimitRepoStub struct {
	stubOpenAIAccountRepo
	setRateLimitedCalls int
	setErrorCalls       int
}

func (r *webDeepseekRateLimitRepoStub) SetRateLimited(_ context.Context, _ int64, _ time.Time) error {
	r.setRateLimitedCalls++
	return nil
}

func (r *webDeepseekRateLimitRepoStub) SetError(_ context.Context, _ int64, _ string) error {
	r.setErrorCalls++
	return nil
}

// newWebDeepseekTestRateLimitService 构造带 repo stub 的 RateLimitService（与其它
// service 层测试同口径：仅注入账号 repo，其余依赖为空）。
func newWebDeepseekTestRateLimitService(repo *webDeepseekRateLimitRepoStub) *RateLimitService {
	return NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
}

// runForwardWebDeepseek 驱动一次 forwardWebDeepseek（非流式入站），返回 gin recorder
// 与上游 recorder，并把 forward 错误记入测试对象。
func runForwardWebDeepseek(
	t *testing.T,
	account *Account,
	body []byte,
	model string,
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

	_, err := svc.forwardWebDeepseek(context.Background(), c, account, body, model, false, time.Now(), webResponseModeChat)
	return recorder, err
}

func webDeepseekInboundBody(model string) []byte {
	return []byte(`{"model":"` + model + `","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
}

// TestForwardWebDeepseek_RequestBuildAndNonStreamAggregate 覆盖：
//   - PoW 端点返回实测 40002 形态时按无 PoW 出站；
//   - 出站端点 /api/v0/chat/completion、指纹头（Cookie 同串携带 / Origin / Referer / UA）；
//   - 请求体实测已知字段（chat_session_id / parent_message_id / model_class / prompt /
//     thinking_enabled=false / search_enabled=false）；
//   - base_url 凭证覆盖默认域名；
//   - SSE 回程聚合为单条 chat.completion JSON，模型名回填原始请求模型。
func TestForwardWebDeepseek_RequestBuildAndNonStreamAggregate(t *testing.T) {
	account := webDeepseekTestAccount(8801, map[string]any{
		"cookie":            "ds_session_id=sess-abc; HWWAFSESID=waf-xyz; HWWAFSESTIME=1726450000",
		"base_url":          "https://chat.deepseek.com",
		"chat_session_id":   "sess-42",
		"parent_message_id": "msg-9",
	})
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		webDeepseekMissingTokenResponse(), // PoW 挑战端点
		webDeepseekSSECompletionResponse(),
	}}

	recorder, err := runForwardWebDeepseek(t, account, webDeepseekInboundBody("deepseek-chat"), "deepseek-chat", upstream, &RateLimitService{})
	require.NoError(t, err)

	require.Len(t, upstream.requests, 2, "PoW challenge fetch + completion request")

	powReq := upstream.requests[0]
	require.Equal(t, "/api/v0/chat/create_pow_challenge", powReq.URL.Path)

	chatReq := upstream.requests[1]
	require.Equal(t, "/api/v0/chat/completion", chatReq.URL.Path)
	require.Equal(t, "chat.deepseek.com", chatReq.URL.Host, "credentials.base_url must override default base url")
	require.Equal(t, "https://chat.deepseek.com", chatReq.Header.Get("Origin"))
	require.Equal(t, "https://chat.deepseek.com/", chatReq.Header.Get("Referer"))
	require.Equal(t, "application/json", chatReq.Header.Get("Content-Type"))
	require.Equal(t, webDeepseekClientUA, chatReq.Header.Get("User-Agent"))

	// Cookie 同串携带：整串登录 Cookie（含 WAF Cookie）原样出现在 Cookie 头。
	cookieHeader := chatReq.Header.Get("Cookie")
	require.Contains(t, cookieHeader, "ds_session_id=sess-abc")
	require.Contains(t, cookieHeader, "HWWAFSESID=waf-xyz")
	require.Contains(t, cookieHeader, "HWWAFSESTIME=1726450000")

	// 请求体：实测已知字段。
	body := upstream.bodies[1]
	require.Equal(t, "deepseek_chat", gjson.GetBytes(body, "model_class").String())
	require.Equal(t, "hi", gjson.GetBytes(body, "prompt").String())
	require.Equal(t, "sess-42", gjson.GetBytes(body, "chat_session_id").String())
	require.Equal(t, "msg-9", gjson.GetBytes(body, "parent_message_id").String())
	require.False(t, gjson.GetBytes(body, "thinking_enabled").Bool())
	require.False(t, gjson.GetBytes(body, "search_enabled").Bool())
	require.True(t, gjson.GetBytes(body, "model_class").Exists())
	require.True(t, gjson.GetBytes(body, "prompt").Exists())

	// 回程聚合：SSE → 单条 chat.completion JSON，模型回填原始请求模型，usage 提取。
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "application/json", recorder.Header().Get("Content-Type"))
	var completion map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &completion))
	require.Equal(t, "chat.completion", completion["object"])
	require.Equal(t, "deepseek-chat", completion["model"])
	choices, ok := completion["choices"].([]any)
	require.True(t, ok)
	require.Len(t, choices, 1)
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

// TestForwardWebDeepseek_ReasonerThinkingEnabled 覆盖方案 §3.3：
// deepseek-reasoner → model_class=deepseek_chat + thinking_enabled=true。
func TestForwardWebDeepseek_ReasonerThinkingEnabled(t *testing.T) {
	account := webDeepseekTestAccount(8802, nil)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		webDeepseekMissingTokenResponse(),
		webDeepseekSSECompletionResponse(),
	}}

	_, err := runForwardWebDeepseek(t, account, webDeepseekInboundBody("deepseek-reasoner"), "deepseek-reasoner", upstream, &RateLimitService{})
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 2)
	require.Equal(t, "deepseek_chat", gjson.GetBytes(upstream.bodies[1], "model_class").String())
	require.True(t, gjson.GetBytes(upstream.bodies[1], "thinking_enabled").Bool())
}

// TestForwardWebDeepseek_ModelMappingPassthrough 覆盖模型映射：账号 model_mapping 命中
// 时按映射后的模型归一 model_class；未配置映射时默认透传（未知模型名 model_class 原样）。
func TestForwardWebDeepseek_ModelMappingPassthrough(t *testing.T) {
	// 命中映射：my-alias → deepseek-chat → model_class=deepseek_chat。
	account := webDeepseekTestAccount(8803, map[string]any{
		"model_mapping": map[string]any{"my-alias": "deepseek-chat"},
	})
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		webDeepseekMissingTokenResponse(),
		webDeepseekSSECompletionResponse(),
	}}
	_, err := runForwardWebDeepseek(t, account, webDeepseekInboundBody("my-alias"), "my-alias", upstream, &RateLimitService{})
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 2)
	require.Equal(t, "deepseek_chat", gjson.GetBytes(upstream.bodies[1], "model_class").String())

	// 未配置映射：未知模型名透传同名（方案 §3.3 默认透传；真实取值待登录态实测补全）。
	passthrough := webDeepseekTestAccount(8804, nil)
	upstream2 := &httpUpstreamRecorder{responses: []*http.Response{
		webDeepseekMissingTokenResponse(),
		webDeepseekSSECompletionResponse(),
	}}
	_, err = runForwardWebDeepseek(t, passthrough, webDeepseekInboundBody("custom-model"), "custom-model", upstream2, &RateLimitService{})
	require.NoError(t, err)
	require.Len(t, upstream2.bodies, 2)
	require.Equal(t, "custom-model", gjson.GetBytes(upstream2.bodies[1], "model_class").String())
}

// runForwardWebDeepseekStream 驱动一次流式入站的 forwardWebDeepseek，返回 gin recorder。
func runForwardWebDeepseekStream(
	t *testing.T,
	account *Account,
	body []byte,
	model string,
	upstream *httpUpstreamRecorder,
) *httptest.ResponseRecorder {
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
		rateLimitService: &RateLimitService{},
	}
	_, err := svc.forwardWebDeepseek(context.Background(), c, account, body, model, true, time.Now(), webResponseModeChat)
	require.NoError(t, err)
	return recorder
}

func webDeepseekStreamInboundBody() []byte {
	return []byte(`{"model":"deepseek-chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
}

// TestForwardWebDeepseek_StreamingResponseRelay 覆盖流式回程：上游 SSE 增量经通用映射
// 重包为 chat.completion.chunk 流，终止帧收口 data: [DONE]，SSE 头齐备，usage 记入结果。
func TestForwardWebDeepseek_StreamingResponseRelay(t *testing.T) {
	account := webDeepseekTestAccount(8810, map[string]any{"base_url": "https://chat.deepseek.com"})
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		webDeepseekMissingTokenResponse(),
		webDeepseekSSECompletionResponse(),
	}}

	recorder := runForwardWebDeepseekStream(t, account, webDeepseekStreamInboundBody(), "deepseek-chat", upstream)

	require.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
	out := recorder.Body.String()
	require.True(t, strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]"), "stream must terminate with [DONE], got: %s", out)
	require.Contains(t, out, `"content":"hello"`)
	require.Contains(t, out, `"content":" world"`)
	require.Contains(t, out, `"finish_reason":"stop"`)
	require.Contains(t, out, `"model":"deepseek-chat"`)
	require.Contains(t, out, `"object":"chat.completion.chunk"`)
}

// TestForwardWebDeepseek_MissingCookieFailsClosed Cookie 缺失必须失败关闭，且不发出
// 任何上游请求；错误信息不得包含任何凭证值。
func TestForwardWebDeepseek_MissingCookieFailsClosed(t *testing.T) {
	account := webDeepseekTestAccount(8805, map[string]any{"cookie": ""})
	upstream := &httpUpstreamRecorder{}

	_, err := runForwardWebDeepseek(t, account, webDeepseekInboundBody("deepseek-chat"), "deepseek-chat", upstream, &RateLimitService{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "cookie")
	require.Len(t, upstream.requests, 0, "no upstream request may be made without a login cookie")
}

// TestForwardWebDeepseek_PoWSolvedCarriesHeader 上游返回可解 challenge（实测
// data.biz_data.challenge 结构）时，求解成功且对话请求携带 x-ds-pow-response 头。
func TestForwardWebDeepseek_PoWSolvedCarriesHeader(t *testing.T) {
	account := webDeepseekTestAccount(8806, nil)
	// 用 nonce=5 求解出的 challenge（确定性：solvableNonceChallenge 在包内计算）。
	digest := webDeepseekPowStateDigest([]byte("salt123_1739764288699_5"))
	challengeHex := hex.EncodeToString(digest[:])
	challengeBody := `{"code":0,"msg":"","data":{"biz_code":0,"biz_msg":"","biz_data":{"challenge":{"algorithm":"DeepSeekHashV1","challenge":"` + challengeHex + `","salt":"salt123","signature":"sig","difficulty":100,"expire_at":1739764288699,"target_path":"/api/v0/chat/completion"}}}}`
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		&http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(challengeBody)),
		},
		&http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(webDeepseekSSECompletionBody())),
		},
	}}

	recorder, err := runForwardWebDeepseek(t, account, webDeepseekInboundBody("deepseek-chat"), "deepseek-chat", upstream, &RateLimitService{})
	require.NoError(t, err)
	require.True(t, recorder.Body.Len() > 0)
	require.Len(t, upstream.requests, 2, "challenge + completion requests expected")
	powHeader := upstream.requests[1].Header.Get("X-Ds-PoW-Response")
	require.NotEmpty(t, powHeader, "completion request must carry x-ds-pow-response header")
	// 头值可解码且 answer 为数值。
	payload, decodeErr := base64.StdEncoding.DecodeString(powHeader)
	require.NoError(t, decodeErr)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(payload, &parsed))
	answer, ok := parsed["answer"].(float64)
	require.True(t, ok, "answer must be a number, got: %v", parsed["answer"])
	require.Equal(t, float64(5), answer)
	require.Equal(t, challengeHex, parsed["challenge"])
}

// TestForwardWebDeepseek_RateLimitClassification 覆盖 429 / code=40002 分类接入
// RateLimitService.HandleUpstreamError（CN 供应商语义：冷却账号）。
func TestForwardWebDeepseek_RateLimitClassification(t *testing.T) {
	newAccount := func(id int64) *Account { return webDeepseekTestAccount(id, nil) }

	t.Run("http_429", func(t *testing.T) {
		repo := &webDeepseekRateLimitRepoStub{}
		rlSvc := newWebDeepseekTestRateLimitService(repo)
		account := newAccount(8807)
		upstream := &httpUpstreamRecorder{responses: []*http.Response{
			webDeepseekMissingTokenResponse(),
			&http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":429,"msg":"rate limited"}`)),
			},
		}}
		recorder, err := runForwardWebDeepseek(t, account, webDeepseekInboundBody("deepseek-chat"), "deepseek-chat", upstream, rlSvc)
		require.Error(t, err)
		require.Equal(t, 1, repo.setRateLimitedCalls, "429 must cool the account via RateLimitService")
		// 凭证脱敏：错误响应不得回显 Cookie。
		require.NotContains(t, recorder.Body.String(), "ds_session_id=sess-abc")
		require.NotContains(t, recorder.Body.String(), "waf-xyz")
	})

	t.Run("http_200_code_40002", func(t *testing.T) {
		repo := &webDeepseekRateLimitRepoStub{}
		rlSvc := newWebDeepseekTestRateLimitService(repo)
		account := newAccount(8808)
		upstream := &httpUpstreamRecorder{responses: []*http.Response{
			webDeepseekMissingTokenResponse(),
			// 实测形态：业务错误码随 HTTP 200 返回（{"code":40002,"msg":"Missing Token"}）。
			&http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":40002,"msg":"Missing Token"}`)),
			},
		}}
		recorder, err := runForwardWebDeepseek(t, account, webDeepseekInboundBody("deepseek-chat"), "deepseek-chat", upstream, rlSvc)
		_ = recorder
		require.Error(t, err)
		require.Equal(t, 1, repo.setRateLimitedCalls, "code=40002 (HTTP 200) must be classified as rate limit and cool the account")
	})

	t.Run("unit_classify", func(t *testing.T) {
		require.Equal(t, webDeepseekErrKindRateLimited, classifyWebDeepseekUpstreamError(429, nil))
		require.Equal(t, webDeepseekErrKindRateLimited, classifyWebDeepseekUpstreamError(200, []byte(`{"code":40002,"msg":"Missing Token"}`)))
		require.Equal(t, webDeepseekErrKindOther, classifyWebDeepseekUpstreamError(200, []byte(`{"code":0}`)))
		require.Equal(t, webDeepseekErrKindOther, classifyWebDeepseekUpstreamError(500, []byte(`{"code":500,"msg":"boom"}`)))
	})
}

// TestForwardWebDeepseek_CredentialNeverInClientResponse 凭证不出现在任何客户端可见
// 错误响应中（含缺 Cookie 与上游错误两条路径）。
func TestForwardWebDeepseek_CredentialNeverInClientResponse(t *testing.T) {
	const secretCookie = "ds_session_id=SECRETVALUE123456; HWWAFSESID=WAFSECRETVALUE1"
	account := webDeepseekTestAccount(8809, map[string]any{
		"cookie":   secretCookie,
		"base_url": "https://chat.deepseek.com",
	})
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		webDeepseekMissingTokenResponse(),
		// 上游错误体回显 Cookie 片段（最坏情况），必须被脱敏后再透传。
		&http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"code":401,"msg":"bad session ` + secretCookie + `"}`)),
		},
	}}
	repo := &webDeepseekRateLimitRepoStub{}
	recorder, err := runForwardWebDeepseek(t, account, webDeepseekInboundBody("deepseek-chat"), "deepseek-chat", upstream, newWebDeepseekTestRateLimitService(repo))
	require.Error(t, err)
	require.Equal(t, 1, repo.setErrorCalls, "401 must be handled as auth error via RateLimitService")
	require.NotContains(t, recorder.Body.String(), "SECRETVALUE123456")
	require.NotContains(t, recorder.Body.String(), "WAFSECRETVALUE1")
}
