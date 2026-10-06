//go:build unit

// D2 定向测试：TokenHarbor 主动复探链核心（全站唯一探测链）。
// 覆盖派发单 Done when 的预算 / 冻结上界 / 写入口 / 恢复迁移 / 冷却 各条，
// 以及第 0 步核证②④的只读查证（见 D2-evidence.md）。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ===================== 内存 fake：探测链 SSOT 写入口仓储面 =====================

type d2ObsMeta struct {
	lastEventAt time.Time
	revision    int64
}

// d2ProbeObsRepo 是 modelRateLimitObservationRepository 的内存实现，供写入口单测，
// 不依赖真实 Postgres。嵌入 mockAccountRepoForGemini 以满足 AccountRepository 接口。
type d2ProbeObsRepo struct {
	mockAccountRepoForGemini
	mu               sync.Mutex
	entries          map[string]map[string]any
	meta             map[string]d2ObsMeta
	listTokenHarbor  []*Account
	listTempUnsched  []*Account
	// commitErr 非 nil 时，CommitModelRateLimitObservation 返回该错误且不改变任何状态
	// （E1 原子性定向测试：模拟「第二次写失败」场景）。
	commitErr error
	// commitScopeCount 记录 CommitModelRateLimitObservation 按 scope 的写入次数
	//（E27 #1 定向测试：断言账号级候选在一次 RunOnce 内恰好写入 1 次观测）。
	commitScopeCount map[string]int
	// syncSnapshotCalls 记录 SyncSchedulerAccountSnapshot（恢复提交后快照同步）被调用次数
	//（E30 #1 定向测试：断言恢复提交成功后补做快照同步）。
	syncSnapshotCalls int
	// syncSnapshotErr 非 nil 时，SyncSchedulerAccountSnapshot 返回该错误（E30 #1 定向测试：
	// 同步失败必须沿写入口返回错误，可观测不静默）。
	syncSnapshotErr error
	// clearTempUnschedCalls 记录 ClearTempUnschedulable 被调用次数
	//（E27 #2 定向测试：断言提交失败时账号仍 parked，恢复写入未被调用）。
	clearTempUnschedCalls int
	// entryErr 非 nil 时，GetModelRateLimitEntry 返回该错误（E8 #2 读失败停止读改写定向
	// 测试：读失败须沿唯一写入口 return，既不能当空条目继续提交，也不能推进 meta）。
	entryErr error
	// initialSetCalls 记录 SetModelRateLimitWithPreciseReset（E8 #4 初始 SET）的入参，
	// 供定向测试断言初始 SET 参与事件裁决（旧 SET no-op / 与新探测不互覆）。
	initialSetCalls []struct {
		id           int64
		scope        string
		resetAt      time.Time
		preciseReset bool
		reason       string
	}
}

func newD2ProbeObsRepo() *d2ProbeObsRepo {
	return &d2ProbeObsRepo{
		// accountsByID 由嵌入的 mockAccountRepoForGemini 提供 GetByID；
		// 必须初始化否则测试中 repo.accountsByID[id] = ... 会写入 nil map panic。
		mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: make(map[int64]*Account)},
		entries:                  make(map[string]map[string]any),
		meta:                     make(map[string]d2ObsMeta),
		commitScopeCount:         make(map[string]int),
	}
}

// ListTokenHarborModelRateLimitedAccounts 覆盖嵌入实现，返回测试注入的候选账号。
func (r *d2ProbeObsRepo) ListTokenHarborModelRateLimitedAccounts(_ context.Context, _ time.Time, _ int) ([]*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.listTokenHarbor, nil
}

// ListTempUnschedulableAccounts 覆盖嵌入实现，返回测试注入的账号级（熔断）候选。
func (r *d2ProbeObsRepo) ListTempUnschedulableAccounts(_ context.Context, _ time.Time, _ int) ([]*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.listTempUnsched, nil
}

func d2Key(id int64, scope string) string { return fmt.Sprintf("%d\x00%s", id, scope) }

func (r *d2ProbeObsRepo) GetModelRateLimitEntry(_ context.Context, id int64, scope string) (map[string]any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entryErr != nil {
		return nil, r.entryErr
	}
	e, ok := r.entries[d2Key(id, scope)]
	if !ok {
		return nil, nil
	}
	out := make(map[string]any, len(e))
	for k, v := range e {
		out[k] = v
	}
	return out, nil
}

// CommitModelRateLimitObservation 在同一“写入”内提交状态条目（clear=true 清除，否则
// 写 entry）与 meta（E1 原子性测试面）。commitErr 非 nil 时整体失败、两者均不变。
func (r *d2ProbeObsRepo) CommitModelRateLimitObservation(_ context.Context, id int64, scope string, entry map[string]any, clear bool, lastEventAt time.Time, revision int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.commitErr != nil {
		return r.commitErr
	}
	key := d2Key(id, scope)
	if clear {
		delete(r.entries, key)
	} else {
		cp := make(map[string]any, len(entry))
		for k, v := range entry {
			cp[k] = v
		}
		r.entries[key] = cp
	}
	r.meta[key] = d2ObsMeta{lastEventAt: lastEventAt, revision: revision}
	// 函数已持 r.mu（顶部 Lock + defer Unlock），此处直接递增，避免重复加锁自死锁。
	r.commitScopeCount[scope]++
	return nil
}

// WithModelRateLimitAccountLock 实现 modelRateLimitObservationRepository 的写锁窄面（E38 下沉）：
// 测试替身直接执行 fn（E30/E33 惯例），不再重复加锁——否则会与 CommitModelRateLimitObservation
// 内部的 r.mu 自死锁。真实仓库由 accountRepository 持锁，保证并发串行化。
func (r *d2ProbeObsRepo) WithModelRateLimitAccountLock(ctx context.Context, _ int64, fn func(ctx context.Context) error) error {
	return fn(ctx)
}

// SetModelRateLimitWithPreciseReset 模拟真实仓库的初始 SET（E8 #4）：读 meta → 按
// (事件时间, tie_breaker) 裁决（严格更旧 no-op）→ 单语句原子写条目+meta。与真实
// accountRepository 同语义，供 service 层「初始 SET 与并发探测不互覆」定向测试使用。
func (r *d2ProbeObsRepo) SetModelRateLimitWithPreciseReset(_ context.Context, id int64, scope string, resetAt time.Time, preciseReset bool, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.initialSetCalls = append(r.initialSetCalls, struct {
		id           int64
		scope        string
		resetAt      time.Time
		preciseReset bool
		reason       string
	}{id: id, scope: scope, resetAt: resetAt, preciseReset: preciseReset, reason: reason})
	key := d2Key(id, scope)
	now := time.Now()
	if m, ok := r.meta[key]; ok && now.Before(m.lastEventAt) {
		return nil // 旧 SET 整体 no-op，不覆盖较新的探测/业务状态
	}
	entry := map[string]any{
		"rate_limited_at":     now.UTC().Format(time.RFC3339),
		"rate_limit_reset_at": resetAt.UTC().Format(time.RFC3339),
		"precise_reset":       preciseReset,
	}
	if reason != "" {
		entry["reason"] = reason
	}
	cp := make(map[string]any, len(entry))
	for k, v := range entry {
		cp[k] = v
	}
	r.entries[key] = cp
	rev := int64(0)
	if m, ok := r.meta[key]; ok {
		rev = m.revision
	}
	r.meta[key] = d2ObsMeta{lastEventAt: now, revision: rev + 1}
	return nil
}

func (r *d2ProbeObsRepo) GetModelRateLimitMeta(_ context.Context, id int64, scope string) (time.Time, int64, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.meta[d2Key(id, scope)]
	if !ok {
		return time.Time{}, 0, false, nil
	}
	return m.lastEventAt, m.revision, true, nil
}

// WithObservationTx 是 observationTxRepository 窄面在内存替身上的实现：内存仓储无真实
// ent 事务，直接以 ctx 执行 fn（写与告警关闭同一次调用内完成，且任一返回错误整体不落地，
// 与 E1 原子性定向测试语义一致）。该实现仅存在于测试代码，不进生产路径。
func (r *d2ProbeObsRepo) WithObservationTx(ctx context.Context, fn func(txCtx context.Context) error) error {
	return fn(ctx)
}

// SyncSchedulerAccountSnapshot 是 schedulerSnapshotSyncRepository 窄面在内存替身上的实现
// （E30 #1：恢复提交后补做快照同步，事务内不预览未提交状态）。记录调用次数并支持注入错误。
func (r *d2ProbeObsRepo) SyncSchedulerAccountSnapshot(_ context.Context, _ int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.syncSnapshotCalls++
	if r.syncSnapshotErr != nil {
		return r.syncSnapshotErr
	}
	return nil
}

// ClearTempUnschedulable 覆盖嵌入实现，记录调用次数（E27 #2：断言提交失败时恢复写入未被触发）。
func (r *d2ProbeObsRepo) ClearTempUnschedulable(_ context.Context, _ int64) error {
	r.mu.Lock()
	r.clearTempUnschedCalls++
	r.mu.Unlock()
	return nil
}

func (r *d2ProbeObsRepo) metaFor(id int64, scope string) (d2ObsMeta, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.meta[d2Key(id, scope)]
	return m, ok
}

// presetEntry 直接预置存量条目（测试 setup 用，不经过 Commit，不受 commitErr 影响）。
func (r *d2ProbeObsRepo) presetEntry(id int64, scope string, entry map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make(map[string]any, len(entry))
	for k, v := range entry {
		cp[k] = v
	}
	r.entries[d2Key(id, scope)] = cp
}

// presetMeta 直接预置存量元数据（测试 setup 用，不经过 Commit，不受 commitErr 影响）。
func (r *d2ProbeObsRepo) presetMeta(id int64, scope string, lastEventAt time.Time, revision int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.meta[d2Key(id, scope)] = d2ObsMeta{lastEventAt: lastEventAt, revision: revision}
}

func (r *d2ProbeObsRepo) entryFor(id int64, scope string) (map[string]any, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[d2Key(id, scope)]
	if !ok {
		return nil, false
	}
	out := make(map[string]any, len(e))
	for k, v := range e {
		out[k] = v
	}
	return out, true
}

// ===================== 预算（Done when：预算） =====================

// 超 60 候选总发出量不超限：同账号滚动 60s 滑窗内最多 60 次。
func TestD2ProbeBudget_CapAt60PerMinute(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newProbeBudgetManager(func() time.Time { return now })

	const N = 200
	okCount := 0
	for i := 0; i < N; i++ {
		if b.TryAcquire(1, fmt.Sprintf("m%d", i)) {
			okCount++
		}
	}
	require.Equal(t, probeBudgetPerMinute, okCount, "同账号滚动 60s 窗口出站不得超过 60/min")
	// 窗口内第 61 次必失败。
	require.False(t, b.TryAcquire(1, "overflow"), "窗口已满应被拒绝")
}

// 混合候选（模型级 + 账号级）共用同一预算窗口，合计 ≤60/min：窗口满后无论是模型级
// 还是账号级候选均被拒绝（共用节流）。
func TestD2ProbeBudget_MixedCandidatesShareWindow(t *testing.T) {
	now := time.Unix(2000, 0)
	b := newProbeBudgetManager(func() time.Time { return now })

	// 60 个模型级占满滚动窗口。
	for i := 0; i < probeBudgetPerMinute; i++ {
		require.True(t, b.TryAcquire(7, fmt.Sprintf("m%d", i)), "模型级候选应获准直到窗口满")
	}
	// 第 61 个无论是另一个模型级还是账号级，都因共用窗口而失败。
	require.False(t, b.TryAcquire(7, "another-model"), "窗口已满：模型级候选应被拒绝")
	require.False(t, b.TryAcquire(7, ""), "窗口已满：账号级候选同样被共用窗口拒绝")
}

// 账号级无模型在途键：账号级在途只阻塞账号级自身，不阻塞模型级候选，反之亦然。
func TestD2ProbeBudget_AccountLevelNoModelInflightKey(t *testing.T) {
	now := time.Unix(3000, 0)
	b := newProbeBudgetManager(func() time.Time { return now })

	require.True(t, b.TryAcquire(9, "m1"), "模型 m1 应获准")
	// 账号级与模型级在途键独立：账号级可立即获准。
	require.True(t, b.TryAcquire(9, ""), "账号级不应被模型级在途键阻塞")
	// 同一模型 m1 仍在途 → 拒绝。
	require.False(t, b.TryAcquire(9, "m1"), "m1 在途应被去重拒绝")
	// 另一模型 m2 不受影响。
	require.True(t, b.TryAcquire(9, "m2"), "m2 不应被 m1 在途阻塞")
	// 第二个账号级在途 → 拒绝（账号级单在途键）。
	require.False(t, b.TryAcquire(9, ""), "账号级单在途键应去重")
}

// 超时占满并发槽不阻塞后续候选下一轮派发：滚动窗口滑动后新候选可派发，
// 且不同模型/账号级在途位互不阻塞（确定性时钟）。
func TestD2ProbeBudget_TimeoutSlotSlidesNextRound(t *testing.T) {
	clock := time.Unix(4000, 0)
	b := newProbeBudgetManager(func() time.Time { return clock })

	// t=0 占满 60 个模型窗口（在途位永不释放，模拟请求挂起超时）。
	for i := 0; i < probeBudgetPerMinute; i++ {
		require.True(t, b.TryAcquire(3, fmt.Sprintf("m%d", i)))
	}
	// 同刻第 61 个被窗口拒绝。
	require.False(t, b.TryAcquire(3, "overflow"), "窗口满应拒绝")

	// 滚动窗口滑动到 t=61s：先前 60 个发送已滑出窗口，新候选可派发，
	// 证明超时占满并发槽不永久阻塞后续候选。
	clock = clock.Add(probeBudgetWindow + time.Second)
	require.True(t, b.TryAcquire(3, "next-round"), "窗口滑动后新候选应可派发")
}

// ===================== 冻结上界（Done when：冻结上界） =====================

// 队尾候选在先前批次恢复移出后阈值不缩短：冻结值仅首次进入调度时计算一次。
func TestD2FreezeCandidateBound_TailNotShrunkOnShrink(t *testing.T) {
	svc := &AccountHealthRecoveryProbeService{frozenBounds: make(map[string]int)}

	// 候选总数=100 时进入调度，冻结队尾候选 A。
	svc.freezeCandidateBound(1, "A", 100)
	// 早期候选恢复移出，集合收缩到 50，再次进入调度。
	svc.freezeCandidateBound(1, "B", 50)
	// 重新进入时不应缩短已冻结的 A。
	svc.freezeCandidateBound(1, "A", 50)

	require.Equal(t, 100, svc.frozenCandidateBound(1, "A"), "队尾候选冻结值不得因集合收缩而缩短")
	require.Equal(t, 50, svc.frozenCandidateBound(1, "B"), "后进入候选按当时总数冻结")
}

// 普通场景（≤60，无预算等待项）与最坏场景（冻结值 >60）两口径闭合。
func TestD2ComputeProbeUpperBound_NormalVsWorst(t *testing.T) {
	rp, ht, cd := probeRoundPeriod, probeRequestHardTimeout, probeStartupCooldown
	base := rp + ht + cd

	// 普通场景：候选总数=60（≤60）→ 无额外预算等待项。
	require.Equal(t, base, ComputeProbeUpperBound(60, rp, ht, cd), "普通场景=轮次周期+硬超时+冷启动，无预算等待项")
	require.Equal(t, base, ComputeProbeUpperBound(1, rp, ht, cd), "候选=1 同样无预算等待项")
	require.Equal(t, base, ComputeProbeUpperBound(0, rp, ht, cd), "候选=0 退化为 0 预算分钟")

	// 最坏场景：候选总数=100（>60）→ ceil(100/60)=2 分钟预算等待。
	require.Equal(t, 2*time.Minute+base, ComputeProbeUpperBound(100, rp, ht, cd), "最坏场景=2 分钟+轮次+硬超时+冷启动")
	// 候选=61 → 仍 2 分钟（ceil(61/60)=2）。
	require.Equal(t, 2*time.Minute+base, ComputeProbeUpperBound(61, rp, ht, cd))
}

// ===================== 未冻结空集合语义（终审 #9） =====================

// 未冻结（key 不存在）的 scope 必须直接返回 0（空集合→基线），不得把总数 0 传入
// 公式得到 135s 基底和；只有确实冻结过的候选才计算探测上界。
func TestD2AccountFreshnessUpperBound_UnfrozenIsZero(t *testing.T) {
	svc := &AccountHealthRecoveryProbeService{frozenBounds: make(map[string]int)}

	require.Equal(t, time.Duration(0), svc.AccountFreshnessUpperBound(1, "never-scheduled"),
		"未冻结 scope 必须返回 0（空集合），不得计算基底和")
	require.Equal(t, time.Duration(0), svc.AccountFreshnessUpperBound(1, ""),
		"未冻结账号级维度同样返回 0")

	// 冻结后返回公式值。
	svc.freezeCandidateBound(1, "A", 100)
	require.Equal(t,
		ComputeProbeUpperBound(100, probeRoundPeriod, probeRequestHardTimeout, probeStartupCooldown),
		svc.AccountFreshnessUpperBound(1, "A"),
		"冻结 scope 返回公式值")
	require.Equal(t, time.Duration(0), svc.AccountFreshnessUpperBound(1, "B"),
		"同账号其他未冻结 scope 仍为 0")
	// 显式冻结 0 也是「已冻结」→ 按公式（0 预算分钟）计算，与「未冻结返回 0」区分。
	svc.freezeCandidateBound(1, "Z", 0)
	require.Equal(t,
		ComputeProbeUpperBound(0, probeRoundPeriod, probeRequestHardTimeout, probeStartupCooldown),
		svc.AccountFreshnessUpperBound(1, "Z"),
		"显式冻结 0 仍按公式计算（非未冻结）")
}

// ===================== 冻结上界生命周期（终审 #8） =====================

// 冻结值生命周期绑定当前异常候选周期：模型级探测恢复成功后清除冻结值，
// 下一次异常重新冻结时按当时候选总数（130）计算，不复用上一轮（1）。
func TestD2FrozenBound_LifecycleClearedOnModelRecovery(t *testing.T) {
	svc := &AccountHealthRecoveryProbeService{frozenBounds: make(map[string]int)}

	// 第一轮异常：候选总数 1，冻结候选 A。
	svc.freezeCandidateBound(7, "A", 1)
	require.Equal(t, 1, svc.frozenCandidateBound(7, "A"), "首次异常按候选总数 1 冻结")

	// 恢复成功：走既有结果应用点（probeOneTokenHarbor）→ 应清除冻结值。
	svc.probeTokenHarborOverride = func(_ context.Context, _ *Account, _ string) (ProbeOutcome, error) {
		return ProbeOutcomeSuccess, nil
	}
	svc.probeOneTokenHarbor(context.Background(), &Account{ID: 7}, tokenHarborCandidate{modelKey: "A"})
	require.Equal(t, 0, svc.frozenCandidateBound(7, "A"), "模型级恢复成功后应清除冻结值")

	// 再次异常：候选总数 130，重新冻结应按 130，不复用旧值 1。
	svc.freezeCandidateBound(7, "A", 130)
	require.Equal(t, 130, svc.frozenCandidateBound(7, "A"), "再次异常应按新一轮候选总数 130 冻结")
	require.Equal(t,
		ComputeProbeUpperBound(130, probeRoundPeriod, probeRequestHardTimeout, probeStartupCooldown),
		svc.AccountFreshnessUpperBound(7, "A"),
		"阈值应按 130 计算（不复用 1）")
}

// 账号级（无模型键）候选同样在恢复成功后清除冻结值，下一轮重新冻结。
func TestD2FrozenBound_LifecycleClearedOnAccountLevelRecovery(t *testing.T) {
	svc := &AccountHealthRecoveryProbeService{frozenBounds: make(map[string]int)}

	svc.freezeCandidateBound(8, "", 1)
	require.Equal(t, 1, svc.frozenCandidateBound(8, ""))

	// 账号级恢复成功（probeOneTokenHarbor 账号级结果应用点）→ 清除账号级冻结值。
	svc.probeTokenHarborOverride = func(_ context.Context, _ *Account, _ string) (ProbeOutcome, error) {
		return ProbeOutcomeSuccess, nil
	}
	svc.probeOneTokenHarbor(context.Background(), &Account{ID: 8}, tokenHarborCandidate{modelKey: "", accountLevel: true})
	require.Equal(t, 0, svc.frozenCandidateBound(8, ""), "账号级恢复成功后应清除账号级冻结值")

	svc.freezeCandidateBound(8, "", 130)
	require.Equal(t, 130, svc.frozenCandidateBound(8, ""), "再次异常应按 130 冻结")
}

// ===================== 写入口（Done when：写入口） =====================

// 同账号不同模型候选并发成功与并发 429，各模型键状态与观测时间均正确落盘、不互相覆盖。
func TestD2ApplyObservation_ConcurrentSuccessAnd429(t *testing.T) {
	repo := newD2ProbeObsRepo()
	svc := &RateLimitService{accountRepo: repo}
	const acct int64 = 11

	// 预置 A、B 两个模型级限流条目。
	repo.presetEntry(acct, "A", map[string]any{
		"rate_limit_reset_at": time.Unix(10, 0).UTC().Format(time.RFC3339),
	})
	repo.presetEntry(acct, "B", map[string]any{
		"rate_limit_reset_at": time.Unix(20, 0).UTC().Format(time.RFC3339),
	})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = svc.ApplyModelRateLimitObservation(context.Background(), acct, "A", ModelRateLimitObservation{
			EventTime: time.Unix(100, 0), Outcome: ProbeOutcomeSuccess,
		})
	}()
	go func() {
		defer wg.Done()
		_ = svc.ApplyModelRateLimitObservation(context.Background(), acct, "B", ModelRateLimitObservation{
			EventTime: time.Unix(100, 0), Outcome: ProbeOutcomeFreeTier429,
			ResetAt: time.Unix(20, 0), Reason: "tokenharbor_free_tier_exhausted:x",
		})
	}()
	wg.Wait()

	// A 成功 → 幂等清除（条目消失）。
	aEntry, aOK := repo.entryFor(acct, "A")
	require.False(t, aOK, "A 成功应清除限流条目")
	_ = aEntry
	// B 免费档 429 → 保留条目、置 observed_at、reset_at/reason 保留。
	bEntry, bOK := repo.entryFor(acct, "B")
	require.True(t, bOK, "B 免费档 429 不应清除限流条目")
	require.Contains(t, bEntry, entryObservedAtKey, "B 应记录 observed_at")
	require.Equal(t, time.Unix(20, 0).UTC().Format(time.RFC3339), bEntry["rate_limit_reset_at"], "B 的 reset_at 应保留不被扩展")
}

// 旧事件整体 no-op：较旧事件时间不覆盖较新写入的状态+时间。
func TestD2ApplyObservation_OldEventNoOp(t *testing.T) {
	repo := newD2ProbeObsRepo()
	svc := &RateLimitService{accountRepo: repo}
	const acct int64 = 12
	const scope = "m-scope"

	t2 := time.Unix(200, 0)
	// 现有权威状态：observed_at=t2，且 meta 记录 lastEventAt=t2, rev=5。
	repo.presetEntry(acct, scope, map[string]any{
		entryObservedAtKey:    t2.UTC().Format(time.RFC3339),
		"rate_limit_reset_at": time.Unix(50, 0).UTC().Format(time.RFC3339),
	})
	repo.presetMeta(acct, scope, t2, 5)

	// 较旧事件 t1 < t2 → 整体 no-op。
	err := svc.ApplyModelRateLimitObservation(context.Background(), acct, scope, ModelRateLimitObservation{
		EventTime: time.Unix(100, 0), Outcome: ProbeOutcomeFreeTier429,
		ResetAt: time.Unix(999, 0), Reason: "new-reason",
	})
	require.NoError(t, err)

	entry, ok := repo.entryFor(acct, scope)
	require.True(t, ok)
	require.Equal(t, t2.UTC().Format(time.RFC3339), entry[entryObservedAtKey], "旧事件不得刷新 observed_at")
	require.Equal(t, time.Unix(50, 0).UTC().Format(time.RFC3339), entry["rate_limit_reset_at"], "旧事件不得扩展 reset_at")
	m, mOK := repo.metaFor(acct, scope)
	require.True(t, mOK)
	require.Equal(t, int64(5), m.revision, "旧事件不得推进 revision")
}

// 同事件时间精度碰撞：两序裁决一致 + 重放一致。比较键固定 (事件时间, tie_breaker)。
func TestD2ApplyObservation_SameTimeTieBreakerConsistent(t *testing.T) {
	T := time.Unix(300, 0)

	// 正序：Success(tb=1) 先，FreeTier429(tb=2) 后。
	repoFwd := newD2ProbeObsRepo()
	svcFwd := &RateLimitService{accountRepo: repoFwd}
	const acct int64 = 21
	_ = svcFwd.ApplyModelRateLimitObservation(context.Background(), acct, "S", ModelRateLimitObservation{
		EventTime: T, Outcome: ProbeOutcomeSuccess, TieBreaker: 1,
	})
	_ = svcFwd.ApplyModelRateLimitObservation(context.Background(), acct, "S", ModelRateLimitObservation{
		EventTime: T, Outcome: ProbeOutcomeFreeTier429, TieBreaker: 2,
		ResetAt: time.Unix(1, 0), Reason: "r",
	})
	fwdEntry, fwdOK := repoFwd.entryFor(acct, "S")
	fwdMeta, _ := repoFwd.metaFor(acct, "S")

	// 逆序：FreeTier429(tb=2) 先，Success(tb=1) 后。
	repoRev := newD2ProbeObsRepo()
	svcRev := &RateLimitService{accountRepo: repoRev}
	_ = svcRev.ApplyModelRateLimitObservation(context.Background(), acct, "S", ModelRateLimitObservation{
		EventTime: T, Outcome: ProbeOutcomeFreeTier429, TieBreaker: 2,
		ResetAt: time.Unix(1, 0), Reason: "r",
	})
	_ = svcRev.ApplyModelRateLimitObservation(context.Background(), acct, "S", ModelRateLimitObservation{
		EventTime: T, Outcome: ProbeOutcomeSuccess, TieBreaker: 1,
	})
	revEntry, revOK := repoRev.entryFor(acct, "S")
	revMeta, _ := repoRev.metaFor(acct, "S")

	// 两序最终一致：FreeTier429（更高 tie_breaker）胜出，条目不被清除。
	require.Equal(t, fwdOK, revOK, "两序最终可观测状态应一致")
	require.True(t, fwdOK, "更高 tie_breaker 的 FreeTier429 应保留条目")
	require.Equal(t, fwdEntry[entryObservedAtKey], revEntry[entryObservedAtKey], "observed_at 两序一致")
	require.Equal(t, fwdMeta.revision, revMeta.revision, "revision 两序一致")

	// 重放一致：再对逆序结果重放同序一次，状态不变。
	_ = svcRev.ApplyModelRateLimitObservation(context.Background(), acct, "S", ModelRateLimitObservation{
		EventTime: T, Outcome: ProbeOutcomeFreeTier429, TieBreaker: 2,
		ResetAt: time.Unix(1, 0), Reason: "r",
	})
	replayEntry, _ := repoRev.entryFor(acct, "S")
	require.Equal(t, revEntry[entryObservedAtKey], replayEntry[entryObservedAtKey], "重放后状态不变")
}

// 默认 tie_breaker=0（由入口分配入口顺序号）不得被误判为字面 0 而错误 no-op 正常新观测。
func TestD2ApplyObservation_DefaultTieBreakerNotNoOp(t *testing.T) {
	repo := newD2ProbeObsRepo()
	svc := &RateLimitService{accountRepo: repo}
	const acct int64 = 22
	T := time.Unix(400, 0)

	_ = svc.ApplyModelRateLimitObservation(context.Background(), acct, "S", ModelRateLimitObservation{
		EventTime: T, Outcome: ProbeOutcomeFreeTier429, // TieBreaker=0
	})
	m1, _ := repo.metaFor(acct, "S")
	require.Equal(t, int64(1), m1.revision, "首次写入 revision=1")

	// 同事件时间再次写入（TieBreaker=0）→ 入口分配 rev+1=2，应应用而非 no-op。
	_ = svc.ApplyModelRateLimitObservation(context.Background(), acct, "S", ModelRateLimitObservation{
		EventTime: T, Outcome: ProbeOutcomeFreeTier429,
	})
	m2, _ := repo.metaFor(acct, "S")
	require.Equal(t, int64(2), m2.revision, "默认 tie_breaker 不应被误判 no-op，revision 应推进")
}

// ===================== 恢复迁移（Done when：恢复迁移） =====================

// 最小请求成功（含慢速 2xx）幂等清除限流条目，且多次成功保持清除。
func TestD2ApplyObservation_SuccessIdempotentClear(t *testing.T) {
	repo := newD2ProbeObsRepo()
	svc := &RateLimitService{accountRepo: repo}
	const acct int64 = 31

	repo.presetEntry(acct, "M", map[string]any{
		"rate_limit_reset_at": time.Unix(5, 0).UTC().Format(time.RFC3339),
	})
	// 连续两次成功（含慢速 2xx 视为同一 Outcome=Success）。
	for i := 0; i < 2; i++ {
		err := svc.ApplyModelRateLimitObservation(context.Background(), acct, "M", ModelRateLimitObservation{
			EventTime: time.Unix(500+int64(i), 0), Outcome: ProbeOutcomeSuccess,
		})
		require.NoError(t, err)
	}
	_, ok := repo.entryFor(acct, "M")
	require.False(t, ok, "成功应幂等清除限流条目")
}

// /models 成功但目标模型仍 429 不解除：模型级限流条目在 FreeTier429（模型仍受限）下保留。
func TestD2ApplyObservation_FreeTier429DoesNotClear(t *testing.T) {
	repo := newD2ProbeObsRepo()
	svc := &RateLimitService{accountRepo: repo}
	const acct int64 = 32
	const scope = "deepseek:free"

	repo.presetEntry(acct, scope, map[string]any{
		"rate_limit_reset_at": time.Unix(60, 0).UTC().Format(time.RFC3339),
		"reason":              tokenHarborFreeTierReasonPrefix + ":x",
	})
	// 目标模型探测仍返回免费档 429（=/models 成功但模型 429 的等价判定）。
	err := svc.ApplyModelRateLimitObservation(context.Background(), acct, scope, ModelRateLimitObservation{
		EventTime: time.Unix(600, 0), Outcome: ProbeOutcomeFreeTier429,
		ResetAt: time.Unix(60, 0), Reason: tokenHarborFreeTierReasonPrefix + ":x",
	})
	require.NoError(t, err)

	entry, ok := repo.entryFor(acct, scope)
	require.True(t, ok, "模型仍 429 不应解除限流")
	require.Contains(t, entry, entryObservedAtKey, "应刷新观测时间确认仍受限")
	require.Equal(t, time.Unix(60, 0).UTC().Format(time.RFC3339), entry["rate_limit_reset_at"], "FreeTier429 不扩展既有 reset_at")
}

// ===================== 不可分类结果（Done when：冻结上界 - 不可分类） =====================

// 不可分类结果（5xx/超时/传输/非免费档 429）只更新尝试时间、不改状态不刷观测时间。
// 非免费档 429 经上游分类为 Unclassified，故不延长限流、不刷新 observed_at。
func TestD2ApplyObservation_UnclassifiedOnlyAttemptTime(t *testing.T) {
	repo := newD2ProbeObsRepo()
	svc := &RateLimitService{accountRepo: repo}
	const acct int64 = 33
	const scope = "deepseek:free"

	observed := time.Unix(700, 0)
	repo.presetEntry(acct, scope, map[string]any{
		entryObservedAtKey:    observed.UTC().Format(time.RFC3339),
		"rate_limit_reset_at": time.Unix(70, 0).UTC().Format(time.RFC3339),
	})
	// 非免费档 429 / 5xx / 超时 均映射为 Unclassified。
	err := svc.ApplyModelRateLimitObservation(context.Background(), acct, scope, ModelRateLimitObservation{
		EventTime: time.Unix(710, 0), Outcome: ProbeOutcomeUnclassified,
	})
	require.NoError(t, err)

	entry, ok := repo.entryFor(acct, scope)
	require.True(t, ok)
	require.Contains(t, entry, entryAttemptedAtKey, "不可分类结果应记录 attempted_at")
	require.Equal(t, observed.UTC().Format(time.RFC3339), entry[entryObservedAtKey], "不可分类结果不得刷新 observed_at")
	require.Equal(t, time.Unix(70, 0).UTC().Format(time.RFC3339), entry["rate_limit_reset_at"], "不可分类结果不得延长/扩展限流")
}

// ===================== E1：状态条目+元数据原子提交 =====================

// 原子性定向测试：meta 写入（第二次写）失败时，状态条目/清除同样不变（原子性）。
// commitErr 模拟「第二次写失败」——写入口改为单次提交后，该失败等价于整体失败。
func TestE1ApplyObservation_CommitFailureLeavesStateUntouched(t *testing.T) {
	ctx := context.Background()

	// 场景 1：FreeTier429 写入（写条目+推进 meta）失败 → 条目与 meta 均保持原值。
	{
		repo := newD2ProbeObsRepo()
		svc := &RateLimitService{accountRepo: repo}
		const acct int64 = 61
		const scope = "deepseek:free"

		// 预置存量受限状态：lastEventAt=t1, rev=3。
		t1 := time.Unix(800, 0)
		repo.presetEntry(acct, scope, map[string]any{
			entryObservedAtKey:    t1.UTC().Format(time.RFC3339),
			"rate_limit_reset_at": time.Unix(80, 0).UTC().Format(time.RFC3339),
			"reason":              tokenHarborFreeTierReasonPrefix + ":x",
		})
		repo.presetMeta(acct, scope, t1, 3)

		// 注入提交失败（模拟 meta 写失败）。
		repo.commitErr = errors.New("simulated meta write failure")

		err := svc.ApplyModelRateLimitObservation(ctx, acct, scope, ModelRateLimitObservation{
			EventTime: time.Unix(900, 0), Outcome: ProbeOutcomeFreeTier429,
			ResetAt: time.Unix(90, 0), Reason: tokenHarborFreeTierReasonPrefix + ":y",
		})
		require.Error(t, err, "提交失败应返回错误")

		// 原子性：条目未变（observed_at/reset_at/reason 均保持原值）。
		entry, ok := repo.entryFor(acct, scope)
		require.True(t, ok, "条目必须仍存在")
		require.Equal(t, t1.UTC().Format(time.RFC3339), entry[entryObservedAtKey], "整体失败时 observed_at 不得刷新")
		require.Equal(t, time.Unix(80, 0).UTC().Format(time.RFC3339), entry["rate_limit_reset_at"], "整体失败时 reset_at 不得改变")
		require.Equal(t, tokenHarborFreeTierReasonPrefix+":x", entry["reason"], "整体失败时 reason 不得改变")
		// 原子性：meta 未推进。
		m, mOK := repo.metaFor(acct, scope)
		require.True(t, mOK)
		require.Equal(t, int64(3), m.revision, "整体失败时 revision 不得推进")
		require.True(t, m.lastEventAt.Equal(t1), "整体失败时 last_event_at 不得改变")
	}

	// 场景 2：模型级成功（清除条目+推进 meta）失败 → 条目仍在、meta 未推进。
	{
		repo := newD2ProbeObsRepo()
		svc := &RateLimitService{accountRepo: repo}
		const acct int64 = 62
		const scope = "gpt-4"

		t1 := time.Unix(1000, 0)
		repo.presetEntry(acct, scope, map[string]any{
			"rate_limit_reset_at": time.Unix(100, 0).UTC().Format(time.RFC3339),
		})
		repo.presetMeta(acct, scope, t1, 7)

		repo.commitErr = errors.New("simulated meta write failure")

		err := svc.ApplyModelRateLimitObservation(ctx, acct, scope, ModelRateLimitObservation{
			EventTime: time.Unix(1100, 0), Outcome: ProbeOutcomeSuccess,
		})
		require.Error(t, err, "提交失败应返回错误")

		// 原子性：清除未生效，条目仍在。
		entry, ok := repo.entryFor(acct, scope)
		require.True(t, ok, "整体失败时条目不得被清除")
		require.Equal(t, time.Unix(100, 0).UTC().Format(time.RFC3339), entry["rate_limit_reset_at"], "条目内容不得改变")
		m, mOK := repo.metaFor(acct, scope)
		require.True(t, mOK)
		require.Equal(t, int64(7), m.revision, "整体失败时 revision 不得推进")
	}

	// 场景 3：双写成功 → 状态与 meta 一并提交（与既有 TestD2 行为兼容）。
	{
		repo := newD2ProbeObsRepo()
		svc := &RateLimitService{accountRepo: repo}
		const acct int64 = 63
		const scope = "deepseek:free"

		t1 := time.Unix(1200, 0)
		repo.presetEntry(acct, scope, map[string]any{
			"rate_limit_reset_at": time.Unix(120, 0).UTC().Format(time.RFC3339),
		})
		repo.presetMeta(acct, scope, t1, 9)

		err := svc.ApplyModelRateLimitObservation(ctx, acct, scope, ModelRateLimitObservation{
			EventTime: time.Unix(1300, 0), Outcome: ProbeOutcomeFreeTier429,
			ResetAt: time.Unix(120, 0), Reason: tokenHarborFreeTierReasonPrefix + ":x",
		})
		require.NoError(t, err)

		entry, ok := repo.entryFor(acct, scope)
		require.True(t, ok)
		require.Equal(t, time.Unix(1300, 0).UTC().Format(time.RFC3339), entry[entryObservedAtKey], "成功时 observed_at 应更新")
		require.Equal(t, time.Unix(120, 0).UTC().Format(time.RFC3339), entry["rate_limit_reset_at"], "成功时 reset_at 保留")
		m, _ := repo.metaFor(acct, scope)
		require.Equal(t, int64(10), m.revision, "成功时 revision 推进为 rev+1")
	}
}

// ===================== 启动冷却（Done when：冷却） =====================

// 满预算后立即重启：新进程首个 60s 内不发 TokenHarbor 主动探测（确定性时钟）。
func TestD2ColdStart_SuppressesTokenHarborProbeFirst60s(t *testing.T) {
	now := time.Unix(800, 0)
	repo := newD2ProbeObsRepo()
	rl := &RateLimitService{accountRepo: repo}

	// 候选账号：extra 含 active 的 TokenHarbor 免费档模型级限流。
	cand := &Account{
		ID: 42,
		Extra: map[string]any{
			modelRateLimitsKey: map[string]any{
				"deepseek:free": map[string]any{
					"reason":              tokenHarborFreeTierReasonPrefix + ":x",
					"rate_limit_reset_at": now.Add(time.Hour).UTC().Format(time.RFC3339),
				},
			},
		},
	}
	// ListTokenHarborModelRateLimitedAccounts 返回该候选。
	repo.listTokenHarbor = []*Account{cand}

	var mu sync.Mutex
	calls := 0
	svc := &AccountHealthRecoveryProbeService{
		nowFunc:   func() time.Time { return now },
		startTime: now, // 进程刚启动
		budget:    newProbeBudgetManager(func() time.Time { return now }),
		frozenBounds: make(map[string]int),
		rateLimit:    rl,
		accountRepo:  repo,
		probeTokenHarborOverride: func(_ context.Context, _ *Account, _ string) (ProbeOutcome, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			return ProbeOutcomeSuccess, nil
		},
	}

	// 冷启动内：不发探测。
	svc.runTokenHarborProbePhase(context.Background(), map[int64]struct{}{})
	mu.Lock()
	require.Equal(t, 0, calls, "冷启动 60s 内不得发送主动探测")
	mu.Unlock()

	// 越过冷启动：启动时刻设为 61s 前，应发送探测。
	svc.startTime = now.Add(-61 * time.Second)
	svc.runTokenHarborProbePhase(context.Background(), map[int64]struct{}{})
	mu.Lock()
	require.Equal(t, 1, calls, "冷启动结束后应发送主动探测")
	mu.Unlock()
}

// ===================== RunOnce 按候选类使能门禁（D6 P0 缺口） =====================

// TestRunOnce_ProbeClassGating 锁定「按候选类拆分使能门禁」：
//   - settings.Enabled=false && Probe.Enabled=true → TokenHarbor 相位执行，熔断相位不执行；
//   - Probe.Enabled=false → 两相位均不执行。
//
// 全程确定性时钟，不依赖 sleep。
func TestRunOnce_ProbeClassGating(t *testing.T) {
	now := time.Unix(100000, 0)

	// 账号级（熔断）候选：temp-unschedulable + 未来 until + 可恢复标记。
	breakerUntil := now.Add(time.Hour)
	breakerAcc := &Account{
		ID: 1,
		TempUnschedulableReason: func() string {
			b, _ := json.Marshal(TempUnschedState{MatchedKeyword: openAIAPIKeyHealthBreakerReason, ProbeAttempts: 0})
			return string(b)
		}(),
		TempUnschedulableUntil: &breakerUntil,
	}
	// TokenHarbor 免费档模型级候选：extra 含 active 模型级限流。
	thAcc := &Account{
		ID: 2,
		Extra: map[string]any{
			modelRateLimitsKey: map[string]any{
				"deepseek:free": map[string]any{
					"reason":              tokenHarborFreeTierReasonPrefix + ":x",
					"rate_limit_reset_at": now.Add(time.Hour).UTC().Format(time.RFC3339),
				},
			},
		},
	}

	buildSvc := func(breakerEnabled, probeEnabled bool) (*AccountHealthRecoveryProbeService, *int64, *int64) {
		settings := OpenAIAPIKeyHealthBreakerSettings{
			Enabled: breakerEnabled,
			Probe:   &OpenAIAPIKeyHealthBreakerProbeSettings{Enabled: probeEnabled, IntervalSeconds: 30, MaxAttempts: 3},
		}
		raw, err := json.Marshal(settings)
		require.NoError(t, err)
		ss := NewSettingService(&fakeSettingRepo{vals: map[string]string{
			SettingKeyOpenAIAPIKeyHealthBreakerSettings: string(raw),
		}}, nil)

		repo := newD2ProbeObsRepo()
		repo.listTokenHarbor = []*Account{thAcc}
		repo.listTempUnsched = []*Account{breakerAcc}
		rl := &RateLimitService{accountRepo: repo}

		var breakerCalls, thCalls int64
		svc := &AccountHealthRecoveryProbeService{
			nowFunc:         func() time.Time { return now },
			startTime:       now.Add(-61 * time.Second), // 越过冷启动 60s 门禁
			budget:          newProbeBudgetManager(func() time.Time { return now }),
			frozenBounds:    make(map[string]int),
			rateLimit:       rl,
			accountRepo:     repo,
			settingService:  ss,
			probeOverride: func(_ context.Context, _ *Account) (bool, error) {
				atomic.AddInt64(&breakerCalls, 1)
				return false, nil
			},
			probeTokenHarborOverride: func(_ context.Context, _ *Account, _ string) (ProbeOutcome, error) {
				atomic.AddInt64(&thCalls, 1)
				return ProbeOutcomeSuccess, nil
			},
		}
		return svc, &breakerCalls, &thCalls
	}

	// Case A：settings.Enabled=false && Probe.Enabled=true。
	svc, cb, th := buildSvc(false, true)
	svc.RunOnce(context.Background())
	require.Equal(t, int64(0), atomic.LoadInt64(cb), "熔断相位在 Enabled=false 下不应执行")
	require.Equal(t, int64(1), atomic.LoadInt64(th), "TokenHarbor 相位在 Probe.Enabled=true 下应执行（默认配置不再被双开关短路）")

	// Case B：Probe.Enabled=false（无论 breaker 开关）。
	svc, cb, th = buildSvc(true, false)
	svc.RunOnce(context.Background())
	require.Equal(t, int64(0), atomic.LoadInt64(cb), "Probe.Enabled=false 时熔断相位不执行")
	require.Equal(t, int64(0), atomic.LoadInt64(th), "Probe.Enabled=false 时 TokenHarbor 相位不执行")

	// Case C（健全性）：双开关均开 → 两相位都执行。
	svc, cb, th = buildSvc(true, true)
	svc.RunOnce(context.Background())
	require.Equal(t, int64(1), atomic.LoadInt64(cb), "双开关开启时熔断相位执行")
	require.Equal(t, int64(1), atomic.LoadInt64(th), "双开关开启时 TokenHarbor 相位执行")
}

// ===================== E26：恢复事务失败关闭（E20 验收整改） =====================

// 仓库未实现 observationTxRepository 窄面时，commitRecoveryAtomically 必须失败关闭：
// 返回明确错误且绝不调用 commit（即不做任何写入）。这堵住 E20 修复前的静默非原子退化分支
// （E9b 同款失败关闭——缺原子能力是明确错误，不留降级分支）。
func TestE26RecoveryTx_FailClosedWhenRepoLacksAtomicCapability(t *testing.T) {
	// mockAccountRepoForGemini 未实现 WithObservationTx，等效于「未实现原子窄面」的仓库。
	rl := &RateLimitService{accountRepo: &mockAccountRepoForGemini{}}
	var commitCalled atomic.Bool
	err := rl.commitRecoveryAtomically(context.Background(), 1, "model/scope", false, func(txCtx context.Context) error {
		commitCalled.Store(true)
		return nil
	})
	require.Error(t, err, "缺原子能力必须失败关闭")
	require.Contains(t, err.Error(), "does not support atomic recovery transaction", "错误文案须明确指向缺失的原子恢复事务能力")
	require.False(t, commitCalled.Load(), "失败关闭时 commit 不得被调用（不做任何写入）")
}
