package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCodeBuddyPlatformMigration 校验 244 号迁移把 codebuddy 加入 4 处 platform CHECK
// 约束（user_platform_quotas / composite_model_routes / channel_monitors /
// channel_monitor_request_templates）。约束未放宽时，GetDefaultPlatformQuotas 跟随
// AllowedQuotaPlatforms 预填充的 codebuddy 配额行会整条 INSERT 中止 → 新用户零配额行
// （缺失配额行 = 无限额），管理端设置 codebuddy 平台配额直接 500。
func TestCodeBuddyPlatformMigration(t *testing.T) {
	content, err := FS.ReadFile("244_user_platform_quotas_add_codebuddy.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")

	// 1. user_platform_quotas.platform
	require.Contains(t, sql, "DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check")
	require.Contains(t, sql,
		"CHECK (platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'grok', 'kimi', 'zhipu', 'deepseek', 'minimax', 'other', 'codebuddy'))")

	// 2. composite_model_routes.target_platform
	require.Contains(t, sql, "DROP CONSTRAINT IF EXISTS composite_model_routes_target_platform_check")
	require.Contains(t, sql,
		"CHECK (target_platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'grok', 'kimi', 'zhipu', 'deepseek', 'minimax', 'codebuddy'))")

	// 3. channel_monitors.provider
	require.Contains(t, sql,
		"CHECK (provider IN ('openai', 'anthropic', 'gemini', 'grok', 'antigravity', 'kimi', 'zhipu', 'deepseek', 'minimax', 'codebuddy'))")

	// 4. channel_monitor_request_templates.provider
	require.Contains(t, sql,
		"CHECK (provider IN ('openai', 'anthropic', 'gemini', 'grok', 'antigravity', 'kimi', 'zhipu', 'deepseek', 'minimax', 'codebuddy'))")
}
