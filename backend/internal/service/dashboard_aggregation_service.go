package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/google/uuid"
)

const (
	defaultDashboardAggregationTimeout         = 2 * time.Minute
	defaultDashboardAggregationBackfillTimeout = 30 * time.Minute
	dashboardAggregationRetentionInterval      = 6 * time.Hour

	// dashboardAggregationLeaderLockKey 保证多副本部署中每个周期只有一个实例执行聚合。
	dashboardAggregationLeaderLockKey = "dashboard:aggregation:leader"
	// TTL 必须覆盖 dashboard 聚合与分组日汇总两个有界阶段，避免任务中途失锁。
	dashboardAggregationLeaderLockTTL = 5 * time.Minute

	// 启动回填耗时可能远长于周期聚合，因此使用独立锁并让 TTL 严格大于回填超时。
	dashboardAggregationGroupUsageBackfillLeaderLockKey = "dashboard:aggregation:group-usage-backfill:leader"
	dashboardAggregationGroupUsageBackfillLeaderLockTTL = defaultDashboardAggregationBackfillTimeout + time.Minute
)

var (
	// ErrDashboardBackfillDisabled 当配置禁用回填时返回。
	ErrDashboardBackfillDisabled = errors.New("仪表盘聚合回填已禁用")
	// ErrDashboardBackfillTooLarge 当回填跨度超过限制时返回。
	ErrDashboardBackfillTooLarge   = errors.New("回填时间跨度过大")
	errDashboardAggregationRunning = errors.New("聚合作业正在运行")
)

// DashboardAggregationRepository 定义仪表盘预聚合仓储接口。
type DashboardAggregationRepository interface {
	AggregateRange(ctx context.Context, start, end time.Time) error
	// RecomputeRange 重新计算指定时间范围内的聚合数据（包含活跃用户等派生表）。
	// 设计目的：当 usage_logs 被批量删除/回滚后，确保聚合表可恢复一致性。
	RecomputeRange(ctx context.Context, start, end time.Time) error
	GetAggregationWatermark(ctx context.Context) (time.Time, error)
	UpdateAggregationWatermark(ctx context.Context, aggregatedAt time.Time) error
	CleanupAggregates(ctx context.Context, hourlyCutoff, dailyCutoff time.Time) error
	CleanupUsageLogs(ctx context.Context, cutoff time.Time) error
	// CleanupUsageLogsTx 是 CleanupUsageLogs 的事务参数变体：删除段在调用方传入的 *sql.Tx
	// 上执行，使源日志清理能与风险表清理（U3）纳入同一 DB 事务（方案 §6.4/§5）。
	CleanupUsageLogsTx(ctx context.Context, tx *sql.Tx, cutoff time.Time) error
	CleanupUsageBillingDedup(ctx context.Context, cutoff time.Time) error
	EnsureUsageLogsPartitions(ctx context.Context, now time.Time) error
}

// DashboardAggregationService 负责定时聚合与回填。
type DashboardAggregationService struct {
	repo                 DashboardAggregationRepository
	timingWheel          *TimingWheelService
	cfg                  config.DashboardAggregationConfig
	running              int32
	lastRetentionCleanup atomic.Value // time.Time

	lockCache  LeaderLockCache
	db         *sql.DB
	instanceID string

	// usageRiskRetentionGate 返回风险保留期天数与读取错误（来自 settings）。为 nil 时
	// 视为未接线（保留现状：跳过风险表清理、源日志独立清理）。非 nil 时：
	//   - err != nil → 本轮两清全部失败关闭（源日志也不删），下轮幂等重试；
	//   - err == nil → 风险表清理与源日志在同一事务内清理，无论分析全局开关状态
	//     （风险数据 TTL 独立于功能开关，保留期绑定校验由 settings/启动重验把关）。
	// 由 U4 接线点注入（从 SettingService.LoadUsageRiskPolicy 读取）；本单只定义契约，
	// 不在 service 内直接依赖 SettingService，避免循环依赖。
	usageRiskRetentionGate func(ctx context.Context) (riskRetentionDays int, err error)
	// usageRiskRepo 是风险表清理的同一事务入口（U3 CleanupReportAndRollupTx 的 *sql.Tx 适配）。
	// 为 nil 时源日志清理走独立路径（无风险表清理）。
	usageRiskRepo usageRiskRetentionCleaner
	// retentionCoordinator 在唯一 DB 事务内协调「风险表清理 + 源日志清理」。
	// 生产默认实现 lazy 构造（见 dbRetentionCoordinator）；测试可注入 fake 记录调用序列。
	retentionCoordinator retentionCoordinator
}

// usageRiskRetentionCleaner 是风险表保留清理的同一事务入口。
// 由 repository.usageRiskRepository 经新增适配方法 CleanupReportAndRollupTxDB 实现
// （service 包无法引用未导出的 repository.sqlExecutor，故以导出的 *sql.Tx 暴露）。
type usageRiskRetentionCleaner interface {
	CleanupReportAndRollupTxDB(ctx context.Context, tx *sql.Tx, reportCutoff, rollupCutoff time.Time) (int64, int64, error)
}

// usageLogsRetentionTxExecutor 是源日志保留清理的事务入口（即 DashboardAggregationRepository）。
type usageLogsRetentionTxExecutor interface {
	CleanupUsageLogsTx(ctx context.Context, tx *sql.Tx, cutoff time.Time) error
}

// retentionCoordinator 在唯一 DB 事务内协调风险表清理与源日志清理。
// 任一失败整体回滚（调用方不推进 lastRetentionCleanup，下轮按年龄截止幂等重试）。
// 测试可注入 fake 以记录调用序列并模拟回滚。
type retentionCoordinator interface {
	RunRetentionCleanup(ctx context.Context, riskEnabled bool, usageCutoff, riskCutoff time.Time) error
}

// dbRetentionCoordinator 是 retentionCoordinator 的生产实现：在 db 上的同一事务内
// 先清风险表/rollup（U3），再清源日志（CleanupUsageLogsTx），任一失败整体回滚。
type dbRetentionCoordinator struct {
	db           *sql.DB
	usageRiskRepo usageRiskRetentionCleaner
	usageLogsRepo usageLogsRetentionTxExecutor
}

func (c *dbRetentionCoordinator) RunRetentionCleanup(ctx context.Context, riskEnabled bool, usageCutoff, riskCutoff time.Time) error {
	// riskEnabled 恒为 true（service 仅在功能开启时调用协调器）。
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin retention tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // 已提交则为 no-op；失败/panic 兜底回滚。

	if _, _, err := c.usageRiskRepo.CleanupReportAndRollupTxDB(ctx, tx, riskCutoff, riskCutoff); err != nil {
		return fmt.Errorf("cleanup usage risk reports/rollup: %w", err)
	}
	if err := c.usageLogsRepo.CleanupUsageLogsTx(ctx, tx, usageCutoff); err != nil {
		return fmt.Errorf("cleanup usage logs: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit retention tx: %w", err)
	}
	return nil
}

// SetUsageRiskRetentionGate 注入风险保留期契约（风险保留天数 + 读取错误）。
// 生产由 U4 job 注册处注入（从 SettingService.LoadUsageRiskPolicy 读取）；不注入则未接线
// （跳过风险表清理、源日志独立清理）。gate 返回错误时调用方整轮失败关闭。
func (s *DashboardAggregationService) SetUsageRiskRetentionGate(gate func(ctx context.Context) (riskRetentionDays int, err error)) {
	s.usageRiskRetentionGate = gate
}

// SetUsageRiskRetentionCleaner 注入风险表清理的同一事务入口（U3 适配）。
func (s *DashboardAggregationService) SetUsageRiskRetentionCleaner(c usageRiskRetentionCleaner) {
	s.usageRiskRepo = c
}

// SetRetentionCoordinator 注入保留期协调器（主要用于测试 fake 记录调用序列）。
func (s *DashboardAggregationService) SetRetentionCoordinator(c retentionCoordinator) {
	s.retentionCoordinator = c
}

// NewDashboardAggregationService 创建聚合服务。
func NewDashboardAggregationService(repo DashboardAggregationRepository, timingWheel *TimingWheelService, cfg *config.Config) *DashboardAggregationService {
	var aggCfg config.DashboardAggregationConfig
	if cfg != nil {
		aggCfg = cfg.DashboardAgg
	}
	return &DashboardAggregationService{
		repo:        repo,
		timingWheel: timingWheel,
		cfg:         aggCfg,
		instanceID:  uuid.NewString(),
	}
}

// SetLeaderLock injects the leader-lock cache and DB used to elect a single
// instance for the periodic scheduled aggregation. When both are nil the job runs
// ungated (single-instance / test behavior).
func (s *DashboardAggregationService) SetLeaderLock(lockCache LeaderLockCache, db *sql.DB) {
	if s == nil {
		return
	}
	s.lockCache = lockCache
	s.db = db
}

// Start 启动定时聚合作业（重启生效配置）。
func (s *DashboardAggregationService) Start() {
	if s == nil || s.repo == nil || s.timingWheel == nil {
		return
	}
	if !s.cfg.Enabled {
		logger.LegacyPrintf("service.dashboard_aggregation", "[DashboardAggregation] 聚合作业已禁用")
		return
	}
	go s.runStartupGroupUsageSync()

	interval := time.Duration(s.cfg.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = time.Minute
	}

	if s.cfg.RecomputeDays > 0 {
		go s.recomputeRecentDays()
	}

	s.timingWheel.ScheduleRecurring("dashboard:aggregation", interval, func() {
		s.runScheduledAggregation()
	})
	logger.LegacyPrintf("service.dashboard_aggregation", "[DashboardAggregation] 聚合作业启动 (interval=%v, lookback=%ds)", interval, s.cfg.LookbackSeconds)
	if !s.cfg.BackfillEnabled {
		logger.LegacyPrintf("service.dashboard_aggregation", "[DashboardAggregation] 回填已禁用，如需补齐保留窗口以外历史数据请手动回填")
	}
}

// TriggerBackfill 触发回填（异步）。
func (s *DashboardAggregationService) TriggerBackfill(start, end time.Time) error {
	if s == nil || s.repo == nil {
		return errors.New("聚合服务未初始化")
	}
	if !s.cfg.BackfillEnabled {
		logger.LegacyPrintf("service.dashboard_aggregation", "[DashboardAggregation] 回填被拒绝: backfill_enabled=false")
		return ErrDashboardBackfillDisabled
	}
	if !end.After(start) {
		return errors.New("回填时间范围无效")
	}
	if s.cfg.BackfillMaxDays > 0 {
		maxRange := time.Duration(s.cfg.BackfillMaxDays) * 24 * time.Hour
		if end.Sub(start) > maxRange {
			return ErrDashboardBackfillTooLarge
		}
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), defaultDashboardAggregationBackfillTimeout)
		defer cancel()
		if err := s.backfillRange(ctx, start, end); err != nil {
			logger.LegacyPrintf("service.dashboard_aggregation", "[DashboardAggregation] 回填失败: %v", err)
		}
	}()
	return nil
}

// TriggerRecomputeRange 触发指定范围的重新计算（异步）。
// 与 TriggerBackfill 不同：
// - 不依赖 backfill_enabled（这是内部一致性修复）
// - 不更新 watermark（避免影响正常增量聚合游标）
func (s *DashboardAggregationService) TriggerRecomputeRange(start, end time.Time) error {
	if s == nil || s.repo == nil {
		return errors.New("聚合服务未初始化")
	}
	if !s.cfg.Enabled {
		return errors.New("聚合服务已禁用")
	}
	if !end.After(start) {
		return errors.New("重新计算时间范围无效")
	}

	go func() {
		const maxRetries = 3
		for i := 0; i < maxRetries; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), defaultDashboardAggregationBackfillTimeout)
			err := s.recomputeRange(ctx, start, end)
			cancel()
			if err == nil {
				return
			}
			if !errors.Is(err, errDashboardAggregationRunning) {
				logger.LegacyPrintf("service.dashboard_aggregation", "[DashboardAggregation] 重新计算失败: %v", err)
				return
			}
			time.Sleep(5 * time.Second)
		}
		logger.LegacyPrintf("service.dashboard_aggregation", "[DashboardAggregation] 重新计算放弃: 聚合作业持续占用")
	}()
	return nil
}

func (s *DashboardAggregationService) recomputeRecentDays() {
	days := s.cfg.RecomputeDays
	if days <= 0 {
		return
	}
	now := time.Now().UTC()
	start := now.AddDate(0, 0, -days)

	ctx, cancel := context.WithTimeout(context.Background(), defaultDashboardAggregationBackfillTimeout)
	defer cancel()
	if err := s.backfillRange(ctx, start, now); err != nil {
		logger.LegacyPrintf("service.dashboard_aggregation", "[DashboardAggregation] 启动重算失败: %v", err)
		return
	}
}

func (s *DashboardAggregationService) recomputeRange(ctx context.Context, start, end time.Time) error {
	if !atomic.CompareAndSwapInt32(&s.running, 0, 1) {
		return errDashboardAggregationRunning
	}
	defer atomic.StoreInt32(&s.running, 0)

	jobStart := time.Now().UTC()
	if err := s.repo.RecomputeRange(ctx, start, end); err != nil {
		return err
	}
	logger.LegacyPrintf("service.dashboard_aggregation", "[DashboardAggregation] 重新计算完成 (start=%s end=%s duration=%s)",
		start.UTC().Format(time.RFC3339),
		end.UTC().Format(time.RFC3339),
		time.Since(jobStart).String(),
	)
	return nil
}

func (s *DashboardAggregationService) runScheduledAggregation() {
	if !atomic.CompareAndSwapInt32(&s.running, 0, 1) {
		return
	}
	defer atomic.StoreInt32(&s.running, 0)

	jobStart := time.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), defaultDashboardAggregationTimeout)
	defer cancel()

	// Multi-instance guard: only the leader runs the periodic aggregation; peers
	// skip this cycle to avoid N× redundant GROUP BY queries and watermark races.
	release, ok := tryAcquireSingletonLeaderLock(ctx, s.lockCache, s.db, dashboardAggregationLeaderLockKey, s.instanceID, dashboardAggregationLeaderLockTTL)
	if !ok {
		return
	}
	defer release()
	defer s.runScheduledGroupUsageSync()

	now := time.Now().UTC()
	last, err := s.repo.GetAggregationWatermark(ctx)
	if err != nil {
		logger.LegacyPrintf("service.dashboard_aggregation", "[DashboardAggregation] 读取水位失败: %v", err)
		last = time.Unix(0, 0).UTC()
	}

	lookback := time.Duration(s.cfg.LookbackSeconds) * time.Second
	epoch := time.Unix(0, 0).UTC()
	start := last.Add(-lookback)
	if !last.After(epoch) {
		retentionDays := s.cfg.Retention.UsageLogsDays
		if retentionDays <= 0 {
			retentionDays = 1
		}
		start = truncateToDayUTC(now.AddDate(0, 0, -retentionDays))
	} else if start.After(now) {
		start = now.Add(-lookback)
	}

	if err := s.aggregateRange(ctx, start, now); err != nil {
		logger.LegacyPrintf("service.dashboard_aggregation", "[DashboardAggregation] 聚合失败: %v", err)
		return
	}

	updateErr := s.repo.UpdateAggregationWatermark(ctx, now)
	if updateErr != nil {
		logger.LegacyPrintf("service.dashboard_aggregation", "[DashboardAggregation] 更新水位失败: %v", updateErr)
	}
	slog.Debug("[DashboardAggregation] 聚合完成",
		"start", start.Format(time.RFC3339),
		"end", now.Format(time.RFC3339),
		"duration", time.Since(jobStart).String(),
		"watermark_updated", updateErr == nil,
	)

	s.maybeCleanupRetention(ctx, now)
}

func (s *DashboardAggregationService) runScheduledGroupUsageSync() {
	ctx, cancel := context.WithTimeout(context.Background(), defaultDashboardAggregationTimeout)
	defer cancel()
	if err := s.syncGroupUsageRollups(ctx, time.Now().UTC()); err != nil {
		logger.LegacyPrintf("service.dashboard_aggregation", "[DashboardAggregation] 分组用量日汇总失败: %v", err)
	}
}

func (s *DashboardAggregationService) runStartupGroupUsageSync() {
	ctx, cancel := context.WithTimeout(context.Background(), defaultDashboardAggregationBackfillTimeout)
	defer cancel()
	release, ok := tryAcquireSingletonLeaderLock(ctx, s.lockCache, s.db, dashboardAggregationGroupUsageBackfillLeaderLockKey, s.instanceID, dashboardAggregationGroupUsageBackfillLeaderLockTTL)
	if !ok {
		return
	}
	defer release()
	if err := s.syncGroupUsageRollups(ctx, time.Now().UTC()); err != nil {
		logger.LegacyPrintf("service.dashboard_aggregation", "[DashboardAggregation] 启动分组用量回填失败: %v", err)
	}
}

func (s *DashboardAggregationService) syncGroupUsageRollups(ctx context.Context, now time.Time) error {
	repo, ok := s.repo.(GroupUsageRollupRepository)
	if !ok {
		return nil
	}
	return repo.SyncGroupUsageRollups(ctx, GroupUsageTodayStart(now))
}

func (s *DashboardAggregationService) backfillRange(ctx context.Context, start, end time.Time) error {
	if !atomic.CompareAndSwapInt32(&s.running, 0, 1) {
		return errDashboardAggregationRunning
	}
	defer atomic.StoreInt32(&s.running, 0)

	jobStart := time.Now().UTC()
	startUTC := start.UTC()
	endUTC := end.UTC()
	if !endUTC.After(startUTC) {
		return errors.New("回填时间范围无效")
	}

	cursor := truncateToDayUTC(startUTC)
	for cursor.Before(endUTC) {
		windowEnd := cursor.Add(24 * time.Hour)
		if windowEnd.After(endUTC) {
			windowEnd = endUTC
		}
		if err := s.aggregateRange(ctx, cursor, windowEnd); err != nil {
			return err
		}
		cursor = windowEnd
	}

	updateErr := s.repo.UpdateAggregationWatermark(ctx, endUTC)
	if updateErr != nil {
		logger.LegacyPrintf("service.dashboard_aggregation", "[DashboardAggregation] 更新水位失败: %v", updateErr)
	}
	logger.LegacyPrintf("service.dashboard_aggregation", "[DashboardAggregation] 回填聚合完成 (start=%s end=%s duration=%s watermark_updated=%t)",
		startUTC.Format(time.RFC3339),
		endUTC.Format(time.RFC3339),
		time.Since(jobStart).String(),
		updateErr == nil,
	)

	s.maybeCleanupRetention(ctx, endUTC)
	return nil
}

func (s *DashboardAggregationService) aggregateRange(ctx context.Context, start, end time.Time) error {
	if !end.After(start) {
		return nil
	}
	if err := s.repo.EnsureUsageLogsPartitions(ctx, end); err != nil {
		logger.LegacyPrintf("service.dashboard_aggregation", "[DashboardAggregation] 分区检查失败: %v", err)
	}
	return s.repo.AggregateRange(ctx, start, end)
}

func (s *DashboardAggregationService) maybeCleanupRetention(ctx context.Context, now time.Time) {
	lastAny := s.lastRetentionCleanup.Load()
	if lastAny != nil {
		if last, ok := lastAny.(time.Time); ok && now.Sub(last) < dashboardAggregationRetentionInterval {
			return
		}
	}

	hourlyCutoff := now.AddDate(0, 0, -s.cfg.Retention.HourlyDays)
	dailyCutoff := now.AddDate(0, 0, -s.cfg.Retention.DailyDays)
	usageCutoff := now.AddDate(0, 0, -s.cfg.Retention.UsageLogsDays)
	dedupCutoff := now.AddDate(0, 0, -s.cfg.Retention.UsageBillingDedupDays)

	// 聚合与去重清理保持独立（既有语义不变），不参与风险表同事务协调。
	aggErr := s.repo.CleanupAggregates(ctx, hourlyCutoff, dailyCutoff)
	if aggErr != nil {
		logger.LegacyPrintf("service.dashboard_aggregation", "[DashboardAggregation] 聚合保留清理失败: %v", aggErr)
	}
	dedupErr := s.repo.CleanupUsageBillingDedup(ctx, dedupCutoff)
	if dedupErr != nil {
		logger.LegacyPrintf("service.dashboard_aggregation", "[DashboardAggregation] usage_billing_dedup 保留清理失败: %v", dedupErr)
	}

	// 风险表清理与 usage_risk 全局开关解耦：只要策略可读（gate 无错），
	// 风险报告/rollup 即按风险截止点参与同事务清理，无论分析开关状态
	// （数据 TTL 独立于功能开关；保留期绑定校验由 settings/启动重验把关）。
	// gate 未注入（nil）→ 保留现状：跳过风险清理、源日志独立清理。
	if s.usageRiskRetentionGate == nil {
		usageErr := s.repo.CleanupUsageLogs(ctx, usageCutoff)
		if usageErr != nil {
			logger.LegacyPrintf("service.dashboard_aggregation", "[DashboardAggregation] usage_logs 保留清理失败: %v", usageErr)
		}
		if aggErr == nil && usageErr == nil && dedupErr == nil {
			s.lastRetentionCleanup.Store(now)
		}
		return
	}

	riskRetentionDays, gateErr := s.usageRiskRetentionGate(ctx)
	if gateErr != nil {
		// 失败关闭：本轮两清全部跳过（含源日志），结构化日志，下轮幂等重试。
		// 不得降级为「只清源」或「只清风险」，以免绕过两清同事务唯一权威路径。
		logger.LegacyPrintf("service.dashboard_aggregation",
			"[DashboardAggregation] 保留期 gate 读取策略失败，本轮两清全部失败关闭（源日志与风险表均不清理，下轮幂等重试）: %v", gateErr)
		return
	}

	// 风险报告/rollup 与源日志在同一 DB 事务内清理：两个截止点各自按配置计算。
	// 任一失败整体回滚、本轮不推进（下轮按年龄截止幂等重试）。
	riskCutoff := now.AddDate(0, 0, -riskRetentionDays)
	if s.retentionCoordinator == nil {
		if s.db == nil || s.usageRiskRepo == nil {
			// 事务化能力未注入（U4 接线点未收口）：无法走两清同事务权威路径，
			// 失败关闭（本轮两清均跳过，含源日志），不得降级为「只清源」。
			logger.LegacyPrintf("service.dashboard_aggregation",
				"[DashboardAggregation] 保留期事务化能力未注入，本轮两清失败关闭（源日志与风险表均不清理）")
			return
		}
		s.retentionCoordinator = &dbRetentionCoordinator{db: s.db, usageRiskRepo: s.usageRiskRepo, usageLogsRepo: s.repo}
	}
	if err := s.retentionCoordinator.RunRetentionCleanup(ctx, true, usageCutoff, riskCutoff); err != nil {
		logger.LegacyPrintf("service.dashboard_aggregation",
			"[DashboardAggregation] 保留期协调清理失败（风险表与源日志均回滚，本轮不推进）: %v", err)
		return
	}
	if aggErr == nil && dedupErr == nil {
		s.lastRetentionCleanup.Store(now)
	}
}

func truncateToDayUTC(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
