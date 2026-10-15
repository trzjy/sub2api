package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPlatformCheckConstraintsSupersetMigration 校验 288 号迁移（取代 278+279）：
//   - 可重入：先 DROP CONSTRAINT IF EXISTS 再 ADD CONSTRAINT；
//   - 约束存在性：两张表都重建 platform/target_platform CHECK；
//   - 全清单成员可写：并集成员逐个出现在两个 CHECK 的 IN(...) 中；
//   - 非 member 拒写：未登记平台（future_platform/bogus/composite 等）不在白名单内。
func TestPlatformCheckConstraintsSupersetMigration(t *testing.T) {
	content, err := FS.ReadFile("288_platform_check_constraints_superset.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")

	// 可重入：先删后加。
	require.Contains(t, sql, "ALTER TABLE user_platform_quotas DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check;")
	require.Contains(t, sql, "ALTER TABLE composite_model_routes DROP CONSTRAINT IF EXISTS composite_model_routes_target_platform_check;")
	require.Contains(t, sql, "ALTER TABLE user_platform_quotas ADD CONSTRAINT user_platform_quotas_platform_check")
	require.Contains(t, sql, "ALTER TABLE composite_model_routes ADD CONSTRAINT composite_model_routes_target_platform_check")

	// 全清单成员（推导链并集，muse 必在集内）。
	members := []string{
		"anthropic", "openai", "gemini", "antigravity", "grok",
		"kimi", "zhipu", "deepseek", "minimax", "other", "codebuddy", "muse",
		"opencode_go", "typesafe", "command_code", "cline",
	}
	joined := "'" + strings.Join(members, "', '") + "'"
	quotaCheck := "CHECK (platform IN (" + joined + "))"
	routeCheck := "CHECK (target_platform IN (" + joined + "))"
	require.Contains(t, sql, quotaCheck)
	require.Contains(t, sql, routeCheck)
	// muse 必在集内（门一/门二推导硬性要求）。
	require.Contains(t, sql, "'muse'")

	// 非 member 拒写：未登记平台不在白名单。
	for _, nonMember := range []string{"future_platform", "bogus", "moonshot", "composite", "unknown"} {
		require.NotContains(t, sql, "'"+nonMember+"'")
	}

	// 取代关系与门二 B 裁定写入文件头注释。
	require.Contains(t, sql, "288 取代 278")
	require.Contains(t, sql, "门二 B")
	require.Contains(t, sql, "domain.IsConcretePlatform")
}
