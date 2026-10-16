package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestOpenCodeGoPlatformMigration 校验 281 号迁移的四处约束重建。
//
// 2026-10-16 修订：原实现把 user_platform_quotas / composite_model_routes 的白名单写成上游
// 10 平台（漏 codebuddy 与 other），ADD CONSTRAINT 全表校验时被生产存量的 465 行
// （codebuddy 232 + other 233）拒绝 → 迁移失败、容器起不来。现改为与
// 288_platform_check_constraints_superset.sql 逐字符一致的 16 平台超集。
func TestOpenCodeGoPlatformMigration(t *testing.T) {
	content, err := FS.ReadFile("281_opencode_go_platform.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	_ = sql
	require.Contains(t, sql, "user_platform_quotas_platform_check")
	require.Contains(t, sql, "composite_model_routes_target_platform_check")
	require.Contains(t, sql, "channel_monitors_provider_check")
	require.Contains(t, sql, "channel_monitor_request_templates_provider_check")
	require.Contains(t, sql, "'opencode_go'")
	require.Contains(t, sql, "'minimax'")

	// 渠道监控两表：逐成员核对守卫取代了原来的「只看 opencode_go」单点判断，
	// 上一轮留下的部分正确状态也能收敛到目标定义。
	require.Contains(t, sql, "monitor_needs_rebuild")
	require.Contains(t, sql, "template_needs_rebuild")
	require.Contains(t, sql, "position(required_member IN monitor_constraint_def) = 0")
	require.Contains(t, sql, "position(required_member IN template_constraint_def) = 0")
}

// TestOpenCodeGoQuotaChecksAreSuperset 断言两张配额表的 CHECK 是 16 平台超集，
// 且与 288 完全一致（288 再次执行只会 DROP 后加回同一份定义，不产生分叉）。
func TestOpenCodeGoQuotaChecksAreSuperset(t *testing.T) {
	raw281, err := FS.ReadFile("281_opencode_go_platform.sql")
	require.NoError(t, err)
	raw288, err := FS.ReadFile("288_platform_check_constraints_superset.sql")
	require.NoError(t, err)

	sql281 := strings.Join(strings.Fields(string(raw281)), " ")
	sql288 := strings.Join(strings.Fields(string(raw288)), " ")

	members := []string{
		"anthropic", "openai", "gemini", "antigravity", "grok",
		"kimi", "zhipu", "deepseek", "minimax", "other", "codebuddy", "muse",
		"opencode_go", "typesafe", "command_code", "cline",
	}
	joined := "'" + strings.Join(members, "', '") + "'"

	quotaCheck := "CHECK (platform IN (" + joined + "))"
	routeCheck := "CHECK (target_platform IN (" + joined + "))"
	require.Contains(t, sql281, quotaCheck)
	require.Contains(t, sql281, routeCheck)

	// 与 288 逐字符一致：281 先落 16 平台超集，288 再重建一次得到同一结果。
	require.Contains(t, sql288, quotaCheck)
	require.Contains(t, sql288, routeCheck)

	// fork 侧自研平台必须在集内 —— 这正是原实现漏掉、导致生产部署失败的两个平台。
	require.Contains(t, sql281, "'codebuddy'")
	require.Contains(t, sql281, "'other'")

	// 非 member 仍拒写。
	for _, nonMember := range []string{"future_platform", "bogus", "moonshot", "composite"} {
		require.NotContains(t, sql281, "'"+nonMember+"'")
	}
}

// TestOpenCodeGoChannelMonitorChecksCoverProductionAndHandler 断言渠道监控两表的 provider
// 白名单覆盖「生产现行约束 ∪ admin handler oneof」的并集：
// 生产现行含 codebuddy（281 原版会把它删掉），handler oneof 含 muse/opencode_go
// （281 原版缺），任一方向漏配都会让线上写入失败或 handler 放行但 DB 拒收。
func TestOpenCodeGoChannelMonitorChecksCoverProductionAndHandler(t *testing.T) {
	content, err := FS.ReadFile("281_opencode_go_platform.sql")
	require.NoError(t, err)

	// 生产 pg_constraint 实读（2026-10-16）10 项 + handler oneof 的 muse/opencode_go。
	productionMembers := []string{
		"openai", "anthropic", "gemini", "grok", "antigravity",
		"kimi", "zhipu", "deepseek", "minimax", "codebuddy",
	}
	handlerOnlyMembers := []string{"muse", "opencode_go"}

	expectedList := "'" + strings.Join(append(append([]string{}, productionMembers...), handlerOnlyMembers...), "', '") + "'"

	// 只断言 provider CHECK 自身的成员集：typesafe/command_code/cline 合法出现在同文件的
	// 配额 CHECK（16 平台超集）里，所以负向断言必须限定在 provider 子句，不能扫整份 SQL。
	// 正向断言也只看可执行 SQL，避免文件头注释里列举平台名造成假失败。
	execSQL := strings.Join(strings.Fields(stripSQLComments(string(content))), " ")
	providerChecks := extractProviderChecks(t, execSQL)
	require.Len(t, providerChecks, 2, "channel_monitors 与 channel_monitor_request_templates 各一处 provider CHECK")

	for _, check := range providerChecks {
		require.Equal(t, expectedList, check, "provider CHECK 成员集必须是「生产现行 ∪ handler oneof」")
	}
	// typesafe/command_code/cline 无渠道监控探针实现支撑，不得放行（由上面的等值断言保证，
	// 此处显式再钉一次，防止有人把 expectedList 改成超集后无人察觉）。
	for _, unsupported := range []string{"typesafe", "command_code", "cline"} {
		require.NotContains(t, strings.Join(providerChecks, " "), "'"+unsupported+"'")
	}
}

// stripSQLComments 去掉整行 `--` 注释，保留可执行 SQL（含 DO $$ 块内的内容）。
func stripSQLComments(content string) string {
	var kept []string
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		kept = append(kept, trimmed)
	}
	return strings.Join(kept, "\n")
}

// extractProviderChecks 从可执行 SQL 中取出每个 CHECK (provider IN (...)) 的成员列表文本。
func extractProviderChecks(t *testing.T, execSQL string) []string {
	t.Helper()
	const openMarker = "CHECK (provider IN ("
	var out []string
	for {
		i := strings.Index(execSQL, openMarker)
		if i < 0 {
			return out
		}
		rest := execSQL[i+len(openMarker):]
		j := strings.Index(rest, "))")
		require.GreaterOrEqual(t, j, 0, "provider CHECK 未闭合")
		out = append(out, rest[:j])
		execSQL = rest[j+2:]
	}
}
