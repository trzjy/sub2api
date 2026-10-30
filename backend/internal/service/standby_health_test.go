//go:build unit

package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func standbyFloat(v float64) *float64 { return &v }

// standbyHealthyInput 三信号全部健康：TTFT = 基线、0 限流、账号全可调度。
func standbyHealthyInput() StandbyHealthInput {
	return StandbyHealthInput{
		Model:                 "deepseek-v4-pro",
		TTFTBaselineP50Ms:     standbyFloat(10000),
		TTFTBaselineDays:      14,
		TTFTCurrentP50Ms:      standbyFloat(10000),
		TTFTCurrentSamples:    40,
		UpstreamThrottleCount: 0,
		UpstreamRequestCount:  100,
		SchedulableRatio:      standbyFloat(1.0),
	}
}

func TestStandbyEvaluateMainChannelHealth_HealthySymmetricBaseline(t *testing.T) {
	in := standbyHealthyInput()
	require.Equal(t, StandbyVerdictHealthy, EvaluateMainChannelHealth(in))
}

// OR 进入对称性：任一信号越线即 Degraded，其余健康不抵消。
func TestStandbyEvaluateMainChannelHealth_DegradedOnAnySignal(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*StandbyHealthInput)
	}{
		{
			name: "ttft 2.5x",
			mutate: func(in *StandbyHealthInput) {
				in.TTFTCurrentP50Ms = standbyFloat(25000)
			},
		},
		{
			name: "upstream throttle rate 0.30",
			mutate: func(in *StandbyHealthInput) {
				in.UpstreamThrottleCount = 30
			},
		},
		{
			name: "schedulable ratio 0.40",
			mutate: func(in *StandbyHealthInput) {
				in.SchedulableRatio = standbyFloat(0.40)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := standbyHealthyInput()
			tc.mutate(&in)
			require.Equal(t, StandbyVerdictDegraded, EvaluateMainChannelHealth(in))
		})
	}
}

// 边界：恰好 2.0× 不进入（L4 用严格大于），落在滞回带 → 不判定。
func TestStandbyEvaluateMainChannelHealth_EnterBoundary(t *testing.T) {
	in := standbyHealthyInput()
	in.TTFTCurrentP50Ms = standbyFloat(20000)
	require.Equal(t, StandbyVerdictInconclusive, EvaluateMainChannelHealth(in))

	in.TTFTCurrentP50Ms = standbyFloat(20000.001)
	require.Equal(t, StandbyVerdictDegraded, EvaluateMainChannelHealth(in))
}

// 边界：恰好 1.35× 不退出（L4 用严格小于），落在滞回带 → 不判定。
func TestStandbyEvaluateMainChannelHealth_ExitBoundary(t *testing.T) {
	in := standbyHealthyInput()
	in.TTFTCurrentP50Ms = standbyFloat(13500)
	require.Equal(t, StandbyVerdictInconclusive, EvaluateMainChannelHealth(in))

	in.TTFTCurrentP50Ms = standbyFloat(13499.999)
	require.Equal(t, StandbyVerdictHealthy, EvaluateMainChannelHealth(in))
}

// 滞回带内即使故障信号健康也不得判 Healthy：否则会在进入/退出之间抖动。
func TestStandbyEvaluateMainChannelHealth_HysteresisBandBlocksHealthy(t *testing.T) {
	in := standbyHealthyInput()
	in.TTFTCurrentP50Ms = standbyFloat(17000) // 1.7× 基线
	require.Equal(t, StandbyVerdictInconclusive, EvaluateMainChannelHealth(in))
}

// OR 与滞回带的优先级：TTFT 落在滞回带时，另一信号的退化仍必须判 Degraded，
// 不能被滞回带的 Inconclusive 掩盖（单遍遍历实现的经典丢判定）。
func TestStandbyEvaluateMainChannelHealth_DegradedNotMaskedByAmbiguous(t *testing.T) {
	in := standbyHealthyInput()
	in.TTFTCurrentP50Ms = standbyFloat(17000) // 1.7× → 滞回带
	in.UpstreamThrottleCount = 30             // 0.30 → 退化
	require.Equal(t, StandbyVerdictDegraded, EvaluateMainChannelHealth(in))

	in = standbyHealthyInput()
	in.TTFTCurrentP50Ms = standbyFloat(17000)
	in.SchedulableRatio = standbyFloat(0.30)
	require.Equal(t, StandbyVerdictDegraded, EvaluateMainChannelHealth(in))
}

// 样本不足：TTFT 样本 < M 时延迟信号不可用，单凭延迟越线不得判 Degraded；
// 判定只由剩余可用信号（此处全健康）给出，与 L16 的故障侧口径对称。
func TestStandbyEvaluateMainChannelHealth_InsufficientTTFTSamples(t *testing.T) {
	in := standbyHealthyInput()
	in.TTFTCurrentSamples = StandbyTTFTMinSamples - 1
	in.TTFTCurrentP50Ms = standbyFloat(90000)
	require.Equal(t, StandbyVerdictHealthy, EvaluateMainChannelHealth(in))

	in.TTFTCurrentSamples = StandbyTTFTMinSamples
	require.Equal(t, StandbyVerdictDegraded, EvaluateMainChannelHealth(in))
}

// 基线天数不足视作基线缺失，同样不得凭延迟判 Degraded。
func TestStandbyEvaluateMainChannelHealth_InsufficientBaselineDays(t *testing.T) {
	in := standbyHealthyInput()
	in.TTFTBaselineDays = StandbyBaselineMinDays - 1
	in.TTFTCurrentP50Ms = standbyFloat(90000)
	require.Equal(t, StandbyVerdictHealthy, EvaluateMainChannelHealth(in))

	// 同样场景下故障信号越线仍可进入（L16）。
	in.UpstreamThrottleCount = 30
	require.Equal(t, StandbyVerdictDegraded, EvaluateMainChannelHealth(in))
}

// L16：低流量夜间 TTFT 样本不足，但 429/529 率越线 → 仍判 Degraded。
func TestStandbyEvaluateMainChannelHealth_LowTrafficNightFaultStillDegrades(t *testing.T) {
	in := StandbyHealthInput{
		Model:                 "kimi-k3",
		TTFTBaselineP50Ms:     standbyFloat(8000),
		TTFTBaselineDays:      14,
		TTFTCurrentP50Ms:      standbyFloat(9000),
		TTFTCurrentSamples:    5, // < M
		UpstreamThrottleCount: 12,
		UpstreamRequestCount:  40, // >= StandbyFaultMinRequests
		SchedulableRatio:      standbyFloat(1.0),
	}
	require.Equal(t, StandbyVerdictDegraded, EvaluateMainChannelHealth(in))
}

// L16 的另一面：故障信号也有独立最小样本，样例不足时不得下结论。
func TestStandbyEvaluateMainChannelHealth_FaultSampleGate(t *testing.T) {
	in := standbyHealthyInput()
	in.UpstreamThrottleCount = StandbyFaultMinRequests - 1
	in.UpstreamRequestCount = StandbyFaultMinRequests - 1 // 100% 但样本不足
	in.TTFTCurrentSamples = 0
	in.TTFTCurrentP50Ms = nil
	in.SchedulableRatio = nil
	require.Equal(t, StandbyVerdictInconclusive, EvaluateMainChannelHealth(in))

	in.UpstreamThrottleCount = 4
	in.UpstreamRequestCount = StandbyFaultMinRequests // 0.20 > 0.15
	require.Equal(t, StandbyVerdictDegraded, EvaluateMainChannelHealth(in))
}

// 故障类信号的滞回带同样阻断 Healthy。
func TestStandbyEvaluateMainChannelHealth_ThrottleBandBlocksHealthy(t *testing.T) {
	in := standbyHealthyInput()
	in.UpstreamThrottleCount = 10 // 0.10，介于 0.05 与 0.15 之间
	require.Equal(t, StandbyVerdictInconclusive, EvaluateMainChannelHealth(in))
}

// 无任何可用信号 → Inconclusive，不迁移。
func TestStandbyEvaluateMainChannelHealth_NoSignal(t *testing.T) {
	require.Equal(t, StandbyVerdictInconclusive, EvaluateMainChannelHealth(StandbyHealthInput{Model: "deepseek-v4-pro"}))
}

// 亏损保护：可用信号全健康时，缺失信号不阻塞 Healthy（与 L16 同一口径）。
func TestStandbyEvaluateMainChannelHealth_MissingSignalDoesNotBlockHealthy(t *testing.T) {
	in := StandbyHealthInput{
		Model:                 "glm-5.3",
		UpstreamThrottleCount: 0,
		UpstreamRequestCount:  60,
		SchedulableRatio:      standbyFloat(1.0),
	}
	require.Equal(t, StandbyVerdictHealthy, EvaluateMainChannelHealth(in))
}

func TestStandbyVerdictString(t *testing.T) {
	require.Equal(t, "inconclusive", StandbyVerdictInconclusive.String())
	require.Equal(t, "healthy", StandbyVerdictHealthy.String())
	require.Equal(t, "degraded", StandbyVerdictDegraded.String())
	require.True(t, strings.HasPrefix(StandbyPhaseOpen.String(), "open"))
}
