package service

// 账号额度维度解析 + F6 调度前置维度门（方案 §2.1/§2.2/§3.1；C3 卡）。
//
// 定位：把既有额度快照键映射为「账号额度维度」列表，并据 §3.1 判定表做发送前
// 零探针门。只读 account.Extra 快照，门内零网络请求、零探针（方案 §3.1）。
//
// 契约边界：
//   - 余额类维度只消费 C1 已交付的三态契约（cn_quota_balance_state.go：
//     ResolveBalanceState / ResolveKiraVNDBalanceState / ResolveTHWalletBalanceState /
//     BalanceState），不重新解释余额键（方案 §6 C1 卡 R7-F4：C3 只消费不复制）。
//   - 存在性与状态分离（R2-F5）：维度是否存在由能力字段决定（如 TH
//     spend_after_allowance=true ⇒ 付费维度存在；平台不支持 ⇒ 不存在）；
//     remaining/exhausted/unknown 由数值字段决定；余额键缺失但能力字段为真 ⇒
//     维度存在且 unknown（不是不存在）。
//   - 逐来源年龄门（R3-F1/R14）：每个维度绑定其来源自身的采集时间与年龄门，
//     逐来源判新鲜；过期来源只对该来源的维度降 unknown，禁止账号级单一时间或
//     任一他源时间替代（防 A 源陈旧与 B 源新鲜混合生成错误 confirmed）。
//   - servable=false 只能由新鲜明确证据产生（Kira：新鲜 VND≤0）；数据缺失/过期
//     永不产生 servable=false（只产生 unknown）。
//   - 模型级免费档（model_rate_limits / tokenharbor_free_tier_exhausted）经方案 §2.1
//     纠偏（C3-r2）纳入解析：仅产出 scope=model 投影，经权威导出
//     ActiveTokenHarborFreeTierScopes 判定"命中前缀且未到期"的条目；前缀/到期语义一律
//     经该权威导出，本文件零字面复制（方案 §2.2 只消费不复制）。模型级维度不参与 F6
//     调度门整号放行/跳过判定（门只消费 scope=account）。
//   - 无任何额度快照的账号族（Anthropic OAuth/纯 API key/Bedrock 等）⇒ 空列表；
//     不做探测、不造默认值。
//
// 判定表（方案 §3.1，只消费 scope=account 维度；按序判定命中即止）：
//   | 维度列表为空                    | 放行（等同现状） |
//   | 有维度但全部 unknown            | 放行（响应式链兜底） |
//   | 任一 confirmed remaining>0 且 servable≠false | 放行 |
//   | 其余（存在 confirmed exhausted 或 servable=false 且无可用 confirmed 剩余） | 跳过 |
//
// 不变式：unknown 永不单独导致跳过（空列表与全 unknown 均放行，不存在
// unknown-only 的跳过分支）。

import (
	"strings"
	"time"
)

// QuotaDimensionScope 维度作用域（方案 §2.1 R10-F1）。
type QuotaDimensionScope string

const (
	// QuotaDimensionScopeAccount 账号级维度：F6 调度门唯一消费的作用域。
	QuotaDimensionScopeAccount QuotaDimensionScope = "account"
	// QuotaDimensionScopeModel 模型级维度：C3-r2 经权威导出产出作投影，归既有
	// model_rate_limits 语义，不进 F6 调度门（门只消费 scope=account）。
	QuotaDimensionScopeModel QuotaDimensionScope = "model"
	// QuotaDimensionScopePath 路径级维度：归既有语义，不进本门。
	QuotaDimensionScopePath QuotaDimensionScope = "path"
)

// QuotaDimensionKind 维度种类（方案 §2.1：free / paid / subscription）。
type QuotaDimensionKind string

const (
	// QuotaDimensionKindFree 免费额度维度。
	QuotaDimensionKindFree QuotaDimensionKind = "free"
	// QuotaDimensionKindPaid 付费余额维度。
	QuotaDimensionKindPaid QuotaDimensionKind = "paid"
	// QuotaDimensionKindSubscription 订阅维度。
	QuotaDimensionKindSubscription QuotaDimensionKind = "subscription"
)

// QuotaDimensionStatus 维度状态（存在性与状态分离，R2-F5）。
type QuotaDimensionStatus int

const (
	// QuotaDimensionUnknown 维度存在但数值不可知（来源过期/数值键缺失/不可解析）。
	// unknown 永不单独决定跳过。
	QuotaDimensionUnknown QuotaDimensionStatus = iota
	// QuotaDimensionRemaining confirmed 且有剩余（remaining>0）。
	QuotaDimensionRemaining
	// QuotaDimensionExhausted confirmed 且已耗尽（remaining≤0 / exhausted 标志为真）。
	QuotaDimensionExhausted
)

// QuotaServableState 维度可服务三态（方案 §3.1 不变式：unknown 视为未证伪）。
type QuotaServableState int

const (
	// QuotaServableUnknown 未证伪（servable unknown 视为可服务）。
	QuotaServableUnknown QuotaServableState = iota
	// QuotaServableYes 明确可服务。
	QuotaServableYes
	// QuotaServableNo 明确不可服务：只能由新鲜明确证据产生（Kira 新鲜 VND≤0）。
	QuotaServableNo
)

// QuotaDimension 单个账号额度维度（方案 §2.1：{kind, scope, target,
// remaining|exhausted, reset_at, servable}；本卡实现 remaining|exhausted 状态与
// servable 三态，reset_at 非 F6 判定输入故未纳入）。
type QuotaDimension struct {
	Kind QuotaDimensionKind
	// Scope 作用域；account 级维度由 F6 门消费，model 级维度（C3-r2）仅作投影、不进本门。
	Scope  QuotaDimensionScope
	Target string
	// Status confirmed 状态；QuotaDimensionUnknown 表示存在但数值不可知。
	Status QuotaDimensionStatus
	// Servable 默认 QuotaServableUnknown（未证伪）；仅新鲜明确证据可置 No。
	Servable QuotaServableState
	// Source 来源键族标识（逐来源年龄门的来源名，R3-F1）。
	Source string
	// ObservedAt 来源自身采集时间；零值表示来源无有效采集时间（→ unknown）。
	ObservedAt time.Time
}

// Confirmed 报告维度是否有 confirmed 数值（remaining/exhausted）。
func (d QuotaDimension) Confirmed() bool { return d.Status != QuotaDimensionUnknown }

// HasRemaining 报告维度 confirmed 且有剩余。
func (d QuotaDimension) HasRemaining() bool { return d.Status == QuotaDimensionRemaining }

// IsExhausted 报告维度 confirmed 且已耗尽。
func (d QuotaDimension) IsExhausted() bool { return d.Status == QuotaDimensionExhausted }

// ServableFalsified 报告维度被明确证伪为不可服务（仅新鲜明确证据可产生）。
func (d QuotaDimension) ServableFalsified() bool { return d.Servable == QuotaServableNo }

// 来源标识（逐来源年龄门的来源名）。
const (
	quotaDimensionSourceKiraVND = "kira_vnd_balance"
)

// ResolveAccountQuotaDimensions 把账号既有额度快照键映射为账号级维度列表
// （方案 §2.2 数据源映射；scope 全为 account）。
//
// 返回空列表表示该账号族无任何账号级额度快照（Anthropic OAuth/纯 API key/
// Bedrock 等）⇒ §3.1 判定表放行、等同现状。
func ResolveAccountQuotaDimensions(account *Account, now time.Time) []QuotaDimension {
	if account == nil || account.Extra == nil {
		return nil
	}
	switch {
	case accountIsTokenHarborBaseURL(account):
		return resolveTokenHarborQuotaDimensions(account, now)
	case accountIsKiraBaseURL(account):
		return resolveKiraQuotaDimensions(account, now)
	default:
		dims := resolveCodingPlanQuotaDimensions(account, now)
		if d := resolveRelayPaidDimension(account, now); d != nil {
			dims = append(dims, *d)
		}
		return dims
	}
}

// resolveTokenHarborQuotaDimensions 解析 TH（tokenharbor.ai）账号的维度：
//   - subscription：th_pass_snapshot.has_pass=true 时存在，状态由 plan_exhausted 决定；
//   - free：th_pass_snapshot 官方 free-tier 口径（exhausted）；
//   - paid：存在性由能力字段 spend_after_allowance=true 决定（R2-F5），状态经
//     ResolveTHWalletBalanceState（C6 钱包 SSOT 键族 th_balance/_updated_at）三态；
//   - 用量维度（th_usage_snapshot.windows）：原始计数无额度分母，存在即 unknown。
func resolveTokenHarborQuotaDimensions(account *Account, now time.Time) []QuotaDimension {
	var dims []QuotaDimension
	snap, ok := TokenHarborPassSnapshotFromExtra(account)
	if ok && snap != nil {
		fresh := quotaDimensionSourceFresh(snap.FetchedAt, now)
		if snap.HasPass {
			status := QuotaDimensionUnknown
			if fresh {
				status = quotaStatusFromExhaustedFlag(snap.PlanExhausted)
			}
			dims = append(dims, QuotaDimension{
				Kind:       QuotaDimensionKindSubscription,
				Scope:      QuotaDimensionScopeAccount,
				Status:     status,
				Servable:   QuotaServableUnknown,
				Source:     TokenHarborPassSnapshotExtraKey,
				ObservedAt: snap.FetchedAt,
			})
		}
		// free 维度：th free-tier 官方口径字段（exhausted）。快照存在即维度存在。
		freeStatus := QuotaDimensionUnknown
		if fresh {
			freeStatus = quotaStatusFromExhaustedFlag(snap.Exhausted)
		}
		dims = append(dims, QuotaDimension{
			Kind:       QuotaDimensionKindFree,
			Scope:      QuotaDimensionScopeAccount,
			Status:     freeStatus,
			Servable:   QuotaServableUnknown,
			Source:     TokenHarborPassSnapshotExtraKey,
			ObservedAt: snap.FetchedAt,
		})
		// paid（钱包）维度：能力字段为真才存在；数值键缺失/过期 → unknown（存在性不受影响）。
		if snap.SpendAfterAllowance {
			wallet := ResolveTHWalletBalanceState(account, now)
			dims = append(dims, QuotaDimension{
				Kind:       QuotaDimensionKindPaid,
				Scope:      QuotaDimensionScopeAccount,
				Status:     quotaStatusFromBalanceState(wallet),
				Servable:   QuotaServableUnknown,
				Source:     TokenHarborWalletBalanceExtraKey,
				ObservedAt: wallet.ObservedAt,
			})
		}
	}
	// 用量维度：th_usage_snapshot 原始计数（requests/tokens_in/tokens_out）无额度
	// 分母，无法产生 confirmed 状态——存在即 unknown（不参与跳过侧）。
	if _, ok := TokenHarborUsageSnapshotFromExtra(account); ok {
		dims = append(dims, QuotaDimension{
			Kind:     QuotaDimensionKindFree,
			Scope:    QuotaDimensionScopeAccount,
			Status:   QuotaDimensionUnknown,
			Servable: QuotaServableUnknown,
			Source:   TokenHarborUsageSnapshotExtraKey,
		})
	}
	// 模型级免费档投影（方案 §2.1 纠偏，C3-r2）：经权威导出
	// ActiveTokenHarborFreeTierScopes 判定"命中 tokenharbor_free_tier_exhausted 前缀且未
	// 到期"的 model_rate_limits 条目，每条产出一个 scope=model 维度。前缀/到期语义一律经
	// 该权威导出，本文件不复制任何字面量与到期规则（方案 §2.2 只消费不复制）。scope=model
	// 维度不进 F6 调度门（EvaluateAccountQuotaDimensionGate 已按 scope 过滤）。
	for _, scope := range ActiveTokenHarborFreeTierScopes(account.Extra, now) {
		dims = append(dims, QuotaDimension{
			Kind:     QuotaDimensionKindFree,
			Scope:    QuotaDimensionScopeModel,
			Target:   scope,
			Status:   QuotaDimensionExhausted,
			Servable: QuotaServableUnknown,
			Source:   modelRateLimitsKey,
			// 权威导出仅返回 scope 字符串、未暴露条目时间戳，故 ObservedAt 取零值
			// （方案 §2.1：如权威导出无时间语义则零值）。
			ObservedAt: time.Time{},
		})
	}
	return dims
}

// resolveKiraQuotaDimensions 解析 Kira（kiraai.vn）账号的维度：
//   - free：kira_usage_snapshot（每日免费池）；servable 语义（P1）：新鲜 VND≤0 ⇒
//     免费维度 servable=false（免费有余量+付费耗尽不得放行）；VND 缺失/过期 ⇒ servable
//     unknown（未证伪）→ 按"confirmed remaining + servable unknown"放行，响应式链兜底；
//   - paid：Kira VND 余额，经 ResolveKiraVNDBalanceState 三态；存在性 = Kira 上游
//     能力恒为真（键缺失/过期 → 存在 + unknown）。
func resolveKiraQuotaDimensions(account *Account, now time.Time) []QuotaDimension {
	var dims []QuotaDimension
	vnd := ResolveKiraVNDBalanceState(account, now)
	if snap := kiraQuotaUsageSnapshotFromExtra(account); snap != nil {
		status := QuotaDimensionUnknown
		if quotaDimensionSourceFresh(snap.fetchedAt, now) {
			status = quotaStatusFromUsedPercent(snap.usedPercent, snap.hasUsedPercent)
		}
		free := QuotaDimension{
			Kind:       QuotaDimensionKindFree,
			Scope:      QuotaDimensionScopeAccount,
			Status:     status,
			Servable:   QuotaServableUnknown,
			Source:     kiraUsageSnapshotExtraKey,
			ObservedAt: snap.fetchedAt,
		}
		// Kira servable：仅新鲜 VND≤0 产生 servable=false（新鲜明确证据）。
		if vnd.IsExhausted() {
			free.Servable = QuotaServableNo
		}
		dims = append(dims, free)
	}
	dims = append(dims, QuotaDimension{
		Kind:       QuotaDimensionKindPaid,
		Scope:      QuotaDimensionScopeAccount,
		Status:     quotaStatusFromBalanceState(vnd),
		Servable:   QuotaServableUnknown,
		Source:     quotaDimensionSourceKiraVND,
		ObservedAt: vnd.ObservedAt,
	})
	return dims
}

// resolveCodingPlanQuotaDimensions 解析 Coding Plan 滚动窗口维度
// （{prefix}_5h_*/weekly_*/monthly_*，由 cnQuotaExtraUpdates 落键）。逐窗口：
// used 键存在且新鲜 ⇒ confirmed（used_percent≥100 耗尽，否则有剩余）；否则 unknown。
// 来源年龄门 = {prefix}_usage_updated_at（逐 provider 独立采集时间）。
func resolveCodingPlanQuotaDimensions(account *Account, now time.Time) []QuotaDimension {
	provider := resolveCNQuotaProvider(account)
	if provider == "" || provider == providerKira {
		return nil
	}
	updated, hasUpdated := parseQuotaDimensionRFC3339(account.Extra[cnExtraKey(provider, cnExtraSuffixUsageUpdated)])
	fresh := hasUpdated && quotaDimensionSourceFresh(updated, now)
	sourceKey := cnExtraKey(provider, cnExtraSuffixUsageUpdated)

	windows := []struct{ usedSuffix, resetSuffix string }{
		{cnExtraSuffix5hUsed, cnExtraSuffix5hReset},
		{cnExtraSuffixWeeklyUsed, cnExtraSuffixWeeklyReset},
		{cnExtraSuffixMonthlyUsed, cnExtraSuffixMonthlyReset},
	}
	var dims []QuotaDimension
	for _, w := range windows {
		usedRaw, hasUsed := account.Extra[cnExtraKey(provider, w.usedSuffix)]
		_, hasReset := account.Extra[cnExtraKey(provider, w.resetSuffix)]
		if !hasUsed && !hasReset {
			continue // 该窗口不存在（cnQuotaExtraUpdates 缺档写 nil 清键）
		}
		status := QuotaDimensionUnknown
		if fresh && hasUsed {
			if used, ok := cnParseF64(usedRaw); ok {
				status = quotaStatusFromUsedPercent(used, true)
			}
		}
		dims = append(dims, QuotaDimension{
			Kind:       QuotaDimensionKindFree,
			Scope:      QuotaDimensionScopeAccount,
			Status:     status,
			Servable:   QuotaServableUnknown,
			Source:     sourceKey,
			ObservedAt: updated,
		})
	}
	return dims
}

// resolveRelayPaidDimension 解析 relay / payg 付费余额维度
// （{platform}_balance + {platform}_balance_unlimited/_plan_name）。存在性 = 键族
// 任一存在（平台不支持该维度 ⇒ 无键 ⇒ 不存在）；状态经 ResolveBalanceState 三态，
// unlimited=true 且新鲜 ⇒ 有剩余。
func resolveRelayPaidDimension(account *Account, now time.Time) *QuotaDimension {
	platform := account.Platform
	if platform == "" {
		return nil
	}
	balanceKey := cnExtraKey(platform, cnBalanceExtraSuffixBalance)
	unlimitedKey := cnExtraKey(platform, cnBalanceExtraSuffixUnlimited)
	planNameKey := cnExtraKey(platform, cnBalanceExtraSuffixPlanName)
	_, hasBalance := account.Extra[balanceKey]
	_, hasUnlimited := account.Extra[unlimitedKey]
	_, hasPlan := account.Extra[planNameKey]
	if !hasBalance && !hasUnlimited && !hasPlan {
		return nil
	}
	st := ResolveBalanceState(account.Extra, platform, now)
	dim := QuotaDimension{
		Kind:       QuotaDimensionKindPaid,
		Scope:      QuotaDimensionScopeAccount,
		Status:     quotaStatusFromBalanceState(st),
		Servable:   QuotaServableUnknown,
		Source:     balanceKey,
		ObservedAt: st.ObservedAt,
	}
	if st.IsFresh() {
		if quotaExtraBool(account.Extra[unlimitedKey]) || st.Value > 0 {
			dim.Status = QuotaDimensionRemaining
		} else {
			dim.Status = QuotaDimensionExhausted
		}
	}
	return &dim
}

// EvaluateAccountQuotaDimensionGate 实现方案 §3.1 判定表（只消费 scope=account
// 维度，scope=model/path 维度不进入本表）。
func EvaluateAccountQuotaDimensionGate(dims []QuotaDimension) QuotaDimensionGateDecision {
	var confirmed, usableRemaining int
	for i := range dims {
		d := dims[i]
		if d.Scope != QuotaDimensionScopeAccount {
			continue
		}
		if !d.Confirmed() {
			continue // unknown 永不单独决定跳过
		}
		confirmed++
		if d.HasRemaining() && !d.ServableFalsified() {
			usableRemaining++
		}
	}
	// 空列表与全 unknown 均为 confirmed==0 → 放行（§3.1 前两行）。
	if confirmed == 0 {
		return QuotaDimensionGateAllow
	}
	if usableRemaining > 0 {
		return QuotaDimensionGateAllow
	}
	return QuotaDimensionGateSkip
}

// QuotaDimensionGateDecision F6 调度前置维度门判定结果。
type QuotaDimensionGateDecision int

const (
	// QuotaDimensionGateAllow 放行（等同可调度）。
	QuotaDimensionGateAllow QuotaDimensionGateDecision = iota
	// QuotaDimensionGateSkip 跳过（由响应式链与恢复语义兜底）。
	QuotaDimensionGateSkip
)

// quotaDimensionSourceFresh 逐来源年龄门（R3-F1）：绑定来源自身采集时间，
// 未来时间戳与超过阈值的年龄均判不新鲜。阈值 = 余额/快照既有 10 分钟年龄门
// （cnQuotaBalanceFreshnessThresholdMinutes，C0 ⑤ 核证值）。
func quotaDimensionSourceFresh(observedAt, now time.Time) bool {
	if observedAt.IsZero() {
		return false
	}
	age := now.Sub(observedAt)
	return age >= 0 && age <= time.Duration(cnQuotaBalanceFreshnessThresholdMinutes)*time.Minute
}

// quotaStatusFromExhaustedFlag 依据官方 exhausted 布尔产生 confirmed 状态。
func quotaStatusFromExhaustedFlag(exhausted bool) QuotaDimensionStatus {
	if exhausted {
		return QuotaDimensionExhausted
	}
	return QuotaDimensionRemaining
}

// quotaStatusFromUsedPercent 依据 used_percent 产生状态；未知值返回 unknown。
func quotaStatusFromUsedPercent(usedPercent float64, hasValue bool) QuotaDimensionStatus {
	if !hasValue {
		return QuotaDimensionUnknown
	}
	if usedPercent >= 100 {
		return QuotaDimensionExhausted
	}
	return QuotaDimensionRemaining
}

// quotaStatusFromBalanceState 把 C1 余额三态映射为维度状态：fresh>0 → 有剩余，
// fresh≤0 → 耗尽，其余（stale/error）→ unknown（不产生任何耗尽结论）。
func quotaStatusFromBalanceState(st BalanceState) QuotaDimensionStatus {
	if !st.IsFresh() {
		return QuotaDimensionUnknown
	}
	if st.Value > 0 {
		return QuotaDimensionRemaining
	}
	return QuotaDimensionExhausted
}

// quotaExtraBool 读取 bool 型 extra 值（非 bool 视为 false）。
func quotaExtraBool(raw any) bool {
	v, ok := raw.(bool)
	return ok && v
}

// parseQuotaDimensionRFC3339 解析 RFC3339 时间戳字符串（其余形态 → false）。
func parseQuotaDimensionRFC3339(raw any) (time.Time, bool) {
	s, ok := raw.(string)
	if !ok {
		return time.Time{}, false
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}

// kiraQuotaUsageSnapshot 是 kira_usage_snapshot 的最小读取投影（window/used_percent/
// used_tokens/limit_tokens/reset_at/fetched_at 单键 JSON，见 cn_provider_kira.go）。
type kiraQuotaUsageSnapshot struct {
	fetchedAt      time.Time
	usedPercent    float64
	hasUsedPercent bool
}

// kiraQuotaUsageSnapshotFromExtra 从账号 Extra 读取 kira_usage_snapshot 快照；
// 键缺失/形态不符 → nil。不做探测、不造默认值。
func kiraQuotaUsageSnapshotFromExtra(account *Account) *kiraQuotaUsageSnapshot {
	if account == nil || account.Extra == nil {
		return nil
	}
	raw, ok := account.Extra[kiraUsageSnapshotExtraKey]
	if !ok || raw == nil {
		return nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	snap := &kiraQuotaUsageSnapshot{}
	if ts, ok := parseQuotaDimensionRFC3339(m["fetched_at"]); ok {
		snap.fetchedAt = ts
	}
	if used, ok := cnParseF64(m["used_percent"]); ok {
		snap.usedPercent = used
		snap.hasUsedPercent = true
	}
	return snap
}
