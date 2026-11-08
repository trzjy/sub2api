//go:build unit

package apicompat

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFlattenResponsesNamespaces_CustomChildFlattened verifies that a namespace
// child of type "custom" is flattened into a top-level entry alongside a
// "function" child: the custom tool keeps its type, gets the flattened name,
// and preserves its non-name fields (e.g. format).
func TestFlattenResponsesNamespaces_CustomChildFlattened(t *testing.T) {
	req := map[string]any{
		"tools": []any{
			map[string]any{
				"type": "namespace",
				"name": "functions",
				"tools": []any{
					map[string]any{
						"type":     "custom",
						"name":     "exec",
						"description": "run a command",
						"format": map[string]any{
							"type": "grammar",
							"grammar": "expr := ...",
						},
					},
					map[string]any{
						"type": "function",
						"name": "sum",
						"description": "add numbers",
						"parameters": map[string]any{"type": "object"},
					},
				},
			},
		},
	}

	names, changed, err := FlattenResponsesNamespaces(req)
	require.NoError(t, err)
	require.True(t, changed)

	tools, ok := req["tools"].([]any)
	require.True(t, ok)
	require.Len(t, tools, 2)

	// Collect flattened entries by name for order-independent checks.
	byName := map[string]map[string]any{}
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		require.True(t, ok)
		byName[stringValue(tool["name"])] = tool
	}

	// Both children are present as top-level entries.
	require.Len(t, byName, 2)

	// custom child: flattened name, type preserved, format preserved.
	customTool, ok := byName["functions__exec"]
	require.True(t, ok, "custom child must be flattened to functions__exec")
	require.Equal(t, "custom", customTool["type"])
	require.Equal(t, "run a command", customTool["description"])
	require.Equal(t, map[string]any{
		"type":    "grammar",
		"grammar": "expr := ...",
	}, customTool["format"])

	// function child: flattened name, type preserved.
	functionTool, ok := byName["functions__sum"]
	require.True(t, ok, "function child must be flattened to functions__sum")
	require.Equal(t, "function", functionTool["type"])
	require.Equal(t, "add numbers", functionTool["description"])

	// Mapping registered for the custom child.
	require.Equal(t, ResponsesNamespaceName{Namespace: "functions", Name: "exec"}, names["functions__exec"])
	require.Equal(t, ResponsesNamespaceName{Namespace: "functions", Name: "sum"}, names["functions__sum"])
}

// TestFlattenResponsesNamespaces_CustomChildMapping verifies that the flattened
// name -> {Namespace, Name} mapping contains the custom child entry.
func TestFlattenResponsesNamespaces_CustomChildMapping(t *testing.T) {
	req := map[string]any{
		"tools": []any{
			map[string]any{
				"type": "namespace",
				"name": "functions",
				"tools": []any{
					map[string]any{"type": "custom", "name": "exec"},
				},
			},
		},
	}

	names, changed, err := FlattenResponsesNamespaces(req)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, ResponsesNamespaceName{Namespace: "functions", Name: "exec"}, names["functions__exec"])
}

// TestFlattenResponsesNamespaces_CustomChildTopLevelCollision verifies that a
// custom namespace child that flattens onto a top-level tool name yields the
// same error text as the function case.
func TestFlattenResponsesNamespaces_CustomChildTopLevelCollision(t *testing.T) {
	req := map[string]any{"tools": []any{
		map[string]any{"type": "function", "name": "functions__exec"},
		map[string]any{"type": "namespace", "name": "functions", "tools": []any{
			map[string]any{"type": "custom", "name": "exec"},
		}},
	}}

	_, _, err := FlattenResponsesNamespaces(req)
	require.ErrorContains(t, err, "conflicts with a top-level tool")
}

// TestFlattenResponsesNamespaces_FunctionChildUnchanged confirms that the
// pre-existing function-only behavior is preserved after opening the filter to
// custom children: flattening, mapping, and collision error are unchanged.
func TestFlattenResponsesNamespaces_FunctionChildUnchanged(t *testing.T) {
	// Mapping + flatten for a plain function child.
	req := map[string]any{
		"tools": []any{
			map[string]any{
				"type": "namespace",
				"name": "collaboration",
				"tools": []any{
					map[string]any{"type": "function", "name": "spawn_agent", "description": "spawn"},
				},
			},
		},
	}
	names, changed, err := FlattenResponsesNamespaces(req)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, ResponsesNamespaceName{Namespace: "collaboration", Name: "spawn_agent"}, names["collaboration__spawn_agent"])

	tools, ok := req["tools"].([]any)
	require.True(t, ok)
	require.Len(t, tools, 1)
	flatTool, ok := tools[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "collaboration__spawn_agent", flatTool["name"])
	require.Equal(t, "function", flatTool["type"])
	require.Equal(t, "spawn", flatTool["description"])

	// Collision error text for a function child is unchanged.
	collide := map[string]any{"tools": []any{
		map[string]any{"type": "function", "name": "collaboration__spawn_agent"},
		map[string]any{"type": "namespace", "name": "collaboration", "tools": []any{
			map[string]any{"type": "function", "name": "spawn_agent"},
		}},
	}}
	_, _, err = FlattenResponsesNamespaces(collide)
	require.ErrorContains(t, err, "conflicts with a top-level tool")
}
