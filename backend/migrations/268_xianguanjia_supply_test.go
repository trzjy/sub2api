package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestXianguanjiaSupplyMigration 校验 268 迁移：在既有 252 表上追加货源方向两列，
// 不改动 ERP 列、不新增表。
func TestXianguanjiaSupplyMigration(t *testing.T) {
	content, err := FS.ReadFile("268_xianguanjia_supply.sql")
	require.NoError(t, err)
	// 去掉 SQL 行注释后再做断言，避免注释文字触发误判。
	var stmt strings.Builder
	for _, line := range strings.Split(string(content), "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		stmt.WriteString(line)
		stmt.WriteString(" ")
	}
	sql := strings.Join(strings.Fields(stmt.String()), " ")

	require.Contains(t, sql, "ALTER TABLE xianyu_xianguanjia_config")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS supply_app_id VARCHAR(120)")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS supply_app_secret_encrypted TEXT")

	// 只增列、不建表/不删列。
	require.NotContains(t, sql, "CREATE TABLE")
	require.NotContains(t, sql, "DROP COLUMN")

	// 仅两条 ALTER，均作用于 252 表。
	require.Equal(t, 2, strings.Count(sql, "ALTER TABLE"))
	require.Equal(t, 2, strings.Count(sql, "xianyu_xianguanjia_config"))
}
