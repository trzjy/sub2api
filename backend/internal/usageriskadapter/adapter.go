// Package usageriskadapter 将 *repository.UsageRiskRepository 适配到 service 包内
// 消费者定义的 service.UsageRiskRepository 接口，并做 DTO 翻译。
//
// 之所以需要本包：repository 包反向 import service 包（account_credentials_crypt.go
// 引用 service.SensitiveCredentialKeys），service 包不得 import repository，否则形成
// import cycle。本包是唯一同时 import 两个包的边界，仅供 cmd/server 接线使用。
package usageriskadapter

import (
	"context"
	"database/sql"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/google/wire"
)

// adapter 包裹 *repository.UsageRiskRepository，实现 service.UsageRiskRepository。
type adapter struct {
	repo repository.UsageRiskRepository
}

// New 构造适配后的 service.UsageRiskRepository。
func New(repo repository.UsageRiskRepository) service.UsageRiskRepository {
	return &adapter{repo: repo}
}

// NewRetentionCleaner 返回具体 *adapter，满足 service 包内 usageRiskRetentionCleaner 接口
// （dashboard 保留期协调器在唯一 DB 事务内同事务清理风险表）。Go 按结构满足接口，
// 调用方（cmd/server）可直接将 *adapter 传入需 usageRiskRetentionCleaner 的参数。
func NewRetentionCleaner(repo repository.UsageRiskRepository) *adapter {
	return &adapter{repo: repo}
}

var _ service.UsageRiskRepository = (*adapter)(nil)

// ───────────────────────── 阶段一：聚合 ─────────────────────────

func (a *adapter) AggregateHourly(ctx context.Context, windowStart, windowEnd time.Time, p service.AggregateParams) ([]service.UsageRiskHourAggregate, error) {
	src, err := a.repo.AggregateHourly(ctx, windowStart, windowEnd, toRepoAggregateParams(p))
	if err != nil {
		return nil, err
	}
	out := make([]service.UsageRiskHourAggregate, 0, len(src))
	for _, h := range src {
		out = append(out, service.UsageRiskHourAggregate{
			UserID:                h.UserID,
			GroupID:               h.GroupID,
			BucketHour:            h.BucketHour,
			RequestCount:          h.RequestCount,
			InputTokensSum:        h.InputTokensSum,
			OutputTokensSum:       h.OutputTokensSum,
			CacheReadTokensSum:    h.CacheReadTokensSum,
			CostUSDSum:            h.CostUSDSum,
			OccupiedMsSum:         h.OccupiedMsSum,
			NonWhitelistedUACount: h.NonWhitelistedUACount,
		})
	}
	return out, nil
}

func (a *adapter) AggregateGroupRPMMinutes(ctx context.Context, windowStart, windowEnd time.Time, p service.AggregateParams, limits []service.UsageRiskRPMLimit, ratio float64) (map[service.UsageRiskUserGroupKey]int, error) {
	src, err := a.repo.AggregateGroupRPMMinutes(ctx, windowStart, windowEnd, toRepoAggregateParams(p), toRepoRPMLimitSlice(limits), ratio)
	if err != nil {
		return nil, err
	}
	out := make(map[service.UsageRiskUserGroupKey]int, len(src))
	for k, v := range src {
		out[service.UsageRiskUserGroupKey{UserID: k.UserID, GroupID: k.GroupID}] = v
	}
	return out, nil
}

func (a *adapter) AggregateUserGlobalRPMMinutes(ctx context.Context, windowStart, windowEnd time.Time, limits []service.UsageRiskUserLimit, ratio float64) (map[int64]int, error) {
	return a.repo.AggregateUserGlobalRPMMinutes(ctx, windowStart, windowEnd, toRepoUserLimitSlice(limits), ratio)
}

func (a *adapter) ListCandidates(ctx context.Context, windowStart, windowEnd time.Time, p service.AggregateParams, minDailyRequests int, offset, batchSize int) ([]service.UsageRiskCandidate, error) {
	src, err := a.repo.ListCandidates(ctx, windowStart, windowEnd, toRepoAggregateParams(p), minDailyRequests, offset, batchSize)
	if err != nil {
		return nil, err
	}
	out := make([]service.UsageRiskCandidate, 0, len(src))
	for _, c := range src {
		out = append(out, service.UsageRiskCandidate{UserID: c.UserID, GroupID: c.GroupID, RequestCount: c.RequestCount})
	}
	return out, nil
}

func (a *adapter) RebuildHourlyWindow(ctx context.Context, windowStart, windowEnd time.Time, p service.AggregateParams) (int64, error) {
	return a.repo.RebuildHourlyWindow(ctx, windowStart, windowEnd, toRepoAggregateParams(p))
}

// ───────────────────────── 阶段二：明细 ─────────────────────────

func (a *adapter) DistinctIPs(ctx context.Context, userID int64, dayStart, dayEnd time.Time, p service.AggregateParams) ([]string, error) {
	return a.repo.DistinctIPs(ctx, userID, dayStart, dayEnd, toRepoAggregateParams(p))
}

func (a *adapter) IPAssociatedUsers(ctx context.Context, userID int64, dayStart, dayEnd time.Time, p service.AggregateParams) (map[string]service.IPClusterFact, error) {
	return a.repo.IPAssociatedUsers(ctx, userID, dayStart, dayEnd, toRepoAggregateParams(p))
}

func (a *adapter) UADistribution(ctx context.Context, userID int64, dayStart, dayEnd time.Time, p service.AggregateParams) (map[string]int, error) {
	return a.repo.UADistribution(ctx, userID, dayStart, dayEnd, toRepoAggregateParams(p))
}

func (a *adapter) KeyUsageDistribution(ctx context.Context, userID int64, dayStart, dayEnd time.Time, p service.AggregateParams) (map[string]int, error) {
	return a.repo.KeyUsageDistribution(ctx, userID, dayStart, dayEnd, toRepoAggregateParams(p))
}

func (a *adapter) ActiveHourHeatmap(ctx context.Context, userID int64, dayStart, dayEnd time.Time, tzName string, p service.AggregateParams) (map[int]int, error) {
	return a.repo.ActiveHourHeatmap(ctx, userID, dayStart, dayEnd, tzName, toRepoAggregateParams(p))
}

func (a *adapter) R2PeerDayCosts(ctx context.Context, groupID int64, dayStart, dayEnd time.Time, p service.AggregateParams, minDailyRequests int) ([]service.UsageRiskPeerCost, error) {
	src, err := a.repo.R2PeerDayCosts(ctx, groupID, dayStart, dayEnd, toRepoAggregateParams(p), minDailyRequests)
	if err != nil {
		return nil, err
	}
	out := make([]service.UsageRiskPeerCost, 0, len(src))
	for _, c := range src {
		out = append(out, service.UsageRiskPeerCost{UserID: c.UserID, CostUSD: c.CostUSD})
	}
	return out, nil
}

// UserActiveBucketHours 委托 U3 只读回看窗口内某用户 distinct 活跃桶（rollup）。
func (a *adapter) UserActiveBucketHours(ctx context.Context, userID int64, lookbackStart, reportDay time.Time) ([]time.Time, error) {
	return a.repo.UserActiveBucketHours(ctx, userID, lookbackStart, reportDay)
}

// ───────────────────────── 报告写入（列级隔离） ─────────────────────────

func (a *adapter) UpdateReportStatus(ctx context.Context, reportID int64, oldStatus, newStatus string, adminID int64, at time.Time) (bool, error) {
	return a.repo.UpdateReportStatus(ctx, reportID, oldStatus, newStatus, adminID, at)
}

// UpdateStatusWithAudit 委托 U3 在单事务内完成状态变更与审计落账（U4b-R1，方案 §5:91）。
func (a *adapter) UpdateStatusWithAudit(ctx context.Context, reportID int64, oldStatus, newStatus string, adminID int64, at time.Time, entry *service.AuditLog) (bool, error) {
	return a.repo.UpdateStatusWithAudit(ctx, reportID, oldStatus, newStatus, adminID, at, entry)
}

func (a *adapter) ReconcileReportBatchUPSERTOnly(ctx context.Context, reportDate time.Time, records []service.UsageRiskReportRecord) error {
	return a.repo.ReconcileReportBatchUPSERTOnly(ctx, reportDate, toRepoReportRecordSlice(records))
}

func (a *adapter) InvalidateOutsideKeySet(ctx context.Context, reportDate time.Time, userIDs, groupIDs []int64) error {
	return a.repo.InvalidateOutsideKeySet(ctx, reportDate, userIDs, groupIDs)
}

// InvalidateAllEffective 委托 U3 全量失效当前有效报告（时区换日迁移单一 SQL，不推测范围）。
func (a *adapter) InvalidateAllEffective(ctx context.Context) error {
	return a.repo.InvalidateAllEffective(ctx)
}

// ───────────────────────── 台账 usage_risk_runs ─────────────────────────

func (a *adapter) CreateRun(ctx context.Context, r service.UsageRiskRunRecord) (int64, error) {
	return a.repo.CreateRun(ctx, toRepoRunRecord(r))
}

func (a *adapter) UpdateRunProgress(ctx context.Context, runID int64, p service.UsageRiskRunProgress) error {
	return a.repo.UpdateRunProgress(ctx, runID, toRepoRunProgress(p))
}

func (a *adapter) CompleteRun(ctx context.Context, runID int64, status string, p service.UsageRiskRunProgress) error {
	return a.repo.CompleteRun(ctx, runID, status, toRepoRunProgress(p))
}

func (a *adapter) ConvergeStaleRuns(ctx context.Context, staleBefore time.Time, failureStage string) error {
	return a.repo.ConvergeStaleRuns(ctx, staleBefore, failureStage)
}

func (a *adapter) LatestRun(ctx context.Context) (*service.UsageRiskRunStatus, error) {
	return a.repo.LatestRun(ctx)
}

// LoadLatestReconCursor 委托 U3 只读补读上一轮持久化游标三元组（U4b-R3 断点续跑）。
func (a *adapter) LoadLatestReconCursor(ctx context.Context) (*service.UsageRiskReconCursor, error) {
	return a.repo.LoadLatestReconCursor(ctx)
}

// ───────────────────────── 查询端 ─────────────────────────

func (a *adapter) ListReports(ctx context.Context, f service.UsageRiskListFilter, minScore int) ([]service.UsageRiskReportItem, int, error) {
	return a.repo.ListReports(ctx, f, minScore)
}

func (a *adapter) GetReportDetail(ctx context.Context, reportID int64) (*service.UsageRiskReportDetail, error) {
	return a.repo.GetReportDetail(ctx, reportID)
}

func (a *adapter) RiskSummary(ctx context.Context, minScore, topN int) (*service.UsageRiskSummary, error) {
	return a.repo.RiskSummary(ctx, minScore, topN)
}

// CleanupReportAndRollupTxDB 委托 U3 的 *sql.Tx 暴露版本，使 *adapter 满足
// dashboard 的 usageRiskRetentionCleaner 接口（保留期同事务清理）。
func (a *adapter) CleanupReportAndRollupTxDB(ctx context.Context, tx *sql.Tx, reportCutoff, rollupCutoff time.Time) (int64, int64, error) {
	return a.repo.CleanupReportAndRollupTxDB(ctx, tx, reportCutoff, rollupCutoff)
}

// ───────────────────────── DTO 翻译 ─────────────────────────

func toRepoAggregateParams(p service.AggregateParams) repository.AggregateParams {
	return repository.AggregateParams{
		UnlimitedGroupsOnly: p.UnlimitedGroupsOnly,
		UAWhitelist:        p.UAWhitelist,
	}
}

func toRepoRPMLimitSlice(in []service.UsageRiskRPMLimit) []repository.UsageRiskRPMLimit {
	out := make([]repository.UsageRiskRPMLimit, 0, len(in))
	for _, v := range in {
		out = append(out, repository.UsageRiskRPMLimit{UserID: v.UserID, GroupID: v.GroupID, GroupLimit: v.GroupLimit})
	}
	return out
}

func toRepoUserLimitSlice(in []service.UsageRiskUserLimit) []repository.UsageRiskUserLimit {
	out := make([]repository.UsageRiskUserLimit, 0, len(in))
	for _, v := range in {
		out = append(out, repository.UsageRiskUserLimit{UserID: v.UserID, UserLimit: v.UserLimit})
	}
	return out
}

func toRepoReportRecordSlice(in []service.UsageRiskReportRecord) []repository.UsageRiskReportRecord {
	out := make([]repository.UsageRiskReportRecord, 0, len(in))
	for _, v := range in {
		out = append(out, repository.UsageRiskReportRecord{
			UserID:        v.UserID,
			GroupID:       v.GroupID,
			ReportDate:    v.ReportDate,
			PolicyVersion: v.PolicyVersion,
			Score:         v.Score,
			Level:         v.Level,
			RuleHits:      v.RuleHits,
			Evidence:      v.Evidence,
			InvalidatedAt: v.InvalidatedAt,
		})
	}
	return out
}

func toRepoRunRecord(r service.UsageRiskRunRecord) repository.UsageRiskRunRecord {
	return repository.UsageRiskRunRecord{
		RunAt:           r.RunAt,
		WindowStart:     r.WindowStart,
		WindowEnd:       r.WindowEnd,
		PolicyVersion:   r.PolicyVersion,
		PolicySnapshot:  r.PolicySnapshot,
		CandidatesTotal: r.CandidatesTotal,
	}
}

func toRepoRunProgress(p service.UsageRiskRunProgress) repository.UsageRiskRunProgress {
	return repository.UsageRiskRunProgress{
		BatchesDone:         p.BatchesDone,
		BatchesFailed:       p.BatchesFailed,
		FailedBatches:       p.FailedBatches,
		BudgetExhausted:     p.BudgetExhausted,
		ReconCursorDate:     p.ReconCursorDate,
		ReconBatchOffset:    p.ReconBatchOffset,
		ReconBatchesDone:    p.ReconBatchesDone,
		HistoryCovered:      p.HistoryCovered,
		ConsecutivePartials: p.ConsecutivePartials,
		R1ReevalPending:     p.R1ReevalPending,
		CandidatesTotal:     p.CandidatesTotal,
		FinishedAt:          p.FinishedAt,
		FailureStage:        p.FailureStage,
	}
}

// ProvideUsageRiskAnalysisService 构造并启动异常调用分析服务（DI 入口，返回 U4c 契约接口）。
func ProvideUsageRiskAnalysisService(
	repo repository.UsageRiskRepository,
	client *ent.Client,
	userGroupRateRepo service.UserGroupRateRepository,
	timingWheel *service.TimingWheelService,
	lockCache service.LeaderLockCache,
	db *sql.DB,
	settingSvc *service.SettingService,
	auditLog *service.AuditLogService,
	cfg *config.Config,
) service.UsageRiskService {
	svc := service.NewUsageRiskAnalysisService(
		New(repo),
		client,
		userGroupRateRepo,
		timingWheel,
		lockCache,
		db,
		settingSvc,
		auditLog,
		cfg,
	)
	svc.Start()
	return svc
}

// ProviderSet 是 usageriskadapter 的 wire 提供集，供 cmd/server 接线引入。
var ProviderSet = wire.NewSet(New, ProvideUsageRiskAnalysisService)
