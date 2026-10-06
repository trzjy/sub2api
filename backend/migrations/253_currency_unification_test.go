package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCurrencyUnificationMigration 校验 253 号迁移（货币计费统一）的关键结构：
// FX_RATES 种子、payment_orders.currency 列与回填、plan 价格 USD 化两分支、
// 倍率原值迁移（不乘汇率）与旧 key 删除。
func TestCurrencyUnificationMigration(t *testing.T) {
	content, err := FS.ReadFile("253_currency_unification.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")

	// FX_RATES 种子：仅在不存在时插入
	require.Contains(t, sql, "INSERT INTO settings (key, value, updated_at)")
	require.Contains(t, sql, "VALUES ('FX_RATES', jsonb_build_object('CNY', v_fx_cny)::text, NOW())")
	require.Contains(t, sql, "ON CONFLICT (key) DO NOTHING")

	// fx_cny 三级优先级：旧订阅汇率 > 0 → 旧 FX_RATES.CNY → 7.15
	require.Contains(t, sql, "SELECT value INTO v_old_rate FROM settings WHERE key = 'SUBSCRIPTION_USD_TO_CNY_RATE'")
	require.Contains(t, sql, "v_old_rate::numeric > 0")
	require.Contains(t, sql, "SELECT value INTO v_old_fx_rates FROM settings WHERE key = 'FX_RATES'")
	require.Contains(t, sql, "(v_json->>'CNY')::numeric > 0")
	require.Contains(t, sql, "v_fx_cny := 7.15")

	// 订单币种列 + 快照回填
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS currency VARCHAR(3) NOT NULL DEFAULT 'CNY'")
	require.Contains(t, sql, "SET currency = COALESCE(NULLIF(provider_snapshot->>'currency', ''), 'CNY')")

	// plan 价格 USD 化：分支 A（旧 rate>0）仅改标记；分支 B（rate=0）除以 fx_cny 后 ROUND(...,2)
	require.Contains(t, sql, "UPDATE subscription_plans SET currency = 'USD' WHERE COALESCE(currency, '') <> 'USD'")
	require.Contains(t, sql, "SET price = ROUND(price / v_fx_cny, 2),")
	require.Contains(t, sql, "WHEN original_price IS NOT NULL THEN ROUND(original_price / v_fx_cny, 2)")
	require.Contains(t, sql, "currency = 'USD'")

	// 倍率迁移：原值迁移（不乘汇率），写新 key、删旧 key（含订阅汇率 key）
	require.Contains(t, sql, "SELECT 'RECHARGE_MARKUP', value, NOW()")
	require.Contains(t, sql, "FROM settings WHERE key = 'BALANCE_RECHARGE_MULTIPLIER'")
	require.Contains(t, sql, "'BALANCE_RECHARGE_MULTIPLIER',")
	require.Contains(t, sql, "'SUBSCRIPTION_USD_TO_CNY_RATE'")

	// 评审裁定标记：原值迁移不做存量到账保全
	require.Contains(t, sql, "原值迁移")
}

// TestCurrencyUnificationDownMigration 校验 down 脚本只做结构回滚，
// 不含 plan 价格回写（数据回滚不承诺无损，以 pg_dump 备份为唯一手段）。
func TestCurrencyUnificationDownMigration(t *testing.T) {
	content, err := FS.ReadFile("253_currency_unification.down.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "DROP COLUMN IF EXISTS currency")
	require.Contains(t, sql, "DELETE FROM settings WHERE key IN ('FX_RATES', 'RECHARGE_MARKUP')")
	require.NotContains(t, sql, "subscription_plans")
}
