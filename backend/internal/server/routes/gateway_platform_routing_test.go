package routes

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestIsOpenAICompatibleGatewayPlatform 钉住 /chat/completions、/responses、/count_tokens
// 的 OpenAI 网关分流白名单。
//
// 活体验收 F6（2026-09-13）：codebuddy 漏列该白名单，导致 codebuddy 分组的
// /chat/completions 落入 h.Gateway（GatewayService）通用转发，绕过了挂在
// h.OpenAIGateway 上的 forwardCodeBuddy（§2.4 指纹头 + §2.5 出站改写管线），
// 出站缺少 Origin/Referer/X-Product/X-User-Id/X-Domain → 上游 403 "Request not allowed"。
func TestIsOpenAICompatibleGatewayPlatform(t *testing.T) {
	viaOpenAIGateway := []string{
		service.PlatformOpenAI,
		service.PlatformGrok,
		service.PlatformKimi,
		service.PlatformZhipu,
		service.PlatformDeepseek,
		service.PlatformMiniMax,
		service.PlatformCodeBuddy, // F6 回归点
		service.PlatformOther,
	}
	for _, platform := range viaOpenAIGateway {
		require.Truef(t, isOpenAICompatibleGatewayPlatform(platform),
			"平台 %q 必须经 OpenAI 网关转发", platform)
	}

	viaUnifiedGateway := []string{
		service.PlatformAnthropic,
		service.PlatformGemini,
		service.PlatformAntigravity,
		service.PlatformComposite,
		"",
		"bogus",
	}
	for _, platform := range viaUnifiedGateway {
		require.Falsef(t, isOpenAICompatibleGatewayPlatform(platform),
			"平台 %q 不应经 OpenAI 网关转发", platform)
	}
}
