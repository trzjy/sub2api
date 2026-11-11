package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestQuotaKeysGroupIDsBackfillMigration 守护 308 迁移的静态结构契约
// （与 quota_keys_backfill_settings_migration_test.go 同型，不连库，对迁移源码做断言）：
//   - 文件可读（被 embed.FS 嵌入）；
//   - 不硬编码 schema：不得含 'public.' 或 table_schema = 'public'，以便隔离 schema 测试与
//     search_path 兼容；
//   - 幂等判定：仅当 quota_keys.group_ids IS NULL 时回填，已回填的码被 FOR 循环过滤跳过；
//   - 取最新未软删 cursor 账号（platform = 'cursor' 且 deleted_at IS NULL）；
//   - 读 account_groups 聚合分组集合；
//   - 冲突日志：同码多账号分组集合不一致时 RAISE NOTICE 记录，以最新账号为准并同步其余账号。
func TestQuotaKeysGroupIDsBackfillMigration(t *testing.T) {
	content, err := FS.ReadFile("308_quota_keys_backfill_group_ids.sql")
	require.NoError(t, err, "308 迁移文件必须存在且被嵌入（embed.FS 覆盖 *.sql）")

	sql := strings.Join(strings.Fields(string(content)), " ")

	// 1) 不得硬编码 public schema（隔离 schema 测试与 search_path 兼容）。
	require.NotContains(t, sql, "public.", "禁止硬编码 'public.'：迁移须走 search_path 兼容隔离 schema")
	require.NotContains(t, sql, "table_schema = 'public'", "禁止 table_schema = 'public'：迁移须走 search_path 兼容隔离 schema")

	// 2) 幂等判定：FOR 循环仅遍历 group_ids IS NULL 的激活码，已回填的码跳过。
	require.Contains(t, sql, "group_ids IS NULL", "缺少幂等过滤（FOR 循环须仅处理 group_ids IS NULL 的激活码）")

	// 3) 仅处理未软删记录。
	require.Contains(t, sql, "deleted_at IS NULL", "缺少软删过滤（须仅处理 deleted_at IS NULL 的激活码/账号）")

	// 4) 取最新未软删 cursor 账号。
	require.Contains(t, sql, "platform = 'cursor'", "缺少平台过滤（须取 platform = 'cursor' 的账号）")

	// 5) 读 account_groups 聚合分组集合到码级列。
	require.Contains(t, sql, "account_groups", "缺少 account_groups 读写（须读账号分组并聚合写入码级列）")
	require.Contains(t, sql, "to_jsonb(groups_arr)", "缺少码级写入（须 UPDATE quota_keys SET group_ids = to_jsonb(<数组>）")
	require.Contains(t, sql, "priority ASC, ag.group_id ASC", "缺少聚合排序（须按 priority ASC, group_id ASC 聚合）")

	// 6) 冲突日志：同码多账号分组集合不一致时以最新账号为准并记录。
	require.Contains(t, sql, "RAISE NOTICE", "缺少 RAISE NOTICE（冲突须记录通知）")
	require.Contains(t, sql, "group backfill conflict, adopting latest account", "缺少冲突日志文案（须 RAISE NOTICE 记录同码分组冲突并以最新账号为准）")

	// 7) 冲突同步：以最新账号为准把其余账号 account_groups 替换（先删后建）。
	require.Contains(t, sql, "DELETE FROM account_groups WHERE account_id", "缺少冲突同步删除（须先删其余账号的 account_groups）")
	require.Contains(t, sql, "INSERT INTO account_groups (account_id, group_id, priority)", "缺少冲突同步重建（须按 priority = 序号 重建）")
}
