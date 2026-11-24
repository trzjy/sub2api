//go:build unit

package service

// CUR-SVC4：响应侧「无损运输」解码验收。
//
// 上游只回调一个 invoke_client_tool，f2 map 只含 tool_name / arguments_json 两个
// 字符串值。本站保留 arguments_json 原始字节：不转 map、不补 null、不猜类型、
// 不做 schema retype、不接受旧直接参数格式。duplex 与独立 parser 两条消费路径
// 共用同一份解码实现与同一份真实 f4 定义。

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

// cursorSDKTransportArgsFixture 逐字运输参数：大整数 / null / false / 0 / 空串 /
// 嵌套数组 / 中文与 HTML 字符 / 转义引号、反斜杠与换行。任何 map 往返或数值
// 归一化都会改变这些字节（尤其 big 会被 float64 改成 9007199254740992）。
const cursorSDKTransportArgsFixture = `{"big":9007199254740993,"null":null,"false":false,"zero":0,"empty":"","arr":[1,{"x":[true,null]}],"cn":"中文<>&","esc":"a\"b\\c\n"}`

// cursorSDKTransportArgEntry 是一条 f2 map entry：key 为 f1 string，value 为
// 已编码的 structpb.Value 消息字节（f2）。
type cursorSDKTransportArgEntry struct {
	key   string
	value []byte
}

// cursorSDKTransportStringValue 构造 structpb.Value 的 string 分支（f3, wire 2）。
func cursorSDKTransportStringValue(s string) []byte {
	return cursorAppendStringField(nil, 3, s)
}

// cursorSDKTransportBuildMcpArgsRaw 用给定 entry 序列组装 McpArgs 原始字节：
// f3 tool_call_id、f5 外层工具名、每条 f2 即一条 map entry）。
func cursorSDKTransportBuildMcpArgsRaw(callID, outerName string, entries ...cursorSDKTransportArgEntry) []byte {
	var mcpArgs []byte
	for _, e := range entries {
		entry := cursorAppendStringField(nil, 1, e.key)
		entry = cursorAppendBytesField(entry, 2, e.value)
		mcpArgs = cursorAppendBytesField(mcpArgs, 2, entry)
	}
	mcpArgs = cursorAppendStringField(mcpArgs, 3, callID)
	mcpArgs = cursorAppendStringField(mcpArgs, 5, outerName)
	return mcpArgs
}

// cursorSDKTransportStringEntries 构造正常运输帧的两个字符串 entry。
func cursorSDKTransportStringEntries(toolName, argsJSON string) []cursorSDKTransportArgEntry {
	return []cursorSDKTransportArgEntry{
		{key: cursorTransportToolNameKey, value: cursorSDKTransportStringValue(toolName)},
		{key: cursorTransportArgumentsKey, value: cursorSDKTransportStringValue(argsJSON)},
	}
}

// cursorSDKTransportBuildMcpArgs 构造一条合法运输帧。
func cursorSDKTransportBuildMcpArgs(callID, toolName, argsJSON string) []byte {
	return cursorSDKTransportBuildMcpArgsRaw(callID, cursorTransportToolName,
		cursorSDKTransportStringEntries(toolName, argsJSON)...)
}

// cursorSDKTransportExecFrame 构造承载 f11 mcp_args 的 AgentServerMessage 帧：
// f1 exec_id=7（真实值）、f15 id 原值、f11 mcp_args。
func cursorSDKTransportExecFrame(mcpArgs []byte) []byte {
	exec := cursorAppendVarintField(nil, 1, 7)
	exec = cursorAppendBytesField(exec, 15, []byte("exec-id"))
	exec = cursorAppendBytesField(exec, 11, mcpArgs)
	return cursorAppendBytesField(nil, cursorFldServerExecMessage, exec)
}

// cursorSDKTransportRequest 构造声明了给定客户端工具的请求体。
func cursorSDKTransportRequest(toolNames ...string) *apicompat.ChatCompletionsRequest {
	tools := make([]apicompat.ChatTool, 0, len(toolNames))
	for _, name := range toolNames {
		tools = append(tools, apicompat.ChatTool{
			Type: "function",
			Function: &apicompat.ChatFunction{
				Name:       name,
				Parameters: json.RawMessage(`{"type":"object"}`),
			},
		})
	}
	return &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools:    tools,
	}
}

// cursorSDKTransportDeclaredDefs 取真实 f4（经 cursorConvertRequest →
// cursorBuildMcpTools）的每条 McpToolDefinition 原始字节；不复制目录算法。
func cursorSDKTransportDeclaredDefs(t *testing.T, toolNames ...string) [][]byte {
	t.Helper()
	return cursorTestDeclaredToolDefs(t, cursorSDKTransportRequest(toolNames...))
}

// TestCursorSDKTransportDecodePrecision 验证 duplex 与独立 parser 两条消费路径
// 都把 arguments_json 原字节逐字节交付，并补齐 ID / Name / ExecID / IDField。
func TestCursorSDKTransportDecodePrecision(t *testing.T) {
	req := cursorSDKTransportRequest("get_weather")
	initBody, err := cursorConvertRequest(req, "grok-4.7")
	require.NoError(t, err)
	defs := cursorTestDeclaredToolDefs(t, req)
	require.NotEmpty(t, defs, "必须注入真实 f4 工具定义（含 tool_name enum）")

	mcpArgs := cursorSDKTransportBuildMcpArgs("call_sdk_1", "get_weather", cursorSDKTransportArgsFixture)
	frame := cursorSDKTransportExecFrame(mcpArgs)

	// 路径 1：duplex 拥有解码权，分享记录由它写入。
	d, err := newCursorAgentDuplex(initBody, newCursorToolDelegations())
	require.NoError(t, err)
	defer d.close(nil)
	done, err := d.respond(frame)
	require.NoError(t, err)
	require.False(t, done, "mcp_args 委托不得终结双向连接")
	duplexCalls := d.delegations.snapshot()
	require.Len(t, duplexCalls, 1)
	require.Equal(t, "call_sdk_1", duplexCalls[0].ID)
	require.Equal(t, "get_weather", duplexCalls[0].Name)
	require.Equal(t, cursorSDKTransportArgsFixture, duplexCalls[0].ArgsJSON,
		"duplex 路径：ArgsJSON 必须与 arguments_json fixture 逐字节相等")
	require.Equal(t, uint64(7), duplexCalls[0].ExecID)
	require.Equal(t, []byte("exec-id"), duplexCalls[0].IDField)

	// 路径 2：独立 parser（无 duplex）从同一 exec 帧补齐ExecID/IDField 并自行解码。
	p := cursorNewStreamParser("grok-4.7")
	p.attachToolSchemas(defs)
	_, err = p.cursorParseMessageFrame(frame)
	require.NoError(t, err)
	parserCalls := p.capturedToolCalls()
	require.Len(t, parserCalls, 1)
	require.Equal(t, "call_sdk_1", parserCalls[0].ID)
	require.Equal(t, "get_weather", parserCalls[0].Name)
	require.Equal(t, cursorSDKTransportArgsFixture, parserCalls[0].ArgsJSON,
		"parser 路径：ArgsJSON 必须与 arguments_json fixture 逐字节相等")
	require.Equal(t, uint64(7), parserCalls[0].ExecID)
	require.Equal(t, []byte("exec-id"), parserCalls[0].IDField)
}

// TestCursorSDKTransportDecodeRejectsInvalid 表驱动：非法运输帧一律返回错误且
// 零委托登记。零委托断言用独立 parser 路径（本地记录，不会读取 duplex 结果）。
func TestCursorSDKTransportDecodeRejectsInvalid(t *testing.T) {
	defs := cursorSDKTransportDeclaredDefs(t, "get_weather")
	const okArgs = `{"city":"Beijing"}`

	badBoolValue := cursorAppendVarintField(nil, 4, 1) // structpb.Value{bool_value:true}
	cases := []struct {
		name string
		args []byte
		defs [][]byte
	}{
		{
			name: "未知工具不在 f4 enum",
			args: cursorSDKTransportBuildMcpArgs("c", "not_declared", okArgs),
			defs: defs,
		},
		{
			name: "外层工具名不是 invoke_client_tool",
			args: cursorSDKTransportBuildMcpArgsRaw("c", "get_weather",
				cursorSDKTransportStringEntries("get_weather", okArgs)...),
			defs: defs,
		},
		{
			name: "缺 tool_name 键",
			args: cursorSDKTransportBuildMcpArgsRaw("c", cursorTransportToolName,
				cursorSDKTransportArgEntry{key: cursorTransportArgumentsKey, value: cursorSDKTransportStringValue(okArgs)}),
			defs: defs,
		},
		{
			name: "缺 arguments_json 键",
			args: cursorSDKTransportBuildMcpArgsRaw("c", cursorTransportToolName,
				cursorSDKTransportArgEntry{key: cursorTransportToolNameKey, value: cursorSDKTransportStringValue("get_weather")}),
			defs: defs,
		},
		{
			name: "多出一个键",
			args: cursorSDKTransportBuildMcpArgsRaw("c", cursorTransportToolName,
				append(cursorSDKTransportStringEntries("get_weather", okArgs),
					cursorSDKTransportArgEntry{key: "extra", value: cursorSDKTransportStringValue("x")})...),
			defs: defs,
		},
		{
			name: "重复 tool_name 键",
			args: cursorSDKTransportBuildMcpArgsRaw("c", cursorTransportToolName,
				append(cursorSDKTransportStringEntries("get_weather", okArgs),
					cursorSDKTransportArgEntry{key: cursorTransportToolNameKey, value: cursorSDKTransportStringValue("get_weather")})...),
			defs: defs,
		},
		{
			name: "值非字符串",
			args: cursorSDKTransportBuildMcpArgsRaw("c", cursorTransportToolName,
				cursorSDKTransportArgEntry{key: cursorTransportToolNameKey, value: badBoolValue},
				cursorSDKTransportArgEntry{key: cursorTransportArgumentsKey, value: cursorSDKTransportStringValue(okArgs)}),
			defs: defs,
		},
		{
			name: "坏 JSON 截断",
			args: cursorSDKTransportBuildMcpArgs("c", "get_weather", `{"city":`),
			defs: defs,
		},
		{
			name: "数组参数",
			args: cursorSDKTransportBuildMcpArgs("c", "get_weather", `[1,2]`),
			defs: defs,
		},
		{
			name: "null 参数",
			args: cursorSDKTransportBuildMcpArgs("c", "get_weather", `null`),
			defs: defs,
		},
		{
			name: "标量参数",
			args: cursorSDKTransportBuildMcpArgs("c", "get_weather", `42`),
			defs: defs,
		},
		{
			name: "尾随第二文档",
			args: cursorSDKTransportBuildMcpArgs("c", "get_weather", `{"a":1} {"b":2}`),
			defs: defs,
		},
		{
			name: "空 f4 定义（空声明不能接收工具调用）",
			args: cursorSDKTransportBuildMcpArgs("c", "get_weather", okArgs),
			defs: nil,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			p := cursorNewStreamParser("grok-4.7")
			p.attachToolSchemas(c.defs)
			_, err := p.cursorParseMessageFrame(cursorSDKTransportExecFrame(c.args))
			require.Error(t, err, c.name)
			require.Empty(t, p.capturedToolCalls(), c.name+": 非法帧不得登记任何委托")
		})
	}
}

// TestCursorSDKTransportDecodeLegacyFormatRejected 旧直接参数格式（f5 是客户端
// 工具名、f2 直接是原参数键）必须拒绝：既不按旧格式解码，也不做 fallback。
func TestCursorSDKTransportDecodeLegacyFormatRejected(t *testing.T) {
	defs := cursorSDKTransportDeclaredDefs(t, "get_weather")
	legacy := cursorSDKTransportBuildMcpArgsRaw("call_legacy", "get_weather",
		cursorSDKTransportArgEntry{key: "city", value: cursorSDKTransportStringValue("Beijing")},
		cursorSDKTransportArgEntry{key: "days", value: cursorSDKTransportStringValue("3")})

	p := cursorNewStreamParser("grok-4.7")
	p.attachToolSchemas(defs)
	_, err := p.cursorParseMessageFrame(cursorSDKTransportExecFrame(legacy))
	require.Error(t, err, "旧直接参数格式必须被拒绝，不得 fallback")
	require.Empty(t, p.capturedToolCalls(), "拒绝后不得登记任何委托")
}
