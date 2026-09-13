package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWidenRedeemCodeColumnMigration 校验 248 迁移：code 列放宽到 VARCHAR(64)，
// 容纳生成器的 XXXX-XXXX-XXXX-XXXX（35 字符）格式。
func TestWidenRedeemCodeColumnMigration(t *testing.T) {
	content, err := FS.ReadFile("248_widen_redeem_code_column.sql")
	require.NoError(t, err)
	sql := strings.Join(strings.Fields(string(content)), " ")

	require.Contains(t, sql, "ALTER TABLE redeem_codes")
	require.Contains(t, sql, "ALTER COLUMN code TYPE VARCHAR(64)")

	// 不应触碰其他表或列。
	require.Equal(t, 1, strings.Count(sql, "ALTER TABLE"))
	require.NotContains(t, sql, "DROP")
}
