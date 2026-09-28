// cockpit_platform_map_test.go —— slug 三态表单元测试（方案 §1.2）。
package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCockpitPlatformMap_ClassifiesAllKnownSlugs 校验 §1.1 全量 20 slug 均被三态表覆盖（known=true）。
func TestCockpitPlatformMap_ClassifiesAllKnownSlugs(t *testing.T) {
	for _, slug := range KnownCockpitSlugs() {
		m, known := ClassifyCockpitSlug(slug)
		require.True(t, known, "slug %q 应被三态表覆盖", slug)
		_ = m
	}
}

// TestCockpitPlatformMap_PendingSitePlatform 校验 pending 平台的站点归属（§1.2）。
func TestCockpitPlatformMap_PendingSitePlatform(t *testing.T) {
	cases := map[string]string{
		"codebuddy_cn":      "codebuddy",
		"codebuddy":         "codebuddy",
		"workbuddy":         "codebuddy",
		"codex":             "openai",
		"codex_api_service": "openai",
		"claude_manager":    "anthropic",
		"antigravity":       "antigravity",
		"antigravity_ide":   "antigravity",
		"grok":              "grok",
	}
	for slug, want := range cases {
		m, known := ClassifyCockpitSlug(slug)
		require.True(t, known, "pending slug %q 应 known", slug)
		require.Equal(t, CockpitPlatformPending, m.State, "slug %q 应为 pending", slug)
		require.Equal(t, want, m.SitePlatform, "slug %q 站点平台应为 %q", slug, want)
	}
}

// TestCockpitPlatformMap_UnsupportedSlugs 校验 unsupported 平台（§1.2）。
func TestCockpitPlatformMap_UnsupportedSlugs(t *testing.T) {
	unsupported := []string{
		"zed", "github-copilot", "windsurf", "kiro", "cursor",
		"qoder", "zcode", "trae", "trae_solo", "trae_cn", "trae_solo_cn",
	}
	for _, slug := range unsupported {
		m, known := ClassifyCockpitSlug(slug)
		require.True(t, known, "unsupported slug %q 应 known", slug)
		require.Equal(t, CockpitPlatformUnsupported, m.State, "slug %q 应为 unsupported", slug)
		require.Empty(t, m.SitePlatform)
	}
}

// TestCockpitPlatformMap_EnabledEmpty 校验 enabled 表当前为空（§1.2 取证现状）。
func TestCockpitPlatformMap_EnabledEmpty(t *testing.T) {
	for _, slug := range KnownCockpitSlugs() {
		m, _ := ClassifyCockpitSlug(slug)
		require.NotEqual(t, CockpitPlatformEnabled, m.State, "当前 enabled 应为空，slug %q 不应为 enabled", slug)
	}
}

// TestCockpitPlatformMap_SlugLowercaseExactMatch 校验 slug 按小写精确匹配（§1.3 slug 规则）。
func TestCockpitPlatformMap_SlugLowercaseExactMatch(t *testing.T) {
	m, known := ClassifyCockpitSlug("CodeBuddy_CN")
	require.True(t, known)
	require.Equal(t, CockpitPlatformPending, m.State)
	require.Equal(t, "codebuddy", m.SitePlatform)

	// 大小写不同不属于同一 slug 白名单：未知 slug 返回 known=false。
	_, knownUnknown := ClassifyCockpitSlug("CodeBuddy_XYZ")
	require.False(t, knownUnknown)
}

// TestCockpitPlatformMap_UnknownSlug 校验 Cockpit 未来新增未知 slug 按 unsupported 处理并 known=false。
func TestCockpitPlatformMap_UnknownSlug(t *testing.T) {
	m, known := ClassifyCockpitSlug("future_new_platform")
	require.False(t, known)
	require.Equal(t, CockpitPlatformUnsupported, m.State)
	require.Empty(t, m.SitePlatform)
}

// TestCockpitPlatformMap_ThreeStateCoverage 校验三态表恰好覆盖全部 20 slug（无遗漏、无越界）。
func TestCockpitPlatformMap_ThreeStateCoverage(t *testing.T) {
	pending, unsupported, enabled := 0, 0, 0
	for _, slug := range KnownCockpitSlugs() {
		m, known := ClassifyCockpitSlug(slug)
		require.True(t, known)
		switch m.State {
		case CockpitPlatformPending:
			pending++
		case CockpitPlatformUnsupported:
			unsupported++
		case CockpitPlatformEnabled:
			enabled++
		}
	}
	require.Equal(t, 9, pending, "pending 应为 9 个")
	require.Equal(t, 11, unsupported, "unsupported 应为 11 个")
	require.Equal(t, 0, enabled, "enabled 当前应为空")
	require.Equal(t, 20, pending+unsupported, "三类合计应覆盖全部 20 slug")
}
