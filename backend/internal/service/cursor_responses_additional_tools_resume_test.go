//go:build unit

package service

// 本文件覆盖 2026-11-06 线上 CUR-RES-99 的真实形态：新版 Codex 把运行时工具声明放进
// input 里的 {"type":"additional_tools","tools":[...]} 条目（top-level tools 为空），
// 续轮完整回放时该条目随 input 一起回传。
//
// 该条目是**工具声明**而不是对话内容：交付侧历史登记从不记录它，续轮前缀比对若把它
// 当成"未知内容类型"整轮拒掉，正常续轮会恒定失败（线上表现：首轮成功、接手工具后
// 第二次请求 400 + CUR-RES-99「工具续轮处理失败」）。
//
// 修复口径：续轮上下文解析把 additional_tools 跳过（工具本身由
// apicompat.EffectiveResponsesTools 提升进工具集，语义不丢），其余未知类型仍失败关闭。

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

const cursorAdditionalToolsFirst = `{
  "model": "cursor-test-model",
  "input": [
    {"type":"additional_tools","tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]},
    {"type":"message","role":"user","content":"run the task"}
  ]
}`

// 续轮完整回放：additional_tools 原样回传 + 已交付调用 + 本批结果。
const cursorAdditionalToolsResume = `{
  "model": "cursor-test-model",
  "input": [
    {"type":"additional_tools","tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]},
    {"type":"message","role":"user","content":"run the task"},
    {"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"hz\"}"},
    {"type":"function_call_output","call_id":"call_1","output":"sunny"}
  ]
}`

// TestCursorResumeAdditionalToolsDeclarationSkipped：input 带 additional_tools 的完整回放续轮
// 必须成功，且真实结果整批写入一次。
func TestCursorResumeAdditionalToolsDeclarationSkipped(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolResponses, []byte(cursorAdditionalToolsFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: `{"city":"hz"}`}},
		"mc-1", "resp_1")

	_, err := fixture.resume(t, CursorResumeProtocolResponses, []byte(cursorAdditionalToolsResume), false)
	require.NoError(t, err, "additional_tools 是工具声明，续轮必须跳过它而不是整轮拒绝")
	require.Equal(t, 1, fixture.intake.callCount(), "真实结果必须整批写入一次")
	require.Equal(t, []string{"call_1"}, []string{fixture.intake.results[0][0].CallID})
	require.Equal(t, []string{"sunny"}, fixture.intake.results[0][0].Text)
}

// TestCursorResumeUnknownItemStillRejected：修复只放行声明类条目，未知类型仍失败关闭，
// 不得把"看不懂就跳过"变成通用行为。
func TestCursorResumeUnknownItemStillRejected(t *testing.T) {
	body := `{
  "model": "cursor-test-model",
  "input": [
    {"type":"message","role":"user","content":"run the task"},
    {"type":"totally_unknown_item","whatever":1}
  ]
}`
	var req apicompat.ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(body), &req))
	_, err := cursorPlanFromResponses(&req, "cursor-test-model")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCursorResumeUnsupportedContent)
}

// TestCursorPlanDeclaresAdditionalToolsUpstream：声明只写在 input 里（顶层 tools 为空）时，
// 上行 config 必须真的带上这些工具声明——否则"元数据认识它、上行没有它"仍是错的。
func TestCursorPlanDeclaresAdditionalToolsUpstream(t *testing.T) {
	var req apicompat.ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(cursorAdditionalToolsFirst), &req))
	plan, err := cursorPlanFromResponses(&req, "cursor-test-model")
	require.NoError(t, err)
	require.Equal(t, []string{"get_weather"}, plan.tools, "additional_tools 里的声明必须进上行工具集")
	require.True(t, plan.responsesTools.functionTools["get_weather"], "元数据与工具集必须同源")
}
