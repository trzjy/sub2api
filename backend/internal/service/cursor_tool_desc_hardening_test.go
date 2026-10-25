//go:build unit

package service

// B10 卡：出站工具描述「参数必须严格遵循 schema」加固后缀的钉字节单测。
//
// 动机（实测对照）：tool_choice=auto + 默认描述时，Cursor 上游对 claude 系模型会把
// 数组参数偶发写成 {"item":[...]}（数字退化为字符串），3/3 出现；描述显式写明结构
// 约束后 0/3。本文件钉死：出站 McpToolDefinition.description 追加固定后缀，其余
// 四字段（name / provider_identifier / tool_name / input_schema_json）不变，
// 且入站 ChatTool.Function.Description 零副作用。

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

// cursorBuildMcpToolsDescFields 解出首条 McpToolDefinition 的字段列表。
func cursorBuildMcpToolsDescFields(t *testing.T, tool apicompat.ChatTool) []cursorPBField {
	t.Helper()
	mcp := cursorBuildMcpTools([]apicompat.ChatTool{tool})
	require.NotEmpty(t, mcp, "tool must be declared")
	mcpFields, err := cursorDecodeProtobuf(mcp)
	require.NoError(t, err)
	defs := cursorAllFields(mcpFields, cursorFldMcpToolsTool)
	require.Len(t, defs, 1)
	fields, err := cursorDecodeProtobuf(defs[0].Bytes)
	require.NoError(t, err)
	return fields
}

// TestCursorBuildMcpToolsDescriptionGetsHardeningSuffix 钉死 D1（非空原描述）：
// description = 原描述 + 空格 + 后缀；其余四字段与入站一一对应、不变。
func TestCursorBuildMcpToolsDescriptionGetsHardeningSuffix(t *testing.T) {
	tool := apicompat.ChatTool{
		Type: "function",
		Function: &apicompat.ChatFunction{
			Name:        "get_weather",
			Description: "Get the weather.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		},
	}
	fields := cursorBuildMcpToolsDescFields(t, tool)

	require.Equal(t, "Get the weather."+" "+cursorToolSchemaHardeningSuffix,
		cursorTestFieldString(fields, cursorFldMcpToolDefDescription),
		"description must be original + space + hardening suffix")
	require.Equal(t, "get_weather", cursorTestFieldString(fields, cursorFldMcpToolDefName))
	require.Equal(t, cursorMcpProviderIdentifier, cursorTestFieldString(fields, cursorFldMcpToolDefProvider))
	require.Equal(t, "get_weather", cursorTestFieldString(fields, cursorFldMcpToolDefToolName))
	require.Equal(t, `{"type":"object","properties":{"city":{"type":"string"}}}`,
		cursorTestFieldString(fields, cursorFldMcpToolDefInputSchema))
}

// TestCursorBuildMcpToolsEmptyDescriptionBecomesSuffixAlone 钉死 D1（空原描述）：
// description == 后缀本身（无前置空格），其余四字段不变。
func TestCursorBuildMcpToolsEmptyDescriptionBecomesSuffixAlone(t *testing.T) {
	tool := apicompat.ChatTool{
		Type:     "function",
		Function: &apicompat.ChatFunction{Name: "no_desc", Parameters: json.RawMessage(`{"type":"object"}`)},
	}
	fields := cursorBuildMcpToolsDescFields(t, tool)

	require.Equal(t, cursorToolSchemaHardeningSuffix,
		cursorTestFieldString(fields, cursorFldMcpToolDefDescription),
		"empty description must yield the suffix alone")
	require.Equal(t, "no_desc", cursorTestFieldString(fields, cursorFldMcpToolDefName))
	require.Equal(t, cursorMcpProviderIdentifier, cursorTestFieldString(fields, cursorFldMcpToolDefProvider))
	require.Equal(t, "no_desc", cursorTestFieldString(fields, cursorFldMcpToolDefToolName))
	require.Equal(t, `{"type":"object"}`, cursorTestFieldString(fields, cursorFldMcpToolDefInputSchema))
}

// TestCursorBuildMcpToolsDoesNotMutateInboundDescription 钉死 D2：入站对象零副作用。
//
// 加固后缀只属于出站 McpToolDefinition；t.Function.Description 不得被就地改写
// （否则会污染同一工具的后续发送/回给客户端的内容）。
func TestCursorBuildMcpToolsDoesNotMutateInboundDescription(t *testing.T) {
	orig := "Get the weather."
	tool := apicompat.ChatTool{
		Type: "function",
		Function: &apicompat.ChatFunction{
			Name:        "get_weather",
			Description: orig,
			Parameters:  json.RawMessage(`{"type":"object"}`),
		},
	}
	_ = cursorBuildMcpTools([]apicompat.ChatTool{tool})
	require.Equal(t, orig, tool.Function.Description,
		"inbound ChatTool.Function.Description must not be modified in place")
}
