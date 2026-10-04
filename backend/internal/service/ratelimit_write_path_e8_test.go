//go:build unit

package service

// E8 定向测试：写入口事件裁决三缺陷整改（终审重放 #2/#3/#4）。
//   - #2 读取失败停止读改写：GetModelRateLimitEntry 读错误必须沿唯一写入口 return，
//     不得当空条目继续提交，也不得推进事件元数据；
//   - #3 事件时间纳秒精度：meta 写入与读取统一 RFC3339Nano，同秒内纳秒精度裁决不误判；
//   - #4 初始 SET 参与事件裁决：初始 SET 在 per-account 写锁下读 meta → 裁决 → 原子写
//     （last_event_at=写入时刻、revision=rev+1），延迟旧探测不得清除新 SET 状态，初始
//     SET 与并发探测不互覆。

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ===================== #2 读取失败停止读改写 =====================

// 读错误时三分类分支均须沿唯一写入口 return：既不得提交（Commit 不被调用），
// 也不得推进 meta 裁决基线。
func TestE8ApplyObservation_ReadErrorAbortsWrite(t *testing.T) {
	readErr := errors.New("simulated entry read failure")

	for _, tc := range []struct {
		name    string
		outcome ProbeOutcome
		account bool
	}{
		{name: "account_level_success", outcome: ProbeOutcomeSuccess, account: true},
		{name: "free_tier_429", outcome: ProbeOutcomeFreeTier429},
		{name: "unclassified", outcome: ProbeOutcomeUnclassified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newD2ProbeObsRepo()
			repo.entryErr = readErr
			// 预置存量条目与 meta：若读失败被当作空条目继续提交，将丢失既有 reset_at/
			// reason/precise_reset 并推进 meta —— 断言两者均不得改变。
			repo.presetEntry(42, "S", map[string]any{
				"rate_limit_reset_at": time.Unix(60, 0).UTC().Format(time.RFC3339),
				"reason":              tokenHarborFreeTierReasonPrefix + ":x",
				"precise_reset":       true,
			})
			repo.presetMeta(42, "S", time.Unix(50, 0), 7)

			svc := &RateLimitService{accountRepo: repo}
			err := svc.ApplyModelRateLimitObservation(context.Background(), 42, "S", ModelRateLimitObservation{
				EventTime:    time.Now(),
				Outcome:      tc.outcome,
				AccountLevel: tc.account,
			})
			require.ErrorIs(t, err, readErr, "读错误必须沿唯一写入口传播")

			entry, ok := repo.entryFor(42, "S")
			require.True(t, ok, "读失败不得当空条目继续提交清除既有条目")
			require.Equal(t, "tokenharbor_free_tier_exhausted:x", entry["reason"], "既有 reason 不得被空条目覆盖")
			require.Equal(t, true, entry["precise_reset"], "既有 precise_reset 不得丢失")
			m, ok := repo.metaFor(42, "S")
			require.True(t, ok)
			require.Equal(t, int64(7), m.revision, "读失败不得推进事件裁决基线")
			require.True(t, m.lastEventAt.Equal(time.Unix(50, 0)), "读失败不得刷新 last_event_at")
		})
	}
}

// ===================== #3 事件时间纳秒精度（service 层同秒裁决） =====================

// 同秒内、亚秒精度碰撞：旧事件（早 500ms）不得误判为新并覆盖较新状态。
// 旧事件比较的是完整纳秒 EventTime，秒级 meta 写入会让同秒内早事件恒「等于」、
// 因 rev 更高而错误应用 —— 回归守卫。
func TestE8ApplyObservation_SameSecondNanoAdjudication(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()

	// t_new：业务 429 在 00.900s 落盘（revision 升至 n）。
	t_new := base.Add(900 * time.Millisecond)
	repo := newD2ProbeObsRepo()
	svc := &RateLimitService{accountRepo: repo}
	require.NoError(t, svc.ApplyModelRateLimitObservation(context.Background(), 43, "S", ModelRateLimitObservation{
		EventTime: t_new, Outcome: ProbeOutcomeFreeTier429,
		ResetAt: time.Unix(120, 0), Reason: "r-new",
	}))
	m1, _ := repo.metaFor(43, "S")
	require.Equal(t, int64(1), m1.revision)

	// 同秒内更旧事件：00.400s 的成功探测（revision 传入 0 → 分配 rev+1=2）。
	// 若裁决基线只有秒级精度，00.400s 会被判「等于」00.900s → 更高 rev 应用成功 → 清除新 429。
	t_old := base.Add(400 * time.Millisecond)
	require.NoError(t, svc.ApplyModelRateLimitObservation(context.Background(), 43, "S", ModelRateLimitObservation{
		EventTime: t_old, Outcome: ProbeOutcomeSuccess,
	}))
	entry, ok := repo.entryFor(43, "S")
	require.True(t, ok, "同秒内更旧成功不得清除较新 429 条目")
	require.Equal(t, "r-new", entry["reason"], "较新 429 的 reason 必须保留")
	require.Equal(t, t_new.UTC().Format(time.RFC3339Nano), entry[entryObservedAtKey], "observed_at 必须是较新事件时刻（纳秒精度，E20 #4）")
	m2, _ := repo.metaFor(43, "S")
	require.True(t, m2.lastEventAt.Equal(t_new), "meta 必须保留较新事件（纳秒精度）")
	require.Equal(t, int64(1), m2.revision, "更旧事件整体 no-op，revision 不推进")
}

// ===================== #4 初始 SET 参与事件裁决 =====================

// 初始 SET 后，完成时间更晚的延迟旧探测（事件时间早于 SET 写入时刻）必须整体 no-op，
// 不得清除 SET 建立的状态（#4 场景：t2 建立限流，t1.5 的延迟探测不得清除 t2 状态）。
func TestE8InitialSet_DelayedOldProbeNoOp(t *testing.T) {
	repo := newD2ProbeObsRepo()
	svc := &RateLimitService{accountRepo: repo}
	const accountID int64 = 44
	const scope = "deepseek-v4.1-flash:free"

	// 实时请求 t2：初始 SET 建立限流（last_event_at=写入时刻）。
	resetAt := time.Now().Add(48 * time.Hour)
	require.NoError(t, svc.setModelRateLimitWithPreciseReset(context.Background(), accountID, scope, resetAt, true, tokenHarborFreeTierReasonPrefix+":signal"))
	require.Len(t, repo.initialSetCalls, 1, "初始 SET 必须走真实实现路径")
	entry, ok := repo.entryFor(accountID, scope)
	require.True(t, ok, "初始 SET 必须建立限流条目")
	require.Equal(t, true, entry["precise_reset"], "precise_reset 必须持久化")

	meta, _ := repo.metaFor(accountID, scope)
	require.Greater(t, meta.revision, int64(0), "初始 SET 必须写入 meta revision")
	require.True(t, meta.lastEventAt.After(time.Time{}), "初始 SET 必须写入 last_event_at=写入时刻")

	// 完成时间 t1.5 的延迟旧探测（早于 SET 写入时刻）→ 整体 no-op。
	probeEvent := meta.lastEventAt.Add(-500 * time.Millisecond)
	require.NoError(t, svc.ApplyModelRateLimitObservation(context.Background(), accountID, scope, ModelRateLimitObservation{
		EventTime: probeEvent, Outcome: ProbeOutcomeSuccess,
	}))
	entry, ok = repo.entryFor(accountID, scope)
	require.True(t, ok, "延迟旧探测不得清除初始 SET 建立的限流状态")
	require.Equal(t, true, entry["precise_reset"], "延迟旧探测不得改动条目内容")

	// 较新的成功探测（晚于 SET 写入）仍正常清除（恢复语义不变）。
	recovery := meta.lastEventAt.Add(time.Second)
	require.NoError(t, svc.ApplyModelRateLimitObservation(context.Background(), accountID, scope, ModelRateLimitObservation{
		EventTime: recovery, Outcome: ProbeOutcomeSuccess,
	}))
	_, ok = repo.entryFor(accountID, scope)
	require.False(t, ok, "较新的成功探测应正常清除限流状态")
}

// 初始 SET 与并发探测不互覆：初始 SET 持 per-account 写锁，读 meta → 裁决 → 原子写，
// 与 ApplyModelRateLimitObservation 同锁语义（同一账号串行化）。并发写同账号不同 scope
// 各自状态正确落盘、互不覆盖。
func TestE8InitialSet_ConcurrentProbeDoesNotOverwrite(t *testing.T) {
	repo := newD2ProbeObsRepo()
	svc := &RateLimitService{accountRepo: repo}
	const accountID int64 = 45
	const scopeA = "model-a"
	const scopeB = "model-b"

	// 同一账号并发：A 走初始 SET，B 走探测写入口。共享 per-account 锁 → 串行化。
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = svc.setModelRateLimitWithPreciseReset(context.Background(), accountID, scopeA, time.Now().Add(time.Hour), false, tokenHarborFreeTierReasonPrefix+":no-signal")
	}()
	go func() {
		defer wg.Done()
		_ = svc.ApplyModelRateLimitObservation(context.Background(), accountID, scopeB, ModelRateLimitObservation{
			EventTime: time.Now(), Outcome: ProbeOutcomeFreeTier429,
			ResetAt: time.Now().Add(2 * time.Hour), Reason: "r-b",
		})
	}()
	wg.Wait()

	// 两个 scope 的状态各自正确落盘，互不覆盖。
	entryA, okA := repo.entryFor(accountID, scopeA)
	require.True(t, okA, "初始 SET 写入的 scope A 条目必须存在")
	require.Equal(t, false, entryA["precise_reset"], "scope A 的 precise_reset 必须保留")

	entryB, okB := repo.entryFor(accountID, scopeB)
	require.True(t, okB, "探测写入口写入的 scope B 条目必须存在")
	require.Equal(t, "r-b", entryB["reason"], "scope B 的 reason 必须保留")

	// meta 两键各自独立推进（互不覆盖）。
	metaA, okA := repo.metaFor(accountID, scopeA)
	require.True(t, okA)
	metaB, okB := repo.metaFor(accountID, scopeB)
	require.True(t, okB)
	require.Greater(t, metaA.revision, int64(0), "初始 SET 必须推进 scope A meta")
	require.Greater(t, metaB.revision, int64(0), "探测写入必须推进 scope B meta")
}

// ===================== E20 #2 恢复告警关闭纳入同一事务 + 错误传播 =====================

// TestFreshnessAlertResolver_ErrorPropagatesOnRecovery 验证 E20 #2：恢复迁移同一状态变更内
// 关闭告警时，若告警关闭失败，错误必须沿唯一写入口传播（删除原 slog.Warn 吞错分支），
// 由入口层决定是否整体回滚。此处用 in-memory 替身验证「传播」契约本身；真正的事务整体
// 回滚由生产 accountRepository.WithObservationTx 在 ent 事务内保证（见 repo 层 WithObservationTx 测试）。
func TestFreshnessAlertResolver_ErrorPropagatesOnRecovery(t *testing.T) {
	rl := NewRateLimitService(newD2ProbeObsRepo(), nil, nil, nil, nil)
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

	resolveErr := errors.New("simulated alert close failure")
	rl.SetFreshnessAlertResolver(func(_ context.Context, _ int64, _ string, _ bool) error {
		return resolveErr
	})

	// 模型级成功（恢复迁移路径）→ 触发关闭钩子，钩子失败必须沿写入口 return。
	err := rl.ApplyModelRateLimitObservation(context.Background(), 7, "gpt-4", ModelRateLimitObservation{
		EventTime: now,
		Outcome:   ProbeOutcomeSuccess,
	})
	require.ErrorIs(t, err, resolveErr, "告警关闭失败必须沿唯一写入口传播（E20 #2）")
}

// ===================== E30 #2/#4 窄接口缺失失败关闭 + #1 提交后快照同步 =====================

// e30BareAccountRepo 仅嵌入 AccountRepository（nil），不实现任何观测/精确恢复窄面——用于
// 定向测试窄接口缺失时的失败关闭（E30 #2/#4）：不得静默 no-op / 不得回退旧入口。
type e30BareAccountRepo struct {
	AccountRepository
}

// E30 #2：observationRepo() 为 nil（仓库未实现观测窄面）时，Apply 必须明确失败关闭，而非
// 静默 no-op（探测成功照常清冻结但观测根本没写，会把读取/接线故障误判为健康）。
func TestApplyModelRateLimitObservation_NarrowMissingFailClosed(t *testing.T) {
	repo := &e30BareAccountRepo{}
	svc := &RateLimitService{accountRepo: repo}
	err := svc.ApplyModelRateLimitObservation(context.Background(), 42, "S", ModelRateLimitObservation{
		EventTime: time.Now(), Outcome: ProbeOutcomeSuccess,
	})
	require.Error(t, err, "窄接口缺失：Apply 必须失败关闭，不得静默 no-op")
	require.Contains(t, err.Error(), "does not implement model rate limit observation narrow interface")
}

// E30 #2：Get 在观测窄面缺失时必须明确失败关闭，而非返回「无观测健康态」（读取故障不得被
// 误判为健康）。
func TestGetModelRateLimitObservation_NarrowMissingFailClosed(t *testing.T) {
	repo := &e30BareAccountRepo{}
	svc := &RateLimitService{accountRepo: repo}
	_, err := svc.GetModelRateLimitObservation(context.Background(), 42, "S")
	require.Error(t, err, "窄接口缺失：Get 必须失败关闭，不得返回无观测健康态")
	require.Contains(t, err.Error(), "does not implement model rate limit observation narrow interface")
}

// E30 #4：setModelRateLimitWithPreciseReset 在 precise_reset 窄面缺失时必须明确失败关闭，
// 不得回退旧 SetModelRateLimit 入口（回退会跳过 precise_reset 持久化与初始 SET 的 meta
// 事件裁决）。
func TestSetModelRateLimitWithPreciseReset_NarrowMissingFailClosed(t *testing.T) {
	repo := &e30BareAccountRepo{}
	svc := &RateLimitService{accountRepo: repo}
	err := svc.setModelRateLimitWithPreciseReset(context.Background(), 42, "S", time.Now().Add(time.Hour), true, "reason")
	require.Error(t, err, "窄接口缺失：precise_reset 必须失败关闭，不得回退旧入口")
	require.Contains(t, err.Error(), "does not implement precise reset narrow interface")
}

// E30 #1：恢复路径提交成功后必须补做快照同步（事务内不预览未提交状态，故不在提交前同步）；
// 同步失败必须沿写入口返回错误，可观测、不静默。
func TestCommitModelRateLimitRecovery_PostCommitSnapshotSync(t *testing.T) {
	repo := newD2ProbeObsRepo()
	svc := &RateLimitService{accountRepo: repo}

	// 提交成功 → 提交后必须补做快照同步。
	err := svc.commitRecoveryAtomically(context.Background(), 42, "S", false, func(txCtx context.Context) error { return nil })
	require.NoError(t, err, "提交成功不得误报错误")
	require.Equal(t, 1, repo.syncSnapshotCalls, "恢复提交成功后必须调用快照同步")

	// 同步失败不得误判为提交失败（E34）：事务已确认提交、恢复已成功，必须返回 nil，
	// 调用方须走成功路径（清冻结上界、计成功）；同步错误仅可观测（打 Warn 日志），不得改变探测结论。
	repo.syncSnapshotErr = errors.New("simulated snapshot sync failure")
	err = svc.commitRecoveryAtomically(context.Background(), 42, "S", false, func(txCtx context.Context) error { return nil })
	require.NoError(t, err, "快照同步失败不得返回错误（已提交恢复成功，调用方须走成功路径）")
	require.Equal(t, 2, repo.syncSnapshotCalls, "失败路径的同步调用仍需发生（错误在同步内产生，仅可观测）")
}
