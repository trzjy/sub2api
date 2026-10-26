//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestComputeDashboardHealthScore_IdleReturns100(t *testing.T) {
	t.Parallel()

	score := computeDashboardHealthScore(time.Now().UTC(), &OpsDashboardOverview{})
	require.Equal(t, 100, score)
}

// TestOpsTTFTCurve_NewThresholds pins the 2026-10-27 TTFT curve adjustment
// (good=3000 / bad=10000 / zero=120000):
//
// 本站是缓存密集型长上下文业务，TTFT 主要等于上游预填充时间（生产实测 1h 窗：
// TTFT 占总耗时 68.4%；分位 P50=14684 / P90=34610 / P95=44308 / P99=69042ms）。
// 旧曲线 zero=15000 让 P99 直接归零，业务分仅 10.5，把「上游算力慢」误报成
// 「本站不健康」。新阈值下：
//   - 0ms 仍是满分（TTFT 不是问题）；
//   - 69042ms（生产实测 P99）落在 ramp 段 -> 非零分，反映「慢但未失控」；
//   - 200000ms（真正的劣化）仍然归零，保留告警能力。
func TestOpsTTFTCurve_NewThresholds(t *testing.T) {
	t.Parallel()

	ttftScore := func(p99ms int64) float64 {
		return opsBusinessSubScore(float64(p99ms), opsTTFTCurve)
	}

	require.Equal(t, 100.0, ttftScore(0), "TTFT 0ms 应为满分")
	require.Equal(t, 100.0, ttftScore(3000), "TTFT 正好 good=3000 应为满分")

	// 生产实测 P99：落在 ramp 段（bad=10000）-> 非零但明显低于满分。
	p99 := ttftScore(69042)
	require.Greater(t, p99, 0.0, "生产实测 P99 69042ms 不得再归零")
	require.Less(t, p99, 100.0, "生产实测 P99 不应满分（上游确实慢）")

	// 真正的劣化：超过 zero=120000 归零，保留告警能力。
	require.Equal(t, 0.0, ttftScore(120000), "TTFT 达 zero 阈值应归零")
	require.Equal(t, 0.0, ttftScore(200000), "TTFT 严重劣化应归零")

	// 旧阈值下的 P99=69042 必须已被新曲线救回（原 zero=15000 会归零）。
	require.NotEqual(t, 0.0, p99, "旧曲线归零的行为不应保留")
}

// TestOpsTTFTCurve_BusinessScoreNotZeroAtProductionP99 pins the end-to-end effect:
// 生产实测 P99=69042ms 时业务分不再是 10.5（近乎归零），而是可区分的中间值。
func TestOpsTTFTCurve_BusinessScoreNotZeroAtProductionP99(t *testing.T) {
	t.Parallel()

	ov := &OpsDashboardOverview{
		SLA:               0.99,
		ErrorRate:         0,
		UpstreamErrorRate: 0,
		TTFT:              OpsPercentiles{P99: intPtr(69042)},
	}
	score := computeBusinessHealth(ov)
	require.Greater(t, score, 30.0, "生产实测 P99 下业务分不应塌到接近 0")
	require.Less(t, score, 100.0, "业务分仍应反映上游偏慢")
}

func TestComputeDashboardHealthScore_DegradesOnBadSignals(t *testing.T) {
	t.Parallel()

	ov := &OpsDashboardOverview{
		RequestCountTotal: 100,
		RequestCountSLA:   100,
		SuccessCount:      90,
		ErrorCountTotal:   10,
		ErrorCountSLA:     10,

		SLA:               0.90,
		ErrorRate:         0.10,
		UpstreamErrorRate: 0.08,

		Duration: OpsPercentiles{P99: intPtr(20_000)},
		TTFT:     OpsPercentiles{P99: intPtr(2_000)},

		SystemMetrics: &OpsSystemMetricsSnapshot{
			DBOK:                  boolPtr(false),
			RedisOK:               boolPtr(false),
			CPUUsagePercent:       float64Ptr(98.0),
			MemoryUsagePercent:    float64Ptr(97.0),
			DBConnWaiting:         intPtr(3),
			ConcurrencyQueueDepth: intPtr(10),
		},
		JobHeartbeats: []*OpsJobHeartbeat{
			{
				JobName:     "job-a",
				LastErrorAt: healthTimePtr(time.Now().UTC().Add(-1 * time.Minute)),
				LastError:   stringPtr("boom"),
			},
		},
	}

	score := computeDashboardHealthScore(time.Now().UTC(), ov)
	require.Less(t, score, 80)
	require.GreaterOrEqual(t, score, 0)
}

func TestComputeDashboardHealthScore_Comprehensive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		overview *OpsDashboardOverview
		wantMin  int
		wantMax  int
	}{
		{
			name:     "nil overview returns 0",
			overview: nil,
			wantMin:  0,
			wantMax:  0,
		},
		{
			name: "perfect health",
			overview: &OpsDashboardOverview{
				RequestCountTotal: 1000,
				RequestCountSLA:   1000,
				SLA:               1.0,
				ErrorRate:         0,
				UpstreamErrorRate: 0,
				Duration:          OpsPercentiles{P99: intPtr(500)},
				TTFT:              OpsPercentiles{P99: intPtr(100)},
				SystemMetrics: &OpsSystemMetricsSnapshot{
					DBOK:               boolPtr(true),
					RedisOK:            boolPtr(true),
					CPUUsagePercent:    float64Ptr(30),
					MemoryUsagePercent: float64Ptr(40),
				},
			},
			wantMin: 100,
			wantMax: 100,
		},
		{
			name: "good health - SLA 99.8%",
			overview: &OpsDashboardOverview{
				RequestCountTotal: 1000,
				RequestCountSLA:   1000,
				SLA:               0.998,
				ErrorRate:         0.003,
				UpstreamErrorRate: 0.001,
				Duration:          OpsPercentiles{P99: intPtr(800)},
				TTFT:              OpsPercentiles{P99: intPtr(200)},
				SystemMetrics: &OpsSystemMetricsSnapshot{
					DBOK:               boolPtr(true),
					RedisOK:            boolPtr(true),
					CPUUsagePercent:    float64Ptr(50),
					MemoryUsagePercent: float64Ptr(60),
				},
			},
			wantMin: 95,
			wantMax: 100,
		},
		{
			name: "medium health - SLA 96%",
			overview: &OpsDashboardOverview{
				RequestCountTotal: 1000,
				RequestCountSLA:   1000,
				SLA:               0.96,
				ErrorRate:         0.02,
				UpstreamErrorRate: 0.01,
				Duration:          OpsPercentiles{P99: intPtr(3000)},
				TTFT:              OpsPercentiles{P99: intPtr(600)},
				SystemMetrics: &OpsSystemMetricsSnapshot{
					DBOK:               boolPtr(true),
					RedisOK:            boolPtr(true),
					CPUUsagePercent:    float64Ptr(70),
					MemoryUsagePercent: float64Ptr(75),
				},
			},
			wantMin: 96,
			wantMax: 97,
		},
		{
			name: "DB failure",
			overview: &OpsDashboardOverview{
				RequestCountTotal: 1000,
				RequestCountSLA:   1000,
				SLA:               0.995,
				ErrorRate:         0,
				UpstreamErrorRate: 0,
				Duration:          OpsPercentiles{P99: intPtr(500)},
				SystemMetrics: &OpsSystemMetricsSnapshot{
					DBOK:               boolPtr(false),
					RedisOK:            boolPtr(true),
					CPUUsagePercent:    float64Ptr(30),
					MemoryUsagePercent: float64Ptr(40),
				},
			},
			wantMin: 70,
			wantMax: 90,
		},
		{
			name: "Redis failure",
			overview: &OpsDashboardOverview{
				RequestCountTotal: 1000,
				RequestCountSLA:   1000,
				SLA:               0.995,
				ErrorRate:         0,
				UpstreamErrorRate: 0,
				Duration:          OpsPercentiles{P99: intPtr(500)},
				SystemMetrics: &OpsSystemMetricsSnapshot{
					DBOK:               boolPtr(true),
					RedisOK:            boolPtr(false),
					CPUUsagePercent:    float64Ptr(30),
					MemoryUsagePercent: float64Ptr(40),
				},
			},
			wantMin: 85,
			wantMax: 95,
		},
		{
			name: "high CPU usage",
			overview: &OpsDashboardOverview{
				RequestCountTotal: 1000,
				RequestCountSLA:   1000,
				SLA:               0.995,
				ErrorRate:         0,
				UpstreamErrorRate: 0,
				Duration:          OpsPercentiles{P99: intPtr(500)},
				SystemMetrics: &OpsSystemMetricsSnapshot{
					DBOK:               boolPtr(true),
					RedisOK:            boolPtr(true),
					CPUUsagePercent:    float64Ptr(95),
					MemoryUsagePercent: float64Ptr(40),
				},
			},
			wantMin: 85,
			wantMax: 100,
		},
		{
			name: "combined failures - business degraded + infra healthy",
			overview: &OpsDashboardOverview{
				RequestCountTotal: 1000,
				RequestCountSLA:   1000,
				SLA:               0.90,
				ErrorRate:         0.05,
				UpstreamErrorRate: 0.02,
				Duration:          OpsPercentiles{P99: intPtr(10000)},
				SystemMetrics: &OpsSystemMetricsSnapshot{
					DBOK:               boolPtr(true),
					RedisOK:            boolPtr(true),
					CPUUsagePercent:    float64Ptr(20),
					MemoryUsagePercent: float64Ptr(30),
				},
			},
			wantMin: 89,
			wantMax: 90,
		},
		{
			name: "combined failures - business healthy + infra degraded",
			overview: &OpsDashboardOverview{
				RequestCountTotal: 1000,
				RequestCountSLA:   1000,
				SLA:               0.998,
				ErrorRate:         0.001,
				UpstreamErrorRate: 0,
				Duration:          OpsPercentiles{P99: intPtr(600)},
				SystemMetrics: &OpsSystemMetricsSnapshot{
					DBOK:               boolPtr(false),
					RedisOK:            boolPtr(false),
					CPUUsagePercent:    float64Ptr(95),
					MemoryUsagePercent: float64Ptr(95),
				},
			},
			wantMin: 70,
			wantMax: 90,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score := computeDashboardHealthScore(time.Now().UTC(), tt.overview)
			require.GreaterOrEqual(t, score, tt.wantMin, "score should be >= %d", tt.wantMin)
			require.LessOrEqual(t, score, tt.wantMax, "score should be <= %d", tt.wantMax)
			require.GreaterOrEqual(t, score, 0, "score must be >= 0")
			require.LessOrEqual(t, score, 100, "score must be <= 100")
		})
	}
}

func TestComputeBusinessHealth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		overview *OpsDashboardOverview
		wantMin  float64
		wantMax  float64
	}{
		{
			name: "perfect metrics",
			overview: &OpsDashboardOverview{
				SLA:               1.0,
				ErrorRate:         0,
				UpstreamErrorRate: 0,
				Duration:          OpsPercentiles{P99: intPtr(500)},
			},
			wantMin: 100,
			wantMax: 100,
		},
		{
			name: "SLA boundary 99.5%",
			overview: &OpsDashboardOverview{
				SLA:               0.995,
				ErrorRate:         0,
				UpstreamErrorRate: 0,
				Duration:          OpsPercentiles{P99: intPtr(500)},
			},
			wantMin: 100,
			wantMax: 100,
		},
		{
			name: "SLA boundary 95%",
			overview: &OpsDashboardOverview{
				SLA:               0.95,
				ErrorRate:         0,
				UpstreamErrorRate: 0,
				Duration:          OpsPercentiles{P99: intPtr(500)},
			},
			wantMin: 100,
			wantMax: 100,
		},
		{
			name: "error rate boundary 1%",
			overview: &OpsDashboardOverview{
				SLA:               0.99,
				ErrorRate:         0.01,
				UpstreamErrorRate: 0,
				Duration:          OpsPercentiles{P99: intPtr(500)},
			},
			wantMin: 100,
			wantMax: 100,
		},
		{
			// 5% sits in the ramp segment (good=1%, bad=10%): errorScore=68.89,
			// so business = 68.89*0.5 + 100*0.5 = 84.44.
			name: "error rate 5%",
			overview: &OpsDashboardOverview{
				SLA:               0.95,
				ErrorRate:         0.05,
				UpstreamErrorRate: 0,
				Duration:          OpsPercentiles{P99: intPtr(500)},
			},
			wantMin: 84,
			wantMax: 85,
		},
		{
			// 2000ms 在 good=3000ms 之内 -> ttftScore=100，business = 100。
			// （旧曲线 good=1000/bad=3000 时此处为 ramp 段 65 分，改曲线后已更新。）
			name: "TTFT boundary 2s",
			overview: &OpsDashboardOverview{
				SLA:               0.99,
				ErrorRate:         0,
				UpstreamErrorRate: 0,
				TTFT:              OpsPercentiles{P99: intPtr(2000)},
			},
			wantMin: 100,
			wantMax: 100,
		},
		{
			name: "upstream error dominates",
			overview: &OpsDashboardOverview{
				SLA:               0.995,
				ErrorRate:         0.001,
				UpstreamErrorRate: 0.03,
				Duration:          OpsPercentiles{P99: intPtr(500)},
			},
			wantMin: 92,
			wantMax: 93,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score := computeBusinessHealth(tt.overview)
			require.GreaterOrEqual(t, score, tt.wantMin, "score should be >= %.1f", tt.wantMin)
			require.LessOrEqual(t, score, tt.wantMax, "score should be <= %.1f", tt.wantMax)
			require.GreaterOrEqual(t, score, 0.0, "score must be >= 0")
			require.LessOrEqual(t, score, 100.0, "score must be <= 100")
		})
	}
}

func TestComputeInfraHealth(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()

	tests := []struct {
		name     string
		overview *OpsDashboardOverview
		wantMin  float64
		wantMax  float64
	}{
		{
			name: "all infrastructure healthy",
			overview: &OpsDashboardOverview{
				RequestCountTotal: 1000,
				SystemMetrics: &OpsSystemMetricsSnapshot{
					DBOK:               boolPtr(true),
					RedisOK:            boolPtr(true),
					CPUUsagePercent:    float64Ptr(30),
					MemoryUsagePercent: float64Ptr(40),
				},
			},
			wantMin: 100,
			wantMax: 100,
		},
		{
			name: "DB down",
			overview: &OpsDashboardOverview{
				RequestCountTotal: 1000,
				SystemMetrics: &OpsSystemMetricsSnapshot{
					DBOK:               boolPtr(false),
					RedisOK:            boolPtr(true),
					CPUUsagePercent:    float64Ptr(30),
					MemoryUsagePercent: float64Ptr(40),
				},
			},
			wantMin: 50,
			wantMax: 70,
		},
		{
			name: "Redis down",
			overview: &OpsDashboardOverview{
				RequestCountTotal: 1000,
				SystemMetrics: &OpsSystemMetricsSnapshot{
					DBOK:               boolPtr(true),
					RedisOK:            boolPtr(false),
					CPUUsagePercent:    float64Ptr(30),
					MemoryUsagePercent: float64Ptr(40),
				},
			},
			wantMin: 80,
			wantMax: 95,
		},
		{
			name: "CPU at 90%",
			overview: &OpsDashboardOverview{
				RequestCountTotal: 1000,
				SystemMetrics: &OpsSystemMetricsSnapshot{
					DBOK:               boolPtr(true),
					RedisOK:            boolPtr(true),
					CPUUsagePercent:    float64Ptr(90),
					MemoryUsagePercent: float64Ptr(40),
				},
			},
			wantMin: 85,
			wantMax: 95,
		},
		{
			name: "failed background job",
			overview: &OpsDashboardOverview{
				RequestCountTotal: 1000,
				SystemMetrics: &OpsSystemMetricsSnapshot{
					DBOK:               boolPtr(true),
					RedisOK:            boolPtr(true),
					CPUUsagePercent:    float64Ptr(30),
					MemoryUsagePercent: float64Ptr(40),
				},
				JobHeartbeats: []*OpsJobHeartbeat{
					{
						JobName:     "test-job",
						LastErrorAt: &now,
					},
				},
			},
			wantMin: 70,
			wantMax: 90,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score := computeInfraHealth(now, tt.overview)
			require.GreaterOrEqual(t, score, tt.wantMin, "score should be >= %.1f", tt.wantMin)
			require.LessOrEqual(t, score, tt.wantMax, "score should be <= %.1f", tt.wantMax)
			require.GreaterOrEqual(t, score, 0.0, "score must be >= 0")
			require.LessOrEqual(t, score, 100.0, "score must be <= 100")
		})
	}
}

func healthTimePtr(v time.Time) *time.Time { return &v }

func stringPtr(v string) *string { return &v }
