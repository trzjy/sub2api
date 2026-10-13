package schema

import (
	"testing"

	"entgo.io/ent/entc/load"
	"github.com/stretchr/testify/require"
)

// loadAccountSchema 加载 Account schema 的 load.Schema 表示（v3.3 §4.2 / C2）。
// 与 quota_key_schema_test.go 同构：通过 AST 解析拿到完整 schema 元数据，
// 校验本次 C2 新增的 quota_key_id 字段与 quota_key 反向边声明。
func loadAccountSchema(t *testing.T) *load.Schema {
	t.Helper()
	spec, err := (&load.Config{Path: "."}).Load()
	require.NoError(t, err)
	for _, s := range spec.Schemas {
		if s.Name == "Account" {
			return s
		}
	}
	t.Fatal("Account schema not found")
	return nil
}

// TestAccountSchemaQuotaKeyField 校验 v3.3 §4.2 新增字段 quota_key_id。
// 字段存在且为 int64（与 C1 quota_keys.id 外键列对齐）。
func TestAccountSchemaQuotaKeyField(t *testing.T) {
	account := loadAccountSchema(t)
	f := requireSchemaField(t, account, "quota_key_id")
	require.Equal(t, "int64", f.Info.Type.String(), "quota_key_id must be int64")
}

// TestAccountSchemaQuotaKeyEdge 校验 v3.3 §4.2 反向边 quota_key。
// 与 C1 quota_key.go 的 edge.To("accounts", Account.Type) 配对：
//   - Name == "quota_key"
//   - Type == "QuotaKey"（反向到 C1 激活码 schema）
//   - Field == "quota_key_id"（外键列）
//   - Unique == true（1:N 反向边唯一）
func TestAccountSchemaQuotaKeyEdge(t *testing.T) {
	account := loadAccountSchema(t)

	var edge *load.Edge
	for i := range account.Edges {
		if account.Edges[i].Name == "quota_key" {
			edge = account.Edges[i]
			break
		}
	}
	require.NotNil(t, edge, "Account must declare reverse edge 'quota_key'")

	require.Equal(t, "QuotaKey", edge.Type, "reverse edge must point to QuotaKey schema")
	require.Equal(t, "quota_key_id", edge.Field, "reverse edge must bind to fk column quota_key_id")
	require.True(t, edge.Unique, "reverse edge (1:N) must be Unique")
}
