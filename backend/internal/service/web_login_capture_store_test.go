package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWebLoginCaptureStore_CreateResolve(t *testing.T) {
	store := NewWebLoginCaptureStore()
	token, expires, err := store.Create("web-zhipu")
	require.NoError(t, err)
	require.NotEmpty(t, token)
	require.False(t, expires.IsZero())

	entry, err := store.Resolve(token)
	require.NoError(t, err)
	require.NotNil(t, entry)
	require.Equal(t, "web-zhipu", entry.Platform)
	require.Empty(t, entry.Cookie)
}

func TestWebLoginCaptureStore_CreateRejectsUnknownPlatformStillStores(t *testing.T) {
	// Create 不校验 platform 合法性（仅 handler 校验），这里仅验证 token 唯一性。
	store := NewWebLoginCaptureStore()
	t1, _, _ := store.Create("web-zhipu")
	t2, _, _ := store.Create("web-deepseek")
	require.NotEqual(t, t1, t2)
}

func TestWebLoginCaptureStore_Expired(t *testing.T) {
	store := NewWebLoginCaptureStore()
	token, _, err := store.Create("web-kimi")
	require.NoError(t, err)

	// 未过期前应可解析。
	_, err = store.Resolve(token)
	require.NoError(t, err)

	// 直接驱动底层缓存过期（绕过 1 分钟清理间隔），验证哨兵错误。
	store.cache.Delete(token)
	_, err = store.Resolve(token)
	require.ErrorIs(t, err, ErrWebLoginSessionExpired)

	// 空 token 也应返回过期哨兵。
	_, err = store.Resolve("")
	require.ErrorIs(t, err, ErrWebLoginSessionExpired)
}

func TestWebLoginCaptureStore_SetCookieOverwrite(t *testing.T) {
	store := NewWebLoginCaptureStore()
	token, _, err := store.Create("web-deepseek")
	require.NoError(t, err)

	require.NoError(t, err)

	store.SetCookie(token, "ds_session_id=abc123; other=1")
	cookie, ok := store.Cookie(token)
	require.True(t, ok)
	require.Equal(t, "ds_session_id=abc123; other=1", cookie)

	// 覆盖写入。
	store.SetCookie(token, "ds_session_id=def456")
	cookie, ok = store.Cookie(token)
	require.True(t, ok)
	require.Equal(t, "ds_session_id=def456", cookie)

	// 不存在的 token 写入应静默忽略，读取返回 false。
	store.SetCookie("nope", "x=1")
	_, ok = store.Cookie("nope")
	require.False(t, ok)
}

func TestWebLoginCaptureStore_TouchExtends(t *testing.T) {
	store := NewWebLoginCaptureStore()
	token, expires, err := store.Create("web-zhipu")
	require.NoError(t, err)

	// 等待极短时间后 Touch，过期时间应被显著推后。
	time.Sleep(20 * time.Millisecond)
	store.Touch(token)
	entry, err := store.Resolve(token)
	require.NoError(t, err)
	require.True(t, entry.ExpiresAt.After(expires.Add(10*time.Millisecond)),
		"Touch should push expiry forward, got %v vs %v", entry.ExpiresAt, expires)
}

func TestWebLoginCaptureStore_Delete(t *testing.T) {
	store := NewWebLoginCaptureStore()
	token, _, err := store.Create("web-zhipu")
	require.NoError(t, err)

	store.Delete(token)
	_, ok := store.Cookie(token)
	require.False(t, ok)

	_, err = store.Resolve(token)
	require.ErrorIs(t, err, ErrWebLoginSessionExpired)
}

func TestBuildUpstreamURL(t *testing.T) {
	tests := []struct {
		name     string
		platform string
		path     string
		query    string
		wantErr  bool
		want    string
	}{
		{"zhipu ok", "web-zhipu", "/", "", false, "https://chatglm.cn/"},
		{"deepseek ok with query", "web-deepseek", "/login", "a=1", false, "https://chat.deepseek.com/login?a=1"},
		{"kimi ok", "web-kimi", "/chat", "", false, "https://www.kimi.com/chat"},
		{"unknown platform", "web-x", "/", "", true, ""},
		{"path not leading slash", "web-zhipu", "foo", "", true, ""},
		{"traversal", "web-zhipu", "/../etc", "", true, ""},
		{"nested traversal", "web-zhipu", "/a/b/../c", "", true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, err := BuildUpstreamURL(tc.platform, tc.path, tc.query)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, u.String())
		})
	}
}

func TestWebLoginProxyRedirectAllowlist(t *testing.T) {
	wl := WebLoginProxyRedirectAllowlist()
	require.True(t, wl["chatglm.cn"])
	require.True(t, wl["chat.deepseek.com"])
	require.True(t, wl["www.kimi.com"])
	require.True(t, wl["platform.deepseek.com"])
	require.Len(t, wl, 4)
	require.False(t, wl["evil.com"])
}

func TestWebLoginProxyPlatformInfo(t *testing.T) {
	origin, cookie, ok := WebLoginProxyPlatformInfo("web-zhipu")
	require.True(t, ok)
	require.Equal(t, "https://chatglm.cn", origin)
	require.Equal(t, "chatglm_token", cookie)

	origin, cookie, ok = WebLoginProxyPlatformInfo("web-kimi")
	require.True(t, ok)
	require.Equal(t, "https://www.kimi.com", origin)
	require.Equal(t, "", cookie)

	_, _, ok = WebLoginProxyPlatformInfo("nope")
	require.False(t, ok)
}

func TestWebLoginProxyAllowedCookieNames(t *testing.T) {
	require.Equal(t, []string{"chatglm_token", "chatglm_refresh_token", "chatglm_user_id"},
		WebLoginProxyAllowedCookieNames("web-zhipu"))
	require.Equal(t, []string{"ds_session_id"},
		WebLoginProxyAllowedCookieNames("web-deepseek"))
	// kimi 白名单为空：不从入站 Cookie 提取（仅手动粘贴 Token JSON）。
	require.Empty(t, WebLoginProxyAllowedCookieNames("web-kimi"))
	// 未知平台返回 nil（handler 不得据此捕获任何入站 Cookie）。
	require.Nil(t, WebLoginProxyAllowedCookieNames("nope"))
}
