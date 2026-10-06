package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// makeFakeWebZhipuJWT 手工拼一个假 chatglm_token JWT（header.payload.sig），仅用于测试。
// 不携带任何真实凭证，payload 按入参编码；sig 段为占位串。
func makeFakeWebZhipuJWT(t *testing.T, payload map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	body := base64.RawURLEncoding.EncodeToString(raw)
	return header + "." + body + ".fakesig"
}

// webZhipuTestAccount 构造 zhipu 网页转发测试账号：整串 Cookie 落 credentials
// （含阿里 CDN Cookie，与线上同串携带口径一致），base_url 覆盖默认域名便于断言出站目标。
func webZhipuTestAccount(id int64, credentials map[string]any) *Account {
	if credentials == nil {
		credentials = map[string]any{}
	}
	acc := &Account{
		ID:          id,
		Name:        "zhipu-web-test",
		Platform:    PlatformZhipu,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: credentials,
	}
	if _, ok := credentials["cookie"]; !ok {
		acc.Credentials["cookie"] = "chatglm_token=tok-abc; acw_tc=cdn-xyz; cdn_sec_tc=sec-123"
	}
	if _, ok := credentials["access_mode"]; !ok {
		acc.Credentials["access_mode"] = AccountAccessModeWeb
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

	// 签名三件套 + X-Request-Id（2026-09-17 官方 main.js 逆向 + 抓包验证）：
	// 13 位数字时间戳 / 32 hex nonce / 32 hex sign / 32 hex request-id。
	xTimestamp := chatReq.Header.Get("X-Timestamp")
	require.Regexp(t, regexp.MustCompile(`^\d{13}$`), xTimestamp, "X-Timestamp must be 13 digits")
	xNonce := chatReq.Header.Get("X-Nonce")
	require.Regexp(t, regexp.MustCompile(`^[0-9a-f]{32}$`), xNonce, "X-Nonce must be 32 lower-hex")
	xSign := chatReq.Header.Get("X-Sign")
	require.Regexp(t, regexp.MustCompile(`^[0-9a-f]{32}$`), xSign, "X-Sign must be 32 lower-hex")
	require.Regexp(t, regexp.MustCompile(`^[0-9a-f]{32}$`), chatReq.Header.Get("X-Request-Id"), "X-Request-Id must be 32 lower-hex")
	// 自洽性：X-Sign 必须能由出站 X-Timestamp + X-Nonce 重算得到。
	require.Equal(t, webZhipuSignFrom(xTimestamp, xNonce), xSign, "X-Sign must match x-timestamp + x-nonce")

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

// TestForwardWebZhipu_AuthErrorCoolsAccount 401（实测 Cookie 过期形态）→ 冷却账号；
// 本账号 Cookie 无 chatglm_refresh_token，故不发刷新请求、直接冷却，凭证不出现在错误响应。
func TestForwardWebZhipu_AuthErrorCoolsAccount(t *testing.T) {
	repo := &webZhipuRateLimitRepoStub{}
	rlSvc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	const secretCookie = "chatglm_token=SECRETVALUE123456; acw_tc=CDNSECRET1"
	account := webZhipuTestAccount(9004, map[string]any{"cookie": secretCookie})
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		{
			StatusCode: http.StatusUnauthorized,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"message":"You need to be authenticated ` + secretCookie + `"}`)),
		},
	}}
	recorder, err := runForwardWebZhipu(t, account, webZhipuInboundBody("glm-5.3-flash"), upstream, rlSvc)
	require.Error(t, err)
	require.Len(t, upstream.requests, 1, "no refresh request without chatglm_refresh_token")
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

// runForwardWebZhipuWithModel 与 runForwardWebZhipu 同构，但允许指定入站原始模型名
// （用于未知模型失败关闭测试）。
func runForwardWebZhipuWithModel(
	t *testing.T,
	account *Account,
	body []byte,
	upstream *httpUpstreamRecorder,
	rlSvc *RateLimitService,
	originalModel string,
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

	_, err := svc.forwardWebZhipu(context.Background(), c, account, body, originalModel, false, time.Now(), webResponseModeChat)
	return recorder, err
}

// TestValidateWebZhipuModel 覆盖 ValidateWebZhipuModel：默认目录内模型通过；空串、
// glm-4、glm-4.7、未知值必须报错。
func TestValidateWebZhipuModel(t *testing.T) {
	// 允许：默认目录内的实测模型。
	require.NoError(t, ValidateWebZhipuModel("glm-5.3-flash"))
	// 拒绝：空串 / 已知旧模型 / 未知值（均不在当前默认目录内）。
	for _, m := range []string{"", "glm-4", "glm-4.7", "glm-999"} {
		require.Error(t, ValidateWebZhipuModel(m), "model %q must be rejected", m)
	}
	// 错误信息仅含模型名，不泄露敏感信息。
	require.Contains(t, ValidateWebZhipuModel("glm-999").Error(), "glm-999")
	require.Contains(t, ValidateWebZhipuModel("").Error(), "empty")
}

// TestForwardWebZhipu_UnknownModelFailsClosed 未知模型入站必须失败关闭，且不发出任何
// 上游请求（与 "zhipu web requires at least one user message" 同风格，直接 return error）。
func TestForwardWebZhipu_UnknownModelFailsClosed(t *testing.T) {
	account := webZhipuTestAccount(9103, nil)
	upstream := &httpUpstreamRecorder{}

	_, err := runForwardWebZhipuWithModel(t, account, webZhipuInboundBody("glm-999"), upstream, &RateLimitService{}, "glm-999")
	require.Error(t, err)
	require.Contains(t, err.Error(), "glm-999")
	require.Len(t, upstream.requests, 0, "no upstream request may be made for an unsupported model")
}

// TestWebZhipuResolveAssistantID 覆盖 assistant_id 单一事实来源：凭证覆盖生效；
// 无凭证回落默认公共助手 ID。
func TestWebZhipuResolveAssistantID(t *testing.T) {
	withOverride := webZhipuTestAccount(9101, map[string]any{"assistant_id": "custom-assistant-123"})
	require.Equal(t, "custom-assistant-123", webZhipuResolveAssistantID(withOverride))

	noCred := webZhipuTestAccount(9102, nil)
	require.Equal(t, webZhipuDefaultAssistantID, webZhipuResolveAssistantID(noCred))
}

// TestWebZhipuSignTimestamp 覆盖官方 x-timestamp 变换（2026-09-17 main.js 逆向 + 抓包验证）：
//   - 黄金用例：输入 "1789648837735"（真实抓包实测 now）输出必须仍为 "1789648837735"；
//   - 非回文用例：输入 1700000000001，按公式手工算得 digits sum=9、digits[len-2]=0、
//     t=9、t%10=9，输出 "1700000000091"。
func TestWebZhipuSignTimestamp(t *testing.T) {
	require.Equal(t, "1789648837735", webZhipuSignTimestamp(1789648837735),
		"golden captured now must be invariant under the transform")
	require.Equal(t, "1700000000091", webZhipuSignTimestamp(1700000000001),
		"non-palindrome case hand-computed from the formula")
}

// TestWebZhipuComputeSign 覆盖 x-sign 生成黄金用例（2026-09-17 真实抓包验证值）：
// 输入 ts="1789648837735"、nonce="68c8bc6d488c4869961703710a72eb33" 时
// x-sign 必须是 "3a19f494b85cd19deb788467b154e76e"。
func TestWebZhipuComputeSign(t *testing.T) {
	got := webZhipuSignFrom("1789648837735", "68c8bc6d488c4869961703710a72eb33")
	require.Equal(t, "3a19f494b85cd19deb788467b154e76e", got,
		"x-sign must match the 2026-09-17 captured value")
}

// TestWebZhipuTokenIsGuest 覆盖游客态判定纯函数：
//   - 游客 JWT（payload is_guest=true）→ true；
//   - 真实登录态形态（无 is_guest 字段）→ false（不得误伤）；
//   - is_guest=false → false；
//   - 乱串 → false；空串 → false。
//
// 全部用手工拼的假 JWT，不携带任何真实凭证。
func TestWebZhipuTokenIsGuest(t *testing.T) {
	guest := makeFakeWebZhipuJWT(t, map[string]any{"is_guest": true, "device_id": "dev-guest"})
	real := makeFakeWebZhipuJWT(t, map[string]any{"device_id": "dev-real"})
	explicitFalse := makeFakeWebZhipuJWT(t, map[string]any{"is_guest": false, "device_id": "dev-real"})

	require.True(t, webZhipuTokenIsGuest(guest), "is_guest=true must be detected as guest")
	require.False(t, webZhipuTokenIsGuest(real), "real login token without is_guest must NOT be treated as guest")
	require.False(t, webZhipuTokenIsGuest(explicitFalse), "is_guest=false must NOT be treated as guest")
	require.False(t, webZhipuTokenIsGuest("garbage-not-a-jwt"), "garbage token must NOT be treated as guest")
	require.False(t, webZhipuTokenIsGuest(""), "empty token must NOT be treated as guest")
}

// TestForwardWebZhipu_GuestTokenFailsClosed 游客态 token（is_guest=true）必须在发出任何
// 上游请求前失败关闭：mock 上游 recorder 断言 0 次上游请求；错误信息含 "guest"；错误信息
// 不得含任何 token 内容（安全红线）。
func TestForwardWebZhipu_GuestTokenFailsClosed(t *testing.T) {
	guestJWT := makeFakeWebZhipuJWT(t, map[string]any{"is_guest": true, "device_id": "guest-device-id"})
	account := webZhipuTestAccount(9110, map[string]any{
		"cookie": "chatglm_token=" + guestJWT + "; acw_tc=cdn-xyz",
	})
	upstream := &httpUpstreamRecorder{}

	_, err := runForwardWebZhipu(t, account, webZhipuInboundBody("glm-5.3-flash"), upstream, &RateLimitService{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "guest", "guest failure must be diagnosable from the message")
	require.Len(t, upstream.requests, 0, "guest token must not issue any upstream request")

	// 安全红线：错误信息不得含 token / device_id 等凭证内容。
	require.NotContains(t, err.Error(), guestJWT, "error message must not leak the token")
	require.NotContains(t, err.Error(), "guest-device-id", "error message must not leak token payload")
}

// --- D1: zhipu 网页刷新成功后把新凭证持久化到账号凭据（同构 kimi 网页刷新链） ---

// webZhipuRefreshRepoStub 记录 UpdateCredentials 调用（刷新成功持久化断言用）。
// 全部用手工拼的假值，不携带任何真实 Cookie / JWT。
type webZhipuRefreshRepoStub struct {
	AccountRepository // 嵌入接口：其余方法提升为 nil（本测试不调用）
	updatedID         int64
	updatedCreds      map[string]any
}

func (r *webZhipuRefreshRepoStub) UpdateCredentials(_ context.Context, id int64, creds map[string]any) error {
	r.updatedID = id
	r.updatedCreds = creds
	return nil
}

// webZhipuRefreshResponse 构造刷新端点响应（status / body 由调用方给定）。
func webZhipuRefreshResponse(code int, body string) *http.Response {
	return &http.Response{
		StatusCode: code,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(body))),
	}
}

func TestRefreshWebZhipuAccessTokenPersistsCredentials(t *testing.T) {
	t.Run("cookie_fields_replaced_others_preserved", func(t *testing.T) {
		account := &Account{
			ID:       9801,
			Platform: PlatformZhipu,
			Credentials: map[string]any{
				"access_mode": AccountAccessModeWeb,
				"cookie":      "chatglm_token=old-access; chatglm_refresh_token=old-refresh; acw_tc=cdn-xyz; cdn_sec_tc=sec-123",
			},
		}
		repo := &webZhipuRefreshRepoStub{}
		svc := &OpenAIGatewayService{
			accountRepo: repo,
			httpUpstream: &httpUpstreamRecorder{resp: webZhipuRefreshResponse(http.StatusOK,
				`{"result":{"access_token":"new-access","refresh_token":"new-refresh"}}`)},
		}

		got := svc.refreshWebZhipuAccessToken(context.Background(), account)
		require.Equal(t, "new-access", got, "应返回刷新后的 access_token")

		require.Equal(t, int64(9801), repo.updatedID)
		// cookie 中两字段已替换。
		newCookie := repo.updatedCreds["cookie"].(string)
		require.Contains(t, newCookie, "chatglm_token=new-access")
		require.Contains(t, newCookie, "chatglm_refresh_token=new-refresh")
		// 其余 cookie 字段原样保留。
		require.Contains(t, newCookie, "acw_tc=cdn-xyz")
		require.Contains(t, newCookie, "cdn_sec_tc=sec-123")
		require.NotContains(t, newCookie, "old-access")
		require.NotContains(t, newCookie, "old-refresh")

		// 账号内存凭据同步更新。
		require.Equal(t, newCookie, account.Credentials["cookie"])

		// 原凭证无显式 chatglm_token / refresh_token 键 → 不应凭空新增。
		_, hasChatglmToken := repo.updatedCreds["chatglm_token"]
		_, hasRefreshToken := repo.updatedCreds["refresh_token"]
		require.False(t, hasChatglmToken, "原凭证无显式键时不应新增 chatglm_token")
		require.False(t, hasRefreshToken, "原凭证无显式键时不应新增 refresh_token")
	})

	t.Run("explicit_keys_synced_when_present", func(t *testing.T) {
		account := &Account{
			ID:       9802,
			Platform: PlatformZhipu,
			Credentials: map[string]any{
				"access_mode":   AccountAccessModeWeb,
				"cookie":        "chatglm_token=old-access; chatglm_refresh_token=old-refresh; acw_tc=cdn-xyz",
				"chatglm_token": "old-access",
				"refresh_token": "old-refresh",
			},
		}
		repo := &webZhipuRefreshRepoStub{}
		svc := &OpenAIGatewayService{
			accountRepo: repo,
			httpUpstream: &httpUpstreamRecorder{resp: webZhipuRefreshResponse(http.StatusOK,
				`{"result":{"access_token":"new-access","refresh_token":"new-refresh"}}`)},
		}

		got := svc.refreshWebZhipuAccessToken(context.Background(), account)
		require.Equal(t, "new-access", got)
		require.Equal(t, "new-access", repo.updatedCreds["chatglm_token"], "显式 chatglm_token 应同步写新 access token")
		require.Equal(t, "new-refresh", repo.updatedCreds["refresh_token"], "显式 refresh_token 应同步写新 refresh token")
		require.Contains(t, repo.updatedCreds["cookie"].(string), "chatglm_token=new-access")
	})
}

func TestRefreshWebZhipuAccessTokenNoPersistOnFailure(t *testing.T) {
	account := &Account{
		ID:       9803,
		Platform: PlatformZhipu,
		Credentials: map[string]any{
			"access_mode": AccountAccessModeWeb,
			"cookie":      "chatglm_token=old-access; chatglm_refresh_token=old-refresh; acw_tc=cdn-xyz",
		},
	}
	repo := &webZhipuRefreshRepoStub{}
	// 刷新端点返回非 200 → 刷新失败，不持久化。
	svc := &OpenAIGatewayService{
		accountRepo:  repo,
		httpUpstream: &httpUpstreamRecorder{resp: webZhipuRefreshResponse(http.StatusUnauthorized, `{"message":"invalid refresh token"}`)},
	}
	got := svc.refreshWebZhipuAccessToken(context.Background(), account)
	require.Equal(t, "", got)
	require.Equal(t, int64(0), repo.updatedID, "刷新失败不应写库")
	require.Nil(t, repo.updatedCreds, "刷新失败不应写库")
}

func TestRefreshWebZhipuAccessTokenNoRefreshTokenNoRequest(t *testing.T) {
	account := &Account{
		ID:       9804,
		Platform: PlatformZhipu,
		Credentials: map[string]any{
			// 整串 Cookie 中无 chatglm_refresh_token，也无显式 refresh_token / chatglm_token。
			"access_mode": AccountAccessModeWeb,
			"cookie":      "chatglm_token=old-access; acw_tc=cdn-xyz",
		},
	}
	// 若误发刷新请求，返回 200 空 JSON（newAccessToken 空 → 返回 ""），由 requests 数断言捕获。
	upstream := &httpUpstreamRecorder{resp: webZhipuRefreshResponse(http.StatusOK, `{}`)}
	svc := &OpenAIGatewayService{
		accountRepo:  &webZhipuRefreshRepoStub{},
		httpUpstream: upstream,
	}
	got := svc.refreshWebZhipuAccessToken(context.Background(), account)
	require.Equal(t, "", got, "无 refresh token 应返回空")
	require.Len(t, upstream.requests, 0, "无 refresh token 不应发出刷新请求")
}

// TestForwardWebZhipu_RefreshOn401 覆盖 401 → refresh_token 刷新 → 重试一次：
//   - 刷新成功：刷新端点收到 Bearer <chatglm_refresh_token> 与空 JSON 体，成功后用新 token 重试；
//   - 刷新失败（401）：按原 401 走冷却与错误路径，凭证不出现在错误响应，且不写库。
func TestForwardWebZhipu_RefreshOn401(t *testing.T) {
	t.Run("refresh_success_then_retry", func(t *testing.T) {
		account := webZhipuTestAccount(9805, map[string]any{
			"cookie": "chatglm_token=old-access; chatglm_refresh_token=old-refresh; acw_tc=cdn-xyz",
		})
		upstream := &httpUpstreamRecorder{responses: []*http.Response{
			{ // 第一次对话：401
				StatusCode: http.StatusUnauthorized,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"message":"expired"}`)),
			},
			webZhipuRefreshResponse(http.StatusOK, `{"result":{"access_token":"new-access","refresh_token":"new-refresh"}}`),
			webZhipuSSECompletionResponse(), // 重试：成功
		}}
		recorder, err := runForwardWebZhipu(t, account, webZhipuInboundBody("glm-5.3-flash"), upstream, &RateLimitService{})
		require.NoError(t, err)
		require.Len(t, upstream.requests, 3, "chat 401 + refresh + chat retry")

		// 1) 首次对话请求：Bearer 旧 access token。
		require.Equal(t, "/chatglm/backend-api/assistant/stream", upstream.requests[0].URL.Path)
		require.Equal(t, "Bearer old-access", upstream.requests[0].Header.Get("Authorization"))

		// 2) 刷新请求：端点、Bearer 旧 refresh token、空 JSON 体、指纹头。
		refreshReq := upstream.requests[1]
		require.Equal(t, "/user-api/user/refresh", refreshReq.URL.Path)
		require.Equal(t, "Bearer old-refresh", refreshReq.Header.Get("Authorization"))
		require.Equal(t, "chatglm", refreshReq.Header.Get("app-name"))
		require.Equal(t, "pc", refreshReq.Header.Get("x-app-platform"))
		require.NotEmpty(t, refreshReq.Header.Get("x-sign"), "刷新请求须带签名头")
		require.JSONEq(t, `{}`, string(upstream.bodies[1]), "刷新请求体为空 JSON")

		// 3) 重试请求：Bearer 新 access token，Cookie 中两字段已更新。
		retryReq := upstream.requests[2]
		require.Equal(t, "/chatglm/backend-api/assistant/stream", retryReq.URL.Path)
		require.Equal(t, "Bearer new-access", retryReq.Header.Get("Authorization"))
		require.Contains(t, retryReq.Header.Get("Cookie"), "chatglm_token=new-access")
		require.Contains(t, retryReq.Header.Get("Cookie"), "chatglm_refresh_token=new-refresh")
		require.Contains(t, retryReq.Header.Get("Cookie"), "acw_tc=cdn-xyz")

		// 账号内存凭据同步更新（无 accountRepo 时仍就地更新，供重试使用）。
		require.Contains(t, account.Credentials["cookie"], "chatglm_token=new-access")
		require.Equal(t, http.StatusOK, recorder.Code)
	})

	t.Run("refresh_failure_cools_account", func(t *testing.T) {
		repo := &webZhipuRateLimitRepoStub{}
		rlSvc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
		account := webZhipuTestAccount(9806, map[string]any{
			"cookie": "chatglm_token=old-access; chatglm_refresh_token=old-refresh; acw_tc=cdn-xyz",
		})
		upstream := &httpUpstreamRecorder{responses: []*http.Response{
			{ // 第一次对话：401
				StatusCode: http.StatusUnauthorized,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"message":"expired"}`)),
			},
			webZhipuRefreshResponse(http.StatusUnauthorized, `{"message":"invalid refresh token"}`), // 刷新端点：401
		}}
		recorder, err := runForwardWebZhipu(t, account, webZhipuInboundBody("glm-5.3-flash"), upstream, rlSvc)
		require.Error(t, err)
		require.Len(t, upstream.requests, 2, "chat 401 + refresh attempt; no third request")
		require.Equal(t, 1, repo.setRateLimitedCalls, "刷新失败必须回落到 401 错误路径并冷却账号")
		// 刷新失败不写库：cookie 原样保留。
		require.Contains(t, account.Credentials["cookie"], "chatglm_token=old-access")
		require.NotContains(t, account.Credentials["cookie"], "new-access")
		// 凭证脱敏：错误响应不得回显 token。
		require.NotContains(t, recorder.Body.String(), "old-access")
		require.NotContains(t, recorder.Body.String(), "old-refresh")
	})
}
