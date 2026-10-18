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
