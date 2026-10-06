package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPromoIntelMigration 校验 247 迁移的关键 DDL：
// 两表结构、唯一去重约束、轮询扫描索引与降级补跑语义列。
func TestPromoIntelMigration(t *testing.T) {
	content, err := FS.ReadFile("247_promo_intel.sql")
	require.NoError(t, err)
	sql := strings.Join(strings.Fields(string(content)), " ")

	// 资讯源表：轮询配置 + 已整理指纹 + 幂等补种唯一名。
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS promo_intel_sources")
	require.Contains(t, sql, "fetch_interval_minutes INTEGER NOT NULL DEFAULT 1440")
	require.Contains(t, sql, "last_extracted_hash VARCHAR(64) NOT NULL DEFAULT ''")
	require.Contains(t, sql, "CREATE UNIQUE INDEX IF NOT EXISTS idx_promo_intel_sources_name")
	require.Contains(t, sql, "ON promo_intel_sources (name)")
	require.Contains(t, sql,
		"CREATE INDEX IF NOT EXISTS idx_promo_intel_sources_enabled_last_fetched ON promo_intel_sources (enabled, last_fetched_at)")

	// 情报条目表：结构化字段 + 降级原文 + 简报日期。
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS promo_intel_items")
	require.Contains(t, sql, "source_id BIGINT NOT NULL REFERENCES promo_intel_sources(id) ON DELETE CASCADE")
	require.Contains(t, sql, "relevance VARCHAR(16) NOT NULL DEFAULT 'medium'")
	require.Contains(t, sql, "status VARCHAR(16) NOT NULL DEFAULT 'pending'")
	require.Contains(t, sql, "extract_status VARCHAR(16) NOT NULL DEFAULT 'llm'")
	require.Contains(t, sql, "digest_date DATE")

	// 去重与查询索引。
	require.Contains(t, sql, "CREATE UNIQUE INDEX IF NOT EXISTS idx_promo_intel_items_fingerprint")
	require.Contains(t, sql, "ON promo_intel_items (fingerprint)")
	require.Contains(t, sql,
		"CREATE INDEX IF NOT EXISTS idx_promo_intel_items_status_digest ON promo_intel_items (status, digest_date DESC, id DESC)")

	// 不应触碰任何既有表。
	require.NotContains(t, sql, "ALTER TABLE users")
	require.NotContains(t, sql, "ALTER TABLE accounts")
}
