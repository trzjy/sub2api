package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"go.uber.org/zap"
)

// accountHealthProbeCandidateLimit caps how many temp-unschedulable accounts a single
// probe sweep evaluates. The breaker cooldown is short (minutes), so the active set
// is small; the cap protects against pathological backlog after a long outage.
const accountHealthProbeCandidateLimit = 200

// accountHealthProbeRepository is the narrow repository surface the recovery probe
// needs. It is intentionally a subset of AccountRepository so the real repository
// satisfies it without forcing every AccountRepository test double to implement the
// probe-specific methods.
type accountHealthProbeRepository interface {
	GetByID(ctx context.Context, id int64) (*Account, error)
	ListTempUnschedulableAccounts(ctx context.Context, now time.Time, limit int) ([]*Account, error)
	SetTempUnschedulableReason(ctx context.Context, id int64, reason string) error
}

// AccountHealthRecoveryProbeService implements the optional Phase C probe-based
// recovery. While an account is inside the health-breaker cooldown, it periodically
// sends a cheap, real upstream request (preferring /models, else a minimal
// completion). A successful probe clears the temp-unschedulable state early; a
// failing probe keeps the account parked until either the cooldown expires or
// maxAttempts is reached (then it falls back to expiry-based recovery).
//
// The probe is opt-in: Start() is a no-op unless the admin explicitly enables
// settings.probe.enabled. SSRF protection is enforced via cnValidateProbeURL before
// any request is built, reusing the same gate as the gateway's own outbound calls.
type AccountHealthRecoveryProbeService struct {
	accountRepo         accountHealthProbeRepository
	httpUpstream        HTTPUpstream
	cfg                 *config.Config
	rateLimit           *RateLimitService
	settingService      *SettingService
	tlsFPProfileService *TLSFingerprintProfileService

	marker string

	startMu sync.Mutex
	stopCh  chan struct{}
	wg      sync.WaitGroup

	// probeOverride, when set, replaces the real upstream probe (used by tests).
	probeOverride func(ctx context.Context, account *Account) (ok bool, err error)
}

// NewAccountHealthRecoveryProbeService constructs the recovery probe service.
func NewAccountHealthRecoveryProbeService(
	accountRepo accountHealthProbeRepository,
	httpUpstream HTTPUpstream,
	cfg *config.Config,
	rateLimit *RateLimitService,
	settingService *SettingService,
	tlsFPProfileService *TLSFingerprintProfileService,
) *AccountHealthRecoveryProbeService {
	return &AccountHealthRecoveryProbeService{
		accountRepo:         accountRepo,
		httpUpstream:        httpUpstream,
		cfg:                 cfg,
		rateLimit:           rateLimit,
		settingService:      settingService,
		tlsFPProfileService: tlsFPProfileService,
		marker:              openAIAPIKeyHealthBreakerReason,
	}
}

// probeEnabled reports whether the recovery probe is enabled in current settings.
func (p *AccountHealthRecoveryProbeService) probeEnabled(ctx context.Context) bool {
	if p == nil || p.settingService == nil {
		return false
	}
	settings, err := p.settingService.GetOpenAIAPIKeyHealthBreakerSettings(ctx)
	if err != nil || settings == nil || settings.Probe == nil {
		return false
	}
	return settings.Enabled && settings.Probe.Enabled
}

// Start launches the probe sweep loop if enabled. It is safe to call when disabled
// (it returns immediately) and safe to call multiple times (only the first
// effective start spawns a goroutine).
func (p *AccountHealthRecoveryProbeService) Start(ctx context.Context) {
	if p == nil {
		return
	}
	if !p.probeEnabled(ctx) {
		logger.L().Info("openai.apikey_health_probe_disabled")
		return
	}
	p.startMu.Lock()
	defer p.startMu.Unlock()
	if p.stopCh != nil {
		return
	}

	interval := 60 * time.Second
	if settings, err := p.settingService.GetOpenAIAPIKeyHealthBreakerSettings(ctx); err == nil && settings != nil && settings.Probe != nil {
		if settings.Probe.IntervalSeconds >= 30 {
			interval = time.Duration(settings.Probe.IntervalSeconds) * time.Second
		}
	}

	p.stopCh = make(chan struct{})
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		logger.L().Info("openai.apikey_health_probe_started", zap.Duration("interval", interval))
		for {
			select {
			case <-p.stopCh:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.RunOnce(ctx)
			}
		}
	}()
}

// Stop terminates the probe loop if running.
func (p *AccountHealthRecoveryProbeService) Stop() {
	if p == nil {
		return
	}
	p.startMu.Lock()
	stopCh := p.stopCh
	p.stopCh = nil
	p.startMu.Unlock()
	if stopCh != nil {
		close(stopCh)
		p.wg.Wait()
	}
}

// RunOnce performs a single probe sweep over the currently cooldowned accounts.
func (p *AccountHealthRecoveryProbeService) RunOnce(ctx context.Context) {
	if p == nil || p.accountRepo == nil {
		return
	}
	if !p.probeEnabled(ctx) {
		return
	}
	settings, err := p.settingService.GetOpenAIAPIKeyHealthBreakerSettings(ctx)
	if err != nil || settings == nil || settings.Probe == nil {
		return
	}
	maxAttempts := settings.Probe.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	now := time.Now()
	candidates, err := p.accountRepo.ListTempUnschedulableAccounts(ctx, now, accountHealthProbeCandidateLimit)
	if err != nil {
		logger.L().Warn("openai.apikey_health_probe_list_failed", zap.Error(err))
		return
	}
	for _, acc := range candidates {
		if !p.isHealthBreakerTrip(acc, now) {
			continue
		}
		p.probeAccount(ctx, acc, maxAttempts)
	}
}

// isHealthBreakerTrip reports whether the account's active temp-unschedulable block
// was opened by the health breaker and has not yet expired.
func (p *AccountHealthRecoveryProbeService) isHealthBreakerTrip(acc *Account, now time.Time) bool {
	if acc == nil || acc.TempUnschedulableReason == "" {
		return false
	}
	if acc.TempUnschedulableUntil == nil || !acc.TempUnschedulableUntil.After(now) {
		return false
	}
	state, ok := parseHealthBreakerState(acc.TempUnschedulableReason)
	if !ok || state == nil {
		return false
	}
	return state.MatchedKeyword == p.marker
}

// parseHealthBreakerState parses the JSON reason payload written by the breaker.
func parseHealthBreakerState(reason string) (*TempUnschedState, bool) {
	if strings.TrimSpace(reason) == "" {
		return nil, false
	}
	var state TempUnschedState
	if err := json.Unmarshal([]byte(reason), &state); err != nil {
		return nil, false
	}
	return &state, true
}

// probeAccount performs one probe attempt for a cooldowned account.
func (p *AccountHealthRecoveryProbeService) probeAccount(ctx context.Context, acc *Account, maxAttempts int) {
	state, ok := parseHealthBreakerState(acc.TempUnschedulableReason)
	if !ok || state == nil {
		return
	}
	attempts := state.ProbeAttempts + 1
	if attempts > maxAttempts {
		// Reached the attempt ceiling: give up probing and let the cooldown expire.
		logger.L().Info("openai.apikey_health_probe_gave_up",
			zap.Int64("account_id", acc.ID),
			zap.Int("max_attempts", maxAttempts),
		)
		return
	}

	ok, err := p.runProbe(ctx, acc)
	if err != nil {
		// Degraded platform (no safe/cheap endpoint): stop probing, fall back to expiry.
		logger.L().Warn("openai.apikey_health_probe_degraded",
			zap.Int64("account_id", acc.ID),
			zap.Error(err),
		)
		p.bumpProbeAttempts(ctx, acc.ID, maxAttempts)
		return
	}

	if ok {
		if p.rateLimit != nil {
			if clearErr := p.rateLimit.ClearTempUnschedulable(ctx, acc.ID); clearErr != nil {
				logger.L().Warn("openai.apikey_health_probe_clear_failed",
					zap.Int64("account_id", acc.ID),
					zap.Error(clearErr),
				)
				return
			}
		}
		logger.L().Info("openai.apikey_health_probe_recovered",
			zap.Int64("account_id", acc.ID),
			zap.Int("attempt", attempts),
		)
		return
	}

	// Probe failed: bump the attempt counter; keep the account parked.
	p.bumpProbeAttempts(ctx, acc.ID, attempts)
	logger.L().Info("openai.apikey_health_probe_failed",
		zap.Int64("account_id", acc.ID),
		zap.Int("attempt", attempts),
		zap.Int("max_attempts", maxAttempts),
	)
}

// bumpProbeAttempts rewrites the temp-unschedulable reason to record the new attempt
// count. It is best-effort: a store failure only skips this cycle's bookkeeping.
func (p *AccountHealthRecoveryProbeService) bumpProbeAttempts(ctx context.Context, accountID int64, attempts int) {
	if p.accountRepo == nil {
		return
	}
	acc, err := p.accountRepo.GetByID(ctx, accountID)
	if err != nil || acc == nil {
		return
	}
	state, ok := parseHealthBreakerState(acc.TempUnschedulableReason)
	if !ok || state == nil {
		return
	}
	state.ProbeAttempts = attempts
	updated, mErr := json.Marshal(state)
	if mErr != nil {
		return
	}
	if err := p.accountRepo.SetTempUnschedulableReason(ctx, accountID, string(updated)); err != nil {
		logger.L().Debug("openai.apikey_health_probe_attempt_update_failed",
			zap.Int64("account_id", accountID), zap.Error(err))
	}
}

// runProbe executes the upstream probe, preferring a real request but allowing an
// override for tests.
func (p *AccountHealthRecoveryProbeService) runProbe(ctx context.Context, acc *Account) (bool, error) {
	if p.probeOverride != nil {
		return p.probeOverride(ctx, acc)
	}
	return p.probeUpstream(ctx, acc)
}

// probeUpstream sends a cheap real upstream request through the account's own
// forwarding path. It prefers the zero-cost /models endpoint; if that cannot be
// constructed safely (e.g. a platform without a /models surface) it returns an error
// so the caller degrades to expiry-based recovery rather than spamming probes.
func (p *AccountHealthRecoveryProbeService) probeUpstream(ctx context.Context, acc *Account) (bool, error) {
	if p.httpUpstream == nil || p.cfg == nil {
		return false, errors.New("probe transport not configured")
	}
	validateBaseURL := func(raw string) (string, error) {
		return cnValidateProbeURL(p.cfg, raw)
	}
	req, err := buildOpenAIAPIKeyModelsRequest(ctx, acc, validateBaseURL)
	if err != nil {
		return false, fmt.Errorf("build probe request: %w", err)
	}

	proxyURL := ""
	if acc.ProxyID != nil && acc.Proxy != nil {
		proxyURL = acc.Proxy.URL()
	}
	var tlsProfile *tlsfingerprint.Profile
	if p.tlsFPProfileService != nil {
		tlsProfile = p.tlsFPProfileService.ResolveTLSProfile(acc)
	}

	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := p.httpUpstream.DoWithTLS(req.WithContext(callCtx), proxyURL, acc.ID, acc.Concurrency, tlsProfile)
	if err != nil {
		return false, nil
	}
	defer func() { _ = resp.Body.Close() }()

	// A 2xx (including 200 from /models) confirms the account is healthy again.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
		return true, nil
	}
	// Drain to reuse the connection, then treat any non-2xx as an unhealthy probe.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
	return false, nil
}

// SetProbeOverride installs a probe function override (tests only).
func (p *AccountHealthRecoveryProbeService) SetProbeOverride(fn func(ctx context.Context, account *Account) (bool, error)) {
	if p != nil {
		p.probeOverride = fn
	}
}
