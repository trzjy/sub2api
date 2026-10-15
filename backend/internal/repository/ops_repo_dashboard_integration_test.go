//go:build integration

package repository

import (
	"context"
	"strconv"
	"testing"
	"time"

	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestOpsErrorCounts_RecoveredRowsExcludedFromUpstreamNumerator exercises the
// remediation-1 predicate against a real PostgreSQL instance.
//
// sqlmock can only assert the SQL text; this test inserts the two row shapes
// ops_error_logs actually holds for provider-owned errors and checks the
// numbers the dashboard would display:
//
//   - genuine upstream failures (final status >= 400) -> counted
//   - recovered rows (final status 2xx, upstream attempt failed) -> NOT counted
//
// It is the regression guard for the production symptom where recovered rows
// made up 54% of upstream_excl (2252 measured vs the correct 1036).
//
// Isolation: rows go to integrationDB (opsRepository holds a *sql.DB, not a Tx)
// and are scoped by a unique lowercase platform marker, because buildErrorWhere
// matches platform exactly after lower-casing the filter value.
func TestOpsErrorCounts_RecoveredRowsExcludedFromUpstreamNumerator(t *testing.T) {
	ctx := context.Background()
	repo := &opsRepository{db: integrationDB}

	now := time.Now().UTC()
	start := now.Add(-1 * time.Hour)
	end := now.Add(1 * time.Hour)

	// request_id is varchar(64), so keep the marker short: a unique suffix
	// scopes the rows without depending on the (long) test name.
	marker := "recgate" + uniqueMarkerSuffix()
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM ops_error_logs WHERE platform = $1`, marker)
	})

	insert := func(tag string, statusCode int, upstreamStatusCode *int, owner string, businessLimited bool) {
		t.Helper()
		_, err := integrationDB.ExecContext(ctx, `
INSERT INTO ops_error_logs (
  request_id, platform, error_phase, error_type, severity, status_code,
  is_business_limited, error_owner, upstream_status_code, is_count_tokens, created_at
) VALUES ($1, $2, 'upstream', 'upstream_error', 'P2', $3, $4, $5, $6, FALSE, $7)`,
			marker+"-"+tag, marker, statusCode, businessLimited, owner, upstreamStatusCode, now)
		require.NoError(t, err, "insert %s", tag)
	}

	code500 := 500
	code429 := 429
	code529 := 529

	// 1) Genuine upstream 5xx failures: request failed (502), upstream said 500.
	//    These MUST still be counted.
	for i := 0; i < 3; i++ {
		insert("r500-"+string(rune('a'+i)), 502, &code500, "provider", false)
	}
	// 2) Recovered rows: request ultimately SUCCEEDED (200) after an upstream
	//    500 was recorded. These MUST NOT be counted.
	for i := 0; i < 5; i++ {
		insert("rec-"+string(rune('a'+i)), 200, &code500, "provider", false)
	}
	// 3) Upstream 429 -> counted only in upstream_429, never in upstream_excl.
	insert("r429", 429, &code429, "provider", false)
	// 4) Upstream 529 -> counted only in upstream_529.
	insert("r529", 529, &code529, "provider", false)
	// 5) Recovered rows whose upstream attempt was 429/529: a success is a
	//    success regardless of which upstream status preceded it.
	insert("rec429", 200, &code429, "provider", false)
	insert("rec529", 200, &code529, "provider", false)
	// 6) Client-owned failure: must not appear in any upstream numerator.
	insert("client", 400, nil, "client", false)
	// 7) Business-limited rows are excluded from upstream numerators.
	insert("bizlim", 429, &code429, "provider", true)

	errorTotal, businessLimited, errorSLA, upstreamExcl, upstream429, upstream529, err :=
		repo.queryErrorCounts(ctx, &service.OpsDashboardFilter{Platform: marker}, start, end)
	require.NoError(t, err)

	// error_total counts every failed request (status >= 400):
	// 3 real 500s + 1 real 429 + 1 real 529 + 1 client 400 + 1 business-limited = 7
	require.Equal(t, int64(7), errorTotal, "error_total must count all failed requests")
	require.Equal(t, int64(1), businessLimited, "business_limited must be counted")
	require.Equal(t, int64(6), errorSLA, "error_sla excludes the business-limited row")

	// upstream_excl: only the 3 genuine upstream 500 failures. All 5 recovered
	// rows must NOT appear; this is the whole point of the fix.
	require.Equal(t, int64(3), upstreamExcl,
		"recovered rows (final 2xx) must not be counted as upstream errors")

	// upstream_429/upstream_529 keep their PRE-EXISTING semantics: they count
	// provider-owned rows whose upstream status was 429/529 regardless of the
	// final client status. So each picks up 2 rows here (the genuinely failed
	// one plus the recovered one whose upstream attempt was 429/529).
	//
	// This asymmetry is deliberate and NOT changed by remediation 1, which was
	// scoped to the upstream_excl numerator (the value the SLA/upstream error
	// rate is built from). See the report: aligning 429/529 with upstream_excl
	// would need a separate decision, because a recovered-after-429 row is a
	// genuine upstream throttle event and arguably belongs in that counter.
	require.Equal(t, int64(2), upstream429,
		"upstream_429 keeps counting recovered-after-429 rows (unchanged semantics)")
	require.Equal(t, int64(2), upstream529,
		"upstream_529 keeps counting recovered-after-529 rows (unchanged semantics)")
}

// TestOpsErrorCounts_RecoveredRowsAreNotErrorsAtAll makes the contrast explicit:
// recovered rows never enter error_total either, because the request succeeded.
func TestOpsErrorCounts_RecoveredRowsAreNotErrorsAtAll(t *testing.T) {
	ctx := context.Background()
	repo := &opsRepository{db: integrationDB}

	now := time.Now().UTC()
	start := now.Add(-1 * time.Hour)
	end := now.Add(1 * time.Hour)

	marker := "reconly" + uniqueMarkerSuffix()
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM ops_error_logs WHERE platform = $1`, marker)
	})

	code500 := 500
	for i := 0; i < 4; i++ {
		_, err := integrationDB.ExecContext(ctx, `
INSERT INTO ops_error_logs (
  request_id, platform, error_phase, error_type, severity, status_code,
  is_business_limited, error_owner, upstream_status_code, is_count_tokens, created_at
) VALUES ($1, $2, 'upstream', 'upstream_error', 'P2', 200, FALSE, 'provider', $3, FALSE, $4)`,
			marker+"-"+string(rune('a'+i)), marker, code500, now)
		require.NoError(t, err)
	}

	errorTotal, _, _, upstreamExcl, _, _, err := repo.queryErrorCounts(ctx,
		&service.OpsDashboardFilter{Platform: marker}, start, end)
	require.NoError(t, err)

	require.Equal(t, int64(0), errorTotal,
		"recovered rows have a final 2xx status and are not errors")
	require.Equal(t, int64(0), upstreamExcl,
		"and they must not be counted as upstream errors either")
}

// uniqueMarkerSuffix returns a short lowercase suffix so concurrently-created
// rows in the shared integration database never collide across runs.
func uniqueMarkerSuffix() string {
	return strconv.FormatInt(time.Now().UnixNano()%1_000_000_000, 36)
}

func TestOpsOutputTPSDistribution(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	user := mustCreateUser(t, client, &service.User{})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID})
	account := mustCreateAccount(t, client, &service.Account{Name: "output-tps", Platform: service.PlatformAnthropic})
	group := mustCreateGroup(t, client, &service.Group{Name: "output-tps", Platform: service.PlatformOpenAI})
	otherGroup := mustCreateGroup(t, client, &service.Group{Name: "other-output-tps", Platform: service.PlatformOpenAI})
	start := time.Date(2021, 1, 17, 0, 30, 0, 0, time.UTC)
	end := start.Add(6 * time.Hour)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM usage_logs WHERE user_id = $1", user.ID)
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM api_keys WHERE id = $1", key.ID)
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM accounts WHERE id = $1", account.ID)
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM ops_metrics_hourly WHERE bucket_start >= $1 AND bucket_start < $2", start.Truncate(time.Hour), end)
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM groups WHERE id IN ($1, $2)", group.ID, otherGroup.ID)
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM users WHERE id = $1", user.ID)
	})
	insert := func(at time.Time, groupID *int64, output int, duration *int, requestType service.RequestType, imageCount, imageTokens int, billingMode string) {
		t.Helper()
		_, err := integrationDB.ExecContext(ctx, `INSERT INTO usage_logs
   (user_id, api_key_id, account_id, group_id, request_id, model, input_tokens, output_tokens, duration_ms, request_type, image_count, image_output_tokens, billing_mode, created_at, image_size)
   VALUES ($1,$2,$3,$4,$5,'gpt-test',100000,$6,$7,$8,$9,$10,$11,$12,CASE WHEN $9 > 0 THEN '1K' ELSE NULL END)`,
			user.ID, key.ID, account.ID, groupID, fmt.Sprintf("output-tps-%d-%d", user.ID, time.Now().UnixNano()), output, duration, requestType, imageCount, imageTokens, billingMode, at)
		require.NoError(t, err)
	}
	// Rates 1, 10, 100, 1000: unequal durations catch ratio-of-sums mistakes.
	// Put samples in the raw head, stable hourly buckets, and raw tail.
	for _, sample := range []struct {
		output, duration int
		kind             service.RequestType
		at               time.Time
	}{
		{1, 1000, service.RequestTypeSync, start},
		{20, 2000, service.RequestTypeStream, start.Add(time.Hour)},
		{100, 1000, service.RequestTypeWSV2, start.Add(2 * time.Hour)},
		{500, 500, service.RequestTypeCyberBlocked, end.Add(-time.Minute)},
	} {
		insert(sample.at, &group.ID, sample.output, &sample.duration, sample.kind, 0, 0, "token")
	}
	positive, zero, negative := 1000, 0, -1
	for _, d := range []*int{nil, &zero, &negative} {
		insert(start, &group.ID, 9999, d, service.RequestTypeSync, 0, 0, "token")
	}
	for _, n := range []int{0, -1} {
		insert(start, &group.ID, n, &positive, service.RequestTypeSync, 0, 0, "token")
	}
	for _, kind := range []service.RequestType{service.RequestTypeLive, service.RequestTypeUnknown} {
		insert(start, &group.ID, 9999, &positive, kind, 0, 0, "token")
	}
	insert(start, &group.ID, 9999, &positive, service.RequestTypeSync, 1, 0, "token")
	insert(start, &group.ID, 9999, &positive, service.RequestTypeSync, 0, 1, "token")
	insert(start, &group.ID, 9999, &positive, service.RequestTypeSync, 0, 0, "image")
	insert(start.Add(-time.Second), &group.ID, 9999, &positive, service.RequestTypeSync, 0, 0, "token")
	insert(end, &group.ID, 9999, &positive, service.RequestTypeSync, 0, 0, "token")
	insert(start, &otherGroup.ID, 9999, &positive, service.RequestTypeSync, 0, 0, "token")
	insert(start, nil, 50, &positive, service.RequestTypeSync, 0, 0, "token")
	repo := NewOpsRepository(integrationDB).(*opsRepository)
	filter := &service.OpsDashboardFilter{StartTime: start, EndTime: end, Platform: " OpenAI ", GroupID: &group.ID}
	assertStats := func(stats *service.OpsOutputTPS) {
		t.Helper()
		require.NotNil(t, stats)
		require.EqualValues(t, 4, stats.SampleCount)
		require.NotNil(t, stats.P5)
		require.NotNil(t, stats.P10)
		require.NotNil(t, stats.P50)
		require.NotNil(t, stats.Avg)
		require.InDelta(t, 2.35, *stats.P5, 1e-9)
		require.InDelta(t, 3.7, *stats.P10, 1e-9)
		require.InDelta(t, 55, *stats.P50, 1e-9)
		require.InDelta(t, 277.75, *stats.Avg, 1e-9)
	}
	// Auto first exercises fallback before hourly rows exist.
	for _, mode := range []service.OpsQueryMode{service.OpsQueryModeRaw, service.OpsQueryModeAuto} {
		filter.QueryMode = mode
		overview, err := repo.GetDashboardOverview(ctx, filter)
		require.NoError(t, err)
		assertStats(overview.OutputTPS)
	}
	require.NoError(t, repo.UpsertHourlyMetrics(ctx, start.Truncate(time.Hour), end))
	for _, mode := range []service.OpsQueryMode{service.OpsQueryModePreagg, service.OpsQueryModeAuto} {
		filter.QueryMode = mode
		overview, err := repo.GetDashboardOverview(ctx, filter)
		require.NoError(t, err)
		assertStats(overview.OutputTPS)
	}
	filter.Platform = service.PlatformAnthropic
	empty, err := repo.queryOutputTPS(ctx, filter)
	require.NoError(t, err)
	require.Equal(t, &service.OpsOutputTPS{}, empty)
	// Ungrouped logs use the account platform; group platform takes precedence.
	filter.GroupID = nil
	fallback, err := repo.queryOutputTPS(ctx, filter)
	require.NoError(t, err)
	require.EqualValues(t, 1, fallback.SampleCount)
	require.Equal(t, 50.0, *fallback.P50)
}

func TestOpsOutputTPSQueryDeadline(t *testing.T) {
	tx := testTx(t)
	_, err := tx.ExecContext(context.Background(), "LOCK TABLE usage_logs IN ACCESS EXCLUSIVE MODE")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	repo := NewOpsRepository(integrationDB).(*opsRepository)
	now := time.Now().UTC()
	stats, err := repo.queryOutputTPS(ctx, &service.OpsDashboardFilter{StartTime: now.Add(-time.Hour), EndTime: now})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, stats)
}
