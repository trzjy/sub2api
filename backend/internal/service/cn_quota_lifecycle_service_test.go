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
				"has_pass":       true,
				"pass_name":      "Agent Pass",
				"renews_at":      quotaLifecycleBase.Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339), // 订阅续期日（≈28 天），不参与恢复判定
				"reset_at":       quotaLifecycleBase.Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339),  // free-tier 真实额度周期重置时刻
				"plan_exhausted": true,
				"fetched_at":     quotaLifecycleBase.Format(time.RFC3339),
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

// ShortenTempUnschedulableIfOwned 实现 F4 受控缩短原语（派发单 C1-a-r3）：单条条件
// 缩短，镜像生产 SQL 的四条硬约束——① until>newUntil 且 reason 以 ownedReasonPrefix
// 前缀开头才缩短；② 非前缀拥有不动；③ newUntil>=现有 until 时 no-op（不延长）；
// ④ 不改 reason、不清除其他字段。返回是否实际缩短。
func (r *quotaLifecycleFakeRepo) ShortenTempUnschedulableIfOwned(_ context.Context, id int64, newUntil time.Time, ownedReasonPrefix string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.parked[id]
	if !ok {
		return false, nil
	}
	if !rec.until.After(newUntil) || !strings.HasPrefix(rec.reason, ownedReasonPrefix) {
		return false, nil
	}
	r.parked[id] = quotaParkedRecord{until: newUntil, reason: rec.reason}
	if a, ok := r.accounts[id]; ok {
		u := newUntil
		a.TempUnschedulableUntil = &u
		// 约束 ④：reason 不变。
	}
	return true, nil
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

// 确认探针属实 → 停调至官方恢复时间（TH free-tier reset_at，非订阅续期日 renews_at）+ fire 告警 + 状态落库。
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
	// 2) 停调到期 = 官方恢复时间（th_pass_snapshot.reset_at，非订阅续期日 renews_at），非滚动冷却。
	until, ok := repo.parkedUntil(207)
	require.True(t, ok, "confirmed exhaustion must park the account")
	resetAt, _ := time.Parse(time.RFC3339, quotaLifecycleBase.Add(7*24*time.Hour).UTC().Format(time.RFC3339))
	require.Equal(t, resetAt, until)
	require.True(t, strings.HasPrefix(repo.parkedReason(207), cnQuotaExhaustedReasonPrefix))
	// 3) 告警 fire（kind=quota_exhausted，账号级维度）且恰好一条。
	fired := alerts.firingEvents(quotaAlertDimsFor(207))
	require.Len(t, fired, 1)
	require.Equal(t, OpsAlertStatusFiring, fired[0].Status)
	require.Equal(t, quotaAlertSeverity, fired[0].Severity)
	// 4) 状态落库：exhausted + recovery_at=reset_at（非 renews_at）+ 来源 snapshot。
	st, ok := cnQuotaLifecycleStateFromExtra(account.Extra)
	require.True(t, ok)
	require.Equal(t, cnQuotaLifecycleStateExhausted, st.State)
	require.Equal(t, resetAt.UTC().Format(time.RFC3339), st.RecoveryAt)
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

	t.Run("TH snapshot reset_at when no upstream time", func(t *testing.T) {
		account := newQuotaLifecycleTHAccount(207)
		repo := newQuotaLifecycleFakeRepo(account)
		alerts := &fakeQuotaAlertStore{}
		probe := &quotaProbeController{outcome: quotaProbeExhausted}
		svc := newQuotaLifecycleTestService(repo, alerts, probe)

		require.NoError(t, svc.OnUpstreamQuotaExhausted(context.Background(), account, "Your Pass allowance for this period is used"))

		until, ok := repo.parkedUntil(207)
		require.True(t, ok)
		want, _ := time.Parse(time.RFC3339, quotaLifecycleBase.Add(7*24*time.Hour).UTC().Format(time.RFC3339))
		require.Equal(t, want, until)
		st, _ := cnQuotaLifecycleStateFromExtra(account.Extra)
		require.Equal(t, cnQuotaRecoverySourceSnapshot, st.RecoverySource)
	})

	t.Run("TH without reset_at registers unknown and uses far-future placeholder", func(t *testing.T) {
		account := newQuotaLifecycleTHAccount(207)
		delete(account.Extra, TokenHarborPassSnapshotExtraKey) // 无 th_pass_snapshot（无 reset_at → 免费链 7 天滚动无精确时刻）
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

// ---------- TH 恢复来源 = free-tier reset_at（D-QLM-007 §1）----------

// TH 快照 reset_at 含未来值 → recovery_at=reset_at、source=snapshot（不采用 renews_at）。
func TestQuotaLifecycleTHResetAtFutureParksToResetAt(t *testing.T) {
	account := newQuotaLifecycleTHAccount(207)
	repo := newQuotaLifecycleFakeRepo(account)
	alerts := &fakeQuotaAlertStore{}
	probe := &quotaProbeController{outcome: quotaProbeExhausted}
	svc := newQuotaLifecycleTestService(repo, alerts, probe)

	require.NoError(t, svc.OnUpstreamQuotaExhausted(context.Background(), account, "Your Pass allowance for this period is used"))

	until, ok := repo.parkedUntil(207)
	require.True(t, ok)
	want, _ := time.Parse(time.RFC3339, quotaLifecycleBase.Add(7*24*time.Hour).UTC().Format(time.RFC3339))
	require.Equal(t, want, until, "TH recovery must use free-tier reset_at, not subscription renews_at")
	st, _ := cnQuotaLifecycleStateFromExtra(account.Extra)
	require.Equal(t, want.UTC().Format(time.RFC3339), st.RecoveryAt)
	require.Equal(t, cnQuotaRecoverySourceSnapshot, st.RecoverySource)
}

// TH 快照 reset_at 已过期且 plan_exhausted=true → 登记 unknown，走 5 分钟兜底循环。
func TestQuotaLifecycleTHResetAtExpiredFallsToUnknown(t *testing.T) {
	account := newQuotaLifecycleTHAccount(207)
	expired := quotaLifecycleBase.Add(-time.Hour)
	account.Extra[TokenHarborPassSnapshotExtraKey] = map[string]any{
		"has_pass":       true,
		"pass_name":      "Agent Pass",
		"renews_at":      quotaLifecycleBase.Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339),
		"reset_at":       expired.UTC().Format(time.RFC3339),
		"plan_exhausted": true,
		"fetched_at":     quotaLifecycleBase.Format(time.RFC3339),
	}
	repo := newQuotaLifecycleFakeRepo(account)
	alerts := &fakeQuotaAlertStore{}
	probe := &quotaProbeController{outcome: quotaProbeExhausted}
	svc := newQuotaLifecycleTestService(repo, alerts, probe)

	require.NoError(t, svc.OnUpstreamQuotaExhausted(context.Background(), account, "Your Pass allowance for this period is used"))

	until, ok := repo.parkedUntil(207)
	require.True(t, ok)
	require.Equal(t, quotaLifecycleBase.Add(quotaLifecycleUnknownRecoveryPlaceholder), until,
		"expired reset_at with plan_exhausted must fall to the unknown far-future placeholder")
	st, _ := cnQuotaLifecycleStateFromExtra(account.Extra)
	require.Equal(t, "", st.RecoveryAt, "expired reset_at must register unknown recovery time")
	require.Equal(t, cnQuotaRecoverySourceUnknown, st.RecoverySource)
}

// ---------- TH 存量收敛（D-QLM-007 §2）----------

// 存量收敛（cur 早于新 reset_at，D-QLM-012 §2 单调守卫下仅此形态才收敛）：
// 账号 extra 已存 renewsAt 口径的 recovery_at 且早于新快照 reset_at，刷新后
// 快照带 plan_exhausted=true + 新 reset_at，则 recovery_at 被改写为 reset_at。
func TestQuotaLifecycleTHStockRecoveryConvergesRenewsAtToResetAt(t *testing.T) {
	account := newQuotaLifecycleTHAccount(207)
	// 模拟线上存量：历史按 renewsAt 写入的 recovery_at（早于新窗口 reset_at）。
	oldRenewsAt := quotaLifecycleBase.Add(3 * 24 * time.Hour)
	account.Extra[cnQuotaLifecycleExtraKey] = map[string]any{
		"state":             cnQuotaLifecycleStateExhausted,
		"recovery_at":       oldRenewsAt.UTC().Format(time.RFC3339),
		"recovery_source":   cnQuotaRecoverySourceSnapshot,
		"last_probe_at":     quotaLifecycleBase.Format(time.RFC3339),
		"last_probe_outcome": cnQuotaLifecycleProbeExhausted,
		"updated_at":        quotaLifecycleBase.Format(time.RFC3339),
	}
	// 刷新后快照带 plan_exhausted=true + 新 reset_at（7 天窗口，比残留值晚）。
	newResetAt := quotaLifecycleBase.Add(7 * 24 * time.Hour)
	account.Extra[TokenHarborPassSnapshotExtraKey] = map[string]any{
		"has_pass":       true,
		"pass_name":      "Agent Pass",
		"renews_at":      quotaLifecycleBase.Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339),
		"reset_at":       newResetAt.UTC().Format(time.RFC3339),
		"plan_exhausted": true,
		"fetched_at":     quotaLifecycleBase.Format(time.RFC3339),
	}
	repo := newQuotaLifecycleFakeRepo(account)
	svc := newQuotaLifecycleTestService(repo, &fakeQuotaAlertStore{}, &quotaProbeController{outcome: quotaProbeExhausted})

	svc.convergeTHStockRecovery(context.Background(), account)

	// 既存的 renewsAt 口径 recovery_at 必须被收敛到最新 reset_at。
	st, ok := cnQuotaLifecycleStateFromExtra(account.Extra)
	require.True(t, ok)
	require.NotEqual(t, oldRenewsAt.UTC().Format(time.RFC3339), st.RecoveryAt,
		"stale renewsAt-based recovery_at must be overwritten")
	require.Equal(t, newResetAt.UTC().Format(time.RFC3339), st.RecoveryAt,
		"recovery_at must converge from old renewsAt to the latest reset_at")
	require.Equal(t, cnQuotaRecoverySourceSnapshot, st.RecoverySource)
}

// 存量收敛边界：plan_exhausted=true 且 reset_at 已过期，且 lifecycle 残留的
// recovery_at 本身也已过期（或为空）→ 无残留可清，维持现状语义不变
//（不把过期 reset_at 写回 recovery_at，残留已过期时 unknown 循环本就按期推进）。
func TestQuotaLifecycleTHStockRecoverySkipsExpiredResetAt(t *testing.T) {
	account := newQuotaLifecycleTHAccount(207)
	expiredResidual := quotaLifecycleBase.Add(-time.Hour)
	account.Extra[cnQuotaLifecycleExtraKey] = map[string]any{
		"state":       cnQuotaLifecycleStateExhausted,
		"recovery_at": expiredResidual.UTC().Format(time.RFC3339),
		"updated_at":  quotaLifecycleBase.Format(time.RFC3339),
	}
	expired := quotaLifecycleBase.Add(-time.Hour)
	account.Extra[TokenHarborPassSnapshotExtraKey] = map[string]any{
		"has_pass":       true,
		"reset_at":       expired.UTC().Format(time.RFC3339),
		"plan_exhausted": true,
	}
	repo := newQuotaLifecycleFakeRepo(account)
	svc := newQuotaLifecycleTestService(repo, &fakeQuotaAlertStore{}, &quotaProbeController{outcome: quotaProbeExhausted})

	svc.convergeTHStockRecovery(context.Background(), account)

	// 过期 reset_at 且残留 recovery_at 本身已过期：无残留可清，recovery_at 保持
	// 不变（维持现状语义，交由 unknown 兜底循环）。
	st, ok := cnQuotaLifecycleStateFromExtra(account.Extra)
	require.True(t, ok)
	require.Equal(t, expiredResidual.UTC().Format(time.RFC3339), st.RecoveryAt,
		"expired reset_at must not be written back as recovery_at")
}

// 存量收敛回归（D-QLM-016 d）：reset_at 已过期/缺失但 lifecycle 残留未来的
// recovery_at → converge 对 repo 零写入（写侧清理已删除，残留由 sweep 读侧判定
// 推进，见 TestQuotaLifecycleSweepProbesTHResidualMismatch...）。
func TestQuotaLifecycleTHStockRecoveryExpiredResetAtWritesNothing(t *testing.T) {
	account := newQuotaLifecycleTHAccount(207)
	// 残留：旧口径 renewsAt（≈28 天后，在未来）。
	oldRenewsAt := quotaLifecycleBase.Add(28 * 24 * time.Hour)
	account.Extra[cnQuotaLifecycleExtraKey] = map[string]any{
		"state":       cnQuotaLifecycleStateExhausted,
		"recovery_at": oldRenewsAt.UTC().Format(time.RFC3339),
		"updated_at":  quotaLifecycleBase.Format(time.RFC3339),
	}
	// 快照：plan_exhausted=true 但 reset_at 已过期。
	expired := quotaLifecycleBase.Add(-time.Hour)
	account.Extra[TokenHarborPassSnapshotExtraKey] = map[string]any{
		"has_pass":       true,
		"reset_at":       expired.UTC().Format(time.RFC3339),
		"plan_exhausted": true,
	}
	repo := newQuotaLifecycleFakeRepo(account)
	svc := newQuotaLifecycleTestService(repo, &fakeQuotaAlertStore{}, &quotaProbeController{outcome: quotaProbeExhausted})

	svc.convergeTHStockRecovery(context.Background(), account)

	// 零写入回归断言：残留 recovery_at 原样保留、repo 与内存一致、无 unknown 改写。
	st, ok := cnQuotaLifecycleStateFromExtra(account.Extra)
	require.True(t, ok)
	require.Equal(t, oldRenewsAt.UTC().Format(time.RFC3339), st.RecoveryAt,
		"converge must not clear the residual recovery_at (zero-write on expired reset_at)")
	require.Equal(t, oldRenewsAt.UTC().Format(time.RFC3339), repo.accounts[207].Extra[cnQuotaLifecycleExtraKey].(map[string]any)["recovery_at"],
		"converge must write nothing to the repo on expired reset_at")
}

// 存量收敛守卫（D-QLM-012 §2 单调）：cur recovery_at 已晚于快照 reset_at →
// 不改写（官方 reset_at 随窗口单调递增，cur 更晚只可能是并发旧快照后写）。
func TestQuotaLifecycleTHStockRecoveryNeverRollsBackLaterRecovery(t *testing.T) {
	account := newQuotaLifecycleTHAccount(207)
	// cur：确认路径已写入的新官方恢复时间（比刷新链旧快照的 reset_at 晚）。
	curRecoveryAt := quotaLifecycleBase.Add(14 * 24 * time.Hour)
	account.Extra[cnQuotaLifecycleExtraKey] = map[string]any{
		"state":       cnQuotaLifecycleStateExhausted,
		"recovery_at": curRecoveryAt.UTC().Format(time.RFC3339),
		"updated_at":  quotaLifecycleBase.Format(time.RFC3339),
	}
	// 并发旧快照：reset_at 早于 cur。
	oldResetAt := quotaLifecycleBase.Add(7 * 24 * time.Hour)
	account.Extra[TokenHarborPassSnapshotExtraKey] = map[string]any{
		"has_pass":       true,
		"reset_at":       oldResetAt.UTC().Format(time.RFC3339),
		"plan_exhausted": true,
	}
	repo := newQuotaLifecycleFakeRepo(account)
	svc := newQuotaLifecycleTestService(repo, &fakeQuotaAlertStore{}, &quotaProbeController{outcome: quotaProbeExhausted})

	svc.convergeTHStockRecovery(context.Background(), account)

	st, ok := cnQuotaLifecycleStateFromExtra(account.Extra)
	require.True(t, ok)
	require.Equal(t, curRecoveryAt.UTC().Format(time.RFC3339), st.RecoveryAt,
		"later recovery_at must never be rolled back to an older snapshot reset_at")
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

// sweep 读侧矛盾判定六向断言（D-QLM-016 a/b/c + D-QLM-017 来源允许清单收窄 +
// D-QLM-018 第三形态 + D-QLM-021 第三形态去 pe 前提）：lifecycle 残留未来
// recovery_at（旧口径 renewsAt 存量）且快照侧 reset_at 呈矛盾形态之一 → 快照与
// 恢复时间自相矛盾，probeDue=false 仍被 sweep 选中探针（unknown 确认循环可达）。
// 矛盾形态：reset_at 已过期（016，要求 pe=true）或停调时刻晚于未来 reset_at
//（018/021，时间矛盾独立成立、pe 值不作前提）；来源按显式允许清单只认 snapshot
//（旧 renewsAt 存量唯一来源）——upstream（L4 最权威）与未知来源（如 "mystery"，
// 覆盖任意未知/未来新增 RecoverySource）不触发；pe=false 不触发的是第一/二形态
//（过期 reset_at 负控，021）；cur 早于未来 reset_at 属 converge 正常收敛域，
// 不触发。
func TestQuotaLifecycleSweepProbesTHResidualMismatch(t *testing.T) {
	// newResidualAccount 构造「残留未来 recovery_at + 过期 reset_at」的停调 TH 账号；
	// 供 018 用例经 overrideResetAt/overrideRecoveryAt 覆写为矛盾第三形态。
	newResidualAccount := func(id int64, source string, planExhausted bool) *Account {
		account := newQuotaLifecycleTHAccount(id)
		account.Extra[cnQuotaLifecycleExtraKey] = map[string]any{
			"state":           cnQuotaLifecycleStateExhausted,
			"recovery_at":     quotaLifecycleBase.Add(28 * 24 * time.Hour).UTC().Format(time.RFC3339),
			"recovery_source": source,
			"last_probe_at":   quotaLifecycleBase.Add(-time.Minute).UTC().Format(time.RFC3339),
		}
		account.Extra[TokenHarborPassSnapshotExtraKey] = map[string]any{
			"has_pass":       true,
			"renews_at":      quotaLifecycleBase.Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339),
			"reset_at":       quotaLifecycleBase.Add(-time.Hour).UTC().Format(time.RFC3339),
			"plan_exhausted": planExhausted,
			"fetched_at":     quotaLifecycleBase.Format(time.RFC3339),
		}
		return account
	}
	// newFutureResetAccount 构造「停调晚于未来 reset_at」形态（D-QLM-018）：残留
	// recovery_at=cur、快照 reset_at=resetAt 均可指定，reset_at 在未来。
	newFutureResetAccount := func(id int64, source string, planExhausted bool, cur, resetAt time.Time) *Account {
		account := newResidualAccount(id, source, planExhausted)
		lc := account.Extra[cnQuotaLifecycleExtraKey].(map[string]any)
		lc["recovery_at"] = cur.UTC().Format(time.RFC3339)
		snap := account.Extra[TokenHarborPassSnapshotExtraKey].(map[string]any)
		snap["reset_at"] = resetAt.UTC().Format(time.RFC3339)
		return account
	}
	park := func(repo *quotaLifecycleFakeRepo) {
		require.NoError(t, repo.SetTempUnschedulable(context.Background(), 207,
			quotaLifecycleBase.Add(28*24*time.Hour), cnQuotaExhaustedReasonPrefix))
	}

	t.Run("residual future recovery with expired reset_at is probed", func(t *testing.T) {
		account := newResidualAccount(207, cnQuotaRecoverySourceSnapshot, true)
		repo := newQuotaLifecycleFakeRepo(account)
		park(repo)
		probe := &quotaProbeController{outcome: quotaProbeRecovered}
		svc := newQuotaLifecycleTestService(repo, &fakeQuotaAlertStore{}, probe)

		// 前置：残留形态下 probeDue 确为 false，选中只能来自读侧矛盾判定。
		st, ok := cnQuotaLifecycleStateFromExtra(account.Extra)
		require.True(t, ok)
		require.False(t, st.probeDue(quotaLifecycleBase, quotaLifecycleSweepInterval),
			"probeDue must be false while residual recovery_at is in the future")

		require.NoError(t, svc.RunRecoverySweep(context.Background()))
		require.Equal(t, 1, probe.calls, "snapshot/lifecycle contradiction must reach the confirmation probe")
		require.Equal(t, 1, repo.clearCalls, "recovered probe closes the loop by clearing the park")
	})

	t.Run("future_reset_at_with_later_parked_recovery_is_probed", func(t *testing.T) {
		// D-QLM-018 第三形态：停调到期点（renewsAt 旧口径，+28d）晚于官方未来
		// reset_at（+6d，生产实证 2026-10-08）→ 矛盾成立，探针触发。
		account := newFutureResetAccount(207, cnQuotaRecoverySourceSnapshot, true,
			quotaLifecycleBase.Add(28*24*time.Hour), quotaLifecycleBase.Add(6*24*time.Hour))
		repo := newQuotaLifecycleFakeRepo(account)
		park(repo)
		probe := &quotaProbeController{outcome: quotaProbeRecovered}
		svc := newQuotaLifecycleTestService(repo, &fakeQuotaAlertStore{}, probe)

		// 前置：probeDue=false（残留 recovery_at 在未来），选中只能来自读侧第三形态。
		st, ok := cnQuotaLifecycleStateFromExtra(account.Extra)
		require.True(t, ok)
		require.False(t, st.probeDue(quotaLifecycleBase, quotaLifecycleSweepInterval))

		require.NoError(t, svc.RunRecoverySweep(context.Background()))
		require.Equal(t, 1, probe.calls, "parked recovery later than a future reset_at is a contradiction and must reach the probe")
		require.Equal(t, 1, repo.clearCalls, "recovered probe closes the loop by clearing the park")
	})

	t.Run("upstream source is not triggered", func(t *testing.T) {
		account := newResidualAccount(207, cnQuotaRecoverySourceUpstream, true)
		repo := newQuotaLifecycleFakeRepo(account)
		park(repo)
		probe := &quotaProbeController{outcome: quotaProbeRecovered}
		svc := newQuotaLifecycleTestService(repo, &fakeQuotaAlertStore{}, probe)

		require.NoError(t, svc.RunRecoverySweep(context.Background()))
		require.Zero(t, probe.calls, "upstream-sourced recovery_at must keep its own deadline semantics")
		require.Zero(t, repo.clearCalls)
	})

	t.Run("unknown source is not triggered", func(t *testing.T) {
		// D-QLM-017：来源允许清单收窄为显式 snapshot 后，任意未知/未来新增
		// RecoverySource 一律不触发残留判定（fail-closed）。
		account := newResidualAccount(207, "mystery", true)
		repo := newQuotaLifecycleFakeRepo(account)
		park(repo)
		probe := &quotaProbeController{outcome: quotaProbeRecovered}
		svc := newQuotaLifecycleTestService(repo, &fakeQuotaAlertStore{}, probe)

		require.NoError(t, svc.RunRecoverySweep(context.Background()))
		require.Zero(t, probe.calls, "unknown-sourced recovery_at must not trigger the residual mismatch gate")
		require.Zero(t, repo.clearCalls)
	})

	t.Run("plan_exhausted_false_with_later_parked_is_probed", func(t *testing.T) {
		// D-QLM-021：第三形态以时间矛盾独立成立（pe 值不作前提）——pe=false +
		// cur > reset_at（停调晚于官方口径）→ 触发探针（生产实证 2026-10-08
		// 第二轮 4 账号形态：官方已恢复 pe=false 但停调挂旧口径）。
		account := newFutureResetAccount(207, cnQuotaRecoverySourceSnapshot, false,
			quotaLifecycleBase.Add(28*24*time.Hour), quotaLifecycleBase.Add(6*24*time.Hour))
		repo := newQuotaLifecycleFakeRepo(account)
		park(repo)
		probe := &quotaProbeController{outcome: quotaProbeRecovered}
		svc := newQuotaLifecycleTestService(repo, &fakeQuotaAlertStore{}, probe)

		// 前置：probeDue=false（残留 recovery_at 在未来），选中只能来自读侧第三形态。
		st, ok := cnQuotaLifecycleStateFromExtra(account.Extra)
		require.True(t, ok)
		require.False(t, st.probeDue(quotaLifecycleBase, quotaLifecycleSweepInterval))

		require.NoError(t, svc.RunRecoverySweep(context.Background()))
		require.Equal(t, 1, probe.calls, "later parked than reset_at is a time contradiction regardless of plan_exhausted")
		require.Equal(t, 1, repo.clearCalls, "recovered probe closes the loop by clearing the park")
	})

	t.Run("plan_exhausted_false_with_expired_reset_at_is_probed", func(t *testing.T) {
		// D-QLM-021-r2：pe=false + reset_at 已过期（官方说健康、窗口已滚、停调
		// 挂旧口径未来值）是最纯的时间矛盾，经第三形态独立触发探针。
		account := newResidualAccount(207, cnQuotaRecoverySourceSnapshot, false)
		repo := newQuotaLifecycleFakeRepo(account)
		park(repo)
		probe := &quotaProbeController{outcome: quotaProbeRecovered}
		svc := newQuotaLifecycleTestService(repo, &fakeQuotaAlertStore{}, probe)

		require.NoError(t, svc.RunRecoverySweep(context.Background()))
		require.Equal(t, 1, probe.calls, "pe=false with expired reset_at is the purest time contradiction for the third form")
		require.Equal(t, 1, repo.clearCalls, "recovered probe closes the loop by clearing the park")
	})

	t.Run("earlier_parked_recovery_is_not_probed", func(t *testing.T) {
		// D-QLM-018 负控：cur（+1d）早于未来 reset_at（+6d）属 converge 正常收敛
		// 域，不算矛盾，不触发读侧判定。
		account := newFutureResetAccount(207, cnQuotaRecoverySourceSnapshot, true,
			quotaLifecycleBase.Add(1*24*time.Hour), quotaLifecycleBase.Add(6*24*time.Hour))
		repo := newQuotaLifecycleFakeRepo(account)
		require.NoError(t, repo.SetTempUnschedulable(context.Background(), 207,
			quotaLifecycleBase.Add(1*24*time.Hour), cnQuotaExhaustedReasonPrefix))
		probe := &quotaProbeController{outcome: quotaProbeRecovered}
		svc := newQuotaLifecycleTestService(repo, &fakeQuotaAlertStore{}, probe)

		require.NoError(t, svc.RunRecoverySweep(context.Background()))
		require.Zero(t, probe.calls, "parked recovery earlier than a future reset_at is converge's normal domain, not a contradiction")
		require.Zero(t, repo.clearCalls)
	})
}

// ---------- 读侧残留命中后的定向断言（D-QLM-017 §2 a/b）----------

// 读侧矛盾判定命中后的两条定向断言（D-QLM-017，不改生产代码）：
// a. 探针仍耗尽 → 按 resolveRecoveryTime 解析值重停（temp_unschedulable 更新）
//    且 lifecycle 写入可达的下一次确认时间；
// b. 探针返回错误 → 失败关闭：保持原停调不变、告警保持 firing、确认循环不丢
//    （读侧矛盾证据仍在，下一轮 sweep 再次探针）。生产 sweepProbeAccount 不确定
//    分支按既有契约返回 nil（错误仅告警日志，见 TestQuotaLifecycleSweepProbeFailureNeverRecovers
//    的 NoError 断言），故此处断言失败关闭不变量而非函数错误返回。
func TestQuotaLifecycleSweepResidualHitStillExhaustedAndProbeError(t *testing.T) {
	// newResidualTHAccount 构造「读侧矛盾」残留形态的停调 TH 账号：lifecycle 挂着
	// 未来的 residualRecoveryAt（来源 snapshot，旧口径存量），快照侧
	// plan_exhausted=true 且 reset_at 已过期。
	newResidualTHAccount := func(id int64, residualRecoveryAt time.Time) *Account {
		account := newQuotaLifecycleTHAccount(id)
		account.Extra[cnQuotaLifecycleExtraKey] = map[string]any{
			"state":           cnQuotaLifecycleStateExhausted,
			"recovery_at":     residualRecoveryAt.UTC().Format(time.RFC3339),
			"recovery_source": cnQuotaRecoverySourceSnapshot,
			"last_probe_at":   quotaLifecycleBase.Add(-time.Minute).UTC().Format(time.RFC3339),
		}
		account.Extra[TokenHarborPassSnapshotExtraKey] = map[string]any{
			"has_pass":       true,
			"renews_at":      quotaLifecycleBase.Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339),
			"reset_at":       quotaLifecycleBase.Add(-time.Hour).UTC().Format(time.RFC3339),
			"plan_exhausted": true,
			"fetched_at":     quotaLifecycleBase.Format(time.RFC3339),
		}
		return account
	}
	precreateFiringAlert := func(t *testing.T, alerts *fakeQuotaAlertStore) {
		t.Helper()
		_, err := alerts.CreateAlertEvent(context.Background(), &OpsAlertEvent{
			Status: OpsAlertStatusFiring, Severity: quotaAlertSeverity, Dimensions: quotaAlertDimsFor(207), FiredAt: quotaLifecycleBase.Add(-time.Hour),
		})
		require.NoError(t, err)
	}

	t.Run("still exhausted re-parks to resolved placeholder and keeps next confirmation reachable", func(t *testing.T) {
		// 残留取 +2d（≈28 天存量已部分流逝，仍在未来）：远期占位（+7d）晚于残留
		// 停调点，「仍耗尽」续停分支才会实际改写 temp_unschedulable。
		residual := quotaLifecycleBase.Add(2 * 24 * time.Hour)
		account := newResidualTHAccount(207, residual)
		repo := newQuotaLifecycleFakeRepo(account)
		require.NoError(t, repo.SetTempUnschedulable(context.Background(), 207, residual, cnQuotaExhaustedReasonPrefix))
		alerts := &fakeQuotaAlertStore{}
		precreateFiringAlert(t, alerts)
		probe := &quotaProbeController{outcome: quotaProbeExhausted}
		svc := newQuotaLifecycleTestService(repo, alerts, probe)

		// 前置：probeDue=false，探针选中只能来自读侧矛盾判定。
		st, ok := cnQuotaLifecycleStateFromExtra(account.Extra)
		require.True(t, ok)
		require.False(t, st.probeDue(quotaLifecycleBase, quotaLifecycleSweepInterval))

		require.NoError(t, svc.RunRecoverySweep(context.Background()))
		require.Equal(t, 1, probe.calls)

		// a1) temp_unschedulable 按 resolveRecoveryTime 解析值重停：读侧命中场景
		// reset_at 已过期 → 解析落 unknown → 远期占位（base+7d）。
		until, ok := repo.parkedUntil(207)
		require.True(t, ok, "still-exhausted after residual hit must re-park")
		require.Equal(t, quotaLifecycleBase.Add(quotaLifecycleUnknownRecoveryPlaceholder), until)
		// a2) lifecycle 登记 unknown（recovery_at 空），下一次确认时间可达：
		// 节距内不重探，+5min 兜底循环按期推进。
		st, ok = cnQuotaLifecycleStateFromExtra(account.Extra)
		require.True(t, ok)
		require.Equal(t, cnQuotaLifecycleStateExhausted, st.State)
		require.Equal(t, "", st.RecoveryAt, "unknown resolution must register empty recovery_at")
		require.Equal(t, cnQuotaRecoverySourceUnknown, st.RecoverySource)
		require.Equal(t, cnQuotaLifecycleProbeExhausted, st.LastProbeOutcome)
		require.False(t, st.probeDue(quotaLifecycleBase, quotaLifecycleSweepInterval))
		require.True(t, st.probeDue(quotaLifecycleBase.Add(quotaLifecycleSweepInterval), quotaLifecycleSweepInterval),
			"lifecycle must carry a reachable next confirmation time (5-minute cadence)")
		// 告警保持 firing。
		require.Len(t, alerts.firingEvents(quotaAlertDimsFor(207)), 1)
		require.Empty(t, alerts.resolvedEvents(quotaAlertDimsFor(207)))
	})

	t.Run("probe error fail-closed keeps park, alert and confirmation loop", func(t *testing.T) {
		// 经典残留形态（≈28 天存量，仍在未来）。
		residual := quotaLifecycleBase.Add(28 * 24 * time.Hour)
		account := newResidualTHAccount(207, residual)
		repo := newQuotaLifecycleFakeRepo(account)
		require.NoError(t, repo.SetTempUnschedulable(context.Background(), 207, residual, cnQuotaExhaustedReasonPrefix))
		alerts := &fakeQuotaAlertStore{}
		precreateFiringAlert(t, alerts)
		probe := &quotaProbeController{outcome: quotaProbeUncertain, err: errors.New("probe transport down")}
		svc := newQuotaLifecycleTestService(repo, alerts, probe)

		require.NoError(t, svc.RunRecoverySweep(context.Background()))
		require.Equal(t, 1, probe.calls)

		// b1) 失败关闭：保持原停调不变（不清除、不缩短、不延长）。
		require.Zero(t, repo.clearCalls)
		until, ok := repo.parkedUntil(207)
		require.True(t, ok)
		require.Equal(t, residual, until)
		// b2) 告警保持 firing。
		require.Len(t, alerts.firingEvents(quotaAlertDimsFor(207)), 1)
		require.Empty(t, alerts.resolvedEvents(quotaAlertDimsFor(207)))
		// b3) 状态仅登记 uncertain，残留 recovery_at/source 原样保留。
		st, ok := cnQuotaLifecycleStateFromExtra(account.Extra)
		require.True(t, ok)
		require.Equal(t, cnQuotaLifecycleStateExhausted, st.State)
		require.Equal(t, residual.UTC().Format(time.RFC3339), st.RecoveryAt)
		require.Equal(t, cnQuotaRecoverySourceSnapshot, st.RecoverySource)
		require.Equal(t, cnQuotaLifecycleProbeUncertain, st.LastProbeOutcome)
		// b4) 确认循环不丢：矛盾证据（未来 recovery_at + 过期 reset_at）未被失败
		// 关闭抹掉，下一轮 sweep 仍命中读侧判定、再次探针。
		require.NoError(t, svc.RunRecoverySweep(context.Background()))
		require.Equal(t, 2, probe.calls, "fail-closed must not lose the confirmation loop")
		require.Zero(t, repo.clearCalls)
	})
}

// ---------- 读侧第三形态端到端收敛（D-QLM-018）----------

// 端到端（D-QLM-018）：gate 第三形态触发 + 探针返回 exhausted → 收敛经由探针
// 而非写侧：lifecycle recovery_at 被改写为 snap.ResetAt（confirmExhausted 链路
// 按 resolveRecoveryTime 取未来 reset_at，来源 snapshot）、temp_unschedulable
// parkedUntil 同步到 reset_at；converge 写侧对 cur>reset_at 形态零写入（守卫
// 保留，gate 零写入）。探针返回 recovered 时闭环清停调（表驱动正态已覆盖
// clearCalls，这里锁定 exhausted 主形态的收敛落点）。
func TestQuotaLifecycleSweepThirdFormConvergesViaProbe(t *testing.T) {
	resetAt := quotaLifecycleBase.Add(6 * 24 * time.Hour)
	account := newQuotaLifecycleTHAccount(207)
	account.Extra[cnQuotaLifecycleExtraKey] = map[string]any{
		"state":           cnQuotaLifecycleStateExhausted,
		"recovery_at":     quotaLifecycleBase.Add(28 * 24 * time.Hour).UTC().Format(time.RFC3339), // renewsAt 旧口径存量
		"recovery_source": cnQuotaRecoverySourceSnapshot,
		"last_probe_at":   quotaLifecycleBase.Add(-time.Minute).UTC().Format(time.RFC3339),
	}
	account.Extra[TokenHarborPassSnapshotExtraKey] = map[string]any{
		"has_pass":       true,
		"renews_at":      quotaLifecycleBase.Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339),
		"reset_at":       resetAt.UTC().Format(time.RFC3339), // 未来 reset_at（生产实证形态）
		"plan_exhausted": true,
		"fetched_at":     quotaLifecycleBase.Format(time.RFC3339),
	}
	repo := newQuotaLifecycleFakeRepo(account)
	require.NoError(t, repo.SetTempUnschedulable(context.Background(), 207,
		quotaLifecycleBase.Add(28*24*time.Hour), cnQuotaExhaustedReasonPrefix))
	alerts := &fakeQuotaAlertStore{}
	probe := &quotaProbeController{outcome: quotaProbeExhausted}
	svc := newQuotaLifecycleTestService(repo, alerts, probe)

	// 前置：probeDue=false，命中只能来自读侧第三形态。
	st, ok := cnQuotaLifecycleStateFromExtra(account.Extra)
	require.True(t, ok)
	require.False(t, st.probeDue(quotaLifecycleBase, quotaLifecycleSweepInterval))

	require.NoError(t, svc.RunRecoverySweep(context.Background()))
	require.Equal(t, 1, probe.calls)

	// 收敛落点：lifecycle recovery_at 改写为 snap.ResetAt、来源 snapshot。
	st, ok = cnQuotaLifecycleStateFromExtra(account.Extra)
	require.True(t, ok)
	require.Equal(t, resetAt.UTC().Format(time.RFC3339), st.RecoveryAt,
		"confirmation probe must rewrite recovery_at to the official reset_at")
	require.Equal(t, cnQuotaRecoverySourceSnapshot, st.RecoverySource)
	require.Equal(t, cnQuotaLifecycleProbeExhausted, st.LastProbeOutcome)
	// lifecycle 停调口径同步：st.parkedUntil（由 recovery_at 还原）已收敛到官方
	// reset_at——这一改写只有探针路径能完成（converge 单调守卫对 cur>reset_at
	// 零写入），证明收敛经由探针而非写侧。
	lcUntil := st.parkedUntil()
	require.NotNil(t, lcUntil)
	require.Equal(t, resetAt.UTC().Format(time.RFC3339), lcUntil.UTC().Format(time.RFC3339))
	// F4 受控缩短（派发单 C1-a-r3）：仍耗尽且 lifecycle 拥有该 until，账号级 until
	// (+28d) 晚于官方 reset_at(+6d) → 缩短原语把 until 收敛到 reset_at（过度暂停收敛，
	// 替代旧「只延长不缩短」的过度停调安全侧）。
	until, ok := repo.parkedUntil(207)
	require.True(t, ok, "probe-exhausted must keep the account parked")
	require.Equal(t, resetAt, until, "F4 shorten must converge the over-parked until to the official reset_at")
	// 告警保持 firing（仍耗尽）。
	require.Len(t, alerts.firingEvents(quotaAlertDimsFor(207)), 1)
	require.Empty(t, alerts.resolvedEvents(quotaAlertDimsFor(207)))
}

// 失败关闭（D-QLM-018，与 017 同口径）：gate 第三形态触发 + 探针错误 → 停调
// 保持、告警保持 firing、确认循环三保持。
func TestQuotaLifecycleSweepThirdFormProbeErrorFailClosed(t *testing.T) {
	residual := quotaLifecycleBase.Add(28 * 24 * time.Hour)
	account := newQuotaLifecycleTHAccount(207)
	account.Extra[cnQuotaLifecycleExtraKey] = map[string]any{
		"state":           cnQuotaLifecycleStateExhausted,
		"recovery_at":     residual.UTC().Format(time.RFC3339),
		"recovery_source": cnQuotaRecoverySourceSnapshot,
		"last_probe_at":   quotaLifecycleBase.Add(-time.Minute).UTC().Format(time.RFC3339),
	}
	account.Extra[TokenHarborPassSnapshotExtraKey] = map[string]any{
		"has_pass":       true,
		"renews_at":      quotaLifecycleBase.Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339),
		"reset_at":       quotaLifecycleBase.Add(6 * 24 * time.Hour).UTC().Format(time.RFC3339), // 未来 reset_at，cur 晚于它
		"plan_exhausted": true,
		"fetched_at":     quotaLifecycleBase.Format(time.RFC3339),
	}
	repo := newQuotaLifecycleFakeRepo(account)
	require.NoError(t, repo.SetTempUnschedulable(context.Background(), 207, residual, cnQuotaExhaustedReasonPrefix))
	alerts := &fakeQuotaAlertStore{}
	_, err := alerts.CreateAlertEvent(context.Background(), &OpsAlertEvent{
		Status: OpsAlertStatusFiring, Severity: quotaAlertSeverity, Dimensions: quotaAlertDimsFor(207), FiredAt: quotaLifecycleBase.Add(-time.Hour),
	})
	require.NoError(t, err)
	probe := &quotaProbeController{outcome: quotaProbeUncertain, err: errors.New("probe transport down")}
	svc := newQuotaLifecycleTestService(repo, alerts, probe)

	require.NoError(t, svc.RunRecoverySweep(context.Background()))
	require.Equal(t, 1, probe.calls)

	// 保持一：停调原样（不清除、不缩短、不延长）。
	require.Zero(t, repo.clearCalls)
	until, ok := repo.parkedUntil(207)
	require.True(t, ok)
	require.Equal(t, residual, until)
	// 保持二：告警保持 firing。
	require.Len(t, alerts.firingEvents(quotaAlertDimsFor(207)), 1)
	require.Empty(t, alerts.resolvedEvents(quotaAlertDimsFor(207)))
	// 保持三：确认循环不丢——第三形态矛盾证据未被失败关闭抹掉，下一轮 sweep
	// 再次探针。
	require.NoError(t, svc.RunRecoverySweep(context.Background()))
	require.Equal(t, 2, probe.calls, "fail-closed must not lose the confirmation loop")
	require.Zero(t, repo.clearCalls)
}

// ---------- 第三形态恢复闭环连续时间推进（D-QLM-019）----------

// setOutcome 运行中切换探针返回值（连续时间线需要两次 sweep 不同结果）。
func (c *quotaProbeController) setOutcome(o quotaProbeOutcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.outcome = o
}

// 端到端闭环（D-QLM-019）：第三形态第一次探针把 recovery_at 收敛到 reset_at 后，
// 连续推进时钟到 reset_at 到期，验证「probeDue → probe → recovered → 清停调 +
// resolve 告警 + recovered 墓碑」全链闭环（sweepCandidates 持久候选发现 +
// sweepProbeAccount recovered 分支）。
func TestQuotaLifecycleSweepThirdFormRecoveryClosure(t *testing.T) {
	resetAt := quotaLifecycleBase.Add(6 * 24 * time.Hour)
	residual := quotaLifecycleBase.Add(28 * 24 * time.Hour)
	account := newQuotaLifecycleTHAccount(207)
	account.Extra[cnQuotaLifecycleExtraKey] = map[string]any{
		"state":           cnQuotaLifecycleStateExhausted,
		"recovery_at":     residual.UTC().Format(time.RFC3339), // renewsAt 旧口径存量
		"recovery_source": cnQuotaRecoverySourceSnapshot,
		"last_probe_at":   quotaLifecycleBase.Add(-time.Minute).UTC().Format(time.RFC3339),
	}
	account.Extra[TokenHarborPassSnapshotExtraKey] = map[string]any{
		"has_pass":       true,
		"renews_at":      quotaLifecycleBase.Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339),
		"reset_at":       resetAt.UTC().Format(time.RFC3339), // 未来 reset_at（第三形态矛盾证据）
		"plan_exhausted": true,
		"fetched_at":     quotaLifecycleBase.Format(time.RFC3339),
	}
	repo := newQuotaLifecycleFakeRepo(account)
	require.NoError(t, repo.SetTempUnschedulable(context.Background(), 207, residual, cnQuotaExhaustedReasonPrefix))
	alerts := &fakeQuotaAlertStore{}
	_, err := alerts.CreateAlertEvent(context.Background(), &OpsAlertEvent{
		Status: OpsAlertStatusFiring, Severity: quotaAlertSeverity, Dimensions: quotaAlertDimsFor(207), FiredAt: quotaLifecycleBase.Add(-time.Hour),
	})
	require.NoError(t, err)
	probe := &quotaProbeController{outcome: quotaProbeExhausted}
	svc := newQuotaLifecycleTestService(repo, alerts, probe)
	clock := quotaLifecycleBase
	svc.SetQuotaLifecycleClock(func() time.Time { return clock })

	// --- t0 第一次 sweep：gate 第三形态触发，recovery_at 收敛到 reset_at ---
	// 前置：probeDue=false，选中只能来自读侧第三形态。
	st, ok := cnQuotaLifecycleStateFromExtra(account.Extra)
	require.True(t, ok)
	require.False(t, st.probeDue(quotaLifecycleBase, quotaLifecycleSweepInterval))

	require.NoError(t, svc.RunRecoverySweep(context.Background()))
	require.Equal(t, 1, probe.calls, "third-form contradiction must reach the probe at t0")
	st, ok = cnQuotaLifecycleStateFromExtra(account.Extra)
	require.True(t, ok)
	require.Equal(t, resetAt.UTC().Format(time.RFC3339), st.RecoveryAt,
		"first probe must converge recovery_at to the official reset_at")
	require.Equal(t, cnQuotaRecoverySourceSnapshot, st.RecoverySource)
	// F4 受控缩短（派发单 C1-a-r3）：t0 探针仍耗尽且 lifecycle 拥有该 until，账号级
	// until(+28d) 晚于官方 reset_at(+6d) → 缩短原语把 until 收敛到 reset_at（而非
	// 只延长不缩短）。
	until, ok := repo.parkedUntil(207)
	require.True(t, ok)
	require.Equal(t, resetAt, until, "F4 shorten must converge the over-parked until to the official reset_at")
	require.Len(t, alerts.firingEvents(quotaAlertDimsFor(207)), 1)

	// --- F4 受控缩短后的闭环（派发单 C1-a-r3）---
	// t0 把过度停调 until(+28d) 收敛到官方 reset_at(+6d) 后，账号在 reset_at 自然
	// 到期——矛盾由缩短消解，不再产生探针、不再过度续停（旧「recovered 探针清停调」
	// 闭环节被缩短替代；recovered 清除闭环由 TestQuotaLifecycleSweepThirdForm
	// PlanExhaustedFalseRecoveryClosure 覆盖）。
	clock = resetAt.Add(time.Minute) // reset_at 到期后
	// 矛盾门已消解：账号级 until(=reset_at) 不再晚于 reset_at。
	require.False(t, svc.sweepTHResidualRecoveryDue(account, st, clock),
		"F4 shorten at t0 already resolved the time contradiction; the gate must no longer trip")
	require.NoError(t, svc.RunRecoverySweep(context.Background()))
	// 缩短后的 until 已到期 → 账号退出停调候选，不再产生探针（无过度续停、无 spurious probe）。
	require.Equal(t, 1, probe.calls, "F4 shorten resolved the over-park; no further probe after reset_at")
	require.NotNil(t, account.TempUnschedulableUntil, "park record still present")
	require.False(t, account.TempUnschedulableUntil.After(clock),
		"shortened until must have naturally expired at reset_at (F4 resolves the over-park)")
	// 告警仍 firing（上游仍耗尽，仅停调窗口到期；后续探针确认恢复由既有 recovered 路径负责）。
	require.Len(t, alerts.firingEvents(quotaAlertDimsFor(207)), 1)
}

// 端到端闭环（D-QLM-021）：第三形态去掉 pe 前提后的生产实证第二轮形态——快照
// plan_exhausted=false（官方 free-tier 已恢复）+ 停调挂旧口径未来 recovery_at
// + reset_at 在未来（cur > reset_at）→ gate 以时间矛盾独立触发探针；探针返回
// recovered → 断言清停调 + 告警 resolve + state=recovered 墓碑（镜像 019 主用例
// 断言集）。
func TestQuotaLifecycleSweepThirdFormPlanExhaustedFalseRecoveryClosure(t *testing.T) {
	resetAt := quotaLifecycleBase.Add(12 * 24 * time.Hour)
	residual := quotaLifecycleBase.Add(28 * 24 * time.Hour)
	account := newQuotaLifecycleTHAccount(207)
	account.Extra[cnQuotaLifecycleExtraKey] = map[string]any{
		"state":           cnQuotaLifecycleStateExhausted,
		"recovery_at":     residual.UTC().Format(time.RFC3339), // renewsAt 旧口径存量，晚于 reset_at
		"recovery_source": cnQuotaRecoverySourceSnapshot,
		"last_probe_at":   quotaLifecycleBase.Add(-time.Minute).UTC().Format(time.RFC3339),
	}
	account.Extra[TokenHarborPassSnapshotExtraKey] = map[string]any{
		"has_pass":       true,
		"renews_at":      quotaLifecycleBase.Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339),
		"reset_at":       resetAt.UTC().Format(time.RFC3339), // 未来 reset_at，cur 晚于它
		"plan_exhausted": false,                              // 官方已恢复（生产实证第二轮 4 账号形态）
		"fetched_at":     quotaLifecycleBase.Format(time.RFC3339),
	}
	repo := newQuotaLifecycleFakeRepo(account)
	require.NoError(t, repo.SetTempUnschedulable(context.Background(), 207, residual, cnQuotaExhaustedReasonPrefix))
	alerts := &fakeQuotaAlertStore{}
	_, err := alerts.CreateAlertEvent(context.Background(), &OpsAlertEvent{
		Status: OpsAlertStatusFiring, Severity: quotaAlertSeverity, Dimensions: quotaAlertDimsFor(207), FiredAt: quotaLifecycleBase.Add(-time.Hour),
	})
	require.NoError(t, err)
	probe := &quotaProbeController{outcome: quotaProbeRecovered}
	svc := newQuotaLifecycleTestService(repo, alerts, probe)

	// 前置：probeDue=false（残留 recovery_at 在未来），选中只能来自读侧第三形态
	//（pe=false 下的时间矛盾）。
	st, ok := cnQuotaLifecycleStateFromExtra(account.Extra)
	require.True(t, ok)
	require.False(t, st.probeDue(quotaLifecycleBase, quotaLifecycleSweepInterval))
	require.True(t, svc.sweepTHResidualRecoveryDue(account, st, quotaLifecycleBase),
		"pe=false with cur later than reset_at must trigger the third form via time contradiction")

	require.NoError(t, svc.RunRecoverySweep(context.Background()))
	require.Equal(t, 1, probe.calls, "time contradiction with pe=false must reach the confirmation probe")
	// 闭环一：清停调。
	require.Equal(t, 1, repo.clearCalls, "recovered probe must clear the temp-unschedulable park")
	_, parked := repo.parkedUntil(207)
	require.False(t, parked, "closure must remove the park record")
	// 闭环二：告警 resolve。
	require.Empty(t, alerts.firingEvents(quotaAlertDimsFor(207)))
	require.Len(t, alerts.resolvedEvents(quotaAlertDimsFor(207)), 1)
	// 闭环三：recovered 墓碑。
	st, ok = cnQuotaLifecycleStateFromExtra(account.Extra)
	require.True(t, ok)
	require.Equal(t, cnQuotaLifecycleStateRecovered, st.State)
	require.Equal(t, cnQuotaLifecycleProbeRecovered, st.LastProbeOutcome)
}

// 分支覆盖（D-QLM-019）：同型初态，reset_at 到期时探针仍返回 exhausted → 快照
// reset_at 已过期，resolveRecoveryTime 落 unknown → 远期占位续停 + source=unknown
// + recovery_at 登记空；告警保持 firing，5 分钟兜底循环不丢（probe.calls 继续递增）。
func TestQuotaLifecycleSweepThirdFormStillExhaustedReParksAndKeepsLoop(t *testing.T) {
	resetAt := quotaLifecycleBase.Add(6 * 24 * time.Hour)
	residual := quotaLifecycleBase.Add(28 * 24 * time.Hour)
	account := newQuotaLifecycleTHAccount(207)
	account.Extra[cnQuotaLifecycleExtraKey] = map[string]any{
		"state":           cnQuotaLifecycleStateExhausted,
		"recovery_at":     residual.UTC().Format(time.RFC3339),
		"recovery_source": cnQuotaRecoverySourceSnapshot,
		"last_probe_at":   quotaLifecycleBase.Add(-time.Minute).UTC().Format(time.RFC3339),
	}
	account.Extra[TokenHarborPassSnapshotExtraKey] = map[string]any{
		"has_pass":       true,
		"renews_at":      quotaLifecycleBase.Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339),
		"reset_at":       resetAt.UTC().Format(time.RFC3339),
		"plan_exhausted": true,
		"fetched_at":     quotaLifecycleBase.Format(time.RFC3339),
	}
	repo := newQuotaLifecycleFakeRepo(account)
	require.NoError(t, repo.SetTempUnschedulable(context.Background(), 207, residual, cnQuotaExhaustedReasonPrefix))
	alerts := &fakeQuotaAlertStore{}
	_, err := alerts.CreateAlertEvent(context.Background(), &OpsAlertEvent{
		Status: OpsAlertStatusFiring, Severity: quotaAlertSeverity, Dimensions: quotaAlertDimsFor(207), FiredAt: quotaLifecycleBase.Add(-time.Hour),
	})
	require.NoError(t, err)
	probe := &quotaProbeController{outcome: quotaProbeExhausted}
	svc := newQuotaLifecycleTestService(repo, alerts, probe)
	clock := quotaLifecycleBase
	svc.SetQuotaLifecycleClock(func() time.Time { return clock })

	// t0 第一次 sweep：矛盾第三形态 + 仍耗尽 → F4 受控缩短（派发单 C1-a-r3）把过度
	// 停调 until(+28d) 收敛到官方 reset_at(+6d)；recovery_at 同步收敛到 reset_at。
	require.NoError(t, svc.RunRecoverySweep(context.Background()))
	require.Equal(t, 1, probe.calls)
	st, ok := cnQuotaLifecycleStateFromExtra(account.Extra)
	require.True(t, ok)
	require.Equal(t, resetAt.UTC().Format(time.RFC3339), st.RecoveryAt)
	until, ok := repo.parkedUntil(207)
	require.True(t, ok, "F4 shorten must leave the park record")
	require.Equal(t, resetAt, until, "F4 shorten must converge the over-parked until to the official reset_at")

	// 缩短后矛盾消解：账号级 until(=reset_at) 不再晚于 reset_at，矛盾门关闭。
	require.False(t, svc.sweepTHResidualRecoveryDue(account, st, quotaLifecycleBase),
		"F4 shorten collapsed until to reset_at; the time contradiction must be resolved")

	// F4 缩短的闭环（派发单 C1-a-r3）：缩短后的 until 在 reset_at 自然到期，账号退出
	// 停调候选，不再产生探针（无过度续停、无 spurious probe）；矛盾门亦不再成立；
	// 告警保持 firing（上游仍耗尽，仅停调窗口到期）。
	clock = resetAt.Add(time.Minute) // reset_at 到期后
	require.NoError(t, svc.RunRecoverySweep(context.Background()))
	require.Equal(t, 1, probe.calls, "expired shortened until must drop out of parked candidates (no spurious probe)")
	require.False(t, svc.sweepTHResidualRecoveryDue(account, st, clock),
		"F4 shortened until no longer exceeds reset_at; contradiction gate must be false")
	require.NotNil(t, account.TempUnschedulableUntil, "park record still present")
	require.False(t, account.TempUnschedulableUntil.After(clock),
		"shortened until must have naturally expired at reset_at (F4 resolves the over-park)")
	// 告警保持 firing（上游仍耗尽、未恢复；仅停调窗口自然到期）。
	require.Len(t, alerts.firingEvents(quotaAlertDimsFor(207)), 1)
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

	t.Run("free pool headroom + fresh VND>0 means recovered", func(t *testing.T) {
		// 余额三态契约：account.Extra 写入新鲜 VND 付费余额 >0（付费池未耗尽）。
		// 免费池有余量 + 付费池新鲜未耗尽 → 确属恢复（Recovered）。
		account := newQuotaLifecycleKiraAccount(208)
		account.Extra[cnExtraKey(account.Platform, cnBalanceExtraSuffixBalance)] = 5000.0
		account.Extra[cnExtraKey(account.Platform, cnBalanceExtraSuffixUpdated)] = quotaLifecycleBase.UTC().Format(time.RFC3339)
		upstream := &quotaCaptureUpstream{statusCode: http.StatusOK,
			body: `{"summary":{"tokensUsedToday":1000,"freeDailyLimit":6000000,"vndBalance":0}}`}
		svc := NewCNQuotaLifecycleService(newQuotaLifecycleFakeRepo(account), upstream, cfg, nil)
		svc.SetQuotaLifecycleClock(func() time.Time { return quotaLifecycleBase })
		outcome, err := svc.probeKiraExhaustion(context.Background(), account)
		require.NoError(t, err)
		require.Equal(t, quotaProbeRecovered, outcome)
	})

	t.Run("free pool headroom but VND unknown means uncertain (no fresh evidence)", func(t *testing.T) {
		// F1/D1：免费池有余量但 VND 余额无新鲜证据（Extra 缺键/过期）→ 不得结论
		// Recovered（fail-closed）。避免 VND=0 被误判为已恢复而永不停调。
		account := newQuotaLifecycleKiraAccount(208)
		// 不写入 VND 余额键 → ResolveKiraVNDBalanceState 为 unknown。
		upstream := &quotaCaptureUpstream{statusCode: http.StatusOK,
			body: `{"summary":{"tokensUsedToday":1000,"freeDailyLimit":6000000,"vndBalance":0}}`}
		svc := NewCNQuotaLifecycleService(newQuotaLifecycleFakeRepo(account), upstream, cfg, nil)
		svc.SetQuotaLifecycleClock(func() time.Time { return quotaLifecycleBase })
		outcome, err := svc.probeKiraExhaustion(context.Background(), account)
		require.Error(t, err)
		require.Equal(t, quotaProbeUncertain, outcome,
			"free pool headroom with unknown VND must not conclude Recovered")
	})

	t.Run("free pool headroom but fresh VND=0 means exhausted (D1 fix)", func(t *testing.T) {
		// D1：免费池有余量但付费 VND 余额新鲜=0 → 付费池耗尽，应 Exhausted，
		// 不得 Recovered（旧逻辑免费池余量即 Recovered 导致 VND=0 永不 park）。
		account := newQuotaLifecycleKiraAccount(208)
		account.Extra[cnExtraKey(account.Platform, cnBalanceExtraSuffixBalance)] = 0.0
		account.Extra[cnExtraKey(account.Platform, cnBalanceExtraSuffixUpdated)] = quotaLifecycleBase.UTC().Format(time.RFC3339)
		upstream := &quotaCaptureUpstream{statusCode: http.StatusOK,
			body: `{"summary":{"tokensUsedToday":1000,"freeDailyLimit":6000000,"vndBalance":0}}`}
		svc := NewCNQuotaLifecycleService(newQuotaLifecycleFakeRepo(account), upstream, cfg, nil)
		svc.SetQuotaLifecycleClock(func() time.Time { return quotaLifecycleBase })
		outcome, err := svc.probeKiraExhaustion(context.Background(), account)
		require.NoError(t, err)
		require.Equal(t, quotaProbeExhausted, outcome,
			"fresh VND=0 with free-pool headroom must exhaust (D1 fix)")
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

// ---------- F2 结构化作用域矩阵 + 402 账号级权威冻结 ----------

func TestClassifyQuotaExhaustionScopeMatrix(t *testing.T) {
	kira := newQuotaLifecycleKiraAccount(208)
	nonKira := newQuotaLifecycleTHAccount(207) // TH 非 Kira

	t.Run("kira 402 = account (authoritative freeze)", func(t *testing.T) {
		require.Equal(t, QuotaScopeAccount, ClassifyQuotaExhaustionScope(kira, 402, quotaLifecycleBase))
	})
	t.Run("non-Kira 402 = model (probe-confirm)", func(t *testing.T) {
		require.Equal(t, QuotaScopeModel, ClassifyQuotaExhaustionScope(nonKira, 402, quotaLifecycleBase))
	})
	t.Run("429 = model (no account-wide freeze)", func(t *testing.T) {
		require.Equal(t, QuotaScopeModel, ClassifyQuotaExhaustionScope(kira, 429, quotaLifecycleBase))
		require.Equal(t, QuotaScopeModel, ClassifyQuotaExhaustionScope(nonKira, 429, quotaLifecycleBase))
	})
	t.Run("kira 502/504 only account when fresh VND<=0, else unknown", func(t *testing.T) {
		recent := quotaLifecycleBase.Add(-time.Minute)
		kira.Extra[cnExtraKey(kira.Platform, cnBalanceExtraSuffixBalance)] = 0.0
		kira.Extra[cnExtraKey(kira.Platform, cnBalanceExtraSuffixUpdated)] = recent.UTC().Format(time.RFC3339)
		require.Equal(t, QuotaScopeAccount, ClassifyQuotaExhaustionScope(kira, 502, quotaLifecycleBase),
			"kira 502/504 with fresh VND=0 warrants account freeze")
		// 清除 VND 键 → unknown（不得把通用 5xx 泛化为账号冻结）。
		delete(kira.Extra, cnExtraKey(kira.Platform, cnBalanceExtraSuffixBalance))
		delete(kira.Extra, cnExtraKey(kira.Platform, cnBalanceExtraSuffixUpdated))
		require.Equal(t, QuotaScopeUnknown, ClassifyQuotaExhaustionScope(kira, 504, quotaLifecycleBase))
	})
	t.Run("non-Kira 502/504 = unknown", func(t *testing.T) {
		require.Equal(t, QuotaScopeUnknown, ClassifyQuotaExhaustionScope(nonKira, 502, quotaLifecycleBase))
	})
}

func TestQuotaLifecycleKira402AuthoritativeFreeze(t *testing.T) {
	// F2：Kira 账号 402 → 账号级权威冻结（三态裁决表三行全冻结），reason 标记
	// [402-account-authoritative]，并持久化 probe_due_at（§3.3 R19-F4）。
	account := newQuotaLifecycleKiraAccount(208)
	repo := newQuotaLifecycleFakeRepo(account)
	alerts := &fakeQuotaAlertStore{}
	probe := &quotaProbeController{outcome: quotaProbeExhausted}
	svc := newQuotaLifecycleTestService(repo, alerts, probe)

	require.NoError(t, svc.OnUpstreamQuotaExhausted(context.Background(), account,
		"Insufficient VND wallet balance (0 VND remaining)"))

	until, parked := repo.parkedUntil(208)
	require.True(t, parked, "kira 402 must freeze the account")
	require.Contains(t, repo.parkedReason(208), cnQuotaExhaustedReason402Account,
		"402 authoritative freeze must be tagged")
	st, ok := cnQuotaLifecycleStateFromExtra(account.Extra)
	require.True(t, ok)
	require.Equal(t, cnQuotaLifecycleStateExhausted, st.State)
	require.NotEmpty(t, st.ProbeDueAt, "402 authoritative freeze must persist probe_due_at")
	require.Len(t, alerts.firingEvents(quotaAlertDimsFor(208)), 1)
	_ = until
}

func TestQuotaLifecycleNonKira402ModelScopeNoFreezeOnUncertain(t *testing.T) {
	// F2 控制组：非 Kira 平台 402 经结构化信号判定为 model 作用域（探针确认语义），
	// 探针不确定 → 失败关闭，不整号冻结。
	account := newQuotaLifecycleTHAccount(207)
	repo := newQuotaLifecycleFakeRepo(account)
	alerts := &fakeQuotaAlertStore{}
	probe := &quotaProbeController{outcome: quotaProbeUncertain, err: errors.New("transport down")}
	svc := newQuotaLifecycleTestService(repo, alerts, probe)

	require.NoError(t, svc.OnUpstreamQuotaExhausted(context.Background(), account,
		"Your Pass allowance for this period is used"))

	_, parked := repo.parkedUntil(207)
	require.False(t, parked, "non-Kira 402 with uncertain probe must not freeze the account")
	st, ok := cnQuotaLifecycleStateFromExtra(account.Extra)
	require.True(t, ok)
	require.Equal(t, cnQuotaLifecycleStateUncertain, st.State)
}

func TestQuotaLifecycleUnknownScopeNoConversion(t *testing.T) {
	// F2：scope=unknown 不做整号转换（不冻结、不改账号级状态）。
	account := newQuotaLifecycleKiraAccount(208)
	repo := newQuotaLifecycleFakeRepo(account)
	alerts := &fakeQuotaAlertStore{}
	probe := &quotaProbeController{outcome: quotaProbeExhausted}
	svc := newQuotaLifecycleTestService(repo, alerts, probe)

	signal := QuotaExhaustionSignal{Status: 502, Scope: QuotaScopeUnknown, Platform: account.Platform, Msg: "upstream 502"}
	require.NoError(t, svc.OnUpstreamQuotaExhaustedScoped(context.Background(), account, "upstream 502", signal))

	_, parked := repo.parkedUntil(208)
	require.False(t, parked, "unknown scope must not cause whole-account conversion")
	require.Zero(t, probe.calls, "unknown scope must not trigger the confirmation probe")
}

func TestQuotaLifecycleSweepStillExhaustedDoesNotTouchNonLifecycleUntil(t *testing.T) {
	// F4 受控缩短：账号级 until 由他因（非 lifecycle 域）拥有时，仍耗尽分支不得
	// 改写/缩短——保持只延长不缩短安全侧，他因暂停不受 lifecycle 影响。
	account := newQuotaLifecycleKiraAccount(208)
	externalUntil := quotaLifecycleBase.Add(3 * 24 * time.Hour)
	account.TempUnschedulableUntil = &externalUntil
	account.TempUnschedulableReason = "muse:usage-window-exhausted" // 他因 reason
	account.Extra[cnQuotaLifecycleExtraKey] = map[string]any{
		"state":           cnQuotaLifecycleStateExhausted,
		"recovery_at":     quotaLifecycleBase.Add(-time.Hour).UTC().Format(time.RFC3339),
		"recovery_source": cnQuotaRecoverySourceSnapshot,
	}
	repo := newQuotaLifecycleFakeRepo(account)
	require.NoError(t, repo.SetTempUnschedulable(context.Background(), 208, externalUntil, "muse:usage-window-exhausted"))
	alerts := &fakeQuotaAlertStore{}
	probe := &quotaProbeController{outcome: quotaProbeExhausted}
	svc := newQuotaLifecycleTestService(repo, alerts, probe)
	svc.track(208) // 来自不确定循环跟踪

	require.NoError(t, svc.RunRecoverySweep(context.Background()))

	// lifecycle 不得触碰他因 until：仍耗尽分支跳过 re-park/缩短。
	until, ok := repo.parkedUntil(208)
	require.True(t, ok)
	require.Equal(t, externalUntil, until, "lifecycle must not touch a non-lifecycle-owned until")
}

// ---------- C1-a-r3：F4 受控缩短路径 ----------

// TestQuotaLifecycleF4ShortenEndToEnd F4 端到端（派发单 C1-a-r3 / 方案 §4 F4）：
// 存量形态 until=renews_at（旧口径，+28d，晚于官方 reset_at）+ 快照 reset_at(+6d)
// → 读侧矛盾判定 → 探针 still-exhausted → until 被缩短至 reset_at（断言落库值）。
func TestQuotaLifecycleF4ShortenEndToEnd(t *testing.T) {
	resetAt := quotaLifecycleBase.Add(6 * 24 * time.Hour)
	residual := quotaLifecycleBase.Add(28 * 24 * time.Hour)
	account := newQuotaLifecycleTHAccount(207)
	account.Extra[cnQuotaLifecycleExtraKey] = map[string]any{
		"state":           cnQuotaLifecycleStateExhausted,
		"recovery_at":     residual.UTC().Format(time.RFC3339), // renewsAt 旧口径存量，晚于 reset_at
		"recovery_source": cnQuotaRecoverySourceSnapshot,
		"last_probe_at":   quotaLifecycleBase.Add(-time.Minute).UTC().Format(time.RFC3339),
	}
	account.Extra[TokenHarborPassSnapshotExtraKey] = map[string]any{
		"has_pass":       true,
		"renews_at":      quotaLifecycleBase.Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339),
		"reset_at":       resetAt.UTC().Format(time.RFC3339), // 官方 reset_at（矛盾落点）
		"plan_exhausted": true,
		"fetched_at":     quotaLifecycleBase.Format(time.RFC3339),
	}
	repo := newQuotaLifecycleFakeRepo(account)
	require.NoError(t, repo.SetTempUnschedulable(context.Background(), 207, residual, cnQuotaExhaustedReasonPrefix))
	alerts := &fakeQuotaAlertStore{}
	probe := &quotaProbeController{outcome: quotaProbeExhausted}
	svc := newQuotaLifecycleTestService(repo, alerts, probe)

	// 前置：读侧矛盾成立（账号级 until 晚于 reset_at），且由 lifecycle 拥有。
	st, ok := cnQuotaLifecycleStateFromExtra(account.Extra)
	require.True(t, ok)
	require.False(t, st.probeDue(quotaLifecycleBase, quotaLifecycleSweepInterval))
	require.True(t, svc.sweepTHResidualRecoveryDue(account, st, quotaLifecycleBase),
		"account-level until later than reset_at must trip the contradiction gate")

	require.NoError(t, svc.RunRecoverySweep(context.Background()))
	require.Equal(t, 1, probe.calls, "contradiction must reach the confirmation probe")

	// F4 缩短：until 收敛到 reset_at（落库值）。
	until, ok := repo.parkedUntil(207)
	require.True(t, ok)
	require.Equal(t, resetAt, until, "F4 shorten must collapse the over-parked until to the official reset_at")
	// 约束 ④：reason 不变。
	require.Equal(t, cnQuotaExhaustedReasonPrefix, repo.parkedReason(207))
	// 缩短后矛盾消解：账号级 until 不再晚于 reset_at。
	require.False(t, svc.sweepTHResidualRecoveryDue(account, st, quotaLifecycleBase),
		"after F4 shorten the contradiction must be resolved")
	require.Len(t, alerts.firingEvents(quotaAlertDimsFor(207)), 1)
}

// TestQuotaLifecycleF4ShortenNegativeNonOwned F4 负例（派发单 C1-a-r3）：同样矛盾
//（账号级 until 晚于 reset_at）但 reason 非 lifecycle 拥有 → until 不变。
func TestQuotaLifecycleF4ShortenNegativeNonOwned(t *testing.T) {
	resetAt := quotaLifecycleBase.Add(6 * 24 * time.Hour)
	residual := quotaLifecycleBase.Add(28 * 24 * time.Hour)
	account := newQuotaLifecycleTHAccount(207)
	externalReason := "muse:usage-window-exhausted" // 他因 reason
	account.TempUnschedulableUntil = &residual
	account.TempUnschedulableReason = externalReason
	account.Extra[cnQuotaLifecycleExtraKey] = map[string]any{
		"state":           cnQuotaLifecycleStateExhausted,
		"recovery_at":     residual.UTC().Format(time.RFC3339),
		"recovery_source": cnQuotaRecoverySourceSnapshot,
		"last_probe_at":   quotaLifecycleBase.Add(-time.Minute).UTC().Format(time.RFC3339),
	}
	account.Extra[TokenHarborPassSnapshotExtraKey] = map[string]any{
		"has_pass":       true,
		"renews_at":      quotaLifecycleBase.Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339),
		"reset_at":       resetAt.UTC().Format(time.RFC3339),
		"plan_exhausted": true,
		"fetched_at":     quotaLifecycleBase.Format(time.RFC3339),
	}
	repo := newQuotaLifecycleFakeRepo(account)
	require.NoError(t, repo.SetTempUnschedulable(context.Background(), 207, residual, externalReason))
	alerts := &fakeQuotaAlertStore{}
	probe := &quotaProbeController{outcome: quotaProbeExhausted}
	svc := newQuotaLifecycleTestService(repo, alerts, probe)

	// 读侧矛盾门（sweepTHResidualRecoveryDue）因 reason 非 lifecycle 拥有而关闭：
	// lifecycle 不接管他因暂停。
	st, ok := cnQuotaLifecycleStateFromExtra(account.Extra)
	require.True(t, ok)
	require.False(t, svc.sweepTHResidualRecoveryDue(account, st, quotaLifecycleBase),
		"non-lifecycle-owned contradiction must not trip the lifecycle contradiction gate")

	// 即便经其它候选路径进入 sweep（此处借 probeDue：recovery_at 已过期），仍耗尽分支
	// 也不得改写/缩短他因 until。
	account.TempUnschedulableReason = externalReason
	svc.track(207)
	require.NoError(t, svc.RunRecoverySweep(context.Background()))
	until, ok := repo.parkedUntil(207)
	require.True(t, ok)
	require.Equal(t, residual, until, "F4 negative: non-lifecycle-owned until must stay unchanged")
	require.Equal(t, externalReason, repo.parkedReason(207), "F4 negative: reason must stay unchanged")
}

// TestQuotaLifecycleShortenTempUnschedulableIfOwnedContract 原语四条硬约束（派发单
// C1-a-r3）：缩短生效 / 非拥有不动 / newUntil>=现状 no-op / reason 与其他字段不变。
// 经 fake 接口替身真实执行（生产 SQL 等价约束见 repo 层 SQL 形态测试）。
func TestQuotaLifecycleShortenTempUnschedulableIfOwnedContract(t *testing.T) {
	t.Run("shorten applies when owned and until>newUntil", func(t *testing.T) {
		account := newQuotaLifecycleTHAccount(207)
		repo := newQuotaLifecycleFakeRepo(account)
		old := quotaLifecycleBase.Add(28 * 24 * time.Hour)
		newU := quotaLifecycleBase.Add(6 * 24 * time.Hour)
		require.NoError(t, repo.SetTempUnschedulable(context.Background(), 207, old, cnQuotaExhaustedReasonPrefix))

		applied, err := repo.ShortenTempUnschedulableIfOwned(context.Background(), 207, newU, cnQuotaExhaustedReasonPrefix)
		require.NoError(t, err)
		require.True(t, applied, "owned over-parked until must be shortened")
		until, ok := repo.parkedUntil(207)
		require.True(t, ok)
		require.Equal(t, newU, until)
	})

	t.Run("non-owned reason is untouched", func(t *testing.T) {
		account := newQuotaLifecycleTHAccount(207)
		repo := newQuotaLifecycleFakeRepo(account)
		old := quotaLifecycleBase.Add(28 * 24 * time.Hour)
		newU := quotaLifecycleBase.Add(6 * 24 * time.Hour)
		require.NoError(t, repo.SetTempUnschedulable(context.Background(), 207, old, "muse:usage-window-exhausted"))

		applied, err := repo.ShortenTempUnschedulableIfOwned(context.Background(), 207, newU, cnQuotaExhaustedReasonPrefix)
		require.NoError(t, err)
		require.False(t, applied, "non-owned reason must not be shortened")
		until, ok := repo.parkedUntil(207)
		require.True(t, ok)
		require.Equal(t, old, until)
	})

	t.Run("newUntil>=current is a no-op and never extends", func(t *testing.T) {
		account := newQuotaLifecycleTHAccount(207)
		repo := newQuotaLifecycleFakeRepo(account)
		old := quotaLifecycleBase.Add(6 * 24 * time.Hour)
		// newUntil 等于现状：no-op（不得延长）；newUntil 晚于现状同样 no-op。
		for _, newU := range []time.Time{old, old.Add(24 * time.Hour)} {
			require.NoError(t, repo.SetTempUnschedulable(context.Background(), 207, old, cnQuotaExhaustedReasonPrefix))
			applied, err := repo.ShortenTempUnschedulableIfOwned(context.Background(), 207, newU, cnQuotaExhaustedReasonPrefix)
			require.NoError(t, err)
			require.False(t, applied, "newUntil>=current must be a no-op (never extend)")
			until, ok := repo.parkedUntil(207)
			require.True(t, ok)
			require.Equal(t, old, until, "no-op must leave the until unchanged")
		}
	})

	t.Run("reason and other fields are preserved", func(t *testing.T) {
		account := newQuotaLifecycleTHAccount(207)
		repo := newQuotaLifecycleFakeRepo(account)
		old := quotaLifecycleBase.Add(28 * 24 * time.Hour)
		newU := quotaLifecycleBase.Add(6 * 24 * time.Hour)
		reason := cnQuotaExhaustedReasonPrefix + "：停调至官方恢复时间 2099-01-01T00:00:00Z（来源 snapshot）"
		require.NoError(t, repo.SetTempUnschedulable(context.Background(), 207, old, reason))

		applied, err := repo.ShortenTempUnschedulableIfOwned(context.Background(), 207, newU, cnQuotaExhaustedReasonPrefix)
		require.NoError(t, err)
		require.True(t, applied)
		require.Equal(t, reason, repo.parkedReason(207), "shorten must not alter the reason")
	})
}

// ---------- C1-a-r2 整改：补齐三组缺口测试（测试-only，仅新增函数） ----------

// 组 1：probe_due_at 迁移表四态（方案 §3.3 R19-F4）。
// 覆盖 resolveProbeDueAt + probeDue 的语义边界：
//   - 余额 fresh>0 → 立即 due（now）；
//   - 余额 unknown / fresh≤0（Kira）→ kiraNextDailyReset（每日重置兜底）；
//   - TH 账号冻结且有未来 reset_at → 返回该 reset_at；
//   - probeDue：probe_due_at 在未来→false、已过→true；为空回退 RecoveryAt（回归保护）。
func TestProbeDueMigrationTableFourStates(t *testing.T) {
	now := quotaLifecycleBase
	svc := newQuotaLifecycleTestService(newQuotaLifecycleFakeRepo(), &fakeQuotaAlertStore{}, &quotaProbeController{})
	signal := QuotaExhaustionSignal{Status: 402, Scope: QuotaScopeAccount, Platform: PlatformOther}

	// 态 A：冻结时余额 fresh>0 → 立即 due（返回 now）。
	t.Run("fresh_positive_returns_now", func(t *testing.T) {
		kira := newQuotaLifecycleKiraAccount(208)
		recent := now.Add(-30 * time.Second) // 基点前 1 分钟内，新鲜
		kira.Extra[cnExtraKey(kira.Platform, cnBalanceExtraSuffixBalance)] = 1000.0
		kira.Extra[cnExtraKey(kira.Platform, cnBalanceExtraSuffixUpdated)] = recent.UTC().Format(time.RFC3339)
		got := svc.resolveProbeDueAt(kira, signal, now)
		require.Equal(t, now, got, "fresh VND>0 must return now (immediate due)")
	})

	// 态 B：冻结时余额 unknown（缺键）→ Kira 每日重置兜底。
	t.Run("unknown_balance_returns_kira_daily_reset", func(t *testing.T) {
		kira := newQuotaLifecycleKiraAccount(208)
		got := svc.resolveProbeDueAt(kira, signal, now)
		require.Equal(t, kiraNextDailyReset(now), got, "unknown Kira balance must fall back to daily reset")
	})

	// 态 C：冻结时余额 fresh≤0（Kira）→ Kira 每日重置兜底。
	t.Run("fresh_zero_returns_kira_daily_reset", func(t *testing.T) {
		kira := newQuotaLifecycleKiraAccount(208)
		recent := now.Add(-30 * time.Second)
		kira.Extra[cnExtraKey(kira.Platform, cnBalanceExtraSuffixBalance)] = 0.0
		kira.Extra[cnExtraKey(kira.Platform, cnBalanceExtraSuffixUpdated)] = recent.UTC().Format(time.RFC3339)
		got := svc.resolveProbeDueAt(kira, signal, now)
		require.Equal(t, kiraNextDailyReset(now), got, "fresh VND=0 must fall back to daily reset")
	})

	// 态 D：TH 账号冻结 → 有未来 reset_at 时返回该 reset_at。
	t.Run("th_future_reset_at_returns_reset_at", func(t *testing.T) {
		th := newQuotaLifecycleTHAccount(207) // 快照 reset_at = 基点后 7 天（未来）
		got := svc.resolveProbeDueAt(th, signal, now)
		want, _ := time.Parse(time.RFC3339, now.Add(7*24*time.Hour).UTC().Format(time.RFC3339))
		require.Equal(t, want, got, "TH future reset_at must be returned as probe_due_at")
	})

	// probeDue 行为：优先 probe_due_at 资格字段；空则回退 RecoveryAt（回归保护）。
	t.Run("probe_due_at_future_is_not_due", func(t *testing.T) {
		st := &cnQuotaLifecycleState{ProbeDueAt: now.Add(time.Hour).UTC().Format(time.RFC3339)}
		require.False(t, st.probeDue(now, quotaLifecycleSweepInterval), "future probe_due_at must not be due")
	})
	t.Run("probe_due_at_past_is_due", func(t *testing.T) {
		st := &cnQuotaLifecycleState{ProbeDueAt: now.Add(-time.Hour).UTC().Format(time.RFC3339)}
		require.True(t, st.probeDue(now, quotaLifecycleSweepInterval), "past probe_due_at must be due")
	})
	t.Run("empty_probe_due_at_falls_back_to_recovery_at_future", func(t *testing.T) {
		st := &cnQuotaLifecycleState{RecoveryAt: now.Add(time.Hour).UTC().Format(time.RFC3339)}
		require.False(t, st.probeDue(now, quotaLifecycleSweepInterval), "empty probe_due_at must fall back to recovery_at")
	})
	t.Run("empty_probe_due_at_falls_back_to_recovery_at_past", func(t *testing.T) {
		st := &cnQuotaLifecycleState{RecoveryAt: now.Add(-time.Hour).UTC().Format(time.RFC3339)}
		require.True(t, st.probeDue(now, quotaLifecycleSweepInterval), "empty probe_due_at must fall back to recovery_at")
	})
}

// 组 2：重启不丢（持久化往返）。402 权威冻结落库后，经
// cnQuotaLifecycleStateFromExtra 读回，断言 state=exhausted + probe_due_at 非空且可解析
// + last_probe_outcome 保留（模拟进程重启后从 extra 重建状态）。
func TestAuthoritativeFreezePersistRoundTrip(t *testing.T) {
	th := newQuotaLifecycleTHAccount(207)
	repo := newQuotaLifecycleFakeRepo(th)
	alerts := &fakeQuotaAlertStore{}
	probe := &quotaProbeController{outcome: quotaProbeExhausted}
	svc := newQuotaLifecycleTestService(repo, alerts, probe)

	signal := QuotaExhaustionSignal{Status: 402, Scope: QuotaScopeAccount, Platform: th.Platform, Msg: "402"}
	require.NoError(t, svc.OnUpstreamQuotaExhaustedScoped(context.Background(), th, "Pass allowance used", signal))

	// 落库断言。
	_, parked := repo.parkedUntil(207)
	require.True(t, parked, "402 authoritative freeze must park")
	require.Contains(t, repo.parkedReason(207), cnQuotaExhaustedReason402Account, "must be tagged [402-account-authoritative]")

	// 模拟进程重启：从 extra 重新解析状态。
	st, ok := cnQuotaLifecycleStateFromExtra(th.Extra)
	require.True(t, ok, "state must be persisted in extra")
	require.Equal(t, cnQuotaLifecycleStateExhausted, st.State)
	require.NotEmpty(t, st.ProbeDueAt, "probe_due_at must survive restart")
	parsed, err := time.Parse(time.RFC3339, st.ProbeDueAt)
	require.NoError(t, err, "probe_due_at must be RFC3339 parseable")
	require.False(t, parsed.IsZero())
	require.Equal(t, cnQuotaLifecycleProbeExhausted, st.LastProbeOutcome, "last_probe_outcome must survive restart")
}

// 组 3：Kira 402×三态裁决表三行（方案 §3.2）。三行都冻结并带
// [402-account-authoritative] 标记，余额三态只影响 probe_due 时机。
func TestKira402ThreeStateRuling(t *testing.T) {
	balanceKey := func(a *Account) string { return cnExtraKey(a.Platform, cnBalanceExtraSuffixBalance) }
	updatedKey := func(a *Account) string { return cnExtraKey(a.Platform, cnBalanceExtraSuffixUpdated) }
	recent := func() time.Time { return quotaLifecycleBase.Add(-30 * time.Second) } // 基点前 1 分钟内，新鲜

	// 行 1：Kira 402 + fresh VND≤0 → park + 标记。
	t.Run("fresh_vnd_le_zero_parked_and_tagged", func(t *testing.T) {
		kira := newQuotaLifecycleKiraAccount(208)
		kira.Extra[balanceKey(kira)] = 0.0
		kira.Extra[updatedKey(kira)] = recent().UTC().Format(time.RFC3339)
		repo := newQuotaLifecycleFakeRepo(kira)
		svc := newQuotaLifecycleTestService(repo, &fakeQuotaAlertStore{}, &quotaProbeController{outcome: quotaProbeExhausted})
		require.NoError(t, svc.OnUpstreamQuotaExhausted(context.Background(), kira, "Insufficient VND (0 remaining)"))
		_, parked := repo.parkedUntil(208)
		require.True(t, parked)
		require.Contains(t, repo.parkedReason(208), cnQuotaExhaustedReason402Account)
		st, ok := cnQuotaLifecycleStateFromExtra(kira.Extra)
		require.True(t, ok)
		require.Equal(t, cnQuotaLifecycleStateExhausted, st.State)
	})

	// 行 2：Kira 402 + fresh VND>0 → 仍 park + 标记（上游即时权威，余额展示可能滞后）。
	t.Run("fresh_vnd_positive_still_parked_and_tagged", func(t *testing.T) {
		kira := newQuotaLifecycleKiraAccount(208)
		kira.Extra[balanceKey(kira)] = 1000.0
		kira.Extra[updatedKey(kira)] = recent().UTC().Format(time.RFC3339)
		repo := newQuotaLifecycleFakeRepo(kira)
		svc := newQuotaLifecycleTestService(repo, &fakeQuotaAlertStore{}, &quotaProbeController{outcome: quotaProbeExhausted})
		require.NoError(t, svc.OnUpstreamQuotaExhausted(context.Background(), kira, "Insufficient VND (account-level 402)"))
		_, parked := repo.parkedUntil(208)
		require.True(t, parked, "fresh VND>0 must still freeze on 402 authoritative")
		require.Contains(t, repo.parkedReason(208), cnQuotaExhaustedReason402Account)
		st, ok := cnQuotaLifecycleStateFromExtra(kira.Extra)
		require.True(t, ok)
		require.Equal(t, cnQuotaLifecycleStateExhausted, st.State)
		require.Equal(t, quotaLifecycleBase.UTC().Format(time.RFC3339), st.ProbeDueAt, "fresh VND>0 → immediate due (now)")
	})

	// 行 3：Kira 402 + unknown → 仍 park + 标记（402 单独即账号级到达证据）。
	t.Run("unknown_balance_still_parked_and_tagged", func(t *testing.T) {
		kira := newQuotaLifecycleKiraAccount(208)
		// 不设置任何 VND 余额键 → unknown。
		repo := newQuotaLifecycleFakeRepo(kira)
		svc := newQuotaLifecycleTestService(repo, &fakeQuotaAlertStore{}, &quotaProbeController{outcome: quotaProbeExhausted})
		require.NoError(t, svc.OnUpstreamQuotaExhausted(context.Background(), kira, "402 account-level"))
		_, parked := repo.parkedUntil(208)
		require.True(t, parked, "unknown balance must still freeze on 402 authoritative")
		require.Contains(t, repo.parkedReason(208), cnQuotaExhaustedReason402Account)
		st, ok := cnQuotaLifecycleStateFromExtra(kira.Extra)
		require.True(t, ok)
		require.Equal(t, cnQuotaLifecycleStateExhausted, st.State)
		require.Equal(t, kiraNextDailyReset(quotaLifecycleBase).UTC().Format(time.RFC3339), st.ProbeDueAt, "unknown → daily reset due")
	})
}
