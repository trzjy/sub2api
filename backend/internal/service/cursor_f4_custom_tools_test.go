//go:build unit

package service

// Cursor custom 工具目录与原生回程契约：client 声明的 custom 工具原名与原 schema
// 出现在 f4 的工具目录里；回程 f11 按第三方原生 mcp_args 经 cursorDecodeMcpArgs
// 解码后回到真实工具名与逐 Value 解码的参数。请求侧声明改造由后续卡负责。

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCursorSDKTransportCustomToolCatalog 钉住 custom 工具的目录与原生回程契约：
//   - 当前 f4 仍只有一条定义（invoke_client_tool），目录在 f2 description；
//   - 目录保留 custom 工具原名与原 schema；
//   - 原生 f11 解码后回到真实工具名与原参数值。
func TestCursorSDKTransportCustomToolCatalog(t *testing.T) {
	const customSchema = `{"type":"object","properties":{"input":{"type":"string"}}}`
	req := &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools: []apicompat.ChatTool{
			{
				Type:   "function",
				Custom: true,
				Function: &apicompat.ChatFunction{
					Name:       "exec",
					Parameters: json.RawMessage(customSchema),
				},
			},
			{
				Type: "function",
				Function: &apicompat.ChatFunction{
					Name:       "wait",
					Parameters: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`),
				},
			},
		},
	}
	body, err := cursorConvertRequest(req, "codex-x")
	require.NoError(t, err)

	run := cursorAgentRunOf(t, body)
	mcp, ok := cursorFindField(run, cursorFldRunMcpTools)
	require.True(t, ok, "f4 (McpTools) 必须存在")
	require.NotEmpty(t, mcp.Bytes)
	mcpFields, e := cursorDecodeProtobuf(mcp.Bytes)
	require.NoError(t, e)
	defs := cursorAllFields(mcpFields, cursorFldMcpToolsTool)
	require.Len(t, defs, 1, "单入口运输：f4 只有一条定义")
	fields, e := cursorDecodeProtobuf(defs[0].Bytes)
	require.NoError(t, e)

	// 当前请求侧 f4 仍是单入口包装（声明改造属后续卡），用字面量避免引用将被删除的常量。
	assert.Equal(t, "invoke_client_tool", cursorTestFieldString(fields, cursorFldMcpToolDefName))
	assert.Equal(t, "invoke_client_tool", cursorTestFieldString(fields, cursorFldMcpToolDefToolName))

	// f2 description 中 "Tool catalog: " 之后是 JSON 数组，保留每个工具的原名与原 schema。
	desc := cursorTestFieldString(fields, cursorFldMcpToolDefDescription)
	idx := strings.Index(desc, "Tool catalog: ")
	require.NotEqual(t, -1, idx, "description 必须含 Tool catalog")
	var catalog []struct {
		Name       string          `json:"name"`
		Parameters json.RawMessage `json:"parameters"`
	}
	require.NoError(t, json.Unmarshal([]byte(desc[idx+len("Tool catalog: "):]), &catalog))
	require.Len(t, catalog, 2)
	byName := make(map[string]json.RawMessage, len(catalog))
	for _, entry := range catalog {
		byName[entry.Name] = entry.Parameters
	}
	require.Contains(t, byName, "exec", "目录必须保留 custom 工具原名")
	require.Contains(t, byName, "wait", "目录必须保留普通 function 工具原名")
	assert.Contains(t, string(byName["exec"]), `"input"`, "custom 工具的原 schema 文本必须出现在目录里")

	// 原生 f11：f5=真实工具名 exec，f2 map entry 的键是客户自有参数 input；
	// 声明夹具用本地 f4 定义（f5=exec），请求侧声明改造属后续卡。
	const argsJSON = `{"input":"echo hi"}`
	mcpArgs := cursorToolTestBuildMcpArgs("call_exec", "exec", []cursorToolTestMcpArgEntry{
		{key: "input", kind: "string", val: "echo hi"},
	})
	nativeDef := cursorTestBuildToolDef(t, "exec", customSchema)
	_, toolName, outArgsJSON, err := cursorDecodeMcpArgs(mcpArgs, [][]byte{nativeDef})
	require.NoError(t, err)
	assert.Equal(t, "exec", toolName, "真实工具名来自 f5")
	assert.Equal(t, argsJSON, outArgsJSON, "参数按 Value 解码后逐字节一致")
}

// TestCursorF4Custom_FlattenCollisionErrors 钉住冲突语义：namespace 子工具摊平名与顶层
// 工具撞名时必须报错（歧义不可消除）。
func TestCursorF4Custom_FlattenCollisionErrors(t *testing.T) {
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
