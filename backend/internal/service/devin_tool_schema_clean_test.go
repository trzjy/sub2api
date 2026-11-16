//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// ① 无 tools（甚至非法结构）时原样返回，字节不变。
func TestStripDevinUnsupportedToolSchemaKeywords_NoToolsUnchanged(t *testing.T) {
	bodies := [][]byte{
		[]byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`),
		[]byte(`{"tools":null}`),
		[]byte(`{"tools":"not-an-array"}`),
		[]byte(`not-json-at-all`),
	}
	for _, body := range bodies {
		got := stripDevinUnsupportedToolSchemaKeywords(body)
		require.Equal(t, body, got, "body without a valid tools array must be byte-identical")
	}
}

// ② 含 propertyNames → 输出不含该键（含嵌套任意层级），其余字段保留。
func TestStripDevinUnsupportedToolSchemaKeywords_RemovesPropertyNames(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","function":{"name":"f","parameters":{
		"type":"object",
		"propertyNames":{"pattern":"^root"},
		"properties":{"a":{"type":"string","propertyNames":{"pattern":"^a"}}}
	}}}]}`)

	got := stripDevinUnsupportedToolSchemaKeywords(body)
	require.True(t, gjson.ValidBytes(got))
	require.NotContains(t, string(got), "propertyNames")
	require.True(t, gjson.GetBytes(got, "tools.0.function.parameters.properties.a.type").Exists())
	require.Equal(t, "string", gjson.GetBytes(got, "tools.0.function.parameters.properties.a.type").String())
}

// ③ 含 prefixItems 且无 items → items 取最佳元素（object 优先、type 保持小写），且无 prefixItems。
func TestStripDevinUnsupportedToolSchemaKeywords_PrefixItemsBestElement(t *testing.T) {
	for _, tc := range []struct {
		name      string
		itemsJSON string
	}{
		{name: "missing items", itemsJSON: ""},
		{name: "null items", itemsJSON: `"items":null,`},
		{name: "boolean items", itemsJSON: `"items":false,`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"tools":[{"function":{"parameters":{"type":"array",` + tc.itemsJSON + `
				"prefixItems":[
					{"type":"string"},
					{"type":"array","items":{"type":"integer"}},
					{"type":"object","properties":{"id":{"type":"integer"}},"propertyNames":{"pattern":"^id$"}}
				]
			}}}]}`)

			got := stripDevinUnsupportedToolSchemaKeywords(body)
			require.True(t, gjson.ValidBytes(got))
			s := string(got)
			require.NotContains(t, s, "prefixItems")
			require.NotContains(t, s, "propertyNames", "selected best element must be cleaned too")
			require.NotContains(t, s, "OBJECT", "type must stay lowercase")
			require.Equal(t, "object", gjson.GetBytes(got, "tools.0.function.parameters.items.type").String())
			require.True(t, gjson.GetBytes(got, "tools.0.function.parameters.items.properties.id").Exists())
		})
	}
}

// ③ 续：prefixItems 全 0 分回退到小写 type:"string"；已有合法 items 时保留。
func TestStripDevinUnsupportedToolSchemaKeywords_PrefixItemsFallbackAndKeepItems(t *testing.T) {
	fallback := []byte(`{"tools":[{"function":{"parameters":{"prefixItems":[
		{"type":"null"},{"unknown":true}
	]}}}]}`)
	got := stripDevinUnsupportedToolSchemaKeywords(fallback)
	require.NotContains(t, string(got), "prefixItems")
	require.Equal(t, "string", gjson.GetBytes(got, "tools.0.function.parameters.items.type").String())

	keep := []byte(`{"tools":[{"function":{"parameters":{
		"items":{"type":"number"},
		"prefixItems":[{"type":"object","properties":{"id":{"type":"string"}}}]
	}}}]}`)
	got = stripDevinUnsupportedToolSchemaKeywords(keep)
	require.NotContains(t, string(got), "prefixItems")
	require.Equal(t, "number", gjson.GetBytes(got, "tools.0.function.parameters.items.type").String(),
		"existing usable items must not be replaced")
}

// ④ 嵌套（items.properties 内、anyOf/oneOf/allOf 分支内）同样清理。
func TestStripDevinUnsupportedToolSchemaKeywords_NestedCleanup(t *testing.T) {
	body := []byte(`{"tools":[{"function":{"parameters":{"type":"object","properties":{
		"arr":{"type":"array","items":{"type":"object",
			"propertyNames":{"pattern":"^n"},
			"properties":{"x":{"type":"string"}}}}
	},"anyOf":[
		{"type":"string","propertyNames":{"pattern":"^a"}},
		{"prefixItems":[{"type":"string"}]}
	],"oneOf":[{"propertyNames":{"pattern":"^b"}}],
	"allOf":[{"prefixItems":[{"type":"integer"}]}]
	}}}]}`)

	got := stripDevinUnsupportedToolSchemaKeywords(body)
	require.True(t, gjson.ValidBytes(got))
	s := string(got)
	require.NotContains(t, s, "propertyNames")
	require.NotContains(t, s, "prefixItems")
	require.True(t, gjson.GetBytes(got, "tools.0.function.parameters.properties.arr.items.properties.x").Exists())
	require.True(t, gjson.GetBytes(got, "tools.0.function.parameters.anyOf.1.items.type").Exists())
	require.Equal(t, "integer", gjson.GetBytes(got, "tools.0.function.parameters.allOf.0.items.type").String())
}

// ⑤ 简单合法 schema（无不支持关键字）→ 字节不变。
func TestStripDevinUnsupportedToolSchemaKeywords_SimpleSchemaUnchanged(t *testing.T) {
	body := []byte(`{"model":"gpt","tools":[{"type":"function","function":{"name":"get_weather","description":"d","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],"messages":[]}`)
	got := stripDevinUnsupportedToolSchemaKeywords(body)
	require.Equal(t, body, got)
}

// ⑥ 大整数保真：触发清洗的同时，无关字段中的大整数不得因 float64 重序列化被改写。
func TestStripDevinUnsupportedToolSchemaKeywords_LargeIntegerPreserved(t *testing.T) {
	body := []byte(`{"model":"gpt","tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","propertyNames":{"pattern":"^x"},"properties":{"a":{"type":"string"}}}}}],"tool_result":{"value":9007199254740993},"messages":[]}`)

	got := stripDevinUnsupportedToolSchemaKeywords(body)
	require.True(t, gjson.ValidBytes(got))
	require.NotContains(t, string(got), "propertyNames", "cleanup must still happen")
	require.Contains(t, string(got), "9007199254740993", "large integer must survive re-serialization unchanged")
}

// ⑦ 深层遍历：GF-11 遗漏的 schema 承载位置也应继续下钻。
func TestStripDevinUnsupportedToolSchemaKeywords_DeepTraversal(t *testing.T) {
	t.Run("additionalProperties nested propertyNames removed", func(t *testing.T) {
		body := []byte(`{"tools":[{"function":{"parameters":{"type":"object","additionalProperties":{"type":"object","propertyNames":{"pattern":"^k"},"properties":{"v":{"type":"string"}}}}}}]}`)
		got := stripDevinUnsupportedToolSchemaKeywords(body)
		require.NotEqual(t, body, got, "changed must be true when a nested propertyNames is removed")
		require.NotContains(t, string(got), "propertyNames")
		require.Equal(t, "object", gjson.GetBytes(got, "tools.0.function.parameters.additionalProperties.type").String())
	})

	t.Run("$defs nested prefixItems folded", func(t *testing.T) {
		body := []byte(`{"tools":[{"function":{"parameters":{"$defs":{"row":{"prefixItems":[{"type":"object","properties":{"id":{"type":"integer"}}}]}}}}}]}`)
		got := stripDevinUnsupportedToolSchemaKeywords(body)
		require.NotEqual(t, body, got, "changed must be true when prefixItems is folded")
		require.NotContains(t, string(got), "prefixItems")
		require.Equal(t, "object", gjson.GetBytes(got, "tools.0.function.parameters.$defs.row.items.type").String())
	})
}

// ⑧ 数据字段不被误改：enum 中的字符串/对象即使长得像关键字也不得被当成 schema 下钻清理。
func TestStripDevinUnsupportedToolSchemaKeywords_DataFieldsUntouched(t *testing.T) {
	t.Run("enum string", func(t *testing.T) {
		body := []byte(`{"tools":[{"function":{"parameters":{"type":"object","properties":{"a":{"type":"string","enum":["propertyNames","prefixItems"]}}}}}]}`)
		got := stripDevinUnsupportedToolSchemaKeywords(body)
		require.Equal(t, body, got, "no real dirty keys -> byte-identical")
		require.Equal(t, "propertyNames", gjson.GetBytes(got, "tools.0.function.parameters.properties.a.enum.0").String())
	})

	t.Run("enum object", func(t *testing.T) {
		body := []byte(`{"tools":[{"function":{"parameters":{"type":"object","properties":{"a":{"enum":[{"propertyNames":{"pattern":"^data"}}]}}}}}]}`)
		got := stripDevinUnsupportedToolSchemaKeywords(body)
		require.Equal(t, body, got, "enum data objects must not be treated as schema nodes")
	})
}
