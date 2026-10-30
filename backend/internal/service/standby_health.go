package service

import "time"

// 主力渠道延迟/故障自适应备用切换 · 感知与决策层（第 1 片）。
//
// 本文件的判定口径与 docs-local/main-channel-latency-standby-switch-plan.md
// 的 L1–L16 对齐：
//   - L1  延迟口径用 TTFT p50（不吃整单 duration）；
//   - L2  基线窗口为多日同小时（hour-of-day）中位数；
//   - L4  进入 > 2.0× 基线，退出 < 1.35× 基线（滞回带内既不进也不出）；
//   - L11 同一 evaluate、同一信号集；进入 OR、退出 AND；样本不足不判定；
//   - L16 故障信号（上游 429/529 率、可调度占比）使用独立最小样本，
//     低流量夜间 TTFT 样本不足时仍可由故障信号判定 Degraded。
//
// 本文件只做纯逻辑：不查库、不写库、不碰调度资格。
const (
	// StandbyTTFTMinSamples 延迟信号的最小样本数（L11 的 M）。
	StandbyTTFTMinSamples = 30
	// StandbyFaultMinRequests 故障类信号的最小窗口请求数（L16）。
	StandbyFaultMinRequests = 20

	// StandbyWindow 当前值窗口。
	StandbyWindow = 5 * time.Minute
	// StandbyBaselineDays 基线回看天数。
	StandbyBaselineDays = 15
	// StandbyBaselineExcludeWindow 基线排除的最近时长（当日异常不污染基线）。
	StandbyBaselineExcludeWindow = 24 * time.Hour
	// StandbyBaselineMinDays 基线最少可得天数，不足则该信号不可用。
	StandbyBaselineMinDays = 3
	// StandbyConfirmDuration 进入/退出均需连续满足的确认时长（L5）。
	StandbyConfirmDuration = 5 * time.Minute

	// StandbyEnterTTFTMultiplier 进入线：TTFT p50 > 2.0 × 基线（L4）。
	StandbyEnterTTFTMultiplier = 2.0
	// StandbyExitTTFTMultiplier 退出线：TTFT p50 < 1.35 × 基线（L4）。
	StandbyExitTTFTMultiplier = 1.35

	// StandbyEnterUpstreamThrottleRate 上游 429/529 率进入阈值。
	// 方案 §7 待用生产基线校准，此处取暂定值。
	StandbyEnterUpstreamThrottleRate = 0.15
	// StandbyExitUpstreamThrottleRate 上游 429/529 率恢复阈值（低于才算恢复）。
	StandbyExitUpstreamThrottleRate = 0.05

	// StandbyEnterSchedulableRatio 可调度占比进入阈值：低于该值视为骤降。
	StandbyEnterSchedulableRatio = 0.50
	// StandbyExitSchedulableRatio 可调度占比恢复阈值：达到该值才算恢复。
	StandbyExitSchedulableRatio = 0.90
)

// StandbyVerdict 是唯一判定结果。Inconclusive 表示"不判定"，不是"健康"也不是"退化"。
type StandbyVerdict int

const (
	// StandbyVerdictInconclusive 样本不足或数据缺失，不产生判定。
	StandbyVerdictInconclusive StandbyVerdict = iota
	// StandbyVerdictHealthy 全部可用信号均在恢复线以内。
	StandbyVerdictHealthy
	// StandbyVerdictDegraded 任一可用信号越过进入线。
	StandbyVerdictDegraded
)

func (v StandbyVerdict) String() string {
	switch v {
	case StandbyVerdictHealthy:
		return "healthy"
	case StandbyVerdictDegraded:
		return "degraded"
	default:
		return "inconclusive"
	}
}

// StandbyHealthInput 是一次判定的完整输入。所有可选值为 nil / 零值即表示该信号缺失：
// 缺失的信号不参与判定，但全部缺失时结果为 Inconclusive。
type StandbyHealthInput struct {
	// Model 是信号键（如 "deepseek-v4-pro" / "kimi-k3" / "glm-5.3"）。
	Model string

	// TTFTBaselineP50Ms 为最近 StandbyBaselineDays 天、排除最近 1 天、同 hour-of-day 的
	// 逐日 TTFT p50 的中位数；TTFTBaselineDays 为参与聚合的天数。
	TTFTBaselineP50Ms *float64
	TTFTBaselineDays  int

	// TTFTCurrentP50Ms 为近 StandbyWindow 的 TTFT p50，TTFTCurrentSamples 为窗口样本数。
	TTFTCurrentP50Ms   *float64
	TTFTCurrentSamples int

	// UpstreamThrottleCount 为窗口内 upstream_status_code ∈ {429, 529} 的错误行数；
	// UpstreamRequestCount 为窗口请求数（成功用量行 + 上述限流错误行，避免 recovered 行双计）。
	UpstreamThrottleCount int
	UpstreamRequestCount  int

	// SchedulableRatio 为该模型对应供给平台下 IsSchedulable()==true 的账号占比。
	SchedulableRatio *float64
}

// standbySignalState 是单信号三态。滞回带内为 ambiguous：既不算健康也不算退化，
// 使判定在带内保持 Inconclusive，避免进入/退出之间抖动。
type standbySignalState int

const (
	standbySignalUnavailable standbySignalState = iota
	standbySignalHealthy
	standbySignalDegraded
	standbySignalAmbiguous
)

func (s standbySignalState) String() string {
	switch s {
	case standbySignalHealthy:
		return "healthy"
	case standbySignalDegraded:
		return "degraded"
	case standbySignalAmbiguous:
		return "ambiguous"
	default:
		return "unavailable"
	}
}

// EvaluateMainChannelHealth 是唯一判定函数（L11）：进入与退出共用同一实现。
//
//	任一会话信号退化                       → Degraded（OR）
//	无退化、无滞回带、且至少一个信号健康   → Healthy（AND：所有可用信号均健康）
//	其余（样本不足 / 数据缺失 / 滞回带内） → Inconclusive
//
// Inconclusive 一律不迁移状态，由状态机层消费。
func EvaluateMainChannelHealth(in StandbyHealthInput) StandbyVerdict {
	states := []standbySignalState{
		standbyTTFTSignal(in),
		standbyUpstreamThrottleSignal(in),
		standbySchedulableSignal(in),
	}

	// OR 进入优先：任一信号退化即 Degraded，不能被其他信号的滞回带掩盖，
	// 因此先扫退化，再扫滞回带（必须两遍，单遍会因遍历顺序丢判定）。
	healthy := 0
	ambiguous := false
	for _, st := range states {
		switch st {
		case standbySignalDegraded:
			return StandbyVerdictDegraded
		case standbySignalAmbiguous:
			ambiguous = true
		case standbySignalHealthy:
			healthy++
		}
	}
	if ambiguous || healthy == 0 {
		return StandbyVerdictInconclusive
	}
	return StandbyVerdictHealthy
}

func standbyTTFTSignal(in StandbyHealthInput) standbySignalState {
	if in.TTFTCurrentP50Ms == nil || in.TTFTBaselineP50Ms == nil {
		return standbySignalUnavailable
	}
	if in.TTFTCurrentSamples < StandbyTTFTMinSamples || in.TTFTBaselineDays < StandbyBaselineMinDays {
		return standbySignalUnavailable
	}
	baseline := *in.TTFTBaselineP50Ms
	if baseline <= 0 {
		return standbySignalUnavailable
	}
	current := *in.TTFTCurrentP50Ms
	switch {
	case current > StandbyEnterTTFTMultiplier*baseline:
		return standbySignalDegraded
	case current < StandbyExitTTFTMultiplier*baseline:
		return standbySignalHealthy
	default:
		return standbySignalAmbiguous
	}
}

func standbyUpstreamThrottleSignal(in StandbyHealthInput) standbySignalState {
	if in.UpstreamRequestCount < StandbyFaultMinRequests {
		return standbySignalUnavailable
	}
	rate := float64(in.UpstreamThrottleCount) / float64(in.UpstreamRequestCount)
	switch {
	case rate > StandbyEnterUpstreamThrottleRate:
		return standbySignalDegraded
	case rate < StandbyExitUpstreamThrottleRate:
		return standbySignalHealthy
	default:
		return standbySignalAmbiguous
	}
}

func standbySchedulableSignal(in StandbyHealthInput) standbySignalState {
	if in.SchedulableRatio == nil {
		return standbySignalUnavailable
	}
	ratio := *in.SchedulableRatio
	switch {
	case ratio < StandbyEnterSchedulableRatio:
		return standbySignalDegraded
	case ratio >= StandbyExitSchedulableRatio:
		return standbySignalHealthy
	default:
		return standbySignalAmbiguous
	}
}
