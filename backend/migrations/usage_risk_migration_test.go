package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUsageRiskMigrationCreatesTables 校验 257 迁移文件包含三张新表且全幂等，
// 字段/约束/部分索引齐全，且绝对不触碰 usage_logs。
func TestUsageRiskMigrationCreatesTables(t *testing.T) {
	content, err := FS.ReadFile("257_usage_risk_analysis.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")

	// 三张表均幂等创建
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS user_usage_metrics_rollup")
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS usage_risk_reports")
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS usage_risk_runs")

	// reports 主键为 report_id identity
	require.Contains(t, sql, "report_id BIGSERIAL PRIMARY KEY")
	// runs 主键为 run_id identity
	require.Contains(t, sql, "run_id BIGSERIAL PRIMARY KEY")

	// rollup 代理主键 id + 唯一键 (user_id, group_id, bucket_hour)
	require.Contains(t, sql, "id BIGSERIAL PRIMARY KEY")
	require.Contains(t, sql, "CONSTRAINT user_usage_metrics_rollup_unique UNIQUE (user_id, group_id, bucket_hour)")

	// 唯一键约束
	require.Contains(t, sql, "UNIQUE (user_id, group_id, report_date)")
	require.Contains(t, sql, "UNIQUE (run_at)")

	// CHECK 约束（等级 / 状态枚举，reports 与 runs 两处）
	require.Contains(t, sql, "('low', 'medium', 'high', 'critical')")
	require.Contains(t, sql, "('open', 'acknowledged', 'dismissed', 'resolved')")
	require.Contains(t, sql, "('running', 'completed', 'partial')")

	// 部分索引（WHERE invalidated_at IS NULL / status = 'open'）
	require.Contains(t, sql, "WHERE invalidated_at IS NULL")
	require.Contains(t, sql, "WHERE status = 'open' AND invalidated_at IS NULL")

	// 数值精度与 jsonb 默认
	require.Contains(t, sql, "NUMERIC(20,10)")
	require.Contains(t, sql, "DEFAULT '[]'::jsonb")
	require.Contains(t, sql, "DEFAULT '{}'::jsonb")
}

// TestUsageRiskMigrationDoesNotTouchUsageLogs 断言迁移不涉及任何 usage_logs 改动。
func TestUsageRiskMigrationDoesNotTouchUsageLogs(t *testing.T) {
	content, err := FS.ReadFile("257_usage_risk_analysis.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	upper := strings.ToUpper(sql)
	require.NotContains(t, upper, "ALTER TABLE USAGE_LOGS")
	require.NotContains(t, upper, "USAGE_LOGS")
}
