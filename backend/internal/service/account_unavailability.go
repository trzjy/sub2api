package service

import (
	"time"
)

// ============================================================================
// 账号"暂时不可调度"的唯一规范（全站唯一事实源）
//
// 背景：改造前全站有 82 处写入点、4 条并行通道（temp_unschedulable_until /
// rate_limit_reset_at / model_rate_limits / overload_until）、20+ 个各自拍板的
// 时长常量。同一个上游 429 在不同代码路径上得到 61 秒 → 9999 年四种答案，
// 取决于账号是哪个平台、走了哪条 if 分支。生产事故：8 个 priority=1 的账号
// 因为一次 429 被健康熔断器写成 9999-12-31，永久退出调度池，最长 7 天。
//
// 本文件的立场：
//   1. 谁能决定"罚多久"只有一处 —— ClassifyAccountUnavailability。
//   2. 生产方只负责陈述事实（状态码、上游给的恢复时间），不负责拍时长。
//   3. "看不懂"一律按最轻处罚处理，绝不按最重处理。
//      旧设计把未知错误罚 7 天 / 9999 年，等于"我不认识你，所以永久封杀你"。
//   4. 除凭据失效外，所有停用都必须能自动恢复 —— 不依赖人工，不依赖探针闯关。
//
// 迁移状态：本文件先接管健康熔断器（生产事故来源），其余调用点按其所属故障
// 类型逐批迁移；未迁移的调用点保持不变行为，不做半路过早改动。
// ============================================================================

// AccountUnavailabilityKind 是账号"暂时为何不可调度"的分类枚举。
// 新增上游错误时必须先到这里加类型，而不是在调用点加 if 分支。
type AccountUnavailabilityKind string

const (
	// UnavailRateLimited 上游限流（429）：请求太快，不是账号坏了。
	// 这是最常见的类型，必须短冷却 + 自动恢复。
	UnavailRateLimited AccountUnavailabilityKind = "rate_limited"

	// UnavailQuotaExhausted 配额窗口确实用完（官方用量快照触顶）。
	// 冷却到窗口重置点；重置点未知时用短冷却反复重查，不猜长期限。
	UnavailQuotaExhausted AccountUnavailabilityKind = "quota_exhausted"

	// UnavailConcurrency 并发超限：上游允许稍后重试。
	UnavailConcurrency AccountUnavailabilityKind = "concurrency"

	// UnavailUpstreamFault 上游自身故障（5xx，502 除外）。
	// 账号本身没坏，但当下不值得继续打。
	UnavailUpstreamFault AccountUnavailabilityKind = "upstream_fault"

	// UnavailCredentialDead 凭据失效（401 / refresh_token 缺失）。
	// 唯一需要人工介入的类型：冷却是为了止损，不是为了自愈。
	UnavailCredentialDead AccountUnavailabilityKind = "credential_dead"

	// UnavailTransient 网络/传输层瞬时错误（超时、连接重置）。
	UnavailTransient AccountUnavailabilityKind = "transient"

	// UnavailUnknown 看不清原因。
	// 旧设计对它的处罚是 7 天或 9999 年；现在改为与限流同档的短冷却，
	// 让账号快速回到池子里重新试，用真实请求代替猜测。
	UnavailUnknown AccountUnavailabilityKind = "unknown"
)

// UnavailabilityRecovery 描述账号靠什么回到调度池。
type UnavailabilityRecovery string

const (
	// RecoverByExpiry 到点自动恢复，无需任何外部动作。这是默认。
	RecoverByExpiry UnavailabilityRecovery = "expiry"
	// RecoverByCredentialRefresh 需要凭据刷新成功（OAuth token 轮换等）。
	RecoverByCredentialRefresh UnavailabilityRecovery = "credential_refresh"
	// RecoverByOperator 只能人工处理（凭据彻底失效、账号被封）。
	RecoverByOperator UnavailabilityRecovery = "operator"
)

// 唯一一张时长表。所有调用点共用，不允许在调用点另写常量。
const (
	// unavailRateLimitDefaultCooldown 上游没给 Retry-After 时的限流冷却。
	// 61 秒确保跨过整分钟边界，兼容按分钟重置限额的中转上游。
	unavailRateLimitDefaultCooldown = 61 * time.Second

	// unavailQuotaUnknownCooldown 配额重置点未知时的重查间隔。
	// 宁可 5 分钟后再问一次，也不要猜一个长期限把账号关起来。
	unavailQuotaUnknownCooldown = 5 * time.Minute

	// unavailConcurrencyCooldown 并发超限冷却。
	unavailConcurrencyCooldown = 30 * time.Second

	// unavailUpstreamFaultCooldown 上游 5xx 冷却。
	// 账号无辜，只是上游此刻不行；冷却到上游大概率恢复即可。
	unavailUpstreamFaultCooldown = 30 * time.Minute

	// unavailTransientCooldown 传输层瞬时错误冷却。
	unavailTransientCooldown = 2 * time.Minute

	// unavailCredentialDeadCooldown 凭据失效冷却。
	// 冷却只是止损，真正恢复要靠刷新凭据或人工换 key。
	unavailCredentialDeadCooldown = 30 * time.Minute

	// unavailUnknownCooldown 未知原因冷却 —— 全文件最重要的一行。
	// 旧设计：7 天 / 9999 年。新设计：与限流同档。
	// 理由：把"我不认识这个错误"当作"永久禁用这个账号"，是生产事故的直接成因。
	unavailUnknownCooldown = 60 * time.Second

	// unavailMaxCooldown 单次停用的硬上限。
	// 任何路径都不允许超过这个值 —— 9999 年这类哨兵值从此禁止出现在停用时长里。
	// 需要"无限期"的场景只有一种：凭据真的废了，那应该 status=error（人工可见），
	// 而不是用一个看不见的远期时间戳把账号静默藏起来。
	unavailMaxCooldown = 6 * time.Hour
)

// UnavailabilityFact 是生产方陈述的"故障事实"。生产方只填事实，不填时长。
type UnavailabilityFact struct {
	// Kind 故障类型（必填）。
	Kind AccountUnavailabilityKind

	// RetryAfter 上游通过 Retry-After 明确告知的等待时长；0 表示上游没说。
	RetryAfter time.Duration

	// ResetAt 上游明确的恢复时刻（配额窗口重置点等）；nil 表示不知道。
	ResetAt *time.Time

	// Now 判定基准时刻；零值时取 time.Now()。
	Now time.Time
}

// UnavailabilityDecision 是分类结果：停到什么时候、靠什么恢复。
type UnavailabilityDecision struct {
	Kind     AccountUnavailabilityKind
	Until    time.Time
	Recovery UnavailabilityRecovery
	// Source 记录时长来源，便于排障时回答"为什么是这个时间"。
	// 取值：retry_after / reset_at / kind_default / clamped
	Source string
}

// Cooldown 返回停用时长（便于日志与测试断言）。
func (d UnavailabilityDecision) Cooldown() time.Duration {
	return time.Until(d.Until)
}

// ClassifyAccountUnavailability 是全站唯一决定"账号停多久"的函数。
//
// 规则优先级（自上而下，命中即止）：
//  1. 上游明确给了恢复时刻（ResetAt）→ 就用它（但仍受上限约束）
//  2. 上游明确给了等待时长（RetryAfter）→ 就用它（但仍受上限约束）
//  3. 按故障类型取该类型的默认时长
//  4. 无论哪条，最终都夹在 (0, unavailMaxCooldown] 区间内
//
// 这条优先级链本身就是"上游说得清的听上游，说不清的不猜"的落地。
func ClassifyAccountUnavailability(fact UnavailabilityFact) UnavailabilityDecision {
	now := fact.Now
	if now.IsZero() {
		now = time.Now()
	}

	kind := fact.Kind
	if kind == "" {
		kind = UnavailUnknown
	}

	recovery := recoveryForKind(kind)

	// 1) 上游给的恢复时刻最权威。
	if fact.ResetAt != nil && fact.ResetAt.After(now) {
		return clampDecision(kind, now, *fact.ResetAt, recovery, "reset_at")
	}

	// 2) 上游给的等待时长次之（Retry-After 是 HTTP 标准语义，值得信任）。
	if fact.RetryAfter > 0 {
		return clampDecision(kind, now, now.Add(fact.RetryAfter), recovery, "retry_after")
	}

	// 3) 按类型取默认值。
	return clampDecision(kind, now, now.Add(defaultCooldownForKind(kind)), recovery, "kind_default")
}

// defaultCooldownForKind 是时长表的唯一实现，与上面常量一一对应。
func defaultCooldownForKind(kind AccountUnavailabilityKind) time.Duration {
	switch kind {
	case UnavailRateLimited:
		return unavailRateLimitDefaultCooldown
	case UnavailQuotaExhausted:
		return unavailQuotaUnknownCooldown
	case UnavailConcurrency:
		return unavailConcurrencyCooldown
	case UnavailUpstreamFault:
		return unavailUpstreamFaultCooldown
	case UnavailTransient:
		return unavailTransientCooldown
	case UnavailCredentialDead:
		return unavailCredentialDeadCooldown
	case UnavailUnknown:
		return unavailUnknownCooldown
	default:
		// 未知的 kind（未来新增类型忘了登记）也落到最轻处罚，不落最重。
		return unavailUnknownCooldown
	}
}

func recoveryForKind(kind AccountUnavailabilityKind) UnavailabilityRecovery {
	switch kind {
	case UnavailCredentialDead:
		return RecoverByCredentialRefresh
	case UnavailQuotaExhausted:
		// 窗口重置点未知时靠到点重查，仍需 expiry 兜底。
		return RecoverByExpiry
	default:
		return RecoverByExpiry
	}
}

// clampDecision 把决策时长夹进合法区间：
//   - 不超过 unavailMaxCooldown（禁止再出现 9999 年）
//   - 至少 1 秒（禁止写出"立即过期"导致状态形同虚设）
//
// 被夹住时 Source 追加标注，排障时能看出"上游说要等 3 天，被我们收到 6 小时"。
func clampDecision(kind AccountUnavailabilityKind, now, until time.Time, recovery UnavailabilityRecovery, source string) UnavailabilityDecision {
	d := until.Sub(now)
	if d < time.Second {
		d = time.Second
	}
	if d > unavailMaxCooldown {
		d = unavailMaxCooldown
		if source == "reset_at" || source == "retry_after" {
			source += "_clamped"
		}
	}
	return UnavailabilityDecision{
		Kind:     kind,
		Until:    now.Add(d),
		Recovery: recovery,
		Source:   source,
	}
}

// ClassifyUpstreamStatus 是 ClassifyAccountUnavailability 的便捷入口：
// 按上游 HTTP 状态码和已知的恢复时刻给出决策。用于把"状态码 → 故障类型"
// 这张对应表也收口到一处，避免各调用点各自 switch。
func ClassifyUpstreamStatus(statusCode int, retryAfter time.Duration, resetAt *time.Time, now time.Time) UnavailabilityDecision {
	return ClassifyAccountUnavailability(UnavailabilityFact{
		Kind:       KindForUpstreamStatus(statusCode),
		RetryAfter: retryAfter,
		ResetAt:    resetAt,
		Now:        now,
	})
}

// KindForUpstreamStatus 是"上游状态码 → 故障类型"的唯一映射表。
// 调用点不再自行判断某个状态码算不算账号故障。
func KindForUpstreamStatus(statusCode int) AccountUnavailabilityKind {
	switch {
	case statusCode == 429:
		return UnavailRateLimited
	case statusCode == 401 || statusCode == 403:
		// 403 有内容策略/权限两类语义，调用点若已分辨应直接指定 kind；
		// 落到这里的 403 按凭据/权限问题处理。
		return UnavailCredentialDead
	case statusCode == 402:
		return UnavailQuotaExhausted
	case statusCode == 408 || statusCode == 504:
		return UnavailTransient
	case statusCode == 502:
		// 502 是网关把上游超时/非 2xx 映射出来的，不代表账号故障。
		return UnavailTransient
	case statusCode >= 500:
		return UnavailUpstreamFault
	default:
		return UnavailUnknown
	}
}
