package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// webKimiTestAccount 构造 kimi 网页转发测试账号：access_token 落 credentials，
// base_url 覆盖默认域名便于断言出站目标。
func webKimiTestAccount(id int64, credentials map[string]any) *Account {
	if credentials == nil {
		credentials = map[string]any{}
	}
	credentials["access_mode"] = AccountAccessModeWeb
	acc := &Account{
		ID:          id,
		Name:        "kimi-web-test",
		Platform:    PlatformKimi,
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

// webKimiFixtureEnvelopeFrames 是回放/单测用的 Connect envelope 载荷集合（登录态实测 10 §4
// 结构的精简等价）：chat.id 首帧、assistant message 生成帧、block.text.content 正文增量帧、
// message.status 完成帧、done 终止帧。
var webKimiFixtureEnvelopeFrames = []string{
	`{"op":"set","eventOffset":1,"chat":{"id":"chat-1","name":"未命名会话"}}`,
	`{"op":"set","mask":"message","eventOffset":5,"message":{"id":"asst-1","parentId":"u1","role":"assistant","status":"MESSAGE_STATUS_GENERATING"}}`,
	`{"op":"append","mask":"block.text.content","eventOffset":10,"block":{"id":"4","parentId":"","text":{"content":"hi "}}}`,
	`{"op":"append","mask":"block.text.content","eventOffset":11,"block":{"id":"4","parentId":"","text":{"content":"there"}}}`,
	`{"op":"set","mask":"message.status","eventOffset":12,"message":{"id":"asst-1","status":"MESSAGE_STATUS_COMPLETED"}}`,
	`{"eventOffset":13,"done":{}}`,
}

func webKimiEnvelopeFrameWithFlag(flag byte, jsonStr string) []byte {
	b := []byte(jsonStr)
	buf := make([]byte, 5+len(b))
	buf[0] = flag
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(b)))
	copy(buf[5:], b)
	return buf
}

// wrapWebKimiEnvelope 把一帧 JSON 载荷包成 Connect RPC envelope（1 字节 flag=0x00 + 4 字节
// 大端长度 + payload），与上游实测线格式一致。
func wrapWebKimiEnvelope(jsonStr string) []byte {
	return webKimiEnvelopeFrameWithFlag(0x00, jsonStr)
}

// webKimiTrailerResponse 构造只回 Connect trailer 错误帧的 200 响应（flag=0x02 是 trailer
// 语义；readWebKimiConnectEnvelope 只把 flag=0x01 当 gzip，其余读 payload），即生产上
// 免费档请求 k3 被订阅墙拒绝的实测形态。
func webKimiTrailerResponse(payloads ...string) *http.Response {
	var body []byte
	for _, p := range payloads {
		body = append(body, webKimiEnvelopeFrameWithFlag(0x02, p)...)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/connect+json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

// unwrapWebKimiEnvelope 剥掉出站请求体的 Connect envelope 头（5 字节），返回 payload JSON。
// 必要性：请求体帧化修复后（buildWebKimiRequestBody → encodeWebKimiConnectEnvelope），
// 直接 gjson 解原始 body 会全部失配。剥壳过程同时对帧头做回归断言（flag=0x00 +
// 大端长度 == 剩余字节数），即请求体帧化的回归锚点。
func unwrapWebKimiEnvelope(t *testing.T, frame []byte) []byte {
	t.Helper()
	require.GreaterOrEqual(t, len(frame), 5, "outbound kimi body must carry a 5-byte Connect envelope header")
	require.Equal(t, byte(0x00), frame[0], "envelope flag must be 0x00 (uncompressed)")
	declared := int(binary.BigEndian.Uint32(frame[1:5]))
	require.Equal(t, declared, len(frame)-5, "envelope length prefix must match the payload size")
	return frame[5:]
}

// webKimiEnvelopeResponse 构造 200 + Connect RPC envelope 流响应（登录态实测 10 §4 线格式）。
func webKimiEnvelopeResponse(payloads []string) *http.Response {
	var body []byte
	for _, p := range payloads {
		body = append(body, wrapWebKimiEnvelope(p)...)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/connect+json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

// webKimiConnectResponse 构造非流式 Connect RPC envelope 响应（登录态实测回放用）：聚合后应
// 得到正文 "hi there"、响应 id asst-1、chat.id chat-1。
func webKimiConnectResponse() *http.Response {
	return webKimiEnvelopeResponse(webKimiFixtureEnvelopeFrames)
}

// webKimiStreamResponse 构造流式 Connect RPC envelope 响应（与 webKimiConnectResponse 同载荷）。
func webKimiStreamResponse() *http.Response {
	return webKimiEnvelopeResponse(webKimiFixtureEnvelopeFrames)
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

	_, err := svc.forwardWebKimi(context.Background(), c, account, body, originalModel, false, time.Now(), webResponseModeChat)
	return recorder, err
}

func webKimiInboundBody(model string) []byte {
	return []byte(`{"model":"` + model + `","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
}

// TestForwardWebKimi_RequestBuildAndNonStreamAggregate 覆盖：
//   - 出站端点 /apiv2/kimi.chat.v1.ChatService/Chat、Connect RPC 头（Bearer 认证载体 /
//     Origin / Referer / UA / x-language / x-msh-platform / x-msh-version）；
//   - 请求体已验证结构（blocks[].text.content 简单形态、options.model=官方值、
//     scenario=SCENARIO_CHAT、thinking/enable_plugin/reasoning_effort 实测默认、**不带 chatId**）；
//   - base_url 凭证覆盖默认域名；
//   - 非流式 Connect 响应聚合为单条 chat.completion JSON，模型名回填原始请求模型。
func TestForwardWebKimi_RequestBuildAndNonStreamAggregate(t *testing.T) {
	account := webKimiTestAccount(8901, map[string]any{
		"access_token": "kimi-access-token-abc123",
		"base_url":     "https://www.kimi.com",
	})
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		webKimiConnectResponse(),
	}}

	recorder, err := runForwardWebKimi(t, account, webKimiInboundBody("kimi-k3"), "kimi-k3", upstream, &RateLimitService{})
	require.NoError(t, err)

	require.Len(t, upstream.requests, 1)
	chatReq := upstream.requests[0]
	require.Equal(t, "/apiv2/kimi.chat.v1.ChatService/Chat", chatReq.URL.Path)
	require.Equal(t, "www.kimi.com", chatReq.URL.Host, "credentials.base_url must override default base url")
	require.Equal(t, "Bearer kimi-access-token-abc123", chatReq.Header.Get("Authorization"))
	require.Equal(t, "application/connect+json", chatReq.Header.Get("Content-Type"))
	require.Equal(t, "https://www.kimi.com", chatReq.Header.Get("Origin"))
	require.Equal(t, webKimiClientUA, chatReq.Header.Get("User-Agent"))
	// 头族实测（10 §2）：x-language / x-msh-platform / x-msh-version 常量。
	require.Equal(t, "zh-CN", chatReq.Header.Get("x-language"))
	require.Equal(t, "web", chatReq.Header.Get("x-msh-platform"))
	require.Equal(t, "2.2.0", chatReq.Header.Get("x-msh-version"))

	// 请求体：登录态实测结构（10 §3）。出站体已 Connect 帧化，断言前先剥掉 5 字节帧头。
	body := unwrapWebKimiEnvelope(t, upstream.bodies[0])
	require.Equal(t, "k3", gjson.GetBytes(body, "options.model").String())
	require.True(t, gjson.GetBytes(body, "options.thinking").Bool())
	require.True(t, gjson.GetBytes(body, "options.enable_plugin").Bool())
	require.Equal(t, "REASONING_EFFORT_LOW", gjson.GetBytes(body, "options.reasoning_effort").String())
	require.Equal(t, "SCENARIO_CHAT", gjson.GetBytes(body, "scenario").String())
	require.Equal(t, "user", gjson.GetBytes(body, "message.role").String())
	require.Equal(t, "hi", gjson.GetBytes(body, "message.blocks.0.text.content").String())
	require.Equal(t, "", gjson.GetBytes(body, "message.blocks.0.message_id").String())
	require.Equal(t, "", gjson.GetBytes(body, "project_id").String())
	require.Len(t, gjson.GetBytes(body, "tools").Array(), 2)
	// 登录态实测：请求体不带 chatId / kimiPlusId（chatId 服务端生成）。
	require.False(t, gjson.GetBytes(body, "chatId").Exists(), "request must not carry chatId")
	require.False(t, gjson.GetBytes(body, "kimiPlusId").Exists(), "request must not carry kimiPlusId")
	require.Equal(t, http.MethodPost, upstream.requests[0].Method)

	// 回程聚合：Connect envelope → 单条 chat.completion，模型回填原始请求模型。
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
	require.Equal(t, "asst-1", completion["id"])
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
		payload := unwrapWebKimiEnvelope(t, upstream.bodies[0])
		require.Equal(t, tc.expected, gjson.GetBytes(payload, "options.model").String())
	}
}

// TestForwardWebKimi_RequestBodyIsFramedOnce 请求体帧化的回归锚点（分析文档 §1.4）：
// Connect RPC 流式 RPC 的请求体必须打 envelope 帧（flag=0x00 + 4 字节大端长度），否则上游
// 一律回 HTTP 200 + trailer {"error":{"code":"invalid_argument"}}。
// 帧化点必须是 buildWebKimiRequestBody（而非 buildWebKimiUpstreamRequest）：后者会被
// 401→refresh→重试链路复用同一份 body，在那里帧化会导致重试请求出现双帧。
func TestForwardWebKimi_RequestBodyIsFramedOnce(t *testing.T) {
	t.Run("single_envelope_frame", func(t *testing.T) {
		account := webKimiTestAccount(8941, nil)
		upstream := &httpUpstreamRecorder{responses: []*http.Response{webKimiConnectResponse()}}
		_, err := runForwardWebKimi(t, account, webKimiInboundBody("kimi-k2d6-chat"), "kimi-k2d6-chat", upstream, &RateLimitService{})
		require.NoError(t, err)
		require.Len(t, upstream.bodies, 1)

		frame := upstream.bodies[0]
		require.GreaterOrEqual(t, len(frame), 5, "outbound body must carry the envelope header")
		require.Equal(t, byte(0x00), frame[0], "flag byte must be 0x00 (uncompressed)")
		require.Equal(t, uint32(len(frame)-5), binary.BigEndian.Uint32(frame[1:5]),
			"the 4-byte big-endian length prefix must equal the trailing payload size")
		require.True(t, gjson.ValidBytes(frame[5:]), "payload after the header must be valid JSON")
	})

	t.Run("401_retry_does_not_double_frame", func(t *testing.T) {
		account := webKimiTestAccount(8942, map[string]any{"refresh_token": "refresh-token-abc12345"})
		upstream := &httpUpstreamRecorder{responses: []*http.Response{
			webKimiUnauthenticatedResponse(),
			{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"accessToken":"new-token-xyz98765"}`)),
			},
			webKimiConnectResponse(),
		}}
		_, err := runForwardWebKimi(t, account, webKimiInboundBody("kimi-k3"), "kimi-k3", upstream, &RateLimitService{})
		require.NoError(t, err)
		require.Len(t, upstream.bodies, 3)
		require.Equal(t, upstream.bodies[0], upstream.bodies[2], "retry must resend the same framed body, not re-framed")
		// 逐字节校验重试帧仍是单帧（无第二层帧头）。
		retry := upstream.bodies[2]
		require.Equal(t, uint32(len(retry)-5), binary.BigEndian.Uint32(retry[1:5]))
		require.True(t, gjson.ValidBytes(retry[5:]))
	})
}

// TestForwardWebKimi_TrailerOnlyErrorFailsClosed 上游只回 trailer 错误帧（HTTP 200 +
// flag=2 + {"error":{"code":"invalid_argument"}}，生产免费档请求付费模型的实测形态）：
// 流式不得伪成功（不得写正常 [DONE]），非流式不得返回空正文。
func TestForwardWebKimi_TrailerOnlyErrorFailsClosed(t *testing.T) {
	const trailer = `{"error":{"code":"invalid_argument","message":"subscription required","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo"}]}}`

	t.Run("non_streaming", func(t *testing.T) {
		account := webKimiTestAccount(8943, nil)
		upstream := &httpUpstreamRecorder{responses: []*http.Response{webKimiTrailerResponse(trailer)}}
		recorder, err := runForwardWebKimi(t, account, webKimiInboundBody("kimi-k3"), "kimi-k3", upstream, &RateLimitService{})
		require.Error(t, err, "trailer-only error must fail closed, not return an empty completion")
		require.NotContains(t, recorder.Body.String(), `"object":"chat.completion"`, "must not emit a completion object")
	})

	t.Run("streaming", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(webKimiInboundBody("kimi-k3")))
		svc := &OpenAIGatewayService{
			cfg: &config.Config{
				Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true}},
			},
		}
		_, err := svc.handleWebKimiStreamingResponse(context.Background(), webKimiTrailerResponse(trailer),
			c, webKimiTestAccount(8944, nil), "kimi-k3", "k3", time.Now(), webResponseModeChat)
		require.Error(t, err, "trailer-only error must not be reported as a successful stream")
		require.NotContains(t, recorder.Body.String(), "data: [DONE]", "must not emit a normal terminal frame")
	})
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

// TestForwardWebKimi_StreamingResponse 覆盖流式回程：Connect envelope 增量重包为
// chat.completion.chunk 流 + 终止 [DONE]。
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
		context.Background(), webKimiStreamResponse(), c, account, "kimi-k3", "k3", time.Now(), webResponseModeChat)
	require.NoError(t, err)
	require.True(t, result.Stream)

	out := recorder.Body.String()
	require.Contains(t, out, `"content":"hi "`)
	require.Contains(t, out, `"content":"there"`)
	require.Contains(t, out, "chat.completion.chunk")
	require.Contains(t, out, "data: [DONE]")
}

// TestForwardWebKimi_StreamMidstreamBusinessError 覆盖流式中段业务错误收口（Codex 审查
// #1）：首帧已写出正文后，上游在流中抛出业务错误（unauthenticated / 非 0 code）时，必须向
// 客户端写明确 error 标记并终结 SSE，且不得伪造正常 finish_reason+usage 终止帧。
func TestForwardWebKimi_StreamMidstreamBusinessError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(webKimiInboundBody("kimi-k3")))
	svc := &OpenAIGatewayService{
		cfg: &config.Config{
			Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true}},
		},
	}
	resp := webKimiEnvelopeResponse([]string{
		`{"op":"set","mask":"message","eventOffset":5,"message":{"id":"asst-1","role":"assistant","status":"MESSAGE_STATUS_GENERATING"}}`,
		`{"op":"append","mask":"block.text.content","eventOffset":10,"block":{"id":"4","parentId":"","text":{"content":"hello"}}}`,
		`{"code":"unauthenticated","message":"login expired midstream"}`,
	})
	account := webKimiTestAccount(8935, nil)
	result, err := svc.handleWebKimiStreamingResponse(
		context.Background(), resp, c, account, "kimi-k3", "k3", time.Now(), webResponseModeChat)
	require.NoError(t, err, "midstream error is conveyed in-stream, not as a Go error")
	require.True(t, result.Stream)

	out := recorder.Body.String()
	require.Contains(t, out, `"content":"hello"`, "first content frame must be relayed before the error")
	require.Contains(t, out, `"error"`, "midstream error must be marked in-stream")
	require.Contains(t, out, `"upstream_error"`)
	// 错误帧必须是 [DONE] 之前的最后一帧业务帧：中间没有任何正常 finish_reason+usage 终止帧。
	// 错误信息采用上游真实 message（经 sanitize，不回显凭证），而非写死文案。
	require.Contains(t, out,
		`data: {"error":{"message":"login expired midstream","type":"upstream_error"}}`+"\n\n"+`data: [DONE]`,
		"error frame must immediately precede [DONE], with no normal terminal frame in between")
	require.NotContains(t, out, `"usage"`, "midstream error must NOT emit a normal usage terminal frame")
	// 凭证脱敏：错误响应不得回显 access_token。
	require.NotContains(t, recorder.Body.String(), "kimi-access-token-abc123")
}

// TestForwardWebKimi_UnrecognizedShapeFailsClosed Connect envelope 单帧但结构不可识别
// （无 heartbeat/done/code/chat/message/block）：聚合为空，失败关闭，不伪造成功响应。
func TestForwardWebKimi_UnrecognizedShapeFailsClosed(t *testing.T) {
	account := webKimiTestAccount(8926, nil)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		webKimiEnvelopeResponse([]string{`{"weird":"shape"}`}),
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

// TestWebKimiBareJSONError 纯函数单测（#3）：裸 JSON（非 SSE data: 前缀）业务错误判定层。
//   - 字符串 code（如 {"code":"unauthenticated"}）须识别为业务错误，不再被忽略；
//   - 数字 code 非 0 仍识别为业务错误；数字 0 / 空字符串 code 不识别；
//   - 纯内容裸 JSON（无 code）仍忽略（isError=false）；data: 前缀行不进入此判定。
func TestWebKimiBareJSONError(t *testing.T) {
	cases := []struct {
		name     string
		line     string
		isError  bool
	}{
		{"string_code_unauthenticated", `{"code":"unauthenticated","message":"token expired"}`, true},
		{"string_code_empty_message", `{"code":"unauthenticated","message":""}`, true},
		{"numeric_code_nonzero", `{"code":401,"message":"auth failed"}`, true},
		{"numeric_code_zero", `{"code":0,"message":"ok"}`, false},
		{"string_code_empty", `{"code":"","message":"ok"}`, false},
		{"no_code_content", `{"content":"hello world"}`, false},
		{"bare_sse_frame_skipped", `data: {"code":"unauthenticated"}`, false},
		{"empty_line_skipped", ``, false},
		{"invalid_json_skipped", `not json`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, isErr := webKimiBareJSONError(tc.line)
			require.Equal(t, tc.isError, isErr, "line=%q", tc.line)
		})
	}
}

// TestParseWebKimiEnvelopePayload 解析器单测：覆盖登录态实测（10 §4）各帧型——
// 心跳跳过、done 终止、chat.id / assistant message.id 提取、block.text.content 正文增量、
// block.think.content 思考增量、mask "block.text" 的 set 帧（首段正文）、认证/业务错误、
// 以及 user role 消息正文必须排除（不得混入用户提问）。
func TestParseWebKimiEnvelopePayload(t *testing.T) {
	t.Run("heartbeat_skips_content", func(t *testing.T) {
		ev := parseWebKimiEnvelopePayload([]byte(`{"heartbeat":{}}`))
		require.True(t, ev.Heartbeat)
		require.Empty(t, ev.TextDelta)
		require.Empty(t, ev.ThinkDelta)
	})
	t.Run("done_termination", func(t *testing.T) {
		ev := parseWebKimiEnvelopePayload([]byte(`{"eventOffset":88,"done":{}}`))
		require.True(t, ev.Done)
	})
	t.Run("chat_id_first_frame", func(t *testing.T) {
		ev := parseWebKimiEnvelopePayload([]byte(`{"op":"set","eventOffset":1,"chat":{"id":"c1","name":"x"}}`))
		require.Equal(t, "c1", ev.ChatID)
	})
	t.Run("assistant_message_id", func(t *testing.T) {
		ev := parseWebKimiEnvelopePayload([]byte(`{"op":"set","mask":"message","eventOffset":5,"message":{"id":"a1","role":"assistant","status":"MESSAGE_STATUS_GENERATING"}}`))
		require.Equal(t, "a1", ev.AssistantID)
	})
	t.Run("text_delta_append", func(t *testing.T) {
		ev := parseWebKimiEnvelopePayload([]byte(`{"op":"append","mask":"block.text.content","eventOffset":10,"block":{"id":"4","text":{"content":"hi "}}}`))
		require.Equal(t, "hi ", ev.TextDelta)
	})
	t.Run("think_delta", func(t *testing.T) {
		ev := parseWebKimiEnvelopePayload([]byte(`{"op":"append","mask":"block.think.content","eventOffset":11,"block":{"id":"3","think":{"content":"thinking"}}}`))
		require.Equal(t, "thinking", ev.ThinkDelta)
	})
	t.Run("set_block_text_first_segment", func(t *testing.T) {
		// 实测首段正文以 op=set + mask "block.text"（而非 block.text.content）下发。
		ev := parseWebKimiEnvelopePayload([]byte(`{"op":"set","mask":"block.text","eventOffset":75,"block":{"id":"4","parentId":"","text":{"content":"你好"}}}`))
		require.Equal(t, "你好", ev.TextDelta)
	})
	t.Run("unauthenticated_error", func(t *testing.T) {
		ev := parseWebKimiEnvelopePayload([]byte(`{"code":"unauthenticated","message":"expired"}`))
		require.True(t, ev.AuthFailed)
	})
	t.Run("numeric_code_error", func(t *testing.T) {
		ev := parseWebKimiEnvelopePayload([]byte(`{"code":401,"message":"auth failed"}`))
		require.NotZero(t, ev.ErrCode)
	})
	t.Run("trailer_nested_string_code", func(t *testing.T) {
		// Connect trailer 实测形态：flag=2 + {"error":{"code":"invalid_argument",...}}。
		// 修复前顶层无 code/message → 全零值事件 → 不计有效帧 → 探活报「空流」。
		ev := parseWebKimiEnvelopePayload([]byte(`{"error":{"code":"invalid_argument","message":"subscription required","details":[{"@type":"x"}]}}`))
		require.Equal(t, "invalid_argument", ev.ErrCodeStr, "string business code must survive normalization")
		require.Zero(t, ev.ErrCode, "string codes must not collapse into ErrCode=0 silently")
		require.False(t, ev.AuthFailed)
	})
	t.Run("trailer_nested_unauthenticated", func(t *testing.T) {
		ev := parseWebKimiEnvelopePayload([]byte(`{"error":{"code":"unauthenticated","message":"token expired"}}`))
		require.True(t, ev.AuthFailed, "nested unauthenticated must set AuthFailed")
		require.Equal(t, "unauthenticated", ev.ErrCodeStr)
	})
	t.Run("trailer_nested_numeric_code", func(t *testing.T) {
		ev := parseWebKimiEnvelopePayload([]byte(`{"error":{"code":16,"message":"quota exceeded"}}`))
		require.Equal(t, int64(16), ev.ErrCode)
		require.Empty(t, ev.ErrCodeStr)
		require.False(t, ev.AuthFailed)
	})
	t.Run("user_role_text_excluded", func(t *testing.T) {
		// 用户消息帧（role=user）的正文不得被当作助手正文聚合。
		ev := parseWebKimiEnvelopePayload([]byte(`{"op":"set","mask":"message","eventOffset":4,"message":{"id":"u1","role":"user","blocks":[{"text":{"content":"你好"}}]}}`))
		require.Empty(t, ev.AssistantID)
		require.Empty(t, ev.TextDelta)
	})
}

// kimiExtractJSONObjects 从原始 Connect 流字节中恢复各帧 JSON 载荷。
//
// 实测 fixture（raw/kimi-chat-decoded.txt）的 envelope 长度前缀字节因 UTF-8 重编码损坏
// （>=0x80 的长度字节被替换为 U+FFFD 三字节序列），故不能依赖 1 字节 flag + 4 字节长度
// 去切帧。所有真实 payload 均为以 `{"` 开头的完整 JSON 对象，这里锚定 `{"` 后用
// 括号/字符串/转义感知的匹配器逐帧提取，与 parseWebKimiEnvelopePayload 解耦（解析器单测
// 已独立覆盖）。
func kimiExtractJSONObjects(data []byte) [][]byte {
	var objs [][]byte
	start := 0
	for start < len(data) {
		idx := bytes.Index(data[start:], []byte(`{"`))
		if idx < 0 {
			break
		}
		p := start + idx
		end, ok := matchJSONObject(data, p)
		if !ok {
			start = p + 1
			continue
		}
		objs = append(objs, data[p:end])
		start = end
	}
	return objs
}

// matchJSONObject 从 data[start]（须为 '{'）起，按深度/字符串/转义感知匹配到配对的 '}，
// 返回结束位置（不含）；非法结构返回 ok=false。
func matchJSONObject(data []byte, start int) (int, bool) {
	if start >= len(data) || data[start] != '{' {
		return 0, false
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(data); i++ {
		c := data[i]
		if inStr {
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1, true
			}
		case '"':
			inStr = true
		}
	}
	return 0, false
}

// TestForwardWebKimi_FixtureReplay 用登录态实测流（raw/kimi-chat-decoded.txt）做回放
// fixture：解析器须正确提取思考块、正文块、chat.id、assistant message.id、done 终止；
// heartbeat 帧不产出正文。聚合正文须等于实测内容
// 「你好！很高兴见到你。有什么我可以帮你的吗？」。fixture 不在仓库内，缺失时跳过。
func TestForwardWebKimi_FixtureReplay(t *testing.T) {
	const fixturePath = "/home/zjy/.sub2api-acceptance/sub2api-20260918-deepseek-kimi-evidence/raw/kimi-chat-decoded.txt"
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Skipf("kimi fixture unavailable (expected outside repo): %v", err)
	}
	objs := kimiExtractJSONObjects(raw)
	require.NotEmpty(t, objs, "fixture must yield at least one JSON frame")

	var (
		chatID      string
		assistantID string
		text        strings.Builder
		think       strings.Builder
		heartbeats  int
		done        bool
	)
	for _, o := range objs {
		ev := parseWebKimiEnvelopePayload(o)
		if ev.Heartbeat {
			heartbeats++
			continue
		}
		if ev.ChatID != "" {
			chatID = ev.ChatID
		}
		if ev.AssistantID != "" {
			assistantID = ev.AssistantID
		}
		if ev.Done {
			done = true
		}
		text.WriteString(ev.TextDelta)
		think.WriteString(ev.ThinkDelta)
	}

	require.Equal(t, "你好！很高兴见到你。有什么我可以帮你的吗？", text.String(),
		"replayed fixture text must match measured stream content")
	require.Equal(t, "1a0b0c27-46e2-88b2-8000-0914bdb43c0e", chatID, "chat.id from first frame")
	require.Equal(t, "1a0b0c27-46e2-88b4-8000-0a1448151b8d", assistantID, "assistant message.id")
	require.True(t, done, "done termination frame must be detected")
	require.Equal(t, 3, heartbeats, "3 heartbeat frames must be skipped (produce no text)")
	require.GreaterOrEqual(t, think.Len(), 100, "thinking block must be extracted")
	require.Contains(t, think.String(), "你好", "thinking references the greeting")
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
