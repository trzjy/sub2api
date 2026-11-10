//go:build unit

package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestQuotaKeysBackfillAccountSettingsMigration 守护 306 迁移的静态结构契约
// （与 group_model_allowlist_repair_migration_test.go 同风格，不连库，对迁移源码做断言）：
//   - 文件可读（被 embed.FS 嵌入）；
//   - 不硬编码 schema：不得含 'public.' 或 table_schema = 'public'，以便隔离 schema 测试与
//     search_path 兼容；
//   - 含幂等判定（仅在码级列初始状态时回填，不覆盖手工设定值）；
//   - 含冲突日志（主/备账号设置不一致时 RAISE NOTICE 记录，以最新账号为准）。
func TestQuotaKeysBackfillAccountSettingsMigration(t *testing.T) {
	content, err := FS.ReadFile("306_quota_keys_backfill_account_settings.sql")
	require.NoError(t, err, "306 迁移文件必须存在且被嵌入（embed.FS 覆盖 *.sql）")

	sql := strings.Join(strings.Fields(string(content)), " ")

	// 1) 不得硬编码 public schema（隔离 schema 测试与 search_path 兼容）。
	require.NotContains(t, sql, "public.", "禁止硬编码 'public.'：迁移须走 search_path 兼容隔离 schema")
	require.NotContains(t, sql, "table_schema = 'public'", "禁止 table_schema = 'public'：迁移须走 search_path 兼容隔离 schema")

	// 2) 幂等判定：仅在码级列初始状态时回填，不覆盖手工设定值。
	//    concurrency：q.concurrency IS NULL AND acc.concurrency > 0
	require.Contains(t, sql, "concurrency IS NULL", "缺少 concurrency 幂等判定（仅当码级为 NULL 时回填）")
	require.Contains(t, sql, "acc.concurrency > 0", "缺少 concurrency 正数值校验（>0 才回填）")
	//    load_factor：q.load_factor IS NULL AND acc.load_factor IS NOT NULL AND acc.load_factor > 0
	require.Contains(t, sql, "load_factor IS NULL", "缺少 load_factor 幂等判定（仅当码级为 NULL 时回填）")
	//    rate_multiplier：q.rate_multiplier = 1.0 且非 1.0 才回填
	require.Contains(t, sql, "rate_multiplier = 1.0", "缺少 rate_multiplier 幂等判定（仅当中性值 1.0 时回填）")
	require.Contains(t, sql, "acc.rate_multiplier <> 1.0", "缺少 rate_multiplier 差异校验（<>1.0 才回填）")
	//    model_mapping：JSONB 对象且非空才回填
	require.Contains(t, sql, "jsonb_typeof(acc.credentials->'model_mapping') = 'object'", "缺少 model_mapping 类型校验（须为 object）")
	require.Contains(t, sql, "acc.credentials->'model_mapping' <> '{}'::jsonb", "缺少 model_mapping 非空校验（非 {} 才回填）")

	// 3) 冲突日志：主/备账号设置不一致时以最新账号为准并记录。
	require.Contains(t, sql, "backfill conflict, adopting latest account", "缺少冲突日志（主/备账号设置不一致须 RAISE NOTICE 记录）")

	// 4) 到期时间对齐：码级非空时把名下未软删 cursor 账号 expires_at 同步为码级值。
	require.Contains(t, sql, "expires_at IS DISTINCT FROM resolved_expires", "缺少到期时间对齐（仅当账号与码级到期时间不同才更新）")
}
