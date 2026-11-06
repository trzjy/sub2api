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

// TestCursorResumeUnknownItemIsSkippedNotRejected：本站还没接纳的新条目类型不得整轮拒。
//
// 产品口径（2026-11-06）：客户端每加一种新条目，若一律 400，客户只会认为"你们模型不稳定"。
// 正确做法是丢掉这一条、继续把本轮做完，并记一笔台账（plan.skippedItems）供我们观察。
func TestCursorResumeUnknownItemIsSkippedNotRejected(t *testing.T) {
	body := `{
  "model": "cursor-test-model",
  "input": [
    {"type":"message","role":"user","content":"run the task"},
    {"type":"totally_unknown_item","whatever":1}
  ]
}`
	var req apicompat.ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(body), &req))
	plan, err := cursorPlanFromResponses(&req, "cursor-test-model")
	require.NoError(t, err, "未知条目必须被丢弃，不得整轮拒")
	require.Equal(t, 1, plan.skippedItems["totally_unknown_item"])
	require.Equal(t, 1, plan.history.ItemCount(), "只保留 message 这条历史")
}

// web_search_call 是模型自己联网搜的痕迹：本站从不转发，回放时也不得让它把整轮打断。
func TestCursorResumeHostedWebSearchCallIsSkipped(t *testing.T) {
	body := `{
  "model": "cursor-test-model",
  "input": [
    {"type":"message","role":"user","content":"查一下天气"},
    {"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"杭州天气"}},
    {"type":"message","role":"assistant","content":"杭州 24 度"}
  ]
}`
	var req apicompat.ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(body), &req))
	plan, err := cursorPlanFromResponses(&req, "cursor-test-model")
	require.NoError(t, err)
	require.Equal(t, 1, plan.skippedItems["web_search_call"])
	require.Equal(t, 2, plan.history.ItemCount(), "两侧 message 仍进历史")
}

// 宿主工具的「调用 + 结果」必须**成对丢弃**：只丢调用、把结果当本批结果投递，
// 上游会看到"有结果、没有对应调用"，那才是真正会把模型答错的情形。
func TestCursorResumeHostedShellPairSkippedTogether(t *testing.T) {
	body := `{
  "model": "cursor-test-model",
  "input": [
    {"type":"message","role":"user","content":"跑个命令"},
    {"type":"local_shell_call","call_id":"shell_1","action":{"type":"exec","command":["ls"]}},
    {"type":"local_shell_call_output","call_id":"shell_1","output":"a.txt"}
  ]
}`
	var req apicompat.ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(body), &req))
	plan, err := cursorPlanFromResponses(&req, "cursor-test-model")
	require.NoError(t, err, "宿主工具条目不得整轮拒")
	require.Empty(t, plan.results, "local_shell_call_output 不是本站投递过的结果，不能进本批结果")
	require.Equal(t, 1, plan.skippedItems["local_shell_call"])
	require.Equal(t, 1, plan.skippedItems["local_shell_call_output"])
}

// 认得类型但内容畸形（message role 不认识）也只丢这一条，不整轮拒。
func TestCursorResumeMalformedItemSkippedNotRejected(t *testing.T) {
	body := `{
  "model": "cursor-test-model",
  "input": [
    {"type":"message","role":"weird_role","content":"???"},
    {"type":"message","role":"user","content":"真实的用户消息"}
  ]
}`
	var req apicompat.ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(body), &req))
	plan, err := cursorPlanFromResponses(&req, "cursor-test-model")
	require.NoError(t, err)
	require.Equal(t, 1, plan.skippedItems["message"])
	require.Equal(t, 1, plan.history.ItemCount(), "好的那条 message 仍进历史")
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

// cursorAdditionalToolsDuplicateDeclare 是**同一条工具被两个声明站点各写一遍**的形态：
// 顶层 tools 与 input.additional_tools 都声明 get_weather。工具集里它仍然只有一个工具。
const cursorAdditionalToolsDuplicateDeclare = `{
  "model": "cursor-test-model",
  "tools": [{"type":"function","name":"get_weather","parameters":{"type":"object"}}],
  "input": [
    {"type":"additional_tools","tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]},
    {"type":"message","role":"user","content":"run the task"}
  ]
}`

// TestCursorPlanDuplicateDeclarationCountsOnce：同一工具跨站点重复声明，上行只能带一份。
// 多算一份会让 chat 上游收到两份同名定义（实测回空正文 / 502），续轮还会因工具数对不上重建。
func TestCursorPlanDuplicateDeclarationCountsOnce(t *testing.T) {
	var req apicompat.ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(cursorAdditionalToolsDuplicateDeclare), &req))
	plan, err := cursorPlanFromResponses(&req, "cursor-test-model")
	require.NoError(t, err)
	require.Equal(t, []string{"get_weather"}, plan.tools, "同一条工具只算一个")
	require.True(t, plan.responsesTools.functionTools["get_weather"])
}

// TestCursorResumeDuplicateDeclarationKeepsToolCount：线上真实演进形态——首轮用 Lite 形态
// （只在 input 里声明一次）开局，续轮客户端改成两处都写。工具集没变（还是一个），
// 续轮必须被接受并写入结果，而不是因为"工具数 1 vs 2"误判成换了工具集去重建会话。
func TestCursorResumeDuplicateDeclarationKeepsToolCount(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolResponses, []byte(cursorAdditionalToolsFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: `{"city":"hz"}`}},
		"mc-1", "resp_1")

	resumeBody := `{
  "model": "cursor-test-model",
  "tools": [{"type":"function","name":"get_weather","parameters":{"type":"object"}}],
  "input": [
    {"type":"additional_tools","tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]},
    {"type":"message","role":"user","content":"run the task"},
    {"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"hz\"}"},
    {"type":"function_call_output","call_id":"call_1","output":"sunny"}
  ]
}`
	_, err := fixture.resume(t, CursorResumeProtocolResponses, []byte(resumeBody), false)
	var rebuild *CursorResumeRebuildError
	require.NotErrorAs(t, err, &rebuild, "工具集实际没变，不得判成换了工具集去重建")
	require.NoError(t, err)
	require.Equal(t, 1, fixture.intake.callCount(), "真实续轮必须写入本批结果")
	require.Equal(t, []string{"sunny"}, fixture.intake.results[0][0].Text)
}
