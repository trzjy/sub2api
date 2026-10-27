//go:build unit

package service

// Cursor schema 组合/引用写法归一化验收测试（方案 cursor-schema-composition-plan.md §6）。
//
// 覆盖方案 §6 的 9 条：
//  1. $defs + $ref 展开（含嵌套、~1 转义）
//  2. 循环 $ref → schema_unresolvable
//  3. 外部 $ref（https://...）→ schema_unresolvable
//  4. allOf 合并 type/properties/required；冲突 → 失败
//  5. anyOf: [T, null] → 收敛为 T，且带数组参数可正常还原
//  6. anyOf 全 object → 合并 properties；string|object → 失败
//  7. type: ["string","null"] → string；["string","number"] → 失败
//  8. Pydantic v2 风格完整 schema（$defs + nullable + array）端到端还原
//  9. 现有 retype 用例全部保持通过（例外：cursor_mcpargs_retype_test.go:470 的输入
//     按执行期裁定改为 {"type":["string","number"]}，断言不变）
//
// 所有断言均不直接依赖外部网络或真实上游。

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// cursorTestNormalizeSchema 对一段 schema JSON 做纯函数级归一化，返回归一化后的
// JSON 字符串。归一化失败时原样返回错误。
func cursorTestNormalizeSchema(t *testing.T, schema string) (string, error) {
	t.Helper()
	var root map[string]any
	require.NoError(t, json.Unmarshal([]byte(schema), &root))
	out, err := cursorNormalizeSchema(root, root, map[string]bool{}, 0)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(out)
	require.NoError(t, err)
	return string(b), nil
}

// cursorTestRequireUnresolvable 断言 err 是 schema_unresolvable 的失败关闭。
func cursorTestRequireUnresolvable(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCursorProtocol)
	var re *cursorRetypeError
	require.True(t, errors.As(err, &re), "错误必须是 *cursorRetypeError")
	require.Equal(t, cursorRetypeSchemaUnresolvable, re.Reason(), "归一化失败原因必须复用 schema_unresolvable")
}

// 1. $defs + $ref 展开：含嵌套 $ref 与 ~1（'/'）转义，展开后索引形态正确、
// 且内层 array 投影可端到端还原。
func TestCursorSchemaNormalizeDefsRefExpansion(t *testing.T) {
	schema := `{
		"$defs": {
			"a/b": {"type": "object", "properties": {"deep": {"$ref": "#/$defs/inner"}}},
			"inner": {"type": "array", "items": {"type": "number"}}
		},
		"type": "object",
		"properties": {"x": {"$ref": "#/$defs/a~1b"}}
	}`

	got, err := cursorTestNormalizeSchema(t, schema)
	require.NoError(t, err)
	require.JSONEq(t, `{
		"type":"object",
		"properties":{"x":{"type":"object","properties":{"deep":{"type":"array","items":{"type":"number"}}}}}
	}`, got)

	// 端到端：经索引归一化后 {"item":[...]} 投影按展开后的 array 还原。
	def := cursorTestBuildToolDef(t, "t1", schema)
	out, err := cursorRetypeArgsByToolDefs("c", "t1", `{"x":{"deep":{"item":["1","2"]}}}`, [][]byte{def}, nil)
	require.NoError(t, err)
	require.JSONEq(t, `{"x":{"deep":[1,2]}}`, out)

	// 菱形引用不是循环：两个属性各引用一次同一 $defs 目标，必须归一化成功。
	diamond := `{
		"$defs": {"leaf": {"type": "string"}},
		"type": "object",
		"properties": {"p": {"$ref": "#/$defs/leaf"}, "q": {"$ref": "#/$defs/leaf"}}
	}`
	got, err = cursorTestNormalizeSchema(t, diamond)
	require.NoError(t, err, "菱形引用（栈已退出再进）不得误报循环")
	require.JSONEq(t, `{"type":"object","properties":{"p":{"type":"string"},"q":{"type":"string"}}}`, got)
}

// 2. 循环 $ref → schema_unresolvable；经索引以 nil 哨兵失败关闭，绝不静默丢弃。
func TestCursorSchemaNormalizeCycleRef(t *testing.T) {
	schema := `{
		"$defs": {
			"A": {"type": "object", "properties": {"b": {"$ref": "#/$defs/B"}}},
			"B": {"type": "object", "properties": {"a": {"$ref": "#/$defs/A"}}}
		},
		"type": "object",
		"properties": {"x": {"$ref": "#/$defs/A"}}
	}`

	_, err := cursorTestNormalizeSchema(t, schema)
	cursorTestRequireUnresolvable(t, err)

	// 经索引：失败哨兵必须入索引（不得静默丢弃），后续调用按既有 schema_unresolvable 失败。
	def := cursorTestBuildToolDef(t, "t2", schema)
	idx := cursorToolSchemaIndex([][]byte{def})
	v, ok := idx["t2"]
	require.True(t, ok, "归一化失败不得静默丢弃：哨兵必须入索引")
	require.Nil(t, v, "失败哨兵必须是 nil schema")

	var reason cursorRetypeErrReason
	_, err = cursorRetypeArgsByToolDefs("c", "t2", `{}`, [][]byte{def}, func(r cursorRetypeErrReason) {
		reason = r
	})
	cursorTestRequireUnresolvable(t, err)
	require.Equal(t, cursorRetypeSchemaUnresolvable, reason)

	// 自引用（$ref 指向根）同样是循环。
	_, err = cursorTestNormalizeSchema(t, `{"$ref":"#"}`)
	cursorTestRequireUnresolvable(t, err)
}

// 3. 外部 $ref（远程 / 非本地）与不可解析指针 → schema_unresolvable（无网络、不猜）。
func TestCursorSchemaNormalizeRemoteRef(t *testing.T) {
	for name, schema := range map[string]string{
		"https remote":      `{"type":"object","properties":{"x":{"$ref":"https://example.com/s.json"}}}`,
		"relative file":     `{"type":"object","properties":{"x":{"$ref":"shared/types.json#/X"}}}`,
		"missing pointer":   `{"type":"object","properties":{"x":{"$ref":"#/$defs/absent"}}}`,
		"non-object target": `{"type":"object","properties":{"x":{"$ref":"#/$defs/n"}},"$defs":{"n":5}}`,
		"non-string ref":    `{"type":"object","properties":{"x":{"$ref":5}}}`,
		"non-pointer":       `{"type":"object","properties":{"x":{"$ref":"#defs"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := cursorTestNormalizeSchema(t, schema)
			cursorTestRequireUnresolvable(t, err)
		})
	}

	// $ref 与兄弟约束键并存：语义不可判定（静默丢兄弟约束等于放行）→ 失败关闭。
	_, err := cursorTestNormalizeSchema(t, `{"$ref":"#/$defs/x","additionalProperties":false,"$defs":{"x":{"type":"object"}}}`)
	cursorTestRequireUnresolvable(t, err)
}

// 4. allOf 合并 type/properties/required/additionalProperties；冲突失败关闭。
func TestCursorSchemaNormalizeAllOf(t *testing.T) {
	t.Run("merge type properties required", func(t *testing.T) {
		got, err := cursorTestNormalizeSchema(t, `{
			"type": "object",
			"allOf": [
				{"type": "object", "properties": {"a": {"type": "string"}}, "required": ["b"]},
				{"properties": {"b": {"type": "number"}}}
			]
		}`)
		require.NoError(t, err)
		require.JSONEq(t, `{
			"type":"object",
			"properties":{"a":{"type":"string"},"b":{"type":"number"}},
			"required":["b"]
		}`, got)
	})

	t.Run("required union sorted", func(t *testing.T) {
		got, err := cursorTestNormalizeSchema(t, `{
			"allOf": [{"required": ["b", "c"]}, {"required": ["a"]}]
		}`)
		require.NoError(t, err)
		require.JSONEq(t, `{"required":["a","b","c"]}`, got)
	})

	t.Run("additionalProperties takes stricter", func(t *testing.T) {
		got, err := cursorTestNormalizeSchema(t, `{
			"type": "object",
			"allOf": [{"properties": {"a": {"type": "string"}}}, {"additionalProperties": false}]
		}`)
		require.NoError(t, err)
		require.JSONEq(t, `{
			"type":"object",
			"properties":{"a":{"type":"string"}},
			"additionalProperties":false
		}`, got)
	})

	t.Run("same-name property deep-equal ok", func(t *testing.T) {
		got, err := cursorTestNormalizeSchema(t, `{
			"type": "object",
			"allOf": [
				{"properties": {"a": {"type": "string"}}},
				{"properties": {"a": {"type": "string"}}}
			]
		}`)
		require.NoError(t, err)
		require.JSONEq(t, `{"type":"object","properties":{"a":{"type":"string"}}}`, got)
	})

	t.Run("type conflict fails", func(t *testing.T) {
		_, err := cursorTestNormalizeSchema(t, `{"allOf":[{"type":"string"},{"type":"object"}]}`)
		cursorTestRequireUnresolvable(t, err)
	})

	t.Run("same-name property inequivalent fails", func(t *testing.T) {
		_, err := cursorTestNormalizeSchema(t, `{
			"allOf": [
				{"type":"object","properties": {"a": {"type": "string"}}},
				{"type":"object","properties": {"a": {"type": "number"}}}
			]
		}`)
		cursorTestRequireUnresolvable(t, err)
	})

	t.Run("items inequivalent fails, single side kept", func(t *testing.T) {
		_, err := cursorTestNormalizeSchema(t, `{
			"allOf": [
				{"type":"array","items":{"type":"number"}},
				{"type":"array","items":{"type":"string"}}
			]
		}`)
		cursorTestRequireUnresolvable(t, err)

		got, err := cursorTestNormalizeSchema(t, `{
			"allOf": [{"type":"array","items":{"type":"number"}}, {"type":"array"}]
		}`)
		require.NoError(t, err)
		require.JSONEq(t, `{"type":"array","items":{"type":"number"}}`, got)
	})
}

// 5. anyOf: [T, null] → 收敛为 T；带数组参数可正常还原。
func TestCursorSchemaNormalizeAnyOfNullable(t *testing.T) {
	schema := `{
		"type": "object",
		"properties": {
			"arr": {"anyOf": [{"type": "array", "items": {"type": "number"}}, {"type": "null"}]},
			"s":   {"anyOf": [{"type": "string"}, {"type": "null"}]}
		}
	}`

	got, err := cursorTestNormalizeSchema(t, schema)
	require.NoError(t, err)
	require.JSONEq(t, `{
		"type":"object",
		"properties": {
			"arr": {"type":"array","items":{"type":"number"}},
			"s":   {"type":"string"}
		}
	}`, got)

	// 端到端：数组参数（{"item":[...]} 投影形态）按收敛后的 T=array 还原。
	def := cursorTestBuildToolDef(t, "t5", schema)
	out, err := cursorRetypeArgsByToolDefs("c", "t5", `{"arr":{"item":["1.5","2"]},"s":"k"}`, [][]byte{def}, nil)
	require.NoError(t, err)
	require.JSONEq(t, `{"arr":[1.5,2],"s":"k"}`, out)

	// oneOf 同规则（Nullable oneOf 形态）。
	got, err = cursorTestNormalizeSchema(t, `{
		"type":"object",
		"properties":{"x":{"oneOf":[{"type":"array","items":{"type":"boolean"}},{"type":"null"}]}}
	}`)
	require.NoError(t, err)
	require.JSONEq(t, `{"type":"object","properties":{"x":{"type":"array","items":{"type":"boolean"}}}}`, got)

	// 全部分支都是 null（原列表非空）→ 不可判定。
	_, err = cursorTestNormalizeSchema(t, `{"anyOf":[{"type":"null"},{"type":"null"}]}`)
	cursorTestRequireUnresolvable(t, err)
}

// 6. anyOf 全 object → 合并 properties；string|object（真正多态）→ 失败。
func TestCursorSchemaNormalizeAnyOfSameType(t *testing.T) {
	t.Run("objects merge properties", func(t *testing.T) {
		schema := `{
			"anyOf": [
				{"type": "object", "properties": {"a": {"type": "string"}}},
				{"type": "object", "properties": {"b": {"type": "number"}}}
			]
		}`
		got, err := cursorTestNormalizeSchema(t, schema)
		require.NoError(t, err)
		require.JSONEq(t, `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"number"}}}`, got)

		// 端到端：并集属性各按各的类型还原。
		def := cursorTestBuildToolDef(t, "t6", schema)
		out, err := cursorRetypeArgsByToolDefs("c", "t6", `{"a":"x","b":"1"}`, [][]byte{def}, nil)
		require.NoError(t, err)
		require.JSONEq(t, `{"a":"x","b":1}`, out)
	})

	t.Run("string|object fails", func(t *testing.T) {
		_, err := cursorTestNormalizeSchema(t, `{
			"anyOf": [{"type": "string"}, {"type": "object", "properties": {}}]
		}`)
		cursorTestRequireUnresolvable(t, err)
	})

	t.Run("arrays items equivalent kept", func(t *testing.T) {
		got, err := cursorTestNormalizeSchema(t, `{
			"anyOf": [
				{"type": "array", "items": {"type": "number"}},
				{"type": "array", "items": {"type": "number"}}
			]
		}`)
		require.NoError(t, err)
		require.JSONEq(t, `{"type":"array","items":{"type":"number"}}`, got)
	})

	t.Run("arrays items inequivalent omitted", func(t *testing.T) {
		got, err := cursorTestNormalizeSchema(t, `{
			"anyOf": [
				{"type": "array", "items": {"type": "number"}},
				{"type": "array", "items": {"type": "string"}}
			]
		}`)
		require.NoError(t, err)
		// items 不全等价 → 省略 items，元素原样（不投影）。
		require.JSONEq(t, `{"type":"array"}`, got)
	})

	t.Run("same scalar type collapses", func(t *testing.T) {
		got, err := cursorTestNormalizeSchema(t, `{"anyOf":[{"type":"string"},{"type":"string"}]}`)
		require.NoError(t, err)
		require.JSONEq(t, `{"type":"string"}`, got)
	})

	t.Run("anyOf and oneOf coexist fails", func(t *testing.T) {
		_, err := cursorTestNormalizeSchema(t, `{"anyOf":[{"type":"string"}],"oneOf":[{"type":"string"}]}`)
		cursorTestRequireUnresolvable(t, err)
	})
}

// 7. type 数组：["string","null"] → string；["string","number"] → 失败；
// ["null"] → 视为纯 null 原样（无投影）。
func TestCursorSchemaNormalizeTypeArray(t *testing.T) {
	t.Run("nullable string collapses", func(t *testing.T) {
		got, err := cursorTestNormalizeSchema(t, `{"type":["string","null"]}`)
		require.NoError(t, err)
		require.JSONEq(t, `{"type":"string"}`, got)
		// 顺序无关。
		got, err = cursorTestNormalizeSchema(t, `{"type":["null","string"]}`)
		require.NoError(t, err)
		require.JSONEq(t, `{"type":"string"}`, got)
	})

	t.Run("end-to-end nullable property retypes", func(t *testing.T) {
		schema := `{"type":"object","properties":{"s":{"type":["string","null"]}}}`
		def := cursorTestBuildToolDef(t, "t7", schema)
		out, err := cursorRetypeArgsByToolDefs("c", "t7", `{"s":"x"}`, [][]byte{def}, nil)
		require.NoError(t, err)
		require.JSONEq(t, `{"s":"x"}`, out)
	})

	t.Run("genuinely polymorphic fails", func(t *testing.T) {
		schema := `{"type":"object","properties":{"a":{"type":["string","number"]}}}`
		_, err := cursorTestNormalizeSchema(t, schema)
		cursorTestRequireUnresolvable(t, err)

		// 经索引：哨兵 + 后续按既有 schema_unresolvable 失败关闭
		//（cursor_mcpargs_retype_test.go:470 改造后的同款输入，断言口径一致）。
		def := cursorTestBuildToolDef(t, "t7b", schema)
		var reason cursorRetypeErrReason
		_, err = cursorRetypeArgsByToolDefs("c", "t7b", `{"a":1}`, [][]byte{def}, func(r cursorRetypeErrReason) {
			reason = r
		})
		cursorTestRequireUnresolvable(t, err)
		require.Equal(t, cursorRetypeSchemaUnresolvable, reason)
	})

	t.Run("pure null stays as-is", func(t *testing.T) {
		got, err := cursorTestNormalizeSchema(t, `{"type":["null"]}`)
		require.NoError(t, err)
		require.JSONEq(t, `{"type":"null"}`, got)
	})
}

// 8. Pydantic v2 风格完整 schema（$defs + $ref + anyOf null + array）端到端还原。
func TestCursorSchemaNormalizePydanticV2EndToEnd(t *testing.T) {
	schema := `{
		"$defs": {
			"Item": {
				"type": "object",
				"properties": {"id": {"type": "string"}, "vals": {"type": "array", "items": {"type": "number"}}},
				"required": ["id"]
			}
		},
		"type": "object",
		"properties": {
			"label": {"anyOf": [{"type": "string"}, {"type": "null"}]},
			"items": {"anyOf": [{"type": "array", "items": {"$ref": "#/$defs/Item"}}, {"type": "null"}]},
			"meta":  {"$ref": "#/$defs/Item"}
		},
		"required": ["label"]
	}`

	// 归一化索引形态：$ref 展开、nullable anyOf 收敛、$defs 不入输出。
	got, err := cursorTestNormalizeSchema(t, schema)
	require.NoError(t, err)
	require.JSONEq(t, `{
		"type": "object",
		"properties": {
			"label": {"type": "string"},
			"items": {"type": "array", "items": {
				"type": "object",
				"properties": {"id": {"type": "string"}, "vals": {"type": "array", "items": {"type": "number"}}},
				"required": ["id"]
			}},
			"meta": {"type": "object", "properties": {"id": {"type": "string"}, "vals": {"type": "array", "items": {"type": "number"}}}, "required": ["id"]}
		},
		"required": ["label"]
	}`, got)

	// 端到端：$defs + nullable + array 的工具一次还原成功（嵌套 {"item":[...]} 投影）。
	def := cursorTestBuildToolDef(t, "pyd_v2", schema)
	args := `{"label":"L","items":{"item":[{"id":"a","vals":{"item":["1","2"]}},{"id":"b","vals":[3]}]},"meta":{"id":"c","vals":{"item":["4.5"]}}}`
	out, err := cursorRetypeArgsByToolDefs("call_pyd", "pyd_v2", args, [][]byte{def}, nil)
	require.NoError(t, err)
	require.JSONEq(t, `{
		"label":"L",
		"items":[{"id":"a","vals":[1,2]},{"id":"b","vals":[3]}],
		"meta":{"id":"c","vals":[4.5]}
	}`, out)
}

// 9. 现有 retype 行为保持：直接路径对组合关键字仍失败关闭（两路径一致）；
// 经归一化索引的简单 schema 与既有 retype 失败码不回归。
func TestCursorSchemaNormalizeRetypeCompat(t *testing.T) {
	t.Run("direct retype path still fails closed on composition keywords", func(t *testing.T) {
		// retypeValue 未改动：不经归一化的直接路径对组合/引用写法仍 schema_unresolvable。
		for _, schema := range []string{
			`{"type":"object","properties":{"a":{"$ref":"#/x"}}}`,
			`{"type":"object","properties":{"a":{"anyOf":[{"type":"string"}]}}}`,
			`{"type":"object","properties":{"a":{"oneOf":[{"type":"string"}]}}}`,
			`{"type":"object","properties":{"a":{"allOf":[{"type":"string"}]}}}`,
			`{"type":"object","properties":{"a":{"type":["string","null"]}}}`,
		} {
			_, err := cursorRetypeMcpArgs([]byte(`{"a":1}`), []byte(schema))
			require.ErrorIs(t, err, ErrCursorProtocol, "直接 retype 路径必须保持失败关闭: %s", schema)
		}
	})

	t.Run("normalized index keeps existing failure reasons", func(t *testing.T) {
		// 归一化不抢答 retype 的既有失败码（透传畸形，两路径一致）。
		cases := []struct {
			name   string
			schema string
			args   string
			reason cursorRetypeErrReason
		}{
			{"items_not_allowed",
				`{"type":"object","properties":{"a":{"type":"array","items":false}}}`,
				`{"a":[1]}`, cursorRetypeItemsNotAllowed},
			{"additional_property_present",
				`{"type":"object","properties":{},"additionalProperties":false}`,
				`{"x":1}`, cursorRetypeAdditionalPropPresent},
			{"object_type_mismatch",
				`{"type":"object","properties":{"o":{"type":"object"}}}`,
				`{"o":5}`, cursorRetypeObjectTypeMismatch},
			{"numeric_out_of_range",
				`{"type":"object","properties":{"n":{"type":"number"}}}`,
				`{"n":"1e309"}`, cursorRetypeNumericOutOfRange},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				def := cursorTestBuildToolDef(t, "t9_"+c.name, c.schema)
				_, err := cursorRetypeArgsByToolDefs("c", "t9_"+c.name, c.args, [][]byte{def}, nil)
				require.ErrorIs(t, err, ErrCursorProtocol)
				var re *cursorRetypeError
				require.True(t, errors.As(err, &re))
				require.Equal(t, c.reason, re.Reason(), "经归一化索引后既有失败码不得改变")
			})
		}
	})

	t.Run("simple schema behavior identical via both paths", func(t *testing.T) {
		schema := `{"type":"object","properties":{"a":{"type":"array","items":{"type":"number"}},"n":{"type":"number"}}}`
		args := `{"a":{"item":["1","2"]},"n":"3.5"}`
		def := cursorTestBuildToolDef(t, "t9s", schema)

		direct, err := cursorRetypeMcpArgs([]byte(args), []byte(schema))
		require.NoError(t, err)

		viaIndex, err := cursorRetypeArgsByToolDefs("c", "t9s", args, [][]byte{def}, nil)
		require.NoError(t, err)

		require.JSONEq(t, string(direct), viaIndex, "归一化不得改变简单 schema 的还原行为")
		require.JSONEq(t, `{"a":[1,2],"n":3.5}`, viaIndex)
	})
}
