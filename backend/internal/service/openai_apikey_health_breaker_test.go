package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Mocks
// ---------------------------------------------------------------------------

// healthSettingRepo is a minimal SettingRepository double. It records Get/Set
// calls and stores the last persisted value so round-trips can be inspected.
type healthSettingRepo struct {
	SettingRepository
	value    string
	setValue string
	getCalls int
	setCalls int
}

func (r *healthSettingRepo) GetValue(context.Context, string) (string, error) {
	r.getCalls++
	if r.setCalls > 0 {
		return r.setValue, nil
	}
	return r.value, nil
}
func (r *healthSettingRepo) Set(_ context.Context, _ string, value string) error {
	r.setCalls++
	r.setValue = value
	r.value = value
	return nil
}

// healthAccountRepoStub records SetTempUnschedulable writes made by the trip path.
type healthAccountRepoStub struct {
	AccountRepository
	setTempCalls int
	reason       string
}

func (r *healthAccountRepoStub) SetTempUnschedulable(_ context.Context, _ int64, _ time.Time, reason string) error {
	r.setTempCalls++
	r.reason = reason
	return nil
}
func (r *healthAccountRepoStub) ClearTempUnschedulable(_ context.Context, _ int64) error { return nil }
func (r *healthAccountRepoStub) ClearModelRateLimits(_ context.Context, _ int64) error   { return nil }

// healthCacheStub doubles as both the OpenAIAPIKeyHealthCache (record/clear) and
// the TempUnschedCache (SetTempUnsched/DeleteTempUnsched) injected into the
// RateLimitService.
type healthCacheStub struct {
	TempUnschedCache
	recordResult OpenAIAPIKeyHealthRecordResult
	recordErr    error
	recordCalls  int
	setCalls     int
	deleteCalls  int
}

func (c *healthCacheStub) RecordOpenAIAPIKeyHealthFailure(_ context.Context, _ int64, _, _, _, _ int) (OpenAIAPIKeyHealthRecordResult, error) {
	c.recordCalls++
	return c.recordResult, c.recordErr
}
func (c *healthCacheStub) SetTempUnsched(_ context.Context, _ int64, _ *TempUnschedState) error {
	c.setCalls++
	return nil
}
func (c *healthCacheStub) DeleteTempUnsched(_ context.Context, _ int64) error {
	c.deleteCalls++
	return nil
}

type healthRuntimeBlocker struct {
	blockCalls int
}

func (b *healthRuntimeBlocker) BlockAccountScheduling(*Account, time.Time, string) { b.blockCalls++ }
func (b *healthRuntimeBlocker) ClearAccountSchedulingBlock(int64)                  {}

// healthOpsRepoStub records CreateAlertEvent calls (used to assert the L2 warning
// path writes a queryable ops alert).
type healthOpsRepoStub struct {
	OpsRepository
	alertCalls int
	lastEvent  *OpsAlertEvent
}

func (r *healthOpsRepoStub) CreateAlertEvent(_ context.Context, event *OpsAlertEvent) (*OpsAlertEvent, error) {
	r.alertCalls++
	r.lastEvent = event
	return event, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

// healthAccount builds an API-key account on the given platform (non-pool by default).
func healthAccount(platform string) *Account {
	return &Account{ID: 42, Platform: platform, Type: AccountTypeAPIKey}
}

func newSettingService(t *testing.T, settings *OpenAIAPIKeyHealthBreakerSettings) (*SettingService, *healthSettingRepo) {
	t.Helper()
	repo := &healthSettingRepo{value: mustJSON(t, settings)}
	return NewSettingService(repo, &config.Config{}), repo
}

// ---------------------------------------------------------------------------
// classify: which upstream errors are attributable to the account
// ---------------------------------------------------------------------------

func TestClassifyOpenAIAPIKeyHealthFailureExclusions(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		eligible bool
	}{
		{name: "account attributed 502", err: &UpstreamFailoverError{StatusCode: http.StatusBadGateway}, eligible: true},
		{name: "request scoped capacity", err: &UpstreamFailoverError{StatusCode: 529, RequestScopedTransient: true}},
		{name: "provider scoped overload", err: &UpstreamFailoverError{StatusCode: 529, Scope: GatewayFailureScopeProvider}},
		{name: "dedicated same account retry", err: &UpstreamFailoverError{StatusCode: http.StatusTooManyRequests, RetryableOnSameAccount: true}},
		{name: "credential disable path", err: &UpstreamFailoverError{StatusCode: http.StatusUnauthorized, Stage: GatewayFailureStageAccountAuth, Scope: GatewayFailureScopeAccount}},
		{name: "client request", err: &UpstreamFailoverError{StatusCode: http.StatusBadRequest}},
		{name: "context canceled", err: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, eligible := classifyOpenAIAPIKeyHealthFailure(tt.err)
			require.Equal(t, tt.eligible, eligible)
		})
	}
}

// ---------------------------------------------------------------------------
// Scope matrix
// ---------------------------------------------------------------------------

func TestIsOpenAIAPIKeyHealthBreakerAccount(t *testing.T) {
	defaultScope := DefaultOpenAIAPIKeyHealthBreakerSettings()
	grokEnabled := func() *OpenAIAPIKeyHealthBreakerSettings {
		s := DefaultOpenAIAPIKeyHealthBreakerSettings()
		s.IncludeGrok = true
		return s
	}

	cases := []struct {
		name     string
		account  *Account
		settings *OpenAIAPIKeyHealthBreakerSettings
		want     bool
	}{
		{"openai apikey default scope", healthAccount(domain.PlatformOpenAI), defaultScope, true},
		{"deepseek apikey", healthAccount(domain.PlatformDeepseek), defaultScope, true},
		{"kimi apikey", healthAccount(domain.PlatformKimi), defaultScope, true},
		{"zhipu apikey", healthAccount(domain.PlatformZhipu), defaultScope, true},
		{"minimax apikey", healthAccount(domain.PlatformMiniMax), defaultScope, true},
		{"other apikey", healthAccount(domain.PlatformOther), defaultScope, true},
		{"grok excluded by default", healthAccount(domain.PlatformGrok), defaultScope, false},
		{"grok included when toggled", healthAccount(domain.PlatformGrok), grokEnabled(), true},
		{"oauth account never covered", &Account{ID: 1, Platform: domain.PlatformOpenAI, Type: AccountTypeOAuth}, defaultScope, false},
		// Unknown platforms are NOT folded into "openai" by the scope check: an
		// explicit allowlist rejects them so a non-OpenAI account (anthropic/claude/
		// bedrock/gemini/typo) can never be swept into the breaker scope.
		{"unknown platform never swept into scope", healthAccount("totally-unknown-platform"), defaultScope, false},
		{"anthropic account never covered", healthAccount(domain.PlatformAnthropic), defaultScope, false},
		{"unknown platform listed in scope still excluded (allowlist wins)", healthAccount("totally-unknown-platform"), &OpenAIAPIKeyHealthBreakerSettings{ScopePlatforms: []string{"totally-unknown-platform"}}, false},
		{"scope narrowed to openai only", healthAccount(domain.PlatformDeepseek), &OpenAIAPIKeyHealthBreakerSettings{ScopePlatforms: []string{domain.PlatformOpenAI}}, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, isOpenAIAPIKeyHealthBreakerAccount(c.account, c.settings))
		})
	}
}

// ---------------------------------------------------------------------------
// Threshold computation
// ---------------------------------------------------------------------------

func TestComputeHealthBreakerThresholds(t *testing.T) {
	s := &OpenAIAPIKeyHealthBreakerSettings{FailureThreshold: 10, WatchRatio: 0.4, WarningRatio: 0.7}
	watch, warning, trip := s.computeHealthBreakerThresholds()
	require.Equal(t, 10, trip)
	require.Equal(t, 4, watch) // floor(10*0.4)
	require.Equal(t, 7, warning)
	require.Less(t, watch, warning)
	require.Less(t, warning, trip)

	// Small threshold: all three are forced to 1 by the clamps but never collapse
	// into a single distinct boundary beyond what the Lua side also clamps.
	s2 := &OpenAIAPIKeyHealthBreakerSettings{FailureThreshold: 1, WatchRatio: 0.4, WarningRatio: 0.7}
	_, _, trip2 := s2.computeHealthBreakerThresholds()
	require.Equal(t, 1, trip2)
}

// ---------------------------------------------------------------------------
// Settings normalization & backward compatibility
// ---------------------------------------------------------------------------

func TestNormalizeOpenAIAPIKeyHealthBreakerSettings(t *testing.T) {
	t.Run("nil returns defaults", func(t *testing.T) {
		got := normalizeOpenAIAPIKeyHealthBreakerSettings(nil)
		def := DefaultOpenAIAPIKeyHealthBreakerSettings()
		require.Equal(t, def.ScopePlatforms, got.ScopePlatforms)
		require.Equal(t, 0.4, got.WatchRatio)
		require.Equal(t, 0.7, got.WarningRatio)
		require.NotNil(t, got.Probe)
	})

	t.Run("clamps and deduplicates scope", func(t *testing.T) {
		got := normalizeOpenAIAPIKeyHealthBreakerSettings(&OpenAIAPIKeyHealthBreakerSettings{
			WindowMinutes:    0,
			FailureThreshold: 0,
			CooldownMinutes:  999,
			ScopePlatforms:   []string{"openai", "", "openai", "deepseek"},
			WatchRatio:       0.001,
			WarningRatio:     0.95,
			Probe:            &OpenAIAPIKeyHealthBreakerProbeSettings{IntervalSeconds: 9999, MaxAttempts: 999},
		})
		require.Equal(t, 1, got.WindowMinutes)
		require.Equal(t, 1, got.FailureThreshold)
		require.Equal(t, 60, got.CooldownMinutes)
		require.Equal(t, []string{domain.PlatformOpenAI, domain.PlatformDeepseek}, got.ScopePlatforms)
		require.InDelta(t, 0.01, got.WatchRatio, 1e-9)  // clamped up to min
		require.InDelta(t, 0.95, got.WarningRatio, 1e-9)
		require.Equal(t, 600, got.Probe.IntervalSeconds) // clamped to max
		require.Equal(t, 100, got.Probe.MaxAttempts)     // clamped to max
	})

	t.Run("warning must stay above watch", func(t *testing.T) {
		got := normalizeOpenAIAPIKeyHealthBreakerSettings(&OpenAIAPIKeyHealthBreakerSettings{
			FailureThreshold: 10,
			WatchRatio:       0.8,
			WarningRatio:     0.5, // below watch on purpose
		})
		require.Greater(t, got.WarningRatio, got.WatchRatio)
	})

	t.Run("nil probe gets safe defaults", func(t *testing.T) {
		got := normalizeOpenAIAPIKeyHealthBreakerSettings(&OpenAIAPIKeyHealthBreakerSettings{})
		require.NotNil(t, got.Probe)
		require.False(t, got.Probe.Enabled)
		require.Equal(t, 30, got.Probe.IntervalSeconds)
		require.Equal(t, 1, got.Probe.MaxAttempts)
	})
}

func TestGetSetSettingsRoundTrip(t *testing.T) {
	repo := &healthSettingRepo{}
	ss := NewSettingService(repo, &config.Config{})

	input := &OpenAIAPIKeyHealthBreakerSettings{
		Enabled:          true,
		WindowMinutes:    3,
		FailureThreshold: 7,
		CooldownMinutes:  10,
		ScopePlatforms:   []string{domain.PlatformOpenAI, domain.PlatformDeepseek},
		IncludeGrok:      true,
		WatchRatio:       0.3,
		WarningRatio:     0.6,
		Probe:            &OpenAIAPIKeyHealthBreakerProbeSettings{Enabled: true, IntervalSeconds: 120, MaxAttempts: 5},
	}
	require.NoError(t, ss.SetOpenAIAPIKeyHealthBreakerSettings(context.Background(), input))

	// Persisted JSON must round-trip back to the same (normalized) settings.
	require.NotZero(t, repo.setCalls)
	got, err := ss.GetOpenAIAPIKeyHealthBreakerSettings(context.Background())
	require.NoError(t, err)
	require.True(t, got.Enabled)
	require.Equal(t, 3, got.WindowMinutes)
	require.Equal(t, 7, got.FailureThreshold)
	require.Equal(t, []string{domain.PlatformOpenAI, domain.PlatformDeepseek}, got.ScopePlatforms)
	require.True(t, got.IncludeGrok)
	require.InDelta(t, 0.3, got.WatchRatio, 1e-9)
	require.InDelta(t, 0.6, got.WarningRatio, 1e-9)
	require.NotNil(t, got.Probe)
	require.True(t, got.Probe.Enabled)
	require.Equal(t, 120, got.Probe.IntervalSeconds)
	require.Equal(t, 5, got.Probe.MaxAttempts)
}

func TestGetSettingsBackwardCompatOldJSON(t *testing.T) {
	// An old-style stored document without scope/watch/warning/probe fields must
	// load with the new defaults for the missing fields.
	old := `{"enabled":true,"window_minutes":2,"failure_threshold":10,"cooldown_minutes":5}`
	ss := NewSettingService(&healthSettingRepo{value: old}, &config.Config{})
	got, err := ss.GetOpenAIAPIKeyHealthBreakerSettings(context.Background())
	require.NoError(t, err)
	require.True(t, got.Enabled)
	require.Equal(t, DefaultAccountHealthBreakerScopePlatforms, got.ScopePlatforms)
	require.InDelta(t, 0.4, got.WatchRatio, 1e-9)
	require.InDelta(t, 0.7, got.WarningRatio, 1e-9)
	require.NotNil(t, got.Probe)
}

// ---------------------------------------------------------------------------
// Disabled = status quo (regression guard)
// ---------------------------------------------------------------------------

func TestBreakerDisabledIsNoOp(t *testing.T) {
	// Empty repo -> default (disabled) settings.
	ss := NewSettingService(&healthSettingRepo{}, &config.Config{})
	cache := &healthCacheStub{}
	repo := &healthAccountRepoStub{}
	blocker := &healthRuntimeBlocker{}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, cache)
	svc.SetSettingService(ss)
	svc.SetOpenAIAPIKeyHealthCache(cache)
	svc.SetAccountRuntimeBlocker(blocker)

	require.False(t, svc.ObserveOpenAIAPIKeyHealthFailure(context.Background(), healthAccount(domain.PlatformOpenAI), &UpstreamFailoverError{StatusCode: http.StatusBadGateway}))
	require.Zero(t, cache.recordCalls)
	require.Zero(t, repo.setTempCalls)
	require.Zero(t, blocker.blockCalls)

	// Success path must also avoid touching the cache when disabled (it is a
	// no-op in both disabled and enabled modes).
	svc.ObserveOpenAIAPIKeyHealthSuccess(context.Background(), healthAccount(domain.PlatformOpenAI))
	require.Zero(t, cache.recordCalls)
}

// ---------------------------------------------------------------------------
// Three-tier escalation with the real Redis rolling window
// ---------------------------------------------------------------------------

func newRateLimitWithStubCache(t *testing.T, cache *healthCacheStub, ss *SettingService, ops OpsRepository) *RateLimitService {
	t.Helper()
	repo := &healthAccountRepoStub{}
	blocker := &healthRuntimeBlocker{}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, cache)
	svc.SetSettingService(ss)
	svc.SetOpenAIAPIKeyHealthCache(cache)
	svc.SetAccountRuntimeBlocker(blocker)
	if ops != nil {
		svc.SetOpsRepository(ops)
	}
	return svc
}

// TestThreeTierEscalationBreakerReactions drives the breaker with staged cache
// results to confirm: L1 watch logs only (no scheduling impact), L2 warning logs
// and writes an ops alert, and L3 trip persists + blocks scheduling. The actual
// Redis rolling-window three-tier escalation/debounce is covered at the cache
// layer by TestOpenAIAPIKeyHealthCacheTripsWithinRollingWindow.
func TestThreeTierEscalationBreakerReactions(t *testing.T) {
	ss, _ := newSettingService(t, &OpenAIAPIKeyHealthBreakerSettings{
		Enabled: true, WindowMinutes: 1, FailureThreshold: 3, CooldownMinutes: 5,
	})
	cache := &healthCacheStub{}
	ops := &healthOpsRepoStub{}
	svc := newRateLimitWithStubCache(t, cache, ss, ops)
	account := healthAccount(domain.PlatformOpenAI)
	bad := &UpstreamFailoverError{StatusCode: http.StatusBadGateway}

	// attempt 1 -> L1 watch only
	cache.recordResult = OpenAIAPIKeyHealthRecordResult{Count: 1, TrippedWatch: true}
	require.False(t, svc.ObserveOpenAIAPIKeyHealthFailure(context.Background(), account, bad))
	require.Zero(t, svc.accountRepo.(*healthAccountRepoStub).setTempCalls)
	require.Zero(t, ops.alertCalls)

	// attempt 2 -> L2 warning + ops alert
	cache.recordResult = OpenAIAPIKeyHealthRecordResult{Count: 2, TrippedWarning: true}
	require.False(t, svc.ObserveOpenAIAPIKeyHealthFailure(context.Background(), account, bad))
	require.Zero(t, svc.accountRepo.(*healthAccountRepoStub).setTempCalls)
	require.Equal(t, 1, ops.alertCalls)
	require.Equal(t, "P1", ops.lastEvent.Severity)

	// attempt 3 -> L3 trip: persist + block
	cache.recordResult = OpenAIAPIKeyHealthRecordResult{Count: 3, TrippedTrip: true}
	require.True(t, svc.ObserveOpenAIAPIKeyHealthFailure(context.Background(), account, bad))
	require.Equal(t, 1, svc.accountRepo.(*healthAccountRepoStub).setTempCalls)
}

func TestObserveSuccessDoesNotClearWindow(t *testing.T) {
	ss, _ := newSettingService(t, &OpenAIAPIKeyHealthBreakerSettings{
		Enabled: true, WindowMinutes: 1, FailureThreshold: 3, CooldownMinutes: 5,
	})
	cache := &healthCacheStub{}
	svc := newRateLimitWithStubCache(t, cache, ss, nil)

	// A single failure seeds the rolling window (1 recorded failure).
	cache.recordResult = OpenAIAPIKeyHealthRecordResult{Count: 1}
	svc.ObserveOpenAIAPIKeyHealthFailure(context.Background(), healthAccount(domain.PlatformOpenAI), &UpstreamFailoverError{StatusCode: http.StatusBadGateway})
	require.Equal(t, 1, cache.recordCalls)

	// A successful schedule is a NO-OP: it must NOT reset the window and must NOT
	// touch the cache at all. This is what keeps a flaky channel's failures from
	// being wiped by an intermittent success.
	svc.ObserveOpenAIAPIKeyHealthSuccess(context.Background(), healthAccount(domain.PlatformOpenAI))
	require.Equal(t, 1, cache.recordCalls, "success must not clear or rewrite the window")

	// Further failures keep accumulating on top of the prior failure rather than
	// starting fresh, so the breaker can still trip.
	cache.recordResult = OpenAIAPIKeyHealthRecordResult{Count: 2}
	svc.ObserveOpenAIAPIKeyHealthFailure(context.Background(), healthAccount(domain.PlatformOpenAI), &UpstreamFailoverError{StatusCode: http.StatusBadGateway})
	require.Equal(t, 2, cache.recordCalls)

	cache.recordResult = OpenAIAPIKeyHealthRecordResult{Count: 3, TrippedTrip: true}
	require.True(t, svc.ObserveOpenAIAPIKeyHealthFailure(context.Background(), healthAccount(domain.PlatformOpenAI), &UpstreamFailoverError{StatusCode: http.StatusBadGateway}))
}

// ---------------------------------------------------------------------------
// Trip persistence + runtime block (L3)
// ---------------------------------------------------------------------------

func TestHealthBreakerTripPersistsAndBlocks(t *testing.T) {
	ss, _ := newSettingService(t, &OpenAIAPIKeyHealthBreakerSettings{
		Enabled: true, WindowMinutes: 1, FailureThreshold: 3, CooldownMinutes: 5,
	})
	cache := &healthCacheStub{recordResult: OpenAIAPIKeyHealthRecordResult{TrippedTrip: true}}
	repo := &healthAccountRepoStub{}
	blocker := &healthRuntimeBlocker{}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, cache)
	svc.SetSettingService(ss)
	svc.SetOpenAIAPIKeyHealthCache(cache)
	svc.SetAccountRuntimeBlocker(blocker)
	account := healthAccount(domain.PlatformOpenAI)

	require.True(t, svc.ObserveOpenAIAPIKeyHealthFailure(context.Background(), account, &UpstreamFailoverError{StatusCode: http.StatusBadGateway}))

	require.Equal(t, 1, cache.recordCalls)
	require.Equal(t, 1, cache.setCalls) // temp_unsched cache write
	require.Equal(t, 1, repo.setTempCalls)
	require.Equal(t, 1, blocker.blockCalls)
	require.NotNil(t, account.TempUnschedulableUntil)
	require.Contains(t, repo.reason, openAIAPIKeyHealthBreakerReason)

	var state TempUnschedState
	require.NoError(t, json.Unmarshal([]byte(repo.reason), &state))
	require.Equal(t, HealthBreakerTierTrip, state.Tier)
	require.Equal(t, openAIAPIKeyHealthBreakerReason, state.MatchedKeyword)
}

// ---------------------------------------------------------------------------
// Phase C: probe-based recovery
// ---------------------------------------------------------------------------

// probeRepoMock implements the narrow accountHealthProbeRepository.
type probeRepoMock struct {
	accountHealthProbeRepository
	accounts      map[int64]*Account
	candidates    []*Account
	setReasonCall int
	listCalls     int
}

func (m *probeRepoMock) ListTempUnschedulableAccounts(_ context.Context, _ time.Time, _ int) ([]*Account, error) {
	m.listCalls++
	return m.candidates, nil
}
func (m *probeRepoMock) GetByID(_ context.Context, id int64) (*Account, error) {
	if a, ok := m.accounts[id]; ok {
		return a, nil
	}
	return nil, errors.New("not found")
}
func (m *probeRepoMock) SetTempUnschedulableReason(_ context.Context, id int64, reason string) error {
	m.setReasonCall++
	if a, ok := m.accounts[id]; ok {
		a.TempUnschedulableReason = reason
	}
	return nil
}

// probeRateLimitRepo records ClearTempUnschedulable calls from the probe recovery.
type probeRateLimitRepo struct {
	AccountRepository
	clearTempCalls int
}

func (r *probeRateLimitRepo) ClearTempUnschedulable(_ context.Context, _ int64) error {
	r.clearTempCalls++
	return nil
}
func (r *probeRateLimitRepo) ClearModelRateLimits(_ context.Context, _ int64) error { return nil }

// parkedBreakerAccount builds an account blocked by the health breaker, whose
// cooldown has not yet expired.
func parkedBreakerAccount(id int64, probeAttempts int) *Account {
	until := time.Now().Add(5 * time.Minute)
	state := TempUnschedState{
		UntilUnix:      until.Unix(),
		MatchedKeyword: openAIAPIKeyHealthBreakerReason,
		Tier:           HealthBreakerTierTrip,
		ProbeAttempts:  probeAttempts,
	}
	reason, _ := json.Marshal(state)
	return &Account{
		ID:                        id,
		Platform:                  domain.PlatformOpenAI,
		Type:                      AccountTypeAPIKey,
		TempUnschedulableUntil:    &until,
		TempUnschedulableReason:   string(reason),
	}
}

func newProbeService(t *testing.T, probeEnabled bool, maxAttempts int, rl *RateLimitService, repo *probeRepoMock) *AccountHealthRecoveryProbeService {
	t.Helper()
	settings := &OpenAIAPIKeyHealthBreakerSettings{
		Enabled: true,
		Probe:   &OpenAIAPIKeyHealthBreakerProbeSettings{Enabled: probeEnabled, IntervalSeconds: 30, MaxAttempts: maxAttempts},
	}
	ss, _ := newSettingService(t, settings)
	p := NewAccountHealthRecoveryProbeService(repo, nil, &config.Config{}, rl, ss, nil)
	return p
}

func TestProbeRecoverySuccessClearsEarly(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{7: parkedBreakerAccount(7, 0)},
		candidates: []*Account{parkedBreakerAccount(7, 0)},
	}
	p := newProbeService(t, true, 10, rl, repo)
	p.SetProbeOverride(func(_ context.Context, _ *Account) (bool, error) { return true, nil })

	p.RunOnce(context.Background())

	require.Equal(t, 1, rlRepo.clearTempCalls) // recovered early
	require.Zero(t, repo.setReasonCall)        // no attempt bump on success
}

func TestProbeFailureStaysParkedAndBumpsAttempts(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	cand := parkedBreakerAccount(7, 0)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{7: cand},
		candidates: []*Account{cand},
	}
	p := newProbeService(t, true, 10, rl, repo)
	p.SetProbeOverride(func(_ context.Context, _ *Account) (bool, error) { return false, nil })

	p.RunOnce(context.Background())

	require.Zero(t, rlRepo.clearTempCalls) // not recovered
	require.Equal(t, 1, repo.setReasonCall) // attempt counter bumped

	var state TempUnschedState
	require.NoError(t, json.Unmarshal([]byte(cand.TempUnschedulableReason), &state))
	require.Equal(t, 1, state.ProbeAttempts)
}

func TestProbeDegradedPlatformFallsBackToExpiry(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	cand := parkedBreakerAccount(7, 0)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{7: cand},
		candidates: []*Account{cand},
	}
	p := newProbeService(t, true, 10, rl, repo)
	// A degraded platform returns an error: the probe must not spam, it bumps the
	// attempt counter and lets the cooldown expire.
	p.SetProbeOverride(func(_ context.Context, _ *Account) (bool, error) { return false, errors.New("no safe endpoint") })

	p.RunOnce(context.Background())

	require.Zero(t, rlRepo.clearTempCalls)
	require.Equal(t, 1, repo.setReasonCall)
}

func TestProbeSwitchTogglesAtRuntimeWithoutRestart(t *testing.T) {
	// The probe loop re-reads settings.probe.enabled on every tick, so flipping
	// the switch in admin settings must take effect on the already-running loop
	// without restarting the process. This test proves that: disabled RunOnce is a
	// no-op, and after enabling via the setting service the SAME service instance
	// performs the sweep on the next tick.
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	cand := parkedBreakerAccount(7, 0)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{7: cand},
		candidates: []*Account{cand},
	}

	ss, _ := newSettingService(t, &OpenAIAPIKeyHealthBreakerSettings{
		Enabled: true,
		Probe:   &OpenAIAPIKeyHealthBreakerProbeSettings{Enabled: false, IntervalSeconds: 30, MaxAttempts: 10},
	})
	p := NewAccountHealthRecoveryProbeService(repo, nil, &config.Config{}, rl, ss, nil)
	p.SetProbeOverride(func(_ context.Context, _ *Account) (bool, error) { return true, nil })

	// Disabled at start: the running loop never touches the candidate.
	p.RunOnce(context.Background())
	require.Zero(t, rlRepo.clearTempCalls)

	// Flip the switch ON through the setting service (simulates an admin toggle).
	require.NoError(t, ss.SetOpenAIAPIKeyHealthBreakerSettings(context.Background(), &OpenAIAPIKeyHealthBreakerSettings{
		Enabled: true,
		Probe:   &OpenAIAPIKeyHealthBreakerProbeSettings{Enabled: true, IntervalSeconds: 30, MaxAttempts: 10},
	}))

	// Same service instance, no restart: the next tick now performs the sweep.
	p.RunOnce(context.Background())
	require.Equal(t, 1, rlRepo.clearTempCalls, "enabling the probe must take effect on the running loop")
}

func TestProbeGivesUpAfterMaxAttempts(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	cand := parkedBreakerAccount(7, 10) // already at max
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{7: cand},
		candidates: []*Account{cand},
	}
	p := newProbeService(t, true, 10, rl, repo)

	overrideCalls := 0
	p.SetProbeOverride(func(_ context.Context, _ *Account) (bool, error) {
		overrideCalls++
		return false, nil
	})

	p.RunOnce(context.Background())

	require.Zero(t, overrideCalls)           // never probed
	require.Zero(t, rlRepo.clearTempCalls)   // not recovered
	require.Zero(t, repo.setReasonCall)      // no further bookkeeping
}

func TestProbeSkipsNonBreakerBlocks(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	// A block whose reason is NOT the health breaker must be left alone.
	until := time.Now().Add(5 * time.Minute)
	state := TempUnschedState{UntilUnix: until.Unix(), MatchedKeyword: "some_other_rule", Tier: 3}
	reason, _ := json.Marshal(state)
	cand := &Account{ID: 8, Platform: domain.PlatformOpenAI, Type: AccountTypeAPIKey, TempUnschedulableUntil: &until, TempUnschedulableReason: string(reason)}
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{8: cand},
		candidates: []*Account{cand},
	}
	p := newProbeService(t, true, 10, rl, repo)
	overrideCalls := 0
	p.SetProbeOverride(func(_ context.Context, _ *Account) (bool, error) {
		overrideCalls++
		return false, nil
	})

	p.RunOnce(context.Background())

	require.Zero(t, overrideCalls)
	require.Zero(t, rlRepo.clearTempCalls)
}

func TestProbeDisabledIsNoOp(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{7: parkedBreakerAccount(7, 0)},
		candidates: []*Account{parkedBreakerAccount(7, 0)},
	}
	p := newProbeService(t, false, 10, rl, repo) // probe disabled
	overrideCalls := 0
	p.SetProbeOverride(func(_ context.Context, _ *Account) (bool, error) {
		overrideCalls++
		return true, nil
	})

	p.RunOnce(context.Background())

	require.Zero(t, repo.listCalls)    // never listed candidates
	require.Zero(t, overrideCalls)     // never probed
	require.Zero(t, rlRepo.clearTempCalls)
}
