//go:build unit

// D2 探针相位退役后的保留资产测试（th-kira-quota-lifecycle 方案 §7 旧链退役③）。
// 原 account_health_recovery_probe_d2_test.go 中的 D2 相位用例随相位删除；本文件仅保留：
//  1. 共享内存仓储夹具 d2ProbeObsRepo（e24/e27 等存活测试依赖）；
//  2. 写入口（ApplyModelRateLimitObservation）行为锁定用例——写入口本体在
//     ratelimit_service.go（003A 所有），分类枚举 ProbeOutcome 仍被状态机确认探针消费；
//  3. 冻结上界族用例——冻结值经 RateLimitService.freshnessBoundsProvider 供三维
//     新鲜度阈值公式消费（account_freshness_threshold.go / account_freshness_alert.go）；
//  4. E26 恢复事务失败关闭（ratelimit_service.go 资产）。
package service

import (
	"context"
	"encoding/json"
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
	mu              sync.Mutex
	entries         map[string]map[string]any
	meta            map[string]d2ObsMeta
	listTempUnsched []*Account
	// commitErr 非 nil 时，CommitModelRateLimitObservation 返回该错误且不改变任何状态
	//（E1 原子性定向测试：模拟「第二次写失败」场景）。
	commitErr error
	// commitScopeCount 记录 CommitModelRateLimitObservation 按 scope 的写入次数。
	commitScopeCount map[string]int
	// syncSnapshotCalls 记录 SyncSchedulerAccountSnapshot（恢复提交后快照同步）被调用次数。
	syncSnapshotCalls int
	// syncSnapshotErr 非 nil 时，SyncSchedulerAccountSnapshot 返回该错误。
	syncSnapshotErr error
	// clearTempUnschedCalls 记录 ClearTempUnschedulable 被调用次数。
	clearTempUnschedCalls int
	// entryErr 非 nil 时，GetModelRateLimitEntry 返回该错误。
	entryErr error
	// initialSetCalls 记录 SetModelRateLimitWithPreciseReset（E8 #4 初始 SET）的入参。
	initialSetCalls []d2InitialSetCall
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
	r.initialSetCalls = append(r.initialSetCalls, d2InitialSetCall{
		id: id, scope: scope, resetAt: resetAt, preciseReset: preciseReset, reason: reason,
	})
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

// ClearTempUnschedulable 覆盖嵌入实现，记录调用次数。
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

// initialSetCalls 记录 SetModelRateLimitWithPreciseReset（E8 #4 初始 SET）的入参，
// 供定向测试断言初始 SET 参与事件裁决（旧 SET no-op / 与新探测不互覆）。
type d2InitialSetCall struct {
	id           int64
	scope        string
	resetAt      time.Time
	preciseReset bool
	reason       string
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

// ===================== 写入口（保留资产：ratelimit_service.go 所有） =====================

// 同账号不同模型候选并发成功与并发 429，各模型键状态与观测时间均正确落盘、不互相覆盖。
func TestApplyObservation_ConcurrentSuccessAnd429(t *testing.T) {
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
	_, aOK := repo.entryFor(acct, "A")
	require.False(t, aOK, "A 成功应清除限流条目")
	// B 免费档 429 → 保留条目、置 observed_at、reset_at/reason 保留。
	bEntry, bOK := repo.entryFor(acct, "B")
	require.True(t, bOK, "B 免费档 429 不应清除限流条目")
	require.Contains(t, bEntry, entryObservedAtKey, "B 应记录 observed_at")
	require.Equal(t, time.Unix(20, 0).UTC().Format(time.RFC3339), bEntry["rate_limit_reset_at"], "B 的 reset_at 应保留不被扩展")
}

// 旧事件整体 no-op：较旧事件时间不覆盖较新写入的状态+时间。
func TestApplyObservation_OldEventNoOp(t *testing.T) {
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
func TestApplyObservation_SameTimeTieBreakerConsistent(t *testing.T) {
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

// ===================== 冻结上界族（保留资产：freshness 阈值公式输入） =====================

// 队尾候选在先前批次恢复移出后阈值不缩短：冻结值仅首次进入调度时计算一次。
func TestFrozenBound_TailNotShrunkOnShrink(t *testing.T) {
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
func TestComputeProbeUpperBound_NormalVsWorst(t *testing.T) {
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

// 未冻结（key 不存在）的 scope 必须直接返回 0（空集合→基线），不得把总数 0 传入
// 公式得到 135s 基底和；只有确实冻结过的候选才计算探测上界。
func TestAccountFreshnessUpperBound_UnfrozenIsZero(t *testing.T) {
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

// ===================== E26：恢复事务失败关闭（E20 验收整改，ratelimit_service.go 资产） =====================

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

// ===================== RunOnce 相位收敛（D2 退役后的显式空处理） =====================

// D2 相位删除后 RunOnce 仅保留账号级熔断相位 + 孤儿告警清扫；
// settings 读取失败（err/nil Probe）→ 显式空处理直接返回，不触碰任何仓储。
func TestRunOnce_EmptyPhaseExplicitHandling(t *testing.T) {
	now := time.Unix(100000, 0)

	// settings 缺 Probe 段：无任何相位执行 → 空处理路径。
	ss := NewSettingService(&fakeSettingRepo{vals: map[string]string{
		SettingKeyOpenAIAPIKeyHealthBreakerSettings: `{"enabled":true}`,
	}}, nil)

	repo := newD2ProbeObsRepo()
	repo.listTempUnsched = []*Account{{ID: 1}}
	rl := &RateLimitService{accountRepo: repo}

	svc := &AccountHealthRecoveryProbeService{
		frozenBounds:   make(map[string]int),
		rateLimit:      rl,
		accountRepo:    repo,
		settingService: ss,
		probeOverride: func(_ context.Context, _ *Account) (bool, error) {
			t.Fatal("无 Probe 配置时不得发起任何探测")
			return false, nil
		},
	}
	_ = now
	svc.RunOnce(context.Background())
	require.Equal(t, 0, repo.clearTempUnschedCalls, "空相位下不得有任何恢复写入")
}

// settings 正常且双开关开启：仅熔断相位执行（D2 相位已退役，不再有第二相）。
func TestRunOnce_BreakerPhaseOnly(t *testing.T) {
	now := time.Unix(110000, 0)

	breakerUntil := now.Add(time.Hour)
	breakerAcc := &Account{
		ID: 1,
		TempUnschedulableReason: func() string {
			b, _ := json.Marshal(TempUnschedState{MatchedKeyword: openAIAPIKeyHealthBreakerReason, ProbeAttempts: 0})
			return string(b)
		}(),
		TempUnschedulableUntil: &breakerUntil,
	}

	settings := OpenAIAPIKeyHealthBreakerSettings{
		Enabled: true,
		Probe:   &OpenAIAPIKeyHealthBreakerProbeSettings{Enabled: true, IntervalSeconds: 30, MaxAttempts: 3},
	}
	raw, err := json.Marshal(settings)
	require.NoError(t, err)
	ss := NewSettingService(&fakeSettingRepo{vals: map[string]string{
		SettingKeyOpenAIAPIKeyHealthBreakerSettings: string(raw),
	}}, nil)

	repo := newD2ProbeObsRepo()
	repo.listTempUnsched = []*Account{breakerAcc}
	repo.accountsByID[1] = breakerAcc
	rl := &RateLimitService{accountRepo: repo}

	var breakerProbes int64
	svc := &AccountHealthRecoveryProbeService{
		frozenBounds:   make(map[string]int),
		rateLimit:      rl,
		accountRepo:    repo,
		settingService: ss,
		probeOverride: func(_ context.Context, _ *Account) (bool, error) {
			atomic.AddInt64(&breakerProbes, 1)
			return false, nil
		},
	}

	svc.RunOnce(context.Background())

	require.Equal(t, int64(1), atomic.LoadInt64(&breakerProbes), "熔断相位应执行 1 次探测（唯一相位）")
}
