package service

import (
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
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

// grokObservedModelsRepoStub is a minimal AccountRepository stand-in. The
// production path exercised here is the early-return branch of
// syncGrokObservedModels (the account already carries a fresh grok_observed_models
// snapshot), so no repository method is actually invoked. The stub exists solely
// to satisfy the non-nil guard in scheduleGrokObservedModelsSync.
type grokObservedModelsRepoStub struct {
	AccountRepository
}

// TestGrokObservedModelsSyncSharedExtraLock drives the PRODUCTION entry point
// scheduleGrokObservedModelsSync — the real background copy path for RACE-R3 —
// instead of re-implementing the value copy by hand (which is exactly the
// weakness flagged in the regression: the old test never went through the
// production copy site, so deleting the fix line left it green and it proved
// nothing).
//
// RACE-R3 root cause: scheduleGrokObservedModelsSync value-copies a *freshly
// loaded* Account (extraMu still nil, because Extra was never touched) and hands
// the copy to a background goroutine that reads Extra via SnapshotExtra. If the
// source lock is not forced into place BEFORE the copy, the source and the copy
// each CAS-initialize their own *sync.RWMutex on first Extra access, yielding two
// locks guarding the SAME underlying Extra map -> concurrent map read/write race
// (fatal). The fix (grok_observed_models.go:46, `_ = account.extraLock()`) forces
// the source lock before the copy so the copy carries the same mutex pointer and
// both share one lock.
//
// This test reproduces the fresh-account scenario faithfully:
//   - For each iteration it builds a brand-new Account whose extraMu is still nil
//     (no MergeExtra/SnapshotExtra has touched it yet) and pre-seeds a *fresh*
//     grok_observed_models snapshot so the background goroutine takes exactly one
//     SnapshotExtra on the shared map and then returns early (no real network).
//   - It then calls the production entry point scheduleGrokObservedModelsSync
//     FIRST (the background value-copy is taken at this instant, with a nil lock
//     under the unfixed code) and only afterwards hammers the SOURCE with
//     MergeExtra from another goroutine while the background goroutine reads the
//     shared map via SnapshotExtra.
//
// Mutation proof (verified by the experiment documented in the work report):
// deleting grok_observed_models.go:46 makes each background copy carry a nil lock,
// so the background goroutine lazily installs its OWN mutex while the source
// installs a different one — two locks over one map -> `go test -race` reports a
// DATA RACE. Restoring the line makes the copy share the source's lock and the
// run stays clean.
func TestGrokObservedModelsSyncSharedExtraLock(t *testing.T) {
	s := &GrokQuotaService{
		accountRepo: &grokObservedModelsRepoStub{},
		cfg:         &config.Config{},
	}

	const pairs = 400

	// Source-merge goroutines are tracked so we don't exit while they are still
	// writing the shared maps.
	var srcWg sync.WaitGroup

	for i := 0; i < pairs; i++ {
		src := &Account{
			ID:       int64(i + 1),
			Platform: PlatformGrok,
			Type:     AccountTypeOAuth,
			Extra: map[string]any{
				grokObservedModelsExtraKey: map[string]any{
					"models":     []any{"grok-1"},
					"fetched_at": time.Now().UTC().Format(time.RFC3339),
				},
			},
		}

		// CRITICAL ORDER: call the production entry point BEFORE any MergeExtra
		// on this source, so the background value-copy is taken while the
		// source's extraMu is still nil. Under the unfixed code this is exactly
		// the dangerous window; deleting grok_observed_models.go:46 makes the
		// copy carry a nil lock and the background goroutine lazily installs its
		// own mutex, disjoint from the source's.
		s.scheduleGrokObservedModelsSync(src)

		// Source path: concurrent writes (MergeExtra) including first access,
		// which lazily initializes the SOURCE lock.
		srcWg.Add(1)
		go func(a *Account) {
			defer srcWg.Done()
			for j := 0; j < 500; j++ {
				a.MergeExtra(map[string]any{
					grokObservedModelsExtraKey: map[string]any{
						"models":     []any{"grok-1", "grok-2"},
						"fetched_at": time.Now().UTC().Format(time.RFC3339),
					},
				})
			}
		}(src)
	}

	srcWg.Wait()

	// Wait for all background goroutines to finish their single SnapshotExtra on
	// the shared map. Each one clears its flight entry on exit, so an empty
	// flight map means every background copy has stopped touching memory.
	deadline := time.Now().Add(5 * time.Second)
	for {
		empty := true
		grokObservedModelsFlight.Range(func(_, _ any) bool {
			empty = false
			return false
		})
		if empty {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background grok observed-models goroutines did not finish in time")
		}
		time.Sleep(time.Millisecond)
	}
}
