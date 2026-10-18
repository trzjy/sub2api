package service

import (
	"sync"
	"sync/atomic"
	"unsafe"
)

// This file defines the concurrency-safe access boundary for Account.Extra.
//
// Account.Extra (map[string]any) was read and written concurrently from multiple
// goroutines (scheduler sweep, usage probes, recovery goroutines, admin config
// switches, per-platform snapshot merges, ...). That caused a production
// `fatal error: concurrent map read and map write` — an unrecoverable process
// crash.
//
// The mutex (extraMu on Account) is stored as a *sync.RWMutex held in an
// unsafe.Pointer so Account stays safely copyable by value (the codebase passes
// Account by value in many places; see the struct doc comment). Value copies of
// an Account share the same underlying Extra map and the same mutex, so the lock
// still protects the map correctly. The mutex is lazily initialized on first
// access via a lock-free atomic compare-and-swap, so freshly constructed
// Accounts need no explicit setup and there is no init race.
//
// Write contract: MergeExtra / DeleteExtra are the ONLY permitted write entry
// points for Account.Extra from concurrent code paths. Direct
// `a.Extra[key] = v`, `delete(a.Extra, key)` and `a.Extra = make(...)` are
// forbidden outside of the single-goroutine construction sites (ent
// deserialization into a brand new Account). Reads must use SnapshotExtra
// (whole-map, consistent point-in-time) or ExtraValue (single key, cheaper).

// extraLock returns the RWMutex guarding Extra, initializing it on first use.
// extraMu is an unsafe.Pointer storing a *sync.RWMutex; a nil value means the
// lock has not been created yet. The atomic CAS ensures exactly one mutex is
// installed even under concurrent first access.
func (a *Account) extraLock() *sync.RWMutex {
	if a == nil {
		return nil
	}
	p := (*sync.RWMutex)(atomic.LoadPointer(&a.extraMu))
	if p != nil {
		return p
	}
	p = &sync.RWMutex{}
	if atomic.CompareAndSwapPointer(&a.extraMu, nil, unsafe.Pointer(p)) {
		return p
	}
	// Lost the race: another goroutine installed the lock. Use that one.
	return (*sync.RWMutex)(atomic.LoadPointer(&a.extraMu))
}

// SnapshotExtra returns a shallow copy of Account.Extra taken under RLock.
//
// The returned map is a consistent point-in-time snapshot: subsequent mutations
// to the underlying Extra map do not affect the snapshot. A nil Extra map yields
// a nil result. Callers on a hot read path (e.g. the quota lifecycle sweep) may
// call this once per account and reuse the snapshot within the function instead
// of copying repeatedly.
func (a *Account) SnapshotExtra() map[string]any {
	if a == nil {
		return nil
	}
	mu := a.extraLock()
	mu.RLock()
	defer mu.RUnlock()
	if a.Extra == nil {
		return nil
	}
	out := make(map[string]any, len(a.Extra))
	for k, v := range a.Extra {
		out[k] = v
	}
	return out
}

// MergeExtra merges updates into Account.Extra under Lock.
//
// A nil Extra map is initialized. This is the ONLY allowed write entry point for
// Account.Extra from concurrent code paths; it preserves per-key semantics: each
// key in updates overwrites the corresponding key in Extra. (A key is removed
// via DeleteExtra, not via MergeExtra.)
func (a *Account) MergeExtra(updates map[string]any) {
	if a == nil || len(updates) == 0 {
		return
	}
	mu := a.extraLock()
	mu.Lock()
	defer mu.Unlock()
	if a.Extra == nil {
		a.Extra = make(map[string]any, len(updates))
	}
	for k, v := range updates {
		a.Extra[k] = v
	}
}

// DeleteExtra removes the given keys from Account.Extra under Lock.
//
// Provided as an explicit delete entry point so call sites that previously did
// `delete(a.Extra, key)` keep the exact same removal semantics without poking
// the map directly.
func (a *Account) DeleteExtra(keys ...string) {
	if a == nil || len(keys) == 0 {
		return
	}
	mu := a.extraLock()
	mu.Lock()
	defer mu.Unlock()
	if a.Extra == nil {
		return
	}
	for _, k := range keys {
		delete(a.Extra, k)
	}
}

// ExtraValue returns a single key's value under RLock, avoiding a full-map copy
// for one-key reads.
func (a *Account) ExtraValue(key string) (any, bool) {
	if a == nil {
		return nil, false
	}
	mu := a.extraLock()
	mu.RLock()
	defer mu.RUnlock()
	v, ok := a.Extra[key]
	return v, ok
}

// MutateExtra runs fn with exclusive access to the underlying Extra map.
//
// Use it for read-modify-write sequences whose body both reads and writes (or
// deletes) keys — e.g. the admin quota-reset helpers (ComputeQuotaResetAt /
// NormalizeFixedQuotaWindows) that delete keys as a side effect. fn receives the
// live map; the caller must not retain it or use it after fn returns. This keeps
// the mutation atomic with respect to the mutex, exactly mirroring the previous
// direct `account.Extra[...]` mutation but without the data race.
func (a *Account) MutateExtra(fn func(extra map[string]any)) {
	if a == nil || fn == nil {
		return
	}
	mu := a.extraLock()
	mu.Lock()
	defer mu.Unlock()
	if a.Extra == nil {
		a.Extra = make(map[string]any)
	}
	fn(a.Extra)
}
