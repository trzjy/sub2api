package service

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCodeBuddyTokenRefresher_CanRefresh(t *testing.T) {
	r := NewCodeBuddyTokenRefresher(NewCodeBuddyOAuthService(nil))

	require.True(t, r.CanRefresh(&Account{Platform: PlatformCodeBuddy, Type: AccountTypeOAuth}),
		"codebuddy oauth account should be refreshable")

	require.False(t, r.CanRefresh(&Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}),
		"non-codebuddy platform must not be refreshable")
	require.False(t, r.CanRefresh(&Account{Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey}),
		"non-oauth codebuddy account must not be refreshable")

	// 方案 G3/分发跟随母账号：codebuddy 影子 platform=目标分组平台（非 codebuddy），
	// 不能成为刷新候选——其凭证由母账号刷新并透传（测试/刷新路径防护）。
	shadow := &Account{
		ID:               7,
		Platform:         PlatformDeepseek,
		Type:             AccountTypeOAuth,
		ParentAccountID:  ptrI64(5),
		QuotaDimension:   QuotaDimensionCodeBuddy,
	}
	require.False(t, r.CanRefresh(shadow),
		"codebuddy shadow must not be a refresh candidate")
}

func TestCodeBuddyTokenRefresher_NeedsRefresh(t *testing.T) {
	r := NewCodeBuddyTokenRefresher(NewCodeBuddyOAuthService(nil))

	withinWindow := &Account{
		Platform: PlatformCodeBuddy,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"expires_at": strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10),
		},
	}
	require.True(t, r.NeedsRefresh(withinWindow, 0),
		"expires within 24h window should need refresh")

	outsideWindow := &Account{
		Platform: PlatformCodeBuddy,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"expires_at": strconv.FormatInt(time.Now().Add(48*time.Hour).Unix(), 10),
		},
	}
	require.False(t, r.NeedsRefresh(outsideWindow, 0),
		"expires beyond 24h window should not need refresh")

	noExpiry := &Account{Platform: PlatformCodeBuddy, Type: AccountTypeOAuth}
	require.False(t, r.NeedsRefresh(noExpiry, 0),
		"missing expires_at should not need refresh")

	wrongPlatform := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"expires_at": strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10),
		},
	}
	require.False(t, r.NeedsRefresh(wrongPlatform, 0),
		"wrong platform should never need refresh via this refresher")

	// 边界：略大于 24h 窗口（含计算与判定之间的时间差余量）不应触发刷新（< 窗口才刷新）。
	exactlyWindow := &Account{
		Platform: PlatformCodeBuddy,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"expires_at": strconv.FormatInt(time.Now().Add(codeBuddyRefreshWindow+time.Minute).Unix(), 10),
		},
	}
	require.False(t, r.NeedsRefresh(exactlyWindow, 0),
		"outside the 24h window should not need refresh")
}
