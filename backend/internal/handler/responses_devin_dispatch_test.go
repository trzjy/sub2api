//go:build unit

package handler

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestResponsesAccountForwardPath(t *testing.T) {
	for _, tt := range []struct {
		name     string
		account  *service.Account
		expected string
	}{
		// devin 判定不区分账号类型：任意 type 均走 "devin"。
		{"devin default", &service.Account{Platform: service.PlatformDevin}, "devin"},
		{"devin oauth", &service.Account{Platform: service.PlatformDevin, Type: service.AccountTypeOAuth}, "devin"},
		{"devin apikey", &service.Account{Platform: service.PlatformDevin, Type: service.AccountTypeAPIKey}, "devin"},
		// antigravity 仅 OAuth 类型走兼容通道。
		{"antigravity oauth", &service.Account{Platform: service.PlatformAntigravity, Type: service.AccountTypeOAuth}, "antigravity"},
		// antigravity 非 OAuth 类型必须回落 gateway（否则被兼容通道 400）。
		{"antigravity apikey", &service.Account{Platform: service.PlatformAntigravity, Type: service.AccountTypeAPIKey}, "gateway"},
		{"antigravity setup-token", &service.Account{Platform: service.PlatformAntigravity, Type: service.AccountTypeSetupToken}, "gateway"},
		{"antigravity upstream", &service.Account{Platform: service.PlatformAntigravity, Type: service.AccountTypeUpstream}, "gateway"},
		{"antigravity bedrock", &service.Account{Platform: service.PlatformAntigravity, Type: service.AccountTypeBedrock}, "gateway"},
		{"antigravity service-account", &service.Account{Platform: service.PlatformAntigravity, Type: service.AccountTypeServiceAccount}, "gateway"},
		// 普通 gemini 与 nil 账号按空安全处理，走 gateway 兜底。
		{"gemini", &service.Account{Platform: service.PlatformGemini}, "gateway"},
		{"nil account", nil, "gateway"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expected, responsesAccountForwardPath(tt.account))
		})
	}
}

// TestResponsesAccountForwardPathDevinUsageBuckets 复用 forwardResultFromOpenAIResult：
// devin 经 OpenAIGatewayService.Forward 返回的结果在转换为 ForwardResult 时，
// InputTokens 必须按总输入契约拆成互斥桶（减去 cache 读/写），行为与 messages 分支一致。
func TestResponsesAccountForwardPathDevinUsageBuckets(t *testing.T) {
	got := forwardResultFromOpenAIResult(&service.OpenAIForwardResult{
		UpstreamEndpoint: "/v1/chat/completions",
		Usage: service.OpenAIUsage{
			InputTokens:              100,
			OutputTokens:             20,
			CacheCreationInputTokens: 5,
			CacheReadInputTokens:     7,
		},
	})
	require.NotNil(t, got)
	// 100 - 7 - 5 = 88；其余 usage 字段同名直搬，互斥桶行为不变。
	require.Equal(t, 88, got.Usage.InputTokens)
	require.Equal(t, 7, got.Usage.CacheReadInputTokens)
	require.Equal(t, 5, got.Usage.CacheCreationInputTokens)
	require.Equal(t, 20, got.Usage.OutputTokens)
}
