package apicompat

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// codex 的工具声明形态（2026-11-12 生产实测抓包）：
//
//	{"type":"additional_tools","role":"developer","tools":[
//	  {"type":"namespace","name":"functions","tools":[
//	     {"type":"custom","name":"exec","description":"...","format":{"type":"text"}},
//	     {"type":"function","name":"wait",...}]}]}
//
// 关键：namespace 内的 **custom** 子工具。回程必须交付 custom_tool_call
// （codex 只认 custom_tool_call），交付成 function_call 会让 codex 续轮时
// 按 function 语义回放，而本站登记的是 custom → 类型不匹配 → CUR-RES-02。

func codexNamespaceToolSet() []ResponsesTool {
	return []ResponsesTool{
		{
			Type: "namespace",
			Name: "functions",
			Tools: []ResponsesTool{
				{Type: "custom", Name: "exec"},
				{Type: "function", Name: "wait"},
			},
		},
		{
			Type: "namespace",
			Name: "clock",
			Tools: []ResponsesTool{
				{Type: "function", Name: "sleep"},
			},
		},
	}
}

// TestFlattenResponsesToolList_NamespaceToolsCarryIsCustom 是根因回归护栏。
//
// 根因：FlattenResponsesToolList 填 NamespaceTools 时漏了 IsCustom 字段，
// 于是
//   - apicompat.customToolCallName（读 origin.IsCustom）→ 判成 function；
//   - service.cursorCustomToolCallName（读另一个 map）→ 判成 custom。
// 同一判定规则两处实现不一致 → 交付 function_call、登记 custom_tool_call
// → codex 续轮 result_type_mismatch → CUR-RES-02（外审通道全线失败）。
//
// 修法：FlattenResponsesToolList 补上 IsCustom，让两侧天然一致。
func TestFlattenResponsesToolList_NamespaceToolsCarryIsCustom(t *testing.T) {
	flat, err := FlattenResponsesToolList(codexNamespaceToolSet())
	require.NoError(t, err)

	execFlat := FlattenNamespaceToolName("functions", "exec")
	waitFlat := FlattenNamespaceToolName("functions", "wait")
	sleepFlat := FlattenNamespaceToolName("clock", "sleep")

	require.True(t, flat.NamespaceTools[execFlat].IsCustom,
		"namespace 内的 custom 子工具必须在 NamespaceTools 里带 IsCustom=true")
	require.False(t, flat.NamespaceTools[waitFlat].IsCustom,
		"namespace 内的 function 子工具 IsCustom 必须为 false")
	require.False(t, flat.NamespaceTools[sleepFlat].IsCustom,
		"namespace 内的 function 子工具 IsCustom 必须为 false")
}

// TestFlattenResponsesToolList_MatchesNamespaceToolNames 是与既有正确实现的同源校验。
//
// NamespaceToolNames（chatcompletions_responses_bridge.go）**已经**正确填了 IsCustom。
// FlattenResponsesToolList 必须与它逐项一致 —— 两处都从同一份 tools 派生，
// 不允许一个填一个不填。
func TestFlattenResponsesToolList_MatchesNamespaceToolNames(t *testing.T) {
	tools := codexNamespaceToolSet()
	flat, err := FlattenResponsesToolList(tools)
	require.NoError(t, err)

	ref := NamespaceToolNames(tools)
	require.Len(t, flat.NamespaceTools, len(ref))
	for name, origin := range ref {
		got, ok := flat.NamespaceTools[name]
		require.True(t, ok, "Flat 与 NamespaceToolNames 的摊平名集合必须一致：%s", name)
		require.Equal(t, origin.Namespace, got.Namespace, "namespace 必须一致：%s", name)
		require.Equal(t, origin.Name, got.Name, "子工具名必须一致：%s", name)
		require.Equal(t, origin.IsCustom, got.IsCustom, "IsCustom 必须一致：%s", name)
	}
}

// TestCustomToolCallName_NamespaceCustomIsCustom 是交付侧判定的回归护栏。
//
// 交付侧（announceChatToolItem → customToolCallName）读 origin.IsCustom。
// 修好 FlattenResponsesToolList 后，namespace 内的 custom 必须被判成 custom_tool_call。
func TestCustomToolCallName_NamespaceCustomIsCustom(t *testing.T) {
	flat, err := FlattenResponsesToolList(codexNamespaceToolSet())
	require.NoError(t, err)

	execFlat := FlattenNamespaceToolName("functions", "exec")
	waitFlat := FlattenNamespaceToolName("functions", "wait")

	// custom 子工具 → 判 custom，还原成裸名
	name, isCustom := customToolCallName(execFlat, flat.CustomTools, flat.FunctionTools, flat.NamespaceTools)
	require.True(t, isCustom, "namespace 内的 custom 子工具必须判成 custom_tool_call")
	require.Equal(t, "exec", name, "custom 子工具应还原成裸名")

	// function 子工具 → 不判 custom
	name, isCustom = customToolCallName(waitFlat, flat.CustomTools, flat.FunctionTools, flat.NamespaceTools)
	require.False(t, isCustom, "namespace 内的 function 子工具不得判成 custom_tool_call")
	require.Empty(t, name)
}

// TestCustomToolCallName_ServiceCursorCopiesMustAgree 是「交付侧 vs 登记侧」的一致性护栏。
//
// service 包的 cursorCustomToolCallName 是 apicompat.customToolCallName 的复制实现
// （历史原因：service 包内可用）。两份实现必须给出**同样的结论** ——
// 不一致时会出现「交付 function_call、登记 custom_tool_call」的分裂，
// codex 续轮必然 result_type_mismatch。
//
// 本测试用与 service 侧同构的四集合调用 apicompat 判定，与
// NamespaceCustomToolNames 的口径对齐，锁住「custom 集合与 IsCustom 标记同源」。
func TestCustomToolCallName_ServiceCursorCopiesMustAgree(t *testing.T) {
	flat, err := FlattenResponsesToolList(codexNamespaceToolSet())
	require.NoError(t, err)

	// service 侧的判据集合（cursorToolMetaFromTools 的四个字段）
	customTools := flat.CustomTools
	functionTools := flat.FunctionTools
	namespaceTools := flat.NamespaceTools
	namespaceCustomTools := flat.NamespaceCustomTools

	// 交付侧（apicompat）：读 origin.IsCustom
	execFlat := FlattenNamespaceToolName("functions", "exec")
	_, deliveryIsCustom := customToolCallName(execFlat, customTools, functionTools, namespaceTools)

	// 登记侧（service 的等价判据）：namespaceCustomTools 命中
	registrationIsCustom := false
	if origin, ok := namespaceTools[execFlat]; ok && namespaceCustomTools[execFlat] {
		registrationIsCustom = true
		_ = origin
	}

	require.Equal(t, deliveryIsCustom, registrationIsCustom,
		"交付侧与登记侧对同一摊平名的 custom 判定必须一致（不一致 → 续轮 result_type_mismatch）")
	require.True(t, deliveryIsCustom)
}

// TestNamespaceCustomToolNames_MatchesFlattenCustomSet 锁住两个 custom 集合同源。
func TestNamespaceCustomToolNames_MatchesFlattenCustomSet(t *testing.T) {
	tools := codexNamespaceToolSet()
	flat, err := FlattenResponsesToolList(tools)
	require.NoError(t, err)

	refCustom := NamespaceCustomToolNames(tools)
	require.Equal(t, refCustom, flat.NamespaceCustomTools,
		"NamespaceCustomToolNames 与 FlattenResponsesToolList 的 custom 集合必须一致")
}
