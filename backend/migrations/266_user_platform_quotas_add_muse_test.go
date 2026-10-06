package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUserPlatformQuotasMuseMigration 校验 266 号迁移把 Muse 平台加入
// user_platform_quotas.platform 的 CHECK 约束（对照 224/236/237/244 同型事故）。
// 约束未放宽时，注册预填充默认配额会整条 INSERT 中止 → 新用户零配额行。
func TestUserPlatformQuotasMuseMigration(t *testing.T) {
	content, err := FS.ReadFile("266_user_platform_quotas_add_muse.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check")
	require.Contains(t, sql,
		"CHECK (platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'grok', "+
			"'kimi', 'zhipu', 'deepseek', 'minimax', 'other', 'codebuddy', 'muse'))")
}
