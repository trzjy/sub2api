package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAccountCredentialsMACColumnsMigration 校验 246 号迁移为凭证静态加密（E2）
// 增加两个 HMAC 指纹列。列缺失时，写路径维护指纹、SQL 级 CAS 守卫与 Ollama
// 用量分组的指纹比对都会直接报列不存在，属于必须 fail-fast 的硬依赖。
func TestAccountCredentialsMACColumnsMigration(t *testing.T) {
	content, err := FS.ReadFile("246_account_credentials_mac_columns.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")

	require.Contains(t, sql, "ALTER TABLE accounts ADD COLUMN IF NOT EXISTS credentials_mac text")
	require.Contains(t, sql, "ALTER TABLE accounts ADD COLUMN IF NOT EXISTS credentials_api_key_mac text")
}
