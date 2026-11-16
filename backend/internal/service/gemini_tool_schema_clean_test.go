package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCleanToolSchema_RemovesNestedPropertyNamesAtAllLevels(t *testing.T) {
	schema := map[string]any{
		"type":          "object",
		"propertyNames": map[string]any{"pattern": "^root"},
		"properties": map[string]any{
			// 键名叫 propertyNames 的属性本身不是 JSON Schema 关键字，应保留。
			"propertyNames": map[string]any{"type": "string"},
			"nested": map[string]any{
				"type":          "object",
				"propertyNames": map[string]any{"pattern": "^nested"},
				"items": map[string]any{
					"type":          "array",
					"propertyNames": map[string]any{"const": "inside items"},
				},
				"anyOf": []any{
					map[string]any{
						"type":          "string",
						"propertyNames": map[string]any{"pattern": "^branch"},
					},
					map[string]any{"type": "number"},
				},
			},
		},
	}

	cleaned, ok := cleanToolSchema(schema).(map[string]any)
	require.True(t, ok)
	require.NotContains(t, cleaned, "propertyNames")

	properties, ok := cleaned["properties"].(map[string]any)
	require.True(t, ok)
	require.Contains(t, properties, "propertyNames", "property name must survive keyword deletion")

	nested, ok := properties["nested"].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, nested, "propertyNames")

	items, ok := nested["items"].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, items, "propertyNames")

	anyOf, ok := nested["anyOf"].([]any)
	require.True(t, ok)
	require.Len(t, anyOf, 2)
	firstBranch, ok := anyOf[0].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, firstBranch, "propertyNames")
	require.Equal(t, "STRING", firstBranch["type"])
	secondBranch, ok := anyOf[1].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, secondBranch, "propertyNames")
}

func TestCleanToolSchema_ConvertsPrefixItemsToItemsWithBestObjectBranch(t *testing.T) {
	for _, tc := range []struct {
		name     string
		hasItems bool
		items    any
	}{
		{name: "missing items"},
		{name: "nil items", hasItems: true, items: nil},
		{name: "boolean items", hasItems: true, items: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := map[string]any{
				"type": "array",
				"prefixItems": []any{
					map[string]any{"type": "string"}, // 1 分
					map[string]any{ // 2 分
						"type":  "array",
						"items": map[string]any{"type": "integer"},
					},
					map[string]any{ // 3 分：object 分支应胜出
						"type": "object",
						"properties": map[string]any{
							"id": map[string]any{"type": "integer"},
						},
						"propertyNames": map[string]any{"pattern": "^id$"},
						"deprecated":    true,
					},
				},
			}
			if tc.hasItems {
				schema["items"] = tc.items
			}

			cleaned, ok := cleanToolSchema(schema).(map[string]any)
			require.True(t, ok)
			require.NotContains(t, cleaned, "prefixItems")

			items, ok := cleaned["items"].(map[string]any)
			require.True(t, ok)
			require.Equal(t, "OBJECT", items["type"])
			properties, ok := items["properties"].(map[string]any)
			require.True(t, ok)
			require.Contains(t, properties, "id")
			// 选出的最佳元素本身也必须先被清理干净。
			require.NotContains(t, items, "propertyNames")
			require.NotContains(t, items, "deprecated")
		})
	}
}

func TestCleanToolSchema_FallsBackToStringWhenPrefixItemsAllZero(t *testing.T) {
	schema := map[string]any{
		"type": "array",
		"prefixItems": []any{
			map[string]any{"type": "null"},
			map[string]any{"unknown": true},
		},
	}

	cleaned, ok := cleanToolSchema(schema).(map[string]any)
	require.True(t, ok)
	require.NotContains(t, cleaned, "prefixItems")
	require.Equal(t, map[string]any{"type": "STRING"}, cleaned["items"])
}

func TestCleanToolSchema_KeepsExistingObjectItemsWhenPrefixItemsPresent(t *testing.T) {
	schema := map[string]any{
		"type":  "array",
		"items": map[string]any{"type": "string", "deprecated": true},
		"prefixItems": []any{
			map[string]any{
				"type":       "object",
				"properties": map[string]any{"id": map[string]any{"type": "integer"}},
			},
		},
	}

	cleaned, ok := cleanToolSchema(schema).(map[string]any)
	require.True(t, ok)
	require.NotContains(t, cleaned, "prefixItems")

	items, ok := cleaned["items"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "STRING", items["type"], "existing object items must not be replaced")
	require.NotContains(t, items, "deprecated")
}

func TestCleanToolSchema_KeepsAnyOfBranchCleanupBehaviorWithPrefixItemsKeywords(t *testing.T) {
	schema := map[string]any{
		"anyOf": []any{
			map[string]any{
				"type":          "string",
				"deprecated":    true,
				"propertyNames": map[string]any{"pattern": "^a"},
			},
			map[string]any{
				"enum":          []any{"enabled", false, float64(1), nil},
				"prefixItems":   []any{map[string]any{"type": "string"}},
				"propertyNames": map[string]any{"pattern": "^b"},
			},
		},
	}

	cleaned, ok := cleanToolSchema(schema).(map[string]any)
	require.True(t, ok)
	require.NotContains(t, cleaned, "propertyNames")

	anyOf, ok := cleaned["anyOf"].([]any)
	require.True(t, ok)
	require.Len(t, anyOf, 2)

	firstBranch, ok := anyOf[0].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, firstBranch, "deprecated")
	require.NotContains(t, firstBranch, "propertyNames")
	require.Equal(t, "STRING", firstBranch["type"])

	secondBranch, ok := anyOf[1].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, secondBranch, "prefixItems")
	require.NotContains(t, secondBranch, "propertyNames")
	require.Equal(t, []any{"enabled", "false", "1", "null"}, secondBranch["enum"])
}
