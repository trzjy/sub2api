//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// --- 整改1: per-job freshness cadence -------------------------------------

// Regression guard for the production incident where jobScore was pinned at
// 62.5 (= (1 - 3/8) * 100) because three long-period jobs were judged failed by
// a global 15-minute staleness rule, despite having no last_error at all.
func TestOpsJobStaleAfter_PeriodCadence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		jobName  string
		want     time.Duration
		wantNote string
	}{
		{
			name:     "24h retention job gets multi-day window",
			jobName:  cursorAccountRetentionJobName,
			want:     48 * time.Hour,
			wantNote: "24h period * 2 cycles; the pre-fix rule gave 15m and always failed",
		},
		{
			name:     "1h daily pre-aggregation gets multi-hour window",
			jobName:  opsAggDailyJobName,
			want:     2 * time.Hour,
			wantNote: "1h period * 2 cycles; pre-fix 15m failed it whenever it skipped one tick",
		},
		{
			name:    "daily cron cleanup gets multi-day window",
			jobName: opsCleanupJobName,
			want:    48 * time.Hour,
		},
		{
			name:    "daily cron scheduled reports get multi-day window",
			jobName: opsScheduledReportJobName,
			want:    48 * time.Hour,
		},
		{
			name:    "10m pre-aggregation gets 20m",
			jobName: opsAggHourlyJobName,
			want:    20 * time.Minute,
		},
		{
			name:    "short-cycle jobs stay at the 15m floor",
			jobName: quotaKeySweepJobName,
			want:    opsJobStaleFloor,
		},
		{
			name:    "5m reconciler stays at the 15m floor",
			jobName: quotaKeyReconcileJobName,
			want:    opsJobStaleFloor,
		},
		{
			name:    "1m collector stays at the 15m floor",
			jobName: opsMetricsCollectorJobName,
			want:    opsJobStaleFloor,
		},
		{
			name:    "alert evaluator stays at the 15m floor",
			jobName: opsAlertEvaluatorJobName,
			want:    opsJobStaleFloor,
		},
		{
			name:    "unknown job keeps the pre-fix 15m behaviour",
			jobName: "some_future_job",
			want:    opsJobStaleFloor,
		},
		{
			name:    "empty job name does not panic",
			jobName: "",
			want:    opsJobStaleFloor,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, opsJobStaleAfter(tt.jobName), tt.wantNote)
		})
	}
}

// Every job that writes a heartbeat must have a cadence entry, otherwise it
// silently inherits the 15m default and is mis-scored if it runs slower.
func TestOpsJobCadences_CoversEveryHeartbeatWriter(t *testing.T) {
	t.Parallel()

	// All JobName constants in the service package that call
	// OpsRepository.UpsertJobHeartbeat.
	heartbeatWriters := []string{
		opsMetricsCollectorJobName,
		opsAggHourlyJobName,
		opsAggDailyJobName,
		opsCleanupJobName,
		opsScheduledReportJobName,
		opsAlertEvaluatorJobName,
		quotaKeySweepJobName,
		quotaKeyReconcileJobName,
		cursorAccountRetentionJobName,
	}
	require.Len(t, heartbeatWriters, len(opsJobCadences),
		"every heartbeat writer needs a cadence entry, and every entry needs a writer")

	for _, jobName := range heartbeatWriters {
		_, ok := opsJobCadences[jobName]
		require.True(t, ok, "missing cadence entry for heartbeat writer %q", jobName)
	}
}

// Core regression: a 24h-period job that succeeded 20 hours ago is healthy, not
// failed. Under the old global 15-minute rule this alone cost the infra score.
func TestComputeInfraHealth_LongPeriodJobNotJudgedFailed(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	healthyInfra := &OpsSystemMetricsSnapshot{
		DBOK:               boolPtr(true),
		RedisOK:            boolPtr(true),
		CPUUsagePercent:    float64Ptr(30),
		MemoryUsagePercent: float64Ptr(40),
	}

	// The exact production shape: three long-period jobs, last success inside
	// their own period, zero recorded errors.
	ov := &OpsDashboardOverview{
		RequestCountTotal: 1000,
		SystemMetrics:     healthyInfra,
		JobHeartbeats: []*OpsJobHeartbeat{
			{
				JobName:       cursorAccountRetentionJobName,
				LastSuccessAt: healthTimePtr(now.Add(-20 * time.Hour)),
			},
			{
				JobName:       opsAggDailyJobName,
				LastSuccessAt: healthTimePtr(now.Add(-50 * time.Minute)),
			},
			{
				JobName:       opsCleanupJobName,
				LastSuccessAt: healthTimePtr(now.Add(-23 * time.Hour)),
			},
		},
	}

	// jobScore must be 100, so infra = 40%*100 + 30%*100 + 30%*100 = 100.
	score := computeInfraHealth(now, ov)
	require.Equal(t, 100.0, score,
		"long-period jobs inside their own cadence must not be scored as failed")
}

func TestComputeInfraHealth_LongPeriodJobStillDetectedWhenGenuinelyStale(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	ov := &OpsDashboardOverview{
		RequestCountTotal: 1000,
		SystemMetrics: &OpsSystemMetricsSnapshot{
			DBOK:               boolPtr(true),
			RedisOK:            boolPtr(true),
			CPUUsagePercent:    float64Ptr(30),
			MemoryUsagePercent: float64Ptr(40),
		},
		JobHeartbeats: []*OpsJobHeartbeat{
			{
				JobName: cursorAccountRetentionJobName,
				// 72h since last success > 48h window: two missed daily runs.
				LastSuccessAt: healthTimePtr(now.Add(-72 * time.Hour)),
			},
		},
	}

	// jobScore = 0, so infra = 40 + 30 + 0 = 70.
	score := computeInfraHealth(now, ov)
	require.Equal(t, 70.0, score,
		"a 24h job silent for 72h is genuinely stuck and must still be penalised")
}

// Boundary: just inside the 48h window stays healthy, just outside fails.
func TestComputeInfraHealth_RetentionJobStaleBoundary(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	newOverview := func(since time.Duration) *OpsDashboardOverview {
		return &OpsDashboardOverview{
			RequestCountTotal: 1000,
			SystemMetrics: &OpsSystemMetricsSnapshot{
				DBOK:               boolPtr(true),
				RedisOK:            boolPtr(true),
				CPUUsagePercent:    float64Ptr(30),
				MemoryUsagePercent: float64Ptr(40),
			},
			JobHeartbeats: []*OpsJobHeartbeat{{
				JobName:       cursorAccountRetentionJobName,
				LastSuccessAt: healthTimePtr(now.Add(-since)),
			}},
		}
	}

	require.Equal(t, 100.0, computeInfraHealth(now, newOverview(47*time.Hour)),
		"47h < 48h window: still healthy")
	require.Less(t, computeInfraHealth(now, newOverview(49*time.Hour)), 100.0,
		"49h > 48h window: must be penalised")
}

// The whole point of 整改1: production's eight healthy jobs must no longer be
// scored 62.5. With every job inside its cadence, jobScore is 100.
func TestComputeDashboardHealthScore_ProductionHeartbeatShapeNoLongerWhiteDeducted(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	// Production: 8 heartbeats, all last_error empty, 3 of them long-period.
	jobNames := []string{
		cursorAccountRetentionJobName,
		opsAggDailyJobName,
		opsCleanupJobName,
		opsAggHourlyJobName,
		opsMetricsCollectorJobName,
		opsAlertEvaluatorJobName,
		quotaKeySweepJobName,
		quotaKeyReconcileJobName,
	}
	heartbeats := make([]*OpsJobHeartbeat, 0, len(jobNames))
	for _, name := range jobNames {
		age := time.Duration(1) * time.Minute
		if name == cursorAccountRetentionJobName {
			age = 20 * time.Hour
		}
		if name == opsCleanupJobName {
			age = 23 * time.Hour
		}
		if name == opsAggDailyJobName {
			age = 50 * time.Minute
		}
		heartbeats = append(heartbeats, &OpsJobHeartbeat{
			JobName:       name,
			LastSuccessAt: healthTimePtr(now.Add(-age)),
		})
	}

	ov := &OpsDashboardOverview{
		RequestCountTotal: 1000,
		RequestCountSLA:   1000,
		SLA:               0.999,
		ErrorRate:         0.001,
		UpstreamErrorRate: 0.0005,
		TTFT:              OpsPercentiles{P99: intPtr(200)},
		SystemMetrics: &OpsSystemMetricsSnapshot{
			DBOK:               boolPtr(true),
			RedisOK:            boolPtr(true),
			CPUUsagePercent:    float64Ptr(11.8),
			MemoryUsagePercent: float64Ptr(14.1),
		},
		JobHeartbeats: heartbeats,
	}

	score := computeDashboardHealthScore(now, ov)
	require.Equal(t, 100, score,
		"a fully healthy production-shaped snapshot must not be docked by heartbeat cadence")
	// Pin the infra term too: this is the exact 62.5 jobScore from production.
	// Under the old global 15m rule this overview scored 88.75 (jobScore 62.5).
	require.Equal(t, 100.0, computeInfraHealth(now, ov))
}

// A short-cycle job that silently dies is still caught promptly, i.e. the fix
// did not cost sensitivity for fast jobs.
func TestComputeInfraHealth_ShortCycleJobStillDetectedQuickly(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	ov := &OpsDashboardOverview{
		RequestCountTotal: 1000,
		SystemMetrics: &OpsSystemMetricsSnapshot{
			DBOK:               boolPtr(true),
			RedisOK:            boolPtr(true),
			CPUUsagePercent:    float64Ptr(30),
			MemoryUsagePercent: float64Ptr(40),
		},
		JobHeartbeats: []*OpsJobHeartbeat{{
			JobName:       opsMetricsCollectorJobName,
			LastSuccessAt: healthTimePtr(now.Add(-16 * time.Minute)),
		}},
	}

	require.Less(t, computeInfraHealth(now, ov), 100.0,
		"a 1-minute job silent for 16m must still be counted as failed")
}

// A recorded error beats any cadence: last_error after last_success is failed
// regardless of how recently it ran.
func TestComputeInfraHealth_RecordedErrorWins(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	ov := &OpsDashboardOverview{
		RequestCountTotal: 1000,
		SystemMetrics: &OpsSystemMetricsSnapshot{
			DBOK:               boolPtr(true),
			RedisOK:            boolPtr(true),
			CPUUsagePercent:    float64Ptr(30),
			MemoryUsagePercent: float64Ptr(40),
		},
		JobHeartbeats: []*OpsJobHeartbeat{{
			JobName:       opsAggDailyJobName,
			LastRunAt:     healthTimePtr(now.Add(-30 * time.Minute)),
			LastSuccessAt: healthTimePtr(now.Add(-30 * time.Minute)),
			LastErrorAt:   healthTimePtr(now.Add(-29 * time.Minute)),
			LastError:     stringPtr("boom"),
		}},
	}

	require.Less(t, computeInfraHealth(now, ov), 100.0,
		"a job whose last outcome was an error is failed even if it just ran")
}

// --- 整改3: business score transition band --------------------------------

func TestOpsBusinessSubScore_TransitionBandIsContinuous(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		curve opsBusinessCurve
		v     float64
		want  float64
	}{
		{"at good is perfect", opsErrRateCurve, 1.0, 100.0},
		{"below good is perfect", opsErrRateCurve, 0.2, 100.0},
		{"mid ramp", opsErrRateCurve, 5.0, 68.88888888888889},
		{"at bad equals floor", opsErrRateCurve, 10.0, 30.0},
		{"just past bad decays smoothly", opsErrRateCurve, 10.0001, 29.99994},
		{"mid decay tail", opsErrRateCurve, 30.0, 15.0},
		{"at zero", opsErrRateCurve, 50.0, 0.0},
		{"beyond zero pinned", opsErrRateCurve, 99.0, 0.0},
		{"ttft at good", opsTTFTCurve, 1000.0, 100.0},
		{"ttft mid ramp", opsTTFTCurve, 2000.0, 65.0},
		{"ttft at bad equals floor", opsTTFTCurve, 3000.0, 30.0},
		{"ttft just past bad decays smoothly", opsTTFTCurve, 3000.001, 29.99999},
		{"ttft mid decay tail", opsTTFTCurve, 9000.0, 15.0},
		{"ttft at zero", opsTTFTCurve, 15000.0, 0.0},
		{"ttft beyond zero pinned", opsTTFTCurve, 60000.0, 0.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := opsBusinessSubScore(tt.v, tt.curve)
			require.InDelta(t, tt.want, got, 0.001)
		})
	}
}

// The original defect: a hair over the threshold used to score identically to a
// catastrophic reading. Values beyond `bad` must now be strictly ordered.
func TestOpsBusinessSubScore_ResolvesBeyondThreshold(t *testing.T) {
	t.Parallel()

	just := opsBusinessSubScore(10.01, opsErrRateCurve)
	worse := opsBusinessSubScore(20.0, opsErrRateCurve)
	catastrophic := opsBusinessSubScore(45.0, opsErrRateCurve)

	require.Greater(t, just, worse, "10.01%% must score above 20%%")
	require.Greater(t, worse, catastrophic, "20%% must score above 45%%")
	require.Greater(t, catastrophic, 0.0, "45%% is bad but not a total outage")
	require.Less(t, catastrophic, just, "the decay tail must keep descending")
}

// Monotonicity across the whole domain: score must never increase as the
// metric gets worse, including across the bad/zero joins.
func TestOpsBusinessSubScore_MonotonicNonIncreasing(t *testing.T) {
	t.Parallel()

	curves := map[string]opsBusinessCurve{
		"error rate": opsErrRateCurve,
		"ttft":       opsTTFTCurve,
	}
	maxs := map[string]float64{"error rate": 100.0, "ttft": 60000.0}

	for name, curve := range curves {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			prev := opsBusinessSubScore(0, curve)
			for v := 0.0; v <= maxs[name]; v += maxs[name] / 20000.0 {
				cur := opsBusinessSubScore(v, curve)
				require.LessOrEqual(t, cur, prev+1e-9,
					"score must not increase as the metric worsens (v=%v)", v)
				prev = cur
			}
		})
	}
}

// Business health must retain resolution inside the decay tail. Pre-fix every
// TTFT above 3s scored exactly 0, so 3.1s and 14s were indistinguishable.
func TestComputeBusinessHealth_SingleBadComponentKeepsResolution(t *testing.T) {
	t.Parallel()

	// Errors perfect, TTFT degrading: business = 100*0.5 + ttft*0.5.
	business := func(p99ms int) float64 {
		return computeBusinessHealth(&OpsDashboardOverview{
			ErrorRate: 0,
			TTFT:      OpsPercentiles{P99: intPtr(p99ms)},
		})
	}

	justOver := business(3_100) // just past bad=3000, still deep in the tail
	midTail := business(9_000)  // middle of the tail
	atZero := business(15_000)  // tail end: sub-score 0
	worse := business(60_000)   // far past zero: sub-score 0

	require.Greater(t, justOver, midTail, "3.1s must score above 9s")
	require.Greater(t, midTail, atZero, "9s must score above 15s")
	require.Equal(t, atZero, worse, "both are total outages, both pin to 0")

	require.Greater(t, justOver, 50.0,
		"3.1s pre-fix scored exactly 50; the tail must keep partial credit")
	require.Equal(t, 50.0, atZero, "a fully timed-out stream leaves only the perfect half")
}

// Errors bad, TTFT perfect: mirrors the assertion above on the other axis.
func TestComputeBusinessHealth_ErrorRateDecayTail(t *testing.T) {
	t.Parallel()

	justOver := computeBusinessHealth(&OpsDashboardOverview{ErrorRate: 0.10})
	midTail := computeBusinessHealth(&OpsDashboardOverview{ErrorRate: 0.30})
	pinned := computeBusinessHealth(&OpsDashboardOverview{ErrorRate: 0.49})

	require.Greater(t, justOver, midTail, "10%% must score above 30%%")
	require.Greater(t, midTail, pinned, "30%% must score above 49%%")
	require.Greater(t, midTail, 0.0, "30%% is bad but not a total outage")
}
