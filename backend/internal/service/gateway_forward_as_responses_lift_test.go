//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 原生转发路径把 input 里的 additional_tools 提升为顶层 tools 时，必须按工具身份合并：
// 顶层 tools 与 additional_tools 是同一份工具集的两个载体，客户端两处都写同一工具时
// 上游只能收到一份。直接 append 会发出两条同名定义——实测上游 relay 会回空正文。
func TestLiftResponsesAdditionalToolsDeduplicatesAcrossSites(t *testing.T) {
	body := map[string]any{
		"tools": []any{
			map[string]any{"type": "function", "name": "get_weather", "description": "top-level"},
		},
		"input": []any{
			map[string]any{"type": "additional_tools", "tools": []any{
				map[string]any{"type": "function", "name": "get_weather", "description": "additional"},
			}},
			map[string]any{"type": "message", "role": "user", "content": "hi"},
		},
	}

	changed, err := liftResponsesAdditionalTools(body)
	require.NoError(t, err)
	require.True(t, changed)

	tools, ok := body["tools"].([]any)
	require.True(t, ok)
	require.Len(t, tools, 1, "同一工具两处声明只算一个")
	first, _ := tools[0].(map[string]any)
	require.Equal(t, "top-level", first["description"], "先见者为准")

	input, _ := body["input"].([]any)
	require.Len(t, input, 1, "声明条目提升后必须从 input 删除")
	require.Equal(t, "message", input[0].(map[string]any)["type"])
}

// 只在站点之间去重：同一批 additional_tools 里出现两个同名声明是客户端定义冲突，
// 原样保留交给下游显式报错，不能在提升阶段被悄悄合并成一个。
func TestLiftResponsesAdditionalToolsKeepsWithinBatchDuplicates(t *testing.T) {
	body := map[string]any{
		"input": []any{
			map[string]any{"type": "additional_tools", "tools": []any{
				map[string]any{"type": "function", "name": "wait"},
				map[string]any{"type": "function", "name": "wait"},
			}},
		},
	}

	changed, err := liftResponsesAdditionalTools(body)
	require.NoError(t, err)
	require.True(t, changed)

	tools, _ := body["tools"].([]any)
	require.Len(t, tools, 2, "批内重复保留，交给下游冲突检查")
}

// 没有声明条目时不得改动请求体（不重写、不新增 tools 字段）。
func TestLiftResponsesAdditionalToolsNoopWithoutDeclarations(t *testing.T) {
	body := map[string]any{
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "hi"}},
	}
	changed, err := liftResponsesAdditionalTools(body)
	require.NoError(t, err)
	require.False(t, changed)
	_, hasTools := body["tools"]
	require.False(t, hasTools)
}
