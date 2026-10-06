package service

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// defaultPolicy 返回与方案 §4 默认阈值一致的 UsageRiskPolicy（全部规则 Enabled=true）。
func defaultPolicy() UsageRiskPolicy {
	return UsageRiskPolicy{
		Enabled:             true,
		UnlimitedGroupsOnly: true,
		MinDailyRequests:    10,
		R1ActiveHours:         20,
		R1ConsecutiveDays:     3,
		R1Enabled:             true,
		R2Multiple:            1.5,
		R2PeerCount:           10,
		R2Enabled:             true,
		R3ARatio:              0.9,
		R3AMinutes:            30,
		R3AEnabled:            true,
		R3BOccupancy:          0.5,
		R3BEnabled:            true,
		R4DistinctIP:          10,
		R4Enabled:             true,
		R5UserCount:           3,
		R5IPRequests:          100,
		R5Enabled:             true,
		R6UARatio:             0.8,
		R6Enabled:             true,
		R7CacheRatio:          0.05,
		R7MinInputTokens:      5000000,
		R7Enabled:             true,
		R8KeyCount:            3,
		R8KeyRatio:            0.2,
		R8Enabled:             true,
		UAWhitelist:           []string{},
		ListingMinScore:       40,
		RetentionDays:         90,
	}
}

func instants(n int) []time.Time {
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	out := make([]time.Time, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, base.Add(time.Duration(i)*time.Hour))
	}
	return out
}

func ptrInt(v int) *int { return &v }

func hasHit(out RuleOutcome, rule, scope string) (RuleHit, bool) {
	for _, h := range out.RuleHits {
		if h.Rule == rule && h.Scope == scope {
			return h, true
		}
	}
	return RuleHit{}, false
}

func ruleScore(out RuleOutcome, rule string) int {
	s := 0
	for _, h := range out.RuleHits {
		if h.Rule == rule {
			s += h.Points
		}
	}
	return s
}

// hitOK 是 hasHit 的单值便捷封装（仅判断是否存在命中）。
func hitOK(out RuleOutcome, rule, scope string) bool {
	_, ok := hasHit(out, rule, scope)
	return ok
}

// TestUsageRiskRules 表驱动覆盖 R1–R8 命中/不命中/阈值边界（±1 边界）。
func TestUsageRiskRules(t *testing.T) {
	// ── R1 ──
	t.Run("R1", func(t *testing.T) {
		p := defaultPolicy()
		// 命中：活跃 20h 且连续 3 天。
		out := EvaluateRules(p, DayFacts{}, UserFacts{ActiveBucketInstants: instants(20)}, RuleContext{R1HistoryCovered: true, R1ConsecutiveDays: 3})
		if h, ok := hasHit(out, "R1", "user"); !ok || h.Points != 25 {
			t.Fatalf("R1 应命中: hit=%v score=%d", ok, ruleScore(out, "R1"))
		}
		// 边界 -1：活跃 19h。
		out = EvaluateRules(p, DayFacts{}, UserFacts{ActiveBucketInstants: instants(19)}, RuleContext{R1HistoryCovered: true, R1ConsecutiveDays: 3})
		if hitOK(out, "R1", "user") {
			t.Fatalf("R1 活跃19h 不应命中")
		}
		// 边界 -1：连续 2 天。
		out = EvaluateRules(p, DayFacts{}, UserFacts{ActiveBucketInstants: instants(20)}, RuleContext{R1HistoryCovered: true, R1ConsecutiveDays: 2})
		if hitOK(out, "R1", "user") {
			t.Fatalf("R1 连续2天 不应命中")
		}
		// 边界：连续恰 3 天 + 活跃恰 20h → 命中。
		out = EvaluateRules(p, DayFacts{}, UserFacts{ActiveBucketInstants: instants(20)}, RuleContext{R1HistoryCovered: true, R1ConsecutiveDays: 3})
		if !hitOK(out, "R1", "user") {
			t.Fatalf("R1 边界(20h,3天) 应命中")
		}
	})

	// ── R2 ──
	t.Run("R2", func(t *testing.T) {
		p := defaultPolicy()
		peers := make([]float64, 10)
		for i := range peers {
			peers[i] = 100
		}
		ctx := RuleContext{R2PeerCost: peers, R2Filter: "f", R2Algorithm: "linear-interpolation-p95"}
		// 命中：cost=200 > P95(100)*1.5=150。
		out := EvaluateRules(p, DayFacts{CostUSD: 200}, UserFacts{}, ctx)
		if h, ok := hasHit(out, "R2", "group"); !ok || h.Points != 20 {
			t.Fatalf("R2 应命中: hit=%v", ok)
		}
		// 不命中：cost=140 < 150。
		out = EvaluateRules(p, DayFacts{CostUSD: 140}, UserFacts{}, ctx)
		if hitOK(out, "R2", "group") {
			t.Fatalf("R2 cost=140 不应命中")
		}
		// 边界：cost 恰等于阈值 150 → 严格 > 不命中。
		out = EvaluateRules(p, DayFacts{CostUSD: 150}, UserFacts{}, ctx)
		if hitOK(out, "R2", "group") {
			t.Fatalf("R2 cost=150(==阈值) 不应命中（严格 >）")
		}
		// 边界 +1：cost=150.01 → 命中。
		out = EvaluateRules(p, DayFacts{CostUSD: 150.01}, UserFacts{}, ctx)
		if !hitOK(out, "R2", "group") {
			t.Fatalf("R2 cost=150.01 应命中")
		}
	})

	// ── R3b ──
	t.Run("R3b", func(t *testing.T) {
		p := defaultPolicy()
		// 分母 24h = 86400000ms；occupied 恰 0.5 → 命中（>=）。
		out := EvaluateRules(p, DayFacts{OccupiedMs: 43200000}, UserFacts{}, RuleContext{ElapsedHours: 24})
		if h, ok := hasHit(out, "R3b", "group"); !ok || h.Points != 10 {
			t.Fatalf("R3b 0.5 应命中")
		}
		// 边界 -1：occupied=43199999 → 0.49999 < 0.5 不命中。
		out = EvaluateRules(p, DayFacts{OccupiedMs: 43199999}, UserFacts{}, RuleContext{ElapsedHours: 24})
		if hitOK(out, "R3b", "group") {
			t.Fatalf("R3b 边界-1 不应命中")
		}
		// 分母为 0（ElapsedHours=0）不命中且不除零。
		out = EvaluateRules(p, DayFacts{OccupiedMs: 999999999}, UserFacts{}, RuleContext{ElapsedHours: 0})
		if hitOK(out, "R3b", "group") {
			t.Fatalf("R3b 分母0 不应命中")
		}
	})

	// ── R4 ──
	t.Run("R4", func(t *testing.T) {
		p := defaultPolicy()
		out := EvaluateRules(p, DayFacts{}, UserFacts{DistinctIPCount: 10}, RuleContext{})
		if h, ok := hasHit(out, "R4", "user"); !ok || h.Points != 15 {
			t.Fatalf("R4 10 应命中")
		}
		out = EvaluateRules(p, DayFacts{}, UserFacts{DistinctIPCount: 9}, RuleContext{})
		if hitOK(out, "R4", "user") {
			t.Fatalf("R4 9 不应命中")
		}
	})

	// ── R5 ──
	t.Run("R5", func(t *testing.T) {
		p := defaultPolicy()
		// 命中：某 IP distinct_users=5 ≥3 且 requests=200 ≥100。
		uf := UserFacts{IPClusters: map[string]IPClusterFact{"1.2.3.4": {DistinctUserCount: 5, TotalRequests: 200}}}
		out := EvaluateRules(p, DayFacts{}, uf, RuleContext{})
		if h, ok := hasHit(out, "R5", "user"); !ok || h.Points != 25 {
			t.Fatalf("R5 应命中")
		}
		// 边界：distinct_users=2 <3 不命中。
		uf = UserFacts{IPClusters: map[string]IPClusterFact{"1.2.3.4": {DistinctUserCount: 2, TotalRequests: 200}}}
		out = EvaluateRules(p, DayFacts{}, uf, RuleContext{})
		if hitOK(out, "R5", "user") {
			t.Fatalf("R5 distinct_users=2 不应命中")
		}
		// 边界：distinct_users=3 恰达标 命中。
		uf = UserFacts{IPClusters: map[string]IPClusterFact{"1.2.3.4": {DistinctUserCount: 3, TotalRequests: 200}}}
		out = EvaluateRules(p, DayFacts{}, uf, RuleContext{})
		if !hitOK(out, "R5", "user") {
			t.Fatalf("R5 distinct_users=3 应命中")
		}
		// 边界：requests=99 <100 不命中。
		uf = UserFacts{IPClusters: map[string]IPClusterFact{"1.2.3.4": {DistinctUserCount: 5, TotalRequests: 99}}}
		out = EvaluateRules(p, DayFacts{}, uf, RuleContext{})
		if hitOK(out, "R5", "user") {
			t.Fatalf("R5 requests=99 不应命中")
		}
		// 空 IP 排除：IPClusters 含空键不应命中。
		uf = UserFacts{IPClusters: map[string]IPClusterFact{"": {DistinctUserCount: 5, TotalRequests: 200}}}
		out = EvaluateRules(p, DayFacts{}, uf, RuleContext{})
		if hitOK(out, "R5", "user") {
			t.Fatalf("R5 空IP 不应命中（已排除）")
		}
		// 跨组共享 IP：当前用户仅用 IP "9.9.9.9"，该 IP 关联 4 用户、300 请求 → 命中。
		// （distinct 用户跨分组累计由阶段二提供，与当前用户是否唯一无关）
		uf = UserFacts{IPClusters: map[string]IPClusterFact{"9.9.9.9": {DistinctUserCount: 4, TotalRequests: 300}}}
		out = EvaluateRules(p, DayFacts{}, uf, RuleContext{})
		if !hitOK(out, "R5", "user") {
			t.Fatalf("R5 跨组共享IP 应命中")
		}
	})

	// ── R6 ──
	t.Run("R6", func(t *testing.T) {
		p := defaultPolicy()
		// 非白名单 8/10 = 0.8 → 命中（>=）。
		uf := UserFacts{UADistribution: map[string]int{"Mozilla/5.0": 8, "curl/8.0": 2}}
		out := EvaluateRules(p, DayFacts{}, uf, RuleContext{})
		if h, ok := hasHit(out, "R6", "user"); !ok || h.Points != 10 {
			t.Fatalf("R6 0.8 应命中")
		}
		// 边界 -1：白名单 "curl/" 使 3 条计入白名单 → 非白名单 7/10=0.7 <0.8 不命中。
		p6 := defaultPolicy()
		p6.UAWhitelist = []string{"curl/"}
		uf = UserFacts{UADistribution: map[string]int{"Mozilla/5.0": 7, "curl/8.0": 3}}
		out = EvaluateRules(p6, DayFacts{}, uf, RuleContext{})
		if hitOK(out, "R6", "user") {
			t.Fatalf("R6 0.7 不应命中")
		}
		// 白名单子串匹配：UA "python-requests/2.0" 命中白名单 "python" → 计入白名单，
		// 非白名单=5/10=0.5 <0.8 不命中。
		p2 := defaultPolicy()
		p2.UAWhitelist = []string{"python"}
		uf = UserFacts{UADistribution: map[string]int{"python-requests/2.0": 5, "Mozilla/5.0": 5}}
		out = EvaluateRules(p2, DayFacts{}, uf, RuleContext{})
		if hitOK(out, "R6", "user") {
			t.Fatalf("R6 白名单匹配后 0.5 不应命中")
		}
		// 白名单子串敏感：无白名单时全非白名单 → 1.0 命中。
		uf = UserFacts{UADistribution: map[string]int{"python-requests/2.0": 5, "Mozilla/5.0": 5}}
		out = EvaluateRules(p, DayFacts{}, uf, RuleContext{})
		if !hitOK(out, "R6", "user") {
			t.Fatalf("R6 无白名单应命中")
		}
	})

	// ── R7 ──
	t.Run("R7", func(t *testing.T) {
		p := defaultPolicy()
		// 分母为 0（无输入）不命中且不除零。
		out := EvaluateRules(p, DayFacts{InputTokens: 0, CacheReadTokens: 0}, UserFacts{}, RuleContext{})
		if hitOK(out, "R7", "group") {
			t.Fatalf("R7 分母0 不应命中且不除零")
		}
		// 命中：cacheRead=0 → ratio=0 ≤0.05，input=5000000 ≥阈值。
		out = EvaluateRules(p, DayFacts{InputTokens: 5000000, CacheReadTokens: 0}, UserFacts{}, RuleContext{})
		if h, ok := hasHit(out, "R7", "group"); !ok || h.Points != 10 {
			t.Fatalf("R7 命中应成立")
		}
		// 不命中：cacheRead 高使 ratio=0.09>0.05。
		out = EvaluateRules(p, DayFacts{InputTokens: 5000000, CacheReadTokens: 500000}, UserFacts{}, RuleContext{})
		if hitOK(out, "R7", "group") {
			t.Fatalf("R7 ratio=0.09 不应命中")
		}
		// 输入未达 5M：ratio≤0.05 但 input<5000000 不命中。
		out = EvaluateRules(p, DayFacts{InputTokens: 100, CacheReadTokens: 0}, UserFacts{}, RuleContext{})
		if hitOK(out, "R7", "group") {
			t.Fatalf("R7 input<5M 不应命中")
		}
	})

	// ── R8 ──
	t.Run("R8", func(t *testing.T) {
		p := defaultPolicy()
		// 命中：3 key 各 40/30/30 → 最小占比 30% ≥20%。
		uf := UserFacts{KeyCounts: map[string]int{"k1": 40, "k2": 30, "k3": 30}}
		out := EvaluateRules(p, DayFacts{}, uf, RuleContext{})
		if h, ok := hasHit(out, "R8", "user"); !ok || h.Points != 10 {
			t.Fatalf("R8 均摊 应命中")
		}
		// 边界 -1：key 数=2 不命中。
		uf = UserFacts{KeyCounts: map[string]int{"k1": 50, "k2": 50}}
		out = EvaluateRules(p, DayFacts{}, uf, RuleContext{})
		if hitOK(out, "R8", "user") {
			t.Fatalf("R8 key=2 不应命中")
		}
		// 边界：某 key 19% <20% 不命中。
		uf = UserFacts{KeyCounts: map[string]int{"k1": 19, "k2": 41, "k3": 40}}
		out = EvaluateRules(p, DayFacts{}, uf, RuleContext{})
		if hitOK(out, "R8", "user") {
			t.Fatalf("R8 最小占比19%% 不应命中")
		}
		// 边界：某 key 恰 20% 命中。
		uf = UserFacts{KeyCounts: map[string]int{"k1": 20, "k2": 40, "k3": 40}}
		out = EvaluateRules(p, DayFacts{}, uf, RuleContext{})
		if !hitOK(out, "R8", "user") {
			t.Fatalf("R8 最小占比20%% 应命中")
		}
	})
}

// TestEvaluateRulesInsufficient 覆盖 insufficient_history 与 insufficient_peers。
func TestEvaluateRulesInsufficient(t *testing.T) {
	p := defaultPolicy()

	// R1 回看期未覆盖 → insufficient_history、0 分、不入榜。
	out := EvaluateRules(p, DayFacts{}, UserFacts{ActiveBucketInstants: instants(24)}, RuleContext{R1HistoryCovered: false})
	if !out.InsufficientHistory {
		t.Fatalf("应标记 InsufficientHistory")
	}
	if ruleScore(out, "R1") != 0 {
		t.Fatalf("R1 insufficient 时分数应为 0")
	}
	if out.Level != "low" {
		t.Fatalf("R1 insufficient 时 level 应为 low")
	}

	// R2 peer 群不足（<10）→ insufficient_peers、显式记录、不兜底。
	peers := make([]float64, 9)
	for i := range peers {
		peers[i] = 100
	}
	ctx := RuleContext{R2PeerCost: peers}
	out = EvaluateRules(p, DayFacts{CostUSD: 999}, UserFacts{}, ctx)
	if !out.InsufficientPeers {
		t.Fatalf("应标记 InsufficientPeers")
	}
	if ruleScore(out, "R2") != 0 {
		t.Fatalf("R2 insufficient 时分数应为 0")
	}
	if h, ok := hasHit(out, "R2", "group"); !ok || h.Points != 0 {
		t.Fatalf("R2 insufficient 应显式记录(hit=%v)", ok)
	}
}

// TestEvaluateRulesDST 覆盖 23/23.5/24/24.5/25h 日的活跃小时（R1）与 R3b 分母断言。
func TestEvaluateRulesDST(t *testing.T) {
	p := defaultPolicy()
	cases := []struct {
		name       string
		instantN   int
		elapsed    float64
		wantActive bool // 活跃小时达 ≥20 是否使 R1 命中（连续天=3）
		// 用 R3b 分母断言：occupiedMs = 0.5 * elapsed*3600*1000 应命中 R3b。
	}{
		{"23h", 23, 23, true},
		{"23.5h", 24, 23.5, true},
		{"24h", 24, 24, true},
		{"24.5h", 25, 24.5, true},
		{"25h", 25, 25, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// R1 活跃小时 = distinct instant 数（DST 回拨日两个同名墙钟小时计 2 桶）。
			ctx := RuleContext{R1HistoryCovered: true, R1ConsecutiveDays: 3}
			out := EvaluateRules(p, DayFacts{}, UserFacts{ActiveBucketInstants: instants(c.instantN)}, ctx)
			if c.wantActive && !hitOK(out, "R1", "user") {
				t.Fatalf("%s: R1 活跃%dh 应命中", c.name, c.instantN)
			}
			// R3b 分母按实际 elapsed（DST 特殊日）。
			occ := int64(c.elapsed * 3600 * 1000 * 0.5)
			out = EvaluateRules(p, DayFacts{OccupiedMs: occ}, UserFacts{}, RuleContext{ElapsedHours: c.elapsed})
			if !hitOK(out, "R3b", "group") {
				t.Fatalf("%s: R3b 分母=%.1fh 应命中", c.name, c.elapsed)
			}
		})
	}
}

// TestGroupApplicableLimit 直接断言 checkRPM 三层限额选择语义（override 三态）。
func TestGroupApplicableLimit(t *testing.T) {
	// override>0 替代分组限。
	if g := groupApplicableLimit(ptrInt(10), 100); g != 10 {
		t.Fatalf("override>0 应返回 override=10, got %d", g)
	}
	// override=0 免除分组检查 → 0（无分组限额）。
	if g := groupApplicableLimit(ptrInt(0), 100); g != 0 {
		t.Fatalf("override=0 应返回 0(免除), got %d", g)
	}
	// nil override 且 groupLimit>0 → 分组限。
	if g := groupApplicableLimit(nil, 100); g != 100 {
		t.Fatalf("nil override 应返回 groupLimit=100, got %d", g)
	}
	// nil override 且 groupLimit=0 → 0（无分组限额）。
	if g := groupApplicableLimit(nil, 0); g != 0 {
		t.Fatalf("groupLimit=0 应返回 0, got %d", g)
	}
}

// TestEvaluateRulesR3aOverride 覆盖 R3a 双作用域独立判定与 override 三态。
func TestEvaluateRulesR3aOverride(t *testing.T) {
	p := defaultPolicy()

	// 状态 A：override=10（替代分组限，使分组作用域合格），用户全局未达标。
	// group 命中分钟=30（≥R3AMinutes），global=5（<30）。
	ctxA := RuleContext{UserRPMLimit: 200, GroupRPMLimit: 100, Override: ptrInt(10)}
	out := EvaluateRules(p, DayFacts{RPMLineMinutesGroup: 30}, UserFacts{GlobalRPMHitMinutes: 5}, ctxA)
	if _, ok := hasHit(out, "R3a", "group"); !ok {
		t.Fatalf("A: R3a group 应命中")
	}
	if _, ok := hasHit(out, "R3a", "user"); ok {
		t.Fatalf("A: R3a user 不应命中")
	}

	// 状态 B：override=0（免除分组检查）→ 分组作用域不合格；用户全局达标。
	ctxB := RuleContext{UserRPMLimit: 50, GroupRPMLimit: 100, Override: ptrInt(0)}
	out = EvaluateRules(p, DayFacts{RPMLineMinutesGroup: 30}, UserFacts{GlobalRPMHitMinutes: 30}, ctxB)
	if _, ok := hasHit(out, "R3a", "group"); ok {
		t.Fatalf("B: R3a group 不应命中（override=0 免除）")
	}
	if _, ok := hasHit(out, "R3a", "user"); !ok {
		t.Fatalf("B: R3a user 应命中")
	}

	// 状态 C：override=nil，分组限与用户限均合格 → 双作用域均命中（各自独立计分）。
	ctxC := RuleContext{UserRPMLimit: 50, GroupRPMLimit: 100, Override: nil}
	out = EvaluateRules(p, DayFacts{RPMLineMinutesGroup: 30}, UserFacts{GlobalRPMHitMinutes: 30}, ctxC)
	if _, ok := hasHit(out, "R3a", "group"); !ok {
		t.Fatalf("C: R3a group 应命中")
	}
	if _, ok := hasHit(out, "R3a", "user"); !ok {
		t.Fatalf("C: R3a user 应命中")
	}
	if ruleScore(out, "R3a") != 30 {
		t.Fatalf("C: R3a 双作用域应计 30 分, got %d", ruleScore(out, "R3a"))
	}

	// 天花板语义：用户限=0（无全局上限）→ 用户全局作用域不合格。
	ctxD := RuleContext{UserRPMLimit: 0, GroupRPMLimit: 100, Override: nil}
	out = EvaluateRules(p, DayFacts{RPMLineMinutesGroup: 30}, UserFacts{GlobalRPMHitMinutes: 30}, ctxD)
	if _, ok := hasHit(out, "R3a", "user"); ok {
		t.Fatalf("D: 用户限=0 时 user 作用域不应命中")
	}
	if _, ok := hasHit(out, "R3a", "group"); !ok {
		t.Fatalf("D: group 作用域仍应命中")
	}
}

// TestUsageRiskRulesDisabled 验证规则开关关闭零影响。
func TestUsageRiskRulesDisabled(t *testing.T) {
	p := defaultPolicy()
	p.R1Enabled = false

	// R1 关闭：即便事实达标也不命中、不记 insufficient_history、不影响其他规则。
	uf := UserFacts{ActiveBucketInstants: instants(24), DistinctIPCount: 15}
	ctx := RuleContext{R1HistoryCovered: false, R1ConsecutiveDays: 3}
	out := EvaluateRules(p, DayFacts{}, uf, ctx)
	if hitOK(out, "R1", "user") {
		t.Fatalf("R1 关闭不应命中")
	}
	if out.InsufficientHistory {
		t.Fatalf("R1 关闭不应记 insufficient_history")
	}
	if _, ok := hasHit(out, "R4", "user"); !ok {
		t.Fatalf("R1 关闭不应影响 R4 命中")
	}
	if ruleScore(out, "R4") != 15 {
		t.Fatalf("R4 分数应独立为 15")
	}

	// 全部关闭：0 分、low、无 rule_hits。
	allOff := defaultPolicy()
	allOff.R1Enabled = false
	allOff.R2Enabled = false
	allOff.R3AEnabled = false
	allOff.R3BEnabled = false
	allOff.R4Enabled = false
	allOff.R5Enabled = false
	allOff.R6Enabled = false
	allOff.R7Enabled = false
	allOff.R8Enabled = false
	rich := UserFacts{
		ActiveBucketInstants: instants(24), DistinctIPCount: 15,
		IPClusters:    map[string]IPClusterFact{"1.1.1.1": {DistinctUserCount: 5, TotalRequests: 200}},
		UADistribution: map[string]int{"x": 10},
		KeyCounts:      map[string]int{"k": 10},
	}
	out = EvaluateRules(allOff, DayFacts{CostUSD: 999, OccupiedMs: 999, InputTokens: 9000000000}, rich,
		RuleContext{R1HistoryCovered: false, R2PeerCost: []float64{1}})
	if out.Score != 0 || out.Level != "low" || len(out.RuleHits) != 0 {
		t.Fatalf("全部关闭应为 0 分 low 无 hit, got score=%d hits=%d", out.Score, len(out.RuleHits))
	}
	if out.InsufficientHistory || out.InsufficientPeers {
		t.Fatalf("全部关闭不应记 insufficient 状态")
	}
}

// TestScoreLevel 验证分值映射边界。
func TestScoreLevel(t *testing.T) {
	cases := []struct {
		score int
		level string
	}{
		{100, "critical"}, {99, "high"}, {70, "high"}, {69, "medium"}, {40, "medium"}, {39, "low"}, {0, "low"},
	}
	for _, c := range cases {
		if got := scoreLevel(c.score); got != c.level {
			t.Fatalf("scoreLevel(%d)=%s, want %s", c.score, got, c.level)
		}
	}
}

// TestUsageRiskAssembleEvidence 验证 top-K 截断与 256KB truncated 标记。
func TestUsageRiskAssembleEvidence(t *testing.T) {
	p := defaultPolicy()

	// top-K 截断：IP 60 条 → 截断到 ≤50；UA 30 → ≤20；Key 30 → ≤20。
	clusters := map[string]IPClusterFact{}
	for i := 0; i < 60; i++ {
		clusters[string(rune('A'+i%26))+string(rune('0'+i/26))] = IPClusterFact{DistinctUserCount: 1, TotalRequests: i + 1}
	}
	ua := map[string]int{}
	for i := 0; i < 30; i++ {
		ua[string(rune('a'+i%26))+string(rune('0'+i/26))] = i + 1
	}
	keys := map[string]int{}
	for i := 0; i < 30; i++ {
		keys[string(rune('k'+i%26))+string(rune('0'+i/26))] = i + 1
	}
	uf := UserFacts{IPClusters: clusters, UADistribution: ua, KeyCounts: keys, ActiveHourHeatmap: map[int]int{0: 1, 1: 2}}
	ctx := RuleContext{R2PeerCost: []float64{1, 2, 3}, R2Filter: "f", R2Algorithm: "linear-interpolation-p95"}
	ev := AssembleEvidence(DayFacts{}, uf, ctx)
	if len(ev.IPs) > 50 {
		t.Fatalf("IP top-K 应 ≤50, got %d", len(ev.IPs))
	}
	if len(ev.UAs) > 20 {
		t.Fatalf("UA top-K 应 ≤20, got %d", len(ev.UAs))
	}
	if len(ev.Keys) > 20 {
		t.Fatalf("Key top-K 应 ≤20, got %d", len(ev.Keys))
	}
	raw, _ := json.Marshal(ev)
	if len(raw) > evidenceMaxBytes {
		t.Fatalf("普通 top-K 下 evidence 应 ≤256KB, got %d", len(raw))
	}
	if ev.Truncated {
		t.Fatalf("普通数据不应标记 truncated")
	}
	// 未超限路径：original_bytes 必须为零值（omitempty → 序列化后不含该键）。
	if ev.OriginalBytes != 0 {
		t.Fatalf("未超限不应设置 original_bytes, got %d", ev.OriginalBytes)
	}
	var untrunc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &untrunc); err != nil {
		t.Fatalf("unmarshal 失败: %v", err)
	}
	if _, ok := untrunc["original_bytes"]; ok {
		t.Fatalf("未超限路径序列化结果不得含 original_bytes 键")
	}

	// 256KB 超限：构造 60 个超长 IP 串（每个 ~6KB）→ 原始远超 256KB。
	bigClusters := map[string]IPClusterFact{}
	longIP := ""
	for i := 0; i < 6000; i++ {
		longIP += "x"
	}
	for i := 0; i < 60; i++ {
		bigClusters[longIP+string(rune('0'+i/10))+string(rune('0'+i%10))] = IPClusterFact{DistinctUserCount: 1, TotalRequests: 1}
	}
	uf2 := UserFacts{IPClusters: bigClusters}
	ev2 := AssembleEvidence(DayFacts{}, uf2, RuleContext{})
	if !ev2.Truncated {
		t.Fatalf("超大 evidence 应标记 truncated:true")
	}
	if ev2.OriginalBytes <= evidenceMaxBytes {
		t.Fatalf("OriginalBytes 应记录超限前原始字节(>256KB), got %d", ev2.OriginalBytes)
	}
	raw2, _ := json.Marshal(ev2)
	if len(raw2) > evidenceMaxBytes {
		t.Fatalf("截断后 evidence 应 ≤256KB, got %d", len(raw2))
	}
	_ = p
}

// jsonKeys 从任意 JSON 值（对象或数组）提取顶层键集（对象）/ 首元素键集（数组）。
func jsonKeys(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err == nil {
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys
	}
	var arr []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		keys := make([]string, 0, len(arr))
		for k := range arr[0] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys
	}
	t.Fatalf("jsonKeys: 无法解析为对象或数组: %s", string(raw))
	return nil
}

// TestUsageRiskAssembleEvidenceContractGolden 锁死前后端 evidence 契约（R5-1 K / F13）。
// 未超限路径顶层键集恰为 {active_hours, ip_top, ua_top, key_distribution, heatmap,
// r2_context, truncated}（original_bytes 为零值被 omitempty 省略）；子字段名逐一断言，
// 任何改名即破坏前端消费。
func TestUsageRiskAssembleEvidenceContractGolden(t *testing.T) {
	// 构造足以填满各数组与 r2_context 的事实。
	clusters := map[string]IPClusterFact{}
	for i := 0; i < 5; i++ {
		clusters[string(rune('A'+i))] = IPClusterFact{DistinctUserCount: i + 1, TotalRequests: (i + 1) * 10}
	}
	ua := map[string]int{}
	for i := 0; i < 3; i++ {
		ua[string(rune('a'+i))] = (i + 1) * 5
	}
	keys := map[string]int{}
	for i := 0; i < 3; i++ {
		keys[string(rune('k'+i))] = (i + 1) * 3
	}
	uf := UserFacts{
		ActiveBucketInstants: instants(7), // active_hours 应 =7
		IPClusters:           clusters,
		UADistribution:       ua,
		KeyCounts:            keys,
		ActiveHourHeatmap:    map[int]int{0: 1, 1: 2, 2: 3},
	}
	ctx := RuleContext{R2PeerCost: []float64{1, 2, 3}, R2Filter: "f", R2Algorithm: "linear-interpolation-p95"}
	ev := AssembleEvidence(DayFacts{}, uf, ctx)

	if ev.ActiveHours != 7 {
		t.Fatalf("F13: ActiveHours 应=7(跨分组 distinct 桶数), got %d", ev.ActiveHours)
	}

	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal 失败: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("unmarshal 顶层失败: %v", err)
	}

	// 顶层键集断言（排序后精确比较）。本用例为未超限路径，original_bytes 为零值被 omitempty
	// 省略，故期望集不含该键（超限路径的键集由 TestAssembleEvidenceNearLimit 单独锁死）。
	gotKeys := make([]string, 0, len(top))
	for k := range top {
		gotKeys = append(gotKeys, k)
	}
	sort.Strings(gotKeys)
	wantKeys := []string{
		"active_hours", "ip_top", "ua_top", "key_distribution",
		"heatmap", "r2_context", "truncated",
	}
	sort.Strings(wantKeys)
	if strings.Join(gotKeys, ",") != strings.Join(wantKeys, ",") {
		t.Fatalf("F13: 顶层键集不符\n got=%v\nwant=%v", gotKeys, wantKeys)
	}

	// 子字段名逐一断言。
	if got := jsonKeys(t, top["ip_top"]); strings.Join(got, ",") != "distinct_users,ip,requests" {
		t.Fatalf("F13: ip_top 元素键应=distinct_users,ip,requests, got=%v", got)
	}
	if got := jsonKeys(t, top["ua_top"]); strings.Join(got, ",") != "count,ua" {
		t.Fatalf("F13: ua_top 元素键应=count,ua, got=%v", got)
	}
	if got := jsonKeys(t, top["key_distribution"]); strings.Join(got, ",") != "count,key" {
		t.Fatalf("F13: key_distribution 元素键应=count,key, got=%v", got)
	}
	if got := jsonKeys(t, top["r2_context"]); strings.Join(got, ",") != "algorithm,filter,p95,peer_count" {
		t.Fatalf("F13: r2_context 键应=algorithm,filter,p95,peer_count, got=%v", got)
	}

	// heatmap 必须是对象且为整型值（验证 map[string]int 形态）。
	var heatmap map[string]int
	if err := json.Unmarshal(top["heatmap"], &heatmap); err != nil {
		t.Fatalf("F13: heatmap 应可解析为 map[string]int: %v", err)
	}
	if len(heatmap) != 3 {
		t.Fatalf("F13: heatmap 应含 3 个桶, got %d", len(heatmap))
	}

	// active_hours 为整型且值为 7。
	var ah int
	if err := json.Unmarshal(top["active_hours"], &ah); err != nil || ah != 7 {
		t.Fatalf("F13: active_hours 应=7, got=%d err=%v", ah, err)
	}

	// 顶层不得再出现旧字段名（锁死改名）。
	for _, old := range []string{"ips", "uas", "keys", "hour_heatmap"} {
		if _, ok := top[old]; ok {
			t.Fatalf("F13: 契约不得残留旧字段名 %q", old)
		}
	}
}

// TestUsageRiskR5Deterministic 锁定 R5 detail 的 ip 选择确定性（外审⑧ #4）：
// map 遍历序不确定，多 IP 同时达标时 detail 必须恒为 (TotalRequests 降序, IP 字典序升序)
// 排序首项；命中判定（任一达标即命中）与分数不变。
func TestUsageRiskR5Deterministic(t *testing.T) {
	p := defaultPolicy()

	// 多 IP 同时达标、requests 不同：恒选 TotalRequests 最大者。
	uf := UserFacts{IPClusters: map[string]IPClusterFact{
		"10.0.0.1": {DistinctUserCount: 5, TotalRequests: 150},
		"10.0.0.2": {DistinctUserCount: 4, TotalRequests: 200},
		"10.0.0.3": {DistinctUserCount: 6, TotalRequests: 120},
	}}
	for i := 0; i < 100; i++ {
		out := EvaluateRules(p, DayFacts{}, uf, RuleContext{})
		h, ok := hasHit(out, "R5", "user")
		if !ok {
			t.Fatalf("迭代 %d: R5 应命中", i)
		}
		if h.Points != 25 {
			t.Fatalf("迭代 %d: R5 分数应为 25, got %d", i, h.Points)
		}
		if !strings.Contains(h.Detail, "ip=10.0.0.2") {
			t.Fatalf("迭代 %d: detail 应选 TotalRequests 最大 IP 10.0.0.2, got %q", i, h.Detail)
		}
	}

	// requests 相同：恒选字典序首项。
	uf2 := UserFacts{IPClusters: map[string]IPClusterFact{
		"10.0.0.9": {DistinctUserCount: 4, TotalRequests: 150},
		"10.0.0.1": {DistinctUserCount: 4, TotalRequests: 150},
		"10.0.0.5": {DistinctUserCount: 4, TotalRequests: 150},
	}}
	for i := 0; i < 100; i++ {
		out := EvaluateRules(p, DayFacts{}, uf2, RuleContext{})
		h, ok := hasHit(out, "R5", "user")
		if !ok {
			t.Fatalf("迭代 %d: R5 应命中", i)
		}
		if !strings.Contains(h.Detail, "ip=10.0.0.1") {
			t.Fatalf("迭代 %d: requests 相同应取字典序首项 10.0.0.1, got %q", i, h.Detail)
		}
	}

	// 恰一个达标 IP（其余不达标）：detail 仍为该达标 IP，与排序无关。
	uf3 := UserFacts{IPClusters: map[string]IPClusterFact{
		"10.0.0.1": {DistinctUserCount: 2, TotalRequests: 999}, // distinct_users<3 不达标
		"10.0.0.2": {DistinctUserCount: 5, TotalRequests: 300}, // 达标
		"10.0.0.3": {DistinctUserCount: 5, TotalRequests: 99},  // requests<100 不达标
	}}
	out := EvaluateRules(p, DayFacts{}, uf3, RuleContext{})
	h, ok := hasHit(out, "R5", "user")
	if !ok {
		t.Fatalf("R5 应命中")
	}
	if !strings.Contains(h.Detail, "ip=10.0.0.2") {
		t.Fatalf("唯一达标 IP 应被选中, got %q", h.Detail)
	}
}

// TestUsageRiskAssembleEvidenceTruncatedContract 锁死超限路径契约（外审⑧ #6）：
// 超限时必须带 truncated=true + original_bytes，且裁剪（含全部最终字段）后最终
// Marshal(ev) 必须 ≤ evidenceMaxBytes；顶层键集恰含 original_bytes。
func TestUsageRiskAssembleEvidenceTruncatedContract(t *testing.T) {
	// 每个 ~6KB、共 60 个 IP → 原始远超 256KB。
	bigClusters := map[string]IPClusterFact{}
	longIP := ""
	for i := 0; i < 6000; i++ {
		longIP += "x"
	}
	for i := 0; i < 60; i++ {
		ip := longIP + string(rune('0'+i/10)) + string(rune('0'+i%10))
		bigClusters[ip] = IPClusterFact{DistinctUserCount: 1, TotalRequests: 1}
	}
	ev := AssembleEvidence(DayFacts{}, UserFacts{IPClusters: bigClusters}, RuleContext{})
	if !ev.Truncated {
		t.Fatalf("超高 evidence 应标记 truncated")
	}
	if ev.OriginalBytes <= evidenceMaxBytes {
		t.Fatalf("OriginalBytes 应记录超限前原始字节(>256KB), got %d", ev.OriginalBytes)
	}
	finalRaw, _ := json.Marshal(ev)
	if len(finalRaw) > evidenceMaxBytes {
		t.Fatalf("裁剪后证据（含 original_bytes/truncated）应 ≤256KB, got %d", len(finalRaw))
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(finalRaw, &top); err != nil {
		t.Fatalf("unmarshal 顶层失败: %v", err)
	}
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{
		"active_hours", "ip_top", "ua_top", "key_distribution",
		"heatmap", "r2_context", "truncated", "original_bytes",
	}
	sort.Strings(want)
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("超限路径顶层键集不符\n got=%v\nwant=%v", keys, want)
	}
}

// TestUsageRiskAssembleEvidenceNearLimit 覆盖上限边界（外审⑧ #6 核心场景）：
// raw（不含 original_bytes）恰在 [evidenceMaxBytes-25, evidenceMaxBytes] 区间时，
// 未超限则不得设置 original_bytes，最终落库 Marshal(ev) 必须 ≤ evidenceMaxBytes
// （旧实现无条件置 original_bytes 使该区间落库超硬上限）。
func TestUsageRiskAssembleEvidenceNearLimit(t *testing.T) {
	// 构造：19 个固定小 UA + 1 个可变长 UA 键（步进 1），使首序列化长度单调可微调。
	baseUAs := func(uaLen int) map[string]int {
		m := make(map[string]int, 20)
		for i := 0; i < 19; i++ {
			m[fmt.Sprintf("fill%d", i)] = 1
		}
		m[strings.Repeat("a", uaLen)] = 1
		return m
	}
	// 模拟内部首序列化（不含 original_bytes/truncated 渲染差异，truncated=false 恒定出现）。
	marshalBase := func(uas map[string]int) int {
		probe := UsageRiskEvidence{
			IPs:         []IPEvidence{},
			UAs:         buildUAEvidence(uas, 20),
			Keys:        []KeyEvidence{},
			HourHeatmap: map[string]int{},
			R2Context:   R2EvidenceContext{},
		}
		raw, _ := json.Marshal(probe)
		return len(raw)
	}
	// 二分定位使 len 恰落在 [evidenceMaxBytes-25, evidenceMaxBytes] 的 uaLen（单调增）。
	ipLen := 0
	found := false
	low, high := 1, evidenceMaxBytes*2
	for low <= high {
		mid := (low + high) / 2
		n := marshalBase(baseUAs(mid))
		if n >= evidenceMaxBytes-25 && n <= evidenceMaxBytes {
			ipLen = mid
			found = true
			break
		}
		if n < evidenceMaxBytes-25 {
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	if !found {
		t.Skipf("未能构造 [上限-25, 上限] 区间输入（内部布局可能变动），跳过边界精测")
	}

	// 精测：该区间输入调用真实 AssembleEvidence —— 未超限路径。
	ev := AssembleEvidence(DayFacts{}, UserFacts{UADistribution: baseUAs(ipLen)}, RuleContext{})
	if ev.Truncated {
		t.Fatalf("raw 在 [上限-25, 上限] 内不应 truncated（未确认超上限）")
	}
	if ev.OriginalBytes != 0 {
		t.Fatalf("未超限路径不得设置 original_bytes, got %d", ev.OriginalBytes)
	}
	finalRaw, _ := json.Marshal(ev)
	if len(finalRaw) > evidenceMaxBytes {
		t.Fatalf("边界非超限路径最终落库应 ≤256KB, got %d", len(finalRaw))
	}
	if baseRaw := marshalBase(baseUAs(ipLen)); baseRaw > evidenceMaxBytes {
		t.Fatalf("测试前提：baseRaw 应 ≤ 上限, got %d", baseRaw)
	}
}
