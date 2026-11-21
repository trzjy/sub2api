//go:build unit

package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// tickFakeLock 是 LeaderLockCache 的内存实现（单实例语义）。
type tickFakeLock struct {
	mu   sync.Mutex
	held map[string]string
}

func (f *tickFakeLock) TryAcquireLeaderLock(_ context.Context, key, owner string, _ time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.held == nil {
		f.held = map[string]string{}
	}
	if cur, ok := f.held[key]; ok && cur != owner {
		return false, nil
	}
	f.held[key] = owner
	return true, nil
}

func (f *tickFakeLock) ReleaseLeaderLock(_ context.Context, key, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.held[key] == owner {
		delete(f.held, key)
	}
	return nil
}

// tickDenyLock 永远拒绝获取（模拟争锁/后端不可用）。
type tickDenyLock struct{}

func (tickDenyLock) TryAcquireLeaderLock(context.Context, string, string, time.Duration) (bool, error) {
	return false, nil
}
func (tickDenyLock) ReleaseLeaderLock(context.Context, string, string) error { return nil }

func waitCond(t *testing.T, cond func() bool, d time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

func newTestRegistry(now func() time.Time) *BackgroundTickRegistry {
	r := NewBackgroundTickRegistry()
	if now != nil {
		r.SetNow(now)
	}
	return r
}

func TestBackgroundTickRegistryRegisterIdempotent(t *testing.T) {
	r := newTestRegistry(nil)
	task := backgroundTickTask{name: "a", cadence: time.Minute, maxRun: time.Second, run: func(context.Context) error { return nil }}
	r.Register(task)
	r.Register(task)
	if got := r.RegisteredCount(); got != 1 {
		t.Fatalf("RegisteredCount=%d want 1", got)
	}
}

func TestBackgroundTickRegistryFailClosedWithoutLeader(t *testing.T) {
	r := newTestRegistry(nil)
	var calls atomic.Int32
	r.Register(backgroundTickTask{name: "a", cadence: time.Minute, maxRun: time.Second,
		firstRun: tickFirstRunImmediate, run: func(context.Context) error { calls.Add(1); return nil }})
	r.DispatchDue(context.Background())
	time.Sleep(80 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatalf("run called %d times without leader backend; want 0 (fail closed)", calls.Load())
	}
}

func TestBackgroundTickRegistryImmediateFirstRun(t *testing.T) {
	lock := &tickFakeLock{}
	r := newTestRegistry(nil)
	r.SetLeader(lock, "owner-test")
	var calls atomic.Int32
	r.Register(backgroundTickTask{name: "a", cadence: time.Minute, maxRun: time.Second,
		firstRun: tickFirstRunImmediate, run: func(context.Context) error { calls.Add(1); return nil }})
	r.DispatchDue(context.Background())
	if !waitCond(t, func() bool { return calls.Load() == 1 }, time.Second) {
		t.Fatalf("immediate first run not executed; calls=%d", calls.Load())
	}
}

func TestBackgroundTickRegistryAfterCadence(t *testing.T) {
	lock := &tickFakeLock{}
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	var nowVal atomic.Value
	nowVal.Store(base)
	r := newTestRegistry(func() time.Time { return nowVal.Load().(time.Time) })
	r.SetLeader(lock, "owner-test")
	var calls atomic.Int32
	r.Register(backgroundTickTask{name: "a", cadence: time.Minute, maxRun: time.Second,
		firstRun: tickFirstRunAfterCadence, run: func(context.Context) error { calls.Add(1); return nil }})

	r.DispatchDue(context.Background())
	time.Sleep(60 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatalf("afterCadence ran too early: calls=%d", calls.Load())
	}
	// 推进过 cadence。
	nowVal.Store(base.Add(time.Minute))
	r.DispatchDue(context.Background())
	if !waitCond(t, func() bool { return calls.Load() == 1 }, time.Second) {
		t.Fatalf("afterCadence did not run once cadence elapsed; calls=%d", calls.Load())
	}
}

func TestBackgroundTickRegistryDispatchNonBlocking(t *testing.T) {
	lock := &tickFakeLock{}
	r := newTestRegistry(nil)
	r.SetLeader(lock, "owner-test")
	block := make(chan struct{})
	defer close(block)
	r.Register(backgroundTickTask{name: "a", cadence: time.Minute, maxRun: time.Second,
		firstRun: tickFirstRunImmediate, run: func(context.Context) error {
			<-block
			return nil
		}})
	start := time.Now()
	r.DispatchDue(context.Background())
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("DispatchDue blocked for %s; want non-blocking", elapsed)
	}
}

func TestBackgroundTickRegistrySingleRound(t *testing.T) {
	lock := &tickFakeLock{}
	r := newTestRegistry(nil)
	r.SetLeader(lock, "owner-test")
	block := make(chan struct{})
	var calls atomic.Int32
	r.Register(backgroundTickTask{name: "a", cadence: time.Minute, maxRun: time.Second,
		firstRun: tickFirstRunImmediate, run: func(context.Context) error { calls.Add(1); <-block; return nil }})
	r.DispatchDue(context.Background())
	if !waitCond(t, func() bool { return calls.Load() == 1 }, time.Second) {
		t.Fatal("first dispatch did not run")
	}
	// 上一轮仍在跑：再次派发不得重入。
	r.DispatchDue(context.Background())
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("concurrent round re-entered; calls=%d want 1", calls.Load())
	}
	close(block)
	time.Sleep(20 * time.Millisecond) // 让被阻塞的 goroutine 退出
}

func TestBackgroundTickRegistryUnregister(t *testing.T) {
	lock := &tickFakeLock{}
	r := newTestRegistry(nil)
	r.SetLeader(lock, "owner-test")
	var calls atomic.Int32
	r.Register(backgroundTickTask{name: "a", cadence: time.Minute, maxRun: time.Second,
		firstRun: tickFirstRunImmediate, run: func(context.Context) error { calls.Add(1); return nil }})
	r.Unregister("a")
	r.DispatchDue(context.Background())
	time.Sleep(60 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatalf("unregistered task ran: calls=%d", calls.Load())
	}
	if r.RegisteredCount() != 0 {
		t.Fatalf("RegisteredCount=%d want 0", r.RegisteredCount())
	}
}

func TestWithRecoveryAccountProbeLockSkipsWhenHeld(t *testing.T) {
	// 注入一个永远拒绝的锁后端：账号锁不可得 → fn 必须被跳过。
	SetRecoveryAccountLockCache(tickDenyLock{})
	defer SetRecoveryAccountLockCache(nil)

	var ran atomic.Int32
	if ok := withRecoveryAccountProbeLock(context.Background(), 42, func() { ran.Add(1) }); ok {
		t.Fatal("withRecoveryAccountProbeLock returned true under deny lock")
	}
	if ran.Load() != 0 {
		t.Fatalf("fn ran under deny lock: %d", ran.Load())
	}

	// 换成可获取的锁后端 → fn 执行。
	SetRecoveryAccountLockCache(&tickFakeLock{})
	if ok := withRecoveryAccountProbeLock(context.Background(), 42, func() { ran.Add(1) }); !ok {
		t.Fatal("withRecoveryAccountProbeLock returned false under allow lock")
	}
	if ran.Load() != 1 {
		t.Fatalf("fn not run under allow lock: %d", ran.Load())
	}
}

// 外审第3轮：健康探测间隔可配到 600s 时，唯一 tick 的驱动分辨率不得慢于六源最短
// cadence（1 分钟），否则 1 分钟任务会退化为约 10 分钟。
func TestBackgroundTickBaseInterval(t *testing.T) {
	if got := backgroundTickBaseInterval(600*time.Second, time.Minute); got != time.Minute {
		t.Fatalf("probe=600s cadence=1m => base=%s want 1m", got)
	}
	if got := backgroundTickBaseInterval(15*time.Second, time.Minute); got != 15*time.Second {
		t.Fatalf("probe=15s cadence=1m => base=%s want 15s", got)
	}
	if got := backgroundTickBaseInterval(0, 0); got != 15*time.Second {
		t.Fatalf("zero => base=%s want 15s", got)
	}
}

func TestBackgroundTickRegistryMinCadence(t *testing.T) {
	r := newTestRegistry(nil)
	r.Register(backgroundTickTask{name: "a", cadence: 5 * time.Minute, run: func(context.Context) error { return nil }})
	r.Register(backgroundTickTask{name: "b", cadence: time.Minute, run: func(context.Context) error { return nil }})
	if got := r.MinCadence(); got != time.Minute {
		t.Fatalf("MinCadence=%s want 1m", got)
	}
	r.Unregister("b")
	if got := r.MinCadence(); got != 5*time.Minute {
		t.Fatalf("MinCadence after unregister=%s want 5m", got)
	}
}
