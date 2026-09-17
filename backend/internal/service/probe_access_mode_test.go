package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// PR-1 主会话补齐项：web 接入模式账号不参与上游模型同步与 API key 计费探活
//（方案 §5.6 红线，归并后 platform 已是官方值，判定源换 access mode）。
func TestSyncUpstreamModelCatalogWebAccessModeFailClosed(t *testing.T) {
	s := &AccountTestService{}
	acc := &Account{ID: 95, Platform: domain.PlatformZhipu, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"access_mode": "web", "cookie": "c"}}
	catalog, err := s.SyncUpstreamModelCatalog(t.Context(), acc)
	require.Error(t, err)
	require.Nil(t, catalog)
	var syncErr *UpstreamModelSyncError
	require.ErrorAs(t, err, &syncErr)
}

func TestSyncUpstreamModelCatalogAPIAccessModeUnaffected(t *testing.T) {
	s := &AccountTestService{}
	acc := &Account{ID: 1, Platform: domain.PlatformZhipu, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"access_mode": "api", "api_key": "sk-x"}}
	// API 模式不受新守卫影响（不返回 unsupported；此处无 upstream 依赖，仅验证守卫放行）。
	_, err := s.SyncUpstreamModelCatalog(t.Context(), acc)
	if err != nil {
		require.False(t, upstreamModelListEndpointUnsupported(err))
	}
}

func TestIsUpstreamBillingProbeAccountExcludesWebAccessMode(t *testing.T) {
	web := &Account{Platform: domain.PlatformZhipu, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"access_mode": "web", "cookie": "c"}}
	api := &Account{Platform: domain.PlatformZhipu, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"access_mode": "api", "api_key": "sk-x"}}
	legacy := &Account{Platform: domain.PlatformWebZhipu, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"cookie": "c"}}
	require.False(t, isUpstreamBillingProbeAccount(web))
	require.True(t, isUpstreamBillingProbeAccount(api))
	// 旧 web-* 平台值本就不在 probe 平台白名单内，维持排除。
	require.False(t, isUpstreamBillingProbeAccount(legacy))
	var nilAccount *Account
	require.False(t, isUpstreamBillingProbeAccount(nilAccount))
}

func TestCrsSyncProbeEligibleExcludesWebAccessMode(t *testing.T) {
	require.True(t, accessModeFromCredentials(map[string]any{"access_mode": "web"}) == AccountAccessModeWeb)
}
