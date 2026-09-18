package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWebLoginSessionStore_CreateResolve(t *testing.T) {
	store := NewWebLoginSessionStore()
	token, expires, err := store.Create("zhipu", 42, "u@example.com", "")
	require.NoError(t, err)
	require.NotEmpty(t, token)
	require.False(t, expires.IsZero())

	sess, err := store.Resolve(token)
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Equal(t, "zhipu", sess.Platform)
	require.Equal(t, int64(42), sess.AccountID)
	require.Equal(t, "u@example.com", sess.LoginEmail)
	require.Empty(t, sess.LoginPhone)
}

func TestWebLoginSessionStore_CreateUniqueTokens(t *testing.T) {
	store := NewWebLoginSessionStore()
	t1, _, _ := store.Create("zhipu", 1, "", "13800000000")
	t2, _, _ := store.Create("kimi", 2, "", "13900000000")
	require.NotEqual(t, t1, t2)
}

func TestWebLoginSessionStore_Expired(t *testing.T) {
	store := NewWebLoginSessionStore()
	token, _, err := store.Create("kimi", 7, "", "13800000000")
	require.NoError(t, err)

	_, err = store.Resolve(token)
	require.NoError(t, err)

	// 直接驱动底层缓存过期（绕过 1 分钟清理间隔），验证哨兵错误。
	store.cache.Delete(token)
	_, err = store.Resolve(token)
	require.ErrorIs(t, err, ErrWebAutoLoginSessionExpired)

	// 空 token 也应返回过期哨兵。
	_, err = store.Resolve("")
	require.ErrorIs(t, err, ErrWebAutoLoginSessionExpired)
}

func TestWebLoginSessionStore_Delete(t *testing.T) {
	store := NewWebLoginSessionStore()
	token, _, err := store.Create("zhipu", 9, "u@example.com", "")
	require.NoError(t, err)

	store.Delete(token)
	_, err = store.Resolve(token)
	require.ErrorIs(t, err, ErrWebAutoLoginSessionExpired)
}

func TestWebLoginSessionStore_TTLIsTenMinutes(t *testing.T) {
	store := NewWebLoginSessionStore()
	token, expires, err := store.Create("deepseek", 3, "u@example.com", "")
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().Add(webAutoLoginSessionTTL), expires, 5*time.Second)
	_ = token
}
