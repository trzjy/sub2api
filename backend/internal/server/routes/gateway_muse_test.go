package routes

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// muse 平台必须注册进 OpenAI 网关转发集合（照 PlatformMiniMax 同位置）；
// 且不得被当作国产供应商（不命中 CN provider 专属分支）。
func TestMuseRegisteredAsOpenAICompatibleGatewayPlatform(t *testing.T) {
	_, ok := openAICompatibleGatewayPlatforms[service.PlatformMuse]
	require.True(t, ok, "muse 必须注册进 openAICompatibleGatewayPlatforms")
	require.True(t, isOpenAICompatibleGatewayPlatform(service.PlatformMuse),
		"isOpenAICompatibleGatewayPlatform(muse) 必须为 true")

	// muse 非国产供应商：不得命中 CN provider 专属分支（muse-1 已保证，此处锚定）。
	require.False(t, service.IsCNProvider(service.PlatformMuse),
		"muse 不得进入 IsCNProvider 集合（不命中 CN provider 分支）")
}
