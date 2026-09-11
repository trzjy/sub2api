package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

const openAIAPIKeyHealthBreakerReason = "openai_apikey_health_breaker"

// Health breaker tier identifiers recorded into TempUnschedState.Tier.
const (
	HealthBreakerTierWatch   = 1
	HealthBreakerTierWarning = 2
	HealthBreakerTierTrip    = 3
)

// isOpenAIAPIKeyHealthBreakerAccount reports whether the breaker may attribute
// failures to this account. Coverage:
//   - Only API-key accounts qualify (OAuth / PAT / Bedrock excluded).
//   - Platform must be in settings.ScopePlatforms (default: openai/deepseek/kimi/
//     zhipu/minimax/other). grok is excluded unless settings.IncludeGrok is set.
//   - The legacy pool_mode restriction is intentionally removed: the breaker now
//     covers all API-key accounts on the supported OpenAI-compatible platforms.
func isOpenAIAPIKeyHealthBreakerAccount(account *Account, settings *OpenAIAPIKeyHealthBreakerSettings) bool {
	if account == nil || account.Type != AccountTypeAPIKey {
		return false
	}
	if settings == nil {
		return false
	}
	platform := NormalizeOpenAICompatiblePlatform(account.Platform)
	if platform == PlatformGrok {
		return settings.IncludeGrok
	}
	for _, p := range settings.ScopePlatforms {
		if NormalizeOpenAICompatiblePlatform(p) == platform {
			return true
		}
	}
	return false
}

// computeHealthBreakerThresholds derives the L1 (watch) and L2 (warning) trigger
// counts from FailureThreshold and the configured ratios. They are guaranteed to
// satisfy 1 <= watch < warning < trip, so the three tiers never collapse onto a
// single boundary (degenerate trip<3 inputs are clamped by the Lua side too).
func (s *OpenAIAPIKeyHealthBreakerSettings) computeHealthBreakerThresholds() (watch, warning, trip int) {
	trip = s.FailureThreshold
	if trip < 1 {
		trip = 1
	}
	watch = int(math.Floor(float64(trip) * s.WatchRatio))
	warning = int(math.Floor(float64(trip) * s.WarningRatio))
	if watch < 1 {
		watch = 1
	}
	if warning < 1 {
		warning = 1
	}
	if warning >= trip {
		warning = trip - 1
	}
	if warning < 1 {
		warning = 1
	}
	if watch >= warning {
		watch = warning - 1
	}
	if watch < 1 {
		watch = 1
	}
	return watch, warning, trip
}

func classifyOpenAIAPIKeyHealthFailure(err error) (int, []byte, bool) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return 0, nil, false
	}

	var failoverErr *UpstreamFailoverError
	if errors.As(err, &failoverErr) {
		// These failures already have dedicated recovery/state handling or are not
		// attributable to the selected account.
		if failoverErr.IsCredentialFailure() ||
			failoverErr.RequestScopedTransient ||
			failoverErr.RetryableOnSameAccount ||
			failoverErr.Scope == GatewayFailureScopeRequest ||
			failoverErr.Scope == GatewayFailureScopeProvider {
			return failoverErr.StatusCode, failoverErr.ResponseBody, false
		}
		if failoverErr.StatusCode == http.StatusTooManyRequests || failoverErr.StatusCode >= http.StatusInternalServerError {
			return failoverErr.StatusCode, failoverErr.ResponseBody, true
		}
		return failoverErr.StatusCode, failoverErr.ResponseBody, false
	}

	var imageErr *OpenAIImagesUpstreamError
	if errors.As(err, &imageErr) {
		if imageErr.StatusCode == http.StatusTooManyRequests || imageErr.StatusCode >= http.StatusInternalServerError {
			return imageErr.StatusCode, []byte(strings.TrimSpace(imageErr.Message)), true
		}
	}
	return 0, nil, false
}

// ObserveOpenAIAPIKeyHealthFailure records an upstream failure against the account's
// rolling health window and applies the three-tier circuit breaker:
//
//   - L1 watch:   count >= watch threshold    -> structured log only (no scheduling impact)
//   - L2 warning: count >= warning threshold  -> structured log + ops alert
//   - L3 trip:    count >= failure threshold  -> SetTempUnschedulable + cooldown (existing behavior)
//
// Tier transitions are debounced in Redis: each tier escalates at most once per
// window, and the tier only ever rises within a window. A nil/disabled settings,
// an out-of-scope account, or an ineligible error makes this a no-op, identical to
// the prior behavior when the breaker was disabled.
func (s *RateLimitService) ObserveOpenAIAPIKeyHealthFailure(ctx context.Context, account *Account, upstreamErr error) bool {
	if s == nil || s.openAIAPIKeyHealth == nil || s.settingService == nil || s.accountRepo == nil || account == nil {
		return false
	}
	settings, err := s.settingService.GetOpenAIAPIKeyHealthBreakerSettings(ctx)
	if err != nil {
		logger.L().Warn("openai.apikey_health_breaker_settings_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		return false
	}
	if settings == nil || !settings.Enabled {
		return false
	}
	if !isOpenAIAPIKeyHealthBreakerAccount(account, settings) {
		return false
	}

	statusCode, responseBody, eligible := classifyOpenAIAPIKeyHealthFailure(upstreamErr)
	if !eligible {
		return false
	}

	watchThreshold, warningThreshold, tripThreshold := settings.computeHealthBreakerThresholds()
	result, err := s.openAIAPIKeyHealth.RecordOpenAIAPIKeyHealthFailure(ctx, account.ID, settings.WindowMinutes, watchThreshold, warningThreshold, tripThreshold)
	if err != nil {
		logger.L().Warn("openai.apikey_health_breaker_record_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		return false
	}

	// L1 watch: informational log only, does not touch scheduling.
	if result.TrippedWatch {
		logger.L().Info("openai.apikey_health_watch",
			zap.Int64("account_id", account.ID),
			zap.Int64("failure_count", result.Count),
			zap.Int("watch_threshold", watchThreshold),
			zap.Int("window_minutes", settings.WindowMinutes),
			zap.Int("upstream_status", statusCode),
		)
	}

	// L2 warning: log + a queryable ops alert. Skipped when this same record also
	// trips (the trip is the terminal, more severe action).
	if result.TrippedWarning && !result.TrippedTrip {
		logger.L().Warn("openai.apikey_health_warning",
			zap.Int64("account_id", account.ID),
			zap.Int64("failure_count", result.Count),
			zap.Int("warning_threshold", warningThreshold),
			zap.Int("window_minutes", settings.WindowMinutes),
			zap.Int("upstream_status", statusCode),
		)
		s.recordHealthWarningAlert(ctx, account, statusCode, result.Count, warningThreshold, settings.WindowMinutes)
	}

	// L3 trip: open the circuit (existing behavior preserved).
	if result.TrippedTrip {
		return s.tripAccountForHealth(ctx, account, statusCode, responseBody, result.Count, settings)
	}
	return false
}

// tripAccountForHealth applies the L3 trip: persist temp-unschedulable, block
// scheduling in-process, and keep the cache in sync. It mirrors the pre-tiering
// behavior exactly for the trip path.
func (s *RateLimitService) tripAccountForHealth(ctx context.Context, account *Account, statusCode int, responseBody []byte, count int64, settings *OpenAIAPIKeyHealthBreakerSettings) bool {
	now := time.Now()
	until := now.Add(time.Duration(settings.CooldownMinutes) * time.Minute)
	state := &TempUnschedState{
		UntilUnix:            until.Unix(),
		TriggeredAtUnix:      now.Unix(),
		StatusCode:           statusCode,
		MatchedKeyword:       openAIAPIKeyHealthBreakerReason,
		RuleIndex:            -1,
		ErrorMessage:         truncateTempUnschedMessage(responseBody, tempUnschedMessageMaxBytes),
		TriggerCount:         count,
		TriggerThreshold:     settings.FailureThreshold,
		TriggerWindowMinutes: settings.WindowMinutes,
		Tier:                 HealthBreakerTierTrip,
	}
	reasonBytes, _ := json.Marshal(state)
	reason := string(reasonBytes)
	if reason == "" {
		reason = fmt.Sprintf("%s: %d failures in %d minute(s)", openAIAPIKeyHealthBreakerReason, count, settings.WindowMinutes)
	}

	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := s.accountRepo.SetTempUnschedulable(persistCtx, account.ID, until, reason); err != nil {
		logger.L().Warn("openai.apikey_health_breaker_persist_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		return false
	}

	if account.TempUnschedulableUntil == nil || account.TempUnschedulableUntil.Before(until) {
		account.TempUnschedulableUntil = &until
		account.TempUnschedulableReason = reason
	}
	s.notifyAccountSchedulingBlocked(account, until, openAIAPIKeyHealthBreakerReason)
	if s.tempUnschedCache != nil {
		if err := s.tempUnschedCache.SetTempUnsched(persistCtx, account.ID, state); err != nil {
			logger.L().Warn("openai.apikey_health_breaker_cache_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		}
	}
	logger.L().Warn("openai.apikey_health_breaker_tripped",
		zap.Int64("account_id", account.ID),
		zap.Int64("failure_count", count),
		zap.Int("failure_threshold", settings.FailureThreshold),
		zap.Int("window_minutes", settings.WindowMinutes),
		zap.Int("cooldown_minutes", settings.CooldownMinutes),
		zap.Int("upstream_status", statusCode),
		zap.Time("until", until),
	)
	return true
}

// recordHealthWarningAlert writes a queryable ops alert event for the L2 warning so
// the warning is not only a log line. It is best-effort: a missing ops repository or
// a monitoring-disabled backend does not fail the breaker.
func (s *RateLimitService) recordHealthWarningAlert(ctx context.Context, account *Account, statusCode int, count int64, warningThreshold, windowMinutes int) {
	if s == nil || s.opsRepo == nil {
		return
	}
	metric := float64(count)
	threshold := float64(warningThreshold)
	event := &OpsAlertEvent{
		Severity:       "P1",
		Status:         OpsAlertStatusFiring,
		Title:          "账号健康熔断预警 (L2 warning)",
		Description:    fmt.Sprintf("account %d reached the health-breaker warning tier: %d failures within %d min (warning threshold %d)", account.ID, count, windowMinutes, warningThreshold),
		MetricValue:    &metric,
		ThresholdValue: &threshold,
		Dimensions:     map[string]any{"account_id": account.ID, "platform": account.Platform, "reason": openAIAPIKeyHealthBreakerReason},
		FiredAt:        time.Now(),
	}
	if _, err := s.opsRepo.CreateAlertEvent(ctx, event); err != nil {
		logger.L().Warn("openai.apikey_health_warning_alert_failed", zap.Int64("account_id", account.ID), zap.Error(err))
	}
}

// ObserveOpenAIAPIKeyHealthSuccess clears the rolling health window and tier state
// when an account reports a successful schedule result. A healthy account must not
// stay one failure away from tripping; success resets the slate. The reset is
// guarded by the breaker being enabled and the account being in scope so it does
// not add a Redis round-trip to the hot path for out-of-scope or disabled traffic.
func (s *RateLimitService) ObserveOpenAIAPIKeyHealthSuccess(ctx context.Context, account *Account) {
	if s == nil || s.openAIAPIKeyHealth == nil || account == nil {
		return
	}
	if s.settingService == nil {
		return
	}
	settings, err := s.settingService.GetOpenAIAPIKeyHealthBreakerSettings(ctx)
	if err != nil || settings == nil || !settings.Enabled {
		return
	}
	if !isOpenAIAPIKeyHealthBreakerAccount(account, settings) {
		return
	}
	if err := s.openAIAPIKeyHealth.ClearOpenAIAPIKeyHealth(ctx, account.ID); err != nil {
		logger.L().Debug("openai.apikey_health_clear_failed", zap.Int64("account_id", account.ID), zap.Error(err))
	}
}
