//go:build unit

package apicompat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// namespaceCustomCarrierRequest builds the real entry shape the adapter must
// handle in the integration worktree: an empty top-level tools field with the
// runtime tools living in the input[].additional_tools carrier (Codex 0.166
// official form). The namespace "functions" carries a custom child "exec" and a
// function child "wait". The flatten layer (NS-1/NS-2b) surfaces both children
// into req["tools"]; the adapter only lowers and registers.
func namespaceCustomCarrierRequest() map[string]any {
	return map[string]any{
		"model": "codex",
		"tools": []any{},
		"input": []any{
			map[string]any{
				"type": "additional_tools",
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

// TestAdaptResponsesClientTools_NamespaceCustomCarrier is 用例 A：真实入口（请求方向）。
func TestAdaptResponsesClientTools_NamespaceCustomCarrier(t *testing.T) {
	req := namespaceCustomCarrierRequest()

	mapping, changed, err := AdaptResponsesClientTools(req)
	require.NoError(t, err)
	require.True(t, changed, "adapter must report changed=true for a carrier-only request")

	// custom 子工具进入 CustomTools（摊平名）。
	require.True(t, mapping.CustomTools["functions__exec"], "functions__exec must be registered as a lowered custom tool")

	// 摊平名 -> {Namespace, Name} 映射存在。
	entry, ok := mapping.NamespaceTools["functions__exec"]
	require.True(t, ok, "functions__exec must have a namespace mapping")
	require.Equal(t, "functions", entry.Namespace)
	require.Equal(t, "exec", entry.Name)

	// function 子工具也进入映射（但不是 custom）。
	waitEntry, ok := mapping.NamespaceTools["functions__wait"]
	require.True(t, ok)
	require.Equal(t, "functions", waitEntry.Namespace)
	require.Equal(t, "wait", waitEntry.Name)
	require.False(t, mapping.CustomTools["functions__wait"], "namespace function child must not be flagged as custom")

	// req["tools"] 里存在摊平名条目（被降级为 function）。
	tools, ok := req["tools"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, tools)
	found := false
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if stringValue(tool["name"]) == "functions__exec" {
			found = true
			require.Equal(t, "function", tool["type"], "custom child must be lowered to function")
		}
	}
	require.True(t, found, "req[\"tools\"] must contain the flattened functions__exec entry")
}

// TestRestoreResponsesClientToolPayload_NamespaceCustomCarrier is 用例 B：真实入口（回程对称）。
func TestRestoreResponsesClientToolPayload_NamespaceCustomCarrier(t *testing.T) {
	req := namespaceCustomCarrierRequest()
	mapping, _, err := AdaptResponsesClientTools(req)
	require.NoError(t, err)
	require.NotEmpty(t, mapping.CustomTools, "precondition: custom tools registered")

	// 上游回的 function_call，名字用摊平名 functions__exec。
	payload := []byte(`{"type":"function_call","name":"functions__exec","arguments":"{\"input\":\"ls -la\"}"}`)
	restored, changed, err := RestoreResponsesClientToolPayload(payload, mapping)
	require.NoError(t, err)
	require.True(t, changed)

	var out map[string]any
	require.NoError(t, json.Unmarshal(restored, &out))
	require.Equal(t, "custom_tool_call", out["type"], "must be restored to a custom_tool_call")
	require.Equal(t, "exec", stringValue(out["name"]), "name must be the bare child name exec")
	// Codex 的默认命名空间是 functions；保留 namespace 以让 codex 路由。
	require.Equal(t, "functions", stringValue(out["namespace"]), "namespace must be functions")
	require.Equal(t, "ls -la", stringValue(out["input"]), "input must be recovered from arguments")
	require.Empty(t, stringValue(out["arguments"]), "arguments must be consumed")
}

// TestRestoreResponsesClientToolPayload_EmptyMappingNoRedLine is 用例 C：无兜底红线。
func TestRestoreResponsesClientToolPayload_EmptyMappingNoRedLine(t *testing.T) {
	payload := []byte(`{"type":"function_call","name":"functions__exec","arguments":"{\"input\":\"x\"}"}`)
	restored, changed, err := RestoreResponsesClientToolPayload(payload, ResponsesClientToolMapping{})
	require.NoError(t, err)
	require.False(t, changed, "empty mapping must not rewrite the upstream payload")
	require.JSONEq(t, string(payload), string(restored), "payload must be byte-for-byte unchanged")
}

// TestAdaptResponsesClientTools_TopLevelCustomUnchanged is 用例 D：顶层 custom 行为与改动前完全一致。
func TestAdaptResponsesClientTools_TopLevelCustomUnchanged(t *testing.T) {
	req := map[string]any{
		"tools": []any{
			map[string]any{
				"type": "custom",
				"name": "mycustom",
				"format": map[string]any{
					"type":   "grammar",
					"syntax": "lark",
				},
			},
		},
	}

	mapping, changed, err := AdaptResponsesClientTools(req)
	require.NoError(t, err)
	require.True(t, changed)
	require.True(t, mapping.CustomTools["mycustom"], "top-level custom must be registered")
	require.Empty(t, mapping.NamespaceTools, "top-level custom has no namespace mapping")

	// 回程：顶层 custom 不被改名、不挂 namespace。
	payload := []byte(`{"type":"function_call","name":"mycustom","arguments":"{\"input\":\"hello\"}"}`)
	restored, changed, err := RestoreResponsesClientToolPayload(payload, mapping)
	require.NoError(t, err)
	require.True(t, changed)
	var out map[string]any
	require.NoError(t, json.Unmarshal(restored, &out))
	require.Equal(t, "custom_tool_call", out["type"])
	require.Equal(t, "mycustom", stringValue(out["name"]), "top-level custom name must not change")
	_, hasNs := out["namespace"]
	require.False(t, hasNs, "top-level custom must not carry a namespace")
	require.Equal(t, "hello", stringValue(out["input"]))
}

// TestRestoreResponsesClientToolPayload_NamespaceFunctionNotCustom is 用例 E：namespace 内 function 子工具不被误判为 custom。
func TestRestoreResponsesClientToolPayload_NamespaceFunctionNotCustom(t *testing.T) {
	req := namespaceCustomCarrierRequest()
	mapping, _, err := AdaptResponsesClientTools(req)
	require.NoError(t, err)
	require.False(t, mapping.CustomTools["functions__wait"], "namespace function child must not be flagged custom")

	// 上游回的 function_call，名字用摊平名 functions__wait。
	payload := []byte(`{"type":"function_call","name":"functions__wait","arguments":"{}"}`)
	restored, changed, err := RestoreResponsesClientToolPayload(payload, mapping)
	require.NoError(t, err)
	require.True(t, changed)
	var out map[string]any
	require.NoError(t, json.Unmarshal(restored, &out))
	// 类型保持 function_call，名字还原为裸名、带上 namespace（不是 custom_tool_call）。
	require.Equal(t, "function_call", out["type"], "namespace function child must stay a function_call")
	require.Equal(t, "wait", stringValue(out["name"]))
	require.Equal(t, "functions", stringValue(out["namespace"]))
}

// TestAdaptResponsesClientTools_NoDoubleFlatten is 用例 F：摊平层已就位的前提下不产生重复摊平。
func TestAdaptResponsesClientTools_NoDoubleFlatten(t *testing.T) {
	req := namespaceCustomCarrierRequest()

	_, _, err := AdaptResponsesClientTools(req)
	require.NoError(t, err, "must not hit the flattened-name collision error")

	tools, ok := req["tools"].([]any)
	require.True(t, ok)
	count := 0
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if stringValue(tool["name"]) == "functions__exec" {
			count++
		}
	}
	require.Equal(t, 1, count, "flattened name functions__exec must appear exactly once in req[\"tools\"]")
}

// TestNS3R3_RealEntryProbe is the 真实入口探针 required by the dispatch report.
// It logs, on the integration worktree where the flatten layer (NS-1/NS-2b) is
// already in place, exactly the evidence the report must paste:
//   changed=true, CustomTools non-empty, and RESTORED is a custom_tool_call with
//   name=exec / namespace=functions (no duplicate-flatten collision).
func TestNS3R3_RealEntryProbe(t *testing.T) {
	req := namespaceCustomCarrierRequest()

	mapping, changed, err := AdaptResponsesClientTools(req)
	require.NoError(t, err)

	t.Logf("[NS-3R3 PROBE] changed=%v", changed)
	t.Logf("[NS-3R3 PROBE] CustomTools=%v", keysOf(mapping.CustomTools))
	t.Logf("[NS-3R3 PROBE] NamespaceTools=%v", mapping.NamespaceTools)

	require.True(t, changed)
	require.NotEmpty(t, mapping.CustomTools)

	payload := []byte(`{"type":"function_call","name":"functions__exec","arguments":"{\"input\":\"ls -la\"}"}`)
	restored, _, err := RestoreResponsesClientToolPayload(payload, mapping)
	require.NoError(t, err)

	var out map[string]any
	require.NoError(t, json.Unmarshal(restored, &out))
	t.Logf("[NS-3R3 PROBE] RESTORED type=%s name=%s namespace=%s input=%s",
		stringValue(out["type"]), stringValue(out["name"]),
		stringValue(out["namespace"]), stringValue(out["input"]))

	require.Equal(t, "custom_tool_call", out["type"])
	require.Equal(t, "exec", stringValue(out["name"]))
	require.Equal(t, "functions", stringValue(out["namespace"]))
	require.True(t, strings.Contains(string(restored), `"type":"custom_tool_call"`))
}
