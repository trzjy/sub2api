//go:build unit

package handler

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestChatCompletionsAccountForwardPath 锁定 chat completions 账号级分派口径：
// devin 必须优先命中 devin 出口（走 OpenAIGatewayService → forwardDevin），
// gemini/antigravity 的判定与历史条件逐字一致，其余平台与 nil 维持 gateway。
func TestChatCompletionsAccountForwardPath(t *testing.T) {
	tests := []struct {
		name    string
		account *service.Account
		want    string
	}{
		{
			name:    "devin",
			account: &service.Account{Platform: service.PlatformDevin},
			want:    "devin",
		},
		{
			name:    "gemini",
			account: &service.Account{Platform: service.PlatformGemini},
			want:    "gemini",
		},
		{
			name:    "antigravity OAuth",
			account: &service.Account{Platform: service.PlatformAntigravity, Type: service.AccountTypeOAuth},
			want:    "antigravity",
		},
		{
			name:    "antigravity APIKey",
			account: &service.Account{Platform: service.PlatformAntigravity, Type: service.AccountTypeAPIKey},
			want:    "gateway",
		},
		{
			name:    "cursor",
			account: &service.Account{Platform: service.PlatformCursor},
			want:    "gateway",
		},
		{
			name:    "anthropic",
			account: &service.Account{Platform: service.PlatformAnthropic},
			want:    "gateway",
		},
		{
			name:    "nil",
			account: nil,
			want:    "gateway",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, chatCompletionsAccountForwardPath(tt.account))
		})
	}
}
