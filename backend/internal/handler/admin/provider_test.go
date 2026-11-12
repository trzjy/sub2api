//go:build unit

package admin

// CARD-WCQ-W1b：ProvideCNProviderHandlerWithWebConsole 包装 provider 的注入语义。
//
// 包装 provider 落在 admin 包（service 包不得反向 import handler/admin），
// 此处断言构造后 Setter 已把 Web 控制台额度/登录服务写入 handler 字段。

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestProvideCNProviderHandlerWithWebConsole_InjectsServices(t *testing.T) {
	cfg := &config.Config{}
	quota := service.NewCNProviderQuotaService(nil, nil, nil, cfg)
	balance := service.NewCNProviderBalanceService(nil, nil, nil, cfg)
	wcQuota := service.NewWebConsoleQuotaService(nil, cfg)
	wcAuth := service.NewWebConsoleAuthService(wcQuota, nil, nil)
	wcRemote := service.NewWebConsoleRemoteLoginService(nil, wcQuota, cfg, nil)

	h := ProvideCNProviderHandlerWithWebConsole(quota, balance, wcQuota, wcAuth, wcRemote)

	require.NotNil(t, h)
	require.Same(t, quota, h.quotaService, "legacy constructor args must be forwarded unchanged")
	require.Same(t, balance, h.balanceService, "legacy constructor args must be forwarded unchanged")
	require.Same(t, wcQuota, h.webConsoleQuotaService, "SetWebConsoleServices must inject the quota service")
	require.Same(t, wcAuth, h.webConsoleAuthService, "SetWebConsoleServices must inject the auth service")
	require.Same(t, wcRemote, h.webConsoleRemoteLoginService, "remote login service must be injected")
}

func TestProvideCNProviderHandlerWithWebConsole_NilServicesStillBuildsHandler(t *testing.T) {
	h := ProvideCNProviderHandlerWithWebConsole(nil, nil, nil, nil, nil)

	require.NotNil(t, h)
	require.Nil(t, h.webConsoleQuotaService)
	require.Nil(t, h.webConsoleAuthService)
	require.Nil(t, h.webConsoleRemoteLoginService)
}
