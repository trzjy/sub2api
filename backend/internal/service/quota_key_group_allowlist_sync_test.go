//go:build unit

package service

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// QKG-B4：appendModelsToBoundGroupAllowlists 单测。
//
// 覆盖派发单列出的 9 条用例：
//  1. GroupIDs == nil → 不改任何分组。
//  2. Claude 组（enabled）已有 claude-fable-5-1 + 映射 {claude-sonnet-4-6:..} → 新增
//     claude-sonnet-4-6，既有保留。
//  3. 同一次调用含 gpt-6.1-sol → Claude 组不新增该名；OpenAI 组新增。
//  4. enabled=false 的分组 → 不改。
//  5. 已存在同名（大小写不同）→ 不重复追加。
//  6. deepseek-v4-flash（retired） → 不追加。
//  7. 无法判定家族的键（my-custom-alias） → 不追加。
//  8. composite / cursor 平台分组：判定出家族的模型全部追加。
//  9. 返回的被改分组 ID 正确（各用例以 ElementsMatch 校验 mod）。
// ---------------------------------------------------------------------------

// qkSyncSeq 生成全局唯一激活码凭据，避免唯一约束冲突。
var qkSyncSeq int64

// qkSyncCreateGroup 创建 active 未软删分组并设置平台与模型白名单，返回其 ID。
func qkSyncCreateGroup(t *testing.T, client *ent.Client, name, platform string, al domain.GroupModelAllowlist) int64 {
	t.Helper()
	ctx := context.Background()
	g, err := client.Group.Create().
		SetName(name).
		SetStatus(domain.StatusActive).
		SetPlatform(platform).
		SetModelAllowlist(al).
		Save(ctx)
	require.NoError(t, err)
	return g.ID
}

// qkSyncCreateQuotaKey 创建激活码并绑定指定分组（group_ids）。
func qkSyncCreateQuotaKey(t *testing.T, client *ent.Client, groupIDs []int64) int64 {
	t.Helper()
	ctx := context.Background()
	seq := atomic.AddInt64(&qkSyncSeq, 1)
	q := client.QuotaKey.Create().
		SetCodeEncrypted(fmt.Sprintf("enc-%d", seq)).
		SetCodeMAC(fmt.Sprintf("mac-%d", seq))
	if len(groupIDs) > 0 {
		q = q.SetGroupIds(groupIDs)
	}
	qk, err := q.Save(ctx)
	require.NoError(t, err)
	return qk.ID
}

// qkSyncRun 在调用方事务内执行 appendModelsToBoundGroupAllowlists 并提交，返回被改分组 ID。
func qkSyncRun(t *testing.T, svc *QuotaKeyService, client *ent.Client, qkID int64, mapping map[string]string) ([]int64, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	mod, runErr := svc.appendModelsToBoundGroupAllowlists(ctx, tx, qkID, mapping)
	if runErr != nil {
		_ = tx.Rollback()
		return mod, runErr
	}
	require.NoError(t, tx.Commit())
	return mod, nil
}

// qkSyncAllowlist 读取分组落库后的白名单（提交后从 client 直读）。
func qkSyncAllowlist(t *testing.T, client *ent.Client, id int64) domain.GroupModelAllowlist {
	t.Helper()
	ctx := context.Background()
	g, err := client.Group.Get(ctx, id)
	require.NoError(t, err)
	return g.ModelAllowlist
}

func TestAppendModelsToBoundGroupAllowlists(t *testing.T) {
	svc, client := qkNewTestService(t, &fakeUpstreamClient{})
	ctx := context.Background()

	t.Run("nil group_ids changes nothing", func(t *testing.T) {
		gID := qkSyncCreateGroup(t, client, "sync-nil-g", PlatformAnthropic, domain.GroupModelAllowlist{
			Enabled: true, Models: []string{"claude-fable-5-1"},
		})
		qkID := qkSyncCreateQuotaKey(t, client, nil) // 未绑定任何分组
		t.Cleanup(func() {
			_ = client.QuotaKey.DeleteOneID(qkID).Exec(ctx)
			_ = client.Group.DeleteOneID(gID).Exec(ctx)
		})

		mod, err := qkSyncRun(t, svc, client, qkID, map[string]string{"claude-sonnet-4-6": "claude-sonnet-5-5"})
		require.NoError(t, err)
		require.Empty(t, mod) // 用例 1
		al := qkSyncAllowlist(t, client, gID)
		require.Equal(t, []string{"claude-fable-5-1"}, al.Models)
	})

	t.Run("claude group appends new source model keeping existing", func(t *testing.T) {
		gID := qkSyncCreateGroup(t, client, "sync-claude-g", PlatformAnthropic, domain.GroupModelAllowlist{
			Enabled: true, Models: []string{"claude-fable-5-1"},
		})
		qkID := qkSyncCreateQuotaKey(t, client, []int64{gID})
		t.Cleanup(func() {
			_ = client.QuotaKey.DeleteOneID(qkID).Exec(ctx)
			_ = client.Group.DeleteOneID(gID).Exec(ctx)
		})

		mod, err := qkSyncRun(t, svc, client, qkID, map[string]string{"claude-sonnet-4-6": "claude-sonnet-5-5"})
		require.NoError(t, err)
		require.ElementsMatch(t, []int64{gID}, mod) // 用例 9
		al := qkSyncAllowlist(t, client, gID)
		require.Equal(t, []string{"claude-fable-5-1", "claude-sonnet-4-6"}, al.Models) // 用例 2
	})

	t.Run("family mismatch routes to correct platform group", func(t *testing.T) {
		claudeID := qkSyncCreateGroup(t, client, "sync-claude3", PlatformAnthropic, domain.GroupModelAllowlist{
			Enabled: true, Models: []string{"claude-fable-5-1"},
		})
		openaiID := qkSyncCreateGroup(t, client, "sync-openai3", PlatformOpenAI, domain.GroupModelAllowlist{
			Enabled: true, Models: []string{"gpt-4o"},
		})
		qkID := qkSyncCreateQuotaKey(t, client, []int64{claudeID, openaiID})
		t.Cleanup(func() {
			_ = client.QuotaKey.DeleteOneID(qkID).Exec(ctx)
			_ = client.Group.DeleteOneID(claudeID).Exec(ctx)
			_ = client.Group.DeleteOneID(openaiID).Exec(ctx)
		})

		mod, err := qkSyncRun(t, svc, client, qkID, map[string]string{
			"claude-sonnet-4-6": "x",
			"gpt-6.1-sol":       "y",
		})
		require.NoError(t, err)
		require.ElementsMatch(t, []int64{claudeID, openaiID}, mod) // 用例 9
		claudeAL := qkSyncAllowlist(t, client, claudeID)
		require.Equal(t, []string{"claude-fable-5-1", "claude-sonnet-4-6"}, claudeAL.Models)
		require.NotContains(t, claudeAL.Models, "gpt-6.1-sol") // 用例 3
		openaiAL := qkSyncAllowlist(t, client, openaiID)
		require.Equal(t, []string{"gpt-4o", "gpt-6.1-sol"}, openaiAL.Models)
		require.NotContains(t, openaiAL.Models, "claude-sonnet-4-6") // 用例 3
	})

	t.Run("disabled allowlist group unchanged", func(t *testing.T) {
		gID := qkSyncCreateGroup(t, client, "sync-disabled", PlatformAnthropic, domain.GroupModelAllowlist{
			Enabled: false, Models: []string{"claude-fable-5-1"},
		})
		qkID := qkSyncCreateQuotaKey(t, client, []int64{gID})
		t.Cleanup(func() {
			_ = client.QuotaKey.DeleteOneID(qkID).Exec(ctx)
			_ = client.Group.DeleteOneID(gID).Exec(ctx)
		})

		mod, err := qkSyncRun(t, svc, client, qkID, map[string]string{"claude-sonnet-4-6": "x"})
		require.NoError(t, err)
		require.Empty(t, mod) // 用例 4
		al := qkSyncAllowlist(t, client, gID)
		require.False(t, al.Enabled)
		require.Equal(t, []string{"claude-fable-5-1"}, al.Models)
	})

	t.Run("existing same name different case not duplicated", func(t *testing.T) {
		gID := qkSyncCreateGroup(t, client, "sync-case", PlatformAnthropic, domain.GroupModelAllowlist{
			Enabled: true, Models: []string{"Claude-Sonnet-4-6"},
		})
		qkID := qkSyncCreateQuotaKey(t, client, []int64{gID})
		t.Cleanup(func() {
			_ = client.QuotaKey.DeleteOneID(qkID).Exec(ctx)
			_ = client.Group.DeleteOneID(gID).Exec(ctx)
		})

		mod, err := qkSyncRun(t, svc, client, qkID, map[string]string{"claude-sonnet-4-6": "x"})
		require.NoError(t, err)
		require.Empty(t, mod) // 用例 5：已命中（大小写族）→ 不重复追加
		al := qkSyncAllowlist(t, client, gID)
		require.Equal(t, []string{"Claude-Sonnet-4-6"}, al.Models)
	})

	t.Run("retired model name not appended", func(t *testing.T) {
		gID := qkSyncCreateGroup(t, client, "sync-retired", PlatformOpenAI, domain.GroupModelAllowlist{
			Enabled: true, Models: []string{},
		})
		qkID := qkSyncCreateQuotaKey(t, client, []int64{gID})
		t.Cleanup(func() {
			_ = client.QuotaKey.DeleteOneID(qkID).Exec(ctx)
			_ = client.Group.DeleteOneID(gID).Exec(ctx)
		})

		mod, err := qkSyncRun(t, svc, client, qkID, map[string]string{
			"deepseek-v4-flash": "x", // retired，应剔除
			"gpt-6.1-sol":       "y",
		})
		require.NoError(t, err)
		require.ElementsMatch(t, []int64{gID}, mod)
		al := qkSyncAllowlist(t, client, gID)
		require.Equal(t, []string{"gpt-6.1-sol"}, al.Models) // 用例 6
		require.NotContains(t, al.Models, "deepseek-v4-flash")
	})

	t.Run("undetectable family not appended", func(t *testing.T) {
		gID := qkSyncCreateGroup(t, client, "sync-alias", PlatformAnthropic, domain.GroupModelAllowlist{
			Enabled: true, Models: []string{"claude-fable-5-1"},
		})
		qkID := qkSyncCreateQuotaKey(t, client, []int64{gID})
		t.Cleanup(func() {
			_ = client.QuotaKey.DeleteOneID(qkID).Exec(ctx)
			_ = client.Group.DeleteOneID(gID).Exec(ctx)
		})

		mod, err := qkSyncRun(t, svc, client, qkID, map[string]string{
			"my-custom-alias":   "x", // 判不出家族，应剔除
			"claude-sonnet-4-6": "y",
		})
		require.NoError(t, err)
		require.ElementsMatch(t, []int64{gID}, mod)
		al := qkSyncAllowlist(t, client, gID)
		require.Equal(t, []string{"claude-fable-5-1", "claude-sonnet-4-6"}, al.Models) // 用例 7
		require.NotContains(t, al.Models, "my-custom-alias")
	})

	t.Run("composite and cursor groups append all detected families", func(t *testing.T) {
		compositeID := qkSyncCreateGroup(t, client, "sync-composite", PlatformComposite, domain.GroupModelAllowlist{
			Enabled: true, Models: []string{},
		})
		cursorID := qkSyncCreateGroup(t, client, "sync-cursor", PlatformCursor, domain.GroupModelAllowlist{
			Enabled: true, Models: []string{},
		})
		qkID := qkSyncCreateQuotaKey(t, client, []int64{compositeID, cursorID})
		t.Cleanup(func() {
			_ = client.QuotaKey.DeleteOneID(qkID).Exec(ctx)
			_ = client.Group.DeleteOneID(compositeID).Exec(ctx)
			_ = client.Group.DeleteOneID(cursorID).Exec(ctx)
		})

		mod, err := qkSyncRun(t, svc, client, qkID, map[string]string{
			"claude-sonnet-4-6": "a",
			"gpt-6.1-sol":       "b",
			"gemini-2.5-pro":    "c",
		})
		require.NoError(t, err)
		require.ElementsMatch(t, []int64{compositeID, cursorID}, mod) // 用例 8 + 9
		want := []string{"claude-sonnet-4-6", "gpt-6.1-sol", "gemini-2.5-pro"}
		compositeAL := qkSyncAllowlist(t, client, compositeID)
		require.ElementsMatch(t, want, compositeAL.Models)
		cursorAL := qkSyncAllowlist(t, client, cursorID)
		require.ElementsMatch(t, want, cursorAL.Models)
	})
}

// ---------------------------------------------------------------------------
// QKG-FIX-4：并发追加写入串行化（无丢更新）。
//
// 两个 goroutine 各自在**独立事务**里、对同一分组（同一 qk.GroupIds 指向它）调用
// appendModelsToBoundGroupAllowlists 追加**不同**模型名，两笔都提交成功；
// 最终该分组 model_allowlist.models 必须同时包含两个模型名。
//
// 用真实隔离 PostgreSQL（quotaKeyTestEntClient，QUOTA_TEST_DSN 覆盖），不走假仓储。
// 多轮（rounds）重复以放大读到同一基线、交叉提交的概率，稳定复现「后提交者覆盖」的丢更新。
// 分组用 composite 平台：两条映射（claude-sonnet-4-6 / gpt-6.1-sol）均被判定出家族，都会追加。
// ---------------------------------------------------------------------------
func TestAppendModelsToBoundGroupAllowlists_ConcurrentNoLostUpdate(t *testing.T) {
	svc, client := qkNewTestService(t, &fakeUpstreamClient{})
	ctx := context.Background()

	const rounds = 50
	for r := 0; r < rounds; r++ {
		gID := qkSyncCreateGroup(t, client, fmt.Sprintf("sync-conc-g-%d", r), PlatformComposite, domain.GroupModelAllowlist{
			Enabled: true, Models: []string{},
		})
		// 两个激活码都绑定同一分组。
		qkA := qkSyncCreateQuotaKey(t, client, []int64{gID})
		qkB := qkSyncCreateQuotaKey(t, client, []int64{gID})
		t.Cleanup(func() {
			_ = client.QuotaKey.DeleteOneID(qkA).Exec(ctx)
			_ = client.QuotaKey.DeleteOneID(qkB).Exec(ctx)
			_ = client.Group.DeleteOneID(gID).Exec(ctx)
		})

		var wg sync.WaitGroup
		wg.Add(2)
		errCh := make(chan error, 2)
		go func(qkID int64, mapping map[string]string) {
			defer wg.Done()
			tx, e := client.Tx(ctx)
			if e != nil {
				errCh <- e
				return
			}
			if _, runErr := svc.appendModelsToBoundGroupAllowlists(ctx, tx, qkID, mapping); runErr != nil {
				_ = tx.Rollback()
				errCh <- runErr
				return
			}
			errCh <- tx.Commit()
		}(qkA, map[string]string{"claude-sonnet-4-6": "x"})
		go func(qkID int64, mapping map[string]string) {
			defer wg.Done()
			tx, e := client.Tx(ctx)
			if e != nil {
				errCh <- e
				return
			}
			if _, runErr := svc.appendModelsToBoundGroupAllowlists(ctx, tx, qkID, mapping); runErr != nil {
				_ = tx.Rollback()
				errCh <- runErr
				return
			}
			errCh <- tx.Commit()
		}(qkB, map[string]string{"gpt-6.1-sol": "y"})

		wg.Wait()
		close(errCh)
		for e := range errCh {
			require.NoError(t, e, "round %d: 并发追加应全部提交成功", r)
		}

		al := qkSyncAllowlist(t, client, gID)
		require.ElementsMatch(t, []string{"claude-sonnet-4-6", "gpt-6.1-sol"}, al.Models,
			"round %d: 并发追加不可丢更新（两模型必须同时存在于分组白名单）", r)
	}
}
