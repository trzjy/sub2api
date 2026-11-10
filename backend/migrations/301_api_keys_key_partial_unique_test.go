//go:build unit

package migrations

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMigration301APIKeysKeyPartialUnique 守护 KD22-02 的修复契约
// （与 299 同风格，不连库，对迁移源码做断言）：
//   - 文件可读（被 embed.FS 嵌入）；
//   - 无裸事务控制语句（BEGIN/COMMIT/ROLLBACK）——迁移由运行器统一管事务；
//   - 先 DROP 旧约束/索引（api_keys_key_key），将列约束改为部分唯一索引；
//   - CREATE UNIQUE INDEX IF NOT EXISTS api_keys_key_active；
//   - 核心条件 WHERE deleted_at IS NULL（排除软删行）；
//   - 不含 CONCURRENTLY（表小，普通事务迁移足够）。
func TestMigration301APIKeysKeyPartialUnique(t *testing.T) {
	content, err := FS.ReadFile("301_api_keys_key_partial_unique.sql")
	require.NoError(t, err, "301 迁移文件必须存在且被嵌入（embed.FS 覆盖 *.sql）")
	sql := string(content)

	// 1) 无裸事务控制语句：逐行剔除 SQL 注释后再匹配，避免误伤回滚注释里的 BEGIN/COMMIT。
	txStmt := regexp.MustCompile(`(?im)^\s*(BEGIN|COMMIT|ROLLBACK)\s*;`)
	for i, line := range strings.Split(sql, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			continue
		}
		require.False(t, txStmt.MatchString(line),
			"第 %d 行含裸事务控制语句 %q：迁移由运行器统一管事务，文件内不得自带", i+1, trimmed)
	}

	// 2) 先 DROP 旧约束（列约束 api_keys_key_key，其自带同名索引，二者都要清理）。
	require.Contains(t, sql, "ALTER TABLE api_keys DROP CONSTRAINT IF EXISTS api_keys_key_key",
		"缺少 ALTER TABLE api_keys DROP CONSTRAINT IF EXISTS api_keys_key_key（需先丢旧约束）")

	// 3) 重建部分唯一索引 api_keys_key_active。
	require.Contains(t, sql, "CREATE UNIQUE INDEX IF NOT EXISTS api_keys_key_active",
		"缺少 CREATE UNIQUE INDEX IF NOT EXISTS api_keys_key_active（部分唯一索引名需保持）")

	// 4) 核心修复条件：排除软删行。
	require.Contains(t, sql, "WHERE deleted_at IS NULL",
		"缺少核心修复条件 WHERE deleted_at IS NULL（须排除软删行）")

	// 5) 不得使用 CONCURRENTLY（普通事务迁移即可）。
	require.NotContains(t, sql, "CONCURRENTLY",
		"禁止 CONCURRENTLY：表行数小，普通事务迁移足够，无需 _notx 后缀")
}
