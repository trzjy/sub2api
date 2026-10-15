//go:build unit

package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// --- 测试桩 ---

type staleFakeSource struct {
	wm  *ChannelMonitorV2AggregationWatermark
	cfg *ChannelMonitorV2Config
}

func (f *staleFakeSource) GetAggregationWatermark(_ context.Context) (*ChannelMonitorV2AggregationWatermark, error) {
	if f.wm == nil {
		return &ChannelMonitorV2AggregationWatermark{}, nil
	}
	return f.wm, nil
}

func (f *staleFakeSource) GetConfig(_ context.Context) (*ChannelMonitorV2Config, error) {
	if f.cfg == nil {
		return &ChannelMonitorV2Config{RefreshIntervalSeconds: 300}, nil
	}
	return f.cfg, nil
}

type staleFakeRepo struct {
	mu           sync.Mutex
	events       []*OpsAlertEvent
	nextID       int64
	createCalls  int
	resolveCalls int
	emailSent    int
	notifyCalls  int
}

func dimsEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func (r *staleFakeRepo) ListAlertEvents(_ context.Context, filter *OpsAlertEventFilter) ([]*OpsAlertEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*OpsAlertEvent, 0, len(r.events))
	for _, ev := range r.events {
		if ev == nil {
			continue
		}
		if filter.Status != "" && ev.Status != filter.Status {
			continue
		}
		if filter.DimensionExact != nil && !dimsEqual(ev.Dimensions, filter.DimensionExact) {
			continue
		}
		out = append(out, ev)
	}
	return out, nil
}

func (r *staleFakeRepo) CreateAlertEvent(_ context.Context, ev *OpsAlertEvent) (*OpsAlertEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.createCalls++
	r.nextID++
	ev.ID = r.nextID
	cp := *ev
	r.events = append(r.events, &cp)
	return &cp, nil
}

func (r *staleFakeRepo) UpdateAlertEventStatus(_ context.Context, id int64, status string, resolvedAt *time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resolveCalls++
	for _, ev := range r.events {
		if ev != nil && ev.ID == id {
			ev.Status = status
			ev.ResolvedAt = resolvedAt
		}
	}
	return nil
}

func (r *staleFakeRepo) UpdateAlertEventEmailSent(_ context.Context, _ int64, _ bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.emailSent++
	return nil
}

func (r *staleFakeRepo) activeStaleCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, ev := range r.events {
		if ev != nil && ev.Status == OpsAlertStatusFiring && dimsEqual(ev.Dimensions, channelMonitorV2StaleDims()) {
			n++
		}
	}
	return n
}

func newStaleEvaluator(t *testing.T, src *staleFakeSource, graceStart time.Time, now time.Time) (*ChannelMonitorV2StaleAlertEvaluator, *staleFakeRepo) {
	t.Helper()
	repo := &staleFakeRepo{}
	e := NewChannelMonitorV2StaleAlertEvaluator(src, repo)
	e.SetClock(func() time.Time { return now })
	if !graceStart.IsZero() {
		e.SetGraceStart(graceStart)
	}
	return e, repo
}

// TestStaleAlert_WorkerNeverRegistered：worker 完全未注册（水位从未写入，LastSuccessfulAt 零值）
// 且冷启动宽限已满 → 检测体仍应触发告警（覆盖 worker 永不装配死区）。对应 Done when #2。
func TestChannelMonitorV2StaleAlert_WorkerNeverRegistered(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	// 进程启动 > 30 分钟（宽限已过期），水位缺失。
	src := &staleFakeSource{wm: &ChannelMonitorV2AggregationWatermark{}}
	e, repo := newStaleEvaluator(t, src, now.Add(-31*time.Minute), now)

	require.NoError(t, e.Evaluate(context.Background(), defaultOpsAlertRuntimeSettings()))
	require.Equal(t, 1, repo.createCalls, "worker 未注册且宽限期满应创建停滞告警")
	require.Equal(t, 1, repo.activeStaleCount())

	// 重复 evaluate 不应重复创建（幂等键）。
	require.NoError(t, e.Evaluate(context.Background(), defaultOpsAlertRuntimeSettings()))
	require.Equal(t, 1, repo.createCalls, "重复 evaluate 不应重复创建告警")
	require.Equal(t, 1, repo.activeStaleCount())
}

// TestStaleAlert_ColdStartGraceWithin：进程启动 30 分钟内水位缺失 → 不告警。对应 Done when #3。
func TestChannelMonitorV2StaleAlert_ColdStartGraceWithin(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	src := &staleFakeSource{wm: &ChannelMonitorV2AggregationWatermark{}}
	// 进程启动仅 10 分钟（宽限内）。
	e, repo := newStaleEvaluator(t, src, now.Add(-10*time.Minute), now)

	require.NoError(t, e.Evaluate(context.Background(), defaultOpsAlertRuntimeSettings()))
	require.Equal(t, 0, repo.createCalls, "冷启动宽限内（30min）水位缺失不应告警")
	require.Equal(t, 0, repo.activeStaleCount())
}

// TestStaleAlert_ColdStartGraceAfter：进程启动 > 30 分钟且水位缺失 → 告警。对应 Done when #3。
func TestChannelMonitorV2StaleAlert_ColdStartGraceAfter(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	src := &staleFakeSource{wm: &ChannelMonitorV2AggregationWatermark{}}
	e, repo := newStaleEvaluator(t, src, now.Add(-31*time.Minute), now)

	require.NoError(t, e.Evaluate(context.Background(), defaultOpsAlertRuntimeSettings()))
	require.Equal(t, 1, repo.createCalls, "冷启动宽限期满后水位缺失应告警")
	require.Equal(t, 1, repo.activeStaleCount())
}

// TestStaleAlert_AgeGreaterThanTwoX：首次成功扫描后，水位年龄 > 2×刷新周期（严格大于）→ 告警。对应 Done when #3。
func TestChannelMonitorV2StaleAlert_AgeGreaterThanTwoX(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	// RefreshIntervalSeconds=300s → 停滞阈值 = 600s。年龄 601s > 600s → 停滞。
	src := &staleFakeSource{
		wm:  &ChannelMonitorV2AggregationWatermark{LastSuccessfulAt: now.Add(-601 * time.Second)},
		cfg: &ChannelMonitorV2Config{RefreshIntervalSeconds: 300},
	}
	e, repo := newStaleEvaluator(t, src, now.Add(-time.Hour), now)

	require.NoError(t, e.Evaluate(context.Background(), defaultOpsAlertRuntimeSettings()))
	require.Equal(t, 1, repo.createCalls, "年龄 > 2×刷新周期应告警")
	require.Equal(t, 1, repo.activeStaleCount())
}

// TestStaleAlert_AgeEqualsTwoX：年龄 == 2×刷新周期（非严格大于）→ 不告警。对应 Done when #3。
func TestChannelMonitorV2StaleAlert_AgeEqualsTwoX(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	src := &staleFakeSource{
		wm:  &ChannelMonitorV2AggregationWatermark{LastSuccessfulAt: now.Add(-600 * time.Second)},
		cfg: &ChannelMonitorV2Config{RefreshIntervalSeconds: 300},
	}
	e, repo := newStaleEvaluator(t, src, now.Add(-time.Hour), now)

	require.NoError(t, e.Evaluate(context.Background(), defaultOpsAlertRuntimeSettings()))
	require.Equal(t, 0, repo.createCalls, "年龄 == 2×刷新周期（非严格大于）不应告警")
	require.Equal(t, 0, repo.activeStaleCount())
}

// TestStaleAlert_AgeLessThanTwoX：年龄 < 2×刷新周期（正常刷新）→ 不告警。对应 Done when #3。
func TestChannelMonitorV2StaleAlert_AgeLessThanTwoX(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	src := &staleFakeSource{
		wm:  &ChannelMonitorV2AggregationWatermark{LastSuccessfulAt: now.Add(-300 * time.Second)},
		cfg: &ChannelMonitorV2Config{RefreshIntervalSeconds: 300},
	}
	e, repo := newStaleEvaluator(t, src, now.Add(-time.Hour), now)

	require.NoError(t, e.Evaluate(context.Background(), defaultOpsAlertRuntimeSettings()))
	require.Equal(t, 0, repo.createCalls, "年龄 < 2×刷新周期不应告警")
	require.Equal(t, 0, repo.activeStaleCount())
}

// TestStaleAlert_RecoveryAutoResolve：水位刷新（年龄回落）→ 自动 resolve 既有 firing 告警。对应 Done when #3。
func TestChannelMonitorV2StaleAlert_RecoveryAutoResolve(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	src := &staleFakeSource{
		wm:  &ChannelMonitorV2AggregationWatermark{LastSuccessfulAt: now.Add(-601 * time.Second)},
		cfg: &ChannelMonitorV2Config{RefreshIntervalSeconds: 300},
	}
	e, repo := newStaleEvaluator(t, src, now.Add(-time.Hour), now)

	// 先让水位停滞 → 创建 firing 告警。
	require.NoError(t, e.Evaluate(context.Background(), defaultOpsAlertRuntimeSettings()))
	require.Equal(t, 1, repo.activeStaleCount())

	// 恢复：水位刷新到 now（年龄 0）。
	src.wm = &ChannelMonitorV2AggregationWatermark{LastSuccessfulAt: now}
	require.NoError(t, e.Evaluate(context.Background(), defaultOpsAlertRuntimeSettings()))
	require.Equal(t, 0, repo.activeStaleCount(), "水位刷新后应自动 resolve")
	require.Equal(t, 1, repo.resolveCalls, "恢复应恰好关闭一次告警")
}

// TestStaleAlert_IdempotentResolve：重复 evaluate（非停滞 + 无活动告警）不重复 resolve。对应 Done when #4。
func TestChannelMonitorV2StaleAlert_IdempotentResolve(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	src := &staleFakeSource{
		wm:  &ChannelMonitorV2AggregationWatermark{LastSuccessfulAt: now.Add(-601 * time.Second)},
		cfg: &ChannelMonitorV2Config{RefreshIntervalSeconds: 300},
	}
	e, repo := newStaleEvaluator(t, src, now.Add(-time.Hour), now)

	require.NoError(t, e.Evaluate(context.Background(), defaultOpsAlertRuntimeSettings()))
	require.Equal(t, 1, repo.activeStaleCount())

	// 恢复后连续多次 evaluate。
	src.wm = &ChannelMonitorV2AggregationWatermark{LastSuccessfulAt: now}
	require.NoError(t, e.Evaluate(context.Background(), defaultOpsAlertRuntimeSettings()))
	require.NoError(t, e.Evaluate(context.Background(), defaultOpsAlertRuntimeSettings()))
	require.Equal(t, 1, repo.resolveCalls, "重复 evaluate 不应重复 resolve")
}

// TestStaleAlert_IdempotentCreate：重复 evaluate（停滞 + 已有 firing）不重复创建。对应 Done when #4。
func TestChannelMonitorV2StaleAlert_IdempotentCreate(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	src := &staleFakeSource{
		wm:  &ChannelMonitorV2AggregationWatermark{LastSuccessfulAt: now.Add(-601 * time.Second)},
		cfg: &ChannelMonitorV2Config{RefreshIntervalSeconds: 300},
	}
	e, repo := newStaleEvaluator(t, src, now.Add(-time.Hour), now)

	require.NoError(t, e.Evaluate(context.Background(), defaultOpsAlertRuntimeSettings()))
	require.NoError(t, e.Evaluate(context.Background(), defaultOpsAlertRuntimeSettings()))
	require.NoError(t, e.Evaluate(context.Background(), defaultOpsAlertRuntimeSettings()))
	require.Equal(t, 1, repo.createCalls, "重复 evaluate（停滞）应仅创建一次")
	require.Equal(t, 1, repo.activeStaleCount())
}

// TestStaleAlert_EmailViaOpsChain：创建告警应通过注入的 notify 走既有 ops_alert 邮件链。对应 Done when #3 邮件送达。
func TestChannelMonitorV2StaleAlert_EmailViaOpsChain(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	src := &staleFakeSource{
		wm:  &ChannelMonitorV2AggregationWatermark{LastSuccessfulAt: now.Add(-601 * time.Second)},
		cfg: &ChannelMonitorV2Config{RefreshIntervalSeconds: 300},
	}
	e, repo := newStaleEvaluator(t, src, now.Add(-time.Hour), now)

	var notified int
	e.SetNotifier(func(_ context.Context, _ *OpsAlertRuntimeSettings, _ *OpsAlertEvent) bool {
		notified++
		return true
	})

	require.NoError(t, e.Evaluate(context.Background(), defaultOpsAlertRuntimeSettings()))
	require.Equal(t, 1, notified, "创建告警应触发邮件送达")
	require.Equal(t, 1, repo.emailSent, "邮件送达应标记 EmailSent")
}

// TestStaleAlert_NoSourceNoPanic：水源未注册时 Evaluate 安全 no-op（不崩溃）。
func TestChannelMonitorV2StaleAlert_NoSourceNoPanic(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	repo := &staleFakeRepo{}
	e := &ChannelMonitorV2StaleAlertEvaluator{opsRepo: repo}
	e.SetClock(func() time.Time { return now })
	require.NoError(t, e.Evaluate(context.Background(), defaultOpsAlertRuntimeSettings()))
	require.Equal(t, 0, repo.createCalls)
}

// TestChannelMonitorV2StaleAlert_AggregatorNewRegistersSource 验证生产接线：聚合器构造
// （即使 Start 未调用 / worker 完全未装配）即注册共享水位源，检测体随后仍能读到持久水位。
// 对应 Done when #2「worker 完全未注册场景仍能触发告警」的装配前提。
// 注意：本测试修改包级共享源，不 t.Parallel（Go 先串行跑非并行测试，无并发污染）。
func TestChannelMonitorV2StaleAlert_AggregatorNewRegistersSource(t *testing.T) {
	prev := channelMonitorV2StaleWatermarkSource
	defer func() { channelMonitorV2StaleWatermarkSource = prev }()

	repo := &channelMonitorV2RepoStub{}
	aggr := NewChannelMonitorV2Aggregator(repo, nil, nil)
	require.NotNil(t, aggr)
	require.Same(t, repo, channelMonitorV2StaleWatermarkSource,
		"聚合器构造（未 Start）应注册共享水位源，检测体才能读到水位并告警")
}
