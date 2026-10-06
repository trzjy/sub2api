package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/stretchr/testify/require"
)

// ───────────────────────── 编排测试桩：强化录制版 repo / lock ─────────────────────────
//
// 本文件只补编排路径（runHourly / runAnalysis / reconcileDay / handleTimezoneChange）
// 的单测，对齐 dashboard_aggregation_service_test.go 的 fake 模式：纯内存录制，不依赖真实 DB。
// 复用 usage_risk_policy_test.go 的 usageRiskSettingRepoStub（SettingRepository 桩），
// 因为 runAnalysis 经 loadPolicy → settingSvc.LoadUsageRiskPolicy 读策略（不读 policyFn，
// policyFn 仅服务 currentPolicy API 路径）。

// orchFakeRepo 是录制版的 UsageRiskRepository：记录关键编排调用参数/序列，供断言。
type orchFakeRepo struct {
	mu sync.Mutex

	// 近窗重建
	rebuildCalls     int
	rebuildStart     time.Time
	rebuildEnd       time.Time
	rebuildParams    AggregateParams

	rebuildWindows []rebuildWindow // 全量回填窗口录制（含近窗 + reconcileDay 逐日回填）

	// run 台账
	createRunArg     UsageRiskRunRecord
	createRunCalls   int
	completeCalls    int
	completeStatus   string
	completeProgress UsageRiskRunProgress

	// 进度持久化（按调用顺序录制，便于断言游标字段非空）
	updateProgressCalls   int
	updateProgressHistory []UsageRiskRunProgress

	// 自愈收敛
	convergeCalls      int
	convergeStaleBefore time.Time
	convergeStage      string

	// 对账录制（区分 UPSERT-only 与失效，捕获键集/记录）——派发单 U4b-R4 不变量锁死需要。
	// reconcileDates / reconcileHadRecord 保留旧语义（每个对账日恰好一次失效），供既有断言兼容。
	reconcileDates     []time.Time
	reconcileHadRecord []bool
	reconcileCallCount int

	// UPSERT-only 调用录制（批级提交，绝不触发失效）。
	upsertOnlyCalls []upsertOnlyCall
	// 失效调用录制（日完成/空日/时区迁移），捕获键集用于"失效仅全键集"断言。
	invalidateCalls []invalidateCall
	invalidateCount int
	upsertCount     int
	// 时区换日迁移全量失效（InvalidateAllEffective 单一 SQL）调用计数（R13-1 改动 2）。
	invalidateAllCalls int

	// 候选注入：仅 candDay（若非 nil）返回 candList，其余日返回空候选。
	candList []UsageRiskCandidate
	candDay  *time.Time
	// candByDay：多日候选注入（S3 锁定用；优先于 candList/candDay）。
	candByDay map[time.Time][]UsageRiskCandidate

	// 断点续跑/失败批次测试钩子：UPSERT 达到阈值时触发 cancelFn（模拟预算耗尽于批内）。
	cancelAfterUpserts int

	// 状态列写入（断言时区换日迁移不触碰 status 列）
	updateStatusAuditCalls int

	// 可配置返回
	latestRunResult *UsageRiskRunStatus
	latestCursorResult *UsageRiskReconCursor

	// 让某次调用触发 ctx 截止（用于预算耗尽路径）
	cancelOnRebuild bool
	cancelled       context.CancelFunc

	// 断点续跑/失败批次测试钩子：
	//   cancelAfterReconciles：失效（每个对账日一次）调用达到该次数时触发 cancelFn（模拟预算耗尽）。
	//   cancelAfterUpserts：UPSERT-only 调用达到该次数时触发 cancelFn（模拟预算耗尽于批内）。
	//   failDay：该日 AggregateHourly 直接报错（模拟对账日失败，记录下轮重试）。
	cancelAfterReconciles int
	cancelFn             context.CancelFunc
	failDay              *time.Time

	// R9-2：failUpsertNth 按「该日第 N 次 UPSERT-only 调用」注入批级失败（key=日，value=N）。
	// UPSERT 方法不接收 offset，故以调用序号换算批起点：第 N 次调用 ↔ 批起点 offset=(N-1)*100
	// （usageRiskReconBatchKeyLimit）。例：N=3 → offset=2 批（start=200）失败，此前 [0,100)、
	// [100,200) 两批已提交，reconcileDay 返回 nextOffset=200（断点续跑语义）。
	// 未注入（map 为 nil 或无该日键）时行为与既有测试一致。
	failUpsertNth map[time.Time]int
	// 每日 UPSERT-only 调用计数（按日记录第 N 次调用，用于 N↔offset 换算断言）。
	upsertPerDay map[time.Time]int

	// R10-2/#4：注入 CreateRun 失败（非空时 CreateRun 直接报错，模拟台账持久化临时失败）。
	createRunErr error

	// R13-1 改动 3e：R1 数据注入。
	//   aggHourlyByDay 按日注入 AggregateHourly 小时桶（BucketsHour 即活跃桶起点，供 R1 活跃小时统计）。
	//   bucketByUser 注入 UserActiveBucketHours 的 rollup distinct 活跃桶（供 R1 连续活跃天数统计）。
	//   默认 nil = 零数据（与既有测试一致）。
	aggHourlyByDay map[time.Time][]UsageRiskHourAggregate
	bucketByUser   map[int64][]time.Time
}

// upsertOnlyCall 录制一次批 UPSERT-only 提交（含该批记录，用于"已提交批不重做"断言）。
type upsertOnlyCall struct {
	day     time.Time
	records []UsageRiskReportRecord
}

// rebuildWindow 录制一次 RebuildHourlyWindow 调用的窗口与参数（近窗重建 + reconcileDay 逐日回填共用）。
type rebuildWindow struct {
	Start  time.Time
	End    time.Time
	Params AggregateParams
}

// invalidateCall 录制一次失效调用（含键集，用于"失效仅全键集"不变量断言）。
type invalidateCall struct {
	day      time.Time
	userIDs  []int64
	groupIDs []int64
}

var _ UsageRiskRepository = (*orchFakeRepo)(nil)

func (f *orchFakeRepo) AggregateHourly(ctx context.Context, ws, we time.Time, p AggregateParams) ([]UsageRiskHourAggregate, error) {
	if f.failDay != nil && ws.Equal(*f.failDay) {
		return nil, errors.New("simulated day failure")
	}
	if f.aggHourlyByDay != nil {
		if dayList, ok := f.aggHourlyByDay[timezone.StartOfDay(ws)]; ok {
			return dayList, nil
		}
	}
	return nil, nil
}
func (f *orchFakeRepo) AggregateGroupRPMMinutes(ctx context.Context, ws, we time.Time, p AggregateParams, limits []UsageRiskRPMLimit, ratio float64) (map[UsageRiskUserGroupKey]int, error) {
	return map[UsageRiskUserGroupKey]int{}, nil
}
func (f *orchFakeRepo) AggregateUserGlobalRPMMinutes(ctx context.Context, ws, we time.Time, limits []UsageRiskUserLimit, ratio float64) (map[int64]int, error) {
	return map[int64]int{}, nil
}
func (f *orchFakeRepo) ListCandidates(ctx context.Context, ws, we time.Time, p AggregateParams, minDailyRequests, offset, batchSize int) ([]UsageRiskCandidate, error) {
	if f.candByDay != nil {
		if dayList, ok := f.candByDay[timezone.StartOfDay(ws)]; ok {
			if offset >= len(dayList) {
				return nil, nil
			}
			end := offset + batchSize
			if end > len(dayList) {
				end = len(dayList)
			}
			return dayList[offset:end], nil
		}
		return nil, nil
	}
	if f.candDay == nil || !timezone.StartOfDay(ws).Equal(*f.candDay) {
		return nil, nil
	}
	if offset >= len(f.candList) {
		return nil, nil
	}
	end := offset + batchSize
	if end > len(f.candList) {
		end = len(f.candList)
	}
	return f.candList[offset:end], nil
}
func (f *orchFakeRepo) RebuildHourlyWindow(ctx context.Context, ws, we time.Time, p AggregateParams) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancelOnRebuild && f.cancelled != nil {
		f.cancelled()
	}
	f.rebuildCalls++
	f.rebuildWindows = append(f.rebuildWindows, rebuildWindow{Start: ws, End: we, Params: p})
	f.rebuildStart = ws
	f.rebuildEnd = we
	f.rebuildParams = p
	return 0, nil
}
func (f *orchFakeRepo) DistinctIPs(ctx context.Context, userID int64, ds, de time.Time, p AggregateParams) ([]string, error) {
	return nil, nil
}
func (f *orchFakeRepo) IPAssociatedUsers(ctx context.Context, userID int64, ds, de time.Time, p AggregateParams) (map[string]IPClusterFact, error) {
	return map[string]IPClusterFact{}, nil
}
func (f *orchFakeRepo) UADistribution(ctx context.Context, userID int64, ds, de time.Time, p AggregateParams) (map[string]int, error) {
	return map[string]int{}, nil
}
func (f *orchFakeRepo) KeyUsageDistribution(ctx context.Context, userID int64, ds, de time.Time, p AggregateParams) (map[string]int, error) {
	return map[string]int{}, nil
}
func (f *orchFakeRepo) ActiveHourHeatmap(ctx context.Context, userID int64, ds, de time.Time, tzName string, p AggregateParams) (map[int]int, error) {
	return map[int]int{}, nil
}
func (f *orchFakeRepo) R2PeerDayCosts(ctx context.Context, groupID int64, ds, de time.Time, p AggregateParams, minDailyRequests int) ([]UsageRiskPeerCost, error) {
	return nil, nil
}
func (f *orchFakeRepo) UserActiveBucketHours(ctx context.Context, userID int64, lookbackStart, reportDay time.Time) ([]time.Time, error) {
	if f.bucketByUser != nil {
		if buckets, ok := f.bucketByUser[userID]; ok {
			return buckets, nil
		}
	}
	return nil, nil
}
func (f *orchFakeRepo) UpdateReportStatus(ctx context.Context, reportID int64, oldStatus, newStatus string, adminID int64, at time.Time) (bool, error) {
	return true, nil
}
func (f *orchFakeRepo) UpdateStatusWithAudit(ctx context.Context, reportID int64, oldStatus, newStatus string, adminID int64, at time.Time, entry *AuditLog) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateStatusAuditCalls++
	return true, nil
}
func (f *orchFakeRepo) ReconcileReportBatchUPSERTOnly(ctx context.Context, reportDate time.Time, records []UsageRiskReportRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// R9-2：按「该日第 N 次 UPSERT 调用」注入批级失败（N ↔ 批起点 offset=(N-1)*100）。
	// 失败调用不录制进 upsertOnlyCalls（该批未提交，不算「已提交批」），但计入 upsertPerDay 序号。
	if f.upsertPerDay == nil {
		f.upsertPerDay = map[time.Time]int{}
	}
	f.upsertPerDay[reportDate]++
	if f.failUpsertNth != nil {
		if nth, ok := f.failUpsertNth[timezone.StartOfDay(reportDate)]; ok && f.upsertPerDay[reportDate] == nth {
			return errors.New("simulated batch upsert failure")
		}
	}
	f.upsertCount++
	f.upsertOnlyCalls = append(f.upsertOnlyCalls, upsertOnlyCall{day: reportDate, records: records})
	// 批内预算耗尽测试：UPSERT 达到阈值时触发 cancelFn（模拟预算耗尽于批内）。
	if f.cancelAfterUpserts > 0 && f.upsertCount == f.cancelAfterUpserts && f.cancelFn != nil {
		f.cancelFn()
	}
	return nil
}

func (f *orchFakeRepo) InvalidateOutsideKeySet(ctx context.Context, reportDate time.Time, userIDs, groupIDs []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidateCount++
	f.invalidateCalls = append(f.invalidateCalls, invalidateCall{day: reportDate, userIDs: userIDs, groupIDs: groupIDs})
	// 旧 reconcileDates / reconcileHadRecord 语义：每个对账日恰好一次失效调用，且失效不带 records。
	f.reconcileDates = append(f.reconcileDates, reportDate)
	f.reconcileHadRecord = append(f.reconcileHadRecord, false)
	f.reconcileCallCount++
	// 断点续跑测试：失效（每个对账日一次）达到阈值时触发 cancelFn。
	if f.cancelAfterReconciles > 0 && f.invalidateCount == f.cancelAfterReconciles && f.cancelFn != nil {
		f.cancelFn()
	}
	return nil
}
func (f *orchFakeRepo) InvalidateAllEffective(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidateAllCalls++
	return nil
}
func (f *orchFakeRepo) CreateRun(ctx context.Context, r UsageRiskRunRecord) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createRunCalls++
	if f.createRunErr != nil {
		return 0, f.createRunErr
	}
	f.createRunArg = r
	return 1, nil
}
func (f *orchFakeRepo) UpdateRunProgress(ctx context.Context, runID int64, p UsageRiskRunProgress) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateProgressCalls++
	f.updateProgressHistory = append(f.updateProgressHistory, p)
	return nil
}
func (f *orchFakeRepo) CompleteRun(ctx context.Context, runID int64, status string, p UsageRiskRunProgress) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completeCalls++
	f.completeStatus = status
	f.completeProgress = p
	return nil
}
func (f *orchFakeRepo) ConvergeStaleRuns(ctx context.Context, staleBefore time.Time, failureStage string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.convergeCalls++
	f.convergeStaleBefore = staleBefore
	f.convergeStage = failureStage
	return nil
}
func (f *orchFakeRepo) LatestRun(ctx context.Context) (*UsageRiskRunStatus, error) {
	return f.latestRunResult, nil
}
func (f *orchFakeRepo) LoadLatestReconCursor(ctx context.Context) (*UsageRiskReconCursor, error) {
	return f.latestCursorResult, nil
}
func (f *orchFakeRepo) ListReports(ctx context.Context, flt UsageRiskListFilter, minScore int) ([]UsageRiskReportItem, int, error) {
	return nil, 0, nil
}
func (f *orchFakeRepo) GetReportDetail(ctx context.Context, reportID int64) (*UsageRiskReportDetail, error) {
	return nil, nil
}
func (f *orchFakeRepo) RiskSummary(ctx context.Context, minScore, topN int) (*UsageRiskSummary, error) {
	return nil, nil
}

// orchLeaderLock 是录制版 LeaderLockCache：记录获取/释放次数，可模拟被对等实例持有。
type orchLeaderLock struct {
	mu           sync.Mutex
	owner        string
	acquireFail  bool
	releaseCalls int
}

func (l *orchLeaderLock) TryAcquireLeaderLock(_ context.Context, key, owner string, _ time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.acquireFail {
		return false, nil
	}
	if l.owner != "" {
		return false, nil
	}
	l.owner = owner
	return true, nil
}
func (l *orchLeaderLock) ReleaseLeaderLock(_ context.Context, key, owner string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.releaseCalls++
	if l.owner == owner {
		l.owner = ""
	}
	return nil
}

// newOrchService 构造带 fake settingSvc 的服务，使 runAnalysis 能离线读策略。
// retentionDays<=0 时使用默认值（90），否则写入 usage_risk_retention_days 覆盖。
func newOrchService(t *testing.T, repo *orchFakeRepo, retentionDays int) *UsageRiskAnalysisService {
	t.Helper()
	settingSvc := newUsageRiskTestService()
	if retentionDays > 0 {
		settingSvc.settingRepo.(*usageRiskSettingRepoStub).values[SettingKeyUsageRiskRetentionDays] = strconv.Itoa(retentionDays)
	}
	fixed := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	return &UsageRiskAnalysisService{
		repo:       repo,
		settingSvc: settingSvc,
		nowFn:      func() time.Time { return fixed },
		instanceID: "test-instance",
		interval:   time.Hour,
		budget:     4 * time.Minute,
	}
}

// ───────────────────────── 1) 26h 窗口重建（参数精确 + 幂等） ─────────────────────────

func TestUsageRiskOrch_Rebuild26hWindow(t *testing.T) {
	repo := &orchFakeRepo{}
	svc := newOrchService(t, repo, 10)

	pol, _, _, err := svc.loadPolicy(context.Background())
	require.NoError(t, err)
	expectedParams := aggParams(pol)

	// 同输入两次运行，断言近窗重建窗口参数一致且精确。
	for i := 0; i < 2; i++ {
		err := svc.runAnalysis(context.Background(), svc.now())
		require.NoError(t, err)
	}

	// 近窗重建：窗口须为 [now-26h, now]、参数须为 aggParams(policy)；每轮恰好一次。
	nearWin := svc.now().Add(-usageRiskNearWindow)
	nearCount := 0
	var near *rebuildWindow
	for i := range repo.rebuildWindows {
		if repo.rebuildWindows[i].Start.Equal(nearWin) && repo.rebuildWindows[i].End.Equal(svc.now()) {
			nearCount++
			near = &repo.rebuildWindows[i]
		}
	}
	require.Equal(t, 2, nearCount, "两次运行都应重建近窗（26h window）")
	require.NotNil(t, near, "须存在近窗重建窗口")
	require.Equal(t, expectedParams, near.Params, "聚合资格谓词须为 aggParams(policy)")
	// 逐日回填（reconcileDay）与近窗重建均走 RebuildHourlyWindow，总数须 > 近窗数（覆盖 older 区间）。
	require.Greater(t, len(repo.rebuildWindows), 2, "older 区间逐日回填须触发额外重建调用")
}

// ───────────────────────── 2) leader 锁互斥 + running CAS 防重入 ─────────────────────────

func TestUsageRiskOrch_LeaderLockHeldByPeerSkips(t *testing.T) {
	repo := &orchFakeRepo{}
	svc := newOrchService(t, repo, 10)
	lock := &orchLeaderLock{acquireFail: true} // 模拟锁被对等实例持有

	svc.lockCache = lock
	svc.runHourly()

	require.Equal(t, 0, repo.rebuildCalls, "锁未获取时不得重建近窗")
	require.Equal(t, 0, repo.convergeCalls, "锁未获取时不得收敛台账")
	require.Equal(t, 0, repo.createRunArg.RunAt.Second(), "锁未获取时不得建 run（RunAt 零值）")
}

func TestUsageRiskOrch_RunningCASPreventsReentry(t *testing.T) {
	repo := &orchFakeRepo{}
	svc := newOrchService(t, repo, 10)
	// 模拟同进程已有运行在跑（CAS 失败路径）。
	atomic.StoreInt32(&svc.running, 1)

	svc.runHourly()

	require.Equal(t, 0, repo.rebuildCalls, "running 标记置位时不得重入")
	require.Equal(t, int32(1), atomic.LoadInt32(&svc.running), "早返回路径不得复位 running 标记")
}

// ───────────────────────── 3) 硬截止取消回滚 + 锁释放 ─────────────────────────

func TestUsageRiskOrch_HardDeadlineRollback(t *testing.T) {
	repo := &orchFakeRepo{}
	svc := newOrchService(t, repo, 10)

	// 已取消的 ctx：首个对账循环即判定预算耗尽。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := svc.runAnalysis(ctx, svc.now())
	require.NoError(t, err, "预算耗尽应优雅收尾为 partial，而非返回 error")

	require.Equal(t, "partial", repo.completeStatus, "硬截止须以 partial 收尾")
	require.True(t, repo.completeProgress.BudgetExhausted, "须带 BudgetExhausted=true")
	require.NotNil(t, repo.completeProgress.FailureStage, "须带 failure_stage")
	require.Equal(t, "budget_exhausted", *repo.completeProgress.FailureStage, "failure_stage 应为 budget_exhausted")
	require.False(t, repo.completeProgress.HistoryCovered, "预算耗尽时 history_covered 必须为 false")
	require.NotNil(t, repo.completeProgress.FinishedAt, "收尾须写 FinishedAt")
}

func TestUsageRiskOrch_LockReleasedAfterRun(t *testing.T) {
	repo := &orchFakeRepo{}
	svc := newOrchService(t, repo, 10)
	lock := &orchLeaderLock{}
	svc.lockCache = lock

	svc.runHourly()

	require.Equal(t, 1, lock.releaseCalls, "每轮运行结束必须释放 leader 锁")
	require.Equal(t, 0, repo.updateStatusAuditCalls, "正常运行路径不得触碰 status 列")
}

// ───────────────────────── 4) 预算耗尽续跑（partial + history_covered 翻转） ─────────────────────────
// 离线可验证部分：首轮 partial+BudgetExhausted+HistoryCovered=false；
// 次轮（充足预算）收尾为 completed 且 HistoryCovered 翻转为 true。
// 注：断点续跑（从持久化游标而非 retentionStart 重放）现已由 U4b-R3 实现，详见
// TestUsageRiskOrch_CursorResumeFromPersistedPoint / ColdStartCrossRoundCoverage。

func TestUsageRiskOrch_ExhaustionThenCoverageFlip(t *testing.T) {
	repo := &orchFakeRepo{}
	svc := newOrchService(t, repo, 10)

	// 第一轮：硬截止 → partial、预算耗尽、history 未覆盖。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, svc.runAnalysis(ctx, svc.now()))
	require.Equal(t, "partial", repo.completeStatus)
	require.True(t, repo.completeProgress.BudgetExhausted)
	require.False(t, repo.completeProgress.HistoryCovered)

	// 模拟首轮 partial 已持久化（LatestRun 反映未完成覆盖）。
	repo.latestRunResult = &UsageRiskRunStatus{Status: "partial", HistoryCovered: false}

	// 第二轮：充足预算，重放全窗口 → 覆盖完成，history_covered 翻转 true。
	require.NoError(t, svc.runAnalysis(context.Background(), svc.now()))
	require.Equal(t, "completed", repo.completeStatus, "全窗口重放完成后应为 completed")
	require.False(t, repo.completeProgress.BudgetExhausted)
	require.True(t, repo.completeProgress.HistoryCovered, "覆盖完成后 history_covered 须翻转为 true")
}

// ───────────────────────── 5) 对账游标单调推进不被饿死（预留预算先行） ─────────────────────────

func TestUsageRiskOrch_ReconReserveFirstThenNear(t *testing.T) {
	repo := &orchFakeRepo{}
	svc := newOrchService(t, repo, 10) // olderDays>0（retention 10d vs 26h 近窗）

	require.NoError(t, svc.runAnalysis(context.Background(), svc.now()))

	require.NotEmpty(t, repo.reconcileDates, "应当发生对账批次")
	nearStart := timezone.StartOfDay(svc.now().Add(-usageRiskNearWindow))
	// 第一个对账批次必须落在 older 窗口（< nearStart），证明"预留预算先行"已生效。
	require.True(t, repo.reconcileDates[0].Before(nearStart),
		"首个对账批次须为 older 窗口日（预留预算先行），实际首个=%s nearStart=%s",
		repo.reconcileDates[0], nearStart)
	// 保证：首个 near 窗口对账批次之前，必须至少已执行一个 older 窗口对账批次
	//（对账不被候选/近窗批次饿死）。注：older 窗口的剩余批次在 near 之后由"收尾 older"循环补跑，
	// 属既定设计，不视为饿死。
	firstNearIdx := -1
	for i, d := range repo.reconcileDates {
		if !d.Before(nearStart) {
			firstNearIdx = i
			break
		}
	}
	require.GreaterOrEqual(t, firstNearIdx, usageRiskReserveReconBatches,
		"near 窗口对账开始前必须已预留至少 %d 个 older 对账批次（不被饿死）", usageRiskReserveReconBatches)
}

// ───────────────────────── 6) 批内偏移游标经 UpdateRunProgress 持久化 ─────────────────────────
// 验证游标字段确实经 UpdateRunProgress 持久化（非空）。
// 注：游标续跑（日期维度单调推进、批内偏移真实返回/持久化）已由 U4b-R3 实现，详见新增测试。

func TestUsageRiskOrch_CursorPersistedViaUpdateRunProgress(t *testing.T) {
	repo := &orchFakeRepo{}
	svc := newOrchService(t, repo, 10)

	require.NoError(t, svc.runAnalysis(context.Background(), svc.now()))

	require.GreaterOrEqual(t, repo.updateProgressCalls, 1, "应当至少持久化一次进度")
	var sawCursor bool
	for _, p := range repo.updateProgressHistory {
		if p.ReconCursorDate != nil {
			sawCursor = true
			break
		}
	}
	require.True(t, sawCursor, "ReconCursorDate 须经 UpdateRunProgress 持久化")
}

// ───────────────────────── 7) 策略/时区变更重评与换日迁移 ─────────────────────────

// 时区变更：换日迁移全量失效单一 SQL，不触 status 列（外审⑨ 发现3：不再逐日调用 InvalidateOutsideKeySet，
// 不再按当前 RetentionDays 推测日期范围——保留期曾缩短/清理曾失败遗留的范围外有效报告一并失效）。
func TestUsageRiskOrch_TimezoneChangeInvalidatesOldDaysNoStatusTouch(t *testing.T) {
	repo := &orchFakeRepo{}
	svc := newOrchService(t, repo, 10)
	now := svc.now()

	require.NoError(t, svc.handleTimezoneChange(context.Background(), now, UsageRiskPolicy{RetentionDays: 10}, "Asia/Shanghai"))

	require.Equal(t, 1, repo.invalidateAllCalls, "换日迁移须恰一次调用全量失效单一 SQL（InvalidateAllEffective）")
	require.Empty(t, repo.reconcileDates, "换日迁移不再走按日 InvalidateOutsideKeySet 路径（不推测日期范围）")
	require.Equal(t, 0, repo.updateStatusAuditCalls, "换日迁移不得触碰 status 列（仅置 invalidated_at）")
}

// ───────────────────────── 8) 台账自愈收敛（ConvergeStaleRuns + FinishedAt/FailureStage） ─────────────────────────

func TestUsageRiskOrch_StaleRunConvergenceAndFinishedAt(t *testing.T) {
	repo := &orchFakeRepo{}
	svc := newOrchService(t, repo, 10)

	require.NoError(t, svc.runAnalysis(context.Background(), svc.now()))

	require.Equal(t, 1, repo.convergeCalls, "每轮须先收敛超期 running 台账")
	expectedStaleBefore := svc.now().Add(-svc.budget)
	require.WithinDuration(t, expectedStaleBefore, repo.convergeStaleBefore, time.Second, "staleBefore 须为 now-budget")
	require.Equal(t, "stale_timeout", repo.convergeStage, "failure_stage 须为 stale_timeout")

	require.Equal(t, 1, repo.completeCalls)
	require.NotNil(t, repo.completeProgress.FinishedAt, "收尾须写 FinishedAt")
}

// ───────────────────────── 9) 冷启动：无历史 → 首轮未覆盖，覆盖完成后翻转 ─────────────────────────

func TestUsageRiskOrch_ColdStartHistoryCoveredFlip(t *testing.T) {
	repo := &orchFakeRepo{}
	svc := newOrchService(t, repo, 10)
	// 无历史数据（LatestRun=nil）→ 首轮 prevHistoryCovered=false。

	// 首轮：无候选、retention=10d，完整重放全窗口 → 一轮即覆盖完成。
	require.NoError(t, svc.runAnalysis(context.Background(), svc.now()))
	require.True(t, repo.completeProgress.HistoryCovered, "无历史但单轮完整重放窗口后 history_covered 应为 true")
	require.Equal(t, "completed", repo.completeStatus)

	// 第二轮（LatestRun 反映已覆盖）：应继续维持 covered。
	repo.latestRunResult = &UsageRiskRunStatus{Status: "completed", HistoryCovered: true}
	require.NoError(t, svc.runAnalysis(context.Background(), svc.now()))
	require.True(t, repo.completeProgress.HistoryCovered, "第二轮应维持 history_covered=true")
}

// ───────────────────────── 10) 断点续跑：从持久化游标续跑，不从 retentionStart 重放 ─────────────────────────

func TestUsageRiskOrch_CursorResumeFromPersistedPoint(t *testing.T) {
	repo := &orchFakeRepo{}
	svc := newOrchService(t, repo, 10)
	now := svc.now()
	retentionStart := timezone.StartOfDay(now.AddDate(0, 0, -10))

	// 第一轮：预算在第 3 次对账（day 索引 2）后耗尽 → 游标持久化到 day 索引 3。
	ctx, cancel := context.WithCancel(context.Background())
	repo.cancelFn = cancel
	repo.cancelAfterReconciles = 3 // 第 3 次失效（每个对账日一次）触发取消
	require.NoError(t, svc.runAnalysis(ctx, now))
	require.Equal(t, "partial", repo.completeStatus)
	require.True(t, repo.completeProgress.BudgetExhausted)
	require.False(t, repo.completeProgress.HistoryCovered, "首轮未覆盖完成")

	firstRunReconciles := len(repo.reconcileDates)
	require.Equal(t, 3, firstRunReconciles, "首轮应恰好对账 3 个 older 日（0,1,2）")
	cursorDay := retentionStart.AddDate(0, 0, 3) // 游标推进到的下一待处理日

	// 模拟首轮持久化：次轮读取的游标 = cursorDay（覆盖完成前 false）。
	repo.latestRunResult = &UsageRiskRunStatus{Status: "partial", HistoryCovered: false}
	repo.latestCursorResult = &UsageRiskReconCursor{CursorDate: ptrTime(cursorDay), HistoryCovered: false}

	// 第二轮：充足预算，从持久化游标续跑（不应从 retentionStart 重放）。
	require.NoError(t, svc.runAnalysis(context.Background(), now))
	require.Equal(t, "completed", repo.completeStatus, "续跑应能收敛为 completed")
	require.True(t, repo.completeProgress.HistoryCovered, "续跑到 nearStart 后 history_covered 应翻转")

	// 次轮首个对账日必须等于游标日（cursorDay），而非 retentionStart（证明增量续跑）。
	require.Greater(t, len(repo.reconcileDates), firstRunReconciles, "次轮应追加对账")
	secondRunFirst := repo.reconcileDates[firstRunReconciles]
	require.True(t, secondRunFirst.Equal(cursorDay), "次轮首个对账日须为持久化游标日 %s，实际 %s", cursorDay, secondRunFirst)
	require.False(t, secondRunFirst.Equal(retentionStart), "次轮不得从 retentionStart 重放")
	// 次轮不得回退重放首轮已完成的 older 日（day 索引 0,1,2）。
	for _, d := range repo.reconcileDates[firstRunReconciles:] {
		require.False(t, d.Before(cursorDay), "次轮 older 不应回退到游标之前：%s", d)
	}
}

// ───────────────────────── 11) fullReeval：版本变化轮从 retentionStart 重置全量重评 ─────────────────────────

func TestUsageRiskOrch_FullReevalResetsCursorToRetentionStart(t *testing.T) {
	repo := &orchFakeRepo{}
	svc := newOrchService(t, repo, 10)
	now := svc.now()
	retentionStart := timezone.StartOfDay(now.AddDate(0, 0, -10))
	nearStart := timezone.StartOfDay(now.Add(-usageRiskNearWindow))

	// 第一轮：充足预算，完整覆盖（游标推进到 nearStart）。
	require.NoError(t, svc.runAnalysis(context.Background(), now))
	require.Equal(t, "completed", repo.completeStatus)
	require.True(t, repo.completeProgress.HistoryCovered)
	firstRunReconciles := len(repo.reconcileDates)
	require.True(t, repo.reconcileDates[0].Equal(retentionStart), "对照：首轮首个对账须为 retentionStart")

	// 模拟首轮持久化（游标已越过 nearStart）。
	repo.latestRunResult = &UsageRiskRunStatus{Status: "completed", HistoryCovered: true}
	repo.latestCursorResult = &UsageRiskReconCursor{CursorDate: ptrTime(nearStart), HistoryCovered: true}

	// 第二轮：改变策略（listing_min_score）→ 版本变化 → fullReeval，游标重置到 retentionStart。
	stub := svc.settingSvc.settingRepo.(*usageRiskSettingRepoStub)
	stub.values[SettingKeyUsageRiskListingMinScore] = "99" // 与默认 40 不同 → 版本变化
	require.NoError(t, svc.runAnalysis(context.Background(), now))
	require.Equal(t, "completed", repo.completeStatus, "fullReeval 轮应重评完成")
	require.Greater(t, len(repo.reconcileDates), firstRunReconciles)
	secondRunFirst := repo.reconcileDates[firstRunReconciles]
	require.True(t, secondRunFirst.Equal(retentionStart), "fullReeval 须从 retentionStart 全量重评，实际 %s", secondRunFirst)
	require.False(t, secondRunFirst.Equal(nearStart), "fullReeval 不得从持久化游标续跑")
}

// ───────────────────────── 12) 稳态跳过：已覆盖轮只处理近窗，older 零重放 ─────────────────────────

func TestUsageRiskOrch_SteadyStateSkipsOlder(t *testing.T) {
	repo := &orchFakeRepo{}
	svc := newOrchService(t, repo, 10)
	now := svc.now()
	nearStart := timezone.StartOfDay(now.Add(-usageRiskNearWindow))

	// 第一轮：充足预算，完整覆盖（history_covered 翻转）。
	require.NoError(t, svc.runAnalysis(context.Background(), now))
	require.Equal(t, "completed", repo.completeStatus)
	require.True(t, repo.completeProgress.HistoryCovered)
	firstRunReconciles := len(repo.reconcileDates)
	require.Greater(t, firstRunReconciles, 0)

	// 模拟首轮持久化（已覆盖）。
	repo.latestRunResult = &UsageRiskRunStatus{Status: "completed", HistoryCovered: true}
	repo.latestCursorResult = &UsageRiskReconCursor{CursorDate: ptrTime(nearStart), HistoryCovered: true}

	// 第二轮：稳态（已覆盖且非 fullReeval）→ 跳过 older 重放，只处理近窗。
	require.NoError(t, svc.runAnalysis(context.Background(), now))
	require.True(t, repo.completeProgress.HistoryCovered, "稳态应保持 history_covered=true")
	require.Equal(t, "completed", repo.completeStatus)

	// 次轮追加的对账日必须全部 >= nearStart（无 older 重放）。
	require.Greater(t, len(repo.reconcileDates), firstRunReconciles)
	for _, d := range repo.reconcileDates[firstRunReconciles:] {
		require.False(t, d.Before(nearStart), "稳态轮不得重放 older 日：%s", d)
	}
}

// ───────────────────────── 13) 失败批次优先重试 ─────────────────────────

func mustMarshalReconFailed(t *testing.T, fb []reconFailedBatch) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(fb)
	require.NoError(t, err)
	return b
}

func TestUsageRiskOrch_FailedBatchPriorityRetry(t *testing.T) {
	repo := &orchFakeRepo{}
	svc := newOrchService(t, repo, 10)
	now := svc.now()
	retentionStart := timezone.StartOfDay(now.AddDate(0, 0, -10))
	failDay := retentionStart.AddDate(0, 0, 2) // day 索引 2 失败

	// 第一轮：day 2 失败（AggregateHourly 报错），其余 older + 近窗正常。
	repo.failDay = &failDay
	require.NoError(t, svc.runAnalysis(context.Background(), now))
	require.Equal(t, "partial", repo.completeStatus, "有失败批次应为 partial")
	// day 2 不应出现在首轮对账（失败于 AggregateHourly，未提交）。
	for _, d := range repo.reconcileDates {
		require.False(t, d.Equal(failDay), "失败日不应出现在首轮对账序列")
	}

	firstRunReconciles := len(repo.reconcileDates)
	cursorDay := failDay.AddDate(0, 0, 1) // 游标推进越过失败日 → day 3

	// 模拟首轮持久化（游标 day3，HistoryCovered 暂未翻转以走 older 续跑路径；含失败批次）。
	repo.latestRunResult = &UsageRiskRunStatus{Status: "partial", HistoryCovered: false}
	repo.latestCursorResult = &UsageRiskReconCursor{
		CursorDate:     ptrTime(cursorDay),
		HistoryCovered: false,
		FailedBatches:  mustMarshalReconFailed(t, []reconFailedBatch{{Day: failDay, Offset: 0}}),
	}

	// 第二轮：清除 failDay 使重试成功；先重放失败批次 → 首个对账应为 failDay。
	repo.failDay = nil
	require.NoError(t, svc.runAnalysis(context.Background(), now))
	require.Greater(t, len(repo.reconcileDates), firstRunReconciles)
	secondRunFirst := repo.reconcileDates[firstRunReconciles]
	require.True(t, secondRunFirst.Equal(failDay), "失败批次须下轮优先重试（首个对账=失败日），实际 %s", secondRunFirst)

	// 成功后失败批次应从列表移除（不再出现）。
	var remaining []reconFailedBatch
	require.NoError(t, json.Unmarshal(repo.completeProgress.FailedBatches, &remaining))
	require.Len(t, remaining, 0, "失败批次重试成功后须从列表移除")
	// 失败日仅出现一次（重放），不被 older 续跑路径重复。
	count := 0
	for _, d := range repo.reconcileDates[firstRunReconciles:] {
		if d.Equal(failDay) {
			count++
		}
	}
	require.Equal(t, 1, count, "失败日重试后不应被重复对账")
}

// ───────────────────────── 14) 冷启动跨轮覆盖：半窗 partial → 续跑 completed → 稳态维持 ─────────────────────────

func TestUsageRiskOrch_ColdStartCrossRoundCoverage(t *testing.T) {
	repo := &orchFakeRepo{}
	svc := newOrchService(t, repo, 10)
	now := svc.now()
	retentionStart := timezone.StartOfDay(now.AddDate(0, 0, -10))
	nearStart := timezone.StartOfDay(now.Add(-usageRiskNearWindow))

	// 第一轮：预算在 day 2 后耗尽（半窗）→ partial + covered=false。
	ctx, cancel := context.WithCancel(context.Background())
	repo.cancelFn = cancel
	repo.cancelAfterReconciles = 3
	require.NoError(t, svc.runAnalysis(ctx, now))
	require.Equal(t, "partial", repo.completeStatus)
	require.False(t, repo.completeProgress.HistoryCovered)
	firstRunReconciles := len(repo.reconcileDates)

	// 第二轮：充足预算，从游标续跑到 nearStart → completed + covered=true。
	cursorDay := retentionStart.AddDate(0, 0, 3)
	repo.latestRunResult = &UsageRiskRunStatus{Status: "partial", HistoryCovered: false}
	repo.latestCursorResult = &UsageRiskReconCursor{CursorDate: ptrTime(cursorDay), HistoryCovered: false}
	require.NoError(t, svc.runAnalysis(context.Background(), now))
	require.Equal(t, "completed", repo.completeStatus)
	require.True(t, repo.completeProgress.HistoryCovered, "续跑到 nearStart 后 history_covered 须翻转")
	require.Greater(t, len(repo.reconcileDates), firstRunReconciles)
	// 次轮首个对账须为游标日（非 retentionStart），证明增量续跑收敛。
	require.True(t, repo.reconcileDates[firstRunReconciles].Equal(cursorDay), "次轮须从游标续跑")

	// 第三轮：维持稳态（history_covered=true），无 older 重放。
	repo.latestRunResult = &UsageRiskRunStatus{Status: "completed", HistoryCovered: true}
	repo.latestCursorResult = &UsageRiskReconCursor{CursorDate: ptrTime(nearStart), HistoryCovered: true}
	secondRunReconciles := len(repo.reconcileDates)
	require.NoError(t, svc.runAnalysis(context.Background(), now))
	require.True(t, repo.completeProgress.HistoryCovered)
	require.Equal(t, "completed", repo.completeStatus)
	for _, d := range repo.reconcileDates[secondRunReconciles:] {
		require.False(t, d.Before(nearStart), "稳态轮不得重放 older：%s", d)
	}
}

// ───────────────────────── 15) 拆雷回归：失效只可能以"当日完整派生键集"触发 ─────────────────────────
//
// 核心不变量（派发单 U4b-R4）：任何路径的失效调用，其键集必须等于当日完整候选键集；
// 部分键集（如续跑尾段）绝对不得触发失效。本测试直接锁死该点。

func newDailyCandidates(n int) []UsageRiskCandidate {
	out := make([]UsageRiskCandidate, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, UsageRiskCandidate{UserID: int64(i + 1), GroupID: 1, RequestCount: 15})
	}
	return out
}

// sameKeySet 判断两组 (user,group) 键集是否相等（multiset 语义）。
func sameKeySet(u1, g1, u2, g2 []int64) bool {
	if len(u1) != len(u2) {
		return false
	}
	m := map[[2]int64]int{}
	for i := range u1 {
		m[[2]int64{u1[i], g1[i]}]++
	}
	for i := range u2 {
		m[[2]int64{u2[i], g2[i]}]--
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return true
}

func TestUsageRiskOrch_InvalidateOnlyFullKeySet(t *testing.T) {
	repo := &orchFakeRepo{}
	svc := newOrchService(t, repo, 10)
	now := svc.now()

	// 仅在 older 窗口内某日注入 250 候选（>2 个 ≤100 批），其余日空候选。
	retentionStart := timezone.StartOfDay(now.AddDate(0, 0, -10))
	candDay := retentionStart.AddDate(0, 0, 1)
	candList := newDailyCandidates(250)
	repo.candList = candList
	repo.candDay = &candDay

	require.NoError(t, svc.runAnalysis(context.Background(), now))

	// 收集 candDay 的失效调用与 UPSERT-only 调用。
	var candDayInvalidates []invalidateCall
	var candDayUpserts []upsertOnlyCall
	for _, c := range repo.invalidateCalls {
		if c.day.Equal(candDay) {
			candDayInvalidates = append(candDayInvalidates, c)
		} else {
			// 空候选日：失效必须以空键集触发（等价失效该日全部有效旧行）。
			require.Empty(t, c.userIDs, "空候选日失效键集必须为空（失效全部）")
			require.Empty(t, c.groupIDs, "空候选日失效键集必须为空（失效全部）")
		}
	}
	for _, u := range repo.upsertOnlyCalls {
		if u.day.Equal(candDay) {
			candDayUpserts = append(candDayUpserts, u)
		}
	}

	// 日完成：candDay 恰一次失效调用（最后一批之后）。
	require.Len(t, candDayInvalidates, 1, "candDay 日完成应恰一次失效调用")
	inv := candDayInvalidates[0]
	// 拆雷锁死：失效键集必须等于当日完整候选键集（250 个），绝不允许部分集。
	require.Equal(t, 250, len(inv.userIDs), "失效键集必须等于当日完整候选键集")
	require.True(t, sameKeySet(inv.userIDs, inv.groupIDs, candUserIDs(candList), candGroupIDs(candList)),
		"失效键集必须等于当日完整候选键集（部分集即触发雷点）")

	// candDay 的 UPSERT-only 批次数 = 250/100 上取整 = 3，且每个批都带记录（绝不空提交）。
	require.Len(t, candDayUpserts, 3, "candDay 应有 3 个 ≤100 批")
	for _, u := range candDayUpserts {
		require.NotEmpty(t, u.records, "批 UPSERT 必须带记录")
	}
}

// ───────────────────────── 16) 批边界续跑：中断于批 k → 下轮从精确批边界续跑，已提交批不重做 ─────────────────────────

func TestUsageRiskOrch_BatchBoundaryResume(t *testing.T) {
	repo := &orchFakeRepo{}
	svc := newOrchService(t, repo, 10)
	now := svc.now()
	retentionStart := timezone.StartOfDay(now.AddDate(0, 0, -10))
	candDay := retentionStart.AddDate(0, 0, 2) // older 窗口内 day 索引 2
	candList := newDailyCandidates(250)         // 3 个 ≤100 批
	repo.candList = candList
	repo.candDay = &candDay

	// 第一轮：第 2 次 UPSERT 后触发预算耗尽（中断于批 1 末尾，下一批起点 offset=200）。
	ctx, cancel := context.WithCancel(context.Background())
	repo.cancelFn = cancel
	repo.cancelAfterUpserts = 2
	require.NoError(t, svc.runAnalysis(ctx, now))
	require.Equal(t, "partial", repo.completeStatus, "预算耗尽须为 partial")
	// 第一轮 candDay 仅提交前 2 批（[0,200)），未日完成（无失效调用）。
	var r1Upserts int
	for _, u := range repo.upsertOnlyCalls {
		if u.day.Equal(candDay) {
			r1Upserts++
		}
	}
	require.Equal(t, 2, r1Upserts, "第一轮 candDay 应仅提交前 2 批（[0,200)）")
	var r1Invalidates int
	for _, c := range repo.invalidateCalls {
		if c.day.Equal(candDay) {
			r1Invalidates++
		}
	}
	require.Equal(t, 0, r1Invalidates, "第一轮 candDay 未完成不得触发失效")

	// 模拟首轮持久化：游标越过 candDay（candDay+1），失败批次带偏移 200（已 UPSERT 批不重做）。
	repo.latestRunResult = &UsageRiskRunStatus{Status: "partial", HistoryCovered: false}
	repo.latestCursorResult = &UsageRiskReconCursor{
		CursorDate: ptrTime(candDay.AddDate(0, 0, 1)),
		BatchOffset: usageRiskIntPtr(0),
		HistoryCovered: false,
		FailedBatches: mustMarshalReconFailed(t, []reconFailedBatch{{Day: candDay, Offset: 200}}),
	}
	// 第二轮：充足预算，关闭批内取消钩子。
	repo.cancelAfterUpserts = 0
	repo.cancelFn = nil
	require.NoError(t, svc.runAnalysis(context.Background(), now))
	require.Equal(t, "completed", repo.completeStatus, "续跑应收敛为 completed")

	// 第二轮 candDay 仅补做剩余 1 批（[200,250)），不重做 [0,200)。
	var r2Upserts int
	for _, u := range repo.upsertOnlyCalls {
		if u.day.Equal(candDay) {
			r2Upserts++
		}
	}
	require.Equal(t, 3, r2Upserts, "两轮合计 candDay 应仅 3 批（已提交批不重做）")
	require.Equal(t, 1, r2Upserts-r1Upserts, "第二轮 candDay 应仅补做 1 批")

	// 已提交批不重做：两轮合并 candDay 的 UPSERT 键集必须恰好等于全部 250 候选（无重复）。
	var upsertedUsers, upsertedGroups []int64
	for _, u := range repo.upsertOnlyCalls {
		if !u.day.Equal(candDay) {
			continue
		}
		for _, rec := range u.records {
			upsertedUsers = append(upsertedUsers, rec.UserID)
			upsertedGroups = append(upsertedGroups, rec.GroupID)
		}
	}
	require.True(t, sameKeySet(upsertedUsers, upsertedGroups, candUserIDs(candList), candGroupIDs(candList)),
		"candDay 两轮 UPSERT 键集必须恰好等于全部候选（无遗漏、无重复重做）")
	require.Len(t, upsertedUsers, 250, "candDay UPSERT 总数须为 250（无重做）")

	// 失败批次重试完成后应从列表移除，且 candDay 仅一次失效（键集=全集）。
	var remainingFB []reconFailedBatch
	require.NoError(t, json.Unmarshal(repo.completeProgress.FailedBatches, &remainingFB))
	require.Len(t, remainingFB, 0, "失败批次重试完成后须从列表移除")
	var r2Invalidates int
	for _, c := range repo.invalidateCalls {
		if c.day.Equal(candDay) {
			r2Invalidates++
			require.Equal(t, 250, len(c.userIDs), "candDay 失效键集须=全集（部分集即雷点）")
			require.True(t, sameKeySet(c.userIDs, c.groupIDs, candUserIDs(candList), candGroupIDs(candList)))
		}
	}
	require.Equal(t, 1, r2Invalidates, "candDay 应仅一次失效（日完成时）")
}

// candUserIDs / candGroupIDs 提取候选键集（供"失效仅全键集"断言比较）。
func candUserIDs(cands []UsageRiskCandidate) []int64 {
	out := make([]int64, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.UserID)
	}
	return out
}
func candGroupIDs(cands []UsageRiskCandidate) []int64 {
	out := make([]int64, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.GroupID)
	}
	return out
}

// ───────────────────────── R7-2 锁定测试：S1 pending 重评全窗口可续跑重放 ─────────────────────────
//
// S1 语义：
//   - pending R1 重评轮（prevR1ReevalPending=true 且非 fullReeval）olderStart 重置到 retentionStart，
//     cursorOffset=0（与 fullReeval 同构），但 failedBatches 不清空；
//   - 条件清除：仅当本轮重放完成（olderFullyDone && len(failedBatches)==0 && !budgetExhausted）才置 false，
//     否则保持 prevR1ReevalPending（下轮从游标续跑）。
//
// 场景 1（完整重放清 pending）：prev 已覆盖且 pending=true → 本轮以 retentionStart 起始日重放全窗口
// （非空 older 窗口），完成且无失败 → pending 清 false。
// 场景 2（预算耗尽中断保持）：cancelAfterReconciles=1 → 首日失效即取消 → pending 保持 true，
// 游标停在断点（retentionStart+1），重放未完成。
func TestUsageRiskR7_2_S1PendingReevalFullWindowReplay(t *testing.T) {
	// markNotFullReeval 设置 lastPolicyVersion 使本轮 thisRoundReevalR1 成立（而非 fullReeval）。
	markNotFullReeval := func(svc *UsageRiskAnalysisService) {
		pol, _, _, err := svc.loadPolicy(context.Background())
		require.NoError(t, err)
		svc.lastPolicyVersion = UsageRiskPolicyVersion(pol, timezone.Name())
	}

	t.Run("pending round replays from retentionStart then clears", func(t *testing.T) {
		repo := &orchFakeRepo{}
		svc := newOrchService(t, repo, 10)
		now := svc.now()
		retentionStart := timezone.StartOfDay(now.AddDate(0, 0, -10))

		markNotFullReeval(svc)
		// R8-2/F2 翻转轮回卷持久化：pending=true 且上一轮非 pending → 翻转轮统一持久化时
		// 将 ReconCursorDate 回卷到 retentionStart。此处模拟翻转轮已持久化后的状态——
		// 首个 pending 重放轮从该回卷点（retentionStart）续跑（非 nearStart，非空转）。
		repo.latestRunResult = &UsageRiskRunStatus{Status: "completed", HistoryCovered: true, R1ReevalPending: true}
		repo.latestCursorResult = &UsageRiskReconCursor{CursorDate: ptrTime(retentionStart), HistoryCovered: true}

		require.NoError(t, svc.runAnalysis(context.Background(), now))

		// S1 核心：pending 轮 reconcileDay 以 retentionStart 起始日被调用（非空 older 窗口，非空转）。
		// RebuildHourlyWindow 每日一次（reconcileDay 内首步），首个逐日回填窗口（Start 为整日 00:00）须为 retentionStart；
		// 近窗重建窗口（Start=now-26h，非整日）在 reconcileDay 之前，需跳过。
		require.NotEmpty(t, repo.rebuildWindows, "pending 重评轮必须发生逐日回填（不得空转零日）")
		var firstDaily *rebuildWindow
		for i := range repo.rebuildWindows {
			if repo.rebuildWindows[i].Start.Equal(timezone.StartOfDay(repo.rebuildWindows[i].Start)) {
				firstDaily = &repo.rebuildWindows[i]
				break
			}
		}
		require.NotNil(t, firstDaily, "pending 重评轮必须存在逐日回填窗口")
		require.True(t, firstDaily.Start.Equal(retentionStart),
			"pending 重评轮首个 older 日须为 retentionStart=%s，实际=%s", retentionStart, firstDaily.Start)
		// 重放完成且无失败 → pending 清 false。
		require.False(t, repo.completeProgress.R1ReevalPending, "S1：完整重放完成且无失败 → pending 清 false")
		require.True(t, repo.completeProgress.HistoryCovered, "已覆盖实例维持覆盖")
		require.Equal(t, "completed", repo.completeStatus)
	})

	t.Run("budget-exhausted replay keeps pending and cursor at breakpoint", func(t *testing.T) {
		repo := &orchFakeRepo{}
		svc := newOrchService(t, repo, 10)
		now := svc.now()
		retentionStart := timezone.StartOfDay(now.AddDate(0, 0, -10))

		markNotFullReeval(svc)
		repo.latestRunResult = &UsageRiskRunStatus{Status: "completed", HistoryCovered: true, R1ReevalPending: true}
		// R8-2/F2：翻转轮回卷持久化后，首个 pending 轮从回卷点（retentionStart）起跑。
		repo.latestCursorResult = &UsageRiskReconCursor{CursorDate: ptrTime(retentionStart), HistoryCovered: true}

		// 首日失效（预留段第一个 older 日完成）后取消 → 重放中断。
		ctx, cancel := context.WithCancel(context.Background())
		repo.cancelFn = cancel
		repo.cancelAfterReconciles = 1

		require.NoError(t, svc.runAnalysis(ctx, now))
		require.Equal(t, "partial", repo.completeStatus, "预算耗尽中断重放须为 partial")
		require.True(t, repo.completeProgress.R1ReevalPending, "S1：预算耗尽中断重放 → pending 必须保持 true（下轮续跑）")
		require.True(t, repo.completeProgress.HistoryCovered, "已覆盖实例通过 A 组语义维持 history_covered=true（覆盖判定不变）")
		// 游标停在断点：预留段首个 older 日之后。
		breakpoint := retentionStart.AddDate(0, 0, 1)
		require.NotNil(t, repo.completeProgress.ReconCursorDate, "游标须已持久化")
		require.True(t, repo.completeProgress.ReconCursorDate.Equal(breakpoint),
			"S1：游标应停在断点 %s，实际 %s", breakpoint, repo.completeProgress.ReconCursorDate.Format("2006-01-02"))
	})
}

// S1 续跑：中断后下轮（pending 保持）仍从 retentionStart 重放（pending 轮恒重置 olderStart），
// 完成即清 pending；游标单调不回退。
func TestUsageRiskR7_2_S1PendingReplayResumeAfterInterrupt(t *testing.T) {
	repo := &orchFakeRepo{}
	svc := newOrchService(t, repo, 10)
	now := svc.now()
	retentionStart := timezone.StartOfDay(now.AddDate(0, 0, -10))
	pol, _, _, err := svc.loadPolicy(context.Background())
	require.NoError(t, err)
	svc.lastPolicyVersion = UsageRiskPolicyVersion(pol, timezone.Name())

	// 第一轮：中断于首日失效。
	repo.latestRunResult = &UsageRiskRunStatus{Status: "completed", HistoryCovered: true, R1ReevalPending: true}
	ctx, cancel := context.WithCancel(context.Background())
	repo.cancelFn = cancel
	repo.cancelAfterReconciles = 1
	require.NoError(t, svc.runAnalysis(ctx, now))
	require.True(t, repo.completeProgress.R1ReevalPending, "首轮中断 → pending 保持")

	// 第二轮：模拟持久化（首轮结尾 A 组维持 HC=true、pending 仍 true），充足预算，重放走完 → pending 清 false。
	repo.latestRunResult = &UsageRiskRunStatus{Status: "partial", HistoryCovered: true, R1ReevalPending: true}
	repo.latestCursorResult = &UsageRiskReconCursor{CursorDate: ptrTime(retentionStart.AddDate(0, 0, 1)), HistoryCovered: true}
	repo.cancelAfterReconciles = 0
	repo.cancelFn = nil
	require.NoError(t, svc.runAnalysis(context.Background(), now))
	require.Equal(t, "completed", repo.completeStatus, "续跑重放完成应为 completed")
	require.False(t, repo.completeProgress.R1ReevalPending, "S1：续跑重放完成且无失败 → pending 清 false")
	require.True(t, repo.completeProgress.HistoryCovered, "重放走完后覆盖翻转/维持 true")
	// 游标单调：终态游标须越过 nearStart（全窗口走完）。
	require.True(t, repo.completeProgress.ReconCursorDate != nil &&
		!repo.completeProgress.ReconCursorDate.Before(timezone.StartOfDay(now.Add(-usageRiskNearWindow))),
		"重放完成后游标须推进到 nearStart 及以上（不早退）")
}

// ───────────────────────── R7-2 锁定测试：S2 预算次序 = 预留 older 批 → 近窗 → 余量续 older ─────────────────────────
//
// 冷启动轮（older 积压大于单轮预算，中断于近窗首日）：
//   - 近窗日本轮已处理（reconcileDates 含 nearStart）——不被 older 段耗尽而饿死；
//   - older 至少 1 日完成（预留段，reconcileDates[0]==retentionStart）；
//   - 游标持久化在 older 断点（retentionStart+1）。
func TestUsageRiskR7_2_S2NearWindowSurvivesOlderBacklog(t *testing.T) {
	repo := &orchFakeRepo{}
	svc := newOrchService(t, repo, 10)
	now := svc.now()
	retentionStart := timezone.StartOfDay(now.AddDate(0, 0, -10))
	nearStart := timezone.StartOfDay(now.Add(-usageRiskNearWindow))

	// 无历史（冷启动 prevRun=nil）→ 非稳态。cancelAfterReconciles=3：
	//   新行为（预留段 1 日 → 近窗段）第 3 次失效发生在近窗第二日 → 近窗 9-19 已处理；
	//   旧行为（older 一跑到底）第 3 次失效发生在 older 第 3 日（9-12）→ 近窗整轮跳过。
	ctx, cancel := context.WithCancel(context.Background())
	repo.cancelFn = cancel
	repo.cancelAfterReconciles = 3

	require.NoError(t, svc.runAnalysis(ctx, now))
	require.Equal(t, "partial", repo.completeStatus)
	require.True(t, repo.completeProgress.BudgetExhausted)

	// S2 核心 1：近窗日本轮已处理（reconcileDates 含 >=nearStart 的日）——older 积压不得饿死近窗。
	require.NotEmpty(t, repo.reconcileDates, "本轮须发生对账")
	foundNear := false
	for _, d := range repo.reconcileDates {
		if !d.Before(nearStart) {
			foundNear = true
			break
		}
	}
	require.True(t, foundNear, "S2：近窗日（>=nearStart）本轮必须已处理（older 积压不得饿死近窗）")

	// S2 核心 2：older 至少 1 日完成（预留段先行），且首个对账日须为 retentionStart。
	require.True(t, repo.reconcileDates[0].Equal(retentionStart),
		"S2：首个对账批次须为 older 预留段（retentionStart），实际=%s", repo.reconcileDates[0])

	// S2 核心 3：游标持久化在 older 断点（预留段完成后 = retentionStart+1）。
	// R13-1 改动 3b 适配：冷启动轮是 fullReeval（prevRun=nil），fullReeval 轮收尾置
	// r1_reeval_pending=true 并回卷游标到 retentionStart（下轮以完整新桶全窗重评 R1，
	// 见 usage_risk_analysis_service.go fullReeval case 注释）——预算耗尽 partial 的
	// 冷启动轮不再停在 older 断点，而是回卷 retentionStart。下轮续跑从 retentionStart
	// 全窗重放（含续 older 段），断点语义由「日期+批内偏移」游标本身承担，不丢失。
	require.NotNil(t, repo.completeProgress.ReconCursorDate)
	require.True(t, repo.completeProgress.ReconCursorDate.Equal(retentionStart),
		"S2（R13-1 改动 3b 适配）：fullReeval partial 轮须回卷游标到 retentionStart，实际=%s", repo.completeProgress.ReconCursorDate.Format("2006-01-02"))
}

// ───────────────────────── R7-2 锁定测试：S3 candidates_total 跨日累计 ─────────────────────────
//
// 主场景：预留段首日（retentionStart）承载 n1、续 older 首日（retentionStart+1）承载 n2；
// 执行次序为 预留段(9-10,n1) → 近窗(空) → 续 older(9-11,n2) → …，
// UpdateRunProgress 历史携带递增 CandidatesTotal（0→n1→n1+n2），终态台账累计 = n1+n2。
func TestUsageRiskR7_2_S3CandidatesTotalAccumulatesAcrossDays(t *testing.T) {
	repo := &orchFakeRepo{}
	svc := newOrchService(t, repo, 10)
	now := svc.now()
	retentionStart := timezone.StartOfDay(now.AddDate(0, 0, -10))

	n1, n2 := 3, 5
	day1 := retentionStart                // 预留段首个 older 日
	day2 := retentionStart.AddDate(0, 0, 1) // 续 older 段首日
	repo.candByDay = map[time.Time][]UsageRiskCandidate{
		day1: newDailyCandidates(n1),
		day2: newDailyCandidates(n2),
	}

	require.NoError(t, svc.runAnalysis(context.Background(), now))
	require.Equal(t, "completed", repo.completeStatus)

	// S3 核心：UpdateRunProgress 历史携带递增 CandidatesTotal（0→n1→n1+n2）。
	require.NotEmpty(t, repo.updateProgressHistory, "须有进度持久化记录")
	sawZero := false
	sawN1 := false
	sawN12 := false
	for _, p := range repo.updateProgressHistory {
		switch p.CandidatesTotal {
		case 0:
			sawZero = true
		case n1:
			sawN1 = true
		case n1 + n2:
			sawN12 = true
		}
	}
	require.True(t, sawZero, "S3：累计起点须为 0")
	require.True(t, sawN1, "S3：处理 day1（n1=%d 候选）后 CandidatesTotal 须出现 %d", n1, n1)
	require.True(t, sawN12, "S3：处理 day2 后 CandidatesTotal 须累计为 %d", n1+n2)
	require.Equal(t, n1+n2, repo.completeProgress.CandidatesTotal,
		"S3：终态台账 CandidatesTotal 须等于两日候选总数 %d", n1+n2)
}

// 近窗日候选数也须计入累计（近窗段累计路径与 older 段独立实现）。
func TestUsageRiskR7_2_S3CandidatesTotalNearWindowDayCounted(t *testing.T) {
	repo := &orchFakeRepo{}
	svc := newOrchService(t, repo, 10)
	now := svc.now()
	nearStart := timezone.StartOfDay(now.Add(-usageRiskNearWindow))

	n3 := 4
	nearDay := nearStart.AddDate(0, 0, 1) // 近窗窗口内后一日（9-20）
	repo.candByDay = map[time.Time][]UsageRiskCandidate{
		nearDay: newDailyCandidates(n3),
	}

	require.NoError(t, svc.runAnalysis(context.Background(), now))
	require.Equal(t, "completed", repo.completeStatus)
	require.Equal(t, n3, repo.completeProgress.CandidatesTotal,
		"S3：近窗日候选数须计入台账累计（近窗口径），实际=%d", repo.completeProgress.CandidatesTotal)
}

// ───────────────────────── R8-2b 锁定测试：F2 翻转轮回卷 + 断点续跑 ─────────────────────────
//
// F2（外审 #4）：pending 重评的「回卷」由翻转轮（覆盖 false→true、pending 从 false→true）
// 统一持久化一次（ReconCursorDate=retentionStart、offset=0）；后续 pending 轮一律消费持久化
// 游标（回卷点或断点）续跑，不再每轮重置 olderStart。
// mutation 反证：临时恢复 R8-2/F2 删除的「每轮重置块」（thisRoundReevalR1 时 olderStart 重置
// retentionStart）→ 本测试的第三轮（断点续跑断言）必然 FAIL；验毕还原。

func TestUsageRiskR8_2b_F2FlipRollbackThenBreakpointResume(t *testing.T) {
	// markNotFullReeval 设置 lastPolicyVersion 使本轮非 fullReeval（pending 重评路径而非全量重评）。
	markNotFullReeval := func(svc *UsageRiskAnalysisService) {
		pol, _, _, err := svc.loadPolicy(context.Background())
		require.NoError(t, err)
		svc.lastPolicyVersion = UsageRiskPolicyVersion(pol, timezone.Name())
	}

	t.Run("flip round persists rollback to retentionStart", func(t *testing.T) {
		repo := &orchFakeRepo{}
		svc := newOrchService(t, repo, 10)
		now := svc.now()
		retentionStart := timezone.StartOfDay(now.AddDate(0, 0, -10))

		markNotFullReeval(svc)
		// 翻转轮：prev 未覆盖且非 pending → 本轮覆盖由 false→true 完成翻转为 pending。
		repo.latestRunResult = &UsageRiskRunStatus{Status: "partial", HistoryCovered: false}

		require.NoError(t, svc.runAnalysis(context.Background(), now))

		require.True(t, repo.completeProgress.HistoryCovered, "翻转轮须翻转为覆盖")
		require.True(t, repo.completeProgress.R1ReevalPending, "翻转轮须置 pending=true")
		// R8-2/F2 核心：翻转轮统一持久化回卷点——ReconCursorDate=retentionStart、offset=0。
		require.NotNil(t, repo.completeProgress.ReconCursorDate, "回卷游标须非空")
		require.True(t, repo.completeProgress.ReconCursorDate.Equal(retentionStart),
			"翻转轮须持久化游标回卷到 retentionStart=%s，实际=%s", retentionStart, repo.completeProgress.ReconCursorDate.Format("2006-01-02"))
		require.NotNil(t, repo.completeProgress.ReconBatchOffset)
		require.Equal(t, 0, *repo.completeProgress.ReconBatchOffset, "翻转轮回卷 offset 须为 0")
	})

	t.Run("pending round after breakpoint resumes from persisted cursor", func(t *testing.T) {
		repo := &orchFakeRepo{}
		svc := newOrchService(t, repo, 10)
		now := svc.now()
		retentionStart := timezone.StartOfDay(now.AddDate(0, 0, -10))

		markNotFullReeval(svc)
		// 翻转轮已持久化：覆盖 + pending，游标=回卷点 retentionStart。
		repo.latestRunResult = &UsageRiskRunStatus{Status: "completed", HistoryCovered: true, R1ReevalPending: true}
		repo.latestCursorResult = &UsageRiskReconCursor{CursorDate: ptrTime(retentionStart), HistoryCovered: true}

		// Pending 轮 1：首日（retentionStart）完成后预算耗尽 → 断点 = retentionStart+1。
		ctx, cancel := context.WithCancel(context.Background())
		repo.cancelFn = cancel
		repo.cancelAfterReconciles = 1
		require.NoError(t, svc.runAnalysis(ctx, now))
		require.Equal(t, "partial", repo.completeStatus)
		require.True(t, repo.completeProgress.R1ReevalPending, "中断重放须保持 pending")
		breakpoint := retentionStart.AddDate(0, 0, 1)
		require.NotNil(t, repo.completeProgress.ReconCursorDate)
		require.True(t, repo.completeProgress.ReconCursorDate.Equal(breakpoint),
			"pending 轮中断后游标须停在断点 %s，实际 %s", breakpoint, repo.completeProgress.ReconCursorDate.Format("2006-01-02"))
		firstRoundReconciles := len(repo.reconcileDates)

		// Pending 轮 2：消费持久化断点续跑（olderStart=断点日 retentionStart+1 ≠ retentionStart）。
		repo.latestRunResult = &UsageRiskRunStatus{Status: "partial", HistoryCovered: true, R1ReevalPending: true}
		repo.latestCursorResult = &UsageRiskReconCursor{CursorDate: ptrTime(breakpoint), HistoryCovered: true}
		repo.cancelAfterReconciles = 0
		repo.cancelFn = nil
		require.NoError(t, svc.runAnalysis(context.Background(), now))
		require.Equal(t, "completed", repo.completeStatus, "断点续跑应收敛 completed")
		require.False(t, repo.completeProgress.R1ReevalPending, "续跑重放完成 → pending 清 false")

		// R8-2/F2 核心断言：第二 pending 轮首个对账日 = 持久化断点日（≠ retentionStart）。
		// mutation 反证：若恢复"每轮重置块"（thisRoundReevalR1 时 olderStart 重置 retentionStart），
		// 此处第二个对账日将为 retentionStart → 本断言 FAIL。
		require.Greater(t, len(repo.reconcileDates), firstRoundReconciles, "次轮应追加对账")
		secondRoundFirst := repo.reconcileDates[firstRoundReconciles]
		require.True(t, secondRoundFirst.Equal(breakpoint),
			"R8-2/F2：第二 pending 轮须从持久化断点 %s 续跑（olderStart≠retentionStart），实际 %s",
			breakpoint, secondRoundFirst.Format("2006-01-02"))
		require.False(t, secondRoundFirst.Equal(retentionStart),
			"R8-2/F2：第二 pending 轮 olderStart 不得被重置回 retentionStart（消费持久化断点，不每轮重置）")
	})
}

// ───────────────────────── R8-2b 锁定测试：F3 近窗日去重 ─────────────────────────
//
// 上一轮近窗日失败（FailedBatches 含该日）→ 本轮 retry 成功 + 近窗循环跳过该日
// （attempted[day] continue）→ CandidatesTotal 对该日恰计一次。双失败场景：
// retry 仍失败（该日保留于列表）+ 近窗循环跳过 → failedBatches 无重复项。
// 稳态构造：prev 已覆盖且非 pending 且非 fullReeval → steadySkip=true（跳过 older），
// 近窗循环只处理近窗窗口 [nearStart, reportNowDay)。

func TestUsageRiskR8_2b_F3NearWindowRetrySkipDup(t *testing.T) {
	repo := &orchFakeRepo{}
	svc := newOrchService(t, repo, 10)
	now := svc.now()
	nearStart := timezone.StartOfDay(now.Add(-usageRiskNearWindow))
	day := nearStart.AddDate(0, 0, 1) // 近窗窗口内日（9-20）

	// 稳态：已覆盖、非 pending → 跳过 older，只处理近窗。
	repo.latestRunResult = &UsageRiskRunStatus{Status: "completed", HistoryCovered: true}
	repo.latestCursorResult = &UsageRiskReconCursor{CursorDate: ptrTime(nearStart), HistoryCovered: true}
	// 上一轮该近窗日失败 → FailedBatches 含该日（offset 0）。
	repo.latestCursorResult.FailedBatches = mustMarshalReconFailed(t, []reconFailedBatch{{Day: day, Offset: 0}})
	// 该日 5 候选（retry 成功路径返回 cands=5）。
	repo.candByDay = map[time.Time][]UsageRiskCandidate{day: newDailyCandidates(5)}

	markNotFullReevalLocal(svc)
	require.NoError(t, svc.runAnalysis(context.Background(), now))

	// F3 核心：该日恰计一次——CandidatesTotal == 5（retry 成功计一次；近窗循环跳过不再计）。
	require.Equal(t, 5, repo.completeProgress.CandidatesTotal,
		"R8-2b/F3：近窗日 retry 成功 + 近窗循环跳过 → CandidatesTotal 恰计一次（=5），实际=%d", repo.completeProgress.CandidatesTotal)
	// 该日 reconcileDates 中恰出现一次（仅 retry 那次；近窗循环未重对账）。
	count := 0
	for _, d := range repo.reconcileDates {
		if d.Equal(day) {
			count++
		}
	}
	require.Equal(t, 1, count, "R8-2b/F3：近窗日仅被 retry 对账一次，近窗循环不得重复")
	// 失败批次清空（重试成功已移除）。
	var fb []reconFailedBatch
	require.NoError(t, json.Unmarshal(repo.completeProgress.FailedBatches, &fb))
	require.Len(t, fb, 0, "R8-2b/F3：retry 成功后失败批次须移除")
}

// F3 双失败：近窗日 retry 仍失败 → 保留于列表；近窗循环跳过该日 → failedBatches 无重复项。
func TestUsageRiskR8_2b_F3NearWindowDoubleFailNoDup(t *testing.T) {
	repo := &orchFakeRepo{}
	svc := newOrchService(t, repo, 10)
	now := svc.now()
	nearStart := timezone.StartOfDay(now.Add(-usageRiskNearWindow))
	day := nearStart.AddDate(0, 0, 1)

	repo.latestRunResult = &UsageRiskRunStatus{Status: "completed", HistoryCovered: true}
	repo.latestCursorResult = &UsageRiskReconCursor{CursorDate: ptrTime(nearStart), HistoryCovered: true}
	repo.latestCursorResult.FailedBatches = mustMarshalReconFailed(t, []reconFailedBatch{{Day: day, Offset: 0}})
	// 该日持续失败（AggregateHourly 对该日报错 → reconcileDay 返回 err）：retry 失败 → 保留。
	repo.failDay = &day
	// 候选注入（走 collectDayCandidates 前的聚合失败即返回 err，无需候选也能触发）。
	repo.candByDay = map[time.Time][]UsageRiskCandidate{day: newDailyCandidates(5)}

	markNotFullReevalLocal(svc)
	require.NoError(t, svc.runAnalysis(context.Background(), now))

	// 失败批次仅一条（retry 保留）；近窗循环 skip 该日 → 不重复 append。
	var fb []reconFailedBatch
	require.NoError(t, json.Unmarshal(repo.completeProgress.FailedBatches, &fb))
	require.Len(t, fb, 1, "R8-2b/F3：双失败场景 failedBatches 不得有重复项（近窗循环跳过 retry 已处理日）")
	require.True(t, fb[0].Day.Equal(day), "保留的失败批次须为原近窗日")
	require.Equal(t, 0, fb[0].Offset)
}

// ───────────────────────── R9-2 锁定测试：retry 失败批次保留最新批内偏移（外审⑤ F2 整改） ─────────────────────────
//
// 外审⑤ F2：retry-first 块丢弃 reconcileDay 返回的 nextOffset，失败时重加原始 fb（旧 offset）。
// 批级 UPSERT 失败（reconcileDay :788）与预算耗尽（:805）时 nextOffset=已提交批后位置（部分进展）——
// 丢弃导致下轮重做已提交批、同位置反复耗尽永久停滞，违背「日期+批内偏移」断点续跑语义。
//
// 本测试：上一轮持久化 FailedBatches=[{Day:D, Offset:0}]（retry 从 0 起跑）；该日 250 候选=3 批，
// 注入该日第 3 次 UPSERT 调用失败（批起点 offset=(3-1)*100=200）→ 前 2 批已提交、第 3 批失败，
// reconcileDay 返回 nextOffset=200。第一轮断言 FailedBatches=[{D,200}]（锁 nextOffset 保留；
// 若实现退化为重加原始 fb，这里为 Offset:0 → FAIL，即 mutation 反证）。第二轮清除注入续跑：
// 从 offset=200 断点续跑只补 1 批，不回卷重做 [0,200)；FailedBatches 清空、总 UPSERT 恰 3 批、
// 键集=全部 250 候选、该日恰 1 次失效（日完成时）。
func TestUsageRiskR9_2_RetryPartialProgressKeepsNextOffset(t *testing.T) {
	repo := &orchFakeRepo{}
	svc := newOrchService(t, repo, 10)
	now := svc.now()
	nearStart := timezone.StartOfDay(now.Add(-usageRiskNearWindow))
	// R10-2/#5 适配：断点续跑语义仅对冻结 older 日成立（近窗日活数据每轮强制 offset=0 全量重算），
	// 故本测试的失败日取 nearStart 前一日（older 区间）——200-vs-0 断点断言语义不变。
	D := nearStart.AddDate(0, 0, -1)

	// 稳态：已覆盖、非 pending → steadySkip 跳过 older；retry-first 块照常执行（D 在 FailedBatches 内）。
	repo.latestRunResult = &UsageRiskRunStatus{Status: "completed", HistoryCovered: true}
	repo.latestCursorResult = &UsageRiskReconCursor{CursorDate: ptrTime(nearStart), HistoryCovered: true}
	// 上一轮该日持久化失败批次 offset=0（retry 从 0 起跑）。
	repo.latestCursorResult.FailedBatches = mustMarshalReconFailed(t, []reconFailedBatch{{Day: D, Offset: 0}})
	// 该日 250 候选 = 3 批（[0,100)、[100,200)、[200,250)）。
	repo.candByDay = map[time.Time][]UsageRiskCandidate{D: newDailyCandidates(250)}
	// 注入该日第 3 次 UPSERT 失败（批起点 offset=200 失败；此前 2 批已提交，nextOffset=200）。
	repo.failUpsertNth = map[time.Time]int{D: 3}

	markNotFullReevalLocal(svc)

	// ── 第一轮：retry 从 Offset:0 跑，前 2 批成功、第 3 批失败 ──
	require.NoError(t, svc.runAnalysis(context.Background(), now))
	require.Equal(t, "partial", repo.completeStatus, "R9-2 第一轮：批级失败须以 partial 收尾")

	// 断言 1：FailedBatches 恰 1 条，Offset=200（锁 nextOffset 保留，断点续跑语义）。
	//   若实现退化为重加原始 fb（旧 offset=0），此处 Offset 为 0 → FAIL（mutation 反证点）。
	var fb []reconFailedBatch
	require.NoError(t, json.Unmarshal(repo.completeProgress.FailedBatches, &fb))
	require.Len(t, fb, 1, "R9-2 第一轮：失败批次须恰 1 条（不重复、不丢弃）")
	require.True(t, fb[0].Day.Equal(D), "R9-2 第一轮：失败批次 Day 须为原失败日 D")
	require.Equal(t, 200, fb[0].Offset,
		"R9-2 第一轮：失败批次 Offset 须保留 reconcileDay 返回的 nextOffset=200（=已提交批后位置），"+
			"不得退回原始 fb.Offset=0（否则下轮重做已提交批、同位置反复耗尽永久停滞）")

	// 断言 2：该日第一轮已提交批恰 2 批（[0,100)、[100,200)）；第 3 批失败未提交。
	r1Upserts := 0
	for _, u := range repo.upsertOnlyCalls {
		if u.day.Equal(D) {
			r1Upserts++
		}
	}
	require.Equal(t, 2, r1Upserts, "R9-2 第一轮：该日应仅提交前 2 批（[0,200)），失败的第 3 批不得计入已提交批")
	r1Invalidates := 0
	for _, c := range repo.invalidateCalls {
		if c.day.Equal(D) {
			r1Invalidates++
		}
	}
	require.Equal(t, 0, r1Invalidates, "R9-2 第一轮：该日未完成（批失败）不得触发失效")

	// ── 第二轮：清除失败注入，以上一轮收尾状态续跑 retry 路径（FailedBatches 保留 Offset:200）──
	delete(repo.failUpsertNth, D)
	repo.latestRunResult = &UsageRiskRunStatus{Status: "partial", HistoryCovered: true}
	repo.latestCursorResult = &UsageRiskReconCursor{
		CursorDate:     ptrTime(nearStart),
		HistoryCovered: true,
		FailedBatches:  mustMarshalReconFailed(t, []reconFailedBatch{{Day: D, Offset: 200}}),
	}
	require.NoError(t, svc.runAnalysis(context.Background(), now))
	require.Equal(t, "completed", repo.completeStatus, "R9-2 第二轮：断点续跑成功应收敛为 completed")

	// 断言 3：续跑成功后 FailedBatches 清空（移除已完成批次）。
	var fb2 []reconFailedBatch
	require.NoError(t, json.Unmarshal(repo.completeProgress.FailedBatches, &fb2))
	require.Len(t, fb2, 0, "R9-2 第二轮：失败批次续跑成功后须从列表移除")

	// 断言 4：两轮合并该日 UPSERT 总批数恰 3（第二轮只补 [200,250) 1 批，未回卷重做 [0,200)）。
	totalUpserts := 0
	for _, u := range repo.upsertOnlyCalls {
		if u.day.Equal(D) {
			totalUpserts++
		}
	}
	require.Equal(t, 3, totalUpserts, "R9-2 两轮合并：该日 UPSERT 总批数须恰 3（已提交批不重做）")
	require.Equal(t, 1, totalUpserts-r1Upserts, "R9-2 第二轮：该日应仅补做 1 批（[200,250)），不回卷重做 [0,200)")

	// 断言 5：两轮合并该日 UPSERT 键集恰等于全部 250 候选（无遗漏、无重复重做）。
	var upsertedUsers, upsertedGroups []int64
	for _, u := range repo.upsertOnlyCalls {
		if !u.day.Equal(D) {
			continue
		}
		for _, rec := range u.records {
			upsertedUsers = append(upsertedUsers, rec.UserID)
			upsertedGroups = append(upsertedGroups, rec.GroupID)
		}
	}
	require.True(t, sameKeySet(upsertedUsers, upsertedGroups, candUserIDs(repo.candByDay[D]), candGroupIDs(repo.candByDay[D])),
		"R9-2 两轮合并：该日 UPSERT 键集必须恰好等于全部 250 候选（无遗漏、无重复重做）")
	require.Len(t, upsertedUsers, 250, "R9-2 两轮合并：该日 UPSERT 总数须为 250（无重做）")

	// 断言 6：该日恰 1 次失效调用（第二轮日完成时触发；第一轮未完成不触发）。
	r2Invalidates := 0
	for _, c := range repo.invalidateCalls {
		if c.day.Equal(D) {
			r2Invalidates++
			require.Equal(t, 250, len(c.userIDs), "R9-2 该日失效键集须=全集（部分键集即雷点）")
			require.True(t, sameKeySet(c.userIDs, c.groupIDs, candUserIDs(repo.candByDay[D]), candGroupIDs(repo.candByDay[D])))
		}
	}
	require.Equal(t, 1, r2Invalidates, "R9-2 该日应恰 1 次失效（日完成时触发，第一轮失败未完成不得触发）")
}

// markNotFullReevalLocal 设置 lastPolicyVersion 与当前策略一致，使本轮非 fullReeval。
func markNotFullReevalLocal(svc *UsageRiskAnalysisService) {
	pol, _, _, err := svc.loadPolicy(context.Background())
	if err != nil {
		panic(err)
	}
	svc.lastPolicyVersion = UsageRiskPolicyVersion(pol, timezone.Name())
}

// ───────────────── R10-2 锁定（外审⑥ #3/#4/#5）─────────────────

// R10-2/#3：进度 FailedBatches 写库前归一为 []——游标持久化携带 'null'（json null）时，
// 反序列化得 nil slice，原样 Marshal 回 'null' 或经 pq 发 SQL NULL 均违反
// failed_batches JSONB NOT NULL 语义；归一后所有录制进度须为 []。
func TestUsageRiskR10_2_NilFailedBatchesNormalized(t *testing.T) {
	repo := &orchFakeRepo{}
	svc := newOrchService(t, repo, 10)
	now := svc.now()
	nearStart := timezone.StartOfDay(now.Add(-usageRiskNearWindow))
	day := nearStart.AddDate(0, 0, 1)

	repo.latestRunResult = &UsageRiskRunStatus{Status: "completed", HistoryCovered: true}
	// 游标携带 json null（模拟历史脏值/边界形态），反序列化为 nil。
	repo.latestCursorResult = &UsageRiskReconCursor{CursorDate: ptrTime(nearStart), HistoryCovered: true, FailedBatches: json.RawMessage("null")}
	repo.candByDay = map[time.Time][]UsageRiskCandidate{day: newDailyCandidates(5)}
	markNotFullReevalLocal(svc)

	require.NoError(t, svc.runAnalysis(context.Background(), now))

	checks := 0
	for _, p := range repo.updateProgressHistory {
		require.Equal(t, "[]", string(p.FailedBatches),
			"R10-2/#3：所有进度写的 FailedBatches 须归一为 []（nil→SQL NULL 违反 JSONB NOT NULL；'null' 串口径分叉），实际=%q", string(p.FailedBatches))
		checks++
	}
	require.Equal(t, "[]", string(repo.completeProgress.FailedBatches), "R10-2/#3：终态写的 FailedBatches 须归一为 []")
	require.Greater(t, checks, 0, "R10-2/#3：须存在至少一次进度写（否则本测试未覆盖写路径）")
}

// R10-2/#4：版本缓存仅在 CreateRun 成功后推进——CreateRun 失败路径不推进，
// 下轮版本比较仍不等 → fullReeval 照常触发（新策略保留窗口重评不丢失）。
func TestUsageRiskR10_2_PolicyVersionAdvancedAfterCreateRun(t *testing.T) {
	// sub1：CreateRun 失败 → lastPolicyVersion 不推进。
	repo := &orchFakeRepo{}
	svc := newOrchService(t, repo, 10)
	now := svc.now()
	svc.lastPolicyVersion = "stale-version" // 与当前 policy 版本必不相等 → 本轮判 fullReeval。
	repo.createRunErr = errors.New("simulated create-run failure")

	err := svc.runAnalysis(context.Background(), now)
	require.Error(t, err, "R10-2/#4 sub1：CreateRun 失败须显式报错（失败关闭）")
	require.Equal(t, "stale-version", svc.lastPolicyVersion,
		"R10-2/#4 sub1：CreateRun 失败后版本缓存不得推进（否则下轮版本相等 → fullReeval 不触发 → 新策略重评永久丢失）")

	// sub2：CreateRun 成功 → 推进到当前版本（与落账 run 的 PolicyVersion 同值——缓存即本轮持久化版本）。
	repo2 := &orchFakeRepo{}
	svc2 := newOrchService(t, repo2, 10)
	svc2.lastPolicyVersion = "stale-version"
	require.NoError(t, svc2.runAnalysis(context.Background(), svc2.now()))
	require.NotEqual(t, "stale-version", svc2.lastPolicyVersion,
		"R10-2/#4 sub2：CreateRun 成功后版本缓存须推进（不得滞留旧值）")
	require.Equal(t, svc2.lastPolicyVersion, fmt.Sprintf("%016x", uint64(repo2.createRunArg.PolicyVersion)),
		"R10-2/#4 sub2：推进后的缓存（hex）须与落账 run 的 PolicyVersion（十六进制往返）同值")
}

// R10-2/#5：近窗失败日禁跨轮偏移——retry 强制 startOffset=0（每轮全量重算自愈），
// 失败保留 Offset 记 0；older 冻结日断点续跑语义（R9-2）不受影响。
func TestUsageRiskR10_2_NearWindowFailedDayOffsetZero(t *testing.T) {
	// sub1：近窗日 fb{Offset:200} → retry 从 0 全量重算（250 候选 = 3 批；
	// 若沿用 200 只会提交 1 批 [200,250)——本断言即 mutation 反证锚点）。
	repo := &orchFakeRepo{}
	svc := newOrchService(t, repo, 10)
	now := svc.now()
	nearStart := timezone.StartOfDay(now.Add(-usageRiskNearWindow))
	dn := nearStart.AddDate(0, 0, 1)

	repo.latestRunResult = &UsageRiskRunStatus{Status: "completed", HistoryCovered: true}
	repo.latestCursorResult = &UsageRiskReconCursor{CursorDate: ptrTime(nearStart), HistoryCovered: true,
		FailedBatches: mustMarshalReconFailed(t, []reconFailedBatch{{Day: dn, Offset: 200}})}
	repo.candByDay = map[time.Time][]UsageRiskCandidate{dn: newDailyCandidates(250)}
	markNotFullReevalLocal(svc)
	require.NoError(t, svc.runAnalysis(context.Background(), now))

	ups := 0
	for _, u := range repo.upsertOnlyCalls {
		if u.day.Equal(dn) {
			ups++
		}
	}
	require.Equal(t, 3, ups,
		"R10-2/#5 sub1：近窗失败日 retry 须从 offset=0 全量重算（250 候选=3 批）；若跨轮沿用 200 只提交 1 批")

	// sub2：近窗日 retry 再失败 → 保留 fb 的 Offset 记 0（非 nextOffset）。
	repo2 := &orchFakeRepo{}
	svc2 := newOrchService(t, repo2, 10)
	repo2.latestRunResult = &UsageRiskRunStatus{Status: "completed", HistoryCovered: true}
	repo2.latestCursorResult = &UsageRiskReconCursor{CursorDate: ptrTime(nearStart), HistoryCovered: true,
		FailedBatches: mustMarshalReconFailed(t, []reconFailedBatch{{Day: dn, Offset: 0}})}
	repo2.candByDay = map[time.Time][]UsageRiskCandidate{dn: newDailyCandidates(250)}
	repo2.failUpsertNth = map[time.Time]int{dn: 3}
	markNotFullReevalLocal(svc2)
	require.NoError(t, svc2.runAnalysis(context.Background(), now))

	var fb []reconFailedBatch
	require.NoError(t, json.Unmarshal(repo2.completeProgress.FailedBatches, &fb))
	require.Len(t, fb, 1)
	require.True(t, fb[0].Day.Equal(dn))
	require.Equal(t, 0, fb[0].Offset,
		"R10-2/#5 sub2：近窗失败日保留批次 Offset 须记 0（活数据每轮全量重算），不得记 nextOffset=200")

	// sub3：older 冻结日断点续跑语义保持（fb{200} → 只补 [200,250) 1 批，不回卷）。
	repo3 := &orchFakeRepo{}
	svc3 := newOrchService(t, repo3, 10)
	do := nearStart.AddDate(0, 0, -1)
	repo3.latestRunResult = &UsageRiskRunStatus{Status: "completed", HistoryCovered: true}
	repo3.latestCursorResult = &UsageRiskReconCursor{CursorDate: ptrTime(nearStart), HistoryCovered: true,
		FailedBatches: mustMarshalReconFailed(t, []reconFailedBatch{{Day: do, Offset: 200}})}
	repo3.candByDay = map[time.Time][]UsageRiskCandidate{do: newDailyCandidates(250)}
	markNotFullReevalLocal(svc3)
	require.NoError(t, svc3.runAnalysis(context.Background(), now))

	upsOlder := 0
	for _, u := range repo3.upsertOnlyCalls {
		if u.day.Equal(do) {
			upsOlder++
		}
	}
	require.Equal(t, 1, upsOlder,
		"R10-2/#5 sub3：older 冻结日断点续跑语义保持——从 200 续跑只补 [200,250) 1 批（R9-2 回归）")
}

// ───────────────────────── R13-1 锁定测试：fullReeval 轮 R1 延迟到完整新桶后权威重评 ─────────────────────────
//
// 外审⑨ 发现1（P1）：fullReeval 轮（版本/时区/重新启用变化）历史桶按新语义逐日重建中，近窗 R1
// 若继承旧覆盖状态（hc=prevHistoryCovered=true）会在半重建桶上算出错误分数——本轮 R1 一律不参与
// （hc 排除 fullReeval），由收尾置 r1_reeval_pending，下一轮基于完整新桶权威重评（复用既有 pending
// 回卷重放链条）。自查链条：fullReeval completed → pending=true + 回卷 retentionStart → 下轮
// hc=true（thisRoundReevalR1）+ older 全窗重放（桶完整）→ pending 清 → 稳态。fullReeval partial
// → pending=true + 回卷 → 下轮全窗重放续跑。prevPending=true 再触发 fullReeval → pending 保持。
//
// 注入：
//   - aggHourlyByDay：近窗日（9-19/9-20）注入 user1 的 20 个活跃桶（≥ R1ActiveHours=20）；
//   - bucketByUser[1]：注入 reportDay 前 1/2/3 天各 ≥20 桶（≥ R1ConsecutiveDays=3）；
//   - candByDay：近窗日 1 候选（user1,group1）。
//   - olde 窗口日无候选 → 空日失效，不干扰 R1 断言。
func TestUsageRiskR13_1_FullReevalDefersR1ToAuthoritativeReeval(t *testing.T) {
	// r1Points 取该记录 rule_hits 中 Points>0 的 R1 命中点数（0 = 该记录无 R1 真命中）。
	r1Points := func(t *testing.T, rec UsageRiskReportRecord) int {
		t.Helper()
		var hits []RuleHit
		require.NoError(t, json.Unmarshal(rec.RuleHits, &hits), "rule_hits 必须可解析")
		for _, h := range hits {
			if h.Rule == "R1" {
				return h.Points
			}
		}
		return 0
	}
	// nearDayRecords 收集近窗窗口内全部 upserted 记录（按日）。
	nearDayRecords := func(t *testing.T, repo *orchFakeRepo, nearStart time.Time, reportNowDay time.Time) map[time.Time][]UsageRiskReportRecord {
		t.Helper()
		out := map[time.Time][]UsageRiskReportRecord{}
		for _, u := range repo.upsertOnlyCalls {
			if !u.day.Before(nearStart) && u.day.Before(reportNowDay) {
				out[u.day] = append(out[u.day], u.records...)
			}
		}
		return out
	}
	// r1RecordedCount 统计近窗内 R1 真命中的记录数。
	r1RecordedCount := func(t *testing.T, repo *orchFakeRepo, nearStart, reportNowDay time.Time) int {
		t.Helper()
		n := 0
		for _, recs := range nearDayRecords(t, repo, nearStart, reportNowDay) {
			for _, rec := range recs {
				if r1Points(t, rec) > 0 {
					n++
				}
			}
		}
		return n
	}
	// r1Injection 注入 R1 达标数据（近窗日活跃桶 + 连续活跃桶）。
	r1Injection := func(repo *orchFakeRepo, nearStart, reportNowDay time.Time) {
		repo.candByDay = map[time.Time][]UsageRiskCandidate{
			nearStart:                  {{UserID: 1, GroupID: 1, RequestCount: 15}},
			nearStart.AddDate(0, 0, 1): {{UserID: 1, GroupID: 1, RequestCount: 15}},
		}
		agg := map[time.Time][]UsageRiskHourAggregate{}
		for day := nearStart; day.Before(reportNowDay); day = day.AddDate(0, 0, 1) {
			var hrs []UsageRiskHourAggregate
			for i := 0; i < 20; i++ {
				hrs = append(hrs, UsageRiskHourAggregate{UserID: 1, GroupID: 1, BucketHour: day.Add(time.Duration(i) * time.Hour)})
			}
			agg[day] = hrs
		}
		repo.aggHourlyByDay = agg
		// 连续活跃：近窗 day 的前 1/2/3 天各 20 桶（R1ConsecutiveDays=3）。
		var buckets []time.Time
		for d := 1; d <= 3; d++ {
			for i := 0; i < 20; i++ {
				buckets = append(buckets, nearStart.AddDate(0, 0, -d).Add(time.Duration(i)*time.Hour))
			}
		}
		repo.bucketByUser = map[int64][]time.Time{1: buckets}
	}

	newSvcWithR1 := func(t *testing.T, repo *orchFakeRepo) (*UsageRiskAnalysisService, time.Time, time.Time, time.Time) {
		svc := newOrchService(t, repo, 10)
		now := svc.now()
		nearStart := timezone.StartOfDay(now.Add(-usageRiskNearWindow))
		retentionStart := timezone.StartOfDay(now.AddDate(0, 0, -10))
		reportNowDay := timezone.StartOfDay(now).AddDate(0, 0, 1)
		_ = retentionStart // 调用点按需重算（now 固定），此处仅构造 nearStart/reportNowDay
		return svc, now, nearStart, reportNowDay
	}

	// ── 段 1+2：冷启动（fullReeval）翻转 pending → pending 重评轮全窗重放 R1 命中 + pending 清（原语义）。 ──
	t.Run("cold start defers R1 then authoritative re-reeval hits", func(t *testing.T) {
		repo := &orchFakeRepo{}
		svc, now, nearStart, reportNowDay := newSvcWithR1(t, repo)
		r1Injection(repo, nearStart, reportNowDay)

		// 段 1：冷启动（latestRun=nil → fullReeval）。hc 排除 fullReeval → 本轮 R1 不参与；
		// 覆盖翻转（older 空日走完 + 近窗完成）→ pending=true + 回卷 retentionStart。
		require.NoError(t, svc.runAnalysis(context.Background(), now))
		require.Equal(t, "completed", repo.completeStatus)
		require.True(t, repo.completeProgress.HistoryCovered, "冷启动轮须翻转为覆盖")
		require.True(t, repo.completeProgress.R1ReevalPending, "冷启动翻转轮须置 pending=true（R1 延迟到权威重评）")
		require.NotNil(t, repo.completeProgress.ReconCursorDate)
		require.True(t, repo.completeProgress.ReconCursorDate.Equal(timezone.StartOfDay(now.AddDate(0, 0, -10))),
			"冷启动翻转轮须回卷游标到 retentionStart")
		// R13-1 核心：fullReeval 轮近窗记录不得含 R1 真命中（hc 已排除 fullReeval）。
		require.Equal(t, 0, r1RecordedCount(t, repo, nearStart, reportNowDay),
			"fullReeval 轮 R1 一律不参与（宁可缺一轮，不在半重建桶上算错）")

		// 段 2：pending 重评轮（非 fullReeval → thisRoundReevalR1=true，hc=true）older 全窗重放，
		// 完整新桶 + 近窗 R1 命中，重放走完 → pending 清。
		repo.latestRunResult = &UsageRiskRunStatus{Status: "completed", HistoryCovered: true, R1ReevalPending: true}
		repo.latestCursorResult = &UsageRiskReconCursor{CursorDate: ptrTime(timezone.StartOfDay(now.AddDate(0, 0, -10))), HistoryCovered: true}
		require.NoError(t, svc.runAnalysis(context.Background(), now))
		require.Equal(t, "completed", repo.completeStatus)
		require.False(t, repo.completeProgress.R1ReevalPending, "权威重评轮完成 → pending 清 false")
		require.Greater(t, r1RecordedCount(t, repo, nearStart, reportNowDay), 0,
			"pending 权威重评轮：完整新桶上 R1 必须命中")
	})

	// ── 段 3+4：策略变化触发 fullReeval → R1 不参与 + pending + 回卷 → 下一轮 R1 命中恢复 + pending 清。 ──
	t.Run("policy change fullReeval defers R1 then next round recovers", func(t *testing.T) {
		repo := &orchFakeRepo{}
		svc, now, nearStart, reportNowDay := newSvcWithR1(t, repo)
		r1Injection(repo, nearStart, reportNowDay)

		// 模拟既有稳态（已覆盖 + 已清 pending）。
		repo.latestRunResult = &UsageRiskRunStatus{Status: "completed", HistoryCovered: true}
		repo.latestCursorResult = &UsageRiskReconCursor{CursorDate: ptrTime(nearStart), HistoryCovered: true}

		// 策略变化（listing_min_score 99 ≠ 默认 40）→ lastPolicyVersion 与当前不一致 → fullReeval。
		stub := svc.settingSvc.settingRepo.(*usageRiskSettingRepoStub)
		stub.values[SettingKeyUsageRiskListingMinScore] = "99"

		require.NoError(t, svc.runAnalysis(context.Background(), now))
		require.Equal(t, "completed", repo.completeStatus)
		require.True(t, repo.completeProgress.HistoryCovered)
		// 段 3 核心：fullReeval 轮 R1 不参与 + pending=true + 游标回卷 retentionStart。
		require.Equal(t, 0, r1RecordedCount(t, repo, nearStart, reportNowDay),
			"策略变化 fullReeval 轮：近窗 R1 不得在桶重建完成前参与")
		require.True(t, repo.completeProgress.R1ReevalPending, "fullReeval 轮须置 pending=true（下轮权威重评）")
		require.NotNil(t, repo.completeProgress.ReconCursorDate)
		require.True(t, repo.completeProgress.ReconCursorDate.Equal(timezone.StartOfDay(now.AddDate(0, 0, -10))),
			"fullReeval 轮须回卷游标到 retentionStart")

		// 段 4：下一轮（prev 已覆盖 + pending）非 fullReeval → thisRoundReevalR1=true，hc=true，
		// older 全窗重放（桶完整）→ R1 命中恢复 + pending 清。
		repo.latestRunResult = &UsageRiskRunStatus{Status: "completed", HistoryCovered: true, R1ReevalPending: true}
		repo.latestCursorResult = &UsageRiskReconCursor{CursorDate: ptrTime(timezone.StartOfDay(now.AddDate(0, 0, -10))), HistoryCovered: true}
		require.NoError(t, svc.runAnalysis(context.Background(), now))
		require.Equal(t, "completed", repo.completeStatus)
		require.False(t, repo.completeProgress.R1ReevalPending, "权威重评轮完成 → pending 清 false")
		require.Greater(t, r1RecordedCount(t, repo, nearStart, reportNowDay), 0,
			"段 4：完整新桶上 R1 命中恢复")
	})

	// ── 自查链条补充：prevPending=true 再触发 fullReeval → pending 保持（不丢失）。 ──
	t.Run("prev pending then fullReeval keeps pending", func(t *testing.T) {
		repo := &orchFakeRepo{}
		svc, now, nearStart, reportNowDay := newSvcWithR1(t, repo)
		r1Injection(repo, nearStart, reportNowDay)

		// prev 已覆盖且 pending=true（上一轮 R1 待重评）。
		repo.latestRunResult = &UsageRiskRunStatus{Status: "completed", HistoryCovered: true, R1ReevalPending: true}
		repo.latestCursorResult = &UsageRiskReconCursor{CursorDate: ptrTime(nearStart), HistoryCovered: true}

		// 策略变化 → fullReeval（lastPolicyVersion 默认 "" ≠ 当前）；R1 本轮不参与，pending 保持 true。
		stub := svc.settingSvc.settingRepo.(*usageRiskSettingRepoStub)
		stub.values[SettingKeyUsageRiskListingMinScore] = "99"

		require.NoError(t, svc.runAnalysis(context.Background(), now))
		require.Equal(t, "completed", repo.completeStatus)
		require.True(t, repo.completeProgress.R1ReevalPending, "prevPending=true + fullReeval → pending 必须保持")
		require.Equal(t, 0, r1RecordedCount(t, repo, nearStart, reportNowDay),
			"prevPending + fullReeval 轮：R1 仍不参与（桶重建中）")
	})
}

// ───────────────────────── R13-2 锁定测试：failed_batches 解析失败失败关闭（外审⑨ 发现2） ─────────────────────────
//
// 游标 FailedBatches 为合法 JSONB 但结构不符（跨版本部署/回滚/人工恢复可产生）时：
//   - 不得静默当空列表推进本轮（否则 CompleteRun 以 [] 覆盖原状态，未重试批次永久丢失）；
//   - 必须失败关闭：不建 run、不更新任何状态，保留原游标待修复，下轮重试。
// 合法形态（`[{...}]` 数组）必须解析成功不阻断 run 正常创建（回归对照）。
func TestUsageRiskR13_2_MalformedFailedBatchesFailClosed(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	nearStart := timezone.StartOfDay(now.Add(-usageRiskNearWindow))
	day := nearStart.AddDate(0, 0, 1)

	t.Run("malformed JSON object fails closed", func(t *testing.T) {
		repo := &orchFakeRepo{}
		svc := newOrchService(t, repo, 10)
		repo.latestRunResult = &UsageRiskRunStatus{Status: "completed", HistoryCovered: true}
		repo.latestCursorResult = &UsageRiskReconCursor{
			CursorDate:    ptrTime(nearStart),
			HistoryCovered: true,
			FailedBatches:  json.RawMessage(`{"a":1}`), // 合法 JSON 非数组 → json.Unmarshal 失败
		}
		repo.candByDay = map[time.Time][]UsageRiskCandidate{day: newDailyCandidates(5)}

		err := svc.runAnalysis(context.Background(), now)
		require.Error(t, err, "失败批次游标解析失败必须失败关闭（返回错误）")
		require.Contains(t, err.Error(), "解析失败批次游标失败",
			"错误须明确标注失败关闭原因（保留原游标，不降级为空列表）")
		require.Equal(t, 0, repo.createRunCalls, "失败关闭：解析失败不得建 run")
		require.Equal(t, 0, repo.completeCalls, "失败关闭：解析失败不得收尾 run")
		require.Equal(t, 0, repo.updateProgressCalls, "失败关闭：解析失败不得更新任何状态")
	})

	t.Run("well-formed batch array proceeds normally", func(t *testing.T) {
		repo := &orchFakeRepo{}
		svc := newOrchService(t, repo, 10)
		repo.latestRunResult = &UsageRiskRunStatus{Status: "completed", HistoryCovered: true}
		repo.latestCursorResult = &UsageRiskReconCursor{
			CursorDate:     ptrTime(nearStart),
			HistoryCovered: true,
			FailedBatches:  json.RawMessage(`[{"day":"2026-09-20T00:00:00Z","offset":0}]`),
		}
		repo.candByDay = map[time.Time][]UsageRiskCandidate{day: newDailyCandidates(5)}

		require.NoError(t, svc.runAnalysis(context.Background(), now), "合法失败批次数组必须解析成功不阻断")
		require.Equal(t, 1, repo.createRunCalls, "合法形态：run 正常创建")
		require.Equal(t, 1, repo.completeCalls, "合法形态：收尾正常执行")
	})
}
