package service

import (
	"context"
	"net/http"
	"testing"
	"time"
)

type accountUsageCodexProbeRepo struct {
	stubOpenAIAccountRepo
	updateExtraCh    chan map[string]any
	rateLimitCh      chan time.Time
	clearRateLimitCh chan clearRateLimitCall
}

type clearRateLimitCall struct {
	id                int64
	observedLimitedAt time.Time
	observedResetAt   time.Time
}

func (r *accountUsageCodexProbeRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	if r.updateExtraCh != nil {
		copied := make(map[string]any, len(updates))
		for k, v := range updates {
			copied[k] = v
		}
		r.updateExtraCh <- copied
	}
	return nil
}

func (r *accountUsageCodexProbeRepo) SetRateLimited(_ context.Context, _ int64, resetAt time.Time) error {
	if r.rateLimitCh != nil {
		r.rateLimitCh <- resetAt
	}
	return nil
}

func (r *accountUsageCodexProbeRepo) ClearOpenAIRateLimitIfObserved(_ context.Context, id int64, observedLimitedAt, observedResetAt time.Time) (bool, error) {
	if r.clearRateLimitCh != nil {
		r.clearRateLimitCh <- clearRateLimitCall{id: id, observedLimitedAt: observedLimitedAt, observedResetAt: observedResetAt}
	}
	return true, nil
}

func TestShouldRefreshOpenAICodexSnapshot(t *testing.T) {
	t.Parallel()

	rateLimitedUntil := time.Now().Add(5 * time.Minute)
	now := time.Now()
	usage := &UsageInfo{
		FiveHour: &UsageProgress{Utilization: 0},
		SevenDay: &UsageProgress{Utilization: 0},
	}

	if !shouldRefreshOpenAICodexSnapshot(&Account{RateLimitResetAt: &rateLimitedUntil}, usage, now) {
		t.Fatal("expected rate-limited account to force codex snapshot refresh")
	}

	if shouldRefreshOpenAICodexSnapshot(&Account{}, usage, now) {
		t.Fatal("expected complete non-rate-limited usage to skip codex snapshot refresh")
	}

	if !shouldRefreshOpenAICodexSnapshot(&Account{}, &UsageInfo{FiveHour: nil, SevenDay: &UsageProgress{}}, now) {
		t.Fatal("expected missing 5h snapshot to require refresh")
	}

	staleAt := now.Add(-(openAIProbeCacheTTL + time.Minute)).Format(time.RFC3339)
	if !shouldRefreshOpenAICodexSnapshot(&Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"openai_oauth_responses_websockets_v2_enabled": true,
			"codex_usage_updated_at":                       staleAt,
		},
	}, usage, now) {
		t.Fatal("expected stale ws snapshot to trigger refresh")
	}
}

// TestShouldRefreshOpenAICodexSnapshot_SparkShadowIgnoresWSv2 外审第9轮 P1:spark 影子用量走
// QueryUsage(/wham/usage,与 WSv2 无关),staleness 不得被 WSv2 门控,否则首刷后窗口永久冻结。
func TestShouldRefreshOpenAICodexSnapshot_SparkShadowIgnoresWSv2(t *testing.T) {
	t.Parallel()

	now := time.Now()
	usage := &UsageInfo{
		FiveHour: &UsageProgress{Utilization: 0},
		SevenDay: &UsageProgress{Utilization: 0},
	}
	staleAt := now.Add(-(openAIProbeCacheTTL + time.Minute)).Format(time.RFC3339)
	freshAt := now.Add(-time.Minute).Format(time.RFC3339)
	parentID := int64(7001)

	// 影子无 WSv2,但首刷后窗口已存在;过期 codex_usage_updated_at 必须触发再刷新。
	shadowStale := &Account{
		Platform:        PlatformOpenAI,
		Type:            AccountTypeOAuth,
		ParentAccountID: &parentID,
		QuotaDimension:  QuotaDimensionSpark,
		Extra:           map[string]any{"codex_usage_updated_at": staleAt},
	}
	if !shouldRefreshOpenAICodexSnapshot(shadowStale, usage, now) {
		t.Fatal("expected stale spark shadow (no WSv2) to trigger refresh")
	}

	// 影子时间戳仍新鲜→不刷(TTL 生效)。
	shadowFresh := &Account{
		Platform:        PlatformOpenAI,
		Type:            AccountTypeOAuth,
		ParentAccountID: &parentID,
		QuotaDimension:  QuotaDimensionSpark,
		Extra:           map[string]any{"codex_usage_updated_at": freshAt},
	}
	if shouldRefreshOpenAICodexSnapshot(shadowFresh, usage, now) {
		t.Fatal("expected fresh spark shadow to skip refresh (TTL not elapsed)")
	}

	// 反向对照:普通账号无 WSv2 + 过期时间戳→仍不刷(WSv2 门控普通账号的 probe 刷新)。
	normalNoWS := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{"codex_usage_updated_at": staleAt},
	}
	if shouldRefreshOpenAICodexSnapshot(normalNoWS, usage, now) {
		t.Fatal("expected non-WSv2 normal account to skip codex probe refresh")
	}
}

func TestExtractOpenAICodexProbeUpdatesAccepts429WithCodexHeaders(t *testing.T) {
	t.Parallel()

	headers := make(http.Header)
	headers.Set("x-codex-primary-used-percent", "100")
	headers.Set("x-codex-primary-reset-after-seconds", "604800")
	headers.Set("x-codex-primary-window-minutes", "10080")
	headers.Set("x-codex-secondary-used-percent", "100")
	headers.Set("x-codex-secondary-reset-after-seconds", "18000")
	headers.Set("x-codex-secondary-window-minutes", "300")

	updates, err := extractOpenAICodexProbeUpdates(&http.Response{StatusCode: http.StatusTooManyRequests, Header: headers})
	if err != nil {
		t.Fatalf("extractOpenAICodexProbeUpdates() error = %v", err)
	}
	if len(updates) == 0 {
		t.Fatal("expected codex probe updates from 429 headers")
	}
	if got := updates["codex_5h_used_percent"]; got != 100.0 {
		t.Fatalf("codex_5h_used_percent = %v, want 100", got)
	}
	if got := updates["codex_7d_used_percent"]; got != 100.0 {
		t.Fatalf("codex_7d_used_percent = %v, want 100", got)
	}
}

func TestAccountUsageService_PersistOpenAICodexProbeSnapshotOnlyUpdatesExtra(t *testing.T) {
	t.Parallel()

	repo := &accountUsageCodexProbeRepo{
		updateExtraCh: make(chan map[string]any, 1),
		rateLimitCh:   make(chan time.Time, 1),
	}
	svc := &AccountUsageService{accountRepo: repo}
	svc.persistOpenAICodexProbeSnapshot(321, map[string]any{
		"codex_7d_used_percent": 100.0,
		"codex_7d_reset_at":     time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339),
	})

	select {
	case updates := <-repo.updateExtraCh:
		if got := updates["codex_7d_used_percent"]; got != 100.0 {
			t.Fatalf("codex_7d_used_percent = %v, want 100", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("等待 codex 探测快照写入 extra 超时")
	}

	select {
	case got := <-repo.rateLimitCh:
		t.Fatalf("不应将探测快照写入运行时限流状态: %v", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestAccountUsageService_GetOpenAIUsage_DoesNotPromoteCodexExtraToRateLimit(t *testing.T) {
	t.Parallel()

	resetAt := time.Now().Add(6 * 24 * time.Hour).UTC().Truncate(time.Second)
	repo := &accountUsageCodexProbeRepo{
		rateLimitCh: make(chan time.Time, 1),
	}
	svc := &AccountUsageService{accountRepo: repo}
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"codex_5h_used_percent": 1.0,
			"codex_5h_reset_at":     time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339),
			"codex_7d_used_percent": 100.0,
			"codex_7d_reset_at":     resetAt.Format(time.RFC3339),
		},
	}

	usage, err := svc.getOpenAIUsage(context.Background(), account, false)
	if err != nil {
		t.Fatalf("getOpenAIUsage() error = %v", err)
	}
	if usage.SevenDay == nil || usage.SevenDay.Utilization != 100.0 {
		t.Fatalf("预期 7 天用量仍然可见，实际为 %#v", usage.SevenDay)
	}
	if account.RateLimitResetAt != nil {
		t.Fatalf("不应让已耗尽的 codex extra 改写运行时限流状态: %v", account.RateLimitResetAt)
	}
	select {
	case got := <-repo.rateLimitCh:
		t.Fatalf("不应将已耗尽的 codex extra 持久化为运行时限流状态: %v", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestBuildCodexUsageProgressFromExtra_ZerosExpiredWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)

	t.Run("expired 5h window zeroes utilization", func(t *testing.T) {
		extra := map[string]any{
			"codex_5h_used_percent": 42.0,
			"codex_5h_reset_at":     "2026-03-16T10:00:00Z", // 2h ago
		}
		progress := buildCodexUsageProgressFromExtra(extra, "5h", now)
		if progress == nil {
			t.Fatal("expected non-nil progress")
		}
		if progress.Utilization != 0 {
			t.Fatalf("expected Utilization=0 for expired window, got %v", progress.Utilization)
		}
		if progress.RemainingSeconds != 0 {
			t.Fatalf("expected RemainingSeconds=0, got %v", progress.RemainingSeconds)
		}
	})

	t.Run("active 5h window keeps utilization", func(t *testing.T) {
		resetAt := now.Add(2 * time.Hour).Format(time.RFC3339)
		extra := map[string]any{
			"codex_5h_used_percent": 42.0,
			"codex_5h_reset_at":     resetAt,
		}
		progress := buildCodexUsageProgressFromExtra(extra, "5h", now)
		if progress == nil {
			t.Fatal("expected non-nil progress")
		}
		if progress.Utilization != 42.0 {
			t.Fatalf("expected Utilization=42, got %v", progress.Utilization)
		}
	})

	t.Run("expired 7d window zeroes utilization", func(t *testing.T) {
		extra := map[string]any{
			"codex_7d_used_percent": 88.0,
			"codex_7d_reset_at":     "2026-03-15T00:00:00Z", // yesterday
		}
		progress := buildCodexUsageProgressFromExtra(extra, "7d", now)
		if progress == nil {
			t.Fatal("expected non-nil progress")
		}
		if progress.Utilization != 0 {
			t.Fatalf("expected Utilization=0 for expired 7d window, got %v", progress.Utilization)
		}
	})
}

func TestOpenAICodexProbeConfirmsQuotaAvailable(t *testing.T) {
	t.Parallel()

	t.Run("both windows explicit and below 100 confirms", func(t *testing.T) {
		updates := map[string]any{
			"codex_5h_used_percent": 0.0,
			"codex_7d_used_percent": 0.0,
		}
		if !openAICodexProbeConfirmsQuotaAvailable(updates) {
			t.Fatal("expected 5h=0%,7d=0% to confirm quota available")
		}
	})

	t.Run("one window missing does not confirm", func(t *testing.T) {
		updates := map[string]any{"codex_5h_used_percent": 0.0}
		if openAICodexProbeConfirmsQuotaAvailable(updates) {
			t.Fatal("missing 7d used_percent must not confirm")
		}
	})

	t.Run("one window exhausted does not confirm", func(t *testing.T) {
		updates := map[string]any{
			"codex_5h_used_percent": 100.0,
			"codex_7d_used_percent": 0.0,
		}
		if openAICodexProbeConfirmsQuotaAvailable(updates) {
			t.Fatal("5h=100% must not confirm")
		}
	})

	t.Run("one window at or above 100 does not confirm", func(t *testing.T) {
		updates := map[string]any{
			"codex_5h_used_percent": 0.0,
			"codex_7d_used_percent": 100.0,
		}
		if openAICodexProbeConfirmsQuotaAvailable(updates) {
			t.Fatal("7d=100% must not confirm")
		}
	})

	t.Run("empty updates does not confirm", func(t *testing.T) {
		if openAICodexProbeConfirmsQuotaAvailable(nil) {
			t.Fatal("empty updates must not confirm")
		}
	})
}

func TestClearOpenAIRateLimitAfterQuotaRecovery(t *testing.T) {
	t.Parallel()

	limitedAt := time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Second)
	resetAt := time.Now().Add(3 * 24 * time.Hour).UTC().Truncate(time.Second)
	available := map[string]any{
		"codex_5h_used_percent": 0.0,
		"codex_7d_used_percent": 0.0,
	}

	t.Run("fresh 0%/0% success clears the observed generation", func(t *testing.T) {
		repo := &accountUsageCodexProbeRepo{clearRateLimitCh: make(chan clearRateLimitCall, 1)}
		svc := &AccountUsageService{accountRepo: repo}
		svc.clearOpenAIRateLimitAfterQuotaRecovery(244, &limitedAt, &resetAt, available)

		select {
		case call := <-repo.clearRateLimitCh:
			if call.id != 244 {
				t.Fatalf("cleared wrong account id: %d", call.id)
			}
			if !call.observedLimitedAt.Equal(limitedAt) {
				t.Fatalf("observed limitedAt mismatch: %v != %v", call.observedLimitedAt, limitedAt)
			}
			if !call.observedResetAt.Equal(resetAt) {
				t.Fatalf("observed resetAt mismatch: %v != %v", call.observedResetAt, resetAt)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("expected CAS clear to be issued")
		}
	})

	t.Run("one window exhausted does not clear", func(t *testing.T) {
		repo := &accountUsageCodexProbeRepo{clearRateLimitCh: make(chan clearRateLimitCall, 1)}
		svc := &AccountUsageService{accountRepo: repo}
		svc.clearOpenAIRateLimitAfterQuotaRecovery(244, &limitedAt, &resetAt, map[string]any{
			"codex_5h_used_percent": 0.0,
			"codex_7d_used_percent": 100.0,
		})

		select {
		case call := <-repo.clearRateLimitCh:
			t.Fatalf("7d=100%% must not issue a clear: %+v", call)
		case <-time.After(200 * time.Millisecond):
		}
	})

	t.Run("missing window signal does not clear", func(t *testing.T) {
		repo := &accountUsageCodexProbeRepo{clearRateLimitCh: make(chan clearRateLimitCall, 1)}
		svc := &AccountUsageService{accountRepo: repo}
		svc.clearOpenAIRateLimitAfterQuotaRecovery(244, &limitedAt, &resetAt, map[string]any{
			"codex_5h_used_percent": 0.0,
		})

		select {
		case call := <-repo.clearRateLimitCh:
			t.Fatalf("missing 7d signal must not issue a clear: %+v", call)
		case <-time.After(200 * time.Millisecond):
		}
	})

	t.Run("no observed rate-limit generation does not clear", func(t *testing.T) {
		repo := &accountUsageCodexProbeRepo{clearRateLimitCh: make(chan clearRateLimitCall, 1)}
		svc := &AccountUsageService{accountRepo: repo}
		svc.clearOpenAIRateLimitAfterQuotaRecovery(244, nil, nil, available)

		select {
		case call := <-repo.clearRateLimitCh:
			t.Fatalf("no observed generation must not issue a clear: %+v", call)
		case <-time.After(200 * time.Millisecond):
		}
	})
}

func TestClearOpenAIRateLimitAfterQuotaRecovery_StaleProbeBindsPreProbeGeneration(t *testing.T) {
	t.Parallel()

	// 探测开始前读到代际 A；探测期间新 429 写入代际 B（RateLimitedAt=now 更新）。
	// helper 绑定代际 A 的 observed 值提交 CAS，仓库层对 B 失配 → 零写入、保留新限流。
	limitedA := time.Now().Add(-40 * time.Minute).UTC().Truncate(time.Second)
	resetA := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)

	repo := &accountUsageCodexProbeRepo{clearRateLimitCh: make(chan clearRateLimitCall, 1)}
	svc := &AccountUsageService{accountRepo: repo}
	svc.clearOpenAIRateLimitAfterQuotaRecovery(244, &limitedA, &resetA, map[string]any{
		"codex_5h_used_percent": 0.0,
		"codex_7d_used_percent": 0.0,
	})

	select {
	case call := <-repo.clearRateLimitCh:
		// 桩仓库恒返回 true；真实仓库会因代际失配返回 false 且不落盘。这里断言
		// 提交的正是代际 A 的 observed 值，说明 CAS 绑定正确。
		if !call.observedLimitedAt.Equal(limitedA) || !call.observedResetAt.Equal(resetA) {
			t.Fatalf("CAS must bind the pre-probe generation A: %+v", call)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected CAS clear to be issued with generation A")
	}
}
