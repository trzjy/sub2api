//go:build integration

package repository

import (
	"context"
	"strconv"
	"testing"
	"time"

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
