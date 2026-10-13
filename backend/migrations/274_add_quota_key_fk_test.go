package migrations

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMigration274AddQuotaKeyFk 守护 D-CURSOR-025 缺陷 1 的整改：
//
// 迁移运行器在**自己的事务内**执行事务型迁移（repository/migrations_runner.go：
// lockConn.BeginTx → tx.ExecContext(整个文件) → 写 schema_migrations → tx.Commit）。
// 迁移文件自带 BEGIN/COMMIT 会提前提交运行器事务，导致
// `pq: unexpected transaction status idle`，全新库首装必然失败一次。
//
// 本测试锁定：274 不得含任何裸事务控制语句（BEGIN/COMMIT/ROLLBACK），
// 同时保留 4 段 DDL（列 / CHECK / FK / 部分索引）以证伪「删过头」。
func TestMigration274AddQuotaKeyFk(t *testing.T) {
	content, err := FS.ReadFile("274_add_quota_key_fk.sql")
	require.NoError(t, err)
	sql := string(content)

	// 1) 裸事务控制语句：逐行剔除 SQL 注释后再匹配，避免误伤回滚注释里的 BEGIN/COMMIT。
	txStmt := regexp.MustCompile(`(?im)^\s*(BEGIN|COMMIT|ROLLBACK)\s*;`)
	for i, line := range strings.Split(sql, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			continue
		}
		require.False(t, txStmt.MatchString(line),
			"第 %d 行含裸事务控制语句 %q：迁移由运行器统一管事务，文件内不得自带", i+1, trimmed)
	}

	// 2) DDL 契约原样保留（仅删事务控制，不得删语句）。
	require.Contains(t, sql, "ALTER TABLE accounts ADD COLUMN IF NOT EXISTS quota_key_id")
	require.Contains(t, sql, "accounts_cursor_quota_key_required")
	require.Contains(t, sql, "CHECK (platform <> 'cursor' OR quota_key_id IS NOT NULL)")
	require.Contains(t, sql, "FOREIGN KEY (quota_key_id) REFERENCES quota_keys(id) ON DELETE RESTRICT")
	require.Contains(t, sql, "CREATE INDEX idx_accounts_quota_key_working")
}
