package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// --- D2: 本地 token 估算（网页逆向平台上游不返回 usage 时的兜底） ---

func TestEstimateWebUsage(t *testing.T) {
	// 中文按字符计，英文连续词按空白计。
	got := estimateWebUsage("你好world foo", "hi 世界")
	require.Greater(t, got.InputTokens, 0)
	require.Greater(t, got.OutputTokens, 0)
	// 空输入不应 panic 且为 0。
	empty := estimateWebUsage("", "")
	require.Equal(t, 0, empty.InputTokens)
	require.Equal(t, 0, empty.OutputTokens)
}

func TestEstimateTokenCountMixedScript(t *testing.T) {
	// 4+2 个 CJK 字符 + 2 个英文词（"hello"/"world"）→ 8。
	require.Equal(t, 8, estimateTokenCount("你好世界hello世界world"))
	require.Equal(t, 0, estimateTokenCount("   "))
}

// --- C6: 网页逆向平台默认模型目录（共享常量表） ---

func TestDefaultWebModelIDs(t *testing.T) {
	require.Equal(t, []string{"deepseek-chat", "deepseek-reasoner"}, DefaultWebModelIDs(PlatformWebDeepseek))
	require.Equal(t, []string{"kimi-k3"}, DefaultWebModelIDs(PlatformWebKimi))
	require.Nil(t, DefaultWebModelIDs(PlatformOpenAI))

	// PlatformWebZhipu 默认目录（2026-09-17 官网登录态抓包实测：请求体
	// meta_data.selected_model 原值 glm-5.3-flash）。本测试仅为当前实现的证据，
	// 不是模型目录的权威来源；防误回落到 Claude 默认模型的保护必须保留。
	zhipu := DefaultWebModelIDs(PlatformWebZhipu)
	require.NotEmpty(t, zhipu, "web-zhipu 应有非空默认目录，而非回落为 nil")
	require.Equal(t, []string{"glm-5.3-flash"}, zhipu, "实测目录：glm-5.3-flash")
	for _, m := range zhipu {
		require.NotContains(t, []string{"claude-3-5-sonnet", "claude-3-7-sonnet", "claude-sonnet-4"}, m,
			"web-zhipu 目录不得误回落到 Claude 默认模型")
	}
}

// --- C2: Responses 请求归一为 chat.completions，再进入 web 适配器 ---

func TestNormalizeWebRequestBodyForAdapter(t *testing.T) {
	// 仅当存在 input 字段（Responses 协议）时才归一；否则原样返回。
	chatBody := []byte(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`)
	out, err := normalizeWebRequestBodyForAdapter(chatBody)
	require.NoError(t, err)
	require.JSONEq(t, string(chatBody), string(out), "chat completions 请求应原样透传")

	// Responses 请求（input 字段）→ 归一为 chat.completions（含 messages）。
	responsesBody := []byte(`{"model":"gpt-4o","instructions":"be nice","input":[{"type":"text","text":"hello there"}]}`)
	out, err = normalizeWebRequestBodyForAdapter(responsesBody)
	require.NoError(t, err)
	require.Contains(t, string(out), `"messages"`)
	require.Contains(t, string(out), "hello there")
	require.NotContains(t, string(out), `"input"`)
}

// --- D1: web-kimi 刷新成功后把新 token 持久化到账号凭据 ---

type kimiRefreshRepoStub struct {
	AccountRepository // 嵌入接口：其余方法提升为 nil（本测试不调用）
	updatedID         int64
	updatedCreds      map[string]any
}

func (r *kimiRefreshRepoStub) UpdateCredentials(_ context.Context, id int64, creds map[string]any) error {
	r.updatedID = id
	r.updatedCreds = creds
	return nil
}

func refreshResponse(code int, body string) *http.Response {
	return &http.Response{
		StatusCode: code,
		Body:       io.NopCloser(bytes.NewReader([]byte(body))),
		Header:     http.Header{},
	}
}

func TestRefreshWebKimiAccessTokenPersistsCredentials(t *testing.T) {
	account := &Account{
		ID:       9901,
		Platform: PlatformWebKimi,
		Credentials: map[string]any{
			"refresh_token": "old-refresh",
			"access_token":  "old-access",
		},
	}
	repo := &kimiRefreshRepoStub{}
	svc := &OpenAIGatewayService{
		accountRepo: repo,
		httpUpstream: &httpUpstreamRecorder{resp: refreshResponse(http.StatusOK,
			`{"accessToken":"new-access-token","refreshToken":"new-refresh-token"}`)},
	}

	got := svc.refreshWebKimiAccessToken(context.Background(), account, "")
	require.Equal(t, "new-access-token", got, "应返回刷新后的 access_token")
	require.Equal(t, int64(9901), repo.updatedID)
	require.Equal(t, "new-access-token", repo.updatedCreds["access_token"])
	require.Equal(t, "new-refresh-token", repo.updatedCreds["refresh_token"], "轮换的 refresh_token 也应持久化")
}

func TestRefreshWebKimiAccessTokenNoPersistOnFailure(t *testing.T) {
	account := &Account{
		ID:       9902,
		Platform: PlatformWebKimi,
		Credentials: map[string]any{
			"refresh_token": "old-refresh",
		},
	}
	repo := &kimiRefreshRepoStub{}
	// 上游 401（Session expired）→ 刷新失败，不持久化。
	svc := &OpenAIGatewayService{
		accountRepo:  repo,
		httpUpstream: &httpUpstreamRecorder{resp: refreshResponse(http.StatusUnauthorized, `{"code":"unauthenticated"}`)},
	}
	got := svc.refreshWebKimiAccessToken(context.Background(), account, "")
	require.Equal(t, "", got)
	require.Equal(t, int64(0), repo.updatedID, "刷新失败不应写库")
}

// --- C5: web 平台走轻量官方探活（2xx 有效 / 401 失效） ---

func newWebProbeTestService(resp *http.Response) *AccountTestService {
	// deepseek 探活与正式转发同链（PoW + 自动建会话），探活服务与网关共用同一 recorder。
	upstream := &httpUpstreamRecorder{resp: resp}
	cfg := &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true},
		},
	}
	return &AccountTestService{
		httpUpstream:         upstream,
		cfg:                  cfg,
		tlsFPProfileService:  &TLSFingerprintProfileService{},
		openaiGatewayService: &OpenAIGatewayService{httpUpstream: upstream, cfg: cfg},
	}
}

// runWebProbeWithResponses 按序回放多跳响应（deepseek 探活新协议：PoW → 建会话 → completion）。
func runWebProbeWithResponses(t *testing.T, account *Account, responses ...*http.Response) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	svc := newWebProbeTestService(nil)
	svc.httpUpstream.(*httpUpstreamRecorder).responses = responses
	_ = svc.testWebAccountConnection(c, account, "", "")
	return rec.Body.String()
}

func runWebProbe(t *testing.T, account *Account, resp *http.Response) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	svc := newWebProbeTestService(resp)
	_ = svc.testWebAccountConnection(c, account, "", "")
	return rec.Body.String()
}

func TestTestWebAccountConnection_WebDeepseekProbeSuccess(t *testing.T) {
	account := &Account{
		ID:       9911,
		Platform: PlatformWebDeepseek,
		Credentials: map[string]any{
			"cookie": "ds_session_id=sess; HWWAFSESID=waf",
		},
	}
	body := runWebProbeWithResponses(t, account,
		webDeepseekSolvablePowChallengeResponse(),
		webDeepseekSessionCreateResponse(),
		refreshResponse(http.StatusOK, "ok"),
	)
	require.Contains(t, body, "test_complete")
	require.Contains(t, body, `"success":true`)
	require.NotContains(t, body, "invalid")
}

func TestTestWebAccountConnection_WebDeepseekProbeInvalidCredential(t *testing.T) {
	account := &Account{
		ID:       9912,
		Platform: PlatformWebDeepseek,
		Credentials: map[string]any{
			"cookie": "ds_session_id=sess; HWWAFSESID=waf",
		},
	}
	body := runWebProbeWithResponses(t, account,
		webDeepseekSolvablePowChallengeResponse(),
		webDeepseekSessionCreateResponse(),
		refreshResponse(http.StatusUnauthorized, `{"code":40002}`),
	)
	require.Contains(t, body, "error")
	require.Contains(t, body, "invalid", "401 应判为凭证失效")
	require.NotContains(t, body, "sess", "错误文案不得回显 Cookie 凭证")
}

func TestTestWebAccountConnection_WebKimiProbeSuccess(t *testing.T) {
	account := &Account{
		ID:       9913,
		Platform: PlatformWebKimi,
		Credentials: map[string]any{
			"access_token": "kimi-tok",
		},
	}
	body := runWebProbe(t, account, refreshResponse(http.StatusOK, "ok"))
	require.Contains(t, body, "test_complete")
	require.Contains(t, body, `"success":true`)
}
