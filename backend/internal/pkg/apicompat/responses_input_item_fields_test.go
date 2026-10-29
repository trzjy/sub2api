//go:build unit

package apicompat

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestResponsesInputItemCarriesNamespaceCustomInputAndObjectArguments 覆盖
// Cursor Responses 续轮里客户端原样回传的工具调用项：namespace、custom 自由文本
// input，以及对象/数组形态的 arguments。所有用例只验证 DTO 解码，不触碰业务逻辑。
func TestResponsesInputItemCarriesNamespaceCustomInputAndObjectArguments(t *testing.T) {
	cases := []struct {
		name  string
		input string
		check func(t *testing.T, item ResponsesInputItem, err error)
	}{
		{
			name:  "function_call 带 namespace 与字符串 arguments",
			input: `{"type":"function_call","call_id":"c","name":"spawn_agent","namespace":"collaboration","arguments":"{\"a\":1}"}`,
			check: func(t *testing.T, item ResponsesInputItem, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if item.Name != "spawn_agent" {
					t.Errorf("Name = %q, want %q", item.Name, "spawn_agent")
				}
				if item.Namespace != "collaboration" {
					t.Errorf("Namespace = %q, want %q", item.Namespace, "collaboration")
				}
				if item.Arguments != `{"a":1}` {
					t.Errorf("Arguments = %q, want %q", item.Arguments, `{"a":1}`)
				}
				if item.Input != "" {
					t.Errorf("Input = %q, want empty", item.Input)
				}
			},
		},
		{
			name:  "function_call 无 namespace 时不凭空造",
			input: `{"type":"function_call","call_id":"c","name":"plain","arguments":"{}"}`,
			check: func(t *testing.T, item ResponsesInputItem, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if item.Namespace != "" {
					t.Errorf("Namespace = %q, want empty", item.Namespace)
				}
			},
		},
		{
			name:  "custom_tool_call 仅 input",
			input: `{"type":"custom_tool_call","call_id":"c","name":"exec","input":"text(42)"}`,
			check: func(t *testing.T, item ResponsesInputItem, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if item.Input != "text(42)" {
					t.Errorf("Input = %q, want %q", item.Input, "text(42)")
				}
				if item.Arguments != "" {
					t.Errorf("Arguments = %q, want empty", item.Arguments)
				}
			},
		},
		{
			name:  "custom_tool_call input 与 arguments 互不覆盖",
			input: `{"type":"custom_tool_call","call_id":"c","name":"exec","input":"a","arguments":"b"}`,
			check: func(t *testing.T, item ResponsesInputItem, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if item.Input != "a" {
					t.Errorf("Input = %q, want %q", item.Input, "a")
				}
				if item.Arguments != "b" {
					t.Errorf("Arguments = %q, want %q", item.Arguments, "b")
				}
			},
		},
		{
			name:  "tool_search_call 对象形态 arguments 不报错",
			input: `{"type":"tool_search_call","call_id":"c","arguments":{"query":"tools"}}`,
			check: func(t *testing.T, item ResponsesInputItem, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				var buf bytes.Buffer
				if cErr := json.Compact(&buf, []byte(item.Arguments)); cErr != nil {
					t.Fatalf("json.Compact error: %v", cErr)
				}
				if buf.String() != `{"query":"tools"}` {
					t.Errorf("Arguments (compacted) = %q, want %q", buf.String(), `{"query":"tools"}`)
				}
			},
		},
		{
			name:  "tool_search_call 字符串形态 arguments 取字符串内容",
			input: `{"type":"tool_search_call","call_id":"c","arguments":"{\"query\":\"x\"}"}`,
			check: func(t *testing.T, item ResponsesInputItem, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if item.Arguments != `{"query":"x"}` {
					t.Errorf("Arguments = %q, want %q", item.Arguments, `{"query":"x"}`)
				}
			},
		},
		{
			name:  "function_call_output 非字符串 output 走 outputRaw，Output 为原文",
			input: `{"type":"function_call_output","call_id":"c","output":{"k":1}}`,
			check: func(t *testing.T, item ResponsesInputItem, err error) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if item.Output != `{"k":1}` {
					t.Errorf("Output = %q, want %q", item.Output, `{"k":1}`)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var item ResponsesInputItem
			err := json.Unmarshal([]byte(tc.input), &item)
			tc.check(t, item, err)
		})
	}
}

// TestResponsesInputItemMarshalUnchanged 验证 namespace 字段的序列化：有 namespace
// 的项序列化后出现 "namespace" 键；没有 namespace 的项序列化后不得出现 "namespace" 键
// （omitempty 生效），其余字段行为不变。
func TestResponsesInputItemMarshalUnchanged(t *testing.T) {
	withNS := `{"type":"function_call","call_id":"c","name":"spawn_agent","namespace":"collaboration","arguments":"{\"a\":1}"}`
	var item ResponsesInputItem
	if err := json.Unmarshal([]byte(withNS), &item); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	out, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	if !strings.Contains(string(out), `"namespace":"collaboration"`) {
		t.Errorf("marshaled = %s, want it to contain %q", out, `"namespace":"collaboration"`)
	}

	withoutNS := `{"type":"function_call","call_id":"c","name":"plain","arguments":"{}"}`
	var item2 ResponsesInputItem
	if err := json.Unmarshal([]byte(withoutNS), &item2); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	out2, err := json.Marshal(item2)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	if strings.Contains(string(out2), "namespace") {
		t.Errorf("marshaled = %s, should not contain %q key (omitempty)", out2, "namespace")
	}
}
