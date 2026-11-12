package service

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

// 「交付侧 vs 登记侧」的一致性护栏（2026-11-12 外审 R3 LOW 整改后重写）。
//
// 背景：cursor 路径接 codex 时，交付给客户端的工具调用类型与登记的语义曾给出
// **相反结论** —— 交付 function_call、登记 custom_tool_call → codex 续轮
// result_type_mismatch → CUR-RES-02（外审通道全线失败）。
//
// 根因是「namespace 内子工具是不是 custom」这条判定有**两处实现**：
//   - 交付侧 apicompat.customToolCallName（读 NamespacedToolName.IsCustom）
//   - 登记侧 service.cursorCustomToolCallName（曾读另一张 namespaceCustomTools map）
//
// 2026-11-12 整改：service 侧删除复制实现，改调 apicompat.CustomToolCallName；
// 独立的 namespaceCustomTools 表与 NamespaceCustomToolNames 函数全部删除，
// 一律按 origin.IsCustom 判。
//
// ⚠️ 本用例**必须在 service 包**里写：只有这里能同时调到
// 「service 包装函数」与「apicompat 公共判定」两侧。
// 放在 apicompat 包里会让两侧调用同一个函数、相等断言恒真 ——
// 那是自证，不是护栏（这正是外审 R3 指出的问题）。

// codexNamespaceToolInput 是生产抓包里的真实形态（2026-11-12）：
// 工具声明在 input[0].additional_tools 里，且用 namespace 嵌套；
// `functions.exec` 是 custom 类型。
const codexNamespaceToolInput = `[
  {"type":"additional_tools","role":"developer","tools":[
    {"type":"namespace","name":"functions","tools":[
      {"type":"custom","name":"exec","description":"run js","format":{"type":"text"}},
      {"type":"function","name":"wait","parameters":{"type":"object"}}
    ]},
    {"type":"namespace","name":"clock","tools":[
      {"type":"function","name":"sleep","parameters":{"type":"object"}}
    ]}
  ]}
]`

func TestCursorDeliveredCall_MatchesApicompatCustomJudgement(t *testing.T) {
	var req apicompat.ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(`{"model":"gpt-6-astra","input":`+codexNamespaceToolInput+`}`), &req))

	effective, err := apicompat.EffectiveResponsesTools(&req)
	require.NoError(t, err)
	meta, err := cursorToolMetaFromTools(effective)
	require.NoError(t, err)

	execFlat := apicompat.FlattenNamespaceToolName("functions", "exec")
	waitFlat := apicompat.FlattenNamespaceToolName("functions", "wait")

	// 登记侧（service 的包装函数）
	_, regIsCustom := cursorCustomToolCallName(execFlat, meta)
	// 交付侧（apicompat 的公共判定 —— chat 桥回程用的是同一个）
	_, deliveryIsCustom := apicompat.CustomToolCallName(execFlat, meta.customTools, meta.functionTools, meta.namespaceTools)

	require.Equal(t, deliveryIsCustom, regIsCustom,
		"交付侧（apicompat.CustomToolCallName）与登记侧（cursorCustomToolCallName）"+
			"对同一摊平名的 custom 判定必须一致（不一致 → codex 续轮 result_type_mismatch）")
	require.True(t, regIsCustom,
		"namespace 内的 custom 子工具必须判成 custom_tool_call")

	// function 子工具两侧都必须不判 custom（防止「一律判 custom」的假修复）。
	_, regWait := cursorCustomToolCallName(waitFlat, meta)
	_, deliveryWait := apicompat.CustomToolCallName(waitFlat, meta.customTools, meta.functionTools, meta.namespaceTools)
	require.Equal(t, deliveryWait, regWait, "function 子工具两侧结论也必须一致")
	require.False(t, regWait, "namespace 内的 function 子工具不得判成 custom_tool_call")

	// 交付给客户端的项类型也随之确定：custom 子工具必须交付成 custom_tool_call。
	delivered := cursorResponsesDeliveredToolCall(execFlat, meta)
	require.Equal(t, "custom_tool_call", delivered.Kind,
		"namespace 内 custom 子工具必须交付成 custom_tool_call（codex 只认这个类型）")
	require.Equal(t, "exec", delivered.Name, "交付时应还原成裸名")

	deliveredWait := cursorResponsesDeliveredToolCall(waitFlat, meta)
	require.Equal(t, "function_call", deliveredWait.Kind,
		"namespace 内 function 子工具按普通 function_call 交付")
	require.Equal(t, "wait", deliveredWait.Name)
	require.Equal(t, "functions", deliveredWait.Namespace)
}

// TestCursorDeliveredCall_NoSecondCustomSource 锁住「不再有第二份 custom 事实源」。
//
// 判据：cursorResponsesToolMeta 里不得再有独立的 namespaceCustomTools 字段
// （那张表与 NamespacedToolName.IsCustom 是同一份事实的两个副本，
//
//	2026-11-12 已删除）；namespace 内 custom 的信息只能来自 namespaceTools[...].IsCustom。
func TestCursorDeliveredCall_NoSecondCustomSource(t *testing.T) {
	var req apicompat.ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(`{"model":"gpt-6-astra","input":`+codexNamespaceToolInput+`}`), &req))

	effective, err := apicompat.EffectiveResponsesTools(&req)
	require.NoError(t, err)
	meta, err := cursorToolMetaFromTools(effective)
	require.NoError(t, err)

	execFlat := apicompat.FlattenNamespaceToolName("functions", "exec")
	origin, ok := meta.namespaceTools[execFlat]
	require.True(t, ok, "摊平名必须在 namespaceTools 里")
	require.True(t, origin.IsCustom,
		"namespace 内 custom 子工具的唯一标记是 namespaceTools[...].IsCustom")
}
