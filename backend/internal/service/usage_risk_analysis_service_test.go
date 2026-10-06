package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/stretchr/testify/require"
)

// ───────────────────────── 测试桩：UsageRiskRepository ─────────────────────────

// fakeUsageRiskRepo 实现 service.UsageRiskRepository，按测试需要录制调用并返回可配置结果。
// 未被测试显式配置的方法返回零值，保证接口完整性。
type fakeUsageRiskRepo struct {
	// 可配置返回
	getReportDetailResult *UsageRiskReportDetail
	getReportDetailErr    error
	updateReportStatusOK  bool
	updateReportStatusErr error
	// updateStatusWithAuditOK 控制 UpdateStatusWithAudit 的返回；为真时记录 entry 到 auditEntries（模拟成功落账）。
	updateStatusWithAuditOK  bool
	updateStatusWithAuditErr error
	auditEntries            []*AuditLog
	latestRunResult         *UsageRiskRunStatus
	latestCursorResult      *UsageRiskReconCursor
	listReportsResult       []UsageRiskReportItem
	listReportsTotal        int
	riskSummaryResult       *UsageRiskSummary
	aggregateHourlyFn       func(dayStart time.Time) []UsageRiskHourAggregate
	userActiveBucketHoursFn func(userID int64, lookbackStart, reportDay time.Time) []time.Time
	reconcileCalls          int
	upsertDeriveCalls       int
	createRunID             int64

	// 录制字段（供锁定测试断言副作用顺序与计数）。
	callOrder                []string // 方法调用名顺序（B/F2 顺序重排断言）
	createRunCalls           int
	rebuildHourlyCalls       int
	invalidateCalls          int // InvalidateOutsideKeySet 调用次数（F4 部分键集不失效断言）
	completeRunCalls         int
	updateProgressCalls      int
	updateProgressHistory    []UsageRiskRunProgress // 进度持久化按调用顺序录制（R8-2b 锁定测试）
	updateProgressCtxDeadlines []time.Time // F1：每次进度写收到 ctx 的 deadline（证明每写新建 ctx）
	completeRunCtxDeadlines []time.Time // F1：CompleteRun 收到 ctx 的 deadline
	completeRunStatus        string
	completeRunProgress      UsageRiskRunProgress
	completeRunCtxErr        error // CompleteRun 调用时 ctx.Err()（N/F18 断言收尾 ctx 独立性）
	invalidatedDays          []time.Time // F：InvalidateOutsideKeySet 传入的 day 参数录制（上界扩展断言）
	invalidateErr            error
	invalidateAllCalls       int    // InvalidateAllEffective 调用次数（R13-3：时区迁移单一 SQL 全量失效）
	invalidateAllErr         error // InvalidateAllEffective 错误注入（迁移失败关闭路径）
	rebuildHourlyErr         error
	rebuildHourlyWindows     []struct{ Start, End time.Time } // I：回填窗口录制（含当日、不含次日）
	listCandidatesResult     []UsageRiskCandidate
	distinctIPsErr           error
	userActiveBucketHoursCalls int
}

// record 追加方法调用名到 callOrder（保证顺序断言在接口漂移时仍有效）。
func (f *fakeUsageRiskRepo) record(name string) {
	f.callOrder = append(f.callOrder, name)
}

func (f *fakeUsageRiskRepo) AggregateHourly(ctx context.Context, windowStart, windowEnd time.Time, p AggregateParams) ([]UsageRiskHourAggregate, error) {
	if f.aggregateHourlyFn != nil {
		return f.aggregateHourlyFn(windowStart), nil
	}
	return nil, nil
}

func (f *fakeUsageRiskRepo) AggregateGroupRPMMinutes(ctx context.Context, windowStart, windowEnd time.Time, p AggregateParams, limits []UsageRiskRPMLimit, ratio float64) (map[UsageRiskUserGroupKey]int, error) {
	return map[UsageRiskUserGroupKey]int{}, nil
}

func (f *fakeUsageRiskRepo) AggregateUserGlobalRPMMinutes(ctx context.Context, windowStart, windowEnd time.Time, limits []UsageRiskUserLimit, ratio float64) (map[int64]int, error) {
	return map[int64]int{}, nil
}

func (f *fakeUsageRiskRepo) ListCandidates(ctx context.Context, windowStart, windowEnd time.Time, p AggregateParams, minDailyRequests, offset, batchSize int) ([]UsageRiskCandidate, error) {
	if f.listCandidatesResult != nil {
		return f.listCandidatesResult, nil
	}
	return nil, nil
}

func (f *fakeUsageRiskRepo) RebuildHourlyWindow(ctx context.Context, windowStart, windowEnd time.Time, p AggregateParams) (int64, error) {
	f.rebuildHourlyCalls++
	f.record("RebuildHourlyWindow")
	if f.rebuildHourlyErr != nil {
		return 0, f.rebuildHourlyErr
	}
	f.rebuildHourlyWindows = append(f.rebuildHourlyWindows, struct{ Start, End time.Time }{windowStart, windowEnd})
	return 0, nil
}

func (f *fakeUsageRiskRepo) DistinctIPs(ctx context.Context, userID int64, dayStart, dayEnd time.Time, p AggregateParams) ([]string, error) {
	if f.distinctIPsErr != nil {
		return nil, f.distinctIPsErr
	}
	return nil, nil
}

func (f *fakeUsageRiskRepo) IPAssociatedUsers(ctx context.Context, userID int64, dayStart, dayEnd time.Time, p AggregateParams) (map[string]IPClusterFact, error) {
	return map[string]IPClusterFact{}, nil
}

func (f *fakeUsageRiskRepo) UADistribution(ctx context.Context, userID int64, dayStart, dayEnd time.Time, p AggregateParams) (map[string]int, error) {
	return map[string]int{}, nil
}

func (f *fakeUsageRiskRepo) KeyUsageDistribution(ctx context.Context, userID int64, dayStart, dayEnd time.Time, p AggregateParams) (map[string]int, error) {
	return map[string]int{}, nil
}

func (f *fakeUsageRiskRepo) ActiveHourHeatmap(ctx context.Context, userID int64, dayStart, dayEnd time.Time, tzName string, p AggregateParams) (map[int]int, error) {
	return map[int]int{}, nil
}

func (f *fakeUsageRiskRepo) R2PeerDayCosts(ctx context.Context, groupID int64, dayStart, dayEnd time.Time, p AggregateParams, minDailyRequests int) ([]UsageRiskPeerCost, error) {
	return nil, nil
}

func (f *fakeUsageRiskRepo) UserActiveBucketHours(ctx context.Context, userID int64, lookbackStart, reportDay time.Time) ([]time.Time, error) {
	f.userActiveBucketHoursCalls++
	if f.userActiveBucketHoursFn != nil {
		return f.userActiveBucketHoursFn(userID, lookbackStart, reportDay), nil
	}
	return nil, nil
}

func (f *fakeUsageRiskRepo) UpdateReportStatus(ctx context.Context, reportID int64, oldStatus, newStatus string, adminID int64, at time.Time) (bool, error) {
	return f.updateReportStatusOK, f.updateReportStatusErr
}

// UpdateStatusWithAudit 仅在成功（updateStatusWithAuditOK）时把 entry 记入 auditEntries，
// 模拟"与状态变更同事务成功落账"；失败/并发冲突（ok=false）不写审计，对应"零审计调用"断言。
func (f *fakeUsageRiskRepo) UpdateStatusWithAudit(ctx context.Context, reportID int64, oldStatus, newStatus string, adminID int64, at time.Time, entry *AuditLog) (bool, error) {
	if f.updateStatusWithAuditErr != nil {
		return false, f.updateStatusWithAuditErr
	}
	if f.updateStatusWithAuditOK {
		f.auditEntries = append(f.auditEntries, entry)
	}
	return f.updateStatusWithAuditOK, nil
}

func (f *fakeUsageRiskRepo) ReconcileReportBatchUPSERTOnly(ctx context.Context, reportDate time.Time, records []UsageRiskReportRecord) error {
	f.reconcileCalls++
	return nil
}

func (f *fakeUsageRiskRepo) InvalidateOutsideKeySet(ctx context.Context, reportDate time.Time, userIDs, groupIDs []int64) error {
	f.invalidateCalls++
	f.invalidatedDays = append(f.invalidatedDays, reportDate)
	f.record("InvalidateOutsideKeySet")
	if f.invalidateErr != nil {
		return f.invalidateErr
	}
	return nil
}

func (f *fakeUsageRiskRepo) InvalidateAllEffective(ctx context.Context) error {
	f.invalidateAllCalls++
	f.record("InvalidateAllEffective")
	if f.invalidateAllErr != nil {
		return f.invalidateAllErr
	}
	return nil
}

func (f *fakeUsageRiskRepo) CreateRun(ctx context.Context, r UsageRiskRunRecord) (int64, error) {
	f.createRunCalls++
	f.record("CreateRun")
	return f.createRunID, nil
}

func (f *fakeUsageRiskRepo) UpdateRunProgress(ctx context.Context, runID int64, p UsageRiskRunProgress) error {
	// R8-2b/F1：ctx 已失效（共享过期 ctx 的旧实现症状）必须表现为写失败——锁定测试据此断言每写新建 ctx。
	if ctx.Err() != nil {
		f.updateProgressCalls++
		return ctx.Err()
	}
	f.updateProgressCalls++
	f.updateProgressHistory = append(f.updateProgressHistory, p)
	if dl, ok := ctx.Deadline(); ok {
		f.updateProgressCtxDeadlines = append(f.updateProgressCtxDeadlines, dl)
	}
	f.record("UpdateRunProgress")
	return nil
}

func (f *fakeUsageRiskRepo) CompleteRun(ctx context.Context, runID int64, status string, p UsageRiskRunProgress) error {
	// R8-2b/F1：同上——收尾写若拿到已过期 ctx（旧实现贯穿单次创建）即失败，锁定每写新建 ctx。
	if ctx.Err() != nil {
		f.completeRunCalls++
		return ctx.Err()
	}
	f.completeRunCalls++
	f.completeRunCtxErr = ctx.Err()
	if dl, ok := ctx.Deadline(); ok {
		f.completeRunCtxDeadlines = append(f.completeRunCtxDeadlines, dl)
	}
	f.completeRunStatus = status
	f.completeRunProgress = p
	f.record("CompleteRun")
	return nil
}

func (f *fakeUsageRiskRepo) ConvergeStaleRuns(ctx context.Context, staleBefore time.Time, failureStage string) error {
	f.record("ConvergeStaleRuns")
	return nil
}

func (f *fakeUsageRiskRepo) LatestRun(ctx context.Context) (*UsageRiskRunStatus, error) {
	f.record("LatestRun")
	return f.latestRunResult, nil
}

func (f *fakeUsageRiskRepo) LoadLatestReconCursor(ctx context.Context) (*UsageRiskReconCursor, error) {
	f.record("LoadLatestReconCursor")
	return f.latestCursorResult, nil
}

func (f *fakeUsageRiskRepo) ListReports(ctx context.Context, flt UsageRiskListFilter, minScore int) ([]UsageRiskReportItem, int, error) {
	return f.listReportsResult, f.listReportsTotal, nil
}

func (f *fakeUsageRiskRepo) GetReportDetail(ctx context.Context, reportID int64) (*UsageRiskReportDetail, error) {
	if f.getReportDetailErr != nil {
		return nil, f.getReportDetailErr
	}
	return f.getReportDetailResult, nil
}

func (f *fakeUsageRiskRepo) RiskSummary(ctx context.Context, minScore, topN int) (*UsageRiskSummary, error) {
	return f.riskSummaryResult, nil
}

// 断言 fakeUsageRiskRepo 满足接口（契约漂移会在此暴露）。
var _ UsageRiskRepository = (*fakeUsageRiskRepo)(nil)

// ───────────────────────── 测试辅助 ─────────────────────────

func newTestUsageRiskService(repo UsageRiskRepository) *UsageRiskAnalysisService {
	fixed := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	return &UsageRiskAnalysisService{
		repo:      repo,
		nowFn:     func() time.Time { return fixed },
		policyFn:  func(context.Context) (UsageRiskPolicy, error) { return UsageRiskPolicy{ListingMinScore: 40}, nil },
		instanceID: "test-instance",
		interval:  time.Hour,
		budget:    4 * time.Minute,
	}
}

// ───────────────────────── 纯逻辑单测 ─────────────────────────

func TestUsageRiskStateMachine(t *testing.T) {
	cases := []struct {
		from, to string
		want     bool
	}{
		{statusUsageRiskOpen, statusUsageRiskAcknowledged, true},
		{statusUsageRiskOpen, statusUsageRiskDismissed, true},
		{statusUsageRiskOpen, statusUsageRiskResolved, false},
		{statusUsageRiskAcknowledged, statusUsageRiskResolved, true},
		{statusUsageRiskAcknowledged, statusUsageRiskDismissed, true},
		{statusUsageRiskAcknowledged, statusUsageRiskOpen, false},
		{statusUsageRiskResolved, statusUsageRiskAcknowledged, false},
		{statusUsageRiskDismissed, statusUsageRiskOpen, false},
		{statusUsageRiskResolved, statusUsageRiskResolved, false}, // 同态
	}
	for _, c := range cases {
		require.Equal(t, c.want, isValidTransition(c.from, c.to), "%s->%s", c.from, c.to)
	}
}

func TestUsageRiskDaysBetween(t *testing.T) {
	a := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	b := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	require.Equal(t, 9, daysBetween(a, b))
	require.Equal(t, 0, daysBetween(b, a)) // b 不晚于 a
	require.Equal(t, 0, daysBetween(a, a))
}

func TestUsageRiskDayElapsedHours(t *testing.T) {
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	require.InDelta(t, 24.0, dayElapsedHours(day), 1e-9)
}

func TestUsageRiskAggParams(t *testing.T) {
	p := UsageRiskPolicy{UnlimitedGroupsOnly: true, UAWhitelist: []string{"a", "b"}}
	ap := aggParams(p)
	require.True(t, ap.UnlimitedGroupsOnly)
	require.Equal(t, []string{"a", "b"}, ap.UAWhitelist)
}

func TestUsageRiskBuildDayAggMap(t *testing.T) {
	day := timezone.StartOfDay(time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC))
	hourly := []UsageRiskHourAggregate{
		{UserID: 1, GroupID: 2, BucketHour: day.Add(3 * time.Hour), RequestCount: 5, InputTokensSum: 100, OutputTokensSum: 50, CostUSDSum: 0.01, OccupiedMsSum: 1000},
		{UserID: 1, GroupID: 2, BucketHour: day.Add(5 * time.Hour), RequestCount: 3, InputTokensSum: 40, OutputTokensSum: 20, CostUSDSum: 0.02, OccupiedMsSum: 500},
		{UserID: 1, GroupID: 3, BucketHour: day.Add(4 * time.Hour), RequestCount: 1, InputTokensSum: 10, OutputTokensSum: 5, CostUSDSum: 0.005, OccupiedMsSum: 100},
	}
	m := buildDayAggMap(hourly)
	require.Len(t, m, 2)

	key12 := UsageRiskUserGroupKey{UserID: 1, GroupID: 2}
	da, ok := m[key12]
	require.True(t, ok)
	require.Equal(t, 8, da.RequestCount)
	require.Equal(t, int64(140), da.InputTokensSum)
	require.Equal(t, int64(70), da.OutputTokensSum)
	require.InDelta(t, 0.03, da.CostUSDSum, 1e-9)
	require.Equal(t, int64(1500), da.OccupiedMsSum)
	require.Len(t, da.BucketInstants, 2)
}

// ───────────────────────── 编排/对外 API 单测 ─────────────────────────

func TestUsageRiskUpdateStatus(t *testing.T) {
	t.Run("legal transition succeeds and writes audit in same tx", func(t *testing.T) {
		repo := &fakeUsageRiskRepo{
			getReportDetailResult: &UsageRiskReportDetail{
				UsageRiskReportItem: UsageRiskReportItem{ReportID: 1, Status: statusUsageRiskOpen},
			},
			updateStatusWithAuditOK: true,
		}
		svc := newTestUsageRiskService(repo)
		err := svc.UpdateStatus(context.Background(), 1, statusUsageRiskAcknowledged, 99)
		require.NoError(t, err)
		// 合法转移：审计 entry 已构造并同事务落账（action/extra 字段断言）。
		require.Len(t, repo.auditEntries, 1, "合法转移必须写一条审计")
		entry := repo.auditEntries[0]
		require.Equal(t, "admin.usage_risk.status", entry.Action)
		require.Equal(t, "POST", entry.Method)
		require.Equal(t, "/api/v1/admin/usage-risk/reports/1/status", entry.Path)
		require.Equal(t, 200, entry.StatusCode)
		require.Equal(t, int64(99), *entry.ActorUserID)
		require.Equal(t, "open", entry.Extra["old_status"])
		require.Equal(t, "acknowledged", entry.Extra["new_status"])
		require.Equal(t, int64(1), entry.Extra["report_id"])
	})

	t.Run("illegal transition rejected without audit", func(t *testing.T) {
		repo := &fakeUsageRiskRepo{
			getReportDetailResult: &UsageRiskReportDetail{
				UsageRiskReportItem: UsageRiskReportItem{ReportID: 1, Status: statusUsageRiskOpen},
			},
		}
		svc := newTestUsageRiskService(repo)
		err := svc.UpdateStatus(context.Background(), 1, statusUsageRiskResolved, 99)
		require.ErrorIs(t, err, ErrUsageRiskIllegalTransition)
		require.Empty(t, repo.auditEntries, "非法转移不得写审计")
	})

	t.Run("same status is no-op without audit", func(t *testing.T) {
		repo := &fakeUsageRiskRepo{
			getReportDetailResult: &UsageRiskReportDetail{
				UsageRiskReportItem: UsageRiskReportItem{ReportID: 1, Status: statusUsageRiskOpen},
			},
		}
		svc := newTestUsageRiskService(repo)
		err := svc.UpdateStatus(context.Background(), 1, statusUsageRiskOpen, 99)
		require.NoError(t, err)
		require.Empty(t, repo.auditEntries, "同态 no-op 不得写审计")
	})

	t.Run("not found propagated without audit", func(t *testing.T) {
		repo := &fakeUsageRiskRepo{getReportDetailErr: ErrUsageRiskReportNotFound}
		svc := newTestUsageRiskService(repo)
		err := svc.UpdateStatus(context.Background(), 42, statusUsageRiskAcknowledged, 99)
		require.ErrorIs(t, err, ErrUsageRiskReportNotFound)
		require.Empty(t, repo.auditEntries, "未命中报告不得写审计")
	})

	t.Run("concurrent conflict surfaces as illegal with zero audit", func(t *testing.T) {
		repo := &fakeUsageRiskRepo{
			getReportDetailResult: &UsageRiskReportDetail{
				UsageRiskReportItem: UsageRiskReportItem{ReportID: 1, Status: statusUsageRiskOpen},
			},
			updateStatusWithAuditOK: false, // 0 行影响 = 并发冲突，事务内不写审计
		}
		svc := newTestUsageRiskService(repo)
		err := svc.UpdateStatus(context.Background(), 1, statusUsageRiskAcknowledged, 99)
		require.ErrorIs(t, err, ErrUsageRiskIllegalTransition)
		require.Empty(t, repo.auditEntries, "并发冲突（0 行）不得写审计")
	})
}

func TestUsageRiskGetRunStatus(t *testing.T) {
	we := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	repo := &fakeUsageRiskRepo{
		latestRunResult: &UsageRiskRunStatus{Status: "completed", WindowEnd: &we, HistoryCovered: true},
	}
	svc := newTestUsageRiskService(repo)
	st, err := svc.GetRunStatus(context.Background())
	require.NoError(t, err)
	require.Equal(t, "completed", st.Status)
	require.True(t, st.HistoryCovered)
	require.NotNil(t, st.WindowEnd)
}

func TestUsageRiskGetRunStatusIdle(t *testing.T) {
	repo := &fakeUsageRiskRepo{latestRunResult: nil}
	svc := newTestUsageRiskService(repo)
	st, err := svc.GetRunStatus(context.Background())
	require.NoError(t, err)
	require.Equal(t, "idle", st.Status)
}

func TestUsageRiskListReports(t *testing.T) {
	repo := &fakeUsageRiskRepo{
		listReportsResult: []UsageRiskReportItem{{ReportID: 1, Score: 50}},
		listReportsTotal:  1,
	}
	svc := newTestUsageRiskService(repo)
	got, err := svc.ListReports(context.Background(), UsageRiskListFilter{Page: 1, PageSize: 20})
	require.NoError(t, err)
	require.Equal(t, 1, got.Total)
	require.Len(t, got.Items, 1)
}

func TestUsageRiskGetReport(t *testing.T) {
	repo := &fakeUsageRiskRepo{
		getReportDetailResult: &UsageRiskReportDetail{
			UsageRiskReportItem: UsageRiskReportItem{ReportID: 7, Status: statusUsageRiskOpen},
			Evidence:           json.RawMessage(`{}`),
			PolicyVersion:      "1",
		},
	}
	svc := newTestUsageRiskService(repo)
	detail, err := svc.GetReport(context.Background(), 7)
	require.NoError(t, err)
	require.Equal(t, int64(7), detail.ReportID)
	require.Equal(t, "1", detail.PolicyVersion)
}

func TestUsageRiskGetRiskSummary(t *testing.T) {
	repo := &fakeUsageRiskRepo{
		riskSummaryResult: &UsageRiskSummary{OpenTotal: 3, ByLevel: map[string]int{"high": 3}},
	}
	svc := newTestUsageRiskService(repo)
	sum, err := svc.GetRiskSummary(context.Background())
	require.NoError(t, err)
	require.Equal(t, 3, sum.OpenTotal)
}

func TestUsageRiskGetRiskSummaryErrorState(t *testing.T) {
	// 失败关闭态：API 不报错，但 Summary.Error 暴露配置失败。
	fixed := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	svc := &UsageRiskAnalysisService{
		repo:        &fakeUsageRiskRepo{},
		nowFn:       func() time.Time { return fixed },
		failedClosed: true,
		failedErr:   errors.New("retention binding violated"),
		instanceID:  "test",
		interval:    time.Hour,
		budget:      4 * time.Minute,
	}
	sum, err := svc.GetRiskSummary(context.Background())
	require.NoError(t, err)
	require.Contains(t, sum.Error, "retention binding violated")
}

// TestUsageRiskComputeConsecutiveActiveDays 验证 R1 连续活跃天数按回看窗口逐日统计、
// 遇中断即停止（与 U4a R1HistoryCovered 口径一致）。I：统计基于 rollup distinct 桶
// （UserActiveBucketHours），而非逐日 AggregateHourly 重聚合。
func TestUsageRiskComputeConsecutiveActiveDays(t *testing.T) {
	reportDay := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	dayStart := timezone.StartOfDay(reportDay)
	// 配置：回看 4 天，前 3 天活跃（>=2 桶）、第 4 天不活跃 → 期望 3。
	// 直接提供 rollup 的 distinct 活跃桶（跨 dayStart 前 3 天，每天 2 桶），第 4 天无桶 → 中断。
	repo := &fakeUsageRiskRepo{
		userActiveBucketHoursFn: func(userID int64, lookbackStart, rd time.Time) []time.Time {
			return []time.Time{
				dayStart.AddDate(0, 0, -1).Add(1 * time.Hour), dayStart.AddDate(0, 0, -1).Add(3 * time.Hour),
				dayStart.AddDate(0, 0, -2).Add(1 * time.Hour), dayStart.AddDate(0, 0, -2).Add(3 * time.Hour),
				dayStart.AddDate(0, 0, -3).Add(1 * time.Hour), dayStart.AddDate(0, 0, -3).Add(3 * time.Hour),
			}
		},
	}
	svc := newTestUsageRiskService(repo)
	policy := UsageRiskPolicy{R1ConsecutiveDays: 3, R1ActiveHours: 2}
	got, err := svc.computeConsecutiveActiveDays(context.Background(), 1, dayStart, policy)
	require.NoError(t, err)
	require.Equal(t, 3, got)
}

// TestUsageRiskUserActiveBucketHoursReadsRollup 验证 I：连续天数统计走 rollup 读取
// （UserActiveBucketHours），不再逐日重聚合 AggregateHourly。
func TestUsageRiskUserActiveBucketHoursReadsRollup(t *testing.T) {
	reportDay := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	dayStart := timezone.StartOfDay(reportDay)
	calls := 0
	repo := &fakeUsageRiskRepo{
		userActiveBucketHoursFn: func(userID int64, lookbackStart, rd time.Time) []time.Time {
			calls++
			// 断言读取区间覆盖回看窗口起点（reportDay - (R1ConsecutiveDays+1) 天）。
			require.True(t, !lookbackStart.Before(dayStart.AddDate(0, 0, -(3+1))), "lookback 起点须覆盖回看窗口")
			return []time.Time{
				dayStart.AddDate(0, 0, -1).Add(2 * time.Hour),
			}
		},
	}
	svc := newTestUsageRiskService(repo)
	policy := UsageRiskPolicy{R1ConsecutiveDays: 3, R1ActiveHours: 1}
	got, err := svc.computeConsecutiveActiveDays(context.Background(), 1, dayStart, policy)
	require.NoError(t, err)
	require.Equal(t, 1, got, "近 1 天活跃 → 连续 1 天")
	require.Equal(t, 1, calls, "I：仅一次 rollup 读取，不得逐日 AggregateHourly 重聚合")
}

// TestUsageRiskParsePolicyVersionHexBitFidelity 验证 A/F1：策略版本位宽保护。
// ≥2^63（高位被置位）时旧 ParseInt 会 err 并静默成 0——本实现用 ParseUint + int64(u) 位保真，
// 且解析失败（非法 16 进制）必须返回错误（调用方失败关闭），绝不静默 0。
func TestUsageRiskParsePolicyVersionHexBitFidelity(t *testing.T) {
	// 恰好 2^63（0x8000000000000000）：uint64 = 9223372036854775808，int64 保位 = math.MinInt64（-9223372036854775808）。
	const highBit = "8000000000000000"
	v, err := parsePolicyVersionHex(highBit)
	require.NoError(t, err)
	require.Equal(t, int64(-9223372036854775808), v, "≥2^63 必须位保真（不能静默 0）")
	require.NotEqual(t, int64(0), v, "绝不允许 ≥2^63 被静默成 0")

	// 全 1（0xFFFFFFFFFFFFFFFF）：uint64 = 18446744073709551615，int64 = -1（位保真）。
	vmax, err := parsePolicyVersionHex("ffffffffffffffff")
	require.NoError(t, err)
	require.Equal(t, int64(-1), vmax, "全 1 保位应为 -1")
	require.NotEqual(t, int64(0), vmax)

	// 普通小值仍正确。
	vsmall, err := parsePolicyVersionHex("1a2b3c4d5e6f7081")
	require.NoError(t, err)
	require.Equal(t, int64(0x1a2b3c4d5e6f7081), vsmall)

	// 非法 16 进制必须报错（调用方失败关闭路径依赖此错误，绝不能吞）。
	_, err = parsePolicyVersionHex("not-hex!!!")
	require.Error(t, err, "非法版本串必须返回错误而非静默 0")
}

// ───────────────────────── 新增锁定测试（R5-1 A–N） ─────────────────────────

// newSvcWithPolicy 构造带可注入策略的测试服务（client/userGroupRateRepo 为 nil，
// 限额读取路径安全返回零值，不触发 nil 解引用）。
func newSvcWithPolicy(repo UsageRiskRepository, policy UsageRiskPolicy) *UsageRiskAnalysisService {
	fixed := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	return &UsageRiskAnalysisService{
		repo:       repo,
		nowFn:      func() time.Time { return fixed },
		policyFn:   func(context.Context) (UsageRiskPolicy, error) { return policy, nil },
		instanceID: "test-instance",
		interval:   time.Hour,
		budget:     4 * time.Minute,
	}
}

// indexOf 返回 callOrder 中首个匹配方法名的位置（-1 表示未出现）。
func indexOf(order []string, name string) int {
	for i, n := range order {
		if n == name {
			return i
		}
	}
	return -1
}

// TestUsageRiskRunAnalysisReadsPrevBeforeCreate 锁 B/F2：游标读到的是自己（R3 活锁根因）。
// 必须在 CreateRun 之前读取上一轮（LatestRun + LoadLatestReconCursor），否则会读到本轮刚插入的 running 行。
func TestUsageRiskRunAnalysisReadsPrevBeforeCreate(t *testing.T) {
	repo := &fakeUsageRiskRepo{
		latestRunResult: &UsageRiskRunStatus{Status: "completed", HistoryCovered: true},
	}
	svc := newSvcWithPolicy(repo, UsageRiskPolicy{Enabled: true, RetentionDays: 0})
	require.NoError(t, svc.runAnalysis(context.Background(), svc.now()))

	idxCreate := indexOf(repo.callOrder, "CreateRun")
	idxLatest := indexOf(repo.callOrder, "LatestRun")
	idxCursor := indexOf(repo.callOrder, "LoadLatestReconCursor")
	require.Greater(t, idxCreate, idxLatest, "F2/B：LatestRun 须在 CreateRun 之前（不得读到本轮自己）")
	require.Greater(t, idxCreate, idxCursor, "F2/B：LoadLatestReconCursor 须在 CreateRun 之前")
	require.Greater(t, idxLatest, -1, "F2/B：应读取上一轮 run")
	require.Greater(t, idxCursor, -1, "F2/B：应读取上一轮对账游标")
}

// TestUsageRiskRunAnalysisDisabledZeroSideEffect 锁 C/F3：全局开关关闭零 DB 副作用。
func TestUsageRiskRunAnalysisDisabledZeroSideEffect(t *testing.T) {
	repo := &fakeUsageRiskRepo{}
	svc := newSvcWithPolicy(repo, UsageRiskPolicy{Enabled: false, RetentionDays: 0})
	require.NoError(t, svc.runAnalysis(context.Background(), svc.now()))
	require.Equal(t, 0, repo.createRunCalls, "F3/C：关闭时不得建 run")
	require.Equal(t, 0, repo.rebuildHourlyCalls, "F3/C：关闭时不得重建小时桶")
	require.Equal(t, 0, repo.invalidateCalls, "F3/C：关闭时不得产报告/失效")
	require.Empty(t, repo.callOrder, "F3/C：关闭时连 LatestRun 都不应读取（零副作用）")
}

// TestUsageRiskReconcileDayBuildFailureNoInvalidate 锁 D/F4：候选构建失败立即以该批为失败单元返回，
// 当日不触发失效（部分键集永不失效的不变量保持）。
func TestUsageRiskReconcileDayBuildFailureNoInvalidate(t *testing.T) {
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	repo := &fakeUsageRiskRepo{
		aggregateHourlyFn: func(ds time.Time) []UsageRiskHourAggregate {
			return []UsageRiskHourAggregate{{UserID: 1, GroupID: 2, BucketHour: ds.Add(3 * time.Hour), RequestCount: 5}}
		},
		listCandidatesResult: []UsageRiskCandidate{{UserID: 1, GroupID: 2, RequestCount: 5}},
		distinctIPsErr:       errors.New("phase2 unavailable"),
	}
	svc := newSvcWithPolicy(repo, UsageRiskPolicy{Enabled: true, MinDailyRequests: 1})
	progress := UsageRiskRunProgress{}
	batches, _, dayComplete, _, err := svc.reconcileDay(context.Background(), 1, day, 0, UsageRiskPolicy{MinDailyRequests: 1}, "UTC", 1, false, &progress)
	require.Error(t, err, "F4/D：构建失败应返回错误（以该批为失败单元）")
	require.False(t, dayComplete, "F4/D：构建失败当日未完成（不触发失效）")
	require.Equal(t, 0, batches)
	require.Equal(t, 0, repo.invalidateCalls, "F4/D：构建失败当日不得调用 InvalidateOutsideKeySet（部分键集永不失效）")
}

// TestUsageRiskHistoryCoveredFlipRequiresNoFailedBatches 锁 E/F5：覆盖翻转判据新增
// “失败批次为空”——失败批次下轮优先重试，覆盖完成前不得翻转。
func TestUsageRiskHistoryCoveredFlipRequiresNoFailedBatches(t *testing.T) {
	t.Run("no failed batches -> flips to covered", func(t *testing.T) {
		policy := UsageRiskPolicy{Enabled: true, RetentionDays: 0, MinDailyRequests: 1}
		repo := &fakeUsageRiskRepo{
			latestRunResult:      &UsageRiskRunStatus{Status: "partial", HistoryCovered: false},
			listCandidatesResult: []UsageRiskCandidate{{UserID: 1, GroupID: 2, RequestCount: 5}},
		}
		svc := newSvcWithPolicy(repo, policy)
		svc.lastPolicyVersion = UsageRiskPolicyVersion(policy, timezone.Name())
		require.NoError(t, svc.runAnalysis(context.Background(), svc.now()))
		require.True(t, repo.completeRunProgress.HistoryCovered, "F5/E：无失败批次且 older 已完成 → history_covered 翻转为 true")
		require.True(t, repo.completeRunProgress.R1ReevalPending, "F5/E：覆盖由 false→true → r1_reeval_pending 置 true（下轮补算 R1）")
	})
	t.Run("failed batches present -> stays uncovered", func(t *testing.T) {
		policy := UsageRiskPolicy{Enabled: true, RetentionDays: 0, MinDailyRequests: 1}
		repo := &fakeUsageRiskRepo{
			latestRunResult:      &UsageRiskRunStatus{Status: "partial", HistoryCovered: false},
			distinctIPsErr:       errors.New("phase2 down"),
			listCandidatesResult: []UsageRiskCandidate{{UserID: 1, GroupID: 2, RequestCount: 5}},
		}
		svc := newSvcWithPolicy(repo, policy)
		svc.lastPolicyVersion = UsageRiskPolicyVersion(policy, timezone.Name())
		require.NoError(t, svc.runAnalysis(context.Background(), svc.now()))
		require.False(t, repo.completeRunProgress.HistoryCovered, "F5/E：存在失败批次 → history_covered 不得翻转（保持 false）")
	})
}

// TestUsageRiskPendingReevalRoundClearsPending 锁 E/F6：覆盖翻转后待重评 R1 轮。
// 上轮 r1_reeval_pending=true 且非全量重评 → 本轮回填完成（historyCovered=true 传入每日重算 R1）后置 false。
func TestUsageRiskPendingReevalRoundClearsPending(t *testing.T) {
	policy := UsageRiskPolicy{Enabled: true, RetentionDays: 0, MinDailyRequests: 1}
	repo := &fakeUsageRiskRepo{
		latestRunResult:      &UsageRiskRunStatus{Status: "completed", HistoryCovered: true, R1ReevalPending: true},
		listCandidatesResult: []UsageRiskCandidate{{UserID: 1, GroupID: 2, RequestCount: 5}},
	}
	svc := newSvcWithPolicy(repo, policy)
	svc.lastPolicyVersion = UsageRiskPolicyVersion(policy, timezone.Name())
	require.NoError(t, svc.runAnalysis(context.Background(), svc.now()))
	require.False(t, repo.completeRunProgress.R1ReevalPending, "F6/E：pending 重评轮完成后须置 r1_reeval_pending=false")
	require.True(t, repo.completeRunProgress.HistoryCovered, "F6/E：覆盖应保持 true")
}

// TestUsageRiskBuildReportRecordCrossGroupActiveHours 锁 F/F7：用户级活跃桶 = 用户当日
// 全部合格分组的桶起点（跨分组 distinct）。R1 活跃小时 = 跨分组 distinct 桶数。
func TestUsageRiskBuildReportRecordCrossGroupActiveHours(t *testing.T) {
	dayStart := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.AddDate(0, 0, 1)
	h1 := dayStart.Add(1 * time.Hour)
	h2 := dayStart.Add(2 * time.Hour)
	h3 := dayStart.Add(3 * time.Hour)
	h4 := dayStart.Add(4 * time.Hour)
	dayMap := map[UsageRiskUserGroupKey]*dayAgg{
		{UserID: 1, GroupID: 2}: {BucketInstants: []time.Time{h1, h2, h3}}, // 3 桶
		{UserID: 1, GroupID: 3}: {BucketInstants: []time.Time{h3, h4}},     // 2 桶，h3 与分组2共享
		{UserID: 9, GroupID: 2}: {BucketInstants: []time.Time{h1}},          // 其他用户，不计
	}
	c := UsageRiskCandidate{UserID: 1, GroupID: 2, RequestCount: 5}
	svc := newSvcWithPolicy(&fakeUsageRiskRepo{}, UsageRiskPolicy{Enabled: true, MinDailyRequests: 1})
	rec, err := svc.buildReportRecord(context.Background(), c, dayStart, dayEnd, UsageRiskPolicy{MinDailyRequests: 1}, "UTC", dayMap, map[UsageRiskUserGroupKey]int{}, map[int64]int{}, 1, false)
	require.NoError(t, err)
	var ev map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Evidence, &ev))
	var ah int
	require.NoError(t, json.Unmarshal(ev["active_hours"], &ah))
	require.Equal(t, 4, ah, "F7/F：用户跨分组 distinct 桶数应为 4（h1,h2,h3,h4；h3 共享去重；用户9不计）")
}

// TestUsageRiskHandleTimezoneChangeInvalidates 锁 J/F12 + R13-3（外审⑨ 发现3）：时区变更换日
// 迁移仅动有效性维度（对全部当前有效报告失效），保留人工状态与审计；仓储单一 SQL 全量失效
// （invalidated_at IS NULL），不按当前配置推导日期范围——保留期曾缩短/清理曾失败遗留的
// 范围外旧时区报告一并失效，不再与新日界报告并存。
func TestUsageRiskHandleTimezoneChangeInvalidates(t *testing.T) {
	policy := UsageRiskPolicy{Enabled: true, RetentionDays: 2}
	repo := &fakeUsageRiskRepo{}
	svc := newSvcWithPolicy(repo, policy)
	require.NoError(t, svc.handleTimezoneChange(context.Background(), svc.now(), policy, "Asia/Shanghai"))
	require.Equal(t, 1, repo.invalidateAllCalls, "R13-3：换日迁移须恰一次调用全量失效单一 SQL（InvalidateAllEffective）")
	require.Equal(t, 0, repo.invalidateCalls, "R13-3：换日迁移不再走按日失效旧链（范围推测已废止）")
}

// TestUsageRiskRunAnalysisTZChangeFailedClosed 锁 J/F12：时区变更换日迁移失败 → 本轮失败关闭
// （返回错误、不建 run、下轮重试）。
func TestUsageRiskRunAnalysisTZChangeFailedClosed(t *testing.T) {
	policy := UsageRiskPolicy{Enabled: true, RetentionDays: 2}
	repo := &fakeUsageRiskRepo{
		latestRunResult: &UsageRiskRunStatus{Status: "completed", HistoryCovered: true, TZName: "America/New_York"},
		invalidateAllErr: errors.New("migration failed"),
	}
	svc := newSvcWithPolicy(repo, policy)
	svc.lastPolicyVersion = UsageRiskPolicyVersion(policy, timezone.Name()) // 版本相同 → 仅时区变更触发
	require.Error(t, svc.runAnalysis(context.Background(), svc.now()), "F12/J：时区迁移失败 → 本轮失败关闭")
	require.Equal(t, 0, repo.createRunCalls, "F12/J：迁移失败前未建 run（CreateRun 在迁移之后）")
}

// TestUsageRiskRunAnalysisTZChangeSuccessCreatesRun 锁 J/F12：时区变更成功须以 fullReeval 重建（CreateRun 执行）。
func TestUsageRiskRunAnalysisTZChangeSuccessCreatesRun(t *testing.T) {
	policy := UsageRiskPolicy{Enabled: true, RetentionDays: 0}
	repo := &fakeUsageRiskRepo{
		latestRunResult: &UsageRiskRunStatus{Status: "completed", HistoryCovered: true, TZName: "America/New_York"},
	}
	svc := newSvcWithPolicy(repo, policy)
	svc.lastPolicyVersion = UsageRiskPolicyVersion(policy, timezone.Name())
	require.NoError(t, svc.runAnalysis(context.Background(), svc.now()))
	require.Equal(t, 1, repo.createRunCalls, "F12/J：时区变更成功须以 fullReeval 重建（CreateRun 执行）")
}

// TestUsageRiskConsecutivePartialOf 锁 M/F15：连续 partial 计数——completed 重置 0，否则上一轮值+1。
func TestUsageRiskConsecutivePartialOf(t *testing.T) {
	require.Equal(t, 0, consecutivePartialOf("completed", 5), "M/F15：completed 重置 0（无论上一轮多大）")
	require.Equal(t, 1, consecutivePartialOf("partial", 0), "M/F15：0→1")
	require.Equal(t, 2, consecutivePartialOf("partial", 1), "M/F15：1→2")
	require.Equal(t, 3, consecutivePartialOf("partial", 2), "M/F15：2→3")
	require.Equal(t, 0, consecutivePartialOf("completed", 2), "M/F15：序列 2→0 重置")
}

// TestUsageRiskRunAnalysisConsecutivePartialsReset 锁 M/F15：completed 轮跨轮重置连续 partial 为 0。
func TestUsageRiskRunAnalysisConsecutivePartialsReset(t *testing.T) {
	policy := UsageRiskPolicy{Enabled: true, RetentionDays: 0}
	repo := &fakeUsageRiskRepo{
		latestRunResult: &UsageRiskRunStatus{Status: "partial", HistoryCovered: true, ConsecutivePartials: 2},
	}
	svc := newSvcWithPolicy(repo, policy)
	svc.lastPolicyVersion = UsageRiskPolicyVersion(policy, timezone.Name())
	require.NoError(t, svc.runAnalysis(context.Background(), svc.now()))
	require.Equal(t, "completed", repo.completeRunStatus)
	require.Equal(t, 0, repo.completeRunProgress.ConsecutivePartials, "M/F15：completed 须重置连续 partial 为 0")
}

// TestUsageRiskRunAnalysisConsecutivePartialsIncrement 锁 M/F15：partial 轮在上轮基础上 +1（序列 2→3）。
func TestUsageRiskRunAnalysisConsecutivePartialsIncrement(t *testing.T) {
	policy := UsageRiskPolicy{Enabled: true, RetentionDays: 0, MinDailyRequests: 1}
	repo := &fakeUsageRiskRepo{
		latestRunResult:      &UsageRiskRunStatus{Status: "partial", HistoryCovered: true, ConsecutivePartials: 2},
		distinctIPsErr:       errors.New("db down"),
		listCandidatesResult: []UsageRiskCandidate{{UserID: 1, GroupID: 2, RequestCount: 5}},
	}
	svc := newSvcWithPolicy(repo, policy)
	svc.lastPolicyVersion = UsageRiskPolicyVersion(policy, timezone.Name())
	require.NoError(t, svc.runAnalysis(context.Background(), svc.now())) // 批次错误已记录下轮重试，run 不返回 err
	require.Equal(t, "partial", repo.completeRunStatus)
	require.Equal(t, 3, repo.completeRunProgress.ConsecutivePartials, "M/F15：partial 须在上轮 2 基础上 +1 = 3")
}

// TestUsageRiskRunAnalysisFinalizeCtxIndependent 锁 N/F18：预算耗尽/错误路径收尾用独立 ctx
// （context.WithoutCancel 派生 + 10s 超时），即便父 ctx 已取消，收尾进度与台账仍独立落账。
func TestUsageRiskRunAnalysisFinalizeCtxIndependent(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel() // 父 ctx 已取消，模拟业务 ctx 失效
	repo := &fakeUsageRiskRepo{
		rebuildHourlyErr: errors.New("near-window rebuild failed"),
	}
	svc := newSvcWithPolicy(repo, UsageRiskPolicy{Enabled: true, RetentionDays: 0})
	require.Error(t, svc.runAnalysis(parent, svc.now()), "N/F18：近窗重建失败应返回错误")
	require.Equal(t, 1, repo.completeRunCalls, "N/F18：即便父 ctx 已取消，收尾 CompleteRun 仍须执行（避免 run 永远 running）")
	require.False(t, repo.completeRunCtxErr != nil, "N/F18：收尾 ctx 必须独立于父 ctx（非 cancelled）")
}

// TestUsageRiskGetRunStatusErrorState 锁 L/F14：失败关闭态优先于 LatestRun 暴露 error 态。
func TestUsageRiskGetRunStatusErrorState(t *testing.T) {
	fixed := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	svc := &UsageRiskAnalysisService{
		repo:         &fakeUsageRiskRepo{},
		nowFn:        func() time.Time { return fixed },
		failedClosed: true,
		failedErr:    errors.New("retention binding violated"),
		instanceID:   "test",
		interval:     time.Hour,
		budget:       4 * time.Minute,
	}
	st, err := svc.GetRunStatus(context.Background())
	require.NoError(t, err)
	require.Equal(t, "error", st.Status, "L/F14：失败关闭态优先暴露 error 态（而非 idle/completed）")
	require.Contains(t, st.Error, "retention binding violated")
}

// TestUsageRiskReconcileDayBackfillsHourlyBuckets 锁 I/F11：reconcileDay 每经一个本地日即以
// [dayStart, dayEnd) 重建该日小时桶（边界含当日、不含次日），为 R1 历史桶数据源。
func TestUsageRiskReconcileDayBackfillsHourlyBuckets(t *testing.T) {
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	dayStart := timezone.StartOfDay(day)
	dayEnd := dayStart.AddDate(0, 0, 1)
	repo := &fakeUsageRiskRepo{
		aggregateHourlyFn: func(ds time.Time) []UsageRiskHourAggregate {
			return nil
		},
		listCandidatesResult: []UsageRiskCandidate{{UserID: 1, GroupID: 2, RequestCount: 5}},
	}
	svc := newSvcWithPolicy(repo, UsageRiskPolicy{Enabled: true, MinDailyRequests: 1})
	progress := UsageRiskRunProgress{}
	_, _, dayComplete, _, err := svc.reconcileDay(context.Background(), 1, day, 0, UsageRiskPolicy{MinDailyRequests: 1}, "UTC", 1, false, &progress)
	require.NoError(t, err)
	require.True(t, dayComplete, "当日应完整走完")
	require.Equal(t, 1, repo.rebuildHourlyCalls, "reconcileDay 须对该日回填一次小时桶")
	require.Len(t, repo.rebuildHourlyWindows, 1, "须仅录制一个回填窗口")
	require.Equal(t, dayStart, repo.rebuildHourlyWindows[0].Start, "回填窗口起点须为当日 00:00（含当日）")
	require.Equal(t, dayEnd, repo.rebuildHourlyWindows[0].End, "回填窗口终点须为次日 00:00（不含次日）")
}

// TestUsageRiskReconcileDayBackfillFailureClosesDay 锁 I/F11 失败语义：回填失败必须失败关闭
// （返回错误、当日不完成、不触发集合外失效），不得降级跳过回填继续算。
func TestUsageRiskReconcileDayBackfillFailureClosesDay(t *testing.T) {
	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	repo := &fakeUsageRiskRepo{
		rebuildHourlyErr: errors.New("backfill hourly failed"),
	}
	svc := newSvcWithPolicy(repo, UsageRiskPolicy{Enabled: true, MinDailyRequests: 1})
	progress := UsageRiskRunProgress{}
	batches, _, dayComplete, _, err := svc.reconcileDay(context.Background(), 1, day, 0, UsageRiskPolicy{MinDailyRequests: 1}, "UTC", 1, false, &progress)
	require.Error(t, err, "I：回填失败必须失败关闭（返回错误）")
	require.False(t, dayComplete, "回填失败当日未完成（不触发失效）")
	require.Equal(t, 0, batches)
	require.Equal(t, 0, repo.invalidateCalls, "回填失败当日不得触发集合外失效")
}

// ───────────────────────── R6-1 增量重审锁定测试 ─────────────────────────

// TestUsageRiskAFullReevalNotInheritCoverage 锁定 A 组（P1）：fullReeval 轮不继承覆盖。
// 已覆盖实例（prevHistoryCovered=true）因策略/时区变更进 fullReeval 后预算耗尽：
//   - 旧直穿逻辑 historyCovered=prevHistoryCovered 恒 true → 下轮 steadySkip 跳过剩余 older 日；
//   - 修复后 historyCovered=prevHistoryCovered && !fullReeval=false；
//     且失败/预算耗尽下恢复判据 !budgetExhausted==false → 覆盖保持 false（下轮从游标继续，不漏算）。
func TestUsageRiskAFullReevalNotInheritCoverage(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel() // 父 ctx 已取消 → 主循环首个 day 预算耗尽

	policy := UsageRiskPolicy{Enabled: true, RetentionDays: 2, MinDailyRequests: 1}
	repo := &fakeUsageRiskRepo{
		latestRunResult: &UsageRiskRunStatus{Status: "completed", HistoryCovered: true}, // 已覆盖实例
	}
	svc := newSvcWithPolicy(repo, policy)
	svc.lastPolicyVersion = "stale-version" // 与当前版本不同 → fullReeval=true

	require.NoError(t, svc.runAnalysis(parent, svc.now()))

	// 核心断言：fullReeval 轮即便 prevHistoryCovered=true，预算耗尽后覆盖不得继承为 true。
	require.False(t, repo.completeRunProgress.HistoryCovered,
		"A：fullReeval 轮不继承覆盖——预算耗尽时 HistoryCovered 必须为 false（下轮不被 steadySkip 跳过）")
	require.Equal(t, "partial", repo.completeRunStatus,
		"A：fullReeval + 预算耗尽 → 状态为 partial")
	require.True(t, repo.completeRunProgress.BudgetExhausted,
		"A：预算耗尽须如实记录")
}

// TestUsageRiskBDisableMarksVersionInvalid 锁定 B 组（P1）：关闭分支标记版本失效。
// 启用跑一轮（lastPolicyVersion=当前版本）→ 关闭一轮（零 DB 副作用 + lastPolicyVersion=""）
// → 重新启用同阈值同版本 → lastPolicyVersion("") != version 必触发 fullReeval，
// olderStart 重置 retentionStart（Sep18）全量重放——关闭超 26h 的历史不再漏算。
func TestUsageRiskBDisableMarksVersionInvalid(t *testing.T) {
	policy := UsageRiskPolicy{Enabled: true, RetentionDays: 2, MinDailyRequests: 1}
	repo := &fakeUsageRiskRepo{
		latestRunResult: &UsageRiskRunStatus{Status: "completed", HistoryCovered: true},
	}
	svc := newSvcWithPolicy(repo, policy)

	// 步骤 1：启用轮。结束 lastPolicyVersion == 当前版本。
	require.NoError(t, svc.runAnalysis(context.Background(), svc.now()))
	require.NotEmpty(t, svc.lastPolicyVersion, "B：启用轮后 lastPolicyVersion 应为当前版本")

	// 步骤 2：关闭轮（Enabled=false）。零 DB 副作用，且 lastPolicyVersion 被清空。
	disabled := UsageRiskPolicy{Enabled: false, RetentionDays: 2, MinDailyRequests: 1}
	svc.policyFn = func(context.Context) (UsageRiskPolicy, error) { return disabled, nil }
	repo.callOrder = nil // 清空录制，验证关闭轮零副作用
	require.NoError(t, svc.runAnalysis(context.Background(), svc.now()))
	require.Empty(t, repo.callOrder, "B/C：关闭轮零 DB 副作用（连 LatestRun 都不读）")
	require.Empty(t, svc.lastPolicyVersion, "B：关闭分支必须将 lastPolicyVersion 置空（重新启用必然触发 fullReeval）")

	// 步骤 3：重新启用同阈值同版本。lastPolicyVersion="" != version → fullReeval=true，
	// olderStart 重置到 retentionStart（Sep18）。
	svc.policyFn = func(context.Context) (UsageRiskPolicy, error) { return policy, nil }
	var olderDays []time.Time
	repo.aggregateHourlyFn = func(ds time.Time) []UsageRiskHourAggregate {
		olderDays = append(olderDays, timezone.StartOfDay(ds))
		return nil
	}
	require.NoError(t, svc.runAnalysis(context.Background(), svc.now()))
	require.NotEmpty(t, svc.lastPolicyVersion, "B：重新启用轮结束后恢复版本标记")

	// fullReeval 证据：older 循环必须从 retentionStart=Sep18 重放（而非稳态跳过 / 从游标续）。
	foundRetentionStart := false
	for _, d := range olderDays {
		if d.Equal(timezone.StartOfDay(time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC))) {
			foundRetentionStart = true
			break
		}
	}
	require.True(t, foundRetentionStart,
		"B：重新启用必须 fullReeval（older 从 retentionStart=Sep18 重放）；若 lastPolicyVersion 未清空则此处稳态跳过，Sep18 不会进 AggregateHourly")
}

// ───────────────────────── R8-2b 锁定测试：F1 finalizeWrite 每写新建 ctx ─────────────────────────
//
// 墙钟盲区补锁（外审 #3）：finalizeTimeout 注入 100ms；完成一次进度写后 sleep 超过该值，
// 再执行第二次进度写与 CompleteRun 仍须成功——证明每次收尾写都新建独立 ctx（独立满额超时），
// 而非 run 开跑时单次创建、跑超阈值后全部收尾写拿到已过期 ctx（旧实现墙钟盲区症状：
// 既有测试全在 10s 墙钟内跑完故全绿）。fake 已在 ctx 失效时让 UpdateRunProgress/CompleteRun
// 返回 ctx.Err()——若拿到过期 ctx 写入即失败，测试断言自然 FAIL。
func TestUsageRiskR8_2b_F1FinalizeWritePerWriteFreshContext(t *testing.T) {
	repo := &fakeUsageRiskRepo{
		latestRunResult: &UsageRiskRunStatus{Status: "completed", HistoryCovered: true},
	}
	svc := newSvcWithPolicy(repo, UsageRiskPolicy{Enabled: true, RetentionDays: 1, MinDailyRequests: 1})
	svc.finalizeTimeout = 100 * time.Millisecond

	// 第一次进度写（模拟 run 运行中途一次 UpdateRunProgress）。
	progress := UsageRiskRunProgress{ReconCursorDate: ptrTime(svc.now())}
	fctx1, fc1 := svc.finalizeWrite(context.Background())
	require.NoError(t, repo.UpdateRunProgress(fctx1, 1, progress), "首次写须成功（新鲜 ctx）")
	fc1()

	// 完成一次进度写后 sleep 超过 finalizeTimeout。
	time.Sleep(150 * time.Millisecond)

	// 第二次进度写 + CompleteRun 仍须成功（每写新建 ctx，独立满额超时预算）。
	progress2 := UsageRiskRunProgress{ReconCursorDate: ptrTime(svc.now())}
	fctx2, fc2 := svc.finalizeWrite(context.Background())
	require.NoError(t, repo.UpdateRunProgress(fctx2, 1, progress2), "R8-2b/F1：sleep 超阈值后第二次进度写仍须成功（每写新建 ctx）")
	fc2()

	fctx3, fc3 := svc.finalizeWrite(context.Background())
	require.NoError(t, repo.CompleteRun(fctx3, 1, "completed", progress2), "R8-2b/F1：sleep 超阈值后 CompleteRun 仍须成功（每写新建 ctx）")
	fc3()

	// 二次写各获独立满额超时预算：第二次写 deadline 距当前仍 ≈ finalizeTimeout（未因首写耗时而缩短）。
	require.GreaterOrEqual(t, len(repo.updateProgressCtxDeadlines), 2, "须录得两次进度写 deadline")
	d2 := repo.updateProgressCtxDeadlines[len(repo.updateProgressCtxDeadlines)-1]
	require.WithinDuration(t, time.Now().Add(100*time.Millisecond), d2, 80*time.Millisecond,
		"第二次写须持有独立满额超时预算（≈ finalizeTimeout），而非共享开跑时已近过期的 ctx")
	require.Equal(t, 1, repo.completeRunCalls, "CompleteRun 须执行一次且成功")
}

// ───────────────────────── R8-2b 锁定测试：F3 近窗日去重 ─────────────────────────
//
// 上一轮近窗日失败（FailedBatches 含该日）→ 本轮 retry 成功 + 近窗循环跳过该日
// （attempted[day] continue）→ CandidatesTotal 对该日恰计一次。双失败场景：
// retry 仍失败（该日保留于列表）+ 近窗循环跳过 → failedBatches 无重复项。
// 注：实现基于 orchFakeRepo（按日注入候选/失败），见
// usage_risk_analysis_orchestration_test.go TestUsageRiskR8_2b_F3*。
