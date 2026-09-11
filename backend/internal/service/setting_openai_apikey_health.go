package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

const openAIAPIKeyHealthBreakerSettingsCacheTTL = 30 * time.Second

type cachedOpenAIAPIKeyHealthBreakerSettings struct {
	settings  OpenAIAPIKeyHealthBreakerSettings
	expiresAt time.Time
}

const (
	openAIAPIKeyHealthWatchRatioMin    = 0.01
	openAIAPIKeyHealthWatchRatioMax    = 0.99
	openAIAPIKeyHealthWarningRatioMin   = 0.01
	openAIAPIKeyHealthWarningRatioMax   = 0.99
	openAIAPIKeyHealthProbeIntervalMin  = 30
	openAIAPIKeyHealthProbeIntervalMax  = 600
	openAIAPIKeyHealthProbeAttemptsMin  = 1
	openAIAPIKeyHealthProbeAttemptsMax  = 100
)

// normalizeOpenAIAPIKeyHealthBreakerSettings fills in missing fields with defaults
// and clamps every numeric bound to the documented safe range. It is shared by the
// GET (cache) path and the PUT (validation) path so stored and in-memory settings
// always agree.
func normalizeOpenAIAPIKeyHealthBreakerSettings(settings *OpenAIAPIKeyHealthBreakerSettings) *OpenAIAPIKeyHealthBreakerSettings {
	if settings == nil {
		return DefaultOpenAIAPIKeyHealthBreakerSettings()
	}
	result := *settings

	if result.WindowMinutes < 1 {
		result.WindowMinutes = 1
	} else if result.WindowMinutes > 60 {
		result.WindowMinutes = 60
	}
	if result.FailureThreshold < 1 {
		result.FailureThreshold = 1
	} else if result.FailureThreshold > 10000 {
		result.FailureThreshold = 10000
	}
	if result.CooldownMinutes < 1 {
		result.CooldownMinutes = 1
	} else if result.CooldownMinutes > 60 {
		result.CooldownMinutes = 60
	}

	// Scope defaults to the six OpenAI-compatible platforms when unset.
	if len(result.ScopePlatforms) == 0 {
		result.ScopePlatforms = append([]string(nil), DefaultAccountHealthBreakerScopePlatforms...)
	} else {
		// Deduplicate and drop empty entries; keep order for stable admin display.
		seen := make(map[string]struct{}, len(result.ScopePlatforms))
		deduped := result.ScopePlatforms[:0]
		for _, p := range result.ScopePlatforms {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			deduped = append(deduped, p)
		}
		result.ScopePlatforms = deduped
	}

	// Ratios must stay strictly ordered: 0 < watch_ratio < warning_ratio < 1.
	// A zero value means "unset" (e.g. an older stored document without these
	// fields) and falls back to the canonical defaults so old config keeps working.
	if result.WatchRatio <= 0 {
		result.WatchRatio = 0.4
	}
	if result.WarningRatio <= 0 {
		result.WarningRatio = 0.7
	}
	result.WatchRatio = clampFloat(result.WatchRatio, openAIAPIKeyHealthWatchRatioMin, openAIAPIKeyHealthWatchRatioMax)
	result.WarningRatio = clampFloat(result.WarningRatio, openAIAPIKeyHealthWarningRatioMin, openAIAPIKeyHealthWarningRatioMax)
	if result.WarningRatio <= result.WatchRatio {
		result.WarningRatio = math.Min(openAIAPIKeyHealthWarningRatioMax, result.WatchRatio+0.1)
		if result.WarningRatio <= result.WatchRatio {
			result.WarningRatio = math.Min(openAIAPIKeyHealthWarningRatioMax, result.WatchRatio+0.05)
		}
	}

	// Probe defaults when nil; clamp its bounds.
	if result.Probe == nil {
		result.Probe = &OpenAIAPIKeyHealthBreakerProbeSettings{}
	}
	if result.Probe.IntervalSeconds < openAIAPIKeyHealthProbeIntervalMin {
		result.Probe.IntervalSeconds = openAIAPIKeyHealthProbeIntervalMin
	} else if result.Probe.IntervalSeconds > openAIAPIKeyHealthProbeIntervalMax {
		result.Probe.IntervalSeconds = openAIAPIKeyHealthProbeIntervalMax
	}
	if result.Probe.MaxAttempts < openAIAPIKeyHealthProbeAttemptsMin {
		result.Probe.MaxAttempts = openAIAPIKeyHealthProbeAttemptsMin
	} else if result.Probe.MaxAttempts > openAIAPIKeyHealthProbeAttemptsMax {
		result.Probe.MaxAttempts = openAIAPIKeyHealthProbeAttemptsMax
	}

	return &result
}

func clampFloat(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func (s *SettingService) GetOpenAIAPIKeyHealthBreakerSettings(ctx context.Context) (*OpenAIAPIKeyHealthBreakerSettings, error) {
	if s == nil || s.settingRepo == nil {
		return DefaultOpenAIAPIKeyHealthBreakerSettings(), nil
	}
	if cached, ok := s.openAIAPIKeyHealthBreakerCache.Load().(*cachedOpenAIAPIKeyHealthBreakerSettings); ok && cached != nil && time.Now().Before(cached.expiresAt) {
		result := cached.settings
		return &result, nil
	}

	settings := DefaultOpenAIAPIKeyHealthBreakerSettings()
	value, err := s.settingRepo.GetValue(ctx, SettingKeyOpenAIAPIKeyHealthBreakerSettings)
	if err != nil && !errors.Is(err, ErrSettingNotFound) {
		return nil, fmt.Errorf("get OpenAI API key health breaker settings: %w", err)
	}
	if err == nil && strings.TrimSpace(value) != "" {
		var stored OpenAIAPIKeyHealthBreakerSettings
		if json.Unmarshal([]byte(value), &stored) == nil {
			settings = normalizeOpenAIAPIKeyHealthBreakerSettings(&stored)
		}
	}
	s.openAIAPIKeyHealthBreakerCache.Store(&cachedOpenAIAPIKeyHealthBreakerSettings{
		settings:  *settings,
		expiresAt: time.Now().Add(openAIAPIKeyHealthBreakerSettingsCacheTTL),
	})
	result := *settings
	return &result, nil
}

// SetOpenAIAPIKeyHealthBreakerSettings persists and normalizes the API-key health
// breaker configuration. It invalidates the 30s in-process cache so the next GET
// (and the next Observe call) sees the new values immediately.
func (s *SettingService) SetOpenAIAPIKeyHealthBreakerSettings(ctx context.Context, settings *OpenAIAPIKeyHealthBreakerSettings) error {
	if s == nil || s.settingRepo == nil {
		return fmt.Errorf("setting service not initialized")
	}
	if settings == nil {
		return fmt.Errorf("settings cannot be nil")
	}
	normalized := normalizeOpenAIAPIKeyHealthBreakerSettings(settings)
	data, err := json.Marshal(normalized)
	if err != nil {
		return fmt.Errorf("marshal OpenAI API key health breaker settings: %w", err)
	}
	if err := s.settingRepo.Set(ctx, SettingKeyOpenAIAPIKeyHealthBreakerSettings, string(data)); err != nil {
		return fmt.Errorf("persist OpenAI API key health breaker settings: %w", err)
	}
	// Invalidate the read cache so enabled/scope changes take effect at once.
	s.openAIAPIKeyHealthBreakerCache.Store((*cachedOpenAIAPIKeyHealthBreakerSettings)(nil))
	return nil
}
