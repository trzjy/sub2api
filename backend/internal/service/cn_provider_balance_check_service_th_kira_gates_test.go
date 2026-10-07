package service

import (
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// ---- D-TH-05B：TH/Kira 收集期年龄门/退避门 + per-provider 探测间隔配置 ----

// thKiraGateConfig 构造带 B 卡默认探测间隔（60 分钟）的 config，隔离既有零值 config。
func thKiraGateConfig() *config.Config {
	return &config.Config{
		Gateway: config.GatewayConfig{
			CNProviders: config.GatewayCNProvidersConfig{
				ThProbeIntervalMinutes:   60,
				KiraProbeIntervalMinutes: 60,
			},
		},
	}
}

// 测试 1：TH 年龄门（th_usage_snapshot.fetched_at）。
func TestCNBalanceCheckTHAgeGate(t *testing.T) {
	now := time.Now().UTC()
	svc := &CNProviderBalanceCheckService{cfg: thKiraGateConfig()}

	fresh := &Account{ID: 1, Extra: map[string]any{
		TokenHarborUsageSnapshotExtraKey: TokenHarborUsageSnapshot{FetchedAt: now.Add(-5 * time.Minute)},
	}}
	stale := &Account{ID: 2, Extra: map[string]any{
		TokenHarborUsageSnapshotExtraKey: TokenHarborUsageSnapshot{FetchedAt: now.Add(-70 * time.Minute)},
	}}
	none := &Account{ID: 3, Extra: map[string]any{}}

	require.True(t, svc.shouldSkipTokenHarborCollect(now, fresh), "5分钟前快照必须跳过（年龄门）")
	require.False(t, svc.shouldSkipTokenHarborCollect(now, stale), "70分钟前快照必须收集（超出间隔）")
	require.False(t, svc.shouldSkipTokenHarborCollect(now, none), "无快照必须收集")
}

// 测试 2：TH 退避门（extra th_probe_backoff_until，unix 秒）。
func TestCNBalanceCheckTHBackoffGate(t *testing.T) {
	now := time.Now().UTC()
	svc := &CNProviderBalanceCheckService{cfg: thKiraGateConfig()}

	future := &Account{ID: 1, Extra: map[string]any{
		// 退避键优先于年龄门：即使快照够新鲜/缺失，退避未过期仍跳过。
		thProbeBackoffUntilExtraKey: float64(now.Add(10 * time.Minute).Unix()),
	}}
	past := &Account{ID: 2, Extra: map[string]any{
		thProbeBackoffUntilExtraKey: float64(now.Add(-10 * time.Minute).Unix()),
	}}
	missing := &Account{ID: 3, Extra: map[string]any{}}
	nullv := &Account{ID: 4, Extra: map[string]any{
		thProbeBackoffUntilExtraKey: nil,
	}}

	require.True(t, svc.shouldSkipTokenHarborCollect(now, future), "未来退避必须跳过")
	require.False(t, svc.shouldSkipTokenHarborCollect(now, past), "过期退避必须收集")
	require.False(t, svc.shouldSkipTokenHarborCollect(now, missing), "缺失退避必须收集")
	require.False(t, svc.shouldSkipTokenHarborCollect(now, nullv), "null 退避必须收集")

	// 退避门与年龄门叠加：未来退避 + 陈旧快照 → 仍跳过。
	staleAndBackedOff := &Account{ID: 5, Extra: map[string]any{
		TokenHarborUsageSnapshotExtraKey: TokenHarborUsageSnapshot{FetchedAt: now.Add(-70 * time.Minute)},
		thProbeBackoffUntilExtraKey:      float64(now.Add(10 * time.Minute).Unix()),
	}}
	require.True(t, svc.shouldSkipTokenHarborCollect(now, staleAndBackedOff), "未来退避 + 陈旧快照仍跳过")
}

// 测试 3：Kira 仅年龄门（kira_usage_snapshot.fetched_at，RFC3339）。
func TestCNBalanceCheckKiraAgeGate(t *testing.T) {
	now := time.Now().UTC()
	svc := &CNProviderBalanceCheckService{cfg: thKiraGateConfig()}

	fresh := &Account{ID: 1, Extra: map[string]any{
		kiraUsageSnapshotExtraKey: map[string]any{"fetched_at": now.Add(-5 * time.Minute).Format(time.RFC3339)},
	}}
	stale := &Account{ID: 2, Extra: map[string]any{
		kiraUsageSnapshotExtraKey: map[string]any{"fetched_at": now.Add(-70 * time.Minute).Format(time.RFC3339)},
	}}
	none := &Account{ID: 3, Extra: map[string]any{}}
	bad := &Account{ID: 4, Extra: map[string]any{
		kiraUsageSnapshotExtraKey: map[string]any{"fetched_at": "not-a-time"},
	}}

	require.True(t, svc.shouldSkipKiraCollect(now, fresh), "5分钟前 kira 快照必须跳过")
	require.False(t, svc.shouldSkipKiraCollect(now, stale), "70分钟前 kira 快照必须收集")
	require.False(t, svc.shouldSkipKiraCollect(now, none), "无 kira 快照必须收集")
	require.False(t, svc.shouldSkipKiraCollect(now, bad), "不可解析 kira 快照必须收集（不过新）")
}

// 测试 4：预算联动 —— 年龄门过滤后 thTargets 数量变化，超时公式按过滤后数量
// 计算（公式不改），仅被收集账号触发快照刷新（落库 + 上游命中各 1 次）。
func TestCNBalanceCheckRunOnce_THCollectFilterBudget(t *testing.T) {
	fakeTH := newTokenHarborFakeTH(t, false)
	now := time.Now().UTC()
	fakeTH.mu.Lock()
	fakeTH.usageCSVBody = testTHUsageCSV(now)
	fakeTH.mu.Unlock()
	thUpstream := &tokenHarborFakeUpstream{server: fakeTH.server}

	// fresh：th_usage_snapshot 距今 5 分钟 → 年龄门跳过（目标切片不计入）。
	fresh := tokenHarborTestAccount(701)
	fresh.Platform = PlatformOpenAI
	fresh.Status = StatusActive
	fresh.Credentials["base_url"] = "https://tokenharbor.ai/v1"
	fresh.Extra = map[string]any{
		TokenHarborUsageSnapshotExtraKey: TokenHarborUsageSnapshot{FetchedAt: now.Add(-5 * time.Minute)},
	}
	// stale：无快照 → 收集。
	stale := tokenHarborTestAccount(702)
	stale.Platform = PlatformOpenAI
	stale.Status = StatusActive
	stale.Credentials["base_url"] = "https://tokenharbor.ai/v1"
	stale.Extra = map[string]any{}

	repo := &cnRunOnceExtraRepo{
		byPlatform: map[string][]Account{PlatformOpenAI: {*fresh, *stale}},
		byID:       map[int64]*Account{701: fresh, 702: stale},
	}
	thSvc := NewTokenHarborPassService(repo, nil, thUpstream)
	thSvc.baseURL = fakeTH.server.URL
	thSvc.now = func() time.Time { return now }

	svc := &CNProviderBalanceCheckService{accountRepo: repo, httpUpstream: thUpstream, cfg: thKiraGateConfig()}
	svc.SetTokenHarborPassService(thSvc)
	svc.runOnce()

	// 仅 stale 被收集：fresh 被年龄门过滤，目标切片长度 = 1。
	require.Equal(t, 1, fakeTH.usageCSVHits, "fresh TH 账号必须被年龄门过滤，仅 stale 收集（usage 命中=1）")
	loginPosts, _ := fakeTH.stats()
	require.Equal(t, 1, loginPosts, "pass 快照刷新仅 stale 命中（login=1）")

	usageWrites := 0
	for _, w := range repo.extraWrites {
		if _, ok := w[TokenHarborUsageSnapshotExtraKey]; ok {
			usageWrites++
		}
	}
	require.Equal(t, 1, usageWrites, "th_usage_snapshot 仅 stale 落库一次（目标切片长度=1）")
}

// Kira 年龄门在 runOnce 主收集循环中生效：够新鲜的 Kira 账号不进 kiraTargets
// （不触发 dashboard 用量刷新请求）。
func TestCNBalanceCheckRunOnce_KiraAgeGateFiltersCollect(t *testing.T) {
	now := time.Now().UTC()
	freshKira := Account{ID: 801, Platform: PlatformKimi, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{
			"account_mode":  AccountModePayG,
			"base_url":      "https://kiraai.vn/api/v1",
			"kira_jwt":      "stale-jwt",
			"kira_email":    "user@example.com",
			"kira_password": "pw-secret",
		}}
	freshKira.Extra = map[string]any{
		kiraUsageSnapshotExtraKey: map[string]any{"fetched_at": now.Add(-5 * time.Minute).Format(time.RFC3339)},
	}
	staleKira := Account{ID: 802, Platform: PlatformKimi, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{
			"account_mode":  AccountModePayG,
			"base_url":      "https://kiraai.vn/api/v1",
			"kira_jwt":      "stale-jwt",
			"kira_email":    "user@example.com",
			"kira_password": "pw-secret",
		}}
	staleKira.Extra = map[string]any{}

	repo := &cnRunOnceExtraRepo{byPlatform: map[string][]Account{PlatformKimi: {freshKira, staleKira}}}
	upstream := &kiraRecordingUpstream{handler: func(r *http.Request, _ string) (int, string) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/user/usage" {
			return http.StatusOK, kiraUsageSummaryFixture
		}
		return http.StatusNotFound, `{}`
	}}
	balanceSvc := NewCNProviderBalanceService(repo, nil, upstream, nil)
	svc := &CNProviderBalanceCheckService{accountRepo: repo, balanceService: balanceSvc, cfg: thKiraGateConfig()}
	svc.runOnce()

	// 仅 staleKira 进 kiraTargets → 仅 1 次 dashboard 用量刷新请求。
	require.Len(t, upstream.requests, 1, "Kira 年龄门必须过滤掉 freshKira，仅 staleKira 刷新")
	require.Equal(t, "/api/user/usage", upstream.requests[0].URL.Path)
}
