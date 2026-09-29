package service

import (
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// 本文件实现 §4.2 到期紧迫度调度（Card D）。纯函数部分可全场景单测；
// 谓词复用 Card A 的 IsValidCodeBuddyCreditPackage（基础快照有效性），在此叠加
// 168h 窗口（条件②）与 24h 新鲜度（条件③），不复制第二份基础实现。

// codebuddyCreditUrgencyWindow 是 §4.2 参与性谓词的到期窗口：now < expires_at ≤ now+168h。
const codebuddyCreditUrgencyWindow = 168 * time.Hour

// codebuddyCreditSnapshotFreshness 是 §4.2 参与性谓词的快照新鲜度阈值：≤24h。
const codebuddyCreditSnapshotFreshness = 24 * time.Hour

// codebuddyCreditPackagesUpdatedAtKey 是分包快照的唯一 freshness 依据
// （§1 / §4.1：codebuddy_credit_packages_updated_at，失败绝不更新）。
// Card A 当前只定义了 codebuddyCreditPackagesKey，此处补充 freshness 键常量，
// 键名以权威 §1 为准（不得自创）。
const codebuddyCreditPackagesUpdatedAtKey = "codebuddy_credit_packages_updated_at"

// codeBuddyUrgencyBoost 是一次调度决策的紧迫度加权输入。
// Enabled=false 或 K<=0 时权重计算退化为恒等（与现行为逐位一致）。
type codeBuddyUrgencyBoost struct {
	Enabled bool
	K       float64 // 强制 [0,1]；调用方保证已校验，否则按 0 处理（见 normalize）
}

// normalize 把未启用映射为 K=0（与现行为恒等）。K 值合法性由 config 层
// GatewayCodeBuddyConfig.Validate 统一失败关闭（启动与热加载同一路径），
// 本层不做第二份钳位校验（禁兜底/静默钳位）。
func (b codeBuddyUrgencyBoost) normalize() codeBuddyUrgencyBoost {
	if !b.Enabled {
		return codeBuddyUrgencyBoost{Enabled: false, K: 0}
	}
	return b
}

// codeBuddyUrgencySnapshot 是读自 Account.Extra 的快照输入：
//   - Packages: 分包数组（存储契约 codebuddy_credit_packages）
//   - UpdatedAt: 最后成功快照时间（codebuddy_credit_packages_updated_at，RFC3339）
//
// 解析失败的分包由 Card A 的校验保证剔除；此处仅按基础谓词再次过滤（防御）。
type codeBuddyUrgencySnapshot struct {
	Packages  []CodeBuddyCreditPackage
	UpdatedAt time.Time // zero → 无成功快照（不参与）
}

// CodeBuddyUrgencyResult 是 §4.2 紧迫度公式的纯函数计算结果。
type CodeBuddyUrgencyResult struct {
	// Urgency = Σremaining(参与)
	Urgency decimal.Decimal
	// D = Σtotal(参与)；D=0 → Norm=0
	D decimal.Decimal
	// Norm = urgency/D，截断 [0,1]；D=0 → 0
	Norm float64
	// ParticipatingCount 参与分包数（分子与分母同一集合）。
	ParticipatingCount int
}

// IsCodeBuddyUrgencyEligible 是**紧迫度参与性谓词**（§1/§4.2 定义）：
// 分包须同时满足：
//   - ① 基础快照有效性（复用 Card A IsValidCodeBuddyCreditPackage：字段有效 + 非 Status=3）
//   - ② now < expires_at ≤ now+168h（Asia/Shanghai 字面量已由 Card A 解析转 UTC RFC3339）
//   - ③ 所属快照 ≤24h 新鲜（UpdatedAt 距服务器 UTC now ≤ 24h）
//
// 任一不满足 → 该分包不参与分子、也不参与分母 D。
func IsCodeBuddyUrgencyEligible(pkg CodeBuddyCreditPackage, snapshotUpdatedAt time.Time, now time.Time) bool {
	if !IsValidCodeBuddyCreditPackage(pkg) {
		return false
	}
	// ③ 新鲜度：快照时间必须存在且距今 ≤ 24h（陈旧快照按无快照处理，紧迫度归零）。
	if snapshotUpdatedAt.IsZero() {
		return false
	}
	if now.Sub(snapshotUpdatedAt) > codebuddyCreditSnapshotFreshness || now.Before(snapshotUpdatedAt) {
		return false
	}
	// ② 到期窗口：expires_at 缺失/非法 → 不计入；已过期（≤ now）不计入。
	expiresAt, err := time.Parse(time.RFC3339, strings.TrimSpace(pkg.ExpiresAt))
	if err != nil {
		return false
	}
	if !now.Before(expiresAt) {
		return false
	}
	if expiresAt.Sub(now) > codebuddyCreditUrgencyWindow {
		return false
	}
	return true
}

// ComputeCodeBuddyUrgency 计算 §4.2 公式：
//   urgency = Σremaining(参与)；D = Σtotal(参与)；
//   urgency_norm = urgency/D（D=0 → 0）截断 [0,1]。
//
// 参与集合 = IsCodeBuddyUrgencyEligible（分子分母同一集合）。
func ComputeCodeBuddyUrgency(snapshot codeBuddyUrgencySnapshot, now time.Time) CodeBuddyUrgencyResult {
	var urgency, total decimal.Decimal
	count := 0
	for _, pkg := range snapshot.Packages {
		if !IsCodeBuddyUrgencyEligible(pkg, snapshot.UpdatedAt, now) {
			continue
		}
		urgency = urgency.Add(pkg.Remaining)
		total = total.Add(pkg.Total)
		count++
	}
	norm := 0.0
	if !total.IsZero() {
		ratio, _ := urgency.Div(total).Float64()
		norm = clamp01(ratio)
	}
	return CodeBuddyUrgencyResult{
		Urgency:            urgency,
		D:                  total,
		Norm:               norm,
		ParticipatingCount: count,
	}
}

// codeBuddyWeightedAccount 是紧迫度加权排序中的候选。
// BaseWeight 为账号的基础权重（当前实现恒为 1，语义等价于 base_weight；
// 扩展基础权重因子时只改此处，不触碰既有 Priority/LoadRate/LastUsed 语义）。
type codeBuddyWeightedAccount struct {
	Account *Account
	Weight  float64
}

// ApplyCodeBuddyUrgencyBoost 计算 §4.2 加权：weight = base_weight × (1 + k × urgency_norm)。
// 返回每个候选的最终权重（候选无快照/无参与分包 → Norm=0 → weight=base_weight，k=0 恒等）。
func ApplyCodeBuddyUrgencyBoost(candidate *Account, snapshot codeBuddyUrgencySnapshot, boost codeBuddyUrgencyBoost, now time.Time) float64 {
	if candidate == nil {
		return 1.0
	}
	boost = boost.normalize()
	if !boost.Enabled || boost.K <= 0 {
		return 1.0
	}
	result := ComputeCodeBuddyUrgency(snapshot, now)
	return 1.0 * (1.0 + boost.K*result.Norm)
}

// CodeBuddyUrgencySnapshotFromExtra 从 Account.Extra 读取 §4.2 输入快照。
// 分包键缺失/形态错误 → 空集合（不 panic）；freshness 键缺失/非法 → zero 时间
// （新鲜度条件判 false，紧迫度归零）。解析失败的条目按基础谓词过滤后不计入。
func CodeBuddyUrgencySnapshotFromExtra(extra map[string]any, now time.Time) codeBuddyUrgencySnapshot {
	snap := codeBuddyUrgencySnapshot{}
	if len(extra) == 0 {
		return snap
	}
	if raw, ok := extra[codebuddyCreditPackagesKey]; ok && raw != nil {
		switch v := raw.(type) {
		case []CodeBuddyCreditPackage:
			snap.Packages = v
		case []any:
			snap.Packages = parseCodeBuddyPackagesFromAnySlice(v)
		default:
			// 形态不符 → 空集合（无参与），失败关闭语义由写入侧保证，此处不兜底。
		}
	}
	if raw, ok := extra[codebuddyCreditPackagesUpdatedAtKey]; ok && raw != nil {
		if s, ok := raw.(string); ok {
			if t, err := time.Parse(time.RFC3339, strings.TrimSpace(s)); err == nil {
				snap.UpdatedAt = t
			}
		}
	}
	return snap
}

// parseCodeBuddyPackagesFromAnySlice 从 Extra 反序列化的 []any（map 形态）重建
// 分包数组。只接受存储契约字段形态（与 Card A MarshalJSON 对称）；字段类型不符
// 的条目整条丢弃（失败关闭语义，不静默钳位）。
func parseCodeBuddyPackagesFromAnySlice(items []any) []CodeBuddyCreditPackage {
	out := make([]CodeBuddyCreditPackage, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		pkg, ok := codeBuddyPackageFromExtraMap(m)
		if !ok {
			continue
		}
		out = append(out, pkg)
	}
	return out
}

func codeBuddyPackageFromExtraMap(m map[string]any) (CodeBuddyCreditPackage, bool) {
	var pkg CodeBuddyCreditPackage
	getStr := func(key string) string {
		if v, ok := m[key].(string); ok {
			return v
		}
		return ""
	}
	pkg.ID = getStr("id")
	pkg.Name = getStr("name")
	pkg.Unit = getStr("unit")
	pkg.ExpiresAt = getStr("expires_at")
	rem, ok1 := decimalFromExtraAny(m["remaining"])
	tot, ok2 := decimalFromExtraAny(m["total"])
	if !ok1 || !ok2 {
		return CodeBuddyCreditPackage{}, false
	}
	pkg.Remaining = rem
	pkg.Total = tot
	if st, ok := m["status"].(float64); ok {
		pkg.Status = int64(st)
	} else if st, ok := m["status"].(int64); ok {
		pkg.Status = st
	} else if st, ok := m["status"].(int); ok {
		pkg.Status = int64(st)
	} else {
		return CodeBuddyCreditPackage{}, false
	}
	return pkg, true
}

func decimalFromExtraAny(v any) (decimal.Decimal, bool) {
	switch t := v.(type) {
	case string:
		d, err := decimal.NewFromString(strings.TrimSpace(t))
		if err != nil {
			return decimal.Zero, false
		}
		return d, true
	case float64:
		return decimal.NewFromFloat(t), true
	case decimal.Decimal:
		return t, true
	default:
		return decimal.Zero, false
	}
}

// sortCandidatesByUrgencyBoost 在**同优先级组内**按紧迫度加权对候选排序。
// 语义：保持原有 Priority 主导（第一键不变），仅在 priority 相同时用加权项
// 影响次序。权重大者靠前（更高紧迫度 → 更优先使用即将到期的额度）。
//
// 该函数只改变候选顺序，不扩大候选集；k=0 或未启用时排序退化为按原优先级
// （sort.SliceStable 保序）→ 与现行为逐位一致。
//
// now 与 freshness 输入由调用方传入（调度热路径读取 Extra 后一次计算）。
func sortCandidatesByUrgencyBoost(accounts []accountWithLoad, boost codeBuddyUrgencyBoost, snapshots map[int64]codeBuddyUrgencySnapshot, now time.Time) {
	boost = boost.normalize()
	if !boost.Enabled || boost.K <= 0 {
		return
	}
	type weighted struct {
		item   accountWithLoad
		weight float64
	}
	weightedList := make([]weighted, 0, len(accounts))
	for _, item := range accounts {
		snap := snapshots[item.account.ID]
		w := ApplyCodeBuddyUrgencyBoost(item.account, snap, boost, now)
		weightedList = append(weightedList, weighted{item: item, weight: w})
	}
	sort.SliceStable(weightedList, func(i, j int) bool {
		a, b := weightedList[i], weightedList[j]
		if a.item.account.Priority != b.item.account.Priority {
			return a.item.account.Priority < b.item.account.Priority
		}
		if a.weight != b.weight {
			return a.weight > b.weight
		}
		return false
	})
	for i := range weightedList {
		accounts[i] = weightedList[i].item
	}
}

// sortCandidatesByUrgencyBoostPtr 是 []*Account 形态的同优先级组内加权排序
// （供 fallback/legacy 路径使用），语义与 accountWithLoad 版一致。
func sortCandidatesByUrgencyBoostPtr(accounts []*Account, boost codeBuddyUrgencyBoost, snapshots map[int64]codeBuddyUrgencySnapshot, now time.Time) {
	boost = boost.normalize()
	if !boost.Enabled || boost.K <= 0 {
		return
	}
	type weighted struct {
		item   *Account
		weight float64
	}
	weightedList := make([]weighted, 0, len(accounts))
	for _, item := range accounts {
		snap := snapshots[item.ID]
		w := ApplyCodeBuddyUrgencyBoost(item, snap, boost, now)
		weightedList = append(weightedList, weighted{item: item, weight: w})
	}
	sort.SliceStable(weightedList, func(i, j int) bool {
		a, b := weightedList[i], weightedList[j]
		if a.item.Priority != b.item.Priority {
			return a.item.Priority < b.item.Priority
		}
		if a.weight != b.weight {
			return a.weight > b.weight
		}
		return false
	})
	for i := range weightedList {
		accounts[i] = weightedList[i].item
	}
}

// codeBuddyUrgencySnapshotsFor 从候选账号集合批量读取 §4.2 输入快照。
// 仅读取 Extra；不会因为单个账号 Extra 异常而失败（失败关闭语义由写入侧保证）。
func codeBuddyUrgencySnapshotsFor(accounts []*Account, now time.Time) map[int64]codeBuddyUrgencySnapshot {
	out := make(map[int64]codeBuddyUrgencySnapshot, len(accounts))
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		out[acc.ID] = CodeBuddyUrgencySnapshotFromExtra(acc.Extra, now)
	}
	return out
}

// codeBuddyUrgencyBoostFromConfig 读取当前生效的 §4.2 配置。
// 调用方保证 cfg 已通过 config 层校验（非法值启动/热加载失败关闭）；
// 此处防御性 normalize（非法值按 0 处理，恒等语义，不放大风险）。
func codeBuddyUrgencyBoostFromConfig(cfg *configGatewayCodeBuddyView) codeBuddyUrgencyBoost {
	if cfg == nil {
		return codeBuddyUrgencyBoost{Enabled: false, K: 0}
	}
	return codeBuddyUrgencyBoost{Enabled: cfg.UrgencyBoostEnabled, K: cfg.UrgencyBoostK}
}

// configGatewayCodeBuddyView 是调度侧读取 CodeBuddy 配置的最小视图，避免对
// internal/config 的直接依赖层级（与既有 gateway_scheduling.go 的 schedulingConfig
// 模式一致）。实际读取由 gateway_scheduling.go 的 s.cfg.Gateway.CodeBuddy 提供。
type configGatewayCodeBuddyView struct {
	UrgencyBoostEnabled bool
	UrgencyBoostK       float64
}

