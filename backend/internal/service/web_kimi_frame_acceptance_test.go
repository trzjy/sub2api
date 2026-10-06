package service

// Kimi 网页出站修复（commit 688ec381c）的**独立验收**测试。
//
// 立场：验收方视角，不复用实现代理的结论。每条用例都要能回答「凭什么说它真能用」：
//   - 帧化：字节级校验（flag / 大端长度 / payload 逐字节），并对称回读验证「恰好一帧」；
//   - 双帧：直接盯 outbound 字节——buildWebKimiRequestBody 帧化一次，buildWebKimiUpstreamRequest
//     必须原样透传，401 重试的两次 chat body 逐字节相等，同时 refresh 请求体不得被误帧化；
//   - trailer 错误：字符串码 / 数字码 / 嵌套 / 顶层全矩阵；
//   - 假阴性防线：真实成功 fixture 的每一帧都不得被判成错误，端到端流与非流仍健康；
//   - 凭证：假设上游把 access_token 回显在错误帧里，客户端/错误文案都不准出现明文。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// --- 1. 编码器字节级正确性 + 与解码端严格对称 ---

func TestAcceptanceWebKimi_EnvelopeCodecRoundTrip(t *testing.T) {
	cases := map[string]string{
		"minimal":           `{}`,
		"ascii":             `{"message":{"blocks":[{"text":{"content":"hi"}]}}}`,
		"utf8_multibyte":    `{"content":"你好，世界"}`, // 长度必须按字节而非 rune
		"len_127":           `{"p":"` + string(bytes.Repeat([]byte("a"), 100)) + `"}`,
		"len_65535_payload": `{"p":"` + string(bytes.Repeat([]byte("b"), 65500)) + `"}`,
		"escaped_quotes":    `{"content":"say \"hi\"\n\tok"}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			frame := encodeWebKimiConnectEnvelope([]byte(payload))

			require.Len(t, frame, 5+len(payload), "frame must be 5-byte header + payload")
			require.Equal(t, byte(0x00), frame[0], "flag byte must be 0x00 (uncompressed)")
			require.Equal(t, uint32(len(payload)), binary.BigEndian.Uint32(frame[1:5]),
				"4-byte big-endian length prefix must equal the payload byte length")
			require.Equal(t, []byte(payload), frame[5:], "payload must be copied verbatim")

			// 对称回读：必须恰好读出一帧，之后立即 EOF。
			r := bufio.NewReader(bytes.NewReader(frame))
			got, more, err := readWebKimiConnectEnvelope(r)
			require.NoError(t, err)
			require.True(t, more)
			require.Equal(t, payload, string(got), "decode(encode(x)) must be identity")
			_, more, err = readWebKimiConnectEnvelope(r)
			require.NoError(t, err)
			require.False(t, more, "exactly one frame must be produced, trailing bytes indicate double framing")
		})
	}
}

// TestAcceptanceWebKimi_DoubleFrameIsObservable 双帧必须是**可被检出**的非幂等操作，
// 否则「帧化一次」的回归断言本身毫无意义。
func TestAcceptanceWebKimi_DoubleFrameIsObservable(t *testing.T) {
	once := encodeWebKimiConnectEnvelope([]byte(`{"a":1}`))
	twice := encodeWebKimiConnectEnvelope(once)

	require.Len(t, twice, len(once)+5, "second framing adds exactly another 5-byte header")
	require.Equal(t, uint32(len(once)), binary.BigEndian.Uint32(twice[1:5]))
	require.Equal(t, byte(0x00), twice[5], "the double-framed inner header is observable at offset 5")
	require.Equal(t, byte('{'), once[5], "single framing must expose raw JSON at offset 5")
}

// --- 2. build 链路：帧化恰好一次，build-request 不二次帧化 ---

func TestAcceptanceWebKimi_BuildRequestBodyEmitsExactlyOneFrame(t *testing.T) {
	account := webKimiTestAccount(8971, nil)
	frame := buildWebKimiRequestBody("ping 中文", "k3", account)

	require.Equal(t, byte(0x00), frame[0])
	declared := int(binary.BigEndian.Uint32(frame[1:5]))
	require.Equal(t, len(frame)-5, declared, "length prefix must match trailing payload size")

	payload := frame[5:]
	require.True(t, json.Valid(payload), "payload must be valid JSON after stripping the header")
	require.Equal(t, byte('{'), payload[0], "no second envelope header — payload starts with raw JSON")
	require.Equal(t, "k3", gjson.GetBytes(payload, "options.model").String())
	require.Equal(t, "SCENARIO_CHAT", gjson.GetBytes(payload, "scenario").String())
	require.Equal(t, "ping 中文", gjson.GetBytes(payload, "message.blocks.0.text.content").String())
	require.False(t, gjson.GetBytes(payload, "chatId").Exists(), "request must not carry chatId")

	// 回读：整帧只读出一帧，且内容与剥出来的 payload 完全相同。
	r := bufio.NewReader(bytes.NewReader(frame))
	got, more, err := readWebKimiConnectEnvelope(r)
	require.NoError(t, err)
	require.True(t, more)
	require.JSONEq(t, string(payload), string(got))
	_, more, err = readWebKimiConnectEnvelope(r)
	require.NoError(t, err)
	require.False(t, more)
}

func TestAcceptanceWebKimi_BuildUpstreamRequestDoesNotReframe(t *testing.T) {
	account := webKimiTestAccount(8972, nil)
	svc := &OpenAIGatewayService{}
	body := buildWebKimiRequestBody("hi", "k3", account)

	req1, err := svc.buildWebKimiUpstreamRequest(context.Background(), account, "https://www.kimi.com"+webKimiChatPath, "token-one", body)
	require.NoError(t, err)
	sent1, err := io.ReadAll(req1.Body)
	require.NoError(t, err)
	require.Equal(t, body, sent1, "build-request must pass the already-framed body through byte-for-byte")
	require.Equal(t, int64(len(body)), req1.ContentLength, "Content-Length must match the framed body")
	require.Equal(t, "application/connect+json", req1.Header.Get("Content-Type"))

	// 401 → refresh → 重试：换 token 重建请求，出站字节必须逐字节等同。
	req2, err := svc.buildWebKimiUpstreamRequest(context.Background(), account, "https://www.kimi.com"+webKimiChatPath, "token-two", body)
	require.NoError(t, err)
	sent2, err := io.ReadAll(req2.Body)
	require.NoError(t, err)
	require.Equal(t, sent1, sent2, "retry must re-send identical bytes (no re-framing)")
	require.Equal(t, "Bearer token-two", req2.Header.Get("Authorization"))
}

// TestAcceptanceWebKimi_RetryBodyIsNotReframed 端到端：401→refresh→重试链路里三次上游
// 请求的 body 形态都要对（chat 帧化一次且两次一致；refresh 是 REST，绝不能帧化）。
func TestAcceptanceWebKimi_RetryBodyIsNotReframed(t *testing.T) {
	account := webKimiTestAccount(8973, map[string]any{"refresh_token": "refresh-accept-secret-XYZ98765"})
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		webKimiUnauthenticatedResponse(),
		{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader([]byte(`{"accessToken":"new-token-abc54321"}`))),
		},
		webKimiConnectResponse(),
	}}

	_, err := runForwardWebKimi(t, account, webKimiInboundBody("kimi-k3"), "kimi-k3", upstream, &RateLimitService{})
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 3, "expected chat, refresh, retried chat")

	first, second := upstream.bodies[0], upstream.bodies[2]
	require.Equal(t, first, second, "retried chat body must be byte-identical to the first attempt")

	for i, frame := range [][]byte{first, second} {
		require.GreaterOrEqual(t, len(frame), 5, "chat body #%d must be framed", i)
		require.Equal(t, byte(0x00), frame[0], "chat body #%d flag must be 0x00", i)
		require.Equal(t, uint32(len(frame)-5), binary.BigEndian.Uint32(frame[1:5]),
			"chat body #%d length prefix must equal trailing size — a second header would break this", i)
		require.True(t, json.Valid(frame[5:]), "chat body #%d payload must be valid JSON", i)
		require.Equal(t, byte('{'), frame[5], "chat body #%d must not be double framed", i)
	}

	// refresh 端点是 REST JSON：必须仍是裸 JSON，若被帧化则127 字节长之上多出 5 字节帧头。
	refreshBody := upstream.bodies[1]
	require.JSONEq(t, `{"refreshToken":"refresh-accept-secret-XYZ98765"}`, string(refreshBody),
		"token refresh body must stay raw JSON (not Connect framed)")
}

// --- 3. trailer / 错误解析矩阵 ---

func TestAcceptanceWebKimi_ErrorParseMatrix(t *testing.T) {
	cases := []struct {
		name     string
		payload  string
		wantAuth bool
		wantNum  int64
		wantStr  string
	}{
		{"trailer_string_invalid_argument", `{"error":{"code":"invalid_argument","message":"subscription required","details":[{"@type":"x"}]}}`, false, 0, "invalid_argument"},
		{"trailer_string_unauthenticated", `{"error":{"code":"unauthenticated","message":"token expired"}}`, true, 0, "unauthenticated"},
		{"trailer_numeric_code", `{"error":{"code":16,"message":"quota exceeded"}}`, false, 16, ""},
		{"top_level_numeric_code", `{"code":1001,"message":"quota exceeded"}`, false, 1001, ""},
		{"top_level_string_code", `{"code":"resource_exhausted"}`, false, 0, "resource_exhausted"},
		{"trailer_message_unauthenticated_only", `{"error":{"code":"unknown","message":"Unauthenticated"}}`, true, 0, "unknown"},
		{"zero_numeric_code_is_not_an_error", `{"code":0,"chat":{"id":"c-1"}}`, false, 0, ""},
		{"empty_string_code_is_not_an_error", `{"code":"","chat":{"id":"c-1"}}`, false, 0, ""},
		{"null_error_object", `{"error":null,"chat":{"id":"c-1"}}`, false, 0, ""},
		{"unrelated_error_field_string", `{"error":"not an object"}`, false, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := parseWebKimiEnvelopePayload([]byte(tc.payload))
			require.Equal(t, tc.wantAuth, ev.AuthFailed, "AuthFailed")
			require.Equal(t, tc.wantNum, ev.ErrCode, "ErrCode")
			require.Equal(t, tc.wantStr, ev.ErrCodeStr, "ErrCodeStr")
		})
	}
}

// --- 4. 假阴性防线：成功流绝不能被新增校验误判 ---

func TestAcceptanceWebKimi_SuccessFramesNeverFlaggedAsError(t *testing.T) {
	successFrames := append([]string{}, webKimiFixtureEnvelopeFrames...)
	successFrames = append(successFrames,
		`{"heartbeat":{}}`,
		`{"eventOffset":42,"done":{}}`,
		`{"op":"append","mask":"block.think.content","eventOffset":8,"block":{"id":"2","think":{"content":"思考中"}}}`,
		`{"op":"set","mask":"message","eventOffset":6,"message":{"id":"asst-2","role":"assistant","blocks":[{"text":{"content":"全文"}]}}}`,
		`{"eventOffset":1,"chat":{"id":"chat-9","code":"cn-session-alias"}}`, // chat 内部的 code 不是错误码，不得被上提为业务错误
	)
	for i, payload := range successFrames {
		ev := parseWebKimiEnvelopePayload([]byte(payload))
		require.False(t, ev.AuthFailed, "frame #%d (%s) must not be flagged as auth error", i, payload)
		require.Zero(t, ev.ErrCode, "frame #%d (%s) must not carry a numeric error code", i, payload)
		require.Empty(t, ev.ErrCodeStr, "frame #%d (%s) must not carry a string error code", i, payload)
	}
}

func acceptanceWebKimiStreamRunner(t *testing.T, resp *http.Response, account *Account, model string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(webKimiInboundBody(model)))
	svc := &OpenAIGatewayService{
		cfg: &config.Config{
			Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true}},
		},
	}
	_, err := svc.handleWebKimiStreamingResponse(context.Background(), resp, c, account, model, model, time.Now(), webResponseModeChat)
	return recorder, err
}

// TestAcceptanceWebKimi_SuccessEndToEndStillHealthy 修复不得引入假阴性：成功 ITLITE
// 必须仍然正常出正文、流式仍写正常终止帧、探活仍判健康。
func TestAcceptanceWebKimi_SuccessEndToEndStillHealthy(t *testing.T) {
	t.Run("non_streaming", func(t *testing.T) {
		account := webKimiTestAccount(8974, nil)
		upstream := &httpUpstreamRecorder{responses: []*http.Response{webKimiConnectResponse()}}
		recorder, err := runForwardWebKimi(t, account, webKimiInboundBody("kimi-k3"), "kimi-k3", upstream, &RateLimitService{})
		require.NoError(t, err)
		body := recorder.Body.String()
		require.Contains(t, body, `"content":"hi there"`, "success must still aggregate the text")
		require.Contains(t, body, `"id":"asst-1"`)
		require.NotContains(t, body, "error", "success must not be turned into an error")
	})

	t.Run("streaming", func(t *testing.T) {
		account := webKimiTestAccount(8975, nil)
		recorder, err := acceptanceWebKimiStreamRunner(t, webKimiStreamResponse(), account, "kimi-k3")
		require.NoError(t, err)
		body := recorder.Body.String()
		require.Contains(t, body, `"content":"hi "`)
		require.Contains(t, body, `"content":"there"`)
		require.Contains(t, body, `"finish_reason":"stop"`, "success must emit the terminal chunk")
		require.Contains(t, body, "data: [DONE]", "success must terminate normally")
		require.NotContains(t, body, `"error"`, "success must not emit an error marker")
	})

	t.Run("probe", func(t *testing.T) {
		require.Empty(t, evaluateWebKimiProbeStream(bytes.NewReader(webKimiEnvelopeStream(webKimiFixtureEnvelopeFrames...))),
			"the healthy fixture must be reported healthy")
		require.Empty(t, evaluateWebKimiProbeStream(bytes.NewReader(webKimiEnvelopeStream(
			`{"eventOffset":1,"chat":{"id":"c-1"}}`,
			`{"eventOffset":2,"block":{"think":{"content":"思考"}}}`,
			`{"eventOffset":3,"done":{}}`,
		))), "think-only success frames must still count as valid")
	})
}

// TestAcceptanceWebKimi_ProbeReasonMatrix 探活 reason 判定：错误必须落到对应口径，
// 「空流」只留给真的没有任何有效帧的情况。
func TestAcceptanceWebKimi_ProbeReasonMatrix(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"trailer_business_error", string(webKimiEnvelopeTrailerStream(
			`{"error":{"code":"invalid_argument","message":"subscription required"}}`)), webProbeReasonBusinessErr},
		{"trailer_auth_error", string(webKimiEnvelopeTrailerStream(
			`{"error":{"code":"unauthenticated","message":"access token expired"}}`)), webProbeReasonAuthErr},
		{"framed_numeric_code", string(webKimiEnvelopeStream(`{"code":1001,"message":"quota"}`)), webProbeReasonBusinessErr},
		{"framed_string_code", string(webKimiEnvelopeStream(`{"code":"unauthenticated"}`)), webProbeReasonAuthErr},
		{"bare_json_nested_error", `{"error":{"code":"invalid_argument"}}`, webProbeReasonBusinessErr},
		{"bare_json_auth_error", `{"error":{"code":"unauthenticated"}}`, webProbeReasonAuthErr},
		{"empty_body", "", webProbeReasonEmptyStream},
		{"heartbeat_only", string(webKimiEnvelopeStream(`{"heartbeat":{}}`)), webProbeReasonEmptyStream},
		{"truncated_frame", string(webKimiEnvelopeFrame(`{"eventOffset":1,"chat":{"id":"c-1"}}`))[:12], webProbeReasonEmptyStream},
		{"healthy", string(webKimiEnvelopeStream(
			`{"eventOffset":1,"chat":{"id":"c-1"}}`,
			`{"eventOffset":2,"block":{"text":{"content":"hi"}}}`,
			`{"eventOffset":3,"done":{}}`)), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, evaluateWebKimiProbeStream(bytes.NewReader([]byte(tc.body))))
		})
	}
}

// --- 5. 凭证红线：假设上游把 token 回显进错误帧，任何出口都不准出现明文 ---

func TestAcceptanceWebKimi_CredentialsNeverEchoed(t *testing.T) {
	const accessSecret = "ACCEPT_SECRET_ACCESS_1234567890"
	const refreshSecret = "ACCEPT_SECRET_REFRESH_1234567890"
	echo := `{"error":{"code":"invalid_argument","message":"bad token ACCEPT_SECRET_ACCESS_1234567890 for refresh ACCEPT_SECRET_REFRESH_1234567890"}}`
	account := webKimiTestAccount(8976, map[string]any{
		"access_token":  accessSecret,
		"refresh_token": refreshSecret,
	})

	t.Run("non_streaming", func(t *testing.T) {
		upstream := &httpUpstreamRecorder{responses: []*http.Response{webKimiTrailerResponse(echo)}}
		recorder, err := runForwardWebKimi(t, account, webKimiInboundBody("kimi-k3"), "kimi-k3", upstream, &RateLimitService{})
		require.Error(t, err)
		require.NotContains(t, recorder.Body.String(), accessSecret)
		require.NotContains(t, recorder.Body.String(), refreshSecret)
		require.NotContains(t, err.Error(), accessSecret)
		require.NotContains(t, err.Error(), refreshSecret)
	})

	t.Run("streaming_first_frame_error", func(t *testing.T) {
		recorder, err := acceptanceWebKimiStreamRunner(t, webKimiTrailerResponse(echo), account, "kimi-k3")
		require.Error(t, err)
		require.NotContains(t, recorder.Body.String(), accessSecret)
		require.NotContains(t, recorder.Body.String(), refreshSecret)
		require.NotContains(t, err.Error(), accessSecret)
	})

	t.Run("streaming_midstream_error", func(t *testing.T) {
		stream := append(webKimiEnvelopeStream(
			`{"eventOffset":1,"chat":{"id":"c-1"}}`,
			`{"eventOffset":2,"block":{"text":{"content":"partial answer"}}}`,
		), webKimiEnvelopeTrailerFrame(echo)...)
		recorder, err := acceptanceWebKimiStreamRunner(t, &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/connect+json"}},
			Body:       io.NopCloser(bytes.NewReader(stream)),
		}, account, "kimi-k3")
		require.NoError(t, err, "midstream error is absorbed into the SSE stream")
		out := recorder.Body.String()
		require.Contains(t, out, "partial answer")
		require.Contains(t, out, `"error"`, "midstream failure must surface an error marker")
		require.NotContains(t, out, `"finish_reason":"stop"`, "midstream failure must not close as a normal completion")
		require.NotContains(t, out, accessSecret, "credentials echoed by upstream must never reach the client")
		require.NotContains(t, out, refreshSecret)
	})

	t.Run("probe", func(t *testing.T) {
		svc, _ := webProbeService(webProbeResponse(http.StatusOK, string(webKimiEnvelopeTrailerStream(echo))))
		probeAccount := &Account{
			ID: 8977, Platform: PlatformKimi, Type: AccountTypeAPIKey, Concurrency: 1,
			Credentials: map[string]any{
				"access_mode":   AccountAccessModeWeb,
				"access_token":  accessSecret,
				"refresh_token": refreshSecret,
			},
		}
		ctx, rec := newWebTestContext()
		err := svc.testWebAccountConnection(ctx, probeAccount, "", "")
		require.Error(t, err)
		require.NotContains(t, err.Error(), accessSecret)
		require.NotContains(t, rec.Body.String(), accessSecret)
		require.NotContains(t, rec.Body.String(), refreshSecret)
	})
}
