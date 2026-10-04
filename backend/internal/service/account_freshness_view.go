package service

import (
	"fmt"
	"strings"
	"time"
)

// 本文件实现 D4（工作项 3「管理端可见」+ 验收 6）的账号两维观测透传视图。
// 管理端 handler 只做序列化，展示语义与陈旧判定全部收在本文件，保证：
//   - 阈值由三维唯一公式产出（EffectiveFreshnessThreshold 系），展示/告警/验收同一计算结果；
//   - 陈旧只看权威观测时间 observed_at（探测尝试时间 attempted_at 不解除告警）；
//   - 无信号占位（无权威观测）展示 waiting_probe「等待主动复探」，
//     **不得显示为正常倒计时**（哨兵 reset_at 不是恢复倒数，恢复时刻未知）；
//   - observed_at 类型判别与解析统一收在 observedAtFromEntry / parseObservedAt（告警路径
//     共用），存在但非法（类型非 string 或格式不可解析）一律失败关闭，绝不降级为正常态。

// 展示状态枚举（管理端透传）。
const (
	FreshnessDisplayObserved     = "observed"      // 已获得权威观测且未超阈值
	FreshnessDisplayStale        = "stale"         // 权威观测已超有效阈值
	FreshnessDisplayWaitingProbe = "waiting_probe" // 无权威观测（无信号占位），等待主动复探
)

// 维度标识（账号+模型 / 账号级）。
const (
	FreshnessDimAccountModel = "account_model"
	FreshnessDimAccountLevel = "account_level"
)

// FreshnessObservationView 是账号侧观测的管理端透传视图（账号+模型 / 账号级两维）。
// 渠道维经既有 ChannelStatus/ChannelObservedAt 字段透传，不重复建模。
type FreshnessObservationView struct {
	Scope              string
	Dimension          string
	ObservedAt         string
	AttemptedAt        string
	ResetAt            string
	Reason             string
	EffectiveThreshold time.Duration
	Stale              bool
	ActiveAlert        bool
	DisplayState       string
}

// BuildAccountFreshnessView 由写入口条目 + 有效阈值 + 告警状态构造透传视图。
// entry 为唯一状态写入口（ApplyModelRateLimitObservation）持久化的观测条目；threshold
// 为该维度的有效阈值（三维唯一公式产出）；now 为展示时刻；activeAlert 为同维度活跃告警。
// 返回 error：observed_at 存在但类型非法（数字/布尔/对象等非字符串）或格式非法（权威数据
// 损坏）时返回明确错误，沿调用方既有 error 路径传播——不得降级展示为 observed/健康
// （与 accountObservedAt 同口径的失败关闭）。
// 空值=无权威观测（waiting_probe 占位）与合法时间路径行为不变。
func BuildAccountFreshnessView(entry map[string]any, scope string, threshold time.Duration, now time.Time, activeAlert bool) (FreshnessObservationView, error) {
	v := FreshnessObservationView{
		Scope:              scope,
		Dimension:          FreshnessDimAccountModel,
		EffectiveThreshold: threshold,
		ActiveAlert:        activeAlert,
	}
	if scope == tokenHarborAccountLevelProbeScope {
		v.Dimension = FreshnessDimAccountLevel
	}
	if entry != nil {
		// observed_at 类型判别与解析共用 observedAtFromEntry（告警路径同一口径）：
		// 存在但非字符串 = 权威数据损坏，直接返回错误，绝不降级为 waiting_probe。
		observedAt, oerr := observedAtFromEntry(entry)
		if oerr != nil {
			return FreshnessObservationView{}, oerr
		}
		v.ObservedAt = observedAt
		// attempted_at：展示层对损坏条目同样失败关闭（E48），与 isAccountLevelStale
		// 同口径（:1459-1472 逐字同口径）：键不存在 / nil / 空串 / 仅空白 = 无值（既有语义
		// 不变，不复制字段）；存在但类型非 string = 权威数据损坏 → 明确错误失败关闭；存在且
		// 为 string 但 RFC3339 不可解析 → 明确错误失败关闭；合法 → 复制展示。attempted_at 仍
		// 只作展示，不参与 stale 判定（D5 锁定不变）。
		if raw, exists := entry[FreshnessAttemptedAtKey]; exists && raw != nil {
			s, ok := raw.(string)
			if !ok {
				return FreshnessObservationView{}, fmt.Errorf("invalid attempted_at: wrong type %T (want string), authoritative data corrupted", raw)
			}
			if strings.TrimSpace(s) == "" {
				// 空串 / 仅空白 = 无值，沿用既有语义不复制字段、不报错。
			} else if _, e := time.Parse(time.RFC3339, s); e != nil {
				return FreshnessObservationView{}, fmt.Errorf("invalid attempted_at %q: %w", s, e)
			} else {
				v.AttemptedAt = s
			}
		}
		if s, ok := entry[FreshnessResetAtKey].(string); ok {
			v.ResetAt = s
		}
		if s, ok := entry[FreshnessReasonKey].(string); ok {
			v.Reason = s
		}
	}
	// 陈旧只看权威观测时间；尝试时间只作展示，不参与判定、不解除告警（D5 锁定不变）。
	// 空串=无权威观测（waiting_probe 占位），不判陈旧；存在但非法=权威数据损坏，
	// 返回错误令调用方失败关闭，绝不降级为 observed/健康。attempted_at 展示层对损坏条目
	// 同样失败关闭（E48，与上 attempted_at 校验同口径），与 isAccountLevelStale 一致。
	t, hasObserved, err := parseObservedAt(v.ObservedAt)
	if err != nil {
		return FreshnessObservationView{}, err
	}
	if hasObserved {
		v.Stale = now.Sub(t) > threshold
	}
	switch {
	case !hasObserved:
		// 无权威观测（空值/仅空白，含无信号占位）：等待主动复探，不显示倒计时。
		v.DisplayState = FreshnessDisplayWaitingProbe
	case v.Stale:
		v.DisplayState = FreshnessDisplayStale
	default:
		v.DisplayState = FreshnessDisplayObserved
	}
	return v, nil
}

// observedAtFromEntry 读取观测条目中的权威观测时间并做类型判别（唯一判别口径，
// 告警评估 accountObservedAt 与本视图 BuildAccountFreshnessView 共用，与 parseObservedAt
// 一起构成两路径同一事实源）：
//   - 键不存在 = 无观测 → ("", nil)，不算损坏，不判陈旧；
//   - 存在但值为 nil（字面 null）= 无观测 → ("", nil)，不算损坏；
//   - 存在但类型非 string（数字/布尔/对象等损坏 JSON）= 权威数据损坏 → ("", err)，
//     **绝不降级为无观测**；
//   - 存在且为 string → (原值, nil)，由解析口径 parseObservedAt 判空/判格式。
//
// 格式非法（存在且非空 string 但不可解析）由 parseObservedAt 报错；空串/仅空白由
// parseObservedAt 归「无观测」。单一解析口径保证展示与告警判定同一事实源。
func observedAtFromEntry(entry map[string]any) (string, error) {
	// 键不存在 / 值为 nil（字面 null）→ 无观测。map 读不存在的键返回 nil，与「键存在
	// 但 null」一并归无观测（保持既有语义）。
	raw, exists := entry[FreshnessObservedAtKey]
	if !exists || raw == nil {
		return "", nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("invalid observed_at: wrong type %T (want string), authoritative data corrupted", raw)
	}
	return s, nil
}

// parseObservedAt 是权威观测时间字符串的唯一解析口径（告警评估 accountObservedAt 与本
// 视图共用，保证展示与告警判定同一事实源）：
//   - 空串（含仅空白）= 无权威观测 → (零值, false, nil)，不算损坏，不判陈旧；
//   - 零值 RFC3339（0001-01-01T00:00:00Z）= 无有效观测 → (零值, false, nil)，与告警路径
//     accountObservedAt:393、探测路径 account_health_recovery_probe_service.go:1476 同一口径
//     （IsZero 归无观测），三路径同一事实源（E48）；
//   - 格式非法 → (零值, false, err)，权威数据损坏，调用方须失败关闭（不降级为健康态）；
//   - 合法 → (时间, true, nil)。
func parseObservedAt(raw string) (time.Time, bool, error) {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, false, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("invalid observed_at %q: %w", raw, err)
	}
	// 零值时间非有效观测：同一损坏/哨兵条目在管理端（本视图）也须归无观测，与告警/探测
	// 路径同语义，杜绝「管理端显示已观测健康、告警按无观测处理」的口径分裂（E48）。
	if t.IsZero() {
		return time.Time{}, false, nil
	}
	return t, true, nil
}
