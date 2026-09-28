//go:build integration

package repository

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// cleanupCockpitImportAccounts 删除测试写入的账号（按 uid 精确定位，避免误伤）。
func cleanupCockpitImportAccounts(t *testing.T, uids ...string) {
	t.Helper()
	if len(uids) == 0 {
		return
	}
	for _, u := range uids {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM accounts WHERE uid = $1", u)
	}
}

// TestCockpitImportCommitIntegrationDuplicateZeroNew 验证：
//   - 真实部分唯一索引强制 (platform, uid) 唯一；
//   - 重复导入相同载荷 → 零新增（skipped_existing 计数正确，不报错）。
func TestCockpitImportCommitIntegrationDuplicateZeroNew(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewCockpitImportCommitRepository(client)

	payloads := []service.CockpitImportAccountPayload{
		{Platform: "claude", UID: "ci-dup-1", Name: "ci-dup-1", AccountType: "oauth", Credentials: map[string]any{"access_token": "a"}, Extra: map[string]any{}},
		{Platform: "claude", UID: "ci-dup-2", Name: "ci-dup-2", AccountType: "oauth", Credentials: map[string]any{"access_token": "b"}, Extra: map[string]any{}},
	}
	t.Cleanup(func() { cleanupCockpitImportAccounts(t, "ci-dup-1", "ci-dup-2") })

	first, err := repo.CommitCockpitImport(ctx, payloads)
	require.NoError(t, err)
	require.Equal(t, 2, first.Created)
	require.Equal(t, 0, first.SkippedExisting)

	// 重复导入：完全一致的载荷 → 零新增，全部跳过。
	second, err := repo.CommitCockpitImport(ctx, payloads)
	require.NoError(t, err)
	require.Equal(t, 0, second.Created)
	require.Equal(t, 2, second.SkippedExisting)
}

// TestCockpitImportCommitIntegrationNameUniqueFailsClose 验证非目标约束（name 唯一）冲突
// 触发整体回滚 + 显式错误，禁止吞成 skipped_existing（失败关闭）。
func TestCockpitImportCommitIntegrationNameUniqueFailsClose(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewCockpitImportCommitRepository(client)

	// 同批次两条不同 (platform,uid) 但同 name → 命中 name 唯一约束（非部分索引目标）。
	payloads := []service.CockpitImportAccountPayload{
		{Platform: "claude", UID: "ci-name-1", Name: "ci-name-shared", AccountType: "oauth", Credentials: map[string]any{"access_token": "a"}, Extra: map[string]any{}},
		{Platform: "openai", UID: "ci-name-2", Name: "ci-name-shared", AccountType: "oauth", Credentials: map[string]any{"access_token": "b"}, Extra: map[string]any{}},
	}
	t.Cleanup(func() { cleanupCockpitImportAccounts(t, "ci-name-1", "ci-name-2") })

	res, err := repo.CommitCockpitImport(ctx, payloads)
	require.Error(t, err, "非目标 name 唯一约束必须显式失败，不得静默跳过")
	require.Equal(t, 0, res.Created, "约束冲突时不得有任何写入")
	require.Equal(t, 0, res.SkippedExisting, "约束冲突不得计为 skipped_existing")

	var cnt int
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts WHERE uid IN ('ci-name-1','ci-name-2')").Scan(&cnt))
	require.Equal(t, 0, cnt, "约束冲突必须整体回滚，零落库")
}

// TestCockpitImportCommitIntegrationConcurrent 验证并发提交相同/不同键时，
// 部分唯一索引仍保证每个 (platform,uid) 仅落库一次。
func TestCockpitImportCommitIntegrationConcurrent(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewCockpitImportCommitRepository(client)

	const n = 5
	uids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		uids = append(uids, "ci-conc-"+string(rune('a'+i)))
	}
	t.Cleanup(func() { cleanupCockpitImportAccounts(t, uids...) })

	var wg sync.WaitGroup
	results := make([]service.CockpitImportCommitResult, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// 每个 goroutine 提交全部 n 个相同键（竞态点），恰好其中一个创建、其余跳过。
			payloads := make([]service.CockpitImportAccountPayload, 0, n)
			for j := 0; j < n; j++ {
				payloads = append(payloads, service.CockpitImportAccountPayload{
					Platform: "claude", UID: uids[j], Name: uids[j], AccountType: "oauth",
					Credentials: map[string]any{"access_token": "x"}, Extra: map[string]any{},
				})
			}
			results[i], errs[i] = repo.CommitCockpitImport(ctx, payloads)
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		require.NoError(t, errs[i], "并发提交不应产生非预期错误（部分索引冲突被 DO NOTHING 处理）")
	}

	var total int
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts WHERE uid LIKE 'ci-conc-%'").Scan(&total))
	require.Equal(t, n, total, "并发提交后每个 (platform,uid) 恰好落库一次")
}
