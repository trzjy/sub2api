package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMigration268SupplyOrders 守护 D6d 货源订单表的结构契约：
// manager_order_no 唯一（幂等锚点）、card_nos jsonb、status/refunded_at 齐备，
// 且 down 脚本成对存在（仓库 .down.sql 惯例）。
func TestMigration268SupplyOrders(t *testing.T) {
	content, err := FS.ReadFile("269_xianguanjia_supply_orders.sql")
	require.NoError(t, err)
	sql := string(content)

	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS xianguanjia_supply_orders")
	require.Contains(t, sql, "manager_order_no TEXT NOT NULL")
	require.Contains(t, sql, "UNIQUE (manager_order_no)", "幂等锚点：管家订单号必须唯一")
	require.Contains(t, sql, "card_nos         JSONB NOT NULL")
	require.Contains(t, sql, "status           INT NOT NULL")
	require.Contains(t, sql, "refunded_at      TIMESTAMPTZ NULL")
	require.Contains(t, sql, "created_at       TIMESTAMPTZ NOT NULL")

	down, err := FS.ReadFile("269_xianguanjia_supply_orders.down.sql")
	require.NoError(t, err, "268 须成对提供 down 迁移")
	require.Contains(t, strings.ToUpper(string(down)), "DROP TABLE IF EXISTS XIANGUANJIA_SUPPLY_ORDERS")
}
