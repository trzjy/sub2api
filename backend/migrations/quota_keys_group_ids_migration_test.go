package migrations

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestQuotaKeysGroupIDsMigration 守护模块 A1 的 Schema 契约（迁移 307）
// （与 299 同风格，不连库，对迁移源码做断言）：
//   - 文件可读（被 embed.FS 嵌入）；
//   - 幂等保护：ADD COLUMN IF NOT EXISTS，可重复执行；
//   - 关键子句：新增 group_ids 列，类型为 JSONB；
//   - 语义注释：NULL=未显式指定（拉号沿用模型推导挂组）、
//     []=显式解绑全部分组、[1,2]=码级权威分组；
//   - 回滚注释存在：ALTER TABLE quota_keys DROP COLUMN IF EXISTS group_ids；
//   - 无裸事务控制语句（BEGIN/COMMIT/ROLLBACK）——迁移由运行器统一管事务。
func TestQuotaKeysGroupIDsMigration(t *testing.T) {
	content, err := FS.ReadFile("307_quota_keys_group_ids.sql")
	require.NoError(t, err, "307 迁移文件必须存在且被嵌入（embed.FS 覆盖 *.sql）")
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

	// 2) 关键子句 + 幂等保护：ALTER TABLE ... ADD COLUMN IF NOT EXISTS group_ids JSONB。
	require.Contains(t, sql, "ALTER TABLE quota_keys ADD COLUMN IF NOT EXISTS group_ids JSONB",
		"缺少关键子句 ALTER TABLE quota_keys ADD COLUMN IF NOT EXISTS group_ids JSONB（须幂等可重复执行）")

	// 3) 语义注释：三种语义必须被注释说明。
	require.Contains(t, sql, "NULL", "缺少 NULL 语义注释（未显式指定/拉号沿用模型推导挂组）")
	require.Contains(t, sql, "[]", "缺少 [] 语义注释（显式解绑全部分组）")
	require.Contains(t, sql, "[1,2]", "缺少 [1,2] 语义注释（码级权威分组）")

	// 4) 回滚注释存在。
	require.Contains(t, sql, "ALTER TABLE quota_keys DROP COLUMN IF EXISTS group_ids",
		"缺少回滚注释：ALTER TABLE quota_keys DROP COLUMN IF EXISTS group_ids")
}
