//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Stubs
// ---------------------------------------------------------------------------

// pool401AccountRepoStub 记录 SetTempUnschedulable 调用，支持注入前置失败（R6-F2）。
type pool401AccountRepoStub struct {
	AccountRepository
	mu           sync.Mutex
	failNext     int
	failedCalls  int
	setTempCalls int
	lastUntil    time.Time
	lastReason   string
}

func (r *pool401AccountRepoStub) SetTempUnschedulable(_ context.Context, _ int64, until time.Time, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failNext > 0 {
		r.failNext--
		r.failedCalls++
		return errors.New("db unavailable")
	}
	r.setTempCalls++
	r.lastUntil = until
	r.lastReason = reason
	return nil
}

func (r *pool401AccountRepoStub) snapshot() (failed, succeeded int, until time.Time, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failedCalls, r.setTempCalls, r.lastUntil, r.lastReason
}

// pool401OpsRepoStub 记录 CreateAlertEvent 调用（P1 告警断言）。
type pool401OpsRepoStub struct {
	OpsRepository
	mu         sync.Mutex
	alertCalls int
	lastEvent  *OpsAlertEvent
}

func (r *pool401OpsRepoStub) CreateAlertEvent(_ context.Context, event *OpsAlertEvent) (*OpsAlertEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.alertCalls++
	r.lastEvent = event
	return event, nil
}

func (r *pool401OpsRepoStub) alerts() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.alertCalls
}

// pool401CacheStub 记录 TempUnschedCache.SetTempUnsched 调用。
type pool401CacheStub struct {
	TempUnschedCache
	mu       sync.Mutex
	setCalls int
}

func (c *pool401CacheStub) SetTempUnsched(_ context.Context, _ int64, _ *TempUnschedState) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setCalls++
	return nil
}

func (c *pool401CacheStub) sets() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.setCalls
}

// pool401RuntimeBlockerStub 记录进程内调度阻塞通知。
type pool401RuntimeBlockerStub struct {
	mu         sync.Mutex
	blockCalls int
}

func (b *pool401RuntimeBlockerStub) BlockAccountScheduling(*Account, time.Time, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.blockCalls++
}
func (b *pool401RuntimeBlockerStub) ClearAccountSchedulingBlock(int64) {}

func (b *pool401RuntimeBlockerStub) blocks() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.blockCalls
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newPool401Service(repo *pool401AccountRepoStub, ops *pool401OpsRepoStub) (*RateLimitService, *pool401CacheStub, *pool401RuntimeBlockerStub) {
	cache := &pool401CacheStub{}
	blocker := &pool401RuntimeBlockerStub{}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, cache)
	svc.SetOpsRepository(ops)
	svc.SetAccountRuntimeBlocker(blocker)
	return svc, cache, blocker
}

func pool401Account(id int64) *Account {
	return &Account{
		ID:          id,
		Platform:    PlatformDeepseek,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true},
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// 阈值以下不动作；第 6 次跨越边沿恰好一次停调 + 一次告警；同 epoch 后续 401 仅计数
// （R2-F4 边沿幂等）。
func TestNotePool401_EscalatesOnceAtThresholdAndStaysIdempotent(t *testing.T) {
	repo := &pool401AccountRepoStub{}
	ops := &pool401OpsRepoStub{}
	svc, cache, blocker := newPool401Service(repo, ops)
	acc := pool401Account(4101)
	defer resetPool401State(acc.ID)

	base := time.Now()
	for i := 0; i < pool401EscalationThreshold-1; i++ {
		svc.notePool401(context.Background(), acc, base.Add(time.Duration(i)*time.Second))
	}
	failed, succeeded, _, _ := repo.snapshot()
	require.Zero(t, failed)
	require.Zero(t, succeeded, "below threshold must not escalate")
	require.Zero(t, ops.alerts())
	require.Zero(t, cache.sets())
	require.Zero(t, blocker.blocks())

	svc.notePool401(context.Background(), acc, base.Add(time.Duration(pool401EscalationThreshold)*time.Second))
	failed, succeeded, until, reason := repo.snapshot()
	require.Zero(t, failed)
	require.Equal(t, 1, succeeded, "exactly one escalation at the edge")
	require.True(t, until.Equal(base.Add(time.Duration(pool401EscalationThreshold)*time.Second + pool401EscalationCooldown)))
	require.Equal(t, 1, ops.alerts())
	require.Equal(t, 1, cache.sets())
	require.Equal(t, 1, blocker.blocks())

	// 停调原因可解析：marker + Tier=3（供 CARD-C 探测恢复按 marker 识别）。
	var state TempUnschedState
	require.NoError(t, json.Unmarshal([]byte(reason), &state))
	require.Equal(t, PoolMode401EscalationMarker, state.MatchedKeyword)
	require.Equal(t, HealthBreakerTierTrip, state.Tier)
	require.Equal(t, http.StatusUnauthorized, state.StatusCode)

	// 同 epoch 内后续 401 仅计数：不重复停调、不重复告警。
	svc.notePool401(context.Background(), acc, base.Add(10*time.Second))
	svc.notePool401(context.Background(), acc, base.Add(11*time.Second))
	_, succeeded, _, _ = repo.snapshot()
	require.Equal(t, 1, succeeded)
	require.Equal(t, 1, ops.alerts())
	require.Equal(t, 1, cache.sets())
	require.Equal(t, 1, blocker.blocks())
}

// 停调 → 唯一重置入口（调度器成功/probe 恢复出口）→ 再次满 6 次 → 再次停调（R4-F3）。
func TestNotePool401_ResetAllowsSecondEscalation(t *testing.T) {
	repo := &pool401AccountRepoStub{}
	ops := &pool401OpsRepoStub{}
	svc, _, _ := newPool401Service(repo, ops)
	acc := pool401Account(4102)
	defer resetPool401State(acc.ID)

	base := time.Now()
	for i := 0; i < pool401EscalationThreshold; i++ {
		svc.notePool401(context.Background(), acc, base.Add(time.Duration(i)*time.Second))
	}
	_, succeeded, _, _ := repo.snapshot()
	require.Equal(t, 1, succeeded)

	resetPool401State(acc.ID)

	for i := 0; i < pool401EscalationThreshold; i++ {
		svc.notePool401(context.Background(), acc, base.Add(time.Minute).Add(time.Duration(i)*time.Second))
	}
	_, succeeded, _, _ = repo.snapshot()
	require.Equal(t, 2, succeeded, "must escalate again after reset")
	require.Equal(t, 2, ops.alerts())
}

// 停调 → 到期恢复（无探测）→ 窗口自然轮转开新 epoch → 新窗口正常计数并再次跨越边沿（R4-F3）。
func TestNotePool401_WindowRotationOpensNewEpochAndRetrips(t *testing.T) {
	repo := &pool401AccountRepoStub{}
	ops := &pool401OpsRepoStub{}
	svc, _, _ := newPool401Service(repo, ops)
	acc := pool401Account(4103)
	defer resetPool401State(acc.ID)

	t0 := time.Now()
	for i := 0; i < pool401EscalationThreshold; i++ {
		svc.notePool401(context.Background(), acc, t0.Add(time.Duration(i)*time.Second))
	}
	_, succeeded, _, _ := repo.snapshot()
	require.Equal(t, 1, succeeded)

	// 新窗口（> pool401EscalationWindow 之后）：前 5 次只计数，第 6 次再次跨越边沿。
	rotated := t0.Add(pool401EscalationWindow + time.Minute)
	for i := 0; i < pool401EscalationThreshold-1; i++ {
		svc.notePool401(context.Background(), acc, rotated.Add(time.Duration(i)*time.Second))
	}
	_, succeeded, _, _ = repo.snapshot()
	require.Equal(t, 1, succeeded, "new window below threshold must not retrip yet")

	svc.notePool401(context.Background(), acc, rotated.Add(time.Duration(pool401EscalationThreshold)*time.Second))
	_, succeeded, _, _ = repo.snapshot()
	require.Equal(t, 2, succeeded, "new window must retrip at the edge")
	require.Equal(t, 2, ops.alerts())
}

// 跨阈值遇持久化失败：不锁存边沿、无告警；恢复后下一个 401 → 恰好一次停调一次告警（R6-F2）。
func TestNotePool401_PersistFailureDoesNotLatchEdge(t *testing.T) {
	repo := &pool401AccountRepoStub{}
	ops := &pool401OpsRepoStub{}
	svc, cache, _ := newPool401Service(repo, ops)
	acc := pool401Account(4104)
	defer resetPool401State(acc.ID)

	base := time.Now()
	repo.mu.Lock()
	repo.failNext = 1
	repo.mu.Unlock()
	for i := 0; i < pool401EscalationThreshold; i++ {
		svc.notePool401(context.Background(), acc, base.Add(time.Duration(i)*time.Second))
	}
	failed, succeeded, _, _ := repo.snapshot()
	require.Equal(t, 1, failed, "escalation must have been attempted at the edge")
	require.Zero(t, succeeded, "failed persistence must not count as escalation")
	require.Zero(t, ops.alerts(), "no alert on persistence failure")
	require.Zero(t, cache.sets())

	// 仓库恢复：同窗口下一个 401 重试升级，恰好一次停调 + 一次告警。
	// （failed 为累计计数，保留阶段一的 1 次失败；恢复调用本身成功，不再增长。）
	svc.notePool401(context.Background(), acc, base.Add(time.Minute))
	failed, succeeded, _, _ = repo.snapshot()
	require.Equal(t, 1, failed, "cumulative failure count from phase 1 only")
	require.Equal(t, 1, succeeded)
	require.Equal(t, 1, ops.alerts())

	// 成功后边沿已锁存：同 epoch 后续 401 不再重复。
	svc.notePool401(context.Background(), acc, base.Add(2*time.Minute))
	_, succeeded, _, _ = repo.snapshot()
	require.Equal(t, 1, succeeded)
	require.Equal(t, 1, ops.alerts())
}

// 并发 401：边沿只消费一次——恰好一次停调、一次告警（R2-F4）。
func TestNotePool401_ConcurrentNotesEscalateExactlyOnce(t *testing.T) {
	repo := &pool401AccountRepoStub{}
	ops := &pool401OpsRepoStub{}
	svc, cache, blocker := newPool401Service(repo, ops)
	acc := pool401Account(4105)
	defer resetPool401State(acc.ID)

	const goroutines = 60
	now := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc.notePool401(context.Background(), acc, now)
		}()
	}
	wg.Wait()

	_, succeeded, _, _ := repo.snapshot()
	require.Equal(t, 1, succeeded, "exactly one escalation under concurrency")
	require.Equal(t, 1, ops.alerts(), "exactly one alert under concurrency")
	require.Equal(t, 1, cache.sets())
	require.Equal(t, 1, blocker.blocks())
}

// HandleUpstreamError 池模式 401 豁免点喂计数；返回值保持既有语义（shouldDisable=false）；
// 非 401 池模式错误不喂计数。
func TestHandleUpstreamError_PoolMode401FeedsEscalationCounter(t *testing.T) {
	repo := &pool401AccountRepoStub{}
	ops := &pool401OpsRepoStub{}
	svc, _, _ := newPool401Service(repo, ops)
	acc := pool401Account(4106)
	defer resetPool401State(acc.ID)

	ctx := context.Background()
	for i := 0; i < pool401EscalationThreshold; i++ {
		require.False(t, svc.HandleUpstreamError(ctx, acc, http.StatusUnauthorized, nil, []byte(`{"error":"unauthorized"}`)),
			"single 401 must keep the existing non-disable semantics")
	}
	_, succeeded, _, _ := repo.snapshot()
	require.Equal(t, 1, succeeded)
	require.Equal(t, 1, ops.alerts())

	// 非 401 池模式错误不得喂 401 计数：重置后喂 5 次 401 + 1 次 500，不应跨越边沿。
	resetPool401State(acc.ID)
	for i := 0; i < pool401EscalationThreshold-1; i++ {
		svc.HandleUpstreamError(ctx, acc, http.StatusUnauthorized, nil, nil)
	}
	svc.HandleUpstreamError(ctx, acc, http.StatusInternalServerError, nil, nil)
	_, succeeded, _, _ = repo.snapshot()
	require.Equal(t, 1, succeeded, "non-401 pool errors must not feed the 401 window")
	svc.HandleUpstreamError(ctx, acc, http.StatusUnauthorized, nil, nil)
	_, succeeded, _, _ = repo.snapshot()
	require.Equal(t, 2, succeeded)
}
