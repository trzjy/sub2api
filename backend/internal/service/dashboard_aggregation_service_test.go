package service

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type dashboardAggregationRepoTestStub struct {
	aggregateCalls       int
	recomputeCalls       int
	cleanupUsageCalls    int
	cleanupUsageTxCalls  int
	cleanupDedupCalls    int
	ensurePartitionCalls int
	lastStart            time.Time
	lastEnd              time.Time
	watermark            time.Time
	aggregateErr         error
	cleanupAggregatesErr error
	cleanupUsageErr      error
	cleanupUsageTxErr    error
	cleanupDedupErr      error
	ensurePartitionErr   error
	aggregateCtx         context.Context
	events               *[]string
}

type dashboardAggregationRollupRepoTestStub struct {
	*dashboardAggregationRepoTestStub
	groupRollupCalls int
	groupRollupAt    time.Time
	groupRollupErr   error
	groupRollupCtx   context.Context
}

func (s *dashboardAggregationRollupRepoTestStub) SyncGroupUsageRollups(ctx context.Context, todayStart time.Time) error {
	s.groupRollupCalls++
	s.groupRollupAt = todayStart
	s.groupRollupCtx = ctx
	if s.events != nil {
		*s.events = append(*s.events, "group_rollup")
	}
	return s.groupRollupErr
}

func (s *dashboardAggregationRepoTestStub) AggregateRange(ctx context.Context, start, end time.Time) error {
	s.aggregateCalls++
	s.aggregateCtx = ctx
	s.lastStart = start
	s.lastEnd = end
	if s.events != nil {
		*s.events = append(*s.events, "dashboard_aggregation")
	}
	return s.aggregateErr
}

func (s *dashboardAggregationRepoTestStub) RecomputeRange(ctx context.Context, start, end time.Time) error {
	s.recomputeCalls++
	return s.AggregateRange(ctx, start, end)
}

func (s *dashboardAggregationRepoTestStub) GetAggregationWatermark(ctx context.Context) (time.Time, error) {
	return s.watermark, nil
}

func (s *dashboardAggregationRepoTestStub) UpdateAggregationWatermark(ctx context.Context, aggregatedAt time.Time) error {
	return nil
}

func (s *dashboardAggregationRepoTestStub) CleanupAggregates(ctx context.Context, hourlyCutoff, dailyCutoff time.Time) error {
	return s.cleanupAggregatesErr
}

func (s *dashboardAggregationRepoTestStub) CleanupUsageLogs(ctx context.Context, cutoff time.Time) error {
	s.cleanupUsageCalls++
	return s.cleanupUsageErr
}

func (s *dashboardAggregationRepoTestStub) CleanupUsageLogsTx(ctx context.Context, tx *sql.Tx, cutoff time.Time) error {
	s.cleanupUsageTxCalls++
	return s.cleanupUsageTxErr
}

func (s *dashboardAggregationRepoTestStub) CleanupUsageBillingDedup(ctx context.Context, cutoff time.Time) error {
	s.cleanupDedupCalls++
	return s.cleanupDedupErr
}

func (s *dashboardAggregationRepoTestStub) EnsureUsageLogsPartitions(ctx context.Context, now time.Time) error {
	s.ensurePartitionCalls++
	return s.ensurePartitionErr
}

func TestDashboardAggregationService_RunScheduledAggregation_EpochUsesRetentionStart(t *testing.T) {
	repo := &dashboardAggregationRepoTestStub{watermark: time.Unix(0, 0).UTC()}
	svc := &DashboardAggregationService{
		repo: repo,
		cfg: config.DashboardAggregationConfig{
			Enabled:         true,
			IntervalSeconds: 60,
			LookbackSeconds: 120,
			Retention: config.DashboardAggregationRetentionConfig{
				UsageLogsDays: 1,
				HourlyDays:    1,
				DailyDays:     1,
			},
		},
	}

	svc.runScheduledAggregation()

	require.Equal(t, 1, repo.aggregateCalls)
	require.False(t, repo.lastEnd.IsZero())
	require.Equal(t, truncateToDayUTC(repo.lastEnd.AddDate(0, 0, -1)), repo.lastStart)
}

func TestDashboardAggregationService_RunScheduledAggregationSyncsGroupUsageRollups(t *testing.T) {
	baseRepo := &dashboardAggregationRepoTestStub{watermark: time.Now().UTC()}
	repo := &dashboardAggregationRollupRepoTestStub{dashboardAggregationRepoTestStub: baseRepo}
	svc := &DashboardAggregationService{
		repo: repo,
		cfg: config.DashboardAggregationConfig{
			Enabled:         true,
			IntervalSeconds: 60,
			LookbackSeconds: 120,
			Retention: config.DashboardAggregationRetentionConfig{
				UsageLogsDays:         1,
				UsageBillingDedupDays: 2,
				HourlyDays:            1,
				DailyDays:             1,
			},
		},
	}

	before := GroupUsageTodayStart(time.Now())
	svc.runScheduledAggregation()
	after := GroupUsageTodayStart(time.Now())

	require.Equal(t, 1, repo.groupRollupCalls)
	require.Contains(t, []time.Time{before, after}, repo.groupRollupAt)
}

func TestDashboardAggregationService_RunScheduledAggregationSyncsGroupAfterDashboardEarlyReturn(t *testing.T) {
	events := make([]string, 0, 2)
	baseRepo := &dashboardAggregationRepoTestStub{
		watermark:    time.Now().UTC(),
		aggregateErr: errors.New("dashboard aggregation failed"),
		events:       &events,
	}
	repo := &dashboardAggregationRollupRepoTestStub{
		dashboardAggregationRepoTestStub: baseRepo,
		groupRollupErr:                   errors.New("group rollup failed"),
	}
	svc := &DashboardAggregationService{
		repo: repo,
		cfg: config.DashboardAggregationConfig{
			LookbackSeconds: 120,
			Retention: config.DashboardAggregationRetentionConfig{
				UsageLogsDays: 1,
			},
		},
	}

	svc.runScheduledAggregation()

	require.Equal(t, []string{"dashboard_aggregation", "group_rollup"}, events)
	require.NotNil(t, repo.aggregateCtx)
	require.NotNil(t, repo.groupRollupCtx)
	if repo.aggregateCtx == repo.groupRollupCtx {
		t.Fatal("分组日汇总必须使用独立于 dashboard 聚合的 context")
	}
	groupDeadline, ok := repo.groupRollupCtx.Deadline()
	require.True(t, ok, "group rollup context must be bounded")
	require.LessOrEqual(t, time.Until(groupDeadline), defaultDashboardAggregationTimeout)
}

type dashboardAggregationLeaderLockRecordingCache struct {
	delegate    *fakeLeaderLockCache
	acquireKeys []string
	acquireTTLs []time.Duration
}

func (c *dashboardAggregationLeaderLockRecordingCache) TryAcquireLeaderLock(ctx context.Context, key, owner string, ttl time.Duration) (bool, error) {
	c.acquireKeys = append(c.acquireKeys, key)
	c.acquireTTLs = append(c.acquireTTLs, ttl)
	return c.delegate.TryAcquireLeaderLock(ctx, key, owner, ttl)
}

func (c *dashboardAggregationLeaderLockRecordingCache) ReleaseLeaderLock(ctx context.Context, key, owner string) error {
	return c.delegate.ReleaseLeaderLock(ctx, key, owner)
}

func TestDashboardAggregationService_StartupGroupSyncUsesIndependentLongLivedLeaderLock(t *testing.T) {
	delegate := &fakeLeaderLockCache{}
	_, err := delegate.TryAcquireLeaderLock(context.Background(), dashboardAggregationLeaderLockKey, "periodic-peer", time.Hour)
	require.NoError(t, err)
	cache := &dashboardAggregationLeaderLockRecordingCache{delegate: delegate}
	repo := &dashboardAggregationRollupRepoTestStub{dashboardAggregationRepoTestStub: &dashboardAggregationRepoTestStub{}}
	svc := &DashboardAggregationService{
		repo:       repo,
		lockCache:  cache,
		instanceID: "startup-instance",
	}

	svc.runStartupGroupUsageSync()

	require.Len(t, cache.acquireKeys, 1)
	require.NotEqual(t, dashboardAggregationLeaderLockKey, cache.acquireKeys[0])
	require.Len(t, cache.acquireTTLs, 1)
	require.Greater(t, cache.acquireTTLs[0], defaultDashboardAggregationBackfillTimeout)
	require.Equal(t, 1, repo.groupRollupCalls)
}

func TestDashboardAggregationService_CleanupRetentionFailure_DoesNotRecord(t *testing.T) {
	repo := &dashboardAggregationRepoTestStub{cleanupAggregatesErr: errors.New("清理失败")}
	svc := &DashboardAggregationService{
		repo: repo,
		cfg: config.DashboardAggregationConfig{
			Retention: config.DashboardAggregationRetentionConfig{
				UsageLogsDays: 1,
				HourlyDays:    1,
				DailyDays:     1,
			},
		},
	}

	svc.maybeCleanupRetention(context.Background(), time.Now().UTC())

	require.Nil(t, svc.lastRetentionCleanup.Load())
	require.Equal(t, 1, repo.cleanupUsageCalls)
	require.Equal(t, 1, repo.cleanupDedupCalls)
}

func TestDashboardAggregationService_CleanupDedupFailure_DoesNotRecord(t *testing.T) {
	repo := &dashboardAggregationRepoTestStub{cleanupDedupErr: errors.New("dedup cleanup failed")}
	svc := &DashboardAggregationService{
		repo: repo,
		cfg: config.DashboardAggregationConfig{
			Retention: config.DashboardAggregationRetentionConfig{
				UsageLogsDays: 1,
				HourlyDays:    1,
				DailyDays:     1,
			},
		},
	}

	svc.maybeCleanupRetention(context.Background(), time.Now().UTC())

	require.Nil(t, svc.lastRetentionCleanup.Load())
	require.Equal(t, 1, repo.cleanupDedupCalls)
}

func TestDashboardAggregationService_PartitionFailure_DoesNotAggregate(t *testing.T) {
	repo := &dashboardAggregationRepoTestStub{ensurePartitionErr: errors.New("partition failed")}
	svc := &DashboardAggregationService{
		repo: repo,
		cfg: config.DashboardAggregationConfig{
			Enabled:         true,
			IntervalSeconds: 60,
			LookbackSeconds: 120,
			Retention: config.DashboardAggregationRetentionConfig{
				UsageLogsDays:         1,
				UsageBillingDedupDays: 2,
				HourlyDays:            1,
				DailyDays:             1,
			},
		},
	}

	svc.runScheduledAggregation()

	require.Equal(t, 1, repo.ensurePartitionCalls)
	require.Equal(t, 1, repo.aggregateCalls)
}

func TestDashboardAggregationService_TriggerBackfill_TooLarge(t *testing.T) {
	repo := &dashboardAggregationRepoTestStub{}
	svc := &DashboardAggregationService{
		repo: repo,
		cfg: config.DashboardAggregationConfig{
			BackfillEnabled: true,
			BackfillMaxDays: 1,
		},
	}

	start := time.Now().AddDate(0, 0, -3)
	end := time.Now()
	err := svc.TriggerBackfill(start, end)
	require.ErrorIs(t, err, ErrDashboardBackfillTooLarge)
	require.Equal(t, 0, repo.aggregateCalls)
}

// ─────────────── 保留期协调：风险表清理与源日志清理同事务（U7） ───────────────

// fakeRetentionCoordinator 是 retentionCoordinator 的 fake 实现：以内存行集模拟
// 风险表/源日志删除，并记录调用序列与回滚/提交状态，供断言「同一事务、失败回滚、幂等」。
type fakeRetentionCoordinator struct {
	calls      []retentionCoordinatorCall
	failUsage  bool
	failRisk   bool
	committed  bool
	rolledBack bool
	// riskRows / usageRows 以 id 为键、以对照截止点的时间为值（report_date / created_at）。
	riskRows  map[int]time.Time
	usageRows map[int]time.Time
}

type retentionCoordinatorCall struct {
	riskEnabled bool
	usageCutoff time.Time
	riskCutoff  time.Time
}

func newFakeRetentionCoordinator() *fakeRetentionCoordinator {
	return &fakeRetentionCoordinator{
		riskRows:  make(map[int]time.Time),
		usageRows: make(map[int]time.Time),
	}
}

func (f *fakeRetentionCoordinator) RunRetentionCleanup(ctx context.Context, riskEnabled bool, usageCutoff, riskCutoff time.Time) error {
	f.calls = append(f.calls, retentionCoordinatorCall{riskEnabled: riskEnabled, usageCutoff: usageCutoff, riskCutoff: riskCutoff})
	if !riskEnabled {
		// 功能关闭分支：仅独立删源日志（service 实际不走此路径，保留以覆盖契约）。
		f.deleteUsage(usageCutoff)
		f.committed = true
		return nil
	}
	riskVictims := make([]int, 0)
	for id, t := range f.riskRows {
		if t.Before(riskCutoff) {
			riskVictims = append(riskVictims, id)
		}
	}
	usageVictims := make([]int, 0)
	for id, t := range f.usageRows {
		if t.Before(usageCutoff) {
			usageVictims = append(usageVictims, id)
		}
	}
	if f.failRisk {
		f.rolledBack = true
		return errors.New("risk cleanup failed")
	}
	if f.failUsage {
		f.rolledBack = true
		return errors.New("usage logs cleanup failed")
	}
	// 提交：真正删行（age 截止删除天然幂等）。
	for _, id := range riskVictims {
		delete(f.riskRows, id)
	}
	for _, id := range usageVictims {
		delete(f.usageRows, id)
	}
	f.committed = true
	return nil
}

func (f *fakeRetentionCoordinator) deleteUsage(cutoff time.Time) {
	for id, t := range f.usageRows {
		if t.Before(cutoff) {
			delete(f.usageRows, id)
		}
	}
}

// TestDashboardAggregationService_RetentionCoordinator_SameTxUsageFailureRollsBackRisk
// 断言：源日志删除失败 → 风险表删除一并回滚（同一事务语义）。
func TestDashboardAggregationService_RetentionCoordinator_SameTxUsageFailureRollsBackRisk(t *testing.T) {
	repo := &dashboardAggregationRepoTestStub{}
	fake := newFakeRetentionCoordinator()
	fake.failUsage = true
	now := time.Now().UTC()
	// 植入过期行（早于两个截止点，删除段会命中）。
	fake.riskRows[1] = now.AddDate(0, 0, -30)  // 早于 riskCutoff(now-10d)
	fake.usageRows[1] = now.AddDate(0, 0, -50) // 早于 usageCutoff(now-90d)

	svc := &DashboardAggregationService{
		repo:                 repo,
		retentionCoordinator: fake,
		usageRiskRetentionGate: func(ctx context.Context) (int, error) {
			return 10, nil
		},
		cfg: config.DashboardAggregationConfig{
			Retention: config.DashboardAggregationRetentionConfig{
				UsageLogsDays: 90,
				HourlyDays:    1,
				DailyDays:     1,
			},
		},
	}
	svc.maybeCleanupRetention(context.Background(), now)

	require.Len(t, fake.calls, 1, "协调器应被调用一次")
	require.True(t, fake.rolledBack, "源日志失败应触发回滚")
	require.False(t, fake.committed, "不应提交")
	// 风险表删除已回滚：行仍在。
	require.Len(t, fake.riskRows, 1, "风险表删除应被回滚（行未删）")
	require.Len(t, fake.usageRows, 1, "源日志删除应被回滚（行未删）")
	// 本轮不推进。
	require.Nil(t, svc.lastRetentionCleanup.Load())
	// 独立源日志清理未被调用（仅走协调器）。
	require.Equal(t, 0, repo.cleanupUsageCalls)
}

// TestDashboardAggregationService_RetentionCoordinator_DistinctCutoffs
// 断言：risk_days < usage_logs_days 时两个截止点各自计算且不同。
func TestDashboardAggregationService_RetentionCoordinator_DistinctCutoffs(t *testing.T) {
	repo := &dashboardAggregationRepoTestStub{}
	fake := newFakeRetentionCoordinator()
	now := time.Now().UTC()

	svc := &DashboardAggregationService{
		repo:                 repo,
		retentionCoordinator: fake,
		usageRiskRetentionGate: func(ctx context.Context) (int, error) {
			return 10, nil
		},
		cfg: config.DashboardAggregationConfig{
			Retention: config.DashboardAggregationRetentionConfig{
				UsageLogsDays: 90,
				HourlyDays:    1,
				DailyDays:     1,
			},
		},
	}
	svc.maybeCleanupRetention(context.Background(), now)

	require.Len(t, fake.calls, 1)
	call := fake.calls[0]
	riskCutoff := now.AddDate(0, 0, -10)
	usageCutoff := now.AddDate(0, 0, -90)
	require.WithinDuration(t, riskCutoff, call.riskCutoff, time.Second)
	require.WithinDuration(t, usageCutoff, call.usageCutoff, time.Second)
	require.True(t, call.riskCutoff.After(call.usageCutoff), "riskCutoff 应晚于 usageCutoff（保留更短）")
}

// TestDashboardAggregationService_RetentionCoordinator_DisabledStillCleansRisk
// 原语义：gate 返回 enabled=false → 协调器零调用、源日志独立清理。解耦改写后风险 TTL
// 不再依赖分析开关：本用例现锁定「Enabled=false 但策略可读时，风险报告/rollup 仍与源日志
// 在同一事务内清理（协调器被调用、源日志不走独立路径）」这一解耦「语义不降」。
// 「未接线 → 零协调器调用、源日志独立」的关闭语义由 GateNilDisabled 保留。
func TestDashboardAggregationService_RetentionCoordinator_DisabledStillCleansRisk(t *testing.T) {
	repo := &dashboardAggregationRepoTestStub{}
	fake := newFakeRetentionCoordinator()
	now := time.Now().UTC()

	svc := &DashboardAggregationService{
		repo:                 repo,
		retentionCoordinator: fake,
		usageRiskRetentionGate: func(ctx context.Context) (int, error) {
			// 模拟 settings 中 usage_risk_enabled=false，但保留期策略可读（RetentionDays 有效）。
			return 10, nil
		},
		cfg: config.DashboardAggregationConfig{
			Retention: config.DashboardAggregationRetentionConfig{
				UsageLogsDays: 90,
				HourlyDays:    1,
				DailyDays:     1,
			},
		},
	}
	svc.maybeCleanupRetention(context.Background(), now)

	require.Len(t, fake.calls, 1, "Enabled=false 但策略可读时协调器仍应被调用（风险表与源日志同事务清理）")
	require.Equal(t, 0, repo.cleanupUsageCalls, "风险清理同事务进行，不应再独立清理源日志")
	call := fake.calls[0]
	riskCutoff := now.AddDate(0, 0, -10)
	require.WithinDuration(t, riskCutoff, call.riskCutoff, time.Second, "风险截止点应按保留期天数计算")
}

// TestDashboardAggregationService_RetentionCoordinator_GateErrorFailsClosed
// 断言：gate 读取策略出错 → 本轮两清全部失败关闭（源日志与风险表均不清理），下轮幂等重试。
// 不得降级为「只清源」或「只清风险」（禁区要求）。
func TestDashboardAggregationService_RetentionCoordinator_GateErrorFailsClosed(t *testing.T) {
	repo := &dashboardAggregationRepoTestStub{}
	fake := newFakeRetentionCoordinator()
	now := time.Now().UTC()

	svc := &DashboardAggregationService{
		repo:                 repo,
		retentionCoordinator: fake,
		usageRiskRetentionGate: func(ctx context.Context) (int, error) {
			return 0, errors.New("load usage_risk policy: connection refused")
		},
		cfg: config.DashboardAggregationConfig{
			Retention: config.DashboardAggregationRetentionConfig{
				UsageLogsDays: 90,
				HourlyDays:    1,
				DailyDays:     1,
			},
		},
	}
	svc.maybeCleanupRetention(context.Background(), now)

	require.Len(t, fake.calls, 0, "gate 出错时协调器不应被调用（风险表不清理）")
	require.Equal(t, 0, repo.cleanupUsageCalls, "gate 出错时不得降级为只清源日志")
	require.Nil(t, svc.lastRetentionCleanup.Load(), "本轮不推进，下轮幂等重试")
}

// TestDashboardAggregationService_RetentionCoordinator_GateNilDisabled
// 断言：gate 未注入（U4 接线点未收口）时安全降级为功能关闭，独立清理源日志。
func TestDashboardAggregationService_RetentionCoordinator_GateNilDisabled(t *testing.T) {
	repo := &dashboardAggregationRepoTestStub{}
	fake := newFakeRetentionCoordinator()
	now := time.Now().UTC()

	svc := &DashboardAggregationService{
		repo:                 repo,
		retentionCoordinator: fake,
		// usageRiskRetentionGate 未注入
		cfg: config.DashboardAggregationConfig{
			Retention: config.DashboardAggregationRetentionConfig{
				UsageLogsDays: 90,
				HourlyDays:    1,
				DailyDays:     1,
			},
		},
	}
	svc.maybeCleanupRetention(context.Background(), now)

	require.Len(t, fake.calls, 0)
	require.Equal(t, 1, repo.cleanupUsageCalls)
}

// TestDashboardAggregationService_RetentionCoordinator_IdempotentRetry
// 断言：同 cutoff 重跑不重复删（age 截止删除天然幂等）。
func TestDashboardAggregationService_RetentionCoordinator_IdempotentRetry(t *testing.T) {
	fake := newFakeRetentionCoordinator()
	now := time.Now().UTC()
	riskCutoff := now.AddDate(0, 0, -10)
	usageCutoff := now.AddDate(0, 0, -90)
	// 植入过期风险行与源日志行（均早于各自截止点）。
	fake.riskRows[1] = now.AddDate(0, 0, -30) // 早于 riskCutoff(now-10d)
	fake.riskRows[2] = now.AddDate(0, 0, -40)
	fake.usageRows[1] = now.AddDate(0, 0, -100) // 早于 usageCutoff(now-90d)
	fake.usageRows[2] = now.AddDate(0, 0, -120)

	// 第一次运行：删除 2 行风险 + 2 行源日志。
	require.NoError(t, fake.RunRetentionCleanup(context.Background(), true, usageCutoff, riskCutoff))
	require.Len(t, fake.riskRows, 0)
	require.Len(t, fake.usageRows, 0)

	// 第二次运行（同 cutoff）：应为幂等，删除 0 行、不报错。
	require.NoError(t, fake.RunRetentionCleanup(context.Background(), true, usageCutoff, riskCutoff))
	require.Len(t, fake.riskRows, 0)
	require.Len(t, fake.usageRows, 0)

	// 新植入「未过期」行：不应被同 cutoff 删除。
	fake.riskRows[3] = now.AddDate(0, 0, -5) // 晚于 riskCutoff
	fake.usageRows[3] = now.AddDate(0, 0, -5) // 晚于 usageCutoff
	require.NoError(t, fake.RunRetentionCleanup(context.Background(), true, usageCutoff, riskCutoff))
	require.Len(t, fake.riskRows, 1)
	require.Len(t, fake.usageRows, 1)
}
