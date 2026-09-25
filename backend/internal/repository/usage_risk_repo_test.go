package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// 编译期断言：*usageRiskRepository 满足 UsageRiskRepository 接口。
var _ UsageRiskRepository = (*usageRiskRepository)(nil)

func newUsageRiskRepoMock(t *testing.T) (*usageRiskRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return newUsageRiskRepositoryWithSQL(nil, db, db), mock
}

// ── SQL 文本断言：派生列 UPSERT SET 白名单 ──

func TestUsageRiskReportDerivedUpsertSQLSETColumnWhitelist(t *testing.T) {
	// 列级隔离：DO UPDATE SET 只含派生列，绝不含 status/status_updated_by/status_updated_at
	setClause := extractSQLSetClause(t, reportDerivedUpsertSQL)

	for _, col := range []string{"score", "level", "rule_hits", "evidence", "policy_version", "invalidated_at"} {
		require.Contains(t, setClause, col, "派生列 %s 必须出现在 SET 子句", col)
	}
	for _, forbidden := range []string{"status", "status_updated_by", "status_updated_at"} {
		require.NotContains(t, setClause, forbidden, "人工状态/审计列 %s 绝不允许出现在派生列 SET 子句", forbidden)
	}
}

func TestUsageRiskReportDerivedUpsertSQLConflictKey(t *testing.T) {
	require.Contains(t, reportDerivedUpsertSQL, "ON CONFLICT (user_id, group_id, report_date)")
}

// ── SQL 文本断言：状态更新原状态条件 ──

func TestUsageRiskReportStatusUpdateSQLOriginalStatusCondition(t *testing.T) {
	require.Contains(t, reportStatusUpdateSQL, "WHERE report_id = $1 AND status = $2 AND invalidated_at IS NULL",
		"状态更新必须带原状态条件（并发冲突返回 0 行）+ 失效过滤（R10-1/#6：失效报告不参与状态机）")
	for _, col := range []string{"status = $", "status_updated_by = $", "status_updated_at = $"} {
		require.Contains(t, reportStatusUpdateSQL, col)
	}
	// 状态接口只动状态审计列，SET 子句不得触及其它列（含派生列 invalidated_at）。
	// 提取 UPDATE 的 SET 子句（SET 与 WHERE 之间）做白名单检查。
	setIdx := strings.Index(reportStatusUpdateSQL, "SET")
	whereIdx := strings.Index(reportStatusUpdateSQL, "WHERE")
	require.Greater(t, setIdx, -1, "SQL 必须含 SET")
	require.Greater(t, whereIdx, setIdx, "SQL 必须含 WHERE 且在 SET 之后")
	setClause := reportStatusUpdateSQL[setIdx:whereIdx]
	for _, forbidden := range []string{"score", "level", "rule_hits", "evidence", "invalidated_at"} {
		require.NotContains(t, setClause, forbidden)
	}
}

// ── SQL 文本断言：对账失效 SQL ──

func TestUsageRiskReportInvalidateOutsideSQLValidityOnly(t *testing.T) {
	require.Contains(t, reportInvalidateOutsideSQL, "SET invalidated_at = NOW()")
	require.Contains(t, reportInvalidateOutsideSQL, "r.invalidated_at IS NULL",
		"只失效仍有效的行（避免重复写时间戳）")
	require.Contains(t, reportInvalidateOutsideSQL, "NOT EXISTS")
	// 对账失效只动有效性维度，不得碰 status/审计列
	for _, forbidden := range []string{"status", "status_updated_by", "status_updated_at", "score", "level"} {
		require.NotContains(t, reportInvalidateOutsideSQL, forbidden)
	}
}

// ── 操作级：派生列 UPSERT ──

func TestUsageRiskUpsertReportDerived(t *testing.T) {
	repo, mock := newUsageRiskRepoMock(t)
	inv := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	rec := UsageRiskReportRecord{
		UserID: 1, GroupID: 2,
		ReportDate:    time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC),
		PolicyVersion: 7, Score: 45, Level: "medium",
		RuleHits:      json.RawMessage(`[{"rule":"R4","scope":"user","detail":"d","points":15}]`),
		Evidence:      json.RawMessage(`{"ips":[]}`),
		InvalidatedAt: &inv,
	}

	mock.ExpectExec(regexp.QuoteMeta(reportDerivedUpsertSQL)).
		WithArgs(rec.UserID, rec.GroupID, rec.ReportDate, rec.PolicyVersion, rec.Score, rec.Level,
			rec.RuleHits, rec.Evidence, rec.InvalidatedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := repo.UpsertReportDerived(context.Background(), rec)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// ── 操作级：状态更新原状态条件 ──

func TestUsageRiskUpdateReportStatusConflictReturnsFalse(t *testing.T) {
	repo, mock := newUsageRiskRepoMock(t)
	at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	// 并发冲突：影响 0 行（原状态已被他人修改）
	mock.ExpectExec(regexp.QuoteMeta(reportStatusUpdateSQL)).
		WithArgs(int64(9), "open", "dismissed", int64(7), at).
		WillReturnResult(sqlmock.NewResult(0, 0))

	ok, err := repo.UpdateReportStatus(context.Background(), 9, "open", "dismissed", 7, at)
	require.NoError(t, err)
	require.False(t, ok)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageRiskUpdateReportStatusSuccess(t *testing.T) {
	repo, mock := newUsageRiskRepoMock(t)
	at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	mock.ExpectExec(`^UPDATE usage_risk_reports`).
		WithArgs(int64(9), "acknowledged", "resolved", int64(7), at).
		WillReturnResult(sqlmock.NewResult(0, 1))

	ok, err := repo.UpdateReportStatus(context.Background(), 9, "acknowledged", "resolved", 7, at)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, mock.ExpectationsWereMet())
}

// ── 列表默认过滤与 include_low ──

func TestUsageRiskListReportsDefaultFilter(t *testing.T) {
	repo, mock := newUsageRiskRepoMock(t)
	f := service.UsageRiskListFilter{Page: 1, PageSize: 20}
	minScore := 40

	// COUNT 查询带默认榜单过滤
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM usage_risk_reports r`).
		WithArgs(minScore).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	cols := []string{"report_id", "user_id", "username", "group_id", "group_name",
		"report_date", "score", "level", "rule_hits", "status", "invalidated_at"}
	mock.ExpectQuery(`SELECT r\.report_id`).
		WithArgs(minScore, 20, 0).
		WillReturnRows(sqlmock.NewRows(cols).AddRow(
			int64(3), int64(1), "u1", int64(2), "g1", "2026-09-24",
			45, "medium", `[{"rule":"R4"}]`, "resolved", nil, // 非 open 状态：证明默认过滤已删除 status='open'
		))

	items, total, err := repo.ListReports(context.Background(), f, minScore)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Equal(t, 1, total)
	require.Len(t, items, 1)
	require.Equal(t, int64(3), items[0].ReportID)
	require.Equal(t, "resolved", items[0].Status, "F9：默认过滤不得再按 status='open' 过滤（已删除）")
	require.Nil(t, items[0].InvalidatedAt)
}

// include_low 时不得出现默认状态/入榜线过滤
func TestUsageRiskListReportsIncludeLowOpensListing(t *testing.T) {
	repo, mock := newUsageRiskRepoMock(t)
	f := service.UsageRiskListFilter{Page: 1, PageSize: 20, IncludeLow: true}

	// 无 minScore 参数（include_low 不加 score 过滤）
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM usage_risk_reports r`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT r\.report_id`).
		WillReturnRows(sqlmock.NewRows([]string{
			"report_id", "user_id", "username", "group_id", "group_name",
			"report_date", "score", "level", "rule_hits", "status", "invalidated_at",
		}))

	_, total, err := repo.ListReports(context.Background(), f, 40)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Equal(t, 0, total)
}

// ── risk_summary MAX(score) 去重 ──

func TestUsageRiskRiskSummaryTopMaxScorePerUser(t *testing.T) {
	repo, mock := newUsageRiskRepoMock(t)

	// 级别分布 + open 计数
	mock.ExpectQuery(`SELECT r\.level, COUNT\(\*\)`).
		WithArgs(40).
		WillReturnRows(sqlmock.NewRows([]string{"level", "count"}).
			AddRow("high", 1).AddRow("medium", 2))

	// Top N 按 user_id 取 MAX(score)（同 user 多分组只一行）
	mock.ExpectQuery(`SELECT user_id, MAX\(score\)`).
		WithArgs(40, 5).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "max_score"}).
			AddRow(int64(1), 95).AddRow(int64(2), 65))

	// 每个 top 用户取最高分行
	mock.ExpectQuery(`SELECT COALESCE\(u\.username`).
		WithArgs(int64(1)).
		WillReturnRows(sqlmock.NewRows([]string{"username", "level", "rule_hits"}).
			AddRow("u1", "high", `[{"rule":"R1","scope":"user","detail":"d","points":25},{"rule":"R2","scope":"group","detail":"d","points":20}]`))
	mock.ExpectQuery(`SELECT COALESCE\(u\.username`).
		WithArgs(int64(2)).
		WillReturnRows(sqlmock.NewRows([]string{"username", "level", "rule_hits"}).
			AddRow("u2", "medium", `[{"rule":"R4","scope":"user","detail":"d","points":15}]`))

	summary, err := repo.RiskSummary(context.Background(), 40, 5)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Equal(t, 3, summary.OpenTotal)
	require.Equal(t, 1, summary.ByLevel["high"])
	require.Equal(t, 2, summary.ByLevel["medium"])
	require.Len(t, summary.Top, 2)
	require.Equal(t, int64(1), summary.Top[0].UserID)
	require.Equal(t, 95, summary.Top[0].MaxScore)
	// scope=user 分不重复累加：u1 最高分 95 来自 MAX(score)，TopRules 是规则名去重
	require.Equal(t, []string{"R1", "R2"}, summary.Top[0].TopRules)
}

// ── 对账失效/恢复双向 SQL（文本断言 + 操作级） ──

// ── 对账：UPSERT-only 与失效拆分为两个独立单事务方法（派发单 U4b-R4） ──

func TestUsageRiskReconcileReportBatchUPSERTOnlyNoInvalidation(t *testing.T) {
	repo, mock := newUsageRiskRepoMock(t)
	day := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	rec := UsageRiskReportRecord{
		UserID: 1, GroupID: 2, ReportDate: day,
		PolicyVersion: 7, Score: 45, Level: "medium",
		RuleHits: json.RawMessage(`[]`), Evidence: json.RawMessage(`{}`),
		InvalidatedAt: nil, // 恢复有效
	}

	// 仅 UPSERT，绝不触发集合外失效（拆雷点：UPSERT-only 不碰失效）。
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO usage_risk_reports`).
		WithArgs(int64(1), int64(2), day, 7, 45, "medium", json.RawMessage(`[]`), json.RawMessage(`{}`), nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err := repo.ReconcileReportBatchUPSERTOnly(context.Background(), day, []UsageRiskReportRecord{rec})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageRiskInvalidateOutsideKeySet(t *testing.T) {
	repo, mock := newUsageRiskRepoMock(t)
	day := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)

	// 完整键集失效：仅失效该键集之外且有效的旧行。
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE usage_risk_reports r\s+SET invalidated_at = NOW\(\)`).
		WithArgs(day, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()

	err := repo.InvalidateOutsideKeySet(context.Background(), day, []int64{1, 2}, []int64{3, 4})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	// 空键集 = 失效该日全部有效旧行（等价旧 ReconcileReportBatch(nil) 语义）。
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE usage_risk_reports r\s+SET invalidated_at = NOW\(\)`).
		WithArgs(day, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 5))
	mock.ExpectCommit()

	err = repo.InvalidateOutsideKeySet(context.Background(), day, nil, nil)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// ── 台账：创建/进度/收敛 ──

func TestUsageRiskCreateRun(t *testing.T) {
	repo, mock := newUsageRiskRepoMock(t)
	rec := UsageRiskRunRecord{
		RunAt:          time.Date(2026, 9, 24, 1, 0, 0, 0, time.UTC),
		WindowStart:    time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC),
		WindowEnd:      time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC),
		PolicyVersion:  7,
		PolicySnapshot: json.RawMessage(`{"tz":"Asia/Shanghai"}`),
	}
	mock.ExpectQuery(`INSERT INTO usage_risk_runs`).
		WithArgs(rec.RunAt, rec.WindowStart, rec.WindowEnd, 7, rec.PolicySnapshot, 0).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).AddRow(int64(42)))

	runID, err := repo.CreateRun(context.Background(), rec)
	require.NoError(t, err)
	require.Equal(t, int64(42), runID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageRiskConvergeStaleRunsSQLWritesFinishedAtAndStage(t *testing.T) {
	// 文本断言：收敛必须写 finished_at 与 failure_stage
	require.Contains(t, staleRunConvergeSQL, "status = 'partial'")
	require.Contains(t, staleRunConvergeSQL, "finished_at = NOW()")
	require.Contains(t, staleRunConvergeSQL, "failure_stage = $")
	require.Contains(t, staleRunConvergeSQL, "WHERE status = 'running' AND run_at < $")
}

func TestUsageRiskConvergeStaleRuns(t *testing.T) {
	repo, mock := newUsageRiskRepoMock(t)
	staleBefore := time.Date(2026, 9, 24, 0, 50, 0, 0, time.UTC)
	mock.ExpectExec(`UPDATE usage_risk_runs\s+SET status = 'partial'`).
		WithArgs(staleBefore, "aggregate").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := repo.ConvergeStaleRuns(context.Background(), staleBefore, "aggregate")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageRiskLatestRun(t *testing.T) {
	repo, mock := newUsageRiskRepoMock(t)
	windowEnd := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	reconDate := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	windowStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`SELECT status, window_end, consecutive_partials, batches_failed, history_covered,\s+recon_cursor_date, window_start,\s+policy_snapshot->>'tz' AS tz_name, r1_reeval_pending\s+FROM usage_risk_runs\s+ORDER BY run_at DESC\s+LIMIT 1`).
		WillReturnRows(sqlmock.NewRows([]string{
			"status", "window_end", "consecutive_partials", "batches_failed",
			"history_covered", "recon_cursor_date", "window_start", "tz_name", "r1_reeval_pending",
		}).AddRow("partial", windowEnd, 2, 1, false, reconDate, windowStart, "Asia/Shanghai", false))

	status, err := repo.LatestRun(context.Background())
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
	require.NotNil(t, status)
	require.Equal(t, "partial", status.Status)
	require.Equal(t, 2, status.ConsecutivePartials)
	require.Equal(t, 1, status.FailedBatches)
	require.Equal(t, "2026-09-20/2026-06-01", status.ReconProgress)
	require.NotNil(t, status.WindowEnd)
}

// ── 清理 ──

func TestUsageRiskCleanupReportAndRollupTxSharedExecutor(t *testing.T) {
	// 可在外部事务执行：自开便捷版本应 BeginTx 后在同一事务两端删。
	repo, mock := newUsageRiskRepoMock(t)
	reportCutoff := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	rollupCutoff := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM usage_risk_reports WHERE report_date < \$1::date`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectExec(`DELETE FROM user_usage_metrics_rollup WHERE bucket_hour < \$1`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 5))
	mock.ExpectCommit()

	nReport, nRollup, err := repo.CleanupReportAndRollup(context.Background(), reportCutoff, rollupCutoff)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Equal(t, int64(3), nReport)
	require.Equal(t, int64(5), nRollup)
}

// ── 资格谓词 ──

func TestUsageRiskEligibilityClause(t *testing.T) {
	// 非 unlimited：仅订阅事实 + group 非空
	base := eligibilityClause(AggregateParams{UnlimitedGroupsOnly: false})
	require.Contains(t, base, "ul.subscription_id IS NOT NULL")
	require.Contains(t, base, "ul.group_id IS NOT NULL")
	require.NotContains(t, base, "groups")

	// unlimited：追加不限量分组（三限额全 NULL）join
	unlimited := eligibilityClause(AggregateParams{UnlimitedGroupsOnly: true})
	require.Contains(t, unlimited, "ul.subscription_id IS NOT NULL")
	require.Contains(t, unlimited, "daily_limit_usd IS NULL AND weekly_limit_usd IS NULL AND monthly_limit_usd IS NULL")
	require.Contains(t, unlimited, "deleted_at IS NULL")
}

// ── 候选集分页 ──

func TestUsageRiskListCandidatesBatchLimit(t *testing.T) {
	repo, mock := newUsageRiskRepoMock(t)
	start := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)

	mock.ExpectQuery(`SELECT ul\.user_id, ul\.group_id, COUNT\(\*\)::int`).
		WithArgs(start, end, 10, 100, 100). // min_daily_requests, offset, batchSize
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "group_id", "request_count"}).
			AddRow(int64(1), int64(2), 12))

	items, err := repo.ListCandidates(context.Background(), start, end,
		AggregateParams{UnlimitedGroupsOnly: true}, 10, 100, 100)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Len(t, items, 1)
	require.Equal(t, int64(1), items[0].UserID)
	require.Equal(t, 12, items[0].RequestCount)
}

// ── 用户全局 RPM（含按量行，无资格过滤） ──

func TestUsageRiskAggregateUserGlobalRPMMinutesNoEligibilityFilter(t *testing.T) {
	repo, mock := newUsageRiskRepoMock(t)
	start := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	limits := []UsageRiskUserLimit{{UserID: 1, UserLimit: 60}}

	// 断言 SQL 文本不含 subscription_id / group 资格谓词（用户全局含按量行）
	mock.ExpectQuery(`WITH limits AS \(`).
		WithArgs(start, end, sqlmock.AnyArg(), sqlmock.AnyArg(), 0.9).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "hit"}).AddRow(int64(1), 5))

	out, err := repo.AggregateUserGlobalRPMMinutes(context.Background(), start, end, limits, 0.9)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Equal(t, 5, out[1])
}

// ── UpdateStatusWithAudit：状态变更与审计同事务 ──

func TestUsageRiskUpdateStatusWithAuditSuccessSameTx(t *testing.T) {
	repo, mock := newUsageRiskRepoMock(t)
	at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	entry := &service.AuditLog{
		Action:      "admin.usage_risk.status",
		Method:      "POST",
		Path:        "/api/v1/admin/usage-risk/reports/9/status",
		StatusCode:  200,
		Extra:       map[string]any{"report_id": int64(9), "old_status": "open", "new_status": "acknowledged"},
		CreatedAt:   at,
	}

	// 序列断言：同一事务，顺序 UPDATE → INSERT（audit）。
	mock.ExpectBegin()
	mock.ExpectExec(`^UPDATE usage_risk_reports`).
		WithArgs(int64(9), "open", "acknowledged", int64(7), at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`^INSERT INTO audit_logs`).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	ok, err := repo.UpdateStatusWithAudit(context.Background(), 9, "open", "acknowledged", 7, at, entry)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageRiskUpdateStatusWithAuditAuditInsertFailsRollsBack(t *testing.T) {
	repo, mock := newUsageRiskRepoMock(t)
	at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	entry := &service.AuditLog{
		Action:    "admin.usage_risk.status",
		Method:    "POST",
		Path:      "/api/v1/admin/usage-risk/reports/9/status",
		Extra:     map[string]any{"report_id": int64(9)},
		CreatedAt: at,
	}

	// 审计 INSERT 失败 → 整体回滚（UPDATE 也回滚），返回错误。
	mock.ExpectBegin()
	mock.ExpectExec(`^UPDATE usage_risk_reports`).
		WithArgs(int64(9), "open", "acknowledged", int64(7), at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`^INSERT INTO audit_logs`).
		WillReturnError(errors.New("audit insert failed"))
	mock.ExpectRollback()

	ok, err := repo.UpdateStatusWithAudit(context.Background(), 9, "open", "acknowledged", 7, at, entry)
	require.Error(t, err)
	require.False(t, ok)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageRiskUpdateStatusWithAuditZeroRowsNoAudit(t *testing.T) {
	repo, mock := newUsageRiskRepoMock(t)
	at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	entry := &service.AuditLog{
		Action:    "admin.usage_risk.status",
		Path:      "/api/v1/admin/usage-risk/reports/9/status",
		Extra:     map[string]any{"report_id": int64(9)},
		CreatedAt: at,
	}

	// 0 行影响（并发冲突）：不写审计，回滚并返回 false。
	mock.ExpectBegin()
	mock.ExpectExec(`^UPDATE usage_risk_reports`).
		WithArgs(int64(9), "open", "acknowledged", int64(7), at).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	ok, err := repo.UpdateStatusWithAudit(context.Background(), 9, "open", "acknowledged", 7, at, entry)
	require.NoError(t, err)
	require.False(t, ok)
	require.NoError(t, mock.ExpectationsWereMet())
}

// ── 辅助 ──

// extractSQLSetClause 提取 UPSERT 语句中 DO UPDATE SET 之后的子句文本。
func extractSQLSetClause(t *testing.T, sqlText string) string {
	t.Helper()
	idx := strings.Index(sqlText, "DO UPDATE SET")
	require.Greater(t, idx, -1, "SQL 必须含 DO UPDATE SET")
	return sqlText[idx+len("DO UPDATE SET"):]
}

// ── 明细查询资格谓词内联（F8：谓词 SSOT） ──
//
// 派发单 F8：IPAssociatedUsers / UADistribution / KeyUsageDistribution / ActiveHourHeatmap
// 四方法签名加 p AggregateParams，SQL 内联与 DistinctIPs/AggregateHourly 同一 eligibilityClause。
// 本测试断言四方法实际执行 SQL 包含完整资格谓词（subscription_id + group_id IS NOT NULL），
// 且非 unlimited 分支不含 groups join（只验证谓词文本命中）。

func TestUsageRiskDetailQueriesInlineEligibilityClause(t *testing.T) {
	start := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	// 非 unlimited：资格谓词 = subscription_id IS NOT NULL AND group_id IS NOT NULL。
	elig := eligibilityClause(AggregateParams{UnlimitedGroupsOnly: false})

	t.Run("IPAssociatedUsers", func(t *testing.T) {
		repo, mock := newUsageRiskRepoMock(t)
		mock.ExpectQuery(`(?s).*` + regexp.QuoteMeta(elig) + `.*`).
			WillReturnRows(sqlmock.NewRows([]string{"ip_address", "distinct_users", "requests"}))
		_, err := repo.IPAssociatedUsers(context.Background(), 1, start, end, AggregateParams{UnlimitedGroupsOnly: false})
		require.NoError(t, err)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("UADistribution", func(t *testing.T) {
		repo, mock := newUsageRiskRepoMock(t)
		mock.ExpectQuery(`(?s).*` + regexp.QuoteMeta(elig) + `.*`).
			WillReturnRows(sqlmock.NewRows([]string{"ua", "cnt"}))
		_, err := repo.UADistribution(context.Background(), 1, start, end, AggregateParams{UnlimitedGroupsOnly: false})
		require.NoError(t, err)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("KeyUsageDistribution", func(t *testing.T) {
		repo, mock := newUsageRiskRepoMock(t)
		mock.ExpectQuery(`(?s).*` + regexp.QuoteMeta(elig) + `.*`).
			WillReturnRows(sqlmock.NewRows([]string{"key", "cnt"}))
		_, err := repo.KeyUsageDistribution(context.Background(), 1, start, end, AggregateParams{UnlimitedGroupsOnly: false})
		require.NoError(t, err)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("ActiveHourHeatmap", func(t *testing.T) {
		repo, mock := newUsageRiskRepoMock(t)
		mock.ExpectQuery(`(?s).*` + regexp.QuoteMeta(elig) + `.*`).
			WillReturnRows(sqlmock.NewRows([]string{"hour", "cnt"}))
		_, err := repo.ActiveHourHeatmap(context.Background(), 1, start, end, "UTC", AggregateParams{UnlimitedGroupsOnly: false})
		require.NoError(t, err)
		require.NoError(t, mock.ExpectationsWereMet())
	})
}

// 占位避免未使用 import 报错
var _ = sql.ErrNoRows

// ── E 组：ListReports rule 过滤绑定参数为合法 JSON（非含单引号的非法串） ──

func TestUsageRiskERuleFilterValidJSON(t *testing.T) {
	repo, mock := newUsageRiskRepoMock(t)
	f := service.UsageRiskListFilter{Rule: "R1", Page: 1, PageSize: 20}
	minScore := 40

	// 锁定点：绑定参数中的规则 JSON 恰为 `[{"rule":"R1"}]`（合法 JSON，可被 ::jsonb 解析）。
	expectedRuleJSON := `[{"rule":"R1"}]`

	// COUNT 查询：参数序为 [规则 JSON, minScore]
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM usage_risk_reports r`).
		WithArgs(expectedRuleJSON, minScore).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	cols := []string{"report_id", "user_id", "username", "group_id", "group_name",
		"report_date", "score", "level", "rule_hits", "status", "invalidated_at"}
	mock.ExpectQuery(`SELECT r\.report_id`).
		WithArgs(expectedRuleJSON, minScore, 20, 0).
		WillReturnRows(sqlmock.NewRows(cols).AddRow(
			int64(3), int64(1), "u1", int64(2), "g1", "2026-09-24",
			45, "medium", `[{"rule":"R4"}]`, "resolved", nil,
		))

	items, total, err := repo.ListReports(context.Background(), f, minScore)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Equal(t, 1, total)
	require.Len(t, items, 1)

	// 复核：WithArgs 严格等值匹配已证明绑定参数为合法 JSON `[{"rule":"R1"}]`，
	// 而非旧实现产出的含单引号非法串 `[{"rule":'R1'}]`。
	require.Equal(t, expectedRuleJSON, `[{"rule":"R1"}]`)
}

// ── G 组：aggregateHourlySQL 桶表达式为 DST 安全式（保 offset 起点） ──

func TestUsageRiskGBucketExprDSTSafe(t *testing.T) {
	sql := aggregateHourlySQL()

	newExpr := `(date_trunc('hour', ul.created_at AT TIME ZONE $3) - ((ul.created_at AT TIME ZONE $3) - (ul.created_at AT TIME ZONE 'UTC'))) AT TIME ZONE 'UTC' AS bucket_hour`
	oldExpr := `date_trunc('hour', ul.created_at AT TIME ZONE $3) AT TIME ZONE $3 AS bucket_hour`

	// 新表达式必须出现（唯一标识子串之一）
	require.Contains(t, sql, `AT TIME ZONE 'UTC' AS bucket_hour`,
		"G：桶表达式必须回拨到 UTC 再转回，保 offset 桶起点")
	require.Contains(t, sql, `- ((ul.created_at AT TIME ZONE $3) - (ul.created_at AT TIME ZONE 'UTC')`,
		"G：必须含 offset 修正项")
	require.Contains(t, sql, newExpr, "G：应含完整新桶表达式")

	// 旧回转式必须消失
	require.NotContains(t, sql, oldExpr,
		"G：不得再使用回转式 date_trunc(...) AT TIME ZONE $3 AS bucket_hour（DST 回拨日会丢桶）")
}

// ── I 组：RiskSummary ByLevel 初始化四级、空结果全 0、有数据累加 ──

func TestUsageRiskIByLevelInitFourLevels(t *testing.T) {
	t.Run("empty_result_four_levels_zero", func(t *testing.T) {
		repo, mock := newUsageRiskRepoMock(t)

		// 级别分布 + open 计数：空结果集（无行）
		mock.ExpectQuery(`SELECT r\.level, COUNT\(\*\)`).
			WithArgs(40).
			WillReturnRows(sqlmock.NewRows([]string{"level", "count"}))
		// Top N：空结果集
		mock.ExpectQuery(`SELECT user_id, MAX\(score\)`).
			WithArgs(40, 5).
			WillReturnRows(sqlmock.NewRows([]string{"user_id", "max_score"}))

		summary, err := repo.RiskSummary(context.Background(), 40, 5)
		require.NoError(t, err)
		require.NoError(t, mock.ExpectationsWereMet())

		// 空结果 + 四级初始化：四个键全 0
		require.Equal(t, map[string]int{"low": 0, "medium": 0, "high": 0, "critical": 0}, summary.ByLevel)
		require.Equal(t, 0, summary.OpenTotal)
		require.Len(t, summary.Top, 0)
	})

	t.Run("data_accumulates_over_init", func(t *testing.T) {
		repo, mock := newUsageRiskRepoMock(t)

		// 返回 (high,3),(critical,1)：累加结果 high=3 critical=1，low/medium 仍为 0
		mock.ExpectQuery(`SELECT r\.level, COUNT\(\*\)`).
			WithArgs(40).
			WillReturnRows(sqlmock.NewRows([]string{"level", "count"}).
				AddRow("high", 3).AddRow("critical", 1))
		// Top N：空结果集（本用例只验证 ByLevel 累加，不进入 top 行循环）
		mock.ExpectQuery(`SELECT user_id, MAX\(score\)`).
			WithArgs(40, 5).
			WillReturnRows(sqlmock.NewRows([]string{"user_id", "max_score"}))

		summary, err := repo.RiskSummary(context.Background(), 40, 5)
		require.NoError(t, err)
		require.NoError(t, mock.ExpectationsWereMet())

		require.Equal(t, 3, summary.ByLevel["high"])
		require.Equal(t, 1, summary.ByLevel["critical"])
		require.Equal(t, 0, summary.ByLevel["low"])
		require.Equal(t, 0, summary.ByLevel["medium"])
		require.Equal(t, 4, summary.OpenTotal)
		// 四级键齐全
		require.Len(t, summary.ByLevel, 4)
	})
}

// ── H 组：GetReportDetail 将 DB int policy_version 转为 hex 字符串写入 DTO ──

func TestUsageRiskHGetReportDetailPolicyVersionHex(t *testing.T) {
	repo, mock := newUsageRiskRepoMock(t)
	reportID := int64(5)

	cols := []string{"report_id", "user_id", "username", "group_id", "group_name",
		"report_date", "score", "level", "rule_hits", "status", "invalidated_at",
		"evidence", "policy_version"}
	mock.ExpectQuery(`SELECT r\.report_id`).
		WithArgs(reportID).
		WillReturnRows(sqlmock.NewRows(cols).AddRow(
			reportID, int64(1), "u1", int64(2), "g1", "2026-09-24",
			45, "medium", []byte(`[]`), "open", nil, []byte(`{}`), int64(7),
		))

	detail, err := repo.GetReportDetail(context.Background(), reportID)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	// DB 列 policy_version=7（int）经 %016x 格式化应为 16 位小写零填充 hex 串
	// "0000000000000007"（与 sha256 前 16 hex 同格式；F1 位保真）。
	require.Equal(t, "0000000000000007", detail.PolicyVersion,
		"H：DTO.PolicyVersion 必须是 16 位小写零填充 hex 字符串，而非 int")
}

// ═════════════════════════ R7-1 整改锁定测试 ═════════════════════════

// TestUsageRiskR7RebuildHourlyInsertColumnCountMatchesSelect 锁定 [P0]：
// RebuildHourlyWindow 构造的 INSERT 目标列数必须等于 aggregateHourlySQL() 外层 SELECT
// 表达式数（各 10 个）。computed_at 由 DDL DEFAULT NOW() 供给，不得出现在 INSERT 列清单。
// mutation 反证：给 INSERT 加回 computed_at 列 → 列数变 11 → 本测试 FAIL。
func TestUsageRiskR7RebuildHourlyInsertColumnCountMatchesSelect(t *testing.T) {
	// 捕获 RebuildHourlyWindow 实际构造执行的 INSERT 完整文本（含 SELECT 主体）。
	var capturedInsert string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(func(expectedSQL, actualSQL string) error {
		capturedInsert = actualSQL
		return nil
	})))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := newUsageRiskRepositoryWithSQL(nil, db, db)

	start := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	// R10-1/#1：删除与聚合统一使用应用时区整点对齐后的起点（与生产同一表达式）。
	aligned := alignToAppHour(t, start)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM user_usage_metrics_rollup`).
		WithArgs(aligned, end).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO user_usage_metrics_rollup`).
		WithArgs(aligned, end, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	_, err = repo.RebuildHourlyWindow(context.Background(), start, end,
		AggregateParams{UnlimitedGroupsOnly: false, UAWhitelist: []string{"curl"}})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	// 提取 INSERT 目标列清单（括号内以逗号分隔的标识符）。
	targetCols := extractInsertTargetColumns(t, capturedInsert)
	require.Equal(t, 10, len(targetCols),
		"P0：INSERT 目标列数必须与 SELECT 表达式数一致（computed_at 由 DEFAULT NOW() 供给，不入列清单）")
	require.NotContains(t, targetCols, "computed_at",
		"P0：INSERT 列清单绝不允许含 computed_at（依赖 DDL DEFAULT NOW()，等宽靠删列而非凑数）")

	// 从 aggregateHourlySQL() 提取外层 SELECT 表达式数（10 个）。
	selExprs := extractOuterSelectExprs(t, aggregateHourlySQL())
	require.Equal(t, 10, len(selExprs),
		"P0：aggregateHourlySQL 外层 SELECT 表达式数必须为 10（本卡不得改 SELECT 表达式列表）")
	require.Equal(t, len(targetCols), len(selExprs),
		"P0：INSERT 目标列数 == 外层 SELECT 表达式数（奇偶等宽，杜绝 INSERT has more target columns than expressions）")

	// 双 SSOT：列清单与表达式清单逐一对应。
	for i, col := range targetCols {
		require.Equal(t, col, selExprs[i],
			"P0：INSERT 第 %d 列 %s 与 SELECT 第 %d 表达式 %s 必须同位对应", i, col, i, selExprs[i])
	}
}

// TestUsageRiskR7PolicyVersionHexRoundTrip 锁定 [P2]：
// GetReportDetail 的 PolicyVersion 必须是 16 位小写零填充 hex（与 sha256 前 16 hex 同格式）。
// 全链：parsePolicyVersionHex → int64 位保真 → %016x 格式化，高位负号与前导零均不得破坏。
func TestUsageRiskR7PolicyVersionHexRoundTrip(t *testing.T) {
	// 直接锁格式化函数级行为（GetReportDetail 内的同一表达式）。
	format := func(pv int64) string { return fmt.Sprintf("%016x", uint64(pv)) }

	cases := []struct {
		name string
		in   string // 入库前的 hex 串（sha256 前 16 hex）
	}{
		{name: "high_bit_all_ones", in: "ffffffffffffffff"},   // 高位全 1 → int64(-1)，旧 FormatInt 输出 "-1"
		{name: "high_bit_2_63", in: "8000000000000000"},       // ≥2^63 → int64 负数，旧 FormatInt 输出 "-9223...8"
		{name: "leading_zeros", in: "0000000000000ab1"},       // 前导零，旧 FormatInt/FormatUint 丢弃为 "ab1"
		{name: "mixed", in: "1a2b3c4d5e6f7081"},
		{name: "small", in: "0000000000000007"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 解析链与 service.parsePolicyVersionHex 等价（ParseUint 全 64 位无符号 + int64 位保真）。
			u, err := strconv.ParseUint(c.in, 16, 64)
			require.NoError(t, err)
			pv := int64(u)
			got := format(pv)
			require.Equal(t, c.in, got,
				"hex 往返失真：%q → %q（必须 16 位小写零填充，与版本哈希串同格式）", c.in, got)
			require.NotContains(t, got, "-",
				"hex 输出绝不允许出现负号（int64 负数仅表示高位被置位）")
		})
	}

	// 经 GetReportDetail 全链：DB 存 int64(-1) → DTO.PolicyVersion 仍为 ffffffffffffffff。
	repo, mock := newUsageRiskRepoMock(t)
	reportID := int64(6)
	cols := []string{"report_id", "user_id", "username", "group_id", "group_name",
		"report_date", "score", "level", "rule_hits", "status", "invalidated_at",
		"evidence", "policy_version"}
	mock.ExpectQuery(`SELECT r\.report_id`).
		WithArgs(reportID).
		WillReturnRows(sqlmock.NewRows(cols).AddRow(
			reportID, int64(1), "u1", int64(2), "g1", "2026-09-24",
			45, "medium", []byte(`[]`), "open", nil, []byte(`{}`), int64(-1),
		))
	detail, err := repo.GetReportDetail(context.Background(), reportID)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Equal(t, "ffffffffffffffff", detail.PolicyVersion,
		"全链：int64(-1) 必须格式化为 ffffffffffffffff（位保真，无负号无截断）")

	// 前导零经全链：DB 存 0xab1 → DTO.PolicyVersion 为 0000000000000ab1。
	repo2, mock2 := newUsageRiskRepoMock(t)
	mock2.ExpectQuery(`SELECT r\.report_id`).
		WithArgs(reportID).
		WillReturnRows(sqlmock.NewRows(cols).AddRow(
			reportID, int64(1), "u1", int64(2), "g1", "2026-09-24",
			45, "medium", []byte(`[]`), "open", nil, []byte(`{}`), int64(0xab1),
		))
	detail2, err := repo2.GetReportDetail(context.Background(), reportID)
	require.NoError(t, err)
	require.NoError(t, mock2.ExpectationsWereMet())
	require.Equal(t, "0000000000000ab1", detail2.PolicyVersion,
		"全链：0xab1 必须格式化为 0000000000000ab1（前导零不丢失）")
}

// TestUsageRiskR7UAWhitelistNotLikeAnySemantics 锁定 [P2]：
// aggregateHourlySQL 的 UA 白名单语义必须为 "不在任一子串命中" → NOT COALESCE(
// bool_or(strpos(...)))（字面子串，R12-4 外审⑧ #5），而非 LIKE 通配语义——
// 配置中的 %/_ 会被 LIKE 解释为通配符，与规则引擎 isWhitelistedUA 字面匹配事实分叉。
func TestUsageRiskR7UAWhitelistNotLikeAnySemantics(t *testing.T) {
	sql := aggregateHourlySQL()
	require.Contains(t, sql, "NOT COALESCE((",
		"P2：UA 白名单必须为 NOT COALESCE(bool_or(...))（全不匹配才计非白名单，空数组不产生 NULL 过滤）")
	require.Contains(t, sql, "strpos(ul.user_agent, t) > 0",
		"P2：子串匹配必须用 strpos 字面语义（禁 LIKE 通配）")
	require.NotContains(t, sql, "LIKE ANY",
		"P2：禁止残留 LIKE ANY（通配语义与规则引擎字面口径分叉，外审⑧ #5）")
	require.Contains(t, sql, "ul.user_agent IS NULL OR",
		"P2：NULL UA 必须归入非白名单计数（COUNT FILTER 分支保留）")

	// 操作级：多子串参数以 pq.Array 传入且 SQL 文本含正确谓词。
	repo, mock := newUsageRiskRepoMock(t)
	start := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	multi := []string{"curl", "python"}
	terms := uaWhitelistTerms(multi)
	mock.ExpectQuery(`WITH hourly AS \(`).
		WithArgs(start, end, sqlmock.AnyArg(), pq.Array(terms)).
		WillReturnRows(sqlmock.NewRows([]string{
			"user_id", "group_id", "bucket_hour", "request_count", "input_tokens_sum",
			"output_tokens_sum", "cache_read_tokens_sum", "cost_usd_sum",
			"occupied_ms_sum", "non_whitelisted_ua_count",
		}))

	got, err := repo.AggregateHourly(context.Background(), start, end,
		AggregateParams{UnlimitedGroupsOnly: false, UAWhitelist: multi})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Empty(t, got)
}

// TestUsageRiskR12_4_UATermsLiteralAndEmptyListNULL 锁定 [P2]（外审⑧ #5）：
// uaWhitelistTerms 字面直传原文（含 %/_ 不包装、滤空串）；空白名单 → nil →
// pq.Array(NULL) 直传（无空哨兵，unnest 零行 → COALESCE false → 全部计入，
// 与规则侧 isWhitelistedUA(ua, [])==false 口径一致）。
func TestUsageRiskR12_4_UATermsLiteralAndEmptyListNULL(t *testing.T) {
	terms := uaWhitelistTerms([]string{"curl", "50%_off", "", "python"})
	require.Equal(t, []string{"curl", "50%_off", "python"}, terms,
		"白名单子串必须原文直传（含 %/_ 字符不包装）且滤空串")
	require.Nil(t, uaWhitelistTerms(nil), "空白名单返回 nil")
	require.Nil(t, uaWhitelistTerms([]string{"", ""}), "全空串白名单返回 nil")

	// 空白名单操作级：第 4 参为 NULL 数组（pq.Array(nil)），谓词仍为 strpos 单一形态。
	repo, mock := newUsageRiskRepoMock(t)
	start := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	mock.ExpectQuery(`WITH hourly AS \(`).
		WithArgs(start, end, sqlmock.AnyArg(), nil).
		WillReturnRows(sqlmock.NewRows([]string{
			"user_id", "group_id", "bucket_hour", "request_count", "input_tokens_sum",
			"output_tokens_sum", "cache_read_tokens_sum", "cost_usd_sum",
			"occupied_ms_sum", "non_whitelisted_ua_count",
		}))

	got, err := repo.AggregateHourly(context.Background(), start, end,
		AggregateParams{UnlimitedGroupsOnly: false})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Empty(t, got)
}

// TestUsageRiskR7UpdateRunProgressCandidatesTotal 锁定 [P2]：
// UpdateRunProgress 必须持久化 candidates_total（L137 台账契约），参数序号顺延；
// R8-1 适配：recon_cursor_key_range 已全链删除（参数序号前移，run_id 至 $10）。
func TestUsageRiskR7UpdateRunProgressCandidatesTotal(t *testing.T) {
	repo, mock := newUsageRiskRepoMock(t)
	runID := int64(42)
	p := UsageRiskRunProgress{
		BatchesDone:      3,
		BatchesFailed:    0,
		FailedBatches:    json.RawMessage(`[]`),
		BudgetExhausted:  false,
		CandidatesTotal:  128,
		ReconBatchesDone: 1,
		ReconCursorDate:  nil,
		ReconBatchOffset: nil,
		HistoryCovered:   true,
	}

	// 文本断言：candidates_total 出现在 SET 子句；recon_cursor_key_range 不得出现。
	mock.ExpectExec(`UPDATE usage_risk_runs\s+SET .*candidates_total = \$5.*WHERE run_id = \$10`).
		WithArgs(p.BatchesDone, p.BatchesFailed, p.FailedBatches, p.BudgetExhausted,
			p.CandidatesTotal, p.ReconBatchesDone, p.ReconCursorDate,
			p.ReconBatchOffset, p.HistoryCovered, runID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := repo.UpdateRunProgress(context.Background(), runID, p)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// ── R8-1 整改锁定测试（外审④ #1/#2/#8/#7 repo 侧） ──

// TestUsageRiskR8GroupRPMProjectionIncludesGroupLimit 锁定 [P0 外审 #1]：
// AggregateGroupRPMMinutes 的 minutes CTE 必须投影并分组 l.group_limit（外层 FILTER 引用），
// 否则 PostgreSQL 解析期报 column "group_limit" does not exist（空结果也炸）。
func TestUsageRiskR8GroupRPMProjectionIncludesGroupLimit(t *testing.T) {
	start := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	limits := []UsageRiskRPMLimit{{UserID: 1, GroupID: 2, GroupLimit: 30}}

	var captured string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(func(expectedSQL, actualSQL string) error {
		captured = actualSQL
		return nil
	})))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := newUsageRiskRepositoryWithSQL(nil, db, db)

	mock.ExpectQuery(`WITH limits AS \(`).
		WithArgs(start, end, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), 0.8).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "group_id", "hit"}).
			AddRow(int64(1), int64(2), 3))

	_, err = repo.AggregateGroupRPMMinutes(context.Background(), start, end,
		AggregateParams{UnlimitedGroupsOnly: false}, limits, 0.8)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	// 文本断言：minutes CTE 投影列表与 GROUP BY 均含 l.group_limit；外层引用集 {group_limit} 全在投影中。
	minutesBlock := extractCTEBlock(t, captured, "minutes")
	require.Contains(t, minutesBlock, "l.group_limit", "P0：minutes CTE 投影必须含 l.group_limit")
	require.Regexp(t, `(?s)GROUP BY ul\.user_id, ul\.group_id, l\.group_limit, bucket_minute`, minutesBlock,
		"P0：minutes CTE GROUP BY 必须含 l.group_limit（外层 FILTER 引用入组安全）")
	for _, col := range []string{"group_limit"} {
		require.Contains(t, minutesBlock, col,
			"P0：外层引用的限位列 %s 必须在 minutes 投影中（join 后每行同值，入组安全）", col)
	}
	// 外层 FILTER 文本保持原语义（COUNT(*) FILTER (WHERE cnt::float8 >= group_limit * $6::float8)）。
	require.Contains(t, captured, "cnt::float8 >= group_limit * $6::float8")
	// 参数结构不变：仍 $1-$6。
	require.Contains(t, captured, "unnest($3::bigint[], $4::bigint[], $5::int[])")
}

// TestUsageRiskR8GlobalRPMProjectionIncludesUserLimit 锁定 [P0 外审 #2]：
// AggregateUserGlobalRPMMinutes 的 minutes CTE 必须投影并分组 l.user_limit。
func TestUsageRiskR8GlobalRPMProjectionIncludesUserLimit(t *testing.T) {
	start := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	limits := []UsageRiskUserLimit{{UserID: 1, UserLimit: 60}}

	var captured string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(func(expectedSQL, actualSQL string) error {
		captured = actualSQL
		return nil
	})))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := newUsageRiskRepositoryWithSQL(nil, db, db)

	mock.ExpectQuery(`WITH limits AS \(`).
		WithArgs(start, end, sqlmock.AnyArg(), sqlmock.AnyArg(), 0.9).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "hit"}).
			AddRow(int64(1), 5))

	_, err = repo.AggregateUserGlobalRPMMinutes(context.Background(), start, end, limits, 0.9)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	minutesBlock := extractCTEBlock(t, captured, "minutes")
	require.Contains(t, minutesBlock, "l.user_limit", "P0：minutes CTE 投影必须含 l.user_limit")
	require.Regexp(t, `(?s)GROUP BY ul\.user_id, l\.user_limit, bucket_minute`, minutesBlock,
		"P0：minutes CTE GROUP BY 必须含 l.user_limit")
	for _, col := range []string{"user_limit"} {
		require.Contains(t, minutesBlock, col,
			"P0：外层引用的限位列 %s 必须在 minutes 投影中", col)
	}
	// 外层 FILTER 文本保持原语义。
	require.Contains(t, captured, "cnt::float8 >= user_limit * $5::float8")
	require.Contains(t, captured, "unnest($3::bigint[], $4::int[])")
}

// TestUsageRiskR8CompleteRunPersistsCandidatesTotal 锁定 [P2 外审 #8]：
// CompleteRun 终态落账必须写 candidates_total（与 UpdateRunProgress 同位、参数序号顺延）。
func TestUsageRiskR8CompleteRunPersistsCandidatesTotal(t *testing.T) {
	repo, mock := newUsageRiskRepoMock(t)
	runID := int64(42)
	status := "completed"
	p := UsageRiskRunProgress{
		BatchesDone:      3,
		BatchesFailed:    1,
		FailedBatches:    json.RawMessage(`[{"day":"2026-09-23","offset":10}]`),
		BudgetExhausted:  true,
		CandidatesTotal:  128,
		ReconBatchesDone: 1,
		ReconCursorDate:  nil,
		ReconBatchOffset: nil,
		HistoryCovered:   true,
		ConsecutivePartials: 2,
		R1ReevalPending:  false,
		FinishedAt:       nil,
		FailureStage:     nil,
	}
	finished := time.Date(2026, 9, 24, 1, 30, 0, 0, time.UTC)
	p.FinishedAt = &finished

	mock.ExpectExec(`UPDATE usage_risk_runs\s+SET .*candidates_total = \$8.*WHERE run_id = \$15`).
		WithArgs(status, p.FinishedAt, p.FailureStage,
			p.BatchesDone, p.BatchesFailed, p.FailedBatches, p.BudgetExhausted,
			p.CandidatesTotal,
			p.ReconBatchesDone, p.ReconCursorDate, p.ReconBatchOffset,
			p.HistoryCovered, p.ConsecutivePartials, p.R1ReevalPending, runID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := repo.CompleteRun(context.Background(), runID, status, p)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestUsageRiskR8ReconCursorKeyRangeRemoved 锁定 [外审 #7 repo 侧]：
// UpdateRunProgress 与 CompleteRun 的 SQL 文本不得含 recon_cursor_key_range，
// 且参数序号前移后各列占位正确（占位最多到 run_id）。
func TestUsageRiskR8ReconCursorKeyRangeRemoved(t *testing.T) {
	runID := int64(42)
	status := "completed"
	finished := time.Date(2026, 9, 24, 1, 30, 0, 0, time.UTC)
	p := UsageRiskRunProgress{
		BatchesDone:        1,
		BatchesFailed:      0,
		FailedBatches:      json.RawMessage(`[]`),
		BudgetExhausted:    false,
		ReconCursorDate:    nil,
		ReconBatchOffset:   nil,
		ReconBatchesDone:   0,
		HistoryCovered:     true,
		CandidatesTotal:    12,
		ConsecutivePartials: 0,
		R1ReevalPending:    false,
		FinishedAt:         &finished,
		FailureStage:       nil,
	}

	// UpdateRunProgress 操作级（隐式文本断言）：参数序 = 9 个进度字段 + run_id。
	repo, mock := newUsageRiskRepoMock(t)
	mock.ExpectExec(`UPDATE usage_risk_runs\s+SET .*history_covered = \$9.*WHERE run_id = \$10`).
		WithArgs(p.BatchesDone, p.BatchesFailed, p.FailedBatches, p.BudgetExhausted,
			p.CandidatesTotal, p.ReconBatchesDone, p.ReconCursorDate,
			p.ReconBatchOffset, p.HistoryCovered, runID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, repo.UpdateRunProgress(context.Background(), runID, p))
	require.NoError(t, mock.ExpectationsWereMet())

	// CompleteRun 操作级：参数序 = status + 13 进度字段 + run_id（无 keyrange 位）。
	repo2, mock2 := newUsageRiskRepoMock(t)
	mock2.ExpectExec(`UPDATE usage_risk_runs\s+SET .*candidates_total = \$8.*r1_reeval_pending = \$14.*WHERE run_id = \$15`).
		WithArgs(status, p.FinishedAt, p.FailureStage,
			p.BatchesDone, p.BatchesFailed, p.FailedBatches, p.BudgetExhausted,
			p.CandidatesTotal,
			p.ReconBatchesDone, p.ReconCursorDate, p.ReconBatchOffset,
			p.HistoryCovered, p.ConsecutivePartials, p.R1ReevalPending, runID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, repo2.CompleteRun(context.Background(), runID, status, p))
	require.NoError(t, mock2.ExpectationsWereMet())
}

// TestUsageRiskR8LoadLatestReconCursorNoKeyRange 锁定 [外审 #7 repo 侧]：
// LoadLatestReconCursor 的 SELECT 不得含 recon_cursor_key_range，返回结构不含键区间
// （service.UsageRiskReconCursor 已删 CursorKeyRange——契约类型断言在 service 域编译期保证）。
func TestUsageRiskR8LoadLatestReconCursorNoKeyRange(t *testing.T) {
	repo, mock := newUsageRiskRepoMock(t)
	reconDate := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`SELECT recon_cursor_date, recon_batch_offset, failed_batches, history_covered\s+FROM usage_risk_runs\s+ORDER BY run_at DESC\s+LIMIT 1`).
		WillReturnRows(sqlmock.NewRows([]string{
			"recon_cursor_date", "recon_batch_offset", "failed_batches", "history_covered",
		}).AddRow(reconDate, int64(3), `[]`, true))

	cursor, err := repo.LoadLatestReconCursor(context.Background())
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
	require.NotNil(t, cursor)
	require.Equal(t, "2026-09-20", cursor.CursorDate.Format("2006-01-02"))
	require.NotNil(t, cursor.BatchOffset)
	require.Equal(t, 3, *cursor.BatchOffset)
	require.True(t, cursor.HistoryCovered)
}

// extractCTEBlock 提取 WITH 查询中指定 CTE 的块文本（含 SELECT 与 GROUP BY）。
func extractCTEBlock(t *testing.T, sqlText, cteName string) string {
	t.Helper()
	re := regexp.MustCompile(`(?s)` + regexp.QuoteMeta(cteName) + `\s+AS\s+\(\s*([\s\S]*?)\n\)`)
	m := re.FindStringSubmatch(sqlText)
	require.NotNil(t, m, "SQL 必须含 CTE %s：%s", cteName, sqlText)
	return m[1]
}

// ── R7-1 测试辅助 ──

// extractInsertTargetColumns 提取 INSERT INTO ... (cols) 中的目标列清单。
func extractInsertTargetColumns(t *testing.T, insertSQL string) []string {
	t.Helper()
	re := regexp.MustCompile(`(?s)INSERT\s+INTO\s+\w+\s*\(([^)]*)\)`)
	m := re.FindStringSubmatch(insertSQL)
	require.NotNil(t, m, "INSERT SQL 必须含目标列括号：%s", insertSQL)
	cols := strings.Split(m[1], ",")
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		out = append(out, c)
	}
	return out
}

// extractOuterSelectExprs 提取 aggregateHourlySQL() 中外层 SELECT 表达式清单。
// 外层 SELECT 特征：位于 CTE 闭合 ")" 之后，形如 ")\nSELECT expr1, expr2, ... FROM hourly"，
// 与 CTE 内 SELECT 区分（避免捕获 usage_logs 查询）。
func extractOuterSelectExprs(t *testing.T, sqlText string) []string {
	t.Helper()
	re := regexp.MustCompile(`(?s)\)\s*SELECT\s+([\s\S]*?)\s+FROM\s+hourly`)
	m := re.FindStringSubmatch(sqlText)
	require.NotNil(t, m, "SQL 必须含外层 SELECT ... FROM hourly：%s", sqlText)
	exprs := strings.Split(m[1], ",")
	out := make([]string, 0, len(exprs))
	for _, e := range exprs {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		out = append(out, e)
	}
	return out
}
// ───────────────── R10-1（外审⑥ #1/#2/#6）锁定测试 ─────────────────

// alignToAppHour 返回 t 按应用时区墙钟下取整到整点的时刻（测试侧独立表达式：墙钟
// 分钟秒纳秒回退，与生产 alignWindowStartToHour 双独立实现），并自检：结果不晚于 t、
// 与 t 相差 < 1h、墙钟时分秒为零。
func alignToAppHour(t *testing.T, ts time.Time) time.Time {
	t.Helper()
	w := ts.In(timezone.Location())
	aligned := ts.Add(-(time.Duration(w.Minute())*time.Minute +
		time.Duration(w.Second())*time.Second +
		time.Duration(w.Nanosecond())*time.Nanosecond))
	wa := aligned.In(timezone.Location())
	require.Zero(t, wa.Minute(), "对齐结果墙钟分钟必须为零")
	require.Zero(t, wa.Second(), "对齐结果墙钟秒必须为零")
	require.False(t, aligned.After(ts), "下取整结果不得晚于原时刻")
	require.LessOrEqual(t, ts.Sub(aligned), time.Hour, "对齐偏移不得超过 1 小时")
	return aligned
}

// TestUsageRiskR10_1_RebuildWindowBucketAlignedStart 锁定 [P0]（外审⑥ #1）：
// RebuildHourlyWindow 的 DELETE 与 INSERT 第一参数必须是对齐后整点——非整点起点会使
// 首段日志归入早于起点的 trunc 桶，DELETE bucket_hour >= windowStart 不覆盖该桶，
// 重插同键 (user_id,group_id,bucket_hour) 唯一约束冲突（周期任务第二轮必炸）。
// mutation 反证：去掉对齐（直传 windowStart=10:37:23）→ WithArgs(10:00:00) 不匹配 → FAIL。
func TestUsageRiskR10_1_RebuildWindowBucketAlignedStart(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := newUsageRiskRepositoryWithSQL(nil, db, db)

	// 非整点起点 10:37:23 → 对齐后应为应用时区整点 10:00:00。
	start := time.Date(2026, 9, 24, 10, 37, 23, 0, time.UTC)
	end := start.Add(26 * time.Hour)
	aligned := alignToAppHour(t, start)
	require.Equal(t, 37, start.Minute(), "前置：构造的起点必须非整点（否则反证无效）")
	require.Zero(t, aligned.Minute(), "前置：对齐后必须为整点")

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM user_usage_metrics_rollup`).
		WithArgs(aligned, end).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO user_usage_metrics_rollup`).
		WithArgs(aligned, end, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	_, err = repo.RebuildHourlyWindow(context.Background(), start, end,
		AggregateParams{UnlimitedGroupsOnly: false})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestUsageRiskR10_1_ListCandidatesUserLevelUnion 锁定 [P1]（外审⑥ #2）：
// 候选集必须 = 分组达标键 ∪ 用户全量达标用户的全部资格分组键（UNION）——跨分组各
// 不足门槛但用户合计达标的用户，其全部分组键必须进入候选，否则 R1/R4/R5/R6/R8
// 用户级规则漏报。文本断言 UNION 与用户级 IN 子查询（HAVING 门槛两处）；行为断言
// UNION 产物键可扫描。mutation 反证：去掉 UNION 分支或 IN 子查询 → 文本断言 FAIL。
func TestUsageRiskR10_1_ListCandidatesUserLevelUnion(t *testing.T) {
	var capturedSQL string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(func(expectedSQL, actualSQL string) error {
		capturedSQL = actualSQL
		return nil
	})))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := newUsageRiskRepositoryWithSQL(nil, db, db)
	start := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)

	// 行为：UNION 产物（外层 c 列名 user_id/group_id/request_count）行可扫描。
	mock.ExpectQuery(`UNION`).
		WithArgs(start, end, 10, 100, 100).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "group_id", "request_count"}).
			AddRow(int64(7), int64(3), 4). // 用户级达标补入的跨分组键（分组计数 < 门槛）
			AddRow(int64(7), int64(5), 9)) // 同用户另一资格分组键
	items, err := repo.ListCandidates(context.Background(), start, end,
		AggregateParams{UnlimitedGroupsOnly: true}, 10, 100, 100)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Len(t, items, 2)
	require.Equal(t, int64(3), items[0].GroupID)
	require.Equal(t, int64(5), items[1].GroupID)

	// 文本：必须含 UNION 与用户级 IN 子查询；HAVING 门槛两处（分组级 + 用户级）。
	require.Contains(t, capturedSQL, "UNION", "R10-1/#2：候选集必须是分组达标 ∪ 用户级达标的 UNION")
	require.Contains(t, capturedSQL, "ul.user_id IN (", "R10-1/#2：必须含用户全量达标 IN 子查询")
	require.Equal(t, 2, strings.Count(capturedSQL, "HAVING COUNT(*) >= $3"),
		"R10-1/#2：分组级与用户级门槛（HAVING）必须各出现一次")
}

// TestUsageRiskR10_1_InvalidatedReportNotFound 锁定 [P2]（外审⑥ #6）：
// 失效报告（invalidated_at 非空）按 not-found 公开契约——GetReportDetail WHERE 与
// 状态接口 UPDATE WHERE 均必须过滤 invalidated_at IS NULL。
// mutation 反证：去掉任一过滤 → 对应断言 FAIL。
func TestUsageRiskR10_1_InvalidatedReportNotFound(t *testing.T) {
	// 状态接口 UPDATE：失效过滤（源常量文本断言）。
	require.Contains(t, reportStatusUpdateSQL, "invalidated_at IS NULL",
		"R10-1/#6：状态接口 UPDATE 必须过滤已失效报告")

	// GetReportDetail：捕获实际 SQL 文本断言失效过滤 + 空结果走 not-found 语义。
	var capturedSQL string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(func(expectedSQL, actualSQL string) error {
		capturedSQL = actualSQL
		return nil
	})))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := newUsageRiskRepositoryWithSQL(nil, db, db)

	mock.ExpectQuery(`SELECT r\.report_id`).
		WillReturnRows(sqlmock.NewRows([]string{"report_id"}))
	_, err = repo.GetReportDetail(context.Background(), 9)
	require.ErrorIs(t, err, service.ErrUsageRiskReportNotFound, "空结果必须映射为 not-found 公开语义")
	require.NoError(t, mock.ExpectationsWereMet())
	require.Contains(t, capturedSQL, "invalidated_at IS NULL",
		"R10-1/#6：GetReportDetail SQL 必须含失效过滤")
}

// TestUsageRiskR11_DSTFallbackAlignedNotFuture 锁定 [P1]（外审⑦ #1）：
// alignWindowStartToHour 在 DST 回拨重叠小时必须取 ts 同 occurrence 的墙钟整点，
// 绝不向未来对齐（time.Date 歧义 occurrence 实测取第二个=未来，DELETE/INSERT 将
// 遗漏窗口开头段、残留陈旧聚合，违反 SSOT §5 实际覆盖 ≥26h）。
// mutation 反证：退回 time.Date 构造 → Berlin 第一次 02:37 用例对齐到 UTC 01:00
// （未来）→ After 断言 FAIL。
func TestUsageRiskR11_DSTFallbackAlignedNotFuture(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	require.NoError(t, err)
	lordHowe, err := time.LoadLocation("Australia/Lord_Howe")
	require.NoError(t, err)
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)

	// 2026-10-25 03:00 CEST 回拨 02:00 CET（欧洲夏令时结束）：
	// 第一次 02:37 = UTC 00:37（CEST +02）；第二次 02:37 = UTC 01:37（CET +01）。
	cases := []struct {
		name    string
		loc     *time.Location
		ts      time.Time // 以 UTC instant 表达
		wantUTC string
	}{
		{"Berlin 回拨第一次 02:37 CEST", berlin,
			time.Date(2026, 10, 25, 0, 37, 0, 0, time.UTC), "2026-10-25 00:00:00 +0000 UTC"},
		{"Berlin 回拨第二次 02:37 CET", berlin,
			time.Date(2026, 10, 25, 1, 37, 0, 0, time.UTC), "2026-10-25 01:00:00 +0000 UTC"},
		// Lord Howe（半小时 DST）：2026-04-05 02:00 +11:00 回拨 01:30 +10:30（=UTC 04-04 15:00）。
		// 第一次 01:45 = UTC 14:45（+11）；第二次 01:45 = UTC 15:15（+10:30）。
		{"Lord Howe 回拨第一次 01:45 +11", lordHowe,
			time.Date(2026, 4, 4, 14, 45, 0, 0, time.UTC), "2026-04-04 14:00:00 +0000 UTC"},
		{"Lord Howe 回拨第二次 01:45 +10:30", lordHowe,
			time.Date(2026, 4, 4, 15, 15, 0, 0, time.UTC), "2026-04-04 14:30:00 +0000 UTC"},
		{"上海无 DST 常规", shanghai,
			time.Date(2026, 9, 24, 2, 37, 23, 0, time.UTC), "2026-09-24 02:00:00 +0000 UTC"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := alignWindowStartToHour(tc.loc, tc.ts)
			require.False(t, got.After(tc.ts), "对齐结果绝不允许晚于原时刻（DST 歧义向未来即数据遗漏）")
			require.LessOrEqual(t, tc.ts.Sub(got), time.Hour, "对齐偏移不得超过 1 小时")
			// wantUTC 按"ts 所在 occurrence 的墙钟整点"人工预算——got instant 自身的
			// occurrence 可能与 ts 不同（回退跨回拨点，如 Lord Howe 半小时重跳），故
			// 不能用 got.In(loc) 的墙钟分钟做通用判定，以精确 instant 断言为锚。
			require.Equal(t, tc.wantUTC, got.UTC().String(), "必须对齐到 ts 同 occurrence 的墙钟整点")
		})
	}

	// 无 DST 时区与 time.Date 构造等价（生产默认时区行为零变化的回归锚）。
	ws := time.Date(2026, 9, 24, 2, 37, 23, 0, time.UTC)
	w := ws.In(shanghai)
	legacyDate := time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), 0, 0, 0, shanghai)
	require.True(t, legacyDate.Equal(alignWindowStartToHour(shanghai, ws)),
		"无 DST 时区下回退法与 time.Date 构造必须等价")
}
