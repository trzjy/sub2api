//go:build integration

package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/ent/migrate"
	"github.com/stretchr/testify/require"
)

// TestEntSchemaParityWithSQLMigrations 生产建表走嵌入 SQL 迁移，而 ent 运行时按
// ent 模型生成 SQL。两张“图纸”一旦漂移（如 redeem_code_groups 曾缺 id 列），
// 只有线上炸了才知道。本测试在 ApplyMigrations 后的真库上逐表比对列集合，
// 保证 ent 模型期望的每一列都真实存在。
func TestEntSchemaParityWithSQLMigrations(t *testing.T) {
	for _, table := range migrate.Tables {
		tableName := table.Name
		rows, err := integrationDB.Query(
			`SELECT column_name FROM information_schema.columns WHERE table_name = $1`,
			tableName,
		)
		require.NoError(t, err, "查询表 %s 的列", tableName)

		actual := map[string]bool{}
		for rows.Next() {
			var col string
			require.NoError(t, rows.Scan(&col))
			actual[col] = true
		}
		require.NoError(t, rows.Close())

		for _, col := range table.Columns {
			require.Truef(t, actual[col.Name],
				"表 %s 缺少 ent 模型期望的列 %q（SQL 迁移与 ent 模型漂移）", tableName, col.Name)
		}
	}
}
