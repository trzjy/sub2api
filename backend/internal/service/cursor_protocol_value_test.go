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

// cursorTestBuildMcpArgsOfficial 用官方 Value 编码组装第三方原生 McpArgs 字节：
// f3 call_id、f5 真实工具名、每条 f2 是一条原生参数 map entry（{f1 key, f2 Value}）。
// Value 由官方编码器生成，不经 map[string]any/float64 往返。
func cursorTestBuildMcpArgsOfficial(t *testing.T, tcID, toolName string, entries map[string]*structpb.Value) []byte {
	t.Helper()
	var mcpArgs []byte
	for k, v := range entries {
		entry := cursorAppendStringField(nil, 1, k)
		entry = cursorAppendBytesField(entry, 2, cursorTestMarshalValue(t, v))
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
// 4. 整条原生运输帧：command / null / false / 数组 / 嵌套对象逐类型保真
// ---------------------------------------------------------------------------

func TestCursorDecodeMcpArgs(t *testing.T) {
	defs := cursorSDKTransportDeclaredDefs(t, "Bash")
	entries := map[string]*structpb.Value{
		"command": structpb.NewStringValue("echo hi"),
		"nothing": structpb.NewNullValue(),
		"flag":    structpb.NewBoolValue(false),
		"list":    cursorTestVal(t, []any{1.0, "x"}),
		"nested":  cursorTestVal(t, map[string]any{"x": true, "y": []any{}}),
	}
	args := cursorTestBuildMcpArgsOfficial(t, "call_77", "Bash", entries)
	id, name, argsJSON, err := cursorDecodeMcpArgs(args, defs)
	require.NoError(t, err)
	require.Equal(t, "call_77", id, "id 来自 f3")
	require.Equal(t, "Bash", name, "真实工具名来自 f5")
	require.JSONEq(t,
		`{"command":"echo hi","nothing":null,"flag":false,"list":[1,"x"],"nested":{"x":true,"y":[]}}`,
		argsJSON)

	var obj map[string]any
	require.NoError(t, json.Unmarshal([]byte(argsJSON), &obj))
	require.Len(t, obj, 5)
	require.Equal(t, "echo hi", obj["command"], "string 保真")
	require.Nil(t, obj["nothing"], "null 保真")
	require.Equal(t, false, obj["flag"], "bool false 保真")
	require.Equal(t, []any{float64(1), "x"}, obj["list"], "数组保真")
	require.Equal(t, map[string]any{"x": true, "y": []any{}}, obj["nested"], "嵌套对象保真")
}

// ---------------------------------------------------------------------------
// 5. 字节稳定性：同一份原生帧连续解码 64 次逐次与期望完全相同
// ---------------------------------------------------------------------------

func TestCursorValueTypes(t *testing.T) {
	defs := cursorSDKTransportDeclaredDefs(t, "get_weather")
	entries := map[string]*structpb.Value{
		"i":   structpb.NewNumberValue(7),
		"s":   structpb.NewStringValue("Beijing"),
		"b":   structpb.NewBoolValue(true),
		"o":   cursorTestVal(t, map[string]any{"x": 1.0, "y": []any{1.0, 2.0}}),
		"arr": cursorTestVal(t, []any{1.0, "a"}),
	}
	args := cursorTestBuildMcpArgsOfficial(t, "call_1", "get_weather", entries)
	const want = `{"arr":[1,"a"],"b":true,"i":7,"o":{"x":1,"y":[1,2]},"s":"Beijing"}`

	for i := 0; i < 64; i++ {
		_, _, argsJSON, err := cursorDecodeMcpArgs(args, defs)
		require.NoError(t, err)
		require.Equal(t, want, argsJSON, "第 %d 次解码结果必须与期望逐字节一致", i)
	}
}

// ---------------------------------------------------------------------------
// 6. 端到端：真实 exec.f11 帧 → duplex respond → parser 读同一份共享记录 → SSE 取参
// ---------------------------------------------------------------------------

func TestCursorDuplex(t *testing.T) {
	const originalJSON = `{"city":"Beijing","days":3,"flag":true}`
	expectedArgs := cursorSDKTransportExpectedArgsJSON(t, originalJSON)
	args := cursorSDKTransportBuildMcpArgs("call_42", "get_weather", originalJSON)
	defs := cursorSDKTransportDeclaredDefs(t, "get_weather")

	initBody, err := cursorConvertRequest(cursorSDKTransportRequest("get_weather"), "grok-4.7")
	require.NoError(t, err)

	// 真实 duplex：共享记录由 duplex 拥有解码权（parser 不再二次解码）。
	d, err := newCursorAgentDuplex(initBody, newCursorToolDelegations())
	require.NoError(t, err)
	defer d.close(nil)
	// 请求侧 f4 仍为旧包装；本卡只改读取侧，注入与原生 f5 同名的本地声明夹具。
	d.declaredToolDefs = defs
	require.Len(t, d.declaredToolDefs, 1)

	frame := cursorSDKTransportExecFrame(args) // f11 mcp_args，respond 吃剥离 envelope 的 payload
	done, err := d.respond(frame)
	require.NoError(t, err)
	require.False(t, done, "mcp_args 委托不得终结双向连接")

	snapshot := d.delegations.snapshot()
	require.Len(t, snapshot, 1, "duplex 侧委托记录必须恰好一条（唯一解码点）")
	require.Equal(t, "call_42", snapshot[0].ID)
	require.Equal(t, "get_weather", snapshot[0].Name)
	require.Equal(t, expectedArgs, snapshot[0].ArgsJSON, "原生参数必须按 Value 解码结果交付")

	// parser 消费同一帧与 turn_ended 帧，attach 同一份共享记录 → 不得二次解码。
	body := cursorToolTestStreamBody(
		cursorSDKTransportExecFrame(args),
		cursorToolTestTurnEndedFrame(10, 5),
	)
	p := cursorNewStreamParser("grok-4.7")
	p.attachToolDelegations(d.delegations)
	p.attachToolSchemas(d.declaredToolDefs)
	_, err = p.cursorParseStream(body)
	require.NoError(t, err)
	require.Equal(t, snapshot, p.capturedToolCalls(), "parser 未二次解码，应与共享记录逐字一致")

	// 无 duplex 的独立 parser 从同一份帧自行解码，装配同一份声明夹具。
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
