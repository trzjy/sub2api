package service

import (
	"math"
	"time"
)

// =========================
// Background job freshness
// =========================

const (
	// opsJobStaleCycles is how many consecutive scheduled runs a job may miss
	// before it counts as failed. One missed run is routine (deploy, GC pause,
	// transient DB blip); two in a row means the job is genuinely stuck.
	opsJobStaleCycles = 2

	// opsJobStaleFloor keeps the pre-existing 15-minute window for short-cycle
	// jobs, so the per-job cadence below never *loosens* the common case.
	opsJobStaleFloor = 15 * time.Minute

	// opsJobUnknownCadence is assumed for heartbeat jobs missing from
	// opsJobCadences: a typical ticker period. Unknown jobs therefore keep the
	// old 15-minute behaviour rather than silently inheriting a wider window.
	opsJobUnknownCadence = 5 * time.Minute
)

// opsJobCadences maps a heartbeat job name to its scheduled run period.
//
// Every caller of OpsRepository.UpsertJobHeartbeat needs an entry here. Without
// one, a job whose real period exceeds opsJobStaleFloor is scored as failed
// purely for being slow, which docks the infra score permanently.
//
// Values are the job's compiled-in default period. Jobs whose period is
// operator-configurable (collector interval, alert evaluator interval, cleanup
// cron) use their default, which keeps the floor at opsJobStaleFloor exactly as
// before for those jobs; raising such a job's interval beyond its default
// requires raising its entry here too, otherwise it reads as a false failure.
var opsJobCadences = map[string]time.Duration{
	// Configurable via SettingKeyOpsMetricsIntervalSeconds (default 60s).
	opsMetricsCollectorJobName: opsMetricsCollectorMinInterval,
	// Fixed tickers.
	opsAggHourlyJobName: opsAggHourlyInterval,
	opsAggDailyJobName:  opsAggDailyInterval,
	// Cron-driven: assumes the default daily schedule (0 2 * * *). A weekly
	// cleanup schedule would need this raised to match.
	opsCleanupJobName:         24 * time.Hour,
	opsScheduledReportJobName: 24 * time.Hour,
	// Configurable via OpsAlertRuntimeSettings.EvaluationIntervalSeconds
	// (default 60s, see defaultOpsAlertRuntimeSettings).
	opsAlertEvaluatorJobName: 60 * time.Second,
	// Fixed tickers.
	quotaKeySweepJobName:          quotaKeySweepInterval,
	quotaKeyReconcileJobName:      quotaKeyReconcileInterval,
	cursorAccountRetentionJobName: cursorRetentionCheckInterval,
}

// opsJobStaleAfter returns how long a job may go without a successful run
// before its heartbeat counts as failed.
func opsJobStaleAfter(jobName string) time.Duration {
	cadence, ok := opsJobCadences[jobName]
	if !ok {
		cadence = opsJobUnknownCadence
	}
	if stale := cadence * opsJobStaleCycles; stale > opsJobStaleFloor {
		return stale
	}
	return opsJobStaleFloor
}

// computeDashboardHealthScore computes a 0-100 health score from the metrics returned by the dashboard overview.
//
// Design goals:
// - Backend-owned scoring (UI only displays).
// - Layered scoring: Business Health (70%) + Infrastructure Health (30%)
// - Avoids double-counting (e.g., DB failure affects both infra and business metrics)
// - Conservative + stable: penalize clear degradations; avoid overreacting to missing/idle data.
func computeDashboardHealthScore(now time.Time, overview *OpsDashboardOverview) int {
	if overview == nil {
		return 0
	}

	// Idle/no-data: avoid showing a "bad" score when there is no traffic.
	// UI can still render a gray/idle state based on QPS + error rate.
	if overview.RequestCountSLA <= 0 && overview.RequestCountTotal <= 0 && overview.ErrorCountTotal <= 0 {
		return 100
	}

	businessHealth := computeBusinessHealth(overview)
	infraHealth := computeInfraHealth(now, overview)

	// Weighted combination: 70% business + 30% infrastructure
	score := businessHealth*0.7 + infraHealth*0.3
	return int(math.Round(clampFloat64(score, 0, 100)))
}

// computeBusinessHealth calculates business health score (0-100)
// Components: Error Rate (50%) + TTFT (50%)
func computeBusinessHealth(overview *OpsDashboardOverview) float64 {
	// Error rate: combines request errors and upstream errors, worst case wins.
	errorPct := clampFloat64(overview.ErrorRate*100, 0, 100)
	upstreamPct := clampFloat64(overview.UpstreamErrorRate*100, 0, 100)
	combinedErrorPct := math.Max(errorPct, upstreamPct) // Use worst case
	errorScore := opsBusinessSubScore(combinedErrorPct, opsErrRateCurve)

	// TTFT score: Time to first token is critical for user experience.
	ttftScore := 100.0
	if overview.TTFT.P99 != nil {
		ttftScore = opsBusinessSubScore(float64(*overview.TTFT.P99), opsTTFTCurve)
	}

	// Weighted combination: 50% error rate + 50% TTFT
	return errorScore*0.5 + ttftScore*0.5
}

// opsBusinessCurve describes a two-segment piecewise-linear business sub-score.
//
// The ramp segment (good -> bad) spans 100 -> floorScore, and the decay tail
// (bad -> zero) spans floorScore -> 0. The tail is what makes the score
// continuous past the threshold: without it the score hits 0 exactly at `bad`
// and stays pinned there, so a marginally-over-threshold reading and a
// catastrophic one scored identically and the metric lost all resolution.
type opsBusinessCurve struct {
	good       float64 // at or below this value the sub-score is 100
	bad        float64 // end of the ramp / start of the decay tail
	floorScore float64 // sub-score at `bad`, and the ceiling of the decay tail
	zero       float64 // sub-score reaches 0 here and stays 0 beyond
}

var (
	// Error rate in percent: 1% or less is perfect, 10% is the tail's shoulder.
	opsErrRateCurve = opsBusinessCurve{good: 1.0, bad: 10.0, floorScore: 30.0, zero: 50.0}
	// TTFT p99 in milliseconds: 3s or less is perfect, 10s is the tail's shoulder.
	//
	// 2026-10-27 调整（原值 good=1000 / bad=3000 / zero=15000）：
	// 本站是缓存密集型长上下文业务，TTFT 主要等于上游预填充时间而非本站处理耗时
	// （生产实测 1h 窗：TTFT 占总耗时 68.4% = 17951ms / 26246ms；TTFT 分位
	// P50=14684 / P90=34610 / P95=44308 / P99=69042ms；同一类请求在自建额度
	// 通道 TTFT 仅 1.2~3.6s，付费上游 14~24s）。
	// 原 zero=15000 让 P99 直接归零 -> 业务分仅 10.5 -> 健康分被压到 63，
	// 把「上游算力慢」误报成「本站不健康」。新阈值让 P99 落在 ramp 段得非零分，
	// 而真正的劣化（TTFT 超 2 分钟）仍归零。
	opsTTFTCurve = opsBusinessCurve{good: 3000.0, bad: 10000.0, floorScore: 30.0, zero: 120000.0}
)

// opsBusinessSubScore evaluates v against curve, returning a 0-100 sub-score.
func opsBusinessSubScore(v float64, curve opsBusinessCurve) float64 {
	if v <= curve.good {
		return 100
	}
	if v <= curve.bad {
		return 100 - (v-curve.good)/(curve.bad-curve.good)*(100-curve.floorScore)
	}
	if v <= curve.zero {
		return curve.floorScore * (curve.zero - v) / (curve.zero - curve.bad)
	}
	return 0
}

// computeInfraHealth calculates infrastructure health score (0-100)
// Components: Storage (40%) + Compute Resources (30%) + Background Jobs (30%)
func computeInfraHealth(now time.Time, overview *OpsDashboardOverview) float64 {
	// Storage score: DB critical, Redis less critical
	storageScore := 100.0
	if overview.SystemMetrics != nil {
		if overview.SystemMetrics.DBOK != nil && !*overview.SystemMetrics.DBOK {
			storageScore = 0 // DB failure is critical
		} else if overview.SystemMetrics.RedisOK != nil && !*overview.SystemMetrics.RedisOK {
			storageScore = 50 // Redis failure is degraded but not critical
		}
	}

	// Compute resources score: CPU + Memory
	computeScore := 100.0
	if overview.SystemMetrics != nil {
		cpuScore := 100.0
		if overview.SystemMetrics.CPUUsagePercent != nil {
			cpuPct := clampFloat64(*overview.SystemMetrics.CPUUsagePercent, 0, 100)
			if cpuPct > 80 {
				if cpuPct <= 100 {
					cpuScore = (100 - cpuPct) / 20 * 100
				} else {
					cpuScore = 0
				}
			}
		}

		memScore := 100.0
		if overview.SystemMetrics.MemoryUsagePercent != nil {
			memPct := clampFloat64(*overview.SystemMetrics.MemoryUsagePercent, 0, 100)
			if memPct > 85 {
				if memPct <= 100 {
					memScore = (100 - memPct) / 15 * 100
				} else {
					memScore = 0
				}
			}
		}

		computeScore = (cpuScore + memScore) / 2
	}

	// Background jobs score
	jobScore := 100.0
	failedJobs := 0
	totalJobs := 0
	for _, hb := range overview.JobHeartbeats {
		if hb == nil {
			continue
		}
		totalJobs++
		if hb.LastErrorAt != nil && (hb.LastSuccessAt == nil || hb.LastErrorAt.After(*hb.LastSuccessAt)) {
			failedJobs++
		} else if hb.LastSuccessAt != nil && now.Sub(*hb.LastSuccessAt) > opsJobStaleAfter(hb.JobName) {
			failedJobs++
		}
	}
	if totalJobs > 0 && failedJobs > 0 {
		jobScore = (1 - float64(failedJobs)/float64(totalJobs)) * 100
	}

	// Weighted combination
	return storageScore*0.4 + computeScore*0.3 + jobScore*0.3
}

func clampFloat64(v float64, min float64, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
