//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDeepSeekModelNamesUnifiedForDownstream 锁死 2026-11-12 用户裁定：
// DeepSeek 家族对下游一律呈现官方现行调用名（deepseek-flash / deepseek-v4-pro），
// 上游目录里的版本后缀写法（-1017 / -0813）不外露。
//
// 官方依据（api-docs.deepseek.com/quick_start/pricing）：
// 现行调用名只有 deepseek-flash 与 deepseek-v4-pro；
// DeepSeek-V4.1-Flash-1017 与 DeepSeek-V4-Pro-0813 是**版本号**，不是模型名。
func TestDeepSeekModelNamesUnifiedForDownstream(t *testing.T) {
	// 对外展示名：全部写法归到官方现行调用名。
	for _, name := range []string{
		"deepseek-v4.1-flash",
		"deepseek-v4.1-flash-1017",
		"deepseek-v4-1-flash",
		"deepseek-flash",
		"DeepSeek-Flash",
	} {
		require.Equal(t, "deepseek-flash", PublicModelName(name), "name=%q", name)
	}
	for _, name := range []string{
		"deepseek-v4-pro",
		"deepseek-v4-pro-0813",
		"deepseek-v4-pro-ga-260813",
	} {
		require.Equal(t, "deepseek-v4-pro", PublicModelName(name), "name=%q", name)
	}

	// 上游真名必须被封闭在对外展示之外（不能出现在列表/候选里）。
	require.Equal(t, "deepseek-v4.1-flash", NormalizeModelName("deepseek-v4.1-flash-1017"))
	require.Equal(t, "deepseek-v4-pro", NormalizeModelName("deepseek-v4-pro-0813"))
}

// TestGroupModelAllowlistCandidatesUsePublicName 锁死候选列表口径：
// 「实供候选」必须与「已保存规则」用同一个 PublicModelName，
// 不再出现"已保存规则是 deepseek-flash、实供候选却是 deepseek-v4.1-flash-1017"。
func TestGroupModelAllowlistCandidatesUsePublicName(t *testing.T) {
	// 已保存规则侧（presentModelNamesForListing）：旧名折成对外名。
	// 注意 deepseek-v4.1-flash-1017 是已下线名（上游真名），会被先过滤掉，
	// 只留站内写法 deepseek-v4.1-flash 折成的对外名。
	saved := presentModelNamesForListing([]string{
		"deepseek-v4.1-flash",
		"deepseek-v4-pro-0813",
	})
	require.Equal(t, []string{"deepseek-flash", "deepseek-v4-pro"}, saved)

	// 上游真名确实被封闭在对外展示之外。
	require.Empty(t, presentModelNamesForListing([]string{"deepseek-v4.1-flash-1017"}))

	// 候选侧走同一个函数（admin_group_model_candidates.go 的 add() 用 PublicModelName）。
	require.Equal(t, "deepseek-flash", PublicModelName("deepseek-v4.1-flash"))
	require.Equal(t, "deepseek-flash", PublicModelName("deepseek-v4.1-flash-1017"))
	require.Equal(t, "deepseek-v4-pro", PublicModelName("deepseek-v4-pro"))
	require.Equal(t, "deepseek-v4-pro", PublicModelName("deepseek-v4-pro-0813"))
}
