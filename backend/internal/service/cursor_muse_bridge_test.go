//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// B9（用户裁定 2026-10-25）：muse 平台纳入 cursor 跨平台桥接。
//
// 本文件钉死桥接平台集合的口径，与 cursor_o57a_integration_test.go 的既有正负例
// 断言互为补充（muse 已从负例移到正例）。仅覆盖本文件可独立验证的部分：
//   - D1：cursorNativeBridgePlatforms 对 muse 返回 true；composite 等多协议族仍 false；
//         anthropic/gemini/grok/openai 仍 true。
//   - D2：cursorSupplyPlatformsFor 在 muse 已挂组桶中并入 PlatformCursor。

// TestCursorMuseBridgePlatforms 钉死桥接白名单：muse 纳入、composite 等多协议族不扩。
func TestCursorMuseBridgePlatforms(t *testing.T) {
	// 正向：原生 Claude/Gemini + openai 兼容族（grok/openai）+ muse（B9 新增）。
	for _, p := range []string{
		PlatformAnthropic, PlatformGemini,
		PlatformGrok, PlatformOpenAI, PlatformMuse,
	} {
		require.True(t, cursorNativeBridgePlatforms(p),
			"B9: 桥接平台 %q 必须为 true", p)
	}

	// 负向：多协议族与无内置目录平台仍不得接纳 cursor 跨平台供给。
	for _, p := range []string{
		PlatformComposite, PlatformCodeBuddy,
		PlatformOther, PlatformTypeSafe, PlatformCursor,
	} {
		require.False(t, cursorNativeBridgePlatforms(p),
			"平台 %q 不得为桥接平台（composite 等多协议族不扩）", p)
	}
}

// TestCursorMuseBridgeSupplySetIncludesCursor 钉死取回集合：muse 已挂组桶并入 cursor。
func TestCursorMuseBridgeSupplySetIncludesCursor(t *testing.T) {
	groupID := int64(107) // 派发单示例分组
	out := cursorSupplyPlatformsFor(&groupID, []string{PlatformMuse})

	require.Contains(t, out, PlatformMuse, "base 平台 muse 必须保留在查询集合")
	require.Contains(t, out, PlatformCursor, "B9: muse 已挂组桶须并入 PlatformCursor 供给")
}
