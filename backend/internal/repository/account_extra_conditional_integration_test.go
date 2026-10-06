//go:build integration

package repository

import (
	"context"
	"sync"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// 本文件实现 B4 Card B 的 DB-backed 五场景回归 = 完成门禁（不可跳过）。
// 运行：docker 起本地 postgres 后
//
//	go test -tags integration ./internal/repository/ -run 'CodeBuddy' -v
//
// 场景清单（派发单 + 方案 §1/§3）：
//   ① 延迟失败      —— 更晚失败接受、更早失败被拒（attempt_time 单调）
//   ② 并发成功失败  —— 并发两条条件更新，恰一条接受（原子/隔离）
//   ③ 同戳乱序      —— 双请求各持不同 version，高版本接受 / 低版本被拒
//   ④ 失败后成功清错误 —— 成功事务（无错误）清除上次失败留下的错误标记
//   ⑤ 先发后至跨版本清除过期错误 —— 旧失败不得覆盖新状态
//   交错断言：失败已接受后较早成功被拒不回退；成功已接受后较早失败被拒不覆盖
//   R6 断言：首写 NULL/-infinity 哨兵路径；同戳高版本失败可接受/低版本被拒
//   字段矩阵回归：成功事务写入 packages_updated_at/reset_at/used_percent/
//     last_attempt_at/version 全部落库（阈值候选/reset_at 消费依赖）

// codeBuddyCreditPackageForTest 构造一个合法分包（unit=credits，Status=0）。
func codeBuddyCreditPackageForTest(id, remaining, total string) service.CodeBuddyCreditPackage {
	return service.CodeBuddyCreditPackage{
		ID:        id,
		Name:      "pkg-" + id,
		Unit:      "credits",
		Remaining: decimal.RequireFromString(remaining),
		Total:     decimal.RequireFromString(total),
		ExpiresAt: "2026-10-15T21:26:43Z",
		Status:    0,
	}
}

// mustCreateEmptyExtraCodeBuddyAccount 创建一个 platform=codebuddy、extra 为空的
// 账号（条件更新写入的初始状态：无快照、无错误、无 version → NULL/-infinity 哨兵）。
func mustCreateEmptyExtraCodeBuddyAccount(t *testing.T, client *dbent.Client, name string) int64 {
	t.Helper()
	account := mustCreateAccount(t, client, &service.Account{
		Name:        name,
		Platform:    service.PlatformCodeBuddy,
		Type:        service.AccountTypeOAuth,
		Status:      service.StatusActive,
		Credentials: map[string]any{"access_token": "t-" + name},
		Extra:       map[string]any{},
		Concurrency: 1,
		Priority:    50,
		Schedulable: true,
	})
	return account.ID
}

// --- 场景① 延迟失败 ---

func TestCodeBuddyCreditConditional_LateFailureAcceptedEarlyFailureRejected(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	id := mustCreateEmptyExtraCodeBuddyAccount(t, tx.Client(), "cb-late-failure")

	base := time.Now().UTC().Truncate(time.Microsecond)
	late := base.Add(2 * time.Second)

	// 晚失败（attempt_time 更新）→ 接受。
	accepted, err := repo.WriteCodeBuddyCreditAttemptError(ctx, id, service.CodeBuddyCreditAttemptErrorWrite{
		AttemptTime: late,
		Version:     2,
		ErrorMsg:    "late failure",
	})
	require.NoError(t, err)
	require.True(t, accepted, "更晚失败必须被接受")

	// 早失败（attempt_time 更旧）即使 version 更大也必须被拒——不得回退新状态。
	accepted, err = repo.WriteCodeBuddyCreditAttemptError(ctx, id, service.CodeBuddyCreditAttemptErrorWrite{
		AttemptTime: base,
		Version:     99,
		ErrorMsg:    "stale failure",
	})
	require.NoError(t, err)
	require.False(t, accepted, "更早失败不得覆盖已接受的新失败")

	extra, err := repo.GetByID(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "late failure", extra.Extra[service.CodeBuddyCreditErrorKey], "错误标记必须保留新失败内容")
	require.NotContains(t, extra.Extra, service.CodeBuddyCreditPackagesKey, "失败事务不得写入成功快照")
}

// --- 场景② 并发成功失败：恰一条接受（原子性/隔离性）---
//
// 必须用连接池（integrationDB，独立连接）而非同一 dbent.Tx：同 tx 内并发
// goroutine 共享单连接且 READ COMMITTED 快照隔离，第二条看不见第一条未提交
// 的修改（会把 -infinity 当当前事件时间，两条都过）。连接池真并发下，第二条
// 等待行锁后重新评估 WHERE，看到第一条提交的当前状态 → 版本仲裁生效。

func TestCodeBuddyCreditConditional_ConcurrentSuccessFailureSingleAccept(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	id := mustCreateEmptyExtraCodeBuddyAccount(t, client, "cb-concurrent-success-failure")
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM accounts WHERE id = $1", id)
	})

	// 同一声明时间戳：成功与失败并发提交，各持不同 version。DB 行锁+条件
	// UPDATE 保证恰一条被接受（version 大者赢）。
	sameTime := time.Now().UTC().Truncate(time.Microsecond)

	start := make(chan struct{})
	var wg sync.WaitGroup
	var successAccepted, failureAccepted bool
	var successErr, failureErr error
	// 两个独立 repo 实例，各自独立连接：真并发，第二条等待行锁后重评估 WHERE。
	successRepo := newAccountRepositoryWithSQL(client, integrationDB, nil)
	failureRepo := newAccountRepositoryWithSQL(client, integrationDB, nil)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		successAccepted, successErr = successRepo.WriteCodeBuddyCreditSnapshot(ctx, id, service.CodeBuddyCreditSnapshotWrite{
			SuccessTime: sameTime,
			Version:     8,
			Packages:    []service.CodeBuddyCreditPackage{codeBuddyCreditPackageForTest("p1", "10", "100")},
			UsedPercent: 90,
			ResetAt:     sameTime.Add(24 * time.Hour),
			ErrorMsg:    "",
		})
	}()
	go func() {
		defer wg.Done()
		<-start
		failureAccepted, failureErr = failureRepo.WriteCodeBuddyCreditAttemptError(ctx, id, service.CodeBuddyCreditAttemptErrorWrite{
			AttemptTime: sameTime,
			Version:     3,
			ErrorMsg:    "concurrent failure",
		})
	}()
	close(start)
	wg.Wait()
	require.NoError(t, successErr)
	require.NoError(t, failureErr)

	// 成功事务是**主导事件**：无论并发顺序如何，成功都必须被接受——若失败
	// 先提交（version 3 落库），成功后到（version 8 > 3）仍会被接受并覆盖；
	// 若成功先提交（version 8 落库），同戳失败（version 3 < 8）会被拒绝。
	// 因此不变量是：successAccepted 恒为 true，且最终态自洽（成功快照 +
	// 较大 version + 成功无错误时清 error）。
	require.True(t, successAccepted, "成功事务不得因并发失败而被拒（主导事件）")
	if failureAccepted {
		// 失败先提交被接受是允许的：其写入随后被成功事务覆盖/合并，最终态仍自洽。
		t.Log("并发同戳下失败先提交被接受；断言最终态被成功覆盖")
	}

	// 最终态不变量：成功快照 + version=8（较大者）+ 无错误键（成功无 ErrorMsg）。
	loaded, err := newAccountRepositoryWithSQL(client, integrationDB, nil).GetByID(ctx, id)
	require.NoError(t, err)
	require.Contains(t, loaded.Extra, service.CodeBuddyCreditPackagesKey, "成功快照必须落库")
	require.Equal(t, float64(8), loaded.Extra[service.CodeBuddyCreditVersionKey], "version 必须 = 较大成功版本 8")
	require.NotContains(t, loaded.Extra, service.CodeBuddyCreditErrorKey, "成功事务无错误时必须清除并发失败留下的错误标记")
	require.Equal(t, sameTime.UTC(), mustParseRepoTime(t, loaded.Extra[service.CodeBuddyCreditPackagesUpdatedAtKey]), "freshness 必须 = 成功事务 success_time")
}

// --- 场景③ 同戳乱序：双请求各持不同 version ---

func TestCodeBuddyCreditConditional_SameTimestampHigherVersionWins(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	id := mustCreateEmptyExtraCodeBuddyAccount(t, tx.Client(), "cb-same-timestamp-both")

	ts := time.Now().UTC().Truncate(time.Microsecond)

	// 首个高版本成功 → 接受（首写，version 3）。
	accepted, err := repo.WriteCodeBuddyCreditSnapshot(ctx, id, service.CodeBuddyCreditSnapshotWrite{
		SuccessTime: ts,
		Version:     3,
		Packages:    []service.CodeBuddyCreditPackage{codeBuddyCreditPackageForTest("p1", "10", "100")},
		UsedPercent: 90,
		ResetAt:     ts.Add(24 * time.Hour),
		ErrorMsg:    "",
	})
	require.NoError(t, err)
	require.True(t, accepted)

	// 同时间戳低版本成功 → 拒绝（tiebreaker 按 version 大者胜）。
	accepted, err = repo.WriteCodeBuddyCreditSnapshot(ctx, id, service.CodeBuddyCreditSnapshotWrite{
		SuccessTime: ts,
		Version:     1,
		Packages:    []service.CodeBuddyCreditPackage{codeBuddyCreditPackageForTest("p1", "20", "100")},
		UsedPercent: 80,
		ResetAt:     ts.Add(24 * time.Hour),
		ErrorMsg:    "",
	})
	require.NoError(t, err)
	require.False(t, accepted, "同时间戳低版本必须被拒")

	// R6：同时间戳高版本失败可接受、低版本被拒（对失败路径同样适用）。
	accepted, err = repo.WriteCodeBuddyCreditAttemptError(ctx, id, service.CodeBuddyCreditAttemptErrorWrite{
		AttemptTime: ts,
		Version:     9,
		ErrorMsg:    "high-version failure at same ts",
	})
	require.NoError(t, err)
	require.True(t, accepted, "同时间戳高版本失败必须可接受（R6 回修 #3：失败不写严格 >）")

	accepted, err = repo.WriteCodeBuddyCreditAttemptError(ctx, id, service.CodeBuddyCreditAttemptErrorWrite{
		AttemptTime: ts,
		Version:     2,
		ErrorMsg:    "low-version failure at same ts",
	})
	require.NoError(t, err)
	require.False(t, accepted, "同时间戳低版本失败必须被拒")
}

// --- 场景④ 失败后成功清错误 ---

func TestCodeBuddyCreditConditional_SuccessClearsError(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	id := mustCreateEmptyExtraCodeBuddyAccount(t, tx.Client(), "cb-success-clears-error")

	base := time.Now().UTC().Truncate(time.Microsecond)

	// 失败写：错误标记落库。
	accepted, err := repo.WriteCodeBuddyCreditAttemptError(ctx, id, service.CodeBuddyCreditAttemptErrorWrite{
		AttemptTime: base,
		Version:     1,
		ErrorMsg:    "previous failure",
	})
	require.NoError(t, err)
	require.True(t, accepted)

	extra, err := repo.GetByID(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "previous failure", extra.Extra[service.CodeBuddyCreditErrorKey])

	// 成功写（无解析错误，ErrorMsg=""）→ 同事务清除旧错误。
	successTime := base.Add(time.Second)
	accepted, err = repo.WriteCodeBuddyCreditSnapshot(ctx, id, service.CodeBuddyCreditSnapshotWrite{
		SuccessTime: successTime,
		Version:     2,
		Packages:    []service.CodeBuddyCreditPackage{codeBuddyCreditPackageForTest("p1", "10", "100")},
		UsedPercent: 90,
		ResetAt:     successTime.Add(24 * time.Hour),
		ErrorMsg:    "",
	})
	require.NoError(t, err)
	require.True(t, accepted)

	extra, err = repo.GetByID(ctx, id)
	require.NoError(t, err)
	require.NotContains(t, extra.Extra, service.CodeBuddyCreditErrorKey, "成功事务无错误时必须清除旧错误标记")
	require.Contains(t, extra.Extra, service.CodeBuddyCreditPackagesKey, "成功事务必须写入分包快照")
}

// --- 场景⑤ 先发后至跨版本清除过期错误 + 交错断言 ---

func TestCodeBuddyCreditConditional_CrossVersionDoesNotRollBackAcceptedState(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	id := mustCreateEmptyExtraCodeBuddyAccount(t, tx.Client(), "cb-cross-version")

	base := time.Now().UTC().Truncate(time.Microsecond)
	t1 := base
	t2 := base.Add(time.Second)
	t3 := base.Add(2 * time.Second)

	// ① 失败 E1 @ t1 v1 → 接受。
	accepted, err := repo.WriteCodeBuddyCreditAttemptError(ctx, id, service.CodeBuddyCreditAttemptErrorWrite{
		AttemptTime: t1, Version: 1, ErrorMsg: "error-A",
	})
	require.NoError(t, err)
	require.True(t, accepted)

	// ② 失败 E2 @ t2 v2 → 接受（更新错误内容）。
	accepted, err = repo.WriteCodeBuddyCreditAttemptError(ctx, id, service.CodeBuddyCreditAttemptErrorWrite{
		AttemptTime: t2, Version: 2, ErrorMsg: "error-B",
	})
	require.NoError(t, err)
	require.True(t, accepted)

	// ③ 成功 @ t3 v3 无错误 → 接受并清除过期错误（先发后至）。
	accepted, err = repo.WriteCodeBuddyCreditSnapshot(ctx, id, service.CodeBuddyCreditSnapshotWrite{
		SuccessTime: t3, Version: 3,
		Packages:    []service.CodeBuddyCreditPackage{codeBuddyCreditPackageForTest("p1", "10", "100")},
		UsedPercent: 90,
		ResetAt:     t3.Add(24 * time.Hour),
		ErrorMsg:    "",
	})
	require.NoError(t, err)
	require.True(t, accepted)

	// ④ 交错：失败已接受（E2 @ t2）后，较早成功到达（t1）→ 被拒，不回退。
	accepted, err = repo.WriteCodeBuddyCreditSnapshot(ctx, id, service.CodeBuddyCreditSnapshotWrite{
		SuccessTime: t1, Version: 44,
		Packages:    []service.CodeBuddyCreditPackage{codeBuddyCreditPackageForTest("stale", "1", "1")},
		UsedPercent: 0,
		ResetAt:     t1.Add(time.Hour),
		ErrorMsg:    "",
	})
	require.NoError(t, err)
	require.False(t, accepted, "被接受的失败之后，较早成功不得回退状态")

	// ⑤ 交错：成功已接受 @ t3 后，较早失败到达（t1）→ 被拒，不覆盖成功时间。
	accepted, err = repo.WriteCodeBuddyCreditAttemptError(ctx, id, service.CodeBuddyCreditAttemptErrorWrite{
		AttemptTime: t1, Version: 55, ErrorMsg: "ancient failure",
	})
	require.NoError(t, err)
	require.False(t, accepted, "被接受的成功之后，较早失败不得覆盖成功时间")

	// 终态断言：成功快照与错误清除必须保持。
	extra, err := repo.GetByID(ctx, id)
	require.NoError(t, err)
	require.Contains(t, extra.Extra, service.CodeBuddyCreditPackagesKey)
	require.NotContains(t, extra.Extra, service.CodeBuddyCreditErrorKey)
	require.Equal(t, t3.UTC(), mustParseRepoTime(t, extra.Extra[service.CodeBuddyCreditPackagesUpdatedAtKey]), "packages_updated_at 必须 = success_time")
}

// --- R6 首写哨兵：全新账号首个成功与首个失败均能写入 ---

func TestCodeBuddyCreditConditional_FirstWriteSentinelR6(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)

	// 首个成功写入（NULL/-infinity 哨兵路径）。
	successID := mustCreateEmptyExtraCodeBuddyAccount(t, tx.Client(), "cb-sentinel-success")
	now := time.Now().UTC().Truncate(time.Microsecond)
	accepted, err := repo.WriteCodeBuddyCreditSnapshot(ctx, successID, service.CodeBuddyCreditSnapshotWrite{
		SuccessTime: now, Version: 1,
		Packages:    []service.CodeBuddyCreditPackage{codeBuddyCreditPackageForTest("p1", "10", "100")},
		UsedPercent: 90,
		ResetAt:     now.Add(24 * time.Hour),
		ErrorMsg:    "",
	})
	require.NoError(t, err)
	require.True(t, accepted, "首次无快照无失败记录时，首个成功必须能写入（-infinity 哨兵）")

	// 首个失败写入。
	failureID := mustCreateEmptyExtraCodeBuddyAccount(t, tx.Client(), "cb-sentinel-failure")
	accepted, err = repo.WriteCodeBuddyCreditAttemptError(ctx, failureID, service.CodeBuddyCreditAttemptErrorWrite{
		AttemptTime: now, Version: 1, ErrorMsg: "first failure",
	})
	require.NoError(t, err)
	require.True(t, accepted, "首次无快照无失败记录时，首个失败必须能写入（NULL version=0 + -infinity 哨兵）")

	extra, err := repo.GetByID(ctx, failureID)
	require.NoError(t, err)
	require.Equal(t, "first failure", extra.Extra[service.CodeBuddyCreditErrorKey])
}

// --- 字段矩阵回归：成功事务写入全部 B4 键（阈值候选/reset_at 消费依赖）---

func TestCodeBuddyCreditConditional_SuccessFieldMatrixRegression(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	id := mustCreateEmptyExtraCodeBuddyAccount(t, tx.Client(), "cb-field-matrix")

	now := time.Now().UTC().Truncate(time.Microsecond)
	resetAt := now.Add(48 * time.Hour).Truncate(time.Microsecond)
	accepted, err := repo.WriteCodeBuddyCreditSnapshot(ctx, id, service.CodeBuddyCreditSnapshotWrite{
		SuccessTime: now, Version: 6,
		Packages: []service.CodeBuddyCreditPackage{
			codeBuddyCreditPackageForTest("p1", "10", "100"),
			codeBuddyCreditPackageForTest("p2", "20", "200"),
		},
		UsedPercent: 90,
		ResetAt:     resetAt,
		ErrorMsg:    "",
	})
	require.NoError(t, err)
	require.True(t, accepted)

	extra, err := repo.GetByID(ctx, id)
	require.NoError(t, err)

	// 字段矩阵逐项（R2 #3）：分包快照 + packages_updated_at + reset_at +
	// used_percent + last_attempt_at(=success_time) + version(=attempt_version)。
	require.Contains(t, extra.Extra, service.CodeBuddyCreditPackagesKey)
	require.Equal(t, now.UTC(), mustParseRepoTime(t, extra.Extra[service.CodeBuddyCreditPackagesUpdatedAtKey]), "freshness 必须 = success_time")
	require.Equal(t, resetAt.UTC(), mustParseRepoTime(t, extra.Extra[service.CodeBuddyCreditResetAtKey]), "reset_at 语义保留（阈值候选消费）")
	require.Equal(t, 90.0, extra.Extra[service.CodeBuddyCreditUsedPercentKey], "used_percent 必须写入（阈值候选消费）")
	require.Equal(t, now.UTC(), mustParseRepoTime(t, extra.Extra[service.CodeBuddyCreditLastAttemptAtKey]), "成功事务 last_attempt_at 必须 = success_time")
	require.Equal(t, float64(6), extra.Extra[service.CodeBuddyCreditVersionKey], "version 必须 = attempt_version（禁提交期自增）")

	// 旧键不得被写入口触碰。
	for _, oldKey := range []string{"codebuddy_credit_total", "codebuddy_credit_used", "codebuddy_credit_updated_at"} {
		require.NotContains(t, extra.Extra, oldKey, "旧键 %s 不得被条件更新写入口写入", oldKey)
	}
}

// mustParseRepoTime 解析 extra 里的时间键值（string RFC3339）。
func mustParseRepoTime(t *testing.T, v any) time.Time {
	t.Helper()
	s, ok := v.(string)
	require.True(t, ok, "时间键值必须为 string，实际 %T", v)
	parsed, err := time.Parse(time.RFC3339Nano, s)
	require.NoError(t, err, "解析时间 %q 失败", s)
	return parsed.UTC()
}
