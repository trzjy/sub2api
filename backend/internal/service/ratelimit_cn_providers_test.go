//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestCNQuotaSnapshotEarliestExhaustedReset(t *testing.T) {
	now := time.Now()
	past := now.Add(-1 * time.Hour).Format(time.RFC3339)

	// 无任何窗口触顶 → nil（走短冷却）
	if got := cnQuotaSnapshotEarliestExhaustedReset(map[string]any{
		cnExtraKey("volcano", cnExtraSuffix5hUsed):     float64(0),
		cnExtraKey("volcano", cnExtraSuffixWeeklyUsed): float64(8),
		cnExtraKey("volcano", cnExtraSuffixMonthlyUsed): float64(13),
	}, "volcano", now); got != nil {
		t.Fatalf("expected nil (no exhausted window), got %v", *got)
	}

	// 5h 触顶 → 返回 5h 重置点（早于 weekly）
	weekly := now.Add(6 * 24 * time.Hour)
	fiveH := now.Add(2 * time.Hour)
	if got := cnQuotaSnapshotEarliestExhaustedReset(map[string]any{
		cnExtraKey("volcano", cnExtraSuffix5hUsed):      float64(100),
		cnExtraKey("volcano", cnExtraSuffix5hReset):     fiveH.Format(time.RFC3339),
		cnExtraKey("volcano", cnExtraSuffixWeeklyUsed):  float64(100),
		cnExtraKey("volcano", cnExtraSuffixWeeklyReset): weekly.Format(time.RFC3339),
	}, "volcano", now); got == nil || got.Sub(fiveH) > time.Minute {
		t.Fatalf("expected earliest exhausted reset at 5h window, got %v", got)
	}

	// 触顶但重置时间已过期（旧快照）→ 不算耗尽
	if got := cnQuotaSnapshotEarliestExhaustedReset(map[string]any{
		cnExtraKey("volcano", cnExtraSuffix5hUsed):  float64(100),
		cnExtraKey("volcano", cnExtraSuffix5hReset): past,
	}, "volcano", now); got != nil {
		t.Fatalf("expired reset must not count as exhausted, got %v", *got)
	}

	// 快照缺失 → nil
	if got := cnQuotaSnapshotEarliestExhaustedReset(nil, "volcano", now); got != nil {
		t.Fatalf("missing snapshot must return nil, got %v", *got)
	}
}

func TestCNQuotaSnapshotAnyWindowExhausted(t *testing.T) {
	if cnQuotaSnapshotAnyWindowExhausted(map[string]any{
		cnExtraKey("volcano", cnExtraSuffixWeeklyUsed): float64(13),
	}, "volcano") {
		t.Fatal("13% usage must not count as exhausted")
	}
	if !cnQuotaSnapshotAnyWindowExhausted(map[string]any{
		cnExtraKey("volcano", cnExtraSuffixWeeklyUsed): float64(100),
	}, "volcano") {
		t.Fatal("100% usage must count as exhausted")
	}
	if cnQuotaSnapshotAnyWindowExhausted(nil, "volcano") {
		t.Fatal("missing snapshot must be treated as no evidence")
	}
}

type cnReconcileRepoStub struct {
	AccountRepository
	accounts []Account
	cleared  []int64
}

func (s *cnReconcileRepoStub) ListByPlatform(context.Context, string) ([]Account, error) {
	return s.accounts, nil
}

func (s *cnReconcileRepoStub) ClearRateLimit(_ context.Context, id int64) error {
	s.cleared = append(s.cleared, id)
	return nil
}

func TestReconcileCNProviderRateLimitsClearsMisBench(t *testing.T) {
	now := time.Now()
	weekReset := now.Add(6 * 24 * time.Hour)

	misBenched := cnVolcanoTestAccount(1, weekReset)
	misBenched.Extra = map[string]any{
		cnExtraKey("volcano", cnExtraSuffixWeeklyUsed):  float64(8),
		cnExtraKey("volcano", cnExtraSuffixWeeklyReset): weekReset.Format(time.RFC3339),
	}

	shortCooldown := Account{ID: 2, Platform: PlatformZhipu, RateLimitedAt: &now}
	shortReset := now.Add(5 * time.Minute)
	shortCooldown.RateLimitResetAt = &shortReset

	reallyExhausted := cnVolcanoTestAccount(3, weekReset)
	reallyExhausted.Extra = map[string]any{
		cnExtraKey("volcano", cnExtraSuffixWeeklyUsed): float64(100),
	}

	nonCN := Account{ID: 4, Platform: PlatformAnthropic, RateLimitedAt: &now, RateLimitResetAt: &weekReset}

	repo := &cnReconcileRepoStub{accounts: []Account{misBenched, shortCooldown, reallyExhausted, nonCN}}
	cleared := reconcileCNProviderRateLimits(context.Background(), repo, []string{PlatformZhipu}, now)

	if cleared != 1 {
		t.Fatalf("expected exactly 1 cleared (mis-benched account), got %d", cleared)
	}
	if len(repo.cleared) != 1 || repo.cleared[0] != misBenched.ID {
		t.Fatalf("expected only account %d to be cleared, got %v", misBenched.ID, repo.cleared)
	}
}

func cnVolcanoTestAccount(id int64, resetAt time.Time) Account {
	a := Account{ID: id, Platform: PlatformZhipu, Type: AccountTypeAPIKey, RateLimitedAt: &resetAt, RateLimitResetAt: &resetAt}
	a.Credentials = map[string]any{"base_url": "https://ark.cn-beijing.volces.com/api/v3"}
	return a
}

// ---------- D-QL-002：响应式 402/429 入口接线额度耗尽状态机 ----------

// fakeCNQuotaLifecycle 响应式入口窄面的假实现（断言「确认探针/状态机入口被调用」）。
type fakeCNQuotaLifecycle struct {
	mu        sync.Mutex
	calls     []fakeLifecycleCall
	returnErr error
}

type fakeLifecycleCall struct {
	accountID   int64
	upstreamMsg string
}

func (f *fakeCNQuotaLifecycle) OnUpstreamQuotaExhausted(_ context.Context, account *Account, upstreamMsg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeLifecycleCall{accountID: account.ID, upstreamMsg: upstreamMsg})
	return f.returnErr
}

func (f *fakeCNQuotaLifecycle) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func newRatelimitTestService(repo *quotaLifecycleFakeRepo) *RateLimitService {
	return NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
}

// newKiraUnderKimiPlatformAccount 复刻生产形态：Kira 账号 platform 挂在 kimi 下，
// base_url 是唯一事实源（resolveCNQuotaProvider / cnQuotaLifecycleProviderOf 同源）。
func newKiraUnderKimiPlatformAccount(id int64) *Account {
	return &Account{
		ID: id, Platform: PlatformKimi, Type: AccountTypeAPIKey, Status: StatusActive,
		Credentials: map[string]any{
			"api_key":  "kira-test",
			"kira_jwt": "jwt-test",
			"base_url": "https://kiraai.vn/api/v1",
		},
		Extra: map[string]any{},
	}
}

// 402 入口：cn_balance_low 信号标记保留，停调语义移交状态机（确认探针被调用一次），
// 入口自身不再做任何停调（2×interval 滚动冷却已退役）。
func TestHandleCNProviderInsufficientBalanceDelegatesToLifecycle(t *testing.T) {
	account := newKiraUnderKimiPlatformAccount(208)
	repo := newQuotaLifecycleFakeRepo(account)
	lc := &fakeCNQuotaLifecycle{}
	svc := newRatelimitTestService(repo)
	svc.SetCNQuotaLifecycle(lc)

	svc.handleCNProviderInsufficientBalance(context.Background(), account, "Insufficient VND wallet balance (0 VND remaining)")

	// 状态机入口被调用一次，上游文案原样透传。
	require.Equal(t, 1, lc.callCount())
	require.Equal(t, int64(208), lc.calls[0].accountID)
	require.Equal(t, "Insufficient VND wallet balance (0 VND remaining)", lc.calls[0].upstreamMsg)
	// cn_balance_low 响应式信号标记保留。
	require.Equal(t, true, account.Extra[cnExtraKey(PlatformKimi, cnBalanceExtraSuffixLow)])
	// 停调到期时间=状态机给定值：入口自身不停调（无 20 分钟滚动冷却残留）。
	_, parked := repo.parkedUntil(208)
	require.False(t, parked, "reactive 402 entry must not park by itself; parking belongs to the lifecycle state machine")
}

// 402 入口端到端（真实状态机）：确认探针属实 → 停调到期=官方恢复时间（renewsAt），
// 而非 now+20min；告警 fire。
func TestHandleCNProviderInsufficientBalanceParksToOfficialRecoveryTime(t *testing.T) {
	account := newQuotaLifecycleTHAccount(207)
	repo := newQuotaLifecycleFakeRepo(account)
	alerts := &fakeQuotaAlertStore{}
	probe := &quotaProbeController{outcome: quotaProbeExhausted}
	lifecycle := newQuotaLifecycleTestService(repo, alerts, probe)
	svc := newRatelimitTestService(repo)
	svc.SetCNQuotaLifecycle(lifecycle)

	svc.handleCNProviderInsufficientBalance(context.Background(), account, "Your Pass allowance for this period is used")

	// 确认探针恰好一次。
	require.Equal(t, 1, probe.calls)
	// 停调到期 = th_pass_snapshot.renews_at（官方恢复时间），非滚动冷却。
	want := quotaLifecycleBase.Add(30 * 24 * time.Hour)
	until, parked := repo.parkedUntil(207)
	require.True(t, parked, "confirmed exhaustion must park the account")
	require.True(t, want.Equal(until), "park until must equal official recovery time %s, got %s", want, until)
	require.Greater(t, until.Sub(quotaLifecycleBase), 20*time.Minute, "must not be the retired 2×interval rolling cooldown")
	require.True(t, len(repo.parkedReason(207)) >= len(cnQuotaExhaustedReasonPrefix))
	require.True(t, len(alerts.firingEvents(quotaAlertDimsFor(207))) == 1, "quota exhausted alert must fire")
}

// 402 入口端到端：探测不确定 → 失败关闭（入口不停调、状态不动、告警 fire）。
func TestHandleCNProviderInsufficientBalanceFailClosedKeepsState(t *testing.T) {
	account := newQuotaLifecycleTHAccount(207)
	repo := newQuotaLifecycleFakeRepo(account)
	alerts := &fakeQuotaAlertStore{}
	probe := &quotaProbeController{outcome: quotaProbeUncertain, err: errors.New("transport down")}
	lifecycle := newQuotaLifecycleTestService(repo, alerts, probe)
	svc := newRatelimitTestService(repo)
	svc.SetCNQuotaLifecycle(lifecycle)

	svc.handleCNProviderInsufficientBalance(context.Background(), account, "Your Pass allowance for this period is used")

	require.Equal(t, 1, probe.calls)
	_, parked := repo.parkedUntil(207)
	require.False(t, parked, "uncertain probe must fail closed: no park from a clean state")
	require.Len(t, alerts.firingEvents(quotaAlertDimsFor(207)), 1)
}

// 状态机未注入（装配缺位）：只落信号标记、不停调、不 panic（无滚动冷却兜底可回退）。
func TestHandleCNProviderInsufficientBalanceWithoutLifecycleNoPark(t *testing.T) {
	account := newKiraUnderKimiPlatformAccount(208)
	repo := newQuotaLifecycleFakeRepo(account)
	svc := newRatelimitTestService(repo)

	require.NotPanics(t, func() {
		svc.handleCNProviderInsufficientBalance(context.Background(), account, "Insufficient VND wallet balance (0 VND remaining)")
	})
	require.Equal(t, true, account.Extra[cnExtraKey(PlatformKimi, cnBalanceExtraSuffixLow)])
	_, parked := repo.parkedUntil(208)
	require.False(t, parked)
}

// 429 归一为额度耗尽（账号级）：TH/Kira 账号 429 接入状态机，停调到期=官方恢复
// 时间（Kira 每日重置时刻），确认探针被调用。
func TestApplyCNProviderReactive429KiraNormalizesToQuotaExhausted(t *testing.T) {
	account := newKiraUnderKimiPlatformAccount(208)
	repo := newQuotaLifecycleFakeRepo(account)
	alerts := &fakeQuotaAlertStore{}
	probe := &quotaProbeController{outcome: quotaProbeExhausted}
	lifecycle := newQuotaLifecycleTestService(repo, alerts, probe)
	svc := newRatelimitTestService(repo)
	svc.SetCNQuotaLifecycle(lifecycle)

	handled := svc.applyCNProviderReactive429(context.Background(), account, []byte(`{"error":{"message":"rate limit exceeded"}}`))

	require.True(t, handled, "lifecycle-managed account 429 must be handled by the reactive entry")
	require.Equal(t, 1, probe.calls)
	// 停调到期 = Kira 免费池每日重置时刻（越南时区次日零点），非 61s 短冷却。
	want := kiraNextDailyReset(quotaLifecycleBase)
	until, parked := repo.parkedUntil(208)
	require.True(t, parked)
	require.True(t, want.Equal(until), "429 park until must equal state-machine recovery time %s, got %s", want, until)
}

// 非 TH/Kira 的 CN 供应商 429 行为不变：无触顶快照 → 61s 短冷却，不经状态机。
func TestApplyCNProviderReactive429ZhipuShortCooldownUnchanged(t *testing.T) {
	now := time.Now()
	account := &Account{
		ID:       900,
		Platform: PlatformZhipu,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"account_mode": AccountModeCoding,
			"base_url":     "https://open.bigmodel.cn/api/paas/v4",
		},
		Extra: map[string]any{
			cnExtraKey("zhipu", cnExtraSuffix5hUsed): float64(10),
		},
	}
	repo := newQuotaLifecycleFakeRepo(account)
	lc := &fakeCNQuotaLifecycle{}
	svc := newRatelimitTestService(repo)
	svc.SetCNQuotaLifecycle(lc)

	handled := svc.applyCNProviderReactive429(context.Background(), account, []byte(`{"error":{"message":"too many requests"}}`))

	require.True(t, handled)
	require.Equal(t, 0, lc.callCount(), "non-lifecycle CN providers must not enter the quota state machine")
	until, parked := repo.parkedUntil(900)
	require.True(t, parked)
	require.Less(t, until.Sub(now), 2*time.Minute, "short cooldown must stay short (61s), not a rolling 20min park")
	require.Equal(t, cnNonQuota429Reason, repo.parkedReason(900))
}
