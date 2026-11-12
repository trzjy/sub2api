//go:build unit

package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 2026-11-03 生产事故回归：商汤（token.sensenova.cn）对长上下文请求返回
// 400 {"error":{"code":"9","message":"safety check failed","type":"failed_precondition_error"}}，
// 同一条请求换账号多数能过。这类拒绝是上游/账号侧状态，必须换号重试，
// 不能当客户端参数错误透传（透传会让客户端对着同一面墙反复 400）。
func TestIsOpenAIUpstreamSafetyRejection(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		status int
		msg    string
		body   string
		want   bool
	}{
		{
			name:   "商汤结构化审核拒绝",
			status: http.StatusBadRequest,
			msg:    "safety check failed",
			body:   `{"error":{"code":"9","message":"safety check failed","type":"failed_precondition_error"}}`,
			want:   true,
		},
		{
			name:   "文案被改写只留 code 与 type",
			status: http.StatusBadRequest,
			body:   `{"error":{"code":"9","type":"failed_precondition_error"}}`,
			want:   true,
		},
		{
			name:   "纯文本错误体",
			status: http.StatusBadRequest,
			body:   "safety check failed",
			want:   true,
		},
		{
			name:   "大小写不敏感",
			status: http.StatusBadRequest,
			msg:    "Safety Check Failed",
			want:   true,
		},
		{
			name:   "非 400 不归此类",
			status: http.StatusInternalServerError,
			msg:    "safety check failed",
			body:   `{"error":{"message":"safety check failed"}}`,
			want:   false,
		},
		{
			name:   "普通参数错误不得换号",
			status: http.StatusBadRequest,
			msg:    "invalid temperature",
			body:   `{"error":{"message":"invalid temperature","type":"invalid_request_error"}}`,
			want:   false,
		},
		{
			name:   "code 9 但类型不是 failed_precondition",
			status: http.StatusBadRequest,
			body:   `{"error":{"code":"9","type":"invalid_request_error"}}`,
			want:   false,
		},
		{
			name:   "空响应体",
			status: http.StatusBadRequest,
			want:   false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isOpenAIUpstreamSafetyRejection(tc.status, tc.msg, []byte(tc.body))
			assert.Equal(t, tc.want, got)
		})
	}
}

// shouldFailoverOpenAIUpstreamResponse 是全部 OpenAI 兼容入口（messages /
// chat completions / responses）的单一判定落点，审核拒绝必须在它这里放行到 failover。
func TestShouldFailoverOpenAIUpstreamResponse_SafetyRejection(t *testing.T) {
	t.Parallel()

	s := &OpenAIGatewayService{}
	account := &Account{ID: 1}

	body := `{"error":{"code":"9","message":"safety check failed","type":"failed_precondition_error"}}`
	assert.True(t,
		s.shouldFailoverOpenAIUpstreamResponse(nil, account, http.StatusBadRequest, "safety check failed", []byte(body), ""),
		"审核拒绝必须换号")

	assert.False(t,
		s.shouldFailoverOpenAIUpstreamResponse(nil, account, http.StatusBadRequest, "invalid temperature", []byte(`{"error":{"message":"invalid temperature"}}`), ""),
		"普通 400 仍按客户端错误透传")
}

// 分类结果决定 handler 侧的处置：不原地重试、不停号、耗尽时说上游不可用。
func TestNewOpenAIUpstreamFailoverError_SafetyRejection(t *testing.T) {
	t.Parallel()

	body := []byte(`{"error":{"code":"9","message":"safety check failed","type":"failed_precondition_error"}}`)
	foErr := newOpenAIUpstreamFailoverError(http.StatusBadRequest, http.Header{}, body, "safety check failed", false)

	require.NotNil(t, foErr)
	assert.True(t, foErr.IsOpenAIUpstreamSafetyRejection())
	assert.False(t, foErr.RetryableOnSameAccount, "同一账号重试只会再撞一次同一面墙")
	assert.False(t, foErr.RequestScopedTransient, "审核拒绝与账号绑定，不是请求级瞬时故障")
	assert.Equal(t, GatewayFailureScopeAccount, foErr.Scope)
	assert.Equal(t, NextAccountRetry, foErr.NextAccountAction)
	assert.Equal(t, http.StatusBadGateway, foErr.ClientStatusCode)
	assert.NotEmpty(t, foErr.ClientMessage)
}

// 非审核类的 400 不能被误标成审核拒绝（否则会把真正的参数错误变成 502）。
func TestNewOpenAIUpstreamFailoverError_Plain400NotSafetyRejection(t *testing.T) {
	t.Parallel()

	body := []byte(`{"error":{"message":"messages: text content blocks must be non-empty","type":"invalid_request_error"}}`)
	foErr := newOpenAIUpstreamFailoverError(http.StatusBadRequest, http.Header{}, body, "text content blocks must be non-empty", false)

	require.NotNil(t, foErr)
	assert.False(t, foErr.IsOpenAIUpstreamSafetyRejection())
	assert.Zero(t, foErr.ClientStatusCode)
}

// 审核拒绝必须带上「上游主机」，否则 handler 只能逐个撞同源账号：
// 生产实测 419→416→426 连撞三次商汤，切换预算全耗在同一面墙上。
func TestUpstreamHostOfAccount(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		acc  *Account
		want string
	}{
		{
			name: "商汤显式 base_url",
			acc: &Account{
				ID:          419,
				Platform:    "deepseek",
				Type:        AccountTypeAPIKey,
				Credentials: map[string]any{"base_url": "https://token.sensenova.cn/v1/chat/completions"},
			},
			want: "token.sensenova.cn",
		},
		{
			name: "带端口的自建地址",
			acc: &Account{
				ID:          411,
				Platform:    "deepseek",
				Type:        AccountTypeAPIKey,
				Credentials: map[string]any{"base_url": "http://119.28.137.187:18081"},
			},
			want: "119.28.137.187:18081",
		},
		{
			name: "大小写与空格归一",
			acc: &Account{
				ID:          1,
				Type:        AccountTypeAPIKey,
				Credentials: map[string]any{"base_url": " HTTPS://Token.SenseNova.CN/v1 "},
			},
			want: "token.sensenova.cn",
		},
		{
			name: "没有 base_url 时返回空（视为不排除）",
			acc:  &Account{ID: 2, Type: AccountTypeAPIKey, Credentials: map[string]any{}},
			want: "",
		},
		{
			name: "nil 账号",
			acc:  nil,
			want: "",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, upstreamHostOfAccount(tc.acc))
		})
	}
}

// 一次把整片同源账号摘出去：同 host 的全排、别家留着、没有 base_url 的不误伤。
func TestSiblingAccountIDsByHost(t *testing.T) {
	t.Parallel()

	mk := func(id int64, baseURL string) Account {
		creds := map[string]any{}
		if baseURL != "" {
			creds["base_url"] = baseURL
		}
		return Account{ID: id, Platform: "deepseek", Type: AccountTypeAPIKey, Credentials: creds}
	}

	accounts := []Account{
		mk(416, "https://token.sensenova.cn/v1/chat/completions"),
		mk(419, "https://token.sensenova.cn/v1/chat/completions"),
		mk(426, "https://token.sensenova.cn/v1/chat/completions"),
		mk(239, "https://model.kimitk.top/v1"),
		mk(411, "http://119.28.137.187:18081"),
		mk(999, ""),
	}

	assert.Equal(t, []int64{416, 419, 426}, siblingAccountIDsByHost(accounts, "token.sensenova.cn"))
	assert.Equal(t, []int64{239}, siblingAccountIDsByHost(accounts, "model.kimitk.top"))
	assert.Empty(t, siblingAccountIDsByHost(accounts, ""), "空主机名不得排除任何人")
	assert.Empty(t, siblingAccountIDsByHost(accounts, "unknown.example"))
}

// 端到端：safety 400 构造出的 failover 错误必须携带主机，普通 400 不得携带。
func TestNewOpenAIAccountFailoverError_CarriesUpstreamHostForSafety(t *testing.T) {
	t.Parallel()

	s := &OpenAIGatewayService{}
	account := &Account{
		ID:          419,
		Platform:    "deepseek",
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "https://token.sensenova.cn/v1/chat/completions"},
	}

	safetyBody := []byte(`{"error":{"code":"9","message":"safety check failed","type":"failed_precondition_error"}}`)
	foErr := s.newOpenAIAccountFailoverError(account, http.StatusBadRequest, http.Header{}, safetyBody, "safety check failed", false, false, "")
	require.NotNil(t, foErr)
	assert.True(t, foErr.IsOpenAIUpstreamSafetyRejection())
	assert.Equal(t, "token.sensenova.cn", foErr.ExcludeUpstreamHost)

	plainBody := []byte(`{"error":{"message":"invalid temperature","type":"invalid_request_error"}}`)
	plainErr := s.newOpenAIAccountFailoverError(account, http.StatusBadRequest, http.Header{}, plainBody, "invalid temperature", false, false, "")
	require.NotNil(t, plainErr)
	assert.Empty(t, plainErr.ExcludeUpstreamHost, "普通 400 不应触发同源排除")
}
