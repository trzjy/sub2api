//go:build unit

package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestIsOAuth401RecoverableTempUnschedReason 覆盖 token 刷新清停调门的三类 reason 形态：
// 健康熔断（不得被 token 刷新清除）、OAuth 401 结构化 reason、OAuth 401 纯文本 reason。
func TestIsOAuth401RecoverableTempUnschedReason(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		expect bool
	}{
		{
			name: "health breaker reason is not token-refresh recoverable",
			reason: func() string {
				payload, err := json.Marshal(TempUnschedState{
					StatusCode:     401,
					MatchedKeyword: "openai_apikey_health_breaker",
					ErrorMessage:   "health breaker tripped",
				})
				require.NoError(t, err)
				return string(payload)
			}(),
			expect: false,
		},
		{
			name: "oauth 401 structured reason is token-refresh recoverable",
			reason: func() string {
				payload, err := json.Marshal(TempUnschedState{
					StatusCode:     401,
					MatchedKeyword: "oauth_401_recoverable",
					ErrorMessage:   "OAuth 401: unauthorized",
				})
				require.NoError(t, err)
				return string(payload)
			}(),
			expect: true,
		},
		{
			name:   "oauth 401 plain text reason is token-refresh recoverable",
			reason: "OAuth 401: unauthorized",
			expect: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expect, isOAuth401RecoverableTempUnschedReason(tt.reason))
		})
	}
}
