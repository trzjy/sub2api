package service

// web 探活 2xx 响应体内容判定回归测试（线上故障修复）。
//
// 背景：Kimi Connect RPC 的认证/业务错误随 HTTP 200 返回，上游异常时还可能只回空 body
// 或纯心跳帧。此前 testWebAccountConnection 只看 HTTP 状态码，2xx 直接判
// "Web login session is healthy."，把过期 access_token 的坏账号判成 healthy。
//
// 本文件锁定修复后的口径：2xx 必须消费响应流并校验有效帧（≥1 有效帧且无错误帧才算健康），
// 判定一律复用转发链既有解析器（kimi envelope / zhipu SSE / deepseek SSE）。
//
// 安全红线：任何失败文案不得回显凭证值或上游原文。

import (
	"bytes"
	"encoding/binary"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// webKimiEnvelopeFrame 按 Connect RPC 流式 envelope 组帧：1 字节 flag（0x00 不压缩）
// + 4 字节大端长度 + payload JSON（与 readWebKimiConnectEnvelope 严格对称）。
func webKimiEnvelopeFrame(payload string) []byte {
	frame := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	return frame
}

// webKimiEnvelopeStream 把若干 payload 组帧拼成一条完整 Connect 流。
func webKimiEnvelopeStream(payloads ...string) []byte {
	var out bytes.Buffer
	for _, p := range payloads {
		out.Write(webKimiEnvelopeFrame(p))
	}
	return out.Bytes()
}

// webProbeResponse 构造探活上游响应（200 默认；不发起真实网络）。
func webProbeResponse(statusCode int, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func okWebKimiProbeResponse() *http.Response {
	// 登录态实测形态：chat.id 首帧 → block.text.content 正文增量 → done 终止帧。
	return webProbeResponse(http.StatusOK, string(webKimiEnvelopeStream(
		`{"eventOffset":1,"chat":{"id":"chat-probe-1"}}`,
		`{"eventOffset":2,"message":{"id":"msg-probe-1","role":"assistant"}}`,
		`{"eventOffset":3,"block":{"text":{"content":"hi"}}}`,
		`{"eventOffset":4,"done":{}}`,
	)))
}

func okWebZhipuProbeResponse() *http.Response {
	// /assistant/stream 实测形态：parts[].content[].text 正文 + status=finish 终止。
	return webProbeResponse(http.StatusOK,
		`data: {"id":"conv-probe-1","parts":[{"content":[{"type":"text","text":"hi"}]}]}`+"\n\n"+
			`data: {"id":"conv-probe-1","parts":[{"status":"finish","content":[{"type":"tool_calls","tool_calls":{"name":"finish"}}]}],"status":"finish"}`+"\n\n")
}

// okWebDeepseekProbeResponse 深求探活成功响应：复用登录态实测 SSE 全文（09 §5 fixture），
// 保证探活判定与正式转发链同一形态。
func okWebDeepseekProbeResponse() *http.Response { return webDeepseekFixtureSSEResponse() }

// webProbeOKResponse 返回指定平台「登录态健康」形态的 200 探活响应。三个 web 平台的
// 流形态互不通用（kimi 是 Connect 二进制 envelope，zhipu/deepseek 是 SSE），探活判定按
// 平台解析器区分，故成功桩也必须按平台给对应形态。
func webProbeOKResponse(platform string) *http.Response {
	switch platform {
	case PlatformKimi:
		return okWebKimiProbeResponse()
	case PlatformZhipu:
		return okWebZhipuProbeResponse()
	case PlatformDeepseek:
		return okWebDeepseekProbeResponse()
	}
	return nil
}

// webProbeService 组装最小探活服务并回放指定响应（kimi / zhipu 单跳）。
func webProbeService(resp *http.Response) (*AccountTestService, *webProbeUpstream) {
	upstream := &webProbeUpstream{}
	return newWebTestService(upstream, resp), upstream
}

// --- kimi：有效帧 → 健康 ---

func TestWebProbe_KimiValidFramesReportHealthy(t *testing.T) {
	svc, _ := webProbeService(okWebKimiProbeResponse())
	account := &Account{
		ID: 8101, Platform: PlatformKimi, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "access_token": "kimi-access-token"},
	}
	ctx, rec := newWebTestContext()

	require.NoError(t, svc.testWebAccountConnection(ctx, account, "", ""))

	body := rec.Body.String()
	require.Contains(t, body, "Web login session is healthy.")
	require.Contains(t, body, `"type":"test_complete"`)
	require.Contains(t, body, `"success":true`)
}

// --- kimi：空 body → 失败（线上故障形态：空返回被误判为成功） ---

func TestWebProbe_KimiEmptyBodyFailsClosed(t *testing.T) {
	svc, _ := webProbeService(webProbeResponse(http.StatusOK, ""))
	account := &Account{
		ID: 8102, Platform: PlatformKimi, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "access_token": "expired-access-token"},
	}
	ctx, rec := newWebTestContext()

	err := svc.testWebAccountConnection(ctx, account, "", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty response")

	body := rec.Body.String()
	require.Contains(t, body, "empty response")
	require.NotContains(t, body, "Web login session is healthy.")
	require.NotContains(t, body, `"success":true`)
	require.NotContains(t, body, "expired-access-token", "失败文案不得回显凭证")
}

// --- kimi：只有心跳帧 → 失败 ---

func TestWebProbe_KimiHeartbeatOnlyFailsClosed(t *testing.T) {
	svc, _ := webProbeService(webProbeResponse(http.StatusOK, string(webKimiEnvelopeStream(
		`{"heartbeat":{}}`,
		`{"heartbeat":{}}`,
	))))
	account := &Account{
		ID: 8103, Platform: PlatformKimi, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "access_token": "expired-access-token"},
	}
	ctx, rec := newWebTestContext()

	err := svc.testWebAccountConnection(ctx, account, "", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty response")

	body := rec.Body.String()
	require.Contains(t, body, "empty response")
	require.NotContains(t, body, `"success":true`)
	require.NotContains(t, body, "expired-access-token")
}

// --- kimi：HTTP 200 + 裸 JSON unauthenticated → 失败且不回显 token ---

func TestWebProbe_KimiUnauthenticatedBareJSONFailsClosed(t *testing.T) {
	svc, _ := webProbeService(webProbeResponse(http.StatusOK, `{"code":"unauthenticated"}`))
	account := &Account{
		ID: 8104, Platform: PlatformKimi, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "access_token": "SUPER_SECRET_TOKEN_8877"},
	}
	ctx, rec := newWebTestContext()

	err := svc.testWebAccountConnection(ctx, account, "", "")
	require.Error(t, err)

	body := rec.Body.String()
	require.Contains(t, body, webKimiChatPath)
	require.NotContains(t, body, "Web login session is healthy.")
	require.NotContains(t, body, `"success":true`)
	require.NotContains(t, body, "SUPER_SECRET_TOKEN_8877", "失败文案不得回显 token 明文")
	require.NotContains(t, body, "unauthenticated}", "失败文案不得回显上游原文")
}

// --- kimi：HTTP 200 + envelope 内 unauthenticated → 失败（认证错误口径） ---

func TestWebProbe_KimiUnauthenticatedEnvelopeFailsClosed(t *testing.T) {
	svc, _ := webProbeService(webProbeResponse(http.StatusOK, string(webKimiEnvelopeStream(
		`{"heartbeat":{}}`,
		`{"code":"unauthenticated","message":"unauthenticated: access token expired"}`,
	))))
	account := &Account{
		ID: 8105, Platform: PlatformKimi, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "access_token": "SUPER_SECRET_TOKEN_8877"},
	}
	ctx, rec := newWebTestContext()

	err := svc.testWebAccountConnection(ctx, account, "", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unauthenticated")

	body := rec.Body.String()
	require.NotContains(t, body, `"success":true`)
	require.NotContains(t, body, "SUPER_SECRET_TOKEN_8877")
	require.NotContains(t, body, "access token expired", "失败文案不得回显上游原文")
}

// webKimiEnvelopeTrailerFrame 构造 flag=0x02 的 Connect trailer 帧（错误/元数据位），
// 即生产上「HTTP 200 + trailer {"error":{"code":"invalid_argument"}}」的实测线格式
// （分析文档 §1.4）。flag=0x02 对 readWebKimiConnectEnvelope 仍是「读 4 字节长度 + payload」。
func webKimiEnvelopeTrailerFrame(payload string) []byte {
	frame := webKimiEnvelopeFrame(payload)
	frame[0] = 0x02
	return frame
}

// webKimiEnvelopeTrailerStream 把若干错误载荷组 trailer 帧拼成一条完整 Connect 流。
func webKimiEnvelopeTrailerStream(payloads ...string) []byte {
	var out bytes.Buffer
	for _, p := range payloads {
		out.Write(webKimiEnvelopeTrailerFrame(p))
	}
	return out.Bytes()
}

// --- kimi：HTTP 200 + flag=2 trailer 业务错误（invalid_argument）→ 业务错误口径 ---

// 线上故障形态（分析文档 §1.4）：请求体未帧化时上游一律回 HTTP 200 + trailer
// {"error":{"code":"invalid_argument"}}。修复前解析器只认顶层 code → 事件全零 → 不计有效帧
// → 探活报 "an empty response (no valid frames parsed)"，掩盖真实业务错误。
func TestWebProbe_KimiTrailerBusinessErrorFailsClosed(t *testing.T) {
	svc, _ := webProbeService(webProbeResponse(http.StatusOK, string(webKimiEnvelopeTrailerStream(
		`{"error":{"code":"invalid_argument","message":"subscription required","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo"}]}}`,
	))))
	account := &Account{
		ID: 8107, Platform: PlatformKimi, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "access_token": "SUPER_SECRET_TOKEN_8877"},
	}
	ctx, rec := newWebTestContext()

	err := svc.testWebAccountConnection(ctx, account, "", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "business error", "trailer business error must NOT be reported as an empty stream")
	require.NotContains(t, err.Error(), "empty response")

	body := rec.Body.String()
	require.NotContains(t, body, "Web login session is healthy.")
	require.NotContains(t, body, `"success":true`)
	require.NotContains(t, body, "SUPER_SECRET_TOKEN_8877", "失败文案不得回显 token 明文")
}

// --- kimi：HTTP 200 + trailer unauthenticated → 认证错误口径 ---

func TestWebProbe_KimiTrailerUnauthenticatedReportsAuthError(t *testing.T) {
	svc, _ := webProbeService(webProbeResponse(http.StatusOK, string(webKimiEnvelopeTrailerStream(
		`{"error":{"code":"unauthenticated","message":"access token expired"}}`,
	))))
	account := &Account{
		ID: 8108, Platform: PlatformKimi, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "access_token": "SUPER_SECRET_TOKEN_8877"},
	}
	ctx, rec := newWebTestContext()

	err := svc.testWebAccountConnection(ctx, account, "", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unauthenticated", "nested unauthenticated must be classified as an auth error")

	body := rec.Body.String()
	require.NotContains(t, body, "Web login session is healthy.")
	require.NotContains(t, body, `"success":true`)
	require.NotContains(t, body, "access token expired", "失败文案不得回显上游原文")
}

// --- kimi：HTTP 200 + 非 0 业务码帧 → 失败（业务错误口径） ---

func TestWebProbe_KimiBusinessErrorCodeFailsClosed(t *testing.T) {
	svc, _ := webProbeService(webProbeResponse(http.StatusOK, string(webKimiEnvelopeStream(
		`{"eventOffset":1,"chat":{"id":"chat-probe-2"}}`,
		`{"eventOffset":2,"code":16,"message":"kimi quota exceeded"}`,
	))))
	account := &Account{
		ID: 8106, Platform: PlatformKimi, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "access_token": "kimi-access-token"},
	}
	ctx, rec := newWebTestContext()

	err := svc.testWebAccountConnection(ctx, account, "", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "business error")

	body := rec.Body.String()
	require.NotContains(t, body, `"success":true`)
	require.NotContains(t, body, "quota exceeded", "失败文案不得回显上游原文")
}

// --- kimi：HTTP 200 + 裸 JSON（非 envelope）携带会话/正文 → 健康 ---
//
// 裸 JSON 分支（异常形态）必须与 envelope 帧分支同口径：不得因为「不是二进制帧」就无条件
// 报空流，只要载荷里有 chat.id / 正文 / done 等有效信号即判健康。
func TestWebProbe_KimiBareJSONWithChatIDReportsHealthy(t *testing.T) {
	svc, _ := webProbeService(webProbeResponse(http.StatusOK,
		`{"chat":{"id":"chat-bare-1"},"message":{"id":"msg-bare-1","role":"assistant"}}`))
	account := &Account{
		ID: 8109, Platform: PlatformKimi, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "access_token": "kimi-access-token"},
	}
	ctx, rec := newWebTestContext()

	require.NoError(t, svc.testWebAccountConnection(ctx, account, "", ""))
	require.Contains(t, rec.Body.String(), "Web login session is healthy.")
	require.Contains(t, rec.Body.String(), `"success":true`)
}

// --- zhipu：空返回 → 失败；有效帧 → 健康 ---

func TestWebProbe_ZhipuEmptyBodyFailsClosed(t *testing.T) {
	svc, _ := webProbeService(webProbeResponse(http.StatusOK, ""))
	account := &Account{
		ID: 8201, Platform: PlatformZhipu, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "chatglm_token=SUPER_SECRET_JWT"},
	}
	ctx, rec := newWebTestContext()

	err := svc.testWebAccountConnection(ctx, account, "", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty response")

	body := rec.Body.String()
	require.Contains(t, body, webZhipuStreamPath)
	require.NotContains(t, body, "Web login session is healthy.")
	require.NotContains(t, body, `"success":true`)
	require.NotContains(t, body, "SUPER_SECRET_JWT", "失败文案不得回显凭证")
}

func TestWebProbe_ZhipuValidFramesReportHealthy(t *testing.T) {
	svc, _ := webProbeService(okWebZhipuProbeResponse())
	account := &Account{
		ID: 8202, Platform: PlatformZhipu, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "chatglm_token=SUPER_SECRET_JWT"},
	}
	ctx, rec := newWebTestContext()

	require.NoError(t, svc.testWebAccountConnection(ctx, account, "", ""))

	body := rec.Body.String()
	require.Contains(t, body, "Web login session is healthy.")
	require.Contains(t, body, `"success":true`)
	require.NotContains(t, body, "SUPER_SECRET_JWT")
}

// zhipu：HTTP 200 + 非 0 code 帧 → 失败（业务错误口径）。
func TestWebProbe_ZhipuBusinessErrorCodeFailsClosed(t *testing.T) {
	svc, _ := webProbeService(webProbeResponse(http.StatusOK, `data: {"code":40011,"msg":"authentication required"}`+"\n\n"))
	account := &Account{
		ID: 8203, Platform: PlatformZhipu, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "chatglm_token=SUPER_SECRET_JWT"},
	}
	ctx, rec := newWebTestContext()

	err := svc.testWebAccountConnection(ctx, account, "", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "business error")
	require.NotContains(t, rec.Body.String(), `"success":true`)
	require.NotContains(t, rec.Body.String(), "SUPER_SECRET_JWT")
}

// --- deepseek：空返回 → 失败；实测流 → 健康 ---

func newDeepseekProbeService(completion *http.Response) (*AccountTestService, *webProbeUpstream) {
	upstream := &webProbeUpstream{respSeq: []*http.Response{
		webDeepseekSolvablePowChallengeResponse(),
		webDeepseekSessionCreateResponse(),
		completion,
	}}
	return newWebTestService(upstream, completion), upstream
}

func TestWebProbe_DeepseekEmptyBodyFailsClosed(t *testing.T) {
	svc, _ := newDeepseekProbeService(webProbeResponse(http.StatusOK, ""))
	account := &Account{
		ID: 8301, Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "ds_session_id=SUPER_SECRET_SESSION"},
	}
	ctx, rec := newWebTestContext()

	err := svc.testWebAccountConnection(ctx, account, "", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty response")

	body := rec.Body.String()
	require.Contains(t, body, webDeepseekChatCompletionPath)
	require.NotContains(t, body, "Web login session is healthy.")
	require.NotContains(t, body, `"success":true`)
	require.NotContains(t, body, "SUPER_SECRET_SESSION", "失败文案不得回显凭证")
}

func TestWebProbe_DeepseekValidFramesReportHealthy(t *testing.T) {
	// 复用登录态实测 SSE 全文（09 §5），确保真实流不被新口径误杀。
	svc, _ := newDeepseekProbeService(webDeepseekFixtureSSEResponse())
	account := &Account{
		ID: 8302, Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "ds_session_id=SUPER_SECRET_SESSION"},
	}
	ctx, rec := newWebTestContext()

	require.NoError(t, svc.testWebAccountConnection(ctx, account, "", ""))

	body := rec.Body.String()
	require.Contains(t, body, "Web login session is healthy.")
	require.Contains(t, body, `"success":true`)
	require.NotContains(t, body, "SUPER_SECRET_SESSION")
}

// deepseek：HTTP 200 + data.biz_code 非 0 → 失败（双路径业务错误口径）。
func TestWebProbe_DeepseekBusinessErrorCodeFailsClosed(t *testing.T) {
	svc, _ := newDeepseekProbeService(webProbeResponse(http.StatusOK,
		`data: {"code":0,"data":{"biz_code":40003,"biz_msg":"login required"}}`+"\n\n"))
	account := &Account{
		ID: 8303, Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "ds_session_id=SUPER_SECRET_SESSION"},
	}
	ctx, rec := newWebTestContext()

	err := svc.testWebAccountConnection(ctx, account, "", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "business error")
	require.NotContains(t, rec.Body.String(), `"success":true`)
	require.NotContains(t, rec.Body.String(), "SUPER_SECRET_SESSION")
}

// --- 401 / 403：既有分支行为不变（仍走重试/拒绝路径，不受内容判定影响） ---

func TestWebProbe_401And403KeepExistingBehavior(t *testing.T) {
	cases := []struct {
		name       string
		platform   string
		statusCode int
		credential map[string]any
		errText    string
	}{
		{
			name: "kimi-401", platform: PlatformKimi, statusCode: http.StatusUnauthorized,
			credential: map[string]any{"access_mode": AccountAccessModeWeb, "access_token": "expired-access-token"},
			errText:    "web login credential is invalid (HTTP 401)",
		},
		{
			name: "kimi-403", platform: PlatformKimi, statusCode: http.StatusForbidden,
			credential: map[string]any{"access_mode": AccountAccessModeWeb, "access_token": "expired-access-token"},
			errText:    "web login credential is invalid (HTTP 403)",
		},
		{
			name: "zhipu-401", platform: PlatformZhipu, statusCode: http.StatusUnauthorized,
			credential: map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "chatglm_token=OLD-JWT"},
			errText:    "web probe rejected by upstream",
		},
		{
			name: "zhipu-403", platform: PlatformZhipu, statusCode: http.StatusForbidden,
			credential: map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "chatglm_token=OLD-JWT"},
			errText:    "web probe rejected by upstream",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{
				ID: 8400, Platform: tc.platform, Type: AccountTypeAPIKey, Concurrency: 1,
				Credentials: tc.credential,
			}
			svc, recorder := newWebRefreshProbeTestService(&kimiRefreshRepoStub{})
			// 无 refresh_token：401 续期必然失败，直接落入既有文案分支。
			recorder.responses = []*http.Response{webProbeResponse(tc.statusCode, "unauthorized")}
			c, rec := newWebRefreshTestContext()

			err := svc.testWebAccountConnection(c, account, "", "")
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.errText)
			require.Len(t, recorder.requests, 1, "401/403 不得因内容判定额外出站")
			require.Contains(t, rec.Body.String(), tc.errText)
			require.NotContains(t, rec.Body.String(), `"success":true`)
			require.NotContains(t, rec.Body.String(), "Web login session is healthy.")
		})
	}
}

// 401 → 续期重试后返回空流：续期成功也不得判健康（重试路径与主路径同口径）。
func TestWebProbe_KimiRetryEmptyBodyStillFailsClosed(t *testing.T) {
	account := &Account{
		ID:          8501,
		Platform:    PlatformKimi,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_mode":   AccountAccessModeWeb,
			"access_token":  "expired-access-token",
			"refresh_token": "valid-refresh-token",
		},
	}
	svc, recorder := newWebRefreshProbeTestService(&kimiRefreshRepoStub{})
	recorder.responses = []*http.Response{
		unauthorizedTextResponse(),
		refreshResponse(http.StatusOK, `{"accessToken":"NEW-ACCESS-TOKEN"}`),
		webProbeResponse(http.StatusOK, ""), // 续期后重试：200 但空流
	}
	c, rec := newWebRefreshTestContext()

	err := svc.testWebAccountConnection(c, account, "", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty response")

	body := rec.Body.String()
	require.NotContains(t, body, "Web login session is healthy.")
	require.NotContains(t, body, `"success":true`)
	require.NotContains(t, body, "NEW-ACCESS-TOKEN", "失败文案不得回显新 token")
}
