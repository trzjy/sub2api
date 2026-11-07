//go:build unit

package migrations

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMigration297QuotaUpstreams 守护 CARD-KC-16 的上游注册表迁移契约
// （与 296 同风格，不连库，对迁移源码做断言）：
//   - 文件可读（被 embed.FS 嵌入）；
//   - 无裸事务控制语句（BEGIN/COMMIT/ROLLBACK）——迁移由运行器统一管事务；
//   - 建表 quota_upstreams（CREATE TABLE IF NOT EXISTS）；
//   - quota_keys 增列 upstream_key（DEFAULT 'shareai'，IF NOT EXISTS）；
//   - key 唯一索引 uq_quota_upstreams_key；
//   - type CHECK 限定取值集合；
//   - 种子两条上游条目且幂等（ON CONFLICT (key) DO NOTHING）；
//   - 字段/类型/默认值与 ent/schema/quota_upstream.go 逐字对应。
func TestMigration297QuotaUpstreams(t *testing.T) {
	content, err := FS.ReadFile("297_quota_upstreams.sql")
	require.NoError(t, err, "297 迁移文件必须存在且被嵌入（embed.FS 覆盖 *.sql）")
	sql := string(content)

	// 1) 裸事务控制语句：逐行剔除 SQL 注释后再匹配，避免误伤回滚注释里的 BEGIN/COMMIT。
	txStmt := regexp.MustCompile(`(?im)^\s*(BEGIN|COMMIT|ROLLBACK)\s*;`)
	for i, line := range strings.Split(sql, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			continue
		}
		require.False(t, txStmt.MatchString(line),
			"第 %d 行含裸事务控制语句 %q：迁移由运行器统一管事务，文件内不得自带", i+1, trimmed)
	}

	// 2) 建表契约：quota_upstreams 与关键列。
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS quota_upstreams",
		"缺少建表语句：quota_upstreams")
	require.Contains(t, sql, "VARCHAR(64)  NOT NULL", "缺少上游标识 key 列（varchar(64) not null）")
	require.Contains(t, sql, "VARCHAR(100) NOT NULL", "缺少显示名 name 列（varchar(100) not null）")
	require.Contains(t, sql, "VARCHAR(32)  NOT NULL", "缺少类型 type 列（varchar(32) not null）")
	require.Contains(t, sql, "renewal_url  VARCHAR(500)", "缺少续费链接 renewal_url 列（varchar(500)，可空）")
	require.Contains(t, sql, "notes        TEXT", "缺少备注 notes 列（text，可空）")
	require.Contains(t, sql, "deleted_at   TIMESTAMPTZ", "缺少软删 deleted_at 列（SoftDeleteMixin）")

	// 3) key 唯一索引 + type 单索引。
	require.Contains(t, sql, "CREATE UNIQUE INDEX IF NOT EXISTS uq_quota_upstreams_key",
		"缺少 key 唯一索引 uq_quota_upstreams_key")
	require.Contains(t, sql, "CREATE INDEX IF NOT EXISTS idx_quota_upstreams_type",
		"缺少 type 单索引 idx_quota_upstreams_type")

	// 4) type CHECK 限定取值集合（shareai / kn_cy）。
	require.Contains(t, sql, "CHECK (type IN ('shareai', 'kn_cy'))",
		"缺少 type CHECK 约束：需限定取值集合（shareai / kn_cy）")

	// 5) quota_keys 增列（DEFAULT 'shareai'，IF NOT EXISTS，幂等可重入）。
	require.Contains(t, sql, "ALTER TABLE quota_keys ADD COLUMN IF NOT EXISTS upstream_key VARCHAR(64) NOT NULL DEFAULT 'shareai'",
		"缺少 quota_keys.upstream_key 增列（默认 shareai，幂等）")
	require.Contains(t, sql, "CREATE INDEX IF NOT EXISTS idx_quota_keys_upstream_key",
		"缺少 upstream_key 普通索引 idx_quota_keys_upstream_key")

	// 6) 种子两条上游条目且幂等（ON CONFLICT (key) DO NOTHING）。
	require.Contains(t, sql, "INSERT INTO quota_upstreams (key, name, type, renewal_url, created_at, updated_at) VALUES",
		"缺少上游条目种子 INSERT")
	require.Contains(t, sql, "('shareai', 'ShareAI 号池', 'shareai', '', now(), now())",
		"缺少 ShareAI 上游种子")
	require.Contains(t, sql, "('kn_cy', 'kn-cy.com 网关', 'kn_cy', 'https://shop.kn-cy.com/products', now(), now())",
		"缺少 kn-cy 上游种子")
	require.Contains(t, sql, "ON CONFLICT (key) DO NOTHING",
		"种子需幂等：ON CONFLICT (key) DO NOTHING")
}
