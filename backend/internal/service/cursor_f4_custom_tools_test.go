//go:build unit

package service

// Cursor custom 工具与原生回程契约：client 声明的 custom 工具以真实名与原 schema
// 单独出现在 f4（逐工具原生定义）；回程 f11 按第三方原生 mcp_args 经
// cursorDecodeMcpArgs 解码后回到真实工具名与逐 Value 解码的参数。

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCursorSDKTransportCustomToolCatalog 钉住 custom 工具的逐工具声明与原生回程契约：
//   - N 个工具 → N 条 McpToolDefinition，f1/f5 为真实名，f6 为原 schema；
//   - 无 f3、无教学提示；
//   - 原生 f11 解码后回到真实工具名与原参数值。
func TestCursorSDKTransportCustomToolCatalog(t *testing.T) {
	const customSchema = `{"type":"object","properties":{"input":{"type":"string"}}}`
	const waitSchema = `{"type":"object","properties":{"x":{"type":"string"}}}`
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
					Parameters: json.RawMessage(waitSchema),
				},
			},
		},
	}
	body, err := cursorConvertRequest(req, "codex-x")
	require.NoError(t, err)

	defs := cursorTestMcpDefsOf(t, body)
	require.Len(t, defs, 2, "逐工具声明：custom 与普通工具各一条")
	want := []struct{ name, schema string }{{"exec", customSchema}, {"wait", waitSchema}}
	for i, def := range defs {
		assert.Equal(t, want[i].name, cursorTestFieldString(def, cursorFldMcpToolDefName))
		assert.Equal(t, want[i].name, cursorTestFieldString(def, cursorFldMcpToolDefToolName))
		assert.Equal(t, cursorMcpProviderIdentifier, cursorTestFieldString(def, cursorFldMcpToolDefProvider))
		assert.Equal(t, want[i].schema, cursorTestF6SchemaOf(t, def), "原 schema 文本必须逐字节保留在 f6")
	}

	// 原生 f11：f5=真实工具名 exec，f2 map entry 的键是客户自有参数 input；
	// 声明夹具用本地 f4 定义（f5=exec），与逐工具原生声明同形。
	const argsJSON = `{"input":"echo hi"}`
	mcpArgs := cursorToolTestBuildMcpArgs("call_exec", "exec", []cursorToolTestMcpArgEntry{
		{key: "input", kind: "string", val: "echo hi"},
	})
	_, toolName, outArgsJSON, err := cursorDecodeMcpArgs(mcpArgs, [][]byte{cursorTestBuildToolDef(t, "exec", customSchema)})
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
