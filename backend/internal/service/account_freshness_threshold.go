package service

import "time"

// 本文件实现 D4（工作项 3「新鲜度指标」接线）的「三维有效阈值唯一公式」。
// 设计约束（方案 account-channel-freshness v20 工作项 3，验收 6）：
//   - 三维（账号+模型 / 账号级 / 渠道）的有效阈值由**同一公式**产出：
//     max(配置基线, 上界)；上界 <= 0（空集合）时返回配置基线，不得退化为零值。
//   - 展示、告警、验收统一调用该计算，不各算各的。
//   - 账号+模型维与账号级维共用工作项 1 的共享预算总候选上界公式（ComputeProbeUpperBound），
//     仅 modelKey 不同（账号级传 ""）；渠道维复用 channel_monitor_status_derivation.go 的
//     ComputeChannelFreshnessThreshold（已委托到本公式，空集合=基线）。

const (
	// accountModelFreshnessBaseline 账号+模型维阈值下限。
	// 对齐工作项 3「基线默认 = 2×对应探测周期（账号侧默认 60s → 基线 120s）」。
	accountModelFreshnessBaseline = 120 * time.Second
	// accountLevelFreshnessBaseline 账号级维阈值下限（同源，账号侧默认 60s → 120s）。
	accountLevelFreshnessBaseline = 120 * time.Second
)

// EffectiveFreshnessThreshold 是三维有效阈值的唯一公式（工作项 3）：
// max(配置基线, 上界)；上界 <= 0（空集合）时返回配置基线。
// 账号+模型维、账号级维、渠道维（委托调用）全部经由本函数，保证「同一计算结果」。
func EffectiveFreshnessThreshold(configBaseline, upperBound time.Duration) time.Duration {
	if upperBound <= 0 {
		return configBaseline
	}
	if upperBound > configBaseline {
		return upperBound
	}
	return configBaseline
}

// AccountSideUpperBound 由冻结候选总数导出账号侧上界（复用工作项 1 共享预算总候选上界公式）。
// 账号+模型维与账号级维共用同一公式，仅 modelKey 不同（账号级传 "" 由调用方处理）。
// 普通场景（冻结候选总数 ≤ 60）返回 0 预算分钟分支；最坏场景（>60）返回 ceil(总数/60) 分钟
// + 1×轮次周期 + 请求硬超时 + 启动冷却。
func AccountSideUpperBound(frozenCandidateTotal int, roundPeriod, hardTimeout, startupCooldown time.Duration) time.Duration {
	return ComputeProbeUpperBound(frozenCandidateTotal, roundPeriod, hardTimeout, startupCooldown)
}

// AccountModelFreshnessThreshold 账号+模型维有效阈值：max(账号配置基线, 该候选冻结上界)。
// 冻结上界由 probe 调度器在候选进入调度时按当时窗口占用与公平位置冻结（D2/C），不再缩短。
func AccountModelFreshnessThreshold(frozenCandidateTotal int) time.Duration {
	ub := AccountSideUpperBound(frozenCandidateTotal, probeRoundPeriod, probeRequestHardTimeout, probeStartupCooldown)
	return EffectiveFreshnessThreshold(accountModelFreshnessBaseline, ub)
}

// AccountLevelFreshnessThreshold 账号级维有效阈值：max(账号配置基线, 该账号冻结上界)。
// 账号级候选（无模型键）与模型级候选共用同一预算窗口，混合候选 >60 时账号级请求同样合法
// 等待数分钟，漏算预算等待会产生永久误告警——故复用同一上界公式。
func AccountLevelFreshnessThreshold(frozenCandidateTotal int) time.Duration {
	ub := AccountSideUpperBound(frozenCandidateTotal, probeRoundPeriod, probeRequestHardTimeout, probeStartupCooldown)
	return EffectiveFreshnessThreshold(accountLevelFreshnessBaseline, ub)
}

// ChannelFreshnessThreshold 渠道维有效阈值：max(渠道配置基线, 账号侧最坏上界)。
// 空集合（当前无任何账号级/账号+模型异常候选）以 ub <= 0 表示，返回基线，不得退化为零值
// （覆盖「渠道 error 且无账号异常候选」场景）。委托到 EffectiveFreshnessThreshold 保证同一公式。
func ChannelFreshnessThreshold(configBaseline, accountSideWorstUpperBound time.Duration) time.Duration {
	return EffectiveFreshnessThreshold(configBaseline, accountSideWorstUpperBound)
}

// AccountFreshnessUpperBoundProvider 是账号侧冻结上界的生产来源（由探测调度器实现，D2/C）。
// 仅取 AccountFreshnessUpperBound 能力，便于管理端/阈值计算注入。
type AccountFreshnessUpperBoundProvider interface {
	AccountFreshnessUpperBound(accountID int64, modelKey string) time.Duration
}

// 观测条目字段键与维度标识的对外别名（管理端透传使用，D4）。
// 全部指向 D2 既有常量（同一事实源），D4 不另立一套键名，避免展示与写入口读两套字段。
const (
	// FreshnessObservedAtKey 权威观测时间（仅可分类结果推进）。
	FreshnessObservedAtKey = entryObservedAtKey
	// FreshnessAttemptedAtKey 探测尝试时间（不得解除陈旧告警）。
	FreshnessAttemptedAtKey = entryAttemptedAtKey
	// FreshnessResetAtKey 限流重置时间（哨兵远端值表示无信号占位）；键名与唯一写入口
	// 写入的字段一致（写入口暂以字面量书写，此处保持同名字面量以免读两套字段）。
	FreshnessResetAtKey = "rate_limit_reset_at"
	// FreshnessReasonKey 限流原因。
	FreshnessReasonKey = "reason"
	// TokenHarborAccountLevelScope 账号级（无模型键）维度在 model_rate_limits 中的 scope 键。
	TokenHarborAccountLevelScope = tokenHarborAccountLevelProbeScope
	// ModelRateLimitsKey 账号 extra 中存放模型级/账号级限流观测的 jsonb 键。
	ModelRateLimitsKey = modelRateLimitsKey
)
