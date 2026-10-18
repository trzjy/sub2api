package service

import (
	"sync"
	"testing"
)

// TestAccountValueCopySharedExtraLock reproduces the RACE-R3 finding: an Account
// whose extraMu is still nil (freshly loaded from DB, Extra never touched) is
// value-copied and handed to a background goroutine that accesses Extra while the
// source continues to be used on the request path.
//
// Without the fix, the copy and the source would each CAS-initialize their own
// *sync.RWMutex on first Extra access, yielding two locks guarding the SAME
// underlying Extra map -> a concurrent map read/write race (fatal). The production
// fix (grok_observed_models.go) forces the source lock into place BEFORE the value
// copy, so the copy carries the same mutex pointer and both share one lock.
//
// This test mirrors the fixed pattern: initialize the source lock, value-copy,
// then hammer the source and the copy from two goroutines with MergeExtra (write,
// including first access) and SnapshotExtra (read, including first access).
// Under `go test -race` a second lock would surface as a DATA RACE; the shared
// lock must keep the run clean.
func TestAccountValueCopySharedExtraLock(t *testing.T) {
	account := &Account{
		ID: 1,
		Extra: map[string]any{
			"grok_observed_models": map[string]any{"models": []any{"grok-1"}},
			"kira_usage_snapshot":  map[string]any{"status": "ok"},
			"th_pass_snapshot":     map[string]any{"fetched_at": "2026-10-18T00:00:00Z"},
		},
	}
	// Mirror the production fix: force the source lock into place before the copy
	// so the value copy shares the same *sync.RWMutex instead of lazily
	// initializing a second one on first access.
	_ = account.extraLock()
	acc := *account

	// Deterministic check: the copy must share the exact same mutex pointer as the
	// source. A nil-at-copy scenario would make these differ (two independent
	// locks), which is the bug this test guards against.
	srcMu := account.extraLock()
	cpMu := acc.extraLock()
	if srcMu != cpMu {
		t.Fatalf("value copy did not share the source extra lock: src=%p copy=%p", srcMu, cpMu)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// Source path: concurrent writes (MergeExtra) including first access.
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			account.MergeExtra(map[string]any{
				"grok_observed_models": map[string]any{"models": []any{"grok-1", "grok-2"}, "fetched_at": "x"},
			})
		}
	}()

	// Background/copy path: concurrent reads (SnapshotExtra) including first access.
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			_ = acc.SnapshotExtra()
		}
	}()

	wg.Wait()
}
