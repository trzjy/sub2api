package service

// web 逆向平台（官方 zhipu / deepseek / kimi + access_mode=web）账号测试链回归测试。
//
// 这些测试不依赖 //go:build unit 下的共享桩，自成一体，确保默认 `go test` 即可运行
// （验证命令不含 -tags unit）。重点覆盖：
//   - zhipu web 探活复用正式转发链构造函数（buildWebZhipuUpstreamRequest），头一致；
//   - modelID 线程化：选中模型 → 实际出站模型，且选择 glm-4.7-flash 不再固定 glm-4；
//   - 空 modelID 回落默认模型（DefaultWebModelIDs），非空 model_mapping 生效；
//   - test_start 事件 Model == 实际出站模型；
//   - 401/403 不泄露 Cookie，且不再断言凭证失效；
//   - deepseek / kimi 探活不回归。

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// makeFakeWebZhipuJWTProbe 手工拼假 chatglm_token JWT（仅测试用，不携带真实凭证）。
func makeFakeWebZhipuJWTProbe(t *testing.T, payload map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	return header + "." + base64.RawURLEncoding.EncodeToString(raw) + ".fakesig"
}

// webProbeUpstream 记录唯一出站请求并回放注入响应（不发起真实网络）。
type webProbeUpstream struct {
	lastReq *http.Request
	resp    *http.Response
	// respSeq 非空时按序出队（Do 与 DoWithTLS 共用同一队列），耗尽后回落单响应。
	respSeq []*http.Response
}

func (u *webProbeUpstream) next() *http.Response {
	if len(u.respSeq) > 0 {
		r := u.respSeq[0]
		u.respSeq = u.respSeq[1:]
		return r
	}
	return u.resp
}

func (u *webProbeUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.next(), nil
}

func (u *webProbeUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	u.lastReq = req
	return u.next(), nil
}

func newWebTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/1/test", nil)
	return c, rec
}

// parseTestStartModel 从 SSE 输出中解析首个 test_start 事件的 model 字段。
func parseTestStartModel(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if gjson.Get(payload, "type").String() == "test_start" {
			return gjson.Get(payload, "model").String()
		}
	}
	return ""
}

// newWebTestService 组装一个最小可用的 AccountTestService 实例（web 探活只需要
// httpUpstream / tlsFPProfileService / openaiGatewayService 三项）。
func newWebTestService(upstream *webProbeUpstream, resp *http.Response) *AccountTestService {
	upstream.resp = resp
	gw := &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg: &config.Config{
			Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true}},
		},
	}
	return &AccountTestService{
		httpUpstream:         upstream,
		tlsFPProfileService:  &TLSFingerprintProfileService{},
		openaiGatewayService: gw,
	}
}

func okSSEResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(`data: {"content":"hi"}` + "\n\n")),
	}
}

// TestWebZhipuTestRespectsSelectedModel 选中某模型时，出站请求体 meta_data.selected_model
// 必须是该模型（2026-09-17 实测协议：/assistant/stream 无 model 字段，模型选择走
// selected_model + assistant_id 双载体）；测试链与正式转发链一致包含关键请求头。
func TestWebZhipuTestRespectsSelectedModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, rec := newWebTestContext()
	upstream := &webProbeUpstream{}
	svc := newWebTestService(upstream, okWebZhipuProbeResponse())
	account := &Account{
		ID:          1,
		Platform:    PlatformZhipu,
		Concurrency: 1,
		Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "SECRET_COOKIE=abc"},
	}

	err := svc.testWebAccountConnection(ctx, account, "glm-5.3-flash", "")
	require.NoError(t, err)
	require.NotNil(t, upstream.lastReq, "zhipu web probe must issue an upstream request")

	body, rErr := io.ReadAll(upstream.lastReq.Body)
	require.NoError(t, rErr)
	// 旧协议 model 字段已随 /v1/conversation 端点删除，不得再出现。
	require.False(t, gjson.GetBytes(body, "model").Exists(), "outbound body must not carry legacy model field")
	outboundModel := gjson.GetBytes(body, "meta_data.selected_model").String()
	require.Equal(t, "glm-5.3-flash", outboundModel, "selected model must be the outbound selected_model")

	// 实测协议请求体关键载体：assistant_id（实测 GLM-Flash 助手值）+ chat_type。
	require.Equal(t, webZhipuDefaultAssistantID, gjson.GetBytes(body, "assistant_id").String())
	require.Equal(t, "user_chat", gjson.GetBytes(body, "chat_type").String())

	// 与正式转发链一致的关键请求头（2026-09-17 实测认证/指纹头）。
	require.NotEmpty(t, upstream.lastReq.Header.Get("Origin"))
	require.NotEmpty(t, upstream.lastReq.Header.Get("Referer"))
	require.NotEmpty(t, upstream.lastReq.Header.Get("User-Agent"))
	require.Equal(t, "chatglm", upstream.lastReq.Header.Get("app-name"))
	require.Equal(t, "pc", upstream.lastReq.Header.Get("X-App-Platform"))
	require.Contains(t, upstream.lastReq.Header.Get("Cookie"), "SECRET_COOKIE=abc")

	// test_start 显示模型 == 实际出站模型。
	require.Equal(t, "glm-5.3-flash", parseTestStartModel(rec.Body.String()))
}

// TestWebZhipuTestAuthorizationBearerFromCookie Authorization Bearer 双载体：chatglm_token
// 从 Cookie 串自动提取为 Bearer 凭证（2026-09-17 实测认证形态），x-device-id 从 JWT 解出。
func TestWebZhipuTestAuthorizationBearerFromCookie(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newWebTestContext()
	upstream := &webProbeUpstream{}
	svc := newWebTestService(upstream, okWebZhipuProbeResponse())
	// JWT payload: {"device_id":"dev123","typ":"access"}（base64url）
	jwt := "header.eyJkZXZpY2VfaWQiOiJkZXYxMjMiLCJ0eXAiOiJhY2Nlc3MifQ.sig"
	account := &Account{
		ID:          4,
		Platform:    PlatformZhipu,
		Concurrency: 1,
		Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "chatglm_token=" + jwt + "; acw_tc=x"},
	}

	err := svc.testWebAccountConnection(ctx, account, "", "")
	require.NoError(t, err)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, "Bearer "+jwt, upstream.lastReq.Header.Get("Authorization"))
	require.Equal(t, "dev123", upstream.lastReq.Header.Get("X-Device-Id"))
}

// TestWebZhipuTestEmptyModelUsesDefaultAndMappingApplies 空 modelID 才回落默认模型；
// 非空 model_mapping 生效，且 test_start 显示模型 == 实际出站模型。
func TestWebZhipuTestEmptyModelUsesDefaultAndMappingApplies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, rec := newWebTestContext()
	upstream := &webProbeUpstream{}
	svc := newWebTestService(upstream, okWebZhipuProbeResponse())
	account := &Account{
		ID:          3,
		Platform:    PlatformZhipu,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_mode":   AccountAccessModeWeb,
			"cookie":        "C=1",
			"model_mapping": map[string]any{"glm-5.3-flash": "glm-4-flash"},
		},
	}

	err := svc.testWebAccountConnection(ctx, account, "", "")
	require.NoError(t, err)
	require.NotNil(t, upstream.lastReq)

	body, rErr := io.ReadAll(upstream.lastReq.Body)
	require.NoError(t, rErr)
	// 空 modelID → 默认 glm-5.3-flash；命中 model_mapping → 出站 glm-4-flash。
	require.Equal(t, "glm-4-flash", gjson.GetBytes(body, "meta_data.selected_model").String())
	require.Equal(t, "glm-4-flash", parseTestStartModel(rec.Body.String()))
}

// TestWebZhipuProbe401DoesNotAssertCredentialAndLeaksNoCookie 401/403 不再断言凭证失效，
// 错误文案保留平台/端点/状态码，且不泄露 Cookie。
func TestWebZhipuProbe401DoesNotAssertCredentialAndLeaksNoCookie(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, rec := newWebTestContext()
	upstream := &webProbeUpstream{}
	svc := newWebTestService(upstream, &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("You need to be authenticated")),
	})
	account := &Account{
		ID:          2,
		Platform:    PlatformZhipu,
		Concurrency: 1,
		Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "SUPER_SECRET=xyz"},
	}

	err := svc.testWebAccountConnection(ctx, account, "", "")
	require.Error(t, err)
	body := rec.Body.String()
	require.Contains(t, body, "web probe rejected by upstream")
	require.Contains(t, body, "HTTP 401")
	require.NotContains(t, body, "SUPER_SECRET=xyz", "401 response must not leak the cookie")
	require.NotContains(t, body, "credential is invalid", "must not assert credential invalidity on 401")
}

// TestDefaultWebModelIDsZhipuDefaultIsFirstEntry 确认 zhipu 默认 web 模型目录首项是
// DefaultWebModelIDs 提供的值（空 modelID 回落依据；2026-09-17 实测目录）。
func TestDefaultWebModelIDsZhipuDefaultIsFirstEntry(t *testing.T) {
	ids := DefaultWebModelIDs(PlatformZhipu, AccountAccessModeWeb)
	require.NotEmpty(t, ids)
	require.Equal(t, "glm-5.3-flash", ids[0])
}

// TestWebAccountConnection_DeepSeekProbeNoRegression deepseek 探活不回归：空 modelID 回落
// 既有默认（deepseek-chat → model_type "default"，2026-09-18 登录态实测），出站 model_type
// 正确，测试成功。
func TestWebAccountConnection_DeepSeekProbeNoRegression(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newWebTestContext()
	// 新协议探活走完整三跳：PoW 挑战 → 建会话 → completion（09 §1/§3 登录态强制）。
	upstream := &webProbeUpstream{respSeq: []*http.Response{
		webDeepseekSolvablePowChallengeResponse(),
		webDeepseekSessionCreateResponse(),
		nil, // completion 响应稍后注入（newWebTestService 设置 upstream.resp）
	}}
	svc := newWebTestService(upstream, okWebDeepseekProbeResponse())
	upstream.respSeq[2] = upstream.resp
	account := &Account{
		ID:          11,
		Platform:    PlatformDeepseek,
		Concurrency: 1,
		Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "D=1"},
	}

	err := svc.testWebAccountConnection(ctx, account, "", "")
	require.NoError(t, err)
	require.NotNil(t, upstream.lastReq)
	// 探活与正式转发同链：completion 出站必须携带 PoW 头（09 §3）。
	require.NotEmpty(t, upstream.lastReq.Header.Get("X-Ds-PoW-Response"), "probe completion request must carry the PoW header")
	body, rErr := io.ReadAll(upstream.lastReq.Body)
	require.NoError(t, rErr)
	// 默认 deepseek-chat → model_type "default"（2026-09-18 登录态实测，09 §4）。
	require.Equal(t, "default", gjson.GetBytes(body, "model_type").String())
	require.False(t, gjson.GetBytes(body, "thinking_enabled").Bool())
	// 自动建会话：出站体携带非空 chat_session_id（探活不再发无会话请求）。
	require.Equal(t, "sess-new-123", gjson.GetBytes(body, "chat_session_id").String())
}

// TestWebAccountConnection_DeepSeekRespectsModelIDAndMapping deepseek 尊重非空
// modelID + model_mapping（reasoner → thinking_enabled=true）。
func TestWebAccountConnection_DeepSeekRespectsModelIDAndMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newWebTestContext()
	upstream := &webProbeUpstream{respSeq: []*http.Response{
		webDeepseekSolvablePowChallengeResponse(),
		webDeepseekSessionCreateResponse(),
		nil,
	}}
	svc := newWebTestService(upstream, okWebDeepseekProbeResponse())
	upstream.respSeq[2] = upstream.resp
	account := &Account{
		ID:          12,
		Platform:    PlatformDeepseek,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_mode":   AccountAccessModeWeb,
			"cookie":        "D=1",
			"model_mapping": map[string]any{"my-ds": "deepseek-reasoner"},
		},
	}

	err := svc.testWebAccountConnection(ctx, account, "my-ds", "")
	require.NoError(t, err)
	require.NotNil(t, upstream.lastReq)
	body, rErr := io.ReadAll(upstream.lastReq.Body)
	require.NoError(t, rErr)
	require.Equal(t, "default", gjson.GetBytes(body, "model_type").String())
	require.True(t, gjson.GetBytes(body, "thinking_enabled").Bool())
}

// TestWebAccountConnection_KimiProbeNoRegression kimi 探活不回归：空 modelID 回落既有默认。
func TestWebAccountConnection_KimiProbeNoRegression(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newWebTestContext()
	upstream := &webProbeUpstream{}
	svc := newWebTestService(upstream, okWebKimiProbeResponse())
	account := &Account{
		ID:          21,
		Platform:    PlatformKimi,
		Concurrency: 1,
		Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "access_token": "AT=1"},
	}

	err := svc.testWebAccountConnection(ctx, account, "", "")
	require.NoError(t, err)
	require.NotNil(t, upstream.lastReq)
	// 探活与正式转发同链：Connect RPC 头族与 forwardWebKimi 完全一致（10 §2 实测）。
	require.Equal(t, "application/connect+json", upstream.lastReq.Header.Get("Content-Type"))
	body, rErr := io.ReadAll(upstream.lastReq.Body)
	require.NoError(t, rErr)
	require.Equal(t, "k3", gjson.GetBytes(body, "options.model").String())
}

// TestWebAccountConnection_KimiRespectsModelID kimi 尊重非空 modelID + model_mapping。
func TestWebAccountConnection_KimiRespectsModelID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newWebTestContext()
	upstream := &webProbeUpstream{}
	svc := newWebTestService(upstream, okWebKimiProbeResponse())
	account := &Account{
		ID:          22,
		Platform:    PlatformKimi,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_mode":   AccountAccessModeWeb,
			"access_token":  "AT=1",
			"model_mapping": map[string]any{"kimi-k3": "k3-agent-ultra"},
		},
	}

	err := svc.testWebAccountConnection(ctx, account, "kimi-k3", "")
	require.NoError(t, err)
	require.NotNil(t, upstream.lastReq)
	body, rErr := io.ReadAll(upstream.lastReq.Body)
	require.NoError(t, rErr)
	require.Equal(t, "k3-agent-ultra", gjson.GetBytes(body, "options.model").String())
}

// TestWebZhipuProbeGuestTokenFailsClosed 探活与正式转发共用同一登录态判定：游客态 token
// （is_guest=true）必须在发出任何上游请求前失败关闭，错误文案含 "guest"，且不得泄露 JWT
// 内容（安全红线）。upstream.lastReq 必须保持 nil（0 次上游请求）。
func TestWebZhipuProbeGuestTokenFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, rec := newWebTestContext()
	upstream := &webProbeUpstream{}
	svc := newWebTestService(upstream, okSSEResponse())
	guestJWT := makeFakeWebZhipuJWTProbe(t, map[string]any{"is_guest": true, "device_id": "probe-guest-id"})
	account := &Account{
		ID:          5,
		Platform:    PlatformZhipu,
		Concurrency: 1,
		Credentials: map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "chatglm_token=" + guestJWT + "; acw_tc=x"},
	}

	err := svc.testWebAccountConnection(ctx, account, "", "")
	require.Error(t, err)
	require.Nil(t, upstream.lastReq, "guest token probe must not issue any upstream request")
	body := rec.Body.String()
	require.Contains(t, body, "guest", "guest failure must be diagnosable from the probe output")
	require.NotContains(t, body, guestJWT, "probe output must not leak the guest token")
	require.NotContains(t, body, "probe-guest-id", "probe output must not leak token payload")
}
