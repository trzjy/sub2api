//go:build unit

package apicompat

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// namespaceAdditionalToolsCarrier builds a Codex 0.166 request where the runtime
// tools live exclusively in the input[].additional_tools carrier with an empty
// top-level tools field. This is the official carrier shape the flatten layer
// previously could not see.
func namespaceAdditionalToolsCarrier() map[string]any {
	return map[string]any{
		"model": "codex",
		"tools": []any{},
		"input": []any{
			map[string]any{
				"type":  "additional_tools",
				"tools": []any{
					map[string]any{
						"type": "namespace",
						"name": "functions",
						"tools": []any{
							map[string]any{
								"type": "custom",
								"name": "exec",
								"format": map[string]any{
									"type":       "grammar",
									"syntax":     "lark",
									"definition": "start: COMMAND",
								},
							},
							map[string]any{
								"type":       "function",
								"name":       "wait",
								"parameters": map[string]any{"type": "object"},
							},
						},
					},
				},
			},
		},
	}
}

// 用例 A：官方载体形态被摊平，custom 子工具进入摊平视野且类型/format 原样保留。
func TestFlattenResponsesNamespaces_AdditionalToolsCarrierFlattens(t *testing.T) {
	req := namespaceAdditionalToolsCarrier()

	names, changed, err := FlattenResponsesNamespaces(req)
	require.NoError(t, err)
	require.True(t, changed)

	// 两个子工具都进入映射。
	require.Contains(t, names, "functions__exec")
	require.Equal(t, ResponsesNamespaceName{Namespace: "functions", Name: "exec"}, names["functions__exec"])
	require.Contains(t, names, "functions__wait")
	require.Equal(t, ResponsesNamespaceName{Namespace: "functions", Name: "wait"}, names["functions__wait"])

	tools, ok := req["tools"].([]any)
	require.True(t, ok)
	require.Len(t, tools, 2)

	byName := map[string]map[string]any{}
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		byName[stringValue(tool["name"])] = tool
	}

	// custom 子工具被摊平，type 仍为 custom，format 原样保留。
	execTool, ok := byName["functions__exec"]
	require.True(t, ok)
	require.Equal(t, "custom", execTool["type"])
	require.Contains(t, execTool, "format")
	format, _ := execTool["format"].(map[string]any)
	require.Equal(t, "grammar", format["type"])
	require.Equal(t, "lark", format["syntax"])

	// function 子工具被摊平。
	waitTool, ok := byName["functions__wait"]
	require.True(t, ok)
	require.Equal(t, "function", waitTool["type"])

	// 载体项本身不被从 input 删除（客户端声明保留 + 顶层补充摊平条目）。
	input, ok := req["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 1)
	carrier, _ := input[0].(map[string]any)
	require.Equal(t, "additional_tools", carrier["type"])
}

// 用例 B：映射里可查到 custom 子工具的 {Namespace, Name}。
func TestFlattenResponsesNamespaces_AdditionalToolsCarrierCustomMapping(t *testing.T) {
	req := namespaceAdditionalToolsCarrier()

	names, changed, err := FlattenResponsesNamespaces(req)
	require.NoError(t, err)
	require.True(t, changed)

	entry, ok := names["functions__exec"]
	require.True(t, ok)
	require.Equal(t, "functions", entry.Namespace)
	require.Equal(t, "exec", entry.Name)
}

// 用例 C：顶层形态不回归——工具放顶层 tools 时，摊平行为与改动前完全一致。
func TestFlattenResponsesNamespaces_TopLevelFormUnchanged(t *testing.T) {
	req := map[string]any{
		"tools": []any{
			map[string]any{
				"type": "namespace",
				"name": "functions",
				"tools": []any{
					map[string]any{
						"type": "custom",
						"name": "exec",
						"format": map[string]any{
							"type":   "grammar",
							"syntax": "lark",
						},
					},
					map[string]any{
						"type":       "function",
						"name":       "wait",
						"parameters": map[string]any{"type": "object"},
					},
				},
			},
		},
	}

	names, changed, err := FlattenResponsesNamespaces(req)
	require.NoError(t, err)
	require.True(t, changed)
	require.Contains(t, names, "functions__exec")
	require.Contains(t, names, "functions__wait")

	tools, ok := req["tools"].([]any)
	require.True(t, ok)
	require.Len(t, tools, 2)
	byName := map[string]map[string]any{}
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		byName[stringValue(tool["name"])] = tool
	}
	require.Equal(t, "custom", byName["functions__exec"]["type"])
	require.Equal(t, "function", byName["functions__wait"]["type"])
}

// 用例 D：混合形态——顶层与载体同时有工具时两者都被处理，撞名按既有语义报错。
func TestFlattenResponsesNamespaces_MixedTopLevelAndCarrierCollision(t *testing.T) {
	req := map[string]any{
		"tools": []any{
			map[string]any{"type": "function", "name": "functions__exec"},
		},
		"input": []any{
			map[string]any{
				"type": "additional_tools",
				"tools": []any{
					map[string]any{
						"type": "namespace",
						"name": "functions",
						"tools": []any{
							map[string]any{"type": "function", "name": "exec"},
						},
					},
				},
			},
		},
	}

	_, _, err := FlattenResponsesNamespaces(req)
	require.ErrorContains(t, err, "conflicts with a top-level tool")
}
