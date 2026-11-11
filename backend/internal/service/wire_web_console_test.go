//go:build unit

package service

// CARD-WCQ-W1b 装配测试：Web 控制台额度链的 wire 接线。
//
// 覆盖：
//  1. ProvideWebConsoleQuotaService / ProvideWebConsoleAuthService 可构造且依赖透传；
//  2. helper 配置缺失（cfg 为 nil 或 base_url 为空）时构造不 panic，helper 退化为 nil；
//  3. ProvideCNProviderBalanceCheckService 在 SetTokenHarborPassService 之后把
//     WebConsoleQuotaService 注入周期检测服务。

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// ---------- 1. Web 控制台服务 provider ----------

func TestProvideWebConsoleQuotaService_WiresRepoAndConfig(t *testing.T) {
	repo := newQuotaLifecycleFakeRepo()
	cfg := &config.Config{}

	svc := ProvideWebConsoleQuotaService(repo, cfg, nil)

	require.NotNil(t, svc)
	require.Same(t, repo, svc.accountRepo, "account repository must be passed through")
	require.Same(t, cfg, svc.cfg, "config must be passed through")
}

// wireTempUnschedCacheStub 是 TempUnschedCache 的最小桩：仅用于断言 provider 把
// 第 3 个入参透传到 WebConsoleQuotaService（方案 §4.4 不变式 3/4 的缓存失效链路）。
type wireTempUnschedCacheStub struct{}

func (wireTempUnschedCacheStub) SetTempUnsched(_ context.Context, _ int64, _ *TempUnschedState) error {
	return nil
}

func (wireTempUnschedCacheStub) GetTempUnsched(_ context.Context, _ int64) (*TempUnschedState, error) {
	return nil, nil
}

func (wireTempUnschedCacheStub) DeleteTempUnsched(_ context.Context, _ int64) error {
	return nil
}

func TestProvideWebConsoleQuotaService_InjectsTempUnschedCache(t *testing.T) {
	repo := newQuotaLifecycleFakeRepo()
	cfg := &config.Config{}
	cache := &wireTempUnschedCacheStub{}

	svc := ProvideWebConsoleQuotaService(repo, cfg, cache)

	require.NotNil(t, svc)
	require.NotNil(t, svc.tempUnschedCache, "temp unsched cache must be injected when provided")
	require.Same(t, cache, svc.tempUnschedCache, "temp unsched cache must be passed through")
}

func TestProvideWebConsoleAuthService_WiresQuotaAndRepo(t *testing.T) {
	repo := newQuotaLifecycleFakeRepo()
	cfg := &config.Config{}
	quota := ProvideWebConsoleQuotaService(repo, cfg, nil)

	svc := ProvideWebConsoleAuthService(quota, repo, cfg)

	require.NotNil(t, svc)
	require.Same(t, quota, svc.quotaService, "quota service must be shared with the auth orchestrator")
	require.Same(t, repo, svc.accountRepo)
}

// helper 未配置时不得 panic：交互登录入口按既有口径返回 not configured。
func TestProvideWebConsoleAuthService_NilConfigLeavesHelperNil(t *testing.T) {
	repo := newQuotaLifecycleFakeRepo()
	quota := ProvideWebConsoleQuotaService(repo, &config.Config{}, nil)

	svc := ProvideWebConsoleAuthService(quota, repo, nil)

	require.NotNil(t, svc)
	require.Nil(t, svc.helper, "missing config must not fabricate a helper client")
}

func TestProvideWebConsoleAuthService_EmptyBaseURLLeavesHelperNil(t *testing.T) {
	repo := newQuotaLifecycleFakeRepo()
	cfg := &config.Config{} // LocalCaptchaHelper.BaseURL 零值
	quota := ProvideWebConsoleQuotaService(repo, cfg, nil)

	svc := ProvideWebConsoleAuthService(quota, repo, cfg)

	require.NotNil(t, svc)
	require.Nil(t, svc.helper, "empty base_url must not fabricate a helper client")
}

func TestProvideWebConsoleAuthService_ConfiguredBaseURLBuildsHelper(t *testing.T) {
	repo := newQuotaLifecycleFakeRepo()
	cfg := &config.Config{}
	cfg.LocalCaptchaHelper.BaseURL = "http://127.0.0.1:8099"
	cfg.LocalCaptchaHelper.APIKey = "test-secret"
	cfg.LocalCaptchaHelper.Timeout = 2
	quota := ProvideWebConsoleQuotaService(repo, cfg, nil)

	svc := ProvideWebConsoleAuthService(quota, repo, cfg)

	require.NotNil(t, svc)
	require.NotNil(t, svc.helper, "configured base_url must build the /web-auth client")
}

// ---------- 2. 周期链注入 ----------
// ProvideCNProviderBalanceCheckService 在 TH Pass 注入之后回注 Web 控制台额度服务
// （字段断言；字段私有属同包可见）。cfg 零值 BalanceCheckEnabled=false → Start()
// 直接返回，无后台循环。
func TestWireBalanceCheckServiceReceivesWebConsoleQuotaService(t *testing.T) {
	repo := newQuotaLifecycleFakeRepo()
	cfg := &config.Config{}
	wcQuota := ProvideWebConsoleQuotaService(repo, cfg, nil)

	svc := ProvideCNProviderBalanceCheckService(repo, nil, nil, nil, nil, nil, cfg, nil, nil, wcQuota)

	require.NotNil(t, svc)
	require.Same(t, wcQuota, svc.webConsoleQuotaService,
		"SetWebConsoleQuotaService must receive the wired web console quota service")
}

// 未提供 Web 控制台服务时注入跳过，周期探测维持既有三家上游以外行为不 panic。
func TestWireBalanceCheckService_NilWebConsoleQuotaServiceIsSkipped(t *testing.T) {
	repo := newQuotaLifecycleFakeRepo()

	svc := ProvideCNProviderBalanceCheckService(repo, nil, nil, nil, nil, nil, &config.Config{}, nil, nil, nil)

	require.NotNil(t, svc)
	require.Nil(t, svc.webConsoleQuotaService)
}
