//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Scenario: OAuth / setup-token 凭据只能走签发方协议，API Key 与其他凭据类型不受限
func TestAnthropicMessagesCredentialAllowed(t *testing.T) {
	cases := []struct {
		platform string
		typ      string
		allowed  bool
	}{
		{PlatformAnthropic, AccountTypeOAuth, true},
		{PlatformAnthropic, AccountTypeSetupToken, true},
		{PlatformAnthropic, AccountTypeServiceAccount, true},
		{PlatformAnthropic, AccountTypeBedrock, true},
		{PlatformTypeSafe, AccountTypeOAuth, true},
		{PlatformOpenAI, AccountTypeAPIKey, true},
		{PlatformDeepseek, AccountTypeAPIKey, true},
		{PlatformOpenAI, AccountTypeOAuth, false},
		{PlatformOpenAI, AccountTypeSetupToken, false},
		{PlatformGrok, AccountTypeOAuth, false},
		{PlatformGemini, AccountTypeOAuth, false},
		{PlatformAntigravity, AccountTypeOAuth, false},
	}
	for _, tc := range cases {
		allowed := anthropicMessagesCredentialAllowed(&Account{ID: 1, Platform: tc.platform, Type: tc.typ})
		require.Equal(t, tc.allowed, allowed, "%s/%s", tc.platform, tc.typ)
	}
}

// Scenario: 回归 —— OpenAI OAuth 账号误入 Anthropic 管线时，在构建请求前即被拒绝
// （ChatGPT token 不会发往 api.anthropic.com），且只让 handler 换号、不处罚账号
func TestGatewayBuildUpstreamRequestRejectsForeignOAuthWithoutPenalty(t *testing.T) {
	account := &Account{ID: 25, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	body := []byte(`{"model":"gpt-6-astra","messages":[]}`)

	req, _, err := (&GatewayService{}).buildUpstreamRequest(context.Background(), nil, account, body, "chatgpt-token", "oauth", "gpt-6-astra", true, false)
	require.Nil(t, req)

	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr))
	require.True(t, failoverErr.ShouldRetryNextAccount())
	require.False(t, failoverErr.RetryableOnSameAccount, "same-account retry would end in temp-unschedulable")
	require.False(t, failoverErr.IsCredentialFailure())
	require.Equal(t, anthropicMessagesForeignOAuthReason, failoverErr.Reason)
}

// Scenario: count_tokens 对不匹配凭据按「平台不支持」返回 404，不取 token、不出站
func TestForwardCountTokensRejectsForeignOAuthAsUnsupported(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil)

	account := &Account{ID: 25, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	parsed := &ParsedRequest{Model: "gpt-6-astra", Body: NewRequestBodyRef([]byte(`{"model":"gpt-6-astra","messages":[]}`))}

	err := (&GatewayService{}).ForwardCountTokens(context.Background(), c, account, parsed)

	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, w.Code)

// TestBuildCustomRelayURL 锁定 A3 收敛：path 由版本感知原语拼接后，再追加 beta=true 与
// 可选 proxy 查询参数。base 自带版本段时不得出现 /v1/v1 双段，proxy query 追加语义不变。
func TestBuildCustomRelayURL(t *testing.T) {
	s := &GatewayService{}

	cases := []struct {
		name string
		base string
		path string
		want string
	}{
		{
			name: "no version suffix",
			base: "https://upstream.example",
			path: "/v1/messages",
			want: "https://upstream.example/v1/messages?beta=true",
		},
		{
			// 核心回归：base 已带 /v1，只追加相对路径。
			name: "base has /v1",
			base: "https://upstream.example/v1",
			path: "/v1/messages",
			want: "https://upstream.example/v1/messages?beta=true",
		},
		{
			name: "count tokens with /v1 base",
			base: "https://upstream.example/v1",
			path: "/v1/messages/count_tokens",
			want: "https://upstream.example/v1/messages/count_tokens?beta=true",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := s.buildCustomRelayURL(c.base, c.path, &Account{ID: 1})
			if got != c.want {
				t.Fatalf("base=%q path=%q: expected %q, got %q", c.base, c.path, c.want, got)
			}
			if idx := len(got); idx == 0 {
				t.Fatalf("expected non-empty url")
			}
		})
	}
}

// TestBuildCustomRelayURLWithProxyQuery 锁定 proxy 查询参数在原语产出之上正确追加
// （account.ProxyID/Proxy 均非空时追加 &proxy=<escaped>）。
func TestBuildCustomRelayURLWithProxyQuery(t *testing.T) {
	s := &GatewayService{}
	proxyID := int64(7)
	account := &Account{
		ID:      1,
		ProxyID: &proxyID,
		Proxy:   &Proxy{ID: proxyID, Protocol: "http", Host: "127.0.0.1", Port: 7890},
	}

	// base 无版本段：proxy query 追加在原语产出之后。
	if got, want := s.buildCustomRelayURL("https://upstream.example", "/v1/messages", account),
		"https://upstream.example/v1/messages?beta=true&proxy=http%3A%2F%2F127.0.0.1%3A7890"; got != want {
		t.Fatalf("no version suffix: expected %q, got %q", want, got)
	}

	// base 带 /v1 版本段：原语避免双段，proxy query 追加语义不变。
	if got, want := s.buildCustomRelayURL("https://upstream.example/v1", "/v1/messages", account),
		"https://upstream.example/v1/messages?beta=true&proxy=http%3A%2F%2F127.0.0.1%3A7890"; got != want {
		t.Fatalf("base has /v1: expected %q, got %q", want, got)
	}
}
