package schema

import (
	"testing"

	"entgo.io/ent/entc/load"
	"github.com/stretchr/testify/require"
)

// loadQuotaKeySchema 加载 QuotaKey schema 的 load.Schema 表示。
// ent 的 Schema 接口未公开 Name() / Fields() 等元数据，但 load.Config.Load()
// 通过 AST 解析拿到完整 schema 信息，绕过 ent 私有接口限制。
func loadQuotaKeySchema(t *testing.T) *load.Schema {
	t.Helper()
	spec, err := (&load.Config{Path: "."}).Load()
	require.NoError(t, err)
	for _, s := range spec.Schemas {
		if s.Name == "QuotaKey" {
			return s
		}
	}
	t.Fatal("QuotaKey schema not found")
	return nil
}

// TestQuotaKeySchemaFields 验证 v3.3 §4.1 字段清单全部声明。
// 字段：code_encrypted / code_mac / code_last4 / status / exhausted /
// last_refresh_error / last_refresh_at / expires_at / created_at / updated_at / notes
// （11 字段）
func TestQuotaKeySchemaFields(t *testing.T) {
	quotaKey := loadQuotaKeySchema(t)
	requireSchemaFields(t, quotaKey,
		"code_encrypted", "code_mac", "code_last4",
		"status", "exhausted", "last_refresh_error",
		"last_refresh_at", "expires_at",
		"created_at", "updated_at", "notes",
	)
}

// TestQuotaKeySchemaTypes 验证关键字段类型（v3.3 §4.1 不可变契约）。
//   - code_encrypted: string
//   - code_mac:       string
//   - code_last4:     string
//   - status:         string
//   - exhausted:      bool
func TestQuotaKeySchemaTypes(t *testing.T) {
	quotaKey := loadQuotaKeySchema(t)

	cases := []struct {
		name     string
		typeName string
	}{
		{"code_encrypted", "string"},
		{"code_mac", "string"},
		{"code_last4", "string"},
		{"status", "string"},
		{"exhausted", "bool"},
	}
	for _, tc := range cases {
		f := requireSchemaField(t, quotaKey, tc.name)
		require.Equal(t, tc.typeName, f.Info.Type.String(), "field %s type must be %s", tc.name, tc.typeName)
	}
}

// TestQuotaKeySchemaUniqueIndex 验证 code_mac 唯一索引（v3.3 §4.1）。
func TestQuotaKeySchemaUniqueIndex(t *testing.T) {
	quotaKey := loadQuotaKeySchema(t)

	hasUnique := false
	for _, idx := range quotaKey.Indexes {
		if idx.Unique && len(idx.Fields) == 1 && idx.Fields[0] == "code_mac" {
			hasUnique = true
			break
		}
	}
	require.True(t, hasUnique, "quota_keys must have unique index on code_mac")
}

// TestQuotaKeySchemaDeclarations 验证 5 索引（v3.3 §4.1 索引清单）。
// 5 索引：
//   - uq_quota_keys_code_mac (code_mac unique)
//   - idx_quota_keys_status (status)
//   - idx_quota_keys_expires_at (expires_at)
//   - idx_quota_keys_exhausted (exhausted)
//   - idx_quota_keys_reconciler (status, exhausted, expires_at)
func TestQuotaKeySchemaDeclarations(t *testing.T) {
	quotaKey := loadQuotaKeySchema(t)
	require.GreaterOrEqual(t, len(quotaKey.Indexes), 5, "must declare at least 5 indexes (1 unique + 4 non-unique)")

	hasCodeMacIdx := false
	hasStatusIdx := false
	hasExpiresAtIdx := false
	hasExhaustedIdx := false
	hasReconcilerIdx := false
	for _, idx := range quotaKey.Indexes {
		switch len(idx.Fields) {
		case 1:
			switch idx.Fields[0] {
			case "code_mac":
				hasCodeMacIdx = true
			case "status":
				hasStatusIdx = true
			case "expires_at":
				hasExpiresAtIdx = true
			case "exhausted":
				hasExhaustedIdx = true
			}
		case 3:
			if idx.Fields[0] == "status" && idx.Fields[1] == "exhausted" && idx.Fields[2] == "expires_at" {
				hasReconcilerIdx = true
			}
		}
	}
	require.True(t, hasCodeMacIdx, "must have index on code_mac")
	require.True(t, hasStatusIdx, "must have index on status")
	require.True(t, hasExpiresAtIdx, "must have index on expires_at")
	require.True(t, hasExhaustedIdx, "must have index on exhausted")
	require.True(t, hasReconcilerIdx, "must have composite index on (status, exhausted, expires_at)")
}
