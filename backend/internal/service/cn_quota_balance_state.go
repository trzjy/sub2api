package service

// 余额三态契约（方案 §4 F1 / §2.2 / §3.1 R3-F1 / R5-F1；C1 解析器唯一归属）。
//
// 本文件是「余额三态」的唯一解析器。F1（Kira 探针）/ F2（lifecycle 结构化信号）与
// 后续 C3（调度维度解析）都只消费本文件暴露的 API，不得各自重新解释余额键——
// 这是方案 §6 C1 卡 R7-F4 的硬约束：C3 只消费不复制。
//
// 契约边界（消费方必须遵守）：
//   - 三态 = 成功取得且新鲜（fresh，含数值）/ 过期或缺键或缺时间戳（stale-or-missing）/
//     查询失败明确错误（error）。fresh-zero 与真实零余额如实区分，禁止把零值与失败混淆。
//   - IsExhausted 仅在 fresh 且数值 ≤0 时为真（方案 §4 F1 R5-F1：只有成功取得且新鲜
//     的余额 ≤0 才产生 Exhausted 结论）；unknown（stale/error）绝不产生任何耗尽结论。
//   - 每个来源绑定自身 observed_at 判新鲜（逐来源独立年龄门，R3-F1）：禁止使用账号级
//     单一快照时间或任一他源时间替代。本文件 ResolveBalanceState 严格按来源键族解析。
//   - 解析器只消费既有 extra 键族（{platform}_balance + {platform}_balance_updated_at
//     等），不新增探针、不造默认值。
//
// 余额年龄阈值 = 10 分钟（来源 C0 ⑤ 核证：BalanceCheckIntervalMinutes 默认 10，
// config.go:1212 定义、config.go:2702 默认 10，消费点 wire.go:477）。本卡不为此
// 改 config.go，在此以常量表达并引用既有默认语义（见完工报告）。

import (
	"strings"
	"time"
)

// cnQuotaBalanceFreshnessThresholdMinutes 余额快照新鲜度年龄门（分钟）。
//
// 来源：C0 ⑤ 核证 = BalanceCheckIntervalMinutes 默认 10（config.go:1212 定义、
// config.go:2702 默认 10）。本卡不为此改动 config.go，以常量表达既有默认语义。
const cnQuotaBalanceFreshnessThresholdMinutes = 10

// BalanceStateKind 余额三态分类。
type BalanceStateKind int

const (
	// BalanceFresh 成功取得且新鲜（含数值；fresh-zero 与真实零余额如实区分）。
	BalanceFresh BalanceStateKind = iota
	// BalanceStaleOrMissing 过期或缺键或缺时间戳（unknown 态：不产生任何耗尽结论）。
	BalanceStaleOrMissing
	// BalanceError 查询失败明确错误（unknown 态：不产生任何耗尽结论）。
	BalanceError
)

// BalanceState 单一余额来源的三态解析结果。
//
// 语义（消费方契约）：
//   - Kind=BalanceFresh 时 Value 有效（含 0）；fresh-zero 与真实零余额都如实保留。
//   - Kind≠BalanceFresh 时 Value 无意义（unknown 态）。
//   - IsExhausted 仅在 BalanceFresh 且 Value<=0 时为真（方案 §4 F1 R5-F1）。
//   - Confirmed 仅在 BalanceFresh 时为真（有 confirmed 数值，含零）。
type BalanceState struct {
	Kind       BalanceStateKind
	Value      float64
	ObservedAt time.Time
	Err        string
}

// IsFresh 报告成功取得且新鲜（含数值）。
func (b BalanceState) IsFresh() bool { return b.Kind == BalanceFresh }

// Confirmed 报告有 confirmed 数值（含零）；unknown 态为 false。
func (b BalanceState) Confirmed() bool { return b.Kind == BalanceFresh }

// IsUnknown 报告 unknown 态（stale / error），不产生任何耗尽结论。
func (b BalanceState) IsUnknown() bool { return b.Kind != BalanceFresh }

// IsExhausted 仅在 fresh 且数值 ≤0 时为真（方案 §4 F1 R5-F1：只有成功取得且新鲜的
// 余额 ≤0 才产生 Exhausted 结论；unknown 永不直接耗尽）。
func (b BalanceState) IsExhausted() bool {
	return b.Kind == BalanceFresh && b.Value <= 0
}

// BalanceStateError 构造查询失败明确错误态（live 查询失败时使用，非快照解析产物）。
func BalanceStateError(err error) BalanceState {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	return BalanceState{Kind: BalanceError, Err: msg}
}

// ResolveBalanceState 解析单一来源余额三态：消费 {platform}_balance +
// {platform}_balance_updated_at 既有 extra 键族，按该来源自身的 observed_at 施加
// 逐来源独立年龄门（方案 §3.1 R3-F1）。
//
// 返回语义：
//   - 缺余额键 / 缺时间戳键 / 时间戳不可解析 / 距今超过阈值 → BalanceStaleOrMissing
//     （unknown 态，不产生耗尽结论）。
//   - 值不可解析为非数字 → BalanceStaleOrMissing。
//   - 否则 → BalanceFresh（Value 为余额数值，含 0）。
func ResolveBalanceState(extra map[string]any, platform string, now time.Time) BalanceState {
	if extra == nil || platform == "" {
		return BalanceState{Kind: BalanceStaleOrMissing}
	}
	balanceKey := cnExtraKey(platform, cnBalanceExtraSuffixBalance)
	updatedKey := cnExtraKey(platform, cnBalanceExtraSuffixUpdated)
	rawVal, ok := extra[balanceKey]
	if !ok {
		return BalanceState{Kind: BalanceStaleOrMissing}
	}
	rawObs, ok := extra[updatedKey]
	if !ok {
		return BalanceState{Kind: BalanceStaleOrMissing}
	}
	obsStr, _ := rawObs.(string)
	if strings.TrimSpace(obsStr) == "" {
		return BalanceState{Kind: BalanceStaleOrMissing}
	}
	observedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(obsStr))
	if err != nil {
		return BalanceState{Kind: BalanceStaleOrMissing}
	}
	val, ok := cnParseF64(rawVal)
	if !ok {
		return BalanceState{Kind: BalanceStaleOrMissing}
	}
	if age := now.Sub(observedAt); age < 0 || age > time.Duration(cnQuotaBalanceFreshnessThresholdMinutes)*time.Minute {
		return BalanceState{Kind: BalanceStaleOrMissing}
	}
	return BalanceState{Kind: BalanceFresh, Value: val, ObservedAt: observedAt}
}

// ResolveKiraVNDBalanceState Kira 付费 VND 余额三态（消费 {platform}_balance 键族；
// Kira 账号的 platform 即其 base_url 映射的既有 platform，写键见 cn_provider_kira.go
// cnExtraKey(account.Platform, cnBalanceExtraSuffixBalance)）。
func ResolveKiraVNDBalanceState(account *Account, now time.Time) BalanceState {
	if account == nil {
		return BalanceState{Kind: BalanceStaleOrMissing}
	}
	return ResolveBalanceState(account.Extra, account.Platform, now)
}

// ResolveTHWalletBalanceState TH 钱包余额三态（消费 th_balance / th_balance_updated_at
// 既有键族，tokenharbor_pass_service.go 常量 TokenHarborWalletBalance*；C6 已入库）。
// 本卡仅暴露解析器，不接入采集。
func ResolveTHWalletBalanceState(account *Account, now time.Time) BalanceState {
	if account == nil {
		return BalanceState{Kind: BalanceStaleOrMissing}
	}
	if account.Extra == nil {
		return BalanceState{Kind: BalanceStaleOrMissing}
	}
	rawVal, ok := account.Extra[TokenHarborWalletBalanceExtraKey]
	if !ok {
		return BalanceState{Kind: BalanceStaleOrMissing}
	}
	rawObs, ok := account.Extra[TokenHarborWalletBalanceUpdatedAtExtraKey]
	if !ok {
		return BalanceState{Kind: BalanceStaleOrMissing}
	}
	obsStr, _ := rawObs.(string)
	if strings.TrimSpace(obsStr) == "" {
		return BalanceState{Kind: BalanceStaleOrMissing}
	}
	observedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(obsStr))
	if err != nil {
		return BalanceState{Kind: BalanceStaleOrMissing}
	}
	val, ok := cnParseF64(rawVal)
	if !ok {
		return BalanceState{Kind: BalanceStaleOrMissing}
	}
	if age := now.Sub(observedAt); age < 0 || age > time.Duration(cnQuotaBalanceFreshnessThresholdMinutes)*time.Minute {
		return BalanceState{Kind: BalanceStaleOrMissing}
	}
	return BalanceState{Kind: BalanceFresh, Value: val, ObservedAt: observedAt}
}
