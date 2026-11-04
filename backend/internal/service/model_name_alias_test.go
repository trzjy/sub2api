//go:build unit

package service

// 模型名别名族的定向测试（2026-11-04 用户裁定）。
//
// 钉死三件事：
//  1. 用户定的三条归一化规则真按他说的走（点→横线、去尾部版本后缀、sonnet-5 缺版本段）；
//  2. 明确不该动的一律不动（官方日期快照名、参数后缀、不同代模型）；
//  3. 查找命中别名后，**发给上游的仍是账号里配的真名**，不会被改写成统一名。

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeModelName(t *testing.T) {
	cases := map[string]string{
		// 规则一：Claude 版本号点号 → 横线
		"claude-fable-5.1": "claude-fable-5-1",
		"claude-haiku-5.5": "claude-haiku-5-5",
		// 规则二：DeepSeek 去掉尾部版本后缀
		"deepseek-v4.1-flash-1017":  "deepseek-v4.1-flash",
		"deepseek-v4-pro-0813":      "deepseek-v4-pro",
		"deepseek-v4-pro-ga-260813": "deepseek-v4-pro",
		"deepseek-v4-1-flash":       "deepseek-v4.1-flash",
		// 规则三：缺版本段（用户确认一并归一）
		"claude-sonnet-5": "claude-sonnet-5-5",
		// 缺分隔横线
		"claude-fable5": "claude-fable-5",

		// 以下必须原样返回
		"claude-sonnet-5-5":          "claude-sonnet-5-5",
		"claude-opus-5-5":            "claude-opus-5-5",
		"claude-opus-5":              "claude-opus-5",  // 与 opus-5-5 是不同模型
		"claude-fable-5":             "claude-fable-5", // 与 fable-5-1 是不同模型
		"deepseek-v4-flash":          "deepseek-v4-flash",
		"deepseek-v4.1-flash":        "deepseek-v4.1-flash",
		"deepseek-chat":              "deepseek-chat",
		"deepseek-reasoner":          "deepseek-reasoner",
		"claude-opus-4-20250514":     "claude-opus-4-20250514", // 官方快照名，日期是名字的一部分
		"claude-sonnet-4-5-20250929": "claude-sonnet-4-5-20250929",
		"claude-haiku-4-5-20251001":  "claude-haiku-4-5-20251001",
		"claude-sonnet-5-5-high":     "claude-sonnet-5-5-high", // 参数后缀，不是版本号
		"deepseek-v4.1-flash[1M]":    "deepseek-v4.1-flash[1M]",
		"":                           "",
	}
	for input, want := range cases {
		require.Equal(t, want, NormalizeModelName(input), "input=%q", input)
	}
}

func TestModelNameAliasesOrder(t *testing.T) {
	// 输入本身必须排第一：账号里正好配了客户端原写法时，优先用它。
	aliases := ModelNameAliases("deepseek-v4.1-flash-1017")
	require.Equal(t, "deepseek-v4.1-flash-1017", aliases[0])
	require.Contains(t, aliases, "deepseek-v4.1-flash")

	// 族外名字只返回自己。
	require.Equal(t, []string{"claude-opus-5-5"}, ModelNameAliases("claude-opus-5-5"))

	// 大小写变体不重复追加。
	aliases = ModelNameAliases("Claude-Fable-5.1")
	require.Equal(t, "Claude-Fable-5.1", aliases[0])
	require.Contains(t, aliases, "claude-fable-5-1")
	for i := range aliases {
		for j := i + 1; j < len(aliases); j++ {
			require.False(t, strings.EqualFold(aliases[i], aliases[j]), "duplicate alias %q == %q", aliases[i], aliases[j])
		}
	}
}

func TestSameModelNameFamily(t *testing.T) {
	require.True(t, SameModelNameFamily("claude-fable-5.1", "claude-fable-5-1"))
	require.True(t, SameModelNameFamily("deepseek-v4.1-flash", "deepseek-v4.1-flash-1017"))
	require.True(t, SameModelNameFamily("deepseek-v4-pro", "deepseek-v4-pro-0813"))
	require.True(t, SameModelNameFamily("claude-sonnet-5", "claude-sonnet-5-5"))

	// 不同模型绝不能判成一家。
	require.False(t, SameModelNameFamily("claude-fable-5", "claude-fable-5-1"))
	require.False(t, SameModelNameFamily("claude-opus-5", "claude-opus-5-5"))
	require.False(t, SameModelNameFamily("deepseek-v4-flash", "deepseek-v4.1-flash"))
	require.False(t, SameModelNameFamily("claude-opus-5-5", "claude-sonnet-5-5"))
	require.False(t, SameModelNameFamily("claude-sonnet-5-5-high", "claude-sonnet-5-5"))
}

// 账号只配了带版本后缀的那条映射时，客户端用主干名也要能命中。
func TestAccountModelMappingMatchesAliasFamily(t *testing.T) {
	account := &Account{
		Platform: PlatformDeepseek,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"deepseek-v4.1-flash-1017": "deepseek-v4.1-flash-1017",
			},
		},
	}
	require.True(t, account.isModelMappingSupported("deepseek-v4.1-flash"),
		"账号只配了 -1017，客户端写主干名也应命中")

	// 反过来：账号只配了主干名，客户端写带后缀的也要命中。
	account = &Account{
		Platform: PlatformDeepseek,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"deepseek-v4.1-flash": "deepseek-v4.1-flash",
			},
		},
	}
	require.True(t, account.isModelMappingSupported("deepseek-v4.1-flash-1017"))

	// 不同代模型不能被别名族放行。
	account = &Account{
		Platform: PlatformDeepseek,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"deepseek-v4.1-flash": "deepseek-v4.1-flash",
			},
		},
	}
	require.False(t, account.isModelMappingSupported("deepseek-v4-flash"),
		"v4-flash 是另一代，不能被 v4.1 的别名族放行")
}

// 最关键的一条：命中别名后，发给上游的必须是账号里配的真名，不能被改写成统一名。
func TestResolveMappedModelKeepsUpstreamName(t *testing.T) {
	account := &Account{
		Platform: PlatformDeepseek,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"deepseek-v4.1-flash-1017": "hy3",
			},
		},
	}
	mapped, matched := account.ResolveMappedModel("deepseek-v4.1-flash")
	require.True(t, matched)
	require.Equal(t, "hy3", mapped, "上游真名必须原样保留")

	// 账号同时配了两个键时，客户端原写法优先，不被统一名抢走。
	account = &Account{
		Platform: PlatformDeepseek,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"deepseek-v4.1-flash":      "deepseek-v4.1-flash",
				"deepseek-v4.1-flash-1017": "deepseek-v4.1-flash-1017",
			},
		},
	}
	mapped, _ = account.ResolveMappedModel("deepseek-v4.1-flash-1017")
	require.Equal(t, "deepseek-v4.1-flash-1017", mapped)
	mapped, _ = account.ResolveMappedModel("deepseek-v4.1-flash")
	require.Equal(t, "deepseek-v4.1-flash", mapped)
}

// 白名单里写一种写法，客户端发另一种，准入必须放行。
func TestGroupModelAllowlistAllowsAliasFamily(t *testing.T) {
	allowlist := GroupModelAllowlist{
		Enabled: true,
		Models:  []string{"claude-fable-5-1", "deepseek-v4.1-flash"},
	}
	require.True(t, allowlist.Allows("claude-fable-5.1"), "点号写法应命中横线白名单")
	require.True(t, allowlist.Allows("deepseek-v4.1-flash-1017"), "带版本后缀应命中主干白名单")
	require.False(t, allowlist.Allows("claude-sonnet-5"), "白名单没列 sonnet，不应放行")

	// 白名单列的是点号写法时，横线写法同样放行。
	allowlist = GroupModelAllowlist{Enabled: true, Models: []string{"claude-fable-5.1"}}
	require.True(t, allowlist.Allows("claude-fable-5-1"))

	// 不同模型不能被放行。
	allowlist = GroupModelAllowlist{Enabled: true, Models: []string{"claude-fable-5-1"}}
	require.False(t, allowlist.Allows("claude-fable-5"))
	require.False(t, allowlist.Allows("claude-opus-5-5"))
}
