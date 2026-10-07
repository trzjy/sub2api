//go:build unit

package service

// CleanStaleTokenHarborFreeTierExtra 单测（派发单 D-QL-003D + D-QL-007 F2 整改）。
//
// 覆盖：幂等（二次执行零写入）、前缀精确性（tokenharbor_free_tier_x 删、
// tokenharbor_account_level_probe 留、无 meta 账号 no-op）、并发安全
//（重复并发清理收敛到同一 meta 终态且非命中键不被破坏）、并发混合写入
//（D-QL-007 F2：清理读与删之间并发写入者对 meta 桶**其他键**的写入必须存活
// ——旧「整桶回写」实现下此用例必失败，锁死回归；新实现走仓库层 jsonb
// 原子按键删除，他键不受影响）。

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// ---------- fake repo ----------

type staleExtraCleanupRepo struct {
	// AccountRepository 内嵌 nil 基线：只实现被测路径实际触碰的方法
	//（GetByID / DeleteModelRateLimitsMetaKeys），其余方法调用即 panic
	//（同包既有 stub 惯例）。
	AccountRepository
	mu                        sync.Mutex
	accounts                  map[int64]*Account
	deleteCalls               int
	deleteCallsPerAccount     map[int64]int
	// failOn 注入 DeleteModelRateLimitsMetaKeys 定向失败（键为账号 ID）。
	failOn map[int64]error
	// recordedDeleteKeys 记录每次删除的 keys 拷贝（删除参数断言用）。
	recordedDeleteKeys [][]string
	// concurrentWrite 模拟并发写入者（CommitModelRateLimitObservation 同型）：
	// 在清理 GetByID 之后、DeleteModelRateLimitsMetaKeys 执行之前对 meta 桶
	// 其他键写入。语义锁点：旧整桶回写实现基于 GetByID 快照组装 meta 再覆盖
	// 写回，必然丢掉该并发键（用例必失败）；jsonb 按键删除只删命中键，键存活。
	concurrentWrite map[string]any
}

func newStaleExtraCleanupRepo(accounts ...*Account) *staleExtraCleanupRepo {
	r := &staleExtraCleanupRepo{
		accounts:              make(map[int64]*Account),
		deleteCallsPerAccount: make(map[int64]int),
	}
	for _, a := range accounts {
		r.accounts[a.ID] = a
	}
	return r
}

func (r *staleExtraCleanupRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.accounts[id]
	if !ok {
		return nil, ErrAccountNotFound
	}
	// 生产语义：每次读返回独立快照（真实 repo 由 ent 反序列化出新对象），
	// 并发读者之间不共享可变状态。
	cp := *a
	if a.Extra != nil {
		extraCopy := make(map[string]any, len(a.Extra))
		for k, v := range a.Extra {
			// 嵌套深拷贝：meta 桶等 map 值必须再拷一层，否则快照与 canonical
			// 状态共享同一 map 本体，并发 delete 会与 range 迭代相撞（生产 ent
			// 反序列化每次生成全新对象，不存在此共享；stub 必须对齐该语义）。
			if m, isMap := v.(map[string]any); isMap {
				inner := make(map[string]any, len(m))
				for ik, iv := range m {
					inner[ik] = iv
				}
				extraCopy[k] = inner
			} else {
				extraCopy[k] = v
			}
		}
		cp.Extra = extraCopy
	}
	return &cp, nil
}

// DeleteModelRateLimitsMetaKeys 模拟仓库层 jsonb 原子按键删除：只删 keys 命中键，
// meta 桶内其他键与 extra 其他顶层键不受影响；并发写入者写入先于删除生效。
func (r *staleExtraCleanupRepo) DeleteModelRateLimitsMetaKeys(_ context.Context, id int64, keys []string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err, ok := r.failOn[id]; ok {
		return 0, err
	}
	a, ok := r.accounts[id]
	if !ok {
		return 0, ErrAccountNotFound
	}
	if a.Extra == nil {
		a.Extra = map[string]any{}
	}
	meta, _ := a.Extra[modelRateLimitsMetaExtraKey].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	// 并发写入者生效（读-删窗口内对其他键的写入）。
	for k, v := range r.concurrentWrite {
		meta[k] = v
	}
	for _, k := range keys {
		delete(meta, k)
	}
	// jsonb_set + COALESCE 语义：即使删空，meta 桶显式保留为空对象。
	a.Extra[modelRateLimitsMetaExtraKey] = meta
	r.deleteCalls++
	r.deleteCallsPerAccount[id]++
	r.recordedDeleteKeys = append(r.recordedDeleteKeys, append([]string(nil), keys...))
	return 1, nil
}

func (r *staleExtraCleanupRepo) deleteCount(id int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deleteCallsPerAccount[id]
}

func (r *staleExtraCleanupRepo) totalDeleteCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deleteCalls
}

func (r *staleExtraCleanupRepo) recordedKeys() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.recordedDeleteKeys))
	for i, keys := range r.recordedDeleteKeys {
		out[i] = append([]string(nil), keys...)
	}
	return out
}

func newStaleExtraAccount(id int64, extra map[string]any) *Account {
	return &Account{
		ID: id, Platform: PlatformOther, Type: AccountTypeAPIKey, Status: StatusActive,
		Credentials: map[string]any{"api_key": "sk-th-test", "base_url": "https://tokenharbor.ai/v1"},
		Extra:       extra,
	}
}

// productionShapedMeta 构造线上 207/208 形状的 meta 桶（生产口径夹具）：
// 混合陈旧免费档键（带模型名 scope 前缀命中）与账号级探测键（保留）。
func productionShapedMeta() map[string]any {
	return map[string]any{
		"tokenharbor_free_tier_glm-5.3-flash": map[string]any{
			"last_event_at": "2026-10-01T00:00:00Z", "revision": int64(3),
		},
		"tokenharbor_free_tier_qwen3.8-flash": map[string]any{
			"last_event_at": "2026-10-02T00:00:00Z", "revision": int64(7),
		},
		"tokenharbor_account_level_probe": map[string]any{
			"last_event_at": "2026-10-05T00:00:00Z", "revision": int64(11),
		},
	}
}

// ---------- 幂等 ----------

// 首次清理删命中键；二次执行零写入（不动库），返回 0。
func TestStaleExtraCleanupIdempotentSecondRunZeroWrites(t *testing.T) {
	account := newStaleExtraAccount(207, map[string]any{
		"model_rate_limits_meta": productionShapedMeta(),
		"balance_probe_snapshot": map[string]any{"keep": true},
	})
	repo := newStaleExtraCleanupRepo(account)

	n1, err := CleanStaleTokenHarborFreeTierExtra(context.Background(), repo, []int64{207})
	require.NoError(t, err)
	require.Equal(t, 1, n1)
	require.Equal(t, 1, repo.deleteCount(207))

	meta := account.Extra["model_rate_limits_meta"].(map[string]any)
	require.NotContains(t, meta, "tokenharbor_free_tier_glm-5.3-flash")
	require.NotContains(t, meta, "tokenharbor_free_tier_qwen3.8-flash")
	require.Contains(t, meta, "tokenharbor_account_level_probe")
	require.Contains(t, account.Extra, "balance_probe_snapshot", "extra 其他顶层键不得被动")

	n2, err := CleanStaleTokenHarborFreeTierExtra(context.Background(), repo, []int64{207})
	require.NoError(t, err)
	require.Zero(t, n2, "second run must clean nothing")
	require.Equal(t, 1, repo.deleteCount(207), "second run must not write")
}

// 清理后 meta 为空桶 → 桶显式保留为空对象（而非缺失/null）。
func TestStaleExtraCleanupAllKeysStaleWritesExplicitEmptyMeta(t *testing.T) {
	account := newStaleExtraAccount(208, map[string]any{
		"model_rate_limits_meta": map[string]any{
			"tokenharbor_free_tier_glm-5.3-flash": map[string]any{"revision": int64(1)},
		},
	})
	repo := newStaleExtraCleanupRepo(account)

	n, err := CleanStaleTokenHarborFreeTierExtra(context.Background(), repo, []int64{208})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	meta, ok := account.Extra["model_rate_limits_meta"].(map[string]any)
	require.True(t, ok, "cleaned-empty meta must be an explicit empty map, not absent/null")
	require.Empty(t, meta)

	// 显式空桶保证二次执行同样 no-op（零写入）。
	n2, err := CleanStaleTokenHarborFreeTierExtra(context.Background(), repo, []int64{208})
	require.NoError(t, err)
	require.Zero(t, n2)
	require.Equal(t, 1, repo.deleteCount(208))
}

// ---------- 前缀精确性 ----------

// 精确前缀判定：tokenharbor_free_tier_x 删、tokenharbor_account_level_probe 留。
func TestStaleExtraCleanupPrefixPrecision(t *testing.T) {
	account := newStaleExtraAccount(207, map[string]any{
		"model_rate_limits_meta": map[string]any{
			"tokenharbor_free_tier_glm-5.3-flash": map[string]any{"revision": int64(1)},
			"tokenharbor_free_tier_x":             map[string]any{"revision": int64(2)},
			"tokenharbor_account_level_probe":     map[string]any{"revision": int64(3)},
			"claude-fable-5":                      map[string]any{"revision": int64(4)},
			"antigravity:gemini":                  map[string]any{"revision": int64(5)},
		},
	})
	repo := newStaleExtraCleanupRepo(account)

	n, err := CleanStaleTokenHarborFreeTierExtra(context.Background(), repo, []int64{207})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	meta := account.Extra["model_rate_limits_meta"].(map[string]any)
	require.Len(t, meta, 3)
	require.NotContains(t, meta, "tokenharbor_free_tier_glm-5.3-flash")
	require.NotContains(t, meta, "tokenharbor_free_tier_x")
	require.Contains(t, meta, "tokenharbor_account_level_probe", "account-level probe key must be preserved")
	require.Contains(t, meta, "claude-fable-5", "unrelated model scope keys must be preserved")
	require.Contains(t, meta, "antigravity:gemini", "unrelated family scope keys must be preserved")
	// 保留键的值原样（不被空/重建破坏）。
	require.Equal(t, map[string]any{"revision": int64(3)},
		meta["tokenharbor_account_level_probe"])
	// 删除参数只含前缀命中键（仓库层按键删除的入参口径）。
	keys := repo.recordedKeys()
	require.Len(t, keys, 1)
	require.ElementsMatch(t, []string{
		"tokenharbor_free_tier_glm-5.3-flash", "tokenharbor_free_tier_x",
	}, keys[0])
}

// 无 meta 桶 / extra 为 nil / meta 非命中键 → 全 no-op（零写入）。
func TestStaleExtraCleanupNoMetaAccountsAreNoOp(t *testing.T) {
	noExtra := newStaleExtraAccount(301, nil)
	emptyExtra := newStaleExtraAccount(302, map[string]any{})
	otherExtraOnly := newStaleExtraAccount(303, map[string]any{
		"th_pass_snapshot": map[string]any{"has_pass": true},
	})
	metaWithoutHits := newStaleExtraAccount(304, map[string]any{
		"model_rate_limits_meta": map[string]any{
			"tokenharbor_account_level_probe": map[string]any{"revision": int64(9)},
		},
	})
	metaWrongType := newStaleExtraAccount(305, map[string]any{
		"model_rate_limits_meta": "not-a-map",
	})
	emptyMeta := newStaleExtraAccount(306, map[string]any{
		"model_rate_limits_meta": map[string]any{},
	})
	repo := newStaleExtraCleanupRepo(noExtra, emptyExtra, otherExtraOnly, metaWithoutHits, metaWrongType, emptyMeta)

	n, err := CleanStaleTokenHarborFreeTierExtra(context.Background(), repo,
		[]int64{301, 302, 303, 304, 305, 306})
	require.NoError(t, err)
	require.Zero(t, n, "accounts without stale keys must be no-op")
	require.Zero(t, repo.totalDeleteCalls(), "no-op accounts must not trigger any write")
	// 状态原样。
	require.Nil(t, noExtra.Extra)
	require.Empty(t, emptyExtra.Extra)
	require.Contains(t, otherExtraOnly.Extra, "th_pass_snapshot")
	require.Contains(t, metaWithoutHits.Extra["model_rate_limits_meta"].(map[string]any),
		"tokenharbor_account_level_probe")
	require.Equal(t, "not-a-map", metaWrongType.Extra["model_rate_limits_meta"])
	require.NotNil(t, emptyMeta.Extra["model_rate_limits_meta"])
}

// 空 ID 列表与不存在账号：空列表零动作；不存在账号报错且中止。
func TestStaleExtraCleanupEmptyListAndMissingAccount(t *testing.T) {
	repo := newStaleExtraCleanupRepo()

	n, err := CleanStaleTokenHarborFreeTierExtra(context.Background(), repo, nil)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Zero(t, repo.totalDeleteCalls())

	_, err = CleanStaleTokenHarborFreeTierExtra(context.Background(), repo, []int64{404})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrAccountNotFound)
}

// 单账号删除失败：错误上抛、已清理账号的结果保持（幂等重跑续跑语义）。
func TestStaleExtraCleanupWriteErrorPropagates(t *testing.T) {
	account := newStaleExtraAccount(207, map[string]any{
		"model_rate_limits_meta": productionShapedMeta(),
	})
	repo := newStaleExtraCleanupRepo(account)

	failures := map[int64]error{207: errors.New("db write failed")}
	repo.failOn = failures

	n, err := CleanStaleTokenHarborFreeTierExtra(context.Background(), repo, []int64{207})
	require.Error(t, err)
	require.EqualError(t, err, "db write failed")
	require.Zero(t, n)
	// 失败时账号数据保持原样（meta 未被写坏）。
	meta := account.Extra["model_rate_limits_meta"].(map[string]any)
	require.Contains(t, meta, "tokenharbor_free_tier_glm-5.3-flash")
}

// ---------- 并发安全 ----------

// 重复并发清理调用：各并发者各自按键删除同一组命中键。断言无冲突：终态收敛、
// 每次删除的键集合完全一致（纯前缀过滤，输出只依赖读到的键集合）、
// 非命中键与非 meta 键不被破坏。
func TestStaleExtraCleanupConcurrentRepeatedCallsConverge(t *testing.T) {
	account := newStaleExtraAccount(207, map[string]any{
		"model_rate_limits_meta": productionShapedMeta(),
	})
	repo := newStaleExtraCleanupRepo(account)

	const concurrency = 8
	var wg sync.WaitGroup
	results := make([]int, concurrency)
	errs := make([]error, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = CleanStaleTokenHarborFreeTierExtra(context.Background(), repo, []int64{207})
		}(i)
	}
	wg.Wait()

	for i := range errs {
		require.NoError(t, errs[i])
	}
	// 终态收敛：命中键全删、保留键原值；extra 其他顶层键不被动。
	finalMeta, ok := account.Extra["model_rate_limits_meta"].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, finalMeta, "tokenharbor_free_tier_glm-5.3-flash")
	require.NotContains(t, finalMeta, "tokenharbor_free_tier_qwen3.8-flash")
	require.Contains(t, finalMeta, "tokenharbor_account_level_probe")
	require.Equal(t, map[string]any{
		"last_event_at": "2026-10-05T00:00:00Z", "revision": int64(11),
	}, finalMeta["tokenharbor_account_level_probe"])

	// 每次删除都只携带同一组前缀命中键（按键删除，绝无整桶回写）。
	keys := repo.recordedKeys()
	require.NotEmpty(t, keys)
	want := []string{"tokenharbor_free_tier_glm-5.3-flash", "tokenharbor_free_tier_qwen3.8-flash"}
	for _, got := range keys {
		require.ElementsMatch(t, want, got, "every delete must target exactly the stale keys")
	}
}

// ---------- 并发混合写入（D-QL-007 F2 回归锁） ----------

// 清理读（GetByID）与删（DeleteModelRateLimitsMetaKeys）之间，并发写入者
// （CommitModelRateLimitObservation 同型）对 meta 桶**其他键**写入。
// 断言该键存活：
//   - 旧实现（读整桶→内存删键→UpdateExtra 整桶回写）基于读快照组装 meta 覆盖
//     写回，必然丢掉该并发键 → 此用例在旧实现下必失败，锁死回归；
//   - 新实现（仓库层 jsonb 按键原子删除）只删命中键，并发键不受影响。
func TestStaleExtraCleanupConcurrentMixedWritePreservesOtherKeys(t *testing.T) {
	account := newStaleExtraAccount(207, map[string]any{
		"model_rate_limits_meta": productionShapedMeta(),
		"balance_probe_snapshot": map[string]any{"keep": true},
	})
	repo := newStaleExtraCleanupRepo(account)
	// 并发写入者：对同一 meta 桶写入一个既非前缀命中、也非读基线中存在的新键。
	repo.concurrentWrite = map[string]any{
		"deepseek:free": map[string]any{
			"last_event_at": "2026-10-06T00:00:00Z", "revision": int64(1),
		},
	}

	n, err := CleanStaleTokenHarborFreeTierExtra(context.Background(), repo, []int64{207})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	meta := account.Extra["model_rate_limits_meta"].(map[string]any)
	// 命中键删除。
	require.NotContains(t, meta, "tokenharbor_free_tier_glm-5.3-flash")
	require.NotContains(t, meta, "tokenharbor_free_tier_qwen3.8-flash")
	// 读基线中的保留键存活。
	require.Contains(t, meta, "tokenharbor_account_level_probe")
	require.Equal(t, map[string]any{
		"last_event_at": "2026-10-05T00:00:00Z", "revision": int64(11),
	}, meta["tokenharbor_account_level_probe"])
	// 读-删窗口内并发写入的其他键存活（核心断言：整桶回写下必丢失）。
	require.Contains(t, meta, "deepseek:free", "concurrent writer's key must survive the cleanup")
	require.Equal(t, map[string]any{
		"last_event_at": "2026-10-06T00:00:00Z", "revision": int64(1),
	}, meta["deepseek:free"])
	// extra 其他顶层键不被动。
	require.Contains(t, account.Extra, "balance_probe_snapshot")
}
