//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestOpsPreaggHourly_RecoveredRowsExcludedFromUpstreamNumerator 是缺陷2 的
// 数据库级证据：hourly 预聚合路径（preagg 仪表盘与 1h 窗口读的就是它）必须与
// raw 仪表盘给出同一个 upstream_error_count_excl_429_529。
//
// 背景：ops_error_logs 里的 recovered 行（logOpsRecoveredUpstream 为「上游失败
// 但最终成功」的请求记的 provider 归属错误）status_code 是 2xx、
// upstream_status_code 非 2xx。preagg 的分子谓词曾缺 `status_code >= 400` 门控，
// 把它们全算成上游错误——生产实测占该分子 54%（preagg 2252 vs raw 1036）。
//
// 同样的行插入后分别跑 raw 与 preagg 两条路径，断言两者的 excl 分子相等，且都只
// 计入真正失败的上游行。这是「同一指标同一屏不再差 2.17 倍」的直接验证。
func TestOpsPreaggHourly_RecoveredRowsExcludedFromUpstreamNumerator(t *testing.T) {
	ctx := context.Background()
	repo := &opsRepository{db: integrationDB}

	// 桶按整点对齐：UpsertHourlyMetrics 按 date_trunc('hour') 分桶，测试行的
	// created_at 必须落在同一个整点桶内，否则聚合会漏掉。
	bucketStart := time.Now().UTC().Truncate(time.Hour)
	start := bucketStart.Add(-time.Hour)
	end := bucketStart.Add(time.Hour)

	marker := "preaggrec" + uniqueMarkerSuffix()
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM ops_error_logs WHERE platform = $1`, marker)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM ops_metrics_hourly WHERE platform = $1`, marker)
	})

	insert := func(tag string, statusCode int, upstreamStatusCode *int, owner string, businessLimited bool) {
		t.Helper()
		_, err := integrationDB.ExecContext(ctx, `
INSERT INTO ops_error_logs (
  request_id, platform, error_phase, error_type, severity, status_code,
  is_business_limited, error_owner, upstream_status_code, is_count_tokens, created_at
) VALUES ($1, $2, 'upstream', 'upstream_error', 'P2', $3, $4, $5, $6, FALSE, $7)`,
			marker+"-"+tag, marker, statusCode, businessLimited, owner, upstreamStatusCode, bucketStart.Add(10*time.Minute))
		require.NoError(t, err, "insert %s", tag)
	}

	code500 := 500
	code429 := 429
	code529 := 529

	// 与 ops_repo_dashboard_integration_test.go 的行集完全一致，两条路径可直接对比。
	// 1) 真正的上游 5xx 失败（最终 502）：必须计入。
	for i := 0; i < 3; i++ {
		insert("r500-"+string(rune('a'+i)), 502, &code500, "provider", false)
	}
	// 2) recovered 行（最终 200，上游曾 500）：绝不能计入 —— 本测试的核心。
	for i := 0; i < 5; i++ {
		insert("rec-"+string(rune('a'+i)), 200, &code500, "provider", false)
	}
	// 3) 上游 429/529：只计入各自的分子，不进 excl。
	insert("r429", 429, &code429, "provider", false)
	insert("r529", 529, &code529, "provider", false)
	insert("rec429", 200, &code429, "provider", false)
	insert("rec529", 200, &code529, "provider", false)
	// 4) 客户端归属失败：不进任何上游分子。
	insert("client", 400, nil, "client", false)
	// 5) 业务限流：被所有上游分子排除。
	insert("bizlim", 429, &code429, "provider", true)

	// ── raw 路径（基准）───────────────────────────────────────────────────
	_, _, _, rawExcl, raw429, raw529, err := repo.queryErrorCounts(ctx,
		&service.OpsDashboardFilter{Platform: marker}, start, end)
	require.NoError(t, err)
	require.Equal(t, int64(3), rawExcl,
		"raw path: only the 3 genuine upstream 500s may count (recovered rows excluded)")

	// ── preagg 路径（被测）───────────────────────────────────────────────
	require.NoError(t, repo.UpsertHourlyMetrics(ctx, start, end))

	var preaggExcl, preagg429, preagg529 int64
	err = integrationDB.QueryRowContext(ctx, `
SELECT
  COALESCE(SUM(upstream_error_count_excl_429_529), 0),
  COALESCE(SUM(upstream_429_count), 0),
  COALESCE(SUM(upstream_529_count), 0)
FROM ops_metrics_hourly
WHERE platform = $1`, marker).Scan(&preaggExcl, &preagg429, &preagg529)
	require.NoError(t, err)

	require.Equal(t, int64(3), preaggExcl,
		"preagg path: recovered rows (final 2xx) must not be counted as upstream errors")
	require.Equal(t, rawExcl, preaggExcl,
		"preagg and raw must agree on upstream_error_count_excl_429_529 for identical rows")
	require.Equal(t, raw429, preagg429,
		"preagg and raw must agree on upstream_429 (recovered-after-429 rows stay counted in both)")
	require.Equal(t, raw529, preagg529,
		"preagg and raw must agree on upstream_529 (recovered-after-529 rows stay counted in both)")
}

// TestOpsPreaggDaily_TailPercentilesAreWeightedNotMax 用真实 PostgreSQL 验证 daily
// 聚合的 p95/p99 不再用 MAX 退化：小时桶各带不同样本数与分位值时，daily 必须给
// 出样本加权平均，而不是「最差那一小时」的值。
func TestOpsPreaggDaily_TailPercentilesAreWeightedNotMax(t *testing.T) {
	ctx := context.Background()
	repo := &opsRepository{db: integrationDB}

	day := time.Now().UTC().Truncate(24 * time.Hour)
	dayStart := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	start := dayStart.Add(-48 * time.Hour)
	end := dayStart.Add(24 * time.Hour)

	marker := "pctld" + uniqueMarkerSuffix()
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM ops_metrics_hourly WHERE platform = $1`, marker)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM ops_metrics_daily WHERE platform = $1`, marker)
	})

	// 两个小样本量小时桶 + 一个大样本量小时桶，全部落在同一天。
	// hour 00: p95=900, 100 samples
	// hour 01: p95=100, 100 samples
	// hour 02: p95=200, 1800 samples
	// 加权平均 = (900*100 + 100*100 + 200*1800) / 2000 = (90000+10000+360000)/2000 = 230
	// MAX 口径会给出 900 —— 相差近 4 倍，正是本次要消除的跨层漂移。
	hours := []struct {
		offset  time.Duration
		p95     int
		samples int
	}{
		{0, 900, 100},
		{1 * time.Hour, 100, 100},
		{2 * time.Hour, 200, 1800},
	}
	for i, h := range hours {
		bucketStart := dayStart.Add(h.offset)
		_, err := integrationDB.ExecContext(ctx, `
INSERT INTO ops_metrics_hourly (
  bucket_start, platform, group_id, success_count, ttft_sample_count,
  duration_p95_ms, duration_p99_ms, computed_at
) VALUES ($1, $2, NULL, $3, 0, $4, $5, NOW())
ON CONFLICT (bucket_start, COALESCE(platform, ''), COALESCE(group_id, 0)) DO UPDATE SET
  success_count = EXCLUDED.success_count,
  duration_p95_ms = EXCLUDED.duration_p95_ms,
  duration_p99_ms = EXCLUDED.duration_p99_ms,
  computed_at = NOW()`,
			bucketStart, marker, h.samples, h.p95, h.p95*2)
		require.NoError(t, err, "insert hourly bucket %d", i)
	}

	require.NoError(t, repo.UpsertDailyMetrics(ctx, start, end))

	var dailyP95 int
	err := integrationDB.QueryRowContext(ctx, `
SELECT duration_p95_ms
FROM ops_metrics_daily
WHERE platform = $1 AND bucket_date = $2`, marker, dayStart.Format("2006-01-02")).Scan(&dailyP95)
	require.NoError(t, err)

	require.Equal(t, 230, dailyP95,
		"daily p95 must be the sample-weighted average (230); MAX-of-segments would report 900")
}
