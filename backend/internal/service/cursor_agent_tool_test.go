//go:build unit

package service

// O11-B Cursor Agent 工具闭环测试：请求声明 + duplex 委托 + 响应转换 + 续轮文本化。
//
// 断言以字节级回读为主（不只断言"不报错"），确保字段号与嵌套层级真的写对了。

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// cursorTestFieldString 便捷取某字段号的字符串值（字段缺失返回空串）。
func cursorTestFieldString(fields []cursorPBField, n int) string {
	f, ok := cursorFindField(fields, n)
	if !ok {
		return ""
	}
	return f.String()
}

// cursorTestF6SchemaOf 取出逐工具原生定义的 f6 input_schema_json 原文本，并断言 f3 不存在。
// 本 helper 与 cursor_protocol_request_test.go 共用（同包）。
func cursorTestF6SchemaOf(t *testing.T, fields []cursorPBField) string {
	t.Helper()
	_, hasF3 := cursorFindField(fields, 3)
	require.False(t, hasF3, "native tool definition must not carry f3 schema encoding")
	f6, ok := cursorFindField(fields, cursorFldMcpToolDefInputSchemaJSON)
	require.True(t, ok, "f6 input_schema_json must be present")
	return f6.String()
}

// cursorDuplexReadReplyAsync 启动一个并发读取 goroutine 并返回结果通道。
//
// 为什么必须并发：d.send 走 **内存 io.Pipe**，Write 会阻塞直到对端 PipeReader 被读取；
// 而 d.respond 是同步调用、会在 d.send 处阻塞。若测试先调 d.respond 再读 d.reader，
// 则两端互等 → 死锁。生产路径由 responseBody 的读取 goroutine 异步消费，故测试须同样
// 在独立 goroutine 里读取，先起读、再调 respond，让 send 得以解除阻塞。
func cursorDuplexReadReplyAsync(d *cursorAgentDuplex) <-chan struct {
	b   []byte
	err error
} {
	ch := make(chan struct {
		b   []byte
		err error
	}, 1)
	go func() {
		b, err := cursorDuplexReadFrame(d.reader)
		ch <- struct {
			b   []byte
			err error
		}{b, err}
	}()
	return ch
}

// cursorTestBlobContent 解出历史 blob 的 content（blob value 是 JSON，HTML 转义后的
// `<` 会被 json.Unmarshal 还原，故用解码结果断言而非原始字节）。
//
// 历史 content 有两种形状：user blob 为字符串；assistant blob 为
// `[{"type":"text","text":"…"}]` 数组。两种都要能解出纯文本。
func cursorTestBlobContent(t *testing.T, blob [2][]byte) string {
	t.Helper()
	var u struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	require.NoError(t, json.Unmarshal(blob[1], &u))
	if len(u.Content) == 0 {
		return ""
	}
	// 字符串形态（user blob）。
	if u.Content[0] == '"' {
		var s string
		require.NoError(t, json.Unmarshal(u.Content, &s))
		return s
	}
	// 数组形态（assistant blob：text parts 拼接）。
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	require.NoError(t, json.Unmarshal(u.Content, &parts))
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// 构造辅助
// ---------------------------------------------------------------------------

// cursorToolTestMcpArgEntry 是一个 mcp_args 的原生参数条目（key + google.protobuf.Value）。
type cursorToolTestMcpArgEntry struct {
	key  string
	kind string // "string" | "number" | "null" | "bool" | "json"
	val  string
}

// cursorToolTestNativeValue 把测试条目编码成官方 google.protobuf.Value 消息字节。
// "json" 分支直接交给 protojson 解析，覆盖数组/嵌套对象等任意 Value 类型。
func cursorToolTestNativeValue(e cursorToolTestMcpArgEntry) []byte {
	var v *structpb.Value
	switch e.kind {
	case "string":
		v = structpb.NewStringValue(e.val)
	case "number":
		n, err := strconv.ParseFloat(e.val, 64)
		if err != nil {
			panic("bad number value: " + err.Error())
		}
		v = structpb.NewNumberValue(n)
	case "bool":
		v = structpb.NewBoolValue(e.val == "true")
	case "null":
		v = structpb.NewNullValue()
	case "json":
		v = &structpb.Value{}
		if err := protojson.Unmarshal([]byte(e.val), v); err != nil {
			panic("bad json value: " + err.Error())
		}
	default:
		panic("unknown arg kind: " + e.kind)
	}
	b, err := proto.Marshal(v)
	if err != nil {
		panic("marshal mcp value: " + err.Error())
	}
	return b
}

// cursorToolTestBuildMcpArgs 构造第三方原生 f11（mcp_args）载荷：f3 tool_call_id、
// f5 真实工具名，每条 f2 = 一条 map entry（{f1 原参数键, f2 Value}）。签名保持不变，
// 参数键为客户端自有键，不引入包装二键。
func cursorToolTestBuildMcpArgs(tcID, toolName string, entries []cursorToolTestMcpArgEntry) []byte {
	var mcpArgs []byte
	for _, e := range entries {
		entry := cursorAppendStringField(nil, 1, e.key)
		entry = cursorAppendBytesField(entry, 2, cursorToolTestNativeValue(e))
		mcpArgs = cursorAppendBytesField(mcpArgs, 2, entry)
	}
	mcpArgs = cursorAppendStringField(mcpArgs, 3, tcID)
	mcpArgs = cursorAppendStringField(mcpArgs, 5, toolName)
	return mcpArgs
}

// cursorToolTestExecFrame 构造携带 mcp_args（f11）或 mcp_state（f36）的 AgentServerMessage 帧。
func cursorToolTestExecFrame(useF11 bool, payload []byte) []byte {
	exec := cursorAppendVarintField(nil, 1, 7) // f1 exec_id
	exec = cursorAppendBytesField(exec, 15, []byte("exec-id"))
	if useF11 {
		exec = cursorAppendBytesField(exec, 11, payload) // f11 mcp_args
	} else {
		exec = cursorAppendBytesField(exec, 36, payload) // f36 mcp_state_exec_args
	}
	return cursorAppendBytesField(nil, 2, exec) // AgentServerMessage.f2
}

// cursorToolTestTurnEndedFrame 构造 interaction_update(turn_ended) 帧（含 usage）。
func cursorToolTestTurnEndedFrame(input, output int) []byte {
	usage := cursorAppendVarintField(nil, 1, uint64(input))
	usage = cursorAppendVarintField(usage, 2, uint64(output))
	turn := cursorAppendBytesField(nil, 14, usage)
	return cursorAppendBytesField(nil, 1, turn) // AgentServerMessage.f1 interaction_update
}

// cursorToolTestStreamBody 把若干帧拼成带 envelope 的流体。
func cursorToolTestStreamBody(frames ...[]byte) []byte {
	var buf []byte
	for _, f := range frames {
		buf = append(buf, cursorEncodeEnvelope(f)...)
	}
	return buf
}

// cursorTestDeclaredToolDefs 从声明了工具的请求体里解出 f4 每条 McpToolDefinition 原始字节
// （与 duplex 路径同源：经 cursorConvertRequest 实际发出的 f4），供独立解析路径
// attachToolSchemas 注入，使 parser 能按 tool_name 取回 input_schema 做 retype。
// 不放宽 schema_absent：来源必须是真实 f4 原始字节，不得改用 req.Tools 重新序列化。
func cursorTestDeclaredToolDefs(t *testing.T, req *apicompat.ChatCompletionsRequest) [][]byte {
	t.Helper()
	body, err := cursorConvertRequest(req, "grok-4.7")
	require.NoError(t, err)
	run := cursorAgentRunOf(t, body)
	mcp, ok := cursorFindField(run, cursorFldRunMcpTools)
	require.True(t, ok, "f4 (McpTools) must be present")
	if len(mcp.Bytes) == 0 {
		return [][]byte{}
	}
	mcpFields, e := cursorDecodeProtobuf(mcp.Bytes)
	require.NoError(t, e)
	defs := [][]byte{}
	for _, def := range cursorAllFields(mcpFields, cursorFldMcpToolsTool) {
		defs = append(defs, def.Bytes)
	}
	return defs
}

// cursorTestTransportDefs 构造本地 f4 定义夹具：每条 McpToolDefinition 的 f5 是
// 真实工具名（names），与第三方原生 mcp_args.f5 的声明校验同源。仅用于解码测试；
// mcp_state 字节级回显测试不调用本函数，它用真实 f4 原始字节。
func cursorTestTransportDefs(t *testing.T, names ...string) [][]byte {
	t.Helper()
	defs := make([][]byte, 0, len(names))
	for _, name := range names {
		defs = append(defs, cursorTestBuildToolDef(t, name, `{"type":"object"}`))
	}
	return defs
}

// cursorTestDeclaredToolNames 从真实 f4 定义里按声明顺序取工具名（f1）。
func cursorTestDeclaredToolNames(t *testing.T, req *apicompat.ChatCompletionsRequest) []string {
	t.Helper()
	defs := cursorTestDeclaredToolDefs(t, req)
	names := make([]string, 0, len(defs))
	for _, def := range defs {
		fields, err := cursorDecodeProtobuf(def)
		require.NoError(t, err)
		names = append(names, cursorTestFieldString(fields, cursorFldMcpToolDefName))
	}
	return names
}

// cursorTestBuildToolDef 构造一条 McpToolDefinition 原始字节（f1 名 / f2 description /
// f4 provider / f5 tool_name / f6 input_schema_json 原文本），与 cursorBuildMcpTools
// 生产构造同形（仅用于测试夹具，不重新序列化 req.Tools）。
func cursorTestBuildToolDef(t *testing.T, toolName, schema string) []byte {
	t.Helper()
	var def []byte
	def = cursorAppendStringField(def, cursorFldMcpToolDefName, toolName)
	def = cursorAppendStringField(def, cursorFldMcpToolDefDescription, "desc-"+toolName)
	def = cursorAppendStringField(def, cursorFldMcpToolDefProvider, cursorMcpProviderIdentifier)
	def = cursorAppendStringField(def, cursorFldMcpToolDefToolName, toolName)
	def = cursorAppendStringField(def, cursorFldMcpToolDefInputSchemaJSON, schema)
	return def
}

// ---------------------------------------------------------------------------
// 1. 声明编码：每个真实工具一条原生定义（f1/f2/f4/f5/f6，无 f3）
// ---------------------------------------------------------------------------

func TestCursorToolDeclarationEncoding(t *testing.T) {
	const schema2 = `{"type":"object","x":1,"big":9007199254740993}`
	req := &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools: []apicompat.ChatTool{
			{Type: "function", Function: &apicompat.ChatFunction{
				Name: "get_weather", Description: "d1",
				Parameters: json.RawMessage(`{"type":"object"}`)}},
			{Type: "function", Function: &apicompat.ChatFunction{
				Name: "search", Description: "d2",
				Parameters: json.RawMessage(schema2)}},
		},
	}
	body, err := cursorConvertRequest(req, "grok-4.7")
	require.NoError(t, err)

	run := cursorAgentRunOf(t, body)
	mcp, ok := cursorFindField(run, cursorFldRunMcpTools)
	require.True(t, ok, "f4 (McpTools) must be present")
	require.NotEmpty(t, mcp.Bytes)

	mcpFields, e := cursorDecodeProtobuf(mcp.Bytes)
	require.NoError(t, e)
	defs := cursorAllFields(mcpFields, cursorFldMcpToolsTool)
	require.Len(t, defs, 2, "N tools must produce N native definitions")

	want := []struct{ name, desc, schema string }{
		{"get_weather", "d1", `{"type":"object"}`},
		{"search", "d2", schema2},
	}
	// f29 教学提示不得再出现。
	_, hasF29 := cursorFindField(run, 29)
	require.False(t, hasF29, "f29 system_prompt_spec teaching prompt must be removed")
	for i, def := range defs {
		fields, e := cursorDecodeProtobuf(def.Bytes)
		require.NoError(t, e)
		require.Equal(t, want[i].name, cursorTestFieldString(fields, cursorFldMcpToolDefName), "f1 name")
		require.Equal(t, want[i].name, cursorTestFieldString(fields, cursorFldMcpToolDefToolName), "f5 tool_name")
		require.Equal(t, want[i].desc, cursorTestFieldString(fields, cursorFldMcpToolDefDescription), "f2 description")
		require.Equal(t, cursorMcpProviderIdentifier, cursorTestFieldString(fields, cursorFldMcpToolDefProvider), "f4 provider")
		require.Equal(t, want[i].schema, cursorTestF6SchemaOf(t, fields), "f6 input_schema_json must be the original schema bytes")
		require.NotContains(t, want[i].schema, "float64")
	}
}

// ---------------------------------------------------------------------------
// 2/7. tool_choice 四值语义（逐工具声明的子集）
// ---------------------------------------------------------------------------

func TestCursorToolChoiceSemantics(t *testing.T) {
	tools := []apicompat.ChatTool{
		{Type: "function", Function: &apicompat.ChatFunction{Name: "a"}},
		{Type: "function", Function: &apicompat.ChatFunction{Name: "b"}},
	}
	base := func(choice json.RawMessage) *apicompat.ChatCompletionsRequest {
		return &apicompat.ChatCompletionsRequest{
			Messages:   []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
			Tools:      tools,
			ToolChoice: choice,
		}
	}

	t.Run("省略/auto → 全部声明", func(t *testing.T) {
		require.Equal(t, []string{"a", "b"}, cursorTestDeclaredToolNames(t, base(nil)))
		require.Equal(t, []string{"a", "b"}, cursorTestDeclaredToolNames(t, base(json.RawMessage(`"auto"`))))
	})
	t.Run("required → 全部声明", func(t *testing.T) {
		require.Equal(t, []string{"a", "b"}, cursorTestDeclaredToolNames(t, base(json.RawMessage(`"required"`))))
	})
	t.Run("none → 不声明（等价无 tools）", func(t *testing.T) {
		require.Empty(t, cursorTestDeclaredToolNames(t, base(json.RawMessage(`"none"`))))
	})
	t.Run("指定函数 → 仅声明该函数", func(t *testing.T) {
		require.Equal(t, []string{"b"}, cursorTestDeclaredToolNames(t, base(json.RawMessage(`{"type":"function","function":{"name":"b"}}`))))
	})
	t.Run("指定函数不在列表 → 回落全部（不凭空声明）", func(t *testing.T) {
		require.Equal(t, []string{"a", "b"}, cursorTestDeclaredToolNames(t, base(json.RawMessage(`{"type":"function","function":{"name":"missing"}}`))))
	})
}

// ---------------------------------------------------------------------------
// 3. mcp_state 回包：id/exec_id 配对 + McpStateServer f1/f2/f5（repeated 逐项）
// ---------------------------------------------------------------------------

func TestCursorDuplexMcpStateReply(t *testing.T) {
	// 构造带 2 个工具声明的 AgentRun，让 duplex 解析出 declaredToolDefs。
	initReq := &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools: []apicompat.ChatTool{
			{Type: "function", Function: &apicompat.ChatFunction{Name: "get_weather"}},
			{Type: "function", Function: &apicompat.ChatFunction{Name: "search"}},
		},
	}
	initBody, err := cursorConvertRequest(initReq, "grok-4.7")
	require.NoError(t, err)

	d, err := newCursorAgentDuplex(initBody, nil)
	require.NoError(t, err)
	defer d.close(nil)
	require.Len(t, d.declaredToolDefs, 2, "逐工具声明：f4 有两条定义")

	// 期望：回包 f5 与请求 f4 的每条 McpToolDefinition 原始字节逐字节一致（字节级保真，依据 T7）。
	runForExpect := cursorAgentRunOf(t, initBody)
	mcpForExpect, ok := cursorFindField(runForExpect, cursorFldRunMcpTools)
	require.True(t, ok)
	mcpFieldsForExpect, e := cursorDecodeProtobuf(mcpForExpect.Bytes)
	require.NoError(t, e)
	defRaw := [][]byte{}
	for _, def := range cursorAllFields(mcpFieldsForExpect, cursorFldMcpToolsTool) {
		defRaw = append(defRaw, def.Bytes)
	}
	require.Len(t, defRaw, 2, "逐工具声明：f4 有两条 McpToolDefinition 原始字节")

	frame := cursorToolTestExecFrame(false, nil) // f36 mcp_state（respond 吃 envelope 剥离后的 payload）
	// 先起并发读取，再调 respond：d.send 走 io.Pipe 会阻塞直到被读，避免死锁。
	replyCh := cursorDuplexReadReplyAsync(d)
	done, err := d.respond(frame)
	require.NoError(t, err)
	require.False(t, done, "mcp_state reply must not terminate the connection")

	// 读取 duplex 回写（AgentClientMessage）。
	rep := <-replyCh
	require.NoError(t, rep.err)
	reply := rep.b
	outer, err := cursorDecodeProtobuf(reply)
	require.NoError(t, err)
	exec, ok := cursorFindField(outer, 2) // ExecClientMessage
	require.True(t, ok)
	execFields, err := cursorDecodeProtobuf(exec.Bytes)
	require.NoError(t, err)
	// id/exec_id 配对。
	require.Equal(t, uint64(7), cursorVarintField(execFields, 1))
	require.Equal(t, "exec-id", cursorTestFieldString(execFields, 15))

	// f36 = McpStateExecResult → f1 success → f1 servers → f1/f2/f5。
	stateField, ok := cursorFindField(execFields, 36)
	require.True(t, ok, "reply must carry f36 McpStateExecResult")
	state, err := cursorDecodeProtobuf(stateField.Bytes)
	require.NoError(t, err)
	success, ok := cursorFindField(state, 1)
	require.True(t, ok)
	successFields, err := cursorDecodeProtobuf(success.Bytes)
	require.NoError(t, err)
	server, ok := cursorFindField(successFields, 1) // servers (repeated, one server)
	require.True(t, ok)
	serverFields, err := cursorDecodeProtobuf(server.Bytes)
	require.NoError(t, err)
	require.Equal(t, "sub2api", cursorTestFieldString(serverFields, 1), "f1 server_name")
	require.Equal(t, "sub2api", cursorTestFieldString(serverFields, 2), "f2 server_identifier")

	// f5 tools：每个声明工具**单独**一条 f5（repeated 逐项，非拼接），且回包 f5 内层与
	// 请求 f4 的 McpToolDefinition 原始字节逐字节一致（字节级保真，依据活体探针 T7）。
	toolEntries := cursorAllFields(serverFields, 5)
	require.Len(t, toolEntries, len(defRaw), "f5 must have one entry per declared tool")
	for i, te := range toolEntries {
		require.Equal(t, defRaw[i], te.Bytes,
			"f5 entry %d must equal request McpToolDefinition raw bytes (byte-level fidelity)", i)
	}
	tf, e := cursorDecodeProtobuf(toolEntries[0].Bytes)
	require.NoError(t, e)
	require.Equal(t, "get_weather", cursorTestFieldString(tf, cursorFldMcpToolDefName))
	require.Equal(t, "get_weather", cursorTestFieldString(tf, cursorFldMcpToolDefToolName))
}

// ---------------------------------------------------------------------------
// 4. mcp_args 捕获：map args → JSON（string/number/null）；tool_call_id/tool_name 正确
// ---------------------------------------------------------------------------

func TestCursorDecodeMcpArgsMap(t *testing.T) {
	defs := cursorTestTransportDefs(t, "get_weather", "Bash")
	t.Run("string/number/null 用例", func(t *testing.T) {
		args := cursorToolTestBuildMcpArgs("call_42", "get_weather", []cursorToolTestMcpArgEntry{
			{key: "city", kind: "string", val: "Beijing"},
			{key: "days", kind: "number", val: "3"},
			{key: "verbose", kind: "null"},
		})
		id, name, argsJSON, err := cursorDecodeMcpArgs(args, defs)
		require.NoError(t, err)
		require.Equal(t, "call_42", id)
		require.Equal(t, "get_weather", name)
		require.Equal(t, `{"city":"Beijing","days":3,"verbose":null}`, argsJSON)
	})

	t.Run("原生键逐类型保真（command/null/false/数组/嵌套对象）", func(t *testing.T) {
		args := cursorToolTestBuildMcpArgs("call_types", "Bash", []cursorToolTestMcpArgEntry{
			{key: "command", kind: "string", val: "echo hi"},
			{key: "nothing", kind: "null"},
			{key: "flag", kind: "bool", val: "false"},
			{key: "list", kind: "json", val: `[1,"x"]`},
			{key: "nested", kind: "json", val: `{"x":true}`},
		})
		id, name, argsJSON, err := cursorDecodeMcpArgs(args, defs)
		require.NoError(t, err)
		require.Equal(t, "call_types", id, "id 来自 f3")
		require.Equal(t, "Bash", name, "真实工具名来自 f5，不是包装常量")
		require.JSONEq(t,
			`{"command":"echo hi","nothing":null,"flag":false,"list":[1,"x"],"nested":{"x":true}}`,
			argsJSON)
		var obj map[string]any
		require.NoError(t, json.Unmarshal([]byte(argsJSON), &obj))
		require.Equal(t, "echo hi", obj["command"])
		require.Nil(t, obj["nothing"])
		require.Equal(t, false, obj["flag"])
		require.Equal(t, []any{float64(1), "x"}, obj["list"])
		require.Equal(t, map[string]any{"x": true}, obj["nested"])
	})

	t.Run("空 args 返回空对象", func(t *testing.T) {
		args := cursorToolTestBuildMcpArgs("c", "get_weather", nil)
		_, _, argsJSON, err := cursorDecodeMcpArgs(args, defs)
		require.NoError(t, err)
		require.Equal(t, "{}", argsJSON)
	})

	t.Run("未声明工具名失败关闭", func(t *testing.T) {
		args := cursorToolTestBuildMcpArgs("c", "not_declared", nil)
		_, _, _, err := cursorDecodeMcpArgs(args, defs)
		require.Error(t, err)
		require.Contains(t, err.Error(), `undeclared tool name "not_declared"`)
	})

	t.Run("declaredToolDefs 为空失败关闭", func(t *testing.T) {
		args := cursorToolTestBuildMcpArgs("c", "get_weather", nil)
		_, _, _, err := cursorDecodeMcpArgs(args, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), `undeclared tool name "get_weather"`)
	})

	t.Run("重复键失败关闭", func(t *testing.T) {
		one := cursorToolTestBuildMcpArgs("c", "get_weather", []cursorToolTestMcpArgEntry{
			{key: "city", kind: "string", val: "Beijing"}})
		two := cursorToolTestBuildMcpArgs("c", "get_weather", []cursorToolTestMcpArgEntry{
			{key: "city", kind: "string", val: "Shanghai"}})
		// 取 one 的 map entry（f2）追加到 two，保留两条同名 key。
		entryOne, ok := cursorFindField(cursorMustDecodeFields(t, one), 2)
		require.True(t, ok)
		dup := cursorAppendBytesField(two, 2, entryOne.Bytes)
		_, _, _, err := cursorDecodeMcpArgs(dup, defs)
		require.Error(t, err)
		require.Contains(t, err.Error(), `duplicate mcp arg "city"`)
	})
}

// cursorMustDecodeFields 测试内解码 protobuf，失败即终止（用于手工拼装畸形/重复字段帧）。
func cursorMustDecodeFields(t *testing.T, b []byte) []cursorPBField {
	t.Helper()
	fields, err := cursorDecodeProtobuf(b)
	require.NoError(t, err)
	return fields
}

func TestCursorDuplexMcpArgsCapturedNotAnswered(t *testing.T) {
	initReq := &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools: []apicompat.ChatTool{{Type: "function", Function: &apicompat.ChatFunction{
			Name:       "get_weather",
			Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}}},
	}
	initBody, err := cursorConvertRequest(initReq, "grok-4.7")
	require.NoError(t, err)
	d, err := newCursorAgentDuplex(initBody, nil)
	require.NoError(t, err)
	defer d.close(nil)
	// 请求侧 f4 已是逐工具原生定义（f5=get_weather），declaredToolDefs 直接取自真实 f4，不再注入夹具。

	args := cursorToolTestBuildMcpArgs("call_99", "get_weather", []cursorToolTestMcpArgEntry{
		{key: "city", kind: "string", val: "Shanghai"}})
	frame := cursorToolTestExecFrame(true, args) // f11 mcp_args（respond 吃 envelope 剥离后的 payload）

	done, err := d.respond(frame)
	require.NoError(t, err)
	require.False(t, done, "mcp_args 委托不得终结双向连接")
	// 唯一解码点：duplex 侧恰好记一条委托，参数逐字保留。
	snapshot := d.delegations.snapshot()
	require.Len(t, snapshot, 1, "duplex 侧委托记录必须恰好一条（唯一解码点）")
	require.Equal(t, "call_99", snapshot[0].ID)
	require.Equal(t, "get_weather", snapshot[0].Name)
	require.Equal(t, `{"city":"Shanghai"}`, snapshot[0].ArgsJSON)
	require.Equal(t, uint64(7), snapshot[0].ExecID, "exec_id 原样带回")
	require.Equal(t, []byte("exec-id"), snapshot[0].IDField, "id 原值原样带回")

	// 零回包（§24.3 旧链已斩）：委托阶段**绝不**回 McpResult（尤其不得回
	// McpResult.error 诱导模型结束）。真实结果只能在续轮经由 CursorSessionIntake
	// 写回原 exec 关联，故此刻上行通道必须没有任何字节。
	replyCh := cursorDuplexReadReplyAsync(d)
	select {
	case rep := <-replyCh:
		t.Fatalf("mcp_args 委托阶段不得回任何 McpResult（旧链已斩），却收到 %d 字节: %q", len(rep.b), string(rep.b))
	case <-time.After(200 * time.Millisecond):
	}
}

// ---------------------------------------------------------------------------
// 5. finish_reason：有捕获 → tool_calls；无捕获 → 零变化
// ---------------------------------------------------------------------------

func TestCursorFinishReasonWithToolCall(t *testing.T) {
	args := cursorToolTestBuildMcpArgs("call_1", "get_weather", []cursorToolTestMcpArgEntry{
		{key: "city", kind: "string", val: "Rome"}})
	body := cursorToolTestStreamBody(
		cursorToolTestExecFrame(true, args),
		cursorToolTestTurnEndedFrame(10, 5),
	)

	// 独立解析路径（非 duplex）用请求侧真实 f4 声明；两解析路径共用同一解码函数。
	defs := cursorTestDeclaredToolDefs(t, &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools: []apicompat.ChatTool{{Type: "function", Function: &apicompat.ChatFunction{
			Name:       "get_weather",
			Parameters: json.RawMessage(`{"type":"object"}`),
		}}},
	})

	// 非流式：tool_calls 字段 + finish_reason=tool_calls。
	p := cursorNewStreamParser("grok-4.7")
	p.attachToolSchemas(defs)
	_, err := p.cursorParseStream(body)
	require.NoError(t, err)
	completion := p.cursorBuildNonStreamingCompletion()
	require.Contains(t, string(completion), `"finish_reason":"tool_calls"`)
	require.Contains(t, string(completion), `"tool_calls":`)
	require.Contains(t, string(completion), `"name":"get_weather"`)
	require.Contains(t, string(completion), `"arguments":`)
	require.Contains(t, string(completion), `"id":"call_1"`)
	// usage 不丢。
	require.Contains(t, string(completion), `"output_tokens":5`)

	// 流式：SSE 含 tool_calls delta 与 finish_reason=tool_calls。
	p2 := cursorNewStreamParser("grok-4.7")
	p2.attachToolSchemas(defs)
	lines, err := p2.cursorParseStream(body)
	require.NoError(t, err)
	var sse []byte
	for _, l := range lines {
		sse = append(sse, l...)
	}
	sse = append(sse, cursorSSEDoneSentinel...)
	require.Contains(t, string(sse), `"finish_reason":"tool_calls"`)
	require.Contains(t, string(sse), `"tool_calls":`)
	require.Contains(t, string(sse), `"name":"get_weather"`)
	require.Contains(t, string(sse), `"arguments":`)
	require.Contains(t, string(sse), "data: [DONE]")
}

func TestCursorFinishReasonNoToolCall(t *testing.T) {
	// 纯文本链（无 exec 帧）：finish_reason=stop，且无 tool_calls。
	body := cursorToolTestStreamBody(
		// interaction_update(f1) → text_delta(f1) → text(f1) = "hi"
		cursorAppendBytesField(nil, 1, cursorAppendBytesField(nil, 1, cursorAppendStringField(nil, 1, "hi"))),
		cursorToolTestTurnEndedFrame(3, 2),
	)
	completion, err := cursorParseStreamToCompletion(body, "grok-4.7")
	require.NoError(t, err)
	require.Contains(t, string(completion), `"finish_reason":"stop"`)
	require.NotContains(t, string(completion), `"tool_calls"`)
	require.Contains(t, string(completion), `"content":"hi"`)
}

// ---------------------------------------------------------------------------
// 6. 历史按真实结构：assistant.tool_calls + tool 消息 → 原生 tool-call/tool-result
// ---------------------------------------------------------------------------

func TestCursorContinuationToolCallAndResult(t *testing.T) {
	t.Run("文本+两个调用 → content 顺序 text,tool-call,tool-call", func(t *testing.T) {
		req := &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{
				{Role: "user", Content: json.RawMessage(`"weather?"`)},
				{Role: "assistant", Content: json.RawMessage(`"let me check"`), ToolCalls: []apicompat.ChatToolCall{
					{ID: "call_a", Type: "function", Function: apicompat.ChatFunctionCall{
						Name: "a", Arguments: `{"k":1}`}},
					{ID: "call_b", Type: "function", Function: apicompat.ChatFunctionCall{
						Name: "b", Arguments: `{}`}},
				}},
				{Role: "user", Content: json.RawMessage(`"continue"`)},
			},
		}
		body, err := cursorConvertRequest(req, "grok-4.7")
		require.NoError(t, err)
		run := cursorAgentRunOf(t, body)
		require.Equal(t, "continue", cursorActionTextOf(t, run))
		blobs := cursorBlobsOf(t, run)
		require.Len(t, blobs, 2, "first user + assistant history")
		require.JSONEq(t,
			`{"role":"assistant","content":[
				{"type":"text","text":"let me check"},
				{"type":"tool-call","toolCallId":"call_a","toolName":"CallDynamicTool","args":{"arguments":{"k":1},"namespace":"sub2api","toolName":"a"}},
				{"type":"tool-call","toolCallId":"call_b","toolName":"CallDynamicTool","args":{"arguments":{},"namespace":"sub2api","toolName":"b"}}
			],"id":"1"}`,
			string(blobs[1][1]), "history must use the real tool-call structure")
	})

	t.Run("仅工具调用的 assistant 不被裁剪", func(t *testing.T) {
		req := &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{
				{Role: "user", Content: json.RawMessage(`"weather?"`)},
				{Role: "assistant", ToolCalls: []apicompat.ChatToolCall{
					{ID: "call_1", Type: "function", Function: apicompat.ChatFunctionCall{
						Name: "get_weather", Arguments: `{"city":"Beijing"}`}}}},
				{Role: "user", Content: json.RawMessage(`"continue"`)},
			},
		}
		body, err := cursorConvertRequest(req, "grok-4.7")
		require.NoError(t, err)
		blobs := cursorBlobsOf(t, cursorAgentRunOf(t, body))
		require.Len(t, blobs, 2, "tool-calls-only assistant must not be trimmed as an empty tail")
		require.JSONEq(t,
			`{"role":"assistant","content":[{"type":"tool-call","toolCallId":"call_1","toolName":"CallDynamicTool","args":{"arguments":{"city":"Beijing"},"namespace":"sub2api","toolName":"get_weather"}}],"id":"1"}`,
			string(blobs[1][1]))
	})

	t.Run("带空白的 null 参数也归一成空对象", func(t *testing.T) {
		req := &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{
				{Role: "user", Content: json.RawMessage(`"weather?"`)},
				{Role: "assistant", ToolCalls: []apicompat.ChatToolCall{
					{ID: "call_null", Type: "function", Function: apicompat.ChatFunctionCall{
						Name: "get_weather", Arguments: " \t null \n"}}}},
				{Role: "user", Content: json.RawMessage(`"continue"`)},
			},
		}
		body, err := cursorConvertRequest(req, "grok-4.7")
		require.NoError(t, err)
		blobs := cursorBlobsOf(t, cursorAgentRunOf(t, body))
		require.Len(t, blobs, 2)
		require.JSONEq(t,
			`{"role":"assistant","content":[{"type":"tool-call","toolCallId":"call_null","toolName":"CallDynamicTool","args":{"arguments":{},"namespace":"sub2api","toolName":"get_weather"}}],"id":"1"}`,
			string(blobs[1][1]))
	})

	t.Run("tool 消息 → role=tool 的历史 blob（逐字段）", func(t *testing.T) {
		req := &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{
				{Role: "user", Content: json.RawMessage(`"q"`)},
				{Role: "assistant", Content: json.RawMessage(`"done"`)},
				{Role: "tool", ToolCallID: "call_1", Name: "get_weather", Content: json.RawMessage(`"sunny"`)},
				{Role: "user", Content: json.RawMessage(`"next"`)},
			},
		}
		body, err := cursorConvertRequest(req, "grok-4.7")
		require.NoError(t, err)
		run := cursorAgentRunOf(t, body)
		require.Equal(t, "next", cursorActionTextOf(t, run))
		blobs := cursorBlobsOf(t, run)
		require.Len(t, blobs, 3)
		require.JSONEq(t,
			`{"role":"tool","content":[{"type":"tool-result","toolCallId":"call_1","toolName":"CallDynamicTool","result":"sunny","experimental_content":[{"type":"text","text":"sunny"}]}],"id":"call_1","providerOptions":{"cursor":{"highLevelToolCallResult":{"output":{"success":{"content":[{"text":{"text":"sunny"}}]}},"isError":false}}}}`,
			string(blobs[2][1]), "tool result must use the real structure with id and providerOptions")
	})

	t.Run("孤立工具结果不能新建 AgentRun（最后一轮非 user 仍拒绝）", func(t *testing.T) {
		_, err := cursorConvertRequest(&apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{
				{Role: "user", Content: json.RawMessage(`"q"`)},
				{Role: "tool", ToolCallID: "call_1", Name: "get_weather", Content: json.RawMessage(`"sunny"`)},
			},
		}, "grok-4.7")
		require.Error(t, err)
		require.Contains(t, err.Error(), "conversation must end with a user message")
	})
}

// ---------------------------------------------------------------------------
// 8. 计费语义：捕获委托回 error 后流读至 turn_ended，usage 不丢
// ---------------------------------------------------------------------------

func TestCursorBillingAfterToolDeferral(t *testing.T) {
	args := cursorToolTestBuildMcpArgs("call_1", "get_weather", []cursorToolTestMcpArgEntry{
		{key: "city", kind: "string", val: "X"}})
	body := cursorToolTestStreamBody(
		cursorToolTestExecFrame(true, args),
		cursorToolTestTurnEndedFrame(123, 4),
	)
	// 独立解析路径用请求侧真实 f4 声明，工具名/参数经同一解码函数读取。
	defs := cursorTestDeclaredToolDefs(t, &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools: []apicompat.ChatTool{{Type: "function", Function: &apicompat.ChatFunction{
			Name:       "get_weather",
			Parameters: json.RawMessage(`{"type":"object"}`),
		}}},
	})

	p := cursorNewStreamParser("grok-4.7")
	p.attachToolSchemas(defs)
	_, err := p.cursorParseStream(body)
	require.NoError(t, err)
	require.True(t, p.usage.Seen)
	require.Equal(t, 123, p.usage.Input)
	require.Equal(t, 4, p.usage.Output)
	require.Len(t, p.capturedToolCalls(), 1, "parser 侧读取同一份委托记录，恰好一条")
}

// ---------------------------------------------------------------------------
// 9. 历史工具结果为错误文本时仍按成功形状表达（入站无 is_error，不猜错误）
// ---------------------------------------------------------------------------

func TestCursorContinuationToolError(t *testing.T) {
	const errText = "Error: upstream timeout"
	req := &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"q"`)},
			{Role: "tool", ToolCallID: "call_1", Name: "get_weather", Content: json.RawMessage(`"` + errText + `"`)},
			{Role: "assistant", Content: json.RawMessage(`"I will retry."`)},
			{Role: "user", Content: json.RawMessage(`"ok"`)},
		},
	}
	body, err := cursorConvertRequest(req, "grok-4.7")
	require.NoError(t, err)
	run := cursorAgentRunOf(t, body)
	require.Equal(t, "ok", cursorActionTextOf(t, run))
	tails := cursorBlobsOf(t, run)
	require.Len(t, tails, 3)
	require.JSONEq(t,
		`{"role":"tool","content":[{"type":"tool-result","toolCallId":"call_1","toolName":"CallDynamicTool","result":"Error: upstream timeout","experimental_content":[{"type":"text","text":"Error: upstream timeout"}]}],"id":"call_1","providerOptions":{"cursor":{"highLevelToolCallResult":{"output":{"success":{"content":[{"text":{"text":"Error: upstream timeout"}}]}},"isError":false}}}}`,
		string(tails[1][1]), "inbound has no is_error: keep the success shape even for error text")
}
