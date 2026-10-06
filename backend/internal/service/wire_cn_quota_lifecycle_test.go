//go:build unit

package service

// D-QL-006 收口装配测试：额度耗尽状态机生产接线。
//
// 覆盖：
//  1. ProvideCNQuotaLifecycleService 装配后，响应式 402 入口
//     （handleCNProviderInsufficientBalance）走状态机（确认探针 → 停调至官方恢复
//     时间 → OpsService 告警 fire），不再落 cn_quota_lifecycle_not_wired 分支；
//  2. QuotaAlertStore 生产实现窄面（OpsService.GetActiveQuotaAlert /
//     ResolveQuotaAlertOnRecovery）：同维度查/关、无活动告警幂等 no-op；
//  3. QuotaSnapshotRefresher 适配器接 D-QL-004 导出的 Kira 快照链
//     （QueryUsageForAccount → QueryKiraUsageForAccount 落 kira_usage_snapshot）；
//  4. ProvideCNProviderBalanceCheckService 注入 SetQuotaLifecycleHandover。

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// ---------- 1. 响应式入口经 provider 装配后走状态机 ----------

// ProvideCNQuotaLifecycleService 装配 → rateLimitService.SetCNQuotaLifecycle 已注入：
// 402 入口端到端（真实状态机 + 真实 OpsService 告警面 + fake 上游 402 确认耗尽）：
// 停调到期 = th_pass_snapshot.renews_at，告警经 OpsService 窄面可查（kind=quota_exhausted）。
func TestWireCNQuotaLifecycleInjectsReactiveEntryEndToEnd(t *testing.T) {
	account := newQuotaLifecycleTHAccount(207)
	repo := newQuotaLifecycleFakeRepo(account)
	opsRepo := newInMemoryAlertRepo()
	opsService := NewOpsService(opsRepo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	upstream := &quotaCaptureUpstream{
		statusCode: http.StatusPaymentRequired,
		body:       `{"error":{"message":"Your Pass allowance for this period is used"}}`,
	}
	quotaSvc := NewCNProviderQuotaService(repo, nil, upstream, &config.Config{})
	rateLimitSvc := newRatelimitTestService(repo)

	lifecycle := ProvideCNQuotaLifecycleService(repo, upstream, &config.Config{}, opsService, quotaSvc, rateLimitSvc)
	defer lifecycle.Stop()

	// 装配点证据：注入后响应式入口不再走 not_wired 分支。
	require.NotNil(t, rateLimitSvc.cnQuotaLifecycle, "provider must inject SetCNQuotaLifecycle")

	rateLimitSvc.handleCNProviderInsufficientBalance(context.Background(), account, "Your Pass allowance for this period is used")

	// 确认探针（对上游 max_tokens=1 ping，上游 402）→ 确认耗尽。
	require.Equal(t, 1, upstream.tlsCalls, "confirmation probe must hit the upstream once")
	// 停调到期 = 官方恢复时间（renews_at），非滚动冷却。
	want := quotaLifecycleBase.Add(30 * 24 * time.Hour)
	until, parked := repo.parkedUntil(207)
	require.True(t, parked, "confirmed exhaustion must park via the wired state machine")
	require.True(t, want.Equal(until), "park until must equal renews_at %s, got %s", want, until)
	// cn_balance_low 响应式信号标记保留。
	require.Equal(t, true, account.Extra[cnExtraKey(account.Platform, cnBalanceExtraSuffixLow)])
	// 告警经 OpsService 生产实现 fire（GetActiveQuotaAlert 窄面可查）。
	active, err := opsService.GetActiveQuotaAlert(context.Background(), quotaAlertDimsFor(207))
	require.NoError(t, err)
	require.NotNil(t, active, "quota exhausted alert must fire via OpsService narrow face")
	require.Equal(t, OpsAlertStatusFiring, active.Status)
	require.Equal(t, quotaExhaustedAlertKind, active.Dimensions[freshnessDimKind])
}

// 未装配（provider 缺位）时响应式入口只落信号标记不停调（回归，D-QL-002 行为保留）。
func TestWireCNQuotaLifecycleNotInjectedReactiveEntryWarnsOnly(t *testing.T) {
	account := newKiraUnderKimiPlatformAccount(208)
	repo := newQuotaLifecycleFakeRepo(account)
	rateLimitSvc := newRatelimitTestService(repo)
	require.Nil(t, rateLimitSvc.cnQuotaLifecycle)

	rateLimitSvc.handleCNProviderInsufficientBalance(context.Background(), account, "Insufficient VND wallet balance (0 VND remaining)")

	_, parked := repo.parkedUntil(208)
	require.False(t, parked, "unwired reactive entry must not park")
	require.Equal(t, true, account.Extra[cnExtraKey(PlatformKimi, cnBalanceExtraSuffixLow)])
}

// ---------- 2. QuotaAlertStore 生产窄面（OpsService） ----------

// GetActiveQuotaAlert / ResolveQuotaAlertOnRecovery：同维度查/关 + 幂等 no-op
// （照 ResolveFreshnessAlertOnRecovery 同款恢复窄面语义）。
func TestWireOpsServiceQuotaAlertNarrowFace(t *testing.T) {
	opsRepo := newInMemoryAlertRepo()
	opsService := NewOpsService(opsRepo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	dims := quotaAlertDimsFor(207)

	// 无活动告警：查询返回 nil，恢复关闭幂等 no-op。
	active, err := opsService.GetActiveQuotaAlert(context.Background(), dims)
	require.NoError(t, err)
	require.Nil(t, active)
	require.NoError(t, opsService.ResolveQuotaAlertOnRecovery(context.Background(), dims))

	// fire → 查得到；resolve → 关闭；再查为空。
	_, err = opsService.CreateAlertEvent(context.Background(), &OpsAlertEvent{
		Status:     OpsAlertStatusFiring,
		Severity:   quotaAlertSeverity,
		Title:      "账号 207 上游额度耗尽",
		Dimensions: dims,
		FiredAt:    time.Now(),
	})
	require.NoError(t, err)
	active, err = opsService.GetActiveQuotaAlert(context.Background(), dims)
	require.NoError(t, err)
	require.NotNil(t, active)

	require.NoError(t, opsService.ResolveQuotaAlertOnRecovery(context.Background(), dims))
	active, err = opsService.GetActiveQuotaAlert(context.Background(), dims)
	require.NoError(t, err)
	require.Nil(t, active, "resolved alert must no longer be active")
}

// ---------- 3. QuotaSnapshotRefresher 适配器接 Kira 快照链 ----------

// 适配器经 QueryUsageForAccount 分发 Kira dashboard 链：kira_usage_snapshot 落 extra。
func TestWireQuotaSnapshotRefresherAdapterRefreshesKiraSnapshot(t *testing.T) {
	account := newKiraUnderKimiPlatformAccount(208)
	account.Credentials["kira_email"] = "user@example.com"
	account.Credentials["kira_password"] = "pw-secret"
	repo := newQuotaLifecycleFakeRepo(account)
	upstream := &kiraRecordingUpstream{handler: func(r *http.Request, _ string) (int, string) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/user/usage" {
			return http.StatusOK, kiraUsageSummaryFixture
		}
		return http.StatusNotFound, `{}`
	}}
	quotaSvc := NewCNProviderQuotaService(repo, nil, upstream, &config.Config{})

	refresher := cnQuotaSnapshotRefresherAdapter{quotaService: quotaSvc}
	require.NoError(t, refresher.RefreshCNQuotaSnapshot(context.Background(), account))

	snap, ok := account.Extra[kiraUsageSnapshotExtraKey]
	require.True(t, ok, "Kira usage snapshot must be persisted via the quota chain")
	snapMap, ok := snap.(map[string]any)
	require.True(t, ok)
	require.Equal(t, kiraUsageWindowDaily, snapMap["window"])
	require.Contains(t, snapMap, "used_tokens")
	require.Contains(t, snapMap, "limit_tokens")
	// 探测走 dashboard JWT 链（usage 端点），不发推理请求。
	require.Len(t, upstream.requests, 1)
	require.Equal(t, "/api/user/usage", upstream.requests[0].URL.Path)
}

// ---------- 4. 周期检测服务 handover 注入 ----------

// ProvideCNProviderBalanceCheckService 注入状态机交接窄面（字段断言；字段为
// 私有属同包可见，注入后 refreshKiraAccount 的耗尽信号上交状态机）。
func TestWireBalanceCheckServiceReceivesLifecycleHandover(t *testing.T) {
	repo := newQuotaLifecycleFakeRepo()
	balanceSvc := NewCNProviderBalanceService(repo, nil, &kiraRecordingUpstream{handler: func(*http.Request, string) (int, string) {
		return http.StatusNotFound, `{}`
	}}, nil)
	// cfg 零值 BalanceCheckEnabled=false → Start() 直接返回，无后台循环。
	lifecycle := NewCNQuotaLifecycleService(repo, nil, &config.Config{}, nil)

	svc := ProvideCNProviderBalanceCheckService(repo, balanceSvc, nil, nil, nil, nil, &config.Config{}, nil, lifecycle)
	require.NotNil(t, svc)
	require.Same(t, lifecycle, svc.quotaLifecycle, "SetQuotaLifecycleHandover must receive the wired lifecycle service")
}
