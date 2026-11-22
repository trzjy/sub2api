//go:build unit

package service

// 本文件验证会话快照的摘要还原（方案 §2.1 / 派发单 B2-S1）：
//   - 用 AppendItem 推进出主链与骨架链后，取 hex/计数，RestoreFromSnapshot 还原，
//     要求 Equal / SkeletonEqual 且计数一致，trace 被清空；
//   - 非法 hex（长度错、非 hex 字符、奇数长度、骨架链非法）必须返回 error，
//     且 digest 保持未初始化。

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCursorHistoryDigestRestore(t *testing.T) {
	t.Run("快照还原主链与骨架链", func(t *testing.T) {
		src := NewCursorHistoryDigest()
		// 含一条可裁剪正文项，使主链项数与骨架链项数不同，分别校验两条链的还原。
		steps := []struct {
			itemType  string
			canonical string
		}{
			{"chat.user.message", "hello"},
			{"messages.assistant.text", "visible body"},
			{"responses.item.function_call", `{"name":"f","arguments":{}}`},
		}
		for _, step := range steps {
			require.NoError(t, src.AppendItem(step.itemType, []byte(step.canonical)))
		}
		require.NotEqual(t, src.ItemCount(), src.SkeletonItemCount(), "测试前提：主链与骨架链项数应不同")

		mainHex := src.HexString()
		mainItems := src.ItemCount()
		skeletonHex := src.SkeletonHexString()
		skeletonItems := src.SkeletonItemCount()

		restored := &CursorHistoryDigest{} // 零值（未初始化）
		require.NoError(t, restored.RestoreFromSnapshot(mainHex, mainItems, skeletonHex, skeletonItems))

		require.True(t, restored.Equal(src), "主链摘要与项数应一致")
		require.True(t, restored.SkeletonEqual(src), "骨架链摘要与项数应一致")
		require.Equal(t, mainItems, restored.ItemCount())
		require.Equal(t, skeletonItems, restored.SkeletonItemCount())
		require.Equal(t, mainHex, restored.HexString())
		require.Equal(t, skeletonHex, restored.SkeletonHexString())
		// 还原不携带排查轨迹；清空而不是从快照臆造。
		require.Empty(t, restored.Trace())

		// 还原后的摘要可以继续追加，链值推进与原摘要一致。
		require.NoError(t, restored.AppendItem("chat.user.message", []byte("next")))
		require.NoError(t, src.AppendItem("chat.user.message", []byte("next")))
		require.True(t, restored.Equal(src), "还原后继续追加应与原链一致")
	})

	t.Run("非法hex返回错误且digest保持未初始化", func(t *testing.T) {
		validHex := NewCursorHistoryDigest().HexString() // 64 字符 / 32 字节
		require.Len(t, validHex, 64)

		cases := []struct {
			name        string
			mainHex     string
			skeletonHex string
		}{
			{"主链长度过短（31字节）", validHex[:len(validHex)-2], validHex},
			{"主链长度过长（33字节）", validHex + "00", validHex},
			{"主链奇数长度", validHex[:len(validHex)-1], validHex},
			{"主链非hex字符", strings.Repeat("z", 64), validHex},
			{"骨架链长度非法", validHex, validHex[:len(validHex)-2]},
			{"骨架链非hex字符", validHex, strings.Repeat("g", 64)},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				d := &CursorHistoryDigest{}
				err := d.RestoreFromSnapshot(tc.mainHex, 1, tc.skeletonHex, 1)
				require.Error(t, err, "非法 hex 必须返回明确错误")
				require.False(t, d.initialized, "失败后 digest 必须保持未初始化")
				require.Zero(t, d.ItemCount())
				require.Zero(t, d.SkeletonItemCount())
			})
		}
	})

	t.Run("nil接收者返回错误", func(t *testing.T) {
		var d *CursorHistoryDigest
		validHex := NewCursorHistoryDigest().HexString()
		require.Error(t, d.RestoreFromSnapshot(validHex, 0, validHex, 0))
	})
}
