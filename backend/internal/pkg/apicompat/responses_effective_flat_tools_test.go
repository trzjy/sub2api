package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func decodeFlatToolsRequest(t *testing.T, raw string) *ResponsesRequest {
	t.Helper()
	var req ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(raw), &req))
	return &req
}

// namespace 内的 custom 子工具必须摊平：只摊 function 会让 codex 的 custom exec
// 永远下不到上游（2026-11-06 实测：摊平前模型报"没有工具"，摊平并改名后正常出 tool call）。
func TestEffectiveResponsesFlatToolsFlattensNamespaceCustomChildren(t *testing.T) {
	req := decodeFlatToolsRequest(t, `{
	  "model":"m",
	  "tools":[],
	  "input":[
	    {"type":"additional_tools","role":"developer","tools":[
	      {"type":"namespace","name":"functions","tools":[
	        {"type":"custom","name":"exec","format":{"type":"grammar","definition":"start: /.+/"}},
	        {"type":"function","name":"wait","parameters":{"type":"object"}}
	      ]}
	    ]}
	  ]
	}`)

	set, err := EffectiveResponsesFlatTools(req)
	require.NoError(t, err)
	require.Equal(t, []string{"functions__exec", "functions__wait"}, toolNames(set.Tools))
	// custom 按裸名记（回程要还原成客户端声明的名字），摊平名到裸名的关系由 namespace 表承载。
	require.True(t, set.CustomTools["exec"])
	require.False(t, set.CustomTools["functions__exec"])
	require.True(t, set.NamespaceCustomTools["functions__exec"])
	require.True(t, set.FunctionTools["functions__wait"])
	// NamespaceTools 里的 IsCustom 必须为 true（namespace 内的 exec 是 custom 子工具）。
	//
	// 2026-11-12 修正：此断言原为 IsCustom: false —— 那是**把实现缺陷固化成了期望**
	// （同一个测试里 NamespaceCustomTools["functions__exec"] 断言为 true，两张表自相矛盾）。
	// 漏填 IsCustom 的后果：交付侧（读 IsCustom）把 namespace 内 custom 判成 function，
	// 登记侧（读另一张表）判成 custom → 交付 function_call、登记 custom_tool_call
	// → codex 续轮 result_type_mismatch → CUR-RES-02（外审通道全线失败）。
	require.Equal(t, NamespacedToolName{Namespace: "functions", Name: "exec", IsCustom: true},
		set.NamespaceTools["functions__exec"])
	// format 必须原样保真：丢掉 grammar 会让 custom 工具在上游退化成无约束工具。
	require.JSONEq(t, `{"type":"grammar","definition":"start: /.+/"}`,
		string(set.Tools[0].Format))
}

func TestEffectiveResponsesFlatToolsKeepsTopLevelTools(t *testing.T) {
	req := decodeFlatToolsRequest(t, `{
	  "model":"m",
	  "tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}],
	  "input":[{"type":"message","role":"user","content":"hi"}]
	}`)

	set, err := EffectiveResponsesFlatTools(req)
	require.NoError(t, err)
	require.Equal(t, []string{"shell"}, toolNames(set.Tools))
	require.Nil(t, set.NamespaceTools)
}

// 摊平名与顶层工具撞名必须明确失败，不能静默覆盖（上游无法区分两者）。
func TestEffectiveResponsesFlatToolsRejectsFlattenCollision(t *testing.T) {
	req := decodeFlatToolsRequest(t, `{
	  "model":"m",
	  "tools":[
	    {"type":"function","name":"functions__wait","parameters":{"type":"object"}},
	    {"type":"namespace","name":"functions","tools":[{"type":"function","name":"wait"}]}
	  ]
	}`)

	_, err := EffectiveResponsesFlatTools(req)
	require.Error(t, err)
	require.Contains(t, err.Error(), "conflicts with a top-level tool")
}

func toolNames(tools []ResponsesTool) []string {
	out := make([]string, 0, len(tools))
	for _, tool := range tools {
		out = append(out, tool.Name)
	}
	return out
}

// 完全相同的 (namespace, 子工具) 重复声明不构成歧义：去重后只下发一份，且
// 既不应报错，也不应产出两条同名摊平定义（否则出站 f4 会出现两份同名工具）。
func TestEffectiveResponsesFlatToolsDedupesIdenticalNamespaceChildren(t *testing.T) {
	req := decodeFlatToolsRequest(t, `{
	  "model":"m",
	  "tools":[],
	  "input":[
	    {"type":"additional_tools","role":"developer","tools":[
	      {"type":"namespace","name":"gmail","tools":[
	        {"type":"function","name":"send","parameters":{"type":"object"}},
	        {"type":"function","name":"send","parameters":{"type":"object"}}
	      ]}
	    ]}
	  ]
	}`)

	set, err := EffectiveResponsesFlatTools(req)
	require.NoError(t, err)
	require.Equal(t, []string{"gmail__send"}, toolNames(set.Tools), "重复声明的同一子工具只下发一份")
	require.Len(t, set.Tools, 1)
}

// 完全相同 (namespace, custom 子工具) 重复声明同样去重：custom 与 function 走同一条去重。
func TestEffectiveResponsesFlatToolsDedupesIdenticalNamespaceCustomChildren(t *testing.T) {
	req := decodeFlatToolsRequest(t, `{
	  "model":"m",
	  "tools":[],
	  "input":[
	    {"type":"additional_tools","role":"developer","tools":[
	      {"type":"namespace","name":"functions","tools":[
	        {"type":"custom","name":"exec","format":{"type":"grammar","definition":"g"}},
	        {"type":"custom","name":"exec","format":{"type":"grammar","definition":"g"}}
	      ]}
	    ]}
	  ]
	}`)

	set, err := EffectiveResponsesFlatTools(req)
	require.NoError(t, err)
	require.Equal(t, []string{"functions__exec"}, toolNames(set.Tools), "重复的 custom 子工具只下发一份")
	require.True(t, set.NamespaceCustomTools["functions__exec"], "去重后仍标记为 namespace 内 custom")
}
