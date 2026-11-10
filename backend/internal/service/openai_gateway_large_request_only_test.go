//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsLargeRequestOnlyAccount(t *testing.T) {
	t.Run("bool true → true", func(t *testing.T) {
		a := &Account{Extra: map[string]any{LargeRequestOnlyExtraKey: true}}
		require.True(t, IsLargeRequestOnlyAccount(a))
	})

	t.Run("bool false → false", func(t *testing.T) {
		a := &Account{Extra: map[string]any{LargeRequestOnlyExtraKey: false}}
		require.False(t, IsLargeRequestOnlyAccount(a))
	})

	t.Run("字符串 true/1 → true", func(t *testing.T) {
		require.True(t, IsLargeRequestOnlyAccount(&Account{Extra: map[string]any{LargeRequestOnlyExtraKey: "true"}}))
		require.True(t, IsLargeRequestOnlyAccount(&Account{Extra: map[string]any{LargeRequestOnlyExtraKey: "1"}}))
	})

	t.Run("四种非真值 → 均 false", func(t *testing.T) {
		require.False(t, IsLargeRequestOnlyAccount(&Account{Extra: map[string]any{LargeRequestOnlyExtraKey: "false"}}))
		require.False(t, IsLargeRequestOnlyAccount(&Account{Extra: map[string]any{LargeRequestOnlyExtraKey: "0"}}))
		// 数值 1 是非 bool 非上述字符串，按零影响处理 → false
		require.False(t, IsLargeRequestOnlyAccount(&Account{Extra: map[string]any{LargeRequestOnlyExtraKey: 1}}))
		// 其它字符串
		require.False(t, IsLargeRequestOnlyAccount(&Account{Extra: map[string]any{LargeRequestOnlyExtraKey: "yes"}}))
	})

	t.Run("缺键 → false", func(t *testing.T) {
		require.False(t, IsLargeRequestOnlyAccount(&Account{Extra: map[string]any{"other": true}}))
		require.False(t, IsLargeRequestOnlyAccount(&Account{Extra: map[string]any{}}))
		require.False(t, IsLargeRequestOnlyAccount(&Account{}))
	})

	t.Run("nil 账号 → false", func(t *testing.T) {
		require.False(t, IsLargeRequestOnlyAccount(nil))
	})

	t.Run("nil Extra → false", func(t *testing.T) {
		require.False(t, IsLargeRequestOnlyAccount(&Account{Extra: nil}))
	})
}

func TestLargeRequestOnlyAccountIDs(t *testing.T) {
	t.Run("混合集合只挑出启用者", func(t *testing.T) {
		accounts := []Account{
			{ID: 1, Extra: map[string]any{LargeRequestOnlyExtraKey: true}},
			{ID: 2, Extra: map[string]any{LargeRequestOnlyExtraKey: false}},
			{ID: 3, Extra: map[string]any{}},
			{ID: 4, Extra: map[string]any{LargeRequestOnlyExtraKey: "true"}},
			{ID: 5, Extra: map[string]any{LargeRequestOnlyExtraKey: "1"}},
			{ID: 6, Extra: map[string]any{LargeRequestOnlyExtraKey: "false"}},
			{ID: 7, Extra: nil},
		}
		ids := largeRequestOnlyAccountIDs(accounts)
		require.Equal(t, map[int64]struct{}{1: {}, 4: {}, 5: {}}, ids)
	})

	t.Run("空输入返回空集合", func(t *testing.T) {
		require.Empty(t, largeRequestOnlyAccountIDs(nil))
		require.Empty(t, largeRequestOnlyAccountIDs([]Account{}))
	})
}
