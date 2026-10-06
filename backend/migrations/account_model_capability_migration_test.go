package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAccountModelCapabilityMigrationCreatesTable 校验 259 迁移文件包含能力标记表且全幂等，
// 字段/唯一约束/来源与协议 CHECK/级联外键齐全，且绝对不触碰 accounts 表本体。
func TestAccountModelCapabilityMigrationCreatesTable(t *testing.T) {
	content, err := FS.ReadFile("259_account_model_capabilities.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")

	// 幂等建表
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS account_model_capabilities")

	// 全部字段
	require.Contains(t, sql, "id BIGSERIAL PRIMARY KEY")
	require.Contains(t, sql, "account_id BIGINT NOT NULL")
	require.Contains(t, sql, "upstream_model VARCHAR(200) NOT NULL")
	require.Contains(t, sql, "protocol VARCHAR(50) NOT NULL")
	require.Contains(t, sql, "supports_vision BOOLEAN NOT NULL DEFAULT FALSE")
	require.Contains(t, sql, "source VARCHAR(20) NOT NULL DEFAULT 'detect'")
	require.Contains(t, sql, "detected_at TIMESTAMPTZ NULL")
	require.Contains(t, sql, "updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()")

	// 唯一约束 (account_id, upstream_model, protocol)
	require.Contains(t, sql, "UNIQUE (account_id, upstream_model, protocol)")

	// 来源/协议 CHECK 约束
	require.Contains(t, sql, "CHECK (source IN ('detect', 'manual'))")
	require.Contains(t, sql, "CHECK (protocol IN ('chat_completions', 'responses', 'anthropic'))")

	// 级联外键（账号删除时清理标记）
	require.Contains(t, sql, "FOREIGN KEY (account_id) REFERENCES accounts (id) ON DELETE CASCADE")

	// 查询索引
	require.Contains(t, sql, "CREATE INDEX IF NOT EXISTS idx_account_model_capabilities_account")
}

// TestAccountModelCapabilityMigrationDoesNotTouchAccounts 断言迁移只建新表，
// 不 ALTER 任何已有表（accounts 等业务表均不得被触碰）。
func TestAccountModelCapabilityMigrationDoesNotTouchExistingTables(t *testing.T) {
	content, err := FS.ReadFile("259_account_model_capabilities.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	upper := strings.ToUpper(sql)

	// 唯一允许出现的 ALTER 是针对新表加外键（幂等保护内）
	require.NotContains(t, upper, "ALTER TABLE ACCOUNTS")
	require.NotContains(t, upper, "DROP TABLE")
	require.NotContains(t, upper, "TRUNCATE")
}
