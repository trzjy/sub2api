//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// 排序调用点盘点（gateway_scheduling.go，接入前登记，供 B4-D2 报告；以实际代码为准逐行核）：
//
//	:460  routingAvailable 内联 sort.SliceStable（Layer 1 模型路由候选，Priority>LoadRate>LastUsedAt）
//	:479  shuffleWithinSortGroups(routingAvailable)  ← 上述排序后组内打乱
//	:741  filterByMinPriority（Layer 2 分层过滤第一步：取最小优先级集合）
//	:744  filterBySoonestReset（配置 PreferSoonestReset 时）
//	:747  filterByMinLoadRate
//	:749  selectByLRU
//	:780  s.sortCandidatesForFallback（Layer 3 兜底排队）
//	:798  sortAccountsByPriorityAndLastUsed(ordered)（tryAcquireByLegacyOrder：GetAccountsLoadBatch 失败回退）
//	:1721 sortAccountsByPriorityAndLastUsed 定义（排序键 Priority>LastUsedAt；内部 :1741 shuffleWithinPriorityAndLastUsed）
//	:1746 shuffleWithinSortGroups 定义（按 Priority/LoadRate/LastUsedAt 分组组内打乱）
//	:1782 shuffleWithinPriorityAndLastUsed 定义（按 Priority/LastUsedAt 分组组内打乱）
//	:1843 sortCandidatesForFallback 定义（mode=random → :1846 sortAccountsByPriorityOnly + :1847 shuffleWithinPriority；否则 :1850 sortAccountsByPriorityAndLastUsed）
//
// 结论：该文件全部排序/打乱调用点均位于 GatewayService（Anthropic/Gemini/Antigravity 网关）
// 选号链路内；CodeBuddy 分组请求经 routes/gateway.go 的分流（isOpenAICompatibleGatewayPlatform）
// 全部走 OpenAIGatewayService.selectByLoadBalance，本文件的排序点对 CodeBuddy 候选不可达
// ——接入点在 B4-D2 执行中被 BLOCKED 顶回（证据见卡内报告，本文件未做任何源文件改动）。

// 本文件覆盖 §4.2 到期紧迫度纯函数全场景（Card D）：
//   - 参与性谓词 IsCodeBuddyUrgencyEligible：Status=3 剔除、168h 窗口内外、快照 >24h 陈旧、UpdatedAt 零值
//   - ComputeCodeBuddyUrgency：D=0→Norm=0、Norm 截断 [0,1]、分子分母同一集合
//   - ApplyCodeBuddyUrgencyBoost：k=0 与未启用恒等
//   - CodeBuddyUrgencySnapshotFromExtra：形态错误失败关闭
//   - 调度接入（buildOpenAIAccountLoadPlan）：codebuddy 平台门禁、k=0 逐位一致、
//     k>0 高紧迫候选 score 更高、非 codebuddy 平台不受影响、快照陈旧/缺失乘数=1
//
// 说明：sortCandidatesByUrgencyBoost / sortCandidatesByUrgencyBoostPtr（及配套
// 用例 makeAccountWithLoad/makeSnapshotMap）随 D-3 死 helper 清理一并移除，
// 其旧「排序链」接入点已被 buildOpenAIAccountLoadPlan 的 score 乘法路径取代。

func urgencyNow() time.Time {
	return time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
}

// urgencyPackage 构造一个有效参与分包。expiresAtOffset 以固定基准 urgencyNow 为基准。
func urgencyPackage(t *testing.T, id string, remaining, total string, status int64, expiresAtOffset time.Duration) CodeBuddyCreditPackage {
	t.Helper()
	return urgencyPackageAt(t, urgencyNow(), id, remaining, total, status, expiresAtOffset)
}

// urgencyPackageAt 以显式基准时间为基准构造分包（供调度接入测试按真实 time.Now 锚定）。
func urgencyPackageAt(t *testing.T, base time.Time, id string, remaining, total string, status int64, expiresAtOffset time.Duration) CodeBuddyCreditPackage {
	t.Helper()
	return CodeBuddyCreditPackage{
		ID:        id,
		Name:      "pkg-" + id,
		Unit:      codebuddyCreditCanonicalUnit,
		Remaining: mustDecimal(t, remaining),
		Total:     mustDecimal(t, total),
		ExpiresAt: base.Add(expiresAtOffset).UTC().Format(time.RFC3339),
		Status:    status,
	}
}

func freshSnapshot(packages ...CodeBuddyCreditPackage) codeBuddyUrgencySnapshot {
	return codeBuddyUrgencySnapshot{
		Packages:  packages,
		UpdatedAt: urgencyNow().Add(-1 * time.Hour),
	}
}

// --- TestIsCodeBuddyUrgencyEligible ---

func TestIsCodeBuddyUrgencyEligible_ExcludesExhaustedTotal(t *testing.T) {
	now := urgencyNow()
	// Status=3（耗尽/过期）分包：基础谓词已剔除；这里叠加断言"耗尽包 total 不进 D"的前提
	// = 该分包根本不参与分子也不参与分母。
	exhausted := urgencyPackage(t, "exhausted", "1", "100", codebuddyCreditPackageStatusExhausted, 24*time.Hour)
	require.False(t, IsCodeBuddyUrgencyEligible(exhausted, freshSnapshot().UpdatedAt, now))
}

func TestIsCodeBuddyUrgencyEligible_WithinWindow(t *testing.T) {
	now := urgencyNow()
	pkg := urgencyPackage(t, "in", "10", "20", codebuddyCreditPackageStatusActive, 24*time.Hour)
	require.True(t, IsCodeBuddyUrgencyEligible(pkg, freshSnapshot().UpdatedAt, now))
}

func TestIsCodeBuddyUrgencyEligible_ExpiresAtBoundary(t *testing.T) {
	now := urgencyNow()
	// 恰好 now+168h → 计入（窗口闭区间右端）。
	boundary := urgencyPackage(t, "boundary", "10", "20", codebuddyCreditPackageStatusActive, 168*time.Hour)
	require.True(t, IsCodeBuddyUrgencyEligible(boundary, freshSnapshot().UpdatedAt, now))
	// now+169h → 超过窗口，不计入。
	over := urgencyPackage(t, "over", "10", "20", codebuddyCreditPackageStatusActive, 169*time.Hour)
	require.False(t, IsCodeBuddyUrgencyEligible(over, freshSnapshot().UpdatedAt, now))
}

func TestIsCodeBuddyUrgencyEligible_ExpiredOrMissing(t *testing.T) {
	now := urgencyNow()
	// 已过期（expires_at == now）不计入。
	expired := urgencyPackage(t, "expired", "10", "20", codebuddyCreditPackageStatusActive, 0)
	require.False(t, IsCodeBuddyUrgencyEligible(expired, freshSnapshot().UpdatedAt, now))
	// 早于 now 同样不计入。
	past := urgencyPackage(t, "past", "10", "20", codebuddyCreditPackageStatusActive, -1*time.Hour)
	require.False(t, IsCodeBuddyUrgencyEligible(past, freshSnapshot().UpdatedAt, now))
	// expires_at 缺失/非法 → 不计入。
	bad := urgencyPackage(t, "bad", "10", "20", codebuddyCreditPackageStatusActive, 24*time.Hour)
	bad.ExpiresAt = "not-a-time"
	require.False(t, IsCodeBuddyUrgencyEligible(bad, freshSnapshot().UpdatedAt, now))
}

func TestIsCodeBuddyUrgencyEligible_StaleSnapshot(t *testing.T) {
	now := urgencyNow()
	pkg := urgencyPackage(t, "stale", "10", "20", codebuddyCreditPackageStatusActive, 24*time.Hour)
	// 快照 >24h 陈旧 → 不参与（分子分母均排除）。
	stale := now.Add(-24*time.Hour - time.Second)
	require.False(t, IsCodeBuddyUrgencyEligible(pkg, stale, now))
	// 恰好 24h → 满足新鲜度。
	exact := now.Add(-24 * time.Hour)
	require.True(t, IsCodeBuddyUrgencyEligible(pkg, exact, now))
}

func TestIsCodeBuddyUrgencyEligible_ZeroUpdatedAt(t *testing.T) {
	now := urgencyNow()
	pkg := urgencyPackage(t, "zero", "10", "20", codebuddyCreditPackageStatusActive, 24*time.Hour)
	require.False(t, IsCodeBuddyUrgencyEligible(pkg, time.Time{}, now))
}

func TestIsCodeBuddyUrgencyEligible_FutureSnapshotUpdatedAt(t *testing.T) {
	now := urgencyNow()
	pkg := urgencyPackage(t, "future", "10", "20", codebuddyCreditPackageStatusActive, 24*time.Hour)
	// 快照更新于未来（时钟异常）→ 不参与，防御性失败关闭。
	require.False(t, IsCodeBuddyUrgencyEligible(pkg, now.Add(time.Hour), now))
}

// --- TestComputeCodeBuddyUrgency ---

func TestComputeCodeBuddyUrgency_ZeroDZeroNorm(t *testing.T) {
	res := ComputeCodeBuddyUrgency(codeBuddyUrgencySnapshot{}, urgencyNow())
	require.True(t, res.Urgency.IsZero())
	require.True(t, res.D.IsZero())
	require.Equal(t, 0.0, res.Norm)
	require.Equal(t, 0, res.ParticipatingCount)
}

func TestComputeCodeBuddyUrgency_NormClamped(t *testing.T) {
	now := urgencyNow()
	// 两个参与分包：remaining/total 之和 15/50 → norm=0.3。
	snap := freshSnapshot(
		urgencyPackage(t, "a", "10", "20", codebuddyCreditPackageStatusActive, 24*time.Hour),
		urgencyPackage(t, "b", "5", "30", codebuddyCreditPackageStatusActive, 48*time.Hour),
	)
	res := ComputeCodeBuddyUrgency(snap, now)
	require.Equal(t, "15", res.Urgency.String())
	require.Equal(t, "50", res.D.String())
	require.InDelta(t, 0.3, res.Norm, 1e-9)
	require.Equal(t, 2, res.ParticipatingCount)
}

func TestComputeCodeBuddyUrgency_OnlyEligibleIncluded(t *testing.T) {
	now := urgencyNow()
	// 一个 7 天内到期（参与）、一个 7 天外（不参与）、一个 Status=3（不参与）。
	// 仅前者进入分子与分母。
	snap := freshSnapshot(
		urgencyPackage(t, "in", "10", "10", codebuddyCreditPackageStatusActive, 24*time.Hour),
		urgencyPackage(t, "far", "80", "90", codebuddyCreditPackageStatusActive, 300*time.Hour),
		urgencyPackage(t, "dead", "5", "5", codebuddyCreditPackageStatusExhausted, 24*time.Hour),
	)
	res := ComputeCodeBuddyUrgency(snap, now)
	require.Equal(t, "10", res.Urgency.String())
	require.Equal(t, "10", res.D.String())
	require.Equal(t, 0, res.ParticipatingCount-1) // 恰 1 个参与
	require.InDelta(t, 1.0, res.Norm, 1e-9)       // 10/10=1
}

// --- TestApplyCodeBuddyUrgencyBoost ---

func TestApplyCodeBuddyUrgencyBoost_DisabledOrKZeroIdentity(t *testing.T) {
	now := urgencyNow()
	acc := &Account{ID: 1, Extra: map[string]any{}}
	high := freshSnapshot(
		urgencyPackage(t, "a", "10", "10", codebuddyCreditPackageStatusActive, 24*time.Hour),
	)
	// 未启用 → 权重恒为 1。
	require.Equal(t, 1.0, ApplyCodeBuddyUrgencyBoost(acc, high, codeBuddyUrgencyBoost{Enabled: true, K: 0}, now))
	require.Equal(t, 1.0, ApplyCodeBuddyUrgencyBoost(acc, high, codeBuddyUrgencyBoost{Enabled: false, K: 0.8}, now))
	// nil 候选 → 恒等 1。
	require.Equal(t, 1.0, ApplyCodeBuddyUrgencyBoost(nil, high, codeBuddyUrgencyBoost{Enabled: true, K: 0.5}, now))
	// 无快照/无参与 → Norm=0 → 权重 1。
	require.Equal(t, 1.0, ApplyCodeBuddyUrgencyBoost(acc, codeBuddyUrgencySnapshot{}, codeBuddyUrgencyBoost{Enabled: true, K: 0.5}, now))
}

func TestApplyCodeBuddyUrgencyBoost_WeightFormula(t *testing.T) {
	now := urgencyNow()
	acc := &Account{ID: 1, Extra: map[string]any{}}
	// norm = 10/10 = 1 → weight = 1×(1 + 0.5×1) = 1.5。
	resSnap := freshSnapshot(
		urgencyPackage(t, "a", "10", "10", codebuddyCreditPackageStatusActive, 24*time.Hour),
	)
	require.InDelta(t, 1.5, ApplyCodeBuddyUrgencyBoost(acc, resSnap, codeBuddyUrgencyBoost{Enabled: true, K: 0.5}, now), 1e-9)
	// norm = 0.3 → weight = 1.15。
	partial := freshSnapshot(
		urgencyPackage(t, "a", "10", "20", codebuddyCreditPackageStatusActive, 24*time.Hour),
		urgencyPackage(t, "b", "5", "30", codebuddyCreditPackageStatusActive, 48*time.Hour),
	)
	require.InDelta(t, 1.15, ApplyCodeBuddyUrgencyBoost(acc, partial, codeBuddyUrgencyBoost{Enabled: true, K: 0.5}, now), 1e-9)
}

// --- TestCodeBuddyUrgencySnapshotFromExtra ---

func codeBuddyPackageExtraMap(t *testing.T, remaining, total string, status int64) map[string]any {
	t.Helper()
	return map[string]any{
		"id":       "p1",
		"name":     "pkg-p1",
		"unit":     "credits",
		"remaining": remaining,
		"total":     total,
		"expires_at": urgencyNow().Add(24 * time.Hour).UTC().Format(time.RFC3339),
		"status":    float64(status),
	}
}

func TestCodeBuddyUrgencySnapshotFromExtra_InvalidShapeFailClosed(t *testing.T) {
	now := urgencyNow()
	// packages 键形态错误（非数组）→ 空集合。
	badPkg := map[string]any{
		codebuddyCreditPackagesKey: "not-an-array",
		codebuddyCreditPackagesUpdatedAtKey: urgencyNow().Add(-1 * time.Hour).UTC().Format(time.RFC3339),
	}
	snap := CodeBuddyUrgencySnapshotFromExtra(badPkg, now)
	require.Empty(t, snap.Packages)
	require.False(t, snap.UpdatedAt.IsZero())
	require.Equal(t, 0.0, ComputeCodeBuddyUrgency(snap, now).Norm)

	// freshness 键非法时间 → zero（不参与）。
	badFresh := map[string]any{
		codebuddyCreditPackagesKey:            []any{},
		codebuddyCreditPackagesUpdatedAtKey: "not-a-time",
	}
	snap2 := CodeBuddyUrgencySnapshotFromExtra(badFresh, now)
	require.True(t, snap2.UpdatedAt.IsZero())
	// 空 Extra → 空集合 + zero 时间。
	require.Empty(t, CodeBuddyUrgencySnapshotFromExtra(nil, now).Packages)
}

func TestCodeBuddyUrgencySnapshotFromExtra_HappyPathAnySlice(t *testing.T) {
	now := urgencyNow()
	extra := map[string]any{
		codebuddyCreditPackagesKey: []any{
			codeBuddyPackageExtraMap(t, "10", "20", 0),
		},
		codebuddyCreditPackagesUpdatedAtKey: urgencyNow().Add(-1 * time.Hour).UTC().Format(time.RFC3339),
	}
	snap := CodeBuddyUrgencySnapshotFromExtra(extra, now)
	require.Len(t, snap.Packages, 1)
	require.Equal(t, "10", snap.Packages[0].Remaining.String())
	require.Equal(t, "20", snap.Packages[0].Total.String())
	require.False(t, snap.UpdatedAt.IsZero())
}

func TestCodeBuddyUrgencySnapshotFromExtra_DirectTypedSlice(t *testing.T) {
	now := urgencyNow()
	extra := map[string]any{
		codebuddyCreditPackagesKey: []CodeBuddyCreditPackage{
			urgencyPackage(t, "typed", "10", "20", codebuddyCreditPackageStatusActive, 24*time.Hour),
		},
		codebuddyCreditPackagesUpdatedAtKey: urgencyNow().Add(-1 * time.Hour).UTC().Format(time.RFC3339),
	}
	snap := CodeBuddyUrgencySnapshotFromExtra(extra, now)
	require.Len(t, snap.Packages, 1)
	require.Equal(t, "typed", snap.Packages[0].ID)
}

// TestCodeBuddyUrgencySnapshotFromExtra_NonStringDecimalDropped 验证 Extra 反序列化路径
// 仅接受 string 形态的 remaining/total（与存储契约 MarshalJSON 对称）：float64 形态
// 的分包条目整条丢弃、不参与 urgency 权重。
func TestCodeBuddyUrgencySnapshotFromExtra_NonStringDecimalDropped(t *testing.T) {
	now := urgencyNow()
	// 单包数组：remaining/total 均 float64（float64 形态无精度保证，契约禁止）。
	floatPkg := map[string]any{
		"id":         "float-pkg",
		"name":       "pkg-float",
		"unit":       "credits",
		"remaining":  float64(10),
		"total":      float64(20),
		"expires_at": now.Add(1 * time.Hour).UTC().Format(time.RFC3339),
		"status":     float64(0),
	}
	// 同一数组混入合法 string 分包与非法 float64 分包 → 只有 string 分包保留。
	mixed := map[string]any{
		codebuddyCreditPackagesKey: []any{
			codeBuddyPackageExtraMap(t, "3", "5", 0), // string 形态合法
			floatPkg,                                  // float64 形态必须整条丢弃
		},
		codebuddyCreditPackagesUpdatedAtKey: now.Add(-1 * time.Hour).UTC().Format(time.RFC3339),
	}
	snap := CodeBuddyUrgencySnapshotFromExtra(mixed, now)
	require.Len(t, snap.Packages, 1, "float64 形态分包整条丢弃，仅 string 形态参与")
	require.Equal(t, "p1", snap.Packages[0].ID)
	require.Equal(t, "3", snap.Packages[0].Remaining.String())
	require.Equal(t, "5", snap.Packages[0].Total.String())

	// 全量 float64 → 无参与包 → norm=0（不参与紧迫度），且不 panic。
	onlyFloat := map[string]any{
		codebuddyCreditPackagesKey: []any{floatPkg},
		codebuddyCreditPackagesUpdatedAtKey: now.Add(-1 * time.Hour).UTC().Format(time.RFC3339),
	}
	snap2 := CodeBuddyUrgencySnapshotFromExtra(onlyFloat, now)
	require.Empty(t, snap2.Packages)
	require.Equal(t, 0.0, ComputeCodeBuddyUrgency(snap2, now).Norm)
}

// --- 调度接入测试（buildOpenAIAccountLoadPlan score 乘法路径） ---
//
// 策略：直接构造 defaultOpenAIAccountScheduler + OpenAIGatewayService{cfg}，
// 调用 buildOpenAIAccountLoadPlan 检查候选 score，与 openAIResetTestScheduler
// 等既有单测同构（不经过 repository / 迁移 / config.go，避免触碰禁区文件）。

// openAIUrgencyTestScheduler 构造启用 §4.2 加权（或关闭）的测试调度器。
// boostK<=0 且 enable=false 时等价未启用；enable=true 且 k>0 时进入乘法路径。
func openAIUrgencyTestScheduler(enable bool, k float64) *defaultOpenAIAccountScheduler {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights = config.GatewayOpenAIWSSchedulerScoreWeights{
		Priority: 1.0,
		Load:     1.0,
		Queue:    0.7,
		ErrorRate: 0.8,
		TTFT:     0.5,
	}
	cfg.Gateway.CodeBuddy.UrgencyBoostEnabled = enable
	cfg.Gateway.CodeBuddy.UrgencyBoostK = k
	return &defaultOpenAIAccountScheduler{service: &OpenAIGatewayService{cfg: cfg}}
}

// codebuddyUrgencyExtra 构造含 §4.2 输入快照的 Extra（participating = 近 168h 到期，
// 非参与 = 超过 168h 或 Status=3；freshness = 1h 前，满足 ≤24h）。
func codebuddyUrgencyExtra(t *testing.T, now time.Time) map[string]any {
	return map[string]any{
		codebuddyCreditPackagesKey: []CodeBuddyCreditPackage{
			urgencyPackageAt(t, now, "near", "10", "10", codebuddyCreditPackageStatusActive, 1*time.Hour),
			urgencyPackageAt(t, now, "far", "80", "90", codebuddyCreditPackageStatusActive, 300*time.Hour),
			urgencyPackageAt(t, now, "dead", "5", "5", codebuddyCreditPackageStatusExhausted, 1*time.Hour),
		},
		codebuddyCreditPackagesUpdatedAtKey: now.Add(-1 * time.Hour).UTC().Format(time.RFC3339),
	}
}

func TestBuildOpenAIAccountLoadPlan_UrgencyBoost_CodeBuddyPlatform_HighUrgencyScoresHigher(t *testing.T) {
	now := time.Now()
	filtered := []*Account{
		{ID: 1, Priority: 0, Extra: map[string]any{}}, // 无快照 → 乘数 1
		{ID: 2, Priority: 0, Extra: codebuddyUrgencyExtra(t, now)}, // 高紧迫 → norm=1 → 乘 1+k
	}
	req := OpenAIAccountScheduleRequest{Platform: PlatformCodeBuddy}
	sched := openAIUrgencyTestScheduler(true, 0.5)

	plan := sched.buildOpenAIAccountLoadPlan(context.Background(), req, filtered, map[int64]*AccountLoadInfo{})
	scores := openAIPlanScores(plan)
	require.Greater(t, scores[2], scores[1], "codebuddy 平台 + k>0：高紧迫候选 score 应严格更高")
}

func TestBuildOpenAIAccountLoadPlan_UrgencyBoost_KZeroScoreIdentical(t *testing.T) {
	now := time.Now()
	filtered := []*Account{
		{ID: 1, Priority: 0, Extra: codebuddyUrgencyExtra(t, now)},
		{ID: 2, Priority: 0, Extra: map[string]any{}},
	}
	req := OpenAIAccountScheduleRequest{Platform: PlatformCodeBuddy}
	// k=0（启用但系数 0）→ 不进乘法路径，score 与未启用逐位一致。
	schedZero := openAIUrgencyTestScheduler(true, 0.0)
	planZero := schedZero.buildOpenAIAccountLoadPlan(context.Background(), req, filtered, map[int64]*AccountLoadInfo{})
	scoresZero := openAIPlanScores(planZero)

	schedOff := openAIUrgencyTestScheduler(false, 0.5)
	planOff := schedOff.buildOpenAIAccountLoadPlan(context.Background(), req, filtered, map[int64]*AccountLoadInfo{})
	scoresOff := openAIPlanScores(planOff)

	require.Equal(t, scoresOff[1], scoresZero[1], "k=0 与未启用：ID=1 score 逐位一致")
	require.Equal(t, scoresOff[2], scoresZero[2], "k=0 与未启用：ID=2 score 逐位一致")
}

func TestBuildOpenAIAccountLoadPlan_UrgencyBoost_NonCodeBuddyPlatformUnaffected(t *testing.T) {
	now := time.Now()
	filtered := []*Account{
		{ID: 1, Priority: 0, Extra: codebuddyUrgencyExtra(t, now)},
		{ID: 2, Priority: 0, Extra: map[string]any{}},
	}
	req := OpenAIAccountScheduleRequest{Platform: PlatformOpenAI}
	sched := openAIUrgencyTestScheduler(true, 0.5)

	plan := sched.buildOpenAIAccountLoadPlan(context.Background(), req, filtered, map[int64]*AccountLoadInfo{})
	scores := openAIPlanScores(plan)
	// 非 codebuddy 平台：即使启用+k>0，score 也不受紧迫度影响（两账号同一基础分）。
	require.Equal(t, scores[1], scores[2], "非 codebuddy 平台不受紧迫度加权影响")
}

func TestBuildOpenAIAccountLoadPlan_UrgencyBoost_StaleOrMissingSnapshotMultiplierOne(t *testing.T) {
	now := time.Now()
	staleExtra := map[string]any{
		codebuddyCreditPackagesKey: []CodeBuddyCreditPackage{
			urgencyPackage(t, "near", "10", "10", codebuddyCreditPackageStatusActive, 1*time.Hour),
		},
		codebuddyCreditPackagesUpdatedAtKey: now.Add(-48 * time.Hour).UTC().Format(time.RFC3339), // >24h 陈旧
	}
	filtered := []*Account{
		{ID: 1, Priority: 0, Extra: staleExtra},
		{ID: 2, Priority: 0, Extra: map[string]any{}},
	}
	req := OpenAIAccountScheduleRequest{Platform: PlatformCodeBuddy}
	sched := openAIUrgencyTestScheduler(true, 0.5)

	plan := sched.buildOpenAIAccountLoadPlan(context.Background(), req, filtered, map[int64]*AccountLoadInfo{})
	scores := openAIPlanScores(plan)
	require.Equal(t, scores[1], scores[2], "快照陈旧/缺失 → 乘数=1，score 不受影响")
}