package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWelfareRedeemCardsMigration 校验 245 号迁移的关键结构：
// 批次表、batch_id 列、同批次限兑一张的部分唯一索引、一卡多分组表、
// 独立福利余额池。福利余额清零只动 welfare_balances，约束本迁移不触碰
// users.balance（用户充值余额不受福利清零影响是硬需求）。
func TestWelfareRedeemCardsMigration(t *testing.T) {
	content, err := FS.ReadFile("245_welfare_redeem_cards.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")

	// 批次元数据表
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS redeem_batches")

	// redeem_codes.batch_id
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS batch_id BIGINT")

	// 同批次每人限兑一张：部分唯一索引（仅 welfare 卡有 batch_id，旧数据全 NULL 不受影响）
	require.Contains(t, sql, "CREATE UNIQUE INDEX IF NOT EXISTS idx_redeem_codes_batch_one_per_user")
	require.Contains(t, sql, "ON redeem_codes (batch_id, used_by)")
	require.Contains(t, sql, "WHERE batch_id IS NOT NULL AND used_by IS NOT NULL")

	// 一卡多分组勾选快照
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS redeem_code_groups")
	require.Contains(t, sql, "validity_days")

	// 独立福利余额池
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS welfare_balances")
	require.Contains(t, sql, "amount_remaining NUMERIC(20,8)")
	require.Contains(t, sql, "expires_at TIMESTAMPTZ NOT NULL")

	// 迁移本身不得更新 users 表（清零逻辑只允许动 welfare_balances）
	require.NotContains(t, sql, "UPDATE users")
	require.NotContains(t, sql, "ALTER TABLE users")
}
