package apicompat

import "testing"

// TestClassifyResponsesInputItemType 锁死"条目归类"这唯一事实源。
//
// 这张表就是全站对 Responses input 条目的认知：任何新增类型必须先在这里落一格，
// 其余代码只按归类分支，不再各自维护类型名清单（2026-11-06 CUR-RES-99 的根因）。
func TestClassifyResponsesInputItemType(t *testing.T) {
	cases := []struct {
		typeName string
		want     ResponsesInputItemKind
	}{
		{"", ResponsesInputItemConversation}, // 省略 type = message
		{"message", ResponsesInputItemConversation},
		{"Message", ResponsesInputItemConversation},
		{" function_call ", ResponsesInputItemConversation},
		{"custom_tool_call", ResponsesInputItemConversation},
		{"tool_search_call", ResponsesInputItemConversation},
		{"reasoning", ResponsesInputItemConversation},

		{"additional_tools", ResponsesInputItemDeclaration},
		{"Additional_Tools", ResponsesInputItemDeclaration},

		{"function_call_output", ResponsesInputItemResult},
		{"custom_tool_call_output", ResponsesInputItemResult},
		{"tool_search_output", ResponsesInputItemResult},

		{"item_reference", ResponsesInputItemUnknown},
		{"web_search_call", ResponsesInputItemUnknown},
		{"totally_unknown", ResponsesInputItemUnknown},
	}
	for _, c := range cases {
		if got := ClassifyResponsesInputItemType(c.typeName); got != c.want {
			t.Fatalf("type %q: got %v want %v", c.typeName, got, c.want)
		}
	}
}

// TestResponsesInputItemKindHelpers 确保谓词与归类结论一致（避免第二套判断）。
func TestResponsesInputItemKindHelpers(t *testing.T) {
	if !IsResponsesInputDeclaration("additional_tools") || IsResponsesInputDeclaration("message") {
		t.Fatal("declaration 谓词与归类不一致")
	}
	if !IsResponsesInputResult("function_call_output") || IsResponsesInputResult("message") {
		t.Fatal("result 谓词与归类不一致")
	}
}
