package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

// codex 0.166 起的真实请求形态：顶层 tools 为空，工具全在 input[0].additional_tools，
// 且按 namespace 分组（functions 里既有 custom 的 exec 也有 function 的 wait）。
const codexAdditionalNamespaceInput = `[
  {"type":"additional_tools","role":"developer","tools":[
    {"type":"namespace","name":"functions","description":"","tools":[
      {"type":"custom","name":"exec","description":"run js","format":{"type":"grammar","definition":"start: /.+/"}},
      {"type":"function","name":"wait","description":"wait","parameters":{"type":"object","properties":{}},"strict":false}
    ]},
    {"type":"namespace","name":"clock","description":"","tools":[
      {"type":"function","name":"sleep","description":"sleep","parameters":{"type":"object"}}
    ]}
  ]},
  {"type":"message","role":"user","content":[{"type":"input_text","text":"run echo hi"}]}
]`

// 回归：下发声明必须来自"生效并摊平"的工具集。
// 修复前这里读 req.Tools（顶层为空），plan.tools 是空表 —— 模型一个工具都拿不到，
// 于是自行试探，最终在上游不认识的 exec 帧上以 CUR-EXEC-02 失败。
func TestCursorPlanLiftsAdditionalToolsAndFlattensNamespaces(t *testing.T) {
	req := &apicompat.ResponsesRequest{
		Model: "gpt-6-astra",
		Input: json.RawMessage(codexAdditionalNamespaceInput),
	}

	plan, err := cursorPlanFromResponses(req, "gpt-6-astra")
	require.NoError(t, err)

	require.Equal(t, []string{"functions__exec", "functions__wait", "clock__sleep"}, plan.tools,
		"下发给上游的工具名必须是摊平名：namespace 子工具按 ns__name 展开，custom 子工具同样要摊平")

	require.True(t, plan.responsesTools.customTools["exec"],
		"namespace 内的 custom 子工具必须进入 custom 集合（按裸名），否则回程无法还原成 custom_tool_call")
	// 摊平名要标记为 custom 子工具，回程才能判成 custom_tool_call 而不是 function_call。
	//
	// 2026-11-12 修正：断言依据从「另一张 namespaceCustomTools map」改为
	// **NamespacedToolName.IsCustom 字段**（唯一事实源）。此前交付侧读 IsCustom、
	// 登记侧读那张 map，两边结论可以相反 —— 那正是 codex 续轮 result_type_mismatch
	// （CUR-RES-02）的根因。断言语义不变，只是改读同一份事实。
	require.True(t, plan.responsesTools.namespaceTools["functions__exec"].IsCustom,
		"摊平名要标记为 custom 子工具，回程才能判成 custom_tool_call 而不是 function_call")
	require.True(t, plan.responsesTools.functionTools["functions__wait"])
	require.Equal(t, apicompat.NamespacedToolName{Namespace: "functions", Name: "exec", IsCustom: true},
		plan.responsesTools.namespaceTools["functions__exec"])
	require.Equal(t, apicompat.NamespacedToolName{Namespace: "clock", Name: "sleep"},
		plan.responsesTools.namespaceTools["clock__sleep"])
}

// 闭环：上游按摊平名回调时，回程必须还原成客户端声明里的原始形态
// （custom_tool_call + 裸名 exec + namespace functions），codex 才认这个调用。
func TestCursorDeliveredCallRestoresFlattenedNamespaceCustomTool(t *testing.T) {
	req := &apicompat.ResponsesRequest{
		Model: "gpt-6-astra",
		Input: json.RawMessage(codexAdditionalNamespaceInput),
	}
	plan, err := cursorPlanFromResponses(req, "gpt-6-astra")
	require.NoError(t, err)
	meta := plan.responsesTools

	custom := cursorResponsesDeliveredToolCall("functions__exec", meta)
	require.Equal(t, "custom_tool_call", custom.Kind)
	require.Equal(t, "exec", custom.Name)

	wait := cursorResponsesDeliveredToolCall("functions__wait", meta)
	require.Equal(t, "function_call", wait.Kind)
	require.Equal(t, "wait", wait.Name)
	require.Equal(t, "functions", wait.Namespace)
}

// 顶层 tools 与 additional_tools 同时存在时，两边都要进声明，不能只认一边。
func TestCursorPlanMergesTopLevelAndAdditionalTools(t *testing.T) {
	input := `[
	  {"type":"additional_tools","role":"developer","tools":[
	    {"type":"function","name":"extra","description":"d","parameters":{"type":"object"}}
	  ]},
	  {"type":"message","role":"user","content":"hi"}
	]`
	req := &apicompat.ResponsesRequest{
		Model: "m",
		Input: json.RawMessage(input),
		Tools: []apicompat.ResponsesTool{
			{Type: "function", Name: "top", Parameters: json.RawMessage(`{"type":"object"}`)},
		},
	}

	plan, err := cursorPlanFromResponses(req, "m")
	require.NoError(t, err)
	require.Equal(t, []string{"top", "extra"}, plan.tools)
}
