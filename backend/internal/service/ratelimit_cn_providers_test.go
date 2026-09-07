package service

import (
	"context"
	"testing"
	"time"
)

func TestCNProviderResponseIndicatesOverload(t *testing.T) {
	overload := []byte(`{"error":{"message":"The service is currently unable to handle additional requests due to server overload. Please retry later. Request id: 02178876"}}`)
	if !cnProviderResponseIndicatesOverload(overload) {
		t.Fatal("volcano overload 429 body should be detected as overload")
	}
	if cnProviderResponseIndicatesOverload([]byte(`{"error":{"message":"FlowThreshold exceeded: weekly token quota exhausted"}}`)) {
		t.Fatal("quota exhaustion body must not be classified as overload")
	}
	if cnProviderResponseIndicatesOverload(nil) {
		t.Fatal("empty body should not be classified as overload")
	}
}

func TestCNProviderResponseIndicatesQuotaExhaustion(t *testing.T) {
	quota := []byte(`{"error":{"message":"FlowThreshold exceeded: weekly token quota exhausted"}}`)
	if !cnProviderResponseIndicatesQuotaExhaustion(quota) {
		t.Fatal("quota exhaustion body should be detected")
	}
	overload := []byte(`{"error":{"message":"The service is currently unable to handle additional requests due to server overload. Please retry later."}}`)
	if cnProviderResponseIndicatesQuotaExhaustion(overload) {
		t.Fatal("overload body must not be classified as quota exhaustion")
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

func cnVolcanoTestAccount(id int64, resetAt time.Time) Account {
	a := Account{ID: id, Platform: PlatformZhipu, Type: AccountTypeAPIKey, RateLimitedAt: &resetAt, RateLimitResetAt: &resetAt}
	a.Credentials = map[string]any{"base_url": "https://ark.cn-beijing.volces.com/api/v3"}
	return a
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
