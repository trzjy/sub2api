//go:build unit

package service

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestManagedExtraKeysUnchanged 锁定受管键清单（外审 must_fix / 用户裁定 2026-11-12）：
// 不得扩充或缩减既有受管键集合。任何对受管键清单的新增/删除都会让本用例失败，迫使评审显式裁定。
func TestManagedExtraKeysUnchanged(t *testing.T) {
	expected := []string{
		LargeRequestOnlyExtraKey,
		SmallRequestOnlyExtraKey,
		IdentityDisguiseExtraKey,
		"quota_used",
		"quota_daily_used",
		"quota_daily_start",
		"quota_weekly_used",
		"quota_weekly_start",
		grokBillingExtraKey,
		UpstreamBillingProbeEnabledExtraKey,
		UpstreamBillingRateSyncEnabledExtraKey,
		UpstreamBillingProbeExtraKey,
		OllamaCloudUsageSessionExtraKey,
		OllamaCloudUsageAutoRefreshExtraKey,
		OllamaCloudUsageSnapshotExtraKey,
		OpenAIAutoResetCreditStateExtraKey,
		OpenCodeGoUsageAutoRefreshExtraKey,
		OpenCodeGoUsageSnapshotExtraKey,
		webConsoleQuotaSnapshotExtraKey,
	}
	require.ElementsMatch(t, expected, managedExtraKeys, "受管键清单必须与既有集合完全一致（不得扩充/缩减）")
	require.Len(t, managedExtraKeys, len(expected), "受管键清单不得有重复项")
}

// managedExtraCurrentRowRepo 是确定性竞态复现桩：第一次 GetByID 返回「请求开始时的旧快照」，
// 第二次及以后返回「专用接口已写入的新值」，模拟「普通编辑读旧快照」与「专用接口写新值」交错后，
// 落库前重读到的数据库当前行。Update 把工作副本落回内存，便于断言最终落库结果。
type managedExtraCurrentRowRepo struct {
	AccountRepository
	mu     sync.Mutex
	calls  int
	stored Account
}

func (r *managedExtraCurrentRowRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	// 第一次读取 = 旧快照；之后的读取 = 专用接口（如 UpdateAccountIdentityDisguise → UpdateExtra）
	// 在两次读取之间写入的新值。
	disguise := "old-disguise"
	if r.calls >= 2 {
		disguise = "new-disguise-by-dedicated"
	}
	acc := r.stored
	acc.ID = id
	acc.Extra = map[string]any{
		IdentityDisguiseExtraKey: disguise,
		"custom":                 "base",
	}
	return &acc, nil
}

func (r *managedExtraCurrentRowRepo) Update(_ context.Context, a *Account) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stored = *a
	return nil
}

// TestIdentityDisguisePreservedFromCurrentRow 复现并验证竞态修复（DSG-G3）：
// 普通编辑读取旧快照（identity_disguise=old）后，专用接口写入了新值（new）；UpdateAccount 在落库事务内
// 重读当前行并回填受管键，最终库里的 identity_disguise 必须是专用接口写入的新值，而非旧快照。
//
// 变异证明：若回退到「从 account.Extra 快照回带受管键」的实现，落库值会是 old-disguise，本用例失败。
func TestIdentityDisguisePreservedFromCurrentRow(t *testing.T) {
	repo := &managedExtraCurrentRowRepo{stored: Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
	}}
	svc := &adminServiceImpl{accountRepo: repo}

	// 普通编辑只改了 non-managed 字段 custom，未触碰 identity_disguise。
	_, err := svc.UpdateAccount(context.Background(), 1, &UpdateAccountInput{
		Extra: map[string]any{"custom": "edited"},
	})
	require.NoError(t, err)

	require.GreaterOrEqual(t, repo.calls, 2, "应至少重读两次（开始时读快照 + 落库前读当前行）")
	// 关键断言：最终落库的 identity_disguise 是专用接口写入的新值，而非旧快照。
	require.Equal(t, "new-disguise-by-dedicated", repo.stored.Extra[IdentityDisguiseExtraKey],
		"受管键必须取自库当前值，不得被旧快照覆盖")
	// non-managed 字段按请求整份替换。
	require.Equal(t, "edited", repo.stored.Extra["custom"])
}

// TestAccountUpdateManagedKeysFromCurrentRow 回归：无并发时，普通编辑仍整体替换 non-managed 键，
// 受管键保持库中当前值（无并发写入时等于快照值）。
func TestAccountUpdateManagedKeysFromCurrentRow(t *testing.T) {
	accountID := int64(505)
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: {
			ID:       accountID,
			Platform: PlatformOpenAI,
			Type:     AccountTypeAPIKey,
			Status:   StatusActive,
			Extra: map[string]any{
				IdentityDisguiseExtraKey: "stored-disguise",
				LargeRequestOnlyExtraKey: true,
				"quota_used":             float64(10),
				"custom":                 "stored-custom",
			},
		},
	}}
	svc := &adminServiceImpl{accountRepo: repo}

	// 只编辑 non-managed 字段 custom。
	_, err := svc.UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Extra: map[string]any{"custom": "edited-custom"},
	})
	require.NoError(t, err)

	got := repo.accounts[accountID].Extra
	// non-managed 键按整份替换生效。
	require.Equal(t, "edited-custom", got["custom"])
	// 受管键保持库中当前值（无并发写入时等于快照值）。
	require.Equal(t, "stored-disguise", got[IdentityDisguiseExtraKey])
	require.Equal(t, true, got[LargeRequestOnlyExtraKey])
	require.Equal(t, float64(10), got["quota_used"])
}

// TestAccountUpdateMergeBackfillsMissingManagedKeys 直接验证 mergeManagedExtraFromCurrentRow：
// 当 account.Extra 尚未持有某受管键时，必须从数据库当前行回填。
func TestAccountUpdateMergeBackfillsMissingManagedKeys(t *testing.T) {
	accountID := int64(607)
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: {
			ID:       accountID,
			Platform: PlatformOpenAI,
			Type:     AccountTypeAPIKey,
			Status:   StatusActive,
			Extra: map[string]any{
				OllamaCloudUsageSessionExtraKey: "kept",
				IdentityDisguiseExtraKey:        "id",
			},
		},
	}}
	svc := &adminServiceImpl{accountRepo: repo}

	account := &Account{ID: accountID, Extra: map[string]any{"custom": "v"}}
	err := svc.mergeManagedExtraFromCurrentRow(context.Background(), account, nil, nil)
	require.NoError(t, err)
	require.Equal(t, "kept", account.Extra[OllamaCloudUsageSessionExtraKey])
	require.Equal(t, "id", account.Extra[IdentityDisguiseExtraKey])
	require.Equal(t, "v", account.Extra["custom"])
}

// TestAccountUpdateMergeRespectsDroppedManagedKeys 直接验证 mergeManagedExtraFromCurrentRow：
// 本次请求主动删除（身份/资格变化或旧快照失效）的受管键，不得从当前行复活。
func TestAccountUpdateMergeRespectsDroppedManagedKeys(t *testing.T) {
	accountID := int64(606)
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: {
			ID:       accountID,
			Platform: PlatformOpenAI,
			Type:     AccountTypeAPIKey,
			Status:   StatusActive,
			Extra: map[string]any{
				OllamaCloudUsageSessionExtraKey: "should-be-dropped",
			},
		},
	}}
	svc := &adminServiceImpl{accountRepo: repo}

	account := &Account{ID: accountID, Extra: map[string]any{"custom": "v"}}
	drop := map[string]struct{}{OllamaCloudUsageSessionExtraKey: {}}
	err := svc.mergeManagedExtraFromCurrentRow(context.Background(), account, drop, nil)
	require.NoError(t, err)
	require.NotContains(t, account.Extra, OllamaCloudUsageSessionExtraKey, "被主动删除的受管键不得从当前行复活")
	require.Equal(t, "v", account.Extra["custom"])
}

// TestAccountUpdateMergeKeepsExplicitManagedKeys 直接验证 mergeManagedExtraFromCurrentRow：
// 请求本身显式设置的受管键（如探针开关）优先，不被当前行覆盖。
func TestAccountUpdateMergeKeepsExplicitManagedKeys(t *testing.T) {
	accountID := int64(608)
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: {
			ID:       accountID,
			Platform: PlatformOpenAI,
			Type:     AccountTypeAPIKey,
			Status:   StatusActive,
			Extra: map[string]any{
				UpstreamBillingProbeEnabledExtraKey: false,
			},
		},
	}}
	svc := &adminServiceImpl{accountRepo: repo}

	// 请求显式把探针开关置为 true；当前库值为 false。merge 不得用库值(false)覆盖请求值(true)。
	account := &Account{ID: accountID, Extra: map[string]any{
		"custom":                            "v",
		UpstreamBillingProbeEnabledExtraKey: true,
	}}
	explicit := map[string]struct{}{UpstreamBillingProbeEnabledExtraKey: {}}
	err := svc.mergeManagedExtraFromCurrentRow(context.Background(), account, nil, explicit)
	require.NoError(t, err)
	require.Equal(t, true, account.Extra[UpstreamBillingProbeEnabledExtraKey], "请求显式设置的受管键优先")
	require.Equal(t, "v", account.Extra["custom"])
}
