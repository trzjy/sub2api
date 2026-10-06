package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUserPlatformQuotasOtherMigration 校验 236 号迁移把通用平台 other 加入
// user_platform_quotas.platform 的 CHECK 约束（对照 157/224 同型事故）。
// 约束未放宽时，注册预填充 9 平台默认配额会整条 INSERT 中止 → 新用户零配额行。
func TestUserPlatformQuotasOtherMigration(t *testing.T) {
	content, err := FS.ReadFile("236_user_platform_quotas_add_other.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check")
	require.Contains(t, sql,
		"CHECK (platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'grok', 'kimi', 'zhipu', 'deepseek', 'other'))")
}
