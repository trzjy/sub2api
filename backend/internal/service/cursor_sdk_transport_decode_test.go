//go:build unit

package service

// CUR-SVC4：响应侧「无损运输」解码验收。
//
// 上游只回调一个 invoke_client_tool，f2 map 只含 tool_name / arguments_json 两个
// 字符串值。本站保留 arguments_json 原始字节：不转 map、不补 null、不猜类型、
// 不做 schema retype、不接受旧直接参数格式。duplex 与独立 parser 两条消费路径
// 共用同一份解码实现与同一份真实 f4 定义。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
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

// TestCursorSDKTransportParseExecClassification 覆盖独立 parser 对**不含 f11** 的 exec 帧
// 按统一字段表分类（方案 §8.2：该路径按同一 exec 字段表分类，不静默 skip 未实现执行）：
// 意外 native / 未知执行字段明确失败且零委托，纯元数据按原职责忽略；合法 f11 行为不变。
func TestCursorSDKTransportParseExecClassification(t *testing.T) {
	// buildExec 组装 AgentServerMessage.f2 ExecServerMessage{f1 exec_id, f15 id, extra}。
	buildExec := func(extra []byte) []byte {
		exec := cursorAppendVarintField(nil, 1, 7)
		exec = cursorAppendBytesField(exec, 15, []byte("exec-id"))
		exec = append(exec, extra...)
		return cursorAppendBytesField(nil, cursorFldServerExecMessage, exec)
	}

	cases := []struct {
		name       string
		extra      []byte
		wantErr    bool
		wantErrMsg string
	}{
		{
			name:       "意外 native exec 明确失败",
			extra:      cursorAppendBytesField(nil, cursorFldNativeRejectShellArgs, []byte("ls -la")),
			wantErr:    true,
			wantErrMsg: "unexpected native exec field 2",
		},
		{
			name:       "未知执行字段明确失败",
			extra:      cursorAppendBytesField(nil, 40, []byte("unknown")),
			wantErr:    true,
			wantErrMsg: "unsupported exec fields",
		},
		{
			name:  "纯元数据按原职责忽略",
			extra: cursorAppendBytesField(nil, cursorFldExecMetaSpanContext, []byte("span")),
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			p := cursorNewStreamParser("grok-4.7")
			p.attachToolSchemas(cursorSDKTransportDeclaredDefs(t, "get_weather"))
			_, err := p.cursorParseMessageFrame(buildExec(c.extra))
			if c.wantErr {
				require.Error(t, err, c.name)
				require.Contains(t, err.Error(), c.wantErrMsg)
			} else {
				require.NoError(t, err, c.name)
			}
			require.Empty(t, p.capturedToolCalls(), c.name+": 不含 f11 的帧不得登记任何委托")
		})
	}

	// 合法 f11 运输帧仍登记 1 条（既有捕获逻辑不变）。
	p := cursorNewStreamParser("grok-4.7")
	p.attachToolSchemas(cursorSDKTransportDeclaredDefs(t, "get_weather"))
	frame := cursorSDKTransportExecFrame(
		cursorSDKTransportBuildMcpArgs("call_sdk_ok", "get_weather", `{"city":"Beijing"}`))
	_, err := p.cursorParseMessageFrame(frame)
	require.NoError(t, err)
	require.Len(t, p.capturedToolCalls(), 1, "合法 f11 运输帧仍登记 1 条委托")
}

// ---- CUR-PREC4P：参数精度贯穿四协议实际输出 ----
//
// 复用 textonly 的四协议真实 writer 驱动与"混入真实 f11"路径，只把运输参数换成
// cursorSDKTransportArgsFixture（大整数 / null / false / 0 / 空串 / 嵌套数组 /
// 中文 HTML / 转义引号），再按每协议真实交付形态断言：Chat/Responses 交付的
// arguments 字符串逐字节相等；Messages/Gemini 的 input/args 对象用 UseNumber
// 逐字段语义相等（大整数不得被 float64 改成 9007199254740992）。

// cursorPrecisionDrive 与 cursorTextOnlyDrive 同构，唯一差异是 f11 委托携带
// cursorSDKTransportArgsFixture 而非固定 {"city":"Beijing"}。协议解析/回桥/输出
// 读取全部复用既有 helper，不另造协议解析。
func cursorPrecisionDrive(t *testing.T, mode cursorProtocol, stream bool) cursorTextOnlyRun {
	t.Helper()

	delegations := newCursorToolDelegations()
	stamp := fmt.Sprintf("prec-p%d-s%t", mode, stream)
	session := &cursorSessionRecord{
		id:          "sess-" + stamp,
		spec:        cursorSessionSpec{protocol: mode},
		state:       cursorSessionRunning,
		toolCallIDs: make(map[string]struct{}),
		pending:     &cursorPendingBatch{batchID: "batch-" + stamp},
	}
	session.attachDelegations(delegations)

	initBody, err := cursorConvertRequest(cursorSDKTransportRequest("get_weather"), "m")
	require.NoError(t, err)
	d, err := newCursorAgentDuplex(initBody, delegations)
	require.NoError(t, err)
	defer d.close(nil)

	p := cursorNewStreamParser("m")
	p.attachToolDelegations(delegations)
	p.attachToolSchemas(d.declaredToolDefs)

	// 真实 f11：exec_id=7 / f15 id=exec-id / 工具 get_weather / fixture 参数。
	f11 := cursorSDKTransportExecFrame(
		cursorSDKTransportBuildMcpArgs("call_real_f11", "get_weather", cursorSDKTransportArgsFixture))
	done, err := d.respond(f11)
	require.NoError(t, err)
	require.False(t, done, "f11 委托不得终结双向连接")

	// 与 textonly 混合路径同构的流体：reasoning 文本 + 真实 f11 envelope +
	// content 文本 + turn_ended。
	var frames []byte
	for _, text := range cursorTextOnlyReasoningFrames(t) {
		frames = append(frames, cursorTextDeltaFrame(t, text)...)
	}
	frames = append(frames, cursorEncodeEnvelope(f11)...)
	for _, text := range cursorTextOnlyContentFrames(t) {
		frames = append(frames, cursorTextDeltaFrame(t, text)...)
	}
	frames = append(frames, cursorTurnEndedFrame(t, [2]uint64{1, 10}, [2]uint64{2, 5})...)

	s := &cursorSSEStream{source: io.NopCloser(bytes.NewReader(frames)), parser: p}
	sseBody, err := io.ReadAll(s)
	require.NoError(t, err, "parser 必须成功产出 Chat Completions SSE")
	require.Contains(t, string(sseBody), "data:")

	// 非空 svc/session：与既有驱动同构。
	repo := &identityCursorQuotaKeys{byID: map[int64]*ent.QuotaKey{}}
	svc := newCursorTestExecutionService(t, repo, NewCursorTestEncryptor(),
		cursorTestDeviceIDs(repo), NewCursorUpstreamForwarder())

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	w := newCursorProtocolWriter(c.Writer, stream, mode, "m", cursorResponsesToolMeta{},
		"resp-prec", svc, session)

	if mode == cursorProtocolChat {
		if stream {
			_, err = io.WriteString(w.ResponseWriter, string(sseBody))
		} else {
			var completion []byte
			completion, err = cursorAggregateSSEToCompletion(context.Background(), sseBody)
			require.NoError(t, err)
			_, err = io.WriteString(w.ResponseWriter, string(completion))
		}
	} else {
		if stream {
			_, err = w.Write(sseBody)
		} else {
			var completion []byte
			completion, err = cursorAggregateSSEToCompletion(context.Background(), sseBody)
			require.NoError(t, err)
			_, err = w.Write(completion)
		}
		require.NoError(t, err)
		require.NoError(t, w.finalize())
	}
	require.NoError(t, err)

	for _, call := range delegations.snapshot() {
		session.toolCallIDs[call.wireID()] = struct{}{}
	}
	return cursorTextOnlyRun{body: rec.Body.String(), writer: w,
		calls: p.capturedToolCalls(), session: session}
}

// cursorPrecisionDecodeObject 用 UseNumber 解码，避免大整数被 float64 改写。
func cursorPrecisionDecodeObject(t *testing.T, raw string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var out map[string]any
	require.NoError(t, dec.Decode(&out), "参数必须是对象 JSON（UseNumber 解码）")
	return out
}

// cursorPrecisionAssertObjectArgs 按 UseNumber 语义逐字段比对 fixture。
func cursorPrecisionAssertObjectArgs(t *testing.T, got map[string]any, label string) {
	t.Helper()
	want := cursorPrecisionDecodeObject(t, cursorSDKTransportArgsFixture)
	require.Equal(t, want, got, label+": UseNumber 逐字段必须等于 fixture")
	require.Equal(t, json.Number("9007199254740993"), got["big"],
		label+": big 必须精确等于 9007199254740993，不得变成 9007199254740992")
	require.Contains(t, got, "null")
	require.Nil(t, got["null"], label+": null 必须保留")
	require.Equal(t, false, got["false"], label+": false 必须保留")
	require.Equal(t, json.Number("0"), got["zero"], label+": 0 必须保留")
	require.Equal(t, "", got["empty"], label+": 空串必须保留")
}

// cursorPrecisionAssertDelegation 四协议共同的委托底线：仅 1 条真实 f11，元数据原样，
// 输出与委托均不出现 DSML 调用。
func cursorPrecisionAssertDelegation(t *testing.T, run cursorTextOnlyRun) {
	t.Helper()
	snap := run.session.delegations.snapshot()
	require.Len(t, snap, 1, "仅 1 条真实 f11 委托")
	require.Equal(t, "get_weather", snap[0].Name)
	require.Equal(t, uint64(7), snap[0].ExecID)
	require.Equal(t, []byte("exec-id"), snap[0].IDField)
	require.Equal(t, cursorSDKTransportArgsFixture, snap[0].ArgsJSON, "委托 ArgsJSON 逐字节原样")
	require.NotContains(t, snap[0].ID, "call_dsml_", "委托不得带 DSML 前缀")
	require.Equal(t, snap, run.calls, "parser 与会话读取同一份委托记录")
	cursorTextOnlyAssertNoDSML(t, run.body)
}

// cursorPrecisionChatArgs 取 Chat 实际交付的 tool_calls[].function.arguments。
// 流式按 index 累加分片，非流式直接读 message.tool_calls。
func cursorPrecisionChatArgs(t *testing.T, stream bool, body string) (string, string) {
	t.Helper()
	var args, name string
	if stream {
		byIndex := make(map[int]string)
		for _, data := range cursorTextOnlySSEData(body) {
			var e apicompat.ChatCompletionsChunk
			require.NoError(t, json.Unmarshal([]byte(data), &e))
			for _, ch := range e.Choices {
				for _, tc := range ch.Delta.ToolCalls {
					idx := 0
					if tc.Index != nil {
						idx = *tc.Index
					}
					byIndex[idx] += tc.Function.Arguments
					if tc.Function.Name != "" {
						name = tc.Function.Name
					}
				}
			}
		}
		require.Len(t, byIndex, 1, "Chat 流式只交付 1 个 tool_calls 项")
		args = byIndex[0]
	} else {
		var resp apicompat.ChatCompletionsResponse
		require.NoError(t, json.Unmarshal([]byte(body), &resp))
		require.Len(t, resp.Choices, 1)
		tcs := resp.Choices[0].Message.ToolCalls
		require.Len(t, tcs, 1, "Chat 非流式只交付 1 个 tool_calls 项")
		args = tcs[0].Function.Arguments
		name = tcs[0].Function.Name
	}
	return name, args
}

// cursorPrecisionResponsesArgs 取 Responses 实际交付的 function_call.arguments。
func cursorPrecisionResponsesArgs(t *testing.T, stream bool, body string) (string, string) {
	t.Helper()
	var args, name string
	if stream {
		var delta strings.Builder
		var done string
		// added/done 成对描述同一 item，按 item ID 去重后才是一条交付项。
		items := make(map[string]string)
		for _, data := range cursorTextOnlySSEData(body) {
			var e apicompat.ResponsesStreamEvent
			require.NoError(t, json.Unmarshal([]byte(data), &e))
			switch e.Type {
			case "response.function_call_arguments.delta":
				delta.WriteString(e.Delta)
			case "response.function_call_arguments.done":
				done = e.Arguments
			case "response.output_item.added", "response.output_item.done":
				if e.Item != nil && e.Item.Type == "function_call" {
					key := e.Item.ID
					if key == "" {
						key = e.Item.CallID
					}
					items[key] = e.Item.Name
				}
			}
		}
		require.Len(t, items, 1, "Responses 流式只交付 1 个 function_call 项")
		for _, itemName := range items {
			name = itemName
		}
		require.Equal(t, delta.String(), done, "done.arguments 必须等于流式 delta 累加值")
		args = done
	} else {
		var resp apicompat.ResponsesResponse
		require.NoError(t, json.Unmarshal([]byte(body), &resp))
		var items []apicompat.ResponsesOutput
		for _, item := range resp.Output {
			if item.Type == "function_call" {
				items = append(items, item)
			}
		}
		require.Len(t, items, 1, "Responses 非流式只交付 1 个 function_call 项")
		args = items[0].Arguments
		name = items[0].Name
	}
	return name, args
}

// cursorPrecisionAnthropicTool 取 Messages 实际交付的 tool_use.input（流式由
// input_json_delta 分片还原），返回 ID/Name/input JSON 原文。
func cursorPrecisionAnthropicTool(t *testing.T, stream bool, body string) (string, string, string) {
	t.Helper()
	var id, name, input string
	if stream {
		for _, data := range cursorTextOnlySSEData(body) {
			var e apicompat.AnthropicStreamEvent
			require.NoError(t, json.Unmarshal([]byte(data), &e))
			if e.Type == "content_block_start" && e.ContentBlock != nil && e.ContentBlock.Type == "tool_use" {
				id = e.ContentBlock.ID
				name = e.ContentBlock.Name
			}
			if e.Type == "content_block_delta" && e.Delta != nil && e.Delta.Type == "input_json_delta" {
				input += e.Delta.PartialJSON
			}
		}
	} else {
		var resp apicompat.AnthropicResponse
		require.NoError(t, json.Unmarshal([]byte(body), &resp))
		var blocks []apicompat.AnthropicContentBlock
		for _, b := range resp.Content {
			if b.Type == "tool_use" {
				blocks = append(blocks, b)
			}
		}
		require.Len(t, blocks, 1, "Messages 非流式只交付 1 个 tool_use 块")
		id = blocks[0].ID
		name = blocks[0].Name
		input = string(blocks[0].Input)
	}
	require.NotEmpty(t, input, "tool_use.input 不得为空")
	return id, name, input
}

// cursorPrecisionGeminiCalls 从一帧 Gemini 输出里取所有 functionCall。
func cursorPrecisionGeminiCalls(t *testing.T, data string) []map[string]any {
	t.Helper()
	frame := cursorPrecisionDecodeObject(t, data)
	cands, _ := frame["candidates"].([]any)
	var out []map[string]any
	for _, c := range cands {
		cm, _ := c.(map[string]any)
		content, _ := cm["content"].(map[string]any)
		parts, _ := content["parts"].([]any)
		for _, p := range parts {
			pm, _ := p.(map[string]any)
			if fc, ok := pm["functionCall"].(map[string]any); ok {
				out = append(out, fc)
			}
		}
	}
	return out
}

// TestCursorSDKTransportArgsPrecisionAcrossProtocols 把 cursorSDKTransportArgsFixture
// 贯穿 Chat/Messages/Responses/Gemini × 流/非流的真实 writer 输出，按各协议真实交付
// 形态断言精度不丢、不被 float64 改写。
func TestCursorSDKTransportArgsPrecisionAcrossProtocols(t *testing.T) {
	for _, c := range cursorTextOnlyCases() {
		c := c
		t.Run(c.name, func(t *testing.T) {
			run := cursorPrecisionDrive(t, c.mode, c.stream)
			cursorPrecisionAssertDelegation(t, run)

			switch c.mode {
			case cursorProtocolChat:
				name, args := cursorPrecisionChatArgs(t, c.stream, run.body)
				require.Equal(t, "get_weather", name)
				require.NotContains(t, name, "Bash")
				require.Equal(t, cursorSDKTransportArgsFixture, args,
					"Chat tool_calls[].function.arguments 必须与 fixture 逐字节相等")
			case cursorProtocolResponses:
				name, args := cursorPrecisionResponsesArgs(t, c.stream, run.body)
				require.Equal(t, "get_weather", name)
				require.NotContains(t, name, "Bash")
				require.Equal(t, cursorSDKTransportArgsFixture, args,
					"Responses function_call.arguments 必须与 fixture 逐字节相等")
			case cursorProtocolAnthropic:
				id, name, input := cursorPrecisionAnthropicTool(t, c.stream, run.body)
				require.Equal(t, "get_weather", name)
				require.NotContains(t, id, "call_dsml_")
				cursorPrecisionAssertObjectArgs(t, cursorPrecisionDecodeObject(t, input),
					"Messages tool_use.input")
			case cursorProtocolGemini:
				var calls []map[string]any
				if c.stream {
					for _, data := range cursorTextOnlySSEData(run.body) {
						calls = append(calls, cursorPrecisionGeminiCalls(t, data)...)
					}
				} else {
					calls = cursorPrecisionGeminiCalls(t, run.body)
				}
				require.Len(t, calls, 1, "Gemini 只交付 1 个 functionCall")
				name, _ := calls[0]["name"].(string)
				require.Equal(t, "get_weather", name)
				id, _ := calls[0]["id"].(string)
				require.NotContains(t, id, "call_dsml_")
				args, ok := calls[0]["args"].(map[string]any)
				require.True(t, ok, "Gemini functionCall.args 必须是对象")
				cursorPrecisionAssertObjectArgs(t, args, "Gemini functionCall.args")
			}
			t.Logf("[PREC/%s] body=%d bytes args preserved (big=9007199254740993)",
				c.name, len(run.body))
		})
	}
}
