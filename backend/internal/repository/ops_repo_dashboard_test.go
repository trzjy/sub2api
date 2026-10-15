package repository

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// sqlCapture records the SQL a repository method actually sends. Installed as a
// sqlmock query matcher that accepts everything, so tests can assert on the
// exact predicate text that reaches the database.
type sqlCapture struct {
	mu  sync.Mutex
	sql string
}

// Match implements sqlmock.QueryMatcher: it accepts any query and records it.
func (c *sqlCapture) Match(_, actualSQL string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sql = actualSQL
	return nil
}

func (c *sqlCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sql
}

// newCapturingOpsRepo returns a repo whose mock accepts any query and records it.
func newCapturingOpsRepo(t *testing.T) (*opsRepository, sqlmock.Sqlmock, *sqlCapture) {
	t.Helper()
	cap := &sqlCapture{}
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(cap))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return &opsRepository{db: db}, mock, cap
}

func newOpsDashboardRepo(t *testing.T) (*opsRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return &opsRepository{db: db}, mock
}

func dashboardWindow() (time.Time, time.Time) {
	end := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	return end.Add(-1 * time.Hour), end
}

func errorCountRows(total, businessLimited, sla, excl, c429, c529 int64) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"error_total", "business_limited", "error_sla",
		"upstream_excl", "upstream_429", "upstream_529",
	}).AddRow(total, businessLimited, sla, excl, c429, c529)
}

func usageCountRows(success, tokens int64) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"success_count", "token_consumed"}).AddRow(success, tokens)
}

func peakRows(maxReq, maxTokens int64) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"max_req_per_min", "max_tokens_per_min"}).AddRow(maxReq, maxTokens)
}

// filterClauseForAlias extracts "FILTER (WHERE <predicate>) AS <alias>" so an
// assertion targets exactly one metric's numerator.
func filterClauseForAlias(t *testing.T, sqlText, alias string) string {
	t.Helper()
	needle := ") AS " + alias
	idx := strings.Index(sqlText, needle)
	require.GreaterOrEqual(t, idx, 0, "alias %q not found in query:\n%s", alias, sqlText)
	filterIdx := strings.LastIndex(sqlText[:idx], "FILTER (")
	require.GreaterOrEqual(t, filterIdx, 0, "no FILTER clause for alias %q", alias)
	return sqlText[filterIdx:idx]
}

// safeBuffer is a concurrency-safe sink for the captured slog output.
type safeBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

// captureLogs redirects the default slog logger into a buffer for one test.
func captureLogs(t *testing.T) *safeBuffer {
	t.Helper()
	buf := &safeBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func intPtr(v int) *int { return &v }

// ---------------------------------------------------------------------------
// Remediation 1: upstream error numerator must exclude recovered rows.
//
// ops_error_logs mixes two row shapes under error_owner = 'provider':
//   - genuine upstream failures: status_code >= 400
//   - "recovered" rows (handler/ops_error_logger.go logOpsRecoveredUpstream):
//     the request ultimately SUCCEEDED, so status_code is 2xx while
//     upstream_status_code still records the failed upstream attempt.
//
// Without a final-status gate the second shape is counted as an upstream error.
// ---------------------------------------------------------------------------

func TestQueryErrorCounts_UpstreamExclGatedOnFinalStatus(t *testing.T) {
	repo, mock, cap := newCapturingOpsRepo(t)
	mock.ExpectQuery(".*").WillReturnRows(errorCountRows(0, 0, 0, 0, 0, 0))

	start, end := dashboardWindow()
	_, _, _, upstreamExcl, _, _, err := repo.queryErrorCounts(context.Background(), nil, start, end)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Equal(t, int64(0), upstreamExcl)

	clause := filterClauseForAlias(t, cap.String(), "upstream_excl")
	require.Contains(t, clause, "COALESCE(status_code, 0) >= 400",
		"upstream_excl must only count requests that actually failed")
	require.Contains(t, clause, "error_owner = 'provider'")
	require.Contains(t, clause, "NOT is_business_limited")
	require.Contains(t, clause, "NOT IN (429, 529)")
}

func TestQueryErrorCounts_UpstreamExclKeepsUpstreamStatusFor429Exclusion(t *testing.T) {
	repo, mock, cap := newCapturingOpsRepo(t)
	mock.ExpectQuery(".*").WillReturnRows(errorCountRows(0, 0, 0, 0, 0, 0))

	start, end := dashboardWindow()
	_, _, _, _, _, _, err := repo.queryErrorCounts(context.Background(), nil, start, end)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	clause := filterClauseForAlias(t, cap.String(), "upstream_excl")
	// The 429/529 carve-out stays keyed on the UPSTREAM status ...
	require.Contains(t, clause, "COALESCE(upstream_status_code, status_code, 0) NOT IN (429, 529)")
	// ... while the success gate is keyed on the FINAL client status.
	require.Contains(t, clause, "COALESCE(status_code, 0) >= 400")
	require.NotContains(t, clause, "COALESCE(upstream_status_code, status_code, 0) >= 400",
		"gating on the upstream status would let recovered rows back in")
}

func TestQueryErrorCounts_UpstreamExclAlignedWithErrorTotal(t *testing.T) {
	repo, mock, cap := newCapturingOpsRepo(t)
	mock.ExpectQuery(".*").WillReturnRows(errorCountRows(0, 0, 0, 0, 0, 0))

	start, end := dashboardWindow()
	_, _, _, _, _, _, err := repo.queryErrorCounts(context.Background(), nil, start, end)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	sqlText := cap.String()
	errorTotal := filterClauseForAlias(t, sqlText, "error_total")
	errorSLA := filterClauseForAlias(t, sqlText, "error_sla")
	upstreamExcl := filterClauseForAlias(t, sqlText, "upstream_excl")

	require.Contains(t, errorTotal, "COALESCE(status_code, 0) >= 400")
	require.Contains(t, errorSLA, "COALESCE(status_code, 0) >= 400")
	require.Contains(t, upstreamExcl, "COALESCE(status_code, 0) >= 400",
		"upstream_excl must use the same final-status gate as error_total/error_sla")
}

// ---------------------------------------------------------------------------
// Remediation 2: p95/p99 merge as weighted averages; max keeps max-of-segments.
// ---------------------------------------------------------------------------

func TestCombineApproxPercentiles_TailIsWeightedNotMax(t *testing.T) {
	got := combineApproxPercentiles([]opsPercentileSegment{
		{weight: 900, p: service.OpsPercentiles{P95: intPtr(100), P99: intPtr(200), Max: intPtr(5000)}},
		{weight: 100, p: service.OpsPercentiles{P95: intPtr(900), P99: intPtr(1800), Max: intPtr(9000)}},
	})

	require.NotNil(t, got.P95)
	require.Equal(t, 180, *got.P95, "p95 must be the sample-weighted average (max would be 900)")

	require.NotNil(t, got.P99)
	require.Equal(t, 360, *got.P99, "p99 must be the sample-weighted average (max would be 1800)")

	require.NotNil(t, got.Max)
	require.Equal(t, 9000, *got.Max, "max must remain max-of-segments (mathematically exact)")
}

func TestCombineApproxPercentiles_TailInflationIsBounded(t *testing.T) {
	// Reproduces the production symptom: many ordinary buckets plus one
	// pathological bucket used to inflate the window p99 by ~34%.
	var segments []opsPercentileSegment
	for i := 0; i < 24; i++ {
		segments = append(segments, opsPercentileSegment{
			weight: 100,
			p: service.OpsPercentiles{
				P95: intPtr(1000 + i),
				P99: intPtr(2000 + i),
			},
		})
	}
	segments = append(segments, opsPercentileSegment{
		weight: 100,
		p:      service.OpsPercentiles{P95: intPtr(5000), P99: intPtr(9000)},
	})

	got := combineApproxPercentiles(segments)
	require.NotNil(t, got.P99)
	require.Less(t, *got.P99, 3000, "p99 must not be dominated by a single outlier bucket")
	require.Greater(t, *got.P99, 2100)
}

func TestCombineApproxPercentiles_ZeroWeightSegmentsIgnoredForTail(t *testing.T) {
	got := combineApproxPercentiles([]opsPercentileSegment{
		{weight: 0, p: service.OpsPercentiles{P95: intPtr(99999), P99: intPtr(99999), Max: intPtr(99999)}},
		{weight: 10, p: service.OpsPercentiles{P95: intPtr(100), P99: intPtr(200), Max: intPtr(300)}},
	})
	require.Equal(t, 100, *got.P95)
	require.Equal(t, 200, *got.P99)
	// Max carries no weight, so it still reflects the largest observed value.
	require.Equal(t, 99999, *got.Max)
}

func TestCombineApproxPercentiles_EmptySegmentsYieldNil(t *testing.T) {
	got := combineApproxPercentiles(nil)
	require.Nil(t, got.P95)
	require.Nil(t, got.P99)
	require.Nil(t, got.Max)
}

func TestAggregateHourlyRows_TailIsWeightedNotMax(t *testing.T) {
	got := aggregateHourlyRows([]opsHourlyMetricsRow{
		{
			successCount: 900,
			durationP50:  sql.NullInt64{Int64: 50, Valid: true},
			durationP95:  sql.NullInt64{Int64: 100, Valid: true},
			durationP99:  sql.NullInt64{Int64: 200, Valid: true},
			durationMax:  sql.NullInt64{Int64: 5000, Valid: true},
		},
		{
			successCount: 100,
			durationP50:  sql.NullInt64{Int64: 60, Valid: true},
			durationP95:  sql.NullInt64{Int64: 900, Valid: true},
			durationP99:  sql.NullInt64{Int64: 1800, Valid: true},
			durationMax:  sql.NullInt64{Int64: 9000, Valid: true},
		},
	})

	require.Equal(t, 180, *got.duration.P95, "hourly p95 must merge as a weighted average")
	require.Equal(t, 360, *got.duration.P99, "hourly p99 must merge as a weighted average")
	require.Equal(t, 9000, *got.duration.Max, "hourly max must remain max-of-buckets")
}

func TestAggregateHourlyRows_ZeroSampleBucketDoesNotSetTail(t *testing.T) {
	got := aggregateHourlyRows([]opsHourlyMetricsRow{
		{successCount: 0, durationP95: sql.NullInt64{Int64: 7777, Valid: true}},
		{successCount: 10, durationP95: sql.NullInt64{Int64: 100, Valid: true}},
	})
	require.NotNil(t, got.duration.P95)
	require.Equal(t, 100, *got.duration.P95,
		"a bucket with no samples must not contribute to the weighted tail")
}

func TestAggregateHourlyRows_TTFTTailWeightedBySampleCount(t *testing.T) {
	got := aggregateHourlyRows([]opsHourlyMetricsRow{
		{
			ttftSampleCount: 900,
			ttftP95:         sql.NullInt64{Int64: 50, Valid: true},
			ttftP99:         sql.NullInt64{Int64: 100, Valid: true},
			ttftMax:         sql.NullInt64{Int64: 900, Valid: true},
		},
		{
			ttftSampleCount: 100,
			ttftP95:         sql.NullInt64{Int64: 450, Valid: true},
			ttftP99:         sql.NullInt64{Int64: 900, Valid: true},
			ttftMax:         sql.NullInt64{Int64: 1800, Valid: true},
		},
	})

	require.Equal(t, 90, *got.ttft.P95, "50*0.9 + 450*0.1")
	require.Equal(t, 180, *got.ttft.P99, "100*0.9 + 900*0.1")
	require.Equal(t, 1800, *got.ttft.Max, "ttft max must remain max-of-buckets")
}

func TestAggregateHourlyRows_TailNilWithoutSamples(t *testing.T) {
	got := aggregateHourlyRows([]opsHourlyMetricsRow{
		{successCount: 0, ttftSampleCount: 0, ttftP95: sql.NullInt64{Int64: 42, Valid: true}},
	})
	require.Nil(t, got.duration.P95, "no samples must report no tail, not a stray value")
	require.Nil(t, got.ttft.P95)
}

// ---------------------------------------------------------------------------
// Remediation 3: dashboard rate precision.
// ---------------------------------------------------------------------------

func TestRoundRate(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want float64
	}{
		{"production symptom 469 req / 3600s", 469.0 / 3600.0, 0.1303},
		{"zero stays zero", 0, 0},
		{"tiny value keeps resolution", 0.0004, 0.0004},
		{"just below one", 0.9999, 0.9999},
		{"exactly one", 1, 1},
		{"large tps", 16260.2, 16260.2},
		{"large tps rounds to 2dp", 16260.256, 16260.26},
		{"negative sub-one", -0.5, -0.5},
		{"qps peak 5/min", 5.0 / 60.0, 0.0833},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.InDelta(t, tc.want, roundRate(tc.in), 1e-9)
		})
	}
}

func TestRoundRate_ResolvesWhatOneDPLoses(t *testing.T) {
	// Each of these collapses onto 0.1 (or 0.0) under roundTo1DP.
	for _, v := range []float64{0.13, 0.09, 0.02, 0.004, 469.0 / 3600.0} {
		require.NotEqual(t, roundTo1DP(v), roundRate(v),
			"roundRate must not collapse low rates the way roundTo1DP does (v=%v)", v)
	}
}

// ---------------------------------------------------------------------------
// Remediation 5: timeout degradation must be logged, not silent.
// ---------------------------------------------------------------------------

// sqlmock's default QueryMatcherRegexp collapses whitespace runs in the
// EXPECTED regexp as well as in the actual query, so `\s+` inside an
// expectation degrades into a literal " +" and stops matching. These patterns
// therefore use single spaces.
const (
	opsUsageCountsPattern = `SELECT COALESCE\(COUNT\(\*\), 0\) AS success_count, COALESCE\(SUM\(input_tokens \+ output_tokens \+ cache_creation_tokens \+ cache_read_tokens\), 0\) AS token_consumed FROM usage_logs`
	opsLatencyPattern     = `SELECT percentile_cont\(0\.50\) WITHIN GROUP \(ORDER BY duration_ms\) FILTER \(WHERE duration_ms IS NOT NULL\) AS duration_p50, percentile_cont\(0\.90\)`
	opsErrorCountsPattern = `SELECT COALESCE\(COUNT\(\*\) FILTER \(WHERE COALESCE\(status_code, 0\) >= 400\), 0\) AS error_total,`
	opsPeakRatesPattern   = `WITH usage_buckets AS`
)

func TestGetDashboardOverviewRaw_LatencyTimeoutIsLogged(t *testing.T) {
	repo, mock := newOpsDashboardRepo(t)
	start, end := dashboardWindow()

	mock.ExpectQuery(opsUsageCountsPattern).WillReturnRows(usageCountRows(10, 1000))
	mock.ExpectQuery(opsLatencyPattern).WillReturnError(context.DeadlineExceeded)
	mock.ExpectQuery(opsErrorCountsPattern).WillReturnRows(errorCountRows(1, 0, 1, 0, 0, 0))
	mock.ExpectQuery(opsUsageCountsPattern).WillReturnRows(usageCountRows(1, 100))
	mock.ExpectQuery(opsErrorCountsPattern).WillReturnRows(errorCountRows(0, 0, 0, 0, 0, 0))
	mock.ExpectQuery(opsPeakRatesPattern).WillReturnRows(peakRows(20, 2000))

	logs := captureLogs(t)

	got, err := repo.getDashboardOverviewRaw(context.Background(), &service.OpsDashboardFilter{
		StartTime: start,
		EndTime:   end,
	})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	// Latency fields are dropped because the data is genuinely unavailable ...
	require.Nil(t, got.Duration.P99)
	require.Nil(t, got.TTFT.P99)
	// ... but the gap must be visible in the logs instead of reading as
	// "no slow requests".
	joined := logs.String()
	require.Contains(t, joined, "ops dashboard latency percentiles unavailable",
		"a silently emptied duration/ttft must be logged")
	require.Contains(t, joined, "level=WARN")
}

func TestQueryRawPartial_LatencyTimeoutIsLogged(t *testing.T) {
	repo, mock := newOpsDashboardRepo(t)
	start, end := dashboardWindow()

	mock.ExpectQuery(opsUsageCountsPattern).WillReturnRows(usageCountRows(10, 1000))
	mock.ExpectQuery(opsLatencyPattern).WillReturnError(context.DeadlineExceeded)
	mock.ExpectQuery(opsErrorCountsPattern).WillReturnRows(errorCountRows(0, 0, 0, 0, 0, 0))

	logs := captureLogs(t)

	got, err := repo.queryRawPartial(context.Background(), nil, start, end)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	require.Nil(t, got.duration.P99)
	require.Equal(t, int64(0), got.ttftSampleCount)

	joined := logs.String()
	require.Contains(t, joined, "ops dashboard latency percentiles unavailable")
	require.Contains(t, joined, "fragment contributes no duration/ttft")
}

func TestFillDegradedRates_LogsSubstitutedFields(t *testing.T) {
	logs := captureLogs(t)

	qpsCurrent, tpsCurrent, qpsPeak, tpsPeak := 0.0, 0.0, 0.0, 0.0
	fillDegradedRates(&qpsCurrent, &tpsCurrent, &qpsPeak, &tpsPeak, 2.5, 300.0, nil, time.Now(), time.Now())

	require.Equal(t, 2.5, qpsCurrent)
	require.Equal(t, 300.0, tpsCurrent)
	require.Equal(t, 2.5, qpsPeak)
	require.Equal(t, 300.0, tpsPeak)

	joined := logs.String()
	require.Contains(t, joined, "ops dashboard rate query degraded")
	require.Contains(t, joined, "qps.current")
	require.Contains(t, joined, "tps.peak")
	require.Contains(t, joined, "not measured peaks")
}

func TestFillDegradedRates_NoLogWhenNothingSubstituted(t *testing.T) {
	logs := captureLogs(t)

	qpsCurrent, tpsCurrent, qpsPeak, tpsPeak := 1.5, 200.0, 3.0, 400.0
	fillDegradedRates(&qpsCurrent, &tpsCurrent, &qpsPeak, &tpsPeak, 2.5, 300.0, nil, time.Now(), time.Now())

	require.Equal(t, 1.5, qpsCurrent, "measured values must not be overwritten")
	require.Equal(t, 3.0, qpsPeak)
	require.Empty(t, logs.String(), "no substitution means no warning")
}
