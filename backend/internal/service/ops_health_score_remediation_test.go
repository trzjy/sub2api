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

	// 关键点一律由曲线自身的 good/bad/floorScore/zero 推出，不在这里抄一份数字。
	//
	// 教训（2026-11-15）：这张表原先写死 good=1000 / bad=3000 / zero=15000，
	// 而 2026-10-27 已把 TTFT 曲线改成 good=3000 / bad=10000 / zero=120000
	// （缓存密集型长上下文业务，P99 实测 69042ms）。改阈值时没人回来改这张表，
	// 于是 6 条断言长期失败，失败信息还指向「实现坏了」——其实断言的语义全对，
	// 只是数字过期了。曲线是唯一事实源，测试只能引用它，不能再存第二份。
	type point struct {
		name  string
		curve opsBusinessCurve
		v     float64
		want  float64
	}

	band := func(tag string, curve opsBusinessCurve) []point {
		tail := curve.zero - curve.bad
		return []point{
			{tag + " at good is perfect", curve, curve.good, 100.0},
			{tag + " below good is perfect", curve, curve.good / 2, 100.0},
			{tag + " mid ramp", curve, (curve.good + curve.bad) / 2, 100 - (100-curve.floorScore)/2},
			{tag + " at bad equals floor", curve, curve.bad, curve.floorScore},
			{tag + " just past bad decays smoothly", curve, curve.bad + tail*1e-6, curve.floorScore * (1 - 1e-6)},
			{tag + " mid decay tail", curve, (curve.bad + curve.zero) / 2, curve.floorScore / 2},
			{tag + " at zero", curve, curve.zero, 0.0},
			{tag + " beyond zero pinned", curve, curve.zero * 2, 0.0},
		}
	}

	tests := append(band("error rate", opsErrRateCurve), band("ttft", opsTTFTCurve)...)

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
	// 采样点按曲线取（理由同 TestOpsBusinessSubScore_TransitionBandIsContinuous）。
	business := func(p99ms float64) float64 {
		return computeBusinessHealth(&OpsDashboardOverview{
			ErrorRate: 0,
			TTFT:      OpsPercentiles{P99: intPtr(int(p99ms))},
		})
	}

	curve := opsTTFTCurve
	tail := curve.zero - curve.bad
	justOver := business(curve.bad + tail*0.01) // 刚进尾段，余量还很足
	midTail := business(curve.bad + tail*0.50)  // 尾段中点
	atZero := business(curve.zero)              // 尾段终点：分项归零
	worse := business(curve.zero * 4)           // 远超终点：仍归零

	require.Greater(t, justOver, midTail, "尾段顶部必须高于中点")
	require.Greater(t, midTail, atZero, "尾段中点必须高于终点")
	require.Equal(t, atZero, worse, "两个都算彻底超时，都钉在 0")

	require.Greater(t, justOver, 50.0,
		"刚好越过 bad 时旧实现直接给 0 分（业务分只剩完美的另一半 50），尾段必须保留部分信用")
	require.Equal(t, 50.0, atZero, "彻底超时的流只剩完美的那一半")
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
