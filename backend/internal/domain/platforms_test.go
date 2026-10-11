package domain

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 重构前各处手写的平台列表，作为平台清单派生结果的等价基准；重构后新登记的平台
// （Command Code、Cline）按同类平台（OpenCode）的位置补入；fork 自研平台
// （CodeBuddy、Muse）追加在清单末尾。
var (
	legacyDisplayOrder = []string{
		PlatformAnthropic, PlatformOpenAI, PlatformGemini, PlatformAntigravity, PlatformGrok,
		PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo, PlatformTypeSafe,
		PlatformCommandCode,
		PlatformCline,
		PlatformCodeBuddy,
		PlatformMuse,
	}
	legacyCompositePrecedence = []string{
		PlatformAnthropic, PlatformGemini, PlatformOpenAI, PlatformAntigravity, PlatformGrok,
		PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo, PlatformTypeSafe,
		PlatformCommandCode,
		PlatformCline,
		PlatformCodeBuddy,
		PlatformMuse,
	}
	legacyOpenAIGateway = []string{
		PlatformOpenAI, PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo,
		PlatformCommandCode,
		PlatformCline,
		PlatformCodeBuddy,
		PlatformMuse,
	}
	legacyCNProviders    = []string{PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax}
	legacyLiteLLMByPlatf = map[string]string{
		PlatformAnthropic: "anthropic", PlatformOpenAI: "openai", PlatformGemini: "gemini",
		PlatformAntigravity: "anthropic", PlatformGrok: "xai", PlatformKimi: "moonshot",
		PlatformZhipu: "zhipu", PlatformDeepseek: "deepseek", PlatformMiniMax: "minimax",
		PlatformOpenCodeGo: "opencode-go", PlatformTypeSafe: "typesafe", PlatformCommandCode: "", PlatformCline: "",
		PlatformCodeBuddy: "", PlatformMuse: "",
	}
)

func TestPlatformListMatchesLegacyLists(t *testing.T) {
	require.Equal(t, legacyDisplayOrder, ConcretePlatformIDs())
	require.Equal(t, legacyCompositePrecedence, CompositePrecedencePlatformIDs())
	require.Equal(t, legacyOpenAIGateway, PlatformIDsWhere(func(spec PlatformSpec) bool {
		return spec.Gateway == PlatformGatewayOpenAI
	}))
	require.Equal(t, legacyCNProviders, PlatformIDsWhere(func(spec PlatformSpec) bool { return spec.CNProvider }))
	for platform, provider := range legacyLiteLLMByPlatf {
		spec, ok := LookupPlatform(platform)
		require.True(t, ok, platform)
		require.Equal(t, provider, spec.LiteLLMProvider, platform)
	}
}

func TestPlatformListPredicates(t *testing.T) {
	for _, platform := range legacyDisplayOrder {
		require.True(t, IsConcretePlatform(platform), platform)
		require.True(t, IsGroupPlatform(platform), platform)
		spec, ok := LookupPlatform(platform)
		require.True(t, ok, platform)
		require.NotEmpty(t, spec.DisplayName, platform)
		require.NotEmpty(t, spec.Gateway, platform)
	}
	require.False(t, IsConcretePlatform(PlatformComposite))
	require.True(t, IsGroupPlatform(PlatformComposite))
	for _, invalid := range []string{"", "moonshot", "Kimi", "openai ", "glm", "bogus"} {
		require.False(t, IsConcretePlatform(invalid), invalid)
		require.False(t, IsGroupPlatform(invalid), invalid)
		require.False(t, UsesOpenAIGateway(invalid), invalid)
	}
}

func TestPlatformsReturnsCopy(t *testing.T) {
	platforms := Platforms()
	platforms[0].ID = "mutated"
	require.Equal(t, PlatformAnthropic, Platforms()[0].ID)
}

// PlatformOther（`other`，通用 OpenAI 兼容自定义上游）刻意不登记进平台清单：
// 清单驱动的「所有平台」派生列表必须始终不含它，这条路保持着为真时 other 才不会对
// 上游新消费点变得可达。other 在配额侧的放行由 service.AllowedQuotaPlatforms 与
// ent/schema 的显式清单单独维护（B2 批次按 ours 现状复刻）。
func TestPlatformListExcludesOther(t *testing.T) {
	require.False(t, IsConcretePlatform(PlatformOther))
	require.False(t, IsGroupPlatform(PlatformOther))
	require.False(t, UsesOpenAIGateway(PlatformOther))
	_, ok := LookupPlatform(PlatformOther)
	require.False(t, ok)
	require.NotContains(t, ConcretePlatformIDs(), PlatformOther)
	require.NotContains(t, CompositePrecedencePlatformIDs(), PlatformOther)
}

// 我方自研平台（CodeBuddy / Muse）已在清单内：经 OpenAI 网关转发、非国产单一厂商。
func TestPlatformListIncludesForkPlatforms(t *testing.T) {
	for _, platform := range []string{PlatformCodeBuddy, PlatformMuse} {
		spec, ok := LookupPlatform(platform)
		require.True(t, ok, platform)
		require.Equal(t, PlatformGatewayOpenAI, spec.Gateway, platform)
		require.False(t, spec.CNProvider, platform)
		require.True(t, IsGroupPlatform(platform), platform)
	}
}
