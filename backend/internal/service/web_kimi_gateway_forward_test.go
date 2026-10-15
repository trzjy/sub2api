package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
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
		{"kimi-2.6", "k2d6"},
		{"kimi-2.6-chat", "k2d6-chat"},
		{"kimi-k2d6", "k2d6"},
		{"kimi-k2d6-chat", "k2d6-chat"},
		{"k2d6", "k2d6"},
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
		// 恰好一帧：解码端回读一帧后必须到流尾；若存在第二帧即为双帧（重试链路的历史陷阱）。
		rd := bufio.NewReader(bytes.NewReader(frame))
		payload, more, rerr := readWebKimiConnectEnvelope(rd)
		require.NoError(t, rerr)
		require.True(t, more)
		require.JSONEq(t, string(frame[5:]), string(payload))
		_, more, rerr = readWebKimiConnectEnvelope(rd)
		require.NoError(t, rerr)
		require.False(t, more, "exactly one envelope frame, otherwise the body is double framed")
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
			c, webKimiTestAccount(8944, nil), "kimi-k3", "k3", time.Now(), "hi", webResponseModeChat)
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
		context.Background(), webKimiStreamResponse(), c, account, "kimi-k3", "k3", time.Now(), "hi", webResponseModeChat)
	require.NoError(t, err)
	require.True(t, result.Stream)

	out := recorder.Body.String()
	require.Contains(t, out, `"content":"hi "`)
	require.Contains(t, out, `"content":"there"`)
	require.Contains(t, out, "chat.completion.chunk")
	require.Contains(t, out, "data: [DONE]")
}

// TestForwardWebKimi_StreamingUserEchoFrameWithUnauthenticatedWord 端到端复现 2026-09-22
// 线上事故：上游正常首帧为 op/set mask=message role=user 的用户消息回显帧，其正文携带的
// prompt 全文里含英文单词 "unauthenticated"（AGENTS.md 协议文本随 ZCode 会话注入）。
// 旧实现全文扫描误判认证失败 → 502；修复后该流必须正常回写。
func TestForwardWebKimi_StreamingUserEchoFrameWithUnauthenticatedWord(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{}`)))

	frames := []string{
		`{"op":"set", "mask":"message", "eventOffset":4, "message":{"id":"u1","parentId":"p1","role":"user","status":"MESSAGE_STATUS_COMPLETED","blocks":[{"messageId":"","text":{"content":"<system-reminder>\nAGENTS.md: If a required CLI is unavailable or unauthenticated, stop and repair that CLI setup.\n</system-reminder>\nkimi反代，获取模型列表失败这个问题根因分析一下。"}}],"scenario":"SCENARIO_CHAT","createTime":"2026-09-22T09:00:29.204302Z","isGoal":false}}`,
		`{"op":"set","mask":"message","eventOffset":5,"message":{"id":"asst-1","parentId":"u1","role":"assistant","status":"MESSAGE_STATUS_GENERATING"}}`,
		`{"op":"append","mask":"block.text.content","eventOffset":10,"block":{"id":"4","text":{"content":"root cause analysis"}}}`,
		`{"eventOffset":13,"done":{}}`,
	}
	svc := &OpenAIGatewayService{
		cfg: &config.Config{
			Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true}},
		},
	}
	account := webKimiTestAccount(111, nil)
	result, err := svc.handleWebKimiStreamingResponse(
		context.Background(), webKimiEnvelopeResponse(frames), c, account, "kimi-k2d6", "k2d6", time.Now(), "hi", webResponseModeAnthropic)
	require.NoError(t, err, "user-echo frame with 'unauthenticated' word must not fail the stream")
	require.True(t, result.Stream)

	out := recorder.Body.String()
	require.Contains(t, out, "message_start")
	require.Contains(t, out, "root cause analysis")
	require.NotContains(t, out, "unauthenticated", "user prompt echo must never leak into client SSE")
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
		context.Background(), resp, c, account, "kimi-k3", "k3", time.Now(), "hi", webResponseModeChat)
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
		name    string
		line    string
		isError bool
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
	t.Run("user_echo_frame_with_unauthenticated_word_not_auth_failure", func(t *testing.T) {
		// 线上事故回归（2026-09-22）：op/set mask=message 的用户消息回显帧里
		// blocks[].text.content 承载用户 prompt 全文；正文出现英文单词
		// "unauthenticated"（如 AGENTS.md 协议文本被注入 prompt）时，旧实现把
		// 顶层 message 序列化后做全文扫描误判为认证失败，正常流被收口成 502。
		// 修复后认证判定只认结构化错误位置（code 字段值 / 标量 message 文案）。
		frame := `{"op":"set", "mask":"message", "eventOffset":4, "message":{"id":"m1","parentId":"p1","role":"user","status":"MESSAGE_STATUS_COMPLETED","blocks":[{"messageId":"","text":{"content":"If a required CLI is unavailable or unauthenticated, stop and repair that CLI setup."}}],"scenario":"SCENARIO_CHAT","createTime":"2026-09-22T09:00:29.204302Z","isGoal":false}}`
		ev := parseWebKimiEnvelopePayload([]byte(frame))
		require.False(t, ev.AuthFailed, "benign user-echo frame must not be classified as auth failure")
		require.Empty(t, ev.ErrCodeStr)
		require.Zero(t, ev.ErrCode)
	})
	t.Run("assistant_message_with_unauthenticated_word_not_auth_failure", func(t *testing.T) {
		// assistant 消息帧同理：message 对象承载业务载荷，不参与认证扫描。
		ev := parseWebKimiEnvelopePayload([]byte(`{"op":"set","mask":"message","eventOffset":6,"message":{"id":"a1","role":"assistant","status":"MESSAGE_STATUS_GENERATING","blocks":[{"text":{"content":"the token is unauthenticated"}}]}}`))
		require.False(t, ev.AuthFailed)
	})
	t.Run("scalar_message_fallback_auth_detection", func(t *testing.T) {
		// code 缺失但 message 为标量错误文案的兜底形态仍要识别。
		ev := parseWebKimiEnvelopePayload([]byte(`{"message":"request rejected: unauthenticated"}`))
		require.True(t, ev.AuthFailed)
	})
	t.Run("object_message_never_error_text", func(t *testing.T) {
		// message 为对象（业务消息帧）时不得被字符串化提取为错误文案（防用户 prompt
		// 全文泄漏进客户端 error 响应与 ops_error_logs，2026-09-22 事故伴随损伤）。
		frame := []byte(`{"op":"set","mask":"message","eventOffset":4,"message":{"id":"u1","role":"user","blocks":[{"text":{"content":"hi"}}]}}`)
		ev := parseWebKimiEnvelopePayload(frame)
		msg := webKimiUpstreamErrorMessage(frame)
		require.False(t, ev.AuthFailed)
		require.Empty(t, msg, "object-valued message must not be stringified into error text")
	})
	t.Run("scalar_message_error_text_still_extracted", func(t *testing.T) {
		msg := webKimiUpstreamErrorMessage([]byte(`{"code":"unauthenticated","message":"Session expired"}`))
		require.Equal(t, "Session expired", msg)
		msg2 := webKimiUpstreamErrorMessage([]byte(`{"error":{"code":"invalid_argument","message":"subscription required"}}`))
		require.Equal(t, "subscription required", msg2)
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

// ── 整改1/2/3/4 的回归锚点 ────────────────────────────────────────────────

// webKimiSilentBody 是一个「开了 HTTP 200 流但永不吐数据」的响应体：Read
// 永久阻塞且 Close 不解除（模拟挂死连接 / 中间设备吞流）。整改前 kimi 流式
// 回程是裸 for + io.ReadFull，遇到这种上游会永久阻塞、goroutine 不归还；
// 整改后由 webStreamGuard 的 interval 超时收口。
type webKimiSilentBody struct{ closed chan struct{} }

func (b *webKimiSilentBody) Read([]byte) (int, error) {
	<-b.closed // 永不返回数据，直到测试收尾时 Close
	return 0, io.EOF
}

func (b *webKimiSilentBody) Close() error { close(b.closed); return nil }

func webKimiSilentResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/connect+json"}},
		Body:       &webKimiSilentBody{closed: make(chan struct{})},
	}
}

// newWebKimiStreamingTestService 构造带可配超时阈值的 service。interval/keepalive
// 取秒为单位的配置项，测试里给极小值以在毫秒级触发，避免依赖真实 180s 默认值。
func newWebKimiStreamingTestService(intervalSeconds, keepaliveSeconds int) *OpenAIGatewayService {
	return &OpenAIGatewayService{
		cfg: &config.Config{
			Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true}},
			Gateway: config.GatewayConfig{
				StreamDataIntervalTimeout: intervalSeconds,
				StreamKeepaliveInterval:   keepaliveSeconds,
			},
		},
	}
}

// TestForwardWebKimi_StreamingUpstreamSilentHitsIntervalTimeout 整改1 核心用例：
// 上游开流后不吐任何数据，流式回程必须在 interval 内退出，而不是永久阻塞。
// 修复前此用例会挂到 go test 超时。
func TestForwardWebKimi_StreamingUpstreamSilentHitsIntervalTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := newLockedResponseRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := webKimiInboundBody("kimi-k3")
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))

	svc := newWebKimiStreamingTestService(1, 0)
	account := webKimiTestAccount(8991, nil)

	done := make(chan struct{})
	var (
		result *OpenAIForwardResult
		err    error
	)
	go func() {
		defer close(done)
		result, err = svc.handleWebKimiStreamingResponse(
			context.Background(), webKimiSilentResponse(), c, account, "kimi-k3", "k3", time.Now(), "hi", webResponseModeChat)
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("streaming response must terminate on stream data interval timeout; it blocked forever")
	}

	require.Error(t, err, "silent upstream must surface an error, not a fake success")
	require.ErrorIs(t, err, errWebStreamIdleTimeout)
	require.Nil(t, result)
	// 未写出任何帧时不得伪造正常终止帧。
	require.NotContains(t, recorder.String(), "data: [DONE]")
}

// TestForwardWebKimi_StreamingReadFailureRegistersOpsUpstreamContext 整改4：
// 流式读失败必须登记 ops 上游上下文（此前三字段全空，attempt 级遥测丢失）。
// 断言 platform / account / proxy 归属与 kind 均落库。
func TestForwardWebKimi_StreamingReadFailureRegistersOpsUpstreamContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := webKimiInboundBody("kimi-k3")
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))

	svc := newWebKimiStreamingTestService(1, 0)
	account := webKimiTestAccount(8992, nil)

	_, err := svc.handleWebKimiStreamingResponse(
		context.Background(), webKimiSilentResponse(), c, account, "kimi-k3", "k3", time.Now(), "hi", webResponseModeChat)
	require.Error(t, err)

	// upstream_errors：attempt 级遥测必须落库。
	raw, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok, "stream read failure must record an upstream attempt event")
	events := raw.([]*OpsUpstreamErrorEvent)
	require.Len(t, events, 1)
	ev := events[0]
	require.Equal(t, "stream_read_error", ev.Kind)
	require.Equal(t, PlatformKimi, ev.Platform)
	require.Equal(t, account.ID, ev.AccountID)
	require.Equal(t, account.Name, ev.AccountName)
	require.NotEmpty(t, ev.ProxyName, "proxy attribution must be snapshotted even for direct routes")

	// upstream_error_message：此前为空，排障时看不到任何线索。
	msg, ok := c.Get(OpsUpstreamErrorMessageKey)
	require.True(t, ok, "upstream_error_message must be populated")
	require.NotEmpty(t, msg.(string))
}

// TestForwardWebKimi_StreamingProducesNonZeroEstimatedTokens 整改3：
// Connect envelope 流内无 usage 字段，流式此前恒返回零 token，token 计费模式下
// 真实零计费。断言流式与非流式同口径产出非零 usage。
func TestForwardWebKimi_StreamingProducesNonZeroEstimatedTokens(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := webKimiInboundBody("kimi-k3")
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))

	svc := newWebKimiStreamingTestService(0, 0)
	account := webKimiTestAccount(8993, nil)

	result, err := svc.handleWebKimiStreamingResponse(
		context.Background(), webKimiStreamResponse(), c, account, "kimi-k3", "k3", time.Now(),
		"请用一句话解释什么是 goroutine 泄漏", webResponseModeChat)
	require.NoError(t, err)
	require.NotNil(t, result)

	// 上游流内无 usage：两个方向都必须由 estimateWebUsage 兜底为非零，
	// 否则单价×0 → 扣费门禁把整笔请求拦下（真实零计费）。
	require.Greater(t, result.Usage.InputTokens, 0, "streaming input tokens must be estimated, not zero")
	require.Greater(t, result.Usage.OutputTokens, 0, "streaming output tokens must be estimated, not zero")
	// 输出 token 来自聚合正文 "hi there"（2 个 ASCII 词）。
	require.Equal(t, estimateTokenCount("hi there"), result.Usage.OutputTokens)

	// 终帧必须携带 usage，客户端才能观测到用量。
	require.Contains(t, recorder.Body.String(), `"usage"`)
}

// TestForwardWebKimi_StreamingUsageMatchesNonStreamingSameCensus 整改3 口径一致性：
// 同一份上游流，流式与非流式的估算结果必须一致（避免两条路径口径漂移导致
// 同类请求计费不同）。
func TestForwardWebKimi_StreamingUsageMatchesNonStreamingSameCensus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const prompt = "hello world"

	streamRecorder := httptest.NewRecorder()
	streamCtx, _ := gin.CreateTestContext(streamRecorder)
	body := webKimiInboundBody("kimi-k3")
	streamCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	svc := newWebKimiStreamingTestService(0, 0)
	account := webKimiTestAccount(8994, nil)
	streamResult, err := svc.handleWebKimiStreamingResponse(
		context.Background(), webKimiStreamResponse(), streamCtx, account, "kimi-k3", "k3", time.Now(), prompt, webResponseModeChat)
	require.NoError(t, err)

	nonStreamRecorder := httptest.NewRecorder()
	nonStreamCtx, _ := gin.CreateTestContext(nonStreamRecorder)
	nonStreamCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	nonStreamResult, err := svc.handleWebKimiNonStreamingResponse(
		context.Background(), webKimiStreamResponse(), nonStreamCtx, account, "kimi-k3", "k3", time.Now(), prompt, webResponseModeChat)
	require.NoError(t, err)

	require.Equal(t, nonStreamResult.Usage.InputTokens, streamResult.Usage.InputTokens)
	require.Equal(t, nonStreamResult.Usage.OutputTokens, streamResult.Usage.OutputTokens)
}

// TestForwardWebKimi_StreamingKeepaliveBeforeFirstFrame 整改1 keepalive：
// 首帧前的静默期必须向下游发 SSE 注释保活帧，防住 Cloudflare Tunnel / 反代
// 按空闲断开连接；且保活帧必须是 SSE 注释而非 data 帧（不污染 TTFT 与消息序列）。
func TestForwardWebKimi_StreamingKeepaliveBeforeFirstFrame(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// 用带锁 recorder：保活帧由转发 goroutine 写出，本用例在测试 goroutine 里
	// 轮询读取，httptest.ResponseRecorder 的 bytes.Buffer 非并发安全。
	recorder := newLockedResponseRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := webKimiInboundBody("kimi-k3")
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))

	svc := newWebKimiStreamingTestService(5, 1)
	account := webKimiTestAccount(8995, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = svc.handleWebKimiStreamingResponse(
			context.Background(), webKimiSilentResponse(), c, account, "kimi-k3", "k3", time.Now(), "hi", webResponseModeChat)
	}()

	require.Eventually(t, func() bool {
		return strings.Contains(recorder.String(), ": \n")
	}, 10*time.Second, 20*time.Millisecond, "a silent upstream must trigger downstream keepalive before the first frame")
	<-done

	// 保活帧不得是 data 帧——否则会被客户端解析成 assistant 内容。
	require.NotContains(t, recorder.String(), "data: ")
}

// lockedResponseRecorder 是并发安全的 gin.ResponseWriter：转发 goroutine 写、
// 测试 goroutine 轮询读，两者用互斥量隔开。
type lockedResponseRecorder struct {
	mu     sync.Mutex
	header http.Header
	body   bytes.Buffer
	code   int
}

func newLockedResponseRecorder() *lockedResponseRecorder {
	return &lockedResponseRecorder{header: http.Header{}}
}

func (r *lockedResponseRecorder) Header() http.Header { return r.header }

func (r *lockedResponseRecorder) WriteHeader(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.code = code
}

func (r *lockedResponseRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.Write(p)
}

func (r *lockedResponseRecorder) WriteString(s string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.WriteString(s)
}

func (r *lockedResponseRecorder) Flush() {}

func (r *lockedResponseRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.String()
}

// TestForwardWebKimi_StreamingClientDisconnectStopsWritingAndDrains 整改2：
// 客户端断开后不得继续向下游写，但仍须把已产生的 usage 带回结果并标记
// ClientDisconnect——响应已提交、上游已真实消耗额度，此时丢弃 usage 即平台漏记。
func TestForwardWebKimi_StreamingClientDisconnectStopsWritingAndDrains(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := &disconnectAfterFirstFrameRecorder{header: http.Header{}}
	c, _ := gin.CreateTestContext(recorder)
	body := webKimiInboundBody("kimi-k3")
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))

	svc := newWebKimiStreamingTestService(0, 0)
	account := webKimiTestAccount(8996, nil)

	result, err := svc.handleWebKimiStreamingResponse(
		context.Background(), webKimiStreamResponse(), c, account, "kimi-k3", "k3", time.Now(), "hello", webResponseModeChat)
	require.NoError(t, err, "client disconnect must not be surfaced as a forward error")
	require.NotNil(t, result)
	require.True(t, recorder.disconnected, "test precondition: the client writer must have failed")
	require.True(t, result.ClientDisconnect, "result must flag client disconnect so the handler still submits usage")
	// 断开前已收到上游正文，估算 usage 必须非零——否则这次请求白烧上游额度。
	require.Greater(t, result.Usage.OutputTokens, 0, "usage collected before disconnect must still be billed")
}

// disconnectAfterFirstFrameRecorder 在首帧写出后即返回写错误，模拟客户端断开。
type disconnectAfterFirstFrameRecorder struct {
	header       http.Header
	writes       int
	body         bytes.Buffer
	disconnected bool
}

func (r *disconnectAfterFirstFrameRecorder) Header() http.Header { return r.header }
func (r *disconnectAfterFirstFrameRecorder) WriteHeader(int)     {}

func (r *disconnectAfterFirstFrameRecorder) Write(p []byte) (int, error) {
	r.writes++
	if r.writes > 1 {
		r.disconnected = true
		return 0, fmt.Errorf("client gone")
	}
	return r.body.Write(p)
}

func (r *disconnectAfterFirstFrameRecorder) WriteString(s string) (int, error) {
	return r.Write([]byte(s))
}

func (r *disconnectAfterFirstFrameRecorder) Flush() {}
