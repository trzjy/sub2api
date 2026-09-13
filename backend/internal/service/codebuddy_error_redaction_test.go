package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestRedactCodeBuddyIdentityErrorBody 覆盖 A2 的脱敏规则：上游错误文本里的
// 账号标识（uid / user id / nickname / 「for user 123」）必须被替换为 ***，
// 而关键词、结构与其他文案保持不变。
func TestRedactCodeBuddyIdentityErrorBody(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantAbsent  []string
		wantPresent []string
	}{
		{
			name:        "natural_language_for_user",
			in:          `{"error":{"message":"Offline user session for user 123456"}}`,
			wantAbsent:  []string{"123456"},
			wantPresent: []string{"for user ***", "Offline user session"},
		},
		{
			name:        "key_value_uid",
			in:          `{"error":{"message":"uid=123456 rejected"}}`,
			wantAbsent:  []string{"123456"},
			wantPresent: []string{"uid=***"},
		},
		{
			name:        "key_value_nickname",
			in:          `{"error":{"message":"nickname: Alice is not allowed"}}`,
			wantAbsent:  []string{"Alice"},
			wantPresent: []string{"nickname: ***"},
		},
		{
			name:        "json_embedded",
			in:          `{"error":{"message":"{\"uid\":\"123456\",\"nickname\":\"Bob\"}"}}`,
			wantAbsent:  []string{"123456", "Bob"},
			wantPresent: []string{"***"},
		},
		{
			name:        "user_id_snake_case",
			in:          `{"detail":"user_id: 998877 expired"}`,
			wantAbsent:  []string{"998877"},
			wantPresent: []string{"user_id: ***"},
		},
		{
			// 无标识的错误文案必须原样保留，避免误伤正常诊断信息。
			name:        "no_identity_untouched",
			in:          `{"error":{"message":"invalid model: codebuddy-x"}}`,
			wantPresent: []string{"invalid model: codebuddy-x"},
		},
		{
			// 「user session」这类普通短语不含分隔符，不得被误判为标识。
			name:        "user_session_phrase_untouched",
			in:          `{"error":{"message":"user session not found"}}`,
			wantPresent: []string{"user session not found"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := string(redactCodeBuddyIdentityErrorBody([]byte(tc.in)))
			for _, absent := range tc.wantAbsent {
				require.NotContains(t, out, absent)
			}
			for _, present := range tc.wantPresent {
				require.Contains(t, out, present)
			}
		})
	}
}

// TestForwardCodeBuddy_UpstreamErrorRedactsIdentity 端到端验证：上游 400 错误体
// 里的 uid 不会出现在返回给下游的 JSON 中，同时状态码与 error.type 语义保留。
func TestForwardCodeBuddy_UpstreamErrorRedactsIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"model":"codebuddy-model","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))

	upstream400 := &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"Offline user session for user 123456","param":"uid"}}`)),
	}
	upstream := &httpUpstreamRecorder{resp: upstream400}

	cfg := &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{
				Enabled:           false,
				AllowInsecureHTTP: true,
			},
		},
		Gateway: config.GatewayConfig{
			CodeBuddy: config.GatewayCodeBuddyConfig{SanitizeEnabled: true},
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
	account := healthyCodeBuddyGatewayTestAccount(7703, "access-token")

	result, err := svc.forwardCodeBuddy(context.Background(), c, account, body, "codebuddy-model", false, time.Now())
	require.Error(t, err, "上游 400 应返回错误")
	require.Nil(t, result)

	require.Equal(t, http.StatusBadRequest, recorder.Code, "确定性客户端错误应保留上游 400 状态码")
	downstream := recorder.Body.String()
	require.NotContains(t, downstream, "123456", "下游响应不得泄漏上游账号 uid")
	require.Contains(t, downstream, "***", "被脱敏的标识应替换为占位符")
	require.Contains(t, downstream, "invalid_request_error", "错误类型语义应保留")
}
