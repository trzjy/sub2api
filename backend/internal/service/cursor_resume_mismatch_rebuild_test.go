//go:build unit

package service

// 「续轮内容对不上」不再直接回 400 的定向测试（2026-11-04 用户裁定）。
//
// 背景：CUR-RES-04 的 history_mismatch / config_changed / tool_set_changed 原本一律 400。
// 生产实测 12 小时 123 次，29 个会话里 25 个连续失败 3 次以上——第一次对不上之后，客户端
// 重试只会带上更多历史，越试越对不上，**没有自愈能力**。
//
// 裁定后的规则与 a66fc8bf7（换模型）一致：
//   - 正文带完整历史 → 终结旧会话，按本请求正文另开一局（账号不排除）；
//   - 历史不完整   → 维持 400（凭残缺正文重开会丢上下文）。
//
// 防串号依旧成立：旧会话已终结，旧 call_id 的结果永远灌不进新局；结果侧的 call_id 归属、
// 结果类型、结果完整性三道校验一字未动。

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 续轮回传的 tool_use 入参与交付侧登记的不同（hz → sh）：**不再拒绝**（2026-11-14 旧契约下线）。
//
// 原契约要求「同一个 call_id 换了参数必须拒（防篡改）」。下线理由（生产取证）：
// 续轮回写上游的帧只带 capture 的上游委托锚点（ExecID / IDField / ID）+ 结果正文，
// 客户端 replay 的 args 从不发给上游；上游早已按它自己的参数执行完。
//
// 结果侧真正的防护（call_id 归属 / 结果类型 / 完整性）一字未动，见同文件其余用例。
func TestCursorResumeTamperedArgsAccepted(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolMessages, []byte(cursorResumeTestMessagesFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "toolu_1", Name: "get_weather", ArgsJSON: `{"city": "hz"}`}}, "hm-1", "")

	mismatched := []byte(`{
  "model": "cursor-test-model",
  "max_tokens": 64,
  "messages": [
    {"role": "user", "content": [{"type": "text", "text": "weather please"}]},
    {"role": "assistant", "content": [{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "sh"}}]},
    {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_1", "content": [{"type": "text", "text": "sunny"}]}]}
  ],
  "tools": [{"name": "get_weather", "input_schema": {"type": "object"}}]
}`)

	_, err := fixture.resume(t, CursorResumeProtocolMessages, mismatched, false)
	require.NoError(t, err, "参数表示差异不再拒绝（旧防篡改契约已下线：客户端参数从不发给上游）")
	require.Equal(t, 1, fixture.intake.callCount(), "本批结果照常整批写回上游一次")
}

// 续轮比首轮少声明一个工具（生产实测 36 次的真实形态）+ 完整历史：
// 同样按另开一局处置，不再回 config_changed / tool_set_changed。
func TestCursorResumeToolSetChangedAcceptedWithoutRebuild(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolChat, []byte(cursorResumeTestChatFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: `{"city": "hz"}`}}, "cf-1", "")

	// 工具清单为空 = 客户端这一轮少声明了工具（硬组与工具集合同时变化）。
	shrunk := []byte(`{
  "model": "cursor-test-model",
  "messages": [
    {"role": "user", "content": "weather please"},
    {"role": "assistant", "content": "", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\": \"hz\"}"}}]},
    {"role": "tool", "tool_call_id": "call_1", "content": "sunny"}
  ],
  "tools": []
}`)

	_, err := fixture.resume(t, CursorResumeProtocolChat, shrunk, false)

	// 新契约（2026-11-07 五道闸退役）：工具清单对不上不再 400、不再强制重建。
	require.NoError(t, err, "工具清单对不上不得再回 400（五道闸退役）")
	var rebuild *CursorResumeRebuildError
	require.NotErrorAs(t, err, &rebuild, "不再强制重建：按本次正文继续")
	require.Equal(t, 1, fixture.intake.callCount(),
		"按本次正文继续后应当正常出站一轮")
}

// 历史不完整时（客户端按 Responses 官方规范只发「编号 + 新结果」、不重放 function_call）
// **不再回 400**。这正是 CUR-RES-01 的成因：旧判据要求「每条结果的 call_id 都能在
// 本次正文里找到对应的 function_call」，而官方用法就不带 function_call → 恒定判成
// 历史不完整 → 恒定 400（生产近 7 天 57 次）。
//
// 新契约：本轮一律按本次正文构造，不再因「结果与调用对不上」拒绝本站会话续轮。
// 孤儿批（编号不属于本站任何会话）仍由结果侧归属校验拒绝——那是另一条路，不在本用例。
func TestResumeIncompleteHistoryStillAccepted(t *testing.T) {
	svc := &CursorExecutionService{sessions: newCursorSessionStore()}
	record := svc.sessions.start(cursorSessionSpec{
		protocol:       cursorProtocolChat,
		requestedModel: "cursor-test-model",
		upstreamModel:  "cursor-upstream-cursor-test-model",
		accountID:      42,
		quotaKeyID:     7,
	})
	ticket := &CursorResumeTicket{
		SessionID: record.id,
		BatchID:   "hm-1",
		session:   record,
		probe:     &CursorResumeProbe{Plan: &cursorContextPlan{historyComplete: false}},
	}

	// 历史不完整不再构成拒绝条件：resumeMismatchOutcome 已退役（重建逻辑并入
	// resumeModelChangedOutcome），跨轮比对只留痕。
	plan, planErr := cursorResumePlan(ticket.Protocol, CursorResumeServeInput{}, "cursor-test-model")
	require.NoError(t, planErr)
	require.NotNil(t, plan)
	require.False(t, plan.historyComplete, "本用例前提：历史不完整（客户端没重放 function_call）")

	// 判据：不再有「不一致 → 400 / 重建」的出口。
	var rebuild *CursorResumeRebuildError
	require.NotErrorAs(t, nil, &rebuild, "历史不完整不得再被转成重建")
}

// TestCursorResumePrunedProseAccepted 覆盖「客户端裁剪 assistant 可见正文」：
// 参数**没变**，只是正文被裁短 → 这是合法行为，必须放行（本轮按本次正文构造）。
//
// 与 TestCursorResumeTamperedArgsAccepted 的区别：
// 那条改了参数表示（同样放行）；这条连参数都没动，只裁正文。
func TestCursorResumePrunedProseAccepted(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolMessages, []byte(cursorResumeTestMessagesFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "toolu_1", Name: "get_weather", ArgsJSON: `{"city": "hz"}`}}, "hm-1", "")

	// 参数与交付侧登记的**完全一致**（city: hz），只是 assistant 的可见正文被省略。
	pruned := []byte(`{
  "model": "cursor-test-model",
  "max_tokens": 64,
  "messages": [
    {"role": "user", "content": [{"type": "text", "text": "weather please"}]},
    {"role": "assistant", "content": [{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "hz"}}]},
    {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_1", "content": [{"type": "text", "text": "sunny"}]}]}
  ],
  "tools": [{"name": "get_weather", "input_schema": {"type": "object"}}]
}`)

	_, err := fixture.resume(t, CursorResumeProtocolMessages, pruned, false)
	require.NoError(t, err, "正文裁剪（参数未变）必须放行：客户端裁剪 assistant 可见正文是合法行为")
	require.Equal(t, 1, fixture.intake.callCount(), "放行后应当正常出站一轮")
}
