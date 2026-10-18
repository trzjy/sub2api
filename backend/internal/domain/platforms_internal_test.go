package domain

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 第一组：明确登记的内部供给平台为 true。
func TestIsInternalSupplyPlatformTrueIDs(t *testing.T) {
	for _, id := range []string{PlatformCodeBuddy, PlatformMuse, PlatformCursor} {
		require.True(t, IsInternalSupplyPlatform(id), id)
	}
}

// 第二组：非内部供给平台（国产厂商、外部上游、composite、空串与近似串）均为 false。
func TestIsInternalSupplyPlatformFalseIDs(t *testing.T) {
	for _, id := range []string{
		PlatformAnthropic, PlatformOpenAI, PlatformGemini, PlatformAntigravity,
		PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax,
		PlatformOpenCodeGo, PlatformTypeSafe, PlatformCommandCode, PlatformCline,
		PlatformComposite, "", "cursorx",
	} {
		require.False(t, IsInternalSupplyPlatform(id), id)
	}
}

// 第三组：Platforms() 返回的全部登记 ID——codebuddy / muse / cursor 为 true、其余
// 为 false。把「新增平台需同步改谓词」这条耦合显式化：未来登记新内部平台时该断言
// 会失败，提示同步更新 IsInternalSupplyPlatform。
func TestIsInternalSupplyPlatformAllRegistered(t *testing.T) {
	expectedInternal := map[string]bool{
		PlatformCodeBuddy: true,
		PlatformMuse:      true,
		PlatformCursor:    true,
	}
	for _, spec := range Platforms() {
		want, ok := expectedInternal[spec.ID]
		if !ok {
			want = false
		}
		require.Equal(t, want, IsInternalSupplyPlatform(spec.ID), spec.ID)
	}
}
