//go:build unit

package service

// w3-value-20261024：Cursor McpArgs 的 google.protobuf.Value 改用标准库解码。
//
// 全部输入一律用官方 protobuf 编码器生成（proto.Marshal(structpb.NewValue/...)），
// 期望值写死 JSON 字面量——绝不用被测实现自身的输出当期望值。

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// cursorTestVal 用官方 structpb.NewValue（返回 (Value,error)）构造 Value。
func cursorTestVal(t *testing.T, v any) *structpb.Value {
	t.Helper()
	val, err := structpb.NewValue(v)
	require.NoError(t, err)
	return val
}

// cursorTestMarshalValue 用官方编码器把 structpb.Value 序列化成 Value 消息字节。
func cursorTestMarshalValue(t *testing.T, v *structpb.Value) []byte {
	t.Helper()
	b, err := proto.Marshal(v)
	require.NoError(t, err)
	return b
}

// cursorTestBuildMcpArgsOfficial 用官方 Value 编码组装一条真实 McpArgs 字节
// （f3 tool_call_id / f5 tool_name / f2 args，每条 entry = f1 key + f2 Value 直内联）。
func cursorTestBuildMcpArgsOfficial(t *testing.T, tcID, toolName string, entries map[string]*structpb.Value) []byte {
	t.Helper()
	var mcpArgs []byte
	for k, v := range entries {
		valBytes := cursorTestMarshalValue(t, v)
		entry := cursorAppendStringField(nil, 1, k)
		entry = cursorAppendBytesField(entry, 2, valBytes)
		mcpArgs = cursorAppendBytesField(mcpArgs, 2, entry)
	}
	mcpArgs = cursorAppendStringField(mcpArgs, 3, tcID)
	mcpArgs = cursorAppendStringField(mcpArgs, 5, toolName)
	return mcpArgs
}

// ---------------------------------------------------------------------------
// 1. 标准类型矩阵：逐项断言精确 JSON 文本
// ---------------------------------------------------------------------------

func TestCursorDecodeMcpValue(t *testing.T) {
	cases := []struct {
		name string
		in   *structpb.Value
		want string
	}{
		{"null", structpb.NewNullValue(), "null"},
		{"int 7", structpb.NewNumberValue(7), "7"},
		{"int 0", structpb.NewNumberValue(0), "0"},
		{"neg -42", structpb.NewNumberValue(-42), "-42"},
		{"frac 1.25", structpb.NewNumberValue(1.25), "1.25"},
		{"string", structpb.NewStringValue("Beijing"), `"Beijing"`},
		{"bool true", structpb.NewBoolValue(true), "true"},
		{"bool false", structpb.NewBoolValue(false), "false"},
		{"numeric string stays string", structpb.NewStringValue("7"), `"7"`},
		{"nested object", cursorTestVal(t, map[string]interface{}{
			"a": -42.0,
			"b": []any{true, nil, "x"},
			"c": 1.5,
			"d": map[string]interface{}{},
		}), `{"a":-42,"b":[true,null,"x"],"c":1.5,"d":{}}`},
		{"empty object", cursorTestVal(t, map[string]interface{}{}), "{}"},
		{"array three", cursorTestVal(t, []any{1.0, "a", true}), `[1,"a",true]`},
		{"empty array", cursorTestVal(t, []any{}), "[]"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got, err := cursorDecodeMcpValue(cursorTestMarshalValue(t, c.in))
			require.NoError(t, err)
			require.Equal(t, c.want, got)
		})
	}
}

// ---------------------------------------------------------------------------
// 2. 失败关闭矩阵：每项必须 ErrorIs(ErrCursorProtocol) 且返回空串（绝不为 "null"）
// ---------------------------------------------------------------------------

func TestCursorDecodeMcpValueFailure(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"empty (kind unset)", []byte{}},
		{"only unknown field", []byte{0xe6, 0x06, 0x00}}, // field 99, wire 0, varint 0
		{"double truncated", []byte{0x15, 0x01, 0x02, 0x03}},
		{"string truncated", []byte{0x1a, 0x05, 0xab}},
		{"illegal wire type 0x0f", []byte{0x0f}},
		{"tag 0", []byte{0x00}},
		{"NaN number", cursorTestMarshalValue(t, structpb.NewNumberValue(math.NaN()))},
		{"+Inf number", cursorTestMarshalValue(t, structpb.NewNumberValue(math.Inf(1)))},
		{"-Inf number", cursorTestMarshalValue(t, structpb.NewNumberValue(math.Inf(-1)))},
		{"NaN nested in object", cursorTestMarshalValue(t, cursorTestVal(t, map[string]interface{}{
			"bad": math.NaN()}))},
		{"Inf in array element", cursorTestMarshalValue(t, cursorTestVal(t, []any{
			1.0, math.Inf(1)}))},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got, err := cursorDecodeMcpValue(c.in)
			require.ErrorIs(t, err, ErrCursorProtocol)
			require.Equal(t, "", got, "失败路径必须返回空串，绝不能是 null")
		})
	}
}

// ---------------------------------------------------------------------------
// 4. 整条 McpArgs 类型保真：经 cursorDecodeMcpArgs 后 Go 具体类型正确
// ---------------------------------------------------------------------------

func TestCursorDecodeMcpArgs(t *testing.T) {
	entries := map[string]*structpb.Value{
		"i":    structpb.NewNumberValue(7),
		"neg":  structpb.NewNumberValue(-42),
		"frac": structpb.NewNumberValue(1.25),
		"s":    structpb.NewStringValue("hi"),
		"b":    structpb.NewBoolValue(true),
		"o":    cursorTestVal(t, map[string]interface{}{"x": 1.0}),
		"arr":  cursorTestVal(t, []any{1.0, "a"}),
		"n":    structpb.NewNullValue(),
	}
	args := cursorTestBuildMcpArgsOfficial(t, "call_77", "get_weather", entries)
	id, name, argsJSON, err := cursorDecodeMcpArgs(args)
	require.NoError(t, err)
	require.Equal(t, "call_77", id)
	require.Equal(t, "get_weather", name)

	var obj map[string]any
	require.NoError(t, json.Unmarshal([]byte(argsJSON), &obj))
	require.Len(t, obj, 8)
	require.Equal(t, float64(7), obj["i"])
	require.Equal(t, float64(-42), obj["neg"])
	require.Equal(t, 1.25, obj["frac"])
	require.Equal(t, "hi", obj["s"])
	require.Equal(t, true, obj["b"])
	require.Equal(t, nil, obj["n"])
	o, ok := obj["o"].(map[string]any)
	require.True(t, ok, "object must decode to map[string]any")
	require.Equal(t, 1.0, o["x"])
	arr, ok := obj["arr"].([]any)
	require.True(t, ok, "array must decode to []any")
	require.Len(t, arr, 2)
	require.Equal(t, float64(1), arr[0])
	require.Equal(t, "a", arr[1])
}

// ---------------------------------------------------------------------------
// 5. 字节稳定性：同一份 args 连续解码 64 次逐次完全相同，且不含 ", "
// ---------------------------------------------------------------------------

func TestCursorValueTypes(t *testing.T) {
	entries := map[string]*structpb.Value{
		"i":   structpb.NewNumberValue(7),
		"s":   structpb.NewStringValue("Beijing"),
		"b":   structpb.NewBoolValue(true),
		"o":   cursorTestVal(t, map[string]interface{}{"x": 1.0, "y": []any{1, 2}}),
		"arr": cursorTestVal(t, []any{1.0, "a"}),
	}
	args := cursorTestBuildMcpArgsOfficial(t, "call_1", "t", entries)

	var first string
	for i := 0; i < 64; i++ {
		_, _, argsJSON, err := cursorDecodeMcpArgs(args)
		require.NoError(t, err)
		if i == 0 {
			first = argsJSON
		}
		require.Equal(t, first, argsJSON, "第 %d 次解码结果必须与首次一致", i)
		require.NotContains(t, argsJSON, ", ", "protojson 随机空格必须被 json.Compact 去除")
	}
}

// ---------------------------------------------------------------------------
// 6. 端到端：真实 exec.f11 帧 → duplex respond → parser 读同一份共享记录 → SSE 取参
// ---------------------------------------------------------------------------

func TestCursorDuplex(t *testing.T) {
	args := cursorToolTestBuildMcpArgs("call_42", "get_weather", []cursorToolTestMcpArgEntry{
		{key: "city", kind: "string", val: "Beijing"},
		{key: "days", kind: "number", val: "3"},
		{key: "flag", kind: "bool", val: "true"},
	})

	// 本请求真实声明的工具：f4 原始字节的唯一来源（cursorConvertRequest 实际发出的字节），
	// duplex 与独立 parser 共用同一份，绝不另起编码器、绝不用 req.Tools 重新序列化。
	req := &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools: []apicompat.ChatTool{{Type: "function", Function: &apicompat.ChatFunction{
			Name: "get_weather",
			Parameters: json.RawMessage(`{"type":"object","properties":{` +
				`"city":{"type":"string"},"days":{"type":"number"},"flag":{"type":"boolean"}}}`),
		}}},
	}
	initBody, err := cursorConvertRequest(req, "grok-4.7")
	require.NoError(t, err)

	// 真实 duplex：共享记录由 duplex 拥有解码权（parser 不再二次解码）。
	d, err := newCursorAgentDuplex(initBody, newCursorToolDelegations())
	require.NoError(t, err)
	defer d.close(nil)
	require.Len(t, d.declaredToolDefs, 1, "AgentRun 请求 f4 必须携带 get_weather 的原始 McpToolDefinition")

	frame := cursorToolTestExecFrame(true, args) // f11 mcp_args，respond 吃剥离 envelope 的 payload
	done, err := d.respond(frame)
	require.NoError(t, err)
	require.False(t, done, "mcp_args 委托不得终结双向连接")

	snapshot := d.delegations.snapshot()
	require.Len(t, snapshot, 1, "duplex 侧委托记录必须恰好一条（唯一解码点）")
	require.Equal(t, "call_42", snapshot[0].ID)
	require.Equal(t, "get_weather", snapshot[0].Name)
	require.Equal(t, `{"city":"Beijing","days":3,"flag":true}`, snapshot[0].ArgsJSON)

	// parser 消费同一帧与 turn_ended 帧，attach 同一份共享记录 → 不得二次解码。
	body := cursorToolTestStreamBody(
		cursorToolTestExecFrame(true, args),
		cursorToolTestTurnEndedFrame(10, 5),
	)
	p := cursorNewStreamParser("grok-4.7")
	p.attachToolDelegations(d.delegations)
	// 与生产装配一致（cursor_execution_service.go:577-580）：独立解析路径挂载同一份 f4 原始字节。
	p.attachToolSchemas(d.declaredToolDefs)
	_, err = p.cursorParseStream(body)
	require.NoError(t, err)
	require.Equal(t, snapshot, p.capturedToolCalls(), "parser 未二次解码，应与共享记录逐字一致")

	// SSE 取 tool_calls[].function.arguments，解析后同型同值、id/name 正确。
	// cursorParseStreamToSSE 内部自建 parser、无法注入 f4；这里按同一装配显式建独立 parser
	//（挂载同一份 f4 原始字节）并逐行拼 SSE + [DONE] 哨兵，输出口径与 cursorParseStreamToSSE 相同。
	sp := cursorNewStreamParser("grok-4.7")
	sp.attachToolSchemas(d.declaredToolDefs)
	lines, err := sp.cursorParseStream(body)
	require.NoError(t, err)
	var sse []byte
	for _, l := range lines {
		sse = append(sse, l...)
	}
	sse = append(sse, cursorSSEDoneSentinel...)
	tcID, tcName, tcArgs := cursorTestExtractToolCallFromSSE(t, sse)
	require.Equal(t, "call_42", tcID)
	require.Equal(t, "get_weather", tcName)
	var m map[string]any
	require.NoError(t, json.Unmarshal(tcArgs, &m))
	require.Len(t, m, 3)
	require.Equal(t, "Beijing", m["city"])
	require.Equal(t, float64(3), m["days"])
	require.Equal(t, true, m["flag"])
}

// cursorTestExtractToolCallFromSSE 从 SSE 字节流里取出首个 tool_call 的 id/name/arguments。
func cursorTestExtractToolCallFromSSE(t *testing.T, sse []byte) (id, name string, args json.RawMessage) {
	t.Helper()
	for _, line := range strings.Split(string(sse), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		for _, c := range chunk.Choices {
			for _, tc := range c.Delta.ToolCalls {
				if tc.ID != "" {
					id = tc.ID
				}
				if tc.Function.Name != "" {
					name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					args = json.RawMessage(tc.Function.Arguments)
				}
			}
		}
	}
	require.NotEmpty(t, args, "SSE 必须包含 tool_calls arguments")
	return
}
