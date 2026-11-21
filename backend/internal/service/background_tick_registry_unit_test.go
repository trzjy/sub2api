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

// 去多副本后 registry 不再依赖 leader 后端：无 leader 也必须直接执行 run。
func TestBackgroundTickRegistryFailClosedWithoutLeader(t *testing.T) {
	r := newTestRegistry(nil)
	var calls atomic.Int32
	r.Register(backgroundTickTask{name: "a", cadence: time.Minute, maxRun: time.Second,
		firstRun: tickFirstRunImmediate, run: func(context.Context) error { calls.Add(1); return nil }})
	r.DispatchDue(context.Background())
	if !waitCond(t, func() bool { return calls.Load() == 1 }, time.Second) {
		t.Fatalf("run did not execute without leader backend; calls=%d", calls.Load())
	}
}

func TestBackgroundTickRegistryImmediateFirstRun(t *testing.T) {
	r := newTestRegistry(nil)
	var calls atomic.Int32
	r.Register(backgroundTickTask{name: "a", cadence: time.Minute, maxRun: time.Second,
		firstRun: tickFirstRunImmediate, run: func(context.Context) error { calls.Add(1); return nil }})
	r.DispatchDue(context.Background())
	if !waitCond(t, func() bool { return calls.Load() == 1 }, time.Second) {
		t.Fatalf("immediate first run not executed; calls=%d", calls.Load())
	}
}

func TestBackgroundTickRegistryAfterCadence(t *testing.T) {
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	var nowVal atomic.Value
	nowVal.Store(base)
	r := newTestRegistry(func() time.Time { return nowVal.Load().(time.Time) })
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
	r := newTestRegistry(nil)
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
	r := newTestRegistry(nil)
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
	r := newTestRegistry(nil)
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

func TestBackgroundTickRegistryUnregisterDrainsInFlight(t *testing.T) {
	r := newTestRegistry(nil)
	started := make(chan struct{})
	release := make(chan struct{})
	// run 占用在途执行直到 ctx 取消或 release。
	r.Register(backgroundTickTask{name: "a", cadence: time.Minute, maxRun: time.Second,
		firstRun: tickFirstRunImmediate,
		run: func(ctx context.Context) error {
			close(started)
			select {
			case <-ctx.Done():
			case <-release:
			}
			return nil
		}})
	r.DispatchDue(context.Background())
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("in-flight run did not start")
	}
	// Unregister 应取消 ctx 并在远小于有界上限内排空返回，而不是无界等待。
	done := make(chan struct{})
	go func() {
		r.Unregister("a")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Unregister blocked instead of bounded drain")
	}
	if r.RegisteredCount() != 0 {
		t.Fatalf("RegisteredCount=%d want 0", r.RegisteredCount())
	}
	close(release) // 幂等清理：run 已因 ctx 取消退出
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

func TestBackgroundTickRegistryStartStopIdempotent(t *testing.T) {
	r := newTestRegistry(nil)
	var calls atomic.Int32
	// cadence 远小于 tick 间隔：每个自持 tick 都应到期一次。
	r.Register(backgroundTickTask{name: "a", cadence: 10 * time.Millisecond, maxRun: time.Second,
		firstRun: tickFirstRunImmediate,
		run:      func(context.Context) error { calls.Add(1); return nil }})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 多次 Start 必须幂等：只启动一个驱动循环。
	r.Start(ctx)
	r.Start(ctx)
	r.Start(ctx)

	// 启动先派发一次。
	if !waitCond(t, func() bool { return calls.Load() == 1 }, time.Second) {
		t.Fatalf("Start initial dispatch failed; calls=%d", calls.Load())
	}
	// 跨过 tick 间隔后自持 ticker 应继续派发。
	if !waitCond(t, func() bool { return calls.Load() >= 2 }, backgroundTickDispatchInterval*3) {
		t.Fatalf("self-driven ticker did not dispatch; calls=%d", calls.Load())
	}

	// Stop 幂等，且停止后不再有自动派发（非幂等泄漏的 goroutine 会在此暴露）。
	r.Stop()
	r.Stop()
	before := calls.Load()
	time.Sleep(backgroundTickDispatchInterval * 2)
	if after := calls.Load(); after != before {
		t.Fatalf("dispatch continued after Stop: before=%d after=%d", before, after)
	}
}

func TestBackgroundTickRegistryStartStopRestart(t *testing.T) {
	r := newTestRegistry(nil)
	var calls atomic.Int32
	// cadence 远小于 tick 间隔：每个自持 tick 都应到期一次。
	r.Register(backgroundTickTask{name: "a", cadence: 10 * time.Millisecond, maxRun: time.Second,
		firstRun: tickFirstRunImmediate,
		run:      func(context.Context) error { calls.Add(1); return nil }})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 第一次启动：先立即派发一次，随后自持 ticker 持续派发。
	r.Start(ctx)
	if !waitCond(t, func() bool { return calls.Load() >= 1 }, time.Second) {
		t.Fatalf("first Start initial dispatch failed; calls=%d", calls.Load())
	}

	// 停止：Stop 取消并等待 tick goroutine 退出后不再有自动派发。
	r.Stop()
	// Stop 仅等待 tick goroutine，不停在途 task goroutine；稍等其归零后取稳定基线。
	time.Sleep(50 * time.Millisecond)
	stopped := calls.Load()
	time.Sleep(backgroundTickDispatchInterval * 2)
	if after := calls.Load(); after != stopped {
		t.Fatalf("dispatch continued after Stop: before=%d after=%d", stopped, after)
	}

	// 再次启动：应重建 ticker 并重新派发（旧实现因 stopped 门闩会停在此处）。
	r.Start(ctx)
	if !waitCond(t, func() bool { return calls.Load() > stopped }, time.Second) {
		t.Fatalf("Start after Stop did not re-dispatch; stopped=%d now=%d", stopped, calls.Load())
	}
	// 连续第二次 Stop 仍幂等。
	r.Stop()
	r.Stop()
}
