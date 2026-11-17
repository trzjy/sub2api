//go:build unit

package handler

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestMessagesNonGeminiForwardPath(t *testing.T) {
	for _, tt := range []struct {
		name     string
		account  *service.Account
		expected string
	}{
		{"devin", &service.Account{Platform: service.PlatformDevin}, "devin"},
		{"antigravity oauth", &service.Account{Platform: service.PlatformAntigravity, Type: service.AccountTypeOAuth}, "antigravity"},
		// Antigravity 的 APIKey 账号不满足现有判定（Type != APIKey），回落到 gateway。
		{"antigravity api key", &service.Account{Platform: service.PlatformAntigravity, Type: service.AccountTypeAPIKey}, "gateway"},
		{"gemini", &service.Account{Platform: service.PlatformGemini}, "gateway"},
		{"cursor", &service.Account{Platform: service.PlatformCursor}, "gateway"},
		{"anthropic", &service.Account{Platform: service.PlatformAnthropic}, "gateway"},
		// nil 账号按空安全处理：返回 "gateway"，交由既有转发器兜底。
		{"nil account", nil, "gateway"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expected, messagesNonGeminiForwardPath(tt.account))
		})
	}
}
