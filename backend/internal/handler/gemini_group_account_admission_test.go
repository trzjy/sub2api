package handler

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// TestGeminiGroupAdmitsAccount 守护 /v1/chat/completions 的 gemini 分组平台闸。
// 事故背景：分组 134 平台为 gemini，可用账号全是 antigravity OAuth（432/433），
// 旧闸只放行 gemini 平台与 Cursor，导致这些账号在转发前就被跳过，请求在 20ms 内
// 以 502「该分组所有账号均请求失败」返回且不产生任何上游错误记录；同一账号走
// /v1/messages 却正常 200。
func TestGeminiGroupAdmitsAccount(t *testing.T) {
	t.Parallel()

	antigravityOAuth := &service.Account{ID: 432, Platform: service.PlatformAntigravity, Type: service.AccountTypeOAuth}
	antigravityAPIKey := &service.Account{ID: 900, Platform: service.PlatformAntigravity, Type: service.AccountTypeAPIKey}

	cases := []struct {
		name          string
		groupPlatform string
		account       *service.Account
		want          bool
	}{
		{"gemini group + gemini account", service.PlatformGemini, &service.Account{ID: 415, Platform: service.PlatformGemini, Type: service.AccountTypeAPIKey}, true},
		{"gemini group + cursor account", service.PlatformGemini, &service.Account{ID: 447, Platform: service.PlatformCursor, Type: service.AccountTypeOAuth}, true},
		{"gemini group + antigravity oauth account", service.PlatformGemini, antigravityOAuth, true},
		{"gemini group + antigravity apikey account", service.PlatformGemini, antigravityAPIKey, false},
		{"gemini group + openai account", service.PlatformGemini, &service.Account{ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}, false},
		{"gemini group + nil account", service.PlatformGemini, nil, false},
		{"non-gemini group + antigravity account", service.PlatformAntigravity, antigravityOAuth, true},
		{"non-gemini group + openai account", service.PlatformOpenAI, &service.Account{ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := geminiGroupAdmitsAccount(tc.groupPlatform, tc.account); got != tc.want {
				t.Fatalf("geminiGroupAdmitsAccount(%q, %+v) = %v, want %v", tc.groupPlatform, tc.account, got, tc.want)
			}
		})
	}
}
