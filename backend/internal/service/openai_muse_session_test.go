package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
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
