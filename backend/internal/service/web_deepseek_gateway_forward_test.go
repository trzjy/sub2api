package service

import (
	"bufio"
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

// webDeepseekTestAccount 构造 deepseek 网页转发测试账号：整串 Cookie 落 credentials
// （含 WAF Cookie，与线上同串携带口径一致），base_url 覆盖默认域名便于断言出站目标。
func webDeepseekTestAccount(id int64, credentials map[string]any) *Account {
	if credentials == nil {
		credentials = map[string]any{}
	}
	credentials["access_mode"] = AccountAccessModeWeb
	acc := &Account{
		ID:          id,
		Name:        "deepseek-web-test",
		Platform:    PlatformDeepseek,
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

// webDeepseekFixtureSSE 是登录态实测 SSE 流全文（来源 raw/deepseek-sse-decoded.txt，09 §5）。
// 含 ready / update_session / THINK fragment（思考，须丢弃）/ RESPONSE fragment（正文）/
// BATCH accumulated_token_usage(42→112) / response/status FINISHED / close 终止；
// 关键覆盖 delta 无 p 省略形态（首帧带 p/o，后续仅 v）。
const webDeepseekFixtureSSE = `event: ready
data: {"request_message_id":3,"response_message_id":4,"model_type":"default"}

event: update_session
data: {"updated_at":1789671814.046746}

data: {"v":{"response":{"message_id":4,"parent_id":3,"model":"","role":"ASSISTANT","thinking_enabled":true,"ban_edit":false,"ban_regenerate":false,"status":"WIP","incomplete_message":null,"accumulated_token_usage":42,"feedback":null,"inserted_at":1789671814.035786,"search_enabled":true,"fragments":[{"id":2,"type":"THINK","content":"我们需要","elapsed_secs":null,"references":[],"stage_id":1}],"conversation_mode":"DEFAULT","has_pending_fragment":false,"auto_continue":false,"search_triggered":false,"extra_search_providers":[]}}}

data: {"p":"response/fragments/-1/content","o":"APPEND","v":"回答"}

data: {"v":"用户"}

data: {"v":"。"}

data: {"p":"response/fragments/-1/elapsed_secs","o":"SET","v":1.220857351}

data: {"p":"response/fragments","o":"APPEND","v":[{"id":3,"type":"RESPONSE","content":"哈哈","references":[],"stage_id":1}]}

data: {"p":"response/fragments/-1/content","v":"，"}

data: {"v":"我又"}

data: {"v":"好"}

data: {"v":"～"}

data: {"v":" "}

data: {"v":"你好"}

data: {"v":"我也"}

data: {"v":"好"}

data: {"v":" 😄"}

data: {"v":"  \n"}

data: {"v":"今天"}

data: {"v":"有什么"}

data: {"v":"想"}

data: {"v":"聊"}

data: {"v":"的"}

data: {"v":"，"}

data: {"v":"或者"}

data: {"v":"需要"}

data: {"v":"我"}

data: {"v":"帮忙"}

data: {"v":"的吗"}

data: {"v":"？"}

data: {"p":"response","o":"BATCH","v":[{"p":"accumulated_token_usage","v":112},{"p":"quasi_status","v":"FINISHED"}]}

data: {"p":"response/status","o":"SET","v":"FINISHED"}

event: update_session
data: {"updated_at":1789671814.72859}

event: close
data: {"click_behavior":"none","auto_resume":false}
`

// webDeepseekParseFixture 用实测 SSE 流全文回放解析器，返回解析结果。
func webDeepseekParseFixture(t *testing.T) *webDeepseekSSEParser {
	t.Helper()
	parser := newWebDeepseekSSEParser()
	scanner := bufio.NewScanner(strings.NewReader(webDeepseekFixtureSSE))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for {
		name, data, ok := webDeepseekNextSSEEvent(scanner)
		if !ok {
			break
		}
		switch name {
		case "ready":
			if id := gjson.Get(data, "response_message_id").Int(); id != 0 {
				parser.responseID = gjson.Get(data, "response_message_id").String()
			}
			continue
		case "close", "finish":
			continue
		}
		parser.applyDelta(data)
	}
	require.NoError(t, scanner.Err())
	return parser
}

// TestWebDeepseekSSEParser_FixtureReplay 覆盖 SSE 解析器回放（09 §5）：正文提取（THINK 丢弃）、
// ready 的 response_message_id、accumulated_token_usage 最终值（112）、close 终止、无 p 省略形态。
func TestWebDeepseekSSEParser_FixtureReplay(t *testing.T) {
	parser := webDeepseekParseFixture(t)

	// 正文只取 RESPONSE fragment，THINK（"我们需要"/"回答用户说..."）须丢弃。
	body := parser.body.String()
	require.Contains(t, body, "哈哈，我又好～")
	require.Contains(t, body, "需要我帮忙的吗？")
	require.NotContains(t, body, "我们需要", "THINK fragment content must be discarded from body")
	require.NotContains(t, body, "回答用户说", "THINK fragment content must be discarded from body")

	// ready 的 response_message_id 数字 ID。
	require.Equal(t, "4", parser.responseID)

	// accumulated_token_usage 最终值（BATCH 42→112，累计语义）。
	require.Equal(t, int64(112), parser.usage)

	// close 事件 + response/status=FINISHED 均触发终止；解析器 finished 置位。
	require.True(t, parser.finished, "stream must be marked finished by close/status FINISHED")

	// 无 p 省略形态：fixture 原生含首帧 {p,o,v} 后仅 {v} 续写——解析后正文非空即证明状态机生效。
	require.Greater(t, parser.body.Len(), 0, "omit-form frames must have been applied to the current path")
}

// webDeepseekSolvablePowChallengeBody 返回 nonce=5 可解的 challenge 响应（base=salt123_1739764288699_5）。
func webDeepseekSolvablePowChallengeBody() string {
	digest := webDeepseekPowStateDigest([]byte("salt123_1739764288699_5"))
	challengeHex := hex.EncodeToString(digest[:])
	return `{"code":0,"msg":"","data":{"biz_code":0,"biz_msg":"","biz_data":{"challenge":{"algorithm":"DeepSeekHashV1","challenge":"` + challengeHex + `","salt":"salt123","signature":"sig","difficulty":100,"expire_at":1739764288699,"target_path":"/api/v0/chat/completion"}}}}`
}

// webDeepseekSolvablePowChallengeResponse 为 web_protocol_bridge_test.go 的 mock 序列提供
// 可解 PoW 挑战响应（无需 *testing.T 的包装）。
func webDeepseekSolvablePowChallengeResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(webDeepseekSolvablePowChallengeBody())),
	}
}

func webDeepseekSessionCreateResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"code":0,"msg":"","data":{"biz_code":0,"biz_msg":"","biz_data":{"chat_session":{"id":"sess-new-123"}}}}`)),
	}
}

func webDeepseekFixtureSSEResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(webDeepseekFixtureSSE)),
	}
}

// runForwardWebDeepseek 驱动一次 forwardWebDeepseek（非流式入站），返回 gin recorder、结果及上游 recorder。
func runForwardWebDeepseek(
	t *testing.T,
	account *Account,
	body []byte,
	model string,
	upstream *httpUpstreamRecorder,
	rlSvc *RateLimitService,
) (*httptest.ResponseRecorder, *OpenAIForwardResult, error) {
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

	result, err := svc.forwardWebDeepseek(context.Background(), c, account, body, model, false, time.Now(), webResponseModeChat)
	return recorder, result, err
}

func webDeepseekInboundBody(model string) []byte {
	return []byte(`{"model":"` + model + `","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
}

// TestForwardWebDeepseek_RequestBuild_NewProtocol 覆盖请求体重写（09 §4 / 06 §A）：
//   - model_type 字段名（非 model_class）、parent_message_id 恒 null、action null、source 缺省省略；
//   - search_enabled 实测默认 true、ref_file_ids 空数组；
//   - 自动建会话：无 credentials chat_session_id 覆盖时每轮新建，completion 体 chat_session_id 取新建 id。
func TestForwardWebDeepseek_RequestBuild_NewProtocol(t *testing.T) {
	account := webDeepseekTestAccount(8801, map[string]any{
		"cookie":   "ds_session_id=sess-abc; HWWAFSESID=waf-xyz; HWWAFSESTIME=1726450000",
		"base_url": "https://chat.deepseek.com",
	})
	// 出站点序：0=PoW 挑战、1=会话创建、2=对话完成。
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		&http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(webDeepseekSolvablePowChallengeBody()))},
		webDeepseekSessionCreateResponse(),
		webDeepseekFixtureSSEResponse(),
	}}

	_, _, err := runForwardWebDeepseek(t, account, webDeepseekInboundBody("deepseek-chat"), "deepseek-chat", upstream, &RateLimitService{})
	require.NoError(t, err)

	require.Len(t, upstream.requests, 3, "PoW challenge + session create + completion requests expected")

	// 端点顺序与头族。
	require.Equal(t, "/api/v0/chat/create_pow_challenge", upstream.requests[0].URL.Path)
	require.Equal(t, "/api/v0/chat_session/create", upstream.requests[1].URL.Path)
	require.Equal(t, "/api/v0/chat/completion", upstream.requests[2].URL.Path)

	chatReq := upstream.requests[2]
	require.Equal(t, "chat.deepseek.com", chatReq.URL.Host)
	require.Equal(t, webDeepseekClientUA, chatReq.Header.Get("User-Agent"))
	require.Equal(t, "com.deepseek.chat", chatReq.Header.Get("x-client-bundle-id"))
	require.Equal(t, "web", chatReq.Header.Get("x-client-platform"))
	require.Equal(t, "2.5.0", chatReq.Header.Get("x-client-version"))
	require.Equal(t, "zh_CN", chatReq.Header.Get("x-client-locale"))
	require.Equal(t, "28800", chatReq.Header.Get("x-client-timezone-offset"))
	require.NotEmpty(t, chatReq.Header.Get("x-device-id"), "x-device-id must be present (derived or credential)")
	require.Equal(t, "", chatReq.Header.Get("x-device-model"))
	require.NotEmpty(t, chatReq.Header.Get("X-Ds-PoW-Response"), "completion must carry x-ds-pow-response")

	// 请求体字段（09 §4 实测）。
	body := upstream.bodies[2]
	require.Equal(t, "default", gjson.GetBytes(body, "model_type").String(), "model_type field name (not model_class)")
	require.False(t, gjson.GetBytes(body, "model_type").Exists() == false)
	require.Nil(t, gjson.GetBytes(body, "model_class").Value(), "legacy model_class must be absent")
	require.Equal(t, "null", gjson.GetBytes(body, "parent_message_id").Raw, "parent_message_id must be JSON null")
	require.Equal(t, "null", gjson.GetBytes(body, "action").Raw, "action must be JSON null")
	require.False(t, gjson.GetBytes(body, "source").Exists(), "source default omitted")
	require.False(t, gjson.GetBytes(body, "thinking_enabled").Bool(), "deepseek-chat maps to thinking_enabled=false (09 §4: only deepseek-reasoner enables thinking)")
	require.True(t, gjson.GetBytes(body, "search_enabled").Bool(), "search_enabled default true (09 §4)")
	require.True(t, gjson.GetBytes(body, "ref_file_ids").IsArray())
	require.Equal(t, "hi", gjson.GetBytes(body, "prompt").String())
	require.Equal(t, "sess-new-123", gjson.GetBytes(body, "chat_session_id").String(), "chat_session_id from session create")

	// x-hif-*：无 credentials 时不带。
	require.Equal(t, "", chatReq.Header.Get("x-hif-dliq"))
	require.Equal(t, "", chatReq.Header.Get("x-hif-leim"))
}

// TestForwardWebDeepseek_SessionCreateOverrideSkipped 覆盖 credentials chat_session_id 覆盖时
// 跳过自动建会话（直接复用），出站仅 PoW + completion 两次请求。
func TestForwardWebDeepseek_SessionCreateOverrideSkipped(t *testing.T) {
	account := webDeepseekTestAccount(8802, map[string]any{
		"base_url":        "https://chat.deepseek.com",
		"chat_session_id": "sess-override-9",
	})
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		&http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(webDeepseekSolvablePowChallengeBody()))},
		webDeepseekFixtureSSEResponse(),
	}}
	_, _, err := runForwardWebDeepseek(t, account, webDeepseekInboundBody("deepseek-chat"), "deepseek-chat", upstream, &RateLimitService{})
	require.NoError(t, err)
	require.Len(t, upstream.requests, 2, "override skips session create: PoW + completion only")
	require.Equal(t, "/api/v0/chat/completion", upstream.requests[1].URL.Path)
	require.Equal(t, "sess-override-9", gjson.GetBytes(upstream.bodies[1], "chat_session_id").String())
}

// TestForwardWebDeepseek_HeaderFamily_HifPassthrough 覆盖 x-hif-* 在 credentials 提供时透传。
func TestForwardWebDeepseek_HeaderFamily_HifPassthrough(t *testing.T) {
	account := webDeepseekTestAccount(8803, map[string]any{
		"base_url":        "https://chat.deepseek.com",
		"login_device_id": "11111111-1111-4111-8111-111111111111",
		"x-hif-dliq":      "hif-dliq-value",
		"x-hif-leim":      "hif-leim-value",
	})
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		&http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(webDeepseekSolvablePowChallengeBody()))},
		webDeepseekSessionCreateResponse(),
		webDeepseekFixtureSSEResponse(),
	}}
	_, _, err := runForwardWebDeepseek(t, account, webDeepseekInboundBody("deepseek-chat"), "deepseek-chat", upstream, &RateLimitService{})
	require.NoError(t, err)
	chatReq := upstream.requests[2]
	require.Equal(t, "11111111-1111-4111-8111-111111111111", chatReq.Header.Get("x-device-id"), "login_device_id credential override")
	require.Equal(t, "hif-dliq-value", chatReq.Header.Get("x-hif-dliq"))
	require.Equal(t, "hif-leim-value", chatReq.Header.Get("x-hif-leim"))
}

// TestForwardWebDeepseek_ModelTypeMapping 覆盖模型映射：deepseek-chat → "default"；
// deepseek-reasoner → "default" + thinking_enabled=true；未知模型透传同名。
func TestForwardWebDeepseek_ModelTypeMapping(t *testing.T) {
	cases := []struct {
		in        string
		modelType string
		thinking  bool
	}{
		{"deepseek-chat", "default", false},
		{"deepseek-reasoner", "default", true},
		{"custom-model", "custom-model", false},
	}
	for _, tc := range cases {
		account := webDeepseekTestAccount(8800+int64(len(tc.in)), map[string]any{"base_url": "https://chat.deepseek.com"})
		upstream := &httpUpstreamRecorder{responses: []*http.Response{
			&http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(webDeepseekSolvablePowChallengeBody()))},
			webDeepseekSessionCreateResponse(),
			webDeepseekFixtureSSEResponse(),
		}}
		_, _, err := runForwardWebDeepseek(t, account, webDeepseekInboundBody(tc.in), tc.in, upstream, &RateLimitService{})
		require.NoError(t, err, "model %s", tc.in)
		require.Equal(t, tc.modelType, gjson.GetBytes(upstream.bodies[2], "model_type").String(), "model %s model_type", tc.in)
		require.Equal(t, tc.thinking, gjson.GetBytes(upstream.bodies[2], "thinking_enabled").Bool(), "model %s thinking", tc.in)
	}
}

// TestForwardWebDeepseek_MultiTurnPromptConcat 覆盖多轮上下文拼接为单 prompt（09 §5）：
// 全部 user/assistant 文本拼接进 prompt（parent_message_id 不跨请求链式）。
func TestForwardWebDeepseek_MultiTurnPromptConcat(t *testing.T) {
	account := webDeepseekTestAccount(8804, map[string]any{"base_url": "https://chat.deepseek.com"})
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		&http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(webDeepseekSolvablePowChallengeBody()))},
		webDeepseekSessionCreateResponse(),
		webDeepseekFixtureSSEResponse(),
	}}
	inbound := []byte(`{"model":"deepseek-chat","stream":false,"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hello"},{"role":"assistant","content":"hi there"},{"role":"user","content":"bye"}]}`)
	_, _, err := runForwardWebDeepseek(t, account, inbound, "deepseek-chat", upstream, &RateLimitService{})
	require.NoError(t, err)
	prompt := gjson.GetBytes(upstream.bodies[2], "prompt").String()
	require.Contains(t, prompt, "hello")
	require.Contains(t, prompt, "hi there")
	require.Contains(t, prompt, "bye")
	require.Contains(t, prompt, "sys")
	require.Equal(t, "null", gjson.GetBytes(upstream.bodies[2], "parent_message_id").Raw)
}

// TestForwardWebDeepseek_StreamingAuthAfterContentDoesNotRelogin 覆盖流式回程：
// HTTP 200 SSE 已写出 partial 正文帧后跟 auth 错误帧 → contentProduced 顶层闸门生效，
// 不触发重登/重试（requests 恰 3 个），流内收口（upstream_error），无 panic。
func TestForwardWebDeepseek_StreamingAuthAfterContentDoesNotRelogin(t *testing.T) {
	account := webDeepseekTestAccount(8899, map[string]any{"base_url": "https://chat.deepseek.com"})
	authBody := `{"code":0,"data":{"biz_code":40002,"biz_msg":"invalid token"}}`
	stream := "data: {\"p\":\"response/fragments\",\"o\":\"APPEND\",\"v\":[{\"type\":\"RESPONSE\",\"content\":\"partial\"}]}\n\n" + "data: " + authBody + "\n\n"
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		&http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(webDeepseekSolvablePowChallengeBody()))},
		webDeepseekSessionCreateResponse(),
		&http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))},
	}}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(webDeepseekInboundBody("deepseek-chat")))
	svc := &OpenAIGatewayService{
		cfg:              &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true}}},
		httpUpstream:     upstream,
		rateLimitService: &RateLimitService{},
	}
	_, err := svc.forwardWebDeepseek(context.Background(), c, account, webDeepseekInboundBody("deepseek-chat"), "deepseek-chat", true, time.Now(), webResponseModeChat)
	require.NoError(t, err)
	require.Len(t, upstream.requests, 3, "midstream auth failure must not trigger relogin/retry")
	require.Contains(t, recorder.Body.String(), "partial")
	require.Contains(t, recorder.Body.String(), "upstream_error")
}

// TestForwardWebDeepseek_NonStreamingAuthAfterContentDoesNotRelogin 覆盖非流式回程回归：
// HTTP 200 SSE 含 partial 内容帧后跟 auth 错误帧、reqStream=false → contentProduced 闸门
// 生效，不触发重登（requests 恰 3 个）、返回 auth 错误，且 recorder 无凭证泄漏。
func TestForwardWebDeepseek_NonStreamingAuthAfterContentDoesNotRelogin(t *testing.T) {
	const secretCookie = "ds_session_id=SECRETVALUE123456; HWWAFSESID=WAFSECRETVALUE1"
	account := webDeepseekTestAccount(8821, map[string]any{
		"cookie":   secretCookie,
		"base_url": "https://chat.deepseek.com",
	})
	authBody := `{"code":0,"data":{"biz_code":40002,"biz_msg":"invalid token"}}`
	stream := "data: {\"p\":\"response/fragments\",\"o\":\"APPEND\",\"v\":[{\"type\":\"RESPONSE\",\"content\":\"partial\"}]}\n\n" + "data: " + authBody + "\n\n"
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		&http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(webDeepseekSolvablePowChallengeBody()))},
		webDeepseekSessionCreateResponse(),
		&http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))},
	}}
	recorder, _, err := runForwardWebDeepseek(t, account, webDeepseekInboundBody("deepseek-chat"), "deepseek-chat", upstream, &RateLimitService{})
	require.Error(t, err)
	require.Len(t, upstream.requests, 3, "auth failure after aggregated content must not trigger relogin/retry")
	require.NotContains(t, recorder.Body.String(), "SECRETVALUE123456", "credentials must not leak into client response")
	require.NotContains(t, recorder.Body.String(), "WAFSECRETVALUE1", "credentials must not leak into client response")
}

func TestForwardWebDeepseek_FixtureStreamingRelay(t *testing.T) {
	account := webDeepseekTestAccount(8810, map[string]any{"base_url": "https://chat.deepseek.com"})
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		&http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(webDeepseekSolvablePowChallengeBody()))},
		webDeepseekSessionCreateResponse(),
		webDeepseekFixtureSSEResponse(),
	}}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(webDeepseekInboundBody("deepseek-chat")))
	svc := &OpenAIGatewayService{
		cfg:              &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true}}},
		httpUpstream:     upstream,
		rateLimitService: &RateLimitService{},
	}
	_, err := svc.forwardWebDeepseek(context.Background(), c, account, webDeepseekInboundBody("deepseek-chat"), "deepseek-chat", true, time.Now(), webResponseModeChat)
	require.NoError(t, err)

	out := recorder.Body.String()
	require.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
	// 正文分段为多个 chunk（chunk 间有 JSON 帧），需抽取各 delta.content 拼接后再断言，
	// 不能直接对原始 SSE 串做子串匹配（与解析器 body 累计口径一致）。
	var concat strings.Builder
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			continue
		}
		concat.WriteString(gjson.Get(payload, "choices.0.delta.content").String())
	}
	body := concat.String()
	require.Contains(t, body, "哈哈，我又好～", "body fragment must be relayed")
	require.Contains(t, body, "需要我帮忙的吗？", "body fragment must be relayed")
	require.NotContains(t, body, "我们需要", "THINK content must not appear in stream")
	require.Contains(t, out, `"output_tokens":112`, "final usage from accumulated_token_usage")
	require.True(t, strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]"), "stream must terminate with [DONE], got: %s", out)
}

// TestForwardWebDeepseek_MissingCookieFailsClosed Cookie 缺失必须失败关闭，且不发出任何上游请求；
// 错误信息不得包含任何凭证值。
func TestForwardWebDeepseek_MissingCookieFailsClosed(t *testing.T) {
	account := webDeepseekTestAccount(8805, map[string]any{"cookie": ""})
	upstream := &httpUpstreamRecorder{}

	_, _, err := runForwardWebDeepseek(t, account, webDeepseekInboundBody("deepseek-chat"), "deepseek-chat", upstream, &RateLimitService{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "cookie")
	require.Len(t, upstream.requests, 0, "no upstream request may be made without a login cookie")
}

// TestForwardWebDeepseek_PoWRequiredFailClosed 覆盖 PoW 强制（09 §3）：挑战不可达/无可用 challenge
// 时失败关闭，不再"无 PoW 继续出站"。用 401 不可解响应模拟挑战失败。
func TestForwardWebDeepseek_PoWRequiredFailClosed(t *testing.T) {
	account := webDeepseekTestAccount(8806, map[string]any{"base_url": "https://chat.deepseek.com"})
	// 挑战端点返回 401（无可用 challenge）→ 失败关闭，不进入会话创建/对话。
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		&http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"code":401,"msg":"unauthorized"}`))},
	}}
	_, _, err := runForwardWebDeepseek(t, account, webDeepseekInboundBody("deepseek-chat"), "deepseek-chat", upstream, &RateLimitService{})
	require.Error(t, err, "PoW failure must fail closed")
	require.Len(t, upstream.requests, 1, "must not proceed past PoW challenge")
}

// TestForwardWebDeepseek_PoWSolvedCarriesHeader 上游返回可解 challenge 时，求解成功且对话请求
// 携带 x-ds-pow-response 头（answer 为数值、challenge 回显）。
func TestForwardWebDeepseek_PoWSolvedCarriesHeader(t *testing.T) {
	account := webDeepseekTestAccount(8807, map[string]any{"base_url": "https://chat.deepseek.com"})
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		&http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(webDeepseekSolvablePowChallengeBody()))},
		webDeepseekSessionCreateResponse(),
		webDeepseekFixtureSSEResponse(),
	}}
	recorder, _, err := runForwardWebDeepseek(t, account, webDeepseekInboundBody("deepseek-chat"), "deepseek-chat", upstream, &RateLimitService{})
	require.NoError(t, err)
	require.True(t, recorder.Body.Len() > 0)
	powHeader := upstream.requests[2].Header.Get("X-Ds-PoW-Response")
	require.NotEmpty(t, powHeader, "completion request must carry x-ds-pow-response header")
	payload, decodeErr := base64.StdEncoding.DecodeString(powHeader)
	require.NoError(t, decodeErr)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(payload, &parsed))
	answer, ok := parsed["answer"].(float64)
	require.True(t, ok, "answer must be a number, got: %v", parsed["answer"])
	require.Equal(t, float64(5), answer)
}

// TestForwardWebDeepseek_PoWChallengeTargetPath 覆盖 PoW 挑战请求体为 {"target_path":"/api/v0/chat/completion"}（09 §1）。
func TestForwardWebDeepseek_PoWChallengeTargetPath(t *testing.T) {
	account := webDeepseekTestAccount(8808, map[string]any{"base_url": "https://chat.deepseek.com"})
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		&http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(webDeepseekSolvablePowChallengeBody()))},
		webDeepseekSessionCreateResponse(),
		webDeepseekFixtureSSEResponse(),
	}}
	_, _, err := runForwardWebDeepseek(t, account, webDeepseekInboundBody("deepseek-chat"), "deepseek-chat", upstream, &RateLimitService{})
	require.NoError(t, err)
	require.Equal(t, `/api/v0/chat/completion`, gjson.GetBytes(upstream.bodies[0], "target_path").String())
}

// webDeepseekRateLimitRepoStub 记录 SetRateLimited / SetError，用于断言冷却/账号处置接入。
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

func newWebDeepseekTestRateLimitService(repo *webDeepseekRateLimitRepoStub) *RateLimitService {
	return NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
}

// TestForwardWebDeepseek_ErrorClassification 覆盖双路径错误判定与分类接入
// （06 §A 错误映射）：顶层 code 与 data.biz_code 双路径；40002/40003/50006 → 认证处置(SetError)；
// 40029/429 → 限流冷却(SetRateLimited)；40300/40301 → PoW 错误（失败关闭，归一 403）。
func TestForwardWebDeepseek_ErrorClassification(t *testing.T) {
	t.Run("unit_classify_double_path", func(t *testing.T) {
		// 双路径：data.biz_code 非 0（嵌套形态）。
		body := `{"code":0,"msg":"","data":{"biz_code":40002,"biz_msg":"invalid token","biz_data":null}}`
		require.Equal(t, webDeepseekErrKindAuthFailed, classifyWebDeepseekUpstreamError(200, []byte(body)))
		// 顶层 code 非 0（旧形态）。
		require.Equal(t, webDeepseekErrKindAuthFailed, classifyWebDeepseekUpstreamError(200, []byte(`{"code":40002,"msg":"Missing Token"}`)))
		// 40029 IP 受限 → 限流。
		require.Equal(t, webDeepseekErrKindRateLimited, classifyWebDeepseekUpstreamError(200, []byte(`{"code":0,"data":{"biz_code":40029}}`)))
		// 40300 PoW 错误。
		require.Equal(t, webDeepseekErrKindPoWError, classifyWebDeepseekUpstreamError(200, []byte(`{"code":0,"data":{"biz_code":40300}}`)))
		// 50006 禁言 → 认证处置。
		require.Equal(t, webDeepseekErrKindAuthFailed, classifyWebDeepseekUpstreamError(200, []byte(`{"code":0,"data":{"biz_code":50006}}`)))
		// 429 状态 → 限流。
		require.Equal(t, webDeepseekErrKindRateLimited, classifyWebDeepseekUpstreamError(429, nil))
		// 无错误。
		require.Equal(t, webDeepseekErrKindOther, classifyWebDeepseekUpstreamError(200, []byte(`{"code":0}`)))
	})

	t.Run("auth_40002_triggers_set_error", func(t *testing.T) {
		repo := &webDeepseekRateLimitRepoStub{}
		rlSvc := newWebDeepseekTestRateLimitService(repo)
		account := webDeepseekTestAccount(8811, map[string]any{"base_url": "https://chat.deepseek.com"})
		errBody := `{"code":0,"msg":"","data":{"biz_code":40002,"biz_msg":"invalid token","biz_data":null}}`
		upstream := &httpUpstreamRecorder{responses: []*http.Response{
			&http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(webDeepseekSolvablePowChallengeBody()))},
			webDeepseekSessionCreateResponse(),
			&http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(errBody))},
		}}
		_, _, err := runForwardWebDeepseek(t, account, webDeepseekInboundBody("deepseek-chat"), "deepseek-chat", upstream, rlSvc)
		require.Error(t, err)
		require.Equal(t, 1, repo.setErrorCalls, "40002 must disable account (SetError)")
	})

	t.Run("ip_restricted_40029_triggers_cooldown", func(t *testing.T) {
		repo := &webDeepseekRateLimitRepoStub{}
		rlSvc := newWebDeepseekTestRateLimitService(repo)
		account := webDeepseekTestAccount(8812, map[string]any{"base_url": "https://chat.deepseek.com"})
		errBody := `{"code":0,"msg":"","data":{"biz_code":40029,"biz_msg":"ip restricted","biz_data":null}}`
		upstream := &httpUpstreamRecorder{responses: []*http.Response{
			&http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(webDeepseekSolvablePowChallengeBody()))},
			webDeepseekSessionCreateResponse(),
			&http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(errBody))},
		}}
		_, _, err := runForwardWebDeepseek(t, account, webDeepseekInboundBody("deepseek-chat"), "deepseek-chat", upstream, rlSvc)
		require.Error(t, err)
		require.Equal(t, 1, repo.setRateLimitedCalls, "40029 must cool the account (SetRateLimited)")
	})

	t.Run("pow_error_40300_fail_closed", func(t *testing.T) {
		repo := &webDeepseekRateLimitRepoStub{}
		rlSvc := newWebDeepseekTestRateLimitService(repo)
		account := webDeepseekTestAccount(8813, map[string]any{"base_url": "https://chat.deepseek.com"})
		errBody := `{"code":0,"msg":"","data":{"biz_code":40300,"biz_msg":"pow header error","biz_data":null}}`
		upstream := &httpUpstreamRecorder{responses: []*http.Response{
			&http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(webDeepseekSolvablePowChallengeBody()))},
			webDeepseekSessionCreateResponse(),
			&http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(errBody))},
		}}
		_, _, err := runForwardWebDeepseek(t, account, webDeepseekInboundBody("deepseek-chat"), "deepseek-chat", upstream, rlSvc)
		require.Error(t, err)
		require.Equal(t, 0, repo.setRateLimitedCalls)
		require.Equal(t, 0, repo.setErrorCalls)
	})
}

// TestForwardWebDeepseek_WAFFailClosed 覆盖 WAF 失败关闭（01 §8 / 09）：响应头含 x-amzn-waf-action
// 或状态 405 → 失败关闭，不将 WAF 正文透传客户端。
func TestForwardWebDeepseek_WAFFailClosed(t *testing.T) {
	t.Run("x_amzn_waf_action", func(t *testing.T) {
		account := webDeepseekTestAccount(8814, map[string]any{"base_url": "https://chat.deepseek.com"})
		upstream := &httpUpstreamRecorder{responses: []*http.Response{
			&http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(webDeepseekSolvablePowChallengeBody()))},
			webDeepseekSessionCreateResponse(),
			&http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Amzn-Waf-Action": []string{"challenge"}, "Content-Type": []string{"text/html"}}, Body: io.NopCloser(strings.NewReader(`<html>waf</html>`))},
		}}
		_, _, err := runForwardWebDeepseek(t, account, webDeepseekInboundBody("deepseek-chat"), "deepseek-chat", upstream, &RateLimitService{})
		require.Error(t, err)
		require.Contains(t, err.Error(), "WAF")
	})
	t.Run("status_405", func(t *testing.T) {
		account := webDeepseekTestAccount(8815, map[string]any{"base_url": "https://chat.deepseek.com"})
		upstream := &httpUpstreamRecorder{responses: []*http.Response{
			&http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(webDeepseekSolvablePowChallengeBody()))},
			webDeepseekSessionCreateResponse(),
			&http.Response{StatusCode: http.StatusMethodNotAllowed, Header: http.Header{"x-amzn-waf-action": []string{"captcha"}}, Body: io.NopCloser(strings.NewReader(`captcha`))},
		}}
		_, _, err := runForwardWebDeepseek(t, account, webDeepseekInboundBody("deepseek-chat"), "deepseek-chat", upstream, &RateLimitService{})
		require.Error(t, err)
		require.Contains(t, err.Error(), "WAF")
	})
}

// TestForwardWebDeepseek_CredentialNeverInClientResponse 凭证不出现在任何客户端可见错误响应中
// （含缺 Cookie 与上游错误两条路径）。
func TestForwardWebDeepseek_CredentialNeverInClientResponse(t *testing.T) {
	const secretCookie = "ds_session_id=SECRETVALUE123456; HWWAFSESID=WAFSECRETVALUE1"
	account := webDeepseekTestAccount(8809, map[string]any{
		"cookie":   secretCookie,
		"base_url": "https://chat.deepseek.com",
	})
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		&http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(webDeepseekSolvablePowChallengeBody()))},
		webDeepseekSessionCreateResponse(),
		// 上游错误体回显 Cookie 片段（最坏情况），必须被脱敏后再透传。
		&http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"code":401,"msg":"bad session ` + secretCookie + `"}`))},
	}}
	recorder, _, err := runForwardWebDeepseek(t, account, webDeepseekInboundBody("deepseek-chat"), "deepseek-chat", upstream, newWebDeepseekTestRateLimitService(&webDeepseekRateLimitRepoStub{}))
	require.Error(t, err)
	require.NotContains(t, recorder.Body.String(), "SECRETVALUE123456")
	require.NotContains(t, recorder.Body.String(), "WAFSECRETVALUE1")
}

// TestForwardWebDeepseek_NonStreamingFixtureAggregate 覆盖非流式聚合（同步改用新解析器）：
// 实测 fixture 流聚合为单条 chat.completion JSON，正文正确、usage=112、模型回填。
func TestForwardWebDeepseek_NonStreamingFixtureAggregate(t *testing.T) {
	account := webDeepseekTestAccount(8820, map[string]any{"base_url": "https://chat.deepseek.com"})
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		&http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(webDeepseekSolvablePowChallengeBody()))},
		webDeepseekSessionCreateResponse(),
		webDeepseekFixtureSSEResponse(),
	}}
	recorder, _, err := runForwardWebDeepseek(t, account, webDeepseekInboundBody("deepseek-chat"), "deepseek-chat", upstream, &RateLimitService{})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, recorder.Code)
	var completion map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &completion))
	require.Equal(t, "chat.completion", completion["object"])
	require.Equal(t, "deepseek-chat", completion["model"])
	choices := completion["choices"].([]any)
	first := choices[0].(map[string]any)
	message := first["message"].(map[string]any)
	content := message["content"].(string)
	require.Contains(t, content, "哈哈，我又好～")
	require.Contains(t, content, "需要我帮忙的吗？")
	require.NotContains(t, content, "我们需要")
	usage := completion["usage"].(map[string]any)
	require.EqualValues(t, 0, usage["input_tokens"])
	require.EqualValues(t, 112, usage["output_tokens"])
}
