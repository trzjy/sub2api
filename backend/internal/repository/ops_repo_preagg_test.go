package repository

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 缺陷2：preagg / collector / trends 三处「上游错误」分子副本缺最终状态门控。
// ops_error_logs 里的 recovered 行（logOpsRecoveredUpstream 为「上游失败但最终
// 成功」的请求记的 provider 归属错误）status_code 是 2xx、upstream_status_code
// 非 2xx。缺 `status_code >= 400` 门控时它们被算成上游错误，生产实测占该分子
// 54%（preagg 2252 vs raw 1036，同一屏差 2.17 倍）。
//
// 本文件证明：preagg（hourly 表）路径的分子谓词已与 raw 仪表盘逐字同源，
// 且 daily 聚合的 p95/p99 不再用 MAX 退化（否则跨层口径被打回原形）。

// sqlCaptureRecorder 记录 repository 方法实际发出的 SQL。
// 与 ops_repo_dashboard_test.go 的 sqlCapture 同名不同类型会冲突，故此处独立命名。
type sqlCaptureRecorder struct {
	mu  sync.Mutex
	sql string
}

// Match 实现 sqlmock.QueryMatcher：接受任意查询并记录它。
func (c *sqlCaptureRecorder) Match(_, actualSQL string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sql = actualSQL
	return nil
}

func (c *sqlCaptureRecorder) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sql
}

func newCapturingOpsRepoForPreagg(t *testing.T) (*opsRepository, sqlmock.Sqlmock, *sqlCaptureRecorder) {
	t.Helper()
	cap := &sqlCaptureRecorder{}
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(cap))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return &opsRepository{db: db}, mock, cap
}

// TestPreaggHourlyUpstreamNumeratorGatedOnFinalStatus 是缺陷2 的核心回归证据：
// UpsertHourlyMetrics 发出的 SQL 里，三个上游分子谓词必须与 raw 仪表盘
// （ops_repo_dashboard.go queryErrorCounts）逐字一致，且都带最终状态门控。
//
// 若有人日后把门控从 preagg 删掉（例如「优化」谓词），本测试立刻失败。
func TestPreaggHourlyUpstreamNumeratorGatedOnFinalStatus(t *testing.T) {
	repo, mock, cap := newCapturingOpsRepoForPreagg(t)
	mock.ExpectExec(".*").WillReturnResult(sqlmock.NewResult(0, 0))

	start := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	require.NoError(t, repo.UpsertHourlyMetrics(context.Background(), start, end))

	sent := cap.String()
	require.NotEmpty(t, sent, "UpsertHourlyMetrics must have issued a statement")

	// 与 raw 仪表盘逐字同源的三个分子（preagg 的最终状态列是 CTE 投影名
	// client_status_code，见 error_base）。
	gate := service.OpsUpstreamErrorFilterFinalStatusGate("client_status_code")
	excl := service.OpsUpstreamErrorFilterSQL("client_status_code")
	c429 := service.OpsUpstream429FilterFor("client_status_code")
	c529 := service.OpsUpstream529FilterFor("client_status_code")

	require.Contains(t, sent, "FILTER (WHERE "+excl+") AS upstream_error_count_excl_429_529",
		"hourly upstream_error_count_excl_429_529 must use the shared gated predicate; got:\n%s", sent)
	require.Contains(t, sent, "FILTER (WHERE "+c429+") AS upstream_429_count",
		"hourly upstream_429_count must use the shared predicate; got:\n%s", sent)
	require.Contains(t, sent, "FILTER (WHERE "+c529+") AS upstream_529_count",
		"hourly upstream_529_count must use the shared predicate; got:\n%s", sent)

	// 门控必须真的出现在 excl 分子里 —— 这是 recovered 行被排除的全部依据。
	require.True(t, strings.Contains(excl, ">= 400"),
		"the excl predicate must gate on the FINAL client status, otherwise recovered rows count as upstream errors")
	require.Contains(t, sent, gate+" AND error_owner = 'provider'",
		"the final-status gate must precede the provider filter")

	// 反向断言：旧的漏门控写法不得残留。
	require.NotContains(t, sent,
		"error_owner = 'provider' AND NOT is_business_limited AND COALESCE(effective_status_code, 0)",
		"the ungated legacy predicate must not survive in the hourly upsert")
}

// TestPreaggErrorBaseProjectsUpstreamStatusCodeRawName 证明 error_base CTE 投影出
// 原名 upstream_status_code：共享谓词模板内部按 ops_error_logs 原始列名写
// COALESCE(upstream_status_code, <最终状态列>, 0)，两列同名才能直接套用。
// 若有人把这里改回 COALESCE(...) AS effective_status_code，谓词会引用到不存在的列
// 而在生产上报 SQL 错误。
func TestPreaggErrorBaseProjectsUpstreamStatusCodeRawName(t *testing.T) {
	repo, mock, cap := newCapturingOpsRepoForPreagg(t)
	mock.ExpectExec(".*").WillReturnResult(sqlmock.NewResult(0, 0))

	start := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	require.NoError(t, repo.UpsertHourlyMetrics(context.Background(), start, start.Add(time.Hour)))

	sent := cap.String()
	require.Contains(t, sent, "upstream_status_code\n  FROM ops_error_logs",
		"error_base must project upstream_status_code under its original name")
	require.Contains(t, sent, "status_code AS client_status_code",
		"error_base must project the final status as client_status_code for the shared predicate")
	require.NotContains(t, sent, "AS effective_status_code",
		"effective_status_code is gone; the shared predicate expects the raw upstream_status_code")
}

// TestPreaggDailyTailPercentilesAreWeightedNotMax 覆盖任务要求的第二项：daily 聚合
// 曾把 hourly 已算好的 p95/p99 又用 MAX 退化，而 Go 侧
// （ops_repo_dashboard.go combineApproxPercentiles）用的是样本加权平均。同一份数据
// 经 hourly→daily 与直接走 Go 侧会给出不同的 p95/p99，跨层口径被打回原形。
func TestPreaggDailyTailPercentilesAreWeightedNotMax(t *testing.T) {
	repo, mock, cap := newCapturingOpsRepoForPreagg(t)
	mock.ExpectExec(".*").WillReturnResult(sqlmock.NewResult(0, 0))

	start := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	require.NoError(t, repo.UpsertDailyMetrics(context.Background(), start, start.Add(24*time.Hour)))

	sent := cap.String()
	require.NotEmpty(t, sent, "UpsertDailyMetrics must have issued a statement")

	// p95/p99 必须按样本数加权平均，而不是 MAX。
	for _, col := range []string{
		"duration_p50_ms", "duration_p90_ms", "duration_p95_ms", "duration_p99_ms",
		"ttft_p50_ms", "ttft_p90_ms", "ttft_p95_ms", "ttft_p99_ms",
	} {
		require.Contains(t, sent, "SUM("+col+"::double precision *",
			"%s must merge as a sample-weighted average; got:\n%s", col, sent)
		require.NotContains(t, sent, "MAX("+col+") AS "+col,
			"%s must not regress to MAX-of-segments; got:\n%s", col, sent)
	}

	// 权重列：duration 用 success_count，TTFT 用 ttft_sample_count（只有流式行有首包时间）。
	require.Contains(t, sent, "SUM(duration_p95_ms::double precision * success_count)",
		"duration percentiles weight by success_count")
	require.Contains(t, sent, "SUM(ttft_p95_ms::double precision * ttft_sample_count)",
		"TTFT percentiles weight by ttft_sample_count, not success_count")

	// max 类字段仍应保持 MAX（max 对 max 是精确合并，不是近似）。
	require.Contains(t, sent, "MAX(duration_max_ms) AS duration_max_ms",
		"max is exactly mergeable and must stay MAX")
	require.Contains(t, sent, "MAX(ttft_max_ms) AS ttft_max_ms",
		"max is exactly mergeable and must stay MAX")
}

// TestUpstreamErrorPredicateSharedAcrossAllFourCopies 把同一指标 4 份副本的谓词
// 文本摆在同一处比对：只要有一份漂移（哪怕语义等价但写法不同），本测试就失败。
// 这是防止口径再次分叉的 SSOT 断言。
func TestUpstreamErrorPredicateSharedAcrossAllFourCopies(t *testing.T) {
	const finalCol = "status_code"

	// #1 raw 仪表盘（ops_repo_dashboard.go queryErrorCounts，已修且不在本次可改范围）
	// 的谓词文本，即其余三份必须对齐的目标。逐字复制自该文件 :972-974。
	const dashboardExcl = `COALESCE(status_code, 0) >= 400 AND error_owner = 'provider' AND NOT is_business_limited AND COALESCE(upstream_status_code, status_code, 0) NOT IN (429, 529)`
	const dashboard429 = `error_owner = 'provider' AND NOT is_business_limited AND COALESCE(upstream_status_code, status_code, 0) = 429`
	const dashboard529 = `error_owner = 'provider' AND NOT is_business_limited AND COALESCE(upstream_status_code, status_code, 0) = 529`

	require.Equal(t, dashboardExcl, service.OpsUpstreamErrorFilterSQL(finalCol),
		"the shared excl predicate must stay byte-identical to the raw dashboard's")
	require.Equal(t, dashboard429, service.OpsUpstream429FilterFor(finalCol),
		"the shared 429 predicate must stay byte-identical to the raw dashboard's")
	require.Equal(t, dashboard529, service.OpsUpstream529FilterFor(finalCol),
		"the shared 529 predicate must stay byte-identical to the raw dashboard's")
	require.Equal(t, `COALESCE(status_code, 0) >= 400`,
		service.OpsUpstreamErrorFilterFinalStatusGate(finalCol),
		"the final-status gate must stay byte-identical to the raw dashboard's")

	// preagg 用 CTE 投影列名渲染：谓词主体与 raw 版完全相同，只有**最终状态列**
	// （status_code → client_status_code）改名；upstream_status_code 保持原名
	// （error_base CTE 按原名投影它）。最终状态列在谓词里出现两次（门控 + COALESCE
	// 兜底），故按整串替换而非 ReplaceAll —— 后者会把 upstream_status_code 一并改坏。
	expectedPreagg := strings.ReplaceAll(dashboardExcl, "(status_code, 0)", "(client_status_code, 0)")
	expectedPreagg = strings.ReplaceAll(expectedPreagg, ", status_code, 0)", ", client_status_code, 0)")
	require.Equal(t, expectedPreagg, service.OpsUpstreamErrorFilterSQL("client_status_code"),
		"preagg's predicate must be the raw predicate with only the final-status column renamed")
	require.Contains(t, expectedPreagg, "COALESCE(upstream_status_code, client_status_code, 0)",
		"the CTE must keep projecting upstream_status_code under its original name")

	// 本包的两份副本（hourly 表 + 趋势图）发出的 SQL 都必须带共享谓词。
	trendsSQL := captureOpsErrorTrendSQL(t)
	require.Contains(t, trendsSQL, "FILTER (WHERE "+service.OpsUpstreamErrorFilterSQL(finalCol)+") AS upstream_excl",
		"the error-trend query must carry the shared excl predicate verbatim")
	require.Contains(t, trendsSQL, "FILTER (WHERE "+service.OpsUpstream429FilterFor(finalCol)+") AS upstream_429",
		"the error-trend query must carry the shared 429 predicate verbatim")
	require.Contains(t, trendsSQL, "FILTER (WHERE "+service.OpsUpstream529FilterFor(finalCol)+") AS upstream_529",
		"the error-trend query must carry the shared 529 predicate verbatim")
}

// captureOpsErrorTrendSQL 跑一次 GetErrorTrend 并返回它实际发出的 SQL。
func captureOpsErrorTrendSQL(t *testing.T) string {
	t.Helper()
	repo, mock, cap := newCapturingOpsRepoForPreagg(t)
	mock.ExpectQuery(".*").WillReturnRows(sqlmock.NewRows([]string{
		"bucket", "error_total", "business_limited", "error_sla",
		"upstream_excl", "upstream_429", "upstream_529",
	}))

	end := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	filter := &service.OpsDashboardFilter{StartTime: end.Add(-time.Hour), EndTime: end}
	_, err := repo.GetErrorTrend(context.Background(), filter, 60)
	require.NoError(t, err)
	return cap.String()
}
