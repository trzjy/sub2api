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
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// 续轮历史与已登记历史对不上（工具调用入参被改写）+ 完整历史：
// 不得回 400，必须给出"另开一局"的信号，且旧会话已终结、零上游写入。
func TestCursorResumeHistoryMismatchRebuildsWithCompleteHistory(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolMessages, []byte(cursorResumeTestMessagesFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "toolu_1", Name: "get_weather", ArgsJSON: `{"city": "hz"}`}}, "hm-1", "")

	// 续轮回传的 tool_use 入参与交付侧登记的不同（hz → sh）：骨架链对不上 = history_mismatch。
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

	var rebuild *CursorResumeRebuildError
	require.ErrorAs(t, err, &rebuild, "历史对不上且有完整历史：必须转成重建，不得回 400")
	require.Equal(t, CursorResumeRequestRebuildReason, rebuild.Reason)
	require.True(t, rebuild.KeepAccount, "账号没坏，只是客户端历史不同：不得把它排除出候选")
	require.Equal(t, int64(42), rebuild.DeadAccountID)
	require.Equal(t, 0, fixture.intake.callCount(), "重建路径零上游写入")
	require.Equal(t, cursorSessionClosed, cursorSessionStateOf(fixture.record),
		"旧会话必须终结，否则新局会和它抢并发槽")
	// 错误仍可沿既有错误链取回状态与原因（日志/排查口径不破）。
	status, reason := CursorResumeHTTPStatus(err)
	require.Equal(t, 400, status)
	require.Equal(t, "history_mismatch", reason)
}

// 续轮比首轮少声明一个工具（生产实测 36 次的真实形态）+ 完整历史：
// 同样按另开一局处置，不再回 config_changed / tool_set_changed。
func TestCursorResumeToolSetChangedRebuildsWithCompleteHistory(t *testing.T) {
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

	var rebuild *CursorResumeRebuildError
	require.ErrorAs(t, err, &rebuild, "工具清单对不上且有完整历史：必须转成重建")
	require.Equal(t, CursorResumeRequestRebuildReason, rebuild.Reason)
	require.True(t, rebuild.KeepAccount)
	require.Equal(t, 0, fixture.intake.callCount())
	require.Equal(t, cursorSessionClosed, cursorSessionStateOf(fixture.record))
}

// 历史不完整时不许重开：仍按原 400 拒绝，且**不动**会话（它还在等工具结果）。
func TestResumeMismatchOutcomeKeepsDenialWhenHistoryIncomplete(t *testing.T) {
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

	fallback := newCursorResumeError(400, "history_mismatch", ErrCursorResumeHistoryMismatch)
	err := svc.resumeMismatchOutcome(context.Background(), record, ticket,
		&Account{ID: 42, Platform: PlatformCursor}, "history_mismatch", fallback)

	var rebuild *CursorResumeRebuildError
	require.NotErrorAs(t, err, &rebuild, "历史不完整不得重建（会丢上下文）")
	status, reason := CursorResumeHTTPStatus(err)
	require.Equal(t, 400, status)
	require.Equal(t, "history_mismatch", reason)
	require.NotEqual(t, cursorSessionClosed, cursorSessionStateOf(record), "被拒时会话必须保持原状")
}
