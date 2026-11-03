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
		s.shouldFailoverOpenAIUpstreamResponse(account, http.StatusBadRequest, "safety check failed", []byte(body)),
		"审核拒绝必须换号")

	assert.False(t,
		s.shouldFailoverOpenAIUpstreamResponse(account, http.StatusBadRequest, "invalid temperature", []byte(`{"error":{"message":"invalid temperature"}}`)),
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
