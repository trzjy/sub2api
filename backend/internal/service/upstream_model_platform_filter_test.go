package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFilterModelsForPlatform(t *testing.T) {
	t.Run("deepseek 平台过滤聚合套餐里的跨厂商模型", func(t *testing.T) {
		in := []string{
			"deepseek-v4-flash", "deepseek-v4-pro", "glm-5.3", "glm-5.3-flash",
			"doubao-seed-2.1-turbo", "kimi-k3", "minimax-m3",
		}
		kept, filtered, applied := filterModelsForPlatform(in, "deepseek")
		require.True(t, applied)
		require.Equal(t, []string{"deepseek-v4-flash", "deepseek-v4-pro"}, kept)
		require.Equal(t, []string{
			"doubao-seed-2.1-turbo", "glm-5.3", "glm-5.3-flash", "kimi-k3", "minimax-m3",
		}, filtered)
	})

	t.Run("kimi 平台保留 moonshot 词根", func(t *testing.T) {
		kept, _, applied := filterModelsForPlatform([]string{"kimi-k3", "moonshot-v1-8k", "deepseek-v4-pro"}, "kimi")
		require.True(t, applied)
		require.Equal(t, []string{"kimi-k3", "moonshot-v1-8k"}, kept)
	})

	t.Run("openai 词边界 o 系列", func(t *testing.T) {
		kept, _, applied := filterModelsForPlatform([]string{"gpt-5", "o3-mini", "qwen-o", "deepseek-chat"}, "openai")
		require.True(t, applied)
		require.Equal(t, []string{"gpt-5", "o3-mini"}, kept)
	})

	t.Run("other 与未知平台不过滤", func(t *testing.T) {
		in := []string{"glm-5.3", "deepseek-v4-pro"}
		kept, filtered, applied := filterModelsForPlatform(in, "other")
		require.False(t, applied)
		require.Equal(t, in, kept)
		require.Nil(t, filtered)

		kept, filtered, applied = filterModelsForPlatform(in, "")
		require.False(t, applied)
		require.Equal(t, in, kept)
		require.Nil(t, filtered)
	})

	t.Run("平台大小写归一化", func(t *testing.T) {
		kept, _, applied := filterModelsForPlatform([]string{"deepseek-chat", "glm-5.3"}, " DeepSeek ")
		require.True(t, applied)
		require.Equal(t, []string{"deepseek-chat"}, kept)
	})
}

func TestModelMatchesUpstreamPlatform(t *testing.T) {
	require.True(t, modelMatchesUpstreamPlatform("deepseek-v4.1-flash", "deepseek"))
	require.False(t, modelMatchesUpstreamPlatform("glm-5.3", "deepseek"))
	require.True(t, modelMatchesUpstreamPlatform("anything", "other"))
}
