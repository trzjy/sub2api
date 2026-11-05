package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 2026-11-05 用户裁定：`deepseek-v4.1-flash-1017` 只作上游真名，对下游只提供
// `deepseek-v4.1-flash`。本文件锁死两条入口行为：白名单准入与模型列表。

func TestRetiredModelNameRejectedByAllowlist(t *testing.T) {
	// 白名单开启：1017 即使是 flash 的别名族成员也不得准入。
	enabled := GroupModelAllowlist{Enabled: true, Models: []string{"deepseek-v4.1-flash"}}
	require.False(t, enabled.Allows("deepseek-v4.1-flash-1017"))
	require.True(t, enabled.Allows("deepseek-v4.1-flash"))

	// 白名单未开启：仍要拒绝已下线模型名（否则未配白名单的分组会漏）。
	disabled := GroupModelAllowlist{}
	require.False(t, disabled.Allows("deepseek-v4.1-flash-1017"))
	require.True(t, disabled.Allows("deepseek-v4.1-flash"))

	// 大小写变体同样拒绝。
	require.False(t, enabled.Allows("DeepSeek-V4.1-Flash-1017"))
}

func TestRetiredModelNameHiddenFromListing(t *testing.T) {
	// 白名单开启：source 里带 1017 键（账号映射键）时也不能出现在列表里。
	enabled := GroupModelAllowlist{Enabled: true, Models: []string{"deepseek-v4.1-flash"}}
	listed := enabled.FilterForListing([]string{"deepseek-v4.1-flash", "deepseek-v4.1-flash-1017"})
	require.Equal(t, []string{"deepseek-v4.1-flash"}, listed)

	// 白名单未开启：透传 source 时同样过滤。
	disabled := GroupModelAllowlist{}
	listed = disabled.FilterForListing([]string{"deepseek-v4.1-flash-1017", "deepseek-v4.1-flash"})
	require.Equal(t, []string{"deepseek-v4.1-flash"}, listed)
}

func TestAliasFamilyStillUsedForMappingLookup(t *testing.T) {
	// 别名族保留：账号映射把上游真名 1017 挂在标准名 flash 上，下游写 1017
	// 仍能解析到该映射（入口已被白名单拦下，这条只保证内部查找不回退）。
	account := &Account{
		Platform: PlatformDeepseek,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"deepseek-v4.1-flash": "deepseek-v4.1-flash-1017",
			},
		},
	}
	mapped, matched := account.ResolveMappedModel("deepseek-v4.1-flash")
	require.True(t, matched)
	require.Equal(t, "deepseek-v4.1-flash-1017", mapped)
}
