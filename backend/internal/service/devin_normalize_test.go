package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNormalizeOpenAICompatiblePlatformKeepsDevin 钉死归一化保留 devin 原值。
//
// 回归背景：devin 分组 + devin 账号 + /v1/chat/completions 曾返回 503
// 「该分组暂无可用账号」。根因是 NormalizeOpenAICompatiblePlatform("devin")
// 落到默认分支被归为 openai，调度器 account.Platform != Normalize(req.Platform)
// 判定 platform_mismatch，devin 账号被全部排除。devin 与 codebuddy/muse/cursor
// 同组走 OpenAI 网关，必须保留原值以精确匹配账号池。
//
// devin 不在 IsMultiProtocolAPIKeyProvider（多协议 API Key 供应商）清单内，
// 故若显式 case 漏加本测试必然失败——对本次改动有真实回归保护。
func TestNormalizeOpenAICompatiblePlatformKeepsDevin(t *testing.T) {
	require.Equal(t, PlatformDevin, NormalizeOpenAICompatiblePlatform(PlatformDevin),
		"devin 必须保留原值，不得归并为 openai")
	require.NotEqual(t, PlatformOpenAI, NormalizeOpenAICompatiblePlatform(PlatformDevin))

	// 既有平台归一化行为逐位不变（回归护栏）。
	for _, p := range []string{
		PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax,
		PlatformCodeBuddy, PlatformOther, PlatformMuse, PlatformCursor,
	} {
		require.Equal(t, p, NormalizeOpenAICompatiblePlatform(p), p)
	}

	// 默认返回值不变。
	require.Equal(t, PlatformOpenAI, NormalizeOpenAICompatiblePlatform(""))
	require.Equal(t, PlatformOpenAI, NormalizeOpenAICompatiblePlatform("unknown-platform"))
}
