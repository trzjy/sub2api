package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUsageLogsPlatformSideRowsNullableMigration 校验 261 号迁移放开
// usage_logs.user_id / api_key_id 的 NOT NULL（平台侧检测行两列为 NULL，
// 方案 docs/capability-routing-plan.md §6）。FK 必须保留不动（PostgreSQL
// 对 NULL 天然放行，无需重建约束），否则会丢失业务行的引用完整性。
func TestUsageLogsPlatformSideRowsNullableMigration(t *testing.T) {
	content, err := FS.ReadFile("261_usage_logs_platform_side_rows_nullable.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")

	require.Contains(t, sql, "ALTER TABLE usage_logs ALTER COLUMN user_id DROP NOT NULL")
	require.Contains(t, sql, "ALTER TABLE usage_logs ALTER COLUMN api_key_id DROP NOT NULL")
	// 绝不允许触碰 FK：DROP NOT NULL 即可，FK 对 NULL 天然放行
	require.NotContains(t, sql, "DROP CONSTRAINT")
	require.NotContains(t, sql, "ADD CONSTRAINT")
}
