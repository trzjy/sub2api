package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTaskC_DefaultWebModelIDsModeKey 锁定 DefaultWebModelIDs 归并后 (provider, web) 双键
// 与官方平台 + access_mode=web 目录完全一致（方案 §5.6：网页接入由官方平台承载）。
func TestTaskC_DefaultWebModelIDsModeKey(t *testing.T) {
	require.Equal(t, DefaultWebModelIDs(PlatformZhipu, AccountAccessModeWeb), DefaultWebModelIDs(PlatformZhipu, AccountAccessModeWeb))
	require.Equal(t, DefaultWebModelIDs(PlatformDeepseek, AccountAccessModeWeb), DefaultWebModelIDs(PlatformDeepseek, AccountAccessModeWeb))
	require.Equal(t, DefaultWebModelIDs(PlatformKimi, AccountAccessModeWeb), DefaultWebModelIDs(PlatformKimi, AccountAccessModeWeb))

	require.Equal(t, []string{"glm-5.3-flash"}, DefaultWebModelIDs(PlatformZhipu, AccountAccessModeWeb))
	require.Equal(t, []string{"deepseek-chat", "deepseek-reasoner"}, DefaultWebModelIDs(PlatformDeepseek, AccountAccessModeWeb))
	// kimi：免费档实测可用项置前（kimi-2.6-chat / kimi-2.6 可读名，对应上游内部
	// 代号 k2d6-chat / k2d6），付费档 k3 / k3-agent-ultra 保留。
	require.Equal(t, []string{"kimi-2.6-chat", "kimi-2.6", "kimi-k3", "kimi-k3-agent-ultra"},
		DefaultWebModelIDs(PlatformKimi, AccountAccessModeWeb))

	// 双键语义不等于 api 模式：官方平台单键（无 mode）不应返回 web 目录。
	require.Nil(t, DefaultWebModelIDs(PlatformZhipu))
	require.Nil(t, DefaultWebModelIDs(PlatformDeepseek))
	require.Nil(t, DefaultWebModelIDs(PlatformKimi))

	// 未知平台 / 非 web mode 返回 nil。
	require.Nil(t, DefaultWebModelIDs(PlatformZhipu, AccountAccessModeAPI))
	require.Nil(t, DefaultWebModelIDs("nope", AccountAccessModeWeb))
}

// TestWebKimiCatalogCoversFreeTierModels 锁定 kimi web 目录不再只有单项（2026-09-22
// 线上故障）：此前只有 kimi-k3——恰恰是免费档请求会被上游以 invalid_argument 拒绝的
// 付费模型，免费档实测可用的 k2d6 / k2d6-chat 从未出现在下拉里。
func TestWebKimiCatalogCoversFreeTierModels(t *testing.T) {
	ids := DefaultWebModelIDs(PlatformKimi, AccountAccessModeWeb)

	require.GreaterOrEqual(t, len(ids), 4, "kimi web 目录应至少含 4 个公开模型")
	require.Contains(t, ids, "kimi-2.6")
	require.Contains(t, ids, "kimi-2.6-chat")
	// 付费档保留，不因补免费档而丢失。
	require.Contains(t, ids, "kimi-k3")
	require.Contains(t, ids, "kimi-k3-agent-ultra")
	// 免费档可用项置前：空 modelID 回落（resolveWebTestModel 取 ids[0]）应是可用的那个。
	require.Equal(t, "kimi-2.6-chat", ids[0])
	// 目录项均为公开名，出站归一由转发链 webKimiModelName 完成。
	for _, id := range ids {
		require.True(t, strings.HasPrefix(id, "kimi-"), "公开目录用 kimi- 前缀名，实际为 %q", id)
	}
}

// TestWebCatalogOtherPlatformsUnchanged 回归：zhipu / deepseek 目录不受 kimi 补全影响。
func TestWebCatalogOtherPlatformsUnchanged(t *testing.T) {
	require.Equal(t, []string{"glm-5.3-flash"}, DefaultWebModelIDs(PlatformZhipu, AccountAccessModeWeb))
	require.Equal(t, []string{"deepseek-chat", "deepseek-reasoner"}, DefaultWebModelIDs(PlatformDeepseek, AccountAccessModeWeb))
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

	// kimi：目录项全部放行（含 agent-ultra 内部变体），未知模型失败关闭。
	// 2026-09-22 命名修正：公开目录 kimi-2.6 系；旧公开名 kimi-k2d6 系与上游裸代号
	// k2d6 系作为兼容别名继续放行。
	require.NoError(t, ValidateWebModel(PlatformKimi, "kimi-k3"))
	require.NoError(t, ValidateWebModel(PlatformKimi, "kimi-k3-agent-ultra"))
	require.NoError(t, ValidateWebModel(PlatformKimi, "kimi-2.6"))
	require.NoError(t, ValidateWebModel(PlatformKimi, "kimi-2.6-chat"))
	require.NoError(t, ValidateWebModel(PlatformKimi, "kimi-k2d6"))
	require.NoError(t, ValidateWebModel(PlatformKimi, "kimi-k2d6-chat"))
	require.NoError(t, ValidateWebModel(PlatformKimi, "k2d6"))
	require.NoError(t, ValidateWebModel(PlatformKimi, "k2d6-chat"))
	require.Error(t, ValidateWebModel(PlatformKimi, "gpt-4"))

	// 校验集合与目录同源（DefaultWebModelIDs SSOT），不得再各自硬编码漂移。
	kimiIDs := DefaultWebModelIDs(PlatformKimi, AccountAccessModeWeb)
	require.NotEmpty(t, kimiIDs)
	for _, id := range kimiIDs {
		require.NoError(t, ValidateWebModel(PlatformKimi, id))
	}
	for _, id := range DefaultWebModelIDs(PlatformZhipu, AccountAccessModeWeb) {
		require.NoError(t, ValidateWebModel(PlatformZhipu, id))
	}
	for _, id := range DefaultWebModelIDs(PlatformDeepseek, AccountAccessModeWeb) {
		require.NoError(t, ValidateWebModel(PlatformDeepseek, id))
	}

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
