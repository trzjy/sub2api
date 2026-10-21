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

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

// cursorTestFieldString 便捷取某字段号的字符串值（字段缺失返回空串）。
func cursorTestFieldString(fields []cursorPBField, n int) string {
	f, ok := cursorFindField(fields, n)
	if !ok {
		return ""
	}
	return f.String()
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
	kind string // "string" | "number" | "null"
	val  string
}

// cursorToolTestBuildMcpArgs 构造 McpArgs 字节（exec 内层 f11 的载荷）。
//
// 字段号（派发单 §2）：f3 tool_call_id、f5 tool_name、f2 args（map<string,Value>，
// 每个 entry = {f1 key, f2 value(Value)}；Value：f1 null / f2 number / f3 string）。
func cursorToolTestBuildMcpArgs(tcID, toolName string, entries []cursorToolTestMcpArgEntry) []byte {
	var args []byte
	for _, e := range entries {
		var val []byte
		switch e.kind {
		case "string":
			val = cursorAppendStringField(nil, 3, e.val)
		case "number":
			v, err := strconv.ParseInt(e.val, 10, 64)
			if err != nil {
				panic("bad number value: " + e.val)
			}
			val = cursorAppendVarintField(nil, 2, uint64(v))
		case "null":
			val = nil
		}
		entry := cursorAppendStringField(nil, 1, e.key)
		entry = cursorAppendBytesField(entry, 2, val)
		args = cursorAppendBytesField(args, 2, entry)
	}
	mcpArgs := cursorAppendStringField(nil, 3, tcID)
	mcpArgs = cursorAppendStringField(mcpArgs, 5, toolName)
	mcpArgs = cursorAppendBytesField(mcpArgs, 2, args)
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

// ---------------------------------------------------------------------------
// 1. 声明编码：tools[2] → f4 存在、McpToolDefinition 五字段齐、repeated 逐项
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

	// repeated 逐项：必须解出**两条** McpToolDefinition（f1 每条工具）。
	mcpFields, e := cursorDecodeProtobuf(mcp.Bytes)
	require.NoError(t, e)
	defs := cursorAllFields(mcpFields, cursorFldMcpToolsTool)
	require.Len(t, defs, 2, "two tools must be encoded as two repeated f1 entries")

	for i, def := range defs {
		fields, e := cursorDecodeProtobuf(def.Bytes)
		require.NoError(t, e)
		name := cursorTestFieldString(fields, cursorFldMcpToolDefName)
		require.Equal(t, []string{"get_weather", "search"}[i], name)
		// 五字段齐全断言。
		require.Equal(t, name, cursorTestFieldString(fields, cursorFldMcpToolDefToolName), "f5 tool_name must equal f1 name")
		require.Equal(t, cursorMcpProviderIdentifier, cursorTestFieldString(fields, cursorFldMcpToolDefProvider))
		require.NotEmpty(t, cursorTestFieldString(fields, cursorFldMcpToolDefDescription))
		require.JSONEq(t, []string{`{"type":"object"}`, `{"type":"object","x":1}`}[i],
			cursorTestFieldString(fields, cursorFldMcpToolDefInputSchema))
	}
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
	countDeclared := func(t *testing.T, req *apicompat.ChatCompletionsRequest) int {
		body, err := cursorConvertRequest(req, "grok-4.7")
		require.NoError(t, err)
		run := cursorAgentRunOf(t, body)
		mcp, ok := cursorFindField(run, cursorFldRunMcpTools)
		require.True(t, ok)
		if len(mcp.Bytes) == 0 {
			return 0
		}
		mcpFields, e := cursorDecodeProtobuf(mcp.Bytes)
		require.NoError(t, e)
		return len(cursorAllFields(mcpFields, cursorFldMcpToolsTool))
	}

	t.Run("省略/auto → 全部声明", func(t *testing.T) {
		require.Equal(t, 2, countDeclared(t, base(nil)))
		require.Equal(t, 2, countDeclared(t, base(json.RawMessage(`"auto"`))))
	})
	t.Run("required → 全部声明", func(t *testing.T) {
		require.Equal(t, 2, countDeclared(t, base(json.RawMessage(`"required"`))))
	})
	t.Run("none → 不声明（等价无 tools）", func(t *testing.T) {
		require.Equal(t, 0, countDeclared(t, base(json.RawMessage(`"none"`))))
	})
	t.Run("指定函数 → 仅声明该函数", func(t *testing.T) {
		req := base(json.RawMessage(`{"type":"function","function":{"name":"b"}}`))
		body, err := cursorConvertRequest(req, "grok-4.7")
		require.NoError(t, err)
		run := cursorAgentRunOf(t, body)
		mcp, ok := cursorFindField(run, cursorFldRunMcpTools)
		require.True(t, ok)
		require.NotEmpty(t, mcp.Bytes)
		mcpFields, e := cursorDecodeProtobuf(mcp.Bytes)
		require.NoError(t, e)
		defs := cursorAllFields(mcpFields, cursorFldMcpToolsTool)
		require.Len(t, defs, 1)
		fields, _ := cursorDecodeProtobuf(defs[0].Bytes)
		require.Equal(t, "b", cursorTestFieldString(fields, cursorFldMcpToolDefName))
	})
	t.Run("指定函数不在列表 → 回落全部（不凭空声明）", func(t *testing.T) {
		req := base(json.RawMessage(`{"type":"function","function":{"name":"missing"}}`))
		require.Equal(t, 2, countDeclared(t, req))
	})
}

// ---------------------------------------------------------------------------
// 3. mcp_state 回包：id/exec_id 配对 + McpStateServer f1/f2/f5（repeated 逐项）
// ---------------------------------------------------------------------------

func TestCursorDuplexMcpStateReply(t *testing.T) {
	// 构造带 2 个工具声明的 AgentRun，让 duplex 解析出 declaredToolNames。
	initReq := &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools: []apicompat.ChatTool{
			{Type: "function", Function: &apicompat.ChatFunction{Name: "get_weather"}},
			{Type: "function", Function: &apicompat.ChatFunction{Name: "search"}},
		},
	}
	initBody, err := cursorConvertRequest(initReq, "grok-4.7")
	require.NoError(t, err)

	d, err := newCursorAgentDuplex(initBody)
	require.NoError(t, err)
	defer d.close(nil)
	require.Len(t, d.declaredToolNames, 2)

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

	// f5 tools：每个声明工具**单独**一条 f5（repeated 逐项，非拼接）。
	toolEntries := cursorAllFields(serverFields, 5)
	require.Len(t, toolEntries, 2, "f5 must have one entry per declared tool")
	names := map[string]bool{}
	for _, te := range toolEntries {
		tf, e := cursorDecodeProtobuf(te.Bytes)
		require.NoError(t, e)
		names[cursorTestFieldString(tf, 1)] = true
	}
	require.True(t, names["get_weather"])
	require.True(t, names["search"])
}

// ---------------------------------------------------------------------------
// 4. mcp_args 捕获：map args → JSON（string/number/null）；tool_call_id/tool_name 正确
// ---------------------------------------------------------------------------

func TestCursorDecodeMcpArgsMap(t *testing.T) {
	t.Run("string/number/null 用例", func(t *testing.T) {
		args := cursorToolTestBuildMcpArgs("call_42", "get_weather", []cursorToolTestMcpArgEntry{
			{key: "city", kind: "string", val: "Beijing"},
			{key: "days", kind: "number", val: "3"},
			{key: "verbose", kind: "null"},
		})
		id, name, argsJSON, err := cursorDecodeMcpArgs(args)
		require.NoError(t, err)
		require.Equal(t, "call_42", id)
		require.Equal(t, "get_weather", name)
		var obj map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(argsJSON), &obj))
		require.Len(t, obj, 3)
		require.Equal(t, `"Beijing"`, string(obj["city"]))
		require.Equal(t, `3`, string(obj["days"]))
		require.Equal(t, `null`, string(obj["verbose"]))
	})

	t.Run("空 args 返回空对象", func(t *testing.T) {
		args := cursorToolTestBuildMcpArgs("c", "t", nil)
		_, _, argsJSON, err := cursorDecodeMcpArgs(args)
		require.NoError(t, err)
		require.Equal(t, "{}", argsJSON)
	})
}

func TestCursorDuplexMcpArgsReply(t *testing.T) {
	d, err := newCursorAgentDuplex(cursorEncodeEnvelope(cursorAppendBytesField(nil, cursorFldAgentRunRequest, nil)))
	require.NoError(t, err)
	defer d.close(nil)

	args := cursorToolTestBuildMcpArgs("call_99", "get_weather", []cursorToolTestMcpArgEntry{
		{key: "city", kind: "string", val: "Shanghai"}})
	frame := cursorToolTestExecFrame(true, args) // f11 mcp_args（respond 吃 envelope 剥离后的 payload）
	// 先起并发读取，再调 respond：d.send 走 io.Pipe 会阻塞直到被读，避免死锁。
	replyCh := cursorDuplexReadReplyAsync(d)
	done, err := d.respond(frame)
	require.NoError(t, err)
	require.False(t, done, "mcp_args reply must not terminate the connection")
	require.True(t, d.toolCallsFinished)

	rep := <-replyCh
	require.NoError(t, rep.err)
	reply := rep.b
	outer, err := cursorDecodeProtobuf(reply)
	require.NoError(t, err)
	exec, ok := cursorFindField(outer, 2)
	require.True(t, ok)
	execFields, err := cursorDecodeProtobuf(exec.Bytes)
	require.NoError(t, err)
	require.Equal(t, uint64(7), cursorVarintField(execFields, 1))
	require.Equal(t, "exec-id", cursorTestFieldString(execFields, 15))
	// f11 = McpResult → f2 error → f1 error 文案。
	result, ok := cursorFindField(execFields, 11)
	require.True(t, ok, "reply must carry f11 McpResult")
	resultFields, err := cursorDecodeProtobuf(result.Bytes)
	require.NoError(t, err)
	mcpErr, ok := cursorFindField(resultFields, 2)
	require.True(t, ok, "McpResult must carry f2 error")
	errFields, err := cursorDecodeProtobuf(mcpErr.Bytes)
	require.NoError(t, err)
	require.Contains(t, cursorTestFieldString(errFields, 1), "Tool execution deferred")
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

	// 非流式：tool_calls 字段 + finish_reason=tool_calls。
	completion, err := cursorParseStreamToCompletion(body, "grok-4.7")
	require.NoError(t, err)
	require.Contains(t, string(completion), `"finish_reason":"tool_calls"`)
	require.Contains(t, string(completion), `"tool_calls":`)
	require.Contains(t, string(completion), `"name":"get_weather"`)
	require.Contains(t, string(completion), `"arguments":`)
	require.Contains(t, string(completion), `"id":"call_1"`)
	// usage 不丢。
	require.Contains(t, string(completion), `"output_tokens":5`)

	// 流式：SSE 含 tool_calls delta 与 finish_reason=tool_calls。
	sse, err := cursorParseStreamToSSE(body, "grok-4.7")
	require.NoError(t, err)
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
	p := cursorNewStreamParser("grok-4.7")
	_, err := p.cursorParseStream(body)
	require.NoError(t, err)
	require.True(t, p.usage.Seen)
	require.Equal(t, 123, p.usage.Input)
	require.Equal(t, 4, p.usage.Output)
	require.True(t, p.toolCallsFinished)
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
