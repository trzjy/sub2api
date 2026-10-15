package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// muse 平台、客户端未带 session：每请求生成 UUID 塞入 x-opencode-session，
// 并覆写 User-Agent 为 sub2api-relay/1.0。
func TestApplyMuseSessionHeader_InjectsForMuse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	account := &Account{Platform: PlatformMuse}
	headers := http.Header{}
	applyMuseSessionHeader(c, account, headers)

	got := headers.Get("x-opencode-session")
	require.NotEmpty(t, got, "muse 无客户端 session 时应生成 UUID")
	require.Equal(t, museRelayUserAgent, headers.Get("user-agent"))
}

// muse 平台、客户端已带 x-opencode-session：原样保留（每会话稳定 ID），并仍覆写 UA。
func TestApplyMuseSessionHeader_PreservesClientSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("x-opencode-session", "client-stable-id")
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req

	account := &Account{Platform: PlatformMuse}
	headers := http.Header{}
	applyMuseSessionHeader(c, account, headers)

	require.Equal(t, "client-stable-id", headers.Get("x-opencode-session"),
		"客户端已带 session 应原样保留")
	require.Equal(t, museRelayUserAgent, headers.Get("user-agent"))
}

// 非 muse 平台：零改动——不得注入 session，不得覆写 UA。
func TestApplyMuseSessionHeader_NoEffectForOtherPlatform(t *testing.T) {
	gin.SetMode(gin.TestMode)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("x-opencode-session", "should-not-leak")
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req

	account := &Account{Platform: PlatformOpenAI}
	headers := http.Header{}
	applyMuseSessionHeader(c, account, headers)

	require.Empty(t, headers.Get("x-opencode-session"),
		"非 muse 平台不得注入 session")
	require.NotEqual(t, museRelayUserAgent, headers.Get("user-agent"),
		"非 muse 平台不得覆写 UA")
}

// ② 非 muse 平台但 base_url host 为 opencode.ai（伪装账号，如 platform=kimi）：
// 与 muse 同口径注入每请求 UUID session，并覆写 UA。
func TestApplyMuseSessionHeader_InjectsForOpenCodeHostDisguise(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	account := &Account{
		Platform: PlatformKimi,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://opencode.ai/zen/go/v1",
		},
	}
	headers := http.Header{}
	applyMuseSessionHeader(c, account, headers)

	got := headers.Get("x-opencode-session")
	require.NotEmpty(t, got, "opencode.ai base_url 的非 muse 账号应注入 session")
	_, err := uuid.Parse(got)
	require.NoError(t, err, "注入值应为合法 UUID")
	require.Equal(t, museRelayUserAgent, headers.Get("user-agent"))
}

// ③ 非 muse 平台且 base_url host 非 opencode.ai：零改动——不得注入 session，不得覆写 UA。
func TestApplyMuseSessionHeader_NoEffectForNonOpenCodeHost(t *testing.T) {
	gin.SetMode(gin.TestMode)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("x-opencode-session", "should-not-leak")
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req

	account := &Account{
		Platform: PlatformKimi,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://api.moonshot.cn/v1",
		},
	}
	headers := http.Header{}
	applyMuseSessionHeader(c, account, headers)

	require.Empty(t, headers.Get("x-opencode-session"),
		"非 opencode.ai base_url 不得注入 session")
	require.NotEqual(t, museRelayUserAgent, headers.Get("user-agent"),
		"非 opencode.ai base_url 不得覆写 UA")
}

// ④ 非 muse 平台、base_url host 为 opencode.ai、客户端已带 x-opencode-session：
// 原样保留客户端值不覆盖，并仍覆写 UA。
func TestApplyMuseSessionHeader_PreservesClientSessionForOpenCodeHost(t *testing.T) {
	gin.SetMode(gin.TestMode)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("x-opencode-session", "client-stable-id")
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req

	account := &Account{
		Platform: PlatformKimi,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://opencode.ai/zen/go/v1",
		},
	}
	headers := http.Header{}
	applyMuseSessionHeader(c, account, headers)

	require.Equal(t, "client-stable-id", headers.Get("x-opencode-session"),
		"客户端已带 session 应原样保留，不被覆盖")
	require.Equal(t, museRelayUserAgent, headers.Get("user-agent"))
}

// ① OC-T1-FIX4 cc_pipeline 调用点（openai_gateway_cc_pipeline.go，applyMuseSessionHeader
// 紧跟 applyOpenCodeSessionHeader 之后）：构造 cc_pipeline 形状的 upstreamReq（Content-Type
// / Authorization / Accept 等内置头）+ platform=kimi 且 base_url host=opencode.ai 伪装账号，
// 调用后 header 应带 UUID 格式 x-opencode-session，并覆写 UA 为 sub2api-relay/1.0。
func TestApplyMuseSessionHeader_CCPipelineCallSiteInjects(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	upstreamReq := http.Header{}
	upstreamReq.Set("Content-Type", "application/json")
	upstreamReq.Set("Authorization", "Bearer token")
	upstreamReq.Set("Accept", "text/event-stream")

	account := &Account{
		Platform: PlatformKimi,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://opencode.ai/zen/go/v1",
		},
	}
	applyMuseSessionHeader(c, account, upstreamReq)

	got := upstreamReq.Get("x-opencode-session")
	require.NotEmpty(t, got, "cc_pipeline 调用点应为 opencode.ai 伪装账号注入 session")
	_, err := uuid.Parse(got)
	require.NoError(t, err, "注入值应为合法 UUID")
	require.Equal(t, museRelayUserAgent, upstreamReq.Get("user-agent"))
}

// ② OC-T1-FIX4 passthrough 调用点（openai_gateway_passthrough.go，buildUpstreamRequestOpenAIPassthrough
// 内 applyMuseSessionHeader 紧跟 applyOpenCodeSessionHeader 之后）：构造 passthrough 形状的
// upstreamReq + platform=kimi 且 base_url host=opencode.ai 伪装账号，调用后 header 应带 UUID
// 格式 x-opencode-session，并覆写 UA 为 sub2api-relay/1.0。
func TestApplyMuseSessionHeader_PassthroughCallSiteInjects(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	req := http.Header{}
	req.Set("Content-Type", "application/json")
	req.Set("Accept", "application/json")

	account := &Account{
		Platform: PlatformKimi,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://opencode.ai/zen/go/v1",
		},
	}
	applyMuseSessionHeader(c, account, req)

	got := req.Get("x-opencode-session")
	require.NotEmpty(t, got, "passthrough 调用点应为 opencode.ai 伪装账号注入 session")
	_, err := uuid.Parse(got)
	require.NoError(t, err, "注入值应为合法 UUID")
	require.Equal(t, museRelayUserAgent, req.Get("user-agent"))
}

// ③ 非 OpenCode 账号经上述两个调用点（cc_pipeline 形状与 passthrough 形状）均零改动：
// header 不得出现 x-opencode-session，UA 不得被覆写为中继身份。守卫兜底保证非 muse 且
// base_url 非 opencode.ai 的账号不受影响。
func TestApplyMuseSessionHeader_CallSiteNoEffectForNonOpenCodeAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	account := &Account{
		Platform: PlatformKimi,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://api.moonshot.cn/v1",
		},
	}

	// cc_pipeline 形状
	ccHeaders := http.Header{}
	ccHeaders.Set("Content-Type", "application/json")
	ccHeaders.Set("Authorization", "Bearer token")
	applyMuseSessionHeader(c, account, ccHeaders)
	require.Empty(t, ccHeaders.Get("x-opencode-session"),
		"非 opencode.ai base_url 经 cc_pipeline 调用点不得注入 session")
	require.NotEqual(t, museRelayUserAgent, ccHeaders.Get("user-agent"),
		"非 opencode.ai base_url 经 cc_pipeline 调用点不得覆写 UA")

	// passthrough 形状
	ptHeaders := http.Header{}
	ptHeaders.Set("Content-Type", "application/json")
	applyMuseSessionHeader(c, account, ptHeaders)
	require.Empty(t, ptHeaders.Get("x-opencode-session"),
		"非 opencode.ai base_url 经 passthrough 调用点不得注入 session")
	require.NotEqual(t, museRelayUserAgent, ptHeaders.Get("user-agent"),
		"非 opencode.ai base_url 经 passthrough 调用点不得覆写 UA")
}
