package service

// 火山订阅号专用端点在真实网关与账号测试中的落地回归：真实转发与账号测试必须在
// 命中火山 base（host ark.cn-beijing.volces.com + /api/plan|/api/coding）时派生
// {base}/v3/chat/completions 与 {base}/v3/responses，绝不回落 /v1/chat/completions
// 或 /responses（外审-2 火山专用路由一致性）。

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func volcanoRouteTestService() *OpenAIGatewayService {
	return &OpenAIGatewayService{cfg: &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{Enabled: false},
		},
	}}
}

// 精确首段识别：/api/plan-evil /api/coding-v2 等相似子串不得命中。
func TestVolcanoPlanProfileExactPathSegments(t *testing.T) {
	t.Parallel()

	cases := []struct {
		url  string
		ok   bool
		kind string
	}{
		{"https://ark.cn-beijing.volces.com/api/plan", true, volcanoPlanKindAgent},
		{"https://ark.cn-beijing.volces.com/api/plan/v1/messages", true, volcanoPlanKindAgent},
		{"https://ark.cn-beijing.volces.com/api/plan/v3/chat/completions", true, volcanoPlanKindAgent},
		{"https://ark.cn-beijing.volces.com/api/coding", true, volcanoPlanKindCoding},
		{"https://ark.cn-beijing.volces.com/api/coding/v3/responses", true, volcanoPlanKindCoding},
		{"https://ark.cn-beijing.volces.com/api/plan-evil", false, ""},
		{"https://ark.cn-beijing.volces.com/api/plan-v2", false, ""},
		{"https://ark.cn-beijing.volces.com/api/codingv2", false, ""},
		{"https://api.deepseek.com", false, ""},
		{"https://ark.cn-beijing.volces.com/other", false, ""},
	}
	for _, tc := range cases {
		profile, ok := parseVolcanoPlanProfile(tc.url)
		require.Equal(t, tc.ok, ok, "base_url=%s", tc.url)
		if tc.ok {
			require.Equal(t, tc.kind, profile.Kind, "base_url=%s", tc.url)
		}
	}
}

// OpenAI 端点派生 helper：火山→/v3，普通 deepseek 无回归。
func TestVolcanoOpenAIEndpointURLAux(t *testing.T) {
	t.Parallel()

	agentPlan := "https://ark.cn-beijing.volces.com/api/plan"
	codingPlan := "https://ark.cn-beijing.volces.com/api/coding"

	require.Equal(t, agentPlan+"/v3/chat/completions", openAIChatCompletionsURLForBase(agentPlan))
	require.Equal(t, codingPlan+"/v3/chat/completions", openAIChatCompletionsURLForBase(codingPlan))
	require.Equal(t, "https://api.deepseek.com/v1/chat/completions", openAIChatCompletionsURLForBase("https://api.deepseek.com"))

	require.Equal(t, agentPlan+"/v3/responses", openAIResponsesURLForBase(PlatformDeepseek, agentPlan))
	require.Equal(t, codingPlan+"/v3/responses", openAIResponsesURLForBase(PlatformDeepseek, codingPlan))
	// deepseek 官方 Responses 无 /v1 前缀，保持既有行为。
	require.Equal(t, "https://api.deepseek.com/responses", openAIResponsesURLForBase(PlatformDeepseek, "https://api.deepseek.com"))
}

// 真实 CC 转发端点：openAIChatCompletionsTargetURL 命中火山→/v3/chat/completions。
func TestVolcanoOpenAIChatCompletionsTargetURL(t *testing.T) {
	t.Parallel()

	svc := volcanoRouteTestService()
	for _, tc := range []struct {
		base string
		want string
	}{
		{"https://ark.cn-beijing.volces.com/api/coding", "https://ark.cn-beijing.volces.com/api/coding/v3/chat/completions"},
		{"https://ark.cn-beijing.volces.com/api/plan", "https://ark.cn-beijing.volces.com/api/plan/v3/chat/completions"},
		{"https://api.deepseek.com", "https://api.deepseek.com/v1/chat/completions"},
	} {
		acc := &Account{Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": tc.base}}
		got, err := svc.openAIChatCompletionsTargetURL(acc)
		require.NoError(t, err)
		require.Equal(t, tc.want, got, "base_url=%s", tc.base)
	}
}

// 真实 Responses 转发：buildUpstreamRequest 命中火山→/v3/responses（不含 adaptive 回落）。
func TestVolcanoBuildUpstreamRequestResponsesURL(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	svc := volcanoRouteTestService()
	for _, tc := range []struct {
		base string
		want string
	}{
		{"https://ark.cn-beijing.volces.com/api/coding", "https://ark.cn-beijing.volces.com/api/coding/v3/responses"},
		{"https://ark.cn-beijing.volces.com/api/plan", "https://ark.cn-beijing.volces.com/api/plan/v3/responses"},
	} {
		acc := &Account{Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": tc.base}}
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte(`{"model":"gpt-5"}`)))
		req, err := svc.buildUpstreamRequest(c.Request.Context(), c, acc, []byte(`{"model":"gpt-5"}`), "token", false, "", false)
		require.NoError(t, err)
		require.Equal(t, tc.want, req.URL.String(), "base_url=%s", tc.base)
	}
}

// 真实 Responses 透传：buildUpstreamRequestOpenAIPassthrough 命中火山→/v3/responses。
func TestVolcanoBuildUpstreamRequestOpenAIPassthroughURL(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	svc := volcanoRouteTestService()
	acc := &Account{Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": "https://ark.cn-beijing.volces.com/api/coding"}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte(`{"model":"gpt-5"}`)))
	req, err := svc.buildUpstreamRequestOpenAIPassthrough(c.Request.Context(), c, acc, []byte(`{"model":"gpt-5"}`), "token")
	require.NoError(t, err)
	require.Equal(t, "https://ark.cn-beijing.volces.com/api/coding/v3/responses", req.URL.String())
}

// 账号连接测试：testOpenAIChatCompletionsConnection 命中火山 base→/v3/chat/completions。
func TestVolcanoAccountTestChatCompletionsUsesV3Endpoint(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	var requestURL string
	stub := &volcanoSyncHTTPStub{
		respond: func(req *http.Request, _ string) (int, string, error) {
			requestURL = req.URL.String()
			return http.StatusOK, "data: [DONE]\n\n", nil
		},
	}
	svc := newVolcanoSyncService(t, stub)

	for _, base := range []string{
		"https://ark.cn-beijing.volces.com/api/coding",
		"https://ark.cn-beijing.volces.com/api/plan",
	} {
		acc := &Account{Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": base}}
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
		err := svc.testOpenAIChatCompletionsConnection(c, acc, "glm-5.3", "hi", base, "token")
		require.NoError(t, err)
		require.Equal(t, base+"/v3/chat/completions", requestURL, "base_url=%s", base)
		requestURL = ""
	}
}