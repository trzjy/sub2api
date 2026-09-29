//go:build unit

package repository

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// 本文件覆盖 Card B 条件更新 repo 方法的**单元契约**（-tags unit，不连库）：
// SQL 谓词形态（tuple 接受条件 / canonical 转换 / 键集合 / jsonb_build_object
// 仅合并 B4 键）、RowsAffected=0 = 拒绝语义、attempt_version 取号 SQL。
// 真实 DB 的五场景行为由 -tags integration 门禁覆盖。

// memorySQLResult 是 sql.Result 的内存实现（RowsAffected 可编程）。
type memorySQLResult struct {
	affected int64
}

func (r memorySQLResult) LastInsertId() (int64, error) { return 0, errors.New("not supported") }
func (r memorySQLResult) RowsAffected() (int64, error) { return r.affected, nil }

// captureSQLExecutor 捕获最近一次 Exec/Query 的 SQL 与参数，并返回可编程结果。
type captureSQLExecutor struct {
	lastQuery string
	lastArgs  []any
	execResult sql.Result
	execErr    error
	rowsErr    error
}

func (c *captureSQLExecutor) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	c.lastQuery = query
	c.lastArgs = args
	if c.execErr != nil {
		return nil, c.execErr
	}
	if c.execResult != nil {
		return c.execResult, nil
	}
	return memorySQLResult{affected: 1}, nil
}

func (c *captureSQLExecutor) QueryContext(_ context.Context, query string, args ...any) (*sql.Rows, error) {
	c.lastQuery = query
	c.lastArgs = args
	// 取号路径在单元测试中只验证错误分支（rowsErr），成功分支的真实 Rows
	 // 扫描由 -tags integration 门禁覆盖（*sql.Rows 无法在内存中构造）。
	if c.rowsErr != nil {
		return nil, c.rowsErr
	}
	return nil, errors.New("no rows configured")
}

// conditionalRepo 构造一个注入 mock executor 的 accountRepository。
func conditionalRepo(exec *captureSQLExecutor) *accountRepository {
	return &accountRepository{client: nil, sql: exec, schedulerCache: nil}
}

func TestWriteCodeBuddyCreditSnapshot_SQLShapeAndKeys(t *testing.T) {
	exec := &captureSQLExecutor{}
	repo := conditionalRepo(exec)
	now := time.Now().UTC().Truncate(time.Microsecond)

	accepted, err := repo.WriteCodeBuddyCreditSnapshot(context.Background(), 42, service.CodeBuddyCreditSnapshotWrite{
		SuccessTime: now,
		Version:     9,
		Packages: []service.CodeBuddyCreditPackage{{
			ID: "a", Name: "p", Unit: "credits",
			Remaining: decimal.RequireFromString("10"), Total: decimal.RequireFromString("100"),
			ExpiresAt: "2026-10-15T21:26:43Z", Status: 0,
		}},
		UsedPercent: 90,
		ResetAt:     now.Add(time.Hour),
		ErrorMsg:    "",
	})
	require.NoError(t, err)
	require.True(t, accepted)

	// 必须走 UPDATE accounts（条件更新），不得走旧 UpdateExtra 的无条件合并。
	require.True(t, strings.Contains(exec.lastQuery, "UPDATE accounts"))
	require.True(t, strings.Contains(exec.lastQuery, "SET extra ="))
	require.True(t, strings.Contains(exec.lastQuery, "deleted_at IS NULL"))

	// jsonb_build_object 只合并 B4 键，不触碰旧总量/updated_at 旧键。
	for _, key := range []string{
		service.CodeBuddyCreditPackagesKey,
		service.CodeBuddyCreditPackagesUpdatedAtKey,
		service.CodeBuddyCreditResetAtKey,
		service.CodeBuddyCreditUsedPercentKey,
		service.CodeBuddyCreditLastAttemptAtKey,
		service.CodeBuddyCreditVersionKey,
		service.CodeBuddyCreditErrorKey,
	} {
		require.Contains(t, exec.lastQuery, "'"+key+"'", "B4 键 %s 必须由 jsonb_build_object 合并", key)
	}
	for _, oldKey := range []string{
		"codebuddy_credit_total",
		"codebuddy_credit_used",
		"codebuddy_credit_updated_at",
	} {
		require.NotContains(t, exec.lastQuery, "'"+oldKey+"'", "旧键 %s 不得被条件更新写口触碰", oldKey)
	}

	// tuple 接受条件：candidate_event_time > current OR (= AND version > current)。
	require.True(t, strings.Contains(exec.lastQuery, "->> '"+service.CodeBuddyCreditPackagesUpdatedAtKey+"'"))
	require.True(t, strings.Contains(exec.lastQuery, "->> '"+service.CodeBuddyCreditLastAttemptAtKey+"'"))
	require.True(t, strings.Contains(exec.lastQuery, "->> '"+service.CodeBuddyCreditVersionKey+"'"))
	require.True(t, strings.Contains(exec.lastQuery, "-infinity"))
	require.True(t, strings.Contains(exec.lastQuery, "GREATEST"))

	// 参数顺序：$1=SuccessTime, $2=Version, $3=packages JSON, $4=ResetAt,
	// $5=UsedPercent, $6=ErrorMsg, $7=accountID。
	require.Len(t, exec.lastArgs, 7)
	require.Equal(t, now, exec.lastArgs[0])
	require.Equal(t, int64(9), exec.lastArgs[1])
	require.IsType(t, "", exec.lastArgs[2], "packages 必须以 JSON 字符串传参")
	require.Equal(t, now.Add(time.Hour), exec.lastArgs[3])
	require.Equal(t, 90.0, exec.lastArgs[4])
	require.Equal(t, "", exec.lastArgs[5], "无错误时 ErrorMsg 为空 → 删除旧错误键")
	require.Equal(t, int64(42), exec.lastArgs[6])
}

func TestWriteCodeBuddyCreditSnapshot_WithErrorKeepsErrorKey(t *testing.T) {
	exec := &captureSQLExecutor{}
	repo := conditionalRepo(exec)
	now := time.Now().UTC().Truncate(time.Microsecond)

	_, err := repo.WriteCodeBuddyCreditSnapshot(context.Background(), 1, service.CodeBuddyCreditSnapshotWrite{
		SuccessTime: now,
		Version:     1,
		Packages:    nil,
		UsedPercent: 0,
		ResetAt:     now,
		ErrorMsg:    `[{"id":"a","reason":"unit_mismatch","raw":{"unit":"tokens"}}]`,
	})
	require.NoError(t, err)

	// 有错误：不执行 `extra - 'codebuddy_credit_error'` 删除分支，错误值由
	// jsonb_build_object 写入。
	require.True(t, strings.Contains(exec.lastQuery, service.CodeBuddyCreditErrorKey))
	require.True(t, strings.Contains(exec.lastQuery, "to_jsonb($6::text)"))
	require.Equal(t, `[{"id":"a","reason":"unit_mismatch","raw":{"unit":"tokens"}}]`, exec.lastArgs[5])
}

func TestWriteCodeBuddyCreditSnapshot_RowsAffectedZeroMeansRejected(t *testing.T) {
	exec := &captureSQLExecutor{execResult: memorySQLResult{affected: 0}}
	repo := conditionalRepo(exec)
	now := time.Now().UTC().Truncate(time.Microsecond)

	accepted, err := repo.WriteCodeBuddyCreditSnapshot(context.Background(), 1, service.CodeBuddyCreditSnapshotWrite{
		SuccessTime: now,
		Version:     1,
		Packages:    nil,
		UsedPercent: 0,
		ResetAt:     now,
	})
	require.NoError(t, err)
	require.False(t, accepted, "RowsAffected=0 = 条件更新被拒绝")
}

func TestWriteCodeBuddyCreditAttemptError_OnlyFailureKeys(t *testing.T) {
	exec := &captureSQLExecutor{}
	repo := conditionalRepo(exec)
	now := time.Now().UTC().Truncate(time.Microsecond)

	accepted, err := repo.WriteCodeBuddyCreditAttemptError(context.Background(), 7, service.CodeBuddyCreditAttemptErrorWrite{
		AttemptTime: now,
		Version:     3,
		ErrorMsg:    "upstream http 502",
	})
	require.NoError(t, err)
	require.True(t, accepted)

	q := exec.lastQuery
	// 失败事务只合并 last_attempt_at + version + error（写入键集合 = SET 表达式里的
	// jsonb_build_object 段）。
	writeSet := codebuddyWriteKeySet(t, q)
	for _, key := range []string{
		service.CodeBuddyCreditLastAttemptAtKey,
		service.CodeBuddyCreditVersionKey,
		service.CodeBuddyCreditErrorKey,
	} {
		require.Contains(t, writeSet, key, "失败事务必须写键 %s", key)
	}
	// 不得触碰成功快照/freshness/reset_at/used_percent 的**写入**。
	for _, forbidden := range []string{
		service.CodeBuddyCreditPackagesKey,
		service.CodeBuddyCreditPackagesUpdatedAtKey,
		service.CodeBuddyCreditResetAtKey,
		service.CodeBuddyCreditUsedPercentKey,
	} {
		require.NotContains(t, writeSet, forbidden, "失败事务不得写键 %s", forbidden)
	}
	// 同一 tuple 比较器仍存在（WHERE 段可**读**当前已接受状态的两个时间键）。
	require.Contains(t, codebuddyWhereCondition(t, q), "GREATEST")
	require.Contains(t, codebuddyWhereCondition(t, q), "-infinity")
	// 参数：$1=AttemptTime, $2=Version, $3=ErrorMsg, $4=accountID.
	require.Len(t, exec.lastArgs, 4)
	require.Equal(t, now, exec.lastArgs[0])
	require.Equal(t, int64(3), exec.lastArgs[1])
	require.Equal(t, "upstream http 502", exec.lastArgs[2])
	require.Equal(t, int64(7), exec.lastArgs[3])
}

// codebuddyWriteKeySet 提取 UPDATE 的 SET 段（第一个 WHERE 之前）中
// jsonb_build_object 写入的键集合，用于断言"写入口只合并 B4 负责的键"。
func codebuddyWriteKeySet(t *testing.T, query string) string {
	t.Helper()
	setIdx := strings.Index(query, "SET extra =")
	require.GreaterOrEqual(t, setIdx, 0, "SQL 必须含 SET extra")
	rest := query[setIdx:]
	whereIdx := strings.Index(rest, " WHERE ")
	require.GreaterOrEqual(t, whereIdx, 0, "SQL 必须含 WHERE")
	return rest[:whereIdx]
}

// codebuddyWhereCondition 提取 WHERE 段（tuple 接受条件），用于区分
// "比较表达式读当前状态"与"写入键集合"。
func codebuddyWhereCondition(t *testing.T, query string) string {
	t.Helper()
	whereIdx := strings.Index(query, " WHERE ")
	require.GreaterOrEqual(t, whereIdx, 0, "SQL 必须含 WHERE")
	return query[whereIdx:]
}

func TestWriteCodeBuddyCreditAttemptError_RowsAffectedZeroMeansRejected(t *testing.T) {
	exec := &captureSQLExecutor{execResult: memorySQLResult{affected: 0}}
	repo := conditionalRepo(exec)
	now := time.Now().UTC().Truncate(time.Microsecond)

	accepted, err := repo.WriteCodeBuddyCreditAttemptError(context.Background(), 1, service.CodeBuddyCreditAttemptErrorWrite{
		AttemptTime: now,
		Version:     1,
		ErrorMsg:    "err",
	})
	require.NoError(t, err)
	require.False(t, accepted, "失败事务 RowsAffected=0 同样 = 被拒绝")
}

// TestNextCodeBuddyCreditAttemptVersion_SQLShapeAndFailure 断言取号 SQL 形态与
// sequence 名；失败（executor 报错）→ 返回 error（本次抓取失败关闭）。
func TestNextCodeBuddyCreditAttemptVersion_SQLShapeAndFailure(t *testing.T) {
	exec := &captureSQLExecutor{rowsErr: errors.New("db down")}
	repo := conditionalRepo(exec)
	_, err := repo.NextCodeBuddyCreditAttemptVersion(context.Background())
	require.Error(t, err, "取号失败必须返回 error（本次抓取失败关闭）")
	require.Contains(t, exec.lastQuery, "nextval")
	require.Contains(t, exec.lastQuery, codebuddyAttemptVersionSequenceName)
}
