package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// GetAccessMode 四态语义钉住（docs/platform-merge-refactor-plan.md §5.1）：
// 显式合法 / 显式非法失败关闭 / 缺失形状推断 / 缺失默认 api。
func TestGetAccessModeFourStates(t *testing.T) {
	tests := []struct {
		name       string
		platform   string
		credential map[string]any
		want       string
	}{
		{
			name:       "explicit api",
			platform:   domain.PlatformZhipu,
			credential: map[string]any{"access_mode": "api"},
			want:       AccountAccessModeAPI,
		},
		{
			name:       "explicit web",
			platform:   domain.PlatformZhipu,
			credential: map[string]any{"access_mode": "web"},
			want:       AccountAccessModeWeb,
		},
		{
			name:       "explicit invalid returns empty (fail-closed signal)",
			platform:   domain.PlatformZhipu,
			credential: map[string]any{"access_mode": "oauth", "cookie": "x"},
			want:       "",
		},
		{
			name:       "missing + cookie shape infers web",
			platform:   domain.PlatformZhipu,
			credential: map[string]any{"cookie": "x"},
			want:       AccountAccessModeWeb,
		},
		{
			name:       "missing + access_token shape infers web",
			platform:   domain.PlatformKimi,
			credential: map[string]any{"access_token": "x"},
			want:       AccountAccessModeWeb,
		},
		{
			name:       "missing + empty cookie does not infer web",
			platform:   domain.PlatformZhipu,
			credential: map[string]any{"cookie": ""},
			want:       AccountAccessModeAPI,
		},
		{
			name:       "missing + shape on non-merge platform stays api",
			platform:   domain.PlatformOpenAI,
			credential: map[string]any{"cookie": "x"},
			want:       AccountAccessModeAPI,
		},
		{
			name:       "missing + no shape defaults api",
			platform:   domain.PlatformZhipu,
			credential: map[string]any{"api_key": "sk-x"},
			want:       AccountAccessModeAPI,
		},
		{
			name:       "mixed credentials: access_mode is sole source",
			platform:   domain.PlatformDeepseek,
			credential: map[string]any{"access_mode": "api", "api_key": "sk-x", "cookie": "c"},
			want:       AccountAccessModeAPI,
		},
		{
			name:       "mixed credentials explicit web",
			platform:   domain.PlatformDeepseek,
			credential: map[string]any{"access_mode": "web", "api_key": "sk-x", "cookie": "c"},
			want:       AccountAccessModeWeb,
		},
		{
			name:       "legacy web-zhipu platform with cookie infers web",
			platform:   domain.PlatformWebZhipu,
			credential: map[string]any{"cookie": "c"},
			want:       AccountAccessModeWeb,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			acc := &Account{Platform: tt.platform, Credentials: tt.credential}
			require.Equalf(t, tt.want, acc.GetAccessMode(), "GetAccessMode()")
		})
	}
}

func TestResolveAccessModeFailClosedOnInvalid(t *testing.T) {
	acc := &Account{ID: 95, Platform: domain.PlatformZhipu,
		Credentials: map[string]any{"access_mode": "bogus", "cookie": "c"}}

	mode, err := acc.ResolveAccessMode()
	require.Error(t, err)
	require.Empty(t, mode)
	require.Contains(t, err.Error(), "95")
	require.Contains(t, err.Error(), "fail-closed")
	// 失败关闭：不得按形状/默认值推断出 api 或 web。
	require.NotEqual(t, AccountAccessModeAPI, mode)
	require.NotEqual(t, AccountAccessModeWeb, mode)
}

func TestResolveAccessModeValid(t *testing.T) {
	acc := &Account{ID: 1, Platform: domain.PlatformZhipu,
		Credentials: map[string]any{"access_mode": "web"}}
	mode, err := acc.ResolveAccessMode()
	require.NoError(t, err)
	require.Equal(t, AccountAccessModeWeb, mode)
}

func TestIsWebAccessMode(t *testing.T) {
	web := &Account{Platform: domain.PlatformZhipu, Credentials: map[string]any{"access_mode": "web"}}
	api := &Account{Platform: domain.PlatformZhipu, Credentials: map[string]any{"access_mode": "api"}}
	require.True(t, web.IsWebAccessMode())
	require.False(t, api.IsWebAccessMode())
	var nilAccount *Account
	require.False(t, nilAccount.IsWebAccessMode())
}

// GetAccessMode 与 GetAccountMode 互不调用（语义正交）。
func TestAccessModeOrthogonalToAccountMode(t *testing.T) {
	acc := &Account{Platform: domain.PlatformZhipu,
		Credentials: map[string]any{"access_mode": "web", "account_mode": "payg"}}
	require.Equal(t, AccountAccessModeWeb, acc.GetAccessMode())
	require.Equal(t, AccountModePayG, acc.GetAccountMode())
}
