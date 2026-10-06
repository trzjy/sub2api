//go:build unit

package service

// CNQuotaLifecycleService 单测（派发单 D-QL-001）。
//
// 覆盖：状态机四态迁移（确认耗尽/恢复/仍耗尽/不确定）、告警 fire/resolve、
// 失败关闭（探测失败绝不恢复、状态不变 + 保持停调）、恢复时间来源优先级
// （上游响应内 > 面板快照 > 未知兜底循环）、5 分钟循环节距、非管辖账号 no-op、
// TH/Kira 确认探针分类与请求形状。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// ---------- 固定时钟与夹具 ----------

var quotaLifecycleBase = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func quotaLifecycleClock() func() time.Time {
	return func() time.Time { return quotaLifecycleBase }
}

func newQuotaLifecycleTHAccount(id int64) *Account {
	return &Account{
		ID: id, Platform: PlatformOther, Type: AccountTypeAPIKey, Status: StatusActive,
		Credentials: map[string]any{
			"api_key":       "sk-th-test",
			"base_url":      "https://tokenharbor.ai/v1",
			"model_mapping": map[string]any{"glm-test": "raw-model"},
		},
		Extra: map[string]any{
			TokenHarborPassSnapshotExtraKey: map[string]any{
				"has_pass":   true,
				"pass_name":  "Agent Pass",
				"renews_at":  quotaLifecycleBase.Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339),
				"fetched_at": quotaLifecycleBase.Format(time.RFC3339),
			},
		},
	}
}

func newQuotaLifecycleKiraAccount(id int64) *Account {
	return &Account{
		ID: id, Platform: PlatformOther, Type: AccountTypeAPIKey, Status: StatusActive,
		Credentials: map[string]any{
			"api_key":  "kira-test",
			"kira_jwt": "jwt-test",
			"base_url": "https://kiraai.vn/api/v1",
		},
		Extra: map[string]any{},
	}
}

// ---------- fakes ----------

type quotaParkedRecord struct {
	until  time.Time
	reason string
}

type quotaLifecycleFakeRepo struct {
	AccountRepository
	mu         sync.Mutex
	accounts   map[int64]*Account
	parked     map[int64]quotaParkedRecord
	clearCalls int
}

func newQuotaLifecycleFakeRepo(accounts ...*Account) *quotaLifecycleFakeRepo {
	r := &quotaLifecycleFakeRepo{
		accounts: make(map[int64]*Account),
		parked:   make(map[int64]quotaParkedRecord),
	}
	for _, a := range accounts {
		r.accounts[a.ID] = a
	}
	return r
}

func (r *quotaLifecycleFakeRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	a, ok := r.accounts[id]
	if !ok {
		return nil, errors.New("account not found")
	}
	return a, nil
}

func (r *quotaLifecycleFakeRepo) ListTempUnschedulableAccounts(_ context.Context, now time.Time, limit int) ([]*Account, error) {
	var out []*Account
	for _, a := range r.accounts {
		if rec, ok := r.parked[a.ID]; ok && rec.until.After(now) {
			out = append(out, a)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (r *quotaLifecycleFakeRepo) SetTempUnschedulable(_ context.Context, id int64, until time.Time, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.parked[id] = quotaParkedRecord{until: until, reason: reason}
	if a, ok := r.accounts[id]; ok {
		a.TempUnschedulableUntil = &until
		a.TempUnschedulableReason = reason
	}
	return nil
}

func (r *quotaLifecycleFakeRepo) ClearTempUnschedulable(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearCalls++
	delete(r.parked, id)
	if a, ok := r.accounts[id]; ok {
		a.TempUnschedulableUntil = nil
		a.TempUnschedulableReason = ""
	}
	return nil
}

func (r *quotaLifecycleFakeRepo) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.accounts[id]
	if !ok {
		return errors.New("account not found")
	}
	if a.Extra == nil {
		a.Extra = map[string]any{}
	}
	for k, v := range updates {
		a.Extra[k] = v
	}
	return nil
}

func (r *quotaLifecycleFakeRepo) parkedUntil(id int64) (time.Time, bool) {
	rec, ok := r.parked[id]
	return rec.until, ok
}

func (r *quotaLifecycleFakeRepo) parkedReason(id int64) string {
	return r.parked[id].reason
}

type fakeQuotaAlertStore struct {
	mu     sync.Mutex
	events []*OpsAlertEvent
	nextID int64
}

func (f *fakeQuotaAlertStore) CreateAlertEvent(_ context.Context, e *OpsAlertEvent) (*OpsAlertEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	cp := *e
	cp.ID = f.nextID
	if cp.FiredAt.IsZero() {
		cp.FiredAt = time.Now()
	}
	f.events = append(f.events, &cp)
	return &cp, nil
}

func (f *fakeQuotaAlertStore) GetActiveQuotaAlert(_ context.Context, dims map[string]any) (*OpsAlertEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ev := range f.events {
		if ev.Status == OpsAlertStatusFiring && dimensionsMatch(ev.Dimensions, dims) {
			return ev, nil
		}
	}
	return nil, nil
}

func (f *fakeQuotaAlertStore) ResolveQuotaAlertOnRecovery(_ context.Context, dims map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ev := range f.events {
		if ev.Status == OpsAlertStatusFiring && dimensionsMatch(ev.Dimensions, dims) {
			now := time.Now()
			ev.Status = OpsAlertStatusResolved
			ev.ResolvedAt = &now
			return nil
		}
	}
	return nil
}

func (f *fakeQuotaAlertStore) firingEvents(dims map[string]any) []*OpsAlertEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*OpsAlertEvent
	for _, ev := range f.events {
		if ev.Status == OpsAlertStatusFiring && dimensionsMatch(ev.Dimensions, dims) {
			out = append(out, ev)
		}
	}
	return out
}

func (f *fakeQuotaAlertStore) resolvedEvents(dims map[string]any) []*OpsAlertEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*OpsAlertEvent
	for _, ev := range f.events {
		if ev.Status == OpsAlertStatusResolved && dimensionsMatch(ev.Dimensions, dims) {
			out = append(out, ev)
		}
	}
	return out
}

func quotaAlertDimsFor(accountID int64) map[string]any {
	return map[string]any{freshnessDimKind: quotaExhaustedAlertKind, freshnessDimAccountID: accountID}
}

type fakeQuotaSnapshotRefresher struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeQuotaSnapshotRefresher) RefreshCNQuotaSnapshot(_ context.Context, _ *Account) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return nil
}

// newQuotaLifecycleTestService 组装被测服务（探针默认走 override，计数可控）。
type quotaProbeController struct {
	mu      sync.Mutex
	outcome quotaProbeOutcome
	err     error
	calls   int
}

func (c *quotaProbeController) probe(_ context.Context, _ *Account) (quotaProbeOutcome, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.outcome, c.err
}

func newQuotaLifecycleTestService(repo *quotaLifecycleFakeRepo, alerts *fakeQuotaAlertStore, probe *quotaProbeController) *CNQuotaLifecycleService {
	svc := NewCNQuotaLifecycleService(repo, &recordingHTTPUpstream{}, &config.Config{}, alerts)
	svc.SetQuotaLifecycleClock(quotaLifecycleClock())
	svc.SetQuotaProbeOverride(probe.probe)
	return svc
}

// ---------- 入口：确认耗尽 ----------

// 确认探针属实 → 停调至官方恢复时间（TH renews_at）+ fire 告警 + 状态落库。
func TestQuotaLifecycleOnUpstreamConfirmedParksToOfficialRecoveryAndFiresAlert(t *testing.T) {
	account := newQuotaLifecycleTHAccount(207)
	repo := newQuotaLifecycleFakeRepo(account)
	alerts := &fakeQuotaAlertStore{}
	probe := &quotaProbeController{outcome: quotaProbeExhausted}
	svc := newQuotaLifecycleTestService(repo, alerts, probe)

	err := svc.OnUpstreamQuotaExhausted(context.Background(), account, "Your Pass allowance for this period is used")
	require.NoError(t, err)

	// 1) 确认探针恰好一次。
	require.Equal(t, 1, probe.calls)
	// 2) 停调到期 = 官方恢复时间（th_pass_snapshot.renews_at），非滚动冷却。
	until, ok := repo.parkedUntil(207)
	require.True(t, ok, "confirmed exhaustion must park the account")
	renewsAt, _ := time.Parse(time.RFC3339, quotaLifecycleBase.Add(30*24*time.Hour).UTC().Format(time.RFC3339))
	require.Equal(t, renewsAt, until)
	require.True(t, strings.HasPrefix(repo.parkedReason(207), cnQuotaExhaustedReasonPrefix))
	// 3) 告警 fire（kind=quota_exhausted，账号级维度）且恰好一条。
	fired := alerts.firingEvents(quotaAlertDimsFor(207))
	require.Len(t, fired, 1)
	require.Equal(t, OpsAlertStatusFiring, fired[0].Status)
	require.Equal(t, quotaAlertSeverity, fired[0].Severity)
	// 4) 状态落库：exhausted + recovery_at=renews_at + 来源 snapshot。
	st, ok := cnQuotaLifecycleStateFromExtra(account.Extra)
	require.True(t, ok)
	require.Equal(t, cnQuotaLifecycleStateExhausted, st.State)
	require.Equal(t, renewsAt.UTC().Format(time.RFC3339), st.RecoveryAt)
	require.Equal(t, cnQuotaRecoverySourceSnapshot, st.RecoverySource)
	require.Equal(t, cnQuotaLifecycleProbeExhausted, st.LastProbeOutcome)
}

// 确认探针成功（瞬时信号）→ 不停调不告警，保持现状。
func TestQuotaLifecycleOnUpstreamProbeRecoveredNoStateChange(t *testing.T) {
	account := newQuotaLifecycleTHAccount(207)
	repo := newQuotaLifecycleFakeRepo(account)
	alerts := &fakeQuotaAlertStore{}
	probe := &quotaProbeController{outcome: quotaProbeRecovered}
	svc := newQuotaLifecycleTestService(repo, alerts, probe)

	err := svc.OnUpstreamQuotaExhausted(context.Background(), account, "Your Pass allowance for this period is used")
	require.NoError(t, err)

	_, parked := repo.parkedUntil(207)
	require.False(t, parked, "transient signal must not park")
	require.Empty(t, alerts.events, "transient signal must not fire alert")
	_, ok := cnQuotaLifecycleStateFromExtra(account.Extra)
	require.False(t, ok, "transient signal must not persist lifecycle state")
}

// 探测不确定 → 失败关闭：不动现状态（绝不停调也绝不恢复）+ fire 告警 + 进 5 分钟循环。
func TestQuotaLifecycleOnUpstreamUncertainFailsClosed(t *testing.T) {
	account := newQuotaLifecycleTHAccount(207)
	repo := newQuotaLifecycleFakeRepo(account)
	alerts := &fakeQuotaAlertStore{}
	probe := &quotaProbeController{outcome: quotaProbeUncertain, err: errors.New("transport timeout")}
	svc := newQuotaLifecycleTestService(repo, alerts, probe)

	err := svc.OnUpstreamQuotaExhausted(context.Background(), account, "")
	require.NoError(t, err)

	// 失败关闭：状态不变——未停调（不 Set 也不 Clear）。
	_, parked := repo.parkedUntil(207)
	require.False(t, parked, "uncertain probe must not park the account")
	require.Zero(t, repo.clearCalls)
	// 告警 fire。
	require.Len(t, alerts.firingEvents(quotaAlertDimsFor(207)), 1)
	// 状态登记 uncertain，进入 5 分钟循环（进程内跟踪）。
	st, ok := cnQuotaLifecycleStateFromExtra(account.Extra)
	require.True(t, ok)
	require.Equal(t, cnQuotaLifecycleStateUncertain, st.State)
	require.Equal(t, cnQuotaLifecycleProbeUncertain, st.LastProbeOutcome)
	svc.mu.Lock()
	_, tracked := svc.tracked[207]
	svc.mu.Unlock()
	require.True(t, tracked, "uncertain account must enter the 5-minute loop")
}

// ---------- sweep：恢复 / 仍耗尽 / 不确定 ----------

// 到期确认探针成功 → 清停调 + resolve 告警 + 刷新快照 + 状态墓碑。
func TestQuotaLifecycleSweepRecoveredClearsParkResolvesAlertRefreshesSnapshot(t *testing.T) {
	account := newQuotaLifecycleTHAccount(207)
	past := quotaLifecycleBase.Add(-time.Hour)
	account.Extra[cnQuotaLifecycleExtraKey] = map[string]any{
		"state": cnQuotaLifecycleStateExhausted, "recovery_at": past.UTC().Format(time.RFC3339),
	}
	repo := newQuotaLifecycleFakeRepo(account)
	require.NoError(t, repo.SetTempUnschedulable(context.Background(), 207, quotaLifecycleBase.Add(24*time.Hour), cnQuotaExhaustedReasonPrefix))
	alerts := &fakeQuotaAlertStore{}
	require.NoError(t, alerts.ResolveQuotaAlertOnRecovery(context.Background(), quotaAlertDimsFor(207))) // no-op 预热
	_, err := alerts.CreateAlertEvent(context.Background(), &OpsAlertEvent{
		Status: OpsAlertStatusFiring, Severity: quotaAlertSeverity, Dimensions: quotaAlertDimsFor(207), FiredAt: past,
	})
	require.NoError(t, err)
	probe := &quotaProbeController{outcome: quotaProbeRecovered}
	svc := newQuotaLifecycleTestService(repo, alerts, probe)
	refresher := &fakeQuotaSnapshotRefresher{}
	svc.SetQuotaSnapshotRefresher(refresher)

	err = svc.RunRecoverySweep(context.Background())
	require.NoError(t, err)

	// 1) 清停调。
	require.Equal(t, 1, repo.clearCalls)
	_, parked := repo.parkedUntil(207)
	require.False(t, parked)
	// 2) 告警 resolve。
	require.Len(t, alerts.resolvedEvents(quotaAlertDimsFor(207)), 1)
	require.Empty(t, alerts.firingEvents(quotaAlertDimsFor(207)))
	// 3) 快照刷新。
	require.Equal(t, 1, refresher.calls)
	// 4) 状态墓碑 recovered。
	st, ok := cnQuotaLifecycleStateFromExtra(account.Extra)
	require.True(t, ok)
	require.Equal(t, cnQuotaLifecycleStateRecovered, st.State)
	require.Equal(t, cnQuotaLifecycleProbeRecovered, st.LastProbeOutcome)
}

// 到期确认探针仍耗尽 → 保持停调（park 已过期则按 L4 续停）+ 告警保持 firing。
func TestQuotaLifecycleSweepStillExhaustedKeepsParked(t *testing.T) {
	account := newQuotaLifecycleKiraAccount(208)
	past := quotaLifecycleBase.Add(-time.Hour)
	account.Extra[cnQuotaLifecycleExtraKey] = map[string]any{
		"state": cnQuotaLifecycleStateExhausted, "recovery_at": past.UTC().Format(time.RFC3339),
	}
	repo := newQuotaLifecycleFakeRepo(account)
	alerts := &fakeQuotaAlertStore{}
	_, err := alerts.CreateAlertEvent(context.Background(), &OpsAlertEvent{
		Status: OpsAlertStatusFiring, Severity: quotaAlertSeverity, Dimensions: quotaAlertDimsFor(208), FiredAt: past,
	})
	require.NoError(t, err)
	probe := &quotaProbeController{outcome: quotaProbeExhausted}
	svc := newQuotaLifecycleTestService(repo, alerts, probe)
	// 候选来自此前不确定循环的进程内跟踪（park 已过期的续停场景）。
	svc.track(208)

	err = svc.RunRecoverySweep(context.Background())
	require.NoError(t, err)

	// 1) 不清除停调，反而按每日重置续停。
	require.Zero(t, repo.clearCalls)
	until, ok := repo.parkedUntil(208)
	require.True(t, ok, "still-exhausted must (re-)park until next daily reset")
	require.Equal(t, kiraNextDailyReset(quotaLifecycleBase), until)
	// 2) 告警保持 firing（未 resolve）。
	require.Len(t, alerts.firingEvents(quotaAlertDimsFor(208)), 1)
	require.Empty(t, alerts.resolvedEvents(quotaAlertDimsFor(208)))
	// 3) 状态仍是 exhausted，探针结果落库。
	st, ok := cnQuotaLifecycleStateFromExtra(account.Extra)
	require.True(t, ok)
	require.Equal(t, cnQuotaLifecycleStateExhausted, st.State)
	require.Equal(t, cnQuotaLifecycleProbeExhausted, st.LastProbeOutcome)
}

// 探测失败路径（sweep 侧不确定）→ 失败关闭：状态不变 + 保持停调，绝不恢复。
func TestQuotaLifecycleSweepProbeFailureNeverRecovers(t *testing.T) {
	account := newQuotaLifecycleTHAccount(207)
	past := quotaLifecycleBase.Add(-time.Hour)
	future := quotaLifecycleBase.Add(24 * time.Hour)
	account.Extra[cnQuotaLifecycleExtraKey] = map[string]any{
		"state": cnQuotaLifecycleStateExhausted, "recovery_at": past.UTC().Format(time.RFC3339),
	}
	repo := newQuotaLifecycleFakeRepo(account)
	require.NoError(t, repo.SetTempUnschedulable(context.Background(), 207, future, cnQuotaExhaustedReasonPrefix))
	alerts := &fakeQuotaAlertStore{}
	_, err := alerts.CreateAlertEvent(context.Background(), &OpsAlertEvent{
		Status: OpsAlertStatusFiring, Severity: quotaAlertSeverity, Dimensions: quotaAlertDimsFor(207), FiredAt: past,
	})
	require.NoError(t, err)
	probe := &quotaProbeController{outcome: quotaProbeUncertain, err: errors.New("upstream 5xx")}
	svc := newQuotaLifecycleTestService(repo, alerts, probe)

	err = svc.RunRecoverySweep(context.Background())
	require.NoError(t, err)

	// 失败关闭：状态不变 + 保持停调（停调到期不被缩短也不被清除）。
	require.Zero(t, repo.clearCalls, "probe failure must never recover the account")
	until, parked := repo.parkedUntil(207)
	require.True(t, parked, "probe failure must keep the account parked")
	require.Equal(t, future, until)
	// 告警保持 firing。
	require.Len(t, alerts.firingEvents(quotaAlertDimsFor(207)), 1)
	require.Empty(t, alerts.resolvedEvents(quotaAlertDimsFor(207)))
	// 探针结果登记 uncertain，5 分钟后再次确认。
	st, ok := cnQuotaLifecycleStateFromExtra(account.Extra)
	require.True(t, ok)
	require.Equal(t, cnQuotaLifecycleProbeUncertain, st.LastProbeOutcome)
}

// ---------- 恢复时间来源优先级（L4） ----------

func TestQuotaLifecycleRecoveryTimeSourcePriority(t *testing.T) {
	upstreamReset := quotaLifecycleBase.Add(48 * time.Hour).UTC().Format(time.RFC3339)

	t.Run("upstream message beats TH snapshot", func(t *testing.T) {
		account := newQuotaLifecycleTHAccount(207)
		repo := newQuotaLifecycleFakeRepo(account)
		alerts := &fakeQuotaAlertStore{}
		probe := &quotaProbeController{outcome: quotaProbeExhausted}
		svc := newQuotaLifecycleTestService(repo, alerts, probe)

		msg := "Your Pass allowance for this period is used; resets at " + upstreamReset
		require.NoError(t, svc.OnUpstreamQuotaExhausted(context.Background(), account, msg))

		until, ok := repo.parkedUntil(207)
		require.True(t, ok)
		want, _ := time.Parse(time.RFC3339, upstreamReset)
		require.Equal(t, want, until, "upstream response reset time must win over panel snapshot")
		st, _ := cnQuotaLifecycleStateFromExtra(account.Extra)
		require.Equal(t, cnQuotaRecoverySourceUpstream, st.RecoverySource)
	})

	t.Run("TH snapshot renews_at when no upstream time", func(t *testing.T) {
		account := newQuotaLifecycleTHAccount(207)
		repo := newQuotaLifecycleFakeRepo(account)
		alerts := &fakeQuotaAlertStore{}
		probe := &quotaProbeController{outcome: quotaProbeExhausted}
		svc := newQuotaLifecycleTestService(repo, alerts, probe)

		require.NoError(t, svc.OnUpstreamQuotaExhausted(context.Background(), account, "Your Pass allowance for this period is used"))

		until, ok := repo.parkedUntil(207)
		require.True(t, ok)
		want, _ := time.Parse(time.RFC3339, quotaLifecycleBase.Add(30*24*time.Hour).UTC().Format(time.RFC3339))
		require.Equal(t, want, until)
		st, _ := cnQuotaLifecycleStateFromExtra(account.Extra)
		require.Equal(t, cnQuotaRecoverySourceSnapshot, st.RecoverySource)
	})

	t.Run("TH free chain without renews_at registers unknown and uses far-future placeholder", func(t *testing.T) {
		account := newQuotaLifecycleTHAccount(207)
		delete(account.Extra, TokenHarborPassSnapshotExtraKey) // 无订阅快照（免费链 7 天滚动无精确时刻）
		repo := newQuotaLifecycleFakeRepo(account)
		alerts := &fakeQuotaAlertStore{}
		probe := &quotaProbeController{outcome: quotaProbeExhausted}
		svc := newQuotaLifecycleTestService(repo, alerts, probe)

		require.NoError(t, svc.OnUpstreamQuotaExhausted(context.Background(), account, ""))

		until, ok := repo.parkedUntil(207)
		require.True(t, ok)
		require.Equal(t, quotaLifecycleBase.Add(quotaLifecycleUnknownRecoveryPlaceholder), until,
			"unknown recovery time must use the far-future placeholder")
		st, _ := cnQuotaLifecycleStateFromExtra(account.Extra)
		require.Equal(t, "", st.RecoveryAt, "unknown recovery time must be registered as unknown")
		require.Equal(t, cnQuotaRecoverySourceUnknown, st.RecoverySource)
	})

	t.Run("Kira daily reset in Vietnam timezone", func(t *testing.T) {
		account := newQuotaLifecycleKiraAccount(208)
		repo := newQuotaLifecycleFakeRepo(account)
		alerts := &fakeQuotaAlertStore{}
		probe := &quotaProbeController{outcome: quotaProbeExhausted}
		svc := newQuotaLifecycleTestService(repo, alerts, probe)

		require.NoError(t, svc.OnUpstreamQuotaExhausted(context.Background(), account, "Insufficient VND wallet balance (0 VND remaining)"))

		until, ok := repo.parkedUntil(208)
		require.True(t, ok)
		require.Equal(t, kiraNextDailyReset(quotaLifecycleBase), until,
			"Kira free pool must park until next daily reset (Vietnam timezone)")
		st, _ := cnQuotaLifecycleStateFromExtra(account.Extra)
		require.Equal(t, cnQuotaRecoverySourceSnapshot, st.RecoverySource)
	})

	t.Run("Kira upstream message time beats daily reset", func(t *testing.T) {
		account := newQuotaLifecycleKiraAccount(208)
		repo := newQuotaLifecycleFakeRepo(account)
		alerts := &fakeQuotaAlertStore{}
		probe := &quotaProbeController{outcome: quotaProbeExhausted}
		svc := newQuotaLifecycleTestService(repo, alerts, probe)

		require.NoError(t, svc.OnUpstreamQuotaExhausted(context.Background(), account, "quota exhausted, resets at "+upstreamReset))

		until, ok := repo.parkedUntil(208)
		require.True(t, ok)
		want, _ := time.Parse(time.RFC3339, upstreamReset)
		require.Equal(t, want, until)
		st, _ := cnQuotaLifecycleStateFromExtra(account.Extra)
		require.Equal(t, cnQuotaRecoverySourceUpstream, st.RecoverySource)
	})
}

// ---------- 5 分钟循环节距 ----------

func TestQuotaLifecycleSweepUnknownRecoveryLoopCadence(t *testing.T) {
	build := func(t *testing.T, lastProbeAt time.Time) (*CNQuotaLifecycleService, *quotaProbeController, *quotaLifecycleFakeRepo) {
		t.Helper()
		account := newQuotaLifecycleTHAccount(207)
		delete(account.Extra, TokenHarborPassSnapshotExtraKey)
		account.Extra[cnQuotaLifecycleExtraKey] = map[string]any{
			"state":        cnQuotaLifecycleStateExhausted,
			"recovery_at":  "", // 恢复时间未知 → 5 分钟兜底循环
			"last_probe_at": lastProbeAt.UTC().Format(time.RFC3339),
		}
		repo := newQuotaLifecycleFakeRepo(account)
		require.NoError(t, repo.SetTempUnschedulable(context.Background(), 207,
			quotaLifecycleBase.Add(quotaLifecycleUnknownRecoveryPlaceholder), cnQuotaExhaustedReasonPrefix))
		alerts := &fakeQuotaAlertStore{}
		probe := &quotaProbeController{outcome: quotaProbeUncertain, err: errors.New("still uncertain")}
		svc := newQuotaLifecycleTestService(repo, alerts, probe)
		return svc, probe, repo
	}

	t.Run("probes after 5 minutes elapsed", func(t *testing.T) {
		svc, probe, _ := build(t, quotaLifecycleBase.Add(-6*time.Minute))
		require.NoError(t, svc.RunRecoverySweep(context.Background()))
		require.Equal(t, 1, probe.calls, "unknown-recovery accounts must be re-probed every 5 minutes")
	})

	t.Run("skips within 5 minutes of last probe", func(t *testing.T) {
		svc, probe, _ := build(t, quotaLifecycleBase.Add(-time.Minute))
		require.NoError(t, svc.RunRecoverySweep(context.Background()))
		require.Zero(t, probe.calls, "must not re-probe within the 5-minute cadence")
	})

	t.Run("skips when official recovery time is still in the future", func(t *testing.T) {
		account := newQuotaLifecycleTHAccount(207)
		account.Extra[cnQuotaLifecycleExtraKey] = map[string]any{
			"state":       cnQuotaLifecycleStateExhausted,
			"recovery_at": quotaLifecycleBase.Add(24 * time.Hour).UTC().Format(time.RFC3339),
		}
		repo := newQuotaLifecycleFakeRepo(account)
		require.NoError(t, repo.SetTempUnschedulable(context.Background(), 207, quotaLifecycleBase.Add(24*time.Hour), cnQuotaExhaustedReasonPrefix))
		alerts := &fakeQuotaAlertStore{}
		probe := &quotaProbeController{outcome: quotaProbeRecovered}
		svc := newQuotaLifecycleTestService(repo, alerts, probe)

		require.NoError(t, svc.RunRecoverySweep(context.Background()))
		require.Zero(t, probe.calls, "must not probe before the official recovery time")
		require.Zero(t, repo.clearCalls)
	})
}

// ---------- 非管辖账号 ----------

func TestQuotaLifecycleNonManagedAccountNoOp(t *testing.T) {
	account := &Account{
		ID: 999, Platform: PlatformKimi, Type: AccountTypeAPIKey, Status: StatusActive,
		Credentials: map[string]any{"api_key": "sk-kimi", "base_url": "https://api.moonshot.cn/v1"},
		Extra:       map[string]any{},
	}
	repo := newQuotaLifecycleFakeRepo(account)
	alerts := &fakeQuotaAlertStore{}
	probe := &quotaProbeController{outcome: quotaProbeExhausted}
	svc := newQuotaLifecycleTestService(repo, alerts, probe)

	require.NoError(t, svc.OnUpstreamQuotaExhausted(context.Background(), account, "quota exceeded"))
	require.Zero(t, probe.calls, "non-managed accounts must not be probed")
	_, parked := repo.parkedUntil(999)
	require.False(t, parked)
	require.Empty(t, alerts.events)
}

// ---------- TH 确认探针分类与请求形状 ----------

type quotaCaptureUpstream struct {
	mu         sync.Mutex
	statusCode int
	body       string
	err        error
	lastPath   string
	lastBody   string
	lastAuth   string
	doCalls    int
	tlsCalls   int
}

func (u *quotaCaptureUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.doCalls++
	return u.serve(req)
}

func (u *quotaCaptureUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.tlsCalls++
	return u.serve(req)
}

func (u *quotaCaptureUpstream) serve(req *http.Request) (*http.Response, error) {
	if u.err != nil {
		return nil, u.err
	}
	u.lastPath = req.URL.Path
	u.lastAuth = req.Header.Get("Authorization")
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		u.lastBody = string(raw)
	}
	return &http.Response{
		StatusCode: u.statusCode,
		Body:       io.NopCloser(strings.NewReader(u.body)),
		Header:     make(http.Header),
	}, nil
}

func TestQuotaLifecycleTokenHarborProbeClassification(t *testing.T) {
	account := newQuotaLifecycleTHAccount(207)
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}

	t.Run("request shape is max_tokens=1 ping on raw model", func(t *testing.T) {
		upstream := &quotaCaptureUpstream{statusCode: http.StatusOK, body: "{}"}
		svc := NewCNQuotaLifecycleService(newQuotaLifecycleFakeRepo(account), upstream, cfg, nil)
		outcome, err := svc.probeTokenHarborExhaustion(context.Background(), account)
		require.NoError(t, err)
		require.Equal(t, quotaProbeRecovered, outcome)
		require.Equal(t, 1, upstream.tlsCalls)
		require.True(t, strings.HasSuffix(upstream.lastPath, "/chat/completions"))
		require.Equal(t, "Bearer sk-th-test", upstream.lastAuth)
		var body map[string]any
		require.NoError(t, json.Unmarshal([]byte(upstream.lastBody), &body))
		require.Equal(t, float64(1), body["max_tokens"], "confirmation probe must be a max_tokens=1 ping")
		require.Equal(t, "raw-model", body["model"], "probe must use the raw (reverse-mapped) model")
	})

	t.Run("402 confirms exhausted", func(t *testing.T) {
		upstream := &quotaCaptureUpstream{statusCode: http.StatusPaymentRequired, body: `{"error":{"message":"Your Pass allowance for this period is used"}}`}
		svc := NewCNQuotaLifecycleService(newQuotaLifecycleFakeRepo(account), upstream, cfg, nil)
		outcome, err := svc.probeTokenHarborExhaustion(context.Background(), account)
		require.NoError(t, err)
		require.Equal(t, quotaProbeExhausted, outcome)
	})

	t.Run("free-tier 429 confirms exhausted", func(t *testing.T) {
		upstream := &quotaCaptureUpstream{statusCode: http.StatusTooManyRequests, body: `{"error":{"type":"free_tier_limit_reached"}}`}
		svc := NewCNQuotaLifecycleService(newQuotaLifecycleFakeRepo(account), upstream, cfg, nil)
		outcome, err := svc.probeTokenHarborExhaustion(context.Background(), account)
		require.NoError(t, err)
		require.Equal(t, quotaProbeExhausted, outcome)
	})

	t.Run("5xx and 401 are uncertain (fail-closed)", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			code int
			body string
		}{
			{"5xx", http.StatusInternalServerError, `{}`},
			{"401", http.StatusUnauthorized, `{}`},
			{"plain 429", http.StatusTooManyRequests, `{"error":{"message":"rate limited"}}`},
		} {
			upstream := &quotaCaptureUpstream{statusCode: tc.code, body: tc.body}
			svc := NewCNQuotaLifecycleService(newQuotaLifecycleFakeRepo(account), upstream, cfg, nil)
			outcome, _ := svc.probeTokenHarborExhaustion(context.Background(), account)
			require.Equal(t, quotaProbeUncertain, outcome, tc.name)
		}
	})

	t.Run("transport error is uncertain", func(t *testing.T) {
		upstream := &quotaCaptureUpstream{err: errors.New("connection refused")}
		svc := NewCNQuotaLifecycleService(newQuotaLifecycleFakeRepo(account), upstream, cfg, nil)
		outcome, err := svc.probeTokenHarborExhaustion(context.Background(), account)
		require.Error(t, err)
		require.Equal(t, quotaProbeUncertain, outcome)
	})
}

// ---------- Kira 确认探针分类 ----------

func TestQuotaLifecycleKiraProbeClassification(t *testing.T) {
	account := newQuotaLifecycleKiraAccount(208)
	cfg := &config.Config{}

	t.Run("free pool headroom means recovered", func(t *testing.T) {
		upstream := &quotaCaptureUpstream{statusCode: http.StatusOK,
			body: `{"summary":{"tokensUsedToday":1000,"freeDailyLimit":6000000,"vndBalance":0}}`}
		svc := NewCNQuotaLifecycleService(newQuotaLifecycleFakeRepo(account), upstream, cfg, nil)
		outcome, err := svc.probeKiraExhaustion(context.Background(), account)
		require.NoError(t, err)
		require.Equal(t, quotaProbeRecovered, outcome)
	})

	t.Run("free pool exhausted confirms exhausted", func(t *testing.T) {
		upstream := &quotaCaptureUpstream{statusCode: http.StatusOK,
			body: `{"summary":{"tokensUsedToday":6000000,"freeDailyLimit":6000000,"vndBalance":0}}`}
		svc := NewCNQuotaLifecycleService(newQuotaLifecycleFakeRepo(account), upstream, cfg, nil)
		outcome, err := svc.probeKiraExhaustion(context.Background(), account)
		require.NoError(t, err)
		require.Equal(t, quotaProbeExhausted, outcome)
	})

	t.Run("missing summary is uncertain", func(t *testing.T) {
		upstream := &quotaCaptureUpstream{statusCode: http.StatusOK, body: `{"unexpected":true}`}
		svc := NewCNQuotaLifecycleService(newQuotaLifecycleFakeRepo(account), upstream, cfg, nil)
		outcome, _ := svc.probeKiraExhaustion(context.Background(), account)
		require.Equal(t, quotaProbeUncertain, outcome)
	})

	t.Run("non-2xx is uncertain (fail-closed)", func(t *testing.T) {
		upstream := &quotaCaptureUpstream{statusCode: http.StatusInternalServerError, body: `{}`}
		svc := NewCNQuotaLifecycleService(newQuotaLifecycleFakeRepo(account), upstream, cfg, nil)
		outcome, _ := svc.probeKiraExhaustion(context.Background(), account)
		require.Equal(t, quotaProbeUncertain, outcome)
	})
}
