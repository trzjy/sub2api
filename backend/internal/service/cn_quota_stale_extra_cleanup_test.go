//go:build unit

package service

// CleanStaleTokenHarborFreeTierExtra 单测（派发单 D-QL-003D）。
//
// 覆盖：幂等（二次执行零写入）、前缀精确性（tokenharbor_free_tier_x 删、
// tokenharbor_account_level_probe 留、无 meta 账号 no-op）、并发安全
//（UpdateExtra 覆盖写语义下重复调用无冲突断言：多次并发清理收敛到同一
// meta 终态且非命中键不被破坏）。

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
	//（GetByID / UpdateExtra），其余方法调用即 panic（同包既有 stub 惯例）。
	AccountRepository
	mu                        sync.Mutex
	accounts                  map[int64]*Account
	updateExtraCalls          int
	updateExtraCallsPerAccount map[int64]int
	// failOn 注入 UpdateExtra 定向失败（键为账号 ID）。
	failOn map[int64]error
	// recordedUpdates 记录每次 UpdateExtra 的 updates 拷贝（并发覆盖写断言用）。
	recordedUpdates []map[string]any
}

func newStaleExtraCleanupRepo(accounts ...*Account) *staleExtraCleanupRepo {
	r := &staleExtraCleanupRepo{
		accounts:                   make(map[int64]*Account),
		updateExtraCallsPerAccount: make(map[int64]int),
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
			extraCopy[k] = v
		}
		cp.Extra = extraCopy
	}
	return &cp, nil
}

func (r *staleExtraCleanupRepo) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err, ok := r.failOn[id]; ok {
		return err
	}
	a, ok := r.accounts[id]
	if !ok {
		return ErrAccountNotFound
	}
	if a.Extra == nil {
		a.Extra = map[string]any{}
	}
	for k, v := range updates {
		a.Extra[k] = v
	}
	r.updateExtraCalls++
	r.updateExtraCallsPerAccount[id]++
	cp := make(map[string]any, len(updates))
	for k, v := range updates {
		cp[k] = v
	}
	r.recordedUpdates = append(r.recordedUpdates, cp)
	return nil
}

func (r *staleExtraCleanupRepo) updateCount(id int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.updateExtraCallsPerAccount[id]
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
	require.Equal(t, 1, repo.updateCount(207))

	meta := account.Extra["model_rate_limits_meta"].(map[string]any)
	require.NotContains(t, meta, "tokenharbor_free_tier_glm-5.3-flash")
	require.NotContains(t, meta, "tokenharbor_free_tier_qwen3.8-flash")
	require.Contains(t, meta, "tokenharbor_account_level_probe")
	require.Contains(t, account.Extra, "balance_probe_snapshot", "extra 其他顶层键不得被动")

	n2, err := CleanStaleTokenHarborFreeTierExtra(context.Background(), repo, []int64{207})
	require.NoError(t, err)
	require.Zero(t, n2, "second run must clean nothing")
	require.Equal(t, 1, repo.updateCount(207), "second run must not write")
}

// 清理后 meta 为空桶 → 显式写空 map（而非缺失/null）。
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

	// 显式空 map 保证二次执行同样 no-op（零写入）。
	n2, err := CleanStaleTokenHarborFreeTierExtra(context.Background(), repo, []int64{208})
	require.NoError(t, err)
	require.Zero(t, n2)
	require.Equal(t, 1, repo.updateCount(208))
}

// ---------- 前缀精确性 ----------

// 精确前缀判定：tokenharbor_free_tier_x 删、tokenharbor_account_level_probe 留。
func TestStaleExtraCleanupPrefixPrecision(t *testing.T) {
	account := newStaleExtraAccount(207, map[string]any{
		"model_rate_limits_meta": map[string]any{
			"tokenharbor_free_tier_glm-5.3-flash":  map[string]any{"revision": int64(1)},
			"tokenharbor_free_tier_x":              map[string]any{"revision": int64(2)},
			"tokenharbor_account_level_probe":      map[string]any{"revision": int64(3)},
			"claude-fable-5":                       map[string]any{"revision": int64(4)},
			"antigravity:gemini":                   map[string]any{"revision": int64(5)},
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
	require.Zero(t, repo.updateExtraCalls, "no-op accounts must not trigger any write")
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
	require.Zero(t, repo.updateExtraCalls)

	_, err = CleanStaleTokenHarborFreeTierExtra(context.Background(), repo, []int64{404})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrAccountNotFound)
}

// 单账号写入失败：错误上抛、已清理账号的结果保持（幂等重跑续跑语义）。
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

// UpdateExtra 覆盖写语义下重复并发调用：各并发者以共享账号为读基线，分别写回
// 各自基于读基线计算的已清理 meta（后写覆盖先写）。断言无冲突：终态收敛、
// 全部写入值逐键一致（幂等覆盖）、非命中键与非 meta 键不被破坏。
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
	// 覆盖写语义下的并发收敛断言：
	//   1) 终态收敛：命中键全删、保留键原值——任一次覆盖写的结果都相同，
	//      后写覆盖先写也不会破坏终态（无冲突、无数据损坏）；
	//   2) 全部写入的 meta 值逐键一致（清理函数是纯前缀过滤，输出只依赖输入，
	//      重复执行产生同一覆盖值 → 幂等）；
	//   3) 每次写入都恰好是「meta 桶单键」更新，绝无其他 extra 键被牵连。
	finalMeta, ok := account.Extra["model_rate_limits_meta"].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, finalMeta, "tokenharbor_free_tier_glm-5.3-flash")
	require.NotContains(t, finalMeta, "tokenharbor_free_tier_qwen3.8-flash")
	require.Contains(t, finalMeta, "tokenharbor_account_level_probe")
	require.Equal(t, map[string]any{
		"last_event_at": "2026-10-05T00:00:00Z", "revision": int64(11),
	}, finalMeta["tokenharbor_account_level_probe"])

	repo.mu.Lock()
	writes := append([]map[string]any(nil), repo.recordedUpdates...)
	repo.mu.Unlock()
	require.NotEmpty(t, writes)
	wantMeta, ok := writes[0]["model_rate_limits_meta"].(map[string]any)
	require.True(t, ok)
	require.Len(t, wantMeta, 1)
	require.Contains(t, wantMeta, "tokenharbor_account_level_probe")
	for _, w := range writes {
		require.Len(t, w, 1, "cleanup must only ever write the meta bucket key")
		gotMeta, ok := w["model_rate_limits_meta"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, wantMeta, gotMeta, "all concurrent overwrite writes must carry the identical cleaned meta")
	}
}
