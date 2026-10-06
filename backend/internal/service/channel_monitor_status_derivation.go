package service

import (
	"time"
)

// 本文件实现 D3（渠道侧状态推导与存量收敛）的纯函数层。
// 设计约束（方案 account-channel-freshness v20 工作项 2/3，权威规则 A–G）：
//   - 渠道状态由「计算即读」从既有信号实时推导，不新增数据库表 / ent 迁移 / schema 变更。
//   - 聚合优先级固定：error > failed > degraded > operational。
//   - 四值状态枚举不变（operational/degraded/failed/error）。
//   - 账号侧持久事实（D2 域）经注入接口传入，本文件不实现 D2 逻辑。

// channelFreshnessBaselineDefault 渠道 freshness 阈值下限默认值。
// 对齐工作项 3「基线默认 = 2×对应探测周期（账号侧默认 60s → 基线 120s）」，
// 渠道无独立探测周期，取 120s 作为缺省基线；调用方可用 ConfigBaseline 覆盖。
const channelFreshnessBaselineDefault = 120 * time.Second

// ChannelObservation 渠道观测（瞬时事件）：来自 per-model latest 的权威分类结果。
// 每条携带 Status 与 ObservedAt（事件时间）；NoData 表示无数据信号（横线），
// 不改状态、不推进观测时间，仅记录尝试时间。
type ChannelObservation struct {
	Status     string
	ObservedAt time.Time
	NoData     bool
}

// AccountSideAnomaly 账号侧持久事实（持续到对应恢复事件为止）。
// Kind 仅用于语义记录；Active 表示当前有效（由调用方 D2 域判定）。
// 转换表（方案工作项 2）：
//   - temp_unschedulable / circuit_breaker → degraded（聚合，不夸大为 failed/error）
//   - model_rate_limited / free_tier_429  → no-op（不降渠道状态，只进 D2 的账号侧 SSOT）
type AccountSideAnomaly struct {
	Kind   string // "temp_unschedulable" | "circuit_breaker" | "model_rate_limited" | "free_tier_429"
	Active bool
}

// ChannelDerivationInput 推导渠道状态的完整输入（计算即读，纯函数）。
type ChannelDerivationInput struct {
	// Observations 渠道观测（瞬时事件）。
	Observations []ChannelObservation
	// AccountSideAnomalies 账号侧持久事实（D2 域注入）。
	AccountSideAnomalies []AccountSideAnomaly
	// ConfigBaseline 渠道 freshness 阈值下限（配置基线）。
	ConfigBaseline time.Duration
	// Now 当前推导时刻；账号侧异常变更、尝试时间以该时刻推进。
	Now time.Time
}

// ChannelStatusDerivation 渠道状态推导结果。
// Status 为空字符串表示「无权威观测」空状态（无 latest 行或全部 NoData 且无账号侧事实）：
// 前端既有空态分支据此渲染横线，绝不把真实无数据显示为 operational。
type ChannelStatusDerivation struct {
	Status        string
	ObservedAt    time.Time // 权威观测时间（仅由可分类信号推进；无数据/attempt 不刷新）
	LastAttemptAt time.Time // 最近一次尝试时间（含无数据信号）
	AlertEligible bool      // 是否应产生陈旧告警
}

// channelStatusRank 状态优先级权重：error > failed > degraded > operational。
func channelStatusRank(s string) int {
	switch s {
	case MonitorStatusError:
		return 4
	case MonitorStatusFailed:
		return 3
	case MonitorStatusDegraded:
		return 2
	case MonitorStatusOperational:
		return 1
	default:
		return 0
	}
}

// worseStatus 返回优先级更高的状态（error > failed > degraded > operational）。
func worseStatus(a, b string) string {
	if channelStatusRank(a) >= channelStatusRank(b) {
		return a
	}
	return b
}

// DeriveChannelStatus 渠道状态唯一计算者（纯函数）。
// 严格对应方案工作项 2 转换表 + 聚合优先级 + 输入生命周期（C）。
//
// 推导：
//  1. 账号侧持久事实：temp_unschedulable/circuit_breaker → degraded（聚合）；
//     model_rate_limited/free_tier_429 → no-op（不降渠道状态）。
//  2. 渠道观测（瞬时事件）：对**全部** per-model 当前行应用最坏档位聚合
//     （error > failed > degraded > operational），不再跨模型「只留最新一行」。
//     同模型键内新事件替代旧事件由读路径 ListLatestPerModel 保证（每模型当前行），
//     纯函数只做最坏聚合；无数据/空状态不参与。
//  3. 观测时间：仅由可分类权威信号（业务成功、账号侧聚合变更、既有探测可分类结果）
//     按事件时间单调推进，取全部权威观测的最大事件时间；无数据/no-op 只记尝试时间。
//  4. 空状态（#9）：无任何权威观测（无 latest 行或全部 NoData）且无账号侧事实时
//     输出 Status=""（前端横线），不把真实无数据显示为 operational。
func DeriveChannelStatus(in ChannelDerivationInput) ChannelStatusDerivation {
	out := ChannelStatusDerivation{
		// 初始为空状态：只有权威信号（账号侧事实 / 可分类观测）才把它提升为具体档位。
		Status:        "",
		LastAttemptAt: in.Now,
	}

	// 1. 账号侧持久事实（停调/熔断 → degraded；限流/免费档429 → no-op）。
	accountActive := false
	for _, a := range in.AccountSideAnomalies {
		if !a.Active {
			continue
		}
		switch a.Kind {
		case "temp_unschedulable", "circuit_breaker":
			out.Status = worseStatus(out.Status, MonitorStatusDegraded)
			accountActive = true
			// model_rate_limited / free_tier_429 → no-op，不进入状态。
		}
	}

	// 2. 渠道观测（瞬时事件）：对全部 per-model 当前行应用最坏档位聚合。
	//    同模型键内「新事件替代旧事件」由读路径 ListLatestPerModel 保证（每模型当前行），
	//    纯函数只做最坏聚合——模型 A failed（较早）+ 模型 B operational（较晚）不得
	//    把渠道提升为 operational（error > failed > degraded > operational）。
	var maxObservedAt time.Time
	hasAuthoritativeObservation := false
	for i := range in.Observations {
		o := &in.Observations[i]
		if o.NoData || o.Status == "" {
			// 无数据信号：不刷新观测时间、不降状态（只记录尝试时间，上面已设）。
			continue
		}
		hasAuthoritativeObservation = true
		out.Status = worseStatus(out.Status, o.Status)
		if o.ObservedAt.After(maxObservedAt) {
			maxObservedAt = o.ObservedAt
		}
	}
	if !maxObservedAt.IsZero() {
		out.ObservedAt = maxObservedAt
	}

	// 3. 观测时间推进：账号侧异常变更按当前推导时刻（Now）推进；
	//    仅渠道观测时按观测事件时间（上面已设）。
	if accountActive {
		out.ObservedAt = in.Now
	}

	// 4. 无任何权威观测且无账号侧事实 → 空状态（绝不伪装为 operational）。
	if !hasAuthoritativeObservation && !accountActive {
		out.Status = ""
	}

	out.AlertEligible = ChannelAlertEligible(out.Status)
	return out
}

// ComputeChannelFreshnessThreshold 渠道维阈值（工作项 3 公式 F）：
// max(配置基线, 该渠道账号侧最坏上界)；空集合语义固定 = 渠道配置基线。
//
// 空集合（当前无任何账号级/账号+模型异常候选）以 ub <= 0 表示，返回基线，
// 不得退化为零值或不产出阈值。委托 EffectiveFreshnessThreshold（account_freshness_threshold.go）
// 以保证三维阈值「同一公式、同一计算结果」。
func ComputeChannelFreshnessThreshold(configBaseline, accountSideWorstUpperBound time.Duration) time.Duration {
	return EffectiveFreshnessThreshold(configBaseline, accountSideWorstUpperBound)
}

// ChannelAlertEligible 渠道陈旧告警资格（工作项 2 / G）：
// 仅 degraded/failed/error 返回 true；operational 或仅无数据信号的渠道不建陈旧告警
// （零流量场景闭合）。
func ChannelAlertEligible(status string) bool {
	switch status {
	case MonitorStatusDegraded, MonitorStatusFailed, MonitorStatusError:
		return true
	default:
		return false
	}
}

// ChannelAlertClosedOnRecovery 当聚合状态回升（prev 可告警 → next 不可告警）时
// 原子关闭已有渠道告警。仅用于权威恢复事件使聚合状态回升的场景。
func ChannelAlertClosedOnRecovery(prev, next string) bool {
	return ChannelAlertEligible(prev) && !ChannelAlertEligible(next)
}

// LegacyChannelStatusRecord 存量旧 per-model/渠道状态记录（升级前已持久化）。
type LegacyChannelStatusRecord struct {
	MonitorID int64
	Model     string
	Status    string
	// EvidenceNoData：既有来源可证明该旧 status 是「无数据」产物
	// （如对应 quota 抓取无数据、replace 空文本历史等）。D3 前 NoData 未持久化，
	// 故历史数据多数无此证据 → 不可判定。
	EvidenceNoData bool
}

// LegacyMigrationResult 一次性迁移结果。
type LegacyMigrationResult struct {
	Cleared   []LegacyChannelStatusRecord // 被清除并按新规则重算的
	Preserved []LegacyChannelStatusRecord // 原样保留（不可判定或真实故障或非 failed）
	Judgable  bool                        // false = 存在不可判定来源，按完成门禁不得声称完成
}

// MigrateLegacyChannelFailed 存量旧 failed 一次性迁移（方案 E / 完成门禁模式）。
//
// 规则：
//   - 非 failed 状态（operational/degraded/error）不属本次收敛范围，原样保留。
//   - 可判定为无数据产物的旧 failed（EvidenceNoData=true）→ 清除并按新规则重算（Cleared）。
//   - 不可判定来源（EvidenceNoData=false）→ 原样保留并登记，且整体 judgable=false；
//     任何分支都不得启发式批量清除真实故障记录。
func MigrateLegacyChannelFailed(records []LegacyChannelStatusRecord) LegacyMigrationResult {
	res := LegacyMigrationResult{Judgable: true}
	for _, r := range records {
		if r.Status != MonitorStatusFailed {
			res.Preserved = append(res.Preserved, r)
			continue
		}
		if r.EvidenceNoData {
			res.Cleared = append(res.Cleared, r)
		} else {
			// 不可判定：保守保留，触发完成门禁。
			res.Preserved = append(res.Preserved, r)
			res.Judgable = false
		}
	}
	return res
}
