//go:build unit

package migrations

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMigration302QuotaUpstreamsKeyPartialUnique 守护 KD22-03 的修复契约
// （与 299/297 同风格，不连库，对迁移源码做断言）：
//   - 文件可读（被 embed.FS 嵌入）；
//   - 无裸事务控制语句（BEGIN/COMMIT/ROLLBACK）——迁移由运行器统一管事务；
//   - 先 DROP 旧索引（名称保持 uq_quota_upstreams_key）再重建，避免同名校验冲突；
//   - CREATE UNIQUE INDEX IF NOT EXISTS uq_quota_upstreams_key 名称保持不变；
//   - 核心条件 WHERE deleted_at IS NULL（排除软删行）；
//   - 不含 CONCURRENTLY（表小，普通事务迁移足够，无需 _notx 后缀）。
func TestMigration302QuotaUpstreamsKeyPartialUnique(t *testing.T) {
	content, err := FS.ReadFile("302_quota_upstreams_key_partial_unique.sql")
	require.NoError(t, err, "302 迁移文件必须存在且被嵌入（embed.FS 覆盖 *.sql）")
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

	// 2) 先 DROP 旧索引（同名校验需重建）。
	require.Contains(t, sql, "DROP INDEX IF EXISTS uq_quota_upstreams_key",
		"缺少 DROP INDEX IF EXISTS uq_quota_upstreams_key（需先丢旧索引再重建）")

	// 3) 重建唯一索引且名称保持不变。
	require.Contains(t, sql, "CREATE UNIQUE INDEX IF NOT EXISTS uq_quota_upstreams_key",
		"缺少 CREATE UNIQUE INDEX IF NOT EXISTS uq_quota_upstreams_key（索引名需保持）")

	// 4) 核心修复条件：排除软删行。
	require.Contains(t, sql, "WHERE deleted_at IS NULL",
		"缺少核心修复条件 WHERE deleted_at IS NULL（须排除软删行）")

	// 5) 不得使用 CONCURRENTLY（普通事务迁移即可）。
	require.NotContains(t, sql, "CONCURRENTLY",
		"禁止 CONCURRENTLY：表行数小，普通事务迁移足够，无需 _notx 后缀")
}
