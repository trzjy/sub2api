//go:build unit

package service

// Cursor McpArgs 按 schema 还原（派发单 r15 方案 §3）验收测试。
//
// 覆盖方案 §5.1 验收矩阵：不误伤 / 精确还原 / 嵌套 / 失败关闭 / 数值还原 /
// 布尔还原 / items 三态 / 组合引用 schema / additionalProperties / 错误枚举 /
// 大整数 / 无 schema / 两路径一致。所有断言均不直接依赖外部网络或真实上游。

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
)

// cursorTestBuildToolDef 构造一条 McpToolDefinition 原始字节（f1 名 / f2 description /
// f4 provider / f5 tool_name / f6 input_schema），与 cursorBuildMcpTools 同形，
// 仅用于测试夹具（不重新序列化 req.Tools）。
func cursorTestBuildToolDef(t *testing.T, toolName, schema string) []byte {
	t.Helper()
	var def []byte
	def = cursorAppendStringField(def, cursorFldMcpToolDefName, toolName)
	def = cursorAppendStringField(def, cursorFldMcpToolDefDescription, "desc-"+toolName)
	def = cursorAppendStringField(def, cursorFldMcpToolDefProvider, cursorMcpProviderIdentifier)
	def = cursorAppendStringField(def, cursorFldMcpToolDefToolName, toolName)
	def = cursorAppendStringField(def, cursorFldMcpToolDefInputSchema, schema)
	return def
}

// TestCursorToolSchemaIndex 验证 f4 原始字节 → tool_name → schema 的解出。
func TestCursorToolSchemaIndex(t *testing.T) {
	defs := [][]byte{
		cursorTestBuildToolDef(t, "get_weather", `{"type":"object","properties":{"city":{"type":"string"}}}`),
		cursorTestBuildToolDef(t, "calc", `{"type":"object","properties":{"a":{"type":"array","items":{"type":"number"}}}}`),
	}
	idx := cursorToolSchemaIndex(defs)
	require.Len(t, idx, 2)
	require.JSONEq(t, `{"type":"object","properties":{"city":{"type":"string"}}}`, string(idx["get_weather"]))
	require.JSONEq(t, `{"type":"object","properties":{"a":{"type":"array","items":{"type":"number"}}}}`, string(idx["calc"]))

	// 工具名不在声明集 → 取不到。
	_, ok := idx["nope"]
	require.False(t, ok)
}

// TestCursorRetypeScalars 数值/布尔还原（probe19 实测：解析即等价于正确路径 double）。
func TestCursorRetypeScalars(t *testing.T) {
	numSchema := `{"type":"object","properties":{"n":{"type":"number"}}}`
	numCases := []struct {
		name, in, wantJSON string
	}{
		{"1.5", `"1.5"`, `1.5`},
		{"0.1", `"0.1"`, `0.1`},
		{"-0.25", `"-0.25"`, `-0.25`},
		{"1e21", `"1e21"`, `1e+21`}, // JSON 数字序列化用规范式，数值相等即可
		{"1e-7", `"1e-7"`, `1e-07`},
		{"big-int literal 2^53+1", `"9007199254740993"`, `9007199254740992`}, // 解析为 2^53（与正确路径一致）
	}
	for _, c := range numCases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			out, err := cursorRetypeMcpArgs([]byte(`{"n":`+c.in+`}`), []byte(numSchema))
			require.NoError(t, err)
			var got, want map[string]float64
			require.NoError(t, json.Unmarshal(out, &got))
			require.NoError(t, json.Unmarshal([]byte(`{"n":`+c.wantJSON+`}`), &want))
			require.Equal(t, want["n"], got["n"], "解析后必须等于正确路径的 double")
		})
	}

	boolSchema := `{"type":"object","properties":{"b":{"type":"boolean"}}}`
	t.Run("boolean true", func(t *testing.T) {
		out, err := cursorRetypeMcpArgs([]byte(`{"b":"true"}`), []byte(boolSchema))
		require.NoError(t, err)
		require.JSONEq(t, `{"b":true}`, string(out))
	})
	t.Run("boolean false", func(t *testing.T) {
		out, err := cursorRetypeMcpArgs([]byte(`{"b":"false"}`), []byte(boolSchema))
		require.NoError(t, err)
		require.JSONEq(t, `{"b":false}`, string(out))
	})
	t.Run("boolean yes passthrough", func(t *testing.T) {
		// "yes" 非布尔字面量 → 原样保留（不报错、不改写）。
		out, err := cursorRetypeMcpArgs([]byte(`{"b":"yes"}`), []byte(boolSchema))
		require.NoError(t, err)
		require.JSONEq(t, `{"b":"yes"}`, string(out))
	})
}

// TestCursorRetypeNoOp 不误伤：无需转换的叶子逐字节不变。
func TestCursorRetypeNoOp(t *testing.T) {
	t.Run("string leaf", func(t *testing.T) {
		out, err := cursorRetypeMcpArgs(
			[]byte(`{"s":"hello"}`),
			[]byte(`{"type":"object","properties":{"s":{"type":"string"}}}`))
		require.NoError(t, err)
		require.JSONEq(t, `{"s":"hello"}`, string(out))
	})
	t.Run("null leaf", func(t *testing.T) {
		out, err := cursorRetypeMcpArgs(
			[]byte(`{"n":null}`),
			[]byte(`{"type":"object","properties":{"n":{"type":"null"}}}`))
		require.NoError(t, err)
		require.JSONEq(t, `{"n":null}`, string(out))
	})
	t.Run("already-number leaf", func(t *testing.T) {
		out, err := cursorRetypeMcpArgs(
			[]byte(`{"n":1.5}`),
			[]byte(`{"type":"object","properties":{"n":{"type":"number"}}}`))
		require.NoError(t, err)
		require.JSONEq(t, `{"n":1.5}`, string(out))
	})
	t.Run("no-type property", func(t *testing.T) {
		out, err := cursorRetypeMcpArgs(
			[]byte(`{"x":{"k":1}}`),
			[]byte(`{"type":"object","properties":{"x":{}}}`))
		require.NoError(t, err)
		require.JSONEq(t, `{"x":{"k":1}}`, string(out))
	})
	t.Run("object-declared item-object not rewritten", func(t *testing.T) {
		// object 声明处的 {"item":"x"} 不得被误判为 array 投影。
		out, err := cursorRetypeMcpArgs(
			[]byte(`{"item":"x"}`),
			[]byte(`{"type":"object"}`))
		require.NoError(t, err)
		require.JSONEq(t, `{"item":"x"}`, string(out))
	})
	t.Run("string-typed item-object passthrough", func(t *testing.T) {
		// r6-F2：string 类型位置收到 {"item":"x"} 是合法对象，原样保留。
		out, err := cursorRetypeMcpArgs(
			[]byte(`{"s":{"item":"x"}}`),
			[]byte(`{"type":"object","properties":{"s":{"type":"string"}}}`))
		require.NoError(t, err)
		require.JSONEq(t, `{"s":{"item":"x"}}`, string(out))
	})
}

// TestCursorRetypeExactRestore 精确还原 §2.1 表 8 行投影（逐行）。
func TestCursorRetypeExactRestore(t *testing.T) {
	t.Run("array from {item:[...]}", func(t *testing.T) {
		out, err := cursorRetypeMcpArgs(
			[]byte(`{"a":{"item":["1.5","2"]}}`),
			[]byte(`{"type":"object","properties":{"a":{"type":"array","items":{"type":"number"}}}}`))
		require.NoError(t, err)
		require.JSONEq(t, `{"a":[1.5,2]}`, string(out))
	})
	t.Run("array as plain list", func(t *testing.T) {
		out, err := cursorRetypeMcpArgs(
			[]byte(`{"a":[1,2,3]}`),
			[]byte(`{"type":"object","properties":{"a":{"type":"array","items":{"type":"number"}}}}`))
		require.NoError(t, err)
		require.JSONEq(t, `{"a":[1,2,3]}`, string(out))
	})
	t.Run("nested object-in-array", func(t *testing.T) {
		out, err := cursorRetypeMcpArgs(
			[]byte(`{"a":{"item":[{"k":"1"},{"k":"2"}]}}`),
			[]byte(`{"type":"object","properties":{"a":{"type":"array","items":{"type":"object","properties":{"k":{"type":"number"}}}}}}`))
		require.NoError(t, err)
		require.JSONEq(t, `{"a":[{"k":1},{"k":2}]}`, string(out))
	})
	t.Run("nested array-in-object", func(t *testing.T) {
		out, err := cursorRetypeMcpArgs(
			[]byte(`{"inner":{"b":{"item":["true","false"]}}}`),
			[]byte(`{"type":"object","properties":{"inner":{"type":"object","properties":{"b":{"type":"array","items":{"type":"boolean"}}}}}}`))
		require.NoError(t, err)
		require.JSONEq(t, `{"inner":{"b":[true,false]}}`, string(out))
	})
	t.Run("deep nesting root-object contains array contains object contains array", func(t *testing.T) {
		schema := `{"type":"object","properties":{"a":{"type":"array","items":{"type":"object","properties":{"b":{"type":"array","items":{"type":"number"}}}}}}}`
		out, err := cursorRetypeMcpArgs(
			[]byte(`{"a":{"item":[{"b":{"item":["1","2"]}},{"b":["3"]}]}}`), []byte(schema))
		require.NoError(t, err)
		require.JSONEq(t, `{"a":[{"b":[1,2]},{"b":[3]}]}`, string(out))
	})
}

// TestCursorRetypeFailClosed 失败关闭（与数值无关，属结构不可判定）。
func TestCursorRetypeFailClosed(t *testing.T) {
	arraySchema := `{"type":"object","properties":{"a":{"type":"array"}}}`
	t.Run("multi-key item dict", func(t *testing.T) {
		_, err := cursorRetypeMcpArgs(
			[]byte(`{"a":{"item":[1],"x":2}}`), []byte(arraySchema))
		require.ErrorIs(t, err, ErrCursorProtocol)
	})
	t.Run("wrong-key object", func(t *testing.T) {
		_, err := cursorRetypeMcpArgs(
			[]byte(`{"a":{"x":1}}`), []byte(arraySchema))
		require.ErrorIs(t, err, ErrCursorProtocol)
	})
	t.Run("numeric overflow", func(t *testing.T) {
		_, err := cursorRetypeMcpArgs(
			[]byte(`{"n":"1e309"}`),
			[]byte(`{"type":"object","properties":{"n":{"type":"number"}}}`))
		require.ErrorIs(t, err, ErrCursorProtocol)
	})
	t.Run("object declared but value is number", func(t *testing.T) {
		_, err := cursorRetypeMcpArgs(
			[]byte(`{"o":5}`),
			[]byte(`{"type":"object","properties":{"o":{"type":"object"}}}`))
		require.ErrorIs(t, err, ErrCursorProtocol)
	})
}

// TestCursorRetypeItemsStates items 三态。
func TestCursorRetypeItemsStates(t *testing.T) {
	base := `{"type":"object","properties":{"a":{"type":"array"`
	t.Run("items missing -> as-is", func(t *testing.T) {
		out, err := cursorRetypeMcpArgs(
			[]byte(`{"a":[{"x":"1"}]}`), []byte(base+`}}}`))
		require.NoError(t, err)
		require.JSONEq(t, `{"a":[{"x":"1"}]}`, string(out))
	})
	t.Run("items true -> as-is", func(t *testing.T) {
		out, err := cursorRetypeMcpArgs(
			[]byte(`{"a":[{"x":"1"}]}`), []byte(base+`,"items":true}}}`))
		require.NoError(t, err)
		require.JSONEq(t, `{"a":[{"x":"1"}]}`, string(out))
	})
	t.Run("items false empty -> []", func(t *testing.T) {
		out, err := cursorRetypeMcpArgs(
			[]byte(`{"a":[]}`), []byte(base+`,"items":false}}}`))
		require.NoError(t, err)
		require.JSONEq(t, `{"a":[]}`, string(out))
	})
	t.Run("items false nonempty -> items_not_allowed", func(t *testing.T) {
		_, err := cursorRetypeMcpArgs(
			[]byte(`{"a":[1]}`), []byte(base+`,"items":false}}}`))
		require.ErrorIs(t, err, ErrCursorProtocol)
	})
	t.Run("items non-object-non-bool -> items_sub_schema_invalid", func(t *testing.T) {
		_, err := cursorRetypeMcpArgs(
			[]byte(`{"a":[1]}`), []byte(base+`,"items":5}}}`))
		require.ErrorIs(t, err, ErrCursorProtocol)
	})
}

// TestCursorRetypeComboSchemas 组合/引用 schema 全部失败关闭（不做部分解析）。
func TestCursorRetypeComboSchemas(t *testing.T) {
	t.Run("$ref", func(t *testing.T) {
		_, err := cursorRetypeMcpArgs(
			[]byte(`{"a":1}`),
			[]byte(`{"type":"object","properties":{"a":{"$ref":"#/x"}}}`))
		require.ErrorIs(t, err, ErrCursorProtocol)
	})
	t.Run("anyOf", func(t *testing.T) {
		_, err := cursorRetypeMcpArgs(
			[]byte(`{"a":1}`),
			[]byte(`{"type":"object","properties":{"a":{"anyOf":[{"type":"string"}]}}}`))
		require.ErrorIs(t, err, ErrCursorProtocol)
	})
	t.Run("oneOf", func(t *testing.T) {
		_, err := cursorRetypeMcpArgs(
			[]byte(`{"a":1}`),
			[]byte(`{"type":"object","properties":{"a":{"oneOf":[{"type":"string"}]}}}`))
		require.ErrorIs(t, err, ErrCursorProtocol)
	})
	t.Run("allOf", func(t *testing.T) {
		_, err := cursorRetypeMcpArgs(
			[]byte(`{"a":1}`),
			[]byte(`{"type":"object","properties":{"a":{"allOf":[{"type":"string"}]}}}`))
		require.ErrorIs(t, err, ErrCursorProtocol)
	})
	t.Run("type as array", func(t *testing.T) {
		_, err := cursorRetypeMcpArgs(
			[]byte(`{"a":1}`),
			[]byte(`{"type":"object","properties":{"a":{"type":["string","null"]}}}`))
		require.ErrorIs(t, err, ErrCursorProtocol)
	})
	t.Run("unknown type string", func(t *testing.T) {
		_, err := cursorRetypeMcpArgs(
			[]byte(`{"a":1}`),
			[]byte(`{"type":"object","properties":{"a":{"type":"weird"}}}`))
		require.ErrorIs(t, err, ErrCursorProtocol)
	})
}

// TestCursorRetypeAdditionalProperties additionalProperties 三态。
func TestCursorRetypeAdditionalProperties(t *testing.T) {
	t.Run("schema -> recurse", func(t *testing.T) {
		out, err := cursorRetypeMcpArgs(
			[]byte(`{"x":"1"}`),
			[]byte(`{"type":"object","properties":{},"additionalProperties":{"type":"number"}}`))
		require.NoError(t, err)
		require.JSONEq(t, `{"x":1}`, string(out))
	})
	t.Run("false with unknown -> fail", func(t *testing.T) {
		_, err := cursorRetypeMcpArgs(
			[]byte(`{"x":1}`),
			[]byte(`{"type":"object","properties":{},"additionalProperties":false}`))
		require.ErrorIs(t, err, ErrCursorProtocol)
	})
	t.Run("missing/true -> passthrough", func(t *testing.T) {
		out, err := cursorRetypeMcpArgs(
			[]byte(`{"x":"hello"}`),
			[]byte(`{"type":"object","properties":{}}`))
		require.NoError(t, err)
		require.JSONEq(t, `{"x":"hello"}`, string(out))
	})
}

// TestCursorRetypeBigInt 大整数 integer 收到 "9007199254740993" → 解析为 9007199254740992。
func TestCursorRetypeBigInt(t *testing.T) {
	out, err := cursorRetypeMcpArgs(
		[]byte(`{"n":"9007199254740993"}`),
		[]byte(`{"type":"object","properties":{"n":{"type":"integer"}}}`))
	require.NoError(t, err)
	// 序列化后必须落到 2^53（与正确路径一致），而非保留 2^53+1。
	require.JSONEq(t, `{"n":9007199254740992}`, string(out))
}

// TestCursorRetypeNoSchema 无 schema：f4 不可得 / 工具名不在声明集 → ErrCursorProtocol。
func TestCursorRetypeNoSchema(t *testing.T) {
	t.Run("empty defs", func(t *testing.T) {
		_, err := cursorRetypeArgsByToolDefs("call_1", "get_weather", `{"city":"Beijing"}`, nil, nil)
		require.ErrorIs(t, err, ErrCursorProtocol)
	})
	t.Run("tool name not declared", func(t *testing.T) {
		defs := [][]byte{cursorTestBuildToolDef(t, "other", `{"type":"object"}`)}
		_, err := cursorRetypeArgsByToolDefs("call_1", "get_weather", `{"city":"Beijing"}`, defs, nil)
		require.ErrorIs(t, err, ErrCursorProtocol)
	})
}

// TestCursorJSONNumberLiteral RFC 8259 词法校验。
func TestCursorJSONNumberLiteral(t *testing.T) {
	valid := []string{"0", "1", "1.5", "0.1", "-0.25", "1e21", "1e+21", "1e-7", "9007199254740993", "-1.2e-3"}
	for _, s := range valid {
		require.True(t, isJSONNumberLiteral(s), "期望合法 JSON 数字: %q", s)
	}
	invalid := []string{"", "abc", "NaN", "Inf", "0x1p2", "01", "1.", "1e", ".5", "+1", "1.2.3", "-", "1e+", "1e-x"}
	for _, s := range invalid {
		require.False(t, isJSONNumberLiteral(s), "期望非法 JSON 数字: %q", s)
	}
}

// TestCursorRetypeErrorEnum 错误枚举：§3.3 产生的每个 error 都能在 §3.5 枚举中找到同名值，
// 且全部 Unwrap 到 ErrCursorProtocol。
func TestCursorRetypeErrorEnum(t *testing.T) {
	cases := []struct {
		name   string
		reason cursorRetypeErrReason
		run    func() error
	}{
		{"schema_invalid", cursorRetypeSchemaInvalid, func() error {
			_, e := cursorRetypeMcpArgs([]byte(`{"p":1}`), []byte(`{"type":"object","properties":{"p":null}}`))
			return e
		}},
		{"schema_unresolvable", cursorRetypeSchemaUnresolvable, func() error {
			_, e := cursorRetypeMcpArgs([]byte(`{"a":1}`), []byte(`{"type":"object","properties":{"a":{"$ref":"#"}}}`))
			return e
		}},
		{"projection_unresolvable", cursorRetypeProjectionUnresolvable, func() error {
			_, e := cursorRetypeMcpArgs([]byte(`{"a":{"x":1}}`), []byte(`{"type":"object","properties":{"a":{"type":"array"}}}`))
			return e
		}},
		{"items_not_allowed", cursorRetypeItemsNotAllowed, func() error {
			_, e := cursorRetypeMcpArgs([]byte(`{"a":[1]}`), []byte(`{"type":"object","properties":{"a":{"type":"array","items":false}}}`))
			return e
		}},
		{"items_sub_schema_invalid", cursorRetypeItemsSubSchemaInvalid, func() error {
			_, e := cursorRetypeMcpArgs([]byte(`{"a":[1]}`), []byte(`{"type":"object","properties":{"a":{"type":"array","items":5}}}`))
			return e
		}},
		{"additional_property_present", cursorRetypeAdditionalPropPresent, func() error {
			_, e := cursorRetypeMcpArgs([]byte(`{"x":1}`), []byte(`{"type":"object","properties":{},"additionalProperties":false}`))
			return e
		}},
		{"object_type_mismatch", cursorRetypeObjectTypeMismatch, func() error {
			_, e := cursorRetypeMcpArgs([]byte(`{"o":5}`), []byte(`{"type":"object","properties":{"o":{"type":"object"}}}`))
			return e
		}},
		{"numeric_out_of_range", cursorRetypeNumericOutOfRange, func() error {
			_, e := cursorRetypeMcpArgs([]byte(`{"n":"1e309"}`), []byte(`{"type":"object","properties":{"n":{"type":"number"}}}`))
			return e
		}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			err := c.run()
			require.Error(t, err)
			require.ErrorIs(t, err, ErrCursorProtocol)
			var re *cursorRetypeError
			require.True(t, errors.As(err, &re), "错误必须是 *cursorRetypeError")
			require.Equal(t, c.reason, re.Reason(), "错误原因必须与 §3.5 枚举同名")
			// 枚举值与 §3.5 字面量一致。
			require.Equal(t, string(c.reason), string(re.Reason()))
		})
	}

	// schema_absent 只能经 cursorRetypeArgsByToolDefs（工具名不在声明集）产生。
	t.Run("schema_absent", func(t *testing.T) {
		_, err := cursorRetypeArgsByToolDefs("c", "missing", `{}`, nil, nil)
		require.ErrorIs(t, err, ErrCursorProtocol)
		var re *cursorRetypeError
		require.True(t, errors.As(err, &re))
		require.Equal(t, cursorRetypeSchemaAbsent, re.Reason())
	})

	// 所有 §3.5 枚举值都被覆盖（穷举断言）。
	all := []cursorRetypeErrReason{
		cursorRetypeSchemaInvalid, cursorRetypeSchemaAbsent, cursorRetypeSchemaUnresolvable,
		cursorRetypeProjectionUnresolvable, cursorRetypeItemsNotAllowed,
		cursorRetypeItemsSubSchemaInvalid, cursorRetypeAdditionalPropPresent,
		cursorRetypeObjectTypeMismatch, cursorRetypeNumericOutOfRange,
	}
	covered := map[cursorRetypeErrReason]bool{}
	for _, c := range cases {
		covered[c.reason] = true
	}
	covered[cursorRetypeSchemaAbsent] = true
	for _, r := range all {
		require.True(t, covered[r], "§3.5 枚举值未被覆盖: %s", r)
	}
}

// TestCursorRetypeArgsByToolDefs 端到端：经 f4 原始字节取出 schema 还原 mcp_args，
// 失败时在 onFail 回调里记结构化原因（不含参数明文）。
func TestCursorRetypeArgsByToolDefs(t *testing.T) {
	def := cursorTestBuildToolDef(t, "get_weather",
		`{"type":"object","properties":{"city":{"type":"string"},"temp":{"type":"number"}}}`)
	defs := [][]byte{def}

	// 成功：deformed 数值还原。
	args := `{"city":"Beijing","temp":"21.5"}`
	var failedReason cursorRetypeErrReason
	got, err := cursorRetypeArgsByToolDefs("call_1", "get_weather", args, defs, func(r cursorRetypeErrReason) {
		failedReason = r
	})
	require.NoError(t, err)
	require.Equal(t, cursorRetypeErrReason(""), failedReason, "成功路径不应触发 onFail")
	require.JSONEq(t, `{"city":"Beijing","temp":21.5}`, got)

	// 失败：schema 不可解（type 数组）→ onFail 触发且原因正确。
	badDef := cursorTestBuildToolDef(t, "bad", `{"type":"object","properties":{"a":{"type":["string"]}}}`)
	_, err = cursorRetypeArgsByToolDefs("call_2", "bad", `{"a":1}`, [][]byte{badDef}, func(r cursorRetypeErrReason) {
		failedReason = r
	})
	require.ErrorIs(t, err, ErrCursorProtocol)
	require.Equal(t, cursorRetypeSchemaUnresolvable, failedReason)
}

// TestCursorToolSchemaIndexParity 两路径一致：同一夹具驱动双工与独立解析，ArgsJSON 逐字节相等。
func TestCursorToolSchemaIndexParity(t *testing.T) {
	def := cursorTestBuildToolDef(t, "get_weather",
		`{"type":"object","properties":{"city":{"type":"string"},"coords":{"type":"array","items":{"type":"number"}}}}`)
	defs := [][]byte{def}

	// 构造 deformed mcp_args（coords 以 {"item":[...]} 形态到达）。
	entries := map[string]*structpb.Value{
		"city":   structpb.NewStringValue("Beijing"),
		"coords": cursorTestVal(t, map[string]any{"item": []any{"1.5", "2"}}),
	}
	mcpArgs := cursorTestBuildMcpArgsOfficial(t, "call_p", "get_weather", entries)

	// ── 双工路径 ──
	execMsg := cursorAppendBytesField(nil, 11, mcpArgs)
	execParts, err := cursorDecodeProtobuf(execMsg)
	require.NoError(t, err)
	duplex := &cursorAgentDuplex{declaredToolDefs: defs}
	dupTC, err := duplex.cursorCaptureMcpArgs(execParts, 1, []byte("idf"))
	require.NoError(t, err)

	// ── 独立解析路径 ──
	serverMsg := cursorAppendBytesField(nil, cursorFldServerExecMessage, execMsg)
	parser := cursorNewStreamParser("m", "secret")
	parser.attachToolSchemas(defs)
	_, err = parser.cursorParseMessageFrame(serverMsg)
	require.NoError(t, err)
	calls := parser.capturedToolCalls()
	require.Len(t, calls, 1)

	// 两路径 ArgsJSON 逐字节相等（且已是定型后的结果）。
	require.Equal(t, dupTC.ArgsJSON, calls[0].ArgsJSON, "双工与独立解析路径必须产出一致的定型参数")
	require.JSONEq(t, `{"city":"Beijing","coords":[1.5,2]}`, dupTC.ArgsJSON)
}
