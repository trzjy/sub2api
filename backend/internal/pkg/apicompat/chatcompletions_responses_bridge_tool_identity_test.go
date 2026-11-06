package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// 顶层 tools 与 input[].additional_tools 是同一份工具集的两个声明站点。
// 同一条工具在两处都出现时，工具集里仍然只有一个工具：多算一份会让 chat 上游
// 收到两份同名定义（实测回空正文 / 502），续轮侧还会因"工具数 1 vs 2"误判
// config 变更而重建会话。
func TestEffectiveResponsesToolsDeduplicatesAcrossDeclarationSites(t *testing.T) {
	body := `{
		"model":"gpt-5.6-luna",
		"tools":[{"type":"function","name":"wait","description":"top-level copy","parameters":{"type":"object"}}],
		"input":[
			{"type":"additional_tools","tools":[
				{"type":"function","name":"wait","description":"additional copy","parameters":{"type":"object"}},
				{"type":"function","name":"get_weather","parameters":{"type":"object"}}
			]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
		]
	}`
	var req ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(body), &req))

	tools, err := EffectiveResponsesTools(&req)
	require.NoError(t, err)
	require.Len(t, tools, 2, "同名工具跨站点只算一个")
	require.Equal(t, "wait", tools[0].Name)
	require.Equal(t, "get_weather", tools[1].Name, "保留首见顺序")
	require.Equal(t, "top-level copy", tools[0].Description, "跨站点冲突以先见者为准")

	chat, err := ResponsesToChatCompletionsRequest(&req)
	require.NoError(t, err)
	require.Len(t, chat.Tools, 2, "chat 上游只能收到一份同名定义")
}

// 同一份清单内部出现两个同名可执行工具是客户端真实歧义，必须继续由下游显式报错。
// 跨站点去重不能顺手把这条保护一起吞掉——否则客户端定义错误会被静默改写成"能跑"。
func TestEffectiveResponsesToolsKeepsWithinSiteDuplicatesForConflictCheck(t *testing.T) {
	body := `{
		"model":"gpt-5.6-luna",
		"input":[
			{"type":"additional_tools","tools":[
				{"type":"function","name":"wait","parameters":{"type":"object"}},
				{"type":"function","name":"wait","parameters":{"type":"object","properties":{"x":{"type":"string"}}}}
			]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
		]
	}`
	var req ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(body), &req))

	tools, err := EffectiveResponsesTools(&req)
	require.NoError(t, err)
	require.Len(t, tools, 2, "站点内部重复不在合并阶段处理")

	_, err = ResponsesToChatCompletionsRequest(&req)
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate top-level executable tool name")
}

// 内置工具无名字、按类型唯一：同一类型声明两次也只应算一个。
func TestEffectiveResponsesToolsDeduplicatesByNameLessType(t *testing.T) {
	body := `{
		"model":"gpt-5.6-luna",
		"tools":[{"type":"web_search"}],
		"input":[
			{"type":"additional_tools","tools":[{"type":"web_search"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
		]
	}`
	var req ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(body), &req))

	tools, err := EffectiveResponsesTools(&req)
	require.NoError(t, err)
	require.Len(t, tools, 1)
	require.Equal(t, "web_search", tools[0].Type)
}

// 只有顶层 tools、没有 input 声明的请求必须完全走原路径：不去重、不改顺序。
func TestEffectiveResponsesToolsWithoutInputDeclarationsUnchanged(t *testing.T) {
	body := `{
		"model":"gpt-5.6-luna",
		"tools":[
			{"type":"function","name":"b","parameters":{"type":"object"}},
			{"type":"function","name":"a","parameters":{"type":"object"}}
		],
		"input":"plain string input"
	}`
	var req ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(body), &req))

	tools, err := EffectiveResponsesTools(&req)
	require.NoError(t, err)
	require.Len(t, tools, 2)
	require.Equal(t, []string{"b", "a"}, []string{tools[0].Name, tools[1].Name})
}
