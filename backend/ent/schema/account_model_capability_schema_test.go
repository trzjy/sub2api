package schema

import (
	"testing"

	"entgo.io/ent/entc/load"
	"github.com/stretchr/testify/require"
)

// TestAccountModelCapabilitySchema 校验能力标记表 schema：字段齐全、唯一约束为
// (account_id, upstream_model, protocol)。
func TestAccountModelCapabilitySchema(t *testing.T) {
	spec, err := (&load.Config{Path: "."}).Load()
	require.NoError(t, err)

	schemas := map[string]*load.Schema{}
	for _, schema := range spec.Schemas {
		schemas[schema.Name] = schema
	}

	capability := requireSchema(t, schemas, "AccountModelCapability")

	requireSchemaFields(t, capability,
		"account_id",
		"upstream_model",
		"protocol",
		"supports_vision",
		"source",
		"detected_at",
		"updated_at",
	)

	// 唯一约束：(account_id, upstream_model, protocol)
	requireHasUniqueIndex(t, capability, "account_id", "upstream_model", "protocol")

	// account_id 为整型
	accountIDField := requireSchemaField(t, capability, "account_id")
	require.Equal(t, "int64", accountIDField.Info.Type.String())
}
