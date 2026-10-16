package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUserPlatformQuotasPurgeUnlimitedMigration 校验 282 号迁移只删除「软删且三档限额全为
// NULL」的行。
//
// 2026-10-16 修订：原实现只按三档 limit 全空删除活跃行，在本仓库是数据破坏 —— 生产
// user_platform_quotas 存量 2578 行三限全空（100%），且其中 16 行同时带有非零累计用量
// （该表除 limit 外还承载 *_usage_usd / *_window_start）。缺行时
// IncrementUsageWithReset 不建行、BatchSnapshotUsage 跳过不存在的行，删行会让这些
// user×platform 的用量累计静默停止，因此改为只清理所有读路径都不可见的软删行。
// 详见 SQL 文件头注释。
func TestUserPlatformQuotasPurgeUnlimitedMigration(t *testing.T) {
	content, err := FS.ReadFile("282_purge_unlimited_user_platform_quotas.sql")
	require.NoError(t, err)

	// 去掉注释行后只允许这一条 DELETE。
	var stmts []string
	for _, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		stmts = append(stmts, trimmed)
	}
	sql := strings.Join(stmts, " ")
	require.Equal(t, "DELETE FROM user_platform_quotas WHERE deleted_at IS NOT NULL AND daily_limit_usd IS NULL AND weekly_limit_usd IS NULL AND monthly_limit_usd IS NULL;", sql)

	// 删除条件必须带软删守卫：漏掉 deleted_at IS NOT NULL 会退化成删活跃行的数据破坏行为。
	require.Contains(t, sql, "deleted_at IS NOT NULL")
}

// TestPurgeUnlimitedQuotaRowsNeverTouchesActiveRows 断言 282 的 WHERE 无法命中任何活跃行：
// 三限全空是「未配置限额」的既有表示，但活跃行仍承载累计用量与窗口起点，不可删除。
func TestPurgeUnlimitedQuotaRowsNeverTouchesActiveRows(t *testing.T) {
	content, err := FS.ReadFile("282_purge_unlimited_user_platform_quotas.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")

	// 守卫子句存在，且顺序上先于三档 limit 判定（顺序不影响语义，此处防的是守卫被删）。
	require.Contains(t, sql, "WHERE deleted_at IS NOT NULL")
	// 不得出现任何"删活跃行"的变体：软删守卫被移除后剩下的就是原上游语句。
	require.NotContains(t, sql, "DELETE FROM user_platform_quotas WHERE daily_limit_usd IS NULL")
	// 三限判定仍保留（只清理三限全空的软删行，带限额的软删行也要留）。
	for _, col := range []string{"daily_limit_usd IS NULL", "weekly_limit_usd IS NULL", "monthly_limit_usd IS NULL"} {
		require.Contains(t, sql, col)
	}
}
