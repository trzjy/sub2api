//go:build unit

package service

// Cursor 通道阶段 0（根因 B）：namespace 内 custom 工具被丢弃。
//
// 背景一句话：codex 的核心执行工具 exec 声明在 functions namespace 里、类型是 custom。
// 改造前 chat 桥自己实现了一份 namespace 摊平，只认 function 子工具，把 custom 子工具
// continue 丢掉，于是 functions__exec 从未出现在发给上游的 f4（McpTools）里，模型看不见
// 它，就改用 Cursor 自带的原生 shell 工具，被我们拒绝后整轮失败。
//
// 本文件用 codex 真实抓包形状过一遍 请求→f4 与 回程 两条链路，并钉住：
//   - f4 里 functions__exec 必须出现（改造前没有——已实证）；
//   - 它的 McpToolDefinition 不带 f3（input_schema）/ f6（旧 input_schema_json）；
//   - 同 namespace 内 function 子工具（functions__wait）依旧带 f3（schema 保留）；
//   - 回程把 functions__exec 判成 custom_tool_call + 裸名 exec，自由文本参数原样交付；
//   - namespace 摊平名与顶层工具撞名时必须报错（歧义不可消除）。

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// codex 真实请求形状（抓包实证）。顶层 tools 为空，工具全部在 input[] 的
// additional_tools 条目里；functions namespace 下既有 custom 的 exec 也有 function 的 wait。
const cursorCodexRealShapeInput = `[
	{"type":"additional_tools","role":"developer","tools":[
		{"type":"namespace","name":"functions","tools":[
			{"type":"custom","name":"exec","description":"d","format":{"type":"grammar","definition":"g"}},
			{"type":"function","name":"wait","description":"d","parameters":{"type":"object","properties":{"x":{"type":"string"}}}}
		]},
		{"type":"namespace","name":"clock","tools":[
			{"type":"function","name":"sleep","description":"d","parameters":{"type":"object","properties":{"x":{"type":"string"}}}}
		]},
		{"type":"namespace","name":"collaboration","tools":[
			{"type":"function","name":"spawn_agent","description":"d","parameters":{"type":"object","properties":{"x":{"type":"string"}}}}
		]}
	]},
	{"type":"message","role":"user","content":[{"type":"input_text","text":"run dir"}]}
]`

// cursorCodexRealShapeRequest 构造上面那条真实请求。
func cursorCodexRealShapeRequest() *apicompat.ResponsesRequest {
	return &apicompat.ResponsesRequest{
		Model: "codex-x",
		Input: json.RawMessage(cursorCodexRealShapeInput),
	}
}

// TestCursorF4Custom_RealCodexShape 用 codex 真实形状跑完整 请求→f4 链路：
// 经 apicompat 的 Responses→Chat 桥（已收敛到 FlattenResponsesToolList 唯一事实源）
// 再到 cursorConvertRequest 编码出 f4。
func TestCursorF4Custom_RealCodexShape(t *testing.T) {
	respReq := cursorCodexRealShapeRequest()

	// 1) Responses→Chat 桥转换：顶层 custom 与 namespace 内 custom 都应存活。
	chatReq, err := apicompat.ResponsesToChatCompletionsRequest(respReq)
	require.NoError(t, err)
	require.NotEmpty(t, chatReq.Tools, "codex 的工具（含 functions__exec）必须被转换出来")
	byName := make(map[string]apicompat.ChatTool)
	for _, tool := range chatReq.Tools {
		byName[tool.Function.Name] = tool
	}
	require.Contains(t, byName, "functions__exec", "functions__exec 必须出现在转换结果里（改造前被丢弃）")
	require.True(t, byName["functions__exec"].Custom, "functions__exec 是 custom 子工具，必须打 Custom 标记")
	require.Empty(t, byName["functions__exec"].Function.Parameters, "custom 子工具不携带 schema")

	// 2) 编码出 f4 并逐项解析 McpToolDefinition。
	body, err := cursorConvertRequest(chatReq, "codex-x")
	require.NoError(t, err)

	run := cursorAgentRunOf(t, body)
	mcp, ok := cursorFindField(run, cursorFldRunMcpTools)
	require.True(t, ok, "f4 (McpTools) 必须存在")
	require.NotEmpty(t, mcp.Bytes)

	mcpFields, e := cursorDecodeProtobuf(mcp.Bytes)
	require.NoError(t, e)
	defs := cursorAllFields(mcpFields, cursorFldMcpToolsTool)
	require.Len(t, defs, 4, "codex 真实形状应声明 4 个工具（exec/custom、wait、sleep、spawn_agent）")

	defByName := make(map[string][]cursorPBField)
	for _, def := range defs {
		fields, e := cursorDecodeProtobuf(def.Bytes)
		require.NoError(t, e)
		name := cursorTestFieldString(fields, cursorFldMcpToolDefName)
		defByName[name] = fields
	}

	// 3) functions__exec 的 McpToolDefinition 不带 f3（input_schema）/ f6（旧 input_schema_json）。
	execDef, ok := defByName["functions__exec"]
	require.True(t, ok, "f4 里必须有 functions__exec")
	_, hasF3 := cursorFindField(execDef, cursorFldMcpToolDefInputSchemaValue)
	_, hasF6 := cursorFindField(execDef, cursorFldMcpToolDefInputSchemaLegacy)
	assert.False(t, hasF3, "custom 工具（functions__exec）的 f4 不得写 f3 input_schema")
	assert.False(t, hasF6, "custom 工具（functions__exec）的 f4 不得写 f6")
	// 身份字段仍齐全（f1 name / f2 description / f4 provider / f5 tool_name）。
	assert.Equal(t, "functions__exec", cursorTestFieldString(execDef, cursorFldMcpToolDefName))
	assert.Equal(t, cursorMcpProviderIdentifier, cursorTestFieldString(execDef, cursorFldMcpToolDefProvider))
	assert.Equal(t, "functions__exec", cursorTestFieldString(execDef, cursorFldMcpToolDefToolName))

	// 4) 同 namespace 内 function 子工具 functions__wait 依旧带 f3（schema 保留）。
	waitDef, ok := defByName["functions__wait"]
	require.True(t, ok, "f4 里必须有 functions__wait")
	assert.JSONEq(t, `{"type":"object","properties":{"x":{"type":"string"}}}`,
		cursorTestRestoreF3Schema(t, waitDef), "function 子工具的 schema 必须原样保留在 f3")

	// clock__sleep / collaboration__spawn_agent 也带 f3（验证不是个别特例）。
	for _, n := range []string{"clock__sleep", "collaboration__spawn_agent"} {
		def, ok := defByName[n]
		require.True(t, ok, "f4 里必须有 "+n)
		assert.JSONEq(t, `{"type":"object","properties":{"x":{"type":"string"}}}`,
			cursorTestRestoreF3Schema(t, def), n+" 的 schema 必须保留")
	}
}

// TestCursorF4Custom_NamespaceCustomNoSchema 单独钉住：namespace 内 custom 子工具的
// f4 形态——McpToolDefinition 不带 f3 / f6。用最简形状隔离验证。
func TestCursorF4Custom_NamespaceCustomNoSchema(t *testing.T) {
	req := &apicompat.ResponsesRequest{
		Model: "codex-x",
		Input: json.RawMessage(`[
			{"type":"additional_tools","role":"developer","tools":[
				{"type":"namespace","name":"functions","tools":[
					{"type":"custom","name":"exec","description":"run code"}
				]}
			]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
		]`),
	}
	chatReq, err := apicompat.ResponsesToChatCompletionsRequest(req)
	require.NoError(t, err)
	require.Len(t, chatReq.Tools, 1)
	require.Equal(t, "functions__exec", chatReq.Tools[0].Function.Name)
	require.True(t, chatReq.Tools[0].Custom)

	defs := cursorTestDeclaredToolDefs(t, chatReq)
	require.Len(t, defs, 1)
	fields, e := cursorDecodeProtobuf(defs[0])
	require.NoError(t, e)
	_, hasF3 := cursorFindField(fields, cursorFldMcpToolDefInputSchemaValue)
	_, hasF6 := cursorFindField(fields, cursorFldMcpToolDefInputSchemaLegacy)
	assert.False(t, hasF3, "namespace custom 子工具不得写 f3")
	assert.False(t, hasF6, "namespace custom 子工具不得写 f6")
}

// TestCursorF4Custom_NamespaceFunctionHasSchema 单独钉住：namespace 内 function 子工具的
// f4 形态——McpToolDefinition 带 f3（schema 保留）。
func TestCursorF4Custom_NamespaceFunctionHasSchema(t *testing.T) {
	req := &apicompat.ResponsesRequest{
		Model: "codex-x",
		Input: json.RawMessage(`[
			{"type":"additional_tools","role":"developer","tools":[
				{"type":"namespace","name":"functions","tools":[
					{"type":"function","name":"wait","description":"d","parameters":{"type":"object","properties":{"x":{"type":"string"}}}}
				]}
			]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
		]`),
	}
	chatReq, err := apicompat.ResponsesToChatCompletionsRequest(req)
	require.NoError(t, err)
	require.Len(t, chatReq.Tools, 1)
	require.Equal(t, "functions__wait", chatReq.Tools[0].Function.Name)
	require.False(t, chatReq.Tools[0].Custom, "function 子工具不带 Custom 标记")

	defs := cursorTestDeclaredToolDefs(t, chatReq)
	require.Len(t, defs, 1)
	fields, e := cursorDecodeProtobuf(defs[0])
	require.NoError(t, e)
	assert.JSONEq(t, `{"type":"object","properties":{"x":{"type":"string"}}}`,
		cursorTestRestoreF3Schema(t, fields), "function 子工具的 schema 必须保留在 f3")
}

// TestCursorF4Custom_ReturnPathDeliversBareName 钉住回程一致性（第 4 步，只验证不改动
// cursor_execution_service.go / cursor_protocol_response.go）：f4 里出现 functions__exec
// 后，回程判定成 custom_tool_call + 裸名 exec，且自由文本参数按"无 schema"原样交付。
func TestCursorF4Custom_ReturnPathDeliversBareName(t *testing.T) {
	respReq := cursorCodexRealShapeRequest()

	// 元数据从同一份已提升的有效工具集派生（与下发声明同源）。
	effective, err := apicompat.EffectiveResponsesTools(respReq)
	require.NoError(t, err)
	meta, err := cursorToolMetaFromTools(effective)
	require.NoError(t, err)

	// 1) functions__exec 的摊平名命中 namespaceCustomTools → 判成 custom_tool_call / 裸名 exec。
	delivered := cursorResponsesDeliveredToolCall("functions__exec", meta)
	assert.Equal(t, "custom_tool_call", delivered.Kind, "functions__exec 必须判成 custom_tool_call")
	assert.Equal(t, "exec", delivered.Name, "回程必须是裸名 exec（不是 functions__exec）")

	// 同 namespace 内普通 function 子工具 wait 仍是 function_call（不串台）。
	waitDelivered := cursorResponsesDeliveredToolCall("functions__wait", meta)
	assert.Equal(t, "function_call", waitDelivered.Kind)
	assert.Equal(t, "wait", waitDelivered.Name)

	// 2) functions__exec 在 f4 里无 schema → cursorToolDeclaredWithoutSchema 判 true，
	//    自由文本参数原样交付（不被判"参数残缺"）。
	chatReq, err := apicompat.ResponsesToChatCompletionsRequest(respReq)
	require.NoError(t, err)
	defs := cursorTestDeclaredToolDefs(t, chatReq)
	assert.True(t, cursorToolDeclaredWithoutSchema(defs, "functions__exec"),
		"functions__exec 已声明但无 schema：自由文本参数必须原样交付")
	// function 子工具有 schema，不应被判成"无 schema"。
	assert.False(t, cursorToolDeclaredWithoutSchema(defs, "functions__wait"),
		"functions__wait 有 schema，不得被判成无 schema 自由文本工具")
}

// TestCursorF4Custom_FlattenCollisionErrors 钉住冲突语义：namespace 子工具摊平名与顶层
// 工具撞名时必须报错（歧义不可消除）。这是"收敛到 FlattenResponsesToolList 唯一事实源"
// 的必然结果——报错文案由 FlattenResponsesToolList 产出，而非旧 bridge 文案。
func TestCursorF4Custom_FlattenCollisionErrors(t *testing.T) {
	// 顶层 function 名为 functions__exec，与 namespace functions 下 custom 子工具 exec
	// 摊平出的 functions__exec 撞名。
	req := &apicompat.ResponsesRequest{
		Model: "codex-x",
		Input: json.RawMessage(`[
			{"type":"additional_tools","role":"developer","tools":[
				{"type":"function","name":"functions__exec"},
				{"type":"namespace","name":"functions","tools":[
					{"type":"custom","name":"exec"}
				]}
			]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
		]`),
	}
	_, err := apicompat.ResponsesToChatCompletionsRequest(req)
	require.Error(t, err, "摊平名与顶层工具撞名必须拒绝")
	assert.Contains(t, err.Error(), "functions__exec")
	assert.Contains(t, err.Error(), "cannot disambiguate")
}
