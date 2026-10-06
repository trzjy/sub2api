package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMigration270SupplyOrderSnapshot 守护 D6E-02R #3 外审 addendum 整改的快照三列契约：
//   - 三列以可空、无 DEFAULT 的 ADD COLUMN IF NOT EXISTS 加入（废除零默认）；
//   - 回填从商品目录同源表达式（groups + subscription_plans 单价子查询）确定性写入；
//   - 两级失败关闭均 RAISE EXCEPTION（前置非数字 goods_no、后置空名/零价）；
//   - 回填校验后三列 SET NOT NULL；
//   - down 成对存在且含三列 DROP COLUMN IF EXISTS。
func TestMigration270SupplyOrderSnapshot(t *testing.T) {
	content, err := FS.ReadFile("270_xianguanjia_supply_order_snapshot.sql")
	require.NoError(t, err)
	sql := string(content)

	// 1) 三列可空加列，且不得出现零默认（DEFAULT '' / DEFAULT 0）。
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS goods_name")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS unit_price")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS order_amount")
	require.NotContains(t, sql, "DEFAULT ''", "废除零默认：goods_name 不得带 DEFAULT ''")
	require.NotContains(t, sql, "DEFAULT 0", "废除零默认：数值列不得带 DEFAULT 0")
	require.NotContains(t, sql, "NOT NULL DEFAULT", "废除零默认：加列阶段不得出现 NOT NULL DEFAULT")

	// 2) 回填 UPDATE 来自商品目录同源表达式：FROM "groups" 且含 subscription_plans 单价子查询。
	require.Contains(t, sql, `UPDATE xianguanjia_supply_orders o`, "回填须以订单行为目标")
	require.Contains(t, sql, `FROM "groups" g`, "回填须以 groups 为权威源")
	require.Contains(t, sql, "subscription_plans sp", "回填须含 subscription_plans 单价子查询")
	require.Contains(t, sql, "g.deleted_at IS NULL", "回填 JOIN 条件须排除已删除分组")
	require.Contains(t, sql, "g.id = o.goods_no::bigint", "回填 JOIN 条件须 goods_no 十进制串定位 groups.id")
	require.Contains(t, sql, "o.goods_name IS NULL", "回填仅针对尚未填充的既有行")
	require.Contains(t, sql, "order_amount", "回填须落 order_amount 快照")

	// 3) 两级失败关闭：两处 RAISE EXCEPTION，且消息可定位（行数 + 样例 goods_no）。
	require.Contains(t, sql, "RAISE EXCEPTION", "须存在失败关闭异常")
	// 前置：非纯数字 goods_no。
	require.Contains(t, sql, `goods_no !~ '^[0-9]+$'`, "前置校验须拦截非数字 goods_no")
	require.Contains(t, sql, "无法从商品目录推导商品源", "前置校验异常须说明无法推导商品源")
	// 后置：空名 / 零价 / 零负金额。order_amount 必须显式覆盖 NULL（SQL 中 NULL<=0 不为 TRUE）。
	require.Contains(t, sql, "goods_name IS NULL OR unit_price IS NULL OR unit_price <= 0", "后置校验须拦截空名或零价")
	require.Contains(t, sql, "order_amount IS NULL OR order_amount <= 0", "后置校验须拦截零/负金额快照（含 NULL）")
	require.Contains(t, sql, "绝不为 0", "后置校验异常须声明快照金额绝不为 0")
	require.Contains(t, sql, "禁止以 0 快照放行", "后置校验异常须说明堵资金口子")

	// 4) 三列 SET NOT NULL（无默认）。
	require.Contains(t, sql, "ALTER COLUMN goods_name  SET NOT NULL")
	require.Contains(t, sql, "ALTER COLUMN unit_price   SET NOT NULL")
	require.Contains(t, sql, "ALTER COLUMN order_amount SET NOT NULL")

	// 5) down 成对存在且含三列 DROP COLUMN IF EXISTS。
	down, err := FS.ReadFile("270_xianguanjia_supply_order_snapshot.down.sql")
	require.NoError(t, err, "270 须成对提供 down 迁移")
	downUpper := strings.ToUpper(string(down))
	require.Contains(t, downUpper, "DROP COLUMN IF EXISTS GOODS_NAME")
	require.Contains(t, downUpper, "DROP COLUMN IF EXISTS UNIT_PRICE")
	require.Contains(t, downUpper, "DROP COLUMN IF EXISTS ORDER_AMOUNT")
}
