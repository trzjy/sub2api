package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGroupAggregateCodeBuddyEnabledMigration 验证 263 迁移：幂等新增
// aggregate_codebuddy_enabled 列（boolean NOT NULL DEFAULT FALSE），并为该列加注释。
// 既有行因 DEFAULT FALSE 落 false（关闭），部署后聚合分组候选池不含 codebuddy，
// 直至逐分组开绑定。无数据回填、禁止 DROP。
func TestGroupAggregateCodeBuddyEnabledMigration(t *testing.T) {
	content, err := FS.ReadFile("263_add_group_aggregate_codebuddy.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql,
		"ADD COLUMN IF NOT EXISTS aggregate_codebuddy_enabled BOOLEAN NOT NULL DEFAULT FALSE")
	require.Contains(t, sql, "COMMENT ON COLUMN groups.aggregate_codebuddy_enabled")
}
