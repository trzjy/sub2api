package service

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"
)

// 本文件实现异常调用分析的规则引擎纯函数（派发单 U4a）。
//
// 设计约束（方案 §4 / 派发单禁区）：
//   - 纯函数：无 IO、无 DB、无 redis、无 net；不读取时钟（time.Now）或全局时区，
//     所有时间/时区相关值一律由参数传入（RuleContext / *Facts 携带）。
//   - 全部阈值/开关/白名单来自 UsageRiskPolicy 参数，不读 settings、不读全局单例。
//   - 不给 insufficient 状态编造基线、不做兜底打分；样本/历史不足时显式记录、0 分。
//   - 本文件只依赖 Go 标准库，U4b（编排）与本文件通过下方类型对接。
//
// ruleWeight 是 §4 规则表“权重”列（固定不可配置，不进 UsageRiskPolicy）。
var ruleWeight = map[string]int{
	"R1":  25,
	"R2":  20,
	"R3a": 15,
	"R3b": 10,
	"R4":  15,
	"R5":  25,
	"R6":  10,
	"R7":  10,
	"R8":  10,
}

// evidenceMaxBytes 是单行 evidence 的硬上限（方案 §6.2：≤256KB）。
const evidenceMaxBytes = 256 * 1024

// ───────────────────────── 输入事实结构 ─────────────────────────

// DayFacts 是分组级（user_id + group_id 当日）事实。字段以能表达 §4 中
// 分组作用域规则（R2 / R3a 分组作用域 / R3b / R7）的输入为准。
type DayFacts struct {
	// BucketInstants 是当日该分组作用域 distinct 桶起点（UTC instant）。
	// 主要供 24h 热力与未来分组级分析使用；R1 的活跃小时在用户级统计。
	BucketInstants []time.Time
	// RequestCount 是当日该分组合格行（subscription_id IS NOT NULL 且过门槛）总请求数。
	RequestCount int
	// InputTokens / OutputTokens / CacheReadTokens 是分组级 token 汇总（R7）。
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
	// CostUSD 是 sum(actual_cost)，分组级（R2 离群基线对照）。
	CostUSD float64
	// OccupiedMs 是 sum(duration_ms)，R3b 占用率分子。
	OccupiedMs int64
	// RPMLineMinutesGroup 是阶段一已产出的“用户-分组作用域 RPM 贴线命中分钟数”（R3a 分组作用域）。
	// 其计算已采用 checkRPM 的分组限额选择语义，本函数直接消费。
	RPMLineMinutesGroup int
}

// IPClusterFact 描述某非空 IP 当日关联的订阅资格用户聚簇事实（R5）。
// DistinctUserCount / TotalRequests 均由阶段二按 IP 跨分组累计（含当日全部关联用户）。
type IPClusterFact struct {
	DistinctUserCount int // 该 IP 当日 distinct 订阅资格 user_id 数（跨分组累计）
	TotalRequests     int // 该 IP 当日资格请求总量
}

// UserFacts 是用户级（user_id 当日）事实。字段以能表达 §4 中用户作用域规则
// （R1 / R4 / R5 / R6 / R8 / R3a 用户全局作用域）的输入为准。
type UserFacts struct {
	// ActiveBucketInstants 是用户当日活跃桶起点（UTC instant）全集（跨其全部分组合并后 distinct）。
	// R1 活跃小时 = len(distinct ActiveBucketInstants)；DST 回拨日两个同名墙钟小时计 2 桶（instant 不同）。
	ActiveBucketInstants []time.Time
	// ActiveHourHeatmap 是本地墙钟小时 → 请求数（阶段一以应用时区换算后传入），供 24h 热力证据。
	// 键为本地小时（0..23，DST 特殊日可达 24），长度 = 当日实际小时数。
	ActiveHourHeatmap map[int]int
	// DistinctIPCount 是当日 distinct ip_address（精确值，R4）。
	DistinctIPCount int
	// IPClusters 以“当前用户实际使用的 IP”为键，值为该 IP 的跨组聚簇事实（R5）。
	// R5 命中判据：任一键满足 DistinctUserCount ≥ 阈值 且 TotalRequests ≥ 阈值。
	IPClusters map[string]IPClusterFact
	// UADistribution 是 UA 字符串 → 请求数（原始分布，R6 在此做白名单子串匹配，evidence 取 top-K）。
	UADistribution map[string]int
	// KeyCounts 是 api_key_id（字符串）→ 请求数（跨用户全部分组聚合，R8）。
	KeyCounts map[string]int
	// GlobalRPMHitMinutes 是阶段一按 user+minute 独立现算的“用户全局作用域 RPM 贴线命中分钟数”（R3a 用户全局作用域）。
	// 其计算已采用 checkRPM 的 user.rpm_limit 全局天花板语义，本函数直接消费。
	GlobalRPMHitMinutes int
}

// RuleContext 是规则引擎运行期上下文（全部由编排层/ctx 参数传入，无全局读取）。
type RuleContext struct {
	// DayStart / DayEnd 是应用时区日界起止 instant（用于证据与未来扩展）。
	DayStart time.Time
	DayEnd   time.Time
	// ElapsedHours 是 [DayStart, DayEnd) 的实际 elapsed 时长（小时），
	// 按实际值：23/23.5/24/24.5/25h 日分别传入对应值（R3b 分母）。
	ElapsedHours float64

	// R1HistoryCovered 表示 R1 所需回看期历史桶是否已覆盖；未覆盖 → insufficient_history。
	R1HistoryCovered bool
	// R1ConsecutiveDays 是连续活跃天数输入（活跃小时 ≥ 阈值的天数，来自 rollup 历史桶）。
	R1ConsecutiveDays int

	// R2PeerCost 是 peer 群当日日 cost 切片（含被测用户自身；同资格谓词 + min_daily_requests 门槛）。
	R2PeerCost []float64
	// R2Filter 是 peer 群筛选条件描述（evidence 记录）。
	R2Filter string
	// R2Algorithm 是所用分位算法名（evidence 记录，如 "linear-interpolation-p95"）。
	R2Algorithm string

	// 当前限额三元组（分析执行时的当前有效配置，非历史）：
	UserRPMLimit  int  // users.rpm_limit（全局天花板）
	GroupRPMLimit int  // groups.rpm_limit
	Override       *int // user×group override；nil=无 override；*override=0 表示免除分组检查
	// LimitSnapshotTime 是限额快照计算时间（evidence 记录）。
	LimitSnapshotTime time.Time

	// TZName 是应用时区名（证据留痕）。
	TZName string
}

// ───────────────────────── 输出结构 ─────────────────────────

// RuleHit 是单条规则命中记录（方案 §6.2 rule_hits 形状 {rule, scope, detail, points}）。
type RuleHit struct {
	Rule   string `json:"rule"`
	Scope  string `json:"scope"` // "user" | "group"
	Detail string `json:"detail"`
	Points int    `json:"points"`
}

// RuleOutcome 是 EvaluateRules 的产出。
type RuleOutcome struct {
	Score    int
	Level    string // "critical" | "high" | "medium" | "low"
	RuleHits []RuleHit
	// InsufficientHistory 表示 R1 因回看期未覆盖而未计算（0 分、不入榜）。
	InsufficientHistory bool
	// InsufficientPeers 表示 R2 因 peer 群样本不足而未计算（显式记录、不兜底）。
	InsufficientPeers bool
}

// ───────────────────────── 限额选择语义（复用 checkRPM） ─────────────────────────

// groupApplicableLimit 实现 checkRPM 第一层分组限额选择语义（billing_cache_service.go:781）：
//   - override != nil 且 *override > 0：override 替代分组限，返回 *override；
//   - override != nil 且 *override == 0：该用户该分组免除分组检查，返回 0（无分组限额）；
//   - override == nil 且 groupLimit > 0：返回 groupLimit；
//   - 否则返回 0（无分组限额，分组作用域不可命中）。
//
// user.rpm_limit 为全局天花板，始终在用户作用域独立生效（见 EvaluateRules 的 R3a 全局作用域分支）。
func groupApplicableLimit(override *int, groupLimit int) int {
	if override != nil {
		if *override == 0 {
			return 0 // 免除分组检查
		}
		return *override // override 替代分组限
	}
	if groupLimit > 0 {
		return groupLimit
	}
	return 0
}

// ───────────────────────── 分位计算（线性插值 P95） ─────────────────────────

// percentile 对升序切片 sorted 计算 p 分位数，采用线性插值（方案 §4 R2：P95 线性插值）。
// 调用方须保证 sorted 已升序；空切片返回 0。
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	rank := p * float64(n-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return sorted[lo]
	}
	frac := rank - float64(lo)
	return sorted[lo] + frac*(sorted[hi]-sorted[lo])
}

// ───────────────────────── 主入口：逐条实现 R1–R8 ─────────────────────────

// EvaluateRules 是规则引擎唯一权威纯函数路径：输入 policy + 分组级事实 + 用户级事实 + 上下文，
// 逐条实现 R1–R8 并产出 RuleOutcome（分值映射：≥100 critical / 70–99 high / 40–69 medium / 其余 low）。
//
// 每规则先查 policy.<rule>.Enabled；关闭则整条跳过（含 insufficient 状态一律不留痕，rule_hits 中该规则缺席）。
func EvaluateRules(policy UsageRiskPolicy, day DayFacts, user UserFacts, ctx RuleContext) RuleOutcome {
	out := RuleOutcome{}

	addHit := func(rule, scope, detail string, points int) {
		out.RuleHits = append(out.RuleHits, RuleHit{Rule: rule, Scope: scope, Detail: detail, Points: points})
		out.Score += points
	}

	// ── R1 全天候活跃（scope=user） ──
	if policy.R1Enabled {
		if !ctx.R1HistoryCovered {
			// 回看期未覆盖 → insufficient_history，0 分、不入榜（显式记录，不编造基线）。
			out.InsufficientHistory = true
			addHit("R1", "user", "insufficient_history: lookback window not covered", 0)
		} else {
			activeHours := distinctInstantCount(user.ActiveBucketInstants)
			if activeHours >= policy.R1ActiveHours && ctx.R1ConsecutiveDays >= policy.R1ConsecutiveDays {
				addHit("R1", "user",
					fmt.Sprintf("active_hours=%d threshold=%d consecutive_days=%d threshold=%d",
						activeHours, policy.R1ActiveHours, ctx.R1ConsecutiveDays, policy.R1ConsecutiveDays),
					ruleWeight["R1"])
			}
		}
	}

	// ── R2 用量离群（scope=group） ──
	if policy.R2Enabled {
		peerCount := len(ctx.R2PeerCost)
		if peerCount < policy.R2PeerCount {
			// 样本不足 → insufficient_peers（显式记录，不兜底、不静默）。
			out.InsufficientPeers = true
			addHit("R2", "group",
				fmt.Sprintf("insufficient_peers: peer_count=%d threshold=%d", peerCount, policy.R2PeerCount), 0)
		} else {
			sorted := append([]float64(nil), ctx.R2PeerCost...)
			sort.Float64s(sorted)
			p95 := percentile(sorted, 0.95)
			threshold := p95 * policy.R2Multiple
			if day.CostUSD > threshold {
				addHit("R2", "group",
					fmt.Sprintf("cost=%.6f p95=%.6f multiple=%.3f threshold=%.6f peer_count=%d",
						day.CostUSD, p95, policy.R2Multiple, threshold, peerCount),
					ruleWeight["R2"])
			}
		}
	}

	// ── R3a RPM 贴线（双作用域） ──
	if policy.R3AEnabled {
		groupLimit := groupApplicableLimit(ctx.Override, ctx.GroupRPMLimit)
		// 分组作用域：适用分组限额存在（override>0 或 group.rpm_limit>0；override=0 免除）且命中分钟达阈值。
		if groupLimit > 0 && day.RPMLineMinutesGroup >= policy.R3AMinutes {
			addHit("R3a", "group",
				fmt.Sprintf("group_limit=%d hit_minutes=%d threshold=%d override=%s",
					groupLimit, day.RPMLineMinutesGroup, policy.R3AMinutes, overrideRepr(ctx.Override)),
				ruleWeight["R3a"])
		}
		// 用户全局作用域：user.rpm_limit 为全局天花板，始终独立生效。
		if ctx.UserRPMLimit > 0 && user.GlobalRPMHitMinutes >= policy.R3AMinutes {
			addHit("R3a", "user",
				fmt.Sprintf("user_limit=%d hit_minutes=%d threshold=%d",
					ctx.UserRPMLimit, user.GlobalRPMHitMinutes, policy.R3AMinutes),
				ruleWeight["R3a"])
		}
	}

	// ── R3b 占用率贴线（scope=group） ──
	if policy.R3BEnabled {
		elapsedMs := ctx.ElapsedHours * 3600 * 1000
		if elapsedMs > 0 {
			occupancy := float64(day.OccupiedMs) / elapsedMs
			if occupancy >= policy.R3BOccupancy {
				addHit("R3b", "group",
					fmt.Sprintf("occupancy=%.4f occupied_ms=%d elapsed_ms=%.0f threshold=%.4f",
						occupancy, day.OccupiedMs, elapsedMs, policy.R3BOccupancy),
					ruleWeight["R3b"])
			}
		}
	}

	// ── R4 IP 离散（scope=user） ──
	if policy.R4Enabled {
		if user.DistinctIPCount >= policy.R4DistinctIP {
			addHit("R4", "user",
				fmt.Sprintf("distinct_ip=%d threshold=%d", user.DistinctIPCount, policy.R4DistinctIP),
				ruleWeight["R4"])
		}
	}

	// ── R5 同 IP 聚簇（scope=user） ──
	if policy.R5Enabled {
		// map 遍历序不确定，多 IP 同时达标时 detail 展示须确定性：先收集全部达标 IP，
		// 按 (TotalRequests 降序, IP 字典序升序) 取首项（与 buildIPEvidence 口径一致）；
		// 命中判定本身（任一达标即命中）与分数不变。
		var cands []IPEvidence
		for ip, f := range user.IPClusters {
			if ip == "" {
				continue // 空 IP 行一律排除（方案 §4 R5）
			}
			if f.DistinctUserCount >= policy.R5UserCount && f.TotalRequests >= policy.R5IPRequests {
				cands = append(cands, IPEvidence{IP: ip, DistinctUsers: f.DistinctUserCount, Requests: f.TotalRequests})
			}
		}
		if len(cands) > 0 {
			sort.Slice(cands, func(i, j int) bool {
				if cands[i].Requests != cands[j].Requests {
					return cands[i].Requests > cands[j].Requests
				}
				return cands[i].IP < cands[j].IP
			})
			c := cands[0]
			addHit("R5", "user",
				fmt.Sprintf("ip=%s distinct_users=%d threshold=%d requests=%d threshold=%d",
					c.IP, c.DistinctUsers, policy.R5UserCount, c.Requests, policy.R5IPRequests),
				ruleWeight["R5"])
		}
	}

	// ── R6 客户端指纹（scope=user） ──
	if policy.R6Enabled {
		total := 0
		nonWhite := 0
		for ua, c := range user.UADistribution {
			total += c
			if !isWhitelistedUA(ua, policy.UAWhitelist) {
				nonWhite += c
			}
		}
		if total > 0 {
			ratio := float64(nonWhite) / float64(total)
			if ratio >= policy.R6UARatio {
				addHit("R6", "user",
					fmt.Sprintf("non_whitelisted_ua=%d total_ua=%d ratio=%.4f threshold=%.4f",
						nonWhite, total, ratio, policy.R6UARatio),
					ruleWeight["R6"])
			}
		}
	}

	// ── R7 缓存失效（scope=group） ──
	if policy.R7Enabled {
		denom := day.InputTokens + day.CacheReadTokens
		if denom > 0 { // 分母为 0（无输入）不命中且不除零
			ratio := float64(day.CacheReadTokens) / float64(denom)
			if ratio <= policy.R7CacheRatio && day.InputTokens >= policy.R7MinInputTokens {
				addHit("R7", "group",
					fmt.Sprintf("cache_ratio=%.4f threshold=%.4f input_tokens=%d threshold=%d",
						ratio, policy.R7CacheRatio, day.InputTokens, policy.R7MinInputTokens),
					ruleWeight["R7"])
			}
		}
	}

	// ── R8 多 key 均摊（scope=user） ──
	if policy.R8Enabled {
		keys := make([]string, 0, len(user.KeyCounts))
		total := 0
		for k, c := range user.KeyCounts {
			keys = append(keys, k)
			total += c
		}
		if len(keys) >= policy.R8KeyCount && total > 0 {
			allAbove := true
			for _, k := range keys {
				if float64(user.KeyCounts[k])/float64(total) < policy.R8KeyRatio {
					allAbove = false
					break
				}
			}
			if allAbove {
				addHit("R8", "user",
					fmt.Sprintf("key_count=%d threshold=%d min_share=%.4f threshold=%.4f",
						len(keys), policy.R8KeyCount, minKeyShare(user.KeyCounts, total), policy.R8KeyRatio),
					ruleWeight["R8"])
			}
		}
	}

	out.Level = scoreLevel(out.Score)
	return out
}

// scoreLevel 实现分值映射：≥100 critical / 70–99 high / 40–69 medium / 其余 low。
func scoreLevel(score int) string {
	switch {
	case score >= 100:
		return "critical"
	case score >= 70:
		return "high"
	case score >= 40:
		return "medium"
	default:
		return "low"
	}
}

// distinctInstantCount 返回 instants 中 distinct（按瞬时相等）的数量。
// DST 回拨日两个同名墙钟小时对应不同 UTC instant，故计 2 桶（方案 §5 DST 公式）。
func distinctInstantCount(instants []time.Time) int {
	seen := make(map[time.Time]struct{}, len(instants))
	for _, t := range instants {
		seen[t] = struct{}{}
	}
	return len(seen)
}

// isWhitelistedUA 判断 ua 是否命中任一白名单子串（子串匹配，方案 §4 R6）。
func isWhitelistedUA(ua string, whitelist []string) bool {
	for _, w := range whitelist {
		if w != "" && containsSubstr(ua, w) {
			return true
		}
	}
	return false
}

// containsSubstr 是大小写敏感的子串包含（与方案“子串表”语义一致）。
func containsSubstr(s, sub string) bool {
	if len(sub) == 0 {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func minKeyShare(counts map[string]int, total int) float64 {
	if total <= 0 {
		return 0
	}
	min := math.MaxFloat64
	for _, c := range counts {
		share := float64(c) / float64(total)
		if share < min {
			min = share
		}
	}
	return min
}

func overrideRepr(o *int) string {
	if o == nil {
		return "nil"
	}
	if *o == 0 {
		return "0(exempt)"
	}
	return fmt.Sprintf("%d", *o)
}

// ───────────────────────── 证据装配 ─────────────────────────

// IPEvidence 是单条 IP 证据（含关联用户数）。
type IPEvidence struct {
	IP            string `json:"ip"`
	DistinctUsers int    `json:"distinct_users"`
	Requests      int    `json:"requests"`
}

// UAEvidence 是单条 UA 证据。
type UAEvidence struct {
	UA    string `json:"ua"`
	Count int    `json:"count"`
}

// KeyEvidence 是单条 key 证据。
type KeyEvidence struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

// R2EvidenceContext 是 R2 群分位上下文（方案 §6.2 evidence）。
type R2EvidenceContext struct {
	PeerCount int     `json:"peer_count"`
	Filter    string  `json:"filter"`
	Algorithm string  `json:"algorithm"`
	P95       float64 `json:"p95"`
}

// UsageRiskEvidence 是 AssembleEvidence 产出、最终落库的报告 evidence（jsonb）。
//
// 契约冻结（R5-1 K / F13）：顶层 json 键集与字段名前后端必须一致，任何重命名都破坏
// 前端消费，故以下 tag 为权威契约，不得更改：
//   active_hours(用户跨分组 distinct 桶数), ip_top, ua_top, key_distribution,
//   heatmap, r2_context, truncated, original_bytes?(超限截断时才有)。
type UsageRiskEvidence struct {
	Truncated       bool              `json:"truncated"`
	OriginalBytes   int               `json:"original_bytes,omitempty"`
	ActiveHours     int               `json:"active_hours"`
	IPs             []IPEvidence      `json:"ip_top"`
	UAs             []UAEvidence      `json:"ua_top"`
	Keys            []KeyEvidence     `json:"key_distribution"`
	HourHeatmap     map[string]int    `json:"heatmap"`
	R2Context       R2EvidenceContext `json:"r2_context"`
}

// AssembleEvidence 装配阶段二明细证据：ip_top ≤50、ua_top ≤20、key 分布 ≤20、
// 24h 热力（当日实际小时数）、R2 群分位上下文。单行 evidence ≤256KB，超限截断并
// 显式标记 truncated:true + 原始字节数（original_bytes）。
//
// 纯函数约束同 EvaluateRules：不读全局时钟/时区，全部输入来自参数。
func AssembleEvidence(day DayFacts, user UserFacts, ctx RuleContext) UsageRiskEvidence {
	ev := UsageRiskEvidence{
		ActiveHours:  distinctInstantCount(user.ActiveBucketInstants),
		IPs:          buildIPEvidence(user.IPClusters, 50),
		UAs:          buildUAEvidence(user.UADistribution, 20),
		Keys:         buildKeyEvidence(user.KeyCounts, 20),
		HourHeatmap:  buildHeatmap(user.ActiveHourHeatmap),
		R2Context:    buildR2Context(ctx),
	}

	raw, err := json.Marshal(ev)
	if err != nil {
		// 结构化序列化不应失败；失败回退为截断标记空证据，由调用方仍落库（审计可查）。
		ev.Truncated = true
		ev.OriginalBytes = 0
		return ev
	}
	orig := len(raw)

	// 上限口径 = 最终落库形态（调用方重新序列化的完整 ev）。用不含 original_bytes 的 raw
	// 判循环会漏 25 字节级越限（original_bytes 键 + 值 + 标点），故仅在确认超限时设置
	// original_bytes/truncated，且裁剪循环条件与最终校验一律采用含全部最终字段的序列化结果。
	if orig > evidenceMaxBytes {
		ev.Truncated = true
		ev.OriginalBytes = orig
		// 由“最不重要”的尾部开始裁剪（数组已按重要性降序），直至含全部字段的重新序列化
		// ≤ 上限；若数组已空仍超限（极端长单值），保留截断标记退出（既有语义，不新增兜底）。
		for {
			raw, _ = json.Marshal(ev)
			if len(raw) <= evidenceMaxBytes {
				break
			}
			if len(ev.IPs) > 0 {
				ev.IPs = ev.IPs[:len(ev.IPs)-1]
			} else if len(ev.UAs) > 0 {
				ev.UAs = ev.UAs[:len(ev.UAs)-1]
			} else if len(ev.Keys) > 0 {
				ev.Keys = ev.Keys[:len(ev.Keys)-1]
			} else {
				break
			}
		}
	}
	return ev
}

func buildIPEvidence(clusters map[string]IPClusterFact, topK int) []IPEvidence {
	out := make([]IPEvidence, 0, len(clusters))
	for ip, f := range clusters {
		out = append(out, IPEvidence{IP: ip, DistinctUsers: f.DistinctUserCount, Requests: f.TotalRequests})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].IP < out[j].IP
	})
	if len(out) > topK {
		out = out[:topK]
	}
	return out
}

func buildUAEvidence(dist map[string]int, topK int) []UAEvidence {
	out := make([]UAEvidence, 0, len(dist))
	for ua, c := range dist {
		out = append(out, UAEvidence{UA: ua, Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].UA < out[j].UA
	})
	if len(out) > topK {
		out = out[:topK]
	}
	return out
}

func buildKeyEvidence(counts map[string]int, topK int) []KeyEvidence {
	out := make([]KeyEvidence, 0, len(counts))
	for k, c := range counts {
		out = append(out, KeyEvidence{Key: k, Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	if len(out) > topK {
		out = out[:topK]
	}
	return out
}

func buildHeatmap(hm map[int]int) map[string]int {
	if hm == nil {
		return map[string]int{}
	}
	out := make(map[string]int, len(hm))
	for h, c := range hm {
		out[fmt.Sprintf("%d", h)] = c
	}
	return out
}

func buildR2Context(ctx RuleContext) R2EvidenceContext {
	sorted := append([]float64(nil), ctx.R2PeerCost...)
	sort.Float64s(sorted)
	return R2EvidenceContext{
		PeerCount: len(ctx.R2PeerCost),
		Filter:    ctx.R2Filter,
		Algorithm: ctx.R2Algorithm,
		P95:       percentile(sorted, 0.95),
	}
}
