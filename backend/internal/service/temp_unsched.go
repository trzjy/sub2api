package service

import (
	"context"
	"time"
)

// TempUnschedState 临时不可调度状态
type TempUnschedState struct {
	UntilUnix            int64  `json:"until_unix"`                       // 解除时间（Unix 时间戳）
	TriggeredAtUnix      int64  `json:"triggered_at_unix"`                // 触发时间（Unix 时间戳）
	StatusCode           int    `json:"status_code"`                      // 触发的错误码
	MatchedKeyword       string `json:"matched_keyword"`                  // 匹配的关键词
	RuleIndex            int    `json:"rule_index"`                       // 触发的规则索引
	ErrorMessage         string `json:"error_message"`                    // 错误消息
	TriggerCount         int64  `json:"trigger_count,omitempty"`          // 本次触发累计命中次数
	TriggerThreshold     int    `json:"trigger_threshold,omitempty"`      // 触发阈值
	TriggerWindowMinutes int    `json:"trigger_window_minutes,omitempty"` // 计数窗口（分钟）
	// Tier records the health-breaker tier that triggered this block.
	// 1=watch (informational), 2=warning (alert), 3=trip (circuit opened).
	Tier int `json:"tier,omitempty"`
	// ProbeAttempts counts probe-based recovery attempts made while blocked.
	ProbeAttempts int `json:"probe_attempts,omitempty"`
	// Scope 隔离粒度：account / model（上游 v0.2.18 引入）
	Scope string `json:"scope,omitempty"`
	// Model 是触发本次停调的上游模型名（经账号 model_mapping 归一后的出站名）。
	// 恢复探针据此探测真实故障面，而不是笼统探测平台默认模型。空值表示记录方
	// 未提供，探针回落平台默认测试模型。模型级冷却时该字段同时是被冷却的模型键。
	Model string `json:"model,omitempty"`
	// ConsecutiveSuccesses 探针恢复链的连续成功轮次计数（探针 service 专属读写，
	// 首轮 success 置 1，第二轮 success 执行恢复并随停调清除消失；失败重置为 0）。
	ConsecutiveSuccesses int `json:"consecutive_successes,omitempty"`
}

// tempUnschedIndefiniteUntil 是无限期停调的哨兵截止时间。
//
// 【2026-11-11 重要变更】健康熔断器（openai_apikey_health_breaker.go）已不再使用该值。
// 停用时长一律由 account_unavailability.go 的 ClassifyAccountUnavailability 唯一决定，
// 且被 unavailMaxCooldown 硬夹住，任何路径都不允许再写出远期哨兵值。
// 当前仅 cursor 健康探针链路（cursor_account_health_probe.go）仍在使用，
// 该链路属同类风险，待按同一规范迁移。
//
// 历史设计意图（保留供理解，勿据此复用）：语义为无自动过期，等恢复探针确认可用后才解除。
//
// 为什么用远端时间而不是 NULL：temp_unschedulable_until = NULL 在全部读取点都表示
// 无停调（IsSchedulable / tempUnschedulablePredicate / ListTempUnschedulableAccounts），
// 写 NULL 等于立刻恢复可调度，与语义相反。
//
// 该值同时充当代际令牌（ClearTempUnschedulableIfGeneration /
// BumpTempUnschedulableReasonIfGeneration 的等值比较对象），因其恒定不变，条件清除稳定命中。
// Keep the sentinel safely inside JSON's year range after database timezone conversion.
var tempUnschedIndefiniteUntil = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)

// TempUnschedCache 临时不可调度缓存接口
type TempUnschedCache interface {
	SetTempUnsched(ctx context.Context, accountID int64, state *TempUnschedState) error
	GetTempUnsched(ctx context.Context, accountID int64) (*TempUnschedState, error)
	DeleteTempUnsched(ctx context.Context, accountID int64) error
}

// OpenAIAPIKeyHealthRecordResult is the outcome of recording one failure against
// the rolling window. TrippedWatch/TrippedWarning/TrippedTrip report whether this
// record newly crossed the corresponding tier boundary within the current window
// (debounced so repeated observations in the same window do not re-alert).
type OpenAIAPIKeyHealthRecordResult struct {
	Count          int64
	TrippedWatch   bool
	TrippedWarning bool
	TrippedTrip    bool
}

// OpenAIAPIKeyHealthCache is an optional TempUnschedCache extension used to
// aggregate pool API-key failures across gateway instances.
type OpenAIAPIKeyHealthCache interface {
	// RecordOpenAIAPIKeyHealthFailure records a failure in the account's rolling
	// window and returns the resulting tier transitions. windowMinutes is the
	// window; watchThreshold/warningThreshold/tripThreshold are the three tier
	// boundaries (tripThreshold == the L3 failure threshold).
	RecordOpenAIAPIKeyHealthFailure(ctx context.Context, accountID int64, windowMinutes, watchThreshold, warningThreshold, tripThreshold int) (OpenAIAPIKeyHealthRecordResult, error)
}

// TimeoutCounterCache 超时计数器缓存接口
type TimeoutCounterCache interface {
	// IncrementTimeoutCount 增加账户的超时计数，返回当前计数值
	// windowMinutes 是计数窗口时间（分钟），超过此时间计数器会自动重置
	IncrementTimeoutCount(ctx context.Context, accountID int64, windowMinutes int) (int64, error)
	// GetTimeoutCount 获取账户当前的超时计数
	GetTimeoutCount(ctx context.Context, accountID int64) (int64, error)
	// ResetTimeoutCount 重置账户的超时计数
	ResetTimeoutCount(ctx context.Context, accountID int64) error
	// GetTimeoutCountTTL 获取计数器剩余过期时间
	GetTimeoutCountTTL(ctx context.Context, accountID int64) (time.Duration, error)
}
