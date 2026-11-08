package migrations

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMigration296CursorPlatformAllowApiKey 守护 CARD-KC-01 的整改：
//
// 为接入第三方 OpenAI 兼容网关上游（kn-cy 等）作为 Cursor 备用渠道，需允许
// platform = 'cursor' 且 type = 'api_key' 的账号 quota_key_id 为 NULL。历史迁移 274
// 的约束 `CHECK (platform <> 'cursor' OR quota_key_id IS NOT NULL)` 不可变，本迁移 296
// 通过 DROP + ADD 同名约束替换为
//   CHECK (platform <> 'cursor' OR quota_key_id IS NOT NULL OR type = 'api_key')
// 放宽检查条件。
//
// 本测试锁定迁移源码契约（与 274 同风格，不连库，对迁移源码做断言）：
//   - 文件可读（被 embed.FS 嵌入）；
//   - 无裸事务控制语句（BEGIN/COMMIT/ROLLBACK）——迁移由运行器统一管事务；
//   - 含 `ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_cursor_quota_key_required`；
//   - 含 `CHECK (platform <> 'cursor' OR quota_key_id IS NOT NULL OR type = 'api_key')`。
func TestMigration296CursorPlatformAllowApiKey(t *testing.T) {
	content, err := FS.ReadFile("296_cursor_platform_allow_api_key.sql")
	require.NoError(t, err, "296 迁移文件必须存在且被嵌入（embed.FS 覆盖 *.sql）")
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

	// 2) DDL 契约原样保留：DROP 同名约束（重入安全）+ 放宽后的 CHECK 条件。
	require.Contains(t, sql, "ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_cursor_quota_key_required",
		"缺少 DROP 同名约束语句（破坏 274 约束的重入/替换）")
	require.Contains(t, sql, "CHECK (platform <> 'cursor' OR quota_key_id IS NOT NULL OR type = 'api_key')",
		"缺少放宽后的 CHECK 条件：需允许 type='api_key' 时 quota_key_id 为 NULL")
}
