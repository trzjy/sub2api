package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTaskC_DefaultWebModelIDsModeKey 锁定 DefaultWebModelIDs 归并后 (provider, web) 双键
// 与旧 web-* 平台目录完全一致（方案 §5.6：web-zhipu→zhipu+web 等同）。
func TestTaskC_DefaultWebModelIDsModeKey(t *testing.T) {
	require.Equal(t, DefaultWebModelIDs(PlatformWebZhipu), DefaultWebModelIDs(PlatformZhipu, AccountAccessModeWeb))
	require.Equal(t, DefaultWebModelIDs(PlatformWebDeepseek), DefaultWebModelIDs(PlatformDeepseek, AccountAccessModeWeb))
	require.Equal(t, DefaultWebModelIDs(PlatformWebKimi), DefaultWebModelIDs(PlatformKimi, AccountAccessModeWeb))

	require.Equal(t, []string{"glm-5.3-flash"}, DefaultWebModelIDs(PlatformZhipu, AccountAccessModeWeb))
	require.Equal(t, []string{"deepseek-chat", "deepseek-reasoner"}, DefaultWebModelIDs(PlatformDeepseek, AccountAccessModeWeb))
	require.Equal(t, []string{"kimi-k3"}, DefaultWebModelIDs(PlatformKimi, AccountAccessModeWeb))

	// 双键语义不等于 api 模式：官方平台单键（无 mode）不应返回 web 目录。
	require.Nil(t, DefaultWebModelIDs(PlatformZhipu))
	require.Nil(t, DefaultWebModelIDs(PlatformDeepseek))
	require.Nil(t, DefaultWebModelIDs(PlatformKimi))

	// 未知平台 / 非 web mode 返回 nil。
	require.Nil(t, DefaultWebModelIDs(PlatformZhipu, AccountAccessModeAPI))
	require.Nil(t, DefaultWebModelIDs("nope", AccountAccessModeWeb))
}

// TestTaskC_ValidateWebModelFailClosed 锁定 Web 账号未知模型失败关闭（方案 §5.6 B4）：
// 仅允许默认目录内模型，不回落 API 模型目录、不猜测。
func TestTaskC_ValidateWebModelFailClosed(t *testing.T) {
	// zhipu
	require.NoError(t, ValidateWebModel(PlatformZhipu, "glm-5.3-flash"))
	require.Error(t, ValidateWebModel(PlatformZhipu, "gpt-4"))
	require.Error(t, ValidateWebModel(PlatformZhipu, ""))

	// deepseek
	require.NoError(t, ValidateWebModel(PlatformDeepseek, "deepseek-chat"))
	require.NoError(t, ValidateWebModel(PlatformDeepseek, "deepseek-reasoner"))
	require.Error(t, ValidateWebModel(PlatformDeepseek, "deepseek-v4-pro")) // API 目录模型，不得误放行
	require.Error(t, ValidateWebModel(PlatformDeepseek, "gpt-4"))

	// kimi（含 adapter 内部变体 kimi-k3-agent-ultra）
	require.NoError(t, ValidateWebModel(PlatformKimi, "kimi-k3"))
	require.NoError(t, ValidateWebModel(PlatformKimi, "kimi-k3-agent-ultra"))
	require.Error(t, ValidateWebModel(PlatformKimi, "gpt-4"))

	// 旧 ValidateWebZhipuModel 行为不变（兼容只读路径）。
	require.NoError(t, ValidateWebZhipuModel("glm-5.3-flash"))
	require.Error(t, ValidateWebZhipuModel("bad"))
}

// TestTaskC_MergeAndDedupModelIDs 锁定 /models 同名模型去重单条目（方案 §5.6 B4）：
// Web 账号与 API 账号同组时，deepseek-chat / deepseek-reasoner 等同名 ID 只出现一次。
func TestTaskC_MergeAndDedupModelIDs(t *testing.T) {
	// API 目录与 Web 目录含同名模型 → 并集去重单条目。
	apiModels := []string{"deepseek-chat", "deepseek-reasoner", "deepseek-v4-pro"}
	webModels := DefaultWebModelIDs(PlatformDeepseek, AccountAccessModeWeb) // deepseek-chat, deepseek-reasoner
	merged := MergeAndDedupModelIDs(apiModels, webModels)
	require.Len(t, merged, 3)
	require.ElementsMatch(t, []string{"deepseek-chat", "deepseek-reasoner", "deepseek-v4-pro"}, merged)
	require.Equal(t, "deepseek-chat", merged[0], "first occurrence order preserved")

	// 多源重复名：只保留首条。
	require.Equal(t, []string{"a", "b", "c"}, MergeAndDedupModelIDs(
		[]string{"a", "b"}, []string{"a", "c"}, []string{"b", "a"}))

	// 空源安全。
	require.Empty(t, MergeAndDedupModelIDs())
	require.Equal(t, []string{"x"}, MergeAndDedupModelIDs(nil, []string{"x"}, nil))
}
