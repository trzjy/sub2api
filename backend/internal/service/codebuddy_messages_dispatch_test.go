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

// TestForwardAsAnthropic_DirectCodeBuddyRoutesToCodeBuddyUpstream 覆盖 /v1/messages
// 出站分发的直绑 codebuddy 版本（影子版见上）。生产 group 109 下挂的是**直绑**
// codebuddy OAuth 账号（platform=codebuddy、type=oauth、ParentAccountID 为空、
// quota_dimension=global，即非影子），此前 ForwardAsAnthropic 只判
// isCodeBuddyShadowAccount，直绑账号落通用 OpenAI 路径；又因
// shouldForwardOpenAIResponsesViaRawChatCompletions 要求 AccountTypeAPIKey，
// 请求会打到 ChatGPT/Codex 后端并被 403 阻断页拒绝。
//
// 口径与 /v1/chat/completions（openai_gateway_chat_completions.go 的
// account.Platform == PlatformCodeBuddy 分支）和 /v1/responses
// （openai_gateway_forward.go 同名分支）保持一致。
func TestForwardAsAnthropic_DirectCodeBuddyRoutesToCodeBuddyUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const body = `{"model":"space-bunny","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`

	ccOK := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{
			"id":"c1","object":"chat.completion","model":"space-bunny",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`)),
	}
	upstream := &httpUpstreamRecorder{resp: ccOK}
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
		accountRepo:  &codeBuddyShadowRepoStub{},
	}
	// 直绑 codebuddy OAuth 账号：无 ParentAccountID（非影子），凭证即自身。
	direct := healthyCodeBuddyGatewayTestAccount(9002, "direct-access-token")
	require.Nil(t, direct.ParentAccountID, "直绑账号不应有母账号")
	require.True(t, isCodeBuddyPlatformAccount(direct))
	require.False(t, isCodeBuddyShadowAccount(direct), "该账号不是影子，两判定互斥")

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))

	_, err := svc.ForwardAsAnthropic(context.Background(), c, direct, []byte(body), "", "")
	// 先断言出站主机（本次修复的核心），再断言返回错误——这样缺少直绑分支时
	// 失败信息直接指向"打到 chatgpt.com"，而不是被下游流解析错误掩盖。
	require.NotNil(t, upstream.lastReq, "must have sent an upstream request")
	require.Equal(t, "copilot.tencent.com", upstream.lastReq.URL.Host,
		"直绑 codebuddy 账号必须走 CodeBuddy 上游，不得落到 chatgpt.com")
	require.NotContains(t, upstream.lastReq.URL.Host, "chatgpt.com")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code, "Anthropic 客户端应收到 200")
	require.Contains(t, rec.Body.String(), `"type":"message"`,
		"响应应为 Anthropic Messages 形状（CC→Anthropic 回桥生效）")
}

// TestResolveMessagesDispatchModel_CodeBuddySkipsOpenAIDefaults 钉住 codebuddy 分组
// 不得套用 openai 专属的 gpt-5.x 调度级默认模型值：其上游是 copilot.tencent.com，
// 收到 gpt-5.x 必被拒。分组模型白名单关闭时 claude-* 请求可达本函数。
func TestResolveMessagesDispatchModel_CodeBuddySkipsOpenAIDefaults(t *testing.T) {
	g := &Group{Platform: PlatformCodeBuddy}
	require.Empty(t, g.ResolveMessagesDispatchModel("claude-opus-4-6"))
	require.Empty(t, g.ResolveMessagesDispatchModel("claude-sonnet-4-5-20250929"))
	require.Empty(t, g.ResolveMessagesDispatchModel("claude-haiku-4-5-20251001"))
	// 非 claude-* 模型（生产 group 109 的 space-bunny）本就返回空，保持不变。
	require.Empty(t, g.ResolveMessagesDispatchModel("space-bunny"))
}
