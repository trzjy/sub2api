package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// codeBuddyShadowRepoStub 只实现 resolveCredentialAccount 需要的 GetByID，
// 其余 AccountRepository 方法由内嵌接口提供（本测试不会调用）。避免依赖真实数据库。
type codeBuddyShadowRepoStub struct {
	AccountRepository
	parent *Account
}

func (s *codeBuddyShadowRepoStub) GetByID(_ context.Context, id int64) (*Account, error) {
	if s.parent != nil && s.parent.ID == id {
		return s.parent, nil
	}
	return nil, nil
}

// TestIsCodeBuddyShadowAccount 覆盖三入站路径共享的 codebuddy 影子判定（SSOT）：
// 只有「影子标记 + quota_dimension=codebuddy」为真；spark 影子、普通账号、nil 均为假。
func TestIsCodeBuddyShadowAccount(t *testing.T) {
	require.True(t, isCodeBuddyShadowAccount(&Account{
		Platform: PlatformDeepseek, Type: AccountTypeOAuth,
		ParentAccountID: ptrI64(10), QuotaDimension: QuotaDimensionCodeBuddy,
	}))
	require.False(t, isCodeBuddyShadowAccount(nil))
	// spark 影子（openai 母）不得命中
	require.False(t, isCodeBuddyShadowAccount(&Account{
		Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		ParentAccountID: ptrI64(10), QuotaDimension: QuotaDimensionSpark,
	}))
	// 无影子标记的普通账号不得命中
	require.False(t, isCodeBuddyShadowAccount(&Account{
		Platform: PlatformCodeBuddy, Type: AccountTypeOAuth,
	}))
	// 有影子标记但维度不是 codebuddy（遗留/异常）不得命中
	require.False(t, isCodeBuddyShadowAccount(&Account{
		Platform: PlatformDeepseek, Type: AccountTypeOAuth,
		ParentAccountID: ptrI64(10),
	}))
}

// TestForwardAsAnthropic_CodeBuddyShadowRoutesToCodeBuddyUpstream 是 /v1/messages 分发修复
// 的核心回归：Anthropic 入站 + codebuddy 影子 → 不得落到 chatgpt.com（通用 OpenAI 路径），
// 必须走 CodeBuddy 出站上游（copilot.tencent.com），且完成 Anthropic→CC→Anthropic 转换。
func TestForwardAsAnthropic_CodeBuddyShadowRoutesToCodeBuddyUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const body = `{"model":"deepseek-v4-flash","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`

	ccOK := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{
			"id":"c1","object":"chat.completion","model":"deepseek-v4-flash",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`)),
	}
	upstream := &httpUpstreamRecorder{resp: ccOK}
	parent := healthyCodeBuddyGatewayTestAccount(9000, "access-token")
	cfg := &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true},
		},
		Gateway: config.GatewayConfig{
			CodeBuddy: config.GatewayCodeBuddyConfig{SanitizeEnabled: true},
		},
	}
	svc := &OpenAIGatewayService{
		cfg:          cfg,
		httpUpstream: upstream,
		accountRepo:  &codeBuddyShadowRepoStub{parent: parent},
	}
	shadow := &Account{
		ID:              9001,
		Name:            "PRM1-shadow-deepseek",
		Platform:        PlatformDeepseek,
		Type:            AccountTypeOAuth,
		Status:          StatusActive,
		Schedulable:     true,
		Concurrency:     1,
		ParentAccountID: ptrI64(parent.ID),
		QuotaDimension:  QuotaDimensionCodeBuddy,
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))

	_, err := svc.ForwardAsAnthropic(context.Background(), c, shadow, []byte(body), "", "")
	require.NoError(t, err)

	require.NotNil(t, upstream.lastReq, "must have sent an upstream request")
	require.Equal(t, "copilot.tencent.com", upstream.lastReq.URL.Host,
		"codebuddy 影子必须走 CodeBuddy 上游，不得落到 chatgpt.com")
	require.NotContains(t, upstream.lastReq.URL.Host, "chatgpt.com")
	require.Equal(t, http.StatusOK, rec.Code, "Anthropic 客户端应收到 200")
}
