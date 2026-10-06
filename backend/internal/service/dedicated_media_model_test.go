package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestIsCodexDedicatedMediaModel_Hunyuan 回归"能力元数据假告警"根因：
// CodeBuddy 上游的图像生成模型 hunyuan-image-v3.0 必须被识别为专属媒体模型，
// 从而排除出"上游模型能力元数据完整性检查"（它本就没有 context_window），
// 否则每次同步都会误报 "remaining models are still incomplete"。
func TestIsCodexDedicatedMediaModel_Hunyuan(t *testing.T) {
	// 图像/视频/3D 生成模型 → 视为专属媒体（排除出能力完整性检查）
	require.True(t, isCodexDedicatedMediaModel("hunyuan-image-v3.0"), "腾讯 Hunyuan 图像生成模型")
	require.True(t, isCodexDedicatedMediaModel("hunyuan-video-v1.0"), "腾讯 Hunyuan 视频生成模型")
	require.True(t, isCodexDedicatedMediaModel("models/hunyuan-image-v3.0"), "带 models/ 前缀")
	require.True(t, isCodexDedicatedMediaModel("seedream-3.0"), "字节 Seedream 图像生成")
	require.True(t, isCodexDedicatedMediaModel("gpt-image-1"), "OpenAI 图像生成（既有识别）")

	// 普通对话/推理模型 → 不得误判为媒体（否则会从 Codex 目录被错误剔除）
	require.False(t, isCodexDedicatedMediaModel("deepseek-v4-flash"))
	require.False(t, isCodexDedicatedMediaModel("deepseek-v4.1-flash"))
	require.False(t, isCodexDedicatedMediaModel("glm-5.3"))
	require.False(t, isCodexDedicatedMediaModel("kimi-k2.5"))
	require.False(t, isCodexDedicatedMediaModel("gpt-5.6-sol"))
	require.False(t, isCodexDedicatedMediaModel("auto"))
}

// TestUpstreamModelMetadataIncompleteIgnoresMediaModels 验证：即便某个媒体模型元数据
// 缺 context_window，也不再需要"注册表补齐"（即不再产生假告警的判定来源）。
func TestUpstreamModelMetadataIncompleteIgnoresMediaModels(t *testing.T) {
	metadata := map[string]UpstreamModelMetadata{
		"hunyuan-image-v3.0": {ID: "hunyuan-image-v3.0", Reasoning: boolPtr(false), InputModalities: []string{"text"}},
	}
	// 直接判定该模型自身不完整（缺 context_window）
	require.False(t, upstreamModelMetadataIsComplete(metadata["hunyuan-image-v3.0"]))
	// 但经 capabilitySyncModelIDs 过滤后（媒体模型被排除），能力检查不应再要求注册表补齐
	ids := capabilitySyncModelIDs([]string{"hunyuan-image-v3.0"})
	require.Empty(t, ids, "媒体生成模型应被排除出能力完整性检查")
	require.False(t, upstreamCatalogNeedsRegistry(ids, metadata),
		"媒体模型缺 context_window 不得触发假告警")
}
