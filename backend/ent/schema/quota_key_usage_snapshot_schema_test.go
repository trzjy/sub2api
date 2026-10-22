package schema

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"entgo.io/ent/schema/field"
	"github.com/stretchr/testify/require"
)

// TestQuotaKeyUsageSnapshotSchemaField 验证 CR-A（方案 §25.6 / §25.14-C「唯一状态表示」）
// 在 schema 层落地的存储结构即一个可空 JSONB 列 usage_snapshot。
//
// 断言要点：
//   - 字段存在，且类型是 ent 的 JSON（落库 jsonb，不是拆开的整数列——拆列会把
//     「未观测」的 Go 零值写成假事实 0）；
//   - Optional：存量行与新建行都允许缺省（NULL = 尚未观测）；
//   - SchemaType[postgres] == "jsonb"：与迁移 292 / 生成代码三处同型；
//   - 不参与唯一约束、不参与索引（本单只建存储结构，查询形态由后续单决定）。
func TestQuotaKeyUsageSnapshotSchemaField(t *testing.T) {
	quotaKey := loadQuotaKeySchema(t)
	f := requireSchemaField(t, quotaKey, "usage_snapshot")

	require.Equal(t, field.TypeJSON, f.Info.Type, "usage_snapshot must be stored as ent JSON (jsonb) field")
	require.True(t, f.Optional, "usage_snapshot must be optional (NULL = 尚未观测)")
	require.Equal(t, "jsonb", f.SchemaType["postgres"], "usage_snapshot must use postgres jsonb")
	require.False(t, f.Unique, "usage_snapshot must not be unique")
	require.False(t, f.Immutable, "usage_snapshot must be mutable (每次观测覆盖)")

	for _, idx := range quotaKey.Indexes {
		for _, name := range idx.Fields {
			require.NotEqual(t, "usage_snapshot", name, "usage_snapshot must not be part of any index")
		}
	}
}

// TestQuotaKeyUsageSnapshotMigration 验证迁移 292 与 schema 声明同型：
// 只新增一个可空 JSONB 列，且可重入（IF NOT EXISTS），不回填、不改既有列。
//
// 依赖边界说明：本测试只读迁移文件文本（不连库、不执行 SQL），用于钉住
// 「schema / 迁移 / 生成代码三处一致」中的第二处；生成代码一处由 ent 重生成 +
// 编译测试覆盖。
func TestQuotaKeyUsageSnapshotMigration(t *testing.T) {
	path := filepath.Join("..", "..", "migrations", "292_quota_keys_usage_snapshot.sql")
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "migration 292 must exist")

	sql := string(raw)
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS usage_snapshot JSONB NULL",
		"migration 292 must add usage_snapshot as a nullable JSONB column (idempotent)")

	// 只新增列：剥掉 `--` 注释后，迁移里除该 ADD COLUMN 外不得出现其它 DDL/DML 动作
	// （不含回填、不含建索引、不含改写既有列；还原语句只允许留在注释里）。
	var stmt strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		stmt.WriteString(line)
		stmt.WriteString("\n")
	}
	upper := strings.ToUpper(stmt.String())
	for _, forbidden := range []string{"DROP COLUMN", "CREATE INDEX", "UPDATE ", "INSERT ", "DELETE ", "ALTER COLUMN"} {
		require.NotContains(t, upper, forbidden, "migration 292 must not %s", forbidden)
	}
}
