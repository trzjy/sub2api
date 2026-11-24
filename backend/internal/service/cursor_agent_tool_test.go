//go:build unit

package service

// O11-B Cursor Agent 工具闭环测试：请求声明 + duplex 委托 + 响应转换 + 续轮文本化。
//
// 断言以字节级回读为主（不只断言"不报错"），确保字段号与嵌套层级真的写对了。

import (
	"bytes"
	"encoding/json"
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

// cursorTestRestoreF3Schema 把解码后的 McpToolDefinition 字段里 f3（google.protobuf.Value）
// 还原为规范 JSON 字符串，并断言 f3 存在（单入口运输定义只写 f3）。
func cursorTestRestoreF3Schema(t *testing.T, fields []cursorPBField) string {
	t.Helper()
	f3, ok := cursorFindField(fields, cursorFldMcpToolDefInputSchemaValue)
	require.True(t, ok, "f3 input_schema Value must be present")
	v := &structpb.Value{}
	require.NoError(t, proto.Unmarshal(f3.Bytes, v))
	out, err := protojson.Marshal(v)
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, json.Compact(&buf, out))
	return buf.String()
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

// cursorToolTestMcpArgEntry 是一个 mcp_args 的参数条目（key + 值）。
type cursorToolTestMcpArgEntry struct {
	key  string
	kind string // "string" | "number" | "null" | "bool"
	val  string
}

// cursorToolTestBuildMcpArgs 构造运输 f11（mcp_args）载荷，签名保持不变。
//
// 上游只回调单入口 invoke_client_tool：f3 tool_call_id、f5 固定 cursorTransportToolName，
// f2 两条 entry（每条 = {f1 key string, f2 value structpb.Value string}）：
//   - tool_name      = 工具名（真实原名，由 f4 的 enum 校验）
//   - arguments_json = 由 entries 按原顺序拼出的 JSON 对象字符串
//
// arguments_json 中 string 值用 json.Marshal 正确转义，number/bool/null 用原字面量，
// 不做 map 往返 / 数值归一化，保证与 cursorDecodeMcpArgs 的原字节口径一致。
func cursorToolTestBuildMcpArgs(tcID, toolName string, entries []cursorToolTestMcpArgEntry) []byte {
	var obj strings.Builder
	obj.WriteByte('{')
	for i, e := range entries {
		if i > 0 {
			obj.WriteByte(',')
		}
		key, err := json.Marshal(e.key)
		if err != nil {
			panic("bad arg key: " + err.Error())
		}
		obj.Write(key)
		obj.WriteByte(':')
		switch e.kind {
		case "string":
			v, err := json.Marshal(e.val)
			if err != nil {
				panic("bad string value: " + err.Error())
			}
			obj.Write(v)
		case "number", "bool":
			obj.WriteString(e.val)
		case "null":
			obj.WriteString("null")
		default:
			panic("unknown arg kind: " + e.kind)
		}
	}
	obj.WriteByte('}')

	// 每条 entry 先组成 {f1 key, f2 Value.string(v)}，再作为 McpArgs 的 f2 字段。
	var mcpArgs []byte
	toolNameEntry := cursorAppendStringField(nil, 1, cursorTransportToolNameKey)
	toolNameEntry = cursorAppendBytesField(toolNameEntry, 2, cursorAppendStringField(nil, 3, toolName))
	mcpArgs = cursorAppendBytesField(mcpArgs, 2, toolNameEntry)

	argsEntry := cursorAppendStringField(nil, 1, cursorTransportArgumentsKey)
	argsEntry = cursorAppendBytesField(argsEntry, 2, cursorAppendStringField(nil, 3, obj.String()))
	mcpArgs = cursorAppendBytesField(mcpArgs, 2, argsEntry)

	mcpArgs = cursorAppendStringField(mcpArgs, 3, tcID)
	mcpArgs = cursorAppendStringField(mcpArgs, 5, cursorTransportToolName)
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
	require.NotEmpty(t, mcp.Bytes)
	mcpFields, e := cursorDecodeProtobuf(mcp.Bytes)
	require.NoError(t, e)
	defs := [][]byte{}
	for _, def := range cursorAllFields(mcpFields, cursorFldMcpToolsTool) {
		defs = append(defs, def.Bytes)
	}
	return defs
}

// cursorTestTransportDefs 用生产构造 cursorBuildMcpTools 生成真实 f4 单入口运输定义，
// 返回其每条 McpToolDefinition 原始字节。f4 只有一条（invoke_client_tool），f5
// tool_name enum 即 names；mcp_state 回包必须逐字节回显这些定义。
func cursorTestTransportDefs(t *testing.T, names ...string) [][]byte {
	t.Helper()
	tools := make([]apicompat.ChatTool, 0, len(names))
	for _, name := range names {
		tools = append(tools, apicompat.ChatTool{
			Type: "function",
			Function: &apicompat.ChatFunction{
				Name:       name,
				Parameters: json.RawMessage(`{"type":"object"}`),
			},
		})
	}
	f4, err := cursorBuildMcpTools(tools, cursorSkipLedger{})
	require.NoError(t, err)
	fields, err := cursorDecodeProtobuf(f4)
	require.NoError(t, err)
	defs := make([][]byte, 0)
	for _, f := range cursorAllFields(fields, cursorFldMcpToolsTool) {
		defs = append(defs, f.Bytes)
	}
	return defs
}

// cursorTestTransportEnum 从单入口 f4 定义的 f3 input_schema 解出 properties.tool_name.enum。
func cursorTestTransportEnum(t *testing.T, fields []cursorPBField) []string {
	t.Helper()
	var schema struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	require.NoError(t, json.Unmarshal([]byte(cursorTestRestoreF3Schema(t, fields)), &schema))
	return schema.Properties[cursorTransportToolNameKey].Enum
}

// cursorTestBuildToolDef 构造一条 McpToolDefinition 原始字节（f1 名 / f2 description /
// f4 provider / f5 tool_name / f3 input_schema(google.protobuf.Value)），与
// cursorBuildMcpTools 生产构造同形（仅用于测试夹具，不重新序列化 req.Tools）。
func cursorTestBuildToolDef(t *testing.T, toolName, schema string) []byte {
	t.Helper()
	v := &structpb.Value{}
	require.NoError(t, protojson.Unmarshal([]byte(schema), v))
	b, err := proto.Marshal(v)
	require.NoError(t, err)
	var def []byte
	def = cursorAppendStringField(def, cursorFldMcpToolDefName, toolName)
	def = cursorAppendStringField(def, cursorFldMcpToolDefDescription, "desc-"+toolName)
	def = cursorAppendStringField(def, cursorFldMcpToolDefProvider, cursorMcpProviderIdentifier)
	def = cursorAppendStringField(def, cursorFldMcpToolDefToolName, toolName)
	def = cursorAppendBytesField(def, cursorFldMcpToolDefInputSchemaValue, b)
	return def
}

// ---------------------------------------------------------------------------
// 1. 声明编码：单入口运输 f4 只有一条定义，enum 列出全部客户端工具
// ---------------------------------------------------------------------------

func TestCursorToolDeclarationEncoding(t *testing.T) {
	req := &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools: []apicompat.ChatTool{
			{Type: "function", Function: &apicompat.ChatFunction{
				Name: "get_weather", Description: "d1",
				Parameters: json.RawMessage(`{"type":"object"}`)}},
			{Type: "function", Function: &apicompat.ChatFunction{
				Name: "search", Description: "d2",
				Parameters: json.RawMessage(`{"type":"object","x":1}`)}},
		},
	}
	body, err := cursorConvertRequest(req, "grok-4.7")
	require.NoError(t, err)

	run := cursorAgentRunOf(t, body)
	mcp, ok := cursorFindField(run, cursorFldRunMcpTools)
	require.True(t, ok, "f4 (McpTools) must be present")
	require.NotEmpty(t, mcp.Bytes)

	// 单入口运输：无论客户端声明多少工具，f4 只有一条 McpToolDefinition
	// （invoke_client_tool）；真实工具名进 f3 input_schema 的 tool_name enum。
	mcpFields, e := cursorDecodeProtobuf(mcp.Bytes)
	require.NoError(t, e)
	defs := cursorAllFields(mcpFields, cursorFldMcpToolsTool)
	require.Len(t, defs, 1, "all client tools share one transport definition")

	fields, e := cursorDecodeProtobuf(defs[0].Bytes)
	require.NoError(t, e)
	require.Equal(t, cursorTransportToolName, cursorTestFieldString(fields, cursorFldMcpToolDefName))
	require.Equal(t, cursorTransportToolName, cursorTestFieldString(fields, cursorFldMcpToolDefToolName))
	require.Equal(t, cursorMcpProviderIdentifier, cursorTestFieldString(fields, cursorFldMcpToolDefProvider))
	desc := cursorTestFieldString(fields, cursorFldMcpToolDefDescription)
	require.Contains(t, desc, "get_weather")
	require.Contains(t, desc, `"x":1`)
	// enum 准确：恰好两个原名、顺序与声明一致。
	require.Equal(t, []string{"get_weather", "search"}, cursorTestTransportEnum(t, fields))
}

// ---------------------------------------------------------------------------
// 2/7. tool_choice 四值语义
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
	// 单入口运输下，tool_choice 决定 f3 tool_name enum 的内容（f4 恒为一条）。
	transportEnum := func(t *testing.T, req *apicompat.ChatCompletionsRequest) []string {
		defs := cursorTestDeclaredToolDefs(t, req)
		if len(defs) == 0 {
			return nil
		}
		require.Len(t, defs, 1, "单入口运输：f4 只有一条定义")
		fields, err := cursorDecodeProtobuf(defs[0])
		require.NoError(t, err)
		return cursorTestTransportEnum(t, fields)
	}

	t.Run("省略/auto → 全部声明", func(t *testing.T) {
		require.Equal(t, []string{"a", "b"}, transportEnum(t, base(nil)))
		require.Equal(t, []string{"a", "b"}, transportEnum(t, base(json.RawMessage(`"auto"`))))
	})
	t.Run("required → 全部声明", func(t *testing.T) {
		require.Equal(t, []string{"a", "b"}, transportEnum(t, base(json.RawMessage(`"required"`))))
	})
	t.Run("none → 不声明（等价无 tools）", func(t *testing.T) {
		require.Empty(t, transportEnum(t, base(json.RawMessage(`"none"`))))
	})
	t.Run("指定函数 → 仅声明该函数", func(t *testing.T) {
		require.Equal(t, []string{"b"}, transportEnum(t, base(json.RawMessage(`{"type":"function","function":{"name":"b"}}`))))
	})
	t.Run("指定函数不在列表 → 回落全部（不凭空声明）", func(t *testing.T) {
		require.Equal(t, []string{"a", "b"}, transportEnum(t, base(json.RawMessage(`{"type":"function","function":{"name":"missing"}}`))))
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
	require.Len(t, d.declaredToolDefs, 1, "单入口运输：f4 只有一条定义")

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
	require.Len(t, defRaw, 1, "单入口运输：f4 只有一条 McpToolDefinition 原始字节")

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
	require.Equal(t, cursorTransportToolName, cursorTestFieldString(tf, cursorFldMcpToolDefName))
}

// ---------------------------------------------------------------------------
// 4. mcp_args 捕获：map args → JSON（string/number/null）；tool_call_id/tool_name 正确
// ---------------------------------------------------------------------------

func TestCursorDecodeMcpArgsMap(t *testing.T) {
	defs := cursorTestTransportDefs(t, "get_weather")
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

	t.Run("空 args 返回空对象", func(t *testing.T) {
		args := cursorToolTestBuildMcpArgs("c", "get_weather", nil)
		_, _, argsJSON, err := cursorDecodeMcpArgs(args, defs)
		require.NoError(t, err)
		require.Equal(t, "{}", argsJSON)
	})
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

	// 独立解析路径（非 duplex）必须注入本请求 f4 原始字节，否则按 tool_name 取回
	// input_schema 失败 → schema_absent 失败关闭（§B 两路径一致，不放宽）。来源与
	// duplex 同源：声明同名的 get_weather 工具，取其 cursorConvertRequest 实际发出的 f4。
	defs := cursorTestDeclaredToolDefs(t, &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools: []apicompat.ChatTool{{Type: "function", Function: &apicompat.ChatFunction{
			Name: "get_weather", Parameters: json.RawMessage(`{"type":"object"}`)}}},
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
// 6. 续轮文本化：assistant.tool_calls + tool 消息 → blob 文本含边界
// ---------------------------------------------------------------------------

func TestCursorContinuationToolCallAndResult(t *testing.T) {
	t.Run("assistant.tool_calls → <tool_call> blob", func(t *testing.T) {
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
		run := cursorAgentRunOf(t, body)
		require.Equal(t, "continue", cursorActionTextOf(t, run))
		// assistant 历史 blob 必须含 <tool_call> 边界与 arguments（解码后断言）。
		found := false
		for _, b := range cursorBlobsOf(t, run) {
			c := cursorTestBlobContent(t, b)
			if strings.Contains(c, "<tool_call>") &&
				strings.Contains(c, `"id":"call_1"`) &&
				strings.Contains(c, `"name":"get_weather"`) &&
				strings.Contains(c, `"arguments":{"city":"Beijing"}`) {
				found = true
			}
		}
		require.True(t, found, "assistant tool_calls must be text-ified into <tool_call> blob")
	})

	t.Run("tool 消息 → user turn 的 <tool_result>（落为 action 或历史）", func(t *testing.T) {
		req := &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{
				{Role: "user", Content: json.RawMessage(`"q"`)},
				{Role: "assistant", Content: json.RawMessage(`"done"`)},
				{Role: "tool", ToolCallID: "call_1", Name: "get_weather", Content: json.RawMessage(`"sunny"`)},
			},
		}
		body, err := cursorConvertRequest(req, "grok-4.7")
		require.NoError(t, err)
		run := cursorAgentRunOf(t, body)
		// 末轮 tool 结果成为 user turn → 是 action；其文本含 <tool_result> 边界。
		action := cursorActionTextOf(t, run)
		require.Contains(t, action, "<tool_result")
		require.Contains(t, action, `tool_call_id="call_1"`)
		require.Contains(t, action, `name="get_weather"`)
		require.Contains(t, action, "sunny")
		// 历史 blob 含 "q" 与 "done"（不含被丢弃）。
		blobs := cursorBlobsOf(t, run)
		require.Len(t, blobs, 2)
		joined := cursorTestBlobContent(t, blobs[0]) + "|" + cursorTestBlobContent(t, blobs[1])
		require.Contains(t, joined, "q")
		require.Contains(t, joined, "done")
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
	// 独立解析路径（非 duplex）必须注入本请求 f4 原始字节：parser 按 tool_name 取回
	// input_schema 做 retype，缺失即 schema_absent 失败关闭（不放宽）。来源与 duplex
	// 同源：声明同名的 get_weather 工具，取其 cursorConvertRequest 实际发出的 f4。
	defs := cursorTestDeclaredToolDefs(t, &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools: []apicompat.ChatTool{{Type: "function", Function: &apicompat.ChatFunction{
			Name:       "get_weather",
			Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
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
// 9. 续轮文本化含 is_error 场景（工具失败后模型继续）
// ---------------------------------------------------------------------------

func TestCursorContinuationToolError(t *testing.T) {
	// 工具结果内容是一段错误文本，仍应被文本化为 user turn（不失败关闭）。
	req := &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"q"`)},
			{Role: "tool", ToolCallID: "call_1", Name: "get_weather", Content: json.RawMessage(`"Error: upstream timeout"`)},
			{Role: "assistant", Content: json.RawMessage(`"I will retry."`)},
			{Role: "user", Content: json.RawMessage(`"ok"`)},
		},
	}
	body, err := cursorConvertRequest(req, "grok-4.7")
	require.NoError(t, err)
	run := cursorAgentRunOf(t, body)
	require.Equal(t, "ok", cursorActionTextOf(t, run))
	// 含错误文本的工具结果必须落为 user turn，且不得被当作失败关闭。
	tails := cursorBlobsOf(t, run)
	require.Len(t, tails, 3)
	require.Contains(t, string(tails[1][1]), "Error: upstream timeout")
}
