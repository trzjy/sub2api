package repository

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestChannelMonitorV2DateBinOriginIsUTC(t *testing.T) {
	require.Equal(t, "TIMESTAMPTZ '1970-01-01 00:00:00+00'", channelMonitorV2DateBinOrigin)
	require.Equal(t, "date_bin($1::interval,m.bucket_start,TIMESTAMPTZ '1970-01-01 00:00:00+00')", channelMonitorV2DateBinExpr("m.bucket_start"))

	for _, query := range []string{
		channelMonitorV2FixedRollupBoundsSQL,
		channelMonitorV2MetricsRollupSQL,
		channelMonitorV2UserMetricsRollupSQL,
		channelMonitorV2HistogramRollupSQL,
		channelMonitorV2ErrorRollupSQL,
	} {
		require.Contains(t, query, channelMonitorV2DateBinOrigin)
		require.NotContains(t, query, "TIMESTAMPTZ '1970-01-01'")
	}
}

func TestChannelMonitorV2DisplayModelIsPlatformScoped(t *testing.T) {
	// C1: channelMonitorV2DisplayModel 已停消费 cfg.Platforms[].models（见 channel_monitor_v2_repo.go）。
	// named + __other__ 折叠随模型清单归零而退役；此处始终返回真实模型名，仅空模型名回落 __other__。
	// 展示折叠/目录推导归属 C2/C3。
	cfg := service.ChannelMonitorV2Config{Platforms: []service.ChannelMonitorV2PlatformConfig{
		{Platform: "openai", Enabled: true, Models: []string{"shared", "gpt-5"}},
		{Platform: "grok", Enabled: true, Models: []string{"grok-4"}},
		{Platform: "anthropic", Enabled: true, Models: []string{}},
	}}
	require.Equal(t, "shared", channelMonitorV2DisplayModel(cfg, "openai", "shared"))
	// 未列入模型清单的模型仍返回真实名（不再折叠为 __other__）。
	require.Equal(t, "shared", channelMonitorV2DisplayModel(cfg, "grok", "shared"))
	require.Equal(t, "claude-sonnet-4", channelMonitorV2DisplayModel(cfg, "anthropic", "claude-sonnet-4"))
	// 未配置平台仍返回真实模型名。
	require.Equal(t, "gemini-2.5-pro", channelMonitorV2DisplayModel(cfg, "gemini", "gemini-2.5-pro"))
	// 空模型名回落 __other__。
	require.Equal(t, service.ChannelMonitorV2OtherModel, channelMonitorV2DisplayModel(cfg, "openai", ""))
	// 模型选择不再匹配 __other__：展示模型为真实名，与 filter.Models=[__other__] 不符。
	require.False(t, channelMonitorV2ModelSelected(service.ChannelMonitorV2Filter{Models: []string{service.ChannelMonitorV2OtherModel}}, cfg, "grok", "shared"))
	// 选择真实模型名可命中。
	require.True(t, channelMonitorV2ModelSelected(service.ChannelMonitorV2Filter{Models: []string{"shared"}}, cfg, "grok", "shared"))
}

func TestChannelMonitorV2MatrixDimensionKey(t *testing.T) {
	cfg := service.ChannelMonitorV2Config{Platforms: []service.ChannelMonitorV2PlatformConfig{{Platform: "openai", Enabled: true, Models: []string{"gpt-5"}}}}
	key := channelMonitorV2MatrixDimensionKey(service.ChannelMonitorV2GroupByPlatformGroupModel, cfg, "openai", 7, "gpt-5")
	require.Equal(t, channelMonitorV2MatrixKey{platform: "openai", groupID: 7, model: "gpt-5"}, key)
	// C1: 未列入模型清单的模型不再折叠为 __other__，保留真实模型名。
	key = channelMonitorV2MatrixDimensionKey(service.ChannelMonitorV2GroupByPlatformModel, cfg, "openai", 7, "unlisted")
	require.Equal(t, channelMonitorV2MatrixKey{platform: "openai", model: "unlisted"}, key)
	key = channelMonitorV2MatrixDimensionKey(service.ChannelMonitorV2GroupByPlatform, cfg, "openai", 7, "gpt-5")
	require.Equal(t, channelMonitorV2MatrixKey{platform: "openai"}, key)
}

func TestChannelMonitorV2HistogramPercentilesAreMergedFromCounts(t *testing.T) {
	// 100 samples: 50@100, 40@500, 10@1000
	// target = int64(total*p + 0.999999) truncates: p50→50, p90→90, p95→95
	// cumulative hits: p50@100, p90@500 (50+40), p95@1000
	histogram := map[int64]int64{100: 50, 500: 40, 1000: 10}
	require.Equal(t, int64(100), *histPercentile(histogram, .5))
	require.Equal(t, int64(500), *histPercentile(histogram, .9))
	require.Equal(t, int64(1000), *histPercentile(histogram, .95))
	require.Nil(t, histPercentile(nil, .95))
	// latencyMetric exposes avg + p50 + p90 + p95
	lat := latencyMetric(1000, 10, histogram)
	require.NotNil(t, lat.AvgMs)
	require.NotNil(t, lat.P50Ms)
	require.NotNil(t, lat.P90Ms)
	require.NotNil(t, lat.P95Ms)
	require.Equal(t, int64(100), *lat.P50Ms)
	require.Equal(t, int64(500), *lat.P90Ms)
	require.Equal(t, int64(1000), *lat.P95Ms)
}

func TestChannelMonitorV2MetricIncludesSuccessRate(t *testing.T) {
	acc := newMetricAccumulator()
	acc.success, acc.errors = 80, 20
	metric := acc.metric(1, false)
	require.Equal(t, int64(100), metric.RequestCount)
	require.InDelta(t, 0.8, metric.SuccessRate, 0.0001)
	require.InDelta(t, 0.2, metric.ErrorRate, 0.0001)
	require.Nil(t, metric.UpstreamAffectedRequests)

	adminMetric := acc.metric(1, true)
	require.NotNil(t, adminMetric.UpstreamAffectedRequests)
}

func TestChannelMonitorV2WhereUsesConfiguredScopeAndEmptyFilterMeansAllConfigured(t *testing.T) {
	filter := service.ChannelMonitorV2Filter{Start: time.Unix(1, 0), End: time.Unix(2, 0)}
	cfg := service.ChannelMonitorV2Config{
		Platforms: []service.ChannelMonitorV2PlatformConfig{{Platform: "openai", Enabled: true}, {Platform: "grok", Enabled: false}},
		GroupIDs:  []int64{3, 4},
	}
	where, args := channelMonitorV2Where(filter, cfg, "m")
	require.Contains(t, where, "m.platform = ANY($3)")
	require.Contains(t, where, "m.group_id = ANY($4)")
	require.Len(t, args, 4)
}

func TestChannelMonitorV2WhereRejectsGroupFilterOutsideConfiguredScope(t *testing.T) {
	filter := service.ChannelMonitorV2Filter{
		Start: time.Unix(1, 0), End: time.Unix(2, 0), GroupIDs: []int64{9},
	}
	cfg := service.ChannelMonitorV2Config{
		Platforms: []service.ChannelMonitorV2PlatformConfig{{Platform: "openai", Enabled: true}},
		GroupIDs:  []int64{3, 4},
	}
	where, args := channelMonitorV2Where(filter, cfg, "m")
	require.Contains(t, where, "FALSE")
	require.NotContains(t, where, "m.group_id = ANY")
	require.Len(t, args, 3)
}

func TestChannelMonitorV2WhereRestrictsOrdinaryViewerToAllowedConfiguredGroups(t *testing.T) {
	filter := service.ChannelMonitorV2Filter{
		Start: time.Unix(1, 0), End: time.Unix(2, 0),
		GroupIDs: []int64{4, 9}, AllowedGroupIDs: []int64{3, 4}, RestrictGroups: true,
	}
	cfg := service.ChannelMonitorV2Config{
		Platforms: []service.ChannelMonitorV2PlatformConfig{{Platform: "openai", Enabled: true}},
		GroupIDs:  []int64{3, 4, 9},
	}
	where, args := channelMonitorV2Where(filter, cfg, "m")
	require.Contains(t, where, "m.group_id = ANY($4)")
	require.Equal(t, pq.Array([]int64{4}), args[3])
}

func TestChannelMonitorV2WhereRejectsOrdinaryViewerWithNoAllowedGroups(t *testing.T) {
	filter := service.ChannelMonitorV2Filter{
		Start: time.Unix(1, 0), End: time.Unix(2, 0),
		GroupIDs: []int64{9}, RestrictGroups: true,
	}
	cfg := service.ChannelMonitorV2Config{
		Platforms: []service.ChannelMonitorV2PlatformConfig{{Platform: "openai", Enabled: true}},
		GroupIDs:  []int64{3, 9},
	}
	where, _ := channelMonitorV2Where(filter, cfg, "m")
	require.Contains(t, where, "FALSE")
	require.NotContains(t, where, "m.group_id = ANY")
}

func TestChannelMonitorV2CatalogKeepsViewerScopeWhileIgnoringPickerFilters(t *testing.T) {
	filter := service.ChannelMonitorV2Filter{
		Platforms: []string{"openai"}, GroupIDs: []int64{9}, Models: []string{"gpt-5"},
		AllowedGroupIDs: []int64{3}, RestrictGroups: true,
	}
	catalog := channelMonitorV2CatalogFilter(filter)
	require.Empty(t, catalog.Platforms)
	require.Empty(t, catalog.GroupIDs)
	require.Empty(t, catalog.Models)
	require.True(t, catalog.RestrictGroups)
	require.Equal(t, []int64{3}, catalog.AllowedGroupIDs)
}

func TestChannelMonitorV2AdminScopeRemainsGlobal(t *testing.T) {
	filter := service.ChannelMonitorV2Filter{Start: time.Unix(1, 0), End: time.Unix(2, 0), GroupIDs: []int64{9}}
	cfg := service.ChannelMonitorV2Config{
		Platforms: []service.ChannelMonitorV2PlatformConfig{{Platform: "openai", Enabled: true}},
		GroupIDs:  []int64{3, 9},
	}
	where, args := channelMonitorV2Where(filter, cfg, "m")
	require.Contains(t, where, "m.group_id = ANY($4)")
	require.Equal(t, pq.Array([]int64{9}), args[3])
}

func TestChannelMonitorV2MatrixDoesNotSeedGroupsForEmptyViewerScope(t *testing.T) {
	filter := service.ChannelMonitorV2Filter{RestrictGroups: true}
	cfg := service.ChannelMonitorV2Config{
		Platforms: []service.ChannelMonitorV2PlatformConfig{{Platform: "openai", Enabled: true}},
		GroupIDs:  []int64{3, 9},
	}
	accs := seedChannelMonitorV2MatrixAccumulators(filter, cfg, service.ChannelMonitorV2GroupByPlatformGroup, map[int64]channelMonitorV2GroupInfo{
		3: {name: "private"}, 9: {name: "other-private"},
	})
	require.Empty(t, accs)
}

func TestChannelMonitorV2EmptyRestrictedScopeReturnsEmptyInventory(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := &channelMonitorV2Repository{db: db}
	filter := service.ChannelMonitorV2Filter{RestrictGroups: true}
	cfg := service.ChannelMonitorV2Config{
		Platforms: []service.ChannelMonitorV2PlatformConfig{{Platform: "openai", Enabled: true, Models: []string{"gpt-5"}}},
		GroupIDs:  []int64{3, 9},
	}

	dimensions, err := repo.GetDimensions(context.Background(), filter, cfg)
	require.NoError(t, err)
	require.Empty(t, dimensions.Platforms)
	require.Empty(t, dimensions.Groups)
	require.Empty(t, dimensions.Models)
	require.NotNil(t, dimensions.Platforms)
	require.NotNil(t, dimensions.Groups)
	require.NotNil(t, dimensions.Models)

	models, err := repo.GetModels(context.Background(), filter, cfg, false)
	require.NoError(t, err)
	require.Empty(t, models.Items)
	require.NotNil(t, models.Items)
	require.Equal(t, service.ChannelMonitorV2Coverage{}, models.Coverage)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestChannelMonitorV2EmptyRestrictedScopeReturnsEmptySnapshotWithoutQueries(t *testing.T) {
	tests := []struct {
		name   string
		filter service.ChannelMonitorV2Filter
	}{
		{
			name:   "no allowed groups",
			filter: service.ChannelMonitorV2Filter{RestrictGroups: true},
		},
		{
			name: "requested groups exclude allowed configured scope",
			filter: service.ChannelMonitorV2Filter{
				GroupIDs: []int64{9}, AllowedGroupIDs: []int64{3}, RestrictGroups: true,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			repo := &channelMonitorV2Repository{db: db}
			cfg := service.ChannelMonitorV2Config{
				Enabled: true,
				Platforms: []service.ChannelMonitorV2PlatformConfig{{
					Platform: "openai", Enabled: true, Models: []string{"gpt-5"},
				}},
				GroupIDs: []int64{3},
			}

			snapshot, err := repo.GetSnapshot(context.Background(), test.filter, cfg, false)
			require.NoError(t, err)
			require.Equal(t, service.ChannelMonitorV2Config{}, snapshot.Config)
			require.Equal(t, service.ChannelMonitorV2Coverage{}, snapshot.Coverage)
			require.Equal(t, service.ChannelMonitorV2Metric{}, snapshot.Metrics)
			require.Equal(t, service.ChannelMonitorV2Health{}, snapshot.Health)
			require.Empty(t, snapshot.Trend)
			require.NotNil(t, snapshot.Trend)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestChannelMonitorV2EmptyRestrictedScopeReturnsEmptyMatrixForEveryGrouping(t *testing.T) {
	groupings := []service.ChannelMonitorV2GroupBy{
		service.ChannelMonitorV2GroupByPlatform,
		service.ChannelMonitorV2GroupByPlatformModel,
		service.ChannelMonitorV2GroupByPlatformGroup,
		service.ChannelMonitorV2GroupByPlatformGroupModel,
	}
	for _, groupBy := range groupings {
		t.Run(string(groupBy), func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			repo := &channelMonitorV2Repository{db: db}
			filter := service.ChannelMonitorV2Filter{RestrictGroups: true}
			cfg := service.ChannelMonitorV2Config{
				Platforms: []service.ChannelMonitorV2PlatformConfig{{Platform: "openai", Enabled: true, Models: []string{"gpt-5"}}},
				GroupIDs:  []int64{3, 9},
			}

			matrix, err := repo.GetMatrix(context.Background(), filter, cfg, groupBy, false)
			require.NoError(t, err)
			require.Equal(t, groupBy, matrix.GroupBy)
			require.Empty(t, matrix.Items)
			require.NotNil(t, matrix.Items)
			require.Equal(t, service.ChannelMonitorV2Coverage{}, matrix.Coverage)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestChannelMonitorV2ErrorAggregationCountsFinalUserErrorsOnly(t *testing.T) {
	query := strings.ToLower(channelMonitorV2ErrorAggregationSQL)
	require.Contains(t, query, "not current_error.is_count_tokens")
	require.Contains(t, query, "error_type = 'cyber_policy'")
	require.Contains(t, query, "distinct on")
	require.Contains(t, query, "candidate_ids")
	require.Contains(t, query, "where bucket_start >= $1 and bucket_start < $2")
	require.Contains(t, query, "upstream_affected_requests")
	require.Contains(t, query, "jsonb_array_length(current_error.upstream_errors) > 0")
	// request_id dedup must be time-bounded (no full-history scan).
	require.Contains(t, query, "interval '90 minutes'")
	require.Contains(t, query, "current_error.created_at >= $1 - interval '90 minutes'")
}

func TestChannelMonitorV2ErrorAggregationResolvesCompositePlatform(t *testing.T) {
	query := strings.ToLower(channelMonitorV2ErrorAggregationSQL)
	// Composite groups are a routing layer: error facts must resolve the concrete
	// account platform (joining groups/accounts) so they aggregate under the same
	// platform key as usage facts instead of the never-enabled 'composite' platform.
	require.Contains(t, query, "g.platform = 'composite'")
	require.Contains(t, query, "left join groups g on g.id = current_error.group_id")
	require.Contains(t, query, "left join accounts a on a.id = current_error.account_id")
	require.Contains(t, query, "a.platform")
	require.Contains(t, query, "nullif(trim(a.platform), '')")
	require.NotContains(t, query, "nullif(trim(a.platform))")
}

func TestChannelMonitorV2UsageSuccessExcludesCyberBillingRows(t *testing.T) {
	for _, query := range []string{channelMonitorV2UsageMetricsSQL, channelMonitorV2UserMetricsSQL} {
		require.Contains(t, query, "COALESCE(ul.request_type, 0) NOT IN (4, 6)")
		require.Contains(t, query, "ul.actual_cost > 0")
	}
	require.Contains(t, channelMonitorV2PlatformSQL, "g.platform = 'composite'")
	require.Contains(t, channelMonitorV2PlatformSQL, "a.platform")
	require.Contains(t, channelMonitorV2HistogramSQL, "ul.actual_cost > 0")
}

func TestChannelMonitorV2RatesUseCoveredWindow(t *testing.T) {
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	filter := service.ChannelMonitorV2Filter{Start: start, End: start.Add(24 * time.Hour)}
	coverage := service.ChannelMonitorV2Coverage{CoverageStart: start.Add(6 * time.Hour), DataThrough: start.Add(18 * time.Hour)}
	require.Equal(t, 12*60.0, channelMonitorV2CoveredMinutes(filter, coverage))
	effective := channelMonitorV2CommonCoverageFilter(filter, coverage)
	require.Equal(t, coverage.CoverageStart, effective.Start)
	require.Equal(t, coverage.DataThrough, effective.End)
}

func TestChannelMonitorV2HistoryCoverageCompleteIgnoresTrailingLag(t *testing.T) {
	start := time.Date(2026, 8, 7, 2, 20, 0, 0, time.UTC)
	// History reaches the window start → complete even if data_through is behind filter.End.
	require.True(t, channelMonitorV2HistoryCoverageComplete(start, start))
	require.True(t, channelMonitorV2HistoryCoverageComplete(start.Add(-time.Hour), start))
	// Backfill still short of the window start → incomplete.
	require.False(t, channelMonitorV2HistoryCoverageComplete(start.Add(time.Hour), start))
	require.False(t, channelMonitorV2HistoryCoverageComplete(time.Time{}, start))
}

func TestChannelMonitorV2TierRetentionPolicy(t *testing.T) {
	require.Equal(t, 3*24*time.Hour, channelMonitorV2RetentionUser1m)
	require.Equal(t, 7*24*time.Hour, channelMonitorV2RetentionMetrics1m)
	require.Equal(t, 7*24*time.Hour, channelMonitorV2RetentionError1m)
	require.Equal(t, 7*24*time.Hour, channelMonitorV2RetentionHistogram1m)
	require.Equal(t, 7*24*time.Hour, channelMonitorV2RetentionRollup5m)
	require.Equal(t, 30*24*time.Hour, channelMonitorV2RetentionRollup1h)
	require.Equal(t, 45*24*time.Hour, channelMonitorV2RetentionRollup12h)
	require.Equal(t, 90*24*time.Hour, channelMonitorV2RetentionRollup1d)
	require.Equal(t, channelMonitorV2RetentionRollup1d, channelMonitorV2MaxRetention())
	require.Contains(t, channelMonitorV2WatermarkSQL, "INTERVAL '90 days'")

	// Every fixed rollup second must appear with a retention rule.
	wantSeconds := map[int]time.Duration{
		300:   channelMonitorV2RetentionRollup5m,
		3600:  channelMonitorV2RetentionRollup1h,
		43200: channelMonitorV2RetentionRollup12h,
		86400: channelMonitorV2RetentionRollup1d,
	}
	seen := map[int]time.Duration{}
	for _, rule := range channelMonitorV2RetentionRules {
		if rule.bucketSeconds == 0 {
			require.True(t, rule.retention > 0)
			continue
		}
		if prev, ok := seen[rule.bucketSeconds]; ok {
			require.Equal(t, prev, rule.retention)
		}
		seen[rule.bucketSeconds] = rule.retention
	}
	for seconds, want := range wantSeconds {
		got, ok := seen[seconds]
		require.Truef(t, ok, "missing retention rule for bucket_seconds=%d", seconds)
		require.Equal(t, want, got)
	}

	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	require.Equal(t, now.Add(-7*24*time.Hour), channelMonitorV2RetentionCutoff(now, channelMonitorV2RetentionMetrics1m))
	require.Equal(t, now.Add(-90*24*time.Hour), channelMonitorV2RetentionCutoff(now, channelMonitorV2MaxRetention()))
}

func TestSameFixedRollupBucket(t *testing.T) {
	start := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	require.True(t, sameFixedRollupBucket(start, start.Add(10*time.Minute), 86400))
	require.False(t, sameFixedRollupBucket(start, start.Add(15*time.Hour), 43200))
	require.False(t, sameFixedRollupBucket(start, start.Add(24*time.Hour), 86400))
}

// Needles present in service.ClassifyChannelMonitorV2Error must appear in the
// aggregation SQL CASE so rollup categories match drilldown classification.
func TestChannelMonitorV2SQLTaxonomyContainsGoNeedles(t *testing.T) {
	sql := channelMonitorV2ErrorAggregationSQL
	needles := []string{
		"blocked keyword",
		"invalid_api_key",
		"max_tokens",
		"invalid_request",
		"model not supported",
		"billing hard limit",
		"no healthy upstream account",
		"rate_limit",
		"gateway timeout",
		"connection refused",
		"unexpected eof",
	}
	for _, needle := range needles {
		require.Containsf(t, strings.ToLower(sql), strings.ToLower(needle), "SQL taxonomy missing Go needle %q", needle)
	}
}

func TestApplyIgnoredErrorsAdjustsRatesKeepsAbsoluteVolume(t *testing.T) {
	m := service.ChannelMonitorV2Metric{
		RequestCount:  100,
		ErrorRequests: 20,
		ErrorRate:     0.20,
		SuccessRate:   0.80,
	}
	// Success absolute still 80 → success rate stays 0.80 even after ignoring 5 errors.
	m.SuccessRequests = 80
	applyIgnoredErrors(&m, 5)
	require.Equal(t, int64(100), m.RequestCount)
	require.Equal(t, int64(20), m.ErrorRequests)
	require.InDelta(t, 0.15, m.ErrorRate, 0.0001)
	require.InDelta(t, 0.80, m.SuccessRate, 0.0001)

	// Clamp ignored > errors: scored error_rate → 0; success stays true ratio.
	m2 := service.ChannelMonitorV2Metric{RequestCount: 10, ErrorRequests: 2, SuccessRequests: 8, ErrorRate: 0.2, SuccessRate: 0.8}
	applyIgnoredErrors(&m2, 99)
	require.InDelta(t, 0.0, m2.ErrorRate, 0.0001)
	require.InDelta(t, 0.8, m2.SuccessRate, 0.0001)

	// No-op when ignored is zero
	m3 := service.ChannelMonitorV2Metric{RequestCount: 10, ErrorRequests: 2, SuccessRequests: 8, ErrorRate: 0.2, SuccessRate: 0.8}
	applyIgnoredErrors(&m3, 0)
	require.InDelta(t, 0.2, m3.ErrorRate, 0.0001)
	require.InDelta(t, 0.8, m3.SuccessRate, 0.0001)
}

func TestRedactChannelMonitorV2MetricZerosVolume(t *testing.T) {
	// Service helper is in service package; covered there. Keep a smoke note that
	// rates survive a manual zeroing of volume fields used by the UI contract.
	m := service.ChannelMonitorV2Metric{
		RequestCount: 100, ErrorRequests: 10, SuccessRequests: 90,
		TokenCount: 1000, RPM: 5, TPM: 50, ErrorRate: 0.1, SuccessRate: 0.9, CacheRate: 0.4,
	}
	// Mimic redact: zero volume only
	m.RequestCount, m.ErrorRequests, m.SuccessRequests, m.TokenCount = 0, 0, 0, 0
	require.Equal(t, 0.1, m.ErrorRate)
	require.Equal(t, 5.0, m.RPM)
}

func TestChannelMonitorV2CatalogFilterClearsMultiSelectDimensions(t *testing.T) {
	start := time.Unix(1, 0)
	end := time.Unix(2, 0)
	filter := service.ChannelMonitorV2Filter{
		Start: start, End: end, Bucket: time.Minute,
		Platforms: []string{"openai"}, GroupIDs: []int64{3}, Models: []string{"gpt-5"},
	}
	catalog := channelMonitorV2CatalogFilter(filter)
	require.Nil(t, catalog.Platforms)
	require.Nil(t, catalog.GroupIDs)
	require.Nil(t, catalog.Models)
	// Time window / coverage-related fields remain.
	require.Equal(t, start, catalog.Start)
	require.Equal(t, end, catalog.End)
	require.Equal(t, time.Minute, catalog.Bucket)

	cfg := service.ChannelMonitorV2Config{
		Platforms: []service.ChannelMonitorV2PlatformConfig{
			{Platform: "openai", Enabled: true},
			{Platform: "grok", Enabled: true},
		},
		GroupIDs: []int64{3, 4},
	}
	catalogWhere, catalogArgs := channelMonitorV2Where(catalog, cfg, "m")
	_, metricArgs := channelMonitorV2Where(filter, cfg, "m")

	// Catalog WHERE still applies config scope (enabled platforms + group allow-list).
	require.Contains(t, catalogWhere, "m.platform = ANY")
	require.Contains(t, catalogWhere, "m.group_id = ANY")
	require.Len(t, catalogArgs, 4) // start, end, platforms, groups
	require.Len(t, metricArgs, 4)

	// Metrics WHERE is narrower once multi-select platforms/groups are applied.
	require.NotEqual(t, catalogArgs, metricArgs)
	// Group seeding without multi-select uses full config allow-list.
	require.Equal(t, []int64{3, 4}, configuredChannelMonitorV2GroupIDs(catalog, cfg))
	require.Equal(t, []int64{3}, configuredChannelMonitorV2GroupIDs(filter, cfg))
}

func TestChannelMonitorV2GetDimensionsDerivesPlatformsFromAccounts(t *testing.T) {
	// C3-FIX: platform dimensions must come from platforms that actually have an
	// active account (UNION real-traffic platforms), NOT from the config platforms
	// list. So config-only platforms with no accounts (kiro/grok/antigravity) must
	// be absent, while an account-bearing platform with zero traffic (zhipu) and one
	// with real traffic (openai) must both appear.
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := &channelMonitorV2Repository{db: db}

	filter := service.ChannelMonitorV2Filter{
		Start: time.Unix(1, 0), End: time.Unix(2, 0), Bucket: time.Minute,
	}
	// Config still lists the config-only noise platforms plus the real ones.
	cfg := service.ChannelMonitorV2Config{
		Platforms: []service.ChannelMonitorV2PlatformConfig{
			{Platform: "openai", Enabled: true},
			{Platform: "zhipu", Enabled: true},
			{Platform: "kiro", Enabled: true},
			{Platform: "grok", Enabled: true},
			{Platform: "antigravity", Enabled: true},
		},
	}

	// 1) watermark: no data → coverage empty, metrics window clamps to nothing.
	mock.ExpectQuery("FROM channel_monitor_v2_watermarks").WillReturnError(sql.ErrNoRows)
	// 2) metrics: only openai carries real traffic in the window.
	mock.ExpectQuery("SELECT m.platform").WillReturnRows(
		sqlmock.NewRows([]string{"platform", "name", "group_platform", "group_id", "model", "count"}).
			AddRow("openai", "", "unknown", int64(0), "gpt-5", int64(42)),
	)
	// 3) account platforms (active, non-deleted): openai + zhipu only.
	mock.ExpectQuery("FROM accounts").WillReturnRows(
		sqlmock.NewRows([]string{"platform"}).
			AddRow("openai").AddRow("zhipu"),
	)

	dimensions, err := repo.GetDimensions(context.Background(), filter, cfg)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	byValue := map[string]service.ChannelMonitorV2Dimension{}
	for _, p := range dimensions.Platforms {
		byValue[p.Value] = p
	}
	// Config-only platforms with no accounts must not appear.
	require.NotContains(t, byValue, "kiro")
	require.NotContains(t, byValue, "grok")
	require.NotContains(t, byValue, "antigravity")
	// Account-bearing platforms appear: openai (traffic) and zhipu (zero traffic).
	require.Contains(t, byValue, "openai")
	require.Contains(t, byValue, "zhipu")
	require.Equal(t, int64(42), byValue["openai"].RequestCount)
	require.Equal(t, int64(0), byValue["zhipu"].RequestCount)
}

func TestChannelMonitorV2GetProbeStatesReadsCatalogTriples(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := &channelMonitorV2Repository{db: db}

	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	keys := []service.ChannelMonitorCatalogKey{
		{Platform: "openai", Model: "gpt-5", GroupID: 1},
		{Platform: "anthropic", Model: "claude", GroupID: 2},
	}
	mock.ExpectQuery("SELECT platform, model, group_id").
		WithArgs("openai", "gpt-5", int64(1), "anthropic", "claude", int64(2)).
		WillReturnRows(
			sqlmock.NewRows([]string{
				"platform", "model", "group_id", "last_probe_at", "last_ok", "last_detail", "last_account_id", "next_due", "first_token_ms", "total_ms",
			}).
				AddRow("openai", "gpt-5", int64(1), now, true, "ok status=200", int64(11), now.Add(30*time.Minute), int64(2745), int64(5800)).
				AddRow("anthropic", "claude", int64(2), now, false, "upstream status=503", int64(22), now.Add(2*time.Hour), nil, nil),
		)

	states, err := repo.GetProbeStates(context.Background(), keys)
	require.NoError(t, err)
	require.Len(t, states, 2)
	require.NoError(t, mock.ExpectationsWereMet())

	ok := states[keys[0]]
	require.NotNil(t, ok)
	require.True(t, ok.LastOK)
	require.Equal(t, "ok status=200", ok.LastDetail)
	require.Equal(t, int64(11), ok.LastAccountID)
	require.NotNil(t, ok.FirstTokenMs)
	require.Equal(t, 2745, *ok.FirstTokenMs)
	require.NotNil(t, ok.TotalMs)
	require.Equal(t, 5800, *ok.TotalMs)
	fail := states[keys[1]]
	require.NotNil(t, fail)
	require.False(t, fail.LastOK)
	require.Equal(t, int64(22), fail.LastAccountID)
	require.Nil(t, fail.FirstTokenMs)
	require.Nil(t, fail.TotalMs)
}

func TestChannelMonitorV2GetProbeStatesEmptyKeysNoQuery(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := &channelMonitorV2Repository{db: db}

	states, err := repo.GetProbeStates(context.Background(), nil)
	require.NoError(t, err)
	require.Empty(t, states)
	require.NoError(t, mock.ExpectationsWereMet())
}

// ---------- 测试（DP-BE-001）：model 粒度聚合键 / 去渠道化 ----------

// 维度键在 model 粒度下跨平台坍缩；平台与分组被置空，不得带出渠道标识。
func TestChannelMonitorV2MatrixDimensionKeyModelGrainCollapsesAcrossPlatforms(t *testing.T) {
	cfg := service.ChannelMonitorV2Config{Platforms: []service.ChannelMonitorV2PlatformConfig{
		{Platform: "openai", Enabled: true},
		{Platform: "anthropic", Enabled: true},
	}}
	k1 := channelMonitorV2MatrixDimensionKey(service.ChannelMonitorV2GroupByModel, cfg, "openai", 0, "gpt-4")
	k2 := channelMonitorV2MatrixDimensionKey(service.ChannelMonitorV2GroupByModel, cfg, "anthropic", 0, "gpt-4")
	require.Equal(t, k1, k2, "same display model on different platforms must share one key")
	require.Equal(t, channelMonitorV2MatrixKey{platform: "", groupID: 0, model: "gpt-4"}, k1)

	// 空模型名回落 __other__ 且同样跨平台坍缩。
	k3 := channelMonitorV2MatrixDimensionKey(service.ChannelMonitorV2GroupByModel, cfg, "openai", 7, "")
	k4 := channelMonitorV2MatrixDimensionKey(service.ChannelMonitorV2GroupByModel, cfg, "anthropic", 9, "")
	require.Equal(t, k3, k4)
	require.Equal(t, channelMonitorV2MatrixKey{platform: "", groupID: 0, model: service.ChannelMonitorV2OtherModel}, k3)
}

// model 粒度不应产生任何种子键（避免平台标记的零流量行）。
func TestChannelMonitorV2MatrixSeedsNothingForModelGrain(t *testing.T) {
	filter := service.ChannelMonitorV2Filter{}
	cfg := service.ChannelMonitorV2Config{
		Platforms: []service.ChannelMonitorV2PlatformConfig{
			{Platform: "openai", Enabled: true},
			{Platform: "anthropic", Enabled: true},
		},
	}
	accs := seedChannelMonitorV2MatrixAccumulators(filter, cfg, service.ChannelMonitorV2GroupByModel, map[int64]channelMonitorV2GroupInfo{})
	require.Empty(t, accs)
}

// 用例 1：跨平台同名模型合并为一行，成功率为合并后重算值（非任一平台原值），且去渠道化。
func TestChannelMonitorV2GetMatrixModelGrainMergesCrossPlatform(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := &channelMonitorV2Repository{db: db}

	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(90 * time.Minute)
	filter := service.ChannelMonitorV2Filter{Start: start, End: end, Bucket: 5 * time.Minute}
	cfg := service.ChannelMonitorV2Config{
		Enabled: true,
		Platforms: []service.ChannelMonitorV2PlatformConfig{
			{Platform: "openai", Enabled: true},
			{Platform: "anthropic", Enabled: true},
		},
	}

	// 1) watermark：data_through 覆盖到窗口末，使事实窗口有效。
	mock.ExpectQuery("FROM channel_monitor_v2_watermarks").
		WillReturnRows(sqlmock.NewRows([]string{
			"usage_coverage_start", "error_coverage_start", "data_through", "last_successful_at", "backfill_cursor",
		}).AddRow(start, start, end, end, start))

	// 2) facts：openai/gpt-4（80 成功 / 20 错误）与 anthropic/gpt-4（95 成功 / 5 错误）。
	mock.ExpectQuery("FROM channel_monitor_v2_metrics_rollup").
		WithArgs(start, end, pq.Array([]string{"openai", "anthropic"}), int64(300)).
		WillReturnRows(sqlmock.NewRows([]string{
			"bucket_start", "platform", "group_id", "name", "model",
			"success", "errors", "upstream_affected", "upstream_attempts",
			"input", "output", "cache_creation", "cache_read",
			"ttft_sum", "ttft_count", "duration_sum", "duration_count",
		}).
			AddRow(start, "openai", int64(0), "", "gpt-4", int64(80), int64(20), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)).
			AddRow(start, "anthropic", int64(0), "", "gpt-4", int64(95), int64(5), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)))

	// 3) histograms：空。
	mock.ExpectQuery("FROM channel_monitor_v2_latency_histograms_rollup").
		WithArgs(start, end, pq.Array([]string{"openai", "anthropic"}), int64(300), int64(0)).
		WillReturnRows(sqlmock.NewRows([]string{
			"bucket_start", "platform", "group_id", "model", "user_id", "metric", "upper_bound_ms", "sample_count",
		}))

	matrix, err := repo.GetMatrix(context.Background(), filter, cfg, service.ChannelMonitorV2GroupByModel, false)
	require.NoError(t, err)
	require.Equal(t, service.ChannelMonitorV2GroupByModel, matrix.GroupBy)
	require.NoError(t, mock.ExpectationsWereMet())

	// 两平台同名模型坍缩为一行；成功率为合并值 (80+95)/(100+100)=0.875，非 0.8 或 0.95。
	require.Len(t, matrix.Items, 1, "cross-platform same model must merge into one row")
	row := matrix.Items[0]
	require.Equal(t, "gpt-4", row.Model)
	require.Empty(t, row.Platform, "model grain must not carry a platform")
	require.Equal(t, int64(200), row.Metrics.RequestCount)
	require.InDelta(t, 0.875, row.Metrics.SuccessRate, 0.0001)
	require.InDelta(t, 0.125, row.Metrics.ErrorRate, 0.0001)
}

// ---------- 测试（DP-C10-001）：GetModels 模型明细表跨平台合并 ----------

// 用例 1：非管理员跨平台同名模型合并为一行，成功率为合并后重算值（非任一平台原值），且去渠道化（Platform 为空）。
func TestChannelMonitorV2GetModelsNonAdminMergesCrossPlatform(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := &channelMonitorV2Repository{db: db}

	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(90 * time.Minute)
	filter := service.ChannelMonitorV2Filter{Start: start, End: end, Bucket: 5 * time.Minute}
	cfg := service.ChannelMonitorV2Config{
		Enabled: true,
		Platforms: []service.ChannelMonitorV2PlatformConfig{
			{Platform: "openai", Enabled: true},
			{Platform: "anthropic", Enabled: true},
		},
	}

	// 1) watermark：data_through 覆盖到窗口末。
	mock.ExpectQuery("FROM channel_monitor_v2_watermarks").
		WillReturnRows(sqlmock.NewRows([]string{
			"usage_coverage_start", "error_coverage_start", "data_through", "last_successful_at", "backfill_cursor",
		}).AddRow(start, start, end, end, start))

	// 2) facts：openai/gpt-4（80 成功 / 20 错误）与 anthropic/gpt-4（95 成功 / 5 错误）。
	mock.ExpectQuery("FROM channel_monitor_v2_metrics_rollup").
		WithArgs(start, end, pq.Array([]string{"openai", "anthropic"}), int64(300)).
		WillReturnRows(sqlmock.NewRows([]string{
			"bucket_start", "platform", "group_id", "name", "model",
			"success", "errors", "upstream_affected", "upstream_attempts",
			"input", "output", "cache_creation", "cache_read",
			"ttft_sum", "ttft_count", "duration_sum", "duration_count",
		}).
			AddRow(start, "openai", int64(0), "", "gpt-4", int64(80), int64(20), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)).
			AddRow(start, "anthropic", int64(0), "", "gpt-4", int64(95), int64(5), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)))

	// 3) histograms：空。
	mock.ExpectQuery("FROM channel_monitor_v2_latency_histograms_rollup").
		WithArgs(start, end, pq.Array([]string{"openai", "anthropic"}), int64(300), int64(0)).
		WillReturnRows(sqlmock.NewRows([]string{
			"bucket_start", "platform", "group_id", "model", "user_id", "metric", "upper_bound_ms", "sample_count",
		}))

	models, err := repo.GetModels(context.Background(), filter, cfg, false)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	// 两平台同名模型坍缩为一行；成功率为合并值 (80+95)/(100+100)=0.875，非 0.8 或 0.95。
	require.Len(t, models.Items, 1, "cross-platform same model must merge into one row")
	row := models.Items[0]
	require.Equal(t, "gpt-4", row.Model)
	require.Empty(t, row.Platform, "non-admin detail row must not carry a platform")
	require.Equal(t, int64(200), row.Metrics.RequestCount)
	require.InDelta(t, 0.875, row.Metrics.SuccessRate, 0.0001)
	require.InDelta(t, 0.125, row.Metrics.ErrorRate, 0.0001)
}

// 用例 2（管理员路径行为不变）：管理员保留平台维度，两平台同名模型仍是两行，Platform 字段非空，
// 成功率为各平台原值（openai=0.8, anthropic=0.95），未被合并。
func TestChannelMonitorV2GetModelsAdminKeepsPlatformDimension(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := &channelMonitorV2Repository{db: db}

	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(90 * time.Minute)
	filter := service.ChannelMonitorV2Filter{Start: start, End: end, Bucket: 5 * time.Minute}
	cfg := service.ChannelMonitorV2Config{
		Enabled: true,
		Platforms: []service.ChannelMonitorV2PlatformConfig{
			{Platform: "openai", Enabled: true},
			{Platform: "anthropic", Enabled: true},
		},
	}

	mock.ExpectQuery("FROM channel_monitor_v2_watermarks").
		WillReturnRows(sqlmock.NewRows([]string{
			"usage_coverage_start", "error_coverage_start", "data_through", "last_successful_at", "backfill_cursor",
		}).AddRow(start, start, end, end, start))

	mock.ExpectQuery("FROM channel_monitor_v2_metrics_rollup").
		WithArgs(start, end, pq.Array([]string{"openai", "anthropic"}), int64(300)).
		WillReturnRows(sqlmock.NewRows([]string{
			"bucket_start", "platform", "group_id", "name", "model",
			"success", "errors", "upstream_affected", "upstream_attempts",
			"input", "output", "cache_creation", "cache_read",
			"ttft_sum", "ttft_count", "duration_sum", "duration_count",
		}).
			AddRow(start, "openai", int64(0), "", "gpt-4", int64(80), int64(20), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)).
			AddRow(start, "anthropic", int64(0), "", "gpt-4", int64(95), int64(5), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)))

	mock.ExpectQuery("FROM channel_monitor_v2_latency_histograms_rollup").
		WithArgs(start, end, pq.Array([]string{"openai", "anthropic"}), int64(300), int64(0)).
		WillReturnRows(sqlmock.NewRows([]string{
			"bucket_start", "platform", "group_id", "model", "user_id", "metric", "upper_bound_ms", "sample_count",
		}))

	models, err := repo.GetModels(context.Background(), filter, cfg, true)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	// 管理员路径保持平台维度：两平台同名模型两行，Platform 非空，成功率为各平台原值。
	require.Len(t, models.Items, 2, "admin path must keep platform dimension — two rows")
	byKey := map[string]service.ChannelMonitorV2ModelRow{}
	for _, it := range models.Items {
		require.NotEmpty(t, it.Platform, "admin row must keep its platform")
		require.Equal(t, "gpt-4", it.Model)
		byKey[it.Platform] = it
	}
	require.Contains(t, byKey, "openai")
	require.Contains(t, byKey, "anthropic")
	require.Equal(t, int64(100), byKey["openai"].Metrics.RequestCount)
	require.InDelta(t, 0.80, byKey["openai"].Metrics.SuccessRate, 0.0001)
	require.InDelta(t, 0.20, byKey["openai"].Metrics.ErrorRate, 0.0001)
	require.Equal(t, int64(100), byKey["anthropic"].Metrics.RequestCount)
	require.InDelta(t, 0.95, byKey["anthropic"].Metrics.SuccessRate, 0.0001)
	require.InDelta(t, 0.05, byKey["anthropic"].Metrics.ErrorRate, 0.0001)
}

// 用例 3：非管理员 __other__（空模型名）跨平台合并为一行。
func TestChannelMonitorV2GetModelsNonAdminMergesOtherModel(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := &channelMonitorV2Repository{db: db}

	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(90 * time.Minute)
	filter := service.ChannelMonitorV2Filter{Start: start, End: end, Bucket: 5 * time.Minute}
	cfg := service.ChannelMonitorV2Config{
		Enabled: true,
		Platforms: []service.ChannelMonitorV2PlatformConfig{
			{Platform: "openai", Enabled: true},
			{Platform: "anthropic", Enabled: true},
		},
	}

	mock.ExpectQuery("FROM channel_monitor_v2_watermarks").
		WillReturnRows(sqlmock.NewRows([]string{
			"usage_coverage_start", "error_coverage_start", "data_through", "last_successful_at", "backfill_cursor",
		}).AddRow(start, start, end, end, start))

	// 空模型名 → 展示模型回落 __other__；两平台各一行，跨平台合并。
	mock.ExpectQuery("FROM channel_monitor_v2_metrics_rollup").
		WithArgs(start, end, pq.Array([]string{"openai", "anthropic"}), int64(300)).
		WillReturnRows(sqlmock.NewRows([]string{
			"bucket_start", "platform", "group_id", "name", "model",
			"success", "errors", "upstream_affected", "upstream_attempts",
			"input", "output", "cache_creation", "cache_read",
			"ttft_sum", "ttft_count", "duration_sum", "duration_count",
		}).
			AddRow(start, "openai", int64(0), "", "", int64(30), int64(10), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)).
			AddRow(start, "anthropic", int64(0), "", "", int64(50), int64(10), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)))

	mock.ExpectQuery("FROM channel_monitor_v2_latency_histograms_rollup").
		WithArgs(start, end, pq.Array([]string{"openai", "anthropic"}), int64(300), int64(0)).
		WillReturnRows(sqlmock.NewRows([]string{
			"bucket_start", "platform", "group_id", "model", "user_id", "metric", "upper_bound_ms", "sample_count",
		}))

	models, err := repo.GetModels(context.Background(), filter, cfg, false)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	// __other__ 跨平台合并为一行；成功率为合并值 (30+50)/(40+60)=0.8。
	require.Len(t, models.Items, 1, "cross-platform __other__ must merge into one row")
	row := models.Items[0]
	require.Equal(t, service.ChannelMonitorV2OtherModel, row.Model)
	require.Empty(t, row.Platform, "non-admin __other__ row must not carry a platform")
	require.Equal(t, int64(100), row.Metrics.RequestCount)
	require.InDelta(t, 0.80, row.Metrics.SuccessRate, 0.0001)
	require.InDelta(t, 0.20, row.Metrics.ErrorRate, 0.0001)
}
