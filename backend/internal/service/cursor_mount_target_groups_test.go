//go:build unit

package service

// P1-B2「按模型平台挂组」(U7) 定向单测：cursorMountTargetGroups 是手动挂组
// （findCursorPlatformGroups）与自动补池（ListAllowedModels）共用的唯一挂载判定出口，
// 此处以纯内存夹具钉死其三条件：平台、白名单启用（有对应模型）、静态实供清单覆盖。
//
// 覆盖目标（Done when 2）：聚合/福利组不被自动挂入——
//   - allow-all（白名单未启用，Allows 恒真）的聚合组：无对应模型 → 不挂；
//   - 仅含"cursor"字样、不含任何实供模型的福利组：不挂；
//   - 平台非模型所属平台（如 anthropic 专属组）即便白名单覆盖实供模型：不挂。

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// cursorMountGroup 构造挂载判定夹具（不连库）。
func cursorMountGroup(id int64, platform string, al domain.GroupModelAllowlist) *ent.Group {
	return &ent.Group{ID: id, Platform: platform, ModelAllowlist: al}
}

// cursorMountIDs 提取判定结果的分组 ID 序列（保序）。
func cursorMountIDs(groups []*ent.Group) []int64 {
	out := make([]int64, 0, len(groups))
	for _, g := range groups {
		out = append(out, g.ID)
	}
	return out
}

// TestCursorMountTargetGroups_PlatformScoped 钉死 U7 三条件逐条命中/不命中。
func TestCursorMountTargetGroups_PlatformScoped(t *testing.T) {
	entry := CursorStaticEntryModels()
	require.NotEmpty(t, entry)

	groups := []*ent.Group{
		// 1. cursor 专属组 + 白名单启用 + 覆盖实供模型 → 挂（正例）。
		cursorMountGroup(1, PlatformCursor, domain.GroupModelAllowlist{
			Enabled: true, Models: []string{"grok-4.7"},
		}),
		// 2. composite 聚合组 + 白名单启用 + 覆盖实供模型 → 挂（生产形态）。
		cursorMountGroup(2, PlatformComposite, domain.GroupModelAllowlist{
			Enabled: true, Models: []string{"gpt-6-sol"},
		}),
		// 3. composite 聚合组 + 白名单未启用（allow-all，Allows 恒真）→ 不挂（无对应模型）。
		cursorMountGroup(3, PlatformComposite, domain.GroupModelAllowlist{Enabled: false}),
		// 4. composite 聚合组 + 白名单启用但只含"cursor"字样 → 不挂（无任何实供模型）。
		cursorMountGroup(4, PlatformComposite, domain.GroupModelAllowlist{
			Enabled: true, Models: []string{"cursor"},
		}),
		// 5. 福利组形态：白名单启用但只覆盖非实供模型 → 不挂。
		cursorMountGroup(5, PlatformComposite, domain.GroupModelAllowlist{
			Enabled: true, Models: []string{"deepseek-v4-1-flash"},
		}),
		// 6. 平台非模型所属平台（anthropic 专属组）即便覆盖实供模型 → 不挂。
		cursorMountGroup(6, domain.PlatformAnthropic, domain.GroupModelAllowlist{
			Enabled: true, Models: []string{"grok-4.7"},
		}),
		// 7. 平台为空（脏数据）→ 不挂。
		cursorMountGroup(7, "", domain.GroupModelAllowlist{
			Enabled: true, Models: []string{"grok-4.7"},
		}),
		nil, // 8. nil 分组不得 panic。
	}

	got := cursorMountTargetGroups(groups, entry)
	require.Equal(t, []int64{1, 2}, cursorMountIDs(got),
		"only cursor/composite groups with an enabled allowlist covering the static supply list may be mounted")
}

// TestCursorMountTargetGroups_EmptyEntryModels 钉死"无模型上下文 → 零目标"：
// 静态清单为空（不应发生，防御性）时不得退化为"挂一切"。
func TestCursorMountTargetGroups_EmptyEntryModels(t *testing.T) {
	groups := []*ent.Group{
		cursorMountGroup(1, PlatformComposite, domain.GroupModelAllowlist{Enabled: false}),
		cursorMountGroup(2, PlatformCursor, domain.GroupModelAllowlist{
			Enabled: true, Models: []string{"grok-4.7"},
		}),
	}
	require.Empty(t, cursorMountTargetGroups(groups, nil), "empty entry model list must yield no mount target")
}

// TestCursorMountTargetGroups_ProductionMirror 用生产真实分组白名单(2026-10-21 快照)断言挂组目标集。
// 这是本次 U7 收敛修复的端到端判据：原实现只认 platform=cursor/composite，导致原生平台组
// (anthropic/openai/gemini/grok) 即便白名单覆盖实供模型也挂不上，手动拉号恒报
// ErrQuotaKeyModelNoCoverage。修复后目标集 = [34,37,82,86,114]（107 是 allow-all 被跳过，
// 94/110 是 composite 且白名单不含实供模型故不挂）。
func TestCursorMountTargetGroups_ProductionMirror(t *testing.T) {
	entry := CursorStaticEntryModels()
	require.NotEmpty(t, entry)

	// 白名单取生产 2026-10-21 快照（子集，足以定边界）。
	groups := []*ent.Group{
		// 34 claude-max(满血) anthropic 启用 -> claude-opus-5-5/claude-fable-5.1/claude-sonnet-5-5 命中
		cursorMountGroup(34, domain.PlatformAnthropic, domain.GroupModelAllowlist{
			Enabled: true,
			Models:  []string{"claude-fable-5", "claude-opus-5-5", "claude-sonnet-5-5", "claude-fable-5.1"},
		}),
		// 37 gpt-max 满血 openai 启用 -> gpt-6-sol/gpt-6-astra/gpt-6.1-sol/gpt-5.6-sol 命中
		cursorMountGroup(37, domain.PlatformOpenAI, domain.GroupModelAllowlist{
			Enabled: true,
			Models:  []string{"gpt-5.6-sol", "gpt-6-sol", "gpt-6-astra", "gpt-6.1-sol"},
		}),
		// 82 GPT-特价 openai 启用 -> 同上
		cursorMountGroup(82, domain.PlatformOpenAI, domain.GroupModelAllowlist{
			Enabled: true,
			Models:  []string{"gpt-5.6-sol", "gpt-6-sol", "gpt-6-astra", "gpt-6.1-sol"},
		}),
		// 86 Gemini 官方满血 gemini 启用 -> gemini-3.8-flash 命中
		cursorMountGroup(86, domain.PlatformGemini, domain.GroupModelAllowlist{
			Enabled: true,
			Models:  []string{"gemini-3.1-pro-preview", "gemini-3.8-flash"},
		}),
		// 107 muse-spark muse allow-all(enabled=false) -> 不挂
		cursorMountGroup(107, domain.PlatformMuse, domain.GroupModelAllowlist{Enabled: false, Models: []string{"muse-spark-1.3"}}),
		// 110 聚合分组 composite 启用 但只含 deepseek/kimi/glm/space-bunny -> 不挂
		cursorMountGroup(110, domain.PlatformComposite, domain.GroupModelAllowlist{
			Enabled: true,
			Models:  []string{"deepseek-v4.1-flash", "glm-5.3-flash", "kimi-k3", "space-bunny", "glm-5.3"},
		}),
		// 94 限时福利 composite allow-all -> 不挂
		cursorMountGroup(94, domain.PlatformComposite, domain.GroupModelAllowlist{Enabled: false}),
		// 114 grok-max grok 启用 -> grok-4.6/grok-4.7 命中
		cursorMountGroup(114, domain.PlatformGrok, domain.GroupModelAllowlist{
			Enabled: true,
			Models:  []string{"grok-4.6", "grok-4.7"},
		}),
	}

	got := cursorMountTargetGroups(groups, entry)
	require.Equal(t, []int64{34, 37, 82, 86, 114}, cursorMountIDs(got),
		"production native platform groups (anthropic/openai/gemini/grok) must carry cursor accounts per U7")
}

// TestCursorMountTargetGroups_AllowAllNeverAutoMounted 是 Done when 2 的核心反例：
// allow-all 聚合组（Allows 恒真）恒真判入已收敛——未启用白名单 = 无对应模型，不挂。
func TestCursorMountTargetGroups_AllowAllNeverAutoMounted(t *testing.T) {
	// 单个 allow-all 聚合组：Allows 对任意模型恒真，但不得因此被自动挂入。
	allowAll := []*ent.Group{
		cursorMountGroup(1, PlatformComposite, domain.GroupModelAllowlist{Enabled: false}),
	}
	require.Empty(t, cursorMountTargetGroups(allowAll, CursorStaticEntryModels()),
		"allow-all (whitelist-disabled) group must never be auto-mounted (no corresponding model)")

	// 福利组：白名单启用但只覆盖非实供模型（如福利额度模型），同样不挂。
	welfare := []*ent.Group{
		cursorMountGroup(2, PlatformComposite, domain.GroupModelAllowlist{
			Enabled: true, Models: []string{"deepseek-v4-1-flash"},
		}),
	}
	require.Empty(t, cursorMountTargetGroups(welfare, CursorStaticEntryModels()),
		"welfare group covering no real-supply model must not be auto-mounted")
}

// TestCursorMountTargetGroups_WildcardCoverageCounts 钉死通配语义沿用：白名单以通配
// （如 "gpt-*"）覆盖静态清单模型时同样构成覆盖（沿用既有 Allows 前缀/通配匹配）。
func TestCursorMountTargetGroups_WildcardCoverageCounts(t *testing.T) {
	groups := []*ent.Group{
		cursorMountGroup(1, PlatformComposite, domain.GroupModelAllowlist{
			Enabled: true, Models: []string{"gpt-*"},
		}),
	}
	require.Equal(t, []int64{1}, cursorMountIDs(cursorMountTargetGroups(groups, CursorStaticEntryModels())),
		"wildcard allowlist covering static supply models counts as coverage")
}
