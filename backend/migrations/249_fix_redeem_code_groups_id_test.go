package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFixRedeemCodeGroupsIDMigration 校验 249 迁移：redeem_code_groups 补齐
// ent 实体期望的自增 id 主键，原复合主键语义降级为唯一约束保留。
func TestFixRedeemCodeGroupsIDMigration(t *testing.T) {
	content, err := FS.ReadFile("249_fix_redeem_code_groups_id.sql")
	require.NoError(t, err)

	// 剥掉注释行，只对 DDL 断言。
	var ddlLines []string
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "--") {
			continue
		}
		ddlLines = append(ddlLines, line)
	}
	sql := strings.Join(strings.Fields(strings.Join(ddlLines, " ")), " ")

	require.Contains(t, sql, "DROP CONSTRAINT IF EXISTS redeem_code_groups_pkey")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS id BIGSERIAL")
	require.Contains(t, sql, "CREATE UNIQUE INDEX IF NOT EXISTS redeem_code_groups_id_key")
	require.Contains(t, sql, "ON redeem_code_groups (id)")
	require.Contains(t, sql,
		"CREATE UNIQUE INDEX IF NOT EXISTS redeem_code_groups_code_group_key")
	require.Contains(t, sql, "ON redeem_code_groups (redeem_code_id, group_id)")

	// 不允许波及相邻表（redeem_codes / welfare_balances），不许删表。
	require.NotContains(t, sql, "ALTER TABLE redeem_codes")
	require.NotContains(t, sql, "welfare_balances")
	require.NotContains(t, sql, "DROP TABLE")
}
