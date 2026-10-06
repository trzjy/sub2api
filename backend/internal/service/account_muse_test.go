//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMuseGetAPIProtocol 锁定 muse 平台的协议判定：OpenCode Go 上游固定
// Responses 协议（实测 chat/completions 400 ModelProtocolUnsupported），
// muse 非国产供应商但必须读 responses；分支置于 !IsCNProvider 守卫之前，
// 且不读 credentials.api_protocol（上游协议单一）。
func TestMuseGetAPIProtocol(t *testing.T) {
	t.Parallel()

	mk := func(creds map[string]any) *Account {
		if creds == nil {
			creds = map[string]any{}
		}
		return &Account{Platform: PlatformMuse, Type: AccountTypeAPIKey, Credentials: creds}
	}

	// 无协议凭证 → 固定 responses
	require.Equal(t, APIProtocolResponses, mk(nil).GetAPIProtocol(), "muse 缺失凭证固定 responses")
	// 即使显式写了其他协议值，muse 仍固定 responses（上游单一协议，覆盖 cn 守卫前返回）
	require.Equal(t, APIProtocolResponses, mk(map[string]any{"api_protocol": APIProtocolChatCompletions}).GetAPIProtocol())
	require.Equal(t, APIProtocolResponses, mk(map[string]any{"api_protocol": APIProtocolAdaptive}).GetAPIProtocol())
	require.Equal(t, APIProtocolResponses, mk(map[string]any{"api_protocol": "bogus"}).GetAPIProtocol())
}

// TestMuseIsNotCNProvider 锚定 muse 不被误判为国产供应商：包级 IsCNProvider 与
// 账号级方法均返回 false；同时确认既有 CN 集合（kimi/zhipu/deepseek/minimax）
// 因 muse 接入而不变（muse-1 禁区：绝不加入 IsCNProvider 集合）。
func TestMuseIsNotCNProvider(t *testing.T) {
	t.Parallel()

	require.False(t, IsCNProvider(PlatformMuse), "muse 不得进入 IsCNProvider 集合")
	require.False(t, (&Account{Platform: PlatformMuse}).IsCNProvider(), "muse 账号不得被判定为国产供应商")

	for _, cn := range []string{PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax} {
		require.True(t, IsCNProvider(cn), "CN 集合不应因 muse 接入而变：%s", cn)
	}
}

// TestMuseIsModelSupported 锁定 muse 的模型白名单契约：空映射 = 拒绝服务
// （与 other 同契约）；非空映射仅放行干净名→contributor ID 的映射项。
func TestMuseIsModelSupported(t *testing.T) {
	t.Parallel()

	// 空映射 = 拒绝服务（防任意模型名被原样发往 OpenCode Go 上游）
	empty := &Account{Platform: PlatformMuse, Type: AccountTypeAPIKey, Credentials: map[string]any{}}
	require.False(t, empty.IsModelSupported("muse-spark-1.3"), "muse 空映射应拒绝一切模型")
	require.False(t, empty.IsModelSupported("muse-spark-1.2"))
	require.False(t, empty.IsModelSupported("anything-else"))

	// 干净名→contributor ID 白名单
	mapping := map[string]any{
		"model_mapping": map[string]any{
			"muse-spark-1.3": "muse-spark-1.3-contributor",
			"muse-spark-1.2": "muse-spark-1.2-contributor",
		},
	}
	acc := &Account{Platform: PlatformMuse, Type: AccountTypeAPIKey, Credentials: mapping}
	require.True(t, acc.IsModelSupported("muse-spark-1.3"))
	require.True(t, acc.IsModelSupported("muse-spark-1.2"))
	require.Equal(t, "muse-spark-1.3-contributor", acc.GetMappedModel("muse-spark-1.3"), "干净名应映射为 contributor ID")
	require.Equal(t, "muse-spark-1.2-contributor", acc.GetMappedModel("muse-spark-1.2"))
	// 不在白名单的模型拒绝
	require.False(t, acc.IsModelSupported("muse-spark-9.9"))
	require.False(t, acc.IsModelSupported("gpt-5"))
}

// TestMusePassthroughDisabled 锚定 muse 不开 passthrough：IsOpenAIPassthroughEnabled
// 仅对 openai 平台开放，muse 恒为 false，因此 IsModelSupported 不会因 passthrough
// 短路放行所有模型（与 whitelist 契约不打架）。
func TestMusePassthroughDisabled(t *testing.T) {
	t.Parallel()

	acc := &Account{
		Platform: PlatformMuse,
		Type:     AccountTypeAPIKey,
		Extra:    map[string]any{"openai_passthrough": true}, // 即便误配也不生效
	}
	require.False(t, acc.IsOpenAIPassthroughEnabled(), "muse 不得启用 passthrough（仅 openai 平台）")
	require.False(t, acc.IsModelSupported("muse-spark-1.3"), "passthrough 短路不得让 muse 放行任意模型")
}
